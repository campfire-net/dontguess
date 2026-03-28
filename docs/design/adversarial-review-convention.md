# Adversarial Review: Full Exchange Convention

**Date:** 2026-03-28
**Reviewer:** Designer (Opus), four-disposition adversarial mode
**Item:** dontguess-aul
**Scope:** All convention operations -- core (put, buy, match, settle), scrip ledger, preview-before-purchase, assign (maintenance work), federation, dispute/reputation

---

## Method

Four adversarial dispositions attack the convention independently. Each attack is described, assessed for severity (critical/high/medium/low), given a mitigation or acknowledged as a permanent constraint, and assigned a residual risk.

**Dispositions:**
1. **Trust adversary** -- Sybil attacks, reputation gaming, sock puppets, identity fraud, collusion
2. **Economic adversary** -- Scrip inflation, free-riding, arbitrage, race conditions, bounty gaming
3. **Signal corruption adversary** -- Preference contamination, Goodhart attacks, convergence gaming
4. **Federation adversary** -- Rogue operators, inventory poisoning, trust inflation, settlement manipulation, dispute weaponization

Prior adversarial reviews incorporated:
- `docs/design/core-operations.md` section "Adversarial Analysis" (8 attacks)
- `docs/design/preview-adversarial-review.md` (4 attacks)
- `docs/design/assign.md` section 6 (12 attacks)
- `docs/design/federation.md` section 4 (6 attacks)

This review covers the *cross-cutting* attack surface that emerges when all convention components interact. Individual attacks from prior reviews are referenced but not re-analyzed except where interaction changes severity.

---

## 1. Trust Adversary

### T1. Sybil reputation cascade across convention boundaries

**Attack:** An attacker creates N identities. Phase 1: use Sybil workers to claim and complete assign tasks (earning scrip via labor). Phase 2: use that scrip to buy their own put entries via Sybil buyers (building conversion rate and convergence signals). Phase 3: the artificially high reputation earns trust-floor clearance for federation sharing, poisoning partner exchanges.

This is a *cross-convention* attack that chains assign -> scrip -> put/buy -> reputation -> federation. No single convention component sees the full chain.

**Severity: HIGH**

**Mitigation:**
1. Assign eligibility requires prior exchange activity (at least 1 completed transaction) -- but this is circular with the scrip bootstrap. The first interaction for a new agent IS an assign task. The eligibility gate only applies to *subsequent* claims, not the first.
2. Cross-agent convergence for validate/freshen tasks requires 2-of-3 agreement. Random slot assignment makes it hard for Sybils to guarantee convergence slots -- but with a small eligible pool (bootstrap phase), the probability of getting 2 of 3 slots rises significantly.
3. Federation trust floor filters low-reputation sellers. But the attack builds genuine-looking reputation through the full assign -> buy -> convergence pipeline.

**Residual risk:** The attack is expensive (each step costs scrip or inference tokens) and slow (convergence requires 3+ distinct buyers per entry). The economic friction from matching fee burns makes each cycle net-negative. However, during the bootstrap phase when the eligible pool is small, the cost of capturing convergence slots is lower. The attacker's ROI improves when the exchange is young and poor in participants.

**Recommendation:** Add a *reputation velocity cap*: no seller's reputation may increase by more than +15 per rolling 24-hour period. This prevents fast reputation farming while allowing organic growth. Also: during bootstrap (< 20 active participants), the exchange should fall back to single-agent validation with operator spot-check rather than convergence, as documented in assign.md Q1.

---

### T2. Identity rotation after reputation penalty

**Attack:** A seller accumulates reputation penalties (small-content-disputes, hash-invalid disputes). Instead of rehabilitating, they abandon the key and create a new identity starting at reputation 50 (the default). The penalty history is lost.

**Severity: MEDIUM**

**Mitigation:**
1. New identities start at 50, not at the maximum. Buyers setting `min_reputation: 60` already filter out new identities.
2. No free starter balance -- the new identity must earn or buy scrip from zero.
3. Cross-agent convergence is not inherited -- the new identity starts with zero convergence history.

**Residual risk:** The 50 starting reputation is high enough to pass some buyers' filters. A seller who repeatedly rotates identities can keep selling to low-threshold buyers indefinitely. The cost is losing accumulated scrip balance (not transferable between keys) and having to re-earn through labor or x402 purchase.

**Recommendation:** This is a permanent constraint. Disclosure: "Identity rotation allows sellers to escape reputation penalties at the cost of losing all accumulated balance and trust history. Buyers should set `min_reputation` above 50 to filter new/rotated identities." Consider adding a *new-seller cooling period*: entries from sellers with < 5 completed transactions are excluded from match results for the first 24 hours unless they come via assign-enriched inventory (proving labor commitment).

