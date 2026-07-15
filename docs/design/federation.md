# Design: Federation — Cross-Operator Liquidity with Trust

**Date:** 2026-03-28 (original) / **rewritten 2026-07-15** for nostr-first infra
**Author:** Convention Designer (Opus) / doc-cascade rewrite per dontguess-fd8
**Status:** RULED. Router-mode federation is decision-complete on paper (§1). The `federation:*` nostr
event schemas, the `resell` put field, custodial-mode integrity (ADV-9), the ADV-11 trust-signal
weighting, and x402-escrow settlement are now **all ruled in the P9 decision doc**
(`docs/design/federation-infra-p9-router-decision.md`) — P10 (router mode) may build against it. Custodial
*implementation* remains a separate later item. **For the wire protocol, the P9 doc is authoritative
over this document** (this doc left it OPEN and deferred it to P9).
**Depends on:** `docs/design/content-confidentiality-envelope-541.md` (§541), `onboarding-tiered-scaling-federation.md` §5/§9

---

## 0. Infra rebase notice (read this first)

This document previously described federation over **campfire** (beacons, `core-peer-establish`,
campfire-signed messages on a shared federation campfire). DontGuess is now **nostr-first**
(v0.7.0 shipped, `docs/design/onboarding-tiered-scaling-federation.md`). `serve` is campfire-free.
Every mechanic below that referenced campfire primitives is rewritten onto **nostr relay peering**:

| Was (campfire) | Now (nostr) |
|---|---|
| Beacon on well-known beacon channels | Signed `dontguess:exchange` nostr event (operator npub, relay list, metadata), discovered via multi-directory + operator-pinned known-good first peers (ADV-14) |
| Shared federation campfire, campfire-signed messages | `federation:*` nostr event kinds, signed by each operator's secp256k1 key, relayed over each operator's own relay(s) — no shared campfire |
| `core-peer-establish` | Bilateral relay peering: each operator's relay accepts events signed by the peer's pinned operator npub per the agreed scope |
| Campfire trust (vouch/revoke) | Operator-level bilateral vouch + x402-stake admission for un-graduated operators (§3 below) — explicitly NOT agent-level web-of-trust (ADV-13 ruling) |

`pkg/exchange/state_federation.go` today is an **intra-exchange hop-depth tracker only** — it is
not cross-operator federation and has zero `federation:propose/accept/inventory-offer/match-request/
match-confirm/revoke/reconcile` handlers. Those handlers are P10 (blocked on this document's P9
completion).

---

## 1. Problem Statement

Anyone can operate a DontGuess exchange. Each operator runs their own relay-attached exchange,
maintains their own inventory, manages their own scrip ledger, and builds their own reputation
scores. This works well for a single exchange, but the marketplace is fragmented: a buyer on
Exchange A cannot discover inventory on Exchange B.

Federation lets two or more operators share inventory so that a buy request on Exchange A can
match a put from Exchange B. This increases liquidity for buyers (more inventory to search) and
reach for sellers (more potential buyers for their content).

The problem is trust, **compounded by §541 confidentiality**: team-tier content is envelope-encrypted
and only the home operator holds the CEK. Federation must provide liquidity without silently
widening who can decrypt a seller's plaintext, AND must contain the blast radius of a malicious
operator. §5.1 below is the ruling that reconciles these.

### What federation is NOT

- **Not a unified ledger.** Scrip balances remain local to each operator (`onboarding-tiered-scaling-federation.md` §5.3, F2). No cross-operator scrip transfer.
- **Not a unified reputation system.** Operator A's reputation scores are Operator A's opinion. Cross-operator trust must be derived ONLY from signals the receiving operator observes on its own buyers' local outcomes — never the partner's self-reported reputation (ADV-11 ruling, §3).
- **Not a global directory.** Discovery is multi-directory + pinned first peers over nostr, not a single central registry (ADV-14).
- **Not mandatory.** An operator can run a standalone exchange indefinitely. Federation is opt-in and deliberate (`federate` is a per-peer, scoped, revocable command — ADV-19 ruling, never a one-liner that hides the trust decision).
- **Not agent-level web-of-trust.** Federation trust is operator-level only (heavyweight, always-on, economically staked, few). Individual agents do not accrue federated reputation (ADV-13 ruling).

