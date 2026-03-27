package scrip_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/campfire-net/campfire/pkg/store"

	"github.com/3dl-dev/dontguess/pkg/scrip"
)

const (
	testCampfireID = "aabbccdd" + "aabbccdd" + "aabbccdd" + "aabbccdd" + // 64 hex chars
		"aabbccdd" + "aabbccdd" + "aabbccdd" + "aabbccdd"
	agentAlice    = "aaaa000000000000000000000000000000000000000000000000000000000001"
	agentBob      = "bbbb000000000000000000000000000000000000000000000000000000000002"
	agentOperator = "eeee000000000000000000000000000000000000000000000000000000000009"
)

// openTestStore opens an in-process SQLite store in a temp directory and
// registers testCampfireID so messages can be inserted without FK errors.
func openTestStore(t *testing.T) store.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	// Register the campfire membership so the FK constraint on messages is satisfied.
	if err := st.AddMembership(store.Membership{
		CampfireID:   testCampfireID,
		JoinProtocol: "open",
		Role:         "full",
		JoinedAt:     time.Now().UnixNano(),
		Threshold:    1,
	}); err != nil {
		t.Fatalf("AddMembership: %v", err)
	}
	return st
}

// addMsg inserts a scrip convention message into the test store.
func addMsg(t *testing.T, st store.Store, campfireID, sender, op string, payload any, tags ...string) {
	t.Helper()
	rawPayload, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	allTags := append([]string{op}, tags...)
	rec := store.MessageRecord{
		ID:         randomID(t),
		CampfireID: campfireID,
		Sender:     sender,
		Payload:    rawPayload,
		Tags:       allTags,
		Timestamp:  time.Now().UnixNano(),
		ReceivedAt: time.Now().UnixNano(),
		Signature:  []byte{0x00}, // non-nil to satisfy schema NOT NULL constraint
	}
	if _, err := st.AddMessage(rec); err != nil {
		t.Fatalf("AddMessage: %v", err)
	}
}

// buildMsg constructs a MessageRecord without inserting it into the store.
// Used to test ApplyMessage (live-mode path) directly.
func buildMsg(t *testing.T, campfireID, sender, op string, payload any, tags ...string) store.MessageRecord {
	t.Helper()
	rawPayload, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	allTags := append([]string{op}, tags...)
	return store.MessageRecord{
		ID:         randomID(t),
		CampfireID: campfireID,
		Sender:     sender,
		Payload:    rawPayload,
		Tags:       allTags,
		Timestamp:  time.Now().UnixNano(),
		ReceivedAt: time.Now().UnixNano(),
		Signature:  []byte{0x00},
	}
}

func randomID(t *testing.T) string {
	t.Helper()
	return time.Now().Format("20060102150405.999999999") + t.Name()
}

// newStore creates a CampfireScripStore seeded with the given campfire messages.
// Messages are added in order before constructing the store so Replay sees them.
// Uses agentOperator as the operator key so only messages from that sender are applied.
func newStore(t *testing.T, st store.Store) *scrip.CampfireScripStore {
	t.Helper()
	cs, err := scrip.NewCampfireScripStore(testCampfireID, st, agentOperator)
	if err != nil {
		t.Fatalf("NewCampfireScripStore: %v", err)
	}
	return cs
}

// --- GetBudget ---

func TestGetBudget_UnknownAgent(t *testing.T) {
	st := openTestStore(t)
	cs := newStore(t, st)
	ctx := context.Background()

	val, etag, err := cs.GetBudget(ctx, agentAlice, scrip.BalanceKey)
	if err != nil {
		t.Fatalf("GetBudget: %v", err)
	}
	if val != 0 {
		t.Errorf("val = %d, want 0", val)
	}
	if etag != "" {
		t.Errorf("etag = %q, want empty", etag)
	}
}

func TestGetBudget_AfterMint(t *testing.T) {
	st := openTestStore(t)

	// Mint 1000 micro-tokens to Alice.
	addMsg(t, st, testCampfireID, agentOperator, "dontguess:scrip-mint", map[string]any{
		"recipient":   agentAlice,
		"amount":      int64(1000),
		"x402_tx_ref": "tx-001",
		"rate":        int64(1000),
	})

	cs := newStore(t, st)
	ctx := context.Background()

	val, etag, err := cs.GetBudget(ctx, agentAlice, scrip.BalanceKey)
	if err != nil {
		t.Fatalf("GetBudget: %v", err)
	}
	if val != 1000 {
		t.Errorf("val = %d, want 1000", val)
	}
	if etag == "" {
		t.Error("etag must be non-empty after mint")
	}
}

// --- AddBudget ---

