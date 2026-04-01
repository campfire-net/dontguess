package exchange_test

// Tests for the next-work prediction path: settle(complete) →
// recordBuyerSettlement → UpdateCoOccurrence → stagePredictions →
// standing brokered-match assign posted.
//
// Covered:
//   - TestEngine_Prediction_FullPath: full settle(complete) dispatch triggers
//     stagePredictions and posts a standing assign for the predicted entry.
//   - TestEngine_Prediction_FanoutCap: when MaxPredictionFanout (3) open assigns
//     already exist for a predicted entry, stagePredictions posts no additional
//     assigns (fanout cap enforced).

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/campfire-net/campfire/pkg/store"

	"github.com/campfire-net/dontguess/pkg/exchange"
)

// TestEngine_Prediction_FullPath drives a real engine through the complete settle
// path and verifies that stagePredictions posts a standing brokered-match assign
// for the predicted next entry.
//
// The engine requires a ScripStore for the settle(complete) handler to execute
// the recordBuyerSettlement + stagePredictions path (the nil-ScripStore guard is
// at the top of handleSettle, before the prediction block).
//
// Flow:
//  1. Two inventory entries (A, B) are accepted into the exchange.
//  2. The buyer's session is pre-seeded: RecordBuyerSettlementForTest records that
//     the buyer has already settled entry-B, so when entry-A settles, A↔B
//     co-occurrence is recorded and B is predicted by PredictNext(A).
//  3. A full purchase flow for entry-A runs to settle(deliver) via runFullFlowToDeliver.
//  4. The buyer sends settle(complete); DispatchForTest drives the handler, which
//     calls recordBuyerSettlement(buyer, A) → UpdateCoOccurrence(B, A) →
//     stagePredictions(A) → posts a standing brokered-match assign for B.
//  5. After dispatch, the store must contain a brokered-match assign for entry-B.
func TestEngine_Prediction_FullPath(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)

	// Wire a real CampfireScripStore: required so handleSettle does not return
	// early before reaching the recordBuyerSettlement/stagePredictions block.
	cs := newCampfireScripStore(t, h)
	eng := exchange.NewEngine(exchange.EngineOptions{
		CampfireID:  h.cfID,
		Store:       h.st,
		ReadClient:  h.newOperatorClient(),
		WriteClient: h.newOperatorClient(),
		ScripStore:  cs,
		Logger: func(format string, args ...any) {
			t.Logf("[engine] "+format, args...)
		},
	})

	// --- Step 1: Accept entry-B (the predicted follow-on entry) ---
	putMsgB := h.sendMessage(h.seller,
		putPayload("Prediction full path test — entry B (predicted)", "sha256:"+fmt.Sprintf("%064x", 701), "code", 10000, 16000),
		[]string{exchange.TagPut, "exchange:content-type:code", "exchange:domain:go"},
		nil,
	)
	msgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))
	if err := eng.AutoAcceptPut(putMsgB.ID, 7000, time.Now().Add(48*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut entry-B: %v", err)
	}
	entryBID := putMsgB.ID // EntryID == PutMsgID for accepted entries.

	// --- Step 2: Pre-seed buyer session for entry-B ---
	// Recording entry-B for the buyer before the settle-complete flow means that
	// when the buyer later settles entry-A, recordBuyerSettlement(buyer, A) will
	// pair A with B and call UpdateCoOccurrence(B, A). PredictNext(A) → returns B.
	eng.RecordBuyerSettlementForTest(h.buyer.PublicKeyHex(), entryBID)

	// --- Step 3: Run full purchase flow for entry-A to deliver ---
	// runFullFlowToDeliver handles: put → accept → buy → (engine match) →
	// buyer-accept → deliver. Returns the deliver message ID and entry-A's ID.
	_, _, deliverMsgID, entryAID := runFullFlowToDeliver(
		t, h, eng,
		"Prediction full path test — entry A (purchased)",
		700,
	)

	// Record baseline assign count before dispatch.
	preMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagAssign}})
	preCount := len(preMsgs)

	// --- Step 4: Buyer sends settle(complete) and engine dispatches it ---
	completePayload, _ := json.Marshal(map[string]any{
		"phase": "complete",
		"price": int64(7500),
	})
	completeMsgRec := h.sendMessage(h.buyer, completePayload,
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrComplete,
		},
		[]string{deliverMsgID},
	)

	// Reload state so settle(complete) and its antecedent chain are visible.
	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	// Dispatch the complete message through the real engine handler.
	// handleSettle(complete) → recordBuyerSettlement(buyer, A) →
	//   UpdateCoOccurrence(B, A) → stagePredictions(A) → assign for B.
	completeRec, err := h.st.GetMessage(completeMsgRec.ID)
	if err != nil {
		t.Fatalf("GetMessage settle(complete): %v", err)
	}
	if err := eng.DispatchForTest(exchange.FromStoreRecord(completeRec)); err != nil {
		t.Fatalf("DispatchForTest settle(complete): %v", err)
	}

	// --- Step 5: Verify a standing assign for entry-B was posted ---
	postMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagAssign}})
	if len(postMsgs) <= preCount {
		t.Fatalf("stagePredictions: expected new brokered-match assign after settle(complete), got none (pre=%d, post=%d)",
			preCount, len(postMsgs))
	}

	// Find a brokered-match assign for entry-B among the new assigns.
	found := false
	for _, rec := range postMsgs {
		var p struct {
			EntryID  string `json:"entry_id"`
			TaskType string `json:"task_type"`
		}
		if err := json.Unmarshal(rec.Payload, &p); err != nil {
			continue
		}
		if p.EntryID == entryBID && p.TaskType == "brokered-match" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("stagePredictions: no brokered-match assign found for predicted entry-B (%s)", entryBID[:8])
	}

	// Co-occurrence must have been recorded: A→B link exists (via B→A pre-seed +
	// the reverse link written when settle(complete) called UpdateCoOccurrence(B, A)).
	if cnt := eng.State().CoOccurrenceCountForTest(entryAID, entryBID); cnt == 0 {
		t.Errorf("UpdateCoOccurrence: A→B count = 0 after settle(complete) for A, want > 0")
	}
}

