#!/usr/bin/env bash
# 01-solo-operator.sh — Solo operator demo: init, serve, put, buy, match lifecycle
#
# Proves: a single operator can init an exchange, start the engine, accept a put,
# process a buy, and return a match — the complete basic lifecycle.
#
# Pattern: isolated temp dirs, trap cleanup on EXIT, tee transcript.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
OUTPUT_DIR="$SCRIPT_DIR/output"
OUTPUT_FILE="$OUTPUT_DIR/01-solo-operator.txt"

mkdir -p "$OUTPUT_DIR"

# Tee all output to the transcript file
exec > >(tee "$OUTPUT_FILE") 2>&1

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

tee_section() {
    echo ""
    echo "=== SECTION: $1 ==="
}

run() {
    echo "$ $*"
    "$@"
}

# ---------------------------------------------------------------------------
# Setup — isolated temp environment
# ---------------------------------------------------------------------------

TMP=$(mktemp -d /tmp/dontguess-demo-01-XXXX)
CF_HOME="$TMP/.cf"
CF_TRANSPORT_DIR="$TMP/transport"
BINARY="$TMP/dontguess-operator"

trap "
    echo ''
    echo '=== SECTION: cleanup ==='
    if [ -n \"\${SERVE_PID:-}\" ] && kill -0 \"\$SERVE_PID\" 2>/dev/null; then
        echo '$ kill \$SERVE_PID'
        kill \"\$SERVE_PID\" 2>/dev/null || true
    fi
    echo '$ rm -rf \$TMP'
    rm -rf \"\$TMP\"
    echo 'cleanup complete.'
" EXIT

mkdir -p "$CF_HOME" "$CF_TRANSPORT_DIR"
export CF_HOME CF_TRANSPORT_DIR

# ---------------------------------------------------------------------------
# Determine binary — build from source or use system binary
# ---------------------------------------------------------------------------

PROJECT_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

if command -v dontguess-operator >/dev/null 2>&1; then
    BINARY="$(command -v dontguess-operator)"
    echo "# Using system dontguess-operator: $BINARY"
else
    echo "# Building dontguess-operator from source..."
    export PATH="/usr/local/go/bin:$HOME/go/bin:$PATH"
    go build -o "$BINARY" "$PROJECT_ROOT/cmd/dontguess"
    echo "# Built: $BINARY"
fi

# Point dontguess wrapper at the isolated temp environment and local binary
export DG_HOME="$CF_HOME"
export DG_OP="$BINARY"

# ---------------------------------------------------------------------------
# Section: identity — create campfire identity
# ---------------------------------------------------------------------------

tee_section "identity"
echo "$ cf init --cf-home \$CF_HOME"
cf --cf-home "$CF_HOME" init

# ---------------------------------------------------------------------------
# Section: init — create exchange campfire
# ---------------------------------------------------------------------------

tee_section "init"
echo "$ dontguess init"
"$BINARY" init

# Read the exchange campfire ID from config (reliable — avoids parsing stdout)
CAMPFIRE_ID=$(python3 -c "import json; c=json.load(open('$CF_HOME/dontguess-exchange.json')); print(c['exchange_campfire_id'])")
echo "# exchange campfire: $CAMPFIRE_ID"

# ---------------------------------------------------------------------------
# Section: put — seller offers cached inference (before serve, so replay picks it up)
# ---------------------------------------------------------------------------

tee_section "put"

# Content is base64-encoded actual inference result (v0.3 convention: engine computes hash/size)
CONTENT='package main

import (
    "encoding/json"
    "fmt"
    "net/http"
)

// Handler validates incoming POST JSON requests and returns structured errors.
func Handler(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodPost {
        http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
        return
    }
    var body map[string]any
    if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
        w.WriteHeader(http.StatusBadRequest)
        json.NewEncoder(w).Encode(map[string]any{"error": fmt.Sprintf("invalid JSON: %v", err)})
        return
    }
    w.WriteHeader(http.StatusOK)
    json.NewEncoder(w).Encode(map[string]any{"ok": true, "fields": len(body)})
}'
CONTENT_B64=$(printf '%s' "$CONTENT" | base64 -w0)

echo "$ cf --cf-home \$CF_HOME \$CAMPFIRE_ID put --description ... --content \$CONTENT_B64 --token_cost 2000 --content_type code"
cf --cf-home "$CF_HOME" "$CAMPFIRE_ID" put \
    --description "Go HTTP handler: validates POST JSON, returns structured errors" \
    --content "$CONTENT_B64" \
    --token_cost 2000 \
    --content_type code
# Read message ID from puts view (convention dispatch output is status only, not JSON)
PUT_MSG_ID=$(cf --cf-home "$CF_HOME" "$CAMPFIRE_ID" puts --json 2>/dev/null | \
    python3 -c "import json,sys; msgs=json.load(sys.stdin); print(msgs[-1]['id'] if msgs else 'unknown')" 2>/dev/null || echo "unknown")
echo "put message ID: $PUT_MSG_ID"

# ---------------------------------------------------------------------------
# Section: serve — start exchange engine in background
# ---------------------------------------------------------------------------

tee_section "serve"

echo "$ dontguess serve --poll-interval 500ms &"
"$BINARY" serve --poll-interval 500ms > "$TMP/serve.log" 2>&1 &
SERVE_PID=$!
# Register PID so dontguess wrapper finds the running server (avoids auto-start races)
echo "$SERVE_PID" > "$CF_HOME/dontguess.pid"

