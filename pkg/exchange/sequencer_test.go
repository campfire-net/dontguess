package exchange

// Unit tests for the operator-side Sequencer (dontguess-50d, M2). These are
// white-box: they exercise the sequencer component in isolation with synthetic
// events (the sequencer ignores tags/payload — only ID, Timestamp, and
// Antecedents drive it), asserting the four contract properties directly:
// dedup of duplicate delivery, orphan buffering until an antecedent lands,
// canonical (Timestamp,ID) order independent of arrival, and LOUD failure on a
// pruned antecedent or an overflowed buffer. The end-to-end byte-identical-fold
// property is proved separately in sequencer_property_test.go through the real
// exchange fold.

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

// seqMsg builds a synthetic event for the sequencer. Only ID, Timestamp, and
// Antecedents matter to the sequencer; tags/payload are irrelevant.
func seqMsg(id string, ts int64, ante ...string) Message {
	return Message{ID: id, Timestamp: ts, Antecedents: ante}
}

func drainIDs(t *testing.T, s *Sequencer) []string {
	t.Helper()
	released, err := s.Drain()
	if err != nil {
		t.Fatalf("Drain: unexpected error: %v", err)
	}
	ids := make([]string, len(released))
	for i, r := range released {
		ids[i] = r.Msg.ID
		if r.Seq != int64(i) {
			// Drain returns ascending Seq starting at nextSeq; within one drain
			// over a fresh sequencer the i-th release must carry Seq==i.
			t.Fatalf("release %d has Seq=%d, want monotonic %d", i, r.Seq, i)
		}
	}
	return ids
}

func TestSequencer_DedupDuplicateDelivery(t *testing.T) {
	s := NewSequencer(0)
	m := seqMsg("a", 10)
	if err := s.Ingest(m); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	// Same event id arriving again from another relay — must be a no-op.
	if err := s.Ingest(m); err != nil {
		t.Fatalf("Ingest dup: %v", err)
	}
	if got := s.PendingCount(); got != 1 {
		t.Fatalf("PendingCount after dup ingest = %d, want 1", got)
	}
	ids := drainIDs(t, s)
	if len(ids) != 1 || ids[0] != "a" {
		t.Fatalf("Drain released %v, want [a] exactly once", ids)
	}
	// A duplicate arriving AFTER release must still be a no-op (dedup via the
	// emitted set), not a re-release.
	if err := s.Ingest(m); err != nil {
		t.Fatalf("Ingest post-release dup: %v", err)
	}
	ids2 := drainIDs(t, s)
	if len(ids2) != 0 {
		t.Fatalf("Drain after post-release dup released %v, want nothing", ids2)
	}
}

func TestSequencer_OrphanBufferedUntilAntecedentLands(t *testing.T) {
	s := NewSequencer(0)
	// Child arrives BEFORE its antecedent (the exact nostr multi-relay reorder).
	if err := s.Ingest(seqMsg("child", 20, "parent")); err != nil {
		t.Fatalf("Ingest child: %v", err)
	}
	if ids := drainIDs(t, s); len(ids) != 0 {
		t.Fatalf("Drain released %v before antecedent landed, want nothing", ids)
	}
	if got := s.PendingCount(); got != 1 {
		t.Fatalf("PendingCount = %d, want 1 orphan held", got)
	}
	// Antecedent lands.
	if err := s.Ingest(seqMsg("parent", 10)); err != nil {
		t.Fatalf("Ingest parent: %v", err)
	}
	ids := drainIDs(t, s)
	if len(ids) != 2 || ids[0] != "parent" || ids[1] != "child" {
		t.Fatalf("Drain released %v, want [parent child] in causal order", ids)
	}
	if got := s.PendingCount(); got != 0 {
		t.Fatalf("PendingCount after release = %d, want 0", got)
	}
}

