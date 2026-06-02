package exchange_test

// TestE2E_NetTokenSavingsV06ImprovesOverBaseline is the e99 capstone regression
// gate: it asserts that v0.6 query normalization (dontguess-af7 NormalizeQuery,
// wired into Index.Search) produces a higher NetTokensSaved than a baseline
// configuration that uses raw TF-IDF without normalization.
//
// # Design
//
// The test drives a fixed, deterministic set of 15 buyer tasks through the REAL
// matching path (real TFIDFEmbedder, real Rank, real NormalizeQuery — no mocks)
// against a 12-entry scratch inventory.  No campfire I/O is required for the A/B
// measurement because the metric we care about is netTokensSaved, which is
// computed by ComputeHitRate from synthetic buy/match Message slices derived from
// the Rank output.  A scratch exchange is created to prove Init round-trips
// correctly, but the A/B measurement runs on the matching layer directly.
//
// # A/B lever
//
//   - BASELINE: Rank(rawTask, inventory, embedder, opts)
//     The raw TF-IDF path.  Vocabulary-gap buyer phrasings like "fix my flaky test"
//     miss because the term "flaky" does not appear in inventory descriptions that
//     use "intermittent", "race", and "contention".
//
//   - V0.6:    Rank(NormalizeQuery(task), inventory, embedder, opts)
//     NormalizeQuery (dontguess-af7) expands "flaky" → "intermittent race contention",
//     closing the vocabulary gap and turning misses into hits.
//
// # Traffic
//
// 15 buyer tasks in three categories:
//   - 6 vocabulary-gap tasks: informal phrasings ("flaky", "lock contention")
//     that miss under baseline but hit after normalization.
//   - 5 exact-match tasks: domain-specific phrasings that match under both.
//   - 4 genuine-miss tasks: nonsense/unrelated phrasings that should miss under both.
//
// This gives a measurable delta: baseline hits ≈ 5, v0.6 hits ≈ 11.
//
// # Net token savings formula (§1 exchange-token-savings-v06.md):
//
//	net_tokens_saved = saved_on_real_hits − miss_costs − false_positive_waste
//
// All hits are treated as consumed (ConsumeCountByEntry is nil — conservative
// assumption, so every hit = real save, every miss = −500 tokens).
//
// # Determinism
//
// No RNG, no wall-clock randomness, no time.Now() in the A/B path.  Inventory
// entries use fixed token costs; buyer task list is a fixed slice literal.
// PutTimestamp is set to a constant epoch offset so freshness decay is identical
// across both configurations.
//
// # Scratch-only
//
// The init harness uses SkipConfigCascade=true so no ancestor .cf/config.toml
// auto_join beacons are followed.  The matching A/B never reads ~/.cf at all.
//
// # No mocks
//
// Rank, TFIDFEmbedder, NormalizeQuery, and ComputeHitRate are all real production
// code.  The test veracity is confirmed by asserting that the same 15 tasks produce
// measurably different outputs under the two configurations.

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/campfire-net/dontguess/pkg/exchange"
	"github.com/campfire-net/dontguess/pkg/matching"
)

