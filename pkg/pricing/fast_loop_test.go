package pricing_test

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/3dl-dev/dontguess/pkg/exchange"
	"github.com/3dl-dev/dontguess/pkg/pricing"
)

// stubState is a minimal StateReadWriter for fast loop tests.
// It holds fixed inventory + price history and records written adjustments.
type stubState struct {
	inventory     []*exchange.InventoryEntry
	priceHistory  []exchange.PriceRecord
	previewCounts map[string]int
	demandCounts  map[string]int
	adjustments   map[string]exchange.PriceAdjustment
}

func newStubState() *stubState {
	return &stubState{
		previewCounts: make(map[string]int),
		demandCounts:  make(map[string]int),
		adjustments:   make(map[string]exchange.PriceAdjustment),
	}
}

func (s *stubState) PriceHistory() []exchange.PriceRecord  { return s.priceHistory }
func (s *stubState) Inventory() []*exchange.InventoryEntry  { return s.inventory }
func (s *stubState) EntryPreviewCount(entryID string) int   { return s.previewCounts[entryID] }
func (s *stubState) EntryDemandCount(entryID string) int    { return s.demandCounts[entryID] }
func (s *stubState) SetPriceAdjustment(entryID string, adj exchange.PriceAdjustment) {
	s.adjustments[entryID] = adj
}

// TestFastLoop_ColdEntryGetsColdDiscount verifies that an entry with zero recent
// sales and no preview data receives a sub-1.0 price multiplier (cold discount).
// A cold entry has volumeSurplus=0; the logistic maps this to a mild discount
// (~0.75x), which differs from 1.0 by more than the 0.01 skip threshold, so
// an adjustment IS written.
func TestFastLoop_ColdEntryGetsColdDiscount(t *testing.T) {
	t.Parallel()
	st := newStubState()
	st.inventory = []*exchange.InventoryEntry{
		{EntryID: "entry-1", PutPrice: 1000},
	}
	// No price history, no previews — cold entry.

	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	loop := pricing.NewFastLoop(pricing.FastLoopOptions{
		State: st,
		Now:   func() time.Time { return now },
	})
	loop.Tick()

	// A cold entry (no sales, no previews) should have multiplier ≈ 0.75.
	// 0.75 differs from 1.0 by 0.25 > 0.01, so an adjustment IS written.
	// But the discount is mild (velocity near 0 → cold discount).
	adj, ok := st.adjustments["entry-1"]
	if !ok {
		// Cold entries get a mild discount adjustment written
		t.Fatal("expected adjustment to be written for cold entry")
	}
	if adj.Multiplier >= 1.0 {
		t.Errorf("cold entry (no sales) should have multiplier < 1.0, got %.4f", adj.Multiplier)
	}
	if adj.Multiplier < pricing.MinMultiplier {
		t.Errorf("multiplier %.4f below MinMultiplier %.4f", adj.Multiplier, pricing.MinMultiplier)
	}
}

// TestFastLoop_HighDemandIncreasesPrice verifies that an entry with many
// recent sales in the velocity window gets a price multiplier > 1.0.
func TestFastLoop_HighDemandIncreasesPrice(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	windowHours := pricing.DefaultVelocityWindow.Hours() // 1 hour

	st := newStubState()
	st.inventory = []*exchange.InventoryEntry{
		{EntryID: "entry-hot"},
	}

	// 8 sales in the last 30 minutes = 16/hour.
	// Baseline = 1/24/hour ≈ 0.0417/hour. VolumeSurplus = 16 / 0.0417 ≈ 384.
	// At such high surplus, the logistic saturates → multiplier → MaxMultiplier.
	_ = windowHours
	for i := range 8 {
		ts := now.Add(-time.Duration(i+1) * 5 * time.Minute)
		st.priceHistory = append(st.priceHistory, exchange.PriceRecord{
			EntryID:   "entry-hot",
			SalePrice: 500,
			Timestamp: ts.UnixNano(),
		})
	}

	loop := pricing.NewFastLoop(pricing.FastLoopOptions{
		State: st,
		Now:   func() time.Time { return now },
	})
	loop.Tick()

	adj, ok := st.adjustments["entry-hot"]
	if !ok {
		t.Fatal("expected adjustment for high-demand entry")
	}
	if adj.Multiplier <= 1.0 {
		t.Errorf("high-demand entry should have multiplier > 1.0, got %.4f", adj.Multiplier)
	}
	if adj.Multiplier > pricing.MaxMultiplier+0.001 {
		t.Errorf("multiplier %.4f exceeds MaxMultiplier %.4f", adj.Multiplier, pricing.MaxMultiplier)
	}
	if adj.VelocityPerHour <= 0 {
		t.Errorf("VelocityPerHour should be > 0, got %.4f", adj.VelocityPerHour)
	}
	if adj.VolumeSurplus <= 1.0 {
		t.Errorf("VolumeSurplus should be > 1.0 for hot entry, got %.4f", adj.VolumeSurplus)
	}
}

