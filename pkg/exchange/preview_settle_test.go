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
	eng.State().Replay(exchange.FromStoreRecords(msgs))

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
	eng.State().Apply(exchange.FromStoreRecord(buyRec))

	// Emit a match by dispatching the buy message through the engine.
	if err := eng.DispatchForTest(exchange.FromStoreRecord(buyRec)); err != nil {
		t.Fatalf("DispatchForTest buy: %v", err)
	}

	// Find the emitted match message.
	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

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
	eng.State().Apply(exchange.FromStoreRecord(preqRec))

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
	eng.State().Apply(exchange.FromStoreRecord(preqRec))

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
	eng.State().Apply(exchange.FromStoreRecord(preqRec))

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
	eng.State().Apply(exchange.FromStoreRecord(preqRec))

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
	eng.State().Apply(exchange.FromStoreRecord(prevRec))

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
	eng.State().Apply(exchange.FromStoreRecord(preqRec))

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
	eng.State().Apply(exchange.FromStoreRecord(prevRec))

	previewToMatch := eng.State().PreviewToMatchForTest()
	if _, ok := previewToMatch[prevMsg.ID]; ok {
		t.Error("previewToMatch should not be set for non-operator preview response")
	}
}

// TestBuyerAccept_ViaPreviewAntecedent verifies the full preview-before-purchase chain:
// buyer-accept via preview antecedent → deliver → complete → completedEntries populated,
// price record created, and seller reputation updated.
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
	eng.State().Apply(exchange.FromStoreRecord(preqRec))

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
	eng.State().Apply(exchange.FromStoreRecord(prevRec))

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
	eng.State().Apply(exchange.FromStoreRecord(acceptRec))

	// The match should now be accepted (acceptedOrders populated).
	if !eng.State().IsMatchAccepted(matchRec.ID) {
		t.Error("expected match to be accepted after buyer-accept via preview antecedent")
	}

	// Operator delivers content. Antecedent is the buyer-accept message.
	deliverMsg := h.sendMessage(h.operator,
		deliverPayloadFor(entryID),
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrDeliver,
		},
		[]string{acceptMsg.ID},
	)
	deliverRec, _ := h.st.GetMessage(deliverMsg.ID)
	eng.State().Apply(exchange.FromStoreRecord(deliverRec))

	// Buyer completes. Antecedent is the deliver message.
	completeMsg := h.sendMessage(h.buyer,
		completePayloadFor(entryID, 7000),
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrComplete,
			exchange.TagVerdictPrefix + "accepted",
		},
		[]string{deliverMsg.ID},
	)
	completeRec, _ := h.st.GetMessage(completeMsg.ID)
	eng.State().Apply(exchange.FromStoreRecord(completeRec))

	// Re-sync state from the store to pick up all messages.
	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	// After complete: seller reputation must be above the default.
	rep := eng.State().SellerReputation(h.seller.PublicKeyHex())
	if rep <= exchange.DefaultReputation {
		t.Errorf("preview path complete: seller reputation = %d, want > %d", rep, exchange.DefaultReputation)
	}

	// Price history must have exactly one record for this entry.
	hist := eng.State().PriceHistory()
	if len(hist) != 1 {
		t.Errorf("preview path complete: price history len = %d, want 1", len(hist))
	} else if hist[0].EntryID != entryID {
		t.Errorf("preview path complete: price history entry_id = %q, want %q", hist[0].EntryID, entryID)
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
	eng.State().Apply(exchange.FromStoreRecord(acceptRec))

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
	eng.State().Apply(exchange.FromStoreRecord(preqRec))

	if err := eng.DispatchForTest(exchange.FromStoreRecord(preqRec)); err != nil {
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
	eng.State().Apply(exchange.FromStoreRecord(preqRec))

	preMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagSettle}})
	preCount := len(preMsgs)

	if err := eng.DispatchForTest(exchange.FromStoreRecord(preqRec)); err != nil {
		t.Fatalf("DispatchForTest: %v", err)
	}

	postMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagSettle}})
	if len(postMsgs) != preCount {
		t.Errorf("engine emitted %d extra settle messages for invalid preview-request, want 0",
			len(postMsgs)-preCount)
	}
}

