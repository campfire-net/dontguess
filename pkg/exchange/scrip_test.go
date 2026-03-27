package exchange_test

// Tests for SpendingStore integration in the exchange engine.
//
// These tests verify:
//   - handleBuy pre-decrements buyer's scrip by (price + fee) before emitting a match
//   - handleBuy returns ErrBudgetExceeded when buyer has insufficient scrip
//   - handleSettle(complete) pays seller residual and exchange revenue
//   - handleDispute(dispute) refunds buyer's pre-decremented scrip

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/campfire-net/campfire/pkg/store"

	"github.com/3dl-dev/dontguess/pkg/exchange"
	"github.com/3dl-dev/dontguess/pkg/scrip"
)

// --- In-memory SpendingStore for testing ---

// memScripStore is a minimal in-memory SpendingStore for unit tests.
// It does not replay a campfire log — balances are seeded directly.
type memScripStore struct {
	mu           sync.Mutex
	balances     map[string]int64
	reservations map[string]scrip.Reservation
}

func newMemScripStore() *memScripStore {
	return &memScripStore{
		balances:     make(map[string]int64),
		reservations: make(map[string]scrip.Reservation),
	}
}

func (s *memScripStore) seed(agentKey string, amount int64) {
	s.mu.Lock()
	s.balances[agentKey] += amount
	s.mu.Unlock()
}

func (s *memScripStore) balance(agentKey string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.balances[agentKey]
}

func (s *memScripStore) GetBudget(_ context.Context, pk, _ string) (int64, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.balances[pk]
	return v, fmt.Sprintf("%d", v), nil
}

func (s *memScripStore) DecrementBudget(_ context.Context, pk, _ string, amount int64, etag string) (int64, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Minimal ETag check: etag must equal current balance string (our etag is the balance).
	cur := s.balances[pk]
	if fmt.Sprintf("%d", cur) != etag {
		return 0, "", scrip.ErrConflict
	}
	if cur < amount {
		return 0, "", scrip.ErrBudgetExceeded
	}
	s.balances[pk] = cur - amount
	newVal := s.balances[pk]
	return newVal, fmt.Sprintf("%d", newVal), nil
}

func (s *memScripStore) AddBudget(_ context.Context, pk, _ string, amount int64, _ string) (int64, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.balances[pk] += amount
	v := s.balances[pk]
	return v, fmt.Sprintf("%d", v), nil
}

func (s *memScripStore) SaveReservation(_ context.Context, r scrip.Reservation) error {
	s.mu.Lock()
	s.reservations[r.ID] = r
	s.mu.Unlock()
	return nil
}

func (s *memScripStore) GetReservation(_ context.Context, id string) (scrip.Reservation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.reservations[id]
	if !ok {
		return scrip.Reservation{}, scrip.ErrReservationNotFound
	}
	return r, nil
}

func (s *memScripStore) DeleteReservation(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.reservations[id]; !ok {
		return scrip.ErrReservationNotFound
	}
	delete(s.reservations, id)
	return nil
}

// reservationCount returns the number of in-flight reservations.
func (s *memScripStore) reservationCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.reservations)
}

// firstReservation returns the first (only) reservation, or panics if none.
func (s *memScripStore) firstReservation() scrip.Reservation {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.reservations {
		return r
	}
	panic("no reservations")
}

// --- Helpers ---

// newEngineWithScrip builds a testHarness + engine with a memScripStore wired in.
func newEngineWithScrip(t *testing.T, scripStore scrip.SpendingStore) (*testHarness, *exchange.Engine) {
	t.Helper()
	h := newTestHarness(t)
	eng := exchange.NewEngine(exchange.EngineOptions{
		CampfireID:       h.cfID,
		OperatorIdentity: h.operator,
		Store:            h.st,
		Transport:        h.transport,
		ScripStore:       scripStore,
		Logger: func(format string, args ...any) {
			t.Logf("[engine] "+format, args...)
		},
	})
	return h, eng
}

