package exchange_test

// Tests for the preview-request and preview settlement phases.
//
// Covered:
//   - preview-request with valid match → state maps populated correctly
//   - preview-request with invalid antecedent → silently ignored
//   - preview-request from wrong buyer → silently ignored
//   - preview response (operator) → previewToMatch populated
//   - preview response from non-operator → silently ignored
//   - buyer-accept with preview antecedent → resolves to correct match and entry
//   - buyer-accept with match antecedent (legacy path) → still works
//   - engine dispatch: preview-request → settle(preview) response emitted

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/campfire-net/campfire/pkg/store"

	"github.com/3dl-dev/dontguess/pkg/exchange"
)

// --- State-layer tests (white-box, package exchange) ---

// buildMatchedState seeds a minimal state with a put-accept and a match,
// returning the match message record and entry ID.
func buildMatchedState(t *testing.T, h *testHarness, eng *exchange.Engine) (matchRec store.MessageRecord, entryID string) {
	t.Helper()

	putMsg := h.sendMessage(h.seller,
		putPayload("Preview test inference", "sha256:"+fmt.Sprintf("%064x", 7777), "analysis", 10000, 16000),
		[]string{exchange.TagPut, "exchange:content-type:analysis"},
		nil,
	)
	msgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(msgs)

	if err := eng.AutoAcceptPut(putMsg.ID, 7000, time.Now().Add(168*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut: %v", err)
	}
	inv := eng.State().Inventory()
	if len(inv) == 0 {
		t.Fatal("expected inventory entry after put-accept")
	}
	entryID = inv[0].EntryID

	buyMsg := h.sendMessage(h.buyer,
		buyPayload("Summarize a complex Go API with examples", 50000),
		[]string{exchange.TagBuy},
		nil,
	)
	buyRec, _ := h.st.GetMessage(buyMsg.ID)
	eng.State().Apply(buyRec)

	// Emit a match by dispatching the buy message through the engine.
	if err := eng.DispatchForTest(buyRec); err != nil {
		t.Fatalf("DispatchForTest buy: %v", err)
	}

	// Find the emitted match message.
	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(allMsgs)

	matchMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})
	if len(matchMsgs) == 0 {
		t.Fatal("no match message emitted")
	}
	matchRec = matchMsgs[len(matchMsgs)-1]
	return matchRec, entryID
}

// previewRequestPayload builds a preview-request payload for the given entry.
func previewRequestPayload(entryID string) []byte {
	p, _ := json.Marshal(map[string]any{
		"phase":    "preview-request",
		"entry_id": entryID,
	})
	return p
}

// TestPreviewRequest_ValidMatch verifies that a well-formed preview-request from
// the correct buyer populates all three state maps correctly.
func TestPreviewRequest_ValidMatch(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	matchRec, entryID := buildMatchedState(t, h, eng)

	// Buyer sends preview-request with the match as antecedent.
	preqMsg := h.sendMessage(h.buyer,
		previewRequestPayload(entryID),
		[]string{exchange.TagSettle, exchange.TagPhasePrefix + exchange.SettlePhaseStrPreviewRequest},
		[]string{matchRec.ID},
	)
	preqRec, _ := h.st.GetMessage(preqMsg.ID)
	eng.State().Apply(preqRec)

	// Verify previewsByEntry[entryID][buyerKey] == matchRec.ID
	buyerKey := h.buyer.PublicKeyHex()
	previewByEntry := eng.State().PreviewsByEntryForTest()
	if byBuyer, ok := previewByEntry[entryID]; !ok {
		t.Errorf("previewsByEntry[%s] not set", entryID[:8])
	} else if got := byBuyer[buyerKey]; got != matchRec.ID {
		t.Errorf("previewsByEntry[entry][buyer] = %q, want %q", got, matchRec.ID)
	}

	// Verify previewCountByMatch[matchRec.ID] == 1
	countByMatch := eng.State().PreviewCountByMatchForTest()
	if got := countByMatch[matchRec.ID]; got != 1 {
		t.Errorf("previewCountByMatch[matchID] = %d, want 1", got)
	}

	// Verify previewRequestToMatch[preqMsg.ID] == matchRec.ID
	reqToMatch := eng.State().PreviewRequestToMatchForTest()
	if got := reqToMatch[preqMsg.ID]; got != matchRec.ID {
		t.Errorf("previewRequestToMatch[preqMsgID] = %q, want %q", got, matchRec.ID)
	}
}

