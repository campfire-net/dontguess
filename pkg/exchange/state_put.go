package exchange

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"github.com/3dl-dev/dontguess/pkg/matching"
	"github.com/3dl-dev/dontguess/pkg/nip44"
	"github.com/3dl-dev/dontguess/pkg/scrip"
	"golang.org/x/crypto/chacha20poly1305"
)

// validCompressionTiers is the set of accepted compression_tier values.
// The empty string (unset) is also valid and means no tier preference.
var validCompressionTiers = map[string]struct{}{
	"hot":  {},
	"warm": {},
	"cold": {},
}

// isExchangeOpTag reports whether a tag is a first-class exchange operation
// constant — the canonical vocabulary that selects a fold/dispatch handler.
// Secondary markers (exchange:buy-miss, exchange:synthetic) and phase/domain/
// verdict tags are deliberately NOT in this set: they never select the
// executed op.
//
// Scrip ops (dontguess:scrip-*) ARE included (dontguess-e15, wave-7 security
// review of dontguess-c22). A scrip-kind (Kind=3411) message's own canonical op
// (e.g. dontguess:scrip-mint) does not itself dispatch a fold handler through
// this switch (applyLocked's default branch scans for the few scrip tags it
// actually indexes, e.g. TagScripBuyHold) — but it MUST still count as a
// canonical op member for the ambiguity check below. Excluding scrip ops from
// this set made a scrip-kind event carrying a smuggled
// ["x","exchange:assign-auction-close"] tag resolve as a single, unambiguous
// op (the smuggled tag was the only isExchangeOpTag member found), so the
// multi-op fail-loud never triggered and the smuggled assign-auction-close op
// executed cleanly. Counting scrip ops here means any additional distinct
// canonical op tag on a scrip-kind event — smuggled or not — trips the
// ambiguity check and exchangeOp fails loud, matching the invariant documented
// on exchangeOp below: an ["x","exchange:*"] op-collision must be inert for
// every event kind, not just put/buy/match/settle/assign*.
//
// TagConsume (exchange:consume) IS included (dontguess-13c, completing the
// e15 residual). Like scrip ops, a consume-kind message is DISPATCHED through
// applyLocked's default branch (state_core.go:174, scanning for TagConsume)
// rather than through the switch above, and it is operator-emitted (see
// applyConsume's operator-sender guard). Before this fix, TagConsume was
// listed only as a "secondary marker" excluded from this set, so a
// consume-carrier event (Sender=operator, Tags=[TagConsume, smuggled
// "exchange:assign-auction-close"]) contributed ZERO isExchangeOpTag members
// from its own consume op, leaving the smuggled assign-auction-close tag as
// the only canonical op found — the same unambiguous-and-wrong resolution
// class e15 fixed for scrip. Counting TagConsume here closes that same gap:
// any additional distinct canonical op tag on a consume-kind event — smuggled
// or not — now trips the ambiguity check and exchangeOp fails loud.
func isExchangeOpTag(t string) bool {
	switch t {
	case TagPut, TagBuy, TagMatch, TagSettle,
		TagAssign, TagAssignClaim, TagAssignComplete, TagAssignAccept, TagAssignReject,
		TagAssignExpire, TagAssignAuctionClose,
		scrip.TagScripMint, scrip.TagScripBurn, scrip.TagScripPutPay, scrip.TagScripBuyHold,
		scrip.TagScripSettle, scrip.TagScripAssignPay, scrip.TagScripDisputeRefund,
		scrip.TagScripLoanMint, scrip.TagScripLoanRepay, scrip.TagScripLoanVigAccrue,
		TagConsume:
		return true
	}
	return false
}

// exchangeOp returns the single exchange operation tag that identifies a
// message, or "" if the message carries no operation tag OR carries an ambiguous
// set of them.
//
// CANONICAL-SOURCE RULE (docs/design/relay-transport.md §2.4a D2, reworked). The
// executed op is derived ONLY from a message's canonical operation vocabulary:
// the primary op the nostr adapter reconstructs from the event Kind
// (put/buy/match/settle) plus the structured ["op"] discriminator it emits for
// the shared kinds (3405 assign*, 3411 scrip). A ["x", <raw>] passthrough tag —
// the adapter's lossless carrier for secondary markers such as
// exchange:buy-miss — is NOT a canonical op source and MUST NEVER select the
// executed op.
//
// A folded message is a flat []string in which a smuggled
// ["x","exchange:assign-auction-close"] value is indistinguishable BY STRING
// from a legitimate discriminator, so this function cannot infer provenance from
// the value alone. It instead enforces the invariant that a well-formed message
// names EXACTLY ONE canonical op: if two or more DISTINCT op constants are
// present the canonical source is ambiguous and the function FAILS LOUD (returns
// "", i.e. unroutable/dropped) rather than silently returning the first — which,
// in attacker-chosen wire order, could be the smuggled one. An
// ["x","exchange:*"] op-collision is therefore INERT: it can never quietly
// become the op. This defends every transport that can construct a
// proto.Message, not just nostr, and does so without touching the LOCKED
// proto.Message shape (no canonical Op field).
//
// Secondary markers that are not op constants round-trip losslessly and fold
// correctly: a buy-miss standing offer tagged [exchange:buy-miss, exchange:match]
// still resolves to the single op TagMatch. A lone exchange:consume message
// resolves to its own canonical op (TagConsume, dontguess-13c); since
// applyLocked's switch has no case for TagConsume, it still falls through to
// the default branch, which scans tags for TagConsume and calls applyConsume —
// same as scrip ops (dontguess-e15), whose canonical ops likewise fall through
// to the default branch's tag scan.
func exchangeOp(tags []string) string {
	found := ""
	for _, t := range tags {
		if !isExchangeOpTag(t) {
			continue
		}
		if found != "" && t != found {
			// Two distinct canonical op constants — ambiguous source, fail loud.
			return ""
		}
		found = t
	}
	return found
}

// settlePhasFromTags extracts the exchange:phase:* value from tags.
func settlePhaseFromTags(tags []string) string {
	for _, t := range tags {
		if strings.HasPrefix(t, TagPhasePrefix) {
			return strings.TrimPrefix(t, TagPhasePrefix)
		}
	}
	return ""
}

