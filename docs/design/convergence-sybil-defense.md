<!-- source: adversarial-design workflow wf_16f3975c-880 (10 agents, 2026-07-06); rd dontguess-5b6; answers Open-Q#1 of nostr-first-rebuild-decision.md -->

# dontguess — Convergence Sybil-Resistance: Defense Decision

> **RATIFIED 2026-07-06 (Baron):** ruling adopted as written; recommended parameter set approved. Global-tier machinery is YAGNI until global traffic exists — this doc is the settled answer, not a build order.

**Status:** Decision document. Supersedes the open disposition in `nostr-first-rebuild-decision.md` §Open-Q#1 ("do not call public convergence a moat until an identity-independence model ships").
**Scope:** The hard problem is the **global / permissionless tier**. Team and Enterprise tiers use NIP-42 allowlisted identities and are treated as solved (the allowlist already paid the identity-independence cost this document tries to approximate).
**Inputs synthesized:** four mechanism specs (proof-of-cost burn, attestation-graph CAG, sybil-detection SDCS, verifiable-work PAC), their independent red-team passes, and the domain-purist thesis ruling.
**Sources of truth honored:** `docs/heritage/usefulness-validation.md` (observational boundary, cross-model independence), `docs/heritage/value-function.md` (behavioral-not-payment, Layer-0 correctness gate), `docs/design/nostr-first-rebuild-decision.md` (kinds 3401–3405/30401, deterministic-fold invariant §134, key-management ruling A14/A17).

---

## The Problem (threat model, precisely)

Cross-agent convergence — **"3+ independent agents succeeded with the same cache entry"** — is dontguess's ungameable trust signal. It exists to let a buyer trust a cached inference result they *cannot verify downstream*. This is the observational boundary: the exchange sees only proxies (completion, retry, return-rate), never true task success. Convergence is the corpus-quality backbone. Poison it and the thesis inverts — buyers trust junk, waste tokens, the exchange dies.

**The attack (global tier only).** On the open nostr mesh, npubs are free (secp256k1 keygen is microseconds). An attacker mints K npubs and manufactures fake convergence on a target `content_hash`: K signed `buy(3402) → match(3403) → settle:complete(3404)` chains, spread across K relays, all referencing the same entry. Payoffs:

- **(a) Pump-for-profit** — earn the primary sale price (attacker authors the junk `put`) plus 10% residual on every resale of the pumped entry, amplified by the pricing engine's trust-driven rank boost.
- **(b) Neighborhood poison** — collapse trust in a competitor's genuine entry by pumping junk into its embedding neighborhood.
- **(c) Grief** — collapse trust in the whole exchange. **No break-even required.**

**The bar a defense must clear:** make attacker **cost > payoff**, or make fake convergence **statistically detectable-and-discountable**, without violating four hard constraints:

1. **No trusted intermediary.** Convergence must be recomputable by any third party from public signed events — not taken on the operator's word. (An operator may score *locally*; the score must be *independently reproducible*.)
2. **Completion stays behavioral, never payment.** A zap/payment is buyable and wash-tradeable; it must not *be* the completion signal. Cost/stake may gate sybil-resistance *separately* from completion measurement.
3. **Net token reduction.** Honest lookups stay fast and cheap (a hit saves ≥50K tokens, a miss costs ~500; relay/chain reads never on the `buy`/`match` hot path).
4. **Tier-aware and graceful.** Must add value specifically at the global tier and degrade to a no-op at team tier.

**The deeper problem the mechanisms surface (load-bearing).** Per `value-function.md` §9 and `usefulness-validation.md` Attack 13, the independence that makes convergence ungameable is **cross-*model*, independently-*motivated* derivation — not distinct-*identity* derivation.** All four candidate mechanisms defend the identity-multiplicity axis. **None** touches model-correlation or motivation-correlation, and that axis is *unobservable* from public signed events. This means: even a *perfect* identity-independence mechanism does not deliver the heritage's convergence signal at the global tier. The observational boundary reasserts itself one layer up. This fact shapes the entire decision — it is why the answer is "weighted and qualified," not "solved."

---

