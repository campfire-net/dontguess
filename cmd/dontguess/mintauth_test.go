package main

// mintauth_test.go — dontguess-f91 (RT-B#3): the OpMint handler must require an
// operator-key signature, not merely socket reachability. These tests drive the
// REAL operator socket server (serveOperatorSocket + handleOperatorConn) with a
// REAL team-tier exchange.Engine backed by a REAL LocalScripStore, and exercise
// the auth gate end-to-end over a real net.Dial:
//
//   - an unsigned mint request is REJECTED even though it reaches the socket,
//   - a mint signed by a NON-operator key is REJECTED,
//   - a mint whose signature is bound to a DIFFERENT amount/recipient is REJECTED
//     (a captured operator signature cannot be replayed onto another mint),
//   - the LEGITIMATE operator-signed mint SUCCEEDS end-to-end and credits scrip.
//
// No mocks: identity.SignEvent produces a real BIP-340 Schnorr signature and the
// server's verifyMintAuth runs identity.VerifyEvent (the same secp256k1 verify
// the relay uses). Balance assertions read the live LocalScripStore.

import (
	"testing"
	"time"

	"github.com/campfire-net/dontguess/pkg/exchange"
	"github.com/campfire-net/dontguess/pkg/identity"
	"github.com/campfire-net/dontguess/pkg/scrip"
)

// newMintTierEngine builds a team-tier engine (ScripStore != nil) whose operator
// key is a real secp256k1 identity, so a mint-auth event signed by `op` verifies
// against State().OperatorKey. Returns the engine and its live scrip store.
func (h *opTestHarness) newMintTierEngine(t *testing.T, op *identity.Secp256k1Identity) (*exchange.Engine, *scrip.LocalScripStore) {
	t.Helper()
	ss, err := scrip.NewLocalScripStore(h.st, op.PubKeyHex())
	if err != nil {
		t.Fatalf("NewLocalScripStore: %v", err)
	}
	eng := exchange.NewEngine(exchange.EngineOptions{
		CampfireID:        h.cfID,
		LocalStore:        h.st,
		OperatorPublicKey: op.PubKeyHex(),
		ScripStore:        ss,
		Logger:            func(format string, args ...any) { t.Logf("[engine] "+format, args...) },
	})
	return eng, ss
}

