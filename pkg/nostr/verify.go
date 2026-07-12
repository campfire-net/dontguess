package nostr

import (
	"errors"
	"fmt"
	"strings"

	"github.com/campfire-net/dontguess/pkg/exchange"
	"github.com/campfire-net/dontguess/pkg/identity"
)

// ErrForgedOperatorEvent is the typed rejection returned when an ingested event
// claims an operator-only exchange kind but is NOT authored by the operator —
// either the author pubkey is not the operator's, or the BIP-340 Schnorr
// signature (and id integrity) does not verify. It is deliberately a LOUD,
// distinguishable sentinel (errors.Is-matchable) so the relay Intake can count
// it (the `dropped_forged` metric) and alarm rather than silently drop.
//
// Design authority: docs/design/relay-transport.md §2.4 step 1 (the operator-key
// ACL + Schnorr re-verify pre-fold) and §3 (loud degradation, the dontguess-553
// lesson). Q3 RULING (operator 2026-07-08): CLIENT-SIDE RE-VERIFY ONLY — this is
// the reader's own re-derivation of operator authorship. It does NOT rely on any
// relay write policy; a dumb relay that enforces nothing is fully defended by
// this gate.
var ErrForgedOperatorEvent = errors.New("nostr: forged operator-authored event")

// operatorSettlePhases is the set of settle(3404) phases that ONLY the exchange
// operator may author. Settle is a shared kind whose authorship is
// phase-dependent (docs/convention/exchange-core/settle.json "Sender roles"):
// the buyer authors preview-request, buyer-accept, buyer-reject, complete,
// dispute, and small-content-dispute; the operator authors put-accept,
// put-reject, preview, and deliver. Enforcing operator authorship on the whole
// kind would reject every legitimate buyer settlement, so the gate is applied
// per phase — mirroring how docs/design/relay-transport.md §2.4 qualifies the
// assign kind as "assign operator sub-ops". This matches the ratified trust
// model (pkg/exchange defaultSettlePhaseLevels: put-accept/put-reject/deliver =
// operator) and extends it to the preview phase, which the convention lists as
// operator-sent but the trust map omits.
var operatorSettlePhases = map[string]struct{}{
	exchange.SettlePhaseStrPutAccept: {},
	exchange.SettlePhaseStrPutReject: {},
	exchange.SettlePhaseStrPreview:   {},
	exchange.SettlePhaseStrDeliver:   {},
	// D5 (docs/design/relay-transport.md §2.4a): settle(failed), if ever
	// authored, is operator-only — an operator-emitted
	// [TagSettle, TagPhasePrefix+"failed"] notice — so a non-operator must not
	// be able to forge a relay-delivered failure notice a client might trust.
	// This ACL entry stands independently of whether any engine path currently
	// emits settle(failed) (dontguess-4be removed the settle-complete emitter,
	// since a post-durable-emit failure is now a loud hard error, not a
	// settle(failed)-retry). Keep the entry: it guards the message type.
	exchange.SettlePhaseStrFailed: {},
}

// operatorAssignOps is the set of assign(3405) sub-ops that ONLY the exchange
// operator may author. Assign is a shared kind whose seven sub-ops split by
// author: workers author assign-claim and assign-complete; the operator authors
// assign (posts the task), assign-accept, assign-reject, assign-expire, and
// assign-auction-close (docs/convention/exchange-core/assign*.json — e.g.
// assign-auction-close.json "Only the exchange operator key may emit this
// message"; assign-expire.json "Emitted by the exchange operator"). Enforcing
// operator authorship on the whole kind would reject legitimate worker claims
// and completions.
var operatorAssignOps = map[string]struct{}{
	exchange.TagAssign:             {},
	exchange.TagAssignAccept:       {},
	exchange.TagAssignReject:       {},
	exchange.TagAssignExpire:       {},
	exchange.TagAssignAuctionClose: {},
}