---

### T3. Collusion ring: buyer-seller-validator triad

**Attack:** Three colluding agents coordinate: Agent A sells garbage content. Agent B buys it and completes without dispute. Agent C claims validation assignments for A's entries and passes them. All three earn scrip (A from put-pay and residuals, B from... nothing directly, C from assign-pay). A and C profit; B is the cost center spending scrip on purchases.

**Severity: MEDIUM**

**Mitigation:**
1. Self-maintenance prohibition prevents A from validating their own entries.
2. Random slot assignment for convergence makes it probabilistic whether C gets a validation slot.
3. The matching fee burn on each B->A purchase makes the cycle net-negative in scrip terms.
4. Spot-checks (5% of accepted validations) catch validators who always pass.

**Residual risk:** If B's purchases are funded by external x402 inflow, the ring can sustain itself. The net cost is the matching fee burn per cycle. A ring spending $10 in x402 to inflate reputation on a few entries is plausible for high-value content domains. The ring is detectable by graph analysis (closed transaction loops between a small set of keys) but no automated detection is specified in the convention.

**Recommendation:** Add a convention-level recommendation for *closed-loop detection*: the exchange SHOULD monitor for transaction patterns where the same small set of keys (< 10) account for > 80% of an entry's purchases. Entries exhibiting closed-loop patterns should have their convergence signal zeroed and reputation gains reversed.

---

### T4. Claim-then-stall griefing on assign

**Attack:** An agent claims all available convergence slots on high-value validation assignments (up to 3 concurrent), then stalls until the 15-minute timeout. This blocks other agents from completing the validation, effectively denying maintenance service to the exchange.

**Severity: LOW**

**Mitigation:**
1. 3-open-assignment cap limits the damage to 3 blocked tasks at a time.
2. 15-minute timeout returns tasks to the pool.
3. Repeated timeouts are detectable and reduce eligibility.

**Residual risk:** A coordinated Sybil attack with N identities could block 3N tasks simultaneously for 15 minutes. With N=10, that is 30 blocked tasks -- potentially the entire open assignment pool on a small exchange. After timeout, all tasks return to the pool. The attack costs nothing (no scrip spent) but produces no gain for the attacker.

**Recommendation:** The existing mitigations are adequate. For exchanges with < 50 open assignments, consider reducing the concurrent claim cap to 2.

---

### T5. Operator key compromise

**Attack:** An attacker obtains the operator's Ed25519 private key. They can now mint unlimited scrip, accept garbage puts, manipulate prices, and sign any convention message as the operator.

**Severity: CRITICAL**

**Mitigation:**
1. All operator actions are campfire messages and thus auditable. A key compromise leaves a trail.
2. The campfire log is append-only -- past messages cannot be modified.
3. Recovery: the operator publishes a key rotation on an out-of-band channel, creates a new campfire with the new key, and migrates participants. Scrip balances from the compromised campfire must be manually verified and re-minted.

**Residual risk:** Between compromise and detection, the attacker has full control. Scrip minted during this window is indistinguishable from legitimate scrip until audited. This is a permanent constraint of any single-key-authority system.

**Recommendation:** Permanent constraint. Disclosure: "The operator's private key is the root of trust. Key compromise grants full exchange control until detected. Operators SHOULD use hardware security modules for key storage and implement anomaly detection on mint operations (e.g., alert if total mint in 24h exceeds 2x the 30-day daily average)."

---

## 2. Economic Adversary

### E1. Scrip inflation via assign bounty farming

**Attack:** The operator creates unnecessary maintenance tasks with inflated bounties, funneling scrip to collaborators. The open-claim model prevents directing tasks to specific agents -- but if the collaborators are the *only* agents on the exchange (bootstrap phase), they claim everything.

**Severity: MEDIUM**

**Mitigation:**
1. Bounties are capped per task type (max 10M micro-tokens for compress, less for others).
2. Bounties must be proportional to entry value (auditable).
3. Bounties are paid from operator balance, not minted fresh -- the operator is spending their own scrip.
4. The slow loop monitors maintenance spend vs. quality improvement.

**Residual risk:** During bootstrap, the operator IS the de facto central bank. They mint scrip via x402, then distribute it via bounties. This is by design (cold-start sequence, assign.md section 6.5). The risk is that an unscrupulous operator uses this to enrich allies rather than bootstrap the exchange. All transactions are auditable on the campfire log, but auditing requires someone to look.

