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
	"sync/atomic"
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

// TestPostOpenCompressionAssign_LockSerializesSimultaneousWarmCold is the
// dontguess-20e Gap B MUTATION-FOR-THE-LOCK proof. Its sibling
// TestPostOpenCompressionAssign_WarmPollVsColdYieldsSingleAssign is really a mutation
// test for the PREDICATE (hasActiveBuyerOrOpenCompressAssign), NOT the lock: the warm
// poll pipeline (fold+match+dispatch) is far longer than the short cold post, so in a
// free race cold almost always lands its assign FIRST and warm then defers via the
// predicate — reverting ONLY the compressAssignMu Lock/Unlock (keeping the predicate)
// still leaves that sibling GREEN. The one schedule the lock actually protects — BOTH
// writers reading the guard empty, THEN both posting — has zero coverage there because
// free scheduling never reliably produces it.
//
// This test forces that exact schedule DETERMINISTICALLY with the
// compressAssignGuardHook seam (engine_core.go). The hook fires inside the
// compressAssignMu critical section, AFTER each writer's dedup guard read returns
// empty (committed-to-post) and BEFORE its post. A test barrier in the hook holds the
// first arriver until the second also arrives, then releases both to post together:
//
//   - WITH compressAssignMu (production): only ONE writer can be inside the critical
//     section at a time. The first arriver reaches the hook and waits on the barrier;
//     the second is blocked on compressAssignMu.Lock() and never reaches the hook.
//     The barrier TIMES OUT (only one arrival), the first posts and releases the lock,
//     and the second then acquires the lock, observes the applied assign at its recheck
//     (cold: len(ActiveAssigns)>0; warm: the open cold assign via
//     hasActiveBuyerOrOpenCompressAssign) and DEFERS. Result: exactly 1 assign.
//
//   - WITHOUT compressAssignMu (the mutation): BOTH writers enter concurrently, both
//     read the guard empty (neither has posted — each is held at the hook before its
//     post), both reach the hook, the barrier sees two arrivals and releases them
//     together, and BOTH post. Result: 2 assigns = warm+cold double-pay. The test FAILS.
//
// The barrier timeout is generous relative to the warm-poll pipeline latency, so in
// the unlocked case the second (warm) arrival reliably beats the timeout and the
// interleave is deterministic; in the locked case the timeout is simply the (bounded)
// cost of proving the second writer could never reach the post window. State accessors
// are individually State.mu-synchronized, so removing compressAssignMu yields a LOGICAL
// double-post, not a low-level data race — the -race detector stays quiet and the
// 2-vs-1 assertion is what fails. NOT parallel: it installs a process-global hook.
func TestPostOpenCompressionAssign_LockSerializesSimultaneousWarmCold(t *testing.T) {
	// Deliberately NOT t.Parallel(): compressAssignGuardHook is process-global.
	defer exchange.SetCompressAssignGuardHookForTest(nil)

	const iterations = 3
	const tokenCost int64 = 20000
	// Generous vs. the warm-poll pipeline latency (tens of ms under -race): in the
	// UNLOCKED case the second arrival beats this easily so the interleave is
	// deterministic; in the LOCKED case this is the bounded wait the sole arriver
	// spends proving the other writer never reaches the post window.
	const barrierTimeout = 1500 * time.Millisecond

	for it := 0; it < iterations; it++ {
		h := newTestHarness(t)
		eng := h.newEngine()

		desc := fmt.Sprintf("20e lock-mutation fixture %d: bounded worker-pool patterns", it)
		entryID := seedAcceptedEntryNoAssign(t, h, eng, desc, tokenCost)

		// Make the poll-dispatched buy semantically match THIS entry so the warm
		// assign path (writer 4) actually engages.
		idx := matching.NewIndex(nil, matching.RankOptions{})
		idx.Rebuild([]matching.RankInput{{EntryID: entryID, Description: desc, TokenCost: tokenCost}})
		eng.SetMatchIndexForTest(idx)

		// Prime the poll cursors past put + put-accept (confirming no hot assign
		// leaked) so the RACING poll dispatches ONLY the buy — an isolated warm post.
		if err := eng.PollLocalStoreForTest(); err != nil {
			t.Fatalf("iter %d: priming PollLocalStoreForTest: %v", it, err)
		}
		if n := countActiveCompressAssigns(eng, entryID); n != 0 {
			t.Fatalf("iter %d precondition: entry has %d active compress assign(s) before the race, want 0", it, n)
		}

		// Barrier state, fresh per iteration. The hook records each writer's arrival
		// (past its guard, before its post) and holds it until BOTH arrive or the
		// timeout fires.
		var arrivals int32
		bothArrived := make(chan struct{})
		var kindsMu sync.Mutex
		kinds := map[string]int{}
		exchange.SetCompressAssignGuardHookForTest(func(kind string) {
			kindsMu.Lock()
			kinds[kind]++
			kindsMu.Unlock()
			if atomic.AddInt32(&arrivals, 1) == 2 {
				close(bothArrived) // second arrival releases both writers to post
			}
			select {
			case <-bothArrived:
			case <-time.After(barrierTimeout):
			}
		})

		// Append the buy — never hand-dispatched — so the racing poll folds and
		// dispatches it exactly as production's poll loop does.
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
		close(start) // release both simultaneously
		wg.Wait()

		exchange.SetCompressAssignGuardHookForTest(nil)

		// Prove the warm path actually engaged — otherwise "exactly 1" would be
		// trivially satisfied by the cold post alone and the lock would be untested.
		msgs, err := h.st.ListMessages(h.cfID, 0)
		if err != nil {
			t.Fatalf("iter %d: ListMessages: %v", it, err)
		}
		if !hasMatchMessage(msgs) {
			t.Fatalf("iter %d: no match message emitted — the poll loop did not match the buy, so the warm poster never engaged and the lock was not exercised", it)
		}

		if got := countActiveCompressAssigns(eng, entryID); got != 1 {
			kindsMu.Lock()
			warmN, coldN := kinds["warm"], kinds["cold"]
			kindsMu.Unlock()
			t.Fatalf("iter %d: entry has %d active compress assigns, want exactly 1 — with BOTH writers deterministically held past their dedup guard before either posts (warm-hook-fires=%d, cold-hook-fires=%d), only compressAssignMu can collapse the simultaneous warm+cold interleave to a single assign; 2 = the double-post/double-pay this lock prevents",
				it, got, warmN, coldN)
		}
	}
}

// hasMatchMessage reports whether any record in msgs is an exchange:match (and not a
// buy-miss), proving handleBuy matched the racing buy and ran emitMatchResponse.
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
