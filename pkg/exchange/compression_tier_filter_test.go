package exchange_test

// Tests for compression_tier filtering in the matching engine (dontguess-291).
//
// The spec (docs/convention/core-operations.md) defines:
//   - put: compression_tier is "hot" | "warm" | "cold" | "" (unset)
//   - buy: compression_tier is an optional filter
//
// Semantics: when a buyer specifies compression_tier, only inventory entries
// with an exact tier match are candidates. Entries with unset tier ("") are
// excluded — they did not declare a tier and do not implicitly satisfy the filter.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/campfire-net/campfire/pkg/store"

	"github.com/campfire-net/dontguess/pkg/exchange"
)

// putPayloadWithTier builds a put payload including a compression_tier field.
func putPayloadWithTier(desc, contentType, tier string, tokenCost int64) []byte {
	content := []byte("cached inference result: " + desc + " padding to ensure non-empty content")
	contentB64 := base64.StdEncoding.EncodeToString(content)
	fields := map[string]any{
		"description":  desc,
		"content":      contentB64,
		"token_cost":   tokenCost,
		"content_type": contentType,
		"domains":      []string{"go", "testing"},
	}
	if tier != "" {
		fields["compression_tier"] = tier
	}
	p, _ := json.Marshal(fields)
	return p
}

// buyPayloadWithTier builds a buy payload including an optional compression_tier filter.
func buyPayloadWithTier(task string, budget int64, tier string) []byte {
	fields := map[string]any{
		"task":        task,
		"budget":      budget,
		"max_results": 5,
	}
	if tier != "" {
		fields["compression_tier"] = tier
	}
	p, _ := json.Marshal(fields)
	return p
}

// acceptPutAndReplay is a helper that accepts a put and replays state.
func acceptPutAndReplay(t *testing.T, h *testHarness, eng *exchange.Engine, putMsgID string, price int64) {
	t.Helper()
	if err := eng.AutoAcceptPut(putMsgID, price, time.Now().Add(72*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut %s: %v", putMsgID[:8], err)
	}
}

// waitForMatchCount polls until the store has at least wantCount exchange:match
// messages (in addition to preExisting), or the deadline passes.
// Returns all match messages found.
func waitForMatchCount(t *testing.T, h *testHarness, preExisting int, wantCount int, timeout time.Duration) []store.MessageRecord {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		msgs, err := h.st.ListMessages(h.cfID, 0, store.MessageFilter{
			Tags: []string{exchange.TagMatch},
		})
		if err != nil {
			t.Fatalf("listing match messages: %v", err)
		}
		if len(msgs) >= preExisting+wantCount {
			return msgs
		}
		time.Sleep(50 * time.Millisecond)
	}
	msgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{
		Tags: []string{exchange.TagMatch},
	})
	return msgs
}

