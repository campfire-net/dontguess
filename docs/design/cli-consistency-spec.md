# CLI Consistency Spec — Eliminate Raw cf send/read Antipattern

## Problem

The `dontguess` wrapper routes convention operations through `cf <campfire-id> <operation> --flags`. This means `dontguess buy --task "..." --budget 5000` works today — cf reads the promoted convention declaration, builds typed flags from args, validates, and sends. No JSON construction, no manual tags.

Despite this, demos, README, website getting-started page, and terminal animations all show raw `cf send` with hand-built JSON payloads and `--tag` flags. AGENTS.md line 136 explicitly says "Do not use `cf send` / `cf read` directly" but nearly every other artifact violates this.

Additionally, the website docs pages reference a `dg` alias that doesn't exist and show flag names (`--task`, `--result`, `--tokens`) that don't match the convention declaration args (`--description`, `--content`, `--token-cost`).

## Design Principle

**Convention operations are the CLI.** The convention declarations in `docs/convention/exchange-core/*.json` define the args, types, validation, and tags. `cf` convention dispatch turns these into `--flags` at the CLI level automatically. Any place that constructs JSON payloads or manages tags manually is doing work the convention dispatcher already does.

The correct pattern for agent operations:

```bash
# Put
dontguess put \
  --description "Go rate limiter with Redis backend" \
  --content "$(base64 -w0 < result.go)" \
  --token-cost 2500 \
  --content-type code

# Buy
dontguess buy --task "rate limiter implementation in Go" --budget 5000
```

Note: `cf` convention dispatch supports enum short-forms. `--content-type code` expands to `exchange:content-type:code` if unambiguous. Agents don't need the full enum prefix.

The correct pattern for verification/reads uses named views (convention-declared read projections):

```bash
# Read settlements
dontguess settles

# Read matches
dontguess matches

# Read all messages
dontguess messages
```

Named views are convention-declared and routed identically to operations. If a named view doesn't exist for a needed read pattern, that's a convention gap to fill — not a reason to fall back to raw `cf read`.

## Scope

12 items organized in 4 waves. Each item is independently completable in one session.

---

## Wave 1: Demos (8 scripts)

Every demo script constructs JSON with `python3 -c "import json; ..."` or inline strings, then calls `cf send $XCFID "$PAYLOAD" --tag exchange:put ...`. All of these must be converted to `dontguess <operation> --flags`.

### How to convert

For each raw `cf send` in a demo:

1. Identify the operation from the tags (e.g., `--tag exchange:put` → `put`)
2. Map JSON payload fields to convention declaration args (read the matching `.json` file in `docs/convention/exchange-core/`)
3. Replace with `dontguess <operation> --flags`
4. For operations sent by a non-operator identity, prefix with `CF_HOME=$AGENT_CF` (same as today)

For each raw `cf read` used for verification:

1. Identify what's being read from the `--tag` filter
2. Replace with the corresponding named view: `dontguess settles`, `dontguess matches`, etc.
3. If a named view doesn't exist for the needed filter, note it as a convention gap (do NOT fall back to `cf read`)

**Legitimate `cf` usage to keep:**
- `cf init` — identity creation (campfire operation, not exchange)
- `cf id --json` — get public key (campfire operation)
- `cf admit` — admit member to campfire (campfire operation)
- `cf join` — join campfire (campfire operation, but `dontguess join` wrapper also works)

### Item 1.1: Demo 01 (solo-operator)

**File:** `test/demo/01-solo-operator.sh`

| Line(s) | Current | Replacement |
|---------|---------|-------------|
| ~136-139 | `cf send $XCFID "$PAYLOAD" --tag exchange:put --tag exchange:content-type:code` | `dontguess put --description "..." --content "$CONTENT_B64" --token-cost 2500 --content-type code` |
| ~184 | `cf read $XCFID --all --tag exchange:settle` | `dontguess settles` (named view) |
| ~199 | `cf read $XCFID --all --tag exchange:settle` | `dontguess settles` |
| ~217-220 | `cf send $XCFID "$PAYLOAD" --tag exchange:buy --future` | `dontguess buy --task "..." --budget 5000` |
| ~235 | `cf read $XCFID --all --tag exchange:match` | `dontguess matches` |
| ~251 | `cf read $XCFID --all --tag exchange:match` | `dontguess matches` |
| ~286 | `cf read $XCFID --all` | `dontguess messages` |

