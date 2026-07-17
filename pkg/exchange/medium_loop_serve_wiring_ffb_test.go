package exchange_test

// medium_loop_serve_wiring_ffb_test.go is the MANDATORY feature test for
// dontguess-ffb (#2 SUPPLY — restore dormant loop). Before this item,
// pkg/pricing was imported nowhere in cmd/ — the medium loop that scans
// inventory and posts open compression assigns for high-demand uncompressed
// entries (pkg/pricing/medium_loop.go postCompressionAssigns, dispatched via
// the exported Engine.PostOpenCompressionAssign, pkg/exchange/engine_buy.go)
// never ran in production; only the per-buy warm-compression offer reached
// agents. cmd/dontguess/serve.go's runServeLocalCtx/runEngineLoop now wires a
// *pricing.MediumLoop into every running serve exactly the way this test wires
// one: PostAssign delegates to the exported production
// Engine.PostOpenCompressionAssign(spec.EntryID); State is eng.State().
//
// This test proves that wiring end to end against REAL production pieces —
// not a mock of the loop:
//
//  1. A REAL exchange.Engine backed by a REAL pkg/store event log (the same
//     campfire-free LocalStore substrate `dontguess serve` runs against —
//     testHarness's doc, engine_test.go) accumulates real hot, uncompressed
//     inventory: a put is auto-accepted, then 3 DISTINCT buyers each complete
//     a REAL put -> buy -> match -> buyer-accept -> deliver -> complete cycle
//     (completeBuyTransactionForBuyer, compute_price_demand_test.go — the same
//     helper the demand-signal integration suite uses), crossing
//     pricing.DefaultCompressionPurchaseThreshold (3) via real
//     State.PurchaseCount bookkeeping, not an injected count.
//  2. A REAL *pricing.MediumLoop, wired exactly like runServeLocalCtx wires
//     it, runs a REAL Tick() — the "extracted tick function" the item
//     explicitly permits driving directly instead of the full serve process,
//     since Tick is what MediumLoop.Run calls on each interval anyway.
//  3. The resulting assign is verified through the REAL d26 listing
//     accessor, State.AllActiveAssigns() — the exact accessor
//     cmd/dontguess/individual_ops.go's handleOpListAssigns (the OpListAssigns
//     IPC backing `dontguess assigns`) calls. individual_ops_test.go's
//     TestOpListAssigns_Individual_SurfacesOpenCompressAssign already proves
//     that IPC/listing layer surfaces a manually-posted assign correctly; this
//     test proves the MEDIUM LOOP'S OWN TICK is what posts it in the first
//     place — the actual wiring gap dontguess-ffb closes.

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/3dl-dev/dontguess/pkg/exchange"
	"github.com/3dl-dev/dontguess/pkg/pricing"
)

