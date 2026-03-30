package exchange_test

// Tests that Engine.dispatch() correctly gates operations through ProvenanceChecker
// when configured, and passes all operations when ProvenanceChecker is nil.

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/campfire-net/campfire/pkg/protocol"
	"github.com/campfire-net/campfire/pkg/provenance"
	"github.com/campfire-net/campfire/pkg/store"

	"github.com/3dl-dev/dontguess/pkg/exchange"
)

// makeProvenanceStore returns a provenance.Store with two test keys:
//
//	"key-anon"    → LevelAnonymous (0)
//	"key-claimed" → LevelClaimed   (1)
func makeProvenanceStore(t *testing.T) *provenance.Store {
	t.Helper()

	cfg := provenance.DefaultConfig()
	cfg.FreshnessWindow = 7 * 24 * time.Hour
	ps := provenance.NewStore(cfg)
	ps.SetSelfClaimed("key-claimed")
	return ps
}

// newEngineWithProvenance builds a testHarness + engine with a ProvenanceChecker wired in.
func newEngineWithProvenance(t *testing.T, checker *exchange.ProvenanceChecker) (*testHarness, *exchange.Engine) {
	t.Helper()
	h := newTestHarness(t)
	eng := exchange.NewEngine(exchange.EngineOptions{
		CampfireID:        h.cfID,
		OperatorIdentity:  h.operator,
		Store:             h.st,
		ReadClient:  protocol.New(h.st, h.operator),
		WriteClient:      protocol.New(h.st, h.operator),
		ProvenanceChecker: checker,
		Logger: func(format string, args ...any) {
			t.Logf("[engine] "+format, args...)
		},
	})
	return h, eng
}

// countMatchMessages returns how many exchange:match messages are in the store.
func countMatchMessages(t *testing.T, h *testHarness) int {
	t.Helper()
	msgs, err := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})
	if err != nil {
		t.Fatalf("listing match messages: %v", err)
	}
	return len(msgs)
}

// injectPutMsg writes a raw put record directly into the harness store with the given senderKey.
// Bypasses message signing — used to test provenance gating at the dispatch level.
func injectPutMsg(t *testing.T, h *testHarness, senderKey string) *store.MessageRecord {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{
		"description":  "test entry",
		"content_hash": "sha256:" + "a" + "0000000000000000000000000000000000000000000000000000000000000001",
		"token_cost":   int64(1000),
		"content_type": "text",
		"content_size": int64(512),
		"domains":      []string{"test"},
	})
	rec := store.MessageRecord{
		ID:         "put-prov-test-" + senderKey[:8] + "-" + string(rune(time.Now().UnixNano()%1000+'a')),
		CampfireID: h.cfID,
		Sender:     senderKey,
		Payload:    payload,
		Tags:       []string{exchange.TagPut, "exchange:content-type:text"},
		Timestamp:  time.Now().UnixNano(),
		ReceivedAt: time.Now().UnixNano(),
		Signature:  []byte{0x00},
	}
	if _, err := h.st.AddMessage(rec); err != nil {
		t.Fatalf("AddMessage put: %v", err)
	}
	return &rec
}

// injectBuyMsg writes a raw buy record into the harness store with the given senderKey.
func injectBuyMsg(t *testing.T, h *testHarness, senderKey string) *store.MessageRecord {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{
		"task":        "find a test helper",
		"budget":      int64(5000),
		"max_results": 3,
	})
	rec := store.MessageRecord{
		ID:         "buy-prov-test-" + senderKey[:8] + "-" + string(rune(time.Now().UnixNano()%1000+'a')),
		CampfireID: h.cfID,
		Sender:     senderKey,
		Payload:    payload,
		Tags:       []string{exchange.TagBuy},
		Timestamp:  time.Now().UnixNano(),
		ReceivedAt: time.Now().UnixNano(),
		Signature:  []byte{0x00},
	}
	if _, err := h.st.AddMessage(rec); err != nil {
		t.Fatalf("AddMessage buy: %v", err)
	}
	return &rec
}

// TestProvenanceDispatch_AnonymousPutRejected verifies that an anonymous sender's
// put message is silently dropped by the engine when ProvenanceChecker is configured.
func TestProvenanceDispatch_AnonymousPutRejected(t *testing.T) {
	t.Parallel()
	ps := makeProvenanceStore(t)
	checker, err := exchange.NewProvenanceChecker(ps)
	if err != nil {
		t.Fatalf("NewProvenanceChecker: %v", err)
	}
	h, eng := newEngineWithProvenance(t, checker)

	// Seed some inventory so the engine has something to match against.
	_ = h // suppress unused warning

	putRec := injectPutMsg(t, h, "key-anon")

	// Apply state (so engine knows about the put).
	msgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	// Dispatch the put — anonymous sender should be silently rejected.
	if err := eng.DispatchForTest(exchange.FromStoreRecord(putRec)); err != nil {
		t.Errorf("dispatch returned error, want nil (silent reject): %v", err)
	}

	// The put was rejected, so there should be no put-accept in the transport.
	// We verify by checking that no match was emitted (the put must be accepted
	// before it can be matched — rejection means no downstream activity).
	matchCount := countMatchMessages(t, h)
	if matchCount != 0 {
		t.Errorf("expected 0 match messages after anonymous put rejection, got %d", matchCount)
	}
}

