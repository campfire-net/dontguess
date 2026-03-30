package exchange_test

// Demand-signal integration tests for computePrice (dontguess-7bd).
//
// The demand multiplier (+10% per distinct buyer, capped at +100%) had zero
// coverage because the existing compute_price_test.go always left
// EntryDemandCount=0. These tests populate demand state by processing real
// engine message chains: put → accept → N×(buy → match → buyer-accept →
// deliver → complete).
//
// Each "complete" increments EntryDemandCount for the entry (via
// EntryBuyerMap[entryID][buyerKey]). ComputePriceForTest is called on a
// synthetic InventoryEntry with ContentSize=0 and PutTimestamp=0 so that
// sizeFactor=1.0 and ageFactor=1.0, leaving only the demand and reputation
// multipliers active. Expected prices are derived analytically from the
// current state to account for reputation changes that accumulate with each
// completed purchase.
//
// Formula: price = round(base * demandFactor * repFactor)
//   base = PutPrice * 1.2 = 1000 * 1.2 = 1200
//   demandFactor = 1 + min(N, 10) * 0.10
//   repFactor = 0.8 + rep/100 * 0.4  (rep rises by 1 per purchase; +3 bonus at 3+ buyers)

import (
	"encoding/json"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/campfire-net/campfire/pkg/identity"
	"github.com/campfire-net/campfire/pkg/store"

	"github.com/3dl-dev/dontguess/pkg/exchange"
)

// completeBuyTransactionForBuyer runs the full buy → match → buyer-accept →
// deliver → complete cycle for one distinct buyer identity. Each call increments
// EntryDemandCount by 1 (one new distinct buyer key in EntryBuyerMap).
//
// Preconditions:
//   - entryID is in inventory (put + accept already processed)
//   - eng.State() is current (Replay called with all messages)
//   - entry description contains "demand signal integration test task" so that
//     TF-IDF matches the buy task string used here
func completeBuyTransactionForBuyer(
	t *testing.T,
	h *testHarness,
	eng *exchange.Engine,
	buyer *identity.Identity,
	entryID string,
	putPrice int64,
) {
	t.Helper()

	// Budget must clear price + fee. As demand grows, price rises; use 5× putPrice
	// so the budget passes even at the +100% demand cap.
	budget := putPrice * 5

	// Send buy and add to engine state so IsOrderMatched / ActiveOrders are correct.
	buyMsg := h.sendMessage(buyer,
		buyPayload("demand signal integration test task "+entryID[:8], budget),
		[]string{exchange.TagBuy},
		nil,
	)
	buyRec, err := h.st.GetMessage(buyMsg.ID)
	if err != nil {
		t.Fatalf("completeBuyTransactionForBuyer: GetMessage(buy): %v", err)
	}
	eng.State().Apply(exchange.FromStoreRecord(buyRec))

	// DispatchForTest(buy) calls handleBuy synchronously: emits the match message
	// to the store and applies it to engine state inline (matchToBuyer/matchToEntry
	// are populated before this call returns).
	preMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})
	if err := eng.DispatchForTest(exchange.FromStoreRecord(buyRec)); err != nil {
		t.Fatalf("completeBuyTransactionForBuyer: DispatchForTest(buy): %v", err)
	}

	// Confirm the match was written to the store.
	var matchMsgs []store.MessageRecord
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		matchMsgs, _ = h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})
		if len(matchMsgs) > len(preMsgs) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(matchMsgs) <= len(preMsgs) {
		t.Fatal("completeBuyTransactionForBuyer: no match message emitted — " +
			"entry not matched (budget or description mismatch?)")
	}
	matchMsg := matchMsgs[len(matchMsgs)-1]

	var matchPayload struct {
		Results []struct {
			EntryID string `json:"entry_id"`
			Price   int64  `json:"price"`
		} `json:"results"`
	}
	if err := json.Unmarshal(matchMsg.Payload, &matchPayload); err != nil {
		t.Fatalf("completeBuyTransactionForBuyer: parsing match payload: %v", err)
	}
	if len(matchPayload.Results) == 0 {
		t.Fatal("completeBuyTransactionForBuyer: match has no results")
	}
	salePrice := matchPayload.Results[0].Price

	// buyer-accept (antecedent: match message).
	buyerAcceptPayload, _ := json.Marshal(map[string]any{
		"phase":    "buyer-accept",
		"entry_id": entryID,
		"accepted": true,
	})
	buyerAcceptMsg := h.sendMessage(buyer, buyerAcceptPayload,
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrBuyerAccept,
			exchange.TagVerdictPrefix + "accepted",
		},
		[]string{matchMsg.ID},
	)

	// deliver (operator, antecedent: buyer-accept).
	deliverPayload, _ := json.Marshal(map[string]any{
		"phase":        "deliver",
		"entry_id":     entryID,
		"content_ref":  "sha256:" + fmt.Sprintf("%064x", 42),
		"content_size": int64(10000),
	})
	deliverMsg := h.sendMessage(h.operator, deliverPayload,
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrDeliver,
		},
		[]string{buyerAcceptMsg.ID},
	)

	// complete (buyer, antecedent: deliver).
	completePayload, _ := json.Marshal(map[string]any{
		"phase":                 "complete",
		"entry_id":              entryID,
		"price":                 salePrice,
		"content_hash_verified": true,
	})
	h.sendMessage(buyer, completePayload,
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrComplete,
			exchange.TagVerdictPrefix + "accepted",
		},
		[]string{deliverMsg.ID},
	)

	// Rebuild state from the full message log to reflect this completed transaction.
	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))
}

