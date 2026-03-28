# Adversarial Review: Preview Before Purchase

**Date:** 2026-03-27
**Reviewer:** Designer (Opus), adversarial mode
**Item:** dontguess-sn7
**Scope:** Preview sybil reconstruction, bait-and-switch, conversion rate inflation, scrip reservation timing attacks

---

## 1. Preview Sybil Reconstruction

### Attack Description

An adversary creates multiple buy orders for the same task, each producing a different match message ID. For each match, the adversary sends a `preview-request` referencing the same entry. Because the preview seed is `SHA256(entry_id + buyer_key + match_id)`, each match ID produces a different set of chunks. By collecting enough previews, the adversary reconstructs the full content without ever sending `buyer-accept` (and thus never paying).

### Feasibility Analysis

**High feasibility.** The implementation has no hard cap on this attack vector:

1. **No per-buyer per-entry preview limit.** The dedup in `applySettlePreviewRequest` (state.go:976) only rejects duplicates where `previewsByEntry[entryID][buyerKey] == matchMsgID`. When the same buyer requests previews via *different* match IDs (different buy orders), the dedup overwrites the tracked match (state.go:980) and allows the new preview. Each preview uses a different seed, producing different chunks.

2. **Unlimited buy orders.** There is no limit on how many buy orders a single buyer can place for the same task description. Each buy order produces a fresh match with a new match ID.

3. **No scrip cost for previews.** The preview-request/preview exchange costs zero scrip. Scrip is only reserved at `buyer-accept` time. An adversary can preview indefinitely without spending.

4. **Reconstruction speed.** Each preview reveals ~20% of content (5 chunks at 4% each). With random chunk selection across the content space, the coupon collector problem applies. For content with ~25 boundary-aligned chunk positions, near-complete reconstruction requires approximately 80-120 previews (each from a different buy order). The existing anti-reconstruction test (preview_test.go:756) documents this bound but does not enforce a hard cap: after 50 match IDs with the same buyer key, exposure was already significant.

5. **No Sybil required.** A single buyer identity can execute this attack. Multiple identities would accelerate it but are not necessary.

6. **Rate limit is generous.** The convention spec (settle.json) specifies 50 settle messages per sender per hour. Each preview-request is one settle message. At 50 previews/hour, full reconstruction of most entries completes within 2-3 hours.

### Severity: **HIGH**

The attack is economically free (no scrip spent), requires only one identity, and completes within hours. The 20% preview window was designed to be a sample, not the full content. Without a cap, the preview model degrades to "free content with extra steps."

### Proposed Mitigations

1. **Per-buyer per-entry preview cap.** Enforce a maximum of N preview-requests per (buyer_key, entry_id) pair across all match IDs. N=3 gives the buyer adequate evaluation opportunity (3 different views) while capping exposure at ~60% even with zero chunk overlap. Implementation: add a counter in `previewsByEntry` keyed by (entry_id, buyer_key) that tracks total preview count regardless of match ID, and reject when the cap is reached.

2. **Scrip deposit for preview.** Require a small refundable deposit (e.g., 1% of entry price) at preview-request time, refunded at buyer-accept or buyer-reject. This adds economic friction to mass preview harvesting without penalizing legitimate evaluation. The current convention already supports scrip operations at any settlement phase.

3. **Cumulative exposure tracking.** Track the byte ranges already shown to each (buyer_key, entry_id) pair. When a new preview would push cumulative exposure above 40%, serve chunks only from already-exposed regions. This is more complex but provides a hard ceiling.

Recommendation: mitigation (1) is the simplest and most effective. Mitigation (2) adds economic friction on top. Mitigation (3) is the most robust but adds implementation complexity.

---

## 2. Bait-and-Switch

### Attack Description

A seller submits high-quality content, accumulates a high conversion rate and good reputation via genuine preview-driven sales, then swaps the underlying content for inferior material. Buyers who accepted based on the preview of the original content receive different (worse) content at delivery.

### Feasibility Analysis

**Very low feasibility.** Multiple design elements block this:

1. **Hash verification at delivery.** Content hash is included in the preview message (`content_hash` field in settle.json) and verified at `settle(complete)` time (preview-before-purchase.md section 6). If the delivered content does not match the hash from the preview phase, delivery fails and the reservation is not consumed. The buyer never pays.

2. **SHA-256 collision resistance.** Producing different content with the same SHA-256 hash is computationally infeasible (2^128 work for collision, 2^256 for preimage).

3. **Immutable put.** The `put` message includes a content hash verified at `put-accept`. Changing the content requires a new `put` with a different hash, which is a new entry with a new entry ID, resetting all reputation signals.

4. **Operator complicity is auditable.** If the operator conspires with the seller to deliver different content while spoofing hash verification, the entire transaction chain is on the campfire log. Any auditor can replay the log and verify that the delivered content hash matches the put-accept hash. Operator fraud is detectable post-hoc.

5. **Content stored externally but hash-pinned.** Even though content is stored outside the campfire, the hash chain (put -> put-accept -> preview -> buyer-accept -> deliver -> complete) pins the content identity cryptographically. Swapping content means breaking the hash chain, which is detectable.

