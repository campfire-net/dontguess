package exchange_test

// TestE2E_FullHappyPath exercises the complete exchange flow:
//
//	operator inits → seller puts → put-accept → buyer buys →
//	engine matches → buyer-accept → operator delivers → buyer completes
//
// All messages are real campfire messages on a test campfire (fs transport).
// No mocks — real SQLite store, real Ed25519 keys.
//
// Additional cases covered:
//   - buyer reject: escrow path, order not completed
//   - put reject:   rejected entry never enters inventory
//   - empty match:  budget=1 so no inventory candidate passes price filter

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/campfire-net/campfire/pkg/store"

	"github.com/3dl-dev/dontguess/pkg/exchange"
)

// ----------------------------------------------------------------------------
// Happy path: put → accept → buy → match → buyer-accept → deliver → complete
// ----------------------------------------------------------------------------

func TestE2E_FullHappyPath(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	// --- Step 1: Seller puts cached inference ---
	putMsg := h.sendMessage(h.seller,
		putPayload("Go HTTP handler unit test generator", "sha256:"+fmt.Sprintf("%064x", 100), "code", 12000, 20000),
		[]string{exchange.TagPut, "exchange:content-type:code", "exchange:domain:go"},
		nil,
	)

	// Replay to pick up the put.
	msgs, err := h.st.ListMessages(h.cfID, 0)
	if err != nil {
		t.Fatalf("listing messages: %v", err)
	}
	eng.State().Replay(msgs)

	// Inventory must be empty before acceptance.
	if inv := eng.State().Inventory(); len(inv) != 0 {
		t.Errorf("step 1: expected empty inventory before put-accept, got %d", len(inv))
	}

	// --- Step 2: Operator accepts the put ---
	if err := eng.AutoAcceptPut(putMsg.ID, 8400, time.Now().Add(168*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut: %v", err)
	}

	// Inventory must have one live entry.
	inv := eng.State().Inventory()
	if len(inv) != 1 {
		t.Fatalf("step 2: expected 1 inventory entry after put-accept, got %d", len(inv))
	}
	entry := inv[0]
	if entry.PutMsgID != putMsg.ID {
		t.Errorf("step 2: inventory entry PutMsgID = %q, want %q", entry.PutMsgID, putMsg.ID)
	}
	if entry.PutPrice != 8400 {
		t.Errorf("step 2: inventory entry PutPrice = %d, want 8400", entry.PutPrice)
	}

	// --- Step 3: Buyer sends a buy ---
	buyMsg := h.sendMessage(h.buyer,
		buyPayload("Generate unit tests for a Go HTTP handler accepting JSON POST with validation", 50000),
		[]string{exchange.TagBuy},
		nil,
	)

	// Apply the buy to the engine state so the order appears in ActiveOrders.
	buyRec, err := h.st.GetMessage(buyMsg.ID)
	if err != nil {
		t.Fatalf("getting buy message: %v", err)
	}
	eng.State().Apply(buyRec)

	orders := eng.State().ActiveOrders()
	foundOrder := false
	for _, o := range orders {
		if o.OrderID == buyMsg.ID {
			foundOrder = true
		}
	}
	if !foundOrder {
		t.Fatalf("step 3: buy order %s not in active orders", buyMsg.ID[:8])
	}

	// --- Step 4: Engine processes the buy and emits a match ---
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
		t.Fatal("step 4: no match message emitted by engine")
	}

	matchMsg := matchMsgs[len(matchMsgs)-1]

	// Match must reference the buy as antecedent.
	if len(matchMsg.Antecedents) == 0 || matchMsg.Antecedents[0] != buyMsg.ID {
		t.Errorf("step 4: match antecedent = %v, want [%s]", matchMsg.Antecedents, buyMsg.ID)
	}
	// Match sender must be the operator.
	if matchMsg.Sender != h.operator.PublicKeyHex() {
		t.Errorf("step 4: match sender = %q, want operator %q", matchMsg.Sender, h.operator.PublicKeyHex())
	}

	var matchPayload struct {
		Results []struct {
			EntryID  string  `json:"entry_id"`
			Price    int64   `json:"price"`
			Confidence float64 `json:"confidence"`
		} `json:"results"`
	}
	if err := json.Unmarshal(matchMsg.Payload, &matchPayload); err != nil {
		t.Fatalf("step 4: parsing match payload: %v", err)
	}
	if len(matchPayload.Results) == 0 {
		t.Fatal("step 4: match has no results")
	}
	if matchPayload.Results[0].EntryID != putMsg.ID {
		t.Errorf("step 4: match result entry_id = %q, want %q", matchPayload.Results[0].EntryID, putMsg.ID)
	}

	// Order must now be marked matched in state.
	// Re-sync state from store before checking.
	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(allMsgs)
	if !eng.State().IsOrderMatched(buyMsg.ID) {
		t.Error("step 4: buy order should be marked matched after engine emitted match")
	}

	// --- Step 5: Buyer accepts the match ---
	buyerAcceptPayload, _ := json.Marshal(map[string]any{
		"phase":    "buyer-accept",
		"entry_id": matchPayload.Results[0].EntryID,
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

	// acceptedOrders state: matchMsg.ID → entryID (not exported directly, but
	// we can verify by checking that deliver will work).
	// The accepted order is recorded; we verify via state.GetInventoryEntry.
	if eng.State().GetInventoryEntry(entry.EntryID) == nil {
		t.Error("step 5: inventory entry disappeared after buyer-accept")
	}

	// --- Step 6: Operator delivers content ---
	deliverPayload, _ := json.Marshal(map[string]any{
		"phase":        "deliver",
		"entry_id":     matchPayload.Results[0].EntryID,
		"content_ref":  "sha256:" + fmt.Sprintf("%064x", 100),
		"content_size": 20000,
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

	// --- Step 7: Buyer completes the transaction ---
	salePrice := matchPayload.Results[0].Price
	completePayload, _ := json.Marshal(map[string]any{
		"phase":                  "complete",
		"entry_id":               matchPayload.Results[0].EntryID,
		"price":                  salePrice,
		"content_hash_verified":  true,
	})
	h.sendMessage(h.buyer, completePayload,
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrComplete,
			exchange.TagVerdictPrefix + "accepted",
		},
		[]string{deliverMsg.ID},
	)

	allMsgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(allMsgs)

	// After complete, seller reputation must be > default.
	rep := eng.State().SellerReputation(h.seller.PublicKeyHex())
	if rep <= exchange.DefaultReputation {
		t.Errorf("step 7: seller reputation = %d after successful sale, want > %d", rep, exchange.DefaultReputation)
	}

	// Price history must have one record.
	history := eng.State().PriceHistory()
	if len(history) != 1 {
		t.Errorf("step 7: price history len = %d, want 1", len(history))
	}
	if history[0].EntryID != matchPayload.Results[0].EntryID {
		t.Errorf("step 7: price history entry_id = %q, want %q", history[0].EntryID, matchPayload.Results[0].EntryID)
	}
}

// ----------------------------------------------------------------------------
// Buyer reject: buyer rejects the match; no completion, reputation unchanged
// ----------------------------------------------------------------------------

func TestE2E_BuyerReject(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	// Seed inventory.
	putMsg := h.sendMessage(h.seller,
		putPayload("Python async scraper", "sha256:"+fmt.Sprintf("%064x", 200), "code", 8000, 15000),
		[]string{exchange.TagPut, "exchange:content-type:code", "exchange:domain:python"},
		nil,
	)
	msgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(msgs)
	if err := eng.AutoAcceptPut(putMsg.ID, 5600, time.Now().Add(72*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut: %v", err)
	}

	// Buyer sends buy.
	buyMsg := h.sendMessage(h.buyer,
		buyPayload("Async HTTP scraper in Python using aiohttp", 20000),
		[]string{exchange.TagBuy},
		nil,
	)

	// Run engine to emit match.
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
	cancel()

	if len(matchMsgs) <= len(preMsgs) {
		t.Fatal("no match emitted")
	}
	matchMsg := matchMsgs[len(matchMsgs)-1]

	// Parse entry_id from match.
	var mp struct {
		Results []struct{ EntryID string `json:"entry_id"` } `json:"results"`
	}
	if err := json.Unmarshal(matchMsg.Payload, &mp); err != nil || len(mp.Results) == 0 {
		t.Fatalf("parsing match payload: %v", err)
	}

	// Buyer rejects.
	rejectPayload, _ := json.Marshal(map[string]any{
		"phase":    "buyer-reject",
		"entry_id": mp.Results[0].EntryID,
		"accepted": false,
		"reason":   "does not meet freshness requirement",
	})
	h.sendMessage(h.buyer, rejectPayload,
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrBuyerReject,
			exchange.TagVerdictPrefix + "rejected",
		},
		[]string{matchMsg.ID},
	)

	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(allMsgs)

	// No price history: transaction was not completed.
	history := eng.State().PriceHistory()
	if len(history) != 0 {
		t.Errorf("buyer-reject: expected 0 price records, got %d", len(history))
	}

	// Seller reputation must remain at default (no successful sale).
	rep := eng.State().SellerReputation(h.seller.PublicKeyHex())
	if rep != exchange.DefaultReputation {
		t.Errorf("buyer-reject: seller reputation = %d, want %d (default)", rep, exchange.DefaultReputation)
	}

	// Buy order must be marked matched (the match was sent, order was consumed).
	if !eng.State().IsOrderMatched(buyMsg.ID) {
		t.Error("buyer-reject: buy order should be marked matched after match was sent")
	}

	// Inventory entry must still be live (rejection doesn't remove it).
	if eng.State().GetInventoryEntry(mp.Results[0].EntryID) == nil {
		t.Error("buyer-reject: inventory entry should remain after rejection")
	}
}

// ----------------------------------------------------------------------------
// Put reject: rejected put never enters inventory
// ----------------------------------------------------------------------------

func TestE2E_PutReject(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	// Seller puts low-quality content.
	putMsg := h.sendMessage(h.seller,
		putPayload("Placeholder answer", "sha256:"+fmt.Sprintf("%064x", 300), "other", 100, 64),
		[]string{exchange.TagPut, "exchange:content-type:other"},
		nil,
	)

	msgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(msgs)

	// Operator rejects the put.
	rejectPayload, _ := json.Marshal(map[string]any{
		"phase":    "put-reject",
		"entry_id": putMsg.ID,
		"reason":   "content does not meet minimum quality bar",
	})
	h.sendMessage(h.operator, rejectPayload,
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrPutReject,
			exchange.TagVerdictPrefix + "rejected",
		},
		[]string{putMsg.ID},
	)

	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(allMsgs)

	// Inventory must be empty: rejected put never enters inventory.
	if inv := eng.State().Inventory(); len(inv) != 0 {
		t.Errorf("put-reject: expected empty inventory, got %d entries", len(inv))
	}

	// No price history.
	if history := eng.State().PriceHistory(); len(history) != 0 {
		t.Errorf("put-reject: expected 0 price records, got %d", len(history))
	}
}

// ----------------------------------------------------------------------------
// Empty match: no inventory candidates satisfy buyer's budget
// ----------------------------------------------------------------------------

func TestE2E_EmptyMatch(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	// Seed one inventory entry with a high put price → high ask price.
	putMsg := h.sendMessage(h.seller,
		putPayload("Expensive inference result", "sha256:"+fmt.Sprintf("%064x", 400), "analysis", 50000, 80000),
		[]string{exchange.TagPut, "exchange:content-type:analysis", "exchange:domain:finance"},
		nil,
	)
	msgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(msgs)
	// Accept at 35000 scrip → ask price = 35000 * 120/100 = 42000.
	if err := eng.AutoAcceptPut(putMsg.ID, 35000, time.Now().Add(48*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut: %v", err)
	}

	// Buyer sends a buy with budget=1 — below any possible ask price.
	h.sendMessage(h.buyer,
		buyPayload("Finance analysis", 1),
		[]string{exchange.TagBuy},
		nil,
	)

	// Run engine.
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
	cancel()

	if len(matchMsgs) <= len(preMsgs) {
		t.Fatal("empty-match: engine must still emit a match message (with empty results)")
	}

	matchMsg := matchMsgs[len(matchMsgs)-1]
	var mp struct {
		Results    []json.RawMessage `json:"results"`
		SearchMeta struct {
			TotalCandidates int `json:"total_candidates"`
		} `json:"search_meta"`
	}
	if err := json.Unmarshal(matchMsg.Payload, &mp); err != nil {
		t.Fatalf("empty-match: parsing match payload: %v", err)
	}
	if len(mp.Results) != 0 {
		t.Errorf("empty-match: expected 0 results, got %d", len(mp.Results))
	}
	// total_candidates should be 0 (nothing passed budget filter).
	if mp.SearchMeta.TotalCandidates != 0 {
		t.Errorf("empty-match: total_candidates = %d, want 0", mp.SearchMeta.TotalCandidates)
	}
}
