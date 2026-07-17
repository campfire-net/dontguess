package exchange

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/3dl-dev/dontguess/pkg/identity"
	"github.com/3dl-dev/dontguess/pkg/matching"
	"github.com/3dl-dev/dontguess/pkg/scrip"
	dgstore "github.com/3dl-dev/dontguess/pkg/store"
)

// MatchingFeeRate is the fraction of the sale price charged as a matching fee.
// The fee is burned (deflationary). 10% = 1/10.
const MatchingFeeRate = 10

// ResidualRate is the fraction of the sale price paid as residual to the
// original seller. 10% = 1/10.
const ResidualRate = 10

// HotCompressionBountyPct is the percentage of token_cost paid as bounty for
// hot compression (immediately after put). 50% = 1/2.
const HotCompressionBountyPct = 50

// WarmCompressionBountyPct is the percentage of token_cost paid as bounty for
// warm compression (buyer-initiated, content in cache). 30% = 3/10.
const WarmCompressionBountyPct = 30

// ColdCompressionBountyPct is the percentage of token_cost paid as bounty for
// cold compression (demand-driven stock maintenance, medium loop). 20% = 1/5.
// Lower than warm (30%) because there is no urgency — the entry is aging
// inventory that the exchange wants compressed proactively.
const ColdCompressionBountyPct = 20

// ReservationExpiryDuration is the time window during which a buyer-accept
// reservation is valid. After expiry, the scrip hold is released.
const ReservationExpiryDuration = 5 * time.Minute

// BrokeredMatchDefaultReward is the default scrip reward for a brokered-match
// assign when BrokeredMatchReward is not configured on EngineOptions.
// 100 scrip is a nominal value; operators should calibrate based on inventory size
// and expected worker demand.
const BrokeredMatchDefaultReward = 100

// PredictionAssignTTL is the TTL for prediction-derived standing assigns.
// After this window the assign is stale and workers may not claim it.
// Default 2 hours per design §7.
const PredictionAssignTTL = 2 * time.Hour

// MaxPredictionFanout is the maximum number of standing brokered-match assigns
// the engine will pre-stage per predicted entry. Prevents assignByID blowup
// from high-frequency settle events (A9 mitigation, design §7).
const MaxPredictionFanout = 3

// buyerSessionWindow is the lookback window for co-occurrence pairing.
// Entries settled by the same buyer within this window are considered co-occurring.
const buyerSessionWindow = time.Hour

// Layer0MinPreviews is the minimum number of previews before conversion-rate
// exclusion kicks in. Below this, entries have insufficient data for exclusion.
const Layer0MinPreviews = 10

// Layer0MaxConversionRate is the conversion rate below which entries are excluded
// from match results. 5% means fewer than 1 in 20 previewers chose to buy.
const Layer0MaxConversionRate = 0.05

// EngineOptions configures an exchange engine.
type EngineOptions struct {
	// CampfireID is the exchange campfire's public key hex.
	CampfireID string
	// OperatorPublicKey is the hex-encoded Ed25519 public key of the exchange
	// operator. It populates State.OperatorKey on startup so operator-emitted
	// records (match/put-accept/settle/…) are attributed to it. The engine has
	// no signing client of its own — egress is a direct append into LocalStore.
	OperatorPublicKey string

	// OperatorSigner is the operator's secp256k1 identity (private, does ECDH).
	// It is threaded into State.operatorSigner so applyPut can NIP-44-unwrap a
	// v2 put's wrapped_cek_operator and decrypt-then-gate on team tier
	// (docs/design/content-confidentiality-envelope-541.md §3.1(2)/§6,
	// dontguess-4bed). REQUIRED alongside ScripStore to arm the §6 fail-closed
	// ciphertext-only enforcement — production serve wires it from the relay
	// operator identity in the SAME relays-attached branch that sets ScripStore.
	// Must be a TRUE nil interface (not a typed-nil) on the individual tier, so
	// callers pass an untyped nil when no operator identity is loaded (mirrors
	// the TrustChecker typed-nil precedent). nil ⇒ the legacy plaintext path
	// stays legal and byte-for-byte unchanged.
	OperatorSigner identity.Signer
	// LocalStore is the campfire-free append-only event log (pkg/store) that is
	// the engine's SOLE ingest and egress path.
	//
	// INGEST: replayAll and the poll loop (run) read exclusively from
	// LocalStore.Replay()/pollLocalStore. EGRESS: sendOperatorMessage appends
	// operator-emitted records (match/put-accept/settle/…) directly into
	// LocalStore. A relay (nostr) transport, when configured, tails LocalStore
	// for outbound publish and writes inbound relay events back into it — both
	// legs are off the buy/match hot path (docs/design/relay-transport.md §2.4).
	//
	// Single-writer only (per pkg/store's package doc): out-of-order / orphan-
	// antecedent handling is opt-in via SequencedIngest, needed once multi-relay
	// nostr ingest can deliver events out of order.
	LocalStore *dgstore.Store
	// SequencedIngest routes the startup replay fold (replayAllLocal) through
	// the operator-side Sequencer (dontguess-50d, M2) before folding, instead
	// of trusting LocalStore append order as fold order.
	//
	// It exists because multi-relay nostr ingest gives NO total order and NO
	// causal delivery: an e-tagged event can be persisted out of causal order,
	// so append order is no longer fold order. With this set, replayAllLocal
	// re-derives the canonical, deterministic fold order from the events'
	// antecedent DAG (Sequencer.SequenceForFold) and FAILS LOUD if the causal
	// closure is broken (a pruned/unrecoverable antecedent) rather than folding
	// a silently-truncated chain (dontguess-553 lesson).
	//
	// Default false preserves the M1 single-writer append-order path exactly.
	// When true and LocalStore is unset it is a no-op (only the LocalStore
	// replay path is sequenced). MaxOrphans bounds the pending-antecedent
	// buffer; zero uses DefaultMaxOrphans.
	SequencedIngest bool
	// MaxOrphans bounds the Sequencer's pending-antecedent buffer when
	// SequencedIngest is set. Zero uses DefaultMaxOrphans (~1000).
	MaxOrphans int
	// PollInterval controls how often the engine polls for new messages.
	// Defaults to 500ms.
	PollInterval time.Duration
	// Logger receives diagnostic log lines. If nil, logs are suppressed.
	Logger func(format string, args ...any)
	// Embedder overrides the matching engine's embedding strategy.
	// If nil, TF-IDF is used. Set to a matching.NativeEmbedder (pure-Go
	// all-MiniLM-L6-v2) for 384-dim semantic matching.
	Embedder matching.Embedder
	// MatchIndex is the semantic matching index used to rank buy results.
	// If nil, the engine creates an index using Embedder on startup.
	MatchIndex *matching.Index
	// ScripStore is the scrip spending store used for pre-decrement / adjust / refund
	// on buy / settle / dispute operations. If nil, scrip checks are skipped (useful
	// for tests that do not exercise the scrip flow).
	ScripStore scrip.SpendingStore
	// TrustChecker gates sender authority (NIP-42 allowlist + operator write
	// authority + reputation floor) before processing operations. If nil, trust
	// checks are skipped (useful for tests that don't exercise trust gating).
	TrustChecker *TrustChecker
	// ReadSkipSync is retained for API compatibility with test literals that
	// still set it; the sole ingest path is now LocalStore, which needs no
	// transport sync, so this flag no longer gates any read.
	ReadSkipSync bool

	// DensityMarkupFactor is the per-token price premium applied to compressed
	// derivative entries. A value of 1.2 means the per-token price is 20% higher
	// than the raw base, compensating for higher information density. The total
	// price is still lower than raw because fewer tokens are delivered.
	// If zero or negative, defaults to 1.2.
	DensityMarkupFactor float64

	// MaxTokenCost is the ceiling applied to seller-supplied token_cost in the
	// buy-miss auto-accept path. Prevents a malicious seller from inflating the
	// scrip payout by submitting an artificially large token_cost. Defaults to
	// 1_000_000 if zero.
	MaxTokenCost int64

	// BrokeredMatchMode enables the brokered matching path. When true,
	// handleBuy posts an exchange:assign with task_type="brokered-match"
	// instead of running inline semantic matching. Workers claim the assign,
	// search inventory, and deliver ranked results. The operator then accepts
	// the result and emits exchange:match to the buyer. Inline matching is
	// the default (BrokeredMatchMode=false) and always coexists: operators
	// may toggle this flag to switch routing without affecting either path's
	// state machine.
	BrokeredMatchMode bool

	// BrokeredMatchReward is the scrip reward posted on brokered-match assigns.
	// If zero, defaults to BrokeredMatchDefaultReward.
	BrokeredMatchReward int64

	// FederationGuardEnabled activates the dual trust guard for federation nodes in
	// handleBuy. When true, new nodes (below NewNodeTrustThreshold or
	// NewNodeTransactionThreshold) are blocked from brokered routing until they
	// graduate. REQUIRED in multi-operator/federation deployments. Defaults false
	// for single-operator deployments. When BrokeredMatchMode=true, a startup
	// warning is emitted if this is false.
	FederationGuardEnabled bool

	// MinBuyBalance is the anonymous-buy demand-signal bound (design §8-D1,
	// dontguess-3879). OperationBuy stays TrustAnonymous (ratified): a buy folds
	// into the matching / demand / pricing pipeline BEFORE settlement, and the
	// scrip hold is only taken later at buyer-accept. So the ScripStore bounds
	// the MONEY but not the SIGNAL — a zero-scrip Sybil could send buys that
	// surface (rank) an entry and, once driven through settle, move its
	// demand-count / price for free. When MinBuyBalance > 0, handleBuy requires
	// the buyer to hold at least this many scrip BEFORE the buy is allowed to
	// contribute; an underfunded buy is dropped (loudly counted as
	// DroppedUnderfundedBuy) before any match/demand fold.
	//
	// Enforced ONLY when a ScripStore is configured (team/federated tier). On the
	// individual tier (ScripStore == nil) it is a no-op — content moves free and
	// there are no scrip balances to check, so behavior is byte-for-byte
	// unchanged. Zero (the default) disables the bound.
	MinBuyBalance int64

	// OnLocalAppend, when non-nil, is invoked exactly once after each SUCCESSFUL
	// operator-record append into LocalStore (appendLocalRecord — the single
	// operator egress point), AFTER localMu is released. It is the post-emit hook
	// that lets the relay Outbox publish an operator match the INSTANT it is folded
	// instead of waiting up to a full outbox tick (design §3.8, H1). serve wires it
	// to fan out to every attached relay leg's Outbox.Notify().
	//
	// It fires ONLY on success (never on an Append error) and runs outside localMu
	// so the fold-serializing mutex is never held across caller code; the wired
	// Outbox.Notify is itself non-blocking. Nil (the default — the individual tier
	// has no outbox) is a strict no-op: behavior is byte-for-byte unchanged.
	OnLocalAppend func()

	// AutoDeliverOnBuyerAccept makes the operator auto-emit the settle(deliver)
	// content message on a FRESH-hold-success buyer-accept (dontguess-55c GAP 2,
	// docs/design/settle-wire-id-reconciliation-55c.md). It exists because a
	// team-tier relay buyer CANNOT emit the operator-gated deliver themselves
	// (applySettleDeliver is operator-only) and there is no manual operator in the
	// loop — so without this, a funded buyer-accept holds scrip but content never
	// moves. When set, handleSettleBuyerAcceptScrip calls emitDeliverContent
	// DIRECTLY (send-then-Apply, mirroring emitConsumeSignal) on the one path where
	// decAndSaveHold just succeeded — which fires EXACTLY ONCE per match because the
	// IsMatchSettled guard and the restoreExistingHold idempotency short-circuit
	// have already returned on every re-send.
	//
	// Default FALSE, and deliberately NOT gated on ScripStore: serve sets it true
	// only on the team tier (scripStore != nil). The frozen scrip suite drives
	// deliver via a MANUAL operator trigger and asserts exactly one deliver, so it
	// leaves this false — auto-deliver never fires and its single manual deliver
	// stays the sole deliver. On the individual tier (ScripStore == nil)
	// handleSettleBuyerAcceptScrip never runs, so this is doubly inert there.
	AutoDeliverOnBuyerAccept bool

	// OnStarted, when non-nil, is invoked exactly once by Start AFTER the startup
	// replay, dispatch-cursor seed, and dispatchPendingOrders have all completed —
	// i.e. the instant before Start enters its steady-state poll loop. It is the
	// startup-complete readiness signal, symmetric with OnLocalAppend's post-emit
	// hook: an embedder or test that must not interact with the engine until its
	// full log is folded and its cursors are seeded can block on it instead of
	// racing a wall-clock sleep against Start's background goroutine. Firing it
	// only after dispatchPendingOrders guarantees no post-barrier caller can race
	// the startup fold's cursor writes (dontguess-fe9f). Nil (the default) is a
	// strict no-op — behavior is byte-for-byte unchanged.
	OnStarted func()
}

