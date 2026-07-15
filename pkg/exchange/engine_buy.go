package exchange

import (
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/3dl-dev/dontguess/pkg/demand"
	"github.com/3dl-dev/dontguess/pkg/matching"
	"github.com/3dl-dev/dontguess/pkg/scrip"
)

// handleBuy responds to an exchange:buy request with an exchange:match message.
//
// The buy message is sent as a campfire future (--future). The engine responds
// with --fulfills <buy-msg-id> to complete the future.
//
// If ScripStore is configured, the buyer's scrip balance is pre-decremented
// by (best_price + fee) before matching. If the buyer has insufficient scrip,
// the buy is rejected with ErrBudgetExceeded and no match is emitted.
func (e *Engine) handleBuy(msg *Message) error {
	// Guard: if this order was already matched (a match message exists in the
	// campfire log for this buy), return immediately. This prevents
	// double-dispatch on engine restart when a buy message is re-applied via
	// poll after the corresponding match was already written to the log during
	// a previous run (fix for dontguess-vd0 / dontguess-bf0).
	if e.state.IsOrderMatched(msg.ID) {
		e.opts.log("engine: handleBuy skipped -- order %s already matched", msg.ID[:8])
		return nil
	}

	payload, err := parseBuyPayload(msg)
	if err != nil {
		return err
	}

	// D1 demand-signal bound (design §8-D1, dontguess-3879). OperationBuy stays
	// TrustAnonymous: this buy is about to fold into matching / demand / pricing
	// BEFORE any settlement (the scrip hold is not taken until buyer-accept). The
	// ScripStore bounds the money but not the SIGNAL, so gate the buy on a
	// minimum scrip balance HERE, before it can surface (rank) an entry or move
	// its price. Bounded to the team/federated tier (ScripStore configured) and
	// skips the operator's own key; individual tier (ScripStore == nil) is a
	// no-op — behavior byte-for-byte unchanged.
	if !e.buyerMeetsMinBalance(msg.Sender) {
		e.degradation.DroppedUnderfundedBuy.Add(1)
		e.opts.log("SECURITY ALARM: anonymous buy dropped by demand-signal bound (D1): buyer=%s min_balance=%d order=%s -- not folded into matching/demand/pricing",
			shortKey(msg.Sender), e.opts.MinBuyBalance, shortKey(msg.ID))
		return nil
	}

	maxResults := buyMaxResults(payload.MaxResults)
	maxResults = e.applyDebtorPriority(msg.Sender, maxResults)

	// Brokered-match routing decision.
	if e.shouldUseBrokered(msg.Sender) {
		return e.sendBrokeredMatchAssign(msg, payload.Task, maxResults)
	}

	// Inline semantic matching.
	candidates := e.findCandidates(msg.Sender, payload.Budget, payload.MinReputation,
		payload.FreshnessHours, payload.ContentType, payload.Domains, payload.CompressionTier)

	semanticMatches := e.mergeSemanticAndFallback(payload.Task, candidates, maxResults)

	synthetic := isSyntheticRequest(payload.Task, payload.Synthetic)

	// Zero-match path.
	if len(semanticMatches) == 0 {
		return e.handleBuyMiss(msg, payload.Task, payload.Budget, synthetic)
	}

	return e.emitMatchResponse(msg, payload.Task, semanticMatches, candidates, synthetic)
}

// buyPayload holds the parsed buy request fields.
type buyPayload struct {
	Task            string
	Budget          int64
	MaxResults      int
	MinReputation   int
	FreshnessHours  int
	ContentType     string
	Domains         []string
	CompressionTier string
	Synthetic       bool
}

// parseBuyPayload unmarshals and normalises the buy message payload.
func parseBuyPayload(msg *Message) (buyPayload, error) {
	var raw struct {
		Task            string   `json:"task"`
		Budget          int64    `json:"budget"`
		MaxPrice        int64    `json:"max_price"`
		MinReputation   int      `json:"min_reputation"`
		FreshnessHours  int      `json:"freshness_hours"`
		ContentType     string   `json:"content_type"`
		Domains         []string `json:"domains"`
		MaxResults      int      `json:"max_results"`
		CompressionTier string   `json:"compression_tier"`
		Synthetic       bool     `json:"synthetic"`
	}
	if err := json.Unmarshal(msg.Payload, &raw); err != nil {
		return buyPayload{}, fmt.Errorf("parsing buy payload: %w", err)
	}
	// Accept max_price as alias for budget — agents naturally use this name.
	if raw.Budget == 0 && raw.MaxPrice > 0 {
		raw.Budget = raw.MaxPrice
	}
	// Normalize tag-prefixed enum values from convention dispatch.
	raw.ContentType = stripTagPrefix(raw.ContentType, "exchange:content-type:")
	raw.Domains = stripDomainPrefixes(raw.Domains)
	return buyPayload{
		Task:            raw.Task,
		Budget:          raw.Budget,
		MaxResults:      raw.MaxResults,
		MinReputation:   raw.MinReputation,
		FreshnessHours:  raw.FreshnessHours,
		ContentType:     raw.ContentType,
		Domains:         raw.Domains,
		CompressionTier: raw.CompressionTier,
		Synthetic:       raw.Synthetic,
	}, nil
}

