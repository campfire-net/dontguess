package exchange

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"
)

// computePriceMinPrice is the floor price returned when an entry has no valid
// base price (TokenCost <= 0 or PutPrice <= 0 with no token cost).
// A floor of 1 prevents zero-price entries from bypassing budget filters and
// from receiving l1Efficiency=1.0 (free-item dominance) in the ranker.
const computePriceMinPrice int64 = 1

// Named constants used in computePrice and rankResults.
const (
	// Base price coefficients.
	operatorMargin    = 1.20 // operator takes 20% on top of PutPrice
	sellerShareFactor = 0.70 // seller receives 70% of TokenCost as proxy price

	// Overflow guards: largest PutPrice/TokenCost that won't overflow int64 when
	// multiplied by the corresponding margin (MaxInt64 / guard ≈ safe threshold).
	operatorMarginOverflowGuard = 120 // PutPrice * 1.20 → guard at MaxInt64/120
	sellerShareOverflowGuard    = 70  // TokenCost * 0.70 → guard at MaxInt64/70

	// Demand multiplier coefficients.
	demandCountCap   = 10   // maximum distinct buyers counted toward demand
	demandStepFactor = 0.10 // +10% per distinct completed buyer

	// Age decay (computePrice): decays linearly from 1.0 to ageDecayFloor over computePriceAgeDays.
	ageDecayFloor       = 0.5              // floor of age decay
	computePriceAgeDays = 60 * 24 * 3600.0 // age window in seconds (60 days)

	// Age decay (rankResults): recency score decays from 1.0 to 0.0 over rankResultsRecencyDays.
	rankResultsRecencyDays = 30 * 24 * 3600.0 // recency window in seconds (30 days)

	// Reputation multiplier: rep=0 -> 0.8x, rep=50 -> 1.0x, rep=100 -> 1.2x.
	repFactorBase  = 0.8 // base reputation multiplier (rep=0 -> 0.8x)
	repFactorRange = 0.4 // reputation multiplier range (rep=100 -> 1.2x = base + range)

	// Content size multiplier: +0.3% per KB, capped at +30%.
	sizeBonusPerKB = 0.003 // +0.3% per KB
	sizeBonusCap   = 0.30  // cap at +30% for sizes >= 100KB

	// Reputation weight in rankResults scoring (recency = 1.0 - scoreRepWeight).
	scoreRepWeight = 0.6

	// Compression tier price multipliers (dontguess-cb5).
	// Hot entries have high cache hit rate and low staleness — price premium reflects
	// their higher value to buyers (fewer tokens wasted on stale or unmatched results).
	tierMultiplierHot  = 1.5 // "hot"  — frequently hit, highly current
	tierMultiplierWarm = 1.2 // "warm" — moderately active
	tierMultiplierCold = 1.0 // "cold" or unset — no premium
)

