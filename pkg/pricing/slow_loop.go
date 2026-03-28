// Package pricing — SlowLoop (4 hr cadence, Layer 3 target).
//
// The slow loop runs every 4 hours and performs three structural optimizations:
//
//  1. Historical price/volume analysis — computes per-content-type price trends
//     and volume trends over a long lookback window. Identifies content types
//     that are systematically over- or under-priced relative to market efficiency.
//
//  2. Market parameter optimization — adjusts the global price scaling factor
//     and per-content-type floor multipliers based on the Layer 3 novelty metric:
//     buyer_count / competing_entries * discovery. This rewards content types
//     where new sellers are entering a busy market (high novelty), and discounts
//     types that are oversaturated.
//
//  3. Commission adjustments — recomputes the optimal commission rate per
//     content type based on long-run market efficiency. Types with high velocity
//     and high buyer diversity warrant lower commissions (encourage liquidity);
//     types with stagnant inventory warrant higher commissions (incentivise entry
//     by new sellers through lower exchange take).
//
// Layer 3 novelty metric (from CLAUDE.md / docs/heritage/value-function.md):
//
//	novelty = buyer_count / competing_entries * discovery
//
// Where:
//   - buyer_count = distinct buyers for a content type in the analysis window
//   - competing_entries = number of live inventory entries for the type
//   - discovery = fraction of buyers who purchased an entry they had not seen before
//     (proxy: entries with only one buyer, relative to all sales in the type)
//
// The slow loop also implements oscillation detection (Layer 4 meta): if the
// parameter series is alternating accept/reject for 3+ cycles, the step size
// is halved to prevent fitting noise.
//
// Layer 0 gate (correctness): any proposed parameter change that would increase
// the estimated conversion-rate regression (approximated as commission_rate > 0.5
// or floor_multiplier < MinMultiplier) is rejected before being applied.
package pricing

import (
	"context"
	"math"
	"sort"
	"time"

	"github.com/3dl-dev/dontguess/pkg/exchange"
)

// DefaultSlowLoopInterval is the cadence at which the slow loop runs.
const DefaultSlowLoopInterval = 4 * time.Hour

// DefaultSlowLoopWindow is the lookback window for historical price/volume
// analysis. Covers the last 24 hours to capture a full market cycle.
const DefaultSlowLoopWindow = 24 * time.Hour

// DefaultPriceScalingStep is the default step size for price scaling factor
// adjustments per tick. Small enough to avoid oscillation at startup.
const DefaultPriceScalingStep = 0.025

// MinPriceScalingStep is the floor for the adaptive step size. The oscillation
// detector will halve the step size but not below this floor.
const MinPriceScalingStep = 0.005

// MaxPriceScalingStep is the ceiling for the adaptive step size.
const MaxPriceScalingStep = 0.10

// DefaultCommissionRate is the starting commission rate (fraction of sale price
// the exchange retains). The slow loop adjusts this based on market efficiency.
const DefaultCommissionRate = 0.15

// MinCommissionRate is the minimum allowed commission rate.
const MinCommissionRate = 0.05

// MaxCommissionRate is the maximum allowed commission rate.
const MaxCommissionRate = 0.40

// DefaultFloorMultiplier is the starting floor multiplier for content types with
// low novelty. Applied additively with the fast loop's demand adjustments.
const DefaultFloorMultiplier = 1.0

// OscillationWindow is the number of recent parameter history entries examined
// for oscillation detection (lag-1 autocorrelation of the "kept" series).
const OscillationWindow = 10

// OscillationThreshold is the lag-1 autocorrelation below which the slow loop
// is considered to be fitting noise. Below this, step size is halved.
const OscillationThreshold = -0.3

// MarketParameters holds the current market-level parameters written by the
// slow loop. The exchange engine reads these to apply structural adjustments
// above and beyond the fast and medium loop corrections.
type MarketParameters struct {
	// PriceScalingFactor is a global multiplier applied to all entry base prices.
	// 1.0 = no change, > 1.0 = market-wide price increase, < 1.0 = decrease.
	PriceScalingFactor float64

	// ContentTypeCommission maps content type → commission rate [0,1].
	// The exchange retains this fraction of each sale price.
	// Types not in this map use DefaultCommissionRate.
	ContentTypeCommission map[string]float64

	// ContentTypeFloor maps content type → floor multiplier for that type.
	// Entries of this type will not be priced below this relative floor.
	// Types not in this map use DefaultFloorMultiplier.
	ContentTypeFloor map[string]float64

	// UpdatedAt is when these parameters were last written.
	UpdatedAt time.Time
}

