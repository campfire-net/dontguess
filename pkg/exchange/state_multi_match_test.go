package exchange_test

// Tests for multi-result match handling and buyer-accept entry selection.
//
// Bug dontguess-7g0: applyMatch only tracked Results[0].EntryID in matchToEntry.
// If a match has multiple results and the buyer accepts result #1 (not #0),
// the settlement chain broke because matchToEntry[matchMsgID] pointed to result #0.
//
// Fix: matchToResults tracks all entry IDs; applySettleBuyerAccept validates the
// buyer's selected entry_id and updates matchToEntry to the selected entry.

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/3dl-dev/dontguess/pkg/exchange"
)

// buildMatchPayload builds a match message payload with the given entry IDs as results.
func buildMatchPayload(entryIDs []string) []byte {
	type result struct {
		EntryID string `json:"entry_id"`
	}
	results := make([]result, len(entryIDs))
	for i, eid := range entryIDs {
		results[i] = result{EntryID: eid}
	}
	p, _ := json.Marshal(map[string]any{
		"results":     results,
		"search_meta": map[string]any{"total_candidates": len(entryIDs)},
	})
	return p
}

// setupMultiMatchInventory seeds two inventory entries and returns their entry IDs.
func setupMultiMatchInventory(t *testing.T, h *testHarness, eng *exchange.Engine) (entryID0, entryID1 string) {
	t.Helper()

	// Seller puts two entries.
	putMsg0 := h.sendMessage(h.seller,
		putPayload("Go HTTP handler unit tests entry zero", "sha256:"+fmt.Sprintf("%064x", 101), "code", 10000, 18000),
		[]string{exchange.TagPut, "exchange:content-type:code"},
		nil,
	)
	putMsg1 := h.sendMessage(h.seller,
		putPayload("Go HTTP handler unit tests entry one", "sha256:"+fmt.Sprintf("%064x", 102), "code", 11000, 20000),
		[]string{exchange.TagPut, "exchange:content-type:code"},
		nil,
	)

	msgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	if err := eng.AutoAcceptPut(putMsg0.ID, 7000, time.Now().Add(48*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut entry0: %v", err)
	}
	if err := eng.AutoAcceptPut(putMsg1.ID, 7700, time.Now().Add(48*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut entry1: %v", err)
	}

	msgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	inv := eng.State().Inventory()
	if len(inv) < 2 {
		t.Fatalf("expected 2 inventory entries, got %d", len(inv))
	}

	// Determine which entry ID maps to which put message.
	for _, e := range inv {
		if e.PutMsgID == putMsg0.ID {
			entryID0 = e.EntryID
		} else if e.PutMsgID == putMsg1.ID {
			entryID1 = e.EntryID
		}
	}
	if entryID0 == "" || entryID1 == "" {
		t.Fatalf("could not find both inventory entries; entryID0=%q entryID1=%q", entryID0, entryID1)
	}
	return entryID0, entryID1
}

// TestState_MultiMatch_BuyerSelectsSecondResult verifies that when a match has
// multiple results, the buyer can accept result #1 (not #0) and the full
// settlement chain (buyer-accept → deliver → complete) resolves to that entry.
func TestState_MultiMatch_BuyerSelectsSecondResult(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	entryID0, entryID1 := setupMultiMatchInventory(t, h, eng)

	// Buyer places a buy order.
	buyMsg := h.sendMessage(h.buyer,
		buyPayload("Generate unit tests for a Go HTTP handler", 50000),
		[]string{exchange.TagBuy},
		nil,
	)

	buyRec, err := h.st.GetMessage(buyMsg.ID)
	if err != nil {
		t.Fatalf("getting buy message: %v", err)
	}
	eng.State().Apply(exchange.FromStoreRecord(buyRec))

	// Operator emits a match with two results: entryID0 first, entryID1 second.
	matchMsg := h.sendMessage(h.operator,
		buildMatchPayload([]string{entryID0, entryID1}),
		[]string{exchange.TagMatch},
		[]string{buyMsg.ID},
	)

	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	// Buyer accepts result #1 (entryID1, not the default entryID0).
	buyerAcceptMsg := h.sendMessage(h.buyer, buyerAcceptPayloadFor(entryID1),
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrBuyerAccept,
			exchange.TagVerdictPrefix + "accepted",
		},
		[]string{matchMsg.ID},
	)

	// Operator delivers.
	deliverMsg := h.sendMessage(h.operator, deliverPayloadFor(entryID1),
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrDeliver,
		},
		[]string{buyerAcceptMsg.ID},
	)

	// Buyer completes.
	h.sendMessage(h.buyer, completePayloadFor(entryID1, 11000),
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrComplete,
			exchange.TagVerdictPrefix + "accepted",
		},
		[]string{deliverMsg.ID},
	)

	allMsgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	// Seller reputation must have increased (completion recorded).
	rep := eng.State().SellerReputation(h.seller.PublicKeyHex())
	if rep <= exchange.DefaultReputation {
		t.Errorf("multi-match buyer selects result #1: seller reputation = %d, want > %d",
			rep, exchange.DefaultReputation)
	}

	// Price history must have exactly one record for entryID1.
	hist := eng.State().PriceHistory()
	if len(hist) != 1 {
		t.Fatalf("multi-match buyer selects result #1: price history len = %d, want 1", len(hist))
	}
	if hist[0].EntryID != entryID1 {
		t.Errorf("price history entry_id = %q, want %q (entryID1)", hist[0].EntryID, entryID1)
	}
}

