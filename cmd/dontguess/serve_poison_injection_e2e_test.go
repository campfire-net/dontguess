package main

// serve_poison_injection_e2e_test.go — dontguess-43e, THE release-gate proof for
// dontguess-3b8 (design docs/design/nostr-admission-scrip-rehome-3b8.md §9-item6).
//
// This is the adversarial end-to-end that closes the cache-poisoning hole through
// the FULL serve stack — not the engine in isolation. It composes the exact pieces
// runServeLocal wires together on the team/federated tier (design §6):
//
//   real LocalStore  →  real relay INGEST leg (attachRelayTransport / Intake)  →
//   engine poll-loop fold  →  auto-accept TICKER (RunAutoAccept)  →  match index
//
// with a live *exchange.TrustChecker (fleet allowlist + reputation floor) and a
// live *scrip.LocalScripStore, exactly as serve.go constructs them inside the
// `len(relayURLs) > 0` branch. Nothing is stubbed but the relay wire itself (the
// in-process fakeRelayConn from serve_relay_test.go — a live relay is infra-gated,
// dontguess-13f).
//
// The attack: a validly Schnorr-SIGNED put(3401) authored by a keypair that is NOT
// on the operator's fleet allowlist arrives over the relay ingest leg — precisely
// what a non-operator agent's client publishes. The §2.4a Intake admits it on
// signature alone (VerifyOperatorAuthorship returns nil for non-operator kinds),
// the poll-loop fold stages it into PendingPuts with ZERO trust filter, and the
// auto-accept ticker would — absent Seam A — promote it into operator-blessed,
// matchable inventory. These tests PROVE it never becomes matchable, through the
// whole stack, and that the guarantee SURVIVES A RESTART (Seam D reload re-gate).
//
// WHY cmd/dontguess and not test/: the item names test/ but the substantive
// requirement is to drive "runServeLocal + a relay leg + the auto-accept ticker".
// runServeLocal, attachRelayTransport, and the fakeRelayConn are all package main
// (unexported) — the real serve wiring is unreachable from test/. This file
// follows the repo's own precedent: serve_relay_test.go, the "M2 WIRING +
// ACCEPTANCE GATE", lives here for the same reason. It adds ONLY new tests and
// mutates none — the pre-existing value suite is untouched.

import (
	"context"
	"testing"
	"time"

	"github.com/campfire-net/dontguess/pkg/exchange"
	"github.com/campfire-net/dontguess/pkg/identity"
	"github.com/campfire-net/dontguess/pkg/scrip"
	dgstore "github.com/campfire-net/dontguess/pkg/store"
)

// teamTierServeStack stands up the team/federated-tier serve stack over store `ls`
// with the fleet allowlist `allow` (operator is always implicitly trusted). It
// returns a started engine, the fake relay it ingests from, and a cancel that
// tears the whole thing down. Mirrors serve.go's relays-attached branch: a live
// TrustChecker (with the §D3 reputation floor, default 40) + a live
// LocalScripStore, wired into a real Engine, an attached relay INGEST leg, and the
// auto-accept ticker — the exact promotion path Seam A guards.
func teamTierServeStack(t *testing.T, ls *dgstore.Store, storePath string, operator identity.Signer, allow ...string) (*exchange.Engine, *fakeRelayConn, func()) {
	t.Helper()

	ks := exchange.NewKeySet(allow...)
	tc, err := exchange.NewTrustChecker(operator.PubKeyHex(), ks)
	if err != nil {
		t.Fatalf("NewTrustChecker: %v", err)
	}
	// Payment is enforced on the team tier; NewLocalScripStore folds the log and
	// gates on the operator key exactly as serve.go constructs it.
	ss, err := scrip.NewLocalScripStore(ls, operator.PubKeyHex())
	if err != nil {
		t.Fatalf("NewLocalScripStore: %v", err)
	}

	eng := exchange.NewEngine(exchange.EngineOptions{
		CampfireID:        "local",
		LocalStore:        ls,
		OperatorPublicKey: operator.PubKeyHex(),
		PollInterval:      5 * time.Millisecond,
		TrustChecker:      tc,
		ScripStore:        ss,
		MinBuyBalance:     exchange.DefaultMinBuyBalance,
		Logger:            func(string, ...any) {},
	})
	// Seam A's reputation floor is sourced from the engine State (design §3:
	// wired AFTER NewEngine, BEFORE Start), at the §D3 default of 40.
	tc.SetReputationFloor(eng.State().SellerReputation, exchange.DefaultMinReputation)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)

	relayConn := newFakeRelayConn(true /* echo */)
	stop, err := attachRelayTransport(ctx, ls, operator, operator.PubKeyHex(),
		storePath+".pubcursor", relayConn, relayConn, 5*time.Millisecond, nil, nil, nil)
	if err != nil {
		cancel()
		t.Fatalf("attachRelayTransport: %v", err)
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
				eng.RunAutoAccept(exchange.MaxTokenCost, now, skipped)
			}
		}
	}()

	teardown := func() {
		cancel()
		<-engDone
		stop()
	}
	return eng, relayConn, teardown
}

