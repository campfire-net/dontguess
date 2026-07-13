// Package nip44 implements the NIP-44 v2 authenticated-encryption envelope
// (secp256k1 ECDH → HKDF-SHA256 → ChaCha20 + HMAC-SHA256, encrypt-then-MAC)
// behind the identity.Signer port.
//
// It is the wrap primitive for dontguess content-confidentiality
// (docs/design/content-confidentiality-envelope-541.md §3.2, §4.5): the 32-byte
// content-encryption key (CEK) is NIP-44-wrapped to the operator at put and
// re-wrapped to the buyer at deliver. This package is deliberately a thin,
// testable surface — it does NOT touch exchange state, events, or the CEK/AEAD
// bulk-content path (that is later items). It exposes exactly two public
// operations, Seal and Open, both of which derive the NIP-44 conversation key
// via Signer.ECDH (the raw shared X coordinate; see pkg/identity).
//
// CRITICAL: this is security-critical crypto validated byte-for-byte against the
// official NIP-44 v2 known-answer vectors (paulmillr/nip44) in nip44_test.go.
// Do not "simplify" the padding, KDF, or MAC construction without re-running the
// full KAT — the vectors exist to catch exactly those silent mistakes.
package nip44

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/bits"

	"github.com/3dl-dev/dontguess/pkg/identity"
	"golang.org/x/crypto/chacha20"
	"golang.org/x/crypto/hkdf"
)

const (
	// version is the NIP-44 v2 payload version byte. Any other value is rejected
	// on Open (the "unknown version" reject class of the official vectors).
	version = 0x02

	// minPlaintextSize / maxPlaintextSize are the NIP-44 v2 padding bounds. A
	// 0-length or >65535-byte plaintext is rejected before encryption. The
	// 32-byte CEK (this envelope's real payload) sits comfortably inside them.
	minPlaintextSize = 1
	maxPlaintextSize = 65535

	nonceSize = 32 // per-message nonce fed to HKDF-Expand as `info`
	macSize   = 32 // HMAC-SHA256 tag
)

// convSalt is the fixed HKDF-Extract salt for NIP-44 v2. Deriving the
// conversation key with any other salt diverges from every other NIP-44 client.
var convSalt = []byte("nip44-v2")

// Seal encrypts plaintext into a NIP-44 v2 payload string addressed to the
// counterparty, using a fresh crypto/rand nonce. The conversation key is derived
// via signer.ECDH(counterpartyXOnlyHex) — the RAW shared X coordinate, not
// sha256(X). Output is base64(0x02 || nonce(32) || ciphertext || mac(32)).
//
// The public API always draws the nonce from crypto/rand; the known-answer tests
// exercise the byte-exact spec conformance via the internal encryptWithNonce
// seam with the vector's fixed nonce (see nip44_test.go).
func Seal(signer identity.Signer, counterpartyXOnlyHex string, plaintext []byte) (string, error) {
	convKey, err := conversationKey(signer, counterpartyXOnlyHex)
	if err != nil {
		return "", err
	}
	var nonce [nonceSize]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", fmt.Errorf("nip44: read csprng nonce: %w", err)
	}
	return encryptWithNonce(convKey, nonce[:], plaintext)
}

// Open decrypts a NIP-44 v2 payload addressed to us from the counterparty. It
// derives the same conversation key via signer.ECDH, verifies the HMAC in
// constant time BEFORE decrypting, and rejects (returns a non-nil error, never a
// panic) any malformed payload: unknown version, bad base64, bad MAC, or invalid
// padding. A rejection here is the whole point of the encrypt-then-MAC design.
func Open(signer identity.Signer, counterpartyXOnlyHex string, payload string) ([]byte, error) {
	convKey, err := conversationKey(signer, counterpartyXOnlyHex)
	if err != nil {
		return nil, err
	}
	return decryptWithConversationKey(convKey, payload)
}

// conversationKey derives the NIP-44 v2 conversation key for a counterparty via
// the Signer port. signer.ECDH returns the raw 32-byte shared X coordinate;
// conversationKeyFromShared runs HKDF-Extract over it.
func conversationKey(signer identity.Signer, counterpartyXOnlyHex string) ([32]byte, error) {
	sharedX, err := signer.ECDH(counterpartyXOnlyHex)
	if err != nil {
		return [32]byte{}, fmt.Errorf("nip44: ecdh: %w", err)
	}
	return conversationKeyFromShared(sharedX), nil
}

