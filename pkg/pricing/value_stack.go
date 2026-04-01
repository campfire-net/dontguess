// Package pricing — ValueStack wires the three pricing loops to the 4-layer
// value stack and enforces the Layer 0 correctness gate.
//
// The 4-layer value stack (from docs/heritage/value-function.md):
//
//	Layer 0  CORRECTNESS GATE    task_completion_rate       No loop owns this — validation only
//	Layer 1  TRANSACTION EFFICIENCY  tokens_saved / price    Fast loop target
//	Layer 2  VALUE COMPOSITE     completion + efficiency + recency + diversity   Medium loop gate
//	Layer 3  MARKET NOVELTY      buyer_count / competing_entries * discovery    Slow loop target
//	Layer 4  META                oscillation_frequency     Adapts slow loop step size
//
// The correctness gate (Layer 0) is the keystone: any loop tick whose output
// would regress task_completion_rate by more than CorrectnessRegressionTolerance
// is rejected — the loop's adjustments are NOT written to state.
//
// Sequencing:
//  1. Snapshot Layer 0 before any loop runs.
//  2. Run fast loop (Layer 1).
//  3. Check Layer 0 has not regressed; if regressed, undo fast loop adjustments.
//  4. Run medium loop (Layer 2).
//  5. Check Layer 0 has not regressed; if regressed, undo medium loop adjustments.
//  6. Run slow loop (Layer 3); Layer 4 meta (oscillation) is internal to slow loop.
//  7. Check Layer 0 has not regressed; if regressed, revert slow loop parameters.
//
// The undo mechanism for fast/medium loops works by restoring the pre-tick
// price adjustments snapshot (the loops write via SetPriceAdjustment; we capture
// the pre-tick state from AllPriceAdjustments and restore it on rejection).
// The slow loop writes MarketParameters; the stack captures pre-tick parameters
// and restores them on rejection.
package pricing

import (
	"context"
	"time"

	"github.com/3dl-dev/dontguess/pkg/exchange"
	"github.com/3dl-dev/dontguess/pkg/scrip"
)

// CorrectnessRegressionTolerance is the maximum allowed drop in
// task_completion_rate before a loop tick is rejected. A drop larger than this
// fraction (e.g., 0.02 = 2 percentage points) means the loop's adjustments are
// causing buyers to fail to complete transactions, which is the core correctness
// failure mode.
const CorrectnessRegressionTolerance = 0.02

// ValueStackStateReader is the read interface the stack needs from exchange state.
// The concrete *exchange.State satisfies this interface.
type ValueStackStateReader interface {
	// TaskCompletionRate returns the fraction of buyer-accepted orders that
	// have reached settle(complete) via inline (non-brokered) matching.
	// This is the pre-bootstrap Layer 0 correctness metric.
	TaskCompletionRate() float64
	// BrokeredMatchCompletionRate returns the fraction of brokered-match
	// accepted orders that have reached settle(complete). Used by the Layer 0
	// bootstrap gate after enough brokered completions have been observed.
	BrokeredMatchCompletionRate() float64
	// BrokeredCompletionCount returns the raw count of brokered-match
	// settle(complete) messages processed. Used by Layer0Metric to check
	// whether the bootstrap threshold has been crossed.
	BrokeredCompletionCount() int
	// CombinedCompletionRate returns a weighted average of TaskCompletionRate
	// and BrokeredMatchCompletionRate, weighted by each path's share of total
	// accepted orders. Used by Layer0Metric after the bootstrap threshold.
	CombinedCompletionRate() float64
	// AllPriceAdjustments returns a snapshot of all active price adjustments.
	// Used to capture pre-tick state for rollback.
	AllPriceAdjustments() map[string]exchange.PriceAdjustment
	// DebtorScore returns the debtor priority score for an agent in [0.0, 1.0].
	// 1.0 = no debt (full priority); 0.0 = maximum debt (lowest priority).
	// Returns 1.0 for agents with no recorded debt signal.
	DebtorScore(agentKey string) float64
}

// ValueStackStateWriter is the write interface for restoring adjustments.
type ValueStackStateWriter interface {
	// SetPriceAdjustment writes a dynamic price multiplier for an entry.
	SetPriceAdjustment(entryID string, adj exchange.PriceAdjustment)
}

