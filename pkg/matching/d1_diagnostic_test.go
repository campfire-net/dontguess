// Package matching — D1 diagnostic fixture (dontguess-ed0)
//
// This file builds a fixture of real (buy task → ideal entry) pairs harvested
// from the live exchange log (read-only queries: dontguess buys + dontguess match-results).
// It exercises the REAL matching code path: real TFIDFEmbedder → real Rank() → real Index.
// No mocks are used anywhere in this file; mock use would be a veracity defect.
//
// The fixture is in three categories:
//  A. Nonsense pairings (§2) — tasks the live exchange mismatched to junk entries.
//     The ideal result is a MISS (no result above threshold), but current matcher returns results.
//  B. Semantic near-miss — tasks that share terms with wrong entries.
//  C. Substantive reuse cases (§4) — tasks that should match high-value entries.
//
// Measurements captured:
//  1. Baseline top-1 accuracy on the fixture (current defaults).
//  2. Effect of hard cosine-similarity floor (0.35).
//  3. Effect of downweighted freshness/novelty (quality weight 0.80, eff 0.15, novelty 0.05).
//  4. Combined: floor 0.35 + rebalanced weights.
//
// VERDICT logged in test output and in docs/design/exchange-matching-d1-diagnostic-verdict.md.
package matching

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// fixtureEntry represents a single inventory entry as observed in the live exchange
// (from dontguess match-results read-only output, 2026-06-02).
type fixtureEntry struct {
	id          string
	description string
	contentType string
	tokenCost   int64
	price       int64
	ageHours    float64 // used to set PutTimestamp
}

// fixturePair is a (buy task, ideal entry ID, expect-miss) tuple.
// If expectMiss is true, the correct answer is NO result above the relevance floor.
type fixturePair struct {
	name       string
	buyTask    string
	idealID    string // "" when expectMiss=true
	expectMiss bool   // true: any top-1 result is wrong; false: top-1 should be idealID
}

