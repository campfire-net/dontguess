package exchange_test

// Engine-level round-trip tests for the Blossom deliver path (dontguess-7783,
// dontguess-05d2).
//
// Covered:
//   - Full flow: oversize put (offloaded to Blossom) -> put-accept -> buy ->
//     match -> operator deliver trigger -> engine emits a settle(deliver)
//     message carrying a Blossom POINTER + content_hash — never the full
//     bytes (docs/design/nostr-first-rebuild-decision.md L114/L183: "full
//     deliver is a Blossom pointer + client-side hash verification").
//   - Client-side verify-on-fetch: the buyer fetches entry.BlobPointer from
//     the blob store directly and verifies the fetched bytes' sha256 against
//     the delivered content_hash. A healthy blob verifies and matches the
//     original put content exactly (not the inline preview slice).
//   - Tampered blob: the operator still delivers the pointer (it does not
//     fetch or gate on blob health at deliver time — verification is the
//     client's job, per the design), but the buyer's client-side verify
//     catches the mismatch and refuses to trust the fetched bytes.
//   - Size guard: content that exceeded BlossomOffloadThreshold but somehow
//     lacks a BlobPointer (e.g. no blob store was configured at put time) is
//     NEVER inlined into an outgoing settle(deliver) message — the engine
//     refuses to emit any content-bearing or pointer-bearing deliver message
//     at all rather than leak oversize bytes onto the relay.

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	"github.com/campfire-net/campfire/cf-protocol/store"

	"github.com/campfire-net/dontguess/pkg/exchange"
)

// buildOversizeDeliverableState mirrors buildDeliverableState (settle_deliver_test.go)
// but uses content large enough to trigger Blossom offload, with a blob store
// configured on the engine's state before any put is processed.
func buildOversizeDeliverableState(t *testing.T, h *testHarness, eng *exchange.Engine, blobStore exchange.BlobStore) (
	deliverMsg *exchange.Message,
	originalContent []byte,
) {
	t.Helper()

	eng.State().SetBlobStore(blobStore)

	entryID, deliverMsg, originalContent := seedOversizePut(t, h, eng, "TestEngineDeliverBlossom round trip")

	inv := eng.State().Inventory()
	found := false
	for _, e := range inv {
		if e.EntryID == entryID {
			found = true
			if e.BlobPointer == "" {
				t.Fatal("expected oversize entry to have a BlobPointer after put-accept")
			}
		}
	}
	if !found {
		t.Fatal("expected inventory entry after put-accept")
	}

	return deliverMsg, originalContent
}

// buildOversizeDeliverableStateNoBlobStore mirrors buildOversizeDeliverableState
// but deliberately configures NO blob store, so oversize content is stored
// fully inline (BlobPointer == "") — the legacy/no-offload path. This models
// the defensive scenario the emitDeliverContent size guard protects against:
// an entry whose content exceeds BlossomOffloadThreshold but has no
// BlobPointer to deliver as a pointer instead.
func buildOversizeDeliverableStateNoBlobStore(t *testing.T, h *testHarness, eng *exchange.Engine) (
	deliverMsg *exchange.Message,
	originalContent []byte,
) {
	t.Helper()

	entryID, deliverMsg, originalContent := seedOversizePut(t, h, eng, "TestEngineDeliverBlossom size guard (no blob store)")

	inv := eng.State().Inventory()
	found := false
	for _, e := range inv {
		if e.EntryID == entryID {
			found = true
			if e.BlobPointer != "" {
				t.Fatal("expected no BlobPointer when no blob store is configured")
			}
			if len(e.Content) <= exchange.BlossomOffloadThreshold {
				t.Fatalf("expected inline content to exceed BlossomOffloadThreshold (%d), got %d",
					exchange.BlossomOffloadThreshold, len(e.Content))
			}
		}
	}
	if !found {
		t.Fatal("expected inventory entry after put-accept")
	}

	return deliverMsg, originalContent
}

