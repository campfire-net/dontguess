package nostr

// invite.go — the self-service onboarding primitive (design
// docs/design/onboarding-tiered-scaling-federation.md §1 + §9 Gate B/P8, ADV-15).
//
// `dontguess invite <name>` mints an OPERATOR-SIGNED, scoped, single-use, TTL'd,
// npub-bound-on-redeem token (the "dgi1_" blob). `dontguess join <token>` decodes
// it, verifies the operator signature (+ not expired), self-provisions a fresh
// member key, and publishes a REDEEM event (kind 3410) — signed by that fresh
// member key — that EMBEDS the whole invite token. The operator's serve reader
// re-verifies (operator sig valid, not expired, grant-id not already redeemed) and
// promotes the member into the fleet, minting the optional genesis grant.
//
// TWO enforcement points (ADV-15, same defense-in-depth as the roster §2):
//
//	(1) the RELAY writePolicy MAY drop an un-allowlisted publish that is not a
//	    kind-3410 carrying a valid operator-signed invite (OPTIONAL, closed-relay
//	    edge hardening — dontguess-ef1, out of this repo); and
//	(2) the OPERATOR serve reader does 100% of the authoritative verification here,
//	    against ANY open relay — it NEVER trusts that a relay gated the write (ADV-2,
//	    the roster/VerifyOperatorAuthorship rule). A dumb/open relay that forwards a
//	    forged or expired or replayed redeem changes the fleet by EXACTLY NOTHING.
//
// The token is deliberately NOT a reusable bearer credential: it carries a
// one-time grant id the operator persists as redeemed on first successful redeem,
// and the redeem BINDS the member's fresh npub (the redeem event's author). A
// second redeem of the same grant id — even after an operator restart — is
// rejected (the redeemed-id set is persisted to disk).

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/3dl-dev/dontguess/pkg/identity"
)

// InviteTokenPrefix is the human-visible prefix of an invite token, mirroring the
// nostr bech32 "npub1"/"note1" convention so a pasted token is self-identifying.
const InviteTokenPrefix = "dgi1_"

// InviteTokenKind is the nostr kind of the operator-signed invite TOKEN event. It
// is a LOCAL-ONLY kind: the token event is base64'd into the dgi1_ blob and
// EMBEDDED in a kind-3410 redeem's content — it is NEVER published to a relay as a
// standalone event. It sits outside the exchange kind range (3401–3411, 30401) and
// is distinct from the IPC auth kinds (mintAuthKind 27411, allowlistAuthKind 27412)
// so a signed event of another kind can never be replayed as an invite token.
const InviteTokenKind = 27413

// Invite tag names carried in the operator-signed token event.
const (
	inviteIDTag     = "invite-id"     // the one-time admission grant id
	inviteRelayTag  = "relay"         // a relay URL (repeatable)
	inviteScripTag  = "genesis-scrip" // optional genesis scrip amount (decimal)
	inviteExpiryTag = "expiry"        // absolute expiry, unix seconds
	inviteNameTag   = "name"          // member name hint for agent-init --fleet-member
)

// ErrNotInviteToken is returned when a string is not a dgi1_ invite token at all
// (missing prefix, or not decodable). Distinguishable from ErrForgedInvite so a
// caller can tell "this isn't a token" from "this token is forged".
var ErrNotInviteToken = errors.New("nostr: not a dgi1_ invite token")

// ErrForgedInvite is returned when a token DECODES to an invite event but its
// operator signature / id integrity does not verify, or it is not the invite kind.
// It is the invite analogue of ErrForgedOperatorEvent — a LOUD, errors.Is-matchable
// sentinel so a caller counts/alarms rather than silently dropping.
var ErrForgedInvite = errors.New("nostr: forged invite token")

// ErrInviteExpired is returned by CheckInviteFresh when the token's absolute expiry
// has passed. Kept distinct so join can print "expired" rather than "forged".
var ErrInviteExpired = errors.New("nostr: invite token expired")

