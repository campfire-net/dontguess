package matching

import (
	"math"
	"time"
)

// RankedResult is a single match result with its composite score components.
type RankedResult struct {
	// EntryID is the inventory entry's ID.
	EntryID string
	// Similarity is the raw cosine similarity between the buy task and the
	// inventory entry's description embedding. Range [0, 1].
	Similarity float64
	// Confidence is the Layer 2 composite quality score. Range [0, 1].
	// This is the value reported in the exchange:match payload.
	Confidence float64
	// CompositeScore is the final 4-layer ranking score. Higher is better.
	CompositeScore float64
	// IsPartialMatch is true when Confidence < 0.5 but Similarity > 0.
	IsPartialMatch bool
	// EfficiencyScore is the Layer 1 score: tokens_saved / price.
	EfficiencyScore float64
	// NoveltyBoost is the Layer 3 discovery boost for underrepresented sellers.
	NoveltyBoost float64
	// BehavioralBoost is the additive boost applied to CompositeScore from
	// behavioral signals (consume count + cross-agent convergence). Range [0, MaxBehavioralBoost].
	// Only non-zero when the entry has above-floor similarity AND meaningful
	// behavioral signals. Zero does not mean the entry was penalised.
	BehavioralBoost float64

	// FalsePositiveDemotion is the negative adjustment applied to CompositeScore
	// when the entry has a sustained high deliver-without-consume ratio
	// (dontguess-046). Range [MaxBehavioralDemotion, 0].
	// Zero means no demotion (no sustained false-positive pattern observed).
	// Non-zero only when DeliverCount >= FalsePositiveWindowMin AND ratio is high.
	FalsePositiveDemotion float64
}

// BehavioralSignals carries observed post-purchase signals for a single inventory
// entry. These are sourced from exchange state (consume messages + buyer maps)
// and injected into RankInput to close the "behavioral signals over preferences"
// loop described in the heritage value function.
//
// Signals are always observational — they measure what agents actually did, not
// what they rated. All fields are derived from non-spoofable, antecedent-anchored
// message chains in the exchange campfire log.
type BehavioralSignals struct {
	// ConsumeCount is the number of exchange:consume signals emitted for this
	// entry (TagConsume / ConsumeCountByEntry in pkg/exchange/hitrate.go).
	// Each signal records that a buyer completed a settle(complete) for this
	// entry — i.e. they actually received and accepted the content.
	// Sourced from: exchange.ConsumeCountByEntry(consumeMessages)[entryID].
	ConsumeCount int

	// DistinctBuyerCount is the number of distinct buyer agent keys that have
	// completed a purchase of this entry (cross-agent convergence signal).
	// Sourced from: len(State.EntryBuyerMap[entryID]) via BuildConvergenceMap.
	// The heritage "ungameable trust signal": independent agents reaching the
	// same entry without coordination signals genuine utility.
	DistinctBuyerCount int

	// DeliverCount is the number of times this entry has been delivered to a
	// buyer (settle:deliver events recorded by the exchange operator).
	// A delivery means the buyer received the content but may not have consumed
	// it (completed the transaction with settle:complete).
	//
	// The ratio DeliverCount / (ConsumeCount + 1) is the false-positive signal:
	// a high ratio over a sustained window means the entry is often delivered but
	// rarely consumed — buyers searched again after delivery instead of using it.
	// Sourced from: State.entryDeliverCount (dontguess-046).
	DeliverCount int
}

// MaxBehavioralBoost is the ceiling on the additive behavioral boost applied
// to CompositeScore (dontguess-860). Bounded to prevent a popular entry from
// burying genuinely better-matched alternatives — the boost is a tie-breaker
// and soft signal, not a guaranteed slot guarantee.
//
// A boost of 0.10 represents ~12.5% of the typical quality-weighted composite
// score (WeightQuality=0.80 * L2≈1.0), which is meaningful as a tie-breaker
// but cannot lift a weak-similarity entry above a strong one.
const MaxBehavioralBoost = 0.10

