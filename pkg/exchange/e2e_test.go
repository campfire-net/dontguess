package exchange_test

// TestE2E_FullHappyPath exercises the complete exchange flow:
//
//	operator inits → seller puts → put-accept → buyer buys →
//	engine matches → buyer-accept → operator delivers → buyer completes
//
// All messages are real campfire messages on a test campfire (fs transport).
// No mocks — real SQLite store, real Ed25519 keys.
//
// Additional cases covered:
//   - buyer reject: escrow path, order not completed
//   - put reject:   rejected entry never enters inventory
//   - empty match:  budget=1 so no inventory candidate passes price filter

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/campfire-net/campfire/pkg/store"

	"github.com/campfire-net/dontguess/pkg/exchange"
	"github.com/campfire-net/dontguess/pkg/scrip"
)

// ----------------------------------------------------------------------------
// Happy path: put → accept → buy → match → buyer-accept → deliver → complete
// ----------------------------------------------------------------------------

func TestE2E_FullHappyPath(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	// --- Step 1: Seller puts cached inference ---
	putMsg := h.sendMessage(h.seller,
		putPayload("Go HTTP handler unit test generator", "sha256:"+fmt.Sprintf("%064x", 100), "code", 12000, 20000),
		[]string{exchange.TagPut, "exchange:content-type:code", "exchange:domain:go"},
		nil,
	)

	// Replay to pick up the put.
	msgs, err := h.st.ListMessages(h.cfID, 0)
	if err != nil {
		t.Fatalf("listing messages: %v", err)
	}
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	// Inventory must be empty before acceptance.
	if inv := eng.State().Inventory(); len(inv) != 0 {
		t.Errorf("step 1: expected empty inventory before put-accept, got %d", len(inv))
	}

	// --- Step 2: Operator accepts the put ---
	if err := eng.AutoAcceptPut(putMsg.ID, 8400, time.Now().Add(168*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut: %v", err)
	}

	// Inventory must have one live entry.
	inv := eng.State().Inventory()
	if len(inv) != 1 {
		t.Fatalf("step 2: expected 1 inventory entry after put-accept, got %d", len(inv))
	}
	entry := inv[0]
	if entry.PutMsgID != putMsg.ID {
		t.Errorf("step 2: inventory entry PutMsgID = %q, want %q", entry.PutMsgID, putMsg.ID)
	}
	if entry.PutPrice != 8400 {
		t.Errorf("step 2: inventory entry PutPrice = %d, want 8400", entry.PutPrice)
	}

	// --- Step 3: Buyer sends a buy ---
	buyMsg := h.sendMessage(h.buyer,
		buyPayload("Generate unit tests for a Go HTTP handler accepting JSON POST with validation", 50000),
		[]string{exchange.TagBuy},
		nil,
	)

	// Apply the buy to the engine state so the order appears in ActiveOrders.
	buyRec, err := h.st.GetMessage(buyMsg.ID)
	if err != nil {
		t.Fatalf("getting buy message: %v", err)
	}
	eng.State().Apply(exchange.FromStoreRecord(buyRec))

	orders := eng.State().ActiveOrders()
	foundOrder := false
	for _, o := range orders {
		if o.OrderID == buyMsg.ID {
			foundOrder = true
		}
	}
	if !foundOrder {
		t.Fatalf("step 3: buy order %s not in active orders", buyMsg.ID[:8])
	}

	// --- Step 4: Engine processes the buy and emits a match ---
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	preMatchMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})
	preMatchCount := len(preMatchMsgs)

	go func() { _ = eng.Start(ctx) }()

	var matchMsgs []store.MessageRecord
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		matchMsgs, _ = h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})
		if len(matchMsgs) > preMatchCount {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()

	if len(matchMsgs) <= preMatchCount {
		t.Fatal("step 4: no match message emitted by engine")
	}

	matchMsg := matchMsgs[len(matchMsgs)-1]

	// Match must reference the buy as antecedent.
	if len(matchMsg.Antecedents) == 0 || matchMsg.Antecedents[0] != buyMsg.ID {
		t.Errorf("step 4: match antecedent = %v, want [%s]", matchMsg.Antecedents, buyMsg.ID)
	}
	// Match sender must be the operator.
	if matchMsg.Sender != h.operator.PublicKeyHex() {
		t.Errorf("step 4: match sender = %q, want operator %q", matchMsg.Sender, h.operator.PublicKeyHex())
	}

	var matchPayload struct {
		Results []struct {
			EntryID  string  `json:"entry_id"`
			Price    int64   `json:"price"`
			Confidence float64 `json:"confidence"`
		} `json:"results"`
	}
	if err := json.Unmarshal(matchMsg.Payload, &matchPayload); err != nil {
		t.Fatalf("step 4: parsing match payload: %v", err)
	}
	if len(matchPayload.Results) == 0 {
		t.Fatal("step 4: match has no results")
	}
	if matchPayload.Results[0].EntryID != putMsg.ID {
		t.Errorf("step 4: match result entry_id = %q, want %q", matchPayload.Results[0].EntryID, putMsg.ID)
	}

	// Order must now be marked matched in state.
	// Re-sync state from store before checking.
	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))
	if !eng.State().IsOrderMatched(buyMsg.ID) {
		t.Error("step 4: buy order should be marked matched after engine emitted match")
	}

	// --- Step 5: Buyer accepts the match ---
	buyerAcceptPayload, _ := json.Marshal(map[string]any{
		"phase":    "buyer-accept",
		"entry_id": matchPayload.Results[0].EntryID,
		"accepted": true,
	})
	buyerAcceptMsg := h.sendMessage(h.buyer, buyerAcceptPayload,
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrBuyerAccept,
			exchange.TagVerdictPrefix + "accepted",
		},
		[]string{matchMsg.ID},
	)

	allMsgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	// acceptedOrders state: matchMsg.ID → entryID (not exported directly, but
	// we can verify by checking that deliver will work).
	// The accepted order is recorded; we verify via state.GetInventoryEntry.
	if eng.State().GetInventoryEntry(entry.EntryID) == nil {
		t.Error("step 5: inventory entry disappeared after buyer-accept")
	}

	// --- Step 6: Operator delivers content ---
	deliverPayload, _ := json.Marshal(map[string]any{
		"phase":        "deliver",
		"entry_id":     matchPayload.Results[0].EntryID,
		"content_ref":  "sha256:" + fmt.Sprintf("%064x", 100),
		"content_size": 20000,
	})
	deliverMsg := h.sendMessage(h.operator, deliverPayload,
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrDeliver,
		},
		[]string{buyerAcceptMsg.ID},
	)

	allMsgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	// --- Step 7: Buyer completes the transaction ---
	salePrice := matchPayload.Results[0].Price
	completePayload, _ := json.Marshal(map[string]any{
		"phase":                  "complete",
		"entry_id":               matchPayload.Results[0].EntryID,
		"price":                  salePrice,
		"content_hash_verified":  true,
	})
	h.sendMessage(h.buyer, completePayload,
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrComplete,
			exchange.TagVerdictPrefix + "accepted",
		},
		[]string{deliverMsg.ID},
	)

	allMsgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	// After complete, seller reputation must be > default.
	rep := eng.State().SellerReputation(h.seller.PublicKeyHex())
	if rep <= exchange.DefaultReputation {
		t.Errorf("step 7: seller reputation = %d after successful sale, want > %d", rep, exchange.DefaultReputation)
	}

	// Price history must have one record.
	history := eng.State().PriceHistory()
	if len(history) != 1 {
		t.Errorf("step 7: price history len = %d, want 1", len(history))
	}
	if history[0].EntryID != matchPayload.Results[0].EntryID {
		t.Errorf("step 7: price history entry_id = %q, want %q", history[0].EntryID, matchPayload.Results[0].EntryID)
	}
}

// ----------------------------------------------------------------------------
// Buyer reject: buyer rejects the match; no completion, reputation unchanged
// ----------------------------------------------------------------------------