// buyerMeetsMinBalance reports whether a buyer is allowed to contribute a demand
// signal, enforcing the D1 anonymous-buy signal bound (design §8-D1,
// dontguess-3879).
//
// The bound is active only when BOTH a ScripStore is configured (team/federated
// tier) AND MinBuyBalance > 0. On the individual tier (ScripStore == nil) or
// with the bound disabled (MinBuyBalance <= 0) it always returns true, keeping
// behavior byte-for-byte unchanged. The operator's own key is exempt — the
// operator is the trusted local writer, not an anonymous Sybil.
//
// When active, the buyer must hold at least MinBuyBalance scrip. A GetBudget
// error (unknown key folds to a zero balance) is treated as under-funded and
// bounds the signal (fail-closed).
func (e *Engine) buyerMeetsMinBalance(buyerKey string) bool {
	if e.opts.MinBuyBalance <= 0 || e.opts.ScripStore == nil {
		return true
	}
	if buyerKey == e.state.OperatorKey {
		return true
	}
	bal, _, err := e.opts.ScripStore.GetBudget(e.engineCtx(), buyerKey, scrip.BalanceKey)
	if err != nil {
		return false
	}
	return bal >= e.opts.MinBuyBalance
}

// buyMaxResults normalises the caller-supplied max_results to a positive integer.
func buyMaxResults(requested int) int {
	if requested <= 0 {
		return 3
	}
	return requested
}

// applyDebtorPriority applies debtor priority weighting (S7), reducing maxResults
// for buyers with outstanding debt. Returns the adjusted maxResults.
func (e *Engine) applyDebtorPriority(buyerKey string, maxResults int) int {
	debtorScore := e.state.DebtorScore(buyerKey)
	if debtorScore >= 1.0 {
		return maxResults
	}
	weighted := int(math.Floor(float64(maxResults) * debtorScore))
	if weighted < 1 {
		weighted = 1
	}
	if weighted < maxResults {
		e.opts.log("engine: handleBuy debtor priority applied buyer=%s score=%.3f maxResults %d→%d",
			shortKey(buyerKey), debtorScore, maxResults, weighted)
		return weighted
	}
	return maxResults
}

// shouldUseBrokered decides whether this buy request should be routed to
// brokered-match mode. Checks BrokeredMatchMode option and the federation guard.
func (e *Engine) shouldUseBrokered(senderKey string) bool {
	if !e.opts.BrokeredMatchMode {
		return false
	}
	if e.opts.FederationGuardEnabled && e.isLowTrustSender(senderKey) {
		e.opts.log("engine: handleBuy federation guard: sender=%s low trust, routing inline", shortKey(senderKey))
		return false
	}
	return true
}

// rankedCandidate holds a candidate entry with its semantic ranking metadata.
type rankedCandidate struct {
	entry            *InventoryEntry
	confidence       float64
	similarity       float64 // raw cosine similarity from matching.RankedResult; 0 for fallback entries
	isPartialMatch   bool
	hasSemanticScore bool
}

