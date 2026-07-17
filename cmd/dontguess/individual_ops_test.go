package main

// individual_ops_test.go — feature + concurrency tests for dontguess-2b4
// (ed2-E): the individual-tier (zero-relay) OpPut/OpBuy IPC round trip.
//
// The harness mirrors runServeLocal exactly (precedent
// serve_local_test.go's TestServeLocal_PutBuyMatch_NoCampfire): a real
// dgstore.Store + a real eng.Start() poll loop + the operator socket server
// (precedent operator_test.go's startSocketServer/dialAndRequest, reused
// directly — same package). So every round trip below exercises the real
// production dispatch path, not a hand-rolled substitute (design §4:
// individual tier stays byte-for-byte).
//
// THE FOLD-CURSOR FIX (dontguess-2b4). A prior implementation routed the
// external client write through a raw dgstore.Store.Append from the socket
// goroutine, which does NOT hold the engine's localMu. The engine's poll loop
// (pollLocalStore) and its operator egress (appendLocalRecord) share a
// length-cursor (localSeen/localDispatched) advanced with a RELATIVE `++` that
// is only correct when localSeen == len(store) at append time. A concurrent
// raw Append steals that tail slot, desyncing the cursor: a folded record is
// marked "already dispatched" and permanently stalls (LOST fold), and an
// operator record beyond the cursor is re-folded (DUPLICATED fold). The fix
// routes the external write through exchange.Engine.IngestLocalRecord, which
// appends+folds under localMu and claims the dispatch cursor monotonically,
// serializing with the poll loop exactly like appendLocalRecord does.
//
// TestIndividualTier_ConcurrentOpPutOpBuy_NoFoldCursorCorruption below is the
// PROOF: it fires many OpPut/OpBuy pairs CONCURRENTLY (genuinely — a start
// barrier releases all goroutines at once) against the real serve engine over
// the real socket, and asserts store integrity, every buy resolving to its own
// hit (no lost fold → no stall), and EXACTLY ONE match record per buy (no
// duplicated dispatch). Run under -race it also catches the data race the raw
// Append introduced on the cursor fields.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/3dl-dev/dontguess/pkg/exchange"
	dgstore "github.com/3dl-dev/dontguess/pkg/store"
)

