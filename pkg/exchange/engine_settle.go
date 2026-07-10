package exchange

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/campfire-net/dontguess/pkg/scrip"
)

// handleSettle processes settlement messages.
//
// For settle(buyer-accept) phases, if ScripStore is configured, the engine:
//   - Checks the buyer's scrip balance (fails if insufficient)
//   - Pre-decrements the buyer's balance by (price + fee)
//   - Stores the reservation ID in matchToReservation for the complete handler
//
// For settle(complete) phases, if ScripStore is configured, the engine:
//   - Pays the seller their residual (price * ResidualRate / 100)
//   - Burns the matching fee (price * MatchingFeeRate / 100)
//   - Credits exchange revenue (remainder) to the operator
//
// For settle(preview-request) phases, the engine generates a content preview
// using PreviewAssembler and responds with a settle(preview) message. The
// preview antecedent is the preview-request message ID.
func (e *Engine) handleSettle(msg *Message) error {
	phase := settlePhaseFromTags(msg.Tags)

	// Handle preview-request: generate and send a preview response.
	if phase == SettlePhaseStrPreviewRequest {
		return e.handleSettlePreviewRequest(msg)
	}

	// Handle small-content-dispute: fully automated refund path, no operator required.
	if phase == SettlePhaseStrSmallContentDispute {
		return e.handleSettleSmallContentDispute(msg)
	}

	// Handle deliver: emit full content to buyer (does not require ScripStore).
	if phase == SettlePhaseStrDeliver {
		return e.handleSettleDeliverContent(msg)
	}

	// Emit a consume/accept behavioral signal when the buyer completes a
	// transaction. This fires unconditionally when no ScripStore is configured
	// (no reservation concept exists) so the reporter can measure actual buyer
	// usage, not just matcher hits. When a ScripStore IS configured, the
	// reservation-consumed check runs BEFORE the emit (relay-transport.md §E
	// MUST-ENFORCE(3), ADV-7 defense-in-depth): a settle(complete) redelivery
	// whose reservation was already consumed (or never existed) must not emit
	// a second/spurious consume signal.
	if phase == SettlePhaseStrComplete {
		if e.opts.ScripStore == nil || e.hasLiveReservationForComplete(msg) {
			if err := e.emitConsumeSignal(msg); err != nil {
				// Best-effort: log but do not abort the settle flow.
				e.opts.log("engine: settle: emitConsumeSignal: %v", err)
			}
			// Refresh behavioral signals in the match index after settle:complete.
			e.matchIndex.SetBehavioralSignals(e.state.AllEntryBehavioralSignals())
		} else {
			e.opts.log("engine: settle: complete msg=%s has no live reservation (already consumed or absent) — skipping consume signal",
				shortKey(msg.ID))
		}
	}

	if e.opts.ScripStore == nil {
		return nil
	}

	// Handle buyer-accept: scrip hold happens here (not at buy time).
	if phase == SettlePhaseStrBuyerAccept {
		return e.handleSettleBuyerAcceptScrip(msg)
	}

	if phase != SettlePhaseStrComplete {
		// Other phases (put-accept) are tracked in state only.
		return nil
	}

	return e.handleSettleComplete(msg)
}

// handleSettleComplete processes the settle(complete) scrip payment phase.
// Called from handleSettle when ScripStore is configured and phase is complete.
func (e *Engine) handleSettleComplete(msg *Message) error {
	// Derive seller from the antecedent chain: complete → deliver → match → entry → seller.
	sellerKey, deliverMsgID, ok := e.resolveSellerFromComplete(msg)
	if !ok {
		return nil
	}

	// Derive match message ID for reservation lookup.
	matchMsgID, ok := e.state.MatchForDeliver(deliverMsgID)
	if !ok {
		e.opts.log("engine: settle: cannot derive match for deliver=%s — antecedent chain broken", shortKey(deliverMsgID))
		return nil
	}

	// Derive entryID for co-occurrence recording, next-work prediction, and
	// high-reuse residual classification.
	var settledEntry *InventoryEntry
	if se, entryOK := e.state.EntryForDeliver(deliverMsgID); entryOK {
		settledEntry = se
		buyerKey := msg.Sender
		e.recordBuyerSettlement(buyerKey, settledEntry.EntryID)
		e.stagePredictions(settledEntry.EntryID)
	}

	// Look up the reservation created at buyer-accept time (not from buyer payload).
	reservationID, hasReservation := e.reservationFor(matchMsgID)
	if !hasReservation || reservationID == "" {
		e.opts.log("engine: settle: no reservation found for match=%s — buyer-accept scrip hold may not have run", shortKey(matchMsgID))
		return nil
	}

	ctx := e.engineCtx()

	// Deadline-miss check (insurance guarantee). checkDeadlineMiss handles the
	// full refund internally and never surfaces an error to the settle flow
	// (dontguess-471 dead-code cleanup): a failed refund falls through to normal
	// settlement rather than aborting.
	if e.checkDeadlineMiss(ctx, msg, matchMsgID, reservationID) {
		return nil
	}

	return e.performScripSettlement(ctx, msg, sellerKey, matchMsgID, reservationID, settledEntry)
}

// resolveSellerFromComplete derives the seller key and deliver message ID from
// a settle(complete) message's antecedent chain.
// Returns (sellerKey, deliverMsgID, ok).
func (e *Engine) resolveSellerFromComplete(msg *Message) (string, string, bool) {
	if len(msg.Antecedents) == 0 {
		e.opts.log("engine: settle: complete message has no antecedents — cannot derive seller")
		return "", "", false
	}
	deliverMsgID := msg.Antecedents[0]
	sellerKey, ok := e.state.SellerKeyForDeliver(deliverMsgID)
	if !ok {
		e.opts.log("engine: settle: cannot derive seller for deliver=%s — antecedent chain broken", shortKey(deliverMsgID))
		return "", "", false
	}
	return sellerKey, deliverMsgID, true
}

// hasLiveReservationForComplete reports whether a settle(complete) message's
// match has a live (not-yet-consumed) scrip reservation, i.e. whether
// e.matchToReservation still holds an entry for the match derived from this
// complete message's antecedent chain (complete → deliver → match).
//
// Used to gate emitConsumeSignal (relay-transport.md §E MUST-ENFORCE(3)): a
// redelivered settle(complete) whose reservation was already consumed by an
// earlier dispatch of the same event — or a settle(complete) for a match
// that never had a buyer-accept scrip hold in the first place — must not
// emit a consume signal. This mirrors, without duplicating, the reservation
// lookup handleSettleComplete performs a moment later for the actual
// settlement; on the false path handleSettleComplete's own antecedent-chain
// / "no reservation found" logging still runs (accepted duplication for a
// log line on an already-error/edge path, not a correctness concern).
func (e *Engine) hasLiveReservationForComplete(msg *Message) bool {
	_, deliverMsgID, ok := e.resolveSellerFromComplete(msg)
	if !ok {
		return false
	}
	matchMsgID, ok := e.state.MatchForDeliver(deliverMsgID)
	if !ok {
		return false
	}
	reservationID, hasReservation := e.reservationFor(matchMsgID)
	return hasReservation && reservationID != ""
}

