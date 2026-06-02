package exchange_test

// put_quality_gate_test.go — enforcement tests for dontguess-ed1.
//
// Problem (measurement review §2): the live exchange accumulated junk inventory:
//   - "test" entry (token_cost=100) served 1,576 of 2,474 hits (64%) — nonsensical matches
//   - "upgrade smoke test cf v0.31.2 operator" entry polluted match quality
//   - 60% of real-agent buys were served junk entries
//
// Fix: applyPut now enforces three quality gates before accepting a put into
// pendingPuts. The gates compose with the existing 46f plausibility cap
// (token_cost ≤ content_size_bytes * MaxTokensPerByte):
//
//	§1  token_cost floor: reject if token_cost < MinTokenCost (200)
//	§2  content_hash dedup: reject if sha256(content) already in inventory or pendingPuts
//	§3  test-like description: reject bare "test" and "upgrade smoke test" prefix
//
// Covered (bundle: reject+accept in one test file, four functions):
//
//   - TestPutQualityGate_Rejects_MinTokenCost: token_cost=100 (< MinTokenCost=200) is
//     rejected at applyPut time — the put never enters pendingPuts.
//   - TestPutQualityGate_Rejects_DuplicateContentHash: a second put with the same
//     content as an existing pendingPuts entry is rejected.
//   - TestPutQualityGate_Rejects_TestLikeDescription: bare "test" description and
//     "upgrade smoke test" prefix are each rejected.
//   - TestPutQualityGate_Accept_SubstantivePut: a substantive put (plausible cost ≥
//     MinTokenCost, unique content, real description) passes all gates and completes
//     the full put-accept path through AutoAcceptPut.
//
// All rejection tests prove the path by:
//  1. Writing the put message to the campfire store (via sendMessage).
//  2. Calling State.Replay so applyPut runs on every exchange:put message.
//  3. Asserting PendingPuts() is empty — the ONLY way it can be empty after Replay
//     is if applyPut rejected the put.
//  4. Asserting AutoAcceptPut returns an error — nothing to accept.
//
// No mocks are used. The only bypass is the harness's ReadSkipSync=true
// (skips filesystem transport sync, not the quality-gate validation path).

import (
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/campfire-net/dontguess/pkg/exchange"
)

// buildQGPutPayload constructs an exchange:put payload for quality-gate tests.
// desc is the description, tokenCost is the declared inference cost, and
// contentSuffix is appended to make content bytes unique across test cases.
func buildQGPutPayload(t *testing.T, desc string, tokenCost int64, contentSuffix string) []byte {
	t.Helper()
	contentBytes := []byte("cached inference result: " + desc + " " + contentSuffix)
	encoded := base64.StdEncoding.EncodeToString(contentBytes)
	p, err := json.Marshal(map[string]any{
		"description":  desc,
		"content":      encoded,
		"token_cost":   tokenCost,
		"content_type": "exchange:content-type:code",
		"domains":      []string{"go"},
	})
	if err != nil {
		t.Fatalf("buildQGPutPayload: marshal: %v", err)
	}
	return p
}

// TestPutQualityGate_Rejects_MinTokenCost verifies that a put with token_cost below
// MinTokenCost is rejected by applyPut and never enters pendingPuts.
//
// Gate §1: token_cost floor (dontguess-ed1).
// Composition with 46f: 46f enforces the upper bound (token_cost ≤ content_size *
// MaxTokensPerByte). Gate §1 enforces the lower bound independently.
//
// The live junk "test" entry had token_cost=100. MinTokenCost=200 rejects it.
// Test uses token_cost=100 (<200) with plausible content to isolate gate §1.
func TestPutQualityGate_Rejects_MinTokenCost(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	eng := h.newEngine()

	// token_cost=100 < MinTokenCost(200) — must be rejected.
	putMsg := h.sendMessage(h.seller,
		buildQGPutPayload(t, "Go function that checks if a number is prime", 100, "unique-suffix-mintokencost"),
		[]string{exchange.TagPut, "exchange:content-type:code"},
		nil,
	)

	// Replay so applyPut runs on the put message.
	replayAll(t, h, eng)

	// Gate §1 must have rejected the put — PendingPuts is empty.
	pending := eng.State().PendingPuts()
	for _, e := range pending {
		if e.PutMsgID == putMsg.ID {
			t.Errorf("gate §1 (min token_cost) failed: put with token_cost=100 found in pendingPuts "+
				"(MinTokenCost=%d, put.TokenCost=100)", exchange.MinTokenCost)
		}
	}
	if len(pending) == 0 {
		t.Logf("PASS: min-token_cost put correctly rejected (token_cost=100 < MinTokenCost=%d)", exchange.MinTokenCost)
	}

	// AutoAcceptPut must fail — nothing in pendingPuts for this put.
	err := eng.AutoAcceptPut(putMsg.ID, 70, time.Now().Add(72*time.Hour))
	if err == nil {
		t.Error("AutoAcceptPut on min-token_cost-rejected put must return error, got nil")
	}
}

