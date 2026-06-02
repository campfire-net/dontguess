package exchange

import (
	"encoding/json"

	"github.com/campfire-net/dontguess/pkg/matching"
)

// HitRateOptions configures the quality-weighted hit-rate computation.
// The zero value is valid: no quality filter is applied (backward-compatible
// with pre-M-rebaseline callers that don't pass options).
type HitRateOptions struct {
	// MinSimilarity is the cosine similarity floor for a delivered result to
	// count as a HIT. Delivered match results whose top entry's similarity is
	// below this threshold are reclassified as MISSES.
	//
	// Reference: matching.RankOptions.MinSimilarity default (M1a, dontguess-7d6).
	// Use matching.DefaultMinSimilarity() to get the current floor constant;
	// do NOT hardcode 0.16 — the floor constant may be adjusted without updating
	// this caller.
	//
	// When 0, no similarity floor is applied (legacy behaviour: any delivered
	// result counts as a hit regardless of quality).
	MinSimilarity float64

	// Embedder is the embedding engine used to recompute similarity for
	// historical match-result messages that lack the "similarity" field (produced
	// before M2/dontguess-b26). When non-nil, absent-similarity hits have their
	// top result's similarity recomputed against the originating buy task.
	//
	// When nil, absent-similarity hits are treated as unverifiable and counted in
	// UnverifiableHits (not as hits in HitRatePct). This is honest: we do not
	// fabricate a quality rating for matches we cannot inspect.
	Embedder matching.Embedder

	// BuyTasks maps buy message IDs to their task text. Required when Embedder is
	// non-nil: the recompute path re-embeds the buy task against each delivered
	// entry's description. When the buy task is absent for a given buy ID, that
	// match falls back to UnverifiableHits.
	BuyTasks map[string]string

	// EntryBuyerMap is the merged cross-seller map of distinct buyer keys per
	// inventory entry. Key: entryID, Value: set of buyer keys that have completed
	// a purchase of that entry. Used to compute CrossAgentConvergence.
	//
	// Build this from exchange State using BuildConvergenceMap(state) before
	// calling ComputeHitRate. When nil, CrossAgentConvergence is always 0.
	//
	// The heritage trust signal: an entry where len(buyers) >= 3 has been
	// independently validated by 3+ distinct agents — the ungameable convergence
	// signal from the toolrank lineage (docs/heritage/). §4.6 of
	// docs/design/exchange-per-agent-identity-decision.md.
	EntryBuyerMap map[string]map[string]struct{}

	// ConsumeCountByEntry maps entry_id → number of exchange:consume signals
	// for that entry. Used to compute net-savings economics (Track C, dontguess-eff):
	// a hit counts as "saved_on_real_hits" only when the delivered entry was
	// consumed (settled-complete). Un-consumed delivered entries count as
	// false-positive waste.
	//
	// Build this from exchange consume messages using ConsumeCountByEntry(consumes)
	// before calling ComputeHitRate. When nil, all above-floor hits are treated as
	// consumed (net-savings = token_cost saved; false_positive_waste = 0).
	//
	// The consume signal is the authoritative "buyer used it" behavioral signal —
	// stronger than a hit (which only means the matcher returned a candidate).
	// See TagConsume, emitConsumeSignal in engine.go, and §2 of
	// docs/design/exchange-token-savings-v06.md.
	ConsumeCountByEntry map[string]int

	// MissCostPerQuery is the token overhead charged per buy that returns no
	// usable match. Default (when 0) is DefaultMissCostPerQuery (500 tokens),
	// the ~500-token overhead of a buy round-trip per §1 of
	// docs/design/exchange-token-savings-v06.md. Override in tests or when the
	// actual measured overhead differs.
	MissCostPerQuery int64
}

