package exchange

// fold_double_apply_f86_test.go — dontguess-f86: guard non-idempotent fold-path
// accumulators against poll-vs-rebuild double-apply.
//
// engine_core.go's foldAndDispatchLocalSnapshot (the poll loop) claims its fold
// gap [foldStart:total) under localMu, then UNLOCKS and calls state.Apply for
// each message in that gap ONE AT A TIME (engine_core.go ~1103-1108). Between
// any two of those unlocked Apply calls, a concurrent rebuildAndDispatchGapLocal
// (driven by AutoAcceptPut/RejectPut) can acquire localMu, see its own snapshot
// total > localSeen, and run a FULL state.Replay over the whole log — including
// the very messages the poll loop's own claimed-but-not-yet-executed Apply calls
// are about to process. Replay does not know about, and cannot cancel, those
// in-flight Apply calls: when the poll loop resumes, it applies its remaining
// messages AGAIN, onto the state Replay just freshly (and correctly) rebuilt.
//
// State.Apply/State.Replay are NOT racing each other in the memory-safety sense
// (both take s.mu) — this is a LOGIC race the -race detector cannot see: the
// same message id gets folded through applyLocked twice, once inside Replay's
// full pass and once via the poll loop's own stale continuation.
//
// Most fold handlers are immune because their mutation is idempotent map
// assignment (same key, same value, second write is a no-op) or already had a
// per-message-ID dedup guard (applySettleComplete's completedSettlements).
// Handlers whose mutation is a raw counter/slice increment with NO such guard
// double-count under this interleave. This test reproduces the interleave
// DETERMINISTICALLY — no goroutines, no timing, no mocks — by driving the
// real, unmodified State.Apply and State.Replay methods (the exact methods
// foldAndDispatchLocalSnapshot and rebuildAndDispatchGapLocal call) in the
// precise order the race permits: apply a prefix, let a "rebuild" Replay the
// full log (including the not-yet-applied tail), then resume the "poll" and
// apply that same tail message again.
//
// Before the dontguess-f86 fix, TestFoldDoubleApply_SettleDeliverEntryCounter
// observes DeliverCount == 2 (double-counted). After the fix (deliverToMatch
// dedup guard in applySettleDeliver) it observes DeliverCount == 1.

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	dgstore "github.com/3dl-dev/dontguess/pkg/store"
)

// buildDeliverChainMessages returns, in log order, every message from put
// through settle:deliver for a single entry: put, buy, match, settle
// (buyer-accept), settle(deliver). It appends them to ls (so the fold-chain
// helpers that resolve via LocalStore-backed lookups still work if needed) and
// returns the exact same records as a []Message slice ready for State.Apply /
// State.Replay, mirroring engine_local_dispatch_monotonic_test.go's chain
// construction.
func buildDeliverChainMessages(t *testing.T, ls *dgstore.Store, operatorKey, seller, buyer, entryID string) []Message {
	t.Helper()

	buyID := newReservationID()
	mustAppend(t, ls, dgstore.Record{
		ID:         buyID,
		CampfireID: "local",
		Sender:     buyer,
		Payload:    localBuyDropBuyPayload(t, "medieval French troubadour poetry translation", 400),
		Tags:       []string{TagBuy},
		Timestamp:  time.Now().UnixNano(),
	})

	matchID := newReservationID()
	matchPayload, _ := json.Marshal(map[string]any{
		"results": []map[string]any{{"entry_id": entryID}},
	})
	mustAppend(t, ls, dgstore.Record{
		ID:          matchID,
		CampfireID:  "local",
		Sender:      operatorKey,
		Payload:     matchPayload,
		Tags:        []string{TagMatch},
		Antecedents: []string{buyID},
		Timestamp:   time.Now().UnixNano(),
	})

	buyerAcceptID := newReservationID()
	buyerAcceptPayload, _ := json.Marshal(map[string]any{
		"phase":    SettlePhaseStrBuyerAccept,
		"entry_id": entryID,
		"accepted": true,
	})
	mustAppend(t, ls, dgstore.Record{
		ID:          buyerAcceptID,
		CampfireID:  "local",
		Sender:      buyer,
		Payload:     buyerAcceptPayload,
		Tags:        []string{TagSettle, TagPhasePrefix + SettlePhaseStrBuyerAccept, TagVerdictPrefix + "accepted"},
		Antecedents: []string{matchID},
		Timestamp:   time.Now().UnixNano(),
	})

	deliverID := newReservationID()
	deliverPayload, _ := json.Marshal(map[string]any{
		"phase":       SettlePhaseStrDeliver,
		"entry_id":    entryID,
		"content_ref": "sha256:deadbeef",
	})
	mustAppend(t, ls, dgstore.Record{
		ID:          deliverID,
		CampfireID:  "local",
		Sender:      operatorKey,
		Payload:     deliverPayload,
		Tags:        []string{TagSettle, TagPhasePrefix + SettlePhaseStrDeliver},
		Antecedents: []string{buyerAcceptID},
		Timestamp:   time.Now().UnixNano(),
	})

	msgs, err := ls.Replay()
	if err != nil {
		t.Fatalf("ls.Replay: %v", err)
	}
	return msgs
}

