package exchange

import (
	"github.com/3dl-dev/dontguess/pkg/matching"
)

// rebuildMatchIndex rebuilds the semantic match index from the current live inventory.
// Called after replay and when the inventory changes significantly.
func (e *Engine) rebuildMatchIndex() {
	inventory := e.state.Inventory()
	inputs := make([]matching.RankInput, 0, len(inventory))
	for _, entry := range inventory {
		// SEAM D (dontguess-d53, reload re-gate; retention rework dontguess-23c):
		// rebuild re-indexes state.Inventory() with ZERO trust filter, and
		// NeedsRevalidation / AcceptedProvenanceLevel are in-memory-only (reset to
		// zero on Replay). The re-gate must therefore re-derive, from durable state,
		// which entries to withhold.
		//
		// It gates on the REVOCATION tombstone, NOT on current allowlist membership.
		// Every entry in state.Inventory() was already accepted (it only exists
		// because a put-accept folded), and the exchange OWNS accepted content
		// (publisher model). So an entry is RETAINED across restart unless its seller
		// was de-allowlisted FOR CAUSE — which is recorded durably in the revoked set
		// (Config.RevokedSellers, loaded into the TrustChecker at startup). This is
		// what preserves the anti-poisoning invariant (a revoked seller's inventory
		// stays out across restarts) WITHOUT dropping the inventory of an ephemeral
		// seller who was simply never re-admitted to the current roster — that
		// conflation was the data-loss bug (dontguess-23c). Nil checker
		// (individual/no-relay tier) → no revocation, all inventory retained.
		if e.opts.TrustChecker != nil && e.opts.TrustChecker.IsRevoked(entry.SellerKey) {
			e.state.FlagEntryForRevalidation(entry.EntryID)
			continue
		}
		inputs = append(inputs, e.inventoryEntryToRankInput(entry))
	}
	e.matchIndex.Rebuild(inputs)
	// Refresh behavioral signals in the index after rebuild so the ranker sees
	// current consume counts and distinct buyer counts from state.
	e.matchIndex.SetBehavioralSignals(e.state.AllEntryBehavioralSignals())
}

// inventoryEntryToRankInput converts an InventoryEntry to a matching.RankInput.
// Price is computed by the engine's pricing logic so the ranker sees current ask price.
func (e *Engine) inventoryEntryToRankInput(entry *InventoryEntry) matching.RankInput {
	return matching.RankInput{
		EntryID:          entry.EntryID,
		SellerKey:        entry.SellerKey,
		Description:      entry.Description,
		ContentType:      entry.ContentType,
		Domains:          entry.Domains,
		TokenCost:        entry.TokenCost,
		Price:            e.computePrice(entry),
		SellerReputation: e.state.SellerReputation(entry.SellerKey),
		PutTimestamp:     entry.PutTimestamp,
	}
}
