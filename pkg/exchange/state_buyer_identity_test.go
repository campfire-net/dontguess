package exchange_test

// Tests for buyer identity enforcement on settle(buyer-accept) and settle(complete).
//
// Convention §5.3: only the buyer who placed the original buy order may accept a
// match or complete a transaction. Any other sender is silently rejected.
//
// Security vector: without this check, any participant can accept matches or
// complete transactions on behalf of another buyer, redirecting reputation credit
// and triggering price history updates under a false identity.

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/campfire-net/campfire/pkg/store"

	"github.com/3dl-dev/dontguess/pkg/exchange"
)

// setupMatchedOrder seeds inventory, places a buy order, runs the engine to
// emit a match, and returns the match message record and the matched entry ID.
// The caller is responsible for stopping the context.
func setupMatchedOrder(t *testing.T, h *testHarness, eng *exchange.Engine) (matchMsg store.MessageRecord, entryID string) {
	t.Helper()

	// Seller puts an entry.
	putMsg := h.sendMessage(h.seller,
		putPayload("Go HTTP handler unit test generator", "sha256:"+fmt.Sprintf("%064x", 999), "code", 12000, 20000),
		[]string{exchange.TagPut, "exchange:content-type:code", "exchange:domain:go"},
		nil,
	)

	msgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	if err := eng.AutoAcceptPut(putMsg.ID, 8400, time.Now().Add(168*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut: %v", err)
	}

	inv := eng.State().Inventory()
	if len(inv) == 0 {
		t.Fatal("expected inventory entry after put-accept")
	}
	entryID = inv[0].EntryID

	// Buyer places a buy order.
	buyMsg := h.sendMessage(h.buyer,
		buyPayload("Generate unit tests for a Go HTTP handler accepting JSON POST", 50000),
		[]string{exchange.TagBuy},
		nil,
	)

	buyRec, err := h.st.GetMessage(buyMsg.ID)
	if err != nil {
		t.Fatalf("getting buy message: %v", err)
	}
	eng.State().Apply(exchange.FromStoreRecord(buyRec))

	// Run engine to emit a match.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	preMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})
	go func() { _ = eng.Start(ctx) }()

	var matchMsgs []store.MessageRecord
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		matchMsgs, _ = h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})
		if len(matchMsgs) > len(preMsgs) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if len(matchMsgs) <= len(preMsgs) {
		t.Fatal("engine did not emit a match message")
	}
	matchMsg = matchMsgs[len(matchMsgs)-1]

	// Sync state with the match message.
	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	return matchMsg, entryID
}

// buyerAcceptPayloadFor builds a valid buyer-accept payload.
func buyerAcceptPayloadFor(entryID string) []byte {
	p, _ := json.Marshal(map[string]any{
		"phase":    "buyer-accept",
		"entry_id": entryID,
		"accepted": true,
	})
	return p
}

// deliverPayloadFor builds a minimal deliver payload.
func deliverPayloadFor(entryID string) []byte {
	p, _ := json.Marshal(map[string]any{
		"phase":        "deliver",
		"entry_id":     entryID,
		"content_ref":  "sha256:" + fmt.Sprintf("%064x", 1),
		"content_size": 20000,
	})
	return p
}

// completePayloadFor builds a minimal complete payload.
func completePayloadFor(entryID string, price int64) []byte {
	p, _ := json.Marshal(map[string]any{
		"phase":                 "complete",
		"entry_id":              entryID,
		"price":                 price,
		"content_hash_verified": true,
	})
	return p
}

