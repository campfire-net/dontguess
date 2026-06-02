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
