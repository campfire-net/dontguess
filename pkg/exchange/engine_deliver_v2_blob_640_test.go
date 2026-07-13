package exchange_test

// engine_deliver_v2_blob_640_test.go — the done-gate for dontguess-640
// (large-content >32 KiB delivered encrypted via a Blossom CIPHERTEXT blob,
// docs/design/content-confidentiality-envelope-541.md §3.1(1), §3.2, §4.4 C1,
// §7 Phase 4).
//
// This is the FULL end-to-end round-trip with a SINGLE SHARED content-addressed
// MemoryBlobStore wired to the operator's engine and consulted by the buyer,
// over REAL secp256k1 identities + REAL nip44 + REAL ChaCha20-Poly1305 — nothing
// crypto is mocked, and the blob store is real:
//
//	seller offloads the CIPHERTEXT (never plaintext) to Blossom  →
//	operator FETCHES+verifies(sha256==ciphertext_hash)+decrypts+gates+folds
//	  (entry.BlobPointer set, entry.Content nil)  →
//	buy → match → buyer-accept → operator deliver emits ciphertext_ref.blob_pointer →
//	buyer FETCHES the SAME blob, verifies, unwraps the CEK, AEAD-decrypts  →
//	recovers the ORIGINAL >32 KiB plaintext BYTE-FOR-BYTE.
//
// It also proves the CONFIDENTIALITY property for the offload path: the stored
// blob holds AEAD ciphertext, never plaintext (sha256(blob)==ciphertext_hash and
// the plaintext marker is absent from the blob), and the emitted deliver carries
// no content/ciphertext.

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	"golang.org/x/crypto/chacha20poly1305"

	"github.com/3dl-dev/dontguess/pkg/exchange"
	"github.com/3dl-dev/dontguess/pkg/identity"
	"github.com/3dl-dev/dontguess/pkg/nip44"
	"github.com/3dl-dev/dontguess/pkg/store"
)

const v2BlobPlaintextMarker = "OVERSIZE-640-SECRET-MARKER "