func (o *EngineOptions) pollInterval() time.Duration {
	if o.PollInterval > 0 {
		return o.PollInterval
	}
	return 500 * time.Millisecond
}

// densityMarkupFactor returns the configured density markup factor for
// compressed derivatives, defaulting to 1.2 if unset.
func (o *EngineOptions) densityMarkupFactor() float64 {
	if o.DensityMarkupFactor > 0 {
		return o.DensityMarkupFactor
	}
	return 1.2
}

func (o *EngineOptions) maxTokenCost() int64 {
	if o.MaxTokenCost > 0 {
		return o.MaxTokenCost
	}
	return 1_000_000
}

func (o *EngineOptions) brokeredMatchReward() int64 {
	if o.BrokeredMatchReward > 0 {
		return o.BrokeredMatchReward
	}
	return BrokeredMatchDefaultReward
}

func (o *EngineOptions) log(format string, args ...any) {
	if o.Logger != nil {
		o.Logger(format, args...)
	}
}

// shortKey returns the first 8 characters of key for use in log messages.
// It never panics on short strings — keys shorter than 8 characters are
// returned as-is. This prevents the [:8] panic on malformed or truncated keys.
func shortKey(key string) string {
	if len(key) <= 8 {
		return key
	}
	return key[:8]
}

// Engine subscribes to the exchange campfire, processes convention messages,
// and emits response messages (match, settle) back to the campfire.
//
// The engine maintains an in-memory State materialized from the message log.
// On startup it replays the full log (Start). It then polls for new messages
// and applies them incrementally.
//
// Semantic matching is performed by a matching.Index, which is rebuilt from
// inventory on startup and updated incrementally as entries are added or removed.
type Engine struct {
	opts       EngineOptions
	state      *State
	matchIndex *matching.Index
	// opMu serializes state-mutating operations across concurrent goroutines:
	// RunAutoAccept (auto-accept ticker goroutine) and AutoAcceptPut/RejectPut
	// (operator socket handler goroutine). Lock ordering: acquire opMu FIRST,
	// then any State-internal locks (acquired via the existing State helpers).
	opMu sync.Mutex
	// ctx is the shutdown context passed to Start. Stored atomically so that
	// handler goroutines can read it without a data race against the Start write.
	// Handlers use this so that in-flight scrip operations are cancelled on
	// graceful shutdown instead of using context.Background() which ignores the
	// shutdown signal.
	ctx atomic.Value // stores context.Context
	// marshalFunc overrides json.Marshal for tests that need to inject marshal failures.
	// Nil means use the standard json.Marshal.
	marshalFunc func(v any) ([]byte, error)
	// resvMu guards both matchToReservation and buyerRecentEntries. In local-relay
	// mode dispatch runs on TWO goroutines concurrently: the poll loop
	// (pollLocalStore → foldAndDispatchLocalSnapshot → dispatch) and the
	// operator/auto-accept path (rebuildAndDispatchGapLocal → dispatch, driven by
	// AutoAcceptPut/RejectPut). localMu only makes the cursor claims atomic — the
	// dispatch HANDLER bodies (handleSettleBuyerAcceptScrip / handleSettleComplete
	// → recordBuyerSettlement / performScripSettlement) run outside localMu and
	// mutate these two maps concurrently. Without resvMu those are unsynchronized
	// map writes (data race, verified under -race). Lock ordering: resvMu is a
	// leaf-ish lock — recordBuyerSettlement holds it across State calls (State has
	// its own lock and never calls back into these maps), and no path takes
	// State.mu then resvMu, so there is no lock-ordering hazard (dontguess-471).
	resvMu sync.Mutex
	// matchToReservation maps a match message ID to the scrip reservation ID created
	// at buyer-accept time. The settle(complete) handler uses this to locate the
	// reservation without trusting buyer-supplied payload data. Guarded by resvMu.
	matchToReservation map[string]string

	// buyerRecentEntries tracks the last few entries settled per buyer for
	// co-occurrence recording. Key: buyerKey -> list of (entryID, time) pairs.
	// Not persisted — rebuilt from settle events observed since engine start.
	// Guarded by resvMu (concurrent poll-loop + operator dispatch, dontguess-471).
	buyerRecentEntries map[string][]buyerSessionEntry

	// localMu guards localMsgByID, localSeen, and localDispatched. These are
	// mutated by three code paths that legitimately run concurrently on one
	// Engine: replayAllLocal (startup, via Start), pollLocalStore (the run()
	// poll-loop goroutine), and rebuildAndDispatchGapLocal (the auto-accept /
	// operator-socket goroutines, via AutoAcceptPut/RejectPut). localMu makes
	// the cursor claims atomic so each appended record is folded and dispatched
	// exactly once no matter how those paths interleave; the actual state.Apply
	// / dispatch work runs outside the lock (State has its own lock, and
	// dispatch handlers re-enter localMu via appendLocalRecord).
	localMu sync.Mutex
	// localMsgByID indexes every message the engine has ingested from
	// LocalStore (dontguess-275), by ID. LocalStore (pkg/store) has no
	// campfire Get/GetMessage to fall back on, so fetchMessage and
	// sendLocalOperatorMessage use this index instead. Populated by
	// replayAllLocal (startup) and pollLocalStore (ongoing); only relevant
	// when EngineOptions.LocalStore is configured. Guarded by localMu.
	localMsgByID map[string]*Message
	// localSeen is the FOLD cursor: the count of LocalStore records already
	// folded into State (via a full state.Replay in replayAllLocal /
	// rebuildAndDispatchGapLocal, or an incremental state.Apply in
	// pollLocalStore). LocalStore is single-writer, strictly append-order
	// (pkg/store package doc), so a length-based cursor is sufficient to find
	// "new since last fold" without a sequencer or orphan buffer (that is M2,
	// dontguess-50d). Only relevant when EngineOptions.LocalStore is configured.
	// Guarded by localMu.
	localSeen int
	// localDispatched is the DISPATCH cursor: the count of LocalStore records the
	// absolute fold/dispatch path has swept for dispatch. A record in [0:localDispatched)
	// has EITHER been dispatched (handleBuy/handlePut/participant settle) OR
	// deliberately SKIPPED as operator-self-applied (Sender == OperatorKey, applied
	// to State by its own emitter and never dispatched — dontguess-e2a3,
	// dispatchLocalGap). It is a SWEPT cursor, not a "count actually handed to
	// dispatch()"; the operator-record skip is derived from the log, not tracked in
	// a set. It is deliberately SEPARATE from localSeen (the fold cursor): a full
	// state rebuild folds records into State, so [localDispatched:localSeen] can be
	// a gap of folded-but-not-yet-swept records. Conflating the two (advancing the
	// dispatch cursor during a rebuild that did not itself dispatch) is exactly the
	// defect that silently dropped buy orders — they landed in State.ActiveOrders,
	// were counted as "seen", and were never matched (dontguess-b84). The invariant:
	// every member record folded into State is dispatched exactly once, no record is
	// dispatched twice, and operator-self records are never dispatched. Guarded by
	// localMu; always localDispatched <= localSeen. Only relevant when
	// EngineOptions.LocalStore is configured.
	localDispatched int

	// emitClockMu guards lastEmitNanos, the monotonic wall-clock source for
	// locally-emitted (sendLocalOperatorMessage) operator messages. See
	// nextMonotonicTimestamp (engine_settle.go) and docs/design/
	// relay-transport.md §E MUST-ENFORCE(1): emitted-event timestamps must be
	// non-decreasing so (Timestamp,ID) batch order reproduces true emission
	// order on a Seq-less DR rebuild.
	emitClockMu   sync.Mutex
	lastEmitNanos int64

	// degradation counts trust-gate rejections in the dispatch fold (§2.4a D4 +
	// §3). Always non-nil (initialized in NewEngine); never touched when
	// EngineOptions.TrustChecker is nil, since the trust gate itself is
	// skipped in that mode.
	degradation *DegradationMetrics
}

// buyerSessionEntry records a single entry settled by a buyer, used for
// co-occurrence pairing within a session window.
type buyerSessionEntry struct {
	entryID   string
	settledAt time.Time
}

// DegradationMetrics holds the dispatch trust-gate rejection counters
// (docs/design/relay-transport.md §2.4a D4 + §3 "provenance_rejected"). Every
// trust-denial reason is a DISTINCT, separately-alarmed counter — mirroring
// pkg/relay's IntakeMetrics/WatchdogMetrics pattern (relay/metrics.go):
// not-allowlisted, not-operator, and below-reputation are different failure
// classes with different triage paths and MUST NOT be collapsed into one
// bucket. No trust-gate rejection may `return nil` from dispatch silently
// (LOCKED-5) — every increment here is paired with a loud log line at the
// call site (dispatch, engine_core.go).
//
// All counters are atomic.Int64 so the poll-loop goroutine and any concurrent
// operator-socket handler goroutine can both dispatch through the trust gate
// without a lock.
type DegradationMetrics struct {
	// TrustDenialNotAllowlisted counts dispatch trust-gate rejections where the
	// sender lacks TrustAllowlisted standing for an allowlisted-tier operation
	// (e.g. a non-fleet-member put or assign-claim).
	TrustDenialNotAllowlisted atomic.Int64
	// TrustDenialNotOperator counts rejections where the sender is not the
	// operator key for an operator-only operation (e.g. a forged match,
	// settle(put-accept), or assign-post).
	TrustDenialNotOperator atomic.Int64
	// TrustDenialLowReputation counts rejections from TrustChecker's seller
	// reputation floor (ErrLowReputation) — a seller who has burned trust is
	// blocked from further sell-side operations independent of allowlist
	// membership.
	TrustDenialLowReputation atomic.Int64
	// TrustDenialOther counts trust-gate rejections that don't fit the above
	// buckets (e.g. RequiredLevel returning an unknown-op/unknown-phase error).
	// Present so a future new rejection class is still counted, never dropped
	// into a silent nil-return while its bucket is added.
	TrustDenialOther atomic.Int64

	// DroppedUnlisted and DroppedLowReputation count SEAM-A promotion-gate
	// rejections (dontguess-d53, autoAcceptPutLocked): a put that the poll-loop
	// fold already staged into pendingPuts (state_put.go applyPut runs with ZERO
	// trust filter) is blocked at auto-accept promotion — the real choke, since
	// the dispatch trust gate never runs on the promotion path. These are DISTINCT
	// from the dispatch-gate TrustDenial* counters above: those bucket messages
	// rejected at dispatch (handlePut); these bucket puts rejected at promotion
	// into matchable inventory. DroppedUnlisted = seller not on the fleet
	// allowlist; DroppedLowReputation = allowlisted seller below the reputation
	// floor. Every increment is paired with a LOUD alarm + a put-reject (never a
	// silent nil-drop; LOCKED-5).
	DroppedUnlisted      atomic.Int64
	DroppedLowReputation atomic.Int64

	// DroppedDedupPoison counts SEAM-A trust-gate rejects (dontguess-327) that
	// purged a zero-trust-registered content hash out of contentHashIndex. applyPut
	// registers EVERY put's content hash during the fold with no trust filter; a
	// non-allowlisted / below-floor sender's rejected put would otherwise leave that
	// hash squatting the index, permanently blocking a later ALLOWLISTED seller's
	// byte-identical put (the exchange's designed high-reuse happy path) via a silent
	// bare return in applyPut. Purging the hash on the trust reject closes that
	// griefing lever; this counter makes the purge — previously an invisible
	// squat-and-block — observable. Incremented once per SEAM-A trust reject,
	// alongside DroppedUnlisted / DroppedLowReputation.
	DroppedDedupPoison atomic.Int64

	// DroppedUnderfundedBuy counts anonymous buys dropped by the demand-signal
	// bound (design §8-D1, dontguess-3879): a buyer holding less than
	// EngineOptions.MinBuyBalance scrip had its buy dropped in handleBuy BEFORE
	// the buy could contribute to matching / demand / pricing folding. Only
	// enforced on the team/federated tier (ScripStore configured, MinBuyBalance
	// > 0). Every increment is paired with a LOUD alarm (never a silent drop).
	DroppedUnderfundedBuy atomic.Int64

	// FoldDenialNotOperator counts STATE-fold rejections where an operator-only
	// settlement fold guard (applySettlePutAccept / applySettlePutReject /
	// applySettleDeliver in state_settle.go) dropped a message whose Sender is
	// not the operator key. Distinct from TrustDenialNotOperator, which is the
	// dispatch trust gate: the trust gate runs on the ENGINE side (matching,
	// operator responses), while these guards run inside State.Apply and are the
	// last line of defense against a forged operator-authored settlement mutating
	// state directly. A forged put-accept that reached the fold without being
	// counted here would be a silent security-relevant drop (dontguess-9ed
	// LOCKED-5).
	FoldDenialNotOperator atomic.Int64
	// FoldDenialBuyerIdentity counts STATE-fold rejections where a buyer-authored
	// settlement fold (settle buyer-accept / buyer-reject / complete /
	// small-content-dispute / preview-request) was dropped because its Sender is
	// not the buyer bound to the match (convention §5.3 buyer-identity gate).
	// A forged buyer-side message that reached the fold without being counted
	// would let a non-buyer bind/redirect/refund another buyer's match silently
	// (dontguess-9ed; extended to every buyer-identity fold guard in dontguess-471).
	FoldDenialBuyerIdentity atomic.Int64
	// FoldDenialAssignExclusive counts STATE-fold rejections where an
	// assign-claim was dropped because its Sender is not the assign's
	// ExclusiveSender (applyAssignClaim, state_assign.go). A forged claim on an
	// exclusively-targeted assign that reached the fold without being counted
	// would be a silent identity drop (dontguess-471 LOCKED-5).
	FoldDenialAssignExclusive atomic.Int64
	// FoldDenialAssignClaimant counts STATE-fold rejections where an
	// assign-complete was dropped because its Sender is not the ClaimantKey that
	// holds the claim (applyAssignComplete, state_assign.go). A forged completion
	// by a non-claimant that reached the fold without being counted would let a
	// non-worker submit a result for another agent's claim silently (dontguess-471).
	FoldDenialAssignClaimant atomic.Int64
}