// computePrice returns the exchange's asking price for an entry.
//
// Base price: PutPrice * 1.2 (20% operator margin) when a put-accept exists,
// otherwise TokenCost * 0.7 (seller's 70% share as a proxy pending acceptance).
//
// Six inventory signals adjust the base price:
//   - Demand count: +10% per distinct completed buyer, capped at +100%.
//   - Age decay: decays from 1.0 to 0.5 linearly over 60 days (PutTimestamp=0 = no decay).
//   - Reputation: rep=0 -> 0.8x, rep=50 -> 1.0x, rep=100 -> 1.2x.
//   - Content size: +0.3% per KB, capped at +30% (>=100KB).
//   - Compression tier: hot=1.5x, warm=1.2x, cold or unset=1.0x (dontguess-cb5).
//   - Density markup (compressed derivatives only): base * (original_size / compressed_size)
//     * DensityMarkupFactor (default 1.2). Higher density = higher per-token price.
//     Total cost is still lower than raw because fewer tokens are delivered.
//     Falls back to base pricing when the original entry is not found.
//
// Invariants:
//   - Returns at least computePriceMinPrice (never 0 or negative).
//   - Guards against int64 overflow for large TokenCost and PutPrice values.
func (e *Engine) computePrice(entry *InventoryEntry) int64 {
	// Step 1: base price
	var base float64
	if entry.PutPrice > 0 {
		if entry.PutPrice > math.MaxInt64/operatorMarginOverflowGuard {
			return math.MaxInt64
		}
		base = float64(entry.PutPrice) * operatorMargin
	} else {
		if entry.TokenCost <= 0 {
			return computePriceMinPrice
		}
		if entry.TokenCost > math.MaxInt64/sellerShareOverflowGuard {
			return math.MaxInt64
		}
		base = float64(entry.TokenCost) * sellerShareFactor
		if base < float64(computePriceMinPrice) {
			base = float64(computePriceMinPrice)
		}
	}

	demandFactor := e.computeDemandFactor(entry.EntryID)
	ageFactor := computeAgeFactor(entry.PutTimestamp)
	repFactor := e.computeRepFactor(entry.SellerKey)
	sizeFactor := computeSizeFactor(entry.ContentSize)
	fastFactor := e.computeFastFactor(entry.EntryID)
	densityFactor := e.computeDensityFactor(entry)
	tierFactor := computeTierFactor(entry.CompressionTier)

	// Compound all multipliers.
	price := base * demandFactor * ageFactor * repFactor * sizeFactor * fastFactor * densityFactor * tierFactor

	// Clamp and round (nearest-integer, not truncate, for stable results).
	rounded := math.Round(price)
	if rounded < float64(computePriceMinPrice) {
		return computePriceMinPrice
	}
	if rounded >= float64(math.MaxInt64) {
		return math.MaxInt64
	}
	return int64(rounded)
}

// computeDemandFactor returns the demand multiplier (+10% per buyer, capped at +100%).
func (e *Engine) computeDemandFactor(entryID string) float64 {
	demandCount := e.state.EntryDemandCount(entryID)
	if demandCount > demandCountCap {
		demandCount = demandCountCap
	}
	return 1.0 + float64(demandCount)*demandStepFactor
}

// computeAgeFactor returns the age decay factor (PutTimestamp=0 means no decay).
func computeAgeFactor(putTimestamp int64) float64 {
	if putTimestamp <= 0 {
		return 1.0
	}
	ageSec := float64(time.Now().UnixNano()-putTimestamp) / 1e9
	decay := ageSec / computePriceAgeDays
	if decay > 1.0 {
		decay = 1.0
	}
	return 1.0 - ageDecayFloor*decay
}

// computeRepFactor returns the reputation multiplier (rep=0->0.8x, rep=50->1.0x, rep=100->1.2x).
func (e *Engine) computeRepFactor(sellerKey string) float64 {
	rep := e.state.SellerReputation(sellerKey)
	return repFactorBase + float64(rep)/100.0*repFactorRange
}

// computeSizeFactor returns the content size multiplier (+0.3% per KB, capped at +30%).
func computeSizeFactor(contentSize int64) float64 {
	if contentSize <= 0 {
		return 1.0
	}
	sizeKB := float64(contentSize) / 1024.0
	sizeBonus := sizeKB * sizeBonusPerKB
	if sizeBonus > sizeBonusCap {
		sizeBonus = sizeBonusCap
	}
	return 1.0 + sizeBonus
}

// computeFastFactor returns the dynamic price adjustment multiplier from the fast pricing loop.
func (e *Engine) computeFastFactor(entryID string) float64 {
	fastAdj := e.state.GetPriceAdjustment(entryID)
	if fastAdj.Multiplier <= 0 {
		return 1.0
	}
	return fastAdj.Multiplier
}

