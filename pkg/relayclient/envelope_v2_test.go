package relayclient

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/3dl-dev/dontguess/pkg/exchange"
	"github.com/3dl-dev/dontguess/pkg/identity"
	"github.com/3dl-dev/dontguess/pkg/nip44"
	"golang.org/x/crypto/chacha20poly1305"
)

// TestBuildPutMessage_V2Envelope_NoPlaintextLeak_And_RoundTrip is the
// done-gate for dontguess-58f. It builds a put via buildPutMessage with REAL
// seller + operator secp256k1 identities, decodes the marshaled proto.Message
// payload as raw JSON off the wire, and proves the confidentiality property of
// docs/design/content-confidentiality-envelope-541.md §3.3/§4.4:
//
//	(a) the wire carries NO plaintext "content" and NO "plaintext_content_hash"
//	    key anywhere in the payload tree;
//	(b) the "enc" envelope is well-formed with every §3.3 field;
//	(c) ciphertext_hash is sha256 OVER THE CIPHERTEXT (not plaintext);
//	(d) the operator can NIP-44-unwrap the CEK and ChaCha20-Poly1305-decrypt the
//	    inline ciphertext back to the ORIGINAL plaintext, byte-for-byte.
//
// It deliberately decodes the real marshaled bytes (not a Go struct field): a
// struct-field assertion would not prove the WIRE is leak-free.
func TestBuildPutMessage_V2Envelope_NoPlaintextLeak_And_RoundTrip(t *testing.T) {
	seller, err := identity.Generate()
	if err != nil {
		t.Fatalf("generate seller identity: %v", err)
	}
	operator, err := identity.Generate()
	if err != nil {
		t.Fatalf("generate operator identity: %v", err)
	}

	plaintext := []byte("the exact reusable artifact bytes a passive relay reader must NOT see")
	req := PutRequest{
		Description:    "a reusable v2-envelope build recipe",
		Teaser:         "public seller-authored abstract",
		Content:        plaintext,
		TokenCost:      4242,
		ContentType:    "exchange:content-type:code",
		Domains:        []string{"matching", "exchange"},
		OperatorPubKey: operator.PubKeyHex(),
	}

	msg, err := buildPutMessage(seller, req)
	if err != nil {
		t.Fatalf("buildPutMessage: %v", err)
	}

	// Decode the REAL wire bytes as raw JSON — this is what a passive REQ
	// Kinds:[3401] scraper sees.
	var payload map[string]any
	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		t.Fatalf("unmarshal wire payload: %v", err)
	}

	// --- (a) NO plaintext leak keys anywhere in the payload tree ------------
	// A single unsalted plaintext hash on the wire is the §4.4 guess-confirmation
	// oracle; a "content" field is the original cleartext leak. Neither may exist
	// at ANY nesting depth. "content_type"/"content_alg" (which merely CONTAIN
	// the substring "content") are legal — we match exact key names.
	keys := collectKeysRecursive(payload)
	for _, forbidden := range []string{"content", "plaintext_content_hash"} {
		if keys[forbidden] {
			t.Fatalf("wire payload leaks a %q key — passive readers must never see plaintext or a plaintext hash (§3.3/§4.4)", forbidden)
		}
	}
	// Belt-and-suspenders: the raw plaintext bytes must not appear anywhere in
	// the marshaled payload (base64 or otherwise).
	if strings.Contains(string(msg.Payload), base64.StdEncoding.EncodeToString(plaintext)) {
		t.Fatalf("wire payload contains base64(plaintext) — plaintext leaked onto the wire")
	}

	// --- (b) enc is well-formed with all §3.3 fields ------------------------
	if v, _ := payload["v"].(float64); v != 2 {
		t.Fatalf("payload.v = %v, want 2", payload["v"])
	}
	enc, ok := payload["enc"].(map[string]any)
	if !ok {
		t.Fatalf("payload.enc missing or not an object: %T", payload["enc"])
	}
	if alg, _ := enc["content_alg"].(string); alg != "chacha20poly1305" {
		t.Fatalf("enc.content_alg = %q, want chacha20poly1305", enc["content_alg"])
	}
	ctHash, _ := enc["ciphertext_hash"].(string)
	if !strings.HasPrefix(ctHash, "sha256:") {
		t.Fatalf("enc.ciphertext_hash = %q, want sha256:<hex> prefix", ctHash)
	}
	ctB64, _ := enc["ciphertext"].(string)
	if ctB64 == "" {
		t.Fatalf("enc.ciphertext missing (inline path)")
	}
	if _, hasBlob := enc["blob_pointer"]; hasBlob {
		t.Fatalf("enc.blob_pointer present on an inline (<=32 KiB) put — exactly one of ciphertext/blob_pointer must be set")
	}
	keyWrap, ok := enc["key_wrap"].(map[string]any)
	if !ok {
		t.Fatalf("enc.key_wrap missing or not an object: %T", enc["key_wrap"])
	}
	if alg, _ := keyWrap["alg"].(string); alg != "nip44-v2-secp256k1" {
		t.Fatalf("key_wrap.alg = %q, want nip44-v2-secp256k1", keyWrap["alg"])
	}
	if r, _ := keyWrap["recipient"].(string); r != operator.PubKeyHex() {
		t.Fatalf("key_wrap.recipient = %q, want operator pubkey %q", keyWrap["recipient"], operator.PubKeyHex())
	}
	wrapped, _ := keyWrap["wrapped"].(string)
	if wrapped == "" {
		t.Fatalf("key_wrap.wrapped is empty")
	}

	// --- (c) ciphertext_hash is over the CIPHERTEXT -------------------------
	ciphertext, err := base64.StdEncoding.DecodeString(ctB64)
	if err != nil {
		t.Fatalf("decode enc.ciphertext base64: %v", err)
	}
	sum := sha256.Sum256(ciphertext)
	wantHash := "sha256:" + hex.EncodeToString(sum[:])
	if ctHash != wantHash {
		t.Fatalf("ciphertext_hash %q is NOT sha256(ciphertext) %q — the hash must be over ciphertext, never plaintext", ctHash, wantHash)
	}
	// And it must NOT be the plaintext hash (the oracle we removed).
	plainSum := sha256.Sum256(plaintext)
	if ctHash == "sha256:"+hex.EncodeToString(plainSum[:]) {
		t.Fatalf("ciphertext_hash equals sha256(plaintext) — this is the §4.4 guess-confirmation oracle")
	}

	// --- (d) round-trip: operator unwraps CEK, AEAD-decrypts to original ----
	// The operator opens the NIP-44 wrap addressed to it from the seller.
	cek, err := nip44.Open(operator, seller.PubKeyHex(), wrapped)
	if err != nil {
		t.Fatalf("operator nip44.Open(wrapped CEK): %v", err)
	}
	if len(cek) != chacha20poly1305.KeySize {
		t.Fatalf("unwrapped CEK is %d bytes, want %d", len(cek), chacha20poly1305.KeySize)
	}
	aead, err := chacha20poly1305.New(cek)
	if err != nil {
		t.Fatalf("init AEAD from unwrapped CEK: %v", err)
	}
	ns := aead.NonceSize()
	if len(ciphertext) < ns {
		t.Fatalf("ciphertext %d bytes shorter than nonce %d", len(ciphertext), ns)
	}
	nonce, sealed := ciphertext[:ns], ciphertext[ns:]
	recovered, err := aead.Open(nil, nonce, sealed, nil)
	if err != nil {
		t.Fatalf("AEAD open with unwrapped CEK failed: %v", err)
	}
	if string(recovered) != string(plaintext) {
		t.Fatalf("round-trip mismatch:\n got %q\nwant %q", recovered, plaintext)
	}
}

