package exchange_test

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/campfire-net/campfire/pkg/store"

	"github.com/campfire-net/dontguess/pkg/exchange"
)

// TestCompressionDerivative_FullLifecycle exercises the complete path:
//
//  1. Seller puts raw entry → operator accepts → entry live in inventory.
//  2. Operator posts a compress assign task for the raw entry.
//  3. Agent claims → completes with compressed content_hash + content_size.
//  4. Operator accept-assign is dispatched.
//  5. Verify: derivative inventory entry exists with CompressedFrom == original EntryID.
//  6. Verify: buyer can match against the derivative (it appears in match index).
func TestCompressionDerivative_FullLifecycle(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	// ── Step 1: put raw entry ────────────────────────────────────────────────
	rawHash := "sha256:" + fmt.Sprintf("%064x", 99)
	putMsg := h.sendMessage(h.seller,
		putPayload("Explain Go concurrency patterns in depth", rawHash, "analysis", 20000, 48000),
		[]string{exchange.TagPut, "exchange:content-type:analysis", "exchange:domain:go"},
		nil,
	)

	msgs, err := h.st.ListMessages(h.cfID, 0)
	if err != nil {
		t.Fatalf("ListMessages after put: %v", err)
	}
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	if err := eng.AutoAcceptPut(putMsg.ID, 14000, time.Now().Add(168*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut: %v", err)
	}

	inv := eng.State().Inventory()
	if len(inv) != 1 {
		t.Fatalf("expected 1 inventory entry after put-accept, got %d", len(inv))
	}
	origEntry := inv[0]
	origEntryID := origEntry.EntryID

	// Match index should have the raw entry.
	if n := eng.MatchIndexLen(); n != 1 {
		t.Errorf("match index len after put-accept = %d, want 1", n)
	}

	// ── Step 2: operator posts compress assign ───────────────────────────────
	assignPayload, _ := json.Marshal(map[string]any{
		"entry_id":  origEntryID,
		"task_type": "compress",
		"reward":    100,
	})
	assignMsg := h.sendMessage(h.operator, assignPayload, []string{exchange.TagAssign}, nil)

	msgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	assignRec, err := h.st.GetMessage(assignMsg.ID)
	if err != nil {
		t.Fatalf("GetMessage assign: %v", err)
	}
	if err := eng.DispatchForTest(exchange.FromStoreRecord(assignRec)); err != nil {
		t.Fatalf("DispatchForTest assign: %v", err)
	}

	// ── Step 3: agent claims ─────────────────────────────────────────────────
	worker := newTestAgent(t)
	claimMsg := h.sendMessage(worker, []byte(`{}`), []string{exchange.TagAssignClaim}, []string{assignMsg.ID})

	msgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	claimRec, err := h.st.GetMessage(claimMsg.ID)
	if err != nil {
		t.Fatalf("GetMessage claim: %v", err)
	}
	if err := eng.DispatchForTest(exchange.FromStoreRecord(claimRec)); err != nil {
		t.Fatalf("DispatchForTest claim: %v", err)
	}

	// ── Step 4: agent completes with compressed payload ──────────────────────
	compressedHash := "sha256:" + fmt.Sprintf("%064x", 100)
	compressedSize := int64(12000) // smaller than the original 48000 bytes
	completeResult, _ := json.Marshal(map[string]any{
		"content_hash": compressedHash,
		"content_size": compressedSize,
	})
	completeMsg := h.sendMessage(worker, completeResult, []string{exchange.TagAssignComplete}, []string{claimMsg.ID})

	msgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	completeRec, err := h.st.GetMessage(completeMsg.ID)
	if err != nil {
		t.Fatalf("GetMessage complete: %v", err)
	}
	if err := eng.DispatchForTest(exchange.FromStoreRecord(completeRec)); err != nil {
		t.Fatalf("DispatchForTest complete: %v", err)
	}

	// ── Step 5: operator accepts the compression result ──────────────────────
	acceptMsg := h.sendMessage(h.operator, []byte(`{}`), []string{exchange.TagAssignAccept}, []string{completeMsg.ID})

	msgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	acceptRec, err := h.st.GetMessage(acceptMsg.ID)
	if err != nil {
		t.Fatalf("GetMessage accept: %v", err)
	}
	if err := eng.DispatchForTest(exchange.FromStoreRecord(acceptRec)); err != nil {
		t.Fatalf("DispatchForTest accept: %v", err)
	}

	// ── Step 6: verify derivative entry in inventory ─────────────────────────
	inv = eng.State().Inventory()
	if len(inv) != 2 {
		t.Fatalf("expected 2 inventory entries (original + derivative), got %d", len(inv))
	}

	// Find the derivative.
	var derivative *exchange.InventoryEntry
	for _, e := range inv {
		if e.CompressedFrom != "" {
			derivative = e
			break
		}
	}
	if derivative == nil {
		t.Fatal("no derivative entry found (CompressedFrom is empty on all entries)")
	}

	// Verify derivative fields.
	if derivative.CompressedFrom != origEntryID {
		t.Errorf("derivative.CompressedFrom = %q, want %q", derivative.CompressedFrom, origEntryID)
	}
	if derivative.ContentHash != compressedHash {
		t.Errorf("derivative.ContentHash = %q, want %q", derivative.ContentHash, compressedHash)
	}
	if derivative.ContentSize != compressedSize {
		t.Errorf("derivative.ContentSize = %d, want %d", derivative.ContentSize, compressedSize)
	}
	if derivative.ContentType != origEntry.ContentType {
		t.Errorf("derivative.ContentType = %q, want %q (same as original)", derivative.ContentType, origEntry.ContentType)
	}
	if derivative.Description != origEntry.Description {
		t.Errorf("derivative.Description = %q, want %q (inherited from original)", derivative.Description, origEntry.Description)
	}
	if derivative.SellerKey != origEntry.SellerKey {
		t.Errorf("derivative.SellerKey = %q, want %q (inherited from original)", derivative.SellerKey, origEntry.SellerKey)
	}
	// Verify antecedent: PutMsgID should be the accept message ID.
	if derivative.PutMsgID != acceptMsg.ID {
		t.Errorf("derivative.PutMsgID = %q, want acceptMsg.ID %q", derivative.PutMsgID, acceptMsg.ID)
	}

	// ── Step 7: verify derivative appears in match index ─────────────────────
	// Both the original and the derivative should be indexed (2 entries).
	if n := eng.MatchIndexLen(); n != 2 {
		t.Errorf("match index len after compression derivative = %d, want 2 (original + derivative)", n)
	}
}

// TestCompressionDerivative_InvalidContentHashDropped verifies that a
// compress-assign-accept whose complete payload contains a content_hash
// without the required "sha256:" prefix does NOT create a derivative entry.
// The accept is still processed (bounty paid) but the invalid hash is rejected.
func TestCompressionDerivative_InvalidContentHashDropped(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	// Put and accept a raw entry.
	rawHash := "sha256:" + fmt.Sprintf("%064x", 42)
	putMsg := h.sendMessage(h.seller,
		putPayload("Rust ownership explained", rawHash, "analysis", 15000, 30000),
		[]string{exchange.TagPut, "exchange:content-type:analysis", "exchange:domain:rust"},
		nil,
	)
	msgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))
	if err := eng.AutoAcceptPut(putMsg.ID, 10000, time.Now().Add(72*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut: %v", err)
	}

	inv := eng.State().Inventory()
	if len(inv) != 1 {
		t.Fatalf("expected 1 inventory entry, got %d", len(inv))
	}
	origEntryID := inv[0].EntryID

	// Post compression assign.
	assignPayload, _ := json.Marshal(map[string]any{
		"entry_id":  origEntryID,
		"task_type": "compress",
		"reward":    500,
	})
	assignMsg := h.sendMessage(h.operator, assignPayload, []string{exchange.TagAssign}, nil)
	msgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))
	if err := eng.DispatchForTest(exchange.FromStoreRecord(mustGetStoreRecord(t, h, assignMsg.ID))); err != nil {
		t.Fatalf("DispatchForTest assign: %v", err)
	}

	worker := newTestAgent(t)
	claimMsg := h.sendMessage(worker, []byte(`{}`), []string{exchange.TagAssignClaim}, []string{assignMsg.ID})
	msgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))
	if err := eng.DispatchForTest(exchange.FromStoreRecord(mustGetStoreRecord(t, h, claimMsg.ID))); err != nil {
		t.Fatalf("DispatchForTest claim: %v", err)
	}

	// Complete with an invalid content_hash (md5 prefix, not sha256:).
	invalidHash := "md5:cafebabe"
	completeResult, _ := json.Marshal(map[string]any{
		"content_hash": invalidHash,
		"content_size": int64(8000),
	})
	completeMsg := h.sendMessage(worker, completeResult, []string{exchange.TagAssignComplete}, []string{claimMsg.ID})
	msgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))
	if err := eng.DispatchForTest(exchange.FromStoreRecord(mustGetStoreRecord(t, h, completeMsg.ID))); err != nil {
		t.Fatalf("DispatchForTest complete: %v", err)
	}

	// Operator accepts (non-fatal — derivative creation fails, bounty still paid).
	acceptMsg := h.sendMessage(h.operator, []byte(`{}`), []string{exchange.TagAssignAccept}, []string{completeMsg.ID})
	msgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))
	if err := eng.DispatchForTest(exchange.FromStoreRecord(mustGetStoreRecord(t, h, acceptMsg.ID))); err != nil {
		t.Fatalf("DispatchForTest accept: %v", err)
	}

	// Inventory must still have exactly one entry — no derivative with the invalid hash.
	inv = eng.State().Inventory()
	if len(inv) != 1 {
		t.Fatalf("invalid content_hash should not create derivative: expected 1 inventory entry, got %d", len(inv))
	}
	if inv[0].CompressedFrom != "" {
		t.Errorf("unexpected CompressedFrom on original entry after invalid-hash accept: %q", inv[0].CompressedFrom)
	}
}