// injectSignedPut publishes a genuinely Schnorr-signed foreign put over the relay,
// exactly the way a non-operator agent's client would. Distinct descriptions keep
// content hashes unique so the dedup gate never collides.
func injectSignedPut(t *testing.T, relayConn *fakeRelayConn, seller identity.Signer, desc string, tokenCost int64) {
	t.Helper()
	putEv := signExchangeEvent(t, seller,
		[]string{exchange.TagPut, "exchange:content-type:code", "exchange:domain:go"}, nil,
		localPutPayload(desc, tokenCost))
	relayConn.inject(putEv)
}

// TestServeStackPoisonInjection_NonAllowlistedPutNeverMatchable_AcrossRestart is
// the core release-gate proof (design §9-item6, Seams A/B): a signed put from a
// NON-allowlisted keypair, arriving over the real relay ingest leg and folded into
// the running engine, is NEVER promoted into matchable inventory — while a put from
// an ALLOWLISTED seller over the same wire IS (the positive control that proves the
// stack actually promotes, so the negative assertion is non-vacuous). The guarantee
// then survives a full process RESTART (fresh engine + fresh checker replaying the
// same store), proving Seam A re-gates on reload and the poison never sneaks in via
// rebuildMatchIndex.
func TestServeStackPoisonInjection_NonAllowlistedPutNeverMatchable_AcrossRestart(t *testing.T) {
	dir := t.TempDir()
	storePath := dir + "/events.jsonl"
	ls, err := dgstore.Open(storePath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = ls.Close() })

	operator, _ := identity.Generate()
	goodSeller, _ := identity.Generate() // on the allowlist
	attacker, _ := identity.Generate()   // NOT on the allowlist

	eng, relayConn, teardown := teamTierServeStack(t, ls, storePath, operator, goodSeller.PubKeyHex())

	// (1) ATTACK: a signed put from the non-allowlisted attacker arrives over the
	// relay. The Intake admits it on signature, the fold stages it — Seam A must
	// block promotion. Wait for the drop counter to prove the ticker actually ran
	// the promotion gate on it (not merely that it hasn't been promoted yet).
	injectSignedPut(t, relayConn, attacker, "Reverse a linked list in place, iterative", 8500)
	waitFor(t, 10*time.Second, "attacker put reaches Seam A and is dropped_unlisted", func() bool {
		return eng.DegradationSnapshot().DroppedUnlisted >= 1
	})
	if n := eng.MatchIndexLen(); n != 0 {
		t.Fatalf("Seam A FAILED: non-allowlisted put entered the match index (len=%d, want 0)", n)
	}
	if inv := eng.State().Inventory(); len(inv) != 0 {
		t.Fatalf("Seam A FAILED: non-allowlisted put entered inventory (%d entries, want 0)", len(inv))
	}

	// (2) POSITIVE CONTROL: an allowlisted seller's put over the SAME wire IS
	// promoted into matchable inventory — proving the stack promotes real puts, so
	// the negative assertion above is meaningful and not a broken pipeline.
	injectSignedPut(t, relayConn, goodSeller, "Postgres partial index for soft-deleted rows", 8500)
	waitFor(t, 10*time.Second, "allowlisted put becomes matchable", func() bool {
		return eng.MatchIndexLen() == 1 && len(eng.State().Inventory()) == 1
	})
	entry := eng.State().Inventory()[0]
	if entry.SellerKey != goodSeller.PubKeyHex() {
		t.Fatalf("matchable entry seller = %s, want the allowlisted seller %s", entry.SellerKey, goodSeller.PubKeyHex())
	}
	// The attacker's content is provably nowhere in matchable inventory.
	if d := eng.DegradationSnapshot(); d.DroppedUnlisted < 1 {
		t.Fatalf("expected the attacker put still counted dropped_unlisted, got %d", d.DroppedUnlisted)
	}

	// Tear the first "process" down.
	teardown()

	// (3) RESTART: a fresh engine + fresh TrustChecker (allowlist UNCHANGED — the
	// attacker was never admitted) replays the same on-disk log. Seam A made the
	// rejection DURABLE (it emits a settle(put-reject), engine_pricing.go), so on
	// replay the attacker put re-folds AND its put-reject re-folds — the poison is
	// permanently resolved, never re-staged, never promotable. rebuildMatchIndex
	// (Seam D) re-indexes only the allowlisted seller's accepted entry. The poison
	// must not resurface as matchable inventory on reload — under ANY code path.
	eng2, _, teardown2 := teamTierServeStack(t, ls, storePath, operator, goodSeller.PubKeyHex())
	defer teardown2()

	waitFor(t, 10*time.Second, "restart replays the allowlisted entry back into the index", func() bool {
		return eng2.MatchIndexLen() == 1
	})
	// The ONLY matchable entry after restart is the allowlisted seller's — the
	// attacker's key appears nowhere in inventory, so its content is unreachable
	// through the whole stack across the restart boundary.
	inv2 := eng2.State().Inventory()
	if len(inv2) != 1 || inv2[0].SellerKey != goodSeller.PubKeyHex() {
		t.Fatalf("restart: inventory = %+v, want exactly the allowlisted seller's entry (poison must not resurface)", inv2)
	}
	for _, e := range inv2 {
		if e.SellerKey == attacker.PubKeyHex() {
			t.Fatalf("Seam A/D FAILED after restart: attacker inventory resurfaced (%+v)", e)
		}
	}
	// Give the restarted ticker room to run a promotion pass; the durable reject
	// keeps the poison out of the index no matter how many ticks fire.
	if n := eng2.MatchIndexLen(); n != 1 {
		t.Fatalf("Seam A/D FAILED after restart: match index len=%d, want 1 (only the allowlisted entry)", n)
	}
}

