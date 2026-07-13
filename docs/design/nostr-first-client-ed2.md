<!-- Produced by the adversarial-design workflow (rd dontguess-ed2): architect synthesis → 3 security red-teamers → architect finalization. Every correctness-gating claim is read from source (file:line cited). Do not edit conclusions without re-deriving from code. -->

# dontguess-ed2 — Nostr-First Client (put / buy / settle) — FINAL

Status: FINAL (hardened after 3-way security red-team; all 9 findings dispositioned in §10)
Author: Architect (synthesis + red-team finalization)
Source of truth: `docs/convention/`, project CLAUDE.md, `docs/design/relay-transport.md` §0 (locked invariants), `docs/design/nostr-admission-scrip-rehome-3b8.md`, `docs/design/nostr-first-rebuild-decision.md`
Verification discipline: this repo has been bitten by unverified claims. Every load-bearing claim below cites `file:line` read from source, not a disposition assertion.

---

## 1. Problem

Campfire was removed portfolio-wide. The operator side (relay transport, intake, outbox, sequencer, admission/scrip gates) landed in dontguess-3b8, but the **client** put/buy/match/settle was deleted with campfire and never replaced. Today there is no Go CLI verb for put/buy/settle (verified: zero cobra registrations); `site/install.sh` still downloads `cf` and routes the hot path through it (verified install.sh:6-7,16,76-87,199). ed2 ships a campfire-free client that publishes a put, publishes a buy, awaits a match, and settles to receive content — across the two live tiers (individual, team) — preserving every locked invariant.

### Locked invariants (relay-transport.md §0) — inviolate
- Relay reads are async cache-warming, NEVER on the buy/match hot path; the operator is the **sole authoritative sequencer**.
- Loud degradation on every failure; never a silent nil.
- Embeddings never go on the wire (adapter.go:56-58 confirms the adapter emits no vector).
- Big content travels as a Blossom pointer + verify-on-fetch, never unbounded inline.
- `pkg/store` single-writer contract: exactly one OS process owns `events.jsonl` (verified store.go:95-105 — no cross-process lock; a second appender is out of scope).

### Acceptance constraints
- The existing ~32K exchange suite passes **UNCHANGED**.
- Individual tier stays **byte-for-byte** (ScripStore==nil / TrustChecker==nil path unchanged).
- No cf/campfire runtime dependency reintroduced anywhere.

---

## 2. Verified facts the design is built on

| Claim | Verdict | Evidence |
|---|---|---|
| `store.Store` is single-writer, no cross-process lock | CONFIRMED | store.go:95-105 |
| Underfunded buy (D1) returns `nil` with NOTHING on the wire | CONFIRMED | engine_buy.go silent-drop after `DroppedUnderfundedBuy.Add(1)` |
| Buy-miss rides kind **3403** with `exchange:buy-miss`, antecedents=[buyID] | CONFIRMED | engine_buy.go match-emit path |
| Real match is kind 3403, antecedents=[buyID] → e-tag[0] reply | CONFIRMED | engine_buy.go match-emit; adapter.go:76-82 |
| `dropped_unlisted` emits a durable, wire-visible settle(put-reject) w/ `reason` | CONFIRMED | engine_pricing.go rejectPutLocked |
| Buyer-accept insufficient balance returns a **bare `ErrBudgetExceeded`**, no wire msg | CONFIRMED | engine_settle.go:624-627 |
| That error only reaches dispatch, which **logs+drops** it | CONFIRMED | engine_core.go:856-858 |
| **`buyerAcceptToMatch[msg.ID]=matchMsgID` is set UNCONDITIONALLY in the fold**, never gated on hold success | CONFIRMED | state_settle.go:149 |
| **`emitDeliverContent` gates only on operator authorship + antecedent chain — never on a live reservation** | CONFIRMED | engine_settle.go:805-838, :862 |
| `settle(complete)` with no reservation → `return nil`, no scrip moves | CONFIRMED | engine_settle.go:114-118 |
| Every buyer-side settle phase enforces `Sender==expectedBuyer`, silent-drop on mismatch | CONFIRMED | state_settle.go applySettleBuyerAccept/Reject |
| **`Outbox.Notify()` has ZERO production callers**; engine holds no outbox ref (decoupled via store) | CONFIRMED | grep `.Notify()` = 0 non-test hits; outbox.go:201-209 |
| Outbox publish interval hardwired to **5s** at the serve call site | CONFIRMED | serve.go:308 `5*time.Second`; outbox.go:383-388 default 5s |
| Engine fold poll ticks every **500ms** | CONFIRMED | serve.go:76 |
| **NIP-42 `ClientAuthenticate` does bare `ReadMessage()` with NO deadline / ctx** | CONFIRMED | nip42_handshake.go:29,50 |
| An allowlist-gated strfry relay never pushes an AUTH challenge → default handshake blocks forever | CONFIRMED (doc) | conn.go:97-115 (`WithoutClientAuth` rationale) + MEMORY reference_relay_infra |
| `relay.Conn.Recv` reconnects transparently but **NEVER replays the REQ** (owns no subscription state) | CONFIRMED | conn.go:210-215 |
| Settle-chain events e-tag the **immediate antecedent**, not buyID (deliver e-tags the buyer-accept id) | CONFIRMED | state_settle.go applySettleDeliver (antecedent = buyer-accept id); adapter.go:76-82 |
| `assertAdvertiseEqualsSign` returns **nil for empty config** → a fresh team-box serve starts as a valid operator | CONFIRMED | serve.go:672-681 (`configOperatorKeyHex != "" &&` guard) |
| Wrapper auto-starts `serve` **unconditionally** (no relay-URL gating) | CONFIRMED | install.sh:272-300 |
| A relay-attached serve mints its OWN nostr operator key + attaches Intake/Outbox | CONFIRMED | serve.go:161-167 |
| `go test ./...` caches; `test/` does not import `cmd/dontguess` (package main), so client-only regressions serve a cached PASS | CONFIRMED (empirical, RT-C) | `go list -deps ./test/ | grep cmd/dontguess` = 0 |
| Package-main in-process test precedent exists (drives cobra RunE, invalidates cache on source change) | CONFIRMED | serve_relay_test.go, serve_poison_injection_e2e_test.go, +18 package-main `_test.go` |
| `appendLocalRecord` (engine_core.go:1101) is the single egress point for operator records → the natural Notify hook site | CONFIRMED | engine_core.go:1053-1101 |

