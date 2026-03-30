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
	"github.com/campfire-net/campfire/pkg/protocol"
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

// addScripPutPayMsg emits a scrip-put-pay message for a seller who submitted a put.
// The antecedent is the put message ID being paid.
func addScripPutPayMsg(t *testing.T, h *testHarness, putMsgID, seller string, amount, tokenCost int64, discountPct int64, resultHash string) {
	t.Helper()
	rawPayload, err := json.Marshal(map[string]any{
		"seller":       seller,
		"amount":       amount,
		"token_cost":   tokenCost,
		"discount_pct": discountPct,
		"result_hash":  resultHash,
		"put_msg":      putMsgID,
	})
	if err != nil {
		t.Fatalf("marshal scrip-put-pay payload: %v", err)
	}
	rec := store.MessageRecord{
		ID:         fmt.Sprintf("put-pay-%s-%d-%d", seller[:8], amount, time.Now().UnixNano()),
		CampfireID: h.cfID,
		Sender:     h.operator.PublicKeyHex(),
		Payload:    rawPayload,
		Tags:       []string{scrip.TagScripPutPay},
		Antecedents: []string{putMsgID},
		Timestamp:  time.Now().UnixNano(),
		ReceivedAt: time.Now().UnixNano(),
		Signature:  []byte{0x00}, // non-nil to satisfy schema NOT NULL constraint
	}
	if _, err := h.st.AddMessage(rec); err != nil {
		t.Fatalf("AddMessage (scrip-put-pay): %v", err)
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
		ReadClient:       protocol.New(h.st, h.operator),
		WriteClient:      protocol.New(h.st, h.operator),
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
	eng.State().Replay(exchange.FromStoreRecords(msgs))
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

// extractReservationIDFromLog scans the campfire log for the most recent
// scrip-buy-hold message and returns its reservation_id.
// Used after a buyer-accept step triggers the scrip hold (reservation is no
// longer in the match payload — it is created at buyer-accept time).
func extractReservationIDFromLog(t *testing.T, h *testHarness) string {
	t.Helper()
	msgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{scrip.TagScripBuyHold}})
	if len(msgs) == 0 {
		return ""
	}
	last := msgs[len(msgs)-1]
	var p scrip.BuyHoldPayload
	if err := json.Unmarshal(last.Payload, &p); err != nil {
		t.Fatalf("parsing scrip-buy-hold payload: %v", err)
	}
	return p.ReservationID
}

// sendBuyerAccept sends a settle(buyer-accept) message and dispatches it via
// DispatchForTest, triggering the scrip hold. Returns the buyer-accept message.
// The antecedent is matchMsgID (direct match path, no preview).
func sendBuyerAcceptAndDispatch(t *testing.T, h *testHarness, eng *exchange.Engine, matchMsgID, entryID string) *exchange.Message {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{
		"phase":    "buyer-accept",
		"entry_id": entryID,
		"accepted": true,
	})
	msg := h.sendMessage(h.buyer, payload,
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrBuyerAccept,
			exchange.TagVerdictPrefix + "accepted",
		},
		[]string{matchMsgID},
	)
	// Replay state so engine sees the buyer-accept before dispatching.
	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))
	rec, err := h.st.GetMessage(msg.ID)
	if err != nil {
		t.Fatalf("GetMessage buyer-accept: %v", err)
	}
	if err := eng.DispatchForTest(exchange.FromStoreRecord(rec)); err != nil {
		t.Fatalf("DispatchForTest buyer-accept: %v", err)
	}
	return msg
}

// waitForBuyHoldMessage polls until a new scrip-buy-hold message appears, returns the last one.
func waitForBuyHoldMessage(t *testing.T, h *testHarness, before []store.MessageRecord, timeout time.Duration) *store.MessageRecord {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		msgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{scrip.TagScripBuyHold}})
		if len(msgs) > len(before) {
			last := msgs[len(msgs)-1]
			return &last
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("timed out waiting for scrip-buy-hold message")
	return nil
}

// --- Tests ---

