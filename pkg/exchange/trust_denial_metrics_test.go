package exchange_test

// Tests for the dispatch trust-gate degradation counters (dontguess-388,
// docs/design/relay-transport.md §2.4a D4 + §3 "provenance_rejected", build
// outcome 9). Before this item, the trust-denial path in dispatch()
// (engine_core.go) logged one line and returned nil with no metric — a
// security-relevant rejection with no counted alarm. These tests assert that:
//
//  1. A non-allowlisted sender's put increments TrustDenialNotAllowlisted.
//  2. A non-operator sender's match increments TrustDenialNotOperator.
//  3. A below-reputation-floor allowlisted seller's put increments
//     TrustDenialLowReputation.
//  4. Each rejection reason lands in a DISTINCT counter — never collapsed.
//  5. dispatch() still returns nil (silent-to-the-poll-loop reject) and the
//     rejected message never mutates state — the counter is additive
//     observability, not a behavior change.
//
// Real engine, real TrustChecker, real store — no mocks (matches the existing
// trust_dispatch_test.go convention in this package).

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/campfire-net/campfire/cf-protocol/store"

	"github.com/campfire-net/dontguess/pkg/exchange"
)

// injectMsg writes a raw record with the given tag/sender directly into the
// harness store, bypassing message signing. dispatch()'s trust gate runs
// BEFORE any op-specific payload parsing, so a minimal placeholder payload is
// sufficient for operations with no real Apply-time side effect on an empty
// payload (used here only for the operator-only exchange:match probe;
// exchange:put probes use the real injectPutMsg from trust_dispatch_test.go
// so State.Replay's applyPut sees a realistic payload).
func injectMsg(t *testing.T, h *testHarness, tag, senderKey string) *store.MessageRecord {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{})
	rec := store.MessageRecord{
		ID:         fmt.Sprintf("denial-test-%s-%s-%d", tag, senderKey, time.Now().UnixNano()),
		CampfireID: h.cfID,
		Sender:     senderKey,
		Payload:    payload,
		Tags:       []string{tag},
		Timestamp:  time.Now().UnixNano(),
		ReceivedAt: time.Now().UnixNano(),
		Signature:  []byte{0x00},
	}
	if _, err := h.st.AddMessage(rec); err != nil {
		t.Fatalf("AddMessage tag=%s: %v", tag, err)
	}
	return &rec
}

// TestTrustDenial_NotAllowlisted_CountsAndAlarms: a non-allowlisted sender's
// put is trust-rejected and increments exactly TrustDenialNotAllowlisted.
func TestTrustDenial_NotAllowlisted_CountsAndAlarms(t *testing.T) {
	t.Parallel()
	h, eng := newEngineWithTrust(t, dispatchTrustChecker(t))

	before := eng.DegradationSnapshot()

	rec := injectPutMsg(t, h, keyAnon)
	msgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	if err := eng.DispatchForTest(exchange.FromStoreRecord(rec)); err != nil {
		t.Errorf("dispatch returned error, want nil (silent-to-poll-loop reject): %v", err)
	}

	after := eng.DegradationSnapshot()
	if got := after.TrustDenialNotAllowlisted - before.TrustDenialNotAllowlisted; got != 1 {
		t.Errorf("TrustDenialNotAllowlisted delta = %d, want 1", got)
	}
	if after.TrustDenialNotOperator != before.TrustDenialNotOperator {
		t.Errorf("TrustDenialNotOperator changed, want unchanged (reason must not cross-bucket)")
	}
	if after.TrustDenialLowReputation != before.TrustDenialLowReputation {
		t.Errorf("TrustDenialLowReputation changed, want unchanged (reason must not cross-bucket)")
	}

	// The rejected put must never enter inventory — the counter is additive
	// observability, not a change to the reject behavior.
	if inv := eng.State().Inventory(); len(inv) != 0 {
		t.Errorf("inventory should be empty after rejected put, got %d entries", len(inv))
	}
}