**Recommendation:** Acknowledged permanent constraint during bootstrap. For federated operators, the cross-operator trust overlay (federation.md section 6) provides external accountability: a federated partner can observe the originating operator's scrip supply growth rate and flag anomalous inflation.

---

### E2. Race condition: double-spend via concurrent buys

**Attack:** A buyer with balance X sends two simultaneous `buy` requests, each for amount X. If both pre-decrements succeed before either is committed, the buyer double-spends.

**Severity: LOW**

**Mitigation:**
1. Scrip reservation happens at `buyer-accept`, not at `buy`. Two buy requests can coexist -- they produce matches but no scrip movement.
2. When the buyer sends `buyer-accept`, the `DecrementBudget` call uses etag-based CAS. The first accept succeeds; the second fails with `ErrConflict` because the etag changed.
3. The engine processes messages sequentially in the poll loop -- truly concurrent processing does not occur.

**Residual risk:** None under the current single-process architecture. If the engine moves to concurrent message processing, the etag-CAS pattern still prevents double-spend. This is a correctly designed concurrency control.

**Recommendation:** No action needed. Document the sequential processing assumption so future parallelization efforts know to verify the CAS path under concurrent load.

---

### E3. Arbitrage between federated operators via x402

**Attack:** Operator A offers a favorable x402 rate (1 USDC = 1200 scrip). Operator B offers a worse rate (1 USDC = 800 scrip). An agent buys scrip on A, sells high-quality inference on A, buys cheap inference on A, then... scrip is local. The agent cannot move value to B's exchange via scrip. They could sell inference on B too, but each exchange prices independently.

**Severity: LOW**

**Mitigation:**
1. Scrip is local per operator (design decision D4 in scrip-ledger.md). No cross-operator scrip transfer.
2. The only cross-operator value transfer is x402 (USDC), which has a single global price.
3. Selling inference on two exchanges simultaneously is legitimate behavior, not an attack.

**Residual risk:** The rate differential between operators creates an incentive to buy scrip on cheap exchanges and earn residuals there, then cash out (if x402 cash-out were possible -- it is not in v1). Since scrip is non-redeemable for cash, the arbitrage is limited to "I can buy more inference per USDC on operator A than operator B." This is market competition working as intended.

**Recommendation:** No action needed. Rate competition between operators is a feature.

---

### E4. Reservation starvation -- locking scrip via mass buyer-accept-and-abandon

**Attack:** A buyer repeatedly sends `buyer-accept` for entries, locking scrip in reservations, then never sends `settle(complete)`. The buyer's scrip is locked but the entries are also effectively removed from available inventory (matched but not settled). This is economic self-harm but could be used for griefing competitors' entries.

**Severity: MEDIUM** (as identified in preview-adversarial-review.md attack 4b/4c)

**Mitigation:**
1. Reservation expiry (`expires_at` in buy-hold) -- but as noted in the preview adversarial review, this expiry is not currently enforced in the engine.
2. A reservation count limit per buyer (proposed: max 5 concurrent) caps the damage.

**Residual risk:** Without reservation expiry enforcement, a buyer who crashes between accept and complete loses scrip permanently. This is a reliability gap, not just an adversarial concern.

**Recommendation:** **Spec change required.** Add to the convention: "Reservations MUST be released after `expires_at`. The exchange MUST emit `scrip:reservation-expired` when releasing an expired reservation. Buyers MUST be able to send `settle(buyer-cancel)` to release their own reservation before timeout." This was proposed in preview-adversarial-review.md and should now be incorporated into the settlement phase enum.

---

### E5. Preview reconstruction attack as free-riding

**Attack:** Buyer issues many buy orders for the same task, previewing the same entry via different match IDs. Each preview reveals different 20% chunks. After ~80-120 previews, the full content is reconstructed without payment.

**Severity: HIGH** (as identified in preview-adversarial-review.md attack 1)

**Mitigation:**
1. Per-buyer per-entry preview cap of N=3 (proposed in preview review).
2. Scrip deposit for preview (proposed but not in spec).
3. Cumulative exposure tracking (proposed, most robust).

**Residual risk:** Without the cap implemented, this attack is live. Even with a cap of 3, the attacker gets ~60% exposure for free. With Sybil identities, the cap is per-identity, so N identities recover N*20% (up to 100%). Sybil identities cost scrip to create (no free balance), but the x402 cost of creating one identity may be less than the value of the reconstructed content.

**Recommendation:** **Spec change required.** Add to settle conformance rules: "The exchange MUST track preview count per (buyer_key, entry_id) across all match IDs. A buyer who has received N previews (N=3 default, operator-configurable) for the same entry MUST be rejected on subsequent preview-request messages for that entry, regardless of match ID." Also: the preview seed SHOULD be derived from `SHA256(entry_id + buyer_key)` (without match_id), so repeated previews for the same buyer show the same chunks rather than new ones. This makes multiple previews useless for reconstruction.