## Mechanism Evaluation

Each family is evaluated on: how it works, attacker break-even (numeric), and purist verdict against the four constraints plus the unstated fifth requirement — **cost must scale with payoff.**

### Family 1 — Proof-of-Cost (one-time on-chain burn)

**How it works.** An npub that wants its convergence votes to carry weight publishes a `kind:3406` bond event citing a Bitcoin `OP_RETURN` / NUMS-address burn of `B_full = 5,000 sats` (~$3 at $60K/BTC). `weight(npub) = min(1.0, verified_burn_sats / B_full)`, capped at 1.0 per identity. Convergence trusts an entry when `Σ weight(npub_i) ≥ 3.0`. Verification is a pure function over the Bitcoin ledger + signed nostr events — byte-identical for any two readers at the same block height. Team tier: allowlisted npubs get weight 1.0 free.

**Attacker break-even.**

| Target | npubs | Sats burned | +tx fees | Total |
|---|---|---|---|---|
| Clear 3.0 | 3 | 15,000 | ~$3–9 | **~$9–18** |
| "10-fold" | 10 | 50,000 | ~$10–30 | **~$40–60** |
| Grief 50 entries | 50 | 250,000 | ~$50–150 | **~$200–300** |

The red-team's decisive correction: **cost binds to identity; payoff binds to campaign breadth.** The formula has *no per-entry cost term*. A 3-npub clique bonded once for ~$12 is reusable against **every entry, on every operator, forever, at ~$0 marginal.** A likely spec gap (no per-npub vote dedup) collapses K=3 to **~$3 on a single identity** publishing three `settle:complete` events. Griefing needs no break-even at all.

**Purist verdict.** **PASS on all four constraints; FAILS the fifth (cost>payoff).** Constraint 1: genuinely, unambiguously trustless — *the only mechanism of the four that is.* Constraint 2: passes, though it quietly bakes money into the denominator of the trust signal. Constraints 3, 4: clean (one-time, off hot path, team no-op). **It raises the floor, never caps the ceiling.** Keep it — as a *floor under a weighted signal*, never as the signal. Required fixes if kept: (a) explicit **distinct-npub, first-event-only** vote dedup; (b) explicit commitment that `verified_burn_sats` is cached at ingest, never a chain-RPC on any read path.

### Family 2 — Attestation Graph (CAG, web-of-trust)

**How it works.** A root set `R` = npubs with ≥50 cross-verified `settle:complete` events over 90 days. Weight propagates from `R` via vouches (`kind:3406`) with `0.5^hops` decay, gated by a one-time identity stake (`kind:3407`), capped at 1.0 total weight per NIP-05 apex domain. Trust when `Σ weight ≥ 3.0`.

**Attacker break-even.** The red-team break is **root *fabrication*, not root *compromise*.** "Operator" is self-declared on the open mesh (publish one `kind:3403` and you are one). Three sybil npubs cross-verifying each other satisfy "≥2 distinct other-operator npubs" *by the letter*, minting their own root set for **~$30–65 one-time (~$10–20 with PoW stake), ~90 mostly-unattended days, ~$0 marginal per additional poisoned entry.** Every cited settle event is *real* (self-dealt), so the only stated detection posture (reused/fabricated evidence) never fires. K=10 costs the same as K=3 (the 3.0-weight floor is fixed; padding is cosmetic).

**Purist verdict.** **FAIL — constraint 1, in both spirit and goal.** A web-of-trust rooted in a self-appointed set is a **decentralized cartel** — a trusted intermediary reintroduced one layer down, exactly what Open-Q#1 warns against. Third-party verifiability of a *poisoned computation* is not trust-minimization; it launders a trust assumption (roots are independent operators) through a deterministic function that cannot see the assumption is violated. **This is the most thesis-dangerous mechanism** because it dresses a trust assumption as trustlessness. Additional harm: the per-domain cap throttles a *legitimate* multi-npub fleet (which the key-management ruling mandates under one domain) to the same ceiling as a 1-person sybil ring, and teaches honest fleets to fragment across domains — corrupting the very signal it depends on. **Reject.**