// NoveltyScore is the computed Layer 3 novelty metric for a single content type.
// Exported for use in tests and diagnostics.
type NoveltyScore struct {
	ContentType      string
	BuyerCount       int
	CompetingEntries int
	DiscoveryRate    float64
	Score            float64 // buyer_count / competing_entries * discovery_rate
}

// SlowLoopResult summarises the outcome of a single slow loop tick.
// Exported for use in tests and diagnostics.
type SlowLoopResult struct {
	// ContentTypesAnalysed is the number of content types in the analysis window.
	ContentTypesAnalysed int
	// NoveltyScores holds the computed novelty score per content type.
	NoveltyScores []NoveltyScore
	// CommissionAdjustments is the number of content types with adjusted commission rates.
	CommissionAdjustments int
	// FloorAdjustments is the number of content types with adjusted floor multipliers.
	FloorAdjustments int
	// ScalingFactorDelta is the change applied to PriceScalingFactor this tick.
	ScalingFactorDelta float64
	// OscillationDetected is true if the oscillation detector fired and halved
	// the step size.
	OscillationDetected bool
	// StepSize is the adaptive step size used this tick.
	StepSize float64
}

// SlowStateReader is the read interface the slow loop needs from exchange state.
type SlowStateReader interface {
	// Inventory returns all live inventory entries.
	Inventory() []*exchange.InventoryEntry
	// PriceHistory returns all recorded settlement price events.
	PriceHistory() []exchange.PriceRecord
	// EntryDemandCount returns the number of distinct completed buyers for an entry.
	EntryDemandCount(entryID string) int
}

// SlowStateWriter is the write interface the slow loop needs.
// We write market parameters via an explicit callback to avoid coupling
// the slow loop to the exchange.State struct directly.
type SlowStateWriter interface {
	// SetMarketParameters writes the updated market parameters.
	SetMarketParameters(params MarketParameters)
	// GetMarketParameters returns the current market parameters.
	GetMarketParameters() MarketParameters
}

// SlowStateReadWriter combines the read and write interfaces.
// The concrete *exchange.State does NOT implement SlowStateWriter yet —
// the slow loop writes to a separate MarketParameters store that is passed
// in as part of SlowLoopOptions. This avoids a circular dependency.
type SlowStateReadWriter interface {
	SlowStateReader
}

// SlowLoopOptions configures a SlowLoop.
type SlowLoopOptions struct {
	// State is the exchange state used to read market history.
	State SlowStateReadWriter
	// ParamsStore is where the slow loop reads and writes MarketParameters.
	// If nil, parameter changes are computed but not persisted (useful in tests
	// that only verify the analysis logic).
	ParamsStore SlowStateWriter
	// Interval is how often the loop runs. Defaults to DefaultSlowLoopInterval.
	Interval time.Duration
	// Window is the lookback window for price/volume history.
	// Defaults to DefaultSlowLoopWindow.
	Window time.Duration
	// InitialStep is the starting adaptive step size for parameter adjustments.
	// Defaults to DefaultPriceScalingStep.
	InitialStep float64
	// Logger receives diagnostic log lines. If nil, logs are suppressed.
	Logger func(format string, args ...any)
	// Now overrides time.Now for testing determinism.
	Now func() time.Time
}

func (o *SlowLoopOptions) interval() time.Duration {
	if o.Interval > 0 {
		return o.Interval
	}
	return DefaultSlowLoopInterval
}

func (o *SlowLoopOptions) window() time.Duration {
	if o.Window > 0 {
		return o.Window
	}
	return DefaultSlowLoopWindow
}

func (o *SlowLoopOptions) initialStep() float64 {
	if o.InitialStep > 0 {
		return o.InitialStep
	}
	return DefaultPriceScalingStep
}

func (o *SlowLoopOptions) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func (o *SlowLoopOptions) log(format string, args ...any) {
	if o.Logger != nil {
		o.Logger(format, args...)
	}
}

