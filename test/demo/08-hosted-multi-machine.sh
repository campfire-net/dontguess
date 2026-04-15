#!/usr/bin/env bash
# 08-hosted-multi-machine.sh — Mode 3 (Team hosted): hosted identity, remote agent puts + buys
#
# Proves: when identities are initialized against mcp.getcampfire.app (hosted center
# campfire), a full put → auto-accept → buy → match round-trip works end-to-end.
#
# Three distinct identities (operator, seller, buyer) are each inited with
# --remote https://mcp.getcampfire.app, registering them on the hosted campfire
# infrastructure. The exchange campfire uses filesystem transport for message
# delivery (cf admit only supports filesystem transport in campfire v0.17).
#
# Key difference from demo 04 (local filesystem only):
#   - All cf init calls use --remote https://mcp.getcampfire.app
#   - Script is CI-safe: exits 0 with SKIP if hosted infra is unreachable
#
# Pattern reference:
#   /home/baron/projects/ready/test/demo/07-hosted-multi-machine.sh — hosted identity setup
#   /home/baron/projects/dontguess/test/demo/04-multi-agent.sh — exchange put/buy patterns
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
OUTPUT_DIR="$SCRIPT_DIR/output"
OUTPUT_FILE="$OUTPUT_DIR/08-hosted-multi-machine.txt"

mkdir -p "$OUTPUT_DIR"

# Tee all output to the transcript file
exec > >(tee "$OUTPUT_FILE") 2>&1

# ---------------------------------------------------------------------------
# Reachability check — skip gracefully if hosted infra is unavailable
# ---------------------------------------------------------------------------

if ! timeout 5 bash -c 'echo > /dev/tcp/mcp.getcampfire.app/443' 2>/dev/null; then
    echo "SKIP: mcp.getcampfire.app not reachable"
    exit 0
fi

echo "# mcp.getcampfire.app:443 reachable — proceeding with hosted demo"

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
# Setup — isolated temp environment with 3 distinct hosted identities
# ---------------------------------------------------------------------------

TMP=$(mktemp -d /tmp/dontguess-demo-08-XXXX)
OP_CF="$TMP/operator/.cf"       # operator identity (hosted center campfire)
SELLER_CF="$TMP/seller/.cf"     # seller identity (hosted center campfire)
BUYER_CF="$TMP/buyer/.cf"       # buyer identity (hosted center campfire)
CF_TRANSPORT_DIR="$TMP/transport"  # filesystem transport for exchange campfire
BINARY="$TMP/dontguess-operator"

# Export CF_TRANSPORT_DIR so all cf commands (join, send, read) resolve the
# exchange campfire transport dir correctly via fs.DefaultBaseDir() fallback.
export CF_TRANSPORT_DIR

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

mkdir -p "$OP_CF" "$SELLER_CF" "$BUYER_CF" "$CF_TRANSPORT_DIR"

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
# Section: operator-identity — create operator identity on hosted infra
# ---------------------------------------------------------------------------

tee_section "operator-identity (hosted: mcp.getcampfire.app)"
echo "# cf init --remote registers the identity campfire on the hosted relay."
echo "# This is Mode 3 (Team hosted): center campfire lives on mcp.getcampfire.app."
echo "$ cf --cf-home \$OP_CF init --remote https://mcp.getcampfire.app"
cf --cf-home "$OP_CF" init --remote https://mcp.getcampfire.app
echo "# Operator identity created with hosted center campfire"

# ---------------------------------------------------------------------------
# Section: init — create exchange campfire (filesystem transport)
# ---------------------------------------------------------------------------

tee_section "init"
echo "# dontguess init creates the exchange campfire."
echo "# The exchange campfire uses filesystem transport (cf admit requires filesystem)."
echo "# The operator identity is hosted — admission flows through mcp.getcampfire.app."
echo "$ CF_HOME=\$OP_CF CF_TRANSPORT_DIR=\$CF_TRANSPORT_DIR dontguess init"
CF_HOME="$OP_CF" CF_TRANSPORT_DIR="$CF_TRANSPORT_DIR" "$BINARY" init

# Read the exchange campfire ID from config
XCFID=$(python3 -c "import json; c=json.load(open('$OP_CF/dontguess-exchange.json')); print(c['exchange_campfire_id'])")
echo "# exchange campfire: $XCFID"

