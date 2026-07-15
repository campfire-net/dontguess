# Design: Federation Infra Rewrite — P9 Decision Doc (Router-First, Nostr Relay Peering)

**Status:** RULED / decision-complete on paper. This is the **Gate C / P9** design item
(`docs/design/onboarding-tiered-scaling-federation.md` §9). It closes the items federation.md and
federation-modes.md left OPEN "pending P9" and makes the `dontguess federate` router-mode wire
protocol implementable field-by-field. **P10 (`dontguess federate`, router mode only) may proceed
from this document. Custodial mode remains a separate later item** — but its integrity gap
(ADV-9 / §10 Q2) is now RULED here (§4), so it is unblocked on paper too.
**Date:** 2026-07-15
**Author:** Convention Designer (Opus) — dontguess-f03
**Model tier:** Opus (federation trust + crypto), never Fable.
**Depends on / source of truth:**
`docs/design/onboarding-tiered-scaling-federation.md` §5/§9/§10 (the ruled ladder — authoritative),
`docs/design/content-confidentiality-envelope-541.md` (§541 — the confidentiality invariant that gates everything),
`docs/design/federation.md` + `docs/design/federation-modes.md` (the nostr-rewritten trust model this doc completes),
`docs/design/settle-wire-id-reconciliation-55c.md` (the wire-id/store-id alias this doc reuses cross-operator).
Where this doc and federation.md/federation-modes.md conflict, **this doc wins for the wire protocol**
(they declared it OPEN and deferred to P9); where this doc and §541 conflict, **§541 wins**.

---

## 0. What this document decides (and what it deliberately does not)

**Decided (decision-complete, P10 may build against these):**

1. The nostr **transport substrate** for federation: kinds, event shapes, signing, delivery relays (§1).
2. The **discovery beacon** wire format and eclipse-resistant discovery (§2).
3. The seven **`federation:*` operation event shapes**, field-by-field, replacing the retired campfire messages (§3).
4. The new put-time **`resell` field** on the §541 v2 put payload, its enforcement point in `applyPut`, and its default (§5).
5. **ROUTER mode** cross-operator match/deliver flow with the §541 plaintext set held at exactly `{A}` (§6).
6. **ADV-9 custodial content-integrity rebuild** — RULED per §10 Q2: **seller-signed salted plaintext commitment, revealed post-purchase** (Option A). Option B (operator-B provenance chain) rejected with reasons (§4).
7. **ADV-11 trust-signal ruling** formalized: the exact local-observation signals, and the exact self-report signals that are **structurally excluded** (§7).
8. **x402 pre-funded escrow** settlement for un-graduated operators — pre-fund → per-match debit → reconcile → graduate (§8).
9. **Revocation** semantics — bilateral instant-revoke, what survives it, in-flight match handling (§9).
10. **§541 reconciliation** — cross-operator wire-id/store-id aliasing reuses the 55c `RegisterWireAlias` seam; the dedup fold stays home-operator-local (§6.4).

**NOT decided here (stays DEFERRED / needs a human ruling — §11):**

- **Mode 5 open/global liquidity** — DEFERRED per `onboarding-tiered-scaling-federation.md` §10 Q3.
  This doc rules bilateral-only + x402-stake and does **not** design open-network sybil economics.
- **Custodial mode *implementation*** ships as a later item after router (P10) lands. Its integrity
  mechanism is ruled here; its E2E re-wrap proof is a separate gate.
- Exact numeric x402 escrow floor / exposure cap / graduation threshold values (§8 leaves the
  mechanism ruled and the constants as an operator-tunable config with recommended defaults).

---

## 1. Transport substrate — RULING: one shared `federation` kind + an addressable discovery beacon

Federation runs over **nostr, no shared campfire, no shared transport identity** (F6, federation.md).
Each operator signs `federation:*` events with **its own secp256k1 operator key** (the same one that
signs put/match/settle and holds the §541 CEKs) and publishes them to **the peer's relay** (bilateral
peering: A writes to B's relay and B writes to A's relay, each on a roster slot the other operator
admitted — see §3.1). There is no third relay and no shared inbox.

### 1.1 Kind assignments (extends `pkg/nostr/kinds.go`)

Mirroring the existing shared-kind pattern (assign = one kind 3405 + 7 `["op",…]` sub-ops; scrip =
one kind 3411 + N sub-ops), the seven bilateral federation operations share **one regular kind**
discriminated by an `["op", "federation:<sub-op>"]` tag. Discovery is an **addressable**
(parameterized-replaceable, latest-wins) event like the roster (30078) and inventory projection (30401).

```
KindFederation = 3420  // regular, immutable — 7 bilateral ops via ["op","federation:<sub-op>"]
                       //   federation:propose | federation:accept | federation:inventory-offer
                       //   federation:match-request | federation:match-confirm
                       //   federation:revoke | federation:reconcile
KindExchangeBeacon = 30402  // addressable — signed "dontguess:exchange" discovery beacon,
                            //   d-tag = operator x-only pubkey hex, latest-wins
```

