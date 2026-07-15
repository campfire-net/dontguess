package exchange

import (
	"encoding/json"
	"time"
)

// applySettle processes an exchange:settle message.
func (s *State) applySettle(msg *Message) {
	phase := settlePhaseFromTags(msg.Tags)
	switch phase {
	case SettlePhaseStrPutAccept:
		s.applySettlePutAccept(msg)
	case SettlePhaseStrPutReject:
		s.applySettlePutReject(msg)
	case SettlePhaseStrBuyerAccept:
		s.applySettleBuyerAccept(msg)
	case SettlePhaseStrBuyerReject:
		s.applySettleBuyerReject(msg)
	case SettlePhaseStrDeliver:
		s.applySettleDeliver(msg)
	case SettlePhaseStrComplete:
		s.applySettleComplete(msg)
	case SettlePhaseStrPreviewRequest:
		s.applySettlePreviewRequest(msg)
	case SettlePhaseStrPreview:
		s.applySettlePreview(msg)
	case SettlePhaseStrSmallContentDispute:
		s.applySettleSmallContentDispute(msg)
	}
}

// applySettlePutAccept moves an entry from pending to active inventory.
func (s *State) applySettlePutAccept(msg *Message) {
	if s.OperatorKey != "" && msg.Sender != s.OperatorKey {
		s.recordFoldDenial(foldDenialNotOperator, msg)
		return
	}
	if len(msg.Antecedents) == 0 {
		return
	}
	putMsgID := msg.Antecedents[0]
	entry, ok := s.pendingPuts[putMsgID]
	if !ok {
		return
	}

	var payload struct {
		Price     int64  `json:"price"`
		ExpiresAt string `json:"expires_at"` // ISO 8601 or empty
	}
	if err := json.Unmarshal(msg.Payload, &payload); err == nil {
		entry.PutPrice = payload.Price
		if payload.ExpiresAt != "" {
			if t, err := time.Parse(time.RFC3339, payload.ExpiresAt); err == nil {
				entry.ExpiresAt = t
			}
		}
	}

	delete(s.pendingPuts, putMsgID)
	s.inventory[entry.EntryID] = entry
}

// applySettlePutReject removes an entry from pending inventory.
func (s *State) applySettlePutReject(msg *Message) {
	if s.OperatorKey != "" && msg.Sender != s.OperatorKey {
		s.recordFoldDenial(foldDenialNotOperator, msg)
		return
	}
	if len(msg.Antecedents) == 0 {
		return
	}
	putMsgID := msg.Antecedents[0]
	// dontguess-327: honor the SEAM-A trust-gate purge signal. applyPut registers
	// EVERY put's content hash in contentHashIndex ZERO-TRUST during the fold, and
	// a QUALITY-gate reject deliberately KEEPS that hash (anti-respam,
	// state_put.go dedup §2). But a TRUST-gate reject of a non-allowlisted /
	// below-floor sender must NOT leave the hash squatting: doing so permanently
	// blocks a later ALLOWLISTED seller's byte-identical put (the exchange's
	// designed high-reuse happy path) with a silent bare return. The trust-gate
	// reject path sets purge_content_hash=true; QUALITY-gate rejects omit it.
	// Replay-safe: message-content-driven, idempotent delete, no live TrustChecker.
	var payload struct {
		PurgeContentHash bool `json:"purge_content_hash"`
	}
	if entry, ok := s.pendingPuts[putMsgID]; ok {
		if err := json.Unmarshal(msg.Payload, &payload); err == nil && payload.PurgeContentHash {
			delete(s.contentHashIndex, entry.ContentHash)
		}
	}
	delete(s.pendingPuts, putMsgID)
}