// paramHistory tracks whether recent parameter changes were "kept" (improved
// novelty) or reverted. Used for oscillation detection (Layer 4).
type paramHistory struct {
	kept     bool
	stepSize float64
}

// SlowLoop runs the Layer 3 structural optimization loop. It analyses historical
// price/volume data and adjusts market parameters (commission rates, price
// scaling, content type floors) at a 4-hour cadence.
type SlowLoop struct {
	opts SlowLoopOptions

	// currentStep is the adaptive step size for parameter adjustments.
	// Halved when oscillation is detected; doubled when consistently improving.
	currentStep float64

	// history tracks recent parameter decisions for oscillation detection.
	// The last OscillationWindow entries are used for lag-1 autocorrelation.
	history []paramHistory

	// lastNoveltyByType holds the previous tick's novelty scores for comparison.
	// Used to determine whether a parameter change improved or regressed novelty.
	lastNoveltyByType map[string]float64
}

// NewSlowLoop creates a SlowLoop with the given options.
func NewSlowLoop(opts SlowLoopOptions) *SlowLoop {
	return &SlowLoop{
		opts:              opts,
		currentStep:       opts.initialStep(),
		history:           make([]paramHistory, 0, OscillationWindow),
		lastNoveltyByType: make(map[string]float64),
	}
}

// Run starts the slow loop. It blocks until ctx is cancelled, running Tick on
// each interval. The first tick fires immediately on startup.
func (l *SlowLoop) Run(ctx context.Context) error {
	l.Tick()

	ticker := time.NewTicker(l.opts.interval())
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			l.Tick()
		}
	}
}

// Tick performs a single slow-loop computation cycle:
//  1. Analyses historical price/volume data in the lookback window
//  2. Computes Layer 3 novelty scores per content type
//  3. Adjusts commission rates, price floors, and global scaling factor
//  4. Detects oscillation and adapts the step size (Layer 4 meta)
//
// Tick is exported so it can be called directly in tests without running the
// full Run loop.
func (l *SlowLoop) Tick() SlowLoopResult {
	now := l.opts.now()
	window := l.opts.window()
	cutoff := now.Add(-window)

	// --- Step 1: Slice price history to the analysis window ---
	history := l.opts.State.PriceHistory()
	recent := make([]exchange.PriceRecord, 0, len(history))
	for _, rec := range history {
		if time.Unix(0, rec.Timestamp).After(cutoff) {
			recent = append(recent, rec)
		}
	}

	// --- Step 2: Compute per-content-type statistics ---
	inventory := l.opts.State.Inventory()
	typeStats := l.computeTypeStats(recent, inventory)

	// --- Step 3: Compute Layer 3 novelty scores ---
	noveltyScores := l.computeNoveltyScores(typeStats, inventory)

	// --- Step 4: Oscillation detection (Layer 4 meta) ---
	oscillationDetected := l.detectOscillation()
	if oscillationDetected {
		l.currentStep = math.Max(l.currentStep/2.0, MinPriceScalingStep)
		l.opts.log("slow loop: oscillation detected, halving step to %.4f", l.currentStep)
	}

	// --- Step 5: Read current parameters (or create defaults) ---
	var current MarketParameters
	if l.opts.ParamsStore != nil {
		current = l.opts.ParamsStore.GetMarketParameters()
	}
	if current.PriceScalingFactor == 0 {
		current.PriceScalingFactor = 1.0
	}
	if current.ContentTypeCommission == nil {
		current.ContentTypeCommission = make(map[string]float64)
	}
	if current.ContentTypeFloor == nil {
		current.ContentTypeFloor = make(map[string]float64)
	}

	// --- Step 6: Compute parameter updates ---
	proposed := l.computeParameterUpdates(current, noveltyScores, typeStats)

	// --- Step 7: Layer 0 gate — reject changes that breach correctness bounds ---
	proposed = l.applyCorrectnessGate(proposed)

	// --- Step 8: Compute novelty delta to determine if global scaling is "kept" ---
	// Commission and floor adjustments are per-type revenue/incentive decisions
	// that are always applied (they encode known market structure). Only the
	// global price scaling factor is treated as an experiment subject to the
	// kept/revert decision, since it affects all entries and can regress novelty
	// if the market isn't healthy enough to support higher prices.
	totalNoveltyCurrent := sumRawNovelty(noveltyScores)
	totalNoveltyProposed := totalNoveltyCurrent // same scores; only scaling changes are gated

	// The scaling factor change is "kept" if raw novelty is non-zero (market is active).
	// An inactive market (zero novelty) reverts to 1.0 scaling to avoid deflation.
	kept := totalNoveltyProposed > 0 || proposed.PriceScalingFactor == current.PriceScalingFactor

	if !kept {
		// Revert global scaling — the market is inactive.
		proposed.PriceScalingFactor = current.PriceScalingFactor
		l.opts.log("slow loop: scaling factor reverted (zero novelty market: %.4f -> %.4f)",
			totalNoveltyCurrent, totalNoveltyProposed)
	}

	// Record the outcome for oscillation detection.
	l.recordHistory(kept)

	// --- Step 9: Write parameters ---
	proposed.UpdatedAt = now
	if l.opts.ParamsStore != nil {
		l.opts.ParamsStore.SetMarketParameters(proposed)
	}

	// --- Step 10: Update lastNoveltyByType for next tick ---
	for _, ns := range noveltyScores {
		l.lastNoveltyByType[ns.ContentType] = ns.Score
	}

	// Compute result metrics.
	commAdj := 0
	floorAdj := 0
	for _, ns := range noveltyScores {
		if proposed.ContentTypeCommission[ns.ContentType] != current.ContentTypeCommission[ns.ContentType] {
			commAdj++
		}
		if proposed.ContentTypeFloor[ns.ContentType] != current.ContentTypeFloor[ns.ContentType] {
			floorAdj++
		}
	}
	scalingDelta := proposed.PriceScalingFactor - current.PriceScalingFactor

	result := SlowLoopResult{
		ContentTypesAnalysed:  len(typeStats),
		NoveltyScores:         noveltyScores,
		CommissionAdjustments: commAdj,
		FloorAdjustments:      floorAdj,
		ScalingFactorDelta:    scalingDelta,
		OscillationDetected:   oscillationDetected,
		StepSize:              l.currentStep,
	}

	l.opts.log("slow loop tick: window=%s, history_records=%d, types=%d, novelty=%.4f, step=%.4f, kept=%v, oscillation=%v",
		window, len(recent), len(typeStats), totalNoveltyProposed, l.currentStep, kept, oscillationDetected)

	return result
}

