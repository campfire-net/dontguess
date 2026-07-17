package exchange

import (
	"encoding/json"
	"time"
)

// foldTime returns the operator/author-authored message timestamp as a UTC time.
//
// Per the operator ruling of 2026-07-08 (option B, event-sourced-pure), the
// assign fold MUST have no clock: every time value it records is derived from
// this persisted, replay-stable message Timestamp and NEVER from time.Now().
// The wall clock is non-deterministic on replay and manipulable by whichever
// node's clock observes a message first — reading it inside Apply would make the
// folded State depend on when (and where) the log is replayed. A message with no
// Timestamp (test/synthetic only) yields the zero time, which is still fully
// deterministic. Deadline ENFORCEMENT does not live in the fold — it is driven
// exclusively by the off-fold operator sweep emitting operator-authored
// exchange:assign-expire events (engine sweepExpiredClaims /
// sweepStalePredictionAssigns), which the fold then applies deterministically.
func foldTime(msg *Message) time.Time {
	if msg.Timestamp > 0 {
		return time.Unix(0, msg.Timestamp).UTC()
	}
	return time.Time{}
}

// applyAssign processes an exchange:assign message.
//
// Only the operator may post assign tasks. Non-operator senders are rejected
// when OperatorKey is set.
//
// Payload fields:
//   - entry_id: optional inventory entry this task concerns
//   - task_type: category ("freshness", "validation", "compression", ...)
//   - reward: scrip to pay the claimant on accept
//   - exclusive_sender: if set, only this key may claim the task
//
// On success, a new AssignRecord is created in AssignOpen state and indexed
// in assignsByEntry and assignByID.
func (s *State) applyAssign(msg *Message) {
	if s.OperatorKey != "" && msg.Sender != s.OperatorKey {
		s.recordFoldDenial(foldDenialNotOperator, msg)
		return
	}
	// Idempotency guard: skip re-application on replay.
	if _, exists := s.assignByID[msg.ID]; exists {
		return
	}
	var payload struct {
		EntryID              string `json:"entry_id"`
		TaskType             string `json:"task_type"`
		Reward               int64  `json:"reward"`
		Bounty               int64  `json:"bounty"` // convention-dispatched assigns use "bounty"; engine-generated use "reward"
		ExclusiveSender      string `json:"exclusive_sender"`
		ClaimTimeoutMinutes  int    `json:"claim_timeout_minutes"`
		AuctionWindowSeconds int    `json:"auction_window_seconds"`
		BuyMsgID             string `json:"buy_msg_id"`
		DeadlineAt           string `json:"deadline_at"`
	}
	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		return
	}
	// Convention-dispatched assigns use "bounty" (from assign.json spec);
	// engine-generated assigns use "reward". Merge: bounty takes precedence.
	if payload.Bounty > 0 && payload.Reward == 0 {
		payload.Reward = payload.Bounty
	}
	timeoutMinutes := payload.ClaimTimeoutMinutes
	if timeoutMinutes <= 0 || timeoutMinutes > 30 {
		timeoutMinutes = DefaultClaimTimeoutMinutes
	}
	// Receive time is the operator-authored assign message Timestamp — the fold
	// has no clock (see foldTime).
	receivedAt := foldTime(msg)
	rec := &AssignRecord{
		AssignID:            msg.ID,
		EntryID:             payload.EntryID,
		TaskType:            payload.TaskType,
		Reward:              payload.Reward,
		Status:              AssignOpen,
		ExclusiveSender:     payload.ExclusiveSender,
		ClaimTimeoutMinutes: timeoutMinutes,
		AssignReceivedAt:    receivedAt,
		BuyMsgID:            payload.BuyMsgID,
	}
	// Parse deadline_at for standing assigns (e.g. prediction-derived brokered-match).
	if payload.DeadlineAt != "" {
		if t, err := time.Parse(time.RFC3339, payload.DeadlineAt); err == nil {
			rec.DeadlineAt = t.UTC()
		}
	}
	// If the operator requested an auction window, set the deadline.
	if payload.AuctionWindowSeconds > 0 {
		rec.AuctionDeadline = receivedAt.Add(time.Duration(payload.AuctionWindowSeconds) * time.Second)
	}
	s.assignsByEntry[payload.EntryID] = append(s.assignsByEntry[payload.EntryID], rec)
	s.assignByID[msg.ID] = rec
	// For brokered-match tasks, index the buy → assign correlation.
	if payload.TaskType == "brokered-match" && payload.BuyMsgID != "" {
		s.brokerAssigns[payload.BuyMsgID] = msg.ID
	}
}

