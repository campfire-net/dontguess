package exchange

import (
	"fmt"
	"testing"

	"github.com/campfire-net/dontguess/pkg/matching"
)

// buyMsg builds an exchange:buy fixture with the real payload shape observed
// live: {"budget","max_results","min_reputation","task"}. The message ID is the
// buy order ID.
func buyMsg(id, task string) Message {
	return Message{
		ID:      id,
		Tags:    []string{TagBuy},
		Payload: []byte(`{"budget":2000,"max_results":3,"min_reputation":0,"task":"` + task + `"}`),
	}
}

// hitMatch builds a HIT exchange:match fixture: tag exchange:match only, a
// non-empty results array, antecedent = buy order ID. Shape mirrors the live
// payload emitted by Engine.handleBuy.
func hitMatch(id, buyID string) Message {
	return Message{
		ID:          id,
		Tags:        []string{TagMatch},
		Antecedents: []string{buyID},
		Payload: []byte(`{"results":[` +
			`{"entry_id":"e1","put_msg_id":"p1","seller_key":"s1","description":"d",` +
			`"content_hash":"sha256:ab","content_type":"code","price":120,"confidence":0.91,` +
			`"is_partial_match":false,"seller_reputation":80,"token_cost_original":1000,"age_hours":3}` +
			`],"search_meta":{"total_candidates":5},"guide":"Results are ranked by ..."}`),
	}
}

// missMatch builds a MISS exchange:match fixture: tags
// [exchange:buy-miss, exchange:match], top-level buy_msg_id + task_hash and the
// "No cached inference matched" guide, antecedent = buy order ID. Shape mirrors
// Engine.handleBuyMiss.
func missMatch(id, buyID string) Message {
	return Message{
		ID:          id,
		Tags:        []string{TagBuyMiss, TagMatch},
		Antecedents: []string{buyID},
		Payload: []byte(`{"buy_msg_id":"` + buyID + `","expires_at":"2026-06-02T20:13:29Z",` +
			`"guide":"No cached inference matched your task. A standing offer has been created ...",` +
			`"offered_price_rate":70,"task":"zzqq nonsense","task_hash":"b7699bc655dac384"}`),
	}
}

func TestComputeHitRate_HitMissPending(t *testing.T) {
	buys := []Message{
		buyMsg("buy-1", "explain campfire convention dispatch"),
		buyMsg("buy-2", "zzqq nonsense xyzzy no such cached inference"),
		buyMsg("buy-3", "pending order, no match yet"),
	}
	matches := []Message{
		hitMatch("m-1", "buy-1"),
		missMatch("m-2", "buy-2"),
		// buy-3 has no match -> pending.
	}

	rep := ComputeHitRate(buys, matches)

	if rep.TotalBuys != 3 {
		t.Errorf("TotalBuys = %d, want 3", rep.TotalBuys)
	}
	if rep.MatchedBuys != 2 {
		t.Errorf("MatchedBuys = %d, want 2", rep.MatchedBuys)
	}
	if rep.PendingBuys != 1 {
		t.Errorf("PendingBuys = %d, want 1", rep.PendingBuys)
	}
	if rep.Hits != 1 {
		t.Errorf("Hits = %d, want 1", rep.Hits)
	}
	if rep.Misses != 1 {
		t.Errorf("Misses = %d, want 1", rep.Misses)
	}
	if rep.HitRatePct != 50.0 {
		t.Errorf("HitRatePct = %v, want 50.0", rep.HitRatePct)
	}
	if rep.MatchResultsTotal != 2 {
		t.Errorf("MatchResultsTotal = %d, want 2", rep.MatchResultsTotal)
	}
}

