package main

// serve_poison_injection_e2e_test.go — dontguess-43e, THE release-gate proof for
// dontguess-3b8 (design docs/design/nostr-admission-scrip-rehome-3b8.md §9-item6).
//
// This is the adversarial end-to-end that closes the cache-poisoning hole through
// the FULL serve stack — not the engine in isolation. It composes the exact pieces
// runServeLocal wires together on the team/federated tier (design §6):
//
//   real LocalStore  →  real relay INGEST leg (Intake.HandleEvent)  →
//   engine poll-loop fold  →  auto-accept promotion gate (RunAutoAccept)  →  match index
//
// with a live *exchange.TrustChecker (fleet allowlist + reputation floor) and a
// live *scrip.LocalScripStore, exactly as serve.go constructs them inside the
// `len(relayURLs) > 0` branch. Nothing is stubbed but the relay wire itself (the
// in-process fakeRelayConn from serve_relay_test.go — a live relay is infra-gated,
// dontguess-13f).
//
// Those FOUR components are the identical production types; the relayIngestPump
// drives them SYNCHRONOUSLY (relay Intake.HandleEvent → engine PollLocalStoreForTest
// → RunAutoAccept), and StartupReplayForTest brings a restarted engine to its
// post-Start state deterministically. This replaces the earlier version's three
// starved background tickers (runReader / eng.Start / auto-accept) + 10s waitFor
// polls, which FALSE-failed under -race + full-suite CPU load — either starved past
// the deadline, or observing one leg's partial output and racing ahead of the next
// (the restart replay-fold-vs-index-re-gate window). Same de-flake class as
// dontguess-c12, now closed through the async relay ingest leg (dontguess-c84).
// Every assertion is preserved; only the async goroutines + wall-clock waits are gone.
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
	"testing"
	"time"

	"github.com/3dl-dev/dontguess/pkg/exchange"
	"github.com/3dl-dev/dontguess/pkg/identity"
	"github.com/3dl-dev/dontguess/pkg/scrip"
	dgstore "github.com/3dl-dev/dontguess/pkg/store"
)

// relayIngestPump drives a foreign put through the EXACT three legs the team-tier
// serve stack chains — the relay INGEST Intake, the engine poll-loop fold, and the
// auto-accept promotion gate (Seam A) — but SYNCHRONOUSLY, so an injected put is
// deterministically folded+promoted before the pump returns, with no ticker/
// wall-clock wait (dontguess-c84). Each of the three legs is the identical
// production component; only the async runReader/poll/auto-accept GOROUTINES are
// removed. The prior version launched all three as starved goroutines and polled a
// 10s waitFor against them: under -race + full-suite CPU saturation those
// goroutines were scheduled past the deadline, or the test observed one leg's
// partial output and raced ahead of the next (the restart Inventory-vs-index
// re-gate window), FALSE-failing (dontguess-c84, same class as dontguess-c12 but
// through the async relay ingest leg, which had no synchronous seam before this).
type relayIngestPump struct {
	t       *testing.T
	eng     *exchange.Engine
	wiring  *relayWiring
	skipped map[string]struct{}
}

// injectSignedPut synthesizes a genuinely Schnorr-signed foreign put exactly as a
// non-operator agent's client would publish it, then drives it through the three
// ingest legs synchronously. Distinct descriptions keep content hashes unique so
// the dedup gate never collides. After it returns, the engine's inventory, match
// index, and degradation counters reflect the promotion decision deterministically.
func (p *relayIngestPump) injectSignedPut(seller identity.Signer, desc string, tokenCost int64) {
	p.t.Helper()
	putEv := signExchangeEvent(p.t, seller,
		[]string{exchange.TagPut, "exchange:content-type:code", "exchange:domain:go"}, nil,
		localPutPayload(desc, tokenCost))

	// LEG 1 — RELAY INGEST. HandleEvent is the SYNCHRONOUS seam runReader calls per
	// wire frame (§2.4a): universal signature floor -> adapter -> operator-authorship
	// gate -> Sequencer IngestLive -> Drain -> LocalStore.BatchAppend(Origin="relay").
	// A put has no antecedents, so it drains and persists to LocalStore before this
	// returns — driving it directly removes the async runReader goroutine from the
	// path while exercising the identical admission pipeline.
	if err := p.wiring.intake.HandleEvent(identityToNostrEvent(putEv)); err != nil {
		p.t.Fatalf("relay Intake.HandleEvent: %v", err)
	}

	// LEG 2 — ENGINE POLL-LOOP FOLD. The exact body runLocal's ticker runs
	// (pollLocalStore): Replay -> incremental fold, which stages the relay-origin
	// put into PendingPuts (state_put.go applyPut, ZERO trust filter — Seam A is the
	// real choke below), and dispatches it through the real dispatch path.
	if err := p.eng.PollLocalStoreForTest(); err != nil {
		p.t.Fatalf("poll+fold: %v", err)
	}

	// LEG 3 — AUTO-ACCEPT PROMOTION GATE (Seam A). The identical body the auto-accept
	// ticker runs. A non-allowlisted seller is counted (dropped_unlisted) and its put
	// durably rejected (put-reject) so it never enters matchable inventory; an
	// allowlisted seller's put is promoted (put-accept) and indexed. autoAcceptPutLocked
	// applies its own operator record synchronously, so State + the match index are
	// current the instant this returns.
	p.eng.RunAutoAccept(exchange.MaxTokenCost, time.Now(), p.skipped)
}

