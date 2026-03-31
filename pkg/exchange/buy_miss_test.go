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

// TestBuyMiss_WrongSenderIgnored verifies that only the buyer who received the
// buy-miss offer may fulfill it by submitting a put. A different agent
// submitting a put with a matching description must NOT trigger auto-accept.
// When the original buyer then puts the same description, auto-accept fires.
func TestBuyMiss_WrongSenderIgnored(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	task := "Build a distributed key-value store in Go with Raft consensus"
	taskHash := exchange.TaskDescriptionHash(task)

	eng := h.newEngine()

	// Replay the initial exchange bootstrap messages.
	msgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	// Inject a standing offer associating the task with h.buyer as the buyer.
	offer := &exchange.BuyMissOffer{
		TaskHash:  taskHash,
		BuyMsgID:  "fake-buy-msg-id",
		BuyerKey:  h.buyer.PublicKeyHex(),
		Task:      task,
		ExpiresAt: time.Now().Add(exchange.BuyMissOfferTTL),
	}
	eng.State().SetBuyMissOffer(offer)

	// Step 2: An impostor (h.seller) submits a put with the same task description.
	// Use Apply (not Replay) to add the put to state so the injected offer is preserved.
	impostorPut := h.sendMessage(h.seller,
		putPayload(task, "sha256:"+fmt.Sprintf("%064x", 1234), "code", 40000, 80000),
		[]string{exchange.TagPut},
		nil,
	)
	impostorRec, _ := h.st.GetMessage(impostorPut.ID)
	if impostorRec == nil {
		t.Fatal("impostor put not found in store")
	}
	impostorMsg := exchange.FromStoreRecord(impostorRec)
	eng.State().Apply(impostorMsg)

	// Dispatch the impostor put — should be silently ignored (wrong sender).
	if err := eng.DispatchForTest(impostorMsg); err != nil {
		t.Fatalf("DispatchForTest(impostor put): %v", err)
	}

	// No settle should have been emitted.
	settleMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagSettle}})
	for _, sm := range settleMsgs {
		if hasTag(sm.Tags, "exchange:phase:put-accept") {
			t.Errorf("unexpected put-accept for impostor put: msg %s", sm.ID)
		}
	}

	// Standing offer must still be present (not consumed).
	if eng.State().GetBuyMissOffer(taskHash) == nil {
		t.Error("standing offer was consumed by impostor — should still be live")
	}

	// Step 3: The actual buyer now puts the same description — auto-accept must fire.
	// Use Apply to add only this new message without re-running Replay (which would
	// wipe the standing offer since it was injected directly, not via campfire messages).
	preSettle, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagSettle}})

	buyerPut := h.sendMessage(h.buyer,
		putPayload(task, "sha256:"+fmt.Sprintf("%064x", 5678), "code", 40000, 80000),
		[]string{exchange.TagPut},
		nil,
	)
	buyerRec, _ := h.st.GetMessage(buyerPut.ID)
	if buyerRec == nil {
		t.Fatal("buyer put not found in store")
	}
	buyerMsg := exchange.FromStoreRecord(buyerRec)
	eng.State().Apply(buyerMsg)

	if err := eng.DispatchForTest(buyerMsg); err != nil {
		t.Fatalf("DispatchForTest(buyer put): %v", err)
	}

	// A put-accept settle must have been emitted.
	postSettle, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagSettle}})
	var foundAccept bool
	for _, sm := range postSettle {
		// Only consider messages that appeared after the impostor phase.
		alreadySeen := false
		for _, pre := range preSettle {
			if pre.ID == sm.ID {
				alreadySeen = true
				break
			}
		}
		if !alreadySeen && hasTag(sm.Tags, "exchange:phase:put-accept") {
			foundAccept = true
			break
		}
	}
	if !foundAccept {
		t.Error("buyer put did not trigger auto-accept — expected put-accept settle message")
	}

	// Standing offer must now be consumed.
	if eng.State().GetBuyMissOffer(taskHash) != nil {
		t.Error("standing offer still present after buyer fulfillment — should be consumed")
	}
}