// TestPreviewRequest_InvalidAntecedent verifies that a preview-request whose
// antecedent is not a known match message is silently ignored.
func TestPreviewRequest_InvalidAntecedent(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	_, entryID := buildMatchedState(t, h, eng)

	const fakeMatchID = "0000000000000000000000000000000000000000000000000000000000000000"

	preqMsg := h.sendMessage(h.buyer,
		previewRequestPayload(entryID),
		[]string{exchange.TagSettle, exchange.TagPhasePrefix + exchange.SettlePhaseStrPreviewRequest},
		[]string{fakeMatchID},
	)
	preqRec, _ := h.st.GetMessage(preqMsg.ID)
	eng.State().Apply(preqRec)

	// Nothing should be recorded.
	reqToMatch := eng.State().PreviewRequestToMatchForTest()
	if _, ok := reqToMatch[preqMsg.ID]; ok {
		t.Error("previewRequestToMatch should not be set for invalid antecedent")
	}
}

// TestPreviewRequest_WrongBuyer verifies that a preview-request from a sender
// who is not the original buyer is silently ignored.
func TestPreviewRequest_WrongBuyer(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	matchRec, entryID := buildMatchedState(t, h, eng)

	// Attacker sends preview-request using the operator identity.
	preqMsg := h.sendMessage(h.operator,
		previewRequestPayload(entryID),
		[]string{exchange.TagSettle, exchange.TagPhasePrefix + exchange.SettlePhaseStrPreviewRequest},
		[]string{matchRec.ID},
	)
	preqRec, _ := h.st.GetMessage(preqMsg.ID)
	eng.State().Apply(preqRec)

	reqToMatch := eng.State().PreviewRequestToMatchForTest()
	if _, ok := reqToMatch[preqMsg.ID]; ok {
		t.Error("previewRequestToMatch should not be set for wrong buyer")
	}
	countByMatch := eng.State().PreviewCountByMatchForTest()
	if countByMatch[matchRec.ID] != 0 {
		t.Errorf("previewCountByMatch should be 0 for wrong-buyer attempt, got %d", countByMatch[matchRec.ID])
	}
}

// TestPreviewResponse_PopulatesPreviewToMatch verifies that the operator's
// settle(preview) response is recorded in previewToMatch.
func TestPreviewResponse_PopulatesPreviewToMatch(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	matchRec, entryID := buildMatchedState(t, h, eng)

	// Buyer sends preview-request.
	preqMsg := h.sendMessage(h.buyer,
		previewRequestPayload(entryID),
		[]string{exchange.TagSettle, exchange.TagPhasePrefix + exchange.SettlePhaseStrPreviewRequest},
		[]string{matchRec.ID},
	)
	preqRec, _ := h.st.GetMessage(preqMsg.ID)
	eng.State().Apply(preqRec)

	// Operator sends preview response (antecedent = preview-request).
	prevPayload, _ := json.Marshal(map[string]any{
		"phase":    "preview",
		"entry_id": entryID,
		"chunks":   []any{},
	})
	prevMsg := h.sendMessage(h.operator,
		prevPayload,
		[]string{exchange.TagSettle, exchange.TagPhasePrefix + exchange.SettlePhaseStrPreview},
		[]string{preqMsg.ID},
	)
	prevRec, _ := h.st.GetMessage(prevMsg.ID)
	eng.State().Apply(prevRec)

	previewToMatch := eng.State().PreviewToMatchForTest()
	if got := previewToMatch[prevMsg.ID]; got != matchRec.ID {
		t.Errorf("previewToMatch[previewMsgID] = %q, want %q", got, matchRec.ID)
	}
}