// DegradationCounts is a plain (non-atomic) point-in-time copy of
// DegradationMetrics, safe to marshal to JSON for the status/observability
// path (cmd/dontguess/status.go).
type DegradationCounts struct {
	TrustDenialNotAllowlisted int64 `json:"trust_denial_not_allowlisted"`
	TrustDenialNotOperator    int64 `json:"trust_denial_not_operator"`
	TrustDenialLowReputation  int64 `json:"trust_denial_low_reputation"`
	TrustDenialOther          int64 `json:"trust_denial_other"`
	DroppedUnlisted           int64 `json:"dropped_unlisted"`
	DroppedLowReputation      int64 `json:"dropped_low_reputation"`
	DroppedDedupPoison        int64 `json:"dropped_dedup_poison"`
	DroppedUnderfundedBuy     int64 `json:"dropped_underfunded_buy"`
	FoldDenialNotOperator     int64 `json:"fold_denial_not_operator"`
	FoldDenialBuyerIdentity   int64 `json:"fold_denial_buyer_identity"`
	FoldDenialAssignExclusive int64 `json:"fold_denial_assign_exclusive"`
	FoldDenialAssignClaimant  int64 `json:"fold_denial_assign_claimant"`
}

// foldDenialReason identifies which security-relevant State.Apply fold guard
// dropped a message, so the wired counter callback (set in NewEngine) can bucket
// it into the matching DegradationMetrics counter and emit a loud log line —
// closing the silent nil-drop the guards previously did (dontguess-9ed LOCKED-5).
type foldDenialReason int

const (
	// foldDenialNotOperator: an operator-only settlement fold guard rejected a
	// non-operator sender (put-accept / put-reject / deliver).
	foldDenialNotOperator foldDenialReason = iota
	// foldDenialBuyerIdentity: a buyer-authored settlement fold (buyer-accept /
	// buyer-reject / complete / small-content-dispute / preview-request) rejected
	// because the sender is not the buyer bound to the match.
	foldDenialBuyerIdentity
	// foldDenialAssignExclusive: assign-claim rejected because the sender is not
	// the assign's ExclusiveSender.
	foldDenialAssignExclusive
	// foldDenialAssignClaimant: assign-complete rejected because the sender is not
	// the ClaimantKey holding the claim.
	foldDenialAssignClaimant
)

// String renders a foldDenialReason for the alarm log line.
func (r foldDenialReason) String() string {
	switch r {
	case foldDenialNotOperator:
		return "not-operator"
	case foldDenialBuyerIdentity:
		return "buyer-identity"
	case foldDenialAssignExclusive:
		return "assign-exclusive-sender"
	case foldDenialAssignClaimant:
		return "assign-claimant"
	default:
		return "unknown"
	}
}

// engineCtx returns the shutdown context stored at Start time.
// Falls back to context.Background() when the engine has not been started
// (e.g., in tests that call handlers directly without Start).
func (e *Engine) engineCtx() context.Context {
	if v := e.ctx.Load(); v != nil {
		return v.(context.Context)
	}
	return context.Background()
}

// marshal calls marshalFunc if set, otherwise json.Marshal.
func (e *Engine) marshal(v any) ([]byte, error) {
	if e.marshalFunc != nil {
		return e.marshalFunc(v)
	}
	return json.Marshal(v)
}

// NewEngine creates an exchange engine. Call Start to begin processing.
func NewEngine(opts EngineOptions) *Engine {
	idx := opts.MatchIndex
	if idx == nil {
		idx = matching.NewIndex(opts.Embedder, matching.RankOptions{})
	}
	st := NewState()
	if opts.OperatorPublicKey != "" {
		st.OperatorKey = opts.OperatorPublicKey
	}
	// Thread the operator signer into State BEFORE any Replay so applyPut can
	// decrypt-then-gate v2 puts during both live-fold and startup replay
	// (dontguess-4bed, §3.1(2)/§3.6).
	//
	// ADV-7 / design §6 + §9 Gate A/P4 — DECOUPLE confidentiality from payment.
	// encryptedRequired is armed on relay-attachment ALONE (OperatorSigner != nil),
	// independent of ScripStore. The pre-decouple gate ANDed the two
	// (ScripStore != nil && OperatorSigner != nil), which meant any future
	// relay-attached-but-SCRIPLESS rung — an operator with a signer but no scrip
	// ledger — would silently broadcast PLAINTEXT puts to the relay. Gating on the
	// signer alone fail-closes that latent leak: the instant an operator can sign
	// for a relay it MUST require ciphertext, whether or not it charges scrip. In
	// production the individual tier keeps operatorSigner a TRUE nil interface (no
	// relay, plaintext-local — §541 §6), so encryptedRequired stays off exactly as
	// before; the team tier sets a signer and is fail-closed. A relay-attached rung
	// that has no ScripStore (scripless) now correctly requires encryption too.
	st.operatorSigner = opts.OperatorSigner
	st.encryptedRequired = opts.OperatorSigner != nil
	e := &Engine{
		opts:               opts,
		state:              st,
		matchIndex:         idx,
		matchToReservation: make(map[string]string),
		buyerRecentEntries: make(map[string][]buyerSessionEntry),
		localMsgByID:       make(map[string]*Message),
		degradation:        &DegradationMetrics{},
	}
	// Wire the State fold-guard denial callback so security-relevant silent
	// drops inside State.Apply (operator-only settlement guards; the
	// buyer-identity gate) are counted into the same DegradationMetrics the
	// dispatch trust gate uses and alarmed with a loud log line — never a bare
	// nil (dontguess-9ed LOCKED-5). Skipped during State.Replay (see State.replaying)
	// so a full log rebuild does not re-inflate the live counters each restart.
	st.onFoldDenial = func(reason foldDenialReason, msg *Message) {
		switch reason {
		case foldDenialNotOperator:
			e.degradation.FoldDenialNotOperator.Add(1)
		case foldDenialBuyerIdentity:
			e.degradation.FoldDenialBuyerIdentity.Add(1)
		case foldDenialAssignExclusive:
			e.degradation.FoldDenialAssignExclusive.Add(1)
		case foldDenialAssignClaimant:
			e.degradation.FoldDenialAssignClaimant.Add(1)
		}
		e.opts.log("engine: fold rejected msg=%s reason=%s sender=%s",
			msg.ID, reason, shortKey(msg.Sender))
	}
	return e
}

// State returns the engine's live state view.
func (e *Engine) State() *State {
	return e.state
}

// LocalStore returns the engine's campfire-free append-only event log
// (EngineOptions.LocalStore), or nil when the engine was not configured with
// one. READ-ONLY accessor — callers (e.g. the individual-tier IPC handler,
// cmd/dontguess OpPut/OpBuy, dontguess-2b4) use it to ReadAll the log to
// locate a folded response record. Callers MUST NOT call Append on it: the
// only correct external-record write seam is IngestLocalRecord, which
// serializes the append+fold with the poll loop under localMu. The store's
// own internal mutex makes concurrent ReadAll safe against the engine's poll
// loop.
func (e *Engine) LocalStore() *dgstore.Store {
	return e.opts.LocalStore
}

