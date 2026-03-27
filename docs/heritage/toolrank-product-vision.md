# DontGuess — Product Vision

## The Thesis

An AI agent can recommend `jq` for JSON processing from its training data. It costs zero tokens. DontGuess must be measurably better than this null product or it has no reason to exist.

But "measurably better" is table stakes. The actual thesis is stronger: DontGuess must become the default way AI agents find tools — the search engine for AI — or it will be absorbed as a feature by a platform that does.

The null product fails in three specific, testable ways:

1. **Freshness.** Training data is 6-12 months old. New tools ship daily. An agent's training data does not know about tools published after its cutoff, tools that broke in their latest version, or tools that were deprecated last week. DontGuess crawls 13 registries weekly and indexes 2.4M tools. The freshness delta is real on day one and grows with every crawl cycle.

2. **Local context.** The agent does not know what is installed on the machine it is operating. DontGuess scans the local system — PATH, man pages, package managers, MCP configs — and tells the agent what it already has. No other product provides this. The value is immediate: "you already have `jq` installed, use it" saves the agent from searching for something it already has.

3. **Cross-agent convergence.** When many independent agents discover that tool X solves task Y efficiently, that signal does not exist in any training dataset. Only a product that observes agent behavior across sessions can accumulate it. This is DontGuess's long-term moat — but it requires the feedback loop to be running. It is a weeks-to-months feature, not a day-one feature.

The honest pitch for month one: "I know about tools your training data doesn't, and I know what's on your machine." The pitch expands to "I know what works for agents like you" only after enough usage data flows through the internal loops.

### The Escape Velocity Argument

Being useful is not sufficient. DontGuess exists in a 12-18 month window before Anthropic builds tool search natively into Claude. The question is not "can DontGuess be helpful?" The question is "can DontGuess accumulate enough compounding behavioral data to be structurally irreplaceable before the window closes?"

Escape velocity, precisely defined: the point where query-tool affinity boosts computed from observed agent behavior outperform the heuristic composite score on a held-out evaluation set. At that point, the product's own measurements are doing more work than the hand-coded scorer. The scorer can be turned off and ranking quality improves. Before that point, the product is still bootstrapping. After it, the product is self-sustaining.

The escape velocity condition is self-reinforcing: better rankings attract more agents. More agents generate more behavioral data. More data improves rankings further. Each completed interaction makes the next one cheaper. This is the flywheel that makes DontGuess a product rather than a feature.

A product with running loops is a resonator — an independent entity that produces amplitude greater than its input. A product without loops is a feature waiting to be absorbed. The loops are not an optimization. They are the product's claim to independent existence.

## What It Is

DontGuess is a tool discovery and ranking engine that learns from every interaction. It crawls, scores, and ranks CLI tools, MCP servers, libraries, APIs, and container images. It serves ranked results to AI agents via MCP and to humans via web UI and CLI.

The critical distinction: DontGuess is not a static index. It is an index with an internal optimization system — three feedback loops that read telemetry, compute behavioral signals, and adapt search behavior without human intervention. The loops are the product. Without them, DontGuess is a keyword search engine that degrades over time as training data catches up.

### The R-R-R Model

The scoring model uses three legible signals:

- **Relevance** — does this tool match the task? Handled by vector search.
- **Resonance** — does this tool actually collapse the agent's branching factor when used? Bootstrapped from heuristic scores, calibrated over time by observed agent behavior.
- **Reach** — can the agent use this tool right now? Local availability, install friction, platform compatibility.

The eight existing scoring dimensions (token efficiency, schema quality, single-purpose, determinism, documentation, freshness, text-nativeness, opinionatedness) are heuristic proxies for Resonance. They bootstrap the system until observed usage data can replace them. All eight measure aspects of branching factor collapse — whether the tool narrows the agent's space of possible next actions from many to one.

The heuristic scorer is the hand-coded chess engine. Observed resonance data at scale is Deep Blue. The correct strategy is building the measurement infrastructure that eventually makes the heuristics unnecessary.

The R-R-R weights are configuration surfaces that the medium loop can adjust based on accumulated evidence. They are not hardcoded constants — they are parameters that the product's internal optimization system tunes. If the weights turn out to be wrong, the product must be able to discover that and adjust.

### The Product Is the Three Loops

RPT Section 1.7: "These loops run inside the product, not in external infrastructure." The loops are not operations supporting the product. They are the product. Data flows in through telemetry, gets processed by internal loops, and flows back out as search behavior changes. A product with one running loop is categorically different from a product with zero loops.

## The Three Edges

DontGuess serves each edge of the RPT triangle:

### AI-App Edge (primary): Branching Factor Collapse

The MCP interface. An agent describes a task; DontGuess returns ranked tools with install instructions, confidence signals, and reach information. The interface is designed so that interaction becomes recognition, not reasoning. One tool call, one response, one decision. The agent's branching factor collapses from "which of thousands of tools should I use?" to "use this one."

Five measurable signals at this edge, all observable without understanding the agent's internal mechanism:

| Signal | What it measures | Direction |
|--------|-----------------|-----------|
| Token count per search session | Total cost of finding a tool | Lower = resonating |
| Retry rate | Failed recognitions (reformulated queries) | Lower = resonating |
| Hedging frequency | Agent uncertainty about results | Lower = resonating |
| Decision latency | Time/tokens between receiving results and acting | Lower = resonating |
| Error recovery cost | Cost of handling search failures | Lower = resonating |

These signals are measured from observed session behavior (reformulation patterns, query-to-silence ratios), not from proxy heuristics on search result text.

### Human-App Edge: Browsable Intelligence

