package exchange_test

// Tests for the small-content automated dispute path (convention §small-content-dispute).
//
// When content is below SmallContentThreshold tokens, buyers get an automated
// refund path with no operator involvement. The seller's reputation is penalized
// by SmallContentReputationPenalty (3) per refund.

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

// setupSmallContentEntry sets up a small-content inventory entry with token_cost < 500.
// Returns the entryID and putMsgID.
func setupSmallContentEntry(t *testing.T, h *testHarness, eng *exchange.Engine) (entryID, putMsgID string) {
	t.Helper()
	// token_cost = 100 (< SmallContentThreshold 500)
	// content_size = 200 bytes (< 500 * 4 = 2000 bytes)
	putMsg := h.sendMessage(h.seller,
		putPayload("Tiny Go function one-liner", "sha256:"+fmt.Sprintf("%064x", 99), "code", 100, 200),
		[]string{exchange.TagPut, "exchange:content-type:code", "exchange:domain:go"},
		nil,
	)

	msgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(msgs)

	if err := eng.AutoAcceptPut(putMsg.ID, 70, time.Now().Add(72*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut: %v", err)
	}

	inv := eng.State().Inventory()
	if len(inv) != 1 {
		t.Fatalf("expected 1 inventory entry, got %d", len(inv))
	}
	return inv[0].EntryID, putMsg.ID
}

// setupLargeContentEntry sets up an inventory entry with token_cost >= 500 (not small).
func setupLargeContentEntry(t *testing.T, h *testHarness, eng *exchange.Engine) (entryID, putMsgID string) {
	t.Helper()
	// token_cost = 10000 (>= SmallContentThreshold 500)
	// content_size = 50000 bytes (>= 500 * 4 = 2000 bytes)
	putMsg := h.sendMessage(h.seller,
		putPayload("Large Go HTTP handler unit test generator", "sha256:"+fmt.Sprintf("%064x", 88), "code", 10000, 50000),
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
	return inv[0].EntryID, putMsg.ID
}

// smallContentDisputePayload builds a settle(small-content-dispute) JSON payload.
func smallContentDisputePayload(entryID string) []byte {
	p, _ := json.Marshal(map[string]any{
		"phase":    "small-content-dispute",
		"entry_id": entryID,
		"reason":   "content too small for preview — auto-refund requested",
	})
	return p
}

// buildDeliverChain builds the full chain: buy → match → buyer-accept → deliver.
// Returns the deliver message record.
func buildDeliverChain(t *testing.T, h *testHarness, eng *exchange.Engine, entryID string) *deliverChainResult {
	t.Helper()

	// Buyer sends a buy.
	buyMsg := h.sendMessage(h.buyer,
		buyPayload("Small Go function for testing", 10000),
		[]string{exchange.TagBuy},
		nil,
	)

	// Apply buy to state.
	buyRec, err := h.st.GetMessage(buyMsg.ID)
	if err != nil {
		t.Fatalf("getting buy message: %v", err)
	}
	eng.State().Apply(buyRec)

	// Operator emits a match referencing the entry.
	matchPayload, _ := json.Marshal(map[string]any{
		"results": []map[string]any{
			{
				"entry_id":   entryID,
				"price":      int64(70),
				"confidence": 0.9,
			},
		},
	})
	matchMsg := h.sendMessage(h.operator, matchPayload,
		[]string{exchange.TagMatch},
		[]string{buyMsg.ID},
	)

	// Replay to pick up the match.
	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(allMsgs)

	// Buyer sends buyer-accept referencing the match.
	buyerAcceptPayload, _ := json.Marshal(map[string]any{
		"phase":    "buyer-accept",
		"entry_id": entryID,
		"accepted": true,
	})
	buyerAcceptMsg := h.sendMessage(h.buyer, buyerAcceptPayload,
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrBuyerAccept,
			exchange.TagVerdictPrefix + "accepted",
		},
		[]string{matchMsg.ID},
	)

	allMsgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(allMsgs)

	// Operator delivers content.
	deliverPayload, _ := json.Marshal(map[string]any{
		"phase":        "deliver",
		"entry_id":     entryID,
		"content_ref":  "sha256:" + fmt.Sprintf("%064x", 99),
		"content_size": 200,
	})
	deliverMsg := h.sendMessage(h.operator, deliverPayload,
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrDeliver,
		},
		[]string{buyerAcceptMsg.ID},
	)

	allMsgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(allMsgs)

	return &deliverChainResult{
		buyMsgID:     buyMsg.ID,
		matchMsgID:   matchMsg.ID,
		deliverMsgID: deliverMsg.ID,
	}
}

