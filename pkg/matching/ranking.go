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
	// DisputeCount is the total number of upheld disputes against this entry's seller.
	// Used for the Layer 0 correctness gate.
	DisputeCount int
	// HasUpheldDispute is true when this specific entry has an upheld dispute.
	// Layer 0 gates: if true, the entry is excluded from all results.
	HasUpheldDispute bool
}

// RankOptions configures the ranking algorithm.
type RankOptions struct {
	// MinSimilarity is the minimum cosine similarity to include a result.
	// Entries below this threshold are excluded entirely.
	// Default: 0.05 (very low — we want to include most entries, let the score sort them).
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

func (o *RankOptions) minSimilarity() float64 {
	if o.MinSimilarity > 0 {
		return o.MinSimilarity
	}
	return 0.05
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
	return 0.35
}

func (o *RankOptions) weightQuality() float64 {
	if o.WeightQuality > 0 {
		return o.WeightQuality
	}
	return 0.45
}

func (o *RankOptions) weightNovelty() float64 {
	if o.WeightNovelty > 0 {
		return o.WeightNovelty
	}
	return 0.20
}

// Rank applies the 4-layer value stack to a set of candidates and returns
// a sorted slice of RankedResult, highest composite score first.
//
// Layer 0: Correctness gate — entries with HasUpheldDispute=true are excluded.
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

	// Layer 0: Build the dispute-filtered candidate list.
	// sellerCount for Layer 3 must be computed AFTER this filter so that
	// disputed entries do not inflate the seller count and skew novelty scores.
	filtered := candidates[:0:0] // zero-length slice, same backing type
	for _, c := range candidates {
		if !c.HasUpheldDispute {
			filtered = append(filtered, c)
		}
	}

	// Layer 3: count seller appearances over the filtered (non-disputed) set only.
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
		l3Novelty := 1.0 - float64(sellerCount[c.SellerKey])/float64(maxSellerCount)

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
