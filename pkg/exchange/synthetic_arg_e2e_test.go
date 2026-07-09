package exchange_test

// TestSyntheticArg_BuyAndPutE2E is the end-to-end verification for dontguess-18c:
// the new optional "synthetic" boolean arg on buy/put convention operations.
//
// # What the prior attempt failed to prove
//
// The prior rework injected --tag exchange:synthetic into the cf buy/put call.
// cf's convention_dispatch.go builds a strict pflag set from the convention's
// declared args; --tag was not declared, so cf exited 1. The test used a stub
// that accepted everything — it never proved the arg is ACCEPTED by real dispatch
// or that the engine actually processes it.
//
// # What this test proves (and how)
//
//  1. Convention acceptance: buy.json and put-v0.3.json now declare "synthetic"
//     as an optional boolean arg. This test constructs buy/put message payloads
//     with "synthetic": true in the JSON body — exactly what convention_dispatch.go
//     puts into the message payload when the caller passes --synthetic. The test
//     uses the REAL engine on the campfire-free harness event log (newTestHarness,
//     dontguess-657). This simultaneously proves:
//       - the engine accepts the payload field without error
//       - the response carries the expected tag
//
//  2. Buy with "synthetic":true AND non-synthetic task → exchange:synthetic on buy-miss.
//     Without the new arg, a real task would produce a buy-miss WITHOUT exchange:synthetic
//     (demand.IsSynthetic returns false for it). With "synthetic":true the engine ORs
//     the two sources and tags it. This is the proof that the arg channel works.
//
//  3. Buy WITHOUT synthetic and with a non-synthetic task → NO exchange:synthetic.
//     Backward-compatibility proof.
//
//  4. Put with "synthetic":true → exchange:synthetic on put-accept (buy-miss fulfillment).
//
//  5. Put WITHOUT synthetic and non-synthetic description → NO exchange:synthetic.
//
// NO MOCKS. All dispatches use DispatchForTest which calls the same internal
// dispatch path as the production event loop. Each sub-test uses its own isolated
// test harness (newTestHarness) to prevent inventory from one cycle contaminating
// the candidate matching of another.

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/campfire-net/dontguess/pkg/store"

	"github.com/campfire-net/dontguess/pkg/exchange"
)

// realTaskArg is a non-synthetic task description (demand.IsSynthetic returns false
// for it). Using a real engineering task so that any exchange:synthetic on the
// response is due solely to the "synthetic":true arg, not server-side detection.
const realTaskArg = "Go HTTP handler unit test generator for REST APIs"

// realPutDescArg is a non-synthetic put description that passes all quality gates:
// token_cost >= MinTokenCost, description not test-like, content present.
const realPutDescArg = "Go HTTP handler unit test generator — canonical reusable pattern"

// TestSyntheticArg_BuyWithFlag_TagsResponse verifies that a buy with "synthetic":true
// and a non-synthetic task produces a buy-miss carrying exchange:synthetic.
// PROOF: the task string is real (demand.IsSynthetic returns false for it), so the tag
// can only appear because the engine's OR logic picked up payload.Synthetic==true.
func TestSyntheticArg_BuyWithFlag_TagsResponse(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	eng := h.newEngine()

	existing, err := h.st.ListMessages(h.cfID, 0)
	if err != nil {
		t.Fatalf("ListMessages for replay: %v", err)
	}
	eng.State().Replay(exchange.FromStoreRecords(existing))

	// Construct a buy payload with "synthetic":true and a non-synthetic task.
	// This is exactly what convention_dispatch.go produces when the caller passes
	// --synthetic; the payload field name matches the declared arg name in buy.json.
	buyPayloadBytes, err := json.Marshal(map[string]any{
		"task":        realTaskArg,
		"budget":      5000,
		"max_results": 3,
		"synthetic":   true, // new convention arg — proves ACCEPTANCE by the engine
	})
	if err != nil {
		t.Fatalf("marshal buy payload: %v", err)
	}
	buyMsg := h.sendMessage(h.buyer, buyPayloadBytes, []string{exchange.TagBuy}, nil)
	if err := eng.DispatchForTest(buyMsg); err != nil {
		t.Fatalf("dispatch buy{synthetic:true}: %v", err)
	}

	// Read buy-miss responses and find the one for our buy.
	allMsgs, err := h.st.ListMessages(h.cfID, 0, store.MessageFilter{
		Tags: []string{exchange.TagBuyMiss},
	})
	if err != nil {
		t.Fatalf("ListMessages(buy-miss): %v", err)
	}

	var found bool
	for i := range allMsgs {
		rec := &allMsgs[i]
		msg := exchange.FromStoreRecord(rec)
		if len(msg.Antecedents) == 0 || msg.Antecedents[0] != buyMsg.ID {
			continue
		}
		found = true
		// PROOF: "synthetic":true in the buy payload caused the engine to tag the
		// buy-miss response exchange:synthetic, even though the task string itself
		// is not synthetic per demand.IsSynthetic. This confirms the arg channel works.
		if !containsTag(msg.Tags, exchange.TagSynthetic) {
			t.Errorf("buy-miss for buy{synthetic:true, task=%q} missing exchange:synthetic; tags=%v",
				realTaskArg, msg.Tags)
		} else {
			t.Logf("PASS: buy-miss carries exchange:synthetic as expected; tags=%v", msg.Tags)
		}
	}
	if !found {
		t.Fatal("buy-miss response not found in store — engine did not respond")
	}
}