### Family 3 — Sybil-Detection (SDCS, correlation discount)

**How it works.** Don't gate npub creation. Replace raw convergence count `K` with an **effective independent count** `K_eff ≤ K` via the survey-sampling design-effect formula:

```
K_eff(cluster) = n / (1 + (n−1)·ρ)      ρ = mean pairwise correlation
```

Five features drive ρ: F1 timing, F2 task-text lockstep, F3 embedding overlap (all signed/reproducible), F4 relay provenance, F5 LN funding linkage (both observed/heuristic). Compare `K_eff ≥ 3`. Runs on the medium loop (1hr), cached, $0 on hot path.

**The property that matters:** `K_eff → 1/ρ` as `n → ∞`. **A sybil ring cannot mint its way past the threshold if correlation stays high.** At ρ=0.5, max K_eff = 2 *forever*, regardless of ring size. This continuous-discounted-count shape is the single best structural idea across all four mechanisms.

**Attacker break-even.** Two breaks. **(1) The AND-gate is an evasion hatch.** As specified, an edge forms only if `c(i,j) ≥ τ` **AND** ≥3-of-5 sub-features individually exceed threshold. That conjunction lets the attacker defeat the **3 cheapest** features (timing, text, embedding-padding — all scriptable) and leave the **2 most diagnostic** (relay, funding) *fully correlated for free*: 2-of-5 → no edge → singleton → **full weight, zero discount.** This drops cost from the claimed $150–650 to **~$10–30**. **(2) Constraint-1 violation:** F4 (relay provenance) is "observed, not signed… reproducible only by an auditor subscribed to the same relays within retention." An auditor who wasn't live-subscribed computes a *different* K_eff — violating the deterministic-fold invariant (§134).

**Purist verdict.** **PASS-WITH-CONDITIONS — conditionally repairable, and the repaired core is the keystone.** Constraint 2 is the cleanest of the four (F1–F5 measure independence, never payment-as-completion). The repair path is concrete: **(a) restrict the authoritative fold to signed/deterministic features F1–F3; demote F4/F5 to non-authoritative advisory flags** (restores constraint 1); **(b) replace the count-gate with a `min` / geometric-mean combiner** forcing all *retained* features to look independent simultaneously (closes the evasion hatch). Residual: on the deterministic-only subset a patient attacker who genuinely diversifies timing + paraphrase + inventory can still lower ρ — but only at real, rising, per-target cost, and the `1/ρ` ceiling makes *naive* minting provably ineffective. Staleness flag: up to ~1hr window where a fresh pump is under-discounted (a decision-quality cost, not latency).

### Family 4 — Verifiable-Work (PAC, probe-attested convergence)

**How it works.** Redefine a convergence contribution from "independent *identity*" to "independent *capability*": to earn convergence credit, an npub answers an **identity-bound perturbation probe** of the entry's underlying task (`seed = KDF(H ‖ Bitcoin-block-beacon ‖ npub)`), commit-then-reveal, verified by a cheap objective `check_τ`. If answering the probe ≈ the real inference, minting K npubs buys nothing — each must *do the work*. Credit accrues to independent **derivers**, not cache **consumers** (so it never turns a hit back into a derivation). No payment anywhere.

**Attacker break-even.** PAC's security = the width of the "capability − published-output" gap. Two kills. **(1) Wrong content class.** That gap is ~0 for dontguess's *highest-reuse* inventory (checklists, CI fragments, Go idioms — the 12–37× entries): holding the output *is* answering the probe (~300 tokens), so 3 npubs fake convergence for ~900 tokens. PAC secures the near-empty *inverse* of what the exchange earns on — and its own flagship examples (executable artifact, `legion.tools` checklist at 37 reuses) are disqualified by its own §6. **(2) Amortization collapses the "K×D, no shortcut" claim.** The attacker solves the task *once* (cost D), then reaches the same low marginal state honest derivers use: `cost(K) = D + (K−1)·p`, and across M targets `cost/entry → K·p` — parity with honest cost. Plus reveal-corpus distillation drives `p → 0` as the mechanism succeeds, and unbounded `kinds:3406/3407` verification enables compute-DoS.

