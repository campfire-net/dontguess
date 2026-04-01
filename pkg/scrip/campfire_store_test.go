package scrip_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/campfire-net/campfire/pkg/campfire"
	cfencoding "github.com/campfire-net/campfire/pkg/encoding"
	"github.com/campfire-net/campfire/pkg/identity"
	"github.com/campfire-net/campfire/pkg/protocol"
	"github.com/campfire-net/campfire/pkg/store"
	"github.com/campfire-net/campfire/pkg/transport/fs"

	"github.com/3dl-dev/dontguess/pkg/proto"
	"github.com/3dl-dev/dontguess/pkg/scrip"
)

const (
	// agentAlice and agentBob are used only as payload recipients (seller, buyer, worker, recipient).
	// They are not message senders, so they do not require real Ed25519 identities.
	agentAlice = "aaaa000000000000000000000000000000000000000000000000000000000001"
	agentBob   = "bbbb000000000000000000000000000000000000000000000000000000000002"
)

// testEnv holds a minimal filesystem campfire environment for a single test.
// It provides two clients — opClient (operator) and atkClient (attacker) — so
// tests can send messages from different identities to exercise the operator gate.
type testEnv struct {
	campfireID  string          // hex campfire public key
	operatorKey string          // hex pubkey of the operator identity
	attackerKey string          // hex pubkey of the attacker identity
	opClient    *protocol.Client // sends as operator
	atkClient   *protocol.Client // sends as attacker (for forged-message tests)
	st          store.Store     // shared SQLite store (operator's)
}