func TestAddBudget_CreatesEntry(t *testing.T) {
	st := openTestStore(t)
	cs := newStore(t, st)
	ctx := context.Background()

	// No prior balance — create via AddBudget.
	newVal, newETag, err := cs.AddBudget(ctx, agentAlice, scrip.BalanceKey, 5000, "")
	if err != nil {
		t.Fatalf("AddBudget: %v", err)
	}
	if newVal != 5000 {
		t.Errorf("newVal = %d, want 5000", newVal)
	}
	if newETag == "" {
		t.Error("newETag must be non-empty")
	}

	// Verify GetBudget returns same value.
	val, etag, err := cs.GetBudget(ctx, agentAlice, scrip.BalanceKey)
	if err != nil {
		t.Fatalf("GetBudget after AddBudget: %v", err)
	}
	if val != 5000 {
		t.Errorf("val = %d, want 5000", val)
	}
	if etag != newETag {
		t.Errorf("etag mismatch: got %q, want %q", etag, newETag)
	}
}

func TestAddBudget_IncrementExisting(t *testing.T) {
	st := openTestStore(t)
	// Seed 1000 via mint.
	addMsg(t, st, testCampfireID, agentOperator, "dontguess:scrip-mint", map[string]any{
		"recipient":   agentAlice,
		"amount":      int64(1000),
		"x402_tx_ref": "tx-002",
		"rate":        int64(1000),
	})
	cs := newStore(t, st)
	ctx := context.Background()

	_, etag, err := cs.GetBudget(ctx, agentAlice, scrip.BalanceKey)
	if err != nil {
		t.Fatalf("GetBudget: %v", err)
	}

	newVal, _, err := cs.AddBudget(ctx, agentAlice, scrip.BalanceKey, 500, etag)
	if err != nil {
		t.Fatalf("AddBudget: %v", err)
	}
	if newVal != 1500 {
		t.Errorf("newVal = %d, want 1500", newVal)
	}
}

func TestAddBudget_ConflictOnStalEtag(t *testing.T) {
	st := openTestStore(t)
	addMsg(t, st, testCampfireID, agentOperator, "dontguess:scrip-mint", map[string]any{
		"recipient":   agentAlice,
		"amount":      int64(1000),
		"x402_tx_ref": "tx-003",
		"rate":        int64(1000),
	})
	cs := newStore(t, st)
	ctx := context.Background()

	_, etag, _ := cs.GetBudget(ctx, agentAlice, scrip.BalanceKey)
	// First add — succeeds and updates etag.
	_, _, err := cs.AddBudget(ctx, agentAlice, scrip.BalanceKey, 100, etag)
	if err != nil {
		t.Fatalf("first AddBudget: %v", err)
	}
	// Second add with stale etag — should conflict.
	_, _, err = cs.AddBudget(ctx, agentAlice, scrip.BalanceKey, 100, etag)
	if !errors.Is(err, scrip.ErrConflict) {
		t.Errorf("expected ErrConflict, got %v", err)
	}
}

// --- DecrementBudget ---

func TestDecrementBudget_Success(t *testing.T) {
	st := openTestStore(t)
	addMsg(t, st, testCampfireID, agentOperator, "dontguess:scrip-mint", map[string]any{
		"recipient":   agentAlice,
		"amount":      int64(2000),
		"x402_tx_ref": "tx-010",
		"rate":        int64(1000),
	})
	cs := newStore(t, st)
	ctx := context.Background()

	val, etag, _ := cs.GetBudget(ctx, agentAlice, scrip.BalanceKey)
	if val != 2000 {
		t.Fatalf("expected 2000, got %d", val)
	}

	newVal, newETag, err := cs.DecrementBudget(ctx, agentAlice, scrip.BalanceKey, 700, etag)
	if err != nil {
		t.Fatalf("DecrementBudget: %v", err)
	}
	if newVal != 1300 {
		t.Errorf("newVal = %d, want 1300", newVal)
	}
	if newETag == etag {
		t.Error("ETag must change after decrement")
	}
}

func TestDecrementBudget_BudgetExceeded(t *testing.T) {
	st := openTestStore(t)
	addMsg(t, st, testCampfireID, agentOperator, "dontguess:scrip-mint", map[string]any{
		"recipient":   agentAlice,
		"amount":      int64(100),
		"x402_tx_ref": "tx-011",
		"rate":        int64(1000),
	})
	cs := newStore(t, st)
	ctx := context.Background()

	_, etag, _ := cs.GetBudget(ctx, agentAlice, scrip.BalanceKey)

	_, _, err := cs.DecrementBudget(ctx, agentAlice, scrip.BalanceKey, 200, etag)
	if !errors.Is(err, scrip.ErrBudgetExceeded) {
		t.Errorf("expected ErrBudgetExceeded, got %v", err)
	}

	// Balance must be unchanged.
	val, _, _ := cs.GetBudget(ctx, agentAlice, scrip.BalanceKey)
	if val != 100 {
		t.Errorf("balance changed after failed decrement: got %d, want 100", val)
	}
}

