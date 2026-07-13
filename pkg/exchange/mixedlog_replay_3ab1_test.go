package exchange

// mixedlog_replay_3ab1_test.go — the done-gate for dontguess-3ab1 (mixed-log
// Replay + legacy-plaintext grandfathering, cut over on the explicit "v" tag,
// docs/design/content-confidentiality-envelope-541.md §6.3, §7 Migration).
//
// The crux: dontguess-4bed's team-tier fail-closed guard DROPS legacy plaintext
// puts. That is correct for a LIVE put (block new plaintext injection) but WRONG
// for Replay of a MIXED HISTORICAL log — a pre-migration plaintext entry that was
// already accepted+broadcast would be silently LOST on the first restart. This
// test builds a MIXED log (legacy plaintext 3401 + v2 encrypted 3401), runs a
// full Replay/rebuild, and proves the replay-grandfathers-but-live-rejects
// distinction end to end.
//
// It reuses the REAL v2-envelope + legacy-put builders and the teamTierState
// helper from put_confidentiality_4bed_test.go (same package): REAL secp256k1
// identities, REAL nip44.Seal/Open, REAL ChaCha20-Poly1305 — nothing mocked.
//
// Proven:
//
//	(a) Replay completes WITHOUT error over the mixed log — every entry folds and
//	    promotes cleanly (inventory == the 3 accepted entries, pendingPuts empty);
//	(b) the v2 entry folds as CONFIDENTIAL inventory (WrappedCEKOperator set,
//	    CiphertextHash set, LegacyPlaintext == false);
//	(c) the legacy plaintext entries are GRANDFATHERED — present in inventory,
//	    LegacyPlaintext == true, no CEK wrap, and carrying a TTL expiry
//	    (ExpiresAt == PutTimestamp + LegacyGrandfatherTTL) — NOT dropped;
//	(d) after their TTL a grandfathered legacy entry AGES OUT (IsExpired() true);
//	(e) a LIVE legacy plaintext put (Apply, not Replay) is STILL REJECTED — the
//	    4bed fail-closed live enforcement is intact and the grandfather path is
//	    strictly replay-scoped.

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/3dl-dev/dontguess/pkg/identity"
)

// putAcceptMsg builds an operator-authored settle(put-accept) that promotes the
// referenced put from pendingPuts to inventory. expires_at is left EMPTY so a
// grandfathered legacy entry keeps the default LegacyGrandfatherTTL expiry set at
// fold time — this is what lets assertion (d) prove age-out deterministically.
func putAcceptMsg(t *testing.T, operatorHex, putMsgID string) *Message {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"price": int64(100), "expires_at": ""})
	if err != nil {
		t.Fatalf("marshal put-accept: %v", err)
	}
	return &Message{
		ID:          "accept-" + putMsgID,
		Sender:      operatorHex,
		Payload:     payload,
		Tags:        []string{TagSettle, TagPhasePrefix + SettlePhaseStrPutAccept},
		Antecedents: []string{putMsgID},
		Timestamp:   time.Now().UnixNano(),
	}
}