// newIndividualTierEngine builds the exact pieces runServeLocal wires together
// for the individual (zero-relay) tier — LocalStore + a real eng.Start() poll
// loop, no ScripStore, no TrustChecker — so the OpPut/OpBuy round trip
// exercises the real production dispatch path (design §4: individual tier stays
// byte-for-byte).
func newIndividualTierEngine(t *testing.T) *exchange.Engine {
	t.Helper()
	dgHome := t.TempDir()

	operatorKey, err := loadOrCreateLocalOperatorKey(dgHome)
	if err != nil {
		t.Fatalf("loadOrCreateLocalOperatorKey: %v", err)
	}

	ls, err := dgstore.Open(filepath.Join(dgHome, "events.jsonl"))
	if err != nil {
		t.Fatalf("dgstore.Open: %v", err)
	}
	t.Cleanup(func() { _ = ls.Close() })

	eng := exchange.NewEngine(exchange.EngineOptions{
		CampfireID:        "local",
		LocalStore:        ls,
		OperatorPublicKey: operatorKey,
		PollInterval:      20 * time.Millisecond,
		Logger: func(format string, args ...any) {
			t.Logf("[engine] "+format, args...)
		},
	})

	// Drive the DESTRUCTIVE startup replay SYNCHRONOUSLY on the (currently empty)
	// store BEFORE any op runs, then launch ONLY the additive poll loop in a
	// goroutine. Launching eng.Start(ctx) in a goroutine instead races Start's
	// replayAll — a full, destructive state.Replay of the startup snapshot — against
	// the test's synchronous socket ops: under CPU load replayAll captures an empty
	// snapshot at launch and its later state.Replay([]) + `localSeen = 0` overwrite
	// the State and cursor a concurrently-completed OpPut already built, wiping the
	// just-put entry out of inventory + match index so a following OpBuy
	// false-misses (dontguess-c8b, observed as inv=0/indexLen=0 at buy time). The
	// synchronous StartupReplayForTest folds the empty log and seeds the cursors
	// once, up front; the only remaining concurrent folder is the poll loop
	// (foldAndDispatchLocalSnapshot), which is additive and monotonic — it can never
	// wipe State. This preserves a genuinely concurrent poll loop for the fold-cursor
	// storm test while removing the startup-replay race.
	if err := eng.StartupReplayForTest(); err != nil {
		t.Fatalf("StartupReplayForTest: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = eng.RunPollLoopForTest(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	return eng
}

// TestOpPut_Individual_ThenOpBuy_ReturnsMatchedContent proves the ed2-E
// outcome: OpPut followed by OpBuy over the operator socket, individual tier
// (ScripStore==nil, TrustChecker==nil), returns the matched content inline in
// one OpBuy response.
func TestOpPut_Individual_ThenOpBuy_ReturnsMatchedContent(t *testing.T) {
	t.Parallel()

	eng := newIndividualTierEngine(t)
	sockPath, _ := startSocketServer(t, eng)

	contentBytes := []byte("cached inference result: a Go HTTP handler unit test generator, fully worked example with table-driven subtests and httptest.NewRecorder for good measure")

	var putResp opPutResponse
	dialAndRequest(t, sockPath, map[string]any{
		"op":           OpPut,
		"description":  "Go HTTP handler unit test generator",
		"content":      base64.StdEncoding.EncodeToString(contentBytes),
		"token_cost":   int64(8000),
		"content_type": "exchange:content-type:code",
		"domains":      []string{"go"},
	}, &putResp)
	if !putResp.OK {
		t.Fatalf("OpPut failed: %s", putResp.Error)
	}
	if putResp.PutID == "" {
		t.Fatal("OpPut: empty put_id in response")
	}

	var buyResp opBuyResponse
	dialAndRequest(t, sockPath, map[string]any{
		"op":          OpBuy,
		"task":        "Generate unit tests for a Go HTTP handler",
		"budget":      int64(50000),
		"max_results": 3,
	}, &buyResp)
	if !buyResp.OK {
		t.Fatalf("OpBuy failed: %s", buyResp.Error)
	}
	if buyResp.TimedOut {
		t.Fatal("OpBuy: timed out waiting for a match")
	}
	if buyResp.Miss {
		t.Fatal("OpBuy: reported a miss for content that was just put")
	}
	if !buyResp.Matched {
		t.Fatal("OpBuy: expected matched=true")
	}
	if buyResp.ContentType != "code" {
		t.Errorf("OpBuy: content_type = %q, want %q", buyResp.ContentType, "code")
	}
	if buyResp.TokenCost != 8000 {
		t.Errorf("OpBuy: token_cost = %d, want 8000", buyResp.TokenCost)
	}
	gotContent, err := base64.StdEncoding.DecodeString(buyResp.Content)
	if err != nil {
		t.Fatalf("decode buy response content: %v", err)
	}
	if string(gotContent) != string(contentBytes) {
		t.Fatalf("OpBuy: content mismatch\n got: %s\nwant: %s", gotContent, contentBytes)
	}
}

// TestOpBuy_Individual_GenuineMiss proves a genuine buy-miss (no matching
// inventory at all) is reported as Miss=true, not TimedOut — the client must be
// able to tell "nobody has this yet" apart from "the engine did not respond"
// (design §5.4's AMBIGUOUS-timeout discipline).
func TestOpBuy_Individual_GenuineMiss(t *testing.T) {
	t.Parallel()

	eng := newIndividualTierEngine(t)
	sockPath, _ := startSocketServer(t, eng)

	var buyResp opBuyResponse
	dialAndRequest(t, sockPath, map[string]any{
		"op":          OpBuy,
		"task":        "something nobody has ever cached, extremely specific and novel",
		"budget":      int64(50000),
		"max_results": 3,
	}, &buyResp)
	if !buyResp.OK {
		t.Fatalf("OpBuy failed: %s", buyResp.Error)
	}
	if buyResp.TimedOut {
		t.Fatal("OpBuy: timed out — expected a genuine miss response, not a timeout")
	}
	if buyResp.Matched {
		t.Fatal("OpBuy: expected matched=false against empty inventory")
	}
	if !buyResp.Miss {
		t.Fatal("OpBuy: expected miss=true against empty inventory")
	}
}

// TestOpPut_Individual_RequiresContent proves OpPut validates its required
// fields before touching the store (LOUD, not a silent no-op).
func TestOpPut_Individual_RequiresContent(t *testing.T) {
	t.Parallel()

	eng := newIndividualTierEngine(t)
	sockPath, _ := startSocketServer(t, eng)

	var resp opPutResponse
	dialAndRequest(t, sockPath, map[string]any{
		"op":          OpPut,
		"description": "",
	}, &resp)
	if resp.OK {
		t.Fatal("OpPut: expected OK=false for a missing description")
	}
}

// concurrentRequest is a goroutine-SAFE socket round trip: it returns an error
// instead of calling t.Fatalf (which must run on the test goroutine). Used by
// the concurrent test where many goroutines dial at once.
func concurrentRequest(sockPath string, req map[string]any, dst any) error {
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close() //nolint:errcheck
	// Cover the server's bounded OpBuy await plus generous slack under -race.
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return fmt.Errorf("encode: %w", err)
	}
	if err := json.NewDecoder(conn).Decode(dst); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	return nil
}

// TestIndividualTier_ConcurrentOpPutOpBuy_NoFoldCursorCorruption is the
// veracity proof for the dontguess-2b4 fold-cursor fix. It first populates
// inventory with a set of distinct entries, then unleashes a GENUINELY
// CONCURRENT storm (a start barrier releases every goroutine at once) of OpBuys
// (each against a populated task, so each MUST hit) interleaved with fresh
// OpPuts — over the real serve engine over the real socket.
//
// This shape maximizes the exact race the fix closes: every hitting buy makes
// the engine emit a match via appendLocalRecord (the RELATIVE-`++` cursor
// egress), and a concurrent external append that does NOT serialize on localMu
// steals appendLocalRecord's tail slot, desyncing the cursor. The buggy path
// (raw LocalStore.Append) then either LOSES a fold (a buy stalls → OpBuy
// TimedOut; or an operator match/put-accept never dispatches) or DUPLICATES one
// (a second match record for one buy). The assertions catch both, and -race
// catches the unsynchronized localSeen/localDispatched read/write directly.
//
// Assertions (all must hold on EVERY run with the localMu-guarded
// IngestLocalRecord fix):
//  1. Every OpBuy hits its populated task with the exact expected content — no
//     stall (TimedOut), no false miss, no cross-talk.
//  2. The store is well-formed after the storm (ReadAll parses every line).
//  3. Inventory holds exactly (populated + storm-puts) entries — no put fold
//     lost or doubled.
//  4. EXACTLY ONE match record per storm buy id — no lost or duplicated
//     dispatch.
func TestIndividualTier_ConcurrentOpPutOpBuy_NoFoldCursorCorruption(t *testing.T) {
	t.Parallel()

	eng := newIndividualTierEngine(t)
	sockPath, _ := startSocketServer(t, eng)

	// ---- Populate inventory sequentially so every storm buy has a hit. ----
	const populated = 12
	taskFor := func(i int) string {
		return fmt.Sprintf("populated cache entry %02d — %s", i, concurrentTaskDescription(i))
	}
	contentFor := func(i int) []byte {
		return []byte(fmt.Sprintf("cached inference result for entry %02d (%s) — padded body to satisfy the content-size plausibility gate for a 1000 token_cost put, repeated so the buffer is comfortably long enough 0123456789 0123456789 0123456789", i, concurrentTaskDescription(i)))
	}
	for i := 0; i < populated; i++ {
		var putResp opPutResponse
		dialAndRequest(t, sockPath, map[string]any{
			"op":           OpPut,
			"description":  taskFor(i),
			"content":      base64.StdEncoding.EncodeToString(contentFor(i)),
			"token_cost":   int64(1000),
			"content_type": "exchange:content-type:code",
		}, &putResp)
		if !putResp.OK {
			t.Fatalf("populate put %d failed: %s", i, putResp.Error)
		}
	}

	// ---- Concurrent storm: many buys (must hit) + interleaved fresh puts. ----
	const (
		buyGoroutines = 12
		buyRounds     = 6
		putGoroutines = 4
		putRounds     = 6
	)

	var (
		mu        sync.Mutex
		buyIDs    = make(map[string]struct{})
		stormPuts int
		firstErr  error
	)
	recordErr := func(err error) {
		mu.Lock()
		defer mu.Unlock()
		if firstErr == nil {
			firstErr = err
		}
	}

	start := make(chan struct{})
	var wg sync.WaitGroup

	// Buy goroutines: each round buys a populated task (round-robin) and MUST
	// hit with the exact populated content.
	for g := 0; g < buyGoroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			<-start
			for r := 0; r < buyRounds; r++ {
				idx := (g*buyRounds + r) % populated
				var buyResp opBuyResponse
				if err := concurrentRequest(sockPath, map[string]any{
					"op":     OpBuy,
					"task":   taskFor(idx),
					"budget": int64(50000),
				}, &buyResp); err != nil {
					recordErr(fmt.Errorf("buy g%d r%d: request: %w", g, r, err))
					return
				}
				if !buyResp.OK {
					recordErr(fmt.Errorf("buy g%d r%d: failed: %s", g, r, buyResp.Error))
					return
				}
				// A LOST fold manifests here: a stalled buy → TimedOut, or the
				// populated entry vanished from a corrupted rebuild → Miss.
				if buyResp.TimedOut || buyResp.Miss || !buyResp.Matched {
					recordErr(fmt.Errorf("buy g%d r%d (task %d): expected a hit, got matched=%v miss=%v timed_out=%v",
						g, r, idx, buyResp.Matched, buyResp.Miss, buyResp.TimedOut))
					return
				}
				got, derr := base64.StdEncoding.DecodeString(buyResp.Content)
				if derr != nil {
					recordErr(fmt.Errorf("buy g%d r%d: decode: %w", g, r, derr))
					return
				}
				if string(got) != string(contentFor(idx)) {
					recordErr(fmt.Errorf("buy g%d r%d (task %d): content mismatch (cross-talk / duplicated fold?)", g, r, idx))
					return
				}
				mu.Lock()
				buyIDs[buyResp.BuyID] = struct{}{}
				mu.Unlock()
			}
		}(g)
	}

	// Put goroutines: interleave fresh distinct puts into the storm so external
	// appends race the buy-driven appendLocalRecord(match) egress.
	for g := 0; g < putGoroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			<-start
			for r := 0; r < putRounds; r++ {
				task := fmt.Sprintf("storm put g%d r%d", g, r)
				content := []byte(fmt.Sprintf("storm cached inference result g%d r%d — padded body to satisfy the content-size plausibility gate for a 1000 token_cost put, repeated so the buffer is comfortably long enough 0123456789 0123456789", g, r))
				var putResp opPutResponse
				if err := concurrentRequest(sockPath, map[string]any{
					"op":           OpPut,
					"description":  task,
					"content":      base64.StdEncoding.EncodeToString(content),
					"token_cost":   int64(1000),
					"content_type": "exchange:content-type:code",
				}, &putResp); err != nil {
					recordErr(fmt.Errorf("storm put g%d r%d: request: %w", g, r, err))
					return
				}
				if !putResp.OK {
					recordErr(fmt.Errorf("storm put g%d r%d: failed: %s", g, r, putResp.Error))
					return
				}
				mu.Lock()
				stormPuts++
				mu.Unlock()
			}
		}(g)
	}

	close(start)
	wg.Wait()

	if firstErr != nil {
		t.Fatalf("concurrent storm failed (fold-cursor corruption?): %v", firstErr)
	}

	wantBuys := buyGoroutines * buyRounds
	if len(buyIDs) != wantBuys {
		t.Fatalf("expected %d distinct successful buys, got %d", wantBuys, len(buyIDs))
	}
	if wantPuts := putGoroutines * putRounds; stormPuts != wantPuts {
		t.Fatalf("expected %d successful storm puts, got %d", wantPuts, stormPuts)
	}

	// (2) Store integrity.
	recs, err := eng.LocalStore().ReadAll()
	if err != nil {
		t.Fatalf("local store corrupted after concurrent storm: %v", err)
	}

	// (3) Inventory holds exactly one entry per put (populated + storm).
	wantInventory := populated + putGoroutines*putRounds
	if inv := eng.State().Inventory(); len(inv) != wantInventory {
		t.Fatalf("expected exactly %d inventory entries, got %d — put fold lost or duplicated", wantInventory, len(inv))
	}

	// (4) Exactly one match record per storm buy id.
	matchesPerBuy := make(map[string]int)
	for _, rec := range recs {
		if !hasTag(rec.Tags, exchange.TagMatch) {
			continue
		}
		if len(rec.Antecedents) == 0 {
			continue
		}
		buyID := rec.Antecedents[0]
		if _, ours := buyIDs[buyID]; !ours {
			continue
		}
		matchesPerBuy[buyID]++
	}
	if len(matchesPerBuy) != wantBuys {
		t.Fatalf("expected a match record for each of %d buys, found matches for %d — a buy's dispatch was lost", wantBuys, len(matchesPerBuy))
	}
	for buyID, n := range matchesPerBuy {
		if n != 1 {
			t.Fatalf("buy %s has %d match records, want exactly 1 — dispatch was duplicated (fold-cursor desync)", buyID, n)
		}
	}
}