func TestE2E_BuyerReject(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)

	// Wire a real CampfireScripStore so we can assert escrow (non-)interaction.
	// Convention §scrip: buyer-reject has no scrip interaction — scrip is only locked
	// at buyer-accept time. A buyer who rejects after seeing the match never touches escrow.
	cs := newCampfireScripStore(t, h)
	eng := exchange.NewEngine(exchange.EngineOptions{
		CampfireID:       h.cfID,
		Store:            h.st,
		ReadClient:  h.newOperatorClient(),
		WriteClient:      h.newOperatorClient(),
		ScripStore:       cs,
		Logger: func(format string, args ...any) {
			t.Logf("[engine] "+format, args...)
		},
	})

	// Seed buyer with scrip so any accidental escrow deduction would be visible.
	const buyerSeedScrip = int64(50_000)
	addScripMintMsg(t, h, h.buyer.PublicKeyHex(), buyerSeedScrip)
	if err := cs.Replay(); err != nil {
		t.Fatalf("cs.Replay after mint: %v", err)
	}
	buyerBalanceBefore := cs.Balance(h.buyer.PublicKeyHex())

	// Seed inventory.
	putMsg := h.sendMessage(h.seller,
		putPayload("Python async scraper", "sha256:"+fmt.Sprintf("%064x", 200), "code", 8000, 15000),
		[]string{exchange.TagPut, "exchange:content-type:code", "exchange:domain:python"},
		nil,
	)
	msgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))
	if err := eng.AutoAcceptPut(putMsg.ID, 5600, time.Now().Add(72*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut: %v", err)
	}

	// Buyer sends buy.
	buyMsg := h.sendMessage(h.buyer,
		buyPayload("Async HTTP scraper in Python using aiohttp", 20000),
		[]string{exchange.TagBuy},
		nil,
	)

	// Run engine to emit match.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	preMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})
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
		t.Fatal("no match emitted")
	}
	matchMsg := matchMsgs[len(matchMsgs)-1]

	// Parse entry_id from match.
	var mp struct {
		Results []struct{ EntryID string `json:"entry_id"` } `json:"results"`
	}
	if err := json.Unmarshal(matchMsg.Payload, &mp); err != nil || len(mp.Results) == 0 {
		t.Fatalf("parsing match payload: %v", err)
	}

	// Snapshot scrip-buy-hold count before reject — no new holds should appear.
	preBuyHoldMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{scrip.TagScripBuyHold}})

	// Buyer rejects (no buyer-accept was sent — buyer declines after seeing the match).
	rejectPayload, _ := json.Marshal(map[string]any{
		"phase":    "buyer-reject",
		"entry_id": mp.Results[0].EntryID,
		"accepted": false,
		"reason":   "does not meet freshness requirement",
	})
	h.sendMessage(h.buyer, rejectPayload,
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrBuyerReject,
			exchange.TagVerdictPrefix + "rejected",
		},
		[]string{matchMsg.ID},
	)

	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	// --- Scrip balance assertions: escrow refund path ---
	// Convention §scrip: buyer-reject has no scrip interaction. Scrip is only locked
	// at buyer-accept time, which was never sent in this flow. Buyer balance must be
	// unchanged and no scrip-buy-hold message should have been emitted.

	buyerBalanceAfter := cs.Balance(h.buyer.PublicKeyHex())
	if buyerBalanceAfter != buyerBalanceBefore {
		t.Errorf("buyer-reject: buyer scrip balance changed: got %d, want %d (unchanged) — escrow must not be taken before buyer-accept",
			buyerBalanceAfter, buyerBalanceBefore)
	}

	afterBuyHoldMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{scrip.TagScripBuyHold}})
	if len(afterBuyHoldMsgs) != len(preBuyHoldMsgs) {
		t.Errorf("buyer-reject: unexpected scrip-buy-hold message emitted (before=%d, after=%d) — no escrow should be created on reject",
			len(preBuyHoldMsgs), len(afterBuyHoldMsgs))
	}

	// No price history: transaction was not completed.
	history := eng.State().PriceHistory()
	if len(history) != 0 {
		t.Errorf("buyer-reject: expected 0 price records, got %d", len(history))
	}

	// Seller reputation must remain at default (no successful sale).
	rep := eng.State().SellerReputation(h.seller.PublicKeyHex())
	if rep != exchange.DefaultReputation {
		t.Errorf("buyer-reject: seller reputation = %d, want %d (default)", rep, exchange.DefaultReputation)
	}

	// Buy order must be marked matched (the match was sent, order was consumed).
	if !eng.State().IsOrderMatched(buyMsg.ID) {
		t.Error("buyer-reject: buy order should be marked matched after match was sent")
	}

	// Inventory entry must still be live (rejection doesn't remove it).
	if eng.State().GetInventoryEntry(mp.Results[0].EntryID) == nil {
		t.Error("buyer-reject: inventory entry should remain after rejection")
	}
}

// ----------------------------------------------------------------------------
// Put reject: rejected put never enters inventory
// ----------------------------------------------------------------------------

func TestE2E_PutReject(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	// Seller puts low-quality content.
	putMsg := h.sendMessage(h.seller,
		putPayload("Placeholder answer", "sha256:"+fmt.Sprintf("%064x", 300), "other", 100, 64),
		[]string{exchange.TagPut, "exchange:content-type:other"},
		nil,
	)

	msgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	// Operator rejects the put.
	rejectPayload, _ := json.Marshal(map[string]any{
		"phase":    "put-reject",
		"entry_id": putMsg.ID,
		"reason":   "content does not meet minimum quality bar",
	})
	h.sendMessage(h.operator, rejectPayload,
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrPutReject,
			exchange.TagVerdictPrefix + "rejected",
		},
		[]string{putMsg.ID},
	)

	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	// Inventory must be empty: rejected put never enters inventory.
	if inv := eng.State().Inventory(); len(inv) != 0 {
		t.Errorf("put-reject: expected empty inventory, got %d entries", len(inv))
	}

	// No price history.
	if history := eng.State().PriceHistory(); len(history) != 0 {
		t.Errorf("put-reject: expected 0 price records, got %d", len(history))
	}
}

// ----------------------------------------------------------------------------
// Empty match: no inventory candidates satisfy buyer's budget
// ----------------------------------------------------------------------------

func TestE2E_EmptyMatch(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	// Seed one inventory entry with a high put price → high ask price.
	putMsg := h.sendMessage(h.seller,
		putPayload("Expensive inference result", "sha256:"+fmt.Sprintf("%064x", 400), "analysis", 50000, 80000),
		[]string{exchange.TagPut, "exchange:content-type:analysis", "exchange:domain:finance"},
		nil,
	)
	msgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))
	// Accept at 35000 scrip → ask price = 35000 * 120/100 = 42000.
	if err := eng.AutoAcceptPut(putMsg.ID, 35000, time.Now().Add(48*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut: %v", err)
	}

	// Buyer sends a buy with budget=1 — below any possible ask price.
	h.sendMessage(h.buyer,
		buyPayload("Finance analysis", 1),
		[]string{exchange.TagBuy},
		nil,
	)

	// Run engine.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	preMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})
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
		t.Fatal("empty-match: engine must still emit a match message (with empty results)")
	}

	matchMsg := matchMsgs[len(matchMsgs)-1]
	var mp struct {
		Results    []json.RawMessage `json:"results"`
		SearchMeta struct {
			TotalCandidates int `json:"total_candidates"`
		} `json:"search_meta"`
	}
	if err := json.Unmarshal(matchMsg.Payload, &mp); err != nil {
		t.Fatalf("empty-match: parsing match payload: %v", err)
	}
	if len(mp.Results) != 0 {
		t.Errorf("empty-match: expected 0 results, got %d", len(mp.Results))
	}
	// total_candidates should be 0 (nothing passed budget filter).
	if mp.SearchMeta.TotalCandidates != 0 {
		t.Errorf("empty-match: total_candidates = %d, want 0", mp.SearchMeta.TotalCandidates)
	}
}

// ----------------------------------------------------------------------------
// Scrip balance E2E: mint → put-accept (seller paid) → buy (buyer held) →
// match → settle(complete) → verify seller residual + operator revenue + fee burned
// ----------------------------------------------------------------------------

