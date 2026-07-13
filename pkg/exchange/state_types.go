// Package exchange implements the DontGuess exchange engine and state derivation.
//
// State is an in-memory materialized view of the campfire message log.
// No external database — the campfire IS the database. State can always
// be reconstructed by replaying the full message log (Replay).
package exchange

import (
	"fmt"
	"sync"
	"time"

	"github.com/3dl-dev/dontguess/pkg/identity"
	"github.com/3dl-dev/dontguess/pkg/matching"
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
	TagAssignExpire   = "exchange:assign-expire"

	// TagBuyMiss is the tag applied to a buy-miss standing offer message.
	// Sent by the engine as a reply to a buy with no matching inventory.
	TagBuyMiss = "exchange:buy-miss"

	// TagAssignAuctionClose is emitted by the engine when a Vickrey auction
	// window closes. The payload carries the winner key and Vickrey clearing
	// price, allowing state to correctly finalize on replay.
	TagAssignAuctionClose = "exchange:assign-auction-close"

	// TagConsume is emitted by the engine when a buyer completes a transaction,
	// recording a consume/accept behavioral signal for a delivered candidate.
	// Payload: {"entry_id": <string>, "buyer_key": <string>}.
	// Antecedent: the settle(complete) message ID.
	// This is the authoritative signal that the buyer used delivered content —
	// stronger than a hit (which only means the matcher returned a candidate).
	TagConsume = "exchange:consume"

	// TagSynthetic is applied to operator-emitted responses (match, buy-miss,
	// put-accept settle) that were triggered by synthetic load-test traffic.
	// Synthetic traffic is identified using demand.IsSynthetic — a single
	// canonical predicate shared with the demand backlog so both systems agree
	// on what counts as synthetic.
	//
	// Patterns covered: regression-*, *timeout-178*, "test"-class tasks,
	// zzqq/xyzzy probes, parallel-*, validation-preflight-*, and similar
	// infrastructure probes defined in pkg/demand/demand.go:IsSynthetic.
	//
	// Responses tagged exchange:synthetic are excluded from all exchange metrics:
	// the hit-rate reporter (ComputeHitRate) and the demand backlog (BuildBacklog).
	// This prevents load traffic from inflating or deflating production stats.
	TagSynthetic = "exchange:synthetic"

	SettlePhaseStrPutAccept   = "put-accept"
	SettlePhaseStrPutReject   = "put-reject"
	SettlePhaseStrBuyerAccept = "buyer-accept"
	SettlePhaseStrBuyerReject = "buyer-reject"
	// SettlePhaseStrBuyerAcceptReject is the operator's durable, wire-visible
	// rejection of a buyer-accept whose scrip hold could not be reserved
	// (ed2-D §3.6). It mirrors put-reject: emitted BEFORE the ErrBudgetExceeded
	// return so the buyer learns *why* (reason:"insufficient_scrip") instead of
	// only timing out. It has no state-fold handler (applySettle ignores unknown
	// phases) — it exists purely for the buyer's per-phase settle subscription.
	SettlePhaseStrBuyerAcceptReject = "buyer-accept-reject"
	SettlePhaseStrDeliver           = "deliver"
	SettlePhaseStrComplete          = "complete"

	SettlePhaseStrPreviewRequest      = "preview-request"
	SettlePhaseStrPreview             = "preview"
	SettlePhaseStrSmallContentDispute = "small-content-dispute"
	SettlePhaseStrFailed              = "failed"

	// SmallContentThreshold is the token count below which content is too small
	// for meaningful preview. Entries below this threshold use the small-content
	// dispute path instead.
	SmallContentThreshold = 500

	// MaxTeaserBytes is the hard length cap on a seller-authored public teaser
	// (content-confidentiality-envelope-541 §4.1, dontguess-4059). The teaser is
	// the ONLY seller-authored free-text that a settle(preview) echoes on the
	// public wire under encryption, so its size directly bounds intentional
	// exposure. applyPut DROPS (fail-closed) any put whose teaser exceeds this
	// cap: teaser bytes are treated as intentionally-published, so the cap makes
	// pasting whole content into the "teaser" a self-defeating dropped put rather
	// than a leak. 4 KiB is a small multiple of a realistic abstract (a few
	// paragraphs) and well under MaxDescriptionBytes (64 KiB) — a teaser is a
	// human abstract, not a second copy of the content.
	MaxTeaserBytes = 4 * 1024 // 4 KiB

	// CoOccurrenceK is the maximum number of co-occurring entries tracked per entry.
	// When the bounded map reaches capacity, the entry with the lowest count is evicted.
	CoOccurrenceK = 20

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

	// MaxContentBytes is the maximum allowed size for put content (1 MiB).
	// Content is TAINTED — enforce this limit before storing or hashing.
	MaxContentBytes = 1048576 // 1 MiB

	// BlossomOffloadThreshold is the content size (bytes) above which applyPut
	// offloads the full content to the Blossom blob store instead of inlining it
	// in the InventoryEntry / message log (dontguess-7783, design doc "Large
	// content via Blossom": preview stays inline, full deliver is a pointer +
	// client-side hash verification). Chosen well above the typical put size
	// (~10 KB observed in the live exchange, nostr-first rebuild decision doc
	// §NFR) so the common case is unaffected, while still catching genuinely
	// large payloads before they bloat the replicated state/log.
	BlossomOffloadThreshold = 32 * 1024 // 32 KiB

	// MinBytesPerToken is the minimum plausible bytes-per-token ratio used to
	// derive the content-size-based token_cost ceiling at put time.
	//
	// Token cost represents inference cost, not output size — a small output can
	// legitimately cost many tokens to produce (e.g. distilling a large codebase
	// into a short summary). However, there is a physical plausibility floor:
	// producing a result cannot require more tokens than 1000× the output byte size.
	// At 1000 tokens/byte the content is implausibly small relative to the claimed
	// inference cost — a signal of seller inflation rather than real computation.
	//
	// Examples:
	//   1.5 M tokens on  200 bytes → 7500 tokens/byte → REJECT (> 1000)
	//   10 000 tokens on  30 bytes → 333 tokens/byte  → ACCEPT (≤ 1000)
	//   1 000 000 tokens on 24 576 bytes → 41 tokens/byte → ACCEPT (≤ 1000)
	//
	// Derived ceiling: max_plausible_token_cost = content_size_bytes * MaxTokensPerByte.
	// A put exceeding that ceiling is silently dropped at applyPut time.
	// Exposed so tests can compute the same boundary.
	MaxTokensPerByte = int64(1000)

	// MinBytesPerToken is the reciprocal of MaxTokensPerByte, kept for symmetric
	// naming in test code that uses the "bytes per token" framing.
	// MinBytesPerToken = 1 / MaxTokensPerByte (fractional; use MaxTokensPerByte directly).
	MinBytesPerToken = int64(1) // only used in test helpers for content padding

	// MinTokenCost is the minimum accepted TokenCost value on a put (quality gate, dontguess-ed1).
	//
	// Puts claiming fewer than MinTokenCost tokens are rejected as low-value —
	// they represent trivial or synthetic computation (the live "test" entry had
	// token_cost=100) that pollutes matching quality and inflates the hit-rate metric.
	//
	// Value: 500 tokens. This is a conservative floor that admits all legitimate
	// cached-inference results (real inference rounds cost at minimum a few hundred
	// tokens for prompt + response) while rejecting the synthetic smoke-test class
	// (typically token_cost=100) identified in the measurement review §2.
	//
	// Composition with 46f: 46f enforces an upper bound (token_cost ≤ content_size *
	// MaxTokensPerByte). This constant enforces the lower bound. Both checks are in
	// applyPut and apply independently.
	MinTokenCost = int64(200)

	// HighReuseAcceptPriceNumerator is the accept-price percentage (of token_cost)
	// paid to sellers of high-reuse distilled artifacts (§4 class, dontguess-13a).
	// 85% vs the standard 70% — high-reuse artifacts earn a 15-point premium at put time.
	HighReuseAcceptPriceNumerator = int64(85)

	// StandardAcceptPriceNumerator is the accept-price percentage for standard puts.
	// 70% of token_cost is the baseline price paid by the exchange to sellers.
	StandardAcceptPriceNumerator = int64(70)

	// HighReuseResidualDenominator is the residual divisor for high-reuse artifacts.
	// residual = price / HighReuseResidualDenominator → 20% (1/5).
	// Standard residual is price / ResidualRate → 10% (1/10).
	// High-reuse entries earn double the residual per sale because of expected cross-project reuse.
	HighReuseResidualDenominator = int64(5)
)