// TestEngine_TierFilter_OnlyMatchingTierIsCandidate verifies that when a buyer
// specifies compression_tier="hot", only inventory entries with tier="hot" are
// returned as candidates. Entries with tier="cold" or no tier are excluded.
func TestEngine_TierFilter_OnlyMatchingTierIsCandidate(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	// Put three entries:
	//   - seller1: tier="hot"  (should match)
	//   - seller2: tier="cold" (should NOT match)
	//   - seller3: no tier     (should NOT match — unset tier ≠ any specific tier)

	hotMsg := h.sendMessage(h.seller,
		putPayloadWithTier("Go HTTP handler generator", "code", "hot", 8000),
		[]string{exchange.TagPut, "exchange:content-type:code"},
		nil,
	)

	seller2 := newTestAgent(t)
	coldMsg := h.sendMessage(seller2,
		putPayloadWithTier("Go HTTP handler generator", "code", "cold", 8000),
		[]string{exchange.TagPut, "exchange:content-type:code"},
		nil,
	)

	seller3 := newTestAgent(t)
	noTierMsg := h.sendMessage(seller3,
		putPayloadWithTier("Go HTTP handler generator", "code", "", 8000),
		[]string{exchange.TagPut, "exchange:content-type:code"},
		nil,
	)

	// Replay and accept all three puts.
	msgs, err := h.st.ListMessages(h.cfID, 0)
	if err != nil {
		t.Fatalf("listing messages: %v", err)
	}
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	acceptPutAndReplay(t, h, eng, hotMsg.ID, 5600)
	acceptPutAndReplay(t, h, eng, coldMsg.ID, 5600)
	acceptPutAndReplay(t, h, eng, noTierMsg.ID, 5600)

	inv := eng.State().Inventory()
	if len(inv) != 3 {
		t.Fatalf("expected 3 inventory entries, got %d", len(inv))
	}

	// Verify the hot entry's CompressionTier is recorded correctly.
	var hotEntry *exchange.InventoryEntry
	for _, e := range inv {
		if e.EntryID == hotMsg.ID {
			hotEntry = e
		}
	}
	if hotEntry == nil {
		t.Fatal("hot entry not found in inventory")
	}
	if hotEntry.CompressionTier != "hot" {
		t.Errorf("hot entry CompressionTier = %q, want %q", hotEntry.CompressionTier, "hot")
	}

	// Count pre-existing match messages.
	preMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{
		Tags: []string{exchange.TagMatch},
	})
	preMatchCount := len(preMsgs)

	// Buyer sends buy with tier filter "hot".
	buyMsg := h.sendMessage(h.buyer,
		buyPayloadWithTier("Go HTTP handler", 20000, "hot"),
		[]string{exchange.TagBuy},
		nil,
	)

	// Apply buy to state.
	buyRec, err := h.st.GetMessage(buyMsg.ID)
	if err != nil {
		t.Fatalf("getting buy message: %v", err)
	}
	eng.State().Apply(exchange.FromStoreRecord(buyRec))

	// Start engine to dispatch the buy.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() { _ = eng.Start(ctx) }()

	matchMsgs := waitForMatchCount(t, h, preMatchCount, 1, 2*time.Second)
	cancel()

	newMatches := matchMsgs[preMatchCount:]
	if len(newMatches) == 0 {
		t.Fatal("no match message emitted — expected at least one (for the hot entry)")
	}

	// Parse the match payload.
	matchMsg := newMatches[len(newMatches)-1]
	var matchPayload struct {
		Results []struct {
			EntryID string `json:"entry_id"`
		} `json:"results"`
	}
	if err := json.Unmarshal(matchMsg.Payload, &matchPayload); err != nil {
		t.Fatalf("parsing match payload: %v", err)
	}

	// Assert: only the hot entry appears.
	resultIDs := make(map[string]bool, len(matchPayload.Results))
	for _, r := range matchPayload.Results {
		resultIDs[r.EntryID] = true
	}

	if !resultIDs[hotMsg.ID] {
		t.Errorf("hot entry %s missing from match results %v", hotMsg.ID[:8], resultIDs)
	}
	if resultIDs[coldMsg.ID] {
		t.Errorf("cold entry %s incorrectly appeared in hot-filter results", coldMsg.ID[:8])
	}
	if resultIDs[noTierMsg.ID] {
		t.Errorf("no-tier entry %s incorrectly appeared in hot-filter results", noTierMsg.ID[:8])
	}
}