func TestOperatorSocket_Mint_RequiresOperatorSignature(t *testing.T) {
	// Operator identity — the ONLY key that may authorize a mint.
	op, err := identity.Generate()
	if err != nil {
		t.Fatalf("Generate operator: %v", err)
	}
	// A recipient is just a pubkey hex.
	recipient, err := identity.Generate()
	if err != nil {
		t.Fatalf("Generate recipient: %v", err)
	}
	recipientHex := recipient.PubKeyHex()
	const amount = int64(42000)

	// --- (1) unsigned request is rejected even though it reaches the socket ---
	t.Run("unsigned_rejected", func(t *testing.T) {
		h := newOpTestHarness(t)
		eng, ss := h.newMintTierEngine(t, op)
		sockPath, _ := startSocketServer(t, eng)

		var resp okResponse
		dialAndRequest(t, sockPath, map[string]any{
			"op":        OpMint,
			"recipient": recipientHex,
			"amount":    amount,
			// mint_auth deliberately absent
		}, &resp)

		if resp.OK {
			t.Fatal("unsigned mint returned ok=true — socket reachability must NOT authorize a mint")
		}
		if resp.Error == "" {
			t.Error("unsigned mint error message is empty")
		}
		if got := ss.Balance(recipientHex); got != 0 {
			t.Errorf("recipient balance = %d after rejected unsigned mint, want 0", got)
		}
		t.Logf("unsigned mint rejected: %s", resp.Error)
	})

	// --- (2) a mint signed by a NON-operator key is rejected ---
	t.Run("wrong_key_rejected", func(t *testing.T) {
		h := newOpTestHarness(t)
		eng, ss := h.newMintTierEngine(t, op)
		sockPath, _ := startSocketServer(t, eng)

		attacker, err := identity.Generate()
		if err != nil {
			t.Fatalf("Generate attacker: %v", err)
		}
		ev := buildMintAuthEvent(recipientHex, amount, time.Now().Unix())
		if err := identity.SignEvent(attacker, ev); err != nil {
			t.Fatalf("SignEvent attacker: %v", err)
		}

		var resp okResponse
		dialAndRequest(t, sockPath, map[string]any{
			"op":        OpMint,
			"recipient": recipientHex,
			"amount":    amount,
			"mint_auth": ev,
		}, &resp)

		if resp.OK {
			t.Fatal("mint signed by a non-operator key returned ok=true — must be rejected")
		}
		if got := ss.Balance(recipientHex); got != 0 {
			t.Errorf("recipient balance = %d after rejected wrong-key mint, want 0", got)
		}
		t.Logf("wrong-key mint rejected: %s", resp.Error)
	})

	// --- (3) an operator signature bound to a DIFFERENT mint cannot be replayed ---
	t.Run("rebound_amount_rejected", func(t *testing.T) {
		h := newOpTestHarness(t)
		eng, ss := h.newMintTierEngine(t, op)
		sockPath, _ := startSocketServer(t, eng)

		// Operator signs an auth event for amount=1, but the wire request asks
		// for a much larger amount. The binding check must reject it.
		ev := buildMintAuthEvent(recipientHex, 1, time.Now().Unix())
		if err := identity.SignEvent(op, ev); err != nil {
			t.Fatalf("SignEvent operator: %v", err)
		}

		var resp okResponse
		dialAndRequest(t, sockPath, map[string]any{
			"op":        OpMint,
			"recipient": recipientHex,
			"amount":    amount, // != the signed amount (1)
			"mint_auth": ev,
		}, &resp)

		if resp.OK {
			t.Fatal("mint with signature bound to a different amount returned ok=true — must be rejected")
		}
		if got := ss.Balance(recipientHex); got != 0 {
			t.Errorf("recipient balance = %d after rejected rebound mint, want 0", got)
		}
		t.Logf("rebound-amount mint rejected: %s", resp.Error)
	})

	// --- (4) the legitimate operator-signed mint succeeds end-to-end ---
	t.Run("operator_signed_succeeds", func(t *testing.T) {
		h := newOpTestHarness(t)
		eng, ss := h.newMintTierEngine(t, op)
		sockPath, _ := startSocketServer(t, eng)

		ev := buildMintAuthEvent(recipientHex, amount, time.Now().Unix())
		if err := identity.SignEvent(op, ev); err != nil {
			t.Fatalf("SignEvent operator: %v", err)
		}

		var resp okResponse
		dialAndRequest(t, sockPath, map[string]any{
			"op":        OpMint,
			"recipient": recipientHex,
			"amount":    amount,
			"mint_auth": ev,
		}, &resp)

		if !resp.OK {
			t.Fatalf("legitimate operator mint returned ok=false: %s", resp.Error)
		}
		if got := ss.Balance(recipientHex); got != amount {
			t.Errorf("recipient balance = %d after successful mint, want %d", got, amount)
		}
	})
}

// TestVerifyMintAuth_Unit exercises verifyMintAuth's branches directly (the
// pure gate function) so each rejection reason is independently covered without
// standing up a socket.
func TestVerifyMintAuth_Unit(t *testing.T) {
	op, err := identity.Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	opHex := op.PubKeyHex()
	const recipient = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const amount = int64(1000)

	sign := func(ev *identity.Event, signer *identity.Secp256k1Identity) *identity.Event {
		if err := identity.SignEvent(signer, ev); err != nil {
			t.Fatalf("SignEvent: %v", err)
		}
		return ev
	}

	t.Run("nil_auth", func(t *testing.T) {
		if err := verifyMintAuth(nil, opHex, recipient, amount); err == nil {
			t.Fatal("nil auth must be rejected")
		}
	})

	t.Run("empty_operator_key", func(t *testing.T) {
		ev := sign(buildMintAuthEvent(recipient, amount, 1), op)
		if err := verifyMintAuth(ev, "", recipient, amount); err == nil {
			t.Fatal("empty operator key must fail closed")
		}
	})

	t.Run("wrong_kind", func(t *testing.T) {
		ev := buildMintAuthEvent(recipient, amount, 1)
		ev.Kind = 3401 // a real exchange put kind, not mintAuthKind
		ev = sign(ev, op)
		if err := verifyMintAuth(ev, opHex, recipient, amount); err == nil {
			t.Fatal("wrong kind must be rejected")
		}
	})

	t.Run("tampered_after_sign", func(t *testing.T) {
		ev := sign(buildMintAuthEvent(recipient, amount, 1), op)
		// Mutate a tag AFTER signing → id/sig no longer cover the content.
		ev.Tags = [][]string{{mintAuthRecipientTag, recipient}, {mintAuthAmountTag, "999999"}}
		if err := verifyMintAuth(ev, opHex, recipient, amount); err == nil {
			t.Fatal("tampered-after-sign event must fail signature verification")
		}
	})

	t.Run("valid", func(t *testing.T) {
		ev := sign(buildMintAuthEvent(recipient, amount, 1), op)
		if err := verifyMintAuth(ev, opHex, recipient, amount); err != nil {
			t.Fatalf("valid operator-signed auth must pass, got %v", err)
		}
	})
}
