# Bootstrap Harness — Multi-Model Usage Generation & Spend Control

**Status**: Design draft — pending adversarial review
**Date**: 2026-03-18

## Overview

A harness that orchestrates agent sessions across multiple models to generate real DontGuess usage data. The harness controls spend, monitors signal quality, and self-adjusts. It is itself an RPT loop — the infrastructure optimizes its own cost/signal ratio.

## Layer 2: Multi-Model Harness

### The Goal

Generate cross-model convergence data — the structural moat. When Claude, GPT-4, Gemini, and Llama all converge on the same tool for the same task, that signal is genuine and un-gameable. No single model provider can generate it.

### Architecture

```
                    ┌─────────────────────────────┐
                    │     Campaign Orchestrator    │
                    │  (task queue + session mgr)  │
                    └──────────┬──────────────────┘
                               │
              ┌────────────────┼────────────────┐
              │                │                │
     ┌────────▼──────┐ ┌──────▼───────┐ ┌──────▼───────┐
     │  Claude Code  │ │  OpenCode /  │ │  Raw API     │
     │  (MCP native) │ │  Aider       │ │  (Bedrock)   │
     └────────┬──────┘ └──────┬───────┘ └──────┬───────┘
              │                │                │
              │    ┌───────────▼────────────┐   │
              └───►│   DontGuess MCP/HTTP    │◄──┘
                   │   (localhost:8080)     │
                   └───────────┬────────────┘
                               │
                   ┌───────────▼────────────┐
                   │  SQLite Telemetry      │
                   │  (model_id per query)  │
                   └───────────┬────────────┘
                               │
                   ┌───────────▼────────────┐
                   │  Fast Loop (60s)       │
                   │  → affinity boosts     │
                   │  → cross-model signal  │
                   └────────────────────────┘
```

### Model Adapters

Each model needs a different integration path to DontGuess:

**Claude Code (MCP native)**
- Already configured via .mcp.json
- `toolrank_find` available as an MCP tool
- Telemetry captures model_id from clientInfo
- Zero adapter work needed

**OpenCode / Aider (MCP-capable CLI agents)**
- Configure DontGuess as MCP server in their config
- OpenCode: `.opencode/config.json` with MCP server entry
- Aider: may need a wrapper that exposes DontGuess as a function
- Telemetry captures model_id from their clientInfo

**Raw API via AWS Bedrock (GPT-4, Gemini, Llama, Mistral)**
- No MCP support — needs an adapter
- Adapter pattern: wrap DontGuess's HTTP API as a tool description in the model's function-calling format
- For each session:
  1. System prompt includes DontGuess tool spec (JSON schema)
  2. Model receives task, decides whether to call toolrank_find
  3. Adapter intercepts the function call, POSTs to DontGuess HTTP API
  4. Returns results to model as function response
  5. Model continues with the tool recommendation
- ~100 lines of Python per model family
- Bedrock provides: Claude (Anthropic), Titan (Amazon), Llama (Meta), Mistral, Cohere
- Direct API provides: GPT-4 (OpenAI), Gemini (Google)

**Adapter captures:**
- model_id (which model made the query)
- tokens_in / tokens_out (from API response metadata)
- latency (wall clock)
- outcome (did the model use the tool, reformulate, or abandon?)

### Task Distribution

The orchestrator assigns tasks from the bootstrap corpus (data/bootstrap-tasks.jsonl, 1,378 tasks) to model sessions. Each task runs on multiple models to generate convergence data.

**Session types:**
- **Single-model**: One model, one task. Generates behavioral signal.
- **Cross-model**: Same task, N models. Generates convergence signal when models agree.
- **Head-to-head**: Two models, same task. Reveals model-specific biases.

**Task assignment strategy:**
- Round-robin across models for cross-model coverage
- Weight toward domains with coverage gaps (from fast loop output)
- Never repeat the same task on the same model (no artificial repetition)

### Concurrency

- Claude Code: 1-3 concurrent sessions (each is a separate process)
- OpenCode/Aider: 1-3 concurrent sessions
- Raw API: 10-20 concurrent sessions (async HTTP, lightweight)
- Total: 15-25 concurrent sessions on the 96GB machine
- Each session: ~2-3GB RAM (Claude Code), ~500MB (API adapter)

### Telemetry Schema Extension

Current schema: (timestamp, session_id, query, actor_type, intent, domain, result_count, result_ids, latency_ms, is_retry, model_id)

Add for harness sessions:
- `campaign_id`: which campaign run this belongs to
- `task_id`: which bootstrap task was assigned
- `tokens_in`: input tokens consumed by the model (from API response)
- `tokens_out`: output tokens consumed
- `cost_usd`: computed from model pricing
- `adapter`: which adapter was used (mcp, opencode, bedrock-adapter)

## Layer 3: Spend Control Infrastructure

### The Problem

Bootstrap campaigns burn tokens. Without controls, a runaway campaign could spend $500 overnight on low-value signal. The spend control layer ensures every dollar generates measurable improvement.

### Budget Model

```yaml
campaign:
  name: "cross-model-week1"
  budget_usd: 50.00
  budget_tokens: 10_000_000

  # Per-model caps
  models:
    claude-sonnet:   { max_usd: 15.00, max_sessions: 40 }
    gpt-4o:          { max_usd: 15.00, max_sessions: 40 }
    gemini-1.5-pro:  { max_usd: 10.00, max_sessions: 40 }
    llama-3-70b:     { max_usd:  5.00, max_sessions: 40 }
    mistral-large:   { max_usd:  5.00, max_sessions: 40 }

  # Quality gates
  gates:
    min_signal_per_dollar: 2.0        # at least 2 boosts per dollar spent
    max_cost_per_signal: 0.50         # stop if a single boost costs more than $0.50
    min_convergence_rate: 0.1         # at least 10% of cross-model runs should converge
    stale_session_timeout: 300        # kill sessions that produce no queries for 5 min
```

