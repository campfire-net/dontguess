package exchange

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/campfire-net/campfire/pkg/campfire"
	"github.com/campfire-net/campfire/pkg/identity"
	"github.com/campfire-net/campfire/pkg/message"
	"github.com/campfire-net/campfire/pkg/store"
	"github.com/campfire-net/campfire/pkg/transport/fs"

	"github.com/3dl-dev/dontguess/pkg/matching"
	"github.com/3dl-dev/dontguess/pkg/scrip"
)

// MatchingFeeRate is the fraction of the sale price charged as a matching fee.
// The fee is burned (deflationary). 10% = 1/10.
const MatchingFeeRate = 10

// ResidualRate is the fraction of the sale price paid as residual to the
// original seller. 10% = 1/10.
const ResidualRate = 10

// EngineOptions configures an exchange engine.
type EngineOptions struct {
	// CampfireID is the exchange campfire's public key hex.
	CampfireID string
	// OperatorIdentity is the exchange operator's Ed25519 keypair.
	OperatorIdentity *identity.Identity
	// Store is the campfire message store (SQLite).
	Store store.Store
	// Transport is the filesystem transport for writing response messages.
	Transport *fs.Transport
	// PollInterval controls how often the engine polls for new messages.
	// Defaults to 500ms.
	PollInterval time.Duration
	// Logger receives diagnostic log lines. If nil, logs are suppressed.
	Logger func(format string, args ...any)
	// MatchIndex is the semantic matching index used to rank buy results.
	// If nil, the engine creates a default TF-IDF index on startup.
	MatchIndex *matching.Index
	// ScripStore is the scrip spending store used for pre-decrement / adjust / refund
	// on buy / settle / dispute operations. If nil, scrip checks are skipped (useful
	// for tests that do not exercise the scrip flow).
	ScripStore scrip.SpendingStore
	// ProvenanceChecker validates sender provenance levels before processing operations.
	// If nil, provenance checks are skipped (useful for tests that don't exercise provenance).
	ProvenanceChecker *ProvenanceChecker
}

func (o *EngineOptions) pollInterval() time.Duration {
	if o.PollInterval > 0 {
		return o.PollInterval
	}
	return 500 * time.Millisecond
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
	opts        EngineOptions
	state       *State
	matchIndex  *matching.Index
	lastCursor  int64 // received_at cursor: last processed message's received_at
	// ctx is the shutdown context passed to Start. Handlers use this so that
	// in-flight scrip operations are cancelled on graceful shutdown instead of
	// using context.Background() which ignores the shutdown signal.
	ctx context.Context
	// marshalFunc overrides json.Marshal for tests that need to inject marshal failures.
	// Nil means use the standard json.Marshal.
	marshalFunc func(v any) ([]byte, error)
}

// engineCtx returns the shutdown context stored at Start time.
// Falls back to context.Background() when the engine has not been started
// (e.g., in tests that call handlers directly without Start).
func (e *Engine) engineCtx() context.Context {
	if e.ctx != nil {
		return e.ctx
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
		idx = matching.NewIndex(nil, matching.RankOptions{})
	}
	st := NewState()
	if opts.OperatorIdentity != nil {
		st.OperatorKey = operatorKeyHex(opts.OperatorIdentity.PublicKey)
	}
	return &Engine{
		opts:       opts,
		state:      st,
		matchIndex: idx,
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
	e.ctx = ctx
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
		// Fetch the original buy message from the store to dispatch.
		rec, err := e.opts.Store.GetMessage(order.OrderID)
		if err != nil || rec == nil {
			e.opts.log("engine: could not fetch order message %s: %v", order.OrderID, err)
			continue
		}
		if err := e.handleBuy(rec); err != nil {
			e.opts.log("engine: error handling pending buy %s: %v", order.OrderID, err)
		}
	}
	return nil
}

// replayAll loads all historical messages from the store and rebuilds state.
func (e *Engine) replayAll() error {
	msgs, err := e.opts.Store.ListMessages(e.opts.CampfireID, 0)
	if err != nil {
		return fmt.Errorf("listing messages for replay: %w", err)
	}

	e.state.Replay(msgs)

	// Set cursor to the latest received_at so subsequent polls start from here.
	for _, m := range msgs {
		if m.ReceivedAt > e.lastCursor {
			e.lastCursor = m.ReceivedAt
		}
	}

	// Rebuild the match index from the current live inventory.
	e.rebuildMatchIndex()

	e.opts.log("engine: replayed %d messages, cursor=%d, indexed %d entries",
		len(msgs), e.lastCursor, e.matchIndex.Len())
	return nil
}