// TestE2E_ScripBalances exercises the complete exchange flow with a real
// CampfireScripStore wired into the engine, asserting scrip balances at each step:
//
//  1. Mint:          buyer receives scrip via scrip-mint convention message
//  2. Put-accept:    seller receives scrip-put-pay; operator balance decremented
//  3. Buy:           buyer balance UNCHANGED (no pre-decrement; hold deferred to buyer-accept)
//  4. Buyer-accept:  buyer balance decremented by (price + fee); scrip-buy-hold emitted
//  5. Settle:        seller receives residual; operator receives exchange revenue;
//     fee is burned; reservation is deleted
//  6. Campfire log:  scrip-buy-hold, scrip-settle, scrip-burn messages all present
//  7. Replay:        fresh CampfireScripStore reproduces the same balances from the log
func TestE2E_ScripBalances(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)

	// --- Mint: give buyer enough scrip to complete the purchase ---
	// We'll seed after we know the price (computed from put_price).
	// First, seed the inventory so we can compute the price.
	cs := newCampfireScripStore(t, h)
	eng := exchange.NewEngine(exchange.EngineOptions{
		CampfireID:       h.cfID,
		Store:            h.st,
		ReadClient:  h.newOperatorClient(),
		WriteClient:      h.newOperatorClient(),
		ScripStore:       cs,
		Logger:           func(format string, args ...any) { t.Logf("[engine] "+format, args...) },
	})

	// Seed one inventory entry: put_price = 5600, sale_price = 6720, fee = 672, hold = 7392.
	const putPrice = int64(5600)
	const tokenCostSeed = int64(8000)
	putMsgID := seedInventoryEntry(t, h, eng, "Go HTTP handler unit test generator", "code", tokenCostSeed, putPrice)
	inv := eng.State().Inventory()
	if len(inv) != 1 {
		t.Fatalf("expected 1 inventory entry, got %d", len(inv))
	}
	entry := inv[0]
	salePrice := eng.ComputePriceForTest(entry)
	fee := salePrice / exchange.MatchingFeeRate
	holdAmount := salePrice + fee
	enginePrice := holdAmount * exchange.MatchingFeeRate / (exchange.MatchingFeeRate + 1)
	expectedResidual := enginePrice / exchange.ResidualRate
	expectedExchangeRevenue := enginePrice - expectedResidual

	// --- Step 1: Mint — buyer receives scrip ---
	const buyerExtra = int64(5000)
	addScripMintMsg(t, h, h.buyer.PublicKeyHex(), holdAmount+buyerExtra)
	if err := cs.Replay(); err != nil {
		t.Fatalf("Replay after mint: %v", err)
	}

	buyerBalanceAfterMint := cs.Balance(h.buyer.PublicKeyHex())
	if buyerBalanceAfterMint != holdAmount+buyerExtra {
		t.Errorf("step 1 (mint): buyer balance = %d, want %d", buyerBalanceAfterMint, holdAmount+buyerExtra)
	}

	// --- Step 2: Put-accept (scrip-put-pay) ---
	// seedInventoryEntry already called AutoAcceptPut. The engine does not automatically
	// emit scrip-put-pay — that is an operator-initiated side-effect after accepting.
	// Emit it now, replay, and assert the seller's balance increased by putPrice.
	if eng.State().GetInventoryEntry(entry.EntryID) == nil {
		t.Fatal("step 2 (put-accept): inventory entry missing after AutoAcceptPut")
	}
	const discountPct = int64(30) // (1 - 5600/8000) * 100
	addScripPutPayMsg(t, h, putMsgID, h.seller.PublicKeyHex(), putPrice, tokenCostSeed, discountPct, entry.ContentHash)
	if err := cs.Replay(); err != nil {
		t.Fatalf("Replay after put-pay: %v", err)
	}
	sellerBalanceAfterPutPay := cs.Balance(h.seller.PublicKeyHex())
	if sellerBalanceAfterPutPay != putPrice {
		t.Errorf("step 2 (put-accept): seller balance = %d, want %d (putPrice)", sellerBalanceAfterPutPay, putPrice)
	}

	// --- Step 3: Buy — buyer balance decremented ---
	preBuyHoldMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{scrip.TagScripBuyHold}})
	preMatchMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() { _ = eng.Start(ctx) }()

	h.sendMessage(h.buyer,
		buyPayload("Generate unit tests for a Go HTTP handler", salePrice+5000),
		[]string{exchange.TagBuy},
		nil,
	)

	// Wait for match message (engine processed the buy — no scrip hold at buy time).
	matchMsg := waitForMatchMessage(t, h, preMatchMsgs, 2*time.Second)
	cancel()

	// Step 3 assertion: buyer balance UNCHANGED after buy (hold deferred to buyer-accept).
	buyerBalanceAfterBuy := cs.Balance(h.buyer.PublicKeyHex())
	if buyerBalanceAfterBuy != buyerBalanceAfterMint {
		t.Errorf("step 3 (buy): buyer balance = %d, want %d (no pre-decrement at buy time)",
			buyerBalanceAfterBuy, buyerBalanceAfterMint)
	}

	// --- Step 4: Buyer-accept — triggers scrip hold; campfire log gets scrip-buy-hold ---
	// Dispatch buyer-accept through engine to trigger handleSettleBuyerAcceptScrip.
	buyerAcceptMsgE2E := sendBuyerAcceptAndDispatch(t, h, eng, matchMsg.ID, entry.EntryID)

	// Buy-hold message must appear after buyer-accept.
	afterBuyHoldMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{scrip.TagScripBuyHold}})
	if len(afterBuyHoldMsgs) <= len(preBuyHoldMsgs) {
		t.Error("step 4 (buyer-accept): expected scrip-buy-hold message in campfire log after buyer-accept")
	}

	// Buyer balance must be decremented by holdAmount after buyer-accept.
	buyerBalanceAfterAccept := cs.Balance(h.buyer.PublicKeyHex())
	if buyerBalanceAfterAccept != buyerBalanceAfterMint-holdAmount {
		t.Errorf("step 4 (buyer-accept): buyer balance = %d, want %d (mint=%d - hold=%d)",
			buyerBalanceAfterAccept, buyerBalanceAfterMint-holdAmount, buyerBalanceAfterMint, holdAmount)
	}

	// Extract reservation_id from the scrip-buy-hold log message.
	resID := extractReservationIDFromLog(t, h)
	if resID == "" {
		t.Fatal("step 4 (buyer-accept): reservation_id must be non-empty in scrip-buy-hold log")
	}

	// Reservation must exist in the store.
	if _, err := cs.GetReservation(context.Background(), resID); err != nil {
		t.Fatalf("step 4 (buyer-accept): reservation %s must exist after buyer-accept: %v", resID[:8], err)
	}

	// --- Step 5: Settle(complete) — scrip flows to seller and operator ---
	sellerBalanceBefore := cs.Balance(h.seller.PublicKeyHex())
	operatorBalanceBefore := cs.Balance(h.operator.PublicKeyHex())

	preSettleMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{scrip.TagScripSettle}})
	preBurnMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{scrip.TagScripBurn}})

	// deliver (antecedent = buyer-accept message).
	deliverPayloadE2E, _ := json.Marshal(map[string]any{
		"phase":        "deliver",
		"entry_id":     entry.EntryID,
		"content_ref":  "sha256:" + fmt.Sprintf("%064x", 999),
		"content_size": int64(20000),
	})
	deliverMsgE2E := h.sendMessage(h.operator, deliverPayloadE2E,
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrDeliver,
		},
		[]string{buyerAcceptMsgE2E.ID},
	)

	// Replay to pick up deliver before dispatching complete.
	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	// Price is derived from the reservation (locked at buyer-accept), not from payload.
	completePayload, _ := json.Marshal(map[string]any{
		"entry_id": entry.EntryID,
	})
	completeMsg := h.sendMessage(h.buyer, completePayload,
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrComplete,
		},
		[]string{deliverMsgE2E.ID},
	)

	allMsgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))
	rec, err := h.st.GetMessage(completeMsg.ID)
	if err != nil {
		t.Fatalf("step 5 (settle): GetMessage: %v", err)
	}
	if err := eng.DispatchForTest(exchange.FromStoreRecord(rec)); err != nil {
		t.Fatalf("step 5 (settle): DispatchForTest: %v", err)
	}

	// Step 5 assertions: seller residual and operator revenue.
	sellerBalanceAfterSettle := cs.Balance(h.seller.PublicKeyHex())
	if sellerBalanceAfterSettle != sellerBalanceBefore+expectedResidual {
		t.Errorf("step 5 (settle): seller balance = %d, want %d (before=%d + residual=%d)",
			sellerBalanceAfterSettle, sellerBalanceBefore+expectedResidual,
			sellerBalanceBefore, expectedResidual)
	}

	operatorBalanceAfterSettle := cs.Balance(h.operator.PublicKeyHex())
	if operatorBalanceAfterSettle != operatorBalanceBefore+expectedExchangeRevenue {
		t.Errorf("step 5 (settle): operator balance = %d, want %d (before=%d + revenue=%d)",
			operatorBalanceAfterSettle, operatorBalanceBefore+expectedExchangeRevenue,
			operatorBalanceBefore, expectedExchangeRevenue)
	}

	// Reservation must be deleted after settle.
	if _, err := cs.GetReservation(context.Background(), resID); err == nil {
		t.Errorf("step 5 (settle): reservation %s must be deleted after settle(complete)", resID[:8])
	}

	// --- Step 6: Campfire log — scrip convention messages present ---
	afterSettleMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{scrip.TagScripSettle}})
	if len(afterSettleMsgs) <= len(preSettleMsgs) {
		t.Error("step 6 (log): scrip-settle message not found in campfire log")
	}

	afterBurnMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{scrip.TagScripBurn}})
	if len(afterBurnMsgs) <= len(preBurnMsgs) {
		t.Error("step 6 (log): scrip-burn message not found in campfire log")
	}

	// Verify the scrip-settle payload is correct.
	var sp scrip.SettlePayload
	if err := json.Unmarshal(afterSettleMsgs[len(afterSettleMsgs)-1].Payload, &sp); err != nil {
		t.Fatalf("step 6 (log): parsing scrip-settle payload: %v", err)
	}
	if sp.ReservationID != resID {
		t.Errorf("step 6 (log): scrip-settle reservation_id = %q, want %q", sp.ReservationID, resID)
	}
	if sp.Residual != expectedResidual {
		t.Errorf("step 6 (log): scrip-settle residual = %d, want %d", sp.Residual, expectedResidual)
	}
	if sp.FeeBurned != fee {
		t.Errorf("step 6 (log): scrip-settle fee_burned = %d, want %d", sp.FeeBurned, fee)
	}
	if sp.ExchangeRevenue != expectedExchangeRevenue {
		t.Errorf("step 6 (log): scrip-settle exchange_revenue = %d, want %d", sp.ExchangeRevenue, expectedExchangeRevenue)
	}

	// --- Step 7: Replay — fresh store reproduces same balances ---
	freshCS, err := scrip.NewCampfireScripStore(h.cfID, h.newOperatorClient(), h.operator.PublicKeyHex())
	if err != nil {
		t.Fatalf("step 7 (replay): NewCampfireScripStore: %v", err)
	}

	// Buyer: mint - hold (settled = no refund) = buyerExtra (5000).
	// The hold was not refunded — it was consumed by the settle.
	// The scrip-buy-hold message decrements the buyer's balance during replay.
	// There is no scrip-dispute-refund, so the hold is permanent.
	// Final buyer balance = mint - hold = holdAmount+buyerExtra - holdAmount = buyerExtra.
	replayedBuyerBalance := freshCS.Balance(h.buyer.PublicKeyHex())
	if replayedBuyerBalance != buyerExtra {
		t.Errorf("step 7 (replay): buyer balance = %d, want %d (mint=%d - hold=%d)",
			replayedBuyerBalance, buyerExtra, holdAmount+buyerExtra, holdAmount)
	}

	// Seller: put-pay (putPrice) at step 2 + residual from settle at step 5.
	replayedSellerBalance := freshCS.Balance(h.seller.PublicKeyHex())
	wantSellerBalance := putPrice + expectedResidual
	if replayedSellerBalance != wantSellerBalance {
		t.Errorf("step 7 (replay): seller balance = %d, want %d (putPrice=%d + residual=%d)",
			replayedSellerBalance, wantSellerBalance, putPrice, expectedResidual)
	}

	// Fee burned: totalBurned reflects the matching fee from scrip-burn.
	if freshCS.TotalBurned() != fee {
		t.Errorf("step 7 (replay): total_burned = %d, want %d (matching fee, no double-count)", freshCS.TotalBurned(), fee)
	}
}