// checkDeadlineMiss checks whether the settle(complete) arrived after the
// guarantee_deadline. If so, it issues a full refund and returns true (the
// caller must stop — the reservation has been consumed by the refund). If there
// is no deadline miss (or the refund itself failed and we fell through to normal
// settlement) it returns false.
//
// It returns no error: every failure path here is handled internally (a failed
// refund is logged and falls through to normal settlement so the buyer is not
// double-refunded), so there was never an error for the caller to act on
// (dontguess-471 dead-code cleanup).
func (e *Engine) checkDeadlineMiss(ctx context.Context, msg *Message, matchMsgID, reservationID string) bool {
	deadline, insuredAmount, hasGuarantee := e.state.GuaranteeForMatch(matchMsgID)
	if !hasGuarantee {
		return false
	}
	// TRUST + DETERMINISM (relay-transport.md §4 ADV-10 + §Sequencer): the
	// deadline-miss verdict must be derived from an OPERATOR-TRUSTED,
	// replay-deterministic reference time — NEVER wall-clock time.Now() (which
	// makes the refund-vs-settle outcome depend on when replay happens) and
	// NEVER the buyer-authored settle(complete) msg.Timestamp (which a buyer
	// could set to any value to force or dodge the full refund). The operator's
	// own settle(deliver) Timestamp is the authoritative "when did the exchange
	// deliver" signal: deliver is operator-authored and persisted, so it is both
	// counterparty-unforgeable and identical on every replay. The guarantee is
	// missed iff the operator delivered after the deadline.
	deliverTS, haveDeliver := e.state.DeliverTimeForMatch(matchMsgID)
	if !haveDeliver || deliverTS == 0 {
		// No operator-trusted delivery time is available — we cannot make a
		// sound deadline verdict, so do NOT auto-refund (fail closed toward the
		// normal settlement path rather than a manipulable/nondeterministic one).
		return false
	}
	if !time.Unix(0, deliverTS).UTC().After(deadline) {
		return false
	}
	if err := e.handleDeadlineMissRefund(ctx, msg, matchMsgID, reservationID, insuredAmount); err != nil {
		e.opts.log("engine: settle: deadline-miss refund failed for match=%s: %v", shortKey(matchMsgID), err)
		// Fall through to normal settlement — refund failed, do not double-pay.
		return false
	}
	e.opts.log("engine: settle: deadline-miss refund issued for match=%s deadline=%s",
		shortKey(matchMsgID), deadline.Format(time.RFC3339))
	return true
}

// performScripSettlement executes the scrip distribution for a completed settle:
// consumes the reservation, pays residual to seller, credits exchange revenue to
// operator, emits scrip-settle and scrip-burn convention messages.
func (e *Engine) performScripSettlement(ctx context.Context, msg *Message, sellerKey, matchMsgID, reservationID string, settledEntry *InventoryEntry) error {
	// Settled-match guard (dontguess-400 FIX-M1, design §1.4): never emit a second
	// scrip-settle for a match that has already settled. This is the durable belt
	// to the buyer-accept-side suspenders: even if a reservation were somehow
	// re-hydrated, a match settles at most once. The set is rebuilt on Replay from
	// the scrip-settle log AND marked live below, so the guard holds both across a
	// restart and within a single session (before the scrip-settle folds).
	if e.state.IsMatchSettled(matchMsgID) {
		e.opts.log("engine: settle: match %s already settled — skipping settlement (double-settle guard)",
			shortKey(matchMsgID))
		return nil
	}

	// Atomically retrieve and delete reservation (prevents TOCTOU double-spend).
	res, err := e.opts.ScripStore.ConsumeReservation(ctx, reservationID)
	if err != nil {
		e.opts.log("engine: settle: reservation %s not found: %v", shortKey(reservationID), err)
		return nil // reservation missing — already settled or expired
	}

	// Derive price and fee from reservation amount (locked at buyer-accept time).
	price := res.Amount * MatchingFeeRate / (MatchingFeeRate + 1)
	fee := price / MatchingFeeRate

	// High-reuse residual classification.
	residualDenom := int64(ResidualRate)
	if settledEntry != nil && IsHighReuseArtifact(settledEntry) {
		residualDenom = HighReuseResidualDenominator
	}
	residual := price / residualDenom
	exchangeRevenue := price - residual

	operatorKey := e.state.OperatorKey

	// Marshal both convention messages BEFORE mutating scrip state.
	settlePayload, burnPayload, err := e.marshalSettlePayloads(msg, matchMsgID, sellerKey, reservationID, res, residual, fee, exchangeRevenue)
	if err != nil {
		// Restore reservation so the settle can be retried.
		if restoreErr := e.opts.ScripStore.SaveReservation(ctx, res); restoreErr != nil {
			e.opts.log("engine: settle: CRITICAL: failed to restore reservation %s after marshal failure: %v",
				shortKey(reservationID), restoreErr)
			return fmt.Errorf("scrip: settle reservation %s: marshal failed AND restore failed (reservation lost): %w",
				shortKey(reservationID), err)
		}
		return err
	}

	// EMIT-DURABLE-THEN-MUTATE (relay-transport.md §E MUST-ENFORCE(2)): the
	// scrip-settle convention message must land in the durable log BEFORE any
	// balance mutation runs — matching the ordering handleDeadlineMissRefund
	// already uses. The previous ordering here mutated balances first and
	// emitted last (ADV-12): a crash between the two left a live balance
	// change with no durable record for Replay to reconstruct on restart,
	// silently destroying the seller/operator credit. scrip-burn failure
	// stays best-effort (fee-sink bookkeeping only, not fund custody) but is
	// still emitted before the mutation for the same reason.
	if _, emitErr := e.sendOperatorMessage(settlePayload,
		[]string{scrip.TagScripSettle}, []string{msg.ID}); emitErr != nil {
		// Restore reservation so the settle can be retried.
		if restoreErr := e.opts.ScripStore.SaveReservation(ctx, res); restoreErr != nil {
			e.opts.log("engine: settle: CRITICAL: failed to restore reservation %s after scrip-settle emit failure: %v",
				shortKey(reservationID), restoreErr)
			return fmt.Errorf("scrip: settle reservation %s: emit scrip-settle failed AND restore failed (reservation lost): %w",
				shortKey(reservationID), emitErr)
		}
		return fmt.Errorf("scrip: settle: emit scrip-settle: %w", emitErr)
	}
	// PAST THE POINT OF NO RETURN (relay-transport.md §E, dontguess-4be): the
	// scrip-settle is now durably recorded and is AUTHORITATIVE. applySettle
	// credits residual→seller and revenue→operator on EVERY replay of this
	// message (deduped only by message ID), so a cold rebuild reconstructs the
	// full settlement from this one record regardless of what the live balance
	// mutations below do. Consequences that the code below MUST honor:
	//   - The reservation is settled and GONE. Clean up the engine-side mapping
	//     NOW, unconditionally, so cleanup is consistent whether or not the
	//     live credits succeed — no retry may re-enter settlement for this match.
	//   - A subsequent live-balance mutation failure must NOT restore the
	//     reservation and must NOT emit settle(failed)-retry. Either would
	//     contradict the durable settle: on the buyer's retry a SECOND
	//     scrip-settle would be emitted and Replay would double-credit
	//     (double-mint). Such a failure is a LOUD hard error; the missing live
	//     credit is reconciled from the durable log on the next Replay.
	e.deleteReservation(matchMsgID)

	// Mark the match settled LIVE (dontguess-400 FIX-M1). The scrip-settle is now
	// durable and authoritative; retire the match so no re-accept can re-hold and
	// no retry can re-settle it within this session. The same marker is rebuilt on
	// Replay by applyScripSettle folding the durable scrip-settle just emitted, so
	// this is a session-local fast path, not the source of truth.
	e.state.MarkMatchSettled(matchMsgID)

	if len(burnPayload) > 0 {
		if _, emitErr := e.sendOperatorMessage(burnPayload,
			[]string{scrip.TagScripBurn}, []string{msg.ID}); emitErr != nil {
			e.opts.log("engine: warning: emit scrip-burn: %v", emitErr)
		}
	}

	// Credit residual to seller. Post-durable-emit: failure is a loud hard
	// error, never a restore/retry (see block above).
	if err := e.creditResidualToSeller(ctx, sellerKey, reservationID, residual); err != nil {
		return err
	}

	// Credit exchange revenue to operator. Post-durable-emit: failure is a loud
	// hard error, never a restore/retry (see block above).
	if err := e.creditRevenueToOperator(ctx, operatorKey, reservationID, exchangeRevenue); err != nil {
		return err
	}

	e.opts.log("engine: settle: reservation=%s seller=%s price=%d residual=%d fee_burned=%d exchange=%d",
		shortKey(reservationID), shortKey(sellerKey), price, residual, fee, exchangeRevenue)
	return nil
}

