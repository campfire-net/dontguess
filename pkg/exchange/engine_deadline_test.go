package exchange_test

// engine_deadline_test.go — engine-level test for the deadline-miss refund path.
//
// Finding 1 (HIGH): handleDeadlineMissRefund has no engine-level test. The full
// path — buy with guarantee_deadline_seconds → match → buyer-accept → settle(complete)
// arriving after the deadline — is exercised here using the real engine stack with a
// real campfire session and real messages.

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

// TestEngine_DeadlineMissRefund exercises the full path:
//
//  1. Buyer posts a buy with guarantee_deadline_seconds=1 (1-second deadline).
//  2. Engine emits a match (via Start).
//  3. Buyer dispatches buyer-accept, creating a scrip reservation.
//  4. Test waits for the deadline to pass (≥1s).
//  5. Buyer dispatches settle(complete) after the deadline.
//  6. Verifies: scrip:dispute-refund is emitted (not scrip-settle).
//  7. Verifies: GuaranteeForMatch returns the correct deadline and insured amount.
func TestEngine_DeadlineMissRefund(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	cs := newCampfireScripStore(t, h)
	eng := exchange.NewEngine(exchange.EngineOptions{
		CampfireID:  h.cfID,
		Store:       h.st,
		ReadClient:  h.newOperatorClient(),
		WriteClient: h.newOperatorClient(),
		ScripStore:  cs,
		Logger:      func(format string, args ...any) { t.Logf("[engine] "+format, args...) },
	})

	// Seed one inventory entry. put_price = 5600; sale_price = 6720; fee = 672; hold = 7392.
	seedInventoryEntry(t, h, eng, "deadline-miss test entry", "code", 8000, 5600)
	inv := eng.State().Inventory()
	if len(inv) != 1 {
		t.Fatalf("expected 1 inventory entry, got %d", len(inv))
	}
	entryID := inv[0].EntryID
	salePrice := eng.ComputePriceForTest(inv[0])
	fee := salePrice / exchange.MatchingFeeRate
	holdAmount := salePrice + fee

	// Insured amount is explicitly set in the buy payload.
	const insuredAmount int64 = 5000

	// Seed buyer with sufficient scrip (hold amount + margin).
	addScripMintMsg(t, h, h.buyer.PublicKeyHex(), holdAmount+10000)
	if err := cs.Replay(); err != nil {
		t.Fatalf("cs.Replay: %v", err)
	}

	// Step 1: start engine to process the buy and emit a match.
	preMatch, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { _ = eng.Start(ctx) }()

	// Send buy with guarantee_deadline_seconds=1 (1-second guarantee window).
	buyPayloadBytes, _ := json.Marshal(map[string]any{
		"task":                        "find cached inference for deadline test",
		"budget":                      salePrice + 10000,
		"max_results":                 1,
		"guarantee_deadline_seconds":  1,
		"insured_amount":              insuredAmount,
	})
	h.sendMessage(h.buyer, buyPayloadBytes, []string{exchange.TagBuy}, nil)

	// Wait for the engine to emit a match.
	matchMsg := waitForMatchMessage(t, h, preMatch, 3*time.Second)
	cancel() // stop the running engine before dispatch mode

	// Step 2: verify GuaranteeForMatch returns correct terms (deadline is in ~1s from buy).
	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	deadline, gotInsuredAmount, hasGuarantee := eng.State().GuaranteeForMatch(matchMsg.ID)
	if !hasGuarantee {
		t.Fatal("GuaranteeForMatch: expected guarantee for this match, got not-found")
	}
	if gotInsuredAmount != insuredAmount {
		t.Errorf("GuaranteeForMatch: insured_amount = %d, want %d", gotInsuredAmount, insuredAmount)
	}
	// Deadline should be approximately now+1s (within 3s window to account for engine latency).
	deadlineFromNow := time.Until(deadline)
	if deadlineFromNow > 3*time.Second || deadlineFromNow < -5*time.Second {
		t.Errorf("GuaranteeForMatch: deadline = %v, expected ~1s from buy time (got %v from now)", deadline, deadlineFromNow)
	}

	// Step 3: dispatch buyer-accept to create the reservation.
	buyerAcceptMsg := sendBuyerAcceptAndDispatch(t, h, eng, matchMsg.ID, entryID)

	// Verify reservation was created (scrip-buy-hold message present).
	holdMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{scrip.TagScripBuyHold}})
	if len(holdMsgs) == 0 {
		t.Fatal("expected scrip-buy-hold message after buyer-accept")
	}

	// Step 4: wait for the deadline to pass (guarantee_deadline_seconds=1).
	// The deadline was set at buy time; buyer-accept may have taken some time.
	// Sleep until we are sure the deadline has passed.
	if d := time.Until(deadline); d > 0 {
		time.Sleep(d + 200*time.Millisecond)
	}

	// Confirm deadline has passed.
	if time.Now().Before(deadline) {
		t.Fatalf("deadline has not passed yet (deadline=%v, now=%v); test setup issue", deadline, time.Now())
	}

	// Step 5: dispatch settle(complete) after the deadline.
	// The deliver antecedent chain: deliver → buyer-accept → match → buy.
	deliverP, _ := json.Marshal(map[string]any{
		"phase":        "deliver",
		"entry_id":     entryID,
		"content_ref":  fmt.Sprintf("sha256:%064x", 1),
		"content_size": int64(20000),
	})
	deliverMsg := h.sendMessage(h.operator, deliverP,
		[]string{exchange.TagSettle, exchange.TagPhasePrefix + exchange.SettlePhaseStrDeliver},
		[]string{buyerAcceptMsg.ID},
	)

	allMsgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	completeP, _ := json.Marshal(map[string]any{"entry_id": entryID})
	completeMsg := h.sendMessage(h.buyer, completeP,
		[]string{exchange.TagSettle, exchange.TagPhasePrefix + exchange.SettlePhaseStrComplete},
		[]string{deliverMsg.ID},
	)

	allMsgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	completeRec, err := h.st.GetMessage(completeMsg.ID)
	if err != nil {
		t.Fatalf("GetMessage settle(complete): %v", err)
	}
	preSettle := countMsgsWithTag(t, h, scrip.TagScripSettle)
	preRefund := countMsgsWithTag(t, h, scrip.TagScripDisputeRefund)

	if err := eng.DispatchForTest(exchange.FromStoreRecord(completeRec)); err != nil {
		t.Fatalf("DispatchForTest settle(complete): %v", err)
	}

	// Step 6: verify scrip:dispute-refund was emitted (not scrip-settle).
	afterRefund := countMsgsWithTag(t, h, scrip.TagScripDisputeRefund)
	afterSettle := countMsgsWithTag(t, h, scrip.TagScripSettle)

	if afterRefund <= preRefund {
		t.Errorf("expected scrip-dispute-refund to be emitted after deadline-miss settle(complete), got %d (was %d)",
			afterRefund, preRefund)
	}
	if afterSettle > preSettle {
		t.Errorf("expected NO scrip-settle after deadline-miss (refund path taken), but got %d new settle messages",
			afterSettle-preSettle)
	}

	// Step 7: parse the refund message and verify the payload fields.
	refundMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{scrip.TagScripDisputeRefund}})
	if len(refundMsgs) == 0 {
		t.Fatal("scrip-dispute-refund message not found in store")
	}
	lastRefund := refundMsgs[len(refundMsgs)-1]
	var refundPayload scrip.DisputeRefundPayload
	if err := json.Unmarshal(lastRefund.Payload, &refundPayload); err != nil {
		t.Fatalf("parsing scrip-dispute-refund payload: %v", err)
	}
	if refundPayload.Buyer != h.buyer.PublicKeyHex() {
		t.Errorf("dispute-refund buyer = %q, want %q", refundPayload.Buyer, h.buyer.PublicKeyHex())
	}
	if refundPayload.Amount != insuredAmount {
		t.Errorf("dispute-refund amount = %d, want insuredAmount=%d", refundPayload.Amount, insuredAmount)
	}
	if refundPayload.ReservationID == "" {
		t.Error("dispute-refund reservation_id must be non-empty")
	}
}

