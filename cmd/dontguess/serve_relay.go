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
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/3dl-dev/dontguess/pkg/exchange"
	"github.com/3dl-dev/dontguess/pkg/identity"
	"github.com/3dl-dev/dontguess/pkg/nostr"
	"github.com/3dl-dev/dontguess/pkg/relay"
	dgstore "github.com/3dl-dev/dontguess/pkg/store"
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
	// intakeCursor is the durable per-relay "how far ingested" watermark
	// (dontguess-61a). nil when no intake-cursor sidecar path was supplied to
	// buildRelayWiring (WithIntakeCursorPath) — callers that omit it get the
	// bounded-kinds fix only, matching every pre-fix test call site unchanged.
	intakeCursor *relay.IntakeCursor
}

// relayWiringOption customises buildRelayWiring beyond its required core
// parameters. A trailing variadic option keeps every existing call site (7 test
// files + serve.go, none of which need the intake-cursor sidecar to compile or
// pass) source-compatible: omitting it disables the durable Intake cursor
// exactly as before this fix (Since falls back to the local-store watermark
// only, and ResyncAudit falls back to since=0 — see Watchdog.resyncSince).
type relayWiringOption func(*relayWiringConfig)

type relayWiringConfig struct {
	intakeCursorPath string
	climbWatermark   int64
}

// WithClimbWatermark threads the solo→fleet CLIMB egress fence (ADV-18, design
// §6 + §9 Gate A/P4) into the leg's Outbox. watermark is the count of
// operator-authored (Origin=local) records already in the local log at the FIRST
// relay attach — the pre-climb PLAINTEXT corpus the individual tier stored in
// cleartext (§541 §6). buildRelayWiring hands it to relay.WithClimbFence, which
// seeds the durable publish cursor to it ONLY when this leg's cursor sidecar is
// absent (the first-ever attach = the climb), so pre-climb plaintext puts stay
// LOCAL-ONLY and are never republished to the relay. 0 (the default / every
// existing test call site) disables the fence — a fresh cursor publishes the
// whole log, exactly as before.
func WithClimbWatermark(watermark int64) relayWiringOption {
	return func(c *relayWiringConfig) { c.climbWatermark = watermark }
}

// WithIntakeCursorPath wires the durable per-relay Intake cursor sidecar
// (dontguess-61a) at path, mirroring the Outbox's relayCursorPath sidecar. When
// set, buildRelayWiring opens (or creates) the sidecar and attachRelayTransport
// uses its persisted value — re-read from disk, not memory, on every restart —
// to bound the initial subscribe's `since`, and threads the SAME cursor into the
// resync-audit Watchdog so the periodic full-sweep also resumes from it instead
// of an unconditional since=0 every cycle.
func WithIntakeCursorPath(path string) relayWiringOption {
	return func(c *relayWiringConfig) { c.intakeCursorPath = path }
}

// appendNotifier fans a single engine EngineOptions.OnLocalAppend callback out
// to every attached relay leg's Outbox.Notify (design §3.8, H1). The engine is
// constructed with ONE OnLocalAppend before any relay leg exists, and a serve
// process may attach N legs (one per relay URL), each with its own Outbox; so the
// callback the engine holds is this notifier's fire, and each attachRelayTransport
// registers its leg's Notify via add. On an operator record append the engine
// calls fire, which wakes every leg's publish loop immediately — an operator match
// reaches every relay sub-second instead of up to a full outbox tick later.
//
// add and fire are mutex-guarded because legs are attached (add) while the engine
// may already be folding and firing. Notify is non-blocking, so fire holding the
// lock only across a slice-reference read (not the calls) never blocks a fold.
type appendNotifier struct {
	mu     sync.Mutex
	notify []func()
}

// add registers one leg's Outbox.Notify. Called once per attached relay leg.
func (a *appendNotifier) add(fn func()) {
	a.mu.Lock()
	a.notify = append(a.notify, fn)
	a.mu.Unlock()
}

