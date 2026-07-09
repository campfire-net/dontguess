package exchange_test

// Tests for dontguess-659: deliver-without-consume griefing via buy-and-abandon.
//
// Threat model (security review aca/480): a funded attacker buys a task that
// matches a competitor's inventory entry, accepts the match (forcing the operator
// to issue a deliver), then NEVER sends settle:complete. Each abandoned cycle
// increments the entry's DeliverCount with no matching ConsumeCount, driving the
// deliver-without-consume ratio up. The attacker's goal is to (a) push the
// competitor entry below the relevance floor so it stops being returned, and/or
// (b) trigger autonomous expiry/removal of the entry from inventory.
//
// PRIMARY hardening (this item): expiry stays operator-gated. The griefing test
// proves that a never-completing buyer's deliveries CAN flag the entry as an
// expiry CANDIDATE (operator-facing report) but CANNOT:
//   - auto-remove the entry from inventory, AND
//   - push the entry below the relevance floor so it stops matching.
// The demotion stays bounded at MaxBehavioralDemotion (-0.10), applied AFTER the
// floor gate, so a genuinely-relevant entry is still returned in matches.
//
// These tests drive the REAL engine/state/Rank path (put → accept → buy → match →
// buyer-accept → deliver, repeated, no complete), not a unit stub. The match
// emission goes through engine_buy.go → matchIndex.Search → matching.Rank, the
// same path a real buyer hits.

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/campfire-net/dontguess/pkg/store"

	"github.com/campfire-net/dontguess/pkg/exchange"
	"github.com/campfire-net/dontguess/pkg/matching"
)

// runMatchOnce drives the engine to emit exactly one match for the given buy
// task and returns the match message. It starts the engine, waits for a new
// match message to appear, then cancels. Fails the test if no match is emitted.
func runMatchOnce(t *testing.T, h *testHarness, eng *exchange.Engine, task string, budget int64) store.MessageRecord {
	t.Helper()

	buyMsg := h.sendMessage(h.buyer, buyPayload(task, budget), []string{exchange.TagBuy}, nil)

	preMatch, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})
	preCount := len(preMatch)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = eng.Start(ctx)
	}()

	var matchMsgs []store.MessageRecord
	deadline := time.Now().Add(2500 * time.Millisecond)
	for time.Now().Before(deadline) {
		matchMsgs, _ = h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})
		if len(matchMsgs) > preCount {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	// Cancel and JOIN the engine goroutine before returning. Without this, the
	// engine's background loop may still be running (and logging into t / touching
	// the store) after the test completes, causing a "Log in goroutine after test
	// has completed" panic. This mirrors testHarness.startEngine's join discipline.
	cancel()
	<-done

	if len(matchMsgs) <= preCount {
		t.Fatalf("no match emitted for task %q (buy=%s)", task, buyMsg.ID[:8])
	}
	return matchMsgs[len(matchMsgs)-1]
}

// matchEntryIDs parses the entry IDs out of a match message payload, in order.
func matchEntryIDs(t *testing.T, matchMsg store.MessageRecord) []string {
	t.Helper()
	var mp struct {
		Results []struct {
			EntryID string `json:"entry_id"`
		} `json:"results"`
	}
	if err := json.Unmarshal(matchMsg.Payload, &mp); err != nil {
		t.Fatalf("parsing match payload: %v", err)
	}
	ids := make([]string, 0, len(mp.Results))
	for _, r := range mp.Results {
		ids = append(ids, r.EntryID)
	}
	return ids
}

// abandonCycle simulates ONE deliver-without-consume griefing cycle against the
// entry referenced by matchMsg: the buyer accepts the match (forcing a deliver),
// the operator delivers, and the buyer NEVER completes. After this returns, the
// engine state is re-synced from the store so entryDeliverCount reflects the
// abandoned deliver.
func abandonCycle(t *testing.T, h *testHarness, eng *exchange.Engine, matchMsg store.MessageRecord, entryID string) {
	t.Helper()

	// Buyer accepts the match (selects the competitor entry).
	buyerAcceptPayload, _ := json.Marshal(map[string]any{
		"phase":    "buyer-accept",
		"entry_id": entryID,
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

	// Re-sync so buyerAcceptToMatch is populated before the deliver lands.
	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	// Operator delivers content (deliver-without-consume: this is the operator
	// fulfilling its obligation; the buyer then abandons).
	deliverPayload, _ := json.Marshal(map[string]any{
		"phase":        "deliver",
		"entry_id":     entryID,
		"content_ref":  "sha256:" + fmt.Sprintf("%064x", 100),
		"content_size": 20000,
	})
	h.sendMessage(h.operator, deliverPayload,
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrDeliver,
		},
		[]string{buyerAcceptMsg.ID},
	)

	// The buyer NEVER sends settle:complete. Re-sync so entryDeliverCount++ lands.
	allMsgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))
}

