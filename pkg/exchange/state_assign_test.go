package exchange

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
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

// TestAssignCompleteWrongSender: complete message from an agent that did NOT
// claim the task is silently dropped — the assign stays in AssignClaimed.
func TestAssignCompleteWrongSender(t *testing.T) {
	s := NewState()
	s.OperatorKey = operatorKey

	s.Apply(ptr(makeAssignMsg("assign-ws", operatorKey, "entry-ws", "freshness", 50, "")))
	s.Apply(ptr(makeAssignClaimMsg("claim-ws", agentKey, "assign-ws")))

	// agentKey2 did NOT claim — its complete should be dropped.
	s.Apply(ptr(makeAssignCompleteMsg("complete-ws-bad", agentKey2, "claim-ws", nil)))

	rec := s.assignByID["assign-ws"]
	if rec.Status != AssignClaimed {
		t.Fatalf("expected assign still Claimed after wrong-sender complete, got %v", rec.Status)
	}
	if _, pending := s.pendingAssignResults["complete-ws-bad"]; pending {
		t.Fatal("wrong-sender complete must not be indexed in pendingAssignResults")
	}
}

// TestAssignAcceptNonOperator: accept message from a non-operator sender is
// silently dropped — the assign stays in AssignCompleted.
func TestAssignAcceptNonOperator(t *testing.T) {
	s := NewState()
	s.OperatorKey = operatorKey

	s.Apply(ptr(makeAssignMsg("assign-ano", operatorKey, "entry-ano", "freshness", 50, "")))
	s.Apply(ptr(makeAssignClaimMsg("claim-ano", agentKey, "assign-ano")))
	s.Apply(ptr(makeAssignCompleteMsg("complete-ano", agentKey, "claim-ano", nil)))

	// Non-operator tries to accept.
	badAccept := makeAssignAcceptMsg("accept-ano-bad", agentKey, "complete-ano")
	s.Apply(&badAccept)

	rec := s.assignByID["assign-ano"]
	if rec.Status != AssignCompleted {
		t.Fatalf("expected assign still Completed after non-operator accept, got %v", rec.Status)
	}
	if _, pending := s.pendingAssignResults["complete-ano"]; !pending {
		t.Fatal("completed assign must remain in pendingAssignResults after rejected accept")
	}
}

// TestAssignRejectNonOperator: reject message from a non-operator sender is
// silently dropped — the assign stays in AssignCompleted.
func TestAssignRejectNonOperator(t *testing.T) {
	s := NewState()
	s.OperatorKey = operatorKey

	s.Apply(ptr(makeAssignMsg("assign-rno", operatorKey, "entry-rno", "validation", 30, "")))
	s.Apply(ptr(makeAssignClaimMsg("claim-rno", agentKey, "assign-rno")))
	s.Apply(ptr(makeAssignCompleteMsg("complete-rno", agentKey, "claim-rno", nil)))

	// Non-operator tries to reject.
	badReject := makeAssignRejectMsg("reject-rno-bad", agentKey2, "complete-rno")
	s.Apply(&badReject)

	rec := s.assignByID["assign-rno"]
	if rec.Status != AssignCompleted {
		t.Fatalf("expected assign still Completed after non-operator reject, got %v", rec.Status)
	}
}

// TestAssignClaimAlreadyClaimed: a second agent cannot steal a task that is
// already in AssignClaimed state. The original claimant is unchanged.
func TestAssignClaimAlreadyClaimed(t *testing.T) {
	s := NewState()
	s.OperatorKey = operatorKey

	s.Apply(ptr(makeAssignMsg("assign-ac", operatorKey, "entry-ac", "freshness", 100, "")))
	// agentKey claims first.
	s.Apply(ptr(makeAssignClaimMsg("claim-ac-1", agentKey, "assign-ac")))

	// agentKey2 tries to claim the same already-claimed task.
	s.Apply(ptr(makeAssignClaimMsg("claim-ac-2", agentKey2, "assign-ac")))

	rec := s.assignByID["assign-ac"]
	if rec.Status != AssignClaimed {
		t.Fatalf("expected AssignClaimed, got %v", rec.Status)
	}
	if rec.ClaimantKey != agentKey {
		t.Fatalf("original claimant must be retained: expected %s, got %s", agentKey, rec.ClaimantKey)
	}
}

