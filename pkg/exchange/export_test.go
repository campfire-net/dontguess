package exchange

import "github.com/3dl-dev/dontguess/pkg/matching"

// SetMatchIndexForTest replaces the engine's semantic match index.
// Used in tests that need to control which entries win the semantic match.
func (e *Engine) SetMatchIndexForTest(idx *matching.Index) {
	e.matchIndex = idx
}

// DispatchForTest exposes the engine's dispatch method for use in tests.
// It allows tests to trigger specific handler paths (settle, dispute) without
// running the full engine event loop.
func (e *Engine) DispatchForTest(msg *Message) error {
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

// InjectInventoryEntryForTest inserts an entry directly into the inventory map,
// bypassing the normal put → put-accept flow. Used in unit tests that need to
// set up derivative (CompressedFrom) relationships without running the full
// assign-accept pipeline.
func (s *State) InjectInventoryEntryForTest(entry *InventoryEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inventory[entry.EntryID] = entry
}

// AssignByIDForTest exposes the assignByID map for white-box testing of claim
// expiry state (ClaimExpiresAt, ClaimantKey, Status).
func (s *State) AssignByIDForTest() map[string]*AssignRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]*AssignRecord, len(s.assignByID))
	for k, v := range s.assignByID {
		cp := *v
		out[k] = &cp
	}
	return out
}

// SweepExpiredClaimsForTest exposes ExpireStaleClaims for unit tests that need
// to inspect which claims are eligible for expiry without triggering engine I/O.
func (s *State) SweepExpiredClaimsForTest() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ExpireStaleClaims()
}

// CoOccurrenceCountForTest returns the co-occurrence count between two entries.
// Returns 0 if no data exists. Used for white-box testing of UpdateCoOccurrence.
func (s *State) CoOccurrenceCountForTest(entryA, entryB string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, ok := s.coOccurrence[entryA]
	if !ok {
		return 0
	}
	return m.counts[entryB]
}

// RecordBuyerSettlementForTest exposes recordBuyerSettlement for unit tests.
func (e *Engine) RecordBuyerSettlementForTest(buyerKey, entryID string) {
	e.recordBuyerSettlement(buyerKey, entryID)
}

// StagePredictionsForTest exposes stagePredictions for unit tests.
func (e *Engine) StagePredictionsForTest(settledEntryID string) {
	e.stagePredictions(settledEntryID)
}
