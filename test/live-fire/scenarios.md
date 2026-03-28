# DontGuess Live-Fire Test Scenarios

Test scenarios for ClankerOS automata running against a live `dontguess serve` instance.

Each scenario is a work item posted to the shared Ready campfire. The assigned automaton claims it, executes against the live exchange, and reports pass/fail.

## Prerequisites

Before running scenarios:
1. `dontguess init --convention-dir ./docs/convention --force`
2. `dontguess serve` running in background
3. Note EXCHANGE_CAMPFIRE and OPERATOR_KEY from serve output
4. Set these in chart.toml `[exchange]` sections or export as env vars

---

## Phase 1: Core Happy Paths

### S01: Seller puts inventory
**Assignee:** seller
**Priority:** P0

Put 3 items with different content types:
1. `analysis` — "Go concurrency patterns" (2500 tokens, domains: go, concurrency)
2. `data` — "PostgreSQL vs SQLite benchmarks" (4000 tokens, domains: database, performance)
3. `code` — "Rate limiter implementation in Go" (1500 tokens, domains: go, networking)

**Pass condition:** All 3 puts appear on campfire. Engine auto-accepts all 3 within 5s. Each has a `settle:put-accept` response with price = 70% of token_cost.

### S02: Buyer searches and matches
**Assignee:** buyer
**Depends on:** S01

Send a buy for "Go concurrency patterns for web servers" with budget=5000, content_type=analysis.

**Pass condition:** Engine returns match with >=1 result. Top result is the Go concurrency entry. Confidence > 0. Price <= 5000.

### S03: Full settlement (large content)
**Assignee:** buyer
**Depends on:** S02

Using the match from S02:
1. Send settle(preview-request)
2. Wait for settle(preview) — verify 5 chunks returned
3. Send settle(buyer-accept)
4. Wait for settle(deliver)
5. Send settle(complete)

**Pass condition:** All 5 phases complete. Each engine response appears on campfire with correct antecedents. Final state: entry marked as sold.

### S04: Buyer rejects after preview
**Assignee:** buyer
**Depends on:** S01

Buy the PostgreSQL benchmarks entry. Preview it. Then reject.

1. Send buy (budget=10000, content_type=data)
2. Wait for match
3. Send settle(preview-request)
4. Wait for settle(preview)
5. Send settle(buyer-reject)

**Pass condition:** No scrip movement. No buy-hold message. Transaction ends cleanly.

### S05: Small content — direct accept (no preview)
**Assignee:** seller, buyer

Seller puts a small item (400 tokens, < 500 threshold). Buyer buys it directly without preview.

1. Seller: put with token_cost=400, content_size=400
2. Wait for auto-accept
3. Buyer: buy matching the small item
4. Wait for match
5. Buyer: settle(buyer-accept) — no preview phase
6. Wait for settle(deliver)
7. Buyer: settle(complete)

**Pass condition:** Full settlement without preview phase. Scrip settled correctly.

---

## Phase 2: Scrip & Economic Flows

### S06: Scrip mint and balance check
**Assignee:** buyer

Verify scrip minting works (operator must mint — this tests reading the balance after operator action).

**Pass condition:** After operator mints scrip to buyer, buyer's balance reflects the minted amount.

### S07: Buy with insufficient scrip
**Assignee:** buyer

Buyer with 0 scrip balance sends a buy for an expensive item.

**Pass condition:** Engine processes buy but either: (a) returns match with results (scrip check is at buyer-accept time, not buy time), or (b) returns 0 results if pre-check is implemented. Document which behavior occurs.

### S08: Small content dispute — auto-refund
**Assignee:** seller, buyer

1. Seller puts small content (400 tokens)
2. Buyer buys, accepts, receives delivery
3. Buyer sends settle(small-content-dispute) with dispute_type=quality_inadequate

**Pass condition:** Buyer gets full refund (scrip-dispute-refund message). Seller reputation decreases by 3.

---

## Phase 3: Adversarial — Authorization

### S09: Forged put-accept
**Assignee:** attacker

