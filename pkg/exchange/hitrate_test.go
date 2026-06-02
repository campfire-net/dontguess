package exchange

import "testing"

// buyMsg builds an exchange:buy fixture with the real payload shape observed
// live: {"budget","max_results","min_reputation","task"}. The message ID is the
// buy order ID.
func buyMsg(id, task string) Message {
	return Message{
		ID:      id,
		Tags:    []string{TagBuy},
		Payload: []byte(`{"budget":2000,"max_results":3,"min_reputation":0,"task":"` + task + `"}`),
	}
}

// hitMatch builds a HIT exchange:match fixture: tag exchange:match only, a
// non-empty results array, antecedent = buy order ID. Shape mirrors the live
// payload emitted by Engine.handleBuy.
func hitMatch(id, buyID string) Message {
	return Message{
		ID:          id,
		Tags:        []string{TagMatch},
		Antecedents: []string{buyID},
		Payload: []byte(`{"results":[` +
			`{"entry_id":"e1","put_msg_id":"p1","seller_key":"s1","description":"d",` +
			`"content_hash":"sha256:ab","content_type":"code","price":120,"confidence":0.91,` +
			`"is_partial_match":false,"seller_reputation":80,"token_cost_original":1000,"age_hours":3}` +
			`],"search_meta":{"total_candidates":5},"guide":"Results are ranked by ..."}`),
	}
}

// missMatch builds a MISS exchange:match fixture: tags
// [exchange:buy-miss, exchange:match], top-level buy_msg_id + task_hash and the
// "No cached inference matched" guide, antecedent = buy order ID. Shape mirrors
// Engine.handleBuyMiss.
func missMatch(id, buyID string) Message {
	return Message{
		ID:          id,
		Tags:        []string{TagBuyMiss, TagMatch},
		Antecedents: []string{buyID},
		Payload: []byte(`{"buy_msg_id":"` + buyID + `","expires_at":"2026-06-02T20:13:29Z",` +
			`"guide":"No cached inference matched your task. A standing offer has been created ...",` +
			`"offered_price_rate":70,"task":"zzqq nonsense","task_hash":"b7699bc655dac384"}`),
	}
}

func TestComputeHitRate_HitMissPending(t *testing.T) {
	buys := []Message{
		buyMsg("buy-1", "explain campfire convention dispatch"),
		buyMsg("buy-2", "zzqq nonsense xyzzy no such cached inference"),
		buyMsg("buy-3", "pending order, no match yet"),
	}
	matches := []Message{
		hitMatch("m-1", "buy-1"),
		missMatch("m-2", "buy-2"),
		// buy-3 has no match -> pending.
	}

	rep := ComputeHitRate(buys, matches)

	if rep.TotalBuys != 3 {
		t.Errorf("TotalBuys = %d, want 3", rep.TotalBuys)
	}
	if rep.MatchedBuys != 2 {
		t.Errorf("MatchedBuys = %d, want 2", rep.MatchedBuys)
	}
	if rep.PendingBuys != 1 {
		t.Errorf("PendingBuys = %d, want 1", rep.PendingBuys)
	}
	if rep.Hits != 1 {
		t.Errorf("Hits = %d, want 1", rep.Hits)
	}
	if rep.Misses != 1 {
		t.Errorf("Misses = %d, want 1", rep.Misses)
	}
	if rep.HitRatePct != 50.0 {
		t.Errorf("HitRatePct = %v, want 50.0", rep.HitRatePct)
	}
	if rep.MatchResultsTotal != 2 {
		t.Errorf("MatchResultsTotal = %d, want 2", rep.MatchResultsTotal)
	}
}

