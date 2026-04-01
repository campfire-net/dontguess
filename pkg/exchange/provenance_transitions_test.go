package exchange_test

// Tests for provenance level transitions: revocation and freshness expiry (dontguess-hic).
//
// Two distinct transitions are tested here at the provenance.Store layer:
//
//  1. Revocation (verified → revoked): when an attestation is revoked via
//     Store.Revoke, the key's level drops immediately to the highest level
//     it can reach without that attestation (typically LevelAnonymous or
//     LevelClaimed if self-claimed).
//
//  2. Freshness expiry (present → contactable): an attestation that falls
//     outside the FreshnessWindow no longer elevates the key to LevelPresent.
//     The key stays at LevelContactable (attested but stale). Testing this
//     via LevelAt with a future timestamp avoids real-time waits.
//
// These tests exercise the provenance.Store transitions that drive the
// exchange's provenance gating (ProvenanceChecker.Check) and the downgrade
// detection path (MarkStaleProvenanceEntries). They are a prerequisite for
// reasoning about correctness of those higher-level mechanisms.

import (
	"errors"
	"testing"
	"time"

	"github.com/campfire-net/campfire/pkg/provenance"

	"github.com/campfire-net/dontguess/pkg/exchange"
)

// makeTransitionStore returns a provenance.Store with:
//
//	FreshnessWindow = 7 days
//	trusted verifier: "verifier-key"
//	"key-present" → LevelPresent (3) — attested fresh (1 hour ago), also self-claimed
//
// Returns the store and the attestation ID for use in revocation tests.
func makeTransitionStore(t *testing.T) (*provenance.Store, string) {
	t.Helper()
	const freshnessWindow = 7 * 24 * time.Hour
	const attestID = "att-present-transition"

	cfg := provenance.DefaultConfig()
	cfg.FreshnessWindow = freshnessWindow
	cfg.TrustedVerifierKeys = map[string]int{"verifier-key": 0}
	ps := provenance.NewStore(cfg)

	// key-present: fresh attestation → level 3
	fresh := time.Now().Add(-1 * time.Hour)
	if err := ps.AddAttestation(&provenance.Attestation{
		ID:          attestID,
		TargetKey:   "key-present",
		VerifierKey: "verifier-key",
		CoSigned:    true,
		VerifiedAt:  fresh,
	}); err != nil {
		t.Fatalf("AddAttestation key-present: %v", err)
	}

	// Also self-claim key-present so it falls back to LevelClaimed (not LevelAnonymous)
	// after the attestation is revoked.
	ps.SetSelfClaimed("key-present")

	return ps, attestID
}

// TestProvenanceTransition_RevocationDropsLevel verifies that revoking an
// attestation immediately lowers the key's level.
//
// Before revoke: key-present is at LevelPresent (3) — fresh, co-signed.
// After revoke:  key-present drops to LevelClaimed (1) — self-claimed, no valid attestation.
func TestProvenanceTransition_RevocationDropsLevel(t *testing.T) {
	t.Parallel()
	ps, attestID := makeTransitionStore(t)

	// Pre-condition: key is at LevelPresent.
	before := ps.Level("key-present")
	if before != provenance.LevelPresent {
		t.Fatalf("pre-revoke level = %s, want LevelPresent", before)
	}

	// Revoke the attestation.
	if err := ps.Revoke(attestID); err != nil {
		t.Fatalf("Revoke(%q): %v", attestID, err)
	}

	// Post-condition: key drops below LevelContactable (self-claim remains, so not LevelAnonymous).
	after := ps.Level("key-present")
	if after >= provenance.LevelContactable {
		t.Errorf("post-revoke level = %s, want below LevelContactable (expected LevelClaimed)", after)
	}
	if after < provenance.LevelAnonymous {
		t.Errorf("post-revoke level = %s, want at least LevelAnonymous", after)
	}
}

