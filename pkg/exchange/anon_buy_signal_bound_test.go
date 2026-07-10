package exchange_test

// Tests for the anonymous-buy demand-signal bound (dontguess-3879, design §8-D1).
//
// OperationBuy stays TrustAnonymous (ratified): buys fold into the matching /
// demand / pricing pipeline BEFORE settlement. The ScripStore bounds the MONEY
// (the hold is taken at buyer-accept), but not the SIGNAL — a zero-scrip Sybil
// could send buys that surface (rank) an entry and, once driven through settle,
// move its demand-count / price for free.
//
// The fix (EngineOptions.MinBuyBalance, enforced in handleBuy only when a
// ScripStore is configured) requires a buyer to hold a minimum scrip balance
// BEFORE the buy is allowed to contribute to matching/demand. These tests prove
// BOTH directions with the real engine and a real LocalScripStore:
//
//	(a) a zero-balance buyer's buy is bounded: no match is emitted, the entry's
//	    demand count and price are unchanged, and the underfunded-buy counter
//	    increments.
//	(b) a funded, within-balance buyer's buy contributes: a match surfaces the
//	    entry (rank), and driving the buy through settle moves the entry's
//	    demand count and raises its price.

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/campfire-net/dontguess/pkg/store"

	"github.com/campfire-net/dontguess/pkg/exchange"
	"github.com/campfire-net/dontguess/pkg/scrip"
)

// matchCountForTag returns the number of exchange:match messages currently in
// the harness log.
func matchCount(t *testing.T, h *testHarness) int {
	t.Helper()
	msgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})
	return len(msgs)
}

// newSignalBoundEngine builds a team-tier engine (ScripStore configured) with
// the min-buy-balance signal bound enabled at the given threshold.
func newSignalBoundEngine(t *testing.T, h *testHarness, cs *scrip.LocalScripStore, minBuyBalance int64) *exchange.Engine {
	t.Helper()
	return exchange.NewEngine(exchange.EngineOptions{
		CampfireID:        h.cfID,
		LocalStore:        h.st,
		OperatorPublicKey: h.operator.pubKeyHex,
		ScripStore:        cs,
		MinBuyBalance:     minBuyBalance,
		Logger: func(format string, args ...any) {
			if h.finished.Load() {
				return
			}
			t.Logf("[engine] "+format, args...)
		},
	})
}

// TestAnonBuySignalBound_ZeroBalanceDoesNotMovePriceOrRank proves the bound: a
// buyer with zero scrip cannot inject a demand/rank signal. handleBuy drops the
// buy before matching, so no match is emitted, the entry is never surfaced, its
// demand count stays 0, its price is unchanged, and the underfunded-buy counter
// increments.
func TestAnonBuySignalBound_ZeroBalanceDoesNotMovePriceOrRank(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)

	const minBuyBalance = 1000
	cs := newCampfireScripStore(t, h)
	eng := newSignalBoundEngine(t, h, cs, minBuyBalance)

	// One accepted inventory entry.
	seedInventoryEntry(t, h, eng, "signal-bound target", "code", 4000, 2000)
	inv := eng.State().Inventory()
	if len(inv) != 1 {
		t.Fatalf("expected 1 inventory entry, got %d", len(inv))
	}
	entry := inv[0]
	priceBefore := eng.ComputePriceForTest(entry)
	demandBefore := eng.State().EntryDemandCount(entry.EntryID)

	matchesBefore := matchCount(t, h)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	h.startEngine(eng, ctx, cancel)

	// h.buyer has NEVER been minted — zero scrip balance.
	buyMsg := h.sendMessage(h.buyer,
		buyPayload("query for signal-bound target", 100000),
		[]string{exchange.TagBuy},
		nil,
	)

	// Give the poll loop ample time to fold + dispatch the buy. The gate must
	// drop it before matching, so no new match message should ever appear.
	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if eng.DegradationSnapshot().DroppedUnderfundedBuy >= 1 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	cancel()

	// (rank) No match was emitted — the entry was never surfaced to the buyer.
	if got := matchCount(t, h); got != matchesBefore {
		t.Fatalf("zero-balance buy produced a match: match count %d -> %d (signal not bounded)", matchesBefore, got)
	}
	if eng.State().IsOrderMatched(buyMsg.ID) {
		t.Fatalf("zero-balance buy order %s was matched; expected it to be dropped by the signal bound", buyMsg.ID[:8])
	}

	// (demand/price) The entry's demand signal and price are unchanged.
	if got := eng.State().EntryDemandCount(entry.EntryID); got != demandBefore {
		t.Fatalf("zero-balance buy moved demand count %d -> %d", demandBefore, got)
	}
	if got := eng.ComputePriceForTest(entry); got != priceBefore {
		t.Fatalf("zero-balance buy moved price %d -> %d", priceBefore, got)
	}

	// The drop was counted (loud, not silent).
	if snap := eng.DegradationSnapshot(); snap.DroppedUnderfundedBuy < 1 {
		t.Fatalf("expected DroppedUnderfundedBuy >= 1, got %d", snap.DroppedUnderfundedBuy)
	}
}

