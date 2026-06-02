package exchange_test

// TestConsumeSignal_RecordedAndQueryablePerEntry verifies that when a buyer
// completes a transaction (settle(complete)), the engine emits an
// exchange:consume message that is:
//
//  1. Present in the campfire message store tagged exchange:consume.
//  2. Carries the correct entry_id in its payload (derived from the
//     antecedent chain, not from buyer-supplied fields).
//  3. Queryable per-entry via ConsumeCountByEntry — the reporter function
//     used by the hit-rate / value reporter.
//
// This exercises the real path: real campfire (fs transport), real Ed25519
// keys, real SQLite store. No mocks.
//
// Done condition per §5 M5 of docs/design/exchange-matching-measurement-review.md:
// "a test shows that when a buyer accepts a delivered candidate, a consume
// signal is recorded and is queryable per entry."

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/campfire-net/campfire/cf-protocol/store"

	"github.com/campfire-net/dontguess/pkg/exchange"
)

func TestConsumeSignal_RecordedAndQueryablePerEntry(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	// --- Step 1: Seller puts cached inference ---
	putMsg := h.sendMessage(h.seller,
		putPayload("Go flock contention test pattern", "sha256:"+fmt.Sprintf("%064x", 999), "code", 16000, 32000),
		[]string{exchange.TagPut, "exchange:content-type:code", "exchange:domain:go"},
		nil,
	)

	// Replay to pick up the put, then auto-accept.
	msgs, err := h.st.ListMessages(h.cfID, 0)
	if err != nil {
		t.Fatalf("listing messages: %v", err)
	}
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	if err := eng.AutoAcceptPut(putMsg.ID, 11200, time.Now().Add(72*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut: %v", err)
	}

	inv := eng.State().Inventory()
	if len(inv) != 1 {
		t.Fatalf("expected 1 inventory entry after put-accept, got %d", len(inv))
	}
	entryID := inv[0].EntryID

	// --- Step 2: Buyer sends buy, engine runs and emits match ---
	buyMsg := h.sendMessage(h.buyer,
		buyPayload("flock contention test pattern for Go concurrency", 50000),
		[]string{exchange.TagBuy},
		nil,
	)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	preMatchMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})
	go func() { _ = eng.Start(ctx) }()

	var matchMsgs []store.MessageRecord
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		matchMsgs, _ = h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})
		if len(matchMsgs) > len(preMatchMsgs) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()

	if len(matchMsgs) <= len(preMatchMsgs) {
		t.Fatal("no match message emitted by engine")
	}
	matchMsg := matchMsgs[len(matchMsgs)-1]
	_ = buyMsg // exercised via engine; ID used for match antecedent assertion below

	var matchPayload struct {
		Results []struct {
			EntryID string `json:"entry_id"`
			Price   int64  `json:"price"`
		} `json:"results"`
	}
	if err := json.Unmarshal(matchMsg.Payload, &matchPayload); err != nil || len(matchPayload.Results) == 0 {
		t.Fatalf("parsing match payload: %v", err)
	}
	if matchPayload.Results[0].EntryID != entryID {
		t.Errorf("match result entry_id = %q, want %q", matchPayload.Results[0].EntryID, entryID)
	}

	// Re-sync state.
	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	// --- Step 3: Buyer sends buyer-accept ---
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
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	// --- Step 4: Operator delivers content ---
	deliverPayload, _ := json.Marshal(map[string]any{
		"phase":        "deliver",
		"entry_id":     matchPayload.Results[0].EntryID,
		"content_ref":  "sha256:" + fmt.Sprintf("%064x", 999),
		"content_size": 32000,
	})
	deliverMsg := h.sendMessage(h.operator, deliverPayload,
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrDeliver,
		},
		[]string{buyerAcceptMsg.ID},
	)

	allMsgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	// --- Step 5: Buyer completes (the consume event) ---
	// Record consume-message count before sending complete.
	preConsumeMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagConsume}})
	preConsumeCount := len(preConsumeMsgs)

	completePayload, _ := json.Marshal(map[string]any{
		"phase":                 "complete",
		"entry_id":              matchPayload.Results[0].EntryID,
		"price":                 matchPayload.Results[0].Price,
		"content_hash_verified": true,
	})
	completeMsgRec := h.sendMessage(h.buyer, completePayload,
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrComplete,
			exchange.TagVerdictPrefix + "accepted",
		},
		[]string{deliverMsg.ID},
	)

	allMsgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	// Dispatch the complete message through the engine to trigger emitConsumeSignal.
	// engine.dispatch is the handler path that calls handleSettle → emitConsumeSignal.
	if err := eng.DispatchForTest(completeMsgRec); err != nil {
		t.Fatalf("DispatchForTest(complete): %v", err)
	}

	// --- Assertion 1: exchange:consume message was emitted ---
	postConsumeMsgs, err := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagConsume}})
	if err != nil {
		t.Fatalf("listing consume messages: %v", err)
	}
	if len(postConsumeMsgs) <= preConsumeCount {
		t.Fatalf("no exchange:consume message emitted after settle(complete): before=%d, after=%d",
			preConsumeCount, len(postConsumeMsgs))
	}

	consumeMsg := postConsumeMsgs[len(postConsumeMsgs)-1]

	// --- Assertion 2: consume message carries the correct entry_id ---
	var consumePayload struct {
		EntryID  string `json:"entry_id"`
		BuyerKey string `json:"buyer_key"`
	}
	if err := json.Unmarshal(consumeMsg.Payload, &consumePayload); err != nil {
		t.Fatalf("parsing consume message payload: %v", err)
	}
	if consumePayload.EntryID != entryID {
		t.Errorf("consume signal entry_id = %q, want %q (derived entry)", consumePayload.EntryID, entryID)
	}
	if consumePayload.BuyerKey != h.buyer.PublicKeyHex() {
		t.Errorf("consume signal buyer_key = %q, want %q", consumePayload.BuyerKey, h.buyer.PublicKeyHex())
	}

	// --- Assertion 3: antecedent is the complete message ---
	if len(consumeMsg.Antecedents) == 0 || consumeMsg.Antecedents[0] != completeMsgRec.ID {
		t.Errorf("consume message antecedent = %v, want [%s]", consumeMsg.Antecedents, completeMsgRec.ID[:8])
	}

	// --- Assertion 4: ConsumeCountByEntry tallies correctly (reporter path) ---
	consumeExchangeMsgs := exchange.FromStoreRecords(postConsumeMsgs)
	counts := exchange.ConsumeCountByEntry(consumeExchangeMsgs)
	if got := counts[entryID]; got != 1 {
		t.Errorf("ConsumeCountByEntry[%q] = %d, want 1", entryID, got)
	}
}

