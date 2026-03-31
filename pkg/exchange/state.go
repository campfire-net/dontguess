// Package exchange implements the DontGuess exchange engine and state derivation.
//
// State is an in-memory materialized view of the campfire message log.
// No external database — the campfire IS the database. State can always
// be reconstructed by replaying the full message log (Replay).
package exchange

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/3dl-dev/dontguess/pkg/scrip"
)

// Tag constants for exchange convention operations.
const (
	TagPut    = "exchange:put"
	TagBuy    = "exchange:buy"
	TagMatch  = "exchange:match"
	TagSettle = "exchange:settle"

	TagPhasePrefix   = "exchange:phase:"
	TagVerdictPrefix = "exchange:verdict:"

	// Assign lifecycle operation tags.
	TagAssign         = "exchange:assign"
	TagAssignClaim    = "exchange:assign-claim"
	TagAssignComplete = "exchange:assign-complete"
	TagAssignAccept   = "exchange:assign-accept"
	TagAssignReject   = "exchange:assign-reject"

	// TagBuyMiss is the tag applied to a buy-miss standing offer message.
	// Sent by the engine as a reply to a buy with no matching inventory.
	TagBuyMiss = "exchange:buy-miss"

	SettlePhaseStrPutAccept   = "put-accept"
	SettlePhaseStrPutReject   = "put-reject"
	SettlePhaseStrBuyerAccept = "buyer-accept"
	SettlePhaseStrBuyerReject = "buyer-reject"
	SettlePhaseStrDeliver     = "deliver"
	SettlePhaseStrComplete    = "complete"

	SettlePhaseStrPreviewRequest      = "preview-request"
	SettlePhaseStrPreview             = "preview"
	SettlePhaseStrSmallContentDispute = "small-content-dispute"

	// SmallContentThreshold is the token count below which content is too small
	// for meaningful preview. Entries below this threshold use the small-content
	// dispute path instead.
	SmallContentThreshold = 500

	// SmallContentReputationPenalty is the per-refund reputation hit for
	// small-content disputes.
	SmallContentReputationPenalty = 3

	// OrderExpiry is how long a buy order lives before it expires without a match.
	OrderExpiry = time.Hour

	// DefaultReputation is the starting reputation score for new sellers.
	DefaultReputation = 50

	// Input validation bounds for TAINTED fields.
	//
	// MaxDescriptionBytes is the maximum allowed length for a put Description
	// field (64 KiB). Prevents OOM via oversized description strings.
	MaxDescriptionBytes = 64 * 1024 // 64 KiB

	// MaxDomainsCount is the maximum number of domain tags on a put.
	// Convention §2 notes max 5; enforce at the state layer.
	MaxDomainsCount = 5

	// MaxTokenCost is the maximum accepted TokenCost value on a put.
	// Prevents int overflow; capped at MaxInt32 (~2 billion tokens).
	MaxTokenCost = int64(1<<31 - 1) // MaxInt32

	// MaxTaskBytes is the maximum allowed length for a buy Task field (64 KiB).
	// Prevents OOM via oversized task strings.
	MaxTaskBytes = 64 * 1024 // 64 KiB

	// MaxBuyMaxResults caps the MaxResults field on a buy request.
	// Prevents OOM via large result-set allocations.
	MaxBuyMaxResults = 100
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

	// AcceptedProvenanceLevel is the seller's provenance level (0–3) at the time the
	// put-accept was processed. Recorded so we can detect a subsequent downgrade.
	// Zero means the level was not recorded (legacy entries accepted before this field existed).
	AcceptedProvenanceLevel int

	// NeedsRevalidation is set to true when the seller's current provenance level
	// has dropped below AcceptedProvenanceLevel since put-accept.
	//
	// Semantics (dontguess-lqp): when provenance is downgraded, existing inventory
	// entries are NOT immediately purged — they are flagged for re-validation.
	// Rationale: purging would be irreversible and could discard legitimate content;
	// re-validation lets the operator decide whether to keep, reprice, or expire the
	// entry. Flagged entries are excluded from buy match results until cleared.
	// Operators can clear the flag by calling SetEntryProvenanceLevel with the current
	// (lower) level once satisfied the content is still acceptable.
	NeedsRevalidation bool

	// CompressedFrom is the EntryID of the original inventory entry that this
	// entry was derived from via a compression assign task. Non-empty only for
	// derivative entries created by handleAssignAccept when task_type == "compress".
	// The derivative entry is independently priced and matchable.
	CompressedFrom string
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

// PriceAdjustment is a dynamic price multiplier written by the fast pricing loop.
// A multiplier of 1.0 means no adjustment. Values above 1.0 increase the price;
// values below 1.0 decrease it. The adjustment expires at ExpiresAt; stale
// adjustments are treated as 1.0x by computePrice.
type PriceAdjustment struct {
	// Multiplier is the price scaling factor (e.g., 1.5 = +50%, 0.8 = -20%).
	// Must be > 0. Values <= 0 are ignored (treated as 1.0).
	Multiplier float64
	// ExpiresAt is when this adjustment becomes stale and is ignored.
	// Zero ExpiresAt means the adjustment never expires (useful in tests).
	ExpiresAt time.Time
	// VelocityPerHour is the demand velocity (purchases per hour) that drove
	// this adjustment. Stored for diagnostics and medium-loop input.
	VelocityPerHour float64
	// VolumeSurplus is the ratio of actual demand to expected demand for this entry.
	// > 1.0 = demand above baseline, < 1.0 = below baseline.
	VolumeSurplus float64
}

// IsExpired returns true if the adjustment's ExpiresAt is non-zero and past.
func (a *PriceAdjustment) IsExpired() bool {
	return !a.ExpiresAt.IsZero() && time.Now().After(a.ExpiresAt)
}

// BuyMissOffer is a standing offer the exchange makes to a buyer when no
// inventory match is found. If the buyer computes the result themselves and
// puts it within ExpiresAt, the exchange auto-accepts at OfferedPrice.
//
// One offer per TaskHash (SHA-256 of the task description, hex-encoded) — no duplicates.
// Offers expire after BuyMissOfferTTL (24h).
type BuyMissOffer struct {
	// TaskHash is the SHA-256 hex of the canonical task description.
	TaskHash string
	// BuyMsgID is the campfire message ID of the originating exchange:buy.
	BuyMsgID string
	// BuyerKey is the hex-encoded Ed25519 public key of the buyer.
	BuyerKey string
	// Task is the original task description (for logging/diagnostics).
	Task string
	// OfferedPrice is the scrip the exchange will pay on auto-accept
	// (token_cost_from_put * 0.7 — computed at fulfillment time).
	// When zero, the engine uses the standard 70% of token_cost.
	OfferedPrice int64
	// ExpiresAt is when this standing offer expires.
	ExpiresAt time.Time
}

// IsExpired returns true if the offer has passed its expiry.
func (o *BuyMissOffer) IsExpired() bool {
	return time.Now().After(o.ExpiresAt)
}

// BuyMissOfferTTL is how long a buy-miss standing offer remains valid.
const BuyMissOfferTTL = 24 * time.Hour

// BuyMissOfferRate is the fraction of token_cost the exchange pays on
// a buy-miss auto-accept (70% — same as standard auto-accept discount).
const BuyMissOfferRate = 70 // percent

// AssignStatus is the lifecycle state of a single assign task.
type AssignStatus int

const (
	// AssignOpen is the initial state: the task is available to claim.
	AssignOpen AssignStatus = iota
	// AssignClaimed means an agent has claimed the task and is working it.
	AssignClaimed
	// AssignCompleted means the agent has submitted a result pending operator review.
	AssignCompleted
	// AssignAccepted means the operator accepted the result and the task is done.
	AssignAccepted
	// AssignRejected means the operator rejected the result.
	AssignRejected
	// AssignPaid means the bounty has been paid to the claimant (terminal state).
	// Transitions from AssignAccepted → AssignPaid in ClaimAssignPayment to
	// prevent double-payment on replayed accept messages.
	AssignPaid
)

// AssignRecord tracks the lifecycle of a single assign task.
type AssignRecord struct {
	// AssignID is the message ID of the originating exchange:assign message.
	AssignID string
	// EntryID is the inventory entry this task is associated with (e.g. freshness
	// check, validation, context compression). May be empty for generic tasks.
	EntryID string
	// TaskType is a human-readable task category (e.g. "freshness", "validation",
	// "compression").
	TaskType string
	// Reward is the scrip amount paid to the claimant on accept.
	Reward int64
	// Status is the current lifecycle state.
	Status AssignStatus
	// ClaimantKey is the hex-encoded public key of the agent that claimed the task.
	// Empty until claimed.
	ClaimantKey string
	// ClaimMsgID is the message ID of the exchange:assign-claim message.
	ClaimMsgID string
	// CompleteMsgID is the message ID of the exchange:assign-complete message.
	CompleteMsgID string
	// Result is the agent-supplied result payload (from assign-complete).
	// Stored as raw JSON for the operator to inspect before accepting.
	Result []byte
	// ExclusiveSender restricts who may claim this task. If non-empty, only the
	// agent with this public key may claim. Used for seller-only or buyer-only tasks.
	ExclusiveSender string
}

// SellerStats tracks derived signals for a single seller.
type SellerStats struct {
	// SuccessCount is the number of completed sales without dispute.
	SuccessCount int
	// SmallContentRefundCount tracks auto-refunds from small-content disputes.
	// Each refund costs SmallContentReputationPenalty (3) reputation points.
	SmallContentRefundCount int
	// RepeatBuyerMap tracks distinct (seller, buyer) pairs that have completed.
	// Key: buyerKey, Value: count.
	RepeatBuyerMap map[string]int
	// EntryBuyerMap tracks distinct buyers per entry for cross-agent convergence.
	// Key: entryID, Value: set of buyer keys.
	EntryBuyerMap map[string]map[string]struct{}
	// PreviewCount tracks total previews served for this seller's entries.
	PreviewCount int
	// ConversionCount tracks previews that resulted in buyer-accept (purchase).
	ConversionCount int
}

// Reputation computes the derived reputation score (0-100) for a seller.
// New sellers start at 50 (DefaultReputation).
//
// Conversion rate bonus: if 10+ previews have been served, reward high conversion.
// Scale: 0% conversion = -10, 50% = 0, 100% = +10.
func (s *SellerStats) Reputation() int {
	score := DefaultReputation

	// Conversion rate bonus: only applied when there is enough data (10+ previews).
	if s.PreviewCount >= 10 {
		rate := float64(s.ConversionCount) / float64(s.PreviewCount)
		// Scale: 0% = -10, 50% = 0, 100% = +10
		conversionBonus := int((rate - 0.5) * 20)
		score += conversionBonus
	}

	score += s.SuccessCount // +1 per completed sale

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

	// Small-content refund penalty.
	score -= s.SmallContentRefundCount * SmallContentReputationPenalty

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

	// OperatorKey is the hex-encoded Ed25519 public key of the exchange operator.
	// Operator-only messages (put-accept, put-reject, match, deliver) are silently
	// rejected when their Sender does not match this key.
	OperatorKey string

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
	// After buyer-accept selects a specific entry, this is updated to that entry.
	matchToEntry map[string]string

	// matchToResults maps a match message ID to all offered entry IDs.
	// Used by applySettleBuyerAccept to validate the buyer's selected entry_id.
	matchToResults map[string][]string

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

	// completedSettlements tracks settle(complete) message IDs that have
	// already been processed. Guards applySettleComplete against double-application
	// when Apply is called multiple times with the same message (e.g., duplicate
	// delivery). Key: settle(complete) message ID.
	completedSettlements map[string]struct{}

	// previewsByEntry tracks which buyers have requested previews for which entries.
	// Key: entryID -> map[buyerKey]matchMsgID. Used to enforce one-preview-per-match
	// and to trace the preview antecedent chain.
	previewsByEntry map[string]map[string]string

	// previewCountByMatch tracks how many preview-requests have been made per match.
	// Key: matchMsgID -> count. Used for rate limiting and anti-reconstruction detection.
	previewCountByMatch map[string]int

	// previewRequestToMatch maps a preview-request message ID to the match message ID
	// it references. Used by applySettlePreview to trace the antecedent chain.
	previewRequestToMatch map[string]string

	// previewToMatch maps a preview message ID to the match message ID.
	// Used by buyer-accept to validate the antecedent chain (buyer-accept antecedent
	// is now the preview message for content >= 500 tokens).
	previewToMatch map[string]string

	// smallContentDisputes tracks entries that received small-content auto-refund disputes.
	// Key: entryID -> count of disputes. Used by reputation model for -3 per refund.
	smallContentDisputes map[string]int

	// entryPreviewCount tracks previews per entry. Key: entryID.
	// Incremented in applySettlePreviewRequest when the preview-request is accepted.
	entryPreviewCount map[string]int

	// entryConversionCount tracks conversions per entry. Key: entryID.
	// Incremented in applySettleBuyerAccept when the buyer accepts via the preview path.
	entryConversionCount map[string]int

	// priceAdjustments holds dynamic price multipliers written by the fast pricing loop.
	// Key: entryID. The multiplier is applied on top of computePrice's base result.
	// Stale adjustments (past ExpiresAt) are treated as 1.0x by computePrice.
	// Not reset on Replay — externally written, not derived from the campfire log.
	priceAdjustments map[string]PriceAdjustment

	// matchToBuyHold indexes match message IDs to reservation IDs from
	// scrip-buy-hold messages. Populated by applyScripBuyHold during Replay/Apply.
	// O(1) alternative to scanning the full log in findExistingBuyerAcceptHold.
	// Key: matchMsgID (BuyMsg field from BuyHoldPayload). Value: reservationID.
	matchToBuyHold map[string]string

	// assignsByEntry maps entry IDs to all assign records for that entry.
	// Key: entryID. Assigns with no entry use "" as key.
	assignsByEntry map[string][]*AssignRecord

	// assignByID maps assign message IDs to their records for O(1) lookup.
	assignByID map[string]*AssignRecord

	// claimedAssigns maps agent public keys to the assign IDs they currently hold.
	// An agent may only hold one active claim at a time.
	// Key: agentKey -> assignID.
	claimedAssigns map[string]string

	// pendingAssignResults maps complete message IDs to the assign record waiting
	// for operator accept/reject. Key: completeMsgID -> *AssignRecord.
	pendingAssignResults map[string]*AssignRecord

	// buyMissOffers tracks standing buy-miss offers keyed by task description hash.
	// One offer per task hash — no duplicates. Offers expire after BuyMissOfferTTL.
	// Key: SHA-256 hex of canonical task description.
	buyMissOffers map[string]*BuyMissOffer
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
		matchToResults:     make(map[string][]string),
		acceptedOrders:     make(map[string]string),
		buyerAcceptToMatch: make(map[string]string),
		deliveredOrders:    make(map[string]struct{}),
		deliverToMatch:     make(map[string]string),
		completedEntries:      make(map[string]string),
		completedSettlements:  make(map[string]struct{}),
		previewsByEntry:       make(map[string]map[string]string),
		previewCountByMatch:   make(map[string]int),
		previewRequestToMatch: make(map[string]string),
		previewToMatch:        make(map[string]string),
		smallContentDisputes:  make(map[string]int),
		entryPreviewCount:     make(map[string]int),
		entryConversionCount:  make(map[string]int),
		priceAdjustments:     make(map[string]PriceAdjustment),
		matchToBuyHold:       make(map[string]string),
		assignsByEntry:       make(map[string][]*AssignRecord),
		assignByID:           make(map[string]*AssignRecord),
		claimedAssigns:       make(map[string]string),
		pendingAssignResults: make(map[string]*AssignRecord),
		buyMissOffers:        make(map[string]*BuyMissOffer),
	}
}

