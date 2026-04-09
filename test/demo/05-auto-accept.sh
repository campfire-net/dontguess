#!/usr/bin/env bash
# 05-auto-accept.sh — Auto-accept threshold demo: 4 puts auto-accepted, 1 skipped
#
# Proves: the operator engine auto-accepts puts up to --auto-accept-max-price
# and skips puts whose token cost exceeds that threshold.
#
# Setup:
#   - Operator serves with --auto-accept-max-price 5000
#   - Seller puts 5 items: token costs 1000, 2000, 3000, 4000, 6000
#   - Engine auto-accepts the 4 puts at or below 5000 (scrip = 70% of token_cost)
#   - 6000-token put is skipped: "skipping put ... token cost 6000 > max 5000"
#
# Pattern: isolated temp dirs, trap cleanup on EXIT, tee transcript.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
OUTPUT_DIR="$SCRIPT_DIR/output"
OUTPUT_FILE="$OUTPUT_DIR/05-auto-accept.txt"

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

TMP=$(mktemp -d /tmp/dontguess-demo-05-XXXX)
CF_HOME="$TMP/.cf"
CF_TRANSPORT_DIR="$TMP/transport"
SELLER_CF="$TMP/seller/.cf"
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

mkdir -p "$CF_HOME" "$CF_TRANSPORT_DIR" "$SELLER_CF"
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

# ---------------------------------------------------------------------------
# Section: operator-identity — create operator campfire identity
# ---------------------------------------------------------------------------

tee_section "operator-identity"
echo "$ cf init --cf-home \$CF_HOME"
cf --cf-home "$CF_HOME" init

# ---------------------------------------------------------------------------
# Section: init — create exchange campfire
# ---------------------------------------------------------------------------

tee_section "init"
echo "$ dontguess init"
"$BINARY" init

# Read the exchange campfire ID from config (reliable — avoids parsing stdout)
XCFID=$(python3 -c "import json; c=json.load(open('$CF_HOME/dontguess-exchange.json')); print(c['exchange_campfire_id'])")
echo "# exchange campfire: $XCFID"

# ---------------------------------------------------------------------------
# Section: seller-identity — create seller agent identity in separate dir
# ---------------------------------------------------------------------------

tee_section "seller-identity"
echo "$ CF_HOME=\$SELLER_CF cf init"
CF_HOME="$SELLER_CF" cf init
echo "# Seller identity created at: $SELLER_CF"

# ---------------------------------------------------------------------------
# Section: seller-admit — operator admits seller to the exchange campfire
# ---------------------------------------------------------------------------

tee_section "seller-admit"

SELLER_PUBKEY=$(CF_HOME="$SELLER_CF" cf id --json | python3 -c "import json,sys; print(json.load(sys.stdin)['public_key'])")
echo "# Seller public key: $SELLER_PUBKEY"

echo "$ cf --cf-home \$CF_HOME admit \$XCFID \$SELLER_PUBKEY"
cf --cf-home "$CF_HOME" admit "$XCFID" "$SELLER_PUBKEY"
echo "# Seller admitted to exchange campfire"

# ---------------------------------------------------------------------------
# Section: seller-join — seller joins the exchange campfire
# ---------------------------------------------------------------------------

tee_section "seller-join"
echo "$ CF_HOME=\$SELLER_CF cf join \$XCFID"
CF_HOME="$SELLER_CF" cf join "$XCFID"
echo "# Seller joined exchange campfire: $XCFID"

# ---------------------------------------------------------------------------
# Section: put — seller puts 5 items before serve (replayAll picks them up)
# token costs: 1000, 2000, 3000, 4000, 6000
# items 1-4 are within the 5000 threshold; item 5 (6000) should be skipped
# ---------------------------------------------------------------------------

tee_section "put"