// nextMonotonicTimestamp returns a UnixNano timestamp guaranteed to be
// STRICTLY greater than any timestamp previously returned by this method on
// this Engine instance. It is the local emission clock used by
// sendLocalOperatorMessage (engine_core.go) for every operator-emitted
// message in LocalStore/relay-only mode (EngineOptions.WriteClient == nil).
//
// Guards docs/design/relay-transport.md §E MUST-ENFORCE(1): a nanosecond-
// granularity wall-clock tie between two fast successive emissions, or a
// backward NTP step, would otherwise let two of the operator's own emitted
// events land with equal or reordered timestamps. That matters specifically
// for scrip events (scrip-settle, scrip-buy-hold, scrip-burn, scrip-
// dispute-refund): a Seq-less DR rebuild orders events by the canonical
// (Timestamp,ID) linear extension (Sequencer.SequenceForFold), and ID is a
// random identifier uncorrelated with emission order — so a timestamp tie
// among the operator's own totally-ordered emissions (§E: all scrip events
// are operator-authored and never causally concurrent with each other)
// would let the tie-break scramble true emission order on rebuild, breaking
// fold determinism. This is the operator's own single-writer local clock —
// no cross-process coordination is needed (relay/campfire-delivered events
// carry their own transport timestamp, assigned upstream, not here).
func (e *Engine) nextMonotonicTimestamp() int64 {
	e.emitClockMu.Lock()
	defer e.emitClockMu.Unlock()
	now := time.Now().UnixNano()
	if now <= e.lastEmitNanos {
		now = e.lastEmitNanos + 1
	}
	e.lastEmitNanos = now
	return now
}

// marshalSettlePayloads marshals the scrip-settle and scrip-burn payloads
// before any balance mutation. Returns an error if either marshal fails.
func (e *Engine) marshalSettlePayloads(msg *Message, matchMsgID, sellerKey, reservationID string, res scrip.Reservation, residual, fee, exchangeRevenue int64) (settlePayload, burnPayload []byte, err error) {
	settlePayload, err = e.marshal(scrip.SettlePayload{
		ReservationID:   reservationID,
		Seller:          sellerKey,
		Residual:        residual,
		FeeBurned:       fee,
		ExchangeRevenue: exchangeRevenue,
		// MatchMsg is the MATCH msg ID (its documented meaning) — the durable key
		// the State fold (applyScripSettle) uses to rebuild the settled-match set
		// on Replay (dontguess-400 FIX-M1). Previously this carried the complete
		// msg.ID, which no consumer read; it is now the match identity so the
		// settled-match set survives a cold rebuild.
		MatchMsg:   matchMsgID,
		ResultHash: "",
	})
	if err != nil {
		return nil, nil, fmt.Errorf("scrip: marshal settle payload: %w", err)
	}
	if fee > 0 {
		burnPayload, err = e.marshal(scrip.BurnPayload{
			Amount:    fee,
			Reason:    "matching-fee",
			SourceMsg: msg.ID,
		})
		if err != nil {
			return nil, nil, fmt.Errorf("scrip: marshal burn payload: %w", err)
		}
	}
	return settlePayload, burnPayload, nil
}

// creditResidualToSeller credits residual scrip to the seller.
//
// This runs AFTER the authoritative scrip-settle record is durably emitted
// (see performScripSettlement, relay-transport.md §E). It is therefore a
// best-effort live-state update: applySettle re-credits this residual on every
// Replay of the durable record. A failure here must NOT restore the reservation
// and must NOT emit settle(failed) — either would contradict the durable settle
// and, on the buyer's retry, mint a SECOND scrip-settle (double-mint). A failure
// is surfaced as a LOUD hard error; the credit is reconciled from the log on the
// next Replay.
func (e *Engine) creditResidualToSeller(ctx context.Context, sellerKey, reservationID string, residual int64) error {
	if residual <= 0 {
		return nil
	}
	if _, _, err := e.opts.ScripStore.AddBudget(ctx, sellerKey, scrip.BalanceKey, residual, ""); err != nil {
		e.opts.log("engine: settle: CRITICAL: AddBudget(seller %s) FAILED after durable scrip-settle emit for reservation %s: %v — durable record is authoritative; NOT restoring reservation, NOT emitting settle(failed); live credit deferred to next Replay",
			shortKey(sellerKey), shortKey(reservationID), err)
		return fmt.Errorf("scrip: settle: AddBudget(seller %s) after durable emit (reservation %s settled in log, reconciled on Replay): %w",
			shortKey(sellerKey), shortKey(reservationID), err)
	}
	return nil
}

// creditRevenueToOperator credits exchange revenue to the operator.
//
// Like creditResidualToSeller, this runs AFTER the authoritative scrip-settle is
// durably emitted. applySettle re-credits both residual (seller) and revenue
// (operator) from that one durable record on every Replay, so a failure here is
// NOT rolled back against the seller credit and does NOT restore the reservation
// or emit settle(failed): doing so would fight the durable settle and double-mint
// on retry. A failure is a LOUD hard error; the operator credit is reconciled
// from the log on the next Replay.
func (e *Engine) creditRevenueToOperator(ctx context.Context, operatorKey, reservationID string, exchangeRevenue int64) error {
	if exchangeRevenue <= 0 {
		return nil
	}
	if _, _, err := e.opts.ScripStore.AddBudget(ctx, operatorKey, scrip.BalanceKey, exchangeRevenue, ""); err != nil {
		e.opts.log("engine: settle: CRITICAL: AddBudget(operator) FAILED after durable scrip-settle emit for reservation %s: %v — durable record is authoritative; NOT rolling back seller credit, NOT restoring reservation, NOT emitting settle(failed); live credit deferred to next Replay",
			shortKey(reservationID), err)
		return fmt.Errorf("scrip: settle: AddBudget(operator) after durable emit (reservation %s settled in log, reconciled on Replay): %w",
			shortKey(reservationID), err)
	}
	return nil
}

