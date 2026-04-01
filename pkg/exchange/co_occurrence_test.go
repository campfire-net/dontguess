package exchange

import (
	"encoding/json"
	"sort"
	"testing"
	"time"
)

// marshalTestPayload encodes v as JSON for test assign messages.
func marshalTestPayload(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// ptrMsg returns a pointer to a Message value.
func ptrMsg(m Message) *Message { return &m }

// TestCoOccurrenceMap_Increment verifies basic increment and count tracking.
func TestCoOccurrenceMap_Increment(t *testing.T) {
	t.Parallel()

	m := newCoOccurrenceMap()
	m.increment("b")
	m.increment("b")
	m.increment("c")

	if m.counts["b"] != 2 {
		t.Errorf("count[b] = %d, want 2", m.counts["b"])
	}
	if m.counts["c"] != 1 {
		t.Errorf("count[c] = %d, want 1", m.counts["c"])
	}
}

// TestCoOccurrenceMap_EvictsLowest verifies that the lowest-count entry is evicted
// when the map reaches CoOccurrenceK capacity.
func TestCoOccurrenceMap_EvictsLowest(t *testing.T) {
	t.Parallel()

	m := newCoOccurrenceMap()
	// Fill to capacity with distinct entries, all count=1.
	for i := 0; i < CoOccurrenceK; i++ {
		m.increment(string(rune('a' + i)))
	}
	if len(m.counts) != CoOccurrenceK {
		t.Fatalf("len = %d, want %d", len(m.counts), CoOccurrenceK)
	}

	// Bump "a" so it has a higher count and survives eviction.
	m.increment("a")
	// Insert a new entry — one of the count=1 entries must be evicted.
	m.increment("new-entry")
	if len(m.counts) > CoOccurrenceK {
		t.Errorf("len = %d after insert, want <= %d", len(m.counts), CoOccurrenceK)
	}
	// "a" (count=2) should survive.
	if m.counts["a"] != 2 {
		t.Errorf("high-count entry 'a' was evicted or count changed, got %d", m.counts["a"])
	}
}

// TestCoOccurrenceMap_TopK verifies topK returns entries in descending count order.
func TestCoOccurrenceMap_TopK(t *testing.T) {
	t.Parallel()

	m := newCoOccurrenceMap()
	m.counts["low"] = 1
	m.counts["high"] = 5
	m.counts["mid"] = 3

	top := m.topK(3)
	if len(top) != 3 {
		t.Fatalf("topK(3) len = %d, want 3", len(top))
	}
	if top[0] != "high" {
		t.Errorf("top[0] = %q, want %q", top[0], "high")
	}
	if top[1] != "mid" {
		t.Errorf("top[1] = %q, want %q", top[1], "mid")
	}
	if top[2] != "low" {
		t.Errorf("top[2] = %q, want %q", top[2], "low")
	}
}

// TestCoOccurrenceMap_TopKLimitK verifies topK respects the k argument when k < len.
func TestCoOccurrenceMap_TopKLimitK(t *testing.T) {
	t.Parallel()

	m := newCoOccurrenceMap()
	m.counts["x"] = 3
	m.counts["y"] = 2
	m.counts["z"] = 1

	top := m.topK(2)
	if len(top) != 2 {
		t.Fatalf("topK(2) len = %d, want 2", len(top))
	}
	if top[0] != "x" || top[1] != "y" {
		t.Errorf("topK(2) = %v, want [x y]", top)
	}
}

// TestState_UpdateCoOccurrence_Bidirectional verifies that UpdateCoOccurrence
// records both A→B and B→A links.
func TestState_UpdateCoOccurrence_Bidirectional(t *testing.T) {
	t.Parallel()

	s := NewState()
	s.UpdateCoOccurrence("entry-a", "entry-b")

	if cnt := s.CoOccurrenceCountForTest("entry-a", "entry-b"); cnt != 1 {
		t.Errorf("A→B count = %d, want 1", cnt)
	}
	if cnt := s.CoOccurrenceCountForTest("entry-b", "entry-a"); cnt != 1 {
		t.Errorf("B→A count = %d, want 1", cnt)
	}
}

// TestState_UpdateCoOccurrence_SelfPairIgnored verifies that self-pairs are silently ignored.
func TestState_UpdateCoOccurrence_SelfPairIgnored(t *testing.T) {
	t.Parallel()

	s := NewState()
	s.UpdateCoOccurrence("x", "x")

	if cnt := s.CoOccurrenceCountForTest("x", "x"); cnt != 0 {
		t.Errorf("self-pair count = %d, want 0", cnt)
	}
}

// TestState_UpdateCoOccurrence_EmptyIgnored verifies that empty entry IDs are ignored.
func TestState_UpdateCoOccurrence_EmptyIgnored(t *testing.T) {
	t.Parallel()

	s := NewState()
	s.UpdateCoOccurrence("", "entry-b")
	s.UpdateCoOccurrence("entry-a", "")

	if cnt := s.CoOccurrenceCountForTest("", "entry-b"); cnt != 0 {
		t.Errorf("empty A count = %d, want 0", cnt)
	}
}

// TestState_PredictNext_ReturnsTopK verifies that PredictNext returns the
// top-K co-occurring entries in descending count order.
func TestState_PredictNext_ReturnsTopK(t *testing.T) {
	t.Parallel()

	s := NewState()

	// entry-a co-occurs 3x with entry-c, 1x with entry-b.
	s.UpdateCoOccurrence("entry-a", "entry-c")
	s.UpdateCoOccurrence("entry-a", "entry-c")
	s.UpdateCoOccurrence("entry-a", "entry-c")
	s.UpdateCoOccurrence("entry-a", "entry-b")

	predicted := s.PredictNext("entry-a")
	if len(predicted) == 0 {
		t.Fatal("PredictNext returned no results")
	}
	// entry-c should be first (highest count).
	if predicted[0] != "entry-c" {
		t.Errorf("predicted[0] = %q, want %q", predicted[0], "entry-c")
	}
	// Both entry-b and entry-c must be present.
	found := make(map[string]bool)
	for _, id := range predicted {
		found[id] = true
	}
	if !found["entry-b"] {
		t.Error("entry-b not in predictions")
	}
	if !found["entry-c"] {
		t.Error("entry-c not in predictions")
	}
}

// TestState_PredictNext_NoData verifies that PredictNext returns nil for unknown entries.
func TestState_PredictNext_NoData(t *testing.T) {
	t.Parallel()

	s := NewState()
	result := s.PredictNext("unknown-entry")
	if result != nil {
		t.Errorf("PredictNext unknown = %v, want nil", result)
	}
}

// TestState_UpdateCoOccurrence_AccumulatesCount verifies repeated calls accumulate.
func TestState_UpdateCoOccurrence_AccumulatesCount(t *testing.T) {
	t.Parallel()

	s := NewState()
	for i := 0; i < 5; i++ {
		s.UpdateCoOccurrence("a", "b")
	}
	if cnt := s.CoOccurrenceCountForTest("a", "b"); cnt != 5 {
		t.Errorf("A→B count = %d, want 5", cnt)
	}
	if cnt := s.CoOccurrenceCountForTest("b", "a"); cnt != 5 {
		t.Errorf("B→A count = %d, want 5", cnt)
	}
}

// TestRecordBuyerSettlement_UpdatesCoOccurrence verifies that recordBuyerSettlement
// calls UpdateCoOccurrence for entries settled within the session window.
func TestRecordBuyerSettlement_UpdatesCoOccurrence(t *testing.T) {
	t.Parallel()

	e := NewEngine(EngineOptions{})

	// Settle two entries for the same buyer within the session window.
	e.RecordBuyerSettlementForTest("buyer-key", "entry-1")
	e.RecordBuyerSettlementForTest("buyer-key", "entry-2")

	// entry-1 and entry-2 should now co-occur.
	if cnt := e.state.CoOccurrenceCountForTest("entry-1", "entry-2"); cnt != 1 {
		t.Errorf("co-occurrence count = %d, want 1", cnt)
	}
	if cnt := e.state.CoOccurrenceCountForTest("entry-2", "entry-1"); cnt != 1 {
		t.Errorf("reverse co-occurrence count = %d, want 1", cnt)
	}
}

// TestRecordBuyerSettlement_MultiplePairs verifies that three consecutive settlements
// produce all pairwise co-occurrences.
func TestRecordBuyerSettlement_MultiplePairs(t *testing.T) {
	t.Parallel()

	e := NewEngine(EngineOptions{})
	e.RecordBuyerSettlementForTest("buyer", "e1")
	e.RecordBuyerSettlementForTest("buyer", "e2")
	e.RecordBuyerSettlementForTest("buyer", "e3")

	// e1-e2, e1-e3, e2-e3 should all be recorded.
	pairs := [][2]string{{"e1", "e2"}, {"e1", "e3"}, {"e2", "e3"}}
	for _, p := range pairs {
		if cnt := e.state.CoOccurrenceCountForTest(p[0], p[1]); cnt == 0 {
			t.Errorf("co-occurrence %s→%s = 0, want > 0", p[0], p[1])
		}
	}
}

// TestRecordBuyerSettlement_DifferentBuyersNoCoOccurrence verifies that entries
// settled by different buyers do not produce co-occurrence data.
func TestRecordBuyerSettlement_DifferentBuyersNoCoOccurrence(t *testing.T) {
	t.Parallel()

	e := NewEngine(EngineOptions{})
	e.RecordBuyerSettlementForTest("buyer-a", "entry-1")
	e.RecordBuyerSettlementForTest("buyer-b", "entry-2")

	// Different buyers: no co-occurrence should be recorded.
	if cnt := e.state.CoOccurrenceCountForTest("entry-1", "entry-2"); cnt != 0 {
		t.Errorf("cross-buyer co-occurrence = %d, want 0", cnt)
	}
}

// TestOpenPredictionAssignsForEntry verifies the open fanout count for brokered-match assigns.
func TestOpenPredictionAssignsForEntry(t *testing.T) {
	t.Parallel()

	s := NewState()
	s.OperatorKey = "op-key"

	// Post two brokered-match assigns for the same entry.
	s.Apply(ptrMsg(makeAssignMsg("assign-p1", "op-key", "entry-x", "brokered-match", 10, "")))
	s.Apply(ptrMsg(makeAssignMsg("assign-p2", "op-key", "entry-x", "brokered-match", 10, "")))

	count := s.OpenPredictionAssignsForEntry("entry-x")
	if count != 2 {
		t.Errorf("OpenPredictionAssignsForEntry = %d, want 2", count)
	}
}

// TestOpenPredictionAssignsForEntry_ExpiredNotCounted verifies that assigns whose
// DeadlineAt has passed do not count toward the fanout limit.
func TestOpenPredictionAssignsForEntry_ExpiredNotCounted(t *testing.T) {
	t.Parallel()

	s := NewState()
	s.OperatorKey = "op-key"

	past := time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339)
	payload := marshalTestPayload(map[string]any{
		"entry_id":    "entry-y",
		"task_type":   "brokered-match",
		"reward":      int64(10),
		"deadline_at": past,
	})
	msg := Message{ID: "stale-assign", Sender: "op-key", Tags: []string{TagAssign}, Payload: payload}
	s.Apply(&msg)

	count := s.OpenPredictionAssignsForEntry("entry-y")
	if count != 0 {
		t.Errorf("expired assign counted = %d, want 0", count)
	}
}