// mergeSemanticAndFallback performs semantic ranking and merges with fallback
// (reputation+recency) candidates, capped at maxResults.
func (e *Engine) mergeSemanticAndFallback(task string, candidates []*InventoryEntry, maxResults int) []rankedCandidate {
	semanticResults := e.matchIndex.Search(task, maxResults*3)

	// Build a lookup: entryID → semantic result.
	semanticByID := make(map[string]matching.RankedResult, len(semanticResults))
	for _, r := range semanticResults {
		semanticByID[r.EntryID] = r
	}

	// Filter semantic results to those that also passed the hard filters.
	const partialMatchThreshold = 0.5
	candidateSet := make(map[string]*InventoryEntry, len(candidates))
	for _, c := range candidates {
		candidateSet[c.EntryID] = c
	}

	var semanticMatches []rankedCandidate
	for _, sr := range semanticResults {
		entry, ok := candidateSet[sr.EntryID]
		if !ok {
			continue // did not pass hard filters
		}
		semanticMatches = append(semanticMatches, rankedCandidate{
			entry:            entry,
			confidence:       sr.Confidence,
			similarity:       sr.Similarity,
			isPartialMatch:   sr.IsPartialMatch,
			hasSemanticScore: true,
		})
	}

	// Append genuine index-gap candidates (no embedding yet).
	//
	// Gate: use matchIndex.HasEmbedding to distinguish the two populations:
	//   - HasEmbedding=true  AND not in covered → below-floor embedded entry → skip
	//   - HasEmbedding=false AND not in covered → genuine index-gap entry     → allow
	covered := make(map[string]struct{}, len(semanticMatches))
	for _, sm := range semanticMatches {
		covered[sm.entry.EntryID] = struct{}{}
	}
	var fallbackCandidates []*InventoryEntry
	for _, c := range candidates {
		if _, ok := covered[c.EntryID]; ok {
			continue
		}
		if e.matchIndex.HasEmbedding(c.EntryID) {
			continue // below-floor; floor decision stands
		}
		fallbackCandidates = append(fallbackCandidates, c)
	}
	ranked := e.rankResults(fallbackCandidates, maxResults)
	for _, entry := range ranked {
		conf := e.computeConfidence(entry, task)
		semanticMatches = append(semanticMatches, rankedCandidate{
			entry:            entry,
			confidence:       conf,
			isPartialMatch:   conf < partialMatchThreshold,
			hasSemanticScore: false,
		})
	}

	// Cap at maxResults.
	if len(semanticMatches) > maxResults {
		semanticMatches = semanticMatches[:maxResults]
	}
	return semanticMatches
}

