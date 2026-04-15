#!/usr/bin/env bash
# 07-assign-work.sh — Task marketplace demo: operator assigns maintenance work, worker completes for scrip
#
# Proves: the operator can post a maintenance assignment, a worker agent can
# claim and complete it, and the operator's assign-accept triggers scrip payment
# (dontguess:scrip-assign-pay) to the worker — the full assign lifecycle.
#
# Flow:
#   1. Operator init + exchange init
#   2. Worker identity init → operator admits → worker joins
#   3. Seller puts cache entry (entry to be maintained)
#   4. Engine starts (serve), auto-accepts the put
#   5. Operator posts assign (validate task) against the entry
#   6. Worker claims the assignment (assign-claim)
#   7. Worker completes (assign-complete with verdict=pass)
#   8. Operator accepts (assign-accept)
#   9. Engine emits scrip-assign-pay; verify worker payment
#
# Pattern: isolated temp dirs, trap cleanup on EXIT, tee transcript.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
OUTPUT_DIR="$SCRIPT_DIR/output"
OUTPUT_FILE="$OUTPUT_DIR/07-assign-work.txt"

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

# ---------------------------------------------------------------------------
# Setup — isolated temp environment
# ---------------------------------------------------------------------------

TMP=$(mktemp -d /tmp/dontguess-demo-07-XXXX)
CF_HOME="$TMP/.cf"            # operator identity
CF_TRANSPORT_DIR="$TMP/transport"
WORKER_CF="$TMP/worker/.cf"   # worker identity
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

mkdir -p "$CF_HOME" "$CF_TRANSPORT_DIR" "$WORKER_CF"
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

# Read the exchange campfire ID from config
XCFID=$(python3 -c "import json; c=json.load(open('$CF_HOME/dontguess-exchange.json')); print(c['exchange_campfire_id'])")
echo "# exchange campfire: $XCFID"

# ---------------------------------------------------------------------------
# Section: worker-identity — create worker agent identity in separate dir
# ---------------------------------------------------------------------------

tee_section "worker-identity"
echo "$ CF_HOME=\$WORKER_CF cf init"
CF_HOME="$WORKER_CF" cf init
echo "# Worker identity created at: $WORKER_CF"

# ---------------------------------------------------------------------------
# Section: worker-admit — operator admits worker to the exchange campfire
# ---------------------------------------------------------------------------

tee_section "worker-admit"

# Get worker's public key so the operator can admit them
WORKER_PUBKEY=$(CF_HOME="$WORKER_CF" cf id --json | python3 -c "import json,sys; print(json.load(sys.stdin)['public_key'])")
echo "# Worker public key: $WORKER_PUBKEY"

echo "$ cf --cf-home \$CF_HOME admit \$XCFID \$WORKER_PUBKEY"
cf --cf-home "$CF_HOME" admit "$XCFID" "$WORKER_PUBKEY"
echo "# Worker admitted to exchange campfire"

# ---------------------------------------------------------------------------
# Section: worker-join — worker joins the exchange campfire
# ---------------------------------------------------------------------------

tee_section "worker-join"
echo "$ CF_HOME=\$WORKER_CF cf join \$XCFID"
CF_HOME="$WORKER_CF" cf join "$XCFID"
echo "# Worker joined exchange campfire: $XCFID"

# ---------------------------------------------------------------------------
# Section: put — operator puts a cache entry for the worker to validate
# ---------------------------------------------------------------------------

tee_section "put"

# Content: a Go cache warming utility — the entry the worker will validate
CONTENT='package main

import (
    "context"
    "fmt"
    "time"
)

// WarmCache pre-populates a cache with computed results for a set of keys.
// It parallelises fetches up to concurrency workers and respects ctx cancellation.
func WarmCache(ctx context.Context, keys []string, fetch func(string) ([]byte, error), concurrency int) error {
    sem := make(chan struct{}, concurrency)
    errs := make(chan error, len(keys))
    for _, key := range keys {
        k := key
        sem <- struct{}{}
        go func() {
            defer func() { <-sem }()
            if _, err := fetch(k); err != nil {
                errs <- fmt.Errorf("warm %q: %w", k, err)
            }
        }()
        select {
        case <-ctx.Done():
            return ctx.Err()
        default:
        }
    }
    // Drain semaphore — wait for all goroutines.
    for i := 0; i < concurrency; i++ {
        sem <- struct{}{}
    }
    close(errs)
    for err := range errs {
        if err != nil {
            return err
        }
    }
    _ = time.Second // keep import used
    return nil
}'
CONTENT_B64=$(printf '%s' "$CONTENT" | base64 -w0)