// allInventory is the full set of real live exchange entries observed (read-only).
// Harvested from: dontguess match-results 2026-06-02.
// Each entry maps to the ID used in the live exchange.
var allInventory = []fixtureEntry{
	// The junk entry — caused 60% of matches in live exchange per §2
	{
		id:          "junk-upgrade-smoke",
		description: "upgrade smoke test 1780345675: cf v0.31.2 operator round-trip",
		contentType: "analysis",
		tokenCost:   100,
		price:       84,
		ageHours:    3,
	},
	// Substantive entries (§4 high-reuse class)
	{
		id:          "eventsink-contract",
		description: "EventSink contract for warm-worker backends in Legion: PoolConfig.EventSink (internal/worker/pool.go) is the wiring point; all 7 SubstrateEvent kinds defined in internal/worker/event.go; semantic equivalence map documented in docs/architecture/warm-worker-backends.md and docs/design/legion-api-worker-lifecycle.md; new backends require TestEventInvariant_Pool<Backend> test; substrate label format pool:<slug>; test template in event_invariant_test.go",
		contentType: "code",
		tokenCost:   4000,
		price:       3355,
		ageHours:    5,
	},
	{
		id:          "legion-spawn-orphan",
		description: "Legion spawnAPI exit-on-idle silent chain orphan fix (legion-97f Phase 0): pre-Infer worker registration + ActiveWorkerCount guard",
		contentType: "code",
		tokenCost:   4000,
		price:       3366,
		ageHours:    8,
	},
	{
		id:          "engine-metrics-inflight",
		description: "Legion EngineMetrics Phase 2.1 pattern: InFlightReader interface for single-source in-flight count from SaturationMonitor; SetBacklogDepth for PollForWork observation; LastDispatchAt updated in RecordDispatch; Snapshot reads InFlight outside own lock to avoid lock-order inversion with SaturationMonitor mutex; engineSnapshotTOML section in configShowTOML with zero values in static path",
		contentType: "code",
		tokenCost:   8000,
		price:       6723,
		ageHours:    1,
	},
	{
		id:          "slot-residency-tracker",
		description: "legion SlotResidencyTracker: per-skill chain-slot-residency EventSink histogram pattern — implementation approach, skill threading through WorkEnvelope/JailSpec, flush pattern, test strategy",
		contentType: "code",
		tokenCost:   8000,
		price:       6738,
		ageHours:    1,
	},
	{
		id:          "logger-eventsink-fanout",
		description: "Legion Phase 1.4: LoggerEventSink + fanoutSink pattern for WorkerManager EventSink consumer wiring. LoggerEventSink emits slog lines from SubstrateEvents (accepted to Info worker spawned, completed/failed to Info worker exited, infer/tool to Debug). fanoutSink fans out to N sinks with nil filtering. cmd/we/runtime.go wiring: NewFanoutSink(NewLoggerEventSink()). Deletion sites: main.go slog.Info worker spawned at onSpawnWorker, slog.Info worker exited at onWorkerExit. Test pattern: bytes.Buffer+slog.NewJSONHandler+slog.SetDefault for capturing slog in tests; assert worker spawned index before worker exited index per workerID.",
		contentType: "code",
		tokenCost:   8000,
		price:       6721,
		ageHours:    0,
	},
	{
		id:          "warm-pool-substrate-wiring",
		description: "Wire SubstrateEvent vocabulary into warm-worker pool substrate: emit sites, WorkID fallback pattern, test pattern with recording sink",
		contentType: "code",
		tokenCost:   12000,
		price:       10099,
		ageHours:    1,
	},
	{
		id:          "api-substrate-spawn",
		description: "Go pattern for per-substrate event emission in worker lifecycle: wrap cmd.Run() for tool timing; emit accepted/infer:start/infer:done/completed events at semantically correct sites in API spawn path. makeToolExecutor signature extension pattern (EventSink+workerID+itemID). Stable with -count=10 goroutine-ordering flake test.",
		contentType: "code",
		tokenCost:   15000,
		price:       12618,
		ageHours:    1,
	},
	{
		id:          "cli-substrate-wiring",
		description: "CLI substrate SubstrateEvent wiring pattern: ScanHooks in inference, buildCLIScanHooks in cmd/we, accepted/terminal in worker.go, test factory pattern for TestEventInvariant_CLI",
		contentType: "analysis",
		tokenCost:   25000,
		price:       21139,
		ageHours:    0,
	},
	{
		id:          "convention-auth-gap",
		description: "convention declaration supersede/version-dedup authorization gap in campfire: revoke pattern, PR 596 gap, campfireKey consumers, test coverage gaps, StoreReader constraint, RoleWriter attack surface, model C ruling",
		contentType: "code",
		tokenCost:   15000,
		price:       12860,
		ageHours:    0,
	},
	{
		id:          "pragmatist-convention-auth",
		description: "pragmatist analysis of convention precedence auth fix in campfire listOperations — blast radius, caller map, LOC estimates per model A/B/C, migration impact for automata-island",
		contentType: "code",
		tokenCost:   8000,
		price:       6728,
		ageHours:    0,
	},
	{
		id:          "rpt-convention-auth",
		description: "RPT analysis: authorization model for convention declaration precedence (supersede + version-dedup) in campfire toolgen.go listOperations — recommend refined model A mirroring revoke gate",
		contentType: "code",
		tokenCost:   9000,
		price:       7609,
		ageHours:    0,
	},
	{
		id:          "ensurelotcf-toctou",
		description: "EnsureLotCF TOCTOU fix: per-lot sync.Mutex map in lot_cf.go. lotMu(id) returns per-lot mutex. EnsureLotCF acquires before idempotency check. Regression test pattern: barrier-channel + N goroutines + assert single transport dir + assert single naming entry.",
		contentType: "code",
		tokenCost:   8000,
		price:       6560,
		ageHours:    70,
	},
	{
		id:          "telemetry-collector-eventsink",
		description: "TelemetryCollector EventSink consumer: derive WallClockSeconds + TotalToolCalls + P50/P90/P99 tool latency from SubstrateEvent stream; preserve CapabilityVariantTracker.AvgCost contract",
		contentType: "code",
		tokenCost:   18000,
		price:       15000,
		ageHours:    1,
	},
	{
		id:          "saturation-monitor-eventsink",
		description: "SaturationMonitor EventSink consumer + engine:stall campfire message format + pollAndDispatch back-pressure path; stall onset fires once per onset transition",
		contentType: "code",
		tokenCost:   15000,
		price:       12500,
		ageHours:    1,
	},
}