// TestState_MultiMatch_InvalidEntryIDFallsBackToFirst verifies that when a
// buyer-accept payload contains an entry_id not present in the match results,
// the handler falls back to result #0 (the default).
func TestState_MultiMatch_InvalidEntryIDFallsBackToFirst(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	entryID0, entryID1 := setupMultiMatchInventory(t, h, eng)

	// Buyer places a buy order.
	buyMsg := h.sendMessage(h.buyer,
		buyPayload("Generate unit tests for a Go HTTP handler", 50000),
		[]string{exchange.TagBuy},
		nil,
	)

	buyRec, err := h.st.GetMessage(buyMsg.ID)
	if err != nil {
		t.Fatalf("getting buy message: %v", err)
	}
	eng.State().Apply(exchange.FromStoreRecord(buyRec))

	// Operator emits a match with two results.
	matchMsg := h.sendMessage(h.operator,
		buildMatchPayload([]string{entryID0, entryID1}),
		[]string{exchange.TagMatch},
		[]string{buyMsg.ID},
	)

	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	// Buyer sends buyer-accept with a bogus entry_id not in the match results.
	bogusID := fmt.Sprintf("%064x", 9999)
	buyerAcceptMsg := h.sendMessage(h.buyer, buyerAcceptPayloadFor(bogusID),
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrBuyerAccept,
			exchange.TagVerdictPrefix + "accepted",
		},
		[]string{matchMsg.ID},
	)

	// Operator delivers using entryID0 (fallback).
	deliverMsg := h.sendMessage(h.operator, deliverPayloadFor(entryID0),
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrDeliver,
		},
		[]string{buyerAcceptMsg.ID},
	)

	// Buyer completes with entryID0.
	h.sendMessage(h.buyer, completePayloadFor(entryID0, 10000),
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrComplete,
			exchange.TagVerdictPrefix + "accepted",
		},
		[]string{deliverMsg.ID},
	)

	allMsgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	// Settlement chain should have completed against entryID0 (the fallback).
	hist := eng.State().PriceHistory()
	if len(hist) != 1 {
		t.Fatalf("invalid entry_id fallback: price history len = %d, want 1", len(hist))
	}
	if hist[0].EntryID != entryID0 {
		t.Errorf("invalid entry_id fallback: price history entry_id = %q, want %q (entryID0)", hist[0].EntryID, entryID0)
	}
}

// TestState_SingleMatch_BackwardsCompat verifies that the existing single-result
// match case still works — buyer-accept with no explicit entry_id or with the
// only available entry_id both resolve correctly.
func TestState_SingleMatch_BackwardsCompat(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	// Seed one entry.
	putMsg := h.sendMessage(h.seller,
		putPayload("Go HTTP handler unit test generator single", "sha256:"+fmt.Sprintf("%064x", 200), "code", 12000, 20000),
		[]string{exchange.TagPut, "exchange:content-type:code"},
		nil,
	)

	msgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	if err := eng.AutoAcceptPut(putMsg.ID, 8400, time.Now().Add(48*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut: %v", err)
	}

	msgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	inv := eng.State().Inventory()
	if len(inv) == 0 {
		t.Fatal("expected at least one inventory entry")
	}
	entryID := inv[0].EntryID

	// Buyer places a buy order.
	buyMsg := h.sendMessage(h.buyer,
		buyPayload("Generate unit tests for a Go HTTP handler single", 50000),
		[]string{exchange.TagBuy},
		nil,
	)

	buyRec, err := h.st.GetMessage(buyMsg.ID)
	if err != nil {
		t.Fatalf("getting buy message: %v", err)
	}
	eng.State().Apply(exchange.FromStoreRecord(buyRec))

	// Operator emits a single-result match.
	matchMsg := h.sendMessage(h.operator,
		buildMatchPayload([]string{entryID}),
		[]string{exchange.TagMatch},
		[]string{buyMsg.ID},
	)

	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	// Buyer accepts with the correct entry_id.
	buyerAcceptMsg := h.sendMessage(h.buyer, buyerAcceptPayloadFor(entryID),
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrBuyerAccept,
			exchange.TagVerdictPrefix + "accepted",
		},
		[]string{matchMsg.ID},
	)

	// Operator delivers.
	deliverMsg := h.sendMessage(h.operator, deliverPayloadFor(entryID),
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrDeliver,
		},
		[]string{buyerAcceptMsg.ID},
	)

	// Buyer completes.
	h.sendMessage(h.buyer, completePayloadFor(entryID, 12000),
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrComplete,
			exchange.TagVerdictPrefix + "accepted",
		},
		[]string{deliverMsg.ID},
	)

	allMsgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	// Seller reputation must have increased.
	rep := eng.State().SellerReputation(h.seller.PublicKeyHex())
	if rep <= exchange.DefaultReputation {
		t.Errorf("single-match backwards compat: seller reputation = %d, want > %d",
			rep, exchange.DefaultReputation)
	}

	// One price history record.
	hist := eng.State().PriceHistory()
	if len(hist) != 1 {
		t.Fatalf("single-match backwards compat: price history len = %d, want 1", len(hist))
	}
	if hist[0].EntryID != entryID {
		t.Errorf("single-match backwards compat: price history entry_id = %q, want %q", hist[0].EntryID, entryID)
	}
}

// putPayload and buyPayload are defined in engine_test.go (same package).
