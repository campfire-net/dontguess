package exchange_test

import (
	"testing"
	"time"

	"github.com/campfire-net/dontguess/pkg/exchange"
)

// TestAssignCompletionSamples_EmptyState verifies that an empty state returns
// an empty slice.
func TestAssignCompletionSamples_EmptyState(t *testing.T) {
	t.Parallel()
	s := exchange.NewState()
	samples := s.AssignCompletionSamples()
	if len(samples) != 0 {
		t.Errorf("expected 0 samples from empty state, got %d", len(samples))
	}
}

// TestAssignCompletionSamples_CompletedAssign verifies that a fully completed
// assign (claimed + completed) produces a sample with Completed=true and a
// positive Latency.
func TestAssignCompletionSamples_CompletedAssign(t *testing.T) {
	t.Parallel()
	s := exchange.NewState()

	// Use a future date so that expiry checks in applyAssignComplete do not fire.
	now := time.Now().UTC().Add(24 * time.Hour)
	claimTime := now
	completeTime := now.Add(5 * time.Minute)

	// Build assign → claim → complete message sequence.
	assignMsg := &exchange.Message{
		ID:        "assign-1",
		Tags:      []string{exchange.TagAssign},
		Sender:    "operator-key",
		Payload:   []byte(`{"task_type":"brokered-match","reward":100}`),
		Timestamp: now.UnixNano(),
	}
	s.Apply(assignMsg)

	claimMsg := &exchange.Message{
		ID:          "claim-1",
		Tags:        []string{exchange.TagAssignClaim},
		Sender:      "worker-key",
		Antecedents: []string{"assign-1"},
		Timestamp:   claimTime.UnixNano(),
	}
	s.Apply(claimMsg)

	completeMsg := &exchange.Message{
		ID:          "complete-1",
		Tags:        []string{exchange.TagAssignComplete},
		Sender:      "worker-key",
		Antecedents: []string{"claim-1"},
		Payload:     []byte(`{"result":"done"}`),
		Timestamp:   completeTime.UnixNano(),
	}
	s.Apply(completeMsg)

	samples := s.AssignCompletionSamples()
	if len(samples) != 1 {
		t.Fatalf("expected 1 sample, got %d", len(samples))
	}
	s0 := samples[0]
	if s0.TaskType != "brokered-match" {
		t.Errorf("expected TaskType='brokered-match', got %q", s0.TaskType)
	}
	if !s0.Completed {
		t.Error("expected Completed=true for assign that reached assign-complete")
	}
	if s0.Latency <= 0 {
		t.Errorf("expected positive Latency, got %v", s0.Latency)
	}
	// Latency should be approximately 5 minutes (claim → complete).
	if s0.Latency < 4*time.Minute || s0.Latency > 6*time.Minute {
		t.Errorf("expected Latency≈5m, got %v", s0.Latency)
	}
}

// TestAssignCompletionSamples_UnclaimedAssign verifies that an assign that was
// never claimed produces a sample with Completed=false.
func TestAssignCompletionSamples_UnclaimedAssign(t *testing.T) {
	t.Parallel()
	s := exchange.NewState()

	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	assignMsg := &exchange.Message{
		ID:        "assign-unclaimed",
		Tags:      []string{exchange.TagAssign},
		Sender:    "operator-key",
		Payload:   []byte(`{"task_type":"freshness","reward":50}`),
		Timestamp: now.UnixNano(),
	}
	s.Apply(assignMsg)

	samples := s.AssignCompletionSamples()
	if len(samples) != 1 {
		t.Fatalf("expected 1 sample, got %d", len(samples))
	}
	if samples[0].Completed {
		t.Error("expected Completed=false for unclaimed assign")
	}
	if samples[0].Latency != 0 {
		t.Errorf("expected zero Latency for unclaimed assign, got %v", samples[0].Latency)
	}
	if samples[0].TaskType != "freshness" {
		t.Errorf("expected TaskType='freshness', got %q", samples[0].TaskType)
	}
}

// TestGuaranteeForMatch_NoGuarantee verifies that a match without an insured
// order returns not-found.
func TestGuaranteeForMatch_NoGuarantee(t *testing.T) {
	t.Parallel()
	s := exchange.NewState()

	// Apply a buy without guarantee, then a match.
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	buyMsg := &exchange.Message{
		ID:        "buy-1",
		Tags:      []string{exchange.TagBuy},
		Sender:    "buyer-key",
		Payload:   []byte(`{"task":"find me code","budget":500}`),
		Timestamp: now.UnixNano(),
	}
	s.Apply(buyMsg)

	matchMsg := &exchange.Message{
		ID:          "match-1",
		Tags:        []string{exchange.TagMatch},
		Sender:      "operator-key",
		Antecedents: []string{"buy-1"},
		Payload:     []byte(`{"results":[]}`),
		Timestamp:   now.UnixNano(),
	}
	s.Apply(matchMsg)

	deadline, insuredAmount, ok := s.GuaranteeForMatch("match-1")
	if ok {
		t.Errorf("expected not-found for uninsured order, got deadline=%v amount=%d",
			deadline, insuredAmount)
	}
}

