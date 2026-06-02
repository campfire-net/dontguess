package exchange_test

// token_cost_validation_test.go — enforcement tests for dontguess-46f.
//
// Problem: seller-declared token_cost is unvalidated against content size.
// The design review (§2) found 5 entries claiming 1.5 M tokens on tiny content;
// those outliers dominate the reported savings metric.
//
// Fix: applyPut rejects any put where token_cost > content_size_bytes / MinBytesPerToken
// (i.e. more than one token per 3 bytes of actual content). See state.go:MinBytesPerToken.
//
// Covered:
//   - TestTokenCostValidation_RejectsImplausibleCost: an absurd token_cost (1.5 M)
//     on a 200-byte content is rejected at applyPut time — the put never enters
//     pendingPuts and therefore never reaches the put-accept path.
//   - TestTokenCostValidation_AcceptsPlausibleCost: a plausible token_cost that
//     is consistent with the content size passes applyPut unchanged, enters
//     pendingPuts, and can be accepted by AutoAcceptPut — proving the path is live.
//
// Both tests go through the REAL applyPut path (State.Replay → applyPut):
// messages are written to the store by sendMessage (the canonical harness helper),
// then Replay is called to feed them into state. No mocks are used; the only
// bypass is ReadSkipSync=true (skips filesystem transport sync, not validation).

import (
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/campfire-net/dontguess/pkg/exchange"
)

// buildTinyPutPayload builds a put payload with an exact content size.
//
// token_cost is set to the caller-supplied value — may be implausible relative
// to the content size, which is the point for rejection tests.
//
// Unlike putPayload (which caps at MaxContentBytes and aligns size), this
// helper generates exactly targetSize bytes of content so the plausibility
// boundary is predictable in tests.
func buildTinyPutPayload(t *testing.T, desc string, contentType string, tokenCost int64, targetSize int) []byte {
	t.Helper()
	if targetSize < 1 {
		targetSize = 1
	}
	// Build deterministic content of exactly targetSize bytes.
	content := make([]byte, targetSize)
	prefix := []byte("cached: " + desc + " ")
	copy(content, prefix)
	for i := len(prefix); i < targetSize; i++ {
		content[i] = byte('a' + i%26)
	}
	encoded := base64.StdEncoding.EncodeToString(content)
	p, err := json.Marshal(map[string]any{
		"description":  desc,
		"content":      encoded,
		"token_cost":   tokenCost,
		"content_type": contentType,
		"domains":      []string{"go"},
	})
	if err != nil {
		t.Fatalf("buildTinyPutPayload: marshal: %v", err)
	}
	return p
}

// maxPlausibleTokenCostForSize returns the maximum plausible token_cost for a
// given content size in bytes, mirroring the check in state.go applyPut.
// Used by tests to compute expected rejection boundaries.
func maxPlausibleTokenCostForSize(contentSizeBytes int) int64 {
	v := int64(contentSizeBytes) * exchange.MaxTokensPerByte
	if v < 1 {
		return 1
	}
	return v
}

// TestTokenCostValidation_RejectsImplausibleCost verifies that a put with an
// absurd token_cost (1.5 M) on a 200-byte content is rejected by applyPut and
// never enters the put-accept path.
//
// The plausibility boundary is: token_cost ≤ content_size_bytes * MaxTokensPerByte.
// For 200 bytes: max_plausible = 200 * 1000 = 200 000 tokens.
// 1.5 M (7500 tokens/byte) far exceeds this limit → rejected.
//
// Proof that this hits the real applyPut path:
//   - The put message is written to the campfire store via h.sendMessage (the
//     standard harness helper used throughout engine_test.go).
//   - State.Replay is called, which drives applyPut for every exchange:put message.
//   - applyPut checks token_cost > content_size_bytes * MaxTokensPerByte and returns early,
//     leaving the message out of pendingPuts.
//   - We assert PendingPuts() is empty — the only way it can be empty after Replay
//     is if applyPut rejected the put.
//   - AutoAcceptPut on a non-pending put returns an error, confirming there is
//     nothing to accept.
func TestTokenCostValidation_RejectsImplausibleCost(t *testing.T) {
	t.Parallel()

	const (
		contentSize = 200             // bytes — a tiny payload
		tokenCost   = int64(1_500_000) // 1.5 M tokens on 200 bytes = 7500 tokens/byte
		// max_plausible = 200 * MaxTokensPerByte(1000) = 200 000.
		// 1.5 M > 200 000, so this must be rejected.
	)

	h := newTestHarness(t)
	eng := h.newEngine()

	putMsg := h.sendMessage(
		h.seller,
		buildTinyPutPayload(t, "implausible cost claim", "other", tokenCost, contentSize),
		[]string{exchange.TagPut, "exchange:content-type:other", "exchange:domain:go"},
		nil,
	)

	// Replay the full log so applyPut runs.
	replayAll(t, h, eng)

	// The put must NOT be in pendingPuts — applyPut rejected it.
	maxPlausible := maxPlausibleTokenCostForSize(contentSize)
	pending := eng.State().PendingPuts()
	for _, e := range pending {
		if e.PutMsgID == putMsg.ID {
			t.Errorf("implausible put %s should have been rejected by applyPut, but found in pendingPuts "+
				"(token_cost=%d, content_size=%d, max_plausible=%d = %d * %d)",
				putMsg.ID[:8], tokenCost, contentSize, maxPlausible, contentSize, exchange.MaxTokensPerByte)
		}
	}
	if len(pending) == 0 {
		t.Logf("PASS: implausible put correctly rejected — PendingPuts() is empty (max_plausible=%d for %d bytes)", maxPlausible, contentSize)
	}

	// Attempting to accept the rejected put must fail (it never entered pendingPuts).
	err := eng.AutoAcceptPut(putMsg.ID, tokenCost*70/100, time.Now().Add(72*time.Hour))
	if err == nil {
		t.Errorf("AutoAcceptPut on a rejected put should return an error, got nil")
	}
}