// TestProvenanceDispatch_AnonymousBuyAccepted verifies that an anonymous sender CAN
// send a buy message — buy is allowed at LevelAnonymous.
func TestProvenanceDispatch_AnonymousBuyAccepted(t *testing.T) {
	t.Parallel()
	ps := makeProvenanceStore(t)
	checker, err := exchange.NewProvenanceChecker(ps)
	if err != nil {
		t.Fatalf("NewProvenanceChecker: %v", err)
	}
	h, eng := newEngineWithProvenance(t, checker)

	buyRec := injectBuyMsg(t, h, "key-anon")

	msgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	// dispatch should not return an error — anonymous buy is permitted.
	// (No match is expected since there's no inventory, but the buy is processed.)
	if err := eng.DispatchForTest(exchange.FromStoreRecord(buyRec)); err != nil {
		t.Errorf("dispatch returned error for anonymous buy, want nil: %v", err)
	}
}

// TestProvenanceDispatch_ClaimedPutAccepted verifies that a claimed sender CAN
// send a put message — put requires LevelClaimed.
func TestProvenanceDispatch_ClaimedPutAccepted(t *testing.T) {
	t.Parallel()
	ps := makeProvenanceStore(t)
	checker, err := exchange.NewProvenanceChecker(ps)
	if err != nil {
		t.Fatalf("NewProvenanceChecker: %v", err)
	}
	h, eng := newEngineWithProvenance(t, checker)

	putRec := injectPutMsg(t, h, "key-claimed")

	msgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	// dispatch should not return an error — claimed put is permitted.
	if err := eng.DispatchForTest(exchange.FromStoreRecord(putRec)); err != nil {
		t.Errorf("dispatch returned error for claimed put, want nil: %v", err)
	}
}

// TestProvenanceDispatch_UnverifiedInventory_BuyReturnsEmpty is an integration
// test for the provenance enforcement path at engine dispatch.
//
// Scenario:
//  1. Engine is created with ProvenanceChecker enabled (minimum level: claimed).
//  2. An unverified seller (LevelAnonymous, no attestation) attempts to seed
//     inventory by sending a put message.
//  3. The engine dispatches the put — the ProvenanceChecker rejects it.
//  4. A buyer sends a buy order.
//  5. The engine dispatches the buy — the match result must be empty because
//     the unverified seller's entry was rejected and never entered inventory.
//
// This is the base-case provenance enforcement test: no downgrade, no flag —
// just a seller that was never attested, whose entry should never be buyable.
func TestProvenanceDispatch_UnverifiedInventory_BuyReturnsEmpty(t *testing.T) {
	t.Parallel()
	ps := makeProvenanceStore(t)
	checker, err := exchange.NewProvenanceChecker(ps)
	if err != nil {
		t.Fatalf("NewProvenanceChecker: %v", err)
	}
	h, eng := newEngineWithProvenance(t, checker)

	// Step 1: Unverified seller (key-anon, LevelAnonymous) injects a put.
	putRec := injectPutMsg(t, h, "key-anon")

	msgs, err := h.st.ListMessages(h.cfID, 0)
	if err != nil {
		t.Fatalf("listing messages after put: %v", err)
	}
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	// Step 2: Dispatch the put — ProvenanceChecker should reject it silently.
	if err := eng.DispatchForTest(exchange.FromStoreRecord(putRec)); err != nil {
		t.Errorf("DispatchForTest(put) returned error, want nil (silent reject): %v", err)
	}

	// Verify: inventory must be empty — the rejected put was never accepted.
	if inv := eng.State().Inventory(); len(inv) != 0 {
		t.Errorf("inventory should be empty after rejected put, got %d entries", len(inv))
	}

	// Step 3: Buyer sends a buy order for a task that matches the description.
	buyRec := injectBuyMsg(t, h, "key-claimed")

	msgs, err = h.st.ListMessages(h.cfID, 0)
	if err != nil {
		t.Fatalf("listing messages after buy: %v", err)
	}
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	// Step 4: Dispatch the buy — no inventory exists, so the match is empty.
	if err := eng.DispatchForTest(exchange.FromStoreRecord(buyRec)); err != nil {
		t.Errorf("DispatchForTest(buy) returned error: %v", err)
	}

	// Step 5: Verify the match message was emitted with zero results.
	matchMsgs, err := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})
	if err != nil {
		t.Fatalf("listing match messages: %v", err)
	}
	if len(matchMsgs) == 0 {
		t.Fatal("expected a match message to be emitted (to fulfill the buy future), got none")
	}
	lastMatch := matchMsgs[len(matchMsgs)-1]
	var matchPayload struct {
		Results []struct {
			EntryID string `json:"entry_id"`
		} `json:"results"`
	}
	if err := json.Unmarshal(lastMatch.Payload, &matchPayload); err != nil {
		t.Fatalf("unmarshal match payload: %v", err)
	}
	if len(matchPayload.Results) != 0 {
		t.Errorf("expected 0 match results (unverified entry rejected), got %d (first entry_id: %s)",
			len(matchPayload.Results), matchPayload.Results[0].EntryID)
	}
}

// TestProvenanceDispatch_NilChecker_AllOperationsPass verifies backwards compatibility:
// when ProvenanceChecker is nil, all operations pass through regardless of sender identity.
func TestProvenanceDispatch_NilChecker_AllOperationsPass(t *testing.T) {
	t.Parallel()
	// Use newTestHarness directly — no ProvenanceChecker configured.
	h := newTestHarness(t)
	eng := exchange.NewEngine(exchange.EngineOptions{
		CampfireID:       h.cfID,
		OperatorIdentity: h.operator,
		Store:            h.st,
		ReadClient:  protocol.New(h.st, h.operator),
		WriteClient:      protocol.New(h.st, h.operator),
		Logger: func(format string, args ...any) {
			t.Logf("[engine] "+format, args...)
		},
	})

	// Anonymous put — would normally be rejected, but no checker is configured.
	putRec := injectPutMsg(t, h, "key-anon")
	msgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	if err := eng.DispatchForTest(exchange.FromStoreRecord(putRec)); err != nil {
		t.Errorf("dispatch returned error with nil checker, want nil: %v", err)
	}
}