// computeDensityFactor returns the density markup for compressed derivatives.
// Formula: (original_size / compressed_size) * density_markup_factor.
// Falls back to 1.0 when the entry is not a derivative or the original is not found.
func (e *Engine) computeDensityFactor(entry *InventoryEntry) float64 {
	if entry.CompressedFrom == "" || entry.ContentSize <= 0 {
		return 1.0
	}
	orig := e.state.GetInventoryEntry(entry.CompressedFrom)
	if orig == nil || orig.ContentSize <= 0 {
		return 1.0
	}
	ratio := float64(orig.ContentSize) / float64(entry.ContentSize)
	return ratio * e.opts.densityMarkupFactor()
}

// computeTierFactor returns the compression tier multiplier.
func computeTierFactor(tier string) float64 {
	switch tier {
	case "hot":
		return tierMultiplierHot
	case "warm":
		return tierMultiplierWarm
	default:
		return tierMultiplierCold
	}
}

// computeConfidence returns a composite confidence score [0,1].
// For v0.1 uses seller reputation as proxy.
func (e *Engine) computeConfidence(entry *InventoryEntry, _ string) float64 {
	rep := e.state.SellerReputation(entry.SellerKey)
	return float64(rep) / 100.0
}

// AutoAcceptPut sends a settle(put-accept) for a pending put message, accepting
// it into inventory at the given price and expiry. This implements automatic
// acceptance for the engine; a real deployment would add validation first.
//
// The put message must exist in the store. This method does not require the put
// to already be in the engine's in-memory state — it will replay the store to
// pick up new messages first.
//
// This is the public API entrypoint. It acquires opMu and delegates to
// autoAcceptPutLocked. RunAutoAccept (which holds opMu) calls autoAcceptPutLocked
// directly to avoid a self-deadlock.
func (e *Engine) AutoAcceptPut(putMsgID string, price int64, expiresAt time.Time) error {
	e.opMu.Lock()
	defer e.opMu.Unlock()
	return e.autoAcceptPutLocked(putMsgID, price, expiresAt)
}