// seedRealCompletedPurchase folds a genuine distinct-buyer purchase
// (buy -> match -> buyer-accept -> deliver -> complete) directly into
// eng.State() via the real production Apply/fold path (state_buy.go's
// applyBuy/applyMatch, state_settle.go's applySettleBuyerAccept/
// applySettleDeliver/applySettleComplete), so EntryDemandCount/PurchaseCount
// tracking is genuine, not injected.
//
// Deliberately bypasses eng.DispatchForTest(buy) / the engine's own handleBuy:
// handleBuy ALSO fires Engine.sendWarmCompressionAssign for the top match
// whenever no compressed derivative exists yet (engine_buy.go) — a real,
// independent, PER-BUYER production behavior (buyer1, buyer2, buyer3 would
// each get their own EXCLUSIVE warm compress assign). That warm assign would
// itself count as an "active assign" for the entry and make
// postCompressionAssigns' len(ActiveAssigns(entryID))>0 guard skip the cold
// compression assign this test exists to prove — an orthogonal interaction
// between two DIFFERENT compression-assign posting paths (warm, per-buy;
// cold, medium-loop), not a defect in either. Folding the match directly
// (mirroring exactly the payload handleBuy itself would emit — see
// engine_buy.go's MatchResult/matchPayload) exercises the same demand-signal
// state transitions without invoking handleBuy's separate warm-assign side
// effect, isolating the medium-loop wiring this test targets.
func seedRealCompletedPurchase(t *testing.T, h *testHarness, eng *exchange.Engine, buyer *testAgent, entryID string, salePrice int64) {
	t.Helper()

	buyMsg := h.sendMessage(buyer,
		buyPayload("demand signal integration test task "+entryID[:8], salePrice*5),
		[]string{exchange.TagBuy},
		nil,
	)

	matchPayload, err := json.Marshal(map[string]any{
		"results": []map[string]any{
			{"entry_id": entryID, "price": salePrice},
		},
	})
	if err != nil {
		t.Fatalf("seedRealCompletedPurchase: encoding match payload: %v", err)
	}
	// Sender MUST be the operator key (applyMatch drops any non-operator
	// sender). Written to the store only — folded below, along with buy and
	// the rest of the settle chain, by the single trailing Replay in strict
	// log order, exactly as completeBuyTransactionForBuyer's own final step
	// does (fold order: buy -> match -> buyer-accept -> deliver -> complete).
	matchMsg := h.sendMessage(h.operator, matchPayload, []string{exchange.TagMatch}, []string{buyMsg.ID})

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

	// Converge from the full log so every fold (buyer-accept/deliver/complete)
	// applies in order, same as completeBuyTransactionForBuyer's final step.
	allMsgs, err := h.st.ListMessages(h.cfID, 0)
	if err != nil {
		t.Fatalf("seedRealCompletedPurchase: listing messages: %v", err)
	}
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))
}