// TestFastLoop_OutsideWindowIgnored verifies that sales older than the velocity
// window do not contribute to the velocity computation.
func TestFastLoop_OutsideWindowIgnored(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	st := newStubState()
	st.inventory = []*exchange.InventoryEntry{
		{EntryID: "entry-stale"},
	}
	// 10 sales, all older than the default 60-minute window.
	for i := range 10 {
		ts := now.Add(-time.Duration(i+2) * time.Hour) // 2h–11h ago
		st.priceHistory = append(st.priceHistory, exchange.PriceRecord{
			EntryID:   "entry-stale",
			SalePrice: 500,
			Timestamp: ts.UnixNano(),
		})
	}

	loop := pricing.NewFastLoop(pricing.FastLoopOptions{
		State: st,
		Now:   func() time.Time { return now },
	})
	loop.Tick()

	// No recent sales — should be treated as cold (discount adjustment written).
	adj, ok := st.adjustments["entry-stale"]
	if !ok {
		// A cold entry gets a discount — that's still an adjustment.
		t.Fatal("expected cold-discount adjustment for stale-sales entry")
	}
	if adj.Multiplier >= 1.0 {
		t.Errorf("stale-sales entry should get discount (< 1.0), got %.4f", adj.Multiplier)
	}
}

// TestFastLoop_AdjustmentExpirySet verifies that each written adjustment has
// a non-zero ExpiresAt set to approximately now + 2 * interval.
func TestFastLoop_AdjustmentExpirySet(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	st := newStubState()
	st.inventory = []*exchange.InventoryEntry{
		{EntryID: "entry-exp"},
	}

	interval := 5 * time.Minute
	expectedTTL := 2 * interval

	loop := pricing.NewFastLoop(pricing.FastLoopOptions{
		State:    st,
		Interval: interval,
		Now:      func() time.Time { return now },
	})
	loop.Tick()

	adj, ok := st.adjustments["entry-exp"]
	if !ok {
		t.Fatal("expected adjustment to be written")
	}
	if adj.ExpiresAt.IsZero() {
		t.Fatal("ExpiresAt should not be zero")
	}
	expectedExpiry := now.Add(expectedTTL)
	diff := adj.ExpiresAt.Sub(expectedExpiry)
	if math.Abs(diff.Seconds()) > 1 {
		t.Errorf("ExpiresAt = %v, want ≈ %v (diff %v)", adj.ExpiresAt, expectedExpiry, diff)
	}
}