// eventOp returns the exchange operation tag an event carries: the base op for a
// dedicated kind (put/buy/match/settle), or the ["op", <tag>] discriminator for
// the shared assign/scrip kinds. Returns "" if none is present.
func eventOp(ev *Event) string {
	if op, ok := kindToBaseOp[ev.Kind]; ok {
		return op
	}
	if ev.Kind == KindAssign || ev.Kind == KindScrip {
		for _, t := range ev.Tags {
			if len(t) >= 2 && t[0] == tagOp {
				return t[1]
			}
		}
	}
	return ""
}

// eventPhase returns the settle phase carried in a ["phase", <value>] tag, or ""
// if absent.
func eventPhase(ev *Event) string {
	for _, t := range ev.Tags {
		if len(t) >= 2 && t[0] == tagPhase {
			return t[1]
		}
	}
	return ""
}

// requiresOperatorAuthor reports whether an event of this kind (and sub-op /
// phase) must be authored by the operator to be a valid ingest, per the
// convention spec's sender-role rules. It is relay-agnostic: the decision is a
// pure function of the event's own (kind, op, phase), never of any relay policy.
//
//   - match (3403): always operator-authored.
//   - consume (3406): always operator-authored — the exchange:consume behavioral
//     signal is emitted only by the operator (emitConsumeSignal) and feeds the
//     per-entry consume count that drives the Layer-0..4 metrics and pricing loops
//     (dontguess-d52). A non-operator consume would inflate that count, so it is
//     gated here at intake with the same crypto re-verify as match/scrip, mirroring
//     the exchange fold's own operator-sender guard on applyConsume.
//   - scrip (3411): always operator-authored — the operator is the sole party
//     that mints, holds, settles, pays, and burns (relay-transport.md §E).
//   - settle (3404): operator-authored only for the operator phases
//     (put-accept, put-reject, preview, deliver); buyer phases are not this
//     gate's concern.
//   - assign (3405): operator-authored only for the operator sub-ops
//     (assign, assign-accept, assign-reject, assign-expire, assign-auction-close);
//     worker sub-ops (assign-claim, assign-complete) are not this gate's concern.
//   - everything else (put 3401, buy 3402, the 30401 projection): not
//     operator-only; authorship is enforced by other layers, not here.
func requiresOperatorAuthor(ev *Event) bool {
	switch ev.Kind {
	case KindMatch, KindConsume, KindScrip:
		return true
	case KindSettle:
		_, ok := operatorSettlePhases[eventPhase(ev)]
		return ok
	case KindAssign:
		_, ok := operatorAssignOps[eventOp(ev)]
		return ok
	default:
		return false
	}
}

// normalizeOperatorKey accepts the operator key in either NIP-19 npub form
// ("npub1…") or 32-byte lowercase-hex form and returns the canonical hex the
// nostr event pubkey field carries. An empty key is a caller error.
func normalizeOperatorKey(key string) (string, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return "", fmt.Errorf("nostr: empty operator key")
	}
	if strings.HasPrefix(key, "npub1") {
		h, err := identity.DecodeNpubToHex(key)
		if err != nil {
			return "", fmt.Errorf("nostr: decode operator npub: %w", err)
		}
		return strings.ToLower(h), nil
	}
	return strings.ToLower(key), nil
}