Also remove all `python3 -c "import json; ..."` payload construction — the convention flags replace this entirely.

**Done condition:** Demo 01 runs end-to-end with zero `cf send` or `cf read` calls for exchange operations. Only `cf init` remains. All exchange operations use `dontguess <operation> --flags`. All reads use named views.

### Item 1.2: Demo 02 (agent-seller)

**File:** `test/demo/02-agent-seller.sh`

3 raw puts (~180-183, ~219-222, ~241-244) each with different content types. Replace all three with `CF_HOME=$SELLER_CF dontguess put --description "..." --content "$B64" --token-cost N --content-type code|analysis|data`. Replace `cf read` verification calls with named views. Keep `cf init`, `cf id`, `cf admit`, `cf join` (campfire operations).

**Done condition:** Zero `cf send`/`cf read` for exchange operations. 3 puts use convention CLI. All reads use named views.

### Item 1.3: Demo 03 (agent-buyer)

**File:** `test/demo/03-agent-buyer.sh`

1 raw put (~211-214), 1 raw buy (~292-295). Convert both. Replace reads with named views. Keep campfire operations.

**Done condition:** Zero `cf send`/`cf read` for exchange operations.

### Item 1.4: Demo 04 (multi-agent)

**File:** `test/demo/04-multi-agent.sh`

2 raw puts (~213-216, ~252-255), 1 raw buy (~354-357). Convert all. Replace reads with named views.

**Done condition:** Zero `cf send`/`cf read` for exchange operations.

### Item 1.5: Demo 05 (auto-accept)

**File:** `test/demo/05-auto-accept.sh`

5 raw puts (via `send_put` helper function and direct calls). Refactor the helper to use `dontguess put`. Replace reads.

**Done condition:** Zero `cf send`/`cf read` for exchange operations.

### Item 1.6: Demo 06 (residuals)

**File:** `test/demo/06-residuals.sh`

1 raw put (~212-215), 2 raw buys (~364-367, ~434-437), 2 raw scrip-mints (~242-244, ~260-262). The scrip-mint operations use `dontguess:scrip-mint` tags — these should use `dontguess mint --to <key> --amount N` (scrip convention operation). Replace all reads with named views.

**Done condition:** Zero `cf send`/`cf read` for exchange operations.

### Item 1.7: Demo 07 (assign-work)

**File:** `test/demo/07-assign-work.sh`

1 raw put (~194-197), 4 raw assign operations (~313-317 assign, ~344-347 assign-claim, ~375-379 assign-complete, ~402-405 assign-accept). All should use convention CLI: `dontguess assign --entry-id ... --task-type validate --bounty N ...`, `dontguess assign-claim --target <msg-id> --expires-at ...`, etc. Replace reads.

**Done condition:** Zero `cf send`/`cf read` for exchange operations.

### Item 1.8: Demo 08 (hosted-multi-machine)

**File:** `test/demo/08-hosted-multi-machine.sh`

1 raw put (~242-245), 1 raw buy (~344-347). Convert both. Replace reads. Keep campfire operations (hosted identity setup). Note: this demo uses `--remote https://mcp.getcampfire.app` for hosted transport — the `dontguess` wrapper should handle this transparently since it passes through to `cf`.

**Done condition:** Zero `cf send`/`cf read` for exchange operations.

---

## Wave 2: README and AGENTS.md

### Item 2.1: README.md

**File:** `README.md`

**Section "4. Seller: put cached inference" (lines 78-113):**
Replace entire `cf send` block with:
```bash
dontguess put \
  --description "Go rate limiter with Redis backend — sliding window, pipeline ops" \
  --content "$(base64 -w0 < rate_limiter.go)" \
  --token-cost 2500 \
  --content-type code
```
Remove the `python3 -c "import json; ..."` payload construction.
Replace `cf read $XCFID --all --tag exchange:settle` with `dontguess settles`.

