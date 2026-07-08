package exchange_test

// Test for dontguess-4127 (build-outcome 8, docs/design/relay-transport.md
// §E MUST-ENFORCE(2)): performScripSettlement must EMIT the scrip-settle
// convention message to the durable log BEFORE mutating any scrip balance.
// The previous ordering (ADV-12) mutated balances first and emitted last —
// a crash between the two left a live balance change with no durable
// record for Replay to reconstruct on restart, silently destroying the
// seller/operator credit.
//
// Proof strategy: inject a always-failing AddBudget (the balance-mutation
// step) via the same failingAddBudgetStore stub settle_failed_test.go
// already defines, and assert the scrip-settle message is STILL durably
// emitted despite the mutation failure.
//
// This is a genuine differential/regression test: under the PRE-FIX
// ordering (mutate-then-emit), an AddBudget failure aborts the settlement
// before the emit step is ever reached, so NO scrip-settle message would
// appear in the log for this exact failure injection. Under the FIXED
// ordering (emit-then-mutate), the emit succeeds (WriteClient itself is
// healthy — only AddBudget is stubbed to fail) and is durably recorded
// before the doomed mutation runs.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/campfire-net/campfire/cf-protocol/store"

	"github.com/campfire-net/dontguess/pkg/exchange"
	"github.com/campfire-net/dontguess/pkg/scrip"
)

func TestPerformScripSettlement_EmitsDurableRecordBeforeBalanceMutationFails(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	cs := newCampfireScripStore(t, h)

	// Phase 1: build the settle chain (put → accept → buy → match →
	// buyer-accept → deliver) using a real, healthy CampfireScripStore.
	eng1 := exchange.NewEngine(exchange.EngineOptions{
		CampfireID:  h.cfID,
		Store:       h.st,
		ReadClient:  h.newOperatorClient(),
		WriteClient: h.newOperatorClient(),
		ScripStore:  cs,
		Logger:      func(format string, args ...any) { t.Logf("[eng1] "+format, args...) },
	})
	_, deliverMsg, _ := buildSettleChainForPriceTests(t, h, eng1, cs, "emit-before-mutate fixture", 4000)

	resID := extractReservationIDFromLog(t, h)
	if resID == "" {
		t.Fatal("no reservation ID found in buy-hold log — buyer-accept scrip hold did not run")
	}

	// Phase 2: wire a second engine whose AddBudget always fails, wrapping
	// the same real store for every other operation (ConsumeReservation,
	// SaveReservation, GetBudget, DecrementBudget all delegate to cs).
	failStore := &failingAddBudgetStore{
		real: cs,
		err:  scrip.ErrConflict,
	}
	eng2 := exchange.NewEngine(exchange.EngineOptions{
		CampfireID:  h.cfID,
		Store:       h.st,
		ReadClient:  h.newOperatorClient(),
		WriteClient: h.newOperatorClient(),
		ScripStore:  failStore,
		Logger:      func(format string, args ...any) { t.Logf("[eng2] "+format, args...) },
	})

	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng2.State().Replay(exchange.FromStoreRecords(allMsgs))

	// Re-dispatch buyer-accept through eng2 so it detects the existing
	// reservation from the scrip-buy-hold log and populates matchToReservation
	// (engine-local state, not replay state).
	buyerAcceptMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{
		Tags: []string{exchange.TagSettle, exchange.TagPhasePrefix + exchange.SettlePhaseStrBuyerAccept},
	})
	for i := range buyerAcceptMsgs {
		r := buyerAcceptMsgs[i]
		if err := eng2.DispatchForTest(exchange.FromStoreRecord(&r)); err != nil {
			t.Logf("eng2: dispatch buyer-accept %s: %v (expected on re-run)", r.ID[:8], err)
		}
	}

	preScripSettle, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{scrip.TagScripSettle}})

	completePayload, _ := json.Marshal(map[string]any{"phase": "complete"})
	completeMsg := h.sendMessage(h.buyer, completePayload,
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrComplete,
		},
		[]string{deliverMsg.ID},
	)
	rec, err := h.st.GetMessage(completeMsg.ID)
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}

	// The dispatch must fail (AddBudget always errors) ...
	dispatchErr := eng2.DispatchForTest(exchange.FromStoreRecord(rec))
	if dispatchErr == nil {
		t.Fatal("expected DispatchForTest to return an error when AddBudget always fails, got nil")
	}

	// ... but the scrip-settle convention message must STILL have been
	// durably emitted — proving emit ran BEFORE (and independent of) the
	// doomed balance mutation.
	postScripSettle, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{scrip.TagScripSettle}})
	if len(postScripSettle) != len(preScripSettle)+1 {
		t.Fatalf("scrip-settle messages after failed-mutation settle = %d, want %d (exactly one new — "+
			"emit-durable-then-mutate ordering regression: emit did not happen before/independent of the failing mutation)",
			len(postScripSettle), len(preScripSettle)+1)
	}

	// Sanity: the emitted scrip-settle payload references the reservation
	// that was actually being settled.
	newSettleMsg := postScripSettle[len(postScripSettle)-1]
	var settlePayload scrip.SettlePayload
	if err := json.Unmarshal(newSettleMsg.Payload, &settlePayload); err != nil {
		t.Fatalf("parsing scrip-settle payload: %v", err)
	}
	if settlePayload.ReservationID != resID {
		t.Errorf("emitted scrip-settle reservation_id = %q, want %q", settlePayload.ReservationID, resID)
	}

	// And the reservation itself must NOT have been left consumed-and-lost:
	// since the balance mutation failed, creditResidualToSeller restores it
	// via SaveReservation so the settle can be retried.
	if _, err := cs.GetReservation(context.Background(), resID); err != nil {
		t.Errorf("expected reservation %s to be restored after balance-mutation failure, got: %v", resID, err)
	}
}