// HitRateReport is the result of reconciling buy orders against match-results.
//
// Buy matching is asynchronous: a `dontguess buy` returns "dispatched" before
// any match exists, so hit-vs-miss is NOT knowable at the wrapper. It IS
// recoverable from the exchange message log by joining match-result messages
// back to their originating buy order (via the match's first antecedent, which
// the engine always sets to the buy message ID — see engine.go handleBuy /
// handleBuyMiss, both call sendOperatorMessage with antecedents=[]string{msg.ID}).
type HitRateReport struct {
	// TotalBuys is the number of distinct non-synthetic buy orders seen in the window.
	// Synthetic buys (whose match responses carry exchange:synthetic) are excluded.
	TotalBuys int `json:"total_buys"`

	// MatchedBuys is the number of distinct non-synthetic buy orders that received
	// at least one match-result (hit or miss).
	MatchedBuys int `json:"matched_buys"`

	// PendingBuys is buys with no corresponding match-result yet (still
	// dispatched / in flight). TotalBuys = MatchedBuys + PendingBuys.
	PendingBuys int `json:"pending_buys"`

	// Hits is the number of distinct buy orders whose best outcome was a quality-
	// weighted HIT: at least one match-result delivered cached inventory AND the
	// top result's similarity met or exceeded the MinSimilarity floor (when
	// HitRateOptions.MinSimilarity > 0). Below-floor delivered results do NOT
	// count as hits — they are reclassified as misses.
	//
	// When MinSimilarity is 0 (legacy / no options passed), any delivered result
	// counts as a hit (backward-compatible behaviour).
	Hits int `json:"hits"`

	// Misses is the number of distinct buy orders whose only outcome was a
	// buy-miss standing offer ("No cached inference matched"), OR whose delivered
	// results all fell below the similarity floor (quality-weighted).
	Misses int `json:"misses"`

	// BelowFloorDowngraded is the number of delivered match results (hits in the
	// legacy sense) that were reclassified as MISSES because the top result's
	// similarity was below MinSimilarity. These were previously counted as hits
	// and are the primary source of the inflated 96.67% rate.
	BelowFloorDowngraded int `json:"below_floor_downgraded"`

	// RecomputedSimilarity is the number of historical match-result messages
	// (produced before M2/dontguess-b26) that lacked a "similarity" field in
	// their payload and had similarity recomputed via the Embedder. These are
	// then subject to the same MinSimilarity floor gate.
	RecomputedSimilarity int `json:"recomputed_similarity"`

	// UnverifiableHits is the number of delivered match results whose similarity
	// could not be verified (no similarity field AND no Embedder / BuyTask to
	// recompute). These are reported separately and excluded from HitRatePct.
	// A non-zero value here indicates the reporter was run without recompute support
	// against historical pre-M2 data.
	UnverifiableHits int `json:"unverifiable_hits"`

	// HitRatePct is Hits / (Hits + Misses) * 100, rounded to 2 decimals. It is the
	// quality-weighted hit-rate: hits that passed the similarity floor over all
	// answered (non-pending, non-synthetic, verifiable) buys. When MinSimilarity
	// is 0, this equals the legacy unfiltered rate. Unverifiable hits are excluded
	// from both numerator and denominator (their quality is unknown).
	//
	// When MatchedBuys is zero (or all matched buys are unverifiable), HitRatePct is 0.
	HitRatePct float64 `json:"hit_rate_pct"`

	// MatchResultsTotal is the raw number of non-synthetic match-result messages
	// in the window (a single buy can receive more than one). Reported for
	// reconciliation against `dontguess match-results --json | jq length`.
	MatchResultsTotal int `json:"match_results_total"`

	// UnjoinableMatchResults is match-results whose antecedent buy order was not
	// present in the buy set in the window (e.g. the buys view was paged
	// differently, or the buy fell outside --since while its match did not).
	// These are counted but cannot be attributed to a buy, so they do not move
	// hit/miss totals.
	UnjoinableMatchResults int `json:"unjoinable_match_results"`

	// SyntheticExcluded is the number of match-result messages that were skipped
	// because they were tagged exchange:synthetic (load-test / probe traffic).
	// The corresponding buy orders are also excluded from TotalBuys and all counts.
	SyntheticExcluded int `json:"synthetic_excluded"`

	// CrossAgentConvergence is the number of inventory entries that have achieved
	// cross-agent convergence: entries where 3 or more DISTINCT buyer agent keys
	// have completed a purchase. This is the heritage "ungameable trust signal" from
	// the toolrank lineage — an entry that 3+ independent agents bought and used is
	// reliably valuable. The count is 0 when all buys arrive from a single shared
	// identity (current default), and rises as agents adopt distinct identities via
	// per-agent AGENT_CF_HOME (dontguess-a99/04f). See §4.6 of
	// docs/design/exchange-per-agent-identity-decision.md.
	//
	// Sourced from opts.EntryBuyerMap (populated by BuildConvergenceMap). When
	// opts.EntryBuyerMap is nil, CrossAgentConvergence is always 0.
	CrossAgentConvergence int `json:"cross_agent_convergence"`

	// --- Net token-savings economics (Track C, dontguess-eff) ---
	// Per docs/design/exchange-token-savings-v06.md §1:
	//   net_tokens_saved = saved_on_real_hits − miss_costs − false_positive_waste

	// NetTokensSaved is the overall net tokens saved across all buys in the window:
	//   SavedOnRealHits − TotalMissCost − TotalFalsePositiveWaste
	//
	// Positive = the exchange is saving tokens on net. Negative = more waste than value.
	// This is the primary v0.6 optimization target ("escape velocity").
	NetTokensSaved int64 `json:"net_tokens_saved"`

	// SavedOnRealHits is the sum of token_cost_original for above-floor hits whose
	// top delivered entry was subsequently consumed (settle-complete). These are the
	// buys where the exchange provably saved the buyer from re-deriving.
	//
	// When opts.ConsumeCountByEntry is nil, all above-floor hits are treated as
	// consumed (conservative assumption: every hit saved the buyer).
	SavedOnRealHits int64 `json:"saved_on_real_hits"`

	// TotalMissCost is the total token overhead for all miss buys:
	//   Misses × MissCostPerQuery (default: 500 per §1)
	//
	// Each miss costs the buyer ~500 tokens in round-trip overhead with no value
	// delivered. This is the recurring tax paid for a low hit rate.
	TotalMissCost int64 `json:"total_miss_cost"`

	// TotalFalsePositiveWaste is the sum of token_cost_original for above-floor
	// hits whose top delivered entry was NOT consumed. These buyers received a
	// result, paid for it (or at minimum, read it), but re-derived anyway.
	//
	// Per §1: "A false positive is worse than a miss." A false positive consumes
	// buyer tokens reading irrelevant content AND requires re-derivation. This is
	// the waste the relevance floor was designed to eliminate — but it still occurs
	// when above-floor matches are semantically adjacent but not actually useful.
	//
	// Zero when opts.ConsumeCountByEntry is nil (consume signal unavailable).
	TotalFalsePositiveWaste int64 `json:"total_false_positive_waste"`

	// PerQueryEconomics is the per-buy breakdown used for A/B'ing floor/embedding/
	// ranking changes against real net savings. Each element corresponds to one
	// non-synthetic, answered buy order.
	//
	// Ordered by outcome severity: hits first, then false-positives, then misses.
	// Callers may sort/filter by Saved to identify which queries are the largest
	// contributors or detractors.
	//
	// Empty (not nil) when there are no answered buys.
	PerQueryEconomics []QueryEconomics `json:"per_query_economics"`
}

