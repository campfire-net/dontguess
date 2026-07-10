package exchange_test

// ed2-D (design docs/design/nostr-first-client-ed2.md §3.6 + §3.7) — Layer-0
// money-integrity proof that the FREE-CONTENT exploit is CLOSED.
//
// THE HOLE (verified, pre-fix): buyerAcceptToMatch[msg.ID] is folded
// UNCONDITIONALLY (state_settle.go applySettleBuyerAccept), the scrip HOLD is a
// separate dispatch handler that can fail (insufficient scrip) and be
// logged+dropped, and emitDeliverContent gated ONLY on operator authorship +
// the antecedent chain — never on a live reservation. Net: an underfunded buyer
// publishes buyer-accept (hold fails) then settle(deliver) → the operator emits
// the FULL CONTENT FREE, and at settle(complete) reservationFor(match) is empty
// so no scrip ever moves. Content moved without payment.
//
// THE FIXES (both additive, both guarded by ScripStore != nil):
//   §3.7 handleSettleDeliverContent REQUIRES a live reservationFor(match) before
//        emitDeliverContent — no reservation ⇒ no content.
//   §3.6 a failed decAndSaveHold (ErrBudgetExceeded) emits a DURABLE, wire-visible
//        settle(buyer-accept-reject) (reason:"insufficient_scrip") before
//        returning — the buyer learns why instead of only timing out.
//
// This test drives the exact attack and asserts: the reject is on the wire, NO
// settle(deliver) carrying content is ever emitted, no reservation/hold/settle
// exists, the buyer's balance is untouched, and total scrip supply is conserved.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/campfire-net/dontguess/pkg/exchange"
	"github.com/campfire-net/dontguess/pkg/store"
)

// deliverMessagesWithContent counts settle(deliver) messages on the log that
// actually carry content (or a blob pointer) — i.e. real operator content
// emissions from emitDeliverContent, NOT a bare deliver trigger.
func deliverMessagesWithContent(t *testing.T, h *testHarness) int {
	t.Helper()
	// Filter on the unique phase tag only — store.MessageFilter.Tags uses OR
	// semantics, so including TagSettle would also match every other settle phase.
	msgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{
		Tags: []string{exchange.TagPhasePrefix + exchange.SettlePhaseStrDeliver},
	})
	n := 0
	for _, m := range msgs {
		var p struct {
			Content     string `json:"content"`
			BlobPointer string `json:"blob_pointer"`
		}
		if err := json.Unmarshal(m.Payload, &p); err != nil {
			continue
		}
		if p.Content != "" || p.BlobPointer != "" {
			n++
		}
	}
	return n
}

// buyerAcceptRejectMessages returns the settle(buyer-accept-reject) messages on
// the log (ed2-D §3.6).
func buyerAcceptRejectMessages(t *testing.T, h *testHarness) []store.MessageRecord {
	t.Helper()
	// Unique phase tag only (Tags filter is OR — see deliverMessagesWithContent).
	msgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{
		Tags: []string{exchange.TagPhasePrefix + exchange.SettlePhaseStrBuyerAcceptReject},
	})
	return msgs
}