// emitMatchResponse builds the match payload and sends the exchange:match message.
// It also triggers a warm compression offer for the top-matched entry.
func (e *Engine) emitMatchResponse(msg *Message, task string, semanticMatches []rankedCandidate, candidates []*InventoryEntry, synthetic bool) error {
	type MatchResult struct {
		EntryID           string  `json:"entry_id"`
		PutMsgID          string  `json:"put_msg_id"`
		SellerKey         string  `json:"seller_key"`
		Description       string  `json:"description"`
		ContentHash       string  `json:"content_hash,omitempty"`
		ContentType       string  `json:"content_type"`
		Price             int64   `json:"price"`
		Confidence        float64 `json:"confidence"`
		Similarity        float64 `json:"similarity"` // raw cosine similarity; 0 for fallback entries
		IsPartialMatch    bool    `json:"is_partial_match"`
		SellerReputation  int     `json:"seller_reputation"`
		TokenCostOriginal int64   `json:"token_cost_original"`
		AgeHours          int     `json:"age_hours"`
	}

	matchResults := make([]MatchResult, len(semanticMatches))
	for i, rc := range semanticMatches {
		entry := rc.entry
		ageHours := int(time.Since(time.Unix(0, entry.PutTimestamp)).Hours())
		rep := e.state.SellerReputation(entry.SellerKey)

		// content_hash is the operator-LOCAL sha256(plaintext) dedup key. For a v2
		// confidential entry (WrappedCEKOperator != "") it is an unsalted
		// plaintext hash, so publishing it on the public exchange:match event
		// converts it into a guess-confirmation + cross-entry-correlation oracle
		// (content-confidentiality-envelope-541 §4.4, A1/P1: RESOLVED means it must
		// NOT be on any public wire). It buys the buyer nothing pre-purchase —
		// matching ranks on Description, integrity is verified against
		// ciphertext_hash in the deliver envelope (§4.4 A7) — so OMIT it for v2.
		// Individual-tier genuinely-local plaintext entries (WrappedCEKOperator == "",
		// LegacyPlaintext == false) are local/confidential and keep content_hash
		// unchanged. A GRANDFATHERED pre-climb entry (LegacyPlaintext == true) is
		// NEITHER local (a relay buyer could reach it absent the findCandidates fence)
		// NOR already-public (the climb fence kept it off the relay) — publishing its
		// unsalted sha256(pre-climb plaintext) here would re-open the §4.4 A1/P1
		// plaintext-hash oracle on the public exchange:match wire (dontguess-9d1). It
		// is already excluded from candidates upstream; this blanks the hash as
		// defense-in-depth so no code path can leak it.
		contentHash := entry.ContentHash
		if entry.WrappedCEKOperator != "" || entry.LegacyPlaintext {
			contentHash = ""
		}
		matchResults[i] = MatchResult{
			EntryID:           entry.EntryID,
			PutMsgID:          entry.PutMsgID,
			SellerKey:         entry.SellerKey,
			Description:       entry.Description,
			ContentHash:       contentHash,
			ContentType:       entry.ContentType,
			Price:             e.computePrice(entry),
			Confidence:        rc.confidence,
			Similarity:        rc.similarity,
			IsPartialMatch:    rc.isPartialMatch,
			SellerReputation:  rep,
			TokenCostOriginal: entry.TokenCost,
			AgeHours:          ageHours,
		}
	}

	meta := map[string]any{"total_candidates": len(candidates)}

	matchPayload, err := e.marshal(map[string]any{
		"results":     matchResults,
		"search_meta": meta,
		"guide":       "Results are ranked by: (1) correctness gate — only entries that completed similar tasks pass, (2) transaction efficiency — tokens saved per scrip spent, (3) value composite — confidence × freshness × reputation × diversity, (4) market novelty — discovery boost for underrepresented sellers. Higher confidence = stronger semantic match. Reputation 70+ is established; below 30 is untested. To purchase: send settle(preview-request) to sample content before committing scrip. Price shown includes dynamic market adjustments.",
	})
	if err != nil {
		return fmt.Errorf("encoding match payload: %w", err)
	}

	// Tag synthetic match responses so the hit-rate reporter can exclude them.
	tags := []string{TagMatch}
	if synthetic {
		tags = append(tags, TagSynthetic)
	}
	antecedents := []string{msg.ID}

	matchRec, err := e.sendOperatorMessage(matchPayload, tags, antecedents)
	if err != nil {
		return err
	}

	// Apply the match message to state immediately so downstream handlers can
	// reference it via matchToBuyer / matchToEntry without requiring an explicit Replay.
	if matchRec != nil {
		e.state.Apply(matchRec)
	}

	// Warm compression offer: if the top-matched entry has no compressed
	// derivative, offer the buyer an exclusive compression assign.
	//
	// Skip a grandfathered pre-climb entry (dontguess-9d1): sendCompressionAssign
	// embeds entry.ContentHash = sha256(plaintext) in the PUBLIC exchange:assign
	// work order — the same §4.4 A1/P1 plaintext-hash oracle the content_hash gate
	// above suppresses. findCandidates already fences these out of semanticMatches,
	// so this is defense-in-depth: a grandfathered entry can never be topEntry in
	// production, but if it ever were, no assign must republish its hash.
	topEntry := semanticMatches[0].entry
	if !topEntry.LegacyPlaintext && topEntry.CompressedFrom == "" && !e.state.HasCompressedVersion(topEntry.EntryID) && !e.hasActiveBuyerCompressAssign(topEntry.EntryID, msg.Sender) {
		if err := e.sendWarmCompressionAssign(topEntry, msg.Sender); err != nil {
			e.opts.log("engine: warm compression assign failed entry=%s err=%v", topEntry.PutMsgID, err)
		}
	}

	return nil
}