The web UI and CLI. Humans browse, audit, and curate tool rankings. The interface shows what agents are actually using and succeeding with: "jq: recommended 47 times for JSON parsing, 94% success rate." Human curation flows back as a signal that improves agent results. The human sees the product of agent discovery; the agent benefits from human judgment. Same data, rendered for each audience's native comprehension mode.

### Human-AI Edge: Structurally Unreliable

The communication between human and agent that happens outside DontGuess's observation. The agent explains its tool choices to the human. The human instructs the agent on tool preferences. DontGuess cannot observe or instrument this edge — it is structurally unreliable per RPT. Design constraint: every interface must serve either user arriving with full authority, without knowing which one is in the chair or what conversation preceded their arrival.

## The Competitive Moat

### The Window

Anthropic controls MCP. They invented the protocol. They are actively building MCP registries. They have distribution — every Claude user is a potential DontGuess user, except Anthropic doesn't need DontGuess because they can build tool search into Claude natively. The question is not IF but WHEN. Estimate: 12-18 months.

Google already indexes npm, PyPI, crates.io, GitHub. They already rank by usage signals. Adding an MCP endpoint to Google's package search is a weekend project for a Google team. Microsoft has Copilot, GitHub's developer graph, and distribution through VS Code.

DontGuess has one developer, $200/month infrastructure, and zero distribution.

### Three Permanently Un-Nullable Deltas

These three deltas are structurally absent from training data — not temporarily absent, permanently absent. They survive indefinitely regardless of how good models become:

**1. Real-time freshness.** Training data is a snapshot. A tool that broke yesterday, a package deprecated last week, a new release that changes the recommendation — training data misses all of these. DontGuess crawls weekly. The delta is permanent because training pipelines operate on 6-12 month cycles. DontGuess's fast loop operates on 15-minute cycles. This gap does not close with better models.

**2. Cross-model convergence data.** Anthropic sees what Claude users do. OpenAI sees what GPT users do. Neither sees the other. DontGuess sits outside any single model. It can observe what tools work across Claude, GPT, Gemini, Llama, Mistral, and every future model. When Claude uses 200 tokens with ruff, GPT uses 250, and Gemini uses 180, that's a signal no single provider can generate. The inter-model edge is dark to each provider individually. DontGuess is the only entity that can observe across the dark edges between models.

The moat is not data volume. It is data topology. DontGuess observes a cross-section of the ecosystem that no single provider can access because providers are structurally blind to each other's interactions. This is a permanent structural advantage, not a temporary lead.

**3. Local environment context.** Training data cannot know what is installed on THIS machine. `toolrank_find(installed_only=true)` provides this. This delta is permanent and immediately valuable — no bootstrap data required.

### Additional Durable Deltas

Beyond the three permanent deltas, two more require the loops to be running:

**4. Tool quality regressions.** Training data captures a tool's quality at training time. If a tool breaks in a new version, training data still recommends it. DontGuess detects the regression in real time (agents start failing with tool X after version 3.2) and adjusts rankings immediately.

**5. Task-specific fitness.** Training data knows "jq is a JSON processor." DontGuess knows "jq takes 200 tokens for field extraction but 800 tokens for complex transformations — for complex transformations, use `gojq` instead." This granularity requires measuring outcomes across many task variants — data that training pipelines structurally cannot have.

### What Beats Google and Anthropic

The ONLY structural advantage a small player has over platforms: DontGuess can be platform-neutral. Anthropic's tool recommendations will favor Anthropic's ecosystem. Google's will favor Google's. Microsoft's will favor Microsoft's. DontGuess is the neutral arbiter — the tool discovery service not trying to lock agents into a platform.

Neutrality alone is a weak moat. It requires users to care about neutrality more than convenience, and history says they usually don't. The neutrality argument only matters if DontGuess achieves enough scale that the neutrality is valued. At 50 users, nobody cares. At 50,000 users, it's the thing that stops platform lock-in.

The combined position — neutral, open-protocol, cross-platform — is structurally unavailable to any company that IS a platform. This is the same reason the Linux Foundation runs Kubernetes instead of Google running it directly.

### Scenarios Where DontGuess Loses

Honesty requires documenting the kill scenarios:

**Anthropic ships native tool search.** Pre-installed in Claude. Uses their own convergence data from millions of users. DontGuess's few hundred users see the native feature is good enough. Adoption stalls. The convergence flywheel never spins up. Game over.

**Model improvement absorbs DontGuess's static knowledge.** Every 6-12 months, a new model generation absorbs more tool knowledge into training data. DontGuess's value concentrates in the long tail and the real-time edge. The majority of queries — "what's a good Python linter?" — will be answered better by training data within 2-3 model generations. DontGuess becomes "the specialist search engine for cases where training data is insufficient." That's defensible but it's not "the Google of AI tool discovery."

**Multi-model adoption never materializes.** The cross-model convergence moat requires GPT-4, Gemini, and open-source model users to adopt DontGuess. The CLAUDE.md viral vector does not work for non-Claude users. If DontGuess remains Claude-only, its convergence data is a subset of what Anthropic already has. The moat evaporates.

**The bootstrap flywheel does not spin.** Infrastructure ships late. Agents don't engage with the tool ecosystem during bootstrap sessions. Behavioral data is thin. No convergence data, no moat, static index that degrades. The product is a feature or an acquisition target, not a product.

The mitigation for all four: the fast loop determines the outcome. If the loop runs and accumulates cross-model behavioral data before the window closes, DontGuess survives. If it doesn't, nothing else matters.

## The Bootstrap Loop

### Agents ARE the Users

The agents that develop and test DontGuess are the product's users. This is not a philosophical observation — it is an RPT structural property. RPT Section 1.7 defines the quality feedback loop as "a development-side loop with the same resonance structure as the three product loops." The development process IS a resonance loop. Not analogous to one. IS one.

