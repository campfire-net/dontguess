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

	// Pass 1: scan match-results to collect synthetic buy IDs.
	// The engine tags the *response* (match/buy-miss) with exchange:synthetic
	// when demand.IsSynthetic(task) is true. The originating buy message (sent
	// by the buyer) carries no such tag — so we identify synthetic buys via
	// their corresponding tagged response.
	syntheticBuyIDs := make(map[string]struct{})
	var syntheticExcluded int
	for i := range matches {
		m := &matches[i]
		if !isMessageSynthetic(m) {
			continue
		}
		syntheticExcluded++
		buyID := buyMsgIDFor(m)
		if buyID != "" {
			syntheticBuyIDs[buyID] = struct{}{}
		}
	}

	// Build the set of real (non-synthetic) buy order IDs.
	buyIDs := make(map[string]struct{}, len(buys))
	for i := range buys {
		id := buys[i].ID
		if _, isSynthetic := syntheticBuyIDs[id]; !isSynthetic {
			buyIDs[id] = struct{}{}
		}
	}

	// Best outcome per real buy. We track the best quality-gate outcome across
	// all match-results for a given buy. The ranking is:
	//   qualityGateHit > qualityGateRecomputedHit > qualityGateUnverifiable >
	//   qualityGateRecomputedMiss > qualityGateMiss
	//
	// We use a simple bool map for hit tracking, plus a separate
	// unverifiableOnly map for buys whose best outcome is unverifiable.
	type buyOutcome struct {
		hit          bool // true once a hit (above-floor) result is seen
		unverifiable bool // true if best outcome so far is unverifiable (no verified hit yet)
		miss         bool // true if at least one result was seen (and none were hits)
	}
	outcomeByBuy := make(map[string]*buyOutcome)
	var unjoinable, realMatchResults int
	var belowFloorDowngraded, recomputedSimilarity int

	for i := range matches {
		m := &matches[i]
		// Skip synthetic responses — already counted in syntheticExcluded above.
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
		case qualityGateRecomputedHit:
			bo.hit = true
			recomputedSimilarity++
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

	// Tally results.
	hits := 0
	misses := 0
	unverifiableHits := 0
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
	matchedBuys := len(outcomeByBuy)

	// HitRatePct denominator: exclude unverifiable hits (unknown quality).
	// Only count verified hits and verified misses.
	verifiedAnswered := hits + misses
	var hitRatePct float64
	if verifiedAnswered > 0 {
		hitRatePct = round2(float64(hits) / float64(verifiedAnswered) * 100)
	}

	rep := HitRateReport{
		TotalBuys:              len(buyIDs),
		MatchedBuys:            matchedBuys,
		PendingBuys:            len(buyIDs) - matchedBuys,
		Hits:                   hits,
		Misses:                 misses,
		BelowFloorDowngraded:   belowFloorDowngraded,
		RecomputedSimilarity:   recomputedSimilarity,
		UnverifiableHits:       unverifiableHits,
		HitRatePct:             hitRatePct,
		MatchResultsTotal:      realMatchResults,
		UnjoinableMatchResults: unjoinable,
		SyntheticExcluded:      syntheticExcluded,
	}
	return rep
}

// round2 rounds to two decimal places.
func round2(f float64) float64 {
	return float64(int64(f*100+0.5)) / 100
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
// Callers are responsible for windowing (passing only messages within --since).
func ConsumeCountByEntry(consumes []Message) map[string]int {
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
