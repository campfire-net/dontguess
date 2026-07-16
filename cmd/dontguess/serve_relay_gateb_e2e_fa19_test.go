package main

// serve_relay_gateb_e2e_fa19_test.go — dontguess-fa19 GATE-B ADVERSARIAL
// CERTIFICATION (design §9 Gate B, ADV-2/ADV-16, the named "admit reflects in
// BOTH gates within 1s, no restart" E2E).
//
// The sibling ground-source suites each certify ONE Gate-B outcome in isolation:
//   - allowlist_hotreload_test.go (113/P6): a signed admit mutates the live KeySet
//     + republishes a roster + persists config, against a RECORDER sink (no relay);
//   - serve_relay_roster_c06_test.go (c06/P5): a MANUALLY-injected operator roster
//     folds off the relay into a KeySet;
//   - serve_relay_invite_b80_test.go (b80/P8): invite/redeem one-paste onboard;
//     up_test.go (75a/P7): `up` bootstrap + refuse-mint.
//
// What NO single one of them proves — and what this item owns — is the COMPOSITE
// un-desyncable property end-to-end through a REAL relay round-trip:
//
//	ONE operator action (a single signed OpAllowlist add over the real operator
//	socket) is reflected in BOTH gates, and the two gates CANNOT desync because
//	they are derived from the SAME operator-signed roster bytes:
//	  GATE 1 = the exchange's live enforcement KeySet (the SEAM-A promotion gate);
//	  GATE 2 = the operator-signed kind-30078 roster on the relay that every other
//	           subscriber (a second operator / the relay writePolicy) folds.
//
// TOPOLOGY (nothing stubbed but the websocket wire itself):
//
//	Operator stack A — a REAL exchange.Engine + LocalStore + TrustChecker(ksLive),
//	  a REAL operator IPC socket (listenOperatorSocket + serveOperatorSocket wired
//	  with the production allowlistController), and a REAL attachRelayTransport
//	  reader (WithRosterFolder(rfLive)) over an in-process NIP-01 relay (connA).
//	  The controller's publishRoster is wired EXACTLY as serve.go's is — it fans the
//	  operator-signed roster onto the relay; here that means injecting it back onto
//	  connA (the operator's own subscription round-trip) AND onto connB (the relay
//	  broadcasting it to the second-gate subscriber).
//	Subscriber stack B — an INDEPENDENT KeySet(ksB) folded by a REAL second
//	  attachRelayTransport reader (WithRosterFolder(rfB)) over its own relay leg
//	  (connB). It shares NOTHING with stack A but the pinned operator roster
//	  authority — it stands in for a second operator process / the relay writePolicy
//	  verifying+caching the operator-signed roster off the relay (ADV-2: it trusts
//	  the operator SIGNATURE, never that a relay gated the write).
//
// ASSERTED (adversarially):
//	(1) ONE signed admit → GATE 1 live+synchronous, elapsed < 1s, no restart;
//	(2) the SAME action → GATE 2 converges: stack B's independent fold admits the
//	    same member off the relay; the published roster is a valid operator-signed
//	    kind-30078 admitting the member;
//	(3) UN-DESYNC INVARIANT: ksLive and ksB carry byte-identical membership;
//	(4) round-trip IDEMPOTENCE: the operator's own reader re-folds its published
//	    roster (echo) without corrupting/duplicating membership;
//	(5) GATE-1 BEHAVIOURAL + write-hole: the admit actually opened the exchange gate
//	    (the admitted member's put folds into inventory) while a raw put from a
//	    stranger is dropped_unlisted and NEVER self-admits;
//	(6) FORGED admit (non-operator signature) changes NEITHER gate and publishes NO
//	    roster — the gates cannot be desynced by an unauthorized caller;
//	(7) REMOVE via one signed action drops BOTH gates and re-converges on empty —
//	    the two gates track a single action in both directions.

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/3dl-dev/dontguess/pkg/exchange"
	"github.com/3dl-dev/dontguess/pkg/identity"
	"github.com/3dl-dev/dontguess/pkg/nostr"
	dgstore "github.com/3dl-dev/dontguess/pkg/store"
)

