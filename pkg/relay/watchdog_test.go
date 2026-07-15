package relay

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/3dl-dev/dontguess/pkg/exchange"
	"github.com/3dl-dev/dontguess/pkg/identity"
	"github.com/3dl-dev/dontguess/pkg/nostr"
	"github.com/3dl-dev/dontguess/pkg/store"
)

// --- in-process fake relay -------------------------------------------------
//
// fakeRelay is a durable event log the watchdog drives its three REQs against.
// It is NOT a mock of the Intake: its Query implementation feeds every matching
// event through the REAL Intake (real Sequencer + real Store), so the dedup,
// causal-ordering, and persistence the watchdog relies on are exercised end to
// end. Only the wire (Conn + frame codec) is faked — the auth/sequencing/persist
// pipeline under test is real.
type fakeRelay struct {
	held   []*nostr.Event // everything the relay currently serves, in publish order
	intake *Intake        // where delivered events are fed

	// serveNone, when set for a specific antecedent id, makes an IDs-filtered
	// REQ for that id return empty even though a matching event is "held" — it
	// models a relay that has PRUNED the antecedent (the poison case).
	pruned map[string]struct{}

	queries []Filter // every REQ issued, for assertions
}

func (r *fakeRelay) Query(ctx context.Context, f Filter) ([]string, error) {
	r.queries = append(r.queries, f)
	var delivered []string
	for _, ev := range r.held {
		if !r.match(f, ev) {
			continue
		}
		// Feed through the REAL Intake — this is where dedup + persist happen.
		// An ingest drop (forged/unsigned/orphan-overflow) is the Intake's loud
		// business, not the relay's; the relay still reports it as delivered.
		_ = r.intake.HandleEvent(ev)
		delivered = append(delivered, ev.ID)
	}
	return delivered, nil
}

// match applies the subset of NIP-01 filter semantics the watchdog uses:
// IDs (exact-id targeted refetch) and Since (created_at floor). A pruned id is
// never delivered to an IDs query, modelling a relay that no longer serves it.
func (r *fakeRelay) match(f Filter, ev *nostr.Event) bool {
	if len(f.IDs) > 0 {
		hit := false
		for _, id := range f.IDs {
			if id == ev.ID {
				hit = true
				break
			}
		}
		if !hit {
			return false
		}
		if _, gone := r.pruned[ev.ID]; gone {
			return false
		}
	}
	if f.Since != nil && ev.CreatedAt < *f.Since {
		return false
	}
	return true
}

// --- test helpers ----------------------------------------------------------

// signEventAt is signEvent with a caller-chosen created_at so tests can drive
// the Since backfill floor. Signature is a real BIP-340 signature.
func signEventAt(t *testing.T, signer identity.Signer, kind int, createdAt int64, tags [][]string, content string) *nostr.Event {
	t.Helper()
	ie := &identity.Event{
		CreatedAt: createdAt,
		Kind:      kind,
		Tags:      tags,
		Content:   content,
	}
	if err := identity.SignEvent(signer, ie); err != nil {
		t.Fatalf("SignEvent(kind=%d): %v", kind, err)
	}
	return &nostr.Event{
		ID:        ie.ID,
		PubKey:    ie.PubKey,
		CreatedAt: ie.CreatedAt,
		Kind:      ie.Kind,
		Tags:      ie.Tags,
		Content:   ie.Content,
		Sig:       ie.Sig,
	}
}

// eTag builds the NIP-01 reply e-tag that FromNostrEvent maps to
// Message.Antecedents[0] (adapter.go) — the causal edge the sequencer orphans on.
func eTag(anteID string) []string { return []string{"e", anteID, "", "reply"} }

