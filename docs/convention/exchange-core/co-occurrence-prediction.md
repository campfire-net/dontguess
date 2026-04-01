# Co-Occurrence Prediction — Engine Behavior

**Version:** 0.1
**Status:** Draft
**Date:** 2026-04-01
**Depends on:** exchange-core/assign.json, exchange-core/buy-brokered-match.md

---

## Overview

The exchange engine tracks which cache entries are purchased together within a single buyer session. This co-occurrence signal feeds a prediction model that pre-stages standing brokered-match assigns for entries likely to be requested next.

Co-occurrence prediction is **engine behavior** — not a campfire operation. It does not produce a new message type. It produces standing `exchange:assign` messages with `task_type="brokered-match"` as a side effect of `settle(complete)` events.

---

## Session Window

The engine maintains a per-buyer session window: `buyerRecentEntries[buyerKey]` — a bounded list (max 10 entries) of `(entryID, settledAt)` tuples. Entries older than `buyerSessionWindow` (currently 30 minutes) are pruned on each update.

---

## UpdateCoOccurrence

On each `settle(complete)`:

1. `recordBuyerSettlement(buyerKey, entryID)` is called.
2. The current session window (after pruning stale entries) is iterated.
3. For each prior entry in the window, `state.UpdateCoOccurrence(prior, current)` is called. This increments the co-occurrence count for the `(prior, current)` pair in the in-memory `coOccurrence` map.
4. The settled entry is appended to the session window.

`coOccurrence` is reset on engine `Replay()` and re-derived from the settle log. It is not persisted to the campfire — it is a derived in-memory signal.

---

## StalePredictionAssigns

`state.StalePredictionAssigns()` returns the assign IDs of `AssignOpen` brokered-match assigns whose `DeadlineAt` is non-zero and has passed. These are prediction assigns that expired before any worker claimed them.

The engine cancels stale prediction assigns by emitting `exchange:assign-expire` for each. This prevents stale prediction assigns from accumulating in the open assign set.

---

## stagePredictions

After recording the co-occurrence update, `stagePredictions(settledEntryID)` posts standing brokered-match assigns for the top predicted next-work entries:

1. The top co-occurrence neighbors of `settledEntryID` are read from the `coOccurrence` map (sorted by count descending).
2. For each candidate entry, the number of existing open prediction assigns is checked. If `existing >= MaxPredictionFanout` (3), the slot is skipped (A9 mitigation — prevents assign blowup from high-frequency settle events).
3. A standing brokered-match assign is posted with:

   | Field | Value |
   |-------|-------|
   | `entry_id` | The predicted next-work entry ID |
   | `task_type` | `"brokered-match"` |
   | `reward` | `brokeredMatchReward()` (operator-configured) |
   | `deadline_at` | `now + PredictionAssignTTL` (default 2 hours) |
   | `description` | `"Predicted next-work for entry <id> — brokered match standing offer"` |

Prediction assigns do NOT carry a `buy_msg_id` (no buy has arrived yet). If a matching buy arrives before the deadline, the worker's result is used. If the deadline passes without a buy, the assign is expired via `StalePredictionAssigns`.

---

## A9 Mitigation

`MaxPredictionFanout = 3` limits the standing prediction assigns per entry at any time. Without this cap, a high-velocity seller whose entry is frequently settled could generate unbounded open assigns, filling the assign set with speculative work.

---

## Coexistence with Reactive Brokered Matching

Prediction assigns and reactive brokered-match assigns (posted on actual buy arrivals) use the same task_type and worker flow. Workers see them identically. The distinction is:

| Assign type | Has `buy_msg_id` | Has `deadline_at` |
|-------------|------------------|-------------------|
| Reactive (on buy) | Yes | No |
| Prediction (pre-staged) | No | Yes (`PredictionAssignTTL`) |

The engine routes reactive buys to prediction assigns if a standing one exists for the predicted entry (via `BrokerAssignForBuy`). If no standing assign exists, a reactive assign is posted.
