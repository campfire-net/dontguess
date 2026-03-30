package exchange_test

// Tests for the Layer 0 conversion-rate exclusion gate (dontguess-5iz).
//
// The gate excludes inventory entries from findCandidates (and therefore from
// match results) when:
//   - preview count >= Layer0MinPreviews (10), AND
//   - conversion rate < Layer0MaxConversionRate (5%)
//
// The gate is reversible: if an entry's rate improves above 5%, it re-appears
// on the next buy request without any explicit reinstatement.
//
// Covered:
//   - Entry with <5% conversion after 10+ previews is excluded from buy results
//   - Entry with >5% conversion after 10+ previews is included
//   - Entry with <10 previews is NOT excluded (insufficient data)
//   - After conversion rate improves above 5%, entry reappears (reversibility)
//   - Layer0MinPreviews and Layer0MaxConversionRate constants have correct values

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/campfire-net/campfire/pkg/store"

	"github.com/3dl-dev/dontguess/pkg/exchange"
)

// TestLayer0_Constants verifies the Layer 0 gate constants have the correct values.
func TestLayer0_Constants(t *testing.T) {
	t.Parallel()
	if exchange.Layer0MinPreviews != 10 {
		t.Errorf("Layer0MinPreviews = %d, want 10", exchange.Layer0MinPreviews)
	}
	const wantRate = 0.05
	if exchange.Layer0MaxConversionRate != wantRate {
		t.Errorf("Layer0MaxConversionRate = %f, want %f", exchange.Layer0MaxConversionRate, wantRate)
	}
}

// TestLayer0_LowConversionEntryExcludedFromBuyResults verifies that an entry
// with <5% conversion after 10+ previews does not appear in buy match results.
//
// Setup: one entry receives 10 previews but only 0 conversions (0% rate).
// A buy dispatch targeting that entry's description must not return it.
func TestLayer0_LowConversionEntryExcludedFromBuyResults(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	// Seed the target entry.
	putMsg := h.sendMessage(h.seller,
		putPayload("Go HTTP handler unit test patterns", "sha256:"+fmt.Sprintf("%064x", 5001), "code", 10000, 50000),
		[]string{exchange.TagPut, "exchange:content-type:code"},
		nil,
	)
	msgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))
	if err := eng.AutoAcceptPut(putMsg.ID, 7000, time.Now().Add(72*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut: %v", err)
	}

	inv := eng.State().Inventory()
	if len(inv) == 0 {
		t.Fatal("no inventory entry after put accept")
	}
	entryID := inv[0].EntryID

	// Generate 10 previews, 0 conversions (buyers never accept after preview).
	for i := 0; i < 10; i++ {
		_, previewReqID, _ := generateBuyerMatchPreview(t, h, eng, entryID, fmt.Sprintf("layer0-low-noconv-%d", i))
		// Emit a preview response but do NOT emit a buyer-accept.
		prevPayload, _ := json.Marshal(map[string]any{
			"phase":    "preview",
			"entry_id": entryID,
			"chunks":   []any{},
		})
		h.sendMessage(h.operator, prevPayload,
			[]string{exchange.TagSettle, exchange.TagPhasePrefix + exchange.SettlePhaseStrPreview},
			[]string{previewReqID},
		)
		allMsgs, _ := h.st.ListMessages(h.cfID, 0)
		eng.State().Replay(exchange.FromStoreRecords(allMsgs))
	}

	// Verify the gate: LowConversionEntries should list this entry.
	low := eng.State().LowConversionEntries(exchange.Layer0MinPreviews, exchange.Layer0MaxConversionRate)
	found := false
	for _, id := range low {
		if id == entryID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("precondition: entry %s not in LowConversionEntries after 10 previews, 0 conversions", entryID[:8])
	}

	// Now dispatch a buy targeting the entry's description and verify it's absent.
	preMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})

	h.sendMessage(h.buyer,
		buyPayload("Go HTTP handler unit test patterns", 20000),
		[]string{exchange.TagBuy},
		nil,
	)
	buyRec, _ := h.st.GetMessage(h.cfID)
	_ = buyRec

	// Collect all messages and dispatch the buy via the engine event loop.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
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

	// If no match at all was emitted, the excluded entry is absent — pass.
	if len(matchMsgs) <= len(preMsgs) {
		return
	}

	// Parse the match results and assert the excluded entry is absent.
	matchMsg := matchMsgs[len(matchMsgs)-1]
	var mp struct {
		Results []struct {
			EntryID string `json:"entry_id"`
		} `json:"results"`
	}
	if err := json.Unmarshal(matchMsg.Payload, &mp); err != nil {
		t.Fatalf("parsing match payload: %v", err)
	}
	for _, r := range mp.Results {
		if r.EntryID == entryID {
			t.Errorf("entry %s with 0%% conversion appeared in buy results — Layer 0 gate failed", entryID[:8])
		}
	}
}

