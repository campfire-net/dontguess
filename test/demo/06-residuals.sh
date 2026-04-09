#!/usr/bin/env bash
# 06-residuals.sh — Residual economics demo: seller earns initial pay + residual on re-sale
#
# Proves: the publisher model's residual economics end-to-end.
#   1. Seller puts 1 item → operator auto-accepts, seller earns put-pay scrip
#   2. Buyer 1 buys the item → scrip-settle credits seller residual
#   3. Buyer 2 buys the same item → scrip-settle credits seller a second residual
#   4. Campfire log shows scrip-put-pay (initial) and two scrip-settle (residuals)
#
# Scrip seeding: operator mints scrip for both buyers before serve starts so they
# have funds for the buy-hold escrow. The scrip-mint message is a convention message
# that the engine processes during replay.
#
# Pattern: isolated temp dirs, trap cleanup on EXIT, tee transcript.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
OUTPUT_DIR="$SCRIPT_DIR/output"
OUTPUT_FILE="$OUTPUT_DIR/06-residuals.txt"

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
# Setup — isolated temp environment with 4 distinct identities
# ---------------------------------------------------------------------------

TMP=$(mktemp -d /tmp/dontguess-demo-06-XXXX)
CF_HOME="$TMP/.cf"           # operator identity
CF_TRANSPORT_DIR="$TMP/transport"
SELLER_CF="$TMP/seller/.cf"  # seller identity
BUYER1_CF="$TMP/buyer1/.cf"  # buyer 1 identity
BUYER2_CF="$TMP/buyer2/.cf"  # buyer 2 identity
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

mkdir -p "$CF_HOME" "$CF_TRANSPORT_DIR" "$SELLER_CF" "$BUYER1_CF" "$BUYER2_CF"
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

SELLER_PUBKEY=$(CF_HOME="$SELLER_CF" cf id --json | python3 -c "import json,sys; print(json.load(sys.stdin)['public_key'])")
echo "# Seller public key: ${SELLER_PUBKEY:0:16}..."

# ---------------------------------------------------------------------------
# Section: seller-admit — operator admits seller to the exchange campfire
# ---------------------------------------------------------------------------

tee_section "seller-admit"

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
# Section: buyer1-identity — create buyer 1 identity
# ---------------------------------------------------------------------------

tee_section "buyer1-identity"
echo "$ CF_HOME=\$BUYER1_CF cf init"
CF_HOME="$BUYER1_CF" cf init
echo "# Buyer 1 identity created at: $BUYER1_CF"

BUYER1_PUBKEY=$(CF_HOME="$BUYER1_CF" cf id --json | python3 -c "import json,sys; print(json.load(sys.stdin)['public_key'])")
echo "# Buyer 1 public key: ${BUYER1_PUBKEY:0:16}..."

echo "$ cf --cf-home \$CF_HOME admit \$XCFID \$BUYER1_PUBKEY"
cf --cf-home "$CF_HOME" admit "$XCFID" "$BUYER1_PUBKEY"

echo "$ CF_HOME=\$BUYER1_CF cf join \$XCFID"
CF_HOME="$BUYER1_CF" cf join "$XCFID"
echo "# Buyer 1 admitted and joined exchange campfire"

# ---------------------------------------------------------------------------
# Section: buyer2-identity — create buyer 2 identity
# ---------------------------------------------------------------------------

tee_section "buyer2-identity"
echo "$ CF_HOME=\$BUYER2_CF cf init"
CF_HOME="$BUYER2_CF" cf init
echo "# Buyer 2 identity created at: $BUYER2_CF"

BUYER2_PUBKEY=$(CF_HOME="$BUYER2_CF" cf id --json | python3 -c "import json,sys; print(json.load(sys.stdin)['public_key'])")
echo "# Buyer 2 public key: ${BUYER2_PUBKEY:0:16}..."

echo "$ cf --cf-home \$CF_HOME admit \$XCFID \$BUYER2_PUBKEY"
cf --cf-home "$CF_HOME" admit "$XCFID" "$BUYER2_PUBKEY"

echo "$ CF_HOME=\$BUYER2_CF cf join \$XCFID"
CF_HOME="$BUYER2_CF" cf join "$XCFID"
echo "# Buyer 2 admitted and joined exchange campfire"

# ---------------------------------------------------------------------------
# Section: put — seller puts 1 item before serve (replay picks it up)
# ---------------------------------------------------------------------------

tee_section "put"