// ValueStackStateReadWriter combines the read and write interfaces.
type ValueStackStateReadWriter interface {
	ValueStackStateReader
	ValueStackStateWriter
}

// StackLayer identifies which value stack layer is being evaluated.
type StackLayer int

const (
	Layer0Correctness StackLayer = iota
	Layer1FastLoop
	Layer2MediumLoop
	Layer3SlowLoop
	Layer4Meta
)

// LayerMetrics holds the computed metrics for all layers at a point in time.
type LayerMetrics struct {
	// Layer0: task completion rate (0.0 to 1.0; higher is better)
	TaskCompletionRate float64

	// Layer1: fast loop last tick result (nil if not run yet)
	FastLoopRan bool

	// Layer2: medium loop last tick result (nil if not run yet)
	MediumLoopRan bool

	// Layer3: slow loop last tick result (nil if not run yet)
	SlowLoopRan bool

	// Layer4: oscillation detected in slow loop last tick
	OscillationDetected bool

	// RejectedLayer is the layer whose output was rejected this cycle.
	// 0 if no rejection occurred. Uses the StackLayer constant values.
	RejectedLayer StackLayer
}

// StackRunResult is the outcome of a single RunAll cycle.
type StackRunResult struct {
	// Metrics is the final metric snapshot after the run.
	Metrics LayerMetrics
	// FastResult is the fast loop tick result. Zero-value if loop was rejected.
	FastResult interface{} // not typed to avoid coupling; informational only
	// MediumResult is the medium loop tick result.
	MediumResult *MediumLoopResult
	// SlowResult is the slow loop tick result. Nil if loop was rejected.
	SlowResult *SlowLoopResult
	// Layer0RejectedFast is true if the fast loop was rejected by Layer 0 gate.
	Layer0RejectedFast bool
	// Layer0RejectedMedium is true if the medium loop was rejected by Layer 0 gate.
	Layer0RejectedMedium bool
	// Layer0RejectedSlow is true if the slow loop was rejected by Layer 0 gate.
	Layer0RejectedSlow bool
}

// ValueStack orchestrates the three pricing loops and enforces the 4-layer
// value stack gating hierarchy.
type ValueStack struct {
	opts ValueStackOptions
}

// ValueStackOptions configures a ValueStack.
type ValueStackOptions struct {
	// State provides the exchange state for Layer 0 metric reads and adjustment
	// rollback operations. Required.
	State ValueStackStateReadWriter

	// FastLoop is the Layer 1 loop. Required.
	FastLoop *FastLoop

	// MediumLoop is the Layer 2 loop. Required.
	MediumLoop *MediumLoop

	// SlowLoop is the Layer 3 loop. Required.
	SlowLoop *SlowLoop

	// ParamsStore is where SlowLoop reads/writes MarketParameters.
	// Required when the slow loop is non-nil.
	ParamsStore SlowStateWriter

	// CorrectnessRegressionTolerance overrides the package-level constant.
	// Use 0 to use the default.
	CorrectnessRegressionTolerance float64

	// Logger receives diagnostic log lines. If nil, logs are suppressed.
	Logger func(format string, args ...any)
}

func (o *ValueStackOptions) regressionTolerance() float64 {
	if o.CorrectnessRegressionTolerance > 0 {
		return o.CorrectnessRegressionTolerance
	}
	return CorrectnessRegressionTolerance
}

func (o *ValueStackOptions) log(format string, args ...any) {
	if o.Logger != nil {
		o.Logger(format, args...)
	}
}

// NewValueStack creates a ValueStack with the given options.
func NewValueStack(opts ValueStackOptions) *ValueStack {
	return &ValueStack{opts: opts}
}