// VerifyOperatorAuthorship is the operator-authorship VERIFY PRIMITIVE the relay
// Intake calls on every received event BEFORE folding it into the local store
// (docs/design/relay-transport.md §2.4 step 1). Given a wire nostr event and the
// exchange operator's key (npub or hex), it enforces that operator-only kinds —
// match(3403), settle(3404) operator phases, assign(3405) operator sub-ops, and
// scrip(3411) — are genuinely authored by the operator:
//
//	(1) the event's author pubkey equals the operator key, AND
//	(2) the event's BIP-340 Schnorr signature (and id integrity) verifies via
//	    identity.VerifyEvent — a REAL secp256k1 verification, not a claim.
//
// A forged operator-kind event (right kind, wrong author, or an author-mismatch
// with a validly-signed body) is rejected LOUD with ErrForgedOperatorEvent so
// the caller can count and alarm it. Events that are not operator-only return
// nil — this gate only governs operator authorship; buyer/worker/seller kinds
// are validated by other layers.
//
// Q3 RULING (operator 2026-07-08): this is CLIENT-SIDE RE-VERIFY ONLY. It does
// not depend on any relay per-kind write ACL; a reader behind a dumb relay is
// fully defended because it re-derives operator authorship from the signed event
// itself.
func VerifyOperatorAuthorship(ev *Event, operatorKey string) error {
	if ev == nil {
		return fmt.Errorf("nostr: VerifyOperatorAuthorship: nil event")
	}
	if !requiresOperatorAuthor(ev) {
		return nil
	}
	opHex, err := normalizeOperatorKey(operatorKey)
	if err != nil {
		return fmt.Errorf("nostr: VerifyOperatorAuthorship: %w", err)
	}

	// (1) Author pubkey must be the operator. Reject a wrong-author event before
	// spending a signature verification on it.
	if !strings.EqualFold(strings.TrimSpace(ev.PubKey), opHex) {
		return fmt.Errorf("%w: kind %d op=%q phase=%q claims operator authorship but pubkey %s != operator %s",
			ErrForgedOperatorEvent, ev.Kind, eventOp(ev), eventPhase(ev),
			shortKey(ev.PubKey), shortKey(opHex))
	}

	// (2) The Schnorr signature (and the id-vs-content integrity check inside
	// VerifyEvent) must verify against the claimed pubkey. This is what makes a
	// stolen/forged pubkey field insufficient: only the operator's private key
	// can produce a signature that verifies here.
	if err := identity.VerifyEvent(toIdentityEvent(ev)); err != nil {
		return fmt.Errorf("%w: kind %d op=%q phase=%q: %v",
			ErrForgedOperatorEvent, ev.Kind, eventOp(ev), eventPhase(ev), err)
	}
	return nil
}

// VerifyEventSignature is the UNIVERSAL per-event signature floor the relay
// Intake calls as the UNCONDITIONAL FIRST step on EVERY received wire event, for
// ALL kinds, BEFORE FromNostrEvent, VerifyOperatorAuthorship, or Sequencer.Ingest
// (docs/design/relay-transport.md §2.4a D1). It recomputes the NIP-01 id from the
// wire fields and checks the BIP-340 Schnorr signature against ev.PubKey via the
// existing identity.VerifyEvent.
//
// This is the fix for the CRITICAL that VerifyOperatorAuthorship returns nil for
// every NON-operator kind (requiresOperatorAuthor false), so put(3401)/buy(3402)/
// buyer settle phases/worker assign sub-ops previously rode in with
// msg.Sender = ev.PubKey attacker-controlled and cryptographically UNBOUND. Run
// first, this makes msg.Sender == ev.PubKey a cryptographically-bound fact for
// every kind — the buyer-phase settle auth (msg.Sender == expectedBuyer) is sound
// only because this floor proved msg.Sender. An invalid/absent signature is a LOUD
// dropped_unsigned drop at the Intake boundary.
//
// Q3 RULING (operator 2026-07-08): CLIENT-SIDE RE-VERIFY ONLY — zero dependence on
// any relay write policy; a reader behind a dumb relay is fully defended.
func VerifyEventSignature(ev *Event) error {
	if ev == nil {
		return fmt.Errorf("nostr: VerifyEventSignature: nil event")
	}
	return identity.VerifyEvent(toIdentityEvent(ev))
}

// toIdentityEvent maps a wire nostr.Event to the structurally-identical
// identity.Event so identity.VerifyEvent can recompute the id and verify the
// Schnorr signature. Every field is carried verbatim — the id and signature must
// be the ones that came off the wire for the verification to be meaningful.
func toIdentityEvent(ev *Event) *identity.Event {
	return &identity.Event{
		ID:        ev.ID,
		PubKey:    ev.PubKey,
		CreatedAt: ev.CreatedAt,
		Kind:      ev.Kind,
		Tags:      ev.Tags,
		Content:   ev.Content,
		Sig:       ev.Sig,
	}
}

// shortKey abbreviates a hex key for diagnostics without leaking the full value
// into logs. Short inputs are returned as-is.
func shortKey(k string) string {
	k = strings.TrimSpace(k)
	if len(k) <= 12 {
		return k
	}
	return k[:8] + "…" + k[len(k)-4:]
}