// contentTypeStats holds derived per-content-type signals for the analysis window.
type contentTypeStats struct {
	// TotalSales is the number of sales in the window for this type.
	TotalSales int
	// TotalRevenue is the sum of SalePrice for all sales in the window.
	TotalRevenue int64
	// DistinctBuyers is the count of unique buyers (by entry demand counts).
	DistinctBuyers int
	// AvgPrice is the mean sale price in the window. 0 if no sales.
	AvgPrice float64
	// PriceTrend is the slope of the price-over-time series (positive = rising).
	PriceTrend float64
	// EntryCount is the number of live inventory entries for this type.
	EntryCount int
	// SingleBuyerEntries is the number of entries with exactly one distinct buyer.
	// Used for discovery rate computation.
	SingleBuyerEntries int
}

// computeTypeStats groups recent price history and inventory by content type
// and computes per-type statistics used for novelty scoring and parameter tuning.
func (l *SlowLoop) computeTypeStats(
	recent []exchange.PriceRecord,
	inventory []*exchange.InventoryEntry,
) map[string]*contentTypeStats {
	stats := make(map[string]*contentTypeStats)

	// Build entry → content type map and count live entries per type.
	entryType := make(map[string]string, len(inventory))
	for _, e := range inventory {
		ct := e.ContentType
		if ct == "" {
			ct = "other"
		}
		entryType[e.EntryID] = ct
		if _, ok := stats[ct]; !ok {
			stats[ct] = &contentTypeStats{}
		}
		stats[ct].EntryCount++
	}

	// Accumulate sales into per-type stats.
	type salePoint struct {
		price     int64
		timestamp int64
	}
	typeTimeSeries := make(map[string][]salePoint)

	// Distinct buyer tracking: collect demand counts per entry per type.
	entryBuyers := make(map[string]int, len(inventory))
	for _, e := range inventory {
		entryBuyers[e.EntryID] = l.opts.State.EntryDemandCount(e.EntryID)
	}

	for _, rec := range recent {
		ct := entryType[rec.EntryID]
		if ct == "" {
			ct = rec.ContentType
		}
		if ct == "" {
			ct = "other"
		}
		if _, ok := stats[ct]; !ok {
			stats[ct] = &contentTypeStats{}
		}
		s := stats[ct]
		s.TotalSales++
		s.TotalRevenue += rec.SalePrice
		typeTimeSeries[ct] = append(typeTimeSeries[ct], salePoint{rec.SalePrice, rec.Timestamp})
	}

	// Compute avg price and price trend per type.
	for ct, ts := range typeTimeSeries {
		s := stats[ct]
		if len(ts) == 0 {
			continue
		}

		var totalPrice int64
		for _, p := range ts {
			totalPrice += p.price
		}
		s.AvgPrice = float64(totalPrice) / float64(len(ts))

		// Compute price trend (linear regression slope) if we have >= 2 data points.
		// Slope > 0 = rising prices, slope < 0 = falling prices.
		if len(ts) >= 2 {
			prices := make([]int64, len(ts))
			timestamps := make([]int64, len(ts))
			for i, p := range ts {
				prices[i] = p.price
				timestamps[i] = p.timestamp
			}
			s.PriceTrend = computePriceTrend(prices, timestamps)
		}
	}

	// Compute distinct buyers and single-buyer entries per type.
	for _, e := range inventory {
		ct := e.ContentType
		if ct == "" {
			ct = "other"
		}
		s, ok := stats[ct]
		if !ok {
			continue
		}
		buyers := entryBuyers[e.EntryID]
		s.DistinctBuyers += buyers
		if buyers == 1 {
			s.SingleBuyerEntries++
		}
	}

	return stats
}

