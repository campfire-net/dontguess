package exchange_test

// TestSettle_AddBudgetFailure_EmitsSettleFailed verifies that when
// ScripStore.AddBudget returns an error during settle(complete), the engine
// emits a settle(failed) campfire message to the buyer and does NOT silently
// succeed (dontguess-234).
//
// Without this fix, a failed AddBudget was logged and ignored: the buyer
// received no observable signal that the settle did not complete. With the
// fix, a settle(failed) message with an error_code field is emitted so the
// buyer can observe the failure and retry.
//
// Test strategy:
//  1. Build a full settle chain (put → accept → buy → match → buyer-accept →
//     deliver) using a real CampfireScripStore so the reservation is created.
//  2. Wire a second engine with a failingAddBudgetStore that wraps the real
//     CampfireScripStore for ConsumeReservation but injects errors on AddBudget.
//  3. Replay all state into the second engine, then dispatch settle(complete).
//  4. Assert: a settle(failed) message with TagPhasePrefix+"failed" is emitted
//     to the campfire. Buyer key and reservation_id must appear in its payload.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/campfire-net/campfire/pkg/store"

	"github.com/3dl-dev/dontguess/pkg/exchange"
	"github.com/3dl-dev/dontguess/pkg/scrip"
)

// failingAddBudgetStore wraps a real ScripStore but returns errAddBudget on
// every AddBudget call. All other operations delegate to the underlying store.
type failingAddBudgetStore struct {
	real scrip.SpendingStore
	err  error
}

func (s *failingAddBudgetStore) AddBudget(_ context.Context, _, _ string, _ int64, _ string) (int64, string, error) {
	return 0, "", s.err
}

func (s *failingAddBudgetStore) DecrementBudget(ctx context.Context, pk, rk string, amount int64, etag string) (int64, string, error) {
	return s.real.DecrementBudget(ctx, pk, rk, amount, etag)
}

func (s *failingAddBudgetStore) GetBudget(ctx context.Context, pk, rk string) (int64, string, error) {
	return s.real.GetBudget(ctx, pk, rk)
}

func (s *failingAddBudgetStore) SaveReservation(ctx context.Context, r scrip.Reservation) error {
	return s.real.SaveReservation(ctx, r)
}

func (s *failingAddBudgetStore) GetReservation(ctx context.Context, id string) (scrip.Reservation, error) {
	return s.real.GetReservation(ctx, id)
}

func (s *failingAddBudgetStore) DeleteReservation(ctx context.Context, id string) error {
	return s.real.DeleteReservation(ctx, id)
}

func (s *failingAddBudgetStore) ConsumeReservation(ctx context.Context, id string) (scrip.Reservation, error) {
	return s.real.ConsumeReservation(ctx, id)
}

// TestSettle_AddBudgetFailure_EmitsSettleFailed is the main test.
func TestSettle_AddBudgetFailure_EmitsSettleFailed(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	cs := newCampfireScripStore(t, h)

	// Phase 1: build the settle chain (put → accept → buy → match →
	// buyer-accept → deliver) using a real CampfireScripStore.
	eng1 := exchange.NewEngine(exchange.EngineOptions{
		CampfireID:  h.cfID,
		Store:       h.st,
		ReadClient:  h.newOperatorClient(),
		WriteClient: h.newOperatorClient(),
		ScripStore:  cs,
		Logger:      func(format string, args ...any) { t.Logf("[eng1] "+format, args...) },
	})

	_, deliverMsg, _ := buildSettleChainForPriceTests(t, h, eng1, cs, "GraphQL schema migration helper", 4000)

	// Extract the reservation ID from the buy-hold log so we can verify it
	// appears in the settle(failed) payload.
	resID := extractReservationIDFromLog(t, h)
	if resID == "" {
		t.Fatal("no reservation ID found in buy-hold log — buyer-accept scrip hold did not run")
	}

	// Phase 2: wire a second engine with a failing AddBudget stub that wraps
	// the real store for reservation operations.
	failStore := &failingAddBudgetStore{
		real: cs,
		err:  scrip.ErrConflict, // arbitrary non-nil error
	}
	eng2 := exchange.NewEngine(exchange.EngineOptions{
		CampfireID:  h.cfID,
		Store:       h.st,
		ReadClient:  h.newOperatorClient(),
		WriteClient: h.newOperatorClient(),
		ScripStore:  failStore,
		Logger:      func(format string, args ...any) { t.Logf("[eng2] "+format, args...) },
	})

	// Replay all state into eng2 so it knows about the antecedent chain,
	// inventory, and buyer mappings.
	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng2.State().Replay(exchange.FromStoreRecords(allMsgs))

	// matchToReservation is engine state (not replay state): it is populated when
	// handleSettleBuyerAcceptScrip dispatches. Dispatch the buyer-accept message
	// through eng2 so it detects the existing reservation from the scrip-buy-hold
	// log and sets up the mapping. The failingAddBudgetStore delegates
	// ConsumeReservation / GetBudget / DecrementBudget to the real store, so the
	// replay path (which calls GetBudget + SaveReservation) succeeds.
	buyerAcceptMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{
		Tags: []string{exchange.TagSettle, exchange.TagPhasePrefix + exchange.SettlePhaseStrBuyerAccept},
	})
	for i := range buyerAcceptMsgs {
		r := buyerAcceptMsgs[i]
		if err := eng2.DispatchForTest(exchange.FromStoreRecord(&r)); err != nil {
			t.Logf("eng2: dispatch buyer-accept %s: %v (expected on re-run)", r.ID[:8], err)
		}
	}

	// Send and dispatch settle(complete) via eng2.
	completePre, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{
		Tags: []string{exchange.TagSettle, exchange.TagPhasePrefix + exchange.SettlePhaseStrFailed},
	})

	completePayload, _ := json.Marshal(map[string]any{
		"phase": "complete",
	})
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

	// Dispatch should return an error (AddBudget failed).
	dispatchErr := eng2.DispatchForTest(exchange.FromStoreRecord(rec))
	if dispatchErr == nil {
		t.Fatal("expected DispatchForTest to return error when AddBudget fails, got nil")
	}

	// A settle(failed) message must have been emitted.
	deadline := time.Now().Add(5 * time.Second)
	var failedMsg *store.MessageRecord
	for time.Now().Before(deadline) {
		msgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{
			Tags: []string{exchange.TagSettle, exchange.TagPhasePrefix + exchange.SettlePhaseStrFailed},
		})
		if len(msgs) > len(completePre) {
			last := msgs[len(msgs)-1]
			failedMsg = &last
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if failedMsg == nil {
		t.Fatal("no settle(failed) message emitted after AddBudget failure — buyer has no observable signal")
	}

	// The payload must include an error_code and the buyer key.
	var payload map[string]any
	if err := json.Unmarshal(failedMsg.Payload, &payload); err != nil {
		t.Fatalf("settle(failed) payload is not valid JSON: %v", err)
	}

	if payload["error_code"] == nil || payload["error_code"] == "" {
		t.Errorf("settle(failed) payload missing error_code: %v", payload)
	}

	buyerField, _ := payload["buyer"].(string)
	if buyerField != h.buyer.PublicKeyHex() {
		t.Errorf("settle(failed) payload buyer = %q, want %q", buyerField, h.buyer.PublicKeyHex())
	}

	resIDField, _ := payload["reservation_id"].(string)
	if resIDField != resID {
		t.Errorf("settle(failed) payload reservation_id = %q, want %q", resIDField, resID)
	}
}
