package exchange

import (
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// Inventory returns a snapshot of all live (accepted, non-expired) inventory entries.
// Each entry is a copy — callers may not mutate returned entries; changes would not
// be reflected in state and could not be persisted.
func (s *State) Inventory() []*InventoryEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*InventoryEntry, 0, len(s.inventory))
	for _, e := range s.inventory {
		if !e.IsExpired() {
			cp := *e // shallow copy of the struct
			// Deep-copy the Domains slice so callers cannot mutate internal state.
			if len(e.Domains) > 0 {
				cp.Domains = make([]string, len(e.Domains))
				copy(cp.Domains, e.Domains)
			}
			out = append(out, &cp)
		}
	}
	return out
}

// ActiveOrders returns a snapshot of all unfulfilled, non-expired buy orders.
func (s *State) ActiveOrders() []*ActiveOrder {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*ActiveOrder, 0)
	for id, o := range s.activeOrders {
		if _, matched := s.matchedOrders[id]; matched {
			continue
		}
		if o.IsExpired() {
			continue
		}
		out = append(out, o)
	}
	return out
}

// PendingPuts returns a copy of all put messages waiting for operator acceptance.
func (s *State) PendingPuts() []*InventoryEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*InventoryEntry, 0, len(s.pendingPuts))
	for _, e := range s.pendingPuts {
		out = append(out, e)
	}
	return out
}

// HoldPutForReview marks a put message ID as held for review.
// Held puts remain in pendingPuts — no campfire message is emitted.
// The auto-accept loop calls this when entry.TokenCost > max.
// Thread-safe.
func (s *State) HoldPutForReview(putMsgID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.heldForReview[putMsgID] = struct{}{}
}

// PruneHeldForReview removes IDs from heldForReview that are no longer present
// in pendingPuts (i.e., they were accepted or rejected by the operator).
// The auto-accept loop calls this once per tick, passing the current pending ID set.
// Thread-safe.
func (s *State) PruneHeldForReview(pendingIDs map[string]struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id := range s.heldForReview {
		if _, ok := pendingIDs[id]; !ok {
			delete(s.heldForReview, id)
		}
	}
}

// PutsHeldForReview returns the subset of pendingPuts whose IDs are in heldForReview.
// The returned slice is a copy; callers may not mutate the entries.
// Thread-safe (read lock only; no mutation).
func (s *State) PutsHeldForReview() []*InventoryEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*InventoryEntry, 0, len(s.heldForReview))
	for id := range s.heldForReview {
		if entry, ok := s.pendingPuts[id]; ok {
			out = append(out, entry)
		}
	}
	return out
}

// PriceHistory returns a copy of all recorded price events.
func (s *State) PriceHistory() []PriceRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]PriceRecord, len(s.priceHistory))
	copy(out, s.priceHistory)
	return out
}

// SellerReputation returns the derived reputation score (0-100) for a seller key.
// Returns DefaultReputation for unknown sellers.
func (s *State) SellerReputation(sellerKey string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if stats, ok := s.sellers[sellerKey]; ok {
		return stats.Reputation()
	}
	return DefaultReputation
}

// LowConversionEntries returns entry IDs where conversion rate is below maxRate
// and preview count is at or above minPreviews. Used by Layer 0 gate (dontguess-5iz)
// to identify entries with poor preview-to-purchase ratios.
func (s *State) LowConversionEntries(minPreviews int, maxRate float64) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []string
	for entryID, previews := range s.entryPreviewCount {
		if previews < minPreviews {
			continue
		}
		conversions := s.entryConversionCount[entryID]
		rate := float64(conversions) / float64(previews)
		if rate < maxRate {
			out = append(out, entryID)
		}
	}
	return out
}

// GetInventoryEntry returns a single inventory entry by ID, or nil if not found.
func (s *State) GetInventoryEntry(entryID string) *InventoryEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.inventory[entryID]
}

// IsOrderMatched returns true if a buy order has been fulfilled by a match.
func (s *State) IsOrderMatched(orderID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.matchedOrders[orderID]
	return ok
}

// IsMatchAccepted returns true if a match (identified by its message ID) has
// an active buyer-accept that has not been subsequently rejected.
func (s *State) IsMatchAccepted(matchMsgID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.acceptedOrders[matchMsgID]
	return ok
}