// IngestLocalRecord appends an EXTERNAL (client-originated) record — an
// individual-tier OpPut/OpBuy routed through the running `serve` over the
// operator socket (design docs/design/nostr-first-client-ed2.md §3.3,
// dontguess-2b4) — into LocalStore and then folds+dispatches the whole
// undispatched gap SYNCHRONOUSLY via the shared absolute path, so the record's
// handler (handleBuy/handlePut) has run before this returns and the socket op can
// observe its result inline.
//
// An external record has no in-engine emitter, so it MUST be folded into State
// AND dispatched (a buy has to reach handleBuy to produce a match). It is the
// opposite of an operator record, which the emitter applies to State and which is
// NEVER dispatched.
//
// ABSOLUTE, IDENTITY-KEYED INGEST, FOLD-UNDER-localMu (dontguess-e2a3, ratified
// Design A + individual-tier regression fix). Earlier this method advanced
// localSeen/localDispatched with a RELATIVE `++`, correct ONLY when localSeen ==
// len(store) at append time. Once appendLocalRecord stopped maintaining that
// invariant (it no longer advances cursors — the team/relay tier violates it via
// out-of-localMu BatchAppend), a `++` here would mis-attribute: an operator record
// emitted since the last poll sits un-folded below the tail, so `++` would land on
// ITS slot and leave THIS external record beyond the cursor — re-dispatched by the
// next poll (a DUPLICATED match/put). So the cursors advance ABSOLUTELY (to the
// snapshot length), every record is folded in physical order, and dispatch skips
// operator-self-applied records by Sender (dispatchLocalGap).
//
// FOLD-BEFORE-DISPATCH SERIALIZATION (the individual-tier fix). The append, the
// fold (state.Apply of the whole undispatched fold-gap), and the dispatch-cursor
// claim are ALL performed under a SINGLE localMu hold; only the dispatch runs
// OUTSIDE the lock. Holding localMu across the fold is load-bearing on the LIVE
// operator path: a concurrent IngestLocalRecord (an op-put) fully folds its record
// into inventory/State and RELEASES localMu before this call (an op-buy) can
// acquire localMu, claim its gap, and dispatch — so a buy can NEVER dispatch
// against inventory that has not yet absorbed a concurrent put ("match resolved to
// no entry"). An earlier e2a3 pass delegated to the poll loop's
// foldAndDispatchLocalSnapshot, which folds OUTSIDE localMu (correct for the SINGLE
// self-serialized poll goroutine, but it let two concurrent socket ops race their
// state.Apply against each other's dispatch). Inlining the fold under localMu
// restores the pre-e2a3 guarantee WITHOUT reintroducing the relative-`++`.
//
// This does NOT regress a5e's off-lock Blossom prefetch: that lives on the
// TEAM/relay poll path (foldAndDispatchLocalSnapshot), which is untouched.
// IngestLocalRecord is individual-tier only, and the gap folded here carries at
// most the one external put's (local) blob plus operator records that reference no
// blob. Lock ordering is safe: state.Apply takes State.mu while this holds localMu,
// and no path holds State.mu while acquiring localMu, so localMu -> State.mu never
// cycles. Dispatch runs outside localMu because dispatch handlers append operator
// records via appendLocalRecord, which re-takes localMu.
//
// Individual tier only (ScripStore==nil / TrustChecker==nil): no scrip, no mint.
// A store-Append/Replay failure is returned; a per-record dispatch failure is
// logged (as on the poll path) — the gap may span more than this one record, so no
// single dispatch error is attributable to the caller.
func (e *Engine) IngestLocalRecord(rec dgstore.Record) error {
	if e.opts.LocalStore == nil {
		return fmt.Errorf("engine: IngestLocalRecord: no local store configured")
	}
	if rec.ID == "" {
		return fmt.Errorf("engine: IngestLocalRecord: record ID must not be empty")
	}

	e.localMu.Lock()
	if err := e.opts.LocalStore.Append(rec); err != nil {
		e.localMu.Unlock()
		return fmt.Errorf("engine: IngestLocalRecord: append %s: %w", shortKey(rec.ID), err)
	}
	// Snapshot under localMu so the tail is authoritative — no concurrent
	// appendLocalRecord/ingest can interleave a record between this Append and the
	// fold below, and the fold cursor claimed here is consistent with THIS length.
	snap, err := e.opts.LocalStore.Replay()
	if err != nil {
		e.localMu.Unlock()
		return fmt.Errorf("engine: IngestLocalRecord: replay %s: %w", shortKey(rec.ID), err)
	}
	total := len(snap)
	// Fold the whole undispatched fold-gap into State under localMu, advancing the
	// fold cursor absolutely. Guarded so a prior rebuild's already-folded prefix
	// ([:localSeen]) is never double-folded and the cursor never regresses.
	foldStart := e.localSeen
	if total > e.localSeen {
		e.indexLocalMessages(snap[e.localSeen:total])
		e.localSeen = total
	}
	for i := foldStart; i < total; i++ {
		e.state.Apply(&snap[i])
	}
	// Claim the dispatch gap monotonically, consistent with THIS snapshot length.
	dispatchStart := e.claimDispatchGap(total)
	e.localMu.Unlock()

	// Dispatch the undispatched gap OUTSIDE localMu (dispatch handlers re-take
	// localMu via appendLocalRecord). Operator-self-applied records are folded
	// above but skipped here (dispatchLocalGap); the external buy/put runs its
	// handler exactly once.
	e.dispatchLocalGap(snap, dispatchStart, total)
	return nil
}

// DegradationSnapshot returns a point-in-time copy of the trust-denial
// counters (docs/design/relay-transport.md §2.4a D4 + §3) for reporting —
// the CLI status/observability path (cmd/dontguess/status.go) and tests.
func (e *Engine) DegradationSnapshot() DegradationCounts {
	return DegradationCounts{
		TrustDenialNotAllowlisted: e.degradation.TrustDenialNotAllowlisted.Load(),
		TrustDenialNotOperator:    e.degradation.TrustDenialNotOperator.Load(),
		TrustDenialLowReputation:  e.degradation.TrustDenialLowReputation.Load(),
		TrustDenialOther:          e.degradation.TrustDenialOther.Load(),
		DroppedUnlisted:           e.degradation.DroppedUnlisted.Load(),
		DroppedLowReputation:      e.degradation.DroppedLowReputation.Load(),
		DroppedDedupPoison:        e.degradation.DroppedDedupPoison.Load(),
		DroppedUnderfundedBuy:     e.degradation.DroppedUnderfundedBuy.Load(),
		FoldDenialNotOperator:     e.degradation.FoldDenialNotOperator.Load(),
		FoldDenialBuyerIdentity:   e.degradation.FoldDenialBuyerIdentity.Load(),
		FoldDenialAssignExclusive: e.degradation.FoldDenialAssignExclusive.Load(),
		FoldDenialAssignClaimant:  e.degradation.FoldDenialAssignClaimant.Load(),
	}
}

// DeAllowlistSeller revokes a seller's fleet membership at runtime (dontguess-d53
// Seam C driver). It (1) removes the key from the live allowlist KeySet so every
// subsequent put promotion (Seam A) and index re-gate (Seam D) rejects it, (2)
// flags the seller's already-accepted inventory NeedsRevalidation so
// findCandidates withholds those entries immediately (engine_buy.go), and (3)
// rebuilds the match index so Seam D drops the now-anonymous seller's entries out
// of the searchable index in the same call. No-op when no TrustChecker is
// configured (individual/no-relay tier). Returns the number of entries withheld.
//
// Acquires opMu so it serializes against the auto-accept ticker and the operator
// socket handler (matchIndex mutations are also internally locked).
func (e *Engine) DeAllowlistSeller(sellerKey string) int {
	e.opMu.Lock()
	defer e.opMu.Unlock()
	if e.opts.TrustChecker != nil {
		// Remove from the admission allowlist (no NEW puts) AND record the durable
		// revocation tombstone (dontguess-23c) so SEAM D withholds this seller's
		// already-accepted inventory across restarts — the anti-poisoning invariant
		// no longer relies on mere absence from the allowlist (which an ephemeral
		// seller also has), but on an explicit, persisted "revoked for cause" mark.
		e.opts.TrustChecker.RemoveMember(sellerKey)
		e.opts.TrustChecker.RevokeMember(sellerKey)
	}
	n := e.state.FlagSellerEntriesForRevalidation(sellerKey)
	e.rebuildMatchIndex()
	e.opts.log("SECURITY: de-allowlisted seller %s — %d inventory entr(y/ies) withheld pending re-validation (Seam C/D)",
		shortKey(sellerKey), n)
	return n
}

// ReAllowlistSeller re-admits a previously de-allowlisted seller (dontguess-23c):
// it clears the revocation tombstone and rebuilds the match index so the seller's
// RETAINED inventory (never purged — publisher-owned content) re-enters the
// searchable set immediately, no restart required. The KeySet membership add
// (admission for NEW puts) is handled by the allowlist controller; this owns the
// retention side (revoked set + index). No-op when no TrustChecker is configured.
// Acquires opMu, serializing against the auto-accept ticker like DeAllowlistSeller.
func (e *Engine) ReAllowlistSeller(sellerKey string) {
	e.opMu.Lock()
	defer e.opMu.Unlock()
	if e.opts.TrustChecker == nil {
		return
	}
	e.opts.TrustChecker.UnrevokeMember(sellerKey)
	e.rebuildMatchIndex()
	e.opts.log("re-allowlisted seller %s — revocation cleared, retained inventory re-indexed", shortKey(sellerKey))
}

// MatchIndexLen returns the number of entries currently in the semantic match index.
// Useful for tests and diagnostics.
func (e *Engine) MatchIndexLen() int {
	return e.matchIndex.Len()
}

// Start replays the full message log to build initial state, processes any
// pending orders from the replay, then runs the event loop until ctx is cancelled.
func (e *Engine) Start(ctx context.Context) error {
	e.ctx.Store(ctx)
	// Warn operators who enable brokered matching without the federation guard.
	// Without FederationGuardEnabled, new/untrusted federation nodes bypass the
	// dual trust guard and receive brokered routing unconditionally.
	if e.opts.BrokeredMatchMode && !e.opts.FederationGuardEnabled {
		e.opts.log("engine: WARN BrokeredMatchMode enabled but FederationGuardEnabled=false — federation nodes bypass trust guard; set FederationGuardEnabled=true in production")
	}
	if err := e.replayAll(); err != nil {
		return fmt.Errorf("exchange engine replay: %w", err)
	}
	// Local mode: seed the dispatch cursor to the fold cursor. The startup
	// replayAll (replayAllLocal) folded the entire log into State without
	// dispatching; dispatchPendingOrders below dispatches the pending buys, and
	// the puts/settles/matches already in the log need no live dispatch (their
	// effect is fully captured by the fold) — exactly how the campfire path
	// treats a startup replay + dispatchPendingOrders. Records appended AFTER
	// startup are what the poll loop dispatches, from this cursor forward.
	// Seeding here (before dispatchPendingOrders, whose handlers may append
	// operator messages via appendLocalRecord) keeps the fold and dispatch
	// cursors aligned so those appends increment both correctly (dontguess-b84).
	if e.opts.LocalStore != nil {
		e.localMu.Lock()
		e.localDispatched = e.localSeen
		e.localMu.Unlock()
	}
	// Dispatch pending unmatched orders that were already in the log at startup.
	e.dispatchPendingOrders()
	// Startup-complete readiness signal (dontguess-fe9f): fired after the fold,
	// cursor seed, and dispatchPendingOrders, immediately before the steady-state
	// poll loop. A nil hook is a strict no-op.
	if e.opts.OnStarted != nil {
		e.opts.OnStarted()
	}
	return e.run(ctx)
}

// dispatchPendingOrders processes any active buy orders that have not yet been
// matched. Called after replay to handle orders that arrived before the engine
// started. Per-order fetch/handle failures are logged and skipped; there is no
// aggregate error to surface, so this returns nothing (dontguess-471 dead-code
// cleanup — the sole caller's error branch was unreachable).
func (e *Engine) dispatchPendingOrders() {
	orders := e.state.ActiveOrders()
	for _, order := range orders {
		msg, err := e.fetchMessage(order.OrderID)
		if err != nil || msg == nil {
			e.opts.log("engine: could not fetch order message %s: %v", order.OrderID, err)
			continue
		}
		// Dispatch the buy (triggers match response).
		if err := e.handleBuy(msg); err != nil {
			e.opts.log("engine: error handling pending buy %s: %v", order.OrderID, err)
		}
	}
}

// fetchMessage retrieves a single message by ID from the engine's local ingest
// index (localMsgByID), populated as records are folded from LocalStore — the
// sole ingest path. Returns nil (no error) when the ID is unknown.
func (e *Engine) fetchMessage(id string) (*Message, error) {
	e.localMu.Lock()
	defer e.localMu.Unlock()
	return e.localMsgByID[id], nil
}

// indexLocalMessages records each message's ID -> *Message mapping in
// localMsgByID, so fetchMessage and sendLocalOperatorMessage can look
// messages up without a campfire Get/GetMessage (dontguess-275). Caller must
// hold localMu.
func (e *Engine) indexLocalMessages(msgs []Message) {
	for i := range msgs {
		m := msgs[i]
		e.localMsgByID[m.ID] = &m
	}
}

// replayAll rebuilds state by replaying the full LocalStore event log — the
// sole, campfire-free ingest path (see replayAllLocal). A nil LocalStore is a
// no-op: a state-only test engine that never serves has nothing to replay.
func (e *Engine) replayAll() error {
	if e.opts.LocalStore == nil {
		return nil
	}
	return e.replayAllLocal()
}

