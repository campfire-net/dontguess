# Federation Modes — Deployment Tiers

**Status:** Design — rewritten 2026-07-15 for nostr-first infra (dontguess-fd8 doc cascade)
**Date:** 2026-04-01 (original) / 2026-07-15 (rewrite)
**Depends on:** `docs/design/onboarding-tiered-scaling-federation.md` (the current ladder design,
supersedes the mode numbering below for anything already shipped), `docs/design/federation.md`
(trust/confidentiality model for Modes 4-5)

---

## Infra rebase notice (read this first)

This document originally described the deployment ladder as a property of **campfire** ("DontGuess
is a convention on a campfire ... federation is a property of the campfire the exchange runs on").
That premise is retired. DontGuess is **nostr-first** as of v0.7.0 (`serve` is campfire-free).
The current, load-bearing tier ladder is **solo → fleet → federation**, defined in
`docs/design/onboarding-tiered-scaling-federation.md` §0-§6. This document's five-mode framing below
is kept only as historical deployment-tier reasoning and is renumbered/annotated against the current
ladder — **treat `onboarding-tiered-scaling-federation.md` as authoritative wherever the two disagree.**

| Original mode | Current equivalent | Status |
|---|---|---|
| Mode 1: Project (filesystem-local campfire) | **SOLO** (`dontguess up`, local, no relay) | Shipped, nostr-native, no campfire |
| Mode 2: Individual (center campfire, cross-project) | Folded into SOLO — one local operator identity covers all projects on a machine | Shipped |
| Mode 3: Team (hosted/self-hosted/direct-P2P campfire) | **FLEET** (`dontguess up --relay`, strfry relay, roster-gated allowlist) | Shipped, nostr relay replaces all three campfire hosting options |
| Mode 4: Organization (campfire peering + `federation:*` messages) | **FEDERATION, router mode** | Trust model RULED (`federation.md` §3.1/§3.3), wire protocol RULED (`federation-infra-p9-router-decision.md`, P9 closed), code = P10 (unblocked) |
| Mode 5: Global (agentic-internet directory + routing + portable trust) | **FEDERATION, open/global** | DEFERRED — `onboarding-tiered-scaling-federation.md` §10 open question 3; do not promise this while agent-level web-of-trust is explicitly rejected (see `federation.md` §3.3) |

---

## SOLO and FLEET (current, shipped tiers)

See `docs/design/onboarding-tiered-scaling-federation.md` §1 (`dontguess up`) and §6 (tier
transitions) for the authoritative bootstrap flow, operator identity model (one secp256k1 key from
first `up` onward), and the allowlist unification design (§2-§3). Not repeated here to avoid drift
— that document is the source of truth for solo/fleet mechanics.

**E2E proof:** `test/e2e-install.sh` and the Gate A/B test suite defined in
`onboarding-tiered-scaling-federation.md` §9 cover solo and fleet. Do not add new campfire-era
E2E scripts (`TestMode1_ProjectLocal` / `TestMode2_UserLocal` / `TestMode3_Team` below are retired
campfire-era tests; their nostr-native replacements live under the Gate A/B test plan).

---

## FEDERATION (router mode) — trust model ruled, wire protocol OPEN

Multiple operators, each running their own relay-attached exchange. Selective inventory sharing
via bilateral, revocable agreements — never a shared campfire, never transitive trust.

- **Discovery:** nostr beacons — a signed `dontguess:exchange` event (operator npub, relay list,
  metadata) discovered via multiple independent directories plus operator-pinned known-good first
  peers. See `federation.md` §3.3.
- **Trust model:** bilateral invite/vouch + x402-stake for un-graduated operators, NOT open,
  NOT agent-level web-of-trust. See `federation.md` §3.1/§3.3 for the full ruling (ADV-11/12/13).
- **Confidentiality:** ROUTER mode by default — the remote peer never sees plaintext, only
  metadata + ciphertext hashes; the home operator always delivers. CUSTODIAL mode is a per-entry
  seller opt-in with its own unresolved integrity gap (ADV-9). See `federation.md` §3.1/§3.2.
- **Settlement:** scrip stays local (no cross-operator mint); un-graduated operators settle via
  pre-funded x402 escrow, never trailing bilateral credit (ADV-12).
- **Wire protocol (`federation:*` nostr event kinds for propose/accept/inventory-offer/
  match-request/match-confirm/revoke/reconcile):** **RULED** in `federation-infra-p9-router-decision.md`
  (P9 closed) — one shared `KindFederation=3420` + `["op",…]` sub-ops, addressable beacon
  `KindExchangeBeacon=30402`, each op specified field-by-field. **P10 (`dontguess federate`, router
  mode) may now build against the P9 doc.**

**Infrastructure required:** Always-on relay-attached hosting for each exchange (same as FLEET) —
federation cannot run from a laptop that sleeps.

**E2E proof needed (post-P9):** two independent nostr-attached exchanges, federation propose/accept,
router-mode cross-operator match with confidentiality-property verification (peer never receives
the CEK; a passive scrape of the shared relay channel yields only metadata + ciphertext —
`onboarding-tiered-scaling-federation.md` §9, "Ground-source testing" paragraph).

---

## FEDERATION (open/global) — DEFERRED

Same protocol as router-mode federation, but discovery and trust operate at internet scale via the
agentic internet infrastructure (directory service, `cf://`-style naming, path-vector routing,
portable reputation). **This tier is explicitly deferred** — `onboarding-tiered-scaling-federation.md`
§10 open question 3 leaves it unresolved whether to design the sybil economics for open federation
at all. Do not promise open/global federation while the bilateral-only, agent-level-WoT-rejected
trust model (§3.3 of `federation.md`) is the ruled posture; they are in tension by design and Mode 5
stays out of scope until a human decision reopens it.

**Infrastructure required (if ever built):** Always-on hosting, participation in an agentic-internet
directory, x402 settlement rail (no bilateral credit with strangers at this scale).

---

## §8.9 Informed consent (cross-reference)

Federating in custodial mode grants a remote operator plaintext read access to your resold
inventory. Router mode never does. See the full consent block in `docs/design/federation.md`
(§8.9) and the root `CLAUDE.md` publisher-model section — both must carry this language verbatim
per `content-confidentiality-envelope-541.md` §8 item 9.

---

## Infrastructure Layer Ownership (unchanged in spirit, retargeted off campfire)

DontGuess does not build relay or transport infrastructure from scratch. It composes:

| Layer | Provides |
|-------|----------|
| **Transport** | strfry (or compatible) nostr relay: event storage, subscription, `writePolicy` allowlist gating (`onboarding-tiered-scaling-federation.md` §2) |
| **Identity** | secp256k1 operator keypair, NIP-42 auth, NIP-44 envelope encryption (§541) |
| **Settlement** | x402 (USDC) for un-graduated cross-operator fees; local scrip ledger for everything within one operator |

DontGuess owns the **application layer**: exchange conventions (put, buy, match, settle),
federation conventions (propose, accept, inventory-offer, match-request, reconcile — nostr event
kinds, OPEN per above), matching engine, pricing engine, scrip ledger.

---

## E2E Test Strategy

Each tier needs its own E2E test proving the seam works over the current (nostr) transport. Do not
resurrect the campfire-era `TestMode*` names below as new work — they tested a transport this repo
no longer has.

| Tier | Test | Status |
|------|------|--------|
| SOLO | local bootstrap → put → buy → match, no relay | Covered by Gate A/B tests (`onboarding-tiered-scaling-federation.md` §9) |
| FLEET | relay-attached bootstrap → admit → put → buy → match | Covered by Gate A/B tests |
| FEDERATION (router) | two exchanges → propose/accept → cross-operator match → confidentiality-property check | **Blocked on P9** (wire protocol undesigned) then P10 |
| FEDERATION (custodial) | as above + re-wrap integrity proof | **Blocked on P9 custodial integrity rebuild** (ADV-9), separate later item after router ships |
| FEDERATION (open/global) | directory discovery → cross-network match → x402 settle | **Deferred**, not scheduled |

**Rule (unchanged): if it doesn't have a passing E2E test, it doesn't go on the website.**