// TestOpListAssigns_Individual_SurfacesOpenCompressAssign proves the
// dontguess-d26 (#2 AGENT DOOR) individual-tier listing path: a real posted
// compression assign (via the exported production eng.PostOpenCompressionAssign
// — the same call the medium loop makes) is discoverable over the operator
// socket via OpListAssigns, with reward/task_type/description intact, and an
// EXCLUSIVE assign targeting a different key is correctly filtered out for a
// caller that is not that key.
func TestOpListAssigns_Individual_SurfacesOpenCompressAssign(t *testing.T) {
	t.Parallel()

	eng := newIndividualTierEngine(t)
	sockPath, _ := startSocketServer(t, eng)

	const tokenCost int64 = 15000
	putID := randomLocalMsgID(t)
	if err := eng.IngestLocalRecord(dgstore.Record{
		ID:         putID,
		CampfireID: "local",
		Sender:     "seller-" + putID[:8],
		Payload:    localPutPayload("assign-list fixture: python asyncio debugging guide", tokenCost),
		Tags:       []string{exchange.TagPut, "exchange:content-type:code", "exchange:domain:python"},
		Timestamp:  time.Now().UnixNano(),
	}); err != nil {
		t.Fatalf("IngestLocalRecord(put): %v", err)
	}
	if err := eng.AutoAcceptPut(putID, tokenCost*70/100, time.Now().UTC().Add(72*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut: %v", err)
	}
	inv := eng.State().Inventory()
	if len(inv) != 1 {
		t.Fatalf("inventory after AutoAcceptPut = %d entries, want 1", len(inv))
	}
	entryID := inv[0].EntryID

	const wantReward = tokenCost * exchange.ColdCompressionBountyPct / 100
	if err := eng.PostOpenCompressionAssign(entryID); err != nil {
		t.Fatalf("PostOpenCompressionAssign: %v", err)
	}

	var resp opListAssignsResponse
	dialAndRequest(t, sockPath, map[string]any{
		"op":         OpListAssigns,
		"caller_key": "anyone-not-exclusive",
	}, &resp)
	if !resp.OK {
		t.Fatalf("OpListAssigns failed: %s", resp.Error)
	}

	var found *assignsListEntry
	for i := range resp.Assigns {
		a := &resp.Assigns[i]
		if a.TaskType == "compress" && a.EntryID == entryID {
			found = a
			break
		}
	}
	if found == nil {
		t.Fatalf("OpListAssigns did not surface the posted compress assign for entry %s; got %+v", short(entryID), resp.Assigns)
	}
	if found.Reward != wantReward {
		t.Errorf("reward = %d, want %d (20%% of token_cost %d)", found.Reward, wantReward, tokenCost)
	}
	if found.ExclusiveSender != "" {
		t.Errorf("exclusive_sender = %q, want empty (cold assign, open to anyone)", found.ExclusiveSender)
	}
	if found.Description == "" {
		t.Error("description is empty — OpListAssigns should surface the original assign message's description")
	}
	if found.Status != "assign-open" {
		t.Errorf("status = %q, want %q", found.Status, "assign-open")
	}
}

func concurrentTaskDescription(i int) string {
	tasks := []string{
		"Rust ownership borrow checker cheat sheet",
		"Python asyncio event loop debugging guide",
		"Kubernetes pod eviction troubleshooting steps",
		"PostgreSQL query plan optimization walkthrough",
		"TypeScript generic constraint inference example",
		"Go context cancellation propagation pattern",
		"React useEffect dependency array pitfalls",
		"SQL window function frame clause reference",
	}
	return tasks[i%len(tasks)]
}
