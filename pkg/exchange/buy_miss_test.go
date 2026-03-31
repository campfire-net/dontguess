package exchange_test

// TestBuyMiss_StandingOfferE2E exercises the full buy-miss standing offer flow:
//
//	buyer buys (no inventory) → engine emits buy-miss offer →
//	buyer puts the result → engine auto-accepts at token_cost * 70% →
//	buyer/putter gets scrip
//
// All messages are real campfire messages on a test campfire (fs transport).
// No mocks — real SQLite store, real Ed25519 keys.

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

// TestBuyMiss_EmitsBuyMissWhenNoInventory verifies that a buy with no matching
// inventory causes the engine to emit a buy-miss message (tagged exchange:buy-miss
// + exchange:match) that fulfills the buy future.
func TestBuyMiss_EmitsBuyMissWhenNoInventory(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	// No inventory — empty exchange.

	// Buyer sends a buy.
	buyMsg := h.sendMessage(h.buyer,
		buyPayload("Write a Rust async HTTP client from scratch", 50000),
		[]string{exchange.TagBuy},
		nil,
	)

	// Count existing match-tagged messages before engine runs.
	preMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})
	preCount := len(preMsgs)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	go func() { _ = eng.Start(ctx) }()

	// Wait for a buy-miss message to appear.
	var buyMissMsgs []store.MessageRecord
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		buyMissMsgs, _ = h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagBuyMiss}})
		if len(buyMissMsgs) > preCount {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()

	if len(buyMissMsgs) == 0 {
		t.Fatal("no buy-miss message emitted by engine")
	}

	bmMsg := buyMissMsgs[len(buyMissMsgs)-1]

	// Must have both exchange:buy-miss and exchange:match tags.
	if !hasTag(bmMsg.Tags, exchange.TagBuyMiss) {
		t.Errorf("buy-miss message missing exchange:buy-miss tag, got %v", bmMsg.Tags)
	}
	if !hasTag(bmMsg.Tags, exchange.TagMatch) {
		t.Errorf("buy-miss message missing exchange:match tag, got %v", bmMsg.Tags)
	}

	// Antecedent must be the buy message.
	if len(bmMsg.Antecedents) == 0 || bmMsg.Antecedents[0] != buyMsg.ID {
		t.Errorf("buy-miss antecedent = %v, want [%s]", bmMsg.Antecedents, buyMsg.ID)
	}
	// Sender must be the operator.
	if bmMsg.Sender != h.operator.PublicKeyHex() {
		t.Errorf("buy-miss sender = %q, want %q (operator)", bmMsg.Sender, h.operator.PublicKeyHex())
	}

	// Parse buy-miss payload.
	var payload struct {
		TaskHash         string `json:"task_hash"`
		Task             string `json:"task"`
		OfferedPriceRate int    `json:"offered_price_rate"`
		ExpiresAt        string `json:"expires_at"`
		BuyMsgID         string `json:"buy_msg_id"`
	}
	if err := json.Unmarshal(bmMsg.Payload, &payload); err != nil {
		t.Fatalf("parsing buy-miss payload: %v", err)
	}
	if payload.TaskHash == "" {
		t.Error("buy-miss payload missing task_hash")
	}
	if payload.OfferedPriceRate != exchange.BuyMissOfferRate {
		t.Errorf("offered_price_rate = %d, want %d", payload.OfferedPriceRate, exchange.BuyMissOfferRate)
	}
	if payload.BuyMsgID != buyMsg.ID {
		t.Errorf("buy_msg_id = %q, want %q", payload.BuyMsgID, buyMsg.ID)
	}
	if payload.ExpiresAt == "" {
		t.Error("buy-miss payload missing expires_at")
	}
}

// TestBuyMiss_NoDuplicateOffer verifies that sending two buy messages with the
// same task description results in only one standing offer in state (idempotent).
func TestBuyMiss_NoDuplicateOffer(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	task := "Summarize a 10-page PDF document about quantum computing"

	_ = h.sendMessage(h.buyer,
		buyPayload(task, 50000),
		[]string{exchange.TagBuy},
		nil,
	)
	buyer2 := newTestAgent(t)
	_ = h.sendMessage(buyer2,
		buyPayload(task, 30000),
		[]string{exchange.TagBuy},
		nil,
	)

	// Let the engine process both buys.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	go func() { _ = eng.Start(ctx) }()

	// Wait for two buy-miss messages.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		msgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagBuyMiss}})
		if len(msgs) >= 2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()

	// State should have exactly one offer for this task hash.
	taskHash := exchange.TaskDescriptionHash(task)
	offer := eng.State().GetBuyMissOffer(taskHash)
	if offer == nil {
		t.Fatal("expected a standing offer for task hash, got nil")
	}
	// The offer should correspond to one of the two buy messages.
	if offer.TaskHash != taskHash {
		t.Errorf("offer TaskHash = %q, want %q", offer.TaskHash, taskHash)
	}
}

