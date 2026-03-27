package exchange_test

// Tests for SpendingStore integration in the exchange engine.
//
// These tests verify:
//   - handleBuy pre-decrements buyer's scrip by (price + fee) before emitting a match
//   - handleBuy returns ErrBudgetExceeded when buyer has insufficient scrip
//   - handleSettle(complete) pays seller residual and exchange revenue
//   - handleDispute(dispute) refunds buyer's pre-decremented scrip
//
// Uses the real CampfireScripStore backed by the test harness's campfire store.
// Balances are seeded via dontguess:scrip-mint convention messages written
// directly into the harness store, then Replay() is called on the CampfireScripStore
// to materialize the in-memory balance map.

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/campfire-net/campfire/pkg/identity"
	"github.com/campfire-net/campfire/pkg/store"

	"github.com/3dl-dev/dontguess/pkg/exchange"
	"github.com/3dl-dev/dontguess/pkg/scrip"
)

// addScripMintMsg inserts a dontguess:scrip-mint message into the harness store,
// seeding balance for agentKey without involving the campfire transport.
func addScripMintMsg(t *testing.T, h *testHarness, agentKey string, amount int64) {
	t.Helper()
	rawPayload, err := json.Marshal(map[string]any{
		"recipient":   agentKey,
		"amount":      amount,
		"x402_tx_ref": fmt.Sprintf("test-mint-%s-%d", agentKey[:8], amount),
		"rate":        int64(1000),
	})
	if err != nil {
		t.Fatalf("marshal scrip-mint payload: %v", err)
	}
	rec := store.MessageRecord{
		ID:         fmt.Sprintf("mint-%s-%d-%d", agentKey[:8], amount, time.Now().UnixNano()),
		CampfireID: h.cfID,
		Sender:     h.operator.PublicKeyHex(),
		Payload:    rawPayload,
		Tags:       []string{"dontguess:scrip-mint"},
		Timestamp:  time.Now().UnixNano(),
		ReceivedAt: time.Now().UnixNano(),
		Signature:  []byte{0x00}, // non-nil to satisfy schema NOT NULL constraint
	}
	if _, err := h.st.AddMessage(rec); err != nil {
		t.Fatalf("AddMessage (scrip-mint): %v", err)
	}
}

// newCampfireScripStore creates a CampfireScripStore backed by the harness store.
// Must be called after all mint messages are written so Replay sees them.
// Uses the harness operator identity as the operator key.
func newCampfireScripStore(t *testing.T, h *testHarness) *scrip.CampfireScripStore {
	t.Helper()
	cs, err := scrip.NewCampfireScripStore(h.cfID, h.st, h.operator.PublicKeyHex())
	if err != nil {
		t.Fatalf("NewCampfireScripStore: %v", err)
	}
	return cs
}

// countReservations counts the in-flight reservations by iterating over a
// set of known IDs and checking GetReservation. Since SpendingStore has no
// ListReservations, we track IDs in a separate slice for tests that need counts.
//
// reservationIDs is modified by the caller to accumulate IDs.
func countReservations(t *testing.T, cs *scrip.CampfireScripStore, ids []string) int {
	t.Helper()
	ctx := context.Background()
	n := 0
	for _, id := range ids {
		if _, err := cs.GetReservation(ctx, id); err == nil {
			n++
		}
	}
	return n
}

// --- Helpers ---

// newEngineWithScrip builds a testHarness + engine with a SpendingStore wired in.
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

// extractReservationID parses the reservation_id from a match message payload.
func extractReservationID(t *testing.T, matchMsg *store.MessageRecord) string {
	t.Helper()
	var mp struct {
		SearchMeta struct {
			ReservationID string `json:"reservation_id"`
		} `json:"search_meta"`
	}
	if err := json.Unmarshal(matchMsg.Payload, &mp); err != nil {
		t.Fatalf("parsing match payload: %v", err)
	}
	return mp.SearchMeta.ReservationID
}

// --- Tests ---

