# buy — BrokeredMatchMode Routing

**Operation:** `exchange:buy` (variant — brokered routing)
**Version:** 0.1
**Status:** Draft
**Depends on:** exchange-core/buy.json, exchange-core/assign.json

---

## Overview

`exchange:buy` has two routing modes controlled by the operator's `BrokeredMatchMode` engine option:

| Mode | Routing | Default |
|------|---------|---------|
| **Inline** (`BrokeredMatchMode=false`) | Engine runs semantic similarity search synchronously, emits `exchange:match` directly | Yes |
| **Brokered** (`BrokeredMatchMode=true`) | Engine posts an `exchange:assign` with `task_type="brokered-match"`, workers deliver ranked results | No |

The `exchange:buy` message schema (see `buy.json`) is identical in both modes. The routing decision is operator-side and invisible to the buyer at the protocol level.

---

## Brokered-Match Assign Payload

When `BrokeredMatchMode=true`, `handleBuy` posts an `exchange:assign` with the following payload fields:

| Field | Type | Description |
|-------|------|-------------|
| `task_type` | string | Always `"brokered-match"` |
| `buy_msg_id` | string | Message ID of the originating buy. Used to correlate the assign back to the order. |
| `task_description` | string | The buyer's task description (from `buy.task`). Workers use this to search inventory. |
| `max_results` | integer | Maximum ranked results to return (from `buy.max_results`). |
| `reward` | integer | Scrip reward in micro-tokens paid on assign-complete. Defaults to `BrokeredMatchDefaultReward` (100 scrip = 100,000,000 micro-tokens) unless operator configures `BrokeredMatchReward`. |

The assign is sent with the buy message ID as its antecedent. Workers call the standard `exchange:assign-claim`, `exchange:assign-complete` flow to deliver results.

---

## Co-Occurrence Prediction: Standing Brokered-Match Assigns

The engine also posts standing brokered-match assigns **before** a buy arrives, using co-occurrence prediction:

1. On each `settle(complete)`, `recordBuyerSettlement` is called for the buyer's key.
2. For each prior entry the buyer purchased in the same session window, `UpdateCoOccurrence(prev, current)` is called on state.
3. After the co-occurrence map is updated, `stagePredictions(settledEntryID)` posts up to `MaxPredictionFanout` (3) standing assigns for the top predicted next-work entries.
4. These assigns carry `deadline_at` (default `PredictionAssignTTL` = 2 hours). `StalePredictionAssigns()` returns overdue prediction assigns for cancellation.

Standing prediction assigns use the same brokered-match payload schema as reactive assigns. They differ only in having a non-zero `deadline_at` and no `buy_msg_id` (the buy has not yet arrived).

---

## Federation Guard (New-Node Dual Guard)

When both `BrokeredMatchMode=true` and `FederationGuardEnabled=true` are set, a sender whose `TrustScore` is below `NewNodeTrustThreshold` (0.6) is routed to **inline matching** regardless of the operator setting. This is the new-node dual guard (design §4A):

- New federation nodes start at `TrustScore = 0.4` (`NewNodeTrustScoreStart`).
- Local agents start at `TrustScore = 0.7` (`LocalAgentTrustScoreStart`).
- Both converge toward 1.0 via behavioral history written by the slow loop.
- Until `TrustScore >= 0.6`, brokered routing is blocked. This limits exposure from nodes that have not yet established behavioral history.

`FederationGuardEnabled` defaults to `false` for single-operator deployments. Enabling `BrokeredMatchMode` without `FederationGuardEnabled` logs a startup warning.

---

## Operator Configuration Reference

| EngineOptions Field | Type | Default | Description |
|---------------------|------|---------|-------------|
| `BrokeredMatchMode` | bool | `false` | Enable brokered routing path |
| `BrokeredMatchReward` | int64 | `BrokeredMatchDefaultReward` (100 scrip) | Reward for each brokered-match assign |
| `FederationGuardEnabled` | bool | `false` | Activate new-node trust guard |
