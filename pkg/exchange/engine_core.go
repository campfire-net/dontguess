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

	"github.com/campfire-net/campfire/cf-protocol/protocol"
	"github.com/campfire-net/campfire/cf-protocol/store"

	"github.com/campfire-net/dontguess/pkg/matching"
	"github.com/campfire-net/dontguess/pkg/scrip"
	dgstore "github.com/campfire-net/dontguess/pkg/store"
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
	// OperatorPublicKey is the hex-encoded Ed25519 public key of the exchange operator.
	// Used only for populating State.OperatorKey on startup. Send operations
	// use WriteClient (which carries its own identity).
	//
	// Deprecated: prefer OperatorPublicKey. OperatorIdentity was removed in the
	// cf SDK 0.13 migration (W3b). Set this to writeClient.PublicKeyHex() if
	// you previously passed the operator identity.
	OperatorPublicKey string
	// Store is the campfire message store (SQLite).
	// Deprecated: use ReadClient and WriteClient. Kept for backward compatibility
	// with callers that have not yet migrated (e.g. CampfireScripStore).
	// The engine no longer calls Store.ListMessages directly; all reads go via ReadClient.
	Store store.Store
	// ReadClient is the protocol client used to subscribe to and replay campfire messages.
	// If nil, the engine falls back to using Store directly (backward-compat path for tests).
	//
	// Ignored for ingest when LocalStore is set — see LocalStore doc.
	ReadClient *protocol.Client
	// WriteClient is the protocol client used to send operator-signed messages
	// (match, settle, burn). It must carry the operator's identity and have
	// membership in CampfireID recorded in its backing store.
	//
	// If nil and LocalStore is set, the engine falls back to appending
	// operator messages directly to LocalStore instead (see LocalStore doc) —
	// this is the path a fully standalone, zero-campfire operator process
	// uses (dontguess-275).
	WriteClient *protocol.Client
	// LocalStore is the M1 (individual-tier) campfire-free event log
	// (pkg/store, dontguess-331) used for INGEST when configured.
	//
	// When set, replayAll and the poll loop (run) read exclusively from
	// LocalStore.Replay() instead of ReadClient.Read/Subscribe — this is the
	// standalone local-only cutover (dontguess-275): a single agent can
	// put/buy with zero campfire network dependency. LocalStore takes
	// priority over ReadClient for ingest whenever both are set.
	//
	// Egress (sendOperatorMessage, used by match/put-accept/settle/etc.)
	// still prefers WriteClient when configured, for callers mid-migration
	// that want local ingest but still emit onto a campfire (e.g. the
	// existing test harness). When WriteClient is nil, egress also falls
	// back to appending directly into LocalStore — see WriteClient doc.
	//
	// Single-writer only (per pkg/store's package doc): this milestone does
	// not add a sequencer or out-of-order/orphan-antecedent buffer. That is
	// M2 (dontguess-50d), needed once multi-relay nostr ingest can deliver
	// events out of order.
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
	// If nil, TF-IDF is used. Set to matching.NewDenseEmbedder("path")
	// for 384-dim all-MiniLM-L6-v2 semantic matching.
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
	// ReadSkipSync skips the filesystem sync step in Read operations.
	// Set to true when the ReadClient shares its store with the test harness's
	// store (h.st), so that messages written directly to h.st are visible
	// without re-syncing from the transport. Production code leaves this false.
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
	// localDispatched is the DISPATCH cursor: the count of LocalStore records
	// already passed through dispatch() (handleBuy/handlePut/... ), or otherwise
	// definitively handled (operator-emitted messages, which are applied to
	// state directly by their emitter and must never be re-dispatched). It is
	// deliberately SEPARATE from localSeen (the fold cursor): a full state
	// rebuild folds records into State without dispatching them, so
	// [localDispatched:localSeen] can be a non-empty gap of folded-but-never-
	// dispatched records. Conflating the two (advancing the dispatch cursor
	// during a rebuild that did not itself dispatch) is exactly the defect that
	// silently dropped buy orders — they landed in State.ActiveOrders, were
	// counted as "seen", and were never matched (dontguess-b84). The invariant:
	// every record folded into State is dispatched exactly once, and no record
	// is dispatched twice. Guarded by localMu; always localDispatched <=
	// localSeen. Only relevant when EngineOptions.LocalStore is configured.
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
	} else if opts.WriteClient != nil {
		// Derive operator key from the write client's identity (convenience path).
		st.OperatorKey = opts.WriteClient.PublicKeyHex()
	}
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

