package nip44

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/3dl-dev/dontguess/pkg/identity"
)

// vectors mirrors the official paulmillr/nip44 nip44.vectors.json layout (the
// same external ground truth the identity/ecdh_test.go conversation-key vectors
// came from). Only the v2 classes this package owns are decoded.
type vectors struct {
	V2 struct {
		Valid struct {
			GetConversationKey []struct {
				Sec1            string `json:"sec1"`
				Pub2            string `json:"pub2"`
				ConversationKey string `json:"conversation_key"`
			} `json:"get_conversation_key"`
			GetMessageKeys struct {
				ConversationKey string `json:"conversation_key"`
				Keys            []struct {
					Nonce       string `json:"nonce"`
					ChachaKey   string `json:"chacha_key"`
					ChachaNonce string `json:"chacha_nonce"`
					HmacKey     string `json:"hmac_key"`
				} `json:"keys"`
			} `json:"get_message_keys"`
			CalcPaddedLen  [][2]int `json:"calc_padded_len"`
			EncryptDecrypt []struct {
				Sec1            string `json:"sec1"`
				Sec2            string `json:"sec2"`
				ConversationKey string `json:"conversation_key"`
				Nonce           string `json:"nonce"`
				Plaintext       string `json:"plaintext"`
				Payload         string `json:"payload"`
			} `json:"encrypt_decrypt"`
			EncryptDecryptLongMsg []struct {
				ConversationKey string `json:"conversation_key"`
				Nonce           string `json:"nonce"`
				Pattern         string `json:"pattern"`
				Repeat          int    `json:"repeat"`
				PlaintextSHA256 string `json:"plaintext_sha256"`
				PayloadSHA256   string `json:"payload_sha256"`
			} `json:"encrypt_decrypt_long_msg"`
		} `json:"valid"`
		Invalid struct {
			EncryptMsgLengths  []int `json:"encrypt_msg_lengths"`
			GetConversationKey []struct {
				Sec1 string `json:"sec1"`
				Pub2 string `json:"pub2"`
				Note string `json:"note"`
			} `json:"get_conversation_key"`
			Decrypt []struct {
				ConversationKey string `json:"conversation_key"`
				Nonce           string `json:"nonce"`
				Plaintext       string `json:"plaintext"`
				Payload         string `json:"payload"`
				Note            string `json:"note"`
			} `json:"decrypt"`
		} `json:"invalid"`
	} `json:"v2"`
}

func loadVectors(t *testing.T) *vectors {
	t.Helper()
	raw, err := os.ReadFile("testdata/nip44.vectors.json")
	if err != nil {
		t.Fatalf("read vectors: %v", err)
	}
	var v vectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("decode vectors: %v", err)
	}
	return &v
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("decode hex %q: %v", s, err)
	}
	return b
}

func mustKey32(t *testing.T, s string) [32]byte {
	t.Helper()
	b := mustHex(t, s)
	if len(b) != 32 {
		t.Fatalf("expected 32-byte key, got %d from %q", len(b), s)
	}
	var out [32]byte
	copy(out[:], b)
	return out
}

// TestKAT_ValidGetConversationKey pins conversation-key derivation THROUGH the
// Signer.ECDH port: FromPrivHex(sec1).ECDH(pub2) → HKDF-Extract must equal the
// published conversation_key. This is the raw-shared-X + even-Y-lift ground truth
// (a symmetry test cannot catch a wrong KDF or a sha256(X) mistake).
func TestKAT_ValidGetConversationKey(t *testing.T) {
	v := loadVectors(t)
	if len(v.V2.Valid.GetConversationKey) == 0 {
		t.Fatal("no get_conversation_key vectors loaded")
	}
	for i, tc := range v.V2.Valid.GetConversationKey {
		id, err := identity.FromPrivHex(tc.Sec1)
		if err != nil {
			t.Fatalf("case %d: FromPrivHex(%s): %v", i, tc.Sec1, err)
		}
		got, err := conversationKey(id, tc.Pub2)
		if err != nil {
			t.Fatalf("case %d: conversationKey: %v", i, err)
		}
		want := mustKey32(t, tc.ConversationKey)
		if got != want {
			t.Fatalf("case %d: conversation key mismatch\n got  %x\n want %x", i, got, want)
		}
	}
}

// TestKAT_ValidGetMessageKeys pins HKDF-Expand: conversation_key + nonce must
// expand to the exact (chacha_key, chacha_nonce, hmac_key) triple.
func TestKAT_ValidGetMessageKeys(t *testing.T) {
	v := loadVectors(t)
	convKey := mustKey32(t, v.V2.Valid.GetMessageKeys.ConversationKey)
	if len(v.V2.Valid.GetMessageKeys.Keys) == 0 {
		t.Fatal("no get_message_keys vectors loaded")
	}
	for i, k := range v.V2.Valid.GetMessageKeys.Keys {
		nonce := mustHex(t, k.Nonce)
		ck, cn, hk, err := messageKeys(convKey, nonce)
		if err != nil {
			t.Fatalf("case %d: messageKeys: %v", i, err)
		}
		if !bytes.Equal(ck[:], mustHex(t, k.ChachaKey)) {
			t.Fatalf("case %d: chacha_key mismatch\n got %x\n want %s", i, ck, k.ChachaKey)
		}
		if !bytes.Equal(cn[:], mustHex(t, k.ChachaNonce)) {
			t.Fatalf("case %d: chacha_nonce mismatch\n got %x\n want %s", i, cn, k.ChachaNonce)
		}
		if !bytes.Equal(hk[:], mustHex(t, k.HmacKey)) {
			t.Fatalf("case %d: hmac_key mismatch\n got %x\n want %s", i, hk, k.HmacKey)
		}
	}
}

