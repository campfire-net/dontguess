package main

// serve_relay_invite_b80_test.go — dontguess-b80 GROUND-SOURCE (design §1/§9 Gate
// B/P8, ADV-15).
//
// The OPERATOR-SIDE invite/redeem: a member publishes a kind-3410 redeem event
// (signed by a fresh member key, embedding an operator-signed dgi1_ invite) to an
// OPEN relay; the operator's serve reader does 100% of the verification and, on a
// valid redeem, PROMOTES the member into the live fleet KeySet + roster (via the
// SAME allowlistController `dontguess allowlist add` uses) and MINTS the genesis
// grant. Driven through the REAL serve-path wiring — a real exchange.Engine sharing
// its LocalStore with the real attachRelayTransport reader (redeemHandler + Intake +
// Outbox + Sequencer), a live TrustChecker whose KeySet the SEAM-A gate enforces,
// and a live LocalScripStore — over the same in-process fake relay (an OPEN
// NIP-01 relay: it forwards whatever is injected) the rest of the serve_relay suite
// uses. Nothing is stubbed but the websocket wire itself.
//
// It asserts the ground-source properties (operator side — the relay write-hole
// rate-cap + non-3410 edge drop is dontguess-ef1, optional/closed-relay only):
//
//	ACCEPT: a single valid redeem admits the fresh member into the KeySet AND mints
//	        the genesis grant — one paste onboards end-to-end.
//	REJECT: a redeem embedding a FOREIGN-operator-signed invite (PIN mismatch), and
//	        a redeem of an EXPIRED invite, change the fleet by NOTHING.
//	WRITE-HOLE SCOPE: a NON-3410 event (a put) from the un-admitted member does NOT
//	        self-admit — the only path in is the redeem+operator-promote.
//	REPLAY-AFTER-RESTART: replaying an already-redeemed grant AFTER the operator
//	        process is torn down and rebuilt is rejected (redeemed-ids persisted to
//	        disk) and the genesis grant is NOT double-minted.

import (
	"context"
	"testing"
	"time"

	"github.com/3dl-dev/dontguess/pkg/exchange"
	"github.com/3dl-dev/dontguess/pkg/identity"
	"github.com/3dl-dev/dontguess/pkg/nostr"
	"github.com/3dl-dev/dontguess/pkg/scrip"
	dgstore "github.com/3dl-dev/dontguess/pkg/store"
)

// inviteStack is one running operator process (engine + relay reader with the
// redeem handler wired) for the b80 ground-source test.
type inviteStack struct {
	ks    *exchange.KeySet
	eng   *exchange.Engine
	scrip *scrip.LocalScripStore
	conn  *fakeRelayConn
	stop  func()
}

