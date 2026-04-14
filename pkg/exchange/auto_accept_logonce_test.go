package exchange_test

// auto_accept_logonce_test.go — regression tests for dontguess-405.
//
// Problem: autoAcceptPuts (originally in cmd/dontguess/serve.go) logged a
// "skipping put" line every tick for over-cap puts, producing ~86,400 lines/day.
// Fix: RunAutoAccept maintains a skippedPuts set keyed by put message ID.
// Each over-cap put is logged exactly once per operator lifecycle (skippedPuts
// is pruned when a put leaves the pending snapshot).

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/campfire-net/dontguess/pkg/exchange"
)

// newLogCapture returns an EngineOptions Logger that writes formatted lines to buf.
// Each call writes one line (newline-terminated) so callers can count lines easily.
func newLogCapture(buf *bytes.Buffer) func(format string, args ...any) {
	return func(format string, args ...any) {
		line := fmt.Sprintf(format, args...)
		buf.WriteString(line)
		buf.WriteByte('\n')
	}
}

// countLines counts non-empty lines in buf that contain substr.
func countLines(buf *bytes.Buffer, substr string) int {
	n := 0
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.TrimSpace(line) != "" && strings.Contains(line, substr) {
			n++
		}
	}
	return n
}

// sendOverCapPut sends a put with TokenCost > maxAccept via the test harness and
// returns the message's ID.
func sendOverCapPut(t *testing.T, h *testHarness, desc string, tokenCost int64) string {
	t.Helper()
	msg := h.sendMessage(
		h.seller,
		putPayload(desc, "sha256:"+fmt.Sprintf("%064x", tokenCost), "code", tokenCost, 24576),
		[]string{exchange.TagPut, "exchange:content-type:code", "exchange:domain:go"},
		nil,
	)
	return msg.ID
}

// sendUnderCapPut sends a put with TokenCost <= maxAccept.
func sendUnderCapPut(t *testing.T, h *testHarness, desc string, tokenCost int64) string {
	t.Helper()
	msg := h.sendMessage(
		h.seller,
		putPayload(desc, "sha256:"+fmt.Sprintf("%064x", tokenCost+1000), "code", tokenCost, 24576),
		[]string{exchange.TagPut, "exchange:content-type:code", "exchange:domain:go"},
		nil,
	)
	return msg.ID
}

// replayAll replays all campfire messages into the engine state.
func replayAll(t *testing.T, h *testHarness, eng *exchange.Engine) {
	t.Helper()
	msgs, err := h.st.ListMessages(h.cfID, 0)
	if err != nil {
		t.Fatalf("listing messages: %v", err)
	}
	eng.State().Replay(exchange.FromStoreRecords(msgs))
}

// TestRunAutoAccept_CapZero verifies the zero-boundary case: when max=0, ALL puts
// are over-cap regardless of their token cost. Each should land in PutsHeldForReview
// and produce exactly one "skipping put" log line. No puts should be auto-accepted.
// Regression test for dontguess-166.
func TestRunAutoAccept_CapZero(t *testing.T) {
	t.Parallel()

	const maxAccept = int64(0)

	h := newTestHarness(t)

	var logBuf bytes.Buffer
	eng := h.newEngineWithOpts(func(o *exchange.EngineOptions) {
		o.Logger = newLogCapture(&logBuf)
	})

	// Submit 3 puts with varying token costs — all should be over-cap at max=0.
	sendOverCapPut(t, h, "tiny put", 100)
	sendOverCapPut(t, h, "medium put", 1000)
	sendOverCapPut(t, h, "large put", 1_000_000)
	replayAll(t, h, eng)

	pending := eng.State().PendingPuts()
	if len(pending) != 3 {
		t.Fatalf("expected 3 pending puts, got %d", len(pending))
	}

	skipped := make(map[string]struct{})
	now := time.Now()
	eng.RunAutoAccept(maxAccept, now, skipped)

	// All 3 should be held — none accepted.
	held := eng.State().PutsHeldForReview()
	if len(held) != 3 {
		t.Errorf("expected 3 puts in PutsHeldForReview at cap=0, got %d", len(held))
	}

	// All 3 still pending (no campfire state change).
	pending = eng.State().PendingPuts()
	if len(pending) != 3 {
		t.Errorf("expected all 3 puts still pending after cap=0 tick, got %d", len(pending))
	}

	// No inventory — nothing was accepted.
	inv := eng.State().Inventory()
	if len(inv) != 0 {
		t.Errorf("expected empty inventory at cap=0, got %d entries", len(inv))
	}

	// Exactly 3 "skipping put" log lines — one per put.
	skipLines := countLines(&logBuf, "skipping put")
	if skipLines != 3 {
		t.Errorf("expected 3 'skipping put' log lines at cap=0, got %d\nlog:\n%s",
			skipLines, logBuf.String())
	}
}