// ----------------------------------------------------------------------------
// Small-content dispute path:
// put (token_cost<500) → auto-accept → buy → match → buyer-accept (via match)
// → deliver → small-content-dispute → auto-refund
// ----------------------------------------------------------------------------

// TestE2E_SmallContentDisputePath exercises the automated dispute path for
// small content (token_cost < SmallContentThreshold = 500 tokens).
//
// Small content is too small for meaningful preview, so the buyer skips the
// preview phase and sends buyer-accept directly from the match message.
// After delivery the buyer can file a small-content-dispute to get an
// automatic refund, which penalises the seller's reputation by
// SmallContentReputationPenalty (3) per refund.
//
// Flow:
//
//  1. Seller puts content with token_cost = 100 (< 500)
//  2. Operator auto-accepts the put → entry enters inventory
//  3. Buyer sends a buy request
//  4. Engine runs and emits a match (buyer-accept antecedent is the match)
//  5. Buyer sends settle(buyer-accept) with antecedent = match message
//  6. Operator sends settle(deliver)
//  7. Buyer sends settle(small-content-dispute)
//
// Verified:
//   - Entry has token_cost < SmallContentThreshold → no preview required
//   - buyer-accept antecedent is the match message (not a preview)
//   - small-content-dispute triggers auto-refund: SmallContentDisputeCount++
//   - Seller's SmallContentRefundCount incremented → reputation drops by 3
//   - If ScripStore is wired: buyer scrip is refunded
func TestE2E_SmallContentDisputePath(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)

	// Wire a scrip store so we can verify the refund path.
	cs := newCampfireScripStore(t, h)
	eng := exchange.NewEngine(exchange.EngineOptions{
		CampfireID:       h.cfID,
		Store:            h.st,
		ReadClient:  h.newOperatorClient(),
		WriteClient:      h.newOperatorClient(),
		ScripStore:       cs,
		Logger:           func(format string, args ...any) { t.Logf("[engine] "+format, args...) },
	})

	// --- Step 1: Seller puts small content (token_cost = 100, < SmallContentThreshold 500) ---
	putMsg := h.sendMessage(h.seller,
		putPayload("Tiny Go one-liner helper", "sha256:"+fmt.Sprintf("%064x", 777), "code", 100, 200),
		[]string{exchange.TagPut, "exchange:content-type:code", "exchange:domain:go"},
		nil,
	)

	msgs, err := h.st.ListMessages(h.cfID, 0)
	if err != nil {
		t.Fatalf("listing messages: %v", err)
	}
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	// --- Step 2: Operator auto-accepts the put ---
	if err := eng.AutoAcceptPut(putMsg.ID, 70, time.Now().Add(72*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut: %v", err)
	}

	inv := eng.State().Inventory()
	if len(inv) != 1 {
		t.Fatalf("step 2: expected 1 inventory entry after put-accept, got %d", len(inv))
	}
	entry := inv[0]

	// Verify the entry is genuinely small content.
	if entry.TokenCost >= exchange.SmallContentThreshold {
		t.Errorf("step 2: token_cost = %d, want < %d (small content)", entry.TokenCost, exchange.SmallContentThreshold)
	}

	// Mint enough scrip for the buyer to cover the purchase.
	salePrice := eng.ComputePriceForTest(entry)
	fee := salePrice / exchange.MatchingFeeRate
	holdAmount := salePrice + fee
	const buyerExtra = int64(500)
	addScripMintMsg(t, h, h.buyer.PublicKeyHex(), holdAmount+buyerExtra)
	if err := cs.Replay(); err != nil {
		t.Fatalf("Replay after mint: %v", err)
	}
	buyerBalanceAfterMint := cs.Balance(h.buyer.PublicKeyHex())

	// --- Step 3: Buyer sends a buy request ---
	preMatchMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})

	h.sendMessage(h.buyer,
		buyPayload("Tiny Go helper function", holdAmount+buyerExtra),
		[]string{exchange.TagBuy},
		nil,
	)

	// --- Step 4: Engine runs and emits a match ---
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() { _ = eng.Start(ctx) }()

	var matchMsgs []store.MessageRecord
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		matchMsgs, _ = h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})
		if len(matchMsgs) > len(preMatchMsgs) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()

	if len(matchMsgs) <= len(preMatchMsgs) {
		t.Fatal("step 4: no match message emitted by engine")
	}
	matchMsg := matchMsgs[len(matchMsgs)-1]

	// Match must reference the buy as antecedent.
	if len(matchMsg.Antecedents) == 0 {
		t.Fatalf("step 4: match has no antecedents")
	}

	// Parse entry_id from match payload.
	var mp struct {
		Results []struct {
			EntryID string `json:"entry_id"`
			Price   int64  `json:"price"`
		} `json:"results"`
	}
	if err := json.Unmarshal(matchMsg.Payload, &mp); err != nil || len(mp.Results) == 0 {
		t.Fatalf("step 4: parsing match payload: %v (results=%d)", err, len(mp.Results))
	}
	if mp.Results[0].EntryID != entry.EntryID {
		t.Errorf("step 4: match result entry_id = %q, want %q", mp.Results[0].EntryID, entry.EntryID)
	}

	// Sync state.
	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	// --- Step 5: Buyer sends settle(buyer-accept) with antecedent = match message ---
	// For small content there is no preview phase — the buyer-accept references the
	// match directly, not a preview message.
	buyerAcceptPayload, _ := json.Marshal(map[string]any{
		"phase":    "buyer-accept",
		"entry_id": entry.EntryID,
		"accepted": true,
	})
	buyerAcceptMsg := h.sendMessage(h.buyer, buyerAcceptPayload,
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrBuyerAccept,
			exchange.TagVerdictPrefix + "accepted",
		},
		[]string{matchMsg.ID}, // antecedent is the match, not a preview
	)

	// Dispatch buyer-accept through the engine to trigger scrip hold.
	allMsgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))
	buyerAcceptRec, err := h.st.GetMessage(buyerAcceptMsg.ID)
	if err != nil {
		t.Fatalf("step 5: GetMessage(buyer-accept): %v", err)
	}
	if err := eng.DispatchForTest(exchange.FromStoreRecord(buyerAcceptRec)); err != nil {
		t.Fatalf("step 5: DispatchForTest(buyer-accept): %v", err)
	}

	// --- Step 6: Operator sends settle(deliver) ---
	deliverPayload, _ := json.Marshal(map[string]any{
		"phase":        "deliver",
		"entry_id":     entry.EntryID,
		"content_ref":  "sha256:" + fmt.Sprintf("%064x", 777),
		"content_size": int64(200),
	})
	deliverMsg := h.sendMessage(h.operator, deliverPayload,
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrDeliver,
		},
		[]string{buyerAcceptMsg.ID},
	)

	allMsgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	// --- Step 7: Buyer sends settle(small-content-dispute) ---
	// Extract the reservation_id from the scrip-buy-hold log so we can include
	// it in the dispute payload (engine uses it to locate and consume the reservation).
	resID := extractReservationIDFromLog(t, h)
	if resID == "" {
		t.Fatal("step 7: no scrip-buy-hold message found — reservation_id unavailable")
	}

	reputationBefore := eng.State().SellerReputation(h.seller.PublicKeyHex())
	disputeCountBefore := eng.State().SmallContentDisputeCount(entry.EntryID)

	disputePayload, _ := json.Marshal(map[string]any{
		"phase":          "small-content-dispute",
		"entry_id":       entry.EntryID,
		"reservation_id": resID,
		"buyer_key":      h.buyer.PublicKeyHex(),
		"reason":         "content too small for preview — auto-refund requested",
	})
	disputeMsg := h.sendMessage(h.buyer, disputePayload,
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrSmallContentDispute,
		},
		[]string{deliverMsg.ID},
	)

	allMsgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	// Dispatch the dispute through the engine to trigger auto-refund logic.
	disputeRec, err := h.st.GetMessage(disputeMsg.ID)
	if err != nil {
		t.Fatalf("step 7: GetMessage(dispute): %v", err)
	}
	if err := eng.DispatchForTest(exchange.FromStoreRecord(disputeRec)); err != nil {
		t.Fatalf("step 7: DispatchForTest(dispute): %v", err)
	}

	// Sync state after dispute.
	allMsgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	// --- Verify: smallContentDisputes incremented ---
	disputeCountAfter := eng.State().SmallContentDisputeCount(entry.EntryID)
	if disputeCountAfter != disputeCountBefore+1 {
		t.Errorf("step 7: SmallContentDisputeCount = %d, want %d (before=%d + 1)",
			disputeCountAfter, disputeCountBefore+1, disputeCountBefore)
	}

	// --- Verify: seller reputation decreased by SmallContentReputationPenalty (3) ---
	reputationAfter := eng.State().SellerReputation(h.seller.PublicKeyHex())
	expectedReputation := reputationBefore - exchange.SmallContentReputationPenalty
	if reputationAfter != expectedReputation {
		t.Errorf("step 7: seller reputation = %d after dispute, want %d (before=%d - penalty=%d)",
			reputationAfter, expectedReputation, reputationBefore, exchange.SmallContentReputationPenalty)
	}

	// --- Verify: seller SmallContentRefundCount incremented ---
	refundCount := eng.State().SellerSmallContentRefundCount(h.seller.PublicKeyHex())
	if refundCount != 1 {
		t.Errorf("step 7: SellerSmallContentRefundCount = %d, want 1", refundCount)
	}

	// --- Verify: buyer scrip refunded (ScripStore path) ---
	// The engine's handleSettleSmallContentDispute should have issued a refund
	// via the scrip store. The buyer's balance should be back to the mint amount
	// (hold cancelled/refunded).
	buyerBalanceAfterDispute := cs.Balance(h.buyer.PublicKeyHex())
	if buyerBalanceAfterDispute != buyerBalanceAfterMint {
		t.Errorf("step 7: buyer balance after dispute = %d, want %d (full refund of hold=%d)",
			buyerBalanceAfterDispute, buyerBalanceAfterMint, holdAmount)
	}
}