// LegacyGrandfatherTTL bounds how long a pre-migration plaintext entry that was
// GRANDFATHERED during Replay under the team-tier encrypted-required cutover
// survives before it ages out (docs/design/content-confidentiality-envelope-541.md
// §7 Migration, dontguess-3ab1). The migration is a HARD cutover: every NEW put
// must be a v2 confidential envelope, but already-broadcast legacy plaintext
// (permanently public on the append-only relay — no claw-back is possible) is
// grandfathered rather than fail-closed-dropped, so the operator keeps its
// pre-migration inventory across the first restart instead of losing all of it.
// This bounded TTL is applied as the entry's default expiry at grandfather time
// (PutTimestamp + LegacyGrandfatherTTL) so the plaintext corpus drains from live
// inventory over time instead of lingering forever; a historical put-accept that
// carried an explicit expires_at still overrides it via applySettlePutAccept.
const LegacyGrandfatherTTL = 30 * 24 * time.Hour

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

	// CompressionTier is the caching tier for this entry: "hot", "warm", "cold", or ""
	// (unset). Set from the seller's put payload; defaults to "" when not provided.
	// Buyers may filter by tier; entries with "" match any tier filter.
	CompressionTier string

	// Content holds the raw bytes of the cached inference result — OR, when
	// BlobPointer is non-empty (dontguess-7783), only the precomputed inline
	// preview slice (15-25% of the full content, content-type-aware chunked).
	// Populated at put time from the base64-encoded "content" payload field.
	// ContentHash is computed from the full decoded content by applyPut — never
	// trusted from payload — regardless of whether the full bytes are offloaded.
	Content []byte

	// BlobPointer is the Blossom blob identifier for the full content when it
	// was too large to inline (dontguess-7783). Empty means Content holds the
	// full raw bytes (legacy / small-content path). Non-empty means Content
	// holds only the inline preview slice, and the full content must be
	// fetched via BlobStore.Fetch(BlobPointer) and verified against
	// ContentHash before delivery.
	BlobPointer string

	// WrappedCEKOperator is the seller's NIP-44 wrap of the content-encryption
	// key (CEK) to the OPERATOR's key, carried verbatim from the v2 put's
	// enc.key_wrap.wrapped (docs/design/content-confidentiality-envelope-541.md
	// §3.3/§3.5, dontguess-4bed). It is Replay-safe (folded from the put event),
	// and is NOT the raw CEK — the operator re-derives the CEK on demand via
	// NIP-44(operatorPriv, sellerPub, WrappedCEKOperator). The Phase-2 deliver
	// (dontguess-9e8) re-wraps that same CEK to the buyer, so it MUST persist on
	// the entry. Empty for legacy plaintext (individual-tier) entries.
	WrappedCEKOperator string

	// CiphertextHash is "sha256:"+hex(sha256(ciphertext)) — the hash over the
	// AEAD CIPHERTEXT (not plaintext), carried from the v2 put's
	// enc.ciphertext_hash. It is the buyer/Blossom integrity-verify value (§4.4
	// A7) and is DISTINCT from ContentHash, which is the operator-local
	// sha256(plaintext) dedup key. Empty for legacy plaintext entries.
	CiphertextHash string

	// Teaser is the seller-authored public abstract of the content
	// (content-confidentiality-envelope-541 §4.1, dontguess-4059). It is the
	// ONLY seller free-text that settle(preview) echoes on the public wire —
	// the real-content preview-chunk path was deleted because it broadcast
	// 15-25% of plaintext. Validated at applyPut: hard-capped at MaxTeaserBytes
	// (over-cap puts are dropped) and coherence-checked against the DECRYPTED
	// plaintext (an incoherent bait-and-switch teaser is dropped to "" while the
	// put is still accepted). Distinct from Description (Description is the terse
	// matching key; Teaser is the richer human abstract). Empty when the seller
	// authored no teaser or its coherence check failed.
	Teaser string

	// LegacyPlaintext marks an entry that was GRANDFATHERED during Replay of a
	// MIXED historical log: a pre-migration plaintext put (v<2, no "enc" envelope)
	// that was already accepted+broadcast before the team-tier encrypted-required
	// cutover (docs/design/content-confidentiality-envelope-541.md §6.3, §7,
	// dontguess-3ab1). Such an entry is folded — not fail-closed-dropped — so the
	// operator does not lose all pre-migration inventory on the first restart; the
	// plaintext is already permanently public, so grandfathering only lets it age
	// out gracefully via ExpiresAt (defaulted to PutTimestamp + LegacyGrandfatherTTL
	// at grandfather time). A LIVE plaintext put is NEVER grandfathered — it stays
	// fail-closed dropped (dontguess-4bed) so new plaintext injection is blocked.
	// Always false for v2 confidential entries and for individual-tier entries.
	LegacyPlaintext bool
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
	// CompressionTier is the optional compression tier filter (e.g. "lossless", "lossy", "raw").
	CompressionTier string
	// CreatedAt is when the buy message was received (nanoseconds).
	CreatedAt int64

	// GuaranteeDeadline is the absolute time by which the exchange guarantees
	// delivery for insured orders. Zero means no guarantee was purchased.
	// Set from the buy payload's guarantee_deadline field (seconds from receive
	// time). Engine checks this in handleSettle(complete) to detect misses.
	GuaranteeDeadline time.Time

	// InsuredAmount is the total scrip (match_price + premium) escrowed for an
	// insured order. Non-zero only when GuaranteeDeadline is set. Used by the
	// engine to issue the full refund on deadline miss.
	InsuredAmount int64
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

