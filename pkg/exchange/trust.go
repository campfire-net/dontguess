// Package exchange — trust gating for exchange operations.
//
// This replaces the former campfire pkg/provenance dependency. The trust model
// is deliberately narrow (NOT a web-of-trust — a self-minted root cartel is a
// trusted intermediary reintroduced one layer down; see
// docs/design/convergence-sybil-defense.md §"Family 2 — Attestation Graph"):
//
//  1. NIP-42 allowlist — the set of fleet npubs admitted to the team relay.
//     Membership secures the pipe: an allowlisted key is a vetted fleet member.
//  2. Operator write authority — match/settle(put-*)/mint/burn are operator-only.
//     The operator key is the single long-lived npub that signs those events.
//  3. Reputation floor — the EXISTING pkg/exchange behavioral reputation score
//     (SellerStats.Reputation) gates sell-side operations: a seller who has
//     burned trust (disputes, small-content refunds) is blocked from putting
//     more inventory, independent of allowlist membership.
//
// Three trust tiers replace the former 4-level provenance ladder
// (anonymous/claimed/contactable/present). The claimed/contactable distinction
// does not survive an allowlist model — you are either a vetted fleet member or
// you are not:
//
//	anonymous   (0): not allowlisted — buy, inventory-read, price-history-read
//	allowlisted (1): a fleet member on the NIP-42 allowlist — put, assign,
//	                 settle(buyer-accept/reject/complete/dispute)
//	operator    (2): the operator key — match, mint, burn, rate-publish,
//	                 convention promote/supersede, settle(put-accept/reject/deliver)
//
// Operators can override these defaults in the exchange config file
// (trust_levels section) without rebuilding.
//
// References:
//   - docs/convention/core-operations.md §9 (Conformance Checker)
//   - docs/design/nostr-first-rebuild-decision.md §"Provenance/trust gate [TEAM]"
//   - docs/design/convergence-sybil-defense.md (web-of-trust rejected)
package exchange

import (
	"errors"
	"fmt"
	"strings"
	"sync"
)

// TrustLevel is the authority tier a sender holds for exchange operations.
// The integer values are stable (0=anonymous … 2=operator) so that the
// per-entry provenance level (AcceptedProvenanceLevel), stored as a plain int,
// keeps working unchanged.
type TrustLevel int

const (
	// TrustAnonymous is any key not on the allowlist and not the operator.
	TrustAnonymous TrustLevel = 0
	// TrustAllowlisted is a fleet member admitted to the NIP-42 allowlist.
	TrustAllowlisted TrustLevel = 1
	// TrustOperator is the exchange operator key (write authority for
	// match/settle(put-*)/mint/burn).
	TrustOperator TrustLevel = 2
)

// String renders a TrustLevel as its config/name form.
func (l TrustLevel) String() string {
	switch l {
	case TrustAnonymous:
		return "anonymous"
	case TrustAllowlisted:
		return "allowlisted"
	case TrustOperator:
		return "operator"
	default:
		return fmt.Sprintf("TrustLevel(%d)", int(l))
	}
}

// Operation is an exchange operation type.
type Operation string

const (
	// Core operations (put, buy, match, settle).
	OperationPut    Operation = "put"
	OperationBuy    Operation = "buy"
	OperationMatch  Operation = "match"
	OperationSettle Operation = "settle"

	// Extended operations not in core convention v0.1, defined here for completeness.
	//
	// OperationAssign is the legacy flat assign bucket (allowlisted). It is retained
	// for config/back-compat but is NO LONGER the trust op the dispatch gate routes
	// assign messages through — tagToTrustOp maps each of the seven assign-family
	// tags to its own per-sub-op Operation below, so the operator-only sub-ops are
	// not loosened to allowlisted (docs/design/relay-transport.md §2.4a D3).
	OperationAssign              Operation = "assign"
	OperationMint                Operation = "mint"
	OperationBurn                Operation = "burn"
	OperationRatePublish         Operation = "rate-publish"
	OperationConventionPromote   Operation = "convention-promote"
	OperationConventionSupersede Operation = "convention-supersede"

	// Read-only operations (inventory browse, price history).
	OperationInventoryRead    Operation = "inventory-read"
	OperationPriceHistoryRead Operation = "price-history-read"

	// OperationConsume is the trust op for exchange:consume (TagConsume) signals.
	// Consume signals are operator-authored (emitConsumeSignal) — they feed the
	// per-entry behavioral booster (entryConsumeCount), so a forged non-operator
	// consume would let any sender inflate a seller's demand signal. It is
	// operator-only; tagToTrustOp routes TagConsume through this op so a forged
	// consume is gated + counted at the dispatch trust gate rather than passing
	// unchecked (dontguess-9ed).
	OperationConsume Operation = "consume"

	// Per-sub-op assign operations. The assign(3405) kind is shared: the operator
	// authors the task post, accept, reject, expire, and auction-close; workers
	// author claim and complete. Each sub-op carries its own trust level (see
	// defaultOperationLevels) so a fleet member cannot forge an operator-authored
	// finalization while still being able to claim/complete work.
	OperationAssignPost         Operation = "assign-post"
	OperationAssignClaim        Operation = "assign-claim"
	OperationAssignComplete     Operation = "assign-complete"
	OperationAssignAccept       Operation = "assign-accept"
	OperationAssignReject       Operation = "assign-reject"
	OperationAssignExpire       Operation = "assign-expire"
	OperationAssignAuctionClose Operation = "assign-auction-close"
)