// emitConsumeSignal records a buyer consume/accept behavioral signal when a
// settle(complete) is received. The signal is an exchange:consume message
// carrying the entry_id (derived from the antecedent chain, not the buyer
// payload) and buyer_key, with the complete message as antecedent.
//
// Best-effort: the caller logs errors but does not abort the settle flow.
//
// This is the authoritative signal that the buyer actually used a delivered
// candidate — stronger than a hit (matcher returned something) and the
// foundation for the heritage "behavioral signals over preferences" metric.
func (e *Engine) emitConsumeSignal(completeMsg *Message) error {
	if len(completeMsg.Antecedents) == 0 {
		return fmt.Errorf("consume signal: complete message has no antecedents")
	}
	deliverMsgID := completeMsg.Antecedents[0]
	settledEntry, ok := e.state.EntryForDeliver(deliverMsgID)
	if !ok {
		return fmt.Errorf("consume signal: cannot derive entry for deliver=%s — antecedent chain broken", shortKey(deliverMsgID))
	}
	payload, err := e.marshal(map[string]any{
		"entry_id":  settledEntry.EntryID,
		"buyer_key": completeMsg.Sender,
	})
	if err != nil {
		return fmt.Errorf("consume signal: marshal: %w", err)
	}
	consumeMsg, err := e.sendOperatorMessage(payload, []string{TagConsume}, []string{completeMsg.ID})
	if err != nil {
		return fmt.Errorf("consume signal: send: %w", err)
	}
	// Apply the emitted consume message to live state immediately so that
	// entryConsumeCount (and thus AllEntryBehavioralSignals) reflects the signal
	// without requiring a replay/restart. Mirrors the match path and buy-miss path.
	if consumeMsg != nil {
		e.state.Apply(consumeMsg)
	}
	return nil
}

// handleSettleBuyerAcceptScrip performs the scrip hold when a buyer sends a
// settle(buyer-accept) message. This is the "preview-before-purchase" model:
// scrip is locked when the buyer has reviewed the preview and decided to proceed,
// not at buy time.
//
// On success:
//   - Buyer's balance is decremented by (price + fee)
//   - A reservation is saved in ScripStore
//   - The reservation ID is stored in matchToReservation[matchMsgID]
//   - A scrip-buy-hold convention message is emitted for CampfireScripStore replay
//
// The match message ID is resolved from the antecedent chain:
//
//	buyer-accept → preview (optional) → match
//
// This mirrors the antecedent resolution in state.applySettleBuyerAccept.
func (e *Engine) handleSettleBuyerAcceptScrip(msg *Message) error {
	if len(msg.Antecedents) == 0 {
		e.opts.log("engine: buyer-accept scrip: no antecedents, ignoring msg=%s", shortKey(msg.ID))
		return nil
	}
	antecedentID := msg.Antecedents[0]

	// Resolve the match message ID from the antecedent.
	matchMsgID, expectedBuyer, hasMatch := e.state.ResolveMatchFromAntecedent(antecedentID)
	if !hasMatch {
		e.opts.log("engine: buyer-accept scrip: unknown match %s, ignoring", shortKey(matchMsgID))
		return nil
	}

	// Enforce buyer identity: only the original buyer may trigger a scrip hold.
	if msg.Sender != expectedBuyer {
		e.opts.log("engine: buyer-accept scrip: sender %s is not buyer for match %s, ignoring",
			shortKey(msg.Sender), shortKey(matchMsgID))
		return nil
	}

	// Settled-match guard (dontguess-400 FIX-M1, design §1.4): a match is a
	// single-use identity — once its scrip settlement is durable it must never
	// accept a new buyer-accept. Without this, a re-sent buyer-accept for an
	// already-settled match would either re-hydrate the consumed reservation
	// (restoreExistingHold) or re-decrement the buyer (decAndSaveHold) and let a
	// following complete emit a SECOND scrip-settle — the double-settle mint. A
	// legitimate re-purchase is a NEW buy → NEW match msg ID, unaffected.
	if e.state.IsMatchSettled(matchMsgID) {
		e.opts.log("engine: buyer-accept scrip: match %s already settled — ignoring re-accept (double-settle guard)",
			shortKey(matchMsgID))
		return nil
	}

	// Idempotency: if a hold already exists for this match, skip.
	if existingResID := e.findExistingBuyerAcceptHold(matchMsgID); existingResID != "" {
		return e.restoreExistingHold(msg, matchMsgID, existingResID)
	}

	// Determine the price for the entry offered in this match.
	entryID := e.state.MatchEntryID(matchMsgID)
	entry := e.state.GetInventoryEntry(entryID)
	if entry == nil {
		e.opts.log("engine: buyer-accept scrip: entry %s not found for match %s, ignoring",
			shortKey(entryID), shortKey(matchMsgID))
		return nil
	}

	bestPrice := e.computePrice(entry)
	fee := bestPrice / MatchingFeeRate
	holdAmount := bestPrice + fee

	err := e.decAndSaveHold(msg, matchMsgID, holdAmount, bestPrice, fee)
	if err != nil && errors.Is(err, scrip.ErrBudgetExceeded) {
		// ed2-D §3.6: the buyer had insufficient scrip — decAndSaveHold saved NO
		// reservation (so the §3.7 deliver guard will withhold content). Emit a
		// durable, wire-visible settle(buyer-accept-reject) BEFORE returning the
		// error so the buyer learns *why* buyer-accept failed instead of only
		// timing out. This is the mutually-exclusive alternative to deliver for a
		// buyer-accept: a failed hold produces the reject and NO deliverable link
		// (§3.7 item 2). Reached only with ScripStore != nil (handleSettle gates
		// this handler on it), so the individual tier is untouched.
		if rejErr := e.emitBuyerAcceptReject(msg.ID, "insufficient_scrip"); rejErr != nil {
			e.opts.log("engine: buyer-accept: CRITICAL: failed to emit durable buyer-accept-reject for buyer-accept=%s: %v",
				shortKey(msg.ID), rejErr)
		}
	}
	return err
}

// emitBuyerAcceptReject emits the durable, wire-visible settle(buyer-accept-reject)
// operator message (ed2-D §3.6). It mirrors rejectPutLocked's emit pattern
// (TagSettle + phase + verdict:rejected + reason, antecedent = the rejected
// message id) so the buyer's per-phase settle subscription receives a loud,
// attributable reason for a failed buyer-accept rather than a bare timeout. The
// antecedent is the buyer-accept message id — the settle chain e-tags its
// immediate antecedent, matching applySettleDeliver's chain (state_settle.go).
// The phase has no state-fold handler; the message is purely for wire visibility.
func (e *Engine) emitBuyerAcceptReject(buyerAcceptMsgID, reason string) error {
	payload, err := e.marshal(map[string]any{
		"phase":    SettlePhaseStrBuyerAcceptReject,
		"entry_id": buyerAcceptMsgID,
		"reason":   reason,
		"guide":    "Your buyer-accept was rejected: insufficient scrip to reserve the price + fee. No content was delivered and no scrip moved. Ask the operator to run: dontguess mint <your-npub> <amount>, then retry the buy.",
	})
	if err != nil {
		return fmt.Errorf("engine: buyer-accept-reject: marshal payload: %w", err)
	}
	tags := []string{
		TagSettle,
		TagPhasePrefix + SettlePhaseStrBuyerAcceptReject,
		TagVerdictPrefix + "rejected",
	}
	antecedents := []string{buyerAcceptMsgID}
	if _, err := e.sendOperatorMessage(payload, tags, antecedents); err != nil {
		return fmt.Errorf("engine: buyer-accept-reject: send operator message: %w", err)
	}
	return nil
}

