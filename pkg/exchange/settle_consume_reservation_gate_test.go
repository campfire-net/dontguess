package exchange_test

// Tests for dontguess-4127 (build-outcome 8, docs/design/relay-transport.md §E
// MUST-ENFORCE(3)): the reservation-consumed check must run BEFORE
// emitConsumeSignal, not after. Without this fix, a redelivered
// settle(complete) — or a settle(complete) whose match never had a live
// buyer-accept scrip hold — emits a spurious/duplicate exchange:consume
// behavioral signal even though no (or no further) scrip settlement occurs
// for it (ADV-7).
//
// Both tests use a real CampfireScripStore (no mocks at the fold boundary),
// matching the design's test-strategy requirement (§5): "no mocks at the
// fold boundary."

import (
	"encoding/json"
	"testing"

	"github.com/campfire-net/campfire/cf-protocol/store"

	"github.com/campfire-net/dontguess/pkg/exchange"
)

// TestSettleComplete_RedeliveryDoesNotDoubleEmitConsumeSignal verifies that
// dispatching the SAME settle(complete) message twice (simulating
// at-least-once redelivery) emits exactly ONE exchange:consume message.
// The first dispatch consumes the reservation and settles scrip; the
// reservation is gone by the second dispatch, so — after the dontguess-4127
// fix — the second dispatch must NOT call emitConsumeSignal at all.
//
// Before the fix: emitConsumeSignal ran unconditionally ahead of the
// reservation-consumed check, so the second dispatch emitted a second
// exchange:consume message even though the underlying scrip settlement was
// correctly a no-op (ConsumeReservation returns "not found").
func TestSettleComplete_RedeliveryDoesNotDoubleEmitConsumeSignal(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	cs := newCampfireScripStore(t, h)
	eng := exchange.NewEngine(exchange.EngineOptions{
		CampfireID:  h.cfID,
		Store:       h.st,
		ReadClient:  h.newOperatorClient(),
		WriteClient: h.newOperatorClient(),
		ScripStore:  cs,
		Logger: func(format string, args ...any) {
			t.Logf("[engine] "+format, args...)
		},
	})

	_, deliverMsg, _ := buildSettleChainForPriceTests(t, h, eng, cs, "flock contention retry helper", 4000)

	completePayload, _ := json.Marshal(map[string]any{"phase": "complete"})
	completeMsg := h.sendMessage(h.buyer, completePayload,
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrComplete,
		},
		[]string{deliverMsg.ID},
	)

	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	rec, err := h.st.GetMessage(completeMsg.ID)
	if err != nil {
		t.Fatalf("GetMessage(complete): %v", err)
	}
	completeRecord := exchange.FromStoreRecord(rec)

	preConsume, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagConsume}})

	// First dispatch: settles scrip AND emits the consume signal.
	if err := eng.DispatchForTest(completeRecord); err != nil {
		t.Fatalf("first DispatchForTest(complete): %v", err)
	}

	afterFirst, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagConsume}})
	if len(afterFirst) != len(preConsume)+1 {
		t.Fatalf("consume messages after first dispatch = %d, want %d (exactly one new)",
			len(afterFirst), len(preConsume)+1)
	}

	// Second dispatch of the SAME message (redelivery). The reservation is
	// already consumed — must NOT emit a second consume signal.
	if err := eng.DispatchForTest(completeRecord); err != nil {
		t.Fatalf("second DispatchForTest(complete) (redelivery): %v", err)
	}

	afterSecond, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagConsume}})
	if len(afterSecond) != len(afterFirst) {
		t.Errorf("consume messages after redelivery = %d, want %d (redelivery must not double-emit; ADV-7 regression)",
			len(afterSecond), len(afterFirst))
	}
}

// TestSettleComplete_NoConsumeSignalWithoutLiveReservation verifies that when
// ScripStore is configured but the settle(complete)'s match never had a
// buyer-accept scrip hold dispatched through the engine (so
// matchToReservation has no live entry for it), emitConsumeSignal is
// skipped entirely.
//
// Contrast: pkg/exchange/consume_signal_test.go proves a consume signal DOES
// fire for the identical scenario when NO ScripStore is configured at all
// (the "fires unconditionally without ScripStore" branch is untouched by
// this fix). This test proves the differential behavior introduced by
// dontguess-4127: WITH a ScripStore configured, an absent reservation now
// gates the signal too.
func TestSettleComplete_NoConsumeSignalWithoutLiveReservation(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	cs := newCampfireScripStore(t, h)
	eng := exchange.NewEngine(exchange.EngineOptions{
		CampfireID:  h.cfID,
		Store:       h.st,
		ReadClient:  h.newOperatorClient(),
		WriteClient: h.newOperatorClient(),
		ScripStore:  cs,
		Logger: func(format string, args ...any) {
			t.Logf("[engine] "+format, args...)
		},
	})

	matchMsg, entryID := setupMatchedOrder(t, h, eng)

	// Buyer-accept is sent and folded into STATE (so deliver/complete can
	// resolve their antecedent chain), but deliberately never DISPATCHED
	// through the engine — so handleSettleBuyerAcceptScrip never runs and
	// e.matchToReservation never gets an entry for this match. This is the
	// "absent reservation" case (as opposed to the redelivery test's
	// "already-consumed" case).
	buyerAcceptMsg := h.sendMessage(h.buyer, buyerAcceptPayloadFor(entryID),
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrBuyerAccept,
			exchange.TagVerdictPrefix + "accepted",
		},
		[]string{matchMsg.ID},
	)
	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	deliverMsg := h.sendMessage(h.operator, deliverPayloadFor(entryID),
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrDeliver,
		},
		[]string{buyerAcceptMsg.ID},
	)
	allMsgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	completeMsg := h.sendMessage(h.buyer, completePayloadFor(entryID, 12000),
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrComplete,
			exchange.TagVerdictPrefix + "accepted",
		},
		[]string{deliverMsg.ID},
	)
	allMsgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	preConsume, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagConsume}})

	rec, err := h.st.GetMessage(completeMsg.ID)
	if err != nil {
		t.Fatalf("GetMessage(complete): %v", err)
	}
	if err := eng.DispatchForTest(exchange.FromStoreRecord(rec)); err != nil {
		t.Fatalf("DispatchForTest(complete): %v", err)
	}

	afterConsume, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagConsume}})
	if len(afterConsume) != len(preConsume) {
		t.Errorf("consume messages = %d, want %d (no live reservation — consume signal must be skipped)",
			len(afterConsume), len(preConsume))
	}
}
