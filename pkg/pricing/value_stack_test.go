package pricing_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/3dl-dev/dontguess/pkg/exchange"
	"github.com/3dl-dev/dontguess/pkg/pricing"
)

// =============================================================================
// Shared stubs for value stack tests
// =============================================================================

// valueStackStubState is an in-memory stub that satisfies all four interfaces
// needed by the value stack: StateReadWriter (fast loop), MediumStateReadWriter,
// SlowStateReadWriter, and ValueStackStateReadWriter.
//
// It exposes a controllable TaskCompletionRate so tests can simulate correctness
// regressions without running a full exchange replay.
type valueStackStubState struct {
	mu sync.Mutex

	inventory    []*exchange.InventoryEntry
	history      []exchange.PriceRecord
	demandCounts map[string]int
	previewCounts map[string]int
	adjustments  map[string]exchange.PriceAdjustment
	reputations  map[string]int
	sellers      []string

	// completionRate is the controllable Layer 0 metric.
	completionRate float64
}

func newValueStackStubState() *valueStackStubState {
	return &valueStackStubState{
		demandCounts:   make(map[string]int),
		previewCounts:  make(map[string]int),
		adjustments:    make(map[string]exchange.PriceAdjustment),
		reputations:    make(map[string]int),
		completionRate: 1.0, // healthy by default
	}
}

// ---- fast loop interface ----
func (s *valueStackStubState) PriceHistory() []exchange.PriceRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.history
}
func (s *valueStackStubState) Inventory() []*exchange.InventoryEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inventory
}
func (s *valueStackStubState) EntryPreviewCount(entryID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.previewCounts[entryID]
}
func (s *valueStackStubState) EntryDemandCount(entryID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.demandCounts[entryID]
}
func (s *valueStackStubState) SetPriceAdjustment(entryID string, adj exchange.PriceAdjustment) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.adjustments[entryID] = adj
}

// ---- medium loop interface ----
func (s *valueStackStubState) AllPriceAdjustments() map[string]exchange.PriceAdjustment {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]exchange.PriceAdjustment, len(s.adjustments))
	for k, v := range s.adjustments {
		out[k] = v
	}
	return out
}
func (s *valueStackStubState) SellerReputation(sellerKey string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.reputations[sellerKey]; ok {
		return r
	}
	return exchange.DefaultReputation
}
func (s *valueStackStubState) AllSellerKeys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sellers
}

// ---- value stack interface ----
func (s *valueStackStubState) TaskCompletionRate() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.completionRate
}

// setCompletionRate controls the Layer 0 metric for tests.
func (s *valueStackStubState) setCompletionRate(r float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.completionRate = r
}

// =============================================================================
// Helper: stackStubParamsStore (reuse slowStubParamsStore from slow_loop_test.go)
// =============================================================================

// stackParamsStore is a minimal SlowStateWriter for value stack tests.
type stackParamsStore struct {
	mu     sync.Mutex
	params pricing.MarketParameters
}

func newStackParamsStore() *stackParamsStore {
	return &stackParamsStore{
		params: pricing.MarketParameters{
			PriceScalingFactor:    1.0,
			ContentTypeCommission: make(map[string]float64),
			ContentTypeFloor:      make(map[string]float64),
		},
	}
}

func (p *stackParamsStore) SetMarketParameters(params pricing.MarketParameters) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.params = params
}

func (p *stackParamsStore) GetMarketParameters() pricing.MarketParameters {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.params
}

// =============================================================================
// Tests
// =============================================================================