// TestPutQualityGate_Rejects_DuplicateContentHash verifies that a put whose content
// is identical to an existing entry in pendingPuts is rejected by applyPut.
//
// Gate §2: content-hash dedup (dontguess-ed1).
// Prevents sellers from re-putting identical content under a new description
// to bypass expiry, gain a pricing reset, or game the discovery ranking.
//
// Proof through real applyPut path:
//  1. First put: goes through applyPut → contentHashIndex → pendingPuts.
//  2. Second put: same payload bytes → same SHA-256 hash → applyPut checks
//     contentHashIndex, finds collision, silently drops the put.
//  3. AutoAcceptPut on the second put returns an error (nothing to accept).
func TestPutQualityGate_Rejects_DuplicateContentHash(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	eng := h.newEngine()

	// First put — accepted into pendingPuts.
	firstPayload := buildQGPutPayload(t, "Terraform AWS VPC module with private subnets", 5000, "dedup-unique-content")
	firstMsg := h.sendMessage(h.seller,
		firstPayload,
		[]string{exchange.TagPut, "exchange:content-type:code"},
		nil,
	)

	// Second put — identical payload bytes → identical content bytes → identical SHA-256 hash.
	// Different campfire message ID (sendMessage generates a new one), but same content.
	dupMsg := h.sendMessage(h.seller,
		firstPayload,
		[]string{exchange.TagPut, "exchange:content-type:code"},
		nil,
	)

	// Replay both puts so applyPut runs on each.
	replayAll(t, h, eng)

	// First put must be in pendingPuts; dup must not.
	pending := eng.State().PendingPuts()
	foundFirst := false
	for _, e := range pending {
		if e.PutMsgID == firstMsg.ID {
			foundFirst = true
		}
		if e.PutMsgID == dupMsg.ID {
			t.Errorf("gate §2 (content dedup) failed: duplicate put %s found in pendingPuts "+
				"(content hash already registered by first put %s)", dupMsg.ID[:8], firstMsg.ID[:8])
		}
	}
	if !foundFirst {
		t.Errorf("first put %s not in pendingPuts — expected accepted (no prior hash collision)", firstMsg.ID[:8])
	}
	t.Logf("PASS: duplicate put correctly rejected (only first put in pendingPuts, total=%d)", len(pending))

	// AutoAcceptPut on dup must fail.
	if err := eng.AutoAcceptPut(dupMsg.ID, 3500, time.Now().Add(72*time.Hour)); err == nil {
		t.Error("AutoAcceptPut on duplicate-content put must return error, got nil")
	}
}

