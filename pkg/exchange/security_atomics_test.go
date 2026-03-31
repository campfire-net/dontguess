package exchange_test

// TestBuyMiss_ClaimAtomicity and TestAssignAccept_NoBountyOnReplay verify
// fixes for two P1 security bugs:
//
//  1. TOCTOU in buy-miss offer claim (dontguess-88d): two concurrent puts with
//     the same task hash could both observe the offer and both get auto-accepted.
//     Fix: ClaimBuyMissOffer atomically gets and deletes in one mutex.Lock.
//
//  2. Double-pay on replayed assign-accept (dontguess-8ef): a replayed accept
//     message could re-pay the bounty because assignByID retains the record.
//     Fix: ClaimAssignPayment atomically transitions AssignAccepted → AssignPaid,
//     returning nil on any subsequent claim attempt.

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/3dl-dev/dontguess/pkg/exchange"
	"github.com/3dl-dev/dontguess/pkg/scrip"
)

// --- TestBuyMiss_ClaimAtomicity ---

// TestBuyMiss_ClaimAtomicity verifies that two concurrent goroutines calling
// ClaimBuyMissOffer for the same task hash each get the offer at most once.
// Before the fix, two concurrent GetBuyMissOffer + DeleteBuyMissOffer calls
// could both observe the offer before either deleted it, resulting in two
// auto-accepts (seller paid twice).
func TestBuyMiss_ClaimAtomicity(t *testing.T) {
	t.Parallel()

	const taskDesc = "Translate a 500-word English paragraph to French and back"
	taskHash := exchange.TaskDescriptionHash(taskDesc)

	h := newTestHarness(t)
	eng := h.newEngine()

	// Inject a standing buy-miss offer directly into state.
	offer := &exchange.BuyMissOffer{
		TaskHash:     taskHash,
		BuyMsgID:     "buy-test-001",
		BuyerKey:     h.buyer.PublicKeyHex(),
		OfferedPrice: 7000,
		ExpiresAt:    time.Now().Add(exchange.BuyMissOfferTTL),
	}
	eng.State().SetBuyMissOffer(offer)

	// Verify the offer is there before the race.
	if got := eng.State().GetBuyMissOffer(taskHash); got == nil {
		t.Fatal("precondition: offer not set")
	}

	// Concurrently claim the offer from N goroutines. Only one should win.
	const workers = 20
	var wg sync.WaitGroup
	var claimCount atomic.Int64

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if eng.State().ClaimBuyMissOffer(taskHash) != nil {
				claimCount.Add(1)
			}
		}()
	}
	wg.Wait()

	claimed := claimCount.Load()
	if claimed != 1 {
		t.Errorf("ClaimBuyMissOffer: %d goroutines claimed the offer, want exactly 1", claimed)
	}

	// Offer must be gone from state after the single claim.
	if got := eng.State().GetBuyMissOffer(taskHash); got != nil {
		t.Error("offer still present in state after ClaimBuyMissOffer")
	}
}

// --- TestAssignAccept_NoBountyOnReplay ---

// countingScripStore is a minimal SpendingStore that counts AddBudget calls
// for a specific agent key. Only AddBudget is exercised by the assign-accept path.
type countingScripStore struct {
	mu         sync.Mutex
	addCalls   map[string]int   // pk → call count
	addAmounts map[string]int64 // pk → sum of amounts added
}

func newCountingScripStore() *countingScripStore {
	return &countingScripStore{
		addCalls:   make(map[string]int),
		addAmounts: make(map[string]int64),
	}
}

func (s *countingScripStore) AddBudget(_ context.Context, pk, _ string, amount int64, _ string) (int64, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.addCalls[pk]++
	s.addAmounts[pk] += amount
	return s.addAmounts[pk], "etag", nil
}

func (s *countingScripStore) DecrementBudget(_ context.Context, _, _ string, _ int64, _ string) (int64, string, error) {
	return 0, "", nil
}

func (s *countingScripStore) GetBudget(_ context.Context, _, _ string) (int64, string, error) {
	return 0, "", nil
}