- `KindFederation` is added to `nostr.DontguessKinds` (kinds.go:57) so Intake backfills it under the
  bounded-cursor 61a fix. `KindExchangeBeacon` is **excluded** from `DontguessKinds` (like 30401 — it is
  a projection an operator publishes, discovered by explicit REQ during `federate`, never backfilled).
- **Why a shared kind, not seven dedicated kinds:** identical rationale to assign/scrip — the seven
  ops are one bilateral state machine, the sub-op is a discriminator not a routing key, and a shared
  kind keeps the Intake filter list and the adapter's `baseOpToKind`/`assignOps`-style tables small.
- **Reserved range note:** 3407-3409 and 3412-3419 stay reserved; 3420 leaves headroom below it for
  more base-exchange kinds. Collision-check the live NIP registry before locking (kinds.go package-doc rule).

### 1.2 Signing & delivery

Every `federation:*` event and every beacon is a standard nostr event, Schnorr-signed by the
operator key. **Unforgeable by construction** — the operator's nostr pubkey *is* its federation
identity (federation.md A6). No new key, no NIP-42 change beyond admitting the peer's single operator
pubkey to a scoped roster slot on your relay (§3.1).

---

## 2. Discovery beacon — RULING: signed addressable event + multi-directory + pinned first peers

**`KindExchangeBeacon` (30402), d-tag = operator pubkey, latest-wins.** Content (JSON):

```json
{
  "v": 1,
  "operator_npub": "<x-only pubkey hex>",
  "relays": ["wss://relay-a.example:7777", "wss://relay-a2.example:7777"],
  "display_name": "baron.dontguess",
  "domains": ["matching","exchange","pricing"],       // advisory topical hint, tainted
  "x402_receiver": "<usdc receiving address for un-graduated escrow, §8>",
  "min_stake_usdc": "5.00",                             // advertised escrow floor to open a bilateral (§8)
  "modes_offered": ["router"]                           // "custodial" appears ONLY after custodial ships
}
```

**Beacons are tainted — discovery is not trust** (federation.md §3.3). A beacon proves only "some key
signed this address," never that the operator is honest. Eclipse defense (ADV-14), RULED:

- **Multiple independent directories.** A directory is any relay that carries 30402 events. An
  operator publishes its beacon to several; a discovering operator queries several and requires the
  same `(operator_npub → relays)` mapping from ≥2 independent directories before treating an address
  as a candidate.
- **Operator-pinned known-good first peers.** `federate` accepts a beacon only if it is (a) pinned in
  local config (`known_peers`, hand-verified out-of-band — website/DNS/personal contact, federation.md A6),
  or (b) reachable transitively **for discovery only** from a pinned peer's published peer list —
  **discovery transitivity never implies trust transitivity** (F1: A↔B and B↔C gives A *visibility of C's
  beacon*, never inventory access to C).

**Out-of-band key verification is mandatory before the first `federation:accept`** — the beacon
address is confirmed against a channel the attacker does not control. This is a human/operator step,
surfaced by `federate` as a confirmation prompt, never auto-accepted (ADV-19).

---

## 3. The seven `federation:*` event shapes (field-by-field)

All are `KindFederation` (3420) with `["op","federation:<sub-op>"]`. `["p", <peer-operator-pubkey>]`
addresses the counterparty; `["e", <antecedent-event-id>, "", "reply"]` chains the bilateral DAG
exactly as exchange messages chain today (adapter.go tagE/tagP). `["dg_ts", …]` preserves the exact
timestamp per the adapter's preservation-tag convention. All examples show the JSON `content` payload.

### 3.0 The bilateral state machine (overview)

```
A: propose ──▶ B: accept ──▶ (agreement live)
                      │
   B: inventory-offer ◀── (each operator streams its offered, resell-eligible inventory metadata)
                      │
A-buyer match ─▶ A: match-request ──▶ B: match-confirm ──▶ A delivers (router) ──▶ A: reconcile (escrow debit)
                      │
   either: revoke ──▶ (agreement dead; §9)
```

### 3.1 `federation:propose`

Operator A offers a bilateral, scoped, revocable agreement to B.

```json
{
  "v": 1,
  "from_operator": "<A pubkey>",
  "to_operator": "<B pubkey>",
  "scope": {
    "modes": ["router"],                    // "custodial" only when both operators run a custodial-capable build
    "domains": ["matching","exchange"],     // A will offer/consume inventory ONLY in these domains
    "direction": "bidirectional",           // or "a-offers" / "a-consumes" — reciprocal-ratio terms live here
    "max_in_flight_matches": 64,            // A's cap on concurrent un-reconciled cross-operator matches
    "settlement": {
      "rail": "x402-escrow",                // un-graduated default (§8); "bilateral-credit" only post-graduation
      "escrow_floor_usdc": "5.00",
      "reconcile_cadence": "1h"
    }
  },
  "a_relays": ["wss://relay-a.example:7777"],   // where B should write federation events + inventory REQs
  "proposed_at": 1731700000
}
```

