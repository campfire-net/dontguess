package nostr

// roster.go — the operator-signed FLEET ROSTER, the SINGLE source of truth for
// fleet-member admission (design docs/design/onboarding-tiered-scaling-federation.md
// §2, P5). Admitting/removing a fleet member is ONE operator action: an
// operator-signed parameterized-replaceable roster event that is decoded into TWO
// projections of ONE signed event —
//
//	  - the EXCHANGE KeySet   (THIS repo — folded from the event log; the fine,
//	                           "single enforcement point" for which operation at
//	                           what reputation floor), and
//	  - the relay writePolicy (out-of-repo strfry, dontguess-ef1 — the coarse
//	                           "first line" write-admission; OPTIONAL edge-hardening,
//	                           never a prerequisite).
//
// The two gates enforce DIFFERENT properties and must NEVER collapse into "relay
// write == exchange trust" (ADV-2). The exchange fold below re-verifies the
// operator signature on the roster ITSELF — it never trusts that a relay gated the
// write — so a dumb relay that forwarded a forged roster changes exactly nothing.
// This is the roster analogue of VerifyOperatorAuthorship's Q3 CLIENT-SIDE
// RE-VERIFY ruling.
//
// The roster is deliberately NOT part of the exchange message adapter
// (put/buy/match/settle): it is not a folded proto.Message. FromNostrEvent rejects
// it, and the relay reader routes it to the KeySet projection directly (a separate
// fold), keeping the two projections independent by construction.

import (
	"errors"
	"fmt"
	"strings"

	"github.com/3dl-dev/dontguess/pkg/identity"
)

// KindFleetRoster is the nostr kind of the operator-signed fleet roster. It is a
// parameterized-replaceable event (the 30000-39999 range): the relay keeps only
// the LATEST roster per (author, d-tag), so republishing supersedes the previous
// roster and a fresh operator subscribe backfills exactly one current roster.
// Confirmed kind + d-tag: design §2 / Q6 (operator 2026-07-15).
const KindFleetRoster = 30078

// FleetRosterDTag is the ["d", …] value identifying the fleet roster among an
// operator's parameterized-replaceable events.
const FleetRosterDTag = "fleet"

// dTagName is the NIP-01 addressable-event discriminator tag name.
const dTagName = "d"

// ErrNotFleetRoster is returned when an event is not a fleet roster at all (wrong
// kind, or missing/mismatched d-tag). It is distinguishable from ErrForgedFleetRoster
// so a caller can tell "this isn't a roster" apart from "this is a FORGED roster".
var ErrNotFleetRoster = errors.New("nostr: not a fleet roster event")

// ErrForgedFleetRoster is returned when an event CLAIMS to be a fleet roster (right
// kind + d-tag) but is NOT authored by the pinned operator key, or its BIP-340
// signature / id integrity does not verify. It is the roster analogue of
// ErrForgedOperatorEvent — a LOUD, errors.Is-matchable sentinel so the fold counts
// and alarms rather than silently dropping (LOCKED-5).
var ErrForgedFleetRoster = errors.New("nostr: forged fleet roster event")

// FleetRosterTags builds the tag set for a fleet roster admitting memberHexKeys:
// one ["d","fleet"] discriminator followed by one ["p", <hex>] per admitted member
// (lowercased, blanks skipped, de-duplicated preserving first-seen order). The
// caller sets Kind (KindFleetRoster), CreatedAt, and PubKey and signs via
// identity.SignEvent. The member list is AUTHORITATIVE FULL MEMBERSHIP — the roster
// is applied with replace semantics (latest-wins), never as an incremental delta —
// so an admit publishes the full new set and a removal publishes the full set minus
// the removed key.
func FleetRosterTags(memberHexKeys []string) [][]string {
	tags := make([][]string, 0, len(memberHexKeys)+1)
	tags = append(tags, []string{dTagName, FleetRosterDTag})
	seen := make(map[string]struct{}, len(memberHexKeys))
	for _, k := range memberHexKeys {
		k = strings.ToLower(strings.TrimSpace(k))
		if k == "" {
			continue
		}
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		tags = append(tags, []string{tagP, k})
	}
	return tags
}