func TestReplay_MixedLog_GrandfathersLegacyButConfidentialV2(t *testing.T) {
	seller, err := identity.Generate()
	if err != nil {
		t.Fatalf("seller: %v", err)
	}
	operator, err := identity.Generate()
	if err != nil {
		t.Fatalf("operator: %v", err)
	}

	s := teamTierState(t, operator)
	// Set the operator key so the put-accept operator-sender guard is ACTIVE (a
	// realistic team-tier fold), not bypassed.
	s.OperatorKey = operator.PubKeyHex()

	// --- Build the MIXED historical log --------------------------------------
	// A pre-migration legacy plaintext entry, freshly put (within TTL). Distinct,
	// content-bearing descriptions/content so no dedup collision confounds folds.
	legacyFresh := legacyPlaintextPut(t, seller, "legacy fresh reusable go flock contention test pattern",
		[]byte("a pre-migration plaintext artifact already broadcast before the cutover"), 4242)

	// A pre-migration legacy plaintext entry put long ago — its PutTimestamp is
	// older than LegacyGrandfatherTTL, so the grandfather expiry lands in the
	// PAST and IsExpired() must report it as aged out.
	legacyOld := legacyPlaintextPut(t, seller, "legacy old runbook migration recipe symlink bridge step",
		[]byte("an ancient pre-migration plaintext runbook that should age out on rebuild"), 4242)
	legacyOld.Timestamp = time.Now().Add(-2 * LegacyGrandfatherTTL).UnixNano()

	// A new v2 confidential put — folds as confidential inventory (decrypt-then-gate).
	v2Put := v2PutMessage(t, seller, operator.PubKeyHex(), "confidential go flock contention test pattern",
		[]byte("a v2 confidential artifact whose plaintext must never touch the wire"), 4242)

	log := []*Message{
		legacyFresh,
		putAcceptMsg(t, operator.PubKeyHex(), legacyFresh.ID),
		legacyOld,
		putAcceptMsg(t, operator.PubKeyHex(), legacyOld.ID),
		v2Put,
		putAcceptMsg(t, operator.PubKeyHex(), v2Put.ID),
	}
	// Replay takes []Message by value.
	msgs := make([]Message, len(log))
	for i, m := range log {
		msgs[i] = *m
	}

	// --- (a) Replay completes without error over the mixed log ---------------
	s.Replay(msgs) // void; a panic or a wrong end-state is the only failure surface

	if n := len(s.PendingPuts()); n != 0 {
		t.Fatalf("(a) pendingPuts not drained after Replay: %d — a put failed to promote", n)
	}
	// All THREE entries folded+promoted into the raw inventory map — nothing was
	// dropped on the rebuild (GetInventoryEntry reads the raw map, unfiltered).
	for _, id := range []string{legacyFresh.ID, legacyOld.ID, v2Put.ID} {
		if s.GetInventoryEntry(id) == nil {
			t.Fatalf("(a) entry %q missing from inventory after Replay — a mixed-log entry was lost", id)
		}
	}
	// The LIVE snapshot excludes the aged-out legacyOld entry (Inventory() filters
	// IsExpired), so exactly 2 entries are live: legacyFresh + v2.
	if n := len(s.Inventory()); n != 2 {
		t.Fatalf("(a) live inventory = %d, want 2 (legacyFresh + v2; legacyOld aged out)", n)
	}

	// --- (b) v2 folds as CONFIDENTIAL inventory ------------------------------
	ev2 := s.GetInventoryEntry(v2Put.ID)
	if ev2 == nil {
		t.Fatalf("(b) v2 entry missing from inventory")
	}
	if ev2.WrappedCEKOperator == "" {
		t.Fatalf("(b) v2 entry has no WrappedCEKOperator — did not fold as confidential inventory")
	}
	if ev2.CiphertextHash == "" {
		t.Fatalf("(b) v2 entry has no CiphertextHash — confidential envelope not folded")
	}
	if ev2.LegacyPlaintext {
		t.Fatalf("(b) v2 entry is marked LegacyPlaintext — a confidential entry must never be grandfathered")
	}

	// --- (c) legacy plaintext entries are GRANDFATHERED, not dropped ---------
	efresh := s.GetInventoryEntry(legacyFresh.ID)
	if efresh == nil {
		t.Fatalf("(c) legacy fresh entry was DROPPED during Replay — pre-migration inventory lost on restart")
	}
	if !efresh.LegacyPlaintext {
		t.Fatalf("(c) legacy fresh entry not marked LegacyPlaintext — grandfather marker missing")
	}
	if efresh.WrappedCEKOperator != "" || efresh.CiphertextHash != "" {
		t.Fatalf("(c) legacy fresh entry carries CEK wrap / ciphertext hash (%q / %q) — legacy entries have neither",
			efresh.WrappedCEKOperator, efresh.CiphertextHash)
	}
	wantExpiry := time.Unix(0, legacyFresh.Timestamp).Add(LegacyGrandfatherTTL)
	if !efresh.ExpiresAt.Equal(wantExpiry) {
		t.Fatalf("(c) legacy fresh ExpiresAt = %v, want PutTimestamp+LegacyGrandfatherTTL = %v", efresh.ExpiresAt, wantExpiry)
	}
	if efresh.IsExpired() {
		t.Fatalf("(c) a freshly-put grandfathered entry (within TTL) reports expired — TTL math wrong")
	}

	// --- (d) after their TTL, grandfathered legacy entries age out -----------
	eold := s.GetInventoryEntry(legacyOld.ID)
	if eold == nil {
		t.Fatalf("(d) legacy old entry was DROPPED during Replay")
	}
	if !eold.LegacyPlaintext {
		t.Fatalf("(d) legacy old entry not marked LegacyPlaintext")
	}
	if !eold.IsExpired() {
		t.Fatalf("(d) a grandfathered entry put >TTL ago is NOT expired — it must age out via ExpiresAt (%v)", eold.ExpiresAt)
	}

	// --- (e) a LIVE legacy plaintext put is STILL REJECTED (4bed intact) -----
	// Distinct content so the drop is unambiguously the fail-closed live guard,
	// never the content-hash dedup index (which now holds the grandfathered hashes).
	livePlain := legacyPlaintextPut(t, seller, "a live downgrade attempt after migration",
		[]byte("brand-new cleartext a rogue client tries to inject after the cutover"), 4242)
	s.Apply(livePlain) // Apply, NOT Replay → s.replaying == false → no grandfathering

	if _, ok := s.GetPendingPut(livePlain.ID); ok {
		t.Fatalf("(e) a LIVE plaintext put FOLDED into pendingPuts — 4bed fail-closed live enforcement broken")
	}
	if e := s.GetInventoryEntry(livePlain.ID); e != nil {
		t.Fatalf("(e) a LIVE plaintext put reached inventory — grandfathering leaked into the live path")
	}
	if n := len(s.Inventory()); n != 2 {
		t.Fatalf("(e) live inventory changed after a rejected live plaintext put: %d, want 2", n)
	}
}
