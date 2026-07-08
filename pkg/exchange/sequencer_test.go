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
	if testing.Short() {
		t.Skip("skipping O(N) perf regression guard in -short mode")
	}
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