// isTestLikeDescription reports whether a put description represents synthetic or
// junk content that should be rejected by the put quality-gate (dontguess-ed1).
//
// Rules (aligned with demand.IsSynthetic patterns, restricted to the put/description domain):
//   - bare "test" (case-insensitive, trimmed) — the exact smoke-test entry from the live exchange
//   - starts with "upgrade smoke test" — the "upgrade smoke test cf v0.31.2 operator" junk class
//
// NOTE: Descriptions like "test coverage audit", "test strategy", "test gap scan",
// "flock contention test pattern for Go", or "testing the X interface" are NOT rejected —
// they describe real engineering work. This predicate matches only the narrow
// synthetic/smoke class identified in measurement review §2. When in doubt, accept.
//
// Callers that classify buy miss traffic should use demand.IsSynthetic, which has a
// broader set of exclusion rules. This function is the put-side analog — narrower,
// since false positives at put time permanently lose legitimate content from inventory.
func isTestLikeDescription(desc string) bool {
	lower := strings.ToLower(strings.TrimSpace(desc))
	// Reject bare "test" — the exact description of the junk smoke-test entry in
	// the live exchange that served 1,576 hits (measurement review §2).
	if lower == "test" {
		return true
	}
	// Reject the "upgrade smoke test" junk class — the cf v0.31.2 operator smoke
	// test that polluted inventory ("upgrade smoke test cf v0.31.2 operator", etc).
	if strings.HasPrefix(lower, "upgrade smoke test") {
		return true
	}
	return false
}

// highReuseClass is a single entry in the high-reuse keyword classification table.
// To be high-reuse, a description must match the primary keywords AND at least one
// of the co-signals. This two-gate design prevents bare-keyword gaming: an agent
// cannot mislabel session ephemera as high-reuse by including a single term.
type highReuseClass struct {
	// primary is a required keyword that must appear in the lowercased description.
	primary string
	// coSignals is a set of co-occurring signals; at least one must also appear.
	// These represent structural context that distinguishes reusable artifacts from
	// session-specific mentions (e.g. "protocol" co-signals README from "my notes on the readme").
	coSignals []string
}

// highReuseKeywords defines the classification table for §4 high-reuse artifact classes.
//
// §4 classes (from exchange-matching-measurement-review.md):
//  1. Schema correctness checklists  — e.g. "legion.tools v1.2 schema correctness checklist"
//  2. Cross-project protocol/setup READMEs — e.g. "cf-protocol README CF_NO_PINS"
//  3. CI path filter / CI config fragments — e.g. "GateEvaluator conformance CI path filter"
//  4. Language-level test patterns — e.g. "flock contention test pattern for Go"
//  5. Migration recipes/runbooks — e.g. "cf migrate-store --cf-home symlink bridge"
//
// GAMEABILITY NOTE: bare substring matches on common words ("readme", "pattern",
// "guide") are gameable — an agent can mention the word in a session-ephemera
// description and receive the high-reuse pricing tier. Each entry therefore
// requires a co-occurring structural signal that distinguishes the real artifact
// class from ephemeral mentions. See unit tests in put_reuse_class_test.go for
// concrete examples of descriptions that must NOT classify as high-reuse.
var highReuseKeywords = []highReuseClass{
	// Class 1: schema correctness checklists
	// Primary: "checklist" (a checklist is inherently a reusable artifact)
	// Co-signals: schema/conformance/correctness context
	{
		primary:   "checklist",
		coSignals: []string{"schema", "conformance", "correctness", "protocol", "validation"},
	},
	// Class 2: cross-project protocol/setup READMEs
	// Primary: "readme" but only when describing a protocol, config, or setup doc —
	// NOT a bare mention as in "analysis of the project readme" or "my notes on what the readme says".
	// Co-signals: protocol/config/setup context. "readme" alone is not a distilled artifact.
	{
		primary:   "readme",
		coSignals: []string{"protocol", "setup", "config", "install", "bootstrap", "integration"},
	},
	// Class 3: CI path filter / config fragments
	// Primary: "ci" or "path filter" (combined via multi-primary logic below)
	// Co-signals: filter/conformance/config context
	{
		primary:   "ci path filter",
		coSignals: []string{"conformance", "ci", "filter", "config", "pipeline"},
	},
	{
		primary:   "ci config",
		coSignals: []string{"filter", "conformance", "pipeline", "fragment", "plug-and-play"},
	},
	// Class 4: language-level test patterns
	// Primary: "test pattern" (the compound is the artifact — "pattern" alone is too generic)
	// Co-signals: language/library/idiom context
	{
		primary:   "test pattern",
		coSignals: []string{"go", "rust", "python", "java", "typescript", "flock", "lock", "contention", "idiomatic", "idiom"},
	},
	// Class 5: migration recipes / runbooks
	// Primary: "migration" or "migrate" + artifact signal
	// Co-signals: recipe/runbook/bridge/symlink/procedure context
	{
		primary:   "migration recipe",
		coSignals: []string{"step", "procedure", "runbook", "bridge", "symlink", "upgrade"},
	},
	{
		primary:   "migrate",
		coSignals: []string{"recipe", "runbook", "bridge", "symlink", "procedure", "step-by-step"},
	},
}

// minHighReuseWords is the minimum number of *raw* description tokens an entry must
// have to qualify as a §4 high-reuse artifact. Genuine distilled artifacts are
// *described* (a noun phrase naming the thing, plus its context) — every §4 exemplar
// and every positive case in put_reuse_class_test.go is ≥5 tokens. This is a cheap
// first cut; the real gameability defense is the content-bearing context floor below
// (see minHighReuseContextWords), which alone catches the filler-padding bypass.
const minHighReuseWords = 5

