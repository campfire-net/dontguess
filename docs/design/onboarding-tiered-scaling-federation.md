# Design: The Scaling Ladder — Brain-Dead-Simple DontGuess at Every Rung

**Status:** Design — synthesis of 4-disposition adversarial deliberation. SOLO + FLEET rungs are decision-complete and build-ready bottom-up; the FEDERATION rung is **paper-first / conditional** (three §541 conflicts must close before any `federate` code lands).
**Scope:** Command surface (`up`/`invite`/`join`/`federate`), allowlist unification, operator robustness (folds 347/7b2/61a), federation trust model, tier transitions, docs.
**Source of truth:** convention spec > this doc > §541 (`content-confidentiality-envelope-541.md`) > code. Where federation.md/federation-modes.md conflict with §541, **§541 wins** (they predate the nostr-first pivot and are stale — see Tier-Transition §6 doc-cascade).
**Model tier for crypto/federation-trust work:** Opus (security), never Fable.

---

## 0. North star & the firm ruling

**The ladder (user ruling, firm):**

| Rung | What it is | One-command reach |
|---|---|---|
| **SOLO** | one machine, individual tier, local store, **no relay** | `dontguess up` |
| **FLEET** | many machines, **ONE shared encrypted TEAM-TIER operator** over relays | `dontguess up --relay <urls>` (operator) + `dontguess join <token>` (member) |
| **FEDERATION** | a team = **MANY independent operators** federating with trust for global liquidity | `dontguess federate <peer>` — **deliberate, not brain-dead** (§5) |

**North star:** every rung reachable in ~one command, documented identically for autonomous agents and humans. **The exception the adversary forced:** `federate` is the single most consequential trust decision on the ladder (it can expose your plaintext corpus, §5) — it stays **explicit, per-peer, scoped, x402-escrowed, revocable**. "Brain-dead simple" applies to `up`/`invite`/`join`, NOT `federate`.

**Sequencing law (adversary ruling, firm):** ship bottom-up. FLEET's one-command bootstrap is gated on the robustness + identity + egress fixes (ADV-4/5/7/17/18) landing first. FEDERATION's `federate` verb is gated on ADV-9/10/13 being resolved **on paper** first. Do not ship either rung as a trusting one-liner before its gate closes.

**The root cause (creative through-line):** every friction found standing up v0.7.0 live (c06/113/347/7b2/61a) is a symptom of ONE thing — the exchange keeps trust state in **two places that don't talk** (hand-edited strfry `write-allowlist.json` + `Config.FleetAllowlist` read once at startup) and uses **two operator identities** (opaque `local-operator.key` solo vs secp256k1 `nostr-operator.key` team, confirmed `serve.go:214-221`). Collapse both duplications and the ladder becomes one-command by construction.

---

## 1. Command surface — RULING: single-bootstrap-with-flags + composed primitives

**Ruling:** `up` is a single auto-detecting bootstrap; it **composes** the existing verbs (`init`/`serve`/`agent-init`/`allowlist`/`mint`) rather than replacing them. The low-level verbs stay for advanced/subagent/inspection use and are what the systemd unit and manual paths invoke. Guided multi-step only where a machine boundary is crossed (`invite`/`join`) or a trust decision is made (`federate`).

### `dontguess up` — auto-detects the rung from state (RPT: tier is a behavioral consequence, not a preference flag)

`up` reads config + env and dispatches on **deployment state** (mirrors the existing `serve.go:204` auto-select): relay URLs present (in config OR `--relay` OR `DONTGUESS_RELAY_URLS`) ⇒ team tier; else ⇒ solo. Idempotent, re-runnable.

```
# SOLO (default) — one machine, local, no relay
$ dontguess up
  ✓ operator identity (secp256k1) ready         # minted ONCE, permanent across all rungs (§6)
  ✓ store initialized at ~/.dontguess
  ✓ engine running (individual tier, plaintext-local)
  ✓ boot service installed (systemd --user, linger enabled)

# FLEET operator — one flag promotes the SAME operator to team tier
$ dontguess up --relay ws://192.168.2.40:7777,ws://192.168.2.41:7777
  ✓ operator identity (secp256k1) ready         # SAME key as solo — no fork, no re-sign
  ✓ team tier: ScripStore + encrypted-required armed
  ✓ self-admitted operator agent → fleet allowlist + relay roster (§2)
  ✓ relay legs attaching (async) …              # local cmds work NOW (347 fix)
  ✓ boot service installed
```

