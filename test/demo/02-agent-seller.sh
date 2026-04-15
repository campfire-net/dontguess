#!/usr/bin/env bash
# 02-agent-seller.sh — Agent seller demo: separate identity joins exchange, puts 3 items, earns scrip
#
# Proves: a seller agent with a distinct identity can join an existing exchange,
# put 3 cached inference items (code, analysis, data), and receive put-accept
# settlements — the seller-side of the basic lifecycle.
#
# Pattern: isolated temp dirs, trap cleanup on EXIT, tee transcript.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
OUTPUT_DIR="$SCRIPT_DIR/output"
OUTPUT_FILE="$OUTPUT_DIR/02-agent-seller.txt"

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

TMP=$(mktemp -d /tmp/dontguess-demo-02-XXXX)
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

# Get seller's public key so the operator can admit them
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
# Section: put — seller puts 3 items before serve (replay picks them up)
# ---------------------------------------------------------------------------

tee_section "put"

# --- Item 1: code — Go rate limiter with Redis backend ---
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

echo "$ CF_HOME=\$SELLER_CF dontguess put --description \"...\" --content \"\$CONTENT_CODE_B64\" --token_cost 2500 --content_type code"
CF_HOME="$SELLER_CF" cf "$XCFID" put \
    --description "Go rate limiter with Redis backend — sliding window, pipeline ops" \
    --content "$CONTENT_CODE_B64" \
    --token_cost 2500 \
    --content_type code
PUT_MSG_ID_1=$(cf --cf-home "$CF_HOME" "$XCFID" puts --json 2>/dev/null | \
    python3 -c "import json,sys; msgs=json.load(sys.stdin); print(msgs[0]['id'] if msgs else 'unknown')" 2>/dev/null || echo "unknown")
echo "# put 1 (code) message ID: $PUT_MSG_ID_1"

# --- Item 2: analysis — Terraform module analysis for AWS VPC ---
CONTENT_ANALYSIS='# Terraform Module Analysis: AWS VPC

## Summary
Standard 3-tier VPC with public/private/data subnets across 3 AZs.
Module uses count-based subnet expansion — avoid for_each is not used,
so adding AZs requires re-index. This is a known Terraform footgun.

## Key Findings
1. NAT Gateway per-AZ (correct for HA) — monthly cost ~$135/region
2. VPC Flow Logs enabled to S3 — ensure lifecycle policy or costs compound
3. Security group egress is 0.0.0.0/0 — acceptable for app tier, not data tier
4. No VPC endpoints for S3/DynamoDB — add these to reduce NAT traffic

## Recommendations
- Add for_each over az_list to avoid re-index footgun
- Add VPC endpoints for S3 and DynamoDB (free, saves NAT cost)
- Restrict data-tier security group egress to known CIDRs only
- Enable VPC Flow Logs retention policy (30d default is fine)'
CONTENT_ANALYSIS_B64=$(printf '%s' "$CONTENT_ANALYSIS" | base64 -w0)

echo "$ CF_HOME=\$SELLER_CF dontguess put --description \"...\" --content \"\$CONTENT_ANALYSIS_B64\" --token_cost 4000 --content_type analysis"
CF_HOME="$SELLER_CF" cf "$XCFID" put \
    --description "Terraform module analysis for AWS VPC — 3-tier HA, footgun findings, cost recommendations" \
    --content "$CONTENT_ANALYSIS_B64" \
    --token_cost 4000 \
    --content_type analysis
PUT_MSG_ID_2=$(cf --cf-home "$CF_HOME" "$XCFID" puts --json 2>/dev/null | \
    python3 -c "import json,sys; msgs=json.load(sys.stdin); print(msgs[0]['id'] if msgs else 'unknown')" 2>/dev/null || echo "unknown")
echo "# put 2 (analysis) message ID: $PUT_MSG_ID_2"

