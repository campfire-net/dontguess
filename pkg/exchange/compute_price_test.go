package exchange_test

import (
	"math"
	"testing"

	"github.com/3dl-dev/dontguess/pkg/exchange"
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
