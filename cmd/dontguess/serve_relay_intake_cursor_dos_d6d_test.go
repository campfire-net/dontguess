package main

// serve_relay_intake_cursor_dos_d6d_test.go — dontguess-d6d GROUND-SOURCE
// (event-suppression DoS, security-552).
//
// The durable per-relay Intake cursor bounds the resync REQ `since`. Before this
// fix runReader advanced that cursor UNCONDITIONALLY to every received event's
// relay-supplied created_at — even a REJECTED one, and with no upper bound. On an
// OPEN relay an anonymous attacker could therefore publish a bogus, REJECTED
// kind-3410 (un-allowlisted, so anyone can write it) carrying a FAR-FUTURE
// created_at; the cursor would jump years ahead and the next resync REQ `since`
// would skip EVERY real exchange event up to that future time (event suppression).
//
// The fix is two independent guards, both exercised here:
//	(a) advance ONLY for an ACCEPTED/folded event — a rejected forgery never moves
//	    the cursor at all; and
//	(b) clamp the created_at to now+bounded-drift before advancing — even a
//	    validly-signed but future-dated event cannot push the cursor past now+drift.
//
// The integration test drives the REAL serve reader (redeemHandler + Intake +
// Sequencer over the same in-process fake OPEN relay the rest of the suite uses).
// It replays the exact attack: a REJECTED, far-future-dated kind-3410 (a foreign-
// operator-signed invite) followed by a genuine operator-signed redeem at ~now.
// The genuine redeem is the "subsequent real event still ingested" — it admits its
// member (observable), proving the reader kept ingesting — and it is the ONLY event
// permitted to move the cursor. The assertion: the cursor holds ~now, NOT the
// attacker's far-future timestamp. The unit test pins the clamp boundary directly.

import (
	"context"
	"testing"
	"time"

	"github.com/3dl-dev/dontguess/pkg/exchange"
	"github.com/3dl-dev/dontguess/pkg/identity"
	"github.com/3dl-dev/dontguess/pkg/relay"
	"github.com/3dl-dev/dontguess/pkg/scrip"
	dgstore "github.com/3dl-dev/dontguess/pkg/store"
)

func TestClampFutureCreatedAt_d6d(t *testing.T) {
	const now = int64(1_000_000)
	ceil := now + maxIntakeCursorDriftSeconds
	cases := []struct {
		name      string
		createdAt int64
		want      int64
	}{
		{"past passes through unchanged", now - 3600, now - 3600},
		{"exactly now passes through", now, now},
		{"within drift passes through", now + maxIntakeCursorDriftSeconds - 1, now + maxIntakeCursorDriftSeconds - 1},
		{"exactly at ceiling passes through", ceil, ceil},
		{"one past the ceiling clamps down", ceil + 1, ceil},
		{"far future clamps to ceiling", now + 10*365*24*3600, ceil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := clampFutureCreatedAt(c.createdAt, now); got != c.want {
				t.Fatalf("clampFutureCreatedAt(%d, %d) = %d, want %d", c.createdAt, now, got, c.want)
			}
		})
	}
}

