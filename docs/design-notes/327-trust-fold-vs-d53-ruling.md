# Ruling: dontguess-327 (trust-before-fold) vs shipped d53 — adopt Path C

**Date:** 2026-07-13
**Source:** escalation-design (3-pass adversarial: RPT purist/opus + systems pragmatist/sonnet + research/sonnet), unanimous.
**Origin:** swarm dontguess-634 W1; 327's implementer escalated an architecture conflict.

## Problem

Item 327 asked to move `TrustChecker.Check` **before** `state.Apply/Replay` at the fold sites in
`pkg/exchange/engine_core.go` so a non-allowlisted / below-reputation-floor put/buy **never appears in State**.
Implementing this literally **contradicts shipped P1 security design d53**
(`docs/design/nostr-admission-scrip-rehome-3b8.md`), which:

- **Ratified promotion-gating (Seam A)**, not fold-gating.
- **Explicitly deferred the pre-Apply fold reorder as ADV-17** — a firm, dated operator decision
  (`docs/design/relay-transport.md:514,521-522`, "not re-litigated"), because the reorder needs property
  tests that don't exist and risks the b84/-90d fold-cursor invariants (`engine_core.go:1029-1037`).
- **Accepted the transient `pendingPuts` staging as inert (decision D2)** — but D2 rules only the
  **durable-log** residual inert (`nostr-admission-scrip-rehome-3b8.md:99`), compaction-reclaimed.

A literal 327 reorder **breaks 3 shipped d53 tests** (`admission_seam_test.go`): Seam-A drop-counters
(`dropped_unlisted`/`dropped_low_reputation`, incremented only when the put reaches Seam A as `pendingPuts`,
`engine_pricing.go:255,258`) read 0; and Seam-D restart re-gating requires a previously-accepted,
now-de-allowlisted entry to **re-enter** inventory on replay to be re-flagged `NeedsRevalidation`
(`admission_seam_test.go:216-223`) — a `foldTrusted` filter drops it. Testing supremacy forbids weakening these.

Timeline: 327 filed 2026-07-08 but sat idle; d53 was designed, implemented and shipped entirely within the gap
(2026-07-10). **327 is stale against ratified shipped work.**

## The real gap all three reviewers found (un-ruled-upon by d53)

`contentHashIndex` is written **zero-trust** during the fold at `state_put.go:571,627` (before any trust check
exists) and is **never purged on reject** — `applySettlePutReject` (`state_settle.go:66-76`) purges only
`pendingPuts`. The non-purge is **deliberate for quality-gate rejects** (anti-respam, comment at
`state_put.go:623-626`). But it applies identically to **Seam-A trust rejects**: a non-allowlisted sender can
permanently squat a content hash Seam A will never let them sell, silently blocking a legitimate future seller's
identical-content put forever (`state_put.go:571-572`, bare `return`, no counter). Byte-identical content across
agents is the exchange's **designed happy path** (high-reuse checklists / CI fragments / recipes), so collision
is not exotic — it is the reuse thesis turned into a griefing lever. **D2 never covered the in-memory dedup index.**

## Paths considered

- **(A) Close 327 as obsoleted-by-d53.** LOC 0. Rejected: leaves the hash-squat DoS fully open (D2 does not cover it).
- **(B) Re-architect d53** (drop-accounting promotion→fold + provenance-aware restart distinction). Rejected:
  contradicts the firm ADV-17 operator decision without new evidence, breaks all 3 shipped tests, re-opens the
  b84/90d cursor invariants, ~150-300+ LOC + doc rewrite. Highest blast radius, least marginal gain.
- **(C) Narrow 327 to the contentHashIndex residual.** Unanimous recommendation.

## Ruling — adopt Path C

Re-scope 327 from "trust-check before fold / never appears in State" to:
**"Purge `contentHashIndex` on a Seam-A trust-gate put reject, and make the drop observable."**

Implementation (surgical, ~15-25 LOC, **0 shipped d53 tests touched**, replay-safe — message-content-driven,
no live `TrustChecker` in `Apply`):

1. `rejectPutLocked` already threads a `reason` (`engine_pricing.go:358-362`); Seam A tags its call
   `"trust-gate: "+reason` (`engine_pricing.go:262`). Thread an explicit `purge_hash: true` signal on that
   **trust-gate reject path only**.
2. `applySettlePutReject` (`state_settle.go:66-76`) reads it and additionally
   `delete(s.contentHashIndex, entry.ContentHash)`. **Quality-gate rejects omit the field → anti-respam
   persistence (`state_put.go:623-626`) is unchanged.**
3. Add a `dropped_dedup_poison` (or similarly named) counter so the previously-silent drop
   (`state_put.go:571-572`) becomes an observable feedback signal (RPT: feedback-loop integrity).

**Done when:** a test proves a non-allowlisted put's content hash, after a Seam-A reject, does **not** block a
later allowlisted put of identical content, and the counter increments; **all 3 d53 seam tests stay green**;
full exchange suite green.

**Dissent acknowledged:** none material. Purist flagged (and research confirmed) the anti-respam persistence at
`state_put.go:623-626` — addressed by scoping the purge to the trust-gate reject path only, leaving quality-gate
persistence intact.

**Consequence for the tree:** Path C touches `engine_pricing.go` + `state_settle.go` (the reject path), **not**
the `engine_core.go` fold sites. The `221 ← 327` dependency (created to avoid auditing code 327 would change)
is therefore **moot** — 221 audits `engine_core.go` fold/rebuild idempotency, which Path C does not touch.