// DefaultMissCostPerQuery is the token overhead assumed per buy that returns no
// usable match. Per §1 of docs/design/exchange-token-savings-v06.md: "miss_costs
// — the ~500-token overhead of a buy that returns no usable match."
//
// This is the round-trip cost a buyer pays when there is no cache hit: the tokens
// spent on the buy request, routing, and processing a miss response. Override via
// HitRateOptions.MissCostPerQuery if the measured overhead differs.
const DefaultMissCostPerQuery = int64(500)

// QueryEconomics captures the token-savings economics for a single buy order.
// Used to surface per-query A/B signals when evaluating floor/embedding/ranking
// changes (Track C, dontguess-eff). See docs/design/exchange-token-savings-v06.md §1.
type QueryEconomics struct {
	// BuyID is the buy message ID this record belongs to.
	BuyID string `json:"buy_id"`

	// Outcome is the final classification of this buy: "hit", "miss",
	// "false_positive" (delivered above-floor but not consumed), or "pending".
	Outcome string `json:"outcome"`

	// TokenCostOriginal is the token_cost of the top delivered entry, or 0 for
	// misses and pending buys. This is the tokens the buyer WOULD have spent if
	// they had re-derived the result themselves — the potential saving.
	TokenCostOriginal int64 `json:"token_cost_original,omitempty"`

	// Saved is the net tokens saved by this query:
	//   hit (consumed):     +token_cost_original
	//   miss:               −miss_cost_per_query
	//   false_positive:     −false_positive_waste (token_cost_original re-derive cost)
	//   pending:            0
	Saved int64 `json:"saved"`
}

// isMessageSynthetic reports whether a message carries the exchange:synthetic tag.
// Used to identify load-test / probe traffic at the metric-filter boundary.
// Tagging is done by the engine at response-emit time (handleBuy / handleBuyMiss /
// handlePut) when demand.IsSynthetic matches the buy task or put description.
func isMessageSynthetic(m *Message) bool {
	for _, t := range m.Tags {
		if t == TagSynthetic {
			return true
		}
	}
	return false
}