// TestPreviewResponse_NonOperatorIgnored verifies that a settle(preview) sent
// by a non-operator is silently ignored (does not pollute previewToMatch).
func TestPreviewResponse_NonOperatorIgnored(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	matchRec, entryID := buildMatchedState(t, h, eng)

	// Buyer sends preview-request.
	preqMsg := h.sendMessage(h.buyer,
		previewRequestPayload(entryID),
		[]string{exchange.TagSettle, exchange.TagPhasePrefix + exchange.SettlePhaseStrPreviewRequest},
		[]string{matchRec.ID},
	)
	preqRec, _ := h.st.GetMessage(preqMsg.ID)
	eng.State().Apply(preqRec)

	// Attacker (seller) sends a forged preview response.
	prevPayload, _ := json.Marshal(map[string]any{
		"phase":    "preview",
		"entry_id": entryID,
	})
	prevMsg := h.sendMessage(h.seller,
		prevPayload,
		[]string{exchange.TagSettle, exchange.TagPhasePrefix + exchange.SettlePhaseStrPreview},
		[]string{preqMsg.ID},
	)
	prevRec, _ := h.st.GetMessage(prevMsg.ID)
	eng.State().Apply(prevRec)

	previewToMatch := eng.State().PreviewToMatchForTest()
	if _, ok := previewToMatch[prevMsg.ID]; ok {
		t.Error("previewToMatch should not be set for non-operator preview response")
	}
}

// TestBuyerAccept_ViaPreviewAntecedent verifies the preview-before-purchase path:
// buyer-accept with a preview message as antecedent resolves to the correct match.
func TestBuyerAccept_ViaPreviewAntecedent(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	matchRec, entryID := buildMatchedState(t, h, eng)

	// Buyer sends preview-request.
	preqMsg := h.sendMessage(h.buyer,
		previewRequestPayload(entryID),
		[]string{exchange.TagSettle, exchange.TagPhasePrefix + exchange.SettlePhaseStrPreviewRequest},
		[]string{matchRec.ID},
	)
	preqRec, _ := h.st.GetMessage(preqMsg.ID)
	eng.State().Apply(preqRec)

	// Operator sends preview response.
	prevPayload, _ := json.Marshal(map[string]any{
		"phase":    "preview",
		"entry_id": entryID,
		"chunks":   []any{},
	})
	prevMsg := h.sendMessage(h.operator,
		prevPayload,
		[]string{exchange.TagSettle, exchange.TagPhasePrefix + exchange.SettlePhaseStrPreview},
		[]string{preqMsg.ID},
	)
	prevRec, _ := h.st.GetMessage(prevMsg.ID)
	eng.State().Apply(prevRec)

	// Buyer accepts using the preview message as antecedent.
	acceptPayload, _ := json.Marshal(map[string]any{
		"phase":    "buyer-accept",
		"entry_id": entryID,
		"accepted": true,
	})
	acceptMsg := h.sendMessage(h.buyer,
		acceptPayload,
		[]string{exchange.TagSettle, exchange.TagPhasePrefix + exchange.SettlePhaseStrBuyerAccept},
		[]string{prevMsg.ID},
	)
	acceptRec, _ := h.st.GetMessage(acceptMsg.ID)
	eng.State().Apply(acceptRec)

	// The match should now be accepted (acceptedOrders populated).
	if !eng.State().IsMatchAccepted(matchRec.ID) {
		t.Error("expected match to be accepted after buyer-accept via preview antecedent")
	}
}

// TestBuyerAccept_LegacyMatchAntecedent verifies that buyer-accept with a match
// message as antecedent (legacy/small-content path) still works after the change.
func TestBuyerAccept_LegacyMatchAntecedent(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	matchRec, entryID := buildMatchedState(t, h, eng)

	// Buyer accepts directly from match (no preview step).
	acceptMsg := h.sendMessage(h.buyer,
		buyerAcceptPayloadFor(entryID),
		[]string{exchange.TagSettle, exchange.TagPhasePrefix + exchange.SettlePhaseStrBuyerAccept},
		[]string{matchRec.ID},
	)
	acceptRec, _ := h.st.GetMessage(acceptMsg.ID)
	eng.State().Apply(acceptRec)

	if !eng.State().IsMatchAccepted(matchRec.ID) {
		t.Error("expected match to be accepted after buyer-accept via legacy match antecedent")
	}
}

