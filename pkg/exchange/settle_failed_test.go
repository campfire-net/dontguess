package exchange_test

// TestSettle_SellerAddBudgetFailureAfterDurableEmit_NoRestoreNoSettleFailed
// verifies the post-durable-emit invariant (dontguess-4be, relay-transport.md
// §E). performScripSettlement emits the authoritative scrip-settle BEFORE it
// credits the seller. Once that durable record lands, a subsequent AddBudget
// (seller credit) failure MUST be a LOUD hard error that:
//   - does NOT restore the reservation (a restored reservation lets the buyer
//     retry and emit a SECOND scrip-settle → Replay double-mint), and
//   - does NOT emit settle(failed) (the durable log must never hold a
//     contradictory settled + settle(failed) for one reservation).
//
// This supersedes the old dontguess-234 behavior (emit settle(failed) + restore
// on AddBudget failure), which was correct only under the pre-4127 mutate-then-
// emit ordering. Now the scrip-settle is durable first, so the settle is
// authoritative and the missing live credit is reconciled from the log on the
// next Replay — never retried.
//
// Test strategy:
//  1. Build a full settle chain (put → accept → buy → match → buyer-accept →
//     deliver) using a real CampfireScripStore so the reservation is created.
//  2. Wire a second engine with a failingAddBudgetStore that wraps the real
//     CampfireScripStore for ConsumeReservation but injects errors on AddBudget.
//  3. Replay all state into the second engine, then dispatch settle(complete).
//  4. Assert: dispatch returns a loud error; exactly one durable scrip-settle
//     is recorded; NO settle(failed) is emitted; the reservation is NOT restored.

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/campfire-net/dontguess/pkg/store"

	"github.com/campfire-net/dontguess/pkg/exchange"
	"github.com/campfire-net/dontguess/pkg/scrip"
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

// TestSettle_SellerAddBudgetFailureAfterDurableEmit_NoRestoreNoSettleFailed is the main test.
func TestSettle_SellerAddBudgetFailureAfterDurableEmit_NoRestoreNoSettleFailed(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	cs := newCampfireScripStore(t, h)

	// Phase 1: build the settle chain (put → accept → buy → match →
	// buyer-accept → deliver) using a real CampfireScripStore.
	eng1 := exchange.NewEngine(exchange.EngineOptions{
		CampfireID:        h.cfID,
		LocalStore:        h.st,
		OperatorPublicKey: h.operator.pubKeyHex,
		ScripStore:        cs,
		Logger:            func(format string, args ...any) { t.Logf("[eng1] "+format, args...) },
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
		CampfireID:        h.cfID,
		LocalStore:        h.st,
		OperatorPublicKey: h.operator.pubKeyHex,
		ScripStore:        failStore,
		Logger:            func(format string, args ...any) { t.Logf("[eng2] "+format, args...) },
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

	// Send and dispatch settle(complete) via eng2. Snapshot both the
	// settle(failed) count (must not grow) and the durable scrip-settle count
	// (must grow by exactly one — the authoritative record emitted BEFORE the
	// doomed AddBudget).
	failedPre, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{
		// Filter by the failed-phase tag ALONE. The store's Tags filter is
		// OR-semantics, so listing TagSettle too would also match the
		// settle(complete)/deliver/etc. messages and inflate the count. Only
		// settle(failed) messages carry this phase tag.
		Tags: []string{exchange.TagPhasePrefix + exchange.SettlePhaseStrFailed},
	})
	settlePre, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{scrip.TagScripSettle}})

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

	// Dispatch must return a LOUD hard error (post-emit AddBudget failed).
	dispatchErr := eng2.DispatchForTest(exchange.FromStoreRecord(rec))
	if dispatchErr == nil {
		t.Fatal("expected DispatchForTest to return a loud error when post-emit AddBudget fails, got nil")
	}

	// The authoritative scrip-settle must have been durably emitted exactly once
	// (before the failing credit) — Replay will reconcile the missing live credit.
	settlePost, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{scrip.TagScripSettle}})
	if len(settlePost) != len(settlePre)+1 {
		t.Fatalf("durable scrip-settle count = %d, want %d (exactly one authoritative record emitted before the failing credit)",
			len(settlePost), len(settlePre)+1)
	}

	// NO settle(failed) may be emitted — the durable log must never hold a
	// contradictory settled + settle(failed)-retry for one reservation. Give any
	// (erroneously) emitted settle(failed) time to appear before asserting absence.
	time.Sleep(200 * time.Millisecond)
	failedPost, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{
		// Filter by the failed-phase tag ALONE. The store's Tags filter is
		// OR-semantics, so listing TagSettle too would also match the
		// settle(complete)/deliver/etc. messages and inflate the count. Only
		// settle(failed) messages carry this phase tag.
		Tags: []string{exchange.TagPhasePrefix + exchange.SettlePhaseStrFailed},
	})
	if len(failedPost) != len(failedPre) {
		t.Errorf("settle(failed) count = %d, want %d — a post-durable-emit AddBudget failure must NOT emit "+
			"settle(failed) (would contradict the durable scrip-settle and let the buyer retry → double-mint)",
			len(failedPost), len(failedPre))
	}

	// The reservation must NOT be restored — it is settled and gone. A restored
	// reservation is the divergent-recovery regression that enables double-mint.
	if _, err := cs.GetReservation(context.Background(), resID); err == nil {
		t.Errorf("reservation %s was restored after a post-durable-emit AddBudget failure — "+
			"divergent-recovery regression (enables retry → double-mint)", resID)
	}
}