// seedOversizePut drives put -> put-accept -> buy -> match -> buyer-accept ->
// operator deliver-trigger for a 64 KiB (> BlossomOffloadThreshold) entry.
// Whether the entry ends up offloaded depends solely on whether a blob store
// was configured on eng.State() before this call — callers set that up.
func seedOversizePut(t *testing.T, h *testHarness, eng *exchange.Engine, desc string) (
	entryID string,
	deliverMsg *exchange.Message,
	originalContent []byte,
) {
	t.Helper()

	// 64 KiB of line-structured pseudo-code — exceeds BlossomOffloadThreshold
	// (32 KiB). Line-structured (not a flat byte pattern) so PreviewAssembler's
	// boundary-snapping behaves normally (see buildLargePutPayload doc).
	var buf []byte
	for len(buf) < 64*1024 {
		buf = append(buf, []byte("func handler_"+string(rune('a'+(len(buf)/64)%26))+"(w, r) { return doWork(w, r) }\n")...)
	}
	originalContent = buf[:64*1024]

	putPayloadBytes, _ := json.Marshal(map[string]any{
		"description":  desc,
		"content":      base64.StdEncoding.EncodeToString(originalContent),
		"token_cost":   int64(2000000),
		"content_type": "analysis",
		"domains":      []string{"go", "testing"},
	})

	putMsg := h.sendMessage(h.seller, putPayloadBytes,
		[]string{exchange.TagPut, "exchange:content-type:analysis"},
		nil,
	)

	msgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	if err := eng.AutoAcceptPut(putMsg.ID, 1400000, time.Now().Add(168*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut: %v", err)
	}

	inv := eng.State().Inventory()
	if len(inv) == 0 {
		t.Fatal("expected inventory entry after put-accept")
	}
	entryID = inv[0].EntryID

	buyMsg := h.sendMessage(h.buyer,
		buyPayload("Find a large cached analysis document for testing round trips", 50000000),
		[]string{exchange.TagBuy},
		nil,
	)
	buyRec, _ := h.st.GetMessage(buyMsg.ID)
	eng.State().Apply(exchange.FromStoreRecord(buyRec))
	if err := eng.DispatchForTest(exchange.FromStoreRecord(buyRec)); err != nil {
		t.Fatalf("DispatchForTest buy: %v", err)
	}

	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	matchMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})
	if len(matchMsgs) == 0 {
		t.Fatal("no match message emitted")
	}
	matchRec := matchMsgs[len(matchMsgs)-1]

	// Buyer accepts directly (skip preview — not the concern of this test).
	buyerAcceptPayloadBytes, _ := json.Marshal(map[string]any{
		"phase":    "buyer-accept",
		"entry_id": entryID,
		"accepted": true,
	})
	buyerAcceptMsg := h.sendMessage(h.buyer, buyerAcceptPayloadBytes,
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrBuyerAccept,
			exchange.TagVerdictPrefix + "accepted",
		},
		[]string{matchRec.ID},
	)

	allMsgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	deliverTriggerPayload, _ := json.Marshal(map[string]any{
		"phase":    "deliver",
		"entry_id": entryID,
	})
	deliverMsg = h.sendMessage(h.operator, deliverTriggerPayload,
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrDeliver,
		},
		[]string{buyerAcceptMsg.ID},
	)

	allMsgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	return entryID, deliverMsg, originalContent
}

// deliverPointerPayload is the shape of the settle(deliver) message the
// engine emits for a Blossom-offloaded entry.
type deliverPointerPayload struct {
	BlobPointer string `json:"blob_pointer"`
	ContentHash string `json:"content_hash"`
}

// findDeliverContentMessage scans settle messages for an operator-emitted
// deliver message carrying an inline content field.
func findDeliverContentMessage(h *testHarness) *store.MessageRecord {
	return findOperatorDeliverMessage(h, "content")
}

// findDeliverPointerMessage scans settle messages for an operator-emitted
// deliver message carrying a blob_pointer field (the offloaded-entry shape).
func findDeliverPointerMessage(h *testHarness) *store.MessageRecord {
	return findOperatorDeliverMessage(h, "blob_pointer")
}

// findOperatorDeliverMessage scans settle messages for an operator-emitted,
// deliver-phase message whose payload has requiredField present.
func findOperatorDeliverMessage(h *testHarness, requiredField string) *store.MessageRecord {
	msgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagSettle}})
	for i := range msgs {
		m := &msgs[i]
		if m.Sender != h.operator.PublicKeyHex() {
			continue
		}
		hasDeliverPhase := false
		for _, tag := range m.Tags {
			if tag == exchange.TagPhasePrefix+exchange.SettlePhaseStrDeliver {
				hasDeliverPhase = true
				break
			}
		}
		if !hasDeliverPhase {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal(m.Payload, &payload); err != nil {
			continue
		}
		if _, has := payload[requiredField]; !has {
			continue
		}
		return m
	}
	return nil
}

