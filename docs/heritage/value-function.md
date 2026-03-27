# Design: Value Function for DontGuess's Three Improvement Loops

> Produced by architect synthesis of adversarial design deliberation (4 agents, 3 rounds). Source: `/tmp/toolrank-design-deliberation.txt`.

## 1. The Value Function

### Why not one number

A single scalar optimization target invites Goodhart's Law: the loops optimize the metric instead of the product. The current implementation uses `golden_rate` (percentage of hand-curated golden queries returning expected tools) as the sole judge for both the medium loop (`improve.py:246`) and the slow loop (`improve.py:469`). This is the wrong primary metric for three reasons:

1. **Self-referential evaluation.** The slow loop expands the golden query set from coverage gaps (`_expand_golden_from_gaps`), then the medium loop validates changes against that same set. The system validates its own priors.
2. **Binary, not continuous.** Golden pass rate is pass/fail per query. A change that moves a tool from position 8 to position 2 scores the same as no change at all, as long as the tool is still in the result set.
3. **Silence is ambiguous.** A high golden rate says "we return expected tools." It says nothing about whether those tools actually helped the agent. Confidently wrong results pass golden checks.

### The 4-layer value stack

Each loop owns exactly one layer. No loop directly optimizes another loop's metric. Layer 0 gates all others.

```
Layer 0  CORRECTNESS GATE    cluster_return_rate       Validation (no loop owns this)
Layer 1  RESOLUTION DEPTH    mean_session_queries      Fast loop target
Layer 2  QUALITY COMPOSITE   V_medium                  Medium loop gate
Layer 3  NOVELTY             position_surprise         Slow loop target
Layer 4  META                oscillation_frequency     Adapts slow loop step size
```

**Layer 0 — Correctness Gate (Persistence proxy).** Metric: `cluster_return_rate` = fraction of query clusters where the same `model_id` returns to the same cluster within 24 hours. A rising return rate means agents are not finding what they need on the first visit. Any change that increases `cluster_return_rate` by >2% is rejected regardless of what other metrics say. This prevents the "confidently wrong" failure mode: a system that returns plausible-looking wrong answers will see agents come back and retry.

Data source: existing `search_telemetry` table, grouped by `model_id` + `_query_cluster_key()` within a rolling 24-hour window. No schema change required.

**Layer 1 — Resolution Depth (Fast loop target).** Metric: `mean_session_queries` = average number of queries per `session_id` over new telemetry rows. Lower is better — an agent that finds what it needs in one query is better served than one that reformulates three times. The fast loop's affinity boosts directly reduce reformulation need; this metric measures whether they work.

Replaces the current approach where the fast loop has no explicit optimization target (it writes boosts but never measures their effect). Data source: existing `search_telemetry`, `GROUP BY session_id`. No schema change required.

**Layer 2 — Quality Composite (Medium loop gate).** Metric: `V_medium = 0.7 * golden_rate_tier1 + 0.3 * (1 - retry_rate)`. Replaces the current raw `golden_rate` comparison at `improve.py:246`. The golden query set is split into two tiers:

- **Tier 1 (frozen):** Hand-curated golden queries. Never modified by any loop. These are invariant regression tests.
- **Tier 2 (dynamic):** Auto-expanded golden queries from `_expand_golden_from_gaps()`. Used for diagnostics, not gating.

Only tier 1 golden rate feeds the gate. `retry_rate` comes from `is_retry` in the telemetry (already captured but never read by any loop — this is the single biggest waste in the current system).

Edge case: if `COUNT(*)` < 10 in the telemetry window, `retry_rate` is undefined. Fall back to `golden_rate_tier1` alone.

**Layer 3 — Novelty (Slow loop target).** Metric: `position_surprise` = mean difference between a tool's popularity rank (by `graph_score` descending) and its result position in DontGuess output. High position surprise means DontGuess is surfacing tools that a naive popularity ranking would bury — this is the core value proposition. The slow loop's weight experiments should increase position surprise without regressing layers 0-2.

Data source: `search_telemetry.result_ids` joined against the tool DB's `graph_score`. Adds a DB read to the slow loop (acceptable at 4-hour cadence).

Note: the adversary's attack on "counterfactual value" (that it penalizes popularity) is addressed by measuring rank delta, not absolute popularity. A popular tool that DontGuess correctly ranks highly gets a position surprise near zero, not a penalty.

**Layer 4 — Meta (Oscillation detection).** Metric: lag-1 autocorrelation of the `kept` boolean series from the last 20 entries in `data/experiments.jsonl`. If `autocorr < -0.3`, the slow loop is fitting noise (accepting/rejecting changes in alternating fashion). Action: halve `WEIGHT_STEP` (floor 0.005). If `autocorr > 0.3` and last 5 experiments were all kept: double `WEIGHT_STEP` (ceiling 0.05). This prevents the overfitting attack the adversary identified on weight experiments.

Additional guard: if `WEIGHT_STEP` adaptation causes no experiments to be generated (step exceeds distance to weight bounds), reset step to default (0.025).

### How layers compose

The layers do NOT compose into a single scalar. They form a gating hierarchy:

1. Compute Layer 0 (`cluster_return_rate`). If a proposed change increases it by >2%, reject immediately.
2. Compute the loop-specific metric (Layer 1, 2, or 3 depending on which loop is evaluating).
3. Accept only if the loop-specific metric improves AND Layer 0 does not regress.

This is strictly more conservative than the current greedy "keep if `result_rate > best_rate`" approach. That conservatism is intentional — fewer accepted changes means higher bar, which counters the overfitting risk from fast iteration on small evaluation sets.

### Adversary attacks addressed

| Attack | Resolution |
|--------|-----------|
| Golden expansion is circular (system validates own priors) | Tier 1/tier 2 split. Only frozen tier 1 gates decisions. |
| Silence is ambiguous without correctness | Layer 0 persistence gate catches "confidently wrong" |
| One number invites Goodhart's Law | 4-layer stack, no single optimization target |
| Faster iteration without accurate measurement destroys quality | Oscillation detection (Layer 4) pauses experiments when fitting noise |
| `tokens-per-resolution` without correctness optimizes for confident wrongness | Persistence gate catches this; tokens-per-resolution deferred until `toolrank_report_usage` has real callers |

## 2. What to Measure

### What exists today

The `search_telemetry` table (`src/toolrank/telemetry.py:204-217`) captures:

| Column | Type | Used by loops? |
|--------|------|---------------|
| `timestamp` | TEXT | Yes (fast loop watermark) |
| `session_id` | TEXT | Yes (fast loop grouping) |
| `query` | TEXT | Yes (fast loop clustering) |
| `actor_type` | TEXT | **No** — captured but never read by any loop |
| `intent` | TEXT | **No** — captured but never read |
| `domain` | TEXT | **No** — captured but never read |
| `result_count` | INTEGER | Yes (coverage gap detection) |
| `result_ids` | TEXT (JSON) | Yes (fast loop signal detection) |
| `latency_ms` | REAL | **No** — captured but never read |
| `is_retry` | INTEGER | **No** — captured but never read by any loop |
| `model_id` | TEXT | **No** — captured but never read |

