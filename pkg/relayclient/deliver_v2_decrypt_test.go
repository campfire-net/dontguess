package relayclient

// deliver_v2_decrypt_test.go — dontguess-5db: the BUYER's end-to-end decrypt of a
// §3.4 v2 confidential settle(deliver). These exercise verifyDeliver/decryptDeliverV2
// with REAL secp256k1 identities, REAL NIP-44 wraps, and REAL ChaCha20-Poly1305 —
// no crypto mocks. The put ciphertext is fetched over a FAKE conn (the real relay
// fetch is exercised by cmd/dontguess TestE2E_TeamRoundTrip). Proven here:
//
//	(a) a v2 deliver + the referenced put's ciphertext ⇒ the buyer unwraps, fetches,
//	    hash-verifies, and AEAD-decrypts to the ORIGINAL plaintext, byte-for-byte;
//	(b) a ciphertext_hash mismatch ⇒ error, and NO settle(complete);
//	(c) a wrong-buyer Signer CANNOT unwrap the CEK ⇒ error (§4.5.4 replay-to-other-
//	    buyer-undecryptable), both at the recipient fast-fail and at the crypto layer;
//	(d) a legacy individual-tier plaintext deliver still decodes (backward compat).

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/3dl-dev/dontguess/pkg/identity"
	"github.com/3dl-dev/dontguess/pkg/nip44"
	"github.com/3dl-dev/dontguess/pkg/relay"
)

// fakeFetchConn is a deliverConn that serves a fixed list of pre-encoded frames
// on Recv (the referenced put event), then blocks until ctx expiry. Send records
// the REQ frames the fetch issued.
type fakeFetchConn struct {
	mu     sync.Mutex
	frames [][]byte
	sent   [][]byte
}

func (f *fakeFetchConn) Send(_ context.Context, frame []byte) error {
	f.mu.Lock()
	f.sent = append(f.sent, frame)
	f.mu.Unlock()
	return nil
}

func (f *fakeFetchConn) Recv(ctx context.Context) ([]byte, error) {
	f.mu.Lock()
	if len(f.frames) > 0 {
		fr := f.frames[0]
		f.frames = f.frames[1:]
		f.mu.Unlock()
		return fr, nil
	}
	f.mu.Unlock()
	<-ctx.Done()
	return nil, ctx.Err()
}

// v2Fixture bundles a fully wired v2 deliver scenario: a real signed 3401 put
// event carrying the inline ciphertext, and the operator-re-wrapped CEK to the
// buyer — exactly what the operator's emitDeliverEnvelope produces (dontguess-9e8).
type v2Fixture struct {
	seller, operator, buyer identity.Signer
	plaintext               []byte
	putEv                   *identity.Event // real signed 3401 put with enc.ciphertext
	ciphertextHash          string
	wrappedForBuyer         string // NIP-44(operatorPriv, buyerPub, CEK)
	cek                     []byte
}

// newV2Fixture builds the fixture the way the whole pipeline does: seller
// buildPutMessage (CEK-gen + AEAD + wrap-to-operator), operator unwraps the CEK
// and re-seals it to the buyer.
func newV2Fixture(t *testing.T, plaintext []byte) *v2Fixture {
	t.Helper()
	seller, err := identity.Generate()
	if err != nil {
		t.Fatalf("generate seller: %v", err)
	}
	operator, err := identity.Generate()
	if err != nil {
		t.Fatalf("generate operator: %v", err)
	}
	buyer, err := identity.Generate()
	if err != nil {
		t.Fatalf("generate buyer: %v", err)
	}

	putMsg, err := buildPutMessage(seller, PutRequest{
		Description:    "v2 deliver decrypt fixture",
		Content:        plaintext,
		TokenCost:      4242,
		ContentType:    "exchange:content-type:code",
		Domains:        []string{"go"},
		OperatorPubKey: operator.PubKeyHex(),
	})
	if err != nil {
		t.Fatalf("buildPutMessage: %v", err)
	}
	// The real signed 3401 put event (the buyer REQ-fetches this by id).
	putEv, err := signAsIdentityEvent(seller, putMsg)
	if err != nil {
		t.Fatalf("sign put event: %v", err)
	}

	var pp struct {
		Enc struct {
			Ciphertext     string `json:"ciphertext"`
			CiphertextHash string `json:"ciphertext_hash"`
			KeyWrap        struct {
				Wrapped string `json:"wrapped"`
			} `json:"key_wrap"`
		} `json:"enc"`
	}
	if err := json.Unmarshal(putMsg.Payload, &pp); err != nil {
		t.Fatalf("parse put payload: %v", err)
	}

	// Operator unwraps the seller's wrap-to-operator, then re-seals the SAME CEK to
	// the buyer — the deliver-side re-wrap pivot (§3.1(5)).
	cek, err := nip44.Open(operator, seller.PubKeyHex(), pp.Enc.KeyWrap.Wrapped)
	if err != nil {
		t.Fatalf("operator unwrap CEK: %v", err)
	}
	wrappedForBuyer, err := nip44.Seal(operator, buyer.PubKeyHex(), cek)
	if err != nil {
		t.Fatalf("operator re-wrap CEK to buyer: %v", err)
	}

	return &v2Fixture{
		seller: seller, operator: operator, buyer: buyer,
		plaintext:       plaintext,
		putEv:           putEv,
		ciphertextHash:  pp.Enc.CiphertextHash,
		wrappedForBuyer: wrappedForBuyer,
		cek:             cek,
	}
}

