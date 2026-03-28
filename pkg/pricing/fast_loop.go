// Package pricing implements the three-loop dynamic pricing engine for the
// DontGuess exchange.
//
// The fast loop (this file) runs every 5 minutes and targets Layer 1
// (Transaction Efficiency: tokens_saved / price_paid). It reads purchase
// events and cache hit/miss rates from exchange state, computes demand
// velocity per entry, and writes dynamic price adjustments that the exchange
// engine's computePrice function applies on top of the structural base price.
//
// Signal pipeline (per loop tick):
//
//  1. Slice PriceHistory to the recent window (default: last 60 min)
//  2. Compute velocity per entry (sales/hour within the window)
//  3. Compute volume surplus (actual velocity vs. expected baseline)
//  4. Derive price multiplier from velocity using a capped sigmoid
//  5. Write PriceAdjustment to exchange state with TTL = 2× loop interval
//
// The loop also incorporates cache hit/miss rates (preview-to-purchase ratio)
// as a secondary elasticity signal. High hit rate with low conversion suggests
// the price is too high; low hit rate suggests the content itself may be the
// barrier (handled by the medium loop via reputation).
package pricing

import (
	"context"
	"math"
	"time"

	"github.com/3dl-dev/dontguess/pkg/exchange"
)

// DefaultFastLoopInterval is the cadence at which the fast loop runs.
const DefaultFastLoopInterval = 5 * time.Minute

// DefaultVelocityWindow is the lookback window for demand velocity computation.
// Recent sales within this window are used to estimate the current buy rate.
const DefaultVelocityWindow = 60 * time.Minute

// MaxMultiplier is the maximum price multiplier the fast loop can apply.
// Prevents runaway price spikes on viral entries.
const MaxMultiplier = 2.0

// MinMultiplier is the minimum price multiplier the fast loop can apply.
// Prevents price collapse on cold entries below the structural floor.
const MinMultiplier = 0.5

// AdjustmentTTL is how long a fast-loop price adjustment stays live before it
// is treated as expired. Set to 2× the loop interval so a skipped tick does
// not immediately revert prices to 1.0x.
const AdjustmentTTL = 2 * DefaultFastLoopInterval

// BaselineVelocity is the expected purchases-per-hour for an average entry
// across the whole market. Used to compute volume surplus (actual / expected).
// Derived from market history by the slow loop; the fast loop uses a constant
// baseline. This value approximates one sale per 24 hours per entry.
const BaselineVelocity = 1.0 / 24.0 // sales/hour

// StateReader is the read interface the fast loop needs from exchange state.
// Using an interface here allows the loop to be tested without a full exchange.
type StateReader interface {
	// PriceHistory returns all recorded settlement price events.
	PriceHistory() []exchange.PriceRecord
	// Inventory returns all live inventory entries.
	Inventory() []*exchange.InventoryEntry
	// EntryPreviewCount returns the number of previews served for an entry.
	EntryPreviewCount(entryID string) int
	// EntryDemandCount returns the number of distinct completed buyers for an entry.
	EntryDemandCount(entryID string) int
}

// StateWriter is the write interface the fast loop needs from exchange state.
type StateWriter interface {
	// SetPriceAdjustment writes a dynamic price multiplier for an entry.
	SetPriceAdjustment(entryID string, adj exchange.PriceAdjustment)
}

// StateReadWriter combines the read and write interfaces. The concrete
// *exchange.State satisfies this interface.
type StateReadWriter interface {
	StateReader
	StateWriter
}

// FastLoopOptions configures a FastLoop.
type FastLoopOptions struct {
	// State is the exchange state used to read demand signals and write adjustments.
	State StateReadWriter
	// Interval is how often the loop runs. Defaults to DefaultFastLoopInterval.
	Interval time.Duration
	// VelocityWindow is the lookback window for velocity computation.
	// Defaults to DefaultVelocityWindow.
	VelocityWindow time.Duration
	// AdjustmentTTL overrides how long each written adjustment stays live.
	// Defaults to 2× Interval.
	AdjustmentTTL time.Duration
	// Logger receives diagnostic log lines. If nil, logs are suppressed.
	Logger func(format string, args ...any)
	// Now overrides time.Now for testing determinism.
	Now func() time.Time
}