// SetEntryProvenanceLevel records the seller's provenance level for an inventory
// entry. Call this after a put-accept with the seller's current level. The level
// is stored as an int (0=anonymous … 3=present) to avoid coupling state.go to
// the provenance package.
//
// If the entry does not exist (not yet in inventory), this is a no-op.
func (s *State) SetEntryProvenanceLevel(entryID string, level int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if entry, ok := s.inventory[entryID]; ok {
		entry.AcceptedProvenanceLevel = level
	}
}

// FlagEntryForRevalidation marks a single inventory entry NeedsRevalidation so
// findCandidates withholds it (dontguess-d53 Seam D). No-op if the entry is not
// in live inventory. Thread-safe.
func (s *State) FlagEntryForRevalidation(entryID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if entry, ok := s.inventory[entryID]; ok {
		entry.NeedsRevalidation = true
	}
}

// FlagSellerEntriesForRevalidation marks every live inventory entry belonging to
// sellerKey NeedsRevalidation (dontguess-d53 Seam C — runtime de-allowlisting).
// Returns the number of entries flagged. Thread-safe.
func (s *State) FlagSellerEntriesForRevalidation(sellerKey string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, entry := range s.inventory {
		if entry.SellerKey == sellerKey {
			entry.NeedsRevalidation = true
			n++
		}
	}
	return n
}

// EntryNeedsRevalidation returns true if the given entry has been flagged for
// re-validation due to a seller provenance downgrade.
func (s *State) EntryNeedsRevalidation(entryID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if entry, ok := s.inventory[entryID]; ok {
		return entry.NeedsRevalidation
	}
	return false
}

// SellerSmallContentRefundCount returns the number of small-content auto-refunds
// recorded against the seller. Each refund applies a -3 reputation penalty.
// Returns 0 for unknown sellers.
func (s *State) SellerSmallContentRefundCount(sellerKey string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if stats, ok := s.sellers[sellerKey]; ok {
		return stats.SmallContentRefundCount
	}
	return 0
}

// SmallContentDisputeCount returns the number of small-content auto-refund
// disputes filed against the given entry ID. Returns 0 if none.
func (s *State) SmallContentDisputeCount(entryID string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.smallContentDisputes[entryID]
}

// IsMatchDelivered returns true if a match (identified by its message ID)
// has received a settle(deliver) from the exchange operator.
func (s *State) IsMatchDelivered(matchMsgID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.deliveredOrders[matchMsgID]
	return ok
}

// SellerKeyForDeliver derives the seller's public key from the antecedent chain
// starting at a deliver message ID. The chain is:
//
//	deliver → match (via deliverToMatch)
//	match   → entry (via matchToEntry)
//	entry   → seller (via inventory[entry].SellerKey)
//
// This is the authoritative, untainted way to find the seller for residual
// payment — never trust a buyer-supplied seller_key field in the settle payload.
// Returns ("", false) if any link in the chain is missing.
// MatchForDeliver returns the match message ID that a deliver message references.
// Used by the settle(complete) handler to locate the reservation created at
// buyer-accept time via the engine's matchToReservation map.
func (s *State) MatchForDeliver(deliverMsgID string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	// The buyer e-tags the operator deliver's WIRE id; map it to the store id the
	// deliverToMatch index is keyed by (dontguess-55c GAP 1; identity when absent).
	deliverMsgID = s.resolveAlias(deliverMsgID)
	matchMsgID := s.deliverToMatch[deliverMsgID]
	if matchMsgID == "" {
		return "", false
	}
	return matchMsgID, true
}

func (s *State) SellerKeyForDeliver(deliverMsgID string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	// Buyer-supplied deliver WIRE id → store id (dontguess-55c GAP 1).
	deliverMsgID = s.resolveAlias(deliverMsgID)
	matchMsgID := s.deliverToMatch[deliverMsgID]
	if matchMsgID == "" {
		return "", false
	}
	entryID := s.matchToEntry[matchMsgID]
	if entryID == "" {
		return "", false
	}
	entry, ok := s.inventory[entryID]
	if !ok {
		return "", false
	}
	return entry.SellerKey, true
}