// TestBuyMiss_TokenCostCapped verifies that a seller cannot inflate the scrip
// payout by supplying a token_cost above MaxTokenCost. The price paid must be
// capped at MaxTokenCost * BuyMissOfferRate / 100.
func TestBuyMiss_TokenCostCapped(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	const maxTokenCost int64 = 1_000_000

	task := "Generate a high-fidelity physics simulation in Rust"
	taskHash := exchange.TaskDescriptionHash(task)

	eng := exchange.NewEngine(exchange.EngineOptions{
		CampfireID:   h.cfID,
		Store:        h.st,
		ReadClient:   h.newOperatorClient(),
		WriteClient:  h.newOperatorClient(),
		MaxTokenCost: maxTokenCost,
		ReadSkipSync: true,
		Logger: func(format string, args ...any) {
			t.Logf("[engine] "+format, args...)
		},
	})

	// Replay existing state then inject the standing offer. Use Apply (not Replay)
	// for subsequent messages to preserve the injected offer in state.
	msgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	// Inject a standing offer for the buyer.
	offer := &exchange.BuyMissOffer{
		TaskHash:  taskHash,
		BuyMsgID:  "fake-buy-msg-id",
		BuyerKey:  h.buyer.PublicKeyHex(),
		Task:      task,
		ExpiresAt: time.Now().Add(exchange.BuyMissOfferTTL),
	}
	eng.State().SetBuyMissOffer(offer)

	preSettle, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagSettle}})

	// Buyer puts with an inflated token_cost (100× above cap).
	inflatedCost := maxTokenCost * 100
	putMsg := h.sendMessage(h.buyer,
		putPayload(task, "sha256:"+fmt.Sprintf("%064x", inflatedCost), "code", inflatedCost, inflatedCost*2),
		[]string{exchange.TagPut},
		nil,
	)

	// Apply just this new message so pendingPuts is populated without wiping the offer.
	putRec, _ := h.st.GetMessage(putMsg.ID)
	if putRec == nil {
		t.Fatal("put not in store")
	}
	putMsgObj := exchange.FromStoreRecord(putRec)
	eng.State().Apply(putMsgObj)

	if err := eng.DispatchForTest(putMsgObj); err != nil {
		t.Fatalf("DispatchForTest: %v", err)
	}

	// Find the put-accept message.
	postSettle, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagSettle}})
	var putAcceptMsg *store.MessageRecord
	for i := range postSettle {
		alreadySeen := false
		for _, pre := range preSettle {
			if pre.ID == postSettle[i].ID {
				alreadySeen = true
				break
			}
		}
		if !alreadySeen && hasTag(postSettle[i].Tags, "exchange:phase:put-accept") {
			putAcceptMsg = &postSettle[i]
			break
		}
	}
	if putAcceptMsg == nil {
		t.Fatal("no put-accept emitted for buy-miss fulfillment")
	}

	// Parse the price — must be capped at maxTokenCost * BuyMissOfferRate / 100.
	var acceptPayload struct {
		Price int64 `json:"price"`
	}
	if err := json.Unmarshal(putAcceptMsg.Payload, &acceptPayload); err != nil {
		t.Fatalf("parsing put-accept payload: %v", err)
	}
	expectedPrice := maxTokenCost * int64(exchange.BuyMissOfferRate) / 100
	if acceptPayload.Price != expectedPrice {
		t.Errorf("price = %d, want %d (capped at maxTokenCost * %d%%); inflated cost was %d",
			acceptPayload.Price, expectedPrice, exchange.BuyMissOfferRate, inflatedCost)
	}
}

