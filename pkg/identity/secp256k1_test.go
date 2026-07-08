package identity

import (
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	btcec "github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
)

// TestGenerate_DistinctValidKeys proves keygen yields distinct, well-formed
// secp256k1 identities: 32-byte x-only pubkeys, valid npub encoding, and a
// private key that round-trips through hex.
func TestGenerate_DistinctValidKeys(t *testing.T) {
	t.Parallel()

	seen := make(map[string]bool)
	for i := 0; i < 50; i++ {
		id, err := Generate()
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}

		pkHex := id.PubKeyHex()
		raw, err := hex.DecodeString(pkHex)
		if err != nil {
			t.Fatalf("pubkey not hex: %v", err)
		}
		if len(raw) != 32 {
			t.Fatalf("pubkey is %d bytes, want 32 (x-only)", len(raw))
		}
		if _, err := schnorr.ParsePubKey(raw); err != nil {
			t.Fatalf("pubkey not a valid BIP-340 point: %v", err)
		}
		if seen[pkHex] {
			t.Fatalf("duplicate pubkey from Generate: %s", pkHex)
		}
		seen[pkHex] = true

		// npub must decode back to the same pubkey.
		npub := id.Npub()
		if !strings.HasPrefix(npub, "npub1") {
			t.Fatalf("npub %q missing npub1 prefix", npub)
		}
		backHex, err := DecodeNpubToHex(npub)
		if err != nil {
			t.Fatalf("DecodeNpubToHex: %v", err)
		}
		if backHex != pkHex {
			t.Fatalf("npub round-trip mismatch: %s vs %s", backHex, pkHex)
		}

		// Private key must round-trip and reproduce the same pubkey.
		reloaded, err := FromPrivHex(id.PrivHex())
		if err != nil {
			t.Fatalf("FromPrivHex: %v", err)
		}
		if reloaded.PubKeyHex() != pkHex {
			t.Fatalf("priv key round-trip changed pubkey: %s vs %s", reloaded.PubKeyHex(), pkHex)
		}
	}
}

// TestFromPrivHex_Rejects covers invalid private key material: bad hex, wrong
// length, the zero scalar, and a value ≥ the curve order (which btcec would
// otherwise silently reduce mod N, aliasing distinct on-disk keys).
func TestFromPrivHex_Rejects(t *testing.T) {
	t.Parallel()

	// secp256k1 group order N and N (== overflow to zero) both invalid.
	const curveOrderN = "fffffffffffffffffffffffffffffffebaaedce6af48a03bbfd25e8cd0364141"
	cases := map[string]string{
		"bad hex":      "nothex!!",
		"too short":    "00112233",
		"too long":     strings.Repeat("ab", 33),
		"zero scalar":  strings.Repeat("00", 32),
		"equals order": curveOrderN,
		"above order":  "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
	}
	for name, in := range cases {
		if _, err := FromPrivHex(in); err == nil {
			t.Errorf("%s: expected error, got nil for %q", name, in)
		}
	}
}

// TestSignVerify_RoundTrip proves a signature over a hash verifies, and that
// tampering with the hash or using a different key fails verification.
func TestSignVerify_RoundTrip(t *testing.T) {
	t.Parallel()

	id, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	var hash [32]byte
	copy(hash[:], []byte("dontguess-476 nip42 test message"))

	sig, err := id.SignHash(hash)
	if err != nil {
		t.Fatalf("SignHash: %v", err)
	}
	if len(sig) != schnorr.SignatureSize {
		t.Fatalf("sig is %d bytes, want %d", len(sig), schnorr.SignatureSize)
	}

	pub, err := schnorr.ParsePubKey(mustHex(t, id.PubKeyHex()))
	if err != nil {
		t.Fatalf("ParsePubKey: %v", err)
	}
	parsed, err := schnorr.ParseSignature(sig)
	if err != nil {
		t.Fatalf("ParseSignature: %v", err)
	}
	if !parsed.Verify(hash[:], pub) {
		t.Fatal("valid signature failed to verify")
	}

	// Tamper the message: must not verify.
	var bad [32]byte
	copy(bad[:], hash[:])
	bad[0] ^= 0xff
	if parsed.Verify(bad[:], pub) {
		t.Fatal("signature verified over a tampered message")
	}

	// Different key: must not verify.
	other, _ := Generate()
	otherPub, _ := schnorr.ParsePubKey(mustHex(t, other.PubKeyHex()))
	if parsed.Verify(hash[:], otherPub) {
		t.Fatal("signature verified under the wrong public key")
	}
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("decode hex %q: %v", s, err)
	}
	return b
}