// A match-result whose antecedent buy is not in the buy set is unjoinable and
// must not move hit/miss totals.
func TestComputeHitRate_Unjoinable(t *testing.T) {
	buys := []Message{
		buyMsg("buy-1", "task a"),
	}
	matches := []Message{
		hitMatch("m-1", "buy-1"),
		hitMatch("m-orphan", "buy-999"), // buy-999 not in buys
		missMatch("m-orphan2", "buy-998"),
	}

	rep := ComputeHitRate(buys, matches)

	if rep.TotalBuys != 1 {
		t.Errorf("TotalBuys = %d, want 1", rep.TotalBuys)
	}
	if rep.MatchedBuys != 1 {
		t.Errorf("MatchedBuys = %d, want 1", rep.MatchedBuys)
	}
	if rep.Hits != 1 || rep.Misses != 0 {
		t.Errorf("Hits/Misses = %d/%d, want 1/0", rep.Hits, rep.Misses)
	}
	if rep.UnjoinableMatchResults != 2 {
		t.Errorf("UnjoinableMatchResults = %d, want 2", rep.UnjoinableMatchResults)
	}
	if rep.HitRatePct != 100.0 {
		t.Errorf("HitRatePct = %v, want 100.0", rep.HitRatePct)
	}
}

// A buy that receives both a miss and a later hit (e.g. multiple match emissions)
// must be classified as a hit — best outcome wins, regardless of order.
func TestComputeHitRate_BestOutcomeWins(t *testing.T) {
	buys := []Message{buyMsg("buy-1", "task")}

	// Miss first, then hit.
	rep := ComputeHitRate(buys, []Message{
		missMatch("m-1", "buy-1"),
		hitMatch("m-2", "buy-1"),
	})
	if rep.Hits != 1 || rep.Misses != 0 || rep.MatchedBuys != 1 {
		t.Errorf("miss-then-hit: Hits/Misses/Matched = %d/%d/%d, want 1/0/1", rep.Hits, rep.Misses, rep.MatchedBuys)
	}

	// Hit first, then miss — order must not regress the hit.
	rep = ComputeHitRate(buys, []Message{
		hitMatch("m-1", "buy-1"),
		missMatch("m-2", "buy-1"),
	})
	if rep.Hits != 1 || rep.Misses != 0 {
		t.Errorf("hit-then-miss: Hits/Misses = %d/%d, want 1/0", rep.Hits, rep.Misses)
	}
}

func TestComputeHitRate_Empty(t *testing.T) {
	rep := ComputeHitRate(nil, nil)
	if rep.TotalBuys != 0 || rep.MatchedBuys != 0 || rep.HitRatePct != 0 {
		t.Errorf("empty: got %+v, want zero report", rep)
	}
}

// classifyMatchResult must use the buy-miss tag as the authoritative signal and
// fall back to payload shape.
func TestClassifyMatchResult(t *testing.T) {
	hit := hitMatch("m-1", "buy-1")
	if !classifyMatchResult(&hit) {
		t.Error("hit-shaped match classified as miss")
	}
	miss := missMatch("m-2", "buy-2")
	if classifyMatchResult(&miss) {
		t.Error("miss-shaped match classified as hit")
	}

	// Defensive: a miss-shaped payload that lost its tag is still a miss
	// (top-level buy_msg_id present, no results).
	noTagMiss := Message{
		ID:          "m-3",
		Tags:        []string{TagMatch},
		Antecedents: []string{"buy-3"},
		Payload:     []byte(`{"buy_msg_id":"buy-3","task_hash":"abc","offered_price_rate":70}`),
	}
	if classifyMatchResult(&noTagMiss) {
		t.Error("untagged miss-shaped payload classified as hit")
	}
}

// hitMatchWithSim builds a HIT exchange:match fixture whose top result carries
// an explicit "similarity" field (M2/dontguess-b26 payload shape). The
// description is also explicit so quality-weighted tests can verify recompute
// against a known task/description pair.
func hitMatchWithSim(id, buyID, description string, similarity float64) Message {
	payload := fmt.Sprintf(
		`{"results":[{"entry_id":"e1","put_msg_id":"p1","seller_key":"s1",`+
			`"description":%q,`+
			`"content_hash":"sha256:ab","content_type":"code","price":120,`+
			`"confidence":0.55,"similarity":%g,`+
			`"is_partial_match":false,"seller_reputation":80,`+
			`"token_cost_original":1000,"age_hours":3}],`+
			`"search_meta":{"total_candidates":5},"guide":"..."}`,
		description, similarity,
	)
	return Message{
		ID:          id,
		Tags:        []string{TagMatch},
		Antecedents: []string{buyID},
		Payload:     []byte(payload),
	}
}