// TestKAT_ValidCalcPaddedLen pins the power-of-two padding size function.
func TestKAT_ValidCalcPaddedLen(t *testing.T) {
	v := loadVectors(t)
	if len(v.V2.Valid.CalcPaddedLen) == 0 {
		t.Fatal("no calc_padded_len vectors loaded")
	}
	for i, pair := range v.V2.Valid.CalcPaddedLen {
		unpadded, want := pair[0], pair[1]
		if got := calcPaddedLen(unpadded); got != want {
			t.Fatalf("case %d: calcPaddedLen(%d) = %d, want %d", i, unpadded, got, want)
		}
	}
}

// TestKAT_ValidEncryptDecrypt is the core byte-exact conformance test. For each
// vector it: (1) re-derives conversation_key via the Signer port from sec1/sec2
// and checks it equals the published value, (2) encrypts plaintext with the
// vector's FIXED nonce via the internal seam and asserts byte-identical payload,
// (3) decrypts the vector payload back to the exact plaintext.
func TestKAT_ValidEncryptDecrypt(t *testing.T) {
	v := loadVectors(t)
	if len(v.V2.Valid.EncryptDecrypt) == 0 {
		t.Fatal("no encrypt_decrypt vectors loaded")
	}
	for i, tc := range v.V2.Valid.EncryptDecrypt {
		convKey := mustKey32(t, tc.ConversationKey)

		// (1) conversation key through the Signer port (sec1 is us, sec2's pubkey
		// is the counterparty x-only key).
		id1, err := identity.FromPrivHex(tc.Sec1)
		if err != nil {
			t.Fatalf("case %d: FromPrivHex(sec1): %v", i, err)
		}
		id2, err := identity.FromPrivHex(tc.Sec2)
		if err != nil {
			t.Fatalf("case %d: FromPrivHex(sec2): %v", i, err)
		}
		derived, err := conversationKey(id1, id2.PubKeyHex())
		if err != nil {
			t.Fatalf("case %d: conversationKey via port: %v", i, err)
		}
		if derived != convKey {
			t.Fatalf("case %d: port-derived conversation key != vector\n got  %x\n want %x", i, derived, convKey)
		}

		// (2) byte-exact encrypt with the vector's fixed nonce.
		nonce := mustHex(t, tc.Nonce)
		gotPayload, err := encryptWithNonce(convKey, nonce, []byte(tc.Plaintext))
		if err != nil {
			t.Fatalf("case %d: encryptWithNonce: %v", i, err)
		}
		if gotPayload != tc.Payload {
			t.Fatalf("case %d: payload mismatch\n got  %s\n want %s", i, gotPayload, tc.Payload)
		}

		// (3) decrypt the vector payload back to exact plaintext.
		gotPlain, err := decryptWithConversationKey(convKey, tc.Payload)
		if err != nil {
			t.Fatalf("case %d: decrypt: %v", i, err)
		}
		if !bytes.Equal(gotPlain, []byte(tc.Plaintext)) {
			t.Fatalf("case %d: decrypted plaintext mismatch\n got  %q\n want %q", i, gotPlain, tc.Plaintext)
		}
	}
}

// TestKAT_ValidEncryptDecryptLongMsg pins encryption of large plaintexts (up to
// the 65535 max) by comparing sha256 of the produced payload, and round-trips
// the plaintext by sha256.
func TestKAT_ValidEncryptDecryptLongMsg(t *testing.T) {
	v := loadVectors(t)
	if len(v.V2.Valid.EncryptDecryptLongMsg) == 0 {
		t.Fatal("no encrypt_decrypt_long_msg vectors loaded")
	}
	for i, tc := range v.V2.Valid.EncryptDecryptLongMsg {
		convKey := mustKey32(t, tc.ConversationKey)
		nonce := mustHex(t, tc.Nonce)
		plaintext := []byte(strings.Repeat(tc.Pattern, tc.Repeat))

		ptSum := sha256.Sum256(plaintext)
		if hex.EncodeToString(ptSum[:]) != tc.PlaintextSHA256 {
			t.Fatalf("case %d: constructed plaintext sha256 mismatch (bad pattern/repeat)", i)
		}

		payload, err := encryptWithNonce(convKey, nonce, plaintext)
		if err != nil {
			t.Fatalf("case %d: encryptWithNonce: %v", i, err)
		}
		plSum := sha256.Sum256([]byte(payload))
		if hex.EncodeToString(plSum[:]) != tc.PayloadSHA256 {
			t.Fatalf("case %d: payload sha256 mismatch", i)
		}

		back, err := decryptWithConversationKey(convKey, payload)
		if err != nil {
			t.Fatalf("case %d: decrypt: %v", i, err)
		}
		if !bytes.Equal(back, plaintext) {
			t.Fatalf("case %d: long-msg round-trip mismatch", i)
		}
	}
}