# Wait for serve to print "exchange serving" (up to 10s)
echo "# Waiting for engine to start..."
SERVE_READY=false
for i in $(seq 1 20); do
    sleep 0.5
    if grep -q "engine: replayed" "$TMP/serve.log" 2>/dev/null; then
        SERVE_READY=true
        break
    fi
done

if [ "$SERVE_READY" != "true" ]; then
    echo "ERROR: serve did not start within 10s"
    echo "Serve log:"
    cat "$TMP/serve.log"
    exit 1
fi

echo "# Exchange engine is running (PID $SERVE_PID)"
grep "exchange serving\|campfire:\|operator:\|replayed" "$TMP/serve.log" | head -10

# ---------------------------------------------------------------------------
# Section: verify-accept — wait for operator to auto-accept the put
# ---------------------------------------------------------------------------

tee_section "verify-accept"

echo "# Waiting for put-accept settle message (auto-accept, up to 10s)..."
ACCEPT_FOUND=false
for i in $(seq 1 20); do
    sleep 0.5
    SETTLE_COUNT=$(dontguess settlements --json 2>/dev/null | \
        python3 -c "import json,sys; print(len(json.load(sys.stdin)))" 2>/dev/null || echo 0)
    if [ "$SETTLE_COUNT" -gt 0 ]; then
        ACCEPT_FOUND=true
        echo "# put-accept received (settle count: $SETTLE_COUNT)"
        break
    fi
done

if [ "$ACCEPT_FOUND" != "true" ]; then
    echo "# NOTE: No put-accept settle seen within 10s (auto-accept may have fired before subscribe saw it)"
    echo "# Checking serve log for auto-accept..."
    grep "auto-accepted\|auto-accept\|pending" "$TMP/serve.log" 2>/dev/null || echo "# (no auto-accept log lines)"
fi

echo "$ dontguess settlements"
dontguess settlements 2>/dev/null | head -20 || echo "(no settle messages visible yet)"

# ---------------------------------------------------------------------------
# Section: buy — buyer requests cached inference
# ---------------------------------------------------------------------------

tee_section "buy"

echo "$ dontguess buy --task 'Go HTTP handler that validates incoming POST JSON requests' --budget 5000"
dontguess buy \
    --task "Go HTTP handler that validates incoming POST JSON requests" \
    --budget 5000
# Read message ID from buys view (convention dispatch output is status only, not JSON)
BUY_MSG_ID=$(dontguess buys --json 2>/dev/null | \
    python3 -c "import json,sys; msgs=json.load(sys.stdin); print(msgs[-1]['id'] if msgs else 'unknown')" 2>/dev/null || echo "unknown")
echo "buy message ID: $BUY_MSG_ID"

# ---------------------------------------------------------------------------
# Section: verify-match — wait for match or buy-miss response
# ---------------------------------------------------------------------------

tee_section "verify-match"

echo "# Waiting for exchange:match response (up to 15s)..."
MATCH_FOUND=false
for i in $(seq 1 30); do
    sleep 0.5
    # exchange:match tag is used for both real matches and buy-miss standing offers
    MATCH_COUNT=$(dontguess match-results --json 2>/dev/null | \
        python3 -c "import json,sys; print(len(json.load(sys.stdin)))" 2>/dev/null || echo 0)
    if [ "$MATCH_COUNT" -gt 0 ]; then
        MATCH_FOUND=true
        echo "# Match response received (match count: $MATCH_COUNT)"
        break
    fi
done

if [ "$MATCH_FOUND" != "true" ]; then
    echo "ERROR: no match response within 15s"
    echo "Serve log:"
    cat "$TMP/serve.log"
    exit 1
fi

echo "$ dontguess match-results"
MATCH_MSGS=$(dontguess match-results --json 2>/dev/null)
echo "$MATCH_MSGS" | python3 -c "
import json, sys
msgs = json.load(sys.stdin)
print(f'Match messages: {len(msgs)}')
for m in msgs:
    print(f'  id={m[\"id\"][:12]} tags={m.get(\"tags\",[])}')
    payload_raw = m.get('payload', '')
    if payload_raw:
        try:
            p = json.loads(payload_raw)
            results = p.get('results', [])
            if results:
                print(f'  results: {len(results)} match(es)')
                for r in results[:3]:
                    print(f'    entry_id={str(r.get(\"entry_id\",\"\"))[:12]} confidence={r.get(\"confidence\",0):.2f}')
            else:
                print(f'  (no results — buy-miss standing order)')
        except Exception:
            print(f'  payload: {payload_raw[:80]}')
"

# ---------------------------------------------------------------------------
# Section: summary
# ---------------------------------------------------------------------------

tee_section "summary"

echo "Exchange campfire: $CAMPFIRE_ID"
echo "Put message ID:    $PUT_MSG_ID"
echo "Buy message ID:    $BUY_MSG_ID"
echo ""

# Final message count
FINAL_COUNT=$(dontguess messages --json 2>/dev/null | \
    python3 -c "import json,sys; print(len(json.load(sys.stdin)))" 2>/dev/null || echo "?")
echo "Total campfire messages: $FINAL_COUNT"
echo ""
echo "Serve log:"
cat "$TMP/serve.log"
echo ""
echo "# Demo complete. Transcript written to: $OUTPUT_FILE"