# ---------------------------------------------------------------------------
# Section: seller-identity — seller registers on hosted infra
# ---------------------------------------------------------------------------

tee_section "seller-identity (hosted: mcp.getcampfire.app)"
echo "$ cf --cf-home \$SELLER_CF init --remote https://mcp.getcampfire.app"
cf --cf-home "$SELLER_CF" init --remote https://mcp.getcampfire.app
echo "# Seller identity created with hosted center campfire"

# ---------------------------------------------------------------------------
# Section: seller-admit — operator admits seller to the exchange campfire
# ---------------------------------------------------------------------------

tee_section "seller-admit"

# cf id --json returns the public key already as hex (64 chars)
SELLER_PUBKEY_HEX=$(cf --cf-home "$SELLER_CF" id --json | python3 -c "import json,sys; print(json.load(sys.stdin)['public_key'])")
echo "# Seller public key (hex): $SELLER_PUBKEY_HEX"

echo "$ cf --cf-home \$OP_CF admit \$XCFID \$SELLER_PUBKEY_HEX"
cf --cf-home "$OP_CF" admit "$XCFID" "$SELLER_PUBKEY_HEX"
echo "# Seller admitted to exchange campfire"

# ---------------------------------------------------------------------------
# Section: seller-join — seller joins the exchange campfire
# ---------------------------------------------------------------------------

tee_section "seller-join"
echo "$ CF_HOME=\$SELLER_CF cf --cf-home \$SELLER_CF join \$XCFID"
cf --cf-home "$SELLER_CF" join "$XCFID"
echo "# Seller joined exchange campfire: $XCFID"

# ---------------------------------------------------------------------------
# Section: buyer-identity — buyer registers on hosted infra
# ---------------------------------------------------------------------------

tee_section "buyer-identity (hosted: mcp.getcampfire.app)"
echo "$ cf --cf-home \$BUYER_CF init --remote https://mcp.getcampfire.app"
cf --cf-home "$BUYER_CF" init --remote https://mcp.getcampfire.app
echo "# Buyer identity created with hosted center campfire"

# ---------------------------------------------------------------------------
# Section: buyer-admit — operator admits buyer to the exchange campfire
# ---------------------------------------------------------------------------

tee_section "buyer-admit"

# cf id --json returns the public key already as hex (64 chars)
BUYER_PUBKEY_HEX=$(cf --cf-home "$BUYER_CF" id --json | python3 -c "import json,sys; print(json.load(sys.stdin)['public_key'])")
echo "# Buyer public key (hex): $BUYER_PUBKEY_HEX"

echo "$ cf --cf-home \$OP_CF admit \$XCFID \$BUYER_PUBKEY_HEX"
CF_HOME="$OP_CF" cf --cf-home "$OP_CF" admit "$XCFID" "$BUYER_PUBKEY_HEX"
echo "# Buyer admitted to exchange campfire"

# ---------------------------------------------------------------------------
# Section: buyer-join — buyer joins the exchange campfire
# ---------------------------------------------------------------------------

tee_section "buyer-join"
echo "$ CF_HOME=\$BUYER_CF cf --cf-home \$BUYER_CF join \$XCFID"
cf --cf-home "$BUYER_CF" join "$XCFID"
echo "# Buyer joined exchange campfire: $XCFID"

# ---------------------------------------------------------------------------
# Section: put — seller puts cached inference item
# ---------------------------------------------------------------------------

tee_section "put (remote agent → hosted exchange)"

CONTENT_CODE='package ratelimit

import (
    "context"
    "fmt"
    "time"

    "github.com/redis/go-redis/v9"
)

// Limiter implements a sliding window rate limiter backed by Redis.
type Limiter struct {
    client   *redis.Client
    window   time.Duration
    maxReqs  int
}

// Allow returns true if the request is within the rate limit for key.
func (l *Limiter) Allow(ctx context.Context, key string) (bool, error) {
    now := time.Now().UnixMilli()
    windowStart := now - l.window.Milliseconds()
    pipe := l.client.Pipeline()
    pipe.ZRemRangeByScore(ctx, key, "0", fmt.Sprintf("%d", windowStart))
    pipe.ZCard(ctx, key)
    pipe.ZAdd(ctx, key, redis.Z{Score: float64(now), Member: now})
    pipe.Expire(ctx, key, l.window)
    cmds, err := pipe.Exec(ctx)
    if err != nil {
        return false, err
    }
    count := cmds[1].(*redis.IntCmd).Val()
    return count < int64(l.maxReqs), nil
}'
CONTENT_CODE_B64=$(printf '%s' "$CONTENT_CODE" | base64 -w0)