// bip340Vectors holds the official BIP-340 test vectors 0-14 (the core
// sign+verify set with fixed 32-byte messages), copied verbatim from
// https://github.com/bitcoin/bips/blob/master/bip-0340/test-vectors.csv .
// Vectors 15-18 (added 2022-12) use variable-length raw messages, which
// btcec's schnorr.Sign does not support (it requires an exactly-32-byte
// pre-hashed message), so they are not applicable to this wrapper.
var bip340Vectors = []struct {
	index      int
	secKeyHex  string // empty when the vector is verify-only
	pubKeyHex  string
	auxRandHex string // empty when the vector is verify-only
	msgHex     string
	sigHex     string
	valid      bool
	comment    string
}{
	{index: 0, secKeyHex: "0000000000000000000000000000000000000000000000000000000000000003", pubKeyHex: "F9308A019258C31049344F85F89D5229B531C845836F99B08601F113BCE036F9", auxRandHex: "0000000000000000000000000000000000000000000000000000000000000000", msgHex: "0000000000000000000000000000000000000000000000000000000000000000", sigHex: "E907831F80848D1069A5371B402410364BDF1C5F8307B0084C55F1CE2DCA821525F66A4A85EA8B71E482A74F382D2CE5EBEEE8FDB2172F477DF4900D310536C0", valid: true, comment: ""},
	{index: 1, secKeyHex: "B7E151628AED2A6ABF7158809CF4F3C762E7160F38B4DA56A784D9045190CFEF", pubKeyHex: "DFF1D77F2A671C5F36183726DB2341BE58FEAE1DA2DECED843240F7B502BA659", auxRandHex: "0000000000000000000000000000000000000000000000000000000000000001", msgHex: "243F6A8885A308D313198A2E03707344A4093822299F31D0082EFA98EC4E6C89", sigHex: "6896BD60EEAE296DB48A229FF71DFE071BDE413E6D43F917DC8DCF8C78DE33418906D11AC976ABCCB20B091292BFF4EA897EFCB639EA871CFA95F6DE339E4B0A", valid: true, comment: ""},
	{index: 2, secKeyHex: "C90FDAA22168C234C4C6628B80DC1CD129024E088A67CC74020BBEA63B14E5C9", pubKeyHex: "DD308AFEC5777E13121FA72B9CC1B7CC0139715309B086C960E18FD969774EB8", auxRandHex: "C87AA53824B4D7AE2EB035A2B5BBBCCC080E76CDC6D1692C4B0B62D798E6D906", msgHex: "7E2D58D8B3BCDF1ABADEC7829054F90DDA9805AAB56C77333024B9D0A508B75C", sigHex: "5831AAEED7B44BB74E5EAB94BA9D4294C49BCF2A60728D8B4C200F50DD313C1BAB745879A5AD954A72C45A91C3A51D3C7ADEA98D82F8481E0E1E03674A6F3FB7", valid: true, comment: ""},
	{index: 3, secKeyHex: "0B432B2677937381AEF05BB02A66ECD012773062CF3FA2549E44F58ED2401710", pubKeyHex: "25D1DFF95105F5253C4022F628A996AD3A0D95FBF21D468A1B33F8C160D8F517", auxRandHex: "FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF", msgHex: "FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF", sigHex: "7EB0509757E246F19449885651611CB965ECC1A187DD51B64FDA1EDC9637D5EC97582B9CB13DB3933705B32BA982AF5AF25FD78881EBB32771FC5922EFC66EA3", valid: true, comment: "test fails if msg is reduced modulo p or n"},
	{index: 4, secKeyHex: "", pubKeyHex: "D69C3509BB99E412E68B0FE8544E72837DFA30746D8BE2AA65975F29D22DC7B9", auxRandHex: "", msgHex: "4DF3C3F68FCC83B27E9D42C90431A72499F17875C81A599B566C9889B9696703", sigHex: "00000000000000000000003B78CE563F89A0ED9414F5AA28AD0D96D6795F9C6376AFB1548AF603B3EB45C9F8207DEE1060CB71C04E80F593060B07D28308D7F4", valid: true, comment: ""},
	{index: 5, secKeyHex: "", pubKeyHex: "EEFDEA4CDB677750A420FEE807EACF21EB9898AE79B9768766E4FAA04A2D4A34", auxRandHex: "", msgHex: "243F6A8885A308D313198A2E03707344A4093822299F31D0082EFA98EC4E6C89", sigHex: "6CFF5C3BA86C69EA4B7376F31A9BCB4F74C1976089B2D9963DA2E5543E17776969E89B4C5564D00349106B8497785DD7D1D713A8AE82B32FA79D5F7FC407D39B", valid: false, comment: "public key not on the curve"},
	{index: 6, secKeyHex: "", pubKeyHex: "DFF1D77F2A671C5F36183726DB2341BE58FEAE1DA2DECED843240F7B502BA659", auxRandHex: "", msgHex: "243F6A8885A308D313198A2E03707344A4093822299F31D0082EFA98EC4E6C89", sigHex: "FFF97BD5755EEEA420453A14355235D382F6472F8568A18B2F057A14602975563CC27944640AC607CD107AE10923D9EF7A73C643E166BE5EBEAFA34B1AC553E2", valid: false, comment: "has_even_y(R) is false"},
	{index: 7, secKeyHex: "", pubKeyHex: "DFF1D77F2A671C5F36183726DB2341BE58FEAE1DA2DECED843240F7B502BA659", auxRandHex: "", msgHex: "243F6A8885A308D313198A2E03707344A4093822299F31D0082EFA98EC4E6C89", sigHex: "1FA62E331EDBC21C394792D2AB1100A7B432B013DF3F6FF4F99FCB33E0E1515F28890B3EDB6E7189B630448B515CE4F8622A954CFE545735AAEA5134FCCDB2BD", valid: false, comment: "negated message"},
	{index: 8, secKeyHex: "", pubKeyHex: "DFF1D77F2A671C5F36183726DB2341BE58FEAE1DA2DECED843240F7B502BA659", auxRandHex: "", msgHex: "243F6A8885A308D313198A2E03707344A4093822299F31D0082EFA98EC4E6C89", sigHex: "6CFF5C3BA86C69EA4B7376F31A9BCB4F74C1976089B2D9963DA2E5543E177769961764B3AA9B2FFCB6EF947B6887A226E8D7C93E00C5ED0C1834FF0D0C2E6DA6", valid: false, comment: "negated s value"},
	{index: 9, secKeyHex: "", pubKeyHex: "DFF1D77F2A671C5F36183726DB2341BE58FEAE1DA2DECED843240F7B502BA659", auxRandHex: "", msgHex: "243F6A8885A308D313198A2E03707344A4093822299F31D0082EFA98EC4E6C89", sigHex: "0000000000000000000000000000000000000000000000000000000000000000123DDA8328AF9C23A94C1FEECFD123BA4FB73476F0D594DCB65C6425BD186051", valid: false, comment: "sG - eP is infinite. Test fails in single verification if has_even_y(inf) is defined as true and x(inf) as 0"},
	{index: 10, secKeyHex: "", pubKeyHex: "DFF1D77F2A671C5F36183726DB2341BE58FEAE1DA2DECED843240F7B502BA659", auxRandHex: "", msgHex: "243F6A8885A308D313198A2E03707344A4093822299F31D0082EFA98EC4E6C89", sigHex: "00000000000000000000000000000000000000000000000000000000000000017615FBAF5AE28864013C099742DEADB4DBA87F11AC6754F93780D5A1837CF197", valid: false, comment: "sG - eP is infinite. Test fails in single verification if has_even_y(inf) is defined as true and x(inf) as 1"},
	{index: 11, secKeyHex: "", pubKeyHex: "DFF1D77F2A671C5F36183726DB2341BE58FEAE1DA2DECED843240F7B502BA659", auxRandHex: "", msgHex: "243F6A8885A308D313198A2E03707344A4093822299F31D0082EFA98EC4E6C89", sigHex: "4A298DACAE57395A15D0795DDBFD1DCB564DA82B0F269BC70A74F8220429BA1D69E89B4C5564D00349106B8497785DD7D1D713A8AE82B32FA79D5F7FC407D39B", valid: false, comment: "sig[0:32] is not an X coordinate on the curve"},
	{index: 12, secKeyHex: "", pubKeyHex: "DFF1D77F2A671C5F36183726DB2341BE58FEAE1DA2DECED843240F7B502BA659", auxRandHex: "", msgHex: "243F6A8885A308D313198A2E03707344A4093822299F31D0082EFA98EC4E6C89", sigHex: "FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFEFFFFFC2F69E89B4C5564D00349106B8497785DD7D1D713A8AE82B32FA79D5F7FC407D39B", valid: false, comment: "sig[0:32] is equal to field size"},
	{index: 13, secKeyHex: "", pubKeyHex: "DFF1D77F2A671C5F36183726DB2341BE58FEAE1DA2DECED843240F7B502BA659", auxRandHex: "", msgHex: "243F6A8885A308D313198A2E03707344A4093822299F31D0082EFA98EC4E6C89", sigHex: "6CFF5C3BA86C69EA4B7376F31A9BCB4F74C1976089B2D9963DA2E5543E177769FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFEBAAEDCE6AF48A03BBFD25E8CD0364141", valid: false, comment: "sig[32:64] is equal to curve order"},
	{index: 14, secKeyHex: "", pubKeyHex: "FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFEFFFFFC30", auxRandHex: "", msgHex: "243F6A8885A308D313198A2E03707344A4093822299F31D0082EFA98EC4E6C89", sigHex: "6CFF5C3BA86C69EA4B7376F31A9BCB4F74C1976089B2D9963DA2E5543E17776969E89B4C5564D00349106B8497785DD7D1D713A8AE82B32FA79D5F7FC407D39B", valid: false, comment: "public key is not a valid X coordinate because it exceeds the field size"},
}

