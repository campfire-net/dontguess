package main

// serve_relay_roster_antirollback_61a8_test.go — dontguess-61a8 GROUND-SOURCE
// (security fix, hardens the c06 P5 roster fold).
//
// THREAT (stale-roster-replay, ADV-2): the rosterFolder created_at anti-rollback
// guard (fold: ev.CreatedAt < rf.lastCreated) was IN-MEMORY only — lastCreated reset
// to 0 on every operator restart. So after a restart, a stale/lagging/malicious relay
// could serve an OLD (still validly operator-signed) roster that re-admits a
// previously-REMOVED key, and the guard — starting from 0 — would accept it,
// RE-ADMITTING the removed key to the allowlisted tier. That made roster freshness
// depend on RELAY HONESTY, the exact trust the design forbids.
//
// THE FIX persists the latest applied roster created_at to a durable per-DG_HOME
// sidecar (temp+fsync+rename, mirroring the Outbox/Intake cursors) and SEEDS
// rf.lastCreated from it on startup, so the anti-rollback floor survives a restart
// independent of the relay.
//
// This drives the REAL serve path across a simulated RESTART:
//
//	Lifetime 1 (establish the floor): a real attachRelayTransport reader folds an
//	  operator-signed roster admitting K, then a FRESHER roster removing K (admitting
//	  S). The durable floor sidecar advances to the removal roster's created_at.
//	Lifetime 2 (RESTART): a FRESH KeySet + a FRESH rosterFolder built over the SAME
//	  sidecar (its floor re-read from DISK, not memory) + a fresh engine sharing the
//	  store. A stale relay leg re-serves the OLD pre-removal roster (which validly
//	  admits K). The persisted floor REJECTS it, and K's subsequent put is STILL
//	  dropped_unlisted — K stays removed across the restart, independent of the relay.
//
// Nothing is stubbed but the websocket wire (the in-process fake relay). The fold,
// the created_at guard, the temp+fsync+rename persistence, the disk round-trip on
// restart, and the SEAM-A dropped_unlisted are all production code.

import (
	"context"
	"testing"
	"time"

	"github.com/3dl-dev/dontguess/pkg/exchange"
	"github.com/3dl-dev/dontguess/pkg/identity"
	dgstore "github.com/3dl-dev/dontguess/pkg/store"
)