// handleBuyMiss handles the zero-match path for a buy request.
//
// It records a standing buy-miss offer in state and emits an exchange:buy-miss
// message back to the campfire (fulfilling the buy future) with:
//   - offered_price_rate: BuyMissOfferRate (70% of token_cost)
//   - task_hash: SHA-256 of the task description
//   - expires_at: now + BuyMissOfferTTL
//
// One offer per task hash — if a non-expired offer already exists for this task
// the engine still sends the buy-miss response (idempotent from buyer's view)
// but does not create a duplicate offer in state.
//
// synthetic indicates that the buy task matched demand.IsSynthetic. When true,
// the emitted exchange:buy-miss message is tagged exchange:synthetic so the
// hit-rate reporter can exclude it from production metrics.
func (e *Engine) handleBuyMiss(msg *Message, task string, budget int64, synthetic bool) error {
	taskHash := TaskDescriptionHash(task)
	expiresAt := time.Now().Add(BuyMissOfferTTL)

	offer := &BuyMissOffer{
		TaskHash:  taskHash,
		BuyMsgID:  msg.ID,
		BuyerKey:  msg.Sender,
		Task:      task,
		ExpiresAt: expiresAt,
	}
	e.state.SetBuyMissOffer(offer)

	buyMissPayload, err := e.marshal(map[string]any{
		"task_hash":          taskHash,
		"task":               task,
		"offered_price_rate": BuyMissOfferRate,
		"expires_at":         expiresAt.UTC().Format(time.RFC3339),
		"buy_msg_id":         msg.ID,
		"guide":              fmt.Sprintf("No cached inference matched your task. A standing offer has been created: if you (or any agent) compute the result and PUT it to the exchange, the exchange will buy it at %d%% of token_cost. This offer expires at the time shown. Alternatively, try a broader task description, increase your budget, or relax freshness constraints.", BuyMissOfferRate),
	})
	if err != nil {
		return fmt.Errorf("encoding buy-miss payload: %w", err)
	}

	// Tag synthetic buy-miss responses so the hit-rate reporter can exclude them.
	tags := []string{TagBuyMiss, TagMatch}
	if synthetic {
		tags = append(tags, TagSynthetic)
	}
	antecedents := []string{msg.ID}

	rec, err := e.sendOperatorMessage(buyMissPayload, tags, antecedents)
	if err != nil {
		return err
	}
	if rec != nil {
		e.state.Apply(rec)
	}

	e.opts.log("engine: buy-miss: order=%s task_hash=%s expires=%s synthetic=%v",
		msg.ID[:8], taskHash[:16], expiresAt.Format(time.RFC3339), synthetic)
	return nil
}

// findCandidates returns inventory entries that satisfy the buyer's filters.
// compressionTier, when non-empty, restricts candidates to entries with a
// matching CompressionTier. Entries with an unset tier ("") are excluded when
// a tier filter is specified — a seller that did not declare a tier is not a
// match for a buyer that requires one.
func (e *Engine) findCandidates(buyerKey string, budget int64, minRep int,
	freshnessHours int, contentType string, domains []string, compressionTier string) []*InventoryEntry {

	// Layer 0 correctness gate: exclude entries with poor preview-to-purchase
	// conversion rate (dontguess-5iz). Only applied once per call and keyed by
	// entry ID for O(1) lookup in the loop below. Reversible: if an entry's
	// conversion rate improves above the threshold, it re-appears automatically
	// on the next call since LowConversionEntries is computed fresh from state.
	lowConv := e.state.LowConversionEntries(Layer0MinPreviews, Layer0MaxConversionRate)
	excluded := make(map[string]struct{}, len(lowConv))
	for _, id := range lowConv {
		excluded[id] = struct{}{}
	}

	inventory := e.state.Inventory()
	var out []*InventoryEntry

	for _, entry := range inventory {
		// Layer 0: exclude entries with low conversion rate (insufficient buyer demand).
		if _, ok := excluded[entry.EntryID]; ok {
			continue
		}

		// Provenance revalidation gate: exclude entries flagged for re-validation
		// due to a seller provenance downgrade (dontguess-lqp). These entries remain
		// in inventory but are withheld from buyers until the operator clears the flag.
		if entry.NeedsRevalidation {
			continue
		}

		// Climb egress fence — match/deliver leg (dontguess-9d1). A grandfathered
		// pre-climb plaintext entry (LegacyPlaintext, WrappedCEKOperator=="") was
		// folded into ACTIVE inventory during the solo→fleet climb Replay so the
		// operator does not lose its pre-migration corpus. The Outbox climb fence
		// keeps the RAW put off the relay; this keeps the entry off the match/deliver
		// path too. Without it a relay/team buyer could MATCH the entry, receive its
		// unsalted sha256(pre-climb plaintext) on the exchange:match wire (the §4.4
		// A1/P1 plaintext-hash oracle), and — on settle — receive the pre-climb
		// PLAINTEXT itself (design §6 ADV-18). LegacyPlaintext is set ONLY on the team
		// tier (state_put.go grandfathers only under s.encryptedRequired), so
		// individual/solo-tier genuinely-local plaintext (LegacyPlaintext==false) is
		// unaffected and stays matchable.
		if entry.LegacyPlaintext {
			continue
		}

		// Budget filter: price must not exceed budget.
		price := e.computePrice(entry)
		if price > budget {
			continue
		}

		// Reputation filter.
		rep := e.state.SellerReputation(entry.SellerKey)
		if rep < minRep {
			continue
		}

		// Freshness filter.
		if freshnessHours > 0 {
			ageHours := int(time.Since(time.Unix(0, entry.PutTimestamp)).Hours())
			if ageHours > freshnessHours {
				continue
			}
		}

		// Content type filter.
		if contentType != "" && entry.ContentType != contentType {
			continue
		}

		// Domain filter: entry must have at least one matching domain.
		if len(domains) > 0 && !hasOverlap(entry.Domains, domains) {
			continue
		}

		// Compression tier filter: when the buyer specifies a tier, only entries
		// with an exact tier match are candidates. Entries with no tier set are
		// excluded — an unspecified tier does not implicitly match all filters.
		if compressionTier != "" && entry.CompressionTier != compressionTier {
			continue
		}

		out = append(out, entry)
	}
	return out
}

