// Package exchange implements the DontGuess exchange engine and state derivation.
//
// State is an in-memory materialized view of the campfire message log.
// No external database — the campfire IS the database. State can always
// be reconstructed by replaying the full message log (Replay).
package exchange

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/campfire-net/campfire/pkg/store"
)

// Tag constants for exchange convention operations.
const (
	TagPut    = "exchange:put"
	TagBuy    = "exchange:buy"
	TagMatch  = "exchange:match"
	TagSettle = "exchange:settle"

	TagPhasePrefix   = "exchange:phase:"
	TagVerdictPrefix = "exchange:verdict:"

	SettlePhaseStrPutAccept   = "put-accept"
	SettlePhaseStrPutReject   = "put-reject"
	SettlePhaseStrBuyerAccept = "buyer-accept"
	SettlePhaseStrBuyerReject = "buyer-reject"
	SettlePhaseStrDeliver     = "deliver"
	SettlePhaseStrComplete    = "complete"
	SettlePhaseStrDispute     = "dispute"

	// OrderExpiry is how long a buy order lives before it expires without a match.
	OrderExpiry = time.Hour

	// DefaultReputation is the starting reputation score for new sellers.
	DefaultReputation = 50
)

// InventoryEntry is a single cache entry in the exchange inventory.
// An entry is live when a put-accept has settled it and it has not expired.
type InventoryEntry struct {
	// EntryID is derived from the put message ID.
	EntryID string
	// PutMsgID is the campfire message ID of the originating exchange:put.
	PutMsgID string
	// SellerKey is the hex-encoded Ed25519 public key of the seller.
	SellerKey string
	// Description is the seller's task description (TAINTED).
	Description string
	// ContentHash is the sha256: prefixed hash of the cached content (TAINTED).
	ContentHash string
	// ContentType is one of: code, analysis, summary, plan, data, review, other.
	ContentType string
	// Domains is the seller's domain tags (TAINTED, max 5).
	Domains []string
	// TokenCost is the original inference cost in tokens (TAINTED).
	TokenCost int64
	// ContentSize is the content size in bytes (TAINTED).
	ContentSize int64
	// PutPrice is the scrip the exchange paid the seller (from put-accept payload).
	PutPrice int64
	// ExpiresAt is the authoritative expiry set by the exchange operator.
	// Zero means no expiry.
	ExpiresAt time.Time
	// PutTimestamp is the campfire-observed receipt time of the put message (nanoseconds).
	PutTimestamp int64
}

// IsExpired returns true if the entry has passed its expiry time.
func (e *InventoryEntry) IsExpired() bool {
	if e.ExpiresAt.IsZero() {
		return false
	}
	return time.Now().After(e.ExpiresAt)
}

// ActiveOrder is an unfulfilled exchange:buy request.
type ActiveOrder struct {
	// OrderID is the campfire message ID of the exchange:buy message.
	OrderID string
	// BuyerKey is the hex-encoded Ed25519 public key of the buyer.
	BuyerKey string
	// Task is the buyer's task description (TAINTED).
	Task string
	// Budget is the maximum scrip the buyer will spend.
	Budget int64
	// MinReputation is the seller reputation filter (0 = no filter).
	MinReputation int
	// FreshnessHours is the max age of entries to return (0 = no limit).
	FreshnessHours int
	// ContentType is an optional content type filter.
	ContentType string
	// Domains is optional domain tag filters.
	Domains []string
	// MaxResults is the maximum number of matches to return (default 3).
	MaxResults int
	// CreatedAt is when the buy message was received (nanoseconds).
	CreatedAt int64
}

// IsExpired returns true if the order is more than OrderExpiry old.
func (o *ActiveOrder) IsExpired() bool {
	return time.Since(time.Unix(0, o.CreatedAt)) > OrderExpiry
}

// PriceRecord is a single price event derived from a settlement.
type PriceRecord struct {
	// EntryID is the cache entry that was settled.
	EntryID string
	// ContentType is the content type of the settled entry.
	ContentType string
	// Domains are the domain tags of the settled entry.
	Domains []string
	// PutPrice is what the exchange paid the seller.
	PutPrice int64
	// SalePrice is what the buyer paid the exchange.
	SalePrice int64
	// Timestamp is when the settlement occurred (nanoseconds).
	Timestamp int64
}

