package exchange_test

// Tests that Engine.dispatch() gates operations through TrustChecker when
// configured, and passes all operations when TrustChecker is nil. Re-expressed
// from the former provenance_dispatch_test.go against the allowlist primitive:
// "anonymous" is now "not on the fleet allowlist", "claimed put" is now
// "allowlisted put". The dispatch-level outcomes are identical — an
// unallowlisted put never enters inventory; an unallowlisted buy is served.

import (
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/campfire-net/dontguess/pkg/store"

	"github.com/campfire-net/dontguess/pkg/exchange"
)

// newEngineWithTrust builds a testHarness + engine with a TrustChecker wired in.
func newEngineWithTrust(t *testing.T, checker *exchange.TrustChecker) (*testHarness, *exchange.Engine) {
	t.Helper()
	h := newTestHarness(t)
	eng := exchange.NewEngine(exchange.EngineOptions{
		CampfireID:        h.cfID,
		LocalStore:        h.st,
		OperatorPublicKey: h.operator.pubKeyHex,
		TrustChecker:      checker,
		Logger: func(format string, args ...any) {
			t.Logf("[engine] "+format, args...)
		},
	})
	return h, eng
}

// dispatchTrustChecker returns a checker where keyAllowlisted is a fleet member
// and keyAnon is not. No reputation floor.
func dispatchTrustChecker(t *testing.T) *exchange.TrustChecker {
	t.Helper()
	c, err := exchange.NewTrustChecker(keyOperator, exchange.NewKeySet(keyAllowlisted))
	if err != nil {
		t.Fatalf("NewTrustChecker: %v", err)
	}
	return c
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

// injectPutMsg writes a raw put record directly into the harness store with the
// given senderKey. Bypasses message signing — used to test trust gating at the
// dispatch level.
func injectPutMsg(t *testing.T, h *testHarness, senderKey string) *store.MessageRecord {
	t.Helper()
	contentBytes := []byte("test entry content bytes for trust testing")
	contentB64 := base64.StdEncoding.EncodeToString(contentBytes)
	payload, _ := json.Marshal(map[string]any{
		"description":  "test entry",
		"content":      contentB64,
		"token_cost":   int64(1000),
		"content_type": "text",
		"domains":      []string{"test"},
	})
	rec := store.MessageRecord{
		ID:         "put-trust-test-" + senderKey + "-" + string(rune(time.Now().UnixNano()%1000+'a')),
		CampfireID: h.cfID,
		Sender:     senderKey,
		Payload:    payload,
		Tags:       []string{exchange.TagPut, "exchange:content-type:text"},
		Timestamp:  time.Now().UnixNano(),
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
		ID:         "buy-trust-test-" + senderKey + "-" + string(rune(time.Now().UnixNano()%1000+'a')),
		CampfireID: h.cfID,
		Sender:     senderKey,
		Payload:    payload,
		Tags:       []string{exchange.TagBuy},
		Timestamp:  time.Now().UnixNano(),
	}
	if _, err := h.st.AddMessage(rec); err != nil {
		t.Fatalf("AddMessage buy: %v", err)
	}
	return &rec
}

// TestTrustDispatch_AnonymousPutRejected: a non-allowlisted sender's put is
// silently dropped by the engine when TrustChecker is configured.
func TestTrustDispatch_AnonymousPutRejected(t *testing.T) {
	t.Parallel()
	h, eng := newEngineWithTrust(t, dispatchTrustChecker(t))

	putRec := injectPutMsg(t, h, keyAnon)
	msgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	if err := eng.DispatchForTest(exchange.FromStoreRecord(putRec)); err != nil {
		t.Errorf("dispatch returned error, want nil (silent reject): %v", err)
	}

	if matchCount := countMatchMessages(t, h); matchCount != 0 {
		t.Errorf("expected 0 match messages after anonymous put rejection, got %d", matchCount)
	}
}

// TestTrustDispatch_AnonymousBuyAccepted: a non-allowlisted sender CAN buy.
func TestTrustDispatch_AnonymousBuyAccepted(t *testing.T) {
	t.Parallel()
	h, eng := newEngineWithTrust(t, dispatchTrustChecker(t))

	buyRec := injectBuyMsg(t, h, keyAnon)
	msgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	if err := eng.DispatchForTest(exchange.FromStoreRecord(buyRec)); err != nil {
		t.Errorf("dispatch returned error for anonymous buy, want nil: %v", err)
	}
}

// TestTrustDispatch_AllowlistedPutAccepted: an allowlisted fleet member CAN put.
func TestTrustDispatch_AllowlistedPutAccepted(t *testing.T) {
	t.Parallel()
	h, eng := newEngineWithTrust(t, dispatchTrustChecker(t))

	putRec := injectPutMsg(t, h, keyAllowlisted)
	msgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	if err := eng.DispatchForTest(exchange.FromStoreRecord(putRec)); err != nil {
		t.Errorf("dispatch returned error for allowlisted put, want nil: %v", err)
	}
}

// TestTrustDispatch_UnverifiedInventory_BuyReturnsEmpty: an unallowlisted seller's
// put is rejected and never enters inventory, so a subsequent buy matches nothing.
func TestTrustDispatch_UnverifiedInventory_BuyReturnsEmpty(t *testing.T) {
	t.Parallel()
	h, eng := newEngineWithTrust(t, dispatchTrustChecker(t))

	// Step 1: unallowlisted seller injects a put.
	putRec := injectPutMsg(t, h, keyAnon)
	msgs, err := h.st.ListMessages(h.cfID, 0)
	if err != nil {
		t.Fatalf("listing messages after put: %v", err)
	}
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	// Step 2: dispatch the put — TrustChecker rejects it silently.
	if err := eng.DispatchForTest(exchange.FromStoreRecord(putRec)); err != nil {
		t.Errorf("DispatchForTest(put) returned error, want nil (silent reject): %v", err)
	}

	// Inventory must be empty — the rejected put was never accepted.
	if inv := eng.State().Inventory(); len(inv) != 0 {
		t.Errorf("inventory should be empty after rejected put, got %d entries", len(inv))
	}

	// Step 3: an allowlisted buyer sends a buy order.
	buyRec := injectBuyMsg(t, h, keyAllowlisted)
	msgs, err = h.st.ListMessages(h.cfID, 0)
	if err != nil {
		t.Fatalf("listing messages after buy: %v", err)
	}
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	// Step 4: dispatch the buy — no inventory exists, so the match is empty.
	if err := eng.DispatchForTest(exchange.FromStoreRecord(buyRec)); err != nil {
		t.Errorf("DispatchForTest(buy) returned error: %v", err)
	}

	// Step 5: verify the match message was emitted with zero results.
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

// TestTrustDispatch_NilChecker_AllOperationsPass: when TrustChecker is nil, all
// operations pass through regardless of sender identity (backwards compat).
func TestTrustDispatch_NilChecker_AllOperationsPass(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := exchange.NewEngine(exchange.EngineOptions{
		CampfireID:        h.cfID,
		LocalStore:        h.st,
		OperatorPublicKey: h.operator.pubKeyHex,
		Logger: func(format string, args ...any) {
			t.Logf("[engine] "+format, args...)
		},
	})

	putRec := injectPutMsg(t, h, keyAnon)
	msgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	if err := eng.DispatchForTest(exchange.FromStoreRecord(putRec)); err != nil {
		t.Errorf("dispatch returned error with nil checker, want nil: %v", err)
	}
}
