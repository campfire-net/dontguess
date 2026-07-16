package exchange_test

// admission_seam_test.go — dontguess-d53, the four-seam nostr admission gate
// (design docs/design/nostr-admission-scrip-rehome-3b8.md §3).
//
// The VERIFIED subtlety these tests encode: the poll-loop fold (state_put.go
// applyPut) stages EVERY put into pendingPuts with ZERO trust filter, and the
// dispatch trust gate (engine_core.go) only gates handlePut — it is BYPASSED on
// the auto-accept promotion path (it reads .Level for provenance, never .Check).
// So the load-bearing gate is at PROMOTION (Seam A, autoAcceptPutLocked), not at
// dispatch. These tests drive the REAL auto-accept ticker (RunAutoAccept) over a
// REAL TrustChecker — no mock of the gate — and assert a non-admitted seller's
// put NEVER becomes matchable inventory, that de-allowlisting is visible at
// runtime (Seam C) AND survives a restart (Seam D), and that the individual
// (no-relay, nil-checker) tier is byte-for-byte unchanged.

import (
	"testing"
	"time"

	"github.com/3dl-dev/dontguess/pkg/exchange"
)

// admInjectPut writes a substantive, quality-gate-passing exchange:put from the
// given seller agent and returns the put message (its ID is the entry ID after
// acceptance). Distinct descriptions across calls keep content hashes unique so
// the dedup gate never collides.
func admInjectPut(t *testing.T, h *testHarness, seller *testAgent, desc string, tokenCost int64) *exchange.Message {
	t.Helper()
	return h.sendMessage(seller,
		putPayload(desc, "", "code", tokenCost, 24576),
		[]string{exchange.TagPut, "exchange:content-type:code"},
		nil,
	)
}

// TestAdmissionSeamA_NonAllowlistedPutNeverMatchable: a validly-formed put from a
// seller NOT on the fleet allowlist is staged into pendingPuts by the fold, then
// BLOCKED at auto-accept promotion (Seam A) — it never enters inventory or the
// match index, dropped_unlisted increments, and the put is rejected out of
// pendingPuts (so the ticker does not re-alarm every tick).
func TestAdmissionSeamA_NonAllowlistedPutNeverMatchable(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	anon := newTestAgent(t)

	// Allowlist someone else — the anon seller is not admitted.
	ks := exchange.NewKeySet(h.seller.pubKeyHex)
	tc, err := exchange.NewTrustChecker(h.operator.pubKeyHex, ks)
	if err != nil {
		t.Fatalf("NewTrustChecker: %v", err)
	}
	eng := h.newEngineWithOpts(func(o *exchange.EngineOptions) { o.TrustChecker = tc })

	admInjectPut(t, h, anon, "Go generics type inference walkthrough with worked examples", 8500)
	replayAll(t, h, eng)

	// The fold must have staged it (this is the exact hole: staged with no trust
	// check). If it is not pending, the test is not exercising the promotion gate.
	if len(eng.State().PendingPuts()) != 1 {
		t.Fatalf("precondition: expected 1 pending put staged by the fold, got %d", len(eng.State().PendingPuts()))
	}

	// Drive the REAL auto-accept ticker.
	eng.RunAutoAccept(exchange.MaxTokenCost, time.Now(), map[string]struct{}{})

	if n := eng.MatchIndexLen(); n != 0 {
		t.Errorf("Seam A FAILED: non-allowlisted put entered the match index (len=%d, want 0)", n)
	}
	if inv := eng.State().Inventory(); len(inv) != 0 {
		t.Errorf("Seam A FAILED: non-allowlisted put entered inventory (%d entries, want 0)", len(inv))
	}
	d := eng.DegradationSnapshot()
	if d.DroppedUnlisted != 1 {
		t.Errorf("dropped_unlisted=%d, want 1", d.DroppedUnlisted)
	}
	if d.DroppedLowReputation != 0 {
		t.Errorf("dropped_low_reputation=%d, want 0", d.DroppedLowReputation)
	}
	// Rejected out of pendingPuts — not left to re-alarm forever.
	if p := eng.State().PendingPuts(); len(p) != 0 {
		t.Errorf("blocked put still pending after Seam A reject: %d", len(p))
	}
}