func TestSequencer_CanonicalOrderIndependentOfArrival(t *testing.T) {
	// DAG: A(10) root; C(15) root; B(20, ante A); D(30, ante B,C).
	// Canonical (Timestamp,ID) Kahn order: A, C, B, D — regardless of how the
	// events arrive.
	base := []Message{
		seqMsg("A", 10),
		seqMsg("B", 20, "A"),
		seqMsg("C", 15),
		seqMsg("D", 30, "B", "C"),
	}
	want := []string{"A", "C", "B", "D"}

	// Every permutation of arrival order must yield the identical release order.
	perms := permuteIndices(len(base))
	for _, p := range perms {
		s := NewSequencer(0)
		for _, idx := range p {
			if err := s.Ingest(base[idx]); err != nil {
				t.Fatalf("Ingest: %v", err)
			}
		}
		got := drainIDs(t, s)
		if len(got) != len(want) {
			t.Fatalf("arrival %v: released %v, want %v", p, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("arrival %v: released %v, want %v", p, got, want)
			}
		}
	}
}

func TestSequencer_TimestampTieBrokenByID(t *testing.T) {
	// Two independent roots with the SAME timestamp: the ID tiebreak makes the
	// order deterministic ("m" < "z").
	s := NewSequencer(0)
	if err := s.Ingest(seqMsg("z", 5)); err != nil {
		t.Fatalf("Ingest z: %v", err)
	}
	if err := s.Ingest(seqMsg("m", 5)); err != nil {
		t.Fatalf("Ingest m: %v", err)
	}
	ids := drainIDs(t, s)
	if len(ids) != 2 || ids[0] != "m" || ids[1] != "z" {
		t.Fatalf("Drain released %v, want [m z] (ID tiebreak on equal timestamp)", ids)
	}
}

func TestSequencer_MarkEmittedSatisfiesAntecedent(t *testing.T) {
	// Recovery: the parent was folded in a prior checkpoint. Seeding it via
	// MarkEmitted means a child referencing it is NOT treated as an orphan.
	s := NewSequencer(0)
	s.MarkEmitted("parent")
	if err := s.Ingest(seqMsg("child", 20, "parent")); err != nil {
		t.Fatalf("Ingest child: %v", err)
	}
	ids := drainIDs(t, s)
	if len(ids) != 1 || ids[0] != "child" {
		t.Fatalf("Drain released %v, want [child] (parent pre-seeded)", ids)
	}
	if err := s.Seal(); err != nil {
		t.Fatalf("Seal after full release: %v", err)
	}
}

func TestSequencer_SealFailsLoudOnPrunedAntecedent(t *testing.T) {
	s := NewSequencer(0)
	// Child references an antecedent that never arrives (relay-pruned).
	if err := s.Ingest(seqMsg("child", 20, "gone")); err != nil {
		t.Fatalf("Ingest child: %v", err)
	}
	released, err := s.Drain()
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(released) != 0 {
		t.Fatalf("Drain released %v; a child with a missing antecedent must NOT be released", idsOf(released))
	}
	// Seal must FAIL LOUD — never silently drop the orphan (dontguess-553).
	err = s.Seal()
	if err == nil {
		t.Fatal("Seal returned nil with an unrecoverable antecedent; must fail loud")
	}
	if !errors.Is(err, ErrUnrecoverableAntecedent) {
		t.Fatalf("Seal error = %v, want ErrUnrecoverableAntecedent", err)
	}
	// The orphan is preserved (not silently discarded) for inspection/retry.
	if got := s.PendingCount(); got != 1 {
		t.Fatalf("PendingCount after failed Seal = %d, want 1 (orphan preserved)", got)
	}
}