// TestBuy_PreDecrementsScripBeforeMatch verifies that a buy with sufficient
// scrip causes the engine to pre-decrement the buyer's balance by (price + fee)
// and emit a match message that includes a reservation_id.
func TestBuy_PreDecrementsScripBeforeMatch(t *testing.T) {
	t.Parallel()

	// Build harness first so we know the campfire ID and buyer key.
	h := newTestHarness(t)

	// Seed one inventory entry; put_price = 5600, computed sale price = 5600*120/100 = 6720.
	cs := newCampfireScripStore(t, h)
	eng := exchange.NewEngine(exchange.EngineOptions{
		CampfireID:       h.cfID,
		OperatorIdentity: h.operator,
		Store:            h.st,
		Transport:        h.transport,
		ScripStore:       cs,
		Logger: func(format string, args ...any) {
			t.Logf("[engine] "+format, args...)
		},
	})

	seedInventoryEntry(t, h, eng, "Go HTTP handler generator", "code", 8000, 5600)
	inv := eng.State().Inventory()
	if len(inv) != 1 {
		t.Fatalf("expected 1 inventory entry, got %d", len(inv))
	}
	salePrice := inv[0].PutPrice * 120 / 100 // computePrice logic: 20% markup
	fee := salePrice / exchange.MatchingFeeRate
	holdAmount := salePrice + fee

	// Seed buyer with enough scrip via mint message, then replay.
	addScripMintMsg(t, h, h.buyer.PublicKeyHex(), holdAmount+1000)
	if err := cs.Replay(); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	buyerBalanceBefore := cs.Balance(h.buyer.PublicKeyHex())

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
	buyerBalanceAfter := cs.Balance(h.buyer.PublicKeyHex())
	if buyerBalanceAfter != buyerBalanceBefore-holdAmount {
		t.Errorf("buyer balance: got %d, want %d (before=%d - hold=%d)",
			buyerBalanceAfter, buyerBalanceBefore-holdAmount, buyerBalanceBefore, holdAmount)
	}

	// Match payload must include reservation_id.
	resID := extractReservationID(t, matchMsg)
	if resID == "" {
		t.Error("match search_meta.reservation_id must be non-empty after scrip pre-decrement")
	}

	// A reservation must exist for that ID.
	if _, err := cs.GetReservation(context.Background(), resID); err != nil {
		t.Errorf("expected reservation %s to exist, got: %v", resID, err)
	}
}

// TestBuy_InsufficientScripReturnsError verifies that a buy with insufficient
// scrip causes the engine to return an error and NOT emit a match message.
func TestBuy_InsufficientScripReturnsError(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	cs := newCampfireScripStore(t, h)
	eng := exchange.NewEngine(exchange.EngineOptions{
		CampfireID:       h.cfID,
		OperatorIdentity: h.operator,
		Store:            h.st,
		Transport:        h.transport,
		ScripStore:       cs,
		Logger: func(format string, args ...any) {
			t.Logf("[engine] "+format, args...)
		},
	})

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
	addScripMintMsg(t, h, h.buyer.PublicKeyHex(), holdAmount-1)
	if err := cs.Replay(); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	buyerBalanceBefore := cs.Balance(h.buyer.PublicKeyHex())

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

	// Buyer balance must be unchanged.
	if cs.Balance(h.buyer.PublicKeyHex()) != buyerBalanceBefore {
		t.Errorf("buyer balance changed unexpectedly: got %d, want %d",
			cs.Balance(h.buyer.PublicKeyHex()), buyerBalanceBefore)
	}
}