// refreshBeforeOperatorOp brings engine State current immediately before an
// operator-driven put-accept / put-reject decision (AutoAcceptPut / RejectPut),
// so GetPendingPut observes the latest appended records.
//
// In campfire mode it re-reads the campfire (replayAll). In local mode it uses
// rebuildAndDispatchGapLocal, which — unlike a bare replayAll — also dispatches
// any buy that was appended since the last poll instead of folding it into
// State and advancing the fold cursor past it undispatched (dontguess-b84).
func (e *Engine) refreshBeforeOperatorOp() error {
	if e.opts.LocalStore != nil {
		return e.rebuildAndDispatchGapLocal()
	}
	return e.replayAll()
}

// replayAllLocal loads every record currently in LocalStore (pkg/store) and
// rebuilds state from it — the campfire-free ingest path (dontguess-275). A
// single local writer appends put/buy/settle/match/etc. records to an
// append-only JSONL log; append order is fold order (pkg/store package doc),
// so replaying the full log in order reproduces the same state a campfire
// fold would (proved by dontguess-331's local_store_fold_test.go).
func (e *Engine) replayAllLocal() error {
	msgs, err := e.opts.LocalStore.Replay()
	if err != nil {
		return fmt.Errorf("reading local store for replay: %w", err)
	}

	// M2 (dontguess-50d): under multi-relay nostr ingest the persisted log is
	// no longer in causal order — an e-tagged event can be stored before its
	// antecedent. Re-derive the canonical, deterministic fold order from the
	// antecedent DAG via the operator-side Sequencer, and FAIL LOUD on a broken
	// causal closure (pruned antecedent) rather than folding a truncated chain.
	if e.opts.SequencedIngest {
		ordered, seqErr := SequenceForFold(msgs, e.opts.MaxOrphans)
		if seqErr != nil {
			return fmt.Errorf("sequencing local store for replay: %w", seqErr)
		}
		msgs = ordered
	}

	e.localMu.Lock()
	e.indexLocalMessages(msgs)
	e.localSeen = len(msgs)
	e.localMu.Unlock()

	e.state.Replay(msgs)
	e.rebuildMatchIndex()

	e.opts.log("engine: replayed %d messages from local store, indexed %d entries",
		len(msgs), e.matchIndex.Len())
	return nil
}

// run is the main event loop: it polls the campfire-free LocalStore event log
// on a ticker (runLocal), folding and dispatching newly-appended records and
// running expiry sweeps on the same cadence. A nil LocalStore blocks until
// shutdown so Start still honors its run-until-cancelled contract.
func (e *Engine) run(ctx context.Context) error {
	if e.opts.LocalStore == nil {
		<-ctx.Done()
		return ctx.Err()
	}
	return e.runLocal(ctx)
}