// SettlePhase is a settlement phase within the settle operation.
// The trust requirement depends on both the operation and the settle phase.
type SettlePhase string

const (
	SettlePhaseBuyerAccept SettlePhase = "buyer-accept"
	SettlePhaseBuyerReject SettlePhase = "buyer-reject"
	SettlePhasePutAccept   SettlePhase = "put-accept"
	SettlePhasePutReject   SettlePhase = "put-reject"
	SettlePhaseDeliver     SettlePhase = "deliver"
	SettlePhaseComplete    SettlePhase = "complete"
	SettlePhaseDispute     SettlePhase = "dispute"
	// SettlePhasePreview is the operator-authored preview settlement the operator
	// emits in response to a buyer's preview-request. The convention lists it as
	// operator-sent; the trust map previously omitted it, so a settle:preview
	// reaching the dispatch trust gate was rejected as an unknown phase. It is
	// operator-only (docs/design/relay-transport.md §2.4a D3).
	SettlePhasePreview SettlePhase = "preview"
	// SettlePhasePreviewRequest is the buyer-authored request for a content
	// preview (the preview-before-purchase model). It is a fleet-member
	// (allowlisted) operation. The trust map previously OMITTED it, so
	// RequiredLevel returned an "unknown settle phase" error and the dispatch
	// gate silently dropped every preview-request — breaking preview-before-
	// purchase. Added at TrustAllowlisted (dontguess-471).
	SettlePhasePreviewRequest SettlePhase = "preview-request"
	// SettlePhaseSmallContentDispute is the buyer-authored automated auto-refund
	// dispute for below-threshold content. It is a fleet-member (allowlisted)
	// operation. Like preview-request it was OMITTED from the trust map, so the
	// dispatch gate silently dropped it — breaking the automated small-content
	// refund. Added at TrustAllowlisted (dontguess-471).
	SettlePhaseSmallContentDispute SettlePhase = "small-content-dispute"
)

// defaultOperationLevels is the compiled-in default mapping.
//
// Collapse note vs the former 4-level provenance defaults: put (was claimed=1)
// and assign (was contactable=2) both require allowlisted membership now; all
// former "present" ops require the operator key.
var defaultOperationLevels = map[Operation]TrustLevel{
	OperationBuy:                 TrustAnonymous,
	OperationInventoryRead:       TrustAnonymous,
	OperationPriceHistoryRead:    TrustAnonymous,
	OperationPut:                 TrustAllowlisted,
	OperationAssign:              TrustAllowlisted,
	OperationMint:                TrustOperator,
	OperationBurn:                TrustOperator,
	OperationRatePublish:         TrustOperator,
	OperationConventionPromote:   TrustOperator,
	OperationConventionSupersede: TrustOperator,
	OperationMatch:               TrustOperator,
	OperationConsume:             TrustOperator,

	// Assign sub-op axis: operator sub-ops require the operator key; worker sub-ops
	// (claim/complete) require fleet-member (allowlisted) standing. This mirrors
	// operatorAssignOps in pkg/nostr/verify.go and is what tagToTrustOp routes the
	// seven assign tags through (docs/design/relay-transport.md §2.4a D3).
	OperationAssignPost:         TrustOperator,
	OperationAssignAccept:       TrustOperator,
	OperationAssignReject:       TrustOperator,
	OperationAssignExpire:       TrustOperator,
	OperationAssignAuctionClose: TrustOperator,
	OperationAssignClaim:        TrustAllowlisted,
	OperationAssignComplete:     TrustAllowlisted,
}

