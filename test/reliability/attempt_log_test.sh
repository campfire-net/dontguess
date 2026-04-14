#!/bin/sh
# test/reliability/attempt_log_test.sh — tests for wrapper attempt log (dontguess-58c)
# Verifies: JSONL log at $DG_HOME/dontguess-attempts.log
#
# Usage: sh test/reliability/attempt_log_test.sh
# Requires: dontguess, cf, jq, flock installed; real exchange configured at ~/.cf/dontguess-exchange.json
#
# Tests use real wrapper + real cf. No mocks except for failure-mode tests which use
# a scoped DG_HOME (no exchange.json) or chmod tricks.
set -eu

PASS=0
FAIL=0

DG="${HOME}/.local/bin/dontguess"
DG_HOME_DEFAULT="${HOME}/.cf"

pass() { printf "[PASS] %s\n" "$1"; PASS=$((PASS+1)); }
fail() { printf "[FAIL] %s: %s\n" "$1" "${2:-}"; FAIL=$((FAIL+1)); }

# Require jq for JSON parsing
if ! command -v jq >/dev/null 2>&1; then
  printf "SKIP: jq not installed\n"; exit 0
fi

# Require flock
if ! command -v flock >/dev/null 2>&1; then
  printf "SKIP: flock not installed\n"; exit 0
fi

# Require real exchange config
if [ ! -f "${DG_HOME_DEFAULT}/dontguess-exchange.json" ]; then
  printf "SKIP: no dontguess-exchange.json at %s\n" "$DG_HOME_DEFAULT"; exit 0
fi

# ---------------------------------------------------------------------------
# Test 1: Success case — buy logs tag=success, cmd=buy, exit=0
# ---------------------------------------------------------------------------
printf "\n[TEST 1] Success case\n"
TEST1_HOME=$(mktemp -d)
cp "${DG_HOME_DEFAULT}/dontguess-exchange.json" "${TEST1_HOME}/"
ATTEMPTS="${TEST1_HOME}/dontguess-attempts.log"
rm -f "$ATTEMPTS"

DG_HOME="$TEST1_HOME" "$DG" buy --task "attempt-log-success-$$" --budget 100 >/dev/null 2>&1 || true

if [ ! -f "$ATTEMPTS" ]; then
  fail "Test 1" "log file not created at $ATTEMPTS"
else
  LINE=$(tail -1 "$ATTEMPTS")
  if ! printf '%s' "$LINE" | jq -e . >/dev/null 2>&1; then
    fail "Test 1" "last line is not valid JSON: $LINE"
  else
    TAG=$(printf '%s' "$LINE" | jq -r '.tag')
    CMD=$(printf '%s' "$LINE" | jq -r '.cmd')
    EXIT=$(printf '%s' "$LINE" | jq -r '.exit')
    if [ "$TAG" = "success" ] && [ "$CMD" = "buy" ] && [ "$EXIT" = "0" ]; then
      pass "Test 1: success case — tag=success, cmd=buy, exit=0"
    else
      fail "Test 1" "unexpected values: tag=$TAG cmd=$CMD exit=$EXIT"
    fi
    # Verify required fields are present
    for field in ts pid cmd exit tag cf_home cwd caller; do
      if ! printf '%s' "$LINE" | jq -e "has(\"$field\")" >/dev/null 2>&1; then
        fail "Test 1 (fields)" "missing field: $field"
      fi
    done
  fi
fi
rm -rf "$TEST1_HOME"

# ---------------------------------------------------------------------------
# Test 2: No-exchange case — tag=no_exchange_configured, exit=1
# ---------------------------------------------------------------------------
printf "\n[TEST 2] No exchange configured\n"
TEST2_HOME=$(mktemp -d)
ATTEMPTS2="${TEST2_HOME}/dontguess-attempts.log"
rm -f "$ATTEMPTS2"

DG_HOME="$TEST2_HOME" "$DG" buy --task "attempt-log-noex-$$" --budget 100 >/dev/null 2>&1 || true

if [ ! -f "$ATTEMPTS2" ]; then
  fail "Test 2" "log file not created"
else
  LINE=$(tail -1 "$ATTEMPTS2")
  if ! printf '%s' "$LINE" | jq -e . >/dev/null 2>&1; then
    fail "Test 2" "last line is not valid JSON: $LINE"
  else
    TAG=$(printf '%s' "$LINE" | jq -r '.tag')
    EXIT=$(printf '%s' "$LINE" | jq -r '.exit')
    if [ "$TAG" = "no_exchange_configured" ] && [ "$EXIT" = "1" ]; then
      pass "Test 2: no-exchange — tag=no_exchange_configured, exit=1"
    else
      fail "Test 2" "unexpected values: tag=$TAG exit=$EXIT"
    fi
  fi
fi
rm -rf "$TEST2_HOME"