func TestSequencer_OverflowFailsLoudAtIngest(t *testing.T) {
	// dontguess-afb: the orphan bound is enforced AT INGEST (O(1) per call),
	// not deferred to Drain — a reorder/gap flood must not be able to buffer
	// past the bound before anyone calls Drain. Each of these three events
	// waits on a DISTINCT antecedent that never arrives, so all three are true
	// orphans (the antecedent is never known — never emitted, never buffered).
	s := NewSequencer(2) // bound = 2 orphans
	for _, id := range []string{"o1", "o2"} {
		if err := s.Ingest(seqMsg(id, 10, "missing-"+id)); err != nil {
			t.Fatalf("Ingest %s: %v", id, err)
		}
	}
	// The third true orphan pushes the count to 3 over a bound of 2: Ingest
	// itself must fail loud, right here, before it is buffered.
	err := s.Ingest(seqMsg("o3", 10, "missing-o3"))
	if err == nil {
		t.Fatal("Ingest of the 3rd true orphan over a bound of 2 returned nil; must fail loud")
	}
	if !errors.Is(err, ErrOrphanBufferOverflow) {
		t.Fatalf("Ingest error = %v, want ErrOrphanBufferOverflow", err)
	}
	// The rejected event must not have been buffered (bounded memory: the
	// point of checking at ingest is that the buffer never grows past bound).
	if got := s.PendingCount(); got != 2 {
		t.Fatalf("PendingCount after rejected overflow ingest = %d, want 2 (o3 not buffered)", got)
	}
	// Drain over the two orphans that WERE admitted must succeed cleanly (no
	// residual overflow — they are within bound).
	released, err := s.Drain()
	if err != nil {
		t.Fatalf("Drain over in-bound orphans: unexpected error: %v", err)
	}
	if len(released) != 0 {
		t.Fatalf("Drain released %v; both remaining events are still orphaned, want nothing released", idsOf(released))
	}
}

// TestSequencer_TrueOrphanCountIsNotRawBufferSize proves the O(1) ingest-time
// counter tracks TRUE orphans (an antecedent that has never been ingested at
// all), not raw buffer occupancy.
func TestSequencer_TrueOrphanCountIsNotRawBufferSize(t *testing.T) {
	// A causally in-order chain where each event's antecedent is already
	// BUFFERED (not yet emitted) by the time the child is ingested must NOT
	// count any of the chain as a true orphan, even though every one of them
	// sits in the raw buffer simultaneously. This is the distinction that
	// keeps a legitimate large in-order batch from tripping the bound (see
	// TestSequencer_LargeCausallyInOrderBatchDoesNotTripBound).
	s := NewSequencer(2) // bound = 2 true orphans; chain below is longer than that
	root := seqMsg("root", 0)
	if err := s.Ingest(root); err != nil {
		t.Fatalf("Ingest root: %v", err)
	}
	prev := "root"
	for i := 0; i < 10; i++ {
		id := fmt.Sprintf("c%d", i)
		if err := s.Ingest(seqMsg(id, int64(i+1), prev)); err != nil {
			t.Fatalf("Ingest %s: %v", id, err)
		}
		prev = id
	}
	if got := s.PendingCount(); got != 11 {
		t.Fatalf("PendingCount = %d, want 11 (root + 10 chained, none of them true orphans)", got)
	}
	released, err := s.Drain()
	if err != nil {
		t.Fatalf("Drain: unexpected error: %v", err)
	}
	if len(released) != 11 {
		t.Fatalf("Drain released %d events, want all 11 (fully causally closed chain)", len(released))
	}
}

func TestSequencer_EmptyIDRejected(t *testing.T) {
	s := NewSequencer(0)
	if err := s.Ingest(seqMsg("", 10)); err == nil {
		t.Fatal("Ingest of empty-ID event returned nil; must be rejected")
	}
}

func TestSequenceForFold_PrunedAntecedentFailsLoud(t *testing.T) {
	// Broken causal closure: "resp" references "req", but "req" is absent.
	msgs := []Message{
		seqMsg("root", 10),
		seqMsg("resp", 20, "req"), // antecedent "req" pruned
	}
	out, err := SequenceForFold(msgs, 0)
	if err == nil {
		t.Fatal("SequenceForFold returned nil error on broken closure; must fail loud")
	}
	if !errors.Is(err, ErrUnrecoverableAntecedent) {
		t.Fatalf("error = %v, want ErrUnrecoverableAntecedent", err)
	}
	if out != nil {
		t.Fatalf("SequenceForFold returned a partial ordering %v on failure; must return nil (no silent truncated fold)", idsOfMsgs(out))
	}
}