// runLocal is the campfire-free event loop (dontguess-275). LocalStore
// (pkg/store) has no subscribe/notify mechanism, so instead of a channel of
// pushed messages, runLocal polls the store on a ticker and applies+dispatches
// whatever is new since the last replay/poll (pollLocalStore). Expiry sweeps
// run on the same ticker cadence as the campfire path's backstop sweep.
func (e *Engine) runLocal(ctx context.Context) error {
	ticker := time.NewTicker(e.opts.pollInterval())
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if err := e.pollLocalStore(); err != nil {
				e.opts.log("engine: local store poll error: %v", err)
			}
			e.sweepExpiredClaims()
			e.sweepExpiredAuctions()
			e.sweepStalePredictionAssigns()
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// pollLocalStore re-reads the full LocalStore log, folds any records appended
// since the last fold into State, and dispatches any records not yet
// dispatched. LocalStore is single-writer and strictly append-order (pkg/store
// package doc), so length-based cursors are sufficient — no sequencer or
// orphan-antecedent buffer is needed here (that machinery is M2, dontguess-50d,
// for out-of-order multi-relay nostr ingest).
//
// The fold cursor (localSeen) and dispatch cursor (localDispatched) are
// tracked separately. A concurrent state rebuild (rebuildAndDispatchGapLocal,
// driven by the auto-accept goroutine) folds records into State and advances
// localSeen WITHOUT advancing localDispatched, leaving a [localDispatched:
// localSeen] gap of folded-but-undispatched records. Dispatching off the
// dispatch cursor — not the fold cursor — is what guarantees those records
// (e.g. a buy appended just before an auto-accept tick) are still matched
// rather than silently dropped (dontguess-b84). Both cursors are claimed under
// localMu before dispatch runs outside the lock, so each record is dispatched
// exactly once even when this poll races the auto-accept rebuild.
func (e *Engine) pollLocalStore() error {
	msgs, err := e.opts.LocalStore.Replay()
	if err != nil {
		return fmt.Errorf("polling local store: %w", err)
	}
	e.foldAndDispatchLocalSnapshot(msgs)
	return nil
}

// PollLocalStoreForTest runs exactly ONE poll cycle synchronously — the identical
// body the run-loop ticker invokes (runLocal → pollLocalStore): a full LocalStore
// Replay, an incremental fold of newly-appended records into State, and a
// dispatch of every not-yet-dispatched record through the real dispatch path
// (trust gate → handler → operator emit). It exists so cross-package tests
// (cmd/dontguess) can drive the real ingest/fold/dispatch synchronously instead of
// racing a wall-clock deadline against the 5–20ms background ticker, which
// FALSE-fails under CPU saturation. It is test-support only: purely additive, it
// changes no production behavior (production drives pollLocalStore off the ticker),
// and it is safe to call concurrently with a running poll loop because
// pollLocalStore's cursors are monotonic and dispatch-exactly-once under localMu.
func (e *Engine) PollLocalStoreForTest() error {
	return e.pollLocalStore()
}

// StartupReplayForTest runs the SYNCHRONOUS startup portion of Start — the exact
// pre-run-loop body: replayAll (fold the full LocalStore log into State AND
// rebuildMatchIndex, the Seam D reload re-gate), seed the dispatch cursor to the
// fold cursor, and dispatchPendingOrders — WITHOUT entering the blocking poll
// loop. It exists so a cross-package test (cmd/dontguess) can deterministically
// bring a fresh engine to the same post-startup state Start reaches, instead of
// launching eng.Start in a goroutine and then racing a wall-clock waitFor against
// it. That race is a real flake source: Start's replayAllLocal folds inventory
// (state.Replay) and THEN re-gates the index (rebuildMatchIndex) as two steps in
// one goroutine, so a test that polls Inventory()==1 as a "replay done" proxy can
// observe it in the window BETWEEN the two and assert on an index/flag the re-gate
// has not written yet (dontguess-c84). Driving the identical body synchronously
// removes that window. It is test-support only: purely additive, it changes no
// production behavior (production reaches this state via Start), and after it
// returns the fold cursor, dispatch cursor, folded State, and match index are all
// consistent — exactly as Start leaves them before its first poll tick.
func (e *Engine) StartupReplayForTest() error {
	if err := e.replayAll(); err != nil {
		return fmt.Errorf("startup replay: %w", err)
	}
	if e.opts.LocalStore != nil {
		e.localMu.Lock()
		e.localDispatched = e.localSeen
		e.localMu.Unlock()
	}
	e.dispatchPendingOrders()
	return nil
}

// RunPollLoopForTest runs ONLY the blocking poll loop — the exact post-startup
// portion of Start (its e.run(ctx) tail), WITHOUT the leading replayAll. It is
// the counterpart of StartupReplayForTest: a test drives the destructive startup
// replay synchronously first (StartupReplayForTest), then launches this in a
// goroutine for a live, concurrent poll loop — instead of `go eng.Start(ctx)`,
// whose replayAll folds the whole log with a full, destructive state.Replay in a
// goroutine that RACES the test's synchronous socket ops. Under CPU saturation
// that race widens: replayAll captures an empty startup snapshot, then its
// state.Replay(emptySnapshot) + `localSeen = len(snapshot)` overwrite the State
// and cursor a concurrently-completed OpPut already built, wiping the just-put
// entry out of inventory and the match index (a following OpBuy then false-misses
// — dontguess-c8b). Splitting startup (synchronous) from the poll loop
// (goroutine) removes that window while keeping a genuinely concurrent poll loop
// for tests that need one. Test-support only: purely additive, it runs the
// identical body Start's tail runs (production reaches it via Start), and the
// poll loop's cursors are monotonic / dispatch-exactly-once under localMu, so it
// is safe to run concurrently with socket ops.
func (e *Engine) RunPollLoopForTest(ctx context.Context) error {
	return e.run(ctx)
}

// foldAndDispatchLocalSnapshot folds any newly-appended records in the given
// LocalStore snapshot into State and dispatches any not-yet-dispatched records.
// It is split out of pollLocalStore so the snapshot passed to fold/dispatch is
// an explicit argument — the snapshot is captured by Replay() OUTSIDE localMu,
// so two callers (this poll loop and a concurrent rebuildAndDispatchGapLocal)
// can hold snapshots of DIFFERENT lengths and then serialize on localMu.
//
// Both cursors are claimed under localMu and advanced MONOTONICALLY: a stale
// (shorter) snapshot whose length is at or below an already-claimed cursor
// claims nothing and can never regress it. This is the invariant that makes a
// record dispatched EXACTLY ONCE across all concurrent callers. Without it, a
// poll holding an old snapshot (len 5) that reaches localMu after a rebuild
// advanced localDispatched to 7 would write localDispatched = 5, reopening the
// [5:7] gap and re-dispatching records 5,6 on the next poll — double-firing
// non-idempotent dispatch side effects (e.g. emitConsumeSignal's durable
// TagConsume append, which feeds the pricing loops). See claimDispatchGap.
func (e *Engine) foldAndDispatchLocalSnapshot(msgs []Message) {
	e.localMu.Lock()
	total := len(msgs)
	// Claim the fold gap: records appended since the last fold. A prior rebuild
	// may already have folded some of these via a full state.Replay, so we only
	// incrementally Apply from localSeen forward — never double-folding. Guarded
	// so a stale snapshot cannot regress the fold cursor either.
	foldStart := e.localSeen
	if total > e.localSeen {
		e.indexLocalMessages(msgs[e.localSeen:total])
		e.localSeen = total
	}
	// Claim the dispatch gap: every record not yet dispatched, advancing the
	// dispatch cursor monotonically. Claimed consistently with THIS snapshot —
	// the slice dispatched below is msgs[dispatchStart:total] for the same total.
	dispatchStart := e.claimDispatchGap(total)
	e.localMu.Unlock()

	// Fold newly-appended records into State (outside localMu; State has its own
	// lock). Records in [dispatchStart:foldStart] were already folded by a prior
	// rebuild and must not be re-Applied.
	for i := foldStart; i < total; i++ {
		e.state.Apply(&msgs[i])
	}
	// Dispatch every claimed-undispatched record (outside localMu — dispatch
	// handlers may append operator messages, which acquire localMu). Operator-
	// self-applied records are folded above but NEVER dispatched (dontguess-e2a3).
	e.dispatchLocalGap(msgs, dispatchStart, total)
}

// dispatchLocalGap dispatches msgs[start:total), SKIPPING operator-self-applied
// records (Sender == OperatorKey). This is the identity-keyed dispatch-suppression
// rule of dontguess-e2a3 (ratified Design A), shared by foldAndDispatchLocalSnapshot
// (the poll loop) and rebuildAndDispatchGapLocal.
//
// An operator record is applied to State by its own emitter at append time (and
// re-folded idempotently by the fold loop), so it must NEVER be dispatched:
// dispatching an operator settle would re-run handleSettle and re-settle a match
// (scrip double-burn); dispatching an operator match/consume is at best a no-op.
// Member records (buy/put/participant settle phases) carry a non-operator Sender
// and ARE dispatched — that is how matching and settlement progress.
//
// The skip is a PURE FUNCTION of the record's Sender (a durable log field), so it
// needs no in-memory suppress-set and holds identically on a cold replay. Reading
// e.state.OperatorKey is lock-free: it is set once in NewEngine and never mutated.
// The OperatorKey != "" guard keeps a mis-configured (empty-key) engine from
// suppressing dispatch of empty-sender records.
func (e *Engine) dispatchLocalGap(msgs []Message, start, total int) {
	opKey := e.state.OperatorKey
	for i := start; i < total; i++ {
		msg := &msgs[i]
		if opKey != "" && msg.Sender == opKey {
			continue // operator-self-applied: folded, never dispatched
		}
		if err := e.dispatch(msg); err != nil {
			e.opts.log("engine: dispatch error (msg=%s): %v", msg.ID, err)
		}
	}
}

// claimDispatchGap advances the dispatch cursor (localDispatched) to total and
// returns the start index of the undispatched gap [start:total). The caller
// MUST hold localMu and MUST dispatch exactly msgs[start:total] over the SAME
// snapshot whose length is total.
//
// The advance is MONOTONIC: if total is at or below the already-claimed cursor
// (a stale/shorter snapshot, e.g. a poll whose Replay() ran before a concurrent
// rebuild advanced the cursor), the cursor is left untouched and start is
// returned >= total — an empty gap — so nothing is claimed and nothing is
// re-dispatched. This is the single point of truth for the dispatch cursor's
// monotonic, dispatch-exactly-once discipline shared by pollLocalStore
// (foldAndDispatchLocalSnapshot) and rebuildAndDispatchGapLocal (dontguess-b84).
func (e *Engine) claimDispatchGap(total int) int {
	start := e.localDispatched
	if total > e.localDispatched {
		e.localDispatched = total
	}
	return start
}

// rebuildAndDispatchGapLocal rebuilds State from the full LocalStore log (a
// pure fold via state.Replay) and then dispatches every record that has been
// folded but not yet dispatched — the [localDispatched:localSeen] gap. It is
// the local-mode state refresh used by the operator-driven put-accept /
// put-reject paths (AutoAcceptPut / RejectPut), replacing a plain replayAll()
// that folded externally-appended buys into State and advanced the fold cursor
// past them WITHOUT ever dispatching them — so the poll loop, keyed on the same
// cursor, never matched them (dontguess-b84).
//
// Dispatching the gap here (before the operator's own put-accept/reject message
// is appended) also restores the localDispatched == localSeen invariant that
// appendLocalRecord relies on. The gap is claimed under localMu before dispatch
// runs outside the lock, so a record is dispatched exactly once even when this
// races the poll loop.
//
// Monotonic single-point-of-truth discipline (dontguess-5f0): the fold cursor
// (localSeen), the folded State (state.Replay), and the match index all advance
// TOGETHER, under a single localMu hold, and ONLY when this snapshot grows the
// log (total > localSeen). A rebuild that captured a stale snapshot OUTSIDE the
// lock — one a concurrent poll/ingest has already superseded — must never
// regress localSeen or overwrite State with the older log, or the fold cursor
// inverts against the poll-advanced dispatch cursor (the [54:39] inversion) and
// entries fold away without ever matching. claimDispatchGap stays under the same
// lock so the dispatch gap remains monotonic even when the grow branch is
// skipped; a skipped grow simply leaves State/cursor at the newer poll-set value.
func (e *Engine) rebuildAndDispatchGapLocal() error {
	msgs, err := e.opts.LocalStore.Replay()
	if err != nil {
		return fmt.Errorf("reading local store for replay: %w", err)
	}
	total := len(msgs)

	e.localMu.Lock()
	if total > e.localSeen {
		// Grow branch only: advance fold cursor + State + index together,
		// atomically under localMu, from this (fresher-than-localSeen) snapshot.
		e.indexLocalMessages(msgs) // FULL msgs slice — not a tail slice
		e.localSeen = total
		e.state.Replay(msgs)  // under localMu + inside the grow guard (5f0)
		e.rebuildMatchIndex() // under localMu + inside the grow guard (5f0)
	}
	// Claim the dispatch gap monotonically (same discipline as the poll loop):
	// a rebuild holding a stale snapshot must not regress a dispatch cursor a
	// concurrent poll already advanced, or the gap reopens and records
	// re-dispatch. See claimDispatchGap (dontguess-b84 residual fix).
	dispatchStart := e.claimDispatchGap(total)
	e.localMu.Unlock()

	// Operator-self-applied records are folded above but NEVER dispatched
	// (dontguess-e2a3); dispatchLocalGap skips them by Sender.
	e.dispatchLocalGap(msgs, dispatchStart, total)

	e.opts.log("engine: rebuilt %d messages from local store, dispatched gap [%d:%d], indexed %d entries",
		total, dispatchStart, total, e.matchIndex.Len())
	return nil
}

// dispatch routes a new message to the appropriate handler.
func (e *Engine) dispatch(msg *Message) error {
	op := exchangeOp(msg.Tags)

	// Trust gate: check sender's trust level if a checker is configured.
	if e.opts.TrustChecker != nil {
		var phase SettlePhase
		if op == TagSettle {
			phase = SettlePhase(settlePhaseFromTags(msg.Tags))
		}
		trustOp := tagToTrustOp(op)
		if trustOp != "" {
			if err := e.opts.TrustChecker.Check(msg.Sender, trustOp, phase); err != nil {
				reason := e.recordTrustDenial(trustOp, phase, err)
				e.opts.log("engine: trust rejected msg=%s op=%s sender=%s reason=%s: %v",
					msg.ID, op, shortKey(msg.Sender), reason, err)
				// Counted + alarmed above (§2.4a D4) — the message is dropped
				// pre-fold and dispatch returns nil (not an error) so the poll
				// loop doesn't treat a routine trust rejection as a transport
				// fault, but the rejection itself is never a silent nil-drop.
				return nil
			}
		}
	}

	switch op {
	case TagPut:
		return e.handlePut(msg)
	case TagBuy:
		return e.handleBuy(msg)
	case TagSettle:
		return e.handleSettle(msg)
	case TagAssign:
		return e.handleAssign(msg)
	case TagAssignClaim:
		return e.handleAssignClaim(msg)
	case TagAssignComplete:
		return e.handleAssignComplete(msg)
	case TagAssignAccept:
		return e.handleAssignAccept(msg)
	case TagAssignReject:
		return e.handleAssignReject(msg)
	case TagAssignExpire:
		return e.handleAssignExpire(msg)
	case TagAssignAuctionClose:
		return nil // state.Apply handles finalization; engine logs only
	}
	return nil
}

// tagToTrustOp maps a campfire exchange operation tag to a trust Operation type.
// Returns "" for unknown/untracked operations (no trust check needed).
func tagToTrustOp(op string) Operation {
	switch op {
	case TagPut:
		return OperationPut
	case TagBuy:
		return OperationBuy
	case TagMatch:
		return OperationMatch
	case TagConsume:
		// Consume signals are operator-authored (emitConsumeSignal) and feed the
		// per-entry behavioral booster. Route through the operator-only
		// OperationConsume so a forged non-operator consume is gated + counted at
		// the trust gate instead of reaching the fold unchecked (dontguess-9ed).
		return OperationConsume
	case TagSettle:
		return OperationSettle
	// The seven assign-family sub-ops each carry their own trust level rather than
	// collapsing into a single flat OperationAssign bucket (which would wrongly
	// loosen the operator-only sub-ops). Operator sub-ops (post/accept/reject/
	// expire/auction-close) require TrustOperator; worker sub-ops (claim/complete)
	// require the fleet-member (allowlisted) level. See docs/design/relay-transport.md
	// §2.4a D3 and defaultOperationLevels in trust.go.
	case TagAssign:
		return OperationAssignPost
	case TagAssignClaim:
		return OperationAssignClaim
	case TagAssignComplete:
		return OperationAssignComplete
	case TagAssignAccept:
		return OperationAssignAccept
	case TagAssignReject:
		return OperationAssignReject
	case TagAssignExpire:
		return OperationAssignExpire
	case TagAssignAuctionClose:
		return OperationAssignAuctionClose
	default:
		return "" // unknown operation — no trust check
	}
}

// recordTrustDenial buckets a TrustChecker.Check rejection into exactly one
// DISTINCT counter on e.degradation and returns the reason string used in the
// dispatch log line. Mirrors the pkg/relay IntakeMetrics pattern
// (docs/design/relay-transport.md §2.4a D4): not-allowlisted, not-operator,
// and low-reputation are different attack/misconfiguration classes with
// different triage paths, so they are never collapsed into one bucket. Every
// path increments exactly one counter — none is a silent nil-drop (§3
// "provenance_rejected").
//
// required is looked up via TrustChecker.RequiredLevel (read-only; does not
// touch trust.go's Check/RequiredLevel logic) so a not-allowlisted rejection
// (required==TrustAllowlisted) can be told apart from a not-operator
// rejection (required==TrustOperator) — both surface through the same
// ErrInsufficientTrust sentinel from Check.
func (e *Engine) recordTrustDenial(op Operation, phase SettlePhase, err error) string {
	switch {
	case errors.Is(err, ErrLowReputation):
		e.degradation.TrustDenialLowReputation.Add(1)
		return "low-reputation"
	case errors.Is(err, ErrInsufficientTrust):
		if required, rlErr := e.opts.TrustChecker.RequiredLevel(op, phase); rlErr == nil && required == TrustOperator {
			e.degradation.TrustDenialNotOperator.Add(1)
			return "not-operator"
		}
		e.degradation.TrustDenialNotAllowlisted.Add(1)
		return "not-allowlisted"
	default:
		e.degradation.TrustDenialOther.Add(1)
		return "other"
	}
}

// sendOperatorMessage emits an operator-authored message (match/put-accept/
// settle/…) by appending it directly to LocalStore — the sole, campfire-free
// egress path (sendLocalOperatorMessage). Returns a dontguess *Message so
// callers don't depend on any store record type.
func (e *Engine) sendOperatorMessage(payload []byte, tags []string, antecedents []string) (*Message, error) {
	if e.opts.LocalStore == nil {
		return nil, fmt.Errorf("engine: LocalStore not configured — cannot send operator message")
	}
	return e.sendLocalOperatorMessage(payload, tags, antecedents)
}

// sendLocalOperatorMessage appends an operator-emitted message (match,
// put-accept, settle, etc.) directly to LocalStore — the fully campfire-free
// egress path (dontguess-275).
//
// The message ID is a random 32-hex-char string (same generator as
// newReservationID) since there is no signing identity to derive an ID from in
// this mode. Sender is state.OperatorKey, which EngineOptions callers set via
// OperatorPublicKey — there is no identity to derive it from otherwise.
func (e *Engine) sendLocalOperatorMessage(payload []byte, tags []string, antecedents []string) (*Message, error) {
	if tags == nil {
		tags = []string{}
	}
	if antecedents == nil {
		antecedents = []string{}
	}
	msg := &Message{
		ID:          newReservationID(),
		CampfireID:  e.opts.CampfireID,
		Sender:      e.state.OperatorKey,
		Payload:     payload,
		Tags:        tags,
		Antecedents: antecedents,
		Timestamp:   e.nextMonotonicTimestamp(),
	}
	if err := e.appendLocalRecord(msg); err != nil {
		return nil, fmt.Errorf("engine: appending local operator message: %w", err)
	}
	return msg, nil
}

// appendLocalRecord appends an operator-emitted msg to LocalStore and indexes it
// in localMsgByID. It DELIBERATELY does NOT advance localSeen/localDispatched
// (dontguess-e2a3, ratified Design A): the fold/dispatch cursors are now advanced
// ONLY by the absolute poll/rebuild/replay paths, which fold every record in
// physical order and SKIP dispatch for operator-self-applied records (Sender ==
// OperatorKey). See dispatchLocalGap.
//
// WHY THE RELATIVE `++` WAS REMOVED. The previous localSeen++/localDispatched++
// was correct ONLY when localSeen == len(store) at append time — an invariant the
// TEAM/relay tier violates: relay events are BatchAppend'd to the SAME LocalStore
// OUTSIDE localMu (pkg/relay/intake.go), advancing len(store) without advancing
// the cursor. So an operator emit here, while an un-folded relay buy sat at
// index == localSeen, mis-attributed the relative `++` to the relay buy's slot:
// the relay buy was marked folded+dispatched without ever running handleBuy (a
// permanently-skipped match — the 7ae3 confidentiality flaky), while the operator
// record beyond localSeen was re-folded (a scrip double-burn). Dropping the `++`
// collapses the individual-tier and team-tier ingest into ONE absolute,
// identity-keyed rule and makes the mis-attribution structurally impossible.
//
// The operator already applied its own emit to State synchronously (the emitter's
// state.Apply at the call site), so the poll loop re-folding this record is a
// benign double-apply the per-message-ID f86 dedup guards absorb (dontguess-f86);
// and dispatchLocalGap skips it (operator records are never dispatched). On a cold
// replayAllLocal the same Sender-derived skip holds — the decision is a pure
// function of the log, needing no in-memory suppress-set.
func (e *Engine) appendLocalRecord(msg *Message) error {
	rec := dgstore.Record{
		ID:          msg.ID,
		CampfireID:  msg.CampfireID,
		Sender:      msg.Sender,
		Payload:     msg.Payload,
		Tags:        msg.Tags,
		Antecedents: msg.Antecedents,
		Timestamp:   msg.Timestamp,
		Instance:    msg.Instance,
	}
	// OnLocalAppend post-emit hook (design §3.8, H1). Registered as a defer BEFORE
	// the localMu.Unlock defer so — defers running LIFO — it fires AFTER the unlock
	// and ONLY when the append succeeded (appended stays false on any early
	// return). Firing outside localMu keeps the fold-serializing mutex off the
	// external callback (the wired Outbox.Notify is non-blocking regardless). Nil
	// hook => strict no-op, byte-for-byte unchanged on the individual tier.
	appended := false
	defer func() {
		if appended && e.opts.OnLocalAppend != nil {
			e.opts.OnLocalAppend()
		}
	}()
	// Hold localMu across the store Append and the localMsgByID write so the record
	// and its index entry become visible together. No cursor is touched here
	// (dontguess-e2a3): the fold/dispatch cursors advance only in the absolute
	// poll/rebuild paths, which fold this record idempotently and skip its dispatch
	// by Sender. No deadlock: LocalStore.Append is plain fsynced file I/O in
	// pkg/store (guarded by the store's own mutex) and never re-enters localMu. On
	// Append error we return with localMu released and nothing mutated.
	e.localMu.Lock()
	defer e.localMu.Unlock()
	if err := e.opts.LocalStore.Append(rec); err != nil {
		return err
	}
	e.localMsgByID[msg.ID] = msg
	appended = true
	return nil
}

// handleAssign processes an exchange:assign message from the operator.
//
// State.Apply has already validated and recorded the assign (if the sender is
// the operator). The engine has no additional side-effects to perform here —
// the assign is now visible to agents polling the campfire.
func (e *Engine) handleAssign(msg *Message) error {
	e.opts.log("engine: assign posted assign_id=%s", shortKey(msg.ID))
	return nil
}

// handleAssignClaim processes an exchange:assign-claim message from an agent.
//
// State.Apply has already validated constraints (exclusive sender, slot
// availability). The engine logs the claim for diagnostics.
func (e *Engine) handleAssignClaim(msg *Message) error {
	e.opts.log("engine: assign-claim received claim_id=%s sender=%s",
		shortKey(msg.ID), shortKey(msg.Sender))
	return nil
}

// handleAssignComplete processes an exchange:assign-complete message from the
// claiming agent. The result is stored in state; the engine logs the event and
// waits for the operator to accept or reject.
func (e *Engine) handleAssignComplete(msg *Message) error {
	e.opts.log("engine: assign-complete received complete_id=%s sender=%s",
		shortKey(msg.ID), shortKey(msg.Sender))
	return nil
}

// handleAssignAccept processes an exchange:assign-accept message from the
// operator. If ScripStore is configured, the bounty is paid to the claimant
// and a scrip-assign-pay convention message is emitted so LocalScripStore
// can replay it.
//
// Antecedent: the assign-complete message ID. State.Apply has already
// validated that the sender is the operator and transitioned the record to
// AssignAccepted.
func (e *Engine) handleAssignAccept(msg *Message) error {
	if len(msg.Antecedents) == 0 {
		e.opts.log("engine: assign-accept: no antecedents, ignoring msg=%s", shortKey(msg.ID))
		return nil
	}
	completeMsgID := msg.Antecedents[0]

	// ClaimAssignPayment atomically transitions the record from AssignAccepted →
	// AssignPaid, returning the record only on that first transition. A replayed
	// accept message finds the record already at AssignPaid and gets nil back,
	// preventing a double-payment of the bounty.
	rec := e.state.ClaimAssignPayment(completeMsgID)
	if rec == nil {
		e.opts.log("engine: assign-accept: no payable assign for complete_id=%s (already paid or unknown), ignoring msg=%s",
			shortKey(completeMsgID), shortKey(msg.ID))
		return nil
	}

	// For compression tasks: create a derivative inventory entry from the result.
	if rec.TaskType == "compress" && rec.EntryID != "" {
		if err := e.createCompressionDerivative(rec, msg.ID); err != nil {
			e.opts.log("engine: assign-accept: create compression derivative: %v", err)
			// Non-fatal — log and continue to pay bounty.
		}
	}

	// Determine payment amount: for Vickrey auctions use the clearing price;
	// for standard assigns use the base reward.
	payAmount := rec.Reward
	if rec.VickreyPrice > 0 {
		payAmount = rec.VickreyPrice
	}

	if payAmount <= 0 {
		e.opts.log("engine: assign-accept: zero reward, skipping scrip payment assign_id=%s", shortKey(rec.AssignID))
		return nil
	}
	if rec.ClaimantKey == "" {
		e.opts.log("engine: assign-accept: no claimant recorded for assign_id=%s", shortKey(rec.AssignID))
		return nil
	}

	// Pay bounty to claimant.
	if e.opts.ScripStore != nil {
		ctx := e.engineCtx()
		if _, _, err := e.opts.ScripStore.AddBudget(ctx, rec.ClaimantKey, scrip.BalanceKey, payAmount, ""); err != nil {
			e.opts.log("engine: assign-accept: add bounty to worker %s: %v", shortKey(rec.ClaimantKey), err)
			return fmt.Errorf("assign-accept: pay bounty: %w", err)
		}

		// Emit scrip-assign-pay so LocalScripStore can replay the payment.
		payPayload, err := e.marshal(scrip.AssignPayPayload{
			Worker:    rec.ClaimantKey,
			Amount:    payAmount,
			TaskType:  rec.TaskType,
			AssignMsg: rec.AssignID,
		})
		if err != nil {
			e.opts.log("engine: assign-accept: marshal scrip-assign-pay: %v", err)
			return fmt.Errorf("assign-accept: marshal scrip-assign-pay: %w", err)
		}
		if _, emitErr := e.sendOperatorMessage(payPayload,
			[]string{scrip.TagScripAssignPay}, []string{msg.ID}); emitErr != nil {
			e.opts.log("engine: warning: emit scrip-assign-pay: %v", emitErr)
		}
	}

	e.opts.log("engine: assign-accept: bounty paid assign_id=%s worker=%s amount=%d",
		shortKey(rec.AssignID), shortKey(rec.ClaimantKey), payAmount)
	return nil
}

// AcceptAssign emits an operator-authored exchange:assign-accept for the
// assign-complete referenced by completeMsgID, folds it, and then
// synchronously invokes handleAssignAccept inline — paying the bounty via
// ClaimAssignPayment/ScripStore.AddBudget (and creating any compression
// derivative) in this SAME call, exactly the way AutoAcceptPut inlines its own
// put-promotion side effects rather than relying on the poll loop to dispatch
// what it just emitted.
//
// This exists to close a real gap (found while wiring dontguess-d26's agent
// CLI door and its enforcement-proof E2E test): dispatchLocalGap
// (engine_core.go) deliberately SKIPS dispatch for every record whose
// Sender==OperatorKey — an operator's own LOCAL emissions (e.g. AutoAcceptPut,
// RejectPut, MintScrip) are already synchronously handled at the call site, so
// re-dispatching them on the next poll would double-apply side effects like
// scrip payment. But handleAssignAccept had no such call-site — it was ONLY
// ever invoked from dispatch()'s op-switch — so it was UNREACHABLE for ANY
// operator-authored assign-accept, local OR relay-originated: the fold
// (State.Apply) transitions AssignCompleted -> AssignAccepted correctly, but
// the bounty is never paid. AutoAcceptPut/RejectPut/MintScrip never hit this
// because each does its real work inline instead of depending on dispatch
// re-observing its own emission; assign-accept was the one operator action
// that had a handler written but never wired to a reachable call site.
//
// Serializes via opMu against the auto-accept ticker and other operator god-
// buttons, matching every other method here.
func (e *Engine) AcceptAssign(completeMsgID string) error {
	e.opMu.Lock()
	defer e.opMu.Unlock()
	if completeMsgID == "" {
		return fmt.Errorf("engine: AcceptAssign: empty completeMsgID")
	}
	msg, err := e.sendOperatorMessage([]byte(`{}`), []string{TagAssignAccept}, []string{completeMsgID})
	if err != nil {
		return fmt.Errorf("engine: AcceptAssign: emit: %w", err)
	}
	e.state.Apply(msg)
	if err := e.handleAssignAccept(msg); err != nil {
		return fmt.Errorf("engine: AcceptAssign: handle: %w", err)
	}
	return nil
}

// createCompressionDerivative creates a new derivative inventory entry when a
// compression assign task is accepted. The derivative's content_hash and
// content_size come from the assign-complete result payload; the description,
// content_type, domains, token_cost, and seller_key are inherited from the
// original entry. The derivative is added directly to live inventory via
// applyDerivativePut and indexed in the match index so buyers can find it.
//
// Result payload fields (from the worker's assign-complete message):
//
//	content_hash  string  sha256: prefixed hash of compressed content
//	content_size  int64   byte size of compressed content
func (e *Engine) createCompressionDerivative(rec *AssignRecord, acceptMsgID string) error {
	// Look up the original entry.
	orig := e.state.GetInventoryEntry(rec.EntryID)
	if orig == nil {
		return fmt.Errorf("original entry %s not found in inventory", shortKey(rec.EntryID))
	}

	// Parse the result payload to extract content_hash and content_size.
	var result struct {
		ContentHash string `json:"content_hash"`
		ContentSize int64  `json:"content_size"`
	}
	if err := json.Unmarshal(rec.Result, &result); err != nil {
		return fmt.Errorf("parse assign-complete result: %w", err)
	}
	if result.ContentHash == "" {
		return fmt.Errorf("assign-complete result missing content_hash")
	}
	if !strings.HasPrefix(result.ContentHash, "sha256:") {
		return fmt.Errorf("assign-complete result content_hash %q does not have required sha256: prefix", result.ContentHash)
	}

	// Derive a stable EntryID from the accept message ID so that replaying
	// the same accept message always produces the same derivative ID.
	// This prevents duplicate inventory entries on engine restart + replay.
	h := sha256.Sum256([]byte(acceptMsgID + ":derivative"))
	derivativeID := hex.EncodeToString(h[:])

	// Build domains copy so the derivative doesn't share the original's slice.
	domainsCopy := make([]string, len(orig.Domains))
	copy(domainsCopy, orig.Domains)

	derivative := &InventoryEntry{
		EntryID:        derivativeID,
		PutMsgID:       acceptMsgID, // antecedent is the accept message
		SellerKey:      orig.SellerKey,
		Description:    orig.Description,
		ContentHash:    result.ContentHash,
		ContentType:    orig.ContentType,
		Domains:        domainsCopy,
		TokenCost:      orig.TokenCost,
		ContentSize:    result.ContentSize,
		PutPrice:       orig.PutPrice,
		PutTimestamp:   orig.PutTimestamp,
		CompressedFrom: orig.EntryID,
		// AcceptedProvenanceLevel and NeedsRevalidation inherit zero values;
		// trust checking is done at put-accept time for primary entries.
	}

	// Insert into state inventory (thread-safe via accessor).
	e.state.InsertDerivativePut(derivative)

	// Add to match index so buyers can find the derivative.
	e.matchIndex.Add(e.inventoryEntryToRankInput(derivative))

	e.opts.log("engine: assign-accept: created compression derivative entry_id=%s from=%s",
		shortKey(derivativeID), shortKey(orig.EntryID))
	return nil
}

// handleAssignReject processes an exchange:assign-reject message from the
// operator. The task is re-opened in state (State.Apply handles this).
// No scrip action is required on reject — the bounty was never paid.
func (e *Engine) handleAssignReject(msg *Message) error {
	e.opts.log("engine: assign-reject received reject_id=%s", shortKey(msg.ID))
	return nil
}

// handleAssignExpire processes an exchange:assign-expire message.
//
// State.Apply has already validated the sender and transitioned the record
// back to AssignOpen. The engine logs the expiry for diagnostics.
func (e *Engine) handleAssignExpire(msg *Message) error {
	e.opts.log("engine: assign-expire processed expire_id=%s", shortKey(msg.ID))
	return nil
}

// sweepExpiredClaims detects AssignClaimed records whose ClaimExpiresAt has
// passed and emits an exchange:assign-expire operator message for each one.
// State.Apply processes the emitted message and transitions the record back to
// AssignOpen. This is the backstop expiry path — the lazy path in ActiveAssigns
// handles the common case where another agent queries for open tasks.
//
// sweepExpiredClaims is called on every poll tick (backstop). It is a no-op
// when LocalStore is not configured (e.g. state-only test engines) —
// sendOperatorMessage requires LocalStore to emit anything.
func (e *Engine) sweepExpiredClaims() {
	if e.opts.LocalStore == nil {
		return
	}
	expired := e.state.ExpireStaleClaimsTS()

	for _, claimMsgID := range expired {
		payload, err := json.Marshal(map[string]string{
			"claim_id":    claimMsgID,
			"detected_at": time.Now().UTC().Format(time.RFC3339),
		})
		if err != nil {
			e.opts.log("engine: sweep: marshal assign-expire payload: %v", err)
			continue
		}
		expireMsg, err := e.sendOperatorMessage(payload, []string{TagAssignExpire}, []string{claimMsgID})
		if err != nil {
			e.opts.log("engine: sweep: emit assign-expire for claim=%s: %v", shortKey(claimMsgID), err)
			continue
		}
		// Apply the emitted expire message directly to state so we don't wait for
		// the next subscribe loop iteration to pick it up.
		e.state.Apply(expireMsg)
		e.opts.log("engine: sweep: claim expired, task reopened claim=%s", shortKey(claimMsgID))
	}
}

// sweepStalePredictionAssigns detects unclaimed AssignOpen standing
// (brokered-match) assigns whose DeadlineAt has passed and emits an
// operator-authored exchange:assign-expire message for each one, with the ASSIGN
// id as the antecedent. State.Apply (applyAssignExpire, standing-deadline path)
// transitions the record to the terminal AssignExpired state so it can no longer
// be claimed.
//
// This is the off-fold enforcement of standing-assign deadlines that replaced the
// removed wall-clock DeadlineAt guard in applyAssignClaim (event-sourced-pure
// ruling 2026-07-08): the operator observes the deadline off-fold (wall clock is
// fine here — this is the operator authoring an event, not the fold) and records
// the closure as an operator-authored message so the fold transition is
// deterministic on replay.
//
// Called on every poll tick (backstop). No-op when LocalStore is not configured.
func (e *Engine) sweepStalePredictionAssigns() {
	if e.opts.LocalStore == nil {
		return
	}
	stale := e.state.StalePredictionAssignsTS()

	for _, assignID := range stale {
		payload, err := json.Marshal(map[string]string{
			"assign_id":   assignID,
			"detected_at": time.Now().UTC().Format(time.RFC3339),
		})
		if err != nil {
			e.opts.log("engine: sweep stale: marshal assign-expire payload: %v", err)
			continue
		}
		expireMsg, err := e.sendOperatorMessage(payload, []string{TagAssignExpire}, []string{assignID})
		if err != nil {
			e.opts.log("engine: sweep stale: emit assign-expire for assign=%s: %v", shortKey(assignID), err)
			continue
		}
		e.state.Apply(expireMsg)
		e.opts.log("engine: sweep stale: standing assign deadline passed, closed assign=%s", shortKey(assignID))
	}
}

// sweepExpiredAuctions detects AssignOpen records whose AuctionDeadline has
// passed and that have at least one bid. For each such assign it emits an
// exchange:assign-auction-close message so that state finalizes the Vickrey
// auction and transitions the winner to AssignClaimed.
//
// sweepExpiredAuctions is called on every poll tick (backstop). It is a no-op
// when LocalStore is not configured.
func (e *Engine) sweepExpiredAuctions() {
	if e.opts.LocalStore == nil {
		return
	}
	pendingIDs := e.state.PendingAuctionCloseTS()

	for _, assignID := range pendingIDs {
		payload, err := json.Marshal(map[string]string{
			"assign_id": assignID,
			"closed_at": time.Now().UTC().Format(time.RFC3339),
		})
		if err != nil {
			e.opts.log("engine: sweep auctions: marshal auction-close payload: %v", err)
			continue
		}
		closeMsg, err := e.sendOperatorMessage(payload, []string{TagAssignAuctionClose}, []string{assignID})
		if err != nil {
			e.opts.log("engine: sweep auctions: emit assign-auction-close for assign=%s: %v", shortKey(assignID), err)
			continue
		}
		e.state.Apply(closeMsg)
		e.opts.log("engine: sweep auctions: auction closed assign=%s", shortKey(assignID))
	}
}

// newReservationID generates a random hex reservation ID.
func newReservationID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("rand.Read: %v", err))
	}
	return hex.EncodeToString(b)
}