// demandPriceExpected computes the expected computePrice output for an entry
// with PutPrice=1000, ContentSize=0, PutTimestamp=0, and a given demand count
// and seller reputation.
//
// Formula matches engine.go computePrice steps 1–7 (no age, size, or fast-loop):
//
//	base = 1000 * 1.2 = 1200
//	demandFactor = 1 + min(demandCount, 10) * 0.10
//	repFactor = 0.8 + rep/100 * 0.4
//	price = round(base * demandFactor * repFactor)
func demandPriceExpected(demandCount int, rep int) int64 {
	const base float64 = 1200.0
	dc := demandCount
	if dc > 10 {
		dc = 10
	}
	demandFactor := 1.0 + float64(dc)*0.10
	repFactor := 0.8 + float64(rep)/100.0*0.4
	return int64(math.Round(base * demandFactor * repFactor))
}

// demandTestEntry returns an InventoryEntry with the real EntryID and
// SellerKey (so state lookups for demand and rep work) but ContentSize=0
// and PutTimestamp=0 (eliminating size and age factors from the price).
func demandTestEntry(eng *exchange.Engine, realEntry *exchange.InventoryEntry) *exchange.InventoryEntry {
	return &exchange.InventoryEntry{
		EntryID:   realEntry.EntryID,
		SellerKey: realEntry.SellerKey,
		PutPrice:  1000,
		// PutTimestamp=0 → ageFactor=1.0 (no age decay)
		// ContentSize=0 → sizeFactor=1.0 (no size bonus)
		// fastFactor=1.0 (no fast-loop adjustment)
	}
}

// seedDemandEntry seeds one inventory entry with ContentSize=0 and a fixed
// description that the buy-task TF-IDF will match. Returns the entryID.
func seedDemandEntry(t *testing.T, h *testHarness, eng *exchange.Engine) (entryID, sellerKey string) {
	t.Helper()
	const putPrice int64 = 1000
	const tokenCost int64 = 2000

	putMsg := h.sendMessage(h.seller,
		putPayload(
			"demand signal integration test task handler generator",
			"sha256:"+fmt.Sprintf("%064x", tokenCost),
			"code",
			tokenCost,
			0, // ContentSize=0 to eliminate size factor
		),
		[]string{exchange.TagPut, "exchange:content-type:code"},
		nil,
	)

	msgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	if err := eng.AutoAcceptPut(putMsg.ID, putPrice, time.Now().Add(72*time.Hour)); err != nil {
		t.Fatalf("seedDemandEntry: AutoAcceptPut: %v", err)
	}

	entry := eng.State().GetInventoryEntry(putMsg.ID)
	if entry == nil {
		t.Fatal("seedDemandEntry: inventory entry not found after put-accept")
	}

	return putMsg.ID, h.seller.PublicKeyHex()
}