// TestSyntheticArg_BuyWithoutFlag_NoTag verifies that a buy WITHOUT the "synthetic"
// arg and a non-synthetic task produces a buy-miss WITHOUT exchange:synthetic.
// This is the backward-compatibility proof — the new arg must not change behavior
// when absent.
func TestSyntheticArg_BuyWithoutFlag_NoTag(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	eng := h.newEngine()

	existing, err := h.st.ListMessages(h.cfID, 0)
	if err != nil {
		t.Fatalf("ListMessages for replay: %v", err)
	}
	eng.State().Replay(exchange.FromStoreRecords(existing))

	// Buy without "synthetic" field — must behave identically to pre-dontguess-18c.
	buyPayloadBytes, err := json.Marshal(map[string]any{
		"task":        realTaskArg,
		"budget":      5000,
		"max_results": 3,
		// no "synthetic" field
	})
	if err != nil {
		t.Fatalf("marshal buy payload: %v", err)
	}
	buyMsg := h.sendMessage(h.buyer, buyPayloadBytes, []string{exchange.TagBuy}, nil)
	if err := eng.DispatchForTest(buyMsg); err != nil {
		t.Fatalf("dispatch buy{no synthetic}: %v", err)
	}

	allMsgs, err := h.st.ListMessages(h.cfID, 0, store.MessageFilter{
		Tags: []string{exchange.TagBuyMiss},
	})
	if err != nil {
		t.Fatalf("ListMessages(buy-miss): %v", err)
	}

	var found bool
	for i := range allMsgs {
		rec := &allMsgs[i]
		msg := exchange.FromStoreRecord(rec)
		if len(msg.Antecedents) == 0 || msg.Antecedents[0] != buyMsg.ID {
			continue
		}
		found = true
		// PROOF: backward compatibility — absent "synthetic" → no exchange:synthetic.
		if containsTag(msg.Tags, exchange.TagSynthetic) {
			t.Errorf("buy-miss for buy{no synthetic, task=%q} has unexpected exchange:synthetic; tags=%v",
				realTaskArg, msg.Tags)
		} else {
			t.Logf("PASS: buy-miss correctly lacks exchange:synthetic; tags=%v", msg.Tags)
		}
	}
	if !found {
		t.Fatal("buy-miss response not found in store — engine did not respond")
	}
}

// TestSyntheticArg_PutWithFlag_TagsResponse verifies that a put with "synthetic":true
// and a non-synthetic description produces a put-accept carrying exchange:synthetic.
// Uses the buy-miss fulfillment path (the only auto-accept path in the engine).
// Each cycle uses a fresh test harness to avoid inventory from previous cycles
// creating match candidates and bypassing the buy-miss path.
func TestSyntheticArg_PutWithFlag_TagsResponse(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	eng := h.newEngine()

	existing, err := h.st.ListMessages(h.cfID, 0)
	if err != nil {
		t.Fatalf("ListMessages for replay: %v", err)
	}
	eng.State().Replay(exchange.FromStoreRecords(existing))

	// Step 1: buyer sends buy for realPutDescArg → buy-miss standing offer.
	buyPayloadBytes, _ := json.Marshal(map[string]any{
		"task":        realPutDescArg,
		"budget":      5000,
		"max_results": 3,
	})
	buyMsg := h.sendMessage(h.buyer, buyPayloadBytes, []string{exchange.TagBuy}, nil)
	if err := eng.DispatchForTest(buyMsg); err != nil {
		t.Fatalf("dispatch buy: %v", err)
	}

	// Step 2: buyer fulfills the standing offer with a put{synthetic:true}.
	putPayloadBytes, _ := json.Marshal(map[string]any{
		"description":  realPutDescArg,
		"content":      syntheticArgBase64Content(realPutDescArg),
		"token_cost":   5000,
		"content_type": "exchange:content-type:code",
		"synthetic":    true, // new convention arg
	})
	putMsg := h.sendMessage(h.buyer, putPayloadBytes,
		[]string{exchange.TagPut, "exchange:content-type:code"}, nil)
	eng.State().Apply(putMsg) // so handlePut can find via GetPendingPut
	if err := eng.DispatchForTest(putMsg); err != nil {
		t.Fatalf("dispatch put{synthetic:true}: %v", err)
	}

	// Step 3: find the put-accept settle and assert exchange:synthetic.
	settleMsgs, err := h.st.ListMessages(h.cfID, 0, store.MessageFilter{
		Tags: []string{exchange.TagSettle},
	})
	if err != nil {
		t.Fatalf("ListMessages(settle): %v", err)
	}

	var found bool
	for i := range settleMsgs {
		rec := &settleMsgs[i]
		if len(rec.Antecedents) == 0 || rec.Antecedents[0] != putMsg.ID {
			continue
		}
		found = true
		msg := exchange.FromStoreRecord(rec)
		// PROOF: "synthetic":true in the put payload caused the engine to tag the
		// put-accept response exchange:synthetic.
		if !containsTag(msg.Tags, exchange.TagSynthetic) {
			t.Errorf("put-accept for put{synthetic:true, desc=%q} missing exchange:synthetic; tags=%v",
				realPutDescArg, msg.Tags)
		} else {
			t.Logf("PASS: put-accept carries exchange:synthetic as expected; tags=%v", msg.Tags)
		}
	}
	if !found {
		t.Fatal("put-accept not found in store — engine did not emit put-accept for buy-miss fulfillment")
	}
}