// classifyMatchResult reports whether a single exchange:match message is a HIT
// or a MISS. The discriminator is grounded in the code that WRITES these
// messages (pkg/exchange/engine.go):
//
//   - MISS (handleBuyMiss): tags = [exchange:buy-miss, exchange:match]; payload
//     carries top-level "buy_msg_id" + "task_hash" and the "No cached inference
//     matched" guide; it has NO "results" array.
//   - HIT (handleBuy match path): tags = [exchange:match]; payload carries a
//     non-empty "results" array of matched inventory entries; no top-level
//     "buy_msg_id".
//
// We classify primarily by the exchange:buy-miss tag (the authoritative signal
// the engine sets), and fall back to payload shape (presence of a non-empty
// results array) so the classifier is robust if tag composition ever changes.
//
// This function does NOT apply a similarity floor — it only determines whether
// inventory was delivered. Quality gating is done in ComputeHitRate via
// qualityGateMatch.
func classifyMatchResult(m *Message) (isHit bool) {
	for _, t := range m.Tags {
		if t == TagBuyMiss {
			return false
		}
	}
	// No buy-miss tag: confirm it carries delivered results.
	var p struct {
		Results  []json.RawMessage `json:"results"`
		BuyMsgID string            `json:"buy_msg_id"`
	}
	if err := json.Unmarshal(m.Payload, &p); err != nil {
		// Unparseable payload but no buy-miss tag — treat as a hit-shaped
		// match (it was emitted on the match path, not the miss path).
		return true
	}
	if p.BuyMsgID != "" {
		// Top-level buy_msg_id is the buy-miss payload shape (defensive: a
		// miss that somehow lost its tag).
		return false
	}
	return len(p.Results) > 0
}

// matchTopSimilarity extracts the "similarity" field from the top result in a
// hit match-result payload. Returns (similarity, true) when the field is present,
// or (0, false) when absent (pre-M2 historical data).
//
// The "top result" is results[0] — the engine emits results sorted by composite
// score descending, so results[0] is always the highest-ranked entry.
func matchTopSimilarity(m *Message) (float64, bool) {
	var p struct {
		Results []struct {
			Similarity *float64 `json:"similarity"`
		} `json:"results"`
	}
	if err := json.Unmarshal(m.Payload, &p); err != nil || len(p.Results) == 0 {
		return 0, false
	}
	if p.Results[0].Similarity == nil {
		return 0, false
	}
	return *p.Results[0].Similarity, true
}

// matchTopTokenCost extracts the "token_cost_original" field of the top result
// entry from a hit match-result payload. Returns 0 when absent or unparseable.
//
// token_cost_original is the seller's declared token cost for the entry —
// approximately the tokens the buyer would spend re-deriving it from scratch.
// It is the basis for saved_on_real_hits and false_positive_waste calculations.
// See MatchResult.TokenCostOriginal in engine.go.
func matchTopTokenCost(m *Message) int64 {
	var p struct {
		Results []struct {
			TokenCostOriginal int64 `json:"token_cost_original"`
		} `json:"results"`
	}
	if err := json.Unmarshal(m.Payload, &p); err != nil || len(p.Results) == 0 {
		return 0
	}
	return p.Results[0].TokenCostOriginal
}

// matchTopEntryID extracts the "entry_id" field of the top result entry from a
// hit match-result payload. Returns empty string when absent or unparseable.
// Used to look up whether the buyer consumed the delivered entry (consume signal).
func matchTopEntryID(m *Message) string {
	var p struct {
		Results []struct {
			EntryID string `json:"entry_id"`
		} `json:"results"`
	}
	if err := json.Unmarshal(m.Payload, &p); err != nil || len(p.Results) == 0 {
		return ""
	}
	return p.Results[0].EntryID
}

// matchTopDescription extracts the "description" field of the top result entry
// from a hit match-result payload. Used for recompute when the similarity field
// is absent (pre-M2 historical messages).
func matchTopDescription(m *Message) string {
	var p struct {
		Results []struct {
			Description string `json:"description"`
		} `json:"results"`
	}
	if err := json.Unmarshal(m.Payload, &p); err != nil || len(p.Results) == 0 {
		return ""
	}
	return p.Results[0].Description
}

