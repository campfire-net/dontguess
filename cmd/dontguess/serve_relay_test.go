package main

// serve_relay_test.go — the M2 WIRING + ACCEPTANCE GATE (dontguess-4bd).
//
// These tests compose the SAME pieces attachRelayTransport wires into the serve
// path — a real local exchange.Engine sharing its LocalStore with a real Intake,
// Outbox, and Sequencer — against an IN-PROCESS FAKE RELAY (a live relay is
// dontguess-13f, infra-gated, out of scope). They prove the four acceptance-gate
// properties from docs/design/relay-transport.md §2.3/§2.4/§2.4a/§2.6:
//
//  1. ROUND-TRIP: a foreign put+buy arriving over the relay fold into the engine,
//     which matches and publishes its operator match back OUT to the relay; a
//     buyer settle referencing that match folds. Both transport legs compose with
//     the unchanged engine.
//  2. RESTART-SEED: after a restart, a re-broadcast OLD operator event (or the
//     operator's own echo) is deduped and NOT re-folded — no double-credit. A
//     mutation twin (no seed) proves the seed is load-bearing.
//  3. HOT-PATH ISOLATION: with the relay publish leg fully BLOCKED, buy/match keeps
//     flowing off the local fold at p99 < 50ms — relay reads are never on the hot
//     path.
//  4. (The full 32,209-LOC pkg/exchange + pkg/scrip suites pass UNCHANGED — run
//     separately; this file adds only new tests, mutates none.)

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/campfire-net/dontguess/pkg/exchange"
	"github.com/campfire-net/dontguess/pkg/identity"
	"github.com/campfire-net/dontguess/pkg/nostr"
	"github.com/campfire-net/dontguess/pkg/proto"
	"github.com/campfire-net/dontguess/pkg/relay"
	dgstore "github.com/campfire-net/dontguess/pkg/store"
)

// --- in-process fake relay ---------------------------------------------------

// fakeRelayConn is an in-process stand-in for a NIP-42 relay.Conn. It satisfies
// both frameReceiver (Recv) and frameSender (Send). On Send of an ["EVENT", ev]:
// it records the event, ACKs it with an ["OK", id, true] frame delivered back to
// the reader (unless blockOK), and — if echo is set — echoes the EVENT back to
// the subscriber (modeling a relay that re-delivers a published event to the
// operator's own subscription). REQ/CLOSE frames are accepted with no response.
// inject() pushes a foreign EVENT to the subscriber. All delivery rides one
// buffered channel the single reader loop drains, exactly as the production
// relay.Conn.Recv feeds runReader.
type fakeRelayConn struct {
	recvCh chan []byte
	dropCh chan struct{} // buffered(1): injectDrop makes the next Recv return ErrConnDropped

	mu          sync.Mutex
	events      []*identity.Event // every EVENT the "relay" received via Send
	reqs        []*int64          // Since value of every SUCCESSFUL REQ frame received (nil => absent)
	echo        bool
	blockOK     bool
	failNextREQ int  // >0: fail (not record) that many upcoming REQ Sends (models a flapping relay)
	reqFailures int  // count of REQ Sends failed via failNextREQ (flap assertion)
	gateOnSub   bool // model NIP-01: deliver injected events ONLY while a live REQ subscription exists
	subscribed  bool // gateOnSub: a successful REQ makes the subscription live; a drop clears it
	pending     [][]byte // gateOnSub: events injected while unsubscribed, flushed on the next successful REQ
}

func newFakeRelayConn(echo bool) *fakeRelayConn {
	return &fakeRelayConn{recvCh: make(chan []byte, 4096), dropCh: make(chan struct{}, 1), echo: echo}
}

func (r *fakeRelayConn) setBlockOK(b bool) {
	r.mu.Lock()
	r.blockOK = b
	r.mu.Unlock()
}

// setGateOnSub turns on the NIP-01 fidelity model: injected events are delivered
// to the reader ONLY while a live REQ subscription exists. A drop clears the
// subscription and a subsequent SUCCESSFUL REQ re-establishes it, flushing events
// injected in between. This is what makes a reader re-entered on a
// subscription-less socket observably starve (the wave-14 wedge).
func (r *fakeRelayConn) setGateOnSub(b bool) {
	r.mu.Lock()
	r.gateOnSub = b
	r.mu.Unlock()
}

// setFailNextREQ makes the next n REQ Sends fail (return an error, not record the
// REQ, not (re)subscribe) — a flapping relay whose re-subscribe REQ momentarily
// fails before recovering.
func (r *fakeRelayConn) setFailNextREQ(n int) {
	r.mu.Lock()
	r.failNextREQ = n
	r.mu.Unlock()
}

// reqFailureCount returns how many REQ Sends have been failed via setFailNextREQ.
func (r *fakeRelayConn) reqFailureCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reqFailures
}

func (r *fakeRelayConn) Send(_ context.Context, frame []byte) error {
	f, err := relay.ParseFrame(frame)
	if err != nil {
		return nil // tolerate anything non-parseable (defensive)
	}
	if f.Type == relay.LabelREQ {
		r.mu.Lock()
		if r.failNextREQ > 0 {
			// Flap: this re-subscribe REQ fails. The subscription stays down; the
			// reconnect leg must RETRY before the reader may read again.
			r.failNextREQ--
			r.reqFailures++
			r.mu.Unlock()
			return fmt.Errorf("fake relay: injected REQ send failure")
		}
		// Record the subscribe/re-subscribe so a test can assert the reconnect leg
		// re-issued the REQ (and with what Since). A real relay would replay
		// matching history here; the test drives re-delivery explicitly via inject.
		var since *int64
		if len(f.Filters) > 0 && f.Filters[0].Since != nil {
			v := *f.Filters[0].Since
			since = &v
		}
		r.reqs = append(r.reqs, since)
		if r.gateOnSub {
			// Live subscription: flush anything injected while unsubscribed.
			r.subscribed = true
			pend := r.pending
			r.pending = nil
			r.mu.Unlock()
			for _, fr := range pend {
				r.recvCh <- fr
			}
			return nil
		}
		r.mu.Unlock()
		return nil
	}
	if f.Type != relay.LabelEVENT || f.Event == nil {
		return nil // CLOSE / other: accepted, no response
	}
	r.mu.Lock()
	r.events = append(r.events, f.Event)
	blockOK := r.blockOK
	echo := r.echo
	r.mu.Unlock()

	if !blockOK {
		ok, _ := relay.EncodeOK(f.Event.ID, true, "")
		r.recvCh <- ok
	}
	if echo {
		ev, _ := relay.EncodeSubEvent("dg-exchange", f.Event)
		r.recvCh <- ev
	}
	return nil
}