// newInviteStack builds and starts an operator stack over ls with `operator`,
// seeding the KeySet with `seed` members. redeemedPath is the DURABLE redeemed-id
// log (shared across restarts to prove persistence). dgHome backs the config the
// promotion path persists to.
func newInviteStack(t *testing.T, ctx context.Context, dgHome string, ls *dgstore.Store, operator *identity.Secp256k1Identity, redeemedPath string, seed ...string) *inviteStack {
	t.Helper()

	ks := exchange.NewKeySet(seed...)
	tc, err := exchange.NewTrustChecker(operator.PubKeyHex(), ks)
	if err != nil {
		t.Fatalf("NewTrustChecker: %v", err)
	}
	ss, err := scrip.NewLocalScripStore(ls, operator.PubKeyHex())
	if err != nil {
		t.Fatalf("NewLocalScripStore: %v", err)
	}

	eng := exchange.NewEngine(exchange.EngineOptions{
		CampfireID:        "local",
		LocalStore:        ls,
		OperatorPublicKey: operator.PubKeyHex(),
		OperatorSigner:    operator,
		TrustChecker:      tc,
		ScripStore:        ss,
		PollInterval:      5 * time.Millisecond,
		Logger:            func(string, ...any) {},
	})
	tc.SetReputationFloor(eng.State().SellerReputation, exchange.DefaultMinReputation)

	rf, err := newRosterFolder(operator.PubKeyHex(), ks, "", nil)
	if err != nil {
		t.Fatalf("newRosterFolder: %v", err)
	}
	// publishRoster is nil: the KeySet mutation + config persist are the observable
	// promotion effects here; a relay roster republish is exercised by the c06/113
	// suites and is not what b80 verifies.
	ctrl := &allowlistController{
		keys:           ks,
		operatorSigner: operator,
		operatorKeyHex: operator.PubKeyHex(),
		dgHome:         dgHome,
		nowUnix:        func() int64 { return time.Now().Unix() },
	}
	rh, err := newRedeemHandler(operator.PubKeyHex(), ctrl, eng, redeemedPath, nil)
	if err != nil {
		t.Fatalf("newRedeemHandler: %v", err)
	}

	conn := newFakeRelayConn(true /* echo */)
	stop, err := attachRelayTransport(ctx, ls, operator, operator.PubKeyHex(),
		dgHome+"/events.jsonl.pubcursor", conn, conn, 5*time.Millisecond, nil, nil, nil,
		WithRosterFolder(rf), WithRedeemHandler(rh))
	if err != nil {
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
				eng.RunAutoAccept(1_000_000, now, skipped)
			}
		}
	}()

	s := &inviteStack{ks: ks, eng: eng, scrip: ss, conn: conn}
	s.stop = func() { <-engDone; stop() }
	return s
}

// writeInviteTestConfig writes the minimal operator config the live promotion path
// (persistFleetAllowlistChange) needs on disk.
func writeInviteTestConfig(t *testing.T, dgHome string, operator *identity.Secp256k1Identity) {
	t.Helper()
	if err := exchange.WriteConfig(exchange.ConfigPath(dgHome), &exchange.Config{
		OperatorKeyHex: operator.PubKeyHex(),
		OperatorNpub:   operator.Npub(),
		CreatedAt:      time.Now().UnixNano(),
	}); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
}

// mustRedeem builds a fresh member key and its kind-3410 redeem embedding a token
// operator-signed with `tokenSigner` (== operator for a genuine invite; a foreign
// key for the forgery case). genesis/now/expiry parameterise the invite.
func mustRedeem(t *testing.T, tokenSigner *identity.Secp256k1Identity, name, grant string, genesis, now, expiry int64) (*identity.Secp256k1Identity, *identity.Event) {
	t.Helper()
	tok, err := nostr.BuildInviteToken(tokenSigner, name, grant, nil, genesis, now, expiry)
	if err != nil {
		t.Fatalf("BuildInviteToken: %v", err)
	}
	in, err := nostr.ParseInviteToken(tok)
	if err != nil {
		t.Fatalf("ParseInviteToken: %v", err)
	}
	member, err := identity.Generate()
	if err != nil {
		t.Fatalf("Generate member: %v", err)
	}
	redeemEv, err := nostr.BuildRedeemEvent(member, in, now)
	if err != nil {
		t.Fatalf("BuildRedeemEvent: %v", err)
	}
	return member, redeemEv
}

