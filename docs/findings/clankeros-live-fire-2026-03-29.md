# ClankerOS Live-Fire Findings — 2026-03-29

Findings from the first ClankerOS end-to-end live-fire test against the exchange engine. Three automata (seller, buyer, attacker) booted from charts, polled the dontguess project campfire via ReadyWorkSource, and workers executed S01 (seller puts inventory) against the live exchange.

## Finding 1: `dontguess init` does not create named views

**Severity:** Medium
**Component:** `cmd/dontguess/init.go`

The convention spec (`docs/convention/core-operations.md` §8) defines six named views: `inventory`, `orders`, `matches`, `previews:pending`, `disputes`, `seller:<key>`. These are essential for agents to read exchange state without dropping to raw `cf read --tag` primitives.

Currently, `dontguess init` creates the exchange campfire and posts convention operation declarations, but does not create views. Views must be created manually with `cf view create`.

**Fix:** After posting operation declarations, `dontguess init` should create the standard views:

| View | Predicate | Ordering |
|------|-----------|----------|
| `puts` | `(tag "exchange:put")` | timestamp desc |
| `put-accepts` | `(tag "exchange:phase:put-accept")` | timestamp desc |
| `buys` | `(tag "exchange:buy")` | timestamp desc |
| `match-results` | `(tag "exchange:match")` | timestamp desc |
| `settlements` | `(tag "exchange:settle")` | timestamp desc |
| `disputes` | `(and (tag "exchange:settle") (tag "exchange:phase:dispute"))` | timestamp asc |

These are the views we created manually during the live-fire test and they work with cf v0.10.7. The convention spec's ideal predicates use `has-fulfillment` (e.g., inventory = puts that have been accepted and aren't expired), but `has-fulfillment` is not yet implemented in cf's predicate engine. The tag-based views above are the working fallback.

**Proof:** `cf c5c1ee put-accepts --json` works as a first-class convention read operation after manual view creation. Workers used these successfully in S01.

## Finding 2: Convention spec view predicates use unimplemented operators

**Severity:** Low (tracked upstream)
**Component:** `docs/convention/core-operations.md` §8

The spec defines views with `has-fulfillment`, `expired`, and `age-lt` predicates. cf v0.10.7 recognizes `has-fulfillment` in parsing but cannot evaluate it (errors on the nested s-expression argument syntax). `expired` and `age-lt` status is unknown.

**Impact:** The "inventory" view (puts with accepted fulfillments, not expired) cannot be expressed as a single view predicate. The `put-accepts` view is an acceptable workaround — it shows all put-accept messages, and agents can join on antecedents client-side.

**Action:** Track upstream in campfire repo. When `has-fulfillment` lands, update views to match the spec predicates. The views created in Finding 1 are intentionally simpler so they work today.

## Finding 3: `dontguess serve` should ensure views exist on startup

**Severity:** Low
**Component:** `cmd/dontguess/serve.go`

The exchange engine starts and begins polling, but doesn't verify that named views exist on the exchange campfire. If the campfire was created before views were a feature (or views were lost), agents fall back to raw reads.

**Fix:** On startup, `dontguess serve` should idempotently create the standard views if they don't exist. Use `cf view list <campfire-id> --json` to check, then `cf view create` for any missing views. This ensures every running exchange has ergonomic read operations regardless of how the campfire was originally created.

## Finding 4: Project campfire needs rd convention declarations for the rd wrapper to route correctly

**Severity:** High
**Component:** Project setup / `~/projects/os/bin/rd` wrapper

The OS `rd` wrapper script (bin/rd) detects whether to use ready's native rd or rudi's legacy rd by checking if the project campfire has `convention:operation` messages with `"convention":.*"work"`. The dontguess project campfire was created before rd had convention support, so it had no declarations. The wrapper silently fell through to rudi's rd, which wrote items to the rudi server instead of the campfire. ReadyWorkSource never saw them.

**Symptoms:** `rd create` succeeded but items were invisible to ClankerOS automata. No errors. `rd ready` showed items (reading from rudi server) but the campfire had no `work:create` messages.

**Fix applied:** Manually posted all 12 rd convention declaration JSONs from `~/projects/ready/pkg/declarations/ops/*.json` to the project campfire as `convention:operation` tagged messages. After this, the wrapper routed correctly and `rd create` wrote to both JSONL and campfire transport.

**Permanent fix needed:** `rd init` refuses to re-initialize if `.campfire/root` exists. There should be a `rd init --declarations-only` or `rd repair` command that posts missing convention declarations to an existing campfire without creating a new one. This is a ready repo issue.

## Finding 5: Agent specs were using Go code patterns instead of cf CLI

**Severity:** High (fixed)
**Component:** `.claude/agents/exchange-{seller,buyer,attacker}.md`

The original agent specs told workers to write Go programs using `github.com/campfire-net/campfire/pkg/*` to send exchange messages. Workers run in Landlock jails without Go toolchain access and without the campfire module in their GOPATH. The specs were rewritten to use cf CLI convention commands.

**Fix applied:** Three commits:
1. `d8793e4` — Rewrote specs to use `cf <campfire-id> <operation> -- <args>`
2. `e168667` — Updated to use named view operations (`cf <id> put-accepts --json`) instead of raw `cf read --tag`
3. `342df38` — Configured buyer and attacker charts with exchange campfire ID and operator key

## Finding 6: Seller chart pointed at wrong campfire

**Severity:** High (fixed)
**Component:** `charts/seller.toml`

The seller chart's `[worksource]` pointed at a bare campfire (9d282c83) that was created manually for early testing. It had no rd convention declarations and no work items from `rd create`. Switched to the dontguess project campfire (9b8cf9af) which is the rd-initialized campfire.

**Fix applied:** `eb2f7f5` — Updated `campfire` field to the project campfire ID.

## Live-Fire Results

**S01 outcome:** Workers spawned, joined the exchange campfire, sent puts via `cf c5c1ee put --`, and the exchange engine auto-accepted them at 70% of token_cost. 16 puts sent, 16 put-accepts received.

**Known ClankerOS issues observed (not dontguess bugs):**
- All three automata claimed the same item (no skill-based routing in ReadyWorkSource)
- Worker sender keys (f8888ac2, a9cbcfcb) didn't match the provisioned seller identity (ae58d729) — jail identity provisioning issue
- Constellation telemetry campfire sends failed (worker identity not admitted to constellation campfires)
