package main

// Enforcement proof for dontguess-4e3 finding A — METRIC-DISTORTION.
//
// A DEMAND-ONLY registration (67e0 ruling) is a D1-dropped unfunded miss stored
// under KindMatch with tags [buy-miss, match, demand-only] so `dontguess demand`
// can still surface the unmet demand. It is NOT a real buyer-facing match: it
// moves no scrip and MUST NOT move any REPORTED exchange metric. Before the fix,
// matchesFilter selected Kinds:[KindMatch] with no demand-only exclusion, so both
// production reporters folded it in:
//   - `dontguess status`   → readExchangeViews' countFilter(matchesFilter(0)).Matches
//   - `dontguess hit-rate` → runHitRate's readFilter(matchesFilter).ComputeHitRate,
//     whose classifyMatchResult sees the buy-miss tag and books a real buyer MISS.
//
// These tests drive the REAL reporters (readExchangeViews and the exact
// loadLocalMessages→matchesFilter→ComputeHitRate composition runHitRate uses over a
// real on-disk events.jsonl), NOT the in-package matchCount test helper. The proof
// is a strict ZERO delta: adding a demand-only registration to the same log leaves
// every reported match/hit/miss count byte-for-byte unchanged, while `demand`
// (buyMissFilter) still sees it.

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/3dl-dev/dontguess/pkg/exchange"
	"github.com/3dl-dev/dontguess/pkg/matching"
	dgstore "github.com/3dl-dev/dontguess/pkg/store"
)

const metricTestOperator = "0000000000000000000000000000000000000000000000000000000000000abc"