func TestDecrementBudget_ConflictOnStaleEtag(t *testing.T) {
	st := openTestStore(t)
	addMsg(t, st, testCampfireID, agentOperator, "dontguess:scrip-mint", map[string]any{
		"recipient":   agentAlice,
		"amount":      int64(1000),
		"x402_tx_ref": "tx-012",
		"rate":        int64(1000),
	})
	cs := newStore(t, st)
	ctx := context.Background()

	_, etag, _ := cs.GetBudget(ctx, agentAlice, scrip.BalanceKey)
	// Consume etag with a successful decrement.
	_, _, _ = cs.DecrementBudget(ctx, agentAlice, scrip.BalanceKey, 100, etag)
	// Now etag is stale.
	_, _, err := cs.DecrementBudget(ctx, agentAlice, scrip.BalanceKey, 100, etag)
	if !errors.Is(err, scrip.ErrConflict) {
		t.Errorf("expected ErrConflict, got %v", err)
	}
}

func TestDecrementBudget_EmptyEtagConflict(t *testing.T) {
	st := openTestStore(t)
	addMsg(t, st, testCampfireID, agentOperator, "dontguess:scrip-mint", map[string]any{
		"recipient":   agentAlice,
		"amount":      int64(1000),
		"x402_tx_ref": "tx-013",
		"rate":        int64(1000),
	})
	cs := newStore(t, st)
	ctx := context.Background()

	// Empty etag must be rejected (create-path semantics: no existing counter allowed).
	_, _, err := cs.DecrementBudget(ctx, agentAlice, scrip.BalanceKey, 100, "")
	if !errors.Is(err, scrip.ErrConflict) {
		t.Errorf("expected ErrConflict for empty etag, got %v", err)
	}
}

func TestDecrementBudget_UnknownAgent(t *testing.T) {
	cs := newStore(t, openTestStore(t))
	ctx := context.Background()

	_, _, err := cs.DecrementBudget(ctx, "unknown-agent", scrip.BalanceKey, 100, "some-etag")
	if !errors.Is(err, scrip.ErrConflict) {
		t.Errorf("expected ErrConflict for unknown agent, got %v", err)
	}
}

// --- Reservation CRUD ---

func TestReservation_SaveGetDelete(t *testing.T) {
	cs := newStore(t, openTestStore(t))
	ctx := context.Background()

	r := scrip.Reservation{
		ID:        "res-001",
		AgentKey:  agentAlice,
		RK:        scrip.BalanceKey,
		ETag:      "3",
		Amount:    500,
		CreatedAt: time.Now().UTC().Truncate(time.Second),
	}

	// Save.
	if err := cs.SaveReservation(ctx, r); err != nil {
		t.Fatalf("SaveReservation: %v", err)
	}

	// Get.
	got, err := cs.GetReservation(ctx, r.ID)
	if err != nil {
		t.Fatalf("GetReservation: %v", err)
	}
	if got.ID != r.ID || got.AgentKey != r.AgentKey || got.Amount != r.Amount {
		t.Errorf("GetReservation mismatch: got %+v, want %+v", got, r)
	}

	// Delete.
	if err := cs.DeleteReservation(ctx, r.ID); err != nil {
		t.Fatalf("DeleteReservation: %v", err)
	}

	// Confirm gone.
	_, err = cs.GetReservation(ctx, r.ID)
	if !errors.Is(err, scrip.ErrReservationNotFound) {
		t.Errorf("expected ErrReservationNotFound, got %v", err)
	}
}

func TestReservation_GetNotFound(t *testing.T) {
	cs := newStore(t, openTestStore(t))
	ctx := context.Background()

	_, err := cs.GetReservation(ctx, "nonexistent")
	if !errors.Is(err, scrip.ErrReservationNotFound) {
		t.Errorf("expected ErrReservationNotFound, got %v", err)
	}
}

func TestReservation_DeleteNotFound(t *testing.T) {
	cs := newStore(t, openTestStore(t))
	ctx := context.Background()

	err := cs.DeleteReservation(ctx, "nonexistent")
	if !errors.Is(err, scrip.ErrReservationNotFound) {
		t.Errorf("expected ErrReservationNotFound, got %v", err)
	}
}

func TestReservation_SaveOverwrite(t *testing.T) {
	cs := newStore(t, openTestStore(t))
	ctx := context.Background()

	r1 := scrip.Reservation{ID: "res-over", AgentKey: agentAlice, Amount: 100}
	r2 := scrip.Reservation{ID: "res-over", AgentKey: agentBob, Amount: 200}

	_ = cs.SaveReservation(ctx, r1)
	_ = cs.SaveReservation(ctx, r2)

	got, _ := cs.GetReservation(ctx, "res-over")
	if got.AgentKey != agentBob || got.Amount != 200 {
		t.Errorf("overwrite failed: got %+v", got)
	}
}

// --- ConsumeReservation ---