// TestSequenceForFold_LargeCausallyInOrderBatchDoesNotTripBound is the
// dontguess-afb regression: the recovery/replay path (SequenceForFold) feeds
// an entire batch through Ingest before a single Drain call. A long,
// perfectly-resolvable causal chain fed in ORDER (each antecedent ingested
// before its child) must never trip the true-orphan bound, however long the
// chain — because by the time a child is ingested its antecedent is already
// KNOWN (buffered), not a true gap. Bound is deliberately set small (10) and
// the chain is 50x longer, to prove the bound tracks true orphans, not raw
// batch size.
func TestSequenceForFold_LargeCausallyInOrderBatchDoesNotTripBound(t *testing.T) {
	const n = 500
	const bound = 10
	msgs := make([]Message, 0, n)
	msgs = append(msgs, seqMsg("root", 0))
	prev := "root"
	for i := 0; i < n-1; i++ {
		id := fmt.Sprintf("e%d", i)
		msgs = append(msgs, seqMsg(id, int64(i+1), prev))
		prev = id
	}
	out, err := SequenceForFold(msgs, bound)
	if err != nil {
		t.Fatalf("SequenceForFold over a %d-event in-order chain with bound %d: unexpected error: %v", n, bound, err)
	}
	if len(out) != n {
		t.Fatalf("SequenceForFold released %d events, want all %d", len(out), n)
	}
	if out[0].ID != "root" || out[len(out)-1].ID != prev {
		t.Fatalf("SequenceForFold order = [%s..%s], want [root..%s] (causal order preserved)", out[0].ID, out[len(out)-1].ID, prev)
	}
}

// TestSequenceForFold_TrueOrphanFloodTripsBoundAtIngest proves the actual
// DoS-shaped attack — many events each referencing a DISTINCT antecedent that
// never arrives — is rejected loud, and rejected EARLY (at ingest, not after
// the whole flood has already been buffered): SequenceForFold must return
// ErrOrphanBufferOverflow without ever reaching Drain/Seal for the excess.
func TestSequenceForFold_TrueOrphanFloodTripsBoundAtIngest(t *testing.T) {
	const bound = 50
	msgs := make([]Message, 0, bound+1)
	for i := 0; i < bound+1; i++ {
		id := fmt.Sprintf("o%d", i)
		msgs = append(msgs, seqMsg(id, int64(i), "missing-"+id))
	}
	out, err := SequenceForFold(msgs, bound)
	if err == nil {
		t.Fatalf("SequenceForFold over %d true orphans with bound %d returned nil error; must fail loud", len(msgs), bound)
	}
	if !errors.Is(err, ErrOrphanBufferOverflow) {
		t.Fatalf("error = %v, want ErrOrphanBufferOverflow", err)
	}
	if out != nil {
		t.Fatalf("SequenceForFold returned a partial ordering %v on overflow; must return nil", idsOfMsgs(out))
	}
}