// applySettleBuyerAccept records that a buyer has accepted a match.
// Security: only the buyer who placed the original buy order may accept a match.
// Any other sender is silently rejected (convention §5.3: buyer identity gate).
//
// The antecedent may be either:
//   - A match message ID (legacy/small-content direct accept path), or
//   - A preview message ID (preview-before-purchase path, for content >= SmallContentThreshold tokens).
//
// Both paths resolve to the same match and proceed identically from there.
func (s *State) applySettleBuyerAccept(msg *Message) {
	if len(msg.Antecedents) == 0 {
		return
	}
	// The antecedent is the operator match (or preview) the buyer accepted; the
	// buyer e-tags its WIRE id, so map it to the store id previewToMatch /
	// matchToBuyer are keyed by (dontguess-55c GAP 1; identity when no alias, so
	// the in-process suite is byte-for-byte unchanged). This fold sets
	// buyerAcceptToMatch — the mapping the operator auto-deliver (GAP 2) depends on
	// — so it MUST resolve here, not only in the accessors.
	antecedentID := s.resolveAlias(msg.Antecedents[0])

	// Resolve the match message ID from the antecedent.
	// Try preview path first (antecedent is a preview message).
	var matchMsgID string
	if previewMatch, ok := s.previewToMatch[antecedentID]; ok {
		matchMsgID = previewMatch
	} else {
		// Legacy path: antecedent is the match message directly.
		matchMsgID = antecedentID
	}

	// Enforce buyer identity: the sender must be the buyer who placed the
	// original buy order that this match fulfills. An unknown match (!ok) is a
	// benign drop (stale/never-seen antecedent); a KNOWN match with the wrong
	// sender is a security-relevant buyer-identity forgery and is counted +
	// alarmed rather than dropped silently (dontguess-9ed, convention §5.3).
	expectedBuyer, ok := s.matchToBuyer[matchMsgID]
	if !ok {
		return
	}
	if msg.Sender != expectedBuyer {
		s.recordFoldDenial(foldDenialBuyerIdentity, msg)
		return
	}

	// Idempotency guard (dontguess-f86): buyerAcceptToMatch is keyed by THIS
	// buyer-accept message's own ID (set below) and is never overwritten with
	// a differing value for a repeat application, so its presence is a
	// reliable per-message-ID dedup check for the WHOLE handler — including
	// brokeredAcceptedOrders++ and entryConversionCount/stats.ConversionCount
	// below, both raw counter increments with no guard of their own. Without
	// it a concurrent rebuildAndDispatchGapLocal state.Replay racing
	// foldAndDispatchLocalSnapshot's unlocked incremental Apply loop
	// double-counts them (see State.foldDenialCounted doc for the exact
	// interleave).
	if _, dup := s.buyerAcceptToMatch[msg.ID]; dup {
		return
	}

	// Parse selected entry_id from buyer-accept payload.
	var payload struct {
		EntryID string `json:"entry_id"`
	}
	var selectedEntry string
	if err := json.Unmarshal(msg.Payload, &payload); err == nil && payload.EntryID != "" {
		// Validate that the selected entry_id is one of the offered results.
		if validResults, ok := s.matchToResults[matchMsgID]; ok {
			for _, eid := range validResults {
				if eid == payload.EntryID {
					selectedEntry = payload.EntryID
					break
				}
			}
		}
	}
	// Fall back to first result if no valid selection provided.
	if selectedEntry == "" {
		selectedEntry = s.matchToEntry[matchMsgID]
	}

	if selectedEntry != "" {
		s.acceptedOrders[matchMsgID] = selectedEntry
		// Update matchToEntry to the selected entry so the downstream chain
		// (deliver → complete) resolves to the buyer's chosen entry.
		s.matchToEntry[matchMsgID] = selectedEntry
		// Track brokered accepted orders separately for Layer 0 bootstrap gate.
		if _, brokered := s.brokerMatchIDs[matchMsgID]; brokered {
			s.brokeredAcceptedOrders++
		}
	}
	// Record buyer-accept → match mapping so deliver can trace the chain.
	s.buyerAcceptToMatch[msg.ID] = matchMsgID

	// If this accept came via the preview path (antecedent was a preview message),
	// count it as a conversion for the seller and per-entry conversion tracking.
	if _, viaPreview := s.previewToMatch[antecedentID]; viaPreview && selectedEntry != "" {
		s.entryConversionCount[selectedEntry]++
		if entry, ok := s.inventory[selectedEntry]; ok {
			stats := s.sellerStats(entry.SellerKey)
			stats.ConversionCount++
		}
	}
}

