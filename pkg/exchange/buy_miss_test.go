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

// TestBuyMiss_NoContentRejected verifies that a put with no content field is
// dropped at the state layer (not added to pendingPuts), causing dispatch to
// return an error and emit no put-accept.
//
// Previously this test verified an invalid content_hash was rejected at dispatch.
// Content hashing is now engine-computed; the invariant being tested is the same
// (malformed put → no buy-miss auto-accept), but the rejection point moved
// upstream to applyPut.
func TestBuyMiss_NoContentRejected(t *testing.T) {
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

	// Buyer puts with no content field — applyPut silently drops the message.
	// Use Apply (not Replay) to add the put to state without wiping the injected offer.
	noContentPayload, _ := json.Marshal(map[string]any{
		"description":  task,
		"token_cost":   int64(20000),
		"content_type": "code",
		"domains":      []string{"go", "testing"},
	})
	putMsg := h.sendMessage(h.buyer,
		noContentPayload,
		[]string{exchange.TagPut},
		nil,
	)

	putRec, _ := h.st.GetMessage(putMsg.ID)
	if putRec == nil {
		t.Fatal("put not in store")
	}
	putMsgObj := exchange.FromStoreRecord(putRec)
	eng.State().Apply(putMsgObj)

	// Dispatch must not return an error (put is silently ignored at state layer).
	// The put was never added to pendingPuts, so handlePut exits early with nil.
	if err := eng.DispatchForTest(putMsgObj); err != nil {
		t.Errorf("unexpected error dispatching no-content put: %v", err)
	}

	// No put-accept must have been emitted — the buy-miss offer must not be fulfilled.
	settleMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagSettle}})
	for _, sm := range settleMsgs {
		if hasTag(sm.Tags, "exchange:phase:put-accept") {
			t.Errorf("unexpected put-accept emitted for put with no content: msg %s", sm.ID)
		}
	}
}

// TestBuyMiss_ExpiredOfferSkipped verifies that when ClaimBuyMissOffer is called
// for a task hash whose offer has already expired, it returns nil and no
// put-accept is emitted when the matching put is dispatched.
//
// This test exercises the ClaimBuyMissOffer path specifically (rather than
// GetBuyMissOffer) to ensure the atomic claim+delete correctly respects expiry.
func TestBuyMiss_ExpiredOfferSkipped(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	task := "Port a 2000-line Python ML pipeline to Go with ONNX inference"
	taskHash := exchange.TaskDescriptionHash(task)

	eng := h.newEngine()

	// Replay existing state.
	msgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	// Inject an already-expired offer.
	expired := &exchange.BuyMissOffer{
		TaskHash:  taskHash,
		BuyMsgID:  "fake-buy-msg-id",
		BuyerKey:  h.buyer.PublicKeyHex(),
		Task:      task,
		ExpiresAt: time.Now().Add(-1 * time.Hour), // past expiry
	}
	set := eng.State().SetBuyMissOffer(expired)
	if !set {
		t.Fatal("SetBuyMissOffer returned false for empty map insert")
	}

	// ClaimBuyMissOffer must return nil — the offer is expired.
	claimed := eng.State().ClaimBuyMissOffer(taskHash)
	if claimed != nil {
		t.Errorf("ClaimBuyMissOffer returned non-nil for expired offer: %+v", claimed)
	}

	// Re-inject the expired offer (ClaimBuyMissOffer deleted it during the failed claim).
	eng.State().SetBuyMissOffer(expired)

	// Send a put for the same task from the buyer.
	putMsg := h.sendMessage(h.buyer,
		putPayload(task, "sha256:"+fmt.Sprintf("%064x", 30000), "code", 30000, 60000),
		[]string{exchange.TagPut},
		nil,
	)
	putRec, _ := h.st.GetMessage(putMsg.ID)
	if putRec == nil {
		t.Fatal("put message not found in store")
	}
	putMsgObj := exchange.FromStoreRecord(putRec)
	eng.State().Apply(putMsgObj)

	// Dispatch the put — should not trigger auto-accept (no live offer).
	if err := eng.DispatchForTest(putMsgObj); err != nil {
		t.Fatalf("DispatchForTest(put): %v", err)
	}

	// No put-accept must have been emitted.
	settleMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagSettle}})
	for _, sm := range settleMsgs {
		if hasTag(sm.Tags, "exchange:phase:put-accept") {
			t.Errorf("unexpected put-accept emitted for expired offer: msg %s", sm.ID)
		}
	}
}