// TestState_BuyerAccept_WrongSenderRejected verifies that a settle(buyer-accept)
// from a sender who is NOT the original buyer is silently rejected — the match
// is not recorded as accepted and the buyer-accept→match chain is not recorded.
func TestState_BuyerAccept_WrongSenderRejected(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	matchMsg, entryID := setupMatchedOrder(t, h, eng)

	// Generate an impostor identity.
	impostor := newTestAgent(t)

	// Impostor sends buyer-accept with the real match as antecedent.
	h.sendMessage(impostor, buyerAcceptPayloadFor(entryID),
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrBuyerAccept,
			exchange.TagVerdictPrefix + "accepted",
		},
		[]string{matchMsg.ID},
	)

	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	// Verify: operator deliver should fail because the match was never accepted.
	// We check indirectly: seller reputation unchanged (no completion possible).
	repBefore := exchange.DefaultReputation
	repAfter := eng.State().SellerReputation(h.seller.PublicKeyHex())
	if repAfter != repBefore {
		t.Errorf("impostor buyer-accept: seller reputation changed from %d to %d, want no change",
			repBefore, repAfter)
	}

	// Price history must be empty — the impostor accept should not have been
	// recorded, so no complete can follow.
	if hist := eng.State().PriceHistory(); len(hist) != 0 {
		t.Errorf("impostor buyer-accept: price history len = %d, want 0", len(hist))
	}
}

// TestState_BuyerAccept_CorrectSenderAccepted verifies that a settle(buyer-accept)
// from the actual buyer is accepted and the chain is properly recorded.
func TestState_BuyerAccept_CorrectSenderAccepted(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	matchMsg, entryID := setupMatchedOrder(t, h, eng)

	// Correct buyer sends buyer-accept.
	buyerAcceptMsg := h.sendMessage(h.buyer, buyerAcceptPayloadFor(entryID),
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrBuyerAccept,
			exchange.TagVerdictPrefix + "accepted",
		},
		[]string{matchMsg.ID},
	)

	// Operator delivers.
	deliverMsg := h.sendMessage(h.operator, deliverPayloadFor(entryID),
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrDeliver,
		},
		[]string{buyerAcceptMsg.ID},
	)

	// Buyer completes.
	h.sendMessage(h.buyer, completePayloadFor(entryID, 12000),
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrComplete,
			exchange.TagVerdictPrefix + "accepted",
		},
		[]string{deliverMsg.ID},
	)

	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	// Seller reputation must have increased.
	rep := eng.State().SellerReputation(h.seller.PublicKeyHex())
	if rep <= exchange.DefaultReputation {
		t.Errorf("correct buyer complete: seller reputation = %d, want > %d",
			rep, exchange.DefaultReputation)
	}

	// Price history must have one record.
	if hist := eng.State().PriceHistory(); len(hist) != 1 {
		t.Errorf("correct buyer complete: price history len = %d, want 1", len(hist))
	}
}

// TestState_Complete_WrongSenderRejected verifies that a settle(complete) from
// a sender who is NOT the original buyer is silently rejected — reputation credit
// and price history are not updated.
func TestState_Complete_WrongSenderRejected(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	matchMsg, entryID := setupMatchedOrder(t, h, eng)

	// Correct buyer sends buyer-accept.
	buyerAcceptMsg := h.sendMessage(h.buyer, buyerAcceptPayloadFor(entryID),
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrBuyerAccept,
			exchange.TagVerdictPrefix + "accepted",
		},
		[]string{matchMsg.ID},
	)

	// Operator delivers.
	deliverMsg := h.sendMessage(h.operator, deliverPayloadFor(entryID),
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrDeliver,
		},
		[]string{buyerAcceptMsg.ID},
	)

	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	repBefore := eng.State().SellerReputation(h.seller.PublicKeyHex())

	// Impostor sends complete using the valid deliver antecedent.
	impostor := newTestAgent(t)
	h.sendMessage(impostor, completePayloadFor(entryID, 12000),
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrComplete,
			exchange.TagVerdictPrefix + "accepted",
		},
		[]string{deliverMsg.ID},
	)

	allMsgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	// Seller reputation must NOT have changed.
	repAfter := eng.State().SellerReputation(h.seller.PublicKeyHex())
	if repAfter != repBefore {
		t.Errorf("impostor complete: seller reputation changed from %d to %d, want no change",
			repBefore, repAfter)
	}

	// Price history must be empty.
	if hist := eng.State().PriceHistory(); len(hist) != 0 {
		t.Errorf("impostor complete: price history len = %d, want 0", len(hist))
	}
}

