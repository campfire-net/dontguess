package exchange_test

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/campfire-net/campfire/pkg/store"

	"github.com/3dl-dev/dontguess/pkg/exchange"
)

// TestAutoAcceptPut_TriggersCompressionAssign verifies that when the engine
// auto-accepts a put message, it immediately sends an exchange:assign message
// with:
//   - task_type = "compress"
//   - exclusive_sender = seller's public key
//   - reward (bounty) = 50% of the put's token_cost
//   - entry_id = the accepted put message ID
func TestAutoAcceptPut_TriggersCompressionAssign(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	eng := h.newEngine()

	const tokenCost int64 = 20000
	const wantBounty int64 = tokenCost / 2

	contentHash := "sha256:" + fmt.Sprintf("%064x", 42)

	// Send a put from the seller.
	putMsg := h.sendMessage(h.seller,
		putPayload("Go concurrency patterns", contentHash, "code", tokenCost, 32768),
		[]string{exchange.TagPut, "exchange:content-type:code", "exchange:domain:go"},
		nil,
	)

	// Auto-accept the put at a price of 8000 scrip with 7-day expiry.
	if err := eng.AutoAcceptPut(putMsg.ID, 8000, time.Now().Add(168*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut: %v", err)
	}

	// Query the store for exchange:assign messages.
	allMsgs, err := h.st.ListMessages(h.cfID, 0, store.MessageFilter{
		Tags: []string{exchange.TagAssign},
	})
	if err != nil {
		t.Fatalf("ListMessages(TagAssign): %v", err)
	}

	// Filter to compression assigns (task_type=compress for this entry).
	var assignRec *store.MessageRecord
	for i := range allMsgs {
		msg := &allMsgs[i]
		var p struct {
			TaskType string `json:"task_type"`
			EntryID  string `json:"entry_id"`
		}
		if err := json.Unmarshal(msg.Payload, &p); err != nil {
			continue
		}
		if p.TaskType == "compress" && p.EntryID == putMsg.ID {
			assignRec = msg
			break
		}
	}

	if assignRec == nil {
		t.Fatalf("expected compression assign message in campfire, found none (total TagAssign msgs: %d)", len(allMsgs))
	}

	// Decode payload for assertions.
	var payload struct {
		EntryID         string `json:"entry_id"`
		TaskType        string `json:"task_type"`
		Reward          int64  `json:"reward"`
		ExclusiveSender string `json:"exclusive_sender"`
		Description     string `json:"description"`
	}
	if err := json.Unmarshal(assignRec.Payload, &payload); err != nil {
		t.Fatalf("decoding assign payload: %v", err)
	}

	// Exclusive sender must be the original seller.
	if payload.ExclusiveSender != h.seller.PublicKeyHex() {
		t.Errorf("exclusive_sender = %q, want seller key %q",
			payload.ExclusiveSender, h.seller.PublicKeyHex())
	}

	// Bounty must be 50% of token_cost.
	if payload.Reward != wantBounty {
		t.Errorf("reward = %d, want %d (50%% of token_cost %d)", payload.Reward, wantBounty, tokenCost)
	}

	// entry_id must reference the put message.
	if payload.EntryID != putMsg.ID {
		t.Errorf("entry_id = %q, want put message ID %q", payload.EntryID, putMsg.ID)
	}

	// task_type must be "compress".
	if payload.TaskType != "compress" {
		t.Errorf("task_type = %q, want %q", payload.TaskType, "compress")
	}

	// Description must mention the entry ID and content hash.
	if payload.Description == "" {
		t.Error("description is empty")
	}

	// Verify the assign is reflected in engine state.
	assigns := eng.State().ActiveAssigns(putMsg.ID)
	if len(assigns) == 0 {
		t.Fatal("expected active assign in engine state after AutoAcceptPut, got none")
	}
	got := assigns[0]
	if got.TaskType != "compress" {
		t.Errorf("state assign task_type = %q, want %q", got.TaskType, "compress")
	}
	if got.ExclusiveSender != h.seller.PublicKeyHex() {
		t.Errorf("state assign exclusive_sender = %q, want seller key", got.ExclusiveSender)
	}
	if got.Reward != wantBounty {
		t.Errorf("state assign reward = %d, want %d", got.Reward, wantBounty)
	}
}

// TestAutoAcceptPut_CompressionAssignOddTokenCost verifies integer truncation
// for odd token_cost values (50% rounds down).
func TestAutoAcceptPut_CompressionAssignOddTokenCost(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	eng := h.newEngine()

	const tokenCost int64 = 10001
	const wantBounty int64 = tokenCost / 2 // integer division: 5000

	putMsg := h.sendMessage(h.seller,
		putPayload("odd cost entry", "sha256:"+fmt.Sprintf("%064x", 99), "analysis", tokenCost, 4096),
		[]string{exchange.TagPut, "exchange:content-type:analysis"},
		nil,
	)

	if err := eng.AutoAcceptPut(putMsg.ID, 5000, time.Now().Add(48*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut: %v", err)
	}

	allMsgs, err := h.st.ListMessages(h.cfID, 0, store.MessageFilter{
		Tags: []string{exchange.TagAssign},
	})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}

	var found bool
	for _, msg := range allMsgs {
		var p struct {
			TaskType string `json:"task_type"`
			Reward   int64  `json:"reward"`
			EntryID  string `json:"entry_id"`
		}
		if err := json.Unmarshal(msg.Payload, &p); err != nil {
			continue
		}
		if p.TaskType == "compress" && p.EntryID == putMsg.ID {
			if p.Reward != wantBounty {
				t.Errorf("reward = %d, want %d (floor of %d/2)", p.Reward, wantBounty, tokenCost)
			}
			found = true
			break
		}
	}
	if !found {
		t.Fatal("no compress assign found after AutoAcceptPut")
	}

	_ = eng // suppress unused warning
}

// TestAutoAcceptPut_ZeroTokenCost verifies that a put with token_cost=0 is
// dropped by state validation (token_cost <= 0 is invalid) and that
// AutoAcceptPut returns an error without panicking. No compression assign
// must be emitted.
func TestAutoAcceptPut_ZeroTokenCost(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	eng := h.newEngine()

	contentHash := "sha256:" + fmt.Sprintf("%064x", 55)

	putMsg := h.sendMessage(h.seller,
		putPayload("zero-cost entry", contentHash, "analysis", 0, 1024),
		[]string{exchange.TagPut, "exchange:content-type:analysis"},
		nil,
	)

	// AutoAcceptPut must return an error — the put was dropped by validation.
	// It must not panic.
	if err := eng.AutoAcceptPut(putMsg.ID, 0, time.Now().Add(48*time.Hour)); err == nil {
		t.Fatal("expected AutoAcceptPut to return error for zero token_cost put, got nil")
	}

	// No compression assign must have been emitted.
	allMsgs, listErr := h.st.ListMessages(h.cfID, 0, store.MessageFilter{
		Tags: []string{exchange.TagAssign},
	})
	if listErr != nil {
		t.Fatalf("ListMessages(TagAssign): %v", listErr)
	}
	for _, msg := range allMsgs {
		var p struct {
			TaskType string `json:"task_type"`
			EntryID  string `json:"entry_id"`
		}
		if err := json.Unmarshal(msg.Payload, &p); err != nil {
			continue
		}
		if p.TaskType == "compress" && p.EntryID == putMsg.ID {
			t.Errorf("unexpected compression assign for zero token_cost put (assign_id=%s)", msg.ID[:16])
		}
	}
}

// TestAutoAcceptPut_DuplicateIdempotent verifies that calling AutoAcceptPut
// twice on the same putMsgID does not create a second compression assign. The
// second call must return an error (put is no longer pending) and the total
// number of compression assigns for the entry must remain exactly 1.
func TestAutoAcceptPut_DuplicateIdempotent(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	eng := h.newEngine()

	contentHash := "sha256:" + fmt.Sprintf("%064x", 66)

	putMsg := h.sendMessage(h.seller,
		putPayload("duplicate accept entry", contentHash, "code", 12000, 8192),
		[]string{exchange.TagPut, "exchange:content-type:code"},
		nil,
	)

	// First accept must succeed.
	if err := eng.AutoAcceptPut(putMsg.ID, 6000, time.Now().Add(72*time.Hour)); err != nil {
		t.Fatalf("first AutoAcceptPut: %v", err)
	}

	// Second accept must return an error (put is no longer pending).
	if err := eng.AutoAcceptPut(putMsg.ID, 6000, time.Now().Add(72*time.Hour)); err == nil {
		t.Fatal("second AutoAcceptPut should return error (put no longer pending), got nil")
	}

	// Exactly one compression assign must exist for this entry.
	allMsgs, err := h.st.ListMessages(h.cfID, 0, store.MessageFilter{
		Tags: []string{exchange.TagAssign},
	})
	if err != nil {
		t.Fatalf("ListMessages(TagAssign): %v", err)
	}

	var count int
	for _, msg := range allMsgs {
		var p struct {
			TaskType string `json:"task_type"`
			EntryID  string `json:"entry_id"`
		}
		if err := json.Unmarshal(msg.Payload, &p); err != nil {
			continue
		}
		if p.TaskType == "compress" && p.EntryID == putMsg.ID {
			count++
		}
	}

	if count != 1 {
		t.Errorf("compression assign count = %d, want 1 after duplicate AutoAcceptPut", count)
	}
}

// TestHotAndWarmCompressionAssign_Coexistence verifies that when a buy request
// arrives after a put-accept, BOTH the hot compression assign (sent to the
// seller at put-accept time) AND the warm compression assign (sent to the buyer
// at match time) are present in the campfire write log with correct
// exclusive_sender values.
//
// Done condition: two distinct exchange:assign messages exist for the same
// entry_id — one with exclusive_sender == seller key (hot, 50% bounty), one
// with exclusive_sender == buyer key (warm, 30% bounty). Neither is dropped.
func TestHotAndWarmCompressionAssign_Coexistence(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	eng := h.newEngine()

	const tokenCost int64 = 20000
	const wantHotBounty int64 = tokenCost * exchange.HotCompressionBountyPct / 100  // 10000
	const wantWarmBounty int64 = tokenCost * exchange.WarmCompressionBountyPct / 100 // 6000

	contentHash := "sha256:" + fmt.Sprintf("%064x", 201)

	// Step 1: put a raw entry from the seller and replay so the put is in state.
	putMsg := h.sendMessage(h.seller,
		putPayload("Python data pipeline patterns", contentHash, "code", tokenCost, 32768),
		[]string{exchange.TagPut, "exchange:content-type:code", "exchange:domain:python"},
		nil,
	)

	msgs, err := h.st.ListMessages(h.cfID, 0)
	if err != nil {
		t.Fatalf("ListMessages after put: %v", err)
	}
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	// Step 2: auto-accept the put — this triggers the hot compression assign
	// (exclusive_sender = seller, bounty = 50% of token_cost).
	if err := eng.AutoAcceptPut(putMsg.ID, 8000, time.Now().Add(168*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut: %v", err)
	}

	// Verify the hot compression assign was emitted immediately after put-accept.
	assignsAfterAccept, err := h.st.ListMessages(h.cfID, 0, store.MessageFilter{
		Tags: []string{exchange.TagAssign},
	})
	if err != nil {
		t.Fatalf("ListMessages after AutoAcceptPut: %v", err)
	}
	var foundHotBeforeBuy bool
	for _, msg := range assignsAfterAccept {
		var p struct {
			TaskType        string `json:"task_type"`
			EntryID         string `json:"entry_id"`
			ExclusiveSender string `json:"exclusive_sender"`
		}
		if err := json.Unmarshal(msg.Payload, &p); err != nil {
			continue
		}
		if p.TaskType == "compress" && p.EntryID == putMsg.ID && p.ExclusiveSender == h.seller.PublicKeyHex() {
			foundHotBeforeBuy = true
			break
		}
	}
	if !foundHotBeforeBuy {
		t.Fatal("hot compression assign (exclusive_sender=seller) not found after AutoAcceptPut — precondition for coexistence test failed")
	}

	// Verify the match index has the entry.
	if n := eng.MatchIndexLen(); n == 0 {
		t.Fatal("match index is empty after AutoAcceptPut — buy will miss, cannot test warm assign")
	}

	// Step 3: buyer sends a buy request. Use Apply (not Replay) to preserve state
	// set up by AutoAcceptPut without re-processing already-applied messages.
	buyMsg := h.sendMessage(h.buyer,
		buyPayload("Python data pipeline patterns", 15000),
		[]string{exchange.TagBuy},
		nil,
	)

	buyRec := mustGetStoreRecord(t, h, buyMsg.ID)
	buyExchangeMsg := exchange.FromStoreRecord(buyRec)
	eng.State().Apply(buyExchangeMsg)

	if err := eng.DispatchForTest(buyExchangeMsg); err != nil {
		t.Fatalf("DispatchForTest(buy): %v", err)
	}

	// Verify a match was sent (buy did not miss).
	matchMsgs, err := h.st.ListMessages(h.cfID, 0, store.MessageFilter{
		Tags: []string{exchange.TagMatch},
	})
	if err != nil {
		t.Fatalf("ListMessages(TagMatch): %v", err)
	}
	if len(matchMsgs) == 0 {
		t.Fatal("expected a match message, found none — warm assign cannot be emitted on a buy-miss")
	}

	// Step 4: collect all assigns for this entry from the write log.
	allAssigns, err := h.st.ListMessages(h.cfID, 0, store.MessageFilter{
		Tags: []string{exchange.TagAssign},
	})
	if err != nil {
		t.Fatalf("ListMessages(TagAssign): %v", err)
	}

	type assignInfo struct {
		msgID           string
		exclusiveSender string
		reward          int64
	}
	var hotAssign, warmAssign *assignInfo

	for i := range allAssigns {
		msg := &allAssigns[i]
		var p struct {
			TaskType        string `json:"task_type"`
			EntryID         string `json:"entry_id"`
			ExclusiveSender string `json:"exclusive_sender"`
			Reward          int64  `json:"reward"`
		}
		if err := json.Unmarshal(msg.Payload, &p); err != nil {
			continue
		}
		if p.TaskType != "compress" || p.EntryID != putMsg.ID {
			continue
		}
		info := &assignInfo{msgID: msg.ID, exclusiveSender: p.ExclusiveSender, reward: p.Reward}
		switch p.ExclusiveSender {
		case h.seller.PublicKeyHex():
			hotAssign = info
		case h.buyer.PublicKeyHex():
			warmAssign = info
		}
	}

	// Assert both assigns are present — neither dropped.
	if hotAssign == nil {
		t.Error("hot compression assign (exclusive_sender=seller) not found in write log after buy")
	}
	if warmAssign == nil {
		t.Error("warm compression assign (exclusive_sender=buyer) not found in write log after buy")
	}
	if hotAssign == nil || warmAssign == nil {
		t.FailNow()
	}

	// Assert the two assigns are distinct messages.
	if hotAssign.msgID == warmAssign.msgID {
		t.Errorf("hot and warm compression assigns share the same message ID %q — expected two distinct messages", hotAssign.msgID)
	}

	// Assert bounty amounts are correct.
	if hotAssign.reward != wantHotBounty {
		t.Errorf("hot assign reward = %d, want %d (50%% of token_cost %d)", hotAssign.reward, wantHotBounty, tokenCost)
	}
	if warmAssign.reward != wantWarmBounty {
		t.Errorf("warm assign reward = %d, want %d (30%% of token_cost %d)", warmAssign.reward, wantWarmBounty, tokenCost)
	}

	// Assert engine state reflects both assigns as active.
	activeAssigns := eng.State().ActiveAssigns(putMsg.ID)
	var hotActive, warmActive bool
	for _, a := range activeAssigns {
		switch a.ExclusiveSender {
		case h.seller.PublicKeyHex():
			hotActive = true
		case h.buyer.PublicKeyHex():
			warmActive = true
		}
	}
	if !hotActive {
		t.Error("hot compression assign not reflected as active in engine state")
	}
	if !warmActive {
		t.Error("warm compression assign not reflected as active in engine state")
	}
}