// run is the main event loop. It polls for new messages and processes them.
func (e *Engine) run(ctx context.Context) error {
	ticker := time.NewTicker(e.opts.pollInterval())
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := e.poll(); err != nil {
				e.opts.log("engine: poll error: %v", err)
				// Non-fatal: log and continue.
			}
		}
	}
}

// poll fetches new messages since lastCursor, applies them to state, and
// triggers handlers for actionable operations.
func (e *Engine) poll() error {
	filter := store.MessageFilter{
		Tags: []string{TagPut, TagBuy, TagSettle},
		AfterReceivedAt: e.lastCursor,
	}
	msgs, err := e.opts.Store.ListMessages(e.opts.CampfireID, 0, filter)
	if err != nil {
		return fmt.Errorf("polling messages: %w", err)
	}

	for i := range msgs {
		msg := &msgs[i]
		e.state.Apply(msg)
		if err := e.dispatch(msg); err != nil {
			e.opts.log("engine: dispatch error (msg=%s): %v", msg.ID, err)
		}
		if msg.ReceivedAt > e.lastCursor {
			e.lastCursor = msg.ReceivedAt
		}
	}
	return nil
}

// dispatch routes a new message to the appropriate handler.
func (e *Engine) dispatch(msg *store.MessageRecord) error {
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
	case TagBuy:
		return e.handleBuy(msg)
	case TagSettle:
		phase := settlePhaseFromTags(msg.Tags)
		if phase == SettlePhaseStrDispute {
			return e.handleDispute(msg)
		}
		return e.handleSettle(msg)
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
func (e *Engine) handleBuy(msg *store.MessageRecord) error {
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
		MinReputation  int      `json:"min_reputation"`
		FreshnessHours int      `json:"freshness_hours"`
		ContentType    string   `json:"content_type"`
		Domains        []string `json:"domains"`
		MaxResults     int      `json:"max_results"`
	}
	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		return fmt.Errorf("parsing buy payload: %w", err)
	}

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

	// Pre-decrement buyer's scrip before emitting the match.
	// Amount = best price + matching fee. If no results, skip scrip.
	var reservationID string
	if e.opts.ScripStore != nil && len(matchResults) > 0 {
		bestPrice := matchResults[0].Price
		fee := bestPrice / MatchingFeeRate
		holdAmount := bestPrice + fee

		ctx := e.engineCtx()
		buyerKey := msg.Sender

		// Check whether a scrip-buy-hold message already exists for this buy order.
		// This happens when the engine restarts with pending buy orders: the buy-hold
		// message was written to the campfire log on the previous run and the
		// CampfireScripStore already replayed it (decrementing the buyer's balance).
		// Re-running DecrementBudget here would double-charge the buyer.
		existingResID := e.findExistingBuyHold(msg.ID)

		if existingResID != "" {
			// Buy-hold already applied via replay. Restore the in-memory reservation
			// so settle/dispute handlers can reference it, then skip DecrementBudget.
			reservationID = existingResID
			_, currentETag, _ := e.opts.ScripStore.GetBudget(ctx, buyerKey, scrip.BalanceKey)
			res := scrip.Reservation{
				ID:        reservationID,
				AgentKey:  buyerKey,
				RK:        scrip.BalanceKey,
				ETag:      currentETag,
				Amount:    holdAmount,
				CreatedAt: time.Now(),
			}
			if err := e.opts.ScripStore.SaveReservation(ctx, res); err != nil {
				e.opts.log("engine: warning: re-save reservation after restart %s: %v", shortKey(reservationID), err)
			}
			e.opts.log("engine: scrip buy-hold already replayed, skipping pre-decrement buyer=%s reservation=%s",
				shortKey(buyerKey), shortKey(reservationID))
		} else {
			bal, etag, err := e.opts.ScripStore.GetBudget(ctx, buyerKey, scrip.BalanceKey)
			if err != nil {
				return fmt.Errorf("scrip: GetBudget for buyer %s: %w", shortKey(buyerKey), err)
			}
			if bal < holdAmount {
				return fmt.Errorf("scrip: buyer %s: %w (balance=%d, required=%d)",
					shortKey(buyerKey), scrip.ErrBudgetExceeded, bal, holdAmount)
			}

			// Marshal the buy-hold convention message BEFORE mutating scrip state.
			// If marshal fails, no balance mutation has occurred — return the error.
			reservationID = newReservationID()
			expiresAt := time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339)
			holdPayload, err := e.marshal(scrip.BuyHoldPayload{
				Buyer:         buyerKey,
				Amount:        holdAmount,
				Price:         bestPrice,
				Fee:           fee,
				ReservationID: reservationID,
				BuyMsg:        msg.ID,
				ExpiresAt:     expiresAt,
			})
			if err != nil {
				return fmt.Errorf("scrip: marshal buy-hold payload: %w", err)
			}

			_, newETag, err := e.opts.ScripStore.DecrementBudget(ctx, buyerKey, scrip.BalanceKey, holdAmount, etag)
			if err != nil {
				return fmt.Errorf("scrip: DecrementBudget for buyer %s: %w", shortKey(buyerKey), err)
			}

			// Save reservation so settle/dispute can reference it.
			res := scrip.Reservation{
				ID:        reservationID,
				AgentKey:  buyerKey,
				RK:        scrip.BalanceKey,
				ETag:      newETag,
				Amount:    holdAmount,
				CreatedAt: time.Now(),
			}
			if err := e.opts.ScripStore.SaveReservation(ctx, res); err != nil {
				return fmt.Errorf("scrip: SaveReservation: %w", err)
			}
			e.opts.log("engine: scrip pre-decremented buyer=%s hold=%d reservation=%s",
				shortKey(buyerKey), holdAmount, shortKey(reservationID))

			// Emit scrip-buy-hold convention message so CampfireScripStore can replay it.
			if _, emitErr := e.sendOperatorMessage(holdPayload,
				[]string{scrip.TagScripBuyHold}, []string{msg.ID}); emitErr != nil {
				e.opts.log("engine: warning: emit scrip-buy-hold: %v", emitErr)
			}
		}
	}

	meta := map[string]any{"total_candidates": len(candidates)}
	if reservationID != "" {
		meta["reservation_id"] = reservationID
	}

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

	_, err = e.sendOperatorMessage(matchPayload, tags, antecedents)
	return err
}

