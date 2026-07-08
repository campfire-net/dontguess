package identity

import "testing"

// TestAllowlist_AcceptReject proves an allowlist admits members (by npub or hex)
// and rejects everyone else, case-insensitively on the hex.
func TestAllowlist_AcceptReject(t *testing.T) {
	t.Parallel()

	member, _ := Generate()
	stranger, _ := Generate()

	// Admit the member by its npub; admit a second identity by raw hex.
	other, _ := Generate()
	al, err := NewAllowlist(member.Npub(), other.PubKeyHex(), "  ", "")
	if err != nil {
		t.Fatalf("NewAllowlist: %v", err)
	}
	if al.Len() != 2 {
		t.Fatalf("allowlist Len = %d, want 2 (blank entries ignored)", al.Len())
	}

	if !al.Allowed(member.PubKeyHex()) {
		t.Error("member admitted by npub was not Allowed by hex")
	}
	if !al.Allowed(other.PubKeyHex()) {
		t.Error("member admitted by hex was not Allowed")
	}
	if al.Allowed(stranger.PubKeyHex()) {
		t.Error("stranger not on the allowlist was Allowed (fail-open)")
	}
}

// TestAllowlist_RejectsMalformed proves a malformed entry is a hard error, never
// silently dropped (a dropped entry is a fail-open security hole).
func TestAllowlist_RejectsMalformed(t *testing.T) {
	t.Parallel()

	for _, bad := range []string{
		"npub1garbage",               // bad bech32
		"not-hex-not-npub",           // neither form
		"abc123",                     // too-short hex
		"nsec1" + "0000000000000000", // wrong HRP masquerading
	} {
		if _, err := NewAllowlist(bad); err == nil {
			t.Errorf("NewAllowlist accepted malformed entry %q", bad)
		}
	}
}

// TestOpenAllowlist_AdmitsEveryPubkey proves OpenAllowlist().Allowed reports
// true unconditionally, including for a pubkey nobody ever admitted — this is
// the explicit, named opt-out from allowlist enforcement that RelayAuthenticate
// requires callers to pass instead of a bare nil.
func TestOpenAllowlist_AdmitsEveryPubkey(t *testing.T) {
	t.Parallel()

	al := OpenAllowlist()
	if al.Len() != 0 {
		t.Fatalf("OpenAllowlist Len = %d, want 0 (no explicit members)", al.Len())
	}

	stranger, _ := Generate()
	if !al.Allowed(stranger.PubKeyHex()) {
		t.Error("OpenAllowlist rejected a pubkey; want unconditional admission")
	}
	if !al.Allowed("not-even-a-valid-pubkey") {
		t.Error("OpenAllowlist rejected a garbage string; want unconditional admission")
	}
}