// TestSequencer_DrainNoQuadraticRescan is a regression guard for the O(N^2)
// DoS: the prior Drain implementation rescanned the ENTIRE remaining buffer to
// find the single next-ready event, once per release (O(N) work x N releases
// = O(N^2)). Feed a large batch in REVERSE causal order (each child ingested
// before its antecedent — the worst case for a rescan-based algorithm, since
// nothing is ready until the very last ingest) and time a single Drain call.
// The new heap-based cascade is O(N log N): for N=20000 that is comfortably
// sub-second; the old O(N^2) algorithm is many seconds to minutes at this N.
// The bound is set to N so no orphan-bound error is in play — this test is
// purely about algorithmic complexity, not correctness (covered elsewhere).
func TestSequencer_DrainNoQuadraticRescan(t *testing.T) {
	const n = 20000
	s := NewSequencer(n + 1)
	// Build the chain root -> e0 -> e1 -> ... -> e(n-2), then ingest in
	// REVERSE order: e(n-2) first (references e(n-3), unknown), ... , root
	// last. Nothing is causally ready until the final Ingest lands "root".
	ids := make([]string, n)
	ids[0] = "root"
	for i := 1; i < n; i++ {
		ids[i] = fmt.Sprintf("e%d", i)
	}
	for i := n - 1; i >= 0; i-- {
		var ante string
		if i > 0 {
			ante = ids[i-1]
		}
		var m Message
		if ante == "" {
			m = seqMsg(ids[i], int64(i))
		} else {
			m = seqMsg(ids[i], int64(i), ante)
		}
		if err := s.Ingest(m); err != nil {
			t.Fatalf("Ingest %s: %v", ids[i], err)
		}
	}
	start := time.Now()
	released, err := s.Drain()
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Drain: unexpected error: %v", err)
	}
	if len(released) != n {
		t.Fatalf("Drain released %d events, want all %d", len(released), n)
	}
	for i, r := range released {
		if r.Msg.ID != ids[i] {
			t.Fatalf("release %d = %s, want %s (causal order)", i, r.Msg.ID, ids[i])
		}
	}
	// Generous bound: O(N log N) for N=20000 is sub-second on any reasonable
	// hardware; the prior O(N^2) algorithm would take many seconds to
	// minutes. This is a coarse regression guard against reintroducing a
	// full-buffer rescan per release, not a tight performance SLA.
	const budget = 5 * time.Second
	if elapsed > budget {
		t.Fatalf("Drain of %d causally-chained events took %v, want < %v (likely reintroduced an O(N^2) rescan)", n, elapsed, budget)
	}
}

// TestSequencer_IngestLiveChainedFloodBoundsOccupancyEvictsOldest is the core
// §2.5a regression: the CHAINED flood e0<-e1<-..<-eN where ONLY the head
// references a never-arriving antecedent. Under the head-only true-orphan bound
// (Ingest) this bypasses the guard — every later link's antecedent is already
// BUFFERED when it arrives, so trueOrphans stays 1 while the buffer grows without
// bound. The LIVE path (IngestLive) bounds raw OCCUPANCY and evicts the OLDEST
// buffered orphan, so occupancy never exceeds the bound and the buffer becomes a
// sliding window of the most-recent events.
func TestSequencer_IngestLiveChainedFloodBoundsOccupancyEvictsOldest(t *testing.T) {
	const bound = 8
	const flood = 50
	s := NewSequencer(bound)

	// e0 is the never-ingested head. Each e_i (i>=1) references e_{i-1}, which is
	// already buffered by the time e_i arrives — the true-orphan-count bypass.
	prev := "e0"
	for i := 1; i <= flood; i++ {
		id := fmt.Sprintf("e%d", i)
		evicted, err := s.IngestLive(seqMsg(id, int64(i), prev))
		if err != nil {
			t.Fatalf("IngestLive %s: unexpected error (must NEVER reject a well-formed event): %v", id, err)
		}
		if got := s.PendingCount(); got > bound {
			t.Fatalf("after ingesting %s occupancy = %d, exceeds bound %d (buffer grew unbounded — the chained-flood bypass)", id, got, bound)
		}
		// Once the buffer is full, every further admit evicts exactly one oldest.
		if i > bound && len(evicted) != 1 {
			t.Fatalf("ingest %s evicted %v, want exactly one oldest orphan evicted", id, evicted)
		}
		if i <= bound && len(evicted) != 0 {
			t.Fatalf("ingest %s evicted %v while buffer not yet full, want none", id, evicted)
		}
		prev = id
	}
	if got := s.PendingCount(); got != bound {
		t.Fatalf("final occupancy = %d, want exactly the bound %d", got, bound)
	}
	// The buffer is the sliding window of the LAST `bound` events: the oldest
	// survivor is e(flood-bound+1), the newest is e(flood). The earliest links
	// were evicted (oldest-first).
	pend := s.PendingAntecedents()
	// The head e0 must NOT be buffered (it was never ingested); the very first
	// links must have been evicted.
	drained, err := s.Drain()
	if err != nil {
		t.Fatalf("Drain after flood: %v", err)
	}
	if len(drained) != 0 {
		t.Fatalf("Drain released %v; nothing is causally ready (head e0 never arrived)", idsOf(drained))
	}
	if got := s.PendingCount(); got != bound {
		t.Fatalf("occupancy after Drain = %d, want %d (nothing releasable)", got, bound)
	}
	// A brand-new, well-formed INDEPENDENT root event is STILL admitted after the
	// flood (NO wedge — the attempt-3 ADV-5 fill-then-reject failure): it evicts
	// one oldest orphan and then drains cleanly.
	evicted, err := s.IngestLive(seqMsg("fresh-root", 100000))
	if err != nil {
		t.Fatalf("IngestLive fresh-root after flood must succeed (no wedge): %v", err)
	}
	if len(evicted) != 1 {
		t.Fatalf("fresh-root admit evicted %v, want exactly one oldest orphan", evicted)
	}
	rel, err := s.Drain()
	if err != nil {
		t.Fatalf("Drain after fresh-root: %v", err)
	}
	if len(rel) != 1 || rel[0].Msg.ID != "fresh-root" {
		t.Fatalf("Drain released %v, want [fresh-root] (the independent root is releasable)", idsOf(rel))
	}
	_ = pend
}