// TestLayer0_HighConversionEntryIncluded verifies that an entry with >5%
// conversion after 10+ previews is NOT excluded from buy results.
//
// Setup: one entry with 10 previews and 2 conversions (20% rate). A buy
// dispatch targeting that entry must include it in the match results.
func TestLayer0_HighConversionEntryIncluded(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	putMsg := h.sendMessage(h.seller,
		putPayload("Python data analysis with pandas dataframes", "sha256:"+fmt.Sprintf("%064x", 5002), "code", 10000, 50000),
		[]string{exchange.TagPut, "exchange:content-type:code"},
		nil,
	)
	msgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))
	if err := eng.AutoAcceptPut(putMsg.ID, 7000, time.Now().Add(72*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut: %v", err)
	}

	inv := eng.State().Inventory()
	if len(inv) == 0 {
		t.Fatal("no inventory entry after put accept")
	}
	entryID := inv[0].EntryID

	// 8 preview-only (no conversion), then 2 conversions = 2/10 = 20% rate.
	for i := 0; i < 8; i++ {
		_, previewReqID, _ := generateBuyerMatchPreview(t, h, eng, entryID, fmt.Sprintf("layer0-high-noconv-%d", i))
		prevPayload, _ := json.Marshal(map[string]any{
			"phase":    "preview",
			"entry_id": entryID,
			"chunks":   []any{},
		})
		h.sendMessage(h.operator, prevPayload,
			[]string{exchange.TagSettle, exchange.TagPhasePrefix + exchange.SettlePhaseStrPreview},
			[]string{previewReqID},
		)
		allMsgs, _ := h.st.ListMessages(h.cfID, 0)
		eng.State().Replay(exchange.FromStoreRecords(allMsgs))
	}
	for i := 0; i < 2; i++ {
		_, previewReqID, buyer := generateBuyerMatchPreview(t, h, eng, entryID, fmt.Sprintf("layer0-high-conv-%d", i))
		emitPreviewAndAccept(t, h, eng, entryID, previewReqID, buyer)
	}

	// Verify: 20% > 5%, so entry should NOT be in low conversion list.
	low := eng.State().LowConversionEntries(exchange.Layer0MinPreviews, exchange.Layer0MaxConversionRate)
	for _, id := range low {
		if id == entryID {
			t.Fatalf("precondition: entry %s with 20%% conversion should not be in LowConversionEntries", entryID[:8])
		}
	}

	// Run a buy dispatch; the entry must appear in results.
	preMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})
	h.sendMessage(h.buyer,
		buyPayload("Python data analysis with pandas dataframes", 20000),
		[]string{exchange.TagBuy},
		nil,
	)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
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
		t.Fatal("engine emitted no match — entry with 20% conversion should have been included")
	}

	matchMsg := matchMsgs[len(matchMsgs)-1]
	var mp struct {
		Results []struct {
			EntryID string `json:"entry_id"`
		} `json:"results"`
	}
	if err := json.Unmarshal(matchMsg.Payload, &mp); err != nil {
		t.Fatalf("parsing match payload: %v", err)
	}
	found := false
	for _, r := range mp.Results {
		if r.EntryID == entryID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("entry %s with 20%% conversion should appear in buy results but was absent", entryID[:8])
	}
}