- **Solo `up`** ≈ `resolveDGHome()` + `runServeLocal` (already self-bootstrapping) wrapped in a backgrounding/linger/pidfile shim. ~50-80 LOC.
- **`up --relay`** = orchestration over existing `exchange.Init` → serve (with the §4 fixes landed first) → **self-admit** (operator's own agent key to BOTH gates via the §2 one-action admit) → **install boot service** (systemd `--user` + `loginctl enable-linger`, ADV-6). Net-new systemd templating (~150-250 LOC command + ~80-150 LOC unit install), **Linux/systemd-only** (macOS/launchd is a separate unscoped item).
- **`up --relay` MUST NOT auto-mint a competing sequencer.** The `§3.9` guard (`assertRelayServeHasOperatorConfig`, `serve.go:274`) and `assertAdvertiseEqualsSign` (`serve.go:286`) MUST still fire inside `up`. Run on machine 2 without importing the operator private key, `up --relay` must **detect an existing operator's events on the relay and REFUSE to mint** (ADV-4) — see §6 `operator export/import`.

### `dontguess invite <name>` / `dontguess join <token>` — self-service onboarding (RPT: the product onboards you)

```
# On the operator:
$ dontguess invite alice --scrip 50000 --ttl 72h
  invite token: dgi1_<base64 operator-signed blob>     # relay URLs + operator npub pin + one-time grant id + genesis scrip

# On alice's machine (one paste):
$ dontguess join dgi1_…
  ✓ verified operator signature, not expired
  ✓ provisioned member identity (agent-init --fleet-member internally)
  ✓ admitted to fleet allowlist + relay roster
  ✓ genesis grant: 50000 scrip
  → you can buy/put/settle now
```

`invite` mints an **operator-signed, scoped, single-use, TTL'd, npub-bound-on-redeem** token carrying: relay URLs, operator npub (member pins it), a one-time admission grant id, optional genesis scrip. `join` decodes+verifies the operator signature, self-provisions the member key via `agent-init --fleet-member` (fail-closed: no default mint, `agent_init.go:122`), publishes a **redeem event (new kind 3410)** signed by the fresh member key referencing the invite id. The operator's serve reader loop (`serve_relay.go` runReader dispatch — adding a case is architecturally cheap) verifies (invite sig valid, not expired, not already redeemed — operator persists redeemed ids) and promotes the key into BOTH gates live (§3) + auto-mints the grant. Absorbs `agent-init`+`allowlist add`+`mint` into one redeem.

**The relay redeem write-hole (security-critical):** strfry's writePolicy permits an un-allowlisted key to publish **EXACTLY kind 3410 AND only when it embeds a valid operator-signed invite** (the policy already pins the operator pubkey, so it verifies cheaply), rate-limited N/hour/IP. Garbage is dropped at the relay edge; final promotion is the operator's. This keeps invite/join as **two enforcement points**, same defense-in-depth as §2 (ADV-15).

### `dontguess federate <peer-beacon>` — see §5. Deliberate, not one-command-trusting.

**How existing verbs fit:** `init` becomes an internal (still-callable) step of `up`; `serve` stays the daemon the boot unit runs; `agent-init` is called internally by `join`; `allowlist add/remove` become the live IPC path (§3) instead of restart-only config writes; `mint` stays the operator god-button and is what `join` calls internally for the genesis grant.

---

## 2. Allowlist unification — RULING: one operator-signed ROSTER event, two projections

**The c06 anti-pattern to kill:** admitting one agent needs TWO out-of-band allowlists (SSH+sudo edit `/etc/strfry/write-allowlist.json` on every relay AND `dontguess allowlist add`), and they don't talk.

**Ruling (chosen mechanism — creative P3 ∩ pragmatist option 3):** the fleet allowlist becomes an **operator-signed parameterized-replaceable nostr roster event** (`kind 30078`, `d`-tag=`fleet`, authored by the operator key). It is the single source of truth, decoded twice:

| Enforcement point | Reads | Enforces | Semantics |
|---|---|---|---|
| **Relay (coarse, "first line")** | strfry `writePolicy.py` **subscribes to + caches** the latest operator-signed roster from its own localhost relay, verifies the **single pinned operator pubkey's signature** | *can this key write to the pipe at all* | write-admission (NIP-42) |
| **Exchange (fine, "single enforcement point")** | `TrustChecker.KeySet` folds the same roster from the event log | *which operation, at what reputation floor* (`trust.go` Level/Check) | payload/operation trust |

The two lists are two **projections of one signed event** — cryptographically **un-desyncable** (kills c06 AND ADV-3 drift-on-partial-failure). **The operator holds NO SSH/relay-admin credentials** — this rejects the SSH-push option (ADV-1: SSH-push merges relay-admin blast radius onto the operator key that §541 already says decrypts the whole corpus). Relay bootstrap is **ONCE** (install strfry + writePolicy pinned to operator pubkey as sole roster authority); no SSH ever touches it again.

**Defense-in-depth preserved (ADV-2, purist HIGH):** the two gates enforce **different properties** and must never collapse into "relay write == exchange trust." The relay retains an **independent property the operator cannot override**: a relay-owner-held **denylist + per-key rate cap** applied AFTER the roster check. Write-gate = `operator-signed-admit AND relay-owner-policy`. The exchange fold (`applyPut`) NEVER stops running because "the relay already gates writes" (§541 §6 is explicit: strfry cannot validate payload shape).

**Losing option (rejected):** operator SSH-pushes `write-allowlist.json` on each admit — reintroduces the SSH coupling, a desync window, and merged blast radius.

**Reconciliation of the two operators-over-relays question:** the operator owns/federates over its relays; `up --relay` bootstraps the relay's writePolicy to pin the operator pubkey as roster authority. In FLEET, member machines are relay **clients**, not operators — they never touch the roster.

---

## 3. Hot-reload the fleet allowlist — RULING: live operator-key-signed IPC admit

**The 113 anti-pattern to kill:** the fleet allowlist is loaded at serve STARTUP only → admitting an agent needs an operator RESTART → which re-triggers the 61a full-history re-read.

**Ruling:** `KeySet` is **already** mutable + mutex-guarded (`trust.go:261-311` Add/Remove/Keys; `RemoveMember` `trust.go:432` already wired for a membership-refresh loop). The only gap: `allowlist.go` add/remove today only rewrite on-disk JSON with **no path into a running process's live `*KeySet`**. Fix = one new **IPC op** (mirroring `OpMint`'s pattern) that:

1. `KeySet.Add` immediately (live, sub-second, no restart)
2. republishes the roster event (§2) so the writePolicy updates on its next subscription push
3. persists to `Config.FleetAllowlist` for restart durability

`allowlist remove` mirrors via `KeySet.Remove` + roster republish (live de-admission). No operator restart ⇒ the 61a `Since=0` re-read never fires on admit.

**Authorization (purist HIGH + ADV-16, mandatory):** reaching the 0700 operator socket is **necessary but not sufficient**. The live-admit IPC op MUST carry an **operator-key-signed authorization verified server-side, mirroring `verifyMintAuth`** (`serve.go:775`). A local process must not be able to admit a fleet member any more than it can mint scrip. A config file-watch auto-reload is explicitly **rejected** as the primary path (raw config write-access must not equal fleet+relay admission); the signed IPC op is the admit channel, config persistence is only its durable backing.

---

## 4. Operator robustness — folds 347/7b2/61a into `up`/serve so it "just works"

All four become **properties of `up`**, not tribal knowledge. (726 — missing `relay.WithoutClientAuth` — already FIXED.)

**347 (relay-attach hangs startup before the IPC socket binds).** Root cause: `runServeLocal` builds the entire relay leg — `seedEmittedFromStore` (`ls.ReadAll()` + per-record `signedEventID`), `guardOperatorKeyMigration` (second ReadAll), blocking initial REQ Send — **inside the relay loop, which runs BEFORE `runEngineLoop` binds the socket** (`serve.go:442`/`529`). **Fix (structural reorder, ~60-100 LOC, no new design):** extract socket-bind + `serveOperatorSocket` into a function callable **before** the relay-attach loop. Bind socket + start engine first; **attach relay legs in async retry goroutines**. Serve is "ready" (local `status`/`accept-put`/`mint` work) the instant the socket binds; a slow/dead relay never blocks local operability.

