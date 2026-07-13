package exchange

import (
	"time"

	"github.com/campfire-net/dontguess/pkg/matching"
)

// AllEntryBehavioralSignals returns a snapshot map of per-entry behavioral signals
// for all inventory entries that have at least one signal (consume, buyer, or deliver).
// Used by the exchange engine to update the matching index's behavioral signals
// after state changes (settle:complete, consume emission, settle:deliver).
//
// The returned map is safe to use without holding the State lock — it is a copy.
// Entries with zero signals on all fields are omitted.
func (s *State) AllEntryBehavioralSignals() map[string]matching.BehavioralSignals {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make(map[string]matching.BehavioralSignals)

	// Collect consume counts.
	for entryID, count := range s.entryConsumeCount {
		if count > 0 {
			sig := out[entryID]
			sig.ConsumeCount = count
			out[entryID] = sig
		}
	}

	// Collect distinct buyer counts from per-seller EntryBuyerMap.
	for _, stats := range s.sellers {
		for entryID, buyers := range stats.EntryBuyerMap {
			if len(buyers) == 0 {
				continue
			}
			sig := out[entryID]
			sig.DistinctBuyerCount += len(buyers)
			out[entryID] = sig
		}
	}

	// Collect deliver counts (dontguess-046): feeds the false-positive demotion signal.
	for entryID, count := range s.entryDeliverCount {
		if count > 0 {
			sig := out[entryID]
			sig.DeliverCount = count
			out[entryID] = sig
		}
	}

	// Note: no zero-signal cleanup needed. Each populating loop above guards
	// on count > 0 / len(buyers) > 0 before inserting, so every entry in out
	// has at least one non-zero field by construction.

	return out
}

// ExpiryCandidateReport describes a single inventory entry flagged as a
// false-positive expiry candidate by the operator-facing report.
type ExpiryCandidateReport struct {
	// EntryID is the inventory entry's ID.
	EntryID string
	// DeliverCount is the number of times the entry was delivered.
	DeliverCount int
	// ConsumeCount is the number of times the entry was consumed.
	ConsumeCount int
	// Ratio is DeliverCount / max(ConsumeCount, 1). High ratio = strong false-positive signal.
	Ratio float64
}

// ExpiryCandidates returns an operator-facing report of inventory entries that
// are flagged as false-positive expiry candidates based on a sustained high
// deliver-without-consume ratio.
//
// An entry is a candidate when:
//   - DeliverCount >= matching.FalsePositiveWindowMin (sustained pattern, not a
//     single miss), AND
//   - ratio = DeliverCount / max(ConsumeCount, 1) >= matching.FalsePositiveRatioThreshold
//
// This is a READ-ONLY report. The exchange does NOT autonomously expire or delete
// entries based on this signal — the operator decides what action to take.
// Operators may use this list to manually expire entries, re-price them, or
// request re-validation via an assign task.
//
// Thread-safe.
func (s *State) ExpiryCandidates() []ExpiryCandidateReport {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Build a combined deliver+consume view.
	type counts struct {
		deliver int
		consume int
	}
	combined := make(map[string]counts)
	for entryID, dc := range s.entryDeliverCount {
		if dc > 0 {
			c := combined[entryID]
			c.deliver = dc
			combined[entryID] = c
		}
	}
	for entryID, cc := range s.entryConsumeCount {
		if cc > 0 {
			c := combined[entryID]
			c.consume = cc
			combined[entryID] = c
		}
	}

	var candidates []ExpiryCandidateReport
	for entryID, c := range combined {
		// dontguess-1856: scale the effective deliver count fed into the
		// false-positive-expiry criterion by the personal completion rate of
		// the buyers who actually abandoned THIS entry. A low weight (buyers
		// are chronic near-zero-completion griefers) shrinks the effective
		// deliver-without-consume ratio so a funded griefer hammering one
		// entry cannot poison an otherwise-good entry's signal. A weight near
		// 1.0 (abandoning buyers otherwise complete normally elsewhere)
		// leaves the ratio at full strength — that pattern is real signal
		// that the entry itself is stale/bad. entryConsumeCount (the
		// existing per-entry aggregate) is untouched; only the value handed
		// to IsFalsePositiveExpiry is scaled.
		weight := s.entryDeliverAbandonWeightLocked(entryID)
		effectiveDeliver := int(float64(c.deliver)*weight + 0.5)
		sig := matching.BehavioralSignals{
			DeliverCount: effectiveDeliver,
			ConsumeCount: c.consume,
		}
		if matching.IsFalsePositiveExpiry(sig) {
			consumeDenom := c.consume
			if consumeDenom < 1 {
				consumeDenom = 1
			}
			candidates = append(candidates, ExpiryCandidateReport{
				EntryID:      entryID,
				DeliverCount: c.deliver,
				ConsumeCount: c.consume,
				Ratio:        float64(c.deliver) / float64(consumeDenom),
			})
		}
	}

	return candidates
}

