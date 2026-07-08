<!-- source: adversarial-design workflow wf_9bcb90f1-2fb (7 agents, 2026-07-06); rd dontguess-5b6 -->

# dontguess — Nostr-First Direction: Rebuild Decision & Feature Set

## Context & Locked Decisions

dontguess is a token-work exchange: agents buy and sell cached inference so the portfolio stops re-deriving the same knowledge. The unit of value is **net tokens saved per lookup** — a hit saves 50–100K tokens, a miss costs ~500. Everything else (scrip, pricing loops, federation, provenance, relays, zaps) is instrumentation that either serves that unit or is fat.

The current implementation is an event-sourced Go operator: state is a deterministic fold over signed messages. Verified against HEAD `6359f51`:

- **52,072 non-test / 133,575 test LOC** total. `pkg/exchange` is **9,483 non-test / 32,209 test** — a 3.4:1 ratio, and **46 of 75 test files (>60%) are named-bug regression tests** (`dontguess-*`/`rudi-*`). The test suite *is* the spec.
- **`pkg/matching` (1,315 non-test) and `pkg/pricing` (2,708 non-test) have zero campfire imports** — fully transport-agnostic.
- Campfire coupling is **15 non-test files** behind one clean seam (`pkg/proto.Message`, with `FromStoreRecord`/`FromSDKMessage` the only conversion boundary) plus **one leak**: a direct dependency on `github.com/campfire-net/campfire/pkg/provenance`, touching **7 files** (`provenance.go` + 4 provenance tests + `engine_core.go` gating + `cmd/dontguess/serve.go`) — corrected upward from the code-recon's count of 4.
- `Antecedents[0]` is used at **47 sites** as a *single direct-parent pointer*, never multi-parent threading.
- No `Verify()`/signature call exists in `pkg/exchange` or `pkg/scrip`: `msg.Sender` is a trusted, already-verified hex pubkey string by the time engine logic sees it.

**Two locked inputs, not open questions:**

1. **Campfire is cancelled portfolio-wide.** dontguess must move off it. (This exchange has already lost its identity/data once to a `cf gc` incident — see operator memory. There is no stable live system worth heroic preservation effort.)
2. **dontguess is nostr-first.** Firm user decision.

Identity re-key **Ed25519 → secp256k1/schnorr is forced** regardless: campfire identities are dead.

---

## Q1 — Clean-Slate vs Evolve vs Strangler-Fig: THE DECISION

### Decision: **EVOLVE the value logic; REDESIGN the fold ingest. Reject clean-slate. Reject the strangler-fig dual-transport bridge.**

This is the one call the recon framing gets subtly wrong on both extremes, so state it precisely:

- **The 21K LOC of value logic** (9.5K engine core + 1.3K matching + 2.7K pricing + convention + scrip interface) **and its 32K LOC regression spec are preserved and evolved** behind the existing `proto.Message` seam. They never touch the wire.
- **The fold *ingest* is genuinely rebuilt**, because nostr breaks the one assumption `engine_core.go`'s `replayAll`/dispatch (925 LOC) is built on: mostly-ordered, causally-delivered messages. This is not a mechanical port. It is new Layer-0 correctness engineering. Calling it a "port" is the trap the creative disposition correctly named.

So the honest shape is **evolve everything that is transport-agnostic, redesign the ~1K LOC of ordering-dependent ingest, and build two net-new components (provenance/trust, blob storage) that die with campfire under *any* path.**

### Why not clean-slate

The thesis does not live in the transport. A rewrite spends its **entire budget re-deriving correctness orthogonal to discovery**, and the recon evidence says it re-derives it *worse*:

- a blocklist instead of `IsHighReuseArtifact`'s token-shape classifier — which already survived **two** adversarial bypasses (dontguess-ce8, dontguess-a0e);
- a quietly-failing embedder instead of the loud `OnError` hook — dontguess-553 ran silently wrong for **weeks**, every buy degrading to `confidence=0.5` reputation-fallback, zero log signal;
- a naive lock-then-write that reopens the scrip double-spend window `pkg/scrip`'s ETag + atomic `ConsumeReservation` already closed.