// ParseFleetRoster verifies ev is an operator-signed fleet roster and returns the
// admitted member hex keys (lowercased). It enforces, IN ORDER:
//
//	(1) kind == KindFleetRoster AND the PRIMARY d-tag == "fleet" → ErrNotFleetRoster
//	(2) author pubkey == operatorKey                     → ErrForgedFleetRoster
//	(3) the BIP-340 signature + id integrity verify      → ErrForgedFleetRoster
//
// Steps 2-3 mirror VerifyOperatorAuthorship: a forged roster (right kind + d-tag,
// wrong author, OR a validly-signed body under a NON-operator key) returns an error
// and the caller changes NOTHING. It is CLIENT-SIDE RE-VERIFY ONLY (Q3, ADV-2): a
// dumb/compromised relay that forwarded a forged roster is fully defended because
// the fold re-derives operator authorship from the signed event itself and never
// relies on any relay write policy.
//
// operatorKey may be npub or hex form. A roster that admits zero members (bare
// ["d","fleet"], no p-tags) is VALID and returns an empty slice — it is how an
// operator de-admits the last remaining member.
func ParseFleetRoster(ev *Event, operatorKey string) ([]string, error) {
	if ev == nil {
		return nil, fmt.Errorf("nostr: ParseFleetRoster: nil event")
	}
	if ev.Kind != KindFleetRoster || !firstDTagIs(ev.Tags, FleetRosterDTag) {
		return nil, fmt.Errorf("%w: kind=%d", ErrNotFleetRoster, ev.Kind)
	}
	opHex, err := normalizeOperatorKey(operatorKey)
	if err != nil {
		return nil, fmt.Errorf("nostr: ParseFleetRoster: %w", err)
	}

	// (2) Author pubkey must be the operator. Reject a wrong-author roster before
	// spending a signature verification on it.
	if !strings.EqualFold(strings.TrimSpace(ev.PubKey), opHex) {
		return nil, fmt.Errorf("%w: pubkey %s != operator %s",
			ErrForgedFleetRoster, shortKey(ev.PubKey), shortKey(opHex))
	}

	// (3) The Schnorr signature (and the id-vs-content integrity check inside
	// VerifyEvent) must verify against the operator pubkey. This is what makes a
	// stolen/forged pubkey field insufficient: only the operator's private key can
	// produce a signature that verifies here.
	if err := identity.VerifyEvent(toIdentityEvent(ev)); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrForgedFleetRoster, err)
	}

	var members []string
	seen := make(map[string]struct{})
	for _, t := range ev.Tags {
		if len(t) >= 2 && t[0] == tagP {
			k := strings.ToLower(strings.TrimSpace(t[1]))
			if k == "" {
				continue
			}
			if _, dup := seen[k]; dup {
				continue
			}
			seen[k] = struct{}{}
			members = append(members, k)
		}
	}
	return members, nil
}

// firstDTagIs reports whether the NIP-01-authoritative FIRST ["d", …] tag has the
// value want. NIP-01 keys a parameterized-replaceable event by its FIRST d-tag ONLY
// (§ addressable events): the coordinate is (kind, pubkey, first-d-value). So a fleet
// roster is identified by its PRIMARY d-tag being "fleet" — an event whose primary
// d-tag is something else but which carries an INCIDENTAL secondary ["d","fleet"]
// later in the tag list is NOT a fleet roster (d-tag-confusion, dontguess-61a8). The
// scan returns at the first d-tag regardless of match, so a non-fleet primary d-tag
// short-circuits to false and a later ["d","fleet"] is never consulted. A bare
// ["d"] first tag (no value) resolves to the empty d-value "" per NIP-01, ≠ "fleet".
// An event with no d-tag at all is not a match.
func firstDTagIs(tags [][]string, want string) bool {
	for _, t := range tags {
		if len(t) >= 1 && t[0] == dTagName {
			val := ""
			if len(t) >= 2 {
				val = t[1]
			}
			return val == want
		}
	}
	return false
}