// teamTierServeStack stands up the team/federated-tier serve stack over store `ls`
// with the fleet allowlist `allow` (operator is always implicitly trusted). It
// returns the engine, a synchronous relayIngestPump over the real relay Intake, and
// a teardown. Mirrors serve.go's relays-attached branch: a live TrustChecker (with
// the §D3 reputation floor, default 40) + a live LocalScripStore, wired into a real
// Engine, the real relay INGEST leg (its Intake), and the real auto-accept
// promotion gate (RunAutoAccept) — the exact promotion path Seam A guards.
//
// The stack is driven SYNCHRONOUSLY (dontguess-c84): a deterministic
// StartupReplayForTest brings the engine to the same post-Start state (folding the
// log AND running the Seam D reload re-gate, rebuildMatchIndex) instead of racing a
// waitFor against an eng.Start goroutine, and the pump drives ingest/fold/promote
// inline instead of via three starved tickers. No async goroutine sits on the
// ingest->promote path, so no wall-clock deadline can be starved past. The Outbox
// publish leg is intentionally omitted: these tests assert only on inventory / the
// match index / degradation counters, never on what reaches the relay wire.
func teamTierServeStack(t *testing.T, ls *dgstore.Store, storePath string, operator identity.Signer, allow ...string) (*exchange.Engine, *relayIngestPump, func()) {
	return teamTierServeStackRevoked(t, ls, storePath, operator, nil, allow...)
}

