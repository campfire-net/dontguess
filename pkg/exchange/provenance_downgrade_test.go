package exchange_test

// Tests for provenance level downgrade behavior (dontguess-lqp).
//
// Chosen semantics: when a seller's provenance level drops below the level at
// which their inventory entries were accepted, the entries are flagged for
// re-validation (NeedsRevalidation=true) rather than purged. Flagged entries
// are excluded from buy match results until the operator clears the flag.
// See InventoryEntry.NeedsRevalidation for the full rationale.

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/campfire-net/campfire/pkg/provenance"
	"github.com/campfire-net/campfire/pkg/store"

	"github.com/3dl-dev/dontguess/pkg/exchange"
)

// makeDowngradeStore creates a provenance store where:
//
//	"key-contactable" → LevelContactable (2) — attested, stale (outside freshness window)
//
// The key starts at level 2 so we can test a downgrade by revoking the attestation.
func makeDowngradeStore(t *testing.T) *provenance.Store {
	t.Helper()
	cfg := provenance.DefaultConfig()
	cfg.FreshnessWindow = 7 * 24 * time.Hour
	cfg.TrustedVerifierKeys = map[string]int{"verifier-key": 0}
	ps := provenance.NewStore(cfg)

	// key-contactable: attested, stale → level 2
	stale := time.Now().Add(-30 * 24 * time.Hour) // 30 days ago — outside freshness window
	if err := ps.AddAttestation(&provenance.Attestation{
		ID:          "att-contactable",
		TargetKey:   "key-contactable",
		VerifierKey: "verifier-key",
		CoSigned:    true,
		VerifiedAt:  stale,
	}); err != nil {
		t.Fatalf("AddAttestation: %v", err)
	}

	// key-claimed: self-asserted → level 1
	ps.SetSelfClaimed("key-claimed")

	return ps
}

