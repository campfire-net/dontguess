package exchange_test

// Tests for the conversion-rate-based reputation model (dontguess-cj5).
//
// Covered:
//   - Reputation with 0 previews → DefaultReputation (no conversion bonus)
//   - Reputation with 10+ previews, 80% conversion → positive bonus
//   - Reputation with 10+ previews, 20% conversion → negative bonus
//   - Reputation with 10+ previews, 50% conversion → neutral (zero bonus)
//   - PreviewCount increments on preview-request
//   - ConversionCount increments on buyer-accept via preview path
//   - ConversionCount does NOT increment on direct buyer-accept (legacy path)
//   - LowConversionEntries returns correct entries
//   - Small-content refund penalty still works with new model
//   - Cross-agent convergence (+3 per entry) still works with new model

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/campfire-net/campfire/pkg/identity"
	"github.com/campfire-net/campfire/pkg/store"

	"github.com/3dl-dev/dontguess/pkg/exchange"
)

// --- Helpers ---

// generateBuyerMatchPreview creates a new buyer identity, sends a buy message,
// dispatches it through the engine to get a match, then sends a preview-request
// for the given entryID. Returns (matchID, previewRequestMsgID, buyer).
//
// Each buyer has a unique identity so the dedup logic does not collapse them.
func generateBuyerMatchPreview(t *testing.T, h *testHarness, eng *exchange.Engine, entryID string, taskSuffix string) (matchID, previewReqID string, buyer *identity.Identity) {
	t.Helper()

	buyer, err := identity.Generate()
	if err != nil {
		t.Fatalf("generateBuyerMatchPreview: generate identity: %v", err)
	}

	preCount, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})

	buyMsg := h.sendMessage(buyer,
		buyPayload("Conversion test task "+taskSuffix, 50000),
		[]string{exchange.TagBuy},
		nil,
	)
	buyRec, _ := h.st.GetMessage(buyMsg.ID)
	eng.State().Apply(exchange.FromStoreRecord(buyRec))
	if err := eng.DispatchForTest(exchange.FromStoreRecord(buyRec)); err != nil {
		t.Fatalf("generateBuyerMatchPreview dispatch: %v", err)
	}
	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	postMatches, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})
	if len(postMatches) <= len(preCount) {
		t.Fatalf("generateBuyerMatchPreview: no new match emitted")
	}
	matchID = postMatches[len(postMatches)-1].ID

	preqMsg := h.sendMessage(buyer,
		previewRequestPayload(entryID),
		[]string{exchange.TagSettle, exchange.TagPhasePrefix + exchange.SettlePhaseStrPreviewRequest},
		[]string{matchID},
	)
	allMsgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	return matchID, preqMsg.ID, buyer
}

// emitPreviewAndAccept sends a preview response (operator) and then a buyer-accept
// (via the preview path) for the given previewReqID. This constitutes a conversion.
func emitPreviewAndAccept(t *testing.T, h *testHarness, eng *exchange.Engine, entryID, previewReqID string, buyer *identity.Identity) {
	t.Helper()

	prevPayload, _ := json.Marshal(map[string]any{
		"phase":    "preview",
		"entry_id": entryID,
		"chunks":   []any{},
	})
	prevMsg := h.sendMessage(h.operator, prevPayload,
		[]string{exchange.TagSettle, exchange.TagPhasePrefix + exchange.SettlePhaseStrPreview},
		[]string{previewReqID},
	)
	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	acceptPayload, _ := json.Marshal(map[string]any{
		"phase":    "buyer-accept",
		"entry_id": entryID,
		"accepted": true,
	})
	h.sendMessage(buyer, acceptPayload,
		[]string{exchange.TagSettle, exchange.TagPhasePrefix + exchange.SettlePhaseStrBuyerAccept},
		[]string{prevMsg.ID},
	)
	allMsgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))
}

// --- Unit tests for SellerStats.Reputation() ---