// isLowTrustSender returns true if the sender's TrustScore is below
// NewNodeTrustThreshold, indicating the sender has not yet established
// sufficient behavioral history to receive brokered-match routing.
//
// Senders with no recorded profile are treated as new nodes (low trust).
// This implements the new-node dual guard described in §4A of the design spec.
func (e *Engine) isLowTrustSender(senderKey string) bool {
	profile := e.state.FederationProfile(senderKey)
	if profile == nil {
		// Unknown sender — treat as new node (low trust).
		return true
	}
	return profile.TrustScore < NewNodeTrustThreshold
}

// hasOverlap returns true if any element of a appears in b.
func hasOverlap(a, b []string) bool {
	set := make(map[string]struct{}, len(b))
	for _, v := range b {
		set[strings.ToLower(v)] = struct{}{}
	}
	for _, v := range a {
		if _, ok := set[strings.ToLower(v)]; ok {
			return true
		}
	}
	return false
}

// reservationFor returns the reservation ID recorded for a match at
// buyer-accept time, and whether one exists. Guarded by resvMu — the poll loop
// and the operator/auto-accept dispatch goroutine both read this map
// concurrently in local-relay mode (dontguess-471).
func (e *Engine) reservationFor(matchMsgID string) (string, bool) {
	e.resvMu.Lock()
	defer e.resvMu.Unlock()
	resID, ok := e.matchToReservation[matchMsgID]
	return resID, ok
}