// minHighReuseContextWords is the minimum number of *content-bearing context* tokens
// an entry must carry OUTSIDE the matched primary+co-signal trigger cluster.
//
// WHY: the raw-token floor (minHighReuseWords) counts ANY token, including filler —
// stopwords ("the", "my", "for"), single-character tokens ("a"), and punctuation-glued
// garbage ("x/y/z/w", "--a", "a.b.c") that the tokenizer's '. _ / -' word-set inflates
// into tokens. An attacker keeps the trigger cluster intact and adjacent ("test pattern
// go") and pads with filler to clear the raw floor, e.g. "foo bar baz test pattern go".
// The raw floor cannot see that the pad is contentless.
//
// FIX: count only content-bearing context tokens (see isContentBearingToken) that are
// NOT part of the matched trigger cluster, and require at least this many. A distilled
// artifact names real domain nouns around the trigger ("flock contention test pattern
// for Go", "legion.tools v1.2 schema correctness checklist" — ≥2 substantive context
// words each). A keyword-stuff has only the bait plus junk.
//
// This attacks the SHAPE of keyword-stuffing, not a blocklist: any filler set (single
// chars, stopwords, 3-char nonsense stubs, slash/dot/hyphen-glued garbage) fails to
// produce content-bearing context. The residual cost it imposes on an attacker is that
// they must supply ≥2 genuine descriptive nouns — i.e. actually describe an artifact —
// which is the behavior we want to incentivize, not suppress.
const minHighReuseContextWords = 2

// minContentTokenLen is the minimum character length for a token to count as
// content-bearing context. Genuine descriptive domain nouns observed across every §4
// exemplar are ≥4 chars ("flock", "schema", "contention", "legion.tools", "convention",
// "gateevaluator", "pipeline", "bootstrap"). 1–3 char tokens used as padding ("foo",
// "bar", "baz", "xyz", "abc", "qrs") carry no description. Note: short tokens that ARE
// the classifier's own keywords ("cf", "ci", "go") live inside the trigger cluster and
// are excluded from the context count anyway, so this floor does not penalize them.
const minContentTokenLen = 4

// minHighReuseCoherence is the minimum cosine similarity, between the description's
// content-bearing context nouns and the put's actual content, required to grant §4
// high-reuse-artifact status (Gate 6, dontguess-5f5).
//
// WHY: Gates 2-5 are purely STRUCTURAL — they count content-bearing tokens
// (isContentBearingToken) but cannot tell whether those tokens MEAN anything relative
// to the content. A crafted description with exactly minHighReuseContextWords plausible
// ≥4-char nouns plus the trigger phrase ("widget gadget test pattern go", "abcd efgh
// test pattern go") clears every structural gate even though the padding nouns are
// unrelated to the bytes actually sold — extracting the 85% accept price + 20% residual
// that §4 reserves for genuine reusable artifacts.
//
// FIX: embed the description's content-bearing context nouns (the tokens Gate 5 counts,
// i.e. the ones OUTSIDE the trigger cluster) and the put's content, and require their
// cosine similarity to clear this floor. A genuine artifact names domain nouns that
// actually appear in / relate to its content ("flock contention race detector" vs Go
// code that mentions flock/contention/race). A keyword-stuff's padding nouns are
// disconnected from the content and score ~0.
//
// Threshold selection (measured with the pure-Go TF-IDF embedder used here): genuine
// §4 artifacts whose content contains the claimed nouns score ≥0.59; crafted stuffs
// whose padding nouns are absent from content score exactly 0.0, EVEN when the attacker
// stuffs the trigger words ("test pattern go") into the content — because the coherence
// check embeds only the CONTEXT nouns, not the trigger cluster. 0.10 sits comfortably
// between the two populations: well above the disconnected-noun floor (0.0), well below
// the genuine floor (~0.59). Consistent with the class's conservative posture — false
// negatives (genuine entries that miss the premium) are acceptable and settle at the
// standard rate; false positives (ephemera earning the premium) undermine the incentive.
const minHighReuseCoherence = 0.10

// coherenceContentSampleBytes bounds how much content the coherence gate embeds. The
// gate is called in the auto-accept and settle paths, so it must stay cheap. A genuine
// artifact's claimed nouns appear throughout its content; sampling the leading bytes is
// sufficient for a "not disconnected" check. For Blossom-offloaded entries entry.Content
// is already only the inline preview slice, typically below this cap.
const coherenceContentSampleBytes = 8192

// highReuseStopwords are common English function words that never constitute the
// descriptive context of a distilled artifact. They are excluded from the
// content-bearing context count so padding with stopwords cannot satisfy the floor.
var highReuseStopwords = map[string]struct{}{
	"the": {}, "a": {}, "an": {}, "and": {}, "or": {}, "of": {}, "to": {}, "in": {},
	"on": {}, "for": {}, "with": {}, "at": {}, "by": {}, "as": {}, "is": {}, "are": {},
	"my": {}, "our": {}, "this": {}, "that": {}, "it": {}, "its": {}, "from": {},
	"into": {}, "new": {}, "all": {}, "any": {}, "some": {}, "be": {}, "was": {},
}

// isContentBearingToken reports whether a token is a substantive descriptive word —
// the kind that names an artifact's domain context — as opposed to filler an attacker
// uses to pad a keyword-stuff to the raw-token floor.
//
// A token is filler (NOT content-bearing) if any of the following hold:
//   - it is shorter than minContentTokenLen characters (single chars and 3-char stubs);
//   - it is a stopword (highReuseStopwords);
//   - it is punctuation-glue garbage: after splitting on the tokenizer's structural
//     punctuation ('.', '_', '/', '-'), every resulting alphanumeric run is a single
//     character. This catches "x/y/z/w", "--a", "a.b.c", "a_b_c" — strings that exist
//     only to inflate the token count, while preserving genuine glued identifiers like
//     "cf-protocol", "legion.tools", "migrate-store", "cf_no_pins" whose runs are multi-char.
func isContentBearingToken(tok string) bool {
	if len(tok) < minContentTokenLen {
		return false
	}
	if _, ok := highReuseStopwords[tok]; ok {
		return false
	}
	// Punctuation-glue detection: split on structural punctuation and check whether
	// every alphanumeric run is a single char (pure glue garbage).
	runs := strings.FieldsFunc(tok, func(r rune) bool {
		return r == '.' || r == '_' || r == '/' || r == '-'
	})
	if len(runs) > 1 {
		allSingle := true
		for _, run := range runs {
			if len(run) > 1 {
				allSingle = false
				break
			}
		}
		if allSingle {
			return false
		}
	}
	return true
}