// TestBuyMiss_ConcurrentBuyDedup verifies that when two buy messages arrive for
// the same task description, SetBuyMissOffer records exactly one standing offer
// (the second call is a no-op because a non-expired offer already exists).
//
// This directly tests the dedup guard in SetBuyMissOffer rather than relying on
// the engine loop, ensuring the behavior is a correctness property of State, not
// just a side-effect of message ordering.
func TestBuyMiss_ConcurrentBuyDedup(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	task := "Write a distributed tracing library in Go compatible with OpenTelemetry"
	taskHash := exchange.TaskDescriptionHash(task)

	eng := h.newEngine()

	// Inject the first offer.
	offer1 := &exchange.BuyMissOffer{
		TaskHash:  taskHash,
		BuyMsgID:  "buy-msg-001",
		BuyerKey:  h.buyer.PublicKeyHex(),
		Task:      task,
		ExpiresAt: time.Now().Add(exchange.BuyMissOfferTTL),
	}
	set1 := eng.State().SetBuyMissOffer(offer1)
	if !set1 {
		t.Fatal("SetBuyMissOffer returned false for first offer (empty map)")
	}

	// Attempt to set a second offer for the same task hash with a different buy msg ID.
	buyer2 := newTestAgent(t)
	offer2 := &exchange.BuyMissOffer{
		TaskHash:  taskHash,
		BuyMsgID:  "buy-msg-002",
		BuyerKey:  buyer2.PublicKeyHex(),
		Task:      task,
		ExpiresAt: time.Now().Add(exchange.BuyMissOfferTTL),
	}
	set2 := eng.State().SetBuyMissOffer(offer2)
	if set2 {
		t.Error("SetBuyMissOffer returned true for duplicate non-expired offer — should be false (dedup)")
	}

	// State must still hold only the first offer (offer1 not overwritten by offer2).
	got := eng.State().GetBuyMissOffer(taskHash)
	if got == nil {
		t.Fatal("GetBuyMissOffer returned nil — offer lost")
	}
	if got.BuyMsgID != offer1.BuyMsgID {
		t.Errorf("offer BuyMsgID = %q, want %q (first offer should win)", got.BuyMsgID, offer1.BuyMsgID)
	}
	if got.BuyerKey != offer1.BuyerKey {
		t.Errorf("offer BuyerKey = %q, want %q (first offer buyer should win)", got.BuyerKey, offer1.BuyerKey)
	}
}