// MaxBehavioralDemotion is the floor on the negative behavioral adjustment
// applied to CompositeScore when an entry has a sustained high deliver-without-
// consume ratio (dontguess-046). Mirroring MaxBehavioralBoost in magnitude
// ensures demotion cannot push a relevant entry below a completely irrelevant
// junk entry — the relevance floor still gates, and demotion is bounded.
//
// Value: -0.10 (negative, applied additively). The symmetry with MaxBehavioralBoost
// means a maximally-demoted entry still outranks any entry that the positive
// boost cannot reach from baseline.
const MaxBehavioralDemotion = -0.10

// FalsePositiveWindowMin is the minimum number of deliveries required before the
// false-positive demotion signal activates. This prevents a single deliver-without-
// consume from triggering demotion — a sustained pattern is required.
//
// The window requires at least 3 deliveries before any ratio-based penalty is
// computed, mirroring the convergence threshold (3 distinct buyers) in the positive
// signal. Below this window the demotion is zero regardless of the ratio.
const FalsePositiveWindowMin = 3

// FalsePositiveRatioThreshold is the deliver-without-consume ratio above which
// an entry is flagged as an expiry candidate by the operator-facing report.
// Ratio = DeliverCount / max(ConsumeCount, 1). When ratio >= threshold AND
// DeliverCount >= FalsePositiveWindowMin, the entry is a candidate for expiry.
//
// A ratio of 5.0 means 5 deliveries per consume — the entry was delivered 5 times
// as often as it was actually used. This is a strong false-positive signal.
const FalsePositiveRatioThreshold = 5.0

// RankInput carries per-entry data needed by the ranker.
// This struct decouples the ranker from the exchange.InventoryEntry type.
type RankInput struct {
	// EntryID is the inventory entry's ID.
	EntryID string
	// SellerKey is the hex-encoded Ed25519 public key of the seller.
	SellerKey string
	// Description is the entry's description text.
	Description string
	// ContentType is the content type of the entry.
	ContentType string
	// Domains is the entry's domain tags.
	Domains []string
	// TokenCost is the original inference cost in tokens (seller's claim).
	TokenCost int64
	// Price is the exchange's current asking price in scrip.
	Price int64
	// SellerReputation is the seller's derived reputation score (0-100).
	SellerReputation int
	// PutTimestamp is the campfire-observed receipt time of the put (nanoseconds).
	PutTimestamp int64
	// Signals carries optional behavioral signals for this entry.
	// Zero value (both fields 0) means no signals are available — the entry
	// is ranked purely on relevance, efficiency, quality, and novelty.
	Signals BehavioralSignals
}

// RankOptions configures the ranking algorithm.
type RankOptions struct {
	// MinSimilarity is the minimum cosine similarity to include a result.
	// Entries below this threshold are excluded entirely — a hard Layer-1 relevance floor.
	// Default: 0.16 (M1a, dontguess-7d6). Raised from 0.05 to sit between junk_max=0.1548
	// and ideal_min=0.1826 measured on the live exchange D1 fixture. See minSimilarity().
	MinSimilarity float64

	// PartialMatchThreshold is the confidence level below which a result
	// is marked as a partial match. Default: 0.5.
	PartialMatchThreshold float64

	// FreshnessHalflifeDays is the half-life for freshness decay in the
	// Layer 2 quality composite. Default: 14 days.
	FreshnessHalflifeDays float64

	// Layer weights for the composite score.
	// WeightEfficiency controls the Layer 1 contribution.
	// WeightQuality controls the Layer 2 contribution.
	// WeightNovelty controls the Layer 3 contribution.
	// They should sum to approximately 1.0.
	WeightEfficiency float64
	WeightQuality    float64
	WeightNovelty    float64
}

// DefaultMinSimilarity returns the default cosine similarity floor used by the
// matching engine (M1a, dontguess-7d6). Callers that need to apply the same
// floor outside the matcher (e.g. the hit-rate reporter) MUST use this function
// rather than hardcoding 0.16, so the constant stays in sync when it changes.
func DefaultMinSimilarity() float64 {
	return (&RankOptions{}).minSimilarity()
}