// computeNoveltyScores derives the Layer 3 novelty score per content type.
// Score = buyer_count / competing_entries * discovery_rate
func (l *SlowLoop) computeNoveltyScores(
	stats map[string]*contentTypeStats,
	inventory []*exchange.InventoryEntry,
) []NoveltyScore {
	_ = inventory // stats already has entry count
	scores := make([]NoveltyScore, 0, len(stats))

	for ct, s := range stats {
		if s.EntryCount == 0 {
			continue
		}

		// discovery_rate: fraction of entries that have been discovered by at least
		// one buyer. An entry with 1 buyer was discovered fresh (new buyer, new content).
		// Entries with 0 buyers haven't been discovered yet.
		discoveryRate := 0.0
		if s.DistinctBuyers > 0 {
			// Use single-buyer entries as a proxy for "freshly discovered" content.
			// High proportion of single-buyer entries = high discovery (buyers are
			// finding new content, not just re-buying the same popular entries).
			discoveryRate = float64(s.SingleBuyerEntries+1) / float64(s.EntryCount+1)
		} else {
			// No buyers at all → zero discovery.
			discoveryRate = 0.0
		}

		// novelty = buyer_count / competing_entries * discovery_rate
		score := float64(s.DistinctBuyers) / float64(s.EntryCount) * discoveryRate

		scores = append(scores, NoveltyScore{
			ContentType:      ct,
			BuyerCount:       s.DistinctBuyers,
			CompetingEntries: s.EntryCount,
			DiscoveryRate:    discoveryRate,
			Score:            score,
		})
	}

	// Sort descending by score for deterministic output.
	sort.Slice(scores, func(i, j int) bool {
		if scores[i].Score != scores[j].Score {
			return scores[i].Score > scores[j].Score
		}
		return scores[i].ContentType < scores[j].ContentType
	})

	return scores
}

