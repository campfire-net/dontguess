# Design: Usefulness Validation System

> Produced by architect synthesis of adversarial design deliberation (4 dispositions, 3 rounds, 945 lines). Source: `docs/design-team/campfire-usefulness-validation.log`.

## 1. The Problem

DontGuess is a search engine for AI agents. Its value proposition: agents find tools in fewer tokens than guessing from training data. Current validation measures **search correctness** (golden queries, self-retrieval, tournament vs heuristic control arm). It does not measure **usefulness** — whether results actually help agents complete tasks faster or cheaper.

The bootstrap paradox: no real users exist to generate usage data, and no usage data exists to prove usefulness to potential users. Every validation approach risks self-referential measurement — the builder testing their own product against their own expectations.

Three specific failures in the current validation system must be fixed before any usefulness claim is credible:

1. **The tournament control arm is a strawman.** `run_control_arm()` in `bin/tournament` uses a 40-tool keyword lookup table, not actual LLM inference. The 70% beat rate launch gate (`gate_null_product_beat_rate`) is measuring "can DontGuess beat a crippled dictionary?" — not "can DontGuess beat training data?"

2. **The escape velocity gate is circular.** `gate_escape_velocity_consecutive()` in `launch_gates.py` checks `new_boosts > 0 AND sessions > 0`, which means "the system is running," not "behavioral data outperforms heuristics." The design spec defines escape velocity as `mean(behavioral_score - heuristic_score) > 0`. The implementation does not match.

3. **The observational boundary is permanent.** DontGuess cannot see outcomes after delivering results. A fast, confident, wrong answer generates positive telemetry (query-to-silence). This is a structural constraint, not a bug — but it must be disclosed in all validation claims and mitigated by proxy signals.

## 2. Adversary Attacks: Resolution Table

Thirteen attacks were raised across three rounds. Each is resolved, absorbed into the design, or documented as a permanent constraint.

### Devastating (structural changes required)

| # | Attack | Resolution |
|---|--------|-----------|
| 1 | **Tournament control arm is a strawman.** 40-tool keyword dict, not LLM inference. 70% beat rate is meaningless. | **Fix: Priority 1.** Replace `run_control_arm()` with live Claude Haiku call. ~50 LOC. Pattern exists in `bin/ab-test.run_group_b()`. Non-negotiable prerequisite for all other validation. |
| 9 | **Escape velocity is circular.** Implementation checks "system is running," not "behavioral > heuristic." | **Fix: Priority 1.** Rewrite `gate_escape_velocity_consecutive()` to compute actual `escape_velocity_gap = mean(behavioral_score - heuristic_score)` per the design in `design-value-function.md` Section 3. ~30 LOC. |
| 4 | **Observational boundary.** DontGuess cannot see outcomes. Fast+confident+wrong generates positive signals. | **Permanent constraint. Documented, not fixed.** Mitigated by `cluster_return_rate` as weak proxy (agents that fail come back). All validation claims must disclose: "outcome correctness is not observed." Do not claim "DontGuess makes agents more successful." Claim "DontGuess resolves tool queries in fewer tokens, with the caveat that downstream task success is not directly measured." |

### Significant (weaken specific proposals)