// TestAdmissionSeamA_AllowlistedPutMatchable: a put from an admitted fleet member
// is promoted through auto-accept into matchable inventory, with no drop counters.
func TestAdmissionSeamA_AllowlistedPutMatchable(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)

	ks := exchange.NewKeySet(h.seller.pubKeyHex)
	tc, err := exchange.NewTrustChecker(h.operator.pubKeyHex, ks)
	if err != nil {
		t.Fatalf("NewTrustChecker: %v", err)
	}
	eng := h.newEngineWithOpts(func(o *exchange.EngineOptions) { o.TrustChecker = tc })

	admInjectPut(t, h, h.seller, "Postgres partial index design for soft-deleted rows", 8500)
	replayAll(t, h, eng)

	eng.RunAutoAccept(exchange.MaxTokenCost, time.Now(), map[string]struct{}{})

	if n := eng.MatchIndexLen(); n != 1 {
		t.Errorf("allowlisted put not matchable: match index len=%d, want 1", n)
	}
	if inv := eng.State().Inventory(); len(inv) != 1 {
		t.Errorf("allowlisted put not accepted into inventory: %d entries, want 1", len(inv))
	}
	d := eng.DegradationSnapshot()
	if d.DroppedUnlisted != 0 || d.DroppedLowReputation != 0 {
		t.Errorf("unexpected drops for admitted seller: unlisted=%d low_rep=%d", d.DroppedUnlisted, d.DroppedLowReputation)
	}
}

// TestAdmissionSeamA_BelowReputationFloorDropped: an ALLOWLISTED seller whose
// behavioral reputation is below the floor is blocked at promotion with the
// DISTINCT dropped_low_reputation counter — never matchable.
func TestAdmissionSeamA_BelowReputationFloorDropped(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	lowSeller := newTestAgent(t)

	ks := exchange.NewKeySet(lowSeller.pubKeyHex) // admitted, but low reputation
	tc, err := exchange.NewTrustChecker(h.operator.pubKeyHex, ks,
		exchange.WithReputationFloor(func(key string) int {
			if key == lowSeller.pubKeyHex {
				return 20 // below the floor
			}
			return 50
		}, 40))
	if err != nil {
		t.Fatalf("NewTrustChecker: %v", err)
	}
	eng := h.newEngineWithOpts(func(o *exchange.EngineOptions) { o.TrustChecker = tc })

	admInjectPut(t, h, lowSeller, "Redis cluster resharding runbook with slot migration steps", 8500)
	replayAll(t, h, eng)

	eng.RunAutoAccept(exchange.MaxTokenCost, time.Now(), map[string]struct{}{})

	if n := eng.MatchIndexLen(); n != 0 {
		t.Errorf("below-floor seller put entered match index (len=%d, want 0)", n)
	}
	if inv := eng.State().Inventory(); len(inv) != 0 {
		t.Errorf("below-floor seller put entered inventory (%d entries, want 0)", len(inv))
	}
	d := eng.DegradationSnapshot()
	if d.DroppedLowReputation != 1 {
		t.Errorf("dropped_low_reputation=%d, want 1", d.DroppedLowReputation)
	}
	if d.DroppedUnlisted != 0 {
		t.Errorf("dropped_unlisted=%d, want 0 (seller IS allowlisted, only reputation-blocked)", d.DroppedUnlisted)
	}
}