// TestClaimAssignPaymentIdempotent: ClaimAssignPayment transitions
// AssignAccepted → AssignPaid exactly once. A second call on the same
// completeMsgID returns nil (already paid — no double payment).
func TestClaimAssignPaymentIdempotent(t *testing.T) {
	s := NewState()
	s.OperatorKey = operatorKey

	s.Apply(ptr(makeAssignMsg("assign-pay", operatorKey, "entry-pay", "freshness", 75, "")))
	s.Apply(ptr(makeAssignClaimMsg("claim-pay", agentKey, "assign-pay")))
	s.Apply(ptr(makeAssignCompleteMsg("complete-pay", agentKey, "claim-pay", nil)))
	s.Apply(ptr(makeAssignAcceptMsg("accept-pay", operatorKey, "complete-pay")))

	// First call should succeed and return the record.
	rec1 := s.ClaimAssignPayment("complete-pay")
	if rec1 == nil {
		t.Fatal("first ClaimAssignPayment should return the record")
	}
	if rec1.Status != AssignPaid {
		t.Fatalf("expected AssignPaid after first ClaimAssignPayment, got %v", rec1.Status)
	}

	// Second call must return nil — prevents double-payment on replay.
	rec2 := s.ClaimAssignPayment("complete-pay")
	if rec2 != nil {
		t.Fatal("second ClaimAssignPayment must return nil — bounty already paid")
	}
}

// TestApplyAssignReject_StatusObservable verifies that after an operator rejects
// an assign result, the assign record transitions to AssignOpen (re-opened for
// reclaim) and AssignRejected is never set as the final status. The dead write
// bug was: rec.Status = AssignRejected was written then immediately overwritten
// by rec.Status = AssignOpen — making AssignRejected unobservable.
//
// This test guards both:
//  1. The record is in AssignOpen after reject (re-open path works).
//  2. The record is never left in AssignRejected (no orphaned intermediate state).
func TestApplyAssignReject_StatusObservable(t *testing.T) {
	s := NewState()
	s.OperatorKey = operatorKey

	s.Apply(ptr(makeAssignMsg("assign-obs", operatorKey, "entry-obs", "validation", 40, "")))
	s.Apply(ptr(makeAssignClaimMsg("claim-obs", agentKey, "assign-obs")))
	s.Apply(ptr(makeAssignCompleteMsg("complete-obs", agentKey, "claim-obs", nil)))

	// Verify record is in AssignCompleted before reject.
	rec := s.assignByID["assign-obs"]
	if rec == nil {
		t.Fatal("assign-obs not found in assignByID")
	}
	if rec.Status != AssignCompleted {
		t.Fatalf("pre-reject: expected AssignCompleted, got %v", rec.Status)
	}

	// Apply the reject.
	rejectMsg := makeAssignRejectMsg("reject-obs", operatorKey, "complete-obs")
	s.Apply(&rejectMsg)

	// Post-reject: the record must be AssignOpen (re-opened), never AssignRejected.
	if rec.Status == AssignRejected {
		t.Fatalf("post-reject: record is in AssignRejected — this is a dead write bug; status should be AssignOpen")
	}
	if rec.Status != AssignOpen {
		t.Fatalf("post-reject: expected AssignOpen (re-opened for reclaim), got %v", rec.Status)
	}

	// Confirm claimant and complete fields are cleared.
	if rec.ClaimantKey != "" {
		t.Fatalf("post-reject: ClaimantKey not cleared, got %q", rec.ClaimantKey)
	}
	if rec.ClaimMsgID != "" {
		t.Fatalf("post-reject: ClaimMsgID not cleared, got %q", rec.ClaimMsgID)
	}
	if rec.CompleteMsgID != "" {
		t.Fatalf("post-reject: CompleteMsgID not cleared, got %q", rec.CompleteMsgID)
	}

	// Confirm the record no longer appears in pendingAssignResults.
	if _, inPending := s.pendingAssignResults["complete-obs"]; inPending {
		t.Fatal("post-reject: record still in pendingAssignResults after reject")
	}

	// Confirm the record appears in ActiveAssigns (it's re-opened, so it's active).
	actives := s.ActiveAssigns("entry-obs")
	if len(actives) != 1 {
		t.Fatalf("post-reject: expected 1 active assign (re-opened), got %d", len(actives))
	}
	if actives[0].Status != AssignOpen {
		t.Fatalf("post-reject: ActiveAssigns[0].Status = %v, want AssignOpen", actives[0].Status)
	}
}

// makeAssignExpireMsg constructs an exchange:assign-expire message for tests.
// antecedent is the claim message ID that expired.
func makeAssignExpireMsg(id, sender, claimMsgID string) Message {
	payload := map[string]any{
		"claim_id":    claimMsgID,
		"detected_at": "2026-01-01T00:00:00Z",
	}
	b, _ := json.Marshal(payload)
	return Message{
		ID:          id,
		Sender:      sender,
		Tags:        []string{TagAssignExpire},
		Antecedents: []string{claimMsgID},
		Payload:     b,
	}
}

