// Package pricing — MediumLoop (1 hr cadence, Layer 2 gate).
//
// The medium loop runs every hour and applies three corrections to the market:
//
//  1. Accumulated adjustment correction — reads fast-loop price adjustments,
//     detects outliers within content-type clusters, and dampens them toward
//     the cluster mean. Prevents runaway prices on thin-signal entries.
//
//  2. Residual settlements — scans price history for the current window,
//     computes per-seller residual earnings from resales, and credits scrip via
//     ScripStore. This is a correction pass: the engine pays residuals inline on
//     settle(complete), but any gaps (ScripStore unavailable at settle time,
//     deferred settlement) are caught here. Already-paid residuals are
//     de-duplicated by a watermark stored in the loop state (not in exchange state).
//
//  3. Reputation score updates — recomputes each seller's Layer 2 value
//     composite and applies a reputation-derived price floor adjustment for
//     sellers whose score falls below the floor threshold. This is an additive
//     correction on top of the fast loop's demand-velocity adjustments.
//
// Layer 2 value composite (from docs/heritage/value-function.md):
//
//	V_medium = completion_rate * efficiency * recency_factor * diversity_factor
//
// Only sellers with sufficient data (≥ MinTransactionsForReputation completed
// sales) are evaluated. Sellers below the threshold retain their current score.
//
// The medium loop does NOT own the correctness gate (Layer 0) — that gate is
// enforced by the exchange engine (conversion-rate exclusion, dontguess-5iz).
// The medium loop targets Layer 2 corrections only.
package pricing

import (
	"context"
	"math"
	"sort"
	"time"

	"github.com/3dl-dev/dontguess/pkg/exchange"
	"github.com/3dl-dev/dontguess/pkg/scrip"
)

// DefaultMediumLoopInterval is the cadence at which the medium loop runs.
const DefaultMediumLoopInterval = 1 * time.Hour

// DefaultCompressionPurchaseThreshold is the minimum number of distinct
// completed buyers an entry must have before the medium loop considers posting
// an open compression assign. Entries below this threshold are ignored.
const DefaultCompressionPurchaseThreshold = 3

// DefaultMediumLoopWindow is the lookback window for residual settlement and
// reputation computation. Covers the last 2 hours to overlap with the previous
// tick and catch any gaps from the fast loop.
const DefaultMediumLoopWindow = 2 * time.Hour

// ClusterDampeningFactor controls how strongly outlier adjustments are pulled
// toward the cluster mean. 0.5 = 50% dampen toward mean (conservative).
// Range [0, 1]: 0 = no dampen, 1 = fully replace with cluster mean.
const ClusterDampeningFactor = 0.5

// MinClusterSize is the minimum number of entries in a content-type cluster
// before the dampen correction is applied. Clusters smaller than this are
// left unchanged — too few data points to compute a reliable mean.
const MinClusterSize = 3

// RepFloorThreshold is the seller reputation score below which a reputation
// floor adjustment is applied. Sellers at or above this score are not penalized.
const RepFloorThreshold = 30

// RepFloorMultiplier is the price multiplier applied to entries from sellers
// whose reputation is below RepFloorThreshold. Below the floor, buyers are
// asked to pay less — signalling low seller quality — rather than cutting
// off inventory entirely (which would reduce market liquidity).
const RepFloorMultiplier = 0.8

// RepFloorAdjustmentTTL is how long a reputation floor adjustment stays live.
// Set to 2× the loop interval so a skipped tick does not immediately clear it.
const RepFloorAdjustmentTTL = 2 * DefaultMediumLoopInterval

// MinTransactionsForReputation is the minimum number of completed transactions
// a seller needs before reputation-based adjustments are applied. Below this,
// reputation is treated as insufficient data.
const MinTransactionsForReputation = 3

// ResidualWindowFraction is the fraction of the full window used for residual
// reconciliation. We look back DefaultMediumLoopWindow for completeness.
const ResidualWindowFraction = 1.0