// applySettleBuyerReject records that a buyer has rejected a match.
// The buyer walks away — the accepted order entry is removed so the buyer
// is no longer bound to this match. The inventory entry remains available
// for other buyers. Seller reputation is not affected (buyer chose not to buy).
// Security: only the buyer who placed the original buy order may reject a match.
// Convention §5.3: buyer identity gate — same pattern as applySettleBuyerAccept.
func (s *State) applySettleBuyerReject(msg *Message) {
	if len(msg.Antecedents) == 0 {
		return
	}
	// Buyer e-tags the operator match WIRE id → store id (dontguess-55c GAP 1).
	matchMsgID := s.resolveAlias(msg.Antecedents[0])

	// Enforce buyer identity: the sender must be the buyer who placed the
	// original buy order that this match fulfills. An unknown match (!ok) is a
	// benign stale-antecedent drop; a KNOWN match with the wrong sender is a
	// security-relevant buyer-identity forgery — counted + alarmed rather than
	// dropped silently (dontguess-471 LOCKED-5, same split as applySettleBuyerAccept).
	expectedBuyer, ok := s.matchToBuyer[matchMsgID]
	if !ok {
		return
	}
	if msg.Sender != expectedBuyer {
		s.recordFoldDenial(foldDenialBuyerIdentity, msg)
		return
	}

	// Remove the accepted order so the buyer is no longer bound to this match.
	// The inventory entry and matchedOrders entry are intentionally left intact —
	// the match was sent (order was consumed), and the inventory remains available.
	delete(s.acceptedOrders, matchMsgID)
}

// applySettleDeliver records that the exchange has delivered content to the buyer.
// The antecedent is the settle(buyer-accept) message ID.
// It marks the corresponding match as delivered in deliveredOrders and records
// the deliver→match mapping for use by applySettleComplete.
func (s *State) applySettleDeliver(msg *Message) {
	if s.OperatorKey != "" && msg.Sender != s.OperatorKey {
		s.recordFoldDenial(foldDenialNotOperator, msg)
		return
	}
	// Idempotency guard (dontguess-f86): deliverToMatch is keyed by THIS
	// deliver message's own ID (set below) and, unlike deliveredOrders /
	// deliverTimeByMatch, is never overwritten with a differing value for a
	// repeat application — so its presence is a reliable per-message-ID dedup
	// check. Without it entryDeliverCount / buyerDeliverCount /
	// entryDeliverBuyerCount (raw counter increments, no guard of their own)
	// double-count when a concurrent rebuildAndDispatchGapLocal state.Replay
	// races foldAndDispatchLocalSnapshot's unlocked incremental Apply loop
	// (see State.foldDenialCounted doc for the exact interleave) — skewing the
	// false-positive demotion signal (dontguess-046) and the dwc
	// false-positive-expiry refinement (dontguess-1856) that reads them.
	if _, dup := s.deliverToMatch[msg.ID]; dup {
		return
	}
	if len(msg.Antecedents) == 0 {
		return
	}
	// Antecedent is the buyer-accept message. Trace to the match message.
	buyerAcceptMsgID := msg.Antecedents[0]
	matchMsgID := s.buyerAcceptToMatch[buyerAcceptMsgID]
	if matchMsgID == "" {
		return
	}
	s.deliveredOrders[matchMsgID] = struct{}{}
	// Record deliver→match so applySettleComplete can derive entry_id from the
	// antecedent chain without trusting buyer-supplied payload fields.
	s.deliverToMatch[msg.ID] = matchMsgID
	// Record the operator-authored deliver Timestamp keyed by match. This is the
	// operator-trusted, replay-deterministic reference time checkDeadlineMiss
	// uses to decide whether the exchange missed its guarantee deadline —
	// NEVER the buyer-authored settle(complete) Timestamp (relay-transport.md §4
	// ADV-10). This handler is operator-only (guarded above), so msg.Timestamp is
	// operator-set and persisted, making the deadline-miss verdict both
	// counterparty-unforgeable and identical across replays.
	s.deliverTimeByMatch[matchMsgID] = msg.Timestamp

	// Track deliver count per entry for the false-positive demotion signal
	// (dontguess-046). Derive entry_id from the antecedent chain to avoid
	// trusting any payload field.
	if entryID := s.matchToEntry[matchMsgID]; entryID != "" {
		s.entryDeliverCount[entryID]++

		// dontguess-1856: also track per-buyer deliver counts (global,
		// keyed by buyer pubkey) and per-entry-per-buyer deliver counts, so
		// entryDeliverAbandonWeight can later distinguish a chronic
		// never-completer (griefer) hammering this entry from a broad set
		// of otherwise-healthy buyers each abandoning once. Buyer identity
		// is resolved from matchToBuyer (set when the match was created),
		// not from any buyer-supplied field — consistent with the
		// buyer-identity gates used elsewhere in this file.
		if buyer := s.matchToBuyer[matchMsgID]; buyer != "" {
			s.buyerDeliverCount[buyer]++
			if s.entryDeliverBuyerCount[entryID] == nil {
				s.entryDeliverBuyerCount[entryID] = make(map[string]int)
			}
			s.entryDeliverBuyerCount[entryID][buyer]++
		}
	}
}

