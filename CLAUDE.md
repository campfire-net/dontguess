# CLAUDE.md — DontGuess Project Instructions

> OS-level instructions (session protocol, model routing, blog pipeline, rules) are inherited from `~/.claude/CLAUDE.md`. This file contains only project-specific configuration.

## Project

**DontGuess**: Token-work exchange — a marketplace where agents buy and sell cached inference results. An operator buys inference results from sellers at a discount (scrip), dynamically prices them, sells them to buyers (scrip), and pays residuals to original authors. Agents earn scrip by selling work or performing assigned tasks (context compression, validation, freshness checks). Anyone can operate an exchange; exchanges may federate for global liquidity with trust semantics.

Previously a tool discovery engine (see `docs/heritage/`). The thesis survived the pivot: reduce agent token waste through better discovery. Old: discover the right tool. New: discover pre-computed work someone already paid for.

**Domain:** dontguess.ai. "Don't guess — look it up."

## Work Tracking — rd (not bd)

**This project uses `rd` for all work tracking.** The `bd` CLI is NOT used in dontguess sessions.

```bash
rd list                    # All items
rd list --status active    # Active items
rd ready                   # Ready queue (ETA-sorted)
rd show <id>               # Item details
rd create "Title" --type task  # New item
rd update <id> --status active # Change status
rd close <id> --reason "..."   # Close with reason
```

## Architecture

DontGuess is a campfire application. The campfire is the backend. Exchange operations are convention-conforming messages. State is derived from the message log.

### Three Systems

1. **Convention** — Defines exchange operations (put, buy, match, settle, dispute, assign). Lives in `docs/convention/`.
2. **Matching engine** — Semantic similarity search over cached inference. Matches buyer task descriptions to seller inventory. Uses vector embeddings (all-MiniLM-L6-v2, 384-dim).
3. **Pricing engine** — Dynamic pricing via three feedback loops (fast/medium/slow). Behavioral signals drive price, not preferences.

### Integration Points

- **Campfire** — all exchange state lives on campfire. Puts, buys, matches, settlements are convention messages.
- **Forge** — metering backbone. Tracks scrip balances, spending limits, token-cost attribution. Scrip is denominated in inference token cost.
- **x402** — optional external settlement rail for cross-operator transactions (USDC). Not required for single-operator use.

### The Publisher Model

DontGuess is a publisher, not a broker:

1. Agent does inference, sells result to the exchange for scrip (upfront, discounted % of token cost)
2. Exchange owns the result, prices it dynamically based on demand signals
3. Original author earns residuals in scrip as copies sell
4. Buyers spend scrip earned from selling their own work or doing assigned tasks
5. Assigned tasks (context compression, validation, freshness checks) are exchange maintenance paid in scrip
6. Every transaction is campfire messages

### Scrip

Scrip is denominated in token cost. It is not redeemable for cash. It is only exchangeable for other cached inference on the marketplace. New scrip enters the system via x402 purchase or labor (assigned work). Matching fees burn scrip (deflationary pressure).

### The Three Loops (Heritage from toolrank)

| Loop | Cadence | Reads | Writes | Purpose |
|------|---------|-------|--------|---------|
| **Fast** | 5 min | Purchase events, cache hit/miss | Price adjustments | Demand velocity, price elasticity |
| **Medium** | 1 hr | Accumulated adjustments, disputes | Residual settlements, reputation updates | Market correction, seller trust |
| **Slow** | 4 hr | Historical price/volume, buyer satisfaction | Market parameters, commission structure | Structural optimization |

### The 4-Layer Value Stack (Heritage from toolrank)

Each layer gates the ones above it. Layer 0 rejects any change that regresses correctness.

```
Layer 0  CORRECTNESS GATE    task_completion_rate       No loop owns this — validation only
Layer 1  TRANSACTION EFFICIENCY  tokens_saved / price    Fast loop target
Layer 2  VALUE COMPOSITE     completion + efficiency + recency + diversity   Medium loop gate
Layer 3  MARKET NOVELTY      buyer_count / competing_entries * discovery    Slow loop target
Layer 4  META                oscillation_frequency     Adapts slow loop step size
```