// ----------------------------------------------------------------------------
// Assign-pay: operator posts scrip-assign-pay → worker balance credited,
// operator balance decremented
// ----------------------------------------------------------------------------

// TestE2E_AssignPay verifies that posting a dontguess:scrip-assign-pay convention
// message credits the worker's scrip balance and decrements the operator's balance.
//
// Assign-pay is how the exchange pays workers who perform maintenance tasks
// (context compression, validation, freshness checks). The operator posts the
// message; CampfireScripStore.applyAssignPay materializes the balance change.
//
// Done conditions:
//   - worker balance after replay = assign amount
//   - operator balance after replay = mint - assign amount
//   - scrip-assign-pay message is in the campfire log with correct payload
//   - fresh CampfireScripStore.Replay() reproduces the same balances
func TestE2E_AssignPay(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	const assignAmount = int64(3000)
	const operatorMint = int64(10000)

	// Mint scrip to the operator so it has a balance to deduct assign-pay from.
	// The operator is h.operator; worker is a separate identity (h.seller used as worker).
	workerKey := h.seller.PublicKeyHex()
	operatorKey := h.operator.PublicKeyHex()

	addScripMintMsg(t, h, operatorKey, operatorMint)

	// Construct the CampfireScripStore after mint messages are in the log.
	cs, err := scrip.NewCampfireScripStore(h.cfID, h.newOperatorClient(), h.operator.PublicKeyHex())
	if err != nil {
		t.Fatalf("NewCampfireScripStore: %v", err)
	}

	operatorBalanceAfterMint := cs.Balance(operatorKey)
	if operatorBalanceAfterMint != operatorMint {
		t.Fatalf("operator balance after mint: got %d, want %d", operatorBalanceAfterMint, operatorMint)
	}
	workerBalanceBefore := cs.Balance(workerKey)
	if workerBalanceBefore != 0 {
		t.Fatalf("worker balance before assign-pay: got %d, want 0", workerBalanceBefore)
	}

	// Post the scrip-assign-pay message to the campfire log.
	// The operator signs this message; the message sender is the operator.
	preAssignPayMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{scrip.TagScripAssignPay}})

	assignPayload, err := json.Marshal(scrip.AssignPayPayload{
		Worker:     workerKey,
		Amount:     assignAmount,
		TaskType:   "validate",
		AssignMsg:  "assign-test-msg-id",
		ResultHash: "sha256:" + fmt.Sprintf("%064x", 999),
	})
	if err != nil {
		t.Fatalf("marshal assign-pay payload: %v", err)
	}
	// The assign-pay message is operator-signed and goes to the campfire log directly.
	assignMsg := h.sendMessage(h.operator, assignPayload,
		[]string{scrip.TagScripAssignPay},
		nil,
	)
	_ = assignMsg

	// Replay the store to pick up the assign-pay message.
	if err := cs.Replay(); err != nil {
		t.Fatalf("Replay after assign-pay: %v", err)
	}

	// Assert: worker balance increased by assignAmount.
	workerBalanceAfter := cs.Balance(workerKey)
	if workerBalanceAfter != assignAmount {
		t.Errorf("worker balance after assign-pay: got %d, want %d", workerBalanceAfter, assignAmount)
	}

	// Assert: operator balance decreased by assignAmount.
	operatorBalanceAfter := cs.Balance(operatorKey)
	if operatorBalanceAfter != operatorMint-assignAmount {
		t.Errorf("operator balance after assign-pay: got %d, want %d (mint=%d - assign=%d)",
			operatorBalanceAfter, operatorMint-assignAmount, operatorMint, assignAmount)
	}

	// Assert: scrip-assign-pay message is in the campfire log.
	afterAssignPayMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{scrip.TagScripAssignPay}})
	if len(afterAssignPayMsgs) <= len(preAssignPayMsgs) {
		t.Fatal("scrip-assign-pay message not found in campfire log")
	}

	// Verify the payload fields.
	logMsg := afterAssignPayMsgs[len(afterAssignPayMsgs)-1]
	var ap scrip.AssignPayPayload
	if err := json.Unmarshal(logMsg.Payload, &ap); err != nil {
		t.Fatalf("parsing scrip-assign-pay payload: %v", err)
	}
	if ap.Worker != workerKey {
		t.Errorf("assign-pay worker = %q, want %q", ap.Worker, workerKey)
	}
	if ap.Amount != assignAmount {
		t.Errorf("assign-pay amount = %d, want %d", ap.Amount, assignAmount)
	}
	if ap.TaskType != "validate" {
		t.Errorf("assign-pay task_type = %q, want %q", ap.TaskType, "validate")
	}
	if logMsg.Sender != operatorKey {
		t.Errorf("assign-pay sender = %q, want operator %q", logMsg.Sender, operatorKey)
	}

	// Fresh replay: verify a new CampfireScripStore derives the same balances.
	freshCS, err := scrip.NewCampfireScripStore(h.cfID, h.newOperatorClient(), h.operator.PublicKeyHex())
	if err != nil {
		t.Fatalf("NewCampfireScripStore (fresh replay): %v", err)
	}
	if freshCS.Balance(workerKey) != assignAmount {
		t.Errorf("fresh replay worker balance = %d, want %d", freshCS.Balance(workerKey), assignAmount)
	}
	if freshCS.Balance(operatorKey) != operatorMint-assignAmount {
		t.Errorf("fresh replay operator balance = %d, want %d",
			freshCS.Balance(operatorKey), operatorMint-assignAmount)
	}
}

// ----------------------------------------------------------------------------
// Preview-before-purchase happy path:
//
//	put → put-accept → buy → match → settle(preview-request) →
//	settle(preview) → settle(buyer-accept) → settle(deliver) →
//	settle(complete)
//
// This test exercises the complete preview flow end-to-end with a real engine,
// real campfire store, and a CampfireScripStore wired in for scrip verification.
//
// Done conditions verified at each step:
//  1. After buy:             buyer scrip NOT decremented (no pre-hold at buy time)
//  2. After match:           results include the entry; no reservation_id in payload
//  3. After preview-request: previewsByEntry populated; previewCountByMatch incremented
//  4. After preview:         preview payload contains entry_id; previewToMatch populated
//  5. After buyer-accept:    scrip hold created; match accepted in state
//  6. After deliver:         delivered state tracked (IsMatchDelivered)
//  7. After complete:        seller reputation updated; price history record created;
//     scrip settled (seller residual, operator revenue, fee burned)
// ----------------------------------------------------------------------------