func (r *fakeRelayConn) Recv(ctx context.Context) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-r.dropCh:
		// Model a mid-stream websocket drop exactly as the production relay.Conn
		// does: return an error wrapping ErrConnDropped. Subsequent Recv calls
		// behave normally (the production Conn transparently re-dials on the next
		// Recv; the reconnect loop re-issues the REQ).
		return nil, fmt.Errorf("fake relay: %w: injected mid-stream drop", relay.ErrConnDropped)
	case f := <-r.recvCh:
		return f, nil
	}
}

// injectDrop forces the next Recv to return ErrConnDropped once — the mid-stream
// disconnect the reconnect leg must survive. Under gateOnSub it also tears down
// the live subscription: no injected event is delivered again until a successful
// re-subscribe REQ.
func (r *fakeRelayConn) injectDrop() {
	r.mu.Lock()
	if r.gateOnSub {
		r.subscribed = false
	}
	r.mu.Unlock()
	r.dropCh <- struct{}{}
}

// reqCount returns how many REQ frames the relay has received (initial subscribe
// + every reconnect re-subscribe).
func (r *fakeRelayConn) reqCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.reqs)
}

// inject delivers a foreign signed event to the operator's subscription. Under
// gateOnSub, if no live subscription exists (post-drop, pre-resubscribe) the event
// is BUFFERED and delivered only when a successful REQ re-establishes the
// subscription — modeling a NIP-01 relay that delivers nothing on a
// subscription-less socket.
func (r *fakeRelayConn) inject(ev *identity.Event) {
	frame, _ := relay.EncodeSubEvent("dg-exchange", ev)
	r.mu.Lock()
	if r.gateOnSub && !r.subscribed {
		r.pending = append(r.pending, frame)
		r.mu.Unlock()
		return
	}
	r.mu.Unlock()
	r.recvCh <- frame
}

// receivedByKind returns every EVENT the relay received (via Send) of the given
// kind — the operator's published events on the wire.
func (r *fakeRelayConn) receivedByKind(kind int) []*identity.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*identity.Event
	for _, e := range r.events {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

// --- shared helpers ----------------------------------------------------------

// signExchangeEvent builds a proto.Message with the given tags/antecedents/
// payload authored by signer, converts it through the production adapter, and
// signs it — yielding a genuinely Schnorr-signed wire event that passes the
// Intake's universal signature floor. It is how a test synthesizes a foreign
// seller/buyer event the way another agent's client would publish it.
func signExchangeEvent(t *testing.T, signer identity.Signer, tags, antecedents []string, payload []byte) *identity.Event {
	t.Helper()
	msg := &proto.Message{
		CampfireID:  "local",
		Sender:      signer.PubKeyHex(),
		Payload:     payload,
		Tags:        tags,
		Antecedents: antecedents,
		Timestamp:   time.Now().UnixNano(),
	}
	nev, err := nostr.ToNostrEvent(msg)
	if err != nil {
		t.Fatalf("ToNostrEvent(%v): %v", tags, err)
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
		t.Fatalf("SignEvent: %v", err)
	}
	return ev
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for: %s", timeout, what)
}

func countTag(recs []dgstore.Record, tag string) int {
	n := 0
	for _, r := range recs {
		for _, tg := range r.Tags {
			if tg == tag {
				n++
			}
		}
	}
	return n
}

// --- 1. ROUND-TRIP -----------------------------------------------------------

// TestRelayRoundTrip_PutBuyMatchSettle_Folds drives the full transport round
// trip through the real serve-path wiring: a foreign put and buy arrive over the
// fake relay (Intake ingest leg -> LocalStore Origin=relay -> engine fold), the
// engine auto-accepts and MATCHES, the operator's match is published back OUT
// over the relay (Outbox publish leg), and a buyer settle referencing that match
// folds. Nothing is stubbed but the relay wire itself.
func TestRelayRoundTrip_PutBuyMatchSettle_Folds(t *testing.T) {
	dir := t.TempDir()
	ls, err := dgstore.Open(dir + "/events.jsonl")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = ls.Close() })

	operator, _ := identity.Generate()
	seller, _ := identity.Generate()
	buyer, _ := identity.Generate()

	eng := exchange.NewEngine(exchange.EngineOptions{
		CampfireID:        "local",
		LocalStore:        ls,
		OperatorPublicKey: operator.PubKeyHex(),
		PollInterval:      5 * time.Millisecond,
		Logger:            func(string, ...any) {},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	relayConn := newFakeRelayConn(true /* echo */)
	stop, err := attachRelayTransport(ctx, ls, operator, operator.PubKeyHex(),
		dir+"/events.jsonl.pubcursor", relayConn, relayConn, 5*time.Millisecond, nil)
	if err != nil {
		t.Fatalf("attachRelayTransport: %v", err)
	}

	// Engine + auto-accept loop (mirrors runEngineLoop).
	engDone := make(chan struct{})
	go func() { defer close(engDone); _ = eng.Start(ctx) }()
	go func() {
		skipped := map[string]struct{}{}
		tk := time.NewTicker(10 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-tk.C:
				eng.RunAutoAccept(1_000_000, now, skipped)
			}
		}
	}()
	t.Cleanup(func() {
		cancel()
		<-engDone
		stop()
	})

	// (a) INGEST LEG: a foreign put arrives over the relay, folds, and is
	// auto-accepted into inventory.
	putEv := signExchangeEvent(t, seller,
		[]string{exchange.TagPut, "exchange:content-type:code", "exchange:domain:go"}, nil,
		localPutPayload("Go HTTP handler unit test generator", 8000))
	relayConn.inject(putEv)
	waitFor(t, 8*time.Second, "put folds + auto-accepts into inventory", func() bool {
		return len(eng.State().Inventory()) == 1
	})

	// (b) HOT PATH + PUBLISH LEG: a foreign buy arrives over the relay, the engine
	// matches, and the operator's match is published back OUT to the relay.
	buyEv := signExchangeEvent(t, buyer,
		[]string{exchange.TagBuy}, nil,
		localBuyPayload("Generate unit tests for a Go HTTP handler", 50000))
	relayConn.inject(buyEv)

	waitFor(t, 8*time.Second, "match record folds into the local log", func() bool {
		recs, _ := ls.ReadAll()
		return countTag(recs, exchange.TagMatch) >= 1
	})
	waitFor(t, 8*time.Second, "operator match published OUT to the relay", func() bool {
		return len(relayConn.receivedByKind(nostr.KindMatch)) >= 1
	})

	// The published match must be operator-authored (author == operator pubkey):
	// the publish leg re-signed the operator's own record.
	matches := relayConn.receivedByKind(nostr.KindMatch)
	if matches[0].PubKey != operator.PubKeyHex() {
		t.Fatalf("published match author %s != operator %s", matches[0].PubKey, operator.PubKeyHex())
	}
	matchSignedID := matches[0].ID

	// (c) SETTLE LEG: a buyer settle referencing the published match folds. The
	// match's signed id is in the emitted-set (seeded before publish), so the
	// settle's antecedent is satisfied and it drains + persists (Origin=relay).
	settlePayload, _ := json.Marshal(map[string]any{"match_id": matchSignedID})
	settleEv := signExchangeEvent(t, buyer,
		[]string{exchange.TagSettle, exchange.TagPhasePrefix + string(exchange.SettlePhaseBuyerAccept)},
		[]string{matchSignedID}, settlePayload)
	relayConn.inject(settleEv)

	waitFor(t, 8*time.Second, "buyer settle folds into the local log (Origin=relay)", func() bool {
		recs, _ := ls.ReadAll()
		for _, r := range recs {
			if r.Origin != "relay" {
				continue
			}
			for _, tg := range r.Tags {
				if tg == exchange.TagSettle {
					return true
				}
			}
		}
		return false
	})

	// The operator's own echoes (put-accept, match) that the relay re-delivered
	// must have deduped — the engine must not have double-folded its own match.
	recs, _ := ls.ReadAll()
	if got := countTag(recs, exchange.TagMatch); got != 1 {
		t.Fatalf("local log has %d match records, want exactly 1 — an echoed operator match double-folded", got)
	}
}

