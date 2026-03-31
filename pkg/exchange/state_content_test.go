package exchange_test

// Tests for InventoryEntry.Content and applyPut content handling.
//
// Covered:
//   - Put with no content is silently dropped (not added to pendingPuts)
//   - Put with content: ContentHash is computed from content, Content field populated
//   - Put with content exceeding MaxContentBytes is silently dropped

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/3dl-dev/dontguess/pkg/exchange"
)

// buildPutPayloadWithContent constructs an exchange:put JSON payload including a content field.
func buildPutPayloadWithContent(desc string, tokenCost int64, contentB64 string) []byte {
	p, _ := json.Marshal(map[string]any{
		"description":  desc,
		"content":      contentB64,
		"token_cost":   tokenCost,
		"content_type": "code",
		"domains":      []string{"go"},
	})
	return p
}

// buildPutPayloadNoContent constructs an exchange:put JSON payload with no content field.
func buildPutPayloadNoContent(desc string, tokenCost int64) []byte {
	p, _ := json.Marshal(map[string]any{
		"description":  desc,
		"token_cost":   tokenCost,
		"content_type": "code",
		"domains":      []string{"go"},
	})
	return p
}

// TestApplyPut_ContentRequired verifies that a put with no content field is
// silently dropped and does not appear in pendingPuts.
func TestApplyPut_ContentRequired(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	replayIntoEngine(t, h, eng, buildPutPayloadNoContent("Go HTTP handler", 10000))

	pending := eng.State().PendingPuts()
	if len(pending) != 0 {
		t.Errorf("expected empty pendingPuts for put with no content, got %d entries", len(pending))
	}
}

// TestApplyPut_HashComputedFromContent verifies that a put with content has
// its ContentHash computed by the engine from the content bytes, and that
// the Content field is populated on the InventoryEntry.
func TestApplyPut_HashComputedFromContent(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	content := []byte("package main\n\nfunc main() {}\n")
	contentB64 := base64.StdEncoding.EncodeToString(content)

	replayIntoEngine(t, h, eng, buildPutPayloadWithContent("Go main stub", 5000, contentB64))

	pending := eng.State().PendingPuts()
	if len(pending) != 1 {
		t.Fatalf("expected 1 entry in pendingPuts, got %d", len(pending))
	}

	entry := pending[0]

	// Verify Content field is populated.
	if len(entry.Content) == 0 {
		t.Error("InventoryEntry.Content is empty; expected content bytes")
	}
	if string(entry.Content) != string(content) {
		t.Errorf("InventoryEntry.Content mismatch: got %q, want %q", entry.Content, content)
	}

	// Verify ContentHash is computed from content, not trusted from payload.
	sum := sha256.Sum256(content)
	wantHash := "sha256:" + hex.EncodeToString(sum[:])
	if entry.ContentHash != wantHash {
		t.Errorf("ContentHash = %q, want %q", entry.ContentHash, wantHash)
	}
}

// TestApplyPut_ContentSizeLimit verifies that a put with content exceeding
// MaxContentBytes is silently dropped.
func TestApplyPut_ContentSizeLimit(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	// Content just over the 1 MiB limit.
	oversized := make([]byte, exchange.MaxContentBytes+1)
	contentB64 := base64.StdEncoding.EncodeToString(oversized)

	replayIntoEngine(t, h, eng, buildPutPayloadWithContent("oversized content", 10000, contentB64))

	pending := eng.State().PendingPuts()
	if len(pending) != 0 {
		t.Errorf("expected empty pendingPuts for put with oversized content, got %d entries", len(pending))
	}
}