type deliverChainResult struct {
	buyMsgID     string
	matchMsgID   string
	deliverMsgID string
}

// TestState_SmallContentDispute_IncrementsDisputeCount verifies that a valid
// small-content-dispute increments smallContentDisputes for the entry.
func TestState_SmallContentDispute_IncrementsDisputeCount(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	entryID, _ := setupSmallContentEntry(t, h, eng)
	chain := buildDeliverChain(t, h, eng, entryID)

	// Before dispute: count is 0.
	if got := eng.State().SmallContentDisputeCount(entryID); got != 0 {
		t.Errorf("SmallContentDisputeCount before dispute = %d, want 0", got)
	}

	// Buyer files a small-content-dispute referencing the deliver message.
	h.sendMessage(h.buyer, smallContentDisputePayload(entryID),
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrSmallContentDispute,
		},
		[]string{chain.deliverMsgID},
	)

	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(allMsgs)

	// Dispute count must be 1.
	if got := eng.State().SmallContentDisputeCount(entryID); got != 1 {
		t.Errorf("SmallContentDisputeCount after dispute = %d, want 1", got)
	}
}

// TestState_SmallContentDispute_SellerReputationDecreasedBy3 verifies that a valid
// small-content-dispute decreases seller reputation by SmallContentReputationPenalty (3).
func TestState_SmallContentDispute_SellerReputationDecreasedBy3(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	entryID, _ := setupSmallContentEntry(t, h, eng)
	chain := buildDeliverChain(t, h, eng, entryID)

	repBefore := eng.State().SellerReputation(h.seller.PublicKeyHex())

	// Buyer files dispute.
	h.sendMessage(h.buyer, smallContentDisputePayload(entryID),
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrSmallContentDispute,
		},
		[]string{chain.deliverMsgID},
	)

	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(allMsgs)

	repAfter := eng.State().SellerReputation(h.seller.PublicKeyHex())
	const expectedPenalty = exchange.SmallContentReputationPenalty // 3
	if repAfter != repBefore-expectedPenalty {
		t.Errorf("seller reputation after small-content-dispute = %d, want %d (before=%d - penalty=%d)",
			repAfter, repBefore-expectedPenalty, repBefore, expectedPenalty)
	}
}

// TestState_SmallContentDispute_SmallContentRefundCountIncremented verifies that
// SellerSmallContentRefundCount is incremented on each auto-refund.
func TestState_SmallContentDispute_SmallContentRefundCountIncremented(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	entryID, _ := setupSmallContentEntry(t, h, eng)
	chain := buildDeliverChain(t, h, eng, entryID)

	if got := eng.State().SellerSmallContentRefundCount(h.seller.PublicKeyHex()); got != 0 {
		t.Errorf("SellerSmallContentRefundCount before dispute = %d, want 0", got)
	}

	h.sendMessage(h.buyer, smallContentDisputePayload(entryID),
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrSmallContentDispute,
		},
		[]string{chain.deliverMsgID},
	)

	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(allMsgs)

	if got := eng.State().SellerSmallContentRefundCount(h.seller.PublicKeyHex()); got != 1 {
		t.Errorf("SellerSmallContentRefundCount after dispute = %d, want 1", got)
	}
}

// TestState_SmallContentDispute_RejectedForLargeContent verifies that a
// small-content-dispute on content >= 500 tokens is silently rejected.
// No reputation penalty and no dispute count increment should occur.
func TestState_SmallContentDispute_RejectedForLargeContent(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	entryID, _ := setupLargeContentEntry(t, h, eng)
	chain := buildDeliverChain(t, h, eng, entryID)

	repBefore := eng.State().SellerReputation(h.seller.PublicKeyHex())

	// Buyer tries to file small-content-dispute on large content.
	h.sendMessage(h.buyer, smallContentDisputePayload(entryID),
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrSmallContentDispute,
		},
		[]string{chain.deliverMsgID},
	)

	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(allMsgs)

	// Dispute must be rejected — count stays 0.
	if got := eng.State().SmallContentDisputeCount(entryID); got != 0 {
		t.Errorf("SmallContentDisputeCount for large content = %d, want 0 (rejected)", got)
	}

	// Reputation must be unchanged.
	repAfter := eng.State().SellerReputation(h.seller.PublicKeyHex())
	if repAfter != repBefore {
		t.Errorf("reputation changed for large-content dispute: before=%d after=%d, want no change",
			repBefore, repAfter)
	}

	// SmallContentRefundCount must be 0.
	if got := eng.State().SellerSmallContentRefundCount(h.seller.PublicKeyHex()); got != 0 {
		t.Errorf("SellerSmallContentRefundCount for rejected dispute = %d, want 0", got)
	}
}

