package exchange

import (
	"encoding/json"

	"github.com/3dl-dev/dontguess/pkg/scrip"
)

// NewState creates an empty exchange state.
func NewState() *State {
	return &State{
		inventory:              make(map[string]*InventoryEntry),
		pendingPuts:            make(map[string]*InventoryEntry),
		activeOrders:           make(map[string]*ActiveOrder),
		priceHistory:           nil,
		sellers:                make(map[string]*SellerStats),
		matchedOrders:          make(map[string]struct{}),
		putToEntry:             make(map[string]string),
		matchToBuyer:           make(map[string]string),
		matchToEntry:           make(map[string]string),
		matchToResults:         make(map[string][]string),
		acceptedOrders:         make(map[string]string),
		buyerAcceptToMatch:     make(map[string]string),
		deliveredOrders:        make(map[string]struct{}),
		deliverToMatch:         make(map[string]string),
		deliverTimeByMatch:     make(map[string]int64),
		completedEntries:       make(map[string]string),
		completedSettlements:   make(map[string]struct{}),
		previewsByEntry:        make(map[string]map[string]string),
		previewCountByMatch:    make(map[string]int),
		previewRequestToMatch:  make(map[string]string),
		previewToMatch:         make(map[string]string),
		smallContentDisputes:   make(map[string]int),
		entryPreviewCount:      make(map[string]int),
		entryConversionCount:   make(map[string]int),
		entryConsumeCount:      make(map[string]int),
		entryDeliverCount:      make(map[string]int),
		buyerDeliverCount:      make(map[string]int),
		buyerConsumeCount:      make(map[string]int),
		entryDeliverBuyerCount: make(map[string]map[string]int),
		entryConsumeBuyerCount: make(map[string]map[string]int),
		priceAdjustments:       make(map[string]PriceAdjustment),
		wireToStore:            make(map[string]string),
		matchToBuyHold:         make(map[string]string),
		matchToBuyHoldAmount:   make(map[string]int64),
		settledMatches:         make(map[string]struct{}),
		assignsByEntry:         make(map[string][]*AssignRecord),
		assignByID:             make(map[string]*AssignRecord),
		claimedAssigns:         make(map[string]string),
		pendingAssignResults:   make(map[string]*AssignRecord),
		claimMsgToAssign:       make(map[string]string),
		completeMsgToAssign:    make(map[string]string),
		buyMissOffers:          make(map[string]*BuyMissOffer),
		matchToBuyMsgID:        make(map[string]string),
		matchGuarantee:         make(map[string][2]int64),
		brokerAssigns:          make(map[string]string),
		brokerMatchIDs:         make(map[string]struct{}),
		debtorScores:           make(map[string]float64),
		coOccurrence:           make(map[string]*coOccurrenceMap),
		senderHopDepth:         make(map[string][]int),
		federationProfiles:     make(map[string]*FederationNodeProfile),
		heldForReview:          make(map[string]struct{}),
		contentHashIndex:       make(map[string]struct{}),
		foldDenialCounted:      make(map[string]struct{}),
		hopDepthCounted:        make(map[string]struct{}),
		consumeCounted:         make(map[string]struct{}),
		disputeCounted:         make(map[string]struct{}),
	}
}

