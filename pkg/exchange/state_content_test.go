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

// TestApplyPut_ZeroByteContent verifies that a put with zero-byte content
// (base64("") == "") is silently dropped and does not appear in pendingPuts.
// Zero-byte content encodes to the empty string, which triggers the
// content-required guard in applyPut.
func TestApplyPut_ZeroByteContent(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	// base64 of empty byte slice is "", which applyPut treats as absent content.
	contentB64 := base64.StdEncoding.EncodeToString([]byte{})

	replayIntoEngine(t, h, eng, buildPutPayloadWithContent("zero-byte content", 10000, contentB64))

	pending := eng.State().PendingPuts()
	if len(pending) != 0 {
		t.Errorf("expected empty pendingPuts for put with zero-byte content, got %d entries", len(pending))
	}
}

// TestApplyPut_ExactBoundaryAccepted verifies that a put with content of
// exactly MaxContentBytes is accepted and appears in pendingPuts.
// This is the upper boundary: MaxContentBytes+1 is rejected (see TestApplyPut_ContentSizeLimit),
// MaxContentBytes exactly must be accepted.
func TestApplyPut_ExactBoundaryAccepted(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	// Exactly at the limit — must be accepted.
	exactContent := make([]byte, exchange.MaxContentBytes)
	for i := range exactContent {
		exactContent[i] = byte('a' + i%26)
	}
	contentB64 := base64.StdEncoding.EncodeToString(exactContent)

	replayIntoEngine(t, h, eng, buildPutPayloadWithContent("exact boundary content", 10000, contentB64))

	pending := eng.State().PendingPuts()
	if len(pending) != 1 {
		t.Errorf("expected 1 entry in pendingPuts for exact-boundary content, got %d", len(pending))
	}
}

// TestApplyPut_IdempotentByMsgID verifies that replaying the same put message
// ID twice results in only one entry in pendingPuts.
// Campfire deduplicates by message ID at the log level; the state machine
// mirrors this by keying pendingPuts on msg.ID — a second applyPut with the
// same ID overwrites the same map slot, producing exactly one entry.
func TestApplyPut_IdempotentByMsgID(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	content := []byte("cached inference result: idempotency check")
	contentB64 := base64.StdEncoding.EncodeToString(content)
	payload := buildPutPayloadWithContent("idempotency check", 5000, contentB64)

	// Send the message once to get a real msg.ID in the store.
	h.sendMessage(h.seller, payload,
		[]string{exchange.TagPut, "exchange:content-type:code"},
		nil,
	)

	// Fetch the messages from the store — contains exactly one put message.
	msgs, err := h.st.ListMessages(h.cfID, 0)
	if err != nil {
		t.Fatalf("listing messages: %v", err)
	}

	// Build a duplicate slice: same message appended twice.
	// This simulates a campfire log where the same message ID appears twice
	// (e.g., due to a replay bug or network re-delivery).
	exchangeMsgs := exchange.FromStoreRecords(msgs)
	// Find the put message and duplicate it.
	var duplicated []exchange.Message
	for _, m := range exchangeMsgs {
		duplicated = append(duplicated, m)
		for _, tag := range m.Tags {
			if tag == exchange.TagPut {
				duplicated = append(duplicated, m) // same ID, second time
				break
			}
		}
	}

	eng.State().Replay(duplicated)

	pending := eng.State().PendingPuts()
	if len(pending) != 1 {
		t.Errorf("expected 1 entry in pendingPuts after replaying same msg ID twice, got %d", len(pending))
	}
}