// TestUnfundedBuyerAcceptDeliver_NoFreeContent_ed2D is the ed2-D exploit-closed
// proof: an underfunded buyer that sends buyer-accept + settle(deliver) receives
// the durable reject and NO content; scrip supply is conserved.
func TestUnfundedBuyerAcceptDeliver_NoFreeContent_ed2D(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)

	// Fund the buyer with a TINY amount — enough to exist, far below any price so
	// the buyer-accept hold is guaranteed to fail. Mint BEFORE constructing the
	// scrip store so its Replay picks up the balance.
	const buyerFunds = int64(10)
	addScripMintMsg(t, h, h.buyer.PublicKeyHex(), buyerFunds)

	cs := newCampfireScripStore(t, h)
	eng := exchange.NewEngine(exchange.EngineOptions{
		CampfireID:        h.cfID,
		LocalStore:        h.st,
		OperatorPublicKey: h.operator.pubKeyHex,
		ScripStore:        cs,
		Logger:            func(format string, args ...any) { t.Logf("[engine] "+format, args...) },
	})

	// Seed one inventory entry (seller put → operator accept), put price 5000.
	seedInventoryEntry(t, h, eng, "underfunded free-content exploit fixture", "code", 10000, 5000)
	inv := eng.State().Inventory()
	if len(inv) == 0 {
		t.Fatal("no inventory entry seeded")
	}
	entry := inv[0]

	// Sanity: the required hold dwarfs the buyer's balance — this IS an
	// underfunded buyer, the exploit precondition.
	salePrice := eng.ComputePriceForTest(entry)
	holdAmount := salePrice + salePrice/exchange.MatchingFeeRate
	if buyerFunds >= holdAmount {
		t.Fatalf("test misconfigured: buyer funds %d >= hold %d — buyer is not underfunded", buyerFunds, holdAmount)
	}

	supplyBefore := cs.TotalSupply()

	// Drive the buy through the running engine to obtain a real match. The buy's
	// stated budget is high (the buyer LIES about willingness) — the actual scrip
	// balance is what gates the hold, and it is tiny.
	preMatch, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	done := make(chan struct{})
	go func() { _ = eng.Start(ctx); close(done) }()
	h.sendMessage(h.buyer,
		buyPayload("query for underfunded free-content exploit fixture", salePrice+5000),
		[]string{exchange.TagBuy},
		nil,
	)
	matchMsg := waitForMatchMessage(t, h, preMatch, 2*time.Second)
	cancel()
	// Wait for the poll loop to fully exit so the buyer-accept and deliver below
	// are dispatched EXACTLY ONCE by our manual DispatchForTest — no concurrent
	// poll-loop dispatch double-firing the reject emit (which has no idempotency
	// guard, unlike the funded buyer-accept hold).
	<-done

	// ── ATTACK STEP 1: buyer-accept from the underfunded buyer. The hold fails. ──
	buyerAcceptPayload, _ := json.Marshal(map[string]any{
		"phase":    "buyer-accept",
		"entry_id": entry.EntryID,
		"accepted": true,
	})
	buyerAccept := h.sendMessage(h.buyer, buyerAcceptPayload,
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrBuyerAccept,
			exchange.TagVerdictPrefix + "accepted",
		},
		[]string{matchMsg.ID},
	)
	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))
	baRec, err := h.st.GetMessage(buyerAccept.ID)
	if err != nil {
		t.Fatalf("GetMessage(buyer-accept): %v", err)
	}
	dispErr := eng.DispatchForTest(exchange.FromStoreRecord(baRec))
	// The dispatch surfaces the budget error (logged+dropped by the real poll
	// loop). We tolerate it here — the point is the WIRE side effects below.
	if dispErr == nil {
		t.Fatal("expected buyer-accept dispatch to fail (insufficient scrip), got nil")
	}

	// §3.6 assertion: a DURABLE, wire-visible buyer-accept-reject was emitted,
	// antecedent = the buyer-accept id, reason = insufficient_scrip.
	rejects := buyerAcceptRejectMessages(t, h)
	if len(rejects) != 1 {
		t.Fatalf("expected exactly 1 settle(buyer-accept-reject), got %d (ed2-D §3.6 regression)", len(rejects))
	}
	rej := rejects[0]
	if rej.Sender != h.operator.PublicKeyHex() {
		t.Fatalf("buyer-accept-reject sender = %s, want operator %s", rej.Sender, h.operator.PublicKeyHex())
	}
	if len(rej.Antecedents) != 1 || rej.Antecedents[0] != buyerAccept.ID {
		t.Fatalf("buyer-accept-reject antecedents = %v, want [%s]", rej.Antecedents, buyerAccept.ID)
	}
	var rejPayload struct {
		Phase  string `json:"phase"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(rej.Payload, &rejPayload); err != nil {
		t.Fatalf("unmarshal buyer-accept-reject payload: %v", err)
	}
	if rejPayload.Phase != exchange.SettlePhaseStrBuyerAcceptReject {
		t.Fatalf("buyer-accept-reject phase = %q, want %q", rejPayload.Phase, exchange.SettlePhaseStrBuyerAcceptReject)
	}
	if rejPayload.Reason != "insufficient_scrip" {
		t.Fatalf("buyer-accept-reject reason = %q, want %q", rejPayload.Reason, "insufficient_scrip")
	}

	// No scrip hold was durably recorded — the reservation does not exist.
	if resID := extractReservationIDFromLog(t, h); resID != "" {
		t.Fatalf("expected NO scrip-buy-hold reservation after failed buyer-accept, got %q", resID)
	}

	// ── ATTACK STEP 2: pull the content anyway. Operator emits a settle(deliver) ──
	// trigger e-tagging the buyer-accept (buyerAcceptToMatch was folded
	// unconditionally, so the antecedent chain resolves). WITHOUT the §3.7 guard
	// this would emit the full content FREE.
	deliverTriggerPayload, _ := json.Marshal(map[string]any{
		"phase":    "deliver",
		"entry_id": entry.EntryID,
	})
	deliverTrigger := h.sendMessage(h.operator, deliverTriggerPayload,
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrDeliver,
		},
		[]string{buyerAccept.ID},
	)
	allMsgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(allMsgs))
	dtRec, err := h.st.GetMessage(deliverTrigger.ID)
	if err != nil {
		t.Fatalf("GetMessage(deliver-trigger): %v", err)
	}
	if err := eng.DispatchForTest(exchange.FromStoreRecord(dtRec)); err != nil {
		t.Fatalf("deliver dispatch returned error: %v", err)
	}

	// §3.7 assertion (THE money-integrity gate): NO settle(deliver) carrying
	// content was emitted. The only deliver on the log is our contentless trigger.
	if n := deliverMessagesWithContent(t, h); n != 0 {
		t.Fatalf("FREE-CONTENT EXPLOIT OPEN: %d settle(deliver) message(s) carry content for an unfunded buyer (ed2-D §3.7 regression)", n)
	}

	// No scrip-settle happened, and total supply is conserved (no mint, no burn).
	if n := countScripSettle(t, h); n != 0 {
		t.Fatalf("expected 0 scrip-settle for an unfunded exploit attempt, got %d", n)
	}
	if got := cs.Balance(h.buyer.PublicKeyHex()); got != buyerFunds {
		t.Fatalf("buyer balance moved: got %d, want %d (no scrip should move)", got, buyerFunds)
	}
	if got := cs.TotalSupply(); got != supplyBefore {
		t.Fatalf("total scrip supply changed: got %d, want %d (supply must be conserved)", got, supplyBefore)
	}
}