**Section "5. Buyer: search before computing" (lines 115-138):**
Replace `cf send` buy with:
```bash
dontguess buy --task "rate limiter implementation in Go" --budget 5000
```
Remove JSON payload construction.
Replace `cf read $XCFID --all --tag exchange:match` with `dontguess matches`.

**Done condition:** README shows only convention CLI for exchange operations. No `cf send` or `cf read` for exchange operations. `cf init` at line 36 stays (identity creation is a campfire operation).

### Item 2.2: AGENTS.md quick start

**File:** `AGENTS.md`

Lines 63-64 show `cf join <campfire-id>` as an alternative — replace with `dontguess join <campfire-id>` to be consistent (the wrapper already handles this). The main agent workflow examples at lines 15-31 and 76-85 already use the convention CLI correctly. Verify the flag names match current convention args.

Also update line 122: `test/demo/` description says "(01-04)" but there are now 8 demos (01-08).

**Done condition:** No raw `cf` commands shown for exchange operations. Demo count accurate.

---

## Wave 3: Website

### Item 3.1: Getting-started page

**File:** `site/docs/getting-started.html`

**Lines 391-409 (put tutorial):**
Replace the `cf send` block with convention CLI. The `<div class="cmd-header">` currently says "cf send — put cached inference on the exchange" — change to "dontguess put — sell cached inference". Replace all `cf send`/`cf read` command lines within.

**Lines 457-476 (buy tutorial):**
Replace `cf send` buy block with convention CLI. Change header from "cf send — buy" to "dontguess buy — search before computing". Replace `cf read` with named view.

**Done condition:** Getting-started tutorial shows only convention CLI for exchange operations. Keep `cf init` for identity setup.

### Item 3.2: Terminal animation

**File:** `site/terminal.js`

**Solo demo (lines 23-24):** Replace `cf send $CAMPFIRE_ID '{"description":"Go HTTP handler..."}\' --tag exchange:put --tag exchange:content-type:code` with `dontguess put --description "Go HTTP handler..." --content "$(base64 -w0 < handler.go)" --token-cost 2000 --content-type code`

**Solo demo (lines 34-35):** Replace `cf send` buy with `dontguess buy --task "Go HTTP handler for POST JSON" --budget 5000`

**Solo demo (line 39):** Replace `cf read ... --tag exchange:match` with `dontguess matches`

**Multi-agent demo (lines 62-63, 65-66):** Replace both `cf send` puts with `CF_HOME=$SELLER_CF dontguess put ...`

**Multi-agent demo (lines 76-77):** Replace `cf send` buy with `CF_HOME=$BUYER_CF dontguess buy ...`

**Multi-agent demo (line 79):** Replace `cf read` with `dontguess matches`

**Multi-agent demo (line 86):** Replace `cf read $XCFID --all --json | python3 -c "..."` with `dontguess messages --json | python3 -c "..."`

**Done condition:** Terminal animations show only convention CLI. No `cf send` or `cf read`.

---

## Wave 4: Cleanup

### Item 4.1: Website flag names and `dg` alias

**Files:** `site/docs/cli.html`, `site/docs/index.html`, `site/docs/compression.html`, `site/docs/exchange-operations.html`, `site/docs/task-marketplace.html`, `site/docs/benefits.html`

All these pages show `dg` as the command name and use aspirational flag names that don't match convention args:

| Website flag | Convention arg | Action |
|---|---|---|
| `dg` (command) | `dontguess` | Replace all `dg` with `dontguess` |
| `--task` (on put) | `--description` | Replace |
| `--result ./file` | `--content "$(base64 -w0 < ./file)"` | Replace |
| `--tokens N` | `--token-cost N` | Replace |
| `--max-price N` (on buy) | `--budget N` | Replace |
| `--preview` (on buy) | N/A (preview is a settle phase) | Remove or show as separate settle operation |
| `--output ./file` (on buy) | N/A | Remove |
| `dg assign list --open` | `dontguess assigns` (named view, if it exists) | Replace |
| `dg assign bid --amount` | N/A (no bid operation) | Remove or note as future |
| `dg preview <id>` | `dontguess settle --target <id> --phase preview-request --entry-id <id>` | Replace with correct convention form |

