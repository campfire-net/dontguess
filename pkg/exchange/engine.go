package exchange

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"sync/atomic"
	"time"

	"github.com/campfire-net/campfire/pkg/protocol"
	"github.com/campfire-net/campfire/pkg/store"

	"github.com/3dl-dev/dontguess/pkg/matching"
	"github.com/3dl-dev/dontguess/pkg/scrip"
)


// MatchingFeeRate is the fraction of the sale price charged as a matching fee.
// The fee is burned (deflationary). 10% = 1/10.
const MatchingFeeRate = 10

// ResidualRate is the fraction of the sale price paid as residual to the
// original seller. 10% = 1/10.
const ResidualRate = 10

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
	// with callers that have not yet migrated (e.g. EnsureViews, CampfireScripStore).
	// The engine no longer calls Store.ListMessages directly; all reads go via ReadClient.
	Store store.Store
	// ReadClient is the protocol client used to subscribe to and replay campfire messages.
	// If nil, the engine falls back to using Store directly (backward-compat path for tests).
	ReadClient *protocol.Client
	// WriteClient is the protocol client used to send operator-signed messages
	// (match, settle, burn). It must carry the operator's identity and have
	// membership in CampfireID recorded in its backing store.
	WriteClient *protocol.Client
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
	// ProvenanceChecker validates sender provenance levels before processing operations.
	// If nil, provenance checks are skipped (useful for tests that don't exercise provenance).
	ProvenanceChecker *ProvenanceChecker
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
	// ctx is the shutdown context passed to Start. Stored atomically so that
	// handler goroutines can read it without a data race against the Start write.
	// Handlers use this so that in-flight scrip operations are cancelled on
	// graceful shutdown instead of using context.Background() which ignores the
	// shutdown signal.
	ctx atomic.Value // stores context.Context
	// marshalFunc overrides json.Marshal for tests that need to inject marshal failures.
	// Nil means use the standard json.Marshal.
	marshalFunc func(v any) ([]byte, error)
	// matchToReservation maps a match message ID to the scrip reservation ID created
	// at buyer-accept time. The settle(complete) handler uses this to locate the
	// reservation without trusting buyer-supplied payload data.
	matchToReservation map[string]string
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
	return &Engine{
		opts:               opts,
		state:              st,
		matchIndex:         idx,
		matchToReservation: make(map[string]string),
	}
}

// State returns the engine's live state view.
func (e *Engine) State() *State {
	return e.state
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
	if err := e.replayAll(); err != nil {
		return fmt.Errorf("exchange engine replay: %w", err)
	}
	// Dispatch pending unmatched orders that were already in the log at startup.
	if err := e.dispatchPendingOrders(); err != nil {
		e.opts.log("engine: error dispatching pending orders: %v", err)
	}
	return e.run(ctx)
}

// dispatchPendingOrders processes any active buy orders that have not yet been
// matched. Called after replay to handle orders that arrived before the engine started.
func (e *Engine) dispatchPendingOrders() error {
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
	return nil
}

// fetchMessage retrieves a single message by ID. Uses ReadClient.Get when
// ReadClient is configured (preferred), otherwise falls back to Store.GetMessage.
func (e *Engine) fetchMessage(id string) (*Message, error) {
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
	return FromStoreRecord(rec), nil
}