// seedInventoryEntry puts + accepts one entry, returning the put message ID.
func seedInventoryEntry(t *testing.T, h *testHarness, eng *exchange.Engine, desc, contentType string, tokenCost, putPrice int64) string {
	t.Helper()
	putMsg := h.sendMessage(h.seller,
		putPayload(desc, "sha256:"+fmt.Sprintf("%064x", tokenCost), contentType, tokenCost, tokenCost*2),
		[]string{exchange.TagPut, "exchange:content-type:" + contentType},
		nil,
	)
	msgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(msgs)
	if err := eng.AutoAcceptPut(putMsg.ID, putPrice, time.Now().Add(72*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut: %v", err)
	}
	return putMsg.ID
}

// waitForMatchMessage polls until a new match message appears, returns the last one.
func waitForMatchMessage(t *testing.T, h *testHarness, before []store.MessageRecord, timeout time.Duration) *store.MessageRecord {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		msgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})
		if len(msgs) > len(before) {
			last := msgs[len(msgs)-1]
			return &last
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("timed out waiting for match message")
	return nil
}

// --- Tests ---

// TestBuy_PreDecrementsScripBeforeMatch verifies that a buy with sufficient
// scrip causes the engine to pre-decrement the buyer's balance by (price + fee)
// and emit a match message that includes a reservation_id.
func TestBuy_PreDecrementsScripBeforeMatch(t *testing.T) {
	t.Parallel()
	st := newMemScripStore()
	h, eng := newEngineWithScrip(t, st)

	// Seed one inventory entry; put_price = 5600, computed sale price = 5600*120/100 = 6720.
	seedInventoryEntry(t, h, eng, "Go HTTP handler generator", "code", 8000, 5600)
	inv := eng.State().Inventory()
	if len(inv) != 1 {
		t.Fatalf("expected 1 inventory entry, got %d", len(inv))
	}
	salePrice := inv[0].PutPrice * 120 / 100 // computePrice logic: 20% markup
	fee := salePrice / exchange.MatchingFeeRate
	holdAmount := salePrice + fee

	// Seed buyer with enough scrip.
	st.seed(h.buyer.PublicKeyHex(), holdAmount+1000)
	buyerBalanceBefore := st.balance(h.buyer.PublicKeyHex())

	preMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() { _ = eng.Start(ctx) }()

	// Buyer sends buy message.
	h.sendMessage(h.buyer,
		buyPayload("Generate Go HTTP handler unit tests", salePrice+5000),
		[]string{exchange.TagBuy},
		nil,
	)

	matchMsg := waitForMatchMessage(t, h, preMsgs, 2*time.Second)
	cancel()

	// Buyer balance must have decreased by holdAmount.
	buyerBalanceAfter := st.balance(h.buyer.PublicKeyHex())
	if buyerBalanceAfter != buyerBalanceBefore-holdAmount {
		t.Errorf("buyer balance: got %d, want %d (before=%d - hold=%d)",
			buyerBalanceAfter, buyerBalanceBefore-holdAmount, buyerBalanceBefore, holdAmount)
	}

	// A reservation must exist.
	if st.reservationCount() != 1 {
		t.Errorf("expected 1 reservation, got %d", st.reservationCount())
	}

	// Match payload must include reservation_id.
	var mp struct {
		SearchMeta struct {
			ReservationID string `json:"reservation_id"`
		} `json:"search_meta"`
	}
	if err := json.Unmarshal(matchMsg.Payload, &mp); err != nil {
		t.Fatalf("parsing match payload: %v", err)
	}
	if mp.SearchMeta.ReservationID == "" {
		t.Error("match search_meta.reservation_id must be non-empty after scrip pre-decrement")
	}
}

