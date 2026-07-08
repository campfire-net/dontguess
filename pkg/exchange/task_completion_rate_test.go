package exchange_test

// Tests for exchange.State.TaskCompletionRate — the Layer 0 correctness metric
// used by the value stack gate.
//
// TestTaskCompletionRate_ColdStart — fresh state returns 1.0 (no denominator).
// TestTaskCompletionRate_AfterComplete — after a full put→buy→match→accept→
//   deliver→complete flow, TaskCompletionRate returns 1.0.
// TestTaskCompletionRate_AcceptedNotCompleted — after buyer-accept but before
//   settle(complete), TaskCompletionRate is 0.0 (0 completed / 1 accepted).
//
// Both flow tests drive the engine SYNCHRONOUSLY via DispatchForTest (the same
// exported test hook used throughout this package — see settle_deliver_test.go,
// e2e_test.go) instead of running the real-time engine event loop
// (eng.Start(ctx) in a goroutine) and polling the store with a sleep loop.
// handleBuy (invoked by DispatchForTest) emits the match message inline before
// returning, so the match is guaranteed present the moment DispatchForTest
// returns — no timing window, no t.Skip("engine may have filtered due to test
// timing"), and no reliance on wall-clock scheduling. This makes both tests
// deterministic and safe under -race.
//
// acceptedOrders/completedSettlements (the numerator/denominator backing
// TaskCompletionRate) are populated purely by State.Apply when a
// settle(buyer-accept)/settle(complete) message is replayed — see
// state_settle.go applySettleBuyerAccept/applySettleComplete — so those steps
// only need State().Replay, matching the pattern already used in e2e_test.go's
// TestFullExchangeFlow.

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/campfire-net/campfire/cf-protocol/store"

	"github.com/campfire-net/dontguess/pkg/exchange"
)

// TestTaskCompletionRate_ColdStart verifies that a fresh exchange state returns
// task_completion_rate of 1.0 (cold start: no accepted orders means no regression
// possible — the gate should never fire on an empty market).
func TestTaskCompletionRate_ColdStart(t *testing.T) {
	t.Parallel()

	st := exchange.NewState()
	rate := st.TaskCompletionRate()
	if rate != 1.0 {
		t.Errorf("cold start task_completion_rate = %.4f, want 1.0", rate)
	}
}

// taskCompletionRateFixture drives put → put-accept → buy → (synchronous)
// match → buyer-accept for the Layer 0 gate tests below. It returns the match
// message and the entry_id the buyer selected. The engine is driven entirely
// via DispatchForTest/State().Replay — no goroutine, no sleep, no timeout.
func taskCompletionRateFixture(t *testing.T, seed int) (h *testHarness, eng *exchange.Engine, matchMsg *store.MessageRecord, entryID string) {
	t.Helper()

	h = newTestHarness(t)
	eng = h.newEngine()

	// Step 1: put.
	putMsg := h.sendMessage(h.seller,
		putPayload(fmt.Sprintf("Layer0 test entry %d", seed), "sha256:"+fmt.Sprintf("%064x", seed), "code", 5000, 8000),
		[]string{exchange.TagPut, "exchange:content-type:code"},
		nil,
	)
	msgs, err := h.st.ListMessages(h.cfID, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	// Step 2: put-accept.
	if err := eng.AutoAcceptPut(putMsg.ID, 3500, time.Now().Add(24*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut: %v", err)
	}
	msgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	inv := eng.State().Inventory()
	if len(inv) == 0 {
		t.Fatal("expected inventory entry after put-accept")
	}

	// Step 3: buy, dispatched synchronously — handleBuy emits the match
	// message inline before DispatchForTest returns.
	buyMsg := h.sendMessage(h.buyer,
		buyPayload(fmt.Sprintf("Layer0 task %d", seed), 10000),
		[]string{exchange.TagBuy},
		nil,
	)
	buyRec, err := h.st.GetMessage(buyMsg.ID)
	if err != nil {
		t.Fatalf("GetMessage buy: %v", err)
	}
	eng.State().Apply(exchange.FromStoreRecord(buyRec))
	if err := eng.DispatchForTest(exchange.FromStoreRecord(buyRec)); err != nil {
		t.Fatalf("DispatchForTest(buy): %v", err)
	}

	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	matchMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})
	if len(matchMsgs) == 0 {
		t.Fatal("DispatchForTest(buy) did not emit a match message")
	}
	last := matchMsgs[len(matchMsgs)-1]
	matchMsg = &last

	var mp struct {
		Results []struct {
			EntryID string `json:"entry_id"`
		} `json:"results"`
	}
	if err := json.Unmarshal(matchMsg.Payload, &mp); err != nil {
		t.Fatalf("unmarshal match payload: %v", err)
	}
	if len(mp.Results) == 0 {
		t.Fatal("match message has no results")
	}
	entryID = mp.Results[0].EntryID

	return h, eng, matchMsg, entryID
}