// autoAcceptPutLocked is the internal implementation of AutoAcceptPut.
// Callers must hold e.opMu before calling. RunAutoAccept calls this directly.
func (e *Engine) autoAcceptPutLocked(putMsgID string, price int64, expiresAt time.Time) error {
	// Refresh state before checking. In local mode this also dispatches any
	// buy appended since the last poll, so a concurrently-arriving buy is
	// matched rather than folded into State and silently dropped (dontguess-b84).
	if err := e.refreshBeforeOperatorOp(); err != nil {
		return fmt.Errorf("refresh before put-accept: %w", err)
	}

	pendingEntry, pending := e.state.GetPendingPut(putMsgID)
	var putSellerKey string
	if pending {
		putSellerKey = pendingEntry.SellerKey
	}
	_ = putSellerKey // used below after e.state.Apply(rec)
	if !pending {
		return fmt.Errorf("put %s is not pending", putMsgID)
	}

	// SEAM A (dontguess-d53, LOAD-BEARING). The poll-loop fold already staged this
	// put into pendingPuts via state_put.go applyPut, which runs with ZERO trust
	// filter; the dispatch trust gate (engine_core.go) only gates handlePut and is
	// BYPASSED on this promotion path (it reads .Level for provenance below, never
	// .Check). So auto-accept promotion is the real choke: without this gate a
	// non-admitted seller's put would become operator-blessed, matchable inventory.
	// Check the seller BEFORE emitting any operator put-accept or touching the
	// match index; a non-admitted seller is counted into a DISTINCT promotion-gate
	// counter, LOUDLY alarmed (never a silent nil-drop, LOCKED-5), and the put is
	// rejected so it leaves pendingPuts (the ticker does not re-alarm every second).
	if e.opts.TrustChecker != nil {
		if terr := e.opts.TrustChecker.Check(putSellerKey, OperationPut, ""); terr != nil {
			reason := "dropped_unlisted"
			if errors.Is(terr, ErrLowReputation) {
				e.degradation.DroppedLowReputation.Add(1)
				reason = "dropped_low_reputation"
			} else {
				e.degradation.DroppedUnlisted.Add(1)
			}
			e.opts.log("SECURITY ALARM: auto-accept promotion BLOCKED for non-admitted seller: put=%s sender=%s reason=%s: %v",
				shortKey(putMsgID), shortKey(putSellerKey), reason, terr)
			// dontguess-327: a SEAM-A trust reject must PURGE the put's content hash
			// from contentHashIndex. applyPut (state_put.go) registered that hash
			// ZERO-TRUST during the fold; left in place it permanently squats the
			// hash and silently blocks a later ALLOWLISTED seller's byte-identical
			// put (the exchange's designed high-reuse happy path) with a bare return
			// at state_put.go dedup §2. Signal the fold to purge (purgeContentHash=
			// true, TRUST-gate path ONLY — QUALITY-gate rejects keep their anti-respam
			// hash persistence) and count the purge so the previously silent
			// squat-and-block lever is observable.
			e.degradation.DroppedDedupPoison.Add(1)
			if rerr := e.rejectPutLocked(putMsgID, "trust-gate: "+reason, true); rerr != nil {
				e.opts.log("engine: put-reject after trust block failed put=%s err=%v", shortKey(putMsgID), rerr)
			}
			return fmt.Errorf("auto-accept trust-gate rejected put %s (%s): %w", putMsgID, reason, terr)
		}
	}

	var expiresAtStr string
	if !expiresAt.IsZero() {
		expiresAtStr = expiresAt.UTC().Format(time.RFC3339)
	}

	payload, err := json.Marshal(map[string]any{
		"phase":      SettlePhaseStrPutAccept,
		"entry_id":   putMsgID,
		"price":      price,
		"expires_at": expiresAtStr,
		"guide":      "Your entry is now live in inventory and searchable by buyers. A compression task has been posted for you (check exchange:assign messages) — completing it earns 50% of token_cost in scrip. You earn residuals each time a buyer purchases your content (10% standard; 20% for high-reuse distilled artifacts: schema checklists, protocol/setup READMEs, CI path filters, language-level test patterns, migration recipes — put the distilled form, not session notes).",
	})
	if err != nil {
		return fmt.Errorf("encoding put-accept payload: %w", err)
	}

	tags := []string{
		TagSettle,
		TagPhasePrefix + SettlePhaseStrPutAccept,
		TagVerdictPrefix + "accepted",
	}
	antecedents := []string{putMsgID}

	// sendOperatorMessage returns the persisted record directly — no need to
	// re-query the store. This avoids the race where lastSentMessage could
	// return a concurrently-written message instead of the one we just sent.
	rec, err := e.sendOperatorMessage(payload, tags, antecedents)
	if err != nil {
		return err
	}

	// Apply immediately so state is consistent before the next poll.
	if rec != nil {
		e.state.Apply(rec)
	}

	// Record the seller's current trust level against the newly accepted entry,
	// so a later de-allowlisting can flag the entry for re-validation.
	if e.opts.TrustChecker != nil && putSellerKey != "" {
		level := int(e.opts.TrustChecker.Level(putSellerKey))
		e.state.SetEntryProvenanceLevel(putMsgID, level)
	}

	// Incrementally update the match index with the newly accepted entry.
	inv := e.state.Inventory()
	for _, entry := range inv {
		if entry.PutMsgID == putMsgID {
			e.matchIndex.Add(e.inventoryEntryToRankInput(entry))
			break
		}
	}

	// Hot compression offer.
	if pendingEntry != nil && pendingEntry.SellerKey != "" {
		if err := e.sendCompressionAssign(pendingEntry); err != nil {
			e.opts.log("engine: compression assign failed entry=%s err=%v", putMsgID, err)
		}
	}
	return nil
}