// applySettleComplete records a completed transaction and updates seller reputation.
//
// Security: entry_id is derived from the antecedent chain (complete → deliver →
// buyer-accept → match → matchToEntry) rather than trusting payload.EntryID,
// which is buyer-controlled (TAINTED per convention §3). A spoofed payload.EntryID
// cannot redirect reputation credit or price history to a different entry.
//
// The price field is still read from the payload (it is operator-signed by the
// deliver step; the buyer does not control sale price).
func (s *State) applySettleComplete(msg *Message) {
	// Idempotency guard: if this settle(complete) message has already been
	// applied, skip it. Protects against double-application when Apply is
	// called multiple times with the same message (e.g., duplicate delivery).
	if _, seen := s.completedSettlements[msg.ID]; seen {
		return
	}

	if len(msg.Antecedents) == 0 {
		return
	}
	// Antecedent of complete is settle(deliver). The buyer e-tags the operator
	// deliver's WIRE id → store id deliverToMatch is keyed by (dontguess-55c GAP 1).
	deliverMsgID := s.resolveAlias(msg.Antecedents[0])

	// Derive entry_id from the antecedent chain:
	//   deliver → match (via deliverToMatch)
	//   match → entry  (via matchToEntry)
	matchMsgID := s.deliverToMatch[deliverMsgID]
	if matchMsgID == "" {
		// Broken antecedent chain — reject. The deliver message was never
		// processed or doesn't exist in state. Cannot safely attribute credit.
		return
	}
	entryID := s.matchToEntry[matchMsgID]
	if entryID == "" {
		// No entry recorded for this match — reject.
		return
	}

	// Enforce buyer identity: the sender must be the original buyer.
	// The match message recorded the expected buyer key in matchToBuyer. An
	// unknown match is a benign drop; a known match with the wrong sender is a
	// buyer-identity forgery — counted + alarmed (dontguess-471 LOCKED-5).
	expectedBuyer, ok := s.matchToBuyer[matchMsgID]
	if !ok {
		return
	}
	if msg.Sender != expectedBuyer {
		s.recordFoldDenial(foldDenialBuyerIdentity, msg)
		return
	}

	// Read sale price from payload (operator-set; not attacker-controlled at
	// this phase since operators issue deliver messages, not buyers).
	var payload struct {
		Price int64 `json:"price"`
	}
	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		return
	}

	// Mark this message as processed before mutating any state.
	s.completedSettlements[msg.ID] = struct{}{}

	// Track brokered completions separately for the Layer 0 bootstrap gate.
	if _, brokered := s.brokerMatchIDs[matchMsgID]; brokered {
		s.brokeredCompletions++
	}

	// Increment federation transaction count for the buyer. Used by the dual
	// guard to determine when a new node has sufficient history to exit low-trust
	// status.
	s.incrementFederationTransactionCount(msg.Sender)

	buyerKey := msg.Sender
	s.completedEntries[deliverMsgID] = buyerKey

	// dontguess-1856: track per-buyer completion count (global, keyed by
	// buyer pubkey) alongside the existing per-entry entryConsumeCount signal
	// (which is populated separately by applyConsume on the operator-attested
	// exchange:consume message). This settle(complete) event is buyer-identity
	// verified above, so it is a trustworthy source for the buyer's personal
	// completion rate used by entryDeliverAbandonWeight. entryConsumeBuyerCount
	// records the SAME event scoped to (entryID, buyer) so the weighting
	// function can compute a buyer's EXTERNAL completion rate — i.e. their
	// track record on OTHER entries, excluding this one. This is what lets a
	// single-episode abandonment (no track record elsewhere) still flag
	// normally (dontguess-659's griefing regression test requires this),
	// while a buyer with an ESTABLISHED chronic near-zero completion rate on
	// OTHER entries has that pattern discounted from — not built out of —
	// the very entry being judged.
	s.buyerConsumeCount[buyerKey]++
	if s.entryConsumeBuyerCount[entryID] == nil {
		s.entryConsumeBuyerCount[entryID] = make(map[string]int)
	}
	s.entryConsumeBuyerCount[entryID][buyerKey]++

	entry, ok := s.inventory[entryID]
	if !ok {
		return
	}
	sellerKey := entry.SellerKey
	stats := s.sellerStats(sellerKey)
	stats.SuccessCount++

	if stats.RepeatBuyerMap == nil {
		stats.RepeatBuyerMap = make(map[string]int)
	}
	stats.RepeatBuyerMap[buyerKey]++

	if stats.EntryBuyerMap == nil {
		stats.EntryBuyerMap = make(map[string]map[string]struct{})
	}
	if stats.EntryBuyerMap[entryID] == nil {
		stats.EntryBuyerMap[entryID] = make(map[string]struct{})
	}
	stats.EntryBuyerMap[entryID][buyerKey] = struct{}{}

	// Record price history.
	s.priceHistory = append(s.priceHistory, PriceRecord{
		EntryID:     entryID,
		ContentType: entry.ContentType,
		Domains:     entry.Domains,
		PutPrice:    entry.PutPrice,
		SalePrice:   payload.Price,
		Timestamp:   msg.Timestamp,
	})
}