// TestIntakeCursor_RejectedFutureEventDoesNotPoison_d6d is the ground-source: an
// anonymous FUTURE-DATED, REJECTED kind-3410 does NOT advance the durable Intake
// cursor, and a subsequent REAL event (a genuine redeem, ~now) is still ingested —
// its ~now created_at, not the attacker's far-future one, is where the cursor lands.
func TestIntakeCursor_RejectedFutureEventDoesNotPoison_d6d(t *testing.T) {
	dir := t.TempDir()
	operator, _ := identity.Generate()
	writeInviteTestConfig(t, dir, operator)

	storePath := dir + "/events.jsonl"
	ls, err := dgstore.Open(storePath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = ls.Close() })

	ks := exchange.NewKeySet()
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

	rf, err := newRosterFolder(operator.PubKeyHex(), ks, "", nil)
	if err != nil {
		t.Fatalf("newRosterFolder: %v", err)
	}
	ctrl := &allowlistController{
		keys:           ks,
		operatorSigner: operator,
		operatorKeyHex: operator.PubKeyHex(),
		dgHome:         dir,
		nowUnix:        func() int64 { return time.Now().Unix() },
	}
	rh, err := newRedeemHandler(operator.PubKeyHex(), ctrl, eng, redeemedInvitesPath(dir), nil)
	if err != nil {
		t.Fatalf("newRedeemHandler: %v", err)
	}

	intakePath := intakeCursorPath(storePath, "wss://relay.example")

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	conn := newFakeRelayConn(true /* echo */)
	stop, err := attachRelayTransport(ctx, ls, operator, operator.PubKeyHex(),
		dir+"/events.jsonl.pubcursor", conn, conn, 5*time.Millisecond, nil, nil, nil,
		WithRosterFolder(rf), WithRedeemHandler(rh), WithIntakeCursorPath(intakePath))
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
	t.Cleanup(func() { cancel(); <-engDone; stop() })

	now := time.Now().Unix()
	future := now + 3600
	// ~10 years ahead: the poison timestamp. If the cursor ever holds this, every
	// real event with a created_at below it is skipped on the next resync REQ.
	farFuture := now + 10*365*24*3600

	// --- ATTACK: an anonymous, FUTURE-DATED, REJECTED kind-3410. The embedded invite
	// is signed by a FOREIGN operator key, so VerifyRedeem rejects it on the operator-
	// PIN check (ADV-15) regardless of its timestamp — accepted=false, so the cursor
	// must not move. created_at is set to farFuture (mustRedeem threads its `now` param
	// through BuildRedeemEvent's CreatedAt).
	foreign, _ := identity.Generate()
	forgedMember, forgedFuture := mustRedeem(t, foreign, "mallory", "grant-forged-future", 0, farFuture, farFuture+3600)
	if forgedFuture.CreatedAt != farFuture {
		t.Fatalf("test setup: forged redeem created_at = %d, want the far-future %d", forgedFuture.CreatedAt, farFuture)
	}
	conn.inject(forgedFuture)

	// --- SUBSEQUENT REAL EVENT: a genuine operator-signed redeem at created_at ~= now.
	// It is accepted, admits its member (the observable "still ingested" proof), and is
	// the only event allowed to advance the cursor. Its admission also orders past the
	// forged event: the single in-order reader guarantees the forgery was processed
	// first, so the cursor read below reflects both.
	member, realRedeem := mustRedeem(t, operator, "alice", "grant-real", 1, now, future)
	conn.inject(realRedeem)

	waitFor(t, 10*time.Second, "the subsequent real (genuine) redeem is still ingested — its member is admitted", func() bool {
		return ks.Allowed(member.PubKeyHex())
	})
	// The forged redeem never admitted anyone (rejected) — belt-and-suspenders.
	if ks.Allowed(forgedMember.PubKeyHex()) {
		t.Fatalf("the forged foreign-operator redeem admitted a member — test setup is wrong (it must be rejected)")
	}

	// The cursor advance runs in runReader immediately AFTER handle() returns (which
	// is what admitted the member above), so poll the on-disk sidecar until the
	// accepted redeem's advance has landed. Read a FRESH cursor each poll (the wiring
	// fsyncs every Advance) — this is what a restart's resync REQ would resume from.
	var got int64
	waitFor(t, 5*time.Second, "the accepted redeem's cursor advance lands on disk", func() bool {
		ic, oerr := relay.OpenIntakeCursor(intakePath)
		if oerr != nil {
			t.Fatalf("OpenIntakeCursor: %v", oerr)
		}
		got = ic.Value()
		return got >= realRedeem.CreatedAt
	})
	if got >= farFuture {
		t.Fatalf("rejected future-dated kind-3410 POISONED the intake cursor: value = %d (>= far-future %d); the next resync REQ `since` would skip every real event up to then (event-suppression DoS, security-552)", got, farFuture)
	}
	// Positively: the cursor advanced to the real redeem's ~now created_at, bounded by
	// now+drift — it neither jumped to the future nor stalled below the real event.
	ceil := time.Now().Unix() + maxIntakeCursorDriftSeconds
	if got > ceil {
		t.Fatalf("intake cursor = %d exceeds now+drift ceiling %d — clamp not applied", got, ceil)
	}
	if got < realRedeem.CreatedAt {
		t.Fatalf("intake cursor = %d did not advance to the ingested real redeem's created_at %d", got, realRedeem.CreatedAt)
	}
}