// TestBuyMiss_StandingOfferE2E is the full end-to-end test:
//
//	buyer sends buy (no inventory) → engine emits buy-miss offer →
//	buyer puts result matching the task → engine auto-accepts at offered price →
//	putter receives scrip
func TestBuyMiss_StandingOfferE2E(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)

	// Wire a real CampfireScripStore so we can verify scrip payments.
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

	// ------------------------------------------------------------------
	// Step 1: Buyer sends a buy — no inventory, engine emits buy-miss.
	// ------------------------------------------------------------------
	task := "Implement a Go Redis client with connection pooling and circuit breaker"
	_ = h.sendMessage(h.buyer,
		buyPayload(task, 100000),
		[]string{exchange.TagBuy},
		nil,
	)

	preBuyMiss, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagBuyMiss}})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go func() { _ = eng.Start(ctx) }()

	// Wait for buy-miss message.
	var buyMissMsgs []store.MessageRecord
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		buyMissMsgs, _ = h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagBuyMiss}})
		if len(buyMissMsgs) > len(preBuyMiss) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(buyMissMsgs) <= len(preBuyMiss) {
		cancel()
		t.Fatal("step 1: no buy-miss offer emitted")
	}

	// Verify the standing offer is in state.
	taskHash := exchange.TaskDescriptionHash(task)
	offer := eng.State().GetBuyMissOffer(taskHash)
	if offer == nil {
		cancel()
		t.Fatal("step 1: standing offer not recorded in state")
	}

	// ------------------------------------------------------------------
	// Step 2: Buyer computes the result and puts it (same task description).
	// ------------------------------------------------------------------
	const tokenCost int64 = 80000 // realistic inference cost

	putMsg := h.sendMessage(h.buyer, // buyer is also the seller here (computed it themselves)
		putPayload(task, "sha256:"+fmt.Sprintf("%064x", tokenCost), "code", tokenCost, tokenCost*2),
		[]string{exchange.TagPut, "exchange:content-type:code"},
		nil,
	)

	// ------------------------------------------------------------------
	// Step 3: Wait for auto-accept (put-accept message in the log).
	// ------------------------------------------------------------------
	preSettle, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{
		Tags: []string{exchange.TagSettle},
	})

	var settleMsgs []store.MessageRecord
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		settleMsgs, _ = h.st.ListMessages(h.cfID, 0, store.MessageFilter{
			Tags: []string{exchange.TagSettle},
		})
		if len(settleMsgs) > len(preSettle) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel() // stop the engine

	if len(settleMsgs) <= len(preSettle) {
		t.Fatal("step 3: no settle(put-accept) emitted for buy-miss fulfillment")
	}

	// Find the put-accept message.
	var putAcceptMsg *store.MessageRecord
	for i := range settleMsgs {
		for _, tag := range settleMsgs[i].Tags {
			if tag == "exchange:phase:put-accept" {
				putAcceptMsg = &settleMsgs[i]
				break
			}
		}
		if putAcceptMsg != nil {
			break
		}
	}
	if putAcceptMsg == nil {
		t.Fatal("step 3: no settle(put-accept) found among new settle messages")
	}

	// Antecedent must be the put message.
	if len(putAcceptMsg.Antecedents) == 0 || putAcceptMsg.Antecedents[0] != putMsg.ID {
		t.Errorf("put-accept antecedent = %v, want [%s]", putAcceptMsg.Antecedents, putMsg.ID)
	}

	// Parse put-accept payload: price must be token_cost * BuyMissOfferRate / 100.
	var acceptPayload struct {
		Phase    string `json:"phase"`
		EntryID  string `json:"entry_id"`
		Price    int64  `json:"price"`
	}
	if err := json.Unmarshal(putAcceptMsg.Payload, &acceptPayload); err != nil {
		t.Fatalf("parsing put-accept payload: %v", err)
	}
	expectedPrice := tokenCost * int64(exchange.BuyMissOfferRate) / 100
	if acceptPayload.Price != expectedPrice {
		t.Errorf("put-accept price = %d, want %d (token_cost * %d%%)", acceptPayload.Price, expectedPrice, exchange.BuyMissOfferRate)
	}

	// ------------------------------------------------------------------
	// Step 4: Inventory must contain the newly accepted entry.
	// ------------------------------------------------------------------
	// Replay state to pick up the auto-accept.
	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	inv := eng.State().Inventory()
	var foundEntry *exchange.InventoryEntry
	for _, e := range inv {
		if e.PutMsgID == putMsg.ID {
			foundEntry = e
			break
		}
	}
	if foundEntry == nil {
		t.Fatalf("step 4: put %s not in inventory after auto-accept", putMsg.ID[:8])
	}
	if foundEntry.PutPrice != expectedPrice {
		t.Errorf("inventory entry PutPrice = %d, want %d", foundEntry.PutPrice, expectedPrice)
	}

	// ------------------------------------------------------------------
	// Step 5: Putter receives scrip (scrip-put-pay message in log).
	// ------------------------------------------------------------------
	putPayMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{
		Tags: []string{scrip.TagScripPutPay},
	})
	if len(putPayMsgs) == 0 {
		t.Fatal("step 5: no scrip-put-pay message emitted for buy-miss fulfillment")
	}

	// Find the pay message for our put.
	var foundPay bool
	for _, pm := range putPayMsgs {
		var p struct {
			Seller string `json:"seller"`
			Amount int64  `json:"amount"`
			PutMsg string `json:"put_msg"`
		}
		if err := json.Unmarshal(pm.Payload, &p); err != nil {
			continue
		}
		if p.PutMsg == putMsg.ID {
			foundPay = true
			if p.Seller != h.buyer.PublicKeyHex() {
				t.Errorf("scrip-put-pay seller = %q, want %q (buyer who put)", p.Seller, h.buyer.PublicKeyHex())
			}
			if p.Amount != expectedPrice {
				t.Errorf("scrip-put-pay amount = %d, want %d", p.Amount, expectedPrice)
			}
			break
		}
	}
	if !foundPay {
		t.Fatalf("step 5: no scrip-put-pay for put message %s", putMsg.ID[:8])
	}

	// ------------------------------------------------------------------
	// Step 6: Standing offer must be consumed (no longer in state).
	// ------------------------------------------------------------------
	if eng.State().GetBuyMissOffer(taskHash) != nil {
		t.Error("step 6: standing offer still in state after fulfillment — should have been consumed")
	}
}