| # | Attack | Resolution |
|---|--------|-----------|
| 2 | **Google+parse comparison has structural format bias.** Structured API vs HTML scraping measures format, not quality. | **Killed.** Google comparison dropped entirely. The null product is training data, not Google. See Section 7. |
| 3 | **Golden queries are builder-curated tautologies.** Baron wrote the queries, Baron wrote the scorer, Baron chose the expected tools. | **Mitigated.** External query sources (Stack Overflow, cross-registry self-retrieval) break the builder-tautology cycle. Tier 1/tier 2 golden query split preserves frozen regression tests. Residual circularity in expected answers acknowledged. |
| 5 | **Arena selection bias in task design.** Tasks designed by someone who has seen the index will leak. | **Mitigated.** Task corpus sourced from Stack Overflow [tools]+[cli] tags, top 500 by votes. No filtering by builder. External ground truth = accepted answer. Stratification by difficulty tier makes the commodity/long-tail split explicit. |
| 8 | **RPT bootstrap is unfalsifiable.** "Development agents ARE real users" does not provide independent signals — one operator, one task source. | **Acknowledged.** The Canary (internal measurement) is structurally correct but statistically weak at n=1 operator. The Arena supplements with volume. External developer dogfooding is required for credible external evidence but is a go-to-market activity, not an engineering deliverable. |
| 10 | **Arena success gates cannot validate ranking optimality.** An agent that succeeds with a mediocre tool passes. | **Accepted as limitation.** The Arena validates usefulness (does the agent complete the task?), not optimality (did the agent use the best tool?). Ranking quality validation requires comparative execution (try multiple tools per task), which is deferred. |
| 11 | **Statistical significance costs real money.** Proper stratified analysis needs ~250 tasks minimum, not 30. | **Absorbed.** Arena sized at 250 tasks for p<0.05 at 80% power. Cost: ~$87 at Haiku rates. See Section 5. |
| 12 | **Canary proves nothing to external users.** Every builder uses their own product. | **Absorbed.** Canary is primary internal measurement. Arena with published methodology and SO corpus is external evidence. Both are needed; neither alone suffices. |

### Absorbed (team adapted)

| # | Attack | Resolution |
|---|--------|-----------|
| 6 | **Fossil record mining is survivorship bias.** You only find failures, never silent successes. | **Killed.** Proposal dropped. See Section 7. |
| 7 | **Canary suffers from self-dog-food blindness.** DontGuess dev work has narrow tool discovery needs. | **Absorbed.** Canary measures organic sessions across ALL projects using DontGuess, not just DontGuess development. Usage in non-DontGuess projects (if configured) provides less biased signal. |
| 13 | **Agent groupthink.** All deliberation agents share training data; proposals are limited to what one person can build. | **Acknowledged as permanent.** The bootstrap paradox is ultimately a go-to-market problem. External developer adoption is the real validation but is not codeable. Build the Arena and Canary now; pursue external validation in parallel. |

## 3. The Validation System

Three components, ordered by RPT compliance. The Canary is the primary measurement system. The Arena is the bootstrap accelerator. The Tournament is the launch gate.

### 3.1 Canary (Primary — Internal Resonance)

**What it is:** DontGuess configured in every development session via `.mcp.json`. Organic telemetry flows into `search-telemetry.db` with `source='organic'`. The product's own fast loop processes these signals identically to any other telemetry.

**What it measures:** The five AI-App edge signals from RPT on organic queries:

| Signal | How measured | Observable? |
|--------|-------------|-------------|
| Token count per search session | `mean_session_queries` from telemetry grouped by `session_id` | Direct |
| Retry rate | `AVG(is_retry)` from telemetry (column exists, currently unused) | Direct |
| Hedging frequency | Requires agent output text — outside observational boundary | **Not observable** |
| Decision latency | Tokens between receiving results and next action — requires agent-side instrumentation | **Not observable** |
| Error recovery cost | `cluster_return_rate` as weak proxy (agents that fail return to same query cluster) | Proxy only |

**RPT compliance:** 90%. This is the only validation approach where the optimization system lives inside the product (RPT Section 1.7). Low data volume is the limitation, not the architecture.

**What is missing today:** Explicit weekly reporting of the three observable signals on organic data. The telemetry columns exist (`is_retry`, `session_id`, `latency_ms`) but no reporting code reads them for organic-only analysis.

**Implementation:** ~20 LOC addition to `improve.py:measure_telemetry()`: add `WHERE source='organic'` filter and compute `mean_session_queries`, `retry_rate`, `cluster_return_rate` on organic data only. Output to `data/canary-report.jsonl`.

### 3.2 Arena (Secondary — Bootstrap Accelerator)

**What it is:** A task completion benchmark using externally-sourced tasks. Two arms: Claude Haiku alone (the real null product) vs Claude Haiku + DontGuess. Measures end-to-end task completion, not search result quality.

**Critical design decisions:**

1. **Task source is external.** Stack Overflow [tools]+[cli] tags, top 500 by votes. No filtering by the builder. Accepted answer is ground truth. This breaks the builder-tautology cycle (Attack 3).

