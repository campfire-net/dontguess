package exchange_test

// engine_auction_test.go — engine-level tests for the Vickrey auction lifecycle.
//
// Finding 1 (HIGH): sweepExpiredAuctions + engine dispatch of assign-auction-close
// has zero engine-level integration test coverage. These tests drive the full path
// through the running engine, not direct state mutation.
//
// Finding 2 (MEDIUM): handleAssignAccept pays VickreyPrice instead of Reward when
// VickreyPrice > 0. These tests verify the worker receives the second-lowest bid
// as payment, not base_reward and not their own bid.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/campfire-net/campfire/pkg/store"

	"github.com/3dl-dev/dontguess/pkg/exchange"
	"github.com/3dl-dev/dontguess/pkg/scrip"
)

// TestEngine_AuctionSweep_FullPath verifies the complete auction lifecycle through
// the running engine stack:
//
//  1. Operator posts assign with auction_window_seconds=1 (short window).
//  2. Two workers bid via assign-claim with different bid amounts.
//  3. Engine sweep detects the expired auction and emits assign-auction-close.
//  4. State finalizes: winner (lowest bidder) transitions to AssignClaimed.
//  5. Worker delivers assign-complete; operator posts assign-accept.
//  6. State transitions to AssignAccepted; assign is no longer active.
//
// This test uses the real engine event loop (h.startEngine), not DispatchForTest,
// so sweepExpiredAuctions → sendOperatorMessage → state.Apply is fully exercised.
func TestEngine_AuctionSweep_FullPath(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngineWithOpts(func(o *exchange.EngineOptions) {
		// Short poll interval so the backstop sweep fires quickly.
		o.PollInterval = 50 * time.Millisecond
	})

	worker1 := newTestAgent(t)
	worker2 := newTestAgent(t)

	const baseReward int64 = 500

	// Replay existing store messages (operator membership, etc.) into state.
	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	// Step 1: operator posts assign with a 1-second auction window.
	assignPayload, _ := json.Marshal(map[string]any{
		"entry_id":               "",
		"task_type":              "freshness",
		"reward":                 baseReward,
		"auction_window_seconds": 1,
	})
	assignMsg := h.sendMessage(h.operator, assignPayload, []string{exchange.TagAssign}, nil)

	// Step 2: both workers bid within the auction window (bids arrive before
	// the 1-second deadline elapses; the engine will apply them as AuctionBids).
	bid1Payload, _ := json.Marshal(map[string]any{"bid": int64(300)}) // lower bid — will win
	bid2Payload, _ := json.Marshal(map[string]any{"bid": int64(450)}) // higher bid — Vickrey price
	_ = h.sendMessage(worker1, bid1Payload, []string{exchange.TagAssignClaim}, []string{assignMsg.ID})
	_ = h.sendMessage(worker2, bid2Payload, []string{exchange.TagAssignClaim}, []string{assignMsg.ID})

	// Step 3: start the engine. The sweep ticker fires every 50ms. After the
	// 1-second auction window lapses, sweepExpiredAuctions() will:
	//   - call state.PendingAuctionClose() → returns assignMsg.ID
	//   - call sendOperatorMessage with TagAssignAuctionClose
	//   - call state.Apply on the emitted message → finalizes auction
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	h.startEngine(eng, ctx, cancel)

	// Poll for assign-auction-close emitted by the sweep.
	closeMsgs := auctionPollForTag(t, h, exchange.TagAssignAuctionClose, 1, 4*time.Second)
	cancel()

	if len(closeMsgs) == 0 {
		t.Fatal("expected assign-auction-close message from engine sweep, got none")
	}

	// The close message must be from the operator (engine sends as WriteClient identity).
	closeMsg := closeMsgs[len(closeMsgs)-1]
	if closeMsg.Sender != h.operator.PublicKeyHex() {
		t.Errorf("assign-auction-close sender = %q, want operator %q",
			closeMsg.Sender, h.operator.PublicKeyHex())
	}

	// The close message antecedent must be the assign ID.
	if len(closeMsg.Antecedents) == 0 || closeMsg.Antecedents[0] != assignMsg.ID {
		t.Errorf("assign-auction-close antecedents = %v, want [%q]",
			closeMsg.Antecedents, assignMsg.ID)
	}

	// State must reflect: assign is AssignClaimed with worker1 as winner (lowest bid).
	assigns := eng.State().AssignByIDForTest()
	rec, ok := assigns[assignMsg.ID]
	if !ok {
		t.Fatal("assign record not found in state after auction close")
	}
	if rec.Status != exchange.AssignClaimed {
		t.Errorf("assign status = %v, want AssignClaimed", rec.Status)
	}
	if rec.ClaimantKey != worker1.PublicKeyHex() {
		t.Errorf("claimant = %q, want worker1 %q (lowest bidder wins Vickrey)",
			rec.ClaimantKey, worker1.PublicKeyHex())
	}
	if rec.VickreyPrice <= 0 {
		t.Errorf("VickreyPrice = %d, want > 0 after auction close", rec.VickreyPrice)
	}

	// Step 4: winner delivers assign-complete. Use a fresh engine in dispatch mode
	// so we don't race the running engine (already cancelled above).
	eng2 := h.newEngineWithOpts(nil)
	allMsgs2, _ := h.st.ListMessages(h.cfID, 0)
	eng2.State().Replay(exchange.FromStoreRecords(allMsgs2))

	completePayload := []byte(`{"output":"freshness check done"}`)
	// Antecedent for assign-complete is the ClaimMsgID set during auction close.
	completeMsg := h.sendMessage(worker1, completePayload, []string{exchange.TagAssignComplete}, []string{rec.ClaimMsgID})

	allMsgs2, _ = h.st.ListMessages(h.cfID, 0)
	eng2.State().Replay(exchange.FromStoreRecords(allMsgs2))

	completeRec, err := h.st.GetMessage(completeMsg.ID)
	if err != nil {
		t.Fatalf("GetMessage complete: %v", err)
	}
	if err := eng2.DispatchForTest(exchange.FromStoreRecord(completeRec)); err != nil {
		t.Fatalf("DispatchForTest complete: %v", err)
	}

	// Step 5: operator accepts.
	acceptMsg := h.sendMessage(h.operator, []byte(`{}`), []string{exchange.TagAssignAccept}, []string{completeMsg.ID})

	allMsgs2, _ = h.st.ListMessages(h.cfID, 0)
	eng2.State().Replay(exchange.FromStoreRecords(allMsgs2))

	acceptRec, err := h.st.GetMessage(acceptMsg.ID)
	if err != nil {
		t.Fatalf("GetMessage accept: %v", err)
	}
	if err := eng2.DispatchForTest(exchange.FromStoreRecord(acceptRec)); err != nil {
		t.Fatalf("DispatchForTest accept: %v", err)
	}

	// Step 6: assign must now be in terminal state AssignPaid (accept transitions
	// Completed → Accepted, then ClaimAssignPayment transitions Accepted → Paid).
	assigns2 := eng2.State().AssignByIDForTest()
	rec2, ok := assigns2[assignMsg.ID]
	if !ok {
		t.Fatal("assign record missing after accept")
	}
	if rec2.Status != exchange.AssignPaid && rec2.Status != exchange.AssignAccepted {
		t.Errorf("assign status after accept = %v, want AssignAccepted or AssignPaid", rec2.Status)
	}
}

