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
// match, dispatches buyer-accept (triggering the scrip hold), then constructs
// the deliver message chain and replays state. Returns the reservation (from
// the buy-hold log), deliver message, and sale price.
func buildSettleChainForPriceTests(
	t *testing.T,
	h *testHarness,
	eng *exchange.Engine,
	cs *scrip.CampfireScripStore,
	entryDesc string,
	putPrice int64,
) (res scrip.Reservation, deliverMsg *exchange.Message, salePrice int64) {
	t.Helper()

	seedInventoryEntry(t, h, eng, entryDesc, "code", putPrice*2, putPrice)
	inv := eng.State().Inventory()
	if len(inv) == 0 {
		t.Fatal("buildSettleChainForPriceTests: no inventory entries")
	}
	entry := inv[0]
	salePrice = eng.ComputePriceForTest(entry)
	fee := salePrice / exchange.MatchingFeeRate
	holdAmount := salePrice + fee
	// Return the engine-recovered price (not the computed salePrice) so callers can
	// derive consistent residual/revenue expectations matching the engine's own formula.
	salePrice = holdAmount * exchange.MatchingFeeRate / (exchange.MatchingFeeRate + 1)

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

	// buyer-accept triggers the scrip hold.
	buyerAcceptMsg := sendBuyerAcceptAndDispatch(t, h, eng, matchMsg.ID, entry.EntryID)

	// Get the reservation from the scrip-buy-hold message emitted during buyer-accept.
	resID := extractReservationIDFromLog(t, h)
	if resID == "" {
		t.Fatal("buildSettleChainForPriceTests: expected non-empty reservation_id after buyer-accept")
	}

	var err error
	res, err = cs.GetReservation(context.Background(), resID)
	if err != nil {
		t.Fatalf("buildSettleChainForPriceTests: reservation %s not found: %v", resID, err)
	}

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
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	return res, deliverMsg, salePrice
}

// TestSettle_PriceLockedAtBuyerAcceptTime verifies that the price used for
// settlement is the one locked in the reservation at buyer-accept time, not
// whatever the buyer sends in the complete payload.
//
// Security property (dontguess-dl3 preview-before-purchase model):
// Price is locked when the buyer reviews the preview and accepts. Any buyer-
// controlled price field in the complete payload is ignored. The engine derives
// settlement amounts exclusively from the reservation created at buyer-accept.
func TestSettle_PriceLockedAtBuyerAcceptTime(t *testing.T) {
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

	// Send complete with an inflated price (2x the real sale price) in the payload.
	// The engine must ignore this and use the price from the reservation.
	inflatedPrice := salePrice * 2
	completePayload, _ := json.Marshal(map[string]any{
		"price":    inflatedPrice, // buyer-supplied, must be ignored
		"entry_id": res.ID,
	})
	completeMsg := h.sendMessage(h.buyer, completePayload,
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrComplete,
		},
		[]string{deliverMsg.ID},
	)

	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	rec, err := h.st.GetMessage(completeMsg.ID)
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	// Complete must succeed using the reservation-locked price, not the inflated payload price.
	if dispatchErr := eng.DispatchForTest(exchange.FromStoreRecord(rec)); dispatchErr != nil {
		t.Fatalf("expected settle(complete) to succeed using reservation price, got: %v", dispatchErr)
	}

	// Seller must receive residual based on the real sale price (from reservation), NOT inflated.
	expectedResidual := salePrice / exchange.ResidualRate
	if cs.Balance(h.seller.PublicKeyHex()) != sellerBalanceBefore+expectedResidual {
		t.Errorf("seller balance: got %d, want %d (residual from locked price, not payload)",
			cs.Balance(h.seller.PublicKeyHex()), sellerBalanceBefore+expectedResidual)
	}

	// Operator revenue must be based on the real sale price.
	expectedRevenue := salePrice - expectedResidual
	if cs.Balance(h.operator.PublicKeyHex()) != operatorBalanceBefore+expectedRevenue {
		t.Errorf("operator balance: got %d, want %d (revenue from locked price, not payload)",
			cs.Balance(h.operator.PublicKeyHex()), operatorBalanceBefore+expectedRevenue)
	}

	// Reservation must be consumed after successful settle.
	if _, err := cs.GetReservation(context.Background(), res.ID); err == nil {
		t.Errorf("reservation %s must be consumed after settle(complete)", res.ID[:8])
	}
}

