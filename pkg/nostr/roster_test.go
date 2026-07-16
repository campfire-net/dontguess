package nostr

import (
	"errors"
	"testing"
	"time"

	"github.com/3dl-dev/dontguess/pkg/identity"
)

// signRoster builds an operator-signed fleet roster admitting members, then maps
// it to the wire *Event ParseFleetRoster consumes. A genuinely Schnorr-signed
// event so the verify path is exercised for real, not stubbed.
func signRoster(t *testing.T, signer *identity.Secp256k1Identity, createdAt int64, members []string) *Event {
	t.Helper()
	iev := &identity.Event{
		CreatedAt: createdAt,
		Kind:      KindFleetRoster,
		Tags:      FleetRosterTags(members),
		Content:   "",
	}
	if err := identity.SignEvent(signer, iev); err != nil {
		t.Fatalf("SignEvent: %v", err)
	}
	return &Event{
		ID:        iev.ID,
		PubKey:    iev.PubKey,
		CreatedAt: iev.CreatedAt,
		Kind:      iev.Kind,
		Tags:      iev.Tags,
		Content:   iev.Content,
		Sig:       iev.Sig,
	}
}

// TestParseFleetRoster_ValidOperatorSigned proves a genuine operator-signed roster
// returns exactly its admitted members (lowercased), the happy path the exchange
// fold depends on.
func TestParseFleetRoster_ValidOperatorSigned(t *testing.T) {
	op, _ := identity.Generate()
	m1, _ := identity.Generate()
	m2, _ := identity.Generate()

	ev := signRoster(t, op, time.Now().Unix(), []string{m1.PubKeyHex(), m2.PubKeyHex()})
	members, err := ParseFleetRoster(ev, op.PubKeyHex())
	if err != nil {
		t.Fatalf("ParseFleetRoster valid: %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("members = %v, want 2", members)
	}
	got := map[string]bool{members[0]: true, members[1]: true}
	if !got[m1.PubKeyHex()] || !got[m2.PubKeyHex()] {
		t.Fatalf("members = %v, want {%s,%s}", members, m1.PubKeyHex(), m2.PubKeyHex())
	}
}

// TestParseFleetRoster_EmptyMembers proves a bare roster (["d","fleet"], no
// p-tags) is VALID and yields zero members — the last-member-removal path.
func TestParseFleetRoster_EmptyMembers(t *testing.T) {
	op, _ := identity.Generate()
	ev := signRoster(t, op, time.Now().Unix(), nil)
	members, err := ParseFleetRoster(ev, op.PubKeyHex())
	if err != nil {
		t.Fatalf("ParseFleetRoster empty: %v", err)
	}
	if len(members) != 0 {
		t.Fatalf("members = %v, want empty", members)
	}
}

// TestParseFleetRoster_ForgedAuthor proves a roster authored+signed by a
// NON-operator key (the "forged roster" the ground-source assertion 3 injects) is
// rejected with ErrForgedFleetRoster and mutates nothing. The event is validly
// self-signed by the attacker — the ONLY thing wrong is the author is not the
// pinned operator, exactly the relay-forwarding-a-forgery threat.
func TestParseFleetRoster_ForgedAuthor(t *testing.T) {
	op, _ := identity.Generate()
	attacker, _ := identity.Generate()
	victim, _ := identity.Generate()

	// Attacker self-signs a well-formed roster admitting themselves.
	ev := signRoster(t, attacker, time.Now().Unix(), []string{victim.PubKeyHex()})
	_, err := ParseFleetRoster(ev, op.PubKeyHex())
	if !errors.Is(err, ErrForgedFleetRoster) {
		t.Fatalf("forged-author roster err = %v, want ErrForgedFleetRoster", err)
	}
}

// TestParseFleetRoster_TamperedBody proves a roster whose pubkey field is swapped
// to the operator's (claiming operator authorship) but whose signature does NOT
// verify under that key is rejected with ErrForgedFleetRoster — a stolen pubkey
// field is insufficient.
func TestParseFleetRoster_TamperedBody(t *testing.T) {
	op, _ := identity.Generate()
	attacker, _ := identity.Generate()
	member, _ := identity.Generate()

	ev := signRoster(t, attacker, time.Now().Unix(), []string{member.PubKeyHex()})
	// Claim operator authorship by swapping the pubkey; the attacker's signature no
	// longer verifies under the operator key (id/sig were computed for the attacker).
	ev.PubKey = op.PubKeyHex()
	_, err := ParseFleetRoster(ev, op.PubKeyHex())
	if !errors.Is(err, ErrForgedFleetRoster) {
		t.Fatalf("tampered-body roster err = %v, want ErrForgedFleetRoster", err)
	}
}

// TestParseFleetRoster_WrongKind proves an event of a non-roster kind (even one
// validly operator-signed) is ErrNotFleetRoster, distinguishable from a forgery.
func TestParseFleetRoster_WrongKind(t *testing.T) {
	op, _ := identity.Generate()
	iev := &identity.Event{
		CreatedAt: time.Now().Unix(),
		Kind:      KindPut, // not a roster
		Tags:      FleetRosterTags(nil),
		Content:   "",
	}
	if err := identity.SignEvent(op, iev); err != nil {
		t.Fatalf("SignEvent: %v", err)
	}
	ev := &Event{ID: iev.ID, PubKey: iev.PubKey, CreatedAt: iev.CreatedAt, Kind: iev.Kind, Tags: iev.Tags, Content: iev.Content, Sig: iev.Sig}
	_, err := ParseFleetRoster(ev, op.PubKeyHex())
	if !errors.Is(err, ErrNotFleetRoster) {
		t.Fatalf("wrong-kind err = %v, want ErrNotFleetRoster", err)
	}
}

// TestParseFleetRoster_IncidentalSecondaryDTag is the dontguess-61a8 ground-source
// for the first-d-tag rule: a kind-30078 event whose PRIMARY (first) d-tag is NOT
// "fleet" but which carries an INCIDENTAL secondary ["d","fleet"] later in the tag
// list is NOT a fleet roster — NIP-01 keys a parameterized-replaceable event by its
// FIRST d-tag only, so this is a DIFFERENT coordinate (a generic NIP-78 app-data
// event that merely happens to also carry a fleet d-tag). The pre-fix hasDTag matched
// ["d","fleet"] ANYWHERE and would have mis-parsed this as a roster; firstDTagIs
// short-circuits at the non-fleet primary d-tag and returns ErrNotFleetRoster. The
// event is validly operator-signed — the ONLY thing that disqualifies it is the
// primary d-tag, exactly the d-tag-confusion authority hazard.
func TestParseFleetRoster_IncidentalSecondaryDTag(t *testing.T) {
	op, _ := identity.Generate()
	member, _ := identity.Generate()

	iev := &identity.Event{
		CreatedAt: time.Now().Unix(),
		Kind:      KindFleetRoster,
		Tags: [][]string{
			{"d", "app-settings"}, // PRIMARY (first) d-tag — NOT "fleet"
			{"d", FleetRosterDTag}, // incidental SECONDARY d-tag NIP-01 never keys by
			{"p", member.PubKeyHex()},
		},
		Content: "",
	}
	if err := identity.SignEvent(op, iev); err != nil {
		t.Fatalf("SignEvent: %v", err)
	}
	ev := &Event{ID: iev.ID, PubKey: iev.PubKey, CreatedAt: iev.CreatedAt, Kind: iev.Kind, Tags: iev.Tags, Content: iev.Content, Sig: iev.Sig}
	if _, err := ParseFleetRoster(ev, op.PubKeyHex()); !errors.Is(err, ErrNotFleetRoster) {
		t.Fatalf("incidental-secondary-d-tag roster err = %v, want ErrNotFleetRoster (the first d-tag %q is authoritative, not the later fleet one)", err, "app-settings")
	}
}

// TestParseFleetRoster_MissingDTag proves a kind-30078 event WITHOUT the
// ["d","fleet"] discriminator is not treated as the fleet roster (it is some other
// operator addressable event) — ErrNotFleetRoster.
func TestParseFleetRoster_MissingDTag(t *testing.T) {
	op, _ := identity.Generate()
	iev := &identity.Event{
		CreatedAt: time.Now().Unix(),
		Kind:      KindFleetRoster,
		Tags:      [][]string{{"p", op.PubKeyHex()}}, // no d-tag
		Content:   "",
	}
	if err := identity.SignEvent(op, iev); err != nil {
		t.Fatalf("SignEvent: %v", err)
	}
	ev := &Event{ID: iev.ID, PubKey: iev.PubKey, CreatedAt: iev.CreatedAt, Kind: iev.Kind, Tags: iev.Tags, Content: iev.Content, Sig: iev.Sig}
	_, err := ParseFleetRoster(ev, op.PubKeyHex())
	if !errors.Is(err, ErrNotFleetRoster) {
		t.Fatalf("missing-d-tag err = %v, want ErrNotFleetRoster", err)
	}
}