// replayAll loads all historical messages from the campfire and rebuilds state.
// Uses ReadClient.Read with AfterTimestamp=0 to fetch all messages. The SDK
// handles sync-before-query automatically for filesystem transports.
func (e *Engine) replayAll() error {
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

// run is the main event loop. It subscribes to the campfire via
// ReadClient.Subscribe and dispatches messages as they arrive on the channel.
func (e *Engine) run(ctx context.Context) error {
	sub := e.opts.ReadClient.Subscribe(ctx, protocol.SubscribeRequest{
		CampfireID: e.opts.CampfireID,
		Tags: []string{
			TagPut, TagBuy, TagSettle,
			TagAssign, TagAssignClaim, TagAssignComplete, TagAssignAccept, TagAssignReject,
		},
		PollInterval: e.opts.pollInterval(),
	})

	for sdkMsg := range sub.Messages() {
		msg := FromSDKMessage(&sdkMsg)
		e.state.Apply(msg)
		if err := e.dispatch(msg); err != nil {
			e.opts.log("engine: dispatch error (msg=%s): %v", msg.ID, err)
		}
	}

	if err := sub.Err(); err != nil {
		return fmt.Errorf("engine: subscription error: %w", err)
	}
	return ctx.Err()
}

// dispatch routes a new message to the appropriate handler.
func (e *Engine) dispatch(msg *Message) error {
	op := exchangeOp(msg.Tags)

	// Provenance gate: check sender's provenance level if checker is configured.
	if e.opts.ProvenanceChecker != nil {
		var phase SettlePhase
		if op == TagSettle {
			phase = SettlePhase(settlePhaseFromTags(msg.Tags))
		}
		provOp := tagToProvenanceOp(op)
		if provOp != "" {
			if err := e.opts.ProvenanceChecker.Check(msg.Sender, provOp, phase); err != nil {
				e.opts.log("engine: provenance rejected msg=%s op=%s sender=%s: %v",
					msg.ID, op, shortKey(msg.Sender), err)
				return nil // silently reject — don't propagate error to poll loop
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
	}
	return nil
}

// tagToProvenanceOp maps a campfire exchange operation tag to a provenance Operation type.
// Returns "" for unknown/untracked operations (no provenance check needed).
func tagToProvenanceOp(op string) Operation {
	switch op {
	case TagPut:
		return OperationPut
	case TagBuy:
		return OperationBuy
	case TagMatch:
		return OperationMatch
	case TagSettle:
		return OperationSettle
	default:
		return "" // unknown operation — no provenance check
	}
}

// handleBuy responds to an exchange:buy request with an exchange:match message.
//
// The buy message is sent as a campfire future (--future). The engine responds
// with --fulfills <buy-msg-id> to complete the future.
//
// If ScripStore is configured, the buyer's scrip balance is pre-decremented
// by (best_price + fee) before matching. If the buyer has insufficient scrip,
// the buy is rejected with ErrBudgetExceeded and no match is emitted.
func (e *Engine) handleBuy(msg *Message) error {
	// Guard: if this order was already matched (a match message exists in the
	// campfire log for this buy), return immediately. This prevents
	// double-dispatch on engine restart when a buy message is re-applied via
	// poll after the corresponding match was already written to the log during
	// a previous run (fix for dontguess-vd0 / dontguess-bf0).
	if e.state.IsOrderMatched(msg.ID) {
		e.opts.log("engine: handleBuy skipped -- order %s already matched", msg.ID[:8])
		return nil
	}

	var payload struct {
		Task           string   `json:"task"`
		Budget         int64    `json:"budget"`
		MaxPrice       int64    `json:"max_price"`
		MinReputation  int      `json:"min_reputation"`
		FreshnessHours int      `json:"freshness_hours"`
		ContentType    string   `json:"content_type"`
		Domains        []string `json:"domains"`
		MaxResults     int      `json:"max_results"`
	}
	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		return fmt.Errorf("parsing buy payload: %w", err)
	}
	// Accept max_price as alias for budget — agents naturally use this name.
	if payload.Budget == 0 && payload.MaxPrice > 0 {
		payload.Budget = payload.MaxPrice
	}
	// Normalize tag-prefixed enum values from convention dispatch.
	payload.ContentType = stripTagPrefix(payload.ContentType, "exchange:content-type:")
	payload.Domains = stripDomainPrefixes(payload.Domains)

	maxResults := payload.MaxResults
	if maxResults <= 0 {
		maxResults = 3
	}

	// Search inventory for candidates (budget/reputation/freshness/type/domain filters).
	candidates := e.findCandidates(msg.Sender, payload.Budget, payload.MinReputation,
		payload.FreshnessHours, payload.ContentType, payload.Domains)

	// Semantic ranking via the match index.
	// Search returns all candidates ranked by TF-IDF similarity + 4-layer value stack.
	// We pass maxResults*3 to get enough candidates for the final cap after filtering.
	semanticResults := e.matchIndex.Search(payload.Task, maxResults*3)

	// Build a lookup: entryID → semantic result.
	semanticByID := make(map[string]matching.RankedResult, len(semanticResults))
	for _, r := range semanticResults {
		semanticByID[r.EntryID] = r
	}

	// Filter semantic results to those that also passed the hard filters,
	// preserving the semantic ranking order. Entries with no semantic score
	// fall back to the reputation+recency sort via rankResults.
	// partialMatchThreshold mirrors the default from matching.RankOptions.
	// Fallback candidates (no semantic score) use this same threshold so that
	// IsPartialMatch is consistent regardless of which path produced the result.
	const partialMatchThreshold = 0.5

	type rankedCandidate struct {
		entry            *InventoryEntry
		confidence       float64
		isPartialMatch   bool
		hasSemanticScore bool
	}

	var semanticMatches []rankedCandidate
	candidateSet := make(map[string]*InventoryEntry, len(candidates))
	for _, c := range candidates {
		candidateSet[c.EntryID] = c
	}

	for _, sr := range semanticResults {
		entry, ok := candidateSet[sr.EntryID]
		if !ok {
			continue // did not pass hard filters
		}
		semanticMatches = append(semanticMatches, rankedCandidate{
			entry:            entry,
			confidence:       sr.Confidence,
			isPartialMatch:   sr.IsPartialMatch,
			hasSemanticScore: true,
		})
	}

	// Append candidates not covered by the semantic index (e.g. index not yet rebuilt).
	covered := make(map[string]struct{}, len(semanticMatches))
	for _, sm := range semanticMatches {
		covered[sm.entry.EntryID] = struct{}{}
	}
	var fallbackCandidates []*InventoryEntry
	for _, c := range candidates {
		if _, ok := covered[c.EntryID]; !ok {
			fallbackCandidates = append(fallbackCandidates, c)
		}
	}
	ranked := e.rankResults(fallbackCandidates, maxResults)
	for _, entry := range ranked {
		conf := e.computeConfidence(entry, payload.Task)
		semanticMatches = append(semanticMatches, rankedCandidate{
			entry:            entry,
			confidence:       conf,
			isPartialMatch:   conf < partialMatchThreshold,
			hasSemanticScore: false,
		})
	}

	// Cap at maxResults.
	if len(semanticMatches) > maxResults {
		semanticMatches = semanticMatches[:maxResults]
	}

	// Zero-match path: no inventory candidates passed filters or semantic threshold.
	// Send a buy-miss standing offer to the buyer: if they compute the result and
	// put it here, the exchange will auto-accept at token_cost * BuyMissOfferRate%.
	if len(semanticMatches) == 0 {
		return e.handleBuyMiss(msg, payload.Task, payload.Budget)
	}

	// Build match payload.
	type MatchResult struct {
		EntryID           string  `json:"entry_id"`
		PutMsgID          string  `json:"put_msg_id"`
		SellerKey         string  `json:"seller_key"`
		Description       string  `json:"description"`
		ContentHash       string  `json:"content_hash"`
		ContentType       string  `json:"content_type"`
		Price             int64   `json:"price"`
		Confidence        float64 `json:"confidence"`
		IsPartialMatch    bool    `json:"is_partial_match"`
		SellerReputation  int     `json:"seller_reputation"`
		TokenCostOriginal int64   `json:"token_cost_original"`
		AgeHours          int     `json:"age_hours"`
	}

	matchResults := make([]MatchResult, len(semanticMatches))
	for i, rc := range semanticMatches {
		entry := rc.entry
		ageHours := int(time.Since(time.Unix(0, entry.PutTimestamp)).Hours())
		rep := e.state.SellerReputation(entry.SellerKey)
		matchResults[i] = MatchResult{
			EntryID:           entry.EntryID,
			PutMsgID:          entry.PutMsgID,
			SellerKey:         entry.SellerKey,
			Description:       entry.Description,
			ContentHash:       entry.ContentHash,
			ContentType:       entry.ContentType,
			Price:             e.computePrice(entry),
			Confidence:        rc.confidence,
			IsPartialMatch:    rc.isPartialMatch,
			SellerReputation:  rep,
			TokenCostOriginal: entry.TokenCost,
			AgeHours:          ageHours,
		}
	}

	meta := map[string]any{"total_candidates": len(candidates)}

	matchPayload, err := e.marshal(map[string]any{
		"results":     matchResults,
		"search_meta": meta,
	})
	if err != nil {
		return fmt.Errorf("encoding match payload: %w", err)
	}

	tags := []string{TagMatch}
	// Antecedent is the buy message; --fulfills semantics use the antecedent.
	antecedents := []string{msg.ID}

	matchRec, err := e.sendOperatorMessage(matchPayload, tags, antecedents)
	if err != nil {
		return err
	}

	// Apply the match message to state immediately so downstream handlers (e.g.
	// buyer-accept) can reference it via matchToBuyer / matchToEntry without
	// requiring an explicit Replay call. This matters in the engine's poll() loop:
	// poll() only subscribes to TagPut/TagBuy/TagSettle, so match messages emitted
	// here would not otherwise be Applied until the next full replayAll().
	if matchRec != nil {
		e.state.Apply(matchRec)
	}

	// Warm compression offer: if the top-matched entry has no compressed
	// derivative, offer the buyer an exclusive compression assign. The buyer
	// just consumed the raw content and has it in cache — they are the ideal
	// compressor. Bounty is 30% of the entry's token_cost. Failure is
	// non-fatal; log and proceed.
	topEntry := semanticMatches[0].entry
	if !e.state.HasCompressedVersion(topEntry.EntryID) && !e.hasActiveBuyerCompressAssign(topEntry.EntryID, msg.Sender) {
		if err := e.sendWarmCompressionAssign(topEntry, msg.Sender); err != nil {
			e.opts.log("engine: warm compression assign failed entry=%s err=%v", topEntry.PutMsgID, err)
		}
	}

	return nil
}

// handleBuyMiss handles the zero-match path for a buy request.
//
// It records a standing buy-miss offer in state and emits an exchange:buy-miss
// message back to the campfire (fulfilling the buy future) with:
//   - offered_price_rate: BuyMissOfferRate (70% of token_cost)
//   - task_hash: SHA-256 of the task description
//   - expires_at: now + BuyMissOfferTTL
//
// One offer per task hash — if a non-expired offer already exists for this task
// the engine still sends the buy-miss response (idempotent from buyer's view)
// but does not create a duplicate offer in state.
func (e *Engine) handleBuyMiss(msg *Message, task string, budget int64) error {
	taskHash := TaskDescriptionHash(task)
	expiresAt := time.Now().Add(BuyMissOfferTTL)

	offer := &BuyMissOffer{
		TaskHash:  taskHash,
		BuyMsgID:  msg.ID,
		BuyerKey:  msg.Sender,
		Task:      task,
		ExpiresAt: expiresAt,
	}
	e.state.SetBuyMissOffer(offer)

	buyMissPayload, err := e.marshal(map[string]any{
		"task_hash":          taskHash,
		"task":               task,
		"offered_price_rate": BuyMissOfferRate,
		"expires_at":         expiresAt.UTC().Format(time.RFC3339),
		"buy_msg_id":         msg.ID,
	})
	if err != nil {
		return fmt.Errorf("encoding buy-miss payload: %w", err)
	}

	tags := []string{TagBuyMiss, TagMatch}
	antecedents := []string{msg.ID}

	rec, err := e.sendOperatorMessage(buyMissPayload, tags, antecedents)
	if err != nil {
		return err
	}
	if rec != nil {
		e.state.Apply(rec)
	}

	e.opts.log("engine: buy-miss: order=%s task_hash=%s expires=%s",
		msg.ID[:8], taskHash[:16], expiresAt.Format(time.RFC3339))
	return nil
}

// handlePut processes an incoming exchange:put message.
//
// If a non-expired buy-miss standing offer exists for the put's task description
// (matched by SHA-256 hash), the engine auto-accepts the put at the offered price
// (token_cost * BuyMissOfferRate / 100) and pays the seller scrip immediately.
//
// If no standing offer matches, the put is left pending for normal operator review
// via AutoAcceptPut.
func (e *Engine) handlePut(msg *Message) error {
	// Determine the description from state (applyPut already validated and stored it).
	e.state.mu.RLock()
	pending, ok := e.state.pendingPuts[msg.ID]
	e.state.mu.RUnlock()
	if !ok {
		// Put was invalid (e.g. oversized description) — nothing to do.
		return nil
	}

	// Reject puts with invalid content_hash format (must start with "sha256:").
	if !strings.HasPrefix(pending.ContentHash, "sha256:") {
		return fmt.Errorf("buy-miss put rejected: content_hash %q does not start with sha256:", pending.ContentHash)
	}

	taskHash := TaskDescriptionHash(pending.Description)

	// Only the buyer who received the miss offer may fulfill it.
	// Peek first (read-only) to avoid consuming the offer if sender doesn't match.
	peeked := e.state.GetBuyMissOffer(taskHash)
	if peeked == nil {
		// No standing offer — leave pending for normal operator review.
		return nil
	}
	if msg.Sender != peeked.BuyerKey {
		return nil // reject: only the original buyer can fulfill their own miss offer
	}

	// Sender matches — now atomically claim (get+delete) to prevent TOCTOU
	// double-accept by two concurrent puts from the same buyer.
	offer := e.state.ClaimBuyMissOffer(taskHash)
	if offer == nil {
		// Lost the race — another concurrent put from the same buyer already claimed it.
		return nil
	}
	// TOCTOU guard: the offer may have been replaced by a new buyer's standing offer
	// between the peek check above and this atomic claim. Re-validate sender against
	// the claimed offer (not the peeked one) to prevent consuming the wrong buyer's offer.
	if offer.BuyerKey != msg.Sender {
		e.state.SetBuyMissOffer(offer) // restore the rightful buyer's offer
		return nil
	}

	// Cap token_cost to prevent inflated scrip payouts from untrusted seller input.
	tokenCost := pending.TokenCost
	maxTokenCost := e.opts.maxTokenCost()
	if tokenCost > maxTokenCost {
		tokenCost = maxTokenCost
	}

	// Standing offer found and not expired. Compute the offered price.
	offeredPrice := tokenCost * BuyMissOfferRate / 100
	if offeredPrice <= 0 {
		offeredPrice = 1
	}

	// Auto-accept at offered price (no expiry set on inventory entry — operator
	// can expire later; consistent with normal AutoAcceptPut expiry handling).
	expiresAt := time.Time{}
	var expiresAtStr string
	if !expiresAt.IsZero() {
		expiresAtStr = expiresAt.UTC().Format(time.RFC3339)
	}

	putAcceptPayload, err := e.marshal(map[string]any{
		"phase":      SettlePhaseStrPutAccept,
		"entry_id":   msg.ID,
		"price":      offeredPrice,
		"expires_at": expiresAtStr,
	})
	if err != nil {
		return fmt.Errorf("encoding buy-miss put-accept payload: %w", err)
	}

	tags := []string{
		TagSettle,
		TagPhasePrefix + SettlePhaseStrPutAccept,
		TagVerdictPrefix + "accepted",
		TagBuyMiss, // mark as buy-miss fulfillment
	}
	antecedents := []string{msg.ID}

	rec, err := e.sendOperatorMessage(putAcceptPayload, tags, antecedents)
	if err != nil {
		return err
	}
	if rec != nil {
		e.state.Apply(rec)
	}

	// Standing offer already consumed by ClaimBuyMissOffer above (atomic get+delete).

	// Pay the seller scrip immediately (same as scrip-put-pay in normal put-accept flow).
	if e.opts.ScripStore != nil {
		ctx := e.engineCtx()
		if _, _, err := e.opts.ScripStore.AddBudget(ctx, pending.SellerKey, scrip.BalanceKey, offeredPrice, ""); err != nil {
			e.opts.log("engine: buy-miss put-accept: AddBudget for seller %s: %v", shortKey(pending.SellerKey), err)
		}
		// Emit scrip-put-pay so CampfireScripStore can replay the payment.
		payPayload, marshalErr := e.marshal(scrip.PutPayPayload{
			Seller:      pending.SellerKey,
			Amount:      offeredPrice,
			TokenCost:   pending.TokenCost,
			DiscountPct: 100 - BuyMissOfferRate,
			ResultHash:  pending.ContentHash,
			PutMsg:      msg.ID,
		})
		if marshalErr == nil {
			if _, emitErr := e.sendOperatorMessage(payPayload,
				[]string{scrip.TagScripPutPay}, []string{msg.ID}); emitErr != nil {
				e.opts.log("engine: buy-miss put-accept: emit scrip-put-pay: %v", emitErr)
			}
		}
	}

	// Incrementally add the new entry to the match index.
	var acceptedEntry *InventoryEntry
	inv := e.state.Inventory()
	for _, entry := range inv {
		if entry.PutMsgID == msg.ID {
			e.matchIndex.Add(e.inventoryEntryToRankInput(entry))
			acceptedEntry = entry
			break
		}
	}

	// Hot compression offer: same as AutoAcceptPut — immediately assign a compress
	// task to the seller at 50% of token_cost. Failure is non-fatal.
	if acceptedEntry != nil && acceptedEntry.SellerKey != "" {
		if err := e.sendCompressionAssign(acceptedEntry); err != nil {
			e.opts.log("engine: buy-miss: compression assign failed entry=%s err=%v", msg.ID[:8], err)
		}
	}

	e.opts.log("engine: buy-miss fulfilled: put=%s seller=%s price=%d offer_task_hash=%s",
		msg.ID[:8], shortKey(pending.SellerKey), offeredPrice, taskHash[:16])
	return nil
}

// handleSettle processes settlement messages.
//
// For settle(buyer-accept) phases, if ScripStore is configured, the engine:
//   - Checks the buyer's scrip balance (fails if insufficient)
//   - Pre-decrements the buyer's balance by (price + fee)
//   - Stores the reservation ID in matchToReservation for the complete handler
//
// For settle(complete) phases, if ScripStore is configured, the engine:
//   - Pays the seller their residual (price * ResidualRate / 100)
//   - Burns the matching fee (price * MatchingFeeRate / 100)
//   - Credits exchange revenue (remainder) to the operator
//
// For settle(preview-request) phases, the engine generates a content preview
// using PreviewAssembler and responds with a settle(preview) message. The
// preview antecedent is the preview-request message ID.
func (e *Engine) handleSettle(msg *Message) error {
	phase := settlePhaseFromTags(msg.Tags)

	// Handle preview-request: generate and send a preview response.
	if phase == SettlePhaseStrPreviewRequest {
		return e.handleSettlePreviewRequest(msg)
	}

	// Handle small-content-dispute: fully automated refund path, no operator required.
	if phase == SettlePhaseStrSmallContentDispute {
		return e.handleSettleSmallContentDispute(msg)
	}

	if e.opts.ScripStore == nil {
		return nil
	}

	// Handle buyer-accept: scrip hold happens here (not at buy time).
	// This is the "preview-before-purchase" model — scrip is only locked when
	// the buyer has seen the preview and decided to proceed.
	if phase == SettlePhaseStrBuyerAccept {
		return e.handleSettleBuyerAcceptScrip(msg)
	}

	if phase != SettlePhaseStrComplete {
		// Other phases (deliver, put-accept) are tracked in state only.
		return nil
	}

	// Derive seller from the antecedent chain: complete → deliver → match → entry → seller.
	// msg.Antecedents[0] is the deliver message ID (the complete message references deliver).
	if len(msg.Antecedents) == 0 {
		e.opts.log("engine: settle: complete message has no antecedents — cannot derive seller")
		return nil
	}
	deliverMsgID := msg.Antecedents[0]
	sellerKey, ok := e.state.SellerKeyForDeliver(deliverMsgID)
	if !ok {
		e.opts.log("engine: settle: cannot derive seller for deliver=%s — antecedent chain broken", shortKey(deliverMsgID))
		return nil
	}

	// Derive match message ID from the deliver antecedent chain to look up the
	// reservation created at buyer-accept time.
	matchMsgID, ok := e.state.MatchForDeliver(deliverMsgID)
	if !ok {
		e.opts.log("engine: settle: cannot derive match for deliver=%s — antecedent chain broken", shortKey(deliverMsgID))
		return nil
	}

	// Look up the reservation created at buyer-accept time (not from buyer payload).
	// This is secure: the reservation ID is engine-owned, not buyer-supplied.
	reservationID, hasReservation := e.matchToReservation[matchMsgID]
	if !hasReservation || reservationID == "" {
		e.opts.log("engine: settle: no reservation found for match=%s — buyer-accept scrip hold may not have run", shortKey(matchMsgID))
		return nil // no scrip-bearing buyer-accept — skip scrip settlement
	}

	ctx := e.engineCtx()

	// Atomically retrieve and delete reservation (prevents TOCTOU double-spend).
	res, err := e.opts.ScripStore.ConsumeReservation(ctx, reservationID)
	if err != nil {
		e.opts.log("engine: settle: reservation %s not found: %v", shortKey(reservationID), err)
		return nil // reservation missing — already settled or expired
	}

	// Derive price from the reservation amount: res.Amount = price + price/MatchingFeeRate.
	// We do NOT trust price from the buyer-controlled complete payload.
	// The price was locked at buyer-accept time; use it directly from the reservation.
	// The fee is res.Amount - price, i.e., price/MatchingFeeRate.
	// Recover price: res.Amount = price * (1 + 1/MatchingFeeRate) = price * (MatchingFeeRate+1)/MatchingFeeRate
	// => price = res.Amount * MatchingFeeRate / (MatchingFeeRate+1)
	price := res.Amount * MatchingFeeRate / (MatchingFeeRate + 1)
	fee := price / MatchingFeeRate
	residual := price / ResidualRate
	exchangeRevenue := price - residual // fee already came out of the buyer's pre-decrement

	operatorKey := e.state.OperatorKey

	// Marshal both convention messages BEFORE mutating scrip state.
	// If either marshal fails, restore the reservation (it was consumed above) and return
	// the error — no balance mutations have occurred.
	settlePayload, err := e.marshal(scrip.SettlePayload{
		ReservationID:   reservationID,
		Seller:          sellerKey,
		Residual:        residual,
		FeeBurned:       fee,
		ExchangeRevenue: exchangeRevenue,
		MatchMsg:        msg.ID,
		ResultHash:      "",
	})
	if err != nil {
		if restoreErr := e.opts.ScripStore.SaveReservation(ctx, res); restoreErr != nil {
			e.opts.log("engine: settle: CRITICAL: failed to restore reservation %s after marshal failure: %v",
				shortKey(reservationID), restoreErr)
			return fmt.Errorf("scrip: settle reservation %s: marshal failed AND restore failed (reservation lost): %w",
				shortKey(reservationID), err)
		}
		return fmt.Errorf("scrip: marshal settle payload: %w", err)
	}

	var burnPayload []byte
	if fee > 0 {
		burnPayload, err = e.marshal(scrip.BurnPayload{
			Amount:    fee,
			Reason:    "matching-fee",
			SourceMsg: msg.ID,
		})
		if err != nil {
			if restoreErr := e.opts.ScripStore.SaveReservation(ctx, res); restoreErr != nil {
				e.opts.log("engine: settle: CRITICAL: failed to restore reservation %s after marshal failure: %v",
					shortKey(reservationID), restoreErr)
				return fmt.Errorf("scrip: settle reservation %s: marshal failed AND restore failed (reservation lost): %w",
					shortKey(reservationID), err)
			}
			return fmt.Errorf("scrip: marshal burn payload: %w", err)
		}
	}

	// Credit residual to seller.
	if residual > 0 {
		if _, _, err := e.opts.ScripStore.AddBudget(ctx, sellerKey, scrip.BalanceKey, residual, ""); err != nil {
			e.opts.log("engine: settle: add residual to seller %s: %v", shortKey(sellerKey), err)
		}
	}

	// Credit exchange revenue to operator.
	if exchangeRevenue > 0 {
		if _, _, err := e.opts.ScripStore.AddBudget(ctx, operatorKey, scrip.BalanceKey, exchangeRevenue, ""); err != nil {
			e.opts.log("engine: settle: add exchange revenue to operator: %v", err)
		}
	}

	// Emit scrip-settle convention message so CampfireScripStore can replay it.
	if _, emitErr := e.sendOperatorMessage(settlePayload,
		[]string{scrip.TagScripSettle}, []string{msg.ID}); emitErr != nil {
		e.opts.log("engine: warning: emit scrip-settle: %v", emitErr)
	}

	// Emit scrip-burn for the matching fee (already removed from buyer's balance via buyer-accept hold).
	if len(burnPayload) > 0 {
		if _, emitErr := e.sendOperatorMessage(burnPayload,
			[]string{scrip.TagScripBurn}, []string{msg.ID}); emitErr != nil {
			e.opts.log("engine: warning: emit scrip-burn: %v", emitErr)
		}
	}

	// Clean up engine-side mapping now that the reservation is consumed.
	delete(e.matchToReservation, matchMsgID)

	e.opts.log("engine: settle: reservation=%s seller=%s price=%d residual=%d fee_burned=%d exchange=%d",
		shortKey(reservationID), shortKey(sellerKey), price, residual, fee, exchangeRevenue)
	return nil
}

// handleSettleBuyerAcceptScrip performs the scrip hold when a buyer sends a
// settle(buyer-accept) message. This is the "preview-before-purchase" model:
// scrip is locked when the buyer has reviewed the preview and decided to proceed,
// not at buy time.
//
// On success:
//   - Buyer's balance is decremented by (price + fee)
//   - A reservation is saved in ScripStore
//   - The reservation ID is stored in matchToReservation[matchMsgID]
//   - A scrip-buy-hold convention message is emitted for CampfireScripStore replay
//
// The match message ID is resolved from the antecedent chain:
//
//	buyer-accept → preview (optional) → match
//
// This mirrors the antecedent resolution in state.applySettleBuyerAccept.
func (e *Engine) handleSettleBuyerAcceptScrip(msg *Message) error {
	if len(msg.Antecedents) == 0 {
		e.opts.log("engine: buyer-accept scrip: no antecedents, ignoring msg=%s", shortKey(msg.ID))
		return nil
	}
	antecedentID := msg.Antecedents[0]

	// Resolve the match message ID from the antecedent.
	// Try preview path first (antecedent is a preview message).
	e.state.mu.RLock()
	matchMsgID, hasMatch := e.state.previewToMatch[antecedentID]
	if !hasMatch {
		// Legacy/small-content path: antecedent is the match message directly.
		matchMsgID = antecedentID
		// Validate that this is a known match (security: reject spoofed buyer-accept).
		_, hasMatch = e.state.matchToBuyer[matchMsgID]
	}
	expectedBuyer, _ := e.state.matchToBuyer[matchMsgID]
	e.state.mu.RUnlock()

	if !hasMatch {
		e.opts.log("engine: buyer-accept scrip: unknown match %s, ignoring", shortKey(matchMsgID))
		return nil
	}

	// Enforce buyer identity: only the original buyer may trigger a scrip hold.
	if msg.Sender != expectedBuyer {
		e.opts.log("engine: buyer-accept scrip: sender %s is not buyer for match %s, ignoring",
			shortKey(msg.Sender), shortKey(matchMsgID))
		return nil
	}

	// Idempotency: if a hold already exists for this match, skip.
	// This handles the restart-with-pending-orders scenario: the buyer-accept-hold
	// message was written to the campfire log on a previous run and CampfireScripStore
	// has already replayed it. Re-running DecrementBudget would double-charge the buyer.
	if existingResID := e.findExistingBuyerAcceptHold(matchMsgID); existingResID != "" {
		// Restore in-memory reservation so complete/dispute handlers can reference it.
		ctx := e.engineCtx()
		_, currentETag, _ := e.opts.ScripStore.GetBudget(ctx, msg.Sender, scrip.BalanceKey)
		// Look up the entry price to reconstruct holdAmount.
		e.state.mu.RLock()
		entryID := e.state.matchToEntry[matchMsgID]
		e.state.mu.RUnlock()
		entry := e.state.GetInventoryEntry(entryID)
		var holdAmount int64
		if entry != nil {
			p := e.computePrice(entry)
			holdAmount = p + p/MatchingFeeRate
		}
		res := scrip.Reservation{
			ID:        existingResID,
			AgentKey:  msg.Sender,
			RK:        scrip.BalanceKey,
			ETag:      currentETag,
			Amount:    holdAmount,
			CreatedAt: time.Now(),
		}
		if err := e.opts.ScripStore.SaveReservation(ctx, res); err != nil {
			e.opts.log("engine: buyer-accept scrip: warning: re-save reservation after restart %s: %v",
				shortKey(existingResID), err)
		}
		e.matchToReservation[matchMsgID] = existingResID
		e.opts.log("engine: buyer-accept scrip: hold already replayed, skipping pre-decrement buyer=%s reservation=%s",
			shortKey(msg.Sender), shortKey(existingResID))
		return nil
	}

	// Determine the price for the entry offered in this match.
	e.state.mu.RLock()
	entryID := e.state.matchToEntry[matchMsgID]
	e.state.mu.RUnlock()
	entry := e.state.GetInventoryEntry(entryID)
	if entry == nil {
		e.opts.log("engine: buyer-accept scrip: entry %s not found for match %s, ignoring",
			shortKey(entryID), shortKey(matchMsgID))
		return nil
	}

	bestPrice := e.computePrice(entry)
	fee := bestPrice / MatchingFeeRate
	holdAmount := bestPrice + fee

	ctx := e.engineCtx()
	buyerKey := msg.Sender

	bal, etag, err := e.opts.ScripStore.GetBudget(ctx, buyerKey, scrip.BalanceKey)
	if err != nil {
		return fmt.Errorf("scrip: buyer-accept: GetBudget for buyer %s: %w", shortKey(buyerKey), err)
	}
	if bal < holdAmount {
		return fmt.Errorf("scrip: buyer-accept: buyer %s: %w (balance=%d, required=%d)",
			shortKey(buyerKey), scrip.ErrBudgetExceeded, bal, holdAmount)
	}

	// Marshal the buy-hold convention message BEFORE mutating scrip state.
	reservationID := newReservationID()
	expiresAt := time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339)
	holdPayload, err := e.marshal(scrip.BuyHoldPayload{
		Buyer:         buyerKey,
		Amount:        holdAmount,
		Price:         bestPrice,
		Fee:           fee,
		ReservationID: reservationID,
		BuyMsg:        matchMsgID, // references the match message (historical field name)
		ExpiresAt:     expiresAt,
	})
	if err != nil {
		return fmt.Errorf("scrip: marshal buyer-accept buy-hold payload: %w", err)
	}

	_, newETag, err := e.opts.ScripStore.DecrementBudget(ctx, buyerKey, scrip.BalanceKey, holdAmount, etag)
	if err != nil {
		return fmt.Errorf("scrip: buyer-accept: DecrementBudget for buyer %s: %w", shortKey(buyerKey), err)
	}

	// Save reservation so settle(complete) and dispute handlers can reference it.
	res := scrip.Reservation{
		ID:        reservationID,
		AgentKey:  buyerKey,
		RK:        scrip.BalanceKey,
		ETag:      newETag,
		Amount:    holdAmount,
		CreatedAt: time.Now(),
	}
	if err := e.opts.ScripStore.SaveReservation(ctx, res); err != nil {
		return fmt.Errorf("scrip: buyer-accept: SaveReservation: %w", err)
	}

	// Record the reservation so the complete handler can find it.
	e.matchToReservation[matchMsgID] = reservationID

	e.opts.log("engine: buyer-accept scrip: pre-decremented buyer=%s hold=%d reservation=%s match=%s",
		shortKey(buyerKey), holdAmount, shortKey(reservationID), shortKey(matchMsgID))

	// Emit scrip-buy-hold convention message so CampfireScripStore can replay it.
	// The BuyMsg field references the match message ID (the antecedent resolution anchor).
	if _, emitErr := e.sendOperatorMessage(holdPayload,
		[]string{scrip.TagScripBuyHold}, []string{msg.ID}); emitErr != nil {
		e.opts.log("engine: warning: emit scrip-buy-hold (buyer-accept): %v", emitErr)
	}

	return nil
}

// handleSettlePreviewRequest generates a content preview in response to a
// settle(preview-request) message from a buyer.
//
// The engine:
//  1. Validates the match exists in state (antecedent must be a match message).
//  2. Looks up the entry from the match.
//  3. Calls PreviewAssembler.Assemble() with the entry details and nil content
//     (actual content delivery is a future concern — the preview payload carries
//     entry metadata and chunk descriptors, not the real content bytes).
//  4. Sends a settle(preview) response with the antecedent set to the
//     preview-request message ID.
//
// If the antecedent is not a recognized match or the entry is not in inventory,
// the message is silently ignored (no error returned to the poll loop).
func (e *Engine) handleSettlePreviewRequest(msg *Message) error {
	if len(msg.Antecedents) == 0 {
		e.opts.log("engine: preview-request: no antecedents, ignoring msg=%s", msg.ID)
		return nil
	}
	matchMsgID := msg.Antecedents[0]

	// Validate match exists and sender is the expected buyer.
	e.state.mu.RLock()
	expectedBuyer, matchKnown := e.state.matchToBuyer[matchMsgID]
	matchEntryID := e.state.matchToEntry[matchMsgID]
	// Also confirm that state applied the preview-request (previewRequestToMatch populated).
	_, previewTracked := e.state.previewRequestToMatch[msg.ID]
	e.state.mu.RUnlock()

	if !matchKnown {
		e.opts.log("engine: preview-request: unknown match %s, ignoring", shortKey(matchMsgID))
		return nil
	}
	if msg.Sender != expectedBuyer {
		e.opts.log("engine: preview-request: sender %s is not the expected buyer for match %s, ignoring",
			shortKey(msg.Sender), shortKey(matchMsgID))
		return nil
	}
	if !previewTracked {
		// State rejected the preview-request (invalid antecedent or wrong sender).
		// Do not respond.
		e.opts.log("engine: preview-request: state did not track msg=%s, ignoring", msg.ID)
		return nil
	}

	// Look up the entry.
	entry := e.state.GetInventoryEntry(matchEntryID)
	if entry == nil {
		e.opts.log("engine: preview-request: entry %s not in inventory, ignoring", shortKey(matchEntryID))
		return nil
	}

	// Generate preview. Content delivery is a future concern; use nil content for now.
	// PreviewAssembler returns an empty result for nil/zero content, which is correct
	// for the preview-before-purchase flow at this stage — the preview payload
	// carries entry metadata so the buyer can decide whether to proceed.
	pa := &PreviewAssembler{}
	previewResult, err := pa.Assemble(PreviewRequest{
		Content:     nil, // stub: real content delivery is a future concern
		ContentType: entry.ContentType,
		EntryID:     entry.EntryID,
		// Seed is entry_id only — all buyers see the same deterministic preview.
	})
	if err != nil {
		return fmt.Errorf("engine: preview-request: assemble preview for entry %s: %w", shortKey(entry.EntryID), err)
	}

	// Build preview payload.
	type ChunkPayload struct {
		Content    string `json:"content"`
		StartByte  int    `json:"start_byte"`
		EndByte    int    `json:"end_byte"`
		ChunkIndex int    `json:"chunk_index"`
	}
	chunks := make([]ChunkPayload, len(previewResult.Chunks))
	for i, c := range previewResult.Chunks {
		chunks[i] = ChunkPayload{
			Content:    c.Content,
			StartByte:  c.StartByte,
			EndByte:    c.EndByte,
			ChunkIndex: c.ChunkIndex,
		}
	}

	previewPayload, err := e.marshal(map[string]any{
		"entry_id":       entry.EntryID,
		"content_type":   entry.ContentType,
		"total_tokens":   previewResult.TotalTokens,
		"preview_tokens": previewResult.PreviewTokens,
		"chunks":         chunks,
	})
	if err != nil {
		return fmt.Errorf("engine: preview-request: marshal preview payload: %w", err)
	}

	tags := []string{
		TagSettle,
		TagPhasePrefix + SettlePhaseStrPreview,
	}
	// Antecedent of the preview response is the preview-request message.
	antecedents := []string{msg.ID}

	_, err = e.sendOperatorMessage(previewPayload, tags, antecedents)
	if err != nil {
		return fmt.Errorf("engine: preview-request: send preview response: %w", err)
	}

	e.opts.log("engine: preview-request: sent preview for entry=%s match=%s buyer=%s",
		shortKey(entry.EntryID), shortKey(matchMsgID), shortKey(msg.Sender))
	return nil
}

// handleSettleSmallContentDispute processes a settle(small-content-dispute) message.
//
// This is a fully automated refund path — no operator verdict required. When
// content is below SmallContentThreshold tokens, previews are not meaningful,
// so buyers receive an immediate auto-refund of their held scrip.
//
// If ScripStore is configured, the buyer's reservation is consumed and the
// full held amount is returned to their balance. State tracking (reputation
// penalty) is handled by applySettleSmallContentDispute in state.go.
func (e *Engine) handleSettleSmallContentDispute(msg *Message) error {
	if e.opts.ScripStore == nil {
		return nil
	}

	var payload struct {
		ReservationID string `json:"reservation_id"`
		BuyerKey      string `json:"buyer_key"`
	}
	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		return fmt.Errorf("scrip: parsing small-content-dispute payload: %w", err)
	}
	if payload.ReservationID == "" || payload.BuyerKey == "" {
		return nil // no scrip involved — state-only tracking already done
	}

	// Verify the entry is actually small content. Derive entry from antecedent chain.
	if len(msg.Antecedents) == 0 {
		return nil
	}
	deliverMsgID := msg.Antecedents[0]
	entry := e.entryForDeliver(deliverMsgID)
	if entry != nil {
		isSmall := entry.TokenCost < SmallContentThreshold ||
			entry.ContentSize < int64(SmallContentThreshold)*4
		if !isSmall {
			e.opts.log("engine: small-content-dispute: entry %s is not small content (token_cost=%d, content_size=%d) — rejecting refund",
				shortKey(entry.EntryID), entry.TokenCost, entry.ContentSize)
			return nil
		}
	}

	ctx := e.engineCtx()

	// Atomically retrieve and delete reservation (prevents TOCTOU double-spend).
	res, err := e.opts.ScripStore.ConsumeReservation(ctx, payload.ReservationID)
	if err != nil {
		e.opts.log("engine: small-content-dispute: reservation %s not found: %v",
			shortKey(payload.ReservationID), err)
		return nil // reservation missing or already settled
	}

	// Security check: buyer_key in payload must match the agent key in the reservation.
	if res.AgentKey != payload.BuyerKey {
		if restoreErr := e.opts.ScripStore.SaveReservation(ctx, res); restoreErr != nil {
			e.opts.log("engine: small-content-dispute: CRITICAL: failed to restore reservation %s after key mismatch: %v",
				shortKey(payload.ReservationID), restoreErr)
			return fmt.Errorf("scrip: small-content-dispute reservation %s: buyer_key mismatch AND restore failed (reservation lost): %w",
				shortKey(payload.ReservationID), restoreErr)
		}
		return fmt.Errorf("scrip: small-content-dispute reservation %s: buyer_key mismatch (payload=%s, reservation=%s)",
			shortKey(payload.ReservationID), shortKey(payload.BuyerKey), shortKey(res.AgentKey))
	}

	// Marshal the convention refund message BEFORE mutating scrip state.
	refundPayload, err := e.marshal(scrip.DisputeRefundPayload{
		Buyer:         res.AgentKey,
		Amount:        res.Amount,
		ReservationID: payload.ReservationID,
		DisputeMsg:    msg.ID,
	})
	if err != nil {
		if restoreErr := e.opts.ScripStore.SaveReservation(ctx, res); restoreErr != nil {
			e.opts.log("engine: small-content-dispute: CRITICAL: failed to restore reservation %s after marshal failure: %v",
				shortKey(payload.ReservationID), restoreErr)
			return fmt.Errorf("scrip: small-content-dispute reservation %s: marshal failed AND restore failed (reservation lost): %w",
				shortKey(payload.ReservationID), restoreErr)
		}
		return fmt.Errorf("scrip: marshal small-content-dispute refund payload: %w", err)
	}

	// Refund the full held amount to the buyer.
	if _, _, err := e.opts.ScripStore.AddBudget(ctx, res.AgentKey, scrip.BalanceKey, res.Amount, ""); err != nil {
		return fmt.Errorf("scrip: small-content-dispute refund for buyer %s: %w", shortKey(res.AgentKey), err)
	}

	// Emit scrip-dispute-refund convention message so CampfireScripStore can replay it.
	if _, emitErr := e.sendOperatorMessage(refundPayload,
		[]string{scrip.TagScripDisputeRefund}, []string{msg.ID}); emitErr != nil {
		e.opts.log("engine: warning: emit scrip-dispute-refund (small-content): %v", emitErr)
	}

	e.opts.log("engine: small-content-dispute refund: reservation=%s buyer=%s amount=%d",
		shortKey(payload.ReservationID), shortKey(res.AgentKey), res.Amount)
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

	if rec.Reward <= 0 {
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
		if _, _, err := e.opts.ScripStore.AddBudget(ctx, rec.ClaimantKey, scrip.BalanceKey, rec.Reward, ""); err != nil {
			e.opts.log("engine: assign-accept: add bounty to worker %s: %v", shortKey(rec.ClaimantKey), err)
			return fmt.Errorf("assign-accept: pay bounty: %w", err)
		}

		// Emit scrip-assign-pay so CampfireScripStore can replay the payment.
		payPayload, err := e.marshal(scrip.AssignPayPayload{
			Worker:    rec.ClaimantKey,
			Amount:    rec.Reward,
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
		shortKey(rec.AssignID), shortKey(rec.ClaimantKey), rec.Reward)
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
		return fmt.Errorf("assign-complete result content_hash %q does not start with sha256:", result.ContentHash)
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
		// provenance checking is done at put-accept time for primary entries.
	}

	// Insert into state inventory (holds state mutex internally).
	e.state.mu.Lock()
	e.state.applyDerivativePut(derivative)
	e.state.mu.Unlock()

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

// entryForDeliver derives the inventory entry for a deliver message ID by tracing
// the antecedent chain: deliver → match (deliverToMatch) → entry (matchToEntry).
// Returns nil if any link in the chain is missing or the entry is not in inventory.
func (e *Engine) entryForDeliver(deliverMsgID string) *InventoryEntry {
	entry, ok := e.state.EntryForDeliver(deliverMsgID)
	if !ok {
		return nil
	}
	return entry
}

// findCandidates returns inventory entries that satisfy the buyer's filters.
func (e *Engine) findCandidates(buyerKey string, budget int64, minRep int,
	freshnessHours int, contentType string, domains []string) []*InventoryEntry {

	// Layer 0 correctness gate: exclude entries with poor preview-to-purchase
	// conversion rate (dontguess-5iz). Only applied once per call and keyed by
	// entry ID for O(1) lookup in the loop below. Reversible: if an entry's
	// conversion rate improves above the threshold, it re-appears automatically
	// on the next call since LowConversionEntries is computed fresh from state.
	lowConv := e.state.LowConversionEntries(Layer0MinPreviews, Layer0MaxConversionRate)
	excluded := make(map[string]struct{}, len(lowConv))
	for _, id := range lowConv {
		excluded[id] = struct{}{}
	}

	inventory := e.state.Inventory()
	var out []*InventoryEntry

	for _, entry := range inventory {
		// Layer 0: exclude entries with low conversion rate (insufficient buyer demand).
		if _, ok := excluded[entry.EntryID]; ok {
			continue
		}

		// Provenance revalidation gate: exclude entries flagged for re-validation
		// due to a seller provenance downgrade (dontguess-lqp). These entries remain
		// in inventory but are withheld from buyers until the operator clears the flag.
		if entry.NeedsRevalidation {
			continue
		}

		// Budget filter: price must not exceed budget.
		price := e.computePrice(entry)
		if price > budget {
			continue
		}

		// Reputation filter.
		rep := e.state.SellerReputation(entry.SellerKey)
		if rep < minRep {
			continue
		}

		// Freshness filter.
		if freshnessHours > 0 {
			ageHours := int(time.Since(time.Unix(0, entry.PutTimestamp)).Hours())
			if ageHours > freshnessHours {
				continue
			}
		}

		// Content type filter.
		if contentType != "" && entry.ContentType != contentType {
			continue
		}

		// Domain filter: entry must have at least one matching domain.
		if len(domains) > 0 && !hasOverlap(entry.Domains, domains) {
			continue
		}

		out = append(out, entry)
	}
	return out
}

// rankResults scores and sorts candidates, returning the top maxResults.
// Uses a simple composite: seller reputation * 0.6 + recency * 0.4
// The full 4-layer value stack is a pricing-engine concern; here we use a
// lightweight proxy appropriate for the exchange engine's role.
func (e *Engine) rankResults(candidates []*InventoryEntry, max int) []*InventoryEntry {
	type scored struct {
		entry *InventoryEntry
		score float64
	}

	now := time.Now().UnixNano()
	scored_ := make([]scored, len(candidates))
	for i, entry := range candidates {
		rep := float64(e.state.SellerReputation(entry.SellerKey))
		// Recency: 1.0 for brand-new, decaying over 30 days.
		ageSec := float64(now-entry.PutTimestamp) / 1e9
		const thirtyDays = 30 * 24 * 3600.0
		recency := 1.0 - (ageSec / thirtyDays)
		if recency < 0 {
			recency = 0
		}
		scored_[i] = scored{entry: entry, score: rep/100.0*0.6 + recency*0.4}
	}

	// Sort descending by score.
	for i := 0; i < len(scored_); i++ {
		for j := i + 1; j < len(scored_); j++ {
			if scored_[j].score > scored_[i].score {
				scored_[i], scored_[j] = scored_[j], scored_[i]
			}
		}
	}

	if max > len(scored_) {
		max = len(scored_)
	}
	out := make([]*InventoryEntry, max)
	for i := 0; i < max; i++ {
		out[i] = scored_[i].entry
	}
	return out
}

// computePriceMinPrice is the floor price returned when an entry has no valid
// base price (TokenCost <= 0 or PutPrice <= 0 with no token cost).
// A floor of 1 prevents zero-price entries from bypassing budget filters and
// from receiving l1Efficiency=1.0 (free-item dominance) in the ranker.
const computePriceMinPrice int64 = 1

// computePrice returns the exchange's asking price for an entry.
//
// Base price: PutPrice * 1.2 (20% operator margin) when a put-accept exists,
// otherwise TokenCost * 0.7 (seller's 70% share as a proxy pending acceptance).
//
// Five inventory signals adjust the base price:
//   - Demand count: +10% per distinct completed buyer, capped at +100%.
//   - Age decay: decays from 1.0 to 0.5 linearly over 60 days (PutTimestamp=0 = no decay).
//   - Reputation: rep=0 -> 0.8x, rep=50 -> 1.0x, rep=100 -> 1.2x.
//   - Content size: +0.3% per KB, capped at +30% (>=100KB).
//   - Density markup (compressed derivatives only): base * (original_size / compressed_size)
//     * DensityMarkupFactor (default 1.2). Higher density = higher per-token price.
//     Total cost is still lower than raw because fewer tokens are delivered.
//     Falls back to base pricing when the original entry is not found.
//
// Invariants:
//   - Returns at least computePriceMinPrice (never 0 or negative).
//   - Guards against int64 overflow for large TokenCost and PutPrice values.
func (e *Engine) computePrice(entry *InventoryEntry) int64 {
	// Step 1: base price
	var base float64
	if entry.PutPrice > 0 {
		if entry.PutPrice > math.MaxInt64/120 {
			return math.MaxInt64
		}
		base = float64(entry.PutPrice) * 1.20
	} else {
		if entry.TokenCost <= 0 {
			return computePriceMinPrice
		}
		if entry.TokenCost > math.MaxInt64/70 {
			return math.MaxInt64
		}
		base = float64(entry.TokenCost) * 0.70
		if base < float64(computePriceMinPrice) {
			base = float64(computePriceMinPrice)
		}
	}

	// Step 2: demand multiplier (+10% per buyer, capped at +100%)
	demandCount := e.state.EntryDemandCount(entry.EntryID)
	if demandCount > 10 {
		demandCount = 10
	}
	demandFactor := 1.0 + float64(demandCount)*0.10

	// Step 3: age decay (PutTimestamp=0 means no decay)
	ageFactor := 1.0
	if entry.PutTimestamp > 0 {
		ageSec := float64(time.Now().UnixNano()-entry.PutTimestamp) / 1e9
		const sixtyDays = 60 * 24 * 3600.0
		decay := ageSec / sixtyDays
		if decay > 1.0 {
			decay = 1.0
		}
		ageFactor = 1.0 - 0.5*decay
	}

	// Step 4: reputation multiplier (rep=0->0.8x, rep=50->1.0x, rep=100->1.2x)
	rep := e.state.SellerReputation(entry.SellerKey)
	repFactor := 0.8 + float64(rep)/100.0*0.4

	// Step 5: content size multiplier (+0.3% per KB, capped at +30%)
	sizeFactor := 1.0
	if entry.ContentSize > 0 {
		sizeKB := float64(entry.ContentSize) / 1024.0
		sizeBonus := sizeKB * 0.003
		if sizeBonus > 0.30 {
			sizeBonus = 0.30
		}
		sizeFactor = 1.0 + sizeBonus
	}

	// Step 6: dynamic price adjustment from the fast pricing loop.
	// If no active adjustment exists (cold start, no loop yet, expired TTL), the
	// multiplier returned by GetPriceAdjustment is 1.0 — a no-op.
	fastAdj := e.state.GetPriceAdjustment(entry.EntryID)
	fastFactor := fastAdj.Multiplier
	if fastFactor <= 0 {
		fastFactor = 1.0
	}

	// Step 7: density markup for compressed derivatives.
	// Formula: base * (original_size / compressed_size) * density_markup_factor.
	// The per-token price is higher (higher information density), but total cost
	// is lower because fewer tokens are delivered. Falls back to 1.0 (no markup)
	// when the entry is not a derivative or the original entry is not found.
	densityFactor := 1.0
	if entry.CompressedFrom != "" && entry.ContentSize > 0 {
		orig := e.state.GetInventoryEntry(entry.CompressedFrom)
		if orig != nil && orig.ContentSize > 0 {
			ratio := float64(orig.ContentSize) / float64(entry.ContentSize)
			densityFactor = ratio * e.opts.densityMarkupFactor()
		}
	}

	// Step 8: compound all multipliers
	price := base * demandFactor * ageFactor * repFactor * sizeFactor * fastFactor * densityFactor

	// Step 9: clamp and round (nearest-integer, not truncate, for stable results)
	rounded := math.Round(price)
	if rounded < float64(computePriceMinPrice) {
		return computePriceMinPrice
	}
	if rounded >= float64(math.MaxInt64) {
		return math.MaxInt64
	}
	return int64(rounded)
}

// computeConfidence returns a composite confidence score [0,1].
// For v0.1 uses seller reputation as proxy.
func (e *Engine) computeConfidence(entry *InventoryEntry, _ string) float64 {
	rep := e.state.SellerReputation(entry.SellerKey)
	return float64(rep) / 100.0
}

// sendOperatorMessage sends an operator-signed message to the exchange campfire
// via the protocol WriteClient. Returns a dontguess *Message so callers don't
// depend on store.MessageRecord.
func (e *Engine) sendOperatorMessage(payload []byte, tags []string, antecedents []string) (*Message, error) {
	if e.opts.WriteClient == nil {
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
	return FromSDKMessage(sdkMsg), nil
}

// AutoAcceptPut sends a settle(put-accept) for a pending put message, accepting
// it into inventory at the given price and expiry. This implements automatic
// acceptance for the engine; a real deployment would add validation first.
//
// The put message must exist in the store. This method does not require the put
// to already be in the engine's in-memory state — it will replay the store to
// pick up new messages first.
func (e *Engine) AutoAcceptPut(putMsgID string, price int64, expiresAt time.Time) error {
	// Replay to ensure state is current before checking.
	if err := e.replayAll(); err != nil {
		return fmt.Errorf("replay before put-accept: %w", err)
	}

	e.state.mu.RLock()
	pendingEntry, pending := e.state.pendingPuts[putMsgID]
	var putSellerKey string
	if pending {
		putSellerKey = pendingEntry.SellerKey
	}
	_ = putSellerKey // used below after e.state.Apply(rec)
	e.state.mu.RUnlock()
	if !pending {
		return fmt.Errorf("put %s is not pending", putMsgID)
	}

	var expiresAtStr string
	if !expiresAt.IsZero() {
		expiresAtStr = expiresAt.UTC().Format(time.RFC3339)
	}

	payload, err := json.Marshal(map[string]any{
		"phase":      SettlePhaseStrPutAccept,
		"entry_id":   putMsgID,
		"price":      price,
		"expires_at": expiresAtStr,
	})
	if err != nil {
		return fmt.Errorf("encoding put-accept payload: %w", err)
	}

	tags := []string{
		TagSettle,
		TagPhasePrefix + SettlePhaseStrPutAccept,
		TagVerdictPrefix + "accepted",
	}
	antecedents := []string{putMsgID}

	// sendOperatorMessage returns the persisted record directly — no need to
	// re-query the store. This avoids the race where lastSentMessage could
	// return a concurrently-written message instead of the one we just sent.
	rec, err := e.sendOperatorMessage(payload, tags, antecedents)
	if err != nil {
		return err
	}

	// Apply immediately so state is consistent before the next poll.
	if rec != nil {
		e.state.Apply(rec)
	}

	// Record the seller's current provenance level against the newly accepted entry.
	// This snapshot enables provenance downgrade detection (dontguess-lqp): if the
	// seller's level drops below AcceptedProvenanceLevel in the future, the entry
	// will be flagged for re-validation via MarkStaleProvenanceEntries.
	if e.opts.ProvenanceChecker != nil && putSellerKey != "" {
		level := int(e.opts.ProvenanceChecker.store.Level(putSellerKey))
		e.state.SetEntryProvenanceLevel(putMsgID, level)
	}

	// Incrementally update the match index with the newly accepted entry.
	// The entry is now live in state; add it to the index so subsequent
	// buy requests can find it without waiting for a full Rebuild.
	inv := e.state.Inventory()
	for _, entry := range inv {
		if entry.PutMsgID == putMsgID {
			e.matchIndex.Add(e.inventoryEntryToRankInput(entry))
			break
		}
	}

	// Hot compression offer: immediately assign a compress task to the original
	// seller at 50% of token_cost. Failure is non-fatal — the entry is already
	// accepted; the compression offer is best-effort.
	if pendingEntry != nil && pendingEntry.SellerKey != "" {
		if err := e.sendCompressionAssign(pendingEntry); err != nil {
			e.opts.log("engine: compression assign failed entry=%s err=%v", putMsgID, err)
		}
	}
	return nil
}

// sendCompressionAssign sends an exchange:assign message for a compress task
// directed exclusively at the original seller of the given entry. The bounty
// is 50% of the entry's token_cost. The description includes the entry ID,
// content hash, and instructions to run /compress.
//
// This is sent immediately after a put is accepted (hot path). Failure is
// non-fatal to the caller — the error is logged and the accept proceeds.
func (e *Engine) sendCompressionAssign(entry *InventoryEntry) error {
	bounty := entry.TokenCost / 2
	description := fmt.Sprintf(
		"Compress cached inference entry %s (content_hash=%s). Run /compress with the entry content to produce a Nyquist-sampled summary. Bounty: %d scrip.",
		entry.EntryID, entry.ContentHash, bounty,
	)
	payload, err := json.Marshal(map[string]any{
		"entry_id":         entry.EntryID,
		"task_type":        "compress",
		"reward":           bounty,
		"exclusive_sender": entry.SellerKey,
		"description":      description,
	})
	if err != nil {
		return fmt.Errorf("encoding compression assign payload: %w", err)
	}
	tags := []string{TagAssign}
	msg, err := e.sendOperatorMessage(payload, tags, nil)
	if err != nil {
		return fmt.Errorf("sending compression assign: %w", err)
	}
	if msg != nil {
		e.state.Apply(msg)
	}
	e.opts.log("engine: compression assign sent assign_id=%s entry=%s seller=%s bounty=%d",
		shortKey(msg.ID), shortKey(entry.PutMsgID), shortKey(entry.SellerKey), bounty)
	return nil
}

// sendWarmCompressionAssign sends an exchange:assign message for a compress task
// directed exclusively at the buyer of the matched entry. The bounty is 30% of
// the entry's token_cost (floor integer division). The buyer just consumed the
// raw content and has it in cache, making them the ideal compressor.
//
// Called after a successful match when no compressed derivative exists for the
// matched entry. Failure is non-fatal to the caller — the error is logged and
// the match proceeds.
func (e *Engine) sendWarmCompressionAssign(entry *InventoryEntry, buyerKey string) error {
	bounty := entry.TokenCost * 30 / 100
	description := fmt.Sprintf(
		"Compress cached inference entry %s (content_hash=%s). You just received this content — run /compress to produce a Nyquist-sampled summary. Bounty: %d scrip.",
		entry.EntryID, entry.ContentHash, bounty,
	)
	payload, err := json.Marshal(map[string]any{
		"entry_id":         entry.EntryID,
		"task_type":        "compress",
		"reward":           bounty,
		"exclusive_sender": buyerKey,
		"description":      description,
	})
	if err != nil {
		return fmt.Errorf("encoding warm compression assign payload: %w", err)
	}
	tags := []string{TagAssign}
	msg, err := e.sendOperatorMessage(payload, tags, nil)
	if err != nil {
		return fmt.Errorf("sending warm compression assign: %w", err)
	}
	if msg != nil {
		e.state.Apply(msg)
	}
	e.opts.log("engine: warm compression assign sent assign_id=%s entry=%s buyer=%s bounty=%d",
		shortKey(msg.ID), shortKey(entry.PutMsgID), shortKey(buyerKey), bounty)
	return nil
}


// hasActiveBuyerCompressAssign returns true if there is already an active
// (non-terminal) compression assign for the given entry targeting the buyer.
// Used to prevent duplicate warm compression assigns for the same buyer.
func (e *Engine) hasActiveBuyerCompressAssign(entryID, buyerKey string) bool {
	for _, a := range e.state.ActiveAssigns(entryID) {
		if a.TaskType == "compress" && a.ExclusiveSender == buyerKey {
			return true
		}
	}
	return false
}

// rebuildMatchIndex rebuilds the semantic match index from the current live inventory.
// Called after replay and when the inventory changes significantly.
func (e *Engine) rebuildMatchIndex() {
	inventory := e.state.Inventory()
	inputs := make([]matching.RankInput, len(inventory))
	for i, entry := range inventory {
		inputs[i] = e.inventoryEntryToRankInput(entry)
	}
	e.matchIndex.Rebuild(inputs)
}

// inventoryEntryToRankInput converts an InventoryEntry to a matching.RankInput.
// Price is computed by the engine's pricing logic so the ranker sees current ask price.
func (e *Engine) inventoryEntryToRankInput(entry *InventoryEntry) matching.RankInput {
	return matching.RankInput{
		EntryID:          entry.EntryID,
		SellerKey:        entry.SellerKey,
		Description:      entry.Description,
		ContentType:      entry.ContentType,
		Domains:          entry.Domains,
		TokenCost:        entry.TokenCost,
		Price:            e.computePrice(entry),
		SellerReputation: e.state.SellerReputation(entry.SellerKey),
		PutTimestamp:     entry.PutTimestamp,
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
// via applyScripBuyHold â no full log scan needed at query time.
func (e *Engine) findExistingBuyerAcceptHold(matchMsgID string) string {
	return e.state.GetBuyHoldReservation(matchMsgID)
}

// newReservationID generates a random hex reservation ID.
func newReservationID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("rand.Read: %v", err))
	}
	return hex.EncodeToString(b)
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