// handleSettle processes settlement messages.
//
// For settle(complete) phases, if ScripStore is configured, the engine:
//   - Pays the seller their residual (price * ResidualRate / 100)
//   - Burns the matching fee (price * MatchingFeeRate / 100)
//   - Credits exchange revenue (remainder) to the operator
//
// The reservation_id in the settle payload links back to the buy-hold reservation.
func (e *Engine) handleSettle(msg *store.MessageRecord) error {
	if e.opts.ScripStore == nil {
		return nil
	}

	phase := settlePhaseFromTags(msg.Tags)
	if phase != SettlePhaseStrComplete {
		// Only complete phase triggers scrip movement.
		// Other phases (buyer-accept, deliver, put-accept) are tracked in state only.
		return nil
	}

	// Parse the complete payload.
	// SellerKey is intentionally NOT parsed from the payload — it is buyer-controlled
	// (TAINTED) and must never be trusted for payment routing. The real seller is
	// derived from the antecedent chain below (convention §3, security fix rudi-x3y).
	var payload struct {
		ReservationID string `json:"reservation_id"`
		Price         int64  `json:"price"`
	}
	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		return fmt.Errorf("scrip: parsing settle(complete) payload: %w", err)
	}
	// Validate price and reservation_id.
	// Split the check: missing reservation_id is a silent skip (no scrip involved).
	// A non-positive price with a reservation_id is a hard rejection (security fix dontguess-ica).
	if payload.ReservationID == "" {
		return nil // no reservation_id — not a scrip-bearing settlement
	}
	if payload.Price <= 0 {
		e.opts.log("engine: settle: reservation %s rejected — price=%d (must be >0)",
			shortKey(payload.ReservationID), payload.Price)
		return fmt.Errorf("scrip: settle reservation %s: price must be positive, got %d",
			shortKey(payload.ReservationID), payload.Price)
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

	ctx := e.engineCtx()

	// Atomically retrieve and delete reservation (prevents TOCTOU double-spend).
	res, err := e.opts.ScripStore.ConsumeReservation(ctx, payload.ReservationID)
	if err != nil {
		e.opts.log("engine: settle: reservation %s not found: %v", shortKey(payload.ReservationID), err)
		return nil // reservation missing — already settled or expired
	}

	// Cross-check payload.Price against res.Amount to prevent market manipulation.
	// At buy time, the engine set res.Amount = price + price/MatchingFeeRate.
	// If payload.Price (buyer-controlled) differs from what was pre-approved,
	// the buyer is attempting to inflate or deflate the seller credit. Reject.
	// Security fix: dontguess-ica (dontguess-3oo, dontguess-z2a).
	expectedHold := payload.Price + payload.Price/MatchingFeeRate
	if expectedHold != res.Amount {
		if restoreErr := e.opts.ScripStore.SaveReservation(ctx, res); restoreErr != nil {
			e.opts.log("engine: settle: CRITICAL: failed to restore reservation %s after price mismatch: %v",
				shortKey(payload.ReservationID), restoreErr)
			return fmt.Errorf("scrip: settle reservation %s: price mismatch AND restore failed: payload=%d expected_hold=%d res.Amount=%d",
				shortKey(payload.ReservationID), payload.Price, expectedHold, res.Amount)
		}
		e.opts.log("engine: settle: reservation %s rejected — price mismatch payload=%d expected_hold=%d res.Amount=%d",
			shortKey(payload.ReservationID), payload.Price, expectedHold, res.Amount)
		return fmt.Errorf("scrip: settle reservation %s: payload.Price=%d inconsistent with reservation amount=%d",
			shortKey(payload.ReservationID), payload.Price, res.Amount)
	}

	fee := payload.Price / MatchingFeeRate
	residual := payload.Price / ResidualRate
	exchangeRevenue := payload.Price - residual // fee already came out of the buyer's pre-decrement

	operatorKey := operatorKeyHex(e.opts.OperatorIdentity.PublicKey)

	// Marshal both convention messages BEFORE mutating scrip state.
	// If either marshal fails, restore the reservation (it was consumed above) and return
	// the error — no balance mutations have occurred.
	settlePayload, err := e.marshal(scrip.SettlePayload{
		ReservationID:   payload.ReservationID,
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
				shortKey(payload.ReservationID), restoreErr)
			return fmt.Errorf("scrip: settle reservation %s: marshal failed AND restore failed (reservation lost): %w",
				shortKey(payload.ReservationID), err)
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
					shortKey(payload.ReservationID), restoreErr)
				return fmt.Errorf("scrip: settle reservation %s: marshal failed AND restore failed (reservation lost): %w",
					shortKey(payload.ReservationID), err)
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

	// Emit scrip-burn for the matching fee (already removed from buyer's balance via buy-hold).
	if len(burnPayload) > 0 {
		if _, emitErr := e.sendOperatorMessage(burnPayload,
			[]string{scrip.TagScripBurn}, []string{msg.ID}); emitErr != nil {
			e.opts.log("engine: warning: emit scrip-burn: %v", emitErr)
		}
	}

	e.opts.log("engine: settle: reservation=%s seller=%s residual=%d fee_burned=%d exchange=%d",
		shortKey(payload.ReservationID), shortKey(sellerKey), residual, fee, exchangeRevenue)
	return nil
}

