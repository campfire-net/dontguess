package exchange_test

import (
	"math"
	"testing"
	"time"

	"github.com/campfire-net/dontguess/pkg/exchange"
)

// newMinimalEngine returns an engine with no transport or store for unit-testing
// computePrice in isolation.
func newMinimalEngine(t *testing.T) *exchange.Engine {
	t.Helper()
	h := newTestHarness(t)
	eng := h.newEngine()
	return eng
}

// TestComputePrice_ZeroTokenCost verifies that an entry with TokenCost=0 and no
// PutPrice returns the floor price, not 0.
// Regression for dontguess-ffv: zero-cost entries were passing budget filters
// because computePrice returned 0, which is ≤ any budget.
func TestComputePrice_ZeroTokenCost(t *testing.T) {
	t.Parallel()
	eng := newMinimalEngine(t)

	entry := &exchange.InventoryEntry{
		TokenCost: 0,
		PutPrice:  0,
	}
	price := eng.ComputePriceForTest(entry)
	if price <= 0 {
		t.Errorf("computePrice(TokenCost=0, PutPrice=0) = %d, want > 0 (floor price)", price)
	}
	if price != exchange.ComputePriceMinPriceForTest {
		t.Errorf("computePrice(TokenCost=0, PutPrice=0) = %d, want ComputePriceMinPrice=%d",
			price, exchange.ComputePriceMinPriceForTest)
	}
}

// TestComputePrice_NegativeTokenCost verifies that a negative TokenCost also
// returns the floor price.
func TestComputePrice_NegativeTokenCost(t *testing.T) {
	t.Parallel()
	eng := newMinimalEngine(t)

	entry := &exchange.InventoryEntry{
		TokenCost: -1000,
		PutPrice:  0,
	}
	price := eng.ComputePriceForTest(entry)
	if price != exchange.ComputePriceMinPriceForTest {
		t.Errorf("computePrice(TokenCost=-1000, PutPrice=0) = %d, want floor=%d",
			price, exchange.ComputePriceMinPriceForTest)
	}
}

// TestComputePrice_NearMaxInt64TokenCost verifies that a TokenCost near MaxInt64
// does not produce a negative or zero price due to int64 overflow.
// Regression for dontguess-5fh: large TokenCost * 70 / 100 wraps negative.
func TestComputePrice_NearMaxInt64TokenCost(t *testing.T) {
	t.Parallel()
	eng := newMinimalEngine(t)

	entry := &exchange.InventoryEntry{
		TokenCost: math.MaxInt64,
		PutPrice:  0,
	}
	price := eng.ComputePriceForTest(entry)
	if price <= 0 {
		t.Errorf("computePrice(TokenCost=MaxInt64) = %d, want > 0 (no overflow)", price)
	}
}

// TestComputePrice_NearMaxInt64PutPrice verifies that a PutPrice near MaxInt64
// does not overflow when computing the 1.2x markup.
func TestComputePrice_NearMaxInt64PutPrice(t *testing.T) {
	t.Parallel()
	eng := newMinimalEngine(t)

	entry := &exchange.InventoryEntry{
		TokenCost: 1000,
		PutPrice:  math.MaxInt64,
	}
	price := eng.ComputePriceForTest(entry)
	if price <= 0 {
		t.Errorf("computePrice(PutPrice=MaxInt64) = %d, want > 0 (no overflow)", price)
	}
}

// TestComputePrice_SmallTokenCostFloor verifies that a TokenCost so small that
// 70% rounds to 0 still returns the floor price.
func TestComputePrice_SmallTokenCostFloor(t *testing.T) {
	t.Parallel()
	eng := newMinimalEngine(t)

	entry := &exchange.InventoryEntry{
		TokenCost: 1, // 1 * 70 / 100 = 0 in integer division
		PutPrice:  0,
	}
	price := eng.ComputePriceForTest(entry)
	if price < exchange.ComputePriceMinPriceForTest {
		t.Errorf("computePrice(TokenCost=1) = %d, want >= floor=%d",
			price, exchange.ComputePriceMinPriceForTest)
	}
}