// sellerOkOperatorFailStore lets the first AddBudget call (seller credit) succeed
// by delegating to the real store, then injects an error on the second call
// (operator revenue). DecrementBudget always delegates to the real store so the
// seller-credit rollback path can execute normally.
type sellerOkOperatorFailStore struct {
	real        scrip.SpendingStore
	addBudgetN  atomic.Int32 // counts AddBudget calls
	operatorErr error
}

func (s *sellerOkOperatorFailStore) AddBudget(ctx context.Context, pk, rk string, amount int64, etag string) (int64, string, error) {
	n := s.addBudgetN.Add(1)
	if n > 1 {
		// Second and subsequent calls (operator AddBudget) → fail.
		return 0, "", s.operatorErr
	}
	// First call (seller AddBudget) → succeed.
	return s.real.AddBudget(ctx, pk, rk, amount, etag)
}

func (s *sellerOkOperatorFailStore) DecrementBudget(ctx context.Context, pk, rk string, amount int64, etag string) (int64, string, error) {
	return s.real.DecrementBudget(ctx, pk, rk, amount, etag)
}

func (s *sellerOkOperatorFailStore) GetBudget(ctx context.Context, pk, rk string) (int64, string, error) {
	return s.real.GetBudget(ctx, pk, rk)
}

func (s *sellerOkOperatorFailStore) SaveReservation(ctx context.Context, r scrip.Reservation) error {
	return s.real.SaveReservation(ctx, r)
}

func (s *sellerOkOperatorFailStore) GetReservation(ctx context.Context, id string) (scrip.Reservation, error) {
	return s.real.GetReservation(ctx, id)
}

func (s *sellerOkOperatorFailStore) DeleteReservation(ctx context.Context, id string) error {
	return s.real.DeleteReservation(ctx, id)
}

func (s *sellerOkOperatorFailStore) ConsumeReservation(ctx context.Context, id string) (scrip.Reservation, error) {
	return s.real.ConsumeReservation(ctx, id)
}