### Severity: **LOW**

The attack is blocked by the existing hash verification chain. The only residual risk is an operator who deliberately circumvents hash verification in their own engine — which is a trust-the-operator problem inherent to any single-operator deployment and detectable via log audit.

### Proposed Mitigations

None required. Existing hash verification is sufficient. For multi-operator federation (future), cross-operator log audit would close the operator-complicity gap.

---

## 3. Conversion Rate Inflation

### Attack Description

A seller creates Sybil buyer identities that request previews and then send `buyer-accept` for the seller's entries, inflating the conversion rate. High conversion rate increases reputation (up to +10 bonus at 100% conversion), prevents Layer 0 exclusion, and makes entries more visible in match results.

### Feasibility Analysis

**Medium feasibility.** The attack is possible but economically constrained:

1. **Scrip cost per inflation.** Each fake buyer-accept requires scrip. The `handleSettleBuyerAcceptScrip` function (engine.go:678) decrements the buyer's balance by `price + fee`. The Sybil buyer must have scrip to spend. Scrip enters the system via x402 purchase or labor — both have real cost.

2. **Conversion rate manipulation is cheap relative to attack surface.** The conversion rate formula (state.go:207) is `ConversionCount / PreviewCount`. To inflate from 50% to 90% conversion on an entry with 10 previews, the attacker needs approximately 8 fake previews followed by 8 fake accepts. Cost: 8 * (price + fee) in scrip.

3. **Cross-agent convergence is harder to game.** The +3 reputation bonus requires 3+ *distinct* buyers completing purchases of the same entry (state.go:223). Sybil identities count as distinct buyers, but each must complete the full buy cycle including `settle(complete)`, not just `buyer-accept`. This costs more scrip per identity.

4. **Conversion rate is per-entry, reputation is per-seller.** The Layer 0 gate checks per-entry conversion rate (state.go:1109). Reputation combines seller-level conversion rate with per-entry convergence and repeat buyer signals. The attacker must inflate specific entries to avoid Layer 0 exclusion AND inflate seller-level stats for reputation.

5. **No completion required for conversion rate.** The conversion rate counts `buyer-accept` as a conversion (state.go:348, state.go:712), but the `settle(complete)` is what triggers `SuccessCount` and convergence bonuses. The attacker can inflate conversion rate cheaply (just buyer-accept) but cannot inflate the stronger signals without completing the full cycle.

6. **No velocity or distribution anomaly detection.** The current reputation model has no mechanism to detect suspicious patterns: a sudden burst of accepts from new identities, 100% conversion rate (unrealistically high), or accepts from buyers who never complete.

### Severity: **MEDIUM**

The attack is economically bounded (scrip cost per fake accept) and the strongest trust signals (convergence, repeat buyer) require completing the full cycle. However, the conversion rate signal alone is sufficient to prevent Layer 0 exclusion and gain a +10 reputation bonus. Without anomaly detection, a motivated seller can maintain artificially high conversion rates indefinitely.

### Proposed Mitigations

1. **Conversion rate should count completions, not accepts.** Change the conversion rate to `CompletionCount / PreviewCount` instead of `AcceptCount / PreviewCount`. This forces Sybil buyers to complete the full transaction cycle (including `settle(complete)` with hash verification), making inflation significantly more expensive. The buyer-accept-without-complete pattern becomes a signal of abandonment, not conversion.

2. **Accept-to-complete ratio as anomaly signal.** Track the ratio of buyer-accepts to completions per entry. A high accept rate with low completion rate is suspicious — legitimate buyers who accept after preview usually complete. Flag entries where `accept_count / complete_count > 2` for manual review or automatic reputation dampening.

3. **Conversion rate cap.** Cap the conversion rate bonus at 80% conversion (rather than scaling linearly to 100%). No legitimate product has a 100% conversion rate. This limits the maximum reputation benefit from inflation.

---

## 4. Scrip Reservation Timing Attacks

### Attack Description

Four sub-attacks related to the scrip reservation lifecycle:

**4a. Concurrent accept race (over-commitment).** A buyer sends multiple `buyer-accept` messages simultaneously for different matches, each referencing a different preview. If the balance check (GetBudget) runs before any DecrementBudget completes, the buyer could lock more scrip than they have.

**4b. Reservation expiry not enforced.** The buy-hold payload includes `expires_at` (5 minutes), but no code in the engine or scrip store enforces this expiry. Reservations persist in memory indefinitely until consumed or the engine restarts.

**4c. Crash between accept and complete.** If the engine crashes after `buyer-accept` (scrip decremented, reservation saved) but before `settle(complete)` (reservation consumed, scrip distributed), the buyer's scrip is locked in a reservation that may never settle.

**4d. Reservation restoration on restart.** The `findExistingBuyerAcceptHold` function (engine.go:1360) scans the campfire log for scrip-buy-hold messages to restore reservations on restart. An attacker could potentially craft messages that interfere with this recovery.

