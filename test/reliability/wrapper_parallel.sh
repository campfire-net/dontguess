#!/bin/sh
# test/reliability/wrapper_parallel.sh — parallel wrapper reliability harness (dontguess-9b6)
#
# Spawns N (default 50) concurrent dontguess buy calls across 4 context rotations,
# then aggregates results and exits 0 iff >=95% reached the exchange with zero
# hard-failure classes.
#
# Usage:
#   bash test/reliability/wrapper_parallel.sh [N]
#
# Requirements:
#   dontguess, cf, jq, python3 installed
#   Exchange configured at ~/.cf/dontguess-exchange.json (or $DG_HOME/dontguess-exchange.json)
#
# Contexts rotated (0-indexed mod 4):
#   0  default          — no env override
#   1  worktree-cfhome  — CF_HOME=$(mktemp -d)
#   2  session-cfhome   — CF_HOME=/tmp/cf-session-test-$i
#   3  warm-operator    — operator already running from prior calls

set -eu

N="${1:-50}"

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
OUT_DIR="${SCRIPT_DIR}/out"

DG_HOME_DEFAULT="${HOME}/.cf"
DG_HOME="${DG_HOME:-${DG_HOME_DEFAULT}}"
DG="${HOME}/.local/bin/dontguess"
CF="${HOME}/.local/bin/cf"

# Unique suffix for this run so we can filter campfire buy log precisely
RUN_SUFFIX="$(date +%s%N)"

# Get exchange campfire ID (always from DG_HOME, not CF_HOME)
XCFID=$(jq -r .exchange_campfire_id "${DG_HOME}/dontguess-exchange.json")

# ---------------------------------------------------------------------------
# Fix (25b): Clean out/ at the TOP of every run, regardless of prior outcome.
# This prevents stale log files from a previous failed run from contaminating
# the aggregator's classification.
# ---------------------------------------------------------------------------
rm -rf "$OUT_DIR" && mkdir -p "$OUT_DIR"

# ---------------------------------------------------------------------------
# Setup / cleanup helpers
# ---------------------------------------------------------------------------

kill_operator() {
  pkill -f dontguess-operator 2>/dev/null || true
  i=0
  while pgrep -f dontguess-operator >/dev/null 2>&1 && [ "$i" -lt 50 ]; do
    sleep 0.1; i=$((i+1))
  done
}

cleanup_stale_state() {
  rm -f "${DG_HOME}/dontguess.pid" "${DG_HOME}/dontguess.start.lock"
}

cleanup_logs() {
  rm -f "${OUT_DIR}"/run-*.log
}

# ---------------------------------------------------------------------------
# Step 1: kill any running operator + clean stale state
# ---------------------------------------------------------------------------
printf "=== wrapper_parallel.sh: N=%s run_suffix=%s ===\n" "$N" "$RUN_SUFFIX"
printf "Exchange:  %s\n" "$XCFID"
printf "DG_HOME:   %s\n" "$DG_HOME"

printf "\n[setup] Killing any running operator...\n"
kill_operator
cleanup_stale_state
printf "[setup] Done.\n"

# ---------------------------------------------------------------------------
# Step 2: spawn N concurrent buys
# ---------------------------------------------------------------------------
printf "\n[run] Spawning %s concurrent buys...\n" "$N"