// applyAssignClaim processes an exchange:assign-claim message.
//
// Antecedent: the assign message ID.
//
// Validation:
//   - Antecedent must reference a known assign in AssignOpen state.
//   - If ExclusiveSender is set on the assign, the sender must match.
//   - An agent may hold only one active claim at a time.
//
// On success, the assign transitions to AssignClaimed and claimedAssigns
// records the agentKey → assignID binding.
func (s *State) applyAssignClaim(msg *Message) {
	if len(msg.Antecedents) == 0 {
		return
	}
	// resolveAlias (dontguess-55c GAP 1, dontguess-d26): a team-tier agent only
	// ever learns the assign's WIRE (content-hash) id from the relay — the
	// Outbox re-signs every operator-emitted record on publish — but
	// assignByID is keyed by the pre-signature STORE id. Without this, every
	// relay-originated assign-claim silently no-ops here forever (the exact
	// gap state_settle.go's resolveAlias calls already close for match/
	// deliver/buyer-accept antecedents). Identity (no-op) wherever no alias is
	// registered — individual tier and the in-process test suites are
	// byte-for-byte unchanged.
	assignID := s.resolveAlias(msg.Antecedents[0])
	rec, ok := s.assignByID[assignID]
	if !ok || rec.Status != AssignOpen {
		return
	}
	// Exclusive sender constraint: if set, only the designated key may claim. A
	// claim by a non-designated sender is a security-relevant identity drop —
	// counted + alarmed rather than dropped silently (dontguess-471 LOCKED-5).
	if rec.ExclusiveSender != "" && msg.Sender != rec.ExclusiveSender {
		s.recordFoldDenial(foldDenialAssignExclusive, msg)
		return
	}
	// DeadlineAt constraint: standing assigns past their deadline are unclaim-able,
	// but that is NOT enforced here with a wall clock (event-sourced-pure ruling
	// 2026-07-08). Enforcement is off-fold: the operator sweep
	// (engine sweepStalePredictionAssigns) detects a passed DeadlineAt and emits an
	// operator-authored exchange:assign-expire whose fold transition (AssignOpen →
	// AssignExpired) makes this record fail the `Status != AssignOpen` guard above.
	// The fold never reads the clock, so a claim's success is identical on replay.

	// Parse the claim payload (supports both expires_at and bid fields).
	var claimPayload struct {
		ExpiresAt string `json:"expires_at"`
		Bid       *int64 `json:"bid"`
	}
	if len(msg.Payload) > 0 {
		_ = json.Unmarshal(msg.Payload, &claimPayload) // best-effort; ignore parse errors
	}

	// Auction path: if this assign has an auction window, collect bids instead
	// of claiming immediately.
	if !rec.AuctionDeadline.IsZero() {
		msgTime := foldTime(msg)
		if !msgTime.Before(rec.AuctionDeadline) {
			// Auction window closed; stray claim messages are ignored.
			// Finalization is handled by applyAssignAuctionClose.
			return
		}
		// Determine bid amount: use the supplied bid, or fall back to base_reward.
		bidAmount := rec.Reward
		if claimPayload.Bid != nil {
			bidAmount = *claimPayload.Bid
		}
		// Safety ceiling: reject bids exceeding AuctionBidCeilMultiplier * base_reward.
		if rec.Reward > 0 && bidAmount > rec.Reward*AuctionBidCeilMultiplier {
			return
		}
		if bidAmount <= 0 {
			return
		}
		// Deduplicate: one bid per worker — keep the lower of old and new bid.
		for i, existing := range rec.AuctionBids {
			if existing.WorkerKey == msg.Sender {
				if bidAmount < existing.BidAmount {
					rec.AuctionBids[i].BidAmount = bidAmount
					rec.AuctionBids[i].Timestamp = msgTime
				}
				return
			}
		}
		rec.AuctionBids = append(rec.AuctionBids, AuctionBid{
			WorkerKey: msg.Sender,
			BidAmount: bidAmount,
			Timestamp: msgTime,
		})
		return // auction window open — do not claim yet
	}

	// Non-auction path: standard single-claim logic.
	// Agent may hold only one active claim at a time.
	if existing, held := s.claimedAssigns[msg.Sender]; held && existing != "" {
		return
	}
	// Compute claim expiry: ceiling is assign_receive_time + claim_timeout_minutes.
	// If the claimant supplies expires_at, honour it only if within the ceiling.
	ceiling := rec.AssignReceivedAt.Add(time.Duration(rec.ClaimTimeoutMinutes) * time.Minute)
	expiresAt := ceiling // default: ceiling
	if claimPayload.ExpiresAt != "" {
		if parsed, err := time.Parse(time.RFC3339, claimPayload.ExpiresAt); err == nil {
			parsed = parsed.UTC()
			if parsed.Before(ceiling) {
				expiresAt = parsed
			}
		}
	}
	claimTime := foldTime(msg)
	rec.Status = AssignClaimed
	rec.ClaimantKey = msg.Sender
	rec.ClaimMsgID = msg.ID
	rec.ClaimExpiresAt = expiresAt
	rec.ClaimedAt = claimTime
	s.claimedAssigns[msg.Sender] = assignID
	s.claimMsgToAssign[msg.ID] = assignID
}

