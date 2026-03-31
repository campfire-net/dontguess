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
