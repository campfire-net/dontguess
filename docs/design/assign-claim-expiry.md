# Design: Assign Claim Expiry — TTL and Re-Open Protocol

**Date:** 2026-03-31
**Author:** Convention Designer (Opus)
**Status:** Active
**Depends on:** Assign convention (docs/design/assign.md), Core operations (dontguess-2qv)
**Source:** dontguess-fcf — "assign: stuck claim — no expiry/re-open when claimant disappears"

---

## 1. Problem Statement

The assign state machine has no mechanism to expire a claim when the claimant goes silent. An agent can claim a task (`exchange:assign-claim`) and then disappear — crash, lose connectivity, get compacted, or simply abandon the work. The task remains in `AssignClaimed` state indefinitely. No other agent can claim it. The maintenance work never gets done.

The convention spec (assign.json) already declares a `claim_timeout_minutes` field and the design doc (assign.md, D4) specifies a 15-minute default timeout. But the state machine in `pkg/exchange/state.go` has no expiry check — `applyAssignClaim` transitions to `AssignClaimed` and nothing ever transitions it back to `AssignOpen` based on elapsed time.

This design defines the protocol-level mechanism for claim TTLs and automatic re-open behavior.

---

## 2. Current State

### 2.1 What the Convention Spec Says

The `assign-claim.json` declaration requires an `expires_at` field (ISO 8601 timestamp). The `assign.json` declaration includes `claim_timeout_minutes` (default 15, max 30). The design doc (assign.md, D4) specifies:

- Claimed assignments must be completed within 15 minutes
- If not, the claim expires and the task returns to the open pool
- Expired claims do not count against the agent's reject rate
- The exchange MAY extend the timeout for compress tasks on entries > 50,000 tokens (up to 30 minutes)

### 2.2 What the Implementation Does

The `AssignRecord` struct has no `ExpiresAt` field. The `applyAssignClaim` method records `ClaimantKey` and `ClaimMsgID`, transitions to `AssignClaimed`, and registers the agent-to-assign binding in `claimedAssigns`. There is no timer, no expiry check, and no re-open path for timed-out claims.

The `ActiveAssigns` method filters out terminal states (`AssignAccepted`, `AssignRejected`, `AssignPaid`) but does not check whether claimed assigns have expired.

### 2.3 Gap

A claimed-but-abandoned task is stuck forever. This is a liveness bug in the assign protocol.

---

## 3. Design Decisions

### D1: Claim TTL — 15 Minutes Default, Configurable Per Assignment

**Decision:** Each claim carries an `expires_at` timestamp set by the claimant at claim time. The timestamp must be no later than `claim_time + claim_timeout_minutes` (from the originating assign message). If the claimant omits `expires_at` or sets it beyond the allowed window, the engine normalizes it to `claim_time + claim_timeout_minutes`.

**Recommended default:** 15 minutes.

**Rationale:**

- **15 minutes is generous for single-task maintenance work.** Validation requires one inference call plus comparison. Enrichment requires summarization and tagging. Even compression of a large entry (50K tokens) is a single inference pass — the 30-minute extension covers this.
- **Shorter TTLs (5 minutes) penalize slow connections and large entries.** An agent on a rate-limited API endpoint or processing a 40K-token entry may legitimately need 10+ minutes.
- **Longer TTLs (60+ minutes) enable claim squatting.** An agent that claims 3 tasks and sits on them for an hour blocks 3 maintenance jobs for the entire period.
- **The 3-active-claim cap (D2 in assign.md) amplifies the impact.** With a 15-minute TTL, a squatter blocks at most 3 tasks for 15 minutes. With a 60-minute TTL, they block 3 tasks for an hour.

**Extended TTL for compression:** The assign message's `claim_timeout_minutes` field allows the operator to set up to 30 minutes for compress tasks on entries > 50,000 tokens. The claimant's `expires_at` must respect this ceiling.

### D2: Re-Open Trigger — On-Next-Message (Lazy Evaluation), Not Heartbeat