// deliverEvent builds an operator-authored v2 settle(deliver) event pointing at
// the fixture's put, with the given key_wrap recipient/wrapped and ciphertext_hash
// (so a test can tamper any of them). ev.PubKey is the operator key — verifyDeliver
// unwraps the CEK from the deliver's AUTHOR.
func (f *v2Fixture) deliverEvent(recipient, wrapped, ciphertextHash string) *identity.Event {
	payload, _ := json.Marshal(map[string]any{
		"phase":           "deliver",
		"v":               2,
		"entry_id":        "entry-1",
		"content_alg":     "chacha20poly1305",
		"ciphertext_ref":  map[string]any{"put_event": f.putEv.ID},
		"ciphertext_hash": ciphertextHash,
		"key_wrap": map[string]any{
			"alg":       "nip44-v2-secp256k1",
			"recipient": recipient,
			"wrapped":   wrapped,
		},
	})
	return &identity.Event{
		ID:      "deliver-wire-id",
		PubKey:  f.operator.PubKeyHex(),
		Content: string(payload),
	}
}

// fetchConn returns a fake conn that serves the fixture's put event on Recv.
func (f *v2Fixture) fetchConn(t *testing.T) *fakeFetchConn {
	t.Helper()
	frame, err := relay.EncodeSubEvent("dg-fetch", f.putEv)
	if err != nil {
		t.Fatalf("encode put sub-event: %v", err)
	}
	return &fakeFetchConn{frames: [][]byte{frame}}
}

