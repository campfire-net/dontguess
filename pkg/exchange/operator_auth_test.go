package exchange_test

// Tests for operator identity enforcement on operator-only operations (rudi-2f9).
//
// Only the exchange operator should be able to emit put-accept, put-reject,
// match, and deliver messages. A campfire participant who forges one of these
// messages with the wrong sender key must have no effect on state.

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/3dl-dev/dontguess/pkg/exchange"
)

// TestOperatorAuth_ForgePutAcceptIgnored verifies that a put-accept sent by a
// non-operator sender does not move an entry from pendingPuts to inventory.
func TestOperatorAuth_ForgePutAcceptIgnored(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	// Seller submits a put.
	putMsg := h.sendMessage(h.seller,
		putPayload("Go HTTP handler generator", "sha256:"+fmt.Sprintf("%064x", 101), "code", 10000, 20000),
		[]string{exchange.TagPut, "exchange:content-type:code", "exchange:domain:go"},
		nil,
	)

	// Replay so the put lands in state.
	msgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	// Confirm the entry is in pendingPuts.
	pending := eng.State().PendingPuts()
	if len(pending) == 0 {
		t.Fatal("expected put to be in pendingPuts before accept")
	}

	// Attacker (the buyer identity) forges a put-accept.
	acceptPayload, _ := json.Marshal(map[string]any{
		"phase":      exchange.SettlePhaseStrPutAccept,
		"entry_id":   putMsg.ID,
		"price":      9000,
		"expires_at": time.Now().Add(72 * time.Hour).UTC().Format(time.RFC3339),
	})
	h.sendMessage(h.buyer, acceptPayload,
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrPutAccept,
			exchange.TagVerdictPrefix + "accepted",
		},
		[]string{putMsg.ID},
	)

	// Replay all messages including the forged accept.
	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	// Inventory must still be empty — forged accept was rejected.
	inv := eng.State().Inventory()
	if len(inv) != 0 {
		t.Errorf("forged put-accept accepted into inventory: got %d entries, want 0", len(inv))
	}

	// Entry must still be in pendingPuts.
	pending = eng.State().PendingPuts()
	if len(pending) == 0 {
		t.Error("entry was removed from pendingPuts by forged put-accept")
	}
}

// TestOperatorAuth_ForgePutRejectIgnored verifies that a put-reject sent by a
// non-operator sender does not remove an entry from pendingPuts.
func TestOperatorAuth_ForgePutRejectIgnored(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	// Seller submits a put.
	putMsg := h.sendMessage(h.seller,
		putPayload("TypeScript linting config", "sha256:"+fmt.Sprintf("%064x", 102), "code", 5000, 10000),
		[]string{exchange.TagPut, "exchange:content-type:code"},
		nil,
	)

	msgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	if len(eng.State().PendingPuts()) == 0 {
		t.Fatal("expected put to be in pendingPuts")
	}

	// Attacker forges a put-reject.
	rejectPayload, _ := json.Marshal(map[string]any{
		"phase":    exchange.SettlePhaseStrPutReject,
		"entry_id": putMsg.ID,
		"reason":   "low quality",
	})
	h.sendMessage(h.buyer, rejectPayload,
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrPutReject,
		},
		[]string{putMsg.ID},
	)

	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	// Entry must still be in pendingPuts — forged reject was ignored.
	pending := eng.State().PendingPuts()
	if len(pending) == 0 {
		t.Error("forged put-reject removed entry from pendingPuts")
	}
}

// TestOperatorAuth_ForgeMatchIgnored verifies that a match sent by a non-operator
// sender does not mark an order as matched.
func TestOperatorAuth_ForgeMatchIgnored(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	// Seed one inventory entry via the legitimate operator path.
	putMsg := h.sendMessage(h.seller,
		putPayload("Rust async TCP server", "sha256:"+fmt.Sprintf("%064x", 103), "code", 8000, 16000),
		[]string{exchange.TagPut, "exchange:content-type:code", "exchange:domain:rust"},
		nil,
	)
	msgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))
	if err := eng.AutoAcceptPut(putMsg.ID, 5600, time.Now().Add(48*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut: %v", err)
	}

	inv := eng.State().Inventory()
	if len(inv) == 0 {
		t.Fatal("expected entry in inventory after legitimate put-accept")
	}
	entryID := inv[0].EntryID

	// Buyer submits a buy order.
	buyMsg := h.sendMessage(h.buyer,
		buyPayload("async TCP server in Rust", 20000),
		[]string{exchange.TagBuy},
		nil,
	)
	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	// Confirm order is active (not yet matched).
	orders := eng.State().ActiveOrders()
	orderFound := false
	for _, o := range orders {
		if o.OrderID == buyMsg.ID {
			orderFound = true
		}
	}
	if !orderFound {
		t.Fatal("buy order not found in active orders")
	}

	// Attacker (seller) forges a match message for the buy.
	forgedMatchPayload, _ := json.Marshal(map[string]any{
		"results": []map[string]any{
			{"entry_id": entryID, "score": 0.99},
		},
	})
	h.sendMessage(h.seller, forgedMatchPayload,
		[]string{exchange.TagMatch},
		[]string{buyMsg.ID},
	)

	allMsgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	// Order must still be active — forged match was rejected.
	orders = eng.State().ActiveOrders()
	orderStillActive := false
	for _, o := range orders {
		if o.OrderID == buyMsg.ID {
			orderStillActive = true
		}
	}
	if !orderStillActive {
		t.Error("forged match removed buy order from active orders")
	}
}