func (o *RankOptions) minSimilarity() float64 {
	if o.MinSimilarity > 0 {
		return o.MinSimilarity
	}
	// M1a (dontguess-7d6): raised from 0.05 to 0.16.
	// Empirically swept [0.10..0.40] against the D1 fixture (d1_diagnostic_test.go).
	// junk_max=0.1548, ideal_min=0.1826. Floor 0.16 is the lowest value that achieves
	// 100% junk-upgrade-smoke rejection while maximising substantive-reuse survival:
	//   - At floor=0.12+: 7/7 junk rejected, 10/13 substantive survived, both extended pairs survive.
	//   - At floor=0.1826: eventsink-e2e-chained-dispatch (sim=0.1826) is lost → accuracy drops.
	//   - 0.16 gives 7% margin above junk_max with zero real-entry loss.
	return 0.16
}

func (o *RankOptions) partialThreshold() float64 {
	if o.PartialMatchThreshold > 0 {
		return o.PartialMatchThreshold
	}
	return 0.5
}

func (o *RankOptions) freshnessHalflife() float64 {
	if o.FreshnessHalflifeDays > 0 {
		return o.FreshnessHalflifeDays
	}
	return 14.0
}

func (o *RankOptions) weightEfficiency() float64 {
	if o.WeightEfficiency > 0 {
		return o.WeightEfficiency
	}
	// M1a (dontguess-7d6): reduced from 0.35 → 0.15.
	// Prevents a high token_cost/price ratio (junk with tokenCost=100) from
	// competing with relevance when novelty collapses to 0 in single-seller inventory.
	return 0.15
}

func (o *RankOptions) weightQuality() float64 {
	if o.WeightQuality > 0 {
		return o.WeightQuality
	}
	// M1a (dontguess-7d6): raised from 0.45 → 0.80.
	// L2 quality (of which similarity is the dominant sub-component at 0.50 weight)
	// now dominates the composite. Relevance gates the ranking.
	return 0.80
}

func (o *RankOptions) weightNovelty() float64 {
	if o.WeightNovelty > 0 {
		return o.WeightNovelty
	}
	// M1a (dontguess-7d6): reduced from 0.20 → 0.05.
	// Single-seller inventory produces novelty=0 for ALL entries (1-1/1=0).
	// At the old weight=0.20 this was fine since novelty contributed nothing.
	// But after the floor+quality rebalance, novelty is now a minor tie-breaker only.
	return 0.05
}

