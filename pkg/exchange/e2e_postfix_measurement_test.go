package exchange_test

// TestE2E_PostFixMeasurement is the end-to-end re-measurement test for
// dontguess-a8c. It exercises the full post-fix pipeline (M1a floor+enforcement,
// M2 similarity, M3 synthetic, M4 cap, ed1 put-gate, af8 honest reporter) on
// FRESH traffic with REAL similarities, using a scratch exchange (in-test fs
// campfire + SQLite store). No mocks. No live exchange.
//
// Structure:
//  1. PUT a controlled inventory: one junk entry that passes the put-gate but
//     scores below the 0.16 floor against unrelated tasks, plus several
//     substantive entries from the §4 reuse fixtures.
//  2. NONSENSE buys (§2 fixtures: "RPT review of campfire SDK surface",
//     "fix convention.Server subscribe cursor"): must emit exchange:buy-miss
//     against the junk entry. Headline regression: nonsense pairings gone.
//  3. SUBSTANTIVE buys that genuinely match §4 entries: must emit exchange:match
//     (not buy-miss) with real similarity ≥ floor as delivered by the engine.
//  4. Feed all buy+match messages through ComputeHitRate with the floor set to
//     matching.DefaultMinSimilarity(). The resulting HitRatePct must reflect
//     reality: nonsense buys are misses, genuine matches are hits. Reports the
//     honest rate on the controlled dataset.
//
// This test directly addresses the "recompute-approximation caveat" from the af8
// review: all similarity values here are REAL engine-computed similarity (M2/b26
// payload field), not historical recomputes. The rate is grounded in fresh,
// controlled traffic.
//
// Done conditions:
//  - All nonsense buys → exchange:buy-miss (TagBuyMiss present)
//  - All substantive buys → exchange:match (TagBuyMiss absent, results present)
//  - Substantive match results carry similarity ≥ matching.DefaultMinSimilarity()
//  - ComputeHitRate HitRatePct reflects nonsense=miss, genuine=hit structure
//  - Reported rate is NOT inflated to ~96% (the known pre-fix bad value)

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/campfire-net/campfire/cf-protocol/store"

	"github.com/campfire-net/dontguess/pkg/exchange"
	"github.com/campfire-net/dontguess/pkg/matching"
)