// hitMatchNoSim builds a HIT exchange:match fixture WITHOUT a "similarity" field,
// matching the pre-M2 historical payload shape (confidence=0.5, no similarity).
// This is used to test the recompute path for historical match-results.
func hitMatchNoSim(id, buyID, description string) Message {
	payload := fmt.Sprintf(
		`{"results":[{"entry_id":"e1","put_msg_id":"p1","seller_key":"s1",`+
			`"description":%q,`+
			`"content_hash":"sha256:ab","content_type":"code","price":120,`+
			`"confidence":0.5,"is_partial_match":false,"seller_reputation":50,`+
			`"token_cost_original":1000,"age_hours":3}],`+
			`"search_meta":{"total_candidates":5},"guide":"..."}`,
		description,
	)
	return Message{
		ID:          id,
		Tags:        []string{TagMatch},
		Antecedents: []string{buyID},
		Payload:     []byte(payload),
	}
}

// syntheticMatch builds an exchange:match fixture tagged exchange:synthetic,
// mirroring the engine's synthetic tagging for load-test / probe traffic.
func syntheticMatch(id, buyID string) Message {
	return Message{
		ID:          id,
		Tags:        []string{TagMatch, TagSynthetic},
		Antecedents: []string{buyID},
		Payload: []byte(`{"results":[{"entry_id":"e-synth","put_msg_id":"p-synth","seller_key":"s1",` +
			`"description":"upgrade smoke test cf v0.31.2 operator round-trip",` +
			`"content_hash":"sha256:ff","content_type":"analysis","price":84,` +
			`"confidence":0.5,"is_partial_match":false,"seller_reputation":50,` +
			`"token_cost_original":100,"age_hours":1}],` +
			`"search_meta":{"total_candidates":1},"guide":"..."}`),
	}
}

// TestComputeHitRate_QualityWeighted is the M-rebaseline acceptance test
// (dontguess-af8). It exercises the quality-weighted path via fixtures covering
// the three required scenarios:
//
//  1. Above-floor delivered result (similarity ≥ floor) → counted as HIT.
//  2. Below-floor delivered result (similarity < floor) → reclassified as MISS.
//  3. Synthetic buy → EXCLUDED from all counts.
//
// All three scenarios use real fixture data through the real ComputeHitRate path.
// No mocks: the floor constant is read from matching.DefaultMinSimilarity() —
// the same constant the engine uses — so the test is in sync with M1.
func TestComputeHitRate_QualityWeighted(t *testing.T) {
	floor := matching.DefaultMinSimilarity()

	aboveFloorSim := floor + 0.20  // safely above floor
	belowFloorSim := floor - 0.10  // safely below floor (> 0 to avoid zero-vector edge case)
	if belowFloorSim < 0.01 {
		belowFloorSim = 0.01
	}

	// Three buy orders:
	//   buy-above: delivered result with similarity above floor → HIT
	//   buy-below: delivered result with similarity below floor → MISS (downgraded)
	//   buy-synthetic: synthetic buy → EXCLUDED
	buys := []Message{
		buyMsg("buy-above", "campfire convention dispatch lifecycle management"),
		buyMsg("buy-below", "campfire convention dispatch lifecycle management"),
		buyMsg("buy-synthetic", "regression-parallel-178949-buy-0001"),
	}

	// match-above: has explicit similarity field above the floor → HIT.
	matchAbove := hitMatchWithSim("m-above", "buy-above",
		"campfire convention dispatch lifecycle: declare, accept, revoke",
		aboveFloorSim,
	)

	// match-below: has explicit similarity field below the floor → downgraded MISS.
	matchBelow := hitMatchWithSim("m-below", "buy-below",
		"campfire convention dispatch lifecycle: declare, accept, revoke",
		belowFloorSim,
	)

	// match-synthetic: tagged exchange:synthetic → excluded.
	matchSynthetic := syntheticMatch("m-synthetic", "buy-synthetic")

	matches := []Message{matchAbove, matchBelow, matchSynthetic}

	opts := HitRateOptions{
		MinSimilarity: floor,
		// No Embedder — all three match messages have explicit similarity fields
		// (or are synthetic), so recompute is not needed for this scenario.
	}
	rep := ComputeHitRate(buys, matches, opts)

	t.Logf("floor=%.4f above=%.4f below=%.4f", floor, aboveFloorSim, belowFloorSim)
	t.Logf("report: total=%d matched=%d hits=%d misses=%d below_floor=%d synthetic=%d rate=%.2f%%",
		rep.TotalBuys, rep.MatchedBuys, rep.Hits, rep.Misses,
		rep.BelowFloorDowngraded, rep.SyntheticExcluded, rep.HitRatePct)

	// --- Scenario 1: above-floor delivered result → HIT ---
	if rep.Hits != 1 {
		t.Errorf("Hits = %d, want 1 (above-floor result should be a hit)", rep.Hits)
	}

	// --- Scenario 2: below-floor delivered result → MISS, not hit ---
	if rep.Misses != 1 {
		t.Errorf("Misses = %d, want 1 (below-floor result should be reclassified as miss)", rep.Misses)
	}
	if rep.BelowFloorDowngraded != 1 {
		t.Errorf("BelowFloorDowngraded = %d, want 1 (below-floor delivered result must be counted)", rep.BelowFloorDowngraded)
	}

	// --- Scenario 3: synthetic buy → EXCLUDED ---
	if rep.SyntheticExcluded != 1 {
		t.Errorf("SyntheticExcluded = %d, want 1 (synthetic buy should be excluded)", rep.SyntheticExcluded)
	}
	if rep.TotalBuys != 2 {
		t.Errorf("TotalBuys = %d, want 2 (synthetic buy must not appear in total)", rep.TotalBuys)
	}

	// HitRatePct: 1 hit, 1 miss → 50.00%.
	if rep.HitRatePct != 50.0 {
		t.Errorf("HitRatePct = %.2f, want 50.00 (1 hit / 2 verified answered)", rep.HitRatePct)
	}
}