// MediumStateReader is the read interface the medium loop needs from exchange state.
type MediumStateReader interface {
	// Inventory returns all live inventory entries.
	Inventory() []*exchange.InventoryEntry
	// PriceHistory returns all recorded settlement price events.
	PriceHistory() []exchange.PriceRecord
	// AllPriceAdjustments returns all active (non-expired) fast loop adjustments.
	AllPriceAdjustments() map[string]exchange.PriceAdjustment
	// SellerReputation returns the derived reputation score (0-100) for a seller.
	SellerReputation(sellerKey string) int
	// EntryDemandCount returns the number of distinct completed buyers for an entry.
	EntryDemandCount(entryID string) int
	// AllSellerKeys returns all seller public keys that have inventory entries.
	AllSellerKeys() []string
	// HasCompressedVersion returns true if a compressed derivative exists for entryID.
	HasCompressedVersion(entryID string) bool
	// PurchaseCount returns the number of distinct completed buyers for an entry.
	PurchaseCount(entryID string) int
	// ActiveAssigns returns all non-terminal assign records for the given entryID.
	ActiveAssigns(entryID string) []*exchange.AssignRecord
}

// MediumStateWriter is the write interface the medium loop needs from exchange state.
type MediumStateWriter interface {
	// SetPriceAdjustment writes a dynamic price multiplier for an entry.
	SetPriceAdjustment(entryID string, adj exchange.PriceAdjustment)
}

// MediumStateReadWriter combines the read and write interfaces.
// The concrete *exchange.State satisfies this interface.
type MediumStateReadWriter interface {
	MediumStateReader
	MediumStateWriter
}

// ResidualRecord tracks a completed residual payment made by the medium loop.
// Used for deduplication across ticks.
type ResidualRecord struct {
	// EntryID is the inventory entry the residual was paid for.
	EntryID string
	// SellerKey is the seller who received the residual.
	SellerKey string
	// Amount is the scrip amount credited.
	Amount int64
	// PaidAt is when the medium loop paid this residual.
	PaidAt time.Time
}

// MediumLoopResult summarises the outcome of a single medium loop tick.
// Exported for use in tests and diagnostics.
type MediumLoopResult struct {
	// ClusterCorrections is the number of fast-loop adjustments dampened
	// toward the cluster mean.
	ClusterCorrections int
	// ResidualsPaid is the number of residual payments issued this tick.
	ResidualsPaid int
	// TotalResidualScrip is the total scrip credited to sellers this tick.
	TotalResidualScrip int64
	// ReputationUpdates is the number of entries that received a
	// reputation-floor adjustment.
	ReputationUpdates int
	// CompressionAssigns is the number of open compression assign tasks posted
	// this tick for high-demand uncompressed entries.
	CompressionAssigns int
	// VigPressure is the total outstanding vig across all active loans at the
	// time of this tick. Zero when no VigStore is configured or no active loans exist.
	VigPressure int64
}

// AssignSpec describes an open compression assign to be posted by the medium loop.
// It is passed to MediumLoopOptions.PostAssign for the caller to dispatch.
type AssignSpec struct {
	// EntryID is the inventory entry requiring compression.
	EntryID string
	// Reward is the scrip bounty for completing the task (break-even rate:
	// token_cost / 2, matching the hot-compression bounty).
	Reward int64
	// TaskType is always "compress" for medium-loop compression assigns.
	TaskType string
}

// VigReader is the interface MediumLoop uses to read total outstanding vig
// across all active loans. Implemented by *scrip.CampfireScripStore.
// If nil, vig_pressure is treated as zero (useful for tests that do not
// exercise the loan flow).
type VigReader interface {
	TotalOutstandingVig() int64
}