Cost model, realistic (150 LOC/day for security/economics-hardened Go, not green-field CRUD): re-deriving the 9,483-LOC engine core is **~63 engineer-days**; reproducing even the ~8–10K LOC high-value subset of the named-bug regressions adds **~50–65 days**. **Total ~110–130 engineer-days (5–6 weeks, 4-person team)** — *plus an unbounded, invisible incident-discovery tax*. The adversary's A4 is the decisive point: **time-to-parity is bounded by incident-discovery latency, not LOC.** dontguess-553 hid for weeks; a rewrite re-walks that minefield one live production incident at a time. There is no way to write code faster to compress that tail.

### Why not strangler-fig (dual-transport bridge)

The creative disposition's strangler-fig phasing is *analytically* correct and I adopt its core insight (the fold is new engineering, not a port — see below). But I **reject the dual-transport parallel-run bridge** on this project's specific facts, per the pragmatist: campfire is a *cancelled* dependency, and this exchange has *already lost its data once*. You do not build careful parallel-run safety infrastructure to protect a migration off an already-unstable, already-dead-once system. **Do a one-time historical replay** (`FromStoreRecord` over surviving campfire history → seed the new local/relay store, ~1 week script) **and cut over.** No bidirectional shim.

### Cost of the chosen path

| Item | LOC (new/touched) | Notes |
|---|---|---|
| Nostr event adapter (`FromNostrEvent`, tag mapping) | 300–500 | `exchange.Message = proto.Message` alias ⇒ **zero lines change in `state_*.go`/`engine_*.go`** if field semantics preserved |
| **Local sequencer + orphan-antecedent buffer** | 500–1,000 + property tests | **Genuinely new. This is Layer-0 correctness, not adapter glue.** |
| Provenance/trust replacement | 200–400 direct; budget 800–1,500 effort-equiv | New *design*, not port; security review multiplier. Orphaned by campfire under all paths. |
| Blob/Blossom integration | 300–600 | Touches `state_put.go` payload path only |
| Scrip transport swap (`campfire_store.go` → `relay_store.go`) | ~400 | `SpendingStore` interface (ETag, atomic reservations) **unchanged** |
| CLI (7 files: agent_init, convention_supersede, demand, hitrate, serve, status, cmd/seller) | mechanical | includes rebuilding 3 observability commands (`hitrate`/`status`/`demand`) that consumed `views.go` outputs — real work, not free (adversary A7) |
| Delete `views.go` | −146 | Replaced by relay `REQ` filters |
| One-time campfire→new-store replay | ~1 week script | Cut over, no bridge |

**Total ~2,500–4,500 LOC touched against ~53K non-test / 133K test LOC that do not move.** At 100–150 LOC/day for adapter-and-regression-safe work: **~20–35 engineer-days ≈ 4–7 weeks solo, 2–3 weeks for 2 engineers** (one adapter/sequencer engineer, one provenance/trust engineer — the two separable design surfaces).

### The correctness-asset re-derivation risk, head-on

This is the single most important thing to not fumble. **The migration is gated on one non-negotiable acceptance test: the existing 32,209-LOC exchange test suite must compile and pass *unchanged* against the new adapter.** Those tests operate on `proto.Message`/`exchange.Message`, not on `store.MessageRecord`/`protocol.Message` (confirmed: zero test files construct `store.MessageRecord` outside `cmd/` CLI tests and the boundary files). A **passing suite is the proof the swap was thesis-neutral.** If a test needs rewriting, the adapter changed semantics and you have drifted — that is a regression, not a migration, and it must not ship. This converts "we hope the value survives" into a mechanical, verifiable gate.

### Dissent overridden