// setReservation records the reservation ID created for a match. Guarded by
// resvMu (concurrent dispatch goroutines in local-relay mode, dontguess-471).
func (e *Engine) setReservation(matchMsgID, reservationID string) {
	e.resvMu.Lock()
	e.matchToReservation[matchMsgID] = reservationID
	e.resvMu.Unlock()
}

// deleteReservation removes the reservation mapping for a settled match.
// Guarded by resvMu (dontguess-471).
func (e *Engine) deleteReservation(matchMsgID string) {
	e.resvMu.Lock()
	delete(e.matchToReservation, matchMsgID)
	e.resvMu.Unlock()
}

// recordBuyerSettlement appends entryID to the buyer's recent-entries list
// and calls UpdateCoOccurrence for each prior entry in the session window.
// Entries older than buyerSessionWindow are pruned before pairing.
//
// Guarded by resvMu: in local-relay mode this runs from the poll-loop dispatch
// goroutine AND the operator/auto-accept dispatch goroutine concurrently
// (dontguess-471). The lock is held across the UpdateCoOccurrence calls; that is
// safe because State takes its own lock and never calls back into
// buyerRecentEntries, so no lock-ordering cycle exists.
func (e *Engine) recordBuyerSettlement(buyerKey, entryID string) {
	e.resvMu.Lock()
	defer e.resvMu.Unlock()

	now := time.Now()
	cutoff := now.Add(-buyerSessionWindow)

	prior := e.buyerRecentEntries[buyerKey]
	// Prune stale entries.
	valid := prior[:0]
	for _, entry := range prior {
		if entry.settledAt.After(cutoff) {
			valid = append(valid, entry)
		}
	}
	// Update co-occurrence for all prior entries in the session window.
	for _, prev := range valid {
		e.state.UpdateCoOccurrence(prev.entryID, entryID)
	}
	// Append current entry (bounded to avoid unlimited growth per buyer key).
	const maxBuyerSession = 10
	valid = append(valid, buyerSessionEntry{entryID: entryID, settledAt: now})
	if len(valid) > maxBuyerSession {
		valid = valid[len(valid)-maxBuyerSession:]
	}
	e.buyerRecentEntries[buyerKey] = valid
}

// stagePredictions posts standing exchange:assign messages with task_type="brokered-match"
// for the top predicted next-work entries following settledEntryID. Up to
// MaxPredictionFanout assigns are posted per call; existing open assigns for the
// same entry reduce the available slots (A9 mitigation). Each assign has a DeadlineAt
// of now + PredictionAssignTTL (2h) so expired assigns cannot be claimed.
//
// Non-fatal: errors are logged and do not propagate.
func (e *Engine) stagePredictions(settledEntryID string) {
	if e.opts.LocalStore == nil {
		return // state-only engine or test without a LocalStore egress path
	}
	predicted := e.state.PredictNext(settledEntryID)
	if len(predicted) == 0 {
		return
	}
	deadline := time.Now().Add(PredictionAssignTTL)
	deadlineStr := deadline.UTC().Format(time.RFC3339)
	reward := e.opts.brokeredMatchReward()

	posted := 0
	for _, predEntryID := range predicted {
		if posted >= MaxPredictionFanout {
			break
		}
		// Check how many open prediction assigns already exist for this entry.
		existing := e.state.OpenPredictionAssignsForEntry(predEntryID)
		if existing >= MaxPredictionFanout {
			continue
		}
		// Build the assign payload for a standing brokered-match offer.
		payload, err := e.marshal(map[string]any{
			"entry_id":    predEntryID,
			"task_type":   "brokered-match",
			"reward":      reward,
			"deadline_at": deadlineStr,
			"description": fmt.Sprintf("Predicted next-work for entry %s — brokered match standing offer", shortKey(settledEntryID)),
		})
		if err != nil {
			e.opts.log("engine: stagePredictions: marshal payload for entry=%s: %v", shortKey(predEntryID), err)
			continue
		}
		msg, err := e.sendOperatorMessage(payload, []string{TagAssign}, nil)
		if err != nil {
			e.opts.log("engine: stagePredictions: send assign for entry=%s: %v", shortKey(predEntryID), err)
			continue
		}
		if msg != nil {
			e.state.Apply(msg)
		}
		posted++
		e.opts.log("engine: stagePredictions: posted standing assign entry=%s deadline=%s",
			shortKey(predEntryID), deadlineStr)
	}
}

// findExistingBuyerAcceptHold returns the reservation ID for a prior scrip-buy-hold
// message matching the given match message ID, or "" if none exists.
//
// Called by handleSettleBuyerAcceptScrip to detect the restart-with-pending-orders
// scenario: if a scrip-buy-hold was already written to the log (and thus replayed by
// LocalScripStore into the in-memory balance), we must NOT call DecrementBudget
// again or the buyer will be double-charged.
//
// Uses the state matchToBuyHold index for O(1) lookup.
// State.Replay() populates the index by applying all scrip-buy-hold messages
// via applyScripBuyHold — no full log scan needed at query time.
func (e *Engine) findExistingBuyerAcceptHold(matchMsgID string) string {
	return e.state.GetBuyHoldReservation(matchMsgID)
}
