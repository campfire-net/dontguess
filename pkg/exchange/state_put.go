package exchange

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// validCompressionTiers is the set of accepted compression_tier values.
// The empty string (unset) is also valid and means no tier preference.
var validCompressionTiers = map[string]struct{}{
	"hot":  {},
	"warm": {},
	"cold": {},
}

// exchangeOp returns the exchange operation tag from a message's tag list,
// or "" if none is present.
func exchangeOp(tags []string) string {
	for _, t := range tags {
		switch t {
		case TagPut, TagBuy, TagMatch, TagSettle,
			TagAssign, TagAssignClaim, TagAssignComplete, TagAssignAccept, TagAssignReject,
			TagAssignExpire, TagAssignAuctionClose:
			return t
		}
	}
	return ""
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

// minHighReuseWords is the minimum number of description tokens an entry must have
// to qualify as a §4 high-reuse artifact. Genuine distilled artifacts are *described*
// (a noun phrase naming the thing, plus its context) — every §4 exemplar and every
// positive case in put_reuse_class_test.go is ≥5 tokens. Crafted keyword-stuffs that
// game the classifier ("test pattern go idiom", "checklist schema validation",
// "readme setup guide") are ≤4 tokens: a bare concatenation of the classifier's own
// trigger words with no surrounding content. Requiring ≥5 tokens is a structural
// floor, not a blocklist — it rejects the *shape* of a keyword-stuff (all trigger,
// no description) regardless of which specific words are used.
const minHighReuseWords = 5

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
// Gates 2 and 4 attack the *structure* of a keyword-stuff (all-trigger / incidental
// co-signal) rather than enumerating bad strings, so they generalize to crafted inputs
// not seen at design time.
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
				for _, sig := range cls.coSignals {
					if tokens[i] == sig {
						return true
					}
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
	entry := &InventoryEntry{
		EntryID:         msg.ID,
		PutMsgID:        msg.ID,
		SellerKey:       msg.Sender,
		Description:     payload.Description,
		ContentHash:     contentHash,
		ContentType:     stripTagPrefix(payload.ContentType, "exchange:content-type:"),
		Domains:         stripDomainPrefixes(payload.Domains),
		TokenCost:       payload.TokenCost,
		ContentSize:     int64(len(contentBytes)),
		PutTimestamp:    msg.Timestamp,
		CompressionTier: tier,
		Content:         contentBytes,
	}
	s.pendingPuts[msg.ID] = entry
	s.putToEntry[msg.ID] = msg.ID
	// Register content hash in the dedup index so subsequent puts with identical
	// content are rejected (quality gate §2). The hash persists even after the put
	// is accepted into inventory (the inventory entry retains the same hash).
	// Not removed on reject — prevents immediate re-put of identical rejected content.
	s.contentHashIndex[contentHash] = struct{}{}
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