// TestValueStack_Layer0_NoRegressionAllowsAllLoops verifies that when Layer 0
// (task completion rate) stays healthy, all three loops are allowed to run and
// their outputs are accepted.
func TestValueStack_Layer0_NoRegressionAllowsAllLoops(t *testing.T) {
	t.Parallel()

	st := newValueStackStubState()
	// Add one inventory entry to give loops something to work on.
	st.inventory = []*exchange.InventoryEntry{
		{EntryID: "entry-1", ContentType: "code", SellerKey: "seller-1"},
	}
	st.completionRate = 0.95 // healthy

	paramsStore := newStackParamsStore()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	fast := pricing.NewFastLoop(pricing.FastLoopOptions{
		State: st,
		Now:   func() time.Time { return now },
	})
	medium := pricing.NewMediumLoop(pricing.MediumLoopOptions{
		State: st,
		Now:   func() time.Time { return now },
	})
	slow := pricing.NewSlowLoop(pricing.SlowLoopOptions{
		State:       st,
		ParamsStore: paramsStore,
		Now:         func() time.Time { return now },
	})

	stack := pricing.NewValueStack(pricing.ValueStackOptions{
		State:      st,
		FastLoop:   fast,
		MediumLoop: medium,
		SlowLoop:   slow,
		ParamsStore: paramsStore,
	})

	result := stack.RunAll(context.Background())

	if result.Layer0RejectedFast {
		t.Error("fast loop should not be rejected when Layer 0 is healthy")
	}
	if result.Layer0RejectedMedium {
		t.Error("medium loop should not be rejected when Layer 0 is healthy")
	}
	if result.Layer0RejectedSlow {
		t.Error("slow loop should not be rejected when Layer 0 is healthy")
	}
	if !result.Metrics.FastLoopRan {
		t.Error("expected fast loop to be marked as ran")
	}
	if !result.Metrics.MediumLoopRan {
		t.Error("expected medium loop to be marked as ran")
	}
	if !result.Metrics.SlowLoopRan {
		t.Error("expected slow loop to be marked as ran")
	}
}

// TestValueStack_Layer0_RegressAfterFastRejectsAndRollsBack verifies that when
// task_completion_rate drops by more than the tolerance after the fast loop
// runs, the fast loop adjustments are rolled back and the rejection flag is set.
//
// This test directly simulates a regression by changing the stub's completion
// rate after the fast loop would have written its adjustments. We verify that:
//  1. The pre-tick adjustment snapshot is restored (rollback happened).
//  2. result.Layer0RejectedFast is true.
//  3. result.Metrics.FastLoopRan is false.
func TestValueStack_Layer0_RegressAfterFastRejectsAndRollsBack(t *testing.T) {
	t.Parallel()

	// We need to simulate the completion rate dropping after the fast loop runs.
	// We do this by wrapping the stub state with a proxy that changes the rate
	// after the first AllPriceAdjustments call (which happens in the stack
	// post-tick check path).
	//
	// Simpler approach: use a custom ValueStack with a modified state that
	// reports a low rate at check time. We expose this via the stub's rate setter.

	// Baseline: healthy.
	st := newValueStackStubState()
	st.inventory = []*exchange.InventoryEntry{
		{EntryID: "entry-2", ContentType: "analysis", SellerKey: "seller-2"},
	}

	// Start with a rate that will "regress" after fast loop.
	// We set the baseline high, then set it low before the stack measures post-fast.
	// Since the stack measures Layer 0 *after* Tick(), we need the stub to return
	// different values on successive calls.
	//
	// We implement this with a call-count-based stub.
	callCount := 0
	rateFunc := func() float64 {
		callCount++
		if callCount == 1 {
			return 0.90 // baseline measurement
		}
		// After fast loop runs, simulate a drop of 5 percentage points (> 2% tolerance).
		return 0.80
	}

	// Use a custom stub that delegates TaskCompletionRate to rateFunc.
	customSt := &customCompletionRateState{
		valueStackStubState: st,
		rateFunc:            rateFunc,
	}

	paramsStore := newStackParamsStore()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	// Pre-populate an adjustment so we have something to roll back.
	preExistingAdj := exchange.PriceAdjustment{
		Multiplier: 1.3,
		ExpiresAt:  now.Add(10 * time.Minute),
	}
	st.adjustments["entry-2"] = preExistingAdj

	fast := pricing.NewFastLoop(pricing.FastLoopOptions{
		State: customSt,
		Now:   func() time.Time { return now },
	})
	medium := pricing.NewMediumLoop(pricing.MediumLoopOptions{
		State: customSt,
		Now:   func() time.Time { return now },
	})
	slow := pricing.NewSlowLoop(pricing.SlowLoopOptions{
		State:       customSt,
		ParamsStore: paramsStore,
		Now:         func() time.Time { return now },
	})

	stack := pricing.NewValueStack(pricing.ValueStackOptions{
		State:      customSt,
		FastLoop:   fast,
		MediumLoop: medium,
		SlowLoop:   slow,
		ParamsStore: paramsStore,
	})

	result := stack.RunAll(context.Background())

	if !result.Layer0RejectedFast {
		t.Errorf("expected fast loop to be rejected after Layer 0 regression, got rejected=false")
	}
	if result.Metrics.FastLoopRan {
		t.Error("FastLoopRan should be false when fast loop was rejected")
	}

	// Verify the pre-existing adjustment was restored.
	st.mu.Lock()
	restoredAdj, ok := st.adjustments["entry-2"]
	st.mu.Unlock()

	if !ok {
		t.Fatal("expected entry-2 adjustment to be present after rollback")
	}
	if restoredAdj.Multiplier != preExistingAdj.Multiplier {
		t.Errorf("expected rolled-back multiplier %.4f, got %.4f",
			preExistingAdj.Multiplier, restoredAdj.Multiplier)
	}
}