// Replay builds state from scratch by processing all messages in log order.
// It resets the state before processing. Thread-safe.
func (s *State) Replay(msgs []Message) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Suppress fold-guard denial counting for the duration of the replay: the
	// full log is re-applied on every engine restart / state rebuild, so a
	// forged message already on the log must not re-increment the live alarm
	// counters each time (dontguess-9ed). Only real-time Apply counts.
	s.replaying = true
	defer func() { s.replaying = false }()

	// Pre-scan the log for operator put-accepts (dontguess-00d FIX 1). The §6
	// legacy-plaintext grandfather block in applyPut only fires for a put that
	// was PREVIOUSLY ACCEPTED — a genuine pre-cutover plaintext put has an
	// operator put-accept in the log; a post-cutover plaintext put was
	// fail-closed dropped live and has none. applyPut runs during the fold loop
	// BELOW, but a put-accept always folds AFTER its put (it e-tags the put as
	// its antecedent), so applyPut cannot learn "was accepted?" from live fold
	// order. This pre-scan resolves it: collect the put IDs that any operator
	// put-accept references so applyPut can gate grandfathering on membership.
	// The operator-sender guard mirrors applySettlePutAccept exactly (an empty
	// OperatorKey accepts any sender; a set key requires a match) so a forged
	// non-operator put-accept cannot bait a post-cutover plaintext put into
	// inventory. Cleared on exit — meaningful only for this replay's duration.
	s.replayPutAccepts = make(map[string]struct{}, len(msgs))
	defer func() { s.replayPutAccepts = nil }()
	for i := range msgs {
		m := &msgs[i]
		if exchangeOp(m.Tags) != TagSettle {
			continue
		}
		if settlePhaseFromTags(m.Tags) != SettlePhaseStrPutAccept {
			continue
		}
		// resolveAlias canonicalizes a pre-P3 legacy operator sender to the stable
		// nostr operator key (design §6, ADV-17) so a solo-era put-accept still
		// grandfathers its plaintext put; identity for every non-aliased sender, so
		// the forged-non-operator rejection below is unchanged where no alias exists.
		if s.OperatorKey != "" && s.resolveAlias(m.Sender) != s.OperatorKey {
			continue
		}
		if len(m.Antecedents) == 0 {
			continue
		}
		s.replayPutAccepts[m.Antecedents[0]] = struct{}{}
	}

	// Reset.
	s.inventory = make(map[string]*InventoryEntry)
	s.pendingPuts = make(map[string]*InventoryEntry)
	s.activeOrders = make(map[string]*ActiveOrder)
	s.priceHistory = nil
	s.sellers = make(map[string]*SellerStats)
	s.matchedOrders = make(map[string]struct{})
	s.putToEntry = make(map[string]string)
	s.matchToBuyer = make(map[string]string)
	s.matchToEntry = make(map[string]string)
	s.matchToResults = make(map[string][]string)
	s.acceptedOrders = make(map[string]string)
	s.buyerAcceptToMatch = make(map[string]string)
	s.deliveredOrders = make(map[string]struct{})
	s.deliverToMatch = make(map[string]string)
	s.deliverTimeByMatch = make(map[string]int64)
	s.completedEntries = make(map[string]string)
	s.completedSettlements = make(map[string]struct{})
	s.previewsByEntry = make(map[string]map[string]string)
	s.previewCountByMatch = make(map[string]int)
	s.previewRequestToMatch = make(map[string]string)
	s.previewToMatch = make(map[string]string)
	s.smallContentDisputes = make(map[string]int)
	s.entryPreviewCount = make(map[string]int)
	s.entryConversionCount = make(map[string]int)
	s.entryConsumeCount = make(map[string]int)
	s.entryDeliverCount = make(map[string]int)
	s.buyerDeliverCount = make(map[string]int)
	s.buyerConsumeCount = make(map[string]int)
	s.entryDeliverBuyerCount = make(map[string]map[string]int)
	s.entryConsumeBuyerCount = make(map[string]map[string]int)
	s.matchToBuyHold = make(map[string]string)
	s.matchToBuyHoldAmount = make(map[string]int64)
	s.settledMatches = make(map[string]struct{})
	s.assignsByEntry = make(map[string][]*AssignRecord)
	s.assignByID = make(map[string]*AssignRecord)
	s.claimedAssigns = make(map[string]string)
	s.pendingAssignResults = make(map[string]*AssignRecord)
	s.claimMsgToAssign = make(map[string]string)
	s.completeMsgToAssign = make(map[string]string)
	s.buyMissOffers = make(map[string]*BuyMissOffer)
	s.matchToBuyMsgID = make(map[string]string)
	s.matchGuarantee = make(map[string][2]int64)
	s.brokerAssigns = make(map[string]string)
	s.coOccurrence = make(map[string]*coOccurrenceMap)
	// Note: priceAdjustments and brokerMatchIDs are intentionally NOT reset on
	// Replay. They are externally written (by the fast pricing loop and engine
	// respectively), not derived from the campfire log.
	// wireToStore is likewise NOT reset on Replay (dontguess-55c GAP 1): the
	// wire→store alias is a deterministic function of the operator log + the
	// operator signer, but State has no signer to re-derive it — the Outbox
	// (live) and seedEmittedFromStore (restart) repopulate it. Wiping it here
	// would strand every in-flight wire-id-tagged settle until the next publish.
	s.brokeredAcceptedOrders = 0
	s.brokeredCompletions = 0
	// senderHopDepth is re-derived from the campfire log on Replay.
	// Reset it so the replay loop rebuilds it cleanly from messages.
	s.senderHopDepth = make(map[string][]int)
	// contentHashIndex is rebuilt from the campfire log on Replay.
	// The replay loop re-runs applyPut for every exchange:put message, which
	// repopulates the index from the canonical log.
	s.contentHashIndex = make(map[string]struct{})
	// Fold-accumulator dedup guards (dontguess-f86) are reset so a full rebuild
	// starts fresh and repopulates them in log order as the fold loop below runs.
	s.foldDenialCounted = make(map[string]struct{})
	s.hopDepthCounted = make(map[string]struct{})
	s.consumeCounted = make(map[string]struct{})
	s.disputeCounted = make(map[string]struct{})
	// federationProfiles is NOT reset on Replay. The trust_score values written
	// by the slow loop (via SetFederationTrustScore) are externally managed and
	// must survive engine restarts. The HopDepth and FirstSeenAt fields will be
	// updated as messages replay (via trackSenderHopDepth). New senders will get
	// profiles created during replay; existing profiles keep their trust_scores.

	for i := range msgs {
		s.applyLocked(&msgs[i])
	}
}

