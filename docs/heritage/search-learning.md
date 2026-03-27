# Search Learning — Design Specification

**Domain:** Search Learning
**Status:** Approved design, ready for implementation
**Parent vision:** `docs/product-vision.md`

## Problem Statement

Manual dogfooding doesn't scale. Users don't report search failures — they reformulate or abandon. We need the system to identify quality problems automatically and feed them into the search improvement pipeline.

## Architecture

```
MCP Server → query log (JSONL)
                ↓
         quality analysis CLI
                ↓
         quality report (coverage gaps, reformulations, abandonments)
                ↓
         synonym extraction (reformulation pairs → synonym table)
                ↓
         bead pipeline (automated bug discovery → fix → verify)
```

## Component Specifications

### L1. Query Logging

**File:** `src/toolrank/mcp_server.py`

**Current:** MCP server processes requests with no logging.

**Target:** Append every `tools/call` dispatch to `data/query-log.jsonl`.

**Log entry format:**
```json
{
  "timestamp": "2026-03-11T12:34:56Z",
  "session_id": "uuid-generated-at-startup",
  "tool_name": "toolrank_search",
  "arguments": {"query": "python linter", "limit": 10},
  "result_count": 10,
  "top_result_ids": ["pypi/ruff", "brew/pylint", "pypi/flake8"]
}
```

**Implementation:**
- Generate `session_id` (UUID4) in `__init__`
- Add `_log_query()` method that appends JSON line to log file
- Call from `dispatch_tool()` after computing results
- Log file path configurable, default `data/query-log.jsonl`
- Log to file only, no stdout/stderr (would corrupt MCP protocol)

**Constraints:**
- Must not slow down request handling (append-only, no fsync)
- Must not crash server if log directory doesn't exist (create or skip)
- Must not log sensitive data (arguments are search queries, not secrets)

**Acceptance tests:**
- After 3 tool calls, log file has 3 lines
- Each line is valid JSON with required fields
- Session ID is consistent across all calls in one server lifetime
- Log file is created if it doesn't exist

### L2. Quality Analysis CLI

**File:** `bin/search-quality`

**Target:** Read `data/query-log.jsonl`, produce a quality report.

**Report sections:**

1. **Zero-result queries** — queries that returned 0 results. These are coverage gaps.
   ```
   COVERAGE GAPS (0 results):
     "terraform provider aws" — 3 occurrences
     "nmap" — 2 occurrences
   ```

2. **Reformulation chains** — sequential searches in same session within 60 seconds. The first query failed to satisfy.
   ```
   REFORMULATIONS:
     session abc123: "code checker" → "python linter" → "pylint"
     session def456: "k8s" → "kubernetes"
   ```

3. **Low-action searches** — searches with results but no follow-up `toolrank_score` or `toolrank_recommend` call within 120 seconds. Suggests results were irrelevant.
   ```
   ABANDONED SEARCHES (had results, no follow-up):
     "json parser" — 5 occurrences, avg 8 results
   ```

4. **Top queries** — most frequent queries, for prioritization.
   ```
   TOP QUERIES:
     "mcp server" — 12 occurrences
     "git" — 8 occurrences
   ```

5. **Synonym candidates** — reformulation pairs with 2+ occurrences.
   ```
   SYNONYM CANDIDATES:
     "checker" ↔ "linter" — 4 reformulations
     "k8s" ↔ "kubernetes" — 3 reformulations
   ```

**Implementation:**
- Python script, reads JSONL, groups by session_id
- Sorts events by timestamp within each session
- Applies heuristics for reformulation (< 60s gap, both are search calls)
- Applies heuristic for abandonment (search with results, no score/recommend within 120s)
- Outputs plain text report to stdout

**Acceptance tests:**
- Given a log with a reformulation chain, detects it
- Given a log with zero-result queries, reports them
- Given a log with abandoned searches, detects them
- Empty log produces empty report (no crash)

### L3. Query-Log Synonym Extraction

**File:** `src/toolrank/synonym_extractor.py` (extend)

**Depends on:** L1 (query logging), S2 (synonym system)

**Target:** Read reformulation pairs from quality analysis, promote to `data/synonyms.jsonl` when evidence threshold met.

**Implementation:**
- Read `data/query-log.jsonl`, detect reformulation pairs
- Count occurrences of each pair across all sessions
- When a pair has >= 3 occurrences, append to `data/synonyms.jsonl` with `source: "query-log"`
- Dedup against existing entries (don't add if already present)

**Acceptance tests:**
- Given 3 sessions each containing "checker" → "linter" reformulation, adds synonym entry
- Given 2 sessions (below threshold), does NOT add synonym entry
- Does not duplicate existing seed entries

### L4. Flywheel Convention Update

**File:** `CLAUDE.md`

**Target:** Update the Dev → Ops → Dogfood Flywheel section to include:

```
After dogfood:
1. Run `bin/search-quality` on the query log
2. Review quality report
3. File beads for top issues (coverage gaps → crawl bugs, reformulations → search bugs)
4. Run synonym extraction to grow the synonym table
5. Keep spinning
```

This closes the loop: dogfood generates logs → analysis generates beads → beads improve search → better results → fewer reformulations → remaining reformulations reveal new problems.
