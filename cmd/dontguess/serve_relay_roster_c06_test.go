package main

// serve_relay_roster_c06_test.go — dontguess-c06 GROUND-SOURCE (design §2/P5).
//
// The EXCHANGE-side fleet-roster fold: an operator-signed kind-30078 roster event
// is the SOURCE OF TRUTH for the live TrustChecker KeySet, folded from the relay
// event log. Driven through the REAL serve-path wiring — a real exchange.Engine
// sharing its LocalStore with the real attachRelayTransport reader (rosterFolder +
// Intake + Outbox + Sequencer) and a live TrustChecker whose KeySet the SEAM A
// promotion gate enforces — over the same in-process fake relay the rest of the
// serve_relay suite uses. Nothing is stubbed but the websocket wire itself; the
// fold, the signature re-verify, and the SEAM-A drop are all the production code.
//
// It asserts the four ground-source properties:
//
//	(1) an operator-signed roster admitting K makes ks.Allowed(K) true (folded from
//	    the log) AND a put from K folds through to matchable inventory (SEAM A pass);
//	(2) a put from a NON-admitted key is DROPPED at the exchange fold (dropped_unlisted)
//	    and never becomes inventory — the relay is dumb transport, the exchange rejects;
//	(3) a FORGED roster (NOT signed by the pinned operator) does NOT change the KeySet;
//	(4) removing K via a new roster drops K's subsequent put.
//
// ADV-2 is what (3) proves: applyPut/the KeySet NEVER trust that a relay gated the
// write — the fold re-verifies the operator signature itself, so a relay forwarding
// a forgery changes nothing.

import (
	"context"
	"testing"
	"time"

	"github.com/3dl-dev/dontguess/pkg/exchange"
	"github.com/3dl-dev/dontguess/pkg/identity"
	"github.com/3dl-dev/dontguess/pkg/nostr"
	dgstore "github.com/3dl-dev/dontguess/pkg/store"
)

// signFleetRoster builds an operator-signed kind-30078 fleet roster admitting
// memberHexKeys at createdAt, as a genuinely Schnorr-signed wire event the fake
// relay can inject. signer need not be the real operator — assertion (3) signs a
// forged roster with a non-operator key on purpose.
func signFleetRoster(t *testing.T, signer *identity.Secp256k1Identity, createdAt int64, memberHexKeys []string) *identity.Event {
	t.Helper()
	ev := &identity.Event{
		CreatedAt: createdAt,
		Kind:      nostr.KindFleetRoster,
		Tags:      nostr.FleetRosterTags(memberHexKeys),
		Content:   "",
	}
	if err := identity.SignEvent(signer, ev); err != nil {
		t.Fatalf("SignEvent(roster): %v", err)
	}
	return ev
}