// restoreExistingHold re-hydrates an in-memory reservation when a scrip-buy-hold
// was already written to the campfire log on a previous engine run. This prevents
// double-charging the buyer on restart.
//
// The restored Amount is the ORIGINAL amount that was held at buyer-accept time
// (read from the durable scrip-buy-hold event via GetBuyHoldAmount), NOT a fresh
// recomputation from the current dynamic price. The price may have drifted
// between the original hold and this restart; recomputing would restore a
// reservation whose Amount no longer matches the scrip the buyer actually had
// decremented, so a later settle/refund would move the wrong number of scrip
// (dontguess-471 MED). Fall back to a recompute only if the buy-hold amount is
// somehow unavailable (defensive — the reservation existed, so the event should
// too).
func (e *Engine) restoreExistingHold(msg *Message, matchMsgID, existingResID string) error {
	// Settled-match guard (dontguess-400 FIX-M1): never re-hydrate the reservation
	// of a match that has already settled. Re-saving a consumed reservation with no
	// recharge is exactly the defeat of performScripSettlement's "reservation
	// missing → already settled" guard that produced the double-settle mint.
	if e.state.IsMatchSettled(matchMsgID) {
		e.opts.log("engine: buyer-accept scrip: match %s already settled — refusing to re-hydrate reservation %s (double-settle guard)",
			shortKey(matchMsgID), shortKey(existingResID))
		return nil
	}
	ctx := e.engineCtx()
	_, currentETag, _ := e.opts.ScripStore.GetBudget(ctx, msg.Sender, scrip.BalanceKey)
	holdAmount, ok := e.state.GetBuyHoldAmount(matchMsgID)
	if !ok {
		// Defensive fallback: no recorded original amount. Recompute from current
		// price (pre-dontguess-471 behavior) rather than restore a zero hold.
		entryID := e.state.MatchEntryID(matchMsgID)
		if entry := e.state.GetInventoryEntry(entryID); entry != nil {
			p := e.computePrice(entry)
			holdAmount = p + p/MatchingFeeRate
		}
		e.opts.log("engine: buyer-accept scrip: warning: no recorded buy-hold amount for match=%s — recomputed hold=%d from current price",
			shortKey(matchMsgID), holdAmount)
	}
	res := scrip.Reservation{
		ID:        existingResID,
		AgentKey:  msg.Sender,
		RK:        scrip.BalanceKey,
		ETag:      currentETag,
		Amount:    holdAmount,
		CreatedAt: time.Now(),
	}
	if err := e.opts.ScripStore.SaveReservation(ctx, res); err != nil {
		e.opts.log("engine: buyer-accept scrip: warning: re-save reservation after restart %s: %v",
			shortKey(existingResID), err)
	}
	e.setReservation(matchMsgID, existingResID)
	e.opts.log("engine: buyer-accept scrip: hold already replayed, skipping pre-decrement buyer=%s reservation=%s",
		shortKey(msg.Sender), shortKey(existingResID))
	return nil
}

// decAndSaveHold decrements the buyer's balance, saves a new reservation, and
// emits the scrip-buy-hold convention message. Called when no prior hold exists.
func (e *Engine) decAndSaveHold(msg *Message, matchMsgID string, holdAmount, bestPrice, fee int64) error {
	ctx := e.engineCtx()
	buyerKey := msg.Sender

	bal, etag, err := e.opts.ScripStore.GetBudget(ctx, buyerKey, scrip.BalanceKey)
	if err != nil {
		return fmt.Errorf("scrip: buyer-accept: GetBudget for buyer %s: %w", shortKey(buyerKey), err)
	}
	if bal < holdAmount {
		return fmt.Errorf("scrip: buyer-accept: buyer %s: %w (balance=%d, required=%d)",
			shortKey(buyerKey), scrip.ErrBudgetExceeded, bal, holdAmount)
	}

	reservationID := newReservationID()
	expiresAt := time.Now().Add(ReservationExpiryDuration).UTC().Format(time.RFC3339)

	// Marshal the buy-hold convention message BEFORE mutating scrip state.
	holdPayload, err := e.marshal(scrip.BuyHoldPayload{
		Buyer:         buyerKey,
		Amount:        holdAmount,
		Price:         bestPrice,
		Fee:           fee,
		ReservationID: reservationID,
		BuyMsg:        matchMsgID, // references the match message (historical field name)
		ExpiresAt:     expiresAt,
	})
	if err != nil {
		return fmt.Errorf("scrip: marshal buyer-accept buy-hold payload: %w", err)
	}

	// EMIT-DURABLE-THEN-MUTATE (dontguess-400 FIX-M2, design §4; mirrors the
	// hardened settle path at performScripSettlement). The scrip-buy-hold record
	// must land in the durable log BEFORE the buyer's balance is decremented. The
	// previous ordering decremented first and emitted best-effort (warning on
	// failure): an emit failure left a LIVE debit with NO durable record, so
	// Replay (applyBuyHold folds the durable log) never reconstructs the debit —
	// the buyer's balance rebuilds HIGHER than it should, a net mint. Emitting
	// first makes emit failure a hard error with the balance untouched (no debit
	// without a durable hold). The balance-affecting decrement runs only after the
	// hold is durably recorded, and applyBuyHold re-applies it on every Replay.
	if _, emitErr := e.sendOperatorMessage(holdPayload,
		[]string{scrip.TagScripBuyHold}, []string{msg.ID}); emitErr != nil {
		return fmt.Errorf("scrip: buyer-accept: emit scrip-buy-hold (durable) for buyer %s: %w",
			shortKey(buyerKey), emitErr)
	}

	// PAST THE POINT OF NO RETURN: the scrip-buy-hold is durably recorded and is
	// AUTHORITATIVE — applyBuyHold decrements the buyer on every Replay. The live
	// DecrementBudget below merely keeps the in-memory balance in sync; a failure
	// here is a LOUD hard error, reconciled from the log on the next Replay. It
	// must NOT be treated as "no hold happened" — the durable record already
	// commits the debit.
	_, newETag, err := e.opts.ScripStore.DecrementBudget(ctx, buyerKey, scrip.BalanceKey, holdAmount, etag)
	if err != nil {
		e.opts.log("engine: buyer-accept scrip: CRITICAL: DecrementBudget(buyer %s) FAILED after durable scrip-buy-hold emit: %v — durable hold is authoritative; live debit deferred to next Replay",
			shortKey(buyerKey), err)
		return fmt.Errorf("scrip: buyer-accept: DecrementBudget for buyer %s after durable emit (hold recorded in log, reconciled on Replay): %w",
			shortKey(buyerKey), err)
	}

	// Save reservation so settle(complete) and dispute handlers can reference it.
	res := scrip.Reservation{
		ID:        reservationID,
		AgentKey:  buyerKey,
		RK:        scrip.BalanceKey,
		ETag:      newETag,
		Amount:    holdAmount,
		CreatedAt: time.Now(),
	}
	if err := e.opts.ScripStore.SaveReservation(ctx, res); err != nil {
		return fmt.Errorf("scrip: buyer-accept: SaveReservation: %w", err)
	}

	// Record the reservation so the complete handler can find it.
	e.setReservation(matchMsgID, reservationID)

	e.opts.log("engine: buyer-accept scrip: pre-decremented buyer=%s hold=%d reservation=%s match=%s",
		shortKey(buyerKey), holdAmount, shortKey(reservationID), shortKey(matchMsgID))

	return nil
}