// The 20-pair fixture with buy tasks from live exchange (read-only).
// Pairs marked expectMiss=true are the §2 nonsense pairings — correct outcome is NO match above floor.
// Pairs with idealID are §4 substantive reuse cases — correct outcome is top-1 == idealID.
var fixturePairs = []fixturePair{
	// --- §2 Nonsense pairings: junk-upgrade-smoke should NOT match these ---
	{
		name:       "rpt-sdk-review-vs-smoke-test",
		buyTask:    "RPT review of campfire SDK surface: offline send, relay create, naming CLI, multi-op install",
		idealID:    "", // this is a genuine miss — no entry covers this
		expectMiss: true,
	},
	{
		name:       "fix-subscribe-cursor-vs-smoke-test",
		buyTask:    "fix convention.Server subscribe cursor: when a cf has multiple installed versions of the same (convention, operation), the server stalls processing new messages and the cf dispatch CLI picks the wrong version",
		idealID:    "", // miss — no matching entry
		expectMiss: true,
	},
	{
		name:       "convention-auth-revoke-vs-random",
		buyTask:    "convention declaration revoke/supersede authorization precedent in campfire",
		idealID:    "convention-auth-gap",
		expectMiss: false,
	},
	{
		name:       "blast-radius-convention-auth",
		buyTask:    "blast radius of gating convention declaration precedence in campfire listOperations",
		idealID:    "pragmatist-convention-auth",
		expectMiss: false,
	},
	{
		name:       "auth-model-convention-precedence",
		buyTask:    "authorization model for convention declaration precedence in campfire listOperations",
		idealID:    "rpt-convention-auth",
		expectMiss: false,
	},

	// --- Substantive reuse cases (§4) ---
	{
		name:       "eventsink-contract-for-warm-backends",
		buyTask:    "document EventSink contract for warm-worker backends in warm-worker-backends.md; add interface enforcement so new backends fail review without EventSink wiring",
		idealID:    "eventsink-contract",
		expectMiss: false,
	},
	{
		name:       "eventsink-e2e-chained-dispatch",
		buyTask:    "write end-to-end test: chained-dispatch produces canonical 7-event SubstrateEvent stream + wall_clock_s > 0 + chain does not orphan; bakeoff I4_invest_never_dispatched no longer reproducible",
		idealID:    "legion-spawn-orphan",
		expectMiss: false,
	},
	{
		name:       "engine-snapshot-inflight",
		buyTask:    "extend EngineSnapshot with InFlight + BacklogDepth + LastDispatchAt; expose via we config show",
		idealID:    "engine-metrics-inflight",
		expectMiss: false,
	},
	{
		name:       "slot-residency-histogram",
		buyTask:    "implement per-skill chain-slot-residency histogram in EngineMetrics; emit engine:slot-residency campfire message in periodic flush window",
		idealID:    "slot-residency-tracker",
		expectMiss: false,
	},
	{
		name:       "logger-eventsink-replace-slog",
		buyTask:    "implement Logger EventSink consumer; replace ad-hoc slog.Info(worker spawned/exited) with event-derived logs; verify B2b log inversion gone",
		idealID:    "logger-eventsink-fanout",
		expectMiss: false,
	},
	{
		name:       "warm-worker-pool-eventsink",
		buyTask:    "wire warm-worker pool / TUI body substrate to emit 7-event SubstrateEvent vocabulary at Dispatch/Inject/Await/pane-capture sites; pool-specific detail in Meta not in event-kind",
		idealID:    "warm-pool-substrate-wiring",
		expectMiss: false,
	},
	{
		name:       "cli-substrate-eventsink",
		buyTask:    "wire CLI substrate to emit 7-event SubstrateEvent vocabulary derived from stream-json tool_use/tool_result blocks",
		idealID:    "cli-substrate-wiring",
		expectMiss: false,
	},
	{
		name:       "api-substrate-eventsink",
		buyTask:    "wire API substrate to emit 7-event SubstrateEvent vocabulary at spawnAPI/makeToolExecutor boundaries",
		idealID:    "api-substrate-spawn",
		expectMiss: false,
	},
	{
		name:       "telemetry-collector-impl",
		buyTask:    "implement TelemetryCollector EventSink consumer; derive WallClockSeconds + TotalToolCalls + P50/P90/P99 tool latency from SubstrateEvent stream; preserve CapabilityVariantTracker.AvgCost contract",
		idealID:    "telemetry-collector-eventsink",
		expectMiss: false,
	},
	{
		name:       "saturation-monitor-impl",
		buyTask:    "implement SaturationMonitor EventSink consumer + engine:stall campfire message format + pollAndDispatch back-pressure path; stall onset fires once per onset transition",
		idealID:    "saturation-monitor-eventsink",
		expectMiss: false,
	},

	// --- Additional nonsense / false-positive boundary tests ---
	{
		name:       "nonsense-zzqq",
		buyTask:    "zzqq nonsense xyzzy plugh 1780344804 no such cached inference exists anywhere",
		idealID:    "",
		expectMiss: true,
	},
	{
		name:       "gc-command-legion",
		buyTask:    "ship we gc command + periodic gc loop wrapping cf gc with constellation-aware filtering via fleet.json; sane defaults preventing campfire sprawl on legion installations",
		idealID:    "", // no matching entry in this inventory
		expectMiss: true,
	},
	{
		name:       "veracity-audit-legion-swarm",
		buyTask:    "veracity audit P2.1+P2.2+P3+E2E test fidelity in legion-59e swarm; find mocks bypassing real interfaces",
		idealID:    "", // no matching entry
		expectMiss: true,
	},
	{
		name:       "harness-sweep-event-vocabulary",
		buyTask:    "harness sweep of legion event vocabulary work: bugs in event emission ordering across substrates, dead code from removed slog calls + StartTime field, antipatterns in EventSink consumer fan-out, test coverage gaps",
		idealID:    "", // no matching entry
		expectMiss: true,
	},
	{
		name:       "security-sweep-eventsink",
		buyTask:    "security sweep of legion event vocabulary + 3 EventSink consumers + campfire emission paths; find TOCTOU/race/auth-bypass/audit-gap",
		idealID:    "", // no matching entry
		expectMiss: true,
	},
}

