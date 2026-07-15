package identity

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"unsafe"

	btcec "github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
)

// Secp256k1Identity is the concrete Signer: a secp256k1 keypair that signs with
// BIP-340 Schnorr, exactly as nostr requires. It is the replacement for the
// dead campfire Ed25519 identity.
type Secp256k1Identity struct {
	priv *btcec.PrivateKey

	// memlocked records whether mlockLoadedScalar succeeded in pinning priv.Key
	// out of swap. Best-effort (dontguess-973 C3): false is expected and
	// non-fatal on hosts without CAP_IPC_LOCK / adequate RLIMIT_MEMLOCK.
	memlocked bool
}

// Memlocked reports whether the in-process private scalar was successfully
// mlock'd against swap (best-effort, platform- and privilege-dependent). This
// narrows, but does not remove, the in-process key-material exposure window —
// see docs/design/content-confidentiality-envelope-541.md §4.2/§3.5: only a
// hardware-ECDH HSM removes the in-process ECDH side-channel entirely; this
// mitigates the "swapped to disk" sub-case of that exposure, distinct from
// at-rest/transfer custody (1Password/file permissions).
func (i *Secp256k1Identity) Memlocked() bool {
	return i.memlocked
}

// mlockLoadedScalar best-effort mlock(2)'s the memory backing the loaded
// private scalar (btcec.PrivateKey.Key, a ModNScalar embedded by value) so it
// cannot be paged to disk swap. Failure is silent-by-design at this layer
// (non-fatal — see mlockBytes) and surfaced only via Memlocked() for callers
// that want to log it.
func (i *Secp256k1Identity) mlockLoadedScalar() {
	b := scalarBytes(unsafe.Pointer(&i.priv.Key), unsafe.Sizeof(i.priv.Key))
	i.memlocked = mlockBytes(b) == nil
}

// Generate mints a fresh secp256k1 identity from the crypto/rand CSPRNG.
//
// Callers must respect the key-management ruling: this is for provisioning a
// long-lived operator or fleet-member key. Ephemeral per-conversation subagents
// must NOT call Generate — they load their parent fleet-member's persisted key
// (see LoadOrCreate + agent-init) so convergence reputation stays continuous.
func Generate() (*Secp256k1Identity, error) {
	priv, err := btcec.NewPrivateKey()
	if err != nil {
		return nil, fmt.Errorf("secp256k1 keygen: %w", err)
	}
	id := &Secp256k1Identity{priv: priv}
	id.mlockLoadedScalar()
	return id, nil
}

// FromPrivHex loads an identity from a 32-byte hex-encoded private key. This is
// how a subagent adopts its parent fleet-member's key: same bytes in, same
// npub out.
func FromPrivHex(privHex string) (*Secp256k1Identity, error) {
	raw, err := hex.DecodeString(privHex)
	if err != nil {
		return nil, fmt.Errorf("decode private key hex: %w", err)
	}
	if len(raw) != 32 {
		return nil, fmt.Errorf("private key must be 32 bytes, got %d", len(raw))
	}
	// Reject the zero scalar and any value ≥ curve order: btcec clamps silently,
	// which would map distinct on-disk keys to one identity. A private key must
	// be a valid, non-zero scalar mod N.
	if !isValidScalar(raw) {
		return nil, fmt.Errorf("private key is not a valid secp256k1 scalar (zero or ≥ curve order)")
	}
	priv, _ := btcec.PrivKeyFromBytes(raw)
	id := &Secp256k1Identity{priv: priv}
	id.mlockLoadedScalar()
	return id, nil
}

// isValidScalar reports whether the 32-byte big-endian value is in [1, N-1]
// where N is the secp256k1 group order. btcec's ModNScalar.SetByteSlice returns
// true on overflow (value was reduced mod N), which we treat as invalid so that
// on-disk key material round-trips exactly.
func isValidScalar(b []byte) bool {
	var s btcec.ModNScalar
	overflow := s.SetByteSlice(b)
	if overflow {
		return false
	}
	return !s.IsZero()
}

// PrivHex serializes the private key as 32-byte lowercase hex for on-disk
// persistence. Handle only inside the identity home directory (mode 0600).
func (i *Secp256k1Identity) PrivHex() string {
	return hex.EncodeToString(i.priv.Serialize())
}

