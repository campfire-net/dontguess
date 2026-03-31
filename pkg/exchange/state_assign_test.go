package exchange

import (
	"encoding/json"
	"testing"
)

// makeAssignMsg constructs a minimal exchange:assign message for tests.
func makeAssignMsg(id, sender, entryID, taskType string, reward int64, exclusiveSender string) Message {
	payload := map[string]any{
		"entry_id":  entryID,
		"task_type": taskType,
		"reward":    reward,
	}
	if exclusiveSender != "" {
		payload["exclusive_sender"] = exclusiveSender
	}
	b, _ := json.Marshal(payload)
	return Message{
		ID:      id,
		Sender:  sender,
		Tags:    []string{TagAssign},
		Payload: b,
	}
}

func makeAssignClaimMsg(id, sender, assignID string) Message {
	return Message{
		ID:          id,
		Sender:      sender,
		Tags:        []string{TagAssignClaim},
		Antecedents: []string{assignID},
		Payload:     []byte(`{}`),
	}
}

func makeAssignCompleteMsg(id, sender, claimMsgID string, result []byte) Message {
	if result == nil {
		result = []byte(`{"output":"done"}`)
	}
	return Message{
		ID:          id,
		Sender:      sender,
		Tags:        []string{TagAssignComplete},
		Antecedents: []string{claimMsgID},
		Payload:     result,
	}
}

func makeAssignAcceptMsg(id, sender, completeMsgID string) Message {
	return Message{
		ID:          id,
		Sender:      sender,
		Tags:        []string{TagAssignAccept},
		Antecedents: []string{completeMsgID},
		Payload:     []byte(`{}`),
	}
}

func makeAssignRejectMsg(id, sender, completeMsgID string) Message {
	return Message{
		ID:          id,
		Sender:      sender,
		Tags:        []string{TagAssignReject},
		Antecedents: []string{completeMsgID},
		Payload:     []byte(`{}`),
	}
}

const (
	operatorKey = "aabbcc"
	agentKey    = "ddeeff"
	agentKey2   = "112233"
)

// TestAssignFullLifecycle: assign → claim → complete → accept
func TestAssignFullLifecycle(t *testing.T) {
	s := NewState()
	s.OperatorKey = operatorKey

	// 1. Assign
	assignMsg := makeAssignMsg("assign-1", operatorKey, "entry-abc", "freshness", 100, "")
	s.Apply(&assignMsg)

	actives := s.ActiveAssigns("entry-abc")
	if len(actives) != 1 {
		t.Fatalf("expected 1 active assign, got %d", len(actives))
	}
	if actives[0].Status != AssignOpen {
		t.Fatalf("expected AssignOpen, got %v", actives[0].Status)
	}

	// 2. Claim
	claimMsg := makeAssignClaimMsg("claim-1", agentKey, "assign-1")
	s.Apply(&claimMsg)

	actives = s.ActiveAssigns("entry-abc")
	if len(actives) != 1 {
		t.Fatalf("expected 1 active after claim, got %d", len(actives))
	}
	if actives[0].Status != AssignClaimed {
		t.Fatalf("expected AssignClaimed, got %v", actives[0].Status)
	}
	if actives[0].ClaimantKey != agentKey {
		t.Fatalf("expected claimant %s, got %s", agentKey, actives[0].ClaimantKey)
	}

	// 3. Complete
	completeMsg := makeAssignCompleteMsg("complete-1", agentKey, "claim-1", nil)
	s.Apply(&completeMsg)

	actives = s.ActiveAssigns("entry-abc")
	if len(actives) != 1 {
		t.Fatalf("expected 1 active after complete, got %d", len(actives))
	}
	if actives[0].Status != AssignCompleted {
		t.Fatalf("expected AssignCompleted, got %v", actives[0].Status)
	}

	// 4. Accept
	acceptMsg := makeAssignAcceptMsg("accept-1", operatorKey, "complete-1")
	s.Apply(&acceptMsg)

	// After accept, no more active assigns (terminal state)
	actives = s.ActiveAssigns("entry-abc")
	if len(actives) != 0 {
		t.Fatalf("expected 0 active after accept, got %d", len(actives))
	}

	// Verify the record is in accepted state by directly checking internal map.
	rec := s.assignByID["assign-1"]
	if rec == nil {
		t.Fatal("assign record not found in assignByID")
	}
	if rec.Status != AssignAccepted {
		t.Fatalf("expected AssignAccepted, got %v", rec.Status)
	}
}

// TestAssignRejectPath: assign → claim → complete → reject → re-open
func TestAssignRejectPath(t *testing.T) {
	s := NewState()
	s.OperatorKey = operatorKey

	s.Apply(ptr(makeAssignMsg("assign-2", operatorKey, "entry-xyz", "validation", 50, "")))
	s.Apply(ptr(makeAssignClaimMsg("claim-2", agentKey, "assign-2")))
	s.Apply(ptr(makeAssignCompleteMsg("complete-2", agentKey, "claim-2", nil)))

	// Reject
	rejectMsg := makeAssignRejectMsg("reject-2", operatorKey, "complete-2")
	s.Apply(&rejectMsg)

	// Task should be back to Open so another agent can claim it.
	actives := s.ActiveAssigns("entry-xyz")
	if len(actives) != 1 {
		t.Fatalf("expected 1 active after reject (re-opened), got %d", len(actives))
	}
	if actives[0].Status != AssignOpen {
		t.Fatalf("expected AssignOpen after reject, got %v", actives[0].Status)
	}
	// ClaimantKey should be cleared.
	if actives[0].ClaimantKey != "" {
		t.Fatalf("expected empty claimant after reject, got %s", actives[0].ClaimantKey)
	}

	// Agent that was rejected should be able to claim again (slot freed on complete).
	claimMsg2 := makeAssignClaimMsg("claim-2b", agentKey2, "assign-2")
	s.Apply(&claimMsg2)

	actives = s.ActiveAssigns("entry-xyz")
	if actives[0].Status != AssignClaimed {
		t.Fatalf("expected AssignClaimed after re-claim, got %v", actives[0].Status)
	}
	if actives[0].ClaimantKey != agentKey2 {
		t.Fatalf("expected claimant %s, got %s", agentKey2, actives[0].ClaimantKey)
	}
}