// TestConsumeCountByEntry_MultipleConsumes verifies that ConsumeCountByEntry
// correctly tallies multiple consume signals for the same entry and different
// entries from a slice of exchange:consume messages. This exercises the
// reporter function directly using message fixtures (no engine required).
func TestConsumeCountByEntry_MultipleConsumes(t *testing.T) {
	t.Parallel()

	makeConsumeMsg := func(id, entryID, buyerKey string) exchange.Message {
		payload, _ := json.Marshal(map[string]any{
			"entry_id":  entryID,
			"buyer_key": buyerKey,
		})
		return exchange.Message{
			ID:      id,
			Tags:    []string{exchange.TagConsume},
			Payload: payload,
		}
	}

	consumes := []exchange.Message{
		makeConsumeMsg("c-1", "entry-A", "buyer-1"),
		makeConsumeMsg("c-2", "entry-A", "buyer-2"),
		makeConsumeMsg("c-3", "entry-B", "buyer-1"),
		makeConsumeMsg("c-4", "entry-A", "buyer-3"),
	}

	counts := exchange.ConsumeCountByEntry(consumes)

	if counts["entry-A"] != 3 {
		t.Errorf("entry-A count = %d, want 3", counts["entry-A"])
	}
	if counts["entry-B"] != 1 {
		t.Errorf("entry-B count = %d, want 1", counts["entry-B"])
	}
	if _, ok := counts["entry-C"]; ok {
		t.Errorf("entry-C should not appear in counts, got %d", counts["entry-C"])
	}
}