// TestProvenanceTransition_RevocationIsImmediate verifies that the level
// transition takes effect on the very next Level() call with no additional
// triggering required. This ensures ProvenanceChecker.Check re-evaluates
// the level synchronously on every operation rather than caching stale state.
func TestProvenanceTransition_RevocationIsImmediate(t *testing.T) {
	t.Parallel()
	ps, attestID := makeTransitionStore(t)

	levelBefore := ps.Level("key-present")
	if err := ps.Revoke(attestID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	levelAfter := ps.Level("key-present")

	if levelAfter >= levelBefore {
		t.Errorf("level did not drop after revoke: before=%s after=%s", levelBefore, levelAfter)
	}
}

// TestProvenanceTransition_FreshnessExpiry verifies that a key drops from
// LevelPresent to LevelContactable once its attestation ages beyond the
// FreshnessWindow. This is tested deterministically via LevelAt.
//
// Before expiry (verifiedAt+1h):   LevelPresent (3) — within 7-day window
// After expiry  (verifiedAt+8d):   LevelContactable (2) — attestation exists but is stale
func TestProvenanceTransition_FreshnessExpiry(t *testing.T) {
	t.Parallel()

	const freshnessWindow = 7 * 24 * time.Hour
	verifiedAt := time.Now().Add(-1 * time.Hour) // 1 hour ago — fresh

	cfg := provenance.DefaultConfig()
	cfg.FreshnessWindow = freshnessWindow
	cfg.TrustedVerifierKeys = map[string]int{"verifier-key": 0}
	ps := provenance.NewStore(cfg)

	if err := ps.AddAttestation(&provenance.Attestation{
		ID:          "att-freshness-test",
		TargetKey:   "key-freshness",
		VerifierKey: "verifier-key",
		CoSigned:    true,
		VerifiedAt:  verifiedAt,
	}); err != nil {
		t.Fatalf("AddAttestation: %v", err)
	}

	// At verifiedAt + 1 hour: key is present (within 7-day window).
	tFresh := verifiedAt.Add(1 * time.Hour)
	levelFresh := ps.LevelAt("key-freshness", tFresh)
	if levelFresh != provenance.LevelPresent {
		t.Errorf("level at t=+1h = %s, want LevelPresent", levelFresh)
	}

	// At verifiedAt + 8 days: attestation is stale (outside 7-day window).
	tStale := verifiedAt.Add(8 * 24 * time.Hour)
	levelStale := ps.LevelAt("key-freshness", tStale)
	if levelStale != provenance.LevelContactable {
		t.Errorf("level at t=+8d = %s, want LevelContactable (stale attestation)", levelStale)
	}
}

// TestProvenanceTransition_FreshnessExpiryAtBoundary verifies the exact boundary
// condition. The store uses strictly-greater-than for staleness (age > window),
// so at exactly FreshnessWindow the key is still present, and one nanosecond
// past the boundary it becomes stale (contactable only).
func TestProvenanceTransition_FreshnessExpiryAtBoundary(t *testing.T) {
	t.Parallel()

	const freshnessWindow = 7 * 24 * time.Hour
	verifiedAt := time.Now().Add(-1 * time.Hour)

	cfg := provenance.DefaultConfig()
	cfg.FreshnessWindow = freshnessWindow
	cfg.TrustedVerifierKeys = map[string]int{"verifier-key": 0}
	ps := provenance.NewStore(cfg)

	if err := ps.AddAttestation(&provenance.Attestation{
		ID:          "att-boundary-test",
		TargetKey:   "key-boundary",
		VerifierKey: "verifier-key",
		CoSigned:    true,
		VerifiedAt:  verifiedAt,
	}); err != nil {
		t.Fatalf("AddAttestation: %v", err)
	}

	// At exactly the freshness window boundary: store uses age > window,
	// so age == window is still considered fresh (present).
	tAtBoundary := verifiedAt.Add(freshnessWindow)
	levelAtBoundary := ps.LevelAt("key-boundary", tAtBoundary)
	if levelAtBoundary != provenance.LevelPresent {
		t.Errorf("level at exact boundary = %s, want LevelPresent (store uses strictly-greater-than)", levelAtBoundary)
	}

	// One nanosecond past the boundary: attestation age exceeds the window → stale.
	tJustAfter := verifiedAt.Add(freshnessWindow + time.Nanosecond)
	levelJustAfter := ps.LevelAt("key-boundary", tJustAfter)
	if levelJustAfter != provenance.LevelContactable {
		t.Errorf("level just past boundary = %s, want LevelContactable (attestation stale)", levelJustAfter)
	}
}

// TestProvenanceTransition_RevocationWithSelfClaimFallback verifies that a key
// with both a self-claim and an attestation falls back to LevelClaimed (not
// LevelAnonymous) after the attestation is revoked.
func TestProvenanceTransition_RevocationWithSelfClaimFallback(t *testing.T) {
	t.Parallel()
	ps, attestID := makeTransitionStore(t)

	// key-present has both self-claim and a fresh attestation (set in makeTransitionStore).
	before := ps.Level("key-present")
	if before != provenance.LevelPresent {
		t.Fatalf("pre-revoke = %s, want LevelPresent", before)
	}

	if err := ps.Revoke(attestID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	// After revoke: self-claim remains → level at least LevelClaimed (1),
	// but attestation is gone → level below LevelContactable (2).
	after := ps.Level("key-present")
	if after < provenance.LevelClaimed {
		t.Errorf("post-revoke = %s, expected at least LevelClaimed (self-claim should persist)", after)
	}
	if after >= provenance.LevelContactable {
		t.Errorf("post-revoke = %s, expected below LevelContactable (attestation revoked)", after)
	}
}

// TestProvenanceTransition_RevocationAnonymousFallback verifies that a key
// with ONLY an attestation (no self-claim) drops to LevelAnonymous after revoke.
func TestProvenanceTransition_RevocationAnonymousFallback(t *testing.T) {
	t.Parallel()

	cfg := provenance.DefaultConfig()
	cfg.FreshnessWindow = 7 * 24 * time.Hour
	cfg.TrustedVerifierKeys = map[string]int{"verifier-key": 0}
	ps := provenance.NewStore(cfg)

	// key-no-claim: fresh attestation, no self-claim → level 3 initially
	if err := ps.AddAttestation(&provenance.Attestation{
		ID:          "att-no-claim",
		TargetKey:   "key-no-claim",
		VerifierKey: "verifier-key",
		CoSigned:    true,
		VerifiedAt:  time.Now().Add(-1 * time.Hour),
	}); err != nil {
		t.Fatalf("AddAttestation: %v", err)
	}

	if ps.Level("key-no-claim") != provenance.LevelPresent {
		t.Fatalf("pre-revoke: expected LevelPresent, got %s", ps.Level("key-no-claim"))
	}

	if err := ps.Revoke("att-no-claim"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	// No self-claim → falls all the way to LevelAnonymous.
	after := ps.Level("key-no-claim")
	if after != provenance.LevelAnonymous {
		t.Errorf("post-revoke (no self-claim) = %s, want LevelAnonymous", after)
	}
}

// TestProvenanceTransition_FreshnessMaintainedOnFreshReattestation verifies
// that after a freshness-based degradation, a new (fresh) attestation restores
// the key to LevelPresent. Stale is not a permanent state.
func TestProvenanceTransition_FreshnessMaintainedOnFreshReattestation(t *testing.T) {
	t.Parallel()

	const freshnessWindow = 7 * 24 * time.Hour

	cfg := provenance.DefaultConfig()
	cfg.FreshnessWindow = freshnessWindow
	cfg.TrustedVerifierKeys = map[string]int{"verifier-key": 0}
	ps := provenance.NewStore(cfg)

	// First attestation: 30 days ago — stale → level 2 (contactable)
	if err := ps.AddAttestation(&provenance.Attestation{
		ID:          "att-stale",
		TargetKey:   "key-reattest",
		VerifierKey: "verifier-key",
		CoSigned:    true,
		VerifiedAt:  time.Now().Add(-30 * 24 * time.Hour),
	}); err != nil {
		t.Fatalf("AddAttestation stale: %v", err)
	}

	if got := ps.Level("key-reattest"); got != provenance.LevelContactable {
		t.Fatalf("after stale attestation: level = %s, want LevelContactable", got)
	}

	// Second attestation: 1 hour ago — fresh → elevates to level 3
	if err := ps.AddAttestation(&provenance.Attestation{
		ID:          "att-fresh-reattest",
		TargetKey:   "key-reattest",
		VerifierKey: "verifier-key",
		CoSigned:    true,
		VerifiedAt:  time.Now().Add(-1 * time.Hour),
	}); err != nil {
		t.Fatalf("AddAttestation fresh: %v", err)
	}

	if got := ps.Level("key-reattest"); got != provenance.LevelPresent {
		t.Errorf("after fresh re-attestation: level = %s, want LevelPresent", got)
	}
}

// TestProvenanceTransition_CheckRejectsAfterRevocation verifies the integration
// between provenance.Store transitions and ProvenanceChecker.Check: after a
// revocation drops a seller below LevelClaimed, a put operation is rejected.
//
// This is the end-to-end transition gate: the store's level transition feeds
// directly into the checker's decision, with no caching gap between them.
func TestProvenanceTransition_CheckRejectsAfterRevocation(t *testing.T) {
	t.Parallel()

	cfg := provenance.DefaultConfig()
	cfg.FreshnessWindow = 7 * 24 * time.Hour
	cfg.TrustedVerifierKeys = map[string]int{"verifier-key": 0}
	ps := provenance.NewStore(cfg)

	// key-seller: fresh attestation, no self-claim → level 3 before revoke
	if err := ps.AddAttestation(&provenance.Attestation{
		ID:          "att-seller",
		TargetKey:   "key-seller",
		VerifierKey: "verifier-key",
		CoSigned:    true,
		VerifiedAt:  time.Now().Add(-1 * time.Hour),
	}); err != nil {
		t.Fatalf("AddAttestation: %v", err)
	}

	checker, err := exchange.NewProvenanceChecker(ps)
	if err != nil {
		t.Fatalf("NewProvenanceChecker: %v", err)
	}

	// Before revoke: put is allowed (level 3 ≥ LevelClaimed).
	if err := checker.Check("key-seller", exchange.OperationPut, ""); err != nil {
		t.Errorf("Check(put) before revoke: got error %v, want nil", err)
	}

	// Revoke the attestation → drops to LevelAnonymous (no self-claim).
	if err := ps.Revoke("att-seller"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	// After revoke: put is rejected (LevelAnonymous < LevelClaimed).
	checkErr := checker.Check("key-seller", exchange.OperationPut, "")
	if checkErr == nil {
		t.Error("Check(put) after revoke: got nil, want ErrInsufficientProvenance")
		return
	}
	if !errors.Is(checkErr, exchange.ErrInsufficientProvenance) {
		t.Errorf("Check(put) after revoke: got %v, want ErrInsufficientProvenance", checkErr)
	}
}