func mustAppend(t *testing.T, ls *dgstore.Store, rec dgstore.Record) {
	t.Helper()
	if err := ls.Append(rec); err != nil {
		t.Fatalf("append %s: %v", rec.ID, err)
	}
}

// TestFoldDoubleApply_SettleDeliverEntryCounter proves the poll-vs-rebuild fold
// interleave is REACHABLE and, pre-fix, double-counts entryDeliverCount /
// buyerDeliverCount / entryDeliverBuyerCount (applySettleDeliver,
// state_settle.go). Post-fix (deliverToMatch dedup guard) it must observe
// exactly one count no matter how many times the tail message is re-applied.
func TestFoldDoubleApply_SettleDeliverEntryCounter(t *testing.T) {
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
		Logger:            func(format string, args ...any) { t.Logf("[engine] "+format, args...) },
	})
	if err := eng.replayAll(); err != nil {
		t.Fatalf("initial replayAll: %v", err)
	}

	put1 := newReservationID()
	mustAppend(t, ls, dgstore.Record{
		ID:         put1,
		CampfireID: "local",
		Sender:     seller,
		Payload:    localBuyDropPutPayload(t, "Go HTTP handler unit test generator", 8000),
		Tags:       []string{TagPut, "exchange:content-type:code", "exchange:domain:go"},
		Timestamp:  time.Now().UnixNano(),
	})
	if err := eng.AutoAcceptPut(put1, 5600, time.Now().Add(72*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut(put1): %v", err)
	}
	inv := eng.State().Inventory()
	if len(inv) != 1 {
		t.Fatalf("expected 1 inventory entry after accept, got %d", len(inv))
	}
	entryID := inv[0].EntryID

	msgs := buildDeliverChainMessages(t, ls, operatorKey, seller, buyer, entryID)

	st := NewState()
	st.OperatorKey = operatorKey

	// Model the poll loop: it claimed the WHOLE fold gap [0:len(msgs)) under
	// localMu and is now applying messages one at a time, UNLOCKED — exactly
	// the loop body at engine_core.go foldAndDispatchLocalSnapshot
	// (`for i := foldStart; i < total; i++ { e.state.Apply(&msgs[i]) }`).
	// It has applied every message EXCEPT the last (settle:deliver) when it
	// is preempted.
	last := len(msgs) - 1
	for i := 0; i < last; i++ {
		st.Apply(&msgs[i])
	}
	if sig := st.AllEntryBehavioralSignals()[entryID]; sig.DeliverCount != 0 {
		t.Fatalf("precondition: DeliverCount = %d before any deliver applied, want 0", sig.DeliverCount)
	}

	// A concurrent rebuildAndDispatchGapLocal now wins the race: its own
	// snapshot (the SAME full log) has total > localSeen, so it runs a FULL
	// state.Replay over every message — including the settle:deliver the poll
	// loop has not yet reached. This is the EXACT call the grow branch of
	// rebuildAndDispatchGapLocal makes (engine_core.go ~1177: `e.state.Replay(msgs)`).
	st.Replay(msgs)
	sigAfterRebuild := st.AllEntryBehavioralSignals()[entryID]
	if sigAfterRebuild.DeliverCount != 1 {
		t.Fatalf("after rebuild's full Replay, DeliverCount = %d, want 1 (rebuild folds the log exactly once)",
			sigAfterRebuild.DeliverCount)
	}

	// The poll loop resumes, unaware the rebuild already folded its claimed
	// tail via Replay, and applies its own stale reference to the SAME
	// settle:deliver message a second time — reproducing the exact
	// engine_core.go call.
	st.Apply(&msgs[last])

	sigFinal := st.AllEntryBehavioralSignals()[entryID]
	if sigFinal.DeliverCount != 1 {
		t.Fatalf("DeliverCount = %d after poll-vs-rebuild interleave, want exactly 1 — "+
			"settle:deliver was double-applied (dontguess-f86): once inside rebuildAndDispatchGapLocal's "+
			"full state.Replay, once by foldAndDispatchLocalSnapshot's stale unlocked Apply continuation",
			sigFinal.DeliverCount)
	}
}