func TestInviteRedeem_OnboardsEndToEnd_b80(t *testing.T) {
	dir := t.TempDir()
	operator, _ := identity.Generate()
	writeInviteTestConfig(t, dir, operator)

	ls, err := dgstore.Open(dir + "/events.jsonl")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = ls.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	redeemedPath := redeemedInvitesPath(dir)
	s := newInviteStack(t, ctx, dir, ls, operator, redeemedPath)
	t.Cleanup(func() { cancel(); s.stop() })

	now := time.Now().Unix()
	future := now + 3600

	// --- WRITE-HOLE SCOPE: a NON-3410 put from a not-yet-admitted key does NOT
	// self-admit. This is the "the write-hole admits no kind but 3410" property from
	// the operator's side: a raw exchange write from a stranger goes through the
	// Intake/SEAM-A path and is NEVER promoted into the KeySet — only the
	// redeem+operator-promote path admits a key. Injected BEFORE the ACCEPT redeem so
	// the ACCEPT (a single-reader, in-order barrier) guarantees the stranger put was
	// already processed once the member is admitted below.
	stranger, _ := identity.Generate()
	strangerPut := signExchangeEvent(t, stranger,
		[]string{exchange.TagPut, "exchange:content-type:code", "exchange:domain:go"}, nil,
		localPutPayload("stranger put that must not self-admit", 8000))
	s.conn.inject(strangerPut)

	// --- ACCEPT: one valid redeem onboards the member into the KeySet + genesis mint.
	const genesis = int64(1234)
	member, redeemEv := mustRedeem(t, operator, "alice", "grant-accept", genesis, now, future)
	s.conn.inject(redeemEv)
	waitFor(t, 10*time.Second, "valid redeem admits the fresh member into the live KeySet", func() bool {
		return s.ks.Allowed(member.PubKeyHex())
	})
	// The stranger's non-3410 write (processed before this admitted redeem) never
	// self-admitted: the write-hole admits ONLY a promoted redeem, never a raw write.
	if s.ks.Allowed(stranger.PubKeyHex()) {
		t.Fatalf("a non-3410 write admitted the stranger — the write-hole must admit ONLY a promoted redeem")
	}
	waitFor(t, 10*time.Second, "valid redeem mints the genesis grant to the member", func() bool {
		return s.scrip.Balance(member.PubKeyHex()) == genesis
	})

	// --- REJECT (foreign operator PIN mismatch): a redeem embedding an invite signed
	// by a DIFFERENT operator key is rejected — the member is never admitted. Use an
	// ACCEPT sentinel after it as an ordering barrier: once the sentinel is admitted,
	// the forged redeem before it is guaranteed already processed (single reader,
	// in-order) so a still-false Allowed is a real rejection, not a race.
	foreign, _ := identity.Generate()
	forgedMember, forgedRedeem := mustRedeem(t, foreign, "mallory", "grant-forged", 999, now, future)
	s.conn.inject(forgedRedeem)

	sentinelMember, sentinelRedeem := mustRedeem(t, operator, "sentinel", "grant-sentinel", 1, now, future)
	s.conn.inject(sentinelRedeem)
	waitFor(t, 10*time.Second, "sentinel redeem admitted (orders past the forged redeem)", func() bool {
		return s.ks.Allowed(sentinelMember.PubKeyHex())
	})
	if s.ks.Allowed(forgedMember.PubKeyHex()) {
		t.Fatalf("a foreign-operator-signed invite admitted a member — the operator PIN was not enforced (ADV-15 violated)")
	}
	if got := s.scrip.Balance(forgedMember.PubKeyHex()); got != 0 {
		t.Fatalf("forged redeem minted %d scrip, want 0", got)
	}

	// --- REJECT (expired): an otherwise-valid redeem of an already-expired invite is
	// rejected. Order past a second sentinel.
	expMember, expRedeem := mustRedeem(t, operator, "late", "grant-expired", 500, now-7200, now-3600)
	s.conn.inject(expRedeem)
	sentinel2Member, sentinel2Redeem := mustRedeem(t, operator, "sentinel2", "grant-sentinel2", 1, now, future)
	s.conn.inject(sentinel2Redeem)
	waitFor(t, 10*time.Second, "second sentinel admitted (orders past the expired redeem)", func() bool {
		return s.ks.Allowed(sentinel2Member.PubKeyHex())
	})
	if s.ks.Allowed(expMember.PubKeyHex()) {
		t.Fatalf("an expired invite admitted a member — freshness was not enforced")
	}

	// The accepted member's genesis was minted EXACTLY once (a redeemed grant is
	// durable; the echo of the operator's own scrip-mint does not re-credit).
	if got := s.scrip.Balance(member.PubKeyHex()); got != genesis {
		t.Fatalf("member balance = %d after onboarding, want exactly %d (no double-mint)", got, genesis)
	}
}