func TestRelayRosterFold_KeySetSourceOfTruth_c06(t *testing.T) {
	dir := t.TempDir()
	ls, err := dgstore.Open(dir + "/events.jsonl")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = ls.Close() })

	operator, _ := identity.Generate()
	sellerK, _ := identity.Generate() // admitted via roster
	sellerJ, _ := identity.Generate() // never admitted by the operator
	sellerS, _ := identity.Generate() // ordering-sentinel member
	attacker, _ := identity.Generate()

	// Start with an EMPTY KeySet: this proves the roster fold — not the static
	// config allowlist — is what admits K. The SEAM-A promotion gate reads THIS ks.
	ks := exchange.NewKeySet()
	tc, err := exchange.NewTrustChecker(operator.PubKeyHex(), ks)
	if err != nil {
		t.Fatalf("NewTrustChecker: %v", err)
	}

	eng := exchange.NewEngine(exchange.EngineOptions{
		CampfireID:        "local",
		LocalStore:        ls,
		OperatorPublicKey: operator.PubKeyHex(),
		TrustChecker:      tc,
		PollInterval:      5 * time.Millisecond,
		Logger:            func(string, ...any) {},
	})

	rf, err := newRosterFolder(operator.PubKeyHex(), ks, "", nil)
	if err != nil {
		t.Fatalf("newRosterFolder: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	relayConn := newFakeRelayConn(true /* echo */)
	stop, err := attachRelayTransport(ctx, ls, operator, operator.PubKeyHex(),
		dir+"/events.jsonl.pubcursor", relayConn, relayConn, 5*time.Millisecond, nil, nil, nil,
		WithRosterFolder(rf))
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

	base := time.Now().Unix()

	// --- (1) operator-signed roster admits K → KeySet reflects K → put from K folds
	relayConn.inject(signFleetRoster(t, operator, base, []string{sellerK.PubKeyHex()}))
	waitFor(t, 8*time.Second, "KeySet reflects K folded from the operator-signed roster", func() bool {
		return ks.Allowed(sellerK.PubKeyHex())
	})

	putK := signExchangeEvent(t, sellerK,
		[]string{exchange.TagPut, "exchange:content-type:code", "exchange:domain:go"}, nil,
		localPutPayload("Go HTTP handler unit test generator (K)", 8000))
	relayConn.inject(putK)
	waitFor(t, 8*time.Second, "admitted K's put folds through SEAM A into matchable inventory", func() bool {
		return len(eng.State().Inventory()) == 1
	})

	// --- (2) put from NON-admitted J is DROPPED at the exchange fold (dropped_unlisted)
	beforeUnlisted := eng.DegradationSnapshot().DroppedUnlisted
	putJ := signExchangeEvent(t, sellerJ,
		[]string{exchange.TagPut, "exchange:content-type:code", "exchange:domain:rust"}, nil,
		localPutPayload("Rust async task scheduler generator (J)", 9000))
	relayConn.inject(putJ)
	waitFor(t, 8*time.Second, "non-admitted J's put is dropped_unlisted at SEAM A", func() bool {
		return eng.DegradationSnapshot().DroppedUnlisted > beforeUnlisted
	})
	if got := len(eng.State().Inventory()); got != 1 {
		t.Fatalf("inventory = %d after non-admitted put, want 1 (J must never promote)", got)
	}

	// --- (3) FORGED roster (attacker-signed) admitting J does NOT change the KeySet.
	// Inject the forgery, THEN a VALID sentinel roster {K,S}. Because the single
	// reader processes events in order, once S is admitted the forgery before it is
	// GUARANTEED already folded (and rejected) — so a still-false Allowed(J) is a
	// real rejection, not an unprocessed-event false pass.
	relayConn.inject(signFleetRoster(t, attacker, base+1, []string{sellerJ.PubKeyHex()}))
	relayConn.inject(signFleetRoster(t, operator, base+2, []string{sellerK.PubKeyHex(), sellerS.PubKeyHex()}))
	waitFor(t, 8*time.Second, "valid sentinel roster {K,S} folds (orders past the forgery)", func() bool {
		return ks.Allowed(sellerS.PubKeyHex())
	})
	if ks.Allowed(sellerJ.PubKeyHex()) {
		t.Fatalf("forged (non-operator-signed) roster admitted J — the fold trusted a relay-forwarded forgery (ADV-2 violated)")
	}
	if !ks.Allowed(sellerK.PubKeyHex()) {
		t.Fatalf("K dropped by the sentinel roster which included K — replace semantics wrong")
	}

	// --- (4) remove K via a new roster (later created_at, omits K) → K's put dropped
	relayConn.inject(signFleetRoster(t, operator, base+3, []string{sellerS.PubKeyHex()}))
	waitFor(t, 8*time.Second, "K de-admitted by a new operator roster that omits it", func() bool {
		return !ks.Allowed(sellerK.PubKeyHex())
	})

	beforeUnlisted2 := eng.DegradationSnapshot().DroppedUnlisted
	putK2 := signExchangeEvent(t, sellerK,
		[]string{exchange.TagPut, "exchange:content-type:code", "exchange:domain:python"}, nil,
		localPutPayload("Python pytest fixture generator (K after removal)", 8100))
	relayConn.inject(putK2)
	waitFor(t, 8*time.Second, "removed K's subsequent put is dropped_unlisted", func() bool {
		return eng.DegradationSnapshot().DroppedUnlisted > beforeUnlisted2
	})
	if got := len(eng.State().Inventory()); got != 1 {
		t.Fatalf("inventory = %d after removed-K put, want 1 (K's post-removal put must never promote)", got)
	}
}