// TestBIP340OfficialVectors runs the official BIP-340 test vectors against
// the exact primitives this package's Signer relies on (btcec's
// schnorr.Sign/schnorr.ParsePubKey/schnorr.ParseSignature/Verify). Prior
// coverage (TestSignVerify_RoundTrip) only proves self-consistency — our own
// sign output verifies under our own verify call, which would pass even if
// both sides shared a matching bug. These vectors are independently computed
// by the BIP-340 authors, so they catch a wrong-but-self-consistent
// implementation.
func TestBIP340OfficialVectors(t *testing.T) {
	t.Parallel()

	for _, v := range bip340Vectors {
		v := v
		t.Run(fmt.Sprintf("vector_%d", v.index), func(t *testing.T) {
			t.Parallel()

			msg := mustHex(t, v.msgHex)

			// Sign-side check: vectors with a secret key must reproduce the
			// exact expected signature when signed with the vector's
			// aux_rand via BIP-340's own (non-RFC6979) nonce derivation.
			if v.secKeyHex != "" {
				privRaw := mustHex(t, v.secKeyHex)
				priv, pub := btcec.PrivKeyFromBytes(privRaw)
				_ = pub

				var aux [32]byte
				copy(aux[:], mustHex(t, v.auxRandHex))

				sig, err := schnorr.Sign(priv, msg, schnorr.CustomNonce(aux))
				if err != nil {
					t.Fatalf("schnorr.Sign: %v", err)
				}
				gotSig := strings.ToUpper(hex.EncodeToString(sig.Serialize()))
				wantSig := strings.ToUpper(v.sigHex)
				if gotSig != wantSig {
					t.Fatalf("signature mismatch (%s):\n got  %s\n want %s", v.comment, gotSig, wantSig)
				}

				gotPub := strings.ToUpper(hex.EncodeToString(schnorr.SerializePubKey(priv.PubKey())))
				if gotPub != strings.ToUpper(v.pubKeyHex) {
					t.Fatalf("derived pubkey %s does not match vector pubkey %s", gotPub, v.pubKeyHex)
				}
			}

			// Verify-side check: every vector (sign or verify-only) must
			// produce the expected accept/reject verdict. An invalid public
			// key or signature encoding is itself a correct FALSE verdict.
			pubRaw, pubErr := hex.DecodeString(v.pubKeyHex)
			if pubErr != nil {
				t.Fatalf("vector pubkey not hex: %v", pubErr)
			}
			pub, err := schnorr.ParsePubKey(pubRaw)
			if err != nil {
				if v.valid {
					t.Fatalf("expected valid, but ParsePubKey failed (%s): %v", v.comment, err)
				}
				return
			}

			sigRaw, sigErr := hex.DecodeString(v.sigHex)
			if sigErr != nil {
				t.Fatalf("vector signature not hex: %v", sigErr)
			}
			sig, err := schnorr.ParseSignature(sigRaw)
			if err != nil {
				if v.valid {
					t.Fatalf("expected valid, but ParseSignature failed (%s): %v", v.comment, err)
				}
				return
			}

			got := sig.Verify(msg, pub)
			if got != v.valid {
				t.Fatalf("verify = %v, want %v (%s)", got, v.valid, v.comment)
			}
		})
	}
}