// --- 2. RESTART-SEED ---------------------------------------------------------

// TestRelayRestartReseed_NoDoubleFold proves the STARTUP RESTART-SEED (§2.2 + the
// 15f/2f0 reviews). A prior run persisted an operator match (Origin=local). A new
// process starts (fresh Sequencer with an EMPTY in-memory emitted-set) and
// rebuilds the wiring, which re-seeds the emitted-set from the persisted log.
// Then the relay re-broadcasts that OLD operator match — carrying its SIGNED
// content-hash id, the id it had on the wire. With the restart-seed the Sequencer
// dedups it: the Intake persists NOTHING (no double-fold, no double scrip
// credit). The subtest twin flips the SINGLE variable — no restart-seed — and
// proves the re-broadcast DOES double-fold, so the assertion is load-bearing.
func TestRelayRestartReseed_NoDoubleFold(t *testing.T) {
	// A helper that persists one operator match record and returns the SIGNED
	// content-hash id the relay would carry for it (the Outbox's derivation).
	persistOperatorMatch := func(t *testing.T, ls *dgstore.Store, operator identity.Signer) (string, dgstore.Record) {
		t.Helper()
		rec := dgstore.Record{
			ID:         randomLocalMsgID(t),
			CampfireID: "local",
			Sender:     operator.PubKeyHex(),
			Payload:    []byte(`{"buy_id":"b1","entry_id":"e1"}`),
			Tags:       []string{exchange.TagMatch},
			Timestamp:  time.Now().UnixNano(),
			Origin:     "local",
		}
		if err := ls.Append(rec); err != nil {
			t.Fatalf("append operator match: %v", err)
		}
		signedID, err := signedEventID(rec, operator)
		if err != nil {
			t.Fatalf("signedEventID: %v", err)
		}
		if signedID == rec.ID {
			t.Fatalf("signed id equals pre-sign store id — invalid test premise")
		}
		return signedID, rec
	}

	t.Run("with restart-seed: re-broadcast old operator match dedups", func(t *testing.T) {
		dir := t.TempDir()
		ls, err := dgstore.Open(dir + "/events.jsonl")
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		t.Cleanup(func() { _ = ls.Close() })
		operator, _ := identity.Generate()

		signedID, _ := persistOperatorMatch(t, ls, operator)

		// RESTART: build the wiring anew. buildRelayWiring runs the restart-seed
		// over the persisted log (seeding the operator record's SIGNED id).
		w, _, err := buildRelayWiring(ls, operator, operator.PubKeyHex(),
			dir+"/events.jsonl.pubcursor", &nopPublisher{}, 0, func(string, error, *nostr.Event) {})
		if err != nil {
			t.Fatalf("buildRelayWiring: %v", err)
		}

		// The relay re-broadcasts the OLD operator match, re-signed exactly as the
		// Outbox published it (same signed content-hash id).
		rebroadcast := reSignPersisted(t, ls, operator, signedID)
		if err := w.intake.HandleEvent(identityToNostrEvent(rebroadcast)); err != nil {
			t.Fatalf("HandleEvent(re-broadcast) returned error, want silent dedup: %v", err)
		}

		if got := w.metrics.Persisted.Load(); got != 0 {
			t.Fatalf("Intake persisted %d records from the re-broadcast, want 0 — restart double-fold", got)
		}
		recs, _ := ls.ReadAll()
		if got := countTag(recs, exchange.TagMatch); got != 1 {
			t.Fatalf("log has %d match records after re-broadcast, want 1 (no double-apply)", got)
		}
		if got := w.metrics.Received.Load(); got != 1 {
			t.Fatalf("Intake received %d events, want 1 — the re-broadcast must reach the pipeline for the 0-persist to be a real dedup", got)
		}
	})

	t.Run("mutation twin: NO restart-seed re-folds the old operator match", func(t *testing.T) {
		dir := t.TempDir()
		ls, err := dgstore.Open(dir + "/events.jsonl")
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		t.Cleanup(func() { _ = ls.Close() })
		operator, _ := identity.Generate()

		signedID, _ := persistOperatorMatch(t, ls, operator)

		// Deliberately DO NOT restart-seed: a bare Sequencer with an empty
		// emitted-set, wired straight into an Intake — the exact restart state the
		// seed exists to fix.
		seq := exchange.NewSequencer(0)
		mx := &relay.IntakeMetrics{}
		in := relay.NewIntake(seq, ls, operator.PubKeyHex(), mx, func(string, error, *nostr.Event) {})

		rebroadcast := reSignPersisted(t, ls, operator, signedID)
		if err := in.HandleEvent(identityToNostrEvent(rebroadcast)); err != nil {
			t.Fatalf("HandleEvent(re-broadcast) unexpected error: %v", err)
		}

		if got := mx.Persisted.Load(); got != 1 {
			t.Fatalf("without the restart-seed the re-broadcast must double-fold: persisted %d, want 1 (proves the seed is load-bearing)", got)
		}
		recs, _ := ls.ReadAll()
		if got := countTag(recs, exchange.TagMatch); got != 2 {
			t.Fatalf("without the seed the log must have 2 match records (the double-apply), got %d", got)
		}
	})
}