# We spawn each buy as a background subshell.
# The subshell resolves its own CF_HOME for worktree-cfhome context.
i=1
while [ "$i" -le "$N" ]; do
  ctx_idx=$(( (i - 1) % 4 ))
  task="harness-${i}-ctx${ctx_idx}-${RUN_SUFFIX}"
  log_file="${OUT_DIR}/run-${i}.log"

  (
    case "$ctx_idx" in
      0)
        # default: unset CF_HOME so wrapper uses its built-in default
        # Fix (0ed): wrap in timeout 30 so a hung buy doesn't block wait indefinitely.
        # Fix (7ab): capture real exit code rather than always writing exit:0.
        timeout 30 env -u CF_HOME "$DG" buy --task "$task" --budget 100 >"$log_file" 2>&1
        dg_exit=$?
        ;;
      1)
        # worktree-cfhome: CF_HOME = fresh mktemp dir
        # Fix (e78): trap ensures temp dir is removed on EXIT, even on timeout/error.
        wt_dir=$(mktemp -d)
        trap 'rm -rf "$wt_dir" 2>/dev/null || true' EXIT
        # Fix (0ed): per-call timeout.
        timeout 30 env CF_HOME="$wt_dir" "$DG" buy --task "$task" --budget 100 >"$log_file" 2>&1
        dg_exit=$?
        ;;
      2)
        # session-cfhome: CF_HOME = /tmp/cf-session-test-$i
        # Fix (0ed): per-call timeout.
        timeout 30 env CF_HOME="/tmp/cf-session-test-${i}" "$DG" buy --task "$task" --budget 100 >"$log_file" 2>&1
        dg_exit=$?
        ;;
      3)
        # warm-operator: no env override (operator already warm from earlier rounds)
        # Fix (0ed): per-call timeout.
        timeout 30 env -u CF_HOME "$DG" buy --task "$task" --budget 100 >"$log_file" 2>&1
        dg_exit=$?
        ;;
    esac
    # Fix (0ed): if timeout fires, exit code is 124 — record it faithfully.
    # Fix (7ab): write real exit code, not hardcoded 0.
    printf "exit:%d\n" "$dg_exit" >> "$log_file"
  ) &

  i=$((i+1))
done

printf "[run] Waiting for all %s calls to complete...\n" "$N"
wait
printf "[run] All calls finished.\n"

# Clean up session-cfhome dirs
i=1
while [ "$i" -le "$N" ]; do
  rm -rf "/tmp/cf-session-test-${i}" 2>/dev/null || true
  i=$((i+1))
done

# ---------------------------------------------------------------------------
# Step 3: Python aggregator — classify + cross-reference campfire
# ---------------------------------------------------------------------------
printf "\n[aggregate] Fetching campfire buy log and classifying...\n"

python3 - "$OUT_DIR" "$N" "$RUN_SUFFIX" "$XCFID" <<'PYEOF'
import sys, os, json, re, subprocess, math

out_dir  = sys.argv[1]
n        = int(sys.argv[2])
suffix   = sys.argv[3]
xcfid    = sys.argv[4]

# Fix (8c1): Fetch campfire buys — ground truth for "reached exchange".
# On failure, emit a clear diagnostic and abort. The harness must NOT silently
# degrade its ground-truth check.
# Fix (bae): cf read has no --limit flag. Filter client-side by the run-specific
# task suffix (harness-$i-ctx$ctx_idx-$RUN_SUFFIX) which is unique per run.
try:
    cf_path = os.path.expanduser("~/.local/bin/cf")
    raw = subprocess.check_output(
        [cf_path, xcfid, "buys", "--json"],
        stderr=subprocess.PIPE,
    )
    buys = json.loads(raw)
except Exception as e:
    print(f"ERROR: cf buys query failed: {e}", file=sys.stderr)
    sys.exit(1)

# Build set of tasks that reached the exchange (filter by run suffix — unique per run)
reached_tasks = set()
for msg in buys:
    try:
        payload = json.loads(msg.get("payload", "{}"))
        task = payload.get("task", "")
        if suffix in task:
            reached_tasks.add(task)
    except Exception:
        pass

# Classify each log file
buckets = {
    "No exchange configured": 0,
    "operator not running":   0,
    "server failed":          0,
    "identity is wrapped":    0,
    "success":                0,
    "other":                  0,
}

per_run = {}  # i -> {task, ctx_idx, bucket, reached, exit_code}

