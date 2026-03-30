package exchange_test

// Tests for scrip convention message emission in the exchange engine.
//
// Each scrip-affecting operation (buy-hold, settle, burn, dispute-refund) must
// emit a parseable convention message to the campfire so CampfireScripStore can
// replay state from the message log.
//
// These tests verify:
//   - handleSettleBuyerAcceptScrip emits a dontguess:scrip-buy-hold message at buyer-accept time
//   - handleSettle(complete) emits dontguess:scrip-settle and dontguess:scrip-burn messages
//   - handleDispute emits a dontguess:scrip-dispute-refund message
//   - All emitted payloads are parseable by CampfireScripStore after a full Replay

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/campfire-net/campfire/pkg/protocol"
	"github.com/campfire-net/campfire/pkg/store"

	"github.com/3dl-dev/dontguess/pkg/exchange"
	"github.com/3dl-dev/dontguess/pkg/scrip"
)

// waitForScripMessage polls the store until a message with the given scrip tag appears.
// Returns the last matching message or fails the test on timeout.
func waitForScripMessage(t *testing.T, h *testHarness, tag string, before []store.MessageRecord, timeout time.Duration) *store.MessageRecord {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		msgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{tag}})
		if len(msgs) > len(before) {
			last := msgs[len(msgs)-1]
			return &last
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for scrip message with tag %q", tag)
	return nil
}

// countMsgsWithTag counts messages in the store with the given tag.
func countMsgsWithTag(t *testing.T, h *testHarness, tag string) int {
	t.Helper()
	msgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{tag}})
	return len(msgs)
}

// TestBuyHold_EmitsConventionMessage verifies that handleSettleBuyerAcceptScrip emits a
// dontguess:scrip-buy-hold message parseable by CampfireScripStore.
//
// The buy-hold is emitted at buyer-accept time (preview-before-purchase model),
// not at buy time. The engine must emit the hold when the buyer dispatches
// buyer-accept after reviewing the preview.
func TestBuyHold_EmitsConventionMessage(t *testing.T) {
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

	// Seed one inventory entry; put_price = 5600, sale price = 6720, fee = 672, hold = 7392.
	seedInventoryEntry(t, h, eng, "Go HTTP test generator", "code", 8000, 5600)
	inv := eng.State().Inventory()
	if len(inv) != 1 {
		t.Fatalf("expected 1 inventory entry, got %d", len(inv))
	}
	salePrice := eng.ComputePriceForTest(inv[0])
	fee := salePrice / exchange.MatchingFeeRate
	holdAmount := salePrice + fee

	// Seed buyer with sufficient scrip.
	addScripMintMsg(t, h, h.buyer.PublicKeyHex(), holdAmount+5000)
	if err := cs.Replay(); err != nil {
		t.Fatalf("Replay: %v", err)
	}

	preBuyHold := countMsgsWithTag(t, h, scrip.TagScripBuyHold)
	preMatch := countMsgsWithTag(t, h, exchange.TagMatch)

	// Run engine to get a match (no buy-hold emitted at buy time).
	preMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() { _ = eng.Start(ctx) }()

	h.sendMessage(h.buyer,
		buyPayload("Generate Go HTTP handler unit tests", salePrice+5000),
		[]string{exchange.TagBuy},
		nil,
	)

	matchMsg := waitForMatchMessage(t, h, preMsgs, 2*time.Second)
	cancel()

	// Verify match count increased. No buy-hold yet (hold is at buyer-accept time).
	afterMatch := countMsgsWithTag(t, h, exchange.TagMatch)
	if afterMatch <= preMatch {
		t.Fatal("expected at least one match message")
	}
	if countMsgsWithTag(t, h, scrip.TagScripBuyHold) != preBuyHold {
		t.Error("buy-hold must NOT be emitted at buy time (only at buyer-accept)")
	}

	// Dispatch buyer-accept — this triggers the scrip hold.
	_ = sendBuyerAcceptAndDispatch(t, h, eng, matchMsg.ID, inv[0].EntryID)

	// Buy-hold message must appear after buyer-accept.
	holdMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{scrip.TagScripBuyHold}})
	if len(holdMsgs) <= preBuyHold {
		t.Fatal("expected scrip-buy-hold message to be emitted after buyer-accept")
	}
	holdMsg := holdMsgs[len(holdMsgs)-1]

	// Parse the buy-hold payload and verify fields.
	var p scrip.BuyHoldPayload
	if err := json.Unmarshal(holdMsg.Payload, &p); err != nil {
		t.Fatalf("parsing scrip-buy-hold payload: %v", err)
	}
	if p.Buyer != h.buyer.PublicKeyHex() {
		t.Errorf("buy-hold buyer = %q, want %q", p.Buyer, h.buyer.PublicKeyHex())
	}
	if p.Amount != holdAmount {
		t.Errorf("buy-hold amount = %d, want %d", p.Amount, holdAmount)
	}
	if p.Price != salePrice {
		t.Errorf("buy-hold price = %d, want %d", p.Price, salePrice)
	}
	if p.Fee != fee {
		t.Errorf("buy-hold fee = %d, want %d", p.Fee, fee)
	}
	if p.ReservationID == "" {
		t.Error("buy-hold reservation_id must be non-empty")
	}
	if p.BuyMsg == "" {
		t.Error("buy-hold buy_msg (matchMsgID anchor) must be non-empty")
	}
	if p.ExpiresAt == "" {
		t.Error("buy-hold expires_at must be non-empty")
	}

	// Verify the message sender is the operator.
	if holdMsg.Sender != h.operator.PublicKeyHex() {
		t.Errorf("buy-hold sender = %q, want operator %q", holdMsg.Sender, h.operator.PublicKeyHex())
	}

	// Verify CampfireScripStore can materialize the hold: after a full Replay,
	// a fresh store should show the buyer's balance as reduced by holdAmount.
	freshCS, err := scrip.NewCampfireScripStore(h.cfID, protocol.New(h.st, h.operator), h.operator.PublicKeyHex())
	if err != nil {
		t.Fatalf("NewCampfireScripStore (fresh): %v", err)
	}
	// Mint gave buyer holdAmount+5000. After buyer-accept hold, balance = 5000.
	buyerBalance := freshCS.Balance(h.buyer.PublicKeyHex())
	if buyerBalance != 5000 {
		t.Errorf("replayed buyer balance = %d, want 5000 (mint %d - hold %d)",
			buyerBalance, holdAmount+5000, holdAmount)
	}
}