// TestConsumeSignal_LiveStateReflectedWithoutReplay is the regression test for
// the MEDIUM fix (dontguess-fe7 fix 3): after a settle:complete that triggers
// emitConsumeSignal, the live State.entryConsumeCount (and thus
// AllEntryBehavioralSignals) must reflect the consume WITHOUT a replay/restart.
//
// Real path: real engine (real campfire, real keys), DispatchForTest fires the
// full handleSettle → emitConsumeSignal → state.Apply chain. We check the live
// State via AllEntryBehavioralSignals (the same read path used by the match index).
// No mocks of the path under test.
func TestConsumeSignal_LiveStateReflectedWithoutReplay(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	// --- Setup: put → accept → buy → match → buyer-accept → deliver ---
	putMsg := h.sendMessage(h.seller,
		putPayload("campfire convention dispatch lifecycle management", "sha256:"+fmt.Sprintf("%064x", 777), "code", 12000, 24000),
		[]string{exchange.TagPut, "exchange:content-type:code", "exchange:domain:go"},
		nil,
	)
	msgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))
	if err := eng.AutoAcceptPut(putMsg.ID, 8400, time.Now().Add(72*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut: %v", err)
	}
	inv := eng.State().Inventory()
	if len(inv) == 0 {
		t.Fatal("no inventory after put-accept")
	}
	entryID := inv[0].EntryID

	buyMsg := h.sendMessage(h.buyer,
		buyPayload("convention dispatch lifecycle for campfire coordination", 50000),
		[]string{exchange.TagBuy},
		nil,
	)
	_ = buyMsg

	// Operator emits a match directly (skip engine auto-match).
	matchPayloadBytes, _ := json.Marshal(map[string]any{
		"results": []map[string]any{
			{"entry_id": entryID, "score": 0.92, "price": 500},
		},
	})
	matchMsg := h.sendMessage(h.operator, matchPayloadBytes,
		[]string{exchange.TagMatch},
		[]string{buyMsg.ID},
	)

	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	buyerAcceptPayload, _ := json.Marshal(map[string]any{
		"phase":    exchange.SettlePhaseStrBuyerAccept,
		"entry_id": entryID,
		"accepted": true,
	})
	buyerAcceptMsg := h.sendMessage(h.buyer, buyerAcceptPayload,
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrBuyerAccept,
		},
		[]string{matchMsg.ID},
	)

	allMsgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	deliverPayload, _ := json.Marshal(map[string]any{
		"phase":       exchange.SettlePhaseStrDeliver,
		"entry_id":    entryID,
		"content_ref": "sha256:" + fmt.Sprintf("%064x", 777),
	})
	deliverMsg := h.sendMessage(h.operator, deliverPayload,
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrDeliver,
		},
		[]string{buyerAcceptMsg.ID},
	)

	allMsgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	// Verify live state has NO consume signal yet.
	sigsBefore := eng.State().AllEntryBehavioralSignals()
	if sigsBefore[entryID].ConsumeCount != 0 {
		t.Fatalf("expected ConsumeCount=0 before complete, got %d", sigsBefore[entryID].ConsumeCount)
	}

	// --- Key step: dispatch settle:complete through the engine ---
	// This triggers emitConsumeSignal, which (after fix 3) immediately calls
	// state.Apply on the emitted consume message.
	completePayload, _ := json.Marshal(map[string]any{
		"phase":    exchange.SettlePhaseStrComplete,
		"entry_id": entryID,
	})
	completeMsg := h.sendMessage(h.buyer, completePayload,
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrComplete,
		},
		[]string{deliverMsg.ID},
	)

	if err := eng.DispatchForTest(completeMsg); err != nil {
		t.Fatalf("DispatchForTest(complete): %v", err)
	}

	// --- Regression assertion: live state reflects the consume WITHOUT replay ---
	// Before fix 3, entryConsumeCount was only updated on replay. After fix 3,
	// emitConsumeSignal applies the emitted message immediately to live state.
	sigsAfter := eng.State().AllEntryBehavioralSignals()
	if sigsAfter[entryID].ConsumeCount == 0 {
		t.Errorf("AllEntryBehavioralSignals ConsumeCount = 0 after settle:complete — live state was NOT updated by emitConsumeSignal (fix 3 regression)")
	}
}

// TestConsumeCountByEntry_SkipsMalformed verifies that ConsumeCountByEntry
// silently skips consume messages with missing or unparseable entry_id.
func TestConsumeCountByEntry_SkipsMalformed(t *testing.T) {
	t.Parallel()

	consumes := []exchange.Message{
		{ID: "c-1", Tags: []string{exchange.TagConsume}, Payload: []byte(`{"entry_id":"entry-X","buyer_key":"b1"}`)},
		{ID: "c-2", Tags: []string{exchange.TagConsume}, Payload: []byte(`{}`)  },             // missing entry_id
		{ID: "c-3", Tags: []string{exchange.TagConsume}, Payload: []byte(`not-json`)},       // unparseable
		{ID: "c-4", Tags: []string{exchange.TagConsume}, Payload: []byte(`{"entry_id":""}`)}, // empty entry_id
	}

	counts := exchange.ConsumeCountByEntry(consumes)

	if counts["entry-X"] != 1 {
		t.Errorf("entry-X count = %d, want 1", counts["entry-X"])
	}
	if len(counts) != 1 {
		t.Errorf("counts has %d entries, want 1 (malformed messages must be skipped)", len(counts))
	}
}