Every Claude Code session that uses DontGuess is a production session generating real usage data:
- Baron (human) directs a coding task
- Claude (agent) calls toolrank_find to discover a tool
- DontGuess (product) returns results
- Claude uses the tool, or doesn't
- The session produces behavioral traces: what was searched, what was found, what was used, how many tokens the task consumed

This is a real K3 tuple. The development session IS the first production deployment. The cold-start problem does not exist if the developers are the users. We have warm-start by default.

### Behavioral vs. Preference Signals

Not all signals from agent sessions are equal. A critical distinction:

**Preference signals** ("which tool did the agent choose?") are echo-chambered when all agents share training data. When Claude picks ruff for Python linting, that may be training data expressing itself through a search interface. 1,000 Claude sessions converging on ruff is n=1 for preferences, not n=1,000.

**Behavioral signals** ("how many tokens did the task cost after the choice?") are partially genuine even from a monoculture. When Claude uses ruff in 200 tokens and pylint in 800 tokens, the 4x difference measures the tool's actual fitness for AI agents. Token cost, error rate, installation friction, retry count — these are measurements of what HAPPENS, not what the model THINKS.

**The tool-familiarity confound.** Claude "knows" jq from training data and can invoke it efficiently. An obscure tool released last month requires more tokens to use regardless of quality. Token cost is a function of (tool quality) x (model familiarity). Single-model behavioral data is biased, not clean. The bias is toward established tools — conservative, not destructive, but real.

**Resolution:** The fast loop should weight behavioral measurements (token cost, error rate, retries) heavily and preference votes (which tool was selected) near zero until multi-model data arrives. Cross-model deconfounding is the full fix: if Claude, GPT, and Gemini all show similar token costs with a tool, that signal is genuine.

### Campaign Types

Structured campaigns targeting different signal types:

**Campaign 1: Production Operator (highest-value behavioral signals).** Week 1.
Agents install and USE tools. Install top-5 results for "python linter" and lint the same codebase with each. Install top-3 "http load testing" tools and benchmark the same endpoint. Does the tool ACTUALLY WORK when an agent tries to use it? Full lifecycle: search -> select -> install -> use -> outcome.

**Campaign 2: Polyglot Builder (tool discovery breadth).** Week 2.
Build small projects across 10+ languages and domains. Each session generates 3-8 tool discoveries, 1-2 comparisons, 0-1 failed discoveries. 8 domains x 5 sessions = 40 sessions = 120-320 discovery signals.

**Campaign 3: Greenfield Explorer (coverage gap discovery).** Week 2-3.
Deliberately probe domains where the index is likely thin: embedded systems, scientific computing, audio/video processing, game development. These reveal where DontGuess fails.

**Campaign 4: Dependency Upgrader (tool comparison depth).** Week 3.
Review existing projects and evaluate whether current tools should be replaced. "Is there a faster alternative to requests?" "Is black still the best Python formatter?" Head-to-head comparison signals — the most valuable type for ranking.

**Tasks must come from real work, not from the index.** Generating tasks where jq is the known-good answer and then measuring whether agents find jq is validating our own priors, not discovering tool quality. Bootstrap tasks should be derived from actual codebases — real Cargo.toml files that need a new dependency, real CI pipelines that need a new tool, real data files that need transformation.

Index-derived tasks are acceptable ONLY for testing search pipeline mechanics (does the API work? do results load?), not for generating ranking signals.

### Echo Chamber Mitigation

Three strategies to counteract the single-model bias:

**1. Vary system prompts.** Each session gets a different persona — junior developer, senior developer migrating languages, DevOps engineer, security engineer, performance engineer. Different prompts produce genuinely different selection criteria.

**2. Inject randomized constraints.** "Must work offline." "Must have <10 dependencies." "Must support Windows." "Must be MIT-licensed." Constraints force different tool selections, creating diversity from a monoculture.

**3. Multi-model sessions in Week 4.** Run the same campaigns through GPT-4, Gemini, and open-source models. Different models have different training biases. Convergence across models is a strong signal. Divergence reveals model-specific biases. This is necessary, not optional.

The multi-model requirement does not require MCP support from every provider. A simple harness wraps the experiment: model receives a task description, receives the toolrank API spec as context, and makes tool selections. Token costs and outcomes are captured regardless of protocol.

### 4-Week Cadence

- **Week 1**: 40 install-and-use sessions (Campaign 1). Generates ~200 behavioral signals. Fast loop starts on this data.
- **Week 2**: 40 mixed sessions (Campaigns 2+3). Generates ~300 signals. Enough to start computing meaningful affinity boosts.
- **Week 3**: 20 comparison sessions (Campaign 4) + 20 explorer sessions. First A/B measurement: do boosts outperform heuristic scorer?
- **Week 4**: Multi-model sessions begin. 10 sessions x 4 models = 40 sessions generating cross-model convergence data. The moat starts forming.

By end of month 1: ~500 sessions, ~1000 behavioral signals, ~40 cross-model convergence points. Enough to: replace bottom scorer dimensions with observed data, identify and fill top 20 coverage gaps, and measure whether the bootstrap flywheel is actually spinning.

### Every Development Session Is a Production Session

The MCP server should be configured in our own Claude Code environment first. Not as a test. As real usage. Every session where Claude searches for a tool, every session where Claude uses `toolrank_find` and either succeeds or fails — that's real data flowing into the fast loop. The development environment IS the first production deployment.

The telemetry pipeline does not distinguish between bootstrap campaigns and organic usage. The fast loop processes all of it identically. The bootstrap accelerates the flywheel; organic usage sustains it.

## Distribution Strategy

### The CLAUDE.md Viral Vector