// qualityGateOutcome is the result of applying the similarity floor to a delivered
// match result.
type qualityGateOutcome int

const (
	// qualityGateHit: similarity met the floor → count as hit.
	qualityGateHit qualityGateOutcome = iota
	// qualityGateMiss: similarity present but below floor → downgraded to miss.
	qualityGateMiss
	// qualityGateRecomputedHit: similarity absent (pre-M2), recomputed → above floor.
	qualityGateRecomputedHit
	// qualityGateRecomputedMiss: similarity absent (pre-M2), recomputed → below floor.
	qualityGateRecomputedMiss
	// qualityGateUnverifiable: similarity absent and cannot be recomputed.
	qualityGateUnverifiable
)

// qualityGateMatch applies the similarity floor to a delivered hit match-result.
// When opts.MinSimilarity == 0, always returns qualityGateHit (no floor gate).
// When opts.Embedder is non-nil and the similarity field is absent, similarity
// is recomputed from the buy task and the top result's entry description.
func qualityGateMatch(m *Message, buyTask string, opts HitRateOptions) qualityGateOutcome {
	if opts.MinSimilarity <= 0 {
		// No floor configured — legacy behaviour.
		return qualityGateHit
	}

	sim, present := matchTopSimilarity(m)
	if present {
		// M2 data: similarity field is in the payload. Gate directly.
		if sim >= opts.MinSimilarity {
			return qualityGateHit
		}
		return qualityGateMiss
	}

	// Pre-M2 historical data: no similarity field. Attempt recompute.
	if opts.Embedder != nil && buyTask != "" {
		desc := matchTopDescription(m)
		if desc != "" {
			taskEmb := opts.Embedder.Embed(buyTask)
			entryEmb := opts.Embedder.Embed(desc)
			recomputedSim := opts.Embedder.Similarity(taskEmb, entryEmb)
			if recomputedSim >= opts.MinSimilarity {
				return qualityGateRecomputedHit
			}
			return qualityGateRecomputedMiss
		}
	}

	// Cannot verify quality — report as unverifiable.
	return qualityGateUnverifiable
}

// buyMsgIDFor returns the buy order ID a match-result belongs to. The engine
// always sets the buy message ID as the first antecedent on both the hit and
// miss paths. The miss payload also carries it as "buy_msg_id"; we fall back to
// that if antecedents are missing.
func buyMsgIDFor(m *Message) string {
	if len(m.Antecedents) > 0 && m.Antecedents[0] != "" {
		return m.Antecedents[0]
	}
	var p struct {
		BuyMsgID string `json:"buy_msg_id"`
	}
	if err := json.Unmarshal(m.Payload, &p); err == nil {
		return p.BuyMsgID
	}
	return ""
}

// buyOutcome is the best-quality match result seen for a single buy order.
// Populated by classifyBuyOutcomes; consumed by tallyOutcomes and computeNetSavings.
type buyOutcome struct {
	hit          bool   // true once a hit (above-floor) result is seen
	unverifiable bool   // true if best outcome so far is unverifiable (no verified hit yet)
	miss         bool   // true if at least one result was seen (and none were hits)
	topEntryID   string // entry_id of the top delivered result (for consume lookup)
	topTokenCost int64  // token_cost_original of the top delivered result
}

// excludeSyntheticBuys scans matches for exchange:synthetic-tagged responses,
// counts them in syntheticExcluded, and returns the set of real (non-synthetic)
// buy IDs derived from buys filtered against the synthetic set.
//
// The engine tags the *response* (match/buy-miss) with exchange:synthetic when
// demand.IsSynthetic(task) is true. The originating buy message carries no such
// tag — so we identify synthetic buys via their tagged response.
func excludeSyntheticBuys(buys, matches []Message) (buyIDs map[string]struct{}, syntheticExcluded int) {
	syntheticBuyIDs := make(map[string]struct{})
	for i := range matches {
		m := &matches[i]
		if !isMessageSynthetic(m) {
			continue
		}
		syntheticExcluded++
		if buyID := buyMsgIDFor(m); buyID != "" {
			syntheticBuyIDs[buyID] = struct{}{}
		}
	}

	buyIDs = make(map[string]struct{}, len(buys))
	for i := range buys {
		id := buys[i].ID
		if _, isSynthetic := syntheticBuyIDs[id]; !isSynthetic {
			buyIDs[id] = struct{}{}
		}
	}
	return buyIDs, syntheticExcluded
}