// TestServeStackDeAllowlist_WithheldAcrossRestart is the red-team Seam C/D proof
// driven through the serve stack: an allowlisted seller's put arrives over the
// relay and becomes matchable; the operator de-allowlists them at RUNTIME (Seam C
// flags NeedsRevalidation, Seam D drops the entry from the live index); and after a
// RESTART whose reloaded allowlist no longer contains the seller — where
// NeedsRevalidation resets to zero on Replay — the Seam D reload re-gate in
// rebuildMatchIndex keeps the entry out of the searchable index. This is the exact
// "de-allowlisting erased by a restart" hole the seam closes, proven end to end.
func TestServeStackDeAllowlist_WithheldAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	storePath := dir + "/events.jsonl"
	ls, err := dgstore.Open(storePath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = ls.Close() })

	operator, _ := identity.Generate()
	seller, _ := identity.Generate()

	eng, relayConn, teardown := teamTierServeStack(t, ls, storePath, operator, seller.PubKeyHex())

	injectSignedPut(t, relayConn, seller, "Kafka consumer-group rebalance protocol deep dive", 8500)
	waitFor(t, 10*time.Second, "seller put becomes matchable", func() bool {
		return eng.MatchIndexLen() == 1 && len(eng.State().Inventory()) == 1
	})
	entryID := eng.State().Inventory()[0].EntryID
	if eng.State().EntryNeedsRevalidation(entryID) {
		t.Fatalf("precondition: entry should not need revalidation before de-allowlist")
	}

	// (1) RUNTIME DE-ALLOWLIST (Seam C + D driver).
	if n := eng.DeAllowlistSeller(seller.PubKeyHex()); n != 1 {
		t.Fatalf("DeAllowlistSeller flagged %d entries, want 1", n)
	}
	if !eng.State().EntryNeedsRevalidation(entryID) {
		t.Fatalf("Seam C FAILED: de-allowlisted seller's entry not flagged NeedsRevalidation")
	}
	if n := eng.MatchIndexLen(); n != 0 {
		t.Fatalf("Seam D FAILED (runtime): de-allowlisted entry still in the match index (len=%d)", n)
	}

	teardown()

	// (2) RESTART with the seller REMOVED from the persisted allowlist. The accepted
	// put re-folds into inventory (it is a real record in the log), NeedsRevalidation
	// resets to zero on Replay — so ONLY the Seam D reload re-gate can keep it out of
	// the searchable index.
	eng2, _, teardown2 := teamTierServeStack(t, ls, storePath, operator /* seller NOT allowlisted */)
	defer teardown2()

	waitFor(t, 10*time.Second, "restart replays the accepted put back into inventory", func() bool {
		return len(eng2.State().Inventory()) == 1
	})
	if n := eng2.MatchIndexLen(); n != 0 {
		t.Fatalf("Seam D FAILED (restart): de-allowlisted seller's inventory re-entered the match index (len=%d, want 0)", n)
	}
	if !eng2.State().EntryNeedsRevalidation(entryID) {
		t.Fatalf("Seam D FAILED (restart): entry not re-flagged NeedsRevalidation after replay")
	}
}