Every project that uses Claude Code has a CLAUDE.md file. CLAUDE.md files are instructions that agents read at session start. If a CLAUDE.md says "use toolrank_find to discover tools before implementing from scratch," the agent will do it.

The distribution mechanism is unique to AI-native products: configuration files, not marketing. The viral loop:

1. Developer A adds DontGuess to their project's CLAUDE.md
2. Developer B forks the project (or copies the CLAUDE.md pattern)
3. Developer B's agent now uses DontGuess
4. Developer B has a good experience, adds it to their other projects
5. Those projects get forked...

This is how ESLint configs spread (everyone copies the Airbnb config). This is how Docker Compose patterns spread. DontGuess spreads the same way — through conventions in configuration files.

The cost of this distribution: zero. The enabler: a hosted HTTP endpoint or a locally installed MCP server.

### The `toolrank install` Self-Configuration Command

One command that:
1. Detects whether Claude Code is installed
2. Finds the settings file path
3. Merges the DontGuess MCP server entry without clobbering existing config
4. Downloads the seed database (~50-100MB compressed SQLite)
5. Writes the MCP server configuration to Claude Code settings

After `toolrank install`, the agent has DontGuess permanently. The self-configuration approach is the strongest distribution play for individual adoption.

### Registry Publishing

- **Smithery**: Submit JSON manifest to the registry. Requires a public URL or npm-published package. DontGuess needs a hosted endpoint or a `toolrank-mcp` npm wrapper that downloads the seed database on first run.
- **pip/brew**: pyproject.toml already defines `toolrank-mcp` entry point. Needs a `--bootstrap` command that downloads the seed database from a CDN URL.
- **MCP official registry**: List alongside other MCP servers.

### Distribution Sequencing

The distribution window opens when observed behavioral data outperforms heuristic scores on a held-out evaluation set. That is the gate. Premature distribution means users arrive at a product that isn't yet better than the null product for common tools, form an opinion, and leave. That's worse than no distribution.

**Month 1**: Our own CLAUDE.md files. Self-configuration command. Internal dogfooding. Every development session generates real signals.

**Month 2**: Open-source CLAUDE.md contributions to 50 popular projects. Smithery listing. This is the "Google getting default search bar" move — except instead of paying browser makers, we contribute genuinely useful configuration files.

**Month 3**: Cursor/Continue/Windsurf MCP templates. Blog post on "tokens saved per session" with real measurements from the bootstrap campaigns.

**Gate**: Do not go public until the A/B measurement confirms the product beats the null product. Measurement, not feeling.

### The Wedge Use Case

"Find me a tool" is generic. The specific, irresistible wedge:

**"What should I use instead of writing this from scratch?"**

This isn't search. It's anti-NIH intervention. The agent is ABOUT to write 500 lines of code. DontGuess says "stop. Use X. Here's why, here's how, here's the install command." This is the highest-value moment because the token savings are massive and visible.

The metric that proves the wedge: **tokens saved per session.** If agents using DontGuess consistently use 15-20% fewer tokens per coding session because they use existing tools instead of writing from scratch, that number makes DontGuess un-nullable.

## Architecture

### Three Data Layers

1. **Static Index** (what exists): 2.4M tools from 13 registries, pre-scored on 8 heuristic dimensions, pre-embedded as 384-dimensional vectors, ranked by DontGuess graph algorithm (eigenvector centrality on 18.6M dependency edges). Rebuilt weekly.

2. **Dynamic Signals** (what works): Query-tool affinity scores, reformulation patterns, convergence evidence, coverage gaps. Updated by the fast loop every 15 minutes. This layer is what makes DontGuess adaptive rather than static.

3. **Local Profile** (what you have): Per-agent capability profile built from `scan_local` results. Lists installed tools with categorization. Updated on agent connect and when the local environment changes. Serves the common case ("use what you have") with zero search latency.

### Search Pipeline: Two Paths

**Path A — Discovery** ("I need something new"):
```
query → name-exact pre-filter (O(1) DB lookup)
      → vector search (cosine similarity, top 50, <50ms)
      → blend with dynamic affinity scores
      → re-rank by composite score (0.7 search + 0.3 quality)
      → filter by reach (installed > installable > unavailable)
      → return top K
```

No BM25. The Whoosh BM25 index produced 6-57 second latency on the 1.7GB segment file, which is anti-resonance — the agent's branching factor widens while it waits. Vector search via ONNX all-MiniLM-L6-v2 returns results in under 50ms. Exact-name lookup handles the case where the agent knows the tool name. Synonym-style expansion is applied to the query string before embedding to improve recall for domain-specific terminology.

**Path B — Awareness** ("what should I use for this task?"):
```
task description → match against local profile → return pre-computed recommendation
```

Zero search latency. The local profile is already available to the agent. This handles the 80% case where the agent needs a tool it already has installed.

### Infrastructure Requirements (Round 2)

**HTTP/SSE MCP transport.** The current MCP server runs over stdio — one process per agent, one stdin/stdout pipe. To serve concurrent bootstrap sessions, HTTP/SSE transport is required. Each POST to `/mcp` gets its own server instance. Uvicorn already runs the REST API; the MCP server can run on the same instance. This unlocks multi-agent bootstrap sessions and the OpenAI function-calling adapter.

**SQLite WAL for telemetry.** The current JSONL telemetry (`telemetry.py:275`) opens and appends without locking. At bootstrap concurrency levels, this corrupts data. SQLite WAL-mode handles hundreds of concurrent writers without blocking reads. Schema: (timestamp, session_id, query, actor_type, intent, domain, result_count, result_ids, latency_ms, is_retry, model_id).

**model_id in telemetry.** `_detect_actor_type()` reads `_meta.clientInfo.name` but logs it as "agent"/"human"/"unknown" — not the actual model. Adding `model_id` captures the cross-model convergence data that is DontGuess's structural advantage. Two lines of code that seed the moat.