// TestLayer0_InsufficientPreviewsNotExcluded verifies that an entry with fewer
// than Layer0MinPreviews (10) is not excluded, even with 0% conversion rate.
//
// An entry needs sufficient preview data before the gate kicks in.
func TestLayer0_InsufficientPreviewsNotExcluded(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	putMsg := h.sendMessage(h.seller,
		putPayload("Rust async runtime tokio integration tests", "sha256:"+fmt.Sprintf("%064x", 5003), "code", 10000, 50000),
		[]string{exchange.TagPut, "exchange:content-type:code"},
		nil,
	)
	msgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))
	if err := eng.AutoAcceptPut(putMsg.ID, 7000, time.Now().Add(72*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut: %v", err)
	}

	inv := eng.State().Inventory()
	if len(inv) == 0 {
		t.Fatal("no inventory entry after put accept")
	}
	entryID := inv[0].EntryID

	// Only 5 previews, 0 conversions — below the 10-preview threshold.
	for i := 0; i < 5; i++ {
		_, previewReqID, _ := generateBuyerMatchPreview(t, h, eng, entryID, fmt.Sprintf("layer0-insuf-%d", i))
		prevPayload, _ := json.Marshal(map[string]any{
			"phase":    "preview",
			"entry_id": entryID,
			"chunks":   []any{},
		})
		h.sendMessage(h.operator, prevPayload,
			[]string{exchange.TagSettle, exchange.TagPhasePrefix + exchange.SettlePhaseStrPreview},
			[]string{previewReqID},
		)
		allMsgs, _ := h.st.ListMessages(h.cfID, 0)
		eng.State().Replay(exchange.FromStoreRecords(allMsgs))
	}

	// Verify: entry should NOT be in LowConversionEntries (only 5 previews < 10).
	low := eng.State().LowConversionEntries(exchange.Layer0MinPreviews, exchange.Layer0MaxConversionRate)
	for _, id := range low {
		if id == entryID {
			t.Fatalf("entry %s with only 5 previews should not be excluded (below minPreviews threshold)", entryID[:8])
		}
	}

	// Run a buy dispatch; the entry must still appear in results.
	preMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})
	h.sendMessage(h.buyer,
		buyPayload("Rust async runtime tokio integration tests", 20000),
		[]string{exchange.TagBuy},
		nil,
	)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
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
		t.Fatal("engine emitted no match — entry with insufficient preview data should not be excluded")
	}

	matchMsg := matchMsgs[len(matchMsgs)-1]
	var mp struct {
		Results []struct {
			EntryID string `json:"entry_id"`
		} `json:"results"`
	}
	if err := json.Unmarshal(matchMsg.Payload, &mp); err != nil {
		t.Fatalf("parsing match payload: %v", err)
	}
	found := false
	for _, r := range mp.Results {
		if r.EntryID == entryID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("entry %s with 5 previews should appear in buy results (gate not triggered) but was absent", entryID[:8])
	}
}