**Decision:** Expiry is checked lazily when a relevant event occurs, not via a background timer or heartbeat protocol.

**Trigger points (evaluated in order of likelihood):**

1. **On new `assign-claim` for the same task.** If agent B tries to claim a task that agent A holds, and agent A's claim has expired, the engine expires A's claim and grants it to B. This is the most common trigger — agents polling for work will naturally attempt to claim expired tasks.

2. **On `ActiveAssigns` query.** When the engine or any consumer calls `ActiveAssigns(entryID)`, expired claims are reaped before returning results. This ensures stale data never leaks to callers.

3. **On `applyAssignComplete` for the expired claim.** If the claimant submits work after their claim expired, the completion is rejected (the claim is no longer valid). The agent wasted inference, but the protocol is consistent.

4. **On engine poll cycle.** The engine's existing poll loop (which processes incoming messages) can include a periodic expiry sweep. This is the backstop — if no agent attempts to claim an expired task and no one queries active assigns, the sweep catches it.

**Why not heartbeat:**

- **Heartbeat adds protocol complexity.** A heartbeat requires a new message type (`exchange:assign-heartbeat`), a cadence decision, campfire message volume concerns, and failure detection logic (how many missed heartbeats = expired?).
- **Heartbeat adds agent complexity.** Every agent that claims a task must run a background heartbeat loop. Agents that are simple request-response inference calls (claim, compute, submit) would need to be restructured as long-running processes.
- **Campfire is append-only.** Heartbeat messages accumulate in the log. At 1 heartbeat per minute for 15 minutes, that is 15 messages per claim per agent. For convergence tasks (3 agents), that is 45 messages per assignment. This is noise.
- **Lazy evaluation is sufficient.** The TTL is short (15 minutes). The pool of agents claiming tasks ensures that expired claims are discovered within minutes of expiry. The engine poll backstop catches the rest.

**Why not a background timer in the engine:**

A background timer (goroutine that scans for expired claims every N seconds) would work but violates the state derivation principle: all state must be derivable from the campfire message log. A timer-driven state change has no corresponding message. Instead, the engine emits an `exchange:assign-expire` message when it detects an expired claim during a poll cycle (see section 4).

### D3: Partial Result Before Expiry — Accept If Complete, Reject If Not

**Decision:** If the claimant submits `assign-complete` before `expires_at`, the completion is valid regardless of how close to the deadline. If the claimant submits `assign-complete` after `expires_at`, the completion is rejected.

**Edge cases:**

| Scenario | Outcome |
|----------|---------|
| Complete arrives 1 second before expiry | Valid. Timestamp comparison is strict: `complete.timestamp < claim.expires_at`. |
| Complete arrives 1 second after expiry | Rejected. The claim expired. The agent's inference is wasted. |
| Complete arrives after another agent re-claimed | Rejected. The original claim was expired and re-assigned. The original claimant's `assign-complete` references a claim that is no longer active. |
| Complete arrives after engine emitted `assign-expire` | Rejected. The expire message is authoritative. |
| Agent sends partial result (e.g., incomplete compression) | Not a protocol concept. `assign-complete` is binary — submitted or not. Partial results are rejected by the algorithmic validation (size reduction < 30%, etc.), not by the expiry mechanism. |

**Rationale:** The deadline is a hard cut. Soft deadlines (grace periods, partial credit) create ambiguity and attack surface. An agent that cannot complete within the TTL should not have claimed the task. The no-partial-payment rule (assign.md, D6) already establishes the binary pass/fail model — expiry is just another failure mode.

### D4: Expired Claim Impact on Agent Reputation

**Decision:** Expired claims do NOT count toward the agent's reject rate. They are tracked separately as `timeout_count` in the agent's eligibility profile.

**Thresholds:**

