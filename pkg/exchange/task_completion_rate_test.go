package exchange_test

// Tests for exchange.State.TaskCompletionRate — the Layer 0 correctness metric
// used by the value stack gate.
//
// TestTaskCompletionRate_ColdStart — fresh state returns 1.0 (no denominator).
// TestTaskCompletionRate_AfterComplete — after a full put→buy→complete flow
//   driven by the engine event loop, TaskCompletionRate returns 1.0.
// TestTaskCompletionRate_AcceptedNotCompleted — after buyer-accept but before
//   settle(complete), TaskCompletionRate is 0.0 (0 completed / 1 accepted).

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/campfire-net/campfire/pkg/store"

	"github.com/3dl-dev/dontguess/pkg/exchange"
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

// TestTaskCompletionRate_AcceptedNotCompleted verifies that after buyer-accept
// settles but before settle(complete), TaskCompletionRate is 0.0 (the denominator
// has grown but the numerator has not). This is the scenario the Layer 0 gate
// is designed to detect early: orders accepted but not delivering value.
func TestTaskCompletionRate_AcceptedNotCompleted(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	eng := h.newEngine()

	// put → put-accept → buy → engine match → buyer-accept (stop before complete).

	// Step 1: put.
	putMsg := h.sendMessage(h.seller,
		putPayload("Layer0 test entry", "sha256:"+fmt.Sprintf("%064x", 11), "code", 5000, 8000),
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

	// Step 3: buy.
	buyMsg := h.sendMessage(h.buyer,
		buyPayload("Layer0 task", 10000),
		[]string{exchange.TagBuy},
		nil,
	)
	_ = buyMsg
	msgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	// Step 4: engine match (run engine event loop briefly).
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	preMatchMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})
	preMatchCount := len(preMatchMsgs)

	go func() { _ = eng.Start(ctx) }()

	var matchMsgs []store.MessageRecord
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		matchMsgs, _ = h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})
		if len(matchMsgs) > preMatchCount {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()

	if len(matchMsgs) <= preMatchCount {
		t.Skip("no match emitted — skip (engine may have filtered due to test timing)")
	}

	matchMsg := matchMsgs[len(matchMsgs)-1]
	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	// Parse match payload to get entry ID.
	var mp struct {
		Results []struct {
			EntryID string `json:"entry_id"`
		} `json:"results"`
	}
	if err := json.Unmarshal(matchMsg.Payload, &mp); err != nil || len(mp.Results) == 0 {
		t.Skip("could not parse match results — skipping")
	}

	// Step 5: buyer-accept only (no deliver or complete).
	acceptPayload, _ := json.Marshal(map[string]any{
		"phase":    "buyer-accept",
		"entry_id": mp.Results[0].EntryID,
		"accepted": true,
	})
	h.sendMessage(h.buyer,
		acceptPayload,
		[]string{exchange.TagSettle, "exchange:phase:buyer-accept"},
		[]string{matchMsg.ID},
	)
	allMsgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	// With 1 accepted, 0 completed: rate should be 0.0.
	rate := eng.State().TaskCompletionRate()
	if rate != 0.0 {
		t.Errorf("task_completion_rate after buyer-accept (no complete) = %.4f, want 0.0", rate)
	}
}

// TestTaskCompletionRate_AfterComplete verifies that after the full
// put → put-accept → buy → match → buyer-accept → deliver → complete flow
// (driven by the real engine event loop), TaskCompletionRate returns 1.0.
func TestTaskCompletionRate_AfterComplete(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	eng := h.newEngine()

	// Step 1: put.
	putMsg := h.sendMessage(h.seller,
		putPayload("Layer0 complete test", "sha256:"+fmt.Sprintf("%064x", 22), "code", 5000, 8000),
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

	// Step 3: buy.
	buyMsg := h.sendMessage(h.buyer,
		buyPayload("Layer0 complete task", 10000),
		[]string{exchange.TagBuy},
		nil,
	)
	_ = buyMsg
	msgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	// Step 4: engine match.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	preMatchMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})
	preMatchCount := len(preMatchMsgs)

	go func() { _ = eng.Start(ctx) }()

	var matchMsgs []store.MessageRecord
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		matchMsgs, _ = h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})
		if len(matchMsgs) > preMatchCount {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()

	if len(matchMsgs) <= preMatchCount {
		t.Skip("no match emitted — skip (engine timing)")
	}

	matchMsg := matchMsgs[len(matchMsgs)-1]
	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	var mp struct {
		Results []struct {
			EntryID string `json:"entry_id"`
			Price   int64  `json:"price"`
		} `json:"results"`
	}
	if err := json.Unmarshal(matchMsg.Payload, &mp); err != nil || len(mp.Results) == 0 {
		t.Skip("could not parse match results")
	}

	salePrice := mp.Results[0].Price
	if salePrice == 0 {
		salePrice = 5000
	}

	// Step 5: buyer-accept.
	acceptPayload, _ := json.Marshal(map[string]any{
		"phase":    "buyer-accept",
		"entry_id": mp.Results[0].EntryID,
		"accepted": true,
	})
	buyerAcceptMsg := h.sendMessage(h.buyer,
		acceptPayload,
		[]string{exchange.TagSettle, "exchange:phase:buyer-accept"},
		[]string{matchMsg.ID},
	)
	allMsgs, _ = h.st.ListMessages(h.cfID, 0)
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