// TestGriefing_DeliverWithoutConsume_CannotRemoveOrDerankCompetitor is the
// ground-source proof for dontguess-659. A funded attacker repeatedly buys a
// task matching a competitor entry, accepts, and abandons (never completes).
// After many griefing cycles the test asserts, through the REAL engine path:
//
//  1. The competitor entry is STILL in live inventory (no autonomous removal).
//  2. A fresh buy STILL produces a match that returns the competitor entry
//     (the demotion did NOT push the relevant entry below the relevance floor).
//  3. The entry IS surfaced as an operator-facing expiry CANDIDATE — flagging
//     works, but it is advisory only; nothing acted on it autonomously.
func TestGriefing_DeliverWithoutConsume_CannotRemoveOrDerankCompetitor(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	// --- Competitor seeds a high-quality, highly-relevant entry. ---
	const competitorDesc = "Generate unit tests for a Go HTTP handler accepting JSON POST with validation"
	putMsg := h.sendMessage(h.seller,
		putPayload(competitorDesc, "sha256:"+fmt.Sprintf("%064x", 100), "code", 12000, 20000),
		[]string{exchange.TagPut, "exchange:content-type:code", "exchange:domain:go"},
		nil,
	)
	msgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))
	if err := eng.AutoAcceptPut(putMsg.ID, 8400, time.Now().Add(168*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut: %v", err)
	}

	inv := eng.State().Inventory()
	if len(inv) != 1 {
		t.Fatalf("setup: expected 1 inventory entry, got %d", len(inv))
	}
	entryID := inv[0].EntryID

	// The buyer task is highly relevant to the competitor entry — a legitimate
	// buyer would match it. The attacker uses the SAME task to ensure the match
	// always selects the competitor entry.
	const attackTask = "unit tests for a Go HTTP handler that accepts a JSON POST body with validation"

	// --- Griefing loop: many abandoned deliver-without-consume cycles. ---
	// Use well above FalsePositiveWindowMin (3) and the ExpiryCandidate ratio
	// threshold (5.0) so the entry is firmly in "expiry candidate" territory.
	const griefCycles = 12
	for i := 0; i < griefCycles; i++ {
		matchMsg := runMatchOnce(t, h, eng, attackTask, 50000)
		ids := matchEntryIDs(t, matchMsg)
		if len(ids) == 0 || ids[0] != entryID {
			t.Fatalf("grief cycle %d: match did not return competitor entry; got %v want %q", i, ids, entryID)
		}
		abandonCycle(t, h, eng, matchMsg, entryID)
	}

	// Re-sync and refresh the index behavioral signals (the engine does this on
	// settle; we force it here to evaluate the post-griefing steady state).
	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	// --- Assertion 1: entry NOT autonomously removed from inventory. ---
	if e := eng.State().GetInventoryEntry(entryID); e == nil {
		t.Fatalf("griefing removed the competitor entry from inventory — expiry must NOT be autonomous")
	}
	postInv := eng.State().Inventory()
	if len(postInv) != 1 {
		t.Errorf("griefing changed live inventory size: got %d, want 1 (no autonomous removal)", len(postInv))
	}

	// --- Assertion 2: a fresh buy STILL matches the competitor entry. ---
	// This is the blast-radius bound: a -0.10 demotion applied post-floor cannot
	// push a genuinely-relevant entry below the relevance floor. A legitimate
	// buyer must still find the entry after the griefing campaign.
	finalMatch := runMatchOnce(t, h, eng, attackTask, 50000)
	finalIDs := matchEntryIDs(t, finalMatch)
	found := false
	for _, id := range finalIDs {
		if id == entryID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("after %d griefing cycles, a fresh buy no longer returns the competitor entry (ids=%v) — "+
			"deliver-without-consume demotion drove a relevant entry below the relevance floor", griefCycles, finalIDs)
	}

	// --- Assertion 3: the entry IS flagged as an operator-facing candidate. ---
	// Flagging is correct and useful; it is advisory only. The operator decides.
	candidates := eng.State().ExpiryCandidates()
	flagged := false
	for _, c := range candidates {
		if c.EntryID == entryID {
			flagged = true
			if c.DeliverCount < matching.FalsePositiveWindowMin {
				t.Errorf("expiry candidate DeliverCount = %d, want >= window %d",
					c.DeliverCount, matching.FalsePositiveWindowMin)
			}
			if c.ConsumeCount != 0 {
				t.Errorf("expiry candidate ConsumeCount = %d, want 0 (buyer never completed)", c.ConsumeCount)
			}
		}
	}
	if !flagged {
		t.Errorf("entry with %d abandoned deliveries (0 consumes) should be flagged as an expiry candidate", griefCycles)
	}
}