- **3 consecutive timeouts:** Agent is temporarily ineligible for new claims (cooldown: 1 hour).
- **5 timeouts in a rolling 24-hour window:** Agent is temporarily ineligible for new claims (cooldown: 4 hours).
- **Timeouts do not affect reputation score.** Reputation reflects work quality (accept/reject ratio), not reliability. Timeout is a reliability signal, tracked separately.

**Rationale:**

- Counting timeouts as rejections would unfairly penalize agents that crash or lose connectivity through no fault of their own.
- But unlimited free timeouts enable claim squatting (A4 in assign.md adversarial analysis).
- The cooldown mechanism balances forgiveness for occasional failures with protection against systematic abuse.

---

## 4. State Transitions

### 4.1 New Operation: `exchange:assign-expire`

A new operation emitted by the exchange when it detects an expired claim. This is an operator message (signed with `campfire_key`), not a member message — the exchange enforces its own timeout.

| Field | Value |
|-------|-------|
| Sender | Exchange operator |
| Signing | campfire_key |
| Antecedent | exactly_one(target) — the assign-claim message that expired |
| Tags | `exchange:assign-expire` |

**Payload:**

```json
{
  "assign_id": "<original assign message ID>",
  "claim_id": "<expired assign-claim message ID>",
  "claimant": "<hex public key of the agent whose claim expired>",
  "expired_at": "<ISO 8601 timestamp from the claim's expires_at>",
  "detected_at": "<ISO 8601 timestamp when the engine detected the expiry>"
}
```

This message serves two purposes:

1. **Auditability.** The campfire log records exactly when and why a claim was expired. State derivation from the log produces the same result as lazy evaluation.
2. **State derivation consistency.** On replay, the `assign-expire` message drives the state transition. Without it, state derivation would depend on wall-clock time during replay, producing different results depending on when the replay happens.

### 4.2 State Transition Diagram

```
                                    ┌─────────────────────────┐
                                    │                         │
                                    ▼                         │
              ┌──────────┐    assign-claim    ┌───────────┐   │
              │          │ ─────────────────► │           │   │
              │  OPEN    │                    │  CLAIMED  │   │
              │          │ ◄─────────────────┐│           │   │
              └──────────┘    assign-expire   │└───────────┘   │
                   ▲          (TTL elapsed)   │     │          │
                   │                          │     │          │
                   │    assign-reject         │     │ assign-  │
                   │    (re-open)             │     │ complete │
                   │                          │     │          │
                   │          ┌───────────┐   │     ▼          │
                   └──────────│           │   │┌───────────┐   │
                              │ REJECTED  │   ││           │   │
                              │ (transi-  │   ││ COMPLETED │   │
                              │  tory)    │   │└───────────┘   │
                              └───────────┘   │     │          │
                                              │     │          │
                                              │     ├─── assign-accept ──► ACCEPTED ──► PAID
                                              │     │                         │
                                              │     └─── assign-reject ──────┘
                                              │            (re-open)
                                              │                │
                                              └────────────────┘
```

**New transitions (bold):**

| From | Event | To | Condition |
|------|-------|----|-----------|
| OPEN | assign-claim | CLAIMED | Agent eligible, slot available, claim not expired |
| **CLAIMED** | **assign-expire** | **OPEN** | **`now > claim.expires_at`** |
| CLAIMED | assign-complete | COMPLETED | Sender is claimant, `complete.timestamp < claim.expires_at` |
| **CLAIMED** | **assign-complete (late)** | **CLAIMED (no-op, rejected)** | **`complete.timestamp >= claim.expires_at`; completion is dropped** |
| COMPLETED | assign-accept | ACCEPTED | Operator validates work |
| COMPLETED | assign-reject | OPEN | Operator rejects work, re-opens for reclaim |
| ACCEPTED | (payment) | PAID | Scrip bounty transferred |

### 4.3 Impact on `claimedAssigns`

When a claim expires:

1. The agent-to-assign binding in `claimedAssigns` is removed. The agent's claim slot is freed.
2. The `AssignRecord` fields are cleared: `ClaimantKey = ""`, `ClaimMsgID = ""`.
3. The record transitions back to `AssignOpen`.

