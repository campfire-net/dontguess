package exchange_test

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/campfire-net/campfire/pkg/store"

	"github.com/3dl-dev/dontguess/pkg/exchange"
)

// TestWarmCompression_MatchTriggersAssign verifies that when the engine matches
// a buy and the matched entry has no compressed derivative, an exchange:assign
// message is emitted with:
//   - task_type = "compress"
//   - exclusive_sender = buyer's public key
//   - reward = 30% of the entry's token_cost (floor)
//   - entry_id = the put message ID
func TestWarmCompression_MatchTriggersAssign(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	eng := h.newEngine()

	const tokenCost int64 = 20000
	const wantBounty int64 = tokenCost * 30 / 100 // 6000

	contentHash := "sha256:" + fmt.Sprintf("%064x", 77)

	// Step 1: put raw entry and replay so the put is in state.
	putMsg := h.sendMessage(h.seller,
		putPayload("Kubernetes deployment patterns", contentHash, "code", tokenCost, 32768),
		[]string{exchange.TagPut, "exchange:content-type:code", "exchange:domain:go"},
		nil,
	)

	msgs, err := h.st.ListMessages(h.cfID, 0)
	if err != nil {
		t.Fatalf("ListMessages after put: %v", err)
	}
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	// Step 2: accept the put — this adds the entry to live inventory and
	// rebuilds the match index internally.
	if err := eng.AutoAcceptPut(putMsg.ID, 8000, time.Now().Add(168*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut: %v", err)
	}

	inv := eng.State().Inventory()
	if len(inv) != 1 {
		t.Fatalf("expected 1 inventory entry, got %d", len(inv))
	}

	// Verify the match index has the entry so a buy can find it.
	if n := eng.MatchIndexLen(); n == 0 {
		t.Fatal("match index is empty after AutoAcceptPut — cannot test match path")
	}

	// Step 3: buyer sends a buy request with generous budget to cover the
	// engine's 1.20x markup on the accept price (8000 * 1.2 = 9600, plus any
	// fast-loop adjustment). Apply (not Replay) to preserve state.
	buyMsg := h.sendMessage(h.buyer,
		buyPayload("Kubernetes deployment patterns", 15000),
		[]string{exchange.TagBuy},
		nil,
	)

	buyRec := mustGetStoreRecord(t, h, buyMsg.ID)
	buyExchangeMsg := exchange.FromStoreRecord(buyRec)
	eng.State().Apply(buyExchangeMsg)

	if err := eng.DispatchForTest(buyExchangeMsg); err != nil {
		t.Fatalf("DispatchForTest(buy): %v", err)
	}

	// Step 4: verify a match message was sent.
	matchMsgs, err := h.st.ListMessages(h.cfID, 0, store.MessageFilter{
		Tags: []string{exchange.TagMatch},
	})
	if err != nil {
		t.Fatalf("ListMessages(TagMatch): %v", err)
	}
	if len(matchMsgs) == 0 {
		t.Fatal("expected a match message, found none — likely a buy-miss (check match index size)")
	}

	// Step 5: find the warm compression assign for the buyer.
	allAssigns, err := h.st.ListMessages(h.cfID, 0, store.MessageFilter{
		Tags: []string{exchange.TagAssign},
	})
	if err != nil {
		t.Fatalf("ListMessages(TagAssign): %v", err)
	}

	var warmAssign *store.MessageRecord
	for i := range allAssigns {
		msg := &allAssigns[i]
		var p struct {
			TaskType        string `json:"task_type"`
			EntryID         string `json:"entry_id"`
			ExclusiveSender string `json:"exclusive_sender"`
		}
		if err := json.Unmarshal(msg.Payload, &p); err != nil {
			continue
		}
		// The warm assign must target the buyer (exclusive_sender == buyer key)
		// and reference the raw entry.
		if p.TaskType == "compress" && p.EntryID == putMsg.ID && p.ExclusiveSender == h.buyer.PublicKeyHex() {
			warmAssign = msg
			break
		}
	}

	if warmAssign == nil {
		t.Fatalf("expected warm compression assign for buyer (exclusive_sender=%.16s), found none (total TagAssign msgs: %d)",
			h.buyer.PublicKeyHex(), len(allAssigns))
	}

	// Decode and validate payload.
	var payload struct {
		EntryID         string `json:"entry_id"`
		TaskType        string `json:"task_type"`
		Reward          int64  `json:"reward"`
		ExclusiveSender string `json:"exclusive_sender"`
		Description     string `json:"description"`
	}
	if err := json.Unmarshal(warmAssign.Payload, &payload); err != nil {
		t.Fatalf("decoding warm assign payload: %v", err)
	}

	if payload.ExclusiveSender != h.buyer.PublicKeyHex() {
		t.Errorf("exclusive_sender = %q, want buyer key %q",
			payload.ExclusiveSender, h.buyer.PublicKeyHex())
	}
	if payload.Reward != wantBounty {
		t.Errorf("reward = %d, want %d (30%% of token_cost %d)", payload.Reward, wantBounty, tokenCost)
	}
	if payload.EntryID != putMsg.ID {
		t.Errorf("entry_id = %q, want put msg ID %q", payload.EntryID, putMsg.ID)
	}
	if payload.TaskType != "compress" {
		t.Errorf("task_type = %q, want %q", payload.TaskType, "compress")
	}
	if payload.Description == "" {
		t.Error("description is empty")
	}
}

// TestWarmCompression_SkippedWhenDerivativeExists verifies that when a
// compressed derivative already exists for the matched entry, NO warm
// compression assign is sent to the buyer.
func TestWarmCompression_SkippedWhenDerivativeExists(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	eng := h.newEngine()

	const tokenCost int64 = 16000
	contentHash := "sha256:" + fmt.Sprintf("%064x", 88)

	// Step 1: put raw entry and accept it.
	putMsg := h.sendMessage(h.seller,
		putPayload("Go channel patterns", contentHash, "code", tokenCost, 24576),
		[]string{exchange.TagPut, "exchange:content-type:code", "exchange:domain:go"},
		nil,
	)

	msgs, err := h.st.ListMessages(h.cfID, 0)
	if err != nil {
		t.Fatalf("ListMessages after put: %v", err)
	}
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	if err := eng.AutoAcceptPut(putMsg.ID, 7000, time.Now().Add(168*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut: %v", err)
	}

	inv := eng.State().Inventory()
	if len(inv) != 1 {
		t.Fatalf("expected 1 inventory entry, got %d", len(inv))
	}
	origEntryID := inv[0].EntryID

	// Step 2: create compression derivative via the full assign lifecycle.
	// Use Apply (not Replay) so the derivative created by handleAssignAccept
	// persists in the in-memory state across steps.
	compressedHash := "sha256:" + fmt.Sprintf("%064x", 189)
	assignPayload, _ := json.Marshal(map[string]any{
		"entry_id":         origEntryID,
		"task_type":        "compress",
		"reward":           tokenCost / 2,
		"exclusive_sender": h.seller.PublicKeyHex(),
	})
	assignMsg := h.sendMessage(h.operator, assignPayload, []string{exchange.TagAssign}, nil)
	assignExchangeMsg := exchange.FromStoreRecord(mustGetStoreRecord(t, h, assignMsg.ID))
	eng.State().Apply(assignExchangeMsg)
	if err := eng.DispatchForTest(assignExchangeMsg); err != nil {
		t.Fatalf("DispatchForTest(assign): %v", err)
	}

	// Seller claims the assign.
	claimMsg := h.sendMessage(h.seller, []byte(`{}`), []string{exchange.TagAssignClaim}, []string{assignMsg.ID})
	claimExchangeMsg := exchange.FromStoreRecord(mustGetStoreRecord(t, h, claimMsg.ID))
	eng.State().Apply(claimExchangeMsg)
	if err := eng.DispatchForTest(claimExchangeMsg); err != nil {
		t.Fatalf("DispatchForTest(claim): %v", err)
	}

	// Seller completes with compressed payload.
	completePayload, _ := json.Marshal(map[string]any{
		"content_hash": compressedHash,
		"content_size": int64(8000),
	})
	completeMsg := h.sendMessage(h.seller, completePayload, []string{exchange.TagAssignComplete}, []string{claimMsg.ID})
	completeExchangeMsg := exchange.FromStoreRecord(mustGetStoreRecord(t, h, completeMsg.ID))
	eng.State().Apply(completeExchangeMsg)
	if err := eng.DispatchForTest(completeExchangeMsg); err != nil {
		t.Fatalf("DispatchForTest(complete): %v", err)
	}

	// Operator accepts — this triggers createCompressionDerivative in the engine,
	// which calls applyDerivativePut and rebuilds the match index.
	acceptMsg := h.sendMessage(h.operator, []byte(`{}`), []string{exchange.TagAssignAccept}, []string{completeMsg.ID})
	acceptExchangeMsg := exchange.FromStoreRecord(mustGetStoreRecord(t, h, acceptMsg.ID))
	eng.State().Apply(acceptExchangeMsg)
	if err := eng.DispatchForTest(acceptExchangeMsg); err != nil {
		t.Fatalf("DispatchForTest(accept): %v", err)
	}

	// Verify the derivative exists in inventory.
	inv = eng.State().Inventory()
	var hasDerivative bool
	for _, e := range inv {
		if e.CompressedFrom == origEntryID {
			hasDerivative = true
			break
		}
	}
	if !hasDerivative {
		t.Fatal("expected compression derivative in inventory after assign-accept, found none")
	}

	// Record all assign messages before the buy.
	assignsBefore, err := h.st.ListMessages(h.cfID, 0, store.MessageFilter{
		Tags: []string{exchange.TagAssign},
	})
	if err != nil {
		t.Fatalf("ListMessages before buy: %v", err)
	}
	beforeIDs := make(map[string]bool, len(assignsBefore))
	for _, m := range assignsBefore {
		beforeIDs[m.ID] = true
	}

	// Step 3: buyer sends a buy request with generous budget. Use Apply (not
	// Replay) to preserve the derivative in state.
	buyMsg := h.sendMessage(h.buyer,
		buyPayload("Go channel patterns", 15000),
		[]string{exchange.TagBuy},
		nil,
	)
	buyExchangeMsg := exchange.FromStoreRecord(mustGetStoreRecord(t, h, buyMsg.ID))
	eng.State().Apply(buyExchangeMsg)

	if err := eng.DispatchForTest(buyExchangeMsg); err != nil {
		t.Fatalf("DispatchForTest(buy): %v", err)
	}

	// Verify a match was sent.
	matchMsgs, err := h.st.ListMessages(h.cfID, 0, store.MessageFilter{
		Tags: []string{exchange.TagMatch},
	})
	if err != nil {
		t.Fatalf("ListMessages(TagMatch): %v", err)
	}
	if len(matchMsgs) == 0 {
		t.Fatal("expected a match message, found none")
	}

	// Verify no NEW buyer-exclusive warm compression assign was emitted after the buy.
	assignsAfter, err := h.st.ListMessages(h.cfID, 0, store.MessageFilter{
		Tags: []string{exchange.TagAssign},
	})
	if err != nil {
		t.Fatalf("ListMessages after buy: %v", err)
	}

	for i := range assignsAfter {
		msg := &assignsAfter[i]
		if beforeIDs[msg.ID] {
			continue // existed before buy
		}
		var p struct {
			TaskType        string `json:"task_type"`
			EntryID         string `json:"entry_id"`
			ExclusiveSender string `json:"exclusive_sender"`
		}
		if err := json.Unmarshal(msg.Payload, &p); err != nil {
			continue
		}
		if p.TaskType == "compress" && p.EntryID == putMsg.ID && p.ExclusiveSender == h.buyer.PublicKeyHex() {
			t.Errorf("unexpected warm compression assign emitted for buyer when derivative already exists (assign_id=%s)", msg.ID[:16])
		}
	}
}