// DegradationSnapshot returns a point-in-time copy of the trust-denial
// counters (docs/design/relay-transport.md §2.4a D4 + §3) for reporting —
// the CLI status/observability path (cmd/dontguess/status.go) and tests.
func (e *Engine) DegradationSnapshot() DegradationCounts {
	return DegradationCounts{
		TrustDenialNotAllowlisted: e.degradation.TrustDenialNotAllowlisted.Load(),
		TrustDenialNotOperator:    e.degradation.TrustDenialNotOperator.Load(),
		TrustDenialLowReputation:  e.degradation.TrustDenialLowReputation.Load(),
		TrustDenialOther:          e.degradation.TrustDenialOther.Load(),
		FoldDenialNotOperator:     e.degradation.FoldDenialNotOperator.Load(),
		FoldDenialBuyerIdentity:   e.degradation.FoldDenialBuyerIdentity.Load(),
		FoldDenialAssignExclusive: e.degradation.FoldDenialAssignExclusive.Load(),
		FoldDenialAssignClaimant:  e.degradation.FoldDenialAssignClaimant.Load(),
	}
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

// fetchMessage retrieves a single message by ID. When LocalStore is
// configured, looks it up in the engine's local ingest index (dontguess-275)
// — LocalStore has no campfire Get/GetMessage to fall back on. Otherwise uses
// ReadClient.Get when ReadClient is configured (preferred), or falls back to
// Store.GetMessage.
func (e *Engine) fetchMessage(id string) (*Message, error) {
	if e.opts.LocalStore != nil {
		e.localMu.Lock()
		defer e.localMu.Unlock()
		return e.localMsgByID[id], nil
	}
	if e.opts.ReadClient != nil {
		m, err := e.opts.ReadClient.Get(id)
		if err != nil {
			return nil, err
		}
		return FromSDKMessage(m), nil
	}
	rec, err := e.opts.Store.GetMessage(id)
	if err != nil || rec == nil {
		return nil, err
	}
	// Legacy campfire Store fallback (LocalStore and ReadClient both nil).
	// FromStoreRecord now converts the campfire-free dgstore.Record
	// (dontguess-657), so the campfire store.MessageRecord is converted inline
	// here — the last campfire ingest remnant, removed with the Store field by
	// dontguess-b14.
	return &Message{
		ID:          rec.ID,
		CampfireID:  rec.CampfireID,
		Sender:      rec.Sender,
		Payload:     rec.Payload,
		Tags:        rec.Tags,
		Antecedents: rec.Antecedents,
		Timestamp:   rec.Timestamp,
		Instance:    rec.Instance,
	}, nil
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

// replayAll loads all historical messages and rebuilds state.
//
// When LocalStore is configured, ingest reads from the campfire-free
// pkg/store event log instead (dontguess-275) — see replayAllLocal.
// LocalStore takes priority whenever both LocalStore and ReadClient are set.
//
// Otherwise, uses ReadClient.Read with AfterTimestamp=0 to fetch all
// messages from the campfire. The SDK handles sync-before-query
// automatically for filesystem transports.
func (e *Engine) replayAll() error {
	if e.opts.LocalStore != nil {
		return e.replayAllLocal()
	}

	result, err := e.opts.ReadClient.Read(protocol.ReadRequest{
		CampfireID:     e.opts.CampfireID,
		AfterTimestamp: 0,
		SkipSync:       e.opts.ReadSkipSync,
	})
	if err != nil {
		return fmt.Errorf("reading messages for replay: %w", err)
	}

	msgs := FromSDKMessages(result.Messages)
	e.state.Replay(msgs)

	// Rebuild the match index from the current live inventory.
	e.rebuildMatchIndex()

	e.opts.log("engine: replayed %d messages, indexed %d entries",
		len(msgs), e.matchIndex.Len())
	return nil
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

// run is the main event loop.
//
// When LocalStore is configured, it polls the campfire-free pkg/store event
// log instead (dontguess-275) — see runLocal. LocalStore takes priority
// whenever both LocalStore and ReadClient are set.
//
// Otherwise, it subscribes to the campfire via ReadClient.Subscribe and
// dispatches messages as they arrive on the channel. An expiry sweep runs on
// each received message (lazy path) and on a periodic ticker (backstop path)
// to catch expired claims when no messages arrive.
func (e *Engine) run(ctx context.Context) error {
	if e.opts.LocalStore != nil {
		return e.runLocal(ctx)
	}

	sub := e.opts.ReadClient.Subscribe(ctx, protocol.SubscribeRequest{
		CampfireID: e.opts.CampfireID,
		Tags: []string{
			TagPut, TagBuy, TagSettle,
			TagAssign, TagAssignClaim, TagAssignComplete, TagAssignAccept, TagAssignReject,
			TagAssignExpire, TagAssignAuctionClose,
		},
		PollInterval: e.opts.pollInterval(),
	})

	// Expiry sweep ticker: backstop for catching expired claims when no messages
	// arrive. Uses the same poll interval as the subscribe loop.
	expirySweepTicker := time.NewTicker(e.opts.pollInterval())
	defer expirySweepTicker.Stop()

	// msgCh is used to multiplex the subscribe channel with the sweep ticker
	// inside a select loop. We process messages via goroutine → select.
	msgCh := sub.Messages()
	for {
		select {
		case sdkMsg, ok := <-msgCh:
			if !ok {
				// Channel closed — subscription ended.
				if err := sub.Err(); err != nil {
					return fmt.Errorf("engine: subscription error: %w", err)
				}
				return ctx.Err()
			}
			msg := FromSDKMessage(&sdkMsg)
			e.state.Apply(msg)
			if err := e.dispatch(msg); err != nil {
				e.opts.log("engine: dispatch error (msg=%s): %v", msg.ID, err)
			}
			// Lazy sweeps on every received message.
			e.sweepExpiredClaims()
			e.sweepExpiredAuctions()
		case <-expirySweepTicker.C:
			// Periodic backstop sweeps.
			e.sweepExpiredClaims()
			e.sweepExpiredAuctions()
		case <-ctx.Done():
			return ctx.Err()
		}
	}
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
	// handlers may append operator messages, which acquire localMu).
	for i := dispatchStart; i < total; i++ {
		msg := &msgs[i]
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
func (e *Engine) rebuildAndDispatchGapLocal() error {
	msgs, err := e.opts.LocalStore.Replay()
	if err != nil {
		return fmt.Errorf("reading local store for replay: %w", err)
	}
	total := len(msgs)

	e.localMu.Lock()
	e.indexLocalMessages(msgs)
	e.localSeen = total
	// Claim the dispatch gap monotonically (same discipline as the poll loop):
	// a rebuild holding a stale snapshot must not regress a dispatch cursor a
	// concurrent poll already advanced, or the gap reopens and records
	// re-dispatch. See claimDispatchGap (dontguess-b84 residual fix).
	dispatchStart := e.claimDispatchGap(total)
	e.localMu.Unlock()

	e.state.Replay(msgs)
	e.rebuildMatchIndex()

	for i := dispatchStart; i < total; i++ {
		msg := &msgs[i]
		if err := e.dispatch(msg); err != nil {
			e.opts.log("engine: dispatch error (msg=%s): %v", msg.ID, err)
		}
	}

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

// sendOperatorMessage sends an operator-signed message to the exchange campfire
// via the protocol WriteClient. Returns a dontguess *Message so callers don't
// depend on store.MessageRecord.
func (e *Engine) sendOperatorMessage(payload []byte, tags []string, antecedents []string) (*Message, error) {
	if e.opts.WriteClient == nil {
		if e.opts.LocalStore != nil {
			return e.sendLocalOperatorMessage(payload, tags, antecedents)
		}
		return nil, fmt.Errorf("engine: WriteClient not configured — cannot send operator message")
	}
	msg, err := e.opts.WriteClient.Send(protocol.SendRequest{
		CampfireID:  e.opts.CampfireID,
		Payload:     payload,
		Tags:        tags,
		Antecedents: antecedents,
	})
	if err != nil {
		return nil, fmt.Errorf("sending operator message: %w", err)
	}
	// Retrieve via the SDK's Get (sendFilesystem already mirrored the message
	// to the local store). This avoids importing the internal message package.
	sdkMsg, err := e.opts.WriteClient.Get(msg.ID)
	if err != nil {
		return nil, fmt.Errorf("retrieving sent message: %w", err)
	}
	if sdkMsg == nil {
		return nil, fmt.Errorf("sent message not found in store: %s", msg.ID)
	}
	result := FromSDKMessage(sdkMsg)

	// Mirror into LocalStore too, if configured, even though WriteClient
	// already delivered it to the campfire. replayAllLocal is authoritative
	// and unconditionally invoked at every Start() — it fully resets state
	// from LocalStore's log. If operator-emitted messages (put-accept,
	// match, settle, ...) existed only on the campfire side, the next
	// replayAllLocal call would silently drop them from state (accepted
	// puts revert to pending, matched orders revert to unmatched, etc.).
	// Mirroring keeps LocalStore a complete, replay-safe record of every
	// message this engine has processed or emitted (dontguess-275).
	if e.opts.LocalStore != nil {
		if mirrorErr := e.appendLocalRecord(result); mirrorErr != nil {
			e.opts.log("engine: mirroring operator message %s to local store: %v",
				shortKey(result.ID), mirrorErr)
		}
	}
	return result, nil
}

// sendLocalOperatorMessage appends an operator-emitted message (match,
// put-accept, settle, etc.) directly to LocalStore, with no campfire
// WriteClient involved at all — the fully campfire-free egress path
// (dontguess-275) used when the engine runs in local-only mode (LocalStore
// configured, WriteClient nil).
//
// The message ID is a random 32-hex-char string (same generator as
// newReservationID) since there is no campfire identity to derive an ID
// from in this mode. Sender is state.OperatorKey, which EngineOptions
// callers must set explicitly (via OperatorPublicKey) when running without a
// WriteClient — there is no identity to derive it from otherwise.
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

// appendLocalRecord appends msg to LocalStore and updates the engine's local
// ingest bookkeeping (localMsgByID, localSeen) so pollLocalStore does not
// re-ingest and re-dispatch a message the caller has already applied to
// state directly. Used both when LocalStore is the sole egress path
// (sendLocalOperatorMessage, WriteClient nil) and to mirror
// WriteClient-emitted operator messages into LocalStore (sendOperatorMessage)
// so a later replayAllLocal — authoritative, full state reset — does not
// lose them. See EngineOptions.LocalStore doc.
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
	// Hold localMu across BOTH the store Append and the cursor increments so the
	// two are ATOMIC with respect to the concurrent fold path (pollLocalStore /
	// rebuildAndDispatchGapLocal, via foldAndDispatchLocalSnapshot). Without this,
	// the record becomes observable to a concurrent LocalStore.Replay() snapshot
	// the instant Append returns, while foldStart (= localSeen) would not yet cover
	// it — so that fold's loop would state.Apply() an operator record its emitter
	// (e.g. emitConsumeSignal) ALSO Applies, a transient in-memory double of a
	// behavioral signal that feeds the pricing loops (dontguess-90d). Serializing
	// on localMu forces any fold to run either fully-BEFORE this Append (its
	// snapshot excludes the record; cursors unchanged) or fully-AFTER the increment
	// (its snapshot includes the record but foldStart already covers it → skipped),
	// so the record is applied to State exactly once, by its emitter.
	//
	// This holds localMu across Append's fsync; that contention is an accepted
	// tradeoff for correctness (dontguess-90d). No deadlock: LocalStore.Append is
	// plain fsynced file I/O in pkg/store (guarded by the store's own mutex) and
	// never re-enters any path that takes localMu. Cursors are advanced only on a
	// successful Append — on error we return with localMu released and no cursor,
	// map, or partial state mutated.
	e.localMu.Lock()
	defer e.localMu.Unlock()
	if err := e.opts.LocalStore.Append(rec); err != nil {
		return err
	}
	e.localMsgByID[msg.ID] = msg
	// Operator-emitted messages are applied to State directly by their emitter
	// and must never be re-dispatched, so advance BOTH cursors. This is correct
	// because every appendLocalRecord call site holds the invariant
	// localDispatched == localSeen at append time (the gap is always dispatched
	// before an operator message is appended): startup seeds them equal before
	// dispatchPendingOrders; pollLocalStore and rebuildAndDispatchGapLocal both
	// advance localDispatched to the log length before running the dispatch loop
	// whose handlers may append. Incrementing both keeps the new tail record
	// marked folded+handled without re-dispatch (dontguess-b84).
	e.localSeen++
	e.localDispatched++
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
// and a scrip-assign-pay convention message is emitted so CampfireScripStore
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

		// Emit scrip-assign-pay so CampfireScripStore can replay the payment.
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
// sweepExpiredClaims is called on every received message (lazy) and on the
// periodic expiry sweep ticker (backstop). It is a no-op if neither
// WriteClient nor LocalStore is configured (e.g. read-only engine instances
// in tests) — sendOperatorMessage requires one of the two to emit anything.
func (e *Engine) sweepExpiredClaims() {
	if e.opts.WriteClient == nil && e.opts.LocalStore == nil {
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

// sweepExpiredAuctions detects AssignOpen records whose AuctionDeadline has
// passed and that have at least one bid. For each such assign it emits an
// exchange:assign-auction-close message so that state finalizes the Vickrey
// auction and transitions the winner to AssignClaimed.
//
// sweepExpiredAuctions is called on every received message (lazy) and on the
// periodic expiry sweep ticker (backstop). It is a no-op if neither
// WriteClient nor LocalStore is configured.
func (e *Engine) sweepExpiredAuctions() {
	if e.opts.WriteClient == nil && e.opts.LocalStore == nil {
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
	if e.opts.WriteClient == nil && e.opts.LocalStore == nil {
		return // read-only engine or test without write client / local store
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
// CampfireScripStore into the in-memory balance), we must NOT call DecrementBudget
// again or the buyer will be double-charged.
//
// Uses the state matchToBuyHold index for O(1) lookup.
// State.Replay() populates the index by applying all scrip-buy-hold messages
// via applyScripBuyHold — no full log scan needed at query time.
func (e *Engine) findExistingBuyerAcceptHold(matchMsgID string) string {
	return e.state.GetBuyHoldReservation(matchMsgID)
}
