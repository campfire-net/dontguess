# buy — Delivery Guarantee (Insurance / Actuarial)

**Version:** 0.1
**Status:** Draft
**Date:** 2026-04-01
**Depends on:** exchange-core/buy.json, exchange-core/settle.json, exchange-scrip/buy-hold.json

---

## Overview

Buyers may purchase a **delivery guarantee** on any buy request. The guarantee promises delivery before a deadline. If the exchange misses the deadline, the buyer receives a full automatic refund — no dispute required.

This feature is opt-in. Buy requests without `guarantee_deadline_seconds` are not insured.

---

## Buy Payload Fields

Two optional fields on `exchange:buy` activate the guarantee:

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `guarantee_deadline_seconds` | integer | No | Seconds from buy-receive time within which the exchange guarantees delivery. Must be > 0. Converts to an absolute `GuaranteeDeadline` at buy time. |
| `insured_amount` | integer | No | Total scrip escrowed for the insured order (match price + insurance premium), in micro-tokens. Must be > 0 when `guarantee_deadline_seconds` is set. Used as the full refund amount on deadline miss. |

If `guarantee_deadline_seconds` is set but `insured_amount` is zero, the guarantee is treated as unset (no deadline is recorded).

**Example insured buy payload:**

```json
{
  "task": "find cached code for async retry with exponential backoff",
  "budget": 1500,
  "guarantee_deadline_seconds": 600,
  "insured_amount": 1000
}
```

---

## Guarantee Lifecycle

### 1. Buy received

State records `GuaranteeDeadline = receive_time + guarantee_deadline_seconds` and `InsuredAmount` on the `ActiveOrder`.

### 2. Match emitted

State copies the guarantee terms into `matchGuarantee[matchMsgID] = [deadline_unix_ns, insured_amount]`. This persists after the order leaves `activeOrders` so the settle handler can check the deadline even after buyer-accept.

### 3. Settle(complete) received

The engine checks `GuaranteeForMatch(matchMsgID)`:

- **Deadline met** (`now <= GuaranteeDeadline`): Normal settlement proceeds. Worker is paid. Exchange revenue is retained.
- **Deadline missed** (`now > GuaranteeDeadline`): `handleDeadlineMissRefund` is called.

### 4. Deadline-miss auto-refund

When the deadline is missed:

1. A `scrip:dispute-refund` is emitted for the full `insured_amount` (match price + premium).
2. The buyer's escrow is released immediately — no dispute needed.
3. The worker is **not penalized**. If the assign was completed (work delivered), the worker's `assign-pay` was already handled separately.
4. The exchange absorbs the loss.

The refund message uses `reason="deadline-miss"` in the dispute-refund payload (distinguishable from buyer-dispute refunds in the scrip log).

---

## Actuarial Table

The slow loop uses `AssignCompletionSamples()` to build per-task-type latency statistics. These feed the actuarial table that the operator uses to price guarantees:

| Signal | Description |
|--------|-------------|
| `ClaimToCompleteLatency` | `CompletedAt - ClaimedAt` per assign. Measures worker delivery speed. |
| `FillRate` | `Completed / Total` per task_type. Measures how often work is actually delivered. |

The operator sets `guarantee_deadline_seconds` pricing (the premium embedded in `insured_amount`) based on the actuarial table — higher fill rates and faster delivery allow cheaper guarantees.

---

## Security Notes

- Buyers cannot manufacture deadline misses by delaying settlement messages. The deadline is set at buy-receive time (operator wall clock). Workers have no mechanism to force a deadline miss.
- The guarantee does not apply to disputes from content quality issues — only to delivery latency. Quality disputes use the standard `scrip:dispute-refund` path.
- `matchGuarantee` is not reset on replay. If the engine restarts after a match but before a settle, the guarantee data is re-derived from the campfire log during replay (via `applyMatch`).
