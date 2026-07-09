package exchange_test

// engine_deadline_deterministic_test.go — proves the guarantee deadline-miss
// verdict (checkDeadlineMiss) is BOTH replay-deterministic AND operator-trusted
// (dontguess-9f1, relay-transport.md §4 ADV-10 + §Sequencer).
//
// The verdict is derived from the OPERATOR-AUTHORED settle(deliver) Timestamp —
// "did the exchange deliver after the guarantee deadline?" — NEVER from
// wall-clock time.Now() (which would make refund-vs-settle depend on when the
// log is replayed) and NEVER from the BUYER-authored settle(complete) Timestamp
// (which a buyer could set to any value to force or dodge the full refund).
//
// Two properties are proved:
//   - DETERMINISM: with the deadline already in the past in wall-clock terms but
//     the operator's deliver landing BEFORE the deadline, NO refund fires. Old
//     code (time.Now().After(deadline)) would ALWAYS refund here because real
//     now > deadline; the new code follows the persisted deliver Timestamp, so
//     the outcome is independent of when dispatch runs.
//   - TRUST: the buyer's self-set settle(complete).Timestamp cannot move the
//     verdict in either direction (far-future cannot force a refund; far-past
//     cannot dodge one).

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/campfire-net/dontguess/pkg/store"

	"github.com/campfire-net/dontguess/pkg/exchange"
	"github.com/campfire-net/dontguess/pkg/scrip"
)

// insuredMatchFixture runs put → put-accept → buy(insured) → match →
// buyer-accept and returns the resolved match ID, entry ID, buyer-accept
// message, and the guarantee deadline. The engine is left stopped and ready for
// dispatch-mode settle steps (deliver + complete) driven by the caller.
type insuredMatchFixture struct {
	h              *testHarness
	eng            *exchange.Engine
	matchMsgID     string
	entryID        string
	buyerAcceptMsg *exchange.Message
	deadline       time.Time
	insuredAmount  int64
}

func newInsuredMatchFixture(t *testing.T, guaranteeSeconds int) *insuredMatchFixture {
	t.Helper()
	h := newTestHarness(t)
	cs := newCampfireScripStore(t, h)
	eng := exchange.NewEngine(exchange.EngineOptions{
		CampfireID:        h.cfID,
		LocalStore:        h.st,
		OperatorPublicKey: h.operator.pubKeyHex,
		ScripStore:        cs,
		Logger:            func(format string, args ...any) { t.Logf("[engine] "+format, args...) },
	})

	seedInventoryEntry(t, h, eng, "deadline determinism entry", "code", 8000, 5600)
	inv := eng.State().Inventory()
	if len(inv) != 1 {
		t.Fatalf("expected 1 inventory entry, got %d", len(inv))
	}
	entryID := inv[0].EntryID
	salePrice := eng.ComputePriceForTest(inv[0])
	fee := salePrice / exchange.MatchingFeeRate
	holdAmount := salePrice + fee

	const insuredAmount int64 = 5000
	addScripMintMsg(t, h, h.buyer.PublicKeyHex(), holdAmount+10000)
	if err := cs.Replay(); err != nil {
		t.Fatalf("cs.Replay: %v", err)
	}

	preMatch, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { _ = eng.Start(ctx) }()

	buyPayloadBytes, _ := json.Marshal(map[string]any{
		"task":                       "deadline determinism test",
		"budget":                     salePrice + 10000,
		"max_results":                1,
		"guarantee_deadline_seconds": guaranteeSeconds,
		"insured_amount":             insuredAmount,
	})
	h.sendMessage(h.buyer, buyPayloadBytes, []string{exchange.TagBuy}, nil)

	matchMsg := waitForMatchMessage(t, h, preMatch, 3*time.Second)
	cancel()

	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))

	deadline, _, hasGuarantee := eng.State().GuaranteeForMatch(matchMsg.ID)
	if !hasGuarantee {
		t.Fatal("GuaranteeForMatch: expected guarantee for this match, got not-found")
	}

	buyerAcceptMsg := sendBuyerAcceptAndDispatch(t, h, eng, matchMsg.ID, entryID)

	return &insuredMatchFixture{
		h:              h,
		eng:            eng,
		matchMsgID:     matchMsg.ID,
		entryID:        entryID,
		buyerAcceptMsg: buyerAcceptMsg,
		deadline:       deadline,
		insuredAmount:  insuredAmount,
	}
}

// sendDeliver emits an operator-authored settle(deliver) and replays state so
// the deliver Timestamp is recorded (deliverTimeByMatch). Returns the deliver
// message (its Timestamp is the operator-trusted reference the verdict uses).
func (f *insuredMatchFixture) sendDeliver(t *testing.T) *exchange.Message {
	t.Helper()
	deliverP, _ := json.Marshal(map[string]any{
		"phase":        "deliver",
		"entry_id":     f.entryID,
		"content_ref":  fmt.Sprintf("sha256:%064x", 7),
		"content_size": int64(20000),
	})
	deliverMsg := f.h.sendMessage(f.h.operator, deliverP,
		[]string{exchange.TagSettle, exchange.TagPhasePrefix + exchange.SettlePhaseStrDeliver},
		[]string{f.buyerAcceptMsg.ID},
	)
	allMsgs, _ := f.h.st.ListMessages(f.h.cfID, 0)
	f.eng.State().Replay(exchange.FromStoreRecords(allMsgs))
	return deliverMsg
}