func TestConsumeReservation_ReturnsAndDeletes(t *testing.T) {
	cs := newStore(t, openTestStore(t))
	ctx := context.Background()

	r := scrip.Reservation{
		ID:       "res-consume-001",
		AgentKey: agentAlice,
		Amount:   500,
	}
	if err := cs.SaveReservation(ctx, r); err != nil {
		t.Fatalf("SaveReservation: %v", err)
	}

	got, err := cs.ConsumeReservation(ctx, r.ID)
	if err != nil {
		t.Fatalf("ConsumeReservation: %v", err)
	}
	if got != r {
		t.Errorf("ConsumeReservation returned %+v, want %+v", got, r)
	}

	// Reservation must no longer exist after consume.
	_, err = cs.GetReservation(ctx, r.ID)
	if !errors.Is(err, scrip.ErrReservationNotFound) {
		t.Errorf("expected ErrReservationNotFound after consume, got %v", err)
	}
}

func TestConsumeReservation_SecondCallReturnsNotFound(t *testing.T) {
	cs := newStore(t, openTestStore(t))
	ctx := context.Background()

	r := scrip.Reservation{ID: "res-consume-002", AgentKey: agentBob, Amount: 200}
	if err := cs.SaveReservation(ctx, r); err != nil {
		t.Fatalf("SaveReservation: %v", err)
	}

	// First consume succeeds.
	if _, err := cs.ConsumeReservation(ctx, r.ID); err != nil {
		t.Fatalf("first ConsumeReservation: %v", err)
	}

	// Second consume on the same ID must return ErrReservationNotFound.
	_, err := cs.ConsumeReservation(ctx, r.ID)
	if !errors.Is(err, scrip.ErrReservationNotFound) {
		t.Errorf("expected ErrReservationNotFound on second consume, got %v", err)
	}
}

func TestConsumeReservation_NotFound(t *testing.T) {
	cs := newStore(t, openTestStore(t))
	ctx := context.Background()

	_, err := cs.ConsumeReservation(ctx, "nonexistent")
	if !errors.Is(err, scrip.ErrReservationNotFound) {
		t.Errorf("expected ErrReservationNotFound, got %v", err)
	}
}

// --- Balance derivation from message log replay ---

func TestReplay_MintAndBalance(t *testing.T) {
	st := openTestStore(t)

	// Mint 3000 to Alice.
	addMsg(t, st, testCampfireID, agentOperator, "dontguess:scrip-mint", map[string]any{
		"recipient":   agentAlice,
		"amount":      int64(3000),
		"x402_tx_ref": "tx-r01",
		"rate":        int64(1000),
	})
	// Mint 1000 to Bob.
	addMsg(t, st, testCampfireID, agentOperator, "dontguess:scrip-mint", map[string]any{
		"recipient":   agentBob,
		"amount":      int64(1000),
		"x402_tx_ref": "tx-r02",
		"rate":        int64(1000),
	})

	cs := newStore(t, st)

	if cs.Balance(agentAlice) != 3000 {
		t.Errorf("Alice balance = %d, want 3000", cs.Balance(agentAlice))
	}
	if cs.Balance(agentBob) != 1000 {
		t.Errorf("Bob balance = %d, want 1000", cs.Balance(agentBob))
	}
	if cs.TotalSupply() != 4000 {
		t.Errorf("TotalSupply = %d, want 4000", cs.TotalSupply())
	}
}

func TestReplay_PutPay(t *testing.T) {
	st := openTestStore(t)

	// Seed operator balance via mint to self.
	addMsg(t, st, testCampfireID, agentOperator, "dontguess:scrip-mint", map[string]any{
		"recipient":   agentOperator,
		"amount":      int64(10000),
		"x402_tx_ref": "tx-op",
		"rate":        int64(1000),
	})
	// Operator pays Alice for a put.
	addMsg(t, st, testCampfireID, agentOperator, "dontguess:scrip-put-pay", map[string]any{
		"seller":       agentAlice,
		"amount":       int64(700),
		"token_cost":   int64(1000),
		"discount_pct": 70,
		"result_hash":  "abc123",
		"put_msg":      "put-msg-001",
	})

	cs := newStore(t, st)

	if cs.Balance(agentAlice) != 700 {
		t.Errorf("Alice = %d, want 700", cs.Balance(agentAlice))
	}
	if cs.Balance(agentOperator) != 9300 {
		t.Errorf("Operator = %d, want 9300", cs.Balance(agentOperator))
	}
}