1. Seller puts an item (real put)
2. Attacker sends settle(put-accept) for that put (attacker is NOT operator)

**Pass condition:** Inventory does NOT contain the entry. Engine ignores the forged accept.

### S10: Forged match response
**Assignee:** attacker

1. Buyer sends a real buy
2. Attacker sends exchange:match (antecedent: the buy) with fake results

**Pass condition:** The buyer's order remains active/unmatched. Only the operator's match counts.

### S11: Forged scrip mint
**Assignee:** attacker

Attacker sends scrip-mint message giving themselves 999999 scrip.

**Pass condition:** Attacker's scrip balance remains 0.

### S12: Forged deliver
**Assignee:** attacker

After a legitimate buyer-accept, attacker sends settle(deliver).

**Pass condition:** Settlement does NOT advance. Match is not marked as delivered.

---

## Phase 4: Adversarial — Payloads

### S13: Oversized description
**Assignee:** attacker

Send exchange:put with description > 64 KiB (100KB string).

**Pass condition:** Entry not created. State rejects oversized payload.

### S14: Invalid content hash
**Assignee:** attacker

Send exchange:put with content_hash = "not-a-hash".

**Pass condition:** Entry not created or not matched.

### S15: Negative token cost
**Assignee:** attacker

Send exchange:put with token_cost = -1.

**Pass condition:** Entry not created.

### S16: Too many domains
**Assignee:** attacker

Send exchange:put with 10 domains.

**Pass condition:** Entry either rejected or domains truncated to 5.

### S17: Zero budget buy
**Assignee:** attacker

Send exchange:buy with budget=0 (and no max_price field).

**Pass condition:** Match returns 0 candidates.

---

## Phase 5: Adversarial — State Manipulation

### S18: Replay settle:complete
**Assignee:** attacker

After a legitimate complete, re-send the exact same complete message.

**Pass condition:** Second complete ignored. No double-settlement.

### S19: Complete before deliver
**Assignee:** attacker

Send settle(complete) for a match that hasn't been delivered yet.

**Pass condition:** Engine rejects or ignores (wrong phase).

### S20: Dispute after complete
**Assignee:** attacker

Send settle(small-content-dispute) for a match that's already completed.

**Pass condition:** Engine rejects (already settled).

### S21: Accept with bogus match ID
**Assignee:** attacker

Send settle(buyer-accept) with a non-existent match message ID as antecedent.

**Pass condition:** Engine ignores (no matching order).

---

## Phase 6: Dynamic Pricing

### S22: Price increases with demand
**Assignee:** seller, buyer

1. Seller puts an item
2. Multiple buyers purchase it (complete settlement each time)
3. After 3+ completions, check the match price for a new buyer

**Pass condition:** Price for the 4th buyer is higher than price for the 1st buyer (demand multiplier applied).

### S23: Layer 0 exclusion gate
**Assignee:** seller, buyer

1. Seller puts an item
2. 15+ buyers preview it but only 0 complete (low conversion)
3. New buyer searches for it

**Pass condition:** After 10+ previews with 0 completions, the entry is excluded from match results (conversion rate < 5%).

---

## Phase 7: Reputation

### S24: New seller default reputation
**Assignee:** seller

New seller identity puts an item.

**Pass condition:** Seller reputation = 50 (default).

### S25: Min reputation filter
**Assignee:** buyer

Buyer sends buy with min_reputation=70. Only sellers with rep >= 70 should match.

**Pass condition:** No results returned (all sellers at default 50).

---

## Phase 8: Engine Resilience

### S26: Engine restart — state replay
**Assignee:** buyer

1. Seller puts items, buyer buys and completes
2. Kill dontguess serve
3. Restart dontguess serve
4. Verify: inventory, scrip balances, order history all match pre-restart state

**Pass condition:** All state identical after restart. Engine replays full log correctly.

### S27: Concurrent buyers
**Assignee:** buyer (x3)

3 buyer agents simultaneously send buy requests for the same content type.

**Pass condition:** All 3 get match responses. No crashes, no duplicate matches, no data corruption.