for fname in sorted(os.listdir(out_dir)):
    m = re.match(r"run-(\d+)\.log", fname)
    if not m:
        continue
    i = int(m.group(1))
    fpath = os.path.join(out_dir, fname)
    with open(fpath, "r", errors="replace") as f:
        text = f.read()

    ctx_idx = (i - 1) % 4
    task = f"harness-{i}-ctx{ctx_idx}-{suffix}"

    # Fix (7ab): parse real exit code from the last line written by the subshell.
    exit_code = None
    for line in text.splitlines():
        em = re.match(r"exit:(\d+)", line)
        if em:
            exit_code = int(em.group(1))

    # Fix (bd2): expanded hard-failure patterns — transport-level failures ARE
    # server failures from the buyer's perspective. Added: connection refused,
    # connection reset, dial tcp, i/o timeout.
    SERVER_FAIL_PATTERNS = [
        "server failed",
        "server error",
        "connection refused",
        "connection reset",
        "dial tcp",
        "i/o timeout",
    ]

    # Classify by stderr/stdout content
    if "No exchange configured" in text:
        bucket = "No exchange configured"
    elif "operator not running" in text or "operator is not running" in text:
        bucket = "operator not running"
    elif any(p in text for p in SERVER_FAIL_PATTERNS):
        bucket = "server failed"
    elif "identity is wrapped" in text:
        bucket = "identity is wrapped"
    elif task in reached_tasks:
        bucket = "success"
    else:
        # Fix (7ab): also flag timeout exits (124) explicitly in "other" classification
        bucket = "other"

    # Ground truth override: campfire is authoritative
    if task in reached_tasks:
        bucket = "success"

    buckets[bucket] += 1
    per_run[i] = {"task": task, "ctx_idx": ctx_idx, "bucket": bucket,
                  "reached": task in reached_tasks, "exit_code": exit_code}

# reached_exchange = count of runs whose task appeared in campfire buys
reached_count = sum(1 for v in per_run.values() if v["reached"])

pct = (reached_count / n) * 100 if n > 0 else 0

# Summarize fails dict (exclude success)
fails_display = {k: v for k, v in buckets.items() if k != "success" and v > 0}

print(f"\nreached_exchange={reached_count}/{n} ({pct:.1f}%) | success={buckets['success']} | fails={fails_display}")

# Hard-failure buckets — these must be zero for a passing gate
hard_fail_buckets = {"No exchange configured", "operator not running", "server failed"}
hard_fail_count = sum(buckets[b] for b in hard_fail_buckets)

# Fix (bd5): threshold is >=95% (spec title). math.ceil(50 * 0.95) = 48/50.
# Previously used 0.94 (47/50). Aligning to spec. If the observed floor is lower,
# calibrate here and document — but do not silently use a softer threshold.
threshold = math.ceil(n * 0.95)

gate_ok = (reached_count >= threshold) and (hard_fail_count == 0)

if gate_ok:
    print(f"\nRESULT: PASS — {reached_count}/{n} ({pct:.1f}%) reached exchange, zero hard failures.")
    open(os.path.join(out_dir, ".result"), "w").write("PASS")
else:
    diag_lines = []
    if reached_count < threshold:
        diag_lines.append(f"reached_exchange={reached_count}/{n} ({pct:.1f}%) — below {threshold}/{n} (95%) threshold")
    for b in hard_fail_buckets:
        if buckets[b] > 0:
            diag_lines.append(f"hard-failure bucket '{b}' has {buckets[b]} failures (must be 0)")
    print(f"\nRESULT: FAIL")
    for d in diag_lines:
        print(f"  DIAGNOSTIC: {d}")
    open(os.path.join(out_dir, ".result"), "w").write("FAIL")

PYEOF

# ---------------------------------------------------------------------------
# Step 4: Cleanup or preserve logs based on result
# ---------------------------------------------------------------------------
RESULT=$(cat "${OUT_DIR}/.result" 2>/dev/null || echo "FAIL")
rm -f "${OUT_DIR}/.result"

if [ "$RESULT" = "PASS" ]; then
  printf "\n[cleanup] Run passed — removing out/run-*.log\n"
  cleanup_logs
  printf "[done] PASS\n"
  exit 0
else
  printf "\n[cleanup] Run FAILED — logs preserved in %s for debugging\n" "$OUT_DIR"
  printf "[done] FAIL\n"
  exit 1
fi