// Apply processes a single new message, updating state.
// Thread-safe.
func (s *State) Apply(msg *Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applyLocked(msg)
}

// applyLocked applies a message to state. Caller must hold s.mu.
func (s *State) applyLocked(msg *Message) {
	// P3 operator-identity migration (design §6, ADV-17): a pre-P3 solo home signed
	// its operator records under an opaque local operator key that serve registers
	// as a wire-alias of the stable nostr operator key (RegisterWireAlias). Canonicalize
	// the sender through that alias BEFORE any operator-sender gate or attribution so
	// historical solo operator records fold under State.OperatorKey instead of being
	// dropped by the sender-must-be-operator guards. resolveAlias is the identity for
	// every non-aliased sender — participant keys, the operator's own nostr key, and
	// every sender on a fresh home / the in-process suite where no operator-key alias
	// is registered — so this is byte-for-byte unchanged wherever the migration did
	// not run. (Namespaces never collide: an operator wire-alias key is a pubkey; the
	// message-id aliases in the same map are content-hash event ids.)
	if canon := s.resolveAlias(msg.Sender); canon != msg.Sender {
		c := *msg
		c.Sender = canon
		msg = &c
	}

	// Track provenance hop depth for every message from a known sender.
	// Hop depth is approximated from the Antecedents chain length.
	// This populates senderHopDepth for the slow loop's trust_score computation.
	if msg.Sender != "" {
		s.trackSenderHopDepth(msg)
	}

	op := exchangeOp(msg.Tags)
	switch op {
	case TagPut:
		s.applyPut(msg)
	case TagBuy:
		s.applyBuy(msg)
	case TagMatch:
		s.applyMatch(msg)
	case TagSettle:
		s.applySettle(msg)
	case TagAssign:
		s.applyAssign(msg)
	case TagAssignClaim:
		s.applyAssignClaim(msg)
	case TagAssignComplete:
		s.applyAssignComplete(msg)
	case TagAssignAccept:
		s.applyAssignAccept(msg)
	case TagAssignReject:
		s.applyAssignReject(msg)
	case TagAssignExpire:
		s.applyAssignExpire(msg)
	case TagAssignAuctionClose:
		s.applyAssignAuctionClose(msg)
	default:
		// Canonical ops with no switch case above (scrip ops, consume) dispatch
		// here. CRITICAL (dontguess-5be, docs/design/relay-transport.md §2.4a D2):
		// dispatch on the RESOLVED op, never on a raw msg.Tags scan. exchangeOp
		// already enforced the single-canonical-op invariant — if it returned ""
		// the message is ambiguous/smuggled (two+ distinct canonical ops) or
		// carries no canonical op at all, and MUST be dropped, consistent with
		// the switch path. A raw msg.Tags scan would instead fire a handler off a
		// smuggled tag (e.g. [scrip-buy-hold, assign-auction-close] resolving to
		// "" yet still triggering applyScripBuyHold) — the residual half of the
		// parser-differential that c22/e15/13c closed only for the switch path.
		switch op {
		case scrip.TagScripBuyHold:
			s.applyScripBuyHold(msg)
		case scrip.TagScripSettle:
			s.applyScripSettle(msg)
		case TagConsume:
			s.applyConsume(msg)
		}
	}
}