// TestSettle_OperatorAddBudgetFailureAfterDurableEmit_NoRollbackNoRestore
// verifies the post-durable-emit invariant (dontguess-4be) for a partial-credit
// failure: seller AddBudget succeeds, operator AddBudget fails. Because the
// authoritative scrip-settle was durably emitted FIRST and applySettle re-credits
// BOTH residual (seller) and revenue (operator) from that one record on every
// Replay, the engine must NOT try to "undo" the partial live credit. Specifically
// it must:
//  1. NOT roll back the seller credit (Replay re-adds it; a live rollback just
//     diverges live state further from the authoritative log).
//  2. NOT restore the reservation (a restored reservation → retry → second
//     scrip-settle → double-mint).
//  3. NOT emit settle(failed) (no contradictory settled + settle(failed)).
//
// The operator's missing live credit is reconciled from the durable log on the
// next Replay. The failure surfaces as a loud dispatch error.
//
// This inverts the old dontguess-234 rollback behavior, which was correct only
// under the pre-4127 mutate-then-emit ordering.
func TestSettle_OperatorAddBudgetFailureAfterDurableEmit_NoRollbackNoRestore(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	cs := newCampfireScripStore(t, h)

	// Phase 1: build the settle chain using a real CampfireScripStore.
	eng1 := exchange.NewEngine(exchange.EngineOptions{
		CampfireID:        h.cfID,
		LocalStore:        h.st,
		OperatorPublicKey: h.operator.pubKeyHex,
		ScripStore:        cs,
		Logger:            func(format string, args ...any) { t.Logf("[eng1] "+format, args...) },
	})

	_, deliverMsg, _ := buildSettleChainForPriceTests(t, h, eng1, cs, "operator-rollback test entry", 4000)

	resID := extractReservationIDFromLog(t, h)
	if resID == "" {
		t.Fatal("no reservation ID found in buy-hold log")
	}

	// Record seller balance before the failing settle attempt.
	sellerBalBefore, _, err := cs.GetBudget(context.Background(), h.seller.PublicKeyHex(), scrip.BalanceKey)
	if err != nil {
		t.Fatalf("GetBudget(seller) before settle: %v", err)
	}

	// Phase 2: wire a second engine whose AddBudget lets seller succeed but
	// rejects operator with ErrConflict.
	failStore := &sellerOkOperatorFailStore{
		real:        cs,
		operatorErr: scrip.ErrConflict,
	}
	eng2 := exchange.NewEngine(exchange.EngineOptions{
		CampfireID:        h.cfID,
		LocalStore:        h.st,
		OperatorPublicKey: h.operator.pubKeyHex,
		ScripStore:        failStore,
		Logger:            func(format string, args ...any) { t.Logf("[eng2] "+format, args...) },
	})

	// Replay all state into eng2.
	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng2.State().Replay(exchange.FromStoreRecords(allMsgs))

	// Re-dispatch buyer-accept through eng2 to establish matchToReservation.
	buyerAcceptMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{
		Tags: []string{exchange.TagSettle, exchange.TagPhasePrefix + exchange.SettlePhaseStrBuyerAccept},
	})
	for i := range buyerAcceptMsgs {
		r := buyerAcceptMsgs[i]
		if err := eng2.DispatchForTest(exchange.FromStoreRecord(&r)); err != nil {
			t.Logf("eng2: dispatch buyer-accept %s: %v (expected on re-run)", r.ID[:8], err)
		}
	}

	failedPre, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{
		// Filter by the failed-phase tag ALONE. The store's Tags filter is
		// OR-semantics, so listing TagSettle too would also match the
		// settle(complete)/deliver/etc. messages and inflate the count. Only
		// settle(failed) messages carry this phase tag.
		Tags: []string{exchange.TagPhasePrefix + exchange.SettlePhaseStrFailed},
	})
	settlePre, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{scrip.TagScripSettle}})

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

	dispatchErr := eng2.DispatchForTest(exchange.FromStoreRecord(rec))
	if dispatchErr == nil {
		t.Fatal("expected DispatchForTest to return a loud error when operator AddBudget fails, got nil")
	}

	// The authoritative scrip-settle must be durably emitted exactly once.
	settlePost, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{scrip.TagScripSettle}})
	if len(settlePost) != len(settlePre)+1 {
		t.Fatalf("durable scrip-settle count = %d, want %d (exactly one authoritative record)",
			len(settlePost), len(settlePre)+1)
	}

	// NO settle(failed) may be emitted (no contradictory settled + settle(failed)).
	time.Sleep(200 * time.Millisecond)
	failedPost, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{
		// Filter by the failed-phase tag ALONE. The store's Tags filter is
		// OR-semantics, so listing TagSettle too would also match the
		// settle(complete)/deliver/etc. messages and inflate the count. Only
		// settle(failed) messages carry this phase tag.
		Tags: []string{exchange.TagPhasePrefix + exchange.SettlePhaseStrFailed},
	})
	if len(failedPost) != len(failedPre) {
		t.Errorf("settle(failed) count = %d, want %d — a post-durable-emit failure must NOT emit settle(failed)",
			len(failedPost), len(failedPre))
	}

	// Seller balance must be INCREASED (credited, NOT rolled back). The durable
	// scrip-settle is authoritative and Replay re-credits the seller; a live
	// rollback would fight the log. A restored/rolled-back state is the
	// divergent-recovery regression.
	sellerBalAfter, _, err := cs.GetBudget(context.Background(), h.seller.PublicKeyHex(), scrip.BalanceKey)
	if err != nil {
		t.Fatalf("GetBudget(seller) after settle: %v", err)
	}
	if sellerBalAfter <= sellerBalBefore {
		t.Errorf("seller balance not credited: before=%d after=%d — a post-durable-emit operator failure must "+
			"NOT roll back the already-applied seller credit (Replay reconciles from the authoritative log)",
			sellerBalBefore, sellerBalAfter)
	}

	// The reservation must NOT be restored — it is settled and gone.
	if _, resErr := cs.GetReservation(context.Background(), resID); resErr == nil {
		t.Errorf("reservation %s was restored after operator AddBudget failure — divergent-recovery regression "+
			"(enables retry → double-mint)", resID)
	}
}