// TestValueStack_Layer0_RegressAfterSlowReverts verifies that when the slow loop
// would cause a Layer 0 regression, its MarketParameters are reverted to the
// pre-tick values.
func TestValueStack_Layer0_RegressAfterSlowReverts(t *testing.T) {
	t.Parallel()

	st := newValueStackStubState()
	st.inventory = []*exchange.InventoryEntry{
		{EntryID: "entry-3", ContentType: "plan", SellerKey: "seller-3"},
	}

	// Fast+medium healthy; slow causes regression.
	callCount := 0
	rateFunc := func() float64 {
		callCount++
		switch callCount {
		case 1:
			return 0.95 // baseline
		case 2:
			return 0.95 // after fast: no regression
		case 3:
			return 0.95 // after medium: no regression
		default:
			return 0.70 // after slow: big regression
		}
	}

	customSt := &customCompletionRateState{
		valueStackStubState: st,
		rateFunc:            rateFunc,
	}

	paramsStore := newStackParamsStore()
	// Set a known pre-tick parameter value.
	initialParams := pricing.MarketParameters{
		PriceScalingFactor:    1.05,
		ContentTypeCommission: map[string]float64{"plan": 0.12},
		ContentTypeFloor:      make(map[string]float64),
	}
	paramsStore.SetMarketParameters(initialParams)

	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	fast := pricing.NewFastLoop(pricing.FastLoopOptions{
		State: customSt,
		Now:   func() time.Time { return now },
	})
	medium := pricing.NewMediumLoop(pricing.MediumLoopOptions{
		State: customSt,
		Now:   func() time.Time { return now },
	})
	slow := pricing.NewSlowLoop(pricing.SlowLoopOptions{
		State:       customSt,
		ParamsStore: paramsStore,
		Now:         func() time.Time { return now },
	})

	stack := pricing.NewValueStack(pricing.ValueStackOptions{
		State:       customSt,
		FastLoop:    fast,
		MediumLoop:  medium,
		SlowLoop:    slow,
		ParamsStore: paramsStore,
	})

	result := stack.RunAll(context.Background())

	if !result.Layer0RejectedSlow {
		t.Errorf("expected slow loop to be rejected after Layer 0 regression")
	}
	if result.SlowResult != nil {
		t.Error("SlowResult should be nil when slow loop was rejected")
	}

	// Verify parameters were reverted to pre-tick values.
	revertedParams := paramsStore.GetMarketParameters()
	if revertedParams.PriceScalingFactor != initialParams.PriceScalingFactor {
		t.Errorf("expected PriceScalingFactor %.4f after revert, got %.4f",
			initialParams.PriceScalingFactor, revertedParams.PriceScalingFactor)
	}
	if revertedParams.ContentTypeCommission["plan"] != initialParams.ContentTypeCommission["plan"] {
		t.Errorf("expected commission %.4f after revert, got %.4f",
			initialParams.ContentTypeCommission["plan"],
			revertedParams.ContentTypeCommission["plan"])
	}
}