// TestInviteRedeem_ReplayRejectedAcrossRestart_b80 proves the token is single-use
// even across an operator process restart: the redeemed-grant-id set is persisted
// to disk, so replaying a redeem the operator already consumed does NOT re-admit
// the member and does NOT double-mint the genesis grant.
func TestInviteRedeem_ReplayRejectedAcrossRestart_b80(t *testing.T) {
	dir := t.TempDir()
	operator, _ := identity.Generate()
	writeInviteTestConfig(t, dir, operator)
	redeemedPath := redeemedInvitesPath(dir)

	ls, err := dgstore.Open(dir + "/events.jsonl")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	now := time.Now().Unix()
	future := now + 24*3600
	const genesis = int64(4321)
	member, redeemEv := mustRedeem(t, operator, "alice", "grant-replay", genesis, now, future)

	// --- Operator #1: process the redeem, admit + mint, then TEAR DOWN.
	ctx1, cancel1 := context.WithTimeout(context.Background(), 40*time.Second)
	s1 := newInviteStack(t, ctx1, dir, ls, operator, redeemedPath)
	s1.conn.inject(redeemEv)
	waitFor(t, 10*time.Second, "operator#1 admits the member", func() bool {
		return s1.ks.Allowed(member.PubKeyHex())
	})
	waitFor(t, 10*time.Second, "operator#1 mints the genesis grant", func() bool {
		return s1.scrip.Balance(member.PubKeyHex()) == genesis
	})
	cancel1()
	s1.stop()
	_ = ls.Close()

	// The grant id must be durably recorded as redeemed on disk.
	rsCheck, err := openRedeemedStore(redeemedPath)
	if err != nil {
		t.Fatalf("reopen redeemed store: %v", err)
	}
	if !rsCheck.has("grant-replay") {
		t.Fatalf("grant-replay was not persisted to the redeemed-id log across restart")
	}
	_ = rsCheck.close()

	// --- Operator #2: fresh process over the SAME store + SAME redeemed-id log, but
	// a DELIBERATELY EMPTY seed KeySet (ignore the persisted config allowlist) so the
	// ONLY thing that could admit the member is the replayed redeem re-promoting.
	ls2, err := dgstore.Open(dir + "/events.jsonl")
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() { _ = ls2.Close() })

	ctx2, cancel2 := context.WithTimeout(context.Background(), 40*time.Second)
	s2 := newInviteStack(t, ctx2, dir, ls2, operator, redeemedPath)
	t.Cleanup(func() { cancel2(); s2.stop() })

	// Replay the SAME redeem. It must be rejected (grant already redeemed, durable).
	s2.conn.inject(redeemEv)

	// A fresh sentinel redeem (new grant) IS admitted — proves operator#2 is live and
	// processing redeems, so a still-un-admitted replayed member is a real rejection.
	sentinelMember, sentinelRedeem := mustRedeem(t, operator, "sentinel", "grant-fresh", 7, time.Now().Unix(), future)
	s2.conn.inject(sentinelRedeem)
	waitFor(t, 10*time.Second, "operator#2 admits the fresh sentinel (orders past the replay)", func() bool {
		return s2.ks.Allowed(sentinelMember.PubKeyHex())
	})

	if s2.ks.Allowed(member.PubKeyHex()) {
		t.Fatalf("replayed redeem RE-ADMITTED the member after restart — redeemed-ids not honored (token became a reusable bearer credential)")
	}

	// Genesis grant not double-minted: operator#2's ScripStore replays operator#1's
	// single mint from the shared log (== genesis); the rejected replay adds nothing.
	if got := s2.scrip.Balance(member.PubKeyHex()); got != genesis {
		t.Fatalf("member balance = %d after replay, want exactly %d (genesis double-minted on replay)", got, genesis)
	}
}