// Invite is the decoded, signature-verified contents of an invite token. The
// signature is ALREADY verified by ParseInviteToken (a returned *Invite is
// operator-authored by construction); the remaining checks — operator-key PIN,
// expiry, one-time redemption — are the caller's policy, applied by the operator's
// redeem handler.
type Invite struct {
	// OperatorPubKey is the token author's hex pubkey. It is the operator npub PIN:
	// the operator's redeem handler admits ONLY tokens whose OperatorPubKey equals
	// its own persisted operator key, so a token signed by any other key is foreign.
	OperatorPubKey string
	// GrantID is the one-time admission grant id. The operator persists it as
	// redeemed on first successful redeem; a second redeem of the same id is
	// rejected (not a reusable bearer credential).
	GrantID string
	// RelayURLs are the relay(s) join publishes the redeem event to (and the member
	// then talks to). May be empty (join can be pointed at a relay explicitly).
	RelayURLs []string
	// GenesisScrip is the optional starting grant minted to the member on redeem. 0
	// means no genesis grant.
	GenesisScrip int64
	// ExpiryUnix is the absolute expiry (unix seconds). 0 means no expiry (an invite
	// minted with --ttl always sets one; 0 is only the never-expires escape hatch).
	ExpiryUnix int64
	// Name is the member name hint join feeds agent-init --fleet-member.
	Name string
	// token is the exact dgi1_ string this Invite decoded from, so a redeem can
	// re-embed the token verbatim without a re-encode round-trip.
	token string
}

// Token returns the exact dgi1_ string this Invite decoded from (or was built as).
func (in *Invite) Token() string { return in.token }

// Expired reports whether the invite's absolute expiry has passed at nowUnix. A
// zero ExpiryUnix (never-expires) is never expired.
func (in *Invite) Expired(nowUnix int64) bool {
	return in.ExpiryUnix != 0 && nowUnix > in.ExpiryUnix
}

// BuildInviteToken mints an operator-signed invite token. signer MUST be the
// operator identity (its pubkey becomes the PIN the redeem handler checks against
// its own operator key). createdAt/expiryUnix are unix seconds (expiryUnix 0 =
// never expires). grantID is the one-time admission id (caller supplies a random,
// unguessable value). Returns the "dgi1_"-prefixed base64url token.
func BuildInviteToken(signer identity.Signer, name, grantID string, relayURLs []string, genesisScrip, createdAt, expiryUnix int64) (string, error) {
	grantID = strings.TrimSpace(grantID)
	if grantID == "" {
		return "", fmt.Errorf("nostr: BuildInviteToken: empty grant id")
	}
	if genesisScrip < 0 {
		return "", fmt.Errorf("nostr: BuildInviteToken: negative genesis scrip %d", genesisScrip)
	}
	tags := [][]string{{inviteIDTag, grantID}}
	for _, u := range relayURLs {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		tags = append(tags, []string{inviteRelayTag, u})
	}
	if genesisScrip > 0 {
		tags = append(tags, []string{inviteScripTag, strconv.FormatInt(genesisScrip, 10)})
	}
	tags = append(tags, []string{inviteExpiryTag, strconv.FormatInt(expiryUnix, 10)})
	if n := strings.TrimSpace(name); n != "" {
		tags = append(tags, []string{inviteNameTag, n})
	}

	ev := &identity.Event{
		CreatedAt: createdAt,
		Kind:      InviteTokenKind,
		Tags:      tags,
		Content:   "",
	}
	if err := identity.SignEvent(signer, ev); err != nil {
		return "", fmt.Errorf("nostr: BuildInviteToken: sign: %w", err)
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		return "", fmt.Errorf("nostr: BuildInviteToken: marshal: %w", err)
	}
	return InviteTokenPrefix + base64.RawURLEncoding.EncodeToString(raw), nil
}