// TestProvenanceDowngrade_EntriesMarkedOnLevelDrop verifies that when
// MarkStaleProvenanceEntries is called with a lower level, entries whose
// AcceptedProvenanceLevel exceeds the current level are flagged
// NeedsRevalidation=true. This is the core of the dontguess-lqp fix.
func TestProvenanceDowngrade_EntriesMarkedOnLevelDrop(t *testing.T) {
	t.Parallel()
	ps := makeDowngradeStore(t)
	checker, err := exchange.NewProvenanceChecker(ps)
	if err != nil {
		t.Fatalf("NewProvenanceChecker: %v", err)
	}
	h := newTestHarness(t)
	eng := exchange.NewEngine(exchange.EngineOptions{
		CampfireID:        h.cfID,
		OperatorIdentity:  h.operator,
		Store:             h.st,
		Transport:         h.transport,
		ProvenanceChecker: checker,
		Logger: func(format string, args ...any) {
			t.Logf("[engine] "+format, args...)
		},
	})

	sellerKey := "key-contactable"

	// Send a put from a contactable seller (level 2 at time of put-accept).
	putMsg := h.sendMessage(h.seller,
		putPayload("Terraform module generator", "sha256:"+fmt.Sprintf("%064x", 99), "code", 8000, 12000),
		[]string{exchange.TagPut, "exchange:content-type:code"},
		nil,
	)
	// Override the sender in the record so it looks like sellerKey sent it.
	// We can't do this via sendMessage — instead we use injectPutMsg.
	_ = putMsg // discard unsigned put; use injected one below

	putRec := injectPutMsg(t, h, sellerKey)

	// Replay so state picks up the put.
	msgs, err := h.st.ListMessages(h.cfID, 0)
	if err != nil {
		t.Fatalf("listing messages: %v", err)
	}
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	// Accept the put. At this point the seller is at level 2 (contactable).
	// AutoAcceptPut records AcceptedProvenanceLevel=2 on the inventory entry.
	if err := eng.AutoAcceptPut(putRec.ID, 5000, time.Now().Add(72*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut: %v", err)
	}

	// Confirm entry is in inventory and NOT flagged yet.
	inv := eng.State().Inventory()
	var foundEntry *exchange.InventoryEntry
	for _, e := range inv {
		if e.EntryID == putRec.ID {
			cp := *e
			foundEntry = &cp
		}
	}
	if foundEntry == nil {
		t.Fatalf("entry %s not found in inventory after put-accept", putRec.ID)
	}
	if foundEntry.AcceptedProvenanceLevel != int(provenance.LevelContactable) {
		t.Errorf("AcceptedProvenanceLevel = %d, want %d (LevelContactable)",
			foundEntry.AcceptedProvenanceLevel, int(provenance.LevelContactable))
	}
	if foundEntry.NeedsRevalidation {
		t.Error("NeedsRevalidation should be false before any downgrade")
	}

	// Simulate a provenance downgrade: revoke the attestation so the seller
	// drops from level 2 to level 1 (claimed). Verify the current level is now lower.
	if err := ps.Revoke("att-contactable"); err != nil {
		t.Fatalf("Revoke attestation: %v", err)
	}
	currentLevel := int(ps.Level(sellerKey))
	if currentLevel >= int(provenance.LevelContactable) {
		t.Fatalf("expected level to drop below LevelContactable after revoke, got %d", currentLevel)
	}

	// Call MarkStaleProvenanceEntries with the seller's new (lower) level.
	// Entries accepted at a higher level should now be flagged.
	flagged := eng.State().MarkStaleProvenanceEntries(sellerKey, currentLevel)
	if len(flagged) != 1 || flagged[0] != putRec.ID {
		t.Errorf("MarkStaleProvenanceEntries = %v, want [%s]", flagged, putRec.ID)
	}

	// Confirm the flag is set.
	if !eng.State().EntryNeedsRevalidation(putRec.ID) {
		t.Error("EntryNeedsRevalidation should be true after downgrade mark")
	}
}

// TestProvenanceDowngrade_FlaggedEntryExcludedFromMatchResults verifies the
// enforcement: a NeedsRevalidation entry is not returned as a candidate for
// buy match results. Buyers should not receive flagged entries.
func TestProvenanceDowngrade_FlaggedEntryExcludedFromMatchResults(t *testing.T) {
	t.Parallel()
	ps := makeDowngradeStore(t)
	checker, err := exchange.NewProvenanceChecker(ps)
	if err != nil {
		t.Fatalf("NewProvenanceChecker: %v", err)
	}
	h := newTestHarness(t)
	eng := exchange.NewEngine(exchange.EngineOptions{
		CampfireID:        h.cfID,
		OperatorIdentity:  h.operator,
		Store:             h.st,
		Transport:         h.transport,
		ProvenanceChecker: checker,
		Logger: func(format string, args ...any) {
			t.Logf("[engine] "+format, args...)
		},
	})

	sellerKey := "key-contactable"

	// Inject and accept a put from the contactable seller.
	putRec := injectPutMsg(t, h, sellerKey)
	msgs, err := h.st.ListMessages(h.cfID, 0)
	if err != nil {
		t.Fatalf("listing messages: %v", err)
	}
	eng.State().Replay(exchange.FromStoreRecords(msgs))
	if err := eng.AutoAcceptPut(putRec.ID, 5000, time.Now().Add(72*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut: %v", err)
	}

	// Confirm entry is visible before downgrade: inventory must be non-empty.
	if len(eng.State().Inventory()) == 0 {
		t.Fatal("expected non-empty inventory after put-accept")
	}

	// Downgrade: revoke attestation → seller drops to level 1.
	if err := ps.Revoke("att-contactable"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	newLevel := int(ps.Level(sellerKey))

	// Send a buy that would match the flagged entry. Do this BEFORE marking
	// stale — we want to test that dispatch respects NeedsRevalidation.
	// We add the buy message to the store but apply it only via DispatchForTest.
	buyMsg := h.sendMessage(h.buyer,
		buyPayload("test entry", 9999),
		[]string{exchange.TagBuy},
		nil,
	)

	// Now mark entries stale. Do NOT replay after this — Replay would reset the
	// in-memory NeedsRevalidation flag (it is not persisted to the campfire log;
	// it is an ephemeral operator signal).
	eng.State().MarkStaleProvenanceEntries(sellerKey, newLevel)

	// Apply only the new buy message (not a full replay that would reset the flag).
	msgs, err = h.st.ListMessages(h.cfID, 0)
	if err != nil {
		t.Fatalf("listing messages: %v", err)
	}
	eng.State().Apply(exchange.FromStoreRecord(&msgs[len(msgs)-1]))

	// Dispatch the buy — the engine emits a match message but with zero results
	// because the only inventory entry is flagged for re-validation.
	if err := eng.DispatchForTest(buyMsg); err != nil {
		t.Errorf("DispatchForTest buy: %v", err)
	}

	// Verify the match message was emitted with zero results. The engine always
	// emits a match (to fulfill the buy future), but the results list must be empty
	// when all candidates are excluded by the NeedsRevalidation gate.
	matchMsgs, err := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})
	if err != nil {
		t.Fatalf("listing match messages: %v", err)
	}
	if len(matchMsgs) == 0 {
		t.Fatal("expected a match message (to fulfill the buy future), got none")
	}
	// Inspect the last match message — it should have an empty results list.
	lastMatch := matchMsgs[len(matchMsgs)-1]
	var matchPayload struct {
		Results []struct {
			EntryID string `json:"entry_id"`
		} `json:"results"`
	}
	if err := json.Unmarshal(lastMatch.Payload, &matchPayload); err != nil {
		t.Fatalf("unmarshal match payload: %v", err)
	}
	if len(matchPayload.Results) != 0 {
		t.Errorf("expected 0 match results (all flagged), got %d (first entry_id: %s)",
			len(matchPayload.Results), matchPayload.Results[0].EntryID)
	}
}

// TestProvenanceDowngrade_NoFlagWhenLevelUnchanged verifies that
// MarkStaleProvenanceEntries does not flag entries when the current level
// equals or exceeds the AcceptedProvenanceLevel — only genuine downgrades trigger.
func TestProvenanceDowngrade_NoFlagWhenLevelUnchanged(t *testing.T) {
	t.Parallel()
	ps := makeDowngradeStore(t)
	checker, err := exchange.NewProvenanceChecker(ps)
	if err != nil {
		t.Fatalf("NewProvenanceChecker: %v", err)
	}
	h := newTestHarness(t)
	eng := exchange.NewEngine(exchange.EngineOptions{
		CampfireID:        h.cfID,
		OperatorIdentity:  h.operator,
		Store:             h.st,
		Transport:         h.transport,
		ProvenanceChecker: checker,
		Logger: func(format string, args ...any) {
			t.Logf("[engine] "+format, args...)
		},
	})

	sellerKey := "key-contactable"
	putRec := injectPutMsg(t, h, sellerKey)
	msgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))
	if err := eng.AutoAcceptPut(putRec.ID, 5000, time.Now().Add(72*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut: %v", err)
	}

	// Call MarkStaleProvenanceEntries with the SAME level (no downgrade).
	currentLevel := int(ps.Level(sellerKey)) // still level 2
	flagged := eng.State().MarkStaleProvenanceEntries(sellerKey, currentLevel)
	if len(flagged) != 0 {
		t.Errorf("expected no entries flagged when level unchanged, got %v", flagged)
	}
	if eng.State().EntryNeedsRevalidation(putRec.ID) {
		t.Error("NeedsRevalidation should remain false when level unchanged")
	}
}