// defaultSettlePhaseLevels is the compiled-in default for settle phases.
// Buyer-side phases are fleet-member operations; put-accept/reject/deliver are
// operator-authored settlement events.
var defaultSettlePhaseLevels = map[SettlePhase]TrustLevel{
	SettlePhaseBuyerAccept: TrustAllowlisted,
	SettlePhaseBuyerReject: TrustAllowlisted,
	SettlePhaseComplete:    TrustAllowlisted,
	SettlePhaseDispute:     TrustAllowlisted,
	SettlePhasePutAccept:   TrustOperator,
	SettlePhasePutReject:   TrustOperator,
	SettlePhaseDeliver:     TrustOperator,
	SettlePhasePreview:     TrustOperator,
	// Buyer-authored fleet-member phases. Previously omitted → dispatch gate
	// silently dropped them (dontguess-471).
	SettlePhasePreviewRequest:      TrustAllowlisted,
	SettlePhaseSmallContentDispute: TrustAllowlisted,
}

// TrustLevels configures the minimum trust level required for each exchange
// operation. Stored in the exchange config JSON as trust_levels.
//
// Keys are operation names (e.g. "put", "buy", "match") or "settle:<phase>"
// for settle phases (e.g. "settle:put-accept", "settle:buyer-reject").
// Values are level names: "anonymous", "allowlisted", "operator".
//
// Only overridden keys need to be present — missing keys use compiled defaults.
type TrustLevels map[string]string

// levelNames maps level name strings to TrustLevel values.
var levelNames = map[string]TrustLevel{
	"anonymous":   TrustAnonymous,
	"allowlisted": TrustAllowlisted,
	"operator":    TrustOperator,
}

// Membership reports whether a sender's hex pubkey is an admitted fleet member.
//
// *identity.Allowlist satisfies this (its Allowed method) for the nostr fleet-npub
// path. KeySet is the mutable, transport-agnostic implementation used by the
// campfire-backed serve path, where keys are ed25519 and do not parse as
// secp256k1 x-only npubs.
type Membership interface {
	Allowed(hexKey string) bool
}

// KeySet is a mutable, concurrency-safe set of admitted fleet-member hex
// pubkeys. It is the Membership used on the current campfire transport
// (operator + campfire members) and supports runtime de-allowlisting (Remove),
// which the serve membership-refresh loop drives when a member leaves the
// campfire.
//
// The mutex is required, not optional: Allowed runs from the engine's poll-loop
// dispatch goroutine (and the operator-socket handler goroutine), while
// Add/Remove run from the serve membership-refresh goroutine. Without the lock
// those concurrent map reads/writes are a data race (verified under -race).
type KeySet struct {
	mu      sync.RWMutex
	members map[string]struct{}
}

// NewKeySet builds a KeySet from the given hex keys. Empty/whitespace entries
// are ignored. Comparison is case-insensitive (keys are lowercased).
func NewKeySet(keys ...string) *KeySet {
	ks := &KeySet{members: make(map[string]struct{}, len(keys))}
	for _, k := range keys {
		ks.Add(k)
	}
	return ks
}

// Add admits a hex key to the set. Safe for concurrent use.
func (k *KeySet) Add(hexKey string) {
	hexKey = strings.ToLower(strings.TrimSpace(hexKey))
	if hexKey == "" {
		return
	}
	k.mu.Lock()
	k.members[hexKey] = struct{}{}
	k.mu.Unlock()
}

// Remove revokes a hex key from the set (runtime de-allowlisting).
// Safe for concurrent use.
func (k *KeySet) Remove(hexKey string) {
	hexKey = strings.ToLower(strings.TrimSpace(hexKey))
	if hexKey == "" {
		return
	}
	k.mu.Lock()
	delete(k.members, hexKey)
	k.mu.Unlock()
}

// ReplaceAll atomically sets the admitted members to EXACTLY keys, discarding the
// prior membership. This is the fleet-roster fold's replace semantics (design
// §2/P5): an operator-signed kind-30078 roster event is AUTHORITATIVE FULL
// membership (a parameterized-replaceable event, latest-wins), so folding a fresher
// roster REPLACES the KeySet rather than unioning into it — an operator's removal
// of a key takes effect because the new roster simply omits it. Empty/whitespace
// entries are ignored; keys are lowercased (case-insensitive membership). The swap
// is a single locked assignment, so a concurrent Allowed sees either the whole old
// set or the whole new set, never a partially-applied roster. It never touches the
// operator key — operator authority comes from TrustChecker.operatorKey, not
// membership, so a roster can never lock the operator out of its own exchange.
func (k *KeySet) ReplaceAll(keys ...string) {
	next := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		key = strings.ToLower(strings.TrimSpace(key))
		if key == "" {
			continue
		}
		next[key] = struct{}{}
	}
	k.mu.Lock()
	k.members = next
	k.mu.Unlock()
}

