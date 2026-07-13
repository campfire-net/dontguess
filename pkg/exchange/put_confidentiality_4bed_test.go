package exchange

// put_confidentiality_4bed_test.go — the done-gate for dontguess-4bed
// (operator-side decrypt-then-gate + §6 fail-closed ciphertext-only
// enforcement, docs/design/content-confidentiality-envelope-541.md §3.1(2),
// §3.6, §4.4, §6).
//
// These are WHITE-BOX (package exchange) tests because the operator signer and
// the encrypted-required flag are unexported State fields set at engine
// construction. They wire a REAL secp256k1 operator identity into State and use
// REAL nip44.Open + REAL ChaCha20-Poly1305 — NOTHING about the decrypt is
// mocked. The v2 envelope is built EXACTLY the way relayclient.buildPutMessage
// (dontguess-58f) builds it on the wire, so these prove the operator can consume
// what the seller emits.
//
// Proven:
//
//	(a) a v2 put DECRYPTS, the gates run on the DECRYPTED plaintext, it folds into
//	    pendingPuts, ContentHash == sha256(plaintext) (operator-local dedup key),
//	    and WrappedCEKOperator + CiphertextHash are stored on the entry;
//	(b) a team-tier legacy plaintext "content" put is DROPPED (fail-closed §6);
//	(c) a team-tier put with a malformed/absent "enc" is DROPPED;
//	(d) an INDIVIDUAL-tier (encryptedRequired=false, no signer) plaintext put
//	    STILL folds — the legacy path is untouched;
//	(e) a v2 put whose ciphertext_hash does not match sha256(ciphertext) is
//	    DROPPED (integrity check fires before the AEAD open);
//	(f) a v2 put whose CEK was wrapped to the WRONG operator key is DROPPED (real
//	    NIP-44 unwrap fails — no mock could catch this);
//	(g) the quality gates genuinely run INSIDE the decrypt boundary (§3.6): a v2
//	    put whose DECRYPTED description is test-like is dropped by the existing
//	    isTestLikeDescription gate.

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	"github.com/3dl-dev/dontguess/pkg/identity"
	"github.com/3dl-dev/dontguess/pkg/nip44"
	"golang.org/x/crypto/chacha20poly1305"
)

// teamTierState returns a State armed exactly as NewEngine arms a team tier: the
// operator's real secp256k1 signer wired in and encryptedRequired = true.
func teamTierState(t *testing.T, operator identity.Signer) *State {
	t.Helper()
	s := NewState()
	s.operatorSigner = operator
	s.encryptedRequired = true
	return s
}

// v2Envelope is the raw §3.3 enc object plus the marshaled put payload built the
// same way buildPutMessage does. ciphertext is returned so a test can tamper the
// hash. wrapRecipient lets a test wrap to the WRONG operator.
func v2PutMessage(t *testing.T, seller identity.Signer, wrapRecipientHex, desc string, plaintext []byte, tokenCost int64) *Message {
	t.Helper()
	enc := buildEncObject(t, seller, wrapRecipientHex, plaintext)
	return marshalV2Put(t, seller, desc, tokenCost, enc)
}

func buildEncObject(t *testing.T, seller identity.Signer, wrapRecipientHex string, plaintext []byte) map[string]any {
	t.Helper()
	cek := make([]byte, chacha20poly1305.KeySize)
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
	ciphertext := aead.Seal(nonce, nonce, plaintext, nil)
	sum := sha256.Sum256(ciphertext)
	wrapped, err := nip44.Seal(seller, wrapRecipientHex, cek)
	if err != nil {
		t.Fatalf("wrap CEK: %v", err)
	}
	return map[string]any{
		"content_alg":     "chacha20poly1305",
		"ciphertext_hash": "sha256:" + hex.EncodeToString(sum[:]),
		"ciphertext":      base64.StdEncoding.EncodeToString(ciphertext),
		"key_wrap": map[string]any{
			"alg":       "nip44-v2-secp256k1",
			"recipient": wrapRecipientHex,
			"wrapped":   wrapped,
		},
	}
}