// --- Inventory signal tests (dontguess-r13) ---

// TestComputePrice_BasePrice_PutPriceUsedWhenPresent verifies that PutPrice is
// used as the base when present (post-put-accept).
// PutPrice=1000, fresh entry, rep=50 (default) -> repFactor=1.0.
// All other multipliers=1.0. Expected: 1000 * 1.2 = 1200.
func TestComputePrice_BasePrice_PutPriceUsedWhenPresent(t *testing.T) {
	t.Parallel()
	eng := newMinimalEngine(t)

	entry := &exchange.InventoryEntry{
		PutPrice:  1000,
		TokenCost: 5000,
	}
	price := eng.ComputePriceForTest(entry)
	if price != 1200 {
		t.Errorf("computePrice(PutPrice=1000, fresh entry, default rep) = %d, want 1200", price)
	}
}

// TestComputePrice_BasePrice_TokenCostFallback verifies the pre-accept proxy:
// TokenCost * 0.7 when PutPrice=0. TokenCost=1000, rep=50 -> price=700.
func TestComputePrice_BasePrice_TokenCostFallback(t *testing.T) {
	t.Parallel()
	eng := newMinimalEngine(t)

	entry := &exchange.InventoryEntry{
		TokenCost: 1000,
		PutPrice:  0,
	}
	price := eng.ComputePriceForTest(entry)
	if price != 700 {
		t.Errorf("computePrice(TokenCost=1000, PutPrice=0) = %d, want 700", price)
	}
}

// TestComputePrice_AgeFactor_StaleEntryLowerPrice verifies that an entry older
// than 60 days gets the minimum ageFactor (0.5x), halving the price.
// PutPrice=1000 -> base=1200. Stale (>=60d) -> ageFactor=0.5 -> price=600.
func TestComputePrice_AgeFactor_StaleEntryLowerPrice(t *testing.T) {
	t.Parallel()
	eng := newMinimalEngine(t)

	freshEntry := &exchange.InventoryEntry{
		PutPrice:     1000,
		PutTimestamp: time.Now().UnixNano(),
	}
	staleEntry := &exchange.InventoryEntry{
		PutPrice:     1000,
		PutTimestamp: time.Now().Add(-90 * 24 * time.Hour).UnixNano(),
	}

	freshPrice := eng.ComputePriceForTest(freshEntry)
	stalePrice := eng.ComputePriceForTest(staleEntry)

	if stalePrice >= freshPrice {
		t.Errorf("stale entry price %d >= fresh entry price %d: age decay not applied", stalePrice, freshPrice)
	}
	if stalePrice != 600 {
		t.Errorf("computePrice(stale 90d, PutPrice=1000) = %d, want 600 (0.5x ageFactor)", stalePrice)
	}
}

// TestComputePrice_AgeFactor_ZeroTimestampNoDecay verifies that PutTimestamp=0
// (legacy / pending) is treated as brand-new (ageFactor=1.0, no decay).
// PutTimestamp=0 should produce the same price as no-timestamp entries; verified
// by comparing against a clearly stale entry (price must be >= stale price).
func TestComputePrice_AgeFactor_ZeroTimestampNoDecay(t *testing.T) {
	t.Parallel()
	eng := newMinimalEngine(t)

	zeroTimestamp := &exchange.InventoryEntry{
		PutPrice:     1000,
		PutTimestamp: 0,
	}
	staleEntry := &exchange.InventoryEntry{
		PutPrice:     1000,
		PutTimestamp: time.Now().Add(-90 * 24 * time.Hour).UnixNano(),
	}

	zeroPrice := eng.ComputePriceForTest(zeroTimestamp)
	stalePrice := eng.ComputePriceForTest(staleEntry)

	if zeroPrice < stalePrice {
		t.Errorf("PutTimestamp=0 price=%d < stale price=%d: zero timestamp should not decay", zeroPrice, stalePrice)
	}
	// PutTimestamp=0 -> ageFactor=1.0 -> price=1200 (same as no-decay baseline).
	if zeroPrice != 1200 {
		t.Errorf("PutTimestamp=0 price=%d, want 1200 (no decay)", zeroPrice)
	}
}