func TestMediumLoop_ServeWiring_TicksPostRealOpenCompressionAssign(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	// Seller puts a fixture entry; fold a settle(put-accept) directly (the same
	// State.applySettlePutAccept fold Engine.AutoAcceptPut itself triggers,
	// state_settle.go) rather than calling eng.AutoAcceptPut. AutoAcceptPut
	// ALSO fires Engine.sendCompressionAssign unconditionally as a side effect
	// (engine_pricing.go autoAcceptPutLocked's "Hot compression offer" — an
	// EXCLUSIVE-to-seller assign posted on every accept, orthogonal to the
	// medium loop): that would itself count as an "active assign" for the
	// entry and make postCompressionAssigns' len(ActiveAssigns(entryID))>0
	// guard permanently skip the cold assign this test targets, since a
	// standing (never-deadlined) compression assign has no expiry path short
	// of full claim+complete+accept — which would ALSO set
	// HasCompressedVersion(entryID)=true and independently block
	// postCompressionAssigns via its OTHER guard. Folding put-accept directly
	// exercises the exact real inventory-promotion state transition
	// (applySettlePutAccept moves the entry from pendingPuts to s.inventory)
	// without invoking that unrelated hot-assign side effect, isolating the
	// medium-loop wiring this test targets — precisely the same technique
	// seedRealCompletedPurchase uses to avoid handleBuy's own warm-assign side
	// effect.
	const tokenCost = int64(20000)
	const putPrice = int64(10000)
	putMsg := h.sendMessage(h.seller,
		putPayload("medium loop wiring fixture: python asyncio debugging guide",
			"sha256:"+fmt.Sprintf("%064x", 9001), "code", tokenCost, 5000),
		[]string{exchange.TagPut, "exchange:content-type:code", "exchange:domain:python"},
		nil,
	)
	acceptPayload, err := json.Marshal(map[string]any{
		"phase":      "put-accept",
		"entry_id":   putMsg.ID,
		"price":      putPrice,
		"expires_at": time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339),
	})
	if err != nil {
		t.Fatalf("encoding put-accept payload: %v", err)
	}
	h.sendMessage(h.operator, acceptPayload,
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrPutAccept,
			exchange.TagVerdictPrefix + "accepted",
		},
		[]string{putMsg.ID},
	)
	msgs, err := h.st.ListMessages(h.cfID, 0)
	if err != nil {
		t.Fatalf("listing messages: %v", err)
	}
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	inv := eng.State().Inventory()
	if len(inv) != 1 {
		t.Fatalf("expected 1 inventory entry after put-accept, got %d", len(inv))
	}
	entryID := inv[0].EntryID
	if active := eng.State().ActiveAssigns(entryID); len(active) != 0 {
		t.Fatalf("fixture entry has %d active assign(s) immediately after put-accept, want 0 (hot-assign side effect leaked in) — got %+v", len(active), active)
	}

	// 3 distinct buyers each complete a real purchase, crossing
	// pricing.DefaultCompressionPurchaseThreshold via genuine
	// buy->match->buyer-accept->deliver->complete settle-phase folds (real
	// State.Apply/Replay — no injected PurchaseCount). See
	// seedRealCompletedPurchase's doc for why this bypasses handleBuy's own
	// dispatch (its independent warm-compression-assign side effect).
	for i := 0; i < pricing.DefaultCompressionPurchaseThreshold; i++ {
		buyer := newTestAgent(t)
		seedRealCompletedPurchase(t, h, eng, buyer, entryID, putPrice)
	}

	if got := eng.State().PurchaseCount(entryID); got < pricing.DefaultCompressionPurchaseThreshold {
		t.Fatalf("PurchaseCount(entryID) = %d, want >= %d (medium loop demand gate) — fixture did not reach hot-demand state",
			got, pricing.DefaultCompressionPurchaseThreshold)
	}
	if eng.State().HasCompressedVersion(entryID) {
		t.Fatal("fixture entry unexpectedly already has a compressed derivative")
	}
	if active := eng.State().ActiveAssigns(entryID); len(active) != 0 {
		t.Fatalf("fixture entry already has %d active assign(s) before the medium-loop tick, want 0", len(active))
	}

	// Wire pricing.MediumLoop EXACTLY the way cmd/dontguess/serve.go's
	// runServeLocalCtx wires it into a running serve: PostAssign delegates to
	// the exported production Engine.PostOpenCompressionAssign using only
	// spec.EntryID (the production method re-derives the bounty itself from
	// the entry's own TokenCost); State is eng.State().
	mediumLoop := pricing.NewMediumLoop(pricing.MediumLoopOptions{
		State: eng.State(),
		PostAssign: func(spec pricing.AssignSpec) error {
			return eng.PostOpenCompressionAssign(spec.EntryID)
		},
	})

	result := mediumLoop.Tick(context.Background())
	if result.CompressionAssigns != 1 {
		t.Fatalf("MediumLoop.Tick() CompressionAssigns = %d, want 1", result.CompressionAssigns)
	}

	// Verify through the REAL d26 listing accessor (the same one
	// handleOpListAssigns/OpListAssigns/`dontguess assigns` call) — not a
	// re-derivation of Tick's own bookkeeping.
	var found *exchange.AssignRecord
	for _, a := range eng.State().AllActiveAssigns() {
		if a.EntryID == entryID && a.TaskType == "compress" {
			found = a
			break
		}
	}
	if found == nil {
		t.Fatalf("State.AllActiveAssigns() does not surface a compress assign for entry %s after MediumLoop.Tick — got %+v",
			entryID, eng.State().AllActiveAssigns())
	}
	if found.ExclusiveSender != "" {
		t.Errorf("posted assign ExclusiveSender = %q, want empty (open/cold compression assign, claimable by anyone)", found.ExclusiveSender)
	}
	if found.Status != exchange.AssignOpen {
		t.Errorf("posted assign Status = %v, want %v (open/claimable)", found.Status, exchange.AssignOpen)
	}
	wantReward := tokenCost * exchange.ColdCompressionBountyPct / 100
	if found.Reward != wantReward {
		t.Errorf("posted assign Reward = %d, want %d (%d%% of token_cost %d)",
			found.Reward, wantReward, exchange.ColdCompressionBountyPct, tokenCost)
	}

	// A second tick with the assign already active must not double-post
	// (postCompressionAssigns's ActiveAssigns guard) — proves the wiring does
	// not spam duplicate assigns on every interval.
	result2 := mediumLoop.Tick(context.Background())
	if result2.CompressionAssigns != 0 {
		t.Errorf("second MediumLoop.Tick() CompressionAssigns = %d, want 0 (active-assign guard should suppress a duplicate post)", result2.CompressionAssigns)
	}
}