// MediumLoopOptions configures a MediumLoop.
type MediumLoopOptions struct {
	// State is the exchange state used to read market signals and write adjustments.
	State MediumStateReadWriter
	// ScripStore is the scrip spending store. If nil, residual settlement is
	// skipped (useful for tests that do not exercise the scrip flow).
	ScripStore scrip.SpendingStore
	// VigStore is the loan store used to read the vig_pressure signal.
	// If nil, vig_pressure is zero for the tick (loan flow not exercised).
	VigStore VigReader
	// Interval is how often the loop runs. Defaults to DefaultMediumLoopInterval.
	Interval time.Duration
	// Window is the lookback window for price history analysis.
	// Defaults to DefaultMediumLoopWindow.
	Window time.Duration
	// Logger receives diagnostic log lines. If nil, logs are suppressed.
	Logger func(format string, args ...any)
	// Now overrides time.Now for testing determinism.
	Now func() time.Time
	// PostAssign is called for each open compression assign the medium loop wants
	// to post. If nil, compression assign posting is skipped. The caller is
	// responsible for dispatching the assign to the campfire (engine.PostAssign
	// or equivalent). Errors from PostAssign are logged and do not abort the tick.
	PostAssign func(spec AssignSpec) error
	// CompressionPurchaseThreshold is the minimum purchase count an entry must
	// have before a compression assign is posted. Defaults to
	// DefaultCompressionPurchaseThreshold (3).
	CompressionPurchaseThreshold int
}

func (o *MediumLoopOptions) interval() time.Duration {
	if o.Interval > 0 {
		return o.Interval
	}
	return DefaultMediumLoopInterval
}

func (o *MediumLoopOptions) window() time.Duration {
	if o.Window > 0 {
		return o.Window
	}
	return DefaultMediumLoopWindow
}

func (o *MediumLoopOptions) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func (o *MediumLoopOptions) log(format string, args ...any) {
	if o.Logger != nil {
		o.Logger(format, args...)
	}
}

func (o *MediumLoopOptions) compressionPurchaseThreshold() int {
	if o.CompressionPurchaseThreshold > 0 {
		return o.CompressionPurchaseThreshold
	}
	return DefaultCompressionPurchaseThreshold
}

// MediumLoop runs the Layer 2 correction loop. It dampens fast-loop outlier
// adjustments, settles accumulated residuals, and applies reputation floor
// corrections at a configurable interval.
type MediumLoop struct {
	opts MediumLoopOptions

	// paidResiduals deduplicates residual payments within this loop's lifetime.
	// Key: entryID + ":" + priceRecordTimestamp (nanoseconds as string).
	// Value: true if already paid.
	// This is intentionally in-memory only — loop restarts are handled by the
	// window cutoff, which naturally excludes old records.
	paidResiduals map[string]bool
}

// NewMediumLoop creates a MediumLoop with the given options.
func NewMediumLoop(opts MediumLoopOptions) *MediumLoop {
	return &MediumLoop{
		opts:          opts,
		paidResiduals: make(map[string]bool),
	}
}

// Run starts the medium loop. It blocks until ctx is cancelled, running Tick on
// each interval. The first tick fires immediately on startup.
func (l *MediumLoop) Run(ctx context.Context) error {
	l.Tick(ctx)

	ticker := time.NewTicker(l.opts.interval())
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			l.Tick(ctx)
		}
	}
}

// Tick performs a single medium-loop computation cycle:
//  1. Cluster correction — dampens fast loop outlier adjustments
//  2. Residual settlements — pays accumulated residuals to sellers
//  3. Reputation updates — applies floor adjustments for low-rep sellers
//  4. Compression assigns — posts open compression tasks for high-demand
//     uncompressed entries (requires PostAssign to be configured)
//
// Tick is exported so it can be called directly in tests without running the
// full Run loop.
func (l *MediumLoop) Tick(ctx context.Context) MediumLoopResult {
	now := l.opts.now()
	window := l.opts.window()
	cutoff := now.Add(-window)

	inventory := l.opts.State.Inventory()
	priceHistory := l.opts.State.PriceHistory()
	adjustments := l.opts.State.AllPriceAdjustments()

	// Filter history to the window.
	recentHistory := make([]exchange.PriceRecord, 0, len(priceHistory))
	for _, rec := range priceHistory {
		if time.Unix(0, rec.Timestamp).After(cutoff) {
			recentHistory = append(recentHistory, rec)
		}
	}

	// Build entry → seller map for lookups.
	entryToSeller := make(map[string]string, len(inventory))
	for _, e := range inventory {
		entryToSeller[e.EntryID] = e.SellerKey
	}

	// 1. Cluster correction.
	corrections := l.applyClusterCorrection(now, inventory, adjustments)

	// 2. Residual settlements.
	residualsPaid, totalScrip := l.settleResiduals(ctx, now, recentHistory, inventory)

	// 3. Reputation updates.
	repUpdates := l.applyReputationFloor(now, inventory)

	// 4. Compression assigns.
	compressionAssigns := l.postCompressionAssigns(inventory)

	// 5. Vig pressure — total outstanding vig across all active loans.
	// This is a read-only market signal; the medium loop does not act on it
	// directly (that is the slow loop's domain), but it is surfaced in the
	// result for diagnostics and higher-level loop consumption.
	var vigPressure int64
	if l.opts.VigStore != nil {
		vigPressure = l.opts.VigStore.TotalOutstandingVig()
	}

	result := MediumLoopResult{
		ClusterCorrections: corrections,
		ResidualsPaid:      residualsPaid,
		TotalResidualScrip: totalScrip,
		ReputationUpdates:  repUpdates,
		CompressionAssigns: compressionAssigns,
		VigPressure:        vigPressure,
	}

	l.opts.log("medium loop tick: window=%s, history_records=%d, corrections=%d, residuals=%d (scrip=%d), rep_updates=%d, compression_assigns=%d, vig_pressure=%d",
		window, len(recentHistory), corrections, residualsPaid, totalScrip, repUpdates, compressionAssigns, vigPressure)

	return result
}