// (a) full v2 round-trip: unwrap + fetch + hash-verify + AEAD-decrypt to the
// ORIGINAL plaintext, byte-for-byte.
func TestVerifyDeliverV2_RoundTrip_DecryptsToOriginalPlaintext(t *testing.T) {
	fx := newV2Fixture(t, []byte("package main\n\nfunc TestHandler(t *testing.T) {\n\t// the exact bytes the buyer must recover\n}\n"))
	deliverEv := fx.deliverEvent(fx.buyer.PubKeyHex(), fx.wrappedForBuyer, fx.ciphertextHash)
	conn := fx.fetchConn(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	got, gotHash, err := verifyDeliver(ctx, conn, fx.buyer, deliverEv, 0)
	if err != nil {
		t.Fatalf("verifyDeliver (v2 happy path): %v", err)
	}
	if string(got) != string(fx.plaintext) {
		t.Fatalf("decrypted plaintext mismatch:\n got %q\nwant %q", got, fx.plaintext)
	}
	if gotHash != fx.ciphertextHash {
		t.Fatalf("returned hash = %q, want the ciphertext_hash %q", gotHash, fx.ciphertextHash)
	}
	// The fetch actually issued a REQ (proving it went to the relay, not a cache).
	if len(conn.sent) == 0 {
		t.Fatalf("expected the buyer to issue a put-fetch REQ, got none")
	}
}

// (b) ciphertext_hash mismatch ⇒ error, and crucially NO settle(complete): a
// verifyDeliver error aborts the settle chain before the complete is published.
func TestVerifyDeliverV2_CiphertextHashMismatch_FailsLoud(t *testing.T) {
	fx := newV2Fixture(t, []byte("genuine cached inference result bytes"))
	// Tamper the claimed hash; the fetched ciphertext is genuine, so sha256 diverges.
	deliverEv := fx.deliverEvent(fx.buyer.PubKeyHex(), fx.wrappedForBuyer, "sha256:deadbeef")
	conn := fx.fetchConn(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	got, _, err := verifyDeliver(ctx, conn, fx.buyer, deliverEv, 0)
	if err == nil {
		t.Fatalf("expected a LOUD ciphertext-hash mismatch error, got nil (plaintext=%q)", got)
	}
	if !strings.Contains(err.Error(), "CIPHERTEXT HASH MISMATCH") {
		t.Fatalf("error %q does not surface the ciphertext-hash mismatch", err)
	}
	if got != nil {
		t.Fatalf("no content must be returned on a hash mismatch, got %q", got)
	}
}

// (c1) wrong-buyer at the recipient fast-fail: the wrap is addressed to the real
// buyer, but a different Signer tries to decrypt — the recipient guard fails fast.
func TestVerifyDeliverV2_MisroutedRecipient_FailsFast(t *testing.T) {
	fx := newV2Fixture(t, []byte("secret"))
	attacker, err := identity.Generate()
	if err != nil {
		t.Fatalf("generate attacker: %v", err)
	}
	// recipient labels the real buyer; the attacker (a different key) attempts it.
	deliverEv := fx.deliverEvent(fx.buyer.PubKeyHex(), fx.wrappedForBuyer, fx.ciphertextHash)
	conn := fx.fetchConn(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, _, err = verifyDeliver(ctx, conn, attacker, deliverEv, 0)
	if err == nil {
		t.Fatalf("expected a misrouted-recipient error, got nil")
	}
	if !strings.Contains(err.Error(), "different buyer") {
		t.Fatalf("error %q does not surface the misrouted-recipient guard", err)
	}
}

// (c2) wrong-buyer at the CRYPTO layer: even bypassing the recipient label guard
// (recipient omitted), a Signer that is not the wrap's true recipient CANNOT
// unwrap the CEK — the NIP-44 open fails. This is the §4.5.4 property: a captured
// deliver replayed toward a different buyer is undecryptable by them.
func TestVerifyDeliverV2_WrongKeyCannotUnwrap(t *testing.T) {
	fx := newV2Fixture(t, []byte("secret bytes only the paying buyer may read"))
	attacker, err := identity.Generate()
	if err != nil {
		t.Fatalf("generate attacker: %v", err)
	}
	// Omit the recipient label so the fast-fail guard does NOT fire — force the
	// crypto path. wrappedForBuyer is sealed to the real buyer, not the attacker.
	deliverEv := fx.deliverEvent("", fx.wrappedForBuyer, fx.ciphertextHash)
	conn := fx.fetchConn(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	got, _, err := verifyDeliver(ctx, conn, attacker, deliverEv, 0)
	if err == nil {
		t.Fatalf("expected a CEK-unwrap failure for the wrong buyer key, got nil (plaintext=%q)", got)
	}
	if !strings.Contains(err.Error(), "unwrap CEK") {
		t.Fatalf("error %q does not surface the CEK-unwrap failure", err)
	}
	if got != nil {
		t.Fatalf("no plaintext must be recovered by the wrong buyer key, got %q", got)
	}
	// Sanity: the RIGHT buyer, with the same deliver (recipient omitted), decrypts
	// fine — proving the failure above is the key binding, not a malformed fixture.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	ok, _, err := verifyDeliver(ctx2, fx.fetchConn(t), fx.buyer, deliverEv, 0)
	if err != nil {
		t.Fatalf("the real buyer must still decrypt the same deliver: %v", err)
	}
	if string(ok) != string(fx.plaintext) {
		t.Fatalf("real-buyer decrypt mismatch:\n got %q\nwant %q", ok, fx.plaintext)
	}
}

// (d) backward compat: a legacy individual-tier plaintext deliver (content field,
// no key_wrap) still decodes via the legacy path — conn/signer unused (nil-safe).
func TestVerifyDeliver_LegacyPlaintext_StillDecodes(t *testing.T) {
	body := []byte("individual-tier plaintext content, unchanged")
	raw := sha256.Sum256(body)
	hash := "sha256:" + hex.EncodeToString(raw[:])
	payload, _ := json.Marshal(map[string]any{
		"phase":        "deliver",
		"entry_id":     "entry-1",
		"content":      base64.StdEncoding.EncodeToString(body),
		"content_hash": hash,
	})
	ev := &identity.Event{ID: "legacy-deliver-wire", Content: string(payload)}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// nil conn + nil signer: the legacy path must never touch them.
	got, gotHash, err := verifyDeliver(ctx, nil, nil, ev, 0)
	if err != nil {
		t.Fatalf("legacy deliver decode: %v", err)
	}
	if string(got) != string(body) {
		t.Fatalf("legacy content = %q, want %q", got, body)
	}
	if gotHash != hash {
		t.Fatalf("legacy hash = %q, want %q", gotHash, hash)
	}
}