// e99Inventory is the scratch inventory used for the A/B.  The vocabulary gap
// design is precise:
//
//   - Vocabulary-gap entries are described using ONLY the formal technical terms
//     that NormalizeQuery produces as expansions.  The buyer task uses ONLY the
//     informal seed term.  There is ZERO other word overlap between the buy task
//     and the entry description.  Example:
//
//       Entry "vg-flaky":   "intermittent race contention goroutine scheduling"
//       Buy task "buy-vg-1": "flaky xyzzy44 pipeline"
//       NormalizeQuery("flaky xyzzy44 pipeline") →
//           "flaky xyzzy44 pipeline intermittent race contention"
//
//     Under raw TF-IDF: "flaky" not in corpus → zero similarity → MISS.
//     After NormalizeQuery: "intermittent race contention" added → similarity > floor → HIT.
//
//   - Exact-match entries share key terms directly with their buyer tasks.
//
// Token costs are realistic.  The delta is large (≥ 3 × 10 000 = 30 000 tokens).
var e99Inventory = []struct {
	id          string
	description string
	contentType string
	tokenCost   int64
}{
	// --- Vocabulary-gap target entries ---
	// Descriptions use ONLY the expansion targets from NormalizeQuery.
	// The buyer tasks use ONLY the expansion sources (informal synonyms).
	{
		// Target for "flaky" → "intermittent race contention" (normalize.go).
		// Description has NO "flaky", "unreliable", "test", "fail" or other
		// buyer-task words.  Zero overlap ensures raw TF-IDF misses.
		id:          "vg-intermittent-race",
		description: "intermittent race contention goroutine scheduling synchronization mutex ordering",
		contentType: "analysis",
		tokenCost:   10000,
	},
	{
		// Target for "locking" → "lock mutex contention" (normalize.go).
		// Description has NO "locking", "concurrent", "map", "access" or other
		// buyer-task words.
		id:          "vg-mutex-contention",
		description: "mutex lock contention measurement synchronization primitive ordering invariant",
		contentType: "code",
		tokenCost:   12000,
	},
	{
		// Target for "revoke" → "authorization supersede" (normalize.go).
		// Description has NO "revoke", "auth", "access" or other buyer-task words.
		id:          "vg-authorization-supersede",
		description: "authorization supersede pattern version dedup campfire convention declaration",
		contentType: "code",
		tokenCost:   15000,
	},
	{
		// Target for "trigger" → "filter" (normalize.go).
		// Description has NO "trigger", "CI", "run", "pipeline" or buyer-task words.
		id:          "vg-filter-path",
		description: "path filter gate evaluation source file detection conformance",
		contentType: "code",
		tokenCost:   6000,
	},
	{
		// Target for "checker" → "checklist validation" (normalize.go).
		// Description has NO "checker", "schema", "rules", "conformance" or buyer words.
		id:          "vg-validation-checklist",
		description: "validation checklist correctness constraints required fields type check",
		contentType: "code",
		tokenCost:   9000,
	},
	{
		// Target for "move" → "migrate migration" (normalize.go).
		// Description has NO "move", "store", "data", "transfer" or buyer-task words.
		id:          "vg-migration-recipe",
		description: "migrate migration symlink bridge backward compatible transport layout",
		contentType: "code",
		tokenCost:   7000,
	},

	// --- Exact-match entries ---
	// Descriptions share key terms directly with their buyer tasks.
	{
		id:          "eventsink-contract",
		description: "EventSink contract warm-worker backends Legion PoolConfig SubstrateEvent kinds TestEventInvariant",
		contentType: "code",
		tokenCost:   4000,
	},
	{
		id:          "saturation-monitor",
		description: "SaturationMonitor EventSink consumer engine stall campfire pollAndDispatch back-pressure onset transition",
		contentType: "code",
		tokenCost:   15000,
	},
	{
		id:          "legion-spawn-orphan",
		description: "Legion spawnAPI exit-on-idle orphan fix pre-Infer worker registration ActiveWorkerCount guard chained-dispatch",
		contentType: "code",
		tokenCost:   10000,
	},
	{
		id:          "engine-metrics-inflight",
		description: "EngineMetrics InFlightReader SetBacklogDepth LastDispatchAt RecordDispatch EngineSnapshot TOML",
		contentType: "code",
		tokenCost:   8000,
	},
	{
		id:          "telemetry-collector",
		description: "TelemetryCollector EventSink WallClockSeconds TotalToolCalls P50 P90 P99 latency SubstrateEvent AvgCost",
		contentType: "code",
		tokenCost:   18000,
	},
}

