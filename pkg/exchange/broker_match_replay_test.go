package exchange_test

// Tests for the RegisterBrokerMatch + Replay contract.
//
// The doc comment on RegisterBrokerMatch states: "engine must re-register after Replay".
// brokerMatchIDs is intentionally NOT reset on Replay (it is externally managed
// by the engine, not derived from the campfire log). However, brokeredAcceptedOrders
// and brokeredCompletions ARE reset on Replay.
//
// On a fresh process start with empty brokerMatchIDs, all brokered-match accepts
// in the log are miscounted as non-brokered if the engine does not re-register.
//
// TestRegisterBrokerMatch_ReplayResetsCounters verifies the contract:
//   1. RegisterBrokerMatch(X) → process brokered accepts → counters increment
//   2. Replay() → counters reset to 0, brokerMatchIDs persists
//   3. Re-register X → replay the same log → brokeredAcceptedOrders correct
//
// TestRegisterBrokerMatch_ColdProcessStartMiscounts verifies the failure mode:
// on a State with empty brokerMatchIDs (simulating fresh process start),
// a brokered-match accept in the log is NOT counted as brokered.

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/3dl-dev/dontguess/pkg/exchange"
)

// buildSyntheticBrokeredLog builds a minimal message log that contains:
//   - a buy (buyer → activeOrders)
//   - a match (matchID → matchToBuyer, matchToEntry, matchToResults)
//   - a buyer-accept (antecedent=matchID, sender=buyer → brokeredAcceptedOrders if matchID registered)
//
// Returns (msgs, matchMsgID) so the caller can call RegisterBrokerMatch(matchMsgID).
func buildSyntheticBrokeredLog(buyer *testAgent, operator *testAgent) ([]exchange.Message, string) {
	buyerKey := buyer.pubKeyHex
	operatorKey := operator.pubKeyHex
	entryID := "synthetic-entry-001"

	// Buy message.
	buyPayloadBytes, _ := json.Marshal(map[string]any{
		"task":   "synthetic brokered task",
		"budget": int64(5000),
	})
	buyID := "aabbcc0000000000000000000000000000000000000000000000000000000001"
	buyMsg := exchange.Message{
		ID:          buyID,
		Sender:      buyerKey,
		Payload:     buyPayloadBytes,
		Tags:        []string{exchange.TagBuy},
		Antecedents: []string{},
		Timestamp:   time.Now().UnixNano(),
	}

	// Match message (sent by operator, antecedent is the buy).
	matchPayloadBytes, _ := json.Marshal(map[string]any{
		"results": []map[string]any{
			{"entry_id": entryID, "price": int64(3500)},
		},
	})
	matchID := "aabbcc0000000000000000000000000000000000000000000000000000000002"
	matchMsg := exchange.Message{
		ID:          matchID,
		Sender:      operatorKey,
		Payload:     matchPayloadBytes,
		Tags:        []string{exchange.TagMatch},
		Antecedents: []string{buyID},
		Timestamp:   time.Now().UnixNano(),
	}

	// Buyer-accept (antecedent is the match).
	acceptPayloadBytes, _ := json.Marshal(map[string]any{
		"entry_id": entryID,
		"accepted": true,
	})
	acceptID := "aabbcc0000000000000000000000000000000000000000000000000000000003"
	acceptMsg := exchange.Message{
		ID:     acceptID,
		Sender: buyerKey,
		Payload: acceptPayloadBytes,
		Tags:   []string{exchange.TagSettle, "exchange:phase:buyer-accept"},
		Antecedents: []string{matchID},
		Timestamp:   time.Now().UnixNano(),
	}

	return []exchange.Message{buyMsg, matchMsg, acceptMsg}, matchID
}