// handleSettlePreviewRequest generates a content preview in response to a
// settle(preview-request) message from a buyer.
//
// The engine:
//  1. Validates the match exists in state (antecedent must be a match message).
//  2. Looks up the entry from the match.
//  3. Calls PreviewAssembler.Assemble() with the entry details and full content
//     to generate preview chunks. The preview is a subset of the full content
//     (5 non-overlapping random chunks totaling 15-25% of the content).
//  4. Sends a settle(preview) response with the antecedent set to the
//     preview-request message ID.
//
// If the antecedent is not a recognized match or the entry is not in inventory,
// the message is silently ignored (no error returned to the poll loop).
func (e *Engine) handleSettlePreviewRequest(msg *Message) error {
	if len(msg.Antecedents) == 0 {
		e.opts.log("engine: preview-request: no antecedents, ignoring msg=%s", msg.ID)
		return nil
	}
	matchMsgID := msg.Antecedents[0]

	// Validate match exists and sender is the expected buyer.
	expectedBuyer, matchEntryID, matchKnown, previewTracked := e.state.MatchInfo(matchMsgID, msg.ID)
	if !matchKnown {
		e.opts.log("engine: preview-request: unknown match %s, ignoring", shortKey(matchMsgID))
		return nil
	}
	if msg.Sender != expectedBuyer {
		e.opts.log("engine: preview-request: sender %s is not the expected buyer for match %s, ignoring",
			shortKey(msg.Sender), shortKey(matchMsgID))
		return nil
	}
	if !previewTracked {
		e.opts.log("engine: preview-request: state did not track msg=%s, ignoring", msg.ID)
		return nil
	}

	// Look up the entry.
	entry := e.state.GetInventoryEntry(matchEntryID)
	if entry == nil {
		e.opts.log("engine: preview-request: entry %s not in inventory, ignoring", shortKey(matchEntryID))
		return nil
	}

	return e.sendPreviewResponse(msg, matchMsgID, entry)
}

// sendPreviewResponse generates and emits the settle(preview) message for an entry.
func (e *Engine) sendPreviewResponse(msg *Message, matchMsgID string, entry *InventoryEntry) error {
	previewResult, err := previewForEntry(entry)
	if err != nil {
		return fmt.Errorf("engine: preview-request: assemble preview for entry %s: %w", shortKey(entry.EntryID), err)
	}

	type ChunkPayload struct {
		Content    string `json:"content"`
		StartByte  int    `json:"start_byte"`
		EndByte    int    `json:"end_byte"`
		ChunkIndex int    `json:"chunk_index"`
	}
	chunks := make([]ChunkPayload, len(previewResult.Chunks))
	for i, c := range previewResult.Chunks {
		chunks[i] = ChunkPayload(c)
	}

	previewPayload, err := e.marshal(map[string]any{
		"entry_id":       entry.EntryID,
		"content_type":   entry.ContentType,
		"total_tokens":   previewResult.TotalTokens,
		"preview_tokens": previewResult.PreviewTokens,
		"chunks":         chunks,
		"guide":          "Preview shows 5 randomly-selected chunks (15-25% of total content). Chunks are boundary-aligned: code chunks break on function boundaries, prose on paragraphs. This preview is free — no scrip charged. To purchase the full content, send settle(buyer-accept). To decline, send settle(buyer-reject) — no charge. Scrip is reserved at accept, not at preview.",
	})
	if err != nil {
		return fmt.Errorf("engine: preview-request: marshal preview payload: %w", err)
	}

	tags := []string{
		TagSettle,
		TagPhasePrefix + SettlePhaseStrPreview,
	}
	antecedents := []string{msg.ID}

	_, err = e.sendOperatorMessage(previewPayload, tags, antecedents)
	if err != nil {
		return fmt.Errorf("engine: preview-request: send preview response: %w", err)
	}

	e.opts.log("engine: preview-request: sent preview for entry=%s match=%s buyer=%s",
		shortKey(entry.EntryID), shortKey(matchMsgID), shortKey(msg.Sender))
	return nil
}

// handleSettleDeliverContent processes a settle(deliver) message from the operator.
//
// When the operator sends a settle(deliver) trigger (without a content field),
// the engine emits a new settle(deliver) message to the campfire with the full
// content from the inventory entry. The buyer can identify this message by the
// phase tag and the antecedent chain (operator's deliver → buyer-accept → match).
//
// If the incoming message already carries a content field, it is the engine's own
// previously emitted content message — skip to avoid an infinite dispatch loop.
//
// Security: operator gating is enforced at the state layer (applySettleDeliver
// rejects non-operator senders before populating deliverToMatch). The engine only
// emits content when the deliver message is tracked in state (deliverToMatch is
// populated), which guarantees the sender was the operator.
func (e *Engine) handleSettleDeliverContent(msg *Message) error {
	// Skip if this message already carries content or a blob pointer — it is
	// the engine's own emitted response and must not be re-processed. Both
	// fields are checked because the two delivery shapes (inline vs. pointer,
	// see emitDeliverContent) populate different fields.
	var incoming struct {
		Content     string `json:"content"`
		BlobPointer string `json:"blob_pointer"`
	}
	if err := json.Unmarshal(msg.Payload, &incoming); err == nil && (incoming.Content != "" || incoming.BlobPointer != "") {
		return nil
	}

	// Look up the entry via the antecedent chain: deliver → match → entry.
	entry, ok := e.state.EntryForDeliver(msg.ID)
	if !ok {
		e.opts.log("engine: settle-deliver: cannot derive entry for deliver=%s — antecedent chain missing or non-operator sender", shortKey(msg.ID))
		return nil
	}

	// Derive buyer key from the antecedent chain: deliver → match → matchToBuyer.
	matchMsgID, ok := e.state.MatchForDeliver(msg.ID)
	if !ok {
		e.opts.log("engine: settle-deliver: cannot derive match for deliver=%s", shortKey(msg.ID))
		return nil
	}
	buyerKey := e.state.MatchBuyerKey(matchMsgID)
	if buyerKey == "" {
		e.opts.log("engine: settle-deliver: no buyer key for match=%s", shortKey(matchMsgID))
		return nil
	}

	// ed2-D §3.7 (Layer-0 money integrity): on a scrip-enabled exchange, content
	// delivery MUST be gated on a LIVE scrip reservation for this match. The
	// buyer-accept hold (decAndSaveHold) and this deliver are SEPARATE dispatch
	// handlers — an underfunded buyer's buyer-accept fails the hold and saves NO
	// reservation, yet buyerAcceptToMatch is folded unconditionally
	// (state_settle.go applySettleBuyerAccept), so the antecedent chain above
	// still resolves. Without this guard the operator would emit the full content
	// FREE. No live reservation ⇒ do NOT deliver. Guarded by ScripStore != nil so
	// the individual tier (ScripStore == nil, no reservation concept, deliver runs
	// unconditionally) is byte-for-byte unchanged.
	if e.opts.ScripStore != nil {
		if resID, ok := e.reservationFor(matchMsgID); !ok || resID == "" {
			e.opts.log("engine: settle-deliver: REFUSING deliver for match=%s — no live scrip reservation (buyer-accept hold failed or buyer was never funded); content withheld (ed2-D §3.7)",
				shortKey(matchMsgID))
			return nil
		}
	}

	return e.emitDeliverContent(msg, entry, buyerKey)
}