// applyAssignAuctionClose finalizes a Vickrey auction for the assign referenced
// by the message antecedent. It selects the lowest bidder, computes the Vickrey
// clearing price (second-lowest bid), sets DifficultyTier from the bid
// distribution, and transitions the assign to AssignClaimed.
//
// Antecedent: the assign message ID (the auction being closed).
//
// This message is emitted by the engine after AuctionDeadline has passed.
//
// Only the operator may finalize an auction. A non-operator assign-auction-close
// is a NO-OP (no winner selected, no clearing price set), matching the guard on
// every other operator-only handler in this file (applyAssign/Accept/Reject/
// Expire). This per-handler guard lives in the fold, so it is dispatch-ordering
// independent — it is the authoritative authorship boundary for the Vickrey
// finalization (docs/design/relay-transport.md §2.4a D3).
func (s *State) applyAssignAuctionClose(msg *Message) {
	if s.OperatorKey != "" && msg.Sender != s.OperatorKey {
		s.recordFoldDenial(foldDenialNotOperator, msg)
		return
	}
	if len(msg.Antecedents) == 0 {
		return
	}
	// resolveAlias (dontguess-55c GAP 1): see applyAssignClaim's doc — identity
	// no-op wherever no wire alias is registered.
	assignID := s.resolveAlias(msg.Antecedents[0])
	rec, ok := s.assignByID[assignID]
	if !ok || rec.Status != AssignOpen {
		return
	}
	if len(rec.AuctionBids) == 0 {
		// No bids — auction closed with no winner. Task remains open.
		return
	}

	// Sort bids ascending by amount (lowest first) using insertion sort.
	bids := make([]AuctionBid, len(rec.AuctionBids))
	copy(bids, rec.AuctionBids)
	for i := 1; i < len(bids); i++ {
		for j := i; j > 0 && bids[j].BidAmount < bids[j-1].BidAmount; j-- {
			bids[j], bids[j-1] = bids[j-1], bids[j]
		}
	}

	winner := bids[0]
	// Vickrey clearing price: second-lowest bid; if only one bidder, winner's bid.
	clearingPrice := winner.BidAmount
	if len(bids) >= 2 {
		clearingPrice = bids[1].BidAmount
	}

	// Derive difficulty tier from median bid vs base_reward ratio.
	medianBid := bids[len(bids)/2].BidAmount
	var tier string
	if rec.Reward > 0 {
		ratio := float64(medianBid) / float64(rec.Reward)
		switch {
		case ratio <= 1.5:
			tier = "low"
		case ratio <= 5.0:
			tier = "medium"
		default:
			tier = "high"
		}
	} else {
		tier = "low"
	}

	// Transition to AssignClaimed with the Vickrey winner.
	auctionCloseTime := foldTime(msg)
	ceiling := rec.AssignReceivedAt.Add(time.Duration(rec.ClaimTimeoutMinutes) * time.Minute)
	rec.Status = AssignClaimed
	rec.ClaimantKey = winner.WorkerKey
	rec.ClaimMsgID = msg.ID // auction-close message acts as the canonical claim record
	rec.ClaimExpiresAt = ceiling
	rec.VickreyPrice = clearingPrice
	rec.DifficultyTier = tier
	rec.ClaimedAt = auctionCloseTime
	s.claimedAssigns[winner.WorkerKey] = assignID
	s.claimMsgToAssign[msg.ID] = assignID
}

