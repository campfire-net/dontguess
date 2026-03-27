package scrip

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/campfire-net/campfire/pkg/store"
)

// scrip operation tag constants. These match the convention spec in
// docs/convention/scrip-operations.md.
const (
	tagScripMint          = "dontguess:scrip-mint"
	tagScripBurn          = "dontguess:scrip-burn"
	tagScripPutPay        = "dontguess:scrip-put-pay"
	tagScripBuyHold       = "dontguess:scrip-buy-hold"
	tagScripSettle        = "dontguess:scrip-settle"
	tagScripAssignPay     = "dontguess:scrip-assign-pay"
	tagScripDisputeRefund = "dontguess:scrip-dispute-refund"
)

// balanceEntry holds the in-memory balance for a single agent.
type balanceEntry struct {
	mu    sync.Mutex
	value int64
	gen   uint64 // monotonic generation counter — used as ETag
}

func (e *balanceEntry) etag() string {
	return fmt.Sprintf("%d", e.gen)
}

// CampfireScripStore implements SpendingStore backed by a campfire message log.
//
// State is derived by replaying all scrip operation messages in sequence order.
// The in-memory balance map is a materialized view; it can always be rebuilt from
// the message log. ETags are monotonic generation counters that increment on every
// balance mutation.
//
// Reservations are stored in memory. They are ephemeral — the campfire log
// contains the authoritative record of scrip movements (buy-hold messages are
// written to the campfire before a reservation is created here). On restart,
// any in-flight reservations that were not settled become stale; the escrow
// timeout mechanism (external) handles automatic refund.
//
// CampfireScripStore is safe for concurrent use.
type CampfireScripStore struct {
	campfireID string
	st         store.Store

	// balances maps agentKey -> *balanceEntry.
	// Populated on construction via Replay, updated on every mutation.
	balancesMu sync.RWMutex
	balances   map[string]*balanceEntry

	// reservations maps reservation ID -> Reservation.
	resMu        sync.Mutex
	reservations map[string]Reservation

	// seenMsgIDs tracks processed message IDs to prevent replay of duplicates.
	// Uses sync.Map for lock-free concurrent reads.
	seenMsgIDs sync.Map

	// totalSupply tracks total scrip ever minted.
	totalSupply atomic.Int64
	// totalBurned tracks total scrip destroyed.
	totalBurned atomic.Int64
}

// NewCampfireScripStore creates a CampfireScripStore and replays the campfire
// log to build initial balance state.
//
// campfireID is the exchange campfire's public key hex. st is the campfire store.
func NewCampfireScripStore(campfireID string, st store.Store) (*CampfireScripStore, error) {
	s := &CampfireScripStore{
		campfireID:   campfireID,
		st:           st,
		balances:     make(map[string]*balanceEntry),
		reservations: make(map[string]Reservation),
	}
	if err := s.Replay(); err != nil {
		return nil, fmt.Errorf("scrip store: replay: %w", err)
	}
	return s, nil
}

// Replay rebuilds balance state from the campfire message log.
// It resets all balances and re-derives them from scratch.
// Called on construction; can be called again to resync.
func (s *CampfireScripStore) Replay() error {
	msgs, err := s.st.ListMessages(s.campfireID, 0)
	if err != nil {
		return fmt.Errorf("listing messages: %w", err)
	}

	// Reset state.
	s.balancesMu.Lock()
	s.balances = make(map[string]*balanceEntry)
	s.balancesMu.Unlock()

	s.seenMsgIDs = sync.Map{}
	s.totalSupply.Store(0)
	s.totalBurned.Store(0)

	for i := range msgs {
		s.applyMessage(&msgs[i])
	}
	return nil
}

// applyMessage applies a single campfire message to the in-memory balance state.
// It is idempotent for messages already in seenMsgIDs.
func (s *CampfireScripStore) applyMessage(msg *store.MessageRecord) {
	// Idempotency guard: skip messages we've already processed.
	if _, loaded := s.seenMsgIDs.LoadOrStore(msg.ID, struct{}{}); loaded {
		return
	}

	op := scripOp(msg.Tags)
	if op == "" {
		return
	}

	switch op {
	case tagScripMint:
		s.applyMint(msg)
	case tagScripBurn:
		s.applyBurn(msg)
	case tagScripPutPay:
		s.applyPutPay(msg)
	case tagScripBuyHold:
		s.applyBuyHold(msg)
	case tagScripSettle:
		s.applySettle(msg)
	case tagScripAssignPay:
		s.applyAssignPay(msg)
	case tagScripDisputeRefund:
		s.applyDisputeRefund(msg)
	}
}

// applyMint processes a scrip:mint message.
// Payload: { "recipient": "<pubkey>", "amount": <int64>, ... }
func (s *CampfireScripStore) applyMint(msg *store.MessageRecord) {
	var p struct {
		Recipient string `json:"recipient"`
		Amount    int64  `json:"amount"`
	}
	if err := json.Unmarshal(msg.Payload, &p); err != nil || p.Recipient == "" || p.Amount <= 0 {
		return
	}
	s.addToBalance(p.Recipient, p.Amount)
	s.totalSupply.Add(p.Amount)
}