// EntryForDeliver derives the inventory entry from the antecedent chain starting
// at a deliver message ID. The chain is:
//
//	deliver → match (via deliverToMatch)
//	match   → entry (via matchToEntry)
//	entry   → InventoryEntry (via inventory)
//
// Returns a shallow copy of the entry and true on success.
// Returns (nil, false) if any link in the chain is missing.
func (s *State) EntryForDeliver(deliverMsgID string) (*InventoryEntry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	// Buyer-supplied deliver WIRE id → store id (dontguess-55c GAP 1).
	deliverMsgID = s.resolveAlias(deliverMsgID)
	matchMsgID := s.deliverToMatch[deliverMsgID]
	if matchMsgID == "" {
		return nil, false
	}
	entryID := s.matchToEntry[matchMsgID]
	if entryID == "" {
		return nil, false
	}
	entry, ok := s.inventory[entryID]
	if !ok {
		return nil, false
	}
	cp := *entry
	return &cp, true
}

// EntryDemandCount returns the number of distinct buyers who have completed a
// purchase of the given entry. Used as a demand signal for pricing.
// Returns 0 for unknown entries.
func (s *State) EntryDemandCount(entryID string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry, ok := s.inventory[entryID]
	if !ok {
		return 0
	}
	stats, ok := s.sellers[entry.SellerKey]
	if !ok {
		return 0
	}
	buyers, ok := stats.EntryBuyerMap[entryID]
	if !ok {
		return 0
	}
	return len(buyers)
}

// EntryPreviewCount returns the number of previews served for the given entry.
// Returns 0 for unknown entries.
func (s *State) EntryPreviewCount(entryID string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.entryPreviewCount[entryID]
}

// SetPriceAdjustment writes a dynamic price adjustment for an entry.
// Overwrites any prior adjustment for the same entry.
// Called by the fast pricing loop after each computation cycle.
func (s *State) SetPriceAdjustment(entryID string, adj PriceAdjustment) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.priceAdjustments[entryID] = adj
}

// GetPriceAdjustment returns the active price adjustment for an entry.
// Returns a 1.0x adjustment if none exists or the stored adjustment has expired.
func (s *State) GetPriceAdjustment(entryID string) PriceAdjustment {
	s.mu.RLock()
	defer s.mu.RUnlock()
	adj, ok := s.priceAdjustments[entryID]
	if !ok || adj.IsExpired() || adj.Multiplier <= 0 {
		return PriceAdjustment{Multiplier: 1.0}
	}
	return adj
}

// AllPriceAdjustments returns a snapshot of all active (non-expired) price adjustments.
// Used by the medium loop and for diagnostics.
func (s *State) AllPriceAdjustments() map[string]PriceAdjustment {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]PriceAdjustment, len(s.priceAdjustments))
	for id, adj := range s.priceAdjustments {
		if !adj.IsExpired() && adj.Multiplier > 0 {
			out[id] = adj
		}
	}
	return out
}

// SetDebtorScore writes the debtor priority score for an agent.
//
// score must be in [0.0, 1.0]:
//   - 1.0 = no outstanding debt (full match priority)
//   - 0.0 = maximum debt / chronic defaulter (lowest priority)
//
// This is called externally (e.g. by the engine after scrip signals a loan
// state change) rather than derived from the campfire log, so it survives
// Replay and is not reset. It is intentionally NOT imported from pkg/scrip to
// avoid a cross-package dependency — callers derive the score from loan records
// and inject it via this hook.
//
// Thread-safe.
func (s *State) SetDebtorScore(agentKey string, score float64) {
	if score < 0 {
		score = 0
	}
	if score > 1 {
		score = 1
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.debtorScores[agentKey] = score
}

// DebtorScore returns the debtor priority score for an agent in [0.0, 1.0].
// Returns 1.0 (full priority) for agents with no recorded score, since absence
// of debt signal means no known debt.
//
// 1.0 = no debt / full priority
// 0.0 = maximum debt / lowest priority
//
// Thread-safe.
func (s *State) DebtorScore(agentKey string) float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	score, ok := s.debtorScores[agentKey]
	if !ok {
		return 1.0 // no debt signal recorded → full priority
	}
	return score
}

// TaskCompletionRate returns the fraction of buyer-accepted orders that have
// reached the settle(complete) state. This is the Layer 0 correctness metric
// used by the value stack gate: a regression here (rate drops significantly)
// causes the stack to reject any pending loop adjustments.
//
// Returns 1.0 (perfect) when there are no accepted orders yet (cold start).
// The numerator is completed settlements; the denominator is all accepted orders.
func (s *State) TaskCompletionRate() float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	accepted := len(s.acceptedOrders)
	if accepted == 0 {
		return 1.0 // cold start: no accepted orders yet, treat as healthy
	}
	completed := len(s.completedSettlements)
	return float64(completed) / float64(accepted)
}