// DefaultClaimTimeoutMinutes is the default TTL for an assign claim.
// The claimant must submit assign-complete within this window or the claim
// is expired and the task returns to AssignOpen for reclaim.
// Configurable per-assign via claim_timeout_minutes in the assign payload.
const DefaultClaimTimeoutMinutes = 15

// DefaultAuctionWindowSeconds is the default duration of a Vickrey auction
// window. Workers may submit bids during this window; after it closes the
// engine selects the lowest bidder and pays at the second-lowest price.
const DefaultAuctionWindowSeconds = 60

// AuctionBidCeilMultiplier is the maximum bid expressed as a multiple of
// base_reward. Bids exceeding base_reward * AuctionBidCeilMultiplier are
// rejected as safety measure against runaway pricing.
const AuctionBidCeilMultiplier = 10

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
	// AssignExpired is a terminal state for an unclaimed standing assign whose
	// DeadlineAt has passed. The transition AssignOpen → AssignExpired is applied
	// by applyAssignExpire when the operator sweep emits an assign-expire whose
	// antecedent is the assign ID (not a claim ID). This is the event-sourced-pure
	// replacement for the removed wall-clock DeadlineAt guard in applyAssignClaim
	// (ruling 2026-07-08): an expired standing offer is closed by an operator event
	// in the log rather than by the fold reading time.Now(), so it is unclaimable
	// deterministically on replay.
	AssignExpired
)

