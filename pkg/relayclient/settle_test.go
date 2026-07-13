package relayclient

// settle_test.go — focused CLIENT-SIDE guards for ed2-C that need no operator:
// the SAME-KEY invariant, the budget gate, and the deliver-verification LOUD
// failures (content_hash mismatch, oversize inline, Blossom pointer). The full
// real-stack money proofs (scrip really moving through the engine, the underfunded
// reject RECEIVED via the per-phase filter) live in the package-main
// cmd/dontguess/serve_relay_ed2c_test.go so a cmd source edit invalidates the cache.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/3dl-dev/dontguess/pkg/identity"
)

// TestSettle_SameKeyViolation_FailsLoud proves the SAME-KEY guard (§3.5): a settle
// signed by a DIFFERENT npub than the one that signed the buy fails LOUD, client-side,
// BEFORE any wire op — the engine would otherwise silently drop the mismatch and the
// client would hang forever.
func TestSettle_SameKeyViolation_FailsLoud(t *testing.T) {
	buyer, _ := identity.Generate()
	other, _ := identity.Generate()

	buy := &BuyResult{
		BuyID:       "buy-id",
		Outcome:     BuyOutcomeMatch,
		MatchMsgID:  "match-wire-id",
		BuyerPubKey: buyer.PubKeyHex(),
		Matches:     []MatchEntry{{EntryID: "entry-1", Price: 10}},
	}
	// The conn is never dialed — the guard returns before any Send.
	conn := NewConn("ws://unused", other, WithBackoff(testBackoff()))
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := Settle(ctx, conn, other, buy, SettleOptions{Budget: 1000})
	if err == nil {
		t.Fatalf("expected a LOUD same-key violation error, got nil")
	}
	if !strings.Contains(err.Error(), "SAME-KEY") {
		t.Fatalf("error %q does not surface the same-key violation", err)
	}
}

// TestSettle_SameKey_NoViolationWhenStable confirms the guard does NOT false-fire
// when buy and settle share the identity (the normal one-invocation case): with a
// price above budget it must reach the budget gate, not the key guard.
func TestSettle_BudgetExceeded_DoesNotBuyerAccept(t *testing.T) {
	buyer, _ := identity.Generate()
	buy := &BuyResult{
		BuyID:       "buy-id",
		Outcome:     BuyOutcomeMatch,
		MatchMsgID:  "match-wire-id",
		BuyerPubKey: buyer.PubKeyHex(),
		Matches:     []MatchEntry{{EntryID: "entry-1", Price: 5000}},
	}
	conn := NewConn("ws://unused", buyer, WithBackoff(testBackoff()))
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	res, err := Settle(ctx, conn, buyer, buy, SettleOptions{Budget: 100})
	if err != nil {
		t.Fatalf("budget gate should return a terminal outcome, not an error: %v", err)
	}
	if res.Outcome != SettleOutcomeBudgetExceeded {
		t.Fatalf("outcome = %s, want budget-exceeded", res.Outcome)
	}
	if res.Price != 5000 {
		t.Fatalf("surfaced price = %d, want 5000", res.Price)
	}
}

// TestVerifyDeliver_HashMismatch_FailsLoud proves a tampered content body aborts
// BEFORE settle(complete) (§3.5 / relay-transport §0): the client never confirms
// receipt of content whose sha256 does not match the operator's content_hash.
func TestVerifyDeliver_HashMismatch_FailsLoud(t *testing.T) {
	body := []byte("the genuine cached inference result")
	payload, _ := json.Marshal(map[string]any{
		"phase":        "deliver",
		"entry_id":     "entry-1",
		"content":      base64.StdEncoding.EncodeToString(body),
		"content_hash": "sha256:deadbeef", // wrong
	})
	ev := &identity.Event{ID: "deliver-wire", Content: string(payload)}

	_, _, err := verifyDeliver(context.Background(), nil, nil, ev, 0)
	if err == nil {
		t.Fatalf("expected a LOUD content-hash mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), "CONTENT HASH MISMATCH") {
		t.Fatalf("error %q does not surface the hash mismatch", err)
	}
}

// TestVerifyDeliver_Match_ReturnsContent confirms a well-formed inline deliver with
// a correct hash yields the exact content bytes.
func TestVerifyDeliver_Match_ReturnsContent(t *testing.T) {
	body := []byte("package main\n\nfunc Handler() {}\n")
	raw := sha256.Sum256(body)
	hash := "sha256:" + hex.EncodeToString(raw[:])
	payload, _ := json.Marshal(map[string]any{
		"phase":        "deliver",
		"entry_id":     "entry-1",
		"content":      base64.StdEncoding.EncodeToString(body),
		"content_hash": hash,
	})
	ev := &identity.Event{ID: "deliver-wire", Content: string(payload)}

	got, gotHash, err := verifyDeliver(context.Background(), nil, nil, ev, 0)
	if err != nil {
		t.Fatalf("verifyDeliver: %v", err)
	}
	if string(got) != string(body) {
		t.Fatalf("content = %q, want %q", got, body)
	}
	if gotHash != hash {
		t.Fatalf("hash = %q, want %q", gotHash, hash)
	}
}

// TestVerifyDeliver_BlossomPointer_FailsLoud proves a pointer deliver (oversized
// content offloaded to Blossom) LOUD-fails pointing at the deferred Blossom item —
// Blossom fetch is NOT implemented in ed2 (gate G-blossom).
func TestVerifyDeliver_BlossomPointer_FailsLoud(t *testing.T) {
	payload, _ := json.Marshal(map[string]any{
		"phase":        "deliver",
		"entry_id":     "entry-1",
		"blob_pointer": "blossom://abc123",
		"content_hash": "sha256:whatever",
	})
	ev := &identity.Event{ID: "deliver-wire", Content: string(payload)}

	_, _, err := verifyDeliver(context.Background(), nil, nil, ev, 0)
	if err == nil {
		t.Fatalf("expected a LOUD Blossom-pointer error, got nil")
	}
	if !strings.Contains(err.Error(), "BLOSSOM POINTER") || !strings.Contains(err.Error(), "DEFERRED") {
		t.Fatalf("error %q does not point at the deferred Blossom item", err)
	}
}

// TestVerifyDeliver_Oversize_FailsLoud proves the hard max-inline guard: an inline
// body over the ceiling LOUD-fails (never inlined) and points at Blossom.
func TestVerifyDeliver_Oversize_FailsLoud(t *testing.T) {
	body := make([]byte, 200)
	for i := range body {
		body[i] = byte('a' + i%26)
	}
	raw := sha256.Sum256(body)
	hash := "sha256:" + hex.EncodeToString(raw[:])
	payload, _ := json.Marshal(map[string]any{
		"phase":        "deliver",
		"entry_id":     "entry-1",
		"content":      base64.StdEncoding.EncodeToString(body),
		"content_hash": hash,
	})
	ev := &identity.Event{ID: "deliver-wire", Content: string(payload)}

	_, _, err := verifyDeliver(context.Background(), nil, nil, ev, 10) // ceiling far below the 200-byte body
	if err == nil {
		t.Fatalf("expected a LOUD oversize error, got nil")
	}
	if !strings.Contains(err.Error(), "max-inline") && !strings.Contains(err.Error(), "Blossom") {
		t.Fatalf("error %q does not surface the max-inline / Blossom guard", err)
	}
}
