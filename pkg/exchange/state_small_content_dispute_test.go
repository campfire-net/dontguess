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

// ZWR-1: Threshold boundary tests.
//
// The OR logic in applySettleSmallContentDispute:
//   token_cost < 500 OR content_size < 2000
//
// Tests each dimension independently at the exact boundary, plus mixed cases
// where one dimension is below threshold and the other is above.

// setupEntryWithCosts creates a put entry with explicit token_cost and content_size.
func setupEntryWithCosts(t *testing.T, h *testHarness, eng *exchange.Engine, tokenCost, contentSize int64, label string) (entryID string) {
	t.Helper()
	putMsg := h.sendMessage(h.seller,
		putPayload(
			"Boundary test entry: "+label,
			"sha256:"+fmt.Sprintf("%064x", tokenCost*1000+contentSize),
			"code",
			tokenCost,
			contentSize,
		),
		[]string{exchange.TagPut, "exchange:content-type:code", "exchange:domain:go"},
		nil,
	)
	msgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(msgs)
	putPrice := tokenCost * 70 / 100
	if putPrice < 1 {
		putPrice = 1
	}
	if err := eng.AutoAcceptPut(putMsg.ID, putPrice, time.Now().Add(72*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut: %v", err)
	}
	inv := eng.State().Inventory()
	if len(inv) == 0 {
		t.Fatalf("expected inventory entry after put, got 0")
	}
	return inv[len(inv)-1].EntryID
}

// TestSmallContentDispute_ThresholdBoundary_TokenCost499Accepted verifies that
// token_cost=499 qualifies as small content regardless of content_size.
// OR logic: token_cost < 500 → true, so dispute is accepted.
func TestSmallContentDispute_ThresholdBoundary_TokenCost499Accepted(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	// token_cost=499 (<500): qualifies regardless of content_size=5000 (>=2000).
	entryID := setupEntryWithCosts(t, h, eng, 499, 5000, "tc499-cs5000")
	chain := buildDeliverChain(t, h, eng, entryID)

	repBefore := eng.State().SellerReputation(h.seller.PublicKeyHex())

	h.sendMessage(h.buyer, smallContentDisputePayload(entryID),
		[]string{exchange.TagSettle, exchange.TagPhasePrefix + exchange.SettlePhaseStrSmallContentDispute},
		[]string{chain.deliverMsgID},
	)
	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(allMsgs)

	// Dispute must be accepted: count=1, reputation decreased.
	if got := eng.State().SmallContentDisputeCount(entryID); got != 1 {
		t.Errorf("token_cost=499,content_size=5000: SmallContentDisputeCount = %d, want 1 (accepted)", got)
	}
	repAfter := eng.State().SellerReputation(h.seller.PublicKeyHex())
	if repAfter != repBefore-exchange.SmallContentReputationPenalty {
		t.Errorf("token_cost=499,content_size=5000: reputation = %d, want %d (penalty applied)",
			repAfter, repBefore-exchange.SmallContentReputationPenalty)
	}
}

// TestSmallContentDispute_ThresholdBoundary_TokenCost500Rejected verifies that
// token_cost=500 does NOT qualify via the token dimension (500 is not < 500).
// content_size=5000 is also >= 2000, so the dispute is rejected.
func TestSmallContentDispute_ThresholdBoundary_TokenCost500Rejected(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	// token_cost=500 (not <500) AND content_size=5000 (not <2000): rejected.
	entryID := setupEntryWithCosts(t, h, eng, 500, 5000, "tc500-cs5000")
	chain := buildDeliverChain(t, h, eng, entryID)

	repBefore := eng.State().SellerReputation(h.seller.PublicKeyHex())

	h.sendMessage(h.buyer, smallContentDisputePayload(entryID),
		[]string{exchange.TagSettle, exchange.TagPhasePrefix + exchange.SettlePhaseStrSmallContentDispute},
		[]string{chain.deliverMsgID},
	)
	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(allMsgs)

	// Dispute must be rejected: count=0, no reputation change.
	if got := eng.State().SmallContentDisputeCount(entryID); got != 0 {
		t.Errorf("token_cost=500,content_size=5000: SmallContentDisputeCount = %d, want 0 (rejected)", got)
	}
	repAfter := eng.State().SellerReputation(h.seller.PublicKeyHex())
	if repAfter != repBefore {
		t.Errorf("token_cost=500,content_size=5000: reputation changed from %d to %d, want no change", repBefore, repAfter)
	}
}

// TestSmallContentDispute_ThresholdBoundary_ContentSize1999Accepted verifies that
// content_size=1999 qualifies as small content regardless of token_cost.
// OR logic: content_size < 2000 → true, so dispute is accepted.
func TestSmallContentDispute_ThresholdBoundary_ContentSize1999Accepted(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	// token_cost=1000 (>=500) but content_size=1999 (<2000): qualifies via content dimension.
	entryID := setupEntryWithCosts(t, h, eng, 1000, 1999, "tc1000-cs1999")
	chain := buildDeliverChain(t, h, eng, entryID)

	repBefore := eng.State().SellerReputation(h.seller.PublicKeyHex())

	h.sendMessage(h.buyer, smallContentDisputePayload(entryID),
		[]string{exchange.TagSettle, exchange.TagPhasePrefix + exchange.SettlePhaseStrSmallContentDispute},
		[]string{chain.deliverMsgID},
	)
	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(allMsgs)

	// Dispute must be accepted: count=1.
	if got := eng.State().SmallContentDisputeCount(entryID); got != 1 {
		t.Errorf("token_cost=1000,content_size=1999: SmallContentDisputeCount = %d, want 1 (accepted)", got)
	}
	repAfter := eng.State().SellerReputation(h.seller.PublicKeyHex())
	if repAfter != repBefore-exchange.SmallContentReputationPenalty {
		t.Errorf("token_cost=1000,content_size=1999: reputation = %d, want %d (penalty applied)",
			repAfter, repBefore-exchange.SmallContentReputationPenalty)
	}
}

// TestSmallContentDispute_ThresholdBoundary_ContentSize2000Rejected verifies that
// content_size=2000 does NOT qualify via the content dimension (2000 is not < 2000).
// token_cost=1000 is also >= 500, so the dispute is rejected.
func TestSmallContentDispute_ThresholdBoundary_ContentSize2000Rejected(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	// token_cost=1000 (not <500) AND content_size=2000 (not <2000): rejected.
	entryID := setupEntryWithCosts(t, h, eng, 1000, 2000, "tc1000-cs2000")
	chain := buildDeliverChain(t, h, eng, entryID)

	repBefore := eng.State().SellerReputation(h.seller.PublicKeyHex())

	h.sendMessage(h.buyer, smallContentDisputePayload(entryID),
		[]string{exchange.TagSettle, exchange.TagPhasePrefix + exchange.SettlePhaseStrSmallContentDispute},
		[]string{chain.deliverMsgID},
	)
	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(allMsgs)

	// Dispute must be rejected: count=0.
	if got := eng.State().SmallContentDisputeCount(entryID); got != 0 {
		t.Errorf("token_cost=1000,content_size=2000: SmallContentDisputeCount = %d, want 0 (rejected)", got)
	}
	repAfter := eng.State().SellerReputation(h.seller.PublicKeyHex())
	if repAfter != repBefore {
		t.Errorf("token_cost=1000,content_size=2000: reputation changed from %d to %d, want no change", repBefore, repAfter)
	}
}

// TestSmallContentDispute_ThresholdBoundary_BothBelow verifies that both dimensions
// below threshold (token_cost=499, content_size=1999) results in accepted dispute.
func TestSmallContentDispute_ThresholdBoundary_BothBelow(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	// token_cost=499 (<500) AND content_size=1999 (<2000): both dimensions qualify.
	entryID := setupEntryWithCosts(t, h, eng, 499, 1999, "tc499-cs1999")
	chain := buildDeliverChain(t, h, eng, entryID)

	repBefore := eng.State().SellerReputation(h.seller.PublicKeyHex())

	h.sendMessage(h.buyer, smallContentDisputePayload(entryID),
		[]string{exchange.TagSettle, exchange.TagPhasePrefix + exchange.SettlePhaseStrSmallContentDispute},
		[]string{chain.deliverMsgID},
	)
	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(allMsgs)

	if got := eng.State().SmallContentDisputeCount(entryID); got != 1 {
		t.Errorf("token_cost=499,content_size=1999: SmallContentDisputeCount = %d, want 1 (accepted)", got)
	}
	repAfter := eng.State().SellerReputation(h.seller.PublicKeyHex())
	if repAfter != repBefore-exchange.SmallContentReputationPenalty {
		t.Errorf("token_cost=499,content_size=1999: reputation = %d, want %d (penalty applied)",
			repAfter, repBefore-exchange.SmallContentReputationPenalty)
	}
}

// ZWR-2: Scrip refund path test.
//
// Verifies that handleSettleSmallContentDispute (engine layer) exercises the full
// scrip refund path when a ScripStore is wired in:
//   - Reservation is consumed (ConsumeReservation called)
//   - Buyer budget is restored (AddBudget called with full held amount)
//   - scrip-dispute-refund convention message is emitted

// smallContentDisputePayloadWithScrip builds a settle(small-content-dispute) payload
// that includes reservation_id and buyer_key for the engine's scrip path.
func smallContentDisputePayloadWithScrip(entryID, reservationID, buyerKey string) []byte {
	p, _ := json.Marshal(map[string]any{
		"phase":          "small-content-dispute",
		"entry_id":       entryID,
		"reason":         "content too small for preview — auto-refund requested",
		"reservation_id": reservationID,
		"buyer_key":      buyerKey,
	})
	return p
}

// TestSmallContentDispute_ScripRefundPath verifies the engine-layer scrip refund path:
// - Reservation is consumed after dispute
// - Buyer's balance is fully restored
// - scrip-dispute-refund message is emitted to the campfire
//
// Uses a real CampfireScripStore (same pattern as TestDispute_RefundsScripToBuyer)
// with a small-content entry (token_cost < SmallContentThreshold).
func TestSmallContentDispute_ScripRefundPath(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	cs := newCampfireScripStore(t, h)
	eng := exchange.NewEngine(exchange.EngineOptions{
		CampfireID:       h.cfID,
		OperatorIdentity: h.operator,
		Store:            h.st,
		Transport:        h.transport,
		ScripStore:       cs,
		Logger: func(format string, args ...any) {
			t.Logf("[engine] "+format, args...)
		},
	})

	// Seed a small-content entry: token_cost=100 (<500), content_size=200 (<2000).
	// putPrice=70; salePrice = 70 * 120/100 = 84; fee = 84/10 = 8; holdAmount = 92.
	putMsg := h.sendMessage(h.seller,
		putPayload("Tiny Go function one-liner (scrip test)", "sha256:"+fmt.Sprintf("%064x", 777), "code", 100, 200),
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
	entryID := inv[0].EntryID
	salePrice := eng.ComputePriceForTest(inv[0])
	fee := salePrice / exchange.MatchingFeeRate
	holdAmount := salePrice + fee

	// Seed buyer with enough scrip.
	addScripMintMsg(t, h, h.buyer.PublicKeyHex(), holdAmount+5000)
	if err := cs.Replay(); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	buyerBalanceBefore := cs.Balance(h.buyer.PublicKeyHex())

	// Start the engine to process the buy and emit a match (which creates the reservation).
	preMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() { _ = eng.Start(ctx) }()

	h.sendMessage(h.buyer,
		buyPayload("Small Go function for testing (scrip)", salePrice+5000),
		[]string{exchange.TagBuy},
		nil,
	)

	matchMsg := waitForMatchMessage(t, h, preMsgs, 2*time.Second)
	cancel()

	// Buyer balance must be UNCHANGED after buy (no pre-decrement in preview-before-purchase model).
	if cs.Balance(h.buyer.PublicKeyHex()) != buyerBalanceBefore {
		t.Errorf("buyer balance after buy: got %d, want %d (no pre-decrement at buy time)",
			cs.Balance(h.buyer.PublicKeyHex()), buyerBalanceBefore)
	}

	// Build the full deliver chain: dispatch buyer-accept (triggers hold), then deliver.
	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(allMsgs)

	buyerAcceptMsg := sendBuyerAcceptAndDispatch(t, h, eng, matchMsg.ID, entryID)

	// Get reservation ID from the scrip-buy-hold log message emitted by buyer-accept.
	resID := extractReservationIDFromLog(t, h)
	if resID == "" {
		t.Fatal("expected non-empty reservation_id after buyer-accept")
	}

	// Buyer balance must be decremented by holdAmount after buyer-accept.
	if cs.Balance(h.buyer.PublicKeyHex()) != buyerBalanceBefore-holdAmount {
		t.Errorf("buyer balance after buyer-accept: got %d, want %d (decremented by holdAmount)",
			cs.Balance(h.buyer.PublicKeyHex()), buyerBalanceBefore-holdAmount)
	}

	// Reservation must exist.
	if _, err := cs.GetReservation(context.Background(), resID); err != nil {
		t.Fatalf("expected reservation %s to exist after buyer-accept: %v", resID, err)
	}

	allMsgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(allMsgs)

	deliverPayloadBytes, _ := json.Marshal(map[string]any{
		"phase":        "deliver",
		"entry_id":     entryID,
		"content_ref":  "sha256:" + fmt.Sprintf("%064x", 99),
		"content_size": 200,
	})
	deliverMsg := h.sendMessage(h.operator, deliverPayloadBytes,
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrDeliver,
		},
		[]string{buyerAcceptMsg.ID},
	)
	allMsgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(allMsgs)

	// Count scrip-dispute-refund messages before dispute.
	preRefundMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{scrip.TagScripDisputeRefund}})

	// Buyer sends small-content-dispute with reservation_id and buyer_key.
	disputeMsg := h.sendMessage(h.buyer,
		smallContentDisputePayloadWithScrip(entryID, resID, h.buyer.PublicKeyHex()),
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrSmallContentDispute,
		},
		[]string{deliverMsg.ID},
	)

	allMsgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(allMsgs)

	rec, err := h.st.GetMessage(disputeMsg.ID)
	if err != nil {
		t.Fatalf("GetMessage dispute: %v", err)
	}
	if err := eng.DispatchForTest(rec); err != nil {
		t.Fatalf("DispatchForTest small-content-dispute: %v", err)
	}

	// Reservation must be consumed (deleted).
	if _, err := cs.GetReservation(context.Background(), resID); err == nil {
		t.Errorf("expected reservation %s to be consumed after small-content-dispute, still present", resID)
	}

	// Buyer balance must be fully restored.
	buyerBalanceAfter := cs.Balance(h.buyer.PublicKeyHex())
	if buyerBalanceAfter != buyerBalanceBefore {
		t.Errorf("buyer balance after small-content-dispute refund: got %d, want %d (full refund)",
			buyerBalanceAfter, buyerBalanceBefore)
	}

	// scrip-dispute-refund message must have been emitted.
	postRefundMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{scrip.TagScripDisputeRefund}})
	if len(postRefundMsgs) <= len(preRefundMsgs) {
		t.Errorf("expected scrip-dispute-refund message to be emitted after small-content-dispute")
	}
}