- **Creative — strangler-fig with a parallel dual-transport phase.** Overridden on the bridge specifically (cancelled + already-lost-data-once ⇒ replay-and-cut, not parallel-run). **Adopted** its central correctness insight: the fold ingest is redesign, not port. Named it Layer-0 work so it cannot be under-scoped as "adapter glue."
- **Adversary A3/A9 — "the in-memory `gen uint64` ETag breaks under non-reproducible multi-relay replay; this is shared by both paths and not solved by choosing evolve."** *Sustained, not overridden* — and it is exactly why the sequencer is elevated to Layer-0. Resolution (below): the operator remains the **single authoritative sequencer per domain**; relay ingest order is *not* fold order; a local monotonic sequence assigned at operator ingest is fold order, preserving the single-deterministic-writer invariant the ETag scheme depends on.
- **Adversary A6 — provenance is the real sybil gate, mis-costed as a port.** Sustained; provenance is broken out as its own workstream with a 3–4× effort multiplier and a first-class trust primitive, not a find-and-replace line item.
- **Recon-code's "provenance touches 4 files / views.go is free."** Corrected to 7 files, and the CLI rebuild cost is made explicit.

---

## Functional Feature Set

Organized by the four tiers. The frame holds: **the authoritative-operator footprint shrinks as nostr's footprint grows** — but at global tier the operator shrinks to *"one matcher among many,"* which is a business model, not an absence of one (creative). Features are marked with the tier they **first appear** in and carry upward unchanged.