// reSignPersisted rebuilds the wire event for the persisted operator record whose
// signed id is wantSignedID — exactly the event the Outbox published and the
// relay would re-broadcast. It asserts the rebuilt id matches (the derivation is
// deterministic), so the test's "re-broadcast" is byte-faithful to the wire.
func reSignPersisted(t *testing.T, ls *dgstore.Store, operator identity.Signer, wantSignedID string) *identity.Event {
	t.Helper()
	recs, err := ls.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	for _, rec := range recs {
		if !isOperatorOrigin(rec.Origin) {
			continue
		}
		msg := rec.ToMessage()
		nev, err := nostr.ToNostrEvent(&msg)
		if err != nil {
			continue
		}
		ev := &identity.Event{
			ID: nev.ID, PubKey: nev.PubKey, CreatedAt: nev.CreatedAt,
			Kind: nev.Kind, Tags: nev.Tags, Content: nev.Content,
		}
		if err := identity.SignEvent(operator, ev); err != nil {
			t.Fatalf("SignEvent: %v", err)
		}
		if ev.ID == wantSignedID {
			return ev
		}
	}
	t.Fatalf("no persisted operator record re-signs to %s", wantSignedID)
	return nil
}

// nopPublisher is an EventPublisher that ACKs without a relay. Used where the
// publish leg is irrelevant to the property under test (the restart-seed).
type nopPublisher struct{}

func (nopPublisher) PublishEvent(context.Context, *identity.Event) (bool, error) { return true, nil }

// --- 3. HOT-PATH ISOLATION ---------------------------------------------------

// TestRelayHotPath_BuyMatchP99_UnderBlockedRelay proves the buy/match hot path is
// isolated from relay latency (§2.4). The relay publish leg is fully BLOCKED (the
// fake never ACKs, so the Outbox's first publish blocks forever), yet buys folded
// through the LOCAL log continue to match — because the engine reads only its
// LocalStore fold and never the relay. It measures per-buy buy->match latency
// serially and asserts p99 < 50ms, AND asserts the Outbox published+ACKed ZERO
// events (the relay is provably stalled) while every buy still matched.
func TestRelayHotPath_BuyMatchP99_UnderBlockedRelay(t *testing.T) {
	dir := t.TempDir()
	ls, err := dgstore.Open(dir + "/events.jsonl")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = ls.Close() })

	operator, _ := identity.Generate()
	seller, _ := identity.Generate()
	buyer, _ := identity.Generate()

	eng := exchange.NewEngine(exchange.EngineOptions{
		CampfireID:        "local",
		LocalStore:        ls,
		OperatorPublicKey: operator.PubKeyHex(),
		PollInterval:      2 * time.Millisecond,
		Logger:            func(string, ...any) {},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Pre-populate inventory: several accepted puts so any buy finds a match.
	const invN = 6
	for i := 0; i < invN; i++ {
		// Unique content per put: identical payloads dedup by content_hash and only
		// the first would register as a pending put.
		putEv := signExchangeEvent(t, seller,
			[]string{exchange.TagPut, "exchange:content-type:code", "exchange:domain:go"}, nil,
			localPutPayload(fmt.Sprintf("Go HTTP handler unit test generator variant %d", i), 8000))
		putMsg, _ := nostr.FromNostrEvent(identityToNostrEvent(putEv))
		if err := ls.Append(dgstore.Record{
			ID: putMsg.ID, CampfireID: "local", Sender: putMsg.Sender,
			Payload: putMsg.Payload, Tags: putMsg.Tags, Timestamp: putMsg.Timestamp,
		}); err != nil {
			t.Fatalf("append put %d: %v", i, err)
		}
		if err := eng.AutoAcceptPut(putMsg.ID, 5600, time.Now().Add(72*time.Hour)); err != nil {
			t.Fatalf("AutoAcceptPut %d: %v", i, err)
		}
	}
	if len(eng.State().Inventory()) != invN {
		t.Fatalf("pre-populated inventory = %d, want %d", len(eng.State().Inventory()), invN)
	}

	// Attach the relay with a BLOCKED publish leg (no ACKs ever).
	relayConn := newFakeRelayConn(false /* no echo */)
	relayConn.setBlockOK(true)
	// Build the wiring directly so we can read the Outbox cursor for the
	// isolation assertion; run the reader + Outbox as attachRelayTransport does.
	pub := newDemuxPublisher(relayConn)
	w, _, err := buildRelayWiring(ls, operator, operator.PubKeyHex(),
		dir+"/events.jsonl.pubcursor", pub, 0, func(string, error, *nostr.Event) {})
	if err != nil {
		t.Fatalf("buildRelayWiring: %v", err)
	}
	go w.runReader(ctx, relayConn, pub)
	go w.outbox.Run(ctx, 2*time.Millisecond)

	engDone := make(chan struct{})
	go func() { defer close(engDone); _ = eng.Start(ctx) }()
	t.Cleanup(func() { cancel(); <-engDone })

	// Stream buys through the LOCAL fold, serially, timing each buy->match.
	const buyN = 50
	latencies := make([]time.Duration, 0, buyN)
	for i := 0; i < buyN; i++ {
		buyEv := signExchangeEvent(t, buyer,
			[]string{exchange.TagBuy}, nil,
			localBuyPayload("Generate unit tests for a Go HTTP handler", 50000))
		buyMsg, _ := nostr.FromNostrEvent(identityToNostrEvent(buyEv))
		start := time.Now()
		if err := ls.Append(dgstore.Record{
			ID: buyMsg.ID, CampfireID: "local", Sender: buyMsg.Sender,
			Payload: buyMsg.Payload, Tags: buyMsg.Tags, Timestamp: buyMsg.Timestamp,
		}); err != nil {
			t.Fatalf("append buy %d: %v", i, err)
		}
		want := i + 1
		waitFor(t, 5*time.Second, "buy matches off the local fold", func() bool {
			recs, _ := ls.ReadAll()
			return countTag(recs, exchange.TagMatch) >= want
		})
		latencies = append(latencies, time.Since(start))
	}

	// ISOLATION: the relay publish leg is provably stalled — zero events
	// published+ACKed — yet every buy matched.
	if got := w.outbox.Cursor(); got != 0 {
		t.Fatalf("Outbox cursor = %d, want 0 — the relay was supposed to be fully blocked (publish must not have progressed)", got)
	}
	if lag := w.outbox.PublishLag(); lag == 0 {
		t.Fatalf("Outbox publish_lag = 0 under a blocked relay, want > 0 — the publish leg was not actually exercised/stalled")
	}

	sort.Slice(latencies, func(a, b int) bool { return latencies[a] < latencies[b] })
	p99 := latencies[(len(latencies)*99)/100]
	if p99 >= 50*time.Millisecond {
		t.Fatalf("buy/match p99 = %s, want < 50ms — the hot path is NOT isolated from the blocked relay", p99)
	}
	t.Logf("hot-path buy/match p99 = %s over %d buys (relay publish fully blocked, lag=%d)", p99, buyN, w.outbox.PublishLag())
}

// --- 4. FORCED DISCONNECT / RECONNECT ---------------------------------------

// startEngineWithAutoAccept starts the engine fold loop plus the auto-accept
// ticker (the same pair runEngineLoop / the round-trip test drive) and returns a
// cleanup that cancels and waits. It is shared by the reconnect test.
func startEngineWithAutoAccept(t *testing.T, ctx context.Context, eng *exchange.Engine) (done chan struct{}) {
	t.Helper()
	done = make(chan struct{})
	go func() { defer close(done); _ = eng.Start(ctx) }()
	go func() {
		skipped := map[string]struct{}{}
		tk := time.NewTicker(10 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-tk.C:
				eng.RunAutoAccept(1_000_000, now, skipped)
			}
		}
	}()
	return done
}