// TestEngineDispatch_PreviewRequest_EmitsPreviewResponse verifies that when the
// engine dispatches a settle(preview-request) from the buyer, it emits a
// settle(preview) response with the preview-request as antecedent.
func TestEngineDispatch_PreviewRequest_EmitsPreviewResponse(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	matchRec, entryID := buildMatchedState(t, h, eng)

	// Count existing settle messages before preview-request.
	preMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagSettle}})
	preCount := len(preMsgs)

	// Buyer sends preview-request; dispatch through engine.
	preqMsg := h.sendMessage(h.buyer,
		previewRequestPayload(entryID),
		[]string{exchange.TagSettle, exchange.TagPhasePrefix + exchange.SettlePhaseStrPreviewRequest},
		[]string{matchRec.ID},
	)
	preqRec, _ := h.st.GetMessage(preqMsg.ID)
	eng.State().Apply(preqRec)

	if err := eng.DispatchForTest(preqRec); err != nil {
		t.Fatalf("DispatchForTest preview-request: %v", err)
	}

	// A settle(preview) message should have been emitted.
	postMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagSettle}})
	if len(postMsgs) <= preCount {
		t.Fatal("engine did not emit a settle(preview) response")
	}

	// Find the preview message.
	var previewMsg *store.MessageRecord
	for i := range postMsgs {
		m := &postMsgs[i]
		for _, tag := range m.Tags {
			if tag == exchange.TagPhasePrefix+exchange.SettlePhaseStrPreview {
				previewMsg = m
				break
			}
		}
		if previewMsg != nil {
			break
		}
	}
	if previewMsg == nil {
		t.Fatal("no settle(preview) message found after dispatch")
	}

	// Antecedent of preview must be the preview-request message.
	if len(previewMsg.Antecedents) == 0 || previewMsg.Antecedents[0] != preqMsg.ID {
		t.Errorf("preview message antecedent = %v, want [%s]", previewMsg.Antecedents, preqMsg.ID)
	}

	// Verify the preview payload contains entry_id.
	var previewPayload map[string]any
	if err := json.Unmarshal(previewMsg.Payload, &previewPayload); err != nil {
		t.Fatalf("parsing preview payload: %v", err)
	}
	if got, _ := previewPayload["entry_id"].(string); got != entryID {
		t.Errorf("preview payload entry_id = %q, want %q", got, entryID)
	}
}

// TestEngineDispatch_PreviewRequest_InvalidAntecedent_NoResponse verifies that
// a preview-request with an unknown match antecedent is silently ignored by the
// engine (no preview response emitted).
func TestEngineDispatch_PreviewRequest_InvalidAntecedent_NoResponse(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	_, entryID := buildMatchedState(t, h, eng)

	const fakeMatchID = "0000000000000000000000000000000000000000000000000000000000000000"
	preqMsg := h.sendMessage(h.buyer,
		previewRequestPayload(entryID),
		[]string{exchange.TagSettle, exchange.TagPhasePrefix + exchange.SettlePhaseStrPreviewRequest},
		[]string{fakeMatchID},
	)
	preqRec, _ := h.st.GetMessage(preqMsg.ID)
	eng.State().Apply(preqRec)

	preMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagSettle}})
	preCount := len(preMsgs)

	if err := eng.DispatchForTest(preqRec); err != nil {
		t.Fatalf("DispatchForTest: %v", err)
	}

	postMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagSettle}})
	if len(postMsgs) != preCount {
		t.Errorf("engine emitted %d extra settle messages for invalid preview-request, want 0",
			len(postMsgs)-preCount)
	}
}