Five of eleven columns are captured but unused. `is_retry` is the most egregious waste: it is exactly the retry rate signal the medium loop needs, already computed and stored, but never queried.

### What needs to be added

**Schema changes: one column.**

```sql
ALTER TABLE search_telemetry ADD COLUMN score_distribution TEXT DEFAULT '[]';
```

This stores the top-10 composite scores as a JSON array. Enables:
- Entropy computation (score concentration in result sets)
- The escape velocity metric (behavioral score vs heuristic score gap)

Implementation: `TelemetryCapture._write_entry()` receives a score list from the caller. `mcp_server.py` `dispatch_tool()` passes `[r.get('_search_score', 0) for r in raw_results[:10]]` to `record_search()`. Backward-compatible (new column has a default, old rows get `'[]'`).

**JSONL changes: one field.**

Add `"tier": 1` to hand-curated entries in `data/golden-queries.jsonl`. Auto-expanded entries get `"tier": 2` (modify `_expand_golden_from_gaps()` to write `tier: 2`). `measure_golden()` filters to `tier == 1` for gating decisions. No JSONL schema enforcement needed — field is simply present or absent.

**New computations (no schema change):**

| Computation | Data source | Lines | Location |
|------------|-------------|-------|----------|
| `cluster_return_rate` | search_telemetry GROUP BY model_id + cluster | ~25 | `fast_loop.py:run_once()` |
| `mean_session_queries` | search_telemetry GROUP BY session_id | ~10 | `fast_loop.py:run_once()` |
| `retry_rate` | search_telemetry AVG(is_retry) | ~5 | `improve.py:measure_telemetry()` |
| `zero_result_rate` | search_telemetry COUNT WHERE result_count=0 | ~5 | `improve.py:measure_telemetry()` |
| `latency_p95` | search_telemetry ORDER BY latency_ms OFFSET | ~20 | `improve.py:measure_telemetry()` |
| `oscillation_autocorr` | experiments.jsonl 'kept' series | ~15 | `improve.py:run_slow_loop()` |
| `position_surprise` | search_telemetry + DB graph_score | ~40 | `improve.py:run_slow_loop()` |
| `escape_velocity_gap` | score_distribution + affinity_boost | ~30 | `fast_loop.py:run_once()` |

### Implementation cost estimates

From the systems pragmatist's analysis:

- **Phase 1 (no schema changes):** ~125 lines across `improve.py` and `fast_loop.py`. 2-3 days. All data already exists.
- **Phase 2 (one schema column):** ~80 lines across `telemetry.py` and `mcp_server.py`. 1 day. Backward-compatible ALTER TABLE.
- **Phase 3 (DB join in fast loop):** ~60 lines in `fast_loop.py`. 1 day. Adds read-only DB access.
- **Overnight fix:** ~5 lines in `bin/overnight`. 1 hour. Highest leverage single change.

**Grand total:** ~270 lines of Python + 5 lines of bash. Touches 4 files. All reversible.

## 3. How to Interpret Measurements

### When to keep vs revert a change

The current gate (`improve.py:246`, `improve.py:469`) is: keep if `after_rate >= baseline_rate` (medium) or `result_rate > best_rate` (slow). Replace with:

**Medium loop gate:**
```
KEEP if:
  cluster_return_rate(after) <= cluster_return_rate(before) + 0.02
  AND V_medium(after) >= V_medium(before)
  WHERE V_medium = 0.7 * golden_rate_tier1 + 0.3 * (1 - retry_rate)

REVERT otherwise.
```

**Slow loop gate:**
```
KEEP if:
  cluster_return_rate(after) <= cluster_return_rate(before) + 0.02
  AND golden_rate_tier1(after) >= golden_rate_tier1(before)  [held-out validation]
  AND golden_rate_tier1(after, training) >= golden_rate_tier1(before, training)

REVERT otherwise.
```

### Held-out evaluation set

The adversary's demand: a frozen partition the loops cannot modify or train against. Implementation: split `golden-queries.jsonl` tier 1 entries by `hash(query) % 5`. Entries where `hash % 5 != 0` are the training set (used for experiment evaluation). Entries where `hash % 5 == 0` are the validation set (checked after the best training-set experiment is selected). A change must improve on both sets to be accepted.

This prevents the slow loop from overfitting to a small golden set by effectively requiring generalization.

### Oscillation detection

The creative agent's proposal, confirmed implementable by the systems pragmatist:

```python
def detect_oscillation(experiments_file: Path, window: int = 20) -> float:
    """Compute lag-1 autocorrelation of kept series. Returns [-1, 1]."""
    kept_series = []
    for line in experiments_file.read_text().splitlines()[-window:]:
        entry = json.loads(line)
        kept_series.append(1.0 if entry.get("kept") else -1.0)
    if len(kept_series) < 5:
        return 0.0  # insufficient data
    mean = sum(kept_series) / len(kept_series)
    var = sum((x - mean) ** 2 for x in kept_series) / len(kept_series)
    if var < 1e-10:
        return 0.0
    covar = sum((kept_series[i] - mean) * (kept_series[i+1] - mean)
                for i in range(len(kept_series) - 1)) / (len(kept_series) - 1)
    return covar / var
```

Interpretation:
- `autocorr < -0.3`: oscillating (accept/reject/accept/reject). Halve `WEIGHT_STEP`.
- `autocorr > 0.3` and last 5 all kept: converging. Double `WEIGHT_STEP`.
- Otherwise: hold steady.

### Escape velocity metric

Track two scores per query-tool pair:
- **Heuristic score:** the composite score from the search index (BM25 + graph + text signals)
- **Behavioral score:** the affinity boost accumulated from telemetry signals

`escape_velocity_gap = mean(behavioral_score - heuristic_score)` across all queries with both scores.

When `positive_gap_fraction > 0.5` for 7+ consecutive fast loop runs, declare "escape velocity detected" — the behavioral data is outperforming the heuristic priors. At this point, the heuristic weights become less important and behavioral data should be weighted more heavily. This directly measures the product vision's core thesis: that cross-model behavioral data eventually supersedes editorial heuristics.

## 4. Speed Optimization

### The meta-layer: measuring loop speed itself

Each loop tick already logs to `improve_history.jsonl` with a timestamp. The meta-measurement:

- **Fast loop effective cadence:** actual time between `run_once()` calls (currently targets 15 minutes).
- **Medium loop throughput:** fixes attempted and applied per hour.
- **Slow loop experiment velocity:** experiments evaluated per 4-hour window.

These are not optimization targets (optimizing for speed of iteration without accuracy destroys quality). They are diagnostic signals: if the fast loop hasn't run in 2 hours, something is stuck. If the slow loop runs 0 experiments for 3 consecutive windows, the weight bounds are exhausted.

### Minimum viable cycle time per loop

| Loop | Current cadence | Minimum viable | Bottleneck |
|------|----------------|----------------|------------|
| Fast | 15 min | ~2 min (limited by telemetry DB read) | Acceptable |
| Medium | 1 hr | ~10 min (limited by golden query evaluation) | `measure_golden()` runs all queries against Whoosh index |
| Slow | 4 hr | ~30 min (limited by multiple golden evaluations) | Each weight experiment requires a full `measure_golden()` call |