// applySettleSmallContentDispute processes a buyer-initiated small-content dispute.
//
// This is a fully automated path — no operator verdict required. When content
// is below SmallContentThreshold tokens, previews are not meaningful, so buyers
// get an immediate auto-refund with a reputation penalty on the seller.
//
// Validation:
//   - Antecedent must be a deliver message (in deliverToMatch).
//   - Sender must be the buyer who holds the corresponding match.
//   - Entry must exist and have token_cost < SmallContentThreshold
//     OR content_size < SmallContentThreshold*4 bytes (tokens * ~4 bytes/token).
//
// On success:
//   - smallContentDisputes[entryID]++ is incremented.
//   - Seller's SmallContentRefundCount is incremented (-3 reputation per refund).
//
// Silently ignored on any validation failure.
func (s *State) applySettleSmallContentDispute(msg *Message) {
	if len(msg.Antecedents) == 0 {
		return
	}

	// Antecedent must be a deliver message. Buyer e-tags the operator deliver's
	// WIRE id → store id deliverToMatch is keyed by (dontguess-55c GAP 1).
	deliverMsgID := s.resolveAlias(msg.Antecedents[0])
	matchMsgID, ok := s.deliverToMatch[deliverMsgID]
	if !ok || matchMsgID == "" {
		return
	}

	// Sender identity gate: must be the original buyer for this match. An unknown
	// match is a benign drop; a known match with the wrong sender is a
	// buyer-identity forgery — counted + alarmed (dontguess-471 LOCKED-5).
	expectedBuyer, ok := s.matchToBuyer[matchMsgID]
	if !ok {
		return
	}
	if msg.Sender != expectedBuyer {
		s.recordFoldDenial(foldDenialBuyerIdentity, msg)
		return
	}

	// Per-message-ID dedup guard (dontguess-f86, disputeCounted):
	// smallContentDisputes[entryID]++ and stats.SmallContentRefundCount++ are
	// raw counter increments with no natural per-message-ID map to dedup
	// against. Without this guard a concurrent rebuildAndDispatchGapLocal
	// state.Replay racing foldAndDispatchLocalSnapshot's unlocked incremental
	// Apply loop double-applies the -3 reputation penalty (see
	// State.foldDenialCounted doc for the exact interleave).
	if _, dup := s.disputeCounted[msg.ID]; dup {
		return
	}

	// Parse entry_id from payload (informational; we cross-check against chain).
	var payload struct {
		EntryID string `json:"entry_id"`
	}
	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		return
	}

	// Derive entry_id from the antecedent chain to avoid trusting buyer payload.
	chainEntryID := s.matchToEntry[matchMsgID]
	if chainEntryID == "" {
		return
	}

	entry, ok := s.inventory[chainEntryID]
	if !ok {
		return
	}

	// Verify content is actually small: token_cost < threshold OR
	// content_size < threshold * 4 bytes (approximate bytes per token).
	isSmall := entry.TokenCost < SmallContentThreshold ||
		entry.ContentSize < int64(SmallContentThreshold)*4
	if !isSmall {
		return
	}

	// Mark this message as processed before mutating any state (mirrors
	// applySettleComplete's completedSettlements guard placement).
	s.disputeCounted[msg.ID] = struct{}{}

	// Track the auto-refund dispute against this entry.
	s.smallContentDisputes[chainEntryID]++

	// Apply reputation penalty to the seller.
	stats := s.sellerStats(entry.SellerKey)
	stats.SmallContentRefundCount++
}