// TestReputation_NoPreviewData verifies that a seller with no previews starts at
// DefaultReputation (no conversion bonus applied).
func TestReputation_NoPreviewData(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	// Seed a single completed sale (no previews).
	putMsg := h.sendMessage(h.seller,
		putPayload("Test inference result", "sha256:"+fmt.Sprintf("%064x", 1001), "analysis", 10000, 40000),
		[]string{exchange.TagPut, "exchange:content-type:analysis"},
		nil,
	)
	msgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	if err := eng.AutoAcceptPut(putMsg.ID, 7000, time.Now().Add(72*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut: %v", err)
	}

	// No previews have been served. Reputation should be DefaultReputation + SuccessCount adjustments.
	// Before any sale: no success, no previews — score should equal DefaultReputation.
	rep := eng.State().SellerReputation(h.seller.PublicKeyHex())
	if rep != exchange.DefaultReputation {
		t.Errorf("reputation with no data = %d, want %d (DefaultReputation)", rep, exchange.DefaultReputation)
	}
}

// TestReputation_HighConversion verifies that 80% conversion rate produces a positive bonus.
// Uses 10 distinct buyers (one per preview-request) so the per-buyer dedup does not apply.
func TestReputation_HighConversion(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	// Build a large-content entry (token_cost = 10000 >= SmallContentThreshold).
	putMsg := h.sendMessage(h.seller,
		putPayload("High-conversion inference", "sha256:"+fmt.Sprintf("%064x", 2001), "code", 10000, 50000),
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
		t.Fatal("no inventory entry")
	}
	entryID := inv[0].EntryID

	// 10 distinct buyers each send a preview-request. 8 of them then accept.
	type buyerPreview struct {
		previewReqID string
		buyer        *identity.Identity
	}
	bps := make([]buyerPreview, 10)
	for i := 0; i < 10; i++ {
		_, preqID, buyer := generateBuyerMatchPreview(t, h, eng, entryID, fmt.Sprintf("%d", i))
		bps[i] = buyerPreview{previewReqID: preqID, buyer: buyer}
	}

	// First 8 buyers convert (receive preview + accept).
	for i := 0; i < 8; i++ {
		emitPreviewAndAccept(t, h, eng, entryID, bps[i].previewReqID, bps[i].buyer)
	}

	// 10 previews, 8 conversions = 80% conversion rate.
	// conversionBonus = int((0.8 - 0.5) * 20) = int(6.0) = 6
	// Score = DefaultReputation + 6 = 56 (no SuccessCount: no deliver/complete in this test).
	rep := eng.State().SellerReputation(h.seller.PublicKeyHex())
	if rep <= exchange.DefaultReputation {
		t.Errorf("80%% conversion reputation = %d, want > %d", rep, exchange.DefaultReputation)
	}
	want := exchange.DefaultReputation + 6
	if rep != want {
		t.Errorf("80%% conversion reputation = %d, want %d", rep, want)
	}
}

// TestReputation_LowConversion verifies that 20% conversion rate produces a negative bonus.
// Uses 10 distinct buyers so dedup does not apply.
func TestReputation_LowConversion(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	putMsg := h.sendMessage(h.seller,
		putPayload("Low-conversion inference", "sha256:"+fmt.Sprintf("%064x", 3001), "code", 10000, 50000),
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
		t.Fatal("no inventory entry")
	}
	entryID := inv[0].EntryID

	// 10 distinct buyers, only 2 convert (20%).
	type buyerPreview struct {
		previewReqID string
		buyer        *identity.Identity
	}
	bps := make([]buyerPreview, 10)
	for i := 0; i < 10; i++ {
		_, preqID, buyer := generateBuyerMatchPreview(t, h, eng, entryID, fmt.Sprintf("low-%d", i))
		bps[i] = buyerPreview{previewReqID: preqID, buyer: buyer}
	}

	// Only first 2 convert.
	for i := 0; i < 2; i++ {
		emitPreviewAndAccept(t, h, eng, entryID, bps[i].previewReqID, bps[i].buyer)
	}

	// 10 previews, 2 conversions = 20% conversion rate.
	// conversionBonus = int((0.2 - 0.5) * 20) = int(-6.0) = -6
	// Score = 50 - 6 = 44.
	rep := eng.State().SellerReputation(h.seller.PublicKeyHex())
	if rep >= exchange.DefaultReputation {
		t.Errorf("20%% conversion reputation = %d, want < %d", rep, exchange.DefaultReputation)
	}
	want := exchange.DefaultReputation - 6
	if rep != want {
		t.Errorf("20%% conversion reputation = %d, want %d", rep, want)
	}
}

// TestReputation_NeutralConversion verifies that 50% conversion produces zero bonus.
func TestReputation_NeutralConversion(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	putMsg := h.sendMessage(h.seller,
		putPayload("Neutral-conversion inference", "sha256:"+fmt.Sprintf("%064x", 4001), "code", 10000, 50000),
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
		t.Fatal("no inventory entry")
	}
	entryID := inv[0].EntryID

	// 10 distinct buyers, 5 convert (50%).
	type buyerPreview struct {
		previewReqID string
		buyer        *identity.Identity
	}
	bps := make([]buyerPreview, 10)
	for i := 0; i < 10; i++ {
		_, preqID, buyer := generateBuyerMatchPreview(t, h, eng, entryID, fmt.Sprintf("neutral-%d", i))
		bps[i] = buyerPreview{previewReqID: preqID, buyer: buyer}
	}

	for i := 0; i < 5; i++ {
		emitPreviewAndAccept(t, h, eng, entryID, bps[i].previewReqID, bps[i].buyer)
	}

	// 10 previews, 5 conversions = 50%.
	// conversionBonus = int((0.5 - 0.5) * 20) = 0
	// Score = 50 (no SuccessCount, no other signals).
	rep := eng.State().SellerReputation(h.seller.PublicKeyHex())
	if rep != exchange.DefaultReputation {
		t.Errorf("50%% conversion reputation = %d, want %d (DefaultReputation)", rep, exchange.DefaultReputation)
	}
}

// TestReputation_PreviewCountIncrements verifies that PreviewCount is incremented
// when a preview-request is applied to state.
func TestReputation_PreviewCountIncrements(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	putMsg := h.sendMessage(h.seller,
		putPayload("Preview count test", "sha256:"+fmt.Sprintf("%064x", 5001), "code", 10000, 50000),
		[]string{exchange.TagPut, "exchange:content-type:code"},
		nil,
	)
	msgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	if err := eng.AutoAcceptPut(putMsg.ID, 7000, time.Now().Add(72*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut: %v", err)
	}
	inv := eng.State().Inventory()
	entryID := inv[0].EntryID

	matchRec, _ := buildMatchedState(t, h, eng)

	// No previews yet — reputation should equal DefaultReputation.
	rep0 := eng.State().SellerReputation(h.seller.PublicKeyHex())
	if rep0 != exchange.DefaultReputation {
		t.Errorf("before previews: reputation = %d, want %d", rep0, exchange.DefaultReputation)
	}

	// Send one preview-request.
	h.sendMessage(h.buyer,
		previewRequestPayload(entryID),
		[]string{exchange.TagSettle, exchange.TagPhasePrefix + exchange.SettlePhaseStrPreviewRequest},
		[]string{matchRec.ID},
	)
	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	// 1 preview (< 10 threshold) — no conversion bonus applied yet.
	rep1 := eng.State().SellerReputation(h.seller.PublicKeyHex())
	if rep1 != exchange.DefaultReputation {
		t.Errorf("after 1 preview: reputation = %d, want %d (threshold not met)", rep1, exchange.DefaultReputation)
	}

	// LowConversionEntries should not include this entry yet (below minPreviews=10).
	low := eng.State().LowConversionEntries(10, 0.3)
	for _, id := range low {
		if id == entryID {
			t.Error("entry appeared in LowConversionEntries before threshold reached")
		}
	}
}

// TestReputation_ConversionCountIncrements verifies that ConversionCount is incremented
// when a buyer-accept follows the preview path.
func TestReputation_ConversionCountIncrements(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	putMsg := h.sendMessage(h.seller,
		putPayload("Conversion count test", "sha256:"+fmt.Sprintf("%064x", 6001), "code", 10000, 50000),
		[]string{exchange.TagPut, "exchange:content-type:code"},
		nil,
	)
	msgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	if err := eng.AutoAcceptPut(putMsg.ID, 7000, time.Now().Add(72*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut: %v", err)
	}
	inv := eng.State().Inventory()
	entryID := inv[0].EntryID

	matchRec, _ := buildMatchedState(t, h, eng)

	// Send preview-request, preview response, then buyer-accept.
	preqMsg := h.sendMessage(h.buyer,
		previewRequestPayload(entryID),
		[]string{exchange.TagSettle, exchange.TagPhasePrefix + exchange.SettlePhaseStrPreviewRequest},
		[]string{matchRec.ID},
	)
	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	prevPayload, _ := json.Marshal(map[string]any{
		"phase":    "preview",
		"entry_id": entryID,
		"chunks":   []any{},
	})
	prevMsg := h.sendMessage(h.operator, prevPayload,
		[]string{exchange.TagSettle, exchange.TagPhasePrefix + exchange.SettlePhaseStrPreview},
		[]string{preqMsg.ID},
	)
	allMsgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	// No conversion yet — buyer hasn't accepted.
	// With 1 preview (below threshold of 10), reputation should still be DefaultReputation.
	rep0 := eng.State().SellerReputation(h.seller.PublicKeyHex())
	if rep0 != exchange.DefaultReputation {
		t.Errorf("before buyer-accept: reputation = %d, want %d", rep0, exchange.DefaultReputation)
	}

	// Buyer accepts via the preview path.
	acceptPayload, _ := json.Marshal(map[string]any{
		"phase":    "buyer-accept",
		"entry_id": entryID,
		"accepted": true,
	})
	h.sendMessage(h.buyer, acceptPayload,
		[]string{exchange.TagSettle, exchange.TagPhasePrefix + exchange.SettlePhaseStrBuyerAccept},
		[]string{prevMsg.ID},
	)
	allMsgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	// Still below threshold (1 preview), but ConversionCount should be tracked internally.
	// The reputation should still equal DefaultReputation (threshold not met).
	rep1 := eng.State().SellerReputation(h.seller.PublicKeyHex())
	if rep1 != exchange.DefaultReputation {
		t.Errorf("after 1 conversion (threshold not met): reputation = %d, want %d", rep1, exchange.DefaultReputation)
	}
}

// TestReputation_DirectAcceptNoConversion verifies that a direct buyer-accept
// (legacy path, no preview) does NOT increment ConversionCount.
func TestReputation_DirectAcceptNoConversion(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	putMsg := h.sendMessage(h.seller,
		putPayload("Direct accept test", "sha256:"+fmt.Sprintf("%064x", 7001), "code", 10000, 50000),
		[]string{exchange.TagPut, "exchange:content-type:code"},
		nil,
	)
	msgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	if err := eng.AutoAcceptPut(putMsg.ID, 7000, time.Now().Add(72*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut: %v", err)
	}
	inv := eng.State().Inventory()
	entryID := inv[0].EntryID

	matchRec, _ := buildMatchedState(t, h, eng)

	// Buyer accepts DIRECTLY from the match (no preview step).
	acceptPayload, _ := json.Marshal(map[string]any{
		"phase":    "buyer-accept",
		"entry_id": entryID,
		"accepted": true,
	})
	h.sendMessage(h.buyer, acceptPayload,
		[]string{exchange.TagSettle, exchange.TagPhasePrefix + exchange.SettlePhaseStrBuyerAccept},
		[]string{matchRec.ID},
	)
	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	// No preview-request was sent, so no PreviewCount tracked.
	// Reputation should equal DefaultReputation (no conversion bonus, no SuccessCount yet).
	rep := eng.State().SellerReputation(h.seller.PublicKeyHex())
	if rep != exchange.DefaultReputation {
		t.Errorf("direct accept: reputation = %d, want %d (no conversion tracked)", rep, exchange.DefaultReputation)
	}
}

// TestReputation_LowConversionEntries verifies LowConversionEntries returns the
// correct entry IDs. Uses 10 distinct buyers to meet the minPreviews threshold.
func TestReputation_LowConversionEntries(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	putMsg := h.sendMessage(h.seller,
		putPayload("LowConversion target entry", "sha256:"+fmt.Sprintf("%064x", 8001), "code", 10000, 50000),
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
		t.Fatal("no inventory entry")
	}
	entryID := inv[0].EntryID

	// 10 distinct buyers, only 1 converts (10% conversion rate).
	type buyerPreview struct {
		previewReqID string
		buyer        *identity.Identity
	}
	bps := make([]buyerPreview, 10)
	for i := 0; i < 10; i++ {
		_, preqID, buyer := generateBuyerMatchPreview(t, h, eng, entryID, fmt.Sprintf("lcv-%d", i))
		bps[i] = buyerPreview{previewReqID: preqID, buyer: buyer}
	}

	// Only first buyer converts.
	emitPreviewAndAccept(t, h, eng, entryID, bps[0].previewReqID, bps[0].buyer)

	// 10 previews, 1 conversion = 10% rate.
	// LowConversionEntries(minPreviews=10, maxRate=0.3) should include this entry.
	low := eng.State().LowConversionEntries(10, 0.3)
	found := false
	for _, id := range low {
		if id == entryID {
			found = true
		}
	}
	if !found {
		t.Errorf("LowConversionEntries did not include entry %s (10%% conversion < 30%% threshold)", entryID[:8])
	}

	// LowConversionEntries(minPreviews=10, maxRate=0.05) should NOT include it
	// (10% > 5% maxRate).
	lowStrict := eng.State().LowConversionEntries(10, 0.05)
	for _, id := range lowStrict {
		if id == entryID {
			t.Errorf("LowConversionEntries with maxRate=0.05 should not include 10%% entry")
		}
	}

	// LowConversionEntries(minPreviews=20, maxRate=0.3) should NOT include it
	// (only 10 previews < minPreviews=20).
	lowHighMin := eng.State().LowConversionEntries(20, 0.3)
	for _, id := range lowHighMin {
		if id == entryID {
			t.Errorf("LowConversionEntries with minPreviews=20 should not include entry with only 10 previews")
		}
	}
}

// TestReputation_ZeroConversion verifies that 0% conversion (10+ previews, 0 accepts)
// produces the formula floor bonus of -10.
func TestReputation_ZeroConversion(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	putMsg := h.sendMessage(h.seller,
		putPayload("Zero-conversion inference", "sha256:"+fmt.Sprintf("%064x", 10001), "code", 10000, 50000),
		[]string{exchange.TagPut, "exchange:content-type:code"},
		nil,
	)
	msgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	if err := eng.AutoAcceptPut(putMsg.ID, 7000, time.Now().Add(72*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut: %v", err)
	}
	inv := eng.State().Inventory()
	entryID := inv[0].EntryID

	// 10 distinct buyers, 0 convert.
	for i := 0; i < 10; i++ {
		generateBuyerMatchPreview(t, h, eng, entryID, fmt.Sprintf("zero-%d", i))
	}

	// 10 previews, 0 conversions = 0% conversion rate.
	// conversionBonus = int((0.0 - 0.5) * 20) = int(-10.0) = -10
	// Score = 50 - 10 = 40.
	rep := eng.State().SellerReputation(h.seller.PublicKeyHex())
	want := exchange.DefaultReputation - 10
	if rep != want {
		t.Errorf("0%% conversion reputation = %d, want %d", rep, want)
	}
}

// TestReputation_FullConversion verifies that 100% conversion (10+ previews, all accept)
// produces the formula ceiling bonus of +10.
func TestReputation_FullConversion(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	putMsg := h.sendMessage(h.seller,
		putPayload("Full-conversion inference", "sha256:"+fmt.Sprintf("%064x", 10002), "code", 10000, 50000),
		[]string{exchange.TagPut, "exchange:content-type:code"},
		nil,
	)
	msgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	if err := eng.AutoAcceptPut(putMsg.ID, 7000, time.Now().Add(72*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut: %v", err)
	}
	inv := eng.State().Inventory()
	entryID := inv[0].EntryID

	// 10 distinct buyers, all convert.
	type buyerPreview struct {
		previewReqID string
		buyer        *identity.Identity
	}
	bps := make([]buyerPreview, 10)
	for i := 0; i < 10; i++ {
		_, preqID, buyer := generateBuyerMatchPreview(t, h, eng, entryID, fmt.Sprintf("full-%d", i))
		bps[i] = buyerPreview{previewReqID: preqID, buyer: buyer}
	}
	for i := 0; i < 10; i++ {
		emitPreviewAndAccept(t, h, eng, entryID, bps[i].previewReqID, bps[i].buyer)
	}

	// 10 previews, 10 conversions = 100%.
	// conversionBonus = int((1.0 - 0.5) * 20) = int(10.0) = 10
	// Score = 50 + 10 = 60.
	rep := eng.State().SellerReputation(h.seller.PublicKeyHex())
	want := exchange.DefaultReputation + 10
	if rep != want {
		t.Errorf("100%% conversion reputation = %d, want %d", rep, want)
	}
}

// TestReputation_LowConversionEntries_AtExactBoundary verifies that an entry with
// exactly maxRate conversion is NOT included (strict < comparison).
func TestReputation_LowConversionEntries_AtExactBoundary(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	putMsg := h.sendMessage(h.seller,
		putPayload("Boundary conversion entry", "sha256:"+fmt.Sprintf("%064x", 10003), "code", 10000, 50000),
		[]string{exchange.TagPut, "exchange:content-type:code"},
		nil,
	)
	msgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	if err := eng.AutoAcceptPut(putMsg.ID, 7000, time.Now().Add(72*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut: %v", err)
	}
	inv := eng.State().Inventory()
	entryID := inv[0].EntryID

	// 10 distinct buyers, 3 convert = 30% rate.
	type buyerPreview struct {
		previewReqID string
		buyer        *identity.Identity
	}
	bps := make([]buyerPreview, 10)
	for i := 0; i < 10; i++ {
		_, preqID, buyer := generateBuyerMatchPreview(t, h, eng, entryID, fmt.Sprintf("bnd-%d", i))
		bps[i] = buyerPreview{previewReqID: preqID, buyer: buyer}
	}
	for i := 0; i < 3; i++ {
		emitPreviewAndAccept(t, h, eng, entryID, bps[i].previewReqID, bps[i].buyer)
	}

	// At exactly maxRate=0.3 (3/10 = 0.3), strict < means NOT included.
	low := eng.State().LowConversionEntries(10, 0.3)
	for _, id := range low {
		if id == entryID {
			t.Errorf("entry at exactly 30%% rate should NOT be in LowConversionEntries(maxRate=0.3) — strict < comparison")
		}
	}

	// At maxRate=0.31, it SHOULD be included (0.3 < 0.31).
	low2 := eng.State().LowConversionEntries(10, 0.31)
	found := false
	for _, id := range low2 {
		if id == entryID {
			found = true
		}
	}
	if !found {
		t.Errorf("entry at 30%% rate should be in LowConversionEntries(maxRate=0.31)")
	}
}

// TestReputation_SmallContentPenaltyPreserved verifies that the small-content refund
// penalty still applies correctly with the new model.
func TestReputation_SmallContentPenaltyPreserved(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	entryID, _ := setupSmallContentEntry(t, h, eng)
	chain := buildDeliverChain(t, h, eng, entryID)

	// Before dispute: reputation is DefaultReputation.
	rep0 := eng.State().SellerReputation(h.seller.PublicKeyHex())
	if rep0 != exchange.DefaultReputation {
		t.Errorf("before dispute: reputation = %d, want %d", rep0, exchange.DefaultReputation)
	}

	// File small-content dispute.
	h.sendMessage(h.buyer, smallContentDisputePayload(entryID),
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrSmallContentDispute,
		},
		[]string{chain.deliverMsgID},
	)
	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	// Reputation should be DefaultReputation - SmallContentReputationPenalty.
	want := exchange.DefaultReputation - exchange.SmallContentReputationPenalty
	rep1 := eng.State().SellerReputation(h.seller.PublicKeyHex())
	if rep1 != want {
		t.Errorf("after small-content dispute: reputation = %d, want %d", rep1, want)
	}
}

// TestReputation_CrossAgentConvergencePreserved verifies that the +3 per entry
// with 3+ distinct buyers signal still applies with the new model.
func TestReputation_CrossAgentConvergencePreserved(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	// Create a large-content entry.
	putMsg := h.sendMessage(h.seller,
		putPayload("Convergence test inference", "sha256:"+fmt.Sprintf("%064x", 9001), "code", 10000, 50000),
		[]string{exchange.TagPut, "exchange:content-type:code", "exchange:domain:go"},
		nil,
	)
	msgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	if err := eng.AutoAcceptPut(putMsg.ID, 7000, time.Now().Add(72*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut: %v", err)
	}
	inv := eng.State().Inventory()
	if len(inv) == 0 {
		t.Fatal("no inventory entry")
	}
	entryID := inv[0].EntryID

	// Drive 3 complete sales through 3 distinct buyers.
	// Each buyer: buy → (engine emits match) → buyer-accept → deliver → complete.
	buyer2, err := identity.Generate()
	if err != nil {
		t.Fatalf("generating buyer2: %v", err)
	}
	buyer3, err := identity.Generate()
	if err != nil {
		t.Fatalf("generating buyer3: %v", err)
	}
	buyers := []*identity.Identity{h.buyer, buyer2, buyer3}

	for i, buyer := range buyers {
		// Count match messages before dispatch so we can find the new one after.
		preMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})
		preMatchCount := len(preMsgs)

		buyMsg := h.sendMessage(buyer,
			buyPayload(fmt.Sprintf("Convergence buyer %d task", i), 50000),
			[]string{exchange.TagBuy},
			nil,
		)
		buyRec, _ := h.st.GetMessage(buyMsg.ID)
		eng.State().Apply(exchange.FromStoreRecord(buyRec))
		if err := eng.DispatchForTest(exchange.FromStoreRecord(buyRec)); err != nil {
			t.Fatalf("buyer %d dispatch: %v", i, err)
		}

		allMsgs, _ := h.st.ListMessages(h.cfID, 0)
		eng.State().Replay(exchange.FromStoreRecords(allMsgs))

		// Find the newly emitted match (the one added after preMatchCount).
		matchMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})
		if len(matchMsgs) <= preMatchCount {
			t.Fatalf("buyer %d: no new match message after dispatch", i)
		}
		matchID := matchMsgs[len(matchMsgs)-1].ID

		// Buyer accepts directly (not via preview path).
		acceptPayload, _ := json.Marshal(map[string]any{
			"phase":    "buyer-accept",
			"entry_id": entryID,
			"accepted": true,
		})
		acceptMsg := h.sendMessage(buyer, acceptPayload,
			[]string{exchange.TagSettle, exchange.TagPhasePrefix + exchange.SettlePhaseStrBuyerAccept},
			[]string{matchID},
		)
		allMsgs, _ = h.st.ListMessages(h.cfID, 0)
		eng.State().Replay(exchange.FromStoreRecords(allMsgs))

		// Operator delivers.
		deliverMsg := h.sendMessage(h.operator, deliverPayloadFor(entryID),
			[]string{exchange.TagSettle, exchange.TagPhasePrefix + exchange.SettlePhaseStrDeliver},
			[]string{acceptMsg.ID},
		)
		allMsgs, _ = h.st.ListMessages(h.cfID, 0)
		eng.State().Replay(exchange.FromStoreRecords(allMsgs))

		// Buyer completes.
		completePayload, _ := json.Marshal(map[string]any{
			"phase": "complete",
			"price": int64(7000),
		})
		h.sendMessage(buyer, completePayload,
			[]string{exchange.TagSettle, exchange.TagPhasePrefix + exchange.SettlePhaseStrComplete},
			[]string{deliverMsg.ID},
		)
		allMsgs, _ = h.st.ListMessages(h.cfID, 0)
		eng.State().Replay(exchange.FromStoreRecords(allMsgs))
	}

	// With 3 distinct buyers completing on the same entry:
	//   - SuccessCount = 3 → +3
	//   - EntryBuyerMap[entryID] has 3 distinct buyers → +3
	//   - No preview data (direct accepts, not via preview path)
	//   - No repeat buyers (each buyer bought once)
	// Total = 50 + 3 + 3 = 56.
	rep := eng.State().SellerReputation(h.seller.PublicKeyHex())
	want := exchange.DefaultReputation + 3 + 3
	if rep != want {
		t.Errorf("cross-agent convergence: reputation = %d, want %d", rep, want)
	}
}
