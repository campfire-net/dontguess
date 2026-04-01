package exchange_test

// Cold compression integration tests.
//
// These verify Engine.PostOpenCompressionAssign — the public entry point that
// the medium loop's PostAssign callback targets. The method posts a non-exclusive
// compression assign at ColdCompressionBountyPct (20%) bounty.

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/campfire-net/campfire/pkg/store"

	"github.com/3dl-dev/dontguess/pkg/exchange"
)

// TestPostOpenCompressionAssign_PostsColdAssign verifies that
// PostOpenCompressionAssign posts a non-exclusive compression assign at 20%
// bounty for an accepted inventory entry.
func TestPostOpenCompressionAssign_PostsColdAssign(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	eng := h.newEngine()

	const tokenCost int64 = 10000
	const wantBounty int64 = tokenCost * exchange.ColdCompressionBountyPct / 100 // 2000

	contentHash := "sha256:" + fmt.Sprintf("%064x", 99)

	// Put and accept an entry.
	putMsg := h.sendMessage(h.seller,
		putPayload("PostgreSQL optimization guide", contentHash, "analysis", tokenCost, 20000),
		[]string{exchange.TagPut, "exchange:content-type:analysis", "exchange:domain:database"},
		nil,
	)
	if err := eng.AutoAcceptPut(putMsg.ID, 7000, time.Now().Add(72*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut: %v", err)
	}

	// AutoAcceptPut fires a hot compression assign. Count existing assigns.
	preAssigns := listAssignMessages(t, h)

	// Post a cold compression assign via the public method.
	if err := eng.PostOpenCompressionAssign(putMsg.ID); err != nil {
		t.Fatalf("PostOpenCompressionAssign: %v", err)
	}

	// Verify the cold assign message appeared.
	postAssigns := listAssignMessages(t, h)
	coldAssigns := postAssigns[len(preAssigns):]
	if len(coldAssigns) != 1 {
		t.Fatalf("expected 1 new assign message, got %d", len(coldAssigns))
	}

	var ap struct {
		EntryID         string `json:"entry_id"`
		TaskType        string `json:"task_type"`
		Reward          int64  `json:"reward"`
		ExclusiveSender string `json:"exclusive_sender"`
	}
	if err := json.Unmarshal(coldAssigns[0].Payload, &ap); err != nil {
		t.Fatalf("parsing cold assign payload: %v", err)
	}

	if ap.EntryID != putMsg.ID {
		t.Errorf("entry_id = %s, want %s", ap.EntryID, putMsg.ID)
	}
	if ap.TaskType != "compress" {
		t.Errorf("task_type = %s, want compress", ap.TaskType)
	}
	if ap.Reward != wantBounty {
		t.Errorf("reward = %d, want %d (%d%% of %d)", ap.Reward, wantBounty, exchange.ColdCompressionBountyPct, tokenCost)
	}
	if ap.ExclusiveSender != "" {
		t.Errorf("exclusive_sender = %q, want empty (cold assigns are open)", ap.ExclusiveSender)
	}
}

// TestPostOpenCompressionAssign_BountyTiers verifies the three compression
// tiers have distinct bounty rates: hot (50%) > warm (30%) > cold (20%).
func TestPostOpenCompressionAssign_BountyTiers(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	eng := h.newEngine()

	const tokenCost int64 = 20000
	contentHash := "sha256:" + fmt.Sprintf("%064x", 101)

	putMsg := h.sendMessage(h.seller,
		putPayload("Go concurrency patterns", contentHash, "code", tokenCost, 32768),
		[]string{exchange.TagPut, "exchange:content-type:code", "exchange:domain:go"},
		nil,
	)
	if err := eng.AutoAcceptPut(putMsg.ID, 14000, time.Now().Add(72*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut: %v", err)
	}

	// Hot assign was already posted. Post a cold assign.
	if err := eng.PostOpenCompressionAssign(putMsg.ID); err != nil {
		t.Fatalf("PostOpenCompressionAssign: %v", err)
	}

	// Read all assigns.
	allAssigns := listAssignMessages(t, h)
	if len(allAssigns) < 2 {
		t.Fatalf("expected at least 2 assigns (hot + cold), got %d", len(allAssigns))
	}

	// Parse bounties.
	bounties := make(map[int64]bool)
	for _, msg := range allAssigns {
		var ap struct {
			Reward int64 `json:"reward"`
		}
		if err := json.Unmarshal(msg.Payload, &ap); err != nil {
			continue
		}
		bounties[ap.Reward] = true
	}

	hotBounty := tokenCost * exchange.HotCompressionBountyPct / 100   // 10000
	coldBounty := tokenCost * exchange.ColdCompressionBountyPct / 100 // 4000

	if !bounties[hotBounty] {
		t.Errorf("missing hot bounty %d in assigns", hotBounty)
	}
	if !bounties[coldBounty] {
		t.Errorf("missing cold bounty %d in assigns", coldBounty)
	}
	if coldBounty >= hotBounty {
		t.Errorf("cold bounty %d should be less than hot bounty %d", coldBounty, hotBounty)
	}
}

// TestPostOpenCompressionAssign_EntryNotFound verifies that
// PostOpenCompressionAssign returns an error for a non-existent entry.
func TestPostOpenCompressionAssign_EntryNotFound(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	eng := h.newEngine()

	err := eng.PostOpenCompressionAssign("nonexistent-entry-id")
	if err == nil {
		t.Fatal("expected error for non-existent entry, got nil")
	}
}

// --- helpers ---

func listAssignMessages(t *testing.T, h *testHarness) []store.MessageRecord {
	t.Helper()
	msgs, err := h.st.ListMessages(h.cfID, 0, store.MessageFilter{
		Tags: []string{exchange.TagAssign},
	})
	if err != nil {
		t.Fatalf("listing assign messages: %v", err)
	}
	return msgs
}