// TestAdmissionSeamCD_DeAllowlistWithheldAcrossRestart: the red-team blocker. A
// seller's accepted inventory is withheld the instant they are de-allowlisted
// (Seam C sets NeedsRevalidation + Seam D drops it from the index), AND stays
// withheld after a restart — where NeedsRevalidation resets to zero on Replay, so
// only the Seam D reload re-gate keeps it out of the searchable index.
func TestAdmissionSeamCD_DeAllowlistWithheldAcrossRestart(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	seller := h.seller

	ks := exchange.NewKeySet(seller.pubKeyHex)
	tc, err := exchange.NewTrustChecker(h.operator.pubKeyHex, ks)
	if err != nil {
		t.Fatalf("NewTrustChecker: %v", err)
	}
	eng := h.newEngineWithOpts(func(o *exchange.EngineOptions) { o.TrustChecker = tc })

	admInjectPut(t, h, seller, "Kafka consumer-group rebalance protocol deep dive", 8500)
	replayAll(t, h, eng)
	eng.RunAutoAccept(exchange.MaxTokenCost, time.Now(), map[string]struct{}{})

	inv := eng.State().Inventory()
	if len(inv) != 1 {
		t.Fatalf("precondition: expected 1 accepted entry, got %d", len(inv))
	}
	entryID := inv[0].EntryID
	if eng.MatchIndexLen() != 1 {
		t.Fatalf("precondition: entry should be matchable before de-allowlist, index len=%d", eng.MatchIndexLen())
	}
	if eng.State().EntryNeedsRevalidation(entryID) {
		t.Fatalf("precondition: entry should not need revalidation before de-allowlist")
	}

	// Runtime de-allowlist (Seam C + D driver).
	if n := eng.DeAllowlistSeller(seller.pubKeyHex); n != 1 {
		t.Errorf("DeAllowlistSeller flagged %d entries, want 1", n)
	}
	if !eng.State().EntryNeedsRevalidation(entryID) {
		t.Errorf("Seam C FAILED: de-allowlisted seller's entry not flagged NeedsRevalidation")
	}
	if eng.MatchIndexLen() != 0 {
		t.Errorf("Seam D FAILED (runtime): de-allowlisted seller's entry still in match index (len=%d)", eng.MatchIndexLen())
	}

	// Simulate a restart: a FRESH engine (new State, new match index) whose
	// config-reloaded allowlist no longer contains the seller AND whose durable
	// revocation tombstone (Config.RevokedSellers, dontguess-23c) DOES contain it —
	// this is exactly what serve loads at startup. NeedsRevalidation is
	// in-memory-only and resets to zero on Replay, so ONLY the Seam D reload re-gate
	// (now keyed on the revoked tombstone) can keep the entry withheld. This is the
	// "de-allowlist erased by restart" hole the seam closes — the anti-poisoning
	// invariant now rides an explicit persisted tombstone, not mere allowlist
	// absence, so an ephemeral seller's inventory is NOT collateral damage.
	ks2 := exchange.NewKeySet() // persisted allowlist minus the removed seller
	tc2, err := exchange.NewTrustChecker(h.operator.pubKeyHex, ks2)
	if err != nil {
		t.Fatalf("NewTrustChecker (restart): %v", err)
	}
	tc2.SetRevoked(seller.pubKeyHex) // startup loads Config.RevokedSellers
	eng2 := h.newEngineWithOpts(func(o *exchange.EngineOptions) { o.TrustChecker = tc2 })
	if err := eng2.ReplayAllForTest(); err != nil {
		t.Fatalf("restart replay: %v", err)
	}

	// The entry is replayed back into inventory (it is a real accepted put in the
	// log), but Seam D must keep it out of the searchable index and re-flag it.
	if eng2.MatchIndexLen() != 0 {
		t.Errorf("Seam D FAILED (restart): de-allowlisted seller's inventory re-entered the match index (len=%d)", eng2.MatchIndexLen())
	}
	if !eng2.State().EntryNeedsRevalidation(entryID) {
		t.Errorf("Seam D FAILED (restart): entry not re-flagged NeedsRevalidation after replay")
	}
}