// Replay builds state from scratch by processing all messages in log order.
// It resets the state before processing. Thread-safe.
func (s *State) Replay(msgs []Message) {
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
	s.matchToResults = make(map[string][]string)
	s.acceptedOrders = make(map[string]string)
	s.buyerAcceptToMatch = make(map[string]string)
	s.deliveredOrders = make(map[string]struct{})
	s.deliverToMatch = make(map[string]string)
	s.completedEntries = make(map[string]string)
	s.completedSettlements = make(map[string]struct{})
	s.previewsByEntry = make(map[string]map[string]string)
	s.previewCountByMatch = make(map[string]int)
	s.previewRequestToMatch = make(map[string]string)
	s.previewToMatch = make(map[string]string)
	s.smallContentDisputes = make(map[string]int)
	s.entryPreviewCount = make(map[string]int)
	s.entryConversionCount = make(map[string]int)
	s.matchToBuyHold = make(map[string]string)
	s.assignsByEntry = make(map[string][]*AssignRecord)
	s.assignByID = make(map[string]*AssignRecord)
	s.claimedAssigns = make(map[string]string)
	s.pendingAssignResults = make(map[string]*AssignRecord)
	s.buyMissOffers = make(map[string]*BuyMissOffer)
	// Note: priceAdjustments is intentionally NOT reset on Replay.
	// It is externally written by the fast pricing loop, not derived from the log.

	for i := range msgs {
		s.applyLocked(&msgs[i])
	}
}