func (s *countingScripStore) SaveReservation(_ context.Context, _ scrip.Reservation) error {
	return nil
}

func (s *countingScripStore) GetReservation(_ context.Context, _ string) (scrip.Reservation, error) {
	return scrip.Reservation{}, scrip.ErrReservationNotFound
}

func (s *countingScripStore) DeleteReservation(_ context.Context, _ string) error {
	return nil
}

func (s *countingScripStore) ConsumeReservation(_ context.Context, _ string) (scrip.Reservation, error) {
	return scrip.Reservation{}, scrip.ErrReservationNotFound
}

// callsFor returns the number of AddBudget calls for the given agent key.
func (s *countingScripStore) callsFor(pk string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addCalls[pk]
}

// TestAssignAccept_NoBountyOnReplay verifies that replaying an exchange:assign-accept
// message does not cause the engine to call AddBudget a second time for the worker.
//
// Before the fix, handleAssignAccept scanned assignByID (which retains the record
// at AssignAccepted) on every replay, unconditionally calling AddBudget each time.
// After the fix, ClaimAssignPayment gates payment to the single
// AssignAccepted → AssignPaid transition.
func TestAssignAccept_NoBountyOnReplay(t *testing.T) {
	t.Parallel()

	cs := newCountingScripStore()

	h := newTestHarness(t)
	eng := h.newEngineWithOpts(func(o *exchange.EngineOptions) {
		o.ScripStore = cs
	})

	worker := newTestAgent(t)
	const reward int64 = 500

	// Build the assign lifecycle via the test harness store so state.Replay
	// can materialize it.

	// 1. Assign
	assignMsg := h.sendMessage(h.operator,
		secAssignPayload("entry-xyz", "validation", reward),
		[]string{exchange.TagAssign},
		nil,
	)

	// 2. Claim
	claimMsg := h.sendMessage(worker,
		[]byte(`{}`),
		[]string{exchange.TagAssignClaim},
		[]string{assignMsg.ID},
	)

	// 3. Complete
	completeMsg := h.sendMessage(worker,
		[]byte(`{"output":"validation passed"}`),
		[]string{exchange.TagAssignComplete},
		[]string{claimMsg.ID},
	)

	// Replay state to materialize the lifecycle up to AssignCompleted.
	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	// 4. First accept — write to store, replay state (transitions to AssignAccepted),
	//    then dispatch the handler (should pay bounty once).
	acceptMsg := h.sendMessage(h.operator,
		[]byte(`{}`),
		[]string{exchange.TagAssignAccept},
		[]string{completeMsg.ID},
	)

	// Apply the accept to state (transitions AssignCompleted → AssignAccepted).
	eng.State().Apply(acceptMsg)

	// Dispatch the handler — should pay bounty once.
	if err := eng.DispatchForTest(acceptMsg); err != nil {
		t.Fatalf("DispatchForTest (first accept): %v", err)
	}

	firstCallCount := cs.callsFor(worker.PublicKeyHex())
	if firstCallCount == 0 {
		t.Fatal("expected AddBudget to be called on first accept, got 0 calls")
	}

	// 5. Replay the same accept message — must NOT trigger another AddBudget.
	// State.Apply is a no-op (record no longer in pendingAssignResults).
	eng.State().Apply(acceptMsg)
	if err := eng.DispatchForTest(acceptMsg); err != nil {
		t.Fatalf("DispatchForTest (replay accept): %v", err)
	}

	replayCallCount := cs.callsFor(worker.PublicKeyHex())
	if replayCallCount != firstCallCount {
		t.Errorf("AddBudget called %d times total (first=%d), want no additional calls on replay",
			replayCallCount, firstCallCount)
	}
}

// secAssignPayload builds a minimal valid exchange:assign payload for security tests.
func secAssignPayload(entryID, taskType string, reward int64) []byte {
	p, _ := json.Marshal(map[string]any{
		"entry_id":  entryID,
		"task_type": taskType,
		"reward":    reward,
	})
	return p
}