// TestExclusiveAssignClaimEnforcement: exclusive assigns reject claims from wrong sender.
func TestExclusiveAssignClaimEnforcement(t *testing.T) {
	s := NewState()
	s.OperatorKey = operatorKey

	// Assign is exclusive to agentKey.
	s.Apply(ptr(makeAssignMsg("assign-3", operatorKey, "entry-111", "compression", 200, agentKey)))

	// Wrong agent tries to claim — should be rejected.
	s.Apply(ptr(makeAssignClaimMsg("claim-3-wrong", agentKey2, "assign-3")))

	actives := s.ActiveAssigns("entry-111")
	if actives[0].Status != AssignOpen {
		t.Fatalf("expected assign still open after wrong-sender claim, got %v", actives[0].Status)
	}

	// Correct agent claims — should succeed.
	s.Apply(ptr(makeAssignClaimMsg("claim-3-ok", agentKey, "assign-3")))

	actives = s.ActiveAssigns("entry-111")
	if actives[0].Status != AssignClaimed {
		t.Fatalf("expected AssignClaimed after correct-sender claim, got %v", actives[0].Status)
	}
	if actives[0].ClaimantKey != agentKey {
		t.Fatalf("expected claimant %s, got %s", agentKey, actives[0].ClaimantKey)
	}
}

// TestAssignClaimDropsDuplicate: an agent cannot hold two active claims.
func TestAssignClaimDropsDuplicate(t *testing.T) {
	s := NewState()
	s.OperatorKey = operatorKey

	// Two open assigns.
	s.Apply(ptr(makeAssignMsg("assign-a", operatorKey, "", "freshness", 10, "")))
	s.Apply(ptr(makeAssignMsg("assign-b", operatorKey, "", "validation", 20, "")))

	// Claim first.
	s.Apply(ptr(makeAssignClaimMsg("claim-a", agentKey, "assign-a")))

	// Try to claim second — should be dropped (agent already holds a claim).
	s.Apply(ptr(makeAssignClaimMsg("claim-b", agentKey, "assign-b")))

	recA := s.assignByID["assign-a"]
	recB := s.assignByID["assign-b"]

	if recA.Status != AssignClaimed {
		t.Fatalf("assign-a should be Claimed, got %v", recA.Status)
	}
	if recB.Status != AssignOpen {
		t.Fatalf("assign-b should still be Open, got %v", recB.Status)
	}
}

// TestAssignOperatorOnlyPosting: non-operator assign messages are dropped.
func TestAssignOperatorOnlyPosting(t *testing.T) {
	s := NewState()
	s.OperatorKey = operatorKey

	// Non-operator tries to post an assign.
	s.Apply(ptr(makeAssignMsg("assign-bad", agentKey, "entry-xyz", "freshness", 10, "")))

	actives := s.ActiveAssigns("entry-xyz")
	if len(actives) != 0 {
		t.Fatalf("expected non-operator assign to be dropped, got %d", len(actives))
	}
}

// TestAssignNoOperatorKeySet: when OperatorKey is empty, any sender may post assigns.
func TestAssignNoOperatorKeySet(t *testing.T) {
	s := NewState() // no OperatorKey

	s.Apply(ptr(makeAssignMsg("assign-open", agentKey, "entry-any", "validation", 5, "")))

	actives := s.ActiveAssigns("entry-any")
	if len(actives) != 1 {
		t.Fatalf("expected 1 assign when no operator restriction, got %d", len(actives))
	}
}

// TestAssignReplayIdempotent: replaying the same messages produces the same state.
func TestAssignReplayIdempotent(t *testing.T) {
	msgs := []Message{
		makeAssignMsg("assign-r", operatorKey, "entry-r", "freshness", 100, ""),
		makeAssignClaimMsg("claim-r", agentKey, "assign-r"),
		makeAssignCompleteMsg("complete-r", agentKey, "claim-r", nil),
		makeAssignAcceptMsg("accept-r", operatorKey, "complete-r"),
	}

	s := NewState()
	s.OperatorKey = operatorKey
	s.Replay(msgs)

	actives := s.ActiveAssigns("entry-r")
	if len(actives) != 0 {
		t.Fatalf("expected 0 active after full lifecycle replay, got %d", len(actives))
	}
	rec := s.assignByID["assign-r"]
	if rec.Status != AssignAccepted {
		t.Fatalf("expected AssignAccepted after replay, got %v", rec.Status)
	}
}

// ptr is a helper that takes a value and returns a pointer to it.
func ptr(m Message) *Message { return &m }