// TestComputeHitRate_HistoricalRecompute is the M-rebaseline acceptance test for
// approach A (dontguess-af8): historical match-results that lack a "similarity"
// field (pre-M2/dontguess-b26) must have their similarity recomputed via the
// Embedder against the originating buy task.
//
// The test uses two historical-style match messages (no similarity field):
//   - One with a description that shares substantial vocabulary with the buy task
//     → recomputed similarity above the floor → HIT.
//   - One with a description that is semantically unrelated to the buy task
//     → recomputed similarity below the floor → downgraded MISS.
//
// Uses matching.NewTFIDFEmbedder() (real embedder). No mocks: the test exercises
// the exact code path the live hit-rate reporter uses when processing historical
// exchange data.
func TestComputeHitRate_HistoricalRecompute(t *testing.T) {
	floor := matching.DefaultMinSimilarity()

	// HIGH-OVERLAP pair: buy task and entry description share many terms.
	highTask := "campfire convention dispatch lifecycle management: claim revoke"
	highDesc := "campfire convention dispatch lifecycle: declare, claim, revoke management"

	// LOW-OVERLAP pair: buy task and entry description share no significant terms.
	lowTask := "campfire convention dispatch lifecycle management: claim revoke"
	lowDesc := "image pipeline JPEG encoding GPU CUDA resize thumbnail compute shader"

	buys := []Message{
		buyMsg("buy-high", highTask),
		buyMsg("buy-low", lowTask),
	}

	// Both match messages use the pre-M2 payload shape (no similarity field).
	matchHigh := hitMatchNoSim("m-high", "buy-high", highDesc)
	matchLow := hitMatchNoSim("m-low", "buy-low", lowDesc)

	matches := []Message{matchHigh, matchLow}

	// Build a TF-IDF embedder primed with the corpus (same approach as the CLI).
	emb := matching.NewTFIDFEmbedder()
	emb.IndexCorpus([]string{highTask, highDesc, lowTask, lowDesc})

	opts := HitRateOptions{
		MinSimilarity: floor,
		Embedder:      emb,
		BuyTasks: map[string]string{
			"buy-high": highTask,
			"buy-low":  lowTask,
		},
	}

	rep := ComputeHitRate(buys, matches, opts)

	t.Logf("floor=%.4f", floor)
	t.Logf("report: total=%d matched=%d hits=%d misses=%d recomputed=%d below_floor=%d rate=%.2f%%",
		rep.TotalBuys, rep.MatchedBuys, rep.Hits, rep.Misses,
		rep.RecomputedSimilarity, rep.BelowFloorDowngraded, rep.HitRatePct)

	// Both match messages had no similarity field: RecomputedSimilarity must be 2.
	if rep.RecomputedSimilarity != 2 {
		t.Errorf("RecomputedSimilarity = %d, want 2 (both historical matches had no similarity field)",
			rep.RecomputedSimilarity)
	}

	// High-overlap pair should be a HIT after recompute.
	if rep.Hits != 1 {
		t.Errorf("Hits = %d, want 1 (high-overlap historical match should be recomputed as above-floor hit)",
			rep.Hits)
	}

	// Low-overlap pair should be a MISS after recompute (below floor).
	if rep.Misses != 1 {
		t.Errorf("Misses = %d, want 1 (low-overlap historical match should be recomputed as below-floor miss)",
			rep.Misses)
	}
	if rep.BelowFloorDowngraded != 1 {
		t.Errorf("BelowFloorDowngraded = %d, want 1 (low-overlap recomputed miss must be counted)",
			rep.BelowFloorDowngraded)
	}

	// HitRatePct: 1 hit / 2 verified answered = 50%.
	if rep.HitRatePct != 50.0 {
		t.Errorf("HitRatePct = %.2f, want 50.00 (1 recomputed hit, 1 recomputed miss)", rep.HitRatePct)
	}
}