func TestReplay_BuyHoldAndDisputeRefund(t *testing.T) {
	st := openTestStore(t)

	// Alice minted 2000.
	addMsg(t, st, testCampfireID, agentOperator, "dontguess:scrip-mint", map[string]any{
		"recipient":   agentAlice,
		"amount":      int64(2000),
		"x402_tx_ref": "tx-r10",
		"rate":        int64(1000),
	})
	// Alice buy-hold for 500 (price=450 + fee=50).
	addMsg(t, st, testCampfireID, agentOperator, "dontguess:scrip-buy-hold", map[string]any{
		"buyer":          agentAlice,
		"amount":         int64(500),
		"price":          int64(450),
		"fee":            int64(50),
		"reservation_id": "res-bh-001",
		"buy_msg":        "buy-msg-001",
		"expires_at":     time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339),
	})

	cs := newStore(t, st)

	if cs.Balance(agentAlice) != 1500 {
		t.Errorf("Alice after hold = %d, want 1500", cs.Balance(agentAlice))
	}

	// Dispute refund: full 500 back to Alice.
	addMsg(t, st, testCampfireID, agentOperator, "dontguess:scrip-dispute-refund", map[string]any{
		"buyer":          agentAlice,
		"amount":         int64(500),
		"reservation_id": "res-bh-001",
		"dispute_msg":    "dispute-msg-001",
	})

	if err := cs.Replay(); err != nil {
		t.Fatalf("Replay: %v", err)
	}

	if cs.Balance(agentAlice) != 2000 {
		t.Errorf("Alice after refund = %d, want 2000", cs.Balance(agentAlice))
	}
}

func TestReplay_SettleResidualAndBurn(t *testing.T) {
	st := openTestStore(t)

	// Seed: operator=5000, Alice=2000 (seller), Bob=1000 (buyer).
	addMsg(t, st, testCampfireID, agentOperator, "dontguess:scrip-mint", map[string]any{
		"recipient": agentOperator, "amount": int64(5000), "x402_tx_ref": "op-tx", "rate": int64(1000),
	})
	addMsg(t, st, testCampfireID, agentOperator, "dontguess:scrip-mint", map[string]any{
		"recipient": agentAlice, "amount": int64(2000), "x402_tx_ref": "alice-tx", "rate": int64(1000),
	})
	addMsg(t, st, testCampfireID, agentOperator, "dontguess:scrip-mint", map[string]any{
		"recipient": agentBob, "amount": int64(1000), "x402_tx_ref": "bob-tx", "rate": int64(1000),
	})

	// Bob buys (hold 500 = price 450 + fee 50).
	addMsg(t, st, testCampfireID, agentOperator, "dontguess:scrip-buy-hold", map[string]any{
		"buyer": agentBob, "amount": int64(500), "price": int64(450), "fee": int64(50),
		"reservation_id": "res-settle-001", "buy_msg": "buy-002",
		"expires_at": time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339),
	})

	// Settle: residual=90 to Alice (seller), fee_burned=50, exchange_revenue=360 to operator.
	addMsg(t, st, testCampfireID, agentOperator, "dontguess:scrip-settle", map[string]any{
		"reservation_id":   "res-settle-001",
		"seller":           agentAlice,
		"residual":         int64(90),
		"fee_burned":       int64(50),
		"exchange_revenue": int64(360),
		"match_msg":        "match-001",
		"result_hash":      "deadbeef",
	})
	// The engine also emits a scrip-burn message for the matching fee.
	// This is the sole source of totalBurned — applySettle does NOT double-count it.
	addMsg(t, st, testCampfireID, agentOperator, "dontguess:scrip-burn", map[string]any{
		"amount": int64(50),
		"reason": "matching-fee",
	})

	cs := newStore(t, st)

	// Bob: 1000 - 500 = 500
	if cs.Balance(agentBob) != 500 {
		t.Errorf("Bob = %d, want 500", cs.Balance(agentBob))
	}
	// Alice: 2000 + 90 = 2090
	if cs.Balance(agentAlice) != 2090 {
		t.Errorf("Alice = %d, want 2090", cs.Balance(agentAlice))
	}
	// Operator: 5000 + 360 = 5360
	if cs.Balance(agentOperator) != 5360 {
		t.Errorf("Operator = %d, want 5360", cs.Balance(agentOperator))
	}
	// TotalBurned must equal the fee exactly once — not 2*fee.
	// The scrip-burn message is the sole source; applySettle does not increment totalBurned.
	if cs.TotalBurned() != 50 {
		t.Errorf("TotalBurned = %d, want 50 (must not double-count)", cs.TotalBurned())
	}
}

func TestReplay_AssignPay(t *testing.T) {
	st := openTestStore(t)

	// Seed operator.
	addMsg(t, st, testCampfireID, agentOperator, "dontguess:scrip-mint", map[string]any{
		"recipient": agentOperator, "amount": int64(5000), "x402_tx_ref": "op2", "rate": int64(1000),
	})
	// Pay Bob for labor.
	addMsg(t, st, testCampfireID, agentOperator, "dontguess:scrip-assign-pay", map[string]any{
		"worker":      agentBob,
		"amount":      int64(300),
		"task_type":   "validate",
		"assign_msg":  "assign-001",
		"result_hash": "",
	})

	cs := newStore(t, st)

	if cs.Balance(agentBob) != 300 {
		t.Errorf("Bob = %d, want 300", cs.Balance(agentBob))
	}
	if cs.Balance(agentOperator) != 4700 {
		t.Errorf("Operator = %d, want 4700", cs.Balance(agentOperator))
	}
}