// TestSeamD_RetainsUnadmittedSellerInventoryAcrossRestart is the dontguess-23c
// retention invariant — the counterpart to the anti-poisoning test above, and the
// data-loss bug it fixes. A seller who was admitted, had a put accepted, then is
// simply NOT re-admitted on the next restart (ephemeral agent, roster not
// reconstructed) but was NEVER de-allowlisted for cause, must keep their accepted
// inventory SEARCHABLE across restart. Retention is gated on the revocation
// tombstone, not on current allowlist membership, so a missing seller is not
// collateral damage. (This is exactly the 55-sellers-de-indexed-on-restart live
// incident.)
func TestSeamD_RetainsUnadmittedSellerInventoryAcrossRestart(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	seller := h.seller

	ks := exchange.NewKeySet(seller.pubKeyHex)
	tc, err := exchange.NewTrustChecker(h.operator.pubKeyHex, ks)
	if err != nil {
		t.Fatalf("NewTrustChecker: %v", err)
	}
	eng := h.newEngineWithOpts(func(o *exchange.EngineOptions) { o.TrustChecker = tc })

	admInjectPut(t, h, seller, "Raft leader election edge cases and lease safety", 8500)
	replayAll(t, h, eng)
	eng.RunAutoAccept(exchange.MaxTokenCost, time.Now(), map[string]struct{}{})
	if eng.MatchIndexLen() != 1 {
		t.Fatalf("precondition: accepted entry should be matchable, index len=%d", eng.MatchIndexLen())
	}

	// Restart: fresh engine whose allowlist does NOT contain the seller (ephemeral —
	// never re-admitted) and whose revocation tombstone is EMPTY (never revoked for
	// cause). The accepted inventory must be RETAINED and matchable.
	ks2 := exchange.NewKeySet() // seller absent from the reconstructed allowlist
	tc2, err := exchange.NewTrustChecker(h.operator.pubKeyHex, ks2)
	if err != nil {
		t.Fatalf("NewTrustChecker (restart): %v", err)
	}
	// NOTE: no SetRevoked — the seller was never de-allowlisted for cause.
	eng2 := h.newEngineWithOpts(func(o *exchange.EngineOptions) { o.TrustChecker = tc2 })
	if err := eng2.ReplayAllForTest(); err != nil {
		t.Fatalf("restart replay: %v", err)
	}

	if eng2.MatchIndexLen() != 1 {
		t.Errorf("RETENTION FAILED: unadmitted-but-not-revoked seller's inventory dropped on restart (index len=%d, want 1)", eng2.MatchIndexLen())
	}
	inv := eng2.State().Inventory()
	if len(inv) != 1 {
		t.Fatalf("expected 1 replayed entry, got %d", len(inv))
	}
	if eng2.State().EntryNeedsRevalidation(inv[0].EntryID) {
		t.Errorf("RETENTION FAILED: retained entry wrongly flagged NeedsRevalidation")
	}
}

