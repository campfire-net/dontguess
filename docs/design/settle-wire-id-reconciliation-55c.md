<!-- Produced by escalation-design workflow (rd dontguess-55c / ed2-C blocker). 3 adversarial passes (purist/pragmatist/research-verifier) + architect ruling, all claims read from source at bfb6a65. This is a DESIGN CORRECTION: the ed2 client design (nostr-first-client-ed2.md §3.5) missed the wire-id/store-id divergence across the relay boundary. -->

# dontguess-55c — Team-tier settle reconciliation: resolve buyer-accept by match WIRE id + operator auto-deliver

Status: RULED (escalation-design). Operator-side prerequisite that unblocks the purely-client ed2-C (dontguess-008).

## The gap the ed2 design missed

The team-tier settle chain (buyer-accept → deliver → complete) cannot move scrip as designed, **empirically confirmed** (full-stack probe: real engine + LocalScripStore + minted buyer + fake relay — a buyer-accept e-tagging the match wire id produced NO scrip hold, buyer balance unchanged).

- The operator's match id is a **random store id** (`newReservationID()`, `engine_core.go` `sendLocalOperatorMessage`). `applyMatch` keys `matchToBuyer[msg.ID]` by that store id (`state_buy.go:79`); `buyerAcceptToMatch`/`deliverToMatch`/`previewToMatch` likewise.
- The match is published by the Outbox, which **re-signs** it → a content-hash **wire id** (`outbox.go` → `identity.SignEvent`). The relay buyer only ever sees that wire id; ed2-B sets `BuyResult.MatchMsgID = ev.ID` (`buy.go:352`) — the designated e-tag seam.
- A buyer-accept e-tagging the wire id → `ResolveMatchFromAntecedent(wireID)` → `matchToBuyer[wireID]` **miss** → `handleSettleBuyerAcceptScrip` returns at the unknown-match gate (`engine_settle.go:514`). No hold, no settle, and the reject branch is never reached (H3 unreachable).
- **Echo-dedup** (`WithEmittedSeeder`) deliberately keeps the wire-id match echo out of engine state (folding it would double-credit — the 15f/2f0 restart-seed fix).
- The **~32K suite misses it** because it drives settle in-process with consistent store ids. ed2-B/C are the first real relay clients to cross the boundary.

Second gap: nothing emits the operator `settle(deliver)` on a **successful** buyer-accept in team tier (buyer can't — `applySettleDeliver` operator-gated; frozen tests emit deliver manually).

## Ruling: strategy (b) wire→store alias + default-off auto-deliver — ONE operator-side item

Rejected: **(a) sign-at-emit** (engine has no signer by design; rewrites every record id; reopens the echo double-credit; detonates the frozen signer-less suite) and **(c) payload-carried store id** (contradicts the wire-id seam; makes settle antecedents e-tag a phantom id that breaks the Intake/Sequencer causal DAG + dedup).

### GAP 1 fix — wire→store alias (read-time resolution only; no signed event changes)
- New `State.wireToStore map[string]string`; `RegisterWireAlias(wire, store)` (idempotent, collision-alarms) + private `resolveAlias(id)` (identity when absent).
- `resolveAlias` prepended at **every buyer-referenced-operator-id resolution — in the FOLD handlers too, not only accessors** (the fold sets `buyerAcceptToMatch` the auto-deliver depends on): accessors `ResolveMatchFromAntecedent`, `MatchForDeliver`, `SellerKeyForDeliver`, `EntryForDeliver`, `MatchInfo`; fold `applySettleBuyerAccept`, `applySettleBuyerReject`, `applySettleComplete`, `applySettlePreviewRequest`, `applySettleSmallContentDispute`. Coverage = every operator wire id a client e-tags (match, preview, deliver). `applySettleDeliver`/`applySettlePreview` unchanged (native buyer/relay-origin ids, store==wire).
- Registration: **LIVE** at Outbox `Tick` beside the existing `seedEmitted` call (strictly before publish — the buyer cannot know the wire id until after publish, so ordering holds); **RESTART** in `seedEmittedFromStore`'s operator-record loop that already re-derives the nonce-independent `signedEventID` (deterministic → alias rebuilt identically, no persistence of its own).
- `State.Replay` must **NOT** reset `wireToStore` (precedent: `priceAdjustments`/`brokerMatchIDs`/`federationProfiles`) — State has no signer to re-derive it; the Outbox/seed repopulate it.

### GAP 2 fix — operator auto-deliver on fresh-hold success (default-off)
- New `EngineOptions.AutoDeliverOnBuyerAccept bool` (default false; team-tier serve only; **NOT** gated on ScripStore so the frozen scrip suite keeps its single manual deliver).
- On the **fresh-hold success** path of `handleSettleBuyerAcceptScrip` (after `decAndSaveHold` returns nil — the `IsMatchSettled` guard + `restoreExistingHold` short-circuit have already returned on any re-send, so it fires **exactly once per match**), when the flag is set and `reservationFor(match)` is live, call `emitDeliverContent` **directly** (not a trigger — an operator-emitted trigger is folded-but-never-dispatched) and `state.Apply` the returned deliver so `deliverToMatch` is populated for the buyer's `complete` (mirrors `emitConsumeSignal`'s send-then-Apply). `emitDeliverContent`/`emitDeliverPointer`/`sendDeliverMessage` change to return the emitted message.

## Invariants (must not break)
- 32K in-process suite byte-for-byte: no Outbox ⇒ alias empty ⇒ `resolveAlias` identity; flag defaults false.
- Individual tier byte-for-byte: `ScripStore==nil` ⇒ `handleSettleBuyerAcceptScrip` never runs; no relay ⇒ no alias; flag false.
- No scrip double-spend: alias only ADDS resolution for currently-failing ids (no new credit path); `newReservationID` stays random; FIX-M1 settled-match guard + FIX-M2 emit-durable-then-mutate unchanged; auto-deliver gated behind fresh-hold success ⇒ never fires on a re-accept.
- Replay/restart determinism: `wireToStore` is a deterministic function of the operator log + signer, rebuilt identically at restart; `rebuildAndDispatchGapLocal` must not wipe it.
- Thread-safety: `RegisterWireAlias` takes `s.mu` (Outbox writes at publish; poll/dispatch read via `resolveAlias`); no lock-order cycle.

## Acceptance
- New team-tier regression (`serve_relay_test.go`): put→buy→match; capture published match wire id `M_w`; buyer-accept e-tagging `M_w` ⇒ buyer debited price+fee (real hold) + content deliver published; complete e-tagging deliver wire id ⇒ seller credited residual + `IsMatchSettled==true`.
- Restart e2e: a wire-id buyer-accept for a match created BEFORE restart still resolves hold→auto-deliver→complete after `seedEmittedFromStore` rebuilds the alias.
- `resolveAlias`-survives-Replay unit test.
- Auto-deliver fires exactly once (re-accept + post-settlement re-accept emit no second deliver).
- Frozen-suite pin: flag false ⇒ existing manual-trigger e2e/settle tests still emit exactly one deliver, all assertions unchanged.
- Full non-short exchange + cmd/dontguess + test suites green under `-race`, unchanged.

## Client remainder (ed2-C)
Once 55c lands, ed2-C is **purely client-side** (`pkg/relayclient`): drive the per-phase settle chain over the relay, only ever e-tagging the **wire ids** received over the relay (the operator resolves wire→store via the alias); handle buyer-accept-reject + AMBIGUOUS/timeout. No further engine change.