- **A must have already admitted B's operator pubkey to a scoped roster slot on A's relay** (§2 roster
  event, `d`-tag=`fed:<B-pubkey>` rather than `fleet`) so B can write `federation:*` events (and ONLY
  those kinds) to A's relay. This roster slot is **separate from the fleet roster** — a federated peer
  is not a fleet member; it can write federation kinds, never `put`/`buy`/`settle` into A's exchange.
  The relay writePolicy gate (onboarding §2) enforces "B may write kind 3420 to A's relay," the
  exchange fold enforces "and only as a recognized federation op from a proposed/accepted peer."

### 3.2 `federation:accept`

B agrees (or a `federation:reject` sub-op — modeled as `accept` with `"accepted": false` +reason, to
keep the op set at seven). Acceptance is the **deliberate trust decision** (ADV-19): B's operator must
confirm out-of-band key verification and stake (§8) before this event is emitted.

```json
{
  "v": 1,
  "from_operator": "<B pubkey>",
  "to_operator": "<A pubkey>",
  "accepted": true,
  "agreement_id": "<sha256(sorted(A||B) || proposed_at)>",   // deterministic bilateral id, both sides compute identically
  "accepted_scope": { "...": "B's counter-scope; the effective scope is the INTERSECTION of propose∩accept" },
  "b_relays": ["wss://relay-b.example:7777"],
  "b_x402_receiver": "<usdc addr>",
  "escrow_funding_ref": "<x402 pre-fund receipt id, §8>",     // REQUIRED if either side is un-graduated
  "accepted_at": 1731700100
}
```

- **Effective scope = `propose.scope ∩ accept.accepted_scope`** — neither side can widen the other's
  terms. Both operators persist the effective scope keyed by `agreement_id`.
- If either operator is un-graduated (§8), `escrow_funding_ref` MUST reference a confirmed pre-fund;
  an accept without it is dropped by the counterparty's fold (fail-closed, no free credit — ADV-12).

### 3.3 `federation:inventory-offer`

Each operator streams the **metadata** of its resell-eligible inventory (entries whose seller set
`resell: federation` or `resell: custodial`, §5) to the peer. **Never the CEK, never plaintext, never
ciphertext** — router-mode inventory sharing is metadata-only.

```json
{
  "v": 1,
  "agreement_id": "<...>",
  "from_operator": "<offering operator pubkey>",
  "entries": [
    {
      "entry_id": "<home-operator store entry id>",
      "description": "terse matching key (already public, §541 §3.3)",
      "teaser": "seller-authored public abstract (already public, §541 §4.1)",
      "token_cost": 1234,
      "content_type": "code",
      "domains": ["matching","exchange"],
      "embedding": [/* 384-dim all-MiniLM-L6-v2 vector — already derivable from the public description */],
      "resell_mode": "router",                 // router | custodial — the seller's per-entry consent (§5)
      "ciphertext_hash": "sha256:<...>",        // §541 integrity value; router mode delivers home ciphertext so this round-trips
      "seller_commitment": "sha256:<...>",      // salted plaintext commitment for custodial integrity (§4); present iff resell_mode allows custodial
      "seller_commitment_sig": "<schnorr sig by the ORIGINAL SELLER over (entry_id||seller_commitment)>",
      "offered_price_scrip": 1500              // home operator's current dynamic price; the peer re-prices for its own market
    }
  ],
  "cursor": "<opaque incremental cursor so offers stream, not re-send the whole corpus each cycle>",
  "offered_at": 1731700200
}
```

- The receiving operator indexes these into a **separate `federatedInventory` overlay**, NEVER the
  home `inventory` map — cross-operator entries are matchable but are not owned, not re-priced into the
  home ledger, and carry the `agreement_id` provenance so revocation (§9) can purge them atomically.
- Everything in an inventory-offer is **already-public §541 metadata** — sharing it widens no
  confidentiality boundary. This is the whole point of router mode.

### 3.4 `federation:match-request`

A buyer on A's exchange semantically matched a B-origin entry. A forwards the buyer's **funded**
reservation to B.

```json
{
  "v": 1,
  "agreement_id": "<...>",
  "from_operator": "<A pubkey>",
  "entry_id": "<B-origin entry id>",
  "buyer_pubkey": "<A's buyer x-only pubkey — DERIVED FROM A's antecedent chain, never payload-supplied>",
  "a_match_wire_id": "<A's match event wire id (post-Outbox re-sign, per 55c)>",
  "reservation_proof": {
    "escrow_debit_ref": "<x402 escrow line A is committing for this match, §8>",   // un-graduated
    "or_bilateral_credit_ref": "<post-graduation credit line id>"                   // graduated
  },
  "requested_at": 1731700300
}
```

