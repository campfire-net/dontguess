package exchange_test

// TestSettle_HighReuseResidualIs20Pct verifies that when a high-reuse artifact
// (§4 distilled-artifact class) settles through the full
//
//	accept → buyer-accept → deliver → complete
//
// cycle, the seller residual is price / HighReuseResidualDenominator (20%)
// rather than the standard price / ResidualRate (10%).
//
// This is the ground-source proof for the high-reuse residual path added in
// dontguess-13a: the two residual rates are exercised by the same code path
// in handleSettle (engine.go ~line 1162), gated on IsHighReuseArtifact.
//
// Real path: real engine (DispatchForTest / Start), real campfire fs transport,
// real CampfireScripStore. No mocks of the path under test.
//
// Done condition: residual amount paid to seller == price / 5 (20%) for a
// high-reuse entry, and == price / 10 (10%) for a standard entry.

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/campfire-net/dontguess/pkg/store"

	"github.com/campfire-net/dontguess/pkg/exchange"
	"github.com/campfire-net/dontguess/pkg/scrip"
)

// buildHighReuseSettleChain seeds a high-reuse (or standard) inventory entry,
// runs a buy to obtain a match, dispatches buyer-accept (scrip hold), then
// builds the deliver message. Returns the reservation, deliver message, and
// the price recovered from the reservation.
//
// desc must be a §4-class high-reuse description when wantHighReuse=true.
func buildHighReuseSettleChain(
	t *testing.T,
	h *testHarness,
	eng *exchange.Engine,
	cs *scrip.LocalScripStore,
	desc string,
	contentType string,
	putPrice int64,
) (res scrip.Reservation, deliverMsg *exchange.Message, salePrice int64) {
	t.Helper()

	seedInventoryEntry(t, h, eng, desc, contentType, putPrice*2, putPrice)
	inv := eng.State().Inventory()
	if len(inv) == 0 {
		t.Fatal("buildHighReuseSettleChain: no inventory entries after seed")
	}

	// Find the entry we just seeded (by description).
	var entry *exchange.InventoryEntry
	for _, e := range inv {
		if e.Description == desc {
			entry = e
			break
		}
	}
	if entry == nil {
		// Fall back to the only entry if there's just one.
		if len(inv) == 1 {
			entry = inv[0]
		} else {
			t.Fatalf("buildHighReuseSettleChain: entry with desc=%q not found in inventory", desc)
		}
	}

	salePrice = eng.ComputePriceForTest(entry)
	fee := salePrice / exchange.MatchingFeeRate
	holdAmount := salePrice + fee
	// Recover price from reservation amount (mirrors engine formula).
	salePrice = holdAmount * exchange.MatchingFeeRate / (exchange.MatchingFeeRate + 1)

	addScripMintMsg(t, h, h.buyer.PublicKeyHex(), holdAmount+5000)
	if err := cs.Replay(); err != nil {
		t.Fatalf("buildHighReuseSettleChain: Replay: %v", err)
	}

	preMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() { _ = eng.Start(ctx) }()

	h.sendMessage(h.buyer,
		buyPayload("query for "+desc, salePrice+5000),
		[]string{exchange.TagBuy},
		nil,
	)

	matchMsg := waitForMatchMessage(t, h, preMsgs, 2*time.Second)
	cancel()

	// buyer-accept triggers the scrip hold.
	buyerAcceptMsg := sendBuyerAcceptAndDispatch(t, h, eng, matchMsg.ID, entry.EntryID)

	resID := extractReservationIDFromLog(t, h)
	if resID == "" {
		t.Fatal("buildHighReuseSettleChain: no reservation_id after buyer-accept")
	}
	var err error
	res, err = cs.GetReservation(context.Background(), resID)
	if err != nil {
		t.Fatalf("buildHighReuseSettleChain: reservation %s not found: %v", resID, err)
	}

	deliverPayload, _ := json.Marshal(map[string]any{
		"phase":        "deliver",
		"entry_id":     entry.EntryID,
		"content_ref":  "sha256:" + fmt.Sprintf("%064x", 99),
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

// TestSettle_HighReuseResidualIs20Pct is the ground-source test for the
// high-reuse residual rate. It uses a full accept→buyer-accept→deliver→complete
// cycle for a §4-class entry and asserts the seller receives 20% of sale price
// (price / HighReuseResidualDenominator = price / 5).
func TestSettle_HighReuseResidualIs20Pct(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	cs := newCampfireScripStore(t, h)
	eng := exchange.NewEngine(exchange.EngineOptions{
		CampfireID:        h.cfID,
		LocalStore:        h.st,
		OperatorPublicKey: h.operator.pubKeyHex,
		ScripStore:        cs,
		Logger:            func(format string, args ...any) { t.Logf("[engine] "+format, args...) },
	})

	// High-reuse description: §4 Class 4 "test pattern" + co-signal "go"
	// → IsHighReuseArtifact returns true.
	highReuseDesc := "flock contention test pattern for Go goroutine synchronization"
	const putPrice = int64(5000)

	// Verify the classifier agrees before running the full cycle.
	highReuseEntry := &exchange.InventoryEntry{
		Description: highReuseDesc,
		ContentType: "code",
	}
	if !exchange.IsHighReuseArtifactForTest(highReuseEntry) {
		t.Fatalf("IsHighReuseArtifact(%q) = false — test fixture error: description must classify as high-reuse",
			highReuseDesc)
	}

	res, deliverMsg, salePrice := buildHighReuseSettleChain(t, h, eng, cs, highReuseDesc, "code", putPrice)

	sellerBalanceBefore := cs.Balance(h.seller.PublicKeyHex())

	// Build and dispatch settle:complete.
	completePayload, _ := json.Marshal(map[string]any{
		"phase":    exchange.SettlePhaseStrComplete,
		"entry_id": res.ID, // entry_id in payload is ignored by engine; chain resolves it
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
		t.Fatalf("GetMessage complete: %v", err)
	}
	if dispatchErr := eng.DispatchForTest(exchange.FromStoreRecord(rec)); dispatchErr != nil {
		t.Fatalf("DispatchForTest(settle:complete): %v", dispatchErr)
	}

	sellerBalanceAfter := cs.Balance(h.seller.PublicKeyHex())
	actualResidual := sellerBalanceAfter - sellerBalanceBefore

	// High-reuse path: residual = price / HighReuseResidualDenominator (5) → 20%
	expectedResidual := salePrice / exchange.HighReuseResidualDenominator
	if actualResidual != expectedResidual {
		t.Errorf("high-reuse residual = %d, want %d (price=%d / HighReuseResidualDenominator=%d = 20%%)\n"+
			"  standard residual would be %d (price / ResidualRate=10%%)",
			actualResidual, expectedResidual, salePrice, exchange.HighReuseResidualDenominator,
			salePrice/exchange.ResidualRate)
	}

	// The high-reuse residual must be exactly double the standard residual.
	standardResidual := salePrice / exchange.ResidualRate
	if expectedResidual != 2*standardResidual {
		t.Errorf("high-reuse residual (%d) should be exactly 2× standard residual (%d) — arithmetic sanity check",
			expectedResidual, standardResidual)
	}

	t.Logf("PASS: high-reuse settle: price=%d residual=%d (20%% = price/%d), standard would be %d (10%% = price/%d)",
		salePrice, actualResidual, exchange.HighReuseResidualDenominator,
		standardResidual, exchange.ResidualRate)
}

// TestSettle_StandardResidualIs10Pct verifies the standard (non-high-reuse)
// residual rate (price / ResidualRate = 10%) through the same settle cycle.
// This is a control test to confirm the two paths are distinct.
func TestSettle_StandardResidualIs10Pct(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	cs := newCampfireScripStore(t, h)
	eng := exchange.NewEngine(exchange.EngineOptions{
		CampfireID:        h.cfID,
		LocalStore:        h.st,
		OperatorPublicKey: h.operator.pubKeyHex,
		ScripStore:        cs,
		Logger:            func(format string, args ...any) { t.Logf("[engine] "+format, args...) },
	})

	// Ephemeral description: does NOT match any §4 high-reuse class.
	standardDesc := "analysis of the current work queue priority ordering"
	const putPrice = int64(5000)

	// Verify the classifier agrees before running the full cycle.
	standardEntry := &exchange.InventoryEntry{
		Description: standardDesc,
		ContentType: "analysis",
	}
	if exchange.IsHighReuseArtifactForTest(standardEntry) {
		t.Fatalf("IsHighReuseArtifact(%q) = true — test fixture error: description must NOT classify as high-reuse",
			standardDesc)
	}

	res, deliverMsg, salePrice := buildHighReuseSettleChain(t, h, eng, cs, standardDesc, "analysis", putPrice)

	sellerBalanceBefore := cs.Balance(h.seller.PublicKeyHex())

	completePayload, _ := json.Marshal(map[string]any{
		"phase": exchange.SettlePhaseStrComplete,
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
		t.Fatalf("GetMessage complete: %v", err)
	}
	_ = res // reservation is held by scrip store; checked via balance delta
	if dispatchErr := eng.DispatchForTest(exchange.FromStoreRecord(rec)); dispatchErr != nil {
		t.Fatalf("DispatchForTest(settle:complete): %v", dispatchErr)
	}

	sellerBalanceAfter := cs.Balance(h.seller.PublicKeyHex())
	actualResidual := sellerBalanceAfter - sellerBalanceBefore

	// Standard path: residual = price / ResidualRate (10) → 10%
	expectedResidual := salePrice / exchange.ResidualRate
	if actualResidual != expectedResidual {
		t.Errorf("standard residual = %d, want %d (price=%d / ResidualRate=%d = 10%%)",
			actualResidual, expectedResidual, salePrice, exchange.ResidualRate)
	}

	// Standard residual must be LESS than high-reuse residual.
	highReuseResidual := salePrice / exchange.HighReuseResidualDenominator
	if actualResidual >= highReuseResidual {
		t.Errorf("standard residual (%d) should be less than high-reuse residual (%d)",
			actualResidual, highReuseResidual)
	}

	t.Logf("PASS: standard settle: price=%d residual=%d (10%% = price/%d)",
		salePrice, actualResidual, exchange.ResidualRate)
}