// TestGuaranteeForMatch_WithGuarantee verifies that a buy with guarantee_deadline_seconds
// produces a match with accessible guarantee info.
func TestGuaranteeForMatch_WithGuarantee(t *testing.T) {
	t.Parallel()
	s := exchange.NewState()

	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	// Buy with a 600-second (10-minute) guarantee deadline and 1000 insured amount.
	buyMsg := &exchange.Message{
		ID:        "buy-guaranteed",
		Tags:      []string{exchange.TagBuy},
		Sender:    "buyer-key",
		Payload:   []byte(`{"task":"find me code","budget":1500,"guarantee_deadline_seconds":600,"insured_amount":1000}`),
		Timestamp: now.UnixNano(),
	}
	s.Apply(buyMsg)

	matchMsg := &exchange.Message{
		ID:          "match-guaranteed",
		Tags:        []string{exchange.TagMatch},
		Sender:      "operator-key",
		Antecedents: []string{"buy-guaranteed"},
		Payload:     []byte(`{"results":[]}`),
		Timestamp:   now.UnixNano(),
	}
	s.Apply(matchMsg)

	deadline, insuredAmount, ok := s.GuaranteeForMatch("match-guaranteed")
	if !ok {
		t.Fatal("expected guarantee info for insured order")
	}
	if insuredAmount != 1000 {
		t.Errorf("expected InsuredAmount=1000, got %d", insuredAmount)
	}
	expectedDeadline := now.Add(600 * time.Second)
	if diff := deadline.Sub(expectedDeadline); diff < -time.Second || diff > time.Second {
		t.Errorf("expected deadline≈%v, got %v (diff=%v)", expectedDeadline, deadline, diff)
	}
}

// TestGuaranteeForMatch_PersistsAfterOrderRemoval verifies that GuaranteeForMatch
// still returns the correct deadline and insured amount after the order has been
// accepted (buyer-accept applied). The guarantee data lives in matchGuarantee, which
// is set at match time and is not cleared when the order transitions out of activeOrders.
//
// This is the regression case: if GuaranteeForMatch only inspected activeOrders it
// would return not-found after buyer-accept, and the deadline-miss refund path would
// be silently skipped.
func TestGuaranteeForMatch_PersistsAfterOrderRemoval(t *testing.T) {
	t.Parallel()
	s := exchange.NewState()

	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	// Buy with guarantee: 120-second deadline, 750 insured amount.
	const buyerKey = "buyer-key-persist"
	const operatorKey = "operator-key-persist"
	const insuredAmount int64 = 750
	const deadlineSecs = 120

	buyMsg := &exchange.Message{
		ID:      "buy-persist",
		Tags:    []string{exchange.TagBuy},
		Sender:  buyerKey,
		Payload: []byte(`{"task":"cached code search","budget":2000,"guarantee_deadline_seconds":120,"insured_amount":750}`),
		Timestamp: now.UnixNano(),
	}
	s.Apply(buyMsg)

	matchMsg := &exchange.Message{
		ID:          "match-persist",
		Tags:        []string{exchange.TagMatch},
		Sender:      operatorKey,
		Antecedents: []string{"buy-persist"},
		Payload:     []byte(`{"results":[{"entry_id":"entry-abc"}]}`),
		Timestamp:   now.UnixNano(),
	}
	s.Apply(matchMsg)

	// Verify guarantee is present before buyer-accept.
	deadline, gotAmount, ok := s.GuaranteeForMatch("match-persist")
	if !ok {
		t.Fatal("GuaranteeForMatch before buyer-accept: expected found")
	}
	if gotAmount != insuredAmount {
		t.Errorf("before buyer-accept: insured_amount = %d, want %d", gotAmount, insuredAmount)
	}
	expectedDeadline := now.Add(deadlineSecs * time.Second)
	if diff := deadline.Sub(expectedDeadline); diff < -time.Second || diff > time.Second {
		t.Errorf("before buyer-accept: deadline = %v, want ≈%v", deadline, expectedDeadline)
	}

	// Apply buyer-accept — this transitions the order out of the unmatched-active view
	// and records it in acceptedOrders. The guarantee in matchGuarantee must survive.
	buyerAcceptMsg := &exchange.Message{
		ID:          "buyer-accept-persist",
		Tags:        []string{exchange.TagSettle, exchange.TagPhasePrefix + exchange.SettlePhaseStrBuyerAccept, exchange.TagVerdictPrefix + "accepted"},
		Sender:      buyerKey, // must match the original buyer
		Antecedents: []string{"match-persist"},
		Payload:     []byte(`{"phase":"buyer-accept","entry_id":"entry-abc","accepted":true}`),
		Timestamp:   now.UnixNano(),
	}
	s.Apply(buyerAcceptMsg)

	// Verify guarantee still returns correct values after buyer-accept.
	deadline2, gotAmount2, ok2 := s.GuaranteeForMatch("match-persist")
	if !ok2 {
		t.Fatal("GuaranteeForMatch after buyer-accept: expected found (guarantee must persist)")
	}
	if gotAmount2 != insuredAmount {
		t.Errorf("after buyer-accept: insured_amount = %d, want %d", gotAmount2, insuredAmount)
	}
	if !deadline2.Equal(deadline) {
		t.Errorf("after buyer-accept: deadline changed from %v to %v (must be stable)", deadline, deadline2)
	}
}
