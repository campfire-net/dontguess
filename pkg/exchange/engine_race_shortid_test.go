package exchange_test

// engine_race_shortid_test.go — regression tests for dontguess-889 and dontguess-8fd.
//
// dontguess-889: TOCTOU race between RunAutoAccept (ticker goroutine) and
//   AutoAcceptPut/RejectPut (operator socket handler goroutine). Without an
//   engine-level mutex, a held put can be double-accepted.
//
// dontguess-8fd: RunAutoAccept panics on entry.PutMsgID[:8] when the put message
//   ID is shorter than 8 characters. Fix: use shortKey() at all callsites.

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/campfire-net/campfire/pkg/store"
	"github.com/campfire-net/dontguess/pkg/exchange"
)

// --------------------------------------------------------------------------
// dontguess-889: TestEngine_AutoAcceptAndRejectPut_NoDoubleAccept
// --------------------------------------------------------------------------

// TestEngine_AutoAcceptAndRejectPut_NoDoubleAccept verifies that concurrent
// calls to RunAutoAccept and AutoAcceptPut never double-accept a held put.
//
// Setup: create a single put with TokenCost > max so it is held for review.
// Then launch two goroutines: one calls RunAutoAccept in a tight loop (50
// iterations), the other calls AutoAcceptPut directly. With the opMu fix the
// accept is serialized; without it a data race is detected by `go test -race`.
//
// RunAutoAccept does NOT auto-accept over-cap puts (it only classifies them as
// held-for-review), so the only accept message comes from the AutoAcceptPut
// goroutine. The invariant: acceptCount <= 1.
func TestEngine_AutoAcceptAndRejectPut_NoDoubleAccept(t *testing.T) {
	t.Parallel()

	const maxAccept = int64(1_000_000)
	const overCapCost = int64(2_000_000)

	h := newTestHarness(t)
	eng := h.newEngineWithOpts(nil)

	// Send a single over-cap put and replay into state so it shows up pending.
	putID := sendPutWithCost(t, h, "race-test over-cap result", overCapCost)
	replayAll(t, h, eng)

	// Verify it is pending.
	pending := eng.State().PendingPuts()
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending put, got %d", len(pending))
	}

	// First RunAutoAccept tick to classify it as held-for-review.
	skipped := make(map[string]struct{})
	eng.RunAutoAccept(maxAccept, time.Now(), skipped)
	held := eng.State().PutsHeldForReview()
	if len(held) != 1 {
		t.Fatalf("expected 1 held-for-review put before race, got %d", len(held))
	}

	// Race: goroutine A calls RunAutoAccept 50 times; goroutine B calls
	// AutoAcceptPut once. With opMu they are serialized; without it the race
	// detector fires on the shared state mutations (replayAll + state.Apply).
	var wg sync.WaitGroup
	var acceptErr error
	var acceptErrMu sync.Mutex

	wg.Add(1)
	go func() {
		defer wg.Done()
		sk := make(map[string]struct{})
		for i := 0; i < 50; i++ {
			eng.RunAutoAccept(maxAccept, time.Now(), sk)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		price := overCapCost * 70 / 100
		err := eng.AutoAcceptPut(putID, price, time.Now().Add(72*time.Hour))
		acceptErrMu.Lock()
		acceptErr = err
		acceptErrMu.Unlock()
	}()

	wg.Wait()

	// Count settle put-accept messages for this put.
	// Must be exactly 0 or 1 — never > 1.
	msgs, err := h.st.ListMessages(h.cfID, 0)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	acceptCount := 0
	for _, m := range msgs {
		msg := exchange.FromStoreRecord(&m)
		for _, tag := range msg.Tags {
			if tag == exchange.TagPhasePrefix+exchange.SettlePhaseStrPutAccept {
				for _, ant := range msg.Antecedents {
					if ant == putID {
						acceptCount++
					}
				}
			}
		}
	}

	if acceptCount > 1 {
		t.Errorf("double-accept detected: %d settle put-accept messages for put %s, want at most 1",
			acceptCount, putID[:8])
	}
	t.Logf("race test complete: acceptCount=%d acceptErr=%v (0 or 1 are valid; >1 is the bug)",
		acceptCount, acceptErr)
}

// --------------------------------------------------------------------------
// dontguess-8fd: TestRunAutoAccept_ShortPutMsgID
// --------------------------------------------------------------------------

// TestRunAutoAccept_ShortPutMsgID verifies that RunAutoAccept does not panic
// when a put message has an ID shorter than 8 characters.
//
// The bug: entry.PutMsgID[:8] panics when len(PutMsgID) < 8.
// The fix: shortKey(entry.PutMsgID) handles short IDs safely (returns as-is).
//
// This test injects a put record directly into the store with a 4-character ID
// and then calls RunAutoAccept. The test passes if no panic occurs.
func TestRunAutoAccept_ShortPutMsgID(t *testing.T) {
	t.Parallel()

	const maxAccept = int64(1_000_000)
	const overCapCost = int64(2_000_000)

	h := newTestHarness(t)
	eng := h.newEngineWithOpts(nil)

	// Inject a put message with a 4-character ID directly into the store.
	// Normal IDs are 64-character hex; 4 characters triggers the old [:8] panic.
	shortID := "ab12" // 4 characters — shorter than 8
	payload := putPayload(
		fmt.Sprintf("short-id regression test seed=%d", overCapCost),
		"sha256:"+fmt.Sprintf("%064x", overCapCost),
		"code",
		overCapCost,
		24576,
	)
	rec := store.MessageRecord{
		ID:          shortID,
		CampfireID:  h.cfID,
		Sender:      h.seller.pubKeyHex,
		Payload:     payload,
		Tags:        []string{exchange.TagPut, "exchange:content-type:code", "exchange:domain:go"},
		Antecedents: []string{},
		Timestamp:   store.NowNano(),
		Signature:   []byte{},
	}
	if _, err := h.st.AddMessage(rec); err != nil {
		t.Fatalf("AddMessage with short ID: %v", err)
	}

	// Replay messages into engine state so the put appears as pending.
	stMsgs, err := h.st.ListMessages(h.cfID, 0)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	eng.State().Replay(exchange.FromStoreRecords(stMsgs))

	// Verify the short-ID put is in pending before we call RunAutoAccept.
	pending := eng.State().PendingPuts()
	found := false
	for _, p := range pending {
		if p.PutMsgID == shortID {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected short-ID put %q in pending after replay, got %d pending puts",
			shortID, len(pending))
	}

	// This must not panic (panicked before the fix due to [:8] on a 4-char ID).
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("RunAutoAccept panicked on short PutMsgID %q: %v", shortID, r)
			}
		}()
		skipped := make(map[string]struct{})
		eng.RunAutoAccept(maxAccept, time.Now(), skipped)
	}()

	// The short-ID put (cost > max) should be classified as held-for-review.
	held := eng.State().PutsHeldForReview()
	found = false
	for _, e := range held {
		if e.PutMsgID == shortID {
			found = true
		}
	}
	if !found {
		t.Errorf("expected short-ID put %q in PutsHeldForReview after RunAutoAccept", shortID)
	}
	t.Logf("no panic on short PutMsgID %q (shortKey handled it correctly)", shortID)
}