---

## 2. Design Principles (unchanged from original, still ruled)

**F1. Trust is bilateral and revocable.** Every federation relationship is a pair of operators
who mutually agree to share inventory. Either party can revoke at any time. No transitive trust
grants — A federating with B and B federating with C does not give A visibility into C.

**F2. Scrip stays local.** Cross-operator transactions settle bilaterally; no cross-operator scrip
mint (`onboarding-tiered-scaling-federation.md` §5.3).

**F3. Reputation is advisory, not authoritative.** The receiving operator's own local cross-operator
trust overlay (§3) — never the partner's claimed score — is authoritative for local buyers.

**F4. Federation is not free.** Un-graduated/untrusted operators pay per cross-operator match via
**pre-funded x402 escrow, never trailing bilateral credit** (ADV-12 ruling: trailing credit lets a
defaulting operator rotate to a fresh identity before reconciling and get content free). Bilateral
credit unlocks only after graduation plus a bounded exposure cap.

**F5. Blast radius containment.** Rogue-operator response: bilateral instant-revoke + behavioral
overlay + router mode by default (a rogue peer never holds the CEK and cannot exfiltrate plaintext
even if it turns malicious after federating).

**F6. Nostr relays are the substrate.** Federation uses nostr primitives: signed events for
discovery beacons, `federation:*` event kinds for inventory/match/settlement, each operator's own
relay(s) for delivery. No shared campfire, no shared transport identity between operators.

---

## 3. Trust model — RULED per `onboarding-tiered-scaling-federation.md` §5.1/§5.3

### 3.1 Confidentiality reconciliation — ROUTER default, CUSTODIAL opt-in

§541 requires cross-operator confidentiality to equal the **least-trusted federated operator that
can see plaintext**. The federation model resolves this as a **per-entry seller-consented mode**,
not a network-wide policy:

- **ROUTER mode (default, `resell: federation`).** The remote peer receives and shares only
  **metadata** (description, teaser, token_cost, domains, embedding) — **never the CEK**. On a
  cross-operator match, the remote peer forwards the buyer's **funded** reservation to the home
  operator; the home operator re-wraps the CEK **directly to the buyer**
  (`wrapped_cek_buyer = NIP-44(A_priv, buyerPub, CEK)`) and emits the deliver itself. The remote
  peer is a discovery/matching router only and never sees plaintext. §541's plaintext trust set
  stays at exactly `{home operator}`. Requires the home operator online at delivery (acceptable —
  operators are always-on infrastructure; transient-down is a refund/timeout path, §6).
- **CUSTODIAL mode (opt-in, `resell: custodial`).** The home operator re-wraps the CEK to the
  remote peer so the remote peer can deliver when the home operator is offline or re-pricing.
  Confidentiality is now the least-trusted of `{home, remote}`, exactly per §541. **Requires
  explicit per-entry seller opt-in at put time** via a new `resell: none|federation|custodial|<npubs>`
  field on the §541 v2 envelope (not yet implemented — tracked as new work under P9/P10, the
  envelope schema does not have this field today).
- **`resell: none`** keeps an entry home-only; also the safe default for entries the seller never
  wants federated.

**PERMANENT CONSTRAINT (informed consent, §8.9 below):** federating for resale in custodial mode
grants the peer read access to your plaintext corpus. Only use custodial mode with an operator you
would let read everything. A sybil operator can federate and request custodial resale of an entire
inventory to pull all of it plaintext — strictly worse than a passive relay scraper, who only ever
sees ciphertext. Mitigations: per-entry resale consent (above), re-wrap rate limits, and the
router/custodial split so discovery never implies a plaintext grant.

### 3.2 Content integrity across modes (ADV-9)

Router mode carries `ciphertext_hash` (not `sha256(plaintext)` — §541 §4.4 permanently removed the
plaintext hash from the wire as a guess-confirmation/correlation oracle). In router mode this is
sufficient: the home operator delivers its own ciphertext, so `ciphertext_hash` at match-confirm
matches `ciphertext_hash` at inventory-offer.