// TestState_SmallContentDispute_WrongBuyerIgnored verifies that a small-content-dispute
// sent by a party other than the original buyer is silently ignored.
func TestState_SmallContentDispute_WrongBuyerIgnored(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	entryID, _ := setupSmallContentEntry(t, h, eng)
	chain := buildDeliverChain(t, h, eng, entryID)

	repBefore := eng.State().SellerReputation(h.seller.PublicKeyHex())

	// The SELLER (not the buyer) tries to file a small-content-dispute.
	h.sendMessage(h.seller, smallContentDisputePayload(entryID),
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrSmallContentDispute,
		},
		[]string{chain.deliverMsgID},
	)

	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(allMsgs)

	// Dispute count must stay 0 — wrong sender.
	if got := eng.State().SmallContentDisputeCount(entryID); got != 0 {
		t.Errorf("SmallContentDisputeCount for wrong-buyer dispute = %d, want 0", got)
	}

	// Reputation must be unchanged.
	repAfter := eng.State().SellerReputation(h.seller.PublicKeyHex())
	if repAfter != repBefore {
		t.Errorf("reputation changed for wrong-buyer dispute: before=%d after=%d, want no change",
			repBefore, repAfter)
	}
}

// TestState_SmallContentDispute_MultipleDisputes_CumulativePenalty verifies that
// multiple small-content disputes accumulate the reputation penalty correctly:
// 1 dispute → -3, 2 disputes → -6, 3 disputes → -9.
//
// To generate multiple disputes we need multiple deliver chains (one per dispute).
// We reuse the same entry but set up separate buy→match→accept→deliver chains.
func TestState_SmallContentDispute_MultipleDisputes_CumulativePenalty(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	entryID, _ := setupSmallContentEntry(t, h, eng)

	repBase := eng.State().SellerReputation(h.seller.PublicKeyHex())

	for i := 0; i < 3; i++ {
		chain := buildDeliverChain(t, h, eng, entryID)

		h.sendMessage(h.buyer, smallContentDisputePayload(entryID),
			[]string{
				exchange.TagSettle,
				exchange.TagPhasePrefix + exchange.SettlePhaseStrSmallContentDispute,
			},
			[]string{chain.deliverMsgID},
		)

		allMsgs, _ := h.st.ListMessages(h.cfID, 0)
		eng.State().Replay(allMsgs)

		expectedPenalty := (i + 1) * exchange.SmallContentReputationPenalty
		expectedRep := repBase - expectedPenalty
		if expectedRep < 0 {
			expectedRep = 0
		}
		got := eng.State().SellerReputation(h.seller.PublicKeyHex())
		if got != expectedRep {
			t.Errorf("after %d disputes: reputation = %d, want %d (base=%d penalty=%d)",
				i+1, got, expectedRep, repBase, expectedPenalty)
		}

		if got := eng.State().SmallContentDisputeCount(entryID); got != i+1 {
			t.Errorf("after %d disputes: SmallContentDisputeCount = %d, want %d", i+1, got, i+1)
		}
	}
}

// TestState_SmallContentDispute_ReputationCalcIncludesField verifies that the
// Reputation() function properly includes SmallContentRefundCount in its calculation,
// independently from other reputation factors.
func TestState_SmallContentDispute_ReputationCalcIncludesField(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	entryID, _ := setupSmallContentEntry(t, h, eng)
	chain := buildDeliverChain(t, h, eng, entryID)

	// Baseline reputation before any activity.
	repBefore := eng.State().SellerReputation(h.seller.PublicKeyHex())
	if repBefore != exchange.DefaultReputation {
		t.Errorf("baseline reputation = %d, want DefaultReputation %d", repBefore, exchange.DefaultReputation)
	}

	// File one dispute.
	h.sendMessage(h.buyer, smallContentDisputePayload(entryID),
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrSmallContentDispute,
		},
		[]string{chain.deliverMsgID},
	)

	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(allMsgs)

	repAfter := eng.State().SellerReputation(h.seller.PublicKeyHex())
	want := exchange.DefaultReputation - exchange.SmallContentReputationPenalty
	if repAfter != want {
		t.Errorf("reputation after 1 small-content dispute = %d, want %d", repAfter, want)
	}

	// SmallContentRefundCount must be exactly 1.
	if got := eng.State().SellerSmallContentRefundCount(h.seller.PublicKeyHex()); got != 1 {
		t.Errorf("SellerSmallContentRefundCount = %d, want 1", got)
	}
}
