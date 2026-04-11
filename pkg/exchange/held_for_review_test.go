package exchange_test

// held_for_review_test.go — feature tests for dontguess-31d.
//
// Tests the held-for-review classification: puts with TokenCost > auto-accept max
// are retained in pendingPuts (no campfire state change) but tagged in-memory so
// the operator CLI can surface them via PutsHeldForReview().
//
// Covered:
//   - TestHoldForReview_ClassifiesOverCap: over-cap put is held, stays pending,
//     only 1 log line after 10 ticks, buyers cannot match it.
//   - TestHoldForReview_DoesNotEmitConventionMessage: no new campfire messages after hold.
//   - TestAutoAccept_NewDefault_1M: put at 800k auto-accepts at new 1M default.
//   - TestAutoAccept_StillHolds_AboveNewDefault: put at 1.5M is held.
//   - TestHoldForReview_Prune: heldForReview is pruned after the put is accepted.

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	"github.com/campfire-net/dontguess/pkg/exchange"
)

// sendPutWithCost is a helper that sends a put with the given TokenCost.
// It delegates to the existing sendOverCapPut helper (which is defined in
// auto_accept_logonce_test.go and lives in the same test package).
func sendPutWithCost(t *testing.T, h *testHarness, desc string, tokenCost int64) string {
	t.Helper()
	// Re-use sendOverCapPut — the name is misleading but it just sends a put
	// with the given tokenCost regardless of the cap; the cap is applied by RunAutoAccept.
	return sendOverCapPut(t, h, desc, tokenCost)
}

// TestHoldForReview_ClassifiesOverCap verifies that a put with TokenCost > max
// (1) stays in PendingPuts, (2) appears in PutsHeldForReview, (3) produces exactly
// 1 log line after 10 RunAutoAccept ticks, and (4) a buyer cannot match it
// (matching only hits accepted inventory).
func TestHoldForReview_ClassifiesOverCap(t *testing.T) {
	t.Parallel()

	const maxAccept = int64(1_000_000)
	const overCapCost = int64(2_000_000) // > maxAccept

	h := newTestHarness(t)

	var logBuf bytes.Buffer
	eng := h.newEngineWithOpts(func(o *exchange.EngineOptions) {
		o.Logger = newLogCapture(&logBuf)
	})

	putID := sendPutWithCost(t, h, "expensive inference result", overCapCost)
	replayAll(t, h, eng)

	// The put should be in pending.
	pending := eng.State().PendingPuts()
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending put, got %d", len(pending))
	}
	if pending[0].PutMsgID != putID {
		t.Fatalf("pending put ID mismatch: got %s, want %s", pending[0].PutMsgID[:8], putID[:8])
	}

	// Run auto-accept 10 times — should hold the put, logging exactly once.
	skipped := make(map[string]struct{})
	now := time.Now()
	for i := 0; i < 10; i++ {
		eng.RunAutoAccept(maxAccept, now, skipped)
	}

	// (1) Still in PendingPuts.
	pending = eng.State().PendingPuts()
	if len(pending) != 1 {
		t.Errorf("expected put still in PendingPuts after 10 ticks, got %d", len(pending))
	}

	// (2) In PutsHeldForReview.
	held := eng.State().PutsHeldForReview()
	if len(held) != 1 {
		t.Errorf("expected 1 entry in PutsHeldForReview, got %d", len(held))
	} else if held[0].PutMsgID != putID {
		t.Errorf("held put ID mismatch: got %s, want %s", held[0].PutMsgID[:8], putID[:8])
	}

	// (3) Exactly 1 "skipping put" log line across 10 ticks.
	skipLines := countLines(&logBuf, "skipping put")
	if skipLines != 1 {
		t.Errorf("expected exactly 1 'skipping put' log line across 10 ticks, got %d\nlog:\n%s",
			skipLines, logBuf.String())
	}

	// (4) Buyer cannot match the held put (it's not in accepted inventory).
	// The inventory should be empty because the put was never accepted.
	inv := eng.State().Inventory()
	if len(inv) != 0 {
		t.Errorf("expected empty inventory (held put not accepted), got %d entries", len(inv))
	}
}