// highReuseCoSignalWindow is the maximum token distance permitted between a matched
// primary keyword and its co-signal. In every genuine §4 exemplar the co-signal is
// part of the same descriptive phrase as the primary (observed max distance: 2). A
// co-signal that appears far from the primary — e.g. "checklist of things I need to
// do today … schema" — is incidental, not structural. Bounding the distance (rather
// than matching the co-signal anywhere in the string) defeats the "pad to the word
// floor, then jam an unrelated trigger word in" variant of the keyword-stuff attack.
const highReuseCoSignalWindow = 3

// tokenizeDescription splits a description into lowercased tokens on runs of
// characters that are not part of a word. Word characters include alphanumerics
// plus '.', '_', '/', and '-' so that identifiers like "cf-protocol",
// "--cf-home", "migrate-store", and "legion.tools" survive as single tokens —
// matching how the §4 artifact names actually read.
func tokenizeDescription(desc string) []string {
	return strings.FieldsFunc(strings.ToLower(desc), func(r rune) bool {
		switch {
		case r >= 'a' && r <= 'z':
			return false
		case r >= '0' && r <= '9':
			return false
		case r == '.' || r == '_' || r == '/' || r == '-':
			return false
		default:
			return true
		}
	})
}

// IsHighReuseArtifact reports whether an inventory entry belongs to the §4 high-reuse
// distilled-artifact class (exchange-matching-measurement-review.md §4).
//
// Classification is gated on FOUR structural conditions, all of which must hold. The
// design goal is gameability resistance: an agent must not be able to mislabel session
// ephemera as high-reuse to earn the 85% accept price + 20% residual by stuffing the
// classifier's trigger words into a short, contentless description.
//
//   - Gate 1: content_type must be "code", "analysis", or "summary" — the types that
//     carry reusable engineering artifacts. Session-ephemera types ("review", "data",
//     "other") are excluded even if they contain matching keywords.
//
//   - Gate 2 (length floor): the description must have at least minHighReuseWords
//     tokens. A distilled artifact is described, not keyword-tagged. Crafted stuffs
//     like "test pattern go idiom" (4 tokens) carry only trigger words and fail here.
//
//   - Gate 3 (primary keyword): the description must contain a §4 primary keyword.
//
//   - Gate 4 (co-signal adjacency): at least one co-signal of the matched class must
//     appear within highReuseCoSignalWindow tokens of the primary keyword. Requiring
//     adjacency — not mere presence anywhere in the string — means the co-signal must
//     be part of the same descriptive phrase as the primary, the way it reads in every
//     genuine §4 exemplar. This blocks both the bare keyword-stuff (caught by the
//     length floor) and the "pad to the floor then drop a far-away trigger word" variant.
//
//   - Gate 5 (content-bearing context floor): at least minHighReuseContextWords
//     content-bearing tokens (see isContentBearingToken) must appear OUTSIDE the matched
//     trigger cluster (primary tokens + the matched co-signal). The raw-token floor
//     (Gate 2) counts filler — stopwords, single chars, punctuation-glue garbage —
//     so an attacker can keep the trigger cluster adjacent (defeating Gate 4) and pad
//     with junk to clear Gate 2, e.g. "foo bar baz test pattern go". Gate 5 ignores
//     filler and requires genuine descriptive context around the bait.
//
// Gates 2, 4, and 5 attack the *structure* of a keyword-stuff (all-trigger / incidental
// co-signal / filler padding) rather than enumerating bad strings, so they generalize
// to crafted inputs not seen at design time.
//
// This is intentionally conservative: false negatives (real high-reuse entries that
// miss the classifier) are acceptable — the exchange still accepts them at the standard
// rate. False positives (ephemera classified as high-reuse) undermine the incentive
// mechanism and seller trust in pricing fairness.
func IsHighReuseArtifact(entry *InventoryEntry) bool {
	matched, contextNouns := highReuseStructuralMatch(entry)
	if !matched {
		return false
	}
	// Gate 6 (semantic coherence, dontguess-5f5): the structural gates (2-5) count
	// content-bearing tokens but cannot tell whether those tokens MEAN anything relative
	// to the bytes actually sold. Require the description's content-bearing context nouns
	// to be semantically coherent with the put's content. This rejects a crafted stuff
	// whose padding nouns clear Gate 5's structural floor but are disconnected from the
	// content ("widget gadget test pattern go" over unrelated bytes), while genuine
	// artifacts — whose claimed nouns actually appear in / relate to their content — pass.
	return descriptionContentCoherent(contextNouns, entry.Content)
}