// TestRunAutoAccept_EmptyPending verifies that RunAutoAccept is a no-op when there
// are no pending puts: no log lines, no panics, no errors. Regression test for
// dontguess-807.
func TestRunAutoAccept_EmptyPending(t *testing.T) {
	t.Parallel()

	const maxAccept = int64(1_000_000)

	h := newTestHarness(t)

	var logBuf bytes.Buffer
	eng := h.newEngineWithOpts(func(o *exchange.EngineOptions) {
		o.Logger = newLogCapture(&logBuf)
	})

	// Do NOT submit any puts. The pending snapshot is empty.
	skipped := make(map[string]struct{})
	now := time.Now()

	// Run 5 times — should be completely silent.
	for i := 0; i < 5; i++ {
		eng.RunAutoAccept(maxAccept, now, skipped)
	}

	if logBuf.Len() != 0 {
		t.Errorf("expected no log output for empty pending snapshot across 5 ticks, got:\n%s", logBuf.String())
	}
}

// TestAutoAccept_LogOnce is the primary regression test for dontguess-405.
// It verifies that an over-cap put produces exactly one "skipping put" log line
// across N=10 RunAutoAccept calls, not one per call.
//
// IMPORTANT: this test MUST fail against the pre-fix code (it sees N=10 lines
// instead of 1). The failing run was confirmed before the fix was applied.
func TestAutoAccept_LogOnce(t *testing.T) {
	t.Parallel()

	const maxAccept = int64(500)
	const overCapCost = int64(1000) // > maxAccept

	h := newTestHarness(t)

	var logBuf bytes.Buffer
	eng := h.newEngineWithOpts(func(o *exchange.EngineOptions) {
		o.Logger = newLogCapture(&logBuf)
	})

	// Send a single over-cap put and replay into state.
	sendOverCapPut(t, h, "over-cap result", overCapCost)
	replayAll(t, h, eng)

	// The put should be in pending.
	pending := eng.State().PendingPuts()
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending put, got %d", len(pending))
	}

	// Run the auto-accept loop 10 times with the same skippedPuts map.
	skipped := make(map[string]struct{})
	now := time.Now()
	for i := 0; i < 10; i++ {
		eng.RunAutoAccept(maxAccept, now, skipped)
	}

	skipLines := countLines(&logBuf, "skipping put")
	if skipLines != 1 {
		t.Errorf("expected exactly 1 'skipping put' log line across 10 ticks, got %d\nlog output:\n%s",
			skipLines, logBuf.String())
	}
}