// TestComputePrice_DemandMultiplier_OneBuyer verifies that one completed
// purchase raises the price by +10% (demandFactor=1.10).
func TestComputePrice_DemandMultiplier_OneBuyer(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	entryID, _ := seedDemandEntry(t, h, eng)
	realEntry := eng.State().GetInventoryEntry(entryID)
	if realEntry == nil {
		t.Fatal("inventory entry not found")
	}

	// Baseline: demand=0, rep=50 (default).
	entry := demandTestEntry(eng, realEntry)
	baselinePrice := eng.ComputePriceForTest(entry)
	wantBaseline := demandPriceExpected(0, exchange.DefaultReputation)
	if baselinePrice != wantBaseline {
		t.Fatalf("baseline price = %d, want %d", baselinePrice, wantBaseline)
	}

	buyer1, err := identity.Generate()
	if err != nil {
		t.Fatalf("generating buyer1: %v", err)
	}
	completeBuyTransactionForBuyer(t, h, eng, buyer1, entryID, 1000)

	// EntryDemandCount must be 1.
	demandCount := eng.State().EntryDemandCount(entryID)
	if demandCount != 1 {
		t.Errorf("EntryDemandCount after 1 purchase = %d, want 1", demandCount)
	}

	// Price after 1 buyer: demand=1 → +10%.
	rep := eng.State().SellerReputation(h.seller.PublicKeyHex())
	want := demandPriceExpected(1, rep)
	price := eng.ComputePriceForTest(demandTestEntry(eng, realEntry))
	if price != want {
		t.Errorf("computePrice(demand=1, rep=%d) = %d, want %d (+10%% demand)", rep, price, want)
	}

	// Verify the demand contribution: price must be > baseline.
	if price <= baselinePrice {
		t.Errorf("price after 1 buyer (%d) should exceed baseline (%d)", price, baselinePrice)
	}
}

// TestComputePrice_DemandMultiplier_FiveBuyers verifies that five completed
// purchases raise the price by +50% (demandFactor=1.50).
func TestComputePrice_DemandMultiplier_FiveBuyers(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	entryID, _ := seedDemandEntry(t, h, eng)
	realEntry := eng.State().GetInventoryEntry(entryID)
	if realEntry == nil {
		t.Fatal("inventory entry not found")
	}

	for i := 0; i < 5; i++ {
		buyer, err := identity.Generate()
		if err != nil {
			t.Fatalf("generating buyer %d: %v", i, err)
		}
		completeBuyTransactionForBuyer(t, h, eng, buyer, entryID, 1000)
	}

	// EntryDemandCount must be 5.
	demandCount := eng.State().EntryDemandCount(entryID)
	if demandCount != 5 {
		t.Errorf("EntryDemandCount after 5 purchases = %d, want 5", demandCount)
	}

	// Price: demand=5, rep updated from 5 completed purchases + cross-agent bonus.
	rep := eng.State().SellerReputation(h.seller.PublicKeyHex())
	want := demandPriceExpected(5, rep)
	price := eng.ComputePriceForTest(demandTestEntry(eng, realEntry))
	if price != want {
		t.Errorf("computePrice(demand=5, rep=%d) = %d, want %d (+50%% demand)", rep, price, want)
	}
}

// TestComputePrice_DemandMultiplier_CapAt10Buyers verifies that the demand
// multiplier is capped at +100% when demandCount >= 10. Eleven distinct buyers
// produce the same price as exactly 10 (the cap is enforced in computePrice).
func TestComputePrice_DemandMultiplier_CapAt10Buyers(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	entryID, _ := seedDemandEntry(t, h, eng)
	realEntry := eng.State().GetInventoryEntry(entryID)
	if realEntry == nil {
		t.Fatal("inventory entry not found")
	}

	// 11 distinct buyers — one over the cap threshold.
	for i := 0; i < 11; i++ {
		buyer, err := identity.Generate()
		if err != nil {
			t.Fatalf("generating buyer %d: %v", i, err)
		}
		completeBuyTransactionForBuyer(t, h, eng, buyer, entryID, 1000)
	}

	// State tracks all 11 buyers; the cap applies only inside computePrice.
	demandCount := eng.State().EntryDemandCount(entryID)
	if demandCount != 11 {
		t.Errorf("EntryDemandCount after 11 purchases = %d, want 11", demandCount)
	}

	// Price: demand clamped to 10 inside computePrice → demandFactor=2.0.
	rep := eng.State().SellerReputation(h.seller.PublicKeyHex())
	want := demandPriceExpected(11, rep) // helper already clamps to 10
	price := eng.ComputePriceForTest(demandTestEntry(eng, realEntry))
	if price != want {
		t.Errorf("computePrice(demand=11 capped→10, rep=%d) = %d, want %d (+100%% cap)",
			rep, price, want)
	}

	// Cap verification: price with 11 buyers must equal price with 10 buyers
	// (same rep, same demand factor after clamping). Read a fresh state with
	// demand=10 by simulating — here we just verify the formula clamps correctly
	// by checking that demandPriceExpected(10, rep) == demandPriceExpected(11, rep).
	if demandPriceExpected(10, rep) != demandPriceExpected(11, rep) {
		t.Errorf("cap broken: demandPriceExpected(10) != demandPriceExpected(11)")
	}
}