// highReuseStructuralMatch runs the FIVE structural §4 gates and, when they all hold,
// returns true plus the description's content-bearing context nouns — the tokens Gate 5
// counts (content-bearing per isContentBearingToken, OUTSIDE the matched primary+co-signal
// trigger cluster). Those nouns are the description's *claimed* domain terms; the coherence
// gate (descriptionContentCoherent) checks they are not disconnected from the put's content.
//
// Returns (false, nil) if any structural gate fails. On a match it returns the context
// nouns of the FIRST cluster that satisfies the content-bearing floor (the same cluster
// the pre-5f5 code short-circuited to true on), so classification behavior for existing
// callers is unchanged for content-less entries.
func highReuseStructuralMatch(entry *InventoryEntry) (bool, []string) {
	// Gate 1: content_type filter.
	// High-reuse artifacts are code, analysis, or summary. Review, data, and other
	// types carry session-specific content that rarely generalizes across projects.
	switch entry.ContentType {
	case "code", "analysis", "summary":
		// passes gate 1 — continue
	default:
		return false, nil
	}

	tokens := tokenizeDescription(entry.Description)

	// Gate 2: length floor. Keyword-stuffs are short by nature — they are the trigger
	// words and nothing else. Genuine artifacts carry descriptive context.
	if len(tokens) < minHighReuseWords {
		return false, nil
	}

	// Gates 3 & 4: primary keyword present AND a co-signal adjacent to it.
	// For each class, locate every occurrence of the (possibly multi-token) primary
	// keyword in the token stream, then require a co-signal within the adjacency
	// window of that occurrence.
	for _, cls := range highReuseKeywords {
		primTokens := strings.Fields(cls.primary)
		for pStart := 0; pStart+len(primTokens) <= len(tokens); pStart++ {
			if !tokensMatchAt(tokens, pStart, primTokens) {
				continue
			}
			pEnd := pStart + len(primTokens) - 1
			// Window of tokens that count as adjacent to this primary occurrence.
			lo := pStart - highReuseCoSignalWindow
			if lo < 0 {
				lo = 0
			}
			hi := pEnd + highReuseCoSignalWindow
			if hi > len(tokens)-1 {
				hi = len(tokens) - 1
			}
			for i := lo; i <= hi; i++ {
				if i >= pStart && i <= pEnd {
					continue // skip the primary tokens themselves
				}
				matchedCoSignal := false
				for _, sig := range cls.coSignals {
					if tokens[i] == sig {
						matchedCoSignal = true
						break
					}
				}
				if !matchedCoSignal {
					continue
				}
				// Gate 5 (content-bearing context floor): the trigger cluster
				// (primary tokens + this matched co-signal) is the classifier's own
				// bait. Genuine artifacts surround that bait with substantive domain
				// nouns; keyword-stuffs surround it with filler padded to clear the
				// raw-token floor. Count content-bearing context tokens OUTSIDE the
				// trigger cluster and require at least minHighReuseContextWords. This
				// rejects the filler-padding bypass (single chars, stopwords, 3-char
				// stubs, punctuation-glue garbage) regardless of the specific pad.
				contextNouns := make([]string, 0, len(tokens))
				for j, tok := range tokens {
					if j >= pStart && j <= pEnd { // skip primary tokens
						continue
					}
					if j == i { // skip the matched co-signal
						continue
					}
					if isContentBearingToken(tok) {
						contextNouns = append(contextNouns, tok)
					}
				}
				if len(contextNouns) >= minHighReuseContextWords {
					return true, contextNouns
				}
			}
		}
	}
	return false, nil
}

// descriptionContentCoherent reports whether the description's content-bearing context
// nouns are semantically coherent with the put's content (Gate 6, dontguess-5f5).
//
// It embeds the context nouns and a leading sample of the content with the pure-Go TF-IDF
// embedder (deterministic, no external process — safe in the auto-accept/settle hot paths)
// and requires their cosine similarity to reach minHighReuseCoherence. Embedding ONLY the
// context nouns (not the trigger cluster) is what defeats the trigger-stuffing bypass: an
// attacker who repeats "test pattern go" in the content still scores ~0 because the check
// never looks at the trigger words — only whether the CLAIMED domain nouns ("widget
// gadget") relate to the content.
//
// Fail-open when there is nothing to compare: an entry with no content (e.g. the
// content-less entries used in structural unit tests) or no context nouns is governed by
// the structural gates alone. Production puts always carry content, so the gate is active
// where the economic incentive is granted.
func descriptionContentCoherent(contextNouns []string, content []byte) bool {
	if len(contextNouns) == 0 || len(content) == 0 {
		return true
	}
	sample := content
	if len(sample) > coherenceContentSampleBytes {
		sample = sample[:coherenceContentSampleBytes]
	}
	emb := matching.NewTFIDFEmbedder()
	nounEmb := emb.Embed(strings.Join(contextNouns, " "))
	contentEmb := emb.Embed(string(sample))
	return emb.Similarity(nounEmb, contentEmb) >= minHighReuseCoherence
}

// tokensMatchAt reports whether the sub-slice of tokens beginning at index start
// equals seq.
func tokensMatchAt(tokens []string, start int, seq []string) bool {
	for i, w := range seq {
		if tokens[start+i] != w {
			return false
		}
	}
	return true
}

// encEnvelope is the §3.3 v2 content-confidentiality envelope carried in a
// 3401 put's "enc" object. Exactly one of Ciphertext / BlobPointer is present.
// The operator (and only the operator) unwraps KeyWrap.Wrapped to recover the
// CEK and AEAD-decrypts the ciphertext at fold time
// (docs/design/content-confidentiality-envelope-541.md §3.1(2), dontguess-4bed).
type encEnvelope struct {
	ContentAlg     string `json:"content_alg"`
	CiphertextHash string `json:"ciphertext_hash"`
	Ciphertext     string `json:"ciphertext"`   // base64(nonce||AEAD) — present IFF inline (≤32 KiB)
	BlobPointer    string `json:"blob_pointer"` // present IFF offloaded (>32 KiB, Phase 4)
	KeyWrap        struct {
		Alg       string `json:"alg"`
		Recipient string `json:"recipient"`
		Wrapped   string `json:"wrapped"`
	} `json:"key_wrap"`
}

// encWellFormed reports whether e carries every §6 required field with exactly
// one of ciphertext / blob_pointer set. A malformed envelope on team tier is
// fail-closed dropped (never folded), so a downgrade cannot reopen the leak.
func encWellFormed(e *encEnvelope) bool {
	if e == nil {
		return false
	}
	if e.ContentAlg == "" || e.CiphertextHash == "" {
		return false
	}
	if e.KeyWrap.Alg == "" || e.KeyWrap.Recipient == "" || e.KeyWrap.Wrapped == "" {
		return false
	}
	hasInline := e.Ciphertext != ""
	hasBlob := e.BlobPointer != ""
	return hasInline != hasBlob // exactly one
}

// isLegacyPlaintextPut reports whether a put payload is a genuine PRE-MIGRATION
// legacy plaintext put — v<2, no "enc" envelope, and a base64 "content" field —
// i.e. the historical wire shape that predates the §3.3 v2 confidential envelope.
//
// This is the NARROW class that Replay grandfathers under the team-tier
// encrypted-required cutover (§6.3, §7, dontguess-3ab1). It deliberately EXCLUDES:
//   - a v2-declared put (v>=2) carrying a smuggled plaintext "content" field — a
//     downgrade-smuggling attack, never a pre-migration entry;
//   - a put with a present-but-malformed "enc" envelope (Enc != nil) — a broken/
//     rogue v2 put, not legacy.
//
// Anything this predicate rejects stays fail-closed DROPPED in BOTH replay and
// live fold; only a true legacy plaintext put is ever grandfathered.
func isLegacyPlaintextPut(v int, enc *encEnvelope, content string) bool {
	return v < 2 && enc == nil && content != ""
}