// TestAssignClaimHasExpiresAt verifies that after an assign-claim, the
// AssignRecord has a non-zero ClaimExpiresAt that is in the future.
func TestAssignClaimHasExpiresAt(t *testing.T) {
	s := NewState()
	s.OperatorKey = operatorKey

	before := time.Now()
	s.Apply(ptr(makeAssignMsg("assign-exp1", operatorKey, "entry-exp1", "freshness", 100, "")))
	s.Apply(ptr(makeAssignClaimMsg("claim-exp1", agentKey, "assign-exp1")))
	after := time.Now()

	rec := s.assignByID["assign-exp1"]
	if rec == nil {
		t.Fatal("assign record not found")
	}
	if rec.ClaimExpiresAt.IsZero() {
		t.Fatal("ClaimExpiresAt is zero after claim — expiry timestamp not recorded")
	}
	// Must be in the future relative to now (15 minutes from assign receive time).
	if !rec.ClaimExpiresAt.After(after) {
		t.Errorf("ClaimExpiresAt %v is not after now %v", rec.ClaimExpiresAt, after)
	}
	// Must be within the default timeout window (15 minutes from before).
	ceiling := before.Add(time.Duration(DefaultClaimTimeoutMinutes) * time.Minute)
	if rec.ClaimExpiresAt.After(ceiling.Add(time.Second)) {
		t.Errorf("ClaimExpiresAt %v exceeds ceiling %v", rec.ClaimExpiresAt, ceiling)
	}
}

// TestAssignExpireReopensTask verifies that applying an assign-expire message
// transitions the record from AssignClaimed back to AssignOpen, clears claimant
// fields, and frees the agent's claim slot.
func TestAssignExpireReopensTask(t *testing.T) {
	s := NewState()
	s.OperatorKey = operatorKey

	s.Apply(ptr(makeAssignMsg("assign-xe1", operatorKey, "entry-xe1", "freshness", 100, "")))
	s.Apply(ptr(makeAssignClaimMsg("claim-xe1", agentKey, "assign-xe1")))

	// Verify claimed state.
	rec := s.assignByID["assign-xe1"]
	if rec.Status != AssignClaimed {
		t.Fatalf("pre-expire: expected AssignClaimed, got %v", rec.Status)
	}

	// Apply expire.
	s.Apply(ptr(makeAssignExpireMsg("expire-xe1", operatorKey, "claim-xe1")))

	// Record must be back to AssignOpen.
	if rec.Status != AssignOpen {
		t.Fatalf("post-expire: expected AssignOpen, got %v", rec.Status)
	}
	if rec.ClaimantKey != "" {
		t.Errorf("post-expire: ClaimantKey not cleared, got %q", rec.ClaimantKey)
	}
	if rec.ClaimMsgID != "" {
		t.Errorf("post-expire: ClaimMsgID not cleared, got %q", rec.ClaimMsgID)
	}
	if !rec.ClaimExpiresAt.IsZero() {
		t.Errorf("post-expire: ClaimExpiresAt not cleared, got %v", rec.ClaimExpiresAt)
	}

	// claimedAssigns binding must be freed.
	if _, held := s.claimedAssigns[agentKey]; held {
		t.Error("post-expire: claimedAssigns still holds agentKey binding")
	}

	// ActiveAssigns must return the task as AssignOpen (claimable).
	actives := s.ActiveAssigns("entry-xe1")
	if len(actives) != 1 {
		t.Fatalf("post-expire: expected 1 active assign, got %d", len(actives))
	}
	if actives[0].Status != AssignOpen {
		t.Fatalf("post-expire: ActiveAssigns status = %v, want AssignOpen", actives[0].Status)
	}
}

// TestAssignExpireNonOperatorDropped verifies that a non-operator assign-expire
// message is silently dropped — the assign stays in AssignClaimed.
func TestAssignExpireNonOperatorDropped(t *testing.T) {
	s := NewState()
	s.OperatorKey = operatorKey

	s.Apply(ptr(makeAssignMsg("assign-xe2", operatorKey, "entry-xe2", "freshness", 50, "")))
	s.Apply(ptr(makeAssignClaimMsg("claim-xe2", agentKey, "assign-xe2")))

	// Non-operator tries to expire the claim.
	s.Apply(ptr(makeAssignExpireMsg("expire-xe2-bad", agentKey2, "claim-xe2")))

	rec := s.assignByID["assign-xe2"]
	if rec.Status != AssignClaimed {
		t.Fatalf("expected AssignClaimed after non-operator expire, got %v", rec.Status)
	}
	if rec.ClaimantKey != agentKey {
		t.Errorf("claimant changed after non-operator expire: got %q", rec.ClaimantKey)
	}
}