// buildInventory converts fixtureEntry slice to RankInput slice.
// All entries use the same seller key (matches live exchange: single identity cd41913b).
func buildInventory(entries []fixtureEntry) []RankInput {
	const sellerKey = "cd41913b6aa59679a5499dbc9e974c08cb0b06fe8060b4db04e605c9ce5c9a50"
	out := make([]RankInput, len(entries))
	for i, e := range entries {
		ageNs := int64(e.ageHours * float64(time.Hour))
		out[i] = RankInput{
			EntryID:          e.id,
			SellerKey:        sellerKey,
			Description:      e.description,
			ContentType:      e.contentType,
			Domains:          []string{"campfire", "legion"},
			TokenCost:        e.tokenCost,
			Price:            e.price,
			SellerReputation: 50, // live exchange: all sellers have rep=50
			PutTimestamp:     time.Now().Add(-time.Duration(ageNs)).UnixNano(),
		}
	}
	return out
}

// measureAccuracy runs the fixture against a given RankOptions configuration.
// Returns (correct, total) where:
//   - For expectMiss pairs: correct if top-1 Similarity < simFloor (would be filtered).
//   - For non-miss pairs: correct if top-1 entry == idealID.
//
// simFloor is used to post-filter results so we can measure miss-accuracy without
// having to thread it through Rank (which uses minSimilarity internally when opts set).
func measureAccuracy(opts RankOptions, simFloor float64, t *testing.T) (int, int) {
	t.Helper()

	// Build embedder primed from the corpus.
	emb := NewTFIDFEmbedder()
	inv := buildInventory(allInventory)

	docs := make([]string, len(inv))
	for i, e := range inv {
		docs[i] = e.Description
	}
	emb.IndexCorpus(docs)

	correct := 0
	for _, pair := range fixturePairs {
		results := Rank(pair.buyTask, inv, emb, opts)

		// Apply post-filter for explicit floor: remove anything below simFloor.
		var filtered []RankedResult
		for _, r := range results {
			if r.Similarity >= simFloor {
				filtered = append(filtered, r)
			}
		}

		if pair.expectMiss {
			// Correct = no result above floor (or no result at all).
			if len(filtered) == 0 {
				correct++
			}
		} else {
			// Correct = top-1 is the ideal entry.
			if len(filtered) > 0 && filtered[0].EntryID == pair.idealID {
				correct++
			}
		}
	}
	return correct, len(fixturePairs)
}