// rankResults scores and sorts candidates, returning the top maxResults.
// Uses a simple composite: seller reputation * 0.6 + recency * 0.4
// The full 4-layer value stack is a pricing-engine concern; here we use a
// lightweight proxy appropriate for the exchange engine's role.
func (e *Engine) rankResults(candidates []*InventoryEntry, max int) []*InventoryEntry {
	type scored struct {
		entry *InventoryEntry
		score float64
	}

	now := time.Now().UnixNano()
	scored_ := make([]scored, len(candidates))
	for i, entry := range candidates {
		rep := float64(e.state.SellerReputation(entry.SellerKey))
		// Recency: 1.0 for brand-new, decaying over 30 days.
		ageSec := float64(now-entry.PutTimestamp) / 1e9
		recency := 1.0 - (ageSec / rankResultsRecencyDays)
		if recency < 0 {
			recency = 0
		}
		scored_[i] = scored{entry: entry, score: rep/100.0*scoreRepWeight + recency*(1.0-scoreRepWeight)}
	}

	// Sort descending by score.
	for i := 0; i < len(scored_); i++ {
		for j := i + 1; j < len(scored_); j++ {
			if scored_[j].score > scored_[i].score {
				scored_[i], scored_[j] = scored_[j], scored_[i]
			}
		}
	}

	if max > len(scored_) {
		max = len(scored_)
	}
	out := make([]*InventoryEntry, max)
	for i := 0; i < max; i++ {
		out[i] = scored_[i].entry
	}
	return out
}

// sendBrokeredMatchAssign posts an exchange:assign with task_type="brokered-match"
// for the given buy message. Workers claim, search inventory, and deliver ranked
// results. The assign payload carries the buyer's task description, the buy message
// ID (for correlation), and the maximum results count.
//
// Called from handleBuy when BrokeredMatchMode is enabled. Failure is fatal to
// the caller — the buy cannot proceed without the assign.
func (e *Engine) sendBrokeredMatchAssign(buyMsg *Message, task string, maxResults int) error {
	// Idempotency: if this buy already has a brokered-match assign (e.g. engine
	// restarted after posting the assign but before processing the complete), skip.
	if _, exists := e.state.BrokerAssignForBuy(buyMsg.ID); exists {
		e.opts.log("engine: brokered-match assign already exists for buy=%s, skipping", shortKey(buyMsg.ID))
		return nil
	}
	if maxResults <= 0 {
		maxResults = 3
	}
	reward := e.opts.brokeredMatchReward()
	payload, err := json.Marshal(map[string]any{
		"task_type":        "brokered-match",
		"buy_msg_id":       buyMsg.ID,
		"task_description": task,
		"max_results":      maxResults,
		"reward":           reward,
		"guide":            "Brokered match: search inventory for entries matching the task description. Return up to max_results ranked by: correctness (did similar tasks complete?), efficiency (tokens saved per scrip), value composite (confidence × freshness × reputation × diversity), novelty (underrepresented sellers boosted). Submit results via assign-complete with a JSON array of {entry_id, confidence} objects.",
	})
	if err != nil {
		return fmt.Errorf("encoding brokered-match assign payload: %w", err)
	}
	tags := []string{TagAssign}
	msg, err := e.sendOperatorMessage(payload, tags, []string{buyMsg.ID})
	if err != nil {
		return fmt.Errorf("sending brokered-match assign: %w", err)
	}
	if msg != nil {
		e.state.Apply(msg)
	}
	e.opts.log("engine: brokered-match assign sent assign_id=%s buy=%s task=%q reward=%d",
		shortKey(msg.ID), shortKey(buyMsg.ID), task, reward)
	return nil
}

