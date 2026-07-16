package nostr

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/3dl-dev/dontguess/pkg/identity"
)

// mustGen makes a fresh secp256k1 identity or fails the test.
func mustGen(t *testing.T) *identity.Secp256k1Identity {
	t.Helper()
	id, err := identity.Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	return id
}

// TestInviteToken_RoundTrip: a token the operator mints parses back to the same
// fields with a VERIFIED operator signature (OperatorPubKey == the signer).
func TestInviteToken_RoundTrip(t *testing.T) {
	op := mustGen(t)
	relays := []string{"wss://relay.one", "wss://relay.two"}
	tok, err := BuildInviteToken(op, "alice", "grant-abc", relays, 50000, 1000, 2000)
	if err != nil {
		t.Fatalf("BuildInviteToken: %v", err)
	}
	if !strings.HasPrefix(tok, InviteTokenPrefix) {
		t.Fatalf("token missing %q prefix: %q", InviteTokenPrefix, tok)
	}
	in, err := ParseInviteToken(tok)
	if err != nil {
		t.Fatalf("ParseInviteToken: %v", err)
	}
	if in.OperatorPubKey != op.PubKeyHex() {
		t.Fatalf("OperatorPubKey = %s, want %s", in.OperatorPubKey, op.PubKeyHex())
	}
	if in.GrantID != "grant-abc" {
		t.Fatalf("GrantID = %q, want grant-abc", in.GrantID)
	}
	if in.GenesisScrip != 50000 {
		t.Fatalf("GenesisScrip = %d, want 50000", in.GenesisScrip)
	}
	if in.ExpiryUnix != 2000 {
		t.Fatalf("ExpiryUnix = %d, want 2000", in.ExpiryUnix)
	}
	if in.Name != "alice" {
		t.Fatalf("Name = %q, want alice", in.Name)
	}
	if len(in.RelayURLs) != 2 || in.RelayURLs[0] != relays[0] || in.RelayURLs[1] != relays[1] {
		t.Fatalf("RelayURLs = %v, want %v", in.RelayURLs, relays)
	}
	if in.Token() != tok {
		t.Fatalf("Token() did not round-trip")
	}
}

// TestInviteToken_TamperedRejected: mutating any signed field (here: inflating
// genesis scrip) after signing invalidates the id/signature — ParseInviteToken
// returns ErrForgedInvite. The token is not malleable.
func TestInviteToken_TamperedRejected(t *testing.T) {
	op := mustGen(t)
	tok, err := BuildInviteToken(op, "alice", "grant-abc", nil, 10, 1000, 2000)
	if err != nil {
		t.Fatalf("BuildInviteToken: %v", err)
	}
	// Decode the inner event, inflate the genesis scrip, re-encode WITHOUT re-signing.
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(tok, InviteTokenPrefix))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	var ev identity.Event
	if err := json.Unmarshal(raw, &ev); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for i, tag := range ev.Tags {
		if len(tag) >= 2 && tag[0] == inviteScripTag {
			ev.Tags[i][1] = "9999999" // greed
		}
	}
	tampered, _ := json.Marshal(&ev)
	tamperedTok := InviteTokenPrefix + base64.RawURLEncoding.EncodeToString(tampered)

	if _, err := ParseInviteToken(tamperedTok); !errors.Is(err, ErrForgedInvite) {
		t.Fatalf("ParseInviteToken(tampered) err = %v, want ErrForgedInvite", err)
	}
}

// TestInviteToken_NotAToken: garbage / wrong-prefix input is ErrNotInviteToken,
// distinct from a forged (decodable-but-bad-sig) token.
func TestInviteToken_NotAToken(t *testing.T) {
	for _, s := range []string{"", "hello", "npub1abc", "dgi1_!!!not-base64!!!"} {
		if _, err := ParseInviteToken(s); !errors.Is(err, ErrNotInviteToken) {
			t.Fatalf("ParseInviteToken(%q) err = %v, want ErrNotInviteToken", s, err)
		}
	}
}

// TestInviteToken_Expiry: Expired/CheckInviteFresh honor the absolute expiry, and a
// zero expiry never expires.
func TestInviteToken_Expiry(t *testing.T) {
	op := mustGen(t)
	tok, _ := BuildInviteToken(op, "alice", "g", nil, 0, 1000, 2000)
	in, err := ParseInviteToken(tok)
	if err != nil {
		t.Fatalf("ParseInviteToken: %v", err)
	}
	if in.Expired(1999) {
		t.Fatalf("not expired at 1999")
	}
	if !in.Expired(2001) {
		t.Fatalf("expired at 2001")
	}
	if err := CheckInviteFresh(in, 2001); !errors.Is(err, ErrInviteExpired) {
		t.Fatalf("CheckInviteFresh past expiry err = %v, want ErrInviteExpired", err)
	}
	if err := CheckInviteFresh(in, 1500); err != nil {
		t.Fatalf("CheckInviteFresh within window: %v", err)
	}

	// A never-expires token (expiry 0) is fresh at any time.
	tok0, _ := BuildInviteToken(op, "alice", "g", nil, 0, 1000, 0)
	in0, _ := ParseInviteToken(tok0)
	if in0.Expired(1 << 40) {
		t.Fatalf("never-expires token reported expired")
	}
}