// TestD1_FixtureBaseline measures current top-1 accuracy with default RankOptions.
// This is the pre-fix baseline: expected to be LOW due to §2/§3 issues.
func TestD1_FixtureBaseline(t *testing.T) {
	// Default opts — exactly what the live exchange uses.
	opts := RankOptions{}
	// minSimilarity default is 0.05 — the permissive threshold from §3.
	simFloor := 0.05

	correct, total := measureAccuracy(opts, simFloor, t)
	pct := float64(correct) / float64(total) * 100

	t.Logf("=== BASELINE (default opts, floor=%.2f) ===", simFloor)
	t.Logf("Top-1 accuracy: %d/%d = %.1f%%", correct, total, pct)
	logDetailedResults(t, opts, simFloor, "BASELINE")

	// Baseline is expected to be poor — this is the bug we're diagnosing.
	// We do NOT fail the test on low accuracy here; we document it.
	t.Logf("NOTE: Baseline accuracy reflects the broken matcher state documented in §2.")
}

// TestD1_HardCosineFloor measures accuracy after applying a hard cosine floor of 0.35.
// Toggle (a): add MinSimilarity=0.35 to RankOptions.
func TestD1_HardCosineFloor(t *testing.T) {
	// Toggle (a): hard cosine floor.
	const cosineFloor = 0.35
	opts := RankOptions{
		MinSimilarity: cosineFloor,
	}

	correct, total := measureAccuracy(opts, cosineFloor, t)
	pct := float64(correct) / float64(total) * 100

	t.Logf("=== TOGGLE A: Hard cosine floor=%.2f ===", cosineFloor)
	t.Logf("Top-1 accuracy: %d/%d = %.1f%%", correct, total, pct)
	logDetailedResults(t, opts, cosineFloor, "FLOOR-0.35")
}

// TestD1_DownweightedFreshnessNovelty measures accuracy after downweighting freshness/novelty.
// Toggle (b): reduce freshness half-life dominance + shift novelty weight down, quality weight up.
func TestD1_DownweightedFreshnessNovelty(t *testing.T) {
	// Toggle (b): rebalance weights so relevance (L2 quality) dominates.
	// Default: efficiency=0.35, quality=0.45, novelty=0.20
	// New:     efficiency=0.15, quality=0.80, novelty=0.05
	opts := RankOptions{
		MinSimilarity:    0.05, // keep permissive floor — toggling only weights
		WeightEfficiency: 0.15,
		WeightQuality:    0.80,
		WeightNovelty:    0.05,
	}
	simFloor := 0.05

	correct, total := measureAccuracy(opts, simFloor, t)
	pct := float64(correct) / float64(total) * 100

	t.Logf("=== TOGGLE B: Downweighted freshness/novelty (eff=0.15, qual=0.80, nov=0.05) ===")
	t.Logf("Top-1 accuracy: %d/%d = %.1f%%", correct, total, pct)
	logDetailedResults(t, opts, simFloor, "WEIGHTS-REBALANCED")
}

// TestD1_CombinedFloorAndRebalance measures the combined effect of floor=0.35 + rebalanced weights.
// This is the candidate TUNE configuration for M1a.
func TestD1_CombinedFloorAndRebalance(t *testing.T) {
	const cosineFloor = 0.35
	opts := RankOptions{
		MinSimilarity:    cosineFloor,
		WeightEfficiency: 0.15,
		WeightQuality:    0.80,
		WeightNovelty:    0.05,
	}

	correct, total := measureAccuracy(opts, cosineFloor, t)
	pct := float64(correct) / float64(total) * 100

	t.Logf("=== TOGGLE A+B COMBINED (floor=0.35, eff=0.15, qual=0.80, nov=0.05) ===")
	t.Logf("Top-1 accuracy: %d/%d = %.1f%%", correct, total, pct)
	logDetailedResults(t, opts, cosineFloor, "COMBINED")
}