// applyAssignComplete processes an exchange:assign-complete message.
//
// Antecedent: the assign-claim message ID.
//
// Validation:
//   - Antecedent must reference a known assign-claim for an assign in AssignClaimed state.
//   - Sender must be the agent who claimed the task.
//
// On success, the assign transitions to AssignCompleted, the result is stored,
// the claimedAssigns binding is released, and the record is indexed in
// pendingAssignResults for the operator to accept or reject.
func (s *State) applyAssignComplete(msg *Message) {
	if len(msg.Antecedents) == 0 {
		return
	}
	// resolveAlias (dontguess-55c GAP 1): see applyAssignClaim's doc — a
	// team-tier agent's assign-complete e-tags the WIRE id of the assign-claim
	// it is completing (the only id the relay ever exposed for that claim).
	claimMsgID := s.resolveAlias(msg.Antecedents[0])
	// Find the assign record via the O(1) claim-msg index.
	assignID, ok := s.claimMsgToAssign[claimMsgID]
	if !ok {
		return
	}
	rec := s.assignByID[assignID]
	if rec == nil || rec.Status != AssignClaimed {
		return
	}
	// Sender must be the claimant. A completion by a non-claimant is a
	// security-relevant identity drop — counted + alarmed (dontguess-471 LOCKED-5).
	if msg.Sender != rec.ClaimantKey {
		s.recordFoldDenial(foldDenialAssignClaimant, msg)
		return
	}
	// Claim expiry is NOT re-checked here against a wall clock (event-sourced-pure
	// ruling 2026-07-08). A late completion is rejected deterministically instead:
	// the off-fold operator sweep (engine sweepExpiredClaims) emits an
	// operator-authored exchange:assign-expire once ClaimExpiresAt passes, which
	// the fold applies to reopen the assign (AssignClaimed → AssignOpen) and delete
	// the claimMsgToAssign index — so a completion arriving after that either fails
	// the claimMsgToAssign lookup above or the `Status != AssignClaimed` guard.
	completeTime := foldTime(msg)
	rec.Status = AssignCompleted
	rec.CompleteMsgID = msg.ID
	rec.Result = msg.Payload
	rec.CompletedAt = completeTime
	// Release the agent's active claim slot.
	delete(s.claimedAssigns, msg.Sender)
	// Release the claim-msg index (claim is no longer active).
	delete(s.claimMsgToAssign, claimMsgID)
	// Index complete msg for O(1) lookup in ClaimAssignPayment.
	s.completeMsgToAssign[msg.ID] = assignID
	// Index for operator review.
	s.pendingAssignResults[msg.ID] = rec
}