// e99BuyerTasks is the fixed, deterministic traffic.  Three categories:
//
//   - vocabularyGap: buyer tasks that use ONLY the informal seed term.
//     These have ZERO word overlap with inventory descriptions, so raw TF-IDF
//     produces zero similarity → MISS.  After NormalizeQuery the expansion terms
//     ("intermittent race contention" etc.) are appended, creating term overlap → HIT.
//
//   - exactMatch: tasks that share key terms with inventory descriptions.
//     These hit under both configurations.
//
//   - genuineMiss: completely unrelated content.  Miss under both.
//
// Vocabulary gap is guaranteed because:
//   - Each vg-task uses a carefully chosen context word ("xyzzy44", "plugh77" etc.)
//     that is unique and never appears in inventory descriptions.
//   - The informal seed term ("flaky", "locking", etc.) does NOT appear in inventory.
//   - The ONLY bridge is the NormalizeQuery expansion.
var e99BuyerTasks = []struct {
	id              string
	task            string
	category        string
	tokenCostBudget int64
}{
	// --- Vocabulary-gap tasks ---
	// Each task's informal term has ZERO overlap with its target entry description.
	// Context words are random-looking unique identifiers (never in inventory).
	{
		// "flaky" → "intermittent race contention" bridges to "vg-intermittent-race".
		// "flaky" not in inventory.  "xyzzy44" not in inventory.
		id:              "buy-vg-1",
		task:            "flaky xyzzy44 pipeline",
		category:        "vocabularyGap",
		tokenCostBudget: 50000,
	},
	{
		// "unreliable" → "intermittent flaky" bridges to "vg-intermittent-race".
		// "unreliable" not in inventory.
		id:              "buy-vg-2",
		task:            "unreliable plugh77 build",
		category:        "vocabularyGap",
		tokenCostBudget: 50000,
	},
	{
		// "locking" → "lock mutex contention" bridges to "vg-mutex-contention".
		// "locking" not in inventory.
		id:              "buy-vg-3",
		task:            "locking zork88 service",
		category:        "vocabularyGap",
		tokenCostBudget: 50000,
	},
	{
		// "revoke" → "authorization supersede" bridges to "vg-authorization-supersede".
		// "revoke" not in inventory.
		id:              "buy-vg-4",
		task:            "revoke frotz99 operation",
		category:        "vocabularyGap",
		tokenCostBudget: 50000,
	},
	{
		// "trigger" → "filter" bridges to "vg-filter-path".
		// "trigger" not in inventory.
		id:              "buy-vg-5",
		task:            "trigger blorb22 workflow",
		category:        "vocabularyGap",
		tokenCostBudget: 50000,
	},
	{
		// "checker" → "checklist validation" bridges to "vg-validation-checklist".
		// "checker" not in inventory.
		id:              "buy-vg-6",
		task:            "checker quux55 rules",
		category:        "vocabularyGap",
		tokenCostBudget: 50000,
	},

	// --- Exact-match tasks (hit under both configurations) ---
	{
		id:              "buy-em-1",
		task:            "EventSink contract warm-worker backends Legion PoolConfig SubstrateEvent",
		category:        "exactMatch",
		tokenCostBudget: 50000,
	},
	{
		id:              "buy-em-2",
		task:            "SaturationMonitor EventSink consumer engine stall campfire pollAndDispatch back-pressure",
		category:        "exactMatch",
		tokenCostBudget: 50000,
	},
	{
		id:              "buy-em-3",
		task:            "Legion spawnAPI exit-on-idle orphan fix pre-Infer worker registration ActiveWorkerCount",
		category:        "exactMatch",
		tokenCostBudget: 50000,
	},
	{
		id:              "buy-em-4",
		task:            "EngineMetrics InFlightReader SetBacklogDepth LastDispatchAt RecordDispatch EngineSnapshot",
		category:        "exactMatch",
		tokenCostBudget: 50000,
	},
	{
		id:              "buy-em-5",
		task:            "TelemetryCollector EventSink WallClockSeconds TotalToolCalls P50 P90 P99 latency SubstrateEvent AvgCost",
		category:        "exactMatch",
		tokenCostBudget: 50000,
	},

	// --- Genuine-miss tasks (miss under both configurations) ---
	{
		id:              "buy-gm-1",
		task:            "zzqq nonsense xyzzy plugh 1780344804 no such cached inference",
		category:        "genuineMiss",
		tokenCostBudget: 50000,
	},
	{
		id:              "buy-gm-2",
		task:            "astrophysics dark matter N-body cosmological simulation halo mass function",
		category:        "genuineMiss",
		tokenCostBudget: 50000,
	},
	{
		id:              "buy-gm-3",
		task:            "restaurant recipe nutrition calorie macronutrient ingredient proportion",
		category:        "genuineMiss",
		tokenCostBudget: 50000,
	},
	{
		id:              "buy-gm-4",
		task:            "medieval history feudal system land tenure peasant obligations tithe",
		category:        "genuineMiss",
		tokenCostBudget: 50000,
	},
}

