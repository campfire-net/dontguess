# dontguess v0.6 — Token-Savings Foundation (read this first)

**Audience:** any agent picking up a `dontguess-17f` (v0.6) work item cold, with no
conversation history. This doc + your item description = everything you need.

**Status:** foundation/context for the v0.6 tree. Builds on v0.5.0 (shipped 2026-06-02).

---

## 1. The objective — optimize NET token savings, not hit-rate

```
net_tokens_saved = Σ saved_on_real_hits  −  Σ miss_costs  −  Σ false_positive_waste
```

- **saved_on_real_hits** — tokens a buyer avoided by consuming an above-floor cached entry instead of re-deriving (≈ the entry's `token_cost`).
- **miss_costs** — the ~500-token overhead of a buy that returns no usable match.
- **false_positive_waste** — tokens a buyer burned reading a *delivered-but-irrelevant* entry, then re-deriving anyway. **A false positive is worse than a miss.** This is the central design principle: the relevance floor (below) exists to turn false-positive hits into cheap, honest misses.

Every v0.6 change must be evaluable against this metric. Track C (`dontguess-eff`) builds the instrument that measures it; everything else should be A/B'd against it.

---

## 2. What v0.5.0 already shipped (the building blocks — file:symbol)

| Concept | Where it lives | Notes |
|---|---|---|
| **Relevance floor (M1a)** | `pkg/matching/ranking.go` → `RankOptions.MinSimilarity` (default **0.16**) | Below-floor candidates are excluded from `Rank()`. Enforced at the exchange too: `pkg/exchange/engine.go handleBuy` gates the fallback via `matchIndex.HasEmbedding(...)` so a below-floor entry emits `exchange:buy-miss`, **not** a junk hit. Authority: `docs/design/exchange-matching-d1-diagnostic-verdict.md` (note the CORRECTION section: floor is 0.16, not 0.35). |
| **Real similarity (M2)** | `pkg/exchange/engine.go` → `MatchResult.Similarity` (raw cosine) | Distinct from `confidence` (the L2 composite). Varies with real overlap. |
| **Consume signal (M5)** | `TagConsume = "exchange:consume"` (`pkg/exchange/state.go:54`); emitted by `emitConsumeSignal` in `handleSettle` on settle-complete; `entry_id` is **antecedent-derived (non-spoofable)**, not buyer-supplied. Queried by `ConsumeCountByEntry([]Message)` (`pkg/exchange/hitrate.go`). | "Did the buyer actually USE the delivered entry?" The behavioral signal that turns a "hit" into "value." |
| **Cross-agent convergence (412)** | `State.EntryBuyerMap map[string]map[string]struct{}` (`pkg/exchange/state.go:513`) tracks distinct buyer keys per entry; `BuildConvergenceMap(*State)` (`pkg/exchange/hitrate.go`) merges it; `HitRateReport.CrossAgentConvergence` counts entries with ≥3 distinct keys. | The heritage *ungameable* trust signal. **⚠️ See §3 — it is ~0 today.** |
| **Synthetic exclusion (M3)** | `TagSynthetic = "exchange:synthetic"` (`pkg/exchange/state.go:69`); predicate `pkg/demand.IsSynthetic(task)`; `ComputeHitRate` excludes tagged buys (two-pass). | Dev/load/test traffic must not pollute metrics. |
| **Per-agent identity (V4)** | wrapper `AGENT_CF_HOME` (`site/install.sh`, `_SIGNING_HOME`); `dontguess agent-init <name>` (`cmd/dontguess/agent_init.go`). | Lets distinct agents sign with distinct keys → makes convergence measurable. Authority: `docs/design/exchange-per-agent-identity-decision.md`. |
| **Honest reporter** | `pkg/exchange/hitrate.go ComputeHitRate` (quality-weighted: hit ⇔ `similarity ≥ DefaultMinSimilarity()` AND not synthetic) + `cmd/dontguess/hitrate.go`. | The prior 96.67% was inflated; honest rate ≈ 3%. v0.6 extends this reporter (Track C). |
| **Put quality-gate (V2/M4)** | `pkg/exchange/state.go applyPut`: `MaxTokensPerByte` cap (M4), `MinTokenCost`, content-hash dedup, `isTestLikeDescription`. | Track F builds put-economics on top of this. |
| **§4 high-reuse classes** | `docs/design/exchange-matching-measurement-review.md` §4 | Real reuse concentrates in reusable engineering artifacts: schema checklists (37×), cf-protocol CF_NO_PINS README (30×), CI path filter (19×), flock test pattern (16×) — NOT session ephemera. Track F steers toward this class. |
| **The diagnostic fixture** | `pkg/matching/d1_diagnostic_test.go` | Real (buy task → ideal entry) pairs + nonsense pairs + the floor sweep. Reuse it for Track B and for nonsense-regression checks elsewhere. Measured separation: junk cosine max ≈ 0.155, ideal min ≈ 0.183. |

Heritage concepts ("behavioral signals over preferences", "cross-agent convergence", "escape velocity", "observational boundary"): `docs/heritage/` and the project `CLAUDE.md` (Heritage section).

---

## 3. ⚠️ The non-obvious facts that will bite a cold agent

1. **Cross-agent convergence is ~0 in live data TODAY.** Until per-agent identities (V4, just shipped) are widely adopted, every buyer/seller key is the same shared identity, so `EntryBuyerMap` has 1 key per entry and `CrossAgentConvergence == 0`. `hitrate.go` even comments this ("current default: single shared identity"). **Therefore: tests for Tracks A and the e2e MUST SEED distinct buyer keys directly into `State.EntryBuyerMap`** (see `TestHitRate_CrossAgentConvergence` / `TestBuildConvergenceMap_MultiSeller` for the pattern). Do not "validate" against empty live convergence data and conclude the feature is inert.
2. **Relevance gates everything.** No behavioral booster, query expansion, or economic incentive may resurrect a below-floor entry. Layer-0/1 relevance is the gate; behavioral signals are tie-breakers/boosters *within the surviving (above-floor) set*. Keep boosters bounded.
3. **The live exchange (`~/.cf`) is READ-ONLY.** Harvest fixtures with read-only queries (`dontguess match-results`/`buys`/`hit-rate`). NEVER run `dontguess buy`/`put` writes and NEVER mutate `~/.cf` — those inject the exact dev/false-positive traffic this work is trying to suppress. All tests run on a scratch fs-transport exchange (`newTestHarness`/`exchange.Init` with `SkipConfigCascade`), never `~/.cf`. Expiry (Track A2) is an **operator-facing procedure**, not an autonomous live mutation.
4. **Gaming resistance.** Behavioral signals (consume, convergence) and put-economics are adversarial surfaces: an agent could self-consume, spin up sybil identities, or mislabel ephemera as "reusable" for more pay. Tie advantage to *observable, distinct-key* reuse; convergence must require ≥3 genuinely distinct keys.
5. **`go` is at `/usr/local/go/bin`** (`export PATH=/usr/local/go/bin:$PATH`). Build is pinned to **published campfire v0.32.0** — go.mod must NOT contain a local-path `replace` for campfire (that breaks CI/release; see `docs/design/` history). Read real `go test` FAIL/ok output, not a trailing-echo exit; run the FULL (non-short) suite at gates.

---

## 4. Track → entry points

- **A (`dontguess-860`, `-046`)** — ranking. `pkg/matching/ranking.go` (the `Rank`/`RankOptions` path) fed by consume/convergence inputs plumbed from `pkg/exchange` (`State.EntryBuyerMap`, `ConsumeCountByEntry`). A1 = positive boost; A2 = demote/expire on sustained deliver-without-consume. Seed distinct keys in tests (§3.1).
- **B (`dontguess-372`)** — embedding spike (GATE). `pkg/matching/embedding.go` is TF-IDF bag-of-words today. Measure TF-IDF vs a real dense embedder on the §2 fixture; output a tune-vs-replace verdict in `docs/design/`. Do NOT implement the replacement here.
- **C (`dontguess-eff`)** — `pkg/exchange/hitrate.go` + `cmd/dontguess/hitrate.go`. Add net-savings + per-query economics to the existing honest reporter without regressing quality-weighting/synthetic-exclusion.
- **D (`dontguess-af7`)** — buy-path query normalization/expansion (vocabulary alignment) to lift TF-IDF recall without new false positives (check against the nonsense fixture).
- **E (`dontguess-18c`)** — `site/install.sh` wrapper: auto-tag dev/CI buys+puts `exchange:synthetic` on an explicit marker; backward-compatible when absent.
- **F (`dontguess-13a`)** — put path/economics (`pkg/exchange` put-accept/pricing): bias residuals toward the §4 high-reuse class; gameable-resistant.

Every item: read this doc, then your `rd show <id>` description. Dispatch is **lean** — return structured results to the orchestrator; no cf convention ops required.
