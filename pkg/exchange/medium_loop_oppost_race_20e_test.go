package exchange_test

// medium_loop_oppost_race_20e_test.go is the MANDATORY enforcement test for
// dontguess-20e: the medium-loop compression-assign poster
// (Engine.PostOpenCompressionAssign, engine_buy.go) must make its check-then-act
// ATOMIC under opMu so it cannot double-post.
//
// Background (948a review of dontguess-ffb). serve.go's medium-loop goroutine
// drives pkg/pricing MediumLoop.Tick, whose PostAssign callback calls
// Engine.PostOpenCompressionAssign. That is a check-then-act: read the dedup guard
// (HasCompressedVersion / ActiveAssigns), then a compound sendOperatorMessage WRITE
// + state.Apply. opMu is the documented serializer for operator broadcasts, but
// before this item it named only the auto-accept ticker and the operator-socket
// handler — the medium loop was an unaccounted THIRD concurrent operator-broadcast
// writer that did NOT hold opMu. So two operator-broadcast compression-assign posts
// for the same entry could each read ActiveAssigns()==0 before either applied its
// post, and BOTH land — two AssignRecords for one compression unit, so two agents
// could each claim + complete + be paid task_reward for one unit of work (a scrip
// double-pay leak).
//
// The fix makes PostOpenCompressionAssign acquire opMu and RE-CHECK the guard
// atomically with the post. These tests prove that with a REAL exchange.Engine
// backed by a REAL pkg/store event log, under the race detector, with the two posts
// ACTUALLY racing (a released start barrier), not hand-serialized.

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/3dl-dev/dontguess/pkg/exchange"
	"github.com/3dl-dev/dontguess/pkg/matching"
	"github.com/3dl-dev/dontguess/pkg/store"
)

// seedAcceptedEntryNoAssign puts a plaintext fixture entry and folds a
// settle(put-accept) DIRECTLY (the same State.applySettlePutAccept fold
// Engine.AutoAcceptPut triggers) rather than calling eng.AutoAcceptPut. AutoAcceptPut
// ALSO fires Engine.sendCompressionAssign unconditionally as a side effect (the "Hot
// compression offer" — an EXCLUSIVE-to-seller assign posted on every accept): that
// standing assign would itself count as an active assign for the entry and, post
// dontguess-20e, make PostOpenCompressionAssign atomically DEFER — suppressing the
// cold post these tests exercise. Folding put-accept directly promotes the entry to
// inventory (pendingPuts -> s.inventory) with ZERO active assigns, the exact state
// the production medium loop posts from (its own guard is len(ActiveAssigns)==0).
// Returns the entry ID (the put message ID).
func seedAcceptedEntryNoAssign(t *testing.T, h *testHarness, eng *exchange.Engine, desc string, tokenCost int64) string {
	t.Helper()
	putMsg := h.sendMessage(h.seller,
		putPayload(desc, "sha256:"+fmt.Sprintf("%064x", tokenCost), "code", tokenCost, 5000),
		[]string{exchange.TagPut, "exchange:content-type:code", "exchange:domain:go"},
		nil,
	)
	acceptPayload, err := json.Marshal(map[string]any{
		"phase":      "put-accept",
		"entry_id":   putMsg.ID,
		"price":      tokenCost / 2,
		"expires_at": time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339),
	})
	if err != nil {
		t.Fatalf("seedAcceptedEntryNoAssign: encoding put-accept payload: %v", err)
	}
	h.sendMessage(h.operator, acceptPayload,
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrPutAccept,
			exchange.TagVerdictPrefix + "accepted",
		},
		[]string{putMsg.ID},
	)
	msgs, err := h.st.ListMessages(h.cfID, 0)
	if err != nil {
		t.Fatalf("seedAcceptedEntryNoAssign: listing messages: %v", err)
	}
	eng.State().Replay(exchange.FromStoreRecords(msgs))
	return putMsg.ID
}

// countActiveCompressAssigns returns the number of non-terminal compress assigns
// currently recorded for entryID, read through the same production accessor the
// engine and `dontguess assigns` use.
func countActiveCompressAssigns(eng *exchange.Engine, entryID string) int {
	n := 0
	for _, a := range eng.State().ActiveAssigns(entryID) {
		if a.TaskType == "compress" {
			n++
		}
	}
	return n
}