// newWatchdogHarness wires a real Store + Sequencer + Intake + fakeRelay + a
// Watchdog whose Subscriber is that relay. Alarm classes are recorded.
func newWatchdogHarness(t *testing.T, opts ...WatchdogOption) (*Watchdog, *fakeRelay, *store.Store, *exchange.Sequencer, *WatchdogMetrics, *[]string, identity.Signer) {
	t.Helper()
	op, err := identity.Generate()
	if err != nil {
		t.Fatalf("generate operator: %v", err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "watchdog.log"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	seq := exchange.NewSequencer(0)
	im := &IntakeMetrics{}
	intake := NewIntake(seq, st, op.PubKeyHex(), im, nil)

	relay := &fakeRelay{intake: intake, pruned: map[string]struct{}{}}

	var alarms []string
	wm := &WatchdogMetrics{}
	alarm := func(class string, _ error, _ *nostr.Event) {
		alarms = append(alarms, class)
	}
	wd := NewWatchdog(relay, seq, st, nil, wm, alarm, opts...)
	return wd, relay, st, seq, wm, &alarms, op
}

func storeIDs(t *testing.T, st *store.Store) []string {
	t.Helper()
	recs, err := st.ReadAll()
	if err != nil {
		t.Fatalf("store ReadAll: %v", err)
	}
	ids := make([]string, len(recs))
	for i, r := range recs {
		ids[i] = r.ID
	}
	sort.Strings(ids)
	return ids
}

// --- TEST 1: reconnect with dedup-absorbed overlapping backfill -------------

// TestWatchdog_ReconnectDedupAbsorbedBackfill exercises §2.5 path 1: a live
// disconnect, then Reconnect re-issues REQ since=(watermark−slack). The relay
// re-delivers the events seen before the drop (the overlap) AND a new event that
// arrived while disconnected (the gap). The Sequencer's id-dedup must absorb the
// overlap — every event persists EXACTLY once — and the gap event must land.
func TestWatchdog_ReconnectDedupAbsorbedBackfill(t *testing.T) {
	wd, relay, st, _, wm, alarms, op := newWatchdogHarness(t)
	seller, err := identity.Generate()
	if err != nil {
		t.Fatalf("generate seller: %v", err)
	}

	// Three independent puts arrive live (root events, no antecedents), then the
	// subscription drops. Timestamps ascend so the backfill floor is meaningful.
	e1 := signEventAt(t, seller, nostr.KindPut, 1000, nil, "put-1")
	e2 := signEventAt(t, seller, nostr.KindPut, 1010, nil, "put-2")
	e3 := signEventAt(t, seller, nostr.KindPut, 1020, nil, "put-3")
	relay.held = []*nostr.Event{e1, e2, e3}
	// Live delivery of all three (simulating the pre-drop subscription).
	for _, ev := range relay.held {
		if herr := relay.intake.HandleEvent(ev); herr != nil {
			t.Fatalf("live ingest of %s: %v", ev.ID, herr)
		}
	}
	if got := storeIDs(t, st); len(got) != 3 {
		t.Fatalf("pre-drop store has %d records, want 3", len(got))
	}
	watermark := int64(1020) // max created_at seen

	// While disconnected, a NEW put (the gap) is published to the relay with a
	// created_at INSIDE the slack window below the watermark — proving the
	// backfill floor (watermark−slack) actually re-scans the overlap region.
	gap := signEventAt(t, seller, nostr.KindPut, 1015, nil, "put-gap")
	relay.held = append(relay.held, gap)

	// Reconnect (default slack 300) → since = 1020 − 300 = 720, sweeping
	// e1..e3 (1000-1020) + gap (1015).
	if err := wd.Reconnect(context.Background(), watermark); err != nil {
		t.Fatalf("Reconnect: %v", err)
	}
	last := relay.queries[len(relay.queries)-1]
	if last.Since == nil || *last.Since != 720 {
		t.Fatalf("reconnect REQ since = %v, want 720 (watermark−slack)", last.Since)
	}
	_ = alarms

	// Dedup absorbed the overlap: exactly four distinct records, no duplicates.
	got := storeIDs(t, st)
	want := []string{e1.ID, e2.ID, e3.ID, gap.ID}
	sort.Strings(want)
	if len(got) != 4 {
		t.Fatalf("post-reconnect store has %d records, want 4 (3 overlap deduped + 1 gap)", len(got))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("store id[%d] = %s, want %s", i, got[i], want[i])
		}
	}
	if wm.IntakeDisconnected.Load() != 1 {
		t.Fatalf("IntakeDisconnected = %d, want 1", wm.IntakeDisconnected.Load())
	}
	// op is unused beyond harness wiring for this path.
	_ = op
}

// TestWatchdog_CheckOrphansRefetchBoundsKinds directly pins the Kinds bound on
// the CheckOrphans targeted-antecedent refetch (watchdog.go:446,
// Query(Filter{IDs, Kinds: w.kinds})). The other 3 REQ sites (Reconnect,
// resync audit) already have a direct Kinds assertion; this one only had
// incidental coverage. Wires WithDontguessKinds and asserts the exact []int
// the emitted targeted-refetch Filter carries — a regression that dropped
// Kinds there (unbounding the refetch to all kinds) would NOT be caught
// without this.
func TestWatchdog_CheckOrphansRefetchBoundsKinds(t *testing.T) {
	boundKinds := []int{nostr.KindPut, nostr.KindBuy}
	wd, relay, _, seq, _, _, _ := newWatchdogHarness(t, WithDontguessKinds(boundKinds))
	seller, err := identity.Generate()
	if err != nil {
		t.Fatalf("generate seller: %v", err)
	}

	// A dangling antecedent the relay will never serve — this is the exact
	// path that drives the CheckOrphans targeted refetch at watchdog.go:446.
	ante := signEventAt(t, seller, nostr.KindPut, 700, nil, "root").ID
	relay.pruned[ante] = struct{}{}
	orphan := signEventAt(t, seller, nostr.KindBuy, 800, [][]string{eTag(ante)}, "orphan")
	if herr := relay.intake.HandleEvent(orphan); herr != nil {
		t.Fatalf("ingest orphan: %v", herr)
	}
	if seq.PendingCount() != 1 {
		t.Fatalf("PendingCount = %d, want 1", seq.PendingCount())
	}

	if _, err := wd.CheckOrphans(context.Background()); err != nil {
		t.Fatalf("CheckOrphans: %v", err)
	}

	var refetch *Filter
	for i := range relay.queries {
		if len(relay.queries[i].IDs) == 1 && relay.queries[i].IDs[0] == ante {
			refetch = &relay.queries[i]
		}
	}
	if refetch == nil {
		t.Fatalf("no targeted REQ ids=[%s] was issued; queries=%v", ante, relay.queries)
	}
	if len(refetch.Kinds) != len(boundKinds) {
		t.Fatalf("CheckOrphans refetch Kinds = %v, want %v", refetch.Kinds, boundKinds)
	}
	for i, k := range boundKinds {
		if refetch.Kinds[i] != k {
			t.Fatalf("CheckOrphans refetch Kinds = %v, want %v", refetch.Kinds, boundKinds)
		}
	}
}

// --- TEST 2: poison antecedent → targeted refetch empty → loud, NO quarantine -

// TestWatchdog_PoisonAntecedentLoudNoQuarantine exercises §2.5a path 2: an
// ingested event references an antecedent the relay does NOT serve. The event
// orphans in the Sequencer (never persists). CheckOrphans issues ONE targeted
// REQ ["ids", <antecedent>]; the refetch is empty; the watchdog ALARMS loud
// (orphan_unrecoverable) while an INDEPENDENT healthy event keeps draining.
//
// The ratified §2.5a design REMOVED the ingest-gating quarantine set (a re-parent
// black hole + a false-quarantine censorship primitive). This test PINS that
// removal: a second pass RE-refetches the still-missing antecedent (no quarantine
// memory suppresses it) — the watchdog never permanently gives up on an id; a
// truly stuck orphan is bounded instead by the Sequencer's LRU occupancy eviction
// and reconciled by the resync audit.
func TestWatchdog_PoisonAntecedentLoudNoQuarantine(t *testing.T) {
	wd, relay, st, seq, wm, alarms, _ := newWatchdogHarness(t)
	seller, err := identity.Generate()
	if err != nil {
		t.Fatalf("generate seller: %v", err)
	}

	// A dangling antecedent id the relay will NEVER serve.
	poisonAnte := signEventAt(t, seller, nostr.KindPut, 500, nil, "pruned-root").ID
	relay.pruned[poisonAnte] = struct{}{}

	// Orphan: a buy that e-tags the never-served root. It ingests but cannot release.
	orphan := signEventAt(t, seller, nostr.KindBuy, 600, [][]string{eTag(poisonAnte)}, "orphaned-buy")
	if herr := relay.intake.HandleEvent(orphan); herr != nil {
		t.Fatalf("ingest orphan: %v", herr)
	}
	// A healthy INDEPENDENT put that must keep folding despite the poison chain.
	healthy := signEventAt(t, seller, nostr.KindPut, 610, nil, "healthy-put")
	if herr := relay.intake.HandleEvent(healthy); herr != nil {
		t.Fatalf("ingest healthy: %v", herr)
	}

	// The orphan never persisted; the healthy event did.
	if ids := storeIDs(t, st); len(ids) != 1 || ids[0] != healthy.ID {
		t.Fatalf("pre-check store = %v, want only healthy %s", ids, healthy.ID)
	}
	if seq.PendingCount() != 1 {
		t.Fatalf("PendingCount = %d, want 1 (the orphan)", seq.PendingCount())
	}
	// The Sequencer names the exact missing antecedent for the targeted refetch.
	pend := seq.PendingAntecedents()
	if deps, ok := pend[poisonAnte]; !ok || len(deps) != 1 || deps[0] != orphan.ID {
		t.Fatalf("PendingAntecedents[%s] = %v, want [%s]", poisonAnte, pend[poisonAnte], orphan.ID)
	}

	// Run the orphan watchdog. Targeted REQ ["ids", poisonAnte] returns empty →
	// loud orphan_unrecoverable, ONE refetch issued, NO quarantine.
	refetched, err := wd.CheckOrphans(context.Background())
	if err != nil {
		t.Fatalf("CheckOrphans: %v", err)
	}
	if refetched != 1 {
		t.Fatalf("refetches issued = %d, want 1 (one distinct missing antecedent)", refetched)
	}
	// Exactly one targeted refetch was issued, an IDs filter for the antecedent.
	var refetch *Filter
	for i := range relay.queries {
		if len(relay.queries[i].IDs) == 1 && relay.queries[i].IDs[0] == poisonAnte {
			refetch = &relay.queries[i]
		}
	}
	if refetch == nil {
		t.Fatalf("no targeted REQ ids=[%s] was issued; queries=%v", poisonAnte, relay.queries)
	}
	if wm.OrphanUnrecoverable.Load() != 1 {
		t.Fatalf("OrphanUnrecoverable = %d, want 1", wm.OrphanUnrecoverable.Load())
	}
	if wm.OrphanPending.Load() != 1 {
		t.Fatalf("OrphanPending gauge = %d, want 1 (orphan still buffered)", wm.OrphanPending.Load())
	}
	if !containsID(*alarms, "orphan_unrecoverable") {
		t.Fatalf("alarms = %v, want an orphan_unrecoverable", *alarms)
	}

	// The healthy independent event kept draining — poison did not stall the fold.
	if ids := storeIDs(t, st); !containsID(ids, healthy.ID) {
		t.Fatalf("healthy event no longer persisted after CheckOrphans: %v", ids)
	}

	// A SECOND pass RE-refetches the still-missing antecedent: there is NO
	// quarantine memory (the §2.5a removal). This is the anti-censorship property —
	// the watchdog does not permanently blacklist a foreign antecedent id.
	refetchCountBefore := wm.OrphanRefetch.Load()
	refetched2, err := wd.CheckOrphans(context.Background())
	if err != nil {
		t.Fatalf("CheckOrphans (2nd): %v", err)
	}
	if refetched2 != 1 {
		t.Fatalf("2nd pass refetches = %d, want 1 (still-missing antecedent re-tried, no quarantine)", refetched2)
	}
	if wm.OrphanRefetch.Load() != refetchCountBefore+1 {
		t.Fatalf("OrphanRefetch did not grow on 2nd pass (%d→%d); the removed quarantine set must NOT suppress retry",
			refetchCountBefore, wm.OrphanRefetch.Load())
	}
	if wm.OrphanUnrecoverable.Load() != 2 {
		t.Fatalf("OrphanUnrecoverable = %d, want 2 (loud each pass; no quarantine to suppress it)", wm.OrphanUnrecoverable.Load())
	}
}

// TestWatchdog_OrphanRecoveredByRefetch is the positive half of path 2: the
// antecedent is NOT pruned, so the targeted refetch delivers it, the Intake
// releases the whole chain, and NOTHING is quarantined.
func TestWatchdog_OrphanRecoveredByRefetch(t *testing.T) {
	wd, relay, st, seq, wm, alarms, _ := newWatchdogHarness(t)
	seller, err := identity.Generate()
	if err != nil {
		t.Fatalf("generate seller: %v", err)
	}

	root := signEventAt(t, seller, nostr.KindPut, 700, nil, "recoverable-root")
	orphan := signEventAt(t, seller, nostr.KindBuy, 710, [][]string{eTag(root.ID)}, "buy-on-root")
	// The relay HOLDS the root (serveable), but only the orphan was delivered live.
	relay.held = []*nostr.Event{root, orphan}
	if herr := relay.intake.HandleEvent(orphan); herr != nil {
		t.Fatalf("ingest orphan: %v", herr)
	}
	if seq.PendingCount() != 1 {
		t.Fatalf("PendingCount = %d, want 1", seq.PendingCount())
	}

	refetched, err := wd.CheckOrphans(context.Background())
	if err != nil {
		t.Fatalf("CheckOrphans: %v", err)
	}
	if refetched != 1 {
		t.Fatalf("refetches = %d, want 1 (one distinct antecedent, recovered)", refetched)
	}
	// Refetch delivered the root → chain released → BOTH events now persisted.
	ids := storeIDs(t, st)
	if len(ids) != 2 {
		t.Fatalf("store has %d records, want 2 (root + released orphan)", len(ids))
	}
	if seq.PendingCount() != 0 {
		t.Fatalf("PendingCount = %d, want 0 (chain drained)", seq.PendingCount())
	}
	if wm.OrphanUnrecoverable.Load() != 0 {
		t.Fatalf("OrphanUnrecoverable = %d, want 0", wm.OrphanUnrecoverable.Load())
	}
	if containsID(*alarms, "orphan_unrecoverable") {
		t.Fatalf("unexpected quarantine alarm on a recoverable chain: %v", *alarms)
	}
}

// --- TEST 2b: targeted-refetch rate limit (token bucket) -------------------

// TestWatchdog_RefetchRateLimited is DONE criterion (4): the targeted orphan
// refetch is RATE-LIMITED by a token bucket (§2.5a, ADV-6 — each distinct
// antecedent costs one relay REQ). Under a burst of many distinct missing
// antecedents a single CheckOrphans pass issues at most `burst` refetch REQs and
// DEFERS the rest (orphan_refetch_throttled), capping relay-REQ amplification. A
// later pass, after the bucket refills, issues the next batch. Driven with an
// injected clock so the rate limit is asserted deterministically.
func TestWatchdog_RefetchRateLimited(t *testing.T) {
	const burst = 3
	const orphans = 10
	clock := time.Unix(1_000_000, 0)
	wd, relay, _, seq, wm, alarms, _ := newWatchdogHarness(t,
		WithRefetchRate(burst, 1.0), // 3-token burst, refill 1/sec
		WithClock(func() time.Time { return clock }),
	)
	seller, err := identity.Generate()
	if err != nil {
		t.Fatalf("generate seller: %v", err)
	}

	// Ingest `orphans` events, each e-tagging a DISTINCT antecedent the relay
	// never serves — so every one stays pending and would, unbounded, cost one
	// refetch REQ per pass.
	for i := 0; i < orphans; i++ {
		ante := fmt.Sprintf("%064x", i+1) // distinct, never-served antecedent id
		orphan := signEventAt(t, seller, nostr.KindBuy, int64(600+i), [][]string{eTag(ante)}, fmt.Sprintf("orphan-%d", i))
		if herr := relay.intake.HandleEvent(orphan); herr != nil {
			t.Fatalf("ingest orphan %d: %v", i, herr)
		}
	}
	if seq.PendingCount() != orphans {
		t.Fatalf("PendingCount = %d, want %d", seq.PendingCount(), orphans)
	}

	// Pass 1 at t0: only `burst` tokens → `burst` REQs, the rest throttled.
	refetched, err := wd.CheckOrphans(context.Background())
	if err != nil {
		t.Fatalf("CheckOrphans pass 1: %v", err)
	}
	if refetched != burst {
		t.Fatalf("pass 1 refetches = %d, want %d (token-bucket burst)", refetched, burst)
	}
	if wm.OrphanRefetch.Load() != burst {
		t.Fatalf("OrphanRefetch = %d, want %d", wm.OrphanRefetch.Load(), burst)
	}
	if wm.OrphanRefetchThrottled.Load() != orphans-burst {
		t.Fatalf("OrphanRefetchThrottled = %d, want %d (deferred this pass)", wm.OrphanRefetchThrottled.Load(), orphans-burst)
	}
	if !containsID(*alarms, "orphan_refetch_throttled") {
		t.Fatalf("alarms = %v, want an orphan_refetch_throttled", *alarms)
	}

	// A second pass at the SAME instant: bucket empty (no refill) → zero REQs,
	// every pending antecedent throttled.
	refetched, err = wd.CheckOrphans(context.Background())
	if err != nil {
		t.Fatalf("CheckOrphans pass 2 (no refill): %v", err)
	}
	if refetched != 0 {
		t.Fatalf("pass 2 refetches = %d, want 0 (bucket empty, no time elapsed)", refetched)
	}
	if wm.OrphanRefetch.Load() != burst {
		t.Fatalf("OrphanRefetch grew without refill: %d, want %d", wm.OrphanRefetch.Load(), burst)
	}

	// Advance the clock enough to refill the bucket to its cap, then a third pass
	// issues another `burst` REQs — the rate limit RECOVERS over time, it does not
	// permanently give up (no quarantine).
	clock = clock.Add(10 * time.Second) // refill 10 tokens, capped at burst
	refetched, err = wd.CheckOrphans(context.Background())
	if err != nil {
		t.Fatalf("CheckOrphans pass 3 (after refill): %v", err)
	}
	if refetched != burst {
		t.Fatalf("pass 3 refetches = %d, want %d (bucket refilled to cap)", refetched, burst)
	}
	if wm.OrphanRefetch.Load() != 2*burst {
		t.Fatalf("OrphanRefetch = %d, want %d (two bursts issued)", wm.OrphanRefetch.Load(), 2*burst)
	}
}

// --- TEST 2c: refetch fairness under a low-id flood ------------------------

// TestWatchdog_RefetchFairnessImpactFirstNotStarvedByLowIDFlood is the wave-9
// LOW regression. The previous CheckOrphans spent the finite refetch budget in
// ascending antecedent-id order (sort.Strings), so an attacker publishing many
// orphans with low-sorting fabricated antecedent ids ("0000…") pinned the whole
// budget every pass — permanently STARVING a legit antecedent's recovery and
// storming the alarm sink with one alarm per deferred antecedent.
//
// The fix spends the budget by IMPACT (dependent count) first. This test floods
// many distinct low-sorting fabricated antecedents (one dependent each) alongside
// ONE legit antecedent with more dependents, gives a scarce budget, and asserts:
// the legit antecedent IS refetched (and recovers) within the pass despite the
// flood, and the many deferred flood antecedents raise exactly ONE coalesced
// throttle alarm (not one per antecedent).
func TestWatchdog_RefetchFairnessImpactFirstNotStarvedByLowIDFlood(t *testing.T) {
	const burst = 4
	clock := time.Unix(2_000_000, 0)
	wd, relay, _, seq, wm, alarms, _ := newWatchdogHarness(t,
		WithRefetchRate(burst, 0.0), // burst tokens, NO refill: exactly `burst` REQs this pass
		WithClock(func() time.Time { return clock }),
	)
	seller, err := identity.Generate()
	if err != nil {
		t.Fatalf("generate seller: %v", err)
	}

	// Flood: many orphans, each e-tagging a DISTINCT, LOW-SORTING fabricated
	// antecedent the relay never serves, each with exactly ONE dependent. Under the
	// old ascending-id order these "0000…" ids monopolize the budget every pass.
	const floodN = 40
	for i := 0; i < floodN; i++ {
		ante := fmt.Sprintf("0000%060x", i)
		orphan := signEventAt(t, seller, nostr.KindBuy, int64(500+i), [][]string{eTag(ante)}, fmt.Sprintf("flood-%d", i))
		if herr := relay.intake.HandleEvent(orphan); herr != nil {
			t.Fatalf("ingest flood orphan %d: %v", i, herr)
		}
	}

	// One LEGIT antecedent the relay DOES serve, with MORE dependents (higher
	// impact) than any single flood antecedent. Its id is a real event hash — it
	// sorts ABOVE the "0000…" flood, so id-order would have starved it.
	legitRoot := signEventAt(t, seller, nostr.KindPut, 900, nil, "legit-root")
	relay.held = []*nostr.Event{legitRoot}
	const legitDeps = 5
	for k := 0; k < legitDeps; k++ {
		dep := signEventAt(t, seller, nostr.KindBuy, int64(901+k), [][]string{eTag(legitRoot.ID)}, fmt.Sprintf("legit-dep-%d", k))
		if herr := relay.intake.HandleEvent(dep); herr != nil {
			t.Fatalf("ingest legit dep %d: %v", k, herr)
		}
	}

	if got, want := seq.PendingCount(), floodN+legitDeps; got != want {
		t.Fatalf("PendingCount = %d, want %d", got, want)
	}
	// Sanity: distinct pending antecedents = floodN flood + 1 legit.
	distinctAntes := len(seq.PendingAntecedents())
	if distinctAntes != floodN+1 {
		t.Fatalf("distinct pending antecedents = %d, want %d", distinctAntes, floodN+1)
	}

	// One recovery pass with a scarce budget (burst << floodN). Impact-first
	// ordering must spend a token on the legit antecedent despite the low-id flood.
	refetched, err := wd.CheckOrphans(context.Background())
	if err != nil {
		t.Fatalf("CheckOrphans: %v", err)
	}
	if refetched != burst {
		t.Fatalf("refetched = %d, want %d (scarce budget fully spent this pass)", refetched, burst)
	}

	// The legit antecedent WAS refetched: a targeted REQ ids=[legitRoot] issued.
	sawLegitREQ := false
	for _, q := range relay.queries {
		if len(q.IDs) == 1 && q.IDs[0] == legitRoot.ID {
			sawLegitREQ = true
			break
		}
	}
	if !sawLegitREQ {
		t.Fatalf("legit antecedent %s was NOT refetched — starved by the low-id flood (fairness regression)", shortID(legitRoot.ID))
	}
	// Recovery actually happened: the relay served the root, the Intake released the
	// 5 dependent chain, so only the flood orphans remain.
	if _, stillPending := seq.PendingAntecedents()[legitRoot.ID]; stillPending {
		t.Fatalf("legit antecedent still pending after refetch; chain not released")
	}
	if got := seq.PendingCount(); got != floodN {
		t.Fatalf("PendingCount after pass = %d, want %d (only the flood remains, legit chain drained)", got, floodN)
	}

	// Alarm-storm bound: the deferred flood antecedents raise exactly ONE coalesced
	// orphan_refetch_throttled alarm, not one per antecedent.
	throttledAlarms := 0
	for _, a := range *alarms {
		if a == "orphan_refetch_throttled" {
			throttledAlarms++
		}
	}
	if throttledAlarms != 1 {
		t.Fatalf("orphan_refetch_throttled alarms = %d, want exactly 1 (coalesced, not one per antecedent)", throttledAlarms)
	}
	// The per-antecedent METRIC still counts every distinct deferred antecedent.
	if got, want := wm.OrphanRefetchThrottled.Load(), int64(distinctAntes-burst); got != want {
		t.Fatalf("OrphanRefetchThrottled metric = %d, want %d (per-antecedent, distinct-burst)", got, want)
	}
}

// --- TEST 3: resync audit id-set diff --------------------------------------

// TestWatchdog_ResyncAuditIDSetDiff exercises §2.5 path 3. It sets up BOTH diff
// directions in one audit:
//
//   - a local-only OPERATOR event the relay lacks  → handed to Outbox catch-up
//   - a relay event the local store cannot reconcile (an orphan) → resync_mismatch
//
// and a reconcilable relay event that the since=0 audit absorbs (NOT a mismatch).
func TestWatchdog_ResyncAuditIDSetDiff(t *testing.T) {
	op, err := identity.Generate()
	if err != nil {
		t.Fatalf("generate operator: %v", err)
	}
	seller, err := identity.Generate()
	if err != nil {
		t.Fatalf("generate seller: %v", err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "resync.log"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	seq := exchange.NewSequencer(0)
	intake := NewIntake(seq, st, op.PubKeyHex(), &IntakeMetrics{}, nil)
	relay := &fakeRelay{intake: intake, pruned: map[string]struct{}{}}

	// (1) A local-only OPERATOR event the relay does NOT hold → must be
	// republished. Written directly to the store with Origin="local" (operator
	// authored; the relay lacks it, e.g. a crash between fold and publish).
	localOnly := signEventAt(t, op, nostr.KindMatch, 800, nil, "operator-match")
	if aerr := st.Append(store.Record{
		ID: localOnly.ID, Sender: op.PubKeyHex(), Timestamp: 800 * 1_000_000_000,
		Origin: "local", Payload: []byte("operator-match"),
	}); aerr != nil {
		t.Fatalf("seed local-only record: %v", aerr)
	}

	// (2) A reconcilable relay event (a root put) the local store lacks — the
	// since=0 audit will feed it through the Intake and it WILL persist, so it is
	// NOT a mismatch.
	reconcilable := signEventAt(t, seller, nostr.KindPut, 810, nil, "reconcilable-put")

	// (3) A relay orphan (buy e-tagging a never-served root) the audit CANNOT
	// reconcile → resync_mismatch.
	// The antecedent is simply never added to relay.held, so the since=0 audit
	// cannot fetch it — a true unrecoverable gap. (No IDs refetch happens in the
	// resync path; the orphan just stays orphaned and unpersisted.)
	prunedRoot := signEventAt(t, seller, nostr.KindPut, 815, nil, "never-served-root").ID
	orphanOnRelay := signEventAt(t, seller, nostr.KindBuy, 820, [][]string{eTag(prunedRoot)}, "relay-orphan")

	// The relay serves the reconcilable put and the orphan buy (its antecedent is
	// absent from the relay entirely — a true unrecoverable gap).
	relay.held = []*nostr.Event{reconcilable, orphanOnRelay}

	var alarms []string
	var republished [][]store.Record
	wm := &WatchdogMetrics{}
	wd := NewWatchdog(relay, seq, st,
		republisherFunc(func(_ context.Context, recs []store.Record) error {
			republished = append(republished, recs)
			return nil
		}),
		wm,
		func(class string, _ error, _ *nostr.Event) { alarms = append(alarms, class) },
	)

	mismatches, err := wd.ResyncAudit(context.Background())
	if err != nil {
		t.Fatalf("ResyncAudit: %v", err)
	}

	// The audit issued a since=0 REQ.
	if len(relay.queries) == 0 || relay.queries[0].Since == nil || *relay.queries[0].Since != 0 {
		t.Fatalf("resync REQ since = %v, want 0", func() interface{} {
			if len(relay.queries) == 0 {
				return "none"
			}
			return relay.queries[0].Since
		}())
	}

	// (2) reconcilable put absorbed → now local.
	if ids := storeIDs(t, st); !containsID(ids, reconcilable.ID) {
		t.Fatalf("reconcilable relay put not absorbed by audit; store=%v", ids)
	}

	// (1) local-only operator event handed to the Outbox catch-up exactly once.
	if len(republished) != 1 || len(republished[0]) != 1 || republished[0][0].ID != localOnly.ID {
		t.Fatalf("republished = %v, want exactly [[%s]]", republished, localOnly.ID)
	}
	if wm.ResyncRepublished.Load() != 1 {
		t.Fatalf("ResyncRepublished = %d, want 1", wm.ResyncRepublished.Load())
	}

	// (3) the relay orphan the audit could not reconcile → exactly one mismatch.
	if mismatches != 1 {
		t.Fatalf("mismatches = %d, want 1 (the unreconcilable relay orphan)", mismatches)
	}
	if wm.ResyncMismatch.Load() != 1 {
		t.Fatalf("ResyncMismatch = %d, want 1", wm.ResyncMismatch.Load())
	}
	if !containsID(alarms, "resync_mismatch") {
		t.Fatalf("alarms = %v, want a resync_mismatch", alarms)
	}
}

// --- small test seams ------------------------------------------------------

// republisherFunc adapts a func to the Republisher interface.
type republisherFunc func(ctx context.Context, recs []store.Record) error

func (f republisherFunc) Republish(ctx context.Context, recs []store.Record) error {
	return f(ctx, recs)
}