// TestVerifyRedeem_Accept: a redeem the member signs over a valid, in-window,
// this-operator invite verifies and yields the member key + invite.
func TestVerifyRedeem_Accept(t *testing.T) {
	op := mustGen(t)
	member := mustGen(t)
	tok, _ := BuildInviteToken(op, "alice", "grant-xyz", nil, 777, 1000, 5000)
	in, err := ParseInviteToken(tok)
	if err != nil {
		t.Fatalf("ParseInviteToken: %v", err)
	}
	redeemEv, err := BuildRedeemEvent(member, in, 1500)
	if err != nil {
		t.Fatalf("BuildRedeemEvent: %v", err)
	}
	wireEv := toWireEvent(redeemEv)

	r, err := VerifyRedeem(wireEv, op.PubKeyHex(), 1500)
	if err != nil {
		t.Fatalf("VerifyRedeem: %v", err)
	}
	if r.MemberHexKey != member.PubKeyHex() {
		t.Fatalf("MemberHexKey = %s, want %s", r.MemberHexKey, member.PubKeyHex())
	}
	if r.Invite.GrantID != "grant-xyz" || r.Invite.GenesisScrip != 777 {
		t.Fatalf("invite fields wrong: %+v", r.Invite)
	}
}

// TestVerifyRedeem_ForeignOperatorRejected: a redeem embedding an invite signed by
// a DIFFERENT operator key (the PIN mismatch) is rejected with ErrForgedInvite —
// even though the invite is validly self-signed and the member's own signature is
// valid. This is the two-operator confusion / stolen-token-from-another-fleet case.
func TestVerifyRedeem_ForeignOperatorRejected(t *testing.T) {
	realOp := mustGen(t)
	foreignOp := mustGen(t)
	member := mustGen(t)

	tok, _ := BuildInviteToken(foreignOp, "alice", "g", nil, 5, 1000, 5000)
	in, _ := ParseInviteToken(tok)
	redeemEv, _ := BuildRedeemEvent(member, in, 1500)

	if _, err := VerifyRedeem(toWireEvent(redeemEv), realOp.PubKeyHex(), 1500); !errors.Is(err, ErrForgedInvite) {
		t.Fatalf("VerifyRedeem(foreign operator) err = %v, want ErrForgedInvite", err)
	}
}

// TestVerifyRedeem_ExpiredRejected: an in-all-other-ways-valid redeem of an expired
// invite is rejected with ErrInviteExpired.
func TestVerifyRedeem_ExpiredRejected(t *testing.T) {
	op := mustGen(t)
	member := mustGen(t)
	tok, _ := BuildInviteToken(op, "alice", "g", nil, 5, 1000, 2000)
	in, _ := ParseInviteToken(tok)
	redeemEv, _ := BuildRedeemEvent(member, in, 3000)

	if _, err := VerifyRedeem(toWireEvent(redeemEv), op.PubKeyHex(), 3000); !errors.Is(err, ErrInviteExpired) {
		t.Fatalf("VerifyRedeem(expired) err = %v, want ErrInviteExpired", err)
	}
}

// TestVerifyRedeem_ForgedMemberSigRejected: a redeem whose member signature does
// not verify (content tampered after signing) is rejected — the npub-bind is only
// as good as the member's own signature over the event.
func TestVerifyRedeem_ForgedMemberSigRejected(t *testing.T) {
	op := mustGen(t)
	member := mustGen(t)
	tok, _ := BuildInviteToken(op, "alice", "g", nil, 5, 1000, 5000)
	in, _ := ParseInviteToken(tok)
	redeemEv, _ := BuildRedeemEvent(member, in, 1500)
	wireEv := toWireEvent(redeemEv)
	// Tamper: swap the author pubkey to a third key without re-signing.
	imposter := mustGen(t)
	wireEv.PubKey = imposter.PubKeyHex()

	if _, err := VerifyRedeem(wireEv, op.PubKeyHex(), 1500); err == nil {
		t.Fatalf("VerifyRedeem accepted a redeem with a forged member pubkey")
	}
}

// TestVerifyRedeem_SplicedInviteIDRejected: a redeem whose ["invite-id"] tag does
// not match the grant id inside the embedded (correctly operator-signed) token is
// rejected — a member cannot advertise one grant while redeeming another.
func TestVerifyRedeem_SplicedInviteIDRejected(t *testing.T) {
	op := mustGen(t)
	member := mustGen(t)
	tok, _ := BuildInviteToken(op, "alice", "real-grant", nil, 5, 1000, 5000)
	in, _ := ParseInviteToken(tok)
	redeemEv, _ := BuildRedeemEvent(member, in, 1500)
	// Re-sign with a mismatched invite-id tag.
	wireEv := toWireEvent(redeemEv)
	wireEv.Tags = [][]string{{inviteIDTag, "some-other-grant"}}
	idEv := &identity.Event{CreatedAt: wireEv.CreatedAt, Kind: wireEv.Kind, Tags: wireEv.Tags, Content: wireEv.Content}
	if err := identity.SignEvent(member, idEv); err != nil {
		t.Fatalf("re-sign: %v", err)
	}
	spliced := toWireEvent(idEv)

	if _, err := VerifyRedeem(spliced, op.PubKeyHex(), 1500); !errors.Is(err, ErrForgedInvite) {
		t.Fatalf("VerifyRedeem(spliced invite-id) err = %v, want ErrForgedInvite", err)
	}
}

// toWireEvent maps a signed identity.Event to the structurally-identical wire
// nostr.Event the operator reader receives.
func toWireEvent(ev *identity.Event) *Event {
	return &Event{
		ID:        ev.ID,
		PubKey:    ev.PubKey,
		CreatedAt: ev.CreatedAt,
		Kind:      ev.Kind,
		Tags:      ev.Tags,
		Content:   ev.Content,
		Sig:       ev.Sig,
	}
}