// applySettlePreviewRequest records a buyer's request for a content preview.
//
// Validation:
//   - Antecedent must be a match message (in matchToBuyer).
//   - Sender must be the buyer who placed the original buy order for that match.
//   - Payload must contain a non-empty entry_id.
//
// On success, updates previewsByEntry, previewCountByMatch, and previewRequestToMatch.
// Silently ignored on any validation failure — the message remains on the log.
func (s *State) applySettlePreviewRequest(msg *Message) {
	if len(msg.Antecedents) == 0 {
		return
	}
	// Buyer e-tags the operator match WIRE id → store id (dontguess-55c GAP 1).
	matchMsgID := s.resolveAlias(msg.Antecedents[0])

	// Validate that the antecedent is a match message with a known buyer.
	expectedBuyer, ok := s.matchToBuyer[matchMsgID]
	if !ok {
		return
	}
	// Validate sender is the original buyer. A known match with the wrong sender
	// is a buyer-identity forgery — counted + alarmed (dontguess-471 LOCKED-5).
	if msg.Sender != expectedBuyer {
		s.recordFoldDenial(foldDenialBuyerIdentity, msg)
		return
	}

	// Parse entry_id from payload.
	var payload struct {
		EntryID string `json:"entry_id"`
	}
	if err := json.Unmarshal(msg.Payload, &payload); err != nil || payload.EntryID == "" {
		return
	}
	entryID := payload.EntryID

	// Track buyer preview request per entry.
	buyerKey := msg.Sender
	if s.previewsByEntry[entryID] == nil {
		s.previewsByEntry[entryID] = make(map[string]string)
	}

	// Reject duplicate preview-requests: if this buyer already has a tracked
	// preview-request for this entry/match, silently ignore the duplicate.
	// Counting duplicates in previewCountByMatch would inflate the rate-limit
	// counter and cause the engine to emit a second preview response for the same
	// buyer/match pair — both wasteful and a content-reconstruction risk.
	if existing := s.previewsByEntry[entryID][buyerKey]; existing == matchMsgID {
		return
	}

	s.previewsByEntry[entryID][buyerKey] = matchMsgID

	// Track preview count per match.
	s.previewCountByMatch[matchMsgID]++

	// Track preview-request → match mapping for the response chain.
	s.previewRequestToMatch[msg.ID] = matchMsgID

	// Update per-entry and per-seller preview counts for the conversion rate model.
	s.entryPreviewCount[entryID]++
	if entry, ok := s.inventory[entryID]; ok {
		stats := s.sellerStats(entry.SellerKey)
		stats.PreviewCount++
	}
}

// applySettlePreview records an operator's preview response message.
//
// Validation:
//   - Sender must be the operator (if OperatorKey is set).
//   - Antecedent must be a preview-request message (in previewRequestToMatch).
//
// On success, updates previewToMatch so buyer-accept can trace the antecedent chain.
// Silently ignored on any validation failure.
func (s *State) applySettlePreview(msg *Message) {
	if s.OperatorKey != "" && msg.Sender != s.OperatorKey {
		s.recordFoldDenial(foldDenialNotOperator, msg)
		return
	}
	if len(msg.Antecedents) == 0 {
		return
	}
	previewRequestMsgID := msg.Antecedents[0]
	matchMsgID, ok := s.previewRequestToMatch[previewRequestMsgID]
	if !ok {
		return
	}
	s.previewToMatch[msg.ID] = matchMsgID
}

// applyDerivativePut adds a derivative inventory entry (e.g. from a compress
// assign-accept) directly to the live inventory without going through the
// put → put-accept flow. The caller is responsible for constructing the entry
// with all required fields set; in particular CompressedFrom must be non-empty
// for compression derivatives.
//
// The entry is keyed by entry.EntryID. If an entry with the same ID already
// exists, the insertion is skipped (idempotent). This ensures that replaying
// the same assign-accept message on engine restart does not produce duplicate
// inventory entries.
//
// This method does not acquire the state mutex — callers must hold s.mu.Lock().
func (s *State) applyDerivativePut(entry *InventoryEntry) {
	if _, exists := s.inventory[entry.EntryID]; exists {
		return
	}
	s.inventory[entry.EntryID] = entry
}
