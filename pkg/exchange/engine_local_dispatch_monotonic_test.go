package exchange

// engine_local_dispatch_monotonic_test.go — regression test for the dontguess-b84
// residual defect (dontguess-b84-r2).
//
// The b84 fix split the local-ingest cursor into a fold cursor (localSeen) and a
// dispatch cursor (localDispatched). But pollLocalStore advanced the dispatch
// cursor UNCONDITIONALLY — e.localDispatched = total — where total is the length
// of a LocalStore.Replay() snapshot captured OUTSIDE localMu. pollLocalStore holds
// no opMu, so it runs concurrently with rebuildAndDispatchGapLocal (AutoAcceptPut /
// RejectPut, under opMu). Interleaving: a rebuild with a FRESH snapshot advances
// localDispatched to N and dispatches record N-1 (a settle:complete → one durable
// TagConsume via emitConsumeSignal). A poll whose snapshot was captured EARLIER
// (shorter) then reaches localMu and writes localDispatched = (shorter) —
// REGRESSING the dispatch cursor. The gap reopens and the next poll RE-dispatches
// the settle:complete, and emitConsumeSignal (which has no idempotency guard, unlike
// applySettleComplete) DOUBLE-COUNTS the consume behavioral signal that feeds the
// pricing loops. The -race detector cannot see this — all cursor access is under
// localMu; it is a LOGIC race, not a memory race.
//
// This test reproduces the interleaving DETERMINISTICALLY with no goroutines, no
// timing, and no mocks: it captures a real (soon-to-be-stale) snapshot BEFORE the
// settle:complete is appended, then a fresh snapshot AFTER, then drives the shared
// fold/dispatch path (foldAndDispatchLocalSnapshot) with fresh → stale → fresh
// snapshots — exactly the order the two concurrent callers would serialize on
// localMu. It asserts (a) the dispatch cursor never decreases and (b) exactly ONE
// consume signal is recorded for a single settle:complete.
//
// Against the pre-fix code (localDispatched = total, unconditional) the stale
// snapshot regresses the cursor (assertion a fails) and the final fresh poll
// re-dispatches the complete, yielding ConsumeCount == 2 (assertion b fails).
// After the monotonic-claim fix (claimDispatchGap) both hold.

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	dgstore "github.com/campfire-net/dontguess/pkg/store"
)

