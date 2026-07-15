# Content Confidentiality — Envelope Encryption (dontguess-541)

**Status:** Design — approved direction, conditional on the three CRITICAL construction gaps below landing.
**Scope:** Team tier (relay-backed). Individual tier (local socket, `ScripStore == nil`) is already confidential and stays byte-for-byte unchanged.
**Model tier for crypto implementation:** Opus (security), never Fable.

---

## 1. Problem & goal

The nostr exchange broadcasts full sellable content in **cleartext** (base64) in the put event (kind 3401) `Content` field. `buildPutMessage` (`pkg/relayclient/relayclient.go:341-347`) base64-encodes raw content unconditionally; the adapter copies the payload verbatim into the public event (`pkg/nostr/adapter.go:124`); `applyPut` stores decoded plaintext as `entry.Content` (`pkg/exchange/state_put.go:604-720`). Anyone can passively scrape all content for free via one unauthenticated NIP-01 `REQ Kinds:[3401]` — no scrip, no allowlist. Two further plaintext exfiltration paths compound this: the Blossom offload stores **raw plaintext** blobs behind a public pointer (`state_put.go:688`), and the preview path broadcasts **15-25% of real content** in clear inside a public settle event (`engine_settle.go:836-905`). The market gates (scrip-reservation deliver gate `engine_settle.go:994-1000`; trust tiers `trust.go`) protect **write-admission and emission-triggering, not read-confidentiality**.

**Confidentiality property (stated precisely):** *After this change, the complete set of events and blobs a passive adversary can obtain — every 3401 put, every 3404 settle, every Blossom blob, replicated across every mirror relay — reveals of the sellable content only: (a) its `description`, (b) a seller-authored `teaser` bounded in size, (c) `token_cost`/`content_type`/`domains`, and (d) ciphertext plus a hash of that ciphertext. Recovering the plaintext requires the AEAD content-encryption key (CEK), which is transmitted only wrapped to the operator (at put) and wrapped to a buyer who holds a live scrip reservation for a settled match (at deliver). Confidentiality rests on possession of secp256k1 private keys, not on relay read-ACLs.* This is confidentiality **by construction** (§6 makes it fail-closed), not by client discipline.

**Out of scope of the property (permanent constraints, §5):** post-purchase copying by a paying buyer (economic defense only); the operator sees plaintext (publisher model); historical plaintext already broadcast (irreversibly public on an append-only medium).

---

## 2. Threat model

Single operator, always online. Sellers put once and may go offline forever. Buyers are unknown at put time. The relay(s) are untrusted for confidentiality (they enforce NIP-42 write-admission, nothing about payload secrecy). Blossom is an untrusted, unauthenticated, content-addressed CDN.

| Actor | Can decrypt content? | Why / boundary |
|---|---|---|
| **Passive relay reader** (unauth `REQ Kinds:[3401]/[3404]`) | **No** | Sees only `description`, `teaser`, `token_cost`, `content_type`, `domains`, ciphertext, `ciphertext_hash`, and CEK-wrapped-to-others. No key material it can use. |
| **Unauth Blossom fetcher** | **No** | Blobs are AEAD ciphertext addressed by `sha256(ciphertext)`. Fetching yields ciphertext; no CEK. This is why Blossom needs **no** fetch-auth. |
| **Admitted non-paying agent** (allowlisted, no reservation) | **No** | Admission secures the write pipe (`trust.go:8-23`), not reads. Gets a CEK only via a `deliver` event, which the operator emits **only** on a live scrip reservation (`engine_settle.go:994-1000`). No reservation ⇒ no `wrapped_cek_buyer`. |
| **Paying buyer** (settled match) | **Yes, for that entry** | Receives `wrapped_cek_buyer = NIP-44(operatorPriv, buyerPub, CEK)`. Holds CEK permanently thereafter — post-purchase copy is inherent, economic-defense-only (§5 A/P8). |
| **Operator** | **Yes, everything** | Unwraps every `wrapped_cek_operator`. Permanent trust assumption, consistent with the publisher model ("exchange owns the result"). Blast radius: one key decrypts the whole historical corpus (§4.2, §5 A4/P5). |
| **Mirror / second relay** | **No** | Replicates the full ciphertext + wraps + teasers. The crypto is the **only** boundary; a hostile mirror gains nothing a passive reader doesn't. Stated explicitly so mirrors are in-scope-safe. |
| **Future federated operator B** re-selling A's entry | **Only if A re-wraps to B** | Not solved here. B cannot unwrap a CEK wrapped to A. Either the seller wraps to B at put (impossible — B unknown then) or A re-wraps on demand (A sees plaintext ⇒ cross-operator confidentiality = least-trusted federated operator). Deferred (§8); federation.md:90/284 "encrypted to buyer's key" must be reconciled with this. |

---

## 3. Design

### 3.1 End-to-end flow