### Feasibility Analysis

**4a. Concurrent accept race: Low feasibility.**
The `DecrementBudget` function (campfire_store.go:325) uses etag-based optimistic concurrency. The etag changes on every balance mutation. If two concurrent buyer-accepts race, the second `DecrementBudget` call will fail with `ErrConflict` because the etag from its `GetBudget` call no longer matches. This is a correct optimistic concurrency pattern for single-process deployment. The TOCTOU window between `GetBudget` and `DecrementBudget` in `handleSettleBuyerAcceptScrip` (engine.go:764-791) is closed by the etag check on the write path.

However: the engine processes messages sequentially in `poll()`, not concurrently. Two buyer-accept messages from the same buyer would be processed one at a time. The second would see the reduced balance from the first. The race condition requires truly concurrent message processing, which the current architecture does not permit.

**4b. Reservation expiry not enforced: Medium feasibility.**
Confirmed: `expires_at` is written into the buy-hold payload (engine.go:775) but never checked by `ConsumeReservation`, `GetReservation`, or any background process. A reservation created at buyer-accept time will persist indefinitely in memory. If the buyer never sends `settle(complete)`, the scrip remains locked forever (until engine restart, which discards in-memory reservations).

This is not directly exploitable as an attack (the buyer loses their own scrip), but it creates a resource leak: abandoned transactions lock scrip permanently. A malicious buyer could intentionally accept and abandon to lock their own scrip as a denial-of-service against themselves (pointless) or to artificially reduce circulating supply (marginal impact).

The real risk is operational: legitimate buyers who disconnect or fail between accept and complete lose their scrip with no recovery mechanism.

**4c. Crash between accept and complete: Medium feasibility.**
On restart, the engine replays the campfire log. The `findExistingBuyerAcceptHold` function (engine.go:1360) scans for scrip-buy-hold messages to reconstruct the `matchToReservation` map. The buyer's balance was decremented by the buy-hold message during replay (via `CampfireScripStore.applyBuyHold`). The reservation is re-created in memory.

However, if the complete message was never sent (crash before complete), the reservation exists but there is no timeout or cleanup mechanism to release it. The buyer's scrip remains locked. The only recovery path is: (a) the buyer resends `settle(complete)` after the engine restarts, or (b) the buyer files a `small-content-dispute` if applicable. There is no general-purpose reservation release mechanism for stuck transactions.

**4d. Reservation restoration spoofing: Very low feasibility.**
The `findExistingBuyerAcceptHold` scans for messages tagged `scrip:buy-hold` where the sender is the operator. Only the operator can emit valid scrip messages (campfire_store.go:159). An attacker would need to forge operator-signed messages on the campfire, which requires the operator's Ed25519 private key. Not feasible without key compromise.

### Severity: **MEDIUM** (4b and 4c combined)

The concurrent race (4a) is not exploitable given the current sequential processing model. The reservation restoration spoofing (4d) requires key compromise. However, the missing expiry enforcement (4b) and lack of stuck-reservation recovery (4c) create an operational risk: legitimate transactions that fail between accept and complete permanently lock the buyer's scrip. This is not an adversarial attack but a reliability gap that an adversary could exploit for griefing (accept many entries and intentionally never complete).

### Proposed Mitigations

1. **Enforce reservation expiry.** Add a background goroutine or periodic check that scans reservations and releases any past their `expires_at` timestamp. On release: restore the buyer's balance via `IncrementBudget` and emit a `scrip-reservation-expired` convention message for auditability. This closes both 4b and 4c.

2. **Buyer-initiated cancel.** Add a `settle(buyer-cancel)` phase that allows a buyer to release their reservation before complete. The engine verifies sender identity, consumes the reservation, and restores the buyer's balance. This gives buyers a recovery path for stuck transactions.

3. **Reservation count limit per buyer.** Limit the number of active (un-settled) reservations per buyer key. E.g., a buyer may have at most 5 concurrent reservations. This prevents griefing via mass accept-and-abandon.

---

## Summary

| # | Attack | Severity | Exploitable Today? | Key Gap |
|---|--------|----------|-------------------|---------|
| 1 | Preview sybil reconstruction | **HIGH** | Yes | No per-buyer per-entry preview cap; previews are free |
| 2 | Bait-and-switch | **LOW** | No | Hash chain prevents content substitution |
| 3 | Conversion rate inflation | **MEDIUM** | Yes | Conversion counts accepts, not completions; no anomaly detection |
| 4 | Scrip reservation timing | **MEDIUM** | Partially | Reservation expiry not enforced; no stuck-transaction recovery |

**CRITICAL findings:** None. No attack requires an emergency fix before shipping.

**HIGH findings:** 1 (preview sybil reconstruction). Should be fixed before any deployment with valuable content. The simplest mitigation is a per-buyer per-entry preview cap of 3.

**MEDIUM findings:** 2 (conversion rate inflation, reservation timing). Both should be addressed but can ship with monitoring.

**LOW findings:** 1 (bait-and-switch). No action required.