// Apply processes a single new message, updating state.
// Thread-safe.
func (s *State) Apply(msg *Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applyLocked(msg)
}

// applyLocked applies a message to state. Caller must hold s.mu.
func (s *State) applyLocked(msg *Message) {
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
	case TagAssign:
		s.applyAssign(msg)
	case TagAssignClaim:
		s.applyAssignClaim(msg)
	case TagAssignComplete:
		s.applyAssignComplete(msg)
	case TagAssignAccept:
		s.applyAssignAccept(msg)
	case TagAssignReject:
		s.applyAssignReject(msg)
	default:
		// Handle scrip convention messages that are not exchange operations.
		for _, tag := range msg.Tags {
			if tag == scrip.TagScripBuyHold {
				s.applyScripBuyHold(msg)
				return
			}
		}
	}
}

// applyScripBuyHold indexes a scrip-buy-hold message into matchToBuyHold.
// Enables O(1) lookup in GetBuyHoldReservation, replacing the O(n) log scan
// in findExistingBuyerAcceptHold.
func (s *State) applyScripBuyHold(msg *Message) {
	var p scrip.BuyHoldPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		return
	}
	if p.BuyMsg == "" || p.ReservationID == "" {
		return
	}
	s.matchToBuyHold[p.BuyMsg] = p.ReservationID
}