// TestBuy_InsufficientScripReturnsError verifies that a buy with insufficient
// scrip causes the engine to return an error and NOT emit a match message.
func TestBuy_InsufficientScripReturnsError(t *testing.T) {
	t.Parallel()
	st := newMemScripStore()
	h, eng := newEngineWithScrip(t, st)

	// Seed one entry.
	seedInventoryEntry(t, h, eng, "Python scraper generator", "code", 10000, 7000)
	inv := eng.State().Inventory()
	if len(inv) != 1 {
		t.Fatalf("expected 1 inventory entry, got %d", len(inv))
	}
	salePrice := inv[0].PutPrice * 120 / 100
	fee := salePrice / exchange.MatchingFeeRate
	holdAmount := salePrice + fee

	// Seed buyer with LESS than required.
	st.seed(h.buyer.PublicKeyHex(), holdAmount-1)

	preMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() { _ = eng.Start(ctx) }()

	h.sendMessage(h.buyer,
		buyPayload("Build a Python async web scraper", salePrice+5000),
		[]string{exchange.TagBuy},
		nil,
	)

	// Wait the full timeout — no match should appear.
	time.Sleep(1 * time.Second)
	cancel()

	afterMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})
	if len(afterMsgs) > len(preMsgs) {
		t.Error("expected no match message when buyer has insufficient scrip")
	}

	// No reservation should have been saved.
	if st.reservationCount() != 0 {
		t.Errorf("expected 0 reservations on failed buy, got %d", st.reservationCount())
	}

	// Buyer balance must be unchanged.
	if st.balance(h.buyer.PublicKeyHex()) != holdAmount-1 {
		t.Errorf("buyer balance changed unexpectedly: got %d, want %d",
			st.balance(h.buyer.PublicKeyHex()), holdAmount-1)
	}
}

// TestSettle_AdjustsScripOnComplete verifies that when a settle(complete) message
// is dispatched with a valid reservation_id, the engine:
//   - Credits residual to the seller
//   - Credits exchange revenue to the operator
//   - Deletes the reservation
func TestSettle_AdjustsScripOnComplete(t *testing.T) {
	t.Parallel()
	st := newMemScripStore()
	h, eng := newEngineWithScrip(t, st)

	// Seed inventory entry.
	seedInventoryEntry(t, h, eng, "Terraform module generator", "code", 8000, 5600)
	inv := eng.State().Inventory()
	if len(inv) != 1 {
		t.Fatalf("expected 1 inventory entry, got %d", len(inv))
	}
	salePrice := inv[0].PutPrice * 120 / 100
	fee := salePrice / exchange.MatchingFeeRate
	holdAmount := salePrice + fee

	// Seed buyer and run buy to get a reservation.
	st.seed(h.buyer.PublicKeyHex(), holdAmount+5000)

	preMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() { _ = eng.Start(ctx) }()

	h.sendMessage(h.buyer,
		buyPayload("Generate Terraform module for S3", salePrice+5000),
		[]string{exchange.TagBuy},
		nil,
	)

	waitForMatchMessage(t, h, preMsgs, 2*time.Second)
	cancel()

	// Must have a reservation now.
	if st.reservationCount() != 1 {
		t.Fatalf("expected 1 reservation after buy, got %d", st.reservationCount())
	}
	res := st.firstReservation()

	sellerBalanceBefore := st.balance(h.seller.PublicKeyHex())
	operatorBalanceBefore := st.balance(h.operator.PublicKeyHex())

	// Manually dispatch a settle(complete) message with the reservation_id.
	completePayload, _ := json.Marshal(map[string]any{
		"reservation_id": res.ID,
		"seller_key":     h.seller.PublicKeyHex(),
		"price":          salePrice,
		"entry_id":       inv[0].EntryID,
	})
	completeMsg := h.sendMessage(h.operator, completePayload,
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrComplete,
		},
		nil,
	)

	// Apply to state and dispatch.
	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(allMsgs)
	rec, err := h.st.GetMessage(completeMsg.ID)
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if err := eng.DispatchForTest(rec); err != nil {
		t.Fatalf("dispatch settle(complete): %v", err)
	}

	// Verify residual was paid to seller.
	expectedResidual := salePrice / exchange.ResidualRate
	sellerBalanceAfter := st.balance(h.seller.PublicKeyHex())
	if sellerBalanceAfter != sellerBalanceBefore+expectedResidual {
		t.Errorf("seller balance: got %d, want %d (before=%d + residual=%d)",
			sellerBalanceAfter, sellerBalanceBefore+expectedResidual, sellerBalanceBefore, expectedResidual)
	}

	// Verify exchange revenue was credited to operator.
	expectedExchangeRevenue := salePrice - expectedResidual
	operatorBalanceAfter := st.balance(h.operator.PublicKeyHex())
	if operatorBalanceAfter != operatorBalanceBefore+expectedExchangeRevenue {
		t.Errorf("operator balance: got %d, want %d (before=%d + revenue=%d)",
			operatorBalanceAfter, operatorBalanceBefore+expectedExchangeRevenue, operatorBalanceBefore, expectedExchangeRevenue)
	}

	// Reservation must be deleted.
	if st.reservationCount() != 0 {
		t.Errorf("expected 0 reservations after settle(complete), got %d", st.reservationCount())
	}
}

