package exchange_test

// TestSyntheticTagAndExclusion exercises the M3 synthetic traffic filter end-to-end:
//
//  1. A real exchange is set up (campfire, SQLite store, real engine).
//  2. A MIX of synthetic and real buy orders is dispatched through the engine.
//     No inventory is loaded, so every buy produces an exchange:buy-miss response.
//  3. The resulting exchange:buy-miss messages are read from the store and inspected:
//     - Synthetic responses MUST carry the exchange:synthetic tag.
//     - Real responses MUST NOT carry the exchange:synthetic tag.
//  4. ComputeHitRate is called on the buy + match message sets.
//     - SyntheticExcluded equals the count of synthetic buy-miss messages.
//     - TotalBuys equals only the real buy count.
//     - Hit/miss totals reflect only real buys.
//  5. A synthetic put (description == "test") is dispatched through handlePut.
//     The put-accept settle response must carry exchange:synthetic.
//
// NO MOCKS. The engine writes real exchange:buy-miss messages. The test reads
// them from the store exactly as the dontguess hit-rate reporter does in production.

import (
	"encoding/json"
	"testing"

	"github.com/campfire-net/dontguess/pkg/store"

	"github.com/campfire-net/dontguess/pkg/exchange"
)

// syntheticCases are buy tasks that match demand.IsSynthetic — one per distinct
// pattern category so the test covers the full synthetic predicate surface.
var syntheticCases = []struct {
	task    string
	pattern string
}{
	{"regression-parallel-178949-buy-0001", "regression- prefix"},
	{"regression-timeout-999-buy-0002", "regression-timeout prefix"},
	{"timeout-178 infrastructure probe", "contains timeout-178"},
	{"test", "exact 'test'"},
	{"upgrade smoke test cf v0.31.2 operator", "upgrade smoke test prefix"},
	{"parallel-3-105489", "parallel- prefix (load test series)"},
	{"zzqq nonsense xyzzy no such cached inference", "zzqq nonsense prefix"},
	{"validation-preflight-1776348953", "validation-preflight- prefix"},
	{"e2e-startup-probe-42", "e2e-startup-probe prefix"},
}

// realCases are buy tasks that are NOT synthetic — they describe real engineering
// work and must remain in the hit/miss counts.
var realCases = []struct {
	task    string
	comment string
}{
	{"campfire SDK convention declaration lifecycle management", "real campfire task"},
	{"audit test suite for untested endpoints, missing error paths", "real audit task"},
	{"RPT review of campfire SDK surface", "real review task"},
	{"FROST threshold key ceremony protocol for cold wallet signing", "real security task"},
	{"convention declaration revoke/supersede authorization lifecycle", "real convention task"},
}