// TestSequencer_IngestLivePostFloodDelayedEventStillIngests is DONE criterion
// (2): after the buffer has been saturated by a chained-orphan flood, a
// legitimate causally-delayed event (child arriving before its parent) must
// STILL ingest and, once the parent lands, release — the buffer never wedges
// against new well-formed traffic.
func TestSequencer_IngestLivePostFloodDelayedEventStillIngests(t *testing.T) {
	const bound = 4
	s := NewSequencer(bound)

	// Saturate the buffer with a chained-orphan flood (head never arrives).
	prev := "missing-head"
	for i := 1; i <= 20; i++ {
		id := fmt.Sprintf("f%d", i)
		if _, err := s.IngestLive(seqMsg(id, int64(i), prev)); err != nil {
			t.Fatalf("flood IngestLive %s: %v", id, err)
		}
		prev = id
	}
	if s.PendingCount() != bound {
		t.Fatalf("post-flood occupancy = %d, want bound %d", s.PendingCount(), bound)
	}

	// A legitimate causally-delayed pair: child arrives before parent (the exact
	// honest nostr reorder). Both must ingest despite the saturated buffer.
	if _, err := s.IngestLive(seqMsg("child", 1000, "parent")); err != nil {
		t.Fatalf("IngestLive delayed child post-flood must succeed (no wedge): %v", err)
	}
	if _, err := s.IngestLive(seqMsg("parent", 1001)); err != nil {
		t.Fatalf("IngestLive parent post-flood must succeed: %v", err)
	}
	released, err := s.Drain()
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	// parent and child must both be released, parent before child (causal order).
	pi, ci := -1, -1
	for i, r := range released {
		switch r.Msg.ID {
		case "parent":
			pi = i
		case "child":
			ci = i
		}
	}
	if pi < 0 || ci < 0 {
		t.Fatalf("Drain released %v, want both parent and child released (delayed pair not wedged out)", idsOf(released))
	}
	if pi > ci {
		t.Fatalf("Drain released child before parent (%v); causal order violated", idsOf(released))
	}
}