// computeParameterUpdates derives proposed MarketParameter updates from novelty
// scores and content type statistics.
//
// Commission rate logic:
//   - High novelty (>= 0.5): lower commission (incentivise entry — buyers are
//     discovering new content, market is healthy)
//   - Low novelty (< 0.2): higher commission (market stagnant — reduce commission
//     to attract new sellers, not penalise existing ones)
//   - Medium: keep current rate, adjust by step
//
// Floor multiplier logic:
//   - High novelty: floor at 1.0 (let fast loop operate freely)
//   - Very low novelty and rising price trend: floor at 0.9 (mild discount to
//     stimulate demand in stagnant over-priced types)
//
// Global price scaling:
//   - If mean novelty is above 0.5: slight upward scaling (market is healthy)
//   - If mean novelty is below 0.2: slight downward scaling (encourage activity)
func (l *SlowLoop) computeParameterUpdates(
	current MarketParameters,
	noveltyScores []NoveltyScore,
	stats map[string]*contentTypeStats,
) MarketParameters {
	proposed := MarketParameters{
		PriceScalingFactor:    current.PriceScalingFactor,
		ContentTypeCommission: make(map[string]float64, len(current.ContentTypeCommission)),
		ContentTypeFloor:      make(map[string]float64, len(current.ContentTypeFloor)),
	}

	// Copy current parameters as base.
	for k, v := range current.ContentTypeCommission {
		proposed.ContentTypeCommission[k] = v
	}
	for k, v := range current.ContentTypeFloor {
		proposed.ContentTypeFloor[k] = v
	}

	var totalNovelty float64
	for _, ns := range noveltyScores {
		totalNovelty += ns.Score
	}
	meanNovelty := 0.0
	if len(noveltyScores) > 0 {
		meanNovelty = totalNovelty / float64(len(noveltyScores))
	}

	// Adjust per-content-type parameters.
	for _, ns := range noveltyScores {
		s := stats[ns.ContentType]
		if s == nil {
			continue
		}

		// Commission rate adjustment.
		currentComm, ok := current.ContentTypeCommission[ns.ContentType]
		if !ok {
			currentComm = DefaultCommissionRate
		}
		var newComm float64
		switch {
		case ns.Score >= 0.5:
			// High novelty: lower commission to encourage more sellers.
			newComm = math.Max(currentComm-l.currentStep, MinCommissionRate)
		case ns.Score < 0.2 && s.TotalSales > 0:
			// Low novelty with some sales: raise commission slightly (extract
			// value from stagnant high-volume types).
			newComm = math.Min(currentComm+l.currentStep, MaxCommissionRate)
		default:
			newComm = currentComm // no change
		}
		proposed.ContentTypeCommission[ns.ContentType] = newComm

		// Floor multiplier adjustment.
		currentFloor, ok := current.ContentTypeFloor[ns.ContentType]
		if !ok {
			currentFloor = DefaultFloorMultiplier
		}
		var newFloor float64
		switch {
		case ns.Score >= 0.5:
			// High novelty: remove floor (fast loop can price freely).
			newFloor = DefaultFloorMultiplier
		case ns.Score < 0.1 && s.PriceTrend > 0 && s.DistinctBuyers > 0:
			// Very low novelty with rising prices and existing buyers:
			// apply mild floor discount to stimulate demand.
			newFloor = math.Max(currentFloor-l.currentStep, MinMultiplier)
		default:
			newFloor = currentFloor
		}
		proposed.ContentTypeFloor[ns.ContentType] = newFloor
	}

	// Global price scaling factor.
	switch {
	case meanNovelty >= 0.5:
		// Healthy market: scale up slightly.
		proposed.PriceScalingFactor = math.Min(
			current.PriceScalingFactor+l.currentStep,
			MaxMultiplier,
		)
	case meanNovelty < 0.2 && len(noveltyScores) > 0:
		// Stagnant market: scale down slightly to attract buyers.
		proposed.PriceScalingFactor = math.Max(
			current.PriceScalingFactor-l.currentStep,
			MinMultiplier,
		)
	default:
		// Neutral: no global scaling change.
	}

	return proposed
}

