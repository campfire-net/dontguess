#!/bin/sh
# E2E test: install, init, put, buy round-trip.
# Runs in an isolated HOME to avoid polluting the real environment.
# Usage: ./test/e2e-install.sh [path-to-operator-binary] [path-to-cf-binary]
#
# If no binaries are provided, builds from source.

set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

# Colors
RED='\033[0;31m'; GREEN='\033[0;32m'; BOLD='\033[1m'; RESET='\033[0m'
pass() { printf "${GREEN}PASS${RESET}: %s\n" "$1"; }
fail() { printf "${RED}FAIL${RESET}: %s\n" "$1"; FAILURES=$((FAILURES + 1)); }

FAILURES=0
TEST_HOME=$(mktemp -d)
trap 'kill $(cat "$TEST_HOME/.campfire/dontguess.pid" 2>/dev/null) 2>/dev/null; rm -rf "$TEST_HOME"' EXIT

export HOME="$TEST_HOME"
export PATH="$TEST_HOME/.local/bin:$PATH"
mkdir -p "$TEST_HOME/.local/bin"

# --- Setup binaries ---
DG_OP="${1:-}"
CF_BIN="${2:-}"

if [ -z "$DG_OP" ]; then
  printf "${BOLD}Building operator binary...${RESET}\n"
  GOPRIVATE=github.com/campfire-net HOME="$(eval echo ~$(whoami))" \
    go build -o "$TEST_HOME/.local/bin/dontguess-operator" "$REPO_DIR/cmd/dontguess" || exit 1
else
  cp "$DG_OP" "$TEST_HOME/.local/bin/dontguess-operator"
  chmod +x "$TEST_HOME/.local/bin/dontguess-operator"
fi

if [ -z "$CF_BIN" ]; then
  CF_SYSTEM=$(command -v cf 2>/dev/null || true)
  if [ -n "$CF_SYSTEM" ]; then
    cp "$CF_SYSTEM" "$TEST_HOME/.local/bin/cf"
  else
    echo "error: cf not found. Provide path as second argument or install cf." >&2
    exit 1
  fi
else
  cp "$CF_BIN" "$TEST_HOME/.local/bin/cf"
  chmod +x "$TEST_HOME/.local/bin/cf"
fi

# Write the wrapper script (same as install.sh produces)
cat > "$TEST_HOME/.local/bin/dontguess" <<'ENDWRAPPER'
#!/bin/sh
set -e
DG_OP="${HOME}/.local/bin/dontguess-operator"
CF="${HOME}/.local/bin/cf"
CF_HOME="${CF_HOME:-${HOME}/.campfire}"
CFG="${CF_HOME}/dontguess-exchange.json"
PID="${CF_HOME}/dontguess.pid"
LOG="${CF_HOME}/dontguess.log"
case "${1:-}" in
  init|serve|convention) exec "$DG_OP" "$@";;
  join|leave) subcmd="$1"; shift; exec "$CF" "$subcmd" "$@";;
  version|--version) echo "dontguess wrapper"; exit 0;;
  --help|-h|help|"") echo "dontguess — token-work exchange"; exit 0;;
esac
if [ ! -f "$CFG" ]; then echo "No exchange configured. Run: dontguess init" >&2; exit 1; fi
XCFID=$(sed -n 's/.*"exchange_campfire_id" *: *"\([^"]*\)".*/\1/p' "$CFG")
[ -z "$XCFID" ] && { echo "error: cannot read exchange_campfire_id" >&2; exit 1; }
if ! { [ -f "$PID" ] && kill -0 "$(cat "$PID")" 2>/dev/null; }; then
  nohup "$DG_OP" serve >"$LOG" 2>&1 &
  echo $! >"$PID"
  sleep 1
  kill -0 "$(cat "$PID")" 2>/dev/null || { echo "error: server failed. See $LOG" >&2; exit 1; }