func TestRelayRosterAntiRollback_SurvivesRestart_61a8(t *testing.T) {
	dir := t.TempDir()
	storePath := dir + "/events.jsonl"
	ls, err := dgstore.Open(storePath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = ls.Close() })

	operator, _ := identity.Generate()
	sellerK, _ := identity.Generate()  // admitted, then removed
	sellerS, _ := identity.Generate()  // stays admitted through the removal roster
	sellerS2, _ := identity.Generate() // lifetime-2 ordering sentinel

	// The durable anti-rollback floor sidecar — the ONE piece of state that must
	// survive the restart. Shared by both lifetimes' rosterFolders exactly as the
	// per-DG_HOME rosterCursorPath(dgHome) would be across a real operator restart.
	cursorPath := rosterCursorPath(dir)

	base := time.Now().Unix()

	// ============================ LIFETIME 1 ============================
	// Fold admit-K, then remove-K (fresher). The floor advances + persists to disk.
	func() {
		ks := exchange.NewKeySet()
		rf, ferr := newRosterFolder(operator.PubKeyHex(), ks, cursorPath, nil)
		if ferr != nil {
			t.Fatalf("lifetime1 newRosterFolder: %v", ferr)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		conn := newFakeRelayConn(true /* echo */)
		stop, aerr := attachRelayTransport(ctx, ls, operator, operator.PubKeyHex(),
			dir+"/pubcursor1", conn, conn, 5*time.Millisecond, nil, nil, nil,
			WithRosterFolder(rf))
		if aerr != nil {
			t.Fatalf("lifetime1 attachRelayTransport: %v", aerr)
		}
		defer func() { cancel(); stop() }()

		// admit K
		conn.inject(signFleetRoster(t, operator, base, []string{sellerK.PubKeyHex()}))
		waitFor(t, 8*time.Second, "lifetime1: roster admits K", func() bool {
			return ks.Allowed(sellerK.PubKeyHex())
		})
		// remove K (fresher roster, admits only S) — this is the created_at the floor
		// must remember across the restart.
		conn.inject(signFleetRoster(t, operator, base+10, []string{sellerS.PubKeyHex()}))
		waitFor(t, 8*time.Second, "lifetime1: fresher roster removes K, admits S", func() bool {
			return !ks.Allowed(sellerK.PubKeyHex()) && ks.Allowed(sellerS.PubKeyHex())
		})
	}()

	// The floor is durably on disk at the removal roster's created_at. This is what a
	// fresh process reads on restart — proven directly here and via rf2.lastCreated
	// below.
	if got, rerr := readRosterCreatedAt(cursorPath); rerr != nil {
		t.Fatalf("read persisted floor: %v", rerr)
	} else if got != base+10 {
		t.Fatalf("persisted anti-rollback floor = %d, want %d (removal roster created_at)", got, base+10)
	}

	// ============================ LIFETIME 2 (RESTART) ============================
	// Fresh KeySet (a restarted operator's config allowlist does NOT contain the
	// roster-admitted K), fresh rosterFolder seeded from the DURABLE floor, fresh
	// engine sharing the same store.
	ks2 := exchange.NewKeySet()
	tc2, terr := exchange.NewTrustChecker(operator.PubKeyHex(), ks2)
	if terr != nil {
		t.Fatalf("lifetime2 NewTrustChecker: %v", terr)
	}
	rf2, ferr := newRosterFolder(operator.PubKeyHex(), ks2, cursorPath, nil)
	if ferr != nil {
		t.Fatalf("lifetime2 newRosterFolder: %v", ferr)
	}
	// The floor was re-read from DISK by a brand-new struct — NOT carried in memory
	// from lifetime 1. This is the exact defect the fix closes (lastCreated used to
	// reset to 0 here).
	if rf2.lastCreated != base+10 {
		t.Fatalf("restart seeded floor = %d, want %d — the anti-rollback floor did not survive the restart (in-memory reset to 0 is the bug)", rf2.lastCreated, base+10)
	}

	eng := exchange.NewEngine(exchange.EngineOptions{
		CampfireID:        "local",
		LocalStore:        ls,
		OperatorPublicKey: operator.PubKeyHex(),
		TrustChecker:      tc2,
		PollInterval:      5 * time.Millisecond,
		Logger:            func(string, ...any) {},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn := newFakeRelayConn(true /* echo */)
	stop, aerr := attachRelayTransport(ctx, ls, operator, operator.PubKeyHex(),
		dir+"/pubcursor2", conn, conn, 5*time.Millisecond, nil, nil, nil,
		WithRosterFolder(rf2))
	if aerr != nil {
		t.Fatalf("lifetime2 attachRelayTransport: %v", aerr)
	}
	engDone := make(chan struct{})
	go func() { defer close(engDone); _ = eng.Start(ctx) }()
	go func() {
		skipped := map[string]struct{}{}
		tk := time.NewTicker(10 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-tk.C:
				eng.RunAutoAccept(1_000_000, now, skipped)
			}
		}
	}()
	t.Cleanup(func() { cancel(); <-engDone; stop() })

	// A STALE/MALICIOUS relay re-serves the OLD pre-removal roster (created_at=base <
	// floor base+10) that validly admits K. Then a VALID sentinel roster (created_at
	// base+20 > floor) admitting S2. Because the single reader processes events in
	// order, once S2 is admitted the stale roster before it is GUARANTEED already
	// folded (and rejected by the persisted floor) — so a still-false Allowed(K) is a
	// real rejection, not an unprocessed-event false pass.
	conn.inject(signFleetRoster(t, operator, base, []string{sellerK.PubKeyHex()}))
	conn.inject(signFleetRoster(t, operator, base+20, []string{sellerS2.PubKeyHex()}))
	waitFor(t, 8*time.Second, "lifetime2: valid sentinel roster {S2} folds (orders past the stale replay)", func() bool {
		return ks2.Allowed(sellerS2.PubKeyHex())
	})
	if ks2.Allowed(sellerK.PubKeyHex()) {
		t.Fatalf("stale pre-removal roster RE-ADMITTED K after restart — the persisted anti-rollback floor failed; roster freshness fell back to relay honesty (ADV-2 violated)")
	}

	// GROUND-SOURCE: K's subsequent put is STILL dropped_unlisted — K stays removed
	// across the restart, independent of the relay.
	before := eng.DegradationSnapshot().DroppedUnlisted
	putK := signExchangeEvent(t, sellerK,
		[]string{exchange.TagPut, "exchange:content-type:code", "exchange:domain:go"}, nil,
		localPutPayload("Go HTTP handler unit test generator (K, post-restart)", 8000))
	conn.inject(putK)
	waitFor(t, 8*time.Second, "lifetime2: removed K's post-restart put is dropped_unlisted", func() bool {
		return eng.DegradationSnapshot().DroppedUnlisted > before
	})
	if got := len(eng.State().Inventory()); got != 0 {
		t.Fatalf("inventory = %d after removed-K's post-restart put, want 0 (K must never promote across the restart)", got)
	}
}
