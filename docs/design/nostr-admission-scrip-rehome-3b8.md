# dontguess-3b8 — Nostr-first Admission Architecture: re-home seller allowlist + reputation floor, wire scrip, close the ed5 key race

**Status:** DESIGN — awaiting operator approval before swarm dispatch.
**Item:** dontguess-3b8 (P1 SECURITY, release blocker) + folds in dontguess-ed5 (P2).
**Method:** adversarial-design via the Workflow tool (campfire skill can't run in the de-campfired repo) — 6 code-mappers, 4 dispositions (adversary/creative/systems-pragmatist/domain-purist), architect synthesis, then 3 independent security red-teamers. Every load-bearing claim below was **verified against code** by the orchestrator, not taken on the agents' word.
**Source of truth respected:** convergence-sybil-defense.md (RATIFIED 2026-07-06), relay-transport.md §2.4a/§2.5a, nostr-first-rebuild-decision.md (tier model).

---

## 1. Problem

The b14 de-campfire cutover promoted `runServe` — the **individual-tier** path (no trust, no scrip) — to the **default** `dontguess serve`. On an attached relay this is fail-open on seller admission and does zero payment accounting. Three gaps, plus one the red-team surfaced:

1. **Fail-open put admission.** §2.4a Intake (`pkg/relay/intake.go`) admits any validly-**signed** non-operator `put(3401)`/`buy(3402)` on signature alone — `VerifyOperatorAuthorship` returns nil for non-operator kinds (`verify.go:174`). The engine trust gate (`engine_core.go:864`) is nil in this path (fail-open). Old campfire default gated via fleet membership; the re-home onto an npub allowlist is absent. Bounded today only by the strfry write-allowlist (3 keys).

2. **The dispatch gate alone does NOT close it — the auto-accept promotion is the real hole (VERIFIED).** Even wiring `TrustChecker` at `engine_core.go:864` leaves the exploit open: the poll loop's fold pass (`engine_core.go:781`) runs `state.Apply(put)` on every relay put **before** the dispatch gate, staging it in `State.PendingPuts`; the auto-accept ticker (`serveAutoAccept` default true → `RunAutoAccept` → `autoAcceptPutLocked`, engine_pricing.go:222) then emits an operator-signed **put-accept** + `matchIndex.Add` **without ever calling `TrustChecker.Check`** — it only reads `.Level()` for provenance (engine_pricing.go:76-78). *Confirmed: `autoAcceptPutLocked` emits `sendOperatorMessage` at line 64, the `Level` read is at 76, and there is no `.Check()` anywhere in the promotion path.* So a non-allowlisted put becomes live, operator-blessed, matchable inventory.

3. **No scrip accounting.** `ScripStore=nil` → content moves for free; `NewLocalScripStore` (double-spend guards built) is unused by any production caller.

4. **Wiring scrip activates a dormant CRITICAL mint (VERIFIED — red-team "broken" verdict).** `State.matchToBuyHold[match]` is **never retired on settle** (grep: create/write/read sites, zero deletes). `performScripSettlement` guards double-settle via `ConsumeReservation` "missing → already settled" (engine_settle.go:230), **but** `restoreExistingHold` (engine_settle.go:558) *re-saves* the consumed reservation with **no re-charge** when a replayed buyer-accept finds the stale `matchToBuyHold` entry — defeating the guard. A repeated buyer-accept→complete (new msg IDs; the `completedSettlements` dedup is keyed on complete msg.ID and only guards reputation, state_settle.go:246) re-credits seller+operator every loop. **Unbounded self-mint if buyer==seller.** Dormant only because `ScripStore=nil` today; 3b8 turns it on.

5. **ed5 (folded in): non-atomic operator-key create.** `exchange/init.go:170` and `serve.go:292` both do `ReadFile→IsNotExist→Generate→WriteFile` with no O_EXCL. Concurrent first-run init+serve mint mismatched advertised-vs-signing keys → orphans any relay admit **and** silently zeroes the scrip ledger (`relay_store.go:239` drops scrip messages whose `Sender != OperatorKey`).

---

## 2. Firm operator decision (2026-07-09, not re-litigated)

Re-home the **full dontguess-side trust model** — a flat operator-maintained **npub seller allowlist + reputation floor**, enforced by dontguess itself, **not** delegated to the relay write model. Wire `scrip.NewLocalScripStore` into the default serve **when relays are attached**. Fix the ed5 race. **NOT** relay-allowlist-as-gate; **NOT** web-of-trust (CAG is ratified-rejected).

---

## 3. Architecture — one TrustChecker, tier-gated, enforced at four engine seams

The allowlist + reputation floor live in **one** `*exchange.TrustChecker` built from `Config.FleetAllowlist` (validated by `identity.NewAllowlist`, materialized as a mutable `exchange.KeySet` so runtime `Remove` works) + `WithReputationFloor(State.SellerReputation, Config.MinReputation)`. Constructed **only** inside the existing `len(relayURLs) > 0` branch (`serve.go:160-166`), so **tier-awareness falls out of the existing branch** — individual/no-relay keeps `TrustChecker=nil`/`ScripStore=nil` (correct: operator is the sole local writer; local puts use random per-call sender keys that would *brick* under a non-nil allowlist). **Nothing is added at pre-persist Intake** (respects the ratified ADV-17 deferral — a pre-Apply reorder needs property tests that don't exist and risks the b84/-90d fold-cursor invariants).

| Seam | Location | Role | Status |
|------|----------|------|--------|
| **A — promotion gate (load-bearing)** | `autoAcceptPutLocked` (engine_pricing.go:222), **before** `sendOperatorMessage`/`matchIndex.Add` | `TrustChecker.Check(sellerKey, OperationPut, "")`; non-admitted → `RejectPut` + `dropped_unlisted`/`dropped_low_reputation` counter + LOUD alarm, never indexed | **new** |
| **B — dispatch gate** | `engine_core.go:864` | make non-nil; gates buy(anon by design)/settle phases/match/mint/burn/consume/assign-family via existing `tagToTrustOp` | **wire existing** |
| **C — runtime de-allowlist visibility** | `engine_buy.go:400` `findCandidates` (already withholds `NeedsRevalidation`) | on `allowlist remove`, flag the removed seller's already-accepted entries `NeedsRevalidation` so they stop being served | **new (small)** |
| **D — reload re-gate (red-team blocking)** | `rebuildMatchIndex` (engine_index.go:9) | *Verified:* rebuild re-indexes `state.Inventory()` with **zero** trust filter, and `NeedsRevalidation`/`AcceptedProvenanceLevel` are in-memory-only (reset to zero on Replay). Without a reload re-gate, de-allowlisting is **erased by any restart**. Re-gate each entry's `SellerKey` against the live allowlist at rebuild; skip/flag non-admitted. | **new (red-team)** |

**Why A is the choke, not B:** the fold loop already applied the pending put to `State` before any gate ran, and `RunAutoAccept` promotes it with no trust check. Gating the *promotion* is the only place a poison put's path into matchable inventory is severed. B/C/D are defense-in-depth around it.

**Reputation floor is demotion-only, disabled by default.** `DefaultReputation=50`; a floor ≤50 is a no-op vs fresh Sybils, a floor >50 is a cold-start deadlock (new sellers can't earn reputation without first selling). And `Reputation()` only *decrements* via small-content refunds — it can **never** gate a large-content poisoner. So **the flat allowlist is the sole anti-poisoning primitive; the floor is secondary rate-limiting.** This matches the ratified doctrine exactly: *"Team/Enterprise tier — solved… NIP-42 allowlisted identities make independence real. Every global-tier mechanism is a no-op here."* Default `MinReputation=0` (disabled); **clamp/reject any value >50 at config load** (red-team: the entire useful range is empty and >50 hard-bricks onboarding).

---

## 4. Scrip wiring + the two blocking money-integrity fixes

Construct `scrip.NewLocalScripStore(localStore, engineOperatorKey)` inside the `len(relayURLs)>0` branch (both args already in scope) and set `EngineOptions.ScripStore`. Individual tier keeps `ScripStore=nil`. Every consumer is nil-guarded → additive. **But two money bugs must be fixed as part of enabling it, or 3b8 ships an exploit:**

- **FIX-M1 (CRITICAL — the double-settle mint, §1.4).** Retire `matchToBuyHold[match]` (+ `matchToBuyHoldAmount`) on settle, **and** add a **durable settled-match set** (keyed on `matchMsgID`, rebuilt on Replay) that gates **both** `restoreExistingHold` (never re-hydrate a settled match) and `performScripSettlement` (never re-consume). Proof test: `accept→complete→accept→complete` emits exactly **one** scrip-settle; total supply conserved.
- **FIX-M2 (MAJOR — hold-path mutate-then-emit mint).** `decAndSaveHold` decrements the buyer balance (engine_settle.go:600) then emits `scrip-buy-hold` best-effort (warning on failure). On emit failure the debit is live but no durable record → replay loses the debit → net mint. The *settle* path is already hardened (emit-durable-then-mutate, engine_settle.go:397/419); **mirror it on the hold path** (emit durable before `DecrementBudget`, or treat emit failure as a hard rollback).
- **Ledger-conservation invariant test:** `TotalSupply == Σ balances + TotalBurned`, exercised across replay and the accept/complete/dispute flows (would have caught M1 and M2 directly).

**Bootstrap funding (required — else buys deadlock).** A fresh `LocalScripStore` folds zero balances and **no scrip-mint emitter exists**, so the first `settle(buyer-accept)` fails `ErrBudgetExceeded`. Ship an operator-only `dontguess mint <npub> <amount>` that emits a `scrip-mint` via `sendOperatorMessage` (folds through `applyMint`, passes the `:239` operator-gate) as the team-tier genesis path. Thereafter labor income (assign-pay) + residuals recirculate. An unfunded buy then fails **LOUD** (correct) instead of silently moving content for free (today's nil). `dontguess mint` is an operator god-button — audit-logged, clearly operator-only; x402 is the eventual external real-money rail.

**Hard prerequisite:** ed5 must land **before** ScripStore is enabled, and the advertise==sign key equality must be a **startup hard error** (not a warning) before `LocalScripStore` is constructed — a key mismatch silently rebuilds every balance to zero (`:239`) and DoS's all buys.

---

## 5. ed5 fix (revised per red-team — bare O_EXCL is NOT race-free)

The naive "O_EXCL create then write" has a **torn-read window**: `O_EXCL` creates a zero-length file, then the hex is written in a *second* syscall. A concurrent EEXIST loser reads the empty/partial file and `FromPrivHex`es it — a truncated valid-length prefix would be a *different valid key*, i.e. the exact advertise≠sign split ed5 exists to kill.

**Correct fix:** one shared helper in `pkg/identity` (new `keyfile.go`):
- **Winner:** write to a temp file → `fsync` → **atomic rename** onto the final name (file is never observably present-but-empty).
- **Loser (EEXIST / lost the rename):** **bounded retry-read** until a parseable 64-hex key appears (tolerates the not-yet-populated window), then `FromPrivHex`.
- Deployed **raw-hex** format (do NOT reuse `identity.Save`'s JSON/last-writer-wins — wrong format, would break deployed keys).
- Two entry points: `loadOrCreatePrivHexKey(path)` (secp256k1, replaces `init.go:170` + `serve.go:292`) and `loadOrCreateRawKey(path, genFn)` (opaque 16-byte, replaces `serve.go:264`). `hitrate.go:201/207` inherit the fix.
- **advertise==sign assertion** in the relays-attached branch: if `Config.OperatorKeyHex != relaySigner.PubKeyHex()`, **fail closed LOUD** (hard error before ScripStore construction). Do **not** silently auto-reconcile to the signer — for an already-admitted relay the config key may be the authoritative one; detect + alarm, let the operator resolve.

---

## 6. Config surface & fail-mode (per tier)

New `exchange.Config` fields (`init.go`): `FleetAllowlist []string` (`json:"fleet_allowlist,omitempty"`) — the flat operator-maintained seller npub set (no vouching/transitive edges). Existing-but-dead `TrustLevels`/`MinReputation` become consumed. New `dontguess allowlist add|remove|list <npub>` mutates the persisted config (`LoadConfig`/`writeConfig`, `NewAllowlist` validation); `remove` also drives runtime `KeySet.Remove` + Seam C/D. `serve` calls `LoadConfig` best-effort (absent config not fatal).

| Tier | Trigger | Posture |
|------|---------|---------|
| **Individual** | no relays | `TrustChecker=nil`, `ScripStore=nil` — fail-open is **correct**, byte-for-byte unchanged (operator's own local puts, random sender keys, charges nobody) |
| **Team / Federated** (DEFAULT when relays attached) | `len(relayURLs)>0` | **FAIL-CLOSED**: `TrustChecker` non-nil from `FleetAllowlist`; an **empty** allowlist admits only the operator key (operator → `TrustOperator` before `members.Allowed`) — the exchange still serves the operator's own inventory and admits external sellers as npubs are added. `ScripStore` non-nil (payment enforced). `MinReputation` default **40** (demotion-only, clamp ≤50 — see §8 D3). |
| **Global / permissionless** | — | **OUT OF SCOPE.** No `open` config key ships (§7, D4). Built later, with its own Sybil machinery. |

---

## 7. Scope cuts (YAGNI / footgun)

- **`enforcement_mode=open` — CUT from 3b8 (operator decision D4).** All three red-teamers flagged it: open mode admits every author, the reputation floor is a no-op, and the ratified global Sybil machinery (5000-sat burn floor, `K_eff` correlation discount, clique-recurrence, PAC) is **explicitly YAGNI/unbuilt**. Shipping the config key — even defaulting closed — is "today's cache-poisoning hole behind a flag": one flip silently restores it. **The `open` config key is not built.** No `EnforcementMode` field ships; global tier is built later, *with* its machinery. This also sidesteps the boolean `3+` convergence bonus (state_types.go:576) that would violate the ratified weighted-convergence rule at global tier.
- **Burn floor / weighted convergence / clique-recurrence — global-tier YAGNI**, per ratified doctrine. Not built here.
- **Allowlist-graduation-by-reputation — out of scope** (cuts against the ratified team-tier binary posture).

---

## 8. Operator decisions (RESOLVED 2026-07-10)

- **D1 — Anonymous-buy demand-signal skew → INCLUDE NOW (release-blocking, item 7).** `OperationBuy=TrustAnonymous` (ratified) buys fold into fast/medium-loop demand/pricing/ranking **before** settlement; `ScripStore` bounds the *money* but not the *signal* (a zero-scrip Sybil can steer price/rank for free). Fix in-scope: a per-npub buy rate-limit **or** minimum-scrip-balance-to-buy applied **before** the buy contributes to demand/pricing folding. This closes the ranking-gaming lever before any release. Implemented as `EngineOptions.MinBuyBalance` in `handleBuy` (`engine_buy.go`), team-tier only (`ScripStore != nil`); an under-funded buy is dropped from matching/demand/pricing and counted `DroppedUnderfundedBuy`.
  - **AMENDMENT (67e0 ruling, dontguess-4f01 — 2026-07-17).** A D1-dropped unfunded miss is no longer *fully* dropped: it is **REGISTERED AS A DEMAND-ONLY SIGNAL** so `dontguess demand` still sees the unmet demand. This does **not** reopen the free-Sybil ranking lever — the demand-only registration moves **NO scrip**, opens **NO funded `BuyMissOffer`**, and **NEVER folds into matching/ranking/pricing** (the load-bearing D1 invariant is unchanged; `applyMatch` routes the `TagDemandOnly` marker to `applyDemandOnly`, which touches only dedup/cap bookkeeping — never `matchToBuyer`/`matchToEntry`/`matchToResults`/price/demand). The signal is **DEDUPED by `task_hash`** (N identical misses — same OR different Sybil identities — collapse to ONE demand entry) and **CAPPED per unfunded sender per rolling window** (`DemandOnlyPerSenderCap`/`DemandOnlyPerSenderWindow`, plus a global backstop `DemandOnlyGlobalCap`) so volume-flooding is bounded. Emitted as a buy-miss message tagged `[exchange:buy-miss, exchange:match, exchange:demand-only]` with `offered_price_rate: 0`; surfaced by `demand.BuildBacklog` alongside funded misses. Skips are loudly counted (`DemandOnlyRegistered`/`DemandOnlyDeduped`/`DemandOnlyCapped`). The **funded** zero-match miss path (`handleBuyMiss`, sibling dontguess-909) is unaffected — it still opens a real 70%-rate `BuyMissOffer`.
  - **AMENDMENT (cache-warming pivot, operator ruling Baron — 2026-07-17).** The D1 bound is **OFF by default in the deployed fleet.** `serve` now reads it from `--min-buy-balance` (`serveMinBuyBalance`), **default 0 = disabled** (was hard-set to `DefaultMinBuyBalance=1` on the team tier). Rationale: D1 defends against *anonymous* zero-scrip Sybils, a threat that only exists at the OPEN/public/federation tier — **not** in an operator-**allowlisted** fleet, where admission (the allowlist) is the anti-poisoning primitive and every key is vouched-for. With the bound off, cold scrip-poor buys fold into matching and (on a miss) form operator-underwritten `BuyMissOffer` bounties — the mechanism that **warms the cache** from cold (an all-unfunded cold economy would otherwise deadlock: every buy dropped → no bounty ever forms). This also dormant-izes the demand-only Sybil machinery (dontguess-4f01/fd3/4e3), which only fires on the D1-drop path. **The bound is not deleted — it is tier-gated:** `DefaultMinBuyBalance=1` remains as the "armed" value, and a federation/public operator re-arms via `--min-buy-balance 1` when anonymous keys reappear. The demand-only amendment above still applies whenever the bound is armed. (Shipped v0.8.7.)
- **D2 — Durable-log residual → ACCEPT (bounded, compaction-reclaimed).** A rejected poison put still `BatchAppend`s to LocalStore (`Origin=relay`, intake.go:150) but Seam A guarantees it's never promoted/matched; it's inert and reclaimed by compaction. No relay-side NIP-42 requirement added for team tier.
- **D3 — Reputation floor → NON-ZERO demotion floor, default `MinReputation=40`, clamp ≤50.** Blocks sellers whose reputation dropped below 40 via small-content refunds (50 − 4×3 = 38); new sellers at `DefaultReputation=50` are unaffected (50 > 40, no onboarding brick). Documented as demotion-only rate-limiting — the flat allowlist remains the anti-poisoning primitive. Any config value >50 is rejected/clamped at load.
- **D4 — `enforcement_mode=open` → CUT.** No `EnforcementMode` config field ships (§7).

---

## 9. Work-item tree (revised — folds in red-team blockers)

Sequenced by dependency. Model tiers: **opus** for security-critical logic + reviews; **sonnet** for mechanical wiring/tests; no Fable (implementation is not design; and security).

1. **ed5 atomic key-create** *(implementer→sonnet; security-review→opus)* — shared `pkg/identity/keyfile.go` (temp+fsync+rename winner, bounded-retry-read loser, raw-hex); swap `init.go:170`, `serve.go:292`, `serve.go:264`; advertise==sign hard-error assertion. **Test:** `-race` two-goroutine concurrent-create returns one identical key; loser never reads a torn file. *No dependencies — ships first.*
2. **Allowlist config surface + CLI** *(implementer→sonnet)* — `Config.FleetAllowlist`, `LoadConfig`/`writeConfig` round-trip, `dontguess allowlist add|remove|list` with `NewAllowlist` validation (malformed npub rejected loudly); `MinReputation` default **40**, clamp/reject >50 at load. **No `EnforcementMode` field** (D4). **Test:** add/list/remove round-trips on disk; malformed rejected; MinReputation>50 rejected. *No deps.*
3. **Seam A + B + C + D admission** *(security→opus)* — depends on #2. `TrustChecker` wired in the relays-attached branch; `Check` in `autoAcceptPutLocked` before promotion; dispatch gate non-nil; runtime de-allowlist visibility (C); **reload re-gate in `rebuildMatchIndex` (D)**. Distinct `dropped_unlisted`/`dropped_low_reputation` counters + alarms. **Test:** serve-level integration driving `runServeLocal`+relay leg+auto-accept: non-allowlisted put never matchable (dropped_unlisted); allowlisted put matchable; below-floor rejected; **de-allowlisted seller withheld and stays withheld across restart**; individual path byte-for-byte unchanged.
4. **Scrip money-integrity fixes (FIX-M1 + FIX-M2)** *(security→opus)* — depends on #1. Retire `matchToBuyHold` on settle + durable settled-match set gating `restoreExistingHold` + `performScripSettlement`; hold-path emit-durable-then-mutate; ledger-conservation invariant. **Test:** `accept→complete→accept→complete` ⇒ exactly one scrip-settle, supply conserved; hold-emit-failure does not mint.
5. **Scrip wiring + mint bootstrap** *(security→opus)* — depends on #1, #3, #4. `NewLocalScripStore` in relays-attached branch; `dontguess mint <npub> <amount>` operator-only, audit-logged. **Test:** minted buyer completes a paid buy (hold→settle: seller residual + operator revenue + fee burn); unfunded buyer fails LOUD `ErrBudgetExceeded`; individual tier `ScripStore=nil`.
6. **Release-gate proof** *(security→opus; reviewer→opus)* — depends on #3, #4, #5. Adversarial e2e: non-allowlisted keypair writing a signed put to the relay leg is provably never served, through the full serve stack; `security-review` + `/sweep` on the diff; **the 32,209-LOC exchange value suite passes UNCHANGED** (acceptance gate); `go test -race ./...` green.
7. **Anonymous-buy signal bound (D1 — release-blocking)** *(security→opus)* — depends on #5 (min-balance-to-buy needs ScripStore). Per-npub buy rate-limit **or** minimum-scrip-balance-to-buy enforced **before** the buy contributes to fast/medium-loop demand/pricing folding. **Test:** a zero-balance/over-rate npub's buy does not move price/rank for a chosen entry; a funded, within-rate buy does.

**Acceptance gate:** the 32K value suite passes unchanged; full `-race` suite green; the poison-injection e2e closed; supply-conservation invariant holds; anon-buy signal bound proven. Do **not** push main or cut a release until 3b8 and dontguess-ed2 both land.