// Rank applies the 4-layer value stack to a set of candidates and returns
// a sorted slice of RankedResult, highest composite score first.
//
// Layer 1: Transaction efficiency — tokens_saved / price. Higher ratio = better deal.
// tokens_saved = TokenCost (original inference cost the buyer avoids).
//
// Layer 2: Value composite — weighted combination of:
//   - Semantic similarity to the buy task (from the embedder)
//   - Seller reputation (0-100 normalized to [0,1])
//   - Content freshness (exponential decay over FreshnessHalflifeDays)
//   - Content diversity (unique domain breadth, normalized)
//
// Layer 3: Market novelty — discovery boost for underrepresented sellers.
// Sellers who appear rarely in the candidate set get a boost to prevent
// popular sellers from crowding out discovery.
//
// The final composite score = WeightEfficiency * L1 + WeightQuality * L2 + WeightNovelty * L3.
// Similarity is factored into L2 but also gates the minimum inclusion threshold.
func Rank(task string, candidates []RankInput, embedder Embedder, opts RankOptions) []RankedResult {
	if len(candidates) == 0 {
		return nil
	}

	// Compute task embedding.
	taskEmb := embedder.Embed(task)

	now := time.Now().UnixNano()
	halflifeSec := opts.freshnessHalflife() * 24 * 3600

	// Layer 3: count seller appearances for the novelty boost.
	filtered := candidates
	sellerCount := make(map[string]int, len(filtered))
	for _, c := range filtered {
		sellerCount[c.SellerKey]++
	}
	maxSellerCount := 1
	for _, cnt := range sellerCount {
		if cnt > maxSellerCount {
			maxSellerCount = cnt
		}
	}

	results := make([]RankedResult, 0, len(filtered))

	for _, c := range filtered {
		// Compute cosine similarity.
		entryEmb := embedder.Embed(c.Description)
		sim := embedder.Similarity(taskEmb, entryEmb)

		// Exclude entries below minimum similarity threshold.
		if sim < opts.minSimilarity() {
			continue
		}

		// Layer 1: Transaction efficiency.
		// Efficiency = tokens_saved / price.
		// We normalize by dividing by a reference value (1000 tokens/scrip) to keep [0,1].
		// If price is 0, treat as zero efficiency: a zero-price entry has no valid scrip
		// flow and must not dominate rankings via the free-item path.
		// If TokenCost is also 0, efficiency is 0 (no work represented).
		var l1Efficiency float64
		if c.Price > 0 && c.TokenCost > 0 {
			ratio := float64(c.TokenCost) / float64(c.Price)
			// Normalize: ratio of 10 (great deal) → 1.0; ratio < 1 (poor deal) → < 0.1.
			l1Efficiency = math.Min(ratio/10.0, 1.0)
		}

		// Layer 2: Value composite.
		// 2a. Similarity contribution (already computed above).
		simScore := math.Max(sim, 0) // clamp negative cosine similarity

		// 2b. Seller reputation normalized to [0, 1].
		repScore := float64(c.SellerReputation) / 100.0

		// 2c. Content freshness: exponential decay.
		// ageSeconds = time since put; freshness = e^(-age / halflife).
		ageSeconds := float64(now-c.PutTimestamp) / 1e9
		if ageSeconds < 0 {
			ageSeconds = 0
		}
		freshnessScore := math.Exp(-ageSeconds / halflifeSec)

		// 2d. Content diversity: unique domains count normalized to [0, 1].
		// Max possible domains is 5 (per convention).
		domainScore := math.Min(float64(len(c.Domains))/5.0, 1.0)

		// Layer 2 composite: weighted mix.
		// Similarity carries the most weight — it gates relevance.
		l2Quality := 0.50*simScore + 0.25*repScore + 0.15*freshnessScore + 0.10*domainScore

		// Layer 3: Market novelty / discovery boost.
		// Sellers who appear once get boost=1.0; dominant sellers get boost→0.
		// novelty = 1 - (sellerCount / maxSellerCount)
		// This prevents popular sellers from occupying all top slots.
		//
		// Single-seller collapse fix (M1a, dontguess-7d6): when the candidate set has
		// only one unique seller, every entry produces novelty=0 (1-N/N=0). With the
		// old weights (novelty=0.20, efficiency=0.35) this made efficiency the dominant
		// non-similarity signal, allowing a high-efficiency junk entry to compete.
		// Fix: use novelty=0.5 (neutral) when there is only one unique seller — no
		// discovery boost or penalty, preserving the composite's relevance-first order.
		var l3Novelty float64
		if len(sellerCount) == 1 {
			// Single seller: novelty is undefined. Use neutral 0.5 so the composite
			// is fully governed by L1 efficiency and L2 quality (relevance).
			l3Novelty = 0.5
		} else {
			l3Novelty = 1.0 - float64(sellerCount[c.SellerKey])/float64(maxSellerCount)
		}

		// Final composite score (L1 + L2 + L3).
		composite := opts.weightEfficiency()*l1Efficiency +
			opts.weightQuality()*l2Quality +
			opts.weightNovelty()*l3Novelty

		// Behavioral booster (dontguess-860): additive boost for entries that
		// have been consumed and/or convergently validated by distinct agents.
		//
		// Design principles (§3 of docs/design/exchange-token-savings-v06.md):
		//  - Floor gates first: this code is only reached for above-floor entries
		//    (sim >= MinSimilarity). The boost CANNOT resurrect below-floor entries.
		//  - Bounded: capped at MaxBehavioralBoost (0.10) so a highly-consumed
		//    entry cannot bury a more-relevant alternative whose similarity is
		//    significantly higher.
		//  - Gaming-resistant: consume signals are antecedent-anchored (engine
		//    emits TagConsume, not the buyer); convergence requires >=3 distinct
		//    keys (DistinctBuyerCount threshold).
		//  - Zero-safe: when Signals is zero value, boost == 0 → no change to
		//    existing ranking for entries without signals.
		behavioralBoost := computeBehavioralBoost(c.Signals)
		composite += behavioralBoost

		// False-positive demotion (dontguess-046): negative adjustment for entries
		// with a sustained high deliver-without-consume ratio.
		//
		// Design principles:
		//  - Window guard: demotion is zero when DeliverCount < FalsePositiveWindowMin.
		//    A single deliver-without-consume must NOT trigger demotion.
		//  - Bounded: floor at MaxBehavioralDemotion (-0.10), symmetric with the
		//    positive boost. A demoted entry still outranks a below-floor junk entry.
		//  - Additive: applied after the positive boost, so a highly-consumed entry
		//    with a low false-positive ratio is not penalised even if DeliverCount
		//    is also high (the ratio gates the demotion, not the raw deliver count).
		//  - Zero-safe: when Signals.DeliverCount == 0, demotion == 0.
		fpDemotion := computeFalsePositiveDemotion(c.Signals)
		composite += fpDemotion // fpDemotion is <= 0

		// Confidence is the Layer 2 quality composite (what the buyer sees).
		confidence := l2Quality

		results = append(results, RankedResult{
			EntryID:               c.EntryID,
			Similarity:            sim,
			Confidence:            confidence,
			CompositeScore:        composite,
			IsPartialMatch:        confidence < opts.partialThreshold(),
			EfficiencyScore:       l1Efficiency,
			NoveltyBoost:          l3Novelty,
			BehavioralBoost:       behavioralBoost,
			FalsePositiveDemotion: fpDemotion,
		})
	}

	// Sort descending by composite score (insertion sort — candidates are small, ~100s).
	for i := 1; i < len(results); i++ {
		for j := i; j > 0 && results[j].CompositeScore > results[j-1].CompositeScore; j-- {
			results[j], results[j-1] = results[j-1], results[j]
		}
	}

	return results
}

