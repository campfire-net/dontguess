package exchange_test

// Tests for dispute reputation gating (convention §7.4).
//
// A buyer filing settle(dispute) must NOT penalize seller reputation.
// Only an operator-upheld dispute (exchange:verdict:accepted on the dispute
// message) should increment DisputeCount and reduce reputation.

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/3dl-dev/dontguess/pkg/exchange"
)

// setupInventoryEntry puts and accepts a single entry, returning the entryID.
func setupInventoryEntry(t *testing.T, h *testHarness, eng *exchange.Engine) string {
	t.Helper()
	putMsg := h.sendMessage(h.seller,
		putPayload("Go HTTP handler unit test generator",
			"sha256:"+fmt.Sprintf("%064x", 42), "code", 10000, 20000),
		[]string{exchange.TagPut, "exchange:content-type:code", "exchange:domain:go"},
		nil,
	)
	msgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(msgs)

	if err := eng.AutoAcceptPut(putMsg.ID, 7000, time.Now().Add(72*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut: %v", err)
	}
	inv := eng.State().Inventory()
	if len(inv) != 1 {
		t.Fatalf("expected 1 inventory entry, got %d", len(inv))
	}
	return inv[0].EntryID
}

// disputePayload builds a settle(dispute) JSON payload for the given entry.
func disputePayload(entryID, disputeType string) []byte {
	p, _ := json.Marshal(map[string]any{
		"phase":        "dispute",
		"entry_id":     entryID,
		"dispute_type": disputeType,
		"reason":       "content did not match task requirements",
	})
	return p
}

// TestState_FiledDisputeDoesNotPenalizeReputation verifies that a buyer filing
// settle(dispute) without an operator verdict does NOT reduce seller reputation
// (convention §7.4: only "disputes upheld against seller" apply the -5 weight).
func TestState_FiledDisputeDoesNotPenalizeReputation(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	entryID := setupInventoryEntry(t, h, eng)

	repBefore := eng.State().SellerReputation(h.seller.PublicKeyHex())

	// Buyer files a dispute — no operator verdict tag.
	h.sendMessage(h.buyer, disputePayload(entryID, "content_mismatch"),
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrDispute,
		},
		nil,
	)

	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(allMsgs)

	repAfter := eng.State().SellerReputation(h.seller.PublicKeyHex())
	if repAfter != repBefore {
		t.Errorf("filed dispute (no verdict) changed seller reputation: before=%d after=%d, want no change",
			repBefore, repAfter)
	}

	// The dispute must be recorded as pending.
	if !eng.State().HasPendingDispute(entryID) {
		t.Error("filed dispute should be tracked in pending disputes")
	}
}

// TestState_FiledDisputeWithDisputedVerdictDoesNotPenalizeReputation verifies
// that a dispute tagged exchange:verdict:disputed (buyer's filing verdict) also
// does NOT penalize reputation — only exchange:verdict:accepted (operator uphold)
// triggers the penalty.
func TestState_FiledDisputeWithDisputedVerdictDoesNotPenalizeReputation(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	entryID := setupInventoryEntry(t, h, eng)

	repBefore := eng.State().SellerReputation(h.seller.PublicKeyHex())

	// Buyer files dispute with exchange:verdict:disputed — still just a filing.
	h.sendMessage(h.buyer, disputePayload(entryID, "quality_inadequate"),
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrDispute,
			exchange.TagVerdictPrefix + "disputed",
		},
		nil,
	)

	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(allMsgs)

	repAfter := eng.State().SellerReputation(h.seller.PublicKeyHex())
	if repAfter != repBefore {
		t.Errorf("filed dispute (verdict:disputed) changed seller reputation: before=%d after=%d, want no change",
			repBefore, repAfter)
	}
}