// applyAssignAccept processes an exchange:assign-accept message.
//
// Only the operator may accept results.
// Antecedent: the assign-complete message ID.
//
// Validation:
//   - Sender must be the operator (if OperatorKey set).
//   - Antecedent must reference a known assign in AssignCompleted state via
//     pendingAssignResults.
//
// On success, the assign transitions to AssignAccepted and is removed from
// pendingAssignResults.
func (s *State) applyAssignAccept(msg *Message) {
	if s.OperatorKey != "" && msg.Sender != s.OperatorKey {
		s.recordFoldDenial(foldDenialNotOperator, msg)
		return
	}
	if len(msg.Antecedents) == 0 {
		return
	}
	// resolveAlias (dontguess-55c GAP 1): see applyAssignClaim's doc.
	completeMsgID := s.resolveAlias(msg.Antecedents[0])
	rec, ok := s.pendingAssignResults[completeMsgID]
	if !ok || rec.Status != AssignCompleted {
		return
	}
	rec.Status = AssignAccepted
	delete(s.pendingAssignResults, completeMsgID)
}

// applyAssignReject processes an exchange:assign-reject message.
//
// Only the operator may reject results.
// Antecedent: the assign-complete message ID.
//
// Validation:
//   - Sender must be the operator (if OperatorKey set).
//   - Antecedent must reference a known assign in AssignCompleted state via
//     pendingAssignResults.
//
// On success, the assign is removed from pendingAssignResults and transitions
// back to AssignOpen so a different agent may claim the task.
func (s *State) applyAssignReject(msg *Message) {
	if s.OperatorKey != "" && msg.Sender != s.OperatorKey {
		s.recordFoldDenial(foldDenialNotOperator, msg)
		return
	}
	if len(msg.Antecedents) == 0 {
		return
	}
	// resolveAlias (dontguess-55c GAP 1): see applyAssignClaim's doc.
	completeMsgID := s.resolveAlias(msg.Antecedents[0])
	rec, ok := s.pendingAssignResults[completeMsgID]
	if !ok || rec.Status != AssignCompleted {
		return
	}
	delete(s.pendingAssignResults, completeMsgID)
	// Remove from complete-msg index (assign is no longer in completed state).
	delete(s.completeMsgToAssign, completeMsgID)
	// Reset to open so a different agent may claim the task.
	rec.ClaimantKey = ""
	rec.ClaimMsgID = ""
	rec.CompleteMsgID = ""
	rec.Result = nil
	rec.Status = AssignOpen
}

// applyAssignExpire processes an exchange:assign-expire message.
//
// Only the operator may emit assign-expire messages.
// Antecedent: EITHER the assign-claim message ID of a claimed task that expired,
// OR (deadline path) the assign message ID of an unclaimed standing task whose
// DeadlineAt has passed.
//
// Validation:
//   - Sender must be the operator (if OperatorKey set).
//   - Antecedent must reference EITHER a known assign-claim for an assign in
//     AssignClaimed state (claim-expiry path), OR a known AssignOpen assign
//     (standing-deadline path).
//   - Idempotent: replaying the same expire message is a no-op.
//
// Claim-expiry path: the claimant fields are cleared, the claimedAssigns binding
// is removed, and the record transitions back to AssignOpen so another agent may
// claim it. Standing-deadline path: the unclaimed record transitions to the
// terminal AssignExpired state so it is no longer claimable — this is the
// event-sourced-pure replacement for the removed wall-clock DeadlineAt guard in
// applyAssignClaim (ruling 2026-07-08).
func (s *State) applyAssignExpire(msg *Message) {
	if s.OperatorKey != "" && msg.Sender != s.OperatorKey {
		s.recordFoldDenial(foldDenialNotOperator, msg)
		return
	}
	if len(msg.Antecedents) == 0 {
		return
	}
	// resolveAlias (dontguess-55c GAP 1): see applyAssignClaim's doc. This
	// handler's antecedent is always engine-emitted (sweepExpiredClaims /
	// sweepStalePredictionAssigns), so it is already store-form in every
	// production path today — added for defensive consistency with every
	// other assign handler now that the pattern is established.
	antecedent := s.resolveAlias(msg.Antecedents[0])
	// Claim-expiry path: antecedent is a claim-msg ID indexed to a claimed assign.
	if assignID, ok := s.claimMsgToAssign[antecedent]; ok {
		rec := s.assignByID[assignID]
		if rec == nil || rec.Status != AssignClaimed {
			return // idempotent: already expired/open or unknown
		}
		// Free the agent's claim slot.
		delete(s.claimedAssigns, rec.ClaimantKey)
		// Remove from claim-msg index.
		delete(s.claimMsgToAssign, antecedent)
		// Reset to open so a different agent may claim the task.
		rec.ClaimantKey = ""
		rec.ClaimMsgID = ""
		rec.ClaimExpiresAt = time.Time{}
		rec.Status = AssignOpen
		return
	}
	// Standing-deadline path: antecedent is the assign ID of an unclaimed standing
	// task past its DeadlineAt. Close it terminally so it can no longer be claimed.
	if rec, ok := s.assignByID[antecedent]; ok && rec.Status == AssignOpen {
		rec.Status = AssignExpired
	}
	// Anything else: idempotent no-op (already claimed/completed/expired or unknown).
}