// TestEngineDispatch_PreviewRequest_AssemblerMetadataPopulated verifies that the
// engine passes correct metadata fields (ContentType, EntryID) to PreviewAssembler
// when handling a preview-request.
//
// After dontguess-nh4: seed is entry_id only. BuyerKey and MatchID are no longer
// seeding inputs and have been removed from PreviewRequest.
//
// Content delivery is not wired at this stage — the engine uses nil content and
// PreviewAssembler returns an empty chunk slice. What we can verify is that the
// emitted preview payload correctly reflects metadata derived from the inventory
// entry (ContentType, EntryID).
func TestEngineDispatch_PreviewRequest_AssemblerMetadataPopulated(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	matchRec, entryID := buildMatchedState(t, h, eng)

	// Determine the expected ContentType from the inventory.
	inv := eng.State().Inventory()
	if len(inv) == 0 {
		t.Fatal("expected inventory entry")
	}
	expectedContentType := inv[0].ContentType // "analysis" as set in buildMatchedState

	// Buyer sends preview-request.
	preqMsg := h.sendMessage(h.buyer,
		previewRequestPayload(entryID),
		[]string{exchange.TagSettle, exchange.TagPhasePrefix + exchange.SettlePhaseStrPreviewRequest},
		[]string{matchRec.ID},
	)
	preqRec, _ := h.st.GetMessage(preqMsg.ID)
	eng.State().Apply(exchange.FromStoreRecord(preqRec))

	if err := eng.DispatchForTest(exchange.FromStoreRecord(preqRec)); err != nil {
		t.Fatalf("DispatchForTest preview-request: %v", err)
	}

	// Find the emitted preview message.
	postMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagSettle}})
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

	// Parse the preview payload.
	var payload struct {
		EntryID      string `json:"entry_id"`
		ContentType  string `json:"content_type"`
		TotalTokens  int    `json:"total_tokens"`
		PreviewTokens int   `json:"preview_tokens"`
		Chunks       []any  `json:"chunks"`
	}
	if err := json.Unmarshal(previewMsg.Payload, &payload); err != nil {
		t.Fatalf("parsing preview payload: %v", err)
	}

	// ContentType in the payload must match the inventory entry's ContentType.
	// This verifies the engine read entry.ContentType and passed it to the assembler.
	if payload.ContentType != expectedContentType {
		t.Errorf("preview payload content_type = %q, want %q (from inventory entry)",
			payload.ContentType, expectedContentType)
	}

	// entry_id must reference the correct inventory entry.
	if payload.EntryID != entryID {
		t.Errorf("preview payload entry_id = %q, want %q", payload.EntryID, entryID)
	}

	// Antecedent must be the preview-request — confirming the engine used msg.ID
	// (= preview-request ID) as the antecedent and thus has the correct MatchID
	// traceable from the preview-request antecedent chain.
	if len(previewMsg.Antecedents) == 0 || previewMsg.Antecedents[0] != preqMsg.ID {
		t.Errorf("preview antecedent = %v, want [%s] (preview-request)", previewMsg.Antecedents, preqMsg.ID)
	}

	// With nil content, the assembler produces zero chunks. Verify the payload
	// reflects this rather than garbage data, confirming the assembler was called
	// and its result was faithfully serialized.
	if payload.TotalTokens != 0 {
		t.Errorf("preview payload total_tokens = %d with nil content, want 0", payload.TotalTokens)
	}
	if len(payload.Chunks) != 0 {
		t.Errorf("preview payload chunks len = %d with nil content, want 0", len(payload.Chunks))
	}
}

// TestPreviewRequest_DuplicateRejected verifies that a second preview-request from
// the same buyer for the same match is rejected at the state layer.
//
// Behavior (intended): a duplicate preview-request is a no-op. The state must not
// increment previewCountByMatch a second time, and the second preview-request must
// not be tracked in previewRequestToMatch (so the engine will not respond to it).
//
// Rationale: previewCountByMatch is used for rate limiting and anti-reconstruction
// detection. Counting duplicates from the same buyer would incorrectly inflate the
// rate-limit counter and could trigger false positives. The overwrite of
// previewsByEntry[entryID][buyerKey] is benign (same value), but the count
// increment is not — it would double-respond with two preview messages for the
// same buyer/match pair, which is both wasteful and a reconstruction risk.
func TestPreviewRequest_DuplicateRejected(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	matchRec, entryID := buildMatchedState(t, h, eng)

	// Buyer sends first preview-request.
	preqMsg1 := h.sendMessage(h.buyer,
		previewRequestPayload(entryID),
		[]string{exchange.TagSettle, exchange.TagPhasePrefix + exchange.SettlePhaseStrPreviewRequest},
		[]string{matchRec.ID},
	)
	preqRec1, _ := h.st.GetMessage(preqMsg1.ID)
	eng.State().Apply(exchange.FromStoreRecord(preqRec1))

	// Verify first request is tracked.
	countByMatch := eng.State().PreviewCountByMatchForTest()
	if got := countByMatch[matchRec.ID]; got != 1 {
		t.Fatalf("after first preview-request: previewCountByMatch = %d, want 1", got)
	}

	// Buyer sends a duplicate preview-request for the same match.
	preqMsg2 := h.sendMessage(h.buyer,
		previewRequestPayload(entryID),
		[]string{exchange.TagSettle, exchange.TagPhasePrefix + exchange.SettlePhaseStrPreviewRequest},
		[]string{matchRec.ID},
	)
	preqRec2, _ := h.st.GetMessage(preqMsg2.ID)
	eng.State().Apply(exchange.FromStoreRecord(preqRec2))

	// previewCountByMatch must still be 1 — duplicate does not increment.
	countByMatch = eng.State().PreviewCountByMatchForTest()
	if got := countByMatch[matchRec.ID]; got != 1 {
		t.Errorf("after duplicate preview-request: previewCountByMatch = %d, want 1 (duplicate must not increment)",
			got)
	}

	// The second preview-request must NOT be in previewRequestToMatch.
	// If it were tracked, the engine would respond with a second preview — which
	// is both wasteful and a potential reconstruction vector.
	reqToMatch := eng.State().PreviewRequestToMatchForTest()
	if _, ok := reqToMatch[preqMsg2.ID]; ok {
		t.Error("duplicate preview-request must not be tracked in previewRequestToMatch")
	}

	// The first request must still be correctly tracked (not clobbered).
	if got := reqToMatch[preqMsg1.ID]; got != matchRec.ID {
		t.Errorf("first preview-request tracking clobbered: previewRequestToMatch[preqMsg1] = %q, want %q",
			got, matchRec.ID)
	}

	// previewsByEntry must still map to the correct match (the overwrite is the same
	// value, so it must equal the original matchRec.ID).
	buyerKey := h.buyer.PublicKeyHex()
	byEntry := eng.State().PreviewsByEntryForTest()
	if got := byEntry[entryID][buyerKey]; got != matchRec.ID {
		t.Errorf("previewsByEntry[entry][buyer] = %q after duplicate, want %q", got, matchRec.ID)
	}
}