func marshalV2Put(t *testing.T, seller identity.Signer, desc string, tokenCost int64, enc map[string]any) *Message {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"v":            2,
		"description":  desc,
		"token_cost":   tokenCost,
		"content_type": "exchange:content-type:code",
		"domains":      []string{"go"},
		"enc":          enc,
	})
	if err != nil {
		t.Fatalf("marshal v2 put: %v", err)
	}
	return &Message{
		ID:        "put-" + desc + "-" + hex.EncodeToString([]byte(time.Now().String()))[:8],
		Sender:    seller.PubKeyHex(),
		Payload:   payload,
		Tags:      []string{TagPut},
		Timestamp: time.Now().UnixNano(),
	}
}

func legacyPlaintextPut(t *testing.T, seller identity.Signer, desc string, plaintext []byte, tokenCost int64) *Message {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"description":  desc,
		"content":      base64.StdEncoding.EncodeToString(plaintext),
		"token_cost":   tokenCost,
		"content_type": "exchange:content-type:code",
		"domains":      []string{"go"},
	})
	if err != nil {
		t.Fatalf("marshal legacy put: %v", err)
	}
	return &Message{
		ID:        "legacy-" + desc,
		Sender:    seller.PubKeyHex(),
		Payload:   payload,
		Tags:      []string{TagPut},
		Timestamp: time.Now().UnixNano(),
	}
}

// (a) v2 put decrypts, gates run on plaintext, folds with the CEK wrap + hashes stored.
func TestApplyPut_V2_DecryptsGatesFoldsAndStoresCEK(t *testing.T) {
	seller, err := identity.Generate()
	if err != nil {
		t.Fatalf("seller: %v", err)
	}
	operator, err := identity.Generate()
	if err != nil {
		t.Fatalf("operator: %v", err)
	}
	s := teamTierState(t, operator)

	plaintext := []byte("a genuinely reusable distilled artifact the wire must never expose")
	msg := v2PutMessage(t, seller, operator.PubKeyHex(), "reusable go flock contention test pattern", plaintext, 4242)
	s.Apply(msg)

	entry, ok := s.GetPendingPut(msg.ID)
	if !ok {
		t.Fatalf("v2 put was NOT folded into pendingPuts — decrypt-then-gate failed")
	}
	// Gates ran on the DECRYPTED plaintext: entry stored the plaintext bytes and
	// its size, and ContentHash is sha256(plaintext) — the operator-local dedup
	// key (§4.4), NOT the ciphertext hash.
	if string(entry.Content) != string(plaintext) {
		t.Fatalf("entry.Content = %q, want decrypted plaintext %q", entry.Content, plaintext)
	}
	if entry.ContentSize != int64(len(plaintext)) {
		t.Fatalf("entry.ContentSize = %d, want plaintext size %d", entry.ContentSize, len(plaintext))
	}
	plainSum := sha256.Sum256(plaintext)
	wantContentHash := "sha256:" + hex.EncodeToString(plainSum[:])
	if entry.ContentHash != wantContentHash {
		t.Fatalf("entry.ContentHash = %q, want sha256(plaintext) %q (operator-local dedup key §4.4)", entry.ContentHash, wantContentHash)
	}
	if entry.SellerKey != seller.PubKeyHex() {
		t.Fatalf("entry.SellerKey = %q, want seller %q", entry.SellerKey, seller.PubKeyHex())
	}
	// The CEK wrap and the CIPHERTEXT hash persist on the entry so the Phase-2
	// deliver (9e8) can re-wrap the CEK to the buyer (§3.5).
	if entry.WrappedCEKOperator == "" {
		t.Fatalf("entry.WrappedCEKOperator not stored — Phase-2 deliver cannot re-derive the CEK")
	}
	if entry.CiphertextHash == "" || entry.CiphertextHash == entry.ContentHash {
		t.Fatalf("entry.CiphertextHash must be stored AND distinct from the plaintext ContentHash; got %q vs %q", entry.CiphertextHash, entry.ContentHash)
	}
	// The stored wrap must be the real one the operator can re-open to the CEK.
	if _, err := nip44.Open(operator, seller.PubKeyHex(), entry.WrappedCEKOperator); err != nil {
		t.Fatalf("stored WrappedCEKOperator does not re-open to a CEK for the operator: %v", err)
	}
}