func TestLocalDispatchCursorMonotonic_StaleSnapshotNoDoubleConsume(t *testing.T) {
	dir := t.TempDir()
	ls, err := dgstore.Open(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatalf("dgstore.Open: %v", err)
	}
	t.Cleanup(func() { ls.Close() }) //nolint:errcheck

	operatorKey := newReservationID()
	seller := newReservationID()
	buyer := newReservationID()
	eng := NewEngine(EngineOptions{
		CampfireID:        "local",
		LocalStore:        ls,
		OperatorPublicKey: operatorKey,
		PollInterval:      20 * time.Millisecond,
		Logger:            func(format string, args ...any) { t.Logf("[engine] "+format, args...) },
	})

	// Startup fold on the empty log; both cursors begin at 0.
	if err := eng.replayAll(); err != nil {
		t.Fatalf("initial replayAll: %v", err)
	}

	// --- Seed inventory: put1 + auto-accept → one matchable entry. ---
	put1 := newReservationID()
	if err := ls.Append(dgstore.Record{
		ID:         put1,
		CampfireID: "local",
		Sender:     seller,
		Payload:    localBuyDropPutPayload(t, "Go HTTP handler unit test generator", 8000),
		Tags:       []string{TagPut, "exchange:content-type:code", "exchange:domain:go"},
		Timestamp:  time.Now().UnixNano(),
	}); err != nil {
		t.Fatalf("append put1: %v", err)
	}
	if err := eng.AutoAcceptPut(put1, 5600, time.Now().Add(72*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut(put1): %v", err)
	}
	inv := eng.State().Inventory()
	if len(inv) != 1 {
		t.Fatalf("expected 1 inventory entry after accept, got %d", len(inv))
	}
	entryID := inv[0].EntryID

	// --- Build the settlement chain up through deliver, all FOLDED into state. ---
	// The buy task is deliberately dissimilar to the entry so the engine's own
	// matcher misses and does not emit a competing match — we supply an
	// operator-signed match ourselves so the deliver→match→entry chain that
	// emitConsumeSignal walks is under our control.
	buyID := newReservationID()
	if err := ls.Append(dgstore.Record{
		ID:         buyID,
		CampfireID: "local",
		Sender:     buyer,
		Payload:    localBuyDropBuyPayload(t, "medieval French troubadour poetry translation", 400),
		Tags:       []string{TagBuy},
		Timestamp:  time.Now().UnixNano(),
	}); err != nil {
		t.Fatalf("append buy: %v", err)
	}

	matchID := newReservationID()
	matchPayload, _ := json.Marshal(map[string]any{
		"results": []map[string]any{{"entry_id": entryID}},
	})
	if err := ls.Append(dgstore.Record{
		ID:          matchID,
		CampfireID:  "local",
		Sender:      operatorKey,
		Payload:     matchPayload,
		Tags:        []string{TagMatch},
		Antecedents: []string{buyID},
		Timestamp:   time.Now().UnixNano(),
	}); err != nil {
		t.Fatalf("append match: %v", err)
	}

	buyerAcceptID := newReservationID()
	buyerAcceptPayload, _ := json.Marshal(map[string]any{
		"phase":    SettlePhaseStrBuyerAccept,
		"entry_id": entryID,
		"accepted": true,
	})
	if err := ls.Append(dgstore.Record{
		ID:          buyerAcceptID,
		CampfireID:  "local",
		Sender:      buyer,
		Payload:     buyerAcceptPayload,
		Tags:        []string{TagSettle, TagPhasePrefix + SettlePhaseStrBuyerAccept, TagVerdictPrefix + "accepted"},
		Antecedents: []string{matchID},
		Timestamp:   time.Now().UnixNano(),
	}); err != nil {
		t.Fatalf("append buyer-accept: %v", err)
	}

	deliverID := newReservationID()
	deliverPayload, _ := json.Marshal(map[string]any{
		"phase":       SettlePhaseStrDeliver,
		"entry_id":    entryID,
		"content_ref": "sha256:deadbeef",
	})
	if err := ls.Append(dgstore.Record{
		ID:          deliverID,
		CampfireID:  "local",
		Sender:      operatorKey,
		Payload:     deliverPayload,
		Tags:        []string{TagSettle, TagPhasePrefix + SettlePhaseStrDeliver},
		Antecedents: []string{buyerAcceptID},
		Timestamp:   time.Now().UnixNano(),
	}); err != nil {
		t.Fatalf("append deliver: %v", err)
	}

	// One poll folds+dispatches everything appended so far. After it the state
	// has the full deliver→match→entry chain and the cursors are caught up. Any
	// operator messages emitted by these dispatches are marked handled in-line.
	if err := eng.pollLocalStore(); err != nil {
		t.Fatalf("pollLocalStore (build): %v", err)
	}
	if _, ok := eng.State().EntryForDeliver(deliverID); !ok {
		t.Fatalf("deliver→entry chain not in state after build poll — test scaffolding is wrong")
	}

	// --- The bug window ---
	// staleSnap is a REAL snapshot captured now, BEFORE the settle:complete is
	// appended. It models the poll goroutine's Replay() running early. The engine
	// (via a concurrent rebuild, modeled below by the first fresh dispatch) will
	// advance the dispatch cursor past this length before this poll reaches localMu.
	staleSnap, err := ls.Replay()
	if err != nil {
		t.Fatalf("capture stale snapshot: %v", err)
	}
	staleLen := len(staleSnap)

	// Append the settle:complete — the single record whose dispatch emits ONE
	// durable consume signal.
	completeID := newReservationID()
	completePayload, _ := json.Marshal(map[string]any{
		"phase":    SettlePhaseStrComplete,
		"entry_id": entryID,
		"price":    5600,
	})
	if err := ls.Append(dgstore.Record{
		ID:          completeID,
		CampfireID:  "local",
		Sender:      buyer,
		Payload:     completePayload,
		Tags:        []string{TagSettle, TagPhasePrefix + SettlePhaseStrComplete, TagVerdictPrefix + "accepted"},
		Antecedents: []string{deliverID},
		Timestamp:   time.Now().UnixNano(),
	}); err != nil {
		t.Fatalf("append complete: %v", err)
	}

	consumeCount := func() int {
		return eng.State().AllEntryBehavioralSignals()[entryID].ConsumeCount
	}
	if c := consumeCount(); c != 0 {
		t.Fatalf("precondition: ConsumeCount = %d before any complete dispatch, want 0", c)
	}

	// (1) FRESH poll — models the rebuild that observed the complete. Dispatches
	// the settle:complete exactly once → one consume signal, cursor advances.
	freshSnap1, err := ls.Replay()
	if err != nil {
		t.Fatalf("capture fresh snapshot 1: %v", err)
	}
	eng.FoldAndDispatchLocalSnapshotForTest(freshSnap1)
	d1 := eng.LocalDispatchedForTest()
	if c := consumeCount(); c != 1 {
		t.Fatalf("after fresh dispatch of settle:complete, ConsumeCount = %d, want 1", c)
	}
	if d1 < len(freshSnap1) {
		t.Fatalf("dispatch cursor = %d after fresh poll, want >= %d (complete not dispatched)", d1, len(freshSnap1))
	}

	// (2) STALE poll — the late-arriving poll holding the pre-complete snapshot.
	// Pre-fix this wrote localDispatched = staleLen, REGRESSING the cursor below
	// d1. Post-fix (monotonic claim) it must be a no-op.
	if staleLen >= d1 {
		t.Fatalf("test invariant broken: staleLen (%d) must be < post-fresh cursor (%d) to model a regression", staleLen, d1)
	}
	eng.FoldAndDispatchLocalSnapshotForTest(staleSnap)
	d2 := eng.LocalDispatchedForTest()
	if d2 < d1 {
		t.Fatalf("dispatch cursor REGRESSED: %d → %d after a stale (len %d) snapshot poll — "+
			"the [%d:%d] gap reopens and records re-dispatch (dontguess-b84-r2)", d1, d2, staleLen, d2, d1)
	}
	if c := consumeCount(); c != 1 {
		t.Fatalf("stale-snapshot poll changed ConsumeCount to %d, want 1", c)
	}

	// (3) FRESH poll again — models the next poll after the (attempted) regression.
	// Pre-fix, with the cursor regressed to staleLen, this re-dispatches the
	// settle:complete → emitConsumeSignal fires a SECOND time (ConsumeCount == 2).
	// Post-fix the cursor was never regressed, so nothing re-dispatches.
	freshSnap2, err := ls.Replay()
	if err != nil {
		t.Fatalf("capture fresh snapshot 2: %v", err)
	}
	eng.FoldAndDispatchLocalSnapshotForTest(freshSnap2)
	d3 := eng.LocalDispatchedForTest()
	if d3 < d2 {
		t.Fatalf("dispatch cursor REGRESSED across final poll: %d → %d", d2, d3)
	}

	// --- Invariant assertions: the settle:complete was handled EXACTLY ONCE. ---
	if c := consumeCount(); c != 1 {
		t.Fatalf("settle:complete produced ConsumeCount = %d, want exactly 1 — a re-dispatched "+
			"settle:complete double-counted the consume signal (dontguess-b84-r2 dispatch-cursor regression)", c)
	}

	// Cross-check the durable observable: exactly one TagConsume record on the log.
	all, err := ls.Replay()
	if err != nil {
		t.Fatalf("final replay: %v", err)
	}
	consumeRecords := 0
	for _, r := range all {
		for _, tag := range r.Tags {
			if tag == TagConsume {
				consumeRecords++
				break
			}
		}
	}
	if consumeRecords != 1 {
		t.Fatalf("found %d exchange:consume records on the log, want exactly 1", consumeRecords)
	}
}