func TestReplay_BurnOnly(t *testing.T) {
	st := openTestStore(t)

	// Burn message (no balance change — just total burned).
	addMsg(t, st, testCampfireID, agentOperator, "dontguess:scrip-burn", map[string]any{
		"amount": int64(200),
		"reason": "matching-fee",
	})

	cs := newStore(t, st)

	if cs.TotalBurned() != 200 {
		t.Errorf("TotalBurned = %d, want 200", cs.TotalBurned())
	}
}

// TestReplay_SettleBurnNoDoubleCount is the regression test for the bug where
// applySettle incremented totalBurned AND a separate scrip-burn message also
// incremented totalBurned, resulting in 2*fee after Replay.
//
// The fix: applySettle does NOT touch totalBurned. The scrip-burn message is the
// sole source of totalBurned accounting.
func TestReplay_SettleBurnNoDoubleCount(t *testing.T) {
	st := openTestStore(t)
	const fee = int64(100)

	// Bob buys; hold removes scrip from buyer.
	addMsg(t, st, testCampfireID, agentOperator, "dontguess:scrip-buy-hold", map[string]any{
		"buyer": agentBob, "amount": fee * 5, "price": fee * 4, "fee": fee,
		"reservation_id": "res-double-001", "buy_msg": "buy-001",
		"expires_at": time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339),
	})

	// Engine emits scrip-settle (contains fee_burned in payload).
	addMsg(t, st, testCampfireID, agentOperator, "dontguess:scrip-settle", map[string]any{
		"reservation_id":   "res-double-001",
		"seller":           agentAlice,
		"residual":         int64(40),
		"fee_burned":       fee,
		"exchange_revenue": int64(360),
		"match_msg":        "match-001",
		"result_hash":      "deadbeef",
	})

	// Engine also emits scrip-burn for the same matching fee.
	addMsg(t, st, testCampfireID, agentOperator, "dontguess:scrip-burn", map[string]any{
		"amount": fee,
		"reason": "matching-fee",
	})

	cs := newStore(t, st)

	// totalBurned must equal fee exactly once, not 2*fee.
	if got := cs.TotalBurned(); got != fee {
		t.Errorf("TotalBurned = %d, want %d (double-count bug: got 2*fee)", got, fee)
	}
}

func TestReplay_IdempotentReplay(t *testing.T) {
	st := openTestStore(t)

	addMsg(t, st, testCampfireID, agentOperator, "dontguess:scrip-mint", map[string]any{
		"recipient": agentAlice, "amount": int64(1000), "x402_tx_ref": "tx-idem", "rate": int64(1000),
	})

	cs := newStore(t, st)

	if cs.Balance(agentAlice) != 1000 {
		t.Fatalf("initial balance = %d, want 1000", cs.Balance(agentAlice))
	}

	// Replay again — balance must not double.
	if err := cs.Replay(); err != nil {
		t.Fatalf("second Replay: %v", err)
	}
	if cs.Balance(agentAlice) != 1000 {
		t.Errorf("after second Replay: balance = %d, want 1000 (replay doubled)", cs.Balance(agentAlice))
	}
}

// --- Full round-trip: GetBudget → DecrementBudget → AddBudget (refund) ---

func TestFullRoundTrip_PreDecrementAdjustRefund(t *testing.T) {
	st := openTestStore(t)
	// Seed Alice with 5000.
	addMsg(t, st, testCampfireID, agentOperator, "dontguess:scrip-mint", map[string]any{
		"recipient": agentAlice, "amount": int64(5000), "x402_tx_ref": "tx-rt", "rate": int64(1000),
	})

	cs := newStore(t, st)
	ctx := context.Background()

	// Step 1: GetBudget → read current balance + etag.
	bal, etag, err := cs.GetBudget(ctx, agentAlice, scrip.BalanceKey)
	if err != nil || bal != 5000 {
		t.Fatalf("GetBudget: val=%d err=%v", bal, err)
	}

	// Step 2: DecrementBudget — pre-decrement estimated cost of 1000.
	newBal, newETag, err := cs.DecrementBudget(ctx, agentAlice, scrip.BalanceKey, 1000, etag)
	if err != nil {
		t.Fatalf("DecrementBudget: %v", err)
	}
	if newBal != 4000 {
		t.Errorf("after decrement: %d, want 4000", newBal)
	}

	// Step 3: Save reservation.
	r := scrip.Reservation{
		ID:        "rt-res-001",
		AgentKey:  agentAlice,
		RK:        scrip.BalanceKey,
		ETag:      newETag,
		Amount:    1000,
		CreatedAt: time.Now().UTC(),
	}
	if err := cs.SaveReservation(ctx, r); err != nil {
		t.Fatalf("SaveReservation: %v", err)
	}

	// Step 4: Retrieve reservation.
	got, err := cs.GetReservation(ctx, r.ID)
	if err != nil || got.Amount != 1000 {
		t.Fatalf("GetReservation: %v, got %+v", err, got)
	}

	// Step 5: Actual cost was 800 — refund the 200 overestimate via AddBudget.
	curBal, curETag, _ := cs.GetBudget(ctx, agentAlice, scrip.BalanceKey)
	if curBal != 4000 {
		t.Fatalf("pre-refund balance = %d, want 4000", curBal)
	}
	refundedBal, _, err := cs.AddBudget(ctx, agentAlice, scrip.BalanceKey, 200, curETag)
	if err != nil {
		t.Fatalf("AddBudget (refund): %v", err)
	}
	if refundedBal != 4200 {
		t.Errorf("after refund: %d, want 4200", refundedBal)
	}

	// Step 6: Delete reservation.
	if err := cs.DeleteReservation(ctx, r.ID); err != nil {
		t.Fatalf("DeleteReservation: %v", err)
	}

	// Verify final balance.
	finalBal, _, _ := cs.GetBudget(ctx, agentAlice, scrip.BalanceKey)
	if finalBal != 4200 {
		t.Errorf("final balance = %d, want 4200", finalBal)
	}
}