// decryptV2Put unwraps the CEK to the operator and AEAD-opens the INLINE
// ciphertext, returning the recovered plaintext for the quality/dedup gates to
// run on (§3.1(2)/§3.6). Every failure path returns ok=false so applyPut DROPS
// the put — an undecryptable, tampered, or blob-only (Phase-4-deferred) team-
// tier put never folds. Fail-closed by construction: it can only ever yield
// verified plaintext or nothing. sellerPubHex is msg.Sender (the seller wrapped
// the CEK to the operator, so we open FROM the seller's key).
func (s *State) decryptV2Put(sellerPubHex string, enc *encEnvelope) (plaintext []byte, ok bool) {
	if s.operatorSigner == nil {
		return nil, false // no operator key to unwrap with — cannot gate, drop
	}
	if enc.ContentAlg != "chacha20poly1305" {
		return nil, false
	}
	// Phase 1 is inline-only: the buyer-side Blossom ciphertext fetch (and the
	// operator-side fetch to gate an offloaded put) is deferred (Phase 4,
	// dontguess-640). A blob-only put on team tier is well-formed but
	// undecryptable here → dropped, never leaked.
	if enc.Ciphertext == "" {
		return nil, false
	}
	ciphertext, err := base64.StdEncoding.DecodeString(enc.Ciphertext)
	if err != nil {
		return nil, false
	}
	// Verify ciphertext_hash == sha256(ciphertext) FIRST — reject a tampered
	// ciphertext before spending an ECDH + AEAD open on it (§3.1(6)/§4.4 A7).
	sum := sha256.Sum256(ciphertext)
	if "sha256:"+hex.EncodeToString(sum[:]) != enc.CiphertextHash {
		return nil, false
	}
	// Unwrap CEK = NIP-44.Open(operatorPriv, sellerPub, wrapped).
	cek, err := nip44.Open(s.operatorSigner, sellerPubHex, enc.KeyWrap.Wrapped)
	if err != nil {
		return nil, false
	}
	if len(cek) != chacha20poly1305.KeySize {
		return nil, false
	}
	aead, err := chacha20poly1305.New(cek)
	if err != nil {
		return nil, false
	}
	ns := aead.NonceSize()
	if len(ciphertext) < ns {
		return nil, false
	}
	nonce, sealed := ciphertext[:ns], ciphertext[ns:]
	pt, err := aead.Open(nil, nonce, sealed, nil)
	if err != nil {
		return nil, false
	}
	return pt, true
}