// TestComputePrice_ContentSize_LargerContentHigherPrice verifies that a 100KB
// entry has a higher price than a zero-size entry.
// PutPrice=1000, size=100KB: sizeBonus=0.30 (cap) -> sizeFactor=1.30 -> price=1560.
func TestComputePrice_ContentSize_LargerContentHigherPrice(t *testing.T) {
	t.Parallel()
	eng := newMinimalEngine(t)

	noSize := &exchange.InventoryEntry{PutPrice: 1000, ContentSize: 0}
	largeContent := &exchange.InventoryEntry{PutPrice: 1000, ContentSize: 102400} // 100 KB

	noSizePrice := eng.ComputePriceForTest(noSize)
	largePrice := eng.ComputePriceForTest(largeContent)

	if largePrice <= noSizePrice {
		t.Errorf("large content price %d <= no-size price %d: size factor not applied", largePrice, noSizePrice)
	}
	if largePrice != 1560 {
		t.Errorf("computePrice(PutPrice=1000, ContentSize=100KB) = %d, want 1560", largePrice)
	}
}

// TestComputePrice_ContentSize_Cap verifies that the content size bonus is
// capped at 30% for content >=100KB.
func TestComputePrice_ContentSize_Cap(t *testing.T) {
	t.Parallel()
	eng := newMinimalEngine(t)

	atCap := &exchange.InventoryEntry{PutPrice: 1000, ContentSize: 102400}   // 100 KB
	overCap := &exchange.InventoryEntry{PutPrice: 1000, ContentSize: 1024000} // 1000 KB

	priceAtCap := eng.ComputePriceForTest(atCap)
	priceOverCap := eng.ComputePriceForTest(overCap)

	if priceAtCap != priceOverCap {
		t.Errorf("size cap not working: atCap=%d, overCap=%d (should be equal)", priceAtCap, priceOverCap)
	}
}

// TestComputePrice_AllSignals_FreshEntryBaseline verifies the canonical baseline:
// PutPrice=1000, PutTimestamp=0 (no decay), no ContentSize, no demand, seller unknown
// (rep defaults to DefaultReputation=50 -> repFactor=1.0). Expected: 1200.
func TestComputePrice_AllSignals_FreshEntryBaseline(t *testing.T) {
	t.Parallel()
	eng := newMinimalEngine(t)

	// PutTimestamp=0 means no age decay (legacy/pending entry treatment).
	entry := &exchange.InventoryEntry{
		PutPrice:     1000,
		PutTimestamp: 0,
	}
	price := eng.ComputePriceForTest(entry)
	if price != 1200 {
		t.Errorf("computePrice(baseline) = %d, want 1200", price)
	}
}

// --- Compression tier multiplier tests (dontguess-cb5) ---

// TestComputePrice_Tier_HotPremium verifies that a "hot" entry commands a 1.5x
// tier multiplier on top of the base price.
// PutPrice=1000 -> base=1200 (1.2x margin). Tier=hot -> 1.2 * 1.5 = 1800.
func TestComputePrice_Tier_HotPremium(t *testing.T) {
	t.Parallel()
	eng := newMinimalEngine(t)

	entry := &exchange.InventoryEntry{
		PutPrice:        1000,
		CompressionTier: "hot",
	}
	price := eng.ComputePriceForTest(entry)
	if price != 1800 {
		t.Errorf("computePrice(PutPrice=1000, tier=hot) = %d, want 1800 (1.2x margin * 1.5x tier)", price)
	}
}