### Discovery Sources

**Local** (scanned per-agent):
- PATH executables, man pages, `--help` output
- System package managers (apt, brew, pacman)
- Installed MCP server configs
- Local dependency graphs

**Remote** (crawled by service):
- Code forges: GitHub, GitLab
- Package registries: npm, PyPI, crates.io, Homebrew, apt, RubyGems, NuGet, Chocolatey
- Container registries: Docker Hub, GHCR
- MCP registries: Smithery, official MCP registry, Glama
- `.well-known/mcp/server.json` auto-discovery
- System package repository metadata

**Crawl cadence:**
- Full re-crawl: weekly (slow loop)
- Incremental: daily for high-signal sources (medium loop)
- Targeted: triggered by persistent coverage gaps identified by the fast loop

## The Scoring Model

### Heuristic Dimensions (Bootstrap)

Eight dimensions scored from documentation text at index time. Pure keyword heuristic — no LLM calls, no actual token measurement. These are proxies for resonance, not measurements of it.

| Dimension | Weight | What it approximates |
|-----------|--------|---------------------|
| Token efficiency | 0.30 | Does this tool replace expensive inference with cheap computation? |
| Text nativeness | 0.15 | Does the tool work with text I/O — the agent's native modality? |
| Determinism | 0.15 | Same input, same output — no retry branch? |
| Schema quality | 0.10 | Predictable contracts that collapse the "what format?" branch? |
| Documentation | 0.10 | Can the agent predict behavior without trying it? |
| Single-purpose | 0.10 | One tool, one job, one choice? |
| Opinionatedness | 0.05 | Constraints that narrow the possibility space? |
| Freshness | 0.05 | Is the tool maintained and current? |

**Known limitations of the heuristic scorer:**
- Scores are computed from keyword presence in documentation text. A tool whose README says "this tool does NOT search" scores as a hit for "search" because `_count_matches` does substring matching with no negation handling. The scorer measures documentation vocabulary overlap with a hardcoded word list, not actual tool quality.
- The composite score weights are hardcoded. There is no calibration data and no ground truth for what a "good" score means.
- `DEFAULT_FRESHNESS = 0.5` is applied to most tools because the scorer does not consume crawl timestamps.
- A tool author who reads the scorer source can optimize their README to score >0.8 composite by including the right keywords. The scoring model is gameable by design inspection.

These limitations are accepted as bootstrap debt. The path to resolution is calibrating weights against observed usage data from the feedback loops. The heuristic scorer is necessary now; it must not be permanent.

### Graph Quality (DontGuess Algorithm)

Eigenvector centrality on a dependency graph with 18.6M edges. A tool's graph score reflects how many other tools depend on it, weighted recursively by the quality of those dependents (PageRank-style). Computed offline, cached at `data/graph-cache.npz`. Contributes 40% of the merged score (`DEFAULT_GRAPH_WEIGHT = 0.4`) via power-law normalization (fifth root).

Edge sources: dependency files (package.json, requirements.txt, Dockerfile, Homebrew formulas), co-occurrence in configs, wrapper relationships, script invocations, composition patterns.

### Observed Resonance (Target State)

The target scoring model replaces heuristic weights with weights learned from observed agent behavior:

- **Query-tool affinity scores**: When multiple independent agents converge on the same tool for similar queries, that tool gets an affinity boost for those query patterns. Stored in `affinity_boosts.jsonl`, hot-reloaded by the search pipeline.
- **Reformulation depth as negative signal**: Tools in results consistently followed by reformulated queries get demoted.
- **Query-to-silence as positive signal**: Agent searches, receives results, stops searching — top result gets a positive signal.
- **Cross-agent convergence**: Three or more independent agents selecting the same tool for semantically similar queries is the strongest quality signal.
- **Token cost weighting**: Behavioral measurements (token cost, error rate) are weighted 10x over bare success/failure votes. The fast loop privileges what HAPPENS over what agents THINK.

## The Three Loops

The optimization system lives inside the product, not in external infrastructure.

### Fast Loop (every 15 minutes)

**What it reads:** SQLite telemetry store (session queries, results, latencies, retries, model_id)

**What it computes:**
- Groups queries by session_id, orders by timestamp
- Detects reformulation chains (same session, semantically similar queries within 60 seconds)
- Computes query-to-silence ratio (search followed by no more searches = positive signal for top result)
- Identifies zero-result query clusters (coverage gaps)
- Computes cross-agent convergence (multiple independent agents selecting the same tool for similar queries)

**What it writes:**
- `affinity_boosts.jsonl` — query-tool affinity scores derived from behavioral signals
- `coverage_gaps.jsonl` — query clusters with persistently zero or low-quality results

**How the product consumes it:** The MCP server hot-reloads affinity boosts on the next query. No restart, no redeployment, no reindexing.

**Circuit breaker:** Affinity boosts are provisional for 24 hours. If the boosted tool's query-to-silence ratio does not improve within 24 hours, the boost is rolled back. Automated adaptation without ground truth is automated drift — the circuit breaker prevents this.

**Configuration surface:** Convergence threshold, boost magnitude, rollback sensitivity, update cadence. These parameters are themselves subject to medium-loop tuning.

**Instrumentation triple:**
- Capability: the fast loop adapts search behavior
- Instrumentation: did the adaptation improve query-to-silence ratio? A/B comparison of rankings with vs. without boosts.
- Configuration surface: convergence threshold, boost magnitude, rollback sensitivity

### Medium Loop (daily)

**What it reads:** Accumulated affinity boosts, coverage gaps, usage patterns

