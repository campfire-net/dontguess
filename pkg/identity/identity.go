// Package identity is dontguess's identity port: the abstraction over a fleet
// member's cryptographic identity, re-keyed from campfire's Ed25519 to
// secp256k1/BIP-340 Schnorr for the nostr-first rebuild.
//
// Design authority:
//   - docs/design/nostr-first-rebuild-decision.md — "Key-management ruling"
//     (A17) and the NFR key-mgmt row.
//   - docs/design/convergence-sybil-defense.md — why identity independence is
//     load-bearing (convergence is scored at npub granularity).
//
// The key-management ruling this port enforces by construction:
//
//	(1) operator identity is a single long-lived npub that signs
//	    match/settle/mint/burn;
//	(2) each long-lived fleet member has a persistent npub;
//	(3) ephemeral per-conversation subagents sign with their PARENT
//	    fleet-member's npub — never a fresh throwaway (unbounded churn destroys
//	    reputation continuity) and never the operator key (collapses convergence
//	    to "the operator agreed with itself").
//
// The port itself does not police who calls it — that discipline lives in
// agent-init (which reuses a parent's persisted key rather than minting one)
// and in the allowlist enforced at the NIP-42 handshake. What the port
// guarantees is that every identity is a real secp256k1 keypair capable of
// producing a BIP-340 Schnorr signature a nostr relay will accept.
package identity

// Signer is the identity port. It abstracts a secp256k1 keypair down to the
// three operations the rest of dontguess needs: read the nostr public key (hex
// and bech32 npub), and produce a BIP-340 Schnorr signature over a 32-byte
// hash. Everything nostr-facing (event signing, the NIP-42 AUTH handshake)
// composes these three primitives, so mock identities and hardware-backed keys
// can drop in behind the same interface.
type Signer interface {
	// PubKeyHex returns the 32-byte x-only public key as lowercase hex. This is
	// the nostr "pubkey" field carried on every event and the key an allowlist
	// is checked against.
	PubKeyHex() string

	// Npub returns the NIP-19 bech32 encoding of the public key (the
	// human-facing "npub1…" form used in fleet allowlist config).
	Npub() string

	// SignHash produces a 64-byte BIP-340 Schnorr signature over the supplied
	// 32-byte hash. Nostr event signatures are a signature over the event id,
	// which is itself a sha256 — hence a fixed 32-byte input.
	SignHash(hash [32]byte) ([]byte, error)

	// ECDH performs secp256k1 elliptic-curve Diffie-Hellman key agreement with a
	// counterparty and returns the RAW 32-byte big-endian X coordinate of the
	// shared point (our_scalar · their_point). This is the shared secret NIP-44
	// v2 feeds into its HKDF-extract — it is deliberately NOT sha256(X) (the
	// NIP-04/ECIES convention that btcec.GenerateSharedSecret returns), which
	// would be the wrong input for NIP-44.
	//
	// counterpartyXOnlyHex is the counterparty's 32-byte BIP-340 x-only public
	// key as lowercase hex (the nostr "pubkey"). It is lifted to its even-Y
	// (0x02) point before the multiplication; every party must lift identically
	// or the derived secret diverges and the counterparty's decryption silently
	// fails. That lift is centralized in exactly one place behind this port.
	//
	// The one secp256k1 keypair both Schnorr-signs (SignHash) and performs this
	// key agreement — nostr convention. A hardware-backed Signer must therefore
	// implement key agreement, not only signing.
	//
	// This accessor delivers ONLY the raw ECDH shared secret. NIP-44's KDF,
	// padding, ChaCha20-Poly1305, and HMAC live in a separate package that calls
	// this method.
	ECDH(counterpartyXOnlyHex string) ([32]byte, error)
}
