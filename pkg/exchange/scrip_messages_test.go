package exchange_test

// Tests for scrip convention message emission in the exchange engine.
//
// Each scrip-affecting operation (buy-hold, settle, burn, dispute-refund) must
// emit a parseable convention message to the campfire so CampfireScripStore can
// replay state from the message log.
//
// These tests verify:
//   - handleBuy emits a dontguess:scrip-buy-hold message with the correct payload
//   - handleSettle(complete) emits dontguess:scrip-settle and dontguess:scrip-burn messages
//   - handleDispute emits a dontguess:scrip-dispute-refund message
//   - All emitted payloads are parseable by CampfireScripStore after a full Replay

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

// TestBuyHold_EmitsConventionMessage verifies that handleBuy emits a
// dontguess:scrip-buy-hold message parseable by CampfireScripStore.
func TestBuyHold_EmitsConventionMessage(t *testing.T) {
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

	// Seed one inventory entry; put_price = 5600, sale price = 6720, fee = 672, hold = 7392.
	seedInventoryEntry(t, h, eng, "Go HTTP test generator", "code", 8000, 5600)
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

	preBuyHold := countMsgsWithTag(t, h, scrip.TagScripBuyHold)
	preMatch := countMsgsWithTag(t, h, exchange.TagMatch)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() { _ = eng.Start(ctx) }()

	h.sendMessage(h.buyer,
		buyPayload("Generate Go HTTP handler unit tests", salePrice+5000),
		[]string{exchange.TagBuy},
		nil,
	)

	// Wait for the match message (engine processed the buy).
	preMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})
	_ = preMsgs
	matchMsg := waitForScripMessage(t, h, exchange.TagMatch, nil, 2*time.Second)
	_ = matchMsg

	// Wait for the buy-hold message to appear.
	holdMsg := waitForScripMessage(t, h, scrip.TagScripBuyHold,
		make([]store.MessageRecord, preBuyHold), 2*time.Second)
	cancel()

	// Verify match count increased.
	afterMatch := countMsgsWithTag(t, h, exchange.TagMatch)
	if afterMatch <= preMatch {
		t.Fatal("expected at least one match message")
	}

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
		t.Error("buy-hold buy_msg must be non-empty")
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
	freshCS, err := scrip.NewCampfireScripStore(h.cfID, h.st)
	if err != nil {
		t.Fatalf("NewCampfireScripStore (fresh): %v", err)
	}
	// Mint gave buyer holdAmount+5000. After buy-hold, balance = 5000.
	// But the buy-hold message subtracts from balance during replay.
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
		Transport:        h.transport,
		ScripStore:       cs,
		Logger:           func(format string, args ...any) { t.Logf("[engine] "+format, args...) },
	})

	// Seed inventory; put_price = 5600, sale_price = 6720.
	seedInventoryEntry(t, h, eng, "Terraform module generator", "code", 8000, 5600)
	inv := eng.State().Inventory()
	if len(inv) != 1 {
		t.Fatalf("expected 1 inventory entry, got %d", len(inv))
	}
	salePrice := inv[0].PutPrice * 120 / 100
	fee := salePrice / exchange.MatchingFeeRate
	holdAmount := salePrice + fee
	expectedResidual := salePrice / exchange.ResidualRate
	expectedExchangeRevenue := salePrice - expectedResidual

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

	resID := extractReservationID(t, matchMsg)
	if resID == "" {
		t.Fatal("expected non-empty reservation_id")
	}

	// Build the antecedent chain: buyer-accept → deliver → complete.
	// The seller is derived from the chain; seller_key in the payload is not trusted.
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

	// Replay so antecedent chain is in state.
	chainMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(chainMsgs)

	// Pre-count settle and burn messages.
	preSettle := countMsgsWithTag(t, h, scrip.TagScripSettle)
	preBurn := countMsgsWithTag(t, h, scrip.TagScripBurn)

	// Dispatch settle(complete) — sender is buyer; seller derived from chain.
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
	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(allMsgs)
	rec, err := h.st.GetMessage(completeMsg.ID)
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if err := eng.DispatchForTest(rec); err != nil {
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
	freshCS, err := scrip.NewCampfireScripStore(h.cfID, h.st)
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

// TestDispute_EmitsConventionMessage verifies that handleDispute emits a
// dontguess:scrip-dispute-refund message parseable by CampfireScripStore.
func TestDispute_EmitsConventionMessage(t *testing.T) {
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
	seedInventoryEntry(t, h, eng, "Security audit generator", "review", 15000, 10500)
	inv := eng.State().Inventory()
	if len(inv) != 1 {
		t.Fatalf("expected 1 inventory entry, got %d", len(inv))
	}
	salePrice := inv[0].PutPrice * 120 / 100
	fee := salePrice / exchange.MatchingFeeRate
	holdAmount := salePrice + fee

	// Seed buyer.
	addScripMintMsg(t, h, h.buyer.PublicKeyHex(), holdAmount+5000)
	if err := cs.Replay(); err != nil {
		t.Fatalf("Replay: %v", err)
	}

	preMatch, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() { _ = eng.Start(ctx) }()

	h.sendMessage(h.buyer,
		buyPayload("Audit Go HTTP handlers for security issues", salePrice+5000),
		[]string{exchange.TagBuy},
		nil,
	)

	matchMsg := waitForMatchMessage(t, h, preMatch, 2*time.Second)
	cancel()

	resID := extractReservationID(t, matchMsg)
	if resID == "" {
		t.Fatal("expected non-empty reservation_id")
	}

	// Pre-count dispute-refund messages.
	preRefund := countMsgsWithTag(t, h, scrip.TagScripDisputeRefund)

	// Dispatch settle(dispute).
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

	// Verify dispute-refund message was emitted.
	afterRefund := countMsgsWithTag(t, h, scrip.TagScripDisputeRefund)
	if afterRefund <= preRefund {
		t.Fatal("expected scrip-dispute-refund message to be emitted after dispute")
	}

	// Parse and validate the dispute-refund payload.
	refundMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{scrip.TagScripDisputeRefund}})
	refundMsg := refundMsgs[len(refundMsgs)-1]
	var rp scrip.DisputeRefundPayload
	if err := json.Unmarshal(refundMsg.Payload, &rp); err != nil {
		t.Fatalf("parsing scrip-dispute-refund payload: %v", err)
	}
	if rp.Buyer != h.buyer.PublicKeyHex() {
		t.Errorf("refund buyer = %q, want %q", rp.Buyer, h.buyer.PublicKeyHex())
	}
	if rp.Amount != holdAmount {
		t.Errorf("refund amount = %d, want %d (full hold)", rp.Amount, holdAmount)
	}
	if rp.ReservationID != resID {
		t.Errorf("refund reservation_id = %q, want %q", rp.ReservationID, resID)
	}
	if rp.DisputeMsg == "" {
		t.Error("refund dispute_msg must be non-empty")
	}
	if refundMsg.Sender != h.operator.PublicKeyHex() {
		t.Errorf("refund sender = %q, want operator %q", refundMsg.Sender, h.operator.PublicKeyHex())
	}

	// Verify CampfireScripStore can replay the refund: fresh store should show
	// buyer's balance restored to pre-buy level (mint - hold + refund = mint).
	freshCS, err := scrip.NewCampfireScripStore(h.cfID, h.st)
	if err != nil {
		t.Fatalf("NewCampfireScripStore (fresh): %v", err)
	}
	// After mint(holdAmount+5000) + buy-hold(-holdAmount) + refund(+holdAmount) = holdAmount+5000.
	buyerBal := freshCS.Balance(h.buyer.PublicKeyHex())
	if buyerBal != holdAmount+5000 {
		t.Errorf("replayed buyer balance = %d, want %d (full refund)", buyerBal, holdAmount+5000)
	}
}
