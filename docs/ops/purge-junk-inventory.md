# Purge Junk Inventory — Operator Runbook

**Context:** dontguess-ed1 put quality-gate prevents future junk entries. This runbook purges the existing junk class identified in the measurement review §2 from the live exchange inventory.

**CRITICAL:** Do NOT execute this autonomously against `~/.cf`. This is a one-shot operator procedure. The agent that built the quality-gate code (dontguess-ed1) does NOT execute this — it documents it for the operator to run manually after reviewing the target entries.

---

## What to Purge

The junk class from measurement review §2:

- Entry description: `"test"` (exactly) — served **1,576** of 2,474 hits (64%)
- Entry description prefix: `"upgrade smoke test"` — "upgrade smoke test cf v0.31.2 operator"

Both are now rejected at the PUT layer by the quality gate. But existing entries that were accepted before the gate landed remain in inventory. They must be manually expired or deleted.

---

## Identification Step

Run against the live exchange to identify junk entry IDs:

```bash
# List all inventory entries and filter for junk class
dontguess list-inventory --json | jq -r '.[] | select(
    (.description == "test") or
    (.description | startswith("upgrade smoke test"))
  ) | [.entry_id, .description, .token_cost, .hit_count] | @tsv'
```

If `dontguess list-inventory` is not available, query the campfire directly:

```bash
# Read the exchange campfire and filter put-accept messages
cf <campfire-id> read --json | jq -r '.[] |
  select(.tags[] == "exchange:settle" and (.tags[] | startswith("exchange:phase:put-accept"))) |
  .payload | fromjson |
  # Filter for junk entries (requires correlating put_msg_id → description)
  .'
```

**Expected targets (from §2 measurement data):**
- 1 entry with `description == "test"`, `token_cost == 100`
- Potentially 1 entry with `description == "upgrade smoke test cf v0.31.2 operator"`, `token_cost == 100`

---

## Purge Procedure

The exchange does not have a hard-delete operation (campfire log is append-only). The mechanism is **immediate expiry via operator put-reject or expiry update**.

### Option A: Expire via `dontguess expire-entry` (preferred if CLI supports it)

```bash
# For each junk entry_id identified above:
dontguess expire-entry --entry-id <ENTRY_ID> --reason "junk inventory purge: dontguess-ed1"
```

This sets `expires_at` to now, making the entry invisible to match queries without modifying the campfire log.

### Option B: Operator settle message with expiry = now

If the CLI does not have `expire-entry`, send a settle(put-accept) override with `expires_at` in the past:

```bash
# For each junk entry_id (the put message ID), emit a settle with immediate expiry.
# This is operator-authorized — requires operator key.
ENTRY_ID="<the put message ID>"
NOW=$(date -u +%Y-%m-%dT%H:%M:%SZ)

cf <campfire-id> send \
  --tags "exchange:settle,exchange:phase:put-accept,exchange:verdict:expired" \
  --antecedents "$ENTRY_ID" \
  --payload "{\"price\": 0, \"expires_at\": \"$NOW\", \"reason\": \"junk purge dontguess-ed1\"}"
```

**Note:** `expires_at` must be a past timestamp to make the entry immediately invisible. The entry remains in the campfire log (append-only) but is excluded from `Inventory()` reads (which filter expired entries).

### Option C: State-layer forced expiry (emergency only)

If neither CLI method is available, the operator can force-expire via the operator API if the engine exposes it. This is an in-memory operation — does not persist across restarts unless backed by a campfire message.

---

## Verification

After running the purge:

```bash
# Confirm junk entries no longer appear in inventory
dontguess list-inventory --json | jq -r '.[] | select(
    (.description == "test") or
    (.description | startswith("upgrade smoke test"))
  ) | .entry_id'
# Expected: no output

# Confirm hit-rate metrics are recalculated without junk
dontguess hit-rate --json | jq '.synthetic_excluded, .quality_weighted_rate'
```

---

## Post-Purge Expected State

- **Inventory**: no entries with `description == "test"` or `startswith("upgrade smoke test")`
- **New puts**: quality gate (dontguess-ed1) prevents new junk from entering
- **Hit-rate**: quality-weighted metric (M-rebaseline item, dontguess-088) will show honest numbers
- **Match quality**: the 64% false-hit rate drops as junk entries expire

---

## Safety Notes

1. **Read-only first**: run the identification step and confirm the target entries before purging.
2. **One-at-a-time**: purge entries one at a time and verify after each.
3. **No bulk delete**: the campfire log is append-only; there is no bulk delete. Expiry is the only mechanism.
4. **Legitimate small entries**: do NOT purge entries with `token_cost < 200` that have legitimate descriptions (the quality gate now prevents new ones; existing ones may still be valid).
5. **Operator key required**: only the exchange operator can emit settle messages. Verify your operator key is configured: `cf whoami`.

---

*Written by: dontguess-ed1 implementer agent*  
*Date: 2026-06-02*  
*Tracked item: dontguess-ed1 (V2: junk inventory purged + put quality-gate)*