// TestSettle_AdjustsScripOnComplete verifies that when a settle(complete) message
// is dispatched with a valid reservation_id, the engine:
//   - Credits residual to the seller
//   - Credits exchange revenue to the operator
//   - Deletes the reservation
func TestSettle_AdjustsScripOnComplete(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	cs := newCampfireScripStore(t, h)
	eng := exchange.NewEngine(exchange.EngineOptions{
		CampfireID:       h.cfID,
		OperatorIdentity: h.operator,
		Store:            h.st,
		Transport:        h.transport,
		ScripStore:       cs,
		Logger: func(format string, args ...any) {
			t.Logf("[engine] "+format, args...)
		},
	})

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
	addScripMintMsg(t, h, h.buyer.PublicKeyHex(), holdAmount+5000)
	if err := cs.Replay(); err != nil {
		t.Fatalf("Replay: %v", err)
	}

	preMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() { _ = eng.Start(ctx) }()

	h.sendMessage(h.buyer,
		buyPayload("Generate Terraform module for S3", salePrice+5000),
		[]string{exchange.TagBuy},
		nil,
	)

	matchMsg := waitForMatchMessage(t, h, preMsgs, 2*time.Second)
	cancel()

	resID := extractReservationID(t, matchMsg)
	if resID == "" {
		t.Fatal("expected non-empty reservation_id in match payload")
	}

	// Reservation must exist.
	ctx2 := context.Background()
	res, err := cs.GetReservation(ctx2, resID)
	if err != nil {
		t.Fatalf("expected reservation %s to exist, got: %v", resID, err)
	}

	sellerBalanceBefore := cs.Balance(h.seller.PublicKeyHex())
	operatorBalanceBefore := cs.Balance(h.operator.PublicKeyHex())

	// Build the antecedent chain required for seller derivation:
	//   complete → deliver → match (via buyer-accept) → entry → seller

	// buyer-accept (antecedent = match message).
	buyerAcceptPayload, _ := json.Marshal(map[string]any{
		"phase":    "buyer-accept",
		"entry_id": inv[0].EntryID,
		"accepted": true,
	})
	buyerAcceptMsg := h.sendMessage(h.buyer, buyerAcceptPayload,
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrBuyerAccept,
			exchange.TagVerdictPrefix + "accepted",
		},
		[]string{matchMsg.ID},
	)

	// deliver (antecedent = buyer-accept message).
	deliverMsgPayload, _ := json.Marshal(map[string]any{
		"phase":        "deliver",
		"entry_id":     inv[0].EntryID,
		"content_ref":  "sha256:" + fmt.Sprintf("%064x", 999),
		"content_size": int64(20000),
	})
	deliverMsg := h.sendMessage(h.operator, deliverMsgPayload,
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrDeliver,
		},
		[]string{buyerAcceptMsg.ID},
	)

	// Replay all messages so the antecedent chain is in state.
	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(allMsgs)

	// complete (antecedent = deliver message).
	completePayload, _ := json.Marshal(map[string]any{
		"reservation_id": res.ID,
		"price":          salePrice,
		"entry_id":       inv[0].EntryID,
	})
	completeMsg := h.sendMessage(h.buyer, completePayload,
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrComplete,
		},
		[]string{deliverMsg.ID},
	)

	// Apply complete to state and dispatch.
	allMsgs, _ = h.st.ListMessages(h.cfID, 0)
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
	sellerBalanceAfter := cs.Balance(h.seller.PublicKeyHex())
	if sellerBalanceAfter != sellerBalanceBefore+expectedResidual {
		t.Errorf("seller balance: got %d, want %d (before=%d + residual=%d)",
			sellerBalanceAfter, sellerBalanceBefore+expectedResidual, sellerBalanceBefore, expectedResidual)
	}

	// Verify exchange revenue was credited to operator.
	expectedExchangeRevenue := salePrice - expectedResidual
	operatorBalanceAfter := cs.Balance(h.operator.PublicKeyHex())
	if operatorBalanceAfter != operatorBalanceBefore+expectedExchangeRevenue {
		t.Errorf("operator balance: got %d, want %d (before=%d + revenue=%d)",
			operatorBalanceAfter, operatorBalanceBefore+expectedExchangeRevenue, operatorBalanceBefore, expectedExchangeRevenue)
	}

	// Reservation must be deleted.
	if _, err := cs.GetReservation(ctx2, resID); err == nil {
		t.Errorf("expected reservation %s to be deleted after settle(complete), still present", resID)
	}
}