### Real-Time Spend Monitor

A daemon that runs alongside the campaign orchestrator:

```
Every 30 seconds:
  1. Read telemetry: sum(cost_usd) by campaign_id
  2. Read boosts: count new boosts since campaign start
  3. Compute: cost_per_boost = total_cost / new_boosts
  4. Compute: budget_remaining = budget_usd - total_cost
  5. Compute: projected_cost = (total_cost / elapsed_time) * remaining_time

  If budget_remaining < 0:
    → HARD STOP. Kill all sessions. Alert.

  If cost_per_boost > max_cost_per_signal:
    → SOFT STOP. Pause new session spawning. Log reason.
    → Wait for operator review.

  If projected_cost > budget_usd * 1.2:
    → WARN. Reduce concurrency by 50%.

  If min_signal_per_dollar not met after 20% of budget spent:
    → GATE. Pause and present report:
      "Spent $10.00, generated 8 boosts (0.8/dollar, target 2.0/dollar).
       Top signal sources: [model breakdown].
       Recommendation: shift budget from [low-signal model] to [high-signal model]."
    → Wait for operator approval to continue or revise plan.
```

### Self-Adjustment

The harness adjusts its own behavior based on observed signal quality:

**Model routing adjustment:**
- Track cost_per_boost by model
- If Claude produces 5 boosts/$1 and GPT-4 produces 1 boost/$1, shift budget toward Claude
- But maintain minimum cross-model coverage (at least 10 sessions per model for convergence)

**Task routing adjustment:**
- Track boost_count by domain
- If "devops" tasks produce 3x more boosts than "data" tasks, weight toward devops
- But maintain diversity (coverage gaps can only be discovered by probing thin domains)

**Session length adjustment:**
- Track signal per session
- If signal concentrates in the first 3 queries of each session, shorten sessions
- If signal emerges only after 5+ queries (reformulation chains), allow longer sessions

**Cadence adjustment:**
- If the fast loop is producing diminishing returns (each cycle adds fewer new boosts), slow down query generation
- If the fast loop is producing accelerating returns, speed up

### Plan → Approve → Execute → Monitor → Adjust Cycle

```
1. PLAN: Orchestrator proposes a campaign:
   "Run 40 cross-model sessions across 5 models.
    Estimated cost: $25-35.
    Expected signal: 80-120 new boosts, 10+ convergence points.
    Duration: 4 hours."

2. APPROVE: Operator reviews and approves (or adjusts budget/scope).

3. EXECUTE: Campaign runs with spend monitoring.

4. MONITOR: Dashboard shows:
   - Real-time spend vs budget (burn-down chart)
   - Signal generation rate (boosts/hour)
   - Cost efficiency ($/boost by model)
   - Convergence events (cross-model agreements)

5. GATE CHECK (at 20%, 50%, 80% of budget):
   - Is cost/signal within bounds?
   - Are we getting cross-model convergence?
   - Any model producing zero signal? (drop it)

6. ADJUST or STOP:
   - If on track: continue
   - If cost/signal degrading: reduce concurrency, shift models
   - If no convergence: stop, diagnose, revise approach
   - If budget exceeded: hard stop, report
```

### Reporting

Post-campaign report:
```
Campaign: cross-model-week1
Duration: 3h 42m
Cost: $28.50 / $50.00 budget

Signal:
  New boosts: 94
  Convergence events: 12
  Coverage gaps filled: 3
  Cost per boost: $0.30

By model:
  claude-sonnet:    32 boosts, $8.20 ($0.26/boost)  ← best efficiency
  gpt-4o:           28 boosts, $9.10 ($0.33/boost)
  gemini-1.5-pro:   22 boosts, $6.40 ($0.29/boost)
  llama-3-70b:       8 boosts, $2.80 ($0.35/boost)
  mistral-large:     4 boosts, $2.00 ($0.50/boost)  ← consider dropping

Quality:
  Query-to-silence rate: 45% → 52% (+7%)
  Retry rate: 12% → 9% (-3%)
  Avg latency: 85ms (unchanged — infrastructure, not content)

Recommendation:
  Next campaign: increase claude/gemini allocation, reduce mistral.
  Focus on domains: devops, security (highest signal/cost).
  Projected cost for next 100 boosts: $30.
```

### Dashboard Integration

Add to the existing dashboard:
- **Burn-down chart**: budget remaining vs time
- **Signal rate**: boosts per hour, rolling window
- **Cost efficiency**: $/boost by model (bar chart)
- **Convergence radar**: which query patterns have cross-model agreement
- **Gate status**: green/yellow/red for each quality gate

## Open Questions

1. **AWS Bedrock pricing**: What models are available on Baron's subscription? What are the per-token costs? This determines the budget model.
2. **OpenCode MCP support**: Does OpenCode support MCP natively, or does it need a wrapper?
3. **Session orchestration**: Should the orchestrator be a Python script, a Docker Compose service, or something else?
4. **Token counting**: Bedrock returns token counts in response metadata. Claude Code does not expose token counts to MCP servers. How do we capture Claude's token cost per session?
5. **Convergence threshold**: How similar do queries need to be across models to count as "convergence"? Same tool for same query? Same tool for semantically similar queries?