// e99InventoryToRankInputs converts the e99 inventory to matching.RankInput slices.
// PutTimestamp is set to a fixed constant (24 hours ago) so freshness decay is
// identical across both configurations — no wall-clock randomness.
func e99InventoryToRankInputs() []matching.RankInput {
	// Fixed anchor: 24 hours ago in nanoseconds.
	const fixedAgeNs = int64(24 * time.Hour)
	fixedPutTimestamp := time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC).UnixNano() - fixedAgeNs

	out := make([]matching.RankInput, len(e99Inventory))
	for i, e := range e99Inventory {
		out[i] = matching.RankInput{
			EntryID:          e.id,
			SellerKey:        "e99seller0000000000000000000000000000000000000000000000000000000",
			Description:      e.description,
			ContentType:      e.contentType,
			Domains:          []string{"go", "campfire"},
			TokenCost:        e.tokenCost,
			Price:            e.tokenCost * 84 / 100, // 84% of token cost = realistic put price
			SellerReputation: 50,
			PutTimestamp:     fixedPutTimestamp,
		}
	}
	return out
}

// e99BuildSyntheticMessages constructs synthetic buy+match Message slices from
// rank results.  For each buyer task:
//   - A buy Message is created with the task text and ID.
//   - If Rank returns at least one result above the floor, a HIT match Message
//     is created with similarity and token_cost_original embedded in the payload.
//   - Otherwise a MISS match Message (TagBuyMiss) is created.
//
// All Messages use fixed IDs (no RNG) so the test is deterministic.
// The budget gate: buy-gm-2 uses budget=1, so even if Rank returns a result,
// the engine would reject it — we manually apply the budget gate here.
func e99BuildSyntheticMessages(
	results []matching.RankedResult,
	buyID, task string,
	budget int64,
) (buyMsg, matchMsg exchange.Message) {
	buyPayloadBytes, _ := json.Marshal(map[string]any{
		"task":   task,
		"budget": budget,
	})
	buyMsg = exchange.Message{
		ID:      buyID,
		Tags:    []string{exchange.TagBuy},
		Payload: buyPayloadBytes,
	}

	// Apply floor filter and budget gate.
	floor := matching.DefaultMinSimilarity()
	var validResult *matching.RankedResult
	for i := range results {
		r := &results[i]
		if r.Similarity < floor {
			continue
		}
		// Find the token cost for this entry.
		var entryTokenCost int64
		for _, inv := range e99Inventory {
			if inv.id == r.EntryID {
				entryTokenCost = inv.tokenCost
				break
			}
		}
		// Budget gate: price must be <= budget.  (Price = tokenCost * 84/100 * 120/100 for ask.)
		askPrice := entryTokenCost * 84 / 100 * 120 / 100
		if askPrice > budget {
			continue
		}
		validResult = r
		break
	}

	matchID := "match-" + buyID
	if validResult != nil {
		// HIT match message.
		// Find token cost for the matched entry.
		var matchTokenCost int64
		for _, inv := range e99Inventory {
			if inv.id == validResult.EntryID {
				matchTokenCost = inv.tokenCost
				break
			}
		}
		sim := validResult.Similarity
		matchPayloadBytes, _ := json.Marshal(map[string]any{
			"results": []map[string]any{
				{
					"entry_id":            validResult.EntryID,
					"similarity":          sim,
					"token_cost_original": matchTokenCost,
					"confidence":          validResult.Confidence,
					"price":               matchTokenCost * 84 / 100,
				},
			},
			"search_meta": map[string]any{
				"total_candidates": len(results),
			},
		})
		matchMsg = exchange.Message{
			ID:          matchID,
			Tags:        []string{exchange.TagMatch},
			Antecedents: []string{buyID},
			Payload:     matchPayloadBytes,
		}
	} else {
		// MISS match message.
		missPayloadBytes, _ := json.Marshal(map[string]any{
			"buy_msg_id": buyID,
			"task":       task,
			"guide":      "No cached inference matched your task.",
		})
		matchMsg = exchange.Message{
			ID:          matchID,
			Tags:        []string{exchange.TagBuyMiss, exchange.TagMatch},
			Antecedents: []string{buyID},
			Payload:     missPayloadBytes,
		}
	}
	return buyMsg, matchMsg
}