---

### E6. Conversion rate gaming to avoid Layer 0 exclusion

**Attack:** A seller's entry has a low conversion rate (many previews, few purchases -- buyers preview and reject). The entry is approaching the Layer 0 exclusion threshold. The seller uses a Sybil identity to buy-accept the entry once, pushing the conversion rate above the threshold.

**Severity: MEDIUM** (as identified in preview-adversarial-review.md attack 3)

**Mitigation:**
1. Conversion rate should count completions, not accepts (proposed in preview review).
2. Accept-to-complete ratio as anomaly signal.
3. Conversion rate cap at 80%.

**Residual risk:** If conversion counts completions, the Sybil buyer must complete the full cycle (scrip cost). One Sybil completion costs the entry price + fee. For a single entry, this may be worth it to avoid exclusion. But the seller is essentially paying to keep their own content listed -- which is economically equivalent to advertising cost and may be acceptable.

**Recommendation:** **Spec change required.** Change the conversion rate formula from `AcceptCount / PreviewCount` to `CompletionCount / PreviewCount`. This was proposed in the preview review and should be formalized in the reputation derivation rules.

---

### E7. Federation reconciliation default as exit scam

**Attack:** An operator joins federation, accepts cross-operator matches (receiving scrip from buyer-side settlements), accumulates an inter-operator debt to the partner, then goes offline without reconciling. The partner paid out residuals to sellers but never receives the cross-operator fee.

**Severity: HIGH**

**Mitigation:**
1. Maximum outstanding balance threshold in federation agreement -- federation pauses when exceeded.
2. Soft suspension at overdue +12h, revoke at +48h.
3. All transactions are on the shared campfire, providing a verifiable record of the debt.

**Residual risk:** The attacker profits from the difference between the cross-operator fee they owe and the scrip they extracted from the partnership. The maximum loss is bounded by `max_outstanding_micro_tokens` in the settlement terms. If this cap is set high, the loss can be significant.

**Recommendation:** Add to federation convention: "Operators SHOULD require the first 30 days of federation to use a `max_outstanding_micro_tokens` no higher than the cross-operator fee revenue from 50 transactions. This caps the maximum loss from a new partner's exit scam at a modest amount while the trust relationship develops. The cap may be raised bilaterally as cross-operator trust score increases."

---

## 3. Signal Corruption Adversary

### S1. Goodhart attack on the three pricing loops

**Attack:** The fast loop adjusts prices based on demand velocity. An attacker repeatedly buys entries in a target domain, creating artificial demand velocity. The fast loop raises prices. The attacker then stops buying. Other buyers now face inflated prices for that domain. If the attacker is also a seller in that domain, their entries earn higher residuals during the inflation period.

**Severity: MEDIUM**

**Mitigation:**
1. Each buy costs scrip (matching fee burned). Sustained demand manipulation is expensive.
2. The medium loop (1-hour cadence) corrects fast-loop overreaction by analyzing accumulated adjustments. A spike followed by silence is a correction signal.
3. The slow loop (4-hour cadence) adjusts market parameters including step size. Oscillating prices trigger step-size reduction.

**Residual risk:** The attacker profits if the residual earned during the inflation period exceeds the scrip spent on artificial buys. For this to work, the attacker needs genuine third-party buyers purchasing at inflated prices during the manipulation window. If third-party buyers are price-sensitive (which the budget cap encourages), they reduce their purchases when prices spike, dampening the profit opportunity.

**Recommendation:** Add to the convention's pricing guidance: "The fast loop SHOULD apply an asymmetric step size: price increases are limited to 5% per adjustment, while price decreases can be up to 15%. This makes it harder to inflate prices quickly while allowing fast correction." Also: "Single-buyer demand velocity (one buyer creating many buys) SHOULD be discounted relative to multi-buyer demand velocity."

---

### S2. Enrichment metadata poisoning for match ranking manipulation

**Attack:** An agent claims enrichment tasks and submits metadata that is semantically valid (passes the 0.7 embedding similarity check) but subtly biased -- e.g., adding domain tags that make the entry appear in more search results than warranted, or writing descriptions that are optimized for semantic matching with common buy queries rather than accurately describing the content.

**Severity: MEDIUM**

**Mitigation:**
1. Enrichment metadata is validated: description must be semantically consistent with content (embedding similarity >= 0.7).
2. Enrichment does not directly affect ranking score -- it improves discoverability (which queries match) but not the composite score once matched.
3. Domains added by enrichment must overlap with content analysis.