// TestDispute_RefundsScripToBuyer verifies that when a settle(dispute) message
// is dispatched with a valid reservation_id, the engine:
//   - Refunds the full pre-decremented amount to the buyer
//   - Deletes the reservation
func TestDispute_RefundsScripToBuyer(t *testing.T) {
	t.Parallel()
	st := newMemScripStore()
	h, eng := newEngineWithScrip(t, st)

	// Seed inventory entry.
	seedInventoryEntry(t, h, eng, "Security audit generator", "review", 15000, 10500)
	inv := eng.State().Inventory()
	if len(inv) != 1 {
		t.Fatalf("expected 1 inventory entry, got %d", len(inv))
	}
	salePrice := inv[0].PutPrice * 120 / 100
	fee := salePrice / exchange.MatchingFeeRate
	holdAmount := salePrice + fee

	// Seed buyer with enough scrip.
	st.seed(h.buyer.PublicKeyHex(), holdAmount+5000)
	buyerBalanceBefore := st.balance(h.buyer.PublicKeyHex())

	preMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() { _ = eng.Start(ctx) }()

	h.sendMessage(h.buyer,
		buyPayload("Audit Go HTTP handlers for security issues", salePrice+5000),
		[]string{exchange.TagBuy},
		nil,
	)

	waitForMatchMessage(t, h, preMsgs, 2*time.Second)
	cancel()

	if st.reservationCount() != 1 {
		t.Fatalf("expected 1 reservation after buy, got %d", st.reservationCount())
	}
	res := st.firstReservation()

	// Buyer balance must be lower by holdAmount now.
	if st.balance(h.buyer.PublicKeyHex()) != buyerBalanceBefore-holdAmount {
		t.Errorf("buyer balance after buy: got %d, want %d",
			st.balance(h.buyer.PublicKeyHex()), buyerBalanceBefore-holdAmount)
	}

	// Manually dispatch a settle(dispute) message.
	disputePayload, _ := json.Marshal(map[string]any{
		"reservation_id": res.ID,
		"buyer_key":      h.buyer.PublicKeyHex(),
		"entry_id":       inv[0].EntryID,
		"dispute_type":   "quality",
	})
	disputeMsg := h.sendMessage(h.operator, disputePayload,
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrDispute,
		},
		nil,
	)

	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(allMsgs)
	rec, err := h.st.GetMessage(disputeMsg.ID)
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if err := eng.DispatchForTest(rec); err != nil {
		t.Fatalf("dispatch settle(dispute): %v", err)
	}

	// Buyer balance must be fully restored.
	buyerBalanceAfter := st.balance(h.buyer.PublicKeyHex())
	if buyerBalanceAfter != buyerBalanceBefore {
		t.Errorf("buyer balance after dispute: got %d, want %d (full refund)",
			buyerBalanceAfter, buyerBalanceBefore)
	}

	// Reservation must be deleted.
	if st.reservationCount() != 0 {
		t.Errorf("expected 0 reservations after dispute refund, got %d", st.reservationCount())
	}
}