// TestTrustDenial_NotOperator_CountsAndAlarms: an allowlisted (non-operator)
// sender forging an operator-only exchange:match is trust-rejected and
// increments exactly TrustDenialNotOperator — a DISTINCT bucket from
// not-allowlisted, since the sender here IS allowlisted, just not the
// operator.
func TestTrustDenial_NotOperator_CountsAndAlarms(t *testing.T) {
	t.Parallel()
	h, eng := newEngineWithTrust(t, dispatchTrustChecker(t))

	before := eng.DegradationSnapshot()

	rec := injectMsg(t, h, exchange.TagMatch, keyAllowlisted)
	msgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	if err := eng.DispatchForTest(exchange.FromStoreRecord(rec)); err != nil {
		t.Errorf("dispatch returned error, want nil (silent-to-poll-loop reject): %v", err)
	}

	after := eng.DegradationSnapshot()
	if got := after.TrustDenialNotOperator - before.TrustDenialNotOperator; got != 1 {
		t.Errorf("TrustDenialNotOperator delta = %d, want 1", got)
	}
	if after.TrustDenialNotAllowlisted != before.TrustDenialNotAllowlisted {
		t.Errorf("TrustDenialNotAllowlisted changed, want unchanged (reason must not cross-bucket)")
	}
}

// TestTrustDenial_LowReputation_CountsAndAlarms: an allowlisted seller whose
// behavioral reputation is below the configured floor is trust-rejected on
// put and increments exactly TrustDenialLowReputation — a DISTINCT bucket
// from not-allowlisted (the sender here clears the allowlist check; only the
// reputation floor rejects them).
func TestTrustDenial_LowReputation_CountsAndAlarms(t *testing.T) {
	t.Parallel()
	fleet := exchange.NewKeySet(keyAllowlisted)
	checker, err := exchange.NewTrustChecker(keyOperator, fleet,
		exchange.WithReputationFloor(func(key string) int { return 0 }, 50))
	if err != nil {
		t.Fatalf("NewTrustChecker: %v", err)
	}
	h, eng := newEngineWithTrust(t, checker)

	before := eng.DegradationSnapshot()

	rec := injectPutMsg(t, h, keyAllowlisted)
	msgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	if err := eng.DispatchForTest(exchange.FromStoreRecord(rec)); err != nil {
		t.Errorf("dispatch returned error, want nil (silent-to-poll-loop reject): %v", err)
	}

	after := eng.DegradationSnapshot()
	if got := after.TrustDenialLowReputation - before.TrustDenialLowReputation; got != 1 {
		t.Errorf("TrustDenialLowReputation delta = %d, want 1", got)
	}
	if after.TrustDenialNotAllowlisted != before.TrustDenialNotAllowlisted {
		t.Errorf("TrustDenialNotAllowlisted changed, want unchanged (allowlisted sender must not bucket as not-allowlisted)")
	}
	if after.TrustDenialNotOperator != before.TrustDenialNotOperator {
		t.Errorf("TrustDenialNotOperator changed, want unchanged")
	}

	if inv := eng.State().Inventory(); len(inv) != 0 {
		t.Errorf("inventory should be empty after low-reputation-rejected put, got %d entries", len(inv))
	}
}

// TestTrustDenial_AllowedOp_NoCounterIncrement: a trust-CHECK that passes
// must not increment any degradation counter — the counters track rejections
// only.
func TestTrustDenial_AllowedOp_NoCounterIncrement(t *testing.T) {
	t.Parallel()
	h, eng := newEngineWithTrust(t, dispatchTrustChecker(t))

	before := eng.DegradationSnapshot()

	rec := injectMsg(t, h, exchange.TagPut, keyAllowlisted)
	msgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	if err := eng.DispatchForTest(exchange.FromStoreRecord(rec)); err != nil {
		t.Errorf("dispatch returned error for allowlisted put, want nil: %v", err)
	}

	after := eng.DegradationSnapshot()
	if after != before {
		t.Errorf("degradation counters changed on an ALLOWED op: before=%+v after=%+v", before, after)
	}
}