2. **Tasks are stratified by difficulty.** Not all tasks are equal, and DontGuess does not claim to beat training data on commodity queries:
   - **Stratum 1 — Commodity (~60%):** Training data is sufficient. "json parser," "python linter." Expected: DontGuess adds zero value. This is the honest denominator.
   - **Stratum 2 — Recency (~15%):** Tools released after training cutoff. Expected: DontGuess wins via fresh index.
   - **Stratum 3 — Specificity (~15%):** Narrow-domain tools training data has never seen. Expected: DontGuess wins via breadth.
   - **Stratum 4 — Composition (~10%):** Multi-tool workflows. Expected: future capability, currently weak.

3. **Metric hierarchy is strict.** A tool recommendation that saves 50 search tokens but causes task failure is infinitely worse than one that costs 200 tokens and succeeds:
   1. Task success rate (binary, non-negotiable gate)
   2. Total tokens to success (lower is better, ONLY for successful tasks)
   3. Hedging score (regex-based hedging phrase detection in response text)
   4. Time to success

4. **Two arms, not four.** The original proposal had four arms (Naked, DontGuess, Web Search, Both). Simplified to two: Claude Haiku alone vs Claude Haiku + DontGuess. This halves cost and provides the one comparison that matters: does DontGuess add value over the null product?

5. **Arena telemetry routes INTO the product.** Arena results are written to `search-telemetry.db` with `source='arena'`. The fast loop processes them. The Arena is a feeding tube for the product's own loops, not a separate validation system. This is the architectural insight that makes validation and product improvement the same system.

**Arms:**

- **Control:** Claude Haiku receives a task description and must recommend a tool using only its training knowledge. No tool access. Cost: ~$0.001/query.
- **Treatment:** Claude Haiku receives the same task with DontGuess MCP tool available. Cost: ~$0.001/query + DontGuess compute.

**Beat condition:** Treatment wins if:
- Treatment succeeds AND control fails, OR
- Both succeed AND treatment uses at least 20% fewer total tokens (the 20% threshold prevents wins within noise — see Boundary 1 in Section 4)

**Statistical requirements:** 250 tasks minimum for p<0.05 at 80% power on the overall improvement claim. For by-stratum analysis, ~60 per stratum. See Section 5 for cost.

### 3.3 Tournament with Real LLM Control (Tertiary — Launch Gate)

**What it is:** The existing `bin/tournament` infrastructure with the control arm fixed to use real LLM inference instead of the 40-tool keyword dictionary.

**What it measures:** Competitive beat rate — does DontGuess find tools that training data does not, or find them with materially fewer tokens? This is the weakest evidence from an RPT perspective (measures comparison, not resonance) but the most legible for distribution: early adopters need "DontGuess beats guessing" as the entry point.

**Fix required:** Replace `run_control_arm()` (lines 210-272) with a live Claude Haiku call. The pattern exists in `bin/ab-test.run_group_b()`:

```python
# In run_control_arm(), replace COMMON_TOOLS lookup with:
response = anthropic.Anthropic().messages.create(
    model="claude-haiku-4-5-20250315",
    max_tokens=300,
    messages=[{"role": "user", "content": f"What CLI tool best solves: {query}? "
               "Reply with just the tool name and a one-line install command."}]
)
```

Gate on `ANTHROPIC_API_KEY` environment variable. Add `--live-control` flag to preserve the old heuristic mode for cost-free testing.

**Launch gate update:** `gate_null_product_beat_rate()` threshold should remain at 70% but now measures against real LLM inference, making it a meaningful gate rather than a rubber stamp.

### 3.4 Cross-Registry Self-Retrieval (Supplementary — Coverage Quality)

**What it is:** Use tool descriptions from external registries (Smithery, Glama, official MCP registry) as queries to DontGuess. Check whether DontGuess returns the described tool in its results. This is externally-grounded self-retrieval — the registry is the ground truth, not the builder.

**Why it matters:** Self-retrieval against the builder's golden queries is circular. Self-retrieval against external catalogs is not — it measures whether DontGuess's index and ranking can find tools that external registries say exist.