// TestComputePrice_Tier_WarmPremium verifies that a "warm" entry commands a 1.2x
// tier multiplier on top of the base price.
// PutPrice=1000 -> base=1200. Tier=warm -> 1200 * 1.2 = 1440.
func TestComputePrice_Tier_WarmPremium(t *testing.T) {
	t.Parallel()
	eng := newMinimalEngine(t)

	entry := &exchange.InventoryEntry{
		PutPrice:        1000,
		CompressionTier: "warm",
	}
	price := eng.ComputePriceForTest(entry)
	if price != 1440 {
		t.Errorf("computePrice(PutPrice=1000, tier=warm) = %d, want 1440 (1.2x margin * 1.2x tier)", price)
	}
}

// TestComputePrice_Tier_ColdNoChange verifies that a "cold" entry has no tier
// premium (1.0x multiplier — same as unset tier).
// PutPrice=1000 -> base=1200. Tier=cold -> 1200 * 1.0 = 1200.
func TestComputePrice_Tier_ColdNoChange(t *testing.T) {
	t.Parallel()
	eng := newMinimalEngine(t)

	entry := &exchange.InventoryEntry{
		PutPrice:        1000,
		CompressionTier: "cold",
	}
	price := eng.ComputePriceForTest(entry)
	if price != 1200 {
		t.Errorf("computePrice(PutPrice=1000, tier=cold) = %d, want 1200 (no tier premium)", price)
	}
}

// TestComputePrice_Tier_UnsetNoChange verifies that an unset tier ("") has no
// premium — same price as a "cold" entry.
// PutPrice=1000 -> base=1200. Tier="" -> 1200 * 1.0 = 1200.
func TestComputePrice_Tier_UnsetNoChange(t *testing.T) {
	t.Parallel()
	eng := newMinimalEngine(t)

	entry := &exchange.InventoryEntry{
		PutPrice:        1000,
		CompressionTier: "",
	}
	price := eng.ComputePriceForTest(entry)
	if price != 1200 {
		t.Errorf("computePrice(PutPrice=1000, tier=\"\") = %d, want 1200 (unset tier = no premium)", price)
	}
}

// TestComputePrice_Tier_SameTokenCostDifferentTiersProduceDifferentPrices is the
// canonical done-condition test: same TokenCost at different tiers must produce
// different prices, and hot > warm > cold == unset.
// TokenCost=1000 -> base=700 (0.7x seller share). hot=1050, warm=840, cold=700.
func TestComputePrice_Tier_SameTokenCostDifferentTiersProduceDifferentPrices(t *testing.T) {
	t.Parallel()
	eng := newMinimalEngine(t)

	hot := &exchange.InventoryEntry{TokenCost: 1000, CompressionTier: "hot"}
	warm := &exchange.InventoryEntry{TokenCost: 1000, CompressionTier: "warm"}
	cold := &exchange.InventoryEntry{TokenCost: 1000, CompressionTier: "cold"}
	unset := &exchange.InventoryEntry{TokenCost: 1000, CompressionTier: ""}

	hotPrice := eng.ComputePriceForTest(hot)
	warmPrice := eng.ComputePriceForTest(warm)
	coldPrice := eng.ComputePriceForTest(cold)
	unsetPrice := eng.ComputePriceForTest(unset)

	if hotPrice <= warmPrice {
		t.Errorf("hot price %d should be > warm price %d", hotPrice, warmPrice)
	}
	if warmPrice <= coldPrice {
		t.Errorf("warm price %d should be > cold price %d", warmPrice, coldPrice)
	}
	if coldPrice != unsetPrice {
		t.Errorf("cold price %d should equal unset price %d (both 1.0x)", coldPrice, unsetPrice)
	}

	// Exact values: TokenCost=1000, PutPrice=0 -> base=700 (0.7x). rep=50 -> repFactor=1.0.
	// hot:  700 * 1.5 = 1050
	// warm: 700 * 1.2 = 840
	// cold: 700 * 1.0 = 700
	if hotPrice != 1050 {
		t.Errorf("hot price = %d, want 1050 (700 * 1.5)", hotPrice)
	}
	if warmPrice != 840 {
		t.Errorf("warm price = %d, want 840 (700 * 1.2)", warmPrice)
	}
	if coldPrice != 700 {
		t.Errorf("cold price = %d, want 700 (700 * 1.0)", coldPrice)
	}
}