# ---------------------------------------------------------------------------
# Test 3: Failure classification via fake operator binary — operator_down
# ---------------------------------------------------------------------------
printf "\n[TEST 3] Failure classification (operator_down via fake operator)\n"
TEST3_HOME=$(mktemp -d)
cp "${DG_HOME_DEFAULT}/dontguess-exchange.json" "${TEST3_HOME}/"

# Create a fake dontguess-operator that starts but immediately fails
# so the health probe sees "server failed"
FAKE_OP="${TEST3_HOME}/fake-dg-operator"
cat > "$FAKE_OP" <<'FAKEOP'
#!/bin/sh
# fake operator: exits immediately (health probe will fail → "server failed")
if [ "${1:-}" = "serve" ]; then
  exit 1
fi
exit 1
FAKEOP
chmod +x "$FAKE_OP"

ATTEMPTS3="${TEST3_HOME}/dontguess-attempts.log"
rm -f "$ATTEMPTS3"

# Kill any real operator so this test's fake operator gets used
pkill -f dontguess-operator 2>/dev/null || true
sleep 0.2

DG_OP="$FAKE_OP" DG_HOME="$TEST3_HOME" "$DG" buy --task "attempt-log-opdown-$$" --budget 100 >/dev/null 2>&1 || true

if [ ! -f "$ATTEMPTS3" ]; then
  fail "Test 3" "log file not created"
else
  LINE=$(tail -1 "$ATTEMPTS3")
  if ! printf '%s' "$LINE" | jq -e . >/dev/null 2>&1; then
    fail "Test 3" "last line is not valid JSON: $LINE"
  else
    TAG=$(printf '%s' "$LINE" | jq -r '.tag')
    EXIT=$(printf '%s' "$LINE" | jq -r '.exit')
    if [ "$TAG" = "operator_down" ] && [ "$EXIT" = "1" ]; then
      pass "Test 3: operator_down — tag=operator_down, exit=1"
    else
      fail "Test 3" "unexpected values: tag=$TAG exit=$EXIT (expected operator_down/1)"
    fi
  fi
fi
rm -rf "$TEST3_HOME"

# ---------------------------------------------------------------------------
# Test 4: Parallel atomicity — 50 concurrent buys produce valid JSONL lines
# ---------------------------------------------------------------------------
printf "\n[TEST 4] Parallel atomicity (50 concurrent buys)\n"
TEST4_HOME=$(mktemp -d)
cp "${DG_HOME_DEFAULT}/dontguess-exchange.json" "${TEST4_HOME}/"
ATTEMPTS4="${TEST4_HOME}/dontguess-attempts.log"
rm -f "$ATTEMPTS4"

# Wait for operator startup on first call, then parallelize
DG_HOME="$TEST4_HOME" "$DG" buy --task "parallel-warmup-$$" --budget 10 >/dev/null 2>&1 || true

# 50 concurrent buys
i=1
while [ "$i" -le 50 ]; do
  DG_HOME="$TEST4_HOME" "$DG" buy --task "parallel-${i}-$$" --budget 10 >/dev/null 2>&1 &
  i=$((i+1))
done
wait

# Count lines and validate each
TOTAL_LINES=$(wc -l < "$ATTEMPTS4" 2>/dev/null || echo 0)
# We expect at least 51 lines (warmup + 50 parallel)
BAD_LINES=0
if [ -f "$ATTEMPTS4" ]; then
  while IFS= read -r line; do
    if [ -n "$line" ] && ! printf '%s' "$line" | jq -e . >/dev/null 2>&1; then
      BAD_LINES=$((BAD_LINES+1))
    fi
  done < "$ATTEMPTS4"
fi

ACTUAL_LINES=$(grep -c '"cmd"' "$ATTEMPTS4" 2>/dev/null || echo 0)
if [ "$ACTUAL_LINES" -ge 50 ] && [ "$BAD_LINES" -eq 0 ]; then
  pass "Test 4: parallel atomicity — ${ACTUAL_LINES} lines, all valid JSON, 0 corrupt"
else
  fail "Test 4" "lines=${ACTUAL_LINES} (want >=50), bad_lines=${BAD_LINES}"
fi
rm -rf "$TEST4_HOME"

# ---------------------------------------------------------------------------
# Test 5: Fail-safe — log not writable, main command still succeeds
# ---------------------------------------------------------------------------
printf "\n[TEST 5] Fail-safe (log not writable)\n"
TEST5_HOME=$(mktemp -d)
cp "${DG_HOME_DEFAULT}/dontguess-exchange.json" "${TEST5_HOME}/"
ATTEMPTS5="${TEST5_HOME}/dontguess-attempts.log"

# Pre-create log and make it unwritable
touch "$ATTEMPTS5"
chmod 000 "$ATTEMPTS5"

EXIT5=0
DG_HOME="$TEST5_HOME" "$DG" buy --task "failsafe-test-$$" --budget 100 >/dev/null 2>&1 || EXIT5=$?

# Restore perms
chmod 644 "$ATTEMPTS5" 2>/dev/null || true

# The main command should succeed (exit 0) regardless of log failure
if [ "$EXIT5" -eq 0 ]; then
  pass "Test 5: fail-safe — main command succeeded (exit 0) despite unwritable log"