// buildV2BlobPutPayload builds the kind-3401 v2 confidential put payload for an
// OVERSIZE entry EXACTLY as relayclient.buildPutMessage does on the offload path:
// a real CEK, real AEAD, the CEK NIP-44-wrapped to the operator, and the
// CIPHERTEXT (nonce||AEAD) offloaded to the shared Blossom store — the wire
// carries enc.blob_pointer (not enc.ciphertext). Returns the marshaled payload,
// the raw CEK (so the buyer round-trip can be proven), the blob pointer, the
// ciphertext_hash, and the ciphertext bytes (so a test can assert the blob holds
// exactly them).
func buildV2BlobPutPayload(t *testing.T, seller identity.Signer, operatorPubHex, desc string, plaintext []byte, tokenCost int64, blobStore exchange.BlobStore) (payload []byte, cek []byte, pointer, ciphertextHash string, ciphertext []byte) {
	t.Helper()
	if len(plaintext) <= exchange.BlossomOffloadThreshold {
		t.Fatalf("test setup: plaintext %d must exceed BlossomOffloadThreshold %d for the offload path", len(plaintext), exchange.BlossomOffloadThreshold)
	}
	cek = make([]byte, chacha20poly1305.KeySize)
	if _, err := rand.Read(cek); err != nil {
		t.Fatalf("gen CEK: %v", err)
	}
	aead, err := chacha20poly1305.New(cek)
	if err != nil {
		t.Fatalf("init AEAD: %v", err)
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		t.Fatalf("gen nonce: %v", err)
	}
	ciphertext = aead.Seal(nonce, nonce, plaintext, nil)
	sum := sha256.Sum256(ciphertext)
	ciphertextHash = "sha256:" + hex.EncodeToString(sum[:])

	// Offload the CIPHERTEXT (never plaintext) to the shared store.
	pointer, err = blobStore.Put(ciphertext)
	if err != nil {
		t.Fatalf("offload ciphertext to blob store: %v", err)
	}

	wrapped, err := nip44.Seal(seller, operatorPubHex, cek)
	if err != nil {
		t.Fatalf("wrap CEK to operator: %v", err)
	}
	payload, err = json.Marshal(map[string]any{
		"v":            2,
		"description":  desc,
		"token_cost":   tokenCost,
		"content_type": "exchange:content-type:code",
		"domains":      []string{"go"},
		"enc": map[string]any{
			"content_alg":     "chacha20poly1305",
			"ciphertext_hash": ciphertextHash,
			"blob_pointer":    pointer, // OFFLOAD: no inline "ciphertext" field
			"key_wrap": map[string]any{
				"alg":       "nip44-v2-secp256k1",
				"recipient": operatorPubHex,
				"wrapped":   wrapped,
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal v2 blob put: %v", err)
	}
	return payload, cek, pointer, ciphertextHash, ciphertext
}

// TestE2E_V2BlobOffload_RoundTrip_RecoversOriginalPlaintext is the dontguess-640
// done-gate. See the file header for the full flow.
func TestE2E_V2BlobOffload_RoundTrip_RecoversOriginalPlaintext(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	operator, seller, buyer := useSecpIdentities(t, h)

	shared := exchange.NewMemoryBlobStore()
	eng := h.newEngineWithOpts(func(o *exchange.EngineOptions) {
		o.OperatorPublicKey = operator.PubKeyHex()
		o.OperatorSigner = operator
	})
	// The operator gates the offloaded put by FETCHING its ciphertext from the
	// SAME store the seller offloaded to. Wire it before any fold/Replay.
	eng.State().SetBlobStore(shared)

	// A distinctive, >32 KiB plaintext so we can prove byte-for-byte recovery and
	// that the marker never appears in the blob or on any wire.
	plaintext := bytes.Repeat([]byte(v2BlobPlaintextMarker), 2500) // ~67.5 KiB
	if len(plaintext) <= exchange.BlossomOffloadThreshold {
		t.Fatalf("plaintext %d not oversize", len(plaintext))
	}

	const desc = "reusable go flock file-lock contention test pattern for concurrent access"
	putPayload, cek, pointer, ciphertextHash, ciphertext := buildV2BlobPutPayload(
		t, seller, operator.PubKeyHex(), desc, plaintext, 50000, shared)

	// ── seller publishes the offloaded put; operator folds it ──
	putMsg := h.sendMessage(h.seller, putPayload,
		[]string{exchange.TagPut, "exchange:content-type:code"}, nil)
	msgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))
	if err := eng.AutoAcceptPut(putMsg.ID, 2100, time.Now().Add(72*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut: %v", err)
	}

	// ── operator fold assertions: fetched+decrypted+gated, offloaded on the entry ──
	inv := eng.State().Inventory()
	if len(inv) == 0 {
		t.Fatal("oversize v2 put did NOT fold — operator fetch+verify+decrypt+gate failed (fail-closed drop)")
	}
	entry := inv[0]
	if entry.BlobPointer != pointer {
		t.Fatalf("entry.BlobPointer = %q, want the offload pointer %q", entry.BlobPointer, pointer)
	}
	if entry.Content != nil {
		t.Fatalf("entry.Content is non-nil (%d bytes) — an offloaded v2 entry must keep bytes in the blob ONLY (§4059), never inline", len(entry.Content))
	}
	if entry.CiphertextHash != ciphertextHash {
		t.Fatalf("entry.CiphertextHash = %q, want %q", entry.CiphertextHash, ciphertextHash)
	}
	plainSum := sha256.Sum256(plaintext)
	wantContentHash := "sha256:" + hex.EncodeToString(plainSum[:])
	if entry.ContentHash != wantContentHash {
		t.Fatalf("entry.ContentHash = %q, want sha256(plaintext) %q (operator-local dedup key, computed post-decrypt §4.4)", entry.ContentHash, wantContentHash)
	}
	if entry.WrappedCEKOperator == "" {
		t.Fatal("entry.WrappedCEKOperator empty — deliver cannot re-derive the CEK")
	}
	if entry.ContentSize != int64(len(plaintext)) {
		t.Fatalf("entry.ContentSize = %d, want plaintext size %d (gates ran on the decrypted plaintext)", entry.ContentSize, len(plaintext))
	}

	// ── the blob holds CIPHERTEXT, never plaintext ──
	blob, err := shared.Fetch(pointer)
	if err != nil {
		t.Fatalf("fetch stored blob: %v", err)
	}
	if !bytes.Equal(blob, ciphertext) {
		t.Fatal("stored blob != the seller's ciphertext bytes")
	}
	if bytes.Contains(blob, []byte(v2BlobPlaintextMarker)) {
		t.Fatal("BLOSSOM BLOB CONTAINS PLAINTEXT — the blob must hold AEAD ciphertext only (§4.4 C1)")
	}
	if s := sha256.Sum256(blob); "sha256:"+hex.EncodeToString(s[:]) != ciphertextHash {
		t.Fatal("sha256(blob) != ciphertext_hash — blob is not the committed ciphertext")
	}

	// ── buy → match → buyer-accept → operator deliver ──
	buyMsg := h.sendMessage(h.buyer,
		buyPayload("go flock contention test pattern for concurrent lock access", 50000),
		[]string{exchange.TagBuy}, nil)
	buyRec, _ := h.st.GetMessage(buyMsg.ID)
	eng.State().Apply(exchange.FromStoreRecord(buyRec))
	if err := eng.DispatchForTest(exchange.FromStoreRecord(buyRec)); err != nil {
		t.Fatalf("DispatchForTest buy: %v", err)
	}
	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))
	matchMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})
	if len(matchMsgs) == 0 {
		t.Fatal("no match emitted for the oversize v2 entry")
	}
	matchRec := matchMsgs[len(matchMsgs)-1]

	buyerAcceptPayload, _ := json.Marshal(map[string]any{
		"phase": "buyer-accept", "entry_id": entry.EntryID, "accepted": true,
	})
	buyerAcceptMsg := h.sendMessage(h.buyer, buyerAcceptPayload,
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrBuyerAccept,
			exchange.TagVerdictPrefix + "accepted",
		},
		[]string{matchRec.ID})
	allMsgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	deliverTriggerPayload, _ := json.Marshal(map[string]any{
		"phase": "deliver", "entry_id": entry.EntryID,
	})
	deliverTrigger := h.sendMessage(h.operator, deliverTriggerPayload,
		[]string{exchange.TagSettle, exchange.TagPhasePrefix + exchange.SettlePhaseStrDeliver},
		[]string{buyerAcceptMsg.ID})
	allMsgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))
	deliverRec, _ := h.st.GetMessage(deliverTrigger.ID)
	if err := eng.DispatchForTest(exchange.FromStoreRecord(deliverRec)); err != nil {
		t.Fatalf("DispatchForTest deliver: %v", err)
	}

	// ── deliver references the BLOB (not an inline put_event), carries no plaintext ──
	p := findV2DeliverPayload(t, h)
	if p == nil {
		t.Fatal("operator emitted no v2 deliver payload for the oversize entry")
	}
	if p.Content != "" || p.Ciphertext != "" {
		t.Fatalf("deliver leaks content/ciphertext (content=%d ciphertext=%d)", len(p.Content), len(p.Ciphertext))
	}
	if p.CiphertextHash != ciphertextHash {
		t.Fatalf("deliver ciphertext_hash = %q, want %q", p.CiphertextHash, ciphertextHash)
	}
	var ref struct {
		PutEvent    string `json:"put_event"`
		BlobPointer string `json:"blob_pointer"`
	}
	if err := json.Unmarshal(p.CiphertextRef, &ref); err != nil {
		t.Fatalf("unmarshal ciphertext_ref: %v", err)
	}
	if ref.BlobPointer != pointer {
		t.Fatalf("deliver ciphertext_ref.blob_pointer = %q, want the offload pointer %q", ref.BlobPointer, pointer)
	}
	if ref.PutEvent != "" {
		t.Fatalf("deliver ciphertext_ref carries put_event %q for an OFFLOADED entry — must reference the blob only", ref.PutEvent)
	}
	if p.KeyWrap.Recipient != buyer.PubKeyHex() {
		t.Fatalf("key_wrap.recipient = %q, want antecedent buyer %q", p.KeyWrap.Recipient, buyer.PubKeyHex())
	}

	// ── BUYER leg: fetch the SAME blob, verify, unwrap CEK, AEAD-decrypt ──
	fetched, err := shared.Fetch(ref.BlobPointer)
	if err != nil {
		t.Fatalf("buyer fetch blob %q: %v", ref.BlobPointer, err)
	}
	if s := sha256.Sum256(fetched); "sha256:"+hex.EncodeToString(s[:]) != p.CiphertextHash {
		t.Fatal("buyer: sha256(fetched blob) != deliver ciphertext_hash — must abort, not settle(complete)")
	}
	buyerCEK, err := nip44.Open(buyer, operator.PubKeyHex(), p.KeyWrap.Wrapped)
	if err != nil {
		t.Fatalf("buyer unwrap CEK: %v", err)
	}
	if !bytes.Equal(buyerCEK, cek) {
		t.Fatal("buyer-unwrapped CEK != the seller's original CEK")
	}
	aead, err := chacha20poly1305.New(buyerCEK)
	if err != nil {
		t.Fatalf("buyer init AEAD: %v", err)
	}
	ns := aead.NonceSize()
	if len(fetched) < ns {
		t.Fatalf("fetched blob %d shorter than nonce %d", len(fetched), ns)
	}
	recovered, err := aead.Open(nil, fetched[:ns], fetched[ns:], nil)
	if err != nil {
		t.Fatalf("buyer AEAD open: %v", err)
	}
	if !bytes.Equal(recovered, plaintext) {
		t.Fatalf("BYTE-FOR-BYTE MISMATCH: recovered %d bytes != original %d bytes", len(recovered), len(plaintext))
	}
	// Belt-and-suspenders: an UNFUNDED third party who scrapes the deliver + the
	// (unauthenticated) blob still cannot decrypt — the CEK is wrapped to the buyer.
	stranger, _ := identity.Generate()
	if _, err := nip44.Open(stranger, operator.PubKeyHex(), p.KeyWrap.Wrapped); err == nil {
		t.Fatal("a stranger unwrapped the CEK — the blob path leaks the key (recipient not binding)")
	}
}