### When speed hurts

The oscillation detection (Layer 4) is specifically designed to catch the failure mode where too-fast iteration on a small evaluation set leads to overfitting. The mechanism: if experiments are alternating accept/reject, the step size is halved, which effectively slows the loop's rate of change. Speed of iteration is gated by signal quality.

Additional constraint: the held-out validation set prevents the slow loop from learning the training partition. Even at high iteration speed, generalization is enforced.

## 5. Implementation Roadmap

### Phase 1: Zero schema changes, ~125 lines (fix what's broken)

**Files:** `improve.py`, `fast_loop.py`

1. **Add `measure_telemetry()` to `improve.py`** (~50 lines). SQL query against `search_telemetry` for `AVG(is_retry)`, zero-result rate, latency P95 (via Python sort since SQLite lacks native percentile), and `mean_session_queries`. This is the function that reads the five unused telemetry columns.

2. **Add tier field to golden queries** (~15 lines in `improve.py` + JSONL edits). Hand-curated entries in `data/golden-queries.jsonl` get `"tier": 1`. `_expand_golden_from_gaps()` writes `"tier": 2`. `measure_golden()` accepts a `tier` filter parameter.

3. **Replace medium loop gate** (~15 lines in `improve.py:246`). Change from `after["rate"] < baseline["rate"]` to `V_medium(after) < V_medium(baseline)` where `V_medium = 0.7 * golden_rate_tier1 + 0.3 * (1 - retry_rate)`.

4. **Add held-out validation to slow loop** (~15 lines in `improve.py:run_slow_loop()`). Split golden queries by `hash(query) % 5`. Accept weight change only if both training AND validation sets improve.

5. **Add oscillation detection** (~20 lines before `_generate_weight_experiments()`). Load last 20 `kept` booleans from `experiments.jsonl`. Compute lag-1 autocorrelation. Adapt `WEIGHT_STEP` accordingly.

6. **Add `cluster_return_rate` to fast loop** (~25 lines in `fast_loop.py:run_once()`). Group telemetry rows by `model_id` + cluster key within 24-hour window. Write to `fast_loop_history.jsonl` as a monitoring signal.

### Phase 2: One schema column, ~80 lines (add missing measurements)

**Files:** `telemetry.py`, `mcp_server.py`, `fast_loop.py`

7. **Add `score_distribution` column** (~20 lines). ALTER TABLE, update `_write_entry()` to accept and store score list, update `mcp_server.py` to pass scores through.

8. **Add escape velocity tracking** (~30 lines in `fast_loop.py:run_once()`). For each new telemetry row, compare affinity boost to heuristic composite. Track `mean_gap` and `positive_gap_fraction` in `fast_loop_history.jsonl`.

9. **Add `mean_session_queries` tracking** (~10 lines). Already computed in Phase 1 step 1, wire into fast loop history output.

### Phase 3: Full value vector, ~60 lines (position surprise)

**Files:** `fast_loop.py`

10. **Add `position_surprise` computation** (~40 lines). Open tool DB read-only in `run_once()`. For each telemetry row's `result_ids`, fetch `graph_score`. Compute popularity rank bucket. `position_surprise = mean(popularity_rank_bucket - result_position)` for top-5 results.

11. **Wire Layer 3 into slow loop gate** (~20 lines). After weight experiment, compute `position_surprise` delta. Accept only if position surprise is non-negative AND Layers 0-2 pass.

### The 5-line fix in `bin/overnight`

The overnight loop writes `golden_rate: 0` hardcoded for all cycle entries (`bin/overnight:84-101`). This means the improve daemon has no idea the baseline shifted after overnight code changes. Fix:

After each `run_step()` call, run `bin/search-quality` and write the real golden rate to `improve_history.jsonl`:

```bash
# After run_step's python3 -c block (line 101), add:
REAL_RATE=$(PYTHONPATH=src python3 -c "
from toolrank.quality import run_golden_queries
g = run_golden_queries('data/golden-queries.jsonl', 'data/whoosh-index', 'data/toolrank.db')
print(g['passed'] / g['total'] * 100 if g['total'] > 0 else 0)
" 2>/dev/null || echo "0")
# Then use $REAL_RATE instead of hardcoded 0 in the entry
```

This is the single highest-leverage change in this design. It closes the feedback loop between LLM-driven overnight work and the automated measurement system.

## 6. RPT Compliance

### Assessment: 75% compliant

The RPT domain purist's assessment from deliberation.

### Directly observable RPT signals

| RPT Signal | Observable? | How |
|-----------|-------------|-----|
| Query-to-silence (branching factor collapse) | **Direct** | Telemetry session analysis, already implemented in fast loop |
| Reformulation depth (failed collapse) | **Direct** | `detect_reformulation_chains()` in fast loop |
| Cross-agent convergence | **Direct** | `detect_cross_session_convergence()` in fast loop |
| Return rate | **Partial** | Observable for stdio sessions; HTTP sessions fragmented by per-request session_id |

### Proxied RPT signals

| RPT Signal | Proxy Used | Gap |
|-----------|-----------|-----|
| Token count (tokens-per-resolution) | `mean_session_queries` as proxy | No direct token measurement without `toolrank_report_usage` callers |
| Hedging frequency | Not measured | Permanently outside observational boundary (requires seeing agent output text) |
| Error recovery cost | `cluster_return_rate` as proxy | Actual recovery cost requires downstream outcome visibility |

### What closes the gap

1. **`toolrank_report_usage` adoption.** The endpoint exists in the product vision (`product-vision.md:531-543`) but no agent framework calls it today. When real clients adopt it, three proxy signals become direct measurements: token count (from `tokens_spent`), outcome (from `outcome` field), and error recovery cost (from failure-to-retry patterns). This is the single most important step for RPT compliance.

2. **Actor-type-aware value function.** The telemetry captures `actor_type` (agent/human/unknown) but the value function ignores it. RPT says the Human-App edge and AI-App edge require different signal weights. Agent queries should weight reformulation depth heavily (agents reformulate when results are inadequate). Human queries should weight latency and result diversity (humans browse differently). Phase 2 should differentiate value function weights by `actor_type`.

3. **Operator weight surface.** RPT requires that the weights on value function components be adjustable by the operator over time, reflecting strategic phase (engagement vs token efficiency vs revenue). The current design hardcodes `V_medium = 0.7 * golden + 0.3 * (1 - retry)`. These weights should be in a config file, not source code. The slow loop should be able to experiment with the weights themselves (meta-optimization), but only with explicit operator approval.

### What remains surface-level

- The word "resonance" is used freely but no component computes an actual amplification ratio (output/input > 1 per RPT section 2). The escape velocity gap comes closest.
- Loop cadences are hardcoded (15min/1hr/4hr). RPT says cadences should emerge from data availability — the fast loop should run when there is new telemetry, not on a timer. Practical for now, but not principled.

## 7. Known Gaps

### Permanent constraints (cannot be resolved within DontGuess's architecture)