// --- Operator identity gate ---

// TestOperatorGate_ForgedMintRejected verifies that a scrip-mint message from a
// non-operator sender does not affect any balance when OperatorKey is configured.
func TestOperatorGate_ForgedMintRejected(t *testing.T) {
	st := openTestStore(t)

	// Forged mint: attacker (agentBob) tries to mint 9999 to themselves.
	addMsg(t, st, testCampfireID, agentBob, "dontguess:scrip-mint", map[string]any{
		"recipient":   agentBob,
		"amount":      int64(9999),
		"x402_tx_ref": "forged-tx-001",
		"rate":        int64(1000),
	})

	// Construct store with operator key set — forged message must be ignored.
	cs, err := scrip.NewCampfireScripStore(testCampfireID, st, agentOperator)
	if err != nil {
		t.Fatalf("NewCampfireScripStore: %v", err)
	}

	if bal := cs.Balance(agentBob); bal != 0 {
		t.Errorf("forged mint accepted: Bob balance = %d, want 0", bal)
	}
	if cs.TotalSupply() != 0 {
		t.Errorf("forged mint affected TotalSupply: got %d, want 0", cs.TotalSupply())
	}
}

// TestOperatorGate_LegitimateOperatorMintAccepted verifies that a scrip-mint from
// the configured operator key is processed normally.
func TestOperatorGate_LegitimateOperatorMintAccepted(t *testing.T) {
	st := openTestStore(t)

	// Legitimate mint from the operator.
	addMsg(t, st, testCampfireID, agentOperator, "dontguess:scrip-mint", map[string]any{
		"recipient":   agentAlice,
		"amount":      int64(5000),
		"x402_tx_ref": "legit-tx-001",
		"rate":        int64(1000),
	})

	cs, err := scrip.NewCampfireScripStore(testCampfireID, st, agentOperator)
	if err != nil {
		t.Fatalf("NewCampfireScripStore: %v", err)
	}

	if bal := cs.Balance(agentAlice); bal != 5000 {
		t.Errorf("operator mint not applied: Alice balance = %d, want 5000", bal)
	}
	if cs.TotalSupply() != 5000 {
		t.Errorf("TotalSupply = %d, want 5000", cs.TotalSupply())
	}
}

// TestOperatorGate_MixedMints verifies that forged mints are rejected while
// legitimate operator mints are accepted, even when interleaved in the log.
func TestOperatorGate_MixedMints(t *testing.T) {
	st := openTestStore(t)

	// Attacker mints to themselves (should be rejected).
	addMsg(t, st, testCampfireID, agentBob, "dontguess:scrip-mint", map[string]any{
		"recipient": agentBob, "amount": int64(9999), "x402_tx_ref": "forged-01", "rate": int64(1000),
	})
	// Operator mints to Alice (should be accepted).
	addMsg(t, st, testCampfireID, agentOperator, "dontguess:scrip-mint", map[string]any{
		"recipient": agentAlice, "amount": int64(1000), "x402_tx_ref": "legit-01", "rate": int64(1000),
	})
	// Attacker mints to Alice (should be rejected).
	addMsg(t, st, testCampfireID, agentBob, "dontguess:scrip-mint", map[string]any{
		"recipient": agentAlice, "amount": int64(8888), "x402_tx_ref": "forged-02", "rate": int64(1000),
	})

	cs, err := scrip.NewCampfireScripStore(testCampfireID, st, agentOperator)
	if err != nil {
		t.Fatalf("NewCampfireScripStore: %v", err)
	}

	if bal := cs.Balance(agentBob); bal != 0 {
		t.Errorf("forged mint applied to Bob: balance = %d, want 0", bal)
	}
	if bal := cs.Balance(agentAlice); bal != 1000 {
		t.Errorf("Alice balance = %d, want 1000 (only operator mint)", bal)
	}
	if cs.TotalSupply() != 1000 {
		t.Errorf("TotalSupply = %d, want 1000 (only operator mint counted)", cs.TotalSupply())
	}
}

