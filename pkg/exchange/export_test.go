package exchange

import "github.com/campfire-net/campfire/pkg/store"

// DispatchForTest exposes the engine's dispatch method for use in tests.
// It allows tests to trigger specific handler paths (settle, dispute) without
// running the full engine event loop.
func (e *Engine) DispatchForTest(msg *store.MessageRecord) error {
	return e.dispatch(msg)
}

// SetMarshalFuncForTest replaces the engine's JSON marshal function for tests
// that need to inject marshal failures. Pass nil to restore the default (json.Marshal).
func (e *Engine) SetMarshalFuncForTest(fn func(v any) ([]byte, error)) {
	e.marshalFunc = fn
}

// ComputePriceForTest exposes computePrice for unit testing.
func (e *Engine) ComputePriceForTest(entry *InventoryEntry) int64 {
	return e.computePrice(entry)
}

// ComputePriceMinPriceForTest exposes the floor price constant for assertion.
const ComputePriceMinPriceForTest = computePriceMinPrice
// ShortKeyForTest exposes shortKey for white-box testing.
func ShortKeyForTest(key string) string { return shortKey(key) }