// ExpireStaleClaims checks all AssignClaimed records and returns the claim
// message IDs of those whose ClaimExpiresAt has passed (as of now). It does
// NOT mutate state — the caller (engine) is responsible for emitting
// exchange:assign-expire messages and then calling applyAssignExpire via Apply.
// Caller must hold s.mu (at least read lock, but callers use write lock when mutating).
func (s *State) ExpireStaleClaims() []string {
	now := time.Now().UTC()
	var expired []string
	for _, rec := range s.assignByID {
		if rec.Status == AssignClaimed && !rec.ClaimExpiresAt.IsZero() && now.After(rec.ClaimExpiresAt) {
			expired = append(expired, rec.ClaimMsgID)
		}
	}
	return expired
}

// PendingAuctionClose returns the assign IDs of AssignOpen records whose
// AuctionDeadline has passed and that have at least one bid. These are ready
// for finalization via exchange:assign-auction-close. It does NOT mutate state —
// the caller (engine) is responsible for emitting the close message and then
// calling applyAssignAuctionClose via Apply.
// Caller must hold s.mu (at least read lock).
func (s *State) PendingAuctionClose() []string {
	now := time.Now().UTC()
	var ready []string
	for _, rec := range s.assignByID {
		if rec.Status == AssignOpen &&
			!rec.AuctionDeadline.IsZero() &&
			now.After(rec.AuctionDeadline) &&
			len(rec.AuctionBids) > 0 {
			ready = append(ready, rec.AssignID)
		}
	}
	return ready
}

// ClaimAssignPayment atomically checks that the assign record for the given
// completeMsgID is in AssignAccepted state and transitions it to AssignPaid,
// returning the record. Returns nil if the record is not found or is not in
// AssignAccepted state (e.g. a replayed accept whose bounty was already paid).
//
// This prevents double-payment on replayed exchange:assign-accept messages.
// State.Apply transitions AssignCompleted → AssignAccepted on the first accept
// and removes the record from pendingAssignResults. A replayed accept is a no-op
// in State.Apply but handleAssignAccept would still find the record via assignByID.
// ClaimAssignPayment gates payment to the single transition AssignAccepted → AssignPaid.
// Thread-safe.
func (s *State) ClaimAssignPayment(completeMsgID string) *AssignRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	// resolveAlias (dontguess-55c GAP 1, dontguess-d26): the caller
	// (engine_core.go handleAssignAccept) passes msg.Antecedents[0] straight
	// through — an operator emitting a REAL accept for a team-tier agent's
	// completion only ever knows that completion's WIRE id (the relay never
	// exposes the pre-signature store id). Without this, a genuinely-accepted
	// relay-originated completion could never actually pay: the fold
	// (applyAssignAccept, itself now alias-resolved) transitions the record to
	// AssignAccepted, but this SEPARATE payment gate would never find it via
	// completeMsgToAssign (store-keyed) using the raw wire id.
	completeMsgID = s.resolveAlias(completeMsgID)
	assignID, ok := s.completeMsgToAssign[completeMsgID]
	if !ok {
		return nil
	}
	r := s.assignByID[assignID]
	if r == nil || r.Status != AssignAccepted {
		return nil
	}
	r.Status = AssignPaid
	// Belt-and-suspenders: remove from index in case it wasn't removed at accept.
	delete(s.completeMsgToAssign, completeMsgID)
	return r
}