// TestBuyMiss_ExpiryRace verifies that an offer with ExpiresAt set just barely
// in the future transitions cleanly from "live" to "expired". Before expiry,
// ClaimBuyMissOffer must return the offer. After expiry, it must return nil.
//
// This exercises the boundary of IsExpired() — specifically that an offer at
// ExpiresAt = now-1ns is expired and ExpiresAt = now+TTL is not, and that
// ClaimBuyMissOffer does not return a stale offer during the transition.
func TestBuyMiss_ExpiryRace(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	eng := h.newEngine()

	task := "Build a CRDT-based collaborative editor in TypeScript"
	taskHash := exchange.TaskDescriptionHash(task)

	// --- Part 1: offer that is NOT yet expired should be claimable.
	liveOffer := &exchange.BuyMissOffer{
		TaskHash:  taskHash,
		BuyMsgID:  "buy-live-001",
		BuyerKey:  h.buyer.PublicKeyHex(),
		Task:      task,
		ExpiresAt: time.Now().Add(exchange.BuyMissOfferTTL),
	}
	if !eng.State().SetBuyMissOffer(liveOffer) {
		t.Fatal("SetBuyMissOffer (live) returned false — should insert into empty map")
	}
	// Claim it — must succeed immediately (not expired).
	claimed := eng.State().ClaimBuyMissOffer(taskHash)
	if claimed == nil {
		t.Fatal("ClaimBuyMissOffer returned nil for a non-expired offer")
	}
	if claimed.BuyMsgID != liveOffer.BuyMsgID {
		t.Errorf("claimed offer BuyMsgID = %q, want %q", claimed.BuyMsgID, liveOffer.BuyMsgID)
	}
	// Offer must be gone after claim.
	if got := eng.State().GetBuyMissOffer(taskHash); got != nil {
		t.Error("offer still present in state after ClaimBuyMissOffer — should be deleted")
	}

	// --- Part 2: offer that expired one nanosecond ago must NOT be claimable.
	expiredOffer := &exchange.BuyMissOffer{
		TaskHash:  taskHash,
		BuyMsgID:  "buy-expired-001",
		BuyerKey:  h.buyer.PublicKeyHex(),
		Task:      task,
		ExpiresAt: time.Now().Add(-time.Nanosecond), // just expired
	}
	// SetBuyMissOffer overwrites because the slot is now empty (we just claimed).
	if !eng.State().SetBuyMissOffer(expiredOffer) {
		t.Fatal("SetBuyMissOffer (expired) returned false — slot should be empty after claim")
	}
	// ClaimBuyMissOffer must return nil for an expired offer.
	claimed2 := eng.State().ClaimBuyMissOffer(taskHash)
	if claimed2 != nil {
		t.Errorf("ClaimBuyMissOffer returned non-nil for a just-expired offer: %+v", claimed2)
	}
	// GetBuyMissOffer must also return nil.
	if got := eng.State().GetBuyMissOffer(taskHash); got != nil {
		t.Errorf("GetBuyMissOffer returned non-nil for a just-expired offer: %+v", got)
	}
}

// TestBuyMiss_SetOverwritesExpiredOffer verifies that SetBuyMissOffer allows a
// new offer to overwrite an expired one for the same task hash. The dedup guard
// in SetBuyMissOffer only protects non-expired offers; once an offer has expired
// a new one must be accepted.
//
// This prevents a scenario where a task's standing offer expires but the state
// map still holds the stale entry, blocking future offers for the same task.
func TestBuyMiss_SetOverwritesExpiredOffer(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	eng := h.newEngine()

	task := "Generate a multi-threaded file parser in C++ with SIMD acceleration"
	taskHash := exchange.TaskDescriptionHash(task)

	// Insert an offer that has already expired.
	expiredOffer := &exchange.BuyMissOffer{
		TaskHash:  taskHash,
		BuyMsgID:  "buy-stale-001",
		BuyerKey:  h.buyer.PublicKeyHex(),
		Task:      task,
		ExpiresAt: time.Now().Add(-1 * time.Hour), // already expired
	}
	// First set always succeeds (map is empty).
	if !eng.State().SetBuyMissOffer(expiredOffer) {
		t.Fatal("SetBuyMissOffer (initial insert) returned false — map was empty")
	}

	// Expired offer must NOT be visible via Get.
	if got := eng.State().GetBuyMissOffer(taskHash); got != nil {
		t.Fatalf("GetBuyMissOffer returned non-nil for expired offer: %+v", got)
	}

	// Now a fresh buyer sends a new buy for the same task.
	// SetBuyMissOffer must return true (overwrites expired entry).
	buyer2 := newTestAgent(t)
	freshOffer := &exchange.BuyMissOffer{
		TaskHash:  taskHash,
		BuyMsgID:  "buy-fresh-001",
		BuyerKey:  buyer2.PublicKeyHex(),
		Task:      task,
		ExpiresAt: time.Now().Add(exchange.BuyMissOfferTTL),
	}
	if !eng.State().SetBuyMissOffer(freshOffer) {
		t.Fatal("SetBuyMissOffer (fresh offer over expired) returned false — should overwrite expired entry")
	}

	// Fresh offer must be visible via Get.
	got := eng.State().GetBuyMissOffer(taskHash)
	if got == nil {
		t.Fatal("GetBuyMissOffer returned nil for fresh offer that replaced expired one")
	}
	if got.BuyMsgID != freshOffer.BuyMsgID {
		t.Errorf("active offer BuyMsgID = %q, want %q", got.BuyMsgID, freshOffer.BuyMsgID)
	}
	if got.BuyerKey != buyer2.PublicKeyHex() {
		t.Errorf("active offer BuyerKey = %q, want buyer2 %q", got.BuyerKey, buyer2.PublicKeyHex())
	}
}