// TestBuildPutMessage_RejectsMissingOperatorPubKey proves the fail-closed
// guard: a team-tier put with no operator recipient for the CEK is a hard
// error, never a plaintext fallback.
func TestBuildPutMessage_RejectsMissingOperatorPubKey(t *testing.T) {
	seller, err := identity.Generate()
	if err != nil {
		t.Fatalf("generate seller: %v", err)
	}
	_, err = buildPutMessage(seller, PutRequest{
		Description: "no operator key",
		Content:     []byte("bytes"),
		TokenCost:   100,
		ContentType: "exchange:content-type:text",
	})
	if err == nil {
		t.Fatalf("expected error for missing OperatorPubKey, got nil")
	}
}

// TestBuildPutMessage_OversizeContentFailsClosedWithoutBlobStore proves large
// content with NO seller BlobStore is a loud error (dontguess-640) — never an
// inline plaintext or plaintext-blob leak. This is the migrated form of the old
// "oversize is always deferred" test: oversize is now offloadable, but STILL
// fails closed when there is no store to hold the ciphertext.
func TestBuildPutMessage_OversizeContentFailsClosedWithoutBlobStore(t *testing.T) {
	seller, err := identity.Generate()
	if err != nil {
		t.Fatalf("generate seller: %v", err)
	}
	operator, err := identity.Generate()
	if err != nil {
		t.Fatalf("generate operator: %v", err)
	}
	big := make([]byte, maxInlineCiphertextPlaintext+1)
	_, err = buildPutMessage(seller, PutRequest{
		Description:    "oversize",
		Content:        big,
		TokenCost:      100,
		ContentType:    "exchange:content-type:text",
		OperatorPubKey: operator.PubKeyHex(),
		// No BlobStore → must fail closed.
	})
	if err == nil {
		t.Fatalf("expected fail-closed error for oversize content with no BlobStore, got nil")
	}
}

