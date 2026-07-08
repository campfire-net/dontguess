package main

// status_degradation_test.go — feature tests for dontguess-388.
//
// Before this item, the dispatch trust-gate rejection path in
// pkg/exchange/engine_core.go logged one line and returned nil with no
// metric — a security-relevant rejection with no counted alarm. These tests
// verify the full chain end to end:
//
//  1. readDegradationMetrics (status.go client) correctly decodes a canned
//     OpMetrics socket response (TestStatus_SocketMetrics — wire-format
//     contract, mirrors TestStatus_SocketHeldCount).
//  2. The real operator socket handler (serve.go's OpMetrics case) returns a
//     real Engine's DegradationSnapshot as JSON (TestOperatorSocket_Metrics).
//  3. A REAL trust-denied probe — a non-allowlisted put run through the
//     engine's actual poll-loop dispatch, no DispatchForTest shortcut —
//     increments the counter, and it is visible over the socket
//     (TestOperatorSocket_Metrics_TrustDenialProbe). This is the exact DONE
//     condition: "a forged-op / trust-denied probe increments the
//     corresponding counter and it appears in status."
//
// Real net.Listen/net.Dial, real exchange.Engine, real TrustChecker, real
// campfire-backed store. No mocks.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/campfire-net/campfire/cf-protocol/protocol"
	"github.com/campfire-net/campfire/cf-protocol/store"
	"github.com/campfire-net/dontguess/pkg/exchange"
)

// --------------------------------------------------------------------------
// TestStatus_SocketMetrics
// --------------------------------------------------------------------------

// TestStatus_SocketMetrics starts a minimal unix socket server that mimics
// the operator's OpMetrics response, and asserts readDegradationMetrics
// decodes it correctly. Mirrors TestStatus_SocketHeldCount's pattern.
func TestStatus_SocketMetrics(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	sockDir := filepath.Join(dir, "ipc")
	if err := os.MkdirAll(sockDir, 0700); err != nil {
		t.Fatalf("mkdir ipc: %v", err)
	}
	sockPath := filepath.Join(sockDir, "dontguess.sock")

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		var req map[string]any
		json.NewDecoder(conn).Decode(&req) //nolint:errcheck
		json.NewEncoder(conn).Encode(map[string]any{ //nolint:errcheck
			"degradation": map[string]any{
				"trust_denial_not_allowlisted": 3,
				"trust_denial_not_operator":    1,
				"trust_denial_low_reputation":  0,
				"trust_denial_other":           0,
			},
		})
	}()

	got, note := readDegradationMetrics(dir)
	<-done

	if note != "" {
		t.Errorf("unexpected note %q, want empty (socket reachable)", note)
	}
	if got == nil {
		t.Fatal("degradation is nil, want non-nil")
	}
	if got.TrustDenialNotAllowlisted != 3 {
		t.Errorf("TrustDenialNotAllowlisted = %d, want 3", got.TrustDenialNotAllowlisted)
	}
	if got.TrustDenialNotOperator != 1 {
		t.Errorf("TrustDenialNotOperator = %d, want 1", got.TrustDenialNotOperator)
	}
}

// TestStatus_SocketMetrics_Unreachable verifies the degrade-gracefully path:
// no socket present → nil + a non-empty note, matching readHeldCount's
// contract so `status` still renders when the operator isn't running.
func TestStatus_SocketMetrics_Unreachable(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	got, note := readDegradationMetrics(dir)
	if got != nil {
		t.Errorf("degradation = %+v, want nil (socket not present)", *got)
	}
	if note == "" {
		t.Error("note is empty, want non-empty error note")
	}
}

// --------------------------------------------------------------------------
// TestOperatorSocket_Metrics
// --------------------------------------------------------------------------

// TestOperatorSocket_Metrics verifies the real OpMetrics socket handler
// returns a real Engine's (zero-valued, nothing rejected yet) degradation
// counters — proving the wiring (ipc.go constant → serve.go case →
// eng.DegradationSnapshot() → JSON) is correct, independent of whether any
// rejection has occurred.
func TestOperatorSocket_Metrics(t *testing.T) {
	t.Parallel()

	h := newOpTestHarness(t)
	eng := h.newEngine()

	sockPath, _ := startSocketServer(t, eng)

	var resp struct {
		Degradation exchange.DegradationCounts `json:"degradation"`
	}
	dialAndRequest(t, sockPath, map[string]any{"op": OpMetrics}, &resp)

	if resp.Degradation != (exchange.DegradationCounts{}) {
		t.Errorf("degradation = %+v, want zero value (no TrustChecker configured, nothing rejected)", resp.Degradation)
	}
}

