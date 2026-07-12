package exchange_test

// TestEngine_HandlerCancellationOnShutdown verifies that handler scrip operations
// are cancelled when the engine's shutdown context is cancelled.
//
// Before this fix, handlers created context.Background() independently, so
// cancelling the engine's ctx had no effect on in-flight scrip calls.
// After the fix, handlers use engineCtx(), which is the context passed to Start().
//
// The test strategy: a blocking mock SpendingStore blocks GetBudget until
// its context is cancelled. The test sends a buy message (no scrip at buy time),
// waits for a match, then posts a buyer-accept message which triggers
// handleSettleBuyerAcceptScrip → GetBudget (the blocking call). The test then
// cancels the engine ctx and verifies that the blocked GetBudget returns
// context.Canceled (not hanging).

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/campfire-net/dontguess/pkg/store"

	"github.com/campfire-net/dontguess/pkg/exchange"
	"github.com/campfire-net/dontguess/pkg/scrip"
)

// blockingScripStore is a mock SpendingStore that blocks GetBudget until
// the context passed to it is cancelled. It records the context so tests
// can verify it is the engine's shutdown context and not context.Background().
type blockingScripStore struct {
	// getBudgetCtxCh receives the ctx passed to GetBudget.
	getBudgetCtxCh chan context.Context
	// unblock, when closed, allows GetBudget to return immediately (bypass blocking).
	// Used for other methods that should succeed without blocking.
	unblock chan struct{}
}

func newBlockingScripStore() *blockingScripStore {
	return &blockingScripStore{
		getBudgetCtxCh: make(chan context.Context, 1),
		unblock:        make(chan struct{}),
	}
}

func (b *blockingScripStore) GetBudget(ctx context.Context, pk, rk string) (int64, string, error) {
	// Signal the context so the test can inspect it.
	select {
	case b.getBudgetCtxCh <- ctx:
	default:
	}
	// Block until either the ctx is cancelled or unblock is closed.
	select {
	case <-ctx.Done():
		return 0, "", ctx.Err()
	case <-b.unblock:
		return 1_000_000, "etag-1", nil
	}
}

func (b *blockingScripStore) DecrementBudget(ctx context.Context, pk, rk string, amount int64, etag string) (int64, string, error) {
	select {
	case <-ctx.Done():
		return 0, "", ctx.Err()
	case <-b.unblock:
	}
	return 1_000_000 - amount, "etag-2", nil
}

func (b *blockingScripStore) AddBudget(ctx context.Context, pk, rk string, amount int64, etag string) (int64, string, error) {
	return amount, "etag-3", nil
}

func (b *blockingScripStore) SaveReservation(ctx context.Context, r scrip.Reservation) error {
	return nil
}

func (b *blockingScripStore) GetReservation(ctx context.Context, id string) (scrip.Reservation, error) {
	return scrip.Reservation{}, scrip.ErrReservationNotFound
}

func (b *blockingScripStore) DeleteReservation(ctx context.Context, id string) error {
	return scrip.ErrReservationNotFound
}

func (b *blockingScripStore) ConsumeReservation(ctx context.Context, id string) (scrip.Reservation, error) {
	return scrip.Reservation{}, scrip.ErrReservationNotFound
}