**Implementation:** ~100 LOC. For each tool in Smithery/Glama with a description field, query DontGuess with that description. Record hit/miss. Compute retrieval rate by registry. Run weekly as a continuous quality metric.

### 3.5 Demo Recording (Persuasion — One Compelling Example)

**What it is:** A recorded session pair showing the "30-second horror show":
1. Agent without DontGuess tries to find an obscure but excellent tool. Hallucinates a tool name, tries to install it, fails, reformulates, tries pip, tries npm, eventually writes a worse version from scratch — burning 2000+ tokens.
2. Same task with DontGuess: one call, correct answer, 50 tokens.

**Task selection:** Scan `data/golden-queries.jsonl` for expected tools NOT in the old `COMMON_TOOLS` dictionary (obscure tools that training data is unlikely to know). Record both sessions with the same task.

**What it proves:** Nothing statistically. Everything psychologically. Loss aversion is stronger than gain seeking. Showing what agents LOSE without DontGuess is more compelling than showing what they gain.

**Cost:** ~$5 for a few Claude sessions. Zero infrastructure.

## 4. Engineering Boundaries

Five boundary conditions identified during deliberation that constrain implementation:

### Boundary 1: Beat condition token threshold

With a live LLM control arm, both arms use LLM tokens. The control uses ~100-200 tokens; DontGuess search uses ~40-80 structured tokens. A 1-token difference is noise, not a win. **Threshold: treatment beats on tokens only if at least 20% fewer.** This prevents wins within measurement noise.

### Boundary 2: Source column enforcement

`measure_telemetry()` in `improve.py` has no `source` filter. When Arena data enters `search-telemetry.db`, it will contaminate organic metrics. **Blocking pre-condition:** add `WHERE source='organic'` to all SQL queries in `measure_telemetry()` before any Arena data flows into the telemetry DB. The escape velocity gate in `launch_gates.py` must also filter — it currently reads ALL rows in `fast_loop_history.jsonl` regardless of source.

`_VALID_SOURCES` in `telemetry.py` (line 263) already includes `'external'` as an unused label. Add `'arena'` for Arena-specific data, or reuse `'external'` — the label must be distinct from `'organic'` and `'harness'`.

### Boundary 3: Stack Overflow API access

The SO API free tier allows 10,000 requests/day at 30/second max. The [tools] tag has ~8,000 questions. Full scrape: ~4.5 minutes. Returns structured JSON — no HTML parsing needed. `GET https://api.stackexchange.com/2.3/questions?tagged=tools&order=desc&sort=votes&site=stackoverflow`. Second call per question for accepted answer body. No API key required for anonymous access within rate limits.

### Boundary 4: RPT signal observability limits

Hedging frequency and decision latency require reading the agent's response text, which is outside DontGuess's observational boundary in production. In the Arena, the control arm's response text IS available — store it in the result record and apply `hedging_score()` (regex for "might," "could try," "possibly," "may work," "not sure," "I think"). This gives hedging measurement in the Arena but not in production Canary measurement. Decision latency in the Arena is a proxy (tokens between results and first command in response text), not a direct measurement.

### Boundary 5: Escape velocity must be computed on organic data only

The escape velocity metric (`behavioral_score - heuristic_score > 0`) measures whether DontGuess's own behavioral data outperforms its heuristic priors. This must be computed on organic data exclusively. Arena/harness data inflates behavioral signals artificially. The `source` filter (Boundary 2) is the enforcement mechanism.

## 5. Cost Estimates

| Component | API Cost | Compute | LOC | Time |
|-----------|----------|---------|-----|------|
| Fix control arm | ~$0.50/200 queries | Negligible | ~50 | 2 hours |
| Fix source filter | $0 | Negligible | ~20 | 1 hour |
| Fix escape velocity gate | $0 | Negligible | ~30 | 2 hours |
| `hedging_score()` function | $0 | Negligible | ~20 | 1 hour |
| `bin/so-corpus-builder` | $0 (free SO API) | Negligible | ~150 | 1 day |
| Arena telemetry routing | $0 | Negligible | ~100 | 1 day |
| Cross-registry self-retrieval | $0 | Negligible | ~100 | 1 day |
| **Arena run (250 tasks)** | **~$87** | Moderate | N/A | 2-4 hours |
| Demo recording | ~$5 | Negligible | 0 | 1 hour |