// Allowed reports whether the hex key is admitted. Safe for concurrent use.
func (k *KeySet) Allowed(hexKey string) bool {
	hexKey = strings.ToLower(strings.TrimSpace(hexKey))
	k.mu.RLock()
	_, ok := k.members[hexKey]
	k.mu.RUnlock()
	return ok
}

// Len returns the number of admitted keys. Safe for concurrent use.
func (k *KeySet) Len() int {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return len(k.members)
}

// Keys returns a snapshot of the admitted (lowercased) hex keys. The serve
// membership-refresh loop uses this to diff the current allowlist against the
// live campfire membership and revoke (Remove) keys that have departed. Safe
// for concurrent use — the returned slice is a copy.
func (k *KeySet) Keys() []string {
	k.mu.RLock()
	defer k.mu.RUnlock()
	out := make([]string, 0, len(k.members))
	for key := range k.members {
		out = append(out, key)
	}
	return out
}

// ErrInsufficientTrust is returned when a sender's trust level does not meet the
// minimum required for the requested operation.
var ErrInsufficientTrust = errors.New("exchange: insufficient trust level for operation")

// ErrLowReputation is returned when a sender's behavioral reputation is below the
// configured floor for a reputation-gated (sell-side) operation.
var ErrLowReputation = errors.New("exchange: seller reputation below floor for operation")

// TrustChecker validates that a sender is authorized for an exchange operation,
// combining NIP-42 allowlist membership, operator write authority, and a
// behavioral reputation floor on sell-side operations.
type TrustChecker struct {
	operatorKey  string
	members      Membership
	opLevels     map[Operation]TrustLevel
	settleLevels map[SettlePhase]TrustLevel

	// reputation, when non-nil, gates sell-side operations (put): a sender whose
	// score is below minReputation is rejected with ErrLowReputation. nil disables
	// reputation gating entirely.
	reputation    func(key string) int
	minReputation int

	// pendingOverrides holds config overrides staged by WithTrustLevelOverrides
	// until NewTrustChecker validates and applies them. Cleared after construction.
	pendingOverrides TrustLevels
}

// TrustCheckerOption configures a TrustChecker.
type TrustCheckerOption func(*TrustChecker)

// WithTrustLevelOverrides applies operation→level overrides from exchange config.
// Returns an error via the checker constructor if any level name is unknown.
func WithTrustLevelOverrides(overrides TrustLevels) TrustCheckerOption {
	return func(c *TrustChecker) { c.pendingOverrides = overrides }
}

// WithReputationFloor wires a behavioral reputation source and a minimum score.
// Sellers whose reputation is below min are rejected for sell-side operations
// (put). Gating is enabled only when source is non-nil: a nil source disables the
// reputation gate entirely (Check skips it), regardless of min. Pass a real source
// and a floor to enable it. The operator is never subject to the floor.
func WithReputationFloor(source func(key string) int, min int) TrustCheckerOption {
	return func(c *TrustChecker) {
		c.reputation = source
		c.minReputation = min
	}
}

// applyPending validates and applies staged config overrides during construction.
func (c *TrustChecker) applyPending() error {
	if c.pendingOverrides == nil {
		return nil
	}
	for key, levelName := range c.pendingOverrides {
		level, ok := levelNames[levelName]
		if !ok {
			return fmt.Errorf("exchange: unknown trust level %q for key %q", levelName, key)
		}
		if strings.HasPrefix(key, "settle:") {
			c.settleLevels[SettlePhase(strings.TrimPrefix(key, "settle:"))] = level
		} else {
			c.opLevels[Operation(key)] = level
		}
	}
	return nil
}

// reputationGatedOps are the operations subject to the reputation floor.
// Selling (put) is the sole sell-side entry point; buying and settling are not
// reputation-gated (a buyer's reputation should not block their purchase).
var reputationGatedOps = map[Operation]struct{}{
	OperationPut: {},
}

// NewTrustChecker creates a TrustChecker. operatorKey is the operator's pubkey
// (hex or npub form, matched exactly); it always resolves to TrustOperator.
// members is the fleet allowlist (may be nil, e.g. individual tier with no team
// relay — then only the operator is above anonymous). Options apply config
// overrides and the reputation floor.
func NewTrustChecker(operatorKey string, members Membership, opts ...TrustCheckerOption) (*TrustChecker, error) {
	c := &TrustChecker{
		operatorKey:  operatorKey,
		members:      members,
		opLevels:     make(map[Operation]TrustLevel, len(defaultOperationLevels)),
		settleLevels: make(map[SettlePhase]TrustLevel, len(defaultSettlePhaseLevels)),
	}
	for k, v := range defaultOperationLevels {
		c.opLevels[k] = v
	}
	for k, v := range defaultSettlePhaseLevels {
		c.settleLevels[k] = v
	}
	for _, opt := range opts {
		opt(c)
	}
	if err := c.applyPending(); err != nil {
		return nil, err
	}
	c.pendingOverrides = nil
	return c, nil
}

