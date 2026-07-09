package scrip_test

// Race/correctness tests for the double-spend window between the SpendingStore
// mutators (GetBudget/DecrementBudget/AddBudget/Balance) and Replay() on
// LocalScripStore (dontguess-1d8).
//
// Two defects are covered:
//
//  1. TOCTOU on the map swap. The mutators capture a *balanceEntry under
//     balancesMu.RLock, release it, then lock entry.mu and mutate — without
//     holding replayMu. A concurrent Replay() swaps s.balances out from under
//     them, so a mutation can land on a discarded entry (lost decrement =
//     double-spend). The fix holds replayMu.RLock across the entire
//     capture-and-mutate (lock order replayMu -> balancesMu -> entry.mu),
//     while Replay holds replayMu.Lock for its swap+refold.
//
//  2. ABA on the ETag. The pre-fix ETag was "gen". A balance rebuilt from a
//     single mint always re-climbs to the same gen, so a client holding a
//     pre-resync ETag can ABA-match a freshly-rebuilt entry and spend against
//     it. The fix stamps a monotonic per-Replay epoch into every entry so the
//     ETag is "epoch-gen": the stale ETag carries the OLD epoch and can never
//     match a rebuilt entry.
//
// These run against the real dgstore.Store + real LocalScripStore — no
// mocks. Run with -race.

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/campfire-net/dontguess/pkg/scrip"
)

// TestLocalScripStore_ABAEtag_RejectedAfterResync is the deterministic,
// load-bearing regression for the ABA double-spend. It FAILS on the pre-fix
// code: a pre-resync ETag ("1") ABA-matches the single-mint entry rebuilt by
// Replay (also gen 1), so the decrement wrongly succeeds — the client spends
// twice against a balance that was reset from the log. With the epoch-stamped
// ETag the stale token can never match a rebuilt entry.
func TestLocalScripStore_ABAEtag_RejectedAfterResync(t *testing.T) {
	st := newLocalStore(t)
	// Alice's balance derives from exactly one mint, so every Replay rebuilds
	// her entry to the identical gen — the precondition that makes a bare-gen
	// ETag ABA-collide.
	appendScripMsg(t, st, "m1", "operator", scrip.TagScripMint, scrip.MintPayload{
		Recipient: agentAlice, Amount: 1000,
	}, 1)

	cs, err := scrip.NewLocalScripStore(st, "operator")
	if err != nil {
		t.Fatalf("NewLocalScripStore: %v", err)
	}

	ctx := context.Background()

	// Capture an ETag BEFORE the resync.
	_, staleEtag, err := cs.GetBudget(ctx, agentAlice, scrip.BalanceKey)
	if err != nil {
		t.Fatalf("GetBudget: %v", err)
	}
	if staleEtag == "" {
		t.Fatal("expected non-empty etag")
	}

	// Resync: reset-and-refold from the canonical log. The rebuilt entry re-climbs
	// gen from the same single mint.
	if err := cs.Replay(); err != nil {
		t.Fatalf("Replay: %v", err)
	}

	// The pre-resync ETag must NOT validate against the rebuilt entry. On the
	// unfixed code it does (ABA) and this decrement succeeds — a double-spend,
	// because the balance was just reset to its full logged value.
	_, _, err = cs.DecrementBudget(ctx, agentAlice, scrip.BalanceKey, 500, staleEtag)
	if !errors.Is(err, scrip.ErrConflict) {
		t.Fatalf("DecrementBudget with pre-resync etag: err = %v, want ErrConflict "+
			"(ABA double-spend window is open — a stale etag matched a rebuilt entry)", err)
	}

	// A FRESH ETag read after the resync must still work — the fix rejects only
	// stale-epoch tokens, it does not break the normal optimistic-locking path.
	_, freshEtag, err := cs.GetBudget(ctx, agentAlice, scrip.BalanceKey)
	if err != nil {
		t.Fatalf("post-resync GetBudget: %v", err)
	}
	newVal, _, err := cs.DecrementBudget(ctx, agentAlice, scrip.BalanceKey, 500, freshEtag)
	if err != nil {
		t.Fatalf("DecrementBudget with fresh post-resync etag: %v", err)
	}
	if newVal != 500 {
		t.Fatalf("balance after single decrement = %d, want 500", newVal)
	}
}

// TestLocalScripStore_AddBudget_ABAEtag_RejectedAfterResync mirrors the ABA
// guard for the credit path: a pre-resync ETag must not let an AddBudget land
// on a rebuilt entry. AddBudget treats an empty etag as "no CAS", so we must
// exercise it with the captured stale token specifically.
func TestLocalScripStore_AddBudget_ABAEtag_RejectedAfterResync(t *testing.T) {
	st := newLocalStore(t)
	appendScripMsg(t, st, "m1", "operator", scrip.TagScripMint, scrip.MintPayload{
		Recipient: agentAlice, Amount: 1000,
	}, 1)

	cs, err := scrip.NewLocalScripStore(st, "operator")
	if err != nil {
		t.Fatalf("NewLocalScripStore: %v", err)
	}
	ctx := context.Background()

	_, staleEtag, err := cs.GetBudget(ctx, agentAlice, scrip.BalanceKey)
	if err != nil {
		t.Fatalf("GetBudget: %v", err)
	}

	if err := cs.Replay(); err != nil {
		t.Fatalf("Replay: %v", err)
	}

	_, _, err = cs.AddBudget(ctx, agentAlice, scrip.BalanceKey, 250, staleEtag)
	if !errors.Is(err, scrip.ErrConflict) {
		t.Fatalf("AddBudget with pre-resync etag: err = %v, want ErrConflict (ABA window open)", err)
	}
}