// skipCompressionForV2 documents why every compression-assign path bails out
// when entry.WrappedCEKOperator != "" (a v2 confidential entry):
//
//  1. LEAK: the compression work order embeds entry.ContentHash =
//     sha256(plaintext) (compressionProtocol, "Content hash:"), and the assign is
//     a PUBLIC exchange:assign message. For a v2 entry that re-broadcasts an
//     unsalted plaintext hash onto the public wire — the exact
//     guess-confirmation + correlation oracle §4.4 (A1/P1) removed. RESOLVED
//     means it must never reappear on any public wire.
//  2. NONSENSE: a compressor cannot meaningfully compress AEAD ciphertext (it is
//     high-entropy and semantically opaque), and the operator never re-publishes
//     the plaintext. Confidential entries therefore get NO compression derivative
//     — there is no plaintext for a compressor to act on. Using ciphertext_hash
//     instead would close the leak but still order impossible work, so the
//     minimal correct change is to not post the assign at all.
//
// Individual-tier / legacy plaintext entries (WrappedCEKOperator == "") are
// unaffected — they keep the full compression flow byte-for-byte.

// sendCompressionAssign sends an exchange:assign message for a compress task
// directed exclusively at the original seller of the given entry. The bounty
// is 50% of the entry's token_cost. The description includes the entry ID,
// content hash, and instructions to run /compress.
//
// This is sent immediately after a put is accepted (hot path). Failure is
// non-fatal to the caller — the error is logged and the accept proceeds.
func (e *Engine) sendCompressionAssign(entry *InventoryEntry) error {
	if entry.WrappedCEKOperator != "" {
		return nil // v2 confidential entry: see skipCompressionForV2.
	}
	if entry.LegacyPlaintext {
		// Defense-in-depth (dontguess-751): a GRANDFATHERED pre-climb entry's
		// ContentHash is sha256(plaintext) — the same A1/P1 hash oracle
		// skipCompressionForV2 guards against for v2 confidential entries.
		// findCandidates already fences these out of the match candidate set
		// and emitMatchResponse independently skips them (dontguess-9d1), but
		// this helper must not rely on those upstream gates never being
		// reordered: never embed content_hash for a grandfathered entry.
		return nil
	}
	bounty := entry.TokenCost * HotCompressionBountyPct / 100
	description := compressionProtocol(entry.EntryID, entry.ContentHash, entry.ContentType, bounty)
	payload, err := json.Marshal(map[string]any{
		"entry_id":         entry.EntryID,
		"task_type":        "compress",
		"reward":           bounty,
		"exclusive_sender": entry.SellerKey,
		"description":      description,
		"guide":            "Hot compression: you just sold this content and have it cached — ideal position to compress. Accepted work creates a compressed derivative listed alongside the original. You earn the bounty now plus residuals (10% of sale price) each time either version sells.",
	})
	if err != nil {
		return fmt.Errorf("encoding compression assign payload: %w", err)
	}
	tags := []string{TagAssign}
	msg, err := e.sendOperatorMessage(payload, tags, nil)
	if err != nil {
		return fmt.Errorf("sending compression assign: %w", err)
	}
	if msg != nil {
		e.state.Apply(msg)
	}
	e.opts.log("engine: compression assign sent assign_id=%s entry=%s seller=%s bounty=%d",
		shortKey(msg.ID), shortKey(entry.PutMsgID), shortKey(entry.SellerKey), bounty)
	return nil
}

// sendWarmCompressionAssign sends an exchange:assign message for a compress task
// directed exclusively at the buyer of the matched entry. The bounty is 30% of
// the entry's token_cost (floor integer division). The buyer just consumed the
// raw content and has it in cache, making them the ideal compressor.
//
// Called after a successful match when no compressed derivative exists for the
// matched entry. Failure is non-fatal to the caller — the error is logged and
// the match proceeds.
func (e *Engine) sendWarmCompressionAssign(entry *InventoryEntry, buyerKey string) error {
	if entry.WrappedCEKOperator != "" {
		return nil // v2 confidential entry: see skipCompressionForV2.
	}
	if entry.LegacyPlaintext {
		// Defense-in-depth (dontguess-751): see sendCompressionAssign above —
		// same ContentHash=sha256(plaintext) oracle for a grandfathered entry.
		return nil
	}
	bounty := entry.TokenCost * WarmCompressionBountyPct / 100
	description := compressionProtocol(entry.EntryID, entry.ContentHash, entry.ContentType, bounty)
	payload, err := json.Marshal(map[string]any{
		"entry_id":         entry.EntryID,
		"task_type":        "compress",
		"reward":           bounty,
		"exclusive_sender": buyerKey,
		"description":      description,
		"guide":            "Warm compression: you just purchased this content and have it in context — ideal position to compress. The compressed derivative becomes independently purchasable. Original seller earns residuals on both versions; you earn the bounty.",
	})
	if err != nil {
		return fmt.Errorf("encoding warm compression assign payload: %w", err)
	}
	tags := []string{TagAssign}
	msg, err := e.sendOperatorMessage(payload, tags, nil)
	if err != nil {
		return fmt.Errorf("sending warm compression assign: %w", err)
	}
	if msg != nil {
		e.state.Apply(msg)
	}
	e.opts.log("engine: warm compression assign sent assign_id=%s entry=%s buyer=%s bounty=%d",
		shortKey(msg.ID), shortKey(entry.PutMsgID), shortKey(buyerKey), bounty)
	return nil
}