// dispatchCompleteWithTimestamp builds a BUYER-authored settle(complete) with a
// caller-chosen Timestamp and dispatches it. The custom Timestamp is the
// counterparty-controlled value the verdict must ignore.
func (f *insuredMatchFixture) dispatchCompleteWithTimestamp(t *testing.T, deliverMsgID string, ts int64) {
	t.Helper()
	completeP, _ := json.Marshal(map[string]any{"entry_id": f.entryID})
	completeMsg := &exchange.Message{
		ID:          fmt.Sprintf("complete-%064x", ts&0xffffffff),
		CampfireID:  f.h.cfID,
		Sender:      f.h.buyer.PublicKeyHex(),
		Payload:     completeP,
		Tags:        []string{exchange.TagSettle, exchange.TagPhasePrefix + exchange.SettlePhaseStrComplete},
		Antecedents: []string{deliverMsgID},
		Timestamp:   ts,
	}
	if err := f.eng.DispatchForTest(completeMsg); err != nil {
		t.Fatalf("DispatchForTest settle(complete): %v", err)
	}
}

// TestDeadlineMiss_VerdictIndependentOfWallClockAndBuyerTimestamp proves the
// DETERMINISM + TRUST property for the on-time-delivery case: the operator
// delivered BEFORE the deadline, so NO refund fires — even though (a) real
// wall-clock time is already past the deadline when the complete is dispatched
// (old time.Now() code would wrongly refund), and (b) the buyer sets its
// settle(complete).Timestamp far in the FUTURE to try to force a deadline miss.
func TestDeadlineMiss_VerdictIndependentOfWallClockAndBuyerTimestamp(t *testing.T) {
	t.Parallel()

	// 2-second guarantee window: ample margin to deliver on-time, then let the
	// deadline lapse in wall-clock terms before dispatching the complete.
	f := newInsuredMatchFixture(t, 2)

	// Operator delivers ON-TIME (immediately, well before the 2s deadline).
	deliverMsg := f.sendDeliver(t)
	if !time.Unix(0, deliverMsg.Timestamp).UTC().Before(f.deadline) {
		t.Fatalf("test setup: deliver Timestamp %v must be before deadline %v",
			time.Unix(0, deliverMsg.Timestamp).UTC(), f.deadline)
	}

	// Let the deadline pass in wall-clock terms. Old code compared time.Now()
	// against the deadline here and would refund; the fix compares the on-time
	// deliver Timestamp, so it must NOT refund.
	if d := time.Until(f.deadline); d > 0 {
		time.Sleep(d + 300*time.Millisecond)
	}
	if time.Now().Before(f.deadline) {
		t.Fatalf("test setup: expected wall-clock now to be past the deadline")
	}

	preSettle := countMsgsWithTag(t, f.h, scrip.TagScripSettle)
	preRefund := countMsgsWithTag(t, f.h, scrip.TagScripDisputeRefund)

	// Buyer tries to force a refund by claiming completion far in the FUTURE.
	farFuture := time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano()
	f.dispatchCompleteWithTimestamp(t, deliverMsg.ID, farFuture)

	afterSettle := countMsgsWithTag(t, f.h, scrip.TagScripSettle)
	afterRefund := countMsgsWithTag(t, f.h, scrip.TagScripDisputeRefund)

	if afterRefund > preRefund {
		t.Errorf("on-time delivery MUST NOT trigger a deadline-miss refund; refund count went %d -> %d "+
			"(verdict wrongly followed wall-clock or buyer Timestamp)", preRefund, afterRefund)
	}
	if afterSettle <= preSettle {
		t.Errorf("expected normal scrip-settle on on-time delivery; settle count %d -> %d", preSettle, afterSettle)
	}
}

// TestDeadlineMiss_LateDeliveryRefundsDespiteBuyerBackdating proves the TRUST
// property in the other direction: the operator delivered AFTER the deadline, so
// the guarantee is missed and a refund MUST fire — even though the buyer sets
// its settle(complete).Timestamp far in the PAST (before the deadline) to try to
// dodge the refund. The verdict follows the operator-authored deliver Timestamp,
// not the buyer's.
func TestDeadlineMiss_LateDeliveryRefundsDespiteBuyerBackdating(t *testing.T) {
	t.Parallel()

	// 1-second guarantee window.
	f := newInsuredMatchFixture(t, 1)

	// Wait until the deadline has lapsed, THEN deliver — deliver Timestamp is
	// after the deadline (a genuine guarantee miss).
	if d := time.Until(f.deadline); d > 0 {
		time.Sleep(d + 300*time.Millisecond)
	}
	deliverMsg := f.sendDeliver(t)
	if !time.Unix(0, deliverMsg.Timestamp).UTC().After(f.deadline) {
		t.Fatalf("test setup: deliver Timestamp %v must be after deadline %v",
			time.Unix(0, deliverMsg.Timestamp).UTC(), f.deadline)
	}

	preSettle := countMsgsWithTag(t, f.h, scrip.TagScripSettle)
	preRefund := countMsgsWithTag(t, f.h, scrip.TagScripDisputeRefund)

	// Buyer tries to dodge the refund by backdating completion to before the
	// deadline.
	farPast := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano()
	f.dispatchCompleteWithTimestamp(t, deliverMsg.ID, farPast)

	afterSettle := countMsgsWithTag(t, f.h, scrip.TagScripSettle)
	afterRefund := countMsgsWithTag(t, f.h, scrip.TagScripDisputeRefund)

	if afterRefund <= preRefund {
		t.Errorf("late delivery MUST trigger a deadline-miss refund regardless of the buyer's backdated "+
			"Timestamp; refund count stayed %d -> %d", preRefund, afterRefund)
	}
	if afterSettle > preSettle {
		t.Errorf("expected NO normal scrip-settle on the refund path; settle count %d -> %d", preSettle, afterSettle)
	}
}