**The correctness problem.** DontGuess cannot observe whether the agent succeeded after receiving search results. The observational boundary ends at result delivery. "Did the agent install the tool and use it correctly?" is unknowable without explicit reporting from the agent framework. `toolrank_report_usage` is the designed path to partial observability, but it requires adoption by agent frameworks outside DontGuess's control.

Mitigation: persistence (cluster return rate) acts as a weak correctness proxy. An agent that keeps coming back to the same query cluster probably did not succeed. This catches the worst failures but cannot distinguish "succeeded" from "gave up and tried something else."

**The observational boundary.** DontGuess sees: query, results returned, session patterns, timing. DontGuess does not see: what the agent did with the results, how many tokens the agent spent after receiving results, whether the human was satisfied, whether the tool was actually installed, whether the tool worked. Every metric in this design operates within that boundary.

**Hedging frequency.** Detecting whether an agent hedged its recommendation ("this might work, but you could also try...") requires seeing the agent's output text. This is permanently outside DontGuess's observational boundary.

### Future work (resolvable but not in scope)

**Response format optimization.** The adversary correctly identified that the response format affects total token cost. A result that returns 200 tokens of metadata when the agent only needs the install command wastes tokens. This is a real optimization target but is separate from the value function — it's a response compression problem, not a measurement problem. File as a separate workstream.

**HTTP session stitching.** The MCP server creates a new `session_id` per HTTP POST request (`mcp_server.py:488`). Cross-session signals (cluster return rate, convergence) are unreliable for HTTP transport. Workaround: use `model_id` as a weak session proxy (same agent framework across requests will have the same client name). Proper fix: implement session continuity in the HTTP transport (session token in response, sent back in subsequent requests).

**K3 triangle completeness.** The value function measures the AI-App edge only. The Human-App edge (humans using the web dashboard) has no representation. The Human-AI edge (how the human interacts with the agent that uses DontGuess) is structurally unobservable. The dashboard exists (`web/dashboard.html`) but feeds no loop.

**Self-referential value function.** The creative agent proposed loops that optimize which metric to optimize (the value function selects itself). Elegant but premature — requires enough data to compare value functions, which requires the loops to be running first. Deferred until the first value function has been running for 2+ weeks. The pluggable callable architecture should be preserved so the value function CAN be swapped.

**Entropy as quality signal.** Score concentration in result sets is a real signal (tight score clustering means confident ranking), but concentrated-but-wrong is worse than diverse-but-correct. Use entropy as a component of diagnostics, not as a gating metric. Gate with persistence: high confidence is only good if recommendations stick.

### Unresolved adversary attacks

| Attack | Status | Why unresolved |
|--------|--------|---------------|
| "Resolution Depth degrades to retry_rate" | **Partially addressed.** `reformulation_count` and `retry_rate` use different similarity thresholds (0.5 vs 0.4) and different time windows (60s vs unbounded). They are distinct signals but correlated. Monitor divergence over time. |
| "No ground truth for agent success" | **Permanent constraint.** Cannot be resolved without protocol-level changes to MCP or voluntary reporting from agent frameworks. Persistence is the best available proxy. |
| "Position surprise rewards obscurity" | **Addressed by design.** Measuring rank delta (not absolute popularity) means popular tools that DontGuess correctly ranks highly get position surprise near zero, not a penalty. An obscure tool surfaced at rank 1 gets high surprise only if it is genuinely relevant (validated by Layer 0 persistence gate). |

---

## 8. Autonomous Golden Query Generation

> Extension produced by architect synthesis of adversarial design deliberation, Round 2 (4 agents, 3 rounds, 539 lines). Source: `/tmp/toolrank-design-deliberation-r2.txt`. Sections 8-12 extend the value function design with autonomous golden query generation, cross-model usage, escape velocity dashboard, launch gates, and implementation roadmap.

### The problem with hand-curation

The golden query suite (`data/golden-queries.jsonl`) currently contains ~10 hand-curated queries. At 10 queries, the tier 1 regression set is too small to catch regressions with statistical confidence. Manual expansion is a bottleneck — Baron reviewing 20 queries/week reaches 200 tier-1 queries in 10 weeks. The system needs an autonomous pipeline that generates candidates at scale while preserving human oversight for the final promotion gate.

### Three-source scenario generator

Golden query candidates come from three structurally different sources. Each source compensates for the others' blind spots.

**Source 1: Adversarial Opus (~30 candidates/night).** Opus reads `data/coverage_gaps.jsonl` (already produced by the fast loop) and generates queries designed to *break* DontGuess — queries targeting domains where DontGuess currently returns zero or poor results. The adversarial framing is load-bearing: Opus is not asked "what should DontGuess be good at?" (which encodes judgment) but "where does DontGuess currently fail?" (which encodes observation). Each call provides ~8K tokens of context (gap descriptions, domain spec, existing query set to avoid duplication) and produces 5-10 candidate queries with expected answers.

Cost: ~$0.20/call, 5-10 candidates/call, target 50 candidates/day = ~$1.50/day.

**Source 2: Dependency graph mining (~20 candidates/night).** Parse real `Cargo.toml`, `package.json`, `requirements.txt`, and `go.mod` files from the existing 18.6M-edge dependency graph. For each dependency, generate the scenario: "I am building [project type inferred from manifest]. I need [dependency's function]. What should I use?" The expected answer is the actual dependency. Ground truth is *observed* (what real projects actually use), not *authored* (what an LLM thinks is correct). RPT-compliant per Section 1.6: capturing real composition patterns.

Cost: zero API cost — purely computational against existing data.

**Source 3: GitHub trending freshness probes (~10 candidates/week).** Clone trending repositories (shallow, HEAD only), parse dependency manifests, identify dependencies first published in the last 30 days. Generate scenarios where the expected answer is the new dependency. These are diagnostic, not training — they measure DontGuess's freshness delta. If >20% of trending-repo scenarios produce zero DontGuess results, trigger a crawl alert.

Cost: ~$0.50/day for Opus analysis of 75 scenarios. GitHub API calls are free within rate limits for public repos.

### Tier architecture

```
                              ┌──────────────────────┐
 Source 1 (Adversarial Opus)──┤                      │
 Source 2 (Dep graph mining)──┤  golden-candidates   │── Human review (weekly batch)──┐
 Source 3 (GitHub trending) ──┤  .jsonl (staging)    │                                │
                              └──────────────────────┘                                │
                                                                                      ▼
                                        ┌─────────────────────────────────────────────────┐
                                        │  golden-queries.jsonl                           │
                                        │  tier=2  (auto-generated, used for diagnostics) │
                                        │  tier=1  (human-frozen, gates medium loop)      │
                                        └─────────────────────────────────────────────────┘
```

**Schema extension:** Each entry in `golden-queries.jsonl` and `golden-candidates.jsonl` carries:

```json
{
  "query": "...",
  "expect": ["tool-id-1", "tool-id-2"],
  "tier": 1,
  "source": "adversarial-opus | dep-graph | github-trending | human",
  "validated_by": "human | execution | cross-model | none",
  "domain": "embedded | ml | scientific | security | game-dev | mobile | web | devops | data",
  "created": "2026-03-18T00:00:00Z"
}
```