// TestAssignExpireIdempotent verifies that replaying the same expire message is
// a no-op — the record stays in AssignOpen after the first expire.
func TestAssignExpireIdempotent(t *testing.T) {
	s := NewState()
	s.OperatorKey = operatorKey

	s.Apply(ptr(makeAssignMsg("assign-xe3", operatorKey, "entry-xe3", "freshness", 100, "")))
	s.Apply(ptr(makeAssignClaimMsg("claim-xe3", agentKey, "assign-xe3")))
	s.Apply(ptr(makeAssignExpireMsg("expire-xe3", operatorKey, "claim-xe3")))

	// First expire — should be AssignOpen now.
	rec := s.assignByID["assign-xe3"]
	if rec.Status != AssignOpen {
		t.Fatalf("after first expire: expected AssignOpen, got %v", rec.Status)
	}

	// Replay same expire — no-op.
	s.Apply(ptr(makeAssignExpireMsg("expire-xe3", operatorKey, "claim-xe3")))
	if rec.Status != AssignOpen {
		t.Fatalf("after replay expire: expected AssignOpen, got %v", rec.Status)
	}
}

// TestAssignExpireAllowsReclaim verifies that after a claim expires, another
// agent can claim the task.
func TestAssignExpireAllowsReclaim(t *testing.T) {
	s := NewState()
	s.OperatorKey = operatorKey

	s.Apply(ptr(makeAssignMsg("assign-xe4", operatorKey, "entry-xe4", "validation", 75, "")))
	s.Apply(ptr(makeAssignClaimMsg("claim-xe4", agentKey, "assign-xe4")))
	s.Apply(ptr(makeAssignExpireMsg("expire-xe4", operatorKey, "claim-xe4")))

	// A different agent should now be able to claim.
	s.Apply(ptr(makeAssignClaimMsg("claim-xe4-2", agentKey2, "assign-xe4")))

	rec := s.assignByID["assign-xe4"]
	if rec.Status != AssignClaimed {
		t.Fatalf("expected AssignClaimed after reclaim, got %v", rec.Status)
	}
	if rec.ClaimantKey != agentKey2 {
		t.Fatalf("expected claimant %s after reclaim, got %s", agentKey2, rec.ClaimantKey)
	}
}

// TestAssignActiveAssignsLazyExpiry verifies that ActiveAssigns returns
// an expired claim as AssignOpen (effective status) without requiring an
// explicit assign-expire message.
func TestAssignActiveAssignsLazyExpiry(t *testing.T) {
	s := NewState()
	s.OperatorKey = operatorKey

	// Post and claim an assign.
	s.Apply(ptr(makeAssignMsg("assign-lazy1", operatorKey, "entry-lazy1", "freshness", 100, "")))
	s.Apply(ptr(makeAssignClaimMsg("claim-lazy1", agentKey, "assign-lazy1")))

	// Artificially set the claim expiry to the past.
	rec := s.assignByID["assign-lazy1"]
	rec.ClaimExpiresAt = time.Now().Add(-1 * time.Minute)

	// ActiveAssigns should return the task with effective status AssignOpen.
	actives := s.ActiveAssigns("entry-lazy1")
	if len(actives) != 1 {
		t.Fatalf("expected 1 active assign, got %d", len(actives))
	}
	if actives[0].Status != AssignOpen {
		t.Fatalf("lazy expiry: expected AssignOpen, got %v", actives[0].Status)
	}
	if actives[0].ClaimantKey != "" {
		t.Errorf("lazy expiry: expected empty ClaimantKey, got %q", actives[0].ClaimantKey)
	}

	// Internal record must still be AssignClaimed (lazy — no mutation yet).
	if rec.Status != AssignClaimed {
		t.Fatalf("internal record must remain AssignClaimed until expire msg applied, got %v", rec.Status)
	}
}

// TestAssignExpireSweepDetectsExpired verifies that ExpireStaleClaims returns
// the claim message IDs of records whose TTL has elapsed.
func TestAssignExpireSweepDetectsExpired(t *testing.T) {
	s := NewState()
	s.OperatorKey = operatorKey

	s.Apply(ptr(makeAssignMsg("assign-sw1", operatorKey, "entry-sw1", "freshness", 100, "")))
	s.Apply(ptr(makeAssignClaimMsg("claim-sw1", agentKey, "assign-sw1")))

	// Claim not yet expired.
	expired := s.ExpireStaleClaims()
	if len(expired) != 0 {
		t.Fatalf("expected 0 expired before TTL, got %d", len(expired))
	}

	// Artificially expire the claim.
	rec := s.assignByID["assign-sw1"]
	rec.ClaimExpiresAt = time.Now().Add(-1 * time.Second)

	expired = s.ExpireStaleClaims()
	if len(expired) != 1 {
		t.Fatalf("expected 1 expired after TTL, got %d", len(expired))
	}
	if expired[0] != "claim-sw1" {
		t.Errorf("expected claim-sw1 in expired list, got %q", expired[0])
	}
}