// TestRestart_NoDoublePredecrement verifies that dispatchPendingOrders does NOT
// double-charge a buyer whose buy order was pending when the engine restarted.
//
// Scenario (simulates a crash between buy-hold emission and match emission):
//  1. Seed state: inventory entry accepted, buyer has scrip.
//  2. Buyer sends a buy message. Engine would process it but we simulate a crash:
//     we manually insert the dontguess:scrip-buy-hold message into the store
//     WITHOUT a corresponding match message. This is exactly the state left
//     by a crash after buy-hold was persisted but before match was emitted.
//  3. Engine starts (simulating restart): replayAll rebuilds state — buy order
//     is active (no match), CampfireScripStore replays buy-hold (balance decremented).
//  4. dispatchPendingOrders fires, calls handleBuy for the active order.
//  5. BUG (before fix): handleBuy calls DecrementBudget again — double charge.
//     FIX: handleBuy detects the existing buy-hold in the log and skips DecrementBudget.
//
// Done condition: buyer balance after Start() reflects exactly ONE pre-decrement, not two.
func TestRestart_NoDoublePredecrement(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)

	// Use a scrip store to seed balances; we'll reconstruct it after injecting crash state.
	cs0 := newCampfireScripStore(t, h)
	eng0 := exchange.NewEngine(exchange.EngineOptions{
		CampfireID:       h.cfID,
		OperatorIdentity: h.operator,
		Store:            h.st,
		Transport:        h.transport,
		ScripStore:       cs0,
		Logger:           func(format string, args ...any) { t.Logf("[eng0] "+format, args...) },
	})

	// Seed one inventory entry via AutoAcceptPut (real campfire messages).
	seedInventoryEntry(t, h, eng0, "Restart test: go http handler", "code", 8000, 5600)
	inv := eng0.State().Inventory()
	if len(inv) != 1 {
		t.Fatalf("expected 1 inventory entry, got %d", len(inv))
	}
	salePrice := inv[0].PutPrice * 120 / 100 // 6720
	fee := salePrice / exchange.MatchingFeeRate  // 672
	holdAmount := salePrice + fee                // 7392

	// Seed buyer with 2*holdAmount + extraScrip so that after replay (one hold) the balance
	// is holdAmount+extraScrip — still enough for DecrementBudget to succeed if called again.
	// This is the critical condition: if balance after replay is >= holdAmount, the bug
	// causes a second decrement and the buyer ends up at extraScrip instead of holdAmount+extraScrip.
	const extraScrip = int64(3000)
	// Give the buyer 2*holdAmount + extraScrip so that after log replay (which decrements
	// by holdAmount via the injected buy-hold), the balance is holdAmount+extraScrip.
	// That is still >= holdAmount, so WITHOUT the fix, a second DecrementBudget would
	// succeed and take another holdAmount, leaving the buyer with only extraScrip.
	addScripMintMsg(t, h, h.buyer.PublicKeyHex(), 2*holdAmount+extraScrip)

	// Buyer sends a buy message (written directly into the store, simulating
	// a message that arrived before the engine started).
	buyMsg := h.sendMessage(h.buyer,
		buyPayload("Generate Go HTTP handler unit tests", salePrice+5000),
		[]string{exchange.TagBuy},
		nil,
	)

	// Simulate crash: manually inject a scrip-buy-hold message into the campfire store
	// as if the engine processed the buy and wrote the hold but then crashed before
	// writing the match. No exchange:match message is present in the log.
	const crashReservationID = "deadbeefdeadbeefdeadbeefdeadbeef"
	buyHoldPayload, err := json.Marshal(scrip.BuyHoldPayload{
		Buyer:         h.buyer.PublicKeyHex(),
		Amount:        holdAmount,
		Price:         salePrice,
		Fee:           fee,
		ReservationID: crashReservationID,
		BuyMsg:        buyMsg.ID,
		ExpiresAt:     time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339),
	})
	if err != nil {
		t.Fatalf("marshal buy-hold payload: %v", err)
	}
	crashHoldRec := store.MessageRecord{
		ID:         "crash-buy-hold-" + buyMsg.ID,
		CampfireID: h.cfID,
		Sender:     h.operator.PublicKeyHex(), // operator-signed, as the engine would emit
		Payload:    buyHoldPayload,
		Tags:       []string{scrip.TagScripBuyHold},
		Timestamp:  time.Now().UnixNano(),
		ReceivedAt: time.Now().UnixNano(),
		Signature:  []byte{0x01},
	}
	if _, err := h.st.AddMessage(crashHoldRec); err != nil {
		t.Fatalf("inject crash buy-hold message: %v", err)
	}

	// --- Simulate restart ---
	//
	// Fresh CampfireScripStore replays the log on construction.
	// It sees: scrip-mint (buyer gets holdAmount+extraScrip) + scrip-buy-hold (buyer loses holdAmount).
	// Net buyer balance at restart = extraScrip. This is the pre-restart state.
	cs, err := scrip.NewCampfireScripStore(h.cfID, h.st, h.operator.PublicKeyHex())
	if err != nil {
		t.Fatalf("NewCampfireScripStore (restart): %v", err)
	}

	// After replay: mint(2*holdAmount+extraScrip) - buy-hold(holdAmount) = holdAmount+extraScrip.
	balanceAtRestart := cs.Balance(h.buyer.PublicKeyHex())
	if balanceAtRestart != holdAmount+extraScrip {
		t.Fatalf("buyer balance at restart (after log replay): got %d, want %d — buy-hold should be in log",
			balanceAtRestart, holdAmount+extraScrip)
	}

	// Start the engine (simulating a fresh restart).
	// replayAll rebuilds state: buy order is active (no match in log),
	// dispatchPendingOrders calls handleBuy for it.
	// The fix: handleBuy finds the existing buy-hold in the log and skips DecrementBudget.
	eng := exchange.NewEngine(exchange.EngineOptions{
		CampfireID:       h.cfID,
		OperatorIdentity: h.operator,
		Store:            h.st,
		Transport:        h.transport,
		ScripStore:       cs,
		Logger:           func(format string, args ...any) { t.Logf("[eng-restart] "+format, args...) },
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() { _ = eng.Start(ctx) }()

	// Allow time for replayAll + dispatchPendingOrders to complete.
	time.Sleep(400 * time.Millisecond)
	cancel()

	// Buyer's balance must be holdAmount+extraScrip — exactly ONE decrement total (from log replay).
	// If double-charged, it would be extraScrip = holdAmount+extraScrip - holdAmount.
	// With mint=2*holdAmount+extraScrip: replay decrements once → holdAmount+extraScrip.
	// A second decrement in dispatchPendingOrders would take another holdAmount → extraScrip. Wrong.
	balanceAfterRestart := cs.Balance(h.buyer.PublicKeyHex())
	if balanceAfterRestart != holdAmount+extraScrip {
		t.Errorf("buyer balance after restart+dispatchPendingOrders: got %d, want %d (exactly one pre-decrement, not two; double-charge would give %d)",
			balanceAfterRestart, holdAmount+extraScrip, extraScrip)
	}
}

// TestDispute_RefundsScripToBuyer verifies that when a settle(dispute) message
// is dispatched with a valid reservation_id, the engine:
//   - Refunds the full pre-decremented amount to the buyer
//   - Deletes the reservation
func TestDispute_RefundsScripToBuyer(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	cs := newCampfireScripStore(t, h)
	eng := exchange.NewEngine(exchange.EngineOptions{
		CampfireID:       h.cfID,
		OperatorIdentity: h.operator,
		Store:            h.st,
		Transport:        h.transport,
		ScripStore:       cs,
		Logger: func(format string, args ...any) {
			t.Logf("[engine] "+format, args...)
		},
	})

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
	addScripMintMsg(t, h, h.buyer.PublicKeyHex(), holdAmount+5000)
	if err := cs.Replay(); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	buyerBalanceBefore := cs.Balance(h.buyer.PublicKeyHex())

	preMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() { _ = eng.Start(ctx) }()

	h.sendMessage(h.buyer,
		buyPayload("Audit Go HTTP handlers for security issues", salePrice+5000),
		[]string{exchange.TagBuy},
		nil,
	)

	matchMsg := waitForMatchMessage(t, h, preMsgs, 2*time.Second)
	cancel()

	resID := extractReservationID(t, matchMsg)
	if resID == "" {
		t.Fatal("expected non-empty reservation_id in match payload")
	}

	// Buyer balance must be lower by holdAmount now.
	if cs.Balance(h.buyer.PublicKeyHex()) != buyerBalanceBefore-holdAmount {
		t.Errorf("buyer balance after buy: got %d, want %d",
			cs.Balance(h.buyer.PublicKeyHex()), buyerBalanceBefore-holdAmount)
	}

	// Verify reservation exists.
	ctx2 := context.Background()
	if _, err := cs.GetReservation(ctx2, resID); err != nil {
		t.Fatalf("expected reservation %s to exist, got: %v", resID, err)
	}

	// Manually dispatch a settle(dispute) message.
	disputePayload, _ := json.Marshal(map[string]any{
		"reservation_id": resID,
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
	buyerBalanceAfter := cs.Balance(h.buyer.PublicKeyHex())
	if buyerBalanceAfter != buyerBalanceBefore {
		t.Errorf("buyer balance after dispute: got %d, want %d (full refund)",
			buyerBalanceAfter, buyerBalanceBefore)
	}

	// Reservation must be deleted.
	if _, err := cs.GetReservation(ctx2, resID); err == nil {
		t.Errorf("expected reservation %s to be deleted after dispute refund, still present", resID)
	}
}

// TestDispute_MismatchedBuyerKeyRejected verifies that a settle(dispute) message
// with a buyer_key that does not match the reservation's AgentKey is rejected:
//   - handleDispute returns an error
//   - No refund is issued (buyer balance unchanged)
//   - The reservation is NOT deleted (still present)
func TestDispute_MismatchedBuyerKeyRejected(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	cs := newCampfireScripStore(t, h)
	eng := exchange.NewEngine(exchange.EngineOptions{
		CampfireID:       h.cfID,
		OperatorIdentity: h.operator,
		Store:            h.st,
		Transport:        h.transport,
		ScripStore:       cs,
		Logger: func(format string, args ...any) {
			t.Logf("[engine] "+format, args...)
		},
	})

	// Seed inventory entry.
	seedInventoryEntry(t, h, eng, "SQL query optimizer", "code", 10000, 7000)
	inv := eng.State().Inventory()
	if len(inv) != 1 {
		t.Fatalf("expected 1 inventory entry, got %d", len(inv))
	}
	salePrice := inv[0].PutPrice * 120 / 100
	fee := salePrice / exchange.MatchingFeeRate
	holdAmount := salePrice + fee

	// Seed buyer with enough scrip and run a buy to get a valid reservation.
	addScripMintMsg(t, h, h.buyer.PublicKeyHex(), holdAmount+5000)
	if err := cs.Replay(); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	buyerBalanceBefore := cs.Balance(h.buyer.PublicKeyHex())

	preMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() { _ = eng.Start(ctx) }()

	h.sendMessage(h.buyer,
		buyPayload("Optimize a slow SQL query", salePrice+5000),
		[]string{exchange.TagBuy},
		nil,
	)

	matchMsg := waitForMatchMessage(t, h, preMsgs, 2*time.Second)
	cancel()

	resID := extractReservationID(t, matchMsg)
	if resID == "" {
		t.Fatal("expected non-empty reservation_id in match payload")
	}

	// Buyer balance is now pre-decremented.
	buyerBalanceAfterBuy := cs.Balance(h.buyer.PublicKeyHex())
	if buyerBalanceAfterBuy != buyerBalanceBefore-holdAmount {
		t.Errorf("buyer balance after buy: got %d, want %d", buyerBalanceAfterBuy, buyerBalanceBefore-holdAmount)
	}

	// Dispatch a settle(dispute) with a WRONG buyer_key (the seller's key).
	// This simulates a crafted operator message attempting to redirect the refund.
	mismatchedBuyerKey := h.seller.PublicKeyHex()
	disputePayload, _ := json.Marshal(map[string]any{
		"reservation_id": resID,
		"buyer_key":      mismatchedBuyerKey, // wrong — should be h.buyer.PublicKeyHex()
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

	// DispatchForTest must return an error for the mismatched buyer_key.
	dispatchErr := eng.DispatchForTest(rec)
	if dispatchErr == nil {
		t.Error("expected error from handleDispute with mismatched buyer_key, got nil")
	}

	ctx2 := context.Background()

	// Buyer balance must be UNCHANGED — no refund was issued.
	buyerBalanceAfterDispute := cs.Balance(h.buyer.PublicKeyHex())
	if buyerBalanceAfterDispute != buyerBalanceAfterBuy {
		t.Errorf("buyer balance changed after rejected dispute: got %d, want %d (no refund expected)",
			buyerBalanceAfterDispute, buyerBalanceAfterBuy)
	}

	// The mismatched identity (seller) must NOT have received any scrip.
	sellerBalance := cs.Balance(h.seller.PublicKeyHex())
	if sellerBalance != 0 {
		t.Errorf("seller (mismatched key) received unexpected scrip: got %d, want 0", sellerBalance)
	}

	// The reservation must still exist (not deleted after the rejected dispute).
	if _, err := cs.GetReservation(ctx2, resID); err != nil {
		t.Errorf("reservation %s should still exist after rejected dispute, got: %v", resID, err)
	}
}

// TestDispute_ReservationRestoredAfterMismatch verifies that when a settle(dispute)
// message carries a buyer_key that does NOT match the reservation's AgentKey:
//  1. The dispute is rejected (error returned)
//  2. The reservation is restored so the legitimate owner can still claim a refund
//  3. A subsequent valid dispute (correct buyer_key) succeeds and refunds the buyer
//
// This test catches regressions in both the mismatch gate and the reservation
// restore path — ensuring the legitimate buyer is not permanently locked out of
// their escrowed scrip by an attacker's failed hijack attempt.
func TestDispute_ReservationRestoredAfterMismatch(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	cs := newCampfireScripStore(t, h)
	eng := exchange.NewEngine(exchange.EngineOptions{
		CampfireID:       h.cfID,
		OperatorIdentity: h.operator,
		Store:            h.st,
		Transport:        h.transport,
		ScripStore:       cs,
		Logger:           func(format string, args ...any) { t.Logf("[engine] "+format, args...) },
	})

	// Seed inventory entry.
	seedInventoryEntry(t, h, eng, "API rate limiter implementation", "code", 12000, 8400)
	inv := eng.State().Inventory()
	if len(inv) != 1 {
		t.Fatalf("expected 1 inventory entry, got %d", len(inv))
	}
	salePrice := inv[0].PutPrice * 120 / 100
	fee := salePrice / exchange.MatchingFeeRate
	holdAmount := salePrice + fee

	// Seed buyer with enough scrip to buy.
	addScripMintMsg(t, h, h.buyer.PublicKeyHex(), holdAmount+5000)
	if err := cs.Replay(); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	buyerBalanceBefore := cs.Balance(h.buyer.PublicKeyHex())

	// Run the engine to generate a match and reservation.
	preMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() { _ = eng.Start(ctx) }()

	h.sendMessage(h.buyer,
		buyPayload("Implement token-bucket rate limiter", salePrice+5000),
		[]string{exchange.TagBuy},
		nil,
	)
	matchMsg := waitForMatchMessage(t, h, preMsgs, 2*time.Second)
	cancel()

	resID := extractReservationID(t, matchMsg)
	if resID == "" {
		t.Fatal("expected non-empty reservation_id in match payload")
	}

	// Buyer's scrip is now pre-decremented.
	buyerBalanceAfterBuy := cs.Balance(h.buyer.PublicKeyHex())
	if buyerBalanceAfterBuy != buyerBalanceBefore-holdAmount {
		t.Errorf("buyer balance after buy: got %d, want %d", buyerBalanceAfterBuy, buyerBalanceBefore-holdAmount)
	}

	ctx2 := context.Background()

	// Phase 1: Attacker sends a dispute with mismatched buyer_key (seller's key).
	// This must be rejected and the reservation must be restored.
	attackerKey := h.seller.PublicKeyHex()
	mismatchPayload, _ := json.Marshal(map[string]any{
		"reservation_id": resID,
		"buyer_key":      attackerKey, // wrong — attacker trying to hijack the refund
		"entry_id":       inv[0].EntryID,
		"dispute_type":   "quality",
	})
	mismatchMsg := h.sendMessage(h.operator, mismatchPayload,
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrDispute,
		},
		nil,
	)
	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(allMsgs)
	rec, err := h.st.GetMessage(mismatchMsg.ID)
	if err != nil {
		t.Fatalf("GetMessage (mismatch): %v", err)
	}
	mismatchErr := eng.DispatchForTest(rec)
	if mismatchErr == nil {
		t.Error("expected error from handleDispute with mismatched buyer_key, got nil")
	}

	// No scrip must flow to the attacker.
	if cs.Balance(attackerKey) != 0 {
		t.Errorf("attacker (mismatched key) received scrip: got %d, want 0", cs.Balance(attackerKey))
	}
	// Buyer's balance must be unchanged after the rejected dispute.
	if cs.Balance(h.buyer.PublicKeyHex()) != buyerBalanceAfterBuy {
		t.Errorf("buyer balance changed after rejected dispute: got %d, want %d",
			cs.Balance(h.buyer.PublicKeyHex()), buyerBalanceAfterBuy)
	}
	// The reservation must have been restored so the legitimate owner can still dispute.
	if _, err := cs.GetReservation(ctx2, resID); err != nil {
		t.Errorf("reservation %s must be present after rejected dispute (restore failed?): %v", resID, err)
	}

	// Phase 2: Legitimate buyer disputes with the correct key.
	// The reservation must still be usable — scrip must be refunded to res.AgentKey.
	validPayload, _ := json.Marshal(map[string]any{
		"reservation_id": resID,
		"buyer_key":      h.buyer.PublicKeyHex(), // correct key matches res.AgentKey
		"entry_id":       inv[0].EntryID,
		"dispute_type":   "quality",
	})
	validMsg := h.sendMessage(h.operator, validPayload,
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrDispute,
		},
		nil,
	)
	allMsgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(allMsgs)
	rec2, err := h.st.GetMessage(validMsg.ID)
	if err != nil {
		t.Fatalf("GetMessage (valid): %v", err)
	}
	if err := eng.DispatchForTest(rec2); err != nil {
		t.Fatalf("valid dispute after mismatch rejected: %v — reservation may not have been restored", err)
	}

	// Buyer's balance must be fully restored to pre-buy level.
	buyerFinal := cs.Balance(h.buyer.PublicKeyHex())
	if buyerFinal != buyerBalanceBefore {
		t.Errorf("buyer balance after valid dispute: got %d, want %d (full restore to pre-buy)",
			buyerFinal, buyerBalanceBefore)
	}

	// Attacker must still have received nothing — refund went to res.AgentKey (buyer), not attacker.
	if cs.Balance(attackerKey) != 0 {
		t.Errorf("attacker received scrip after valid dispute: got %d, want 0", cs.Balance(attackerKey))
	}

	// Reservation must be deleted after the successful refund.
	if _, err := cs.GetReservation(ctx2, resID); err == nil {
		t.Errorf("reservation %s must be deleted after successful dispute refund", resID)
	}
}

// TestSettle_FakeSellerKeyIgnored verifies that a malicious buyer cannot redirect
// residual payment by injecting a fake seller_key into the settle(complete) payload
// (security fix rudi-x3y).
//
// The complete message includes a seller_key pointing to an attacker-controlled address.
// The engine must derive the real seller from the antecedent chain (complete → deliver
// → buyer-accept → match → entry → SellerKey) and pay the real seller, ignoring
// the tainted payload field entirely.
func TestSettle_FakeSellerKeyIgnored(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	cs := newCampfireScripStore(t, h)
	eng := exchange.NewEngine(exchange.EngineOptions{
		CampfireID:       h.cfID,
		OperatorIdentity: h.operator,
		Store:            h.st,
		Transport:        h.transport,
		ScripStore:       cs,
		Logger:           func(format string, args ...any) { t.Logf("[engine] "+format, args...) },
	})

	// Generate an attacker identity (not part of the exchange, unknown to the engine).
	attacker, err := identity.Generate()
	if err != nil {
		t.Fatalf("generate attacker identity: %v", err)
	}

	// Seed inventory entry (real seller = h.seller).
	seedInventoryEntry(t, h, eng, "Go HTTP handler unit test generator", "code", 8000, 5600)
	inv := eng.State().Inventory()
	if len(inv) != 1 {
		t.Fatalf("expected 1 inventory entry, got %d", len(inv))
	}
	salePrice := inv[0].PutPrice * 120 / 100           // 6720
	fee := salePrice / exchange.MatchingFeeRate         // 672
	holdAmount := salePrice + fee                       // 7392
	expectedResidual := salePrice / exchange.ResidualRate

	// Seed buyer with sufficient scrip.
	addScripMintMsg(t, h, h.buyer.PublicKeyHex(), holdAmount+5000)
	if err := cs.Replay(); err != nil {
		t.Fatalf("Replay: %v", err)
	}

	// Run engine to emit a match (buyer buys, engine pre-decrements and matches).
	preMatchMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() { _ = eng.Start(ctx) }()

	h.sendMessage(h.buyer,
		buyPayload("Generate unit tests for Go HTTP handler", salePrice+5000),
		[]string{exchange.TagBuy},
		nil,
	)

	matchMsg := waitForMatchMessage(t, h, preMatchMsgs, 2*time.Second)
	cancel()

	resID := extractReservationID(t, matchMsg)
	if resID == "" {
		t.Fatal("expected non-empty reservation_id in match payload")
	}

	// Record baseline balances.
	realSellerBefore := cs.Balance(h.seller.PublicKeyHex())
	attackerBefore := cs.Balance(attacker.PublicKeyHex())

	// Build the antecedent chain.
	// buyer-accept (antecedent = match message).
	buyerAcceptP, _ := json.Marshal(map[string]any{
		"phase":    "buyer-accept",
		"entry_id": inv[0].EntryID,
		"accepted": true,
	})
	buyerAcceptMsg := h.sendMessage(h.buyer, buyerAcceptP,
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrBuyerAccept,
			exchange.TagVerdictPrefix + "accepted",
		},
		[]string{matchMsg.ID},
	)

	// deliver (antecedent = buyer-accept message).
	deliverP, _ := json.Marshal(map[string]any{
		"phase":        "deliver",
		"entry_id":     inv[0].EntryID,
		"content_ref":  "sha256:" + fmt.Sprintf("%064x", 1),
		"content_size": int64(20000),
	})
	deliverMsg := h.sendMessage(h.operator, deliverP,
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrDeliver,
		},
		[]string{buyerAcceptMsg.ID},
	)

	// Replay so antecedent chain is in state.
	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(allMsgs)

	// Attacker-controlled complete message: seller_key points to attacker.
	completeP, _ := json.Marshal(map[string]any{
		"reservation_id": resID,
		"seller_key":     attacker.PublicKeyHex(), // FAKE: attacker tries to redirect payment
		"price":          salePrice,
		"entry_id":       inv[0].EntryID,
	})
	completeMsg := h.sendMessage(h.buyer, completeP,
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrComplete,
		},
		[]string{deliverMsg.ID}, // correct antecedent chain
	)

	// Replay and dispatch the complete message.
	allMsgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(allMsgs)
	rec, err := h.st.GetMessage(completeMsg.ID)
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if err := eng.DispatchForTest(rec); err != nil {
		t.Fatalf("dispatch settle(complete): %v", err)
	}

	// The REAL seller must receive the residual.
	realSellerAfter := cs.Balance(h.seller.PublicKeyHex())
	if realSellerAfter != realSellerBefore+expectedResidual {
		t.Errorf("real seller balance: got %d, want %d (before=%d + residual=%d)",
			realSellerAfter, realSellerBefore+expectedResidual, realSellerBefore, expectedResidual)
	}

	// The ATTACKER must receive nothing.
	attackerAfter := cs.Balance(attacker.PublicKeyHex())
	if attackerAfter != attackerBefore {
		t.Errorf("attacker balance: got %d, want %d (no payment should flow to fake seller)",
			attackerAfter, attackerBefore)
	}
}