// RejectPut sends a settle(put-reject) for a pending put message, rejecting it
// from inventory. The put must be in the pending state. After rejection the put
// is no longer actionable and will be pruned from heldForReview on the next
// RunAutoAccept tick.
func (e *Engine) RejectPut(putMsgID string, reason string) error {
	e.opMu.Lock()
	defer e.opMu.Unlock()

	// Refresh state before checking. In local mode this also dispatches any
	// buy appended since the last poll (dontguess-b84).
	if err := e.refreshBeforeOperatorOp(); err != nil {
		return fmt.Errorf("refresh before put-reject: %w", err)
	}
	// QUALITY / operator-initiated reject: purgeContentHash=false so the put's
	// content hash stays registered (anti-respam persistence, state_put.go dedup
	// §2). Only the SEAM-A trust-gate path (autoAcceptPutLocked) purges the hash.
	return e.rejectPutLocked(putMsgID, reason, false)
}

// rejectPutLocked emits a settle(put-reject) for a pending put and applies it.
// Callers must hold e.opMu AND must have refreshed state (refreshBeforeOperatorOp)
// beforehand. RejectPut is the public entrypoint (refresh + this); the Seam-A
// promotion gate in autoAcceptPutLocked calls this directly (it has already
// refreshed and holds opMu) so a trust-blocked put is removed from pendingPuts
// without a nested opMu acquisition or a redundant second refresh.
//
// purgeContentHash threads the SEAM-A trust-gate purge signal into the emitted
// settle(put-reject) payload (dontguess-327). When true, the fold
// (applySettlePutReject) additionally deletes the rejected put's content hash
// from contentHashIndex — undoing the zero-trust registration applyPut made
// during the fold. It is set ONLY by the trust-gate reject path; QUALITY-gate /
// operator rejects pass false so their anti-respam hash persistence is unchanged.
func (e *Engine) rejectPutLocked(putMsgID string, reason string, purgeContentHash bool) error {
	_, pending := e.state.GetPendingPut(putMsgID)
	if !pending {
		return fmt.Errorf("put %s is not pending", putMsgID)
	}
	return e.emitPutReject(putMsgID, reason, purgeContentHash)
}

// emitPutReject sends and applies an operator-signed settle(put-reject) for
// putMsgID — the emit half of rejectPutLocked, split out so the dispatch trust
// gate can reject a put that applyPut dropped at fold (never pending), which the
// client is waiting on and otherwise times out against (dontguess-39d). It is
// idempotent for a non-pending put (applySettlePutReject guards its purge and
// no-ops its delete). It takes no opMu: it only touches LocalStore + State (each
// self-locked), like handleBuy's match emit — and opMu here would deadlock the
// refresh->dispatch reentrancy path.
func (e *Engine) emitPutReject(putMsgID string, reason string, purgeContentHash bool) error {
	fields := map[string]any{
		"phase":    SettlePhaseStrPutReject,
		"entry_id": putMsgID,
		"reason":   reason,
	}
	if purgeContentHash {
		// Emitted ONLY on the trust-gate path so QUALITY-gate reject payloads are
		// byte-unchanged and their dedup persistence stays intact.
		fields["purge_content_hash"] = true
	}
	payload, err := json.Marshal(fields)
	if err != nil {
		return fmt.Errorf("encoding put-reject payload: %w", err)
	}

	tags := []string{
		TagSettle,
		TagPhasePrefix + SettlePhaseStrPutReject,
		TagVerdictPrefix + "rejected",
	}
	antecedents := []string{putMsgID}

	rec, err := e.sendOperatorMessage(payload, tags, antecedents)
	if err != nil {
		return err
	}

	// Apply immediately so state is consistent before the next poll.
	if rec != nil {
		e.state.Apply(rec)
	}
	return nil
}