// emitDeliverContent builds and sends the settle(deliver) message for entry.
//
// Two mutually exclusive shapes, chosen by entry.BlobPointer:
//
//   - Offloaded entry (dontguess-7783, BlobPointer set): the full content
//     lives only in the Blossom blob store — entry.Content holds just the
//     inline preview slice, never the full bytes. Per the shipped design
//     (docs/design/nostr-first-rebuild-decision.md L114/L183 — "full deliver
//     is a Blossom pointer + client-side hash verification"), this emits the
//     BlobPointer and entry.ContentHash, NOT the bytes. The buyer fetches the
//     blob directly from Blossom and verifies its sha256 against content_hash
//     themselves before trusting it. The operator never fetches or inlines
//     the oversize content at deliver time (dontguess-05d2: the previous
//     implementation fetched-and-verified server-side, then still inlined
//     the full bytes into the outgoing message, defeating the offload).
//   - Legacy/small entry (no BlobPointer): entry.Content already holds the
//     full bytes and is delivered inline, unchanged from before.
//
// Size guard: regardless of why an entry lacks a BlobPointer, content larger
// than BlossomOffloadThreshold is never inlined into the outgoing message —
// a hard boundary enforced at delivery time, independent of whatever put-time
// policy produced the entry.
func (e *Engine) emitDeliverContent(msg *Message, entry *InventoryEntry, buyerKey string) error {
	if entry.BlobPointer != "" {
		return e.emitDeliverPointer(msg, entry, buyerKey)
	}

	content := entry.Content
	if len(content) == 0 {
		e.opts.log("engine: settle-deliver: entry=%s has no content — cannot emit deliver", shortKey(entry.EntryID))
		return nil
	}
	if len(content) > BlossomOffloadThreshold {
		e.opts.log("engine: settle-deliver: entry=%s content size %d exceeds BlossomOffloadThreshold %d but has no BlobPointer — refusing to inline",
			shortKey(entry.EntryID), len(content), BlossomOffloadThreshold)
		return nil
	}

	rawHash := sha256.Sum256(content)
	contentHash := "sha256:" + hex.EncodeToString(rawHash[:])

	deliverContentPayload, err := e.marshal(map[string]any{
		"phase":        SettlePhaseStrDeliver,
		"entry_id":     entry.EntryID,
		"content":      base64.StdEncoding.EncodeToString(content),
		"content_hash": contentHash,
		"buyer":        buyerKey,
		"guide":        "Content delivered. Verify integrity: SHA-256 hash the decoded content and compare to content_hash. To confirm receipt, send settle(complete) with the content_hash. A compression task may be posted for you — completing it earns 30% of token_cost in scrip (you have the content cached, making you the ideal compressor).",
	})
	if err != nil {
		return fmt.Errorf("engine: settle-deliver: marshal content payload for entry=%s: %w", shortKey(entry.EntryID), err)
	}

	return e.sendDeliverMessage(msg, entry, buyerKey, deliverContentPayload, contentHash)
}

// emitDeliverPointer emits the pointer-shaped settle(deliver) message for a
// Blossom-offloaded entry: a BlobPointer + content_hash, never the bytes. The
// buyer is responsible for fetching entry.BlobPointer from the Blossom blob
// store and verifying the fetched bytes' sha256 against content_hash before
// trusting the content. See State.FetchAndVerifyBlob for the equivalent check
// implemented for callers that fetch on the buyer's behalf server-side rather
// than delivering a pointer for the buyer to resolve itself.
func (e *Engine) emitDeliverPointer(msg *Message, entry *InventoryEntry, buyerKey string) error {
	if entry.ContentHash == "" {
		e.opts.log("engine: settle-deliver: entry=%s has BlobPointer but no content_hash — refusing to emit pointer deliver", shortKey(entry.EntryID))
		return nil
	}

	deliverPointerPayload, err := e.marshal(map[string]any{
		"phase":        SettlePhaseStrDeliver,
		"entry_id":     entry.EntryID,
		"blob_pointer": entry.BlobPointer,
		"content_hash": entry.ContentHash,
		"buyer":        buyerKey,
		"guide":        "Content is stored off-relay in Blossom (too large to inline). Fetch it via blob_pointer, then verify integrity YOURSELF before trusting it: SHA-256 hash the fetched bytes and compare to content_hash. A mismatch means the blob host served tampered or corrupted bytes — discard it and dispute rather than send settle(complete). To confirm receipt after a successful verify, send settle(complete) with the content_hash. A compression task may be posted for you — completing it earns 30% of token_cost in scrip (you have the content cached, making you the ideal compressor).",
	})
	if err != nil {
		return fmt.Errorf("engine: settle-deliver: marshal pointer payload for entry=%s: %w", shortKey(entry.EntryID), err)
	}

	return e.sendDeliverMessage(msg, entry, buyerKey, deliverPointerPayload, entry.ContentHash)
}

// sendDeliverMessage emits the settle(deliver) convention message shared by
// both the inline-content and pointer delivery shapes, antecedent to the
// operator's deliver trigger (msg).
func (e *Engine) sendDeliverMessage(msg *Message, entry *InventoryEntry, buyerKey string, payload []byte, contentHash string) error {
	tags := []string{
		TagSettle,
		TagPhasePrefix + SettlePhaseStrDeliver,
	}
	antecedents := []string{msg.ID}

	_, err := e.sendOperatorMessage(payload, tags, antecedents)
	if err != nil {
		return fmt.Errorf("engine: settle-deliver: send content for entry=%s: %w", shortKey(entry.EntryID), err)
	}

	e.opts.log("engine: settle-deliver: emitted content for entry=%s buyer=%s content_hash=%s",
		shortKey(entry.EntryID), shortKey(buyerKey), contentHash[:24])
	return nil
}