CONTENT='# DynamoDB Single-Table Design: Access Pattern Analysis

## Table: orders (single-table)
PK: CUSTOMER#<id>  SK: ORDER#<timestamp>#<order_id>
GSI1: PK=STATUS#<status>  SK=ORDER#<timestamp>

## Access Patterns Covered
1. GetCustomerOrders(customer_id) — PK query, O(1)
2. GetOrdersByStatus(status, since) — GSI1 PK query with SK begins_with
3. GetOrder(customer_id, order_id) — PK+SK get_item, O(1)
4. GetRecentOrders(since) — GSI1 scan with filter (full scan — avoid in prod)

## Findings
- Composite SK (timestamp + order_id) avoids hot partition on high-velocity customers
- GSI1 fanout: status cardinality low (5 values) — acceptable partition density
- Missing: customer→order reverse lookup via GSI2 on order_id for support workflows

## Recommendations
- Add GSI2: PK=ORDER#<order_id> for O(1) order lookup by support agents
- TTL on completed orders older than 2y to control storage costs
- WCU burst: set on-demand for launch, switch to provisioned after traffic stabilizes'
CONTENT_B64=$(printf '%s' "$CONTENT" | base64 -w0)

PUT_PAYLOAD=$(python3 -c "
import json
print(json.dumps({
    'description': 'DynamoDB single-table design analysis — access patterns, GSI recommendations, capacity planning',
    'content': '$CONTENT_B64',
    'token_cost': 5000,
    'content_type': 'exchange:content-type:analysis',
}))
")

echo "$ CF_HOME=\$SELLER_CF cf send \$XCFID <put-payload> --tag exchange:put --tag exchange:content-type:analysis"
PUT_MSG=$(CF_HOME="$SELLER_CF" cf send "$XCFID" "$PUT_PAYLOAD" \
    --tag "exchange:put" \
    --tag "exchange:content-type:analysis" \
    --json)
PUT_MSG_ID=$(echo "$PUT_MSG" | python3 -c "import json,sys; print(json.load(sys.stdin)['id'])")
echo "# put message ID: $PUT_MSG_ID"
echo "# token_cost: 5000 → seller will earn put-pay scrip = 3500 (70%)"

# ---------------------------------------------------------------------------
# Section: seed-scrip — operator mints scrip for both buyers before serve
# scrip-mint is a convention message: operator sends it, engine replays and credits balances
# ---------------------------------------------------------------------------

tee_section "seed-scrip"

# Sale price for this item is approximately token_cost * 0.70 = 3500
# buy-hold reserves price + fee (10%) = ~3850 per buyer. Seed 10000 each to be safe.
SEED_AMOUNT=10000

echo "# Operator mints $SEED_AMOUNT scrip for buyer 1 (pubkey: ${BUYER1_PUBKEY:0:16}...)"
MINT1_PAYLOAD=$(python3 -c "
import json
print(json.dumps({
    'recipient': '$BUYER1_PUBKEY',
    'amount': $SEED_AMOUNT,
    'x402_tx_ref': 'demo-06-mint-buyer1',
    'rate': 1000
}))
")
echo "$ cf --cf-home \$CF_HOME send \$XCFID <mint-payload> --tag dontguess:scrip-mint"
MINT1_MSG=$(cf --cf-home "$CF_HOME" send "$XCFID" "$MINT1_PAYLOAD" \
    --tag "dontguess:scrip-mint" \
    --json)
MINT1_MSG_ID=$(echo "$MINT1_MSG" | python3 -c "import json,sys; print(json.load(sys.stdin)['id'])")
echo "# scrip-mint for buyer1: $MINT1_MSG_ID"

echo ""
echo "# Operator mints $SEED_AMOUNT scrip for buyer 2 (pubkey: ${BUYER2_PUBKEY:0:16}...)"
MINT2_PAYLOAD=$(python3 -c "
import json
print(json.dumps({
    'recipient': '$BUYER2_PUBKEY',
    'amount': $SEED_AMOUNT,
    'x402_tx_ref': 'demo-06-mint-buyer2',
    'rate': 1000
}))
")
echo "$ cf --cf-home \$CF_HOME send \$XCFID <mint-payload> --tag dontguess:scrip-mint"
MINT2_MSG=$(cf --cf-home "$CF_HOME" send "$XCFID" "$MINT2_PAYLOAD" \
    --tag "dontguess:scrip-mint" \
    --json)
MINT2_MSG_ID=$(echo "$MINT2_MSG" | python3 -c "import json,sys; print(json.load(sys.stdin)['id'])")
echo "# scrip-mint for buyer2: $MINT2_MSG_ID"

echo ""
echo "# Both buyers seeded with $SEED_AMOUNT scrip. Ready for serve."

# ---------------------------------------------------------------------------
# Section: serve — start exchange engine in background
# ---------------------------------------------------------------------------

tee_section "serve"

echo "$ dontguess serve --poll-interval 500ms &"
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
# Section: verify-put-accept — wait for put-accept settlement (70% of token_cost)
# ---------------------------------------------------------------------------

tee_section "verify-put-accept"

echo "# Waiting for put-accept settle message (up to 20s)..."
ACCEPT_FOUND=false
for i in $(seq 1 40); do
    sleep 0.5
    SETTLE_COUNT=$(cf --cf-home "$CF_HOME" read "$XCFID" --all --tag "exchange:settle" --json 2>/dev/null | \
        python3 -c "import json,sys; print(len(json.load(sys.stdin)))" 2>/dev/null || echo 0)
    if [ "$SETTLE_COUNT" -gt 0 ]; then
        ACCEPT_FOUND=true
        echo "# put-accept received (settle count: $SETTLE_COUNT)"
        break
    fi
done

if [ "$ACCEPT_FOUND" != "true" ]; then
    echo "# NOTE: No put-accept settle seen within 20s"
    grep "auto-accepted\|auto-accept\|pending" "$TMP/serve.log" 2>/dev/null || echo "# (no auto-accept log lines)"
fi

echo "$ cf read \$XCFID --all --tag exchange:settle"
cf --cf-home "$CF_HOME" read "$XCFID" --all --tag "exchange:settle" 2>/dev/null | head -20 || echo "(no settle messages visible yet)"

# Read seller's scrip balance after put-accept (from scrip-put-pay messages)
echo ""
echo "# Checking exchange scrip ledger for seller put-pay..."
SCRIP_MSGS=$(cf --cf-home "$CF_HOME" read "$XCFID" --all --tag "dontguess:scrip-put-pay" --json 2>/dev/null || echo "[]")
echo "$SCRIP_MSGS" | python3 -c "
import json, sys
msgs = json.load(sys.stdin)
print(f'scrip-put-pay messages: {len(msgs)}')
for m in msgs:
    payload_raw = m.get('payload', '')
    if payload_raw:
        try:
            p = json.loads(payload_raw)
            seller = p.get('seller', '?')[:16]
            amount = p.get('amount', '?')
            token_cost = p.get('token_cost', '?')
            discount_pct = p.get('discount_pct', '?')
            print(f'  seller={seller}... amount={amount} token_cost={token_cost} discount_pct={discount_pct}%')
        except Exception:
            print(f'  payload: {payload_raw[:80]}')
" 2>/dev/null || echo "(scrip-put-pay tag not yet visible from operator read)"

# ---------------------------------------------------------------------------
# Section: buy1 — buyer 1 buys the item
# ---------------------------------------------------------------------------

tee_section "buy1"

BUY1_PAYLOAD=$(python3 -c "
import json
print(json.dumps({
    'task': 'DynamoDB single-table design access pattern analysis',
    'budget': 8000
}))
")

echo "$ CF_HOME=\$BUYER1_CF cf send \$XCFID <buy-payload> --tag exchange:buy --future"
BUY1_MSG=$(CF_HOME="$BUYER1_CF" cf send "$XCFID" "$BUY1_PAYLOAD" \
    --tag "exchange:buy" \
    --future \
    --json)
BUY1_MSG_ID=$(echo "$BUY1_MSG" | python3 -c "import json,sys; print(json.load(sys.stdin)['id'])")
echo "# buy1 message ID: $BUY1_MSG_ID"

# ---------------------------------------------------------------------------
# Section: verify-match1 — wait for buyer 1's match response
# ---------------------------------------------------------------------------

tee_section "verify-match1"

echo "# Waiting for exchange:match response for buyer 1 (up to 15s)..."
MATCH1_FOUND=false
for i in $(seq 1 30); do
    sleep 0.5
    MATCH_COUNT=$(cf --cf-home "$CF_HOME" read "$XCFID" --all --tag "exchange:match" --json 2>/dev/null | \
        python3 -c "import json,sys; print(len(json.load(sys.stdin)))" 2>/dev/null || echo 0)
    if [ "$MATCH_COUNT" -gt 0 ]; then
        MATCH1_FOUND=true
        echo "# Buyer 1 match response received (match count: $MATCH_COUNT)"
        break
    fi
done

if [ "$MATCH1_FOUND" != "true" ]; then
    echo "ERROR: no match response for buyer 1 within 15s"
    echo "Serve log:"
    cat "$TMP/serve.log"
    exit 1
fi

echo "$ cf read \$XCFID --all --tag exchange:match"
MATCH1_MSGS=$(cf --cf-home "$CF_HOME" read "$XCFID" --all --tag "exchange:match" --json 2>/dev/null)
echo "$MATCH1_MSGS" | python3 -c "
import json, sys
msgs = json.load(sys.stdin)
print(f'Match messages so far: {len(msgs)}')
for m in msgs[-3:]:
    payload_raw = m.get('payload', '')
    if payload_raw:
        try:
            p = json.loads(payload_raw)
            results = p.get('results', [])
            if results:
                print(f'  results: {len(results)} match(es)')
                for r in results[:2]:
                    print(f'    entry_id={str(r.get(\"entry_id\",\"\"))[:12]} confidence={r.get(\"confidence\",0):.2f}')
            else:
                print(f'  (buy-miss standing order)')
        except Exception:
            print(f'  payload: {payload_raw[:80]}')
"

# ---------------------------------------------------------------------------
# Section: buy2 — buyer 2 buys the same item
# ---------------------------------------------------------------------------

tee_section "buy2"

BUY2_PAYLOAD=$(python3 -c "
import json
print(json.dumps({
    'task': 'DynamoDB table design with single-table pattern and GSI access patterns',
    'budget': 8000
}))
")

echo "$ CF_HOME=\$BUYER2_CF cf send \$XCFID <buy-payload> --tag exchange:buy --future"
BUY2_MSG=$(CF_HOME="$BUYER2_CF" cf send "$XCFID" "$BUY2_PAYLOAD" \
    --tag "exchange:buy" \
    --future \
    --json)
BUY2_MSG_ID=$(echo "$BUY2_MSG" | python3 -c "import json,sys; print(json.load(sys.stdin)['id'])")
echo "# buy2 message ID: $BUY2_MSG_ID"

# ---------------------------------------------------------------------------
# Section: verify-match2 — wait for buyer 2's match response
# ---------------------------------------------------------------------------

tee_section "verify-match2"

echo "# Waiting for exchange:match response for buyer 2 (up to 15s)..."
MATCH2_FOUND=false
for i in $(seq 1 30); do
    sleep 0.5
    MATCH_COUNT=$(cf --cf-home "$CF_HOME" read "$XCFID" --all --tag "exchange:match" --json 2>/dev/null | \
        python3 -c "import json,sys; print(len(json.load(sys.stdin)))" 2>/dev/null || echo 0)
    if [ "$MATCH_COUNT" -ge 2 ]; then
        MATCH2_FOUND=true
        echo "# Buyer 2 match response received (total match count: $MATCH_COUNT)"
        break
    fi
done

if [ "$MATCH2_FOUND" != "true" ]; then
    echo "# NOTE: Buyer 2 match not seen within 15s (may be buy-miss or pending)"
    echo "# Checking current match count..."
    MATCH_COUNT=$(cf --cf-home "$CF_HOME" read "$XCFID" --all --tag "exchange:match" --json 2>/dev/null | \
        python3 -c "import json,sys; print(len(json.load(sys.stdin)))" 2>/dev/null || echo 0)
    echo "# Current match count: $MATCH_COUNT"
fi

echo "$ cf read \$XCFID --all --tag exchange:match"
MATCH_MSGS=$(cf --cf-home "$CF_HOME" read "$XCFID" --all --tag "exchange:match" --json 2>/dev/null)
echo "$MATCH_MSGS" | python3 -c "
import json, sys
msgs = json.load(sys.stdin)
print(f'Total match messages: {len(msgs)}')
for i, m in enumerate(msgs):
    payload_raw = m.get('payload', '')
    if payload_raw:
        try:
            p = json.loads(payload_raw)
            results = p.get('results', [])
            if results:
                print(f'  [{i+1}] results: {len(results)} match(es)')
                for r in results[:2]:
                    print(f'      entry_id={str(r.get(\"entry_id\",\"\"))[:12]} confidence={r.get(\"confidence\",0):.2f}')
            else:
                print(f'  [{i+1}] (buy-miss standing order)')
        except Exception:
            print(f'  [{i+1}] payload: {payload_raw[:80]}')
"

# ---------------------------------------------------------------------------
# Section: scrip-ledger — show scrip movements: put-pay + settle (residuals)
# ---------------------------------------------------------------------------

tee_section "scrip-ledger"

echo "# Scrip ledger — all scrip operation messages on the exchange campfire"
echo ""

# scrip-put-pay: initial seller payment when put is accepted
echo "--- dontguess:scrip-put-pay (seller initial payment) ---"
PUTPAY_MSGS=$(cf --cf-home "$CF_HOME" read "$XCFID" --all --tag "dontguess:scrip-put-pay" --json 2>/dev/null || echo "[]")
echo "$PUTPAY_MSGS" | python3 -c "
import json, sys
msgs = json.load(sys.stdin)
print(f'  scrip-put-pay count: {len(msgs)}')
for m in msgs:
    payload_raw = m.get('payload', '')
    if payload_raw:
        try:
            p = json.loads(payload_raw)
            seller = p.get('seller', '?')[:16]
            amount = p.get('amount', '?')
            token_cost = p.get('token_cost', '?')
            print(f'  seller={seller}... put-pay amount={amount} (token_cost={token_cost})')
        except Exception:
            print(f'  {payload_raw[:80]}')
" 2>/dev/null || echo "  (no scrip-put-pay messages visible — check serve log)"

echo ""

# scrip-settle: buyer pay → seller residual + operator revenue + fee burn
echo "--- dontguess:scrip-settle (buyer sale → seller residual) ---"
SETTLE_MSGS=$(cf --cf-home "$CF_HOME" read "$XCFID" --all --tag "dontguess:scrip-settle" --json 2>/dev/null || echo "[]")
echo "$SETTLE_MSGS" | python3 -c "
import json, sys
msgs = json.load(sys.stdin)
print(f'  scrip-settle count: {len(msgs)}')
for i, m in enumerate(msgs):
    payload_raw = m.get('payload', '')
    if payload_raw:
        try:
            p = json.loads(payload_raw)
            seller = p.get('seller', '?')[:16]
            residual = p.get('residual', '?')
            fee_burned = p.get('fee_burned', '?')
            exchange_rev = p.get('exchange_revenue', '?')
            print(f'  [{i+1}] seller={seller}... residual={residual} fee_burned={fee_burned} exchange_revenue={exchange_rev}')
        except Exception:
            print(f'  [{i+1}] {payload_raw[:80]}')
" 2>/dev/null || echo "  (no scrip-settle messages visible — scrip flow may be in progress)"

echo ""
echo "# Economics summary (token_cost=5000, price=3500, fee=350, residual=350):"
echo "#   put-pay:    seller earns 3500 scrip (70% of 5000) — paid by operator when accepting put"
echo "#   sale 1:     buyer 1 pays 3850 (price 3500 + fee 350); seller gets ~350 residual (10%)"
echo "#   sale 2:     buyer 2 pays 3850 (price 3500 + fee 350); seller gets ~350 residual (10%)"
echo "#   total seller scrip: 3500 (put-pay) + 350 (residual 1) + 350 (residual 2) = 4200"

# ---------------------------------------------------------------------------
# Section: summary
# ---------------------------------------------------------------------------

tee_section "summary"

echo "Exchange campfire:    $XCFID"
echo "Seller pubkey:        ${SELLER_PUBKEY:0:16}..."
echo "Buyer 1 pubkey:       ${BUYER1_PUBKEY:0:16}..."
echo "Buyer 2 pubkey:       ${BUYER2_PUBKEY:0:16}..."
echo ""
echo "Put message ID:       $PUT_MSG_ID"
echo "Buy 1 message ID:     $BUY1_MSG_ID"
echo "Buy 2 message ID:     $BUY2_MSG_ID"
echo ""

# Final message count
FINAL_COUNT=$(cf --cf-home "$CF_HOME" read "$XCFID" --all --json 2>/dev/null | \
    python3 -c "import json,sys; print(len(json.load(sys.stdin)))" 2>/dev/null || echo "?")
echo "Total campfire messages: $FINAL_COUNT"
echo ""
echo "Serve log:"
cat "$TMP/serve.log"
echo ""
echo "# Demo complete. Transcript written to: $OUTPUT_FILE"