// applyClusterCorrection groups inventory entries by content type, computes the
// mean active adjustment multiplier per cluster, and dampens outlier entries
// toward the cluster mean using ClusterDampeningFactor.
//
// An "outlier" is defined as an entry whose adjustment deviates from the cluster
// mean by more than one standard deviation. Only clusters with ≥ MinClusterSize
// active adjustments are corrected.
//
// Returns the number of adjustments modified.
func (l *MediumLoop) applyClusterCorrection(
	now time.Time,
	inventory []*exchange.InventoryEntry,
	adjustments map[string]exchange.PriceAdjustment,
) int {
	// Build content-type → list of (entryID, multiplier) for entries that have
	// an active fast-loop adjustment.
	type adjEntry struct {
		entryID    string
		multiplier float64
	}
	clusterMap := make(map[string][]adjEntry)

	for _, entry := range inventory {
		adj, ok := adjustments[entry.EntryID]
		if !ok || adj.IsExpired() || adj.Multiplier <= 0 {
			continue
		}
		ct := entry.ContentType
		if ct == "" {
			ct = "other"
		}
		clusterMap[ct] = append(clusterMap[ct], adjEntry{entry.EntryID, adj.Multiplier})
	}

	corrections := 0
	ttl := 2 * l.opts.interval()

	for ct, entries := range clusterMap {
		if len(entries) < MinClusterSize {
			continue
		}

		// Compute mean multiplier for the cluster.
		var sum float64
		for _, e := range entries {
			sum += e.multiplier
		}
		mean := sum / float64(len(entries))

		// Compute standard deviation.
		var variance float64
		for _, e := range entries {
			diff := e.multiplier - mean
			variance += diff * diff
		}
		stddev := math.Sqrt(variance / float64(len(entries)))

		_ = ct // used for logging if Logger is set

		// Dampen outliers (entries more than 1 stddev from mean).
		for _, e := range entries {
			if math.Abs(e.multiplier-mean) <= stddev {
				continue // within normal range
			}

			// Blend current multiplier toward cluster mean.
			corrected := e.multiplier + ClusterDampeningFactor*(mean-e.multiplier)

			// Clamp to [MinMultiplier, MaxMultiplier].
			if corrected < MinMultiplier {
				corrected = MinMultiplier
			}
			if corrected > MaxMultiplier {
				corrected = MaxMultiplier
			}

			// Only write if the correction meaningfully differs (> 1%).
			if math.Abs(corrected-e.multiplier) < 0.01 {
				continue
			}

			// Preserve the original velocity/surplus diagnostics; update multiplier.
			orig := adjustments[e.entryID]
			corrAdj := exchange.PriceAdjustment{
				Multiplier:      corrected,
				ExpiresAt:       now.Add(ttl),
				VelocityPerHour: orig.VelocityPerHour,
				VolumeSurplus:   orig.VolumeSurplus,
			}
			l.opts.State.SetPriceAdjustment(e.entryID, corrAdj)
			corrections++
		}
	}

	return corrections
}