// TestAdmissionSeamA_TrustRejectPurgesContentHashPoison: the dontguess-327 gap.
//
// applyPut registers EVERY put's content hash in contentHashIndex ZERO-TRUST
// during the fold, BEFORE the SEAM-A trust gate ever runs. Before the fix, a
// non-allowlisted seller's put was trust-rejected at promotion but its content
// hash was left squatting the index — so a later ALLOWLISTED seller putting the
// BYTE-IDENTICAL content (the exchange's designed high-reuse happy path) hit the
// dedup collision in applyPut and was silently dropped forever. This is a real
// griefing lever: an attacker squats the hash of content they can never sell to
// permanently block the seller who can.
//
// This test drives the REAL fold + REAL auto-accept ticker over a REAL
// TrustChecker (no mock of the gate) and proves: (1) the anon reject purges the
// poison so the allowlisted identical-content put becomes matchable inventory,
// and (2) the new dropped_dedup_poison counter increments on the purge.
func TestAdmissionSeamA_TrustRejectPurgesContentHashPoison(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	anon := newTestAgent(t)

	// Allowlist the legitimate seller only; anon is NOT admitted.
	ks := exchange.NewKeySet(h.seller.pubKeyHex)
	tc, err := exchange.NewTrustChecker(h.operator.pubKeyHex, ks)
	if err != nil {
		t.Fatalf("NewTrustChecker: %v", err)
	}
	eng := h.newEngineWithOpts(func(o *exchange.EngineOptions) { o.TrustChecker = tc })

	// Byte-identical content across both puts: same description AND same content
	// size → putPayload emits identical content bytes → identical SHA-256 hash.
	// This is the exact collision the dedup index keys on.
	const desc = "gRPC deadline propagation across service boundaries with worked examples"
	const tokenCost = 8500
	const contentSize = 24576

	// 1) Non-allowlisted anon seller puts the content. The fold stages it into
	//    pendingPuts AND registers its content hash zero-trust.
	anonPut := h.sendMessage(anon,
		putPayload(desc, "", "code", tokenCost, contentSize),
		[]string{exchange.TagPut, "exchange:content-type:code"},
		nil,
	)
	replayAll(t, h, eng)
	if len(eng.State().PendingPuts()) != 1 {
		t.Fatalf("precondition: expected anon put staged by the fold, got %d pending", len(eng.State().PendingPuts()))
	}

	// 2) Auto-accept ticker: SEAM-A trust-rejects the anon put. With the fix, the
	//    reject purges the poisoned content hash from contentHashIndex.
	eng.RunAutoAccept(exchange.MaxTokenCost, time.Now(), map[string]struct{}{})

	if inv := eng.State().Inventory(); len(inv) != 0 {
		t.Fatalf("anon put must not enter inventory, got %d entries", len(inv))
	}
	d := eng.DegradationSnapshot()
	if d.DroppedUnlisted != 1 {
		t.Errorf("dropped_unlisted=%d, want 1", d.DroppedUnlisted)
	}
	if d.DroppedDedupPoison != 1 {
		t.Errorf("dropped_dedup_poison=%d, want 1 (trust reject must purge + count the poisoned hash)", d.DroppedDedupPoison)
	}
	_ = anonPut

	// 3) ALLOWLISTED seller puts the BYTE-IDENTICAL content. Before the fix this
	//    hit the still-squatting hash and was silently dropped by applyPut. After
	//    the fix the hash was purged, so the put stages into pendingPuts.
	goodPut := h.sendMessage(h.seller,
		putPayload(desc, "", "code", tokenCost, contentSize),
		[]string{exchange.TagPut, "exchange:content-type:code"},
		nil,
	)
	replayAll(t, h, eng)

	foundGood := false
	for _, e := range eng.State().PendingPuts() {
		if e.PutMsgID == goodPut.ID {
			foundGood = true
		}
	}
	if !foundGood {
		t.Fatalf("REGRESSION (dontguess-327): allowlisted seller's identical-content put was blocked by the " +
			"non-purged poison hash — it never reached pendingPuts")
	}

	// 4) The allowlisted identical-content put promotes into matchable inventory.
	eng.RunAutoAccept(exchange.MaxTokenCost, time.Now(), map[string]struct{}{})
	if n := eng.MatchIndexLen(); n != 1 {
		t.Errorf("allowlisted identical-content put not matchable after poison purge: match index len=%d, want 1", n)
	}
	if inv := eng.State().Inventory(); len(inv) != 1 {
		t.Errorf("allowlisted identical-content put not accepted into inventory: %d entries, want 1", len(inv))
	}
}

// TestAdmissionIndividualTier_NilCheckerUnchanged: the individual / no-relay tier
// (TrustChecker=nil) is byte-for-byte unchanged — a put from ANY seller is
// promoted into matchable inventory (fail-open is correct there: the operator is
// the sole local writer). No drop counters ever increment.
func TestAdmissionIndividualTier_NilCheckerUnchanged(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine() // no TrustChecker

	anyone := newTestAgent(t)
	admInjectPut(t, h, anyone, "SQLite WAL checkpoint tuning notes for write-heavy loads", 8500)
	replayAll(t, h, eng)

	eng.RunAutoAccept(exchange.MaxTokenCost, time.Now(), map[string]struct{}{})

	if n := eng.MatchIndexLen(); n != 1 {
		t.Errorf("individual tier: put not matchable (index len=%d, want 1) — behavior changed", n)
	}
	if inv := eng.State().Inventory(); len(inv) != 1 {
		t.Errorf("individual tier: put not accepted (%d entries, want 1) — behavior changed", len(inv))
	}
	d := eng.DegradationSnapshot()
	if d.DroppedUnlisted != 0 || d.DroppedLowReputation != 0 {
		t.Errorf("individual tier must never drop: unlisted=%d low_rep=%d", d.DroppedUnlisted, d.DroppedLowReputation)
	}
}