// GetBuyHoldReservation returns the reservation ID for a prior scrip-buy-hold
// message matching the given match message ID, or "" if none exists.
// O(1) — replaces the O(n) log scan in findExistingBuyerAcceptHold.
func (s *State) GetBuyHoldReservation(matchMsgID string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.matchToBuyHold[matchMsgID]
}

// exchangeOp returns the exchange operation tag from a message's tag list,
// or "" if none is present.
func exchangeOp(tags []string) string {
	for _, t := range tags {
		switch t {
		case TagPut, TagBuy, TagMatch, TagSettle,
			TagAssign, TagAssignClaim, TagAssignComplete, TagAssignAccept, TagAssignReject:
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

// applyPut processes an exchange:put message.
func (s *State) applyPut(msg *Message) {
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
	// Validate TAINTED fields. Drop silently — the message is already on the
	// campfire log; we cannot remove it. By not adding it to pendingPuts the
	// operator's put-accept will find nothing to accept.
	if len(payload.Description) > MaxDescriptionBytes {
		return
	}
	if len(payload.Domains) > MaxDomainsCount {
		return
	}
	if payload.TokenCost <= 0 || payload.TokenCost > MaxTokenCost {
		return
	}
	entry := &InventoryEntry{
		EntryID:      msg.ID,
		PutMsgID:     msg.ID,
		SellerKey:    msg.Sender,
		Description:  payload.Description,
		ContentHash:  payload.ContentHash,
		ContentType:  stripTagPrefix(payload.ContentType, "exchange:content-type:"),
		Domains:      stripDomainPrefixes(payload.Domains),
		TokenCost:    payload.TokenCost,
		ContentSize:  payload.ContentSize,
		PutTimestamp: msg.Timestamp,
	}
	s.pendingPuts[msg.ID] = entry
	s.putToEntry[msg.ID] = msg.ID
}

// applyBuy processes an exchange:buy message.
func (s *State) applyBuy(msg *Message) {
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
	// Validate TAINTED fields. Drop silently — see applyPut comment.
	if len(payload.Task) > MaxTaskBytes {
		return
	}
	maxResults := payload.MaxResults
	if maxResults <= 0 {
		maxResults = 3
	}
	if maxResults > MaxBuyMaxResults {
		return
	}
	order := &ActiveOrder{
		OrderID:        msg.ID,
		BuyerKey:       msg.Sender,
		Task:           payload.Task,
		Budget:         payload.Budget,
		MinReputation:  payload.MinReputation,
		FreshnessHours: payload.FreshnessHours,
		ContentType:    stripTagPrefix(payload.ContentType, "exchange:content-type:"),
		Domains:        stripDomainPrefixes(payload.Domains),
		MaxResults:     maxResults,
		CreatedAt:      msg.Timestamp,
	}
	s.activeOrders[msg.ID] = order
}

// applyMatch processes an exchange:match message.
// The match fulfills a buy future. We mark the order matched and record match→buyer.
func (s *State) applyMatch(msg *Message) {
	if s.OperatorKey != "" && msg.Sender != s.OperatorKey {
		return
	}
	if len(msg.Antecedents) == 0 {
		return
	}
	buyMsgID := msg.Antecedents[0]
	s.matchedOrders[buyMsgID] = struct{}{}

	// Find the buyer key from the order.
	if order, ok := s.activeOrders[buyMsgID]; ok {
		s.matchToBuyer[msg.ID] = order.BuyerKey
	}

	// Extract all result entry_ids.
	// matchToResults tracks the full set for buyer-accept validation.
	// matchToEntry is pre-populated with the first result as the default selection.
	var payload struct {
		Results []struct {
			EntryID string `json:"entry_id"`
		} `json:"results"`
	}
	if err := json.Unmarshal(msg.Payload, &payload); err == nil && len(payload.Results) > 0 {
		s.matchToEntry[msg.ID] = payload.Results[0].EntryID
		entryIDs := make([]string, 0, len(payload.Results))
		for _, r := range payload.Results {
			if r.EntryID != "" {
				entryIDs = append(entryIDs, r.EntryID)
			}
		}
		s.matchToResults[msg.ID] = entryIDs
	}
}

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
		return
	}
	if len(msg.Antecedents) == 0 {
		return
	}
	putMsgID := msg.Antecedents[0]
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
	antecedentID := msg.Antecedents[0]

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
	// original buy order that this match fulfills.
	expectedBuyer, ok := s.matchToBuyer[matchMsgID]
	if !ok || msg.Sender != expectedBuyer {
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
	matchMsgID := msg.Antecedents[0]

	// Enforce buyer identity: the sender must be the buyer who placed the
	// original buy order that this match fulfills.
	expectedBuyer, ok := s.matchToBuyer[matchMsgID]
	if !ok || msg.Sender != expectedBuyer {
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

	// Mark this message as processed before mutating any state.
	s.completedSettlements[msg.ID] = struct{}{}

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

	// Antecedent must be a deliver message.
	deliverMsgID := msg.Antecedents[0]
	matchMsgID, ok := s.deliverToMatch[deliverMsgID]
	if !ok || matchMsgID == "" {
		return
	}

	// Sender identity gate: must be the original buyer for this match.
	expectedBuyer, ok := s.matchToBuyer[matchMsgID]
	if !ok || msg.Sender != expectedBuyer {
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
	matchMsgID := msg.Antecedents[0]

	// Validate that the antecedent is a match message with a known buyer.
	expectedBuyer, ok := s.matchToBuyer[matchMsgID]
	if !ok {
		return
	}
	// Validate sender is the original buyer.
	if msg.Sender != expectedBuyer {
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

// applyAssign processes an exchange:assign message.
//
// Only the operator may post assign tasks. Non-operator senders are rejected
// when OperatorKey is set.
//
// Payload fields:
//   - entry_id: optional inventory entry this task concerns
//   - task_type: category ("freshness", "validation", "compression", ...)
//   - reward: scrip to pay the claimant on accept
//   - exclusive_sender: if set, only this key may claim the task
//
// On success, a new AssignRecord is created in AssignOpen state and indexed
// in assignsByEntry and assignByID.
func (s *State) applyAssign(msg *Message) {
	if s.OperatorKey != "" && msg.Sender != s.OperatorKey {
		return
	}
	// Idempotency guard: skip re-application on replay.
	if _, exists := s.assignByID[msg.ID]; exists {
		return
	}
	var payload struct {
		EntryID         string `json:"entry_id"`
		TaskType        string `json:"task_type"`
		Reward          int64  `json:"reward"`
		ExclusiveSender string `json:"exclusive_sender"`
	}
	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		return
	}
	rec := &AssignRecord{
		AssignID:        msg.ID,
		EntryID:         payload.EntryID,
		TaskType:        payload.TaskType,
		Reward:          payload.Reward,
		Status:          AssignOpen,
		ExclusiveSender: payload.ExclusiveSender,
	}
	s.assignsByEntry[payload.EntryID] = append(s.assignsByEntry[payload.EntryID], rec)
	s.assignByID[msg.ID] = rec
}

// applyAssignClaim processes an exchange:assign-claim message.
//
// Antecedent: the assign message ID.
//
// Validation:
//   - Antecedent must reference a known assign in AssignOpen state.
//   - If ExclusiveSender is set on the assign, the sender must match.
//   - An agent may hold only one active claim at a time.
//
// On success, the assign transitions to AssignClaimed and claimedAssigns
// records the agentKey → assignID binding.
func (s *State) applyAssignClaim(msg *Message) {
	if len(msg.Antecedents) == 0 {
		return
	}
	assignID := msg.Antecedents[0]
	rec, ok := s.assignByID[assignID]
	if !ok || rec.Status != AssignOpen {
		return
	}
	// Exclusive sender constraint: if set, only the designated key may claim.
	if rec.ExclusiveSender != "" && msg.Sender != rec.ExclusiveSender {
		return
	}
	// Agent may hold only one active claim at a time.
	if existing, held := s.claimedAssigns[msg.Sender]; held && existing != "" {
		return
	}
	rec.Status = AssignClaimed
	rec.ClaimantKey = msg.Sender
	rec.ClaimMsgID = msg.ID
	s.claimedAssigns[msg.Sender] = assignID
}

// applyAssignComplete processes an exchange:assign-complete message.
//
// Antecedent: the assign-claim message ID.
//
// Validation:
//   - Antecedent must reference a known assign-claim for an assign in AssignClaimed state.
//   - Sender must be the agent who claimed the task.
//
// On success, the assign transitions to AssignCompleted, the result is stored,
// the claimedAssigns binding is released, and the record is indexed in
// pendingAssignResults for the operator to accept or reject.
func (s *State) applyAssignComplete(msg *Message) {
	if len(msg.Antecedents) == 0 {
		return
	}
	claimMsgID := msg.Antecedents[0]
	// Find the assign record via the claim message ID.
	var rec *AssignRecord
	for _, r := range s.assignByID {
		if r.ClaimMsgID == claimMsgID {
			rec = r
			break
		}
	}
	if rec == nil || rec.Status != AssignClaimed {
		return
	}
	// Sender must be the claimant.
	if msg.Sender != rec.ClaimantKey {
		return
	}
	rec.Status = AssignCompleted
	rec.CompleteMsgID = msg.ID
	rec.Result = msg.Payload
	// Release the agent's active claim slot.
	delete(s.claimedAssigns, msg.Sender)
	// Index for operator review.
	s.pendingAssignResults[msg.ID] = rec
}

// applyAssignAccept processes an exchange:assign-accept message.
//
// Only the operator may accept results.
// Antecedent: the assign-complete message ID.
//
// Validation:
//   - Sender must be the operator (if OperatorKey set).
//   - Antecedent must reference a known assign in AssignCompleted state via
//     pendingAssignResults.
//
// On success, the assign transitions to AssignAccepted and is removed from
// pendingAssignResults.
func (s *State) applyAssignAccept(msg *Message) {
	if s.OperatorKey != "" && msg.Sender != s.OperatorKey {
		return
	}
	if len(msg.Antecedents) == 0 {
		return
	}
	completeMsgID := msg.Antecedents[0]
	rec, ok := s.pendingAssignResults[completeMsgID]
	if !ok || rec.Status != AssignCompleted {
		return
	}
	rec.Status = AssignAccepted
	delete(s.pendingAssignResults, completeMsgID)
}

// applyAssignReject processes an exchange:assign-reject message.
//
// Only the operator may reject results.
// Antecedent: the assign-complete message ID.
//
// Validation:
//   - Sender must be the operator (if OperatorKey set).
//   - Antecedent must reference a known assign in AssignCompleted state via
//     pendingAssignResults.
//
// On success, the assign transitions to AssignRejected and is removed from
// pendingAssignResults. The task returns to AssignOpen so another agent may
// claim it.
func (s *State) applyAssignReject(msg *Message) {
	if s.OperatorKey != "" && msg.Sender != s.OperatorKey {
		return
	}
	if len(msg.Antecedents) == 0 {
		return
	}
	completeMsgID := msg.Antecedents[0]
	rec, ok := s.pendingAssignResults[completeMsgID]
	if !ok || rec.Status != AssignCompleted {
		return
	}
	rec.Status = AssignRejected
	delete(s.pendingAssignResults, completeMsgID)
	// Reset to open so a different agent may claim the task.
	rec.ClaimantKey = ""
	rec.ClaimMsgID = ""
	rec.CompleteMsgID = ""
	rec.Result = nil
	rec.Status = AssignOpen
}

// ClaimAssignPayment atomically checks that the assign record for the given
// completeMsgID is in AssignAccepted state and transitions it to AssignPaid,
// returning the record. Returns nil if the record is not found or is not in
// AssignAccepted state (e.g. a replayed accept whose bounty was already paid).
//
// This prevents double-payment on replayed exchange:assign-accept messages.
// State.Apply transitions AssignCompleted → AssignAccepted on the first accept
// and removes the record from pendingAssignResults. A replayed accept is a no-op
// in State.Apply but handleAssignAccept would still find the record via assignByID.
// ClaimAssignPayment gates payment to the single transition AssignAccepted → AssignPaid.
// Thread-safe.
func (s *State) ClaimAssignPayment(completeMsgID string) *AssignRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.assignByID {
		if r.CompleteMsgID == completeMsgID {
			if r.Status != AssignAccepted {
				return nil
			}
			r.Status = AssignPaid
			return r
		}
	}
	return nil
}

// ActiveAssigns returns a snapshot of all AssignRecord entries associated with
// the given entryID that are not yet in a terminal state (Accepted or Rejected).
// Pass "" to query assigns with no associated entry.
// Returns a copy of each record — callers must not mutate.
func (s *State) ActiveAssigns(entryID string) []*AssignRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	recs := s.assignsByEntry[entryID]
	out := make([]*AssignRecord, 0, len(recs))
	for _, r := range recs {
		if r.Status == AssignAccepted || r.Status == AssignRejected || r.Status == AssignPaid {
			continue
		}
		cp := *r
		out = append(out, &cp)
	}
	return out
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

// MarkStaleProvenanceEntries scans the inventory for entries belonging to
// sellerKey whose AcceptedProvenanceLevel exceeds currentLevel, and sets
// NeedsRevalidation=true on each. Returns the entry IDs that were flagged.
//
// This should be called whenever the exchange detects that a seller's provenance
// level has dropped (e.g., attestation expired or revoked). See InventoryEntry
// for the chosen re-validation semantics.
func (s *State) MarkStaleProvenanceEntries(sellerKey string, currentLevel int) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var flagged []string
	for _, entry := range s.inventory {
		if entry.SellerKey != sellerKey {
			continue
		}
		if entry.AcceptedProvenanceLevel > currentLevel {
			entry.NeedsRevalidation = true
			flagged = append(flagged, entry.EntryID)
		}
	}
	return flagged
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
	matchMsgID := s.deliverToMatch[deliverMsgID]
	if matchMsgID == "" {
		return "", false
	}
	return matchMsgID, true
}

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

// stripTagPrefix removes a convention tag prefix from a value if present.
// Convention dispatch sends full tag form (e.g. "exchange:content-type:analysis")
// where the engine expects bare enum values ("analysis"). Accept both.
func stripTagPrefix(val, prefix string) string {
	if strings.HasPrefix(val, prefix) {
		return val[len(prefix):]
	}
	return val
}

// stripDomainPrefixes normalizes domain values, stripping "exchange:domain:"
// prefix if convention dispatch sent the full tag form.
func stripDomainPrefixes(domains []string) []string {
	out := make([]string, len(domains))
	for i, d := range domains {
		out[i] = stripTagPrefix(d, "exchange:domain:")
	}
	return out
}