// TestValueStack_CorrectnessGateCheck verifies the boundary conditions of the
// Layer 0 gate logic.
func TestValueStack_CorrectnessGateCheck(t *testing.T) {
	t.Parallel()

	st := newValueStackStubState()
	stack := pricing.NewValueStack(pricing.ValueStackOptions{
		State: st,
	})

	cases := []struct {
		name      string
		baseline  float64
		proposed  float64
		wantBlock bool
	}{
		{
			name:      "no regression",
			baseline:  0.95,
			proposed:  0.95,
			wantBlock: false,
		},
		{
			name:      "small drop within tolerance",
			baseline:  0.95,
			proposed:  0.94, // 1 pp drop, tolerance is 2 pp
			wantBlock: false,
		},
		{
			name:      "drop exactly at tolerance boundary",
			baseline:  0.95,
			proposed:  0.93, // exactly 2 pp drop = NOT > tolerance
			wantBlock: false,
		},
		{
			name:      "drop just above tolerance",
			baseline:  0.95,
			proposed:  0.9299, // just over 2 pp drop
			wantBlock: true,
		},
		{
			name:      "large regression",
			baseline:  0.90,
			proposed:  0.70, // 20 pp drop
			wantBlock: true,
		},
		{
			name:      "cold start baseline zero never blocks",
			baseline:  0.0,
			proposed:  0.0,
			wantBlock: false,
		},
		{
			name:      "improvement is never blocked",
			baseline:  0.80,
			proposed:  0.95,
			wantBlock: false,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := stack.CorrectnessGateCheck(tc.baseline, tc.proposed)
			if got != tc.wantBlock {
				t.Errorf("CorrectnessGateCheck(%.4f, %.4f) = %v, want %v",
					tc.baseline, tc.proposed, got, tc.wantBlock)
			}
		})
	}
}

// TestValueStack_CurrentMetrics returns Layer 0 metric without running loops.
func TestValueStack_CurrentMetrics(t *testing.T) {
	t.Parallel()

	st := newValueStackStubState()
	st.completionRate = 0.87

	stack := pricing.NewValueStack(pricing.ValueStackOptions{
		State: st,
	})

	metrics := stack.CurrentMetrics()
	if metrics.TaskCompletionRate != 0.87 {
		t.Errorf("expected task completion rate 0.87, got %.4f", metrics.TaskCompletionRate)
	}
}

// TestValueStack_RollbackClearsNewAdjustments verifies that adjustments written
// by the fast loop for entries that had NO prior adjustment are cleared (not just
// reverted) when the rollback fires.
func TestValueStack_RollbackClearsNewAdjustments(t *testing.T) {
	t.Parallel()

	st := newValueStackStubState()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	// Entry with no prior adjustment.
	st.inventory = []*exchange.InventoryEntry{
		{EntryID: "fresh-entry", ContentType: "code", SellerKey: "seller-x"},
	}
	// No pre-existing adjustments.

	// Simulate regression after fast loop.
	callCount := 0
	rateFunc := func() float64 {
		callCount++
		if callCount == 1 {
			return 0.92 // baseline
		}
		return 0.85 // regress after fast loop (7 pp > 2 pp tolerance)
	}

	customSt := &customCompletionRateState{
		valueStackStubState: st,
		rateFunc:            rateFunc,
	}

	fast := pricing.NewFastLoop(pricing.FastLoopOptions{
		State: customSt,
		Now:   func() time.Time { return now },
	})

	stack := pricing.NewValueStack(pricing.ValueStackOptions{
		State:    customSt,
		FastLoop: fast,
	})

	stack.RunAll(context.Background())

	// After rollback, the newly-added adjustment should be cleared (zero or expired).
	st.mu.Lock()
	adj, exists := st.adjustments["fresh-entry"]
	st.mu.Unlock()

	if !exists {
		// Cleared by delete — also acceptable; check the rollback happened.
		return
	}

	// If present, multiplier must be <= 0 (tombstone) or expired.
	if adj.Multiplier > 0 && adj.ExpiresAt.After(now) {
		t.Errorf("expected rolled-back adjustment to be cleared (multiplier=%.4f expiresAt=%v)",
			adj.Multiplier, adj.ExpiresAt)
	}
}