// TestPutQualityGate_Rejects_TestLikeDescription verifies that puts with test-like
// descriptions are rejected by applyPut.
//
// Gate §3: test-like description rejection (dontguess-ed1).
// Two sub-cases for the junk classes from measurement review §2:
//
//	(a) bare "test" — the exact description of the live junk entry (1,576 hits).
//	(b) "upgrade smoke test cf v0.31.2 operator" — the smoke-test junk class prefix.
//
// Legitimate descriptions like "test coverage audit" are NOT rejected.
func TestPutQualityGate_Rejects_TestLikeDescription(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	eng := h.newEngine()

	// Case (a): bare "test" — exact description of the live junk entry.
	bareTestMsg := h.sendMessage(h.seller,
		buildQGPutPayload(t, "test", 1000, "bare-test-suffix"),
		[]string{exchange.TagPut, "exchange:content-type:code"},
		nil,
	)

	// Case (b): "upgrade smoke test" prefix — cf v0.31.2 operator smoke-test class.
	smokeTestMsg := h.sendMessage(h.seller,
		buildQGPutPayload(t, "upgrade smoke test cf v0.31.2 operator", 1000, "smoke-test-suffix"),
		[]string{exchange.TagPut, "exchange:content-type:code"},
		nil,
	)

	// Replay so applyPut runs on both.
	replayAll(t, h, eng)

	// Both must be absent from pendingPuts (gate §3 rejected them).
	pending := eng.State().PendingPuts()
	for _, e := range pending {
		if e.PutMsgID == bareTestMsg.ID {
			t.Errorf("gate §3 failed: bare 'test' description put found in pendingPuts (putMsgID=%s)", bareTestMsg.ID[:8])
		}
		if e.PutMsgID == smokeTestMsg.ID {
			t.Errorf("gate §3 failed: 'upgrade smoke test' description put found in pendingPuts (putMsgID=%s)", smokeTestMsg.ID[:8])
		}
	}
	t.Logf("PASS: test-like description puts correctly rejected (pending=%d)", len(pending))

	// AutoAcceptPut on both must fail.
	if err := eng.AutoAcceptPut(bareTestMsg.ID, 700, time.Now().Add(72*time.Hour)); err == nil {
		t.Error("AutoAcceptPut on bare-'test' description put must return error, got nil")
	}
	if err := eng.AutoAcceptPut(smokeTestMsg.ID, 700, time.Now().Add(72*time.Hour)); err == nil {
		t.Error("AutoAcceptPut on 'upgrade smoke test' description put must return error, got nil")
	}
}

// TestPutQualityGate_Accept_SubstantivePut verifies that a substantive put
// (plausible cost ≥ MinTokenCost, unique content, real description) passes all
// three quality gates and completes the full put-accept path.
//
// This is the acceptance case bundled with the rejection tests above.
// Proof through the REAL put-accept path:
//  1. sendMessage + Replay → applyPut → pendingPuts (asserted non-empty).
//  2. AutoAcceptPut (the standard operator accept method) succeeds without error.
//  3. Entry appears in Inventory() with correct fields — put-accept complete.
func TestPutQualityGate_Accept_SubstantivePut(t *testing.T) {
	t.Parallel()

	const (
		desc      = "Go flock contention test pattern using sync.Mutex and race detector"
		tokenCost = int64(8500)
	)

	h := newTestHarness(t)
	eng := h.newEngine()

	putMsg := h.sendMessage(h.seller,
		buildQGPutPayload(t, desc, tokenCost, "accept-unique-content"),
		[]string{exchange.TagPut, "exchange:content-type:code"},
		nil,
	)

	// Replay so applyPut runs.
	replayAll(t, h, eng)

	// Put must be in pendingPuts — all three gates passed.
	pending := eng.State().PendingPuts()
	var found bool
	for _, e := range pending {
		if e.PutMsgID == putMsg.ID {
			found = true
			if e.TokenCost != tokenCost {
				t.Errorf("entry TokenCost=%d in pendingPuts, want %d", e.TokenCost, tokenCost)
			}
			if e.Description != desc {
				t.Errorf("entry Description=%q in pendingPuts, want %q", e.Description, desc)
			}
		}
	}
	if !found {
		t.Fatalf("substantive put %s not in pendingPuts — applyPut incorrectly rejected it "+
			"(token_cost=%d ≥ MinTokenCost=%d, unique content, real description)",
			putMsg.ID[:8], tokenCost, exchange.MinTokenCost)
	}

	// Accept through the real AutoAcceptPut path (operator accept).
	price := tokenCost * 70 / 100 // 5950
	if err := eng.AutoAcceptPut(putMsg.ID, price, time.Now().Add(72*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut on substantive put: %v", err)
	}

	// Entry must be in inventory — full put-accept path complete.
	inv := eng.State().Inventory()
	var inInv bool
	for _, e := range inv {
		if e.PutMsgID == putMsg.ID {
			inInv = true
			if e.TokenCost != tokenCost {
				t.Errorf("inventory TokenCost=%d, want %d", e.TokenCost, tokenCost)
			}
			if e.PutPrice != price {
				t.Errorf("inventory PutPrice=%d, want %d", e.PutPrice, price)
			}
		}
	}
	if !inInv {
		t.Errorf("accepted put %s not in inventory — put-accept path incomplete", putMsg.ID[:8])
	}
	t.Logf("PASS: substantive put accepted into inventory (token_cost=%d, price=%d)", tokenCost, price)
}