// TestBuyerAccept_DecrementsScripAfterPreview verifies that a buyer-accept with
// sufficient scrip causes the engine to decrement the buyer's balance by (price + fee)
// and emit a scrip-buy-hold convention message. The buy step itself does NOT lock scrip;
// scrip is locked only when the buyer accepts after reviewing the preview.
func TestBuyerAccept_DecrementsScripAfterPreview(t *testing.T) {
	t.Parallel()

	// Build harness first so we know the campfire ID and buyer key.
	h := newTestHarness(t)

	// Seed one inventory entry; put_price = 5600, computed sale price = 5600*120/100 = 6720.
	cs := newCampfireScripStore(t, h)
	eng := exchange.NewEngine(exchange.EngineOptions{
		CampfireID:       h.cfID,
		OperatorIdentity: h.operator,
		Store:            h.st,
		ReadClient:  protocol.New(h.st, h.operator),
		WriteClient:      protocol.New(h.st, h.operator),
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
	salePrice := eng.ComputePriceForTest(inv[0])
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

	// Buyer balance must be UNCHANGED after buy — scrip not locked yet.
	buyerBalanceAfterBuy := cs.Balance(h.buyer.PublicKeyHex())
	if buyerBalanceAfterBuy != buyerBalanceBefore {
		t.Errorf("buyer balance after buy: got %d, want %d (no hold at buy time)",
			buyerBalanceAfterBuy, buyerBalanceBefore)
	}

	// Match payload must NOT include reservation_id (hold not created yet).
	var mp struct {
		SearchMeta struct {
			ReservationID string `json:"reservation_id"`
		} `json:"search_meta"`
	}
	_ = json.Unmarshal(matchMsg.Payload, &mp)
	if mp.SearchMeta.ReservationID != "" {
		t.Error("match search_meta.reservation_id must be empty — scrip hold not created at buy time")
	}

	// Buyer sends buyer-accept → engine creates scrip hold.
	preBuyHoldMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{scrip.TagScripBuyHold}})
	sendBuyerAcceptAndDispatch(t, h, eng, matchMsg.ID, inv[0].EntryID)

	// Buyer balance must have decreased by holdAmount.
	buyerBalanceAfter := cs.Balance(h.buyer.PublicKeyHex())
	if buyerBalanceAfter != buyerBalanceBefore-holdAmount {
		t.Errorf("buyer balance after buyer-accept: got %d, want %d (before=%d - hold=%d)",
			buyerBalanceAfter, buyerBalanceBefore-holdAmount, buyerBalanceBefore, holdAmount)
	}

	// A scrip-buy-hold message must be in the log.
	afterBuyHoldMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{scrip.TagScripBuyHold}})
	if len(afterBuyHoldMsgs) <= len(preBuyHoldMsgs) {
		t.Error("scrip-buy-hold message must be emitted after buyer-accept")
	}

	// A reservation must exist for the hold.
	resID := extractReservationIDFromLog(t, h)
	if resID == "" {
		t.Error("reservation_id must be non-empty after buyer-accept scrip hold")
	}
	if _, err := cs.GetReservation(context.Background(), resID); err != nil {
		t.Errorf("expected reservation %s to exist, got: %v", resID, err)
	}
}