// TestLocalScripStore_ConcurrentReplayMutations hammers DecrementBudget and
// AddBudget from many goroutines while Replay() runs concurrently, under the
// race detector. It proves:
//
//   - the replayMu -> balancesMu -> entry.mu lock ordering is deadlock-free
//     (the test terminates) and race-free (`go test -race` reports nothing);
//   - no balance ever goes negative under the storm;
//   - the ABA guard holds under concurrency: an ETag captured before any
//     resync never validates after a final resync (deterministic FAIL on the
//     unfixed code, since Alice's single-mint entry always rebuilds to the
//     same gen).
func TestLocalScripStore_ConcurrentReplayMutations(t *testing.T) {
	st := newLocalStore(t)
	// Two independently-minted balances so the storm exercises multiple entries.
	appendScripMsg(t, st, "m1", "operator", scrip.TagScripMint, scrip.MintPayload{
		Recipient: agentAlice, Amount: 1_000_000,
	}, 1)
	appendScripMsg(t, st, "m2", "operator", scrip.TagScripMint, scrip.MintPayload{
		Recipient: agentBob, Amount: 1_000_000,
	}, 2)

	cs, err := scrip.NewLocalScripStore(st, "operator")
	if err != nil {
		t.Fatalf("NewLocalScripStore: %v", err)
	}
	ctx := context.Background()

	// ETag captured before any resync — used for the ABA post-check.
	_, staleEtag, err := cs.GetBudget(ctx, agentAlice, scrip.BalanceKey)
	if err != nil {
		t.Fatalf("GetBudget: %v", err)
	}

	const workers = 16
	const iters = 200
	var mutators sync.WaitGroup // decrement + add workers
	var replayers sync.WaitGroup
	var stopReplay atomic.Bool
	var negativeSeen atomic.Bool

	// Decrement workers on Alice: read-etag then CAS-decrement. These live
	// mutations are intentionally NOT appended to the log, so a concurrent
	// Replay legitimately resets them — the point is to stress capture-and-mutate
	// against the swap, not to preserve the decrements.
	mutators.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer mutators.Done()
			for i := 0; i < iters; i++ {
				_, etag, err := cs.GetBudget(ctx, agentAlice, scrip.BalanceKey)
				if err != nil || etag == "" {
					continue
				}
				val, _, err := cs.DecrementBudget(ctx, agentAlice, scrip.BalanceKey, 1, etag)
				if err == nil && val < 0 {
					negativeSeen.Store(true)
				}
			}
		}()
	}

	// AddBudget workers on Bob: exercise the create/update path concurrently.
	mutators.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer mutators.Done()
			for i := 0; i < iters; i++ {
				if _, _, err := cs.AddBudget(ctx, agentBob, scrip.BalanceKey, 1, ""); err != nil {
					continue
				}
			}
		}()
	}

	// Concurrent resync loop — the map swap that races the mutators. Runs until
	// the mutators finish, then stops.
	replayers.Add(1)
	go func() {
		defer replayers.Done()
		for !stopReplay.Load() {
			if err := cs.Replay(); err != nil {
				t.Errorf("concurrent Replay: %v", err)
				return
			}
		}
	}()

	mutators.Wait() // all bounded mutator goroutines done
	stopReplay.Store(true)
	replayers.Wait() // the unbounded replay loop observes the stop and joins

	if negativeSeen.Load() {
		t.Fatal("a balance went negative under concurrent Replay/mutation (torn state)")
	}

	// Final deterministic resync, then the ABA post-check: the ETag captured
	// before any resync must not validate against the rebuilt (single-mint,
	// same-gen) entry. FAILS on the unfixed code.
	if err := cs.Replay(); err != nil {
		t.Fatalf("final Replay: %v", err)
	}
	if _, _, err := cs.DecrementBudget(ctx, agentAlice, scrip.BalanceKey, 1, staleEtag); !errors.Is(err, scrip.ErrConflict) {
		t.Fatalf("post-storm DecrementBudget with pre-resync etag: err = %v, want ErrConflict "+
			"(ABA window open across resync)", err)
	}

	// Sanity: Alice's balance re-derives to her full logged mint after the final
	// resync (the un-logged live decrements are correctly discarded).
	if got := cs.Balance(agentAlice); got != 1_000_000 {
		t.Fatalf("Alice balance after final resync = %d, want 1000000", got)
	}
}