// String returns a human-readable name for the AssignStatus value.
func (s AssignStatus) String() string {
	switch s {
	case AssignOpen:
		return "assign-open"
	case AssignClaimed:
		return "assign-claimed"
	case AssignCompleted:
		return "assign-completed"
	case AssignAccepted:
		return "assign-accepted"
	case AssignRejected:
		return "assign-rejected"
	case AssignPaid:
		return "assign-paid"
	case AssignExpired:
		return "assign-expired"
	default:
		return fmt.Sprintf("assign-unknown(%d)", int(s))
	}
}

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
	// ClaimExpiresAt is the deadline by which the claimant must submit
	// assign-complete. Computed at claim time as min(claim.expires_at,
	// assign_receive_time + claim_timeout_minutes). Zero value means no expiry
	// is tracked (e.g. for legacy records without expiry data).
	ClaimExpiresAt time.Time
	// CompleteMsgID is the message ID of the exchange:assign-complete message.
	CompleteMsgID string
	// Result is the agent-supplied result payload (from assign-complete).
	// Stored as raw JSON for the operator to inspect before accepting.
	Result []byte
	// ExclusiveSender restricts who may claim this task. If non-empty, only the
	// agent with this public key may claim. Used for seller-only or buyer-only tasks.
	ExclusiveSender string
	// ClaimTimeoutMinutes is the maximum minutes a claimant has to complete the
	// task. Sourced from the assign payload's claim_timeout_minutes field.
	// Defaults to DefaultClaimTimeoutMinutes (15) if not set.
	ClaimTimeoutMinutes int
	// AssignReceivedAt is the engine-observed receive time of the assign message,
	// used as the reference point for computing ClaimExpiresAt. Stored as UTC.
	AssignReceivedAt time.Time
	// AuctionDeadline is the time after which no new bids are accepted and the
	// engine selects the Vickrey winner. Zero means no auction (legacy flow).
	AuctionDeadline time.Time
	// AuctionBids holds all bids received during the open auction window.
	// Each entry corresponds to one assign-claim message with a bid field.
	AuctionBids []AuctionBid
	// DifficultyTier is the post-facto difficulty label derived from the bid
	// distribution after the auction closes ("low", "medium", "high").
	// Empty until the auction is finalized.
	DifficultyTier string
	// VickreyPrice is the clearing price computed at auction close: the
	// second-lowest bid (or lowest bid if only one bidder). The claimant is
	// paid this amount instead of the base Reward.
	VickreyPrice int64
	// BuyMsgID is the campfire message ID of the originating exchange:buy that
	// triggered this assign. Non-empty only for task_type="brokered-match" assigns.
	// Used to correlate the brokered-match assign back to its originating buy order.
	BuyMsgID string

	// DeadlineAt is the absolute time after which this standing assign should be
	// ignored even if unclaimed. Used for prediction-derived brokered-match assigns
	// to bound assignByID growth (A9 mitigation). Zero means no deadline.
	// Workers may not claim assigns past their DeadlineAt.
	DeadlineAt time.Time

	// ClaimedAt is the wall-clock time when the assign was claimed. Populated by
	// applyAssignClaim (and applyAssignAuctionClose for auction-based claims).
	// Used together with CompletedAt to compute claim-to-complete latency for the
	// actuarial table.
	ClaimedAt time.Time

	// CompletedAt is the wall-clock time when assign-complete was processed.
	// Populated by applyAssignComplete. Zero if the assign was never completed.
	CompletedAt time.Time

	// GuaranteeDeadline is the absolute time by which the exchange guarantees
	// delivery for insured orders. Zero means no guarantee was purchased.
	// Set from the buy payload's guarantee_deadline field (converted to absolute
	// time at buy-time). Engine checks this in handleSettle(complete) to detect
	// deadline misses and issue automatic dispute-refunds.
	GuaranteeDeadline time.Time

	// InsuredAmount is the total scrip amount (match_price + premium) held in
	// escrow for an insured order. Non-zero only when GuaranteeDeadline is set.
	// Used by the engine to issue the full refund on deadline miss.
	InsuredAmount int64
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

	// +3 for each entry where CrossAgentConvergenceThreshold+ distinct buyers have completed
	for _, buyers := range s.EntryBuyerMap {
		if len(buyers) >= matching.CrossAgentConvergenceThreshold {
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

// AuctionBid records a single bid submitted during a Vickrey auction window.
type AuctionBid struct {
	// WorkerKey is the hex-encoded public key of the bidding agent.
	WorkerKey string
	// BidAmount is the scrip amount the worker is willing to accept for the task.
	BidAmount int64
	// Timestamp is the time the bid was recorded (derived from the assign-claim message).
	Timestamp time.Time
}

// coOccurrenceMap is a bounded map tracking how often entry IDs co-occur with
// a given entry. When the map reaches CoOccurrenceK entries, the entry with the
// lowest count is evicted before inserting the new one. This bounds memory to
// O(K) per entry without requiring an external LRU package.
type coOccurrenceMap struct {
	counts map[string]int // entryID -> co-occurrence count
}

// newCoOccurrenceMap allocates an empty co-occurrence map.
func newCoOccurrenceMap() *coOccurrenceMap {
	return &coOccurrenceMap{counts: make(map[string]int)}
}

// increment adds one to the count for peerID, evicting the lowest-count entry
// if the map is at capacity (CoOccurrenceK).
func (m *coOccurrenceMap) increment(peerID string) {
	if _, exists := m.counts[peerID]; exists {
		m.counts[peerID]++
		return
	}
	// Evict lowest-count entry if at capacity.
	if len(m.counts) >= CoOccurrenceK {
		minKey := ""
		minVal := int(^uint(0) >> 1) // MaxInt
		for k, v := range m.counts {
			if v < minVal {
				minVal = v
				minKey = k
			}
		}
		delete(m.counts, minKey)
	}
	m.counts[peerID] = 1
}

// topK returns up to k entry IDs sorted by co-occurrence count descending.
// Returns IDs only (not counts).
func (m *coOccurrenceMap) topK(k int) []string {
	if len(m.counts) == 0 {
		return nil
	}
	// Collect and sort by count descending.
	type kv struct {
		id    string
		count int
	}
	pairs := make([]kv, 0, len(m.counts))
	for id, c := range m.counts {
		pairs = append(pairs, kv{id, c})
	}
	// Insertion sort is fine for K=20 (small n).
	for i := 1; i < len(pairs); i++ {
		for j := i; j > 0 && pairs[j].count > pairs[j-1].count; j-- {
			pairs[j], pairs[j-1] = pairs[j-1], pairs[j]
		}
	}
	if k > len(pairs) {
		k = len(pairs)
	}
	out := make([]string, k)
	for i := 0; i < k; i++ {
		out[i] = pairs[i].id
	}
	return out
}

// FederationNodeProfile is the trust profile for a counterparty (local agent or
// federated exchange node). Computed by the slow loop from observed behavioral
// signals. Local agents and federation nodes use the same model — they differ
// only in starting trust_score (local: 0.7, new federation node: 0.4).
//
// See docs/design/semantic-matching-marketplace.md §4A.
type FederationNodeProfile struct {
	// SenderKey is the hex-encoded Ed25519 public key of the counterparty.
	SenderKey string
	// TrustScore is the computed trust score in [0.0, 1.0]. New nodes start
	// at 0.4; local agents start at 0.7. Both converge toward 1.0 via behavioral
	// history. Written by the slow loop.
	TrustScore float64
	// HopDepth is the median observed provenance hop depth for this sender.
	// Approximated from Antecedents chain length. Advisory input only (F4).
	HopDepth int
	// FirstSeenAt is the timestamp of the first message from this sender.
	FirstSeenAt time.Time
	// TransactionCount is the number of completed transactions (settle:complete)
	// traced to this sender. Used for new-node dual guard exit condition.
	TransactionCount int
}

// IsNewNode returns true if the sender has not yet exited new-node status.
// Both conditions must be satisfied to exit new-node status:
//  1. transaction_count >= NewNodeTransactionThreshold
//  2. age since FirstSeenAt >= NewNodeAgeDuration
func (p *FederationNodeProfile) IsNewNode(now time.Time) bool {
	if p.TransactionCount < NewNodeTransactionThreshold {
		return true
	}
	return now.Sub(p.FirstSeenAt) < NewNodeAgeDuration
}

const (
	// NewNodeTransactionThreshold is the minimum completed transaction count
	// required to exit new-node status. See §4A dual guard.
	NewNodeTransactionThreshold = 50

	// NewNodeAgeDuration is the minimum age from first observation required
	// to exit new-node status.
	NewNodeAgeDuration = 30 * 24 * time.Hour

	// NewNodeTrustScoreStart is the initial trust_score for new federation nodes.
	NewNodeTrustScoreStart = 0.4

	// SenderHopDepthWindowSize is the maximum number of hop-depth observations
	// retained per sender. Older observations are evicted when the window is full.
	// The slow loop only needs a statistical median, not the full history.
	// Matches the CoOccurrenceK philosophy of bounding per-sender state.
	SenderHopDepthWindowSize = 1000

	// LocalAgentTrustScoreStart is the initial trust_score for local agents.
	LocalAgentTrustScoreStart = 0.7

	// NewNodeTrustThreshold is the trust_score below which a node is routed to
	// inline matching regardless of BrokeredMatchMode. See §4A dual guard.
	NewNodeTrustThreshold = 0.6
)

// State is the in-memory materialized view of the exchange campfire log.
// It is rebuilt on startup by Replay and updated incrementally by Apply.
//
// All mutation must go through Apply — callers must not modify exported maps
// directly.
type State struct {
	mu sync.RWMutex

	// OperatorKey is the hex-encoded Ed25519 public key of the exchange operator.
	// Operator-only messages (put-accept, put-reject, match, deliver) are
	// rejected when their Sender does not match this key. Those rejections are
	// counted + alarmed via onFoldDenial (dontguess-9ed), not dropped silently.
	OperatorKey string

	// operatorSigner is the operator's secp256k1 identity (private, does ECDH).
	// It is the ONLY key that can NIP-44-unwrap a v2 put's wrapped_cek_operator,
	// so applyPut needs it to decrypt-then-gate on team tier
	// (docs/design/content-confidentiality-envelope-541.md §3.1(2)/§3.6,
	// dontguess-4bed). Set at engine construction (NewEngine) from
	// EngineOptions.OperatorSigner, BEFORE Replay, so both live-fold and Replay
	// can decrypt. nil on the individual tier (ScripStore == nil, local socket,
	// already confidential) — the legacy plaintext path never needs it.
	operatorSigner identity.Signer

	// encryptedRequired is the §6 team-tier fail-closed flag. When true, applyPut
	// DROPS any 3401 that carries a legacy plaintext "content" field, lacks a
	// well-formed v2 "enc" envelope, or has v<2 — a downgrade cannot reopen the
	// content-confidentiality leak. Set true by NewEngine only when BOTH a
	// ScripStore (team tier) AND an operatorSigner are present (production serve
	// wires the two together inside its relays-attached branch). false on the
	// individual tier keeps the plaintext path legal and byte-for-byte unchanged.
	encryptedRequired bool

	// onFoldDenial, when non-nil, is invoked when a security-relevant fold guard
	// (operator-only settlement guard, or the buyer-identity gate) rejects a
	// message — it counts + alarms the drop. Wired by NewEngine to increment the
	// engine's DegradationMetrics and log. nil when State is constructed directly
	// in tests: the guard still rejects, just without counting. Caller holds s.mu.
	onFoldDenial func(reason foldDenialReason, msg *Message)

	// replaying is true only for the duration of Replay. Fold-guard denials seen
	// during a full log replay MUST NOT re-increment the live counters — the log
	// is replayed on every engine restart / state rebuild, so counting there
	// would inflate the alarm counters without a new real-time rejection. Live
	// Apply (not replaying) is the only path that counts.
	replaying bool

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

	// deliverTimeByMatch maps a match message ID to the Timestamp (nanoseconds
	// since epoch) of the OPERATOR-AUTHORED settle(deliver) message for that
	// match. This is the operator-trusted, replay-deterministic reference time
	// the guarantee deadline-miss verdict is derived from (relay-transport.md §4
	// ADV-10 + §Sequencer): the deliver phase is operator-authored (only the
	// operator emits it — see applySettleDeliver's operator-sender guard), so its
	// Timestamp is both operator-set and persisted, unlike the buyer-authored
	// settle(complete) Timestamp which a counterparty controls and must NEVER
	// drive a fold decision. Key: match message ID.
	deliverTimeByMatch map[string]int64

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

	// entryConsumeCount tracks the number of exchange:consume signals per entry.
	// Key: entryID. Incremented by applyConsume when a TagConsume message is
	// processed. This is the M5 consume signal (dontguess-860): a buyer who
	// completes settle(complete) and triggers emitConsumeSignal is counted here.
	// Unlike EntryBuyerMap (which tracks distinct buyers), consume count tracks
	// total consume events including repeat consumers.
	// Reset on Replay — rebuilt from the campfire log.
	entryConsumeCount map[string]int

	// entryDeliverCount tracks the number of times each entry has been delivered
	// to a buyer via settle(deliver). Key: entryID.
	// Incremented by applySettleDeliver when the entry_id can be derived from
	// the antecedent chain (deliver → buyer-accept → match → matchToEntry).
	// Used by AllEntryBehavioralSignals to populate BehavioralSignals.DeliverCount
	// for the false-positive demotion signal (dontguess-046).
	// Reset on Replay — rebuilt from the campfire log.
	entryDeliverCount map[string]int

	// buyerDeliverCount tracks, per buyer pubkey (GLOBAL across all entries),
	// the number of settle(deliver) events addressed to that buyer.
	// buyerConsumeCount tracks, per buyer pubkey, the number of settle(complete)
	// events that buyer successfully closed.
	// Together these give a buyer's personal completion rate
	// (buyerConsumeCount[k] / buyerDeliverCount[k]), used by
	// entryDeliverAbandonWeight (dontguess-1856) to distinguish a chronic
	// never-completer (funded griefer) hammering one entry from a broad set of
	// otherwise-healthy buyers each abandoning once. This is ADDED ALONGSIDE the
	// existing per-entry entryDeliverCount/entryConsumeCount aggregates above —
	// those are left untouched.
	// Populated at the SAME call sites as the entry-level counters
	// (applySettleDeliver / applySettleComplete in state_settle.go).
	// Reset on Replay — rebuilt from the campfire log.
	buyerDeliverCount map[string]int
	buyerConsumeCount map[string]int

	// entryDeliverBuyerCount tracks, per entry, per buyer, how many
	// settle(deliver) events that buyer received for that entry. Key: entryID
	// -> buyerKey -> count. This is what lets ExpiryCandidates identify WHICH
	// buyers' personal completion rates are relevant to a given entry's
	// deliver-without-consume signal (dontguess-1856).
	// Reset on Replay — rebuilt from the campfire log.
	entryDeliverBuyerCount map[string]map[string]int

	// entryConsumeBuyerCount is the settle(complete) counterpart of
	// entryDeliverBuyerCount: per entry, per buyer, how many settle(complete)
	// events that buyer closed for that entry. Key: entryID -> buyerKey ->
	// count. Together with entryDeliverBuyerCount, this lets
	// entryDeliverAbandonWeightLocked compute a buyer's EXTERNAL completion
	// rate (buyerDeliverCount/buyerConsumeCount MINUS this entry's own
	// contribution) rather than a self-referential one, so a single-episode
	// abandonment (no track record on any other entry) is not mistaken for
	// an established chronic griefer (dontguess-1856).
	// Reset on Replay — rebuilt from the campfire log.
	entryConsumeBuyerCount map[string]map[string]int

	// priceAdjustments holds dynamic price multipliers written by the fast pricing loop.
	// Key: entryID. The multiplier is applied on top of computePrice's base result.
	// Stale adjustments (past ExpiresAt) are treated as 1.0x by computePrice.
	// Not reset on Replay — externally written, not derived from the campfire log.
	priceAdjustments map[string]PriceAdjustment

	// wireToStore aliases an operator-emitted message's on-wire CONTENT-HASH id
	// (the id a relay buyer sees and e-tags) back to its pre-signature STORE id
	// (the id every state map is keyed by) — the read-time GAP-1 reconciliation
	// for the team-tier settle chain (dontguess-55c, docs/design/
	// settle-wire-id-reconciliation-55c.md). An operator match/preview/deliver is
	// keyed in state by its random store id (sendLocalOperatorMessage), but the
	// Outbox RE-SIGNS it on publish → a DIFFERENT content-hash wire id; a buyer can
	// only ever e-tag that wire id, so the settle antecedent misses every store-keyed
	// map without this alias. It is written LIVE by the Outbox at publish time
	// (WithAliasRegistrar → RegisterWireAlias) and rebuilt on restart in
	// seedEmittedFromStore's operator-record loop; resolveAlias consults it at every
	// buyer-referenced-operator-id resolution. Key: wire id. Value: store id.
	//
	// NOT reset on Replay (precedent: priceAdjustments / brokerMatchIDs /
	// federationProfiles) — State has no signer to re-derive the wire id, so the
	// Outbox / restart seed repopulate it; a Replay that wiped it would strand every
	// in-flight wire-id settle. No-Outbox paths (the ~32K in-process suite, the
	// individual tier) never register an alias, so it stays empty and resolveAlias is
	// the identity — behavior is byte-for-byte unchanged there.
	wireToStore map[string]string

	// matchToBuyHold indexes match message IDs to reservation IDs from
	// scrip-buy-hold messages. Populated by applyScripBuyHold during Replay/Apply.
	// O(1) alternative to scanning the full log in findExistingBuyerAcceptHold.
	// Key: matchMsgID (BuyMsg field from BuyHoldPayload). Value: reservationID.
	matchToBuyHold map[string]string

	// matchToBuyHoldAmount indexes match message IDs to the ORIGINAL held amount
	// (price + fee) from the scrip-buy-hold event. Populated alongside
	// matchToBuyHold by applyScripBuyHold. Used by restoreExistingHold to restore
	// the exact scrip held at buyer-accept time on restart rather than recomputing
	// from the current (possibly drifted) dynamic price (dontguess-471).
	// Key: matchMsgID (BuyMsg field). Value: amount.
	matchToBuyHoldAmount map[string]int64

	// settledMatches is the DURABLE settled-match set (dontguess-400 FIX-M1,
	// design §1.4/§4). A match msg ID is in this set once its scrip settlement
	// has been durably emitted. It gates BOTH restoreExistingHold (never
	// re-hydrate a settled match's consumed reservation) and performScripSettlement
	// (never emit a second scrip-settle for a match). It is rebuilt on Replay by
	// applyScripSettle folding the durable scrip-settle log, and is also marked
	// live by performScripSettlement (via MarkMatchSettled) so the guard holds
	// within a single session — before the scrip-settle folds on the next poll.
	// Keyed on matchMsgID (SettlePayload.MatchMsg). Reset on Replay, rebuilt from
	// the log. Unlike completedSettlements (keyed on the complete msg.ID, which a
	// fresh buyer-accept→complete pair with new msg IDs trivially evades) this is
	// keyed on the single-use match identity, so a re-accept + re-complete of an
	// already-settled match cannot mint a second settlement.
	settledMatches map[string]struct{}

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

	// claimMsgToAssign maps a claim message ID to the assign ID it corresponds to.
	// Populated when an assign is claimed; deleted on expire or reject.
	// Enables O(1) lookup in applyAssignComplete and applyAssignExpire.
	// Key: claimMsgID -> assignID.
	claimMsgToAssign map[string]string

	// completeMsgToAssign maps a complete message ID to the assign ID it corresponds to.
	// Populated when an assign is completed; enables O(1) lookup in ClaimAssignPayment.
	// Key: completeMsgID -> assignID.
	completeMsgToAssign map[string]string

	// buyMissOffers tracks standing buy-miss offers keyed by task description hash.
	// One offer per task hash — no duplicates. Offers expire after BuyMissOfferTTL.
	// Key: SHA-256 hex of canonical task description.
	buyMissOffers map[string]*BuyMissOffer

	// matchToBuyMsgID maps a match message ID to the buy message ID it fulfills.
	// Populated by applyMatch from the match antecedent.
	matchToBuyMsgID map[string]string

	// matchGuarantee maps a match message ID to the insurance terms for the
	// corresponding buy order. Set in applyMatch when the buy order has a
	// GuaranteeDeadline. Persists after the order leaves activeOrders so the
	// settle(complete) handler can still check the deadline.
	// Key: matchMsgID. Value: [deadline, insuredAmount].
	matchGuarantee map[string][2]int64 // [deadline_unix_ns, insured_amount]

	// brokerAssigns maps buy message IDs to the assign message IDs of the
	// brokered-match task posted for that buy. Populated by applyAssign when
	// task_type="brokered-match" and buy_msg_id is present. Derived from the
	// campfire log (reset on Replay). Key: buyMsgID -> assignID.
	brokerAssigns map[string]string

	// brokerMatchIDs is the set of exchange:match message IDs that were produced
	// by a brokered-match assign (task_type="brokered-match"). Populated externally
	// by the engine when a brokered-match assign is accepted; NOT reset on Replay
	// (it is written by the engine, not derived from the campfire log).
	// Key: matchMsgID.
	brokerMatchIDs map[string]struct{}

	// brokeredAcceptedOrders is the count of accepted orders whose fulfilling
	// match was brokered. Reset on Replay; derived from brokerMatchIDs + log.
	brokeredAcceptedOrders int

	// brokeredCompletions is the count of settle(complete) messages whose
	// antecedent chain traces back to a brokered match. Reset on Replay.
	brokeredCompletions int

	// debtorScores holds pre-computed debtor priority scores per buyer key.
	// Key: agentKey (hex-encoded Ed25519 public key of the buyer).
	// Value: score in [0.0, 1.0] — 1.0 means no debt (full priority), lower
	// means higher outstanding debt (lower match priority).
	//
	// This map is NOT reset on Replay — it is externally written via
	// SetDebtorScore when the scrip store signals debt changes (e.g. after
	// loan-mint or loan-repay). It is a callback/hook pattern to avoid a
	// cross-package import of pkg/scrip inside pkg/exchange.
	debtorScores map[string]float64

	// coOccurrence tracks which entries co-occur in buyer sessions.
	// Key: entryID. Value: bounded map of peer entryID -> co-occurrence count.
	// Updated by UpdateCoOccurrence after each settle(complete).
	// Reset on Replay — derived from the settle log. Reset on Replay.
	coOccurrence map[string]*coOccurrenceMap

	// senderHopDepth tracks, per sender key, the observed provenance hop depths
	// across all messages from that sender. Hop depth is approximated from the
	// Antecedents chain length. Populated by applyLocked for every message.
	// Reset on Replay (re-derived from the log).
	// Key: senderKey (hex-encoded Ed25519 public key). Value: slice of hop depths.
	senderHopDepth map[string][]int

	// federationProfiles holds the computed trust profile for each sender key.
	// Key: senderKey. Written by UpdateFederationProfile (called by the slow loop
	// or explicitly). Reset on Replay — re-derived from senderHopDepth.
	federationProfiles map[string]*FederationNodeProfile

	// heldForReview tracks put message IDs that exceed the auto-accept cap.
	// These puts remain in pendingPuts (no campfire state change — no new convention
	// op) but are tagged so the auto-accept loop ignores them and the operator CLI
	// can surface them via PutsHeldForReview().
	//
	// NOT reset on Replay — re-derived by the auto-accept loop on each tick.
	// On operator restart the loop re-evaluates all pending puts against the cap.
	// Set by HoldPutForReview; pruned by PruneHeldForReview when a put leaves pending.
	heldForReview map[string]struct{}

	// contentHashIndex is the deduplication index for put content hashes (dontguess-ed1).
	// Key: sha256: prefixed content hash. Value: empty struct (presence check).
	//
	// Tracks hashes of entries in pendingPuts and inventory. When applyPut
	// sees a content_hash already in this index, the put is silently dropped —
	// the exchange already holds the same content. This prevents sellers from
	// re-putting identical content under a different description to game pricing.
	//
	// Lifecycle:
	//   - Insert on applyPut (when the put is accepted into pendingPuts).
	//   - applySettlePutReject does NOT remove from index — prevents re-put of
	//     the same content immediately after rejection (e.g., junk content).
	//   - Reset on Replay — rebuilt from the log by re-running applyPut for all msgs.
	contentHashIndex map[string]struct{}

	// blobStore is the optional Blossom client seam (dontguess-7783). When set,
	// applyPut offloads oversize content (> BlossomOffloadThreshold bytes) to the
	// blob store instead of retaining the full raw bytes inline in the entry's
	// Content field. Nil means legacy behavior: all content (up to MaxContentBytes)
	// stays inline, exactly as before this change. See SetBlobStore.
	blobStore BlobStore
}