// NewValueStackFromState is a convenience constructor that builds all three
// loops from a shared exchange state and returns a ready-to-run ValueStack.
//
// This is the primary entry point for production use: callers provide the
// exchange state and scrip store, and get back a fully-wired stack that can
// be driven with RunAll.
func NewValueStackFromState(
	state interface {
		StateReadWriter
		MediumStateReadWriter
		SlowStateReadWriter
		ValueStackStateReadWriter
	},
	scripStore scrip.SpendingStore,
	paramsStore SlowStateWriter,
	logger func(format string, args ...any),
) *ValueStack {
	fast := NewFastLoop(FastLoopOptions{
		State:  state,
		Logger: logger,
	})
	medium := NewMediumLoop(MediumLoopOptions{
		State:      state,
		ScripStore: scripStore,
		Logger:     logger,
	})
	slow := NewSlowLoop(SlowLoopOptions{
		State:       state,
		ParamsStore: paramsStore,
		Logger:      logger,
	})
	return NewValueStack(ValueStackOptions{
		State:       state,
		FastLoop:    fast,
		MediumLoop:  medium,
		SlowLoop:    slow,
		ParamsStore: paramsStore,
		Logger:      logger,
	})
}

// Layer0Metric computes the current Layer 0 correctness metric.
//
// Before the brokered-match bootstrap threshold is reached, this returns
// TaskCompletionRate (inline matches only) — brokered matching is too new to
// include in the correctness gate without generating false rollbacks during the
// cold-start period.
//
// After the threshold (default DefaultBrokerMatchBootstrapThreshold brokered
// completions, configurable via MarketParameters.BrokerMatchBootstrapThreshold),
// this returns CombinedCompletionRate — a weighted average that holds both
// inline and brokered matching to the same correctness standard.
func (vs *ValueStack) Layer0Metric() float64 {
	threshold := DefaultBrokerMatchBootstrapThreshold
	if vs.opts.ParamsStore != nil {
		params := vs.opts.ParamsStore.GetMarketParameters()
		if params.BrokerMatchBootstrapThreshold > 0 {
			threshold = params.BrokerMatchBootstrapThreshold
		}
	}
	if vs.opts.State.BrokeredCompletionCount() < threshold {
		return vs.opts.State.TaskCompletionRate()
	}
	return vs.opts.State.CombinedCompletionRate()
}

// RunAll executes the three loops in sequence, enforcing the Layer 0 gate
// between each loop:
//
//  1. Snapshot Layer 0 baseline.
//  2. Run FastLoop.Tick() — Layer 1.
//  3. Re-measure Layer 0; if regressed > tolerance, roll back fast adjustments.
//  4. Run MediumLoop.Tick() — Layer 2.
//  5. Re-measure Layer 0; if regressed > tolerance, roll back medium adjustments.
//  6. Run SlowLoop.Tick() — Layer 3 + Layer 4 oscillation.
//  7. Re-measure Layer 0; if regressed > tolerance, revert slow parameters.
//
// Returns a StackRunResult with per-loop outcomes and rejection flags.
func (vs *ValueStack) RunAll(ctx context.Context) StackRunResult {
	result := StackRunResult{}

	// Snapshot Layer 0 baseline before any mutations.
	baseline := vs.Layer0Metric()
	vs.opts.log("value stack: Layer 0 baseline task_completion_rate=%.4f", baseline)

	tolerance := vs.opts.regressionTolerance()

	// --- Layer 1: Fast loop ---
	if vs.opts.FastLoop != nil {
		// Snapshot current price adjustments for rollback.
		preTickAdj := vs.opts.State.AllPriceAdjustments()

		vs.opts.FastLoop.Tick()

		// Check Layer 0 after fast loop.
		postFast := vs.Layer0Metric()
		if isRegression(baseline, postFast, tolerance) {
			vs.opts.log("value stack: Layer 0 regressed after fast loop (%.4f → %.4f), rolling back",
				baseline, postFast)
			vs.rollbackAdjustments(preTickAdj)
			result.Layer0RejectedFast = true
			result.Metrics.RejectedLayer = Layer1FastLoop
		} else {
			result.Metrics.FastLoopRan = true
		}
	}

	// --- Layer 2: Medium loop ---
	if vs.opts.MediumLoop != nil {
		// Snapshot current price adjustments for rollback.
		preTickAdj := vs.opts.State.AllPriceAdjustments()

		medResult := vs.opts.MediumLoop.Tick(ctx)

		// Check Layer 0 after medium loop.
		postMedium := vs.Layer0Metric()
		if isRegression(baseline, postMedium, tolerance) {
			vs.opts.log("value stack: Layer 0 regressed after medium loop (%.4f → %.4f), rolling back",
				baseline, postMedium)
			vs.rollbackAdjustments(preTickAdj)
			result.Layer0RejectedMedium = true
			if result.Metrics.RejectedLayer == 0 {
				result.Metrics.RejectedLayer = Layer2MediumLoop
			}
		} else {
			result.Metrics.MediumLoopRan = true
			result.MediumResult = &medResult
		}
	}

	// --- Layer 3 + 4: Slow loop (oscillation detection is internal) ---
	if vs.opts.SlowLoop != nil {
		// Snapshot current market parameters for rollback.
		var preTickParams MarketParameters
		if vs.opts.ParamsStore != nil {
			preTickParams = vs.opts.ParamsStore.GetMarketParameters()
		}

		slowResult := vs.opts.SlowLoop.Tick()

		// Check Layer 0 after slow loop.
		postSlow := vs.Layer0Metric()
		if isRegression(baseline, postSlow, tolerance) {
			vs.opts.log("value stack: Layer 0 regressed after slow loop (%.4f → %.4f), reverting parameters",
				baseline, postSlow)
			if vs.opts.ParamsStore != nil {
				vs.opts.ParamsStore.SetMarketParameters(preTickParams)
			}
			result.Layer0RejectedSlow = true
			if result.Metrics.RejectedLayer == 0 {
				result.Metrics.RejectedLayer = Layer3SlowLoop
			}
		} else {
			result.Metrics.SlowLoopRan = true
			result.Metrics.OscillationDetected = slowResult.OscillationDetected
			result.SlowResult = &slowResult
		}
	}

	// Final Layer 0 snapshot.
	result.Metrics.TaskCompletionRate = vs.Layer0Metric()
	vs.opts.log("value stack: RunAll complete, task_completion_rate=%.4f, rejected_fast=%v, rejected_medium=%v, rejected_slow=%v",
		result.Metrics.TaskCompletionRate,
		result.Layer0RejectedFast,
		result.Layer0RejectedMedium,
		result.Layer0RejectedSlow,
	)

	return result
}

