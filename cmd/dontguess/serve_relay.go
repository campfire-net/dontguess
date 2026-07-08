package main

// serve_relay.go is the M2 WIRING KEYSTONE (dontguess-4bd): it composes the
// merged pkg/relay single-relay transport around the campfire-free local
// exchange engine (WriteClient=nil + LocalStore set). It is the integration
// boundary where the Intake (subscribe leg), the Outbox (publish leg), the
// shared Sequencer, and the operator's local fold compose into one operator
// process — see docs/design/relay-transport.md §2.3/§2.4/§2.4a/§2.6.
//
// The load-bearing composition invariants (each proven by a test in
// serve_relay_test.go):
//
//   - HOT-PATH ISOLATION (§2.4). The engine folds ONLY from its LocalStore. The
//     Intake writes relay events INTO LocalStore (Origin="relay") via the store
//     mutex; the Outbox tails LocalStore and publishes operator (Origin="local")
//     records on its OWN goroutine. Neither leg is ever on the buy/match
//     response path: a relay that is slow, blocked, or unreachable cannot add
//     latency to buy/match, which read the local fold and nothing else.
//
//   - RESTART-SEED (§2.2 + the 15f replay / 2f0 echo reviews). BEFORE the
//     subscribe loop accepts any relay event, the Sequencer's emitted-set is
//     seeded from the persisted LocalStore (seedEmittedFromStore). The in-memory
//     emitted-set is otherwise EMPTY on restart, so a validly-signed OLD operator
//     match/settle — or the operator's own echo — re-broadcast/re-delivered after
//     a restart would re-fold and double-credit scrip. The seed closes that
//     replay + echo restart double-fold.
//
//   - ECHO DEDUP (§D). The Outbox is wired WithEmittedSeeder(Sequencer.MarkEmitted)
//     so every operator event's SIGNED content-hash id is in the emitted-set
//     STRICTLY BEFORE it is published, so the relay's echo of that event dedups
//     in the concurrent Intake subscriber and never re-folds.

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/campfire-net/dontguess/pkg/exchange"
	"github.com/campfire-net/dontguess/pkg/identity"
	"github.com/campfire-net/dontguess/pkg/nostr"
	"github.com/campfire-net/dontguess/pkg/relay"
	dgstore "github.com/campfire-net/dontguess/pkg/store"
)

// relayWiring holds the M2 relay-transport components composed around one local
// exchange engine's LocalStore: the shared Sequencer (the single dedup + causal
// order authority both legs consult), the ingest Intake, the publish Outbox, and
// the Intake's metrics. The engine and both legs share the SAME *dgstore.Store —
// the engine folds from it, the Intake appends relay events to it, the Outbox
// tails it.
type relayWiring struct {
	seq     *exchange.Sequencer
	intake  *relay.Intake
	outbox  *relay.Outbox
	metrics *relay.IntakeMetrics
}

// frameReceiver is the subscription read surface the single reader loop drives:
// one blocking read of the next wire frame. *relay.Conn satisfies it (Recv
// transparently reconnects on drop); tests inject an in-process fake relay.
type frameReceiver interface {
	Recv(ctx context.Context) ([]byte, error)
}

// frameSender is the publish write surface the Outbox's demuxPublisher drives.
// *relay.Conn satisfies it; tests inject an in-process fake relay.
type frameSender interface {
	Send(ctx context.Context, frame []byte) error
}

