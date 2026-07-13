package identity

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// TestECDH_SymmetryOverRandomKeys is the mandatory done-gate: real secp256k1
// key agreement over crypto/rand keys must be symmetric. Two freshly generated
// identities, lifting each other's x-only pubkey and multiplying by their own
// scalar from OPPOSITE sides, MUST derive the byte-identical shared secret
// (d_A · P_B == d_B · P_A). No mocks, no fixed dummy scalars — if this fails,
// the even-Y lift or the scalar-mult path is wrong and buyers would silently
// fail to decrypt.
func TestECDH_SymmetryOverRandomKeys(t *testing.T) {
	t.Parallel()

	for iter := 0; iter < 32; iter++ {
		a, err := Generate()
		if err != nil {
			t.Fatalf("Generate A: %v", err)
		}
		b, err := Generate()
		if err != nil {
			t.Fatalf("Generate B: %v", err)
		}

		sa, err := a.ECDH(b.PubKeyHex())
		if err != nil {
			t.Fatalf("A.ECDH(B): %v", err)
		}
		sb, err := b.ECDH(a.PubKeyHex())
		if err != nil {
			t.Fatalf("B.ECDH(A): %v", err)
		}

		if sa != sb {
			t.Fatalf("shared secret asymmetric on iter %d:\n A→B %x\n B→A %x", iter, sa, sb)
		}
		if len(sa) != 32 {
			t.Fatalf("shared secret is %d bytes, want 32", len(sa))
		}
		if sa == ([32]byte{}) {
			t.Fatalf("shared secret is all-zero — degenerate key agreement")
		}
	}
}

// nip44ConversationKeyVectors are official NIP-44 v2 reference vectors
// (paulmillr/nip44 · v2.valid.get_conversation_key). Each is (sec1, pub2,
// conversation_key) where conversation_key == HMAC-SHA256(key="nip44-v2",
// data=ecdh_shared_x). They are the external ground truth that pins TWO edges
// this item owns:
//
//   - RAW shared X, not sha256(X): if ECDH returned the NIP-04/ECIES-style
//     sha256(X), HMAC-SHA256("nip44-v2", sha256(X)) would NOT equal the
//     published conversation_key. Symmetry tests can't catch that (sha256 is
//     also symmetric); only a known-answer vector can.
//   - the BIP-340 x-only → even-Y lift: pub2 is a 32-byte x-only key; only the
//     even-Y lift reproduces the vector.
var nip44ConversationKeyVectors = []struct {
	sec1, pub2, conversationKey string
}{
	{
		sec1:            "315e59ff51cb9209768cf7da80791ddcaae56ac9775eb25b6dee1234bc5d2268",
		pub2:            "c2f9d9948dc8c7c38321e4b85c8558872eafa0641cd269db76848a6073e69133",
		conversationKey: "3dfef0ce2a4d80a25e7a328accf73448ef67096f65f79588e358d9a0eb9013f1",
	},
	{
		sec1:            "a1e37752c9fdc1273be53f68c5f74be7c8905728e8de75800b94262f9497c86e",
		pub2:            "03bb7947065dde12ba991ea045132581d0954f042c84e06d8c00066e23c1a800",
		conversationKey: "4d14f36e81b8452128da64fe6f1eae873baae2f444b02c950b90e43553f2178b",
	},
	{
		sec1:            "98a5902fd67518a0c900f0fb62158f278f94a21d6f9d33d30cd3091195500311",
		pub2:            "aae65c15f98e5e677b5050de82e3aba47a6fe49b3dab7863cf35d9478ba9f7d1",
		conversationKey: "9c00b769d5f54d02bf175b7284a1cbd28b6911b06cda6666b2243561ac96bad7",
	},
	{
		sec1:            "86ae5ac8034eb2542ce23ec2f84375655dab7f836836bbd3c54cefe9fdc9c19f",
		pub2:            "59f90272378089d73f1339710c02e2be6db584e9cdbe86eed3578f0c67c23585",
		conversationKey: "19f934aafd3324e8415299b64df42049afaa051c71c98d0aa10e1081f2e3e2ba",
	},
	{
		sec1:            "2528c287fe822421bc0dc4c3615878eb98e8a8c31657616d08b29c00ce209e34",
		pub2:            "f66ea16104c01a1c532e03f166c5370a22a5505753005a566366097150c6df60",
		conversationKey: "c833bbb292956c43366145326d53b955ffb5da4e4998a2d853611841903f5442",
	},
}

// TestECDH_NIP44KnownAnswerVectors validates ECDH against official NIP-44 v2
// vectors. It reconstructs the NIP-44 conversation key from our raw shared X
// (HMAC-SHA256("nip44-v2", sharedX)) and asserts it equals the published value.
// This is the ground-truth check that our shared secret is the RAW X coordinate
// with the correct even-Y lift — not a self-referential comparison against the
// same library code path.
func TestECDH_NIP44KnownAnswerVectors(t *testing.T) {
	t.Parallel()

	for _, v := range nip44ConversationKeyVectors {
		id, err := FromPrivHex(v.sec1)
		if err != nil {
			t.Fatalf("FromPrivHex(%s): %v", v.sec1, err)
		}
		sharedX, err := id.ECDH(v.pub2)
		if err != nil {
			t.Fatalf("ECDH(%s): %v", v.pub2, err)
		}

		// NIP-44 v2 get_conversation_key = hkdf_extract(IKM=sharedX,
		// salt="nip44-v2") = HMAC-SHA256(key="nip44-v2", data=sharedX).
		mac := hmac.New(sha256.New, []byte("nip44-v2"))
		mac.Write(sharedX[:])
		gotKey := mac.Sum(nil)

		wantKey, err := hex.DecodeString(v.conversationKey)
		if err != nil {
			t.Fatalf("decode want conversation_key: %v", err)
		}
		if !bytes.Equal(gotKey, wantKey) {
			t.Fatalf("NIP-44 conversation key mismatch for pub2=%s\n got  %x\n want %x\n(raw shared X was %x)",
				v.pub2, gotKey, wantKey, sharedX)
		}
	}
}

// TestECDH_InvalidCounterparty proves a malformed counterparty key returns an
// error, never a panic. Covers non-hex, odd-length hex, wrong byte length, and
// an x-coordinate that is not a valid curve point (x ≥ field prime).
func TestECDH_InvalidCounterparty(t *testing.T) {
	t.Parallel()

	id, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	cases := []struct {
		name string
		hex  string
	}{
		{"non-hex", "zzzz"},
		{"odd-length-hex", "abc"},
		{"too-short", "abcd"},
		{"too-long-33-bytes", "02" + "c2f9d9948dc8c7c38321e4b85c8558872eafa0641cd269db76848a6073e69133"},
		{"empty", ""},
		{"x-not-on-curve", "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			shared, err := id.ECDH(tc.hex)
			if err == nil {
				t.Fatalf("expected error for %q, got shared=%x", tc.hex, shared)
			}
			if shared != ([32]byte{}) {
				t.Fatalf("expected zero shared secret on error path, got %x", shared)
			}
		})
	}
}