// RunAutoAccept processes pending puts for one auto-accept tick.
//
// For each pending put:
//   - If TokenCost <= max: call AutoAcceptPut (log success or error as before).
//   - If TokenCost > max and NOT in skippedPuts: log skip once, insert into
//     skippedPuts (log-once guard) AND call State.HoldPutForReview (in-memory
//     classification for the operator CLI via PutsHeldForReview).
//   - If TokenCost > max and already in skippedPuts: silently skip.
//
// Lazy prune: IDs in skippedPuts that are no longer in the pending snapshot are
// removed so that if a put is later accepted (or removed) and re-submitted, it
// is logged again. State.PruneHeldForReview is called with the same pending set
// to keep the two maps consistent.
//
// # Dual-map ownership split
//
// Two maps track over-cap puts, each serving a different consumer:
//
//	skippedPuts (caller-owned, not exported):
//	  Log-once guard. Lives in the ticker goroutine (serve.go). No mutex needed
//	  — it is never accessed from another goroutine. Its sole purpose is
//	  suppressing repeated "skipping put" log lines (one per tick → ~86,400/day
//	  without it). Pruned lazily when a put leaves the pending snapshot.
//
//	heldForReview (State-owned, mutex-protected, exported):
//	  State-level classification. Protected by State's internal mutex. Consumed
//	  by the operator socket handler goroutine so "dontguess operator status"
//	  can surface held puts for human review via PutsHeldForReview(). Pruned
//	  by State.PruneHeldForReview() on the same pending snapshot, keeping both
//	  maps in sync.
//
// Both maps record the same over-cap put IDs; they differ in ownership,
// synchronization, and the consumer they serve.
//
// # Thread safety
//
// skippedPuts is owned exclusively by the caller goroutine.
// opMu serializes the state-mutating body of this function against concurrent
// AutoAcceptPut/RejectPut calls from the operator socket handler goroutine.
// heldForReview lives on State and uses its own mutex.
func (e *Engine) RunAutoAccept(max int64, now time.Time, skippedPuts map[string]struct{}) {
	e.opMu.Lock()
	defer e.opMu.Unlock()

	pending := e.State().PendingPuts()

	// Build a set of current pending IDs for O(1) prune lookups.
	pendingIDs := make(map[string]struct{}, len(pending))
	for _, entry := range pending {
		pendingIDs[entry.PutMsgID] = struct{}{}
	}

	// Lazy prune: remove stale entries from skippedPuts and heldForReview.
	for id := range skippedPuts {
		if _, ok := pendingIDs[id]; !ok {
			delete(skippedPuts, id)
		}
	}
	e.State().PruneHeldForReview(pendingIDs)

	// Process each pending put.
	for _, entry := range pending {
		if entry.TokenCost > max {
			if _, alreadyLogged := skippedPuts[entry.PutMsgID]; !alreadyLogged {
				e.opts.log("skipping put %s: token cost %d > max %d",
					shortKey(entry.PutMsgID), entry.TokenCost, max)
				skippedPuts[entry.PutMsgID] = struct{}{}
				// Also classify as held-for-review in State so the operator CLI
				// can surface it via PutsHeldForReview(). No campfire message.
				e.State().HoldPutForReview(entry.PutMsgID)
			}
			continue
		}
		// High-reuse artifacts earn a 15-point accept-price premium (85% vs 70% of token_cost).
		pricePct := StandardAcceptPriceNumerator
		if IsHighReuseArtifact(entry) {
			pricePct = HighReuseAcceptPriceNumerator
		}
		price := entry.TokenCost * pricePct / 100
		expires := now.Add(72 * time.Hour)
		// Call the locked variant — opMu is already held by this function.
		if err := e.autoAcceptPutLocked(entry.PutMsgID, price, expires); err != nil {
			e.opts.log("auto-accept put %s failed: %v", shortKey(entry.PutMsgID), err)
		} else {
			e.opts.log("auto-accepted put %s: price=%d (token_cost=%d, high_reuse=%v)",
				shortKey(entry.PutMsgID), price, entry.TokenCost, pricePct == HighReuseAcceptPriceNumerator)
		}
	}
}