**Promotion pipeline:**

1. Source generates candidate → writes to `golden-candidates.jsonl` with `tier=2`, `validated_by=none`.
2. Negative validation filter: only candidates where DontGuess currently *fails* (expected tools not in top-5 results) pass through. Candidates where DontGuess already succeeds go to a regression tracking file, not to the candidate queue. This inverts the echo chamber — the system generates tests that target weaknesses, not confirmations.
3. Execution validation (optional, for high-break-score scenarios): an agent installs the expected tool and verifies it works for the described task. Upgrades `validated_by` to `execution`.
4. Cross-model validation (optional): 3+ models independently search DontGuess for the same scenario and converge on the expected tool. Upgrades `validated_by` to `cross-model`.
5. Human review gate: Baron reviews ~20 candidates/week interactively via `bin/review-golden`. Accepted candidates get promoted to `tier=1` in `golden-queries.jsonl`. 15 minutes/week.

Only `tier=1` queries gate the medium loop (Section 1, Layer 2). `tier=2` queries feed diagnostics, coverage analysis, and break-score tracking. The medium loop in `improve.py` filters on `tier == 1` for all gating decisions.

### Domain distribution quotas

The golden query suite must cover the breadth of the tool ecosystem. Structural enforcement, not prompting:

```python
MIN_DOMAIN_FRACTION = 0.05   # No domain below 5% of suite
MAX_DOMAIN_FRACTION = 0.30   # No domain above 30% of suite
REQUIRED_DOMAINS = [
    'embedded', 'ml', 'scientific', 'security',
    'game-dev', 'mobile', 'web', 'devops', 'data'
]
```

The generation pipeline checks domain distribution before writing candidates. If any domain exceeds `MAX_DOMAIN_FRACTION` or any required domain is below `MIN_DOMAIN_FRACTION`, generation is blocked for over-represented domains and prioritized for under-represented ones. This is enforced in code (~30 lines), not by prompting Opus to "diversify."

The existing bootstrap-runner's 39 synthetic tasks have zero coverage of embedded, scientific, game-dev, audio, or hardware design. The domain quota mechanism directly addresses this gap.

### Echo chamber mitigation: negative validation

The adversary's Attack 1 (Round 1) identified circularity: Opus generates queries, DontGuess evaluates them, Opus judges the evaluation. The negative validation filter breaks this circle:

1. Opus generates candidate queries targeting known coverage gaps.
2. DontGuess evaluates each candidate immediately.
3. **Only candidates where DontGuess fails are accepted.** A query where DontGuess already returns the expected tool is useless for improvement — it confirms existing capability, not a gap.
4. For accepted candidates, the *expected answers* remain Opus-authored. This is the residual circularity the adversary correctly identified. Mitigation: human expert review for tier-1 promotion. 20 expert-reviewed queries/week is more valuable for correctness than 2,000 Opus-generated queries/night.

The RPT purist's assessment (60% compliance) correctly identifies that autonomous generation is the Bitter Lesson in reverse — a more sophisticated hand-coded chess engine. Accepted. The design explicitly positions golden queries as a *regression safety net*, not a *resonance measurement*. Golden queries test retrieval accuracy (does the right tool appear in results?), not interaction resonance (does the agent's branching factor collapse?). The A/B test (Section 11) measures resonance. Golden queries measure capability.

### Integration with overnight loop

The overnight loop (`bin/overnight`) triggers golden generation as a pipeline stage:

```
overnight: ... → generate-golden → tournament → assess → ...
```

`bin/generate-golden` reads `data/coverage_gaps.jsonl`, calls Opus for adversarial candidates, applies negative validation filter, writes to `golden-candidates.jsonl`. The tournament harness picks up new candidates for cross-model validation.

Cadence: 50 new candidates/night. At a 30% negative-validation pass rate (70% of candidates hit queries DontGuess already handles), ~15 validated candidates/night enter the staging area. Weekly human review promotes ~10-15 to tier 1. Timeline to 200 tier-1 queries: ~15 weeks from pipeline start (accounting for the existing ~10 tier-1 queries).

### Cost

| Component | Daily cost | Monthly cost |
|-----------|-----------|-------------|
| Adversarial Opus generation (50 candidates) | $1.50 | $45 |
| GitHub trending analysis (75 scenarios/week) | $0.07 | $2 |
| Dependency graph mining | $0 | $0 |
| **Total golden generation** | **~$1.57** | **~$47** |

## 9. Cross-Model Usage (Production Usage by AI Agents)

### The operator correction

> "The models ARE the users. This is a search engine FOR AI. When Sonnet searches DontGuess and finds the right tool in 1 query instead of 5, that IS a real user having a real experience. Cross-model simulation is not synthetic — it is production usage by the actual target audience."

This reframes the entire section. The deliberation's language of "simulation" and "synthetic data" was wrong. When Llama searches DontGuess, that is a real search by a real user — the AI agent. The data it produces is production usage data, not synthetic approximation. The distinction between "organic" and "synthetic" collapses: an AI agent searching DontGuess to solve a task *is* an organic user, regardless of whether a human initiated the task or an overnight harness did.

### Reconciling with the RPT purist's topology argument

The purist argues: "One organic data point from a GPT-4 user genuinely using DontGuess to solve a real problem changes the topology. 25,200 synthetic points do not." This argument has force — but it rests on a premise that the operator correction challenges.

The purist's distinction is between:
- **K3 data** (human + agent + product): A human directs an agent, the agent uses DontGuess, the human evaluates the outcome. Full three-node graph.
- **K2 data** (agent + product only): An overnight harness directs an agent, the agent uses DontGuess, no human evaluates the outcome. Two-node graph.

The operator says: *for this product*, K2 is sufficient for the AI-App edge because the AI agent is the primary user. The Human-AI and Human-App edges matter for distribution and trust, but the core product question — "does DontGuess help AI agents find better tools faster?" — is answered entirely at the AI-App edge.

**Resolution: Both are right, about different things.**