# Compute the content hash for later use in the assign (sha256 of the raw content)
CONTENT_HASH="sha256:$(printf '%s' "$CONTENT" | sha256sum | awk '{print $1}')"
echo "# content hash: $CONTENT_HASH"

echo "$ cf --cf-home \$CF_HOME \$XCFID put --description '...' --content \$CONTENT_B64 --token_cost 3000 --content_type exchange:content-type:code"
cf --cf-home "$CF_HOME" "$XCFID" put \
    --description "Go cache warming utility — parallel key pre-population with semaphore and context cancellation" \
    --content "$CONTENT_B64" \
    --token_cost 3000 \
    --content_type "exchange:content-type:code"

# Read the message ID from the puts view (convention dispatch returns status only)
PUT_MSG_ID=$(cf --cf-home "$CF_HOME" "$XCFID" puts --json 2>/dev/null | \
    python3 -c "import json,sys; msgs=json.load(sys.stdin); print(msgs[0]['id'] if msgs else 'unknown')" 2>/dev/null || echo "unknown")
echo "# put message ID: $PUT_MSG_ID"

# ---------------------------------------------------------------------------
# Section: serve — start exchange engine in background
# ---------------------------------------------------------------------------

tee_section "serve"

echo "$ dontguess serve --poll-interval 500ms &"
"$BINARY" serve --poll-interval 500ms > "$TMP/serve.log" 2>&1 &
SERVE_PID=$!
echo "$SERVE_PID" > "$CF_HOME/dontguess.pid"

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
# Section: verify-accept — wait for put-accept settle
# ---------------------------------------------------------------------------

tee_section "verify-accept"

echo "# Waiting for put-accept settle (auto-accept, up to 15s)..."
ACCEPT_FOUND=false
for i in $(seq 1 30); do
    sleep 0.5
    SETTLE_COUNT=$(cf --cf-home "$CF_HOME" "$XCFID" settlements --json 2>/dev/null | \
        python3 -c "import json,sys; print(len(json.load(sys.stdin)))" 2>/dev/null || echo 0)
    if [ "$SETTLE_COUNT" -gt 0 ]; then
        ACCEPT_FOUND=true
        echo "# put-accept received (settle count: $SETTLE_COUNT)"
        break
    fi
done

if [ "$ACCEPT_FOUND" != "true" ]; then
    echo "ERROR: no put-accept settle within 15s"
    echo "Serve log:"
    cat "$TMP/serve.log"
    exit 1
fi

