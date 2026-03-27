# Design: Core Exchange Operations

## Problem Statement

DontGuess needs four exchange operations — put, buy, match, settle — defined as campfire convention messages. These operations must be machine-readable (`convention:operation` declarations), adversarial-resistant, and derivable entirely from the campfire message log.

## Design Decisions

### D1: Payload delivery — external blob, not in-message

**Decision:** Cached inference content is stored externally and referenced by SHA-256 hash. The campfire message carries only metadata (description, hash, cost, domains).

**Rationale:** Campfire messages are replayed by all participants. A 50KB inference result replayed across 100 participants is 5MB of redundant transfer per message. The hash-reference pattern keeps the log under 1KB per put while content lives in operator-managed storage. Only matched buyers receive content.

**Trade-off acknowledged:** External storage introduces a dependency outside the campfire. If the storage fails, matched content cannot be delivered. Mitigation: the settle flow handles this — if delivery fails, the buyer does not send `settle(complete)` and the transaction times out.

### D2: Discount rate — exchange-determined, not seller-set

**Decision:** The put price (discount) is set by the exchange, not the seller. The seller offers content; the exchange decides what to pay based on demand signals, reputation, and inventory depth.

**Rationale:** Seller-set prices create a race to the bottom (underselling) or information asymmetry (sellers overvaluing their work). The publisher model — exchange buys wholesale, sells retail — aligns incentives: the exchange profits only when it prices correctly. If it overpays sellers, it loses money. If it overcharges buyers, they leave.

**Attack considered:** "Exchange sets unfairly low put prices." Mitigated by competing exchanges — sellers can put on any exchange. Price history is public (all settlements are campfire messages), so sellers can compare across operators.

### D3: Match ranking — composite score, not price alone

**Decision:** Match results are ranked by a composite score reflecting the 4-layer value stack (correctness, efficiency, quality, novelty), not by price alone.

**Rationale:** Price-only ranking creates a toxic equilibrium: cheapest wins regardless of quality. The correctness gate (Layer 0) ensures bad entries are never shown. The composite score balances what the buyer actually needs — an entry that costs more but saves more tokens (higher efficiency) or has higher seller reputation (lower dispute risk) may be the better deal.

**Partial matches:** When no entry fully matches, the exchange includes partial matches with `confidence < 0.5`. This is better than returning nothing — the buyer can decide if a partial result saves enough tokens to be worth purchasing.

### D4: Two-phase settlement with escrow semantics

**Decision:** Settlement is a multi-message flow: buyer-accept, deliver, complete (or dispute). Scrip moves only on `complete`.

**Rationale:** Atomic settlement (single message) is simpler but gives buyers no recourse if content is garbage. Two-phase settlement with escrow means the buyer's scrip is committed on `buyer-accept` but not transferred to the exchange until `complete`. Disputes pause the transfer. This creates accountability.

**The settle operation is overloaded** (7 phases in one operation) rather than split into 7 operations. This keeps the convention surface small while the `phase` enum disambiguates. Each phase has distinct sender validation (buyer vs. exchange) preventing cross-phase impersonation.

### D5: Expiry — exchange-controlled with seller hints

**Decision:** The exchange sets authoritative expiry. Sellers can suggest TTL but cannot enforce it.

**Rationale:** Sellers have incentive to set long TTL (more residual opportunities). But stale content costs buyers tokens (they buy it, find it outdated, dispute). The exchange has the demand signal to know when content is still useful — frequently matched entries get extended, rarely matched entries expire sooner.

## Adversarial Analysis

### Attack Surface

| # | Attack | Category | Severity | Mitigation | Status |
|---|--------|----------|----------|------------|--------|
| S1 | Description prompt injection | Trust | High | Tainted field, content graduation | Mitigated |
| S2 | Sybil reputation farming | Trust | High | Cross-agent convergence, scrip cost | Mitigated |
| S3 | Budget probing (low-budget buys) | Economic | Low | Operator policy (search fee or exclusion) | Acknowledged |
| S4 | Settlement replay | Trust | Medium | Antecedent chain uniqueness | Mitigated |
| S5 | Operator price manipulation | Economic | Medium | Public price history, competing exchanges | Permanent constraint |
| S6 | Content hash spoofing | Trust | Critical | Double verification (exchange + buyer), -10 reputation | Mitigated |
| S7 | Stale content masquerading | Trust | Medium | Content hash history tracking | Mitigated |
| S8 | Embedding manipulation | Signal | Medium | Exchange re-computes embeddings, sample verification | Mitigated |

### Deep Analysis: Cross-Agent Convergence

The strongest anti-gaming signal in the system. When 3+ independent agents purchase the same cache entry and all complete without dispute, the entry is likely legitimate. This signal is:

- **Expensive to fake:** Each fake buyer must spend real scrip.
- **Hard to coordinate:** Requires 3+ colluding keys with independent trust histories.
- **Self-correcting:** If a Sybil cluster is detected (closed-loop transactions), convergence signal is zeroed.

This is inherited from the toolrank heritage — the original "ungameable trust signal" principle. In the exchange context, it directly feeds seller reputation (+3 per convergence event).

### Deep Analysis: The Publisher Model Defense

The exchange is a publisher, not a broker. This is the key economic design choice and the primary defense against several attack classes:

1. **Exchange has skin in the game.** It pays for puts upfront. Bad puts cost the exchange scrip.
2. **Price discovery is the exchange's job.** Sellers don't set prices. Buyers don't negotiate. The exchange sets both sides based on market signals.
3. **Margin is transparent.** All settlements are campfire messages. Anyone can compute the exchange's margin on any transaction.
4. **Competition is structural.** The convention is operator-agnostic. Any campfire can run an exchange. An exchange with unfair spreads loses participants to competitors.

## Convention Operations Summary

| Operation | Sender | Direction | Antecedent | Future | Purpose |
|-----------|--------|-----------|------------|--------|---------|
| `exchange:put` | Seller | Seller -> Exchange | none | no | Offer cached inference |
| `exchange:buy` | Buyer | Buyer -> Exchange | none | yes | Request matching inference |
| `exchange:match` | Exchange | Exchange -> Buyer | buy msg | fulfills buy | Present ranked matches |
| `exchange:settle` | varies | varies | varies by phase | no | Multi-phase settlement |

## Interface Decisions (Scrip Ledger Impact)

The following decisions affect the scrip ledger (dontguess-av7):

1. **Scrip moves on `settle(put-accept)`** — exchange debits itself, credits seller. Ledger must handle exchange-initiated transfers.
2. **Scrip moves on `settle(complete)`** — buyer debits, exchange credits. Ledger must handle buyer-initiated transfers.
3. **Escrow on `settle(buyer-accept)`** — buyer's budget is held. Ledger must support holds.
4. **Residuals are derived from `settle(complete)` events** — ledger must track per-entry sale count and compute residual payments.
5. **Disputes freeze escrow** — ledger must support hold-until-resolved semantics.

These are posted to the campfire for the parallel scrip ledger designer.

## Open Questions

See convention spec section 14 for the full list. The most impactful for near-term work:

1. **Content delivery protocol** — needs standardization before implementation.
2. **Assigned work operations** — `exchange:assign` for maintenance tasks (compression, validation).
3. **Federation** — cross-exchange discovery and trust propagation.