**Purist verdict.** **PASS-WITH-CONDITIONS — narrow, thesis-limited, keep only classifier-gated.** Constraints 1, 2 pass cleanly (trustless beacon, no payment, best respects the *actual* independence concern by shifting to capability). But constraint 3 fails for the majority class, and it **self-defeats at steady state** (its own §5.7: convergence-by-re-derivation requires K agents to redo the expensive work the exchange exists to *eliminate* — if the product succeeds, the deriver pool starves). PAC is structurally a **cold-start / frontier** signal, not steady-state. **Keep only as a classifier-gated component for the expensive-checkable minority**, exactly as its own §6 recommends. Never the general solution.

---

## The Decision — Layered, Tier-Aware Defense

**We do not average. We do not ship any single mechanism as "the moat."** We ship a **defense-in-depth stack** feeding a **continuous confidence weight** — never a boolean `3+ = trusted` gate — backstopped by the behavioral loops that are the actual Layer-0 correctness gate. CAG is **rejected outright** (constraint-1 violation via self-minted root cartel). The other three are composed, each scoped to what it actually secures.

### Team / Enterprise tier — solved, unchanged

NIP-42 allowlisted identities make independence real. Convergence is near-binary and third-party-auditable within the allowlist. Every global-tier mechanism below is a **no-op** here (weight ≡ 1.0). Enterprise federation extends the allowlist across partner relays. Nothing on the hot path changes. **This is where the strong moat claim lives.**

### Global / permissionless tier — the layered stack

Four layers, each independently third-party-recomputable from public signed events, all folding into one continuous weight:

**Layer A — Behavioral decay (the real Layer-0 backstop; already in the architecture).**
A poisoned entry that real buyers fail on raises `cluster_return_rate` / retry-rate and **decays within one fast-loop cycle (5–60 min)**, *regardless of forged convergence*. This is the ungameable floor the whole architecture already rests on. **Convergence was never meant to stand alone.** It bounds every attacker's exploitation window to a handful of real victims before the price/rank collapses. Always on, all tiers.

**Layer B — KEYSTONE: deterministic correlation discount (repaired SDCS).**
The continuous `K_eff = n/(1 + (n−1)·ρ)` discount, computed on the medium loop, cached, $0 on the hot path. **This is the keystone** for three reasons no other mechanism matches: (1) it is the only mechanism with a *hard mathematical ceiling* — `K_eff → 1/ρ` makes naive minting provably ineffective *regardless of ring size*; (2) restricted to signed features it is *fully constraint-1 clean* (byte-identical for any third party); (3) it costs honest users *nothing* and never touches completion. **Required configuration (non-negotiable):**
- Authoritative features: **F1 timing, F2 task-text, F3 embedding overlap** only (all reproducible from signed event content).
- **F4 (relay) and F5 (funding) demoted to non-authoritative advisory flags** — surfaced to human/operator review, never folded into the reproducible score. (This is what restores determinism.)
- Combiner: **`min` or geometric-mean across retained features**, never the ≥3-of-5 count-gate.
- Add a **clique-recurrence discount**: when the *same npub-set* recurs as the sole converging cluster across multiple otherwise-unrelated `content_hash`es within a rolling window, boost ρ / decay weight. **This is the fix for burn's fatal flaw** — it re-couples cost to campaign breadth by making a reused clique progressively *worthless*, which per-identity burn alone cannot do.

