# Exchange Federation — Trust Model

**Version:** 0.1
**Status:** Draft
**Date:** 2026-04-01
**Depends on:** exchange-federation/propose.json, exchange-federation/accept.json, core-operations.md

---

## Overview

Federation trust is a per-sender behavioral score computed by the slow loop (4-hour cadence). It applies to both local agents and remote federation nodes — the model is unified; only starting scores differ.

Trust scores gate brokered-match routing (see `exchange-core/buy-brokered-match.md`). They are advisory for all other operations.

---

## FederationNodeProfile

Each sender key seen on the campfire gets a `FederationNodeProfile`:

| Field | Type | Description |
|-------|------|-------------|
| `SenderKey` | string | Hex-encoded Ed25519 public key of the counterparty |
| `TrustScore` | float64 | Computed trust score in [0.0, 1.0]. Written by the slow loop via `SetFederationTrustScore`. |
| `HopDepth` | int | Median observed provenance hop depth (approximated from antecedents chain length). Advisory signal (F4). |
| `TransactionCount` | int | Total transactions observed from this sender. Used for new-node graduation. |
| `FirstSeenAt` | time.Time | Wall-clock time of first message from this sender. |

Profiles are **not** reset on engine replay. Trust scores survive restarts. `HopDepth` and `FirstSeenAt` are re-derived from the log on replay; `TrustScore` is retained from the last slow-loop write.

---

## Starting Trust Scores

| Sender Type | Starting TrustScore | Rationale |
|-------------|---------------------|-----------|
| New federation node | 0.4 (`NewNodeTrustScoreStart`) | Unknown behavioral history; conservative |
| Local agent | 0.7 (`LocalAgentTrustScoreStart`) | Known operator context; more trusted by default |

Both converge toward 1.0 as behavioral history accumulates. The slow loop reads observed signals (transaction success rate, dispute rate, hop depth consistency) and calls `SetFederationTrustScore` to update.

---

## HopDepth Signal

`HopDepth` is approximated from the length of a message's `Antecedents` chain. It is an advisory signal used by the slow loop to estimate how deep in a provenance graph a sender typically operates.

- `trackSenderHopDepth` is called for every message from a non-empty sender.
- Observations are windowed: at most `SenderHopDepthWindowSize` (1000) per sender.
- `HopDepth` is set to the median of the observation window after each update.
- High hop depth (many antecedents) suggests deeply coordinated work; low hop depth suggests standalone messages. Neither is inherently good or bad — it is an input to the slow loop's trust computation, not a direct gate.

---

## New-Node Graduation

A node is considered "new" until BOTH conditions are satisfied:

1. `TransactionCount >= NewNodeTransactionThreshold` (10 transactions)
2. Age since `FirstSeenAt >= NewNodeAgeDuration` (30 days)

Until graduation, the node is subject to the new-node dual guard: brokered routing is blocked regardless of `TrustScore` when `FederationGuardEnabled=true`.

---

## New-Node Dual Guard (§4A)

When `FederationGuardEnabled=true` in `EngineOptions`, `handleBuy` checks the sender's `TrustScore` before routing to brokered matching:

```
if TrustScore < NewNodeTrustThreshold (0.6):
    route to inline matching
    log: "federation guard: sender=<key> low trust, routing inline"
```

This limits exposure from new or distant nodes that have not yet established sufficient behavioral history. The guard is opt-in (`FederationGuardEnabled=false` by default) to preserve backward compatibility with single-operator deployments.

**Enabling `BrokeredMatchMode` without `FederationGuardEnabled`** logs a startup warning:
```
engine: WARN BrokeredMatchMode enabled but FederationGuardEnabled=false —
federation nodes bypass trust guard; set FederationGuardEnabled=true in production
```

---

## Threshold Reference

| Constant | Value | Description |
|----------|-------|-------------|
| `NewNodeTrustThreshold` | 0.6 | TrustScore below which brokered routing is blocked |
| `NewNodeTrustScoreStart` | 0.4 | Starting score for new federation nodes |
| `LocalAgentTrustScoreStart` | 0.7 | Starting score for local agents |
| `NewNodeTransactionThreshold` | 10 | Minimum transactions to graduate from new-node status |
| `NewNodeAgeDuration` | 30 days | Minimum age to graduate from new-node status |
| `SenderHopDepthWindowSize` | 1000 | Max hop-depth observations retained per sender |