// conversationKeyFromShared computes conversation_key = HKDF-Extract(
// salt="nip44-v2", IKM=shared_x). For SHA-256 this is exactly
// HMAC-SHA256("nip44-v2", shared_x) and is 32 bytes.
func conversationKeyFromShared(sharedX [32]byte) [32]byte {
	prk := hkdf.Extract(sha256.New, sharedX[:], convSalt)
	var out [32]byte
	copy(out[:], prk)
	return out
}

// messageKeys expands the conversation key into the per-message
// (chacha_key, chacha_nonce, hmac_key) triple via HKDF-Expand with info=nonce and
// L=76: bytes [0:32) chacha_key, [32:44) chacha_nonce (12 bytes), [44:76) hmac_key.
func messageKeys(conversationKey [32]byte, nonce []byte) (chachaKey [32]byte, chachaNonce [12]byte, hmacKey [32]byte, err error) {
	if len(nonce) != nonceSize {
		err = fmt.Errorf("nip44: nonce must be %d bytes, got %d", nonceSize, len(nonce))
		return
	}
	r := hkdf.Expand(sha256.New, conversationKey[:], nonce)
	var okm [76]byte
	if _, e := io.ReadFull(r, okm[:]); e != nil {
		err = fmt.Errorf("nip44: hkdf-expand: %w", e)
		return
	}
	copy(chachaKey[:], okm[0:32])
	copy(chachaNonce[:], okm[32:44])
	copy(hmacKey[:], okm[44:76])
	return
}

// calcPaddedLen returns the NIP-44 v2 power-of-two padded length for an
// unpadded plaintext of `unpadded` bytes (this is the content length, EXCLUDING
// the 2-byte length prefix). Ported verbatim from the spec; validated against
// the official calc_padded_len vectors.
func calcPaddedLen(unpadded int) int {
	if unpadded <= 32 {
		return 32
	}
	// nextPower = 1 << (floor(log2(unpadded-1)) + 1). bits.Len(x) == floor(log2(x))+1
	// for x > 0, so this is exactly the spec's next_power.
	nextPower := 1 << bits.Len(uint(unpadded-1))
	chunk := 32
	if nextPower > 256 {
		chunk = nextPower / 8
	}
	return chunk * ((unpadded-1)/chunk + 1)
}

// pad applies the NIP-44 v2 padding scheme: u16-big-endian length prefix, the
// plaintext, then zero padding out to calcPaddedLen. Rejects empty or
// oversized plaintext (the invalid encrypt-length vector class).
func pad(plaintext []byte) ([]byte, error) {
	n := len(plaintext)
	if n < minPlaintextSize || n > maxPlaintextSize {
		return nil, fmt.Errorf("nip44: invalid plaintext length %d (must be %d..%d)", n, minPlaintextSize, maxPlaintextSize)
	}
	padded := make([]byte, 2+calcPaddedLen(n))
	binary.BigEndian.PutUint16(padded[0:2], uint16(n))
	copy(padded[2:], plaintext)
	return padded, nil
}

// unpad reverses pad and validates the framing exactly per spec: the declared
// length is in-bounds and the total padded size matches calcPaddedLen. A wrong
// total length or out-of-range declared length is the "invalid padding" reject
// class.
func unpad(padded []byte) ([]byte, error) {
	if len(padded) < 2 {
		return nil, errors.New("nip44: invalid padding (too short for length prefix)")
	}
	unpaddedLen := int(binary.BigEndian.Uint16(padded[0:2]))
	if unpaddedLen < minPlaintextSize || unpaddedLen > maxPlaintextSize {
		return nil, errors.New("nip44: invalid padding (declared length out of range)")
	}
	if len(padded) != 2+calcPaddedLen(unpaddedLen) {
		return nil, errors.New("nip44: invalid padding (total length mismatch)")
	}
	return padded[2 : 2+unpaddedLen], nil
}