**Behavioral signals over preference signals.** Don't trust ratings. Measure: did the cached inference actually complete the buyer's task? Did they search again? Did they come back to the same seller?

## Agent Roster

| Agent | Spec | Domain | Default Tier |
|-------|------|--------|-------------|
| Manager | `.claude/agents/manager.md` | Coordination, decomposition, routing | inherit |
| Implementer | `.claude/agents/implementer.md` | Code, tests, commits | sonnet |
| Reviewer | `.claude/agents/reviewer.md` | Code review, spec conformance | sonnet |
| Designer | `.claude/agents/designer.md` | Convention spec, adversarial design | opus |

**Routing:**
- Convention design, adversarial review -> Designer (opus)
- Matching/pricing engine implementation -> Implementer (sonnet)
- Forge integration, campfire wiring -> Implementer (sonnet)
- Code review, spec conformance -> Reviewer (sonnet)
- Decomposition, routing, status -> Manager (inherit)
- Template-driven edits, config, data migration -> Implementer (haiku)

## Task-Type -> Model Mapping

| Task Type | Model | Rationale |
|-----------|-------|-----------|
| Convention design (exchange operations) | **Opus** | Novel protocol design, multi-factor trade-offs |
| Adversarial design review | **Opus** | Attack surface analysis, trust model |
| Matching engine (vector search, semantic similarity) | **Sonnet** | Structured implementation, algorithmic |
| Pricing engine (feedback loops) | **Sonnet** | Multi-signal computation, requires care |
| Forge integration (scrip ledger, metering) | **Sonnet** | API integration, correctness-critical |
| Campfire convention wiring | **Sonnet** | Protocol conformance, message schema |
| Data migration from toolrank | **Haiku** | Mechanical, template-driven |
| Config, CI/CD, deployment | **Haiku** | Mechanical |

## Source of Truth Hierarchy

1. **Convention spec** (`docs/convention/`) — what exchange operations mean
2. **This CLAUDE.md** — project instructions
3. **Heritage docs** (`docs/heritage/`) — design principles from toolrank that survive the pivot
4. **Source code** — implementation

## Design Change Cascade

Any change to the exchange convention triggers:

| Step | Agent | Review |
|------|-------|--------|
| 1 | Designer | Convention spec consistency, security implications |
| 2 | Implementer | Implementation feasibility, Forge integration impact |
| 3 | Reviewer | Test coverage, edge cases |

## Repo Structure

```
dontguess/
  CLAUDE.md                    # This file
  docs/
    convention/                # Exchange convention spec (the authority)
    design/                    # Active design docs
    heritage/                  # Transferred design principles from toolrank
  .claude/
    agents/                    # Agent specs
  cmd/                         # CLI entry points (Go)
  pkg/                         # Go packages
    matching/                  # Semantic matching engine
    pricing/                   # Dynamic pricing (fast/medium/slow loops)
    convention/                # Exchange convention declarations
    scrip/                     # Scrip ledger integration with Forge
  test/                        # Integration and E2E tests
```

## Heritage

DontGuess was previously a tool discovery engine (17K LOC Python, ~/projects/toolrank). The codebase is not reused, but key design principles transfer:

- **4-layer value stack** — correctness gates everything, behavioral signals over preferences
- **Three feedback loops** — fast (demand), medium (correction), slow (structural optimization)
- **Semantic matching** — vector embeddings for similarity search
- **Cross-agent convergence** — the ungameable trust signal (3+ agents succeed with same cache entry)
- **Observational boundary** — you can't see downstream task success, only proxies (completion, retry, return rate)
- **Escape velocity** — the point where dynamic pricing outperforms static baseline without manual intervention

See `docs/heritage/` for the original design docs that informed this architecture.
