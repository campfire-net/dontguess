package exchange_test

// TestMatchResult_SimilarityFieldVaries verifies that the delivered match result
// carries a distinct "similarity" field (M2, dontguess-b26) that reflects the
// real cosine similarity between the buy task and the inventory entry, NOT the
// constant ~0.5 composite confidence value.
//
// Done-condition assertions (per item spec):
//  1. A high-overlap buy yields a similarity significantly above the partial-match
//     threshold (demonstrably not a constant 0.5).
//  2. A disjoint buy against the same inventory yields a much lower similarity.
//  3. The two similarity values differ by at least 0.1 — they are not constant.
//
// Uses the real engine path (DispatchForTest, real matchIndex). No mocks.
//
// Why no mock: the bug was that engine.go hardwired confidence (composite) into
// the result payload rather than threading the raw cosine similarity from
// matching.RankedResult. A mock would mask exactly the regression being tested.

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/campfire-net/dontguess/pkg/store"

	"github.com/campfire-net/dontguess/pkg/exchange"
)

// TestMatchResult_SimilarityFieldVaries is the M2 acceptance test.
// It proves that the delivered "similarity" field is the real cosine similarity,
// not a stubbed constant.
func TestMatchResult_SimilarityFieldVaries(t *testing.T) {
	t.Parallel()

	// matchResultSimilarity is the shape of a single result from the match payload.
	// We parse only the fields relevant to this test.
	type matchResultSimilarity struct {
		EntryID    string  `json:"entry_id"`
		Confidence float64 `json:"confidence"`
		Similarity float64 `json:"similarity"`
	}
	type matchPayload struct {
		Results []matchResultSimilarity `json:"results"`
	}

	// dispatchBuy is a helper that seeds the engine with the given put description,
	// triggers the given buy task, and returns the parsed match payload.
	// Returns nil if the engine emitted a buy-miss (no qualifying matches).
	dispatchBuy := func(t *testing.T, putDesc, buyTask string) *matchPayload {
		t.Helper()
		h := newTestHarness(t)
		eng := h.newEngine()

		// Seed one inventory entry.
		putMsg := h.sendMessage(h.seller,
			putPayload(putDesc, "sha256:b26000000000000000000000000000000000000000000000000000000000001", "code", 8000, 12000),
			[]string{exchange.TagPut, "exchange:content-type:code", "exchange:domain:go"},
			nil,
		)

		// Replay log and accept the put so it appears in inventory.
		msgs, err := h.st.ListMessages(h.cfID, 0)
		if err != nil {
			t.Fatalf("listing messages: %v", err)
		}
		eng.State().Replay(exchange.FromStoreRecords(msgs))

		if err := eng.AutoAcceptPut(putMsg.ID, 3000, time.Now().Add(72*time.Hour)); err != nil {
			t.Fatalf("AutoAcceptPut: %v", err)
		}

		// Verify inventory landed.
		if inv := eng.State().Inventory(); len(inv) != 1 {
			t.Fatalf("expected 1 inventory entry, got %d", len(inv))
		}

		// Send the buy.
		preMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})
		preCount := len(preMsgs)

		buyMsg := h.sendMessage(h.buyer,
			buyPayload(buyTask, 50000),
			[]string{exchange.TagBuy},
			nil,
		)
		buyRec, err := h.st.GetMessage(buyMsg.ID)
		if err != nil {
			t.Fatalf("getting buy record: %v", err)
		}

		eng.State().Apply(exchange.FromStoreRecord(buyRec))
		if err := eng.DispatchForTest(exchange.FromStoreRecord(buyRec)); err != nil {
			t.Fatalf("DispatchForTest: %v", err)
		}

		// Reload state.
		allMsgs, _ := h.st.ListMessages(h.cfID, 0)
		eng.State().Replay(exchange.FromStoreRecords(allMsgs))

		postMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})
		if len(postMsgs) <= preCount {
			t.Fatal("engine emitted no match-tagged message after buy")
		}
		emitted := postMsgs[len(postMsgs)-1]

		// If the engine emitted a buy-miss, return nil so the caller can decide.
		if hasTag(emitted.Tags, exchange.TagBuyMiss) {
			return nil
		}

		var mp matchPayload
		if err := json.Unmarshal(emitted.Payload, &mp); err != nil {
			t.Fatalf("parsing match payload: %v", err)
		}
		return &mp
	}

	// HIGH-OVERLAP case:
	// The put description and buy task share substantial vocabulary
	// (EventSink, contract, warm-worker, backends). TF-IDF cosine similarity
	// should be well above the 0.16 floor and the 0.5 partial-match threshold.
	highOverlapPut := "EventSink contract for warm-worker backends: emit accepted infer completed events"
	highOverlapBuy := "EventSink contract for warm-worker backends: how to emit inference events"

	highResult := dispatchBuy(t, highOverlapPut, highOverlapBuy)
	if highResult == nil {
		t.Fatal("high-overlap buy emitted a buy-miss; expected a real match. " +
			"Check that the put/buy descriptions share enough vocabulary to pass the 0.16 floor.")
	}
	if len(highResult.Results) == 0 {
		t.Fatal("high-overlap match payload has no results")
	}
	highSim := highResult.Results[0].Similarity
	t.Logf("high-overlap similarity = %.4f, confidence = %.4f", highSim, highResult.Results[0].Confidence)

	// DISJOINT case:
	// The put description is about Go locking patterns; the buy task is about
	// image processing pipelines. They share no domain vocabulary.
	// Similarity should be very low (likely 0 — no shared tokens after floor).
	// We use the SAME put description but a radically different buy task so the
	// inventory entry is the same entry evaluated twice with different queries.
	disjointPut := "EventSink contract for warm-worker backends: emit accepted infer completed events"
	disjointBuy := "image pipeline JPEG encoding resize thumbnail GPU CUDA compute shader"

	disjointResult := dispatchBuy(t, disjointPut, disjointBuy)
	// A buy-miss is the correct outcome for a disjoint pair — it means the entry
	// scored below the 0.16 floor and was correctly excluded. Record similarity = 0.
	var disjointSim float64
	if disjointResult == nil {
		// Buy-miss: similarity was below the floor (< 0.16). Use 0 for comparison.
		disjointSim = 0.0
		t.Logf("disjoint buy correctly emitted buy-miss (similarity below floor, recorded as 0 for comparison)")
	} else {
		if len(disjointResult.Results) == 0 {
			disjointSim = 0.0
		} else {
			disjointSim = disjointResult.Results[0].Similarity
		}
		t.Logf("disjoint-overlap similarity = %.4f, confidence = %.4f", disjointSim, disjointResult.Results[0].Confidence)
	}

	// ASSERTION 1: high-overlap similarity must be meaningfully above 0.5.
	// The composite confidence is ~0.5 for most entries (it's a weighted mix).
	// The raw cosine similarity for a strong lexical match should be clearly higher.
	const minHighSim = 0.50
	if highSim < minHighSim {
		t.Errorf("M2 DEFECT: high-overlap similarity = %.4f, want >= %.2f. "+
			"The 'similarity' field may still be the composite confidence (~0.5) rather than the raw cosine value. "+
			"Check that engine.go threads sr.Similarity (not sr.Confidence) into the rankedCandidate and MatchResult.",
			highSim, minHighSim)
	}

	// ASSERTION 2: disjoint similarity must be significantly lower than high-overlap.
	const minDelta = 0.10
	delta := highSim - disjointSim
	if delta < minDelta {
		t.Errorf("M2 DEFECT: similarity does not vary with query/entry overlap. "+
			"high-overlap=%.4f, disjoint=%.4f, delta=%.4f (want >= %.2f). "+
			"If both values are ~0.5, the 'similarity' field is still a constant composite, not the raw cosine.",
			highSim, disjointSim, delta, minDelta)
	}

	// ASSERTION 3: neither value must be exactly 0.5 (the known bad constant).
	// This guards against accidental reintroduction of the stub.
	const stubbedValue = 0.5
	const epsilon = 0.001
	if highSim > stubbedValue-epsilon && highSim < stubbedValue+epsilon {
		t.Errorf("M2 DEFECT: high-overlap similarity = %.4f is suspiciously close to the stubbed constant 0.5. "+
			"Verify that engine.go uses sr.Similarity (raw cosine), not sr.Confidence (composite).",
			highSim)
	}

	t.Logf("M2 PASS: high-overlap similarity=%.4f, disjoint similarity=%.4f, delta=%.4f (>= %.2f required)",
		highSim, disjointSim, delta, minDelta)
}