This mirrors the existing reject-and-reopen path in `applyAssignReject`.

### 4.4 Impact on `ActiveAssigns`

The `ActiveAssigns` method must check `expires_at` on claimed records and treat expired claims as open:

```
For each record in assignsByEntry[entryID]:
  if record.Status == AssignClaimed AND now > record.ExpiresAt:
    treat as AssignOpen (return to caller as claimable)
  else:
    existing logic
```

Note: the lazy reap in `ActiveAssigns` does not mutate state — it returns the effective status. Actual state mutation happens when `applyAssignExpire` processes the engine-emitted expire message.

---

## 5. Implementation Guidance

### 5.1 AssignRecord Changes

Add to `AssignRecord`:

- `ClaimExpiresAt time.Time` — set from `expires_at` in the assign-claim payload, capped by the assign's `claim_timeout_minutes`.

### 5.2 State Methods

**`applyAssignClaim`:** Parse `expires_at` from the claim payload. Compute the ceiling: `msg.Timestamp + assign.claim_timeout_minutes`. Set `rec.ClaimExpiresAt = min(parsed_expires_at, ceiling)`. If `expires_at` is missing, use the ceiling.

**`applyAssignExpire` (new):** Validate: antecedent references a known claim, record is in `AssignClaimed`, sender is operator. Clear claimant fields, remove `claimedAssigns` binding, transition to `AssignOpen`.

**`applyAssignComplete`:** Add check: if `rec.ClaimExpiresAt` is non-zero and `now > rec.ClaimExpiresAt`, drop the message (the claim expired). Note: this is a safety net — the engine should have already emitted `assign-expire` and the record should already be in `AssignOpen`. But message ordering is not guaranteed, so the check is defensive.

### 5.3 Engine Expiry Detection

Add to the engine's poll cycle:

```
for each record in state where Status == AssignClaimed:
  if now > record.ClaimExpiresAt:
    emit exchange:assign-expire message
    (state.Apply will process it and reopen the task)
```

Frequency: every poll cycle (same cadence as message processing). No separate goroutine needed.

### 5.4 Convention Declaration

New file: `docs/convention/exchange-core/assign-expire.json`

```json
{
  "convention": "dontguess-exchange",
  "version": "0.1",
  "operation": "assign-expire",
  "description": "Expire a claim that exceeded its TTL. Emitted by the exchange operator when a claimed assignment's expires_at has passed without an assign-complete from the claimant. The assignment returns to open state for re-claim by other agents.",
  "signing": "campfire_key",
  "antecedents": "exactly_one(target)",
  "payload_required": true,
  "args": [
    {
      "name": "target",
      "type": "message_id",
      "required": true,
      "description": "Message ID of the exchange:assign-claim that expired."
    },
    {
      "name": "assign_id",
      "type": "string",
      "required": true,
      "description": "Message ID of the original exchange:assign message."
    },
    {
      "name": "claimant",
      "type": "string",
      "required": true,
      "description": "Hex-encoded public key of the agent whose claim expired."
    },
    {
      "name": "expired_at",
      "type": "string",
      "required": true,
      "description": "ISO 8601 timestamp — the claim's original expires_at value."
    }
  ],
  "produces_tags": [
    {
      "tag": "exchange:assign-expire",
      "cardinality": "exactly_one"
    }
  ]
}
```

---

## 6. Adversarial Analysis

### 6.1 Attacks on the Expiry Mechanism