// TestE2E_PostFixMeasurement is the primary end-to-end validation test for
// the complete fix tree (dontguess-7d6+b26+e93+ed1+af8+720). It proves on
// fresh, controlled traffic that:
//  1. Nonsense pairings are gone (§2 buys → miss, not served junk).
//  2. Substantive matches hit with real similarity ≥ floor (§4 pairs).
//  3. ComputeHitRate reports an honest quality-weighted rate.
func TestE2E_PostFixMeasurement(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	eng := h.newEngine()

	// Replay existing state (convention declarations written by Init).
	existing, err := h.st.ListMessages(h.cfID, 0)
	if err != nil {
		t.Fatalf("ListMessages for initial replay: %v", err)
	}
	eng.State().Replay(exchange.FromStoreRecords(existing))

	floor := matching.DefaultMinSimilarity()

	// -------------------------------------------------------------------------
	// Step 1: PUT controlled inventory
	// -------------------------------------------------------------------------
	//
	// 1a. Junk entry: passes the ed1 put-gate (token_cost ≥ MinTokenCost=200,
	//     description is not "test"/"upgrade smoke test") but scores below the
	//     0.16 floor against unrelated tasks. Description resembles the live D1
	//     junk class but with wording that passes the gate.
	//
	//     The pre-fix behavior: this entry was served as a HIT to any buy (via
	//     the fallback path in handleBuy). Post-fix (M1a+720): it's gated out.
	//
	// 1b. Three substantive entries from the §4 reuse fixtures:
	//     - "legion.tools v1.2 schema correctness checklist"
	//     - "cf-protocol README CF_NO_PINS pin-skip mode"
	//     - "flock contention test pattern for Go concurrent map writes"
	//
	//     These pass both the put-gate and the floor for their matching buy tasks.

	// 1a. Junk entry (passes ed1 gate, expected to score below floor vs unrelated tasks).
	junkDesc := "smoke operator validation pre-release build artifact"
	junkPutMsg := h.sendMessage(h.seller,
		putPayload(junkDesc, "sha256:a8c0000000000000000000000000000000000000000000000000000000000001", "analysis", 500, 512),
		[]string{exchange.TagPut, "exchange:content-type:analysis"},
		nil,
	)

	msgs, err := h.st.ListMessages(h.cfID, 0)
	if err != nil {
		t.Fatalf("listing messages after junk put: %v", err)
	}
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	if err := eng.AutoAcceptPut(junkPutMsg.ID, 350, time.Now().Add(72*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut (junk): %v", err)
	}

	// 1b. Substantive entry 1: schema checklist
	schemaDesc := "legion.tools v1.2 schema correctness checklist: required fields, type constraints, enum values"
	schemaPutMsg := h.sendMessage(h.seller,
		putPayload(schemaDesc, "sha256:a8c0000000000000000000000000000000000000000000000000000000000002", "analysis", 8000, 12000),
		[]string{exchange.TagPut, "exchange:content-type:analysis"},
		nil,
	)

	msgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	if err := eng.AutoAcceptPut(schemaPutMsg.ID, 5600, time.Now().Add(72*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut (schema): %v", err)
	}

	// 1b. Substantive entry 2: CF_NO_PINS documentation
	pinsDesc := "cf-protocol README CF_NO_PINS pin-skip mode: disable pin enforcement for dev environments"
	pinsPutMsg := h.sendMessage(h.seller,
		putPayload(pinsDesc, "sha256:a8c0000000000000000000000000000000000000000000000000000000000003", "code", 6000, 8000),
		[]string{exchange.TagPut, "exchange:content-type:code"},
		nil,
	)

	msgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	if err := eng.AutoAcceptPut(pinsPutMsg.ID, 4200, time.Now().Add(72*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut (pins): %v", err)
	}

	// 1b. Substantive entry 3: Go flock contention test pattern
	flockDesc := "flock contention test pattern for Go concurrent map writes: sync.Mutex, race detector, parallel subtests"
	flockPutMsg := h.sendMessage(h.seller,
		putPayload(flockDesc, "sha256:a8c0000000000000000000000000000000000000000000000000000000000004", "code", 10000, 15000),
		[]string{exchange.TagPut, "exchange:content-type:code"},
		nil,
	)

	msgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	if err := eng.AutoAcceptPut(flockPutMsg.ID, 7000, time.Now().Add(72*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut (flock): %v", err)
	}

	// Verify inventory state: 4 entries (1 junk + 3 substantive).
	inv := eng.State().Inventory()
	if len(inv) != 4 {
		t.Fatalf("expected 4 inventory entries, got %d", len(inv))
	}

	t.Logf("Inventory seeded: 1 junk + 3 substantive entries")

	// -------------------------------------------------------------------------
	// Step 2: NONSENSE buys — §2 fixtures from the D1 review
	//
	// These tasks were the "headline regression": they matched the junk entry
	// in the live exchange before the fix (similarity ~0.116, below floor 0.16).
	// Post-fix: engine must emit exchange:buy-miss for all of them.
	// -------------------------------------------------------------------------

	nonsenseCases := []struct {
		task    string
		comment string
	}{
		{
			"RPT review of campfire SDK surface: offline send, relay create, naming CLI, multi-op install",
			"§2 primary nonsense fixture — matched junk entry pre-fix",
		},
		{
			"fix convention.Server subscribe cursor to persist across reconnect",
			"§2 secondary nonsense fixture — semantically unrelated to junk entry",
		},
		{
			"FROST threshold key ceremony signing wallet cold storage cryptography",
			"§2 tertiary — cryptography domain, no shared vocabulary with any inventory entry",
		},
	}

	preMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})
	preMatchCount := len(preMsgs)

	type buyRecord struct {
		msgID   string
		task    string
		comment string
	}

	var nonsenseBuyRecords []buyRecord
	for _, tc := range nonsenseCases {
		buyMsg := h.sendMessage(h.buyer,
			buyPayload(tc.task, 50000),
			[]string{exchange.TagBuy},
			nil,
		)
		buyRec, err := h.st.GetMessage(buyMsg.ID)
		if err != nil {
			t.Fatalf("GetMessage(nonsense buy %q): %v", tc.task[:20], err)
		}
		eng.State().Apply(exchange.FromStoreRecord(buyRec))
		if err := eng.DispatchForTest(exchange.FromStoreRecord(buyRec)); err != nil {
			t.Fatalf("DispatchForTest(nonsense buy %q): %v", tc.task[:20], err)
		}
		nonsenseBuyRecords = append(nonsenseBuyRecords, buyRecord{msgID: buyMsg.ID, task: tc.task, comment: tc.comment})
	}

	// Reload state to pick up emitted match messages.
	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	postNonsenseMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})
	newNonsenseMatches := len(postNonsenseMsgs) - preMatchCount
	if newNonsenseMatches < len(nonsenseCases) {
		t.Fatalf("engine emitted %d match messages for %d nonsense buys, want %d",
			newNonsenseMatches, len(nonsenseCases), len(nonsenseCases))
	}

	// Build a map from buy message ID to emitted match/miss message.
	nonsenseBuyToMatch := make(map[string]*store.MessageRecord)
	for i := range postNonsenseMsgs[preMatchCount:] {
		m := &postNonsenseMsgs[preMatchCount+i]
		if len(m.Antecedents) > 0 {
			// Check if antecedent is one of our nonsense buys.
			for _, br := range nonsenseBuyRecords {
				if m.Antecedents[0] == br.msgID {
					rec := m
					nonsenseBuyToMatch[br.msgID] = rec
				}
			}
		}
	}

	// PRIMARY ASSERTION: every nonsense buy must produce exchange:buy-miss.
	// This is the headline regression: nonsense pairings are gone.
	t.Log("--- Nonsense buys: asserting all → exchange:buy-miss ---")
	for _, br := range nonsenseBuyRecords {
		emitted, ok := nonsenseBuyToMatch[br.msgID]
		if !ok {
			t.Errorf("DEFECT: nonsense buy %q has no matching emitted message", br.task[:40])
			continue
		}
		if !hasTag(emitted.Tags, exchange.TagBuyMiss) {
			// Parse match payload for diagnostic detail.
			var mp struct {
				Results []struct {
					EntryID    string  `json:"entry_id"`
					Similarity float64 `json:"similarity"`
					Confidence float64 `json:"confidence"`
				} `json:"results"`
			}
			_ = json.Unmarshal(emitted.Payload, &mp)
			t.Errorf("DEFECT (dontguess-a8c): nonsense buy served as HIT, want exchange:buy-miss.\n"+
				"  task: %q\n  (%s)\n  tags: %v\n  match results: %v",
				br.task, br.comment, emitted.Tags, mp.Results)
		} else {
			t.Logf("PASS: nonsense buy → exchange:buy-miss  [%s]", br.task[:40])
		}
	}

	// -------------------------------------------------------------------------
	// Step 3: SUBSTANTIVE buys — genuine matches against §4 inventory entries
	//
	// These tasks share substantial vocabulary with the put descriptions.
	// Post-fix: engine must emit exchange:match (not buy-miss), and the top
	// result's similarity must be ≥ floor (real M2/b26 similarity field).
	// -------------------------------------------------------------------------

	substantiveCases := []struct {
		task      string
		putMsgID  string
		desc      string
		entryDesc string
	}{
		{
			task:      "legion.tools schema validation: check required fields and type constraints in v1.2",
			putMsgID:  schemaPutMsg.ID,
			desc:      "schema checklist matching",
			entryDesc: schemaDesc,
		},
		{
			task:      "cf-protocol pin-skip mode CF_NO_PINS: how to disable pin enforcement in development",
			putMsgID:  pinsPutMsg.ID,
			desc:      "CF_NO_PINS README matching",
			entryDesc: pinsDesc,
		},
		{
			task:      "Go concurrent map write test with flock contention and race detector",
			putMsgID:  flockPutMsg.ID,
			desc:      "flock contention test pattern matching",
			entryDesc: flockDesc,
		},
	}

	preSubstantiveMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})
	preSubstantiveCount := len(preSubstantiveMsgs)

	var substantiveBuyRecords []buyRecord
	for _, tc := range substantiveCases {
		buyMsg := h.sendMessage(h.buyer,
			buyPayload(tc.task, 50000),
			[]string{exchange.TagBuy},
			nil,
		)
		buyRec, err := h.st.GetMessage(buyMsg.ID)
		if err != nil {
			t.Fatalf("GetMessage(substantive buy %q): %v", tc.desc, err)
		}
		eng.State().Apply(exchange.FromStoreRecord(buyRec))
		if err := eng.DispatchForTest(exchange.FromStoreRecord(buyRec)); err != nil {
			t.Fatalf("DispatchForTest(substantive buy %q): %v", tc.desc, err)
		}
		substantiveBuyRecords = append(substantiveBuyRecords, buyRecord{msgID: buyMsg.ID, task: tc.task, comment: tc.desc})
	}

	// Reload state.
	allMsgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	postSubstantiveMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})
	newSubstantiveMatches := len(postSubstantiveMsgs) - preSubstantiveCount
	if newSubstantiveMatches < len(substantiveCases) {
		t.Fatalf("engine emitted %d match messages for %d substantive buys, want %d",
			newSubstantiveMatches, len(substantiveCases), len(substantiveCases))
	}

	// Build map: buy ID → emitted message.
	substantiveBuyToMatch := make(map[string]*store.MessageRecord)
	for i := range postSubstantiveMsgs[preSubstantiveCount:] {
		m := &postSubstantiveMsgs[preSubstantiveCount+i]
		if len(m.Antecedents) > 0 {
			for _, br := range substantiveBuyRecords {
				if m.Antecedents[0] == br.msgID {
					rec := m
					substantiveBuyToMatch[br.msgID] = rec
				}
			}
		}
	}

	// Parse match payload shape for similarity extraction.
	type matchResult struct {
		EntryID    string  `json:"entry_id"`
		Similarity float64 `json:"similarity"`
		Confidence float64 `json:"confidence"`
	}
	type matchPayload struct {
		Results []matchResult `json:"results"`
	}

	t.Log("--- Substantive buys: asserting all → exchange:match with similarity ≥ floor ---")

	var substantiveHitCount int
	var totalRealSimilarity float64

	for idx, br := range substantiveBuyRecords {
		tc := substantiveCases[idx]
		emitted, ok := substantiveBuyToMatch[br.msgID]
		if !ok {
			t.Errorf("DEFECT: substantive buy %q has no matching emitted message", tc.desc)
			continue
		}

		// PRIMARY ASSERTION: must NOT be a buy-miss.
		if hasTag(emitted.Tags, exchange.TagBuyMiss) {
			t.Errorf("DEFECT (dontguess-a8c): genuine substantive buy served as MISS, want exchange:match.\n"+
				"  task: %q\n  (%s)\n  tags: %v\n"+
				"  entry description: %q",
				br.task, tc.desc, emitted.Tags, tc.entryDesc)
			continue
		}

		// Must have the exchange:match tag.
		if !hasTag(emitted.Tags, exchange.TagMatch) {
			t.Errorf("substantive buy %q: response missing exchange:match tag, got %v", tc.desc, emitted.Tags)
			continue
		}

		// Parse results.
		var mp matchPayload
		if err := json.Unmarshal(emitted.Payload, &mp); err != nil {
			t.Fatalf("parsing match payload for %q: %v", tc.desc, err)
		}
		if len(mp.Results) == 0 {
			t.Errorf("DEFECT: substantive buy %q: match payload has no results", tc.desc)
			continue
		}

		topSim := mp.Results[0].Similarity
		t.Logf("PASS: substantive buy → exchange:match  [%s]\n"+
			"  top result similarity=%.4f, confidence=%.4f (floor=%.4f)",
			tc.desc, topSim, mp.Results[0].Confidence, floor)

		// SIMILARITY ASSERTION: top result must be ≥ floor.
		// This proves the M2 similarity field is present and real (not constant/stub).
		if topSim < floor {
			t.Errorf("DEFECT (dontguess-a8c): substantive buy %q: top result similarity=%.4f is below floor %.4f.\n"+
				"  The result should have been gated as a miss by the floor enforcement.\n"+
				"  If this test reaches here, the M1a floor gate is not correctly applied.",
				tc.desc, topSim, floor)
		}

		substantiveHitCount++
		totalRealSimilarity += topSim
	}

	// -------------------------------------------------------------------------
	// Step 4: Feed all buys+matches through ComputeHitRate (af8 reporter)
	//
	// The reporter is called with MinSimilarity=floor (quality-weighted mode).
	// Expected:
	//   - nonsense buys → miss
	//   - substantive buys → hit (similarity present in M2 payload, ≥ floor)
	//   - HitRatePct = substantive hits / total verified answered
	//   - Rate is NOT ~96% (the pre-fix inflated value)
	// -------------------------------------------------------------------------

	// Collect all buy messages for this test.
	allBuyMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagBuy}})
	allMatchMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})

	// Build the test buy ID set (nonsense + substantive).
	testBuyIDs := make(map[string]struct{})
	for _, br := range nonsenseBuyRecords {
		testBuyIDs[br.msgID] = struct{}{}
	}
	for _, br := range substantiveBuyRecords {
		testBuyIDs[br.msgID] = struct{}{}
	}

	// Filter to test buys only.
	var buys []exchange.Message
	for i := range allBuyMsgs {
		rec := &allBuyMsgs[i]
		if _, ok := testBuyIDs[rec.ID]; ok {
			buys = append(buys, *exchange.FromStoreRecord(rec))
		}
	}

	// Filter to test match messages only (antecedent is a test buy).
	var matches []exchange.Message
	for i := range allMatchMsgs {
		rec := &allMatchMsgs[i]
		if len(rec.Antecedents) > 0 {
			if _, ok := testBuyIDs[rec.Antecedents[0]]; ok {
				matches = append(matches, *exchange.FromStoreRecord(rec))
			}
		}
	}

	opts := exchange.HitRateOptions{
		MinSimilarity: floor,
		// No Embedder needed: all match messages are fresh M2 traffic with the
		// "similarity" field present (b26 payload). No historical recompute required.
	}

	report := exchange.ComputeHitRate(buys, matches, opts)

	t.Logf("=== ComputeHitRate report on controlled fresh traffic ===")
	t.Logf("  floor             : %.4f", floor)
	t.Logf("  total_buys        : %d (nonsense=%d + substantive=%d)",
		report.TotalBuys, len(nonsenseCases), len(substantiveCases))
	t.Logf("  matched_buys      : %d", report.MatchedBuys)
	t.Logf("  hits              : %d", report.Hits)
	t.Logf("  misses            : %d", report.Misses)
	t.Logf("  below_floor_downgraded: %d", report.BelowFloorDowngraded)
	t.Logf("  unverifiable_hits : %d", report.UnverifiableHits)
	t.Logf("  synthetic_excluded: %d", report.SyntheticExcluded)
	t.Logf("  hit_rate_pct      : %.2f%%", report.HitRatePct)
	if substantiveHitCount > 0 {
		t.Logf("  avg similarity (hits): %.4f", totalRealSimilarity/float64(substantiveHitCount))
	}
	t.Logf("  (pre-fix inflated rate was ~96.67%%; this fresh controlled rate reflects reality)")

	// HONEST RATE ASSERTIONS:

	// TotalBuys: all test buys in the window.
	wantTotalBuys := len(nonsenseCases) + len(substantiveCases)
	if report.TotalBuys != wantTotalBuys {
		t.Errorf("TotalBuys = %d, want %d", report.TotalBuys, wantTotalBuys)
	}

	// All buys should be matched (every buy got a response).
	if report.PendingBuys != 0 {
		t.Errorf("PendingBuys = %d, want 0 (every buy got a response)", report.PendingBuys)
	}

	// Hits: only substantive buys that produced a genuine above-floor match.
	// We can't assert the exact count without knowing whether TF-IDF actually
	// produces above-floor similarity for all three pairs (vocabulary overlap
	// depends on the corpus), so we assert at least 1 hit.
	if report.Hits < 1 {
		t.Errorf("Hits = %d, want ≥ 1 (at least one substantive buy must hit with real similarity ≥ floor)", report.Hits)
	}

	// Misses must include all nonsense buys.
	// Nonsense buys all produced buy-miss responses (asserted above in Step 2).
	// The reporter should count them as misses.
	wantMinMisses := len(nonsenseCases)
	if report.Misses < wantMinMisses {
		t.Errorf("Misses = %d, want ≥ %d (at minimum all nonsense buys must be misses)",
			report.Misses, wantMinMisses)
	}

	// HitRatePct: nonsense buys are misses; substantive buys are hits.
	// Rate must be strictly less than 100% (we have real misses from nonsense buys).
	if report.HitRatePct >= 100.0 {
		t.Errorf("HitRatePct = %.2f%%, want < 100%% (nonsense buys must register as misses)",
			report.HitRatePct)
	}

	// Rate must be strictly less than the pre-fix inflated 96.67%.
	// With 3 nonsense misses and ≤3 substantive hits, the maximum possible rate
	// is 3/(3+3)*100 = 50%. In practice it will be ≤50% because some substantive
	// buys may also miss (below-floor TF-IDF similarity for the task/entry pair).
	const preFixInflatedRate = 96.67
	if report.HitRatePct >= preFixInflatedRate {
		t.Errorf("HitRatePct = %.2f%%, want < %.2f%% (pre-fix inflated rate). "+
			"Reporter may still be counting below-floor hits.",
			report.HitRatePct, preFixInflatedRate)
	}

	// No unverifiable hits: all match messages are fresh M2 traffic with the
	// similarity field present. The reporter should not fall back to unverifiable.
	if report.UnverifiableHits != 0 {
		t.Errorf("UnverifiableHits = %d, want 0 (all match messages are fresh M2 traffic with similarity field)",
			report.UnverifiableHits)
	}

	// BelowFloorDowngraded: any delivered results (legacy hits) reclassified as
	// misses due to below-floor similarity. Count depends on actual TF-IDF scores.
	// This is informational; we don't assert a specific value but log it.
	t.Logf("  below_floor_downgraded (delivered but gated): %d", report.BelowFloorDowngraded)

	t.Logf("=== SUMMARY ===")
	t.Logf("  Nonsense pairings gone: %d/%d nonsense buys → miss (pre-fix: served junk hit at ~96.67%%)",
		len(nonsenseCases), len(nonsenseCases))
	t.Logf("  Genuine matches hit: %d/%d substantive buys → hit with real similarity",
		report.Hits, len(substantiveCases))
	t.Logf("  Honest hit-rate on controlled fresh traffic: %.2f%%", report.HitRatePct)
	t.Logf("  (af8 recompute caveat addressed: these are REAL similarities from M2/b26 payload)")
}