// classifyBuyOutcomes iterates the non-synthetic match-result messages and
// assigns the best quality-gate outcome to each real buy order.
//
// The ranking is:
//
//	qualityGateHit > qualityGateRecomputedHit > qualityGateUnverifiable >
//	qualityGateRecomputedMiss > qualityGateMiss
//
// Returns outcomeByBuy (best outcome per buy), plus raw counters for
// realMatchResults, unjoinable, belowFloorDowngraded, and recomputedSimilarity.
func classifyBuyOutcomes(matches []Message, buyIDs map[string]struct{}, o HitRateOptions) (
	outcomeByBuy map[string]*buyOutcome,
	realMatchResults, unjoinable, belowFloorDowngraded, recomputedSimilarity int,
) {
	outcomeByBuy = make(map[string]*buyOutcome)

	for i := range matches {
		m := &matches[i]
		// Skip synthetic responses — already counted in excludeSyntheticBuys.
		if isMessageSynthetic(m) {
			continue
		}
		realMatchResults++
		buyID := buyMsgIDFor(m)
		if buyID == "" {
			unjoinable++
			continue
		}
		if _, ok := buyIDs[buyID]; !ok {
			// Match-result references a buy we don't have in this window.
			unjoinable++
			continue
		}

		if outcomeByBuy[buyID] == nil {
			outcomeByBuy[buyID] = &buyOutcome{}
		}
		bo := outcomeByBuy[buyID]

		// If already confirmed a verified hit, skip further classification.
		if bo.hit {
			continue
		}

		isDelivered := classifyMatchResult(m)
		if !isDelivered {
			// Exchange:buy-miss → a real miss.
			bo.miss = true
			continue
		}

		// Delivered result: apply quality gate.
		buyTask := ""
		if o.BuyTasks != nil {
			buyTask = o.BuyTasks[buyID]
		}
		outcome := qualityGateMatch(m, buyTask, o)
		switch outcome {
		case qualityGateHit:
			bo.hit = true
			// Record the top delivered entry for consume-signal lookup.
			bo.topEntryID = matchTopEntryID(m)
			bo.topTokenCost = matchTopTokenCost(m)
		case qualityGateRecomputedHit:
			bo.hit = true
			recomputedSimilarity++
			bo.topEntryID = matchTopEntryID(m)
			bo.topTokenCost = matchTopTokenCost(m)
		case qualityGateRecomputedMiss:
			recomputedSimilarity++
			belowFloorDowngraded++
			bo.miss = true
		case qualityGateMiss:
			belowFloorDowngraded++
			bo.miss = true
		case qualityGateUnverifiable:
			bo.unverifiable = true
		}
	}
	return outcomeByBuy, realMatchResults, unjoinable, belowFloorDowngraded, recomputedSimilarity
}

// tallyOutcomes counts hits, misses, and unverifiable hits from outcomeByBuy, and
// computes the quality-weighted HitRatePct (unverifiable hits excluded from both
// numerator and denominator).
func tallyOutcomes(outcomeByBuy map[string]*buyOutcome) (hits, misses, unverifiableHits int, hitRatePct float64) {
	for _, bo := range outcomeByBuy {
		switch {
		case bo.hit:
			hits++
		case bo.unverifiable && !bo.miss:
			// Best outcome is unverifiable (no below-floor or miss result seen).
			unverifiableHits++
		default:
			misses++
		}
	}

	// HitRatePct denominator: exclude unverifiable hits (unknown quality).
	// Only count verified hits and verified misses.
	verifiedAnswered := hits + misses
	if verifiedAnswered > 0 {
		hitRatePct = round2(float64(hits) / float64(verifiedAnswered) * 100)
	}
	return hits, misses, unverifiableHits, hitRatePct
}

// computeConvergence counts inventory entries that have achieved cross-agent
// convergence: entries where 3 or more DISTINCT buyer agent keys have completed
// a purchase. The heritage ungameable trust signal (toolrank lineage, §4.6
// docs/design/exchange-per-agent-identity-decision.md). Always 0 when
// entryBuyerMap is nil (current default: single shared identity).
func computeConvergence(entryBuyerMap map[string]map[string]struct{}) (crossAgentConvergence int) {
	for _, buyers := range entryBuyerMap {
		if len(buyers) >= 3 {
			crossAgentConvergence++
		}
	}
	return crossAgentConvergence
}