// TestAssignExpireReplay verifies the done condition: simulate claim + timeout
// + replay of assign-expire, assert assign is re-opened. This mirrors what
// happens when the engine replays the campfire log on restart.
func TestAssignExpireReplay(t *testing.T) {
	msgs := []Message{
		makeAssignMsg("assign-rpl1", operatorKey, "entry-rpl1", "freshness", 100, ""),
		makeAssignClaimMsg("claim-rpl1", agentKey, "assign-rpl1"),
		makeAssignExpireMsg("expire-rpl1", operatorKey, "claim-rpl1"),
	}

	s := NewState()
	s.OperatorKey = operatorKey
	s.Replay(msgs)

	// After replay, the assign must be AssignOpen (re-opened by the expire message).
	actives := s.ActiveAssigns("entry-rpl1")
	if len(actives) != 1 {
		t.Fatalf("expected 1 active assign after replay, got %d", len(actives))
	}
	if actives[0].Status != AssignOpen {
		t.Fatalf("after replay: expected AssignOpen, got %v", actives[0].Status)
	}
	if actives[0].ClaimantKey != "" {
		t.Errorf("after replay: expected empty ClaimantKey, got %q", actives[0].ClaimantKey)
	}

	// A new agent should be able to claim the reopened task.
	claimMsg := makeAssignClaimMsg("claim-rpl1-2", agentKey2, "assign-rpl1")
	s.Apply(&claimMsg)

	actives = s.ActiveAssigns("entry-rpl1")
	if actives[0].Status != AssignClaimed {
		t.Fatalf("after reclaim: expected AssignClaimed, got %v", actives[0].Status)
	}
	if actives[0].ClaimantKey != agentKey2 {
		t.Fatalf("after reclaim: expected claimant %s, got %s", agentKey2, actives[0].ClaimantKey)
	}
}

// TestAssignExpireCompleteAfterExpiry verifies that a late assign-complete
// (submitted after the claim TTL) is dropped by the state machine.
func TestAssignExpireCompleteAfterExpiry(t *testing.T) {
	s := NewState()
	s.OperatorKey = operatorKey

	s.Apply(ptr(makeAssignMsg("assign-late1", operatorKey, "entry-late1", "freshness", 100, "")))
	s.Apply(ptr(makeAssignClaimMsg("claim-late1", agentKey, "assign-late1")))

	// Set expiry to the past.
	rec := s.assignByID["assign-late1"]
	rec.ClaimExpiresAt = time.Now().Add(-1 * time.Second)

	// Submit complete after expiry — must be dropped.
	s.Apply(ptr(makeAssignCompleteMsg("complete-late1", agentKey, "claim-late1", nil)))

	if rec.Status != AssignClaimed {
		t.Fatalf("expected AssignClaimed (complete dropped), got %v", rec.Status)
	}
	if _, pending := s.pendingAssignResults["complete-late1"]; pending {
		t.Fatal("late complete must not be indexed in pendingAssignResults")
	}
}

// TestAssignClaimCustomTimeout verifies that a claim with a custom (shorter)
// expires_at honours the claimant-supplied value when within the ceiling.
func TestAssignClaimCustomTimeout(t *testing.T) {
	s := NewState()
	s.OperatorKey = operatorKey

	s.Apply(ptr(makeAssignMsg("assign-ct1", operatorKey, "entry-ct1", "freshness", 100, "")))

	// Claim with a 5-minute custom expires_at.
	shortExpiry := time.Now().Add(5 * time.Minute).UTC()
	claimPayload, _ := json.Marshal(map[string]string{
		"expires_at": shortExpiry.Format(time.RFC3339),
	})
	claimMsg := Message{
		ID:          "claim-ct1",
		Sender:      agentKey,
		Tags:        []string{TagAssignClaim},
		Antecedents: []string{"assign-ct1"},
		Payload:     claimPayload,
	}
	s.Apply(&claimMsg)

	rec := s.assignByID["assign-ct1"]
	if rec.ClaimExpiresAt.IsZero() {
		t.Fatal("ClaimExpiresAt is zero")
	}
	// Should be at most 6 minutes from now (5 min + 1 min buffer for rounding).
	maxExpiry := time.Now().Add(6 * time.Minute)
	if rec.ClaimExpiresAt.After(maxExpiry) {
		t.Errorf("ClaimExpiresAt %v exceeds expected ceiling %v", rec.ClaimExpiresAt, maxExpiry)
	}
	// Should not have been pushed to the full 15-minute default.
	minDefault := time.Now().Add(14 * time.Minute)
	if rec.ClaimExpiresAt.After(minDefault) {
		t.Errorf("ClaimExpiresAt %v was rounded up to the default timeout — custom value ignored", rec.ClaimExpiresAt)
	}
}

// ptr is a helper that takes a value and returns a pointer to it.
func ptr(m Message) *Message { return &m }