// ActiveAssigns returns a snapshot of all AssignRecord entries associated with
// the given entryID that are not yet in a terminal state (Accepted or Rejected).
// Pass "" to query assigns with no associated entry.
//
// Expired claims (AssignClaimed with ClaimExpiresAt in the past) are returned
// with their effective status as AssignOpen — the claim slot is logically free
// even if the engine has not yet emitted the assign-expire message. This ensures
// callers always see claimable tasks without waiting for the next poll cycle.
//
// Thread-safe.
func (s *State) ActiveAssigns(entryID string) []*AssignRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	now := time.Now().UTC()
	recs := s.assignsByEntry[entryID]
	out := make([]*AssignRecord, 0, len(recs))
	for _, r := range recs {
		if r.Status == AssignAccepted || r.Status == AssignRejected || r.Status == AssignPaid || r.Status == AssignExpired {
			continue
		}
		cp := *r
		// Lazy expiry: if the claim TTL has elapsed, present effective status as
		// AssignOpen so callers see the task as claimable. State mutation happens
		// when the engine emits and applies exchange:assign-expire.
		if cp.Status == AssignClaimed && !cp.ClaimExpiresAt.IsZero() && now.After(cp.ClaimExpiresAt) {
			cp.Status = AssignOpen
			cp.ClaimantKey = ""
			cp.ClaimMsgID = ""
		}
		out = append(out, &cp)
	}
	return out
}

// AllActiveAssigns returns a snapshot of every non-terminal AssignRecord across
// ALL entries (including entryID=="" brokered/generic assigns) — unlike
// ActiveAssigns, which is scoped to one entryID. Mirrors ActiveAssigns's
// terminal-state exclusion and lazy-claim-expiry presentation (a AssignClaimed
// record whose ClaimExpiresAt has passed is presented as AssignOpen so a caller
// sees it as claimable without waiting for the next assign-expire poll tick).
//
// Added for the CLI agent door (dontguess-d26): `dontguess assigns` needs to
// discover every open/claimable task in one call, not just those tied to a
// single inventory entry.
//
// Thread-safe.
func (s *State) AllActiveAssigns() []*AssignRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	now := time.Now().UTC()
	out := make([]*AssignRecord, 0, len(s.assignByID))
	for _, r := range s.assignByID {
		if r.Status == AssignAccepted || r.Status == AssignRejected || r.Status == AssignPaid || r.Status == AssignExpired {
			continue
		}
		cp := *r
		if cp.Status == AssignClaimed && !cp.ClaimExpiresAt.IsZero() && now.After(cp.ClaimExpiresAt) {
			cp.Status = AssignOpen
			cp.ClaimantKey = ""
			cp.ClaimMsgID = ""
		}
		out = append(out, &cp)
	}
	return out
}

// sellerStats returns the SellerStats for the given key, creating if absent.
// Caller must hold s.mu.
func (s *State) sellerStats(sellerKey string) *SellerStats {
	if stats, ok := s.sellers[sellerKey]; ok {
		return stats
	}
	stats := &SellerStats{
		RepeatBuyerMap: make(map[string]int),
		EntryBuyerMap:  make(map[string]map[string]struct{}),
	}
	s.sellers[sellerKey] = stats
	return stats
}