1. **PUT** (seller, online once). Seller generates a random 32-byte CEK from `crypto/rand`. Computes `ciphertext = AEAD(CEK, nonce, content)` (nonce prepended). Publishes put(3401) with the §3.3 payload: ciphertext (inline, or a Blossom blob pointer if `len(content) > BlossomOffloadThreshold` = 32 KiB, `state_types.go:138`), `ciphertext_hash = sha256(ciphertext)`, `description`, `teaser`, `token_cost`, `content_type`, `domains`, and `wrapped_cek_operator = NIP-44(sellerPriv, operatorPub, CEK)`. **No plaintext, and no plaintext hash, ever touches the wire.**
2. **ADMISSION / MATCH** (operator, always online). Operator unwraps CEK (`NIP-44(operatorPriv, sellerPub, wrapped_cek_operator)`), decrypts, and runs the quality/dedup/plausibility gates on plaintext **inside its decrypt boundary** (§3.6). Matching itself never touches content — it embeds `Description` only (confirmed: `pkg/matching/engine.go`, `ranking.go:333`), so the removal of the plaintext hash from the wire costs matching nothing.
3. **PREVIEW** (§4.1). No real content chunks. Operator echoes the seller-authored public `teaser` (validated for coherence at put-accept). The `settle(preview)` real-chunk path is deleted.
4. **BUY → BUYER-ACCEPT → reservation.** Unchanged `decAndSaveHold` flow.
5. **DELIVER** (operator, gated on the existing live-reservation check `engine_settle.go:994-1000`). Operator re-wraps the **same** CEK to the buyer: `wrapped_cek_buyer = NIP-44(operatorPriv, buyerPub, CEK)`, where `buyerPub` is derived from the antecedent chain (`MatchBuyerKey`, `engine_settle.go:978`), never from a payload field. Emits deliver(3404) with the §3.4 payload (wrapped CEK + a reference to the already-public ciphertext + `ciphertext_hash`). **Deliver never carries ciphertext.**
6. **BUYER DECRYPT.** Buyer unwraps CEK (`NIP-44(buyerPriv, operatorPub, wrapped_cek_buyer)`), fetches the ciphertext (from the referenced put event, or Blossom), verifies `sha256(ciphertext) == ciphertext_hash`, then AEAD-decrypts.

The **always-online operator is the re-wrapping pivot**, so seller and buyer never overlap — this is what makes async work.

### 3.2 Chosen primitives and the uniform hybrid model