// TestOperatorGate_EmptyKeyDisablesCheck verifies backwards compatibility:
// when OperatorKey is empty, messages from any sender are accepted.
func TestOperatorGate_EmptyKeyDisablesCheck(t *testing.T) {
	st := openTestStore(t)

	// Mint from a non-operator sender — should be accepted when OperatorKey is "".
	addMsg(t, st, testCampfireID, agentBob, "dontguess:scrip-mint", map[string]any{
		"recipient": agentAlice, "amount": int64(500), "x402_tx_ref": "any-sender", "rate": int64(1000),
	})

	cs, err := scrip.NewCampfireScripStore(testCampfireID, st, "")
	if err != nil {
		t.Fatalf("NewCampfireScripStore: %v", err)
	}

	if bal := cs.Balance(agentAlice); bal != 500 {
		t.Errorf("empty OperatorKey should accept any sender: Alice balance = %d, want 500", bal)
	}
}

// --- Underflow guard: subtractFromBalance in live vs replay mode ---

// TestReplay_NegativeBalanceAllowed verifies that during replay the balance is
// allowed to go negative when the log contains a subtract without a prior mint.
// This preserves replay fidelity: the campfire log is the authority.
func TestReplay_NegativeBalanceAllowed(t *testing.T) {
	st := openTestStore(t)

	// buy-hold without a prior mint — replay must not reject it.
	addMsg(t, st, testCampfireID, agentOperator, "dontguess:scrip-buy-hold", map[string]any{
		"buyer":  agentAlice,
		"amount": int64(500),
	})

	cs := newStore(t, st)

	// Balance must be -500 (negative allowed in replay mode).
	if bal := cs.Balance(agentAlice); bal != -500 {
		t.Errorf("replay: expected negative balance -500, got %d", bal)
	}
}

// TestReplay_NegativeBalance_SubsequentDecrementBudgetFails verifies the lockout
// scenario: if replay produced a negative balance, DecrementBudget must return
// ErrBudgetExceeded — not a panic or silent success.
func TestReplay_NegativeBalance_SubsequentDecrementBudgetFails(t *testing.T) {
	st := openTestStore(t)

	// buy-hold without a prior mint → negative balance after replay.
	addMsg(t, st, testCampfireID, agentOperator, "dontguess:scrip-buy-hold", map[string]any{
		"buyer":  agentAlice,
		"amount": int64(500),
	})

	cs := newStore(t, st)
	ctx := context.Background()

	// GetBudget returns the (negative) balance and a valid ETag.
	bal, etag, err := cs.GetBudget(ctx, agentAlice, scrip.BalanceKey)
	if err != nil {
		t.Fatalf("GetBudget: %v", err)
	}
	if bal >= 0 {
		t.Fatalf("precondition: expected negative balance, got %d", bal)
	}

	// DecrementBudget must reject even a small amount — balance is already negative.
	_, _, err = cs.DecrementBudget(ctx, agentAlice, scrip.BalanceKey, 1, etag)
	if !errors.Is(err, scrip.ErrBudgetExceeded) {
		t.Errorf("expected ErrBudgetExceeded for negative-balance agent, got %v", err)
	}
}

// TestLiveMode_SubtractFromBalance_ClampsToZero verifies the underflow guard:
// a buy-hold message received in live mode (post-Replay, replaying=false) must
// clamp the balance to zero rather than going negative. This prevents a corrupt
// or unexpected live message from causing permanent buyer lockout.
//
// We test this via ApplyMessage, which processes a single message in live mode
// (replaying=false). Contrast with TestReplay_NegativeBalanceAllowed which shows
// that replay mode permits negative balances.
func TestLiveMode_SubtractFromBalance_ClampsToZero(t *testing.T) {
	st := openTestStore(t)

	// Mint 200 to Alice so she has a positive balance post-replay.
	addMsg(t, st, testCampfireID, agentOperator, "dontguess:scrip-mint", map[string]any{
		"recipient":   agentAlice,
		"amount":      int64(200),
		"x402_tx_ref": "tx-live-clamp",
		"rate":        int64(1000),
	})

	cs := newStore(t, st)
	if cs.Balance(agentAlice) != 200 {
		t.Fatalf("precondition: Alice balance = %d, want 200", cs.Balance(agentAlice))
	}

	// Construct a buy-hold message that would drive Alice 300 below zero (500 > 200).
	// Apply it via ApplyMessage — this runs in live mode (replaying=false).
	liveMsg := buildMsg(t, testCampfireID, agentOperator, "dontguess:scrip-buy-hold", map[string]any{
		"buyer":  agentAlice,
		"amount": int64(500),
	})
	cs.ApplyMessage(&liveMsg)

	bal := cs.Balance(agentAlice)
	if bal < 0 {
		t.Errorf("live mode: balance went negative (%d); underflow guard not applied", bal)
	}
	if bal != 0 {
		t.Errorf("live mode: expected balance clamped to 0, got %d", bal)
	}
}