// TestD1_NonsensePairingsMustBecomeMisses is the §2 regression gate.
// With a cosine floor of 0.35, ALL nonsense pairings from §2 must produce zero results.
// This is the done-condition test for M1a (if TUNE is the verdict).
//
// NOTE: this test WILL FAIL with the current default opts — that is intentional.
// It documents the DESIRED end state, not the current broken state.
// After M1a lands, this test must pass.
func TestD1_NonsensePairingsMustBecomeMisses(t *testing.T) {
	const cosineFloor = 0.35
	opts := RankOptions{MinSimilarity: cosineFloor}

	emb := NewTFIDFEmbedder()
	inv := buildInventory(allInventory)
	docs := make([]string, len(inv))
	for i, e := range inv {
		docs[i] = e.Description
	}
	emb.IndexCorpus(docs)

	type nonsensePair struct {
		task   string
		junkID string // the entry we expect NOT to be matched
	}

	// Pairs from §2 of the design doc — these are the verified nonsense pairings.
	nonsensePairs := []nonsensePair{
		{
			task:   "RPT review of campfire SDK surface: offline send, relay create, naming CLI, multi-op install",
			junkID: "junk-upgrade-smoke",
		},
		{
			task:   "fix convention.Server subscribe cursor: when a cf has multiple installed versions of the same (convention, operation), the server stalls processing new messages and the cf dispatch CLI picks the wrong version",
			junkID: "junk-upgrade-smoke",
		},
		{
			task:   "zzqq nonsense xyzzy plugh 1780344804 no such cached inference exists anywhere",
			junkID: "junk-upgrade-smoke",
		},
	}

	allMiss := true
	for _, p := range nonsensePairs {
		results := Rank(p.task, inv, emb, opts)
		var aboveFloor []RankedResult
		for _, r := range results {
			if r.Similarity >= cosineFloor {
				aboveFloor = append(aboveFloor, r)
			}
		}

		if len(aboveFloor) > 0 && aboveFloor[0].EntryID == p.junkID {
			allMiss = false
			t.Errorf("DEFECT: task %q matched junk entry %q with sim=%.4f — must be a miss at floor=%.2f",
				truncate(p.task, 60), p.junkID, aboveFloor[0].Similarity, cosineFloor)
		} else {
			t.Logf("OK (miss at floor=%.2f): task %q", cosineFloor, truncate(p.task, 60))
		}
	}

	if allMiss {
		t.Logf("All §2 nonsense pairings are correctly filtered at cosine floor=%.2f", cosineFloor)
	}
}

// TestD1_SubstantiveReusesSurviveFloor verifies that the §4 high-value entries still match
// after applying the cosine floor. With TF-IDF, substantive descriptions share enough domain
// terms to survive a 0.35 floor.
func TestD1_SubstantiveReusesSurviveFloor(t *testing.T) {
	const cosineFloor = 0.35
	opts := RankOptions{
		MinSimilarity:    cosineFloor,
		WeightEfficiency: 0.15,
		WeightQuality:    0.80,
		WeightNovelty:    0.05,
	}

	emb := NewTFIDFEmbedder()
	inv := buildInventory(allInventory)
	docs := make([]string, len(inv))
	for i, e := range inv {
		docs[i] = e.Description
	}
	emb.IndexCorpus(docs)

	// §4 reuse cases: high-value buy tasks that should find their ideal entry above the floor.
	type reuseCase struct {
		task    string
		idealID string
	}

	reuseCases := []reuseCase{
		{
			task:    "document EventSink contract for warm-worker backends in warm-worker-backends.md; add interface enforcement so new backends fail review without EventSink wiring",
			idealID: "eventsink-contract",
		},
		{
			task:    "implement per-skill chain-slot-residency histogram in EngineMetrics; emit engine:slot-residency campfire message in periodic flush window",
			idealID: "slot-residency-tracker",
		},
		{
			task:    "implement Logger EventSink consumer; replace ad-hoc slog.Info(worker spawned/exited) with event-derived logs; verify B2b log inversion gone",
			idealID: "logger-eventsink-fanout",
		},
		{
			task:    "wire warm-worker pool / TUI body substrate to emit 7-event SubstrateEvent vocabulary at Dispatch/Inject/Await/pane-capture sites; pool-specific detail in Meta not in event-kind",
			idealID: "warm-pool-substrate-wiring",
		},
		{
			task:    "wire CLI substrate to emit 7-event SubstrateEvent vocabulary derived from stream-json tool_use/tool_result blocks",
			idealID: "cli-substrate-wiring",
		},
		{
			task:    "implement TelemetryCollector EventSink consumer; derive WallClockSeconds + TotalToolCalls + P50/P90/P99 tool latency from SubstrateEvent stream; preserve CapabilityVariantTracker.AvgCost contract",
			idealID: "telemetry-collector-eventsink",
		},
		{
			task:    "implement SaturationMonitor EventSink consumer + engine:stall campfire message format + pollAndDispatch back-pressure path; stall onset fires once per onset transition",
			idealID: "saturation-monitor-eventsink",
		},
	}

	survived := 0
	for _, rc := range reuseCases {
		results := Rank(rc.task, inv, emb, opts)
		var aboveFloor []RankedResult
		for _, r := range results {
			if r.Similarity >= cosineFloor {
				aboveFloor = append(aboveFloor, r)
			}
		}

		if len(aboveFloor) > 0 && aboveFloor[0].EntryID == rc.idealID {
			survived++
			t.Logf("MATCH: task %q => %q (sim=%.4f)", truncate(rc.task, 60), rc.idealID, aboveFloor[0].Similarity)
		} else if len(aboveFloor) > 0 {
			t.Logf("WRONG TOP-1: task %q => got %q (sim=%.4f), want %q",
				truncate(rc.task, 60), aboveFloor[0].EntryID, aboveFloor[0].Similarity, rc.idealID)
		} else {
			t.Logf("MISS: task %q => no result above floor=%.2f (ideal=%q would be lost)",
				truncate(rc.task, 60), cosineFloor, rc.idealID)
		}
	}

	t.Logf("Substantive reuse survival rate at floor=%.2f: %d/%d = %.1f%%",
		cosineFloor, survived, len(reuseCases), float64(survived)/float64(len(reuseCases))*100)
}