// SellerStats tracks derived signals for a single seller.
type SellerStats struct {
	// SuccessCount is the number of completed sales without dispute.
	SuccessCount int
	// DisputeCount is the number of upheld disputes against this seller.
	DisputeCount int
	// HashInvalidCount is the number of hash_invalid disputes.
	HashInvalidCount int
	// RepeatBuyerMap tracks distinct (seller, buyer) pairs that have completed.
	// Key: buyerKey, Value: count.
	RepeatBuyerMap map[string]int
	// EntryBuyerMap tracks distinct buyers per entry for cross-agent convergence.
	// Key: entryID, Value: set of buyer keys.
	EntryBuyerMap map[string]map[string]struct{}
}

// Reputation computes the derived reputation score (0-100) for a seller.
// New sellers start at 50 (DefaultReputation).
func (s *SellerStats) Reputation() int {
	score := DefaultReputation
	score += s.SuccessCount         // +1 per success
	score -= s.DisputeCount * 5     // -5 per upheld dispute
	score -= s.HashInvalidCount * 5 // additional -5 per hash_invalid (total -10)

	// +2 for each buyer who has purchased from this seller more than once
	for _, count := range s.RepeatBuyerMap {
		if count > 1 {
			score += 2
		}
	}

	// +3 for each entry where 3+ distinct buyers have completed
	for _, buyers := range s.EntryBuyerMap {
		if len(buyers) >= 3 {
			score += 3
		}
	}

	if score < 0 {
		return 0
	}
	if score > 100 {
		return 100
	}
	return score
}

// State is the in-memory materialized view of the exchange campfire log.
// It is rebuilt on startup by Replay and updated incrementally by Apply.
//
// All mutation must go through Apply — callers must not modify exported maps
// directly.
type State struct {
	mu sync.RWMutex

	// Inventory is keyed by EntryID (= put message ID).
	// Includes only accepted, non-expired entries.
	inventory map[string]*InventoryEntry

	// pendingPuts tracks put messages waiting for a put-accept.
	// Key: put message ID. Cleared on put-accept/put-reject.
	pendingPuts map[string]*InventoryEntry

	// activeOrders is keyed by order (buy message) ID.
	// Includes only orders that have not yet been matched and are not expired.
	activeOrders map[string]*ActiveOrder

	// priceHistory is an append-only log of settlement price records.
	priceHistory []PriceRecord

	// sellers tracks per-seller reputation signals.
	// Key: seller hex-encoded public key.
	sellers map[string]*SellerStats

	// matchedOrders tracks buy message IDs that have been matched (have a
	// fulfilling exchange:match message). Used to exclude from active orders.
	matchedOrders map[string]struct{}

	// putToEntry maps a put message ID to its EntryID for settle lookups.
	putToEntry map[string]string

	// matchToBuyer maps a match message ID to the original buy message's buyer key.
	// Used to validate settle(buyer-accept) sender.
	matchToBuyer map[string]string

	// matchToEntry maps a match message ID to the entry it offered.
	// Used to find the entry when a buyer settles.
	matchToEntry map[string]string

	// settleCounts tracks buy message IDs that have a settle(buyer-accept).
	// Key: buy message ID. Value: entry ID accepted.
	acceptedOrders map[string]string

	// buyerAcceptToMatch maps a buyer-accept message ID to the match message ID
	// it accepted. Used by applySettleDeliver to find the match from the deliver
	// antecedent chain.
	buyerAcceptToMatch map[string]string

	// deliveredOrders tracks match message IDs that have received a deliver.
	// Key: match message ID.
	deliveredOrders map[string]struct{}

	// deliverToMatch maps a deliver message ID to the match message ID it
	// references. Used by applySettleComplete to trace the antecedent chain
	// without trusting the buyer-supplied payload.EntryID.
	deliverToMatch map[string]string

	// completedEntries tracks entry IDs and their buyers who have completed.
	// Key: entryID -> buyerKey.
	completedEntries map[string]string

	// pendingDisputes tracks entries with filed but not-yet-upheld disputes.
	// Key: entryID. A dispute message filed by the buyer records the entry here
	// without penalizing seller reputation. Only an operator upheld verdict
	// (exchange:verdict:accepted on a settle(dispute) message) triggers the
	// reputation penalty.
	pendingDisputes map[string]struct{}
}