// computeBehavioralBoost computes the additive behavioral signal boost for a
// single inventory entry given its observed signals.
//
// Two signals contribute independently:
//
//  1. Consume signal (M5): each exchange:consume event for this entry adds a
//     small, logarithmically-dampened boost. Using log1p dampens the effect of
//     high consume counts so one very popular entry cannot dominate indefinitely.
//     Weight: up to half of MaxBehavioralBoost.
//
//  2. Cross-agent convergence (dontguess-412): entries consumed by >=3 distinct
//     agent keys receive a flat step-up bonus. The threshold (3) comes from the
//     heritage "ungameable trust signal" — 3+ independent agents reaching the same
//     entry without coordination is a strong utility signal. Entries with fewer
//     distinct buyers get a proportional partial bonus, so convergence is a
//     continuous reward rather than an all-or-nothing gate.
//     Weight: up to half of MaxBehavioralBoost.
//
// Total boost is capped at MaxBehavioralBoost (0.10) and is always non-negative.
// The boost is zero when both signal fields are zero (backward-compatible).
func computeBehavioralBoost(s BehavioralSignals) float64 {
	if s.ConsumeCount == 0 && s.DistinctBuyerCount == 0 {
		return 0
	}

	// Consume signal: log1p-dampened, half weight.
	// log1p(1)≈0.693, log1p(10)≈2.40, log1p(100)≈4.61 — saturates slowly.
	// Normalize so log1p(10 consumes) → ~0.5 half-weight contribution.
	consumeNorm := math.Log1p(float64(s.ConsumeCount)) / math.Log1p(10.0)
	if consumeNorm > 1.0 {
		consumeNorm = 1.0
	}
	consumeContrib := consumeNorm * (MaxBehavioralBoost / 2.0)

	// Convergence signal: proportional to distinct buyers up to the threshold.
	// At >=3 buyers: full half-weight (0.05). Below 3: linear partial reward.
	const convergenceThreshold = 3.0
	buyerNorm := math.Min(float64(s.DistinctBuyerCount)/convergenceThreshold, 1.0)
	convergenceContrib := buyerNorm * (MaxBehavioralBoost / 2.0)

	total := consumeContrib + convergenceContrib
	if total > MaxBehavioralBoost {
		total = MaxBehavioralBoost
	}
	return total
}