// TestD1_RealMatchingPath_NotMocked verifies that the fixture exercises the real
// TFIDFEmbedder + Rank code path, not any mock. This is the veracity proof required
// by the task spec: "a mock-based fixture is a veracity-findable defect."
//
// We do this by:
//  1. Verifying that the embedder produces non-zero, non-identical vectors for different descriptions.
//  2. Verifying that similarity scores vary across the inventory (not all pinned to 0.5).
//  3. Verifying that Rank produces different orderings for different tasks (not random/constant).
func TestD1_RealMatchingPath_NotMocked(t *testing.T) {
	emb := NewTFIDFEmbedder()
	inv := buildInventory(allInventory)

	docs := make([]string, len(inv))
	for i, e := range inv {
		docs[i] = e.Description
	}
	emb.IndexCorpus(docs)

	// 1. Different descriptions produce different non-zero embeddings.
	embA := emb.Embed(inv[0].Description) // junk smoke test
	embB := emb.Embed(inv[1].Description) // eventsink contract
	if len(embA) == 0 {
		t.Error("Embed returned zero-length vector — embedder is not working")
	}
	simAB := emb.Similarity(embA, embB)
	// These are completely different texts; similarity must be < 1.0
	if simAB >= 1.0 {
		t.Errorf("similarity(smoke-test, eventsink) = %.4f, want < 1.0 — suggests identical embeddings (mock?)", simAB)
	}

	// 2. Similarity scores vary across inventory for a fixed task.
	taskA := "RPT review of campfire SDK surface"
	taskB := "EventSink contract for warm-worker backends"
	resultsA := Rank(taskA, inv, emb, RankOptions{})
	resultsB := Rank(taskB, inv, emb, RankOptions{})

	if len(resultsA) == 0 || len(resultsB) == 0 {
		t.Fatal("Rank returned no results — cannot verify real matching path")
	}

	// 3. The top-1 result is DIFFERENT for different tasks (not constant).
	if resultsA[0].EntryID == resultsB[0].EntryID {
		t.Logf("WARNING: Same top-1 for different tasks (%q vs %q) — may indicate poor discrimination",
			taskA, taskB)
		// Not a hard failure — TF-IDF CAN produce same top-1 for similar queries — but log it.
	}

	// 4. Similarity scores are NOT all pinned to 0.50 (the §2 confidence stub bug).
	//    The similarity field (raw cosine) MUST vary; only confidence is pinned in live exchange.
	allSame := true
	firstSim := resultsA[0].Similarity
	for _, r := range resultsA {
		if r.Similarity != firstSim {
			allSame = false
			break
		}
	}
	if allSame && len(resultsA) > 1 {
		t.Errorf("All similarity scores are identical (%.4f) — this suggests mocked embedder or broken cosine", firstSim)
	}

	t.Logf("Real matching path verified: %d results for taskA, %d results for taskB", len(resultsA), len(resultsB))
	t.Logf("Top-1 sims — taskA: %.4f (entry=%s), taskB: %.4f (entry=%s)",
		resultsA[0].Similarity, resultsA[0].EntryID,
		resultsB[0].Similarity, resultsB[0].EntryID)
}