// NewState creates an empty exchange state.
func NewState() *State {
	return &State{
		inventory:          make(map[string]*InventoryEntry),
		pendingPuts:        make(map[string]*InventoryEntry),
		activeOrders:       make(map[string]*ActiveOrder),
		priceHistory:       nil,
		sellers:            make(map[string]*SellerStats),
		matchedOrders:      make(map[string]struct{}),
		putToEntry:         make(map[string]string),
		matchToBuyer:       make(map[string]string),
		matchToEntry:       make(map[string]string),
		acceptedOrders:     make(map[string]string),
		buyerAcceptToMatch: make(map[string]string),
		deliveredOrders:    make(map[string]struct{}),
		deliverToMatch:     make(map[string]string),
		completedEntries:   make(map[string]string),
		pendingDisputes:    make(map[string]struct{}),
	}
}

// Replay builds state from scratch by processing all messages in log order.
// It resets the state before processing. Thread-safe.
func (s *State) Replay(msgs []store.MessageRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Reset.
	s.inventory = make(map[string]*InventoryEntry)
	s.pendingPuts = make(map[string]*InventoryEntry)
	s.activeOrders = make(map[string]*ActiveOrder)
	s.priceHistory = nil
	s.sellers = make(map[string]*SellerStats)
	s.matchedOrders = make(map[string]struct{})
	s.putToEntry = make(map[string]string)
	s.matchToBuyer = make(map[string]string)
	s.matchToEntry = make(map[string]string)
	s.acceptedOrders = make(map[string]string)
	s.buyerAcceptToMatch = make(map[string]string)
	s.deliveredOrders = make(map[string]struct{})
	s.deliverToMatch = make(map[string]string)
	s.completedEntries = make(map[string]string)
	s.pendingDisputes = make(map[string]struct{})

	for i := range msgs {
		s.applyLocked(&msgs[i])
	}
}

// Apply processes a single new message, updating state.
// Thread-safe.
func (s *State) Apply(msg *store.MessageRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applyLocked(msg)
}

// applyLocked applies a message to state. Caller must hold s.mu.
func (s *State) applyLocked(msg *store.MessageRecord) {
	op := exchangeOp(msg.Tags)
	switch op {
	case TagPut:
		s.applyPut(msg)
	case TagBuy:
		s.applyBuy(msg)
	case TagMatch:
		s.applyMatch(msg)
	case TagSettle:
		s.applySettle(msg)
	}
}

// exchangeOp returns the exchange operation tag from a message's tag list,
// or "" if none is present.
func exchangeOp(tags []string) string {
	for _, t := range tags {
		switch t {
		case TagPut, TagBuy, TagMatch, TagSettle:
			return t
		}
	}
	return ""
}

// settlePhasFromTags extracts the exchange:phase:* value from tags.
func settlePhaseFromTags(tags []string) string {
	for _, t := range tags {
		if strings.HasPrefix(t, TagPhasePrefix) {
			return strings.TrimPrefix(t, TagPhasePrefix)
		}
	}
	return ""
}

// verdictFromTags extracts the exchange:verdict:* value from tags, or "" if absent.
func verdictFromTags(tags []string) string {
	for _, t := range tags {
		if strings.HasPrefix(t, TagVerdictPrefix) {
			return strings.TrimPrefix(t, TagVerdictPrefix)
		}
	}
	return ""
}