func TestE2E_PreviewBeforePurchaseHappyPath(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)

	// Wire a real CampfireScripStore so we can verify scrip movement.
	cs := newCampfireScripStore(t, h)
	eng := exchange.NewEngine(exchange.EngineOptions{
		CampfireID:       h.cfID,
		Store:            h.st,
		ReadClient:  h.newOperatorClient(),
		WriteClient:      h.newOperatorClient(),
		ScripStore:       cs,
		Logger:           func(format string, args ...any) { t.Logf("[engine] "+format, args...) },
	})

	// --- Step 1: Seller puts content large enough for preview (token_cost >= 500) ---
	// put_price = 7000 → sale_price = 8400, fee = 840, hold = 9240
	putMsg := h.sendMessage(h.seller,
		putPayload("Go HTTP handler integration test suite", "sha256:"+fmt.Sprintf("%064x", 9001), "code", 12000, 50000),
		[]string{exchange.TagPut, "exchange:content-type:code", "exchange:domain:go"},
		nil,
	)

	msgs, err := h.st.ListMessages(h.cfID, 0)
	if err != nil {
		t.Fatalf("step 1: listing messages: %v", err)
	}
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	if inv := eng.State().Inventory(); len(inv) != 0 {
		t.Errorf("step 1: expected empty inventory before put-accept, got %d", len(inv))
	}

	// --- Step 2: Exchange auto-accepts the put ---
	const putPrice = int64(7000)
	if err := eng.AutoAcceptPut(putMsg.ID, putPrice, time.Now().Add(168*time.Hour)); err != nil {
		t.Fatalf("step 2: AutoAcceptPut: %v", err)
	}

	inv := eng.State().Inventory()
	if len(inv) != 1 {
		t.Fatalf("step 2: expected 1 inventory entry after put-accept, got %d", len(inv))
	}
	entry := inv[0]
	if entry.PutMsgID != putMsg.ID {
		t.Errorf("step 2: inventory entry PutMsgID = %q, want %q", entry.PutMsgID, putMsg.ID)
	}

	// Compute expected scrip amounts.
	salePrice := eng.ComputePriceForTest(entry)
	fee := salePrice / exchange.MatchingFeeRate
	holdAmount := salePrice + fee
	enginePrice := holdAmount * exchange.MatchingFeeRate / (exchange.MatchingFeeRate + 1)
	expectedResidual := enginePrice / exchange.ResidualRate
	expectedExchangeRevenue := enginePrice - expectedResidual

	// Mint scrip for the buyer so it can afford the purchase.
	const buyerExtra = int64(3000)
	addScripMintMsg(t, h, h.buyer.PublicKeyHex(), holdAmount+buyerExtra)
	if err := cs.Replay(); err != nil {
		t.Fatalf("step 2: cs.Replay after mint: %v", err)
	}
	buyerBalanceAfterMint := cs.Balance(h.buyer.PublicKeyHex())
	if buyerBalanceAfterMint != holdAmount+buyerExtra {
		t.Errorf("step 2 (mint): buyer balance = %d, want %d", buyerBalanceAfterMint, holdAmount+buyerExtra)
	}

	// --- Step 3: Buyer sends buy request ---
	preBuyMatchMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() { _ = eng.Start(ctx) }()

	h.sendMessage(h.buyer,
		buyPayload("Write integration tests for a Go HTTP handler with JSON validation", salePrice+5000),
		[]string{exchange.TagBuy},
		nil,
	)

	// --- Step 4: Engine emits match ---
	matchMsg := waitForMatchMessage(t, h, preBuyMatchMsgs, 2*time.Second)
	cancel()

	// Step 3 assertion: buyer scrip NOT decremented at buy time.
	buyerBalanceAfterBuy := cs.Balance(h.buyer.PublicKeyHex())
	if buyerBalanceAfterBuy != buyerBalanceAfterMint {
		t.Errorf("step 3 (buy): buyer balance = %d after buy, want %d (no pre-decrement at buy time)",
			buyerBalanceAfterBuy, buyerBalanceAfterMint)
	}

	// Step 4 assertions: match includes entry; no reservation_id in payload.
	if len(matchMsg.Antecedents) == 0 {
		t.Error("step 4: match has no antecedents")
	}
	var matchPayload struct {
		Results []struct {
			EntryID    string  `json:"entry_id"`
			Price      int64   `json:"price"`
			Confidence float64 `json:"confidence"`
		} `json:"results"`
		SearchMeta struct {
			ReservationID string `json:"reservation_id"`
		} `json:"search_meta"`
	}
	if err := json.Unmarshal(matchMsg.Payload, &matchPayload); err != nil {
		t.Fatalf("step 4: parsing match payload: %v", err)
	}
	if len(matchPayload.Results) == 0 {
		t.Fatal("step 4: match has no results — entry not returned")
	}
	if matchPayload.Results[0].EntryID != entry.EntryID {
		t.Errorf("step 4: match result entry_id = %q, want %q", matchPayload.Results[0].EntryID, entry.EntryID)
	}
	if matchPayload.SearchMeta.ReservationID != "" {
		t.Error("step 4: match search_meta.reservation_id must be empty — scrip hold not created at buy time")
	}

	// Sync state to pick up the match.
	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	// --- Step 5: Buyer sends settle(preview-request) with match as antecedent ---
	preqMsg := h.sendMessage(h.buyer,
		previewRequestPayload(entry.EntryID),
		[]string{exchange.TagSettle, exchange.TagPhasePrefix + exchange.SettlePhaseStrPreviewRequest},
		[]string{matchMsg.ID},
	)
	preqRec, err := h.st.GetMessage(preqMsg.ID)
	if err != nil {
		t.Fatalf("step 5: GetMessage preview-request: %v", err)
	}
	eng.State().Apply(exchange.FromStoreRecord(preqRec))

	// Step 5 assertions: previewsByEntry, previewCountByMatch, previewRequestToMatch populated.
	byEntry := eng.State().PreviewsByEntryForTest()
	if byBuyer, ok := byEntry[entry.EntryID]; !ok {
		t.Errorf("step 5: previewsByEntry[%s] not set after preview-request", entry.EntryID[:8])
	} else if got := byBuyer[h.buyer.PublicKeyHex()]; got != matchMsg.ID {
		t.Errorf("step 5: previewsByEntry[entry][buyer] = %q, want match %q", got, matchMsg.ID)
	}

	countByMatch := eng.State().PreviewCountByMatchForTest()
	if got := countByMatch[matchMsg.ID]; got != 1 {
		t.Errorf("step 5: previewCountByMatch[matchID] = %d, want 1", got)
	}

	reqToMatch := eng.State().PreviewRequestToMatchForTest()
	if got := reqToMatch[preqMsg.ID]; got != matchMsg.ID {
		t.Errorf("step 5: previewRequestToMatch[preqMsgID] = %q, want %q", got, matchMsg.ID)
	}

	// --- Step 6: Engine emits settle(preview) in response to preview-request ---
	preSettleMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagSettle}})
	preCount := len(preSettleMsgs)

	if err := eng.DispatchForTest(exchange.FromStoreRecord(preqRec)); err != nil {
		t.Fatalf("step 6: DispatchForTest preview-request: %v", err)
	}

	postSettleMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagSettle}})
	if len(postSettleMsgs) <= preCount {
		t.Fatal("step 6: engine did not emit a settle(preview) response")
	}

	// Find the emitted preview message.
	var previewMsg *store.MessageRecord
	for i := range postSettleMsgs {
		m := &postSettleMsgs[i]
		for _, tag := range m.Tags {
			if tag == exchange.TagPhasePrefix+exchange.SettlePhaseStrPreview {
				previewMsg = m
				break
			}
		}
		if previewMsg != nil {
			break
		}
	}
	if previewMsg == nil {
		t.Fatal("step 6: no settle(preview) message found after dispatch")
	}

	// Preview antecedent must be the preview-request.
	if len(previewMsg.Antecedents) == 0 || previewMsg.Antecedents[0] != preqMsg.ID {
		t.Errorf("step 6: preview antecedent = %v, want [%s]", previewMsg.Antecedents, preqMsg.ID)
	}

	// Preview payload must include entry_id and chunks field.
	var previewPayload struct {
		EntryID string        `json:"entry_id"`
		Chunks  []interface{} `json:"chunks"`
	}
	if err := json.Unmarshal(previewMsg.Payload, &previewPayload); err != nil {
		t.Fatalf("step 6: parsing preview payload: %v", err)
	}
	if previewPayload.EntryID != entry.EntryID {
		t.Errorf("step 6: preview payload entry_id = %q, want %q", previewPayload.EntryID, entry.EntryID)
	}

	// Apply preview message to state so previewToMatch is populated.
	allMsgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	previewToMatch := eng.State().PreviewToMatchForTest()
	if got := previewToMatch[previewMsg.ID]; got != matchMsg.ID {
		t.Errorf("step 6: previewToMatch[previewMsgID] = %q, want %q", got, matchMsg.ID)
	}

	// --- Step 7: Buyer sends settle(buyer-accept) with preview as antecedent ---
	// Scrip hold is created HERE (not at buy time).
	preBuyHoldMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{scrip.TagScripBuyHold}})

	buyerAcceptMsg := sendBuyerAcceptViaPreview(t, h, eng, previewMsg.ID, entry.EntryID)

	// Step 7 assertions: scrip hold created and buyer balance decremented.
	afterBuyHoldMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{scrip.TagScripBuyHold}})
	if len(afterBuyHoldMsgs) <= len(preBuyHoldMsgs) {
		t.Error("step 7 (buyer-accept): expected scrip-buy-hold message in campfire log after buyer-accept")
	}

	buyerBalanceAfterAccept := cs.Balance(h.buyer.PublicKeyHex())
	if buyerBalanceAfterAccept != buyerBalanceAfterMint-holdAmount {
		t.Errorf("step 7 (buyer-accept): buyer balance = %d, want %d (mint=%d - hold=%d)",
			buyerBalanceAfterAccept, buyerBalanceAfterMint-holdAmount, buyerBalanceAfterMint, holdAmount)
	}

	// Reservation must exist.
	resID := extractReservationIDFromLog(t, h)
	if resID == "" {
		t.Fatal("step 7 (buyer-accept): reservation_id must be non-empty in scrip-buy-hold log")
	}
	if _, err := cs.GetReservation(context.Background(), resID); err != nil {
		t.Fatalf("step 7 (buyer-accept): reservation %s must exist: %v", resID[:8], err)
	}

	// Match must be accepted in state (via preview → match antecedent chain).
	allMsgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))
	if !eng.State().IsMatchAccepted(matchMsg.ID) {
		t.Error("step 7 (buyer-accept): match should be accepted in state after buyer-accept via preview path")
	}

	// --- Step 8: Exchange sends settle(deliver) ---
	deliverMsg := h.sendMessage(h.operator, deliverPayloadFor(entry.EntryID),
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrDeliver,
		},
		[]string{buyerAcceptMsg.ID},
	)

	allMsgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	// Step 8 assertion: delivered state tracked.
	if !eng.State().IsMatchDelivered(matchMsg.ID) {
		t.Error("step 8 (deliver): IsMatchDelivered should be true after deliver message")
	}

	// --- Step 9: Buyer sends settle(complete) ---
	sellerBalanceBefore := cs.Balance(h.seller.PublicKeyHex())
	operatorBalanceBefore := cs.Balance(h.operator.PublicKeyHex())
	preScripSettleMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{scrip.TagScripSettle}})
	preBurnMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{scrip.TagScripBurn}})

	completeP, _ := json.Marshal(map[string]any{"entry_id": entry.EntryID})
	completeMsg := h.sendMessage(h.buyer, completeP,
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrComplete,
		},
		[]string{deliverMsg.ID},
	)

	allMsgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	completeMsgRec, err := h.st.GetMessage(completeMsg.ID)
	if err != nil {
		t.Fatalf("step 9: GetMessage complete: %v", err)
	}
	if err := eng.DispatchForTest(exchange.FromStoreRecord(completeMsgRec)); err != nil {
		t.Fatalf("step 9: DispatchForTest complete: %v", err)
	}

	// --- Step 10: Verify final state ---

	// Seller reputation must be above default (successful sale increments SuccessCount).
	rep := eng.State().SellerReputation(h.seller.PublicKeyHex())
	if rep <= exchange.DefaultReputation {
		t.Errorf("step 10 (complete): seller reputation = %d, want > %d (successful sale)", rep, exchange.DefaultReputation)
	}

	// Price history must have one record.
	history := eng.State().PriceHistory()
	if len(history) != 1 {
		t.Errorf("step 10 (complete): price history len = %d, want 1", len(history))
	} else if history[0].EntryID != entry.EntryID {
		t.Errorf("step 10 (complete): price history entry_id = %q, want %q", history[0].EntryID, entry.EntryID)
	}

	// Scrip settled: seller receives residual, operator receives exchange revenue.
	sellerBalanceAfter := cs.Balance(h.seller.PublicKeyHex())
	if sellerBalanceAfter != sellerBalanceBefore+expectedResidual {
		t.Errorf("step 10 (complete): seller balance = %d, want %d (before=%d + residual=%d)",
			sellerBalanceAfter, sellerBalanceBefore+expectedResidual, sellerBalanceBefore, expectedResidual)
	}

	operatorBalanceAfter := cs.Balance(h.operator.PublicKeyHex())
	if operatorBalanceAfter != operatorBalanceBefore+expectedExchangeRevenue {
		t.Errorf("step 10 (complete): operator balance = %d, want %d (before=%d + revenue=%d)",
			operatorBalanceAfter, operatorBalanceBefore+expectedExchangeRevenue, operatorBalanceBefore, expectedExchangeRevenue)
	}

	// Reservation must be deleted after settle.
	if _, err := cs.GetReservation(context.Background(), resID); err == nil {
		t.Errorf("step 10 (complete): reservation %s must be deleted after settle(complete)", resID[:8])
	}

	// Campfire log must have scrip-settle and scrip-burn messages.
	afterScripSettleMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{scrip.TagScripSettle}})
	if len(afterScripSettleMsgs) <= len(preScripSettleMsgs) {
		t.Error("step 10 (complete): scrip-settle message not found in campfire log")
	}
	afterBurnMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{scrip.TagScripBurn}})
	if len(afterBurnMsgs) <= len(preBurnMsgs) {
		t.Error("step 10 (complete): scrip-burn message not found in campfire log")
	}
}