// A match-result whose antecedent buy is not in the buy set is unjoinable and
// must not move hit/miss totals.
func TestComputeHitRate_Unjoinable(t *testing.T) {
	buys := []Message{
		buyMsg("buy-1", "task a"),
	}
	matches := []Message{
		hitMatch("m-1", "buy-1"),
		hitMatch("m-orphan", "buy-999"), // buy-999 not in buys
		missMatch("m-orphan2", "buy-998"),
	}

	rep := ComputeHitRate(buys, matches)

	if rep.TotalBuys != 1 {
		t.Errorf("TotalBuys = %d, want 1", rep.TotalBuys)
	}
	if rep.MatchedBuys != 1 {
		t.Errorf("MatchedBuys = %d, want 1", rep.MatchedBuys)
	}
	if rep.Hits != 1 || rep.Misses != 0 {
		t.Errorf("Hits/Misses = %d/%d, want 1/0", rep.Hits, rep.Misses)
	}
	if rep.UnjoinableMatchResults != 2 {
		t.Errorf("UnjoinableMatchResults = %d, want 2", rep.UnjoinableMatchResults)
	}
	if rep.HitRatePct != 100.0 {
		t.Errorf("HitRatePct = %v, want 100.0", rep.HitRatePct)
	}
}

// A buy that receives both a miss and a later hit (e.g. multiple match emissions)
// must be classified as a hit — best outcome wins, regardless of order.
func TestComputeHitRate_BestOutcomeWins(t *testing.T) {
	buys := []Message{buyMsg("buy-1", "task")}

	// Miss first, then hit.
	rep := ComputeHitRate(buys, []Message{
		missMatch("m-1", "buy-1"),
		hitMatch("m-2", "buy-1"),
	})
	if rep.Hits != 1 || rep.Misses != 0 || rep.MatchedBuys != 1 {
		t.Errorf("miss-then-hit: Hits/Misses/Matched = %d/%d/%d, want 1/0/1", rep.Hits, rep.Misses, rep.MatchedBuys)
	}

	// Hit first, then miss — order must not regress the hit.
	rep = ComputeHitRate(buys, []Message{
		hitMatch("m-1", "buy-1"),
		missMatch("m-2", "buy-1"),
	})
	if rep.Hits != 1 || rep.Misses != 0 {
		t.Errorf("hit-then-miss: Hits/Misses = %d/%d, want 1/0", rep.Hits, rep.Misses)
	}
}

func TestComputeHitRate_Empty(t *testing.T) {
	rep := ComputeHitRate(nil, nil)
	if rep.TotalBuys != 0 || rep.MatchedBuys != 0 || rep.HitRatePct != 0 {
		t.Errorf("empty: got %+v, want zero report", rep)
	}
}

// classifyMatchResult must use the buy-miss tag as the authoritative signal and
// fall back to payload shape.
func TestClassifyMatchResult(t *testing.T) {
	hit := hitMatch("m-1", "buy-1")
	if !classifyMatchResult(&hit) {
		t.Error("hit-shaped match classified as miss")
	}
	miss := missMatch("m-2", "buy-2")
	if classifyMatchResult(&miss) {
		t.Error("miss-shaped match classified as hit")
	}

	// Defensive: a miss-shaped payload that lost its tag is still a miss
	// (top-level buy_msg_id present, no results).
	noTagMiss := Message{
		ID:          "m-3",
		Tags:        []string{TagMatch},
		Antecedents: []string{"buy-3"},
		Payload:     []byte(`{"buy_msg_id":"buy-3","task_hash":"abc","offered_price_rate":70}`),
	}
	if classifyMatchResult(&noTagMiss) {
		t.Error("untagged miss-shaped payload classified as hit")
	}
}

// buyMsgIDFor prefers the antecedent and falls back to payload buy_msg_id.
func TestBuyMsgIDFor(t *testing.T) {
	withAnt := hitMatch("m-1", "buy-1")
	if got := buyMsgIDFor(&withAnt); got != "buy-1" {
		t.Errorf("antecedent join: got %q, want buy-1", got)
	}

	// No antecedent, but buy_msg_id in payload (miss path).
	noAnt := Message{
		ID:      "m-2",
		Tags:    []string{TagBuyMiss, TagMatch},
		Payload: []byte(`{"buy_msg_id":"buy-2","task_hash":"abc"}`),
	}
	if got := buyMsgIDFor(&noAnt); got != "buy-2" {
		t.Errorf("payload fallback join: got %q, want buy-2", got)
	}

	// Neither — unjoinable.
	bare := Message{ID: "m-3", Tags: []string{TagMatch}, Payload: []byte(`{}`)}
	if got := buyMsgIDFor(&bare); got != "" {
		t.Errorf("unjoinable: got %q, want empty", got)
	}
}