// applyPut processes an exchange:put message.
func (s *State) applyPut(msg *store.MessageRecord) {
	var payload struct {
		Description string   `json:"description"`
		ContentHash string   `json:"content_hash"`
		TokenCost   int64    `json:"token_cost"`
		ContentType string   `json:"content_type"`
		Domains     []string `json:"domains"`
		ContentSize int64    `json:"content_size"`
	}
	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		return
	}
	entry := &InventoryEntry{
		EntryID:      msg.ID,
		PutMsgID:     msg.ID,
		SellerKey:    msg.Sender,
		Description:  payload.Description,
		ContentHash:  payload.ContentHash,
		ContentType:  payload.ContentType,
		Domains:      payload.Domains,
		TokenCost:    payload.TokenCost,
		ContentSize:  payload.ContentSize,
		PutTimestamp: msg.Timestamp,
	}
	s.pendingPuts[msg.ID] = entry
	s.putToEntry[msg.ID] = msg.ID
}

// applyBuy processes an exchange:buy message.
func (s *State) applyBuy(msg *store.MessageRecord) {
	var payload struct {
		Task           string   `json:"task"`
		Budget         int64    `json:"budget"`
		MinReputation  int      `json:"min_reputation"`
		FreshnessHours int      `json:"freshness_hours"`
		ContentType    string   `json:"content_type"`
		Domains        []string `json:"domains"`
		MaxResults     int      `json:"max_results"`
	}
	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		return
	}
	maxResults := payload.MaxResults
	if maxResults <= 0 {
		maxResults = 3
	}
	order := &ActiveOrder{
		OrderID:        msg.ID,
		BuyerKey:       msg.Sender,
		Task:           payload.Task,
		Budget:         payload.Budget,
		MinReputation:  payload.MinReputation,
		FreshnessHours: payload.FreshnessHours,
		ContentType:    payload.ContentType,
		Domains:        payload.Domains,
		MaxResults:     maxResults,
		CreatedAt:      msg.Timestamp,
	}
	s.activeOrders[msg.ID] = order
}

// applyMatch processes an exchange:match message.
// The match fulfills a buy future. We mark the order matched and record match→buyer.
func (s *State) applyMatch(msg *store.MessageRecord) {
	if len(msg.Antecedents) == 0 {
		return
	}
	buyMsgID := msg.Antecedents[0]
	s.matchedOrders[buyMsgID] = struct{}{}

	// Find the buyer key from the order.
	if order, ok := s.activeOrders[buyMsgID]; ok {
		s.matchToBuyer[msg.ID] = order.BuyerKey
	}

	// Extract first result's entry_id for match→entry mapping.
	var payload struct {
		Results []struct {
			EntryID string `json:"entry_id"`
		} `json:"results"`
	}
	if err := json.Unmarshal(msg.Payload, &payload); err == nil && len(payload.Results) > 0 {
		s.matchToEntry[msg.ID] = payload.Results[0].EntryID
	}
}

// applySettle processes an exchange:settle message.
func (s *State) applySettle(msg *store.MessageRecord) {
	phase := settlePhaseFromTags(msg.Tags)
	switch phase {
	case SettlePhaseStrPutAccept:
		s.applySettlePutAccept(msg)
	case SettlePhaseStrPutReject:
		s.applySettlePutReject(msg)
	case SettlePhaseStrBuyerAccept:
		s.applySettleBuyerAccept(msg)
	case SettlePhaseStrDeliver:
		s.applySettleDeliver(msg)
	case SettlePhaseStrComplete:
		s.applySettleComplete(msg)
	case SettlePhaseStrDispute:
		s.applySettleDispute(msg)
	}
}