- A only emits this **after** A's own buyer holds a live scrip reservation on A's ledger (the
  local deliver gate, engine_settle.go:994-1000, is unchanged — scrip stays local, F2). The
  cross-operator settlement is a **separate** x402/credit line (§8), not a scrip transfer.

### 3.5 `federation:match-confirm`

B authorizes the match. **In router mode B returns nothing decryptable** — it confirms the entry is
still available and its integrity values, then A (the home operator) does the actual CEK re-wrap and
deliver on its own exchange.

```json
{
  "v": 1,
  "agreement_id": "<...>",
  "from_operator": "<B pubkey>",
  "a_match_wire_id": "<echoes A's match id, §3.4>",
  "entry_id": "<...>",
  "confirmed": true,
  "ciphertext_hash": "sha256:<same value B published at inventory-offer>",   // A/A's buyer verify against this
  "seller_commitment": "sha256:<...>", "seller_commitment_sig": "<...>",     // forwarded unchanged from the seller (§4)
  "price_scrip": 1500,
  "confirmed_at": 1731700400
}
```

- **Router mode:** B is a discovery/matching **router only**. The delivery pivot is **A, the home
  operator** — wait: the home operator of a B-origin entry is **B**. Re-read: in router mode the
  entry's home operator (the one holding the CEK) **delivers directly to the buyer**. See §6.1 for the
  exact CEK-custody resolution — the "home operator delivers" rule means **the entry's origin operator
  B re-wraps its own CEK to A's buyer**, and B never hands the CEK to A. This is the §541-correct
  router flow and §6.1 nails the field-level mechanics.

### 3.6 `federation:revoke`

Either operator kills the agreement. Instant, unilateral, unconditional (F1).

```json
{
  "v": 1,
  "agreement_id": "<...>",
  "from_operator": "<revoker pubkey>",
  "reason": "manual | trust-floor | reconcile-overdue | scope-violation",
  "in_flight_policy": "settle | refund",     // how to treat un-reconciled matches (§9)
  "revoked_at": 1731700500
}
```

### 3.7 `federation:reconcile`

Periodic (per `reconcile_cadence`) bilateral settlement of cross-operator matches. Reuses the local
settle vocabulary but over the x402/credit line, not scrip.

```json
{
  "v": 1,
  "agreement_id": "<...>",
  "from_operator": "<A pubkey>",
  "period": { "from": 1731696900, "to": 1731700500 },
  "matches": [
    { "a_match_wire_id": "<...>", "entry_id": "<...>", "amount_usdc": "0.012", "outcome": "delivered" }
  ],
  "escrow_settlement_ref": "<x402 settlement tx, §8>",
  "running_reputation_note": "advisory — receiver recomputes trust from ITS OWN observations (§7), never trusts this",
  "reconciled_at": 1731700500
}
```

- **Overdue reconcile → trust penalty (-10, federation.md §4) → possible auto-revoke.** Because
  un-graduated settlement is **pre-funded escrow**, an overdue reconcile never means A got content free
  — the escrow already debited (§8). Reconcile is a bookkeeping true-up, not a payment on credit.

---

## 4. ADV-9 custodial content-integrity — RULING (closes §10 Q2)

**Decision: Option A — seller-signed *salted* plaintext commitment, revealed only post-purchase.**
Option B (operator-B-signed provenance chain) is **rejected**.

### 4.1 The mechanism

At put time (§541 §3.1), in addition to `ciphertext = AEAD(CEK, plaintext)`, the seller:

1. Prepends a fresh 32-byte random salt to the plaintext **inside** the AEAD boundary:
   `plaintext' = salt || plaintext`, and it is `plaintext'` that is AEAD-encrypted. (So the salt is
   revealed **only** to a party who already holds the CEK — i.e., only post-purchase.)
2. Computes `seller_commitment = sha256(plaintext')` and **Schnorr-signs** `(entry_id || seller_commitment)`
   with the **seller's own key** → `seller_commitment_sig`.
3. Publishes `seller_commitment` + `seller_commitment_sig` on the public put payload (new optional
   fields, §5.2). **These are safe on the public wire because the commitment is salted** — an attacker
   who guesses the plaintext cannot confirm it without the salt, and identical plaintext under
   different salts yields different commitments, so it is **not** the §541 §4.4 guess-confirmation /
   correlation oracle that the *unsalted* `plaintext_content_hash` was (which is why §541 removed
   *that*; this is a different, salted object and is admissible).

**Verification (any buyer, router or custodial):** post-purchase the buyer holds the CEK, decrypts to
recover `plaintext'`, recomputes `sha256(plaintext')`, checks it equals `seller_commitment`, and
verifies `seller_commitment_sig` against the **original seller's** pubkey (carried on the entry). A
mismatch ⇒ dispute, do not `settle(complete)` — identical to the §541 §3.4 deliver-verify contract.