// entryDeliverAbandonWeightLocked returns, in [0.0, 1.0], the weight used to
// scale entryID's effective deliver-without-consume ratio for the
// false-positive-expiry criterion (dontguess-1856).
//
// It is the deliver-count-weighted average, across every buyer who received a
// settle(deliver) for entryID, of that buyer's EXTERNAL personal completion
// rate: (buyerConsumeCount[buyer] - entryConsumeBuyerCount[entryID][buyer]) /
// (buyerDeliverCount[buyer] - entryDeliverBuyerCount[entryID][buyer]) — i.e.
// the buyer's track record on every OTHER entry, deliberately excluding this
// entry's own contribution. A buyer with NO external deliveries recorded
// (their entire footprint is this one entry — e.g. a single-episode
// abandonment, or a first-time buyer) contributes a neutral rate of 1.0: with
// zero external observations there is no basis to call them a "chronic"
// never-completer, so the entry's own signal is judged unweighted. This
// self-exclusion is deliberate — without it, a lone, single-episode
// abandoner would always compute an inclusive rate of 0 (delivered here,
// completed nowhere) and every such entry would suppress itself, which would
// break the dontguess-659 griefing regression (a single-episode attacker
// hammering ONE competitor entry must still surface the entry as an
// operator-facing expiry candidate — only an ESTABLISHED pattern across
// OTHER entries should discount the signal).
//
// Intuition:
//   - If entryID's abandons are dominated by a buyer who has an ESTABLISHED
//     track record of never completing anything on OTHER entries (a chronic
//     griefer), that external rate is near 0, so the weight is near 0 and the
//     effective deliver count — and thus the ratio — is scaled down,
//     preventing a false-positive-expiry flag.
//   - If entryID's abandons come from a broad set of buyers who otherwise
//     complete normally on OTHER entries, their external rate is near 1, so
//     the weight stays near 1 and the entry's ratio is judged at full
//     strength — that is real signal the entry itself is stale/low quality.
//   - If a buyer has no external track record at all, the weight for their
//     contribution is neutral (1.0) — absence of evidence is not evidence of
//     chronic abandonment.
//
// No data (entry has no recorded deliver-buyer association, e.g. it predates
// this fold or was replayed from an older log) returns 1.0 — no scaling,
// falling back to the pre-existing unweighted behavior.
//
// Caller must hold s.mu (read lock is sufficient) — invoked from
// ExpiryCandidates, which holds s.mu.RLock() for its whole body.
func (s *State) entryDeliverAbandonWeightLocked(entryID string) float64 {
	buyers := s.entryDeliverBuyerCount[entryID]
	if len(buyers) == 0 {
		return 1.0
	}
	var weightedSum, totalCount float64
	for buyer, cnt := range buyers {
		extDeliver := s.buyerDeliverCount[buyer] - cnt
		extConsume := s.buyerConsumeCount[buyer] - s.entryConsumeBuyerCount[entryID][buyer]

		rate := 1.0
		if extDeliver > 0 {
			rate = float64(extConsume) / float64(extDeliver)
			if rate > 1.0 {
				rate = 1.0
			}
			if rate < 0 {
				rate = 0
			}
		}
		weightedSum += rate * float64(cnt)
		totalCount += float64(cnt)
	}
	if totalCount == 0 {
		return 1.0
	}
	return weightedSum / totalCount
}

// UpdateCoOccurrence records that entryA and entryB co-occurred in the same
// buyer session (both were settled by the same buyer within a session window).
// The co-occurrence is bidirectional: both A→B and B→A are updated.
// Increments by 1 per call; the bounded map evicts the lowest-count peer when
// at capacity (K=20). Self-pairs (entryA == entryB) are silently ignored.
// Thread-safe.
func (s *State) UpdateCoOccurrence(entryA, entryB string) {
	if entryA == "" || entryB == "" || entryA == entryB {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// A → B
	if s.coOccurrence[entryA] == nil {
		s.coOccurrence[entryA] = newCoOccurrenceMap()
	}
	s.coOccurrence[entryA].increment(entryB)
	// B → A
	if s.coOccurrence[entryB] == nil {
		s.coOccurrence[entryB] = newCoOccurrenceMap()
	}
	s.coOccurrence[entryB].increment(entryA)
}

// PredictNext returns the top-K entry IDs most likely to be needed after entryID,
// based on co-occurrence patterns from prior settled buyer sessions. Returns at
// most CoOccurrenceK results (or fewer if less data is available). Returns nil if
// no co-occurrence data exists for entryID.
// Thread-safe.
func (s *State) PredictNext(entryID string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, ok := s.coOccurrence[entryID]
	if !ok {
		return nil
	}
	return m.topK(CoOccurrenceK)
}

// OpenPredictionAssignsForEntry returns the count of open (unclaimed or actively
// claimed) brokered-match assigns whose entry_id matches entryID and whose
// DeadlineAt is in the future (or zero). Used to enforce the MaxPredictionFanout
// limit when pre-staging standing assigns.
// Caller must hold s.mu (at least read lock).
func (s *State) openPredictionAssignsForEntry(entryID string) int {
	now := time.Now().UTC()
	count := 0
	for _, rec := range s.assignsByEntry[entryID] {
		if rec.TaskType != "brokered-match" {
			continue
		}
		if rec.Status == AssignAccepted || rec.Status == AssignRejected || rec.Status == AssignPaid || rec.Status == AssignExpired {
			continue
		}
		// Expired standing assigns don't count toward the fanout limit.
		if !rec.DeadlineAt.IsZero() && now.After(rec.DeadlineAt) {
			continue
		}
		count++
	}
	return count
}

// OpenPredictionAssignsForEntry is the exported thread-safe version for the engine.
// Thread-safe.
func (s *State) OpenPredictionAssignsForEntry(entryID string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.openPredictionAssignsForEntry(entryID)
}

// StalePredictionAssigns returns the assign IDs of AssignOpen brokered-match
// assigns whose DeadlineAt is non-zero and has passed. These are safe to cancel
// (they will not be claimed by any worker after their deadline).
// Caller must hold s.mu (read lock is sufficient).
func (s *State) StalePredictionAssigns() []string {
	now := time.Now().UTC()
	var stale []string
	for id, rec := range s.assignByID {
		if rec.TaskType != "brokered-match" {
			continue
		}
		if rec.Status != AssignOpen {
			continue
		}
		if rec.DeadlineAt.IsZero() {
			continue
		}
		if now.After(rec.DeadlineAt) {
			stale = append(stale, id)
		}
	}
	return stale
}