// sortedKeys returns ks.Keys() sorted, for order-independent set comparison.
func sortedKeys(ks *exchange.KeySet) []string {
	k := ks.Keys()
	sort.Strings(k)
	return k
}

// assertSameMembership fails unless a and b are the identical set AND equal want.
// This is the un-desync invariant: gate 1 (ksLive) and gate 2 (ksB) must always
// agree, and agree on the expected membership.
func assertSameMembership(t *testing.T, live, sub *exchange.KeySet, want ...string) {
	t.Helper()
	sort.Strings(want)
	lk := sortedKeys(live)
	sk := sortedKeys(sub)
	eq := func(a, b []string) bool {
		if len(a) != len(b) {
			return false
		}
		for i := range a {
			if a[i] != b[i] {
				return false
			}
		}
		return true
	}
	if !eq(lk, sk) {
		t.Fatalf("GATE DESYNC: live KeySet=%v but subscriber KeySet=%v — the two gates diverged", lk, sk)
	}
	if !eq(lk, want) {
		t.Fatalf("membership=%v, want %v", lk, want)
	}
}

// gatebRosterSubscriber stands up an INDEPENDENT roster-folding stack (gate 2):
// a fresh KeySet + rosterFolder pinned to the operator authority, folded by a real
// attachRelayTransport reader over its own relay leg. Returns the KeySet + the leg
// the operator's publishRoster broadcasts onto.
func newGatebRosterSubscriber(t *testing.T, ctx context.Context, operatorHex string) (*exchange.KeySet, *fakeRelayConn) {
	t.Helper()
	dir := t.TempDir()
	ls, err := dgstore.Open(dir + "/events.jsonl")
	if err != nil {
		t.Fatalf("subscriber store open: %v", err)
	}
	t.Cleanup(func() { _ = ls.Close() })

	ksB := exchange.NewKeySet()
	rfB, err := newRosterFolder(operatorHex, ksB, "", nil)
	if err != nil {
		t.Fatalf("subscriber newRosterFolder: %v", err)
	}
	// A distinct node identity: stack B is a DIFFERENT operator process that merely
	// TRUSTS operatorHex as the fleet's pinned roster authority (that pin lives in
	// rfB). Its own signer is unrelated — sharing nothing with stack A but the pin.
	subSigner, err := identity.Generate()
	if err != nil {
		t.Fatalf("subscriber identity: %v", err)
	}
	connB := newFakeRelayConn(false /* no echo — B only reads what the relay broadcasts */)
	stop, err := attachRelayTransport(ctx, ls, subSigner, subSigner.PubKeyHex(),
		dir+"/events.jsonl.pubcursor", connB, connB, 5*time.Millisecond, nil, nil, nil,
		WithRosterFolder(rfB))
	if err != nil {
		t.Fatalf("subscriber attachRelayTransport: %v", err)
	}
	t.Cleanup(stop)
	return ksB, connB
}