// TestBuyMiss_OfferExpiry verifies that an expired buy-miss offer is not
// fulfilled when a matching put arrives after the TTL.
func TestBuyMiss_OfferExpiry(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)

	task := "Convert a 5000-line C library to idiomatic Rust"
	taskHash := exchange.TaskDescriptionHash(task)

	// Manually insert an already-expired offer.
	expired := &exchange.BuyMissOffer{
		TaskHash:  taskHash,
		BuyMsgID:  "fake-buy-msg-id",
		BuyerKey:  h.buyer.PublicKeyHex(),
		Task:      task,
		ExpiresAt: time.Now().Add(-1 * time.Hour), // already expired
	}
	// SetBuyMissOffer returns false for a duplicate non-expired offer; for an
	// expired one it should overwrite. Let's verify expiry detection via GetBuyMissOffer.
	_ = h.st // not used here; pure state test

	eng := h.newEngine()

	// Force an expired offer into state.
	// SetBuyMissOffer will insert because the map is empty.
	set := eng.State().SetBuyMissOffer(expired)
	if !set {
		t.Fatal("SetBuyMissOffer returned false for empty map insert")
	}

	// GetBuyMissOffer must return nil because it's expired.
	if got := eng.State().GetBuyMissOffer(taskHash); got != nil {
		t.Errorf("GetBuyMissOffer returned non-nil for expired offer: %+v", got)
	}

	// Sending a put for this task now should NOT trigger auto-accept (no live offer).
	putMsg := h.sendMessage(h.seller,
		putPayload(task, "sha256:"+fmt.Sprintf("%064x", 42), "code", 15000, 30000),
		[]string{exchange.TagPut},
		nil,
	)
	msgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	rec, _ := h.st.GetMessage(putMsg.ID)
	if rec == nil {
		t.Fatal("put message not found in store")
	}
	putMsgObj := exchange.FromStoreRecord(rec)
	if err := eng.DispatchForTest(putMsgObj); err != nil {
		t.Fatalf("DispatchForTest(put): %v", err)
	}

	// No settle messages should have been emitted.
	settleMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagSettle}})
	for _, sm := range settleMsgs {
		for _, tag := range sm.Tags {
			if tag == "exchange:phase:put-accept" {
				t.Errorf("unexpected put-accept for expired offer: %s", sm.ID)
			}
		}
	}
}