// fire wakes every registered leg's publish loop. It reads the slice header under
// the lock (append never mutates an existing backing element in place, so the
// captured header is a stable snapshot) then invokes each Notify OUTSIDE the lock.
// With no legs registered (individual tier never sets this as OnLocalAppend) it is
// a no-op, but serve leaves OnLocalAppend nil there anyway.
func (a *appendNotifier) fire() {
	a.mu.Lock()
	fns := a.notify
	a.mu.Unlock()
	for _, fn := range fns {
		fn()
	}
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
// aliasRegistrar (dontguess-55c GAP 1), when non-nil, is the exchange State's
// RegisterWireAlias: it is rebuilt over the persisted log by the restart-seed AND
// wired into the Outbox so every operator record's wire→store id alias is recorded
// both at restart and at live publish. nil (the individual/no-alias callers, and
// the existing test callers) is a strict no-op — no wire→store alias is wired, and
// resolveAlias stays the identity.
func buildRelayWiring(
	ls *dgstore.Store,
	signer identity.Signer,
	operatorKeyHex string,
	cursorPath string,
	pub relay.EventPublisher,
	maxOrphans int,
	alarm relay.AlarmFunc,
	aliasRegistrar func(wire, store string),
	opts ...relayWiringOption,
) (*relayWiring, int64, error) {
	if ls == nil {
		return nil, 0, fmt.Errorf("relay wiring: nil local store")
	}
	if signer == nil {
		return nil, 0, fmt.Errorf("relay wiring: nil signer")
	}

	var cfg relayWiringConfig
	for _, opt := range opts {
		opt(&cfg)
	}
	var intakeCursor *relay.IntakeCursor
	if cfg.intakeCursorPath != "" {
		ic, icerr := relay.OpenIntakeCursor(cfg.intakeCursorPath)
		if icerr != nil {
			return nil, 0, fmt.Errorf("relay wiring: open intake cursor: %w", icerr)
		}
		intakeCursor = ic
	}

	seq := exchange.NewSequencer(maxOrphans)

	// RESTART-SEED (§2.2). Seed the emitted-set from the persisted log BEFORE any
	// relay event can be accepted, so an old operator event / echo re-delivered
	// after a restart is deduped rather than re-folded. The same pass REBUILDS the
	// wire→store alias (dontguess-55c GAP 1) so a wire-id settle for a pre-restart
	// match resolves.
	watermark, err := seedEmittedFromStore(seq, ls, signer, aliasRegistrar)
	if err != nil {
		return nil, 0, fmt.Errorf("relay wiring: restart-seed: %w", err)
	}

	metrics := &relay.IntakeMetrics{}
	intake := relay.NewIntake(seq, ls, operatorKeyHex, metrics, alarm)

	// ECHO DEDUP (§D): seed the SIGNED content-hash id into the emitted-set
	// STRICTLY BEFORE publish, so the relay echo of the operator's own event
	// dedups in the concurrent Intake subscriber. WIRE→STORE ALIAS (dontguess-55c
	// GAP 1) is registered at the SAME pre-publish seam so a team-tier buyer's
	// settle antecedent (which carries the wire id) resolves to the store id.
	outbox, err := relay.NewOutbox(ls, signer, pub, cursorPath,
		relay.WithEmittedSeeder(func(id string) { seq.MarkEmitted(id) }),
		relay.WithAliasRegistrar(aliasRegistrar),
		// CLIMB EGRESS FENCE (ADV-18, §6 + §9 Gate A/P4): fence the pre-climb
		// plaintext corpus local-only. No-op (0) unless WithClimbWatermark was
		// threaded (serve.go's relay-attach path) AND this leg's cursor is fresh.
		relay.WithClimbFence(cfg.climbWatermark))
	if err != nil {
		return nil, 0, fmt.Errorf("relay wiring: outbox: %w", err)
	}

	return &relayWiring{seq: seq, intake: intake, outbox: outbox, metrics: metrics, intakeCursor: intakeCursor}, watermark, nil
}

// isOperatorOrigin reports whether a persisted record is operator-authored
// (Origin "" legacy/default, or "local"). Operator records reach the wire ONLY
// via the Outbox, which re-signs them with a content-hash id.
func isOperatorOrigin(origin string) bool { return origin == "" || origin == "local" }

// climbWatermarkPath is the sidecar under dgHome recording the solo→fleet CLIMB
// egress watermark (ADV-18, design §6 + §9 Gate A/P4): the count of
// operator-authored (Origin=local) records already in the local log at the FIRST
// relay attach. Every relay leg's Outbox fences egress below this count so the
// pre-climb PLAINTEXT corpus (the individual tier stored content in cleartext,
// §541 §6) is NEVER republished to a relay on the climb.
func climbWatermarkPath(dgHome string) string {
	return filepath.Join(dgHome, "climb-egress.watermark")
}

// establishClimbWatermark returns the durable climb watermark, creating it on the
// FIRST relay-attached serve by RECONSTRUCTING the true climb point from ls (see
// below). It is IDEMPOTENT: once written it is never recomputed, so (a) restarts
// reuse the original climb point instead of drifting upward as post-climb
// inventory grows, and (b) a relay added to an already-fleet operator later fences
// the SAME pre-climb corpus and correctly backfills the post-climb (encrypted)
// inventory created above the watermark. A born-fleet operator's first start has an
// empty log ⇒ watermark 0 ⇒ nothing fenced. The write uses temp→fsync→rename so
// the fence survives a crash immediately after the climb.
//
// SIDECAR-LOSS RECONSTRUCTION (dontguess-897): the sidecar-absent branch is reached
// on the genuine climb AND on a sidecar loss/corruption of an ALREADY-fleet
// operator. It must NOT simply count the current operator-record total: that total
// DRIFTS UPWARD as post-climb v2-ciphertext inventory accumulates, so on a loss it
// would over-fence — a SECOND relay added afterward seeds its fresh cursor to the
// inflated count and never backfills the encrypted inventory (a replication gap on
// the new relay; no plaintext leak, since every over-fenced record is ciphertext).
// Instead it reconstructs the TRUE climb point: the position, in the Outbox's
// local-origin subsequence, PAST the last operator record carrying INLINE PLAINTEXT
// content (a solo/individual-tier put or settle(deliver), §541 §6). Post-climb
// content records are v2 envelopes and protocol metadata carries no content, so
// both sit ABOVE this point and republish freely. The reconstructed value is STABLE
// under post-climb growth (the last plaintext-content position never moves), so a
// recompute-after-loss yields the SAME fence the original climb wrote.
func establishClimbWatermark(path string, ls *dgstore.Store) (int64, error) {
	b, rerr := os.ReadFile(path)
	if rerr == nil {
		s := strings.TrimSpace(string(b))
		if s == "" {
			// FAIL LOUD (dontguess-9d1): an EXISTING but empty/whitespace watermark
			// sidecar is corrupt/truncated — writeClimbWatermarkFile always writes at
			// least "0\n", so an empty file is never a state we produced. Returning 0
			// here would FAIL OPEN: watermark 0 silently disables the fence, and an
			// empty sidecar + a fresh relay cursor would re-broadcast the entire
			// pre-climb plaintext corpus. Treat it like the negative/non-numeric cases
			// below and refuse to serve until it is repaired (delete to force a clean
			// recompute, or restore from backup).
			return 0, fmt.Errorf("climb watermark %s: file is empty (truncated or corrupt) — refusing to fail open and re-broadcast the pre-climb corpus; delete it to force recompute or restore from backup", path)
		}
		n, perr := strconv.ParseInt(s, 10, 64)
		if perr != nil {
			return 0, fmt.Errorf("climb watermark %s: parse %q: %w", path, s, perr)
		}
		if n < 0 {
			return 0, fmt.Errorf("climb watermark %s: negative value %d", path, n)
		}
		return n, nil
	}
	if !errors.Is(rerr, os.ErrNotExist) {
		return 0, fmt.Errorf("climb watermark %s: read: %w", path, rerr)
	}

	// No sidecar yet — reconstruct the true climb point (see the doc comment). Walk
	// the log; for every operator-authored record advance the local-origin position,
	// and remember the position of the last one carrying inline plaintext content.
	// That position (records fenced up to and including the last plaintext put/
	// deliver) is the watermark: it fences every plaintext-content record while
	// leaving post-climb v2 inventory and protocol metadata above the fence.
	recs, err := ls.ReadAll()
	if err != nil {
		return 0, fmt.Errorf("climb watermark: read store: %w", err)
	}
	var localPos, w int64
	for i := range recs {
		if !isOperatorOrigin(recs[i].Origin) {
			continue
		}
		localPos++ // 1-indexed position within the Outbox's local-origin subsequence
		if recordCarriesInlinePlaintextContent(recs[i].Payload) {
			w = localPos
		}
	}
	if err := writeClimbWatermarkFile(path, w); err != nil {
		return 0, err
	}
	return w, nil
}

// recordCarriesInlinePlaintextContent reports whether a persisted operator
// record's payload inlines PLAINTEXT content that the climb egress fence must keep
// local-only (ADV-18, §541 §6). It is the reconstruction signal
// establishClimbWatermark uses to find the true climb point after a sidecar loss
// (dontguess-897).
//
// A §3.3 v2 confidential envelope (v>=2 with a non-null "enc" object) carries NO
// plaintext — its content travels only as AEAD ciphertext, so it is safe to
// republish. Everything else is inspected for a non-empty base64 "content" field:
// a solo/individual-tier put or settle(deliver) inlines cleartext there, while
// protocol metadata (match/buy/settle-complete) has no "content" field and is not
// fenced.
//
// Conservative on a payload it cannot parse: an operator record that fails to
// decode is treated as content-bearing (fenced), favouring no-leak over backfill —
// though in a healthy log every operator record is valid JSON (the Outbox re-signs
// each one to publish), so this branch is not reached in practice.
func recordCarriesInlinePlaintextContent(payload []byte) bool {
	var p struct {
		V       int             `json:"v"`
		Content string          `json:"content"`
		Enc     json.RawMessage `json:"enc"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return true
	}
	enc := strings.TrimSpace(string(p.Enc))
	if p.V >= 2 && enc != "" && enc != "null" {
		return false
	}
	return p.Content != ""
}

// writeClimbWatermarkFile durably persists the watermark (temp→fsync→rename +
// best-effort dir fsync), matching the Outbox cursor's crash discipline so a
// crash right after the climb cannot lose the fence and re-broadcast the corpus.
func writeClimbWatermarkFile(path string, w int64) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".climb-egress-*.tmp")
	if err != nil {
		return fmt.Errorf("climb watermark: create temp in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.WriteString(strconv.FormatInt(w, 10) + "\n"); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("climb watermark: write temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("climb watermark: fsync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("climb watermark: close temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("climb watermark: rename into place: %w", err)
	}
	committed = true
	if d, derr := os.Open(dir); derr == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

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
//
// aliasRegistrar (dontguess-55c GAP 1), when non-nil, is called with the derived
// SIGNED wire id and the persisted STORE id of every operator record, rebuilding
// the wire→store alias the live Outbox registers at publish — so a wire-id settle
// for a match created BEFORE this restart still resolves after the seed. It reuses
// the SAME signedID the echo-dedup seed derives (one derivation, two consumers).
// nil is a no-op (no alias rebuild), preserving the existing callers' behavior.
func seedEmittedFromStore(seq *exchange.Sequencer, ls *dgstore.Store, signer identity.Signer, aliasRegistrar func(wire, store string)) (int64, error) {
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
		// Rebuild the wire→store alias for this operator record (dontguess-55c GAP
		// 1): signedID is its wire id, rec.ID its store id. Deterministic across
		// restarts (content-hash id), so the alias is reconstructed identically to
		// the live-publish registration.
		if aliasRegistrar != nil {
			aliasRegistrar(signedID, rec.ID)
		}
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
			// dontguess-61a: advance the durable per-relay Intake cursor to this
			// event's created_at REGARDLESS of accept/drop — even a dropped event
			// means the relay has served us up to this point in time, so a
			// subsequent REQ never needs to re-request it. Advance is a max-climb,
			// crash-safe fsync; a nil cursor (no sidecar wired) is a no-op.
			if w.intakeCursor != nil {
				if aerr := w.intakeCursor.Advance(f.Event.CreatedAt); aerr != nil {
					log.Printf("relay/reader: intake cursor persist failed (since will be recomputed from the store watermark next restart): %v", aerr)
				}
			}
		case relay.LabelOK:
			if pub != nil {
				pub.routeOK(f.EventID, f.Accepted)
			}
		}
	}
}

// relaySubID is the single subscription id the operator process uses for its one
// backfill+live REQ (and every reconnect re-subscribe re-issues it).
const relaySubID = "dg-exchange"

// runReaderReconnect is the RECONNECT-LOOP wrapper around the single read pass
// (runReader) — the §2.5 reconnection leg. runReader owns the only Recv and
// returns on ctx cancel OR when the connection drops (relay.Conn.Recv returns
// ErrConnDropped on the first websocket drop; the fake relay injects one for the
// test). This loop makes ONE disconnect a recoverable event rather than the
// silent death of the transport it was before this leg was wired (the LOCKED-5
// defeat: ingest stopped folding, the OK-demux died, and the next Outbox publish
// wedged Outbox.Run indefinitely).
//
// On each drop it, in order:
//
//  1. FAILS every in-flight publish (pub.failInFlight) so a blocked Outbox
//     publish returns and retries rather than wedging on an OK that can no longer
//     route — this is the "Outbox publish cannot wedge" invariant;
//  2. RE-SUBSCRIBES BEFORE re-reading (the wave-14 HIGH fix): it drives the
//     Watchdog reconnect (wd.Reconnect) — which bumps intake_disconnected, alarms
//     + loud-logs, and RE-ISSUES REQ since=(watermark−slack) — and RETRIES it with
//     bounded backoff UNTIL the REQ Send actually succeeds. Only then does the
//     reader re-enter. wd.Reconnect and runReader share the SAME connection:
//     relay.Conn transparently re-dials + re-AUTHs on the next Recv but NEVER
//     replays the REQ — only this re-subscribe issues it. Re-entering the reader
//     after a FAILED re-subscribe could hand runReader a LIVE-but-subscription-less
//     socket (the relay recovered during backoff): NIP-01 then delivers nothing,
//     Recv blocks forever, and wd.Reconnect (the sole re-subscribe) is never
//     retried — a permanent SILENT ingest death on a healthy-looking socket. The
//     retry-until-REQ-succeeds loop closes that wedge; the alarm stays loud on
//     every retry (each wd.Reconnect bumps intake_disconnected + alarms);
//  3. bounds the backoff (grows to backoff.Max on repeated failure, reset on a
//     clean re-subscribe) and applies a settle delay before resuming so a relay
//     that drops instantly on each fresh socket cannot hot-spin the reconnect;
//  4. re-enters runReader, resuming ingest of the re-delivered events.
//
// watermarkFn re-samples the LOCAL high-water mark (NIP-01 seconds) at each
// reconnect, so backfill resumes from everything folded since the last REQ.
func (w *relayWiring) runReaderReconnect(
	ctx context.Context,
	recv frameReceiver,
	pub *demuxPublisher,
	wd *relay.Watchdog,
	watermarkFn func() int64,
	backoff relay.Backoff,
) {
	initial := backoff.Initial
	if initial <= 0 {
		initial = 10 * time.Millisecond
	}
	delay := initial
	for {
		if ctx.Err() != nil {
			return
		}
		// One read pass: consumes frames through the Intake + OK-demux until Recv
		// errors (drop) or ctx is cancelled.
		w.runReader(ctx, recv, pub)
		if ctx.Err() != nil {
			return
		}

		// The connection dropped mid-stream. Unwedge any in-flight publish FIRST,
		// then re-subscribe.
		pub.failInFlight()

		// RESUBSCRIBE-BEFORE-READ (wave-14 HIGH). Retry wd.Reconnect with bounded
		// backoff until its REQ Send actually SUCCEEDS, holding the reader out until
		// then. A reader re-entered on a re-subscribe that FAILED could read a
		// live-but-unsubscribed socket forever (NIP-01 delivers nothing without a
		// REQ), and wd.Reconnect — the only re-subscribe — would never be retried:
		// silent ingest death on a healthy socket. Looping here guarantees a live
		// subscription precedes every reader pass.
		for {
			if ctx.Err() != nil {
				return
			}
			if rerr := wd.Reconnect(ctx, watermarkFn()); rerr == nil {
				delay = initial // clean re-subscribe: reset the backoff
				break
			} else if ctx.Err() != nil {
				return
			} else {
				log.Printf("relay/reader: reconnect re-subscribe REQ failed (reader held out, will retry): %v", rerr)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
			if backoff.Max > 0 {
				if delay *= 2; delay > backoff.Max {
					delay = backoff.Max
				}
			}
		}

		// Settle delay before resuming the reader on the now-live subscription, so a
		// relay that drops instantly on each fresh socket cannot hot-spin the loop.
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

// resubscriber is the Watchdog Subscriber for the SINGLE-reader transport: its
// Query issues ONE REQ frame over the shared send half and returns immediately
// with NO delivered ids. In this design the one runReader loop owns the only
// Recv and consumes the re-delivered events through the Intake — the Subscriber
// MUST NOT read the wire itself (that would race the reader for frames). The
// reconnect leg (Watchdog.Reconnect) ignores the delivered-id set anyway; it only
// needs the REQ re-issued + the intake_disconnected alarm, both of which this
// provides. (The orphan-refetch / resync audit legs, which DO consume delivered
// ids, are §2.5 paths 2/3 and out of scope for this reconnect item.)
type resubscriber struct {
	send  frameSender
	subID string
}

func (s *resubscriber) Query(ctx context.Context, f relay.Filter) ([]string, error) {
	frame, err := relay.EncodeReq(s.subID, f)
	if err != nil {
		return nil, fmt.Errorf("resubscribe: encode REQ: %w", err)
	}
	if err := s.send.Send(ctx, frame); err != nil {
		return nil, fmt.Errorf("resubscribe: send REQ: %w", err)
	}
	return nil, nil
}

// newReconnectWatchdog builds the §2.5 reconnect-leg Watchdog for the single-
// reader transport. Its Subscriber re-issues the REQ over send (the single
// runReader consumes the re-delivered events); it reads the live orphan view
// (w.seq) and the local store (ls) and drives the intake_disconnected alarm +
// WatchdogMetrics. repub is nil — the Outbox resync catch-up is §2.5 path 3, not
// the reconnect leg. The slack matches the initial subscribe so backfill overlap
// is consistent (the Sequencer's id-dedup absorbs it either way).
func (w *relayWiring) newReconnectWatchdog(ls *dgstore.Store, send frameSender, alarm relay.AlarmFunc) (*relay.Watchdog, *relay.WatchdogMetrics) {
	m := &relay.WatchdogMetrics{}
	sub := &resubscriber{send: send, subID: relaySubID}
	wd := relay.NewWatchdog(sub, w.seq, ls, nil, m, alarm,
		relay.WithReconnectSlack(reconnectSlackSeconds),
		// dontguess-61a: bound both the reconnect backfill AND the periodic
		// resync audit to ONLY dontguess kinds, and thread the SAME durable
		// per-relay Intake cursor (nil if buildRelayWiring wasn't given
		// WithIntakeCursorPath) so the resync audit resumes from it instead of
		// re-flooding since=0 every cycle.
		relay.WithDontguessKinds(nostr.DontguessKinds),
		relay.WithIntakeCursor(w.intakeCursor),
	)
	return wd, m
}

// storeWatermarkSeconds is the LOCAL high-water mark (max persisted Timestamp) in
// NIP-01 created_at seconds — the value the reconnect REQ resumes backfill from.
// Timestamps are nanoseconds in the store; the relay's Since filter is seconds. A
// read error yields 0 (fetch from the beginning — a redundant backfill is free,
// dedup absorbs it; a silent narrow window would be the unsafe direction).
func storeWatermarkSeconds(ls *dgstore.Store) int64 {
	recs, err := ls.ReadAll()
	if err != nil {
		return 0
	}
	var wm int64
	for i := range recs {
		if recs[i].Timestamp > wm {
			wm = recs[i].Timestamp
		}
	}
	return wm / 1_000_000_000
}

// guardOperatorKeyMigration LOUD-warns when the relay is being enabled on a
// DG_HOME that already holds operator-authored records signed under a DIFFERENT
// operator key than the nostr relay identity now in force — the campfire-era →
// nostr operator-key switch (serve.go sets engineOperatorKey = relaySigner
// pubkey when the relay is on). The Outbox would RE-SIGN those historical records
// under the new key and republish them, re-attributing authorship; surfacing it
// LOUD (LOCKED-5) lets the operator migrate deliberately rather than silently
// republish old inventory under a new identity. It is non-fatal (a warning, not a
// brick): the operator may legitimately intend the switch. Returns the count of
// mismatched operator records. Relay-origin records are foreign and NEVER counted
// (they are not ours to republish); the legacy empty-Sender operator origin is
// treated as the current operator's, not a mismatch.
func guardOperatorKeyMigration(ls *dgstore.Store, operatorKeyHex string, logf func(format string, args ...any)) int {
	warn := logf
	if warn == nil {
		warn = func(format string, args ...any) { log.Printf(format, args...) }
	}
	recs, err := ls.ReadAll()
	if err != nil {
		warn("  relay migration guard: WARN could not read local store to check operator-key migration: %v", err)
		return 0
	}
	foreign := map[string]struct{}{}
	n := 0
	for i := range recs {
		r := recs[i]
		if !isOperatorOrigin(r.Origin) || r.Sender == "" || r.Sender == operatorKeyHex {
			continue
		}
		n++
		foreign[r.Sender] = struct{}{}
	}
	if n > 0 {
		warn("  relay migration guard: WARN %d operator-authored record(s) under %d prior operator key(s) differ from the nostr relay operator key %s… — enabling the relay will RE-SIGN and republish them under the new identity (campfire-era → nostr operator-key switch). Migrate deliberately if this is not intended.",
			n, len(foreign), operatorKeyHex[:min(16, len(operatorKeyHex))])
	}
	return n
}

// okResult carries a relay ACK verdict from the reader to the blocked publisher.
// dropped=true is the RECONNECT signal: the reader's connection dropped while the
// publish was in flight, so no OK will ever route for it — the publisher must
// FAIL (not block forever) and let the Outbox retry once the demux is
// re-established (§2.5, the "Outbox publish cannot wedge" invariant).
type okResult struct {
	accepted bool
	dropped  bool
}

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
		if r.dropped {
			// The reader lost the connection with this publish in flight. Return a
			// loud error (never block for an OK that can no longer arrive) so the
			// Outbox's publishWithRetry retries; after the reader re-subscribes the
			// demux is live again and the retried publish's OK routes normally. This
			// is what keeps ONE disconnect from wedging Outbox.Run forever (§2.5).
			return false, fmt.Errorf("demux publish: connection dropped awaiting OK for %s: %w", ev.ID, relay.ErrConnDropped)
		}
		return r.accepted, nil
	}
}

// failInFlight fails every publish currently blocked awaiting an OK, delivering
// the dropped signal so each PublishEvent returns an error (and the Outbox
// retries) rather than blocking forever on an OK that will never route. The
// reconnect loop calls it the instant the reader's Recv drops — BEFORE it
// re-subscribes — so the OK-demux cannot wedge across a disconnect (§2.5). The
// send is non-blocking on the buffered-1 waiter channel; a waiter whose OK
// already routed is a harmless no-op.
func (p *demuxPublisher) failInFlight() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, ch := range p.waiters {
		select {
		case ch <- okResult{dropped: true}:
		default:
		}
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
// aliasRegistrar (dontguess-55c GAP 1) is the exchange State's RegisterWireAlias,
// threaded to buildRelayWiring so both the restart-seed and the live Outbox record
// the wire→store id alias a team-tier buyer's settle antecedent needs. nil is a
// no-op (no alias wiring) — the individual tier and tests that don't exercise the
// wire-id settle pass nil.
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
	notifier *appendNotifier,
	aliasRegistrar func(wire, store string),
	opts ...relayWiringOption,
) (stop func(), err error) {
	pub := newDemuxPublisher(send)
	wiring, watermark, err := buildRelayWiring(ls, signer, operatorKeyHex, cursorPath, pub, 0, nil, aliasRegistrar, opts...)
	if err != nil {
		return nil, err
	}

	// Register this leg's Outbox.Notify with the shared fan-out (design §3.8, H1)
	// so an operator record folded into LocalStore wakes THIS leg's publish loop
	// immediately. nil notifier (e.g. a test that does not exercise the near-instant
	// publish path) simply skips registration — the Outbox still publishes on its
	// interval tick exactly as before.
	if notifier != nil {
		notifier.add(wiring.outbox.Notify)
	}

	// LOW: warn if enabling the relay would re-attribute campfire-era operator
	// records under the new nostr operator key (non-fatal, LOUD).
	guardOperatorKeyMigration(ls, operatorKeyHex, logf)

	// Backfill + live subscription: resume from the seeded watermark with slack;
	// the Sequencer dedups the overlap (§2.5). Timestamp is nanoseconds; nostr
	// created_at (Since) is seconds.
	//
	// dontguess-61a: prefer the durable per-relay Intake cursor (re-read from its
	// on-disk sidecar by buildRelayWiring, NOT memory) over the local-store
	// watermark when it has advanced further — the cursor tracks THIS relay's
	// ingest progress specifically, while the store watermark blends every
	// attached relay's history. A genuinely fresh operator/relay pair (both the
	// cursor and the local store are empty) falls back to a BOUNDED backfill
	// window instead of Since=0 — the documented "pre-bootstrap entries not
	// ingested" semantic — so the very first subscribe of a brand-new operator
	// never re-reads a relay's entire retained history. Kinds is ALWAYS bounded
	// to nostr.DontguessKinds regardless of which since branch is taken: this is
	// what actually closes the dropped_smuggled flood (a relay serving every
	// kind any client has ever published there).
	storeSince := watermark/1_000_000_000 - reconnectSlackSeconds
	if storeSince < 0 {
		storeSince = 0
	}
	since := storeSince
	if wiring.intakeCursor != nil {
		if cv := wiring.intakeCursor.Value(); cv > 0 {
			cursorSince := cv - reconnectSlackSeconds
			if cursorSince < 0 {
				cursorSince = 0
			}
			if cursorSince > since {
				since = cursorSince
			}
		} else if storeSince == 0 {
			// Neither the cursor nor the local store has ever seen anything from
			// this relay: bound the very first backfill instead of Since=0.
			since = time.Now().Unix() - relay.DefaultBackfillWindowSeconds
			if since < 0 {
				since = 0
			}
		}
	}
	reqFrame, err := relay.EncodeReq(relaySubID, relay.Filter{Since: &since, Kinds: nostr.DontguessKinds})
	if err != nil {
		return nil, fmt.Errorf("relay attach: encode REQ: %w", err)
	}
	if err := send.Send(ctx, reqFrame); err != nil {
		return nil, fmt.Errorf("relay attach: send REQ: %w", err)
	}

	// The reconnect-leg Watchdog re-issues the REQ (over send) + drives the
	// intake_disconnected alarm on every drop (§2.5), and runs the periodic
	// resync audit (§2.5 path 3) — both bounded to nostr.DontguessKinds and, for
	// the audit, resuming from the SAME durable per-relay Intake cursor rather
	// than an unconditional since=0 every cycle (dontguess-61a).
	wd, _ := wiring.newReconnectWatchdog(ls, send, nil)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		wiring.runReaderReconnect(ctx, recv, pub, wd, func() int64 { return storeWatermarkSeconds(ls) }, relay.DefaultBackoff)
	}()
	go func() { defer wg.Done(); wiring.outbox.Run(ctx, publishInterval) }()

	if logf != nil {
		logf("  relay transport: attached (subscribe since=%d, publish interval=%s)", since, publishInterval)
	}
	return func() { wg.Wait() }, nil
}

// shutdownRelayTransport tears down an attached relay transport in the ONLY
// safe order: cancel the context FIRST, then close the underlying connection,
// then wait for the reader/outbox goroutines to actually exit. stop (the
// wg.Wait() returned by attachRelayTransport) blocks until those goroutines
// observe ctx.Done() and return — calling stop() before cancel() hangs
// forever (dontguess-e35). closeConn may be nil (e.g. a fake connection in
// tests with nothing to close); cancel and stop must not be nil.
func shutdownRelayTransport(cancel context.CancelFunc, closeConn func() error, stop func()) {
	cancel()
	if closeConn != nil {
		_ = closeConn()
	}
	stop()
}

// reconnectSlackSeconds is the backfill overlap (seconds) subtracted from the
// watermark on (re)subscribe so no event straddling the cursor is missed; the
// Sequencer dedups the redelivered overlap (§2.5).
const reconnectSlackSeconds = int64(60)

// relayLeg is one successfully-attached relay transport leg's shutdown
// handles: the live connection (closed first so in-flight IO unblocks) and
// the stop func attachRelayTransport returned (waits for its goroutines).
type relayLeg struct {
	conn *relay.Conn
	stop func()
}

// relayAttachInitialBackoff / relayAttachMaxBackoff bound the retry schedule
// attachRelayLegsAsync uses when a leg's attachRelayTransport call fails
// (typically the initial REQ Send / dial+auth against an unreachable relay).
// Package vars so tests can shrink them for deterministic, fast retry
// coverage without touching production behavior.
var (
	relayAttachInitialBackoff = 2 * time.Second
	relayAttachMaxBackoff     = 30 * time.Second
)

// attachRelayLegsAsync attaches one relay transport leg per URL in relayURLs,
// each in its OWN background retry goroutine (dontguess-347, design §4/§9 Gate
// A/P1). This is what keeps a dead/slow relay from blocking serve startup: the
// operator socket (bound by bindOperatorSocket, BEFORE this is called) and the
// engine loop come up immediately, while each leg's dial + NIP-42 handshake +
// restart-seed (seedEmittedFromStore) + migration guard
// (guardOperatorKeyMigration) + initial REQ Send run off the startup path. A
// leg that fails to attach (attachRelayTransport returns an error — most
// commonly the initial REQ Send blocking/failing against an unreachable
// relay) is retried with exponential backoff, bounded by
// relayAttachInitialBackoff/relayAttachMaxBackoff, until it succeeds or ctx is
// cancelled. wg is incremented once per relay URL and Done exactly once when
// that URL's goroutine exits (success or ctx cancellation) — callers wait on
// it during shutdown, AFTER cancelling ctx, so no goroutine is left running
// past the caller's cleanup. Successfully-attached legs are appended to
// *legs under legsMu (the slice may be read concurrently by the caller's
// shutdown path once wg.Wait() returns, so all writes happen only under the
// lock and all appends happen-before the corresponding wg.Done()).
func attachRelayLegsAsync(
	ctx context.Context,
	wg *sync.WaitGroup,
	legsMu *sync.Mutex,
	legs *[]relayLeg,
	relayURLs []string,
	localStore *dgstore.Store,
	relaySigner *identity.Secp256k1Identity,
	localStorePath string,
	appendNotify *appendNotifier,
	eng *exchange.Engine,
	logger *log.Logger,
	climbWatermark int64,
) {
	for _, relayURL := range relayURLs {
		relayURL := relayURL
		wg.Add(1)
		go func() {
			defer wg.Done()
			backoff := relayAttachInitialBackoff
			for {
				if ctx.Err() != nil {
					return
				}
				// WithoutClientAuth: match the client side (relayclient.go DEFAULT)
				// and the production strfry relays, which gate writes by a
				// signed-author allowlist and never push a NIP-42 AUTH challenge.
				// Without this the leg blocks forever in dialAndAuth on a challenge
				// that never arrives (conn.go §WithoutClientAuth) — now confined to
				// this async retry goroutine, never the startup path.
				conn := relay.New(relayURL, relaySigner, relay.WithoutClientAuth())
				stop, aerr := attachRelayTransport(ctx, localStore, relaySigner, relaySigner.PubKeyHex(),
					relayCursorPath(localStorePath, relayURL), conn, conn, 5*time.Second, logger.Printf, appendNotify,
					eng.State().RegisterWireAlias,
					WithIntakeCursorPath(intakeCursorPath(localStorePath, relayURL)),
					WithClimbWatermark(climbWatermark))
				if aerr != nil {
					_ = conn.Close()
					if ctx.Err() != nil {
						return
					}
					logger.Printf("  relay:     %s attach failed (retrying in %s): %v", relayURL, backoff, aerr)
					select {
					case <-ctx.Done():
						return
					case <-time.After(backoff):
					}
					if backoff *= 2; backoff > relayAttachMaxBackoff {
						backoff = relayAttachMaxBackoff
					}
					continue
				}
				legsMu.Lock()
				*legs = append(*legs, relayLeg{conn: conn, stop: stop})
				legsMu.Unlock()
				logger.Printf("  relay:     %s (operator npub %s)", relayURL, relaySigner.Npub())
				return
			}
		}()
	}
}
