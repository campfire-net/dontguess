package exchange_test

import (
	"testing"
	"time"

	"github.com/3dl-dev/dontguess/pkg/exchange"
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