// applySettlePutAccept moves an entry from pending to active inventory.
func (s *State) applySettlePutAccept(msg *store.MessageRecord) {
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
func (s *State) applySettlePutReject(msg *store.MessageRecord) {
	if len(msg.Antecedents) == 0 {
		return
	}
	putMsgID := msg.Antecedents[0]
	delete(s.pendingPuts, putMsgID)
}

// applySettleBuyerAccept records that a buyer has accepted a match.
// Security: only the buyer who placed the original buy order may accept a match.
// Any other sender is silently rejected (convention §5.3: buyer identity gate).
func (s *State) applySettleBuyerAccept(msg *store.MessageRecord) {
	if len(msg.Antecedents) == 0 {
		return
	}
	matchMsgID := msg.Antecedents[0]

	// Enforce buyer identity: the sender must be the buyer who placed the
	// original buy order that this match fulfills.
	expectedBuyer, ok := s.matchToBuyer[matchMsgID]
	if !ok || msg.Sender != expectedBuyer {
		return
	}

	entryID := s.matchToEntry[matchMsgID]
	if entryID != "" {
		s.acceptedOrders[matchMsgID] = entryID
	}
	// Record buyer-accept → match mapping so deliver can trace the chain.
	s.buyerAcceptToMatch[msg.ID] = matchMsgID
}

// applySettleDeliver records that the exchange has delivered content to the buyer.
// The antecedent is the settle(buyer-accept) message ID.
// It marks the corresponding match as delivered in deliveredOrders and records
// the deliver→match mapping for use by applySettleComplete.
func (s *State) applySettleDeliver(msg *store.MessageRecord) {
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
func (s *State) applySettleComplete(msg *store.MessageRecord) {
	if len(msg.Antecedents) == 0 {
		return
	}
	// Antecedent of complete is settle(deliver).
	deliverMsgID := msg.Antecedents[0]

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
	// The match message recorded the expected buyer key in matchToBuyer.
	expectedBuyer, ok := s.matchToBuyer[matchMsgID]
	if !ok || msg.Sender != expectedBuyer {
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

	buyerKey := msg.Sender
	s.completedEntries[deliverMsgID] = buyerKey

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

// applySettleDispute records a dispute filing. Seller reputation is only penalized
// when the dispute is upheld by the operator — indicated by an exchange:verdict:accepted
// tag on the settle(dispute) message. A dispute filed by the buyer alone (no verdict
// tag, or exchange:verdict:disputed) is tracked in pendingDisputes but does NOT
// affect seller reputation. This prevents buyers from gaming reputation by filing
// unlimited unreviewed disputes (convention §7.4).
func (s *State) applySettleDispute(msg *store.MessageRecord) {
	var payload struct {
		EntryID     string `json:"entry_id"`
		DisputeType string `json:"dispute_type"`
	}
	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		return
	}
	entryID := payload.EntryID
	if entryID == "" {
		return
	}

	// Track all filed disputes, regardless of verdict.
	s.pendingDisputes[entryID] = struct{}{}

	// Only penalize reputation on operator-upheld disputes (exchange:verdict:accepted).
	verdict := verdictFromTags(msg.Tags)
	if verdict != "accepted" {
		return
	}

	entry, ok := s.inventory[entryID]
	if !ok {
		return
	}
	stats := s.sellerStats(entry.SellerKey)
	stats.DisputeCount++
	if payload.DisputeType == "hash_invalid" {
		stats.HashInvalidCount++
	}
}

// sellerStats returns the SellerStats for the given key, creating if absent.
// Caller must hold s.mu.
func (s *State) sellerStats(sellerKey string) *SellerStats {
	if stats, ok := s.sellers[sellerKey]; ok {
		return stats
	}
	stats := &SellerStats{
		RepeatBuyerMap: make(map[string]int),
		EntryBuyerMap:  make(map[string]map[string]struct{}),
	}
	s.sellers[sellerKey] = stats
	return stats
}

// Inventory returns a snapshot of all live (accepted, non-expired) inventory entries.
func (s *State) Inventory() []*InventoryEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*InventoryEntry, 0, len(s.inventory))
	for _, e := range s.inventory {
		if !e.IsExpired() {
			out = append(out, e)
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

// HasPendingDispute returns true if a dispute has been filed against the entry
// (regardless of whether it has been upheld by the operator).
func (s *State) HasPendingDispute(entryID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.pendingDisputes[entryID]
	return ok
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
func (s *State) SellerKeyForDeliver(deliverMsgID string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
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

// operatorKeyHex converts a raw Ed25519 public key to its hex representation.
func operatorKeyHex(pub []byte) string {
	return fmt.Sprintf("%x", pub)
}

// decodeHexKey decodes a hex-encoded key to bytes. Returns nil on error.
func decodeHexKey(hexKey string) []byte {
	b, err := hex.DecodeString(hexKey)
	if err != nil {
		return nil
	}
	return b
}