// --- Vickrey Auction Tests ---

const agentKey3 = "445566"

// makeAuctionAssignMsg creates an exchange:assign message with an auction window.
func makeAuctionAssignMsg(id, sender, taskType string, reward int64, auctionWindowSeconds int, receivedAt time.Time) Message {
	payload := map[string]any{
		"task_type":              taskType,
		"reward":                 reward,
		"auction_window_seconds": auctionWindowSeconds,
	}
	b, _ := json.Marshal(payload)
	return Message{
		ID:        id,
		Sender:    sender,
		Tags:      []string{TagAssign},
		Payload:   b,
		Timestamp: receivedAt.UnixNano(),
	}
}

// makeAuctionBidMsg creates an exchange:assign-claim message with a bid field.
func makeAuctionBidMsg(id, sender, assignID string, bid int64, sentAt time.Time) Message {
	payload := map[string]any{"bid": bid}
	b, _ := json.Marshal(payload)
	return Message{
		ID:          id,
		Sender:      sender,
		Tags:        []string{TagAssignClaim},
		Antecedents: []string{assignID},
		Payload:     b,
		Timestamp:   sentAt.UnixNano(),
	}
}

// makeAuctionCloseMsg creates an exchange:assign-auction-close message.
func makeAuctionCloseMsg(id, sender, assignID string) Message {
	payload := map[string]any{
		"assign_id": assignID,
		"closed_at": time.Now().UTC().Format(time.RFC3339),
	}
	b, _ := json.Marshal(payload)
	return Message{
		ID:          id,
		Sender:      sender,
		Tags:        []string{TagAssignAuctionClose},
		Antecedents: []string{assignID},
		Payload:     b,
	}
}

// TestAuctionBidAccumulatesDuringWindow verifies that assign-claim messages with
// bid fields are recorded in AuctionBids and do not claim the task directly.
func TestAuctionBidAccumulatesDuringWindow(t *testing.T) {
	s := NewState()
	s.OperatorKey = operatorKey

	now := time.Now().UTC()
	s.Apply(ptr(makeAuctionAssignMsg("assign-auc1", operatorKey, "freshness", 100, 60, now)))

	rec := s.assignByID["assign-auc1"]
	if rec == nil {
		t.Fatal("assign record not found")
	}

	// Two bids arrive within the auction window.
	bid1Time := now.Add(5 * time.Second)
	bid2Time := now.Add(10 * time.Second)
	s.Apply(ptr(makeAuctionBidMsg("bid-auc1-a", agentKey, "assign-auc1", 80, bid1Time)))
	s.Apply(ptr(makeAuctionBidMsg("bid-auc1-b", agentKey2, "assign-auc1", 90, bid2Time)))

	// Task must still be AssignOpen (not yet claimed).
	if rec.Status != AssignOpen {
		t.Errorf("expected AssignOpen during auction window, got %v", rec.Status)
	}
	if len(rec.AuctionBids) != 2 {
		t.Fatalf("expected 2 bids, got %d", len(rec.AuctionBids))
	}
	// No claimant yet.
	if rec.ClaimantKey != "" {
		t.Errorf("ClaimantKey should be empty during auction, got %q", rec.ClaimantKey)
	}
}

// TestAuctionVickreyWinnerSelection verifies that the auction-close message
// selects the lowest bidder and sets the Vickrey clearing price (second-lowest bid).
func TestAuctionVickreyWinnerSelection(t *testing.T) {
	s := NewState()
	s.OperatorKey = operatorKey

	now := time.Now().UTC()
	// Auction window: 60s. All bids come in during window.
	s.Apply(ptr(makeAuctionAssignMsg("assign-vic1", operatorKey, "freshness", 100, 60, now)))

	bidTime := now.Add(5 * time.Second)
	s.Apply(ptr(makeAuctionBidMsg("bid-vic1-low", agentKey, "assign-vic1", 70, bidTime)))   // lowest
	s.Apply(ptr(makeAuctionBidMsg("bid-vic1-mid", agentKey2, "assign-vic1", 85, bidTime)))  // second-lowest
	s.Apply(ptr(makeAuctionBidMsg("bid-vic1-high", agentKey3, "assign-vic1", 95, bidTime))) // highest

	// Close the auction (after window).
	s.Apply(ptr(makeAuctionCloseMsg("close-vic1", operatorKey, "assign-vic1")))

	rec := s.assignByID["assign-vic1"]
	if rec.Status != AssignClaimed {
		t.Fatalf("expected AssignClaimed after auction close, got %v", rec.Status)
	}
	if rec.ClaimantKey != agentKey {
		t.Errorf("expected winner %s (lowest bid), got %s", agentKey, rec.ClaimantKey)
	}
	// Vickrey clearing price = second-lowest bid (85).
	if rec.VickreyPrice != 85 {
		t.Errorf("expected VickreyPrice=85 (second-lowest bid), got %d", rec.VickreyPrice)
	}
	// ClaimMsgID should be the auction-close message ID.
	if rec.ClaimMsgID != "close-vic1" {
		t.Errorf("expected ClaimMsgID=close-vic1, got %s", rec.ClaimMsgID)
	}
}