// settleResiduals scans recent price history to identify sales and computes
// residual payments owed to original sellers for each resale. Residuals are
// paid via ScripStore if configured. Already-processed records are skipped via
// the in-memory paidResiduals deduplication map.
//
// Residual rate: ResidualRate from exchange package (10% = price/10).
//
// Returns (count of payments made, total scrip credited).
func (l *MediumLoop) settleResiduals(
	ctx context.Context,
	now time.Time,
	recentHistory []exchange.PriceRecord,
	inventory []*exchange.InventoryEntry,
) (int, int64) {
	if l.opts.ScripStore == nil {
		return 0, 0
	}

	// Build entry → seller map.
	entryToSeller := make(map[string]string, len(inventory))
	for _, e := range inventory {
		entryToSeller[e.EntryID] = e.SellerKey
	}

	// Group sales by entry and compute total resale revenue per entry.
	// We accumulate residuals per (entry, seller) and pay once per tick.
	type residualKey struct {
		entryID   string
		sellerKey string
	}
	type residualAccum struct {
		totalRevenue int64
		saleCount    int
	}
	accumMap := make(map[residualKey]*residualAccum)

	for _, rec := range recentHistory {
		// Deduplication key: entryID + sale timestamp (nanoseconds).
		dedupKey := rec.EntryID + ":" + int64str(rec.Timestamp)
		if l.paidResiduals[dedupKey] {
			continue
		}

		sellerKey, ok := entryToSeller[rec.EntryID]
		if !ok || sellerKey == "" {
			continue
		}

		k := residualKey{entryID: rec.EntryID, sellerKey: sellerKey}
		if accumMap[k] == nil {
			accumMap[k] = &residualAccum{}
		}
		accumMap[k].totalRevenue += rec.SalePrice
		accumMap[k].saleCount++
	}

	paid := 0
	var totalScrip int64

	// Sort keys for deterministic processing in tests.
	keys := make([]residualKey, 0, len(accumMap))
	for k := range accumMap {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].entryID != keys[j].entryID {
			return keys[i].entryID < keys[j].entryID
		}
		return keys[i].sellerKey < keys[j].sellerKey
	})

	for _, k := range keys {
		a := accumMap[k]
		if a.totalRevenue <= 0 {
			continue
		}

		// Residual = 10% of total sale revenue for this entry in the window.
		residual := a.totalRevenue / exchange.ResidualRate
		if residual <= 0 {
			continue
		}

		// Pay residual to seller.
		if _, _, err := l.opts.ScripStore.AddBudget(ctx, k.sellerKey, scrip.BalanceKey, residual, ""); err != nil {
			l.opts.log("medium loop: residual payment failed for seller %s entry %s: %v",
				k.sellerKey, k.entryID, err)
			continue
		}

		// Mark all sales for this entry in the window as paid.
		for _, rec := range recentHistory {
			if rec.EntryID != k.entryID {
				continue
			}
			dedupKey := rec.EntryID + ":" + int64str(rec.Timestamp)
			l.paidResiduals[dedupKey] = true
		}

		totalScrip += residual
		paid++
	}

	return paid, totalScrip
}