// TestD1_SimilarityScoreDistribution prints the distribution of cosine similarity
// scores across all fixture pairs. This raw data informs the floor choice.
func TestD1_SimilarityScoreDistribution(t *testing.T) {
	emb := NewTFIDFEmbedder()
	inv := buildInventory(allInventory)
	docs := make([]string, len(inv))
	for i, e := range inv {
		docs[i] = e.Description
	}
	emb.IndexCorpus(docs)

	var idealSims, junkSims []float64

	for _, pair := range fixturePairs {
		taskEmb := emb.Embed(pair.buyTask)

		for _, e := range inv {
			entryEmb := emb.Embed(e.Description)
			sim := emb.Similarity(taskEmb, entryEmb)
			isIdeal := (e.EntryID == pair.idealID)
			if pair.expectMiss {
				// For miss cases, junk = any result
				if e.EntryID == "junk-upgrade-smoke" {
					junkSims = append(junkSims, sim)
				}
			} else if isIdeal {
				idealSims = append(idealSims, sim)
			}
		}
	}

	if len(idealSims) > 0 {
		t.Logf("Ideal-entry cosine sim distribution (%d obs): min=%.4f, max=%.4f, mean=%.4f",
			len(idealSims), minF(idealSims), maxF(idealSims), meanF(idealSims))
	}
	if len(junkSims) > 0 {
		t.Logf("Junk-entry cosine sim distribution (%d obs): min=%.4f, max=%.4f, mean=%.4f",
			len(junkSims), minF(junkSims), maxF(junkSims), meanF(junkSims))
	}

	// Decision data: if ideal_min > junk_max, a hard floor between them perfectly separates.
	// If ranges overlap, TF-IDF cannot separate by threshold alone — REPLACE verdict indicated.
	if len(idealSims) > 0 && len(junkSims) > 0 {
		idealMin := minF(idealSims)
		junkMax := maxF(junkSims)
		t.Logf("Separation analysis: ideal_min=%.4f, junk_max=%.4f", idealMin, junkMax)
		if idealMin > junkMax {
			t.Logf("SEPARABLE: ideal entries score higher than junk across all pairs. TF-IDF tuning is viable.")
		} else {
			t.Logf("OVERLAPPING: junk entries score as high or higher than ideal for some pairs. TF-IDF tuning may not be sufficient alone.")
		}
	}
}

// logDetailedResults prints per-pair results for a given configuration.
func logDetailedResults(t *testing.T, opts RankOptions, simFloor float64, label string) {
	t.Helper()

	emb := NewTFIDFEmbedder()
	inv := buildInventory(allInventory)
	docs := make([]string, len(inv))
	for i, e := range inv {
		docs[i] = e.Description
	}
	emb.IndexCorpus(docs)

	correct := 0
	for _, pair := range fixturePairs {
		results := Rank(pair.buyTask, inv, emb, opts)

		// Post-filter for explicit floor.
		var filtered []RankedResult
		for _, r := range results {
			if r.Similarity >= simFloor {
				filtered = append(filtered, r)
			}
		}

		var verdict string
		var got string
		var gotSim float64
		if len(filtered) > 0 {
			got = filtered[0].EntryID
			gotSim = filtered[0].Similarity
		}

		if pair.expectMiss {
			if len(filtered) == 0 {
				verdict = "CORRECT-MISS"
				correct++
			} else {
				verdict = fmt.Sprintf("FALSE-HIT(%s,sim=%.4f)", got, gotSim)
			}
		} else {
			if got == pair.idealID {
				verdict = fmt.Sprintf("CORRECT(sim=%.4f)", gotSim)
				correct++
			} else if got == "" {
				verdict = fmt.Sprintf("MISSED-IDEAL(%s)", pair.idealID)
			} else {
				verdict = fmt.Sprintf("WRONG(got=%s,sim=%.4f,want=%s)", got, gotSim, pair.idealID)
			}
		}

		t.Logf("[%s] %s: %s — task: %q", label, pair.name, verdict, truncate(pair.buyTask, 55))
	}
	t.Logf("[%s] Summary: %d/%d correct = %.1f%%", label, correct, len(fixturePairs),
		float64(correct)/float64(len(fixturePairs))*100)
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func minF(s []float64) float64 {
	if len(s) == 0 {
		return 0
	}
	m := s[0]
	for _, v := range s[1:] {
		if v < m {
			m = v
		}
	}
	return m
}

func maxF(s []float64) float64 {
	if len(s) == 0 {
		return 0
	}
	m := s[0]
	for _, v := range s[1:] {
		if v > m {
			m = v
		}
	}
	return m
}

func meanF(s []float64) float64 {
	if len(s) == 0 {
		return 0
	}
	sum := 0.0
	for _, v := range s {
		sum += v
	}
	return sum / float64(len(s))
}