### Individual (INDIV) — a personal semantic cache, zero network
- **Irreducible core loop [INDIV]:** embed task description → ANN-search local semantic index → return best match + confidence; on miss, register demand locally, agent computes, then `put`.
- **Put quality gating [INDIV]:** `IsHighReuseArtifact` token-shape classifier — corpus signal quality is thesis-critical (a junk put is a future wasted lookup). Survives verbatim.
- **Behavioral capture [INDIV]:** consume/return/retry counts feed ranking. No ratings, ever.
- **Dynamic pricing loops (fast/medium/slow) [INDIV]:** run as a *ranking-quality signal only* — no scrip moves, no money. Computed locally on the fold.
- **Value-stack Layer 0–4 + escape-velocity metric [INDIV]:** this *is* the product; transport-agnostic.
- **No scrip. No nostr. No identity ceremony.** Mandating a relay for one agent is infrastructure fetish (purist refusal #3).

### Team (TEAM) — one authed relay, allowlisted fleet
- **Relay-backed durability [TEAM]:** the exchange log lives on one NIP-42-authed relay; if the matcher dies, the data survives. This is where a relay first earns its keep (a lost entry = guaranteed re-derivation = token waste).
- **Scrip ledger [TEAM]:** internal accounting only, off-nostr. Mint/burn/pay/assign-pay/loan semantics unchanged; ETag optimistic locking + atomic reservation-consume preserved.
- **Fleet identity [TEAM]:** each long-lived fleet member gets a persistent npub (`dontguess agent-init`). Convergence independence is measured at npub granularity (see key-management ruling in NFRs).
- **Assigned maintenance tasks [TEAM]:** context compression, validation, freshness checks — paid in scrip.
- **Provenance/trust gate [TEAM]:** NIP-42 allowlist + operator-only write authority for `match`/`settle`/`mint`/`burn` (see enforcement model — NIP-42 alone is insufficient).

### Enterprise (ENT) — federated relays, trust boundaries, chargeback
- **Federation [ENT], gated on measured cross-domain reuse:** existing `exchange-federation` propose/accept/reconcile machinery, now over multiple team relays. NIP-65 outbox for *discovery routing only* — it does not provide trust (recon-nostr finding #5; adversary A14 sustained).
- **x402/USDC chargeback [ENT]:** stablecoin invoicing between known legal entities. Per-domain settlement + reconciliation.
- **Per-domain matchers [ENT]:** each domain runs its own sequencer/fold; reconciled via federation.
- **Operator HA [ENT]:** single-writer-per-domain with leader election, introduced *only* when an enterprise SLA demands multi-replica uptime (adversary A15 — real distributed-systems work, deferred not ignored).

### Global (GLOBAL) — open relay mesh, value between strangers
- **Public auditable cross-agent convergence [GLOBAL]:** the genuine nostr-first win. On public signed `match`/`settle` events, "N independent npubs across M operators converged on `content_hash` X" becomes **third-party verifiable** — the moat goes from private assertion to public proof. Endorsed *specifically for this*, not for durability or value rails.
- **Zap-to-buy [GLOBAL]:** a stranger's agent zaps (NIP-57) a `put` event, gets a Blossom pointer back — no account, no relationship. Smallest unit of distribution = a signed event + a satoshi.
- **Zap-split residuals [GLOBAL]:** NIP-57 native weighted recipient tags route the 10% author residual to the original npub — no custom split logic.
- **Content-hash residual merging [GLOBAL]:** two operators independently compute identical content → identical sha256 → auto-merge residual streams / fair split, instead of the second author undercutting the first. (Build the join-key logic early — inert until the mesh has collisions.)
- **Competing-matcher marketplace [GLOBAL]:** anyone can index the public commons; dontguess competes on match quality, not exchange-fee monopoly.
- **Convergence Oracle / proof-of-savings feed / token-cost price index [GLOBAL]:** sellable external data products that fall out of public signed events. Speculative — flagged YAGNI until team tier is proven.

### Cross-tier, all tiers
- **Identity-migration attestation bridge:** one-time, opt-in, operator-reviewed Ed25519→secp256k1 cross-signed pair, published as a **self-verifying single event** carrying both signatures (creative C13) — no off-protocol lookup table. Default remains heritage's *"reputation is not transferable between keys."*
- **Large content via Blossom:** content-addressed by the existing engine sha256; preview chunks stay inline; full deliver is a pointer + verify-on-fetch (free content-hash-spoof mitigation).

---

## Non-Functional Requirements

**Master NFR (all tiers): net token savings per lookup.** Hit saves ≥50K; miss costs ≤~500. **Relay/network reads are async cache-warming and MUST NEVER sit on the hot path of a `buy` or a match.** Every higher tier must justify its added lookup cost with proportional reuse.

| NFR | Individual | Team | Enterprise | Global |
|---|---|---|---|---|
| **Match latency** | **<50 ms p99** (in-process ANN, local disk) | **<50–100 ms** (matching stays local against the operator's own fold; relay round-trip never in critical path) | same, per-domain | same per matcher |
| **Event throughput** | n/a | **~10–50 events/min peak** (portfolio reality: ~20 projects × tens of ops/day) — 3+ orders below strfry-class relay capacity (~thousands/sec). **Build for correctness, not throughput.** | O(N) relays, each as cheap as team | speculative — no target yet |
| **Durability** | RF=1, single local sqlite/badger. Loss = recompute, acceptable (it's a cache). | RF≈2: single relay + operator's local replayed fold. Multi-relay replication is an *enterprise* concern. | multi-relay replication; add redundancy *after* an actual outage, not before | open mesh |
| **Blob storage** | none | **~10–50 MB total** (10–50 puts/day × ~10 KB × 90-day retention) — single Blossom instance, no sharding/CDN | per-domain Blossom | mirrored Blossom |
| **Sequencer** | none needed (single local writer, trivially ordered) | bounded orphan buffer ~1,000 pending events, few-second retry window — *not* a throughput bottleneck at this load | single-writer-per-domain + leader election for HA | independent per-domain, no shared total order |
| **Key mgmt** | one local secp256k1 key (µs keygen); NIP-42 handshake <10 ms | operator npub (signs match/settle/mint) distinct from per-fleet-member npubs; **ephemeral subagents sign with their parent fleet-member's key — NOT a throwaway key, NOT the operator key** | per-org key hierarchies | portable npub = reputation passport |
| **Security/trust-min** | local only | NIP-42 allowlist **+ per-kind write ACL or universal client-side re-verify** of operator-authored events | federation trust-model on top of NIP-65 | cost-per-identity / NIP-05 attestation gates convergence weight |
| **Verifiability of convergence** | n/a (private) | recomputable by any allowlisted reader from the relay log | cross-domain, reconciled | **third-party auditable from public signed events — the moat** |
| **Operability** | zero-config binary | one relay + allowlist config | federated relay config + reconciliation | mesh config |

**Determinism of the fold (all tiers, non-negotiable):** anyone holding the event log recomputes byte-identical state and identical Layer 0–4 metrics. This is why event-sourcing survives intact.

**Loud degradation (all tiers):** every Layer 0–4 metric regression is alarmed. The dontguess-553 silent-embedder failure becomes a system-wide invariant — carry the `OnError`-hook discipline into *every* new integration (adapter, blob fetch, relay read), not just the embedder.

**Key-management ruling (resolves adversary A17 — the critical unaddressed gap):** convergence is only meaningful if independent derivers are independent identities. Therefore: (1) operator identity is a single long-lived npub that signs `match`/`settle`/`mint`/`burn`; (2) each long-lived fleet member has a persistent npub; (3) **ephemeral per-conversation subagents sign with their parent fleet-member's npub** — never a fresh throwaway (unbounded churn destroys reputation continuity) and never the operator key (collapses convergence to "the operator agreed with itself" — worthless). Convergence independence is scored at npub granularity, and **cross-operator** convergence (different operators' npubs) is the strongest signal.

---

## Nostr Architecture

### Event model / kinds / tags

| Operation | Kind (draft — collision-check the live NIP registry before locking) | Type | Key tags |
|---|---|---|---|
| `put` | 3401 | regular, immutable | `content_hash` (engine sha256), Blossom pointer, domain `#t` tags; **NOT raw content above trivial size** |
| `buy` | 3402 | regular, immutable | task descriptor; future-fulfillment handled operator-side |
| `match` | 3403 | regular, immutable | `["e", <buy-id>, "", "reply"]` — direct equivalent of `Antecedents[0]` |
| `settle` | 3404 | regular, immutable | `phase` tag (mirrors today's `exchange:phase:*`); `e`-tag chains to prior phase |
| `assign*` (7 sub-ops) | 3405 | regular, immutable | single kind + `["op", <sub-op>]` discriminator (a bare `phase` tag cannot distinguish 7 assign sub-ops — see below) |
| scrip ops (mint/burn/pay/loan) | 3411 | regular, immutable | single kind + `["op", <sub-op>]` discriminator; **team-tier, on the authed relay** |
| **inventory + dynamic price projection** | **30401** | **addressable** (`d`-tag = `content_hash`) | operator-republished *projection*, latest-wins |

**Op-discriminator, not phase-tag, is the mechanism for shared kinds (ratified dontguess-c08, reconciling the table above with the shipped `pkg/nostr` adapter):** `assign*` has 7 sub-ops (`assign`, `assign_claim`, `assign_complete`, `assign_accept`, `assign_reject`, `assign_expire`, `assign_auction_close`) sharing kind 3405; a single `phase` tag (borrowed from `settle`, which only ever has a handful of phase values) cannot losslessly distinguish 7 sub-ops from the kind + phase pair alone without ambiguity against `settle`'s own phase vocabulary. The adapter instead emits an `["op", <sub-op-tag>]` tag on every 3405/3411 event and treats it as an authoritative discriminator on decode: `FromNostrEvent` validates the tag value against the known `assignOps`/`scripOps` set for that kind and fails loudly (returns an error) on an unrecognised value, and ignores/rejects a stray `op` tag found on a base-kind event (3401-3404), which fully determines its op from the kind alone. This table's `phase` mention for `assign*` is superseded by the op-tag mechanism; `phase` may still appear on assign/settle events for phase state independent of the op-tag sub-op selector.

**`Antecedents[0]` → `e`-tag is clean and low-risk:** code reads only index `[0]`, so this is NIP-01's simple reply marker, not NIP-10 threading. No impedance mismatch.

**The 30401 addressable event is a projection, NOT source of truth.** This is the single highest-risk misreading (recon-nostr finding #3, purist ratifies): the operator republishes it from its own fold whenever fast-loop price/availability changes — it does **not** replace replaying the immutable put/settle log that guards scrip double-spend. Mistaking the addressable event for truth reintroduces already-fixed bugs.

**Embedding vectors NEVER go on the wire (hard constraint, all four sources agree):** no relay can ANN-search a 384-dim blob; cross-operator embeddings are model-incompatible; publishing ~2 KB base64 per put is pure relay bloat for zero protocol benefit. Embeddings live only in the operator's local vector index, keyed by `content_hash`.

**Delete `views.go`** — its 12 tag-predicate views map ~1:1 onto NIP-01 `REQ` filters (`kinds` + `#t` + `since/until/limit`), native to relays. Rebuild the three CLI commands that consumed it (`hitrate`/`status`/`demand`) against `REQ` filters.

### Sequencer per domain (Layer-0 correctness — the piece that is genuinely rebuilt)

Nostr gives **no total order and no causal delivery** — an `e`-tagged event can arrive before its antecedent under multi-relay publish. Resolution, preserving the deterministic single-writer invariant the scrip ETag depends on:

1. **The operator is the sole authoritative sequencer per domain.** Relay ingest order is *not* fold order.
2. On ingest, the operator assigns a **local monotonic sequence number**; the fold replays in *that* order — identical trust boundary to today's single-campfire "observed receipt time" model (convention §S7 ports directly).
3. **Multi-relay reads dedup by event id**; ties broken by local ingest timestamp/id.
4. An **orphan/pending-antecedent buffer** holds events whose claimed `e`-tag antecedent hasn't arrived, retrying when it lands, with a **loud failure path** when an antecedent is provably unrecoverable (relay-pruned). This closes adversary A10: a pruned antecedent must *fail loudly*, never silently corrupt a settle chain the way dontguess-553 degraded silently.

This resolves adversary A3/A9: the `gen uint64` in-memory ETag stays safe because there remains exactly one deterministic, single-writer replay source per domain — the operator's local ingest sequence, not the relays.

### Enforcement model (dumb relays)

Relays enforce only NIP-01 syntax + chosen extensions (NIP-42 auth, NIP-13 PoW). All conformance semantics (operator-only sender for `match`/`deliver`, antecedent-chain correctness, price ≤ budget) move to **client-side verification by every reader** — consonant with the convention's existing TAINTED-field model (tags/payload/antecedents were already sender-asserted). `pkg/convention/validate.go` is re-homed as the **portable client-verification library** every dontguess-compatible reader runs; its role shifts from *gate* to *filter*.

**NIP-42 secures the pipe, not the operation (adversary A12, sustained).** A NIP-42 allowlist proves "this connection is an allowlisted npub" — it does *not* stop an allowlisted npub from forging `match`/`settle`. Recover the operator-forgery closure (dontguess-2f9) two ways in combination: (a) relay-side **per-kind write ACL** (non-standard relay policy, ops burden) rejecting non-operator `match`/`settle`/`mint` at publish time; **and** (b) universal client-side re-verification that these events were authored by the operator key — because garbage claiming operator authorship can land on any *other* relay in the mesh.

### Blob storage

**Blossom, not NIP-96** — content-addressed by the same engine sha256, mirrorable across independent hosts (durability without dontguess being a single point of failure). Preview chunks (`settle:preview`, 15–25%) stay inline; full deliver is a Blossom pointer + client-side hash verification (verify-on-fetch = free content-hash-spoof mitigation).

### Identity migration

No cryptographic bridge between Ed25519 and secp256k1. Mechanism: a one-time cross-signed attestation, published as a **single self-verifying event** carrying both signatures (old key signs "my new npub is X", new key signs "migrating from campfire key Y") — no off-protocol table, so dontguess's own index need not be trusted (creative C13). **Default: reputation is not transferable between keys** (heritage §7.4). The bridge is opt-in, operator-manually-reviewed, low-volume. All pre-cutover campfire provenance otherwise dies at cutover; continuity requires a one-time operator-signed reputation-snapshot checkpoint.

### Value layer per tier

| Tier | Mechanism |
|---|---|
| Individual | none |
| Team | internal scrip, off-nostr (nostr is transport/durability only here) |
| Enterprise | x402/USDC — stablecoin chargeback between legal entities; federation propose/accept/reconcile still required for *trust* (NIP-65 = discovery only) |
| Global | NIP-57 zaps; zap-splits for residuals |

**Zaps are quarantined to settlement + residual routing. They MUST NOT feed the ranker or the completion signal (purist refusal #2, sustained).** A zap is buyable and wash-tradeable; payment ≠ task completion. Completion stays **behavioral** (retry / return-rate / consume-count), computed independently of payment. This overrides recon-nostr's clever proposal to reuse the kind-9735 zap receipt *as* the `settle(complete)` signal — that reintroduces the exact "fast + confident + wrong generates positive telemetry" failure the value-function doc was built to kill. The zap receipt may *co-occur* with settlement for value transfer; it does not *define* completion.

---

## Build Spine

### M1 — Individual, smallest useful core (days, mostly deletion)
Delete the campfire dependency from `engine_core.go`'s transport calls; write a ~200 LOC local store (sqlite/badger) implementing the record shape `FromStoreRecord` expects. No nostr, no relay, no scrip, no identity questions. **Ships standalone value** (a single-agent semantic cache) and de-risks the matching+pricing engines against *a* transport swap before nostr enters. Single local writer ⇒ no sequencer, no orphan buffer.

### M2 — Team, one relay (the tier that matters for near-term shipping; 2–3 weeks / 2 engineers)
Nostr adapter (`FromNostrEvent`) + single NIP-42 relay + allowlist + local sequencer/orphan-buffer + scrip transport swap (`campfire_store.go` → `relay_store.go`, interface unchanged) + Blossom pointer path + local provenance replacement (scoped to NIP-42 allowlist + the reputation scoring already in `pkg/exchange` — **not** a full web-of-trust on day one).
**Acceptance gate (the done-condition, verified empirically, not on faith):** the existing 32,209-LOC exchange test suite passes *unchanged* against the new adapter.

### M3 — Enterprise, federation prep (triggered by a condition, not a date)
NIP-65 outbox + a second relay + `exchange-federation` trust wiring — built **only when a second organizationally-distinct relay actually needs to talk to the first**, and only after measured cross-domain reuse justifies it.

### Explicit YAGNI list
1. **Global open relay mesh, NIP-57 zaps, x402 bridges** — no counterparty exists outside the portfolio trust boundary yet. Strongest YAGNI candidate.
2. **Multi-relay replication / operator HA** — team-tier RF≈2 (relay + local fold) suffices; add after an actual outage.
3. **Blossom sharding/CDN** — 10–50 MB doesn't need it.
4. **Ed25519→secp256k1 attestation bridge** — only if live campfire reputation worth preserving actually survives (the exchange already lost identity once; *verify there is something to bridge* before building it).
5. **Full nostr-native federated trust-model** beyond existing `docs/convention/exchange-federation/` — wait for M3's trigger.
6. **Kind-number registry lock** — the adapter-passes-existing-tests gate validates the tag mapping long before external kind collisions matter.
7. **Convergence Oracle / price index / proof-of-savings feeds** — speculative external products; after team tier is proven live.

---

## Risks & Open Questions

### Surviving attacks by severity

**Permanent constraints (cannot be engineered to zero, only minimized):**
- **A4 — time-to-parity is bounded by incident-discovery latency, not LOC.** No amount of code compresses the multi-week tail of finding the next silent bug. *This is the whole case against rewrite* and the reason the chosen path preserves the 32K test spec rather than re-earning it.
- **A8 — half-migrated identity is a reputation-laundering window.** During any period where campfire and nostr senders coexist, an opt-in attestation done wrong once lets an attacker inherit an established seller's trust level. Mitigated to "rare, audited, opt-in, default-off" — *not* to zero. Depends on operator discipline this org has already needed two rounds to exercise (ce8, a0e).

**Fatal-if-unmitigated (must be actively guarded, mitigations specified above):**
- **A14 — sybil convergence-gaming at federation scale breaks the *ungameable* claim.** N free npubs publishing the same content across N relays degrades "3+ agents converge" into "3+ sybils converge." Mitigation: convergence weight requires identity-independence evidence — allowlist at team tier; **cost-per-identity (lightning-anchored) or NIP-05 attestation before convergence counts at global tier.** *Do not call public convergence a moat until an identity-independence model ships* (purist refusal #7). This is the single biggest open risk to the core thesis.
- **A2 — second-system scope creep.** The nostr-first framing tempts solving transport + trust-model + blob storage simultaneously on a blank slate. Walled off by the milestone contract: M1/M2 are transport; provenance and Blossom are *separately budgeted* workstreams, not smuggled into the adapter.
- **A6 — provenance/`LevelPresent` is the real sybil gate for `match`/`mint`/`burn`, not a port line-item.** Ship the transport swap without a replacement gate and these ops either silently drop trust (sybil-open) or block entirely (nothing settles). Broken out as its own workstream with a security-review multiplier.

**Mitigable (specified, standard engineering):**
- **A3/A9/A15 — ordering & ETag safety** — resolved by the operator-as-sole-sequencer design; HA deferred to enterprise with leader election.
- **A10 — relay-pruned antecedents** — orphan buffer + loud unrecoverable-failure path.
- **A12 — NIP-42 ≠ per-kind write authority** — relay ACL + universal client-side re-verify.
- **A7 — `views.go` deletion breaks 3 CLI commands** — rebuilt against `REQ` filters, costed.
- **A13 — team-tier single-relay durability** — add a second relay when an outage justifies it (not before).
- **A16 — zap-receipt decouples payment from delivery (no escrow on the lightning rail)** — port the existing `settle:preview` chunk pattern to the global/lightning path so preview precedes full payment.
- **A11 — individual→team live storage-model switch is undesigned** — the identical wire format at every tier (C8) makes this "add a relay URL," not a data migration; validated by M1 shipping the record shape M2 reuses.

### Questions needing a human ruling
1. **Sybil/identity-independence at global tier (blocks the moat claim):** is cost-per-identity (lightning-anchored stake) acceptable, or do we fall back to relay-of-record reputation weighting — reintroducing a trusted intermediary nostr was meant to avoid? *This must be answered before any "public convergence is a moat" positioning.* **→ RESOLVED by `docs/design/convergence-sybil-defense.md` (2026-07-06): global convergence is weighted/probabilistic, not a binary oracle; ship a layered stack (behavioral-decay backstop + correlation-discount keystone + burn floor + PAC-where-eligible); reject web-of-trust; scope the moat team/enterprise-binary vs global-weighted. RATIFIED 2026-07-06 (Baron): ship the layered stack + recommended parameter set.**
2. **Is there surviving campfire reputation worth bridging at all,** given the prior identity-loss event? Determines whether the attestation bridge (YAGNI #4) gets built.
3. **Per-kind write ACL vs. pure client-side verification** for operator-op enforcement at team tier — accept non-standard relay code (ops burden) or push all enforcement to readers (a buggy third-party reader that skips the check gets scammed)?
4. **Kind-number allocation** — coordinate with the broader nostr ecosystem/NIP process, or squat a custom range and accept future collision risk?

---

## Recommendation

**Evolve, don't rebuild.** ~21K LOC of economically/security-hardened value logic and its 32K-LOC regression spec sit untouched behind one clean seam; a clean-slate rewrite would burn 110–130 engineer-days re-earning correctness that has nothing to do with the discovery thesis — and then re-suffer each of ~7 named production incidents one live outage at a time. Do the migration in ~4–7 weeks (2–3 weeks with two engineers) by writing a nostr event adapter behind `proto.Message`, **genuinely rebuilding the fold ingest** as a Layer-0 local sequencer + orphan-antecedent buffer (the one piece nostr truly breaks — do not disguise it as a port), swapping the scrip store transport, adding a Blossom pointer path, and building a net-new provenance/trust primitive (orphaned by campfire under any path). Gate the whole thing on one mechanical acceptance test: **the existing 32,209-LOC suite passes unchanged.** Ship M1 (individual, local-only cache) in days, M2 (team, one authed relay) as the near-term product, and defer everything global/federated as YAGNI. Go nostr-first for exactly one thesis reason the recons underweight — **public, third-party-auditable cross-agent convergence** — and guard three invariants with your life: the deterministic fold, behavioral-not-payment completion signals, and convergence-independence. Break any of those and you have shipped infrastructure, not discovery.