// TestAuctionSingleBidderClearingPrice verifies that with one bidder, the
// clearing price equals the winner's own bid (no second-price available).
func TestAuctionSingleBidderClearingPrice(t *testing.T) {
	s := NewState()
	s.OperatorKey = operatorKey

	now := time.Now().UTC()
	s.Apply(ptr(makeAuctionAssignMsg("assign-solo1", operatorKey, "freshness", 100, 60, now)))

	bidTime := now.Add(5 * time.Second)
	s.Apply(ptr(makeAuctionBidMsg("bid-solo1", agentKey, "assign-solo1", 75, bidTime)))

	s.Apply(ptr(makeAuctionCloseMsg("close-solo1", operatorKey, "assign-solo1")))

	rec := s.assignByID["assign-solo1"]
	if rec.Status != AssignClaimed {
		t.Fatalf("expected AssignClaimed, got %v", rec.Status)
	}
	if rec.VickreyPrice != 75 {
		t.Errorf("expected VickreyPrice=75 (solo bid), got %d", rec.VickreyPrice)
	}
}

// TestAuctionNoBidsRemainsOpen verifies that an auction-close with no bids
// leaves the assign in AssignOpen state.
func TestAuctionNoBidsRemainsOpen(t *testing.T) {
	s := NewState()
	s.OperatorKey = operatorKey

	now := time.Now().UTC()
	s.Apply(ptr(makeAuctionAssignMsg("assign-nobid", operatorKey, "freshness", 100, 60, now)))

	// Close immediately without any bids.
	s.Apply(ptr(makeAuctionCloseMsg("close-nobid", operatorKey, "assign-nobid")))

	rec := s.assignByID["assign-nobid"]
	if rec.Status != AssignOpen {
		t.Errorf("expected AssignOpen (no bids), got %v", rec.Status)
	}
}

// TestAuctionBidCeilingRejected verifies that bids exceeding 10x base_reward
// are silently dropped.
func TestAuctionBidCeilingRejected(t *testing.T) {
	s := NewState()
	s.OperatorKey = operatorKey

	now := time.Now().UTC()
	s.Apply(ptr(makeAuctionAssignMsg("assign-ceil1", operatorKey, "freshness", 100, 60, now)))

	bidTime := now.Add(5 * time.Second)
	// 1001 > 100 * 10 — should be rejected (strictly above ceiling).
	s.Apply(ptr(makeAuctionBidMsg("bid-ceil1-bad", agentKey, "assign-ceil1", 1001, bidTime)))
	// 1000 == 100 * 10 — exactly at ceiling, accepted (spec: reject bids > 10x).
	s.Apply(ptr(makeAuctionBidMsg("bid-ceil1-edge", agentKey2, "assign-ceil1", 1000, bidTime)))
	// 999 < 100 * 10 — valid.
	s.Apply(ptr(makeAuctionBidMsg("bid-ceil1-ok", agentKey3, "assign-ceil1", 999, bidTime)))

	rec := s.assignByID["assign-ceil1"]
	if len(rec.AuctionBids) != 2 {
		t.Errorf("expected 2 valid bids (only strictly-above-ceiling rejected), got %d", len(rec.AuctionBids))
	}
	// All surviving bids should not be from agentKey (the rejected one).
	for _, bid := range rec.AuctionBids {
		if bid.WorkerKey == agentKey {
			t.Errorf("bid from agentKey (1001) should have been rejected")
		}
	}
}

// TestAuctionBidDeduplication verifies that a second bid from the same worker
// replaces the first only if the new bid is lower.
func TestAuctionBidDeduplication(t *testing.T) {
	s := NewState()
	s.OperatorKey = operatorKey

	now := time.Now().UTC()
	s.Apply(ptr(makeAuctionAssignMsg("assign-dedup", operatorKey, "freshness", 100, 60, now)))

	t1 := now.Add(5 * time.Second)
	t2 := now.Add(10 * time.Second)
	t3 := now.Add(15 * time.Second)

	s.Apply(ptr(makeAuctionBidMsg("bid-dedup1", agentKey, "assign-dedup", 80, t1)))
	// Higher bid from same worker — should be ignored.
	s.Apply(ptr(makeAuctionBidMsg("bid-dedup2", agentKey, "assign-dedup", 90, t2)))
	// Lower bid from same worker — should replace.
	s.Apply(ptr(makeAuctionBidMsg("bid-dedup3", agentKey, "assign-dedup", 60, t3)))

	rec := s.assignByID["assign-dedup"]
	if len(rec.AuctionBids) != 1 {
		t.Fatalf("expected 1 bid entry (deduplication), got %d", len(rec.AuctionBids))
	}
	if rec.AuctionBids[0].BidAmount != 60 {
		t.Errorf("expected deduped bid of 60, got %d", rec.AuctionBids[0].BidAmount)
	}
}