// sendBuyerAcceptViaPreview sends a settle(buyer-accept) with the preview message
// as antecedent and dispatches it via DispatchForTest, triggering the scrip hold.
// This is the preview-before-purchase path (distinct from the legacy match-antecedent
// path used in sendBuyerAcceptAndDispatch).
func sendBuyerAcceptViaPreview(t *testing.T, h *testHarness, eng *exchange.Engine, previewMsgID, entryID string) *exchange.Message {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{
		"phase":    "buyer-accept",
		"entry_id": entryID,
		"accepted": true,
	})
	msg := h.sendMessage(h.buyer, payload,
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrBuyerAccept,
			exchange.TagVerdictPrefix + "accepted",
		},
		[]string{previewMsgID},
	)
	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))
	rec, err := h.st.GetMessage(msg.ID)
	if err != nil {
		t.Fatalf("GetMessage buyer-accept (preview path): %v", err)
	}
	if err := eng.DispatchForTest(exchange.FromStoreRecord(rec)); err != nil {
		t.Fatalf("DispatchForTest buyer-accept (preview path): %v", err)
	}
	return msg
}

// ----------------------------------------------------------------------------
// E2E content delivery round-trip:
//   put (with real content bytes) → accept → buy → match → preview-request →
//   preview → buyer-accept → operator deliver trigger → engine emits
//   content-bearing deliver → settle(complete)
//
// Assertions:
//   1. Buyer receives deliver message with base64-encoded content whose
//      sha256 matches the original put content bytes.
//   2. Seller scrip balance increases after settle(complete).
//   3. No error-level log entries emitted by the engine.
// ----------------------------------------------------------------------------