// TestSequencer_IngestLiveEvictionInertOnCausallyClosedSet is DONE criterion
// (5) at the sequencer level: on a causally-closed set whose reorder window fits
// within the bound, eviction NEVER fires, so the LIVE path releases the
// byte-identical sequence it would with an unbounded buffer. The released Seq
// sequence is the sole input to the fold, so a byte-identical released sequence
// is a byte-identical fold. A linear chain has a UNIQUE causal linearization, so
// the live release order is deterministic regardless of arrival order — proven
// here by feeding the chain in REVERSE and comparing to the canonical
// SequenceForFold order.
func TestSequencer_IngestLiveEvictionInertOnCausallyClosedSet(t *testing.T) {
	const n = 12
	// Linear chain root -> e1 -> ... -> e(n-1), timestamps ascending.
	chain := make([]Message, 0, n)
	chain = append(chain, seqMsg("root", 0))
	prev := "root"
	for i := 1; i < n; i++ {
		id := fmt.Sprintf("e%d", i)
		chain = append(chain, seqMsg(id, int64(i), prev))
		prev = id
	}

	// Canonical batch order (the reference the fold would consume).
	canonical, err := SequenceForFold(chain, n)
	if err != nil {
		t.Fatalf("SequenceForFold(chain): %v", err)
	}
	wantOrder := idsOfMsgs(canonical)

	// Drive the LIVE path in REVERSE arrival order (worst case: nothing is ready
	// until the root lands last, so the whole chain co-resides in the buffer).
	// bound == n so the causally-closed set fits and eviction must NOT fire.
	liveOrder, evictions := driveLive(t, chain, reversedIndices(n), n)
	if evictions != 0 {
		t.Fatalf("eviction fired %d time(s) on a causally-closed set within bound; must be inert", evictions)
	}
	if len(liveOrder) != len(wantOrder) {
		t.Fatalf("live released %d events, want %d", len(liveOrder), len(wantOrder))
	}
	for i := range wantOrder {
		if liveOrder[i] != wantOrder[i] {
			t.Fatalf("live release order diverged from canonical at %d: got %s want %s\n live=%v\n canon=%v",
				i, liveOrder[i], wantOrder[i], liveOrder, wantOrder)
		}
	}

	// Same set, hugely oversized bound: byte-identical released sequence — the
	// bound/eviction machinery does not perturb a causally-closed fold.
	liveOrderBig, evBig := driveLive(t, chain, reversedIndices(n), 10000)
	if evBig != 0 {
		t.Fatalf("eviction fired with an oversized bound; must be inert")
	}
	for i := range liveOrder {
		if liveOrderBig[i] != liveOrder[i] {
			t.Fatalf("released sequence differs between bound=n and bound=huge at %d: %s vs %s",
				i, liveOrder[i], liveOrderBig[i])
		}
	}
}

// driveLive feeds chain[arrivalOrder[k]] through the LIVE path (IngestLive then
// Drain per event, exactly as the relay Intake does), returning the concatenated
// released id sequence and the total number of orphans evicted.
func driveLive(t *testing.T, chain []Message, arrivalOrder []int, bound int) (order []string, evictions int) {
	t.Helper()
	s := NewSequencer(bound)
	for _, idx := range arrivalOrder {
		evicted, err := s.IngestLive(chain[idx])
		if err != nil {
			t.Fatalf("IngestLive %s: %v", chain[idx].ID, err)
		}
		evictions += len(evicted)
		released, err := s.Drain()
		if err != nil {
			t.Fatalf("Drain: %v", err)
		}
		for _, r := range released {
			order = append(order, r.Msg.ID)
		}
	}
	return order, evictions
}

// reversedIndices returns [n-1, n-2, ..., 0].
func reversedIndices(n int) []int {
	out := make([]int, n)
	for i := 0; i < n; i++ {
		out[i] = n - 1 - i
	}
	return out
}

func idsOf(rel []Sequenced) []string {
	out := make([]string, len(rel))
	for i, r := range rel {
		out[i] = r.Msg.ID
	}
	return out
}

func idsOfMsgs(ms []Message) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.ID
	}
	return out
}

// permuteIndices returns all permutations of [0..n).
func permuteIndices(n int) [][]int {
	idx := make([]int, n)
	for i := range idx {
		idx[i] = i
	}
	var out [][]int
	var rec func(k int)
	rec = func(k int) {
		if k == n {
			cp := make([]int, n)
			copy(cp, idx)
			out = append(out, cp)
			return
		}
		for i := k; i < n; i++ {
			idx[k], idx[i] = idx[i], idx[k]
			rec(k + 1)
			idx[k], idx[i] = idx[i], idx[k]
		}
	}
	rec(0)
	return out
}