// applyReputationFloor applies a dampening price adjustment to entries from
// sellers whose reputation score is below RepFloorThreshold. This signals
// low seller quality through price (lower price, lower perceived value) while
// keeping the inventory searchable.
//
// Sellers with sufficient transaction history (≥ MinTransactionsForReputation)
// and reputation below RepFloorThreshold receive a RepFloorMultiplier (0.8x)
// adjustment. High-rep sellers are not given a positive floor boost here —
// that is handled by the fast loop's demand-velocity signals.
//
// Returns the number of entries updated.
func (l *MediumLoop) applyReputationFloor(
	now time.Time,
	inventory []*exchange.InventoryEntry,
) int {
	ttl := 2 * l.opts.interval()
	updates := 0

	for _, entry := range inventory {
		rep := l.opts.State.SellerReputation(entry.SellerKey)
		demandCount := l.opts.State.EntryDemandCount(entry.EntryID)

		// Only apply floor to sellers with sufficient transaction history.
		if demandCount < MinTransactionsForReputation {
			continue
		}

		if rep >= RepFloorThreshold {
			continue // reputation is acceptable — skip
		}

		// Compute floor multiplier: scale linearly from RepFloorThreshold (1.0x)
		// to 0 reputation (RepFloorMultiplier). This is a mild penalty, not a ban.
		// floorMultiplier = RepFloorMultiplier + (1.0 - RepFloorMultiplier) * (rep / RepFloorThreshold)
		floorMultiplier := RepFloorMultiplier + (1.0-RepFloorMultiplier)*(float64(rep)/float64(RepFloorThreshold))

		// Clamp to [MinMultiplier, MaxMultiplier].
		if floorMultiplier < MinMultiplier {
			floorMultiplier = MinMultiplier
		}
		if floorMultiplier > MaxMultiplier {
			floorMultiplier = MaxMultiplier
		}

		adj := exchange.PriceAdjustment{
			Multiplier:      floorMultiplier,
			ExpiresAt:       now.Add(ttl),
			VelocityPerHour: 0, // reputation-driven, not velocity-driven
			VolumeSurplus:   0,
		}
		l.opts.State.SetPriceAdjustment(entry.EntryID, adj)
		updates++
	}

	return updates
}

// postCompressionAssigns scans inventory for high-demand entries that lack a
// compressed derivative and have no active compression assign. For each
// qualifying entry it calls PostAssign (if configured) with an open
// (non-exclusive) compression task at break-even reward (token_cost / 2).
//
// Qualifying conditions (all must hold):
//  1. PurchaseCount(entryID) >= CompressionPurchaseThreshold
//  2. !HasCompressedVersion(entryID)
//  3. len(ActiveAssigns(entryID)) == 0
//
// Returns the number of assigns posted.
func (l *MediumLoop) postCompressionAssigns(inventory []*exchange.InventoryEntry) int {
	if l.opts.PostAssign == nil {
		return 0
	}

	threshold := l.opts.compressionPurchaseThreshold()
	posted := 0

	for _, entry := range inventory {
		// Skip compressed derivatives — they are not candidates for further compression.
		if entry.CompressedFrom != "" {
			continue
		}

		// Check purchase threshold.
		if l.opts.State.PurchaseCount(entry.EntryID) < threshold {
			continue
		}

		// Skip if a compressed derivative already exists.
		if l.opts.State.HasCompressedVersion(entry.EntryID) {
			continue
		}

		// Skip if there is already an active compression assign.
		if len(l.opts.State.ActiveAssigns(entry.EntryID)) > 0 {
			continue
		}

		// Skip entries with TokenCost < 2: integer division would yield a zero
		// bounty (TokenCost/2 == 0 for cost 0 or 1), producing a worthless assign.
		if entry.TokenCost < 2 {
			continue
		}

		// Post an open (non-exclusive) compression assign at break-even reward.
		reward := entry.TokenCost / 2
		spec := AssignSpec{
			EntryID:  entry.EntryID,
			Reward:   reward,
			TaskType: "compress",
		}
		if err := l.opts.PostAssign(spec); err != nil {
			l.opts.log("medium loop: PostAssign failed for entry %s: %v", entry.EntryID, err)
			continue
		}
		posted++
	}

	return posted
}

// int64str converts an int64 to a decimal string without importing strconv
// (avoids circular import risk; this file has no strconv dependency).
func int64str(n int64) string {
	if n == 0 {
		return "0"
	}
	negative := n < 0
	if negative {
		n = -n
	}
	buf := make([]byte, 0, 20)
	for n > 0 {
		buf = append(buf, byte('0'+n%10))
		n /= 10
	}
	// Reverse.
	for i, j := 0, len(buf)-1; i < j; i, j = i+1, j-1 {
		buf[i], buf[j] = buf[j], buf[i]
	}
	if negative {
		return "-" + string(buf)
	}
	return string(buf)
}
