// Package nostr is the event-shape adapter between the dontguess exchange's
// internal message representation (proto.Message) and nostr events.
//
// It sits behind the existing proto.Message seam: FromNostrEvent produces a
// *proto.Message the unchanged exchange engine folds, and ToNostrEvent converts
// a *proto.Message the engine (or a publisher) emits into a nostr Event. The
// exchange state machine and engine handlers are untouched — they continue to
// operate on proto.Message/exchange.Message exactly as before.
//
// Scope (dontguess-3d7): the event<->Message shape round-trip only. This package
// is NOT the local sequencer / orphan-antecedent buffer (dontguess-50d), NOT the
// scrip transport swap (dontguess-203), and NOT the provenance/trust primitive
// (dontguess-3311). It does not sign events, compute nostr event ids, fetch
// Blossom blobs, or build the 30401 inventory projection. Those are separate
// workstreams; see docs/design/nostr-first-rebuild-decision.md.
//
// Authoritative event mapping: docs/design/nostr-first-rebuild-decision.md
// §Nostr Architecture. Kind numbers are draft (collision-check the live NIP
// registry before locking — YAGNI #6 in that doc): the "adapter passes the
// existing exchange test suite unchanged" gate validates the tag mapping long
// before external kind collisions matter.
package nostr

import (
	"github.com/campfire-net/dontguess/pkg/exchange"
	"github.com/campfire-net/dontguess/pkg/scrip"
)

// Nostr event kinds for exchange operations (draft — see package doc).
//
// 3401-3405 and 3411 are "regular" (immutable, replayable) events. 30401 is an
// "addressable" (parameterised-replaceable) event — a PROJECTION republished
// from the fold, latest-wins by d-tag; it is NOT source of truth and is NOT part
// of the Message<->event round-trip (there is no folded Message for it — the
// operator generates it from derived state). It is defined here only so the kind
// range is documented in one place.
const (
	KindPut                 = 3401  // exchange:put            (regular, immutable)
	KindBuy                 = 3402  // exchange:buy            (regular, immutable)
	KindMatch               = 3403  // exchange:match          (regular, immutable)
	KindSettle              = 3404  // exchange:settle         (regular, immutable)
	KindAssign              = 3405  // exchange:assign* (7 sub-ops, single kind)
	KindConsume             = 3406  // exchange:consume        (regular, operator behavioral signal)
	KindScrip               = 3411  // dontguess:scrip-*       (regular, team-tier)
	KindInventoryProjection = 30401 // inventory+price PROJECTION (addressable, NOT source of truth)
)

// Nostr tag names used by the adapter.
const (
	tagE     = "e"     // NIP-01 reply marker: ["e", <antecedent-id>, "", "reply"]
	tagP     = "p"     // author reference (Message.Sender)
	tagT     = "t"     // NIP-01 topic tag: domains  ->  ["t", <domain>]
	tagPhase = "phase" // settle/assign phase: exchange:phase:X -> ["phase", X]
	tagOp    = "op"    // sub-op discriminator for shared kinds (3405 assign, 3411 scrip)
	tagX     = "x"     // lossless passthrough for any other exchange tag

	// dontguess-namespaced preservation tags. These carry campfire-era Message
	// fields that have no nostr-native home so the round-trip is exact. They are
	// adapter-internal; a pure-nostr future may drop them, but while the unchanged
	// engine consumes proto.Message they preserve full fidelity.
	tagDGTimestamp = "dg_ts"       // exact Message.Timestamp (nanoseconds)
	tagDGCampfire  = "dg_cf"       // Message.CampfireID
	tagDGInstance  = "dg_instance" // Message.Instance (tainted self-asserted role)

	replyMarker = "reply"
)

// baseOpToKind maps the exchange operations that each own a DEDICATED kind — the
// four base client ops (put/buy/match/settle) plus the operator-authored consume
// behavioral signal (dontguess-d52). For all of these the kind alone fully
// determines the op tag, so the op tag is consumed by the kind (not re-emitted)
// and reconstructed from the kind on the reverse path. (consume rides here rather
// than an ["op", …] discriminator because, like the base ops, it is the sole op on
// its kind — the discriminator mechanism only exists for the SHARED assign/scrip
// kinds where the kind cannot disambiguate the sub-op.)
var baseOpToKind = map[string]int{
	exchange.TagPut:     KindPut,
	exchange.TagBuy:     KindBuy,
	exchange.TagMatch:   KindMatch,
	exchange.TagSettle:  KindSettle,
	exchange.TagConsume: KindConsume,
}

// kindToBaseOp is the inverse of baseOpToKind.
var kindToBaseOp = map[int]string{
	KindPut:     exchange.TagPut,
	KindBuy:     exchange.TagBuy,
	KindMatch:   exchange.TagMatch,
	KindSettle:  exchange.TagSettle,
	KindConsume: exchange.TagConsume,
}

// assignOps is the set of the seven assign sub-op tags. They all share kind 3405;
// the exact sub-op is preserved in an ["op", <tag>] discriminator tag (mirroring
// how settle compacts its variants under a single kind + phase tag).
var assignOps = map[string]struct{}{
	exchange.TagAssign:             {},
	exchange.TagAssignClaim:        {},
	exchange.TagAssignComplete:     {},
	exchange.TagAssignAccept:       {},
	exchange.TagAssignReject:       {},
	exchange.TagAssignExpire:       {},
	exchange.TagAssignAuctionClose: {},
}

// scripOps is the set of scrip op tags. They all share kind 3411; the exact sub-op
// is preserved in an ["op", <tag>] discriminator tag. Included for completeness of
// the kind range — the scrip transport swap itself is a separate workstream
// (dontguess-203); this adapter only fixes the event shape.
var scripOps = map[string]struct{}{
	scrip.TagScripMint:          {},
	scrip.TagScripBurn:          {},
	scrip.TagScripPutPay:        {},
	scrip.TagScripBuyHold:       {},
	scrip.TagScripSettle:        {},
	scrip.TagScripAssignPay:     {},
	scrip.TagScripDisputeRefund: {},
	scrip.TagScripLoanMint:      {},
	scrip.TagScripLoanRepay:     {},
	scrip.TagScripLoanVigAccrue: {},
}

// kindForOp returns the nostr kind for an exchange op tag and whether the op is
// recognised.
func kindForOp(op string) (int, bool) {
	if k, ok := baseOpToKind[op]; ok {
		return k, true
	}
	if _, ok := assignOps[op]; ok {
		return KindAssign, true
	}
	if _, ok := scripOps[op]; ok {
		return KindScrip, true
	}
	return 0, false
}

// primaryOp scans a tag list for the single exchange operation tag that
// identifies the message. Mirrors exchange.exchangeOp's precedence (first match
// wins) but also recognises the scrip ops so the adapter covers every kind in the
// draft table. Returns "" if no operation tag is present.
func primaryOp(tags []string) string {
	for _, t := range tags {
		if _, ok := kindForOp(t); ok {
			return t
		}
	}
	return ""
}