// TestBuyMiss_MultipleOffersDifferentTasks verifies that buy-miss offers for
// distinct task descriptions are tracked independently. Each task hash gets its
// own slot — claiming one does not affect the other.
//
// This tests that the task-hash key space is correctly scoped and a bug in
// key derivation (e.g. collision or shared map key) does not cause cross-task
// offer interference.
func TestBuyMiss_MultipleOffersDifferentTasks(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	eng := h.newEngine()

	taskA := "Write a ZK-proof circuit for a hash commitment in Circom"
	taskB := "Build a Wasm runtime in Rust with memory-safe sandbox isolation"
	hashA := exchange.TaskDescriptionHash(taskA)
	hashB := exchange.TaskDescriptionHash(taskB)

	if hashA == hashB {
		t.Fatal("task hashes collide — test setup invalid")
	}

	offerA := &exchange.BuyMissOffer{
		TaskHash:  hashA,
		BuyMsgID:  "buy-task-a",
		BuyerKey:  h.buyer.PublicKeyHex(),
		Task:      taskA,
		ExpiresAt: time.Now().Add(exchange.BuyMissOfferTTL),
	}
	offerB := &exchange.BuyMissOffer{
		TaskHash:  hashB,
		BuyMsgID:  "buy-task-b",
		BuyerKey:  h.seller.PublicKeyHex(),
		Task:      taskB,
		ExpiresAt: time.Now().Add(exchange.BuyMissOfferTTL),
	}

	if !eng.State().SetBuyMissOffer(offerA) {
		t.Fatal("SetBuyMissOffer(offerA) returned false")
	}
	if !eng.State().SetBuyMissOffer(offerB) {
		t.Fatal("SetBuyMissOffer(offerB) returned false")
	}

	// Both offers must be independently retrievable.
	gotA := eng.State().GetBuyMissOffer(hashA)
	if gotA == nil {
		t.Fatal("GetBuyMissOffer(hashA) returned nil")
	}
	if gotA.BuyMsgID != offerA.BuyMsgID {
		t.Errorf("offer A BuyMsgID = %q, want %q", gotA.BuyMsgID, offerA.BuyMsgID)
	}

	gotB := eng.State().GetBuyMissOffer(hashB)
	if gotB == nil {
		t.Fatal("GetBuyMissOffer(hashB) returned nil")
	}
	if gotB.BuyMsgID != offerB.BuyMsgID {
		t.Errorf("offer B BuyMsgID = %q, want %q", gotB.BuyMsgID, offerB.BuyMsgID)
	}

	// Claiming offer A must not affect offer B.
	claimedA := eng.State().ClaimBuyMissOffer(hashA)
	if claimedA == nil {
		t.Fatal("ClaimBuyMissOffer(hashA) returned nil")
	}
	if claimedA.BuyMsgID != offerA.BuyMsgID {
		t.Errorf("claimed A BuyMsgID = %q, want %q", claimedA.BuyMsgID, offerA.BuyMsgID)
	}

	// Offer A is gone; offer B survives.
	if eng.State().GetBuyMissOffer(hashA) != nil {
		t.Error("offer A still present after ClaimBuyMissOffer — should be deleted")
	}
	if eng.State().GetBuyMissOffer(hashB) == nil {
		t.Error("offer B was deleted when offer A was claimed — should be independent")
	}

	// Claim offer B — must succeed independently.
	claimedB := eng.State().ClaimBuyMissOffer(hashB)
	if claimedB == nil {
		t.Fatal("ClaimBuyMissOffer(hashB) returned nil after claiming A")
	}
	if claimedB.BuyMsgID != offerB.BuyMsgID {
		t.Errorf("claimed B BuyMsgID = %q, want %q", claimedB.BuyMsgID, offerB.BuyMsgID)
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

// TestBuyMiss_AutoAcceptEmitsHotCompressionAssignToSeller verifies the full
// end-to-end path: seller sends buy (no inventory) → engine emits buy-miss offer
// → same seller puts the result (fulfilling their own miss) → engine auto-accepts
// → compression assign (hot-compression, task_type="compress") is emitted
// exclusively to the seller.
//
// The buy-miss protocol requires the putter to be the same agent who received
// the offer (BuyerKey). Here h.seller plays both roles: it sends the buy, receives
// the miss offer, computes the result, and puts it. The compression assign then
// targets h.seller as exclusive_sender.
//
// This test uses the real engine harness (newTestHarness + startEngine) with
// real put+buy messages in the store. No mocking of sendCompressionAssign.
func TestBuyMiss_AutoAcceptEmitsHotCompressionAssignToSeller(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	eng := h.newEngine()

	// -----------------------------------------------------------------------
	// Step 1: Seller sends buy — no inventory, engine emits buy-miss offer.
	// The seller is the requester here; it will also fulfill its own miss.
	// -----------------------------------------------------------------------
	task := "Implement a Rust async executor with work-stealing scheduler"
	const tokenCost int64 = 60000

	// h.seller sends the buy (no inventory → buy-miss emitted with BuyerKey=seller).
	_ = h.sendMessage(h.seller,
		buyPayload(task, tokenCost*2),
		[]string{exchange.TagBuy},
		nil,
	)

	preBuyMiss, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagBuyMiss}})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	h.startEngine(eng, ctx, cancel)

	// Wait for buy-miss offer.
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
		t.Fatal("step 1: no buy-miss offer emitted by engine")
	}

	// -----------------------------------------------------------------------
	// Step 2: Seller puts the result matching the buy-miss task description.
	// The sender must match the BuyerKey of the standing offer (h.seller).
	// -----------------------------------------------------------------------
	preAssigns, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagAssign}})
	preAssignCount := len(preAssigns)

	// h.seller sends the put — matches BuyerKey so the engine auto-accepts.
	putMsg := h.sendMessage(h.seller,
		putPayload(task, "sha256:"+fmt.Sprintf("%064x", tokenCost), "code", tokenCost, tokenCost*2),
		[]string{exchange.TagPut, "exchange:content-type:code"},
		nil,
	)

	// -----------------------------------------------------------------------
	// Step 3: Wait for compression assign to appear in the store.
	// -----------------------------------------------------------------------
	var assignMsgs []store.MessageRecord
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		assignMsgs, _ = h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagAssign}})
		// Look for a new assign with task_type="compress" for this put entry.
		var found bool
		for i := range assignMsgs {
			if i < preAssignCount {
				continue
			}
			var p struct {
				TaskType string `json:"task_type"`
				EntryID  string `json:"entry_id"`
			}
			if json.Unmarshal(assignMsgs[i].Payload, &p) == nil &&
				p.TaskType == "compress" && p.EntryID == putMsg.ID {
				found = true
				break
			}
		}
		if found {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()

	// -----------------------------------------------------------------------
	// Step 4: Assert the assign message properties.
	// -----------------------------------------------------------------------
	var compressionAssign *store.MessageRecord
	for i := range assignMsgs {
		var p struct {
			TaskType string `json:"task_type"`
			EntryID  string `json:"entry_id"`
		}
		if json.Unmarshal(assignMsgs[i].Payload, &p) == nil &&
			p.TaskType == "compress" && p.EntryID == putMsg.ID {
			compressionAssign = &assignMsgs[i]
			break
		}
	}
	if compressionAssign == nil {
		t.Fatalf("buy-miss auto-accept: no hot-compression assign emitted for put entry %s (total TagAssign msgs: %d)",
			putMsg.ID[:8], len(assignMsgs))
	}

	// Assign must be sent by the operator.
	if compressionAssign.Sender != h.operator.PublicKeyHex() {
		t.Errorf("assign sender = %q, want operator %q", compressionAssign.Sender, h.operator.PublicKeyHex())
	}

	// Decode and verify payload fields.
	var ap struct {
		TaskType        string `json:"task_type"`
		EntryID         string `json:"entry_id"`
		Reward          int64  `json:"reward"`
		ExclusiveSender string `json:"exclusive_sender"`
		Description     string `json:"description"`
	}
	if err := json.Unmarshal(compressionAssign.Payload, &ap); err != nil {
		t.Fatalf("decoding compression assign payload: %v", err)
	}

	// task_type must be "compress" (hot-compression path).
	if ap.TaskType != "compress" {
		t.Errorf("assign task_type = %q, want %q", ap.TaskType, "compress")
	}

	// entry_id must reference the seller's put message.
	if ap.EntryID != putMsg.ID {
		t.Errorf("assign entry_id = %q, want put message ID %q", ap.EntryID, putMsg.ID)
	}

	// exclusive_sender must be the seller (the original author of the cached result).
	if ap.ExclusiveSender != h.seller.PublicKeyHex() {
		t.Errorf("assign exclusive_sender = %q, want seller key %q", ap.ExclusiveSender, h.seller.PublicKeyHex())
	}

	// Reward (bounty) must be 50% of token_cost (hot compression rate).
	wantBounty := tokenCost * int64(exchange.HotCompressionBountyPct) / 100
	if ap.Reward != wantBounty {
		t.Errorf("assign reward = %d, want %d (%d%% of token_cost %d)",
			ap.Reward, wantBounty, exchange.HotCompressionBountyPct, tokenCost)
	}

	// Description must be non-empty.
	if ap.Description == "" {
		t.Error("assign description is empty")
	}
}