// SetReputationFloor wires (or replaces) the behavioral reputation source and
// floor after construction. This exists because the reputation source (the
// engine State) is created inside NewEngine, after the TrustChecker it receives.
//
// Not safe for concurrent use with Check: call it once, before the engine's poll
// loop starts. A nil source disables reputation gating.
func (c *TrustChecker) SetReputationFloor(source func(key string) int, min int) {
	c.reputation = source
	c.minReputation = min
}

// RemoveMember revokes a key from the live fleet allowlist when the underlying
// Membership is a mutable *KeySet (runtime de-allowlisting, dontguess-d53 Seam
// C). It is a no-op for an immutable Membership (e.g. *identity.Allowlist) and
// never affects the operator key — operator authority comes from operatorKey,
// not membership. Safe for concurrent use (KeySet.Remove is locked).
func (c *TrustChecker) RemoveMember(key string) {
	if ks, ok := c.members.(*KeySet); ok {
		ks.Remove(key)
	}
}

// Level returns the trust tier for a sender key: operator if it is the operator
// key, allowlisted if it is on the allowlist, anonymous otherwise.
func (c *TrustChecker) Level(key string) TrustLevel {
	if c.operatorKey != "" && key == c.operatorKey {
		return TrustOperator
	}
	if c.members != nil && c.members.Allowed(key) {
		return TrustAllowlisted
	}
	return TrustAnonymous
}

// Check returns nil if the sender is authorized for the operation, or a non-nil
// error (ErrInsufficientTrust or ErrLowReputation, wrapping details) if not.
func (c *TrustChecker) Check(senderKey string, op Operation, phase SettlePhase) error {
	required, err := c.RequiredLevel(op, phase)
	if err != nil {
		return err
	}

	actual := c.Level(senderKey)
	if actual < required {
		return fmt.Errorf("%w: operation=%q phase=%q requires %s, sender %q has %s",
			ErrInsufficientTrust, op, phase, required, shortSenderKey(senderKey), actual)
	}

	// Reputation floor on sell-side operations. Applied only when a reputation
	// source is configured. The operator is never blocked by the floor — the
	// operator's own settlement/mint events are trust-anchored, not reputation-scored.
	if c.reputation != nil && actual != TrustOperator {
		if _, gated := reputationGatedOps[op]; gated {
			if score := c.reputation(senderKey); score < c.minReputation {
				return fmt.Errorf("%w: operation=%q sender %q has reputation %d, floor is %d",
					ErrLowReputation, op, shortSenderKey(senderKey), score, c.minReputation)
			}
		}
	}
	return nil
}

// RequiredLevel returns the minimum trust level required for an operation (and
// settle phase). Returns an error if the operation or phase is unknown.
func (c *TrustChecker) RequiredLevel(op Operation, phase SettlePhase) (TrustLevel, error) {
	if op == OperationSettle {
		if phase == "" {
			return 0, fmt.Errorf("exchange: settle operation requires a non-empty phase")
		}
		level, ok := c.settleLevels[phase]
		if !ok {
			return 0, fmt.Errorf("exchange: unknown settle phase %q", phase)
		}
		return level, nil
	}

	level, ok := c.opLevels[op]
	if !ok {
		return 0, fmt.Errorf("exchange: unknown operation %q", op)
	}
	return level, nil
}

// RequiredLevel is a package-level convenience that uses the compiled defaults.
// Prefer the method on TrustChecker for production use.
func RequiredLevel(op Operation, phase SettlePhase) (TrustLevel, error) {
	if op == OperationSettle {
		if phase == "" {
			return 0, fmt.Errorf("exchange: settle operation requires a non-empty phase")
		}
		level, ok := defaultSettlePhaseLevels[phase]
		if !ok {
			return 0, fmt.Errorf("exchange: unknown settle phase %q", phase)
		}
		return level, nil
	}

	level, ok := defaultOperationLevels[op]
	if !ok {
		return 0, fmt.Errorf("exchange: unknown operation %q", op)
	}
	return level, nil
}

// shortSenderKey truncates a key for log/error readability without leaking the
// full key material into every rejection message.
func shortSenderKey(key string) string {
	if len(key) <= 12 {
		return key
	}
	return key[:12] + "…"
}