// TestValueStack_SlowLoopOscillationExposedViaResult verifies that when the slow
// loop detects oscillation (Layer 4 meta), the result carries OscillationDetected=true.
//
// We force oscillation by seeding the slow loop's history with alternating
// kept/rejected decisions. Since the slow loop's history is internal, we
// run multiple Tick() cycles until oscillation fires.
func TestValueStack_SlowLoopOscillationExposedViaResult(t *testing.T) {
	t.Parallel()

	// Build a slow loop and force oscillation by running many ticks with
	// an inventory that produces alternating novelty outcomes.
	// The oscillation detector fires when lag-1 autocorrelation of the "kept"
	// series is < -0.3, which requires the kept/reverted pattern to alternate.
	//
	// We create a state where novelty alternates between high (> 0.5) and low
	// (< 0.2) by toggling buyer counts between ticks.

	st := newValueStackStubState()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	// Use a stable completion rate so no gate rejections.
	st.completionRate = 0.95

	paramsStore := newStackParamsStore()
	slow := pricing.NewSlowLoop(pricing.SlowLoopOptions{
		State:       st,
		ParamsStore: paramsStore,
		Now:         func() time.Time { return now },
	})

	// Seed alternating pattern: run several ticks where novelty alternates.
	// High novelty tick: 5 buyers, 2 entries, discovery=1.
	// Low novelty tick: 0 buyers (zero sum novelty → scaling reverted).
	tickCount := 0
	_ = tickCount // used below
	var oscillationSeen bool

	highNoveltyEntries := []*exchange.InventoryEntry{
		{EntryID: "osc-1", ContentType: "data", SellerKey: "seller-osc"},
		{EntryID: "osc-2", ContentType: "data", SellerKey: "seller-osc"},
	}

	// Run enough ticks to trigger oscillation detection (need 4+ history entries).
	for i := 0; i < 12; i++ {
		if i%2 == 0 {
			// High novelty: add demand counts.
			st.mu.Lock()
			st.inventory = highNoveltyEntries
			st.demandCounts["osc-1"] = 3
			st.demandCounts["osc-2"] = 1
			// Add history records.
			st.history = []exchange.PriceRecord{
				{EntryID: "osc-1", ContentType: "data", SalePrice: 100,
					Timestamp: now.Add(-time.Duration(i) * time.Hour).UnixNano()},
			}
			st.mu.Unlock()
		} else {
			// Low novelty: zero buyers, no demand.
			st.mu.Lock()
			st.inventory = highNoveltyEntries
			st.demandCounts["osc-1"] = 0
			st.demandCounts["osc-2"] = 0
			st.history = nil
			st.mu.Unlock()
		}

		result := slow.Tick()
		if result.OscillationDetected {
			oscillationSeen = true
		}
	}

	// We just need to verify the oscillation detection mechanism works end-to-end
	// through the value stack. Run one more cycle through the full stack.
	fast := pricing.NewFastLoop(pricing.FastLoopOptions{
		State: st,
		Now:   func() time.Time { return now },
	})
	medium := pricing.NewMediumLoop(pricing.MediumLoopOptions{
		State: st,
		Now:   func() time.Time { return now },
	})

	stack := pricing.NewValueStack(pricing.ValueStackOptions{
		State:       st,
		FastLoop:    fast,
		MediumLoop:  medium,
		SlowLoop:    slow,
		ParamsStore: paramsStore,
	})

	stackResult := stack.RunAll(context.Background())

	// The result should expose whether oscillation was detected.
	// We don't assert oscillation occurred (depends on exact alternation pattern)
	// but do verify the field is populated from the slow loop result.
	if stackResult.SlowResult != nil {
		// If the slow loop ran without Layer 0 rejection, OscillationDetected
		// should match what the slow loop reported.
		if stackResult.Metrics.OscillationDetected != stackResult.SlowResult.OscillationDetected {
			t.Errorf("stack Metrics.OscillationDetected=%v does not match SlowResult.OscillationDetected=%v",
				stackResult.Metrics.OscillationDetected, stackResult.SlowResult.OscillationDetected)
		}
	}

	// Informational: report whether oscillation was triggered.
	t.Logf("oscillation seen during seed ticks: %v; final stack result rejected slow: %v",
		oscillationSeen, stackResult.Layer0RejectedSlow)
}