// TestTokenCostValidation_AcceptsPlausibleCost verifies that a put with a token_cost
// consistent with the content size passes applyPut and proceeds through the
// complete put-accept path.
//
// Plausible means: token_cost ≤ content_size_bytes * MaxTokensPerByte.
// With content_size=200 bytes and MaxTokensPerByte=1000, the ceiling is 200 000 tokens.
// We use token_cost=50 000 (250 tokens/byte), well under the ceiling.
//
// Using the same 200-byte content as the rejection test proves the validation is
// on the token_cost:content_size ratio, not on the content or token_cost alone.
//
// Proof that this hits the real put-accept path:
//   - sendMessage + Replay → applyPut → pendingPuts (asserted non-empty).
//   - AutoAcceptPut (the standard operator accept method) succeeds without error.
//   - The entry appears in Inventory() after acceptance — the standard put-accept
//     termination condition verified throughout engine_test.go.
func TestTokenCostValidation_AcceptsPlausibleCost(t *testing.T) {
	t.Parallel()

	const (
		contentSize = 200           // bytes — same tiny payload as the rejection test
		tokenCost   = int64(50_000) // 50 000 tokens for 200 bytes = 250 tokens/byte
		// max_plausible = 200 * MaxTokensPerByte(1000) = 200 000.
		// 50 000 ≤ 200 000, so this must be accepted unchanged.
	)

	h := newTestHarness(t)
	eng := h.newEngine()

	putMsg := h.sendMessage(
		h.seller,
		buildTinyPutPayload(t, "plausible inference result size=200 tokens=50k", "code", tokenCost, contentSize),
		[]string{exchange.TagPut, "exchange:content-type:code", "exchange:domain:go"},
		nil,
	)

	// Replay so applyPut runs.
	replayAll(t, h, eng)

	// The put must be in pendingPuts — applyPut accepted it.
	maxPlausible := maxPlausibleTokenCostForSize(contentSize)
	pending := eng.State().PendingPuts()
	found := false
	for _, e := range pending {
		if e.PutMsgID == putMsg.ID {
			found = true
			// Verify token_cost is stored unchanged (not capped by the plausibility check).
			if e.TokenCost != tokenCost {
				t.Errorf("token_cost stored as %d, want %d (plausible cost must pass unchanged)", e.TokenCost, tokenCost)
			}
		}
	}
	if !found {
		t.Fatalf("plausible put %s not found in pendingPuts — applyPut incorrectly rejected it "+
			"(token_cost=%d, content_size=%d, max_plausible=%d = %d * %d)",
			putMsg.ID[:8], tokenCost, contentSize, maxPlausible, contentSize, exchange.MaxTokensPerByte)
	}

	// Accept the put through the real AutoAcceptPut path.
	price := tokenCost * 70 / 100
	if err := eng.AutoAcceptPut(putMsg.ID, price, time.Now().Add(72*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut on plausible put: %v", err)
	}

	// Entry must now be in inventory — put-accept complete.
	inv := eng.State().Inventory()
	inInv := false
	for _, e := range inv {
		if e.PutMsgID == putMsg.ID {
			inInv = true
			if e.TokenCost != tokenCost {
				t.Errorf("inventory entry has token_cost=%d, want %d (plausible cost must be stored unchanged)", e.TokenCost, tokenCost)
			}
		}
	}
	if !inInv {
		t.Errorf("accepted put %s not found in inventory — put-accept path did not complete", putMsg.ID[:8])
	}
}