// applyBurn processes a scrip:burn message.
// Payload: { "amount": <int64>, ... }
// Note: burn destroys scrip that was already removed from a balance (e.g. via buy-hold).
// It only affects totalBurned, not individual balances.
func (s *CampfireScripStore) applyBurn(msg *store.MessageRecord) {
	var p struct {
		Amount int64 `json:"amount"`
	}
	if err := json.Unmarshal(msg.Payload, &p); err != nil || p.Amount <= 0 {
		return
	}
	s.totalBurned.Add(p.Amount)
}

// applyPutPay processes a scrip:put-pay message.
// Payload: { "seller": "<pubkey>", "amount": <int64>, ... }
// The operator pays the seller; operator balance is decremented, seller balance incremented.
// For the store's purpose: we track both sides. The operator key is the message sender.
func (s *CampfireScripStore) applyPutPay(msg *store.MessageRecord) {
	var p struct {
		Seller string `json:"seller"`
		Amount int64  `json:"amount"`
	}
	if err := json.Unmarshal(msg.Payload, &p); err != nil || p.Seller == "" || p.Amount <= 0 {
		return
	}
	// Operator (sender) pays seller.
	s.addToBalance(p.Seller, p.Amount)
	if msg.Sender != "" {
		s.subtractFromBalance(msg.Sender, p.Amount)
	}
}

// applyBuyHold processes a scrip:buy-hold message.
// Payload: { "buyer": "<pubkey>", "amount": <int64>, ... }
// Buyer's balance is decremented (escrow hold).
func (s *CampfireScripStore) applyBuyHold(msg *store.MessageRecord) {
	var p struct {
		Buyer  string `json:"buyer"`
		Amount int64  `json:"amount"`
	}
	if err := json.Unmarshal(msg.Payload, &p); err != nil || p.Buyer == "" || p.Amount <= 0 {
		return
	}
	s.subtractFromBalance(p.Buyer, p.Amount)
}

// applySettle processes a scrip:settle message.
// Payload: { "reservation_id": "<string>", "seller": "<pubkey>", "residual": <int64>,
//
//	"fee_burned": <int64>, "exchange_revenue": <int64>, ... }
//
// Residual is added to the seller's balance; exchange_revenue to the operator's.
// fee_burned is NOT tracked here — the engine also emits a separate scrip-burn
// message for the matching fee, and applyBurn is the sole source of totalBurned
// accounting. Counting it here too would double-count after Replay.
// The operator identity is the message sender.
func (s *CampfireScripStore) applySettle(msg *store.MessageRecord) {
	var p struct {
		Seller          string `json:"seller"`
		Residual        int64  `json:"residual"`
		FeeBurned       int64  `json:"fee_burned"`
		ExchangeRevenue int64  `json:"exchange_revenue"`
	}
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		return
	}
	if p.Seller != "" && p.Residual > 0 {
		s.addToBalance(p.Seller, p.Residual)
	}
	if msg.Sender != "" && p.ExchangeRevenue > 0 {
		s.addToBalance(msg.Sender, p.ExchangeRevenue)
	}
	// Do NOT increment totalBurned here. The engine emits a scrip-burn message
	// for the matching fee; applyBurn handles totalBurned exclusively.
}

// applyAssignPay processes a scrip:assign-pay message.
// Payload: { "worker": "<pubkey>", "amount": <int64>, ... }
// Operator pays laborer; operator balance decremented, worker balance incremented.
func (s *CampfireScripStore) applyAssignPay(msg *store.MessageRecord) {
	var p struct {
		Worker string `json:"worker"`
		Amount int64  `json:"amount"`
	}
	if err := json.Unmarshal(msg.Payload, &p); err != nil || p.Worker == "" || p.Amount <= 0 {
		return
	}
	s.addToBalance(p.Worker, p.Amount)
	if msg.Sender != "" {
		s.subtractFromBalance(msg.Sender, p.Amount)
	}
}

// applyDisputeRefund processes a scrip:dispute-refund message.
// Payload: { "buyer": "<pubkey>", "amount": <int64>, ... }
// Buyer's balance is restored (escrow released).
func (s *CampfireScripStore) applyDisputeRefund(msg *store.MessageRecord) {
	var p struct {
		Buyer  string `json:"buyer"`
		Amount int64  `json:"amount"`
	}
	if err := json.Unmarshal(msg.Payload, &p); err != nil || p.Buyer == "" || p.Amount <= 0 {
		return
	}
	s.addToBalance(p.Buyer, p.Amount)
}

// --- SpendingStore implementation ---

