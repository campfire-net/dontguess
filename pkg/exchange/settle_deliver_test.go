package exchange_test

// Tests for settle(deliver) content emission.
//
// Covered:
//   - Full round-trip: put → buy → match → preview-request → preview → buyer-accept →
//     operator deliver (trigger) → engine emits deliver with full content →
//     buyer receives message with content whose sha256 matches original put content.
//   - Non-operator deliver trigger is silently ignored (no content emitted).

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/campfire-net/campfire/pkg/store"

	"github.com/3dl-dev/dontguess/pkg/exchange"
)

// buildDeliverableState seeds a full flow up to buyer-accept (preview path),
// returning the deliver-ready state: deliver trigger message sent by operator.
// Returns the operator deliver message and the original content bytes.
func buildDeliverableState(t *testing.T, h *testHarness, eng *exchange.Engine) (
	deliverMsg *exchange.Message,
	originalContent []byte,
) {
	t.Helper()

	// Step 1: Seller puts cached inference with known content.
	desc := "Go HTTP handler unit test generator for TestSettleDeliver"
	originalContent = []byte("cached inference result: " + desc + " " +
		"func TestHTTPHandler(t *testing.T) { /* full test body */ }")

	contentB64 := base64.StdEncoding.EncodeToString(originalContent)
	putPayloadBytes, _ := json.Marshal(map[string]any{
		"description":  desc,
		"content":      contentB64,
		"token_cost":   int64(12000),
		"content_type": "code",
		"domains":      []string{"go", "testing"},
	})

	putMsg := h.sendMessage(h.seller, putPayloadBytes,
		[]string{exchange.TagPut, "exchange:content-type:code"},
		nil,
	)

	// Replay to pick up the put.
	msgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	// Step 2: Operator accepts the put.
	if err := eng.AutoAcceptPut(putMsg.ID, 8400, time.Now().Add(168*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut: %v", err)
	}

	inv := eng.State().Inventory()
	if len(inv) == 0 {
		t.Fatal("expected inventory entry after put-accept")
	}
	entryID := inv[0].EntryID

	// Step 3: Buyer sends a buy and engine matches.
	buyMsg := h.sendMessage(h.buyer,
		buyPayload("Generate unit tests for a Go HTTP handler accepting JSON POST", 50000),
		[]string{exchange.TagBuy},
		nil,
	)
	buyRec, _ := h.st.GetMessage(buyMsg.ID)
	eng.State().Apply(exchange.FromStoreRecord(buyRec))

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
	matchRec := matchMsgs[len(matchMsgs)-1]

	// Step 4: Buyer sends preview-request.
	preqPayload, _ := json.Marshal(map[string]any{
		"phase":    "preview-request",
		"entry_id": entryID,
	})
	preqMsg := h.sendMessage(h.buyer, preqPayload,
		[]string{exchange.TagSettle, exchange.TagPhasePrefix + exchange.SettlePhaseStrPreviewRequest},
		[]string{matchRec.ID},
	)
	preqRec, _ := h.st.GetMessage(preqMsg.ID)
	eng.State().Apply(exchange.FromStoreRecord(preqRec))

	// Step 5: Engine dispatches preview-request → emits settle(preview).
	if err := eng.DispatchForTest(exchange.FromStoreRecord(preqRec)); err != nil {
		t.Fatalf("DispatchForTest preview-request: %v", err)
	}

	// Find the emitted preview message.
	allMsgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	previewMsgs, _ := h.st.ListMessages(h.cfID, 0,
		store.MessageFilter{Tags: []string{exchange.TagSettle, exchange.TagPhasePrefix + exchange.SettlePhaseStrPreview}})
	if len(previewMsgs) == 0 {
		t.Fatal("no preview message emitted")
	}
	previewRec := previewMsgs[len(previewMsgs)-1]

	// Step 6: Buyer accepts (antecedent = preview message).
	buyerAcceptPayloadBytes, _ := json.Marshal(map[string]any{
		"phase":    "buyer-accept",
		"entry_id": entryID,
		"accepted": true,
	})
	buyerAcceptMsg := h.sendMessage(h.buyer, buyerAcceptPayloadBytes,
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrBuyerAccept,
			exchange.TagVerdictPrefix + "accepted",
		},
		[]string{previewRec.ID},
	)

	allMsgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	// Step 7: Operator sends settle(deliver) trigger — no content field (just metadata).
	deliverTriggerPayload, _ := json.Marshal(map[string]any{
		"phase":        "deliver",
		"entry_id":     entryID,
		"content_ref":  fmt.Sprintf("sha256:%064x", sha256.Sum256(originalContent)),
		"content_size": len(originalContent),
	})
	deliverMsg = h.sendMessage(h.operator, deliverTriggerPayload,
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrDeliver,
		},
		[]string{buyerAcceptMsg.ID},
	)

	allMsgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	return deliverMsg, originalContent
}