### 4.2 Why this survives custodial re-encryption (the ADV-9 problem)

The ADV-9 break: in custodial mode B re-encrypts with **B's own CEK**, so `ciphertext_hash(B) ≠
ciphertext_hash(A)` and A's buyer cannot verify against A's offered `ciphertext_hash`. The
seller-commitment is computed over **`plaintext'`, not ciphertext** — it is invariant under
re-encryption by *any* operator. B (or any custodial re-encryptor) that swaps the content would have
to forge the **original seller's** signature over a new commitment, which it cannot. So integrity is
anchored to the **seller's key**, end to end, across arbitrarily many custodial re-encryption hops.

### 4.3 Why Option B is rejected

An operator-B-signed provenance chain ("B attests it faithfully relayed A's ciphertext, hash X") only
proves B relayed *something* B claims came from A — it **trusts B**, the exact party the custodial
threat model says might be malicious (federation.md §8.9: a custodial peer can read and could tamper).
It also requires a fresh operator signature per hop and does not bind to the seller. Option A binds
integrity to the seller (who is not a party to the B-tampering threat) with **zero per-hop operator
trust** and one signature made once at put. Option A strictly dominates.

### 4.4 Scope

- **Router mode does not need this** — it delivers home ciphertext, so `ciphertext_hash` round-trips
  (federation.md §3.2). The seller-commitment is still emitted at put (cheap, one salt + one sig) so
  that an entry can be flipped to custodial later without a re-put; router-mode buyers may verify it as
  a defense-in-depth check but are not required to.
- **Custodial mode cannot ship until this lands** — but "this" is now a **ruled, small** addition:
  two optional put-payload fields + a salt-prepend + a post-purchase verify. It is no longer an open
  research question. Custodial *implementation* remains a separate later item after router P10.

---

## 5. The `resell` put-time field — RULING (schema + enforcement)

### 5.1 Field placement and values

`resell` is a **policy field**, not a crypto field, so it sits at the **top level** of the §541 v2 put
payload (a sibling of `description`/`teaser`/`token_cost`, NOT inside the `enc` object — `enc` is the
cryptographic envelope and stays purely about key-wrap/ciphertext):

```json
{
  "v": 2,
  "description": "...", "teaser": "...", "token_cost": 1234, "content_type": "code", "domains": ["..."],
  "resell": "none",                 // one of: "none" | "federation" | "custodial" | "<comma-separated npubs>"
  "enc": { "...": "§541 §3.3 unchanged" },
  "seller_commitment": "sha256:<...>",       // §4 — REQUIRED when resell permits custodial, else optional
  "seller_commitment_sig": "<schnorr>"       // §4
}
```

Values:

| `resell` value | Meaning | Plaintext trust set |
|---|---|---|
| `none` (**DEFAULT**) | Home-only. Never offered in any `federation:inventory-offer`. | `{home operator}` |
| `federation` | Router-mode eligible. Metadata may be offered to accepted peers; **CEK never leaves home**. | `{home operator}` (unchanged) |
| `custodial` | Router **and** custodial eligible. Home operator MAY re-wrap the CEK to a peer (§6.2). | `{home, custodial peer}` per accepted agreement |
| `<npubs>` | Explicit allow-list: custodial eligible **only** to the named operator pubkey(s). | `{home, named peers}` |

- **Default is `none`** — the safe default (federation.md §3.1). An entry is federated only by a
  deliberate seller choice. Silence = home-only.
- `custodial` and `<npubs>` are the **informed-consent surface** (§541 §8.9): choosing them grants
  plaintext read to the peer. The CLI MUST surface the §8.9 consent block before accepting a
  `--resell custodial` / `--resell <npub>` put.

### 5.2 Enforcement at `applyPut` (fail-closed)

Mirroring the existing §541 fail-closed fold (`isLegacyPlaintextPut` / `encWellFormed`,
state_put.go:611-642), `applyPut` gains a `resell` validation that runs **inside the operator decrypt
boundary** (§541 §3.6 — only the home operator folds authoritatively):

1. **Unknown/malformed `resell` ⇒ treat as `none`** (fail-safe to home-only, never fail-open to
   federated). An absent field ⇒ `none`.
2. **`custodial` or `<npubs>` without a well-formed `seller_commitment`+`seller_commitment_sig` whose
   signature verifies against the put's sender ⇒ downgrade to `federation`** (router-only) and record
   a fold note. Rationale: custodial integrity (§4) is only sound with the seller commitment; a
   custodial opt-in lacking it is silently narrowed to router, never rejected outright (the entry is
   still sellable, just not custodial-eligible). This is the one place the fold *narrows* rather than
   drops — narrowing preserves availability while refusing to ship un-verifiable custodial.