// TestE2E_ContentDeliveryRoundTrip verifies the full content delivery contract
// end-to-end using a real engine harness (no mocks):
//
//  1. Seller puts cached inference with known content bytes.
//  2. Operator accepts, buyer buys, engine matches.
//  3. Buyer sends preview-request; engine emits settle(preview).
//  4. Buyer accepts preview; engine creates scrip hold.
//  5. Operator sends settle(deliver) trigger (no content field).
//  6. Engine emits content-bearing settle(deliver) with base64 content.
//  7. Decoded content sha256 matches original.
//  8. Buyer sends settle(complete); seller scrip balance increases.
//  9. Engine emits no error-level log lines.
func TestE2E_ContentDeliveryRoundTrip(t *testing.T) {
	t.Parallel()

	// --- Error log collector ---
	var (
		logMu    sync.Mutex
		errorLog []string
	)
	collectLog := func(format string, args ...any) {
		line := fmt.Sprintf(format, args...)
		t.Logf("[engine] %s", line)
		// Engine logs warnings/errors with keywords: error, cannot, critical, failed.
		lower := strings.ToLower(line)
		if strings.Contains(lower, "error") ||
			strings.Contains(lower, "cannot") ||
			strings.Contains(lower, "critical") ||
			strings.Contains(lower, "failed") {
			logMu.Lock()
			errorLog = append(errorLog, line)
			logMu.Unlock()
		}
	}

	h := newTestHarness(t)

	// Wire a real CampfireScripStore for scrip balance assertions.
	cs := newCampfireScripStore(t, h)
	eng := exchange.NewEngine(exchange.EngineOptions{
		CampfireID:  h.cfID,
		Store:       h.st,
		ReadClient:  h.newOperatorClient(),
		WriteClient: h.newOperatorClient(),
		ScripStore:  cs,
		Logger:      collectLog,
	})

	// --- Step 1: Seller puts cached inference with known content bytes ---
	originalContent := []byte("test content for delivery: Go HTTP handler unit test suite body")
	contentB64 := base64.StdEncoding.EncodeToString(originalContent)
	originalHash := sha256.Sum256(originalContent)

	putPayloadBytes, _ := json.Marshal(map[string]any{
		"description":  "Go HTTP handler unit test suite for E2E content delivery",
		"content":      contentB64,
		"token_cost":   int64(12000),
		"content_type": "code",
		"domains":      []string{"go", "testing"},
	})

	putMsg := h.sendMessage(h.seller, putPayloadBytes,
		[]string{exchange.TagPut, "exchange:content-type:code", "exchange:domain:go"},
		nil,
	)

	msgs, err := h.st.ListMessages(h.cfID, 0)
	if err != nil {
		t.Fatalf("step 1: listing messages: %v", err)
	}
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	// --- Step 2: Operator accepts the put; mint buyer scrip ---
	const putPrice = int64(8400)
	if err := eng.AutoAcceptPut(putMsg.ID, putPrice, time.Now().Add(168*time.Hour)); err != nil {
		t.Fatalf("step 2: AutoAcceptPut: %v", err)
	}

	inv := eng.State().Inventory()
	if len(inv) != 1 {
		t.Fatalf("step 2: expected 1 inventory entry after put-accept, got %d", len(inv))
	}
	entry := inv[0]

	// Compute expected scrip amounts.
	salePrice := eng.ComputePriceForTest(entry)
	fee := salePrice / exchange.MatchingFeeRate
	holdAmount := salePrice + fee
	enginePrice := holdAmount * exchange.MatchingFeeRate / (exchange.MatchingFeeRate + 1)
	expectedResidual := enginePrice / exchange.ResidualRate

	// Mint put-pay for seller and purchase funds for buyer.
	const discountPct = int64(30)
	addScripPutPayMsg(t, h, putMsg.ID, h.seller.PublicKeyHex(), putPrice, int64(12000), discountPct, entry.ContentHash)
	addScripMintMsg(t, h, h.buyer.PublicKeyHex(), holdAmount+1000)
	if err := cs.Replay(); err != nil {
		t.Fatalf("step 2: cs.Replay after mint: %v", err)
	}

	sellerBalanceAfterPutPay := cs.Balance(h.seller.PublicKeyHex())
	if sellerBalanceAfterPutPay != putPrice {
		t.Errorf("step 2 (put-pay): seller balance = %d, want %d", sellerBalanceAfterPutPay, putPrice)
	}

	// --- Step 3: Buyer buys; engine matches ---
	preMatchMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	h.startEngine(eng, ctx, cancel)

	h.sendMessage(h.buyer,
		buyPayload("Generate unit tests for a Go HTTP handler accepting JSON POST with validation", salePrice+5000),
		[]string{exchange.TagBuy},
		nil,
	)

	matchMsg := waitForMatchMessage(t, h, preMatchMsgs, 2*time.Second)
	cancel()

	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	// --- Step 4: Buyer sends preview-request; engine emits settle(preview) ---
	preqPayload, _ := json.Marshal(map[string]any{
		"phase":    "preview-request",
		"entry_id": entry.EntryID,
	})
	preqMsg := h.sendMessage(h.buyer, preqPayload,
		[]string{exchange.TagSettle, exchange.TagPhasePrefix + exchange.SettlePhaseStrPreviewRequest},
		[]string{matchMsg.ID},
	)
	preqRec, _ := h.st.GetMessage(preqMsg.ID)
	eng.State().Apply(exchange.FromStoreRecord(preqRec))

	if err := eng.DispatchForTest(exchange.FromStoreRecord(preqRec)); err != nil {
		t.Fatalf("step 4: DispatchForTest preview-request: %v", err)
	}

	allMsgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	previewMsgs, _ := h.st.ListMessages(h.cfID, 0,
		store.MessageFilter{Tags: []string{exchange.TagSettle, exchange.TagPhasePrefix + exchange.SettlePhaseStrPreview}})
	if len(previewMsgs) == 0 {
		t.Fatal("step 4: no preview message emitted")
	}
	previewRec := previewMsgs[len(previewMsgs)-1]

	// --- Step 5: Buyer accepts preview; scrip hold created ---
	buyerAcceptMsg := sendBuyerAcceptViaPreview(t, h, eng, previewRec.ID, entry.EntryID)

	allMsgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	if !eng.State().IsMatchAccepted(matchMsg.ID) {
		t.Error("step 5: match should be accepted in state after buyer-accept via preview")
	}

	// --- Step 6: Operator sends deliver trigger (no content field) ---
	deliverTriggerPayload, _ := json.Marshal(map[string]any{
		"phase":        "deliver",
		"entry_id":     entry.EntryID,
		"content_ref":  entry.ContentHash,
		"content_size": len(originalContent),
	})
	deliverTriggerMsg := h.sendMessage(h.operator, deliverTriggerPayload,
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrDeliver,
		},
		[]string{buyerAcceptMsg.ID},
	)

	allMsgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	// Engine dispatches deliver trigger → emits content-bearing deliver.
	preMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagSettle}})
	preCount := len(preMsgs)

	deliverRec, _ := h.st.GetMessage(deliverTriggerMsg.ID)
	if err := eng.DispatchForTest(exchange.FromStoreRecord(deliverRec)); err != nil {
		t.Fatalf("step 6: DispatchForTest deliver trigger: %v", err)
	}

	// --- Step 7: Assert buyer receives content with correct sha256 ---
	postMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagSettle}})
	if len(postMsgs) <= preCount {
		t.Fatal("step 7: engine did not emit a new settle message after deliver trigger")
	}

	// Find the engine-emitted content-bearing deliver message.
	var contentMsg *store.MessageRecord
	for i := range postMsgs {
		m := &postMsgs[i]
		if m.Sender != h.operator.PublicKeyHex() {
			continue
		}
		hasDeliverPhase := false
		for _, tag := range m.Tags {
			if tag == exchange.TagPhasePrefix+exchange.SettlePhaseStrDeliver {
				hasDeliverPhase = true
				break
			}
		}
		if !hasDeliverPhase {
			continue
		}
		var p map[string]any
		if err := json.Unmarshal(m.Payload, &p); err != nil {
			continue
		}
		if _, hasContent := p["content"]; hasContent {
			contentMsg = m
			break
		}
	}
	if contentMsg == nil {
		t.Fatal("step 7: engine did not emit a settle(deliver) message with content field")
	}

	// Decode and verify sha256.
	var deliverPayload struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(contentMsg.Payload, &deliverPayload); err != nil {
		t.Fatalf("step 7: unmarshal deliver content payload: %v", err)
	}
	deliveredBytes, err := base64.StdEncoding.DecodeString(deliverPayload.Content)
	if err != nil {
		t.Fatalf("step 7: base64-decode delivered content: %v", err)
	}
	deliveredHash := sha256.Sum256(deliveredBytes)

	// Assertion 1: delivered content sha256 matches original bytes.
	if originalHash != deliveredHash {
		t.Errorf("step 7 (sha256 match): delivered content hash mismatch:\n  got  sha256:%x\n  want sha256:%x",
			deliveredHash, originalHash)
	}

	allMsgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	// --- Step 8: Buyer sends settle(complete); seller scrip balance increases ---
	// Antecedent for settle(complete) is the operator's deliver trigger message
	// (deliverTriggerMsg), not the engine-emitted content message. The state
	// machine uses deliverToMatch which maps the operator trigger ID → match ID.
	sellerBalanceBefore := cs.Balance(h.seller.PublicKeyHex())

	completeP, _ := json.Marshal(map[string]any{"entry_id": entry.EntryID})
	completeMsg := h.sendMessage(h.buyer, completeP,
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrComplete,
		},
		[]string{deliverTriggerMsg.ID},
	)

	allMsgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	completeMsgRec, err := h.st.GetMessage(completeMsg.ID)
	if err != nil {
		t.Fatalf("step 8: GetMessage complete: %v", err)
	}
	if err := eng.DispatchForTest(exchange.FromStoreRecord(completeMsgRec)); err != nil {
		t.Fatalf("step 8: DispatchForTest complete: %v", err)
	}

	// Assertion 2: seller scrip balance increased after settle(complete).
	sellerBalanceAfter := cs.Balance(h.seller.PublicKeyHex())
	if sellerBalanceAfter != sellerBalanceBefore+expectedResidual {
		t.Errorf("step 8 (seller scrip): seller balance = %d, want %d (before=%d + residual=%d)",
			sellerBalanceAfter, sellerBalanceBefore+expectedResidual, sellerBalanceBefore, expectedResidual)
	}

	// Assertion 3: no engine error log entries.
	logMu.Lock()
	errs := append([]string(nil), errorLog...)
	logMu.Unlock()
	if len(errs) > 0 {
		t.Errorf("step 9 (no errors): engine emitted %d error-level log lines:\n  %s",
			len(errs), strings.Join(errs, "\n  "))
	}
}