// recordFoldDenial counts + alarms a security-relevant fold-guard rejection
// (operator-only settlement guard, or the buyer-identity gate) via the callback
// wired by NewEngine. It is a no-op during Replay (so a re-applied log does not
// re-inflate the counters) and when no callback is wired (State built directly
// in tests). Caller must hold s.mu — the callback only touches atomic counters
// and the logger, so holding s.mu across it introduces no lock-ordering hazard.
//
// Per-message-ID dedup guard (dontguess-f86): foldDenialCounted ensures a given
// message's denial is counted at most once even if the SAME message is folded
// twice — e.g. once inside a concurrent rebuildAndDispatchGapLocal's full
// state.Replay and again by a poll-loop foldAndDispatchLocalSnapshot's stale,
// unlocked in-flight Apply loop (see State.foldDenialCounted doc). Previously
// this guard was ONLY s.replaying, which suppresses counting for the entire
// duration of a Replay call but does nothing once Replay returns — so a
// message re-applied via a standalone Apply after Replay finished still
// double-counted its denial reason.
func (s *State) recordFoldDenial(reason foldDenialReason, msg *Message) {
	if s.replaying || s.onFoldDenial == nil {
		return
	}
	if _, seen := s.foldDenialCounted[msg.ID]; seen {
		return
	}
	s.foldDenialCounted[msg.ID] = struct{}{}
	s.onFoldDenial(reason, msg)
}

// applyScripBuyHold indexes a scrip-buy-hold message into matchToBuyHold (and
// records the ORIGINAL held amount in matchToBuyHoldAmount). Enables O(1) lookup
// in GetBuyHoldReservation / GetBuyHoldAmount, replacing the O(n) log scan in
// findExistingBuyerAcceptHold.
func (s *State) applyScripBuyHold(msg *Message) {
	var p scrip.BuyHoldPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		return
	}
	if p.BuyMsg == "" || p.ReservationID == "" {
		return
	}
	s.matchToBuyHold[p.BuyMsg] = p.ReservationID
	// Record the original held amount so restoreExistingHold can restore the
	// EXACT scrip that was decremented at buyer-accept time, rather than
	// recomputing from a possibly-drifted current price (dontguess-471 MED).
	s.matchToBuyHoldAmount[p.BuyMsg] = p.Amount
}

// GetBuyHoldReservation returns the reservation ID for a prior scrip-buy-hold
// message matching the given match message ID, or "" if none exists.
// O(1) — replaces the O(n) log scan in findExistingBuyerAcceptHold.
func (s *State) GetBuyHoldReservation(matchMsgID string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.matchToBuyHold[matchMsgID]
}

// GetBuyHoldAmount returns the ORIGINAL held amount (price + fee) recorded in the
// scrip-buy-hold event for the given match, and whether one exists. Used by
// restoreExistingHold to re-hydrate a reservation with the exact amount held at
// buyer-accept time instead of recomputing from the current dynamic price
// (dontguess-471). Returns (0, false) when no buy-hold has been recorded.
func (s *State) GetBuyHoldAmount(matchMsgID string) (int64, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	amt, ok := s.matchToBuyHoldAmount[matchMsgID]
	return amt, ok
}