// teamTierServeStackRevoked is teamTierServeStack plus a durable revocation
// tombstone set (dontguess-23c) — mirroring how serve loads Config.RevokedSellers
// at startup so SEAM D withholds exactly the revoked-for-cause sellers on restart.
func teamTierServeStackRevoked(t *testing.T, ls *dgstore.Store, storePath string, operator identity.Signer, revoked []string, allow ...string) (*exchange.Engine, *relayIngestPump, func()) {
	t.Helper()

	ks := exchange.NewKeySet(allow...)
	tc, err := exchange.NewTrustChecker(operator.PubKeyHex(), ks)
	if err != nil {
		t.Fatalf("NewTrustChecker: %v", err)
	}
	tc.SetRevoked(revoked...)
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

	// SYNCHRONOUS startup replay — the same pre-run-loop body Start runs: fold the
	// full log into State AND rebuildMatchIndex (Seam D reload re-gate). On a fresh
	// store this is a no-op; on a restart over a populated log it deterministically
	// re-folds inventory and re-gates the index in one call, so the restart
	// assertions below observe a fully-settled state (no replay-vs-re-gate race).
	if err := eng.StartupReplayForTest(); err != nil {
		t.Fatalf("StartupReplayForTest: %v", err)
	}

	// Build ONLY the relay INGEST wiring (its Intake) — the synchronous seam the
	// pump drives. nopPublisher + no runReader/Outbox goroutines: nothing async runs.
	wiring, _, err := buildRelayWiring(ls, operator, operator.PubKeyHex(),
		storePath+".pubcursor", nopPublisher{}, 0, nil, nil)
	if err != nil {
		t.Fatalf("buildRelayWiring: %v", err)
	}

	pump := &relayIngestPump{t: t, eng: eng, wiring: wiring, skipped: map[string]struct{}{}}
	teardown := func() {} // fully synchronous — no goroutines to stop
	return eng, pump, teardown
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

	eng, pump, teardown := teamTierServeStack(t, ls, storePath, operator, goodSeller.PubKeyHex())

	// (1) ATTACK: a signed put from the non-allowlisted attacker arrives over the
	// relay. The Intake admits it on signature, the fold stages it — Seam A must
	// block promotion. The pump drives the ingest+fold+promotion gate synchronously,
	// so after it returns Seam A has provably run on the put; assert the drop counter
	// deterministically (not "hasn't been promoted yet" — the gate ran and rejected).
	pump.injectSignedPut(attacker, "Reverse a linked list in place, iterative", 8500)
	if got := eng.DegradationSnapshot().DroppedUnlisted; got < 1 {
		t.Fatalf("attacker put must reach Seam A and be dropped_unlisted, got DroppedUnlisted=%d", got)
	}
	if n := eng.MatchIndexLen(); n != 0 {
		t.Fatalf("Seam A FAILED: non-allowlisted put entered the match index (len=%d, want 0)", n)
	}
	if inv := eng.State().Inventory(); len(inv) != 0 {
		t.Fatalf("Seam A FAILED: non-allowlisted put entered inventory (%d entries, want 0)", len(inv))
	}

	// (2) POSITIVE CONTROL: an allowlisted seller's put over the SAME wire IS
	// promoted into matchable inventory — proving the stack promotes real puts, so
	// the negative assertion above is meaningful and not a broken pipeline.
	pump.injectSignedPut(goodSeller, "Postgres partial index for soft-deleted rows", 8500)
	if n, inv := eng.MatchIndexLen(), len(eng.State().Inventory()); n != 1 || inv != 1 {
		t.Fatalf("allowlisted put must become matchable: match index len=%d, inventory=%d, want 1 and 1", n, inv)
	}
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

	// StartupReplayForTest (inside teamTierServeStack) already ran the full replay +
	// Seam D re-gate synchronously, so the index is settled the instant the stack is
	// built — assert directly rather than racing an eng2.Start goroutine.
	if n := eng2.MatchIndexLen(); n != 1 {
		t.Fatalf("restart must replay the allowlisted entry back into the index: match index len=%d, want 1", n)
	}
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
	// Drive an explicit promotion pass on the restarted engine; the durable reject
	// (re-folded on replay) leaves the attacker put non-pending, so no auto-accept
	// tick can ever re-promote it — the poison stays out of the index.
	eng2.RunAutoAccept(exchange.MaxTokenCost, time.Now(), map[string]struct{}{})
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

	eng, pump, teardown := teamTierServeStack(t, ls, storePath, operator, seller.PubKeyHex())

	pump.injectSignedPut(seller, "Kafka consumer-group rebalance protocol deep dive", 8500)
	if n, inv := eng.MatchIndexLen(), len(eng.State().Inventory()); n != 1 || inv != 1 {
		t.Fatalf("seller put must become matchable: match index len=%d, inventory=%d, want 1 and 1", n, inv)
	}
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
	// Restart with the seller NOT allowlisted AND on the durable revocation
	// tombstone (dontguess-23c) — exactly what serve persists+loads after a
	// de-allowlist. SEAM D withholds a revoked seller's inventory; an ordinary
	// unadmitted seller's inventory is retained.
	eng2, _, teardown2 := teamTierServeStackRevoked(t, ls, storePath, operator, []string{seller.PubKeyHex()} /* revoked */)
	defer teardown2()

	// StartupReplayForTest re-folded the accepted put back into inventory AND ran the
	// Seam D re-gate (rebuildMatchIndex) synchronously in one call, so inventory and
	// the index/revalidation flag are consistent the instant the stack is built. The
	// prior async version raced a waitFor on Inventory()==1 against the eng2.Start
	// goroutine and could observe inventory populated in the window BEFORE the re-gate
	// flagged the entry — FALSE-failing line "entry not re-flagged NeedsRevalidation"
	// (dontguess-c84). Assert all three facts directly now.
	if inv := len(eng2.State().Inventory()); inv != 1 {
		t.Fatalf("restart must replay the accepted put back into inventory: inventory=%d, want 1", inv)
	}
	if n := eng2.MatchIndexLen(); n != 0 {
		t.Fatalf("Seam D FAILED (restart): de-allowlisted seller's inventory re-entered the match index (len=%d, want 0)", n)
	}
	if !eng2.State().EntryNeedsRevalidation(entryID) {
		t.Fatalf("Seam D FAILED (restart): entry not re-flagged NeedsRevalidation after replay")
	}
}