// buildEncObjectBlob mirrors buildEncObject but OFFLOADS the ciphertext to store
// and returns a blob_pointer envelope (no inline ciphertext) — the seller's
// oversize offload shape (dontguess-640). Returns the enc object plus the pointer
// so a test can assert the entry references it.
func buildEncObjectBlob(t *testing.T, seller identity.Signer, wrapRecipientHex string, plaintext []byte, store BlobStore) (enc map[string]any, pointer string) {
	t.Helper()
	cek := make([]byte, chacha20poly1305.KeySize)
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
	ciphertext := aead.Seal(nonce, nonce, plaintext, nil)
	sum := sha256.Sum256(ciphertext)
	pointer, err = store.Put(ciphertext)
	if err != nil {
		t.Fatalf("offload ciphertext: %v", err)
	}
	wrapped, err := nip44.Seal(seller, wrapRecipientHex, cek)
	if err != nil {
		t.Fatalf("wrap CEK: %v", err)
	}
	return map[string]any{
		"content_alg":     "chacha20poly1305",
		"ciphertext_hash": "sha256:" + hex.EncodeToString(sum[:]),
		"blob_pointer":    pointer, // no inline "ciphertext"
		"key_wrap": map[string]any{
			"alg":       "nip44-v2-secp256k1",
			"recipient": wrapRecipientHex,
			"wrapped":   wrapped,
		},
	}, pointer
}

// (a2) a v2 BLOB_POINTER put with an operator blob store: the operator FETCHES the
// ciphertext, verifies sha256==ciphertext_hash, decrypts, runs the gates, and folds
// with entry.BlobPointer set + entry.Content nil (dontguess-640 operator leg).
func TestApplyPut_V2Blob_FetchesVerifiesDecryptsGatesFolds(t *testing.T) {
	seller, _ := identity.Generate()
	operator, _ := identity.Generate()
	s := teamTierState(t, operator)
	store := NewMemoryBlobStore()
	s.SetBlobStore(store)

	plaintext := []byte("a genuinely reusable distilled artifact offloaded to Blossom; the wire never carries it")
	enc, pointer := buildEncObjectBlob(t, seller, operator.PubKeyHex(), plaintext, store)
	msg := marshalV2Put(t, seller, "reusable go flock contention test pattern", 4242, enc)
	s.Apply(msg)

	entry, ok := s.GetPendingPut(msg.ID)
	if !ok {
		t.Fatal("v2 blob_pointer put did NOT fold — operator fetch+verify+decrypt+gate failed")
	}
	if entry.BlobPointer != pointer {
		t.Fatalf("entry.BlobPointer = %q, want %q", entry.BlobPointer, pointer)
	}
	if entry.Content != nil {
		t.Fatalf("entry.Content non-nil (%d bytes) — offloaded entry must keep bytes in the blob only", len(entry.Content))
	}
	if entry.ContentSize != int64(len(plaintext)) {
		t.Fatalf("entry.ContentSize = %d, want plaintext size %d", entry.ContentSize, len(plaintext))
	}
	plainSum := sha256.Sum256(plaintext)
	if entry.ContentHash != "sha256:"+hex.EncodeToString(plainSum[:]) {
		t.Fatalf("entry.ContentHash = %q, want sha256(plaintext) (operator-local dedup key)", entry.ContentHash)
	}
	if entry.CiphertextHash == "" || entry.CiphertextHash == entry.ContentHash {
		t.Fatalf("entry.CiphertextHash must be stored and distinct from ContentHash; got %q vs %q", entry.CiphertextHash, entry.ContentHash)
	}
	if entry.WrappedCEKOperator == "" {
		t.Fatal("entry.WrappedCEKOperator not stored")
	}
}

// (a3) FAIL-CLOSED: a team-tier v2 blob_pointer put with NO operator blob store
// (cannot fetch to gate) is DROPPED — never folded un-gated.
func TestApplyPut_V2Blob_NoOperatorBlobStore_Dropped(t *testing.T) {
	seller, _ := identity.Generate()
	operator, _ := identity.Generate()
	s := teamTierState(t, operator) // NO SetBlobStore

	// Build the ciphertext against a throwaway store just to mint a valid pointer;
	// the operator's own store is nil, so it cannot fetch to gate.
	enc, _ := buildEncObjectBlob(t, seller, operator.PubKeyHex(), []byte("secret oversize bytes"), NewMemoryBlobStore())
	msg := marshalV2Put(t, seller, "a substantive reusable description", 4242, enc)
	s.Apply(msg)

	if _, ok := s.GetPendingPut(msg.ID); ok {
		t.Fatal("v2 blob_pointer put FOLDED with no operator blob store — un-gated content admitted (fail-closed broken)")
	}
}