// TestSettle_EmitsConventionMessages verifies that handleSettle(complete) emits:
//   - dontguess:scrip-settle with correct residual/fee_burned/exchange_revenue
//   - dontguess:scrip-burn for the matching fee
func TestSettle_EmitsConventionMessages(t *testing.T) {
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

	// Seed inventory; put_price = 5600, sale_price = 6720.
	seedInventoryEntry(t, h, eng, "Terraform module generator", "code", 8000, 5600)
	inv := eng.State().Inventory()
	if len(inv) != 1 {
		t.Fatalf("expected 1 inventory entry, got %d", len(inv))
	}
	salePrice := eng.ComputePriceForTest(inv[0])
	fee := salePrice / exchange.MatchingFeeRate
	holdAmount := salePrice + fee
	enginePrice := holdAmount * exchange.MatchingFeeRate / (exchange.MatchingFeeRate + 1)
	expectedResidual := enginePrice / exchange.ResidualRate
	expectedExchangeRevenue := enginePrice - expectedResidual

	// Seed buyer with sufficient scrip and run buy to get a reservation.
	addScripMintMsg(t, h, h.buyer.PublicKeyHex(), holdAmount+5000)
	if err := cs.Replay(); err != nil {
		t.Fatalf("Replay: %v", err)
	}

	preMatch, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() { _ = eng.Start(ctx) }()

	h.sendMessage(h.buyer,
		buyPayload("Generate Terraform S3 module", salePrice+5000),
		[]string{exchange.TagBuy},
		nil,
	)

	matchMsg := waitForMatchMessage(t, h, preMatch, 2*time.Second)
	cancel()

	// Dispatch buyer-accept to trigger the scrip hold and create the reservation.
	buyerAcceptMsg := sendBuyerAcceptAndDispatch(t, h, eng, matchMsg.ID, inv[0].EntryID)

	// Get reservation ID from the scrip-buy-hold log message.
	resID := extractReservationIDFromLog(t, h)
	if resID == "" {
		t.Fatal("expected non-empty reservation_id after buyer-accept")
	}

	// Build the deliver antecedent for the complete message.
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

	// Replay so antecedent chain is in state.
	chainMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(chainMsgs))

	// Pre-count settle and burn messages.
	preSettle := countMsgsWithTag(t, h, scrip.TagScripSettle)
	preBurn := countMsgsWithTag(t, h, scrip.TagScripBurn)

	// Dispatch settle(complete) — sender is buyer; seller derived from chain.
	// Price is derived from the reservation (locked at buyer-accept), not from payload.
	completePayload, _ := json.Marshal(map[string]any{
		"entry_id": inv[0].EntryID,
	})
	completeMsg := h.sendMessage(h.buyer, completePayload,
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrComplete,
		},
		[]string{deliverMsgRec.ID},
	)
	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))
	rec, err := h.st.GetMessage(completeMsg.ID)
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if err := eng.DispatchForTest(exchange.FromStoreRecord(rec)); err != nil {
		t.Fatalf("dispatch settle(complete): %v", err)
	}

	// Verify scrip-settle message was emitted.
	afterSettle := countMsgsWithTag(t, h, scrip.TagScripSettle)
	if afterSettle <= preSettle {
		t.Fatal("expected scrip-settle message to be emitted after settle(complete)")
	}

	// Parse the settle payload.
	settleMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{scrip.TagScripSettle}})
	settleMsg := settleMsgs[len(settleMsgs)-1]
	var sp scrip.SettlePayload
	if err := json.Unmarshal(settleMsg.Payload, &sp); err != nil {
		t.Fatalf("parsing scrip-settle payload: %v", err)
	}
	if sp.ReservationID != resID {
		t.Errorf("settle reservation_id = %q, want %q", sp.ReservationID, resID)
	}
	if sp.Seller != h.seller.PublicKeyHex() {
		t.Errorf("settle seller = %q, want %q", sp.Seller, h.seller.PublicKeyHex())
	}
	if sp.Residual != expectedResidual {
		t.Errorf("settle residual = %d, want %d", sp.Residual, expectedResidual)
	}
	if sp.FeeBurned != fee {
		t.Errorf("settle fee_burned = %d, want %d", sp.FeeBurned, fee)
	}
	if sp.ExchangeRevenue != expectedExchangeRevenue {
		t.Errorf("settle exchange_revenue = %d, want %d", sp.ExchangeRevenue, expectedExchangeRevenue)
	}

	// Verify scrip-burn message was emitted for the matching fee.
	afterBurn := countMsgsWithTag(t, h, scrip.TagScripBurn)
	if afterBurn <= preBurn {
		t.Fatal("expected scrip-burn message to be emitted for matching fee")
	}

	burnMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{scrip.TagScripBurn}})
	burnMsg := burnMsgs[len(burnMsgs)-1]
	var bp scrip.BurnPayload
	if err := json.Unmarshal(burnMsg.Payload, &bp); err != nil {
		t.Fatalf("parsing scrip-burn payload: %v", err)
	}
	if bp.Amount != fee {
		t.Errorf("burn amount = %d, want %d (matching fee)", bp.Amount, fee)
	}
	if bp.Reason != "matching-fee" {
		t.Errorf("burn reason = %q, want %q", bp.Reason, "matching-fee")
	}

	// Verify CampfireScripStore replays correctly: seller gets residual, operator gets revenue.
	freshCS, err := scrip.NewCampfireScripStore(h.cfID, protocol.New(h.st, h.operator), h.operator.PublicKeyHex())
	if err != nil {
		t.Fatalf("NewCampfireScripStore (fresh): %v", err)
	}
	sellerBal := freshCS.Balance(h.seller.PublicKeyHex())
	if sellerBal != expectedResidual {
		t.Errorf("replayed seller balance = %d, want %d (residual)", sellerBal, expectedResidual)
	}
	if freshCS.TotalBurned() < fee {
		t.Errorf("replayed total_burned = %d, want >= %d (fee)", freshCS.TotalBurned(), fee)
	}
}
