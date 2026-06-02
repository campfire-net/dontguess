package exchange_test

// Tests for the exchange-level relevance floor enforcement (dontguess-720).
//
// Root cause (7d6 follow-up): pkg/exchange/engine.go handleBuy's fallback path
// was serving below-floor EMBEDDED entries as HITs instead of MISSES. The
// matching.Rank() floor (0.16) correctly excluded these entries from
// semanticResults, but the fallback loop re-admitted any candidate NOT in the
// "covered" set — which included below-floor embedded entries. Only genuine
// index-gap entries (not yet embedded) should use the fallback path.
//
// Fix: matchIndex.HasEmbedding(entryID) gates the fallback. Entries with an
// embedding that scored below the floor are excluded from fallbackCandidates
// so the buy correctly emits exchange:buy-miss.
//
// Covered:
//  1. TestBelowFloor_JunkOnlyInventory_YieldsMiss — junk-only inventory
//     ("upgrade smoke test cf v0.31.2 operator", sim ~0.116 vs an unrelated
//     buy task) must emit exchange:buy-miss, not exchange:match. This was
//     FAILING before the fix (served a junk hit via fallback).
//  2. TestAboveFloor_SemanticMatch_YieldsHit — above-floor inventory still
//     delivers a hit (the floor fix must not turn all results into misses).

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/campfire-net/campfire/cf-protocol/store"

	"github.com/campfire-net/dontguess/pkg/exchange"
)

// TestBelowFloor_JunkOnlyInventory_YieldsMiss verifies that when the only
// inventory entry is below the relevance floor (cosine similarity < 0.16 vs the
// buy task), the engine emits exchange:buy-miss instead of exchange:match.
//
// This is the primary done-condition regression test for dontguess-720.
// It was FAILING before the fix: the handleBuy fallback admitted the below-floor
// entry as a candidate because it was not in the "covered" semantic set, then
// served it as a hit via reputation+recency ranking.
//
// Uses the real engine path (DispatchForTest / real store). No mocks.
func TestBelowFloor_JunkOnlyInventory_YieldsMiss(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	// Seed the junk entry that caused 60% of false hits in the live exchange (D1 fixture).
	// This entry's TF-IDF similarity to any unrelated task is ~0.116, below the 0.16 floor.
	junkDesc := "upgrade smoke test cf v0.31.2 operator"
	putMsg := h.sendMessage(h.seller,
		putPayload(junkDesc, "sha256:aabbccdd00000000000000000000000000000000000000000000000000000001", "analysis", 100, 512),
		[]string{exchange.TagPut, "exchange:content-type:analysis"},
		nil,
	)

	// Replay and accept the put.
	msgs, err := h.st.ListMessages(h.cfID, 0)
	if err != nil {
		t.Fatalf("listing messages: %v", err)
	}
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	if err := eng.AutoAcceptPut(putMsg.ID, 84, time.Now().Add(72*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut (junk): %v", err)
	}

	inv := eng.State().Inventory()
	if len(inv) != 1 {
		t.Fatalf("expected 1 inventory entry, got %d", len(inv))
	}

	// Buy task is semantically unrelated to the junk description.
	// This task is from the D1 fixture (§2 nonsense pairings): it matched the
	// junk entry in the live exchange before the fix.
	unrelatedTask := "RPT review of campfire SDK surface: offline send, relay create, naming CLI, multi-op install"

	preMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})
	preMatchCount := len(preMsgs)

	buyMsg := h.sendMessage(h.buyer,
		buyPayload(unrelatedTask, 50000),
		[]string{exchange.TagBuy},
		nil,
	)
	buyRec, err := h.st.GetMessage(buyMsg.ID)
	if err != nil {
		t.Fatalf("getting buy message: %v", err)
	}

	// Apply buy to state and dispatch through the engine (real path, no event loop).
	eng.State().Apply(exchange.FromStoreRecord(buyRec))
	if err := eng.DispatchForTest(exchange.FromStoreRecord(buyRec)); err != nil {
		t.Fatalf("DispatchForTest: %v", err)
	}

	// Reload state to pick up the emitted message.
	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	// Exactly one new match-tagged message must have been emitted.
	postMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})
	if len(postMsgs) <= preMatchCount {
		t.Fatal("engine emitted no response — expected at least one message with exchange:match tag")
	}
	emitted := postMsgs[len(postMsgs)-1]

	// PRIMARY ASSERTION: the emitted message must have exchange:buy-miss tag.
	// Before the fix this assertion FAILED — the engine served a junk hit.
	if !hasTag(emitted.Tags, exchange.TagBuyMiss) {
		// Parse the match payload to give a useful failure message.
		var mp struct {
			Results []struct {
				EntryID    string  `json:"entry_id"`
				Confidence float64 `json:"confidence"`
			} `json:"results"`
		}
		_ = json.Unmarshal(emitted.Payload, &mp)
		t.Errorf("DEFECT (dontguess-720): below-floor junk entry served as HIT, want exchange:buy-miss. "+
			"Tags: %v. Match results: %v",
			emitted.Tags, mp.Results)
	}

	// Must have antecedent pointing to the buy message (future fulfillment).
	if len(emitted.Antecedents) == 0 || emitted.Antecedents[0] != buyMsg.ID {
		t.Errorf("buy-miss antecedent = %v, want [%s]", emitted.Antecedents, buyMsg.ID)
	}

	// Sender must be the operator.
	if emitted.Sender != h.operator.PublicKeyHex() {
		t.Errorf("buy-miss sender = %q, want operator %q", emitted.Sender, h.operator.PublicKeyHex())
	}
}