// buildRelayWiring constructs the relay transport around ls. It signs operator
// echoes/publishes with signer, gates operator-kind authorship on operatorKeyHex
// (the operator's own nostr pubkey), persists the Outbox publish cursor at
// cursorPath, and publishes via pub. maxOrphans bounds the Sequencer's live
// orphan buffer (0 => DefaultMaxOrphans); alarm is the loud-degradation sink
// (nil => the Intake's default logging sink).
//
// It performs the STARTUP RESTART-SEED before returning (seedEmittedFromStore),
// and returns the backfill watermark (max persisted Timestamp) the subscribe REQ
// should resume from (§2.5).
func buildRelayWiring(
	ls *dgstore.Store,
	signer identity.Signer,
	operatorKeyHex string,
	cursorPath string,
	pub relay.EventPublisher,
	maxOrphans int,
	alarm relay.AlarmFunc,
) (*relayWiring, int64, error) {
	if ls == nil {
		return nil, 0, fmt.Errorf("relay wiring: nil local store")
	}
	if signer == nil {
		return nil, 0, fmt.Errorf("relay wiring: nil signer")
	}

	seq := exchange.NewSequencer(maxOrphans)

	// RESTART-SEED (§2.2). Seed the emitted-set from the persisted log BEFORE any
	// relay event can be accepted, so an old operator event / echo re-delivered
	// after a restart is deduped rather than re-folded.
	watermark, err := seedEmittedFromStore(seq, ls, signer)
	if err != nil {
		return nil, 0, fmt.Errorf("relay wiring: restart-seed: %w", err)
	}

	metrics := &relay.IntakeMetrics{}
	intake := relay.NewIntake(seq, ls, operatorKeyHex, metrics, alarm)

	// ECHO DEDUP (§D): seed the SIGNED content-hash id into the emitted-set
	// STRICTLY BEFORE publish, so the relay echo of the operator's own event
	// dedups in the concurrent Intake subscriber.
	outbox, err := relay.NewOutbox(ls, signer, pub, cursorPath,
		relay.WithEmittedSeeder(func(id string) { seq.MarkEmitted(id) }))
	if err != nil {
		return nil, 0, fmt.Errorf("relay wiring: outbox: %w", err)
	}

	return &relayWiring{seq: seq, intake: intake, outbox: outbox, metrics: metrics}, watermark, nil
}

// isOperatorOrigin reports whether a persisted record is operator-authored
// (Origin "" legacy/default, or "local"). Operator records reach the wire ONLY
// via the Outbox, which re-signs them with a content-hash id.
func isOperatorOrigin(origin string) bool { return origin == "" || origin == "local" }

// seedEmittedFromStore re-seeds seq's emitted-set from the persisted local log
// so that, after a restart, a re-broadcast OLD operator event — or the
// operator's own echo re-delivered by the relay — is deduped in the Sequencer
// and NEVER re-folded (§2.2 + the 15f replay / 2f0 echo restart double-fold).
//
// It seeds TWO id spaces, because an operator record's on-wire id DIFFERS from
// its store id:
//
//   - The raw store id of EVERY record. A relay-origin record carries the nostr
//     content-hash id verbatim as its store id (the Intake set msg.ID = ev.ID),
//     so marking it dedups that record's re-delivery directly.
//
//   - For operator-authored records, the SIGNED content-hash id ADDITIONALLY.
//     Operator records go on the wire only via the Outbox, which re-signs each
//     one; identity.SignEvent stamps a content-hash id that DIFFERS from the
//     pre-signature store id (relay-transport.md §D / WithEmittedSeeder). The
//     relay echo/re-broadcast carries THAT signed id, so it — not the store id —
//     is what must be seeded to dedup the operator's own re-broadcast. It is
//     re-derived via the IDENTICAL ToNostrEvent->SignEvent path the Outbox
//     publishes with, and the id is deterministic (a content hash, independent of
//     the Schnorr nonce), so the restart derivation matches the original publish.
//
// Returns the max Timestamp across the log — the backfill watermark (§2.5).
func seedEmittedFromStore(seq *exchange.Sequencer, ls *dgstore.Store, signer identity.Signer) (int64, error) {
	recs, err := ls.ReadAll()
	if err != nil {
		return 0, fmt.Errorf("read local log: %w", err)
	}
	var watermark int64
	for i := range recs {
		rec := recs[i]
		if rec.Timestamp > watermark {
			watermark = rec.Timestamp
		}
		if rec.ID != "" {
			seq.MarkEmitted(rec.ID)
		}
		if !isOperatorOrigin(rec.Origin) {
			continue
		}
		signedID, derr := signedEventID(rec, signer)
		if derr != nil {
			// Operator records are always valid exchange messages — the Outbox
			// asserts the same invariant when it publishes them (its convert/sign
			// FATAL). A derivation failure here is therefore a real defect, but a
			// missed signed-id seed only weakens echo dedup for THIS one record;
			// aborting startup over it would be worse. Surface it LOUD (LOCKED-5)
			// and keep seeding the rest.
			log.Printf("relay/restart-seed: WARN cannot derive signed id for operator record %s (echo-dedup weakened for it): %v", rec.ID, derr)
			continue
		}
		seq.MarkEmitted(signedID)
	}
	return watermark, nil
}