// TestSettle_CompleteWithoutBuyerAcceptIsSkipped verifies that a settle(complete)
// message for which no buyer-accept scrip hold exists is silently skipped (no scrip
// movement, no error). This prevents a buyer from receiving cached inference for free
// by skipping the buyer-accept step and sending a complete directly.
//
// In the preview-before-purchase model, scrip is locked at buyer-accept time.
// The complete handler looks up the reservation from the engine's matchToReservation
// map. If the buyer never sent a buyer-accept (or it was rejected), no reservation
// exists for that match and the complete is a no-op.
func TestSettle_CompleteWithoutBuyerAcceptIsSkipped(t *testing.T) {
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

	seedInventoryEntry(t, h, eng, "Docker compose generator skip test", "code", 12000, 6000)
	inv := eng.State().Inventory()
	if len(inv) == 0 {
		t.Fatal("no inventory entries")
	}
	salePrice := eng.ComputePriceForTest(inv[0])
	fee := salePrice / exchange.MatchingFeeRate
	holdAmount := salePrice + fee

	addScripMintMsg(t, h, h.buyer.PublicKeyHex(), holdAmount+5000)
	if err := cs.Replay(); err != nil {
		t.Fatalf("Replay: %v", err)
	}

	preMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() { _ = eng.Start(ctx) }()

	h.sendMessage(h.buyer,
		buyPayload("docker compose generator test", salePrice+5000),
		[]string{exchange.TagBuy},
		nil,
	)

	matchMsg := waitForMatchMessage(t, h, preMsgs, 2*time.Second)
	cancel()

	// Send buyer-accept to get the state chain built (antecedent for deliver).
	// BUT do NOT dispatch buyer-accept through the engine — so no scrip hold.
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

	deliverPayload, _ := json.Marshal(map[string]any{
		"phase":        "deliver",
		"entry_id":     inv[0].EntryID,
		"content_ref":  "sha256:" + fmt.Sprintf("%064x", 42),
		"content_size": int64(10000),
	})
	deliverMsg := h.sendMessage(h.operator, deliverPayload,
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrDeliver,
		},
		[]string{buyerAcceptMsg.ID},
	)

	// Replay state (buyer-accept in state, but NOT dispatched through engine).
	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	sellerBalanceBefore := cs.Balance(h.seller.PublicKeyHex())
	operatorBalanceBefore := cs.Balance(h.operator.PublicKeyHex())

	// Send complete without a prior engine-dispatched buyer-accept.
	// The engine has no reservation for this match → silently skip.
	completePayload, _ := json.Marshal(map[string]any{
		"entry_id": inv[0].EntryID,
	})
	completeMsg := h.sendMessage(h.buyer, completePayload,
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrComplete,
		},
		[]string{deliverMsg.ID},
	)

	allMsgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	rec, err := h.st.GetMessage(completeMsg.ID)
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	// Must not return an error — missing reservation is a silent skip.
	if dispatchErr := eng.DispatchForTest(exchange.FromStoreRecord(rec)); dispatchErr != nil {
		t.Errorf("expected silent skip for complete without buyer-accept, got error: %v", dispatchErr)
	}

	// No scrip movement.
	if cs.Balance(h.seller.PublicKeyHex()) != sellerBalanceBefore {
		t.Errorf("seller balance changed on no-reservation complete: got %d, want %d",
			cs.Balance(h.seller.PublicKeyHex()), sellerBalanceBefore)
	}
	if cs.Balance(h.operator.PublicKeyHex()) != operatorBalanceBefore {
		t.Errorf("operator balance changed on no-reservation complete: got %d, want %d",
			cs.Balance(h.operator.PublicKeyHex()), operatorBalanceBefore)
	}
}
