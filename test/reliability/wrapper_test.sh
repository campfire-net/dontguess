#!/bin/sh
# test/reliability/wrapper_test.sh — regression tests for dontguess wrapper hardening
# Tests dontguess-8da: DG_HOME pin + flock + cmdline PID verify + health probe
#
# Usage: sh test/reliability/wrapper_test.sh
# Requires: dontguess, cf, jq installed; real exchange configured at ~/.cf/dontguess-exchange.json
#
# These tests exercise the REAL wrapper + operator + exchange pipeline. No mocks.
set -eu

PASS=0
FAIL=0

# ----- helpers -----

DG_HOME_DEFAULT="${HOME}/.cf"
DG="${HOME}/.local/bin/dontguess"
CF="${HOME}/.local/bin/cf"

# Get exchange campfire ID from the canonical config (never CF_HOME)
XCFID=$(jq -r .exchange_campfire_id "${DG_HOME_DEFAULT}/dontguess-exchange.json")

pass() { printf "[PASS] %s\n" "$1"; PASS=$((PASS+1)); }
fail() { printf "[FAIL] %s\n" "$1"; FAIL=$((FAIL+1)); }

# kill any running operator and wait for it to die
kill_operator() {
  pkill -f dontguess-operator 2>/dev/null || true
  # wait up to 3s for it to die
  i=0
  while pgrep -f dontguess-operator >/dev/null 2>&1 && [ "$i" -lt 30 ]; do
    sleep 0.1; i=$((i+1))
  done
}

# buy a task and verify it hit the exchange campfire
buy_and_verify() {
  local task="$1"
  local timeout="${2:-15}"

  # run the buy (may auto-start operator)
  "$DG" buy --task "$task" --budget 100 >/dev/null 2>&1 || true

  # poll for the message to appear in campfire (up to timeout seconds)
  local i=0
  while [ "$i" -lt "$timeout" ]; do
    local found
    found=$("$CF" "$XCFID" buys --json 2>/dev/null \
      | jq -r --arg t "$task" '.[] | select((.payload | fromjson? // {}) | .task==$t) | .id' 2>/dev/null || true)
    if [ -n "$found" ]; then
      return 0
    fi
    sleep 1; i=$((i+1))
  done
  return 1
}

# ----- test 1: default context -----
test_default_context() {
  printf "\n--- Test 1: default context ---\n"
  local task="regression-default-$$-1"

  # unset both env vars, kill operator so it must auto-start
  kill_operator
  env -u CF_HOME -u DG_HOME "$DG" buy --task "$task" >/dev/null 2>&1 || true

  if buy_and_verify "$task" 15; then
    pass "default context: buy reaches exchange"
  else
    fail "default context: buy not found in exchange within timeout"
  fi
}

# ----- test 2: session-dir context -----
test_session_dir_context() {
  printf "\n--- Test 2: session-dir CF_HOME ---\n"
  local task="regression-session-$$-2"
  local session_dir
  session_dir=$(mktemp -d)

  # Operator must already be running (from test 1 or will auto-start via DG_HOME default)
  # CF_HOME points to a temp dir — DG_HOME should still default to ~/.cf
  CF_HOME="$session_dir" "$DG" buy --task "$task" >/dev/null 2>&1 || true

  rm -rf "$session_dir"

  if buy_and_verify "$task" 15; then
    pass "session-dir CF_HOME: buy reaches exchange (wrapper uses DG_HOME, not CF_HOME)"
  else
    fail "session-dir CF_HOME: buy not found in exchange — wrapper may be using CF_HOME for exchange state"
  fi
}

# ----- test 3: stale PID (nonexistent process) -----
test_stale_pid() {
  printf "\n--- Test 3: stale PID (pid 999999) ---\n"
  local task="regression-stale-$$-3"

  kill_operator
  printf '999999\n' > "${DG_HOME_DEFAULT}/dontguess.pid"

  "$DG" buy --task "$task" >/dev/null 2>&1 || true

  if buy_and_verify "$task" 20; then
    pass "stale PID: detected stale, restarted, buy succeeded"
  else
    fail "stale PID: buy not found in exchange after restart"
  fi
}

# ----- test 4: PID reuse (shell's PID — alive but not operator) -----
test_pid_reuse() {
  printf "\n--- Test 4: PID reuse (shell's PID, not operator) ---\n"
  local task="regression-pidreuse-$$-4"

  kill_operator
  printf '%d\n' "$$" > "${DG_HOME_DEFAULT}/dontguess.pid"

  "$DG" buy --task "$task" >/dev/null 2>&1 || true

  if buy_and_verify "$task" 20; then
    pass "PID reuse: cmdline mismatch detected, restarted, buy succeeded"
  else
    fail "PID reuse: buy not found — wrapper may not be verifying cmdline"
  fi
}

