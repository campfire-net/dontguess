package exchange

import "github.com/campfire-net/campfire/pkg/store"

// DispatchForTest exposes the engine's dispatch method for use in tests.
// It allows tests to trigger specific handler paths (settle, dispute) without
// running the full engine event loop.
func (e *Engine) DispatchForTest(msg *store.MessageRecord) error {
	return e.dispatch(msg)
}

// ComputePriceForTest exposes computePrice for unit testing.
func (e *Engine) ComputePriceForTest(entry *InventoryEntry) int64 {
	return e.computePrice(entry)
}

// ComputePriceMinPriceForTest exposes the floor price constant for assertion.
const ComputePriceMinPriceForTest = computePriceMinPrice