else
  fail "Test 5" "main command exited $EXIT5 (expected 0)"
fi
rm -rf "$TEST5_HOME"

# ---------------------------------------------------------------------------
# Test 6: Caller prefix — either null or 8 lowercase hex chars
# ---------------------------------------------------------------------------
printf "\n[TEST 6] Caller prefix format\n"
TEST6_HOME=$(mktemp -d)
cp "${DG_HOME_DEFAULT}/dontguess-exchange.json" "${TEST6_HOME}/"
ATTEMPTS6="${TEST6_HOME}/dontguess-attempts.log"
rm -f "$ATTEMPTS6"

DG_HOME="$TEST6_HOME" "$DG" buy --task "caller-test-$$" --budget 100 >/dev/null 2>&1 || true

if [ ! -f "$ATTEMPTS6" ]; then
  fail "Test 6" "log not created"
else
  LINE=$(tail -1 "$ATTEMPTS6")
  CALLER=$(printf '%s' "$LINE" | jq -r '.caller // "null"')
  if [ "$CALLER" = "null" ]; then
    pass "Test 6: caller=null (identity not hex-prefixed or absent)"
  else
    # Must match ^[0-9a-f]{8}$
    case "$CALLER" in
      [0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f])
        pass "Test 6: caller=${CALLER} — 8 hex chars";;
      *)
        fail "Test 6" "caller '${CALLER}' is not null or 8 hex chars";;
    esac
  fi
fi
rm -rf "$TEST6_HOME"

# ---------------------------------------------------------------------------
# Test 6b: Caller prefix — hex public_key identity produces 8-char hex caller
# (dontguess-493: explicit coverage for the hex code path)
# ---------------------------------------------------------------------------
printf "\n[TEST 6b] Caller prefix — hex identity path\n"
TEST6B_HOME=$(mktemp -d)
cp "${DG_HOME_DEFAULT}/dontguess-exchange.json" "${TEST6B_HOME}/"
ATTEMPTS6B="${TEST6B_HOME}/dontguess-attempts.log"
rm -f "$ATTEMPTS6B"

# Synthetic identity.json with a lowercase hex public_key (64 hex chars)
HEX_KEY="deadbeef01234567abcdef890123456789abcdef01234567deadbeef01234567"
mkdir -p "${TEST6B_HOME}"
printf '{"public_key":"%s","display_name":"test-hex-identity"}\n' "$HEX_KEY" \
  > "${TEST6B_HOME}/identity.json"

# Run a buy using this hex identity for CF_HOME (so _attempt_log_write finds it)
DG_HOME="$TEST6B_HOME" CF_HOME="$TEST6B_HOME" "$DG" buy \
  --task "caller-hex-test-$$" --budget 100 >/dev/null 2>&1 || true

if [ ! -f "$ATTEMPTS6B" ]; then
  fail "Test 6b" "log not created"
else
  LINE=$(tail -1 "$ATTEMPTS6B")
  TAG6B=$(printf '%s' "$LINE" | jq -r '.tag // "null"')
  CALLER=$(printf '%s' "$LINE" | jq -r '.caller // "null"')
  EXPECTED="${HEX_KEY%"${HEX_KEY#????????}"}"  # first 8 chars
  if [ "$TAG6B" = "no_exchange_configured" ]; then
    fail "Test 6b" "exchange not reached (tag=${TAG6B}) — hex identity test inconclusive"
  elif [ "$CALLER" = "$EXPECTED" ]; then
    pass "Test 6b: caller=${CALLER} tag=${TAG6B} — matches first 8 hex chars of public_key"
  else
    fail "Test 6b" "caller '${CALLER}' expected '${EXPECTED}' (first 8 hex chars of public_key)"
  fi
fi
rm -rf "$TEST6B_HOME"

# ---------------------------------------------------------------------------
# Test 7: Sensitive data absence — task description not in log
# ---------------------------------------------------------------------------
printf "\n[TEST 7] Sensitive data not logged\n"
TEST7_HOME=$(mktemp -d)
cp "${DG_HOME_DEFAULT}/dontguess-exchange.json" "${TEST7_HOME}/"
ATTEMPTS7="${TEST7_HOME}/dontguess-attempts.log"
rm -f "$ATTEMPTS7"

SECRET="SECRET_TOKEN_abc123_$$"
DG_HOME="$TEST7_HOME" "$DG" buy --task "$SECRET" --budget 100 >/dev/null 2>&1 || true

if [ -f "$ATTEMPTS7" ] && grep -q "$SECRET" "$ATTEMPTS7" 2>/dev/null; then
  fail "Test 7" "sensitive task description found in log!"
else
  pass "Test 7: sensitive data not in log"
fi
rm -rf "$TEST7_HOME"

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------
printf "\n----- attempt_log_test.sh results -----\n"
printf "PASS: %d  FAIL: %d\n" "$PASS" "$FAIL"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
exit 0