// handleDispute processes settle(dispute) messages.
//
// If ScripStore is configured, the engine refunds the buyer's pre-decremented
// scrip using the reservation_id from the dispute payload.
func (e *Engine) handleDispute(msg *store.MessageRecord) error {
	if e.opts.ScripStore == nil {
		return nil
	}

	var payload struct {
		ReservationID string `json:"reservation_id"`
		BuyerKey      string `json:"buyer_key"`
	}
	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		return fmt.Errorf("scrip: parsing dispute payload: %w", err)
	}
	if payload.ReservationID == "" || payload.BuyerKey == "" {
		return nil // not a scrip-bearing dispute
	}

	ctx := e.engineCtx()

	// Atomically retrieve and delete reservation (prevents TOCTOU double-spend).
	res, err := e.opts.ScripStore.ConsumeReservation(ctx, payload.ReservationID)
	if err != nil {
		e.opts.log("engine: dispute: reservation %s not found: %v", shortKey(payload.ReservationID), err)
		return nil
	}

	// Security check: the buyer_key in the dispute payload must match the agent key
	// recorded in the reservation at buy time. Reject any dispute that tries to
	// redirect the refund to a different identity.
	// If the check fails, restore the reservation before returning — ConsumeReservation
	// already deleted it, and the legitimate owner must still be able to claim a refund.
	if res.AgentKey != payload.BuyerKey {
		// Restore the atomically consumed reservation so the legitimate owner can
		// still dispute and claim their refund. If restore fails, the reservation
		// is permanently lost — surface an error that includes both failures so the
		// caller has full context.
		if restoreErr := e.opts.ScripStore.SaveReservation(ctx, res); restoreErr != nil {
			e.opts.log("engine: dispute: CRITICAL: failed to restore reservation %s after key mismatch: %v",
				shortKey(payload.ReservationID), restoreErr)
			return fmt.Errorf("scrip: dispute reservation %s: buyer_key mismatch AND restore failed (reservation lost): %w",
				shortKey(payload.ReservationID), restoreErr)
		}
		return fmt.Errorf("scrip: dispute reservation %s: buyer_key mismatch (payload=%s, reservation=%s)",
			shortKey(payload.ReservationID), shortKey(payload.BuyerKey), shortKey(res.AgentKey))
	}

	// Sender identity gate: the campfire message sender must be either the buyer
	// who holds the reservation, or the exchange operator processing the dispute.
	// Convention §9.5: "dispute: sender must be the buyer."
	// The operator is permitted because the scrip-bearing dispute is the operator's
	// action to issue a refund after investigating the buyer's initial filing.
	// Any other campfire member sending a dispute with a valid reservation_id is
	// rejected — they cannot trigger a refund on behalf of another buyer.
	operatorKey := operatorKeyHex(e.opts.OperatorIdentity.PublicKey)
	if msg.Sender != res.AgentKey && msg.Sender != operatorKey {
		if restoreErr := e.opts.ScripStore.SaveReservation(ctx, res); restoreErr != nil {
			e.opts.log("engine: dispute: CRITICAL: failed to restore reservation %s after sender mismatch: %v",
				shortKey(payload.ReservationID), restoreErr)
			return fmt.Errorf("scrip: dispute reservation %s: sender mismatch AND restore failed (reservation lost): %w",
				shortKey(payload.ReservationID), restoreErr)
		}
		return fmt.Errorf("scrip: dispute reservation %s: sender mismatch (sender=%s, buyer=%s)",
			shortKey(payload.ReservationID), shortKey(msg.Sender), shortKey(res.AgentKey))
	}

	// Marshal the convention message BEFORE mutating scrip state.
	// If marshal fails, restore the reservation (it was consumed above) and return the error.
	refundPayload, err := e.marshal(scrip.DisputeRefundPayload{
		Buyer:         res.AgentKey,
		Amount:        res.Amount,
		ReservationID: payload.ReservationID,
		DisputeMsg:    msg.ID,
	})
	if err != nil {
		if restoreErr := e.opts.ScripStore.SaveReservation(ctx, res); restoreErr != nil {
			e.opts.log("engine: dispute: CRITICAL: failed to restore reservation %s after marshal failure: %v",
				shortKey(payload.ReservationID), restoreErr)
			return fmt.Errorf("scrip: dispute reservation %s: marshal failed AND restore failed (reservation lost): %w",
				shortKey(payload.ReservationID), restoreErr)
		}
		return fmt.Errorf("scrip: marshal dispute-refund payload: %w", err)
	}

	// Refund the full held amount to the buyer.
	// Use res.AgentKey (the trusted identity recorded at buy time), not payload.BuyerKey
	// (attacker-controlled). The check above confirmed they match, but we always use the
	// reservation's authoritative key as the refund target.
	if _, _, err := e.opts.ScripStore.AddBudget(ctx, res.AgentKey, scrip.BalanceKey, res.Amount, ""); err != nil {
		return fmt.Errorf("scrip: dispute refund for buyer %s: %w", shortKey(res.AgentKey), err)
	}

	// Emit scrip-dispute-refund convention message so CampfireScripStore can replay it.
	if _, emitErr := e.sendOperatorMessage(refundPayload,
		[]string{scrip.TagScripDisputeRefund}, []string{msg.ID}); emitErr != nil {
		e.opts.log("engine: warning: emit scrip-dispute-refund: %v", emitErr)
	}

	e.opts.log("engine: dispute refund: reservation=%s buyer=%s amount=%d",
		shortKey(payload.ReservationID), shortKey(res.AgentKey), res.Amount)
	return nil
}

