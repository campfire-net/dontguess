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

// PreviewsByEntryForTest returns a snapshot of the previewsByEntry map for
// white-box testing of the preview-request state apply logic.
func (s *State) PreviewsByEntryForTest() map[string]map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]map[string]string, len(s.previewsByEntry))
	for entryID, byBuyer := range s.previewsByEntry {
		cp := make(map[string]string, len(byBuyer))
		for buyer, matchID := range byBuyer {
			cp[buyer] = matchID
		}
		out[entryID] = cp
	}
	return out
}

// PreviewCountByMatchForTest returns a snapshot of the previewCountByMatch map.
func (s *State) PreviewCountByMatchForTest() map[string]int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]int, len(s.previewCountByMatch))
	for k, v := range s.previewCountByMatch {
		out[k] = v
	}
	return out
}

// PreviewRequestToMatchForTest returns a snapshot of the previewRequestToMatch map.
func (s *State) PreviewRequestToMatchForTest() map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]string, len(s.previewRequestToMatch))
	for k, v := range s.previewRequestToMatch {
		out[k] = v
	}
	return out
}

// PreviewToMatchForTest returns a snapshot of the previewToMatch map.
func (s *State) PreviewToMatchForTest() map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]string, len(s.previewToMatch))
	for k, v := range s.previewToMatch {
		out[k] = v
	}
	return out
}

// DeleteInventoryEntryForTest removes an entry from the inventory map.
// Used only in tests that simulate expiry-between-deliver-and-dispute scenarios.
func (s *State) DeleteInventoryEntryForTest(entryID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.inventory, entryID)
}