// TestOperatorSocket_Metrics_TrustDenialProbe is the DONE-condition test: a
// forged/trust-denied probe run through the REAL engine poll-loop dispatch
// (eng.Start, not DispatchForTest) increments the counter, and the socket
// surfaces it — "a forged-op / trust-denied probe increments the
// corresponding counter and it appears in status."
func TestOperatorSocket_Metrics_TrustDenialProbe(t *testing.T) {
	t.Parallel()

	h := newOpTestHarness(t)

	// h.seller is NOT on the fleet allowlist — only the operator identity is.
	operatorKey := h.operatorClient.PublicKeyHex()
	checker, err := exchange.NewTrustChecker(operatorKey, exchange.NewKeySet())
	if err != nil {
		t.Fatalf("NewTrustChecker: %v", err)
	}

	eng := exchange.NewEngine(exchange.EngineOptions{
		CampfireID:   h.cfID,
		Store:        h.st,
		ReadClient:   protocol.New(h.st, nil),
		WriteClient:  h.operatorClient,
		ReadSkipSync: true,
		TrustChecker: checker,
		PollInterval: 20 * time.Millisecond,
		Logger: func(format string, args ...any) {
			t.Logf("[engine] "+format, args...)
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	engDone := make(chan struct{})
	go func() {
		defer close(engDone)
		_ = eng.Start(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-engDone
	})

	sockPath, _ := startSocketServer(t, eng)

	// Sanity: no rejection has happened yet.
	var before struct {
		Degradation exchange.DegradationCounts `json:"degradation"`
	}
	dialAndRequest(t, sockPath, map[string]any{"op": OpMetrics}, &before)
	if before.Degradation.TrustDenialNotAllowlisted != 0 {
		t.Fatalf("precondition failed: TrustDenialNotAllowlisted = %d before probe, want 0", before.Degradation.TrustDenialNotAllowlisted)
	}

	// Inject a raw put message from the non-allowlisted seller directly into
	// the store — the real poll loop (not DispatchForTest) will pick it up,
	// run it through the trust gate, and reject it.
	idBytes := make([]byte, 32)
	if _, err := rand.Read(idBytes); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	payload, _ := json.Marshal(map[string]any{
		"description":  "trust-denial probe",
		"content":      "cGxhY2Vob2xkZXI=",
		"token_cost":   int64(1000),
		"content_type": "exchange:content-type:text",
		"domains":      []string{"test"},
	})
	rec := store.MessageRecord{
		ID:          hex.EncodeToString(idBytes),
		CampfireID:  h.cfID,
		Sender:      h.seller,
		Payload:     payload,
		Tags:        []string{exchange.TagPut, "exchange:content-type:text"},
		Antecedents: []string{},
		Timestamp:   store.NowNano(),
		Signature:   []byte{},
	}
	if _, err := h.st.AddMessage(rec); err != nil {
		t.Fatalf("AddMessage: %v", err)
	}

	// Poll the socket until the poll loop has dispatched the rejection (or
	// time out). PollInterval is 20ms; allow generous headroom for CI.
	deadline := time.Now().Add(5 * time.Second)
	var after struct {
		Degradation exchange.DegradationCounts `json:"degradation"`
	}
	for time.Now().Before(deadline) {
		dialAndRequest(t, sockPath, map[string]any{"op": OpMetrics}, &after)
		if after.Degradation.TrustDenialNotAllowlisted > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if after.Degradation.TrustDenialNotAllowlisted != 1 {
		t.Errorf("TrustDenialNotAllowlisted = %d, want 1 (real poll-loop trust denial, surfaced over the operator socket)",
			after.Degradation.TrustDenialNotAllowlisted)
	}

	// The rejected put must never have entered inventory.
	if inv := eng.State().Inventory(); len(inv) != 0 {
		t.Errorf("inventory should be empty after rejected put, got %d entries", len(inv))
	}
}