// hmacAAD computes HMAC-SHA256(hmac_key, aad || message). NIP-44 v2 MACs over
// nonce || ciphertext, so callers pass aad=nonce, message=ciphertext. aad must
// be the 32-byte nonce (enforced by the caller's nonce-length checks).
func hmacAAD(key [32]byte, message, aad []byte) []byte {
	h := hmac.New(sha256.New, key[:])
	h.Write(aad)
	h.Write(message)
	return h.Sum(nil)
}

// encryptWithNonce is the byte-exact NIP-44 v2 encrypt core. It is the test-only
// seam the known-answer vectors drive with a FIXED nonce (public Seal supplies a
// crypto/rand nonce). Steps: derive message keys, pad, ChaCha20 (unauthenticated
// stream), HMAC over nonce||ciphertext, then base64(version||nonce||ct||mac).
func encryptWithNonce(conversationKey [32]byte, nonce []byte, plaintext []byte) (string, error) {
	if len(nonce) != nonceSize {
		return "", fmt.Errorf("nip44: nonce must be %d bytes, got %d", nonceSize, len(nonce))
	}
	chachaKey, chachaNonce, hmacKey, err := messageKeys(conversationKey, nonce)
	if err != nil {
		return "", err
	}
	padded, err := pad(plaintext)
	if err != nil {
		return "", err
	}
	c, err := chacha20.NewUnauthenticatedCipher(chachaKey[:], chachaNonce[:])
	if err != nil {
		return "", fmt.Errorf("nip44: chacha20 init: %w", err)
	}
	ciphertext := make([]byte, len(padded))
	c.XORKeyStream(ciphertext, padded)

	mac := hmacAAD(hmacKey, ciphertext, nonce)

	out := make([]byte, 0, 1+nonceSize+len(ciphertext)+macSize)
	out = append(out, version)
	out = append(out, nonce...)
	out = append(out, ciphertext...)
	out = append(out, mac...)
	return base64.StdEncoding.EncodeToString(out), nil
}

// decryptWithConversationKey is the byte-exact NIP-44 v2 decrypt core used by
// Open. It validates payload/version, verifies the HMAC in constant time BEFORE
// decrypting, then ChaCha20-decrypts and unpads. Every failure path returns an
// error (never a panic) — these are the official should-reject classes.
func decryptWithConversationKey(conversationKey [32]byte, payload string) ([]byte, error) {
	plen := len(payload)
	// Bounds mirror the spec: min is a 1-byte plaintext payload, max is a
	// 65535-byte plaintext payload, both base64-encoded.
	if plen < 132 || plen > 87472 {
		return nil, fmt.Errorf("nip44: invalid payload length %d", plen)
	}
	// '#' is the spec's reserved sentinel for a non-base64 / future encoding —
	// reject before attempting base64 decode.
	if payload[0] == '#' {
		return nil, errors.New("nip44: unknown encryption version")
	}
	data, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return nil, fmt.Errorf("nip44: base64 decode: %w", err)
	}
	dlen := len(data)
	if dlen < 99 || dlen > 65603 {
		return nil, fmt.Errorf("nip44: invalid decoded length %d", dlen)
	}
	if data[0] != version {
		return nil, fmt.Errorf("nip44: unknown version %d", data[0])
	}
	nonce := data[1 : 1+nonceSize]
	ciphertext := data[1+nonceSize : dlen-macSize]
	mac := data[dlen-macSize:]

	chachaKey, chachaNonce, hmacKey, err := messageKeys(conversationKey, nonce)
	if err != nil {
		return nil, err
	}
	calcMac := hmacAAD(hmacKey, ciphertext, nonce)
	if subtle.ConstantTimeCompare(calcMac, mac) != 1 {
		return nil, errors.New("nip44: invalid mac")
	}

	c, err := chacha20.NewUnauthenticatedCipher(chachaKey[:], chachaNonce[:])
	if err != nil {
		return nil, fmt.Errorf("nip44: chacha20 init: %w", err)
	}
	padded := make([]byte, len(ciphertext))
	c.XORKeyStream(padded, ciphertext)

	plaintext, err := unpad(padded)
	if err != nil {
		return nil, err
	}
	return plaintext, nil
}
