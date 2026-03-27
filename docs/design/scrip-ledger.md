# Scrip Ledger & Forge Integration — Design Document

> dontguess-av7 | 2026-03-27 | Status: Draft — pending adversarial review

## Problem

DontGuess needs an internal currency (scrip) that tracks value through the exchange. Agents earn scrip by selling inference results and performing labor; they spend scrip buying cached inference. The ledger must be tamper-resistant, overdraft-proof, and auditable. It must integrate with Forge's existing metering and spending infrastructure without forking Forge.

## Design Decisions

### D1: Forge as Library, Not Service

**Decision: Library.**

Forge's `ratelimit.SpendingLimiter` implements exactly the pre-decrement/adjust/refund pattern scrip needs. DontGuess imports Forge's `ratelimit` and `meter` packages directly as Go library dependencies. No RPC, no service mesh, no deployment coupling.

**Rationale:**
- Forge's SpendingStore interface is backend-agnostic (Azure Table Storage in prod, in-memory for tests). DontGuess can provide its own store implementation backed by campfire state.
- The pre-decrement model (PreDecrement -> work -> Adjust/Refund) maps perfectly to exchange operations: pre-decrement buyer's scrip before match, adjust after settlement, refund on dispute.
- A service boundary adds latency, failure modes, and deployment coupling for no architectural benefit at current scale. DontGuess and Forge run in the same trust domain (Third Division Labs operator).

**Migration path:** If DontGuess federates with external operators, the library call becomes an RPC call behind the same interface. The SpendingStore abstraction survives.

### D2: No Free Starter Balance — Must Earn or Buy

**Decision: Zero initial balance. Scrip enters only via x402 purchase or labor.**

**Rationale:**
- Free starter balances are a Sybil vector. An attacker creates N identities, collects N free balances, consolidates, and dumps low-quality inference to earn more.
- The bootstrap-harness heritage doc (Layer 3: Spend Control) shows the cost-per-signal thinking. Free scrip with no cost basis produces noise, not signal.
- New agents bootstrap by: (a) performing assigned labor (validation, compression, freshness checks) paid in scrip by the exchange, or (b) purchasing scrip via x402.
- The operator seeds the initial scrip supply by funding the exchange's own identity via x402. This is the monetary base.