// TestEngine_Prediction_FanoutCap verifies that stagePredictions does NOT post a
// 4th standing brokered-match assign when MaxPredictionFanout (3) open assigns
// already exist for the predicted entry.
//
// Setup:
//  1. Two entries (A, B) in inventory.
//  2. Co-occurrence pre-seeded: PredictNext(A) returns B.
//  3. Three standing brokered-match assigns for entry-B are injected via the
//     operator (sendMessage + DispatchForTest) to fill the fanout cap.
//  4. StagePredictionsForTest("A") is called with a real engine (WriteClient set).
//  5. Assign count for entry-B must remain 3 — no 4th assign was posted.
func TestEngine_Prediction_FanoutCap(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	// --- Step 1: Accept two inventory entries ---
	putMsgA := h.sendMessage(h.seller,
		putPayload("Fanout cap test entry A", "sha256:"+fmt.Sprintf("%064x", 800), "code", 10000, 16000),
		[]string{exchange.TagPut, "exchange:content-type:code", "exchange:domain:go"},
		nil,
	)
	msgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))
	if err := eng.AutoAcceptPut(putMsgA.ID, 7000, time.Now().Add(48*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut entry-A: %v", err)
	}
	entryAID := putMsgA.ID

	putMsgB := h.sendMessage(h.seller,
		putPayload("Fanout cap test entry B", "sha256:"+fmt.Sprintf("%064x", 801), "code", 10000, 16000),
		[]string{exchange.TagPut, "exchange:content-type:code", "exchange:domain:go"},
		nil,
	)
	msgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))
	if err := eng.AutoAcceptPut(putMsgB.ID, 7000, time.Now().Add(48*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut entry-B: %v", err)
	}
	entryBID := putMsgB.ID

	// --- Step 2: Seed co-occurrence so PredictNext(A) returns B ---
	eng.State().UpdateCoOccurrence(entryAID, entryBID)
	eng.State().UpdateCoOccurrence(entryAID, entryBID)

	// Verify PredictNext(A) includes B.
	predicted := eng.State().PredictNext(entryAID)
	if len(predicted) == 0 {
		t.Fatal("PredictNext(A): expected non-empty prediction after seeding co-occurrence")
	}
	foundB := false
	for _, id := range predicted {
		if id == entryBID {
			foundB = true
			break
		}
	}
	if !foundB {
		t.Fatalf("PredictNext(A): entry-B not in predictions %v", predicted)
	}

	// --- Step 3: Pre-stage MaxPredictionFanout (3) open assigns for entry-B ---
	// Use sendMessage (operator identity) + DispatchForTest to inject them through
	// the engine's dispatch path, which calls state.Apply via the assign handler.
	deadline := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)
	for i := 0; i < exchange.MaxPredictionFanout; i++ {
		assignPayload, _ := json.Marshal(map[string]any{
			"entry_id":    entryBID,
			"task_type":   "brokered-match",
			"reward":      int64(100),
			"deadline_at": deadline,
		})
		assignRec := h.sendMessage(h.operator, assignPayload, []string{exchange.TagAssign}, nil)
		rec, err := h.st.GetMessage(assignRec.ID)
		if err != nil {
			t.Fatalf("GetMessage assign %d: %v", i, err)
		}
		if err := eng.DispatchForTest(exchange.FromStoreRecord(rec)); err != nil {
			t.Fatalf("DispatchForTest assign %d: %v", i, err)
		}
	}

	// Reload state to ensure all assigns are visible.
	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	// Confirm exactly MaxPredictionFanout open assigns exist for entry-B.
	open := eng.State().OpenPredictionAssignsForEntry(entryBID)
	if open != exchange.MaxPredictionFanout {
		t.Fatalf("pre-check: OpenPredictionAssignsForEntry(B) = %d, want %d", open, exchange.MaxPredictionFanout)
	}

	// Record assign count in the store before StagePredictionsForTest.
	preMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagAssign}})
	preCount := len(preMsgs)

	// --- Step 4: Call StagePredictionsForTest — must be a no-op for entry-B ---
	eng.StagePredictionsForTest(entryAID)

	// --- Step 5: Verify assign count did NOT increase ---
	postMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagAssign}})
	if len(postMsgs) > preCount {
		t.Errorf("fanout cap: assign count increased from %d to %d after StagePredictionsForTest; want no new assigns for entry-B (cap=%d)",
			preCount, len(postMsgs), exchange.MaxPredictionFanout)
	}

	// Confirm OpenPredictionAssignsForEntry is unchanged via state.
	openAfter := eng.State().OpenPredictionAssignsForEntry(entryBID)
	if openAfter != exchange.MaxPredictionFanout {
		t.Errorf("fanout cap: OpenPredictionAssignsForEntry(B) = %d after StagePredictionsForTest, want %d",
			openAfter, exchange.MaxPredictionFanout)
	}
}