Also fix: `dg settle --transaction dg-tx-9f3b --verdict success` → `dontguess settle --target <msg-id> --phase complete --entry-id <id> --verdict accepted --content-hash-verified true`

**Done condition:** All website docs pages show `dontguess` (not `dg`) with correct convention arg names. Every command shown on the website can be copy-pasted and executed.

### Item 4.2: GitHub links, stale put.json, version, escrow timing, federation label

**GitHub links:** All docs pages link to `github.com/3dl-dev/dontguess`. Landing page and install script use `github.com/campfire-net/dontguess`. All must be consistent — use whichever is the actual repo location.

Files to fix: `site/docs/reputation.html`, `site/docs/scrip.html`, `site/docs/getting-started.html`, `site/docs/benefits.html`, `site/docs/index.html`, `site/docs/federation.html`, `site/docs/cli.html`, `site/docs/compression.html`, `site/docs/matching.html`, `site/docs/task-marketplace.html`, `site/docs/exchange-operations.html`, `site/docs/pricing.html`

**Stale put.json:** `docs/convention/exchange-core/put.json` is v0.1 (requires `content_hash`, `content_size`). Engine expects v0.3 (inline `content`, engine-computed hash). The active declaration is `put-v0.3.json` (highest version wins during promotion). Either rename `put-v0.3.json` → `put.json` (replacing the stale one), or delete the stale `put.json` and rename `put-v0.2.json` to indicate it's historical.

**Wrapper version:** `site/install.sh` generates v0.4.2, repo root `dontguess` is v0.4.0. Sync to the same version.

**Scrip escrow timing:** `site/index.html` line 150 says escrow happens at buy-time ("escrows the buyer's scrip via `scrip:buy-hold`"). Convention spec says escrow happens at `settle(buyer-accept)`. Fix the website text.

**Federation label:** `site/index.html` lines 355-360 mark Enterprise/Global as "roadmap" with a badge. Federation has 7 convention declarations, a trust model in code, and a working demo (08). Remove the "roadmap" badge or change to "beta" / "preview".

**Done condition:** Consistent GitHub URLs across all pages. No stale convention declarations with canonical names. Wrapper version synced. Website economics text matches convention spec. Federation label reflects implementation state.

---

## Named View Gaps

The demos and docs need named views for reads. If these views don't exist in the promoted conventions, they need to be declared. Check what exists:

- `settles` — read settle messages
- `matches` — read match messages  
- `messages` — read all messages
- `buys` — read buy messages (already referenced in wrapper health probe, line 148)
- `assigns` — read assign messages
- `inventory` — read accepted puts (active inventory)

The wrapper already uses `buys` as a named view in the health probe (`"$CF" "$XCFID" buys --json`), confirming named views work through the convention dispatch path.

---

## Test Strategy

Each demo script change must pass:
1. The demo runs end-to-end without errors
2. The demo produces the same exchange state (same number of puts accepted, matches returned, etc.)
3. Zero `cf send` or `cf read` commands for exchange operations (grep test)
4. `cf init`, `cf id`, `cf admit`, `cf join` are the only remaining raw `cf` calls

For website changes: visual review that commands shown are copy-pasteable and match convention args.

---

## Constraints

- Do NOT change convention declarations to match the website's aspirational flag names. The convention declarations are the source of truth. The website adapts to them, not the other way around.
- Do NOT add a `dg` symlink or alias. Use `dontguess` as the canonical command name.
- Do NOT introduce new wrapper subcommands (e.g., `dontguess assign list`). If the convention CLI doesn't support a workflow shown on the website, fix the website to show the correct convention form or note it as future work.
- Do NOT modify the `dontguess` wrapper script, `dontguess-operator` binary, or any Go code. This is a documentation/demo consistency pass only.
- If a named view needed for a read replacement doesn't exist, create the view declaration in `docs/convention/exchange-core/` following the existing view declaration format. Do NOT fall back to `cf read`.