// TestEngine_AuctionVickreyPrice_PaymentCorrectness verifies that when a Vickrey
// auction closes with two bidders, the winner (lowest bidder) is paid the
// second-lowest bid as the clearing price — not their own bid and not base_reward.
//
// Setup:
//   - base_reward = 500
//   - worker1 bids 200 (wins — lowest bid)
//   - worker2 bids 350 (second-lowest = Vickrey clearing price)
//   - Expected payment to worker1: 350 (not 200, not 500)
func TestEngine_AuctionVickreyPrice_PaymentCorrectness(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)

	// Build scrip store for balance verification.
	cs, err := scrip.NewCampfireScripStore(h.cfID, h.newOperatorClient(), h.operator.PublicKeyHex())
	if err != nil {
		t.Fatalf("NewCampfireScripStore: %v", err)
	}
	if err := cs.Replay(); err != nil {
		t.Fatalf("cs.Replay: %v", err)
	}

	eng := exchange.NewEngine(exchange.EngineOptions{
		CampfireID:  h.cfID,
		Store:       h.st,
		ReadClient:  h.newOperatorClient(),
		WriteClient: h.newOperatorClient(),
		ScripStore:  cs,
		Logger:      func(format string, args ...any) { t.Logf("[engine] "+format, args...) },
	})

	worker1 := newTestAgent(t)
	worker2 := newTestAgent(t)

	const baseReward int64 = 500
	const bid1 int64 = 200 // worker1 — lowest bid, wins
	const bid2 int64 = 350 // worker2 — second-lowest bid = Vickrey clearing price

	// Step 1: replay current state.
	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	// Step 2: operator posts assign with a large auction window (window stays open
	// for the duration of this test; finalization is triggered manually below).
	assignPayload, _ := json.Marshal(map[string]any{
		"entry_id":               "",
		"task_type":              "validation",
		"reward":                 baseReward,
		"auction_window_seconds": 3600, // 1-hour window — won't expire during test
	})
	assignMsg := h.sendMessage(h.operator, assignPayload, []string{exchange.TagAssign}, nil)

	allMsgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	assignRec, err := h.st.GetMessage(assignMsg.ID)
	if err != nil {
		t.Fatalf("GetMessage assign: %v", err)
	}
	if err := eng.DispatchForTest(exchange.FromStoreRecord(assignRec)); err != nil {
		t.Fatalf("DispatchForTest assign: %v", err)
	}

	// Step 3: worker1 bids 200, worker2 bids 350 (both within auction window).
	bid1Payload, _ := json.Marshal(map[string]any{"bid": bid1})
	bid2Payload, _ := json.Marshal(map[string]any{"bid": bid2})
	claim1Msg := h.sendMessage(worker1, bid1Payload, []string{exchange.TagAssignClaim}, []string{assignMsg.ID})
	claim2Msg := h.sendMessage(worker2, bid2Payload, []string{exchange.TagAssignClaim}, []string{assignMsg.ID})

	allMsgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	// Dispatch both claim messages so bids are recorded in state.
	claimRec1, err := h.st.GetMessage(claim1Msg.ID)
	if err != nil {
		t.Fatalf("GetMessage claim1: %v", err)
	}
	if err := eng.DispatchForTest(exchange.FromStoreRecord(claimRec1)); err != nil {
		t.Fatalf("DispatchForTest claim1: %v", err)
	}

	claimRec2, err := h.st.GetMessage(claim2Msg.ID)
	if err != nil {
		t.Fatalf("GetMessage claim2: %v", err)
	}
	if err := eng.DispatchForTest(exchange.FromStoreRecord(claimRec2)); err != nil {
		t.Fatalf("DispatchForTest claim2: %v", err)
	}

	// Sanity check: assign is still Open (auction window not expired).
	assigns := eng.State().AssignByIDForTest()
	rec := assigns[assignMsg.ID]
	if rec == nil {
		t.Fatal("assign record not found after bids")
	}
	if rec.Status != exchange.AssignOpen {
		t.Errorf("assign status after bids = %v, want AssignOpen (window still open)", rec.Status)
	}
	if len(rec.AuctionBids) != 2 {
		t.Errorf("auction bid count = %d, want 2", len(rec.AuctionBids))
	}

	// Step 4: operator emits assign-auction-close (simulates sweepExpiredAuctions
	// firing after the deadline). Antecedent = assign message ID.
	closePayload, _ := json.Marshal(map[string]any{
		"assign_id": assignMsg.ID,
		"closed_at": time.Now().UTC().Format(time.RFC3339),
	})
	closeMsg := h.sendMessage(h.operator, closePayload, []string{exchange.TagAssignAuctionClose}, []string{assignMsg.ID})

	allMsgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	closeRec, err := h.st.GetMessage(closeMsg.ID)
	if err != nil {
		t.Fatalf("GetMessage close: %v", err)
	}
	if err := eng.DispatchForTest(exchange.FromStoreRecord(closeRec)); err != nil {
		t.Fatalf("DispatchForTest close: %v", err)
	}

	// Verify VickreyPrice = bid2 (second-lowest), not bid1 or base_reward.
	assigns = eng.State().AssignByIDForTest()
	rec = assigns[assignMsg.ID]
	if rec == nil {
		t.Fatal("assign record missing after auction close")
	}
	if rec.Status != exchange.AssignClaimed {
		t.Errorf("assign status after close = %v, want AssignClaimed", rec.Status)
	}
	if rec.ClaimantKey != worker1.PublicKeyHex() {
		t.Errorf("winner = %q, want worker1 %q (lowest bidder)", rec.ClaimantKey, worker1.PublicKeyHex())
	}
	if rec.VickreyPrice != bid2 {
		t.Errorf("VickreyPrice = %d, want %d (second-lowest bid); own bid was %d, base_reward was %d",
			rec.VickreyPrice, bid2, bid1, baseReward)
	}

	// Step 5: winner delivers assign-complete. Antecedent = ClaimMsgID (the
	// auction-close message ID, which acts as the canonical claim record).
	completePayload := []byte(`{"output":"validation done"}`)
	completeMsg := h.sendMessage(worker1, completePayload, []string{exchange.TagAssignComplete}, []string{rec.ClaimMsgID})

	allMsgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	completeRec, err := h.st.GetMessage(completeMsg.ID)
	if err != nil {
		t.Fatalf("GetMessage complete: %v", err)
	}
	if err := eng.DispatchForTest(exchange.FromStoreRecord(completeRec)); err != nil {
		t.Fatalf("DispatchForTest complete: %v", err)
	}

	// Step 6: operator accepts — engine must pay VickreyPrice (350), not Reward (500).
	acceptMsg := h.sendMessage(h.operator, []byte(`{}`), []string{exchange.TagAssignAccept}, []string{completeMsg.ID})

	allMsgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	acceptRec, err := h.st.GetMessage(acceptMsg.ID)
	if err != nil {
		t.Fatalf("GetMessage accept: %v", err)
	}
	if err := eng.DispatchForTest(exchange.FromStoreRecord(acceptRec)); err != nil {
		t.Fatalf("DispatchForTest accept: %v", err)
	}

	// Verify worker1 received bid2 (350) = Vickrey clearing price, not base_reward (500).
	worker1Balance := cs.Balance(worker1.PublicKeyHex())
	if worker1Balance != bid2 {
		t.Errorf("worker1 balance = %d, want %d (Vickrey clearing price); own_bid=%d base_reward=%d",
			worker1Balance, bid2, bid1, baseReward)
	}

	// Verify worker2 received nothing (did not win the auction).
	worker2Balance := cs.Balance(worker2.PublicKeyHex())
	if worker2Balance != 0 {
		t.Errorf("worker2 balance = %d, want 0 (did not win auction)", worker2Balance)
	}

	// Verify scrip-assign-pay was emitted with Vickrey clearing price.
	payMsgs, err := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{scrip.TagScripAssignPay}})
	if err != nil {
		t.Fatalf("listing scrip-assign-pay messages: %v", err)
	}
	if len(payMsgs) == 0 {
		t.Fatal("expected scrip-assign-pay message to be emitted, got none")
	}
	last := payMsgs[len(payMsgs)-1]
	var payLoad scrip.AssignPayPayload
	if err := json.Unmarshal(last.Payload, &payLoad); err != nil {
		t.Fatalf("parsing scrip-assign-pay payload: %v", err)
	}
	if payLoad.Worker != worker1.PublicKeyHex() {
		t.Errorf("scrip-assign-pay worker = %q, want worker1 %q",
			payLoad.Worker, worker1.PublicKeyHex())
	}
	if payLoad.Amount != bid2 {
		t.Errorf("scrip-assign-pay amount = %d, want %d (Vickrey clearing price = second-lowest bid)",
			payLoad.Amount, bid2)
	}
}

// auctionPollForTag polls h.st for messages with the given tag until at least
// minCount messages appear or the timeout elapses. Returns the messages found.
func auctionPollForTag(t *testing.T, h *testHarness, tag string, minCount int, timeout time.Duration) []store.MessageRecord {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		msgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{tag}})
		if len(msgs) >= minCount {
			return msgs
		}
		time.Sleep(50 * time.Millisecond)
	}
	msgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{tag}})
	return msgs
}