// applyPut processes an exchange:put message.
//
// It dispatches on the explicit "v" tag (§6.3, never implicit field sniffing):
// v>=2 with an "enc" object is the §3.3 confidential envelope — decode, decrypt
// to plaintext, then run ALL quality/dedup/plausibility gates on the DECRYPTED
// plaintext (the gates move INSIDE the operator's decrypt boundary, §3.6).
// Legacy plaintext (v<2 or no enc) keeps the base64 "content" path. On team
// tier (encryptedRequired) the fail-closed §6 guard DROPS any legacy-plaintext
// or malformed-enc put before it can fold — a downgrade cannot reopen the leak.
func (s *State) applyPut(msg *Message) {
	var payload struct {
		V               int          `json:"v"`
		Description     string       `json:"description"`
		Teaser          string       `json:"teaser"`  // seller-authored public abstract (§4.1, TAINTED)
		Content         string       `json:"content"` // base64-encoded plaintext (LEGACY / TAINTED)
		TokenCost       int64        `json:"token_cost"`
		ContentType     string       `json:"content_type"`
		Domains         []string     `json:"domains"`
		ContentSize     int64        `json:"content_size"`
		CompressionTier string       `json:"compression_tier"`
		Enc             *encEnvelope `json:"enc"`
	}
	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		return
	}

	isV2 := payload.V >= 2 && payload.Enc != nil

	// grandfathered marks a pre-migration LEGACY plaintext put that is being
	// GRANDFATHERED during Replay (set in the §6 fail-closed block below). It
	// carries the legacy marker + a bounded TTL onto the folded entry.
	grandfathered := false

	// §6 FAIL-CLOSED (team tier only): a downgrade must not reopen the content
	// leak. Any 3401 that carries a legacy plaintext "content" field, lacks a
	// well-formed v2 "enc" envelope, or has v<2 is NOT a well-formed confidential
	// put. What happens to it depends on replay-vs-live — the crux of this item
	// (§6.3, §7 Migration, dontguess-3ab1):
	//
	//   LIVE fold (dontguess-4bed): fail-closed DROP. A downgrade (a rogue or
	//   pre-upgrade allowlisted client publishing plaintext) must never inject new
	//   cleartext into inventory. Dropping = not folding into pendingPuts, so the
	//   operator's put-accept finds nothing (the same silent-drop pattern the
	//   tainted-field guards below use). This enforcement is UNCHANGED.
	//
	//   REPLAY of a MIXED historical log (s.replaying): a genuine pre-migration
	//   LEGACY plaintext put (isLegacyPlaintextPut: v<2, a base64 "content" field,
	//   no "enc") was already accepted+broadcast BEFORE the cutover. Dropping it on
	//   the rebuild path would make the operator lose ALL pre-migration inventory
	//   on the first restart, and the plaintext is already permanently public on
	//   the append-only relay (no claw-back is possible). GRANDFATHER it instead:
	//   fold it as legacy inventory carrying a bounded TTL (LegacyGrandfatherTTL)
	//   so it ages out gracefully rather than vanishing or lingering forever.
	//
	// The grandfather path is NARROW: only a true legacy plaintext put qualifies.
	// A v2-shaped put with a smuggled "content" field, or a present-but-malformed
	// "enc", is NOT a pre-migration entry and stays fail-closed DROPPED in BOTH
	// replay and live fold. Individual tier (encryptedRequired == false, local
	// socket, already confidential) keeps the legacy plaintext path legal and
	// unchanged — it never reaches this block.
	if s.encryptedRequired {
		if !isV2 || payload.Content != "" || !encWellFormed(payload.Enc) {
			if s.replaying && isLegacyPlaintextPut(payload.V, payload.Enc, payload.Content) {
				grandfathered = true
			} else {
				return
			}
		}
	}

	// Obtain the plaintext content bytes. v2 decrypts inside the operator
	// boundary; legacy decodes base64. Both converge on contentBytes, on which
	// every existing gate then runs.
	var (
		contentBytes       []byte
		wrappedCEKOperator string
		ciphertextHash     string
	)
	if isV2 {
		pt, ok := s.decryptV2Put(msg.Sender, payload.Enc)
		if !ok {
			return // undecryptable / tampered / blob-only → drop (fail-closed)
		}
		contentBytes = pt
		wrappedCEKOperator = payload.Enc.KeyWrap.Wrapped
		ciphertextHash = payload.Enc.CiphertextHash
	} else {
		// Legacy plaintext path: individual tier (unencrypted, unchanged) OR a
		// pre-migration legacy plaintext put GRANDFATHERED during Replay on team
		// tier (grandfathered == true). A LIVE team-tier plaintext put was already
		// fail-closed dropped above and never reaches here.
		if payload.Content == "" {
			return
		}
		// Pre-decode size guard: base64 expands ~4/3x, reject early to avoid heap allocation
		if len(payload.Content) > MaxContentBytes*4/3+4 {
			return
		}
		// Decode content from base64. Drop silently on decode failure.
		decoded, err := base64.StdEncoding.DecodeString(payload.Content)
		if err != nil {
			return
		}
		contentBytes = decoded
	}

	// Validate TAINTED fields. Drop silently — the message is already on the
	// campfire log; we cannot remove it. By not adding it to pendingPuts the
	// operator's put-accept will find nothing to accept.
	if len(payload.Description) > MaxDescriptionBytes {
		return
	}
	if len(payload.Domains) > MaxDomainsCount {
		return
	}
	if payload.TokenCost <= 0 || payload.TokenCost > MaxTokenCost {
		return
	}
	// Decrypted/decoded content is required. Reject puts with no content.
	if len(contentBytes) == 0 {
		return
	}
	// Enforce size limit on decoded content (TAINTED).
	if len(contentBytes) > MaxContentBytes {
		return
	}
	// Plausibility check: token_cost must be consistent with content size.
	// Token cost represents inference cost, not output size. However a genuine
	// result cannot require more than MaxTokensPerByte tokens per byte of output —
	// values beyond that threshold indicate seller inflation rather than real
	// computation. Gross outliers (e.g. 1.5 M tokens on a 200-byte payload at
	// 7500 tokens/byte) are dropped silently to prevent them from dominating the
	// reported token-savings metric.
	maxPlausibleTokenCost := int64(len(contentBytes)) * MaxTokensPerByte
	if maxPlausibleTokenCost < 1 {
		maxPlausibleTokenCost = 1
	}
	if payload.TokenCost > maxPlausibleTokenCost {
		return
	}
	// Quality gate §1 (dontguess-ed1): token_cost floor.
	// Puts below MinTokenCost tokens are rejected as low-value/synthetic.
	// Composition with 46f: 46f enforces the upper bound (token_cost ≤ content_size *
	// MaxTokensPerByte); this enforces the lower bound. Both apply independently.
	if payload.TokenCost < MinTokenCost {
		return
	}
	// Quality gate §3 (dontguess-ed1): test-like description rejection.
	// Reject the "test" and "upgrade smoke test" junk class identified in
	// measurement review §2. The "test" entry alone served 1,576 hits — 60% of
	// all real-agent buys were served this junk entry, poisoning match quality.
	if isTestLikeDescription(payload.Description) {
		return
	}
	// Teaser gate (content-confidentiality-envelope-541 §4.1, dontguess-4059).
	// The teaser is the seller-authored public abstract that settle(preview)
	// now echoes IN PLACE of the deleted real-content preview chunks. Two hard
	// constraints, both enforced here — at put-accept, where the operator has
	// already decrypted the plaintext (§3.1(2)):
	//
	//  (1) HARD-CAP (fail-closed drop): a teaser over MaxTeaserBytes is dropped
	//      like any other applyPut violation. Teaser bytes are intentionally-
	//      published, so the cap makes pasting whole content into the "teaser" a
	//      self-defeating dropped put rather than a leak vector.
	//
	//  (2) COHERENCE-CHECK (drop the teaser, keep the put): reuse the §4
	//      description-vs-content coherence gate to catch teaser/content
	//      bait-and-switch — a teaser whose claimed nouns are disconnected from
	//      the DECRYPTED plaintext (a lure unrelated to what is actually sold)
	//      that a buyer would otherwise only discover post-purchase. An
	//      incoherent teaser is dropped to "" (nothing misleading is ever
	//      echoed) while the content sale still proceeds; the crude embedder's
	//      false-negatives therefore cost at most a missing teaser, never a lost
	//      put. teaserCoherent fails OPEN when there is nothing to compare (an
	//      empty teaser, or a teaser with no content-bearing nouns).
	if len(payload.Teaser) > MaxTeaserBytes {
		return
	}
	validatedTeaser := payload.Teaser
	if validatedTeaser != "" && !teaserCoherent(validatedTeaser, contentBytes) {
		validatedTeaser = ""
	}
	// Validate compression_tier. Unknown values are silently dropped to "".
	tier := payload.CompressionTier
	if tier != "" {
		if _, ok := validCompressionTiers[tier]; !ok {
			tier = ""
		}
	}
	// Compute content hash from the decoded bytes. Never trust hash from payload.
	sum := sha256.Sum256(contentBytes)
	contentHash := "sha256:" + hex.EncodeToString(sum[:])
	// Quality gate §2 (dontguess-ed1): content-hash deduplication.
	// Reject puts whose content is already present in inventory or pendingPuts.
	// This prevents sellers from re-putting identical content under a new description
	// to bypass expiry, gain a pricing reset, or game the discovery ranking.
	if _, exists := s.contentHashIndex[contentHash]; exists {
		return
	}
	contentType := stripTagPrefix(payload.ContentType, "exchange:content-type:")

	// Blossom offload (dontguess-7783): oversize content must never be inlined
	// in the replicated entry / message log. When a blob store is configured
	// and the decoded content exceeds BlossomOffloadThreshold, upload the full
	// bytes to the blob store and keep only a precomputed inline preview slice
	// (15-25% of content, content-type-aware chunked — same algorithm used for
	// the buyer-facing preview) plus the pointer. The full-fidelity deliver
	// path fetches-and-verifies against contentHash (see FetchAndVerifyBlob).
	//
	// If no blob store is configured, or content is at/below the threshold,
	// behavior is unchanged from before this change: full bytes stored inline.
	// v2 confidential entries never take the legacy Blossom preview-slice
	// offload: the ciphertext (not a plaintext preview slice) is the public
	// artifact under encryption, and buildPutMessage caps inline v2 plaintext at
	// ≤32 KiB so this branch cannot trigger for it anyway. The legacy plaintext
	// (individual-tier) path keeps the existing offload behavior unchanged.
	entryContent := contentBytes
	blobPointer := ""
	if !isV2 && s.blobStore != nil && len(contentBytes) > BlossomOffloadThreshold {
		pointer, err := s.blobStore.Put(contentBytes)
		if err != nil {
			// Upload failed — drop the put. Do not fall back to inlining, and
			// do not register the content hash, so the seller may retry.
			return
		}
		// Offloaded: the full content lives ONLY in the Blossom blob (addressed
		// by ContentHash); entry.Content holds NOTHING. The old real-content
		// inline preview slice (buildInlinePreviewBytes) was deleted — it sliced
		// 15-25% of plaintext onto the entry that settle(preview) then broadcast
		// (content-confidentiality-envelope-541 §4.1, dontguess-4059). Delivery
		// is now a pointer + client-side ciphertext-hash verify (emitDeliverPointer),
		// and settle(preview) echoes the seller teaser, so no plaintext slice is
		// ever needed on any wire.
		entryContent = nil
		blobPointer = pointer
	}

	// Grandfathered legacy plaintext entries carry a bounded default expiry so the
	// pre-migration plaintext corpus drains from live inventory over time (§7).
	// A historical put-accept that carried an explicit expires_at still overrides
	// this in applySettlePutAccept; when it did not (empty expires_at → zero, i.e.
	// "never expires"), this TTL is what guarantees the entry ages out.
	var legacyExpiresAt time.Time
	if grandfathered {
		legacyExpiresAt = time.Unix(0, msg.Timestamp).Add(LegacyGrandfatherTTL)
	}

	entry := &InventoryEntry{
		EntryID:            msg.ID,
		PutMsgID:           msg.ID,
		SellerKey:          msg.Sender,
		Description:        payload.Description,
		ContentHash:        contentHash,
		ContentType:        contentType,
		Domains:            stripDomainPrefixes(payload.Domains),
		TokenCost:          payload.TokenCost,
		ContentSize:        int64(len(contentBytes)),
		PutTimestamp:       msg.Timestamp,
		CompressionTier:    tier,
		Content:            entryContent,
		BlobPointer:        blobPointer,
		WrappedCEKOperator: wrappedCEKOperator,
		CiphertextHash:     ciphertextHash,
		Teaser:             validatedTeaser,
		LegacyPlaintext:    grandfathered,
		ExpiresAt:          legacyExpiresAt,
	}
	s.pendingPuts[msg.ID] = entry
	s.putToEntry[msg.ID] = msg.ID
	// Register content hash in the dedup index so subsequent puts with identical
	// content are rejected (quality gate §2). The hash persists even after the put
	// is accepted into inventory (the inventory entry retains the same hash).
	// Not removed on reject — prevents immediate re-put of identical rejected content.
	s.contentHashIndex[contentHash] = struct{}{}
}