// TestEngine_DeadlineMissRefund_Idempotent verifies that a second
// settle(complete) for the same match after a deadline-miss refund does NOT
// produce a second refund. After the first refund, ClearMatchGuarantee removes
// the entry from matchGuarantee, so GuaranteeForMatch returns ok=false and the
// engine takes the normal settle path (which will fail due to the consumed
// reservation, not produce a second credit).
func TestEngine_DeadlineMissRefund_Idempotent(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	cs := newCampfireScripStore(t, h)
	eng := exchange.NewEngine(exchange.EngineOptions{
		CampfireID:  h.cfID,
		Store:       h.st,
		ReadClient:  h.newOperatorClient(),
		WriteClient: h.newOperatorClient(),
		ScripStore:  cs,
		Logger:      func(format string, args ...any) { t.Logf("[engine] "+format, args...) },
	})

	seedInventoryEntry(t, h, eng, "idempotent deadline test entry", "code", 8000, 5600)
	inv := eng.State().Inventory()
	if len(inv) != 1 {
		t.Fatalf("expected 1 inventory entry, got %d", len(inv))
	}
	entryID := inv[0].EntryID
	salePrice := eng.ComputePriceForTest(inv[0])
	fee := salePrice / exchange.MatchingFeeRate
	holdAmount := salePrice + fee

	const insuredAmount int64 = 4000

	addScripMintMsg(t, h, h.buyer.PublicKeyHex(), holdAmount+10000)
	if err := cs.Replay(); err != nil {
		t.Fatalf("cs.Replay: %v", err)
	}

	preMatch, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { _ = eng.Start(ctx) }()

	buyPayloadBytes, _ := json.Marshal(map[string]any{
		"task":                       "idempotency test",
		"budget":                     salePrice + 10000,
		"max_results":                1,
		"guarantee_deadline_seconds": 1,
		"insured_amount":             insuredAmount,
	})
	h.sendMessage(h.buyer, buyPayloadBytes, []string{exchange.TagBuy}, nil)

	matchMsg := waitForMatchMessage(t, h, preMatch, 3*time.Second)
	cancel()

	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	deadline, _, hasGuarantee := eng.State().GuaranteeForMatch(matchMsg.ID)
	if !hasGuarantee {
		t.Fatal("GuaranteeForMatch: expected guarantee for this match, got not-found")
	}

	buyerAcceptMsg := sendBuyerAcceptAndDispatch(t, h, eng, matchMsg.ID, entryID)

	if d := time.Until(deadline); d > 0 {
		time.Sleep(d + 200*time.Millisecond)
	}

	// First settle(complete) after the deadline — triggers the deadline-miss refund.
	deliverP, _ := json.Marshal(map[string]any{
		"phase":        "deliver",
		"entry_id":     entryID,
		"content_ref":  fmt.Sprintf("sha256:%064x", 2),
		"content_size": int64(20000),
	})
	deliverMsg := h.sendMessage(h.operator, deliverP,
		[]string{exchange.TagSettle, exchange.TagPhasePrefix + exchange.SettlePhaseStrDeliver},
		[]string{buyerAcceptMsg.ID},
	)

	allMsgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	completeP, _ := json.Marshal(map[string]any{"entry_id": entryID})
	completeMsg1 := h.sendMessage(h.buyer, completeP,
		[]string{exchange.TagSettle, exchange.TagPhasePrefix + exchange.SettlePhaseStrComplete},
		[]string{deliverMsg.ID},
	)

	allMsgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	completeRec1, err := h.st.GetMessage(completeMsg1.ID)
	if err != nil {
		t.Fatalf("GetMessage first settle(complete): %v", err)
	}

	// First dispatch — should produce a refund.
	preRefund := countMsgsWithTag(t, h, scrip.TagScripDisputeRefund)
	if err := eng.DispatchForTest(exchange.FromStoreRecord(completeRec1)); err != nil {
		t.Fatalf("DispatchForTest first settle(complete): %v", err)
	}
	afterFirstRefund := countMsgsWithTag(t, h, scrip.TagScripDisputeRefund)
	if afterFirstRefund <= preRefund {
		t.Fatal("expected first settle(complete) to produce a dispute-refund")
	}

	// After the first refund, GuaranteeForMatch must return ok=false (guarantee was cleared).
	_, _, stillHasGuarantee := eng.State().GuaranteeForMatch(matchMsg.ID)
	if stillHasGuarantee {
		t.Error("GuaranteeForMatch returned ok=true after deadline-miss refund; ClearMatchGuarantee did not fire")
	}

	// Second dispatch of the same settle(complete) — must NOT produce another refund.
	// (The reservation is already consumed so the engine will error out before emitting anything.)
	preRefund2 := countMsgsWithTag(t, h, scrip.TagScripDisputeRefund)
	_ = eng.DispatchForTest(exchange.FromStoreRecord(completeRec1)) // error expected; ignore it
	afterSecondRefund := countMsgsWithTag(t, h, scrip.TagScripDisputeRefund)
	if afterSecondRefund > preRefund2 {
		t.Errorf("second settle(complete) produced %d extra dispute-refund message(s); double-spend not prevented",
			afterSecondRefund-preRefund2)
	}
}