**Ruling: always hybrid (CEK + AEAD), for content of every size.** Bulk content is encrypted with a standard AEAD (**ChaCha20-Poly1305**, matching NIP-44's cipher family and available via the NIP-44 dependency) under a random per-entry CEK. Only the 32-byte CEK is NIP-44-wrapped per recipient. NIP-44 (secp256k1 ECDH → HKDF-SHA256 → ChaCha20 + HMAC-SHA256) is the right wrap primitive: it reuses the one load-bearing secp256k1 npub identity (`identity.go` — the one-npub model), unlike HPKE/X25519 which would force a second keypair per agent.

The unifying invariant: **ciphertext is always public** — inline in the 3401 `Content` when `len(content) ≤ 32 KiB`, or in a Blossom blob otherwise. The CEK is wrapped-to-operator at put and re-wrapped-to-buyer at deliver. Deliver transmits only the 32-byte wrap + a reference. Blob-vs-inline is orthogonal to the crypto; it only decides *where the ciphertext lives*.

> **Contested — losing option:** the creative disposition's dual-path (small content = direct `NIP-44(operatorPub, content)` with no CEK; large = CEK+Blossom). *Tradeoff:* saves one CEK generation + one ECDH for sub-32 KiB puts, but costs a wire-format discriminator, a second decryption code path, and breaks the uniform "always re-wrap exactly 32 bytes" deliver path. Uniformity and one code path win; the micro-optimization is rejected.

Two hygiene requirements: CEK is from `crypto/rand`, **never** derived from content (content-derived keys re-create the P1 hash oracle via convergent encryption); the AEAD nonce is unique per encryption.

### 3.3 PUT payload wire format (kind 3401 `Content`, schema v2)

```json
{
  "v": 2,
  "description": "terse matching key (public, unchanged)",
  "teaser": "seller-authored abstract, public, hard-capped (see §4.1)",
  "token_cost": 1234,
  "content_type": "code",
  "domains": ["matching", "exchange"],
  "enc": {
    "content_alg": "chacha20poly1305",
    "ciphertext_hash": "sha256:<hex of the exact ciphertext bytes below/in blob>",
    "ciphertext": "<base64(nonce || AEAD_ciphertext || tag)>",   // present IFF inline (≤32 KiB)
    "blob_pointer": "blossom:<hex sha256(ciphertext)>",           // present IFF offloaded (>32 KiB)
    "key_wrap": {
      "alg": "nip44-v2-secp256k1",
      "recipient": "<operator x-only pubkey hex>",
      "wrapped": "<base64 NIP-44(sellerPriv, operatorPub, CEK)>"
    }
  }
}
```

- Exactly one of `ciphertext` / `blob_pointer` is present.
- **No `content` field. No `plaintext_content_hash`.** `applyPut` **rejects** any put carrying a legacy plaintext `content` field or lacking a well-formed `enc` (§6).
- `ciphertext_hash` is over the **ciphertext** (integrity, for buyer/Blossom verify) — never over plaintext (§4.4, A7).

### 3.4 DELIVER payload wire format (kind 3404 settle, operator-emitted)

```json
{
  "phase": "deliver",
  "v": 2,
  "entry_id": "<entry id>",
  "content_alg": "chacha20poly1305",
  "ciphertext_ref": { "put_event": "<3401 event id>" },   // inline case: fetch ciphertext from the put event
  // or: "ciphertext_ref": { "blob_pointer": "blossom:<...>" }  // offloaded case
  "ciphertext_hash": "sha256:<same value published at put>",
  "key_wrap": {
    "alg": "nip44-v2-secp256k1",
    "recipient": "<buyer x-only pubkey, DERIVED FROM ANTECEDENT CHAIN>",
    "wrapped": "<base64 NIP-44(operatorPriv, buyerPub, CEK)>"
  },
  "guide": "Unwrap the CEK, fetch the ciphertext via ciphertext_ref, verify sha256(ciphertext)==ciphertext_hash, then AEAD-decrypt. Mismatch ⇒ dispute, do NOT settle(complete)."
}
```

- `recipient` **MUST** equal `MatchBuyerKey(matchMsgID)` from the antecedent chain (`engine_settle.go:978`). A captured deliver replayed toward a different (unfunded) buyer is simply undecryptable by them — the recipient key **is** the binding (§4.5, A2/P7).
- Deliver never carries content or ciphertext, only the wrap + a reference.

### 3.5 CEK custody & rotation

- **At rest:** the CEK is not a separate persisted secret. It is re-derivable on demand by the operator from `wrapped_cek_operator` (stored on the entry) + the operator key. `InventoryEntry` gains a `WrappedCEKOperator` field (Replay-safe, folded from the put event) rather than a raw-CEK field, so no plaintext key sits in state.
- **Per-entry, immutable.** One CEK per entry, generated once, shared across all buyers of that entry (the ciphertext is shared and the seller is gone — it cannot be per-sale). The operator re-wraps the *same* CEK per buyer.
- **Operator key custody — two SEPARATE properties, corrected (dontguess-973 C1):** an earlier draft of this section credited 1Password with removing the operator's in-process plaintext exposure. That conflated two distinct properties and is corrected here:
  - **At-rest / transfer custody** (persisted at `$DG_HOME` today, `cmd/dontguess/serve.go:459` → `pkg/identity/store.go:61-77`/`pkg/identity/keyfile.go`, mode 0600, verified on every load per dontguess-973 C3): **1Password IS the right tool for this** — it protects the key file when it is not loaded into a running operator process (backup, transfer between hosts via `operator export/import`, at-rest-on-disk theft). This is what 1Password custody (§4.2, `operator export/import`, dontguess-ead) actually buys.
  - **In-process ECDH side-channel:** while `serve` is running, the operator's live secp256k1 scalar sits in process memory and is used directly to perform NIP-44 ECDH key agreement (unwrap every CEK) and BIP-340 Schnorr signing. **1Password custody does nothing for this window** — the key must be loaded in-process to do its job. Only a **hardware HSM that performs the ECDH/Schnorr operation itself** (key never leaves the device) removes this side-channel; a software key loaded from 1Password into process memory is exactly as exposed in-process as one loaded from a plain file. dontguess-973 C3 ships best-effort mitigation for the narrower "paged to disk swap" sub-case (verify the key file is 0600 on load; `mlock`/no-swap the loaded scalar where the platform allows it) — this reduces, but does not remove, the in-process exposure window; it is not a side-channel fix and must not be described as one.
- **Rotation runbook:** rotating the operator key orphans every existing `wrapped_cek_operator` (they were wrapped to the old pubkey). Because the operator holds every CEK (via unwrap), rotation re-wraps the CEK index to the new key in a one-time local pass; the old privkey is retained decrypt-only until that pass completes. Optionally advertise a separate **operator encryption subkey** distinct from the signing key so the signing key can be HSM'd/rotated independently. Operator-facing rotation steps (dontguess-973 C2) are published in the operator runbook — `docs/design/onboarding-tiered-scaling-federation.md` §7.3 "Operator-key leak and rotation runbook".

### 3.6 Fold determinism trade-off (stated explicitly)

Today `applyPut` is a pure function of the raw log: any node replaying the relay recomputes byte-identical `contentHashIndex`/inventory (`blossom.go:14-20`, `state_core.go:67-146`). Under encryption, the quality/dedup/plausibility gates (`state_put.go:625-720`) require `NIP-44Dec(operatorPriv, …)` first — only the operator can run them. **Ruling: the gates move inside the operator's decrypt boundary. A non-operator replaying the raw log no longer reconstructs `contentHashIndex`.** This is acceptable for a single-operator team tier (the operator is the only authoritative folder) and is documented here as a deliberate trade-off, not discovered later. It is the direct cause of the dedup-hash decision in §4.4, and it is a **breaking dependency for any future multi-operator federation** (§8).

---

## 4. The five sharp edges

### 4.1 Preview redesign without leakage

**Delete** the real-content preview machinery for team tier: `PreviewAssembler` chunk emission in `sendPreviewResponse` (`engine_settle.go:895-905`, 5 chunks / 15-25% of plaintext), and the put-time `buildInlinePreviewBytes` real-slice-as-`entry.Content` for offloaded entries (`state_put.go:681,727`). Both put real plaintext where it can leak.

**Replace** with a single optional **seller-authored `teaser`** field carried on the public put (§3.3), a distinct field from `description` (description = terse matching key; teaser = richer human abstract — kept separate so a verbose teaser doesn't dilute match precision). `sendPreviewResponse` just echoes the stored teaser.

Two hard constraints:
- **Bounded.** Enforce a hard length/ratio cap on `teaser` at `applyPut` — treat teaser bytes as intentionally-published. Nothing stops a greedy seller pasting whole content as "teaser"; the cap plus the fact that it is the seller's *own* content leaking makes over-exposure economically self-defeating.
- **Coherence-checked, not free-form-trusted.** Since the operator already decrypts at put-accept, it runs the existing coherence gate (`IsHighReuseArtifact` / `descriptionContentCoherent`, `state_put.go:431-553`) to sanity-check `teaser` vs plaintext, catching teaser/content bait-and-switch that a buyer would otherwise only discover post-purchase.

Keep the `SmallContentThreshold = 500` (`state_types.go:88`) auto-refund path (`handleSettleSmallContentDispute`) as buyer protection — a teaser can still be uninformative for tiny content.

> **Contested — losing option:** the systems-pragmatist's operator-generated truncated real extract (deterministic, content-derived, anti-fraud). *Tradeoff:* it preserves a "grounded in real content, hard to fake" guarantee — but it puts *real plaintext* (even if truncated) on the public wire, which is exactly the leak class we are removing. Seller-authored + operator-coherence-checked wins: it keeps the seller consciously choosing exposure while recovering most of the anti-fraud property via the coherence gate + the economic dispute/reputation backstop. The lost property is the *deterministic* content-derived guarantee.

### 4.2 CEK custody / rotation + the operator-plaintext trust assumption

The operator sees all plaintext (unwraps every CEK) — a **permanent** trust assumption, consistent with the publisher model. This is *not* a v1 stopgap (see §4.3 for why PRE cannot remove it). Custody and rotation per §3.5.

**Custody wording correction (dontguess-973 C1, HSM-gate condition on dontguess-973's defer-with-conditions ruling):** §3.5 previously read as though 1Password custody addressed the operator-plaintext trust assumption described in this section. It does not. **1Password/HSM-backed *at-rest and transfer* custody (§3.5) is a SEPARATE property from the in-process ECDH side-channel this section describes.** At-rest custody protects the key file while `serve` is not running or while it is being moved between hosts. It buys nothing while the operator process is up and actively performing NIP-44 ECDH/Schnorr with the live scalar in memory — that exposure is inherent to the "operator sees all plaintext" trust model and is removed only by a hardware HSM that performs the ECDH operation itself (key material never leaves the device; see §3.5, §4.3). Do not credit 1Password with closing the in-process side-channel anywhere in this doc or in the operator runbook.

**Permanent constraint — no forward secrecy, retroactive blast radius (dontguess-973 C2 — threat-model line, published):** both the wrap envelopes and the ciphertext live forever on an append-only relay (and Blossom). **On operator-key leak, the entire immutable historical corpus is offline-decryptable from data already scraped off the relay and Blossom** — every `wrapped_cek_operator` ever emitted unwraps with the leaked key, and every ciphertext blob is already public. Rotation (§3.5) gives NO retroactive protection because old envelopes are immutable — it only protects content put *after* rotation completes. MEMORY notes the operator key lives as a file in `~/.dontguess` and that a prior infra GC already overwrote a campfire key — so this is a live custody risk, not theoretical. This threat-model line is republished verbatim in the operator runbook and the informed-consent block (`docs/design/onboarding-tiered-scaling-federation.md` §7.3, §8.9-equivalent consent language) so operators and putting agents see it before they commit content, not after a leak. Mitigations shipped/planned: 1Password/HSM at-rest custody (§3.5, narrows leak-at-rest, does not touch the in-process window above), dontguess-973 C3 best-effort mlock/no-swap + 0600-on-load verification of the loaded scalar (narrows the "paged to disk" sub-case of the in-process window, still not a side-channel fix), optional operator epoch subkeys (accepting entries expire with their epoch — which fights the publisher model's indefinite re-sale, a tension stated openly). This pushes any operator-blind upgrade up the priority list — but see §4.3 for why "blind" is not actually reachable.

### 4.3 Wire-format forward-compat for PRE

The wire is PRE-ready **by construction**: the versioned `enc` container (§3.3) keeps `content_alg` (the AEAD over the body) **independent** of `key_wrap.alg` (the recipient wrap). A future scheme swaps only `key_wrap.alg` and the `wrapped` contents; the AEAD ciphertext, the Blossom blob, and every surrounding field stay byte-identical. `v` on the whole put drives both migration (§7) and downgrade detection (§6), and `applyPut` dispatches decode on it.

**Honesty correction (ruling):** the operator-**blind** proxy-re-encryption upgrade (Umbral/AFGH) is **not** a real non-breaking drop-in and must be labeled **aspirational / research**, not a promised upgrade path. Unidirectional PRE requires the **delegator (seller)** to generate a pairwise re-key `rk_{seller→buyer}`, which requires knowing the buyer. This design's entire async win is that the seller is **offline forever** and buyers are **unknown at put time** — so no party can generate `rk` later without the seller's secret. The only PRE variants that fit (threshold proxy cohorts, or seller-online-at-delivery) reintroduce exactly the availability problem the operator pivot solved. **Therefore single-operator-sees-plaintext is the permanent trust model under this async design.** We keep the versioned wire slot (cheap, harmless, and lets *some* future scheme slot in non-breakingly) but we do **not** market a blind-operator future the construction cannot deliver.

### 4.4 Matching / dedup hash — the guess-confirmation oracle

**Ruling: `plaintext_content_hash` never goes on the wire.** Publishing an unsalted `sha256(plaintext)` on the public put (`state_put.go:657-659`) converts a private dedup key into a public guess-confirmation + cross-entry-correlation oracle: any passive scraper hashes a guessed plaintext (a config, a known snippet, a canonical answer — precisely the reusable-artifact class the exchange optimizes for) and confirms it for free, defeating the AEAD; identical hashes also correlate identical inventory globally. It buys **nothing**: matching embeds `Description` only (`engine.go`, `ranking.go:333`), and the operator unwraps every CEK so it dedups on plaintext locally anyway.

Resolution:
- Remove `plaintext_content_hash` from the payload entirely. Dedup stays **100% operator-local**: the operator recomputes `sha256(plaintext)` **after decrypt** at put-accept and keys `contentHashIndex` on that value — exactly mirroring the existing "Never trust hash from payload" invariant (`state_put.go:656`). dontguess-327's `contentHashIndex` becomes operator-side state, **not** a wire field.
- Keep plaintext dedup a **non-protocol-visible best-effort** so its disappearance under any future blind mode is invisible to clients (this is what preserves forward-compat of the dedup story).
- **If** federated cross-operator dedup is ever genuinely required, use `HMAC(operator_secret, plaintext)` — keyed so it is not a public brute-force oracle. *Losing option:* even the HMAC still leaks plaintext-equality across puts and breaks cross-operator dedup (different operator secrets), so operator-side-only is strictly better and is the default.
- **Separate hash roles (A7):** `ciphertext_hash` (over ciphertext, on the wire, for Blossom/buyer integrity verify) and the plaintext dedup value (operator-local) are two **distinct** values and must never be conflated — a single field cannot serve both, since random CEKs make identical plaintext produce divergent ciphertext.

### 4.5 NIP-44 envelope correctness (wrap at put, re-wrap at deliver)

Keys are secp256k1 BIP-340 x-only (`secp256k1.go:75-96`); btcec/v2 v2.5.0 is present (`go.mod:6`); **no NIP-44/NIP-04 exists in non-test Go today** (verified) — this is net-new crypto (~250-400 LOC + possibly a new dep). Correctness edges to nail:

1. **x-only → even-Y parity lift.** All three parties must lift the counterparty's 32-byte x-only key to the even-Y point (0x02) identically before ECDH, or the shared secret diverges and decryption fails at the buyer. Confine this in one place behind the Signer port.
2. **Exact NIP-44 v2 KDF.** HKDF-extract (salt `nip44-v2`) over the ECDH x-coord → per-message HKDF-expand with a 32-byte nonce → ChaCha20 key/nonce + HMAC-SHA256 key; version byte `0x02`; power-of-two length padding; MAC-then-check over `nonce || ciphertext`. **Implement against the published NIP-44 v2 test vectors — do not hand-roll the padding/HMAC.** NIP-44 has had real padding-length side-channel bugs elsewhere.
3. **Tiny-payload handling.** The CEK wrap is a fixed 32-byte plaintext; confirm min-padding behavior on the tiny payload against the vectors.
4. **No AAD — bind via recipient + signed chain.** NIP-44 has no associated-data field, so `wrapped_cek_buyer` cannot be crypto-bound to `match_id` *inside* the envelope. Binding instead rests on: (a) the operator's Schnorr signature over the antecedent-chained deliver event, (b) the deliver→match→buyer antecedent chain, and (c) the existing live-reservation gate (`engine_settle.go:994-1000`). The **recipient key derived from the chain** (`engine_settle.go:978`) is the anti-replay binding — never a payload-supplied pubkey. This combination is adequate for replay/authorization.
5. **Same key signs and does ECDH.** The one secp256k1 keypair both Schnorr-signs and NIP-44-ECDH-decrypts. This is nostr convention and acceptable; call it out. It forces a Signer-port extension (§7): add an ECDH/`Seal`/`Open` accessor to the `Signer` interface (`identity.go:34-48`) that every mock and any future HSM must satisfy (an HSM must now perform key-agreement, not only sign).

---

## 5. Adversary attacks — resolution table

| ID | Attack | Resolution / permanent constraint |
|---|---|---|
| **A1 / P1 / P3** | Public unsalted `plaintext_content_hash` = guess-confirmation + correlation oracle | **RESOLVED.** Removed from wire; dedup recomputed operator-side post-decrypt, `contentHashIndex` operator-local (§4.4). |
| **A5 / P2** | Nothing enforces ciphertext-only; old/rogue allowlisted client publishes cleartext | **RESOLVED.** `applyPut` fail-closed: rejects any 3401 with a plaintext `content` field or lacking well-formed `enc`, dispatched on `v` (§6). |
| **A6 / P7** | `settle(preview)` broadcasts 15-25% real plaintext; teaser uncapped | **RESOLVED.** Real-chunk path deleted; seller teaser bounded + coherence-checked (§4.1). |
| **A4 / P5** | Envelope turns transient exposure into permanent offline-decryptable archive under one long-lived key | **PERMANENT CONSTRAINT.** No forward secrecy on append-only medium. Mitigations: HSM/1Password custody, epoch subkeys (fights re-sale), documented total-history-break on key leak (§4.2). |
| **A2 / P8** | No forward secrecy; buyer-key compromise retro-decrypts purchases; wrap must bind to signed reservation key | **RESOLVED (binding) + CONSTRAINT (FS).** `wrapped_cek_buyer` recipient = antecedent-chain buyer key, never payload field (§3.4, §4.5). No-FS documented; CEK per-entry so first buyer's CEK unlocks the public ciphertext for anyone who obtains it — economic defense only. |
| **A7 / P3** | Blossom integrity hash vs plaintext dedup conflated | **RESOLVED.** `ciphertext_hash` (integrity, wire) and plaintext dedup (operator-local) are distinct; Blossom stores/addresses ciphertext, `FetchAndVerifyBlob` (`blossom.go:114`) verifies against ciphertext hash (§4.4, §7). |
| **A8 / P6** | Dedup-by-plaintext-hash vs future blind-operator PRE mutually exclusive | **RESOLVED by versioning.** `v` tag on every put; dedup non-protocol-visible best-effort; `key_wrap` a generic slot (§4.3). (PRE itself relabeled aspirational — §4.3.) |
| **A9 / P8** | NIP-44 even-Y lift, tiny-payload padding, no per-sale freshness | **RESOLVED (implementation reqs).** Test-vector validation, single even-Y lift point, static-forwarding accepted (reduces to "CEK stays secret") (§4.5). |
| **C1 / P3 (Blossom plaintext)** | Blossom stores raw plaintext behind public pointer — second free-scrape vector | **RESOLVED.** Blossom blob = AEAD ciphertext addressed by `sha256(ciphertext)`; unauth fetch now safe (§3.2, §7). |
| **P4** | Operator-blind PRE sold as non-breaking upgrade | **CORRECTED.** PRE incompatible with seller-offline + unknown-future-buyer; relabeled aspirational/research; single-operator-sees-plaintext is permanent (§4.3). |
| **Fold-determinism (pragmatist)** | Gates on plaintext break replay-by-anyone under encryption | **ACCEPTED TRADE-OFF.** Gates move inside operator decrypt boundary; non-operator replayers can't reproduce `contentHashIndex`; acceptable single-operator, breaking for federation (§3.6). |
| **Post-purchase copy** | Paying buyer redistributes CEK/plaintext | **PERMANENT CONSTRAINT.** Inherent; economic/reputation defense only. |

---

## 6. Enforcement — how ciphertext-only is guaranteed

Confidentiality-by-construction requires that a downgrade (a rogue or pre-upgrade allowlisted client publishing plaintext) **cannot** reopen the leak. strfry cannot validate payload shape, so the **operator's `applyPut` fold is the single enforcement point**:

1. **Fail-closed `applyPut`.** Under a per-exchange `encrypted-required` flag (team tier), `applyPut` **rejects** (drops, does not fold into `pendingPuts`) any 3401 that: carries a legacy plaintext `content` field; or lacks a well-formed `enc` object with `{content_alg, ciphertext_hash, (ciphertext|blob_pointer), key_wrap{alg,recipient,wrapped}}`; or whose `v < 2`. Individual tier (local socket, `ScripStore == nil`, already confidential) keeps the plaintext path legal — the flag is strictly team-tier.
2. **Relay write-allowlist is the first line.** The strfry NIP-42 write-allowlist (`trust.go:8-23`; reference_relay_infra hot-reload admit) already blocks non-admitted publishers, so plaintext injection to the operator's relay requires an *admitted* key. Residual risk collapses to "an honest admitted seller runs a bad client and leaks their **own** content" — economically self-defeating and acceptable.
3. **Schema-version cutover, explicit tag.** Dispatch on the explicit `v` tag, never implicit field-presence sniffing. A mixed log (old plaintext 3401 + new `v:2` 3401) is expected during Replay; the fold handles both shapes but only folds `v:2` as confidential inventory once the flag is set.
4. **Adapter unchanged.** `pkg/nostr/adapter.go:124` stays payload-opaque (it never parses the payload) — a genuine plus; zero edits there. Enforcement lives in the engine fold, where it belongs.

**Acceptance test for the property:** a passive `REQ Kinds:[3401]` and an unauthenticated Blossom fetch each yield **only ciphertext** for any `v:2` entry; a plaintext put is dropped by `applyPut` and never appears in inventory.

---

## 7. Feasibility & phased build plan

**Crypto reality:** btcec/v2 present; **no NIP-44/NIP-04/ChaCha20 in non-test Go** (verified); `golang.org/x/crypto` not in `go.mod`. Net-new crypto + a dependency. `Signer` (`identity.go:34-48`) is Schnorr-sign-only — no ECDH accessor; `Secp256k1Identity.priv` unexported (`secp256k1.go:16`).

**Change surface (file:line → work, LOC):**

| Area | File:line | Change | LOC |
|---|---|---|---|
| PUT build | `pkg/relayclient/relayclient.go:341-347` | Replace `base64(content)` with CEK-gen + AEAD + `enc` envelope + `wrapped_cek_operator`; add `teaser`; drop plaintext hash | 40-60 + NIP-44 call |
| PUT fold | `pkg/exchange/state_put.go:579-720` | Decrypt-then-gate reorder inside operator boundary; reject non-`enc` puts; recompute plaintext hash post-decrypt; store `WrappedCEKOperator`; move plausibility/size gates post-decrypt (ciphertext size ≠ plaintext size due to AEAD+padding) | 80-120 |
| Entry state | `pkg/exchange/state_types.go` | Add `WrappedCEKOperator`, `CiphertextHash`; repurpose/rename fields; teaser field | 20-30 |
| Preview | `engine_settle.go:836-905`, `preview.go`, `state_put.go:681,727` | Delete real-chunk emission + `buildInlinePreviewBytes`; echo seller teaser; coherence-check | 60-100 (net deletion) |
| DELIVER | `engine_settle.go:1042-1121` (`emitDeliverContent`/`emitDeliverPointer`/`sendDeliverMessage`) | Emit `wrapped_cek_buyer` + `ciphertext_ref` + `ciphertext_hash`; re-wrap CEK to antecedent buyer key; stop inlining ciphertext | 30-50 |
| Buyer decrypt | `pkg/relayclient/settle.go:416-470` (`verifyDeliver`) | Unwrap CEK, fetch ciphertext (put event / Blossom), verify `ciphertext_hash`, AEAD-decrypt | 50-70 |
| Signer port | `pkg/identity/identity.go:34-48`, `secp256k1.go` | Add ECDH accessor; update all mocks (~20+ test doubles) | 30 + mocks |
| Blossom | `blossom.go:56-114` | Store/address ciphertext; `FetchAndVerifyBlob` checks ciphertext hash (interface unchanged) | ~10 |
| NIP-44 | new pkg | Port/vendor reviewed impl + KATs | 250-400 |
| Tests | ~20 `_test.go` touching the `content` JSON key; 8 shared put-builder helpers | Fixture migration to `enc` shape (Sonnet-tier mechanical) | 1-2 days |

**What breaks:**
- **dontguess-327 `contentHashIndex`** becomes operator-local, computed post-decrypt (§4.4). Any design assuming it is wire-visible or replay-reproducible-by-anyone breaks (§3.6). Non-operator replayers no longer reconstruct it — accepted.
- **Buyer-side Blossom fetch does not exist** — `settle.go:433-438` fails loud on any pointer deliver ("Blossom fetch is DEFERRED"). Large-content encrypted delivery has **zero existing consumer**; it is new work, not part of "add encryption."

**Migration of existing plaintext puts:** every already-broadcast 3401 is **permanently public** on an append-only relay — no claw-back is possible or attempted. Hard cutover: flip `encrypted-required`, start encrypting new puts, grandfather legacy plaintext entries to expire via TTL, operator rebuilds `contentHashIndex` from its own decrypted view. Replay must dispatch on the explicit `v` tag to walk the mixed historical log. Affected sellers are told historical content cannot be un-leaked; re-put-as-encrypted only protects new copies.

**Phases (do not bundle "add encryption" with "add the buyer's first Blossom fetch"):**
- **Phase 0 — crypto foundation.** Vendor/port a reviewed NIP-44 v2 Go impl; validate against official test vectors **independently** before wiring. Add ECDH to `Signer` port + update mocks. A crypto bug here is silent and catastrophic.
- **Phase 1 — PUT.** `buildPutMessage` envelope + `applyPut` decrypt-then-gate + fail-closed rejection. Single-node round-trip test.
- **Phase 2 — DELIVER (inline / ≤32 KiB).** `emitDeliverContent` `wrapped_cek_buyer` + `verifyDeliver` decrypt. Full E2E extending `TestE2E_ContentDeliveryRoundTrip` (`e2e_test.go:1556`). Passive-REQ-yields-only-ciphertext assertion.
- **Phase 3 — preview redesign.** Delete real-chunk path; seller teaser + coherence check + cap.
- **Phase 4 — Blossom ciphertext-ref (>32 KiB).** Only after (and alongside) building the buyer-side Blossom fetch client that `settle.go:433-438` currently defers. Encrypted large-content delivery lands here.
- **PRE:** **not attempted.** Wire is versioned so a fitting future scheme can slot in; documented aspirational (§8).

Ground-source testing (project rule 10): no phase closes with a skipped/absent test for the interface it touches. Phase 2/4 must include the "passive scraper + unauth Blossom fetch both yield only ciphertext" test as the confidentiality-property gate.

---

## 8. Open questions / deferred

1. **Operator-blind PRE — named future phase, currently blocked.** Relabeled aspirational/research (§4.3): cryptographically incompatible with seller-offline + unknown-future-buyers. Revisit only if a threshold-proxy or seller-online-at-delivery variant is acceptable, which reintroduces the availability problem. Wire is forward-compatible regardless.
2. **Federated / mirror operators (federation.md:90,284 "encrypted to buyer's key").** A federated operator B re-selling A's entry cannot unwrap a CEK wrapped to A. Options: seller wraps to B at put (impossible — B unknown then) or A re-wraps on demand (A sees plaintext ⇒ cross-operator confidentiality = least-trusted federated operator). The federated threat model needs an explicit "which operators can decrypt" statement before federation ships. Also: any node folding authoritatively in federation cannot reproduce the operator-local dedup index (§3.6).
3. **Cross-seller vs per-seller dedup.** If only per-seller anti-replay is required, operator-side dedup keyed by `(sellerKey, plaintextHash)` fully suffices and the oracle concern is trivially clean. If cross-seller/global dedup is a real product requirement, decide whether leaking plaintext-equality across sellers via an operator HMAC is acceptable, or drop dedup in favor of confidentiality.
4. **Teaser bait-and-switch depth.** Operator coherence-check at put-accept (§4.1) is the ruling; is the economic dispute/reputation path a sufficient additional backstop, or is a stricter operator teaser-vs-content similarity threshold warranted?
5. **`token_cost` plausibility on padded ciphertext.** Confirmed moved operator-side post-decrypt (uses plaintext size). No wire-side plausibility on ciphertext (NIP-44 power-of-two padding inflates small payloads materially).
6. **Operator-key custody timeline.** HSM/1Password backing before team-tier launch (§4.2); confirm the Signer ECDH extension preserves hardware-key compatibility (an HSM must now perform key-agreement, not only Schnorr sign).
7. **NIP-44 dependency choice.** Add `nbd-wtf/go-nostr`'s nip44 (pulls a new crypto surface into the release gate) vs in-tree impl behind the Signer port (needs its own KATs). Either way, validate against the official NIP-44 vectors.
8. **Entry-level revocation.** A seller cannot withdraw content once ciphertext is public (append-only). Likely "impossible, economic-only" — state it in the convention spec.
9. **Consent language.** Write the operator-sees-plaintext trust model explicitly into the (Draft) `federation.md` and the CLAUDE.md publisher-model language, so buyers/sellers have informed consent rather than it being implied by "the exchange owns the result."
<!-- adversarial-design: 4 dispositions (adversary/creative/systems/security-purist, opus×3+sonnet) + architect(opus); 2026-07-13 -->
<!-- source item: dontguess-541 -->