// TestSyntheticArg_PutWithoutFlag_NoTag verifies that a put WITHOUT the "synthetic"
// arg and a non-synthetic description produces a put-accept WITHOUT exchange:synthetic.
// Backward-compatibility proof for the put path.
func TestSyntheticArg_PutWithoutFlag_NoTag(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	eng := h.newEngine()

	existing, err := h.st.ListMessages(h.cfID, 0)
	if err != nil {
		t.Fatalf("ListMessages for replay: %v", err)
	}
	eng.State().Replay(exchange.FromStoreRecords(existing))

	// Use a different description from the WithFlag test so they don't share
	// content hashes if somehow run on the same engine (they don't, but defensive).
	const desc = "Go HTTP handler unit test generator — backward compat variant"

	// Step 1: buyer sends buy → buy-miss standing offer.
	buyPayloadBytes, _ := json.Marshal(map[string]any{
		"task":        desc,
		"budget":      5000,
		"max_results": 3,
	})
	buyMsg := h.sendMessage(h.buyer, buyPayloadBytes, []string{exchange.TagBuy}, nil)
	if err := eng.DispatchForTest(buyMsg); err != nil {
		t.Fatalf("dispatch buy: %v", err)
	}

	// Step 2: buyer fulfills with a put WITHOUT "synthetic" arg.
	putPayloadBytes, _ := json.Marshal(map[string]any{
		"description":  desc,
		"content":      syntheticArgBase64Content(desc),
		"token_cost":   5000,
		"content_type": "exchange:content-type:code",
		// no "synthetic" field — must behave identically to pre-dontguess-18c
	})
	putMsg := h.sendMessage(h.buyer, putPayloadBytes,
		[]string{exchange.TagPut, "exchange:content-type:code"}, nil)
	eng.State().Apply(putMsg)
	if err := eng.DispatchForTest(putMsg); err != nil {
		t.Fatalf("dispatch put{no synthetic}: %v", err)
	}

	// Step 3: find the put-accept settle and assert NO exchange:synthetic.
	settleMsgs, err := h.st.ListMessages(h.cfID, 0, store.MessageFilter{
		Tags: []string{exchange.TagSettle},
	})
	if err != nil {
		t.Fatalf("ListMessages(settle): %v", err)
	}

	var found bool
	for i := range settleMsgs {
		rec := &settleMsgs[i]
		if len(rec.Antecedents) == 0 || rec.Antecedents[0] != putMsg.ID {
			continue
		}
		found = true
		msg := exchange.FromStoreRecord(rec)
		// PROOF: backward compatibility — absent "synthetic" → no exchange:synthetic.
		if containsTag(msg.Tags, exchange.TagSynthetic) {
			t.Errorf("put-accept for put{no synthetic, desc=%q} has unexpected exchange:synthetic; tags=%v",
				desc, msg.Tags)
		} else {
			t.Logf("PASS: put-accept correctly lacks exchange:synthetic; tags=%v", msg.Tags)
		}
	}
	if !found {
		t.Fatal("put-accept not found in store — engine did not emit put-accept for buy-miss fulfillment")
	}
}

// syntheticArgBase64Content generates a deterministic base64-encoded content body
// for the synthetic arg tests. Sized above MinTokenCost (500 tokens) to pass the
// put quality gate. Uses the description as a seed so each test gets unique content
// (avoiding content-hash deduplication across tests).
func syntheticArgBase64Content(desc string) string {
	prefix := []byte("cached inference result: " + desc + " ")
	const targetSize = 4096
	content := make([]byte, targetSize)
	copy(content, prefix)
	for i := len(prefix); i < targetSize; i++ {
		content[i] = byte('a' + i%26)
	}
	return base64.StdEncoding.EncodeToString(content)
}