// TestSyntheticTag_BuyMissTaggedAndExcluded is the primary M3 verification test.
// It is package-level external test (exchange_test) using the real engine harness.
func TestSyntheticTag_BuyMissTaggedAndExcluded(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	eng := h.newEngine()

	// Replay existing state (convention declarations written by Init).
	existing, err := h.st.ListMessages(h.cfID, 0)
	if err != nil {
		t.Fatalf("ListMessages for replay: %v", err)
	}
	eng.State().Replay(exchange.FromStoreRecords(existing))

	// --- Dispatch all buy orders (no inventory → every buy produces buy-miss) ---

	syntheticBuyIDs := make(map[string]string) // buyMsgID → task
	for _, tc := range syntheticCases {
		payload := syntheticBuyPayload(t, tc.task)
		msg := h.sendMessage(h.buyer, payload, []string{exchange.TagBuy}, nil)
		syntheticBuyIDs[msg.ID] = tc.task
		if err := eng.DispatchForTest(msg); err != nil {
			t.Fatalf("dispatch synthetic buy(%q): %v", tc.task, err)
		}
	}

	realBuyIDs := make(map[string]string) // buyMsgID → task
	for _, tc := range realCases {
		payload := syntheticBuyPayload(t, tc.task)
		msg := h.sendMessage(h.buyer, payload, []string{exchange.TagBuy}, nil)
		realBuyIDs[msg.ID] = tc.task
		if err := eng.DispatchForTest(msg); err != nil {
			t.Fatalf("dispatch real buy(%q): %v", tc.task, err)
		}
	}

	// --- Read exchange:buy-miss messages from the store ---

	allMsgs, err := h.st.ListMessages(h.cfID, 0, store.MessageFilter{
		Tags: []string{exchange.TagBuyMiss},
	})
	if err != nil {
		t.Fatalf("ListMessages(buy-miss): %v", err)
	}

	// --- Assert tag presence on each buy-miss response ---

	// Join buy-miss messages back to buy orders via antecedent.
	for i := range allMsgs {
		rec := &allMsgs[i]
		msg := exchange.FromStoreRecord(rec)

		// Resolve the originating buy ID (antecedent[0]).
		var origBuyID string
		if len(msg.Antecedents) > 0 {
			origBuyID = msg.Antecedents[0]
		}
		if origBuyID == "" {
			continue // skip buy-miss messages from Init / other traffic
		}

		syntheticTag := containsTag(msg.Tags, exchange.TagSynthetic)

		if _, isSynthetic := syntheticBuyIDs[origBuyID]; isSynthetic {
			if !syntheticTag {
				t.Errorf("buy-miss for synthetic buy(%q) is missing exchange:synthetic tag; tags=%v",
					syntheticBuyIDs[origBuyID], msg.Tags)
			}
		}

		if _, isReal := realBuyIDs[origBuyID]; isReal {
			if syntheticTag {
				t.Errorf("buy-miss for real buy(%q) has unexpected exchange:synthetic tag; tags=%v",
					realBuyIDs[origBuyID], msg.Tags)
			}
		}
	}

	// --- Build buy and match sets for ComputeHitRate ---

	// Collect all buy messages from the store (tagged exchange:buy).
	buyMsgs, err := h.st.ListMessages(h.cfID, 0, store.MessageFilter{
		Tags: []string{exchange.TagBuy},
	})
	if err != nil {
		t.Fatalf("ListMessages(buy): %v", err)
	}
	// Filter to only the buy orders we sent in this test.
	var buys []exchange.Message
	allTestBuyIDs := make(map[string]struct{})
	for id := range syntheticBuyIDs {
		allTestBuyIDs[id] = struct{}{}
	}
	for id := range realBuyIDs {
		allTestBuyIDs[id] = struct{}{}
	}
	for i := range buyMsgs {
		rec := &buyMsgs[i]
		if _, ok := allTestBuyIDs[rec.ID]; ok {
			buys = append(buys, *exchange.FromStoreRecord(rec))
		}
	}

	// Collect all match messages (both hits and misses carry exchange:match).
	matchMsgs, err := h.st.ListMessages(h.cfID, 0, store.MessageFilter{
		Tags: []string{exchange.TagMatch},
	})
	if err != nil {
		t.Fatalf("ListMessages(match): %v", err)
	}
	// Filter to matches whose antecedent is one of our test buy IDs.
	var matches []exchange.Message
	for i := range matchMsgs {
		rec := &matchMsgs[i]
		msg := exchange.FromStoreRecord(rec)
		if len(msg.Antecedents) > 0 {
			if _, ok := allTestBuyIDs[msg.Antecedents[0]]; ok {
				matches = append(matches, *msg)
			}
		}
	}

	// --- Assert ComputeHitRate excludes synthetics ---

	rep := exchange.ComputeHitRate(buys, matches)

	wantSyntheticExcluded := len(syntheticCases)
	if rep.SyntheticExcluded != wantSyntheticExcluded {
		t.Errorf("SyntheticExcluded = %d, want %d", rep.SyntheticExcluded, wantSyntheticExcluded)
	}

	wantTotalBuys := len(realCases)
	if rep.TotalBuys != wantTotalBuys {
		t.Errorf("TotalBuys = %d, want %d (only real buys)", rep.TotalBuys, wantTotalBuys)
	}

	// All real buys got a buy-miss response → all are matched (none pending).
	if rep.PendingBuys != 0 {
		t.Errorf("PendingBuys = %d, want 0 (every real buy got a buy-miss)", rep.PendingBuys)
	}

	// No inventory → no hits; all real buys are misses.
	if rep.Hits != 0 {
		t.Errorf("Hits = %d, want 0 (no inventory, all real buys miss)", rep.Hits)
	}
	if rep.Misses != wantTotalBuys {
		t.Errorf("Misses = %d, want %d (all real buys miss)", rep.Misses, wantTotalBuys)
	}

	t.Logf("ComputeHitRate: total=%d synthetic_excluded=%d real=%d matched=%d hits=%d misses=%d pending=%d",
		len(buys)+len(syntheticCases), rep.SyntheticExcluded,
		rep.TotalBuys, rep.MatchedBuys, rep.Hits, rep.Misses, rep.PendingBuys)
}