// TestHitRate_CrossAgentConvergence is the ground-source acceptance test for
// dontguess-412. It exercises the full path: real State seeded with distinct
// buyer keys → BuildConvergenceMap → ComputeHitRate with opts.EntryBuyerMap →
// CrossAgentConvergence field in the report.
//
// Scenario:
//   - entry-alpha: 3 DISTINCT buyer keys → converged → counted
//   - entry-beta:  2 buyer keys (one duplicate key) → not converged → not counted
//
// The test verifies CrossAgentConvergence == 1.
//
// No mocks: uses real State and BuildConvergenceMap — the same path the CLI uses.
// The buy/match messages are minimal fixtures; their content does not affect the
// convergence count (which is derived from EntryBuyerMap, not from match messages).
func TestHitRate_CrossAgentConvergence(t *testing.T) {
	// Seed a fresh State with two inventory entries and their buyer histories.
	st := NewState()
	st.OperatorKey = "operator-key-hex"

	// Directly populate sellers[sellerKey].EntryBuyerMap without going through
	// the full engine settle pipeline. This is white-box seeding (same package).
	// The mu lock protects concurrent access but tests are sequential here.
	const sellerKey = "seller-alpha-key-hex"
	st.sellers[sellerKey] = &SellerStats{
		RepeatBuyerMap: make(map[string]int),
		EntryBuyerMap: map[string]map[string]struct{}{
			// entry-alpha: 3 DISTINCT buyer keys → converged.
			"entry-alpha": {
				"buyer-agent-001": {},
				"buyer-agent-002": {},
				"buyer-agent-003": {},
			},
			// entry-beta: only 2 buyer keys → NOT converged.
			"entry-beta": {
				"buyer-agent-001": {},
				"buyer-agent-002": {},
			},
		},
	}

	// BuildConvergenceMap merges across all sellers into a flat entryID → buyers map.
	convergenceMap := BuildConvergenceMap(st)

	// Verify the map is correctly built.
	if len(convergenceMap["entry-alpha"]) != 3 {
		t.Fatalf("entry-alpha: want 3 buyers in convergenceMap, got %d", len(convergenceMap["entry-alpha"]))
	}
	if len(convergenceMap["entry-beta"]) != 2 {
		t.Fatalf("entry-beta: want 2 buyers in convergenceMap, got %d", len(convergenceMap["entry-beta"]))
	}

	// Pass the convergence map via HitRateOptions (the real ComputeHitRate path).
	// Buys and matches are empty — CrossAgentConvergence is independent of
	// the buy/match message history (it's an inventory-level signal).
	opts := HitRateOptions{
		EntryBuyerMap: convergenceMap,
	}
	rep := ComputeHitRate(nil, nil, opts)

	// Only entry-alpha has 3+ distinct buyers → CrossAgentConvergence == 1.
	if rep.CrossAgentConvergence != 1 {
		t.Errorf("CrossAgentConvergence = %d, want 1 (only entry-alpha has 3+ distinct buyer keys)",
			rep.CrossAgentConvergence)
	}

	// Confirm that hit-rate fields are zero (no buy/match messages passed).
	if rep.TotalBuys != 0 || rep.Hits != 0 || rep.Misses != 0 {
		t.Errorf("unexpected non-zero hit-rate fields: total=%d hits=%d misses=%d",
			rep.TotalBuys, rep.Hits, rep.Misses)
	}
}