// TestEngineDeliverBlossom_DeliversPointerNotInlineBytes verifies that for an
// oversize (Blossom-offloaded) entry, the deliver path emits a settle(deliver)
// message carrying a Blossom pointer + content_hash — and that the message
// does NOT carry an inline content field. This is the direct regression test
// for dontguess-05d2: the engine previously fetched-and-verified the full
// content server-side and STILL inlined it into the outgoing message,
// defeating the offload dontguess-7783 shipped to keep oversize bytes off
// the relay.
func TestEngineDeliverBlossom_DeliversPointerNotInlineBytes(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	blobStore := exchange.NewMemoryBlobStore()
	deliverMsg, _ := buildOversizeDeliverableState(t, h, eng, blobStore)

	deliverRec, _ := h.st.GetMessage(deliverMsg.ID)
	if err := eng.DispatchForTest(exchange.FromStoreRecord(deliverRec)); err != nil {
		t.Fatalf("DispatchForTest deliver: %v", err)
	}

	if contentMsg := findDeliverContentMessage(h); contentMsg != nil {
		t.Fatal("engine inlined full content for a Blossom-offloaded entry — must deliver a pointer instead (dontguess-05d2)")
	}

	pointerMsg := findDeliverPointerMessage(h)
	if pointerMsg == nil {
		t.Fatal("engine did not emit a settle(deliver) message with a blob_pointer field")
	}

	var payload deliverPointerPayload
	if err := json.Unmarshal(pointerMsg.Payload, &payload); err != nil {
		t.Fatalf("unmarshal deliver pointer payload: %v", err)
	}

	inv := eng.State().Inventory()
	if len(inv) == 0 {
		t.Fatal("expected inventory entry")
	}
	entry := inv[0]

	if payload.BlobPointer != entry.BlobPointer {
		t.Errorf("delivered blob_pointer = %q, want entry.BlobPointer %q", payload.BlobPointer, entry.BlobPointer)
	}
	if payload.ContentHash != entry.ContentHash {
		t.Errorf("delivered content_hash = %q, want entry.ContentHash %q", payload.ContentHash, entry.ContentHash)
	}
}

// TestEngineDeliverBlossom_ClientVerifiesFetchedBlobAgainstHash simulates the
// buyer's client-side responsibility per the shipped design: given the
// delivered blob_pointer + content_hash, fetch the blob directly and verify
// its sha256 before trusting it. Against a healthy blob store, the fetched
// bytes verify and match the ORIGINAL full content the seller put (not the
// inline preview slice).
func TestEngineDeliverBlossom_ClientVerifiesFetchedBlobAgainstHash(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	blobStore := exchange.NewMemoryBlobStore()
	deliverMsg, originalContent := buildOversizeDeliverableState(t, h, eng, blobStore)

	deliverRec, _ := h.st.GetMessage(deliverMsg.ID)
	if err := eng.DispatchForTest(exchange.FromStoreRecord(deliverRec)); err != nil {
		t.Fatalf("DispatchForTest deliver: %v", err)
	}

	pointerMsg := findDeliverPointerMessage(h)
	if pointerMsg == nil {
		t.Fatal("engine did not emit a settle(deliver) message with a blob_pointer field")
	}
	var payload deliverPointerPayload
	if err := json.Unmarshal(pointerMsg.Payload, &payload); err != nil {
		t.Fatalf("unmarshal deliver pointer payload: %v", err)
	}

	// Client-side: fetch the blob directly via the delivered pointer, then
	// verify its sha256 against the delivered content_hash BEFORE trusting it.
	fetched, err := blobStore.Fetch(payload.BlobPointer)
	if err != nil {
		t.Fatalf("client fetch via delivered blob_pointer: %v", err)
	}
	gotHash := sha256.Sum256(fetched)
	gotHashStr := "sha256:" + hex.EncodeToString(gotHash[:])
	if gotHashStr != payload.ContentHash {
		t.Fatalf("client-side verify failed on a healthy blob: fetched hash %s != delivered content_hash %s",
			gotHashStr, payload.ContentHash)
	}

	if len(fetched) != len(originalContent) {
		t.Fatalf("client-fetched content length = %d, want %d (full content, not the inline preview slice)",
			len(fetched), len(originalContent))
	}
	originalHash := sha256.Sum256(originalContent)
	if gotHash != originalHash {
		t.Errorf("client-fetched content hash mismatch:\n  got  sha256:%x\n  want sha256:%x", gotHash, originalHash)
	}
}

