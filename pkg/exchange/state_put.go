package exchange

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/campfire-net/dontguess/pkg/scrip"
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
	// Gate 1: content_type filter.
	// High-reuse artifacts are code, analysis, or summary. Review, data, and other
	// types carry session-specific content that rarely generalizes across projects.
	switch entry.ContentType {
	case "code", "analysis", "summary":
		// passes gate 1 — continue
	default:
		return false
	}

	tokens := tokenizeDescription(entry.Description)

	// Gate 2: length floor. Keyword-stuffs are short by nature — they are the trigger
	// words and nothing else. Genuine artifacts carry descriptive context.
	if len(tokens) < minHighReuseWords {
		return false
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
				context := 0
				for j, tok := range tokens {
					if j >= pStart && j <= pEnd { // skip primary tokens
						continue
					}
					if j == i { // skip the matched co-signal
						continue
					}
					if isContentBearingToken(tok) {
						context++
					}
				}
				if context >= minHighReuseContextWords {
					return true
				}
			}
		}
	}
	return false
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

// applyPut processes an exchange:put message.
func (s *State) applyPut(msg *Message) {
	var payload struct {
		Description     string   `json:"description"`
		Content         string   `json:"content"` // base64-encoded content bytes (TAINTED)
		TokenCost       int64    `json:"token_cost"`
		ContentType     string   `json:"content_type"`
		Domains         []string `json:"domains"`
		ContentSize     int64    `json:"content_size"`
		CompressionTier string   `json:"compression_tier"`
	}
	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		return
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
	// Content is required. Reject puts with no content.
	if payload.Content == "" {
		return
	}
	// Pre-decode size guard: base64 expands ~4/3x, reject early to avoid heap allocation
	if len(payload.Content) > MaxContentBytes*4/3+4 {
		return
	}
	// Decode content from base64. Drop silently on decode failure.
	contentBytes, err := base64.StdEncoding.DecodeString(payload.Content)
	if err != nil {
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
	entryContent := contentBytes
	blobPointer := ""
	if s.blobStore != nil && len(contentBytes) > BlossomOffloadThreshold {
		previewBytes, err := buildInlinePreviewBytes(contentBytes, contentType, msg.ID)
		if err != nil {
			// Cannot produce a safe inline preview — drop the put rather than
			// inline the oversize content as a fallback (hard constraint: never
			// inline oversize content on the relay).
			return
		}
		pointer, err := s.blobStore.Put(contentBytes)
		if err != nil {
			// Upload failed — drop the put. Do not fall back to inlining, and
			// do not register the content hash, so the seller may retry.
			return
		}
		entryContent = previewBytes
		blobPointer = pointer
	}

	entry := &InventoryEntry{
		EntryID:         msg.ID,
		PutMsgID:        msg.ID,
		SellerKey:       msg.Sender,
		Description:     payload.Description,
		ContentHash:     contentHash,
		ContentType:     contentType,
		Domains:         stripDomainPrefixes(payload.Domains),
		TokenCost:       payload.TokenCost,
		ContentSize:     int64(len(contentBytes)),
		PutTimestamp:    msg.Timestamp,
		CompressionTier: tier,
		Content:         entryContent,
		BlobPointer:     blobPointer,
	}
	s.pendingPuts[msg.ID] = entry
	s.putToEntry[msg.ID] = msg.ID
	// Register content hash in the dedup index so subsequent puts with identical
	// content are rejected (quality gate §2). The hash persists even after the put
	// is accepted into inventory (the inventory entry retains the same hash).
	// Not removed on reject — prevents immediate re-put of identical rejected content.
	s.contentHashIndex[contentHash] = struct{}{}
}

// buildInlinePreviewBytes computes the buyer-facing preview chunks for content
// (same algorithm PreviewAssembler uses at settle(preview) time) and
// concatenates them into a single inline byte slice. Used at put time to
// derive the inline preview that stays with the entry when the full content
// is offloaded to Blossom (dontguess-7783).
func buildInlinePreviewBytes(content []byte, contentType, entryID string) ([]byte, error) {
	pa := &PreviewAssembler{}
	result, err := pa.Assemble(PreviewRequest{
		Content:     content,
		ContentType: contentType,
		EntryID:     entryID,
	})
	if err != nil {
		return nil, err
	}
	var buf strings.Builder
	for _, c := range result.Chunks {
		buf.WriteString(c.Content)
	}
	return []byte(buf.String()), nil
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