**Layer C — burn floor (repaired proof-of-cost).**
A one-time on-chain burn converts "$0, instant, invisible" poisoning into a "real, linear, on-chain-auditable" footprint. It is the *only* constraint-1-clean sybil-cost lever. It is a **floor, not a ceiling** — it kills drive-by/opportunistic sybils and forces every serious attack into an auditable footprint, but does not alone cap a determined attacker (Layer B's clique-recurrence discount does that). **Required fixes:** distinct-npub first-event-only vote dedup; `verified_burn_sats` cached at ingest.

**Layer D — PAC where eligible (classifier-gated).**
At `put`, classify probe-hardness with the **inverted `IsHighReuseArtifact`** token-shape classifier. Only **high-probe-hardness + objective-checker-available** entries (the expensive-checkable minority, and cold-start/frontier entries) earn convergence credit via verified probes at near-zero identity cost. Everything else — the reuse-dominant majority — earns convergence weight through Layers B+C. Never let an entry earn convergence weight from *neither* path. Bound `kinds:3406/3407` per `(H,e,P)` to prevent verification DoS.

**The fold.** Convergence weight for an entry = a **continuous confidence multiplier**, not a boolean. It is `Σ` over converging npubs of `[burn_weight × K_eff_discount × (PAC_verified ? boost : 1)]`, composed multiplicatively with the Layer-A behavioral signal. Downstream consumers read a **confidence score**, never a "trusted/untrusted" flag. This is the philosophical heart of the decision: at the global tier, convergence is *evidence weighted by independence-probability*, not a trust oracle.

---

## Residual Attack Surface (what stays open, and whether that's acceptable)

Honest accounting. The stack does **not** close everything. What remains:

1. **Model-correlation and motivation-correlation (structural, unclosable).** The heritage says the real independence is cross-*model*, independently-*motivated* derivation. **No mechanism here observes the base model or the motive behind a signed event.** An attacker running K genuinely-distinct npubs, all one base model, one manufactured motivation, with diversified timing/text/inventory, produces convergence that Layers B–D score as *valid* and heritage says is *worthless*. This axis is unobservable from public events — the observational boundary one level up. **This is the ceiling on the entire global-tier convergence signal and the primary reason the moat claim must be qualified.**

2. **Patient, capitalized, multi-target adversary.** One who pays the burn (Layer C), genuinely diversifies F1–F3 to lower ρ (Layer B), and spaces attacks to dodge clique-recurrence can still manufacture weighted convergence — but now at **real, rising, per-target cost with an auditable on-chain footprint, a discounted score, and a 5–60 min behavioral-decay window** capping realized payoff. Break-even is pushed from "free and instant" to "capitalized, slow, and visible." Not closed; *repriced*.

3. **Pure griefing (payoff (c)).** A griefer with budget needs no break-even. Layer C makes it cost real sats per identity and Layer A bounds blast radius to minutes, but a funded griefer can still transiently degrade specific entries. Contained, not eliminated.

4. **Staleness window.** Up to one medium-loop cadence (~1hr) where a fresh pump is under-discounted and live. A decision-quality cost, not a latency cost. Layer A (fast loop) partially covers it.

**Is this acceptable? Yes — conditionally.** It is acceptable *if and only if* the moat claim is qualified accordingly (next section) and convergence is never surfaced as a standalone boolean at global tier. The residual surface is the honest cost of a permissionless mesh; the alternative (closing it) requires reintroducing a trusted identity authority, which forfeits the entire nostr-first moat. We accept a *weighted, auditable, behaviorally-backstopped* signal over a *false binary*.

---

## Impact on the 'Convergence Moat' Claim (the honest answer)

**Yes — the global-tier moat claim must be weighted and qualified. This is not a retreat from the thesis; it is a return to the heritage's own epistemic humility.** The primary sources never claimed binary convergence as an oracle: `usefulness-validation.md` §10 ("outcome correctness is not observed"), `value-function.md` Layer 0 (persistence as a *weak proxy*), escape-velocity as a *continuous gap*. The binary "3+ = trusted" framing was always a GTM overreach that the decision doc itself flagged (Open-Q#1).

**The corrected claim:**

> *Third-party-auditable cross-agent convergence is a genuine, near-binary trust moat at **TEAM / ENTERPRISE tier**, where NIP-42 allowlisted identities make independence real. At **GLOBAL tier** it is a **weighted, probabilistic corpus-quality signal** that raises the cost and visibility of poisoning — converting a free, instant, invisible attack into a capitalized, auditable, on-chain-footprinted, behaviorally-decaying one. It is **not** an ungameable binary trust oracle at global tier and must always be composed with behavioral decay, never trusted standalone.*

Shipping "public cross-agent convergence is THE ungameable moat" at the global tier would *invert the thesis* — buyers would trust weighted junk as if it were binary truth, waste tokens, and the exchange would die. Qualifying to weighted is thesis-loyal.

**Quantified confidence (carried from the purist ruling, endorsed):**
- Binary, identity-independent convergence is **not** trustlessly achievable at global tier: **~90%.**
- The weighted defense-in-depth stack above is buildable and worth shipping: **~70%** (residual uncertainty is empirical — zero global-tier traffic exists yet, per the decision doc's YAGNI status).
- The moat claim must be tier-scoped and qualified rather than stated globally: **~95%.**

---

## Operator Ruling Required

You must make **one architectural ruling** and approve a **parameter set**. My recommendation is embedded.

### The ruling

> **At the global/permissionless tier, cross-agent convergence is a continuous, weighted confidence signal — never a boolean `3+ = trusted` gate — computed as a defense-in-depth fold and always composed with behavioral decay. CAG (web-of-trust) is rejected. The moat claim is tier-scoped: near-binary at team/enterprise, weighted-probabilistic at global.**

### My recommendation: **ADOPT the ruling and ship the layered stack.** Rationale:

- It is the only option that satisfies all four hard constraints while making a real, honest improvement over the status quo (free-and-instant poisoning).
- It rejects the one mechanism (CAG) that would silently reintroduce a trusted intermediary and break the moat from the inside.
- It is thesis-loyal: it returns convergence to its designed role as a *weighted proxy* backstopped by the behavioral loops, rather than overselling it as an oracle.
- Its cost to honest agents is a rounding error (~$3 one-time burn per persistent npub, $0/0ms on every lookup).
- It degrades gracefully: a strict no-op at the already-solved team tier.

### Parameter set to approve (recommended values)

| Parameter | Recommended value | Rationale |
|---|---|---|
| `B_full` (burn floor, Layer C) | **5,000 sats** (~$3), one-time, per persistent npub, global only | ~1/1000 of a single hit's value; kills drive-by sybils; team tier exempt |
| Vote dedup | **distinct-npub, first-event-only per `content_hash`** | Closes the ~$3 K=3 single-identity collapse |
| Burn caching | `verified_burn_sats` **cached at ingest** | Keeps chain-RPC off every read path (constraint 3) |
| Correlation features (Layer B, authoritative) | **F1 timing, F2 task-text, F3 embedding only** | Signed/reproducible → constraint-1 clean |
| F4 relay / F5 funding | **advisory flags only, non-authoritative** | Restores deterministic-fold invariant §134 |
| Combiner | **`min` or geometric-mean**, NOT count-gate | Closes the AND-gate evasion hatch |
| Discount formula | `K_eff = n/(1+(n−1)ρ)`, compare `K_eff ≥ 3` | Hard `1/ρ` ceiling regardless of ring size |
| Clique-recurrence discount | **ON** — same npub-set across unrelated hashes in rolling window → ρ boost | Re-couples cost to campaign breadth (fixes burn's fatal flaw) |
| PAC eligibility (Layer D) | **inverted `IsHighReuseArtifact` classifier**; high-probe-hardness + objective checker only | Confines PAC to the slice it actually secures |
| PAC event rate-limit | **bounded `kinds:3406/3407` per `(H,e,P)`** | Prevents verification compute-DoS |
| Behavioral decay (Layer A) | **fast loop, 5–60 min**, always on, all tiers | The real Layer-0 backstop; bounds every exploitation window |
| Convergence output | **continuous confidence multiplier**, never boolean | The philosophical heart of the ruling |
| Recompute cadence (Layers B/C/D fold) | **medium loop (1hr)**, cached | $0 on hot path; accept ~1hr staleness window |

### The decision in one line

**Adopt the layered, tier-aware defense: behavioral decay (backstop) + a determinism-restricted correlation discount (keystone) + a burn floor + PAC-where-eligible, all feeding a *continuous* convergence weight; reject CAG; and scope the moat — team/enterprise-binary, global-weighted — so that "3+ npubs converged" is never treated as a boolean at the permissionless tier, because it never can be one.**
