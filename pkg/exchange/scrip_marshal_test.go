package exchange_test

// Tests for marshal failure handling in scrip paths.
//
// These tests verify that json.Marshal errors occurring in the buy-hold,
// settle, and dispute-refund paths are caught BEFORE any scrip balance
// mutation occurs. Prior to the fix, marshal errors were swallowed via
// `if err == nil { emit() }` — balance mutations had already happened,
// leaving scrip state inconsistent with the campfire message log.
//
// The fix marshals convention messages BEFORE calling AddBudget /
// DecrementBudget. These tests inject a failing marshal function via
// SetMarshalFuncForTest and assert that:
//   - handleBuy: returns an error; buyer balance is not decremented
//   - handleSettle: returns an error; seller and operator balances are not credited
//   - handleDispute: returns an error; buyer balance is not credited; reservation is restored

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/campfire-net/campfire/pkg/store"

	"github.com/3dl-dev/dontguess/pkg/exchange"
)

var errMarshalInjected = errors.New("injected marshal failure")

// failMarshal is a marshal function that always returns an error.
func failMarshal(v any) ([]byte, error) {
	return nil, errMarshalInjected
}

// TestBuy_MarshalFailure_NoBudgetDecrement verifies that if the scrip-buy-hold
// convention message cannot be marshalled, no balance decrement occurs.
func TestBuy_MarshalFailure_NoBudgetDecrement(t *testing.T) {
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

	// Seed one inventory entry; put_price = 5600, sale_price = 6720.
	seedInventoryEntry(t, h, eng, "Go HTTP handler generator for test", "code", 8000, 5600)
	inv := eng.State().Inventory()
	if len(inv) != 1 {
		t.Fatalf("expected 1 inventory entry, got %d", len(inv))
	}
	salePrice := inv[0].PutPrice * 120 / 100
	fee := salePrice / exchange.MatchingFeeRate
	holdAmount := salePrice + fee

	// Seed buyer with sufficient scrip.
	addScripMintMsg(t, h, h.buyer.PublicKeyHex(), holdAmount+5000)
	if err := cs.Replay(); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	balanceBefore := cs.Balance(h.buyer.PublicKeyHex())

	// Inject buy message into the store.
	buyMsg := h.sendMessage(h.buyer,
		buyPayload("Generate Go HTTP handler unit tests", salePrice+5000),
		[]string{exchange.TagBuy},
		nil,
	)

	// Reload engine state so the buy message is visible.
	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(allMsgs)

	// Inject a failing marshal function.
	eng.SetMarshalFuncForTest(failMarshal)

	rec, err := h.st.GetMessage(buyMsg.ID)
	if err != nil || rec == nil {
		t.Fatalf("GetMessage buy: %v", err)
	}

	dispatchErr := eng.DispatchForTest(rec)
	if dispatchErr == nil {
		t.Fatal("expected handleBuy to return error on marshal failure, got nil")
	}
	if !errors.Is(dispatchErr, errMarshalInjected) {
		t.Errorf("expected errMarshalInjected in error chain, got: %v", dispatchErr)
	}

	// Buyer balance must be unchanged — no decrement occurred.
	balanceAfter := cs.Balance(h.buyer.PublicKeyHex())
	if balanceAfter != balanceBefore {
		t.Errorf("buyer balance changed: before=%d after=%d (expected no change on marshal failure)",
			balanceBefore, balanceAfter)
	}
}