// TestEngineDeliverBlossom_ClientDetectsTamperedBlob verifies that when the
// blob resolvable via the delivered pointer does not match the delivered
// content_hash (simulated tamper/corruption at the blob host), the BUYER's
// client-side verify-on-fetch catches it. The operator still emits the
// pointer deliver message unconditionally — it does not fetch or gate on
// blob health at deliver time (that would defeat the point of not inlining);
// verification is the client's responsibility per the design
// (nostr-first-rebuild-decision.md L114/L183).
func TestEngineDeliverBlossom_ClientDetectsTamperedBlob(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	// A blob store double whose Fetch always returns content that does NOT
	// match whatever hash the caller expects — simulates a compromised or
	// corrupted Blossom host serving the wrong bytes for a pointer.
	tamperingStore := &tamperingBlobStore{inner: exchange.NewMemoryBlobStore()}

	deliverMsg, _ := buildOversizeDeliverableState(t, h, eng, tamperingStore)

	deliverRec, _ := h.st.GetMessage(deliverMsg.ID)
	if err := eng.DispatchForTest(exchange.FromStoreRecord(deliverRec)); err != nil {
		t.Fatalf("DispatchForTest deliver: %v", err)
	}

	pointerMsg := findDeliverPointerMessage(h)
	if pointerMsg == nil {
		t.Fatal("engine did not emit a settle(deliver) pointer message — operator should deliver the pointer regardless of blob health, per design")
	}
	var payload deliverPointerPayload
	if err := json.Unmarshal(pointerMsg.Payload, &payload); err != nil {
		t.Fatalf("unmarshal deliver pointer payload: %v", err)
	}

	// Client-side: fetch via the delivered pointer (through the tampering
	// store) and verify against the delivered content_hash.
	fetched, err := tamperingStore.Fetch(payload.BlobPointer)
	if err != nil {
		t.Fatalf("client fetch via delivered blob_pointer: %v", err)
	}
	gotHash := sha256.Sum256(fetched)
	gotHashStr := "sha256:" + hex.EncodeToString(gotHash[:])
	if gotHashStr == payload.ContentHash {
		t.Fatal("client-side verify did not detect a tampered blob — hash matched when it should not have")
	}
}

// TestEngineDeliverBlossom_OversizeContentNeverInlinedWithoutBlobPointer
// verifies the size guard in emitDeliverContent: an entry whose content
// exceeds BlossomOffloadThreshold but has no BlobPointer (e.g. no blob store
// was configured at put time, so the legacy inline-everything path stored it
// fully inline) must NEVER be inlined into an outgoing settle(deliver)
// message. The engine refuses to emit any content-bearing OR pointer-bearing
// deliver message in this case — there is no pointer to deliver, and the
// bytes are too large to inline.
func TestEngineDeliverBlossom_OversizeContentNeverInlinedWithoutBlobPointer(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()
	// Deliberately no SetBlobStore call — content stays inline regardless of size.

	deliverMsg, _ := buildOversizeDeliverableStateNoBlobStore(t, h, eng)

	preMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagSettle}})
	preCount := len(preMsgs)

	deliverRec, _ := h.st.GetMessage(deliverMsg.ID)
	if err := eng.DispatchForTest(exchange.FromStoreRecord(deliverRec)); err != nil {
		t.Fatalf("DispatchForTest deliver: %v", err)
	}

	if contentMsg := findDeliverContentMessage(h); contentMsg != nil {
		t.Fatal("engine inlined oversize content that has no BlobPointer — size guard did not stop delivery")
	}
	if pointerMsg := findDeliverPointerMessage(h); pointerMsg != nil {
		t.Fatal("engine emitted a blob_pointer deliver message for an entry with no BlobPointer")
	}

	postMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagSettle}})
	if len(postMsgs) > preCount {
		t.Fatalf("expected no new settle messages after a size-guard-refused deliver, got %d new", len(postMsgs)-preCount)
	}
}

// tamperingBlobStore wraps a real BlobStore but returns corrupted bytes from
// Fetch (Put still stores the real content, so the corruption is only
// observable via the hash-mismatch it causes on Fetch — modeling a blob host
// that serves the wrong bytes for a given pointer).
type tamperingBlobStore struct {
	inner exchange.BlobStore
}

func (t *tamperingBlobStore) Put(content []byte) (string, error) {
	return t.inner.Put(content)
}

func (t *tamperingBlobStore) Fetch(pointer string) ([]byte, error) {
	content, err := t.inner.Fetch(pointer)
	if err != nil {
		return nil, err
	}
	// Corrupt: flip the first byte if present.
	if len(content) > 0 {
		content[0] ^= 0xFF
	}
	return content, nil
}