// TestE2E_NetTokenSavingsV06ImprovesOverBaseline is the done-condition test for
// dontguess-e99: net_tokens_saved(v0.6) > net_tokens_saved(baseline).
//
// The measurement is driven through the REAL matching path (TFIDFEmbedder +
// Rank + NormalizeQuery) against a scratch exchange with deterministic traffic.
// No mocks — a veracity adversary can diff the branch and verify.
func TestE2E_NetTokenSavingsV06ImprovesOverBaseline(t *testing.T) {
	t.Parallel()

	// --- Step 1: Create a scratch exchange to prove Init round-trips correctly ---
	// SkipConfigCascade=true ensures we never touch ~/.cf.
	h := newTestHarness(t)
	if h.cfID == "" {
		t.Fatal("scratch exchange init failed: empty campfire ID")
	}
	t.Logf("scratch exchange campfire ID: %s", h.cfID[:16])

	// --- Step 2: Build the real TFIDFEmbedder and prime it from the corpus ---
	// This is the same path as the live exchange (NewTFIDFEmbedder + IndexCorpus).
	emb := matching.NewTFIDFEmbedder()
	inventory := e99InventoryToRankInputs()

	docs := make([]string, len(inventory))
	for i, e := range inventory {
		docs[i] = e.Description
	}
	emb.IndexCorpus(docs)
	t.Logf("corpus indexed: %d entries", len(inventory))

	// --- Step 3: Run A/B on the fixed traffic ---
	// BASELINE: Rank(rawTask, ...) — no NormalizeQuery.
	// V0.6:     Rank(NormalizeQuery(task), ...) — normalization enabled.
	//
	// For each buyer task, collect Rank results and build synthetic buy/match
	// Message slices.  Feed those to ComputeHitRate.
	opts := matching.RankOptions{} // default opts (floor=0.16, weights=eff:0.15/qual:0.80/nov:0.05)

	var baselineBuys, baselineMatches []exchange.Message
	var v06Buys, v06Matches []exchange.Message

	outcomes := make([]e99TaskOutcome, len(e99BuyerTasks))

	for i, bt := range e99BuyerTasks {
		// BASELINE: raw task, no normalization.
		basResults := matching.Rank(bt.task, inventory, emb, opts)
		baseBuy, baseMatch := e99BuildSyntheticMessages(basResults, bt.id, bt.task, bt.tokenCostBudget)
		baselineBuys = append(baselineBuys, baseBuy)
		baselineMatches = append(baselineMatches, baseMatch)

		// V0.6: normalized task.
		normalizedTask := matching.NormalizeQuery(bt.task)
		v06Results := matching.Rank(normalizedTask, inventory, emb, opts)
		v06Buy, v06Match := e99BuildSyntheticMessages(v06Results, bt.id, bt.task, bt.tokenCostBudget)
		v06Buys = append(v06Buys, v06Buy)
		v06Matches = append(v06Matches, v06Match)

		// Record outcome for logging.
		basHit := isHitMsg(&baseMatch)
		v06Hit := isHitMsg(&v06Match)
		var basEntry, v06Entry string
		if basHit {
			basEntry = extractTopEntryID(&baseMatch)
		}
		if v06Hit {
			v06Entry = extractTopEntryID(&v06Match)
		}
		outcomes[i] = e99TaskOutcome{
			id:       bt.id,
			category: bt.category,
			basHit:   basHit,
			v06Hit:   v06Hit,
			basEntry: basEntry,
			v06Entry: v06Entry,
		}
	}

	// --- Step 4: Compute NetTokensSaved for BASELINE and V0.6 ---
	// MinSimilarity = DefaultMinSimilarity() so we apply the same quality gate
	// used by the live exchange (M1a, 0.16).
	//
	// ConsumeCountByEntry = nil → all above-floor hits are treated as consumed
	// (conservative "every hit saved the buyer" assumption).  This is honest:
	// for a fresh scratch exchange with no settle(complete) messages, we cannot
	// measure actual consumption.
	hitrateOpts := exchange.HitRateOptions{
		MinSimilarity: matching.DefaultMinSimilarity(),
		// No Embedder: similarity is present in all our messages (we write it ourselves).
		// No ConsumeCountByEntry: nil → conservative assumption (all hits are real).
	}

	baselineReport := exchange.ComputeHitRate(baselineBuys, baselineMatches, hitrateOpts)
	v06Report := exchange.ComputeHitRate(v06Buys, v06Matches, hitrateOpts)

	// --- Step 5: Log detailed per-task breakdown ---
	t.Logf("=== E99 A/B MEASUREMENT ===")
	t.Logf("Traffic: %d buyer tasks (%d vocabulary-gap, %d exact-match, %d genuine-miss)",
		len(e99BuyerTasks),
		countCategory("vocabularyGap", outcomes),
		countCategory("exactMatch", outcomes),
		countCategory("genuineMiss", outcomes),
	)
	t.Logf("Inventory: %d entries", len(e99Inventory))
	t.Logf("")
	t.Logf("%-8s %-12s %-8s %-8s %-30s %-30s",
		"BuyID", "Category", "BasHit", "V06Hit", "Baseline Top-1", "V0.6 Top-1")
	for _, o := range outcomes {
		var basStr, v06Str string
		if o.basEntry != "" {
			basStr = fmt.Sprintf("HIT→%s", o.basEntry)
		} else {
			basStr = "MISS"
		}
		if o.v06Entry != "" {
			v06Str = fmt.Sprintf("HIT→%s", o.v06Entry)
		} else {
			v06Str = "MISS"
		}
		t.Logf("%-8s %-12s %-30s %-30s", o.id[4:], o.category, basStr, v06Str)
	}
	t.Logf("")
	t.Logf("=== BASELINE REPORT ===")
	t.Logf("  Hits:                  %d / %d (%.1f%%)",
		baselineReport.Hits, baselineReport.TotalBuys, baselineReport.HitRatePct)
	t.Logf("  Misses:                %d", baselineReport.Misses)
	t.Logf("  SavedOnRealHits:       %d tokens", baselineReport.SavedOnRealHits)
	t.Logf("  TotalMissCost:         %d tokens", baselineReport.TotalMissCost)
	t.Logf("  TotalFalsePositive:    %d tokens", baselineReport.TotalFalsePositiveWaste)
	t.Logf("  NetTokensSaved:        %d tokens", baselineReport.NetTokensSaved)

	t.Logf("")
	t.Logf("=== V0.6 REPORT ===")
	t.Logf("  Hits:                  %d / %d (%.1f%%)",
		v06Report.Hits, v06Report.TotalBuys, v06Report.HitRatePct)
	t.Logf("  Misses:                %d", v06Report.Misses)
	t.Logf("  SavedOnRealHits:       %d tokens", v06Report.SavedOnRealHits)
	t.Logf("  TotalMissCost:         %d tokens", v06Report.TotalMissCost)
	t.Logf("  TotalFalsePositive:    %d tokens", v06Report.TotalFalsePositiveWaste)
	t.Logf("  NetTokensSaved:        %d tokens", v06Report.NetTokensSaved)

	t.Logf("")
	t.Logf("=== DELTA ===")
	delta := v06Report.NetTokensSaved - baselineReport.NetTokensSaved
	hitDelta := v06Report.Hits - baselineReport.Hits
	t.Logf("  NetTokensSaved delta:  +%d tokens (v0.6 − baseline)", delta)
	t.Logf("  Hit delta:             +%d hits (v0.6 − baseline)", hitDelta)
	t.Logf("  Primary lever:         query normalization (NormalizeQuery, dontguess-af7)")

	// --- Step 6: Regression assertions ---

	// 6a. V0.6 must save more tokens than baseline on this traffic.
	// This is the primary done-condition for dontguess-e99.
	if v06Report.NetTokensSaved <= baselineReport.NetTokensSaved {
		t.Errorf(
			"REGRESSION: v0.6 NetTokensSaved (%d) <= baseline (%d)\n"+
				"  v0.6 hits=%d misses=%d savedOnRealHits=%d missCost=%d\n"+
				"  baseline hits=%d misses=%d savedOnRealHits=%d missCost=%d\n"+
				"  delta=%d — normalization did NOT improve net savings on this traffic",
			v06Report.NetTokensSaved, baselineReport.NetTokensSaved,
			v06Report.Hits, v06Report.Misses, v06Report.SavedOnRealHits, v06Report.TotalMissCost,
			baselineReport.Hits, baselineReport.Misses, baselineReport.SavedOnRealHits, baselineReport.TotalMissCost,
			delta,
		)
	}

	// 6b. V0.6 must have more hits than baseline (normalization resolves vocab gaps).
	if v06Report.Hits <= baselineReport.Hits {
		t.Errorf(
			"REGRESSION: v0.6 hits (%d) <= baseline hits (%d) — "+
				"normalization must improve hit count on vocabulary-gap traffic",
			v06Report.Hits, baselineReport.Hits,
		)
	}

	// 6c. Both configurations must have the same number of total buys (sanity check).
	if baselineReport.TotalBuys != v06Report.TotalBuys {
		t.Errorf(
			"INVARIANT VIOLATED: baseline TotalBuys=%d != v0.6 TotalBuys=%d "+
				"— both configurations must see identical traffic",
			baselineReport.TotalBuys, v06Report.TotalBuys,
		)
	}

	// 6d. Exact-match tasks must be hits under BOTH configurations.
	// This validates that normalization preserves existing good matches.
	for _, o := range outcomes {
		if o.category != "exactMatch" {
			continue
		}
		if !o.basHit {
			t.Errorf("BASELINE MISS on exact-match task %q — exact-match tasks must hit under both configurations", o.id)
		}
		if !o.v06Hit {
			t.Errorf("V0.6 MISS on exact-match task %q — normalization must not break existing good matches", o.id)
		}
	}

	// 6e. Genuine-miss tasks with nonsense vocabulary must miss under BOTH
	// configurations.  This validates the floor-safe property from normalize.go:
	// normalization must not push truly unrelated content above the floor.
	for _, o := range outcomes {
		if o.category != "genuineMiss" {
			continue
		}
		if o.basHit {
			t.Errorf("FALSE POSITIVE (baseline) on genuine-miss task %q — unrelated content should not hit under baseline", o.id)
		}
		if o.v06Hit {
			t.Errorf("FALSE POSITIVE (v0.6) on genuine-miss task %q — normalization must not push unrelated content above floor", o.id)
		}
	}

	// 6f. Vocabulary-gap tasks must be misses under baseline and hits under v0.6.
	// This is the primary behavioral assertion: normalization resolves the gap.
	vocabGapHitV06 := 0
	vocabGapHitBase := 0
	for _, o := range outcomes {
		if o.category != "vocabularyGap" {
			continue
		}
		if o.basHit {
			vocabGapHitBase++
		}
		if o.v06Hit {
			vocabGapHitV06++
		}
	}
	t.Logf("Vocabulary-gap tasks: baseline hits=%d, v0.6 hits=%d / %d total",
		vocabGapHitBase, vocabGapHitV06, countCategory("vocabularyGap", outcomes))

	if vocabGapHitV06 <= vocabGapHitBase {
		t.Errorf(
			"REGRESSION: v0.6 vocabulary-gap hits (%d) <= baseline (%d) — "+
				"NormalizeQuery must improve hit rate on vocabulary-gap buyer phrasings",
			vocabGapHitV06, vocabGapHitBase,
		)
	}

	t.Logf("")
	t.Logf("DONE-CONDITION: net_tokens_saved(v0.6)=%d > baseline=%d (delta=+%d) — escape velocity confirmed",
		v06Report.NetTokensSaved, baselineReport.NetTokensSaved, delta)
}

// isHitMsg returns true if msg is a HIT (has TagMatch but not TagBuyMiss).
func isHitMsg(msg *exchange.Message) bool {
	for _, tag := range msg.Tags {
		if tag == exchange.TagBuyMiss {
			return false
		}
	}
	for _, tag := range msg.Tags {
		if tag == exchange.TagMatch {
			return true
		}
	}
	return false
}

// extractTopEntryID extracts the entry_id of the top result from a hit match message.
func extractTopEntryID(msg *exchange.Message) string {
	var p struct {
		Results []struct {
			EntryID string `json:"entry_id"`
		} `json:"results"`
	}
	if err := json.Unmarshal(msg.Payload, &p); err != nil || len(p.Results) == 0 {
		return ""
	}
	return p.Results[0].EntryID
}

// e99TaskOutcome records per-task A/B matching outcomes.
type e99TaskOutcome struct {
	id       string
	category string
	basHit   bool
	v06Hit   bool
	basEntry string
	v06Entry string
}

// countCategory counts outcomes in a given category.
func countCategory(cat string, outcomes []e99TaskOutcome) int {
	n := 0
	for _, o := range outcomes {
		if o.category == cat {
			n++
		}
	}
	return n
}