// RegisterBrokerMatch marks a match message ID as having been produced by a
// brokered-match assign. Called by the engine when it accepts an
// exchange:assign-complete for a task_type="brokered-match" assign.
//
// This is NOT reset on Replay — the brokerMatchIDs set is externally managed
// by the engine (not derived from the campfire log). The engine must re-register
// all brokered match IDs after a Replay to ensure the counters are correct.
// Thread-safe.
func (s *State) RegisterBrokerMatch(matchMsgID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.brokerMatchIDs[matchMsgID] = struct{}{}
}

// BrokeredMatchCompletionRate returns the fraction of brokered-match accepted
// orders that have reached settle(complete). Used by the Layer 0 bootstrap gate
// to measure brokered matching correctness independently from inline matching.
//
// Returns 1.0 when there are no brokered accepted orders yet (cold start).
// The numerator is brokered completions; the denominator is brokered accepted orders.
func (s *State) BrokeredMatchCompletionRate() float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.brokeredAcceptedOrders == 0 {
		return 1.0 // cold start: no brokered orders yet
	}
	return float64(s.brokeredCompletions) / float64(s.brokeredAcceptedOrders)
}

// BrokeredCompletionCount returns the raw count of brokered-match
// settle(complete) messages processed. Used by the Layer 0 bootstrap gate
// to determine whether the threshold for including brokered rate has been crossed.
// Thread-safe.
func (s *State) BrokeredCompletionCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.brokeredCompletions
}

// CombinedCompletionRate returns a weighted average of TaskCompletionRate and
// BrokeredMatchCompletionRate. The weight is proportional to each path's share
// of total accepted orders.
//
// Used by Layer0Metric after the bootstrap threshold is reached.
// Returns 1.0 when there are no accepted orders at all (cold start).
func (s *State) CombinedCompletionRate() float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	total := len(s.acceptedOrders)
	if total == 0 {
		return 1.0 // cold start
	}
	inlineAccepted := total - s.brokeredAcceptedOrders
	if inlineAccepted < 0 {
		inlineAccepted = 0
	}

	// Compute inline and brokered rates independently.
	var inlineRate float64
	if inlineAccepted == 0 {
		inlineRate = 1.0
	} else {
		inlineCompleted := len(s.completedSettlements) - s.brokeredCompletions
		if inlineCompleted < 0 {
			inlineCompleted = 0
		}
		inlineRate = float64(inlineCompleted) / float64(inlineAccepted)
	}

	var brokerRate float64
	if s.brokeredAcceptedOrders == 0 {
		brokerRate = 1.0
	} else {
		brokerRate = float64(s.brokeredCompletions) / float64(s.brokeredAcceptedOrders)
	}

	// Weighted average by share of total accepted orders.
	inlineWeight := float64(inlineAccepted) / float64(total)
	brokerWeight := float64(s.brokeredAcceptedOrders) / float64(total)
	return inlineRate*inlineWeight + brokerRate*brokerWeight
}

// AllSellerKeys returns the deduplicated set of seller public keys that have
// at least one live inventory entry. Used by the medium loop for per-seller
// reputation and residual computation.
func (s *State) AllSellerKeys() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	seen := make(map[string]struct{}, len(s.sellers))
	for _, entry := range s.inventory {
		if !entry.IsExpired() {
			seen[entry.SellerKey] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	return out
}

// HasCompressedVersion returns true if there is at least one live inventory
// entry whose CompressedFrom field equals entryID. Used by the medium loop to
// determine whether a compression assign should be posted for a high-demand entry.
func (s *State) HasCompressedVersion(entryID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, e := range s.inventory {
		if e.CompressedFrom == entryID && !e.IsExpired() {
			return true
		}
	}
	return false
}

// PurchaseCount returns the number of distinct completed buyers for an entry.
// Semantically identical to EntryDemandCount — exposed under a name that is
// natural in the medium-loop compression-assign context.
func (s *State) PurchaseCount(entryID string) int {
	return s.EntryDemandCount(entryID)
}

// TaskDescriptionHash returns the SHA-256 hex of a task description string.
// Used as the key for buy-miss standing offers.
func TaskDescriptionHash(task string) string {
	h := sha256.Sum256([]byte(task))
	return hex.EncodeToString(h[:])
}

// SetBuyMissOffer records a standing buy-miss offer for the given task hash.
// If a non-expired offer already exists for this hash, it is NOT overwritten
// (one offer per task hash — no duplicates).
// Thread-safe.
func (s *State) SetBuyMissOffer(offer *BuyMissOffer) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.buyMissOffers[offer.TaskHash]; ok && !existing.IsExpired() {
		return false // duplicate — non-expired offer already exists
	}
	s.buyMissOffers[offer.TaskHash] = offer
	return true
}