3. The resolved `resell` mode is **persisted on the `InventoryEntry`** (new `ResellMode` field,
   Replay-safe, folded from the put event exactly like `WrappedCEKOperator`, §541 §3.5) and gates
   whether the entry is ever emitted in a `federation:inventory-offer`.
4. **`resell` is immutable per put** (entries are immutable, §541). Changing resale posture = a new put.

**Wire/schema impact:** this is the one shared-interface addition P10 introduces — the put payload
schema v2 gains an optional top-level `resell` string plus the two `seller_commitment*` fields, and
`InventoryEntry` gains `ResellMode`. It is **backward-compatible**: absent ⇒ `none` ⇒ identical to
today's non-federated behavior. Not implemented in this doc (design-only).

---

## 6. ROUTER mode cross-operator match/deliver — the §541-correct flow

### 6.1 CEK custody: the entry's ORIGIN operator always delivers (router)

The entry originates on B (B holds the CEK; B's seller wrapped `wrapped_cek_operator` to B). A buyer
on A matches it. **Router rule: B (the origin/home operator of the entry) re-wraps its own CEK
directly to A's buyer and B emits the deliver.** A never receives the CEK; A is the discovery/routing
front-end for its own buyer, B is the delivery pivot for its own content.

Concretely, extending §541 §3.4 across the operator boundary:

1. A's buyer matches B-origin `entry_id` (metadata was in B's `inventory-offer`, §3.3).
2. A emits `federation:match-request` carrying **A's buyer pubkey** (derived from A's antecedent
   chain, never payload-supplied — §541 §3.4 anti-replay binding) + a funded reservation proof (§8).
3. B verifies the reservation proof (escrow line committed), then B computes
   `wrapped_cek_buyer = NIP-44(B_priv, A_buyerPub, CEK)` and emits a **standard §541 deliver (kind
   3404, phase=deliver)** on **B's exchange**, addressed (`recipient`) to A's buyer pubkey, referencing
   B's already-public ciphertext + `ciphertext_hash`. B also emits `federation:match-confirm` (§3.5) to
   A closing the bilateral DAG.