// PubKeyHex returns the 32-byte x-only public key (BIP-340 serialization) as
// lowercase hex — the nostr pubkey.
func (i *Secp256k1Identity) PubKeyHex() string {
	return hex.EncodeToString(schnorr.SerializePubKey(i.priv.PubKey()))
}

// Npub returns the NIP-19 bech32 "npub1…" encoding of the public key.
func (i *Secp256k1Identity) Npub() string {
	// EncodePubKeyToNpub only errors if the pubkey is not 32 bytes; a
	// schnorr-serialized key always is, so this cannot fail here.
	npub, err := EncodeNpub(schnorr.SerializePubKey(i.priv.PubKey()))
	if err != nil {
		// Unreachable given a 32-byte input; surface loudly rather than return
		// a malformed npub that would silently break allowlist matching.
		panic(fmt.Sprintf("identity: npub encode of valid key failed: %v", err))
	}
	return npub
}

// SignHash produces a 64-byte BIP-340 Schnorr signature over the 32-byte hash.
func (i *Secp256k1Identity) SignHash(hash [32]byte) ([]byte, error) {
	sig, err := schnorr.Sign(i.priv, hash[:])
	if err != nil {
		return nil, fmt.Errorf("schnorr sign: %w", err)
	}
	return sig.Serialize(), nil
}

// liftXOnlyToEvenY is the ONE place the BIP-340 x-only → even-Y (0x02) parity
// lift happens behind the Signer port. A 32-byte x-only key omits the Y
// coordinate's sign; BIP-340 defines its canonical point as the one with EVEN
// Y, and schnorr.ParsePubKey returns exactly that even-Y point. Every party
// (operator/seller/buyer) must lift identically before ECDH — a divergent lift
// yields a different shared point and makes the counterparty's decryption fail
// silently. Confining the lift here guarantees they cannot diverge.
func liftXOnlyToEvenY(xOnlyHex string) (*btcec.PublicKey, error) {
	raw, err := hex.DecodeString(xOnlyHex)
	if err != nil {
		return nil, fmt.Errorf("decode counterparty x-only pubkey hex: %w", err)
	}
	if len(raw) != 32 {
		return nil, fmt.Errorf("counterparty x-only pubkey must be 32 bytes, got %d", len(raw))
	}
	// schnorr.ParsePubKey applies the BIP-340 lift_x: it rejects x ≥ field prime
	// and x values with no square-root Y (not on curve), and returns the even-Y
	// point. This is the single authoritative lift.
	pub, err := schnorr.ParsePubKey(raw)
	if err != nil {
		return nil, fmt.Errorf("lift x-only pubkey to even-Y point: %w", err)
	}
	return pub, nil
}

// ECDH performs secp256k1 key agreement against the counterparty's x-only key
// and returns the raw 32-byte big-endian X coordinate of the shared point.
//
// It multiplies our private scalar by the counterparty's even-Y point and
// returns the affine X of the product. This is the RAW shared X that NIP-44 v2
// consumes — NOT sha256(X). btcec.GenerateSharedSecret is deliberately avoided
// because it hashes the X coordinate (NIP-04/ECIES style), which is the wrong
// value for NIP-44.
//
// The private scalar is read in place (i.priv.Key) and never returned or
// exported — this method leaks no key material.
func (i *Secp256k1Identity) ECDH(counterpartyXOnlyHex string) ([32]byte, error) {
	var shared [32]byte
	pub, err := liftXOnlyToEvenY(counterpartyXOnlyHex)
	if err != nil {
		return shared, err
	}
	var point, product btcec.JacobianPoint
	pub.AsJacobian(&point)
	// product = privScalar · counterpartyPoint. Both operands are valid (a
	// non-zero scalar mod N and a point of prime order N), so the product is
	// never the point at infinity.
	btcec.ScalarMultNonConst(&i.priv.Key, &point, &product)
	product.ToAffine()
	shared = *product.X.Bytes()
	return shared, nil
}

// randBytes is a small helper for callers needing CSPRNG bytes (challenge
// generation in the relay handshake). Kept here so the crypto source is
// centralized.
func randBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("read csprng: %w", err)
	}
	return b, nil
}

var _ Signer = (*Secp256k1Identity)(nil)