// TestBuyMiss_SellerBalanceAfterStandingOfferFulfillment verifies that the
// seller's scrip balance in the CampfireScripStore increases by exactly the
// put-pay amount after the engine auto-accepts a put matching a standing offer.
//
// Done condition (dontguess-3c3): cs.Balance(sellerKey) == tokenCost *
// BuyMissOfferRate / 100 after put-accept, asserted via store state —
// not log parsing.
//
// Two assertions:
//  1. In-memory balance updated immediately by the engine (AddBudget path).
//  2. Fresh Replay from campfire log reproduces the same balance (scrip-put-pay
//     message is correctly emitted and replayed via applyPutPay).
func TestBuyMiss_SellerBalanceAfterStandingOfferFulfillment(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)

	// Mint operator scrip first so applyPutPay can decrement the operator
	// balance during Replay without underflowing. The mint must be in the
	// campfire log before constructing the scrip store so Replay sees it.
	const tokenCost int64 = 90000
	expectedPutPay := tokenCost * int64(exchange.BuyMissOfferRate) / 100
	// Operator needs at least expectedPutPay to cover the put-pay disbursement.
	addScripMintMsg(t, h, h.operator.PublicKeyHex(), expectedPutPay+10000)

	// Build the scrip store after the mint message is in the log.
	cs := newCampfireScripStore(t, h)
	eng := exchange.NewEngine(exchange.EngineOptions{
		CampfireID:  h.cfID,
		Store:       h.st,
		ReadClient:  h.newOperatorClient(),
		WriteClient: h.newOperatorClient(),
		ScripStore:  cs,
		Logger:      func(format string, args ...any) { t.Logf("[engine] "+format, args...) },
	})

	task := "Translate a 3000-line C++ game engine to idiomatic Go"
	sellerKey := h.buyer.PublicKeyHex() // buyer is also the seller in buy-miss (computed it themselves)

	// Step 1: Buyer sends a buy — no inventory => engine emits buy-miss offer.
	_ = h.sendMessage(h.buyer,
		buyPayload(task, 120000),
		[]string{exchange.TagBuy},
		nil,
	)

	preBuyMiss, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagBuyMiss}})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go func() { _ = eng.Start(ctx) }()

	// Wait for the buy-miss offer to appear.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		msgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagBuyMiss}})
		if len(msgs) > len(preBuyMiss) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	buyMissMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagBuyMiss}})
	if len(buyMissMsgs) <= len(preBuyMiss) {
		cancel()
		t.Fatal("step 1: no buy-miss offer emitted")
	}

	// Step 2: Buyer puts the result — same task description triggers auto-accept.
	_ = h.sendMessage(h.buyer,
		putPayload(task, "sha256:"+fmt.Sprintf("%064x", tokenCost), "code", tokenCost, tokenCost*2),
		[]string{exchange.TagPut, "exchange:content-type:code"},
		nil,
	)

	// Step 3: Wait for the put-accept settle message.
	preSettle, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagSettle}})
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		msgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagSettle}})
		if len(msgs) > len(preSettle) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()

	settleMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagSettle}})
	if len(settleMsgs) <= len(preSettle) {
		t.Fatal("step 3: no put-accept settle message emitted")
	}
	var putAcceptFound bool
	for _, sm := range settleMsgs {
		if hasTag(sm.Tags, "exchange:phase:put-accept") {
			putAcceptFound = true
			break
		}
	}
	if !putAcceptFound {
		t.Fatal("step 3: settle messages exist but none have put-accept phase tag")
	}

	// --- Assertion 1: in-memory store balance updated by engine (AddBudget path) ---
	//
	// The engine calls ScripStore.AddBudget(sellerKey, offeredPrice) directly
	// before emitting the scrip-put-pay campfire message. The balance must reflect
	// the payment immediately in the live store instance.
	inMemoryBalance := cs.Balance(sellerKey)
	if inMemoryBalance != expectedPutPay {
		t.Errorf("in-memory seller balance after put-accept: got %d, want %d (tokenCost=%d * offerRate=%d%%)",
			inMemoryBalance, expectedPutPay, tokenCost, exchange.BuyMissOfferRate)
	}

	// --- Assertion 2: fresh Replay from campfire log reproduces the same balance ---
	//
	// The engine emits a scrip-put-pay campfire message so CampfireScripStore can
	// reconstruct state from the log. A fresh store constructed from the same log
	// must show the same seller balance. This verifies the campfire message was
	// actually emitted and applyPutPay handles it correctly.
	freshCS, err := scrip.NewCampfireScripStore(h.cfID, h.newOperatorClient(), h.operator.PublicKeyHex())
	if err != nil {
		t.Fatalf("NewCampfireScripStore (fresh): %v", err)
	}
	replayedBalance := freshCS.Balance(sellerKey)
	if replayedBalance != expectedPutPay {
		t.Errorf("replayed seller balance after put-accept: got %d, want %d (tokenCost=%d * offerRate=%d%%)",
			replayedBalance, expectedPutPay, tokenCost, exchange.BuyMissOfferRate)
	}
}