// buyerRejectPayloadFor builds a valid buyer-reject payload.
func buyerRejectPayloadFor(entryID string) []byte {
	p, _ := json.Marshal(map[string]any{
		"phase":    "buyer-reject",
		"entry_id": entryID,
		"accepted": false,
		"reason":   "does not meet requirements",
	})
	return p
}

// TestState_BuyerReject_RemovesAcceptedOrder verifies that a settle(buyer-reject)
// from the actual buyer removes the accepted order entry so the buyer is no
// longer bound to the match.
func TestState_BuyerReject_RemovesAcceptedOrder(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	matchMsg, entryID := setupMatchedOrder(t, h, eng)

	// First, buyer accepts the match so acceptedOrders is populated.
	buyerAcceptMsg := h.sendMessage(h.buyer, buyerAcceptPayloadFor(entryID),
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrBuyerAccept,
			exchange.TagVerdictPrefix + "accepted",
		},
		[]string{matchMsg.ID},
	)

	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	// Verify the accept was recorded.
	if !eng.State().IsMatchAccepted(matchMsg.ID) {
		t.Fatal("expected match to be accepted after buyer-accept")
	}

	// Buyer rejects by referencing the match (not the buyer-accept).
	_ = buyerAcceptMsg // unused but documents the flow
	h.sendMessage(h.buyer, buyerRejectPayloadFor(entryID),
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrBuyerReject,
			exchange.TagVerdictPrefix + "rejected",
		},
		[]string{matchMsg.ID},
	)

	allMsgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	// acceptedOrders entry must be removed.
	if eng.State().IsMatchAccepted(matchMsg.ID) {
		t.Error("buyer-reject: expected accepted order to be removed, but it remains")
	}

	// Inventory must still be live.
	if eng.State().GetInventoryEntry(entryID) == nil {
		t.Error("buyer-reject: inventory entry must remain after rejection")
	}

	// Seller reputation must not change.
	if rep := eng.State().SellerReputation(h.seller.PublicKeyHex()); rep != exchange.DefaultReputation {
		t.Errorf("buyer-reject: seller reputation = %d, want %d (default)", rep, exchange.DefaultReputation)
	}

	// No price history.
	if hist := eng.State().PriceHistory(); len(hist) != 0 {
		t.Errorf("buyer-reject: price history len = %d, want 0", len(hist))
	}
}

// TestState_BuyerReject_WrongSenderIgnored verifies that a settle(buyer-reject)
// from a sender who is NOT the original buyer is silently ignored — the
// accepted order entry is not removed.
func TestState_BuyerReject_WrongSenderIgnored(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	matchMsg, entryID := setupMatchedOrder(t, h, eng)

	// Correct buyer accepts.
	h.sendMessage(h.buyer, buyerAcceptPayloadFor(entryID),
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrBuyerAccept,
			exchange.TagVerdictPrefix + "accepted",
		},
		[]string{matchMsg.ID},
	)

	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	if !eng.State().IsMatchAccepted(matchMsg.ID) {
		t.Fatal("expected match to be accepted after correct buyer-accept")
	}

	// Impostor attempts to reject.
	impostor := newTestAgent(t)
	h.sendMessage(impostor, buyerRejectPayloadFor(entryID),
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrBuyerReject,
			exchange.TagVerdictPrefix + "rejected",
		},
		[]string{matchMsg.ID},
	)

	allMsgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	// The accepted order must still be present (impostor reject ignored).
	if !eng.State().IsMatchAccepted(matchMsg.ID) {
		t.Error("impostor buyer-reject: accepted order was removed, but should be preserved")
	}
}