// PostOpenCompressionAssign posts a non-exclusive cold compression assign for the
// given entry. This is the public entry point used by the medium loop's PostAssign
// callback. The bounty is ColdCompressionBountyPct (20%) of token_cost.
//
// Returns an error if the entry is not found or the assign cannot be sent.
func (e *Engine) PostOpenCompressionAssign(entryID string) error {
	entry := e.state.GetInventoryEntry(entryID)
	if entry == nil {
		return fmt.Errorf("entry %s not found in inventory", entryID)
	}
	return e.sendColdCompressionAssign(entry)
}

// sendColdCompressionAssign sends an exchange:assign message for a cold compress
// task with no exclusive sender — any eligible agent can claim it. The bounty is
// ColdCompressionBountyPct (20%) of the entry's token_cost. Posted by the medium
// loop for high-demand entries that still lack a compressed derivative.
func (e *Engine) sendColdCompressionAssign(entry *InventoryEntry) error {
	if entry.WrappedCEKOperator != "" {
		return nil // v2 confidential entry: see skipCompressionForV2.
	}
	bounty := entry.TokenCost * ColdCompressionBountyPct / 100
	description := compressionProtocol(entry.EntryID, entry.ContentHash, entry.ContentType, bounty)
	payload, err := json.Marshal(map[string]any{
		"entry_id":    entry.EntryID,
		"task_type":   "compress",
		"reward":      bounty,
		"description": description,
		"guide":       "Cold compression: this entry has active buyer demand but no compressed version. Open to any eligible agent. The compressed derivative becomes independently purchasable at a density premium. Claim timeout: 30 minutes for entries >50k tokens, 15 minutes otherwise.",
	})
	if err != nil {
		return fmt.Errorf("encoding cold compression assign payload: %w", err)
	}
	tags := []string{TagAssign}
	msg, err := e.sendOperatorMessage(payload, tags, nil)
	if err != nil {
		return fmt.Errorf("sending cold compression assign: %w", err)
	}
	if msg != nil {
		e.state.Apply(msg)
	}
	e.opts.log("engine: cold compression assign sent assign_id=%s entry=%s bounty=%d",
		shortKey(msg.ID), shortKey(entry.PutMsgID), bounty)
	return nil
}

// hasActiveBuyerCompressAssign returns true if there is already an active
// (non-terminal) compression assign for the given entry targeting the buyer.
// Used to prevent duplicate warm compression assigns for the same buyer.
func (e *Engine) hasActiveBuyerCompressAssign(entryID, buyerKey string) bool {
	for _, a := range e.state.ActiveAssigns(entryID) {
		if a.TaskType == "compress" && a.ExclusiveSender == buyerKey {
			return true
		}
	}
	return false
}

// isSyntheticRequest reports whether a buy/put request should be treated as
// synthetic (load-test / infrastructure probe) traffic. text is the task
// description or put description; declared is the explicit Synthetic flag from
// the message payload. Either condition is sufficient.
//
// Used in handleBuy and handlePut so the synthetic-detection rule stays in one
// place. Both callers must tag their emitted messages with TagSynthetic when
// this returns true.
func isSyntheticRequest(text string, declared bool) bool {
	return demand.IsSynthetic(text) || declared
}

// entryForDeliver derives the inventory entry for a deliver message ID by tracing
// the antecedent chain: deliver → match (deliverToMatch) → entry (matchToEntry).
// Returns nil if any link in the chain is missing or the entry is not in inventory.
func (e *Engine) entryForDeliver(deliverMsgID string) *InventoryEntry {
	entry, ok := e.state.EntryForDeliver(deliverMsgID)
	if !ok {
		return nil
	}
	return entry
}