// TestStalePredictionAssigns verifies that expired open brokered-match assigns
// are returned by StalePredictionAssigns.
func TestStalePredictionAssigns(t *testing.T) {
	t.Parallel()

	s := NewState()
	s.OperatorKey = "op-key"

	past := time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339)
	future := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)

	makeDeadlineAssign := func(id, entryID, deadline string) Message {
		payload := marshalTestPayload(map[string]any{
			"entry_id":    entryID,
			"task_type":   "brokered-match",
			"reward":      int64(10),
			"deadline_at": deadline,
		})
		return Message{ID: id, Sender: "op-key", Tags: []string{TagAssign}, Payload: payload}
	}

	s.Apply(ptrMsg(makeDeadlineAssign("stale-1", "entry-z", past)))
	s.Apply(ptrMsg(makeDeadlineAssign("live-1", "entry-z", future)))

	stale := s.StalePredictionAssigns()
	if len(stale) != 1 {
		t.Fatalf("StalePredictionAssigns len = %d, want 1", len(stale))
	}
	if stale[0] != "stale-1" {
		t.Errorf("stale assign = %q, want %q", stale[0], "stale-1")
	}
}

// TestDeadlineAt_ClaimRejected verifies that an assign-claim is rejected when
// the assign's DeadlineAt has passed.
func TestDeadlineAt_ClaimRejected(t *testing.T) {
	t.Parallel()

	s := NewState()
	s.OperatorKey = "op-key"

	past := time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339)
	payload := marshalTestPayload(map[string]any{
		"entry_id":    "entry-d",
		"task_type":   "brokered-match",
		"reward":      int64(10),
		"deadline_at": past,
	})
	assignMsg := Message{ID: "assign-deadline", Sender: "op-key", Tags: []string{TagAssign}, Payload: payload}
	s.Apply(&assignMsg)

	// Attempt to claim the expired assign.
	claimMsg := makeAssignClaimMsg("claim-1", "worker-key", "assign-deadline")
	s.Apply(&claimMsg)

	// The assign should still be open (claim rejected).
	rec := s.assignByID["assign-deadline"]
	if rec == nil {
		t.Fatal("assign record not found")
	}
	if rec.Status != AssignOpen {
		t.Errorf("status = %v after expired claim, want AssignOpen", rec.Status)
	}
}