// TestSettleDeliver_ContentDelivered verifies the full round-trip: after the operator
// sends settle(deliver), the engine emits a message with the full content, and the
// content sha256 matches the original put content.
func TestSettleDeliver_ContentDelivered(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	deliverMsg, originalContent := buildDeliverableState(t, h, eng)

	// Count settle messages before dispatching the operator deliver trigger.
	preMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagSettle}})
	preCount := len(preMsgs)

	// Dispatch the operator deliver trigger through the engine.
	deliverRec, _ := h.st.GetMessage(deliverMsg.ID)
	if err := eng.DispatchForTest(exchange.FromStoreRecord(deliverRec)); err != nil {
		t.Fatalf("DispatchForTest deliver: %v", err)
	}

	// Engine must have emitted at least one new settle message.
	postMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagSettle}})
	if len(postMsgs) <= preCount {
		t.Fatalf("settle(deliver) dispatch: expected engine to emit content message, got no new settle messages")
	}

	// Find the engine-emitted deliver content message (operator-signed, phase=deliver, has content field).
	var contentMsg *store.MessageRecord
	for i := range postMsgs {
		m := &postMsgs[i]
		if m.Sender != h.operator.PublicKeyHex() {
			continue
		}
		// Must be a deliver phase message.
		hasDeliverPhase := false
		for _, tag := range m.Tags {
			if tag == exchange.TagPhasePrefix+exchange.SettlePhaseStrDeliver {
				hasDeliverPhase = true
				break
			}
		}
		if !hasDeliverPhase {
			continue
		}
		// Must have a content field.
		var payload map[string]any
		if err := json.Unmarshal(m.Payload, &payload); err != nil {
			continue
		}
		if _, hasContent := payload["content"]; !hasContent {
			continue
		}
		contentMsg = m
		break
	}

	if contentMsg == nil {
		t.Fatal("engine did not emit a settle(deliver) message with content field")
	}

	// Decode and verify the content matches the original put content.
	var payload struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(contentMsg.Payload, &payload); err != nil {
		t.Fatalf("unmarshal deliver content payload: %v", err)
	}
	deliveredBytes, err := base64.StdEncoding.DecodeString(payload.Content)
	if err != nil {
		t.Fatalf("base64-decode delivered content: %v", err)
	}

	// Content must match what the seller originally put.
	originalHash := sha256.Sum256(originalContent)
	deliveredHash := sha256.Sum256(deliveredBytes)
	if originalHash != deliveredHash {
		t.Errorf("delivered content hash mismatch:\n  got  sha256:%x\n  want sha256:%x",
			deliveredHash, originalHash)
	}

	// The content message antecedent must be the operator's deliver trigger.
	hasDeliverAntecedent := false
	for _, ant := range contentMsg.Antecedents {
		if ant == deliverMsg.ID {
			hasDeliverAntecedent = true
			break
		}
	}
	if !hasDeliverAntecedent {
		t.Errorf("engine deliver content message antecedents = %v, want to include deliver trigger %s",
			contentMsg.Antecedents, deliverMsg.ID[:8])
	}
}

// TestSettleDeliver_NonOperatorIgnored verifies that a deliver trigger from a
// non-operator sender does not cause content emission (operator auth gate).
func TestSettleDeliver_NonOperatorIgnored(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	_, originalContent := buildDeliverableState(t, h, eng)
	_ = originalContent

	// Count settle messages before the attacker's deliver.
	preMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagSettle}})

	// Attacker (buyer) sends a deliver trigger.
	inv := eng.State().Inventory()
	if len(inv) == 0 {
		t.Fatal("expected inventory entry")
	}
	fakeDeliverPayload, _ := json.Marshal(map[string]any{
		"phase":    "deliver",
		"entry_id": inv[0].EntryID,
	})
	fakeMsg := h.sendMessage(h.buyer, fakeDeliverPayload,
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrDeliver,
		},
		nil,
	)
	fakeRec, _ := h.st.GetMessage(fakeMsg.ID)
	eng.State().Apply(exchange.FromStoreRecord(fakeRec))

	if err := eng.DispatchForTest(exchange.FromStoreRecord(fakeRec)); err != nil {
		t.Fatalf("DispatchForTest fake deliver: %v", err)
	}

	postMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagSettle}})
	// The fake deliver message itself was written (by sendMessage), so count it.
	// Engine should NOT have emitted an additional content message.
	// preMsgs includes messages up to buildDeliverableState; fakeMsg adds 1 (by sendMessage).
	// Engine should emit 0 new messages in response.
	engineEmitted := 0
	for i := range postMsgs {
		m := &postMsgs[i]
		if m.Sender == h.operator.PublicKeyHex() && m.ID != fakeMsg.ID {
			// Check if it was emitted after the preCount baseline.
			alreadyCounted := false
			for _, pm := range preMsgs {
				if pm.ID == m.ID {
					alreadyCounted = true
					break
				}
			}
			if !alreadyCounted {
				var p map[string]any
				json.Unmarshal(m.Payload, &p)
				if _, hasContent := p["content"]; hasContent {
					engineEmitted++
				}
			}
		}
	}
	if engineEmitted > 0 {
		t.Errorf("engine emitted %d content messages in response to non-operator deliver trigger, want 0", engineEmitted)
	}
}
