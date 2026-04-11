#!/bin/sh
# test/reliability/wrapper_parallel.sh — parallel wrapper reliability harness (dontguess-9b6)
#
# Spawns N (default 50) concurrent dontguess buy calls across 4 context rotations,
# then aggregates results and exits 0 iff >=94% reached the exchange with zero
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
# Setup / cleanup helpers
# ---------------------------------------------------------------------------

mkdir -p "$OUT_DIR"

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
        env -u CF_HOME "$DG" buy --task "$task" --budget 100 >"$log_file" 2>&1 || true
        ;;
      1)
        # worktree-cfhome: CF_HOME = fresh mktemp dir
        wt_dir=$(mktemp -d)
        CF_HOME="$wt_dir" "$DG" buy --task "$task" --budget 100 >"$log_file" 2>&1 || true
        rm -rf "$wt_dir" 2>/dev/null || true
        ;;
      2)
        # session-cfhome: CF_HOME = /tmp/cf-session-test-$i
        CF_HOME="/tmp/cf-session-test-${i}" "$DG" buy --task "$task" --budget 100 >"$log_file" 2>&1 || true
        ;;
      3)
        # warm-operator: no env override (operator already warm from earlier rounds)
        env -u CF_HOME "$DG" buy --task "$task" --budget 100 >"$log_file" 2>&1 || true
        ;;
    esac
    # Record exit status as last line (subshell always exits 0 due to || true above;
    # we capture the exit via the grep-able prefix instead)
    printf 'exit:0\n' >> "$log_file"
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
import sys, os, json, re, subprocess

out_dir  = sys.argv[1]
n        = int(sys.argv[2])
suffix   = sys.argv[3]
xcfid    = sys.argv[4]

# Fetch campfire buys — ground truth for "reached exchange"
try:
    cf_path = os.path.expanduser("~/.local/bin/cf")
    raw = subprocess.check_output([cf_path, xcfid, "buys", "--json"], stderr=subprocess.DEVNULL)
    buys = json.loads(raw)
except Exception as e:
    print(f"[warn] Could not fetch cf buys: {e}", file=sys.stderr)
    buys = []

# Build set of tasks that reached the exchange (filter by run suffix)
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

per_run = {}  # i -> {task, ctx_idx, bucket, reached}

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

    # Classify by stderr/stdout content
    if "No exchange configured" in text:
        bucket = "No exchange configured"
    elif "operator not running" in text or "operator is not running" in text:
        bucket = "operator not running"
    elif "server failed" in text or "server error" in text:
        bucket = "server failed"
    elif "identity is wrapped" in text:
        bucket = "identity is wrapped"
    elif task in reached_tasks:
        bucket = "success"
    else:
        # Exit code 0 in log means wrapper returned ok but might not have hit exchange
        bucket = "other"

    # Ground truth override: campfire is authoritative
    if task in reached_tasks:
        bucket = "success"

    buckets[bucket] += 1
    per_run[i] = {"task": task, "ctx_idx": ctx_idx, "bucket": bucket,
                  "reached": task in reached_tasks}

# reached_exchange = count of runs whose task appeared in campfire buys
reached_count = sum(1 for v in per_run.values() if v["reached"])

pct = (reached_count / n) * 100 if n > 0 else 0

# Summarize fails dict (exclude success)
fails_display = {k: v for k, v in buckets.items() if k != "success" and v > 0}

print(f"\nreached_exchange={reached_count}/{n} ({pct:.1f}%) | success={buckets['success']} | fails={fails_display}")

# Hard-failure buckets — these must be zero for a passing gate
hard_fail_buckets = {"No exchange configured", "operator not running", "server failed"}
hard_fail_count = sum(buckets[b] for b in hard_fail_buckets)

# Gate: >=94% reached AND zero hard failures
threshold = max(1, int(n * 0.94))
# Use ceiling: 47 out of 50 = 94%
import math
threshold = math.ceil(n * 0.94)

gate_ok = (reached_count >= threshold) and (hard_fail_count == 0)

if gate_ok:
    print(f"\nRESULT: PASS — {reached_count}/{n} ({pct:.1f}%) reached exchange, zero hard failures.")
    open(os.path.join(out_dir, ".result"), "w").write("PASS")
else:
    diag_lines = []
    if reached_count < threshold:
        diag_lines.append(f"reached_exchange={reached_count}/{n} ({pct:.1f}%) — below {threshold}/{n} (94%) threshold")
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
