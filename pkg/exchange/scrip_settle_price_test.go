package exchange_test

// Tests for settle price path security fixes (dontguess-ica):
//   - payload.Price is cross-checked against res.Amount (reservation)
//   - price=0 with a non-empty reservation_id is rejected
//
// Without these checks, a malicious buyer could send a settle(complete) with
// an inflated or deflated payload.Price to change the seller residual and
// operator revenue, or submit price=0 to receive cached inference for free
// while leaving the reservation dangling.

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/campfire-net/campfire/pkg/store"

	"github.com/3dl-dev/dontguess/pkg/exchange"
	"github.com/3dl-dev/dontguess/pkg/scrip"
)

// buildSettleChainForPriceTests seeds an inventory entry, runs a buy to get a
// reservation, then constructs the buyer-accept + deliver message chain and
// replays state. Returns the reservation, deliver message, and sale price.
// The caller can then issue a complete message with an arbitrary price.
func buildSettleChainForPriceTests(
	t *testing.T,
	h *testHarness,
	eng *exchange.Engine,
	cs *scrip.CampfireScripStore,
	entryDesc string,
	putPrice int64,
) (res scrip.Reservation, deliverMsg *store.MessageRecord, salePrice int64) {
	t.Helper()

	seedInventoryEntry(t, h, eng, entryDesc, "code", putPrice*2, putPrice)
	inv := eng.State().Inventory()
	if len(inv) == 0 {
		t.Fatal("buildSettleChainForPriceTests: no inventory entries")
	}
	entry := inv[0]
	salePrice = entry.PutPrice * 120 / 100
	fee := salePrice / exchange.MatchingFeeRate
	holdAmount := salePrice + fee

	addScripMintMsg(t, h, h.buyer.PublicKeyHex(), holdAmount+5000)
	if err := cs.Replay(); err != nil {
		t.Fatalf("buildSettleChainForPriceTests: Replay: %v", err)
	}

	preMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() { _ = eng.Start(ctx) }()

	h.sendMessage(h.buyer,
		buyPayload("query for "+entryDesc, salePrice+5000),
		[]string{exchange.TagBuy},
		nil,
	)

	matchMsg := waitForMatchMessage(t, h, preMsgs, 2*time.Second)
	cancel()

	resID := extractReservationID(t, matchMsg)
	if resID == "" {
		t.Fatal("buildSettleChainForPriceTests: expected non-empty reservation_id in match payload")
	}

	var err error
	res, err = cs.GetReservation(context.Background(), resID)
	if err != nil {
		t.Fatalf("buildSettleChainForPriceTests: reservation %s not found: %v", resID, err)
	}

	// buyer-accept.
	buyerAcceptPayload, _ := json.Marshal(map[string]any{
		"phase":    "buyer-accept",
		"entry_id": entry.EntryID,
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

	// deliver.
	deliverPayload, _ := json.Marshal(map[string]any{
		"phase":        "deliver",
		"entry_id":     entry.EntryID,
		"content_ref":  "sha256:" + fmt.Sprintf("%064x", 42),
		"content_size": int64(10000),
	})
	deliverMsg = h.sendMessage(h.operator, deliverPayload,
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrDeliver,
		},
		[]string{buyerAcceptMsg.ID},
	)

	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(allMsgs)

	return res, deliverMsg, salePrice
}

// TestSettle_PriceMismatchRejected verifies that a settle(complete) with a
// payload.Price that does not match the reservation amount is rejected, and
// the reservation is restored (no scrip movement, no double-spend).
//
// Security fix (dontguess-ica / dontguess-3oo / dontguess-z2a):
// A malicious buyer could inflate payload.Price to increase seller credit
// paid from the exchange's pre-approved reservation, effectively draining
// the exchange. The cross-check ensures payload.Price is consistent with
// the pre-approved res.Amount before any scrip moves.
func TestSettle_PriceMismatchRejected(t *testing.T) {
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

	res, deliverMsg, salePrice := buildSettleChainForPriceTests(t, h, eng, cs, "SQL migration generator", 5000)

	sellerBalanceBefore := cs.Balance(h.seller.PublicKeyHex())
	operatorBalanceBefore := cs.Balance(h.operator.PublicKeyHex())

	// Send complete with an inflated price (2x the real sale price).
	inflatedPrice := salePrice * 2
	completePayload, _ := json.Marshal(map[string]any{
		"reservation_id": res.ID,
		"price":          inflatedPrice,
	})
	completeMsg := h.sendMessage(h.buyer, completePayload,
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrComplete,
		},
		[]string{deliverMsg.ID},
	)

	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(allMsgs)

	rec, err := h.st.GetMessage(completeMsg.ID)
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if dispatchErr := eng.DispatchForTest(rec); dispatchErr == nil {
		t.Error("expected error for price mismatch, got nil")
	}

	// No scrip movement.
	if cs.Balance(h.seller.PublicKeyHex()) != sellerBalanceBefore {
		t.Errorf("seller balance changed on price-mismatch settle: got %d, want %d",
			cs.Balance(h.seller.PublicKeyHex()), sellerBalanceBefore)
	}
	if cs.Balance(h.operator.PublicKeyHex()) != operatorBalanceBefore {
		t.Errorf("operator balance changed on price-mismatch settle: got %d, want %d",
			cs.Balance(h.operator.PublicKeyHex()), operatorBalanceBefore)
	}

	// Reservation must be restored (not consumed).
	if _, err := cs.GetReservation(context.Background(), res.ID); err != nil {
		t.Errorf("reservation %s must be restored after price mismatch, got: %v", res.ID[:8], err)
	}
}