**Grand total implementation:** ~470 LOC across ~5 files, ~4 days of work.

**Arena run cost:** ~$87 per full run at Haiku rates (250 tasks x 2 arms x ~$0.001/query, plus DontGuess compute, plus SO scraping overhead). Reruns after index/scorer changes cost the same. Budget for 3-5 runs during initial validation: ~$250-435.

**Minimum Arena size for statistical significance:** 250 tasks (two-proportion z-test, detecting 7% improvement at p<0.05 with 80% power, given expected ~20-30% improvement in recency/specificity strata representing ~30% of queries).

## 6. Build Plan

### Priority 1 — BLOCKING (nothing else is valid until these are fixed)

These three fixes are prerequisites. No validation number produced by the current system is meaningful.

**1a. Fix `run_control_arm()` in `bin/tournament`** (~50 LOC, 2 hours)

Replace the `COMMON_TOOLS` keyword dictionary with a live Claude Haiku API call. Transplant the pattern from `bin/ab-test.run_group_b()`. Gate on `ANTHROPIC_API_KEY` environment variable. Add `--live-control` flag to preserve old heuristic mode for cost-free regression testing.

**1b. Fix source filter in `measure_telemetry()`** (~20 LOC, 1 hour)

Add `WHERE source='organic'` to all SQL queries in `improve.py:measure_telemetry()`. Critical before any Arena data enters `search-telemetry.db` — without this, harness/arena data contaminates organic behavioral metrics.

**1c. Fix `gate_escape_velocity_consecutive()` in `launch_gates.py`** (~30 LOC, 2 hours)

Replace the current check (`new_boosts > 0 AND sessions > 0`) with the actual escape velocity computation: `escape_velocity_gap = mean(behavioral_score - heuristic_score)` per `design-value-function.md` Section 3. The gate should pass when `positive_gap_fraction > 0.5` for N consecutive fast loop runs, not when "the system is running."

### Priority 2 — HIGH VALUE, LOW COST

**2a. Add `hedging_score()` to Arena result analysis** (~20 LOC, 1 hour)

Simple regex function counting hedging phrases ("might," "could try," "possibly," "may work," "not sure," "I think") divided by total word count. Applied to control arm and treatment arm response text. High signal, trivial cost.

```python
import re

HEDGING_PATTERNS = re.compile(
    r'\b(might|could try|possibly|may work|not sure|I think|perhaps|'
    r'you could|one option|it.s possible)\b', re.IGNORECASE
)

def hedging_score(text: str) -> float:
    """Fraction of words that are hedging phrases. Lower = more confident."""
    words = text.split()
    if not words:
        return 0.0
    hedges = len(HEDGING_PATTERNS.findall(text))
    return hedges / len(words)
```

**2b. `bin/so-corpus-builder`** (~150 LOC, 1 day)

Stack Overflow API scraper producing the external task corpus. Output format: JSONL compatible with existing golden query pipeline.

Behavior:
1. Fetch top 500 questions tagged [tools]+[cli] by votes from SO API
2. For each question with an accepted answer, extract the tool name from the answer
3. Classify into strata (commodity/recency/specificity/composition) based on tool age and domain
4. Output: `data/so-arena-corpus.jsonl` with schema `{query, expected_tool, stratum, so_question_id, so_score}`
5. Rate limit: 30 requests/second, exponential backoff on 429

### Priority 3 — COMPLETE THE ARCHITECTURE

**3a. Route Arena telemetry into `search-telemetry.db`** (~100 LOC, 1 day)

Add `'arena'` to `_VALID_SOURCES` in `telemetry.py`. Write a `flush_arena_results_to_telemetry()` function that reads Arena/tournament result JSONL and inserts non-duplicate records into `search-telemetry.db` with `source='arena'`. The fast loop processes all labeled sources identically. The Canary metrics filter to `source='organic'` only.