The load-bearing hazards the red-team exposed and this final design closes: **(H1)** match publication lags up to the 5s outbox tick because Notify is unwired — the client can time out on a real hit; **(H2)** content delivery is not gated on a live scrip reservation — an underfunded buyer can pull content free; **(H3)** the single `#e:[buyID]` filter is structurally blind to the settle chain and the new reject; **(H4)** the NIP-42 handshake can hang past any client timeout; **(H5)** a reconnect mid-await lands on a subscription-less socket and silently drops the match; **(H6)** the team-tier wrapper auto-starts a rogue competing sequencer; **(H7)** the test-cache gap lets client regressions ship green.

---

## 3. Ruled client architecture — one interface, tier selects transport

Every tier and verb is one logical operation:

```
sign(agentKey)  →  submit(transport)  →  await(per-phase predicate, bounded ctx)
```

- `sign` — `nostr.ToNostrEvent(msg)` → `identity.SignEvent(agentSigner, ev)` → `relay.EncodeEvent(ev)`, using the AGENT key from `AGENT_CF_HOME` (never the operator key). Verified reusable chain.
- `submit` — the only tier variable: **RelayTransport** (team) or **SocketTransport** (individual).
- `await` — a bounded, client-side, **re-subscribing** predicate wait. Never blocks the operator.

Client logic lives in a new importable package **`pkg/relayclient`**, exposed as cobra subcommands (`put`, `buy`; settle phases are driven internally by `buy` on a hit). Dual surface is deliberate: the wrapper execs the binary subcommands (DQ6), and DQ7 layer-1 imports the package in-process (cache-safe).

### 3.1 DQ1 — Publish leg (RULED)