// TestSettle_PriceZeroWithReservationRejected verifies that a settle(complete)
// with price=0 and a non-empty reservation_id is rejected.
//
// Security fix (dontguess-ica / dontguess-qfv):
// Without this check, price=0 would skip scrip movement (the old code path)
// while state still records the completed transaction — leaving the reservation
// dangling and the buyer having received cached inference for free (the
// reservation hold was never consumed, but the transaction was credited).
func TestSettle_PriceZeroWithReservationRejected(t *testing.T) {
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

	res, deliverMsg, _ := buildSettleChainForPriceTests(t, h, eng, cs, "Docker compose generator", 6000)

	sellerBalanceBefore := cs.Balance(h.seller.PublicKeyHex())
	operatorBalanceBefore := cs.Balance(h.operator.PublicKeyHex())

	// Send complete with price=0.
	completePayload, _ := json.Marshal(map[string]any{
		"reservation_id": res.ID,
		"price":          int64(0),
	})
	completeMsg := h.sendMessage(h.buyer, completePayload,
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrComplete,
		},
		[]string{deliverMsg.ID},
	)

	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(allMsgs)

	rec, err := h.st.GetMessage(completeMsg.ID)
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if dispatchErr := eng.DispatchForTest(rec); dispatchErr == nil {
		t.Error("expected error for price=0 with non-empty reservation_id, got nil")
	}

	// No scrip movement.
	if cs.Balance(h.seller.PublicKeyHex()) != sellerBalanceBefore {
		t.Errorf("seller balance changed on price=0 settle: got %d, want %d",
			cs.Balance(h.seller.PublicKeyHex()), sellerBalanceBefore)
	}
	if cs.Balance(h.operator.PublicKeyHex()) != operatorBalanceBefore {
		t.Errorf("operator balance changed on price=0 settle: got %d, want %d",
			cs.Balance(h.operator.PublicKeyHex()), operatorBalanceBefore)
	}

	// Reservation must still exist (was not consumed because price was rejected early).
	if _, err := cs.GetReservation(context.Background(), res.ID); err != nil {
		t.Errorf("reservation %s must remain after price=0 rejection, got: %v", res.ID[:8], err)
	}
}