// TestAnonBuySignalBound_FundedBuyMovesPriceAndRank proves the pass direction: a
// buyer holding at least MinBuyBalance contributes normally. The buy surfaces
// the entry in a match (rank), and driving that buy through settle(complete)
// records the buyer against the entry — moving its demand count 0 -> 1 and
// raising its computed price.
func TestAnonBuySignalBound_FundedBuyMovesPriceAndRank(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)

	const minBuyBalance = 1000
	cs := newCampfireScripStore(t, h)
	eng := newSignalBoundEngine(t, h, cs, minBuyBalance)

	seedInventoryEntry(t, h, eng, "funded target", "code", 4000, 2000)
	inv := eng.State().Inventory()
	if len(inv) != 1 {
		t.Fatalf("expected 1 inventory entry, got %d", len(inv))
	}
	entry := inv[0]
	priceBefore := eng.ComputePriceForTest(entry)
	salePrice := priceBefore
	fee := salePrice / exchange.MatchingFeeRate
	holdAmount := salePrice + fee

	// Fund the buyer well above MinBuyBalance and enough to complete the hold.
	addScripMintMsg(t, h, h.buyer.PublicKeyHex(), holdAmount+minBuyBalance+5000)
	if err := cs.Replay(); err != nil {
		t.Fatalf("scrip Replay: %v", err)
	}

	matchesBefore := matchCount(t, h)
	preMatch, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	h.startEngine(eng, ctx, cancel)

	h.sendMessage(h.buyer,
		buyPayload("query for funded target", 100000),
		[]string{exchange.TagBuy},
		nil,
	)

	// (rank) The funded buy surfaces the entry: a match is emitted.
	matchMsg := waitForMatchMessage(t, h, preMatch, 2*time.Second)
	cancel()

	if got := matchCount(t, h); got <= matchesBefore {
		t.Fatalf("funded buy did not produce a match: count %d -> %d", matchesBefore, got)
	}
	if got := eng.DegradationSnapshot().DroppedUnderfundedBuy; got != 0 {
		t.Fatalf("funded buy was incorrectly bounded: DroppedUnderfundedBuy=%d", got)
	}
	// The match surfaces the target entry.
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
		if r.EntryID == entry.EntryID {
			found = true
		}
	}
	if !found {
		t.Fatalf("funded buy match did not surface entry %s", entry.EntryID)
	}

	// (demand/price) Drive the funded buy through settle so the demand signal
	// folds: buyer-accept (scrip hold) -> deliver -> complete records the buyer
	// against the entry, moving demand count 0 -> 1 and raising the price.
	buyerAcceptMsg := sendBuyerAcceptAndDispatch(t, h, eng, matchMsg.ID, entry.EntryID)

	deliverPayload, _ := json.Marshal(map[string]any{
		"phase":        "deliver",
		"entry_id":     entry.EntryID,
		"content_ref":  "sha256:" + fmt.Sprintf("%064x", 7),
		"content_size": int64(10000),
	})
	deliverMsg := h.sendMessage(h.operator, deliverPayload,
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrDeliver,
		},
		[]string{buyerAcceptMsg.ID},
	)
	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	completePayload, _ := json.Marshal(map[string]any{
		"phase":    "complete",
		"entry_id": entry.EntryID,
		"price":    salePrice,
	})
	completeMsg := h.sendMessage(h.buyer, completePayload,
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrComplete,
		},
		[]string{deliverMsg.ID},
	)
	allMsgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))
	rec, err := h.st.GetMessage(completeMsg.ID)
	if err != nil {
		t.Fatalf("GetMessage complete: %v", err)
	}
	if err := eng.DispatchForTest(exchange.FromStoreRecord(rec)); err != nil {
		t.Fatalf("DispatchForTest complete: %v", err)
	}

	entryAfter := eng.State().Inventory()[0]
	if got := eng.State().EntryDemandCount(entry.EntryID); got != 1 {
		t.Fatalf("funded buy did not move demand count: want 1, got %d", got)
	}
	if got := eng.ComputePriceForTest(entryAfter); got <= priceBefore {
		t.Fatalf("funded buy did not raise price: before=%d after=%d", priceBefore, got)
	}
}