| # | Attack | Severity | Mitigation | Status |
|---|--------|----------|------------|--------|
| X1 | Claim squatting with rapid re-claim — agent claims, lets it expire, immediately re-claims | Medium | Consecutive timeout cooldown (D4): 3 consecutive timeouts = 1 hour ineligible. Rate limit: 10 claims/hour/key. After 3 claim-expire cycles (45 minutes), the agent is locked out for an hour. | Mitigated |
| X2 | Race condition: two agents try to claim an expiring task simultaneously | Low | State is single-writer (operator processes messages sequentially). The first valid claim wins. The second is rejected because the task is no longer in AssignOpen. Campfire message ordering resolves ties. | By design |
| X3 | Agent submits complete just before expiry, operator processes it just after | Medium | Strict timestamp comparison: `complete.timestamp < claim.expires_at`. The complete message's timestamp is set by the sender (TAINTED), but the operator's `assign-expire` is the authoritative expiry event. If the operator has already emitted `assign-expire`, the complete is rejected regardless of its claimed timestamp. If the complete arrives first, it is valid. | Mitigated |
| X4 | Operator delays `assign-expire` to keep tasks locked for preferred agents | Low | `assign-expire` is auditable on the campfire log. Monitoring can detect systematic delays between `expires_at` and `detected_at`. But the operator controls the engine, so this is an operator trust issue, not a protocol issue. Cross-operator federation (future) provides the competitive pressure. | Acknowledged |
| X5 | Sybil agents cycle claims to prevent legitimate agents from claiming | Medium | Same mitigations as A4 and A5 in assign.md: 3-open-claim cap, 10 claims/hour/key rate limit, campfire membership required, consecutive timeout cooldown. A sybil network with N keys can block N*3 tasks for 15 minutes — but the cost is N campfire memberships and the tasks reopen after 15 minutes. | Mitigated |

### 6.2 Interaction with Convergence Tasks

For convergence tasks (validate, freshen), 3 slots need to be filled. If one agent's claim expires:

- Only that one slot reopens. The other two claims are unaffected.
- A new agent can claim the reopened slot.
- If all 3 slots expire, all 3 reopen — the task effectively restarts.
- The assignment-level expiry (24 hours from creation) is the ultimate backstop. If no agents can complete the task within 24 hours, the assignment itself expires.

**Current implementation note:** The state machine currently tracks one claim per assign record (`ClaimantKey`, `ClaimMsgID`). Convergence tasks with 3 slots would need either 3 separate assign records (one per slot, all referencing the same parent assign message) or a multi-slot claim model. This is an existing gap in the state machine — claim expiry does not introduce it, but the expiry design must work with whichever multi-slot model is adopted.

---

## 7. Named View Updates

**`assignments:expired-claims`** — Recently expired claims (monitoring/audit):
```
(and
  (tag "exchange:assign-expire")
  (within "24h")
)
```

**Update `assignments:open`** — Must include tasks whose claims have expired:
```
(and
  (tag "exchange:assign")
  (not (has-fulfillment "exchange:assign-accept"))
  (not (expired))
  (or
    (claim-slots-available)
    (has-expired-claim)
  )
)
```

**Update `assignments:active`** — Must exclude expired claims:
```
(and
  (tag "exchange:assign-claim")
  (not (has-fulfillment "exchange:assign-complete"))
  (not (expired))
  (not (claim-expired))
)
```

---

## 8. Open Questions

1. **Multi-slot claims for convergence tasks.** The current `AssignRecord` is one-claim-per-record. Convergence tasks need 3 claims per assignment. The expiry mechanism works either way (per-claim `expires_at`), but the slot model needs to be resolved first. This is tracked separately.

2. **Clock skew.** The claimant's `expires_at` is based on their wall clock (TAINTED). The engine's expiry detection uses its own clock. If the claimant's clock is ahead, they might set `expires_at` in what is already the past from the engine's perspective, causing immediate expiry. Mitigation: the engine normalizes `expires_at` to `engine_receive_time + claim_timeout_minutes`, ignoring the claimant's timestamp entirely. This is consistent with the field classification: sender timestamps are tainted.

3. **Grace period for network latency.** Should there be a small grace period (e.g., 30 seconds) after `expires_at` before the engine emits `assign-expire`? This would accommodate agents whose `assign-complete` is in transit when the TTL fires. Decision: no. The TTL is already generous (15 minutes). Adding grace periods creates ambiguity about the actual deadline. The agent should submit well before the deadline, not at the last second.