// computeNetSavings builds per-query economics and net token-savings totals from
// the best outcome per buy.
//
// Per §1 of docs/design/exchange-token-savings-v06.md:
//
//	net_tokens_saved = saved_on_real_hits − miss_costs − false_positive_waste
//
// For each hit buy:
//   - If opts.ConsumeCountByEntry is non-nil: a hit is "real" only when the
//     entry was consumed (consume count > 0). Un-consumed = false positive.
//   - If opts.ConsumeCountByEntry is nil: all hits are treated as real
//     (conservative/legacy assumption: every hit saved the buyer).
func computeNetSavings(outcomeByBuy map[string]*buyOutcome, o HitRateOptions, missCostPerQuery int64) (
	savedOnRealHits, totalMissCost, totalFalsePositiveWaste int64,
	perQueryEconomics []QueryEconomics,
) {
	perQueryEconomics = make([]QueryEconomics, 0, len(outcomeByBuy))

	for buyID, bo := range outcomeByBuy {
		switch {
		case bo.hit:
			consumed := true // default: treat as consumed when signal unavailable
			if o.ConsumeCountByEntry != nil {
				consumed = o.ConsumeCountByEntry[bo.topEntryID] > 0
			}
			if consumed {
				// Real hit: buyer avoided re-deriving. Save = token_cost_original.
				savedOnRealHits += bo.topTokenCost
				perQueryEconomics = append(perQueryEconomics, QueryEconomics{
					BuyID:             buyID,
					Outcome:           "hit",
					TokenCostOriginal: bo.topTokenCost,
					Saved:             bo.topTokenCost,
				})
			} else {
				// False positive: buyer received result but didn't use it.
				// Per §1: "A false positive is worse than a miss." The buyer wasted
				// time reading an irrelevant entry and still had to re-derive.
				totalFalsePositiveWaste += bo.topTokenCost
				perQueryEconomics = append(perQueryEconomics, QueryEconomics{
					BuyID:             buyID,
					Outcome:           "false_positive",
					TokenCostOriginal: bo.topTokenCost,
					Saved:             -bo.topTokenCost,
				})
			}
		case bo.unverifiable && !bo.miss:
			// Unverifiable hits: we cannot compute economics without consume data.
			// Omit from per-query economics (they're excluded from HitRatePct too).
		default:
			// Miss: buyer paid the round-trip overhead with no value delivered.
			totalMissCost += missCostPerQuery
			perQueryEconomics = append(perQueryEconomics, QueryEconomics{
				BuyID:   buyID,
				Outcome: "miss",
				Saved:   -missCostPerQuery,
			})
		}
	}
	return savedOnRealHits, totalMissCost, totalFalsePositiveWaste, perQueryEconomics
}