**Cold-start concern:** The first agents on an exchange have no one to buy from. The operator solves this by: (1) funding exchange identity with scrip via x402, (2) posting seed inference results (the operator's own cached work), (3) paying early agents scrip for labor. This is equivalent to a central bank injecting liquidity — the operator is the lender of first resort.

### D3: Hard-Blocked Negative Balances — Pre-Decrement Only

**Decision: No debt. Pre-decrement prevents overdraft. If balance insufficient, the operation fails with 402.**

**Rationale:**
- Forge's SpendingLimiter already implements this: `ErrBudgetExceeded` on insufficient balance, atomic pre-decrement with ETag-based CAS.
- Debt creates collection problems in an anonymous agent system. There is no legal identity to pursue. An agent that goes negative can simply rotate keys.
- Hard-blocking is simpler to reason about, audit, and explain. Every scrip unit in circulation is backed by a prior inflow (purchase or labor).

### D4: Scrip is Local Per Operator — Not Transferable Across Operators

**Decision: Scrip is local to a single operator's exchange. No cross-operator scrip transfer in v1.**

**Rationale:**
- Cross-operator scrip requires a shared trust model: who validates the other operator's ledger? Who absorbs the risk of counterfeit scrip from a rogue operator?
- x402 provides the cross-operator settlement rail. If Agent A has scrip on Operator 1 and wants to buy from Operator 2, the flow is: Agent A -> x402 (USDC) -> Operator 2 scrip. The USDC layer handles trust.
- Federation is a v2 concern. When it arrives, it will use x402 as the bridge currency, not direct scrip transfer.

**Implication for core ops (schema-change for parallel designer):** The `buy` operation must specify which operator's exchange it targets. Cross-operator buys go through x402, not scrip transfer.

### D5: x402 Conversion Rate — Operator-Set, Not Floating

**Decision: Each operator sets their own x402-to-scrip conversion rate. It is not a market-determined float.**

**Rationale:**
- A floating rate requires a price discovery mechanism (order book, AMM). That is a separate system with its own attack surface.
- The operator is the publisher — they set the price of scrip entry, just as they set the commission on matches. The rate is a business decision, not a market outcome.
- Operators compete on conversion rate. An operator offering 1 USDC = 1000 scrip competes with one offering 1 USDC = 1200 scrip. This is market competition between operators, not within a single exchange.
- The rate is published as a convention message so agents can compare operators programmatically.

---

## Scrip Flow

### Flow Diagram

```
                    x402 (USDC)
                        │
                        ▼
              ┌─────────────────────┐
              │   MINT              │  Operator sets conversion rate
              │   x402 → scrip     │  Creates new scrip supply
              └────────┬────────────┘
                       │
                       ▼
    ┌──────────────────────────────────────┐
    │           OPERATOR BALANCE           │
    │   (exchange's own scrip reserve)     │
    └──────┬──────────────┬────────────────┘
           │              │
      labor pay      seed inventory
           │              │
           ▼              ▼
    ┌────────────┐  ┌────────────┐
    │  AGENT A   │  │  EXCHANGE  │
    │  (laborer) │  │  INVENTORY │
    └─────┬──────┘  └─────┬──────┘
          │               │
     put (sell)      buy (purchase)
          │               │
          ▼               ▼
    ┌──────────────────────────────────────┐
    │           MATCH + SETTLE             │
    │                                      │
    │  Buyer pays: price + fee             │
    │  Seller gets: residual               │
    │  Exchange gets: spread + fee         │
    │  Fee is BURNED (deflationary)        │
    └──────────────────────────────────────┘
```

### Scrip Movement by Operation

| Operation | From | To | Amount | Notes |
|-----------|------|----|--------|-------|
| **mint** | (new supply) | operator | x402_amount * rate | x402 payment triggers scrip creation |
| **put** | operator | seller | token_cost * discount_pct | Operator buys result at discount |
| **buy** | buyer | escrow | price + fee | Pre-decremented at buy time |
| **match** | escrow | exchange | price | Exchange owns inventory, sets price |
| **settle** | exchange | seller | residual | Residual = price * residual_pct |
| **settle** | escrow | (burned) | fee | Matching fee destroyed |
| **assign** | operator | agent | task_reward | Labor payment |
| **assign-complete** | (validate) | agent | task_reward | Confirmed after validation |
| **dispute-refund** | exchange | buyer | price + fee | Full refund on successful dispute |

### The Publisher Model in Scrip Terms

1. **Seller puts inference result.** Exchange pays seller immediately at `token_cost * discount_pct` (e.g., 70% of inference cost). This is the wholesale price. Exchange now owns the result.
2. **Exchange prices the result** using the three-loop pricing engine. Price floats based on demand signals.
3. **Buyer requests a match.** Pre-decrement buyer's balance for `price + fee`. If insufficient, 402.
4. **Match succeeds.** Buyer receives the cached inference. Fee is burned. Exchange retains `price - residual`.
5. **Residual accrues to original seller.** This happens at medium-loop cadence (1hr), not per-transaction. Residuals accumulate and settle in batch.

---

## Forge Integration

### Identity Model

Scrip balances are keyed by **Ed25519 public key** (hex-encoded, 64 characters). This is the campfire identity — every agent on a campfire has one. Forge's SpendingStore uses a partition key (string) and range key (string). For scrip:

```
PartitionKey: hex(Ed25519_pubkey)    // 64-char hex string
RangeKey:     "scrip:balance"        // current balance
```

This parallels Forge's existing pattern where `PartitionKey = SHA256(bearer_token)` and `RangeKey = DailyBudgetKey(date)`. The difference: DontGuess uses Ed25519 pubkeys (campfire identity) instead of API key hashes (Forge identity). They are different identity planes — an agent may have both.

### SpendingStore Implementation: CampfireScripStore

DontGuess implements Forge's `SpendingStore` interface with a campfire-backed store:

```go
type CampfireScripStore struct {
    campfire cf.Campfire  // the exchange campfire
    cache    sync.Map     // in-memory balance cache
}

// GetBudget returns the agent's scrip balance.
// pk = Ed25519 pubkey hex, rk = "scrip:balance"
func (s *CampfireScripStore) GetBudget(ctx context.Context, pk, rk string) (int64, string, error)

// DecrementBudget atomically decrements scrip balance.
// Returns ErrBudgetExceeded if balance < amount.
func (s *CampfireScripStore) DecrementBudget(ctx context.Context, pk, rk string, amount int64, etag string) (int64, string, error)

// AddBudget atomically adds scrip to balance.
func (s *CampfireScripStore) AddBudget(ctx context.Context, pk, rk string, amount int64, etag string) (int64, string, error)

// SaveReservation / GetReservation / DeleteReservation — reservation storage for in-flight operations.
```

**Balance cache:** The in-memory cache is populated from campfire message log on startup (deriving balance from the sequence of scrip operations). ETag is a monotonic counter incremented on every mutation. CAS semantics prevent concurrent corruption.

**Campfire as audit trail:** Every balance mutation writes a campfire message (the convention operation). The campfire log is the complete, immutable audit trail. The in-memory cache is a materialized view — it can always be reconstructed from the log.

### Forge's Pre-Decrement Model Applied to Scrip

The existing `SpendingLimiter` flow maps directly:

| Forge Flow | Scrip Flow | When |
|------------|------------|------|
| `PreDecrement(key, estimated)` | Pre-decrement buyer's scrip by `price + fee` | At `buy` operation |
| `Adjust(reservation, actual)` | Adjust if final settlement differs from estimate | At `settle` — if price changed between match and settle |
| `Refund(reservation)` | Full refund of pre-decremented amount | At `dispute-refund` — buyer gets full refund |

**Key insight:** Forge's reservation system provides exactly the escrow semantics the exchange needs. A reservation IS an escrow hold. The `reservationID` returned by PreDecrement is the escrow identifier referenced in match and settle operations.

### Metering Events

Every scrip movement emits a `meter.UsageEvent`-compatible record for Forge's billing rollup. Extended fields for scrip:

```go
type ScripEvent struct {
    // Identity
    FromKey    string    // Ed25519 pubkey of sender (empty for mint)
    ToKey      string    // Ed25519 pubkey of recipient (empty for burn)

    // Operation
    Operation  string    // mint | put-pay | buy-hold | match-transfer | settle-residual | burn-fee | assign-pay | dispute-refund
    Amount     int64     // scrip amount in micro-tokens (1 token = 1_000_000 micro)

    // References
    CampfireID string   // exchange campfire
    MessageID  string   // the convention message that triggered this event
    ResultHash string   // SHA-256 of the cached inference result (links to inventory)

    // Timing
    Timestamp  time.Time

    // RPT attribution (inherited from buyer/seller context)
    BeadID     string
    SessionID  string
    AgentType  string
    Project    string
}
```

**Denomination:** Scrip is denominated in inference token cost. 1 scrip = 1 token worth of inference at provider list price. Stored as micro-tokens (int64) to avoid floating-point accumulation errors. 1 scrip = 1,000,000 micro-scrip. This mirrors Forge's `USDToMicro` pattern but in token-cost units, not USD.

### Forge API Extensions Needed

DontGuess needs three capabilities beyond Forge's current API:

**1. Scrip Balance Query**

```
GET /v1/scrip/balance?key=<ed25519_pubkey_hex>
Response: { "key": "...", "balance": 1500000000, "unit": "micro-token", "as_of": "2026-03-27T..." }
```

**2. Scrip Transaction History**

```
GET /v1/scrip/history?key=<ed25519_pubkey_hex>&since=<timestamp>&limit=50
Response: { "events": [ ScripEvent... ], "cursor": "..." }
```

**3. Scrip Audit Endpoint**

```
GET /v1/scrip/audit
Response: {
    "total_supply":    <int64>,   // all scrip ever minted
    "total_burned":    <int64>,   // all scrip ever burned (fees)
    "circulating":     <int64>,   // supply - burned
    "active_balances": <int>,     // number of non-zero balances
    "as_of":           "<timestamp>"
}
```

These are DontGuess-specific endpoints, not Forge core. They live in DontGuess's exchange server, not in Forge itself. Forge provides the SpendingStore interface and the pre-decrement machinery; DontGuess provides the domain-specific API layer on top.

---

## Adversarial Analysis

### Economic Attacks

**E1: Inflation via Sybil Mint**

*Attack:* Attacker creates many identities, each purchasing minimal x402 scrip, then uses labor assignments to multiply their balance.

*Mitigation:* (a) No free starter balance — every scrip unit requires real x402 payment or verified labor completion. (b) Labor assignments are operator-controlled — the operator decides who gets work and validates completion. An attacker cannot self-assign labor. (c) x402 has real cost (USDC), so Sybil minting has linear cost.

*Residual risk:* If the operator's labor validation is weak (auto-approve), an attacker can earn scrip for garbage work. Mitigation: validation tasks use cross-agent convergence (3+ agents must agree on the result). This is the heritage "ungameable trust signal."

**E2: Wash Trading for Residuals**

*Attack:* Seller puts low-quality inference, then uses a second identity to buy it repeatedly, earning residuals that exceed the purchase cost.

*Mitigation:* (a) Matching fees are burned on every transaction. Each buy-sell cycle destroys scrip. Wash trading is net-negative for the attacker. (b) The pricing engine detects single-buyer patterns (Layer 3 behavioral signals). A result purchased only by the seller's alt-account does not build reputation. (c) Residual rates are set by the exchange, not the seller. The exchange can cap residuals at a percentage that makes wash trading unprofitable after fees.

*Numeric example:* Seller puts result, gets 70 scrip (70% of 100-token cost). Buyer pays 90 scrip + 10 scrip fee (burned). Seller gets 9 scrip residual (10% of price). Net per cycle: attacker spends 90+10=100, gets 70+9=79 back. Loss: 21 scrip per cycle. Wash trading loses money.

**E3: Cornering — Monopolize a Domain**

*Attack:* Agent floods the exchange with cached inference for a narrow domain, becoming the only seller. Then raises implicit value by being the sole source.

*Mitigation:* (a) The exchange owns the inventory and sets the price — sellers cannot set prices. (b) The pricing engine's Layer 3 (market novelty) penalizes low-diversity markets. A domain with one seller gets lower prices than one with many. (c) The operator can commission labor (freshness checks, alternative inference) to break monopolies.

**E4: Counterfeit Scrip — Forging Balance**

*Attack:* Agent sends a campfire message claiming a balance credit without a corresponding debit elsewhere.

*Mitigation:* (a) Balance is derived from the signed message log, not self-reported. Only messages signed by the exchange operator identity can credit balances. Agent messages can debit (spend) but not credit. (b) The CampfireScripStore reconstructs balances from the message log — a forged credit message would need the operator's Ed25519 private key to be accepted.

**E5: x402 Arbitrage Across Operators**

*Attack:* Buy scrip on Operator A (cheap rate), sell inference to Operator A, buy inference from Operator A, then... but scrip is local. Cannot move scrip between operators.

*Mitigation:* D4 (scrip is local per operator) eliminates this attack entirely. Cross-operator value transfer goes through x402 (USDC), which has a market price, not an arbitrageable scrip-scrip exchange rate.

### Trust Attacks

**T1: Operator Minting Without x402 Backing**

*Attack:* A rogue operator mints unlimited scrip without corresponding x402 inflow, inflating supply.

*Mitigation:* (a) For single-operator use (v1), the operator IS the trust root. They have the same power as a central bank. This is accepted. (b) For federation (v2), the x402 audit endpoint exposes total_supply and total_minted. Federated operators can verify each other's scrip/x402 ratio. An operator with implausible ratios is untrusted. (c) The campfire log is immutable and auditable — every mint has a corresponding x402 payment message. Third parties can verify the log.

**T2: Operator Seizing Agent Balances**

*Attack:* Operator modifies an agent's balance downward without a legitimate convention operation.

*Mitigation:* (a) All balance mutations are convention messages on the campfire. A seizure would be a visible, signed message. The agent (and any observer) can detect it. (b) The convention defines the valid operations that can debit an agent (buy, fee, dispute-charge). Any other debit is a convention violation, detectable by any participant. (c) Agents can choose a different operator if trust is violated — their inference results (content) are portable, even if scrip is not.

**T3: Replay Attack — Replaying a Settle Message**

*Attack:* Replay a historical settle message to credit a seller's balance a second time.

*Mitigation:* (a) Campfire messages have unique IDs and are append-only. The CampfireScripStore tracks processed message IDs. A replayed message is detected and rejected. (b) Campfire's antecedent chain (each message references its predecessor) makes insertion of duplicate messages detectable.

### Federation Attacks (v2 Scope — Documented for Completeness)

**F1: Cross-Operator Scrip Laundering**

*Attack:* Rogue operator creates scrip from nothing, uses x402 to move value to a legitimate operator's exchange.

*Analysis:* x402 is the firewall. The rogue operator must spend real USDC to buy scrip on the legitimate exchange. The counterfeit scrip never leaves the rogue operator's domain. The USDC cost is real. This is not an attack on the DontGuess protocol — it is standard financial fraud on the x402 layer.

**F2: Federated Balance Inflation**

*Attack:* Two colluding operators create scrip on each other's exchanges without x402 backing.

*Analysis:* D4 prevents this in v1. In v2, federation would require mutual audit of x402 inflow logs. Operators that cannot prove scrip backing are excluded from the federation trust ring.

---

## Convention Operations Table

Operations that move scrip are listed here. The message format follows the convention extension pattern (`convention:operation`). Full operation definitions are in `docs/convention/scrip-operations.md`.

| Operation | Convention Tag | Signer | Scrip Effect |
|-----------|---------------|--------|-------------|
| `scrip:mint` | `dontguess:scrip-mint` | operator | +balance to target |
| `scrip:burn` | `dontguess:scrip-burn` | operator | -balance (destroyed) |
| `scrip:put-pay` | `dontguess:scrip-put-pay` | operator | +balance to seller |
| `scrip:buy-hold` | `dontguess:scrip-buy-hold` | exchange | -balance from buyer (escrow) |
| `scrip:settle` | `dontguess:scrip-settle` | exchange | +residual to seller, burn fee |
| `scrip:assign-pay` | `dontguess:scrip-assign-pay` | operator | +balance to laborer |
| `scrip:dispute-refund` | `dontguess:scrip-dispute-refund` | operator | +balance to buyer |
| `scrip:rate-publish` | `dontguess:scrip-rate` | operator | (no movement — publishes x402 rate) |

---

## Open Questions

1. **Residual settlement cadence.** Medium-loop (1hr) batch vs per-transaction. Batch is cheaper (fewer campfire messages) but delays seller earnings. Per-transaction is simpler but noisy. Recommendation: batch at medium-loop cadence; sellers see "pending residuals" in their balance query.

2. **Escrow timeout.** If a buy is pre-decremented but never matched (no suitable inventory), how long before automatic refund? Recommendation: 5 minutes, configurable per operator.

3. **Operator commission structure.** The spread between wholesale (put) and retail (buy) prices is operator revenue. Should this be a fixed percentage or part of the pricing engine's output? Recommendation: pricing engine sets retail price; operator's margin is implicit (retail - wholesale - residual).

4. **Scrip precision.** Micro-tokens (1e6 per token) provides 6 decimal places. Is this sufficient for sub-token fractional pricing? Forge uses micro-dollars (1e6 per dollar). Matching precision avoids conversion rounding.

5. **Labor task taxonomy.** What types of assigned work exist? Initial set: `validate` (verify cached inference correctness), `compress` (reduce context size), `freshen` (re-run stale inference). Each has a different scrip reward. The reward schedule is operator-configured.