Integration point: `telemetry.py` line 263 already has an unused `'external'` source label. Either reuse it or add `'arena'` alongside it.

**3b. Cross-registry self-retrieval** (~100 LOC, 1 day)

For each tool in Smithery/Glama with a description:
1. Query DontGuess with the description text
2. Check if the tool appears in top-10 results
3. Record hit/miss per registry

Output: `data/cross-registry-retrieval.jsonl`. Run weekly. This is the externally-grounded coverage quality metric.

### Priority 4 — RUN THE ARENA

**4a. Run the Arena** (2-4 hours compute, ~$87)

Execute the SO-sourced task corpus (250 tasks minimum) through both arms:
- Control: Claude Haiku, no tools, raw inference
- Treatment: Claude Haiku + DontGuess MCP tool

Record per-task: success (binary), total tokens, hedging score, tool recommended, stratum.

**4b. Publish results** (1 hour)

Report ALL results, including where training data wins. Stratify by difficulty tier:
- Overall: "DontGuess adds value in X% of queries"
- By stratum: "zero delta on commodity, +Y% on recency/specificity"
- Statistical: confidence intervals, not single numbers

The honest claim: "In 250+ externally-sourced tool discovery tasks, DontGuess improved outcomes on the 20-30% of queries where training data was insufficient. For commodity queries, it adds no delta and no harm."

### Priority 5 — EXTERNAL EVIDENCE (not blocked by code)

- Find one external developer to install DontGuess, use it for a week, report results
- Published Arena methodology and results that anyone can reproduce
- Demo recording of the "horror show" scenario

These are go-to-market activities, not engineering tasks. They require external contact and cannot be fully automated.

## 7. Killed Proposals and Why

| Proposal | Killed By | Reason |
|----------|-----------|--------|
| **Google+parse comparison** | Adversary (Attack 2) + RPT purist | Structural format bias: structured API vs HTML scraping measures format, not quality. The null product is training data, not Google. Infrastructure cost (~200 LOC, API keys, unstable results) not justified for a measurement that answers the wrong question. |
| **Fossil record mining** | Adversary (Attack 6) | Survivorship bias. Mining public conversations for "agent struggle moments" guarantees finding cases where DontGuess would have helped. Cannot mine the 95% of tool discovery that happens silently (agent knows from training data). Counterfactuals from transcripts are not computable — you cannot know what the agent would have done with different results without running the alternate timeline. |
| **Raw token counting without success gates** | Adversary (Attack 4) | The flyway/MongoDB attack: fast + confident + wrong = negative value. An answer that costs 50 tokens but causes 500 tokens of error recovery is worse than a 200-token answer that succeeds. Tokens are meaningful ONLY when gated on task success. |
| **4-arm Arena** | Adversary (Attack 11) + Systems pragmatist | Prohibitively expensive for statistical significance. 4 arms x 450 tasks = 1,800 sessions = ~$1,600. Simplified to 2 arms (null product vs DontGuess), which is the one comparison that matters. |
| **Google as null product baseline** | RPT purist | The null product is the agent's own training data (RPT Section 5.2: "the model is a battery"). Google is a different product, not the absence of DontGuess. The honest comparison is: does DontGuess add value beyond what the agent already knows? |
| **Shadow traffic proxy** | Not explicitly killed, but subsumed | Retrospective A/B from session logs is the Canary with extra steps. If session logs are accessible, analyze them directly as organic telemetry. |
| **Task completion loop (executable commands in results)** | Deferred, not killed | Interesting product direction (DontGuess returns executable one-liners with verification commands) but is a feature, not a validation architecture. Validation should measure the product as it exists, not a hypothetical extension. |

## 8. Expected Results and Honest Positioning

Based on the difficulty gradient analysis, the expected Arena results:

| Stratum | % of Queries | Expected DontGuess Delta | Why |
|---------|-------------|-------------------------|-----|
| Commodity (training data sufficient) | ~60% | Zero or slightly negative (added latency) | Claude knows "json parser = jq" from training data. DontGuess adds a tool call with no new information. |
| Recency (tools post-training cutoff) | ~15% | Positive (DontGuess wins) | DontGuess crawls weekly. Training data is 6-12 months old. Tools like `uv`, `oxlint`, `biome` are in DontGuess's index but not in training data. |
| Specificity (narrow domain) | ~15% | Positive (DontGuess wins) | "CLI for diffing SQLite databases," "MCP server for Jira." Training data has weak coverage of the long tail. DontGuess indexes 2.4M tools across 13 registries. |
| Composition (multi-tool workflows) | ~10% | Neutral (future capability) | "What works well with difftastic?" requires co-usage data that does not yet exist. |

**The honest pitch:** DontGuess extends an agent's capability on the 20-30% of tool discovery tasks where training data is insufficient. For those tasks, it is the difference between success and failure. For commodity tasks, it adds no value and no harm. The value concentrates in the long tail — recency and specificity — and grows as the behavioral feedback loops accumulate cross-agent convergence data.

**What DontGuess does NOT claim:** It does not claim to beat training data on common tools. It does not claim to find the optimal tool (only a useful one). It does not claim to observe whether the agent succeeded after using a recommendation.

## 9. Relationship to Existing Systems

### Connection to Value Function (`design-value-function.md`)

The usefulness validation system is complementary to, not a replacement for, the 4-layer value stack:

- **Layer 0 (Correctness Gate):** `cluster_return_rate` remains the persistence proxy for outcome quality. The Arena provides external validation of this proxy's effectiveness.
- **Layer 1 (Resolution Depth):** `mean_session_queries` from the Canary directly measures whether organic users resolve queries in fewer searches.
- **Layer 2 (Quality Composite):** The Arena's task success rate is an external check on whether `V_medium` improvements actually translate to agent success.
- **Layer 3 (Novelty):** `position_surprise` is validated when Arena results show that non-obvious tool recommendations (high surprise) lead to successful task completion.

### Connection to Product Vision (`product-vision.md`)

The validation system operationalizes the product vision's claim that "development agents ARE real users":

- The Canary IS the product's metabolism — not a test of the product, but the product itself.
- The Arena IS the bootstrap campaign infrastructure described in the product vision's 4-week cadence — but with external task sources instead of builder-curated ones.
- The Tournament with real LLM control IS the escape velocity measurement described in the product vision — but with an honest control arm instead of a strawman.

### Connection to Telemetry Infrastructure

The key architectural insight: validation infrastructure and product loops are the same system. Arena telemetry flows into `search-telemetry.db` via the existing `source` column. The fast loop processes behavioral signals from all sources identically. The Canary metrics filter to `source='organic'` for escape velocity computation. One data store, properly labeled, not two parallel systems.

Integration point: `telemetry.py` line 263 defines `_VALID_SOURCES = frozenset({"organic", "harness", "external", "ab_control"})`. The `"external"` label is defined but unused — this is the hook for Arena data.

## 10. Permanent Constraints

These cannot be resolved within DontGuess's architecture. Design around them.

1. **The observational boundary.** DontGuess sees: query, results returned, session patterns, timing. DontGuess does not see: what the agent did with results, downstream task success, whether the tool was installed, whether the human was satisfied. `toolrank_report_usage` is the designed path to partial observability, but adoption depends on agent frameworks outside DontGuess's control.

2. **Hedging frequency is unobservable in production.** Detecting whether an agent hedged requires reading the agent's output text, which is permanently outside DontGuess's observational boundary. Measurable only in the Arena (where we control the agent and can read responses).

3. **Single-operator bootstrap.** One developer generating organic telemetry is statistically insufficient for confident conclusions. The Arena compensates with volume at the cost of external orchestration. This limitation resolves only when external users adopt the product — a go-to-market outcome, not an engineering deliverable.

4. **Agent groupthink.** All bootstrap agents share training data. Convergence signals from a single model family are n=1 for independence. Multi-model Arena runs (GPT-4, Gemini, open-source) are the deconfounding mechanism but require additional API integrations. The `model_id` telemetry column (already captured) enables this analysis when multi-model data arrives.