// ParseInviteToken decodes token, VERIFIES the operator signature + id integrity
// (identity.VerifyEvent — a real BIP-340 Schnorr check), confirms it is the invite
// kind, and extracts the fields. A returned *Invite is operator-authored BY THE KEY
// IN OperatorPubKey by construction; the caller still PINS that against its own
// operator key. A missing prefix / undecodable blob → ErrNotInviteToken; a decodable
// blob whose signature does not verify, or a wrong-kind event → ErrForgedInvite.
//
// It does NOT check expiry or the operator-key pin — those are caller policy
// (CheckInviteFresh + the redeem handler's pin check) so a forged-vs-expired-vs-
// foreign token can be reported distinctly.
func ParseInviteToken(token string) (*Invite, error) {
	token = strings.TrimSpace(token)
	if !strings.HasPrefix(token, InviteTokenPrefix) {
		return nil, fmt.Errorf("%w: missing %q prefix", ErrNotInviteToken, InviteTokenPrefix)
	}
	blob := strings.TrimPrefix(token, InviteTokenPrefix)
	raw, err := base64.RawURLEncoding.DecodeString(blob)
	if err != nil {
		return nil, fmt.Errorf("%w: base64: %v", ErrNotInviteToken, err)
	}
	var ev identity.Event
	if err := json.Unmarshal(raw, &ev); err != nil {
		return nil, fmt.Errorf("%w: json: %v", ErrNotInviteToken, err)
	}
	if ev.Kind != InviteTokenKind {
		return nil, fmt.Errorf("%w: kind=%d (want %d)", ErrForgedInvite, ev.Kind, InviteTokenKind)
	}
	// REAL Schnorr verify: recompute the id from the content and check the signature
	// against ev.PubKey. A tampered field (added scrip, pushed-out expiry, swapped
	// grant id) changes the id and fails here — the token is not malleable.
	if err := identity.VerifyEvent(&ev); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrForgedInvite, err)
	}

	in := &Invite{OperatorPubKey: strings.ToLower(strings.TrimSpace(ev.PubKey)), token: token}
	haveExpiry := false
	for _, t := range ev.Tags {
		if len(t) < 2 {
			continue
		}
		switch t[0] {
		case inviteIDTag:
			in.GrantID = strings.TrimSpace(t[1])
		case inviteRelayTag:
			if u := strings.TrimSpace(t[1]); u != "" {
				in.RelayURLs = append(in.RelayURLs, u)
			}
		case inviteScripTag:
			n, perr := strconv.ParseInt(strings.TrimSpace(t[1]), 10, 64)
			if perr != nil || n < 0 {
				return nil, fmt.Errorf("%w: bad genesis-scrip %q", ErrForgedInvite, t[1])
			}
			in.GenesisScrip = n
		case inviteExpiryTag:
			n, perr := strconv.ParseInt(strings.TrimSpace(t[1]), 10, 64)
			if perr != nil || n < 0 {
				return nil, fmt.Errorf("%w: bad expiry %q", ErrForgedInvite, t[1])
			}
			in.ExpiryUnix = n
			haveExpiry = true
		case inviteNameTag:
			in.Name = strings.TrimSpace(t[1])
		}
	}
	if in.GrantID == "" {
		return nil, fmt.Errorf("%w: missing invite-id", ErrForgedInvite)
	}
	if !haveExpiry {
		return nil, fmt.Errorf("%w: missing expiry", ErrForgedInvite)
	}
	return in, nil
}

// CheckInviteFresh returns ErrInviteExpired when the invite has expired at nowUnix,
// else nil. Split out so join and the operator handler share one freshness check.
func CheckInviteFresh(in *Invite, nowUnix int64) error {
	if in == nil {
		return fmt.Errorf("nostr: CheckInviteFresh: nil invite")
	}
	if in.Expired(nowUnix) {
		return fmt.Errorf("%w: expiry %d < now %d", ErrInviteExpired, in.ExpiryUnix, nowUnix)
	}
	return nil
}

// BuildRedeemEvent constructs the kind-3410 REDEEM event a joining member publishes:
// signed by the member's FRESH key (memberSigner — its pubkey becomes the npub the
// operator binds to the grant), carrying an ["invite-id", grantID] tag and the FULL
// dgi1_ token in Content so the operator can re-verify the operator signature on the
// embedded invite. createdAt is unix seconds.
func BuildRedeemEvent(memberSigner identity.Signer, in *Invite, createdAt int64) (*identity.Event, error) {
	if in == nil || in.token == "" {
		return nil, fmt.Errorf("nostr: BuildRedeemEvent: nil/empty invite")
	}
	ev := &identity.Event{
		CreatedAt: createdAt,
		Kind:      KindInvite,
		Tags:      [][]string{{inviteIDTag, in.GrantID}},
		Content:   in.token,
	}
	if err := identity.SignEvent(memberSigner, ev); err != nil {
		return nil, fmt.Errorf("nostr: BuildRedeemEvent: sign: %w", err)
	}
	return ev, nil
}