// TestBuildPutMessage_OversizeContentOffloadsCiphertext proves large content WITH
// a seller BlobStore is AEAD-encrypted and its CIPHERTEXT (never plaintext) is
// offloaded to Blossom: the wire carries enc.blob_pointer (not enc.ciphertext),
// the stored blob is exactly the ciphertext (sha256(blob)==ciphertext_hash), and
// the blob does NOT contain the plaintext. This is the seller half of the
// dontguess-640 round-trip (§3.2/§4.4 C1).
func TestBuildPutMessage_OversizeContentOffloadsCiphertext(t *testing.T) {
	seller, err := identity.Generate()
	if err != nil {
		t.Fatalf("generate seller: %v", err)
	}
	operator, err := identity.Generate()
	if err != nil {
		t.Fatalf("generate operator: %v", err)
	}
	store := exchange.NewMemoryBlobStore()

	// Distinctive, compressible-but-searchable plaintext > 32 KiB.
	plaintext := bytes.Repeat([]byte("OVERSIZE-SECRET-MARKER-0123456789 "), 2000) // ~68 KiB
	if len(plaintext) <= maxInlineCiphertextPlaintext {
		t.Fatalf("test setup: plaintext %d must exceed inline limit %d", len(plaintext), maxInlineCiphertextPlaintext)
	}

	msg, err := buildPutMessage(seller, PutRequest{
		Description:    "an oversize reusable artifact offloaded to Blossom",
		Content:        plaintext,
		TokenCost:      50000,
		ContentType:    "exchange:content-type:code",
		OperatorPubKey: operator.PubKeyHex(),
		BlobStore:      store,
	})
	if err != nil {
		t.Fatalf("buildPutMessage (oversize with store): %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		t.Fatalf("unmarshal wire payload: %v", err)
	}
	enc, ok := payload["enc"].(map[string]any)
	if !ok {
		t.Fatalf("payload.enc missing: %T", payload["enc"])
	}
	// Wire carries blob_pointer, NOT inline ciphertext (exactly one).
	pointer, _ := enc["blob_pointer"].(string)
	if pointer == "" {
		t.Fatalf("oversize put must carry enc.blob_pointer, got %v", enc["blob_pointer"])
	}
	if _, hasInline := enc["ciphertext"]; hasInline {
		t.Fatalf("oversize put must NOT carry inline enc.ciphertext — exactly one of ciphertext/blob_pointer")
	}
	ctHash, _ := enc["ciphertext_hash"].(string)
	if !strings.HasPrefix(ctHash, "sha256:") {
		t.Fatalf("enc.ciphertext_hash = %q, want sha256:<hex>", ctHash)
	}

	// The WIRE payload must not leak plaintext (no oversize bytes on the relay).
	if strings.Contains(string(msg.Payload), "OVERSIZE-SECRET-MARKER") {
		t.Fatalf("wire payload contains the plaintext marker — oversize plaintext leaked onto the relay")
	}

	// The stored blob holds CIPHERTEXT, never plaintext.
	blob, err := store.Fetch(pointer)
	if err != nil {
		t.Fatalf("fetch offloaded blob: %v", err)
	}
	if bytes.Contains(blob, []byte("OVERSIZE-SECRET-MARKER")) {
		t.Fatalf("BLOSSOM BLOB CONTAINS PLAINTEXT — the blob must hold AEAD ciphertext only (§4.4 C1)")
	}
	sum := sha256.Sum256(blob)
	if got := "sha256:" + hex.EncodeToString(sum[:]); got != ctHash {
		t.Fatalf("sha256(blob) %q != enc.ciphertext_hash %q — blob is not the committed ciphertext", got, ctHash)
	}

	// And the operator can still recover the ORIGINAL plaintext from the blob:
	// unwrap CEK, AEAD-open the blob ciphertext byte-for-byte.
	keyWrap, _ := enc["key_wrap"].(map[string]any)
	wrapped, _ := keyWrap["wrapped"].(string)
	cek, err := nip44.Open(operator, seller.PubKeyHex(), wrapped)
	if err != nil {
		t.Fatalf("operator unwrap CEK: %v", err)
	}
	aead, err := chacha20poly1305.New(cek)
	if err != nil {
		t.Fatalf("init AEAD: %v", err)
	}
	ns := aead.NonceSize()
	if len(blob) < ns {
		t.Fatalf("blob %d shorter than nonce %d", len(blob), ns)
	}
	recovered, err := aead.Open(nil, blob[:ns], blob[ns:], nil)
	if err != nil {
		t.Fatalf("AEAD open blob ciphertext: %v", err)
	}
	if !bytes.Equal(recovered, plaintext) {
		t.Fatalf("oversize round-trip mismatch: recovered %d bytes != original %d bytes", len(recovered), len(plaintext))
	}
}

// collectKeysRecursive returns the set of every JSON object key appearing at any
// depth in v (maps and slices are walked). Used to prove forbidden keys are
// absent from the entire wire payload tree.
func collectKeysRecursive(v any) map[string]bool {
	out := map[string]bool{}
	var walk func(any)
	walk = func(n any) {
		switch t := n.(type) {
		case map[string]any:
			for k, child := range t {
				out[k] = true
				walk(child)
			}
		case []any:
			for _, child := range t {
				walk(child)
			}
		}
	}
	walk(v)
	return out
}