// TestFoldDoubleApply_RecordFoldDenial documents recordFoldDenial's actual
// behavior under the poll-vs-rebuild interleave (dontguess-f86 audit item:
// "FoldDenial*.Add(1) guarded only by s.replaying, not per-message dedup").
//
// Tracing the interleave shows this counter was ALREADY safe before this fix,
// for a reason distinct from the other five: recordFoldDenial's PRE-EXISTING
// s.replaying guard makes State.Replay's bulk fold pass contribute ZERO real
// counts (by design — a full rebuild must never re-inflate live alarm
// counters). Combined with the fold-cursor's monotonic exactly-once claim
// (localSeen only advances forward — see State.foldDenialCounted doc), a
// given message receives AT MOST ONE live (non-Replay) Apply call across the
// system's entire lifetime, and only that ONE live call ever increments a
// FoldDenial* counter — regardless of whether it lands before or after a
// concurrent rebuild's Replay. This test proves both orderings still alarm
// exactly once. Unlike entryDeliverCount / entryConsumeCount / etc. (which
// have NO replaying gate at all and so DO double-count when Replay's bulk
// pass and a live Apply both process the same message — see
// TestFoldDoubleApply_SettleDeliverEntryCounter), recordFoldDenial's
// per-message-ID guard (foldDenialCounted) is defense-in-depth, not the sole
// mechanism preventing double-alarm for this specific race.
func TestFoldDoubleApply_RecordFoldDenial(t *testing.T) {
	operatorKey := newReservationID()
	forger := newReservationID()

	st := NewState()
	st.OperatorKey = operatorKey

	var denials int
	st.onFoldDenial = func(reason foldDenialReason, msg *Message) {
		if reason == foldDenialNotOperator {
			denials++
		}
	}

	forged := Message{
		ID:          newReservationID(),
		Sender:      forger, // NOT the operator — applySettleDeliver rejects and alarms.
		Tags:        []string{TagSettle, TagPhasePrefix + SettlePhaseStrDeliver},
		Antecedents: []string{newReservationID()},
		Timestamp:   time.Now().UnixNano(),
	}
	forged.Payload, _ = json.Marshal(map[string]any{
		"phase":       SettlePhaseStrDeliver,
		"content_ref": "sha256:deadbeef",
	})

	full := []Message{forged}

	// Concurrent rebuild wins the race first: a full Replay over the log,
	// which includes the forged message but — by the pre-existing
	// s.replaying design — contributes ZERO real alarm counts.
	st.Replay(full)
	if denials != 0 {
		t.Fatalf("after rebuild's full Replay, denials = %d, want 0 (Replay never alarms — s.replaying suppresses it by design)", denials)
	}

	// Poll loop resumes and applies its own stale reference to the SAME
	// forged message — the ONE live (non-Replay) Apply this message ever
	// receives across the system's lifetime.
	st.Apply(&full[0])

	if denials != 1 {
		t.Fatalf("denials = %d after poll-vs-rebuild interleave, want exactly 1", denials)
	}
}