# ----- test 5: parallel flock (10 concurrent buys, exactly 1 operator) -----
test_parallel_flock() {
  printf "\n--- Test 5: parallel flock (10 concurrent buys) ---\n"
  local base_task="regression-parallel-$$-5"

  kill_operator
  # Remove PID file so all 10 think the operator is not running
  rm -f "${DG_HOME_DEFAULT}/dontguess.pid"

  # spawn 10 concurrent buys
  i=1
  while [ "$i" -le 10 ]; do
    "$DG" buy --task "${base_task}-${i}" --budget 100 >/dev/null 2>&1 &
    i=$((i+1))
  done

  # wait for all background jobs
  wait

  # Count operator processes — must be exactly 1
  local op_count
  op_count=$(pgrep -f dontguess-operator 2>/dev/null | wc -l | tr -d ' ')
  if [ "$op_count" -eq 1 ]; then
    pass "parallel flock: exactly 1 operator running (count=$op_count)"
  else
    fail "parallel flock: expected 1 operator, got $op_count"
  fi

  # Verify at least 5 of the 10 buys reached the exchange (network/timing may drop some)
  local found=0
  i=1
  while [ "$i" -le 10 ]; do
    local task="${base_task}-${i}"
    local f
    f=$("$CF" "$XCFID" buys --json 2>/dev/null \
      | jq -r --arg t "$task" '.[] | select((.payload | fromjson? // {}) | .task==$t) | .id' 2>/dev/null || true)
    [ -n "$f" ] && found=$((found+1))
    i=$((i+1))
  done

  if [ "$found" -ge 5 ]; then
    pass "parallel flock: $found/10 buys reached exchange"
  else
    fail "parallel flock: only $found/10 buys reached exchange"
  fi
}

# ----- test 6: health-probe timeout -----
test_health_probe_timeout() {
  printf "\n--- Test 6: health-probe timeout (fake operator sleeps 30s) ---\n"

  # Create a fake operator binary that sleeps 30s (will never be ready)
  printf '#!/bin/sh\nexec sleep 30\n' > /tmp/fake-dontguess-op
  chmod +x /tmp/fake-dontguess-op

  kill_operator
  rm -f "${DG_HOME_DEFAULT}/dontguess.pid"

  # Time the call — should bail within ~6s
  local start_ts end_ts elapsed output
  start_ts=$(date +%s)
  output=$(DG_OP=/tmp/fake-dontguess-op "$DG" buy --task "regression-timeout-$$-6" --budget 100 2>&1 || true)
  end_ts=$(date +%s)
  elapsed=$((end_ts - start_ts))

  # Verify bail message
  if printf '%s' "$output" | grep -q "not ready in 5s"; then
    pass "health-probe timeout: wrapper bailed with 'not ready in 5s'"
  else
    fail "health-probe timeout: expected 'not ready in 5s' in output, got: $output"
  fi

  # Verify it bailed within 12s (5s probe + process start overhead)
  if [ "$elapsed" -le 12 ]; then
    pass "health-probe timeout: bailed in ${elapsed}s (within 12s window)"
  else
    fail "health-probe timeout: took ${elapsed}s (expected <=12s)"
  fi

  # Clean up: kill the fake operator by PID (pkill -f dontguess-operator does
  # NOT match 'sleep 30'), release the flock, and drop stale pid/lock files so
  # subsequent tests can acquire the start lock.
  if [ -f "${DG_HOME_DEFAULT}/dontguess.pid" ]; then
    fake_pid=$(cat "${DG_HOME_DEFAULT}/dontguess.pid" 2>/dev/null || true)
    if [ -n "$fake_pid" ]; then
      kill "$fake_pid" 2>/dev/null || true
      # wait up to 2s for the fake process to die
      i=0
      while kill -0 "$fake_pid" 2>/dev/null && [ "$i" -lt 20 ]; do
        sleep 0.1; i=$((i+1))
      done
      kill -9 "$fake_pid" 2>/dev/null || true
    fi
  fi
  kill_operator
  rm -f "${DG_HOME_DEFAULT}/dontguess.pid" "${DG_HOME_DEFAULT}/dontguess.start.lock"
  rm -f /tmp/fake-dontguess-op
}

# ----- sanity: one existing demo -----
test_existing_demo() {
  printf "\n--- Sanity: existing demo (01-solo-operator) ---\n"
  local demo="/home/baron/projects/dontguess/test/demo/01-solo-operator.sh"
  if [ ! -f "$demo" ]; then
    printf "[SKIP] demo not found: %s\n" "$demo"
    return
  fi
  if bash "$demo" >/dev/null 2>&1; then
    pass "existing demo 01-solo-operator: still passes"
  else
    fail "existing demo 01-solo-operator: REGRESSED"
  fi
}

# ----- main -----
printf "=== DontGuess wrapper regression tests (dontguess-8da) ===\n"
printf "Exchange campfire: %s\n" "$XCFID"
printf "Wrapper:           %s\n" "$DG"

test_default_context
test_session_dir_context
test_stale_pid
test_pid_reuse
test_parallel_flock
test_health_probe_timeout
test_existing_demo

printf "\n=== Results: %d passed, %d failed ===\n" "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ] || exit 1