// TestHoldForReview_DoesNotEmitConventionMessage verifies that after a put is
// classified as held-for-review, no new campfire messages are emitted.
// Only the original put message should exist for this put ID.
func TestHoldForReview_DoesNotEmitConventionMessage(t *testing.T) {
	t.Parallel()

	const maxAccept = int64(1_000_000)
	const overCapCost = int64(2_000_000)

	h := newTestHarness(t)
	eng := h.newEngineWithOpts(nil)

	// Count messages before submitting the put.
	msgsBefore, err := h.st.ListMessages(h.cfID, 0)
	if err != nil {
		t.Fatalf("listing messages before put: %v", err)
	}
	countBefore := len(msgsBefore)

	putID := sendPutWithCost(t, h, "held inference result", overCapCost)
	replayAll(t, h, eng)

	// Count messages after submitting the put — one new message expected.
	msgsAfterPut, err := h.st.ListMessages(h.cfID, 0)
	if err != nil {
		t.Fatalf("listing messages after put: %v", err)
	}
	countAfterPut := len(msgsAfterPut)
	if countAfterPut != countBefore+1 {
		t.Fatalf("expected %d messages after put (1 new), got %d", countBefore+1, countAfterPut)
	}

	// Run auto-accept 3 times — should hold the put, emitting no new messages.
	skipped := make(map[string]struct{})
	now := time.Now()
	for i := 0; i < 3; i++ {
		eng.RunAutoAccept(maxAccept, now, skipped)
	}

	msgsAfterHold, err := h.st.ListMessages(h.cfID, 0)
	if err != nil {
		t.Fatalf("listing messages after hold: %v", err)
	}
	countAfterHold := len(msgsAfterHold)
	if countAfterHold != countAfterPut {
		t.Errorf("expected no new messages after held-for-review classification (got %d, want %d)\nput ID: %s",
			countAfterHold, countAfterPut, putID[:8])
	}
}

// TestAutoAccept_NewDefault_1M verifies that the flag default (1,000,000) allows
// a put with TokenCost=800,000 to auto-accept without being held.
func TestAutoAccept_NewDefault_1M(t *testing.T) {
	t.Parallel()

	const newDefault = int64(1_000_000) // matches serve.go new default
	const underCapCost = int64(800_000) // < 1M — should auto-accept

	h := newTestHarness(t)
	eng := h.newEngineWithOpts(nil)

	sendPutWithCost(t, h, "under-cap inference result at 800k", underCapCost)
	replayAll(t, h, eng)

	pending := eng.State().PendingPuts()
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending put, got %d", len(pending))
	}

	skipped := make(map[string]struct{})
	eng.RunAutoAccept(newDefault, time.Now(), skipped)

	// After auto-accept the put should leave pending.
	pending = eng.State().PendingPuts()
	if len(pending) != 0 {
		t.Errorf("expected put to leave pending after auto-accept at 1M default, got %d pending", len(pending))
	}

	// And not in heldForReview.
	held := eng.State().PutsHeldForReview()
	if len(held) != 0 {
		t.Errorf("expected empty PutsHeldForReview after auto-accept, got %d", len(held))
	}

	// Should be in inventory.
	inv := eng.State().Inventory()
	if len(inv) != 1 {
		t.Errorf("expected 1 accepted entry in inventory, got %d", len(inv))
	}
}