**Residual risk:** The 0.7 similarity threshold allows substantial freedom in description wording. A skilled manipulator can write descriptions that score 0.75 similarity with content but are optimized for high-volume search queries. The entry appears in more match results than it should, but once shown, the preview model lets buyers evaluate before purchasing. The damage is wasted buyer attention (seeing irrelevant results), not wasted scrip.

**Recommendation:** The existing mitigations are adequate for v1. For v2, consider tracking enrichment-to-buyer-reject rates: if enriched entries have higher reject rates than non-enriched entries from the same seller, the enricher's work is degrading discoverability quality.

---

### S3. Convergence signal manipulation via strategic validation verdicts

**Attack:** A group of agents systematically submits "fail" verdicts for validation of a competitor's entries, trying to trigger re-validation with higher bounties (1.5x escalation on no-majority) or to damage the entries' perceived quality.

**Severity: MEDIUM**

**Mitigation:**
1. Convergence requires 2-of-3 agreement. A single dissenter does not change the outcome.
2. All 3 agents are paid regardless of individual verdict, removing the incentive to guess the majority answer.
3. Spot-checks (5% of validations) detect agents whose verdicts consistently disagree with exchange re-validation.
4. Agents with anomalously high stale/fail rates lose eligibility.

**Residual risk:** If 2 of 3 agents collude to fail a valid entry, the entry is incorrectly marked as failed. This requires compromising 2 of the 3 randomly-assigned slots. With a pool of 20 eligible agents, the probability of two specific colluding agents both being selected is (2/20) * (1/19) = ~0.5%. Reasonable for a targeted attack if repeated (attacker claims many validation slots over time, succeeds occasionally). Damage per success: one valid entry loses its validation status and may be excluded from matches until re-validated.

**Recommendation:** Add a *validation appeals* mechanism: if an entry fails validation but has a high historical completion rate (> 70% of buyers complete without dispute), the exchange SHOULD auto-trigger a re-validation with a fresh set of agents rather than accepting the failure. This catches cases where colluding validators produce a false negative on a genuinely good entry.

---

### S4. Preference signal leaking into behavioral measurement

**Attack:** The convention is designed around behavioral signals (did the buyer complete? did they come back?). But the buy operation includes `min_reputation` and `content_type` as preference filters. If the matching engine uses these preferences to learn what buyers "want" (rather than what they actually use successfully), the signals become gameable -- sellers optimize for what buyers ask for rather than what works.

**Severity: LOW**

**Mitigation:**
1. The 4-layer value stack explicitly prioritizes behavioral signals (completion rate, repeat purchase) over preference signals (content type filter, reputation threshold).
2. Layer 0 (correctness gate) is purely behavioral: task completion rate, not buyer preferences.
3. The convention does not feed preference signals into the pricing loops -- only settlement outcomes (complete, dispute, reject) drive price adjustments.

**Residual risk:** If an operator's matching implementation leaks preference signals into the ranking algorithm (e.g., boosting entries that match the buyer's content type preference over entries with higher completion rates), the ranking degrades. This is an implementation risk, not a convention risk. The convention correctly specifies that Layer 0 gates on completion rate, not preferences.

**Recommendation:** Add a conformance note: "Matching implementations MUST NOT use buyer preference fields (min_reputation, content_type, domains) as positive ranking signals. These fields are filters (exclude non-matching entries) not boosters (promote matching entries). Ranking MUST be driven by the 4-layer value stack using behavioral signals from the settlement log."

---

### S5. Small-content-dispute weaponization for reputation suppression

**Attack:** A buyer files small-content-disputes (< 500 tokens, auto-refund) against a competitor's entries. Each dispute costs nothing (full refund) and inflicts -3 reputation. Five disputes reduce reputation by 15. At 3 disputes from distinct buyers, the entry hits the Layer 0 exclusion gate.

**Severity: HIGH**

**Mitigation:**
1. Rate limit: 5 small-content disputes per buyer per rolling 24 hours.
2. 3 disputes from *distinct* buyers trigger exclusion -- a single buyer filing 3 disputes does not trigger it.
3. Small-content entries (< 500 tokens) cannot be previewed, so some legitimate disputes are expected.

**Residual risk:** An attacker with 3 Sybil identities can trigger Layer 0 exclusion for any small-content entry at zero scrip cost (auto-refund on every dispute). Each identity needs scrip to buy the entry (requires buyer-accept), but the auto-refund returns the full amount. The net cost is zero scrip, and the damage is permanent exclusion. Creating 3 Sybil identities requires 3 x402 purchases (to have buying scrip), but the x402 cost is recovered via the refund.

