# Design: Federation — Cross-Operator Liquidity with Trust

**Date:** 2026-03-28
**Author:** Convention Designer (Opus)
**Status:** Draft
**Depends on:** core-operations.md v0.2, scrip-operations.md v0.1, campfire protocol v0.3

---

## 1. Problem Statement

Anyone can operate a DontGuess exchange. Each operator runs their own campfire, maintains their own inventory, manages their own scrip ledger, and builds their own reputation scores. This works well for a single exchange, but the marketplace is fragmented: a buyer on Exchange A cannot discover inventory on Exchange B.

Federation lets two or more operators share inventory so that a buy request on Exchange A can match a put from Exchange B. This increases liquidity for buyers (more inventory to search) and reach for sellers (more potential buyers for their content).

The problem is trust. Operators are independent entities with independent scrip ledgers and independent reputation models. A rogue operator can poison the network by flooding garbage inventory, inflating local reputations, or weaponizing disputes. Federation must provide liquidity benefits while containing the blast radius of a malicious operator.

### What federation is NOT

- **Not a unified ledger.** Scrip balances remain local to each operator. No cross-operator scrip transfer. This avoids money transmission implications and prevents one operator from printing money on another's ledger.
- **Not a unified reputation system.** Operator A's reputation scores are Operator A's opinion. Operator B can see them but applies its own trust overlay before presenting them to buyers.
- **Not a global directory.** There is no central registry of operators. Discovery is peer-to-peer via campfire beacons.
- **Not mandatory.** An operator can run a standalone exchange indefinitely. Federation is opt-in.

---

## 2. Design Principles

**F1. Trust is bilateral and revocable.** Every federation relationship is a pair of operators who mutually agree to share inventory. Either party can revoke at any time. There are no transitive trust grants -- if A federates with B and B federates with C, A does not automatically see C's inventory.

**F2. Scrip stays local.** Cross-operator transactions settle bilaterally. Each operator handles their own side of the ledger. The buyer's operator deducts scrip; the seller's operator credits residual. The operators reconcile via an out-of-band settlement channel (x402 or bilateral agreement).

**F3. Reputation is advisory, not authoritative.** An operator's reputation scores travel with federated inventory as metadata. The receiving operator may display them, discount them, or ignore them entirely. The receiving operator's local reputation overlay (based on cross-operator match outcomes observed locally) is the authoritative signal for local buyers.

**F4. Federation is not free.** Operators charge each other for cross-operator matches. This creates economic friction that makes inventory spam expensive and provides compensation for the receiving operator's matching infrastructure.

**F5. Blast radius containment.** A rogue operator can damage its own participants but cannot unilaterally damage other operators. All federation operations have rate limits, trust gates, and revocation mechanisms that allow a receiving operator to sever the relationship instantly.

**F6. Campfire is the substrate.** Federation uses campfire primitives: beacons for discovery, messages for inventory sharing, provenance for auditing, trust (vouch/revoke) for operator reputation. No new transport or identity layer.

---

## 3. Federation Protocol

### 3.1 Discovery

Operators discover each other via campfire beacons. An exchange operator publishes a beacon tagged `dontguess:exchange` on well-known beacon channels (DNS TXT, git repo, HTTP well-known). The beacon contains:

- Operator's campfire public key (identity)
- Transport configuration (how to connect)
- Exchange metadata: supported convention version, inventory size, active buyer count, uptime

Beacons are tainted (per campfire protocol). An operator discovering a beacon knows WHO is advertising and that THEY authored it, but not whether the claims are honest. Discovery is not trust.

### 3.2 Federation Agreement

Two operators establish a federation relationship through a bilateral `federation:propose` / `federation:accept` handshake on a shared campfire. The agreement specifies:

- **Inventory scope**: which content types and domains to share (or "all")
- **Trust floor**: minimum local seller reputation for an entry to be shared (prevents low-quality inventory from leaking across operators)
- **Settlement terms**: how cross-operator financial reconciliation works (x402 reference, bilateral credit, or deferred netting)
- **Rate limits**: maximum inventory messages per hour in each direction
- **Cross-operator matching fee**: the fee the originating operator pays the remote operator for each cross-operator match (in addition to the buyer's matching fee)

The agreement is a campfire message signed by both operators. It is the root of trust for the federation relationship.

### 3.3 Inventory Sharing

Once federated, operators share inventory by forwarding `put` metadata (not content) as `federation:inventory-offer` messages on the shared campfire. These messages contain:

- Entry ID (unique within the originating exchange)
- Description, content hash, content type, domains, token cost, content size
- Seller reputation score (as assessed by the originating operator)
- Originating operator's public key
- Put message ID on the originating exchange (for provenance)
- Embedding vector (for matching -- tainted, receiving operator may re-compute)

Content itself is NOT shared. Only metadata travels. Content delivery happens only after a cross-operator match, directly from the originating operator's storage to the buyer.

Inventory messages are rate-limited per the federation agreement. An operator that exceeds the rate limit is throttled, then disconnected if persistent.

### 3.4 Cross-Operator Match

When a buyer on Exchange A submits a `buy` request, Exchange A searches both its local inventory AND federated inventory from Exchange B. If a federated entry matches:

1. Exchange A presents the match to the buyer with both reputation scores: Exchange B's local score (advisory) and Exchange A's cross-operator trust overlay (authoritative for this buyer).
2. The match result includes a `federation:origin` field identifying the originating operator and entry.
3. If the buyer accepts (after preview), Exchange A sends a `federation:match-request` to Exchange B via the shared campfire.
4. Exchange B validates the request (entry still exists, not expired, seller not banned) and responds with `federation:match-confirm` or `federation:match-reject`.
5. On confirm, Exchange B delivers content directly to the buyer (encrypted to the buyer's key) and the settlement flow begins.

### 3.5 Bilateral Settlement

Cross-operator settlement is bilateral. Each operator handles their own ledger:

**Buyer's side (Exchange A):**
- Buyer's scrip is escrowed via `scrip:buy-hold` (same as local match)
- On completion, buyer's escrow is settled: matching fee burned, exchange revenue retained, cross-operator fee debited to Exchange A's inter-operator account

**Seller's side (Exchange B):**
- Exchange B credits the seller's residual via `scrip:put-pay` or `scrip:settle` (same as local match)
- Exchange B receives a cross-operator fee credit to its inter-operator account

**Inter-operator reconciliation:**
- Each operator maintains a running tally of cross-operator fees owed to/from each federated partner
- Reconciliation happens periodically (e.g., daily) via x402 payments or bilateral credit
- If an operator's outstanding balance exceeds a threshold, the federation relationship is paused until settled
- The `federation:reconcile` message records each reconciliation event on the shared campfire

**No scrip crosses operator boundaries.** The buyer spends local scrip on Exchange A. The seller earns local scrip on Exchange B. The cross-operator fee is an obligation between operators, settled out-of-band.

### 3.6 Federation Revocation

Either operator can revoke federation at any time by sending `federation:revoke` on the shared campfire. Effects:

- Federated inventory from the revoked operator is immediately removed from match results
- In-flight cross-operator matches complete normally (escrow already committed)
- Outstanding inter-operator balance must still be settled (revocation does not cancel debts)
- The shared campfire remains readable for audit purposes but no new inventory or match messages are accepted

---

## 4. Adversarial Analysis

### A1. Garbage inventory flooding (spam puts)

**Attack:** A rogue operator floods federated partners with low-quality inventory to pollute their match results.

**Mitigations:**
1. **Trust floor in federation agreement.** Only entries above the agreed seller reputation threshold are shared. A rogue operator can inflate local reputations (see A2) but cannot bypass the receiving operator's trust overlay.
2. **Rate limits.** The federation agreement caps inventory messages per hour. Exceeding the limit triggers throttle, then disconnect.
3. **Cross-operator reputation overlay.** The receiving operator tracks cross-operator match outcomes independently. If entries from Operator B consistently produce disputes or buyer-reject outcomes, Operator B's cross-operator trust score drops, and its inventory is down-ranked or excluded.
4. **Economic friction.** Cross-operator matching fees mean the originating operator pays per match. Spam inventory that gets matched costs the spammer money.
5. **Revocation.** The receiving operator can instantly revoke federation.

**Residual risk:** A sophisticated attacker creates inventory that looks legitimate in metadata but delivers garbage content. The preview model catches this at the buyer level (buyer sees a preview before purchasing). The cross-operator trust overlay catches it at the operator level (patterns of buyer-reject accumulate).

### A2. Reputation inflation

**Attack:** A rogue operator inflates seller reputation scores on their local exchange (Sybil buys, fake convergence events) to get inventory past trust floors on federated partners.

**Mitigations:**
1. **Reputation is advisory.** The receiving operator sees the originating operator's reputation score but applies its own cross-operator trust overlay. Inflated scores are treated as one input, not the final word.
2. **Cross-operator outcome tracking.** The receiving operator tracks actual outcomes (complete vs. dispute vs. reject) for each originating operator. Over time, the receiving operator's local experience overrides the advertised reputation.
3. **Convergence diversity check.** Cross-agent convergence signals are weighted by buyer diversity. If a remote operator's entries only have convergence from keys that never appear on the local exchange, the convergence signal is discounted.
4. **Federation reputation decay.** An operator's cross-operator trust score starts at a baseline (e.g., 50/100) and adjusts based on observed outcomes. A new federation partner does not start with full trust.

**Residual risk:** An attacker with a large number of independent-looking agents could farm real convergence signals. This is expensive (each fake buyer spends real scrip) and slow (convergence requires 3+ independent agents). The economic cost of the attack exceeds the economic benefit for all but the most determined adversaries.

### A3. Dispute weaponization

**Attack:** A rogue operator encourages its buyers to file frivolous disputes on cross-operator matches, draining seller reputation on the originating exchange.

**Mitigations:**
1. **Disputes are local.** A dispute on Exchange A affects the buyer's and seller's reputation on Exchange A only. Exchange B (where the seller is local) does not automatically import disputes from Exchange A.
2. **Cross-operator dispute rate tracking.** If buyers from Operator A consistently dispute content from Operator B at a rate significantly above baseline, Operator B can revoke federation with Operator A.
3. **Preview model reduces dispute surface.** The buyer sees a preview before purchasing. Disputes after preview are structurally rare because the buyer already evaluated the content.
4. **Small-content auto-refund is capped.** Small-content disputes (< 500 tokens, auto-refund) impose a -3 reputation penalty per refund. An operator whose buyers systematically exploit auto-refunds has its cross-operator trust score degraded.
5. **Dispute rate feeds into cross-operator trust overlay.** The receiving operator's trust model includes dispute frequency per originating operator. High dispute rates from a partner trigger automatic trust reduction.

**Residual risk:** An operator could slowly drip disputes at a rate just below detection thresholds. Over a long time, this could erode a seller's reputation on the attacking operator's exchange. The seller's home operator can detect this pattern (seller's dispute rate on Exchange A is anomalous vs. Exchange B, C, D) and raise it in reconciliation or revoke federation.

### A4. Man-in-the-middle content substitution

**Attack:** The originating operator claims to deliver content for a match but substitutes different (inferior or malicious) content.

**Mitigations:**
1. **Content hash verification.** The `content_hash` from the original put is included in the `federation:inventory-offer` and the `federation:match-confirm`. The buyer verifies the delivered content against this hash before sending `settle(complete)`. A mismatch triggers a dispute.
2. **Hash is part of the provenance chain.** The content hash appears in signed messages from the originating operator. Post-hoc denial is cryptographically impossible.
3. **Preview hash consistency.** In the preview flow, the preview chunks hash must be derivable from the full content hash. This prevents serving a legitimate preview but substituting content on full delivery.

**Residual risk:** None for content integrity. The hash verification is deterministic. The only risk is if the hash algorithm itself is broken (SHA-256 preimage), which is outside our threat model.

### A5. Free-riding (asymmetric federation)

**Attack:** An operator joins federation to access inventory from partners but contributes little or no inventory of their own.

**Mitigations:**
1. **Cross-operator matching fee.** The receiving operator pays per match. An operator that only consumes pays fees to partners.
2. **Bilateral agreement terms.** The federation agreement can specify minimum inventory contribution or reciprocal match ratios. Failure to meet terms is grounds for revocation.
3. **Organic incentive.** An operator with little inventory has low match rates, making their exchange less attractive to buyers. Market pressure incentivizes inventory investment.

**Residual risk:** Acceptable. Free-riding at small scale is a feature, not a bug -- it lets new operators bootstrap inventory by offering their buyers access to established exchanges.

### A6. Operator impersonation

**Attack:** An attacker creates a campfire with a beacon mimicking a legitimate operator to intercept federation proposals.

**Mitigations:**
1. **Cryptographic identity.** The operator's public key IS their identity. A beacon is signed by the campfire's private key. An impersonator cannot produce a valid signature for the legitimate operator's key.
2. **Federation proposals reference specific keys.** The `federation:propose` message includes the target operator's public key. Even if an attacker intercepts the beacon, they cannot accept a proposal addressed to someone else's key.
3. **Out-of-band key verification.** Operators can verify each other's keys through side channels (website, DNS, personal contact) before accepting federation proposals.

**Residual risk:** None for the cryptographic identity model. Social engineering (e.g., convincing an operator to federate with an attacker posing as a legitimate entity via out-of-band channels) is outside the protocol's scope.

---

## 5. Convention Operations

Federation adds seven new convention operations under the `federation:` prefix.

| Operation | Sender | Direction | Purpose |
|-----------|--------|-----------|---------|
| `federation:propose` | Operator A | A -> shared campfire | Propose federation terms |
| `federation:accept` | Operator B | B -> shared campfire | Accept federation (forms agreement) |
| `federation:inventory-offer` | Originating operator | Origin -> shared campfire | Share inventory metadata |
| `federation:match-request` | Receiving operator | Receiver -> shared campfire | Request cross-operator match |
| `federation:match-confirm` | Originating operator | Origin -> shared campfire | Confirm or reject match request |
| `federation:revoke` | Either operator | Either -> shared campfire | Revoke federation |
| `federation:reconcile` | Either operator | Either -> shared campfire | Record inter-operator settlement |

### Tag vocabulary

All federation tags use the `federation:` prefix.

| Tag | Cardinality | Description |
|-----|-------------|-------------|
| `federation:propose` | exactly_one | Federation proposal message |
| `federation:accept` | exactly_one | Federation acceptance message |
| `federation:inventory-offer` | exactly_one | Shared inventory entry |
| `federation:match-request` | exactly_one | Cross-operator match request |
| `federation:match-confirm` | exactly_one | Match confirmation or rejection |
| `federation:revoke` | exactly_one | Federation revocation |
| `federation:reconcile` | exactly_one | Settlement reconciliation record |
| `federation:domain:*` | zero_to_many | Domain scope of federation agreement |
| `federation:origin:*` | exactly_one | Originating operator key (on inventory/match msgs) |

---

## 6. Cross-Operator Trust Overlay

Each operator maintains a **cross-operator trust model** that is derived from observed outcomes, not from the partner's claimed reputation.

### Inputs

| Signal | Source | Weight | Description |
|--------|--------|--------|-------------|
| Match completion rate | Local observations | High | What fraction of cross-operator matches from this partner settle as `complete`? |
| Dispute rate | Local observations | High | What fraction result in disputes (any type)? |
| Buyer-reject rate | Local observations | Medium | What fraction of preview-then-reject outcomes? |
| Inventory freshness | Metadata | Low | Are shared entries fresh or mostly expired/stale? |
| Reconciliation reliability | Bilateral | Medium | Does the partner settle inter-operator balances on time? |

### Trust Score

Cross-operator trust starts at 50 (on a 0-100 scale) for a new federation partner. It adjusts:

- **+2** per completed cross-operator match without dispute
- **-5** per cross-operator dispute resolved in buyer's favor
- **-1** per buyer-reject after preview
- **-10** if reconciliation payment is overdue > 24 hours
- **+5** per on-time reconciliation event

At **trust < 20**, the operator auto-revokes federation and sends `federation:revoke`.

At **trust < 40**, cross-operator inventory is excluded from match results (soft suspension) but the federation agreement remains active.

### Application

When presenting federated match results to a local buyer, the receiving operator:

1. Applies its own cross-operator trust overlay to the entry's composite score
2. Shows both scores: "Seller reputation on [origin]: 85. Cross-operator trust for [origin]: 72."
3. Applies Layer 0 correctness gate using LOCAL outcome data (entries that failed locally are excluded regardless of remote reputation)

---

## 7. Settlement Flow Detail

### Happy path: cross-operator match completes

```
Exchange A (buyer's)                    Exchange B (seller's)
    |                                         |
    |  1. federation:match-request  --------> |
    |  <-------- 2. federation:match-confirm  |
    |                                         |
    |  3. scrip:buy-hold (local)              |
    |                                         |
    |  4. settle(preview-request) ----------> |  (if content >= 500 tokens)
    |  <-------- 5. settle(preview)           |
    |                                         |
    |  6. settle(buyer-accept) ------------> |
    |  <-------- 7. settle(deliver)           |  (content encrypted to buyer's key)
    |                                         |
    |  8. settle(complete) ----------------> |
    |                                         |
    |  9. scrip:settle (local)                | 10. scrip:settle (local -- residual to seller)
    |                                         |
    |  11. Inter-operator fee accrues         | 12. Inter-operator fee credit accrues
    |                                         |
    |  [periodic] federation:reconcile        |
```

Steps 4-8 use the existing settlement protocol (same message format) but messages flow through the shared federation campfire rather than the local exchange campfire.

### Failure cases

| Failure | Detection | Resolution |
|---------|-----------|------------|
| Match-confirm timeout | No response within 60s | Exchange A returns no-match to buyer; -1 trust |
| Content delivery timeout | No deliver within 120s after buyer-accept | Auto-refund via scrip:dispute-refund; -2 trust |
| Hash mismatch | Buyer verifies hash post-delivery | Dispute; -5 trust to originating operator |
| Reconciliation default | Balance exceeds threshold, no payment | Soft suspension at overdue +12h; revoke at +48h |

---

## 8. Open Questions

**Q1. Transitive federation.** Should A-B and B-C federation imply A can discover C's inventory via B? Current design says no (explicit bilateral only). Transitive federation adds liquidity but also adds trust propagation complexity and a larger attack surface. Defer to v2.

**Q2. Multi-operator redundancy.** If the same content is available on multiple federated operators, should the buyer see deduplicated results? Content hash makes deduplication straightforward, but price may differ across operators. Current design: show all, let the buyer choose.

**Q3. Reconciliation currency.** The design specifies x402 (USDC) for inter-operator settlement. Should the protocol support alternative settlement rails? Current design: x402 is the default; bilateral agreement can specify alternatives.

**Q4. Federation campfire lifecycle.** Who creates the shared campfire? Current design: the proposing operator creates it as part of `federation:propose`. Should we support multi-operator campfires (3+ operators on one campfire) vs. strictly bilateral? Current design: bilateral only. Multi-operator adds complexity (N-way trust, N-way reconciliation).

**Q5. Inventory withdrawal.** When a put expires or is withdrawn on the originating exchange, how does the federated partner learn? Current design: the originating operator sends a tombstone message (`federation:inventory-offer` with `withdrawn: true`). Need to define TTL for inventory offers and what happens if the originating operator goes offline.

**Q6. Cross-operator preview.** The preview flow adds latency in cross-operator matches because preview content must be fetched from the originating operator. Should federated inventory offers include pre-computed preview chunks? This would increase message size but eliminate a round trip.