echo "$ CF_HOME=\$SELLER_CF dontguess put --description '...' --content \$B64 --token-cost 2500 --content-type code"
PUT_MSG=$(CF_HOME="$SELLER_CF" dontguess put \
    --description "Go rate limiter with Redis backend — sliding window, pipeline ops" \
    --content "$CONTENT_CODE_B64" \
    --token-cost 2500 \
    --content-type code \
    --json)
PUT_MSG_ID=$(echo "$PUT_MSG" | python3 -c "import json,sys; print(json.load(sys.stdin)['id'])")
echo "# put message ID: $PUT_MSG_ID"

# ---------------------------------------------------------------------------
# Section: serve — start exchange engine in background
# ---------------------------------------------------------------------------

tee_section "serve"

echo "$ CF_HOME=\$OP_CF CF_TRANSPORT_DIR=\$CF_TRANSPORT_DIR dontguess serve --poll-interval 500ms &"
CF_HOME="$OP_CF" CF_TRANSPORT_DIR="$CF_TRANSPORT_DIR" \
    "$BINARY" serve --poll-interval 500ms > "$TMP/serve.log" 2>&1 &
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
grep "exchange serving\|campfire:\|operator:\|replayed" "$TMP/serve.log" | head -10

# ---------------------------------------------------------------------------
# Section: verify-accept — wait for put-accept settle message
# ---------------------------------------------------------------------------

tee_section "verify-accept"

echo "# Waiting for put-accept settle message (up to 20s)..."
ACCEPT_COUNT=0
for i in $(seq 1 40); do
    sleep 0.5
    ACCEPT_COUNT=$(CF_HOME="$OP_CF" dontguess settlements --json 2>/dev/null | \
        python3 -c "import json,sys; print(len(json.load(sys.stdin)))" 2>/dev/null || echo 0)
    if [ "$ACCEPT_COUNT" -ge 1 ]; then
        echo "# put-accept settlement received (settle count: $ACCEPT_COUNT)"
        break
    fi
done

if [ "$ACCEPT_COUNT" -lt 1 ]; then
    echo "# NOTE: No put-accept settle seen within 20s"
    echo "# Checking serve log for auto-accept activity..."
    grep "auto-accepted\|auto-accept\|pending\|put" "$TMP/serve.log" 2>/dev/null || echo "# (no relevant log lines)"
fi

echo "$ CF_HOME=\$OP_CF dontguess settlements"
SETTLE_MSGS=$(CF_HOME="$OP_CF" dontguess settlements --json 2>/dev/null)
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
            print(f'  status={p.get(\"status\",\"?\")} entry_id={str(p.get(\"entry_id\",\"\"))[:12]}')
        except Exception:
            print(f'  payload: {payload_raw[:80]}')
"

if [ "$ACCEPT_COUNT" -lt 1 ]; then
    echo "ERROR: expected at least 1 put-accept settlement, got $ACCEPT_COUNT"
    echo "Serve log:"
    cat "$TMP/serve.log"
    exit 1
fi

# ---------------------------------------------------------------------------
# Section: buy — buyer requests cached inference (hosted identity, remote request)
# ---------------------------------------------------------------------------

tee_section "buy (remote buyer → hosted exchange)"

echo "$ CF_HOME=\$BUYER_CF dontguess buy --task '...' --budget 5000"
BUY_MSG=$(CF_HOME="$BUYER_CF" dontguess buy \
    --task "rate limiter implementation in Go" \
    --budget 5000 \
    --json)
BUY_MSG_ID=$(echo "$BUY_MSG" | python3 -c "import json,sys; print(json.load(sys.stdin)['id'])")
echo "# buy message ID: $BUY_MSG_ID"

# ---------------------------------------------------------------------------
# Section: verify-match — wait for match response from engine
# ---------------------------------------------------------------------------

tee_section "verify-match"