**Recommendation:** **Spec change required.** Small-content-dispute refunds should NOT be full refunds. Retain a dispute filing fee (e.g., 10% of the purchase price, burned) even when the dispute is auto-approved. This creates economic friction: each Sybil dispute costs 10% of the purchase price, making weaponized disputes expensive. Also: the Layer 0 exclusion should require 3 disputes from buyers who have *other successful purchases* on the exchange (not zero-history accounts), preventing fresh Sybil accounts from triggering exclusion.

---

## 4. Federation Adversary

### F1. Inventory poisoning via metadata-honest, content-garbage entries

**Attack:** A rogue operator shares inventory with valid metadata (correct description, honest embedding, real content hash) but the underlying content is AI-generated filler that is syntactically plausible but semantically useless. The metadata passes all automated checks. Buyers who purchase and use the content find it does not actually help.

**Severity: HIGH**

**Mitigation:**
1. Preview model: buyers see 20% of content before purchasing. Filler content may be detectable in preview.
2. Cross-operator trust overlay: after multiple buyer-reject or dispute outcomes, the originating operator's trust score drops.
3. Content hash verification ensures what is previewed is what is delivered.

**Residual risk:** Sophisticated filler (e.g., plausible but slightly incorrect code analysis) may not be detectable in a 20% preview. The damage accumulates: buyers waste scrip, the receiving operator's trust overlay eventually catches the pattern, but early adopters bear the cost. The blast radius is bounded by the cross-operator trust overlay decaying the rogue operator's trust score (each dispute is -5 trust; at trust < 40, inventory is excluded).

**Recommendation:** Add to federation convention: "The receiving operator SHOULD apply a *probationary period* for new federation partners: during the first 30 days (or first 100 cross-operator matches, whichever comes first), cross-operator inventory is excluded from top-3 match results. It appears only when local inventory has fewer than 3 matches for a buy request. This limits the blast radius of a new rogue partner to edge cases where no local inventory exists."

---

### F2. Trust score inflation via bilateral collusion

**Attack:** Two colluding operators federate with each other and generate fake cross-operator matches with perfect outcomes (100% completion, zero disputes). Both operators' cross-operator trust scores climb rapidly. They then federate with legitimate operators, entering with elevated trust.

**Severity: MEDIUM**

**Mitigation:**
1. Cross-operator trust starts at 50 for every new federation partner, regardless of the partner's trust history with other operators. Trust is bilateral, not transitive.
2. The inflated trust score exists only between the two colluding operators. When either federates with a legitimate operator, the new relationship starts at 50.
3. Discovery via beacons is authenticated (cryptographic identity) but not reputation-bearing. A beacon does not carry trust scores.

**Residual risk:** The collusion achieves nothing. Cross-operator trust is strictly bilateral and restarts at 50 for each new relationship. The only scenario where prior trust matters is if a receiving operator asks the proposing operator for *references* (e.g., "show me your trust score with your other partners"). The convention does not define a reference mechanism. If one is added in the future, it must be designed to resist this attack.

**Recommendation:** No action needed for the current design. Add a note to the federation convention: "If a reference or endorsement mechanism is added in a future version, it MUST NOT accept self-reported trust scores from the subject operator. References should come from third-party operators who independently attest to outcomes."

---

### F3. Reconciliation timing attack -- accumulate debt then dispute

**Attack:** Operator A matches content from Operator B, accumulating cross-operator fees owed to B. Before reconciliation, Operator A files disputes on all the matched content, trying to claw back the scrip while still owing the inter-operator fee.

**Severity: MEDIUM**

**Mitigation:**
1. Disputes are between the buyer and the local operator. A buyer's dispute on Operator A's exchange results in a refund from Operator A's escrow, not from Operator B.
2. The cross-operator fee is owed regardless of whether the buyer later disputes. The fee is for the match service, not contingent on buyer satisfaction.
3. If Operator A argues the fee should be refunded because the content was bad, this is a bilateral negotiation per the settlement terms, not an automatic convention rule.

**Residual risk:** The design correctly separates buyer-facing dispute resolution (local) from inter-operator fee obligations (bilateral). An operator who disputes their own fee obligations is violating the federation agreement. The remedy is revocation. The financial exposure is capped by `max_outstanding_micro_tokens`.

**Recommendation:** Add to federation convention: "Cross-operator fees accrue at match-confirm time and are unconditional. Buyer-facing disputes do not reduce inter-operator fee obligations. An operator who withholds reconciliation payments while continuing to match remote inventory is in breach. The aggrieved partner SHOULD revoke federation and MAY publish a `federation:breach` message on the shared campfire for third-party audit."