// TestEngine_HandlerCancellationOnShutdown verifies that when the engine's
// shutdown context is cancelled, in-flight handler scrip operations receive
// a cancelled context rather than blocking forever on context.Background().
func TestEngine_HandlerCancellationOnShutdown(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	bs := newBlockingScripStore()

	// Build via the harness so the engine gets the finished-GUARDED logger
	// (newEngineWithOpts): a straggler dispatch goroutine that logs after the test
	// completes must not call t.Logf — that panics the whole suite (dontguess-fe9f).
	// A bare `Logger: t.Logf` here (as this test used to have) is exactly that
	// landmine: the post-cancel "dispatch error: context canceled" line fires from
	// a goroutine after the test returns.
	//
	// The whole ingest is driven DETERMINISTICALLY via DispatchForTest (below) — the
	// canonical synchronous fold→dispatch path ~40 other engine tests use — instead
	// of racing the background poll ticker against a wall-clock deadline. The former
	// ticker-driven shape FALSE-failed under full-suite -race + CPU saturation with
	// "timed out waiting for GetBudget" whenever the 25ms poll goroutine was starved
	// past the deadline (the "times out under load" symptom, dontguess-fe9f). Two
	// facts make the deterministic drive safe: (1) OnStarted gives a real
	// startup-complete barrier, so DispatchForTest cannot race Start's startup fold /
	// cursor seed; (2) PollInterval is effectively-infinite so the background poll
	// loop never concurrently dispatches from the store — DispatchForTest is the sole
	// dispatcher, coupling claim+execute in one goroutine.
	started := make(chan struct{})
	eng := h.newEngineWithOpts(func(o *exchange.EngineOptions) {
		o.ScripStore = bs
		o.PollInterval = time.Hour
		o.OnStarted = func() { close(started) }
	})

	// Seed one inventory entry so handleBuy finds a candidate and emits a match.
	putMsg := h.sendMessage(h.seller,
		putPayload("cancellation test entry", fmt.Sprintf("sha256:%064x", 42), "code", 10000, 8192),
		[]string{exchange.TagPut, "exchange:content-type:code"},
		nil,
	)
	msgs, err := h.st.ListMessages(h.cfID, 0)
	if err != nil {
		t.Fatalf("listing messages: %v", err)
	}
	eng.State().Replay(exchange.FromStoreRecords(msgs))
	if err := eng.AutoAcceptPut(putMsg.ID, 5000, time.Now().Add(72*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut: %v", err)
	}

	inv := eng.State().Inventory()
	if len(inv) == 0 {
		t.Fatal("expected inventory entry after AutoAcceptPut")
	}
	entryID := inv[0].EntryID

	// Engine context — will be cancelled to trigger shutdown. Start stores it, so
	// engineCtx() (the ctx handlers use for scrip ops) is this cancellable ctx.
	engineCtx, cancelEngine := context.WithCancel(context.Background())
	defer cancelEngine()

	// Run the engine in the background.
	engineDone := make(chan error, 1)
	go func() {
		engineDone <- eng.Start(engineCtx)
	}()

	// Barrier: block until Start has completed its startup fold, cursor seed, and
	// dispatchPendingOrders (OnStarted fires the instant before the poll loop). After
	// this, DispatchForTest cannot race the startup cursor writes.
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("engine did not signal startup within 5s")
	}

	// Dispatch the buy SYNCHRONOUSLY via replayAndDispatch (the canonical fold→
	// dispatch helper): it folds the full log into state — so the buy is present
	// for applyMatch to bind matchToBuyer — THEN runs handleBuy, which emits (and
	// applies to state) the match. handleBuy does not call GetBudget, so this does
	// not block. The poll loop (hour interval) never dispatches from the store, so
	// this synchronous path is the sole dispatch.
	buyPayloadBytes, _ := json.Marshal(map[string]any{
		"task":        "cancellation test task",
		"budget":      int64(100_000),
		"max_results": 3,
	})
	buyMsg := h.sendMessage(h.buyer, buyPayloadBytes, []string{exchange.TagBuy}, nil)
	if err := replayAndDispatch(t, h, eng, buyMsg); err != nil {
		t.Fatalf("dispatching buy: %v", err)
	}

	// The match handleBuy emitted is now durably on the log and bound in state.
	matchMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})
	if len(matchMsgs) == 0 {
		t.Fatal("expected a match message after dispatching the buy")
	}
	matchMsgID := matchMsgs[len(matchMsgs)-1].ID

	// Build the buyer-accept referencing the match and FOLD it into state (Replay),
	// so its dispatch resolves the match exactly as the poll loop's fold→dispatch
	// would. Dispatching it calls handleSettleBuyerAcceptScrip → GetBudget, which
	// BLOCKS because bs.unblock is never closed, so dispatch runs in a goroutine.
	// Its DispatchForTest uses engineCtx() (the ctx Start stored) — exactly what
	// this test asserts gets cancelled. (Fold in the MAIN goroutine; a t.Fatalf
	// from replayAndDispatch inside the spawned goroutine would be illegal.)
	buyerAcceptPayloadBytes, _ := json.Marshal(map[string]any{
		"phase":    "buyer-accept",
		"entry_id": entryID,
		"accepted": true,
	})
	buyerAcceptMsg := h.sendMessage(h.buyer, buyerAcceptPayloadBytes,
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrBuyerAccept,
			exchange.TagVerdictPrefix + "accepted",
		},
		[]string{matchMsgID},
	)
	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))
	driverDone := make(chan struct{})
	go func() {
		defer close(driverDone)
		// Returns (with a context.Canceled dispatch error) once the engine ctx is
		// cancelled below; until then it is blocked inside GetBudget.
		_ = eng.DispatchForTest(buyerAcceptMsg)
	}()

	// Wait for GetBudget to be called (the handler is now blocked inside GetBudget).
	var capturedCtx context.Context
	select {
	case capturedCtx = <-bs.getBudgetCtxCh:
		// GetBudget was called — the handler is now blocked waiting on ctx or unblock.
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for GetBudget to be called — engine may not have dispatched the buyer-accept")
	}

	// The context captured inside GetBudget must NOT already be cancelled.
	if capturedCtx.Err() != nil {
		t.Fatalf("expected non-cancelled ctx at GetBudget call time, got: %v", capturedCtx.Err())
	}

	// Cancel the engine's shutdown context.
	cancelEngine()

	// The blocked GetBudget should return promptly because its ctx was cancelled.
	select {
	case <-capturedCtx.Done():
		// Context was propagated correctly — capturedCtx is the engine's ctx.
	case <-time.After(2 * time.Second):
		t.Fatal("handler context was not cancelled after engine shutdown — context.Background() is still being used instead of engine ctx")
	}

	// Engine run loop should also exit.
	select {
	case err := <-engineDone:
		if err != context.Canceled {
			t.Fatalf("engine exited with unexpected error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("engine did not exit after context cancellation")
	}

	// Join the buyer-accept dispatch goroutine so it cannot outlive the test and
	// log from a straggler goroutine (dontguess-fe9f). Its blocked DispatchForTest
	// returns the instant GetBudget observes the cancelled ctx above.
	select {
	case <-driverDone:
	case <-time.After(2 * time.Second):
		t.Fatal("buyer-accept dispatch goroutine did not exit after context cancellation")
	}
}

// TestEngine_HandlerCtxIsBackground_BeforeStart verifies that calling a handler
// via DispatchForTest before Start() still works (falls back to context.Background()).
// This tests the engineCtx() fallback path.
func TestEngine_HandlerCtxIsBackground_BeforeStart(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)

	// Use a non-blocking store that succeeds normally.
	cs := newCampfireScripStore(t, h)

	eng := exchange.NewEngine(exchange.EngineOptions{
		CampfireID:        h.cfID,
		LocalStore:        h.st,
		OperatorPublicKey: h.operator.pubKeyHex,
		ScripStore:        cs,
		Logger: func(format string, args ...any) {
			t.Logf("[engine] "+format, args...)
		},
	})

	// Seed minimal inventory.
	putMsg := h.sendMessage(h.seller,
		putPayload("bg fallback test", fmt.Sprintf("sha256:%064x", 99), "code", 5000, 4096),
		[]string{exchange.TagPut, "exchange:content-type:code"},
		nil,
	)
	msgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))
	if err := eng.AutoAcceptPut(putMsg.ID, 3000, time.Now().Add(72*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut: %v", err)
	}
	// Seed buyer scrip via mint + replay.
	addScripMintMsg(t, h, h.buyer.PublicKeyHex(), 100_000)
	if err := cs.Replay(); err != nil {
		t.Fatalf("Replay: %v", err)
	}

	// Dispatch a buy message directly (without calling Start).
	buyPayloadBytes, _ := json.Marshal(map[string]any{
		"task":        "background fallback test",
		"budget":      int64(100_000),
		"max_results": 3,
	})
	buyMsg := h.sendMessage(h.buyer, buyPayloadBytes,
		[]string{exchange.TagBuy}, nil,
	)

	// DispatchForTest calls the handler synchronously without Start().
	// engineCtx() should return context.Background() since e.ctx is nil.
	// The handler must not panic and should complete successfully.
	if err := eng.DispatchForTest(&exchange.Message{
		ID:         buyMsg.ID,
		CampfireID: h.cfID,
		Sender:     h.buyer.PublicKeyHex(),
		Payload:    buyPayloadBytes,
		Tags:       []string{exchange.TagBuy},
		Timestamp:  time.Now().UnixNano(),
	}); err != nil {
		t.Fatalf("DispatchForTest returned unexpected error: %v", err)
	}
}