// signedEventID re-derives the SIGNED nostr content-hash id of a persisted
// record, using the exact ToNostrEvent->identity.SignEvent path the Outbox
// publishes with (relay-transport.md §2.3). The id is a content hash over
// [0,pubkey,created_at,kind,tags,content] and does NOT depend on the Schnorr
// nonce, so it is deterministic across process restarts given the same signer.
func signedEventID(rec dgstore.Record, signer identity.Signer) (string, error) {
	msg := rec.ToMessage()
	nev, err := nostr.ToNostrEvent(&msg)
	if err != nil {
		return "", fmt.Errorf("to nostr event: %w", err)
	}
	ev := &identity.Event{
		ID:        nev.ID,
		PubKey:    nev.PubKey,
		CreatedAt: nev.CreatedAt,
		Kind:      nev.Kind,
		Tags:      nev.Tags,
		Content:   nev.Content,
	}
	if err := identity.SignEvent(signer, ev); err != nil {
		return "", fmt.Errorf("sign event: %w", err)
	}
	return ev.ID, nil
}

// identityToNostrEvent copies a wire identity.Event into the structurally
// identical nostr.Event the Intake pipeline consumes, carrying the Schnorr sig
// verbatim so the universal signature floor (Intake STEP 0) verifies the
// genuine on-wire signature.
func identityToNostrEvent(ev *identity.Event) *nostr.Event {
	return &nostr.Event{
		ID:        ev.ID,
		PubKey:    ev.PubKey,
		CreatedAt: ev.CreatedAt,
		Kind:      ev.Kind,
		Tags:      ev.Tags,
		Content:   ev.Content,
		Sig:       ev.Sig,
	}
}

// runReader is the SINGLE relay read loop for the operator process. It owns the
// only Recv on the connection and demultiplexes every frame:
//
//   - ["EVENT", ev]  -> the full Intake.HandleEvent pipeline (signature floor ->
//     adapter -> operator authorship -> IngestLive -> Drain -> BatchAppend
//     Origin="relay"); the engine's poll loop then folds the new canonical tail
//     (§2.4 step 6).
//   - ["OK", id, ok] -> routed to the Outbox's demuxPublisher, which is blocked
//     waiting for exactly this ACK (the OK-demux the ConnPublisher scope-note
//     deferred: the reader owns the only Recv, so the Outbox cannot read its own
//     OK — the reader hands it over).
//   - EOSE / NOTICE  -> ignored.
//
// A per-event Intake drop is already counted+alarmed inside the Intake, so it is
// logged and the loop CONTINUES — one forged/dropped event can never wedge the
// subscription. The reader never touches the engine's fold path or the buy/match
// dispatch lock: its only write is the Intake's LocalStore.BatchAppend (store
// mutex only), which is why a backfill storm cannot serialize behind buy/match
// (§2.4, ADV-11). pub may be nil (a read-only reader with no publish leg).
func (w *relayWiring) runReader(ctx context.Context, recv frameReceiver, pub *demuxPublisher) {
	for {
		if ctx.Err() != nil {
			return
		}
		raw, err := recv.Recv(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			// LOUD: a receive failure is surfaced, not swallowed (LOCKED-5). The
			// production *relay.Conn transparently reconnects on the next Recv;
			// returning here ends this reader pass and the caller re-enters.
			log.Printf("relay/reader: recv failed, ending read pass: %v", err)
			return
		}
		f, perr := relay.ParseFrame(raw)
		if perr != nil {
			log.Printf("relay/reader: skipping malformed frame: %v", perr)
			continue
		}
		switch f.Type {
		case relay.LabelEVENT:
			if f.Event == nil {
				continue
			}
			if herr := w.intake.HandleEvent(identityToNostrEvent(f.Event)); herr != nil {
				// Counted + alarmed inside the Intake already; log and keep going.
				log.Printf("relay/reader: intake dropped event %s: %v", f.Event.ID, herr)
			}
		case relay.LabelOK:
			if pub != nil {
				pub.routeOK(f.EventID, f.Accepted)
			}
		}
	}
}

// okResult carries a relay ACK verdict from the reader to the blocked publisher.
type okResult struct{ accepted bool }

// demuxPublisher is the production relay.EventPublisher for the single-reader
// design. PublishEvent encodes+sends the EVENT frame over the shared connection,
// then blocks until runReader routes the matching ["OK", id, accepted] frame
// back via the per-event channel. This is the OK-demux the pkg/relay
// ConnPublisher scope-note explicitly deferred ("When Intake lands it will own
// the read loop and hand OK frames to the Outbox"): the Intake reader owns the
// ONLY Recv loop, so the Outbox must not read its own OK.
type demuxPublisher struct {
	send frameSender

	mu      sync.Mutex
	waiters map[string]chan okResult
}