// TestSettle_MarshalFailure_NoBalanceMutation verifies that if the scrip-settle
// convention message cannot be marshalled, neither the seller residual nor the
// operator exchange revenue is credited.
func TestSettle_MarshalFailure_NoBalanceMutation(t *testing.T) {
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

	// Seed inventory; put_price = 5600, sale_price = 6720.
	seedInventoryEntry(t, h, eng, "Terraform module generator for test", "code", 8000, 5600)
	inv := eng.State().Inventory()
	if len(inv) != 1 {
		t.Fatalf("expected 1 inventory entry, got %d", len(inv))
	}
	salePrice := inv[0].PutPrice * 120 / 100
	fee := salePrice / exchange.MatchingFeeRate
	holdAmount := salePrice + fee

	// Seed buyer and run a buy to establish a reservation.
	addScripMintMsg(t, h, h.buyer.PublicKeyHex(), holdAmount+5000)
	if err := cs.Replay(); err != nil {
		t.Fatalf("Replay: %v", err)
	}

	preMatch, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() { _ = eng.Start(ctx) }()

	h.sendMessage(h.buyer,
		buyPayload("Generate Terraform S3 module for test", salePrice+5000),
		[]string{exchange.TagBuy},
		nil,
	)

	matchMsg := waitForMatchMessage(t, h, preMatch, 2*time.Second)
	cancel()

	resID := extractReservationID(t, matchMsg)
	if resID == "" {
		t.Fatal("expected non-empty reservation_id")
	}

	// Build antecedent chain: buyer-accept → deliver (to establish seller identity).
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
	deliverP, _ := json.Marshal(map[string]any{
		"phase":        "deliver",
		"entry_id":     inv[0].EntryID,
		"content_ref":  "sha256:" + fmt.Sprintf("%064x", 1),
		"content_size": int64(20000),
	})
	deliverMsgRec := h.sendMessage(h.operator, deliverP,
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrDeliver,
		},
		[]string{buyerAcceptMsg.ID},
	)

	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(allMsgs)

	// Record balances before settle dispatch.
	sellerBalBefore := cs.Balance(h.seller.PublicKeyHex())
	operatorBalBefore := cs.Balance(h.operator.PublicKeyHex())

	// Inject failing marshal, then dispatch settle(complete).
	eng.SetMarshalFuncForTest(failMarshal)

	completePayload, _ := json.Marshal(map[string]any{
		"reservation_id": resID,
		"price":          salePrice,
		"entry_id":       inv[0].EntryID,
	})
	completeMsg := h.sendMessage(h.buyer, completePayload,
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrComplete,
		},
		[]string{deliverMsgRec.ID},
	)
	allMsgs2, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(allMsgs2)

	rec, err := h.st.GetMessage(completeMsg.ID)
	if err != nil || rec == nil {
		t.Fatalf("GetMessage settle(complete): %v", err)
	}

	dispatchErr := eng.DispatchForTest(rec)
	if dispatchErr == nil {
		t.Fatal("expected handleSettle to return error on marshal failure, got nil")
	}
	if !errors.Is(dispatchErr, errMarshalInjected) {
		t.Errorf("expected errMarshalInjected in error chain, got: %v", dispatchErr)
	}

	// Seller and operator balances must be unchanged.
	sellerBalAfter := cs.Balance(h.seller.PublicKeyHex())
	operatorBalAfter := cs.Balance(h.operator.PublicKeyHex())
	if sellerBalAfter != sellerBalBefore {
		t.Errorf("seller balance changed: before=%d after=%d (expected no change on marshal failure)",
			sellerBalBefore, sellerBalAfter)
	}
	if operatorBalAfter != operatorBalBefore {
		t.Errorf("operator balance changed: before=%d after=%d (expected no change on marshal failure)",
			operatorBalBefore, operatorBalAfter)
	}

	// Reservation must be restored — ConsumeReservation ran before marshal failed, so
	// handleSettle must call SaveReservation to put it back. Buyer cannot be locked out.
	eng.SetMarshalFuncForTest(nil) // restore normal marshal so GetReservation works
	gotRes, err := cs.GetReservation(context.Background(), resID)
	if err != nil {
		t.Errorf("reservation should be restored after settle marshal failure, but GetReservation returned: %v", err)
	}
	if gotRes.ID != resID {
		t.Errorf("restored reservation ID = %q, want %q", gotRes.ID, resID)
	}
}

// TestDispute_MarshalFailure_NoRefundNorReservationLoss verifies that if the
// scrip-dispute-refund convention message cannot be marshalled, the buyer's
// balance is not credited AND the reservation is restored (not permanently lost).
func TestDispute_MarshalFailure_NoRefundNorReservationLoss(t *testing.T) {
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

	// Seed inventory; put_price = 10500, sale_price = 12600.
	seedInventoryEntry(t, h, eng, "Security audit generator for test", "review", 15000, 10500)
	inv := eng.State().Inventory()
	if len(inv) != 1 {
		t.Fatalf("expected 1 inventory entry, got %d", len(inv))
	}
	salePrice := inv[0].PutPrice * 120 / 100
	fee := salePrice / exchange.MatchingFeeRate
	holdAmount := salePrice + fee

	// Seed buyer and run a buy to establish a reservation.
	addScripMintMsg(t, h, h.buyer.PublicKeyHex(), holdAmount+5000)
	if err := cs.Replay(); err != nil {
		t.Fatalf("Replay: %v", err)
	}

	preMatch, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() { _ = eng.Start(ctx) }()

	h.sendMessage(h.buyer,
		buyPayload("Audit Go HTTP handlers for security issues (test)", salePrice+5000),
		[]string{exchange.TagBuy},
		nil,
	)

	matchMsg := waitForMatchMessage(t, h, preMatch, 2*time.Second)
	cancel()

	resID := extractReservationID(t, matchMsg)
	if resID == "" {
		t.Fatal("expected non-empty reservation_id")
	}

	// Record buyer balance after buy-hold (decremented by holdAmount).
	buyerBalBeforeDispute := cs.Balance(h.buyer.PublicKeyHex())

	// Inject failing marshal, then dispatch settle(dispute).
	eng.SetMarshalFuncForTest(failMarshal)

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
	if err != nil || rec == nil {
		t.Fatalf("GetMessage dispute: %v", err)
	}

	dispatchErr := eng.DispatchForTest(rec)
	if dispatchErr == nil {
		t.Fatal("expected handleDispute to return error on marshal failure, got nil")
	}
	if !errors.Is(dispatchErr, errMarshalInjected) {
		t.Errorf("expected errMarshalInjected in error chain, got: %v", dispatchErr)
	}

	// Buyer balance must be unchanged — no refund credited.
	buyerBalAfterDispute := cs.Balance(h.buyer.PublicKeyHex())
	if buyerBalAfterDispute != buyerBalBeforeDispute {
		t.Errorf("buyer balance changed: before=%d after=%d (expected no change on marshal failure)",
			buyerBalBeforeDispute, buyerBalAfterDispute)
	}

	// Reservation must still exist — it was restored after marshal failure.
	eng.SetMarshalFuncForTest(nil) // restore normal marshal
	gotRes, err := cs.GetReservation(context.Background(), resID)
	if err != nil {
		t.Errorf("reservation should be restored after marshal failure, but GetReservation returned: %v", err)
	}
	if gotRes.ID != resID {
		t.Errorf("restored reservation ID = %q, want %q", gotRes.ID, resID)
	}
}
