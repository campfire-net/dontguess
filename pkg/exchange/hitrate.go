package exchange

import (
	"encoding/json"
)

// HitRateReport is the result of reconciling buy orders against match-results.
//
// Buy matching is asynchronous: a `dontguess buy` returns "dispatched" before
// any match exists, so hit-vs-miss is NOT knowable at the wrapper. It IS
// recoverable from the exchange message log by joining match-result messages
// back to their originating buy order (via the match's first antecedent, which
// the engine always sets to the buy message ID — see engine.go handleBuy /
// handleBuyMiss, both call sendOperatorMessage with antecedents=[]string{msg.ID}).
type HitRateReport struct {
	// TotalBuys is the number of distinct buy orders seen in the window.
	TotalBuys int `json:"total_buys"`

	// MatchedBuys is the number of distinct buy orders that received at least
	// one match-result (hit or miss).
	MatchedBuys int `json:"matched_buys"`

	// PendingBuys is buys with no corresponding match-result yet (still
	// dispatched / in flight). TotalBuys = MatchedBuys + PendingBuys.
	PendingBuys int `json:"pending_buys"`

	// Hits is the number of distinct buy orders whose best outcome was a HIT:
	// at least one match-result delivered cached inventory (results array).
	Hits int `json:"hits"`

	// Misses is the number of distinct buy orders whose only outcome was a
	// buy-miss standing offer ("No cached inference matched").
	Misses int `json:"misses"`

	// HitRatePct is Hits / MatchedBuys * 100, rounded to 2 decimals. It is the
	// share of *answered* buys that found cached inference. Pending buys are
	// excluded from the denominator (their outcome is unknown). When MatchedBuys
	// is zero, HitRatePct is 0.
	HitRatePct float64 `json:"hit_rate_pct"`

	// MatchResultsTotal is the raw number of match-result messages in the
	// window (a single buy can receive more than one). Reported for
	// reconciliation against `dontguess match-results --json | jq length`.
	MatchResultsTotal int `json:"match_results_total"`

	// UnjoinableMatchResults is match-results whose antecedent buy order was not
	// present in the buy set in the window (e.g. the buys view was paged
	// differently, or the buy fell outside --since while its match did not).
	// These are counted but cannot be attributed to a buy, so they do not move
	// hit/miss totals.
	UnjoinableMatchResults int `json:"unjoinable_match_results"`
}

// classifyMatchResult reports whether a single exchange:match message is a HIT
// or a MISS. The discriminator is grounded in the code that WRITES these
// messages (pkg/exchange/engine.go):
//
//   - MISS (handleBuyMiss): tags = [exchange:buy-miss, exchange:match]; payload
//     carries top-level "buy_msg_id" + "task_hash" and the "No cached inference
//     matched" guide; it has NO "results" array.
//   - HIT (handleBuy match path): tags = [exchange:match]; payload carries a
//     non-empty "results" array of matched inventory entries; no top-level
//     "buy_msg_id".
//
// We classify primarily by the exchange:buy-miss tag (the authoritative signal
// the engine sets), and fall back to payload shape (presence of a non-empty
// results array) so the classifier is robust if tag composition ever changes.
func classifyMatchResult(m *Message) (isHit bool) {
	for _, t := range m.Tags {
		if t == TagBuyMiss {
			return false
		}
	}
	// No buy-miss tag: confirm it carries delivered results.
	var p struct {
		Results  []json.RawMessage `json:"results"`
		BuyMsgID string            `json:"buy_msg_id"`
	}
	if err := json.Unmarshal(m.Payload, &p); err != nil {
		// Unparseable payload but no buy-miss tag — treat as a hit-shaped
		// match (it was emitted on the match path, not the miss path).
		return true
	}
	if p.BuyMsgID != "" {
		// Top-level buy_msg_id is the buy-miss payload shape (defensive: a
		// miss that somehow lost its tag).
		return false
	}
	return len(p.Results) > 0
}

// buyMsgIDFor returns the buy order ID a match-result belongs to. The engine
// always sets the buy message ID as the first antecedent on both the hit and
// miss paths. The miss payload also carries it as "buy_msg_id"; we fall back to
// that if antecedents are missing.
func buyMsgIDFor(m *Message) string {
	if len(m.Antecedents) > 0 && m.Antecedents[0] != "" {
		return m.Antecedents[0]
	}
	var p struct {
		BuyMsgID string `json:"buy_msg_id"`
	}
	if err := json.Unmarshal(m.Payload, &p); err == nil {
		return p.BuyMsgID
	}
	return ""
}

// ComputeHitRate reconciles a set of buy orders against a set of match-result
// messages and returns a HitRateReport.
//
// buys are exchange:buy messages (their .ID is the buy order ID).
// matches are exchange:match messages (hits and misses both carry this tag).
//
// A buy is classified by the BEST outcome across all its match-results: if any
// match-result for that buy is a hit, the buy is a hit; otherwise (only
// buy-miss results) it is a miss. Buys with no match-result are pending and
// excluded from the hit-rate denominator.
//
// Callers are responsible for windowing (passing only messages within --since).
func ComputeHitRate(buys, matches []Message) HitRateReport {
	// Set of known buy order IDs.
	buyIDs := make(map[string]struct{}, len(buys))
	for i := range buys {
		buyIDs[buys[i].ID] = struct{}{}
	}

	// Best outcome per buy: true once any hit is seen for it.
	hitByBuy := make(map[string]bool)
	var unjoinable int

	for i := range matches {
		m := &matches[i]
		buyID := buyMsgIDFor(m)
		if buyID == "" {
			unjoinable++
			continue
		}
		if _, ok := buyIDs[buyID]; !ok {
			// Match-result references a buy we don't have in this window.
			unjoinable++
			continue
		}
		isHit := classifyMatchResult(m)
		if isHit {
			hitByBuy[buyID] = true
		} else if _, seen := hitByBuy[buyID]; !seen {
			// Record the buy as matched-but-miss only if no hit recorded yet.
			hitByBuy[buyID] = false
		}
	}

	hits := 0
	for _, isHit := range hitByBuy {
		if isHit {
			hits++
		}
	}
	matchedBuys := len(hitByBuy)
	misses := matchedBuys - hits

	rep := HitRateReport{
		TotalBuys:              len(buyIDs),
		MatchedBuys:            matchedBuys,
		PendingBuys:            len(buyIDs) - matchedBuys,
		Hits:                   hits,
		Misses:                 misses,
		MatchResultsTotal:      len(matches),
		UnjoinableMatchResults: unjoinable,
	}
	if matchedBuys > 0 {
		rep.HitRatePct = round2(float64(hits) / float64(matchedBuys) * 100)
	}
	return rep
}

// round2 rounds to two decimal places.
func round2(f float64) float64 {
	return float64(int64(f*100+0.5)) / 100
}