// TestLayer0_ReversibilityAfterConversionImproves verifies that an entry which
// was excluded by the Layer 0 gate (0% conversion after 10+ previews) re-appears
// in buy results once its conversion rate rises above the threshold.
//
// The reversal is automatic: LowConversionEntries() is computed fresh on every
// findCandidates call with no permanent blacklist. No explicit reinstatement is needed.
//
// The realistic scenario that produces a conversion after exclusion: a buyer
// received a preview response BEFORE the entry hit the 10-preview threshold
// (when it was still included in match results), then submits the buyer-accept
// AFTER the entry is excluded. The buyer-accept registers a conversion against
// the stale preview message, raising the rate above 5% and reinstating the entry.
//
// Sequence:
//  1. Seed entry; give buyer A a pending preview-request (1 preview, 0 conversions).
//     Entry NOT excluded (below minPreviews=10 threshold).
//  2. Add 9 more previews without conversions → 10 previews, 0 conversions → excluded.
//  3. Buyer A accepts their stale preview → conversion registered → 10 previews,
//     1 conversion = 10% rate → no longer excluded.
//  4. A new buy dispatch returns the reinstated entry.
func TestLayer0_ReversibilityAfterConversionImproves(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	putMsg := h.sendMessage(h.seller,
		putPayload("TypeScript React hooks unit testing best practices", "sha256:"+fmt.Sprintf("%064x", 5004), "code", 10000, 50000),
		[]string{exchange.TagPut, "exchange:content-type:code"},
		nil,
	)
	msgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))
	if err := eng.AutoAcceptPut(putMsg.ID, 7000, time.Now().Add(72*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut: %v", err)
	}

	inv := eng.State().Inventory()
	if len(inv) == 0 {
		t.Fatal("no inventory entry after put accept")
	}
	entryID := inv[0].EntryID

	// Phase 1: Buyer A gets a preview-request AND a preview response (pending accept).
	// The entry has 1 preview, 0 conversions — well below the 10-preview threshold.
	_, previewReqIDA, buyerA := generateBuyerMatchPreview(t, h, eng, entryID, "layer0-rev-buyerA")
	prevPayloadA, _ := json.Marshal(map[string]any{
		"phase":    "preview",
		"entry_id": entryID,
		"chunks":   []any{},
	})
	prevMsgA := h.sendMessage(h.operator, prevPayloadA,
		[]string{exchange.TagSettle, exchange.TagPhasePrefix + exchange.SettlePhaseStrPreview},
		[]string{previewReqIDA},
	)
	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))
	// Confirm: 1 preview, not excluded.
	lowAfterA := eng.State().LowConversionEntries(exchange.Layer0MinPreviews, exchange.Layer0MaxConversionRate)
	for _, id := range lowAfterA {
		if id == entryID {
			t.Fatal("precondition: entry should not be excluded with only 1 preview")
		}
	}

	// Phase 2: Add 9 more previews without conversions → 10 total, 0 conversions → excluded.
	for i := 0; i < 9; i++ {
		_, previewReqID, _ := generateBuyerMatchPreview(t, h, eng, entryID, fmt.Sprintf("layer0-rev-noconv-%d", i))
		prevPayload, _ := json.Marshal(map[string]any{
			"phase":    "preview",
			"entry_id": entryID,
			"chunks":   []any{},
		})
		h.sendMessage(h.operator, prevPayload,
			[]string{exchange.TagSettle, exchange.TagPhasePrefix + exchange.SettlePhaseStrPreview},
			[]string{previewReqID},
		)
		allMsgs, _ = h.st.ListMessages(h.cfID, 0)
		eng.State().Replay(exchange.FromStoreRecords(allMsgs))
	}

	// Verify entry is now excluded (10 previews, 0 conversions = 0% < 5%).
	low := eng.State().LowConversionEntries(exchange.Layer0MinPreviews, exchange.Layer0MaxConversionRate)
	found := false
	for _, id := range low {
		if id == entryID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("precondition: entry %s should be excluded after 10 previews, 0 conversions", entryID[:8])
	}

	// Phase 3: Buyer A (who had a preview response before exclusion) now accepts.
	// This registers a conversion: 10 previews, 1 conversion = 10% → no longer excluded.
	acceptPayload, _ := json.Marshal(map[string]any{
		"phase":    "buyer-accept",
		"entry_id": entryID,
		"accepted": true,
	})
	h.sendMessage(buyerA, acceptPayload,
		[]string{exchange.TagSettle, exchange.TagPhasePrefix + exchange.SettlePhaseStrBuyerAccept},
		[]string{prevMsgA.ID},
	)
	allMsgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	// Verify reversal: entry should no longer be in LowConversionEntries.
	lowAfter := eng.State().LowConversionEntries(exchange.Layer0MinPreviews, exchange.Layer0MaxConversionRate)
	for _, id := range lowAfter {
		if id == entryID {
			t.Fatalf("entry %s should not be in LowConversionEntries after stale accept raised rate to 10%%", entryID[:8])
		}
	}

	// Verify end-to-end: dispatch a buy and assert the entry reappears.
	preMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})
	h.sendMessage(h.buyer,
		buyPayload("TypeScript React hooks unit testing best practices", 20000),
		[]string{exchange.TagBuy},
		nil,
	)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
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
		t.Fatal("engine emitted no match — reinstated entry should appear in buy results")
	}

	matchMsg := matchMsgs[len(matchMsgs)-1]
	var mp struct {
		Results []struct {
			EntryID string `json:"entry_id"`
		} `json:"results"`
	}
	if err := json.Unmarshal(matchMsg.Payload, &mp); err != nil {
		t.Fatalf("parsing match payload: %v", err)
	}
	foundInResults := false
	for _, r := range mp.Results {
		if r.EntryID == entryID {
			foundInResults = true
			break
		}
	}
	if !foundInResults {
		t.Errorf("entry %s should reappear in buy results after stale accept raised rate to 10%% but was absent", entryID[:8])
	}
}
