package main

// allowlist_hotreload_race_9ef_test.go — dontguess-9ef GROUND-SOURCE (sweep 7abc,
// MED race): the P6 hot-reload roster republish must reflect the JUST-PERSISTED
// CONFIG membership (the authoritative admit intent held under c.mu), NOT the live
// KeySet — which a concurrent rosterFolder.fold() (running under rf.mu, a DIFFERENT
// mutex) can ReplaceAll out from under apply() between the KeySet mutation and the
// roster build.
//
// THE BUG (pre-fix): apply() did c.keys.Add(K), then buildRoster read c.keys.Keys()
// to build the authoritative kind-30078 roster. A concurrent fold of another leg's
// in-flight PRIOR roster (created_at above the anti-rollback floor but predating this
// admit) ReplaceAll'd the shared KeySet between the Add and the Keys() read, so the
// republished roster OMITTED the just-admitted member (symmetrically re-INCLUDED a
// just-removed one). Because the roster is authoritative-on-fold (serve.go reconciles
// the relay's latest roster via ReplaceAll, OVERRIDING even the config-seeded startup
// KeySet), the wrong membership is DURABLE across a restart — a member the operator
// explicitly admitted is silently de-admitted until a manual re-admit.
//
// DETERMINISTIC INTERLEAVING: buildRoster calls c.nowUnix() and THEN reads the
// membership. We inject the stale fold via the nowUnix seam so it lands in EXACTLY
// the race window the bug requires — after apply()'s Add/Remove, before the roster's
// membership is read. This is a faithful, always-reproducing instantiation of the
// real concurrent interleaving (the fold uses the production rosterFolder.fold on the
// real shared *KeySet — nothing about the fold is stubbed), so reverting the fix
// fails this test every run rather than flakily.
//
// The "durable membership" assertion models the restart reconcile: a fresh KeySet
// seeded from the persisted config (startup) then folded with the republished roster
// (serve.go authoritative-on-fold override) — the exact path that makes the desync
// durable.

import (
	"sync"
	"testing"

	"github.com/3dl-dev/dontguess/pkg/exchange"
	"github.com/3dl-dev/dontguess/pkg/identity"
)

// seedFleetConfig writes dgHome's Config.FleetAllowlist to exactly memberHex.
func seedFleetConfig(t *testing.T, dgHome string, memberHex ...string) {
	t.Helper()
	cfg, err := exchange.LoadConfig(dgHome)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	cfg.FleetAllowlist = append([]string(nil), memberHex...)
	if err := exchange.WriteConfig(exchange.ConfigPath(dgHome), cfg); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
}

// durableMembershipAfterRestart models the restart reconcile that makes a bad roster
// durable: a fresh KeySet seeded from the persisted config (startup seed) is then
// folded with the operator's latest republished roster (serve.go reconciles the
// relay's authoritative roster via ReplaceAll, OVERRIDING the config-seed). The
// resulting KeySet is what enforcement sees after a restart. Uses the PRODUCTION
// rosterFolder.fold and the real config→hex seeding path.
func durableMembershipAfterRestart(t *testing.T, dgHome, operatorKeyHex string, republished *identity.Event) *exchange.KeySet {
	t.Helper()
	cfg, err := exchange.LoadConfig(dgHome)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	allow, err := identity.NewAllowlist(cfg.FleetAllowlist...)
	if err != nil {
		t.Fatalf("NewAllowlist(config): %v", err)
	}
	ks := exchange.NewKeySet(allow.HexKeys()...) // config-seeded startup KeySet
	rf, err := newRosterFolder(operatorKeyHex, ks, "", nil)
	if err != nil {
		t.Fatalf("newRosterFolder: %v", err)
	}
	// The republished roster is the relay's authoritative latest roster on restart;
	// folding it ReplaceAll's the KeySet, overriding the config-seed (serve.go path).
	if !rf.fold(identityToNostrEvent(republished)) {
		t.Fatal("durable reconcile: republished roster failed to fold (not operator-signed?)")
	}
	return ks
}