// TestHitRate_CrossAgentConvergence_ZeroWhenNilMap verifies backward
// compatibility: when opts.EntryBuyerMap is nil (e.g. no state replay
// performed), CrossAgentConvergence is always 0 and no panic occurs.
func TestHitRate_CrossAgentConvergence_ZeroWhenNilMap(t *testing.T) {
	rep := ComputeHitRate(nil, nil)
	if rep.CrossAgentConvergence != 0 {
		t.Errorf("CrossAgentConvergence = %d, want 0 when opts.EntryBuyerMap is nil",
			rep.CrossAgentConvergence)
	}
}

// TestBuildConvergenceMap_MultiSeller verifies that BuildConvergenceMap correctly
// merges EntryBuyerMap across multiple sellers. When seller-A and seller-B both
// have buyers for the same entry (possible in derivative/compression scenarios),
// the merged map reflects all distinct buyers.
func TestBuildConvergenceMap_MultiSeller(t *testing.T) {
	st := NewState()
	st.OperatorKey = "op-key"

	// Two sellers both selling "shared-entry" (derivative scenario).
	st.sellers["seller-A"] = &SellerStats{
		RepeatBuyerMap: make(map[string]int),
		EntryBuyerMap: map[string]map[string]struct{}{
			"shared-entry": {
				"buyer-001": {},
				"buyer-002": {},
			},
		},
	}
	st.sellers["seller-B"] = &SellerStats{
		RepeatBuyerMap: make(map[string]int),
		EntryBuyerMap: map[string]map[string]struct{}{
			"shared-entry": {
				"buyer-002": {}, // duplicate — already counted above
				"buyer-003": {},
			},
			// Solo entry for seller-B only.
			"solo-entry": {
				"buyer-001": {},
			},
		},
	}

	merged := BuildConvergenceMap(st)

	// shared-entry has 3 unique buyers (001, 002, 003) across the two sellers.
	if len(merged["shared-entry"]) != 3 {
		t.Errorf("shared-entry: want 3 unique buyers, got %d", len(merged["shared-entry"]))
	}
	// solo-entry has 1 buyer.
	if len(merged["solo-entry"]) != 1 {
		t.Errorf("solo-entry: want 1 buyer, got %d", len(merged["solo-entry"]))
	}

	// CrossAgentConvergence: shared-entry has 3 → count 1. solo-entry has 1 → not counted.
	opts := HitRateOptions{EntryBuyerMap: merged}
	rep := ComputeHitRate(nil, nil, opts)
	if rep.CrossAgentConvergence != 1 {
		t.Errorf("CrossAgentConvergence = %d, want 1 (only shared-entry is converged)", rep.CrossAgentConvergence)
	}
}

// hitMatchWithSimEntry builds a HIT exchange:match fixture with explicit entry_id,
// token_cost_original, and similarity — the full shape needed for net-savings
// economics tests (Track C, dontguess-eff).
func hitMatchWithSimEntry(id, buyID, entryID, description string, similarity float64, tokenCost int64) Message {
	payload := fmt.Sprintf(
		`{"results":[{"entry_id":%q,"put_msg_id":"p1","seller_key":"s1",`+
			`"description":%q,`+
			`"content_hash":"sha256:ab","content_type":"code","price":120,`+
			`"confidence":0.55,"similarity":%g,`+
			`"is_partial_match":false,"seller_reputation":80,`+
			`"token_cost_original":%d,"age_hours":3}],`+
			`"search_meta":{"total_candidates":5},"guide":"..."}`,
		entryID, description, similarity, tokenCost,
	)
	return Message{
		ID:          id,
		Tags:        []string{TagMatch},
		Antecedents: []string{buyID},
		Payload:     []byte(payload),
	}
}