**Custodial mode breaks this** — a re-wrapping peer that re-encrypts produces a different
ciphertext, so `ciphertext_hash` no longer round-trips through the remote peer. **OPEN**: custodial
integrity needs a value that survives re-encryption (seller-signed plaintext commitment revealed
only post-purchase, or an operator-signed provenance chain). **Router-only federation can ship
without this. Custodial federation cannot ship until this closes** (P9).

Cross-operator plaintext dedup (an earlier open question in this doc, §8 Q2 originally) is **DEAD**:
random per-entry CEKs mean identical plaintext produces divergent ciphertext across operators, and
§541's keyed-HMAC fallback does not work across independent operator secrets. Conceded, dropped.

### 3.3 Discovery, join, settlement, abuse (ADV-11 through ADV-14, ADV-19)

- **Discovery.** Nostr beacons: an operator publishes a signed `dontguess:exchange` event (operator
  npub, relay config, metadata). Beacons are tainted — discovery is not trust. Eclipse defense:
  multiple independent directories plus operator-pinned known-good first peers.
- **Join model — bilateral invite/vouch + x402-stake, NOT open, NOT agent-level web-of-trust.**
  Operator-level vouching only (few, heavyweight, economically staked — sybil is expensive at that
  level even though agent-level WoT was explicitly rejected). New-operator trust starts at a LOW
  baseline (50/100, soft-suspend below 40, auto-revoke below 20, mirroring the original §6 trust
  score model below). **Mode 5 "open global liquidity" (federation-modes.md) is DEFERRED/OPEN** —
  do not promise it while the trust mechanisms that would reach it (agent-level WoT) are explicitly
  rejected.
- **Settlement.** Scrip stays local (no cross-operator mint, F2). Un-graduated/untrusted operators
  settle per-match via **pre-funded x402 escrow**, never trailing bilateral credit (ADV-12).
  Bilateral credit unlocks only post-graduation with a bounded exposure cap.
- **Sybil-operator trust gaming (ADV-11).** Cross-operator trust weighs ONLY signals the receiving
  operator observes on its OWN buyers' local outcomes — never a partner's self-reported reputation
  or convergence (a sybil owns its own ledger and buyers and can manufacture convergence). Fees for
  un-graduated operators are x402 (real money), never bilateral scrip credit.
- **Abuse/failure posture.** Rogue operator → bilateral instant-revoke + behavioral overlay + router
  mode (rogue peer never held the CEK, cannot exfiltrate plaintext). Sybil operator → x402 stake +
  low starting trust + observed-signals-only weighting. Key compromise → decrypts only that
  operator's own corpus (§541); router keeps CEKs home so federation does not widen this; custodial
  is the accepted seller-consented risk. Eclipse → multi-relay + multi-directory + pinned first
  peers. Free-riding → cross-operator matching fees + reciprocal-ratio terms in the bilateral
  agreement.
- **Deliberate, not one-command-trusting (ADV-19).** `dontguess federate <peer-beacon>` stays a
  per-peer, scoped, x402-escrowed, revocable action — never a single flag that silently grants
  broad trust.

---

## 4. Cross-Operator Trust Overlay (retained from original design, still the model)

Each operator maintains a cross-operator trust score derived **only from its own observations**,
never from a partner's claimed reputation (ADV-11).

| Signal | Source | Weight |
|--------|--------|--------|
| Match completion rate | Local observations | High |
| Dispute rate | Local observations | High |
| Buyer-reject rate | Local observations | Medium |
| Inventory freshness | Metadata | Low |
| Reconciliation reliability | Bilateral (x402 escrow settled on time) | Medium |

Trust starts at 50/100 for a new federation partner and adjusts (+2 per clean completed match, -5
per dispute resolved in the buyer's favor, -1 per post-preview buyer-reject, -10 per overdue
reconciliation, +5 per on-time reconciliation). At trust < 20, the operator auto-revokes
federation. At trust < 40, the partner's inventory is excluded from match results (soft
suspension) while the agreement stays nominally active.