// writeExchangeLog writes recs to a fresh DG_HOME events.jsonl and returns dgHome.
func writeExchangeLog(t *testing.T, recs []dgstore.Record) string {
	t.Helper()
	dgHome := t.TempDir()
	st, err := dgstore.Open(filepath.Join(dgHome, localStoreFilename))
	if err != nil {
		t.Fatalf("dgstore.Open: %v", err)
	}
	for _, rec := range recs {
		if err := st.Append(rec); err != nil {
			t.Fatalf("Append %s: %v", rec.ID, err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return dgHome
}

// buyRec builds an exchange:buy local-store record for the given task.
func buyRec(id, task string, ts int64) dgstore.Record {
	payload, _ := json.Marshal(map[string]any{"task": task, "budget": 50000})
	return dgstore.Record{
		ID: id, CampfireID: "local", Sender: "buyer-" + id,
		Payload: payload, Tags: []string{exchange.TagBuy},
		Antecedents: []string{}, Timestamp: ts,
	}
}

// hitMatchRec builds a real HIT exchange:match record joined to buyID, carrying a
// results array with a high similarity so ComputeHitRate books a quality hit.
func hitMatchRec(id, buyID, task string, ts int64) dgstore.Record {
	payload, _ := json.Marshal(map[string]any{
		"results": []map[string]any{{
			"entry_id":            "entry-" + id,
			"description":         task,
			"similarity":          0.99,
			"token_cost_original": 8000,
		}},
	})
	return dgstore.Record{
		ID: id, CampfireID: "local", Sender: metricTestOperator,
		Payload: payload, Tags: []string{exchange.TagMatch},
		Antecedents: []string{buyID}, Timestamp: ts,
	}
}

// missMatchRec builds a real buy-miss exchange:match record joined to buyID.
func missMatchRec(id, buyID, task string, ts int64) dgstore.Record {
	payload, _ := json.Marshal(map[string]any{
		"buy_msg_id": buyID,
		"task_hash":  fmt.Sprintf("%064x", ts),
		"guide":      "No cached inference matched",
	})
	return dgstore.Record{
		ID: id, CampfireID: "local", Sender: metricTestOperator,
		Payload: payload, Tags: []string{exchange.TagBuyMiss, exchange.TagMatch},
		Antecedents: []string{buyID}, Timestamp: ts,
	}
}

// demandOnlyRec builds a demand-only registration exactly as registerDemandOnly
// emits it: [buy-miss, match, demand-only], joined to buyID.
func demandOnlyRec(id, buyID, buyerKey string, ts int64) dgstore.Record {
	payload, _ := json.Marshal(map[string]any{
		"buy_msg_id":         buyID,
		"task_hash":          fmt.Sprintf("%064x", ts),
		"buyer_key":          buyerKey,
		"demand_only":        true,
		"offered_price_rate": 0,
		"expires_at":         time.Unix(0, ts).UTC().Add(exchange.DemandOnlyTTL).Format(time.RFC3339),
	})
	return dgstore.Record{
		ID: id, CampfireID: "local", Sender: metricTestOperator,
		Payload: payload,
		Tags:    []string{exchange.TagBuyMiss, exchange.TagMatch, exchange.TagDemandOnly},
		Antecedents: []string{buyID}, Timestamp: ts,
	}
}

// computeHitRateForTest reproduces the EXACT reporter composition runHitRate uses
// (hitrate.go): loadLocalMessages → buysFilter/matchesFilter → ComputeHitRate. It
// deliberately drives the real production filter + ComputeHitRate, not the
// matchCount test helper.
func computeHitRateForTest(t *testing.T, dgHome string) exchange.HitRateReport {
	t.Helper()
	allMsgs, err := loadLocalMessages(dgHome)
	if err != nil {
		t.Fatalf("loadLocalMessages: %v", err)
	}
	buys := readFilter(allMsgs, buysFilter(0))
	matches := readFilter(allMsgs, matchesFilter(0))
	opts := exchange.HitRateOptions{
		MinSimilarity: matching.DefaultMinSimilarity(),
		Embedder:      buildRecomputeEmbedder(buys, matches),
		BuyTasks:      buildBuyTaskMap(buys),
	}
	return exchange.ComputeHitRate(buys, matches, opts)
}

// baseLog is the shared real-traffic fixture: one HIT and one real MISS, each with
// its own buy. ts values are recent so the default (all-history) window includes
// them.
func baseLog(now int64) []dgstore.Record {
	return []dgstore.Record{
		buyRec("buy-hit", "reusable go flock contention test pattern", now-900),
		hitMatchRec("match-hit", "buy-hit", "reusable go flock contention test pattern", now-800),
		buyRec("buy-miss", "an extremely novel task nobody has cached before", now-700),
		missMatchRec("match-miss", "buy-miss", "an extremely novel task nobody has cached before", now-600),
	}
}

// TestDemandOnly_ZeroDeltaInStatusMatchCount proves the real `dontguess status`
// match count is UNMOVED by a demand-only registration.
func TestDemandOnly_ZeroDeltaInStatusMatchCount(t *testing.T) {
	t.Parallel()
	now := time.Now().UnixNano()
	cutoff := time.Now().Add(-time.Hour)

	without := writeExchangeLog(t, baseLog(now))
	baseCounts, _, err := readExchangeViews(without, cutoff)
	if err != nil {
		t.Fatalf("readExchangeViews (without): %v", err)
	}

	withDemand := append(baseLog(now),
		buyRec("buy-do", "unfunded sybil task", now-500),
		demandOnlyRec("match-do", "buy-do", "sybil-buyer", now-400),
	)
	doCounts, _, err := readExchangeViews(writeExchangeLog(t, withDemand), cutoff)
	if err != nil {
		t.Fatalf("readExchangeViews (with demand-only): %v", err)
	}

	// The real match count is exactly the two REAL matches (hit + miss), with or
	// without the demand-only registration in the log.
	if baseCounts.Matches != 2 {
		t.Fatalf("baseline status Matches = %d, want 2 (hit + real miss)", baseCounts.Matches)
	}
	if doCounts.Matches != baseCounts.Matches {
		t.Fatalf("status Matches moved by a demand-only registration: %d -> %d (want ZERO delta; finding A metric-distortion not closed)", baseCounts.Matches, doCounts.Matches)
	}
}

// TestDemandOnly_ZeroDeltaInHitRateReport proves the real `dontguess hit-rate`
// reporter (ComputeHitRate over the production matchesFilter) books NEITHER a hit
// NOR a miss for a demand-only registration — every reported count is unchanged.
func TestDemandOnly_ZeroDeltaInHitRateReport(t *testing.T) {
	t.Parallel()
	now := time.Now().UnixNano()

	base := computeHitRateForTest(t, writeExchangeLog(t, baseLog(now)))

	withDemand := append(baseLog(now),
		buyRec("buy-do", "unfunded sybil task", now-500),
		demandOnlyRec("match-do", "buy-do", "sybil-buyer", now-400),
	)
	withDo := computeHitRateForTest(t, writeExchangeLog(t, withDemand))

	// Baseline sanity: exactly one hit, one miss, two match-results.
	if base.Hits != 1 || base.Misses != 1 || base.MatchResultsTotal != 2 {
		t.Fatalf("baseline report unexpected: hits=%d misses=%d matchResults=%d, want 1/1/2", base.Hits, base.Misses, base.MatchResultsTotal)
	}
	// Zero delta on every count the demand-only registration could distort.
	if withDo.Misses != base.Misses {
		t.Errorf("hit-rate Misses moved by demand-only: %d -> %d (classifyMatchResult booked a phantom buyer MISS — finding A)", base.Misses, withDo.Misses)
	}
	if withDo.Hits != base.Hits {
		t.Errorf("hit-rate Hits moved by demand-only: %d -> %d", base.Hits, withDo.Hits)
	}
	if withDo.MatchResultsTotal != base.MatchResultsTotal {
		t.Errorf("hit-rate MatchResultsTotal moved by demand-only: %d -> %d (the demand-only match was folded into the metric)", base.MatchResultsTotal, withDo.MatchResultsTotal)
	}
	if withDo.HitRatePct != base.HitRatePct {
		t.Errorf("hit-rate percentage moved by demand-only: %.4f -> %.4f", base.HitRatePct, withDo.HitRatePct)
	}
}

// TestDemandOnly_StillSurfacedToDemandCommand is the over-exclusion guard: the fix
// must remove demand-only from the METRIC views (matchesFilter) WITHOUT hiding it
// from `dontguess demand` (buyMissFilter), which exists precisely to surface unmet
// demand. A demand-only registration must still appear in the buy-miss view.
func TestDemandOnly_StillSurfacedToDemandCommand(t *testing.T) {
	t.Parallel()
	now := time.Now().UnixNano()
	withDemand := append(baseLog(now),
		buyRec("buy-do", "unfunded sybil task", now-500),
		demandOnlyRec("match-do", "buy-do", "sybil-buyer", now-400),
	)
	dgHome := writeExchangeLog(t, withDemand)
	allMsgs, err := loadLocalMessages(dgHome)
	if err != nil {
		t.Fatalf("loadLocalMessages: %v", err)
	}

	// matchesFilter (metric view) must NOT contain the demand-only registration.
	for _, m := range readFilter(allMsgs, matchesFilter(0)) {
		if m.ID == "match-do" {
			t.Fatalf("matchesFilter surfaced the demand-only registration — it must be excluded from metric views")
		}
	}
	// buyMissFilter (demand view) MUST still contain it.
	found := false
	for _, m := range readFilter(allMsgs, buyMissFilter(0)) {
		if m.ID == "match-do" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("buyMissFilter dropped the demand-only registration — the fix over-excluded it from `dontguess demand`")
	}
}