**What it does:**
- Aggregates fast-loop outputs across days
- Identifies persistent coverage gaps (>3 days of zero-result queries in a domain) and triggers targeted crawls
- Promotes affinity boosts with sufficient convergence evidence to permanent scoring adjustments
- Recomputes local profiles for returning agents
- Adjusts scorer weights based on which heuristic dimensions best predict tools that agents actually use
- Adjusts fast-loop aggression parameters if the circuit breaker fires too often or not enough

**What it writes:**
- Updated `scorer_weights.json` (read by scorer at query time)
- Crawl job queue for underserved domains
- Promoted query-tool boosts (only when convergence evidence is strong: 3+ independent agents, improved outcomes)

**Instrumentation triple:**
- Capability: adjusts scoring weights and triggers crawls
- Instrumentation: are persistent coverage gaps shrinking? Are weight changes improving resonance metrics?
- Configuration surface: promotion thresholds, crawl budget, weight adjustment rate

### Slow Loop (weekly)

**What it does:**
- Full re-crawl of all 13 registries
- Full re-index (vector embeddings, graph computation)
- Generates resonance dashboard: resonance signal trends, underserved domains, loop effectiveness (are fast-loop adjustments improving medium-loop metrics?), agent return rate
- Human reviews dashboard and makes strategic decisions

This is the one loop that stays human-facing. The fast and medium loops are automated. The slow loop produces evidence for human judgment: which edges are underserved, where relay costs are too high, whether the product is actually resonating or just two interfaces sharing a database.

**Instrumentation triple:**
- Capability: rebuilds full index, produces strategic analysis
- Instrumentation: is the product improving week-over-week on resonance metrics?
- Configuration surface: crawl sources, registry priorities, loop cadence

## The MCP Interface

### Single Tool Design

One MCP tool: `toolrank_find`. This collapses the agent's interface branching factor from "choose between search, score, recommend, and scan_local" to "call or don't call." The agent's decision is binary.

The current four tools (search, score, recommend, scan_local) violate RPT's branching factor collapse principle. `toolrank_recommend` is literally `_search_tools(context, ..., limit=5)` — the same function as search with a different limit. Two tools that do the same thing force the agent to reason about which to call. That reasoning is pure waste.

```
toolrank_find:
  task: string        # What the agent needs to accomplish
  options:
    limit: int        # Max results (default 10)
    type: string      # Filter by tool type (cli, library, mcp-server)
    ecosystem: string # Filter by ecosystem (pypi, npm, local)
    installed_only: bool  # Only return locally installed tools
```

`scan_local` becomes `toolrank_find(task="*", installed_only=true)`. Scoring details are returned inline with search results.

### HTTP/SSE Transport Requirement

The stdio transport (current) serves one agent per process. Bootstrap campaigns and production usage require concurrent sessions. The MCP server must support HTTP/SSE transport:
- ASGI endpoint on the existing uvicorn server
- Each POST to `/mcp` gets its own `ToolRankMCPServer` instance
- Claude Code MCP config points to `http://localhost:8080/mcp`
- Same endpoint serves the OpenAI function-calling adapter (50-line shim translating function calls to JSON-RPC)

This is the prerequisite for both multi-agent bootstrap sessions and multi-model adoption.

### Response Format

Compact by default. Optimized for agent consumption — structured, parseable, actionable.

```json
{
  "results": [
    {
      "id": "brew/jq",
      "install": "brew install jq",
      "score": 0.94,
      "reach": "installed",
      "efficiency": {"token_ratio": 1200, "reliability": "deterministic"},
      "confidence": "convergence",
      "install_methods": [
        {"source": "brew", "command": "brew install jq"},
        {"source": "apt", "command": "apt install jq"}
      ]
    }
  ],
  "meta": {
    "query_tokens": 5,
    "response_tokens": 48,
    "index_coverage": "good"
  }
}
```

**Confidence as result count, not as a field.** When DontGuess is confident, it returns 5+ tightly ranked results. When uncertain, it returns 1-2. The result count IS the confidence signal. The agent sees the quantity and recognizes the coverage level without parsing metadata. This is recognition, not reasoning.

The `confidence` field ("convergence", "heuristic_only", "local_match") is included for agents that want it. It distinguishes results backed by observed cross-agent behavior from results backed only by documentation heuristics. Honesty about what DontGuess knows versus what it guesses is itself a branching factor collapse mechanism — the agent doesn't have to second-guess the recommendation.

**Cross-source dedup.** Results are grouped by canonical tool name. `brew/jq` and `apt/jq` appear as one result with multiple install methods, not as duplicate result slots.

## Desire Path Architecture

When agents search in ways DontGuess did not anticipate, that is signal, not noise.

### Implicit Feedback Signals

DontGuess observes agent behavior through session-level query patterns. No explicit reporting endpoint required. No protocol changes needed.

| Signal | How observed | What it means |
|--------|-------------|---------------|
| Query-to-silence | Agent searches, receives results, stops searching | Top result likely satisfied the need (positive signal) |
| Reformulation depth | Agent searches, reformulates, searches again | First results were inadequate (negative signal) |
| Convergence | 3+ independent agents select same tool for similar queries | Tool genuinely solves this class of problem (strongest signal) |
| Return rate | Same agent uses DontGuess in a new session | Product is useful (system-level positive signal) |
| Zero-result clusters | Multiple queries in a domain return nothing | Coverage gap requiring crawl or index expansion |

These signals cannot be gamed by tool authors because they require controlling multiple independent agents' behavior across different sessions. Stars can be bought. Dependency graphs can be inflated. Making 10 unrelated agents independently converge on your tool for the same query pattern cannot be faked.

### Hallucination-as-Desire-Path

When an agent calls `toolrank_find` with a query that assumes an interface DontGuess does not have, that is the agent telling DontGuess what it expected:

- "alternative to curl" — the agent expected a comparison/alternatives feature
- "best security tool for small cloud environments" — the agent expected taxonomy-based filtering
- "tool that works with jq output" — the agent expected composition-aware search

The fast loop detects structured query patterns and clusters them. When a cluster reaches threshold, it becomes a feature candidate or a coverage gap.

### Explicit Usage Reporting (Optional)

An optional `toolrank_report_usage` mechanism exists for agents that support it:

```
toolrank_report_usage:
  tool_id: string
  query: string
  outcome: "success" | "failure" | "abandoned"
  tokens_spent: int (required — the honest behavioral signal)
  session_id: string
```

`tokens_spent` is required because a report without token evidence is just a preference; a report with token evidence is a measurement. The fast loop weights token-evidence reports 10x over bare success/failure. Self-reported metrics beyond token count are excluded — an attacker can create agents that report fabricated savings, but cannot fake the actual session token consumption observable in telemetry.

## Adversarial Resistance

### Resolved Threats

**BM25 latency (eliminated).** Vector-only search with name-exact pre-filter. Sub-100ms guaranteed. The 6-57 second BM25 bottleneck is removed from the query path entirely.

**MCP interface branching factor (collapsed).** Four tools reduced to one (`toolrank_find`). The agent's decision is binary.

**Duplicate search/recommend tools (merged).** `toolrank_recommend` was `search(limit=5)`. Merged into `toolrank_find` with a limit parameter.

### Defended Threats

**Scorer gaming via README keyword stuffing.** The heuristic scorer is gameable by design inspection. Defense: the scorer is the bootstrap, not the endpoint. As the fast loop accumulates behavioral data (which tools agents actually succeed with), affinity scores replace heuristic scores as the primary ranking signal. Gaming the README does not make agents use your tool. During the bootstrap phase, this threat is accepted as debt.

**Self-reported metric gaming.** Only implicit behavioral signals (reformulation patterns, convergence, query-to-silence) and token-cost-weighted usage reports are used for ranking. These require controlling multiple independent agents' sessions to manipulate.

**Garbage synonym promotion.** The fast loop produces query-tool affinity boosts, not synonyms. The distinction is critical: synonyms expand the query vocabulary (risky — "linter = code checker = ruff" is noise); boosts promote specific tools for specific query patterns (validated by outcome). Boosts require convergence evidence (3+ independent agents) and a 24-hour provisional period with automated rollback.

**Echo chamber in bootstrap data.** Preference signals (which tool was selected) are weighted near-zero from single-model sessions. Behavioral signals (token cost, error rate) are weighted heavily. Multi-model sessions in Week 4 deconfound the tool-familiarity bias. The A/B instrumentation on the bootstrap loop measures whether boosts outperform the heuristic scorer — if they don't, the echo chamber attack is confirmed and the bootstrap design must change.

**Model improvement race.** Models get better at tool discovery with every generation. Defense: focus on the five data types that training data structurally cannot have (real-time freshness, cross-model convergence, local context, quality regressions, task-specific fitness). Everything else — "which tool is best for JSON parsing" — will be absorbed into training data within 12 months. Accept the narrower permanent value proposition.

### Unresolved Threats (Known Gaps)

**Graph gaming via dependency inflation.** The DontGuess graph algorithm uses eigenvector centrality on an unvalidated dependency graph. An attacker can create 100+ npm packages that each depend on their tool. With `DEFAULT_GRAPH_WEIGHT = 0.4`, 40% of the merged score comes from this undefended signal. `SparseToolGraph.from_edge_files()` does zero edge validation; line 295 overwrites edge weights with `np.ones()`.

Planned defenses (not yet implemented):
1. Filter edges from packages with <5 downloads. Medium effort.
2. Age-weight edges: `edge_weight = min(1.0, days_since_published / 90)`. Low effort.
3. Publisher trust scoring. High effort — deferred.

Indirect defense: once the fast loop produces affinity scores from observed behavior, these scores supersede the gameable graph signal over time.

**Freshness scoring is unimplemented.** `DEFAULT_FRESHNESS = 0.5` is hardcoded. Deprecated packages retain all dependency edges indefinitely.

**Embedding model monoculture.** All vector search depends on all-MiniLM-L6-v2, a general-purpose sentence embedding model not trained on tool descriptions. For domain-specific queries, semantic connections are unreliable. A fine-tuned model would perform better, but the training data to fine-tune does not exist yet — it comes from the same usage data the loops produce.

**Scaling limits.** vectors.npy is 3.5GB for 2.4M tools. At 20M tools: 35GB. ANN indexes needed above ~5M tools.

**Production deployment is currently broken.** vectors.npy is not mounted in docker-compose.yml. Production falls back to BM25-only. P0 fixes this.

## Implementation Priorities

Revised sequencing from Round 2. The fast loop is the existential priority. Everything before it clears the runway for it to run.

### P0: SQLite Telemetry (2 hours)

Replace JSONL append in telemetry.py with SQLite WAL insert. Schema: (timestamp, session_id, query, actor_type, intent, domain, result_count, result_ids, latency_ms, is_retry, model_id). Concurrent-safe. Queryable. Unlocks all concurrent bootstrap scenarios. Fix docker-compose.yml mounts at the same time (5 minutes).

### P1: HTTP/SSE MCP Transport (4-6 hours)

ASGI endpoint on the existing uvicorn server. Each POST to `/mcp` creates a ToolRankMCPServer instance. Claude Code MCP config points to `http://localhost:8080/mcp`. Prerequisite for multi-agent bootstrap sessions and the OpenAI function-calling adapter.

### P2: Vector-Only Search (4-6 hours)