// GetBuyMissOffer returns the standing buy-miss offer for the given task hash,
// or nil if none exists or it has expired.
// Thread-safe.
func (s *State) GetBuyMissOffer(taskHash string) *BuyMissOffer {
	s.mu.RLock()
	defer s.mu.RUnlock()
	offer, ok := s.buyMissOffers[taskHash]
	if !ok || offer.IsExpired() {
		return nil
	}
	return offer
}

// ClaimBuyMissOffer atomically retrieves and removes the standing buy-miss offer
// for the given task hash in a single mutex acquisition. Returns the offer if
// one exists and is not expired; returns nil otherwise.
//
// Use this instead of separate GetBuyMissOffer + DeleteBuyMissOffer calls to
// prevent TOCTOU races where two concurrent puts could both observe the offer
// before either has deleted it.
// Thread-safe.
func (s *State) ClaimBuyMissOffer(taskHash string) *BuyMissOffer {
	s.mu.Lock()
	defer s.mu.Unlock()
	offer, ok := s.buyMissOffers[taskHash]
	if !ok || offer.IsExpired() {
		return nil
	}
	delete(s.buyMissOffers, taskHash)
	return offer
}

// BrokerAssignForBuy returns the assign message ID of the brokered-match assign
// posted for the given buy message, and whether one exists.
// Used by the engine to detect duplicate brokered-match posting on restart.
func (s *State) BrokerAssignForBuy(buyMsgID string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.brokerAssigns[buyMsgID]
	return id, ok
}

// AssignCompletionSample holds a single assign lifecycle observation for
// actuarial table computation. Returned by AssignCompletionSamples() for the
// slow loop to build per-task-type latency statistics and fill rates.
type AssignCompletionSample struct {
	// TaskType is the assign task_type field (e.g. "brokered-match", "freshness").
	TaskType string
	// Latency is the time from claim to complete. Zero if Completed is false.
	Latency time.Duration
	// Completed is true when the assign reached AssignCompleted state.
	// False means it was posted but never reached assign-complete (reduces fill rate).
	Completed bool
}

// AssignCompletionSamples returns all assign lifecycle samples for actuarial
// table computation by the slow loop. One sample per assign record.
//
// A sample is "Completed" if the assign reached AssignCompleted (or beyond) and
// both ClaimedAt and CompletedAt are non-zero. Latency is CompletedAt - ClaimedAt.
//
// Thread-safe.
func (s *State) AssignCompletionSamples() []AssignCompletionSample {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]AssignCompletionSample, 0, len(s.assignByID))
	for _, rec := range s.assignByID {
		sample := AssignCompletionSample{
			TaskType: rec.TaskType,
		}
		if !rec.ClaimedAt.IsZero() && !rec.CompletedAt.IsZero() &&
			rec.Status >= AssignCompleted {
			sample.Completed = true
			latency := rec.CompletedAt.Sub(rec.ClaimedAt)
			if latency > 0 {
				sample.Latency = latency
			}
		}
		out = append(out, sample)
	}
	return out
}

// GuaranteeForMatch returns the GuaranteeDeadline and InsuredAmount for the
// order that was fulfilled by the given match message ID. Returns zero values
// if the match has no corresponding insured order or the deadline is zero.
// Thread-safe.
func (s *State) GuaranteeForMatch(matchMsgID string) (deadline time.Time, insuredAmount int64, ok bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	g, found := s.matchGuarantee[matchMsgID]
	if !found || g[0] == 0 {
		return
	}
	return time.Unix(0, g[0]).UTC(), g[1], true
}

