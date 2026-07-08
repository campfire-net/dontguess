package exchange

// Tests for dontguess-4127 (build-outcome 8, docs/design/relay-transport.md
// §E MUST-ENFORCE(1)): locally-emitted operator message timestamps must be
// STRICTLY non-decreasing across successive emissions on one Engine, so
// (Timestamp,ID) canonical batch order (Sequencer.SequenceForFold) cannot be
// scrambled by a nanosecond-granularity wall-clock tie or a backward NTP
// step between two of the operator's own totally-ordered emissions.
//
// White-box (package exchange, not exchange_test) so the unit test can call
// the unexported nextMonotonicTimestamp directly and force a deterministic
// clock regression — no goroutines, no timing, no flakiness.

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	dgstore "github.com/campfire-net/dontguess/pkg/store"
)

// TestNextMonotonicTimestamp_StrictlyIncreasing proves repeated calls never
// produce an equal or decreasing value, even under a tight loop where two
// calls landing in the same wall-clock nanosecond is plausible.
func TestNextMonotonicTimestamp_StrictlyIncreasing(t *testing.T) {
	eng := &Engine{}
	prev := eng.nextMonotonicTimestamp()
	for i := 0; i < 20000; i++ {
		next := eng.nextMonotonicTimestamp()
		if next <= prev {
			t.Fatalf("non-monotonic at iteration %d: prev=%d next=%d", i, prev, next)
		}
		prev = next
	}
}

// TestNextMonotonicTimestamp_SurvivesClockRegression deterministically forces
// the scenario a real wall-clock backward NTP step would cause: the engine's
// recorded last-emitted timestamp is ahead of the real clock. Every
// subsequent call must still advance strictly past the last emitted value,
// never falling back to (a now-stale) real time.Now().
func TestNextMonotonicTimestamp_SurvivesClockRegression(t *testing.T) {
	eng := &Engine{}
	forced := time.Now().Add(24 * time.Hour).UnixNano()
	eng.lastEmitNanos = forced

	for i := 0; i < 5; i++ {
		got := eng.nextMonotonicTimestamp()
		if got <= forced {
			t.Fatalf("iteration %d: nextMonotonicTimestamp = %d, want > forced clock %d (must not fall back to wall time)",
				i, got, forced)
		}
		forced = got
	}
}

// TestSendLocalOperatorMessage_EmitsStrictlyIncreasingTimestamps is the
// wiring-level regression test: it proves sendLocalOperatorMessage (the
// LocalStore/relay-only egress path, EngineOptions.WriteClient == nil) is
// actually wired to nextMonotonicTimestamp, by driving two real
// operator-emitted messages (two settle(put-accept) emissions, one per put)
// back-to-back through the real engine and LocalStore, then reading the
// durable log back and asserting the two emitted records are strictly
// ordered — not just equal-or-greater, and not reliant on the two calls
// happening to land in different wall-clock nanoseconds.
func TestSendLocalOperatorMessage_EmitsStrictlyIncreasingTimestamps(t *testing.T) {
	dir := t.TempDir()
	ls, err := dgstore.Open(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatalf("dgstore.Open: %v", err)
	}
	t.Cleanup(func() { _ = ls.Close() }) //nolint:errcheck

	operatorKey := newReservationID()
	seller := newReservationID()
	eng := NewEngine(EngineOptions{
		CampfireID:        "local",
		LocalStore:        ls,
		OperatorPublicKey: operatorKey,
		Logger:            func(format string, args ...any) { t.Logf("[engine] "+format, args...) },
	})
	if err := eng.replayAll(); err != nil {
		t.Fatalf("initial replayAll: %v", err)
	}

	appendPut := func(id string, tokenCost int64) {
		if err := ls.Append(dgstore.Record{
			ID:         id,
			CampfireID: "local",
			Sender:     seller,
			Payload:    localBuyDropPutPayload(t, "monotonic clock fixture "+id, tokenCost),
			Tags:       []string{TagPut, "exchange:content-type:code", "exchange:domain:go"},
			Timestamp:  1, // fixed, well in the past — irrelevant to this test
		}); err != nil {
			t.Fatalf("append put %s: %v", id, err)
		}
	}

	put1, put2 := "put-1", "put-2"
	appendPut(put1, 8000)
	if err := eng.AutoAcceptPut(put1, 5600, time.Now().Add(72*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut(put1): %v", err)
	}
	appendPut(put2, 9000)
	if err := eng.AutoAcceptPut(put2, 6300, time.Now().Add(72*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut(put2): %v", err)
	}

	msgs, err := ls.Replay()
	if err != nil {
		t.Fatalf("ls.Replay: %v", err)
	}

	var acceptTimestamps []int64
	acceptTag := TagPhasePrefix + SettlePhaseStrPutAccept
	for _, m := range msgs {
		for _, tag := range m.Tags {
			if tag == acceptTag {
				acceptTimestamps = append(acceptTimestamps, m.Timestamp)
				break
			}
		}
	}
	if len(acceptTimestamps) != 2 {
		t.Fatalf("expected 2 emitted put-accept messages, got %d (raw log: %s)",
			len(acceptTimestamps), mustJSON(t, msgs))
	}
	if acceptTimestamps[1] <= acceptTimestamps[0] {
		t.Errorf("emitted put-accept timestamps not strictly increasing: first=%d second=%d",
			acceptTimestamps[0], acceptTimestamps[1])
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		return "<marshal error>"
	}
	return string(b)
}