// newTestEnv creates a temp filesystem campfire with two member identities
// (operator and attacker). Both are registered as "full" members so Client.Send works.
func newTestEnv(t *testing.T) *testEnv {
	t.Helper()

	transportDir := t.TempDir()
	storeDir := t.TempDir()

	// Generate two distinct identities.
	opID, err := identity.Generate()
	if err != nil {
		t.Fatalf("generate operator identity: %v", err)
	}
	atkID, err := identity.Generate()
	if err != nil {
		t.Fatalf("generate attacker identity: %v", err)
	}

	// Open a shared store (both clients will share it — fine for tests since
	// they run sequentially and Client is not goroutine-safe).
	st, err := store.Open(filepath.Join(storeDir, "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	// Create a campfire identity for the campfire itself.
	cfID, err := identity.Generate()
	if err != nil {
		t.Fatalf("generate campfire identity: %v", err)
	}
	campfireID := cfID.PublicKeyHex()

	// Set up the campfire directory structure.
	cfDir := filepath.Join(transportDir, campfireID)
	for _, sub := range []string{"members", "messages"} {
		if err := os.MkdirAll(filepath.Join(cfDir, sub), 0755); err != nil {
			t.Fatalf("mkdir %s: %v", sub, err)
		}
	}

	// Write campfire state file.
	state := &campfire.CampfireState{
		PublicKey:             cfID.PublicKey,
		PrivateKey:            cfID.PrivateKey,
		JoinProtocol:          "open",
		ReceptionRequirements: []string{},
		CreatedAt:             time.Now().UnixNano(),
	}
	stateData, err := cfencoding.Marshal(state)
	if err != nil {
		t.Fatalf("marshal campfire state: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cfDir, "campfire.cbor"), stateData, 0644); err != nil {
		t.Fatalf("write campfire state: %v", err)
	}

	tr := fs.New(transportDir)

	// Register both identities as full members on disk and in the store.
	for _, id := range []*identity.Identity{opID, atkID} {
		if err := tr.WriteMember(campfireID, campfire.MemberRecord{
			PublicKey: id.PublicKey,
			JoinedAt:  time.Now().UnixNano(),
			Role:      campfire.RoleFull,
		}); err != nil {
			t.Fatalf("WriteMember(%x): %v", id.PublicKey, err)
		}
	}

	// Add a single membership record — both clients share the same store, and the
	// campfire membership record is keyed by campfireID (not by member pubkey), so
	// one record is sufficient for both clients to look up the transport dir.
	if err := st.AddMembership(store.Membership{
		CampfireID:    campfireID,
		TransportDir:  tr.CampfireDir(campfireID),
		JoinProtocol:  "open",
		Role:          campfire.RoleFull,
		JoinedAt:      time.Now().UnixNano(),
		Threshold:     1,
		TransportType: "filesystem",
	}); err != nil {
		t.Fatalf("AddMembership: %v", err)
	}

	return &testEnv{
		campfireID:  campfireID,
		operatorKey: opID.PublicKeyHex(),
		attackerKey: atkID.PublicKeyHex(),
		opClient:    protocol.New(st, opID),
		atkClient:   protocol.New(st, atkID),
		st:          st,
	}
}

// addMsg sends a scrip convention message via the given client.
// The message sender is determined by the client's identity — callers use
// env.opClient to send operator messages and env.atkClient to send forged messages.
func addMsg(t *testing.T, client *protocol.Client, campfireID, op string, payload any, tags ...string) {
	t.Helper()
	rawPayload, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	allTags := append([]string{op}, tags...)
	if _, err := client.Send(protocol.SendRequest{
		CampfireID: campfireID,
		Payload:    rawPayload,
		Tags:       allTags,
	}); err != nil {
		t.Fatalf("client.Send(%s): %v", op, err)
	}
}

// buildMsg constructs a proto.Message without sending it to any store.
// Used to test ApplyMessage (live-mode path) directly.
// sender must be the hex pubkey of the operator (env.operatorKey) so that
// the operator gate accepts the message.
func buildMsg(t *testing.T, campfireID, sender, op string, payload any, tags ...string) proto.Message {
	t.Helper()
	rawPayload, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	allTags := append([]string{op}, tags...)
	return proto.Message{
		ID:         randomID(t),
		CampfireID: campfireID,
		Sender:     sender,
		Payload:    rawPayload,
		Tags:       allTags,
		Timestamp:  time.Now().UnixNano(),
	}
}

func randomID(t *testing.T) string {
	t.Helper()
	return time.Now().Format("20060102150405.999999999") + t.Name()
}

// newStore creates a CampfireScripStore using a read-only protocol.Client (no identity)
// wrapping env's store. The operator key is set to env.operatorKey.
func newStore(t *testing.T, env *testEnv) *scrip.CampfireScripStore {
	t.Helper()
	// Read-only client: pass nil identity — Replay only reads, never sends.
	client := protocol.New(env.st, nil)
	cs, err := scrip.NewCampfireScripStore(env.campfireID, client, env.operatorKey)
	if err != nil {
		t.Fatalf("NewCampfireScripStore: %v", err)
	}
	return cs
}

// --- GetBudget ---

func TestGetBudget_UnknownAgent(t *testing.T) {
	env := newTestEnv(t)
	cs := newStore(t, env)
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
	env := newTestEnv(t)

	// Mint 1000 micro-tokens to Alice.
	addMsg(t, env.opClient, env.campfireID, "dontguess:scrip-mint", map[string]any{
		"recipient":   agentAlice,
		"amount":      int64(1000),
		"x402_tx_ref": "tx-001",
		"rate":        int64(1000),
	})

	cs := newStore(t, env)
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
	env := newTestEnv(t)
	cs := newStore(t, env)
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
	env := newTestEnv(t)
	// Seed 1000 via mint.
	addMsg(t, env.opClient, env.campfireID, "dontguess:scrip-mint", map[string]any{
		"recipient":   agentAlice,
		"amount":      int64(1000),
		"x402_tx_ref": "tx-002",
		"rate":        int64(1000),
	})
	cs := newStore(t, env)
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
	env := newTestEnv(t)
	addMsg(t, env.opClient, env.campfireID, "dontguess:scrip-mint", map[string]any{
		"recipient":   agentAlice,
		"amount":      int64(1000),
		"x402_tx_ref": "tx-003",
		"rate":        int64(1000),
	})
	cs := newStore(t, env)
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
	env := newTestEnv(t)
	addMsg(t, env.opClient, env.campfireID, "dontguess:scrip-mint", map[string]any{
		"recipient":   agentAlice,
		"amount":      int64(2000),
		"x402_tx_ref": "tx-010",
		"rate":        int64(1000),
	})
	cs := newStore(t, env)
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
	env := newTestEnv(t)
	addMsg(t, env.opClient, env.campfireID, "dontguess:scrip-mint", map[string]any{
		"recipient":   agentAlice,
		"amount":      int64(100),
		"x402_tx_ref": "tx-011",
		"rate":        int64(1000),
	})
	cs := newStore(t, env)
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
	env := newTestEnv(t)
	addMsg(t, env.opClient, env.campfireID, "dontguess:scrip-mint", map[string]any{
		"recipient":   agentAlice,
		"amount":      int64(1000),
		"x402_tx_ref": "tx-012",
		"rate":        int64(1000),
	})
	cs := newStore(t, env)
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
	env := newTestEnv(t)
	addMsg(t, env.opClient, env.campfireID, "dontguess:scrip-mint", map[string]any{
		"recipient":   agentAlice,
		"amount":      int64(1000),
		"x402_tx_ref": "tx-013",
		"rate":        int64(1000),
	})
	cs := newStore(t, env)
	ctx := context.Background()

	// Empty etag must be rejected (create-path semantics: no existing counter allowed).
	_, _, err := cs.DecrementBudget(ctx, agentAlice, scrip.BalanceKey, 100, "")
	if !errors.Is(err, scrip.ErrConflict) {
		t.Errorf("expected ErrConflict for empty etag, got %v", err)
	}
}

func TestDecrementBudget_UnknownAgent(t *testing.T) {
	env := newTestEnv(t)
	cs := newStore(t, env)
	ctx := context.Background()

	_, _, err := cs.DecrementBudget(ctx, "unknown-agent", scrip.BalanceKey, 100, "some-etag")
	if !errors.Is(err, scrip.ErrConflict) {
		t.Errorf("expected ErrConflict for unknown agent, got %v", err)
	}
}

// --- Reservation CRUD ---

func TestReservation_SaveGetDelete(t *testing.T) {
	env := newTestEnv(t)
	cs := newStore(t, env)
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
		t.Errorf("GetReservation: got %+v, want %+v", got, r)
	}

	// Delete.
	if err := cs.DeleteReservation(ctx, r.ID); err != nil {
		t.Fatalf("DeleteReservation: %v", err)
	}

	// Not found after delete.
	_, err = cs.GetReservation(ctx, r.ID)
	if !errors.Is(err, scrip.ErrReservationNotFound) {
		t.Errorf("expected ErrReservationNotFound after delete, got %v", err)
	}
}

func TestReservation_GetNotFound(t *testing.T) {
	env := newTestEnv(t)
	cs := newStore(t, env)
	ctx := context.Background()

	_, err := cs.GetReservation(ctx, "nonexistent")
	if !errors.Is(err, scrip.ErrReservationNotFound) {
		t.Errorf("expected ErrReservationNotFound, got %v", err)
	}
}

func TestReservation_DeleteNotFound(t *testing.T) {
	env := newTestEnv(t)
	cs := newStore(t, env)
	ctx := context.Background()

	err := cs.DeleteReservation(ctx, "nonexistent")
	if !errors.Is(err, scrip.ErrReservationNotFound) {
		t.Errorf("expected ErrReservationNotFound, got %v", err)
	}
}

func TestReservation_SaveOverwrite(t *testing.T) {
	env := newTestEnv(t)
	cs := newStore(t, env)
	ctx := context.Background()

	r := scrip.Reservation{
		ID: "res-overwrite", AgentKey: agentAlice, RK: scrip.BalanceKey,
		ETag: "1", Amount: 100, CreatedAt: time.Now().UTC(),
	}
	if err := cs.SaveReservation(ctx, r); err != nil {
		t.Fatalf("first SaveReservation: %v", err)
	}
	r.Amount = 999
	if err := cs.SaveReservation(ctx, r); err != nil {
		t.Fatalf("second SaveReservation: %v", err)
	}
	got, err := cs.GetReservation(ctx, r.ID)
	if err != nil {
		t.Fatalf("GetReservation: %v", err)
	}
	if got.Amount != 999 {
		t.Errorf("overwrite not applied: got %d, want 999", got.Amount)
	}
}

func TestConsumeReservation_ReturnsAndDeletes(t *testing.T) {
	env := newTestEnv(t)
	cs := newStore(t, env)
	ctx := context.Background()

	r := scrip.Reservation{
		ID: "res-consume-001", AgentKey: agentAlice, RK: scrip.BalanceKey,
		ETag: "1", Amount: 300, CreatedAt: time.Now().UTC(),
	}
	if err := cs.SaveReservation(ctx, r); err != nil {
		t.Fatalf("SaveReservation: %v", err)
	}

	got, err := cs.ConsumeReservation(ctx, r.ID)
	if err != nil {
		t.Fatalf("ConsumeReservation: %v", err)
	}
	if got.Amount != r.Amount {
		t.Errorf("amount = %d, want %d", got.Amount, r.Amount)
	}

	// Must be gone after consume.
	_, err = cs.GetReservation(ctx, r.ID)
	if !errors.Is(err, scrip.ErrReservationNotFound) {
		t.Errorf("expected ErrReservationNotFound after consume, got %v", err)
	}
}

func TestConsumeReservation_SecondCallReturnsNotFound(t *testing.T) {
	env := newTestEnv(t)
	cs := newStore(t, env)
	ctx := context.Background()

	r := scrip.Reservation{
		ID: "res-consume-002", AgentKey: agentAlice, RK: scrip.BalanceKey,
		ETag: "1", Amount: 100, CreatedAt: time.Now().UTC(),
	}
	if err := cs.SaveReservation(ctx, r); err != nil {
		t.Fatalf("SaveReservation: %v", err)
	}
	if _, err := cs.ConsumeReservation(ctx, r.ID); err != nil {
		t.Fatalf("first ConsumeReservation: %v", err)
	}
	_, err := cs.ConsumeReservation(ctx, r.ID)
	if !errors.Is(err, scrip.ErrReservationNotFound) {
		t.Errorf("second ConsumeReservation: expected ErrReservationNotFound, got %v", err)
	}
}

func TestConsumeReservation_NotFound(t *testing.T) {
	env := newTestEnv(t)
	cs := newStore(t, env)
	ctx := context.Background()

	_, err := cs.ConsumeReservation(ctx, "no-such-reservation")
	if !errors.Is(err, scrip.ErrReservationNotFound) {
		t.Errorf("expected ErrReservationNotFound, got %v", err)
	}
}

// --- Replay ---

func TestReplay_MintAndBalance(t *testing.T) {
	env := newTestEnv(t)

	// Mint 3000 to Alice.
	addMsg(t, env.opClient, env.campfireID, "dontguess:scrip-mint", map[string]any{
		"recipient":   agentAlice,
		"amount":      int64(3000),
		"x402_tx_ref": "tx-r01",
		"rate":        int64(1000),
	})
	// Mint 1000 to Bob.
	addMsg(t, env.opClient, env.campfireID, "dontguess:scrip-mint", map[string]any{
		"recipient":   agentBob,
		"amount":      int64(1000),
		"x402_tx_ref": "tx-r02",
		"rate":        int64(1000),
	})

	cs := newStore(t, env)

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
	env := newTestEnv(t)

	// Seed operator balance via mint to self.
	addMsg(t, env.opClient, env.campfireID, "dontguess:scrip-mint", map[string]any{
		"recipient":   env.operatorKey,
		"amount":      int64(10000),
		"x402_tx_ref": "tx-op",
		"rate":        int64(1000),
	})
	// Operator pays Alice for a put.
	addMsg(t, env.opClient, env.campfireID, "dontguess:scrip-put-pay", map[string]any{
		"seller":       agentAlice,
		"amount":       int64(700),
		"token_cost":   int64(1000),
		"discount_pct": 70,
		"result_hash":  "abc123",
		"put_msg":      "put-msg-001",
	})

	cs := newStore(t, env)

	// Alice receives 700.
	if cs.Balance(agentAlice) != 700 {
		t.Errorf("Alice = %d, want 700", cs.Balance(agentAlice))
	}
	// Operator: 10000 - 700 = 9300.
	if cs.Balance(env.operatorKey) != 9300 {
		t.Errorf("Operator = %d, want 9300", cs.Balance(env.operatorKey))
	}
}

func TestReplay_BuyHoldAndDisputeRefund(t *testing.T) {
	env := newTestEnv(t)

	// Alice minted 2000.
	addMsg(t, env.opClient, env.campfireID, "dontguess:scrip-mint", map[string]any{
		"recipient":   agentAlice,
		"amount":      int64(2000),
		"x402_tx_ref": "tx-r10",
		"rate":        int64(1000),
	})
	// Alice buy-hold for 500 (price=450 + fee=50).
	addMsg(t, env.opClient, env.campfireID, "dontguess:scrip-buy-hold", map[string]any{
		"buyer":          agentAlice,
		"amount":         int64(500),
		"price":          int64(450),
		"fee":            int64(50),
		"reservation_id": "res-bh-001",
		"buy_msg":        "buy-msg-001",
		"expires_at":     time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339),
	})

	cs := newStore(t, env)

	if cs.Balance(agentAlice) != 1500 {
		t.Errorf("Alice after hold = %d, want 1500", cs.Balance(agentAlice))
	}

	// Dispute refund: full 500 back to Alice.
	addMsg(t, env.opClient, env.campfireID, "dontguess:scrip-dispute-refund", map[string]any{
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
	env := newTestEnv(t)

	// Seed: operator=5000, Alice=2000 (seller), Bob=1000 (buyer).
	addMsg(t, env.opClient, env.campfireID, "dontguess:scrip-mint", map[string]any{
		"recipient": env.operatorKey, "amount": int64(5000), "x402_tx_ref": "op-tx", "rate": int64(1000),
	})
	addMsg(t, env.opClient, env.campfireID, "dontguess:scrip-mint", map[string]any{
		"recipient": agentAlice, "amount": int64(2000), "x402_tx_ref": "alice-tx", "rate": int64(1000),
	})
	addMsg(t, env.opClient, env.campfireID, "dontguess:scrip-mint", map[string]any{
		"recipient": agentBob, "amount": int64(1000), "x402_tx_ref": "bob-tx", "rate": int64(1000),
	})

	// Bob buys (hold 500 = price 450 + fee 50).
	addMsg(t, env.opClient, env.campfireID, "dontguess:scrip-buy-hold", map[string]any{
		"buyer": agentBob, "amount": int64(500), "price": int64(450), "fee": int64(50),
		"reservation_id": "res-settle-001", "buy_msg": "buy-002",
		"expires_at": time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339),
	})

	// Settle: residual=90 to Alice (seller), fee_burned=50, exchange_revenue=360 to operator.
	addMsg(t, env.opClient, env.campfireID, "dontguess:scrip-settle", map[string]any{
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
	addMsg(t, env.opClient, env.campfireID, "dontguess:scrip-burn", map[string]any{
		"amount": int64(50),
		"reason": "matching-fee",
	})

	cs := newStore(t, env)

	// Bob: 1000 - 500 = 500
	if cs.Balance(agentBob) != 500 {
		t.Errorf("Bob = %d, want 500", cs.Balance(agentBob))
	}
	// Alice: 2000 + 90 = 2090
	if cs.Balance(agentAlice) != 2090 {
		t.Errorf("Alice = %d, want 2090", cs.Balance(agentAlice))
	}
	// Operator: 5000 + 360 = 5360
	if cs.Balance(env.operatorKey) != 5360 {
		t.Errorf("Operator = %d, want 5360", cs.Balance(env.operatorKey))
	}
	// TotalBurned must equal the fee exactly once — not 2*fee.
	// The scrip-burn message is the sole source; applySettle does not increment totalBurned.
	if cs.TotalBurned() != 50 {
		t.Errorf("TotalBurned = %d, want 50 (must not double-count)", cs.TotalBurned())
	}
}

func TestReplay_AssignPay(t *testing.T) {
	env := newTestEnv(t)

	// Seed operator.
	addMsg(t, env.opClient, env.campfireID, "dontguess:scrip-mint", map[string]any{
		"recipient": env.operatorKey, "amount": int64(5000), "x402_tx_ref": "op2", "rate": int64(1000),
	})
	// Pay Bob for labor.
	addMsg(t, env.opClient, env.campfireID, "dontguess:scrip-assign-pay", map[string]any{
		"worker":      agentBob,
		"amount":      int64(300),
		"task_type":   "validate",
		"assign_msg":  "assign-001",
		"result_hash": "",
	})

	cs := newStore(t, env)

	if cs.Balance(agentBob) != 300 {
		t.Errorf("Bob = %d, want 300", cs.Balance(agentBob))
	}
	if cs.Balance(env.operatorKey) != 4700 {
		t.Errorf("Operator = %d, want 4700", cs.Balance(env.operatorKey))
	}
}

func TestReplay_BurnOnly(t *testing.T) {
	env := newTestEnv(t)

	// Burn message (no balance change — just total burned).
	addMsg(t, env.opClient, env.campfireID, "dontguess:scrip-burn", map[string]any{
		"amount": int64(200),
		"reason": "matching-fee",
	})

	cs := newStore(t, env)

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
	env := newTestEnv(t)
	const fee = int64(100)

	// Bob buys; hold removes scrip from buyer.
	addMsg(t, env.opClient, env.campfireID, "dontguess:scrip-buy-hold", map[string]any{
		"buyer": agentBob, "amount": fee * 5, "price": fee * 4, "fee": fee,
		"reservation_id": "res-double-001", "buy_msg": "buy-001",
		"expires_at": time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339),
	})

	// Engine emits scrip-settle (contains fee_burned in payload).
	addMsg(t, env.opClient, env.campfireID, "dontguess:scrip-settle", map[string]any{
		"reservation_id":   "res-double-001",
		"seller":           agentAlice,
		"residual":         int64(40),
		"fee_burned":       fee,
		"exchange_revenue": int64(360),
		"match_msg":        "match-001",
		"result_hash":      "deadbeef",
	})

	// Engine also emits scrip-burn for the same matching fee.
	addMsg(t, env.opClient, env.campfireID, "dontguess:scrip-burn", map[string]any{
		"amount": fee,
		"reason": "matching-fee",
	})

	cs := newStore(t, env)

	if cs.TotalBurned() != fee {
		t.Errorf("TotalBurned = %d, want %d (must count burn message once only)", cs.TotalBurned(), fee)
	}
}

func TestReplay_IdempotentReplay(t *testing.T) {
	env := newTestEnv(t)

	addMsg(t, env.opClient, env.campfireID, "dontguess:scrip-mint", map[string]any{
		"recipient": agentAlice, "amount": int64(1000), "x402_tx_ref": "tx-idem", "rate": int64(1000),
	})

	cs := newStore(t, env)

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
	env := newTestEnv(t)
	// Seed Alice with 5000.
	addMsg(t, env.opClient, env.campfireID, "dontguess:scrip-mint", map[string]any{
		"recipient": agentAlice, "amount": int64(5000), "x402_tx_ref": "tx-rt", "rate": int64(1000),
	})

	cs := newStore(t, env)
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
	env := newTestEnv(t)

	// Forged mint: attacker tries to mint 9999 to themselves.
	addMsg(t, env.atkClient, env.campfireID, "dontguess:scrip-mint", map[string]any{
		"recipient":   env.attackerKey,
		"amount":      int64(9999),
		"x402_tx_ref": "forged-tx-001",
		"rate":        int64(1000),
	})

	// Construct store with operator key set — forged message must be ignored.
	cs := newStore(t, env)

	if bal := cs.Balance(env.attackerKey); bal != 0 {
		t.Errorf("forged mint accepted: attacker balance = %d, want 0", bal)
	}
	if cs.TotalSupply() != 0 {
		t.Errorf("forged mint affected TotalSupply: got %d, want 0", cs.TotalSupply())
	}
}

// TestOperatorGate_LegitimateOperatorMintAccepted verifies that a scrip-mint from
// the configured operator key is processed normally.
func TestOperatorGate_LegitimateOperatorMintAccepted(t *testing.T) {
	env := newTestEnv(t)

	// Legitimate mint from the operator.
	addMsg(t, env.opClient, env.campfireID, "dontguess:scrip-mint", map[string]any{
		"recipient":   agentAlice,
		"amount":      int64(5000),
		"x402_tx_ref": "legit-tx-001",
		"rate":        int64(1000),
	})

	cs := newStore(t, env)

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
	env := newTestEnv(t)

	// Attacker mints to themselves (should be rejected).
	addMsg(t, env.atkClient, env.campfireID, "dontguess:scrip-mint", map[string]any{
		"recipient": env.attackerKey, "amount": int64(9999), "x402_tx_ref": "forged-01", "rate": int64(1000),
	})
	// Operator mints to Alice (should be accepted).
	addMsg(t, env.opClient, env.campfireID, "dontguess:scrip-mint", map[string]any{
		"recipient": agentAlice, "amount": int64(1000), "x402_tx_ref": "legit-01", "rate": int64(1000),
	})
	// Attacker mints to Alice (should be rejected).
	addMsg(t, env.atkClient, env.campfireID, "dontguess:scrip-mint", map[string]any{
		"recipient": agentAlice, "amount": int64(8888), "x402_tx_ref": "forged-02", "rate": int64(1000),
	})

	cs := newStore(t, env)

	if bal := cs.Balance(env.attackerKey); bal != 0 {
		t.Errorf("forged mint applied to attacker: balance = %d, want 0", bal)
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
	env := newTestEnv(t)

	// Mint from a non-operator sender — should be accepted when OperatorKey is "".
	addMsg(t, env.atkClient, env.campfireID, "dontguess:scrip-mint", map[string]any{
		"recipient": agentAlice, "amount": int64(500), "x402_tx_ref": "any-sender", "rate": int64(1000),
	})

	// Use a read-only client with empty operator key.
	client := protocol.New(env.st, nil)
	cs, err := scrip.NewCampfireScripStore(env.campfireID, client, "")
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
	env := newTestEnv(t)

	// buy-hold without a prior mint — replay must not reject it.
	addMsg(t, env.opClient, env.campfireID, "dontguess:scrip-buy-hold", map[string]any{
		"buyer":  agentAlice,
		"amount": int64(500),
	})

	cs := newStore(t, env)

	// Balance must be -500 (negative allowed in replay mode).
	if bal := cs.Balance(agentAlice); bal != -500 {
		t.Errorf("replay: expected negative balance -500, got %d", bal)
	}
}

// TestReplay_NegativeBalance_SubsequentDecrementBudgetFails verifies the lockout
// scenario: if replay produced a negative balance, DecrementBudget must return
// ErrBudgetExceeded — not a panic or silent success.
func TestReplay_NegativeBalance_SubsequentDecrementBudgetFails(t *testing.T) {
	env := newTestEnv(t)

	// buy-hold without a prior mint → negative balance after replay.
	addMsg(t, env.opClient, env.campfireID, "dontguess:scrip-buy-hold", map[string]any{
		"buyer":  agentAlice,
		"amount": int64(500),
	})

	cs := newStore(t, env)
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

// TestLiveMode_SubtractFromBalance_RejectsUnderflow verifies the underflow guard:
// a buy-hold message received in live mode (post-Replay, replaying=false) that
// would drive the balance negative must be hard-rejected — the balance must remain
// unchanged and not go negative or be clamped to zero.
//
// We test this via ApplyMessage, which processes a single message in live mode
// (replaying=false). Contrast with TestReplay_NegativeBalanceAllowed which shows
// that replay mode permits negative balances.
func TestLiveMode_SubtractFromBalance_RejectsUnderflow(t *testing.T) {
	env := newTestEnv(t)

	// Mint 200 to Alice so she has a positive balance post-replay.
	addMsg(t, env.opClient, env.campfireID, "dontguess:scrip-mint", map[string]any{
		"recipient":   agentAlice,
		"amount":      int64(200),
		"x402_tx_ref": "tx-live-reject",
		"rate":        int64(1000),
	})

	cs := newStore(t, env)
	if cs.Balance(agentAlice) != 200 {
		t.Fatalf("precondition: Alice balance = %d, want 200", cs.Balance(agentAlice))
	}

	// Construct a buy-hold message that would drive Alice 300 below zero (500 > 200).
	// Apply it via ApplyMessage — this runs in live mode (replaying=false).
	liveMsg := buildMsg(t, env.campfireID, env.operatorKey, "dontguess:scrip-buy-hold", map[string]any{
		"buyer":  agentAlice,
		"amount": int64(500),
	})
	cs.ApplyMessage(&liveMsg)

	// Balance must be unchanged at 200 — hard-reject, not clamp.
	bal := cs.Balance(agentAlice)
	if bal != 200 {
		t.Errorf("live mode: expected balance unchanged at 200 after underflow rejection, got %d", bal)
	}
}

// TestLiveMode_SubtractFromBalance_ZeroBalance_Rejects verifies that a
// subtractFromBalance call against a zero balance in live mode is hard-rejected:
// the message is dropped and the balance remains at zero.
func TestLiveMode_SubtractFromBalance_ZeroBalance_Rejects(t *testing.T) {
	env := newTestEnv(t)

	// Alice has no prior mint — balance starts at 0.
	cs := newStore(t, env)
	if cs.Balance(agentAlice) != 0 {
		t.Fatalf("precondition: Alice balance = %d, want 0", cs.Balance(agentAlice))
	}

	// A buy-hold on a zero balance must be rejected in live mode.
	liveMsg := buildMsg(t, env.campfireID, env.operatorKey, "dontguess:scrip-buy-hold", map[string]any{
		"buyer":  agentAlice,
		"amount": int64(1),
	})
	cs.ApplyMessage(&liveMsg)

	bal := cs.Balance(agentAlice)
	if bal != 0 {
		t.Errorf("live mode: expected balance unchanged at 0 after underflow rejection, got %d", bal)
	}
}

// TestConcurrentGetReservation_NoDeadlock verifies that concurrent GetReservation
// calls do not deadlock or data-race under resMu (RWMutex allows multiple readers).
// This is a regression test for the resMu sync.Mutex → sync.RWMutex change
// (dontguess-g6d safety hardening).
func TestConcurrentGetReservation_NoDeadlock(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	cs := newStore(t, env)
	ctx := context.Background()

	const numReservations = 10
	const numReaders = 20

	// Seed reservations.
	for i := 0; i < numReservations; i++ {
		r := scrip.Reservation{
			ID:        fmt.Sprintf("concurrent-res-%03d", i),
			AgentKey:  agentAlice,
			RK:        scrip.BalanceKey,
			ETag:      fmt.Sprintf("%d", i),
			Amount:    int64(100 + i),
			CreatedAt: time.Now().UTC().Truncate(time.Second),
		}
		if err := cs.SaveReservation(ctx, r); err != nil {
			t.Fatalf("SaveReservation %d: %v", i, err)
		}
	}

	// Launch concurrent readers. Each goroutine reads all reservations
	// simultaneously. The test passes if there is no deadlock and no panic.
	var wg sync.WaitGroup
	for g := 0; g < numReaders; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < numReservations; i++ {
				id := fmt.Sprintf("concurrent-res-%03d", i)
				if _, err := cs.GetReservation(ctx, id); err != nil {
					// Not fatal from goroutine; the missing reservation would
					// be caught by the main goroutine's assertions below.
					_ = err
				}
			}
		}()
	}
	wg.Wait() // deadlock here means resMu is not an RWMutex

	// Verify all reservations are still readable after concurrent load.
	for i := 0; i < numReservations; i++ {
		id := fmt.Sprintf("concurrent-res-%03d", i)
		got, err := cs.GetReservation(ctx, id)
		if err != nil {
			t.Errorf("GetReservation(%q) after concurrent reads: %v", id, err)
			continue
		}
		if got.ID != id {
			t.Errorf("GetReservation(%q): got ID %q", id, got.ID)
		}
	}
}

// --- scrip:loan-mint tests ---

// TestLoanMint_LoanRecordStored verifies that applyLoanMint creates a LoanRecord
// in the store, credits the borrower's balance, and indexes by borrower key.
func TestLoanMint_LoanRecordStored(t *testing.T) {
	env := newTestEnv(t)

	dueAt := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	addMsg(t, env.opClient, env.campfireID, scrip.TagScripLoanMint, scrip.LoanMintPayload{
		Borrower:          agentAlice,
		Principal:         500,
		VigRateBPS:        200,
		DueAt:             dueAt,
		LoanID:            "loan-001",
		SettlementMsgID:   "settle-msg-abc",
		CommitmentTokenID: "token-xyz",
	})

	cs := newStore(t, env)

	// Borrower balance must equal the minted principal.
	bal := cs.Balance(agentAlice)
	if bal != 500 {
		t.Errorf("balance = %d, want 500", bal)
	}

	// LoanRecord must be retrievable.
	rec, ok := cs.GetLoan("loan-001")
	if !ok {
		t.Fatal("GetLoan(loan-001): not found")
	}
	if rec.BorrowerKey != agentAlice {
		t.Errorf("BorrowerKey = %q, want %q", rec.BorrowerKey, agentAlice)
	}
	if rec.Principal != 500 {
		t.Errorf("Principal = %d, want 500", rec.Principal)
	}
	if rec.VigRateBPS != 200 {
		t.Errorf("VigRateBPS = %d, want 200", rec.VigRateBPS)
	}
	if rec.SettlementMsgID != "settle-msg-abc" {
		t.Errorf("SettlementMsgID = %q, want settle-msg-abc", rec.SettlementMsgID)
	}
	if rec.CommitmentID != "token-xyz" {
		t.Errorf("CommitmentID = %q, want token-xyz", rec.CommitmentID)
	}
	if rec.Status != scrip.LoanActive {
		t.Errorf("Status = %v, want LoanActive", rec.Status)
	}

	// loansByBorrower index must include the loan.
	ids := cs.LoansByBorrower(agentAlice)
	if len(ids) != 1 || ids[0] != "loan-001" {
		t.Errorf("LoansByBorrower = %v, want [loan-001]", ids)
	}
}

// TestLoanMint_TotalSupplyInvariant verifies that loan-mint increments totalSupply
// and totalLoanPrincipal by the principal, and that they track independently of
// regular mints.
func TestLoanMint_TotalSupplyInvariant(t *testing.T) {
	env := newTestEnv(t)

	// Regular mint for 1000 to Bob.
	addMsg(t, env.opClient, env.campfireID, scrip.TagScripMint, scrip.MintPayload{
		Recipient: agentBob,
		Amount:    1000,
		Rate:      1000,
	})

	// Loan mint for 500 to Alice.
	dueAt := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	addMsg(t, env.opClient, env.campfireID, scrip.TagScripLoanMint, scrip.LoanMintPayload{
		Borrower:          agentAlice,
		Principal:         500,
		VigRateBPS:        200,
		DueAt:             dueAt,
		LoanID:            "loan-supply-test",
		SettlementMsgID:   "settle-msg-supply",
		CommitmentTokenID: "token-supply",
	})

	cs := newStore(t, env)

	// TotalSupply = regular mint + loan mint.
	if got := cs.TotalSupply(); got != 1500 {
		t.Errorf("TotalSupply = %d, want 1500", got)
	}

	// TotalLoanPrincipal tracks only the loan portion.
	if got := cs.TotalLoanPrincipal(); got != 500 {
		t.Errorf("TotalLoanPrincipal = %d, want 500", got)
	}
}

// TestLoanMint_Replay verifies that a loan-mint message is correctly replayed
// on Replay(), producing identical state to an initial build from the log.
func TestLoanMint_Replay(t *testing.T) {
	env := newTestEnv(t)

	dueAt := time.Now().Add(12 * time.Hour).UTC().Format(time.RFC3339)
	addMsg(t, env.opClient, env.campfireID, scrip.TagScripLoanMint, scrip.LoanMintPayload{
		Borrower:          agentAlice,
		Principal:         300,
		VigRateBPS:        100,
		DueAt:             dueAt,
		LoanID:            "loan-replay",
		SettlementMsgID:   "settle-replay",
		CommitmentTokenID: "token-replay",
	})

	cs := newStore(t, env)

	// Verify state before replay.
	if cs.Balance(agentAlice) != 300 {
		t.Fatalf("pre-replay balance = %d, want 300", cs.Balance(agentAlice))
	}

	// Force a Replay and verify state is identical.
	if err := cs.Replay(); err != nil {
		t.Fatalf("Replay: %v", err)
	}

	if bal := cs.Balance(agentAlice); bal != 300 {
		t.Errorf("post-replay balance = %d, want 300", bal)
	}
	if got := cs.TotalSupply(); got != 300 {
		t.Errorf("post-replay TotalSupply = %d, want 300", got)
	}
	if got := cs.TotalLoanPrincipal(); got != 300 {
		t.Errorf("post-replay TotalLoanPrincipal = %d, want 300", got)
	}

	rec, ok := cs.GetLoan("loan-replay")
	if !ok {
		t.Fatal("post-replay GetLoan(loan-replay): not found")
	}
	if rec.Principal != 300 {
		t.Errorf("post-replay Principal = %d, want 300", rec.Principal)
	}
}

// TestLoanMint_LiveMode verifies that ApplyMessage in live mode applies a loan-mint
// message and updates balance, supply, and loan records correctly.
func TestLoanMint_LiveMode(t *testing.T) {
	env := newTestEnv(t)

	// Build a store with no prior messages.
	cs := newStore(t, env)

	dueAt := time.Now().Add(6 * time.Hour).UTC().Format(time.RFC3339)
	payloadBytes, err := json.Marshal(scrip.LoanMintPayload{
		Borrower:          agentBob,
		Principal:         750,
		VigRateBPS:        150,
		DueAt:             dueAt,
		LoanID:            "loan-live",
		SettlementMsgID:   "settle-live",
		CommitmentTokenID: "token-live",
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	liveMsg := proto.Message{
		ID:         "loan-live-msg-id",
		CampfireID: env.campfireID,
		Sender:     env.operatorKey,
		Payload:    payloadBytes,
		Tags:       []string{scrip.TagScripLoanMint},
		Timestamp:  time.Now().UnixNano(),
	}
	cs.ApplyMessage(&liveMsg)

	if bal := cs.Balance(agentBob); bal != 750 {
		t.Errorf("live mode balance = %d, want 750", bal)
	}
	if got := cs.TotalLoanPrincipal(); got != 750 {
		t.Errorf("live mode TotalLoanPrincipal = %d, want 750", got)
	}
	rec, ok := cs.GetLoan("loan-live")
	if !ok {
		t.Fatal("live mode GetLoan(loan-live): not found")
	}
	if rec.BorrowerKey != agentBob {
		t.Errorf("live mode BorrowerKey = %q, want %q", rec.BorrowerKey, agentBob)
	}
}

// TestLoanMint_DuplicateLoanID_Idempotent verifies that a duplicate loan_id in two
// distinct messages does not double-credit the borrower or create a duplicate record.
func TestLoanMint_DuplicateLoanID_Idempotent(t *testing.T) {
	env := newTestEnv(t)

	dueAt := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	payload := scrip.LoanMintPayload{
		Borrower:          agentAlice,
		Principal:         400,
		VigRateBPS:        200,
		DueAt:             dueAt,
		LoanID:            "loan-dup",
		SettlementMsgID:   "settle-dup",
		CommitmentTokenID: "token-dup",
	}
	addMsg(t, env.opClient, env.campfireID, scrip.TagScripLoanMint, payload)

	// Second message — same loan_id, different message ID (seenMsgIDs won't filter it).
	payload2 := scrip.LoanMintPayload{
		Borrower:          agentAlice,
		Principal:         400,
		VigRateBPS:        200,
		DueAt:             dueAt,
		LoanID:            "loan-dup", // same loan_id
		SettlementMsgID:   "settle-dup-2",
		CommitmentTokenID: "token-dup-2",
	}
	addMsg(t, env.opClient, env.campfireID, scrip.TagScripLoanMint, payload2)

	cs := newStore(t, env)

	// Only the first loan should have been applied; balance must be 400, not 800.
	if bal := cs.Balance(agentAlice); bal != 400 {
		t.Errorf("balance = %d, want 400 (duplicate loan_id must not double-credit)", bal)
	}
	if got := cs.TotalLoanPrincipal(); got != 400 {
		t.Errorf("TotalLoanPrincipal = %d, want 400", got)
	}
	ids := cs.LoansByBorrower(agentAlice)
	if len(ids) != 1 {
		t.Errorf("LoansByBorrower len = %d, want 1 (duplicate must not create second record)", len(ids))
	}
}

// TestLoanMint_InvalidPayload_Rejected verifies that loan-mint messages with missing
// required fields (borrower, loan_id, principal <= 0) are silently ignored.
func TestLoanMint_InvalidPayload_Rejected(t *testing.T) {
	env := newTestEnv(t)

	// Missing borrower.
	addMsg(t, env.opClient, env.campfireID, scrip.TagScripLoanMint, map[string]any{
		"principal": int64(100),
		"loan_id":   "loan-no-borrower",
	})
	// Missing loan_id.
	addMsg(t, env.opClient, env.campfireID, scrip.TagScripLoanMint, map[string]any{
		"borrower":  agentAlice,
		"principal": int64(100),
	})
	// Zero principal.
	addMsg(t, env.opClient, env.campfireID, scrip.TagScripLoanMint, map[string]any{
		"borrower":  agentAlice,
		"loan_id":   "loan-zero-principal",
		"principal": int64(0),
	})

	cs := newStore(t, env)

	if bal := cs.Balance(agentAlice); bal != 0 {
		t.Errorf("balance = %d, want 0 (invalid messages must not credit borrower)", bal)
	}
	if got := cs.TotalLoanPrincipal(); got != 0 {
		t.Errorf("TotalLoanPrincipal = %d, want 0", got)
	}
}

// TestLoanMint_MultipleBorrowers verifies that multiple borrowers each get their own
// loan records and balances, and loansByBorrower indexes them separately.
func TestLoanMint_MultipleBorrowers(t *testing.T) {
	env := newTestEnv(t)

	dueAt := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	addMsg(t, env.opClient, env.campfireID, scrip.TagScripLoanMint, scrip.LoanMintPayload{
		Borrower:          agentAlice,
		Principal:         600,
		VigRateBPS:        200,
		DueAt:             dueAt,
		LoanID:            "loan-alice-1",
		SettlementMsgID:   "settle-a1",
		CommitmentTokenID: "token-a1",
	})
	addMsg(t, env.opClient, env.campfireID, scrip.TagScripLoanMint, scrip.LoanMintPayload{
		Borrower:          agentAlice,
		Principal:         400,
		VigRateBPS:        150,
		DueAt:             dueAt,
		LoanID:            "loan-alice-2",
		SettlementMsgID:   "settle-a2",
		CommitmentTokenID: "token-a2",
	})
	addMsg(t, env.opClient, env.campfireID, scrip.TagScripLoanMint, scrip.LoanMintPayload{
		Borrower:          agentBob,
		Principal:         200,
		VigRateBPS:        300,
		DueAt:             dueAt,
		LoanID:            "loan-bob-1",
		SettlementMsgID:   "settle-b1",
		CommitmentTokenID: "token-b1",
	})

	cs := newStore(t, env)

	if bal := cs.Balance(agentAlice); bal != 1000 {
		t.Errorf("Alice balance = %d, want 1000", bal)
	}
	if bal := cs.Balance(agentBob); bal != 200 {
		t.Errorf("Bob balance = %d, want 200", bal)
	}

	aliceLoans := cs.LoansByBorrower(agentAlice)
	if len(aliceLoans) != 2 {
		t.Errorf("Alice loan count = %d, want 2", len(aliceLoans))
	}
	bobLoans := cs.LoansByBorrower(agentBob)
	if len(bobLoans) != 1 {
		t.Errorf("Bob loan count = %d, want 1", len(bobLoans))
	}

	// TotalSupply and TotalLoanPrincipal cover all three loans.
	if got := cs.TotalSupply(); got != 1200 {
		t.Errorf("TotalSupply = %d, want 1200", got)
	}
	if got := cs.TotalLoanPrincipal(); got != 1200 {
		t.Errorf("TotalLoanPrincipal = %d, want 1200", got)
	}
}

// --- scrip:loan-repay tests ---

// TestLoanRepay_PartialRepay verifies that a partial repayment increments
// LoanRecord.Repaid and burns scrip from totalSupply without transitioning the
// loan to LoanRepaid.
func TestLoanRepay_PartialRepay(t *testing.T) {
	env := newTestEnv(t)

	dueAt := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	addMsg(t, env.opClient, env.campfireID, scrip.TagScripLoanMint, scrip.LoanMintPayload{
		Borrower:  agentAlice,
		Principal: 1000,
		DueAt:     dueAt,
		LoanID:    "loan-repay-partial",
	})

	// Partial repayment: 400 of 1000.
	addMsg(t, env.opClient, env.campfireID, scrip.TagScripLoanRepay, scrip.LoanRepayPayload{
		LoanID: "loan-repay-partial",
		Amount: 400,
	})

	cs := newStore(t, env)

	rec, ok := cs.GetLoan("loan-repay-partial")
	if !ok {
		t.Fatal("GetLoan: not found")
	}
	if rec.Repaid != 400 {
		t.Errorf("Repaid = %d, want 400", rec.Repaid)
	}
	if rec.Status != scrip.LoanActive {
		t.Errorf("Status = %v, want LoanActive (not fully repaid)", rec.Status)
	}
	// totalSupply reduced by repayment amount; totalLoanPrincipal unchanged until full repayment.
	if got := cs.TotalSupply(); got != 600 {
		t.Errorf("TotalSupply = %d, want 600", got)
	}
	if got := cs.TotalLoanPrincipal(); got != 1000 {
		t.Errorf("TotalLoanPrincipal = %d, want 1000", got)
	}
}

// TestLoanRepay_FullRepay verifies that a full repayment sets LoanRecord.Status
// to LoanRepaid and decrements totalLoanPrincipal by the principal.
func TestLoanRepay_FullRepay(t *testing.T) {
	env := newTestEnv(t)

	dueAt := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	addMsg(t, env.opClient, env.campfireID, scrip.TagScripLoanMint, scrip.LoanMintPayload{
		Borrower:  agentAlice,
		Principal: 800,
		DueAt:     dueAt,
		LoanID:    "loan-repay-full",
	})

	// Full repayment in one message.
	addMsg(t, env.opClient, env.campfireID, scrip.TagScripLoanRepay, scrip.LoanRepayPayload{
		LoanID: "loan-repay-full",
		Amount: 800,
	})

	cs := newStore(t, env)

	rec, ok := cs.GetLoan("loan-repay-full")
	if !ok {
		t.Fatal("GetLoan: not found")
	}
	if rec.Repaid != 800 {
		t.Errorf("Repaid = %d, want 800", rec.Repaid)
	}
	if rec.Status != scrip.LoanRepaid {
		t.Errorf("Status = %v, want LoanRepaid", rec.Status)
	}
	// Both supply counters reduced.
	if got := cs.TotalSupply(); got != 0 {
		t.Errorf("TotalSupply = %d, want 0", got)
	}
	if got := cs.TotalLoanPrincipal(); got != 0 {
		t.Errorf("TotalLoanPrincipal = %d, want 0", got)
	}
}

// TestLoanRepay_TwoInstalmentsFullRepay verifies that two partial repayments
// that sum to the principal transition the loan to LoanRepaid.
func TestLoanRepay_TwoInstalmentsFullRepay(t *testing.T) {
	env := newTestEnv(t)

	dueAt := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	addMsg(t, env.opClient, env.campfireID, scrip.TagScripLoanMint, scrip.LoanMintPayload{
		Borrower:  agentAlice,
		Principal: 600,
		DueAt:     dueAt,
		LoanID:    "loan-repay-two",
	})

	addMsg(t, env.opClient, env.campfireID, scrip.TagScripLoanRepay, scrip.LoanRepayPayload{
		LoanID: "loan-repay-two",
		Amount: 300,
	})
	addMsg(t, env.opClient, env.campfireID, scrip.TagScripLoanRepay, scrip.LoanRepayPayload{
		LoanID: "loan-repay-two",
		Amount: 300,
	})

	cs := newStore(t, env)

	rec, ok := cs.GetLoan("loan-repay-two")
	if !ok {
		t.Fatal("GetLoan: not found")
	}
	if rec.Repaid != 600 {
		t.Errorf("Repaid = %d, want 600", rec.Repaid)
	}
	if rec.Status != scrip.LoanRepaid {
		t.Errorf("Status = %v, want LoanRepaid", rec.Status)
	}
	if got := cs.TotalLoanPrincipal(); got != 0 {
		t.Errorf("TotalLoanPrincipal = %d, want 0", got)
	}
}

// TestLoanRepay_UnknownLoan_Ignored verifies that a repayment for an unknown
// loan_id is silently ignored without panicking or corrupting state.
func TestLoanRepay_UnknownLoan_Ignored(t *testing.T) {
	env := newTestEnv(t)

	addMsg(t, env.opClient, env.campfireID, scrip.TagScripLoanRepay, scrip.LoanRepayPayload{
		LoanID: "does-not-exist",
		Amount: 500,
	})

	cs := newStore(t, env)

	if got := cs.TotalSupply(); got != 0 {
		t.Errorf("TotalSupply = %d, want 0 (no-op for unknown loan)", got)
	}
}

// TestLoanRepay_AlreadyRepaid_Ignored verifies that a repayment message for a
// loan already in LoanRepaid status is silently ignored.
func TestLoanRepay_AlreadyRepaid_Ignored(t *testing.T) {
	env := newTestEnv(t)

	dueAt := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	addMsg(t, env.opClient, env.campfireID, scrip.TagScripLoanMint, scrip.LoanMintPayload{
		Borrower:  agentAlice,
		Principal: 200,
		DueAt:     dueAt,
		LoanID:    "loan-already-repaid",
	})
	// Full repayment.
	addMsg(t, env.opClient, env.campfireID, scrip.TagScripLoanRepay, scrip.LoanRepayPayload{
		LoanID: "loan-already-repaid",
		Amount: 200,
	})
	// Attempt second repayment — must be ignored.
	addMsg(t, env.opClient, env.campfireID, scrip.TagScripLoanRepay, scrip.LoanRepayPayload{
		LoanID: "loan-already-repaid",
		Amount: 100,
	})

	cs := newStore(t, env)

	rec, ok := cs.GetLoan("loan-already-repaid")
	if !ok {
		t.Fatal("GetLoan: not found")
	}
	if rec.Repaid != 200 {
		t.Errorf("Repaid = %d, want 200 (second repay must be ignored)", rec.Repaid)
	}
	if rec.Status != scrip.LoanRepaid {
		t.Errorf("Status = %v, want LoanRepaid", rec.Status)
	}
	// totalSupply must only have been decremented by the first repayment.
	if got := cs.TotalSupply(); got != 0 {
		t.Errorf("TotalSupply = %d, want 0", got)
	}
}

// --- scrip:loan-vig-accrue tests ---

// TestLoanVigAccrue_AccruesToOutstanding verifies that a vig-accrue message
// increments LoanRecord.Outstanding without affecting Repaid or totalSupply.
func TestLoanVigAccrue_AccruesToOutstanding(t *testing.T) {
	env := newTestEnv(t)

	dueAt := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	addMsg(t, env.opClient, env.campfireID, scrip.TagScripLoanMint, scrip.LoanMintPayload{
		Borrower:  agentAlice,
		Principal: 1000,
		DueAt:     dueAt,
		LoanID:    "loan-vig-accrue",
	})

	addMsg(t, env.opClient, env.campfireID, scrip.TagScripLoanVigAccrue, scrip.LoanVigAccruePayload{
		LoanID: "loan-vig-accrue",
		Amount: 50,
	})

	cs := newStore(t, env)

	rec, ok := cs.GetLoan("loan-vig-accrue")
	if !ok {
		t.Fatal("GetLoan: not found")
	}
	if rec.Outstanding != 50 {
		t.Errorf("Outstanding = %d, want 50", rec.Outstanding)
	}
	if rec.Repaid != 0 {
		t.Errorf("Repaid = %d, want 0 (vig does not touch Repaid)", rec.Repaid)
	}
	// totalSupply is not affected by vig accrual.
	if got := cs.TotalSupply(); got != 1000 {
		t.Errorf("TotalSupply = %d, want 1000", got)
	}
}

// TestLoanVigAccrue_Cumulative verifies that multiple vig-accrue messages
// accumulate correctly in LoanRecord.Outstanding.
func TestLoanVigAccrue_Cumulative(t *testing.T) {
	env := newTestEnv(t)

	dueAt := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	addMsg(t, env.opClient, env.campfireID, scrip.TagScripLoanMint, scrip.LoanMintPayload{
		Borrower:  agentAlice,
		Principal: 1000,
		DueAt:     dueAt,
		LoanID:    "loan-vig-cumulative",
	})

	for _, amt := range []int64{20, 30, 15} {
		addMsg(t, env.opClient, env.campfireID, scrip.TagScripLoanVigAccrue, scrip.LoanVigAccruePayload{
			LoanID: "loan-vig-cumulative",
			Amount: amt,
		})
	}

	cs := newStore(t, env)

	rec, ok := cs.GetLoan("loan-vig-cumulative")
	if !ok {
		t.Fatal("GetLoan: not found")
	}
	if rec.Outstanding != 65 {
		t.Errorf("Outstanding = %d, want 65 (20+30+15)", rec.Outstanding)
	}
}

// TestLoanVigAccrue_UnknownLoan_Ignored verifies that a vig-accrue for an
// unknown loan_id is silently ignored.
func TestLoanVigAccrue_UnknownLoan_Ignored(t *testing.T) {
	env := newTestEnv(t)

	addMsg(t, env.opClient, env.campfireID, scrip.TagScripLoanVigAccrue, scrip.LoanVigAccruePayload{
		LoanID: "ghost-loan",
		Amount: 99,
	})

	cs := newStore(t, env)
	// No loan record, no supply change — just silence.
	if got := cs.TotalSupply(); got != 0 {
		t.Errorf("TotalSupply = %d, want 0", got)
	}
}

// TestLoanVigAccrue_RepaidLoan_Ignored verifies that vig does not accrue on a
// fully-repaid loan.
func TestLoanVigAccrue_RepaidLoan_Ignored(t *testing.T) {
	env := newTestEnv(t)

	dueAt := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	addMsg(t, env.opClient, env.campfireID, scrip.TagScripLoanMint, scrip.LoanMintPayload{
		Borrower:  agentAlice,
		Principal: 500,
		DueAt:     dueAt,
		LoanID:    "loan-vig-repaid",
	})
	addMsg(t, env.opClient, env.campfireID, scrip.TagScripLoanRepay, scrip.LoanRepayPayload{
		LoanID: "loan-vig-repaid",
		Amount: 500,
	})
	// Try to accrue vig after repayment — must be ignored.
	addMsg(t, env.opClient, env.campfireID, scrip.TagScripLoanVigAccrue, scrip.LoanVigAccruePayload{
		LoanID: "loan-vig-repaid",
		Amount: 25,
	})

	cs := newStore(t, env)

	rec, ok := cs.GetLoan("loan-vig-repaid")
	if !ok {
		t.Fatal("GetLoan: not found")
	}
	if rec.Outstanding != 0 {
		t.Errorf("Outstanding = %d, want 0 (vig must not accrue on repaid loan)", rec.Outstanding)
	}
}

// TestTotalOutstandingVig_AcrossLoans verifies that TotalOutstandingVig sums
// Outstanding across all active loans and excludes repaid loans.
func TestTotalOutstandingVig_AcrossLoans(t *testing.T) {
	env := newTestEnv(t)

	dueAt := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)

	// Two active loans with vig.
	for _, args := range []struct {
		loanID string
		amt    int64
	}{
		{"loan-vig-sum-a", 100},
		{"loan-vig-sum-b", 200},
	} {
		addMsg(t, env.opClient, env.campfireID, scrip.TagScripLoanMint, scrip.LoanMintPayload{
			Borrower:  agentAlice,
			Principal: 1000,
			DueAt:     dueAt,
			LoanID:    args.loanID,
		})
		addMsg(t, env.opClient, env.campfireID, scrip.TagScripLoanVigAccrue, scrip.LoanVigAccruePayload{
			LoanID: args.loanID,
			Amount: args.amt,
		})
	}

	// One fully-repaid loan with vig that was accrued before repayment.
	addMsg(t, env.opClient, env.campfireID, scrip.TagScripLoanMint, scrip.LoanMintPayload{
		Borrower:  agentBob,
		Principal: 300,
		DueAt:     dueAt,
		LoanID:    "loan-vig-sum-c",
	})
	addMsg(t, env.opClient, env.campfireID, scrip.TagScripLoanVigAccrue, scrip.LoanVigAccruePayload{
		LoanID: "loan-vig-sum-c",
		Amount: 50,
	})
	addMsg(t, env.opClient, env.campfireID, scrip.TagScripLoanRepay, scrip.LoanRepayPayload{
		LoanID: "loan-vig-sum-c",
		Amount: 300,
	})

	cs := newStore(t, env)

	// TotalOutstandingVig must only sum active loans (a + b = 300); repaid loan c excluded.
	if got := cs.TotalOutstandingVig(); got != 300 {
		t.Errorf("TotalOutstandingVig = %d, want 300", got)
	}
}