// CorrectnessGateCheck evaluates whether a proposed rate regresses the baseline
// beyond the configured tolerance. Returns true if the change would be rejected.
// Exported for use in tests and diagnostics.
func (vs *ValueStack) CorrectnessGateCheck(baseline, proposed float64) bool {
	return isRegression(baseline, proposed, vs.opts.regressionTolerance())
}

// CurrentMetrics computes the current layer metrics without running any loops.
// Used for monitoring and diagnostics.
func (vs *ValueStack) CurrentMetrics() LayerMetrics {
	return LayerMetrics{
		TaskCompletionRate: vs.Layer0Metric(),
	}
}

// isRegression returns true when proposed drops below baseline by more than
// tolerance. Precision: drop = baseline - proposed > tolerance.
// If baseline == 0 (cold start), any proposed value is not a regression.
func isRegression(baseline, proposed, tolerance float64) bool {
	if baseline == 0 {
		return false
	}
	drop := baseline - proposed
	return drop > tolerance
}

// rollbackAdjustments restores price adjustments to the pre-tick snapshot.
// Entries that existed before the tick are restored; entries added by the tick
// that weren't in the snapshot are cleared by writing a zero-multiplier tombstone
// (the adjustment reader skips multiplier <= 0).
//
// Note: a zero-multiplier write signals "no adjustment" — the exchange
// computePrice function treats Multiplier <= 0 as 1.0x.
func (vs *ValueStack) rollbackAdjustments(preTickSnapshot map[string]exchange.PriceAdjustment) {
	// Get the current (post-tick) adjustments to find what was added.
	postTick := vs.opts.State.AllPriceAdjustments()

	// For every entry in the post-tick snapshot that wasn't in the pre-tick
	// snapshot, write a tombstone (zero multiplier) to clear the adjustment.
	for entryID := range postTick {
		if _, existed := preTickSnapshot[entryID]; !existed {
			vs.opts.State.SetPriceAdjustment(entryID, exchange.PriceAdjustment{
				Multiplier: 0, // treated as no adjustment by computePrice
				ExpiresAt:  time.Unix(0, 1), // immediately expired
			})
		}
	}

	// Restore pre-tick values for all entries that existed before the tick.
	for entryID, adj := range preTickSnapshot {
		vs.opts.State.SetPriceAdjustment(entryID, adj)
	}
}