// computeFalsePositiveDemotion computes the negative behavioral adjustment for
// entries with a sustained high deliver-without-consume ratio (dontguess-046).
//
// A "false positive" occurs when the exchange delivers an entry to a buyer but
// the buyer does not consume it (does not complete the transaction). The buyer
// searching again immediately after delivery is the proxy signal — the entry
// matched on semantics but failed on utility.
//
// The demotion is gated by a window requirement: DeliverCount must be >=
// FalsePositiveWindowMin (3) before any penalty is applied. This prevents a
// single deliver-without-consume from triggering demotion.
//
// When above the window, the demotion is proportional to the deliver-without-
// consume ratio, capped at MaxBehavioralDemotion (-0.10). The ratio is computed
// as DeliverCount / max(ConsumeCount, 1).
//
// Design invariant: the returned value is always <= 0 (a demotion, never a boost).
// Returns 0.0 when Signals.DeliverCount == 0 or below the window threshold.
func computeFalsePositiveDemotion(s BehavioralSignals) float64 {
	// Window guard: require sustained pattern before any demotion.
	if s.DeliverCount < FalsePositiveWindowMin {
		return 0
	}

	// Ratio: deliveries per consume. Use max(ConsumeCount, 1) to avoid divide-by-zero.
	consumeDenom := s.ConsumeCount
	if consumeDenom < 1 {
		consumeDenom = 1
	}
	ratio := float64(s.DeliverCount) / float64(consumeDenom)

	// No demotion when the ratio is low (entry is well-consumed relative to deliveries).
	// A ratio of 1.0 means every delivery was consumed — ideal. We start penalising
	// above 2.0 (twice as many deliveries as consumes), ramping to full demotion at
	// FalsePositiveRatioThreshold (5.0).
	const ratioLow = 2.0
	if ratio <= ratioLow {
		return 0
	}

	// Linear ramp from 0 at ratio=ratioLow to MaxBehavioralDemotion at ratio=FalsePositiveRatioThreshold.
	// norm in [0, 1]: 0 at ratioLow, 1 at FalsePositiveRatioThreshold.
	norm := (ratio - ratioLow) / (FalsePositiveRatioThreshold - ratioLow)
	if norm > 1.0 {
		norm = 1.0
	}

	// MaxBehavioralDemotion is negative, so multiply to get a value in [MaxBehavioralDemotion, 0].
	demotion := norm * MaxBehavioralDemotion
	// Note: the lower-bound clamp (demotion < MaxBehavioralDemotion) is unreachable
	// because norm is already clamped to [0, 1] above, so demotion ∈ [MaxBehavioralDemotion, 0].
	return demotion
}

// IsFalsePositiveExpiry reports whether an entry with the given signals should
// be flagged as an expiry candidate based on its sustained deliver-without-consume
// ratio. This is the criterion used by State.ExpiryCandidates().
//
// Criteria (dontguess-046):
//   - DeliverCount >= FalsePositiveWindowMin (sustained, not a single miss)
//   - ratio = DeliverCount / max(ConsumeCount, 1) >= FalsePositiveRatioThreshold
//
// Returns true when both conditions are met. The operator-facing report surfaces
// these entries; the exchange does NOT autonomously expire or remove them.
func IsFalsePositiveExpiry(s BehavioralSignals) bool {
	if s.DeliverCount < FalsePositiveWindowMin {
		return false
	}
	consumeDenom := s.ConsumeCount
	if consumeDenom < 1 {
		consumeDenom = 1
	}
	ratio := float64(s.DeliverCount) / float64(consumeDenom)
	return ratio >= FalsePositiveRatioThreshold
}