// newDemuxPublisher builds a demuxPublisher over the send half of a connection.
func newDemuxPublisher(send frameSender) *demuxPublisher {
	return &demuxPublisher{send: send, waiters: make(map[string]chan okResult)}
}

// PublishEvent sends ev and blocks for the reader-routed OK. It registers the
// waiter BEFORE sending so an OK that races back cannot be missed.
func (p *demuxPublisher) PublishEvent(ctx context.Context, ev *identity.Event) (bool, error) {
	frame, err := relay.EncodeEvent(ev)
	if err != nil {
		return false, fmt.Errorf("demux publish: encode EVENT %s: %w", ev.ID, err)
	}
	ch := make(chan okResult, 1)
	p.mu.Lock()
	p.waiters[ev.ID] = ch
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.waiters, ev.ID)
		p.mu.Unlock()
	}()

	if err := p.send.Send(ctx, frame); err != nil {
		return false, fmt.Errorf("demux publish: send EVENT %s: %w", ev.ID, err)
	}
	select {
	case <-ctx.Done():
		return false, fmt.Errorf("demux publish: await OK for %s: %w", ev.ID, ctx.Err())
	case r := <-ch:
		return r.accepted, nil
	}
}

// routeOK delivers a relay ACK to the publisher goroutine blocked on eventID. An
// OK for an unknown/already-resolved id is a harmless no-op (the send is
// non-blocking on a buffered-1 channel).
func (p *demuxPublisher) routeOK(eventID string, accepted bool) {
	p.mu.Lock()
	ch := p.waiters[eventID]
	p.mu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- okResult{accepted: accepted}:
	default:
	}
}

// attachRelayTransport composes the M2 relay transport around ls for a running
// operator serve process and starts both legs (§2.3/§2.4). It:
//
//  1. builds the wiring (buildRelayWiring), which performs the restart-seed;
//  2. subscribes with one backfill+live REQ resuming from the seed watermark;
//  3. starts the SINGLE reader loop (runReader) — Intake ingest + OK demux;
//  4. starts the Outbox publish loop (Outbox.Run) on its own goroutine.
//
// signer is the operator's persisted nostr identity; operatorKeyHex is its own
// pubkey (== signer.PubKeyHex()); cursorPath is the Outbox durable publish
// cursor sidecar. It returns a stop func that the caller defers to unblock the
// goroutines (they also stop when ctx is cancelled).
//
// The live network dial + NIP-42 handshake are owned by the *relay.Conn passed
// in as recv/send; provisioning a LIVE relay connection is dontguess-13f
// (infra-gated) and out of scope here — this function composes the transport
// legs and is exercised end-to-end by an in-process fake relay in the tests.
func attachRelayTransport(
	ctx context.Context,
	ls *dgstore.Store,
	signer identity.Signer,
	operatorKeyHex string,
	cursorPath string,
	recv frameReceiver,
	send frameSender,
	publishInterval time.Duration,
	logf func(format string, args ...any),
) (stop func(), err error) {
	pub := newDemuxPublisher(send)
	wiring, watermark, err := buildRelayWiring(ls, signer, operatorKeyHex, cursorPath, pub, 0, nil)
	if err != nil {
		return nil, err
	}

	// Backfill + live subscription: resume from the seeded watermark with slack;
	// the Sequencer dedups the overlap (§2.5). Timestamp is nanoseconds; nostr
	// created_at (Since) is seconds.
	since := watermark/1_000_000_000 - reconnectSlackSeconds
	if since < 0 {
		since = 0
	}
	reqFrame, err := relay.EncodeReq("dg-exchange", relay.Filter{Since: &since})
	if err != nil {
		return nil, fmt.Errorf("relay attach: encode REQ: %w", err)
	}
	if err := send.Send(ctx, reqFrame); err != nil {
		return nil, fmt.Errorf("relay attach: send REQ: %w", err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); wiring.runReader(ctx, recv, pub) }()
	go func() { defer wg.Done(); wiring.outbox.Run(ctx, publishInterval) }()

	if logf != nil {
		logf("  relay transport: attached (subscribe since=%d, publish interval=%s)", since, publishInterval)
	}
	return func() { wg.Wait() }, nil
}

// reconnectSlackSeconds is the backfill overlap (seconds) subtracted from the
// watermark on (re)subscribe so no event straddling the cursor is missed; the
// Sequencer dedups the redelivered overlap (§2.5).
const reconnectSlackSeconds = int64(60)