// findCandidates returns inventory entries that satisfy the buyer's filters.
func (e *Engine) findCandidates(buyerKey string, budget int64, minRep int,
	freshnessHours int, contentType string, domains []string) []*InventoryEntry {

	inventory := e.state.Inventory()
	var out []*InventoryEntry

	for _, entry := range inventory {
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
// For v0.1 this is a simple 1.2x markup over put price (20% margin).
// The full pricing engine (dontguess-pricing) will replace this.
//
// Invariants:
//   - Returns at least computePriceMinPrice (never 0 or negative).
//   - Guards against int64 overflow for large TokenCost values.
func (e *Engine) computePrice(entry *InventoryEntry) int64 {
	if entry.PutPrice > 0 {
		// 1.2x markup: guard against overflow on very large PutPrice.
		// MaxInt64 / 120 ≈ 7.69e16; prices above that would overflow.
		if entry.PutPrice > math.MaxInt64/120 {
			return math.MaxInt64
		}
		return entry.PutPrice * 120 / 100
	}

	// No put-accept yet (pending): use 70% of token cost as a proxy.
	// Guard: TokenCost <= 0 means no valid base price — return floor.
	if entry.TokenCost <= 0 {
		return computePriceMinPrice
	}
	// Guard against int64 overflow: MaxInt64 / 70 ≈ 1.32e17.
	if entry.TokenCost > math.MaxInt64/70 {
		return math.MaxInt64
	}
	price := entry.TokenCost * 70 / 100
	if price < computePriceMinPrice {
		return computePriceMinPrice
	}
	return price
}

// computeConfidence returns a composite confidence score [0,1].
// For v0.1 uses seller reputation as proxy.
func (e *Engine) computeConfidence(entry *InventoryEntry, _ string) float64 {
	rep := e.state.SellerReputation(entry.SellerKey)
	return float64(rep) / 100.0
}

// sendOperatorMessage creates, signs, and writes an operator-signed message to
// the exchange campfire transport.
func (e *Engine) sendOperatorMessage(payload []byte, tags []string, antecedents []string) (*store.MessageRecord, error) {
	op := e.opts.OperatorIdentity
	msg, err := message.NewMessage(op.PrivateKey, op.PublicKey, payload, tags, antecedents)
	if err != nil {
		return nil, fmt.Errorf("creating operator message: %w", err)
	}

	// Add provenance hop from the exchange campfire.
	state, err := e.opts.Transport.ReadState(e.opts.CampfireID)
	if err != nil {
		return nil, fmt.Errorf("reading campfire state for hop: %w", err)
	}
	members, err := e.opts.Transport.ListMembers(e.opts.CampfireID)
	if err != nil {
		return nil, fmt.Errorf("listing members for hop: %w", err)
	}
	cf := state.ToCampfire(members)
	if err := msg.AddHop(
		state.PrivateKey, state.PublicKey,
		cf.MembershipHash(), len(members),
		state.JoinProtocol, state.ReceptionRequirements,
		campfire.RoleFull,
	); err != nil {
		return nil, fmt.Errorf("adding provenance hop: %w", err)
	}

	if err := e.opts.Transport.WriteMessage(e.opts.CampfireID, msg); err != nil {
		return nil, fmt.Errorf("writing operator message: %w", err)
	}

	// Persist to the store so subsequent polls see it.
	rec := store.MessageRecordFromMessage(e.opts.CampfireID, msg, store.NowNano())
	if _, err := e.opts.Store.AddMessage(rec); err != nil {
		// Non-fatal: the message is already in the transport. Log and continue.
		e.opts.log("engine: warning: adding message to store: %v", err)
	}

	return &rec, nil
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
	return nil
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
		DisputeCount:     e.state.SellerDisputeCount(entry.SellerKey),
		HasUpheldDispute: e.state.HasUpheldDispute(entry.EntryID),
	}
}

// findExistingBuyHold scans the campfire log for a scrip-buy-hold message
// that was emitted for the given buy message ID. Returns the reservation ID
// if found, or "" if no prior buy-hold exists.
//
// Called by handleBuy to detect the restart-with-pending-orders scenario:
// if a scrip-buy-hold was already written to the log (and thus replayed by
// CampfireScripStore into the in-memory balance), we must NOT call
// DecrementBudget again or the buyer will be double-charged.
func (e *Engine) findExistingBuyHold(buyMsgID string) string {
	msgs, err := e.opts.Store.ListMessages(e.opts.CampfireID, 0,
		store.MessageFilter{
			Tags:   []string{scrip.TagScripBuyHold},
			Sender: operatorKeyHex(e.opts.OperatorIdentity.PublicKey),
		})
	if err != nil {
		return ""
	}
	for i := range msgs {
		var p scrip.BuyHoldPayload
		if err := json.Unmarshal(msgs[i].Payload, &p); err != nil {
			continue
		}
		if p.BuyMsg == buyMsgID && p.ReservationID != "" {
			return p.ReservationID
		}
	}
	return ""
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