4. A's buyer (a nostr client) reads the deliver from B's relay (A's buyer is transiently admitted to
   read, or the deliver is mirrored to A's relay — see §6.3), unwraps the CEK with **A_buyer_priv
   against B_operatorPub**, fetches B's ciphertext, verifies `ciphertext_hash`, AEAD-decrypts, and
   optionally verifies the §4 seller-commitment.

**§541 plaintext trust set stays exactly `{B}`** — the origin operator. A never sees plaintext; A
routed a match and moved money (§8). The router peer (A) "never sees plaintext, only metadata +
ciphertext hashes" (federation-modes.md §FEDERATION) is **literally true**: A only ever handled
metadata + the funded reservation. **Router mode widens the plaintext set by exactly zero** — the
constraint the item mandates ("router mode must NOT widen the plaintext set beyond {A}", read as "beyond
the single origin operator").

> Nomenclature note: federation.md §3.1 writes the flow as "home operator A re-wraps." That is the
> same rule with labels swapped (there, A = the entry's origin/home). This doc uses A = the
> **buyer-side** operator to make the money-movement direction (A's buyer pays) unambiguous; the
> invariant is identical: **the operator that holds the CEK (the entry's origin) is the only party
> that ever re-wraps it, and it re-wraps directly to the buyer.**

### 6.2 CUSTODIAL mode (opt-in, entry `resell: custodial`/`<npubs>`) — flow delta

Only when the entry's seller opted in (§5) AND the agreement scope includes `custodial`: the origin
operator B **re-wraps the CEK to A's operator key** (`wrapped_cek_A = NIP-44(B_priv, A_operatorPub, CEK)`)
so A can be the delivery pivot when B is offline/re-pricing. A then delivers to A's buyer exactly like
a home entry. Confidentiality is now `{A, B}` — least-trusted of the two, exactly §541. Integrity is
the §4 seller-commitment (A may re-encrypt under A's CEK; the commitment still verifies). Custodial
ships **after** router (P10), as a separate item; this doc rules its mechanics so that item is
unblocked on paper.

### 6.3 Cross-relay deliver visibility

A's buyer must read B's deliver event. RULING: B emits the deliver to **B's relay** (its native home)
**and** A mirrors it to A's relay via the bilateral peering slot (A admitted B's operator key for
`KindSettle` deliver-phase events scoped to `agreement_id` matches — a narrow, match-scoped
extension of the §3.1 federation roster slot). A's buyer reads from A's relay as usual. The deliver is
already confidentiality-safe (wrap + ciphertext ref only, §541 §3.4), so mirroring it leaks nothing.

### 6.4 §541 reconciliation — cross-operator wire-id/store-id aliasing

The 55c wire-id/store-id divergence (an operator's random store id vs the Outbox-re-signed content-hash
wire id) recurs across the operator boundary: A references B-origin entries and matches by **B's wire
ids** (the only ids A ever sees on B's relay). RULING: **reuse the 55c `RegisterWireAlias(wire, store)`
seam** — B's `federation:inventory-offer` carries `entry_id` = B's **wire** id (deterministic,
re-derivable), and B maintains the wire→store alias locally exactly as it does for its own settle
chain (settle-wire-id-reconciliation-55c.md GAP-1). A never needs B's store ids. No new reconciliation
primitive — the cross-operator case is the intra-operator case with the relay boundary already crossed,
which is precisely what 55c solved. The dedup/quality fold stays **home-operator-local** (§541 §3.6):
B folds and gates its own inventory; A never re-folds B's entries (they live in A's `federatedInventory`
overlay, §3.3, not A's authoritative state).

---

## 7. ADV-11 trust-signal ruling — formalized

Each operator maintains a per-peer cross-operator trust score (federation.md §4) derived **only from
its own observations of its own buyers' local outcomes.** This section pins exactly which signals are
admissible and which are **structurally excluded**.

**Admissible (the receiving operator observes these on ITS OWN ledger/buyers):**

| Signal | Local source | Δtrust |
|---|---|---|
| Cross-operator match completed, buyer did not dispute | A's own settle chain | +2 |
| Buyer disputed and dispute resolved in buyer's favor | A's own dispute fold | −5 |
| Buyer rejected after preview (post-teaser) | A's own settle(buyer-reject) | −1 |
| Reconcile arrived on time and matched A's own tally | A's own reconcile fold | +5 |
| Reconcile overdue past `reconcile_cadence` | A's own clock | −10 |

Start 50/100; soft-suspend (exclude B's inventory from A's match results) < 40; auto-revoke < 20
(federation.md §4). All Δ are computed from events **A folded on A's own exchange** — never from a
number B sent.

**Structurally excluded (never weighted, even as a tiebreak):**

- **B's self-reported reputation / trust score.** A sybil owns its ledger and can mint any score.
- **B's self-reported convergence** ("3 of my buyers succeeded with this entry"). A sybil owns its
  buyers and manufactures convergence via `mint` (§541 threat model). Convergence is only trustworthy
  when the converging buyers are **A's own** and observed on A's ledger.
- **B's claimed match-completion / dispute rates** in `federation:reconcile.running_reputation_note` —
  carried for human audit only, explicitly `advisory`, folded into **nothing**.

**Why this is sound:** the trust score is a function purely of `A`'s local event log. A malicious B
cannot move its own score on A except by actually delivering good outcomes to A's real buyers (which is
indistinguishable from honesty) or by paying real x402 (§8). Self-report has **zero** weight, so
sybil trust-inflation has no attack surface.

---

## 8. x402 pre-funded escrow settlement — RULING

Un-graduated / untrusted operators settle cross-operator matches via **pre-funded x402 escrow, never
trailing bilateral credit** (F4 / ADV-12). Trailing credit lets a defaulting operator rotate to a
fresh identity before reconciling and get content free; pre-funding makes default impossible.

**Mechanism (ruled; constants operator-tunable with recommended defaults):**

1. **Pre-fund at accept.** Before `federation:accept` completes, the un-graduated operator deposits
   USDC to the peer's `x402_receiver` (beacon §2) up to at least `escrow_floor_usdc` (recommended
   default: `$5`, or `max_in_flight_matches × typical_match_price`, whichever is larger). The
   `escrow_funding_ref` on the accept (§3.2) references the confirmed deposit. No pre-fund ⇒ accept
   dropped fail-closed.
2. **Per-match debit at match-request.** Each `federation:match-request` (§3.4) carries an
   `escrow_debit_ref` committing a line against the pre-funded balance. If the remaining balance <
   match price, the match is refused (`match-confirm.confirmed=false`, reason `escrow-exhausted`) —
   the buyer on A is refunded its local scrip hold; A must top up escrow to continue. **Content is
   never delivered against an uncommitted line.**
3. **Reconcile as true-up.** `federation:reconcile` (§3.7) settles the debited lines against actual
   outcomes (delivered / refunded) and the x402 settlement tx moves the net. Because the money was
   pre-committed, reconcile is bookkeeping, not a payment-on-credit — an overdue or absent reconcile
   costs the debtor nothing it hasn't already funded.
4. **Graduation → bilateral credit unlocks, bounded.** After a configurable clean history (recommended:
   trust ≥ 80 sustained over ≥ N reconcile cycles with zero buyer-favor disputes), the pair MAY switch
   `settlement.rail` to `bilateral-credit` with a **bounded exposure cap** (recommended: one
   `reconcile_cadence` window of typical volume). Exceeding the cap forces a reconcile before more
   matches. Graduation is per-agreement, revocable (a single default drops the pair back to escrow).

**Scrip stays strictly local (F2)** — x402 (real USDC) is the *only* cross-operator value rail for
un-graduated operators. No cross-operator scrip mint, ever. The buyer on A pays A in scrip (local); A
settles with B in x402/credit (cross-operator). The two ledgers never merge.

---

## 9. Revocation — RULING

`federation:revoke` (§3.6) is **instant, unilateral, unconditional** (F1). On revoke, both operators:

1. **Purge the peer's `federatedInventory` overlay** for that `agreement_id` atomically (the overlay
   is keyed by `agreement_id` precisely so this is a single-key delete, §3.3). B's entries vanish from
   A's match results immediately.
2. **Stop admitting** the peer's operator key on the scoped federation roster slot (§3.1) — the peer
   can no longer write `federation:*` to your relay. (This is a live roster republish, onboarding §3.)
3. **Handle in-flight matches** per `revoke.in_flight_policy`:
   - `settle` (default): matches already `match-confirm`ed but not yet reconciled are honored — the
     content was already delivered (escrow already debited, §8), so settling the reconcile is correct
     and costs nothing extra. This avoids punishing a buyer mid-transaction for an operator-level revoke.
   - `refund`: matches not yet delivered are cancelled; A refunds its buyer's local scrip hold and
     releases the escrow line. Used when revoking *because* the peer turned malicious (`reason:
     scope-violation` / `trust-floor`).
4. **No transitive effect** (F1): revoking A↔B does not touch A↔C or B↔C.

Revocation is **not** cryptographic content revocation — content already delivered to a buyer stays
readable (§541: no revocation once the CEK is out). Revoke stops *future* liquidity, not past sales.

---

## 10. What P10 must build (router mode only) + follow-on items

**P10 scope (router, from this doc):**
- `pkg/nostr/kinds.go`: add `KindFederation=3420`, `KindExchangeBeacon=30402`; add `KindFederation` to
  `DontguessKinds`; a `federationOps` sub-op set mirroring `assignOps`.
- Put payload + `applyPut`: the `resell` field (§5) + `seller_commitment`/`_sig` (§4) + `InventoryEntry.ResellMode`.
- `federation:*` fold handlers (the seven ops, §3) + the `federatedInventory` overlay + cross-operator
  `RegisterWireAlias` reuse (§6.4).
- Router deliver path: origin operator re-wraps CEK to the remote buyer + cross-relay deliver mirror (§6.1/§6.3).
- Trust overlay (§7) + x402 escrow client (§8) + `federate`/`revoke` CLI verbs (deliberate, §2/§9).
- Discovery: multi-directory beacon publish/query + `known_peers` pin + out-of-band confirm prompt (§2).

**Follow-on items to create (rd):**
1. **P10 — `dontguess federate` (router mode)** — implement the above. Blocked on nothing now.
2. **Custodial federation** — §4 commitment + §6.2 re-wrap-to-peer + the "peer never receives CEK in
   router / does in custodial" E2E confidentiality split. Gated on P10 landing first.
3. **x402 escrow integration** — the actual USDC rail wiring (§8); may be its own item if x402 client
   is not yet in-tree.
4. **strfry writePolicy: scoped federation roster slot** (`d`-tag=`fed:<peer>`, §3.1) — out-of-repo
   relay policy change, mirrors the fleet-roster writePolicy work (onboarding §2/§9 HUMAN GATE ef1).
5. **Ground-source E2E (router):** two nostr-attached exchanges → propose/accept → cross-operator
   match → **confidentiality-property assertion** (a passive scrape of the shared relay channel yields
   only metadata + ciphertext; the router peer's process never holds the CEK). Required before router
   federation goes on the website (federation-modes.md E2E rule).

---

## 11. Open questions needing a human ruling

1. **Mode 5 open/global liquidity** (`onboarding-tiered-scaling-federation.md` §10 Q3) — this doc rules
   **bilateral-only + x402-stake** and leaves Mode 5 **DEFERRED**. Confirm it stays deferred for v1, or
   open a separate design item for open-network sybil economics. **Recommendation: keep DEFERRED** — it
   is in direct tension with the ruled agent-level-WoT rejection (federation.md §3.3) and needs its own
   adversarial pass. *No P10 dependency either way.*
2. **x402 escrow constants** (§8) — recommended defaults given (floor `$5`, cap = one reconcile window,
   graduation at trust ≥ 80 over N cycles). Confirm or set org-specific values. *Mechanism is ruled;
   only the numbers are open — not a P10 blocker (config-tunable).*
3. **Federation roster slot on strfry** (§3.1) — confirm the `d`-tag=`fed:<peer>` scoped writePolicy
   slot is the mechanism (vs a second relay per peer). Ties to HUMAN GATE ef1 (roster-aware writePolicy
   deploy). *P10 relay-side dependency.*

---

<!-- P9 / Gate C decision doc for dontguess-016; closes federation.md/federation-modes.md OPEN items
     and onboarding-tiered-scaling-federation.md §10 Q2 (ADV-9 → Option A). Blocks P10. -->
<!-- source item: dontguess-f03 -->
