package exchange

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/campfire-net/campfire/pkg/campfire"
	"github.com/campfire-net/campfire/pkg/identity"
	"github.com/campfire-net/campfire/pkg/message"
	"github.com/campfire-net/campfire/pkg/store"
	"github.com/campfire-net/campfire/pkg/transport/fs"
)

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
type Engine struct {
	opts      EngineOptions
	state     *State
	lastCursor int64 // received_at cursor: last processed message's received_at
}

// NewEngine creates an exchange engine. Call Start to begin processing.
func NewEngine(opts EngineOptions) *Engine {
	return &Engine{
		opts:  opts,
		state: NewState(),
	}
}

// State returns the engine's live state view.
func (e *Engine) State() *State {
	return e.state
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
	e.opts.log("engine: replayed %d messages, cursor=%d", len(msgs), e.lastCursor)
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
		return e.handleSettle(msg)
	}
	return nil
}

// handleBuy responds to an exchange:buy request with an exchange:match message.
//
// The buy message is sent as a campfire future (--future). The engine responds
// with --fulfills <buy-msg-id> to complete the future.
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

	// Search inventory for candidates.
	candidates := e.findCandidates(msg.Sender, payload.Budget, payload.MinReputation,
		payload.FreshnessHours, payload.ContentType, payload.Domains)

	// Rank and cap results.
	results := e.rankResults(candidates, maxResults)

	// Build match payload.
	type MatchResult struct {
		EntryID            string  `json:"entry_id"`
		PutMsgID           string  `json:"put_msg_id"`
		SellerKey          string  `json:"seller_key"`
		Description        string  `json:"description"`
		ContentHash        string  `json:"content_hash"`
		ContentType        string  `json:"content_type"`
		Price              int64   `json:"price"`
		Confidence         float64 `json:"confidence"`
		SellerReputation   int     `json:"seller_reputation"`
		TokenCostOriginal  int64   `json:"token_cost_original"`
		AgeHours           int     `json:"age_hours"`
	}

	matchResults := make([]MatchResult, len(results))
	for i, entry := range results {
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
			Confidence:        e.computeConfidence(entry, payload.Task),
			SellerReputation:  rep,
			TokenCostOriginal: entry.TokenCost,
			AgeHours:          ageHours,
		}
	}

	matchPayload, err := json.Marshal(map[string]any{
		"results":     matchResults,
		"search_meta": map[string]any{"total_candidates": len(candidates)},
	})
	if err != nil {
		return fmt.Errorf("encoding match payload: %w", err)
	}

	tags := []string{TagMatch}
	// Antecedent is the buy message; --fulfills semantics use the antecedent.
	antecedents := []string{msg.ID}

	return e.sendOperatorMessage(matchPayload, tags, antecedents)
}

// handleSettle processes operator-side settlement phases.
// Currently handles put-accept (automatic acceptance of puts).
func (e *Engine) handleSettle(msg *store.MessageRecord) error {
	// Settle messages from buyer are acknowledged but need no operator response here.
	// The engine just ensures state is updated (done in Apply).
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