// TestCompressionDerivative_NonCompressTaskNoDerivative verifies that a
// non-compress task type (e.g. "freshness") does NOT create a derivative entry.
func TestCompressionDerivative_NonCompressTaskNoDerivative(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	// Put and accept a raw entry.
	rawHash := "sha256:" + fmt.Sprintf("%064x", 77)
	putMsg := h.sendMessage(h.seller,
		putPayload("Python async patterns", rawHash, "code", 10000, 20000),
		[]string{exchange.TagPut, "exchange:content-type:code", "exchange:domain:python"},
		nil,
	)
	msgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))
	if err := eng.AutoAcceptPut(putMsg.ID, 7000, time.Now().Add(72*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut: %v", err)
	}

	inv := eng.State().Inventory()
	if len(inv) != 1 {
		t.Fatalf("expected 1 inventory entry, got %d", len(inv))
	}
	origEntryID := inv[0].EntryID

	// Freshness assign (not "compress").
	assignPayload, _ := json.Marshal(map[string]any{
		"entry_id":  origEntryID,
		"task_type": "freshness",
		"reward":    50,
	})
	assignMsg := h.sendMessage(h.operator, assignPayload, []string{exchange.TagAssign}, nil)
	msgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))
	if err := eng.DispatchForTest(exchange.FromStoreRecord(mustGetStoreRecord(t, h, assignMsg.ID))); err != nil {
		t.Fatalf("DispatchForTest assign: %v", err)
	}

	worker := newTestAgent(t)
	claimMsg := h.sendMessage(worker, []byte(`{}`), []string{exchange.TagAssignClaim}, []string{assignMsg.ID})
	msgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))
	if err := eng.DispatchForTest(exchange.FromStoreRecord(mustGetStoreRecord(t, h, claimMsg.ID))); err != nil {
		t.Fatalf("DispatchForTest claim: %v", err)
	}

	completeMsg := h.sendMessage(worker, []byte(`{"result":"still fresh"}`), []string{exchange.TagAssignComplete}, []string{claimMsg.ID})
	msgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))
	if err := eng.DispatchForTest(exchange.FromStoreRecord(mustGetStoreRecord(t, h, completeMsg.ID))); err != nil {
		t.Fatalf("DispatchForTest complete: %v", err)
	}

	acceptMsg := h.sendMessage(h.operator, []byte(`{}`), []string{exchange.TagAssignAccept}, []string{completeMsg.ID})
	msgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))
	if err := eng.DispatchForTest(exchange.FromStoreRecord(mustGetStoreRecord(t, h, acceptMsg.ID))); err != nil {
		t.Fatalf("DispatchForTest accept: %v", err)
	}

	// Inventory should still have only one entry — no derivative.
	inv = eng.State().Inventory()
	if len(inv) != 1 {
		t.Fatalf("freshness accept should not create derivative: expected 1 entry, got %d", len(inv))
	}
	if inv[0].CompressedFrom != "" {
		t.Errorf("unexpected CompressedFrom on non-compress task: %q", inv[0].CompressedFrom)
	}
}