// TestAutoAccept_Regression verifies the mixed-bag case:
// 5 over-cap puts produce exactly 5 skip lines total across N=10 ticks,
// and 5 under-cap puts produce exactly 5 auto-accept lines.
func TestAutoAccept_Regression(t *testing.T) {
	t.Parallel()

	const maxAccept = int64(500)

	h := newTestHarness(t)

	var logBuf bytes.Buffer
	eng := h.newEngineWithOpts(func(o *exchange.EngineOptions) {
		o.Logger = newLogCapture(&logBuf)
	})

	// Submit 5 over-cap + 5 under-cap puts.
	for i := 0; i < 5; i++ {
		sendOverCapPut(t, h, fmt.Sprintf("over-cap result %d", i), maxAccept+int64(i+1)*100)
	}
	for i := 0; i < 5; i++ {
		sendUnderCapPut(t, h, fmt.Sprintf("under-cap result %d", i), maxAccept-int64(i+1)*10)
	}
	replayAll(t, h, eng)

	pending := eng.State().PendingPuts()
	if len(pending) != 10 {
		t.Fatalf("expected 10 pending puts, got %d", len(pending))
	}

	skipped := make(map[string]struct{})
	now := time.Now()
	for i := 0; i < 10; i++ {
		eng.RunAutoAccept(maxAccept, now, skipped)
	}

	skipLines := countLines(&logBuf, "skipping put")
	acceptLines := countLines(&logBuf, "auto-accepted put")
	if skipLines != 5 {
		t.Errorf("expected 5 'skipping put' lines (one per over-cap put, logged exactly once), got %d\nlog output:\n%s",
			skipLines, logBuf.String())
	}
	// Under-cap puts are accepted on the first tick they are processed. All 5 should
	// be accepted across the 10 ticks. We assert >= 5 rather than == 5 to avoid
	// brittleness if a future version batches or retries accepts — the invariant is
	// "all under-cap puts eventually accepted," not "exactly one accept line per put."
	// skipLines == 5 (asserted above) already pins the over-cap boundary precisely.
	if acceptLines < 5 {
		t.Errorf("expected at least 5 'auto-accepted put' lines (one per under-cap put), got %d\nlog output:\n%s",
			acceptLines, logBuf.String())
	}
}

// TestAutoAccept_Prune verifies lazy pruning: when an over-cap put is removed
// from the pending snapshot (e.g. accepted with a higher max), its entry is
// removed from skippedPuts, so re-adding the same ID will produce a fresh log line.
func TestAutoAccept_Prune(t *testing.T) {
	t.Parallel()

	const maxAccept = int64(500)
	const overCapCost = int64(1000)

	h := newTestHarness(t)

	var logBuf bytes.Buffer
	eng := h.newEngineWithOpts(func(o *exchange.EngineOptions) {
		o.Logger = newLogCapture(&logBuf)
	})

	// Send one over-cap put and run loop once — should get 1 skip line.
	putID := sendOverCapPut(t, h, "prune-test result", overCapCost)
	replayAll(t, h, eng)

	skipped := make(map[string]struct{})
	now := time.Now()
	eng.RunAutoAccept(maxAccept, now, skipped)

	if _, ok := skipped[putID]; !ok {
		t.Fatalf("expected putID %s in skippedPuts after first tick", putID[:8])
	}
	skipLinesBefore := countLines(&logBuf, "skipping put")
	if skipLinesBefore != 1 {
		t.Fatalf("expected 1 skip line after first tick, got %d", skipLinesBefore)
	}

	// Accept the put with a much higher max so it leaves pending.
	price := overCapCost * 70 / 100
	if err := eng.AutoAcceptPut(putID, price, time.Now().Add(72*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut: %v", err)
	}
	// After accept, PendingPuts should be empty (or not contain this ID).
	pending := eng.State().PendingPuts()
	for _, e := range pending {
		if e.PutMsgID == putID {
			t.Fatalf("put %s still in pending after AutoAcceptPut", putID[:8])
		}
	}

	// Run loop again — prune should remove putID from skippedPuts.
	eng.RunAutoAccept(maxAccept, now, skipped)

	if _, ok := skipped[putID]; ok {
		t.Errorf("expected putID %s pruned from skippedPuts after it left pending", putID[:8])
	}

	// Now re-add the same ID to pending by sending it again (simulate a fresh put
	// with the same logical identity — actually just confirm skipped no longer blocks).
	// Re-run with a fresh put to confirm the map no longer suppresses logging.
	logBuf.Reset()
	putID2 := sendOverCapPut(t, h, "prune-test result 2", overCapCost)
	// Force replay to pick up new put.
	msgs, err := h.st.ListMessages(h.cfID, 0)
	if err != nil {
		t.Fatalf("listing messages: %v", err)
	}
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	eng.RunAutoAccept(maxAccept, now, skipped)
	if _, ok := skipped[putID2]; !ok {
		t.Errorf("expected new putID2 %s added to skippedPuts on first skip", putID2[:8])
	}
	skipLinesAfter := countLines(&logBuf, "skipping put")
	if skipLinesAfter != 1 {
		t.Errorf("expected 1 skip line for new put after prune, got %d\nlog output:\n%s",
			skipLinesAfter, logBuf.String())
	}
}