// TestTaskCompletionRate_AcceptedNotCompleted verifies that after buyer-accept
// settles but before settle(complete), TaskCompletionRate is 0.0 (the denominator
// has grown but the numerator has not). This is the scenario the Layer 0 gate
// is designed to detect early: orders accepted but not delivering value.
func TestTaskCompletionRate_AcceptedNotCompleted(t *testing.T) {
	t.Parallel()

	h, eng, matchMsg, entryID := taskCompletionRateFixture(t, 11)

	// Step 5: buyer-accept only (no deliver or complete).
	acceptPayload, _ := json.Marshal(map[string]any{
		"phase":    "buyer-accept",
		"entry_id": entryID,
		"accepted": true,
	})
	h.sendMessage(h.buyer,
		acceptPayload,
		[]string{exchange.TagSettle, "exchange:phase:buyer-accept"},
		[]string{matchMsg.ID},
	)
	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	// With 1 accepted, 0 completed: rate should be 0.0.
	rate := eng.State().TaskCompletionRate()
	if rate != 0.0 {
		t.Errorf("task_completion_rate after buyer-accept (no complete) = %.4f, want 0.0", rate)
	}
}

// TestTaskCompletionRate_AfterComplete verifies that after the full
// put → put-accept → buy → match → buyer-accept → deliver → complete flow
// (driven synchronously via DispatchForTest/Replay), TaskCompletionRate
// returns 1.0.
func TestTaskCompletionRate_AfterComplete(t *testing.T) {
	t.Parallel()

	h, eng, matchMsg, entryID := taskCompletionRateFixture(t, 22)

	var mp struct {
		Results []struct {
			EntryID string `json:"entry_id"`
			Price   int64  `json:"price"`
		} `json:"results"`
	}
	if err := json.Unmarshal(matchMsg.Payload, &mp); err != nil {
		t.Fatalf("unmarshal match payload: %v", err)
	}
	salePrice := mp.Results[0].Price
	if salePrice == 0 {
		salePrice = 5000
	}

	// Step 5: buyer-accept.
	acceptPayload, _ := json.Marshal(map[string]any{
		"phase":    "buyer-accept",
		"entry_id": entryID,
		"accepted": true,
	})
	buyerAcceptMsg := h.sendMessage(h.buyer,
		acceptPayload,
		[]string{exchange.TagSettle, "exchange:phase:buyer-accept"},
		[]string{matchMsg.ID},
	)
	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	// Verify rate is 0.0 before complete.
	rateBeforeComplete := eng.State().TaskCompletionRate()
	if rateBeforeComplete != 0.0 {
		t.Errorf("task_completion_rate before complete = %.4f, want 0.0", rateBeforeComplete)
	}

	// Step 6: operator delivers.
	deliverPayload, _ := json.Marshal(map[string]any{
		"content": "cached inference result",
		"price":   salePrice,
	})
	deliverMsg := h.sendMessage(h.operator,
		deliverPayload,
		[]string{exchange.TagSettle, "exchange:phase:deliver"},
		[]string{buyerAcceptMsg.ID},
	)
	allMsgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	// Step 7: buyer completes.
	completePayload, _ := json.Marshal(map[string]any{"price": salePrice})
	h.sendMessage(h.buyer,
		completePayload,
		[]string{exchange.TagSettle, "exchange:phase:complete"},
		[]string{deliverMsg.ID},
	)
	allMsgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	// After complete: rate = 1/1 = 1.0.
	rateAfterComplete := eng.State().TaskCompletionRate()
	if rateAfterComplete != 1.0 {
		t.Errorf("task_completion_rate after complete = %.4f, want 1.0", rateAfterComplete)
	}
}