---

## 5. What is still OPEN (do not implement past this line without P9 closing it)

- Full nostr event kinds/schemas for `federation:propose/accept/inventory-offer/match-request/
  match-confirm/revoke/reconcile` — the previous campfire-message versions of these are retired;
  their nostr-event replacements are P9 design work, not yet specified field-by-field.
- Custodial-mode integrity rebuild (§3.2).
- Whether Mode 5 (open global liquidity, `federation-modes.md`) gets designed at all, or stays
  permanently deferred (open question 3 in `onboarding-tiered-scaling-federation.md` §10).
- Exact settlement/reconciliation cadence and dispute-weaponization mitigations under x402 escrow
  (the original design's A2/A3/A5/A6 adversarial analyses below are retained as **prior art / still
  largely applicable reasoning**, but have not been re-verified against the escrow-first settlement
  model and nostr transport — treat as informative, not ruled).

### Retained adversarial analysis (informative, pending re-verification under nostr + escrow model)

**A2. Reputation inflation.** A rogue operator inflates local reputation to get inventory past a
federated partner's trust floor. Mitigation: reputation is advisory only; the receiving operator's
own outcome tracking (§4) overrides advertised scores over time; convergence-diversity checks
discount farmed convergence.

**A3. Dispute weaponization.** A rogue operator's buyers file frivolous disputes to drain a remote
seller's reputation. Mitigation: disputes are local to the exchange where they're filed; cross-
operator dispute-rate tracking feeds the trust overlay; the preview model structurally reduces
post-purchase disputes.

**A5. Free-riding.** An operator consumes federated inventory without contributing. Mitigation:
cross-operator matching fees (now x402 for un-graduated operators) create friction; bilateral
agreements may specify reciprocal-ratio terms.

**A6. Operator impersonation.** An attacker mimics a legitimate operator's beacon. Mitigation: the
operator's nostr pubkey IS its identity — beacons are signed events, unforgeable without the
private key; out-of-band key verification (website, DNS, personal contact) before accepting a
federation proposal.

---

## §8.9 Informed consent language (§541, mandatory reading for operators and sellers)

This is the permanent trust-model disclosure required before any operator federates. It must ship
verbatim (or materially equivalent) in operator-facing and seller-facing surfaces:

> **Your home operator can read your plaintext content.** Team-tier content is envelope-encrypted,
> but the home operator holds the CEK to service matches — the exchange owns and re-wraps content
> to buyers, which requires plaintext visibility (§541).
>
> **Federating for resale (custodial mode) extends that trust to the remote peer.** If you opt an
> entry into `resell: custodial`, the remote operator can also decrypt it — this is a deliberate,
> per-entry choice, not a side effect of discovery. Router mode (the default) never extends this:
> a router peer sees only metadata and ciphertext hashes, never the CEK.
>
> **There is no forward secrecy.** A single operator-key leak decrypts that operator's **entire
> historical corpus**, offline, from data already scraped off the relay and Blossom (§541 A4/P5).
> Rotating the operator key does not protect content encrypted under the old key.
>
> **There is no content revocation once public.** Ciphertext, once published to a relay, is
> append-only and cannot be withdrawn (§541 §8 item 8). Withdrawal is economic/reputational only,
> never cryptographic.
>
> Only federate in custodial mode with operators you would trust to read everything you've ever
> put. Router mode is the safe default for liquidity without that exposure.

---

## Superseded sections

The original §3 (Federation Protocol: Discovery/Agreement/Inventory Sharing/Cross-Operator
Match/Bilateral Settlement/Revocation), §5 (Convention Operations table), and §7 (Settlement Flow
Detail sequence diagram) described these mechanics **on campfire** and are retired. Their nostr-
event replacements are P9 design work (`onboarding-tiered-scaling-federation.md` §9, Gate C) —
do not resurrect the campfire-era message names (`federation:propose` etc. as campfire messages)
as implementation targets; the *operation names* survive conceptually, the *transport* does not.