// TestOperatorAuth_ForgeDeliverIgnored verifies that a deliver sent by a
// non-operator sender does not mark a match as delivered.
func TestOperatorAuth_ForgeDeliverIgnored(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	// Seed inventory.
	putMsg := h.sendMessage(h.seller,
		putPayload("Python data pipeline", "sha256:"+fmt.Sprintf("%064x", 104), "code", 12000, 24000),
		[]string{exchange.TagPut, "exchange:content-type:code", "exchange:domain:python"},
		nil,
	)
	msgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))
	if err := eng.AutoAcceptPut(putMsg.ID, 8400, time.Now().Add(48*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut: %v", err)
	}

	// Buyer sends buy; operator sends a legitimate match.
	buyMsg := h.sendMessage(h.buyer,
		buyPayload("data pipeline in Python", 20000),
		[]string{exchange.TagBuy},
		nil,
	)
	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	// Operator emits a legitimate match.
	matchPayload, _ := json.Marshal(map[string]any{
		"results": []map[string]any{
			{"entry_id": eng.State().Inventory()[0].EntryID, "score": 0.95},
		},
	})
	matchMsg := h.sendMessage(h.operator, matchPayload,
		[]string{exchange.TagMatch},
		[]string{buyMsg.ID},
	)

	// Buyer accepts the match.
	buyerAcceptPayload, _ := json.Marshal(map[string]any{
		"phase":    exchange.SettlePhaseStrBuyerAccept,
		"entry_id": eng.State().Inventory()[0].EntryID,
	})
	buyerAcceptMsg := h.sendMessage(h.buyer, buyerAcceptPayload,
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrBuyerAccept,
		},
		[]string{matchMsg.ID},
	)

	// Verify match is not yet marked as delivered.
	allMsgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))
	if eng.State().IsMatchDelivered(matchMsg.ID) {
		t.Fatal("match incorrectly marked as delivered before any deliver message")
	}

	// Attacker (seller) forges a deliver message.
	forgedDeliverPayload, _ := json.Marshal(map[string]any{
		"phase": exchange.SettlePhaseStrDeliver,
	})
	h.sendMessage(h.seller, forgedDeliverPayload,
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrDeliver,
		},
		[]string{buyerAcceptMsg.ID},
	)

	allMsgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	// Match must NOT be marked as delivered — forged deliver was rejected.
	if eng.State().IsMatchDelivered(matchMsg.ID) {
		t.Error("forged deliver (non-operator sender) marked match as delivered")
	}
}

// TestOperatorAuth_LegitimateOperatorMessagesAccepted verifies that the operator's
// own messages continue to be accepted correctly after the sender check is added.
func TestOperatorAuth_LegitimateOperatorMessagesAccepted(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	// Seller puts, operator accepts — should still work.
	putMsg := h.sendMessage(h.seller,
		putPayload("Kubernetes YAML generator", "sha256:"+fmt.Sprintf("%064x", 105), "code", 15000, 30000),
		[]string{exchange.TagPut, "exchange:content-type:code", "exchange:domain:k8s"},
		nil,
	)
	msgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	if err := eng.AutoAcceptPut(putMsg.ID, 10500, time.Now().Add(72*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut: %v", err)
	}

	inv := eng.State().Inventory()
	if len(inv) != 1 {
		t.Errorf("expected 1 inventory entry after legitimate operator put-accept, got %d", len(inv))
	}
	if inv[0].PutMsgID != putMsg.ID {
		t.Errorf("entry PutMsgID = %q, want %q", inv[0].PutMsgID, putMsg.ID)
	}
}