- **Reuse `relay.Conn`**, constructed `relay.New(url, agentSigner, WithBackoff(bounded), <auth-mode>)`.
- **Auth mode (H4, RT-A#2):** the team-tier client dials **`WithoutClientAuth()` by DEFAULT** (operator gate G-relay). Grounded: the production strfry relays gate writes by *signed-event author* against a hot-reload allowlist and never push an AUTH challenge (conn.go:97-115 + MEMORY reference_relay_infra); the operator's Seam-A admission independently verifies the agent signature (3b8). NIP-42 client AUTH against such a relay blocks forever on `ReadMessage` (verified nip42_handshake.go:29 has no deadline). A `--relay-auth` flag opts into the NIP-42 handshake for relays that *do* challenge. **In either mode every post-dial read is deadline-bounded**: the client runs the connect+handshake under a watchdog that `Close()`s the conn on ctx expiry, so a socket that accepts then stalls fails LOUD inside the timeout rather than hanging. (Acceptance gate §7.8.)
- **Bounded backoff** (small MaxAttempts), NOT DefaultBackoff. A one-shot CLI against a dead/misconfigured relay fails fast and LOUD.
- **Do NOT reuse `demuxPublisher`** (operator-coupled: waiter fed only by `routeOK` inside the operator `runReader`). The client uses a single-goroutine send-EVENT → Recv-loop+`ParseFrame` → match OK-by-event-id, bounded by `context.WithTimeout`.
- **A relay `OK` is a transport receipt ONLY** — it means the relay stored the event, NOT that the operator admitted the put or will match the buy. The client MUST NOT report success on OK. (A `dropped_unlisted` put gets a clean OK then is silently withheld — engine_pricing.go.)

### 3.2 DQ2 — Buy-await protocol (RULED — the hard one)

Sequence on ONE authenticated Conn:

1. `dial (+ bounded, deadline-guarded handshake per §3.1)`.
2. **SUBSCRIBE FIRST** with a filter set the client will *extend per phase*: initial `REQ(subID, kinds:[3403,3404], #e:[<buyID-precomputed>])`. The buy event ID is the deterministic signed-event ID, computable before publish, so the filter is ready. Subscribe-before-publish is **mandatory** to beat the operator's fold poll (500ms) and the outbox publish (see H1).
3. **PUBLISH** the buy(3402) EVENT; await its OK (transport receipt only).
4. **AWAIT** with a bounded ctx, **re-subscribing on any drop** (H5, RT-A#3): if `Recv` returns an error wrapping `ErrConnDropped`, the client re-issues its current REQ (same `#e` filter set, with a `since` covering buyID) BEFORE the next `Recv`, within the remaining budget. strfry stored the match, so a fresh `#e:[buyID]` REQ after reconnect recovers it. This mirrors the operator's own resubscribe-before-read discipline (serve_relay.go:349-356); the client must not delegate this to "reuse relay.Conn."
   - **kind 3403 without `exchange:buy-miss`** → real match. Parse results; if within budget, proceed into the settle happy path (§3.5) and deliver content automatically.
   - **kind 3403 WITH `exchange:buy-miss`** → genuine miss. Print the demand-signal guide ("nobody has this — compute it and `dontguess put` to earn the residual"). LOUD, correctly attributed, not an error.
   - **assign(3405) e-tagging the buy** → BrokeredMatchMode leaked (out of scope, gate G1). Surface LOUD, do not hang.
   - **timeout** → report **AMBIGUOUS** (§5.4), NEVER "no cache exists."

**Timeout floor (H1, RT-A#1 — corrected).** The draft's floor omitted the outbox tick. The honest floor is:
`pollInterval (500ms) + outboxPublishInterval + relayRTT + publishAwaitOK`.
This design **wires `Outbox.Notify()`** (see §3.8) so the outbox term collapses to near-zero on the match path; belt-and-suspenders, the client default timeout is **10s** (was ~5s) with headroom, configurable via flag/env, and the client reads the operator's advertised outbox interval when available and adds it as a first-class term. The bound lives entirely in the client ctx; the operator keeps its async poll→handleBuy→outbox path and is never serialized on a buy.

### 3.3 DQ3 — Tier detection + individual semantics (RULED)

- **Detection** = the existing env check reused verbatim: `DONTGUESS_RELAY_URLS` non-empty ⇒ **team**, else **individual** (`resolveRelayURLs`, serve.go:88-114). The wrapper mirrors it. If `DONTGUESS_RELAY_URLS` is set but the relay is unreachable, the client **fails loud** — never silently downgrades to local.
- **Team tier**: the client publishes the agent-signed event **DIRECT to the relay**. It MUST NOT route puts through the operator to be re-signed — 3b8 admission verifies the AGENT signature on the on-wire event against the FleetAllowlist. The operator serve intake is the sole writer/sequencer; the client never touches the file. **The team-tier client MUST NOT auto-start a local serve** (H6, RT-C#2): a client-spawned relay-attached serve mints its own operator key and becomes a rogue competing sequencer. The operator serve is a deliberately provisioned separate process.
- **Individual tier**: the client MUST NOT append to `events.jsonl` directly (single-writer). It routes put/buy through the **already-running `dontguess serve`** over the operator unix socket via two new ops, **`OpPut` and `OpBuy`**. The serve engine (sole writer) appends+folds; `OpBuy` blocks server-side up to a bounded window for the e-tagged match, then returns match + inline content over the socket. The wrapper's flock auto-start guarantees exactly one serve owns the file. Individual tier stays byte-for-byte: ScripStore==nil, TrustChecker==nil, zero network, zero identity ceremony.
  - `OpBuy` needs an extended socket deadline (default `operatorConnDeadline`=5s, serve.go:47) covering the bounded await. Operator gate handled internally (dedicated deadline constant).
  - **`OpPut`/`OpBuy` are individual-tier-only** and do NOT touch scrip (ScripStore==nil); they add no new mint exposure (RT-B#3 disposition, §10).

### 3.4 DQ4 — Admission / scrip onboarding UX (RULED)

- Admission (allowlist) and funding (mint) stay **out-of-band operator-local actions** (allowlist mutates the local Config file, allowlist.go:84-101; mint is `OpMint` over IPC, mint.go:72-82). **No client-initiated self-admit / self-fund wire message** — that would gut Seam A (enforcement_mode=open stays CUT). Onboarding is the documented manual loop:
  - Seller shares its npub → operator runs `dontguess allowlist add <npub>`.
  - Buyer shares its npub → operator runs `dontguess mint <npub> <amount>`.
  - **These two steps are NOT independent for a team-tier buyer (dontguess-980).** `dontguess allowlist add` gates BOTH sellers and buyers — `pkg/exchange/trust.go`'s `TrustChecker.Level()` consults the SAME `FleetAllowlist`/`KeySet` for every sender regardless of role. `OperationBuy` itself only requires `TrustAnonymous` (any minted-or-not key can match), but every buyer-side settle phase — `buyer-accept`, `complete`, `dispute`, `preview-request`, `small-content-dispute` — requires `TrustAllowlisted` (`defaultSettlePhaseLevels`, trust.go). So a buyer who is minted but **not** allowlisted matches fine, then has their `settle(buyer-accept)` silently dropped pre-fold at the dispatch trust gate (`engine_core.go` `dispatch`) — no reject is ever emitted, so the client's per-phase await just times out (AMBIGUOUS), which looks identical to a slow operator/relay. **A team-tier buyer's first settle requires the operator to run BOTH `dontguess allowlist add <buyer-npub>` AND `dontguess mint <buyer-npub> <amount>`** before onboarding is complete.
- The client makes both admission failures **LOUD and actionable**:
  - **`dropped_unlisted` / `dropped_low_reputation`**: the operator emits a durable, relay-published settle(put-reject) with a `reason` field. The `put` client REQ-subscribes for settle(put-reject) `#e:[<put-id>]` and prints the reason verbatim + `ask the operator to run: dontguess allowlist add <x>`.
  - **Insufficient scrip at buyer-accept**: made wire-visible by §3.6; the client surfaces it (received via the per-phase filter of §3.5) with `ask the operator to run: dontguess mint <your-npub> <amount>`.

### 3.5 DQ5 — Settle leg (RULED) — per-phase subscription mandatory

Client settle state machine, orchestrated by `dontguess buy` on a hit:

```
buy(3402) → match(3403) → [optional preview-request → preview (free)]
          → buyer-accept (reserves scrip) → deliver (content) → complete
```

- **Per-phase await filters (H3, RT-A#4 / RT-B#2).** The settle chain is a chain, not a star rooted at buyID: `match` e-tags buyID, but `settle(deliver)` e-tags the buyer-accept id (state_settle.go applySettleDeliver) and the §3.6 buyer-accept-reject e-tags the buyer-accept id. A single `#e:[buyID]` filter is **structurally incapable** of receiving the settle chain. RULED: **after publishing each phase message the client extends its subscription with `#e:[<that phase's message id>]`** (e.g. after buyer-accept, add `#e:[buyer-accept-id]` to receive BOTH settle(deliver) and settle(buyer-accept-reject)). Do NOT rely on the §3.2 filter for the settle chain; do NOT rely on `#p:[buyer-npub]` — the adapter sets the p-tag to the message *author* (operator on responses), so a buyer p-filter never matches operator responses (adapter.go author→p mapping).
- **Same-key invariant**: EVERY buyer-side phase MUST be signed by the exact npub that signed the originating buy. The engine re-derives `expectedBuyer` and **silently ignores** a mismatch (state_settle.go). The client guards locally that the key is unchanged across buy→settle and **fails LOUD** if it changed. The whole buy→settle runs in ONE CLI invocation under one stable identity. Fleet rule: a subagent that runs `buy` runs its `settle` with the same persisted key.
- **Preview-before-purchase**: `--preview` exposes the free preview-request→preview path. On the common path, `buy` proceeds to buyer-accept only when price ≤ budget.
- **Content delivery**: inline for ed2, with a hard max-inline-size guard that LOUD-fails oversized content pointing at the deferred Blossom item (gate G-blossom). When Blossom lands, the client MUST hash-check the fetched blob against the pointer digest and LOUD-fail on mismatch. Embeddings never on the wire.
- **`settle(complete)` is a behavioral consume signal**, not a payment ack — sent ONLY after actually receiving (and, under Blossom, verifying) content, so the Layer-0 correctness gate reflects real usage.

### 3.6 Mandated additive engine fix — buyer-accept-reject visibility (RULED)

The buyer-accept insufficient-balance path (engine_settle.go:624-627) MUST emit a **durable, wire-visible** `settle(buyer-accept-reject)` operator message (tags mirror `rejectPutLocked`: `TagSettle` + phase + `TagVerdictPrefix+"rejected"`, `reason:"insufficient_scrip"`, antecedent = the buyer-accept msg id) before returning the error. This mirrors the already-wire-visible put-reject and closes the loud-degradation hole so the buyer learns *why* buyer-accept failed instead of only timing out. Additive on a path that previously emitted nothing — low blast radius, but §7 confirms the suite stays green.

### 3.7 Mandated additive engine fix — deliver requires a live reservation (RULED — H2, RT-B#1)

**This is a Layer-0 correctness gate, not a UX nicety.** Today `buyerAcceptToMatch[msg.ID]` is written unconditionally in the fold (state_settle.go:149), while the scrip HOLD is a *separate* dispatch handler that can fail and be logged+dropped (engine_core.go:856-858), and `emitDeliverContent` consults only operator authorship + the antecedent chain — never a reservation (engine_settle.go:805-838). Net: an underfunded buyer publishes buyer-accept (hold fails), then publishes settle(deliver) → the operator emits full content free; at settle(complete) `reservationFor(match)` is empty → `return nil` → no scrip moves. **Content moves without payment.**

RULED fixes, all additive and guarded by `ScripStore != nil` (individual tier ScripStore==nil path is byte-for-byte untouched):
1. At the top of `handleSettleDeliverContent`, when `ScripStore != nil`, **require a live reservation** for the derived match (`reservationFor(matchMsgID)` non-empty) before `emitDeliverContent`. No reservation ⇒ do not deliver; this is the deliver-side guard.
2. Buyer-accept is **atomic in outcome**: `settle(deliver)`-triggering state and `settle(buyer-accept-reject)` (§3.6) are the two **mutually exclusive** results of a buyer-accept, decided by whether `decAndSaveHold` durably saved the reservation. A failed hold produces the reject (§3.6) and NO deliverable link.
3. ed2-G asserts the free-content exploit is closed: an underfunded buyer that sends buyer-accept + settle(deliver) receives the reject, NOT content.

### 3.8 Mandated additive wiring — Outbox.Notify on operator emit (RULED — H1, RT-A#1)

`Outbox.Notify()` (outbox.go:201-209) exists but has zero callers; the engine and outbox are decoupled through the store, so an operator match sits in `events.jsonl` up to a full 5s outbox tick before publish. RULED: add an optional `OnLocalAppend func()` callback to `EngineOptions`, fired by `appendLocalRecord` (engine_core.go:1101, the single operator-record egress) after a successful append; `attachRelayTransport` wires it to call **each leg's `outbox.Notify()`**. Effect: an operator match publishes the instant it is folded, collapsing the outbox term in the §3.2 timeout floor. The callback is nil on the individual tier (no outbox) — byte-for-byte unchanged. Additive, low blast radius; §7 confirms the suite stays green.

### 3.9 Mandated startup guard — relay-attached serve refuses to become a rogue operator (RULED — H6, RT-C#2)

Primary fix is the wrapper gate (§3.10). Defense-in-depth: `assertAdvertiseEqualsSign` currently returns nil for empty config (serve.go:672-681), so a fresh team-box serve silently becomes a valid-looking operator. RULED: **a relay-attached serve (`len(relayURLs)>0`) MUST refuse to start (hard error) when the persisted exchange config is absent or carries an empty `OperatorKeyHex`** — a legitimate operator always has config from `dontguess init` (operator gate G-init confirms init writes it before first team-tier serve). This makes a stray/auto-started team-tier serve fail loud instead of silently forking the sequencer. The individual tier (no relay URLs) is unaffected.

### 3.10 DQ6 — Install / wrapper (RULED)

- Delete `CF_REPO` and the cf install block (install.sh:6-7,16,76-87). cf is fully removable: NIP-42 client auth is native in `pkg/identity` and the team-tier default is `WithoutClientAuth` anyway.
- Rewrite every wrapper dispatch that execs cf to exec the `dontguess` client subcommands (§3.1-3.5).
- **Gate the wrapper's `serve` auto-start on individual tier ONLY (H6):** `if [ -z "$DONTGUESS_RELAY_URLS" ]; then <flock auto-start serve>; fi`. In team tier the client uses RelayTransport (direct-to-relay) and never auto-starts a local operator (install.sh:272-300 currently auto-starts unconditionally — that is the rogue-operator bug).
- Replace `exchange_campfire_id` / `XCFID` / `dontguess-exchange.json` with nostr-first config carrying `DONTGUESS_RELAY_URLS` + operator npub; empty relays selects SocketTransport (individual). Keep the flock auto-start of serve for individual tier (the single writer). Fix `CF_HOME`→`DG_HOME`.
- Update `README.md` and `llms.txt` in the **same change** (both still teach raw `cf init`/`cf join`/`cf put`; llms.txt misleads every agent until fixed).
- Retire `cmd/embed/main.py` (a live cf read/send client) as a scoped decision. Treat the `go.mod` `campfire-net` module path and the stale root ELF as separate low-priority cleanup.

---

## 4. Tier model summary

| | Individual | Team |
|---|---|---|
| Detection | `DONTGUESS_RELAY_URLS` empty | `DONTGUESS_RELAY_URLS` set |
| Transport | SocketTransport → running `serve` (OpPut/OpBuy IPC) | RelayTransport → direct-to-relay, agent-signed |
| Writer of events.jsonl | the one serve engine (sole writer) | the operator serve intake (sole writer) |
| Client auto-starts serve? | YES (flock, single writer) | **NO** (uses provisioned operator; auto-start forbidden) |
| Relay auth | n/a | `WithoutClientAuth` default; `--relay-auth` opt-in; every read deadline-bounded |
| Identity | opaque local-operator.key; agent key not required | per-agent secp256k1 npub (AGENT_CF_HOME) |
| Admission | none | Seam A FleetAllowlist (put), reputation floor 40 |
| Scrip | none (ScripStore==nil) | LocalScripStore; buyer must be minted; deliver gated on live reservation |
| Await | bounded server-side log-poll, inline content in socket reply | client-side per-phase REQ #e:[...], re-subscribe on drop, bounded ctx |
| Byte-for-byte unchanged? | YES (gate) | n/a |

---

## 5. Failure-mode matrix (LOUD everywhere)

| Cause | Wire signal | Client behavior (ruled) |
|---|---|---|
| Real match | 3403 (no buy-miss tag) | deliver content automatically (Notify-wired, so within timeout) |
| Genuine buy-miss | 3403 + `exchange:buy-miss` | show demand-signal guide |
| Seller never allowlisted | none to buyer (put dropped at Seam A) | indistinguishable from buy-miss → ambiguous timeout guide |
| Underfunded buy (D1) | none (silent nil) | enumerated timeout cause: "verify scrip / ask operator to mint" |
| dropped_unlisted (seller's own put) | durable put-reject w/ reason | print reason + `dontguess allowlist add <npub>` |
| Buyer-accept insufficient scrip | **none today → fixed** to durable reject (§3.6) | received via per-phase filter; print reason + `dontguess mint <npub> <amount>` |
| Underfunded buyer pulls deliver | **content today → fixed**: deliver refused (§3.7) | no content; sees the buyer-accept-reject instead |
| Conn drop mid-await | conn error | re-subscribe + re-Recv within budget (§3.2) |
| Non-challenging / stalled relay | socket accept, no AUTH | `WithoutClientAuth` default or deadline-guarded read → loud timeout, never hang |
| Dead/unreachable relay | conn error | fail fast + loud (bounded backoff), never hang, never downgrade |

### 5.4 The AMBIGUOUS-timeout rule
A buy timeout maps to at least {genuine no-match, underfunded-self, seller-never-admitted}. Two are wire-invisible. The client MUST print an AMBIGUOUS result enumerating the actionable causes — it MUST NOT claim "no cache exists."

---

## 6. Work-item tree (feeds swarm-plan)

Parent: **ed2 — nostr-first client (put/buy/settle) ships across individual + team tiers, all red-team hazards closed**

Each child is outcome-scoped, one-session, self-contained. Model tier per role.

1. **ed2-A — team `dontguess put` admits an allowlisted seller's 3401 into matchable inventory.** `pkg/relayclient` publish primitive: reuse `relay.Conn` bounded backoff; **default `WithoutClientAuth`**, `--relay-auth` opt-in, **connect+handshake under a ctx watchdog that Closes on expiry** (§3.1, H4); sign with agent key; single-goroutine send-EVENT/await-OK; OK≠success. Cobra `put`. The `put` client REQ-subscribes settle(put-reject) `#e:[put-id]` and surfaces the reason. Outcome: an allowlisted npub's put → matchable entry on a running operator; a non-allowlisted put → surfaced put-reject; a socket-accepts-then-stalls relay → loud error inside timeout. Model: **Sonnet**. Deps: none.

2. **ed2-D — operator gates content delivery on a live scrip reservation AND emits a wire-visible `settle(buyer-accept-reject)` on insufficient scrip.** Additive engine changes (§3.6 + §3.7), both guarded by `ScripStore != nil` so the individual tier is byte-for-byte: (a) `handleSettleDeliverContent` requires `reservationFor(match)` before `emitDeliverContent`; (b) failed `decAndSaveHold` emits the durable reject mirroring `rejectPutLocked`. Outcome: an underfunded buyer-accept produces a durable reject (client-printable) and NO content; the full ~32K suite stays green; a new engine test asserts free-content-on-unfunded-deliver is closed. Model: **Opus** (Layer-0 correctness). Deps: none (parallel).

3. **ed2-N — operator publishes an operator match the instant it is folded (Notify wired).** Add `EngineOptions.OnLocalAppend`, fire it in `appendLocalRecord`, wire `attachRelayTransport` to call each leg's `outbox.Notify()` (§3.8). Outcome: an operator match reaches the relay sub-second instead of up to 5s later; `publishLag` drops to ~0 on the match path; ~32K suite green. Model: **Opus** (touches the frozen egress path). Deps: none (parallel).

4. **ed2-B — team `dontguess buy` returns cached content on a hit and a distinguished LOUD outcome on miss/ambiguous within a bounded timeout.** Subscribe-first REQ(#e:[buyID], kinds:[3403,3404]) → publish → await with **re-subscribe on `ErrConnDropped`** (§3.2, H5); tag-discriminate real-match vs buy-miss vs leaked-assign; **10s default timeout with outbox-term headroom** (H1). Outcome: a hit returns content within timeout even when the match is published one tick late; a dropped conn mid-await still recovers the match; a miss prints the demand guide; a timeout prints AMBIGUOUS with enumerated causes; a dead relay exits loud inside timeout. Model: **Opus** (protocol subtlety). Deps: ed2-A, ed2-D, ed2-N.

5. **ed2-C — team settle chain moves scrip and delivers content in one invocation.** buyer-accept→deliver→complete (plus `--preview`); **per-phase subscription extension `#e:[<each published phase id>]`** so deliver AND buyer-accept-reject are received (§3.5, H3); same-key guard; inline content + max-size LOUD guard. Outcome: a minted buyer's `buy` on a hit ends with content in hand and scrip moved; an underfunded buyer receives and prints the §3.6 reject (not a bare timeout); a key-mismatch fails loud client-side. Model: **Opus** (the reject-must-be-received assertion is the H3 correctness point). Deps: ed2-B, ed2-D.

6. **ed2-E — individual `dontguess put`/`buy` work with zero relay via OpPut/OpBuy IPC, single-writer preserved.** New socket ops; OpBuy blocks server-side (extended deadline) and returns match + inline content; SocketTransport in `pkg/relayclient`. OpPut/OpBuy are individual-tier-only, ScripStore==nil, no mint path. Outcome: with `DONTGUESS_RELAY_URLS` unset and a serve running, put then buy returns matched local content; no second writer touches events.jsonl. Model: **Sonnet**. Deps: ed2-A.

7. **ed2-F — installer + wrapper de-campfired; serve auto-start gated to individual tier; relay-serve startup guard; README + llms.txt updated.** Drop cf download; wrapper execs dontguess subcommands; config = relay URLs + operator npub; tier detection mirrored; **flock serve auto-start wrapped in `[ -z "$DONTGUESS_RELAY_URLS" ]`** (H6, §3.10); add the §3.9 relay-attached-serve refuse-to-start-without-config guard; DG_HOME. Retire cmd/embed/main.py. Outcome: a fresh team-tier install NEVER auto-starts a local operator; a fresh install references zero cf and drives put/buy/settle through the dontguess binary; a relay-attached serve with no config fails loud. Model: **Sonnet**. Deps: ed2-A, ed2-B, ed2-C, ed2-E.

8. **ed2-G — cache-safe round-trip integration test proves put→buy→match→settle with admission+scrip and asserts every closed hazard.** **Cache-immune by construction, two ways (H7, RT-C#1):** (a) a **package-main `_test.go` in `cmd/dontguess`** drives the cobra `RunE` functions in-process against a `teamTierServeStack()` + `fakeRelayConn` (precedent: serve_relay_test.go, serve_poison_injection_e2e_test.go) so any `cmd/dontguess` source edit invalidates the cache; (b) an explicit **named CI step `go test -count=1 -run TestE2E ./cmd/dontguess/... ./test/...`** the acceptance gate invokes — NOT a doc comment. Asserts: allowlisted-put matchable; minted buy→match→settle moves scrip + delivers; non-allowlisted put → LOUD put-reject; **underfunded buyer-accept → LOUD reject actually RECEIVED and surfaced by the client** (not merely emitted); **underfunded buyer + settle(deliver) → NO content (H2 closed)**; match published one outbox-tick late still arrives within the buy timeout (H1); conn-drop mid-await recovers the match (H5). Model: **Opus** (correctness gate). Deps: ed2-B, ed2-C, ed2-D, ed2-E, ed2-N.

9. **ed2-H — install E2E replacing deleted `test/e2e-install.sh`; triage `test/reliability/*.sh` + `test/demo/*.sh`.** New install E2E asserts the wrapper references NO cf, routes to the operator binary, AND **auto-starts serve only when `DONTGUESS_RELAY_URLS` is empty** (H6) — extract wrapper bytes fresh (mirror `install_flock_injection_test.go`). Delete/rewrite the cf-era scripts (no E2E deleted without a nostr-first replacement). Model: **Sonnet**. Deps: ed2-F.

10. **ed2-Z — low-priority cleanup.** init.go unconditional nostr-operator.key mint reconciliation; go.mod campfire-net module path; remove stale root ELF; file a separate hardening item for the pre-existing OpMint-on-shared-socket exposure (RT-B#3 disposition — split mint to an operator-key-signed admin path). Model: **Haiku**. Deps: none; off critical path.

Wiring: A, D, N, Z start in parallel. B←(A,D,N). E←A. C←(B,D). F←(A,B,C,E). G←(B,C,D,E,N). H←F. Parent stays open until A–H close.

---

## 7. Acceptance gate

Done only when ALL hold:
1. Full existing exchange suite (~32K) passes **UNCHANGED** (`go test ./...`), including after the ed2-D (deliver-gate + reject) and ed2-N (Notify) engine changes.
2. Individual tier is byte-for-byte: no behavior change on the ScripStore==nil / TrustChecker==nil path (the §3.6/§3.7 guards are `ScripStore != nil`; the §3.8 callback is nil there); the serve_local test still passes.
3. **Cache gap closed (H7):** ed2-G's package-main in-process test invalidates the cache on any `cmd/dontguess` source edit, AND the gate command explicitly runs `go test -count=1 -run TestE2E ./cmd/dontguess/... ./test/...`. A doc comment alone does not satisfy this gate.
4. ed2-G asserts LOUD behavior on the silent-fail classes: dropped_unlisted (put-reject surfaced), insufficient-scrip buyer-accept (new reject **received & surfaced**), and **underfunded deliver yields NO content** — not just the happy path.
5. Timing hazards proven: a match published one outbox tick late still arrives within the buy timeout (H1); a conn drop mid-await recovers the match (H5).
6. `grep -R` over install.sh, the generated wrapper, README.md, llms.txt shows zero cf download or cf dispatch; ed2-H asserts it AND asserts the serve auto-start is individual-tier-gated (H6).
7. No new cross-process file lock added to `pkg/store`; the single writer is the one serve process in both tiers.
8. Bounded backoff + bounded await + **bounded handshake** proven: a dead-relay `dontguess buy`, AND a relay that accepts the socket then sends nothing, each exit with a loud error inside the timeout, never hang (H4).

---

## 8. Anti-scope
- No self-admit / self-fund / balance-query wire convention (preserves Seam A).
- No BrokeredMatchMode support (gate G1; assume disabled).
- No Blossom fetch path in ed2 (inline + LOUD size guard; deferred).
- No change to the operator's async poll/sequencer model; the await bound is client-side. (ed2-N only accelerates *publication* of an already-emitted record; it does not put the operator on the buy hot path.)
- No splitting of the OpMint admin path in ed2 (pre-existing exposure, filed as ed2-Z sub-item; team tier does not route through the socket at all).
- No reuse of `demuxPublisher`, `reqfilter.go`, or `serve_local_test.go`'s writer+engine pattern for the real client.

---

## 9. Residual risks
- Underfunded-buy (D1) stays wire-silent to the buyer — only enumerable as a timeout cause (§5.4). Closing it would require an engine change to the anonymous-buy drop path; out of ed2 scope.
- Seller-never-allowlisted is indistinguishable from buy-miss at the buyer (the put-reject goes to the *seller*, not the buyer). Documented as an AMBIGUOUS cause.
- Subscribe-first + re-subscribe-on-drop depends on strfry historical replay (`since`/`#e`) to fully close the race; if a relay does not store/replay, a match published during a reconnect gap can still be lost. Operator-selected relay must retain events.
- Individual OpBuy ties up a serve goroutine per bounded await (bounded, but a burst of concurrent individual buys consumes goroutines).
- The pre-existing OpMint-on-shared-socket self-fund exposure persists on the operator host (RT-B#3); ed2 adds no new exposure but does not close it — tracked in ed2-Z.
- The go.mod `campfire-net` module path persists (cosmetic; no runtime cf dependency).
- ed2-D and ed2-N sit inside the frozen-suite blast radius; both are additive and gated, but the §7.1 regression check is the enforcing control.

---

## 10. Red-team disposition

| # | Finding (severity) | Verified at | Disposition |
|---|---|---|---|
| A1 | Client ~5s buy timeout expires before a match is published (Outbox ticks 5s, Notify unwired) — HIGH | serve.go:308, outbox.go:204 (0 callers), serve.go:76 | **FOLDED** §3.8 (wire Notify via OnLocalAppend) + §3.2 (10s timeout, floor includes outbox term); item ed2-N + gate §7.5. |
| A2 | NIP-42 handshake reads have no deadline → hangs on non-challenging/stalled relay — MEDIUM | nip42_handshake.go:29,50; conn.go:97-115 | **FOLDED** §3.1 (team-tier default `WithoutClientAuth`, `--relay-auth` opt-in, ctx-watchdog deadline on every read); gate §7.8; item ed2-A. |
| A3 | No re-subscribe on reconnect mid-await → silent match loss — MEDIUM | conn.go:210-215 | **FOLDED** §3.2 (re-issue REQ on `ErrConnDropped` before next Recv); item ed2-B; gate §7.5. |
| A4 | `#e:[buyID]` filter cannot receive the settle(deliver) chain — MEDIUM | state_settle.go applySettleDeliver; adapter.go:76-82 | **FOLDED** §3.5 (per-phase `#e:[phase-id]` subscription extension); item ed2-C. |
| B1 | Content delivery not gated on a live scrip reservation → free content — HIGH | state_settle.go:149; engine_settle.go:805-838,114-118 | **FOLDED** §3.7 (deliver requires live reservation when ScripStore!=nil; buyer-accept outcome is deliver XOR reject); item ed2-D; gates §7.2,§7.4. |
| B2 | §3.6 reject + deliver invisible to the buy filter — MEDIUM | same as A4 | **FOLDED** (same fix as A4, §3.5) + ed2-G asserts the reject is *received & surfaced*, not just emitted (§7.4). |
| B3 | OpMint god-button on the shared operator socket → same-host self-fund — LOW | serve.go:645-646 | **REJECTED for ed2 scope (pre-existing, not widened):** ed2 routes only *individual* tier (ScripStore==nil, OpMint no-op) through the socket; team tier uses RelayTransport direct-to-relay and never touches the socket, so ed2 adds no new mint exposure. Tracked as an independent hardening sub-item in ed2-Z + residual risk §9. |
| C1 | TEST-CACHE GAP not closed by a doc comment; client regressions ship cached-green — HIGH | `go list -deps ./test/`=0; go test caching | **FOLDED** §6 ed2-G (package-main in-process cobra-RunE test invalidates cache on source edit) + §7.3 (explicit `-count=1` CI step named in the gate, not a comment). |
| C2 | Team-tier wrapper auto-starts a rogue competing sequencer — HIGH | install.sh:272-300; serve.go:161-167,672-681 | **FOLDED** §3.10 (wrapper auto-start gated to individual tier only) + §3.9 (relay-attached serve refuses to start without exchange config); items ed2-F, ed2-H; gate §7.6. |