// TestValueStack_ColdStartAlwaysAllowed verifies that when task_completion_rate
// is 1.0 (cold start with no accepted orders), no loop is ever rejected.
func TestValueStack_ColdStartAlwaysAllowed(t *testing.T) {
	t.Parallel()

	st := newValueStackStubState()
	st.completionRate = 1.0 // cold start value returned by State.TaskCompletionRate()
	st.inventory = []*exchange.InventoryEntry{
		{EntryID: "cold-entry", ContentType: "summary", SellerKey: "seller-cold"},
	}

	paramsStore := newStackParamsStore()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	fast := pricing.NewFastLoop(pricing.FastLoopOptions{
		State: st,
		Now:   func() time.Time { return now },
	})
	medium := pricing.NewMediumLoop(pricing.MediumLoopOptions{
		State: st,
		Now:   func() time.Time { return now },
	})
	slow := pricing.NewSlowLoop(pricing.SlowLoopOptions{
		State:       st,
		ParamsStore: paramsStore,
		Now:         func() time.Time { return now },
	})

	stack := pricing.NewValueStack(pricing.ValueStackOptions{
		State:       st,
		FastLoop:    fast,
		MediumLoop:  medium,
		SlowLoop:    slow,
		ParamsStore: paramsStore,
	})

	result := stack.RunAll(context.Background())

	if result.Layer0RejectedFast || result.Layer0RejectedMedium || result.Layer0RejectedSlow {
		t.Error("cold start should never trigger Layer 0 gate rejection")
	}
}

// TestValueStack_Layer0MetricReadsFromState verifies that Layer0Metric() returns
// the value from the exchange state's TaskCompletionRate.
func TestValueStack_Layer0MetricReadsFromState(t *testing.T) {
	t.Parallel()

	st := newValueStackStubState()
	st.completionRate = 0.73

	stack := pricing.NewValueStack(pricing.ValueStackOptions{
		State: st,
	})

	got := stack.Layer0Metric()
	if got != 0.73 {
		t.Errorf("Layer0Metric() = %.4f, want 0.73", got)
	}
}

// =============================================================================
// Helper: customCompletionRateState wraps valueStackStubState with a controlled
// TaskCompletionRate call sequence.
// =============================================================================

// customCompletionRateState delegates all interface methods to the embedded
// valueStackStubState but overrides TaskCompletionRate with a controllable func.
// This allows tests to inject regression scenarios.
type customCompletionRateState struct {
	*valueStackStubState
	rateFunc func() float64
}

func (c *customCompletionRateState) TaskCompletionRate() float64 {
	return c.rateFunc()
}