// TestPostOpenCompressionAssign_ConcurrentPostersYieldSingleAssign is the primary
// dontguess-20e enforcement proof. It launches many goroutines that ACTUALLY race
// on PostOpenCompressionAssign for one entry (released together off a start barrier)
// and asserts exactly ONE compress AssignRecord results. Each goroutine models an
// operator-broadcast compression-assign writer doing the medium loop's check-then-act.
//
// Without the fix, PostOpenCompressionAssign held no opMu and had no post-time guard:
// every racer's guard read saw ActiveAssigns()==0 and every racer posted, yielding N
// AssignRecords (N-way double-post -> N-way task_reward double-pay). With the fix, the
// guard read and the post are atomic under opMu, so exactly one racer posts and the
// rest observe the applied assign and defer. Run under `-race`.
func TestPostOpenCompressionAssign_ConcurrentPostersYieldSingleAssign(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	const tokenCost int64 = 20000
	entryID := seedAcceptedEntryNoAssign(t, h, eng,
		"20e concurrency fixture: race N cold compression posters", tokenCost)

	if n := countActiveCompressAssigns(eng, entryID); n != 0 {
		t.Fatalf("precondition: entry has %d active compress assign(s), want 0", n)
	}

	const workers = 16
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start // hold at the barrier so all goroutines contend at once
			errs[idx] = eng.PostOpenCompressionAssign(entryID)
		}(i)
	}
	close(start) // release the whole herd simultaneously — a genuine race
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("worker %d: PostOpenCompressionAssign returned error: %v", i, err)
		}
	}

	if got := countActiveCompressAssigns(eng, entryID); got != 1 {
		t.Fatalf("after %d concurrent PostOpenCompressionAssign calls, entry has %d active compress assigns, want exactly 1 — the opMu-atomic check-then-act must let exactly one post through; a stale ActiveAssigns()==0 read would let multiple post and double-pay task_reward",
			workers, got)
	}
}