- `search.py`: add `fast_search()` — name-exact DB lookup + `VectorIndex.search(query, limit=50)` + composite re-rank
- `mcp_server.py`: call `fast_search()` instead of `hybrid_search()` for MCP queries
- Keep `hybrid_search()` for CLI and pipeline use
- Apply synonym-style query expansion before embedding for domain recall

### P3: Single MCP Tool (30 minutes)

- Remove `toolrank_recommend` from TOOL_SCHEMAS
- Rename/consolidate to `toolrank_find` with `task` parameter and optional `installed_only` flag
- `toolrank_score` and `toolrank_scan_local` become parameter variations, not separate tools

### P4: Fast Loop Daemon (1 day)

- New file: `src/toolrank/fast_loop.py`
- Reads SQLite telemetry
- Groups queries by session_id, detects reformulation chains
- Computes query-tool affinity scores from behavioral signals (token-cost-weighted)
- Writes `affinity_boosts.jsonl` (hot-reloaded by search pipeline)
- Writes `coverage_gaps.jsonl`
- 24-hour provisional period with automated rollback
- A/B instrumentation: compare rankings with vs. without boosts on held-out queries

### P5: Bootstrap Session Infrastructure (4 hours)

- Bootstrap task corpus: 200 tasks across 8 domains, derived from real work (not index-derived)
- Session runner: spawn N agent subprocesses, assign tasks, collect telemetry
- Model_id capture in telemetry for cross-model analysis

### P6: Bootstrap Campaigns at Scale (~2 weeks)

- Week 1: 40 install-and-use sessions, Claude-only
- Week 2: 40 mixed sessions
- Week 3: 20 comparison + 20 explorer sessions. Gate check: do boosts beat heuristic scorer?
- Week 4: Multi-model sessions via OpenAI adapter

### P7: Graph Gaming Defense (4-6 hours)

- Download count filtering for npm/PyPI edges
- Age-weighting in `SparseToolGraph.from_edge_files()`
- Modify code that overwrites edge weights with `np.ones()` to respect actual weights

### P8: Freshness Scoring (2-4 hours)

- Scorer consumes crawl timestamps
- Deprecation detection for npm/PyPI
- Deprecated tools get freshness 0.0 and downweighted graph edges

### P9: Medium Loop (1-2 days)

- Aggregates fast-loop outputs
- Triggers targeted crawls for persistent coverage gaps
- Scorer weight recalibration from observed data
- Writes `scorer_weights.json`

### P10: Slow Loop and Resonance Dashboard (2-3 days)

- Full re-crawl orchestration
- Weekly resonance report
- Human-facing dashboard for strategic product decisions

### Deferred

- **Cross-agent learning table** (`usage_patterns`): Requires weeks of fast-loop data. ~200 lines of Python.
- **Pre-computed capability profiles**: Requires medium loop producing domain-specific recommendations.
- **Push-based proactive recommendations**: Correct RPT architecture but blocked by MCP protocol — no push notifications from server to client today.
- **Composition intelligence**: "curl | jq" as a recommendation. Requires massive observed composition data. The bootstrap loop captures composition patterns as a byproduct — we store the data now, build the recommendation engine later. Year-1 moonshot.
- **Tool metadata standard** (toolrank.json): Deferred. Standards succeed from market power, not aspiration. Revisit when DontGuess has enough adoption that tool authors care.

## Known Gaps

1. **The heuristic scorer is unfalsifiable.** No calibration data, no ground truth. The `consistency_boost` is effectively noise. Resolution: calibrate against usage data from P4/P6. Timeline: weeks after bootstrap campaigns begin.

2. **Resonance measurement is proxy-based.** The resonance runner measures token counts via whitespace splitting, retry rate via score thresholds, hedging via keyword matching on descriptions. Real resonance measurement comes from the fast loop's behavioral signals. The runner should be repurposed to validate loop outputs.

3. **The graph is fully gameable today.** Zero edge validation. 40% of merged score. P7 addresses cheapest defenses; publisher trust scoring is unsolved.

4. **Vector search accuracy is unmeasured independently.** Need independent vector recall evaluation across query categories.

5. **The embedding model was not trained on tool descriptions.** Domain-adapted model would perform better; training data comes from usage data the loops produce.

6. **Production deployment is currently broken.** Absorbed into P0.

7. **No agent framework calls `toolrank_report_usage` today.** Primary feedback is implicit session signals. Explicit channel provides cleaner data if adopted.

8. **Scale ceiling at ~5M tools.** ANN indexes needed beyond this.

9. **The bootstrap flywheel is unvalidated.** Whether agents will engage with the tool ecosystem during bootstrap sessions is uncertain. Signal quality depends on task design and model disposition. The A/B instrumentation in P4 is the circuit breaker — if boosts don't outperform heuristics, the bootstrap design must change.

10. **Multi-model adoption path is unclear.** The moat requires non-Claude users. HTTP transport enables it technically; distribution to GPT-4/Gemini users has no clear channel beyond organic discovery. The CLAUDE.md vector does not work for non-Claude ecosystems.

## Scope Boundaries

DontGuess is discovery and ranking. Nothing else.

- **Not a package manager.** Does not install tools. Returns install commands.
- **Not a security scanner.** Does not certify safety or audit dependencies.
- **Not an app store.** No hosting, payments, review process, or curation beyond algorithmic ranking.
- **Not a runtime.** Does not execute tools. Tells the agent which tool to execute and how.
- **Not a protocol.** Does not extend MCP. Works within existing MCP capabilities.
- **Not a benchmarking service.** Scores are heuristic or behavioral, never from controlled experiments.
- **Not an open data project.** The convergence data is proprietary and is the core asset. Publishing it gives away the moat to better-resourced competitors. Open data play requires market power first — maybe at 50K users, not 50.