// TestAuctionDifficultyTier verifies that DifficultyTier is derived correctly
// from the median bid vs base_reward ratio.
func TestAuctionDifficultyTier(t *testing.T) {
	cases := []struct {
		name     string
		reward   int64
		bids     []int64
		wantTier string
	}{
		// median=90, ratio=0.9 → low
		{"low tier", 100, []int64{80, 90, 95}, "low"},
		// median=300, ratio=3.0 → medium
		{"medium tier", 100, []int64{200, 300, 400}, "medium"},
		// median=700, ratio=7.0 → high
		{"high tier", 100, []int64{600, 700, 800}, "high"},
	}

	now := time.Now().UTC()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := NewState()
			s.OperatorKey = operatorKey

			assignID := "assign-tier-" + tc.name
			s.Apply(ptr(makeAuctionAssignMsg(assignID, operatorKey, "freshness", tc.reward, 60, now)))

			for i, bid := range tc.bids {
				workerKey := []string{agentKey, agentKey2, agentKey3}[i%3]
				bidID := fmt.Sprintf("bid-%s-%d", tc.name, i)
				bidTime := now.Add(time.Duration(i+1) * time.Second)
				s.Apply(ptr(makeAuctionBidMsg(bidID, workerKey, assignID, bid, bidTime)))
			}

			closeID := "close-tier-" + tc.name
			s.Apply(ptr(makeAuctionCloseMsg(closeID, operatorKey, assignID)))

			rec := s.assignByID[assignID]
			if rec.DifficultyTier != tc.wantTier {
				t.Errorf("DifficultyTier = %q, want %q", rec.DifficultyTier, tc.wantTier)
			}
		})
	}
}

// TestAuctionBidAfterDeadlineIgnored verifies that a bid arriving after the
// auction window has closed is silently dropped.
func TestAuctionBidAfterDeadlineIgnored(t *testing.T) {
	s := NewState()
	s.OperatorKey = operatorKey

	now := time.Now().UTC()
	// Auction window of 60s.
	s.Apply(ptr(makeAuctionAssignMsg("assign-late", operatorKey, "freshness", 100, 60, now)))

	// This bid arrives 90s after assign — past the 60s window.
	lateBidTime := now.Add(90 * time.Second)
	s.Apply(ptr(makeAuctionBidMsg("bid-late1", agentKey, "assign-late", 80, lateBidTime)))

	rec := s.assignByID["assign-late"]
	if len(rec.AuctionBids) != 0 {
		t.Errorf("expected 0 bids (late bid dropped), got %d", len(rec.AuctionBids))
	}
}

// TestPendingAuctionClose verifies that PendingAuctionClose returns assigns
// whose AuctionDeadline has passed and that have bids, but not others.
func TestPendingAuctionClose(t *testing.T) {
	s := NewState()
	s.OperatorKey = operatorKey

	now := time.Now().UTC()
	past := now.Add(-10 * time.Minute) // started 10 min ago, 60s window → past deadline
	future := now.Add(5 * time.Minute) // started 5 min from now → future

	// Expired auction with bids.
	s.Apply(ptr(makeAuctionAssignMsg("assign-pa1", operatorKey, "freshness", 100, 60, past)))
	s.Apply(ptr(makeAuctionBidMsg("bid-pa1", agentKey, "assign-pa1", 80, past.Add(5*time.Second))))

	// Non-auction assign (no AuctionDeadline).
	s.Apply(ptr(makeAssignMsg("assign-pa2", operatorKey, "", "freshness", 100, "")))

	// Auction with future deadline.
	s.Apply(ptr(makeAuctionAssignMsg("assign-pa3", operatorKey, "freshness", 100, 60*60*6 /*6h*/, future)))
	s.Apply(ptr(makeAuctionBidMsg("bid-pa3", agentKey2, "assign-pa3", 80, future.Add(5*time.Second))))

	// Expired auction but no bids.
	s.Apply(ptr(makeAuctionAssignMsg("assign-pa4", operatorKey, "freshness", 100, 60, past)))

	s.mu.RLock()
	pending := s.PendingAuctionClose()
	s.mu.RUnlock()

	if len(pending) != 1 {
		t.Fatalf("expected 1 pending auction close, got %d: %v", len(pending), pending)
	}
	if pending[0] != "assign-pa1" {
		t.Errorf("expected assign-pa1, got %s", pending[0])
	}
}