fi
exec "$CF" "$XCFID" "$@"
ENDWRAPPER
chmod +x "$TEST_HOME/.local/bin/dontguess"

printf "\n${BOLD}=== E2E Install Test ===${RESET}\n\n"

# --- Test 1: No exchange configured ---
printf "${BOLD}Test 1: No exchange → helpful error${RESET}\n"
if dontguess buy --task "test" 2>&1 | grep -q "No exchange configured"; then
  pass "Wrapper shows helpful error when no exchange exists"
else
  fail "Wrapper did not show expected error"
fi

# --- Test 2: Init exchange ---
printf "\n${BOLD}Test 2: Init exchange (embedded conventions)${RESET}\n"
if dontguess init 2>&1 | grep -q "Exchange initialized"; then
  pass "Exchange initialized with embedded conventions"
else
  fail "Exchange init failed"
fi

# Verify config was created
if [ -f "$TEST_HOME/.campfire/dontguess-exchange.json" ]; then
  pass "Exchange config file created"
else
  fail "Exchange config file missing"
fi

# --- Test 3: Join exchange ---
printf "\n${BOLD}Test 3: Join exchange via wrapper${RESET}\n"
XCFID=$(sed -n 's/.*"exchange_campfire_id" *: *"\([^"]*\)".*/\1/p' "$TEST_HOME/.campfire/dontguess-exchange.json")
if dontguess join "$XCFID" 2>&1 | grep -q "already a member\|Joined"; then
  pass "Join routes correctly through cf"
else
  fail "Join did not route to cf correctly"
fi

# --- Test 4: Put (auto-starts server) ---
printf "\n${BOLD}Test 4: Put cached inference (auto-starts server)${RESET}\n"
CONTENT=$(echo "Token bucket rate limiter in Go: per-key limits with burst support, Redis backend for distributed use." | base64 -w0)
if dontguess put \
  --description "Token bucket rate limiter in Go with Redis backend" \
  --content "$CONTENT" \
  --token_cost 2000 \
  --content_type code \
  --domain go,networking 2>&1 | grep -q "ok"; then
  pass "Put dispatched successfully"
else
  fail "Put failed"
fi

# Verify server is running
if [ -f "$TEST_HOME/.campfire/dontguess.pid" ] && kill -0 "$(cat "$TEST_HOME/.campfire/dontguess.pid")" 2>/dev/null; then
  pass "Exchange server auto-started"
else
  fail "Exchange server not running"
fi

# Give the engine a moment to process the put
sleep 2

# --- Test 5: Check put was accepted ---
printf "\n${BOLD}Test 5: Verify put-accept${RESET}\n"
XCFID=$(sed -n 's/.*"exchange_campfire_id" *: *"\([^"]*\)".*/\1/p' "$TEST_HOME/.campfire/dontguess-exchange.json")
if cf read "$XCFID" --all 2>&1 | grep -q "exchange:phase:put-accept"; then
  pass "Put was auto-accepted by exchange"
else
  fail "No put-accept found"
fi

# --- Test 6: Buy ---
printf "\n${BOLD}Test 6: Buy (search for cached inference)${RESET}\n"
if dontguess buy \
  --task "rate limiter implementation in Go" \
  --budget 5000 2>&1 | grep -q "ok"; then
  pass "Buy dispatched successfully"
else
  fail "Buy failed"
fi

sleep 2

# --- Test 7: Check match result ---
printf "\n${BOLD}Test 7: Verify match result${RESET}\n"
if cf read "$XCFID" --all 2>&1 | grep -q "exchange:match"; then
  pass "Exchange returned a match for the buy"
else
  fail "No match found — buy-miss or engine did not process"
fi

# --- Summary ---
printf "\n${BOLD}=== Results ===${RESET}\n"
if [ "$FAILURES" -eq 0 ]; then
  printf "${GREEN}All tests passed.${RESET}\n"
  exit 0
else
  printf "${RED}${FAILURES} test(s) failed.${RESET}\n"
  exit 1
fi