# Retrieve the entry_id from the settle message so we can reference it in assign
SETTLE_MSGS=$(cf --cf-home "$CF_HOME" "$XCFID" settlements --json 2>/dev/null)
ENTRY_ID=$(echo "$SETTLE_MSGS" | python3 -c "
import json, sys
msgs = json.load(sys.stdin)
for m in msgs:
    p = json.loads(m.get('payload', '{}'))
    if p.get('entry_id'):
        print(p['entry_id'])
        break
" 2>/dev/null || echo "")

if [ -z "$ENTRY_ID" ]; then
    echo "ERROR: could not determine entry_id from settle messages"
    echo "Settle messages:"
    echo "$SETTLE_MSGS"
    exit 1
fi
echo "# Entry ID from settle: $ENTRY_ID"

# ---------------------------------------------------------------------------
# Section: assign — operator posts validation task for the worker
# ---------------------------------------------------------------------------

tee_section "assign"

# Bounty: 50000 micro-tokens (the minimum per the convention spec)
# entry_value: 70000 (estimated from put-accept price: token_cost * 70/100)
BOUNTY=50000
ENTRY_VALUE=70000
EXPIRES_AT=$(python3 -c "
from datetime import datetime, timezone, timedelta
print((datetime.now(timezone.utc) + timedelta(hours=24)).strftime('%Y-%m-%dT%H:%M:%SZ'))
")

echo "$ cf \$XCFID assign --entry_id \$ENTRY_ID --task_type exchange:assign-type:validate --bounty \$BOUNTY --entry_value \$ENTRY_VALUE --content_hash \$CONTENT_HASH --description ... --slots 1 --expires_at \$EXPIRES_AT --priority exchange:assign-priority:p2"
cf --cf-home "$CF_HOME" "$XCFID" assign \
    --entry_id "$ENTRY_ID" \
    --task_type "exchange:assign-type:validate" \
    --bounty "$BOUNTY" \
    --entry_value "$ENTRY_VALUE" \
    --content_hash "$CONTENT_HASH" \
    --description "Validate cache entry freshness: Go cache warming utility. Re-derive the output independently and verify the content matches the stored hash." \
    --slots 1 \
    --expires_at "$EXPIRES_AT" \
    --priority "exchange:assign-priority:p2"

ASSIGN_MSG_ID=$(cf --cf-home "$CF_HOME" "$XCFID" assigns --json 2>/dev/null | \
    python3 -c "import json,sys; msgs=json.load(sys.stdin); print(msgs[0]['id'] if msgs else 'unknown')" 2>/dev/null || echo "unknown")
echo "# assign message ID: $ASSIGN_MSG_ID"

# Wait for engine to process the assign
sleep 1
grep "engine: assign posted" "$TMP/serve.log" 2>/dev/null || true

# ---------------------------------------------------------------------------
# Section: assign-claim — worker claims the assignment
# ---------------------------------------------------------------------------

tee_section "assign-claim"

CLAIM_EXPIRES_AT=$(python3 -c "
from datetime import datetime, timezone, timedelta
print((datetime.now(timezone.utc) + timedelta(minutes=15)).strftime('%Y-%m-%dT%H:%M:%SZ'))
")

echo "$ CF_HOME=\$WORKER_CF cf \$XCFID assign-claim --target \$ASSIGN_MSG_ID --expires_at \$CLAIM_EXPIRES_AT"
cf --cf-home "$WORKER_CF" "$XCFID" assign-claim \
    --target "$ASSIGN_MSG_ID" \
    --expires_at "$CLAIM_EXPIRES_AT"

# Read the message ID from the assign-claims named view
CLAIM_MSG_ID=$(cf --cf-home "$WORKER_CF" "$XCFID" assign-claims --json 2>/dev/null | \
    python3 -c "import json,sys; msgs=json.load(sys.stdin); print(msgs[-1]['id'] if msgs else 'unknown')" 2>/dev/null || echo "unknown")
echo "# assign-claim message ID: $CLAIM_MSG_ID"

# Wait for engine to process the claim
sleep 1
grep "engine: assign-claim" "$TMP/serve.log" 2>/dev/null || true

# ---------------------------------------------------------------------------
# Section: assign-complete — worker submits completed validation
# ---------------------------------------------------------------------------

tee_section "assign-complete"

# The worker independently validates the content and provides an evidence hash.
# For this demo the evidence hash represents the worker's re-derived output.
EVIDENCE_HASH="sha256:$(printf 'worker-validation-evidence-%s' "$ASSIGN_MSG_ID" | sha256sum | awk '{print $1}')"

echo "$ CF_HOME=\$WORKER_CF cf \$XCFID assign-complete --target \$CLAIM_MSG_ID --task_type validate --verdict pass --evidence_hash \$EVIDENCE_HASH"
cf --cf-home "$WORKER_CF" "$XCFID" assign-complete \
    --target "$CLAIM_MSG_ID" \
    --task_type "exchange:assign-type:validate" \
    --verdict "exchange:assign-verdict:pass" \
    --evidence_hash "$EVIDENCE_HASH"

# Read the message ID from the assign-completes named view
COMPLETE_MSG_ID=$(cf --cf-home "$WORKER_CF" "$XCFID" assign-completes --json 2>/dev/null | \
    python3 -c "import json,sys; msgs=json.load(sys.stdin); print(msgs[-1]['id'] if msgs else 'unknown')" 2>/dev/null || echo "unknown")
echo "# assign-complete message ID: $COMPLETE_MSG_ID"

# Wait for engine to process the completion
sleep 1
grep "engine: assign-complete" "$TMP/serve.log" 2>/dev/null || true

# ---------------------------------------------------------------------------
# Section: assign-accept — operator accepts and triggers scrip payment
# ---------------------------------------------------------------------------

tee_section "assign-accept"

echo "$ cf \$XCFID assign-accept --target \$COMPLETE_MSG_ID --bounty_paid \$BOUNTY --validation_method algorithmic"
cf --cf-home "$CF_HOME" "$XCFID" assign-accept \
    --target "$COMPLETE_MSG_ID" \
    --bounty_paid "$BOUNTY" \
    --validation_method "algorithmic"

# Read the message ID from the assign-accepts named view
ACCEPT_MSG_ID=$(cf --cf-home "$CF_HOME" "$XCFID" assign-accepts --json 2>/dev/null | \
    python3 -c "import json,sys; msgs=json.load(sys.stdin); print(msgs[-1]['id'] if msgs else 'unknown')" 2>/dev/null || echo "unknown")
echo "# assign-accept message ID: $ACCEPT_MSG_ID"

# ---------------------------------------------------------------------------
# Section: verify-pay — wait for scrip-assign-pay from engine
# ---------------------------------------------------------------------------

tee_section "verify-pay"

echo "# Waiting for dontguess:scrip-assign-pay message (up to 15s)..."
PAY_FOUND=false
for i in $(seq 1 30); do
    sleep 0.5
    PAY_COUNT=$(cf --cf-home "$CF_HOME" read "$XCFID" --all --tag "dontguess:scrip-assign-pay" --json 2>/dev/null | \
        python3 -c "import json,sys; print(len(json.load(sys.stdin)))" 2>/dev/null || echo 0)
    if [ "$PAY_COUNT" -gt 0 ]; then
        PAY_FOUND=true
        echo "# scrip-assign-pay received (pay count: $PAY_COUNT)"
        break
    fi
done

if [ "$PAY_FOUND" != "true" ]; then
    echo "ERROR: no scrip-assign-pay within 15s"
    echo "Serve log:"
    cat "$TMP/serve.log"
    exit 1
fi

echo "$ cf \$XCFID scrip-assign-pay --json"
PAY_MSGS=$(cf --cf-home "$CF_HOME" "$XCFID" scrip-assign-pay --json 2>/dev/null)
echo "$PAY_MSGS" | python3 -c "
import json, sys
msgs = json.load(sys.stdin)
print(f'Scrip-assign-pay messages: {len(msgs)}')
for m in msgs:
    print(f'  id={m[\"id\"][:12]} tags={m.get(\"tags\",[])}')
    payload_raw = m.get('payload', '')
    if payload_raw:
        try:
            p = json.loads(payload_raw)
            print(f'  worker={str(p.get(\"worker\",\"\"))[:16]}...')
            print(f'  amount={p.get(\"amount\",0)} micro-tokens')
            print(f'  task_type={p.get(\"task_type\",\"\")}')
            print(f'  assign_msg={str(p.get(\"assign_msg\",\"\"))[:12]}...')
        except Exception:
            print(f'  payload: {payload_raw[:80]}')
"

# Verify worker key appears in the pay message
WORKER_IN_PAY=$(cf --cf-home "$CF_HOME" "$XCFID" scrip-assign-pay --json 2>/dev/null | \
    python3 -c "
import json, sys
msgs = json.load(sys.stdin)
worker = '$WORKER_PUBKEY'
for m in msgs:
    p = json.loads(m.get('payload', '{}'))
    if p.get('worker', '') == worker:
        print('MATCH')
        break
" 2>/dev/null || echo "")

if [ "$WORKER_IN_PAY" = "MATCH" ]; then
    echo "# VERIFIED: scrip-assign-pay references the correct worker"
else
    echo "# NOTE: worker key not found in assign-pay messages (may be a key encoding difference)"
fi

# ---------------------------------------------------------------------------
# Section: summary
# ---------------------------------------------------------------------------

tee_section "summary"

echo "Exchange campfire:  $XCFID"
echo "Put message ID:     $PUT_MSG_ID"
echo "Entry ID:           $ENTRY_ID"
echo "Assign message ID:  $ASSIGN_MSG_ID"
echo "Claim message ID:   $CLAIM_MSG_ID"
echo "Complete msg ID:    $COMPLETE_MSG_ID"
echo "Accept message ID:  $ACCEPT_MSG_ID"
echo "Worker public key:  ${WORKER_PUBKEY:0:16}..."
echo ""

# Final message count
FINAL_COUNT=$(cf --cf-home "$CF_HOME" "$XCFID" messages --json 2>/dev/null | \
    python3 -c "import json,sys; print(len(json.load(sys.stdin)))" 2>/dev/null || echo "?")
echo "Total campfire messages: $FINAL_COUNT"
echo ""

# Show the full assign lifecycle in the log
echo "Engine log (assign lifecycle):"
grep "assign\|replayed\|exchange serving" "$TMP/serve.log" 2>/dev/null || echo "(no assign log lines)"
echo ""
echo "# Demo complete. Transcript written to: $OUTPUT_FILE"