// TestNetTokensSaved_FixtureThreeCases is the Track C (dontguess-eff) acceptance
// test for the net-tokens-saved reporter extension. It covers the three required
// fixture scenarios from the item DONE condition:
//
//  1. Real hit (positive): above-floor delivered result + consumed → SavedOnRealHits > 0
//  2. Miss (negative): buy-miss → TotalMissCost > 0
//  3. False-positive delivery (negative): above-floor delivered + NOT consumed
//     → TotalFalsePositiveWaste > 0
//
// All three cases run through the real ComputeHitRate path — no mocks of the
// reporter under test. The consume signal is supplied via opts.ConsumeCountByEntry
// (populated by ConsumeCountByEntry from exchange:consume messages), which is the
// same path the CLI uses.
//
// The test also verifies that the existing quality-weighting (similarity floor)
// and synthetic-exclusion invariants are NOT regressed by the extension.
func TestNetTokensSaved_FixtureThreeCases(t *testing.T) {
	floor := matching.DefaultMinSimilarity()
	aboveSim := floor + 0.20
	if aboveSim > 1.0 {
		aboveSim = 1.0
	}

	// Three buy orders:
	//   buy-real-hit:         delivered above-floor, entry CONSUMED → real hit
	//   buy-false-positive:   delivered above-floor, entry NOT consumed → false positive
	//   buy-miss:             no matching inventory → miss
	buys := []Message{
		buyMsg("buy-real-hit", "campfire convention dispatch lifecycle management"),
		buyMsg("buy-false-positive", "Go flock contention test pattern"),
		buyMsg("buy-miss", "zzqq nonsense xyzzy no such cached inference"),
	}

	// Match for real hit: entry "entry-consumed", token_cost 2000.
	matchRealHit := hitMatchWithSimEntry(
		"m-real-hit", "buy-real-hit",
		"entry-consumed",
		"campfire convention dispatch lifecycle: declare, claim, revoke",
		aboveSim, 2000,
	)

	// Match for false positive: entry "entry-unconsumed", token_cost 1500.
	matchFalsePositive := hitMatchWithSimEntry(
		"m-false-positive", "buy-false-positive",
		"entry-unconsumed",
		"Go flock contention test pattern for exclusive file locks",
		aboveSim, 1500,
	)

	// Miss match: buy-miss has no matching inventory.
	matchMiss := missMatch("m-miss", "buy-miss")

	matches := []Message{matchRealHit, matchFalsePositive, matchMiss}

	// Build consume signal: only "entry-consumed" was consumed (settle-complete).
	// "entry-unconsumed" has no consume signal → false positive.
	consumeMsg := Message{
		ID:          "consume-1",
		Tags:        []string{TagConsume},
		Antecedents: []string{"settle-1"},
		Payload:     []byte(`{"entry_id":"entry-consumed","buyer_key":"buyer-key-1"}`),
	}
	consumeCounts := ConsumeCountByEntry([]Message{consumeMsg})

	opts := HitRateOptions{
		MinSimilarity:       floor,
		ConsumeCountByEntry: consumeCounts,
	}

	rep := ComputeHitRate(buys, matches, opts)

	t.Logf("floor=%.4f above=%.4f", floor, aboveSim)
	t.Logf("report: total=%d hits=%d misses=%d rate=%.2f%%", rep.TotalBuys, rep.Hits, rep.Misses, rep.HitRatePct)
	t.Logf("net savings: total=%d saved=%d miss_cost=%d fp_waste=%d",
		rep.NetTokensSaved, rep.SavedOnRealHits, rep.TotalMissCost, rep.TotalFalsePositiveWaste)

	// --- v0.5.0 invariants: quality-weighting and synthetic-exclusion NOT regressed ---
	if rep.TotalBuys != 3 {
		t.Errorf("TotalBuys = %d, want 3", rep.TotalBuys)
	}
	if rep.Hits != 2 {
		// Both above-floor delivered results count as hits (quality gate), regardless
		// of consume status. The consume signal only affects net-savings, not hit-rate.
		t.Errorf("Hits = %d, want 2 (both above-floor results pass quality gate)", rep.Hits)
	}
	if rep.Misses != 1 {
		t.Errorf("Misses = %d, want 1", rep.Misses)
	}
	if rep.SyntheticExcluded != 0 {
		t.Errorf("SyntheticExcluded = %d, want 0 (no synthetic buys in this fixture)", rep.SyntheticExcluded)
	}

	// --- Scenario 1: real hit (positive contribution) ---
	// entry-consumed has consume count > 0 → saved_on_real_hits = 2000.
	if rep.SavedOnRealHits != 2000 {
		t.Errorf("SavedOnRealHits = %d, want 2000 (entry-consumed token_cost)", rep.SavedOnRealHits)
	}

	// --- Scenario 2: miss (negative contribution) ---
	// 1 miss × DefaultMissCostPerQuery (500) = 500.
	wantMissCost := int64(1) * DefaultMissCostPerQuery
	if rep.TotalMissCost != wantMissCost {
		t.Errorf("TotalMissCost = %d, want %d (1 miss × %d per query)",
			rep.TotalMissCost, wantMissCost, DefaultMissCostPerQuery)
	}

	// --- Scenario 3: false-positive delivery (negative contribution) ---
	// entry-unconsumed was delivered (above-floor hit) but not consumed.
	// false_positive_waste = token_cost_original = 1500.
	if rep.TotalFalsePositiveWaste != 1500 {
		t.Errorf("TotalFalsePositiveWaste = %d, want 1500 (entry-unconsumed token_cost)", rep.TotalFalsePositiveWaste)
	}

	// --- Net savings: 2000 − 500 − 1500 = 0 in this fixture ---
	wantNet := int64(2000) - wantMissCost - int64(1500)
	if rep.NetTokensSaved != wantNet {
		t.Errorf("NetTokensSaved = %d, want %d (2000 − %d − 1500)",
			rep.NetTokensSaved, wantNet, wantMissCost)
	}

	// --- Per-query economics: 3 answered buys → 3 entries (hit, false_positive, miss) ---
	if len(rep.PerQueryEconomics) != 3 {
		t.Errorf("len(PerQueryEconomics) = %d, want 3", len(rep.PerQueryEconomics))
	}

	// Verify the outcomes are correctly classified in per-query economics.
	outcomeMap := make(map[string]string, 3)
	savedMap := make(map[string]int64, 3)
	for _, q := range rep.PerQueryEconomics {
		outcomeMap[q.BuyID] = q.Outcome
		savedMap[q.BuyID] = q.Saved
	}

	if outcomeMap["buy-real-hit"] != "hit" {
		t.Errorf("buy-real-hit outcome = %q, want hit", outcomeMap["buy-real-hit"])
	}
	if savedMap["buy-real-hit"] != 2000 {
		t.Errorf("buy-real-hit Saved = %d, want 2000", savedMap["buy-real-hit"])
	}

	if outcomeMap["buy-false-positive"] != "false_positive" {
		t.Errorf("buy-false-positive outcome = %q, want false_positive", outcomeMap["buy-false-positive"])
	}
	if savedMap["buy-false-positive"] != -1500 {
		t.Errorf("buy-false-positive Saved = %d, want -1500", savedMap["buy-false-positive"])
	}

	if outcomeMap["buy-miss"] != "miss" {
		t.Errorf("buy-miss outcome = %q, want miss", outcomeMap["buy-miss"])
	}
	if savedMap["buy-miss"] != -DefaultMissCostPerQuery {
		t.Errorf("buy-miss Saved = %d, want %d", savedMap["buy-miss"], -DefaultMissCostPerQuery)
	}
}

// buyMsgIDFor prefers the antecedent and falls back to payload buy_msg_id.
func TestBuyMsgIDFor(t *testing.T) {
	withAnt := hitMatch("m-1", "buy-1")
	if got := buyMsgIDFor(&withAnt); got != "buy-1" {
		t.Errorf("antecedent join: got %q, want buy-1", got)
	}

	// No antecedent, but buy_msg_id in payload (miss path).
	noAnt := Message{
		ID:      "m-2",
		Tags:    []string{TagBuyMiss, TagMatch},
		Payload: []byte(`{"buy_msg_id":"buy-2","task_hash":"abc"}`),
	}
	if got := buyMsgIDFor(&noAnt); got != "buy-2" {
		t.Errorf("payload fallback join: got %q, want buy-2", got)
	}

	// Neither — unjoinable.
	bare := Message{ID: "m-3", Tags: []string{TagMatch}, Payload: []byte(`{}`)}
	if got := buyMsgIDFor(&bare); got != "" {
		t.Errorf("unjoinable: got %q, want empty", got)
	}
}