// Redeem is the operator-side, fully-verified result of a kind-3410 redeem event.
type Redeem struct {
	// MemberHexKey is the redeem author's hex pubkey — the fresh member key the
	// operator promotes into the fleet and binds to the grant. It is
	// cryptographically bound: VerifyRedeem re-verified the member's signature over
	// the redeem event, so this pubkey provably possesses the member private key.
	MemberHexKey string
	// Invite is the decoded, operator-signature-verified, PIN-checked, unexpired
	// invite the redeem embedded.
	Invite *Invite
}

// VerifyRedeem is the AUTHORITATIVE operator-side gate on a kind-3410 redeem event.
// It runs 100% of the verification the operator needs, trusting NOTHING about
// whether a relay gated the write (ADV-2). It enforces, IN ORDER:
//
//	(1) ev is kind KindInvite (3410)                              → error
//	(2) the member's BIP-340 signature over ev verifies           → error
//	    (this is the npub-bind: ev.PubKey provably owns the key)
//	(3) the embedded dgi1_ token in ev.Content parses AND its
//	    operator signature verifies (ParseInviteToken)            → ErrForgedInvite
//	(4) the invite's OperatorPubKey == the pinned operator key    → ErrForgedInvite
//	    (a token signed by any other key is foreign — the PIN)
//	(5) the redeem's ["invite-id"] tag == the invite's GrantID    → ErrForgedInvite
//	(6) the invite has not expired at nowUnix                     → ErrInviteExpired
//
// It does NOT check one-time redemption — that is the caller's persisted
// redeemed-id set (a replay of an already-redeemed grant is rejected there, so it
// survives a process restart). operatorKey may be npub or hex.
func VerifyRedeem(ev *Event, operatorKey string, nowUnix int64) (*Redeem, error) {
	if ev == nil {
		return nil, fmt.Errorf("nostr: VerifyRedeem: nil event")
	}
	if ev.Kind != KindInvite {
		return nil, fmt.Errorf("nostr: VerifyRedeem: kind=%d (want %d)", ev.Kind, KindInvite)
	}
	// (2) The member signed this redeem — the npub-bind. VerifyEventSignature is the
	// same real Schnorr floor the Intake runs on every event.
	if err := VerifyEventSignature(ev); err != nil {
		return nil, fmt.Errorf("nostr: VerifyRedeem: member signature: %w", err)
	}
	opHex, err := normalizeOperatorKey(operatorKey)
	if err != nil {
		return nil, fmt.Errorf("nostr: VerifyRedeem: %w", err)
	}
	// (3) Decode + verify the embedded operator-signed invite.
	in, err := ParseInviteToken(ev.Content)
	if err != nil {
		return nil, fmt.Errorf("nostr: VerifyRedeem: %w", err)
	}
	// (4) PIN: the invite MUST be signed by THIS operator, not merely by some key.
	if !strings.EqualFold(in.OperatorPubKey, opHex) {
		return nil, fmt.Errorf("%w: invite operator %s != this operator %s",
			ErrForgedInvite, shortKey(in.OperatorPubKey), shortKey(opHex))
	}
	// (5) The redeem's invite-id tag must match the signed grant id — a mismatch is
	// a spliced redeem (member advertises one id in the tag, embeds another token).
	if tagID := firstTagValue(ev.Tags, inviteIDTag); !strings.EqualFold(tagID, in.GrantID) {
		return nil, fmt.Errorf("%w: redeem invite-id %q != token grant %q", ErrForgedInvite, tagID, in.GrantID)
	}
	// (6) Freshness.
	if err := CheckInviteFresh(in, nowUnix); err != nil {
		return nil, err
	}
	return &Redeem{MemberHexKey: strings.ToLower(strings.TrimSpace(ev.PubKey)), Invite: in}, nil
}

// firstTagValue returns the value of the first tag named name, or "".
func firstTagValue(tags [][]string, name string) string {
	for _, t := range tags {
		if len(t) >= 2 && t[0] == name {
			return strings.TrimSpace(t[1])
		}
	}
	return ""
}