// GetBudget implements SpendingStore.
// pk is the Ed25519 agent pubkey (hex). rk is typically BalanceKey ("scrip:balance").
// Returns (0, "", nil) if the agent has no balance record.
func (s *CampfireScripStore) GetBudget(_ context.Context, pk, rk string) (int64, string, error) {
	_ = rk // rk is accepted for interface compat; all balances use a single counter per pk
	s.balancesMu.RLock()
	e, ok := s.balances[pk]
	s.balancesMu.RUnlock()
	if !ok {
		return 0, "", nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.value, e.etag(), nil
}

// DecrementBudget implements SpendingStore.
// Atomically subtracts amountMicro from the balance at pk if result >= 0.
// Returns ErrBudgetExceeded if balance < amountMicro.
// Returns ErrConflict on ETag mismatch.
func (s *CampfireScripStore) DecrementBudget(_ context.Context, pk, rk string, amountMicro int64, etag string) (int64, string, error) {
	_ = rk
	if etag == "" {
		return 0, "", ErrConflict
	}

	s.balancesMu.RLock()
	e, ok := s.balances[pk]
	s.balancesMu.RUnlock()

	if !ok {
		return 0, "", ErrConflict
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	if e.etag() != etag {
		return 0, "", ErrConflict
	}
	newValue := e.value - amountMicro
	if newValue < 0 {
		return 0, "", ErrBudgetExceeded
	}
	e.value = newValue
	e.gen++
	return e.value, e.etag(), nil
}

// AddBudget implements SpendingStore.
// Adds amountMicro to the balance at pk. Creates the balance entry if it does not exist.
// If etag is non-empty and stale, returns ErrConflict.
func (s *CampfireScripStore) AddBudget(_ context.Context, pk, rk string, amountMicro int64, etag string) (int64, string, error) {
	_ = rk

	s.balancesMu.RLock()
	e, ok := s.balances[pk]
	s.balancesMu.RUnlock()

	if !ok {
		// Create new balance entry.
		s.balancesMu.Lock()
		// Double-check under write lock.
		e, ok = s.balances[pk]
		if !ok {
			e = &balanceEntry{value: amountMicro, gen: 1}
			s.balances[pk] = e
			s.balancesMu.Unlock()
			return amountMicro, e.etag(), nil
		}
		s.balancesMu.Unlock()
		// Another goroutine created it — fall through to update path.
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	if etag != "" && e.etag() != etag {
		return 0, "", ErrConflict
	}
	e.value += amountMicro
	e.gen++
	return e.value, e.etag(), nil
}

// SaveReservation implements SpendingStore.
func (s *CampfireScripStore) SaveReservation(_ context.Context, r Reservation) error {
	s.resMu.Lock()
	defer s.resMu.Unlock()
	s.reservations[r.ID] = r
	return nil
}

// GetReservation implements SpendingStore.
func (s *CampfireScripStore) GetReservation(_ context.Context, id string) (Reservation, error) {
	s.resMu.Lock()
	defer s.resMu.Unlock()
	r, ok := s.reservations[id]
	if !ok {
		return Reservation{}, ErrReservationNotFound
	}
	return r, nil
}

// DeleteReservation implements SpendingStore.
func (s *CampfireScripStore) DeleteReservation(_ context.Context, id string) error {
	s.resMu.Lock()
	defer s.resMu.Unlock()
	if _, ok := s.reservations[id]; !ok {
		return ErrReservationNotFound
	}
	delete(s.reservations, id)
	return nil
}

// --- Supply stats ---

// TotalSupply returns total scrip ever minted (micro-tokens).
func (s *CampfireScripStore) TotalSupply() int64 {
	return s.totalSupply.Load()
}

// TotalBurned returns total scrip ever burned (micro-tokens).
func (s *CampfireScripStore) TotalBurned() int64 {
	return s.totalBurned.Load()
}

// Balance returns the current balance for the given agent key (micro-tokens).
// Returns 0 for unknown agents.
func (s *CampfireScripStore) Balance(agentKey string) int64 {
	s.balancesMu.RLock()
	e, ok := s.balances[agentKey]
	s.balancesMu.RUnlock()
	if !ok {
		return 0
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.value
}

// --- Internal helpers ---

// addToBalance adds amount to agentKey's balance, creating the entry if needed.
// Called only from Replay/applyMessage — no locking beyond entry-level.
func (s *CampfireScripStore) addToBalance(agentKey string, amount int64) {
	s.balancesMu.Lock()
	e, ok := s.balances[agentKey]
	if !ok {
		e = &balanceEntry{}
		s.balances[agentKey] = e
	}
	s.balancesMu.Unlock()

	e.mu.Lock()
	e.value += amount
	e.gen++
	e.mu.Unlock()
}

// subtractFromBalance subtracts amount from agentKey's balance.
// Allows negative balances during replay (the campfire log is the authority;
// if the log is consistent, balances never go negative in practice).
func (s *CampfireScripStore) subtractFromBalance(agentKey string, amount int64) {
	s.balancesMu.Lock()
	e, ok := s.balances[agentKey]
	if !ok {
		e = &balanceEntry{}
		s.balances[agentKey] = e
	}
	s.balancesMu.Unlock()

	e.mu.Lock()
	e.value -= amount
	e.gen++
	e.mu.Unlock()
}

// scripOp returns the scrip operation tag from a message's tag list, or "".
func scripOp(tags []string) string {
	for _, t := range tags {
		switch t {
		case tagScripMint, tagScripBurn, tagScripPutPay,
			tagScripBuyHold, tagScripSettle, tagScripAssignPay,
			tagScripDisputeRefund:
			return t
		}
	}
	return ""
}