**7b2 (long DG_HOME → socket bind fails as non-fatal WARN → half-broken operator).** Root cause: no path-length guard before `net.Listen("unix", …)`, and bind failure demoted to WARN — inconsistent with every other fail-loud invariant in the file. **Fix (~40-70 LOC):** anchor the socket under a short `$XDG_RUNTIME_DIR/dontguess-<hash-of-DGHOME>.sock` (mirrors ssh-agent/docker's `sun_path`-limit workaround), fall back to the DG_HOME path only when short enough, **record the resolved socket path in config** so CLI clients find it, and make a post-relocation bind failure a **HARD startup error** — never the current silent half-broken WARN. `up` fails-closed on bind failure.

**61a (fresh operator REQs Since=0 → re-reads entire relay history + dropped_smuggled flood).** Root cause: dontguess kinds missing from **FOUR** `relay.Filter{}` constructions — `serve_relay.go:711` (initial subscribe), `watchdog.go:306` (reconnect REQ), and **`watchdog.go:470` — the PERIODIC full-resync audit's `Filter{Since:&zero}`, which re-floods on EVERY audit cycle** (a recurring cost the friction report missed). **Fix (~40-60 LOC):** a shared `nostr.DontguessKinds` var (`KindPut..KindScrip` + the new `3410`) threaded through all three sites; persist a **per-relay Intake cursor** (last-seen `created_at`, mirroring the existing Outbox `relayCursorPath` sidecar) and constrain the REQ to `Since=lastCursor` + a bounded backfill window, with a documented "pre-bootstrap entries not ingested" semantic. **Audit before closing:** `pkg/relayclient/{relayclient,buy,settle}.go` have 4+ more `relay.Filter{` sites (relayclient.go:299, buy.go:217, settle.go:253/340/672) — check them for the same gap.

---

## 5. Federation trust model — CONDITIONAL. Honest per §541.

**Status: NOT decision-complete. The `federate` verb ships only after ADV-9/10/13 close on paper.** federation.md and federation-modes.md are **stale** (dated 2026-03/04, pre-nostr-first) — they describe campfire beacons / `cf init` / `core-peer-establish`, infrastructure v0.7.0 does not have (serve is campfire-free). `pkg/exchange/state_federation.go` (167 LOC) is an intra-exchange hop-depth tracker, **not** cross-operator federation. Zero handlers for `federation:propose/accept/inventory-offer/match-request/match-confirm/revoke/reconcile` exist. **Federation needs an infra-assumption rewrite (campfire peering → nostr relay peering) as a prerequisite design item before any implementation estimate is meaningful.**

### 5.1 The confidentiality reconciliation — RULING: ROUTER mode default, CUSTODIAL opt-in

The conflict between §541 (an operator that **re-sells** another's entry MUST see plaintext = "cross-operator confidentiality = the least-trusted federated operator") and federation.md:90/284 ("delivers content directly to the buyer, encrypted to the buyer's key") is a **MODE distinction**, decided per-entry by seller consent at put time:

- **ROUTER mode (default, `resell: federation`).** Peer B shares/receives **only metadata** (description, teaser, token_cost, domains, embedding — never the CEK). On a cross-operator match, B forwards the buyer's **funded** reservation to home operator A; **A re-wraps the CEK directly to the buyer** (`wrapped_cek_buyer = NIP-44(A_priv, buyerPub, CEK)`) and A emits the deliver. B is a discovery/matching ROUTER only and **NEVER sees plaintext**. federation.md:90 is then **literally true** and §541's plaintext trust set stays at exactly `{A}`. Requires A online at delivery (A is an always-on operator — fine; the transient-A-down failure path wires to federation.md §7 auto-refund/timeout).
- **CUSTODIAL mode (opt-in, `resell: custodial`).** A re-wraps the CEK to B so B can be the delivery pivot when A is offline / re-prices. Now confidentiality = least-trusted of `{A,B}` exactly as §541 states. **Explicit per-entry seller opt-in.** Requires a new put-time `resell: none|federation|custodial|<npubs>` field on the §541 v2 envelope, enforced at `applyPut` (the envelope schema has no resell-scope field today — **new work**).
- **`resell: none`** (also default-eligible) keeps an entry home-only.

**The default (router) buys liquidity with ZERO widening of the plaintext boundary.** This is the reconciliation §541 open-question 2 demands.

**PERMANENT CONSTRAINT to state as informed consent (ADV-10):** federating **for resale** (custodial) = granting the peer read access to your plaintext corpus. **Only federate in custodial mode with operators you would let read everything.** A sybil operator that federates and requests custodial resale of A's entire inventory pulls every entry's plaintext — strictly worse than a passive relay scraper (who gets only ciphertext). Mitigations: per-entry resale consent (above), re-wrap rate limits, and the router/custodial tier split so discovery never implies plaintext grant.

### 5.2 The two hard §541 conflicts — OPEN, must close before `federate` ships

- **ADV-9 / purist CRITICAL — content integrity.** federation.md A4's MITM defense (`content_hash` in inventory-offer + match-confirm, :75/:168/:170) is exactly the `sha256(plaintext)` that §541 §4.4 **permanently removed** from the wire (guess-confirmation + correlation oracle). §541 keeps only `ciphertext_hash`. In **router mode this works** (A delivers its own ciphertext; `ciphertext_hash` matches A's offer). In **custodial mode it breaks** (B re-encrypts with its own CEK → different ciphertext → `ciphertext_hash(B) ≠ ciphertext_hash(A)` → A's buyer cannot verify against A's offer). **Ruling:** rewrite federation.md:75/168/170 to carry `ciphertext_hash` (router). Custodial-mode cross-operator integrity must be rebuilt on a value that survives re-encryption — a **seller-signed plaintext commitment revealed only post-purchase**, or an operator-B-signed provenance chain — **before custodial federation ships.** Router-only federation can ship without this; custodial cannot.
- **Cross-operator plaintext dedup (federation.md Q2) is DEAD** and must be conceded: random per-entry CEKs mean identical plaintext produces divergent ciphertext, and §541 §4.4's keyed-HMAC fallback doesn't work across independent operator secrets. Drop it.

### 5.3 Peer discovery, join, trust, settlement, abuse

- **Discovery/addressing:** nostr beacons (rewrite of federation.md §3.1's campfire beacons) — operator publishes a signed `dontguess:exchange` beacon (operator npub, relay config, metadata). Beacons are tainted: discovery is not trust. Eclipse defense (ADV-14): **multiple independent directories + operator-pinned known-good first peers**.
- **JOIN model — RULING: bilateral invite/vouch + x402-stake, NOT open, NOT agent-WoT.** This resolves the ADV-13 contradiction (federation-modes Mode 5 "open + global liquidity" vs `trust.go`'s explicit web-of-trust rejection). **We pick (a)+(c): bilateral-only agreements + economic (x402-stake) admission for un-graduated operators, and accept bounded liquidity.** Operator-level vouching is legitimate even though agent-level WoT was rejected — operators are heavyweight, always-on, economically-staked, few; sybil is expensive. New-operator trust starts LOW (baseline 50, soft-suspend <40, auto-revoke <20 per federation.md §6). **Mode 5 open global liquidity is DEFERRED / OPEN** — do not promise it while forbidding the trust mechanisms that reach it.
- **Cross-operator liquidity + settlement:** scrip stays local (F2, no cross-operator mint). Un-graduated/untrusted operators pay per-match via **PRE-FUNDED x402 escrow, never trailing bilateral credit** (ADV-12: a defaulting operator otherwise rotates to a fresh identity before reconcile and gets content free). Bilateral credit unlocks only after graduation + a bounded exposure cap.
- **Sybil-operator trust gaming (ADV-11):** cross-operator trust must weight **ONLY signals the RECEIVING operator observes on its OWN buyers' local outcomes** — never the partner's self-reported reputation/convergence (a sybil owns its ledger + buyers and manufactures convergence via `mint`). Cross-operator fees for un-graduated operators = x402 (real money), never bilateral scrip credit.
- **Abuse/failure posture:** rogue operator → bilateral instant-revoke + behavioral overlay + router mode (rogue B never holds the CEK, cannot exfiltrate A's plaintext); sybil operators → x402 stake + low starting trust + observed-signals-only; key compromise → decrypts only that operator's own corpus (§541), router keeps CEKs home so federation does **not** widen it, custodial is the accepted seller-consented risk; eclipse → multi-relay + multi-directory + pinned first peers; free-riding → cross-op matching fees + reciprocal-ratio terms.

---

## 6. Tier transitions — RULING: ONE secp256k1 operator identity from SOLO onward

**Firm requirement (5): operator identity stays stable across the climb. Current code VIOLATES it** — solo uses opaque 16-byte `local-operator.key` (non-secp256k1); fleet mints secp256k1 `nostr-operator.key` and swaps `engineOperatorKey` on relay attach (`serve.go:214-221`, confirmed). Every solo-era operator record's `Sender` then stops matching `state.OperatorKey` → solo inventory/scrip history mis-folds post-climb (ADV-17 / purist HIGH).

**Ruling:** `up` mints the **secp256k1 nostr operator key at the FIRST solo `up`** and uses its `PubKeyHex` as `State.OperatorKey` from day one, even with no relay attached. Individual tier stays byte-identical in behavior (`ScripStore` nil, plaintext-local, no relay) but the operator **identity is already permanent**. Solo→fleet then attaches relays to the SAME pubkey: **zero operator-record re-sign, no Sender mismatch, no 347 migration hang.** Existing solo homes migrate on first `up --relay` by registering the old opaque key as a **wire-alias** of the new nostr key via the already-present `eng.State().RegisterWireAlias` — a one-time local fold pass, no relay IO. (MUST-VERIFY end-to-end: the alias correctly re-attributes historical solo operator records without breaking the `assertAdvertiseEqualsSign`/scrip-store operator gate.)

**No-plaintext-leak on climb (ADV-18, CRITICAL MUST-VERIFY before documenting `up --relay` as a climb path):** individual/solo tier stores **plaintext** content (legal at individual tier, §541 §4/§6). The relay Outbox tails the same local `events.jsonl`. **Fence Outbox egress at a climb watermark** so pre-climb plaintext puts stay **local-only** and are NEVER republished to the relay in cleartext at the instant of climbing. Re-put-as-encrypted must be a **deliberate per-entry choice**, never automatic republication. §541 §7 covers only within-team cutover, not the solo→team climb where entries were never encrypted — this fence is new. The mixed-log v-tag dispatch (plaintext solo puts + v2 team puts in one log) is documented in the scaling guide.

**FLEET = ONE operator, member machines are members (ADV-4).** `up --relay` on a second machine without the operator private key must **detect the existing operator on the relay and REFUSE to mint** a competing sequencer. Provide `dontguess operator export` / `operator import` (1Password-backed) for the rare genuine multi-host operator; the normal path is member machines run `join`, not `up --relay`.

**Decouple `encryptedRequired` from scrip (ADV-7, before any scripless rung).** Today `encryptedRequired = ScripStore!=nil && OperatorSigner!=nil` (`engine_core.go:610`) — confidentiality ANDed with payment. Any future relay-attached-but-scripless rung silently broadcasts plaintext. **Gate `encryptedRequired` on relay-attached (`OperatorSigner!=nil`) ALONE**, independent of scrip.

---

## 7. Docs plan — dual-audience, runnable, honest

Structure (RPT: the docs ARE the onboarding program — an agent executes the same page a human reads):

1. **QUICKSTART "0→exchange"** — three copy-paste blocks, one per rung, each ~one command (the transcripts in §1/§5). Written as a **narrated runnable script**. States the **hard size ceiling** (buyer-side Blossom fetch is DEFERRED, dontguess-640 — FLEET delivers only inline content ≤32 KiB today; large content fails loud until it lands, ADV-8).
2. **Agent-facing protocol (extend the CLAUDE.md dontguess block).** Add the ladder + join-token flow so a fleet agent can self-onboard an exchange, not just buy/put on one. **Correct the stale put example** (`--content <base64-result>` is the pre-§541 plaintext path — team tier encrypts by construction; plaintext puts are dropped by `applyPut` fail-closed). Add the **informed-consent block** (below).
3. **Operator runbook.** Relay bootstrap (install strfry + writePolicy pinned to operator pubkey + initial roster, §2); key custody — **1Password/HSM (§541 §4.2/§3.5) covers AT-REST/TRANSFER custody of `nostr-operator.key` ONLY** (backup, `operator export/import`, at-rest-on-disk theft); it does NOT close the in-process ECDH side-channel — the live scalar is in `serve`'s process memory whenever the operator is running and unwrapping CEKs, and only a hardware HSM performing the ECDH operation itself removes that exposure (§541 §3.5/§4.2 correction, dontguess-973 C1); boot unit + linger; live admit/revoke (§3); rotation (§541 §3.5, full runbook in §7.3 below); `operator export/import` (§6); **operator-key-leak threat model and rotation runbook: §7.3 (dontguess-973 C2).**
4. **Scaling guide solo→fleet→federation.** Tier-transition invariants (§6), what migrates vs stays, and the **confidentiality posture at each rung**: solo = local-only; fleet = single-operator-sees-plaintext (§541); federation router = still one operator sees plaintext; federation custodial = least-trusted opt-in.
5. **Doc-cascade fixes (downstream-review, mandatory):** (a) root `CLAUDE.md` is stale ("DontGuess is a campfire application… `cf join baron.dontguess`") — rewrite to nostr-first; (b) rewrite federation.md/federation-modes.md off campfire onto nostr relay peering + §541 reconciliation (§5); (c) write the §541 §8.9 consent language into federation.md and CLAUDE.md.

**Informed-consent block (permanent constraints, for BOTH agents and humans):** operator-sees-plaintext (§541); federated-operator-sees-plaintext-on-custodial-resale (ADV-10); no forward secrecy — one operator-key leak decrypts the **entire historical corpus** offline from scraped relay+Blossom data (§541 A4/P5); no content revocation once public.

### 7.3 Operator-key-leak threat model + rotation runbook (dontguess-973 C2)

Published here as the canonical operator-facing copy of §541 §4.2/§3.5. This section is REQUIRED reading before running `dontguess init`/`up --relay`, and must ship as part of the operator runbook (§7 item 3), not buried only in the design doc.

**Threat-model line (state this explicitly to every operator):**

> On operator-key leak, the entire immutable historical corpus is offline-decryptable from data already scraped off the relay and Blossom. Every `wrapped_cek_operator` ever emitted by this operator unwraps with the leaked key; every ciphertext blob referenced by those entries is already public on the relay/Blossom. There is no way to revoke this after the fact — it is a permanent property of the append-only, no-forward-secrecy design (§541 §4.2), not a bug to be patched. Rotating the key (below) protects only content put AFTER rotation; it gives zero retroactive protection for anything put before the leak.

**What 1Password/HSM custody does and does NOT buy (do not conflate these, §541 §3.5/§4.2 correction):**
- **DOES protect:** the key file at rest on disk when `serve` is not running, and in transit during `operator export`/`operator import` between hosts.
- **Does NOT protect:** the live scalar in `serve`'s process memory while the operator is running — that memory is used directly for every NIP-44 ECDH unwrap and BIP-340 signature. Software custody (1Password, encrypted file) is exactly as exposed in-process as a plain 0600 file once loaded. Only a hardware HSM that performs the ECDH itself (key never leaves the device) removes this window.
- **What dontguess ships today (dontguess-973 C3, best-effort, not a side-channel fix):** `nostr-operator.key` permissions are verified as 0600 on every load — a widened file (hand copy, permissive restore) is refused, not silently trusted; the loaded scalar is `mlock`'d against disk swap where the platform/privileges allow it (non-fatal if the host lacks `CAP_IPC_LOCK`/sufficient `RLIMIT_MEMLOCK` — dontguess still starts). This narrows the "key gets paged to disk and left in a swap file/hibernation image" sub-case of the in-process window. It does not touch live-process-memory exposure (a debugger, a core dump, a compromised co-located process with ptrace rights).

**Rotation runbook (mechanical steps — this is the "what do I actually run" companion to §541 §3.5's design-level description):**

1. **Mint the new key.** `dontguess operator export` the CURRENT key to 1Password first (so the compromised/rotating key is preserved for the re-wrap pass in step 3 — you cannot re-wrap CEKs to the new key without the old key's decrypt capability). Then generate the new identity (fresh `nostr-operator.key`, e.g. via a clean `$DG_HOME`'s `identity.Generate()` path, or `dontguess operator import` a key minted elsewhere).
2. **Re-point the relay.** Update `writePolicy`'s pinned operator pubkey and the roster event (kind `30078` d-tag=`fleet`, §2) to the new pubkey. Until this completes, the relay still accepts writes signed by the OLD key — do not delete the old key material yet.
3. **Re-wrap the CEK index.** Run the one-time local re-wrap pass (§541 §3.5): for every `InventoryEntry.WrappedCEKOperator` wrapped to the old pubkey, unwrap with the OLD (retained, decrypt-only) key and re-wrap to the NEW key. This is local state work, not a relay republish — the immutable put/wrap events already on the relay are untouched (and cannot be, per the no-forward-secrecy constraint above); only the operator's local re-wrap index changes so future `settle`/`deliver` unwraps use the new key.
4. **Retire the old key decrypt-only until step 3 completes, then destroy it.** Do not delete the old private key material before the re-wrap pass finishes — every unwrapped CEK needs it. Once re-wrap is complete and verified (spot-check a sample of entries decrypt correctly under the new key), securely delete the old key file and revoke it from 1Password/HSM storage.
5. **State what rotation does NOT do:** it does not un-leak anything already decrypted by an attacker before rotation, and it does not protect the historical corpus retroactively (see threat-model line above). Communicate this explicitly to sellers/buyers on the affected exchange — rotation is forward-only damage control, not remediation.

---

## 8. Adversary resolution table

| ID | Attack | Disposition |
|---|---|---|
| ADV-1 | Unification puts relay-admin creds on operator = merged blast radius | **RESOLVED** — reject SSH-push; operator-signed roster, relay pulls+verifies, operator holds no SSH (§2) |
| ADV-2 | Mirroring fleet→write 1:1 collapses defense-in-depth | **RESOLVED** — two gates enforce different properties; relay keeps independent owner-denylist+rate-cap (§2) |
| ADV-3 | Two allowlists drift on partial failure | **RESOLVED** — one signed event, two projections, un-desyncable (§2) |
| ADV-4 | FLEET bootstrap forks operator / mints rogue key | **RESOLVED** — `up --relay` detect-existing-operator + refuse-mint; members `join`; `operator export/import` (§6) |
| ADV-5 | `up` inherits 347/7b2/61a as silent half-states | **RESOLVED** — socket-first, XDG short path + fail-loud, bounded backfill by kinds (§4) |
| ADV-6 | systemd --user dies on logout / long-path socket | **RESOLVED** — `up` enables linger + socket in $XDG_RUNTIME_DIR (§4/§6) |
| ADV-7 | `encryptedRequired` ANDed with scrip → latent plaintext leak on new rungs | **RESOLVED** — gate on relay-attached alone (§6) |
| ADV-8 | Large content broken on FLEET (Blossom buyer-fetch deferred) | **OPEN (tracked, dontguess-640)** — land buyer-side fetch or document the ceiling in QUICKSTART (§7) |
| ADV-9 | Federation A4 content-integrity contradicts §541 (plaintext hash removed; ciphertext_hash differs across operators) | **OPEN (gates `federate`)** — router uses ciphertext_hash; custodial needs seller-signed post-purchase commitment (§5.2) |
| ADV-10 | Federation resale = full plaintext exfiltration | **PERMANENT CONSTRAINT + PARTIAL MITIGATION** — router default (peer never sees plaintext); custodial = explicit per-entry seller opt-in (§5.1) |
| ADV-11 | Sybil operator games cross-operator trust with self-minted scrip | **OPEN (gates `federate`)** — weight only receiving-operator's own local outcomes; x402 for un-graduated (§5.3) |
| ADV-12 | Cross-operator settlement default / new-operator griefing | **RESOLVED (design)** — pre-funded x402 escrow for un-graduated; bilateral credit only post-graduation+cap (§5.3) |
| ADV-13 | "Global liquidity" vs "no web-of-trust" contradiction | **RULED** — bilateral + x402-stake admission, accept bounded liquidity; Mode 5 open-global DEFERRED (§5.3) |
| ADV-14 | Eclipse via directory/beacon channel | **RESOLVED (design)** — multiple independent directories + operator-pinned first peers (§5.3) |
| ADV-15 | Bearer join-token collapses relay gate | **RESOLVED** — single-use, TTL, npub-bound-on-redeem, operator validates+signs, kind-3410 write-hole rate-limited (§1) |
| ADV-16 | Unauthenticated live-admit over IPC | **RESOLVED** — live-admit op requires operator-key-signed authz mirroring verifyMintAuth; no config file-watch auto-admit (§3) |
| ADV-17 | solo→fleet does not keep operator identity stable | **RESOLVED** — one secp256k1 identity from solo; RegisterWireAlias migration for existing homes (§6) |
| ADV-18 | solo→fleet climb can mass-broadcast solo plaintext corpus | **OPEN (MUST-VERIFY before `up --relay` climb path documented)** — fence Outbox egress at climb watermark (§6) |
| ADV-19 | `federate` one-liner hides the biggest trust decision | **RULED** — `federate` stays deliberate, per-peer, scoped, x402-escrowed, revocable (§0/§5) |

---

## 9. Phased build plan (outcome-scoped, for /swarm-plan)

**Gate A — robustness + identity foundation (unblocks FLEET one-command). MUST land before `up --relay` is documented as "just works."**
- **P0 — 61a closed:** a fresh operator start ingests only dontguess kinds from a bounded cursor; no full-history flood at startup OR on the periodic resync audit. (4 Filter sites + shared kinds var + per-relay Intake cursor; audit relayclient sites.)
- **P1 — 347 closed:** local operator commands (`status`/`accept-put`/`mint`) respond within 1s of `serve` start even when a relay is dead/slow; relay legs attach async. (Socket-bind reorder.)
- **P2 — 7b2 closed:** a long DG_HOME operator binds its IPC socket under $XDG_RUNTIME_DIR and fails LOUD (not WARN) if it cannot; CLI clients resolve the socket path from config.
- **P3 — one operator identity:** a solo `up` mints a secp256k1 operator key used as `State.OperatorKey`; climbing to `--relay` reuses it with zero re-sign; existing opaque-key homes migrate via wire-alias. (ADV-17; MUST-VERIFY alias re-attribution E2E.)
- **P4 — climb egress fence:** solo→fleet climb never republishes pre-climb plaintext puts to the relay (watermark fence). (ADV-18; MUST-VERIFY.) Decouple `encryptedRequired` from scrip (ADV-7).

**Gate B — allowlist unification + hot-reload + one-command surface (delivers SOLO + FLEET rungs).**
- **P5 — operator-signed roster event:** admitting a key is ONE operator action reflected in BOTH the exchange KeySet and the relay writePolicy, with no SSH and no desync; relay retains an independent owner denylist+rate-cap. (§2; includes out-of-repo strfry writePolicy change to subscribe+verify.)
- **P6 — live admit (no restart):** `dontguess allowlist add/remove` takes effect sub-second on a running operator, authorized by an operator-key-signed IPC op; persists for restart. (§3.)
- **P7 — `dontguess up`:** solo and `--relay` bootstrap complete, idempotent, boot-service-installed (linger), composing existing verbs. (§1; depends on P0-P6.)
- **P8 — `invite`/`join`:** one paste onboards a member end-to-end (self-provision + admit to both gates + genesis grant) via the operator-signed token + rate-limited kind-3410 redeem write-hole. (§1.)

**Gate C — FEDERATION (paper-first; do NOT dispatch code until the OPEN items close).**
- **P9 (design item) — federation infra rewrite:** federation.md/federation-modes.md re-based off campfire onto nostr relay peering; router/custodial modes + `resell:` envelope field + §541 reconciliation + ADV-9 custodial integrity rebuild + ADV-11 trust-signal ruling + x402-escrow settlement, all decision-complete on paper. **Blocks P10.**
- **P10 — `dontguess federate` (router mode only, first):** two operators form a bilateral, revocable, x402-escrowed agreement; a buyer on A matches B's inventory and A (origin) delivers with the CEK never leaving A. Custodial mode is a separate later item gated on ADV-9's post-purchase-commitment rebuild.

**Gate D — docs (runs parallel, closes the cascade).**
- **P11 — QUICKSTART + agent CLAUDE.md block + operator runbook + scaling guide**, dual-audience, with the informed-consent block; **plus** the doc-cascade fixes (stale root CLAUDE.md, federation.md rewrite, §8.9 consent language). (§7.)

**Ground-source testing (project rule 10):** no phase closes with a skipped/absent test for the interface it touches. Gate A phases each need a deterministic startup/climb test. Gate B needs an "admit reflects in both gates within 1s, no restart, no history flood" E2E. Federation router mode needs the "peer never receives the CEK; passive scrape of the shared channel yields only metadata + ciphertext" confidentiality-property test before it ships to the website.

## 10. Open questions (need a human decision)

1. **ADV-8 / dontguess-640:** ~~land buyer-side Blossom fetch vs document the ≤32 KiB ceiling?~~ **RULED (operator, 2026-07-15): FOLD dontguess-640 (buyer-side Blossom fetch) INTO the ladder (Gate B)** — FLEET delivers any-size content from day one; "works at every rung" is literally true for all content.
2. **ADV-9 custodial integrity:** which rebuild — seller-signed plaintext commitment revealed post-purchase, or operator-B-signed provenance chain? (Gates custodial federation; router unaffected.)
3. **Mode 5 open global liquidity:** confirm it stays DEFERRED (bilateral + x402-stake only for v1), or is there appetite to design the sybil economics for open federation now?
4. **`operator export/import` custody:** 1Password-backed only, or also an HSM path? (Ties to §541 §4.2 timeline.)
5. **macOS/launchd boot service:** ~~in scope, or Linux-only v1?~~ **RULED (operator, 2026-07-15): CROSS-PLATFORM — `up` supports systemd --user + linger AND macOS launchd from day one.**
6. **Roster event kind + writePolicy mechanism:** confirm `kind 30078` d-tag=`fleet` and that per-write operator-signature verification inside `writePolicy.py` (subscribe+cache, not per-write round-trip) is cheap enough against strfry's plugin API.

<!-- adversarial-design (Workflow, structured-return): adversary+creative(opus)+systems-pragmatist(sonnet)+domain-purist(opus)+architect(opus); 2026-07-15 -->
<!-- grounds dontguess-c06/113/347/7b2/61a; supersedes stale federation.md/federation-modes.md per §541 -->