func (o *FastLoopOptions) interval() time.Duration {
	if o.Interval > 0 {
		return o.Interval
	}
	return DefaultFastLoopInterval
}

func (o *FastLoopOptions) velocityWindow() time.Duration {
	if o.VelocityWindow > 0 {
		return o.VelocityWindow
	}
	return DefaultVelocityWindow
}

func (o *FastLoopOptions) adjustmentTTL() time.Duration {
	if o.AdjustmentTTL > 0 {
		return o.AdjustmentTTL
	}
	return 2 * o.interval()
}

func (o *FastLoopOptions) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func (o *FastLoopOptions) log(format string, args ...any) {
	if o.Logger != nil {
		o.Logger(format, args...)
	}
}

// FastLoop runs the Layer 1 pricing loop. It reads demand velocity signals from
// exchange state and writes dynamic price adjustments at a configurable interval.
type FastLoop struct {
	opts FastLoopOptions
}

// NewFastLoop creates a FastLoop with the given options.
func NewFastLoop(opts FastLoopOptions) *FastLoop {
	return &FastLoop{opts: opts}
}

// Run starts the fast loop. It blocks until ctx is cancelled, running Tick on
// each interval. The first tick fires immediately on startup.
func (l *FastLoop) Run(ctx context.Context) error {
	// Run immediately, then on each tick.
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

// Tick performs a single fast-loop computation cycle:
//  1. Slices PriceHistory to the velocity window
//  2. Computes per-entry demand velocity and volume surplus
//  3. Incorporates preview-to-purchase hit rate as elasticity signal
//  4. Writes PriceAdjustment to state for each entry with non-trivial demand
//
// Tick is exported so it can be called directly in tests without running the
// full Run loop.
func (l *FastLoop) Tick() {
	now := l.opts.now()
	window := l.opts.velocityWindow()
	ttl := l.opts.adjustmentTTL()
	cutoff := now.Add(-window)
	windowHours := window.Hours()

	// Slice price history to the velocity window.
	history := l.opts.State.PriceHistory()
	recent := make([]exchange.PriceRecord, 0, len(history))
	for _, rec := range history {
		if time.Unix(0, rec.Timestamp).After(cutoff) {
			recent = append(recent, rec)
		}
	}

	// Count sales per entry within the window.
	salesByEntry := make(map[string]int, len(recent))
	for _, rec := range recent {
		salesByEntry[rec.EntryID]++
	}

	// Compute adjustments for all live inventory entries.
	entries := l.opts.State.Inventory()
	adjusted := 0
	for _, entry := range entries {
		salesInWindow := salesByEntry[entry.EntryID]
		velocityPerHour := float64(salesInWindow) / windowHours

		// Volume surplus: ratio of actual velocity to the market baseline.
		// > 1.0 means this entry is selling faster than average.
		// = 0.0 means no activity (surplus = 0, no adjustment needed).
		volumeSurplus := velocityPerHour / BaselineVelocity

		// Preview-to-purchase elasticity signal.
		// High preview count with low conversion → price may be above willingness-to-pay.
		// We compute an elasticity factor that modestly dampens price when conversion
		// rate is very low (under 10%), and slightly amplifies when conversion is high.
		previewCount := l.opts.State.EntryPreviewCount(entry.EntryID)
		demandCount := l.opts.State.EntryDemandCount(entry.EntryID)
		elasticityFactor := computeElasticity(previewCount, demandCount)

		// Compute the combined multiplier from velocity + elasticity.
		multiplier := computeMultiplier(volumeSurplus, elasticityFactor)

		// Skip entries where the multiplier is effectively 1.0 (within 1%).
		// Writing tiny adjustments wastes state memory and creates noise.
		if math.Abs(multiplier-1.0) < 0.01 {
			continue
		}

		adj := exchange.PriceAdjustment{
			Multiplier:      multiplier,
			ExpiresAt:       now.Add(ttl),
			VelocityPerHour: velocityPerHour,
			VolumeSurplus:   volumeSurplus,
		}
		l.opts.State.SetPriceAdjustment(entry.EntryID, adj)
		adjusted++
	}

	l.opts.log("fast loop tick: %d entries in inventory, %d recent sales in window, %d adjustments written",
		len(entries), len(recent), adjusted)
}

// computeMultiplier derives a price multiplier from volume surplus and
// elasticity. The result is clamped to [MinMultiplier, MaxMultiplier].
//
// Velocity component: uses a sigmoid centered at baseline surplus=1.0 such that:
//   - surplus=0 (cold entry): velocity component ≈ 0.85 (mild discount)
//   - surplus=1 (at baseline): velocity component = 1.0 (no change)
//   - surplus=4 (4× baseline): velocity component ≈ 1.35 (35% premium)
//   - surplus=16+ (high demand): velocity component → MaxMultiplier
//
// Elasticity component: scales based on preview-to-purchase signal.
func computeMultiplier(volumeSurplus, elasticityFactor float64) float64 {
	// Map surplus to a velocity multiplier using a logistic function.
	// logistic(x) = 1 / (1 + exp(-k*(x-mid)))
	// k=0.6, mid=1.0: smooth S-curve centered at baseline.
	// The output is [0,1]; we map it to the desired range using:
	//   velocityMultiplier = 1.0 + (logistic - 0.5) * adjustmentRange
	// This ensures logistic(mid=1.0)=0.5 → velocityMultiplier=1.0 (neutral).
	const k = 0.6
	const mid = 1.0
	const adjustmentRange = MaxMultiplier - MinMultiplier // 1.5
	logistic := 1.0 / (1.0 + math.Exp(-k*(volumeSurplus-mid)))
	// Center: logistic=0.5 → 1.0x, logistic=1.0 → 1.0 + 0.5*1.5 = 1.75x
	// Low:    logistic≈0   → 1.0 - 0.5*1.5 = 0.25x (clamped to MinMultiplier)
	velocityMultiplier := 1.0 + (logistic-0.5)*adjustmentRange

	// Blend with elasticity factor (weighted 70% velocity, 30% elasticity).
	multiplier := 0.7*velocityMultiplier + 0.3*elasticityFactor

	// Clamp to [MinMultiplier, MaxMultiplier].
	if multiplier < MinMultiplier {
		multiplier = MinMultiplier
	}
	if multiplier > MaxMultiplier {
		multiplier = MaxMultiplier
	}
	return multiplier
}

// computeElasticity derives a price elasticity factor from preview and demand
// counts. Returns a value in [MinMultiplier, MaxMultiplier]:
//
//   - No data (0 previews): returns 1.0 — no signal, no change.
//   - Very low conversion (< 5%): returns 0.85 — price may be above willingness-to-pay.
//   - Low conversion (5–15%): returns ~0.95 — slight dampening.
//   - Moderate conversion (15–40%): returns ~1.0 — neutral.
//   - High conversion (> 40%): returns 1.05–1.15 — modest premium for in-demand content.
//
// The minimum preview count threshold of 5 ensures we don't act on noise from
// a single preview/non-purchase.
func computeElasticity(previewCount, demandCount int) float64 {
	const minPreviews = 5
	if previewCount < minPreviews {
		return 1.0 // insufficient data
	}

	conversionRate := float64(demandCount) / float64(previewCount)

	// Map conversion rate to elasticity factor using a linear scale.
	// 0% → 0.85, 20% (neutral) → 1.0, 50% → 1.15, capped at 1.15.
	const neutral = 0.20
	const slopeBelow = (1.0 - 0.85) / neutral  // 0.75 per unit below neutral
	const slopeAbove = (1.15 - 1.0) / (1 - neutral) // 0.1875 per unit above neutral

	var factor float64
	if conversionRate < neutral {
		factor = 1.0 - slopeBelow*(neutral-conversionRate)
	} else {
		factor = 1.0 + slopeAbove*(conversionRate-neutral)
	}

	// Clamp to [MinMultiplier, MaxMultiplier].
	if factor < MinMultiplier {
		factor = MinMultiplier
	}
	if factor > MaxMultiplier {
		factor = MaxMultiplier
	}
	return factor
}