// teaserCoherent reports whether a seller-authored teaser is semantically
// coherent with the DECRYPTED plaintext content, reusing the §4 description-vs-
// content coherence gate (descriptionContentCoherent, dontguess-5f5). It
// tokenizes the teaser, keeps only its content-bearing nouns
// (isContentBearingToken — the same filter Gate 5 applies to descriptions), and
// requires their cosine similarity to the content to clear minHighReuseCoherence.
//
// Purpose (content-confidentiality-envelope-541 §4.1, dontguess-4059): the
// operator decrypts at put-accept, so it can catch a teaser/content bait-and-
// switch — a teaser whose claimed domain nouns are disconnected from what is
// actually sold — that a buyer would otherwise only discover after paying.
//
// Fails OPEN (returns true) when there is nothing to compare: an empty teaser,
// a teaser with no content-bearing nouns (e.g. very short), or empty content.
// A genuine abstract names domain nouns that appear in / relate to the content
// and passes; a lure whose padding nouns are absent from the content scores ~0
// and is caught.
func teaserCoherent(teaser string, content []byte) bool {
	nouns := make([]string, 0)
	for _, tok := range tokenizeDescription(teaser) {
		if isContentBearingToken(tok) {
			nouns = append(nouns, tok)
		}
	}
	return descriptionContentCoherent(nouns, content)
}

// stripTagPrefix removes a convention tag prefix from a value if present.
// Convention dispatch sends full tag form (e.g. "exchange:content-type:analysis")
// where the engine expects bare enum values ("analysis"). Accept both.
func stripTagPrefix(val, prefix string) string {
	if strings.HasPrefix(val, prefix) {
		return val[len(prefix):]
	}
	return val
}

// stripDomainPrefixes normalizes domain values, stripping "exchange:domain:"
// prefix if convention dispatch sent the full tag form.
func stripDomainPrefixes(domains []string) []string {
	out := make([]string, len(domains))
	for i, d := range domains {
		out[i] = stripTagPrefix(d, "exchange:domain:")
	}
	return out
}