// TestBuyMiss_InvalidHashRejected verifies that a put with a content_hash that
// does not start with "sha256:" is rejected in the buy-miss auto-accept path
// (engine returns an error; no put-accept is emitted).
func TestBuyMiss_InvalidHashRejected(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	task := "Translate a 500-page technical document from French to English"
	taskHash := exchange.TaskDescriptionHash(task)

	eng := h.newEngine()

	// Replay existing state.
	msgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	// Inject a standing offer for the buyer.
	offer := &exchange.BuyMissOffer{
		TaskHash:  taskHash,
		BuyMsgID:  "fake-buy-msg-id",
		BuyerKey:  h.buyer.PublicKeyHex(),
		Task:      task,
		ExpiresAt: time.Now().Add(exchange.BuyMissOfferTTL),
	}
	eng.State().SetBuyMissOffer(offer)

	// Buyer puts with an invalid content_hash (no sha256: prefix).
	// Use Apply (not Replay) to add the put to state without wiping the injected offer.
	invalidHash := "md5:d41d8cd98f00b204e9800998ecf8427e"
	putMsg := h.sendMessage(h.buyer,
		putPayload(task, invalidHash, "code", 20000, 40000),
		[]string{exchange.TagPut},
		nil,
	)

	putRec, _ := h.st.GetMessage(putMsg.ID)
	if putRec == nil {
		t.Fatal("put not in store")
	}
	putMsgObj := exchange.FromStoreRecord(putRec)
	eng.State().Apply(putMsgObj)

	// Dispatch should return an error for the invalid hash.
	err := eng.DispatchForTest(putMsgObj)
	if err == nil {
		t.Error("expected error for invalid content_hash, got nil")
	}

	// No put-accept must have been emitted.
	settleMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagSettle}})
	for _, sm := range settleMsgs {
		if hasTag(sm.Tags, "exchange:phase:put-accept") {
			t.Errorf("unexpected put-accept emitted for invalid content_hash: msg %s", sm.ID)
		}
	}
}

// TestBuyMiss_AutoAcceptEmitsCompressionAssign verifies that when a buy-miss
// standing offer is fulfilled (seller puts a matching result), the engine also
// emits a hot compression assign to the seller — consistent with AutoAcceptPut.
func TestBuyMiss_AutoAcceptEmitsCompressionAssign(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	task := "Implement a Go circuit-breaker with exponential back-off"
	taskHash := exchange.TaskDescriptionHash(task)
	const tokenCost int64 = 40000

	eng := h.newEngine()

	// Replay current state (empty).
	msgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	// Inject a standing buy-miss offer for the seller (seller will do the work).
	offer := &exchange.BuyMissOffer{
		TaskHash:  taskHash,
		BuyMsgID:  "fake-buy-msg-id",
		BuyerKey:  h.seller.PublicKeyHex(), // seller fulfills their own offer
		Task:      task,
		ExpiresAt: time.Now().Add(exchange.BuyMissOfferTTL),
	}
	eng.State().SetBuyMissOffer(offer)

	// Seller puts the result.
	contentHash := "sha256:" + fmt.Sprintf("%064x", tokenCost)
	putMsg := h.sendMessage(h.seller,
		putPayload(task, contentHash, "code", tokenCost, tokenCost*2),
		[]string{exchange.TagPut, "exchange:content-type:code"},
		nil,
	)
	putRec, err := h.st.GetMessage(putMsg.ID)
	if err != nil || putRec == nil {
		t.Fatal("put not in store")
	}
	putMsgObj := exchange.FromStoreRecord(putRec)
	eng.State().Apply(putMsgObj)

	if err := eng.DispatchForTest(putMsgObj); err != nil {
		t.Fatalf("DispatchForTest put: %v", err)
	}

	// Verify a compression assign was emitted.
	assignMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagAssign}})
	var compressionAssign *store.MessageRecord
	for i := range assignMsgs {
		var p struct {
			TaskType string `json:"task_type"`
		}
		if json.Unmarshal(assignMsgs[i].Payload, &p) == nil && p.TaskType == "compress" {
			compressionAssign = &assignMsgs[i]
			break
		}
	}
	if compressionAssign == nil {
		t.Fatal("buy-miss auto-accept should emit hot compression assign, got none")
	}

	// Verify the assign targets the seller exclusively and carries a 50% bounty.
	var ap struct {
		TaskType        string `json:"task_type"`
		EntryID         string `json:"entry_id"`
		Reward          int64  `json:"reward"`
		ExclusiveSender string `json:"exclusive_sender"`
	}
	if err := json.Unmarshal(compressionAssign.Payload, &ap); err != nil {
		t.Fatalf("unmarshal compression assign payload: %v", err)
	}
	if ap.ExclusiveSender != h.seller.PublicKeyHex() {
		t.Errorf("compression assign exclusive_sender = %q, want seller %q", ap.ExclusiveSender, h.seller.PublicKeyHex())
	}
	wantBounty := tokenCost / 2
	if ap.Reward != wantBounty {
		t.Errorf("compression assign reward = %d, want %d (50%% of token_cost)", ap.Reward, wantBounty)
	}
}