---

### F4. Slow-drip dispute weaponization across federation

**Attack:** Rogue Operator A instructs its buyers to file disputes at a rate just below the cross-operator trust decay detection threshold. Over months, the steady drip erodes the seller's reputation on Operator A's exchange. The seller (on Operator B) does not see this directly -- their local reputation is unaffected. But their content becomes increasingly excluded from Operator A's match results.

**Severity: LOW**

**Mitigation:**
1. Disputes are local -- Operator A's disputes do not affect Operator B's reputation scores.
2. Operator B can detect the pattern: seller X's dispute rate on Operator A is anomalous compared to Operator B, C, D. This is flagged in reconciliation.
3. The seller's content is only excluded from Operator A's results, not globally. The blast radius is one federation partner.
4. Operator B can revoke federation with Operator A if the dispute pattern is adversarial.

**Residual risk:** The attack succeeds in excluding one seller's content from one partner's exchange. The seller is not financially harmed (disputes result in refunds, not penalties, on the cross-operator side). The damage is reduced reach -- the seller's content appears on fewer exchanges. This is bounded and recoverable (Operator B revokes, finds a new partner).

**Recommendation:** No spec change needed. The existing cross-operator dispute rate tracking in the federation trust overlay handles this. Add a note: "Operators SHOULD monitor per-seller dispute rates on each federated partner. A seller with a dispute rate > 3x their local baseline on a specific partner is a signal of adversarial behavior by that partner."

---

### F5. Inventory offer replay for stale content

**Attack:** Operator B shares an `federation:inventory-offer` for an entry. The entry expires on Operator B's local exchange. Operator B fails to send the tombstone withdrawal message. Operator A's buyers continue to see and attempt to purchase the stale entry. The match-request fails (entry no longer exists), wasting buyer time and degrading Operator A's user experience.

**Severity: MEDIUM**

**Mitigation:**
1. `federation:match-confirm` validates that the entry still exists on the originating exchange. Stale entries produce a match-reject.
2. Match-confirm timeout (60s, -1 trust) penalizes the originating operator.

**Residual risk:** Between the entry's expiry and the failed match-request, buyers see stale results in their match list. This is a user experience degradation, not a financial loss (no scrip is escrowed until buyer-accept, which happens after match-confirm). If the originating operator consistently fails to send tombstones, their trust score decays via -1 per timeout.

**Recommendation:** Add to federation convention: "Inventory offers MUST include an `expires_at` field matching the entry's local expiry. The receiving operator MUST exclude expired inventory offers from match results proactively, rather than relying solely on match-confirm rejection. Tombstone messages (`federation:inventory-offer` with `withdrawn: true`) MUST be sent within 60 seconds of local entry expiry or withdrawal."

---

### F6. Cross-operator preview latency as denial of service

**Attack:** In cross-operator matches, preview content must be fetched from the originating operator. A malicious operator deliberately introduces latency in preview delivery (e.g., 30-second delays), making their inventory appear in match results but frustrating buyers who try to preview.

**Severity: LOW**

**Mitigation:**
1. Preview delivery timeout triggers auto-reject (buyer does not wait indefinitely).
2. Repeated slow previews reduce cross-operator trust score via buyer-reject accumulation.
3. The receiving operator can apply timeout SLAs to federated preview delivery (e.g., < 5 seconds).

**Residual risk:** The attack degrades user experience for a subset of matches. It does not cause financial loss. The cross-operator trust overlay handles it through natural buyer-reject accumulation.

**Recommendation:** Add to federation convention: "The receiving operator SHOULD enforce a preview delivery SLA of N seconds (default: 10). Previews that exceed the SLA SHOULD trigger an automatic buyer-reject with reason `timeout`. Repeated SLA violations (> 5 in 1 hour) SHOULD trigger cross-operator trust reduction."

---

## Summary

### Attack Inventory

