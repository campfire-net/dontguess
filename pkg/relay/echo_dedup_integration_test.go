package relay

// echo_dedup_integration_test.go composes the REAL operator-echo chain end to
// end — Outbox publish → relay echo → Intake subscriber → shared Sequencer — to
// prove the seed-before-publish dedup (docs/design/relay-transport.md §D). No
// half of the chain is stubbed: the events are genuinely Schnorr-signed, the
// Sequencer and both Stores are real, and the echo runs through the production
// Intake.HandleEvent pipeline.
//
// The property under test (a wave-9-review HIGH, reworked here): the operator
// folds its own event locally (RF=1) and the Outbox later publishes it; the relay
// echoes that event back to the operator's own subscriber. If the emitted-set is
// seeded with the SIGNED content-hash id STRICTLY BEFORE publish, the echo dedups
// in Sequencer.Ingest and never re-folds (no double scrip credit). If the seed
// lands AFTER publish, a concurrent echo re-folds — the double-fold this reworks.

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/campfire-net/dontguess/pkg/exchange"
	"github.com/campfire-net/dontguess/pkg/identity"
	"github.com/campfire-net/dontguess/pkg/nostr"
	"github.com/campfire-net/dontguess/pkg/store"
)

// identityToWireEvent copies a signed identity.Event into the wire nostr.Event a
// relay echoes back to a subscriber. identity.Event and nostr.Event are
// structurally identical; this is the same field-for-field shape the production
// Intake receives off the wire, INCLUDING the Schnorr sig — so the echo passes
// the universal signature floor (STEP 0) with a genuinely-valid signature.
func identityToWireEvent(ev *identity.Event) *nostr.Event {
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

// hookPublisher is an EventPublisher that always ACKs and, on every publish,
// invokes hook(ev) — the seam the tests use to inject the relay echo. It records
// every published (signed) id so a test can assert what id the relay actually saw
// on the wire (the content-hash id, not the pre-sign store id).
type hookPublisher struct {
	mu   sync.Mutex
	ids  []string
	hook func(ev *identity.Event)
}

func (p *hookPublisher) PublishEvent(_ context.Context, ev *identity.Event) (bool, error) {
	p.mu.Lock()
	p.ids = append(p.ids, ev.ID)
	p.mu.Unlock()
	if p.hook != nil {
		p.hook(ev)
	}
	return true, nil
}

func (p *hookPublisher) publishedIDs() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.ids))
	copy(out, p.ids)
	return out
}

// echoChain wires the full real chain and returns the pieces a test asserts on.
// The Outbox log store and the Intake persist store are DISTINCT real on-disk
// stores (as in production: the operator's local fold log vs the relay-ingest
// append target); the Sequencer is SHARED — it is the single dedup authority both
// legs consult.
type echoChain struct {
	outbox      *Outbox
	outStore    *store.Store
	seq         *exchange.Sequencer
	intake      *Intake
	intakeStore *store.Store
	intakeMx    *IntakeMetrics
	pub         *hookPublisher
}

