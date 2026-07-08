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
	"testing"
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

func TestSequencer_OverflowFailsLoud(t *testing.T) {
	s := NewSequencer(2) // bound = 2 orphans
	// Three orphans, each waiting on a distinct never-arriving antecedent.
	for _, id := range []string{"o1", "o2", "o3"} {
		if err := s.Ingest(seqMsg(id, 10, "missing-"+id)); err != nil {
			t.Fatalf("Ingest %s: %v", id, err)
		}
	}
	_, err := s.Drain()
	if err == nil {
		t.Fatal("Drain returned nil with 3 orphans over a bound of 2; must fail loud")
	}
	if !errors.Is(err, ErrOrphanBufferOverflow) {
		t.Fatalf("Drain error = %v, want ErrOrphanBufferOverflow", err)
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