// TestFastLoop_ElasticityLowConversionDampensPrice verifies that an entry with
// many previews but low conversion rate produces a sub-1.0 elasticity factor
// that partially dampens the price, even for an entry with moderate velocity.
func TestFastLoop_ElasticityLowConversionDampensPrice(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	st := newStubState()
	st.inventory = []*exchange.InventoryEntry{
		{EntryID: "entry-low-conv"},
	}
	// 20 previews, only 1 purchase → 5% conversion rate (below neutral 20%).
	st.previewCounts["entry-low-conv"] = 20
	st.demandCounts["entry-low-conv"] = 1

	// Moderate velocity: 2 sales in window.
	for i := range 2 {
		ts := now.Add(-time.Duration(i+1) * 10 * time.Minute)
		st.priceHistory = append(st.priceHistory, exchange.PriceRecord{
			EntryID:   "entry-low-conv",
			SalePrice: 500,
			Timestamp: ts.UnixNano(),
		})
	}

	// Compare to a similar entry without preview data (no elasticity dampening).
	st.inventory = append(st.inventory, &exchange.InventoryEntry{EntryID: "entry-no-preview"})
	for i := range 2 {
		ts := now.Add(-time.Duration(i+1) * 10 * time.Minute)
		st.priceHistory = append(st.priceHistory, exchange.PriceRecord{
			EntryID:   "entry-no-preview",
			SalePrice: 500,
			Timestamp: ts.UnixNano(),
		})
	}

	loop := pricing.NewFastLoop(pricing.FastLoopOptions{
		State: st,
		Now:   func() time.Time { return now },
	})
	loop.Tick()

	adjLowConv := st.adjustments["entry-low-conv"]
	adjNoPreview := st.adjustments["entry-no-preview"]

	// Low conversion should produce a lower multiplier than no-preview baseline.
	if adjLowConv.Multiplier >= adjNoPreview.Multiplier {
		t.Errorf("low-conversion entry (multiplier=%.4f) should be dampened vs no-preview (%.4f)",
			adjLowConv.Multiplier, adjNoPreview.Multiplier)
	}
}

// TestFastLoop_HighConversionAmplifies verifies that an entry with high preview
// conversion rate (> 40%) gets a higher multiplier than the baseline.
func TestFastLoop_HighConversionAmplifies(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	st := newStubState()
	st.inventory = []*exchange.InventoryEntry{
		{EntryID: "entry-high-conv"},
		{EntryID: "entry-no-preview"},
	}
	// 10 previews, 6 purchases → 60% conversion (above neutral 20%).
	st.previewCounts["entry-high-conv"] = 10
	st.demandCounts["entry-high-conv"] = 6

	// Same velocity for both entries: 2 sales.
	for _, id := range []string{"entry-high-conv", "entry-no-preview"} {
		for i := range 2 {
			ts := now.Add(-time.Duration(i+1) * 10 * time.Minute)
			st.priceHistory = append(st.priceHistory, exchange.PriceRecord{
				EntryID:   id,
				SalePrice: 500,
				Timestamp: ts.UnixNano(),
			})
		}
	}

	loop := pricing.NewFastLoop(pricing.FastLoopOptions{
		State: st,
		Now:   func() time.Time { return now },
	})
	loop.Tick()

	adjHigh := st.adjustments["entry-high-conv"]
	adjBase := st.adjustments["entry-no-preview"]

	if adjHigh.Multiplier <= adjBase.Multiplier {
		t.Errorf("high-conversion entry (%.4f) should have higher multiplier than no-preview baseline (%.4f)",
			adjHigh.Multiplier, adjBase.Multiplier)
	}
}

// TestFastLoop_MultiplierBounds verifies the hard clamp at [MinMultiplier, MaxMultiplier].
func TestFastLoop_MultiplierBounds(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	st := newStubState()
	st.inventory = []*exchange.InventoryEntry{
		{EntryID: "entry-extreme"},
	}
	// 100 sales in 60 min → extreme surplus.
	for i := range 100 {
		ts := now.Add(-time.Duration(i) * 30 * time.Second)
		st.priceHistory = append(st.priceHistory, exchange.PriceRecord{
			EntryID:   "entry-extreme",
			SalePrice: 500,
			Timestamp: ts.UnixNano(),
		})
	}

	loop := pricing.NewFastLoop(pricing.FastLoopOptions{
		State: st,
		Now:   func() time.Time { return now },
	})
	loop.Tick()

	adj := st.adjustments["entry-extreme"]
	if adj.Multiplier > pricing.MaxMultiplier+0.001 {
		t.Errorf("multiplier %.4f exceeds MaxMultiplier %.4f", adj.Multiplier, pricing.MaxMultiplier)
	}
	if adj.Multiplier < pricing.MinMultiplier-0.001 {
		t.Errorf("multiplier %.4f below MinMultiplier %.4f", adj.Multiplier, pricing.MinMultiplier)
	}
}