// newEchoChain builds the chain. seedBeforePublish selects the ORDERING that is
// the single mutated variable between the correctness test and its bug twin:
//
//   - true  (production): WithEmittedSeeder(seq.MarkEmitted) — the Outbox seeds
//     the emitted-set with the signed id BEFORE publishWithRetry, so the echo
//     (which can only arrive after publish) dedups.
//   - false (bug twin): NO seeder is wired; instead the echo hook seeds
//     seq.MarkEmitted AFTER it has already fed the echo through Intake — modeling
//     seed-AFTER-publish, which cannot stop the echo from re-folding.
//
// echoHook performs the actual echo delivery (inline, or onto a channel a
// subscriber goroutine drains); it is passed ev and must feed
// identityToWireEvent(ev) through the Intake. seededIDs (if non-nil) records every
// id the seeder observed so a test can assert the seed used the signed
// content-hash id.
func newEchoChain(t *testing.T, seedBeforePublish bool, echoHook func(ev *identity.Event), seededIDs *[]string) *echoChain {
	t.Helper()

	signer, err := identity.Generate()
	if err != nil {
		t.Fatalf("identity.Generate: %v", err)
	}

	dir := t.TempDir()
	outPath := filepath.Join(dir, "out.jsonl")
	outStore, err := store.Open(outPath)
	if err != nil {
		t.Fatalf("open out store: %v", err)
	}
	t.Cleanup(func() { _ = outStore.Close() })

	intakeStore, err := store.Open(filepath.Join(dir, "intake.jsonl"))
	if err != nil {
		t.Fatalf("open intake store: %v", err)
	}
	t.Cleanup(func() { _ = intakeStore.Close() })

	seq := exchange.NewSequencer(0)
	mx := &IntakeMetrics{}
	// operatorKey is the Outbox signer's own key. Put(3401) is a non-operator
	// kind so authorship is not gated on it, but wiring the real key keeps the
	// harness coherent and reusable for operator kinds.
	in := NewIntake(seq, intakeStore, signer.PubKeyHex(), mx, func(string, error, *nostr.Event) {})

	c := &echoChain{
		outStore:    outStore,
		seq:         seq,
		intake:      in,
		intakeStore: intakeStore,
		intakeMx:    mx,
	}

	c.pub = &hookPublisher{hook: func(ev *identity.Event) {
		echoHook(ev)
		if !seedBeforePublish {
			// BUG TWIN: seed lands AFTER the echo has already been ingested.
			if seededIDs != nil {
				*seededIDs = append(*seededIDs, ev.ID)
			}
			seq.MarkEmitted(ev.ID)
		}
	}}

	opts := []OutboxOption{WithOutboxLogf(func(string, ...interface{}) {})}
	if seedBeforePublish {
		opts = append(opts, WithEmittedSeeder(func(id string) {
			if seededIDs != nil {
				*seededIDs = append(*seededIDs, id)
			}
			seq.MarkEmitted(id)
		}))
	}
	ob, err := NewOutbox(outStore, signer, c.pub, outPath+".pubcursor", opts...)
	if err != nil {
		t.Fatalf("NewOutbox: %v", err)
	}
	c.outbox = ob
	return c
}

// TestEchoDedup_SeedBeforePublish_DedupsEcho is the correctness path. The Outbox
// seeds the emitted-set with the signed content-hash id before publishing; the
// relay echoes the event straight back into the real Intake. Because the seed
// happens-before the echo, Sequencer.Ingest dedups it: the Intake persists NOTHING
// (no double-fold, no double scrip credit). It also asserts the seeded id is the
// SIGNED id the relay saw on the wire — NOT the pre-signature store record id —
// which is the earlier wave-8b bug (pre-sign id != echo id => no dedup).
func TestEchoDedup_SeedBeforePublish_DedupsEcho(t *testing.T) {
	var seeded []string
	var c *echoChain
	c = newEchoChain(t, true, func(ev *identity.Event) {
		// Relay echo: feed the identical signed event back through the REAL Intake
		// pipeline (sig floor -> adapter -> authorship -> Sequencer.Ingest -> Drain
		// -> persist). Inline delivery here reflects production's true
		// happens-before: seed (inside Tick, before publishWithRetry) strictly
		// precedes this echo.
		if err := c.intake.HandleEvent(identityToWireEvent(ev)); err != nil {
			t.Errorf("intake echo HandleEvent: %v", err)
		}
	}, &seeded)

	rec := localRec("put-1")
	if err := c.outStore.Append(rec); err != nil {
		t.Fatalf("append local record: %v", err)
	}
	if err := c.outbox.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	// The echo was deduped: nothing folded from the relay leg.
	if got := c.intakeMx.Persisted.Load(); got != 0 {
		t.Fatalf("Intake persisted %d records from the echo, want 0 — the operator's own event double-folded", got)
	}
	if n := storeLen(t, c.intakeStore); n != 0 {
		t.Fatalf("intake store has %d records, want 0 — echo re-folded despite seed-before-publish", n)
	}
	// Received counts that the echo reached Intake at all (the chain really ran),
	// so the 0-persist result is a genuine Sequencer dedup, not an upstream drop.
	if got := c.intakeMx.Received.Load(); got != 1 {
		t.Fatalf("Intake received %d events, want exactly 1 (the echo) — chain did not compose", got)
	}

	// The seed used the SIGNED content-hash id, which the relay also saw on the
	// wire — and which DIFFERS from the pre-signature store record id.
	if len(seeded) != 1 {
		t.Fatalf("seeder invoked %d times, want 1", len(seeded))
	}
	pub := c.pub.publishedIDs()
	if len(pub) != 1 {
		t.Fatalf("published %d events, want 1", len(pub))
	}
	if seeded[0] != pub[0] {
		t.Fatalf("seeded id %q != published (signed) id %q — seed did not use the wire id", seeded[0], pub[0])
	}
	if seeded[0] == rec.ID {
		t.Fatalf("seeded id equals the PRE-SIGN store record id %q — regression: seed must use the signed content-hash id", rec.ID)
	}
}