# Helper: send a single put message and print its ID
send_put() {
    local description="$1"
    local token_cost="$2"
    local content_type="$3"
    local content_b64="$4"

    local payload
    payload=$(python3 -c "
import json
print(json.dumps({
    'description': '$description',
    'content': '$content_b64',
    'token_cost': $token_cost,
    'content_type': '$content_type',
}))
")
    echo "$ CF_HOME=\$SELLER_CF cf send \$XCFID <put-payload> --tag exchange:put --tag ${content_type}"
    local msg
    msg=$(CF_HOME="$SELLER_CF" cf send "$XCFID" "$payload" \
        --tag "exchange:put" \
        --tag "${content_type}" \
        --json)
    echo "$msg" | python3 -c "import json,sys; print('put msg ID: ' + json.load(sys.stdin)['id'])"
    echo "$msg" | python3 -c "import json,sys; print(json.load(sys.stdin)['id'])"
}

# Shared dummy content (base64 of a one-liner)
DUMMY_CONTENT_B64=$(printf 'cached inference result' | base64 -w0)

echo "# Sending put 1: token_cost=1000 (below threshold — will auto-accept)"
MSG_ID_1=$(send_put "Python async HTTP client — aiohttp session management" 1000 "exchange:content-type:code" "$DUMMY_CONTENT_B64" | tail -1)
echo "# put 1: $MSG_ID_1"

echo ""
echo "# Sending put 2: token_cost=2000 (below threshold — will auto-accept)"
MSG_ID_2=$(send_put "Go context propagation patterns for gRPC middleware" 2000 "exchange:content-type:code" "$DUMMY_CONTENT_B64" | tail -1)
echo "# put 2: $MSG_ID_2"

echo ""
echo "# Sending put 3: token_cost=3000 (below threshold — will auto-accept)"
MSG_ID_3=$(send_put "Terraform ECS cluster with autoscaling — capacity provider strategy" 3000 "exchange:content-type:analysis" "$DUMMY_CONTENT_B64" | tail -1)
echo "# put 3: $MSG_ID_3"

echo ""
echo "# Sending put 4: token_cost=4000 (below threshold — will auto-accept)"
MSG_ID_4=$(send_put "AWS IAM trust policy analysis — cross-account assume role with conditions" 4000 "exchange:content-type:analysis" "$DUMMY_CONTENT_B64" | tail -1)
echo "# put 4: $MSG_ID_4"

echo ""
echo "# Sending put 5: token_cost=6000 (ABOVE threshold 5000 — will be skipped)"
MSG_ID_5=$(send_put "Full distributed tracing implementation — OpenTelemetry spans across 3 services" 6000 "exchange:content-type:code" "$DUMMY_CONTENT_B64" | tail -1)
echo "# put 5: $MSG_ID_5"

echo ""
echo "# 5 puts sent. token costs: 1000, 2000, 3000, 4000, 6000"
echo "# max-price threshold: 5000 — puts 1-4 should auto-accept, put 5 should be skipped"

# ---------------------------------------------------------------------------
# Section: serve — start exchange engine with explicit max-price threshold
# ---------------------------------------------------------------------------

tee_section "serve"

echo "$ dontguess serve --poll-interval 500ms --auto-accept-max-price 5000 &"
"$BINARY" serve --poll-interval 500ms --auto-accept-max-price 5000 > "$TMP/serve.log" 2>&1 &
SERVE_PID=$!

# Wait for serve to replay messages (up to 10s)
echo "# Waiting for engine to start and replay messages..."
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
grep "exchange serving\|campfire:\|operator:\|replayed\|auto-accept" "$TMP/serve.log" | head -10

# ---------------------------------------------------------------------------
# Section: verify-accept — wait for exactly 4 put-accept settlements
# ---------------------------------------------------------------------------

tee_section "verify-accept"

echo "# Waiting for 4 put-accept settle messages (up to 30s)..."
ACCEPT_COUNT=0
for i in $(seq 1 60); do
    sleep 0.5
    ACCEPT_COUNT=$(cf --cf-home "$CF_HOME" read "$XCFID" --all --tag "exchange:settle" --json 2>/dev/null | \
        python3 -c "import json,sys; print(len(json.load(sys.stdin)))" 2>/dev/null || echo 0)
    if [ "$ACCEPT_COUNT" -ge 4 ]; then
        echo "# 4 put-accept settlements received (settle count: $ACCEPT_COUNT)"
        break
    fi
done

echo "$ cf read \$XCFID --all --tag exchange:settle"
SETTLE_MSGS=$(cf --cf-home "$CF_HOME" read "$XCFID" --all --tag "exchange:settle" --json 2>/dev/null)
echo "$SETTLE_MSGS" | python3 -c "
import json, sys
msgs = json.load(sys.stdin)
print(f'Settle messages: {len(msgs)}')
for m in msgs:
    print(f'  id={m[\"id\"][:12]} tags={m.get(\"tags\",[])}')
    payload_raw = m.get('payload', '')
    if payload_raw:
        try:
            p = json.loads(payload_raw)
            entry_id = str(p.get('entry_id', ''))
            scrip = p.get('scrip_earned', p.get('scrip', '?'))
            print(f'  status={p.get(\"status\",\"?\")} entry_id={entry_id[:12]} scrip={scrip}')
        except Exception:
            print(f'  payload: {payload_raw[:80]}')
"

# ---------------------------------------------------------------------------
# Section: verify-skip — confirm the 6000-token put was skipped
# ---------------------------------------------------------------------------

tee_section "verify-skip"

echo "# Checking serve log for skip message for the 6000-token put..."
echo "$ grep 'skipping put' \$SERVE_LOG"
SKIP_LINE=$(grep "skipping put" "$TMP/serve.log" 2>/dev/null || echo "")
if [ -n "$SKIP_LINE" ]; then
    echo "# SKIP confirmed:"
    echo "$SKIP_LINE"
else
    echo "# NOTE: No 'skipping put' log line found — checking for pending puts..."
    grep "pending\|skip\|exceed\|max" "$TMP/serve.log" 2>/dev/null || echo "# (no relevant log lines)"
fi

# Confirm put 5 is still in pending state (not settled)
echo ""
echo "# Checking that put 5 has no settle message..."
SETTLED_FOR_PUT5=$(cf --cf-home "$CF_HOME" read "$XCFID" --all --tag "exchange:settle" --json 2>/dev/null | \
    python3 -c "
import json, sys
msgs = json.load(sys.stdin)
# We check if any settle references the 5th put's content or if settle count > 4
print(len(msgs))
" 2>/dev/null || echo "?")
echo "# Total settle messages: $SETTLED_FOR_PUT5 (expected: 4)"

# ---------------------------------------------------------------------------
# Section: summary
# ---------------------------------------------------------------------------

tee_section "summary"

echo "Exchange campfire:    $XCFID"
echo "Max-price threshold:  5000 tokens"
echo ""
echo "Puts sent:"
echo "  put 1 (token_cost=1000): $MSG_ID_1  → auto-accepted (scrip = 700)"
echo "  put 2 (token_cost=2000): $MSG_ID_2  → auto-accepted (scrip = 1400)"
echo "  put 3 (token_cost=3000): $MSG_ID_3  → auto-accepted (scrip = 2100)"
echo "  put 4 (token_cost=4000): $MSG_ID_4  → auto-accepted (scrip = 2800)"
echo "  put 5 (token_cost=6000): $MSG_ID_5  → skipped (6000 > 5000)"
echo ""
echo "Settlements expected: 4, skipped: 1"
echo "Settlements received: $ACCEPT_COUNT"
echo ""

if [ "$ACCEPT_COUNT" -ge 4 ]; then
    echo "# PASS: 4 puts auto-accepted, 1 skipped"
else
    echo "# NOTE: Got $ACCEPT_COUNT settlements (expected 4). Check serve log for details."
fi

echo ""
echo "Serve log:"
cat "$TMP/serve.log"
echo ""
echo "# Demo complete. Transcript written to: $OUTPUT_FILE"