// handleSettleSmallContentDispute processes a settle(small-content-dispute) message.
//
// This is a fully automated refund path — no operator verdict required. When
// content is below SmallContentThreshold tokens, previews are not meaningful,
// so buyers receive an immediate auto-refund of their held scrip.
//
// If ScripStore is configured, the buyer's reservation is consumed and the
// full held amount is returned to their balance. State tracking (reputation
// penalty) is handled by applySettleSmallContentDispute in state.go.
func (e *Engine) handleSettleSmallContentDispute(msg *Message) error {
	if e.opts.ScripStore == nil {
		return nil
	}

	var payload struct {
		ReservationID string `json:"reservation_id"`
		BuyerKey      string `json:"buyer_key"`
	}
	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		return fmt.Errorf("scrip: parsing small-content-dispute payload: %w", err)
	}
	if payload.ReservationID == "" || payload.BuyerKey == "" {
		return nil // no scrip involved — state-only tracking already done
	}

	// Verify the entry is actually small content. Derive entry from antecedent chain.
	// The small-content restriction is MANDATORY (dontguess-471): if the entry
	// cannot be derived from the antecedent chain we CANNOT prove the content is
	// below SmallContentThreshold, so we must refuse the auto-refund rather than
	// skip the restriction. The previous `if entry != nil { ... }` shape let a
	// dispute against an unresolvable (or non-small) entry force a refund on
	// normal-sized paid content — bypassing the whole reason this path exists.
	if len(msg.Antecedents) == 0 {
		return nil
	}
	deliverMsgID := msg.Antecedents[0]
	entry := e.entryForDeliver(deliverMsgID)
	if entry == nil {
		e.opts.log("engine: small-content-dispute: cannot derive entry for deliver=%s — refusing auto-refund (small-content restriction is mandatory)",
			shortKey(deliverMsgID))
		return nil
	}
	isSmall := entry.TokenCost < SmallContentThreshold ||
		entry.ContentSize < int64(SmallContentThreshold)*4
	if !isSmall {
		e.opts.log("engine: small-content-dispute: entry %s is not small content (token_cost=%d, content_size=%d) — rejecting refund",
			shortKey(entry.EntryID), entry.TokenCost, entry.ContentSize)
		return nil
	}

	ctx := e.engineCtx()

	// Atomically retrieve and delete reservation (prevents TOCTOU double-spend).
	res, err := e.opts.ScripStore.ConsumeReservation(ctx, payload.ReservationID)
	if err != nil {
		e.opts.log("engine: small-content-dispute: reservation %s not found: %v",
			shortKey(payload.ReservationID), err)
		return nil // reservation missing or already settled
	}

	// Security: the refund may be triggered ONLY by the reservation's own owner —
	// the buyer whose scrip is held. reservation_id is PUBLIC (it is emitted in the
	// scrip-buy-hold event on the relay log), so any allowlisted member can read a
	// victim's reservation_id and craft a small-content-dispute for it. Binding
	// identity to the attacker-controlled payload alone (the pre-dontguess-471
	// check, res.AgentKey != payload.BuyerKey, which an attacker satisfies by
	// setting buyer_key = the victim's own key) let such a member force-consume +
	// refund a victim's live reservation: griefing + ledger corruption (the
	// victim's in-flight purchase is destroyed without consent; a later
	// settle(complete) finds no reservation). Bind to msg.Sender — the signed,
	// unforgeable message author. Restore the reservation on mismatch so the
	// legitimate buyer's hold is not lost.
	if msg.Sender != res.AgentKey {
		if restoreErr := e.opts.ScripStore.SaveReservation(ctx, res); restoreErr != nil {
			e.opts.log("engine: small-content-dispute: CRITICAL: failed to restore reservation %s after sender/owner mismatch: %v",
				shortKey(payload.ReservationID), restoreErr)
			return fmt.Errorf("scrip: small-content-dispute reservation %s: sender is not reservation owner AND restore failed (reservation lost): %w",
				shortKey(payload.ReservationID), restoreErr)
		}
		return fmt.Errorf("scrip: small-content-dispute reservation %s: sender %s is not reservation owner %s (unauthorized refund attempt)",
			shortKey(payload.ReservationID), shortKey(msg.Sender), shortKey(res.AgentKey))
	}

	// Marshal the convention refund message BEFORE mutating scrip state.
	refundPayload, err := e.marshal(scrip.DisputeRefundPayload{
		Buyer:         res.AgentKey,
		Amount:        res.Amount,
		ReservationID: payload.ReservationID,
		DisputeMsg:    msg.ID,
	})
	if err != nil {
		if restoreErr := e.opts.ScripStore.SaveReservation(ctx, res); restoreErr != nil {
			e.opts.log("engine: small-content-dispute: CRITICAL: failed to restore reservation %s after marshal failure: %v",
				shortKey(payload.ReservationID), restoreErr)
			return fmt.Errorf("scrip: small-content-dispute reservation %s: marshal failed AND restore failed (reservation lost): %w",
				shortKey(payload.ReservationID), restoreErr)
		}
		return fmt.Errorf("scrip: marshal small-content-dispute refund payload: %w", err)
	}

	// Refund the full held amount to the buyer.
	if _, _, err := e.opts.ScripStore.AddBudget(ctx, res.AgentKey, scrip.BalanceKey, res.Amount, ""); err != nil {
		return fmt.Errorf("scrip: small-content-dispute refund for buyer %s: %w", shortKey(res.AgentKey), err)
	}

	// Emit scrip-dispute-refund convention message so CampfireScripStore can replay it.
	if _, emitErr := e.sendOperatorMessage(refundPayload,
		[]string{scrip.TagScripDisputeRefund}, []string{msg.ID}); emitErr != nil {
		e.opts.log("engine: warning: emit scrip-dispute-refund (small-content): %v", emitErr)
	}

	e.opts.log("engine: small-content-dispute refund: reservation=%s buyer=%s amount=%d",
		shortKey(payload.ReservationID), shortKey(res.AgentKey), res.Amount)
	return nil
}

// handleDeadlineMissRefund issues an automatic full refund (match_price + premium)
// to the buyer when a settle(complete) arrives after the guarantee_deadline. The
// exchange absorbs the loss — the worker is not penalised, and normal payment for
// the worker was already handled separately via assign-pay.
//
// The refund amount is insuredAmount from the buy order. If insuredAmount is zero,
// the full reservation amount is refunded instead (defensive fallback).
//
// Does NOT consume the reservation — the caller is responsible for NOT calling
// ConsumeReservation before calling this method. This method consumes it internally
// so that the refund path is atomic (consume → refund).
func (e *Engine) handleDeadlineMissRefund(ctx context.Context, msg *Message, matchMsgID, reservationID string, insuredAmount int64) error {
	res, err := e.opts.ScripStore.ConsumeReservation(ctx, reservationID)
	if err != nil {
		return fmt.Errorf("scrip: deadline-miss: consume reservation %s: %w", shortKey(reservationID), err)
	}

	refundAmount := insuredAmount
	if refundAmount <= 0 {
		refundAmount = res.Amount
	}

	refundPayload, marshalErr := e.marshal(scrip.DisputeRefundPayload{
		Buyer:         res.AgentKey,
		Amount:        refundAmount,
		ReservationID: reservationID,
		DisputeMsg:    msg.ID,
	})
	if marshalErr != nil {
		// Restore reservation so the settle can be retried.
		if restoreErr := e.opts.ScripStore.SaveReservation(ctx, res); restoreErr != nil {
			e.opts.log("engine: deadline-miss: CRITICAL: failed to restore reservation %s after marshal failure: %v",
				shortKey(reservationID), restoreErr)
			return fmt.Errorf("scrip: deadline-miss reservation %s: marshal failed AND restore failed: %w",
				shortKey(reservationID), marshalErr)
		}
		return fmt.Errorf("scrip: deadline-miss: marshal refund payload: %w", marshalErr)
	}

	// Emit scrip-dispute-refund convention message BEFORE crediting the buyer so
	// that Replay is consistent: if the emit fails the reservation was already
	// consumed and must be restored, but the balance has not been modified yet.
	if _, emitErr := e.sendOperatorMessage(refundPayload,
		[]string{scrip.TagScripDisputeRefund}, []string{msg.ID}); emitErr != nil {
		// Restore reservation so the settle can be retried.
		if restoreErr := e.opts.ScripStore.SaveReservation(ctx, res); restoreErr != nil {
			e.opts.log("engine: deadline-miss: CRITICAL: failed to restore reservation %s after emit failure: %v",
				shortKey(reservationID), restoreErr)
			return fmt.Errorf("scrip: deadline-miss reservation %s: emit failed AND restore failed: %w",
				shortKey(reservationID), emitErr)
		}
		return fmt.Errorf("scrip: deadline-miss: emit scrip-dispute-refund: %w", emitErr)
	}

	// Credit the buyer's balance.
	if _, _, err := e.opts.ScripStore.AddBudget(ctx, res.AgentKey, scrip.BalanceKey, refundAmount, ""); err != nil {
		// Restore reservation so the settle can be retried.
		if restoreErr := e.opts.ScripStore.SaveReservation(ctx, res); restoreErr != nil {
			e.opts.log("engine: deadline-miss: CRITICAL: failed to restore reservation %s after AddBudget failure: %v",
				shortKey(reservationID), restoreErr)
		}
		return fmt.Errorf("scrip: deadline-miss: AddBudget(buyer %s): %w", shortKey(res.AgentKey), err)
	}

	// Clear the guarantee record so a duplicate settle(complete) cannot re-enter
	// the refund path (double-spend prevention).
	e.state.ClearMatchGuarantee(matchMsgID)

	e.opts.log("engine: deadline-miss refund: match=%s reservation=%s buyer=%s amount=%d",
		shortKey(matchMsgID), shortKey(reservationID), shortKey(res.AgentKey), refundAmount)
	return nil
}