// TestAutoAccept_StillHolds_AboveNewDefault verifies that a put with
// TokenCost=1,500,000 is held when the default cap is 1,000,000.
func TestAutoAccept_StillHolds_AboveNewDefault(t *testing.T) {
	t.Parallel()

	const newDefault = int64(1_000_000)
	const overCapCost = int64(1_500_000) // > 1M — should be held

	h := newTestHarness(t)
	eng := h.newEngineWithOpts(nil)

	sendPutWithCost(t, h, "over-cap inference result at 1.5M", overCapCost)
	replayAll(t, h, eng)

	skipped := make(map[string]struct{})
	eng.RunAutoAccept(newDefault, time.Now(), skipped)

	// Put should still be in pending.
	pending := eng.State().PendingPuts()
	if len(pending) != 1 {
		t.Errorf("expected put still in pending (held for review), got %d", len(pending))
	}

	// And should be in heldForReview.
	held := eng.State().PutsHeldForReview()
	if len(held) != 1 {
		t.Errorf("expected 1 entry in PutsHeldForReview, got %d", len(held))
	}
}

// TestHoldForReview_Prune verifies that when a held put is accepted (via
// AutoAcceptPut with a higher override max), the subsequent RunAutoAccept tick
// prunes it from heldForReview.
func TestHoldForReview_Prune(t *testing.T) {
	t.Parallel()

	const maxAccept = int64(1_000_000)
	const overCapCost = int64(2_000_000)

	h := newTestHarness(t)

	var logBuf bytes.Buffer
	eng := h.newEngineWithOpts(func(o *exchange.EngineOptions) {
		o.Logger = newLogCapture(&logBuf)
	})

	// Submit over-cap put and run once — classifies as held.
	putID := sendPutWithCost(t, h, "prune-test result", overCapCost)
	replayAll(t, h, eng)

	skipped := make(map[string]struct{})
	now := time.Now()
	eng.RunAutoAccept(maxAccept, now, skipped)

	held := eng.State().PutsHeldForReview()
	if len(held) != 1 {
		t.Fatalf("expected 1 held put after classification, got %d", len(held))
	}

	// Simulate post-hoc accept with a much higher override max.
	price := overCapCost * 70 / 100
	if err := eng.AutoAcceptPut(putID, price, now.Add(72*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut: %v", err)
	}
	// Verify the put left pending.
	pending := eng.State().PendingPuts()
	for _, e := range pending {
		if e.PutMsgID == putID {
			t.Fatalf("put %s still in pending after AutoAcceptPut", putID[:8])
		}
	}

	// Run auto-accept once more — prune should remove putID from heldForReview.
	logBuf.Reset()
	eng.RunAutoAccept(maxAccept, now, skipped)

	held = eng.State().PutsHeldForReview()
	if len(held) != 0 {
		t.Errorf("expected empty PutsHeldForReview after prune, got %d entries", len(held))
	}

	// Send a second over-cap put (different ID) to confirm heldForReview tracks new entries.
	logBuf.Reset()
	putID2 := sendPutWithCost(t, h, fmt.Sprintf("prune-test result 2 seed=%d", overCapCost+1), overCapCost)
	// Replay to pick up the new put.
	msgs, err := h.st.ListMessages(h.cfID, 0)
	if err != nil {
		t.Fatalf("listing messages: %v", err)
	}
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	eng.RunAutoAccept(maxAccept, now, skipped)

	held = eng.State().PutsHeldForReview()
	if len(held) != 1 {
		t.Errorf("expected 1 held entry for putID2, got %d", len(held))
	} else if held[0].PutMsgID != putID2 {
		t.Errorf("expected held entry to be putID2 %s, got %s", putID2[:8], held[0].PutMsgID[:8])
	}

	// Also confirm skippedPuts has the new ID.
	if _, ok := skipped[putID2]; !ok {
		t.Errorf("expected putID2 %s in skippedPuts after first skip", putID2[:8])
	}
	skipLines := countLines(&logBuf, "skipping put")
	if skipLines != 1 {
		t.Errorf("expected 1 skip line for putID2 after prune, got %d\nlog:\n%s", skipLines, logBuf.String())
	}
}