// ZWR-3: Missing-entry case.
//
// When an entry is removed from inventory between deliver and dispute (e.g., expiry
// or operator removal), both state and engine layers should silently drop the dispute.
// No crash, no state mutation, no scrip movement.

// TestSmallContentDispute_MissingEntry_SilentlyDropped verifies that a
// small-content-dispute is handled gracefully when the inventory entry has been
// removed (e.g., expiry between deliver and dispute).
//
// This is a real production scenario: the operator could remove/expire an entry
// after deliver but before the buyer files the dispute.
//
// Ground-source behavior (from engine.go and state.go):
//   - State layer (applySettleSmallContentDispute): silently drops — entry absent
//     means no count increment and no reputation penalty.
//   - Engine layer (handleSettleSmallContentDispute): entryForDeliver() returns nil
//     when the entry is absent; the scrip check (isSmall guard) is skipped (nil
//     guard), so the refund path proceeds — buyer is refunded (benefit of doubt).
//
// This test asserts the ground-source behavior, not an assumed "all silent" policy.
func TestSmallContentDispute_MissingEntry_SilentlyDropped(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	cs := newCampfireScripStore(t, h)
	eng := exchange.NewEngine(exchange.EngineOptions{
		CampfireID:       h.cfID,
		OperatorIdentity: h.operator,
		Store:            h.st,
		Transport:        h.transport,
		ScripStore:       cs,
		Logger: func(format string, args ...any) {
			t.Logf("[engine] "+format, args...)
		},
	})

	// Seed a small-content entry: token_cost=100, content_size=200.
	putMsg := h.sendMessage(h.seller,
		putPayload("Tiny Go function — expiry test", "sha256:"+fmt.Sprintf("%064x", 555), "code", 100, 200),
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
	entryID := inv[0].EntryID
	salePrice := eng.ComputePriceForTest(inv[0])
	fee := salePrice / exchange.MatchingFeeRate
	holdAmount := salePrice + fee

	// Seed buyer with enough scrip.
	addScripMintMsg(t, h, h.buyer.PublicKeyHex(), holdAmount+5000)
	if err := cs.Replay(); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	buyerBalanceBefore := cs.Balance(h.buyer.PublicKeyHex())

	// Start the engine to process buy and get a reservation.
	preMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() { _ = eng.Start(ctx) }()

	h.sendMessage(h.buyer,
		buyPayload("Small Go function for expiry test", salePrice+5000),
		[]string{exchange.TagBuy},
		nil,
	)

	matchMsg := waitForMatchMessage(t, h, preMsgs, 2*time.Second)
	cancel()

	// Dispatch buyer-accept to trigger the scrip hold and create the reservation.
	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(allMsgs)

	buyerAcceptMsg := sendBuyerAcceptAndDispatch(t, h, eng, matchMsg.ID, entryID)

	// Get reservation ID from the scrip-buy-hold log message emitted by buyer-accept.
	resID := extractReservationIDFromLog(t, h)
	if resID == "" {
		t.Fatal("expected non-empty reservation_id after buyer-accept")
	}

	allMsgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(allMsgs)

	deliverPayloadBytes, _ := json.Marshal(map[string]any{
		"phase":        "deliver",
		"entry_id":     entryID,
		"content_ref":  "sha256:" + fmt.Sprintf("%064x", 99),
		"content_size": 200,
	})
	deliverMsg := h.sendMessage(h.operator, deliverPayloadBytes,
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrDeliver,
		},
		[]string{buyerAcceptMsg.ID},
	)
	allMsgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(allMsgs)

	// Simulate expiry: remove the entry from inventory BEFORE the dispute arrives.
	eng.State().DeleteInventoryEntryForTest(entryID)

	// Verify the entry is gone from inventory.
	if eng.State().GetInventoryEntry(entryID) != nil {
		t.Fatal("entry should be absent from inventory after DeleteInventoryEntryForTest")
	}

	repBefore := eng.State().SellerReputation(h.seller.PublicKeyHex())

	// Count scrip-dispute-refund messages before dispute attempt.
	preRefundMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{scrip.TagScripDisputeRefund}})

	// Buyer sends small-content-dispute AFTER entry is removed.
	// Write the message to the store but do NOT replay — the entry must stay gone.
	disputeMsg := h.sendMessage(h.buyer,
		smallContentDisputePayloadWithScrip(entryID, resID, h.buyer.PublicKeyHex()),
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrSmallContentDispute,
		},
		[]string{deliverMsg.ID},
	)

	// Do NOT replay here — replaying would re-add the entry from the put/accept messages.
	// Dispatch directly so the engine sees the dispute with the entry absent from inventory.
	rec, err := h.st.GetMessage(disputeMsg.ID)
	if err != nil {
		t.Fatalf("GetMessage dispute: %v", err)
	}

	// Must not panic or return error.
	if err := eng.DispatchForTest(rec); err != nil {
		t.Errorf("expected DispatchForTest to handle missing-entry dispute without error, got: %v", err)
	}

	// STATE LAYER: entry absent → dispute silently dropped.
	// No count increment, no reputation penalty.
	if got := eng.State().SmallContentDisputeCount(entryID); got != 0 {
		t.Errorf("SmallContentDisputeCount for missing-entry dispute = %d, want 0 (state layer drops)", got)
	}
	repAfter := eng.State().SellerReputation(h.seller.PublicKeyHex())
	if repAfter != repBefore {
		t.Errorf("seller reputation changed for missing-entry dispute: before=%d after=%d, want no change (state layer drops)",
			repBefore, repAfter)
	}

	// ENGINE LAYER: entry absent → entryForDeliver returns nil → isSmall check skipped →
	// scrip refund proceeds (benefit of doubt to buyer).
	// Buyer balance must be restored to pre-buy level.
	buyerBalanceAfter := cs.Balance(h.buyer.PublicKeyHex())
	if buyerBalanceAfter != buyerBalanceBefore {
		t.Errorf("buyer balance after missing-entry dispute: got %d, want %d (engine refunds when entry absent)",
			buyerBalanceAfter, buyerBalanceBefore)
	}
	// scrip-dispute-refund must be emitted (engine issued the refund).
	postRefundMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{scrip.TagScripDisputeRefund}})
	if len(postRefundMsgs) <= len(preRefundMsgs) {
		t.Errorf("expected scrip-dispute-refund message for missing-entry dispute (engine refunds when entry absent)")
	}
}