// TestAllowlistHotReload_RepublishReadsConfigNotLiveKeySet_9ef drives apply() while a
// stale prior roster folds the shared KeySet mid-build, and asserts the republished
// roster — and the durable post-restart membership — reflect the CONFIG intent, not
// the concurrently-clobbered live KeySet.
func TestAllowlistHotReload_RepublishReadsConfigNotLiveKeySet_9ef(t *testing.T) {
	op, err := identity.Generate()
	if err != nil {
		t.Fatalf("Generate operator: %v", err)
	}
	baseline, err := identity.Generate() // an already-admitted fleet member "B"
	if err != nil {
		t.Fatalf("Generate baseline: %v", err)
	}
	member, err := identity.Generate() // the member "K" this test admits/removes
	if err != nil {
		t.Fatalf("Generate member: %v", err)
	}
	baseHex := baseline.PubKeyHex()
	memberHex := member.PubKeyHex()

	// newController builds a controller sharing liveKS with a rosterFolder, wiring the
	// nowUnix seam so the FIRST buildRoster() call folds staleRoster mid-build (the
	// exact race window). Returns the controller, the shared live KeySet, and a
	// capture of every republished roster.
	newController := func(t *testing.T, liveMembers []string, staleRosterMembers []string) (*allowlistController, *exchange.KeySet, *[]*identity.Event) {
		t.Helper()
		dgHome := t.TempDir()
		if _, err := exchange.Init(exchange.InitOptions{DGHome: dgHome}); err != nil {
			t.Fatalf("exchange.Init: %v", err)
		}
		// Config baseline == the live members (the operator's committed pre-change set).
		seedFleetConfig(t, dgHome, liveMembers...)

		liveKS := exchange.NewKeySet(liveMembers...)

		// A stale prior roster from another leg: operator-signed, created_at ABOVE the
		// fresh folder's floor (0) so it folds, but predating this admit. It carries the
		// PRE-CHANGE membership, so folding it ReplaceAll's the live KeySet back to that
		// set — dropping a just-added K (or re-adding a just-removed K).
		staleRoster := signFleetRoster(t, op, 1000, staleRosterMembers)
		rf, err := newRosterFolder(op.PubKeyHex(), liveKS, "", nil)
		if err != nil {
			t.Fatalf("newRosterFolder: %v", err)
		}

		var mu sync.Mutex
		var published []*identity.Event
		var once sync.Once

		ctrl := &allowlistController{
			keys:           liveKS,
			operatorSigner: op,
			operatorKeyHex: op.PubKeyHex(),
			dgHome:         dgHome,
			publishRoster: func(ev *identity.Event) {
				mu.Lock()
				published = append(published, ev)
				mu.Unlock()
			},
			nowUnix: func() int64 {
				// Land the concurrent stale fold in the exact race window: after apply()'s
				// KeySet Add/Remove, before buildRoster reads the membership. Only once.
				once.Do(func() { rf.fold(identityToNostrEvent(staleRoster)) })
				return 2000
			},
		}
		return ctrl, liveKS, &published
	}

	signAuth := func(t *testing.T, action, targetHex string) *identity.Event {
		t.Helper()
		ev := buildAllowlistAuthEvent(action, targetHex, 2000)
		if err := identity.SignEvent(op, ev); err != nil {
			t.Fatalf("SignEvent auth: %v", err)
		}
		return ev
	}

	// --- ADMIT: republished roster + durable membership MUST include K ---
	t.Run("admit_survives_concurrent_stale_fold", func(t *testing.T) {
		// Live + config start with baseline B; the stale prior roster is {B} (pre-admit).
		ctrl, liveKS, published := newController(t, []string{baseHex}, []string{baseHex})

		if err := ctrl.apply(allowlistActionAdd, memberHex, signAuth(t, allowlistActionAdd, memberHex)); err != nil {
			t.Fatalf("apply(add K) returned error: %v", err)
		}

		// Sanity: the injected stale fold DID clobber the live KeySet (K dropped from the
		// concurrently-mutable set) — this is the race the fix must survive. If this ever
		// stops holding, the seam no longer exercises the bug and the test is worthless.
		if liveKS.Allowed(memberHex) {
			t.Fatal("precondition broken: stale fold did not clobber the live KeySet, so the race window is not being exercised")
		}

		if len(*published) != 1 {
			t.Fatalf("expected exactly one republished roster, got %d", len(*published))
		}
		roster := (*published)[0]
		if err := identity.VerifyEvent(roster); err != nil {
			t.Fatalf("republished roster is not a valid operator-signed event: %v", err)
		}
		// THE FIX: the roster reflects config intent {B, K}, not the clobbered live set {B}.
		if !rosterAdmits(roster, memberHex) {
			t.Fatal("BUG (9ef): republished roster OMITS the just-admitted member — a concurrent fold clobbered the KeySet the roster was built from")
		}
		if !rosterAdmits(roster, baseHex) {
			t.Fatal("republished roster dropped the baseline member")
		}

		// DURABLE: post-restart reconcile (config-seed + authoritative roster fold) keeps K.
		durable := durableMembershipAfterRestart(t, ctrl.dgHome, op.PubKeyHex(), roster)
		if !durable.Allowed(memberHex) {
			t.Fatal("BUG (9ef): durable post-restart membership DE-ADMITS a member the operator explicitly added")
		}
		if !durable.Allowed(baseHex) {
			t.Fatal("durable membership dropped the baseline member")
		}
	})

	// --- REMOVE: republished roster + durable membership MUST exclude K ---
	t.Run("remove_survives_concurrent_stale_fold", func(t *testing.T) {
		// Live + config start with {B, K}; the stale prior roster is {B, K} (pre-remove).
		ctrl, liveKS, published := newController(t, []string{baseHex, memberHex}, []string{baseHex, memberHex})

		if err := ctrl.apply(allowlistActionRemove, memberHex, signAuth(t, allowlistActionRemove, memberHex)); err != nil {
			t.Fatalf("apply(remove K) returned error: %v", err)
		}

		// Sanity: the injected stale fold RE-ADDED K to the live KeySet (ReplaceAll back to
		// the pre-remove {B, K}) — the symmetric race the fix must survive.
		if !liveKS.Allowed(memberHex) {
			t.Fatal("precondition broken: stale fold did not re-add K to the live KeySet, so the race window is not being exercised")
		}

		if len(*published) != 1 {
			t.Fatalf("expected exactly one republished roster, got %d", len(*published))
		}
		roster := (*published)[0]
		if err := identity.VerifyEvent(roster); err != nil {
			t.Fatalf("republished roster is not a valid operator-signed event: %v", err)
		}
		// THE FIX: the roster reflects config intent {B}, not the clobbered live set {B, K}.
		if rosterAdmits(roster, memberHex) {
			t.Fatal("BUG (9ef): republished roster RE-INCLUDES the just-removed member — a concurrent fold re-added it to the KeySet the roster was built from")
		}
		if !rosterAdmits(roster, baseHex) {
			t.Fatal("republished roster dropped the baseline member")
		}

		// DURABLE: post-restart reconcile keeps K OUT.
		durable := durableMembershipAfterRestart(t, ctrl.dgHome, op.PubKeyHex(), roster)
		if durable.Allowed(memberHex) {
			t.Fatal("BUG (9ef): durable post-restart membership RE-ADMITS a member the operator explicitly removed")
		}
		if !durable.Allowed(baseHex) {
			t.Fatal("durable membership dropped the baseline member")
		}
	})
}