// TestDeadlineAt_Parsed verifies that applyAssign correctly parses deadline_at.
func TestDeadlineAt_Parsed(t *testing.T) {
	t.Parallel()

	s := NewState()
	s.OperatorKey = "op-key"

	deadline := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	payload := marshalTestPayload(map[string]any{
		"entry_id":    "entry-e",
		"task_type":   "brokered-match",
		"reward":      int64(10),
		"deadline_at": deadline.Format(time.RFC3339),
	})
	msg := Message{ID: "assign-parse", Sender: "op-key", Tags: []string{TagAssign}, Payload: payload}
	s.Apply(&msg)

	rec := s.assignByID["assign-parse"]
	if rec == nil {
		t.Fatal("assign record not found")
	}
	if rec.DeadlineAt.IsZero() {
		t.Error("DeadlineAt is zero, want non-zero")
	}
	// Allow 1 second tolerance for RFC3339 truncation.
	diff := rec.DeadlineAt.Sub(deadline)
	if diff < -time.Second || diff > time.Second {
		t.Errorf("DeadlineAt = %v, want ~%v (diff=%v)", rec.DeadlineAt, deadline, diff)
	}
}

// TestPredictNext_AfterReplay verifies that co-occurrence data is cleared on Replay
// (it is engine-side state derived from observed settle events, not from the log).
func TestPredictNext_AfterReplay(t *testing.T) {
	t.Parallel()

	s := NewState()
	s.UpdateCoOccurrence("r1", "r2")
	s.UpdateCoOccurrence("r1", "r2")
	s.UpdateCoOccurrence("r1", "r3")

	predicted := s.PredictNext("r1")
	if len(predicted) == 0 {
		t.Fatal("PredictNext returned no results before replay")
	}
	// Sort for determinism.
	sort.Strings(predicted)
	want := []string{"r2", "r3"}
	sort.Strings(want)
	if len(predicted) != len(want) {
		t.Errorf("predicted = %v, want %v", predicted, want)
	}

	// After Replay, co-occurrence map is reset.
	s.Replay(nil)
	afterReplay := s.PredictNext("r1")
	if afterReplay != nil {
		t.Errorf("after Replay, PredictNext = %v, want nil (reset on replay)", afterReplay)
	}
}