// TestState_UpheldDisputePenalizesReputation verifies that an operator-upheld
// dispute (exchange:verdict:accepted on the dispute message) DOES reduce seller
// reputation by 5 points (convention §7.4: -5 per upheld dispute).
func TestState_UpheldDisputePenalizesReputation(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	entryID := setupInventoryEntry(t, h, eng)

	repBefore := eng.State().SellerReputation(h.seller.PublicKeyHex())

	// Operator upholds the dispute by sending settle(dispute) with verdict:accepted.
	h.sendMessage(h.operator, disputePayload(entryID, "content_mismatch"),
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrDispute,
			exchange.TagVerdictPrefix + "accepted",
		},
		nil,
	)

	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(allMsgs)

	repAfter := eng.State().SellerReputation(h.seller.PublicKeyHex())
	const expectedPenalty = 5 // convention §7.4: -5 per upheld dispute
	if repAfter != repBefore-expectedPenalty {
		t.Errorf("upheld dispute: reputation = %d, want %d (before=%d - penalty=%d)",
			repAfter, repBefore-expectedPenalty, repBefore, expectedPenalty)
	}
}

// TestState_UpheldHashInvalidDisputePenalizesReputation verifies that a
// hash_invalid upheld dispute applies the full -10 combined penalty
// (convention §7.4: -5 for upheld dispute + -5 additional for hash_invalid = -10).
func TestState_UpheldHashInvalidDisputePenalizesReputation(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	entryID := setupInventoryEntry(t, h, eng)

	repBefore := eng.State().SellerReputation(h.seller.PublicKeyHex())

	// Operator upholds a hash_invalid dispute.
	h.sendMessage(h.operator, disputePayload(entryID, "hash_invalid"),
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrDispute,
			exchange.TagVerdictPrefix + "accepted",
		},
		nil,
	)

	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(allMsgs)

	repAfter := eng.State().SellerReputation(h.seller.PublicKeyHex())
	const expectedPenalty = 10 // -5 DisputeCount + -5 HashInvalidCount = -10
	if repAfter != repBefore-expectedPenalty {
		t.Errorf("upheld hash_invalid dispute: reputation = %d, want %d (before=%d - penalty=%d)",
			repAfter, repBefore-expectedPenalty, repBefore, expectedPenalty)
	}
}

// TestState_MultipleFiledDisputesNoReputation verifies that a buyer filing many
// disputes cannot game seller reputation (the security vector from the bug report).
func TestState_MultipleFiledDisputesNoReputation(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	entryID := setupInventoryEntry(t, h, eng)

	repBefore := eng.State().SellerReputation(h.seller.PublicKeyHex())

	// File 10 disputes without any operator verdict.
	for i := 0; i < 10; i++ {
		h.sendMessage(h.buyer, disputePayload(entryID, "content_mismatch"),
			[]string{
				exchange.TagSettle,
				exchange.TagPhasePrefix + exchange.SettlePhaseStrDispute,
			},
			nil,
		)
	}

	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(allMsgs)

	repAfter := eng.State().SellerReputation(h.seller.PublicKeyHex())
	if repAfter != repBefore {
		t.Errorf("10 filed disputes (no verdict) changed seller reputation: before=%d after=%d, want no change",
			repBefore, repAfter)
	}
}

// TestState_HasUpheldDispute verifies that HasUpheldDispute returns true only
// after an operator-upheld dispute (verdict:accepted), not after a mere filing.
func TestState_HasUpheldDispute(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	entryID := setupInventoryEntry(t, h, eng)

	// Before any dispute: not upheld.
	if eng.State().HasUpheldDispute(entryID) {
		t.Error("HasUpheldDispute should be false before any dispute")
	}

	// Buyer files a dispute (no verdict): still not upheld.
	h.sendMessage(h.buyer, disputePayload(entryID, "content_mismatch"),
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrDispute,
		},
		nil,
	)
	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(allMsgs)

	if eng.State().HasUpheldDispute(entryID) {
		t.Error("HasUpheldDispute should be false after filing without verdict")
	}

	// Operator upholds: now upheld.
	h.sendMessage(h.operator, disputePayload(entryID, "content_mismatch"),
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrDispute,
			exchange.TagVerdictPrefix + "accepted",
		},
		nil,
	)
	allMsgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(allMsgs)

	if !eng.State().HasUpheldDispute(entryID) {
		t.Error("HasUpheldDispute should be true after operator upholds dispute")
	}
}