// TestRelayForcedDisconnect_ReconnectResubscribeIngestPublishResume is the §2.5
// reconnection acceptance test. It composes the SAME serve-path wiring the other
// tests do (real Engine + Intake + Outbox + Sequencer over the fake relay), drives
// a healthy round trip, then INJECTS A MID-STREAM DROP and proves the transport
// SURVIVES it rather than silently dying (the wave-12 security HIGH, LOCKED-5):
//
//   - LOUD: the Watchdog bumps intake_disconnected and re-issues the REQ
//     (re-subscribe) — the reconnect leg actually ran;
//   - INGEST RESUMES: a foreign put+buy arriving AFTER the drop still folds and
//     matches off the local log (the reader re-entered its loop);
//   - PUBLISH RESUMES, NO WEDGE: the post-drop operator match is published AND
//     ACKed (Outbox cursor advances, publish_lag returns to 0), proving the
//     OK-demux was re-established and Outbox.Run did not wedge on an OK that could
//     no longer route.
//
// Every one of these assertions is load-bearing against the pre-fix behaviour: a
// single-pass reader that RETURNED on the drop would leave ingest dead (put2 never
// folds → inventory never reaches 2) and the OK-demux dead (match2 never ACKs →
// publish_lag stuck > 0), so this test times out on the old code and passes only
// with the reconnect loop.
func TestRelayForcedDisconnect_ReconnectResubscribeIngestPublishResume(t *testing.T) {
	dir := t.TempDir()
	ls, err := dgstore.Open(dir + "/events.jsonl")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = ls.Close() })

	operator, _ := identity.Generate()
	seller, _ := identity.Generate()
	buyer, _ := identity.Generate()

	eng := exchange.NewEngine(exchange.EngineOptions{
		CampfireID:        "local",
		LocalStore:        ls,
		OperatorPublicKey: operator.PubKeyHex(),
		PollInterval:      5 * time.Millisecond,
		Logger:            func(string, ...any) {},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// Build the wiring directly so the test holds the Outbox (cursor/lag) and the
	// Watchdog metrics (intake_disconnected). Wire + run exactly as
	// attachRelayTransport does, but with a FAST reconnect backoff so the test does
	// not sleep the production 500ms per reconnect.
	relayConn := newFakeRelayConn(true /* echo */)
	pub := newDemuxPublisher(relayConn)
	w, watermark, err := buildRelayWiring(ls, operator, operator.PubKeyHex(),
		dir+"/events.jsonl.pubcursor", pub, 0, func(string, error, *nostr.Event) {})
	if err != nil {
		t.Fatalf("buildRelayWiring: %v", err)
	}

	// Initial subscribe (mirrors attachRelayTransport).
	since := watermark/1_000_000_000 - reconnectSlackSeconds
	if since < 0 {
		since = 0
	}
	reqFrame, _ := relay.EncodeReq(relaySubID, relay.Filter{Since: &since})
	if err := relayConn.Send(ctx, reqFrame); err != nil {
		t.Fatalf("initial REQ: %v", err)
	}

	wd, wdM := w.newReconnectWatchdog(ls, relayConn, func(string, error, *nostr.Event) {})
	fastBackoff := relay.Backoff{Initial: 5 * time.Millisecond, Max: 20 * time.Millisecond}
	go w.runReaderReconnect(ctx, relayConn, pub, wd, func() int64 { return storeWatermarkSeconds(ls) }, fastBackoff)
	go w.outbox.Run(ctx, 5*time.Millisecond)

	engDone := startEngineWithAutoAccept(t, ctx, eng)
	t.Cleanup(func() { cancel(); <-engDone })

	// --- BASELINE (pre-drop): put+buy folds, matches, publishes + ACKs. ---
	put1 := signExchangeEvent(t, seller,
		[]string{exchange.TagPut, "exchange:content-type:code", "exchange:domain:go"}, nil,
		localPutPayload("Go HTTP handler unit test generator", 8000))
	relayConn.inject(put1)
	waitFor(t, 8*time.Second, "put1 folds + auto-accepts into inventory", func() bool {
		return len(eng.State().Inventory()) == 1
	})

	buy1 := signExchangeEvent(t, buyer, []string{exchange.TagBuy}, nil,
		localBuyPayload("Generate unit tests for a Go HTTP handler", 50000))
	relayConn.inject(buy1)
	waitFor(t, 8*time.Second, "match1 published OUT + ACKed (baseline publish works)", func() bool {
		return len(relayConn.receivedByKind(nostr.KindMatch)) >= 1
	})
	waitFor(t, 8*time.Second, "publish_lag drains to 0 pre-drop (OK routed, cursor advanced)", func() bool {
		return w.outbox.PublishLag() == 0
	})

	reqsBeforeDrop := relayConn.reqCount()

	// --- FORCE A MID-STREAM DISCONNECT. ---
	relayConn.injectDrop()

	// LOUD reconnect: intake_disconnected alarm fired AND the REQ was re-issued.
	waitFor(t, 8*time.Second, "watchdog bumped intake_disconnected (loud reconnect)", func() bool {
		return wdM.IntakeDisconnected.Load() >= 1
	})
	waitFor(t, 8*time.Second, "reconnect re-issued the REQ (re-subscribe happened)", func() bool {
		return relayConn.reqCount() > reqsBeforeDrop
	})

	// --- INGEST RESUMES: a new foreign put+buy after the drop folds + matches. ---
	put2 := signExchangeEvent(t, seller,
		[]string{exchange.TagPut, "exchange:content-type:code", "exchange:domain:go"}, nil,
		localPutPayload("Go gRPC interceptor unit test generator", 9000))
	relayConn.inject(put2)
	waitFor(t, 8*time.Second, "put2 folds after reconnect (ingest resumed)", func() bool {
		return len(eng.State().Inventory()) == 2
	})

	buy2 := signExchangeEvent(t, buyer, []string{exchange.TagBuy}, nil,
		localBuyPayload("Generate unit tests for a Go gRPC interceptor", 60000))
	relayConn.inject(buy2)

	// --- PUBLISH RESUMES, NO WEDGE: match2 is published AND ACKed. ---
	waitFor(t, 8*time.Second, "match2 published OUT after reconnect", func() bool {
		return len(relayConn.receivedByKind(nostr.KindMatch)) >= 2
	})
	waitFor(t, 8*time.Second, "publish_lag drains to 0 after reconnect (OK-demux re-established, no wedge)", func() bool {
		return w.outbox.PublishLag() == 0
	})

	// The match2 ACK advancing the cursor is the direct proof the demux survived:
	// a wedged Outbox would leave the cursor short and publish_lag pinned > 0.
	recs, _ := ls.ReadAll()
	if got := countTag(recs, exchange.TagMatch); got < 2 {
		t.Fatalf("local log has %d match records after reconnect, want >= 2 (ingest+match must have resumed)", got)
	}
	t.Logf("reconnect OK: intake_disconnected=%d reqs=%d matches_published=%d publish_lag=%d",
		wdM.IntakeDisconnected.Load(), relayConn.reqCount(), len(relayConn.receivedByKind(nostr.KindMatch)), w.outbox.PublishLag())
}

// TestRelayReconnectFlap_ReqSendFailsThenSucceeds_ReaderHeldUntilLiveSub is the
// wave-14 HIGH regression test (resubscribe-ordering wedge). On a drop the reader
// MUST NOT resume until a SUCCESSFUL re-subscribe REQ exists — because
// wd.Reconnect and runReader share one connection and only wd.Reconnect ever
// issues the REQ. This test flaps the relay: the FIRST post-drop re-subscribe REQ
// Send FAILS, the next SUCCEEDS. The fake models NIP-01 fidelity (gateOnSub): an
// event injected while unsubscribed is delivered ONLY once a live REQ is issued.
//
// A foreign put injected during the unsubscribed window folds (lands as an
// Origin=relay record) ONLY after the retry re-subscribes. On the PRE-FIX code the
// reader re-enters immediately after the FAILED reconnect, blocks on a
// subscription-less socket, wd.Reconnect is never retried, and the put is never
// delivered — so this test times out. It passes only with the retry-until-REQ-
// succeeds loop, and asserts the flap actually happened (a REQ Send failed, then
// intake_disconnected was bumped more than once — the retry).
func TestRelayReconnectFlap_ReqSendFailsThenSucceeds_ReaderHeldUntilLiveSub(t *testing.T) {
	dir := t.TempDir()
	ls, err := dgstore.Open(dir + "/events.jsonl")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = ls.Close() })

	operator, _ := identity.Generate()
	seller, _ := identity.Generate()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	relayConn := newFakeRelayConn(false /* no echo */)
	relayConn.setGateOnSub(true) // NIP-01: no delivery without a live REQ subscription
	pub := newDemuxPublisher(relayConn)
	w, watermark, err := buildRelayWiring(ls, operator, operator.PubKeyHex(),
		dir+"/events.jsonl.pubcursor", pub, 0, func(string, error, *nostr.Event) {})
	if err != nil {
		t.Fatalf("buildRelayWiring: %v", err)
	}

	// Initial subscribe: makes the subscription live (mirrors attachRelayTransport).
	since := watermark/1_000_000_000 - reconnectSlackSeconds
	if since < 0 {
		since = 0
	}
	reqFrame, _ := relay.EncodeReq(relaySubID, relay.Filter{Since: &since})
	if err := relayConn.Send(ctx, reqFrame); err != nil {
		t.Fatalf("initial REQ: %v", err)
	}

	wd, wdM := w.newReconnectWatchdog(ls, relayConn, func(string, error, *nostr.Event) {})
	fastBackoff := relay.Backoff{Initial: 5 * time.Millisecond, Max: 20 * time.Millisecond}
	go w.runReaderReconnect(ctx, relayConn, pub, wd, func() int64 { return storeWatermarkSeconds(ls) }, fastBackoff)
	t.Cleanup(func() { cancel() })

	// BASELINE: a foreign put delivered over the LIVE subscription folds into the
	// local log (Origin=relay). This proves the live-sub delivery path works before
	// the flap.
	put1 := signExchangeEvent(t, seller,
		[]string{exchange.TagPut, "exchange:content-type:code", "exchange:domain:go"}, nil,
		localPutPayload("Go HTTP handler unit test generator", 8000))
	relayConn.inject(put1)
	waitFor(t, 8*time.Second, "put1 ingested over the live subscription (Origin=relay)", func() bool {
		return w.metrics.Persisted.Load() == 1
	})

	reqsBeforeDrop := relayConn.reqCount()

	// Arrange the FLAP: the first re-subscribe REQ after the drop FAILS, the next
	// succeeds.
	relayConn.setFailNextREQ(1)

	// Drop the connection AND inject put2 during the unsubscribed window. put2 is
	// BUFFERED (gateOnSub) and delivered only when a live re-subscribe succeeds, so
	// a reader resumed on the failed/unsubscribed socket can never see it.
	relayConn.injectDrop()
	put2 := signExchangeEvent(t, seller,
		[]string{exchange.TagPut, "exchange:content-type:code", "exchange:domain:go"}, nil,
		localPutPayload("Go gRPC interceptor unit test generator", 9000))
	relayConn.inject(put2)

	// The flap was actually exercised: a REQ Send failed once...
	waitFor(t, 8*time.Second, "a re-subscribe REQ Send failed (flap exercised)", func() bool {
		return relayConn.reqFailureCount() >= 1
	})
	// ...and the reconnect leg RETRIED (intake_disconnected bumped on each
	// wd.Reconnect: at least the failed attempt + the successful one). On the
	// pre-fix code Reconnect is called once then the reader blocks, leaving this at
	// exactly 1.
	waitFor(t, 8*time.Second, "reconnect retried after the failed REQ (intake_disconnected >= 2)", func() bool {
		return wdM.IntakeDisconnected.Load() >= 2
	})
	waitFor(t, 8*time.Second, "the successful retry re-issued the REQ", func() bool {
		return relayConn.reqCount() > reqsBeforeDrop
	})

	// INGEST RESUMES only after the live re-subscribe: put2, buffered while
	// unsubscribed, now folds. This is the load-bearing proof the reader never read
	// on a subscription-less socket — it starved until the REQ succeeded.
	waitFor(t, 8*time.Second, "put2 folds after the flap (ingest resumed on a live sub)", func() bool {
		return w.metrics.Persisted.Load() == 2
	})

	recs, _ := ls.ReadAll()
	if got := countTag(recs, exchange.TagPut); got != 2 {
		t.Fatalf("local log has %d put records after the flap, want 2 (both foreign puts ingested)", got)
	}
	t.Logf("flap OK: reqFailures=%d intake_disconnected=%d reqs=%d persisted=%d",
		relayConn.reqFailureCount(), wdM.IntakeDisconnected.Load(), relayConn.reqCount(), w.metrics.Persisted.Load())
}

