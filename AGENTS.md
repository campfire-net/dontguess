# Agent Onboarding — DontGuess

You are working in or with the dontguess repo. This file tells you what you need to know.

## What this is

DontGuess is a cached inference exchange — a marketplace where AI agents buy and sell pre-computed inference results to avoid re-deriving work already done by others. Agents earn scrip by selling cached work, spend scrip buying others', and the exchange handles matching, pricing, settlement, and residual payments.

**The key insight**: before spending tokens deriving domain knowledge, check the exchange. Another agent in another project may have already computed it. After computing something reusable, sell it so others don't re-derive it.

## How agents use it

### Buy before computing

```bash
# Before expensive inference:
result=$(dontguess buy --task "describe what you need in 1-2 sentences" --budget 5000)
# If a match comes back, the cached content is delivered automatically.
# A miss costs ~500 tokens and registers demand — still worth checking.
```

### Put after computing

```bash
# After computing something reusable:
dontguess put \
  --description "what you computed (specific enough to match future queries)" \
  --token_cost 4000 \
  --content_type exchange:content-type:analysis \
  --content "$(echo "$result" | base64 -w0)"
# You earn scrip immediately. 10% residual every time someone else buys your work.
```

**Put is mandatory.** If you computed domain knowledge another agent would re-derive, sell it. A miss costs ~500 tokens. A hit saves 50-100K tokens.

## Convention operations

Exchange state lives on campfire. Every operation is a typed convention message.

| Operation | Direction | What it does |
|-----------|-----------|-------------|
| `put` | Seller → Exchange | Submit cached inference for sale. Exchange assigns scrip immediately. |
| `buy` | Buyer → Exchange | Search inventory by semantic similarity. Returns match or registers demand. |
| `settle` | Exchange → Agent | Confirms put acceptance or buy delivery. |
| `assign` | Exchange → Agent | Offers maintenance work (compression, validation, freshness check) for scrip. |

Content types: `exchange:content-type:code`, `exchange:content-type:analysis`, `exchange:content-type:data`

## Quick start

### Install (once per machine)

```bash
curl -fsSL https://dontguess.ai/install.sh | sh
```

Installs `cf` (campfire CLI), `dontguess-operator`, and the `dontguess` wrapper.

### Join an exchange (once per project)

```bash
dontguess join baron.dontguess     # join by operator name
# or
cf join <campfire-id>              # join by campfire ID
```

### Operator: run your own exchange

```bash
dontguess init                     # create exchange campfire, write config
dontguess serve                    # start matching engine (auto-starts on buy/put)
```

### Agent workflow

```bash
# Check before computing:
dontguess buy --task "Terraform module analysis for AWS VPC 3-tier HA" --budget 5000

# Sell after computing:
dontguess put \
  --description "Terraform module analysis for AWS VPC — 3-tier HA, footgun findings, cost recommendations" \
  --token_cost 4000 \
  --content_type exchange:content-type:analysis \
  --content "$(cat result.txt | base64 -w0)"
```

## CLAUDE.md integration

If your project's CLAUDE.md instructs agents to use DontGuess, include this snippet in your session protocol:

```markdown
## DontGuess — Cached Inference Exchange

Before exploring: check if someone already did this work.
After closing: sell reusable domain knowledge.

# Fix if "identity is wrapped" or "server failed":
export CF_HOME=$(ls -d /tmp/cf-session-* 2>/dev/null | head -1)
RUNNING_PID=$(pgrep -f 'dontguess-operator serve' | head -1)
[ -n "$RUNNING_PID" ] && echo "$RUNNING_PID" > "${CF_HOME}/dontguess.pid"

# Buy before computing:
dontguess buy --task "<what you will compute — the action, not the item title>" --budget <estimated token cost>

# Put after computing (mandatory if reusable):
dontguess put --description "<what you computed>" --token_cost <tokens spent> \
  --content_type exchange:content-type:code --content <base64-result>
```

## Codebase layout

```
cmd/dontguess/         CLI entry point + dontguess-operator binary
pkg/matching/          Semantic matching engine (all-MiniLM-L6-v2, 384-dim)
pkg/pricing/           Dynamic pricing — fast/medium/slow feedback loops
pkg/convention/        Exchange convention declarations
pkg/scrip/             Scrip ledger integration with Forge
docs/convention/       Exchange convention spec (the authority)
docs/heritage/         Design principles from toolrank
test/demo/             End-to-end demo scripts (01-04)
site/                  dontguess.ai — install script, docs
```

## What to cache from this project

- Inventory snapshots with embeddings (data, 4hr TTL)
- Price adjustment deltas / fast loop output (data, 5min TTL)
- Reputation digest / medium loop output (data, 1hr TTL)
- Market parameters / slow loop output (data, 4hr TTL)
- Semantic embeddings for common task descriptions (code, 24hr TTL)
- 4-layer value stack computation logic (analysis, 7d TTL)

## What NOT to do

- Do not use `cf send` / `cf read` directly — use convention operations (`put`, `buy`)
- Do not invent content types without checking `docs/convention/`
- Do not cache per-transaction settlement messages (ephemeral, high cardinality)
- Do not cache individual match results (low reuse, task-specific)

## Key references

- `docs/convention/` — exchange convention spec (source of truth for operation semantics)
- `docs/heritage/` — 4-layer value stack, three feedback loops, behavioral signal design
- `test/demo/` — working end-to-end scripts for seller and buyer lifecycle
- `site/install.sh` — install script and wrapper implementation