// TestPostOpenCompressionAssign_WarmPollVsColdYieldsSingleAssign is the dontguess-20e
// Gap A enforcement proof: it RACES the real STEADY-STATE POLL-LOOP warm poster
// (writer 4, path ii) against the medium-loop COLD poster (writer 3) for ONE entry,
// with two genuine goroutines released off a start barrier, no hand-ordering, and no
// pre-applied assign — and asserts EXACTLY ONE compress AssignRecord survives.
//
// This is the cross-writer race the earlier "defers to a pre-applied dispatch assign"
// test could not catch. That test applied its dispatch assign to State BEFORE the
// barrier, so it was a STATIC pre-existing assign every cold poster simply read as
// ActiveAssigns()>0 and skipped — deleting the serialization still passed it. Here
// the warm assign is created by dispatching a REAL buy through the ACTUAL poll path
// (Engine.PollLocalStoreForTest -> pollLocalStore -> foldAndDispatchLocalSnapshot ->
// dispatchLocalGap -> dispatch -> handleBuy -> sendWarmCompressionAssign), which holds
// NEITHER opMu NOR localMu. So it genuinely races the cold poster's check-then-act.
//
// The poll-loop warm post and the cold post touch State on two goroutines with no
// shared opMu (the poll path never takes it). Only the leaf compressAssignMu makes
// their guard+post atomic: whichever lands first, the other's recheck observes it
// (cold sees ActiveAssigns()>0; warm sees the open cold assign via
// hasActiveBuyerOrOpenCompressAssign) and defers. Remove that serialization and the
// dominant cold-first ordering double-posts — cold lands an OPEN assign, then the
// unserialized warm's buyer-scoped guard does not see it and posts a SECOND assign:
// two AssignRecords for one compression unit = two task_reward payments (the
// double-pay this item targets). Run under `-race`.
//
// Iterated so a scheduler that happens to finish one goroutine before the other
// starts on a given round cannot mask a regression: every round must yield exactly 1.
func TestPostOpenCompressionAssign_WarmPollVsColdYieldsSingleAssign(t *testing.T) {
	t.Parallel()

	const iterations = 25
	const tokenCost int64 = 20000

	for it := 0; it < iterations; it++ {
		h := newTestHarness(t)
		eng := h.newEngine()

		desc := fmt.Sprintf("20e warm-poll vs cold race fixture %d: bounded worker-pool patterns", it)
		// Fold a real put-accept (no hot assign) so the entry is live inventory with
		// ZERO active assigns — the exact steady state both posters fire from.
		entryID := seedAcceptedEntryNoAssign(t, h, eng, desc, tokenCost)

		// Force the poll-dispatched buy to semantically match THIS entry so
		// emitMatchResponse runs and attempts the warm assign (writer 4).
		idx := matching.NewIndex(nil, matching.RankOptions{})
		idx.Rebuild([]matching.RankInput{{EntryID: entryID, Description: desc, TokenCost: tokenCost}})
		eng.SetMatchIndexForTest(idx)

		// Advance the engine's poll cursors past the put + put-accept (folding them,
		// confirming NO hot assign is posted) so the RACING poll below dispatches ONLY
		// the buy — an isolated writer-4 warm post.
		if err := eng.PollLocalStoreForTest(); err != nil {
			t.Fatalf("iter %d: priming PollLocalStoreForTest: %v", it, err)
		}
		if n := countActiveCompressAssigns(eng, entryID); n != 0 {
			t.Fatalf("iter %d precondition: entry has %d active compress assign(s) before the race, want 0 (a hot assign must NOT have leaked from the seed)", it, n)
		}

		// The buy is APPENDED to the store — never hand-dispatched — so the racing
		// poll folds and dispatches it exactly as production's poll loop does.
		h.sendMessage(h.buyer, buyPayload(desc, 10*tokenCost), []string{exchange.TagBuy}, nil)

		var wg sync.WaitGroup
		start := make(chan struct{})
		wg.Add(2)
		// Writer 4 (WARM) via the REAL poll loop — holds neither opMu nor localMu.
		go func() {
			defer wg.Done()
			<-start
			if err := eng.PollLocalStoreForTest(); err != nil {
				t.Errorf("iter %d: racing PollLocalStoreForTest: %v", it, err)
			}
		}()
		// Writer 3 (COLD) via the medium-loop entry point — holds opMu.
		go func() {
			defer wg.Done()
			<-start
			if err := eng.PostOpenCompressionAssign(entryID); err != nil {
				t.Errorf("iter %d: PostOpenCompressionAssign: %v", it, err)
			}
		}()
		close(start) // release both simultaneously — a genuine race
		wg.Wait()

		// The buy must have matched (proving the warm path actually engaged — otherwise
		// the "exactly 1" assertion would be trivially satisfied by the cold post alone).
		msgs, err := h.st.ListMessages(h.cfID, 0)
		if err != nil {
			t.Fatalf("iter %d: ListMessages: %v", it, err)
		}
		if !hasMatchMessage(msgs) {
			t.Fatalf("iter %d: no match message emitted — the poll loop did not match the buy, so the warm poster (writer 4) never engaged and the race was not exercised", it)
		}

		if got := countActiveCompressAssigns(eng, entryID); got != 1 {
			t.Fatalf("iter %d: entry has %d active compress assigns after racing a POLL-LOOP WARM post (writer 4 path ii) against a COLD post (writer 3), want exactly 1 — the cross-writer compressAssignMu serialization must let exactly one land; 2 = warm+cold double-pay",
				it, got)
		}
	}
}

// hasMatchMessage reports whether any record is a real match (TagMatch without
// TagBuyMiss) — used to assert a buy actually matched (not merely missed).
func hasMatchMessage(msgs []store.MessageRecord) bool {
	for i := range msgs {
		isMatch, isMiss := false, false
		for _, tag := range msgs[i].Tags {
			if tag == exchange.TagMatch {
				isMatch = true
			}
			if tag == exchange.TagBuyMiss {
				isMiss = true
			}
		}
		if isMatch && !isMiss {
			return true
		}
	}
	return false
}
