package exchange

// engine_local_append_atomic_test.go — regression test for dontguess-90d.
//
// THE DEFECT (pre-fix appendLocalRecord in engine_core.go):
//
//	if err := e.opts.LocalStore.Append(rec); err != nil { return err } // record now Replay-visible
//	e.localMu.Lock()
//	e.localMsgByID[msg.ID] = msg
//	e.localSeen++        // fold cursor advanced HERE — a later critical section
//	e.localDispatched++
//	e.localMu.Unlock()
//
// The store Append (which makes the operator record visible to any concurrent
// LocalStore.Replay()) happens OUTSIDE localMu; the fold/dispatch cursor
// increments happen in a LATER critical section. A concurrent fold path
// (pollLocalStore / rebuildAndDispatchGapLocal, via foldAndDispatchLocalSnapshot)
// that (a) takes its Replay() snapshot AND (b) reads foldStart (= localSeen)
// in the window between the Append and the increment sees the newly-appended
// operator record in its snapshot but a foldStart that does NOT yet cover it.
// Its fold loop then state.Apply()s that operator record — which the emitter
// (e.g. emitConsumeSignal) ALSO Applies — producing a TRANSIENT in-memory
// double (entryConsumeCount == 2 for a single consume record). The durable log
// still holds exactly ONE record, so a full replay self-heals it; but until the
// next full replay the doubled count corrupts a behavioral signal feeding the
// pricing loops.
//
// THE FIX makes the store Append and the cursor increments ATOMIC with respect
// to the fold path by holding localMu across BOTH. Any concurrent fold then
// serializes on localMu and runs either fully-BEFORE the append (its Replay
// snapshot excludes the record; cursors unchanged) or fully-AFTER the increment
// (its snapshot includes the record but foldStart already covers it → skipped).
// The record is therefore applied to State EXACTLY once — by its emitter — and
// never re-applied by a fold.
//
// WHY THIS TEST IS CONCURRENT (deterministic white-box is infeasible here):
// The fix's entire effect is the DURATION localMu is held around a store
// Append that has no injectable seam (LocalStore is a concrete *store.Store;
// Append is plain fsynced file I/O). The bug's only observable manifestation is
// a real-time interleave in which a fold goroutine's Replay() lands AFTER the
// Append but its localMu-guarded foldStart read lands BEFORE the increment.
// A single-goroutine reproducer can only recreate that inconsistent
// (snapshot-includes / cursor-excludes) state by BYPASSING the real
// appendLocalRecord — and such a reproducer double-applies regardless of the
// fix, so it cannot distinguish fixed from unfixed code. The only faithful test
// drives the REAL appendLocalRecord and races it against a fold. This is NOT a
// -race (memory) detector test: every access here is mutex-guarded (localMu,
// State.mu, Store.mu); the defect is a LOGIC race the detector cannot see. The
// assertion — in-memory ConsumeCount per entry stays exactly 1 — is what catches
// it, and because the test performs no full rebuild the transient double does
// NOT self-heal, so a single caught window leaves a PERSISTENT count of 2 that
// fails the assertion. Run under -race -count to widen scheduling perturbation.
//
// Pre-fix this test fails (some entry reaches ConsumeCount == 2). Post-fix it
// passes deterministically: the emitter is the sole applier of each record.

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	dgstore "github.com/campfire-net/dontguess/pkg/store"
)