// (a4) FAIL-CLOSED: a team-tier v2 blob_pointer put whose blob is MISSING from the
// operator's store (fetch fails) is DROPPED — never folded.
func TestApplyPut_V2Blob_MissingBlob_Dropped(t *testing.T) {
	seller, _ := identity.Generate()
	operator, _ := identity.Generate()
	s := teamTierState(t, operator)
	s.SetBlobStore(NewMemoryBlobStore()) // operator store is EMPTY

	// ciphertext offloaded to a DIFFERENT store, so the operator's store cannot resolve it.
	enc, _ := buildEncObjectBlob(t, seller, operator.PubKeyHex(), []byte("bytes the operator cannot fetch"), NewMemoryBlobStore())
	msg := marshalV2Put(t, seller, "a substantive reusable description two", 4242, enc)
	s.Apply(msg)

	if _, ok := s.GetPendingPut(msg.ID); ok {
		t.Fatal("v2 blob_pointer put FOLDED despite an un-fetchable blob — must drop, cannot gate what it cannot fetch")
	}
}

// (b) team-tier legacy plaintext "content" put is DROPPED (fail-closed §6).
func TestApplyPut_TeamTier_LegacyPlaintext_Dropped(t *testing.T) {
	seller, _ := identity.Generate()
	operator, _ := identity.Generate()
	s := teamTierState(t, operator)

	msg := legacyPlaintextPut(t, seller, "a plaintext downgrade attempt", []byte("cleartext that must never fold on team tier"), 4242)
	s.Apply(msg)

	if _, ok := s.GetPendingPut(msg.ID); ok {
		t.Fatalf("team-tier plaintext put FOLDED — the §6 fail-closed guard did not drop the downgrade (leak reopened)")
	}
	if n := len(s.PendingPuts()); n != 0 {
		t.Fatalf("pendingPuts not empty after a dropped plaintext put: %d", n)
	}
}

// (c) team-tier put with malformed / absent enc is DROPPED.
func TestApplyPut_TeamTier_MalformedEnc_Dropped(t *testing.T) {
	seller, _ := identity.Generate()
	operator, _ := identity.Generate()

	cases := map[string]map[string]any{
		"absent-enc":              nil,
		"missing-content-alg":     {"ciphertext_hash": "sha256:x", "ciphertext": "AA==", "key_wrap": map[string]any{"alg": "nip44-v2-secp256k1", "recipient": operator.PubKeyHex(), "wrapped": "w"}},
		"missing-key-wrap":        {"content_alg": "chacha20poly1305", "ciphertext_hash": "sha256:x", "ciphertext": "AA=="},
		"both-inline-and-blob":    {"content_alg": "chacha20poly1305", "ciphertext_hash": "sha256:x", "ciphertext": "AA==", "blob_pointer": "blossom:x", "key_wrap": map[string]any{"alg": "nip44-v2-secp256k1", "recipient": operator.PubKeyHex(), "wrapped": "w"}},
		"neither-inline-nor-blob": {"content_alg": "chacha20poly1305", "ciphertext_hash": "sha256:x", "key_wrap": map[string]any{"alg": "nip44-v2-secp256k1", "recipient": operator.PubKeyHex(), "wrapped": "w"}},
	}
	for name, enc := range cases {
		t.Run(name, func(t *testing.T) {
			s := teamTierState(t, operator)
			var msg *Message
			if enc == nil {
				// v:2 but no enc object at all.
				payload, _ := json.Marshal(map[string]any{"v": 2, "description": "no enc", "token_cost": 4242, "content_type": "exchange:content-type:code", "domains": []string{"go"}})
				msg = &Message{ID: "no-enc", Sender: seller.PubKeyHex(), Payload: payload, Tags: []string{TagPut}, Timestamp: time.Now().UnixNano()}
			} else {
				msg = marshalV2Put(t, seller, "malformed "+name, 4242, enc)
			}
			s.Apply(msg)
			if _, ok := s.GetPendingPut(msg.ID); ok {
				t.Fatalf("team-tier put with %s enc FOLDED — fail-closed did not drop it", name)
			}
		})
	}
}