// TestEngine_TierFilter_Absent_MatchesAll verifies that a buy with no
// compression_tier filter returns candidates regardless of their tier.
func TestEngine_TierFilter_Absent_MatchesAll(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	// Put entries at different tiers.
	hotMsg := h.sendMessage(h.seller,
		putPayloadWithTier("SQL query optimizer", "code", "hot", 6000),
		[]string{exchange.TagPut, "exchange:content-type:code"},
		nil,
	)

	seller2 := newTestAgent(t)
	warmMsg := h.sendMessage(seller2,
		putPayloadWithTier("SQL query optimizer", "code", "warm", 6000),
		[]string{exchange.TagPut, "exchange:content-type:code"},
		nil,
	)

	seller3 := newTestAgent(t)
	noTierMsg := h.sendMessage(seller3,
		putPayloadWithTier("SQL query optimizer", "code", "", 6000),
		[]string{exchange.TagPut, "exchange:content-type:code"},
		nil,
	)

	msgs, err := h.st.ListMessages(h.cfID, 0)
	if err != nil {
		t.Fatalf("listing messages: %v", err)
	}
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	acceptPutAndReplay(t, h, eng, hotMsg.ID, 4200)
	acceptPutAndReplay(t, h, eng, warmMsg.ID, 4200)
	acceptPutAndReplay(t, h, eng, noTierMsg.ID, 4200)

	inv := eng.State().Inventory()
	if len(inv) != 3 {
		t.Fatalf("expected 3 inventory entries, got %d", len(inv))
	}

	preMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{
		Tags: []string{exchange.TagMatch},
	})
	preMatchCount := len(preMsgs)

	// Buy with NO tier filter.
	buyMsg := h.sendMessage(h.buyer,
		buyPayloadWithTier("SQL query optimizer", 20000, ""),
		[]string{exchange.TagBuy},
		nil,
	)

	buyRec, err := h.st.GetMessage(buyMsg.ID)
	if err != nil {
		t.Fatalf("getting buy message: %v", err)
	}
	eng.State().Apply(exchange.FromStoreRecord(buyRec))

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() { _ = eng.Start(ctx) }()

	matchMsgs := waitForMatchCount(t, h, preMatchCount, 1, 2*time.Second)
	cancel()

	newMatches := matchMsgs[preMatchCount:]
	if len(newMatches) == 0 {
		t.Fatal("no match message emitted")
	}

	matchMsg := newMatches[len(newMatches)-1]
	var matchPayload struct {
		Results []struct {
			EntryID string `json:"entry_id"`
		} `json:"results"`
	}
	if err := json.Unmarshal(matchMsg.Payload, &matchPayload); err != nil {
		t.Fatalf("parsing match payload: %v", err)
	}

	// All three entries must be candidates (no tier filter means all pass).
	resultIDs := make(map[string]bool, len(matchPayload.Results))
	for _, r := range matchPayload.Results {
		resultIDs[r.EntryID] = true
	}

	for _, id := range []string{hotMsg.ID, warmMsg.ID, noTierMsg.ID} {
		if !resultIDs[id] {
			t.Errorf("entry %s missing from no-filter results — all tiers should be candidates", id[:8])
		}
	}
}

// TestEngine_TierFilter_InvalidTierOnPut_DroppedToEmpty verifies that an
// unrecognised compression_tier value on a put is silently dropped (entry
// stored with tier="" and is therefore NOT excluded by absence of tier filter,
// and IS excluded when buyer requests a specific tier).
func TestEngine_TierFilter_InvalidTierOnPut_DroppedToEmpty(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	// Put with an invalid tier value — should be sanitised to "".
	badTierPayload := func() []byte {
		content := []byte("inference result content bytes for bad tier test")
		contentB64 := base64.StdEncoding.EncodeToString(content)
		p, _ := json.Marshal(map[string]any{
			"description":      "test entry with bad tier",
			"content":          contentB64,
			"token_cost":       5000,
			"content_type":     "code",
			"domains":          []string{"go"},
			"compression_tier": "turbo", // unknown value — should be dropped
		})
		return p
	}

	putMsg := h.sendMessage(h.seller,
		badTierPayload(),
		[]string{exchange.TagPut, "exchange:content-type:code"},
		nil,
	)

	msgs, err := h.st.ListMessages(h.cfID, 0)
	if err != nil {
		t.Fatalf("listing messages: %v", err)
	}
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	acceptPutAndReplay(t, h, eng, putMsg.ID, 3500)

	inv := eng.State().Inventory()
	if len(inv) != 1 {
		t.Fatalf("expected 1 inventory entry, got %d", len(inv))
	}

	entry := inv[0]
	if entry.CompressionTier != "" {
		t.Errorf("invalid tier 'turbo' should be dropped to \"\", got %q", entry.CompressionTier)
	}
}
