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
}

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

		// Final composite score.
		composite := opts.weightEfficiency()*l1Efficiency +
			opts.weightQuality()*l2Quality +
			opts.weightNovelty()*l3Novelty

		// Confidence is the Layer 2 quality composite (what the buyer sees).
		confidence := l2Quality

		results = append(results, RankedResult{
			EntryID:         c.EntryID,
			Similarity:      sim,
			Confidence:      confidence,
			CompositeScore:  composite,
			IsPartialMatch:  confidence < opts.partialThreshold(),
			EfficiencyScore: l1Efficiency,
			NoveltyBoost:    l3Novelty,
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