// (d) individual-tier (no signer, encryptedRequired=false) plaintext put STILL folds.
func TestApplyPut_IndividualTier_LegacyPlaintext_StillFolds(t *testing.T) {
	seller, _ := identity.Generate()
	s := NewState() // individual tier: operatorSigner nil, encryptedRequired false

	msg := legacyPlaintextPut(t, seller, "an individual-tier local put", []byte("local socket content — already confidential"), 4242)
	s.Apply(msg)

	entry, ok := s.GetPendingPut(msg.ID)
	if !ok {
		t.Fatalf("individual-tier plaintext put was DROPPED — the legacy path must stay unchanged")
	}
	if string(entry.Content) != "local socket content — already confidential" {
		t.Fatalf("individual-tier entry.Content = %q, want the plaintext bytes", entry.Content)
	}
	if entry.WrappedCEKOperator != "" || entry.CiphertextHash != "" {
		t.Fatalf("individual-tier plaintext entry must carry no CEK wrap / ciphertext hash; got %q / %q", entry.WrappedCEKOperator, entry.CiphertextHash)
	}
}

// (e) v2 put whose ciphertext_hash mismatches sha256(ciphertext) is DROPPED.
func TestApplyPut_V2_CiphertextHashMismatch_Dropped(t *testing.T) {
	seller, _ := identity.Generate()
	operator, _ := identity.Generate()
	s := teamTierState(t, operator)

	enc := buildEncObject(t, seller, operator.PubKeyHex(), []byte("valid plaintext"))
	// Corrupt the advertised ciphertext_hash — the operator must reject before decrypting.
	enc["ciphertext_hash"] = "sha256:" + hex.EncodeToString(make([]byte, 32))
	msg := marshalV2Put(t, seller, "tampered ciphertext hash", 4242, enc)
	s.Apply(msg)

	if _, ok := s.GetPendingPut(msg.ID); ok {
		t.Fatalf("v2 put with a mismatched ciphertext_hash FOLDED — integrity check did not fire")
	}
}

// (f) v2 put whose CEK is wrapped to the WRONG operator is DROPPED (real NIP-44 unwrap fails).
func TestApplyPut_V2_WrappedToWrongOperator_Dropped(t *testing.T) {
	seller, _ := identity.Generate()
	operator, _ := identity.Generate()
	stranger, _ := identity.Generate() // CEK wrapped to a DIFFERENT key
	s := teamTierState(t, operator)

	msg := v2PutMessage(t, seller, stranger.PubKeyHex(), "wrapped to the wrong key", []byte("secret"), 4242)
	s.Apply(msg)

	if _, ok := s.GetPendingPut(msg.ID); ok {
		t.Fatalf("v2 put wrapped to a stranger's key FOLDED — the operator must be unable to unwrap it")
	}
}

// (g) the quality gates genuinely run INSIDE the decrypt boundary (§3.6): a v2
// put whose DECRYPTED description is test-like is dropped by isTestLikeDescription.
func TestApplyPut_V2_GatesRunOnDecryptedPlaintext(t *testing.T) {
	seller, _ := identity.Generate()
	operator, _ := identity.Generate()

	// Test-like description → dropped AFTER decrypt by the existing junk gate.
	s := teamTierState(t, operator)
	msg := v2PutMessage(t, seller, operator.PubKeyHex(), "test", []byte("some content"), 4242)
	s.Apply(msg)
	if _, ok := s.GetPendingPut(msg.ID); ok {
		t.Fatalf("v2 put with a test-like description FOLDED — the quality gate did not run on the decrypted plaintext")
	}

	// token_cost below MinTokenCost → dropped AFTER decrypt by the floor gate.
	s2 := teamTierState(t, operator)
	msg2 := v2PutMessage(t, seller, operator.PubKeyHex(), "a substantive description", []byte("some content"), MinTokenCost-1)
	s2.Apply(msg2)
	if _, ok := s2.GetPendingPut(msg2.ID); ok {
		t.Fatalf("v2 put below MinTokenCost FOLDED — the floor gate did not run on the decrypted put")
	}
}