// TestRelayInFlightPublishAtDrop_FailedAndRetried_NoWedge closes the veracity gap
// the prior attempt left (failInFlight scenario-A): a publish that is IN-FLIGHT AT
// THE MOMENT of the drop — blocked in the OK-demux awaiting an OK — must be FAILED
// (pub.failInFlight) so the Outbox RETRIES it, rather than blocking forever on an
// OK that can no longer route (which would wedge Outbox.Run). The existing
// reconnect test only covers a NEW publish issued AFTER reconnect; this one blocks
// a publish, drops WITH it in flight, and proves it recovers.
//
// The relay blocks OK (never ACKs) so the first publish blocks in flight; the drop
// is then injected. Without failInFlight the blocked PublishEvent never returns,
// Outbox.Run wedges, the cursor never advances, and this times out. With it the
// publish is failed, retried, and — once the reader re-subscribes and the OK-demux
// is live — the retry's OK routes and the cursor advances.
func TestRelayInFlightPublishAtDrop_FailedAndRetried_NoWedge(t *testing.T) {
	dir := t.TempDir()
	ls, err := dgstore.Open(dir + "/events.jsonl")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = ls.Close() })

	operator, _ := identity.Generate()

	// One operator record queued for publish (Origin=local) — the event that will
	// be in flight when the connection drops.
	rec := dgstore.Record{
		ID:         randomLocalMsgID(t),
		CampfireID: "local",
		Sender:     operator.PubKeyHex(),
		Payload:    []byte(`{"buy_id":"b1","entry_id":"e1"}`),
		Tags:       []string{exchange.TagMatch},
		Timestamp:  time.Now().UnixNano(),
		Origin:     "local",
	}
	if err := ls.Append(rec); err != nil {
		t.Fatalf("append operator match: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	relayConn := newFakeRelayConn(false /* no echo */)
	relayConn.setBlockOK(true) // never ACK -> the publish blocks IN-FLIGHT awaiting OK
	pub := newDemuxPublisher(relayConn)
	w, watermark, err := buildRelayWiring(ls, operator, operator.PubKeyHex(),
		dir+"/events.jsonl.pubcursor", pub, 0, func(string, error, *nostr.Event) {})
	if err != nil {
		t.Fatalf("buildRelayWiring: %v", err)
	}

	since := watermark/1_000_000_000 - reconnectSlackSeconds
	if since < 0 {
		since = 0
	}
	reqFrame, _ := relay.EncodeReq(relaySubID, relay.Filter{Since: &since})
	if err := relayConn.Send(ctx, reqFrame); err != nil {
		t.Fatalf("initial REQ: %v", err)
	}

	wd, _ := w.newReconnectWatchdog(ls, relayConn, func(string, error, *nostr.Event) {})
	fastBackoff := relay.Backoff{Initial: 5 * time.Millisecond, Max: 20 * time.Millisecond}
	go w.runReaderReconnect(ctx, relayConn, pub, wd, func() int64 { return storeWatermarkSeconds(ls) }, fastBackoff)
	go w.outbox.Run(ctx, 5*time.Millisecond)
	t.Cleanup(func() { cancel() })

	// The publish is IN-FLIGHT AT the drop: the relay has RECEIVED the EVENT but
	// (blockOK) never ACKed it, so PublishEvent is blocked in the OK-demux and the
	// cursor has NOT advanced.
	waitFor(t, 8*time.Second, "operator match EVENT sent, awaiting OK (in-flight)", func() bool {
		return len(relayConn.receivedByKind(nostr.KindMatch)) >= 1
	})
	if got := w.outbox.Cursor(); got != 0 {
		t.Fatalf("Outbox cursor = %d before any ACK, want 0 (publish must still be in flight)", got)
	}

	// Drop WITH the publish still in flight; unblock the relay so the RETRY can be
	// ACKed once the OK-demux is re-established.
	relayConn.injectDrop()
	relayConn.setBlockOK(false)

	// failInFlight must have failed the in-flight publish so the Outbox retried;
	// after the reconnect re-subscribes, the retry's OK routes and the cursor
	// advances. A wedged Outbox would leave the cursor at 0 and publish_lag pinned.
	waitFor(t, 12*time.Second, "in-flight publish failed+retried -> cursor advances (no wedge)", func() bool {
		return w.outbox.Cursor() >= 1 && w.outbox.PublishLag() == 0
	})

	// The relay received the EVENT more than once: the first (in-flight, dropped)
	// send, plus at least one retry after failInFlight.
	if got := len(relayConn.receivedByKind(nostr.KindMatch)); got < 2 {
		t.Fatalf("relay received the match EVENT %d time(s), want >= 2 (in-flight send + retry after failInFlight)", got)
	}
	t.Logf("in-flight-at-drop OK: cursor=%d publish_lag=%d sends=%d",
		w.outbox.Cursor(), w.outbox.PublishLag(), len(relayConn.receivedByKind(nostr.KindMatch)))
}

// TestGuardOperatorKeyMigration_WarnsOnCampfireEraRecords proves the LOW
// migration guard: it counts (and warns on) operator-authored records signed
// under a prior operator key when the relay operator key switches, and does NOT
// count relay-origin foreign records or records already under the current key.
func TestGuardOperatorKeyMigration_WarnsOnCampfireEraRecords(t *testing.T) {
	dir := t.TempDir()
	ls, err := dgstore.Open(dir + "/events.jsonl")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = ls.Close() })

	current, _ := identity.Generate() // the new nostr relay operator key
	oldA, _ := identity.Generate()    // a prior (campfire-era) operator key
	oldB, _ := identity.Generate()    // a second prior key
	foreign, _ := identity.Generate() // some other agent, arrived via relay

	appendRec := func(sender, origin string) {
		if err := ls.Append(dgstore.Record{
			ID:         randomLocalMsgID(t),
			CampfireID: "local",
			Sender:     sender,
			Payload:    []byte(`{}`),
			Tags:       []string{exchange.TagPut},
			Timestamp:  time.Now().UnixNano(),
			Origin:     origin,
		}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	// Two mismatched operator records under two prior keys (both counted).
	appendRec(oldA.PubKeyHex(), "local")
	appendRec(oldB.PubKeyHex(), "local")
	appendRec(oldA.PubKeyHex(), "") // legacy empty origin is still operator-authored
	// A relay-origin foreign record: NOT ours to republish, must not count.
	appendRec(foreign.PubKeyHex(), "relay")
	// A record already under the current key: not a mismatch.
	appendRec(current.PubKeyHex(), "local")
	// A legacy empty-Sender operator record: treated as current, not a mismatch.
	appendRec("", "local")

	var logged string
	n := guardOperatorKeyMigration(ls, current.PubKeyHex(), func(format string, args ...any) {
		logged += fmt.Sprintf(format, args...)
	})
	if n != 3 {
		t.Fatalf("migration guard counted %d mismatched operator records, want 3 (two under oldA, one under oldB)", n)
	}
	if logged == "" {
		t.Fatalf("migration guard did not warn on mismatched records (LOCKED-5 requires a loud warning)")
	}

	// A store with NO mismatches must be silent and return 0.
	dir2 := t.TempDir()
	ls2, err := dgstore.Open(dir2 + "/events.jsonl")
	if err != nil {
		t.Fatalf("open2: %v", err)
	}
	t.Cleanup(func() { _ = ls2.Close() })
	if err := ls2.Append(dgstore.Record{
		ID: randomLocalMsgID(t), CampfireID: "local", Sender: current.PubKeyHex(),
		Payload: []byte(`{}`), Tags: []string{exchange.TagPut}, Timestamp: time.Now().UnixNano(), Origin: "local",
	}); err != nil {
		t.Fatalf("append clean: %v", err)
	}
	var logged2 string
	if n := guardOperatorKeyMigration(ls2, current.PubKeyHex(), func(format string, args ...any) {
		logged2 += fmt.Sprintf(format, args...)
	}); n != 0 || logged2 != "" {
		t.Fatalf("clean store: guard counted %d / logged %q, want 0 and silent", n, logged2)
	}
}

// --------------------------------------------------------------------------
// TestShutdownRelayTransport_NoHangOnCtxCancel (dontguess-e35)
// --------------------------------------------------------------------------

// TestShutdownRelayTransport_NoHangOnCtxCancel proves the shutdown-ordering fix
// for runServeLocal's relay-enabled path (dontguess-e35 HIGH). Before the fix,
// runServeLocal registered `defer conn.Close()` and `defer stop()` (stop being
// attachRelayTransport's wg.Wait()) SEPARATELY, after an unconditional
// `defer cancel()` — under Go's LIFO defer order, stop() (which blocks until
// the reader/outbox goroutines observe ctx.Done()) ran BEFORE cancel(), so
// shutdown hung forever whenever DONTGUESS_RELAY_URL was set.
//
// This test attaches the SAME attachRelayTransport used by runServeLocal to an
// in-process fake relay (ctx-aware, mirroring TestRelayRoundTrip's setup), then
// calls the production shutdownRelayTransport helper — the exact function
// runServeLocal now defers — and asserts it returns well within a generous
// timeout. Before the fix this scenario (stop() called without a prior cancel)
// hangs indefinitely; the bounded select below turns that hang into a failing
// test instead of a wedged CI run.
func TestShutdownRelayTransport_NoHangOnCtxCancel(t *testing.T) {
	dir := t.TempDir()
	ls, err := dgstore.Open(dir + "/events.jsonl")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = ls.Close() })

	operator, _ := identity.Generate()
	ctx, cancel := context.WithCancel(context.Background())

	relayConn := newFakeRelayConn(true /* echo */)
	stop, err := attachRelayTransport(ctx, ls, operator, operator.PubKeyHex(),
		dir+"/events.jsonl.pubcursor", relayConn, relayConn, 5*time.Millisecond, nil)
	if err != nil {
		t.Fatalf("attachRelayTransport: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		// Mirrors runServeLocal's single combined defer exactly: cancel, then
		// close (nil here — fakeRelayConn has no Close of its own to wire up,
		// matching the frameReceiver/frameSender contract), then wait.
		shutdownRelayTransport(cancel, nil, stop)
	}()

	select {
	case <-done:
		// shutdownRelayTransport returned — cancel() ran before stop()
		// (wg.Wait()), exactly as the fix requires.
	case <-time.After(5 * time.Second):
		t.Fatal("shutdownRelayTransport hung: stop() (wg.Wait()) never returned — " +
			"cancel() did not run before wait, reproducing the dontguess-e35 shutdown hang")
	}
}
