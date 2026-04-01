package exchange_test

// Tests for applySettleComplete idempotency (dontguess-p37).
//
// When the campfire message log is replayed (e.g., on restart), applySettleComplete
// must not increment SuccessCount more than once for the same settle(complete) message.
// Without the idempotency guard, each replay inflates reputation scores.

import (
	"testing"

	"github.com/campfire-net/dontguess/pkg/exchange"
)

// TestState_SettleComplete_ReplayIdempotent verifies that replaying all messages
// multiple times does not cause SuccessCount to grow with each replay.
// This is the primary regression test for the double-increment bug.
func TestState_SettleComplete_ReplayIdempotent(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	matchMsg, entryID := setupMatchedOrder(t, h, eng)

	// Buyer accepts the match.
	buyerAcceptMsg := h.sendMessage(h.buyer, buyerAcceptPayloadFor(entryID),
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrBuyerAccept,
			exchange.TagVerdictPrefix + "accepted",
		},
		[]string{matchMsg.ID},
	)

	// Operator delivers.
	deliverMsg := h.sendMessage(h.operator, deliverPayloadFor(entryID),
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrDeliver,
		},
		[]string{buyerAcceptMsg.ID},
	)

	// Buyer completes.
	h.sendMessage(h.buyer, completePayloadFor(entryID, 12000),
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrComplete,
			exchange.TagVerdictPrefix + "accepted",
		},
		[]string{deliverMsg.ID},
	)

	// Replay once — record baseline reputation.
	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))
	repAfterFirstReplay := eng.State().SellerReputation(h.seller.PublicKeyHex())

	if repAfterFirstReplay <= exchange.DefaultReputation {
		t.Fatalf("reputation after first replay = %d, want > %d (SuccessCount should be 1)",
			repAfterFirstReplay, exchange.DefaultReputation)
	}

	// Replay again — reputation must not change.
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))
	repAfterSecondReplay := eng.State().SellerReputation(h.seller.PublicKeyHex())

	if repAfterSecondReplay != repAfterFirstReplay {
		t.Errorf("reputation changed across replay: first=%d second=%d — double-increment bug",
			repAfterFirstReplay, repAfterSecondReplay)
	}

	// Replay a third time for extra confidence.
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))
	repAfterThirdReplay := eng.State().SellerReputation(h.seller.PublicKeyHex())

	if repAfterThirdReplay != repAfterFirstReplay {
		t.Errorf("reputation changed across third replay: first=%d third=%d — double-increment bug",
			repAfterFirstReplay, repAfterThirdReplay)
	}
}

// TestState_SettleComplete_SuccessCountExactlyOne verifies that SuccessCount is
// exactly 1 after a single completed transaction, even after multiple replays.
func TestState_SettleComplete_SuccessCountExactlyOne(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	matchMsg, entryID := setupMatchedOrder(t, h, eng)

	buyerAcceptMsg := h.sendMessage(h.buyer, buyerAcceptPayloadFor(entryID),
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrBuyerAccept,
			exchange.TagVerdictPrefix + "accepted",
		},
		[]string{matchMsg.ID},
	)

	deliverMsg := h.sendMessage(h.operator, deliverPayloadFor(entryID),
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrDeliver,
		},
		[]string{buyerAcceptMsg.ID},
	)

	h.sendMessage(h.buyer, completePayloadFor(entryID, 12000),
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrComplete,
			exchange.TagVerdictPrefix + "accepted",
		},
		[]string{deliverMsg.ID},
	)

	allMsgs, _ := h.st.ListMessages(h.cfID, 0)

	// Replay 5 times — SuccessCount must remain 1 throughout.
	for i := 1; i <= 5; i++ {
		eng.State().Replay(exchange.FromStoreRecords(allMsgs))
		// Reputation after 1 success = DefaultReputation + 1 = 51 (no repeat buyer bonus).
		got := eng.State().SellerReputation(h.seller.PublicKeyHex())
		want := exchange.DefaultReputation + 1 // +1 per SuccessCount
		if got != want {
			t.Errorf("after replay %d: reputation = %d, want %d (expected exactly 1 success)",
				i, got, want)
		}
	}
}

// TestState_SettleComplete_PriceHistoryNotDuplicated verifies that replaying the
// message log does not append duplicate price history records.
func TestState_SettleComplete_PriceHistoryNotDuplicated(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	matchMsg, entryID := setupMatchedOrder(t, h, eng)

	buyerAcceptMsg := h.sendMessage(h.buyer, buyerAcceptPayloadFor(entryID),
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrBuyerAccept,
			exchange.TagVerdictPrefix + "accepted",
		},
		[]string{matchMsg.ID},
	)

	deliverMsg := h.sendMessage(h.operator, deliverPayloadFor(entryID),
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrDeliver,
		},
		[]string{buyerAcceptMsg.ID},
	)

	h.sendMessage(h.buyer, completePayloadFor(entryID, 12000),
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrComplete,
			exchange.TagVerdictPrefix + "accepted",
		},
		[]string{deliverMsg.ID},
	)

	allMsgs, _ := h.st.ListMessages(h.cfID, 0)

	// First replay — expect exactly 1 price record.
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))
	priceHistory := eng.State().PriceHistory()
	if len(priceHistory) != 1 {
		t.Fatalf("price history after first replay = %d records, want 1", len(priceHistory))
	}

	// Second replay — still exactly 1 record.
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))
	priceHistory = eng.State().PriceHistory()
	if len(priceHistory) != 1 {
		t.Errorf("price history after second replay = %d records, want 1 (duplicate appended)", len(priceHistory))
	}
}