// applyScripSettle folds a durable scrip-settle message (dontguess-400 FIX-M1,
// design §1.4/§4). It marks the settled match in the durable settledMatches set
// and RETIRES the match's buy-hold index (matchToBuyHold + matchToBuyHoldAmount)
// so a replayed/re-sent buyer-accept for an already-settled match can neither
// re-hydrate the consumed reservation (restoreExistingHold) nor re-settle
// (performScripSettlement). The match key is SettlePayload.MatchMsg — the match
// msg ID the settlement is for.
//
// Operator-sender guard: scrip-settle is operator-authored egress. A non-operator
// sender is rejected (mirrors applyConsume / the scrip-ledger operator gate) so a
// participant cannot forge a settle marker to grief a live match's settlement.
func (s *State) applyScripSettle(msg *Message) {
	if s.OperatorKey != "" && msg.Sender != s.OperatorKey {
		s.recordFoldDenial(foldDenialNotOperator, msg)
		return
	}
	var p scrip.SettlePayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil || p.MatchMsg == "" {
		return
	}
	s.markMatchSettledLocked(p.MatchMsg)
}

// markMatchSettledLocked records matchMsgID as settled and retires its buy-hold
// index. Caller must hold s.mu. Shared by the durable fold path (applyScripSettle)
// and the live-mark path (MarkMatchSettled).
func (s *State) markMatchSettledLocked(matchMsgID string) {
	if matchMsgID == "" {
		return
	}
	s.settledMatches[matchMsgID] = struct{}{}
	delete(s.matchToBuyHold, matchMsgID)
	delete(s.matchToBuyHoldAmount, matchMsgID)
}

// MarkMatchSettled marks a match settled from the live settlement path
// (performScripSettlement), so the settled-match guard holds WITHIN the current
// session — before the durable scrip-settle folds on the next poll. The same set
// is rebuilt independently on Replay by applyScripSettle. Idempotent.
func (s *State) MarkMatchSettled(matchMsgID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.markMatchSettledLocked(matchMsgID)
}

// IsMatchSettled reports whether a scrip settlement has already been durably
// emitted (or live-marked) for the given match msg ID. Used by the engine to
// gate restoreExistingHold, handleSettleBuyerAcceptScrip and
// performScripSettlement against a double-settle mint (dontguess-400 FIX-M1).
func (s *State) IsMatchSettled(matchMsgID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.settledMatches[matchMsgID]
	return ok
}

// applyConsume processes an exchange:consume message, incrementing the
// per-entry consume counter. The entry_id is read from the payload and must
// be non-empty to count. Called from applyLocked.
//
// Operator-sender guard: consume messages must originate from the operator.
// A non-operator sender is rejected to prevent arbitrary campfire members from
// inflating entryConsumeCount and gaming the behavioral booster — counted +
// alarmed as an operator-forgery drop rather than dropped silently
// (dontguess-471 LOCKED-5).
func (s *State) applyConsume(msg *Message) {
	if s.OperatorKey != "" && msg.Sender != s.OperatorKey {
		s.recordFoldDenial(foldDenialNotOperator, msg)
		return
	}
	// Per-message-ID dedup guard (dontguess-f86, consumeCounted): entryConsumeCount++
	// is a raw counter increment with no natural per-message-ID map to dedup
	// against. Without this guard a concurrent rebuildAndDispatchGapLocal
	// state.Replay racing foldAndDispatchLocalSnapshot's unlocked incremental
	// Apply loop double-counts the consume signal (see State.foldDenialCounted
	// doc for the exact interleave), skewing the M5 consume signal
	// (dontguess-860) the pricing/behavioral-signal layer reads.
	if _, dup := s.consumeCounted[msg.ID]; dup {
		return
	}
	var p struct {
		EntryID string `json:"entry_id"`
	}
	if err := json.Unmarshal(msg.Payload, &p); err != nil || p.EntryID == "" {
		return
	}
	s.consumeCounted[msg.ID] = struct{}{}
	s.entryConsumeCount[p.EntryID]++
}