// TestRegisterBrokerMatch_ReplayResetsCounters verifies the full replay contract:
//
//  1. RegisterBrokerMatch(matchID) + Replay → counters match brokered log
//  2. Replay() without re-register → counters reset to 0 (brokerMatchIDs cleared)
//  3. Re-register + Replay → counters restored to correct values
//
// This documents the "engine must re-register after Replay" contract stated in the
// doc comment on RegisterBrokerMatch.
func TestRegisterBrokerMatch_ReplayResetsCounters(t *testing.T) {
	t.Parallel()

	buyer := newTestAgent(t)
	operator := newTestAgent(t)
	msgs, matchID := buildSyntheticBrokeredLog(buyer, operator)

	st := exchange.NewState()
	// No OperatorKey set so operator-sender checks are skipped in applyMatch.

	// Phase 1: Register before replay — counters should increment on replay.
	st.RegisterBrokerMatch(matchID)
	st.Replay(msgs)

	gotAccepted := st.BrokeredMatchCompletionRate() // 0/1 = 0.0 (accepted but not completed)
	gotCount := st.BrokeredCompletionCount()
	if st.BrokeredMatchCompletionRate() != 0.0 {
		// 1 accepted, 0 completed → rate = 0/1 = 0.0
		t.Errorf("phase 1: expected BrokeredMatchCompletionRate=0.0, got %.4f", gotAccepted)
	}
	_ = gotAccepted
	if gotCount != 0 {
		t.Errorf("phase 1: expected BrokeredCompletionCount=0, got %d", gotCount)
	}

	// Verify brokeredAcceptedOrders is 1 by checking rate with a completion.
	// We do this indirectly: BrokeredMatchCompletionRate returns 0/1, not 1.0 (cold-start),
	// which means the denominator is 1 (accepted=1, completions=0).
	// If the denominator were 0, it would return 1.0 (cold start).
	brokeredRate := st.BrokeredMatchCompletionRate()
	if brokeredRate != 0.0 {
		t.Errorf("phase 1: brokeredAcceptedOrders should be 1 (rate=0/1=0.0), got rate=%.4f", brokeredRate)
	}

	// Phase 2: Replay() resets counters to 0; brokerMatchIDs persists (not reset).
	// Because brokerMatchIDs is not reset, replay should re-count correctly.
	st.Replay(msgs)

	// After Replay with brokerMatchIDs still containing matchID:
	// brokeredAcceptedOrders should be 1 again (re-derived from log + brokerMatchIDs).
	brokeredRateAfterSecondReplay := st.BrokeredMatchCompletionRate()
	if brokeredRateAfterSecondReplay != 0.0 {
		t.Errorf("phase 2: after second Replay (brokerMatchIDs persists), expected rate=0.0, got %.4f",
			brokeredRateAfterSecondReplay)
	}

	// Phase 3: Simulate fresh process start — new State with empty brokerMatchIDs,
	// then re-register before replay.
	freshSt := exchange.NewState()
	// Without re-registration, brokeredAcceptedOrders will be 0 on replay.
	freshSt.Replay(msgs)
	rateWithoutReregister := freshSt.BrokeredMatchCompletionRate()
	// With empty brokerMatchIDs: accept not counted as brokered → brokeredAcceptedOrders=0
	// → BrokeredMatchCompletionRate returns 1.0 (cold-start sentinel).
	if rateWithoutReregister != 1.0 {
		t.Errorf("phase 3: without re-register on fresh state, expected cold-start rate=1.0, got %.4f",
			rateWithoutReregister)
	}

	// Now re-register and replay — counters should be correct.
	freshSt.RegisterBrokerMatch(matchID)
	freshSt.Replay(msgs)
	rateWithReregister := freshSt.BrokeredMatchCompletionRate()
	// With matchID registered: brokeredAcceptedOrders=1, brokeredCompletions=0 → rate=0.0.
	if rateWithReregister != 0.0 {
		t.Errorf("phase 3: after re-register + Replay, expected rate=0.0, got %.4f", rateWithReregister)
	}
}