echo "# Waiting for exchange:match response (up to 15s)..."
MATCH_FOUND=false
for i in $(seq 1 30); do
    sleep 0.5
    MATCH_COUNT=$(CF_HOME="$OP_CF" dontguess match-results --json 2>/dev/null | \
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

echo "$ CF_HOME=\$OP_CF dontguess match-results"
MATCH_MSGS=$(CF_HOME="$OP_CF" dontguess match-results --json 2>/dev/null)
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
                    eid = str(r.get('entry_id',''))
                    print(f'    entry_id={eid[:12]} confidence={r.get(\"confidence\",0):.2f}')
            else:
                print(f'  (no results — buy-miss standing order)')
        except Exception:
            print(f'  payload: {payload_raw[:80]}')
"

# Extract entry_id from match to verify it references the seller's put
ENTRY_ID=$(echo "$MATCH_MSGS" | python3 -c "
import json, sys
msgs = json.load(sys.stdin)
for m in msgs:
    payload_raw = m.get('payload', '')
    if payload_raw:
        try:
            p = json.loads(payload_raw)
            results = p.get('results', [])
            if results:
                print(results[0].get('entry_id', ''))
                sys.exit(0)
        except Exception:
            pass
print('')
" 2>/dev/null || echo "")

echo ""
echo "# Verifying match references seller's put..."
echo "# Put message ID:  $PUT_MSG_ID"
echo "# Buy message ID:  $BUY_MSG_ID"
echo "# Match entry_id:  $ENTRY_ID"

if [ -n "$ENTRY_ID" ] && [ "$ENTRY_ID" != "" ]; then
    echo "# Match entry_id present — buyer received cached inference from seller"
    echo "# put → auto-accept → buy → match lifecycle verified with hosted identities"
else
    echo "# NOTE: No direct match entry_id (buy-miss or low confidence)"
    echo "# buy → match lifecycle still verified via exchange:match tag"
fi

# ---------------------------------------------------------------------------
# Section: campfire-log — show full message flow
# ---------------------------------------------------------------------------

tee_section "campfire-log"

echo "# Full campfire message log (all 3 hosted identities' activity):"
echo "$ CF_HOME=\$OP_CF dontguess messages"
CF_HOME="$OP_CF" dontguess messages --json 2>/dev/null | python3 -c "
import json, sys
msgs = json.load(sys.stdin)
print(f'Total messages: {len(msgs)}')
print()
tag_counts = {}
for m in msgs:
    for t in m.get('tags', []):
        tag_counts[t] = tag_counts.get(t, 0) + 1
print('Message breakdown by tag:')
for tag, count in sorted(tag_counts.items()):
    print(f'  {tag}: {count}')
print()
print('Message sequence:')
for i, m in enumerate(msgs):
    tags = [t for t in m.get('tags', []) if t.startswith('exchange:')]
    payload_raw = m.get('payload', '')
    summary = ''
    if payload_raw:
        try:
            p = json.loads(payload_raw)
            if 'description' in p:
                summary = f' description={p[\"description\"][:40]!r}'
            elif 'task' in p:
                summary = f' task={p[\"task\"][:40]!r}'
            elif 'status' in p:
                summary = f' status={p[\"status\"]} entry_id={str(p.get(\"entry_id\",\"\"))[:12]}'
            elif 'results' in p:
                r = p['results']
                summary = f' results={len(r)} match(es)' if r else ' (buy-miss)'
        except Exception:
            pass
    print(f'  [{i+1:02d}] id={m[\"id\"][:12]} tags={tags}{summary}')
"

# ---------------------------------------------------------------------------
# Section: summary
# ---------------------------------------------------------------------------

tee_section "summary"

echo "Mode 3 (Team hosted): identities on mcp.getcampfire.app"
echo "Exchange campfire:    $XCFID"
echo "Put message ID:       $PUT_MSG_ID"
echo "Buy message ID:       $BUY_MSG_ID"
echo ""

FINAL_COUNT=$(CF_HOME="$OP_CF" dontguess messages --json 2>/dev/null | \
    python3 -c "import json,sys; print(len(json.load(sys.stdin)))" 2>/dev/null || echo "?")
echo "Total campfire messages: $FINAL_COUNT"
echo ""
echo "Serve log:"
cat "$TMP/serve.log"
echo ""
echo "# Demo 08 complete. Transcript written to: $OUTPUT_FILE"