func TestGateB_OneActionAdmit_ReflectedInBothGates_UnDesyncable_fa19(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	operator, err := identity.Generate()
	if err != nil {
		t.Fatalf("operator identity: %v", err)
	}
	opHex := operator.PubKeyHex()

	// --- GATE 2 subscriber (independent) ---
	ksB, connB := newGatebRosterSubscriber(t, ctx, opHex)

	// --- Operator stack A (gate 1 + socket + roster publish) ---
	dgHome := t.TempDir()
	if _, err := exchange.Init(exchange.InitOptions{DGHome: dgHome}); err != nil {
		t.Fatalf("exchange.Init: %v", err)
	}
	lsA, err := dgstore.Open(dgHome + "/events.jsonl")
	if err != nil {
		t.Fatalf("operator store open: %v", err)
	}
	t.Cleanup(func() { _ = lsA.Close() })

	ksLive := exchange.NewKeySet() // empty: the admit must be what fills gate 1
	tc, err := exchange.NewTrustChecker(opHex, ksLive)
	if err != nil {
		t.Fatalf("NewTrustChecker: %v", err)
	}
	// NOTE the engine carries NO OperatorSigner: that arms encryptedRequired (§541
	// §6), which would drop the plaintext puts the gate-1-behavioural check injects.
	// This is the SAME plaintext engine the P5 roster ground-source (c06) uses to
	// prove the SEAM-A admission gate — the trust/promotion gate is byte-identical to
	// team tier; only the orthogonal §6 encryption guard differs. Roster PUBLISHING is
	// done by the allowlistController's own operatorSigner below, so both gates are
	// exercised with a real operator signature regardless.
	eng := exchange.NewEngine(exchange.EngineOptions{
		CampfireID:        "local",
		LocalStore:        lsA,
		OperatorPublicKey: opHex,
		TrustChecker:      tc,
		PollInterval:      5 * time.Millisecond,
		Logger:            func(string, ...any) {},
	})
	rfLive, err := newRosterFolder(opHex, ksLive, "", nil)
	if err != nil {
		t.Fatalf("operator newRosterFolder: %v", err)
	}

	connA := newFakeRelayConn(true /* echo: model the operator's own subscription re-delivering its published roster */)
	stopA, err := attachRelayTransport(ctx, lsA, operator, opHex,
		dgHome+"/events.jsonl.pubcursor", connA, connA, 5*time.Millisecond, nil, nil, nil,
		WithRosterFolder(rfLive))
	if err != nil {
		t.Fatalf("operator attachRelayTransport: %v", err)
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
	t.Cleanup(func() { cancel(); <-engDone; stopA() })

	// publishRoster fans the operator-signed roster to the relay EXACTLY as
	// serve.go's closure does. Here the in-process relay broadcasts it to every
	// subscriber: connA (the operator's own leg — round-trip re-fold) AND connB
	// (the second-gate subscriber). Also captured so the test can inspect the
	// exact bytes both gates derive from.
	var pubMu sync.Mutex
	var publishedRosters []*identity.Event
	publishRoster := func(ev *identity.Event) {
		pubMu.Lock()
		publishedRosters = append(publishedRosters, ev)
		pubMu.Unlock()
		connA.inject(ev)
		connB.inject(ev)
	}
	lastRoster := func() *identity.Event {
		pubMu.Lock()
		defer pubMu.Unlock()
		if len(publishedRosters) == 0 {
			return nil
		}
		return publishedRosters[len(publishedRosters)-1]
	}
	publishedCount := func() int { pubMu.Lock(); defer pubMu.Unlock(); return len(publishedRosters) }

	allowCtrl := &allowlistController{
		keys:           ksLive,
		operatorSigner: operator,
		operatorKeyHex: opHex,
		dgHome:         dgHome,
		publishRoster:  publishRoster,
		nowUnix:        func() int64 { return time.Now().Unix() },
	}

	sockPath := dgHome + "/ipc/operator.sock"
	ln, err := listenOperatorSocket(sockPath)
	if err != nil {
		t.Fatalf("listenOperatorSocket: %v", err)
	}
	sockDone := make(chan struct{})
	go func() { defer close(sockDone); serveOperatorSocket(ctx, ln, eng, allowCtrl) }()
	t.Cleanup(func() { cancel(); <-sockDone })

	member, err := identity.Generate()
	if err != nil {
		t.Fatalf("member identity: %v", err)
	}
	memberHex := member.PubKeyHex()

	// ================= (1) ONE signed admit → GATE 1, < 1s, no restart =========
	addAuth := buildAllowlistAuthEvent(allowlistActionAdd, memberHex, time.Now().Unix())
	if err := identity.SignEvent(operator, addAuth); err != nil {
		t.Fatalf("sign add auth: %v", err)
	}
	var addResp okResponse
	start := time.Now()
	dialAndRequest(t, sockPath, map[string]any{
		"op":               OpAllowlist,
		"allowlist_action": allowlistActionAdd,
		"allowlist_target": memberHex,
		"allowlist_auth":   addAuth,
	}, &addResp)
	elapsed := time.Since(start)

	if !addResp.OK {
		t.Fatalf("legitimate operator admit returned ok=false: %s", addResp.Error)
	}
	// GATE 1 is mutated synchronously inside apply() BEFORE the OK is written — so
	// by the time dialAndRequest returns the admit is already live. No restart, no
	// poll. The whole round trip must be well under the design's 1s budget.
	if !ksLive.Allowed(memberHex) {
		t.Fatalf("GATE 1: the live exchange KeySet did not admit the member after a signed add — hot-reload failed")
	}
	if elapsed >= time.Second {
		t.Fatalf("admit round-trip took %s (>= 1s) — the design requires the admit to reflect within 1s", elapsed)
	}

	// ================= (2) SAME action → GATE 2 converges off the relay =========
	waitFor(t, 8*time.Second, "GATE 2: the independent subscriber folds the operator-signed roster off the relay and admits the member", func() bool {
		return ksB.Allowed(memberHex)
	})
	roster := lastRoster()
	if roster == nil {
		t.Fatal("the admit published no roster — GATE 2 has nothing to derive from")
	}
	if roster.Kind != nostr.KindFleetRoster {
		t.Fatalf("published roster kind = %d, want kind-30078 fleet roster", roster.Kind)
	}
	if err := identity.VerifyEvent(roster); err != nil {
		t.Fatalf("published roster is not a valid signed event: %v", err)
	}
	if roster.PubKey != opHex {
		t.Fatalf("published roster author = %s, want the operator %s", roster.PubKey, opHex)
	}
	if !rosterAdmits(roster, memberHex) {
		t.Fatal("published roster does not admit the member")
	}

	// ================= (3) UN-DESYNC INVARIANT: both gates identical ===========
	assertSameMembership(t, ksLive, ksB, memberHex)

	// ================= (4) round-trip IDEMPOTENCE ==============================
	// The operator's own reader re-folds the echoed roster (connA echo + the direct
	// inject). Give the fold a beat, then assert the live KeySet is still EXACTLY
	// {member} — a ReplaceAll with the identical set must not duplicate or drop.
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if k := ksLive.Keys(); len(k) != 1 {
			t.Fatalf("round-trip re-fold corrupted GATE 1 membership: %v", k)
		}
		time.Sleep(20 * time.Millisecond)
	}
	assertSameMembership(t, ksLive, ksB, memberHex)

	// ================= (5) GATE-1 BEHAVIOURAL + write-hole =====================
	// The admit must have actually OPENED the exchange gate: the admitted member's
	// put folds through SEAM A into matchable inventory. A stranger's raw put over
	// the same relay is dropped_unlisted and NEVER self-admits (a raw exchange write
	// is not an admission path — only the signed operator action is).
	stranger, err := identity.Generate()
	if err != nil {
		t.Fatalf("stranger identity: %v", err)
	}
	beforeUnlisted := eng.DegradationSnapshot().DroppedUnlisted
	strangerPut := signExchangeEvent(t, stranger,
		[]string{exchange.TagPut, "exchange:content-type:code", "exchange:domain:go"}, nil,
		localPutPayload("stranger put that must never self-admit", 8000))
	connA.inject(strangerPut)
	memberPut := signExchangeEvent(t, member,
		[]string{exchange.TagPut, "exchange:content-type:code", "exchange:domain:go"}, nil,
		localPutPayload("admitted member's put — proves gate 1 is really open", 8100))
	connA.inject(memberPut)

	// Both effects are eventual and RunAutoAccept iterates pendingPuts in
	// nondeterministic map order, so wait on EACH independently rather than assuming
	// the member-put fold orders past the stranger-put drop: the admitted member's
	// put must fold into inventory (gate 1 is genuinely open) AND the stranger's raw
	// put must be dropped_unlisted (the write-hole never self-admits a raw write).
	waitFor(t, 10*time.Second, "the admitted member's put folds through SEAM A into inventory (gate 1 is genuinely open)", func() bool {
		return len(eng.State().Inventory()) == 1
	})
	waitFor(t, 10*time.Second, "the stranger's raw put is dropped_unlisted at the SEAM-A promotion gate", func() bool {
		return eng.DegradationSnapshot().DroppedUnlisted > beforeUnlisted
	})
	if ksLive.Allowed(stranger.PubKeyHex()) {
		t.Fatal("a raw put self-admitted the stranger into GATE 1 — the write-hole must admit ONLY the signed operator action")
	}
	if got := len(eng.State().Inventory()); got != 1 {
		t.Fatalf("inventory = %d, want 1 (only the admitted member's put may promote)", got)
	}
	// A raw write never touches the roster, so gate 2 cannot have been desynced.
	if ksB.Allowed(stranger.PubKeyHex()) {
		t.Fatal("the stranger somehow reached GATE 2 — only operator-signed rosters fold")
	}

	// ================= (6) FORGED admit changes NEITHER gate ===================
	attacker, err := identity.Generate()
	if err != nil {
		t.Fatalf("attacker identity: %v", err)
	}
	victim, err := identity.Generate()
	if err != nil {
		t.Fatalf("victim-target identity: %v", err)
	}
	victimHex := victim.PubKeyHex()
	countBeforeForgery := publishedCount()
	forgedAuth := buildAllowlistAuthEvent(allowlistActionAdd, victimHex, time.Now().Unix())
	if err := identity.SignEvent(attacker, forgedAuth); err != nil {
		t.Fatalf("sign forged auth: %v", err)
	}
	var forgedResp okResponse
	dialAndRequest(t, sockPath, map[string]any{
		"op":               OpAllowlist,
		"allowlist_action": allowlistActionAdd,
		"allowlist_target": victimHex,
		"allowlist_auth":   forgedAuth,
	}, &forgedResp)
	if forgedResp.OK {
		t.Fatal("a forged (non-operator-signed) admit returned ok=true — socket reachability must not equal admission (ADV-16)")
	}
	if ksLive.Allowed(victimHex) {
		t.Fatal("a forged admit mutated GATE 1")
	}
	if publishedCount() != countBeforeForgery {
		t.Fatal("a forged admit published a roster — GATE 2 could be desynced by an unauthorized caller")
	}
	if ksB.Allowed(victimHex) {
		t.Fatal("a forged admit reached GATE 2")
	}
	// Both gates unchanged — still exactly {member}.
	assertSameMembership(t, ksLive, ksB, memberHex)

	// ================= (7) REMOVE drops BOTH gates, re-converges ===============
	rmAuth := buildAllowlistAuthEvent(allowlistActionRemove, memberHex, time.Now().Unix())
	if err := identity.SignEvent(operator, rmAuth); err != nil {
		t.Fatalf("sign remove auth: %v", err)
	}
	var rmResp okResponse
	dialAndRequest(t, sockPath, map[string]any{
		"op":               OpAllowlist,
		"allowlist_action": allowlistActionRemove,
		"allowlist_target": memberHex,
		"allowlist_auth":   rmAuth,
	}, &rmResp)
	if !rmResp.OK {
		t.Fatalf("legitimate operator remove returned ok=false: %s", rmResp.Error)
	}
	if ksLive.Allowed(memberHex) {
		t.Fatal("GATE 1: the member was not de-admitted after a signed remove")
	}
	waitFor(t, 8*time.Second, "GATE 2: the subscriber folds the fresh roster and drops the removed member", func() bool {
		return !ksB.Allowed(memberHex)
	})
	rmRoster := lastRoster()
	if rmRoster == nil || rosterAdmits(rmRoster, memberHex) {
		t.Fatal("the remove did not publish a fresh roster omitting the member")
	}
	if err := identity.VerifyEvent(rmRoster); err != nil {
		t.Fatalf("remove roster is not a valid operator-signed event: %v", err)
	}
	// Both gates converge on empty — the two gates track a SINGLE operator action in
	// BOTH directions and cannot desync.
	assertSameMembership(t, ksLive, ksB)
}