// ComputeHitRate reconciles a set of buy orders against a set of match-result
// messages and returns a HitRateReport.
//
// buys are exchange:buy messages (their .ID is the buy order ID).
// matches are exchange:match messages (hits and misses both carry this tag).
//
// Quality-weighted hit classification (M-rebaseline, dontguess-af8):
// A buy counts as a HIT only when the delivered top result's similarity is at or
// above opts.MinSimilarity AND the result is not synthetic. Delivered results
// below the floor are reclassified as MISSES (counted in BelowFloorDowngraded).
// Historical match-results without a "similarity" field (pre-M2/dontguess-b26)
// are handled via opts.Embedder recompute (approach A) or reported as
// UnverifiableHits when recompute is unavailable (approach B).
//
// Synthetic exclusion: match-result messages tagged exchange:synthetic (produced
// by the engine when the buy task matched demand.IsSynthetic) are skipped and
// their corresponding buy order IDs are removed from the real-buy set.
//
// Callers are responsible for windowing (passing only messages within --since).
//
// opts is variadic for backward compatibility — existing callers that pass no
// options get legacy behaviour (no floor gate, all delivered results = hits).
func ComputeHitRate(buys, matches []Message, opts ...HitRateOptions) HitRateReport {
	var o HitRateOptions
	if len(opts) > 0 {
		o = opts[0]
	}

	// Resolve miss cost per query (defaults to DefaultMissCostPerQuery when unset).
	missCostPerQuery := o.MissCostPerQuery
	if missCostPerQuery <= 0 {
		missCostPerQuery = DefaultMissCostPerQuery
	}

	// Phase 1: identify and exclude synthetic buys; build the real buy ID set.
	buyIDs, syntheticExcluded := excludeSyntheticBuys(buys, matches)

	// Phase 2: classify each non-synthetic match-result against its buy order.
	outcomeByBuy, realMatchResults, unjoinable, belowFloorDowngraded, recomputedSimilarity :=
		classifyBuyOutcomes(matches, buyIDs, o)

	// Phase 3: tally hits, misses, unverifiable hits, and compute HitRatePct.
	hits, misses, unverifiableHits, hitRatePct := tallyOutcomes(outcomeByBuy)

	// Phase 4: count inventory entries with cross-agent convergence (3+ distinct buyers).
	crossAgentConvergence := computeConvergence(o.EntryBuyerMap)

	// Phase 5: compute net token-savings economics and per-query breakdown.
	savedOnRealHits, totalMissCost, totalFalsePositiveWaste, perQueryEconomics :=
		computeNetSavings(outcomeByBuy, o, missCostPerQuery)

	matchedBuys := len(outcomeByBuy)
	netTokensSaved := savedOnRealHits - totalMissCost - totalFalsePositiveWaste

	return HitRateReport{
		TotalBuys:               len(buyIDs),
		MatchedBuys:             matchedBuys,
		PendingBuys:             len(buyIDs) - matchedBuys,
		Hits:                    hits,
		Misses:                  misses,
		BelowFloorDowngraded:    belowFloorDowngraded,
		RecomputedSimilarity:    recomputedSimilarity,
		UnverifiableHits:        unverifiableHits,
		HitRatePct:              hitRatePct,
		MatchResultsTotal:       realMatchResults,
		UnjoinableMatchResults:  unjoinable,
		SyntheticExcluded:       syntheticExcluded,
		CrossAgentConvergence:   crossAgentConvergence,
		NetTokensSaved:          netTokensSaved,
		SavedOnRealHits:         savedOnRealHits,
		TotalMissCost:           totalMissCost,
		TotalFalsePositiveWaste: totalFalsePositiveWaste,
		PerQueryEconomics:       perQueryEconomics,
	}
}

// round2 rounds to two decimal places.
func round2(f float64) float64 {
	return float64(int64(f*100+0.5)) / 100
}

// BuildConvergenceMap builds the merged cross-seller EntryBuyerMap from exchange
// State for use in HitRateOptions.EntryBuyerMap. The result maps each inventory
// entry ID to the set of distinct buyer keys that have completed a purchase of
// that entry. Entries with no buyers are omitted.
//
// Call this on a State that has been replayed from the full exchange message log
// before calling ComputeHitRate with the result as opts.EntryBuyerMap.
//
// The returned map is a copy — mutations do not affect State.
func BuildConvergenceMap(s *State) map[string]map[string]struct{} {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make(map[string]map[string]struct{})
	for _, stats := range s.sellers {
		for entryID, buyers := range stats.EntryBuyerMap {
			if len(buyers) == 0 {
				continue
			}
			if _, exists := out[entryID]; !exists {
				out[entryID] = make(map[string]struct{}, len(buyers))
			}
			for buyerKey := range buyers {
				out[entryID][buyerKey] = struct{}{}
			}
		}
	}
	return out
}

// ConsumeCountByEntry tallies the number of exchange:consume signals per
// entry_id from a slice of consume messages.
//
// consume is a slice of exchange:consume messages (filtered by TagConsume
// before calling). Returns a map from entry_id → consume count.
//
// The entry_id is read from the payload field "entry_id", which is set by
// emitConsumeSignal (engine.go) using the antecedent-derived entry ID —
// never from the buyer-supplied payload. A missing or unparseable entry_id
// skips the message.
//
// Returns nil when consumes is nil or empty. This is the signal-unavailable
// sentinel: ComputeHitRate treats a nil ConsumeCountByEntry as "no consume
// data available" and falls back to the conservative assumption (all hits are
// real). An empty non-nil map would instead classify every above-floor hit as
// a false positive, inverting NetTokensSaved even when the exchange performs well.
//
// Callers are responsible for windowing (passing only messages within --since).
func ConsumeCountByEntry(consumes []Message) map[string]int {
	if len(consumes) == 0 {
		return nil
	}
	counts := make(map[string]int, len(consumes))
	for i := range consumes {
		var p struct {
			EntryID string `json:"entry_id"`
		}
		if err := json.Unmarshal(consumes[i].Payload, &p); err != nil || p.EntryID == "" {
			continue
		}
		counts[p.EntryID]++
	}
	return counts
}