// TestState_NonOperatorCannotUpholdDispute verifies that a non-operator sender
// cannot apply an operator-verdict (exchange:verdict:accepted) to damage seller
// reputation. Convention §9.5: only the operator may uphold disputes.
//
// Attack vector: any campfire member sends settle(dispute) with verdict:accepted
// to reduce a target seller's reputation without authorization.
//
// Fix (dontguess-nte): applySettleDispute rejects verdict:accepted from
// non-operator senders — upheldDisputes and reputation penalty are not applied.
func TestState_NonOperatorCannotUpholdDispute(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	entryID := setupInventoryEntry(t, h, eng)

	repBefore := eng.State().SellerReputation(h.seller.PublicKeyHex())

	// A buyer (non-operator) sends settle(dispute) with verdict:accepted.
	// This should NOT penalize reputation — only the operator may uphold.
	h.sendMessage(h.buyer, disputePayload(entryID, "content_mismatch"),
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrDispute,
			exchange.TagVerdictPrefix + "accepted",
		},
		nil,
	)

	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(allMsgs)

	// Reputation must be unchanged — non-operator verdict:accepted is ignored.
	repAfter := eng.State().SellerReputation(h.seller.PublicKeyHex())
	if repAfter != repBefore {
		t.Errorf("non-operator verdict:accepted changed seller reputation: before=%d after=%d, want no change",
			repBefore, repAfter)
	}

	// The dispute is still recorded as pending (filing is allowed from anyone),
	// but must NOT be in upheldDisputes.
	if !eng.State().HasPendingDispute(entryID) {
		t.Error("dispute should be tracked as pending (non-operator can still file)")
	}
	if eng.State().HasUpheldDispute(entryID) {
		t.Error("non-operator verdict:accepted must not mark dispute as upheld")
	}

	// Operator upholds: reputation must now change.
	h.sendMessage(h.operator, disputePayload(entryID, "content_mismatch"),
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrDispute,
			exchange.TagVerdictPrefix + "accepted",
		},
		nil,
	)
	allMsgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(allMsgs)

	repFinal := eng.State().SellerReputation(h.seller.PublicKeyHex())
	const expectedPenalty = 5
	if repFinal != repBefore-expectedPenalty {
		t.Errorf("operator upheld dispute: reputation = %d, want %d (before=%d - penalty=%d)",
			repFinal, repBefore-expectedPenalty, repBefore, expectedPenalty)
	}
	if !eng.State().HasUpheldDispute(entryID) {
		t.Error("operator verdict:accepted must mark dispute as upheld")
	}
}

// TestState_SellerDisputeCount verifies that SellerDisputeCount returns the
// correct count of upheld disputes for a seller.
func TestState_SellerDisputeCount(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	entryID := setupInventoryEntry(t, h, eng)

	if got := eng.State().SellerDisputeCount(h.seller.PublicKeyHex()); got != 0 {
		t.Errorf("SellerDisputeCount before disputes = %d, want 0", got)
	}

	// File without upholding: count stays 0.
	h.sendMessage(h.buyer, disputePayload(entryID, "content_mismatch"),
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrDispute,
		},
		nil,
	)
	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(allMsgs)

	if got := eng.State().SellerDisputeCount(h.seller.PublicKeyHex()); got != 0 {
		t.Errorf("SellerDisputeCount after filing (no verdict) = %d, want 0", got)
	}

	// Uphold: count becomes 1.
	h.sendMessage(h.operator, disputePayload(entryID, "content_mismatch"),
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrDispute,
			exchange.TagVerdictPrefix + "accepted",
		},
		nil,
	)
	allMsgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(allMsgs)

	if got := eng.State().SellerDisputeCount(h.seller.PublicKeyHex()); got != 1 {
		t.Errorf("SellerDisputeCount after upheld = %d, want 1", got)
	}
}