func TestAppendLocalRecordAtomic_ConcurrentFoldNoDoubleApply(t *testing.T) {
	dir := t.TempDir()
	ls, err := dgstore.Open(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatalf("dgstore.Open: %v", err)
	}
	t.Cleanup(func() { ls.Close() }) //nolint:errcheck

	operatorKey := newReservationID()
	eng := NewEngine(EngineOptions{
		CampfireID:        "local",
		LocalStore:        ls,
		OperatorPublicKey: operatorKey,
		// No run() loop — folds are driven explicitly by the goroutines below.
		PollInterval: time.Hour,
		Logger:       func(format string, args ...any) {},
	})
	if err := eng.replayAll(); err != nil {
		t.Fatalf("initial replayAll: %v", err)
	}

	// emitOperatorConsume mirrors emitConsumeSignal's local-mode egress exactly:
	// append an operator-signed exchange:consume record via the REAL
	// appendLocalRecord path (sendLocalOperatorMessage), then Apply it to State
	// (the emitter's direct apply). Each call emits exactly ONE consume for a
	// distinct entry, so a correct run leaves every entry at ConsumeCount == 1.
	emitOperatorConsume := func(entryID string) {
		payload, err := json.Marshal(map[string]any{"entry_id": entryID})
		if err != nil {
			t.Errorf("marshal consume payload: %v", err)
			return
		}
		msg, err := eng.sendLocalOperatorMessage(payload, []string{TagConsume}, nil)
		if err != nil {
			t.Errorf("sendLocalOperatorMessage: %v", err)
			return
		}
		// The emitter applies the just-appended operator message to live state,
		// mirroring emitConsumeSignal's trailing e.state.Apply(consumeMsg).
		eng.state.Apply(msg)
	}

	const (
		nEmits   = 400
		nFolders = 3
	)

	var stop atomic.Bool
	var wg sync.WaitGroup
	// Fold goroutines hammer the shared fold/dispatch path: each continuously
	// captures a fresh Replay() snapshot (outside localMu, as pollLocalStore
	// does) and folds it under localMu. Their contention for localMu is what
	// lands a fold in the vulnerable window of a concurrent appendLocalRecord —
	// snapshot including the new operator record, foldStart not yet covering it.
	for f := 0; f < nFolders; f++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				snap, err := ls.Replay()
				if err != nil {
					continue
				}
				eng.foldAndDispatchLocalSnapshot(snap)
			}
		}()
	}

	for i := 0; i < nEmits; i++ {
		emitOperatorConsume(fmt.Sprintf("entry-%d", i))
	}
	stop.Store(true)
	wg.Wait()

	// Quiescent fold: guarantee every emitted record has been folded via the
	// normal cursor path (no records left merely emitted-but-unfolded).
	final, err := ls.Replay()
	if err != nil {
		t.Fatalf("final replay: %v", err)
	}
	eng.foldAndDispatchLocalSnapshot(final)

	// (1) In-memory invariant: every entry consumed EXACTLY once. A fold that
	// double-applied an operator consume inside the append window leaves a
	// PERSISTENT count of 2 (no rebuild in this test to self-heal it).
	sigs := eng.State().AllEntryBehavioralSignals()
	for i := 0; i < nEmits; i++ {
		id := fmt.Sprintf("entry-%d", i)
		if got := sigs[id].ConsumeCount; got != 1 {
			t.Fatalf("entry %s: in-memory ConsumeCount = %d, want exactly 1 — a concurrent fold "+
				"double-applied (or missed) an operator consume appended via appendLocalRecord "+
				"(dontguess-90d: store Append observable before the fold cursor covered it)", id, got)
		}
	}

	// (2) Durable cross-check: the log holds exactly one consume per entry, i.e.
	// any in-memory double was purely a transient fold artifact, never a
	// duplicated record.
	all, err := ls.Replay()
	if err != nil {
		t.Fatalf("cross-check replay: %v", err)
	}
	durable := make(map[string]int, nEmits)
	for _, r := range all {
		isConsume := false
		for _, tag := range r.Tags {
			if tag == TagConsume {
				isConsume = true
				break
			}
		}
		if !isConsume {
			continue
		}
		var p struct {
			EntryID string `json:"entry_id"`
		}
		if err := json.Unmarshal(r.Payload, &p); err != nil {
			t.Fatalf("unmarshal consume record: %v", err)
		}
		durable[p.EntryID]++
	}
	for i := 0; i < nEmits; i++ {
		id := fmt.Sprintf("entry-%d", i)
		if durable[id] != 1 {
			t.Fatalf("entry %s: %d durable consume records on the log, want exactly 1", id, durable[id])
		}
	}
}