// mustGetStoreRecord is a test helper that retrieves a raw store.MessageRecord
// from the store or fatals the test. Callers convert to *exchange.Message via
// exchange.FromStoreRecord.
func mustGetStoreRecord(t *testing.T, h *testHarness, id string) *store.MessageRecord {
	t.Helper()
	rec, err := h.st.GetMessage(id)
	if err != nil {
		t.Fatalf("GetMessage %s: %v", id, err)
	}
	return rec
}

// TestCompressionDerivative_ReplayIdempotent verifies that replaying the full
// compression lifecycle (put → assign → claim → complete → accept) a second
// time — simulating an engine restart — does not produce duplicate inventory
// entries. After one full lifecycle and one replay, inventory must contain
// exactly 2 entries: 1 raw + 1 derivative.
func TestCompressionDerivative_ReplayIdempotent(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	// ── Step 1: put raw entry ────────────────────────────────────────────────
	rawHash := "sha256:" + fmt.Sprintf("%064x", 55)
	putMsg := h.sendMessage(h.seller,
		putPayload("Explain Rust ownership model", rawHash, "analysis", 15000, 36000),
		[]string{exchange.TagPut, "exchange:content-type:analysis", "exchange:domain:rust"},
		nil,
	)

	msgs, err := h.st.ListMessages(h.cfID, 0)
	if err != nil {
		t.Fatalf("ListMessages after put: %v", err)
	}
	eng.State().Replay(exchange.FromStoreRecords(msgs))
	if err := eng.AutoAcceptPut(putMsg.ID, 10000, time.Now().Add(72*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut: %v", err)
	}

	inv := eng.State().Inventory()
	if len(inv) != 1 {
		t.Fatalf("expected 1 inventory entry after put-accept, got %d", len(inv))
	}
	origEntryID := inv[0].EntryID

	// ── Step 2: operator posts compress assign ───────────────────────────────
	assignPayload, _ := json.Marshal(map[string]any{
		"entry_id":  origEntryID,
		"task_type": "compress",
		"reward":    80,
	})
	assignMsg := h.sendMessage(h.operator, assignPayload, []string{exchange.TagAssign}, nil)

	msgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))
	if err := eng.DispatchForTest(exchange.FromStoreRecord(mustGetStoreRecord(t, h, assignMsg.ID))); err != nil {
		t.Fatalf("DispatchForTest assign: %v", err)
	}

	// ── Step 3: agent claims ─────────────────────────────────────────────────
	worker := newTestAgent(t)
	claimMsg := h.sendMessage(worker, []byte(`{}`), []string{exchange.TagAssignClaim}, []string{assignMsg.ID})

	msgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))
	if err := eng.DispatchForTest(exchange.FromStoreRecord(mustGetStoreRecord(t, h, claimMsg.ID))); err != nil {
		t.Fatalf("DispatchForTest claim: %v", err)
	}

	// ── Step 4: agent completes ──────────────────────────────────────────────
	compressedHash := "sha256:" + fmt.Sprintf("%064x", 56)
	completeResult, _ := json.Marshal(map[string]any{
		"content_hash": compressedHash,
		"content_size": int64(9000),
	})
	completeMsg := h.sendMessage(worker, completeResult, []string{exchange.TagAssignComplete}, []string{claimMsg.ID})

	msgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))
	if err := eng.DispatchForTest(exchange.FromStoreRecord(mustGetStoreRecord(t, h, completeMsg.ID))); err != nil {
		t.Fatalf("DispatchForTest complete: %v", err)
	}

	// ── Step 5: operator accepts ─────────────────────────────────────────────
	acceptMsg := h.sendMessage(h.operator, []byte(`{}`), []string{exchange.TagAssignAccept}, []string{completeMsg.ID})

	msgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))
	if err := eng.DispatchForTest(exchange.FromStoreRecord(mustGetStoreRecord(t, h, acceptMsg.ID))); err != nil {
		t.Fatalf("DispatchForTest accept: %v", err)
	}

	// Verify: exactly 2 entries after the first pass.
	inv = eng.State().Inventory()
	if len(inv) != 2 {
		t.Fatalf("after lifecycle: expected 2 inventory entries (1 raw + 1 derivative), got %d", len(inv))
	}

	// ── Step 6: simulate engine restart by replaying all messages again ───────
	// Re-dispatch the accept message as if the engine restarted and is
	// re-processing its message log. The derivative ID must be stable, and
	// applyDerivativePut must be idempotent — no new entry should be created.
	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))
	if err := eng.DispatchForTest(exchange.FromStoreRecord(mustGetStoreRecord(t, h, acceptMsg.ID))); err != nil {
		t.Fatalf("DispatchForTest accept (replay): %v", err)
	}

	// Verify: still exactly 2 entries — replay must not duplicate the derivative.
	inv = eng.State().Inventory()
	if len(inv) != 2 {
		t.Fatalf("after replay: expected 2 inventory entries (1 raw + 1 derivative), got %d (duplicate derivative created)", len(inv))
	}

	// Confirm the derivative is still correct.
	var derivative *exchange.InventoryEntry
	for _, e := range inv {
		if e.CompressedFrom != "" {
			derivative = e
			break
		}
	}
	if derivative == nil {
		t.Fatal("no derivative entry found after replay")
	}
	if derivative.CompressedFrom != origEntryID {
		t.Errorf("derivative.CompressedFrom = %q, want %q", derivative.CompressedFrom, origEntryID)
	}
	if derivative.ContentHash != compressedHash {
		t.Errorf("derivative.ContentHash = %q, want %q", derivative.ContentHash, compressedHash)
	}
}