// DeliverTimeForMatch returns the Timestamp (nanoseconds since epoch) of the
// operator-authored settle(deliver) message for the given match, and whether a
// deliver has been recorded. This is the operator-trusted, replay-deterministic
// reference time the guarantee deadline-miss verdict compares against — the
// deliver phase is operator-authored, so its Timestamp cannot be forged by a
// buyer and is byte-identical across replays (relay-transport.md §4 ADV-10).
// Thread-safe.
func (s *State) DeliverTimeForMatch(matchMsgID string) (int64, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ts, ok := s.deliverTimeByMatch[matchMsgID]
	return ts, ok
}

// ClearMatchGuarantee removes the guarantee record for matchMsgID so that a
// duplicate settle(complete) cannot re-enter the refund path. Called by the
// engine after a successful deadline-miss refund. Thread-safe.
func (s *State) ClearMatchGuarantee(matchMsgID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.matchGuarantee, matchMsgID)
}

// GetPendingPut returns the pending put entry for the given message ID, and
// whether it exists. Thread-safe.
func (s *State) GetPendingPut(msgID string) (*InventoryEntry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry, ok := s.pendingPuts[msgID]
	return entry, ok
}

// ResolveMatchFromAntecedent resolves the match message ID from an antecedent
// ID (which may be a preview message or a match message directly), and returns
// the buyer key for that match. Returns found=false if the antecedent does not
// resolve to a known match. Thread-safe.
func (s *State) ResolveMatchFromAntecedent(antecedentID string) (matchMsgID string, buyerKey string, found bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	// The buyer e-tags the operator match/preview WIRE id; map it to the store id
	// the previewToMatch / matchToBuyer indexes are keyed by (dontguess-55c GAP 1;
	// identity when no alias exists, e.g. the in-process suite / individual tier).
	antecedentID = s.resolveAlias(antecedentID)
	matchMsgID, found = s.previewToMatch[antecedentID]
	if !found {
		// Legacy/small-content path: antecedent is the match message directly.
		matchMsgID = antecedentID
		_, found = s.matchToBuyer[matchMsgID]
	}
	buyerKey = s.matchToBuyer[matchMsgID]
	return matchMsgID, buyerKey, found
}

// MatchEntryID returns the inventory entry ID for the given match message ID.
// Returns empty string if the match is unknown. Thread-safe.
func (s *State) MatchEntryID(matchMsgID string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.matchToEntry[matchMsgID]
}

// MatchInfo returns the buyer key, entry ID, and a flag indicating whether the
// preview-request message msgID has been tracked for the given match. Used by
// the engine to validate preview-request messages atomically. Thread-safe.
func (s *State) MatchInfo(matchMsgID string, previewReqMsgID string) (buyerKey string, entryID string, matchKnown bool, previewTracked bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	// The buyer e-tags the operator match WIRE id → store id (dontguess-55c GAP 1).
	// previewReqMsgID is the buyer's own preview-request (store == wire), so it is
	// left as-is.
	matchMsgID = s.resolveAlias(matchMsgID)
	buyerKey, matchKnown = s.matchToBuyer[matchMsgID]
	entryID = s.matchToEntry[matchMsgID]
	_, previewTracked = s.previewRequestToMatch[previewReqMsgID]
	return buyerKey, entryID, matchKnown, previewTracked
}

// MatchBuyerKey returns the buyer key for the given match message ID.
// Returns empty string if the match is unknown. Thread-safe.
func (s *State) MatchBuyerKey(matchMsgID string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.matchToBuyer[matchMsgID]
}

// InsertDerivativePut inserts a derivative inventory entry atomically. It is
// the thread-safe equivalent of calling applyDerivativePut while holding
// s.mu.Lock(). Idempotent — a second call with the same entry ID is a no-op.
// Thread-safe.
func (s *State) InsertDerivativePut(entry *InventoryEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applyDerivativePut(entry)
}

// ExpireStaleClaimsTS is the thread-safe wrapper around ExpireStaleClaims.
// It acquires a read lock, scans for expired claims, and returns claim message
// IDs. Callers must NOT hold s.mu when calling this method.
func (s *State) ExpireStaleClaimsTS() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ExpireStaleClaims()
}

// PendingAuctionCloseTS is the thread-safe wrapper around PendingAuctionClose.
// It acquires a read lock and returns assign IDs ready for auction close.
// Callers must NOT hold s.mu when calling this method.
func (s *State) PendingAuctionCloseTS() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.PendingAuctionClose()
}