// TestAboveFloor_SemanticMatch_YieldsHit verifies that an above-floor inventory
// entry still delivers a genuine hit after the below-floor gate is applied.
//
// This is the complement to TestBelowFloor_JunkOnlyInventory_YieldsMiss: it
// proves the fix only rejects below-floor entries and does not break real matches.
//
// Uses the same real engine path (DispatchForTest). No mocks.
func TestAboveFloor_SemanticMatch_YieldsHit(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	// Seed an above-floor entry: the put description and buy task share enough
	// domain vocabulary that TF-IDF cosine similarity will be well above 0.16.
	putDesc := "EventSink contract for warm-worker backends: emit accepted infer completed events"
	putMsg := h.sendMessage(h.seller,
		putPayload(putDesc, "sha256:aabbccdd00000000000000000000000000000000000000000000000000000002", "code", 4000, 8000),
		[]string{exchange.TagPut, "exchange:content-type:code", "exchange:domain:go"},
		nil,
	)

	msgs, err := h.st.ListMessages(h.cfID, 0)
	if err != nil {
		t.Fatalf("listing messages: %v", err)
	}
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	if err := eng.AutoAcceptPut(putMsg.ID, 3355, time.Now().Add(72*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut: %v", err)
	}

	inv := eng.State().Inventory()
	if len(inv) != 1 {
		t.Fatalf("expected 1 inventory entry, got %d", len(inv))
	}

	// Buy task shares key vocabulary with the put description.
	// This is a §4 substantive reuse case from the D1 fixture.
	buyTask := "document EventSink contract for warm-worker backends in warm-worker-backends.md"

	preMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})
	preMatchCount := len(preMsgs)

	buyMsg := h.sendMessage(h.buyer,
		buyPayload(buyTask, 20000),
		[]string{exchange.TagBuy},
		nil,
	)
	buyRec, err := h.st.GetMessage(buyMsg.ID)
	if err != nil {
		t.Fatalf("getting buy message: %v", err)
	}

	eng.State().Apply(exchange.FromStoreRecord(buyRec))
	if err := eng.DispatchForTest(exchange.FromStoreRecord(buyRec)); err != nil {
		t.Fatalf("DispatchForTest: %v", err)
	}

	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	postMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})
	if len(postMsgs) <= preMatchCount {
		t.Fatal("engine emitted no match-tagged message — expected a real match for above-floor entry")
	}
	emitted := postMsgs[len(postMsgs)-1]

	// PRIMARY ASSERTION: must NOT have buy-miss tag (must be a real match).
	if hasTag(emitted.Tags, exchange.TagBuyMiss) {
		t.Errorf("above-floor entry served as MISS, want exchange:match with results. "+
			"The below-floor gate must only reject entries that scored below 0.16, not all entries. "+
			"Tags: %v", emitted.Tags)
	}

	// Must have the match tag.
	if !hasTag(emitted.Tags, exchange.TagMatch) {
		t.Errorf("response missing exchange:match tag, got %v", emitted.Tags)
	}

	// Parse results: must contain the put entry.
	var mp struct {
		Results []struct {
			EntryID    string  `json:"entry_id"`
			Confidence float64 `json:"confidence"`
		} `json:"results"`
	}
	if err := json.Unmarshal(emitted.Payload, &mp); err != nil {
		t.Fatalf("parsing match payload: %v", err)
	}
	if len(mp.Results) == 0 {
		t.Fatal("match payload has no results — above-floor entry should appear")
	}

	// The inventory entry must appear in results (it's the only candidate).
	found := false
	for _, r := range mp.Results {
		if r.EntryID == putMsg.ID {
			found = true
			if r.Confidence <= 0 {
				t.Errorf("match result confidence = %v, want > 0 (above-floor entry has real semantic score)", r.Confidence)
			}
			break
		}
	}
	if !found {
		t.Errorf("above-floor entry %s not found in match results: %v", putMsg.ID[:8], mp.Results)
	}

	// Antecedent must point to the buy message.
	if len(emitted.Antecedents) == 0 || emitted.Antecedents[0] != buyMsg.ID {
		t.Errorf("match antecedent = %v, want [%s]", emitted.Antecedents, buyMsg.ID)
	}

	// Start the engine with a full event loop to confirm the match also works
	// end-to-end (not just via DispatchForTest).
	preMsgs2, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})
	buyer2 := newTestAgent(t)
	buyMsg2 := h.sendMessage(buyer2,
		buyPayload(buyTask+" variant", 20000),
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
		if len(matchMsgs) > len(preMsgs2) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()

	if len(matchMsgs) <= len(preMsgs2) {
		t.Fatal("event loop: no match-tagged message emitted for above-floor entry buy")
	}
	_ = buyMsg2 // confirmed it triggered the match
	latest := matchMsgs[len(matchMsgs)-1]
	if hasTag(latest.Tags, exchange.TagBuyMiss) {
		t.Errorf("event loop path: above-floor entry served as MISS instead of real match")
	}
}