// TestBuyerAccept_InsufficientScripReturnsError verifies that a buyer-accept with
// insufficient scrip causes the engine to return an error and NOT create a reservation.
// The buy step itself succeeds (no balance check at buy time) — only buyer-accept checks.
func TestBuyerAccept_InsufficientScripReturnsError(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	cs := newCampfireScripStore(t, h)
	eng := exchange.NewEngine(exchange.EngineOptions{
		CampfireID:       h.cfID,
		OperatorIdentity: h.operator,
		Store:            h.st,
		ReadClient:  protocol.New(h.st, h.operator),
		WriteClient:      protocol.New(h.st, h.operator),
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
	salePrice := eng.ComputePriceForTest(inv[0])
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

	// Buy should succeed — a match message should appear.
	matchMsg := waitForMatchMessage(t, h, preMsgs, 2*time.Second)
	cancel()

	// Buyer balance must be unchanged after buy (no hold at buy time).
	if cs.Balance(h.buyer.PublicKeyHex()) != buyerBalanceBefore {
		t.Errorf("buyer balance changed after buy (should not): got %d, want %d",
			cs.Balance(h.buyer.PublicKeyHex()), buyerBalanceBefore)
	}

	// Send buyer-accept — this should fail because buyer has insufficient scrip.
	preBuyHoldMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{scrip.TagScripBuyHold}})

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
	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))
	rec, err := h.st.GetMessage(buyerAcceptMsg.ID)
	if err != nil {
		t.Fatalf("GetMessage buyer-accept: %v", err)
	}
	dispatchErr := eng.DispatchForTest(exchange.FromStoreRecord(rec))
	if dispatchErr == nil {
		t.Error("expected error from buyer-accept with insufficient scrip, got nil")
	}

	// No scrip-buy-hold message should have been emitted.
	afterBuyHoldMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{scrip.TagScripBuyHold}})
	if len(afterBuyHoldMsgs) > len(preBuyHoldMsgs) {
		t.Error("expected no scrip-buy-hold message when buyer has insufficient scrip")
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
		ReadClient:  protocol.New(h.st, h.operator),
		WriteClient:      protocol.New(h.st, h.operator),
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
	salePrice := eng.ComputePriceForTest(inv[0])
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

	sellerBalanceBefore := cs.Balance(h.seller.PublicKeyHex())
	operatorBalanceBefore := cs.Balance(h.operator.PublicKeyHex())

	// Build the antecedent chain required for seller derivation:
	//   complete → deliver → match (via buyer-accept) → entry → seller

	// buyer-accept (antecedent = match message) — this triggers the scrip hold.
	buyerAcceptMsg := sendBuyerAcceptAndDispatch(t, h, eng, matchMsg.ID, inv[0].EntryID)

	// Get the reservation ID from the scrip-buy-hold message emitted during buyer-accept.
	resID := extractReservationIDFromLog(t, h)
	if resID == "" {
		t.Fatal("expected non-empty reservation_id after buyer-accept scrip hold")
	}

	// Reservation must exist.
	ctx2 := context.Background()
	if _, err := cs.GetReservation(ctx2, resID); err != nil {
		t.Fatalf("expected reservation %s to exist, got: %v", resID, err)
	}

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
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	// complete (antecedent = deliver message).
	// Note: reservation_id is NOT in the complete payload — it is looked up by the
	// engine via matchToReservation[matchMsgID] derived from the antecedent chain.
	completePayload, _ := json.Marshal(map[string]any{
		"price":    salePrice,
		"entry_id": inv[0].EntryID,
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
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))
	rec, err := h.st.GetMessage(completeMsg.ID)
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if err := eng.DispatchForTest(exchange.FromStoreRecord(rec)); err != nil {
		t.Fatalf("dispatch settle(complete): %v", err)
	}

	// Verify residual was paid to seller.
	enginePrice := holdAmount * exchange.MatchingFeeRate / (exchange.MatchingFeeRate + 1)
	expectedResidual := enginePrice / exchange.ResidualRate
	sellerBalanceAfter := cs.Balance(h.seller.PublicKeyHex())
	if sellerBalanceAfter != sellerBalanceBefore+expectedResidual {
		t.Errorf("seller balance: got %d, want %d (before=%d + residual=%d)",
			sellerBalanceAfter, sellerBalanceBefore+expectedResidual, sellerBalanceBefore, expectedResidual)
	}

	// Verify exchange revenue was credited to operator.
	expectedExchangeRevenue := enginePrice - expectedResidual
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

// TestRestart_NoDoubleHoldOnBuyerAccept verifies that if a scrip-buy-hold was
// already written to the campfire log for a buyer-accept (e.g., emitted before
// the engine crashed and restarted), re-dispatching the buyer-accept does NOT
// issue a second DecrementBudget call.
//
// Scenario (simulates a crash between buy-hold emission and deliver/complete):
//  1. Seed state: inventory entry accepted, buyer has scrip; buy → match in log.
//  2. Buyer sends a buyer-accept message. Engine writes scrip-buy-hold but crashes.
//     Inject a scrip-buy-hold message into the log manually (buy-hold anchored to
//     match message ID), with no deliver or complete in the log yet.
//  3. Engine restarts: CampfireScripStore replays log → buyer balance decremented once.
//  4. dispatchPendingOrders fires. The buy order is already matched, so handleBuy skips.
//     But state replays the buyer-accept, which triggers handleSettleBuyerAcceptScrip.
//     BUG (without fix): DecrementBudget fires again → double charge.
//     FIX: findExistingBuyerAcceptHold finds the log entry and restores reservation.
//
// Done condition: buyer balance reflects exactly ONE decrement, not two.
func TestRestart_NoDoubleHoldOnBuyerAccept(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)

	cs0 := newCampfireScripStore(t, h)
	eng0 := exchange.NewEngine(exchange.EngineOptions{
		CampfireID:       h.cfID,
		OperatorIdentity: h.operator,
		Store:            h.st,
		ReadClient:  protocol.New(h.st, h.operator),
		WriteClient:      protocol.New(h.st, h.operator),
		ScripStore:       cs0,
		Logger:           func(format string, args ...any) { t.Logf("[eng0] "+format, args...) },
	})

	// Seed one inventory entry.
	seedInventoryEntry(t, h, eng0, "Restart test: buyer-accept hold", "code", 8000, 5600)
	inv := eng0.State().Inventory()
	if len(inv) != 1 {
		t.Fatalf("expected 1 inventory entry, got %d", len(inv))
	}
	salePrice := eng0.ComputePriceForTest(inv[0])
	fee := salePrice / exchange.MatchingFeeRate  // 672
	holdAmount := salePrice + fee                // 7392

	// Seed buyer with 2*holdAmount + extraScrip so that after replay (one hold) the
	// balance is holdAmount+extraScrip — still >= holdAmount, so without the fix a
	// second DecrementBudget would succeed and double-charge the buyer.
	const extraScrip = int64(3000)
	addScripMintMsg(t, h, h.buyer.PublicKeyHex(), 2*holdAmount+extraScrip)

	// Run a buy → match to get a real match message in the log.
	preMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})

	ctx0, cancel0 := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel0()
	go func() { _ = eng0.Start(ctx0) }()

	h.sendMessage(h.buyer,
		buyPayload("Restart test Go HTTP handler", salePrice+5000),
		[]string{exchange.TagBuy},
		nil,
	)
	matchMsg := waitForMatchMessage(t, h, preMsgs, 2*time.Second)
	cancel0()

	// Buyer sends a buyer-accept message into the log (not yet dispatched).
	buyerAcceptPayload0, _ := json.Marshal(map[string]any{
		"phase":    "buyer-accept",
		"entry_id": inv[0].EntryID,
		"accepted": true,
	})
	buyerAcceptMsg := h.sendMessage(h.buyer, buyerAcceptPayload0,
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrBuyerAccept,
			exchange.TagVerdictPrefix + "accepted",
		},
		[]string{matchMsg.ID},
	)
	_ = buyerAcceptMsg

	// Simulate crash: manually inject a scrip-buy-hold message as if the engine wrote
	// it for the buyer-accept but crashed before completing the flow.
	// BuyMsg = matchMsg.ID (the anchor used by findExistingBuyerAcceptHold).
	const crashReservationID = "deadbeefdeadbeefdeadbeefdeadbeef"
	buyHoldPayload, err := json.Marshal(scrip.BuyHoldPayload{
		Buyer:         h.buyer.PublicKeyHex(),
		Amount:        holdAmount,
		Price:         salePrice,
		Fee:           fee,
		ReservationID: crashReservationID,
		BuyMsg:        matchMsg.ID, // anchored to match message ID (buyer-accept convention)
		ExpiresAt:     time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339),
	})
	if err != nil {
		t.Fatalf("marshal buy-hold payload: %v", err)
	}
	crashHoldRec := store.MessageRecord{
		ID:         "crash-buy-hold-" + matchMsg.ID,
		CampfireID: h.cfID,
		Sender:     h.operator.PublicKeyHex(),
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
	// Fresh CampfireScripStore replays: mint(2*holdAmount+extraScrip) - buy-hold(holdAmount).
	// => balance = holdAmount+extraScrip.
	cs, err := scrip.NewCampfireScripStore(h.cfID, h.st, h.operator.PublicKeyHex())
	if err != nil {
		t.Fatalf("NewCampfireScripStore (restart): %v", err)
	}
	balanceAtRestart := cs.Balance(h.buyer.PublicKeyHex())
	if balanceAtRestart != holdAmount+extraScrip {
		t.Fatalf("buyer balance at restart (after log replay): got %d, want %d",
			balanceAtRestart, holdAmount+extraScrip)
	}

	// Start the restarted engine. It will replay all messages including buyer-accept,
	// which triggers handleSettleBuyerAcceptScrip.
	// The fix: findExistingBuyerAcceptHold finds the log entry and skips DecrementBudget.
	eng := exchange.NewEngine(exchange.EngineOptions{
		CampfireID:       h.cfID,
		OperatorIdentity: h.operator,
		Store:            h.st,
		ReadClient:  protocol.New(h.st, h.operator),
		WriteClient:      protocol.New(h.st, h.operator),
		ScripStore:       cs,
		Logger:           func(format string, args ...any) { t.Logf("[eng-restart] "+format, args...) },
	})

	// Replay state so the engine processes the buyer-accept during dispatch.
	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	// Dispatch the buyer-accept: the fix should detect the existing buy-hold and skip.
	rec, err := h.st.GetMessage(buyerAcceptMsg.ID)
	if err != nil {
		t.Fatalf("GetMessage buyer-accept: %v", err)
	}
	if err := eng.DispatchForTest(exchange.FromStoreRecord(rec)); err != nil {
		// An error here means the double-charge path failed the second DecrementBudget
		// (which would happen if balance was already at 0 or negative). Not the test
		// scenario we want, but still an error to surface.
		t.Fatalf("DispatchForTest buyer-accept on restart: %v", err)
	}

	// Buyer's balance must be holdAmount+extraScrip — exactly ONE decrement total.
	// If double-charged: holdAmount+extraScrip - holdAmount = extraScrip.
	balanceAfterRestart := cs.Balance(h.buyer.PublicKeyHex())
	if balanceAfterRestart != holdAmount+extraScrip {
		t.Errorf("buyer balance after restart dispatch: got %d, want %d (exactly one hold, not two; double-charge would give %d)",
			balanceAfterRestart, holdAmount+extraScrip, extraScrip)
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
		ReadClient:  protocol.New(h.st, h.operator),
		WriteClient:      protocol.New(h.st, h.operator),
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
	salePrice := eng.ComputePriceForTest(inv[0])
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

	// Record baseline balances.
	realSellerBefore := cs.Balance(h.seller.PublicKeyHex())
	attackerBefore := cs.Balance(attacker.PublicKeyHex())

	// Build the antecedent chain.
	// buyer-accept (antecedent = match message) — triggers scrip hold.
	buyerAcceptMsg := sendBuyerAcceptAndDispatch(t, h, eng, matchMsg.ID, inv[0].EntryID)

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
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	// Attacker-controlled complete message: seller_key points to attacker.
	// Note: reservation_id is NOT in the complete payload — engine looks it up internally.
	// The engine must ignore the fake seller_key and use the antecedent chain instead.
	completeP, _ := json.Marshal(map[string]any{
		"seller_key": attacker.PublicKeyHex(), // FAKE: attacker tries to redirect payment
		"price":      salePrice,
		"entry_id":   inv[0].EntryID,
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
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))
	rec, err := h.st.GetMessage(completeMsg.ID)
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if err := eng.DispatchForTest(exchange.FromStoreRecord(rec)); err != nil {
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