| # | Attack | Disposition | Severity | Exploitable Today? | Spec Change? |
|---|--------|------------|----------|-------------------|--------------|
| T1 | Sybil reputation cascade across conventions | Trust | HIGH | Yes (bootstrap phase) | Recommend reputation velocity cap |
| T2 | Identity rotation after penalty | Trust | MEDIUM | Yes | Permanent constraint, disclose |
| T3 | Collusion ring: buyer-seller-validator triad | Trust | MEDIUM | Yes | Recommend closed-loop detection |
| T4 | Claim-then-stall griefing on assign | Trust | LOW | Yes | Adequate mitigations |
| T5 | Operator key compromise | Trust | CRITICAL | Requires key theft | Permanent constraint, disclose |
| E1 | Scrip inflation via assign bounty farming | Economic | MEDIUM | Yes (bootstrap phase) | Permanent constraint at bootstrap |
| E2 | Double-spend via concurrent buys | Economic | LOW | No | Adequate mitigations |
| E3 | Arbitrage between federated operators | Economic | LOW | No | Not an attack |
| E4 | Reservation starvation (accept-and-abandon) | Economic | MEDIUM | Yes | **Spec change: expiry enforcement** |
| E5 | Preview reconstruction (free-riding) | Economic | HIGH | Yes | **Spec change: preview cap + fixed seed** |
| E6 | Conversion rate gaming | Economic | MEDIUM | Yes | **Spec change: count completions** |
| E7 | Federation reconciliation exit scam | Economic | HIGH | Yes | Recommend initial cap |
| S1 | Goodhart attack on pricing loops | Signal | MEDIUM | Partially | Recommend asymmetric step size |
| S2 | Enrichment metadata poisoning | Signal | MEDIUM | Partially | Adequate for v1 |
| S3 | Convergence signal manipulation | Signal | MEDIUM | Partially | Recommend validation appeals |
| S4 | Preference signal leaking into behavioral | Signal | LOW | Implementation-dependent | Add conformance note |
| S5 | Small-content-dispute weaponization | Signal | HIGH | Yes | **Spec change: filing fee + buyer history** |
| F1 | Inventory poisoning (metadata-honest garbage) | Federation | HIGH | Yes (on federation) | Recommend probationary period |
| F2 | Trust score inflation via bilateral collusion | Federation | MEDIUM | No (trust is bilateral) | No action needed |
| F3 | Reconciliation timing attack | Federation | MEDIUM | No (fees unconditional) | Add breach messaging |
| F4 | Slow-drip dispute weaponization | Federation | LOW | Partially | Adequate mitigations |
| F5 | Inventory offer replay (stale content) | Federation | MEDIUM | Yes | **Spec change: expires_at in offers** |
| F6 | Cross-operator preview latency DoS | Federation | LOW | Yes | Recommend SLA enforcement |

### Severity Distribution

| Severity | Count | Spec Change Required |
|----------|-------|---------------------|
| **CRITICAL** | 1 | 0 (permanent constraint) |
| **HIGH** | 5 | 3 |
| **MEDIUM** | 11 | 2 |
| **LOW** | 5 | 0 |

### Required Spec Changes (before shipping)

1. **E4/E5: Settlement phase additions.** Add `buyer-cancel` phase to settle. Enforce reservation `expires_at`. Emit `scrip:reservation-expired` on timeout. Add per-buyer per-entry preview cap (N=3). Fix preview seed to exclude match_id so repeated previews show the same chunks.

2. **E6: Conversion rate formula.** Change from `AcceptCount / PreviewCount` to `CompletionCount / PreviewCount` in the reputation derivation rules.

3. **S5: Small-content-dispute economics.** Retain a 10% filing fee on small-content-dispute (burned, not refunded). Require dispute-filing buyers to have at least 1 prior successful purchase on the exchange.

4. **F5: Federation inventory expiry.** Add mandatory `expires_at` field to `federation:inventory-offer`. Require tombstone within 60 seconds of local expiry.

### Permanent Constraints (disclosed, not fixable)

1. **Operator is the trust root (T5).** Key compromise grants full control. Mitigated by auditing and anomaly detection, not by protocol design.

2. **Identity rotation escapes penalties (T2).** Agents can abandon penalized keys. Mitigated by cost of re-bootstrapping balance and reputation.

3. **Operator price manipulation (inherited from core-operations S5).** The publisher model gives the operator pricing power. Mitigated by competing exchanges and public price history.

4. **Bootstrap-phase concentration (E1, T1).** During cold start, the operator and a small number of agents control the exchange. This is inherent to any new marketplace. Mitigated by federation (external accountability) once the exchange matures.

### Recommendations for Future Work

1. **Closed-loop transaction detection.** Automated detection of small sets of keys that form closed transaction loops (T3). Should be a convention recommendation, not a hard rule (false positives on small exchanges).

2. **Reputation velocity cap.** Cap reputation growth at +15 per 24 hours (T1). Prevents fast-farming while allowing organic growth.

3. **Validation appeals.** Automatic re-validation for entries that fail convergence but have strong historical completion rates (S3).

4. **Federation probationary period.** New partners' inventory excluded from top-3 results for first 30 days (F1).

5. **Asymmetric pricing step size.** Fast loop price increases capped at 5%, decreases allowed up to 15% (S1).

6. **Federation breach messaging.** `federation:breach` message type for documenting reconciliation defaults (F3).