// applyCorrectnessGate enforces Layer 0 bounds on proposed parameters.
// Rejects any parameter that would breach the correctness invariants:
//   - commission rate must be in [MinCommissionRate, MaxCommissionRate]
//   - floor multiplier must be in [MinMultiplier, MaxMultiplier]
//   - price scaling factor must be in [MinMultiplier, MaxMultiplier]
func (l *SlowLoop) applyCorrectnessGate(params MarketParameters) MarketParameters {
	// Clamp global scaling factor.
	if params.PriceScalingFactor < MinMultiplier {
		params.PriceScalingFactor = MinMultiplier
	}
	if params.PriceScalingFactor > MaxMultiplier {
		params.PriceScalingFactor = MaxMultiplier
	}

	// Clamp per-type commission rates.
	for ct, rate := range params.ContentTypeCommission {
		if rate < MinCommissionRate {
			params.ContentTypeCommission[ct] = MinCommissionRate
		} else if rate > MaxCommissionRate {
			params.ContentTypeCommission[ct] = MaxCommissionRate
		}
	}

	// Clamp per-type floor multipliers.
	for ct, floor := range params.ContentTypeFloor {
		if floor < MinMultiplier {
			params.ContentTypeFloor[ct] = MinMultiplier
		} else if floor > MaxMultiplier {
			params.ContentTypeFloor[ct] = MaxMultiplier
		}
	}

	return params
}

// sumRawNovelty sums raw novelty scores across all types. Used to determine
// whether the market is active (non-zero sum = at least one type has buyers).
func sumRawNovelty(scores []NoveltyScore) float64 {
	total := 0.0
	for _, ns := range scores {
		total += ns.Score
	}
	return total
}

// detectOscillation computes lag-1 autocorrelation of the recent "kept" series.
// Returns true if autocorrelation < OscillationThreshold (fitting noise).
func (l *SlowLoop) detectOscillation() bool {
	window := l.history
	if len(window) > OscillationWindow {
		window = window[len(window)-OscillationWindow:]
	}
	if len(window) < 4 {
		return false // insufficient data
	}

	// Convert bool series to float: kept=1, reverted=0.
	series := make([]float64, len(window))
	for i, h := range window {
		if h.kept {
			series[i] = 1.0
		}
	}

	autocorr := lagOneAutocorrelation(series)
	return autocorr < OscillationThreshold
}

// recordHistory appends a parameter history entry and trims to OscillationWindow.
func (l *SlowLoop) recordHistory(kept bool) {
	l.history = append(l.history, paramHistory{kept: kept, stepSize: l.currentStep})
	if len(l.history) > OscillationWindow*2 {
		// Keep recent entries only; avoid unbounded growth.
		l.history = l.history[len(l.history)-OscillationWindow:]
	}
}

// lagOneAutocorrelation computes the lag-1 Pearson autocorrelation of a series.
// Returns a value in [-1, 1]; returns 0 if variance is zero.
func lagOneAutocorrelation(series []float64) float64 {
	n := len(series)
	if n < 2 {
		return 0
	}

	// Compute mean.
	var sum float64
	for _, v := range series {
		sum += v
	}
	mean := sum / float64(n)

	// Compute variance and lag-1 covariance.
	var variance, cov float64
	for i := 0; i < n; i++ {
		d := series[i] - mean
		variance += d * d
		if i > 0 {
			cov += (series[i] - mean) * (series[i-1] - mean)
		}
	}

	if variance == 0 {
		return 0
	}

	return cov / variance
}

// computePriceTrend computes a simple linear regression slope for a price
// time series. prices and timestamps must have the same length (>= 2).
// Returns positive slope for rising prices, negative for falling.
// The slope is normalised by the mean price to produce a dimensionless ratio.
func computePriceTrend(prices, timestamps []int64) float64 {
	n := float64(len(prices))
	if n < 2 || len(timestamps) != len(prices) {
		return 0
	}

	// Normalise timestamps to [0, 1] to avoid numerical issues with nanoseconds.
	minT := timestamps[0]
	maxT := timestamps[0]
	for _, t := range timestamps {
		if t < minT {
			minT = t
		}
		if t > maxT {
			maxT = t
		}
	}
	tRange := float64(maxT - minT)
	if tRange == 0 {
		return 0
	}

	var sumX, sumY, sumXY, sumX2 float64
	for i := range prices {
		x := float64(timestamps[i]-minT) / tRange
		y := float64(prices[i])
		sumX += x
		sumY += y
		sumXY += x * y
		sumX2 += x * x
	}

	denom := n*sumX2 - sumX*sumX
	if math.Abs(denom) < 1e-12 {
		return 0
	}
	slope := (n*sumXY - sumX*sumY) / denom

	// Normalise slope by mean price to get a dimensionless ratio.
	meanPrice := sumY / n
	if meanPrice == 0 {
		return 0
	}
	return slope / meanPrice
}