// TestSyntheticTag_PutAcceptTagged verifies that a put whose description matches
// demand.IsSynthetic causes the engine's put-accept response to carry
// exchange:synthetic. Uses the buy-miss fulfillment path (handlePut) which is
// the only auto-accept path for puts in the engine.
//
// Flow:
//  1. Buyer sends a synthetic buy ("test") → engine dispatches → emits buy-miss
//     standing offer (tagged exchange:synthetic on the miss).
//  2. The same buyer fulfills the standing offer by dispatching a put with the
//     matching description.
//  3. Engine dispatches the put → handlePut finds the standing offer → emits
//     put-accept settle (should carry exchange:synthetic).
//
// All dispatches use DispatchForTest to avoid the full replay+event-loop of
// Start, which would skip already-matched buys on replay.
func TestSyntheticTag_PutAcceptTagged(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	eng := h.newEngine()

	// Replay existing state (convention declarations written by Init).
	existing, err := h.st.ListMessages(h.cfID, 0)
	if err != nil {
		t.Fatalf("ListMessages for replay: %v", err)
	}
	eng.State().Replay(exchange.FromStoreRecords(existing))

	// Step 1: buyer sends a synthetic buy → buy-miss standing offer.
	// NOTE: the task must be synthetic per demand.IsSynthetic (so the buy-miss and
	// put-accept get tagged exchange:synthetic) BUT must not be rejected by the ed1
	// put quality-gate isTestLikeDescription (which drops bare "test"/"upgrade smoke
	// test" puts before they reach the put-accept path). "validation-preflight-*" is
	// synthetic-but-acceptable, so the put-accept synthetic-tag path stays reachable.
	syntheticTask := "validation-preflight-probe-123"
	buyMsg := h.sendMessage(h.buyer,
		syntheticBuyPayload(t, syntheticTask),
		[]string{exchange.TagBuy},
		nil,
	)
	if err := eng.DispatchForTest(buyMsg); err != nil {
		t.Fatalf("dispatch synthetic buy: %v", err)
	}

	// Step 2: buyer puts a result for the same task (buy-miss fulfillment path).
	// The engine's handlePut matches by TaskDescriptionHash(description) against
	// the stored buy-miss standing offer.
	putMsg := h.sendMessage(h.buyer,
		putPayload(syntheticTask, "sha256:"+zeroHash(), "code", 1000, 1024),
		[]string{exchange.TagPut, "exchange:content-type:code"},
		nil,
	)
	// Apply the put to state so handlePut can find it via GetPendingPut.
	eng.State().Apply(putMsg)
	// Dispatch the put — triggers buy-miss fulfillment → put-accept settle.
	if err := eng.DispatchForTest(putMsg); err != nil {
		t.Fatalf("dispatch synthetic put: %v", err)
	}

	// Step 3: read settle messages from the store and find the put-accept.
	settleMsgs, err := h.st.ListMessages(h.cfID, 0, store.MessageFilter{
		Tags: []string{exchange.TagSettle},
	})
	if err != nil {
		t.Fatalf("ListMessages(settle): %v", err)
	}

	var putAcceptMsg *exchange.Message
	for i := range settleMsgs {
		rec := &settleMsgs[i]
		if len(rec.Antecedents) > 0 && rec.Antecedents[0] == putMsg.ID {
			m := exchange.FromStoreRecord(rec)
			putAcceptMsg = m
			break
		}
	}

	if putAcceptMsg == nil {
		t.Fatal("put-accept settle message not found after dispatch")
	}

	// Assert: the put-accept for a synthetic put must carry exchange:synthetic.
	if !containsTag(putAcceptMsg.Tags, exchange.TagSynthetic) {
		t.Errorf("put-accept for synthetic put(description=%q) missing exchange:synthetic tag; tags=%v",
			syntheticTask, putAcceptMsg.Tags)
	}

	t.Logf("synthetic put-accept tags: %v", putAcceptMsg.Tags)
}

// zeroHash returns a 64-character zero-padded hex string for use as a content hash
// in tests. The hash format "sha256:<hex>" is required by handlePut validation.
func zeroHash() string {
	const n = 64
	b := make([]byte, n)
	for i := range b {
		b[i] = '0'
	}
	return string(b)
}

// --- helpers ---

// syntheticBuyPayload builds an exchange:buy JSON payload with the given task.
// Uses a distinct name from the demand integration test's demandBuyPayload to
// avoid conflict (different test file, same external test package).
func syntheticBuyPayload(t *testing.T, task string) []byte {
	t.Helper()
	p := map[string]any{
		"task":           task,
		"budget":         5000,
		"max_results":    3,
		"min_reputation": 0,
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal buy payload: %v", err)
	}
	return b
}

// containsTag reports whether tags contains the given tag string.
func containsTag(tags []string, tag string) bool {
	for _, t := range tags {
		if t == tag {
			return true
		}
	}
	return false
}