// TestFastLoop_EmptyInventoryIsNoop verifies that a tick with no inventory
// writes no adjustments and does not panic.
func TestFastLoop_EmptyInventoryIsNoop(t *testing.T) {
	t.Parallel()
	st := newStubState()
	// No inventory entries.

	loop := pricing.NewFastLoop(pricing.FastLoopOptions{
		State: st,
		Now:   func() time.Time { return time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC) },
	})
	loop.Tick()

	if len(st.adjustments) != 0 {
		t.Errorf("expected 0 adjustments for empty inventory, got %d", len(st.adjustments))
	}
}

// TestFastLoop_RunCancels verifies that Run returns when the context is cancelled.
func TestFastLoop_RunCancels(t *testing.T) {
	t.Parallel()
	st := newStubState()

	loop := pricing.NewFastLoop(pricing.FastLoopOptions{
		State:    st,
		Interval: 10 * time.Millisecond,
		Now:      time.Now,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := loop.Run(ctx)
	if err != context.DeadlineExceeded {
		t.Errorf("expected context.DeadlineExceeded, got %v", err)
	}
}

// TestState_PriceAdjustment_SetGet verifies the SetPriceAdjustment and
// GetPriceAdjustment methods on the real exchange.State.
func TestState_PriceAdjustment_SetGet(t *testing.T) {
	t.Parallel()
	st := exchange.NewState()

	// No adjustment set → returns default 1.0 multiplier.
	adj := st.GetPriceAdjustment("unknown-entry")
	if adj.Multiplier != 1.0 {
		t.Errorf("GetPriceAdjustment(unknown) = %.2f, want 1.0", adj.Multiplier)
	}

	// Set an adjustment.
	st.SetPriceAdjustment("entry-1", exchange.PriceAdjustment{
		Multiplier:      1.5,
		ExpiresAt:       time.Now().Add(10 * time.Minute),
		VelocityPerHour: 2.0,
		VolumeSurplus:   48.0,
	})

	adj = st.GetPriceAdjustment("entry-1")
	if adj.Multiplier != 1.5 {
		t.Errorf("GetPriceAdjustment after set = %.2f, want 1.5", adj.Multiplier)
	}
}

// TestState_PriceAdjustment_ExpiredReturns1 verifies that an expired adjustment
// is treated as 1.0x (no-op).
func TestState_PriceAdjustment_ExpiredReturns1(t *testing.T) {
	t.Parallel()
	st := exchange.NewState()

	st.SetPriceAdjustment("entry-exp", exchange.PriceAdjustment{
		Multiplier: 1.8,
		ExpiresAt:  time.Now().Add(-1 * time.Minute), // already expired
	})

	adj := st.GetPriceAdjustment("entry-exp")
	if adj.Multiplier != 1.0 {
		t.Errorf("expired adjustment should return 1.0, got %.2f", adj.Multiplier)
	}
}

// TestState_AllPriceAdjustments_FiltersExpired verifies that AllPriceAdjustments
// excludes expired entries.
func TestState_AllPriceAdjustments_FiltersExpired(t *testing.T) {
	t.Parallel()
	st := exchange.NewState()

	st.SetPriceAdjustment("active", exchange.PriceAdjustment{
		Multiplier: 1.3,
		ExpiresAt:  time.Now().Add(10 * time.Minute),
	})
	st.SetPriceAdjustment("expired", exchange.PriceAdjustment{
		Multiplier: 1.7,
		ExpiresAt:  time.Now().Add(-1 * time.Minute),
	})

	all := st.AllPriceAdjustments()
	if _, ok := all["active"]; !ok {
		t.Error("active adjustment should be in AllPriceAdjustments")
	}
	if _, ok := all["expired"]; ok {
		t.Error("expired adjustment should not be in AllPriceAdjustments")
	}
	if len(all) != 1 {
		t.Errorf("AllPriceAdjustments len = %d, want 1", len(all))
	}
}

// TestComputePrice_FastLoopAdjustment has been moved to
// pkg/exchange/fast_loop_integration_test.go where newTestHarness is available
// to create a real engine and verify that computePrice applies the fastFactor
// multiplier from exchange state.