// TestEchoDedup_SeedAfterPublish_DoubleFolds_MutationTwin is the mutation twin:
// it flips the SINGLE variable (seed ordering) to seed-AFTER the echo and proves
// the assertion above is load-bearing. With the seed landing after the echo has
// already been ingested, the operator's own event re-folds from the relay leg —
// the Intake persists it a SECOND time (double scrip credit). If this test ever
// went green (0 persisted), the correctness test above would be vacuous.
func TestEchoDedup_SeedAfterPublish_DoubleFolds_MutationTwin(t *testing.T) {
	var c *echoChain
	c = newEchoChain(t, false, func(ev *identity.Event) {
		if err := c.intake.HandleEvent(identityToWireEvent(ev)); err != nil {
			t.Errorf("intake echo HandleEvent: %v", err)
		}
	}, nil)

	if err := c.outStore.Append(localRec("put-1")); err != nil {
		t.Fatalf("append local record: %v", err)
	}
	if err := c.outbox.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	// The echo was NOT deduped (seed too late): it folded a second copy.
	if got := c.intakeMx.Persisted.Load(); got != 1 {
		t.Fatalf("Intake persisted %d records, want 1 — mutation twin must double-fold to prove the dedup test is sensitive", got)
	}
	if n := storeLen(t, c.intakeStore); n != 1 {
		t.Fatalf("intake store has %d records, want 1 (the double-fold)", n)
	}
}

// TestEchoDedup_ConcurrentSubscriberRace runs a real CONCURRENT Intake subscriber
// goroutine racing the Outbox publish, under the production seed-before-publish
// ordering, over many events. The relay echo is delivered on a SEPARATE goroutine
// (as the live subscription is), so the Sequencer is touched concurrently:
// MarkEmitted on the Outbox goroutine, Ingest/Drain on the subscriber goroutine.
// Because the seed happens-before the publish (and thus before the echo is even
// enqueued), no interleaving can let an echo re-fold: the Intake must persist
// ZERO records. Run under -race, this also proves the shared-Sequencer access is
// data-race-free.
func TestEchoDedup_ConcurrentSubscriberRace(t *testing.T) {
	const n = 50

	echoCh := make(chan *nostr.Event, n)
	c := newEchoChain(t, true, func(ev *identity.Event) {
		// Hand the echo to the concurrent subscriber (buffered n, never blocks).
		echoCh <- identityToWireEvent(ev)
	}, nil)

	var wg sync.WaitGroup
	wg.Add(1)
	var subErrs []error
	var subMu sync.Mutex
	go func() {
		defer wg.Done()
		for ev := range echoCh {
			if err := c.intake.HandleEvent(ev); err != nil {
				subMu.Lock()
				subErrs = append(subErrs, err)
				subMu.Unlock()
			}
		}
	}()

	for i := 0; i < n; i++ {
		if err := c.outStore.Append(localRec(fmt.Sprintf("put-%02d", i))); err != nil {
			t.Fatalf("append local record %d: %v", i, err)
		}
	}
	if err := c.outbox.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	close(echoCh)
	wg.Wait()

	subMu.Lock()
	errs := append([]error(nil), subErrs...)
	subMu.Unlock()
	for _, e := range errs {
		// A dedup is a Sequencer no-op that returns nil, so any error here means
		// the echo took an unexpected reject/persist path — surface it.
		t.Errorf("concurrent echo HandleEvent error: %v", e)
	}

	if got := c.intakeMx.Persisted.Load(); got != 0 {
		t.Fatalf("Intake persisted %d records under the concurrent race, want 0 — an echo double-folded", got)
	}
	if sl := storeLen(t, c.intakeStore); sl != 0 {
		t.Fatalf("intake store has %d records under the concurrent race, want 0", sl)
	}
	if got := c.intakeMx.Received.Load(); got != int64(n) {
		t.Fatalf("Intake received %d echoes, want %d — not every publish echoed through the subscriber", got, n)
	}
}