- The **AI-App edge** (does DontGuess improve agent outcomes?) is measurable from agent-only sessions. Cross-model agent sessions produce genuine AI-App edge data. This is production usage data, not synthetic.
- The **data topology** (cross-model convergence as moat) requires *independently motivated* agents from *different model families*. An overnight harness running Llama on curated tasks produces behavioral data from a genuinely different model (different training data, different biases, different tool familiarity). But the tasks are not independently motivated — they come from the same harness. The topology changes when a *different operator's* Llama agent searches DontGuess for *their own reasons*.
- **Labeling discipline:** Agent sessions driven by the overnight harness are labeled `source=harness` in telemetry. Agent sessions from development work (Baron's Claude Code) are labeled `source=organic`. Agent sessions from external users (post-launch) are labeled `source=external`. The escape velocity metric (Section 3) is computed on `source=organic` and `source=external` data only. Harness data feeds coverage analysis, behavioral measurement, and break-score tracking.

The purist is right that harness-driven cross-model data does not constitute the moat. The operator is right that it constitutes real usage data for the AI-App edge. Both claims are honored by labeling and routing the data correctly.

### Local GPU inference architecture

**Hardware:** RTX 3090 (24GB VRAM) + RTX A4500 (20GB VRAM). Sufficient for:
- Two 7-13B models in parallel (one per GPU), or
- One 70B model at 4-bit quantization across both GPUs.

**Inference server:** vLLM or Ollama, exposing an OpenAI-compatible API endpoint on localhost. The tournament harness calls this endpoint directly — no dependency on opencode or Bedrock for local models.

**Models (Phase 1 — 2 models):**
- Llama 3.1 8B on RTX A4500 (reliable instruction following, 8B fits comfortably in 20GB)
- Mistral 7B v0.3 on RTX 3090 (reliable function calling, 7B in 24GB with headroom)

**Models (Phase 2 — time-sharing for 6 models):**
```
Schedule (6-hour overnight window):
00:00-02:00  Llama 3.1 70B (4-bit, both GPUs)  +  [API: Claude Haiku]
02:00-04:00  CodeLlama 34B (GPU 0)              +  Qwen2 72B (4-bit, GPU 1)
04:00-06:00  Llama 3.1 8B (GPU 0)               +  Phi-3 Medium (GPU 1)
```

Each 2-hour slot produces ~120 scenarios at 1 min/scenario. Six models x 120 scenarios = 720 model-scenario pairs per night + Claude via API = ~840 total evaluation points.

**Cost:** Electricity only. Zero marginal cost per session once models are downloaded. This is the fundamental economic advantage of local GPU inference — the cost structure is fixed (hardware + power), not marginal (per-token API pricing).

### Bedrock as fallback

If local GPU inference fails to produce valid MCP-compatible tool calls (the critical integration test), fall back to AWS Bedrock:

| Model | Bedrock cost/session | 100 sessions/day | Monthly |
|-------|---------------------|-------------------|---------|
| Claude 3 Haiku | ~$0.008 | $0.80 | $24 |
| Llama 3.1 (cross-region) | ~$0.005 | $0.50 | $15 |
| 4 models x 100 sessions | ~$0.025 | $2.50 | $75 |

Bedrock costs are real money, not covered by Claude Max. At scale (400 sessions/day across 4 models): ~$3.20/day or ~$100/month. Acceptable but nonzero. Local GPU is strongly preferred.

**Prerequisite verification (Phase 0):** Before committing to either architecture, a 2-4 hour spike must confirm: one local model session produces a telemetry row with the correct `model_id` and valid `session_id`. If this fails, Bedrock becomes the primary path.

### Cross-model tournament architecture

The tournament harness (`bin/tournament`) orchestrates cross-model evaluation:

```
For each scenario in the nightly batch:
  For each model (Llama, Mistral, Claude, ...):
    ┌──────────────────────────────────────────────────┐
    │  CONTROL ARM (no DontGuess)                        │
    │  Agent receives task, uses training data only.    │
    │  Record: tool_selected, tokens_used, outcome.     │
    ├──────────────────────────────────────────────────┤
    │  TREATMENT ARM (DontGuess enabled)                 │
    │  Agent receives same task + toolrank_find access.  │
    │  Record: tool_selected, tokens_used, outcome,     │
    │          toolrank_calls, reformulation_count.      │
    └──────────────────────────────────────────────────┘

    Beat condition (treatment wins if ANY of):
    - Treatment selects a better tool (works when control's fails)
    - Treatment selects same tool in fewer tokens
    - Treatment finds a tool control does not know about (freshness delta)
```

**Honest measurement requirements:**
- Same model in both arms (controls for training data bias)
- Same task, same system prompt (except DontGuess access)
- Temperature = 0 where possible (deterministic execution)
- At least 50 scenarios per A/B batch for statistical significance

### Agent execution as correctness signal

For high-break-score scenarios, the tournament goes beyond search:

1. Agent searches DontGuess, selects a tool
2. Agent installs the tool in a sandboxed environment
3. Agent uses the tool to complete the described task
4. Outcome recorded: `success`, `failure`, `partial`

This provides execution ground truth. A tool that DontGuess recommends and that an agent can successfully install and use is validated at a level no search-only test achieves.

**Limitation (adversary's point):** "Tool works" is not "tool is BEST." Execution validates that the recommended tool solves the problem, not that it is the optimal solution. The "best" determination requires comparative execution (try multiple tools on the same task), which is expensive. Reserve for tier-1 promotion candidates, not for nightly bulk runs.

### Session concurrency and throughput

**Concurrency limits by component:**

| Component | Limit | Bottleneck |
|-----------|-------|-----------|
| MCP HTTP handler | ~100 concurrent | Stateless, Starlette handles natively |
| SQLite telemetry (WAL mode) | ~100 concurrent writes | Lock contention above this |
| Local GPU inference | 1-2 concurrent | VRAM saturation |
| Claude API (Haiku) | ~10 concurrent | Rate limits |

**Practical ceiling: 800-1,200 sessions/day** on current hardware. Limited by GPU inference throughput (1 session/minute per model) across a 6-hour overnight window, not by the DontGuess server.

**Telemetry scaling fix:** At >100 concurrent sessions, batch-write telemetry — buffer in memory per worker, flush every 10 sessions or every 60 seconds. ~20 lines in `telemetry.py`.

## 10. The Escape Velocity Dashboard

### What "sure we're going to win" looks like in measurable terms

The dashboard renders five panels, each targeting a specific dimension of product readiness. The adversary demanded falsifiable metrics; the operator demanded measurable evidence; the RPT purist demanded honest labeling. These panels address all three.

### Panel 1: Null-Product Beat Rate

**What it shows:** Percentage of tournament scenarios where DontGuess-assisted agents outperform agents using training data alone. Computed from the A/B tournament (Section 9).

**Target:** >70% (adversary-approved threshold for launch readiness).

**Why 70% and not higher:** Some queries ("what's a good Python linter?") are answered perfectly by training data. DontGuess adds no value for these. The 30% where training data wins are well-known, well-documented tools. The 70% where DontGuess wins are long-tail, fresh, or context-dependent queries. 70% means DontGuess is additive for the majority of real discovery tasks.

**Kill switch:** If this metric drops below 50% for 7 consecutive nights, the dashboard goes red with a hard stop message: "Product thesis under threat. Training data is winning." This is not a bug — it means the null product has improved (new model generation) or DontGuess has regressed. Either way, it demands investigation before any other work continues.

### Panel 2: Behavioral vs Heuristic Score Gap

**What it shows:** The escape velocity gap from Section 3 — `mean(behavioral_score - heuristic_score)` across all queries with both scores. Plotted as a time series.

**Target:** `positive_gap_fraction > 0.5` for 7+ consecutive fast loop runs.

**What it means when positive:** DontGuess's observed behavioral data (what agents actually converge on) is outperforming the hand-coded heuristic scorer. The product's own measurements are doing more work than editorial judgment. At this point, the heuristic weights become less important and behavioral data should be weighted more heavily. This is the escape velocity condition from the product vision.

**What it means when negative:** The heuristic scorer is still doing more work than behavioral data. The product is still bootstrapping. Not a crisis — expected in the early weeks — but a clear indicator that the flywheel has not yet engaged.

### Panel 3: Cross-Model Convergence Accumulation

**What it shows:** Number of query-tool pairs where 3+ models from 2+ model families independently converge on the same tool. Plotted as a cumulative count over time.

**Labeling discipline:** This panel shows two lines:
- **Harness convergence** (from overnight tournament sessions): useful for behavioral measurement but does not constitute the moat.
- **Organic + external convergence** (from development usage and external users): this IS the moat being built.

The adversary demanded this separation. The purist confirmed it. The operator's correction does not change it — even though agent sessions are real usage, the *independence* of motivation matters for convergence claims. Harness-driven agents share a task source; independently-motivated agents do not.

**Target:** Organic convergence line growing. Rate does not matter initially — even 1 new convergence point per week is signal. The rate matters after launch when external users contribute.

### Panel 4: Domain Coverage Map

**What it shows:** Heatmap of golden query coverage by domain. Green: domain has >5% of tier-1 suite. Red: domain below 5% or absent. Yellow: domain approaching 30% ceiling.

**Required domains:** embedded, ml, scientific, security, game-dev, mobile, web, devops, data.

**What it catches:** Coverage bias in the golden suite. If "web" is 40% of queries and "embedded" is 0%, the dashboard shows the imbalance immediately. The generation pipeline's domain quota enforcement (Section 8) prevents this structurally, but the dashboard makes it visible for human oversight.

### Panel 5: Loop Health and Velocity

**What it shows:** Composite diagnostic panel:
- Fast loop cadence (actual time between `run_once()` calls vs target)
- Medium loop throughput (fixes attempted/applied per hour)
- Slow loop experiment velocity (experiments per 4-hour window)
- Oscillation autocorrelation (from Layer 4)
- Tier-1 golden query count (cumulative, with 200-query launch gate marked)

**What it catches:** Stuck loops. If the fast loop hasn't run in 2 hours, something is broken. If the slow loop runs 0 experiments for 3 consecutive windows, weight bounds are exhausted. If oscillation autocorrelation is <-0.3, the slow loop is fitting noise and has auto-halved its step size.

### The null-product A/B as permanent infrastructure

The A/B test is not a one-time evaluation. It runs every night as part of the tournament. It never lies. If DontGuess stops beating the null product, the dashboard goes red immediately. This is the permanent kill switch — if the product cannot beat training data, the product thesis is wrong.

Cost of permanent A/B: doubling the tournament (control + treatment arms) means ~100 scenarios/night instead of 50 for the same models. Local GPU cost is unchanged (free). Claude API cost doubles to ~$3/night. Acceptable for the most important metric in the system.

## 11. Launch Gate Definition

### What "sure" means in measurable terms

The RPT purist is correct: RPT provides no concept of competitive certainty. "Sure we're going to win" is not an RPT concept. What RPT provides: "the system is resonating, or it is not." Resonance is binary at any given measurement. The launch gate measures whether DontGuess is resonating — not whether it will win the market.

### Falsifiable conditions

All of the following must be true simultaneously. If any one is red, do not launch.

```
LAUNCH_READY =
    tier1_queries >= 200
    AND domain_coverage: all REQUIRED_DOMAINS have >= 5% of suite
    AND domain_coverage: no domain has > 30% of suite
    AND null_product_beat_rate >= 0.70
        (treatment arm wins >70% of tournament scenarios
         on execution-validated queries)
    AND cluster_return_rate < 0.15
        (agents don't come back and retry within 24h)
    AND mean_session_queries < 2.5
        (agents find what they need in <2.5 queries on average)
    AND escape_velocity_gap > 0 for 7 consecutive days
        (behavioral data outperforming heuristic scores,
         computed on organic data only)
    AND baron_ab_directional_positive
        (Baron uses fewer tokens with DontGuess than without,
         directional signal sufficient — n=1 prevents
         statistical significance at p<0.05)
```

### The operator's reframe and its implications

The operator says: "The real A/B is: do AI agents across multiple model families find better tools faster through DontGuess than through their training data alone? Every agent session IS a real session."

This means the cross-model tournament IS the real A/B test. It is not a synthetic approximation of a real test — it is the test itself, conducted on the actual target audience. The 70% beat rate threshold applies to real usage by real AI agents, not to a simulation of hypothetical users.

**Implication for the Baron A/B:** The Baron A/B (human-directed agent sessions with random DontGuess enable/disable) remains in the launch gate, but its role shifts. It is no longer "the only honest gate" (as the adversary claimed). It is *one* gate — the one that validates the Human-AI edge (does the human-agent system benefit from DontGuess?). The cross-model tournament validates the AI-App edge (do agents benefit?). Both gates must pass.

The Baron A/B is deliberately set to directional-positive rather than statistically significant. At n=1, p<0.05 requires an implausibly large effect size. Directional signal (fewer tokens with DontGuess than without, across 2 weeks of real work) is sufficient confidence for launch. Statistical significance comes post-launch from external users.

### The purist's topology point

> "One organic non-Claude data point changes topology more than all synthetic data."

This is true and is NOT contradicted by the operator correction. The operator says agent sessions are real usage (correct). The purist says independently-motivated cross-model usage changes data topology (also correct). Both hold simultaneously:

- Harness-driven Llama sessions produce real AI-App edge data for behavioral measurement.
- An external user's Llama agent searching DontGuess for their own task produces the same AI-App edge data PLUS changes the convergence topology (independent motivation from a different model family).

The launch gate includes `escape_velocity_gap > 0 for 7 consecutive days on organic data` precisely to honor this distinction. Harness data does not contribute to this metric. The escape velocity condition requires organic usage to be producing genuine signal — not just harness-driven signal that looks like organic.

Post-launch, the first external non-Claude user who generates a convergence point with an organic Claude user is the moment the moat topology changes. That single data point is worth more than 25,000 harness-driven points for the strategic thesis, even though the harness points are individually valid as AI-App edge measurements.

### Timeline estimate

At 50 new candidates/night from the generation pipeline, ~15 passing negative validation, and ~10-15 promoted to tier-1 per weekly human review:

| Milestone | Weeks from Phase 0 start |
|-----------|--------------------------|
| Generation pipeline operational | 1-2 |
| Local GPU inference verified | 1-2 (parallel) |
| First tournament run | 2-3 |
| 100 tier-1 golden queries | ~7 |
| 200 tier-1 golden queries (gate) | ~15 |
| Baron A/B protocol complete (2 weeks) | 4-6 |
| Escape velocity gap assessment | 5-7 |
| **All gates green (optimistic)** | **6-8** |
| **All gates green (realistic)** | **10-15** |

The realistic estimate accounts for: GPU integration issues, candidate quality variance requiring multiple review cycles, and the escape velocity gap requiring genuine organic usage volume that may take weeks to accumulate.

## 12. Implementation Roadmap (Extension)

Phases 1-3 from Section 5 remain unchanged. This section adds Phases 4-8 continuing the value function implementation into autonomous generation and cross-model usage.

### Phase 0: GPU Verification Spike (1-2 days, prerequisite)

**This phase gates all cross-model work.** Do not design the 6-model schedule before confirming the basic integration works.

Tasks:
1. Install vLLM or Ollama on the host machine.
2. Download Llama 3.1 8B (4-bit quantized).
3. Verify model loads on RTX A4500 and serves via OpenAI-compatible API.
4. Write `bin/sim-session`: given a task description and model endpoint, spawns an agent that calls `toolrank_find` via HTTP MCP, records session telemetry.
5. **Critical integration test:** One Llama session must produce a telemetry row with `model_id='llama-3.1-8b'` and valid `session_id`. Pass = Phase 0 done. Fail = fall back to Bedrock.

Cost: $0. Time: 2-4 hours of implementation + debugging.

**opencode + Bedrock verification (parallel track):** If Baron has AWS credentials configured, verify Bedrock access in parallel. Install opencode, connect to Bedrock, run one Llama session. If this works and local GPU does not, Bedrock becomes the primary cross-model path. If both work, local GPU is preferred (zero marginal cost).

### Phase 4: Golden Generation Pipeline (3-5 days)

**Files:** `bin/generate-golden`, `improve.py`, `golden-candidates.jsonl` (new)

1. Build `bin/generate-golden`: reads `data/coverage_gaps.jsonl`, calls Opus with adversarial prompt, writes candidates to `data/golden-candidates.jsonl`.
2. Implement negative validation filter: run each candidate against current search index, reject candidates where DontGuess already returns expected tools in top 5.
3. Add domain distribution enforcement: check domain fractions before writing, block over-represented domains, log under-represented ones.
4. Build `bin/review-golden`: interactive CLI for Baron to review candidates, accept/reject, promote accepted to `golden-queries.jsonl` tier=1.
5. Add dependency graph mining: parse real manifests from edge data, generate grounded scenarios.
6. Patch `improve.py` medium loop to filter on `tier == 1` for gating decisions (~30-50 lines).

Cost: ~$1.50/day once operational (Opus API calls).

### Phase 5: Cross-Model Tournament Harness (3-5 days)

**Files:** `bin/tournament`, `bin/sim-session` (from Phase 0)

Depends on: Phase 0 (GPU verification).

1. Build tournament harness: for each scenario, run control arm (no DontGuess) and treatment arm (DontGuess enabled) for each model.
2. Implement beat condition evaluation: compare tool selection, token usage, outcome across arms.
3. Wire to overnight loop: `bin/overnight` triggers `bin/tournament` after `bin/generate-golden`.
4. Add execution test framework for high-break-score scenarios: sandboxed install + use of recommended tool.
5. Implement time-sharing schedule for 6-model diversity (Phase 2 of GPU work, after 2-model validation).

Cost: Electricity for local GPU. ~$3/night for Claude API (control + treatment arms).

### Phase 6: Telemetry Labeling and Source Tracking (1-2 days)

**Files:** `telemetry.py`, `mcp_server.py`

1. Add `source` column to `search_telemetry`: values `harness`, `organic`, `external`.
2. Bootstrap-runner and tournament harness pass `source=harness` via a header or session parameter.
3. Development sessions (Baron's Claude Code) default to `source=organic`.
4. Escape velocity computation in `fast_loop.py` filters to `source IN ('organic', 'external')`.

This phase is small but structurally critical — it enforces the labeling discipline that the adversary, purist, and operator all agree on.

### Phase 7: Escape Velocity Dashboard (2-3 days)

**Files:** `web/dashboard.html`, `src/toolrank/api.py`

1. Add API endpoints for dashboard data: beat rate history, escape velocity gap time series, domain coverage map, convergence accumulation, loop health.
2. Build 5-panel dashboard (Section 10) in `web/dashboard.html`.
3. Add kill switch logic: if beat rate <50% for 7 consecutive nights, dashboard shows red alert.
4. Wire overnight loop to write tournament results to a format the dashboard can read.

### Phase 8: Baron A/B Protocol (2 weeks duration, 1 hour implementation)

**Files:** `mcp_server.py`

1. Add random disable toggle: 50% of sessions, DontGuess returns empty results (simulating no-DontGuess condition).
2. Log which sessions had access (`ab_group: treatment | control`).
3. After 2 weeks, compare tokens-per-resolution and task completion between groups.
4. This is a protocol Baron agrees to follow, not primarily a software task.

### Steady-state costs (post Phase 5)

| Component | Daily | Monthly |
|-----------|-------|---------|
| Golden generation (Opus API) | $1.50 | $45 |
| GitHub trending analysis | $0.07 | $2 |
| Tournament Claude API (A/B arms) | $3.00 | $90 |
| Local GPU inference | $0 (electricity) | ~$5 (electricity) |
| Dependency graph mining | $0 | $0 |
| **Total** | **~$4.57** | **~$142** |

Well under the $200/month infrastructure budget. The dominant cost is the Claude API for tournament A/B arms, which is the single most important measurement in the system and therefore the last thing to cut.

### Integration with existing overnight loop

The overnight loop (`bin/overnight`) currently runs: crawl, enrich, score, assess, index. The extension adds three stages:

```
overnight:
  existing:  crawl → enrich → score → assess → index
  new:       generate-golden → tournament → dashboard-update
```

The new stages are additive — they do not modify existing stages. `generate-golden` reads coverage gaps (output of the fast loop). `tournament` reads golden candidates and runs cross-model evaluation. `dashboard-update` writes results for the web dashboard.

The 5-line fix from Section 5 (real golden rate in overnight history) remains the highest-leverage single change and should be implemented before any of the new stages.

### Adversary attack resolution summary (Round 2)

| Attack | Status | Resolution |
|--------|--------|-----------|
| Opus grading its own exam (Attack 1) | **Resolved.** Negative validation inverts the echo chamber. Human review gates tier-1 promotion. |
| Coverage bias from prompting (Attack 2) | **Resolved.** Domain quotas enforced in code. Three structurally different sources. |
| "Sure we're going to win" undefined (Attack 3) | **Resolved.** Falsifiable launch gate with specific thresholds (Section 11). |
| Synthetic data is not the moat (Attack 4) | **Resolved.** Harness data labeled separately. Escape velocity computed on organic data only. Operator correction accepted: agent sessions are real usage, but independently-motivated sessions are what changes topology. |
| Cost model unquantified (Attack 5) | **Resolved.** $4.57/day steady state. Local GPU at zero marginal cost. |
| opencode/Bedrock unverified (Attack 6) | **Resolved.** Phase 0 verification spike as prerequisite. Bedrock as fallback. |
| Synthetic limits on competitive claims (Attack 7) | **Resolved.** "Bootstrap signal" labeling, not "moat data." |
| Medium loop has no negative-test mechanism (Attack 8, Round 1 residual) | **Partially resolved.** Domain quotas and negative validation help. Cross-domain interference detection (regression check against all domains before accepting medium-loop changes) is deferred to Phase 4. |
| RPT purist 60% compliance assessment | **Accepted.** Golden queries are correctly positioned as retrieval regression tests, not resonance measurements. The A/B test measures resonance. The design does not overstate what golden queries provide. |
| Operator correction (models are users) | **Integrated.** Cross-model sessions reframed as production usage. Labeling discipline preserved for topology claims. Both the operator and purist positions honored without contradiction. |
