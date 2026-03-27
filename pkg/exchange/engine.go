package exchange

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
}

// NewEngine creates an exchange engine. Call Start to begin processing.
func NewEngine(opts EngineOptions) *Engine {
	idx := opts.MatchIndex
	if idx == nil {
		idx = matching.NewIndex(nil, matching.RankOptions{})
	}
	return &Engine{
		opts:       opts,
		state:      NewState(),
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

// handleBuy responds to an exchange:buy request with an exchange:match message.
//
// The buy message is sent as a campfire future (--future). The engine responds
// with --fulfills <buy-msg-id> to complete the future.
//
// If ScripStore is configured, the buyer's scrip balance is pre-decremented
// by (best_price + fee) before matching. If the buyer has insufficient scrip,
// the buy is rejected with ErrBudgetExceeded and no match is emitted.
func (e *Engine) handleBuy(msg *store.MessageRecord) error {
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
	type rankedCandidate struct {
		entry      *InventoryEntry
		confidence float64
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
		semanticMatches = append(semanticMatches, rankedCandidate{
			entry:            entry,
			confidence:       e.computeConfidence(entry, payload.Task),
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

		ctx := context.Background()
		buyerKey := msg.Sender
		bal, etag, err := e.opts.ScripStore.GetBudget(ctx, buyerKey, scrip.BalanceKey)
		if err != nil {
			return fmt.Errorf("scrip: GetBudget for buyer %s: %w", buyerKey[:8], err)
		}
		if bal < holdAmount {
			return fmt.Errorf("scrip: buyer %s: %w (balance=%d, required=%d)",
				buyerKey[:8], scrip.ErrBudgetExceeded, bal, holdAmount)
		}
		_, newETag, err := e.opts.ScripStore.DecrementBudget(ctx, buyerKey, scrip.BalanceKey, holdAmount, etag)
		if err != nil {
			return fmt.Errorf("scrip: DecrementBudget for buyer %s: %w", buyerKey[:8], err)
		}

		// Save reservation so settle/dispute can reference it.
		reservationID = newReservationID()
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
			buyerKey[:8], holdAmount, reservationID[:8])
	}

	meta := map[string]any{"total_candidates": len(candidates)}
	if reservationID != "" {
		meta["reservation_id"] = reservationID
	}

	matchPayload, err := json.Marshal(map[string]any{
		"results":     matchResults,
		"search_meta": meta,
	})
	if err != nil {
		return fmt.Errorf("encoding match payload: %w", err)
	}

	tags := []string{TagMatch}
	// Antecedent is the buy message; --fulfills semantics use the antecedent.
	antecedents := []string{msg.ID}

	return e.sendOperatorMessage(matchPayload, tags, antecedents)
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
	var payload struct {
		ReservationID string `json:"reservation_id"`
		SellerKey     string `json:"seller_key"`
		Price         int64  `json:"price"`
	}
	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		return fmt.Errorf("scrip: parsing settle(complete) payload: %w", err)
	}
	if payload.ReservationID == "" || payload.SellerKey == "" || payload.Price <= 0 {
		// Not a scrip-bearing settlement — skip.
		return nil
	}

	ctx := context.Background()

	// Retrieve and delete reservation.
	_, err := e.opts.ScripStore.GetReservation(ctx, payload.ReservationID)
	if err != nil {
		e.opts.log("engine: settle: reservation %s not found: %v", payload.ReservationID[:8], err)
		return nil // reservation missing — already settled or expired
	}
	if err := e.opts.ScripStore.DeleteReservation(ctx, payload.ReservationID); err != nil {
		e.opts.log("engine: settle: delete reservation %s: %v", payload.ReservationID[:8], err)
	}

	fee := payload.Price / MatchingFeeRate
	residual := payload.Price / ResidualRate
	exchangeRevenue := payload.Price - residual // fee already came out of the buyer's pre-decrement

	operatorKey := operatorKeyHex(e.opts.OperatorIdentity.PublicKey)

	// Credit residual to seller.
	if residual > 0 {
		if _, _, err := e.opts.ScripStore.AddBudget(ctx, payload.SellerKey, scrip.BalanceKey, residual, ""); err != nil {
			e.opts.log("engine: settle: add residual to seller %s: %v", payload.SellerKey[:8], err)
		}
	}

	// Credit exchange revenue to operator.
	if exchangeRevenue > 0 {
		if _, _, err := e.opts.ScripStore.AddBudget(ctx, operatorKey, scrip.BalanceKey, exchangeRevenue, ""); err != nil {
			e.opts.log("engine: settle: add exchange revenue to operator: %v", err)
		}
	}

	// Log burn amount (the matching fee has been pre-decremented from the buyer;
	// it was never credited anywhere — effectively burned by omission).
	e.opts.log("engine: settle: reservation=%s seller=%s residual=%d fee_burned=%d exchange=%d",
		payload.ReservationID[:8], payload.SellerKey[:8], residual, fee, exchangeRevenue)
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

	ctx := context.Background()

	res, err := e.opts.ScripStore.GetReservation(ctx, payload.ReservationID)
	if err != nil {
		e.opts.log("engine: dispute: reservation %s not found: %v", payload.ReservationID[:8], err)
		return nil
	}

	// Refund the full held amount to the buyer.
	if _, _, err := e.opts.ScripStore.AddBudget(ctx, payload.BuyerKey, scrip.BalanceKey, res.Amount, ""); err != nil {
		return fmt.Errorf("scrip: dispute refund for buyer %s: %w", payload.BuyerKey[:8], err)
	}

	if err := e.opts.ScripStore.DeleteReservation(ctx, payload.ReservationID); err != nil {
		e.opts.log("engine: dispute: delete reservation %s: %v", payload.ReservationID[:8], err)
	}

	e.opts.log("engine: dispute refund: reservation=%s buyer=%s amount=%d",
		payload.ReservationID[:8], payload.BuyerKey[:8], res.Amount)
	return nil
}

// findCandidates returns inventory entries that satisfy the buyer's filters.
func (e *Engine) findCandidates(buyerKey string, budget int64, minRep int,
	freshnessHours int, contentType string, domains []string) []*InventoryEntry {

	inventory := e.state.Inventory()
	var out []*InventoryEntry

	for _, entry := range inventory {
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

// computePrice returns the exchange's asking price for an entry.
// For v0.1 this is a simple 1.2x markup over put price (20% margin).
// The full pricing engine (dontguess-pricing) will replace this.
func (e *Engine) computePrice(entry *InventoryEntry) int64 {
	if entry.PutPrice > 0 {
		return entry.PutPrice * 120 / 100
	}
	// If no put-accept yet (pending), use 70% of token cost as a proxy.
	return entry.TokenCost * 70 / 100
}

// computeConfidence returns a composite confidence score [0,1].
// For v0.1 uses seller reputation as proxy.
func (e *Engine) computeConfidence(entry *InventoryEntry, _ string) float64 {
	rep := e.state.SellerReputation(entry.SellerKey)
	return float64(rep) / 100.0
}

// sendOperatorMessage creates, signs, and writes an operator-signed message to
// the exchange campfire transport.
func (e *Engine) sendOperatorMessage(payload []byte, tags []string, antecedents []string) error {
	op := e.opts.OperatorIdentity
	msg, err := message.NewMessage(op.PrivateKey, op.PublicKey, payload, tags, antecedents)
	if err != nil {
		return fmt.Errorf("creating operator message: %w", err)
	}

	// Add provenance hop from the exchange campfire.
	state, err := e.opts.Transport.ReadState(e.opts.CampfireID)
	if err != nil {
		return fmt.Errorf("reading campfire state for hop: %w", err)
	}
	members, err := e.opts.Transport.ListMembers(e.opts.CampfireID)
	if err != nil {
		return fmt.Errorf("listing members for hop: %w", err)
	}
	cf := state.ToCampfire(members)
	if err := msg.AddHop(
		state.PrivateKey, state.PublicKey,
		cf.MembershipHash(), len(members),
		state.JoinProtocol, state.ReceptionRequirements,
		campfire.RoleFull,
	); err != nil {
		return fmt.Errorf("adding provenance hop: %w", err)
	}

	if err := e.opts.Transport.WriteMessage(e.opts.CampfireID, msg); err != nil {
		return fmt.Errorf("writing operator message: %w", err)
	}

	// Persist to the store so subsequent polls see it.
	rec := store.MessageRecordFromMessage(e.opts.CampfireID, msg, store.NowNano())
	if _, err := e.opts.Store.AddMessage(rec); err != nil {
		// Non-fatal: the message is already in the transport. Log and continue.
		e.opts.log("engine: warning: adding message to store: %v", err)
	}

	return nil
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
	_, pending := e.state.pendingPuts[putMsgID]
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

	if err := e.sendOperatorMessage(payload, tags, antecedents); err != nil {
		return err
	}

	// Apply immediately so state is consistent before the next poll.
	rec, err := e.lastSentMessage()
	if err == nil {
		e.state.Apply(rec)
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

// lastSentMessage retrieves the most recent message sent to this campfire.
// Used to apply a just-sent operator message to state immediately.
func (e *Engine) lastSentMessage() (*store.MessageRecord, error) {
	msgs, err := e.opts.Store.ListMessages(e.opts.CampfireID, 0,
		store.MessageFilter{
			Sender: operatorKeyHex(e.opts.OperatorIdentity.PublicKey),
		})
	if err != nil {
		return nil, err
	}
	if len(msgs) == 0 {
		return nil, fmt.Errorf("no operator messages found")
	}
	return &msgs[len(msgs)-1], nil
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
		DisputeCount:     0, // upheld disputes: tracked via SellerStats, not per-entry here
		HasUpheldDispute: false,
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
