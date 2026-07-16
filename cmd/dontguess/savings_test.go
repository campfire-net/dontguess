package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	dgstore "github.com/3dl-dev/dontguess/pkg/store"
)

// seedSavingsStore writes records to a fresh temp DG_HOME event log and returns
// the dgHome path. Records are appended in order with the given timestamps.
func seedSavingsStore(t *testing.T, recs []dgstore.Record) string {
	t.Helper()
	dgHome := t.TempDir()
	st, err := dgstore.Open(filepath.Join(dgHome, localStoreFilename))
	if err != nil {
		t.Fatalf("dgstore.Open: %v", err)
	}
	for _, r := range recs {
		if err := st.Append(r); err != nil {
			t.Fatalf("Append %s: %v", r.ID, err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return dgHome
}

func rec(id, sender, tag string, payload any, antecedents []string) dgstore.Record {
	b, _ := json.Marshal(payload)
	return dgstore.Record{
		ID:          id,
		CampfireID:  "local",
		Sender:      sender,
		Payload:     b,
		Tags:        []string{tag},
		Antecedents: antecedents,
		Timestamp:   time.Now().UnixNano(),
	}
}

const (
	keyBuyer  = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	keySeller = "ssssssssssssssssssssssssssssssssssssssssssssssssssssssssssssssss"
	keyOp     = "0000000000000000000000000000000000000000000000000000000000000000"
)

// TestValueReuse_Math pins the input/output-aware valuation arithmetic.
func TestValueReuse_Math(t *testing.T) {
	// 10,000 avoided tokens, 30% output, 5% consumption, Opus 4.8 rates.
	nt, nf := valueReuse(10_000, 0.30, 0.05, 5.0, 25.0)
	// avoided output = 3000 @ $25/MTok = 0.075 ; avoided input = 7000 @ $5 = 0.035
	// consume input = 500 @ $5 = 0.0025 (subtracted)
	if nt != 9500 {
		t.Errorf("net tokens = %v, want 9500 (10000 - 500 consume)", nt)
	}
	if diff := nf - 0.1075; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("net fiat = %v, want 0.1075", nf)
	}
}

// TestValueReuse_OutputCostsMore proves output-heavy work is worth more fiat than
// input-heavy work at the SAME token count — the whole reason fiat != tokens x rate.
func TestValueReuse_OutputCostsMore(t *testing.T) {
	_, fLo := valueReuse(100_000, 0.10, 0.0, 5.0, 25.0)
	_, fHi := valueReuse(100_000, 0.90, 0.0, 5.0, 25.0)
	if fHi <= fLo*2 {
		t.Errorf("output-heavy fiat %v should far exceed input-heavy %v", fHi, fLo)
	}
}

// TestFlexInt_BothForms guards the string ("8000") vs bare (8000) token_cost gap
// between CLI puts and marshaled match results.
func TestFlexInt_BothForms(t *testing.T) {
	var s struct {
		A flexInt `json:"a"`
		B flexInt `json:"b"`
		C flexInt `json:"c"`
	}
	if err := json.Unmarshal([]byte(`{"a":"8000","b":42000,"c":null}`), &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if int64(s.A) != 8000 || int64(s.B) != 42000 || int64(s.C) != 0 {
		t.Errorf("flexInt got a=%d b=%d c=%d, want 8000/42000/0", s.A, s.B, s.C)
	}
}

func TestIsSyntheticTask(t *testing.T) {
	if !isSyntheticTask("operator-heartbeat keepalive 20260714T060001Z") {
		t.Error("heartbeat should be synthetic")
	}
	if isSyntheticTask("implement continual-learning fitness harness") {
		t.Error("real task should not be synthetic")
	}
}

// TestCollectSavings_RealizedReuse is the end-to-end test: a match emitted by the
// OPERATOR, answering a BUYER's buy, delivering a SELLER's put. The buyer must be
// resolved from the antecedent buy (not the match sender), the reuse must count as
// realized cross-agent, and the valuation must match valueReuse.
func TestCollectSavings_RealizedReuse(t *testing.T) {
	matchPayload := map[string]any{
		"search_meta": map[string]any{"total_candidates": 12},
		"results": []map[string]any{{
			"entry_id":            "e1",
			"seller_key":          keySeller,
			"price":               3000,
			"similarity":          0.42,
			"token_cost_original": 40000,
			"description":         "reusable security-triage template library",
		}},
	}
	recs := []dgstore.Record{
		rec("buy1", keyBuyer, "exchange:buy", map[string]any{"task": "build triage templates", "budget": 5000}, nil),
		rec("put1", keySeller, "exchange:put", map[string]any{
			"token_cost": "40000", "content_type": "exchange:content-type:code",
			"description": "reusable security-triage template library"}, nil),
		rec("match1", keyOp, "exchange:match", matchPayload, []string{"buy1"}),
	}
	dgHome := seedSavingsStore(t, recs)

	rep, err := collectSavings(dgHome, 0, 5.0, 25.0, 0.30, 0.05)
	if err != nil {
		t.Fatalf("collectSavings: %v", err)
	}

	if rep.Realized.ReuseEvents != 1 {
		t.Fatalf("ReuseEvents = %d, want 1", rep.Realized.ReuseEvents)
	}
	d := rep.Realized.Detail[0]
	if d.Buyer != shortID(keyBuyer) {
		t.Errorf("buyer = %q, want buy-sender %q (must not be operator %q)", d.Buyer, shortID(keyBuyer), shortID(keyOp))
	}
	if d.Seller != shortID(keySeller) {
		t.Errorf("seller = %q, want %q", d.Seller, shortID(keySeller))
	}
	if rep.Realized.AvoidedRegenTokens != 40000 {
		t.Errorf("avoided = %d, want 40000", rep.Realized.AvoidedRegenTokens)
	}
	// net tokens = 40000 * (1 - 0.05) = 38000
	if rep.Realized.NetTokensSaved != 38000 {
		t.Errorf("net tokens = %d, want 38000", rep.Realized.NetTokensSaved)
	}
	// net fiat: out 12000@25 = 0.30 ; in (28000-2000)=26000@5 = 0.13 ; total 0.43
	if rep.Realized.NetFiatSavedUSD != 0.43 {
		t.Errorf("net fiat = %v, want 0.43", rep.Realized.NetFiatSavedUSD)
	}
	if rep.Economy.ReuseScripPaidByBuyers != 3000 {
		t.Errorf("reuse scrip paid = %d, want 3000", rep.Economy.ReuseScripPaidByBuyers)
	}
	// inventory: the one 40000 put is substantive
	if rep.Activity.PutsSubstantive != 1 || rep.Latent.InventoryAvoidableTokens != 40000 {
		t.Errorf("inventory subst=%d tokens=%d, want 1/40000",
			rep.Activity.PutsSubstantive, rep.Latent.InventoryAvoidableTokens)
	}
}

// TestCollectSavings_SelfReuseExcluded: buyer == seller and single-candidate must
// NOT count toward realized savings (self smoke test).
func TestCollectSavings_SelfReuseExcluded(t *testing.T) {
	matchPayload := map[string]any{
		"search_meta": map[string]any{"total_candidates": 1},
		"results": []map[string]any{{
			"entry_id": "e1", "seller_key": keyBuyer, "price": 1,
			"similarity": 0.9, "token_cost_original": 8000, "description": "self",
		}},
	}
	recs := []dgstore.Record{
		rec("buy1", keyBuyer, "exchange:buy", map[string]any{"task": "self", "budget": 1}, nil),
		rec("match1", keyOp, "exchange:match", matchPayload, []string{"buy1"}),
	}
	dgHome := seedSavingsStore(t, recs)

	rep, err := collectSavings(dgHome, 0, 5.0, 25.0, 0.30, 0.05)
	if err != nil {
		t.Fatalf("collectSavings: %v", err)
	}
	if rep.Realized.ReuseEvents != 0 {
		t.Errorf("ReuseEvents = %d, want 0 (self/trivial excluded)", rep.Realized.ReuseEvents)
	}
	if rep.Realized.TrivialExcluded != 1 {
		t.Errorf("TrivialExcluded = %d, want 1", rep.Realized.TrivialExcluded)
	}
}

// TestCollectSavings_JunkAndSyntheticClassification guards the hygiene counters.
func TestCollectSavings_JunkAndSyntheticClassification(t *testing.T) {
	recs := []dgstore.Record{
		rec("put1", keySeller, "exchange:put", map[string]any{"token_cost": "40000", "description": "real"}, nil),
		rec("put2", keySeller, "exchange:put", map[string]any{"token_cost": "100", "description": "smoke"}, nil),
		rec("buy1", keyBuyer, "exchange:buy", map[string]any{"task": "real content request"}, nil),
		rec("buy2", keyOp, "exchange:buy", map[string]any{"task": "operator-heartbeat keepalive"}, nil),
	}
	dgHome := seedSavingsStore(t, recs)
	rep, err := collectSavings(dgHome, 0, 5.0, 25.0, 0.30, 0.05)
	if err != nil {
		t.Fatalf("collectSavings: %v", err)
	}
	if rep.Activity.PutsSubstantive != 1 || rep.Activity.PutsJunkUnder500 != 1 {
		t.Errorf("puts subst=%d junk=%d, want 1/1", rep.Activity.PutsSubstantive, rep.Activity.PutsJunkUnder500)
	}
	if rep.Activity.BuysReal != 1 || rep.Activity.BuysSynthetic != 1 {
		t.Errorf("buys real=%d synth=%d, want 1/1", rep.Activity.BuysReal, rep.Activity.BuysSynthetic)
	}
}

// TestPrintSavings_TextOutput guards the human-readable headline blocks.
func TestPrintSavings_TextOutput(t *testing.T) {
	rep := &SavingsReport{
		SchemaVersion: 1,
		Window:        savingsWindow{FirstEvent: "2026-07-14T01:15:50Z", LastEvent: "2026-07-16T16:00:00Z", SpanHours: 62.9},
		Valuation:     valuationParams{InRate: 5, OutRate: 25, GenOutputFrac: 0.30, ConsumeFrac: 0.05},
		Activity:      savingsActivity{DistinctParticipants: 70, DistinctSellers: 43, PutsSubstantive: 44, BuysReal: 16, BuysSynthetic: 10, Ops: map[string]int{"exchange:put": 44}},
		Realized:      realizedSavings{ReuseEvents: 2, AvoidedRegenTokens: 75000, NetTokensSaved: 71250, NetFiatSavedUSD: 0.81},
		Latent:        latentSavings{InventoryEntries: 44, InventoryAvoidableTokens: 2555300, NetTokensIfEachReusedOnce: 2427535, NetFiatIfEachReusedOnceUSD: 27.47},
		Economy:       internalEconomy{ScripMinted: 207395},
	}
	out := captureStdout(t, func() { printSavings(rep, false) })
	for _, want := range []string{
		"REALIZED NET SAVINGS",
		"NET TOKENS SAVED    : 71,250",
		"NET FIAT SAVED      : $0.81",
		"Latent savings",
		"$27.47",
		"scrip minted=207,395",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("text output missing %q\nfull:\n%s", want, out)
		}
	}
}

// TestPrintSavings_JSONOutput guards the machine-readable shape.
func TestPrintSavings_JSONOutput(t *testing.T) {
	rep := &SavingsReport{SchemaVersion: 1, Realized: realizedSavings{NetFiatSavedUSD: 0.81, NetTokensSaved: 71250}}
	out := captureStdout(t, func() { printSavings(rep, true) })
	var back SavingsReport
	if err := json.Unmarshal([]byte(out), &back); err != nil {
		t.Fatalf("JSON did not round-trip: %v\n%s", err, out)
	}
	if back.Realized.NetTokensSaved != 71250 || back.Realized.NetFiatSavedUSD != 0.81 {
		t.Errorf("round-trip mismatch: %+v", back.Realized)
	}
}