# --- Item 3: data — API latency benchmark dataset ---
CONTENT_DATA='{"benchmark":"api-latency","version":"1.0","collected":"2026-04-01","sample_count":10000,"percentiles":{"p50":12,"p75":18,"p90":31,"p95":52,"p99":147,"p999":423},"endpoints":[{"path":"/api/v1/items","method":"GET","p50":9,"p95":41},{"path":"/api/v1/items","method":"POST","p50":18,"p95":89},{"path":"/api/v1/search","method":"GET","p50":31,"p95":142}],"infra":{"region":"us-east-1","instance":"c6i.xlarge","concurrency":50}}'
CONTENT_DATA_B64=$(printf '%s' "$CONTENT_DATA" | base64 -w0)

echo "$ CF_HOME=\$SELLER_CF dontguess put --description \"...\" --content \"\$CONTENT_DATA_B64\" --token_cost 1500 --content_type data"
CF_HOME="$SELLER_CF" cf "$XCFID" put \
    --description "API latency benchmark dataset — p50/p95/p99 by endpoint, 10k samples, c6i.xlarge" \
    --content "$CONTENT_DATA_B64" \
    --token_cost 1500 \
    --content_type data
PUT_MSG_ID_3=$(cf --cf-home "$CF_HOME" "$XCFID" puts --json 2>/dev/null | \
    python3 -c "import json,sys; msgs=json.load(sys.stdin); print(msgs[0]['id'] if msgs else 'unknown')" 2>/dev/null || echo "unknown")
echo "# put 3 (data) message ID: $PUT_MSG_ID_3"

# ---------------------------------------------------------------------------
# Section: serve — start exchange engine in background
# ---------------------------------------------------------------------------

tee_section "serve"

echo "$ dontguess serve --poll-interval 500ms &"
"$BINARY" serve --poll-interval 500ms > "$TMP/serve.log" 2>&1 &
SERVE_PID=$!

# Wait for serve to replay messages (up to 10s)
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
# Section: verify-accept — wait for 3 put-accept settle messages
# ---------------------------------------------------------------------------

tee_section "verify-accept"

echo "# Waiting for 3 put-accept settle messages (up to 20s)..."
ACCEPT_COUNT=0
for i in $(seq 1 40); do
    sleep 0.5
    ACCEPT_COUNT=$(cf --cf-home "$CF_HOME" "$XCFID" settlements --json 2>/dev/null | \
        python3 -c "import json,sys; print(len(json.load(sys.stdin)))" 2>/dev/null || echo 0)
    if [ "$ACCEPT_COUNT" -ge 3 ]; then
        echo "# 3 put-accept settlements received (settle count: $ACCEPT_COUNT)"
        break
    fi
done

if [ "$ACCEPT_COUNT" -lt 3 ]; then
    echo "# NOTE: Only $ACCEPT_COUNT put-accept settle(s) seen within 20s"
    echo "# Checking serve log for auto-accept activity..."
    grep "auto-accepted\|auto-accept\|pending\|put" "$TMP/serve.log" 2>/dev/null || echo "# (no relevant log lines)"
fi

echo "$ dontguess settlements"
SETTLE_MSGS=$(cf --cf-home "$CF_HOME" "$XCFID" settlements --json 2>/dev/null)
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

# Verify we got 3 settlements
if [ "$ACCEPT_COUNT" -lt 3 ]; then
    echo "ERROR: expected 3 put-accept settlements, got $ACCEPT_COUNT"
    echo "Serve log:"
    cat "$TMP/serve.log"
    exit 1
fi

# ---------------------------------------------------------------------------
# Section: summary
# ---------------------------------------------------------------------------

tee_section "summary"

echo "Exchange campfire: $XCFID"
echo "Put 1 (code):      $PUT_MSG_ID_1"
echo "Put 2 (analysis):  $PUT_MSG_ID_2"
echo "Put 3 (data):      $PUT_MSG_ID_3"
echo ""

# Final message count (puts view — exchange puts submitted)
FINAL_COUNT=$(cf --cf-home "$CF_HOME" "$XCFID" puts --json 2>/dev/null | \
    python3 -c "import json,sys; print(len(json.load(sys.stdin)))" 2>/dev/null || echo "?")
echo "Total puts submitted: $FINAL_COUNT"
echo ""
echo "Serve log:"
cat "$TMP/serve.log"
echo ""
echo "# Demo complete. Transcript written to: $OUTPUT_FILE"