// TestKAT_InvalidEncryptMsgLengths asserts the padding gate rejects the invalid
// plaintext lengths (0 and > 65535).
func TestKAT_InvalidEncryptMsgLengths(t *testing.T) {
	v := loadVectors(t)
	if len(v.V2.Invalid.EncryptMsgLengths) == 0 {
		t.Fatal("no encrypt_msg_lengths vectors loaded")
	}
	for _, n := range v.V2.Invalid.EncryptMsgLengths {
		if _, err := pad(make([]byte, n)); err == nil {
			t.Fatalf("expected pad(len=%d) to error, got nil", n)
		}
	}
}

// TestKAT_InvalidGetConversationKey asserts every should-reject key-agreement
// vector fails — either the private key is rejected (zero / >= curve order) or
// the counterparty point is off-curve / small-order-on-twist and the lift+ECDH
// rejects it. Neither must panic; the overall attempt must return an error.
func TestKAT_InvalidGetConversationKey(t *testing.T) {
	v := loadVectors(t)
	if len(v.V2.Invalid.GetConversationKey) == 0 {
		t.Fatal("no invalid get_conversation_key vectors loaded")
	}
	for i, tc := range v.V2.Invalid.GetConversationKey {
		id, err := identity.FromPrivHex(tc.Sec1)
		if err != nil {
			continue // sec1 rejected — correct
		}
		if _, err := conversationKey(id, tc.Pub2); err == nil {
			t.Fatalf("case %d (%s): expected error, got success", i, tc.Note)
		}
	}
}

// TestKAT_InvalidDecrypt asserts every should-reject payload class (unknown
// version, bad base64, bad MAC, invalid padding, out-of-range length) returns an
// error, never a panic, never a wrong plaintext.
func TestKAT_InvalidDecrypt(t *testing.T) {
	v := loadVectors(t)
	if len(v.V2.Invalid.Decrypt) == 0 {
		t.Fatal("no invalid decrypt vectors loaded")
	}
	for i, tc := range v.V2.Invalid.Decrypt {
		convKey := mustKey32(t, tc.ConversationKey)
		got, err := decryptWithConversationKey(convKey, tc.Payload)
		if err == nil {
			t.Fatalf("case %d (%s): expected error, got plaintext %q", i, tc.Note, got)
		}
	}
}

// TestSealOpenRoundTrip exercises the full public API through the Signer port
// with random keys and a fresh crypto/rand nonce, including the 32-byte CEK case
// (the real payload this envelope wraps) and boundary sizes. This complements —
// does NOT replace — the fixed-nonce KATs above: a round-trip alone cannot catch
// a wrong padding/KDF (it would round-trip its own bug).
func TestSealOpenRoundTrip(t *testing.T) {
	alice, err := identity.Generate()
	if err != nil {
		t.Fatalf("generate alice: %v", err)
	}
	bob, err := identity.Generate()
	if err != nil {
		t.Fatalf("generate bob: %v", err)
	}

	sizes := []int{1, 32, 33, 100, 500, 4096, 65535}
	for _, n := range sizes {
		plaintext := make([]byte, n)
		for j := range plaintext {
			plaintext[j] = byte(j*31 + 7)
		}

		// Alice seals to Bob; Bob opens from Alice.
		payload, err := Seal(alice, bob.PubKeyHex(), plaintext)
		if err != nil {
			t.Fatalf("size %d: Seal: %v", n, err)
		}
		back, err := Open(bob, alice.PubKeyHex(), payload)
		if err != nil {
			t.Fatalf("size %d: Open: %v", n, err)
		}
		if !bytes.Equal(back, plaintext) {
			t.Fatalf("size %d: round-trip mismatch", n)
		}

		// A third party (wrong key) must NOT be able to open it.
		mallory, err := identity.Generate()
		if err != nil {
			t.Fatalf("generate mallory: %v", err)
		}
		if _, err := Open(mallory, alice.PubKeyHex(), payload); err == nil {
			t.Fatalf("size %d: mallory opened a payload not addressed to her", n)
		}
	}
}

// TestSealNonceIsRandom guards the public Seal nonce source: two seals of the
// same plaintext to the same recipient must differ (fresh crypto/rand nonce), so
// nobody accidentally wires a fixed nonce into production.
func TestSealNonceIsRandom(t *testing.T) {
	alice, _ := identity.Generate()
	bob, _ := identity.Generate()
	p := []byte("the 32-byte CEK stand-in payload")
	a, err := Seal(alice, bob.PubKeyHex(), p)
	if err != nil {
		t.Fatalf("Seal a: %v", err)
	}
	b, err := Seal(alice, bob.PubKeyHex(), p)
	if err != nil {
		t.Fatalf("Seal b: %v", err)
	}
	if a == b {
		t.Fatal("two Seals of the same plaintext produced identical payloads — nonce not random")
	}
}
