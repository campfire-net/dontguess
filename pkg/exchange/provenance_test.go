package exchange_test

import (
	"errors"
	"testing"
	"time"

	"github.com/3dl-dev/dontguess/pkg/exchange"
	"github.com/campfire-net/campfire/pkg/provenance"
)

// makeStore returns a provenance.Store with a trusted verifier and a set of keys
// at specific levels:
//
//	"key-anon"        → LevelAnonymous (0) — no attestation
//	"key-claimed"     → LevelClaimed  (1) — self-claimed profile
//	"key-contactable" → LevelContactable (2) — attested by trusted verifier (stale)
//	"key-present"     → LevelPresent  (3) — attested by trusted verifier (fresh)
func makeStore(t *testing.T) *provenance.Store {
	t.Helper()

	cfg := provenance.DefaultConfig()
	cfg.FreshnessWindow = 7 * 24 * time.Hour
	// Register "verifier-key" as a trusted verifier (direct, depth=0).
	cfg.TrustedVerifierKeys = map[string]int{"verifier-key": 0}

	store := provenance.NewStore(cfg)

	// key-claimed: self-asserted profile → level 1
	store.SetSelfClaimed("key-claimed")

	// key-contactable: attested by trusted verifier but attestation is older than
	// freshness window → level 2 (contactable, not present)
	stale := time.Now().Add(-30 * 24 * time.Hour) // 30 days ago — outside 7-day window
	err := store.AddAttestation(&provenance.Attestation{
		ID:          "att-contactable",
		TargetKey:   "key-contactable",
		VerifierKey: "verifier-key",
		CoSigned:    true,
		VerifiedAt:  stale,
	})
	if err != nil {
		t.Fatalf("AddAttestation key-contactable: %v", err)
	}

	// key-present: attested by trusted verifier within freshness window → level 3
	fresh := time.Now().Add(-1 * time.Hour) // 1 hour ago — within 7-day window
	err = store.AddAttestation(&provenance.Attestation{
		ID:          "att-present",
		TargetKey:   "key-present",
		VerifierKey: "verifier-key",
		CoSigned:    true,
		VerifiedAt:  fresh,
	})
	if err != nil {
		t.Fatalf("AddAttestation key-present: %v", err)
	}

	return store
}

// TestRequiredLevel verifies the operation → provenance level mapping.
func TestRequiredLevel(t *testing.T) {
	cases := []struct {
		name     string
		op       exchange.Operation
		phase    exchange.SettlePhase
		expected provenance.Level
		wantErr  bool
	}{
		// anonymous operations
		{"buy", exchange.OperationBuy, "", provenance.LevelAnonymous, false},
		{"inventory-read", exchange.OperationInventoryRead, "", provenance.LevelAnonymous, false},
		{"price-history-read", exchange.OperationPriceHistoryRead, "", provenance.LevelAnonymous, false},

		// claimed operations
		{"put", exchange.OperationPut, "", provenance.LevelClaimed, false},

		// contactable operations
		{"assign", exchange.OperationAssign, "", provenance.LevelContactable, false},

		// present operations
		{"mint", exchange.OperationMint, "", provenance.LevelPresent, false},
		{"burn", exchange.OperationBurn, "", provenance.LevelPresent, false},
		{"rate-publish", exchange.OperationRatePublish, "", provenance.LevelPresent, false},
		{"convention-promote", exchange.OperationConventionPromote, "", provenance.LevelPresent, false},
		{"convention-supersede", exchange.OperationConventionSupersede, "", provenance.LevelPresent, false},
		{"match", exchange.OperationMatch, "", provenance.LevelPresent, false},

		// settle buyer phases (claimed)
		{"settle buyer-accept", exchange.OperationSettle, exchange.SettlePhaseBuyerAccept, provenance.LevelClaimed, false},
		{"settle buyer-reject", exchange.OperationSettle, exchange.SettlePhaseBuyerReject, provenance.LevelClaimed, false},
		{"settle complete", exchange.OperationSettle, exchange.SettlePhaseComplete, provenance.LevelClaimed, false},
		{"settle dispute", exchange.OperationSettle, exchange.SettlePhaseDispute, provenance.LevelClaimed, false},

		// settle operator phases (present)
		{"settle put-accept", exchange.OperationSettle, exchange.SettlePhasePutAccept, provenance.LevelPresent, false},
		{"settle put-reject", exchange.OperationSettle, exchange.SettlePhasePutReject, provenance.LevelPresent, false},
		{"settle deliver", exchange.OperationSettle, exchange.SettlePhaseDeliver, provenance.LevelPresent, false},

		// error cases
		{"settle without phase", exchange.OperationSettle, "", 0, true},
		{"unknown operation", "unknown-op", "", 0, true},
		{"settle unknown phase", exchange.OperationSettle, "made-up-phase", 0, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := exchange.RequiredLevel(tc.op, tc.phase)
			if tc.wantErr {
				if err == nil {
					t.Errorf("want error, got nil (level=%v)", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.expected {
				t.Errorf("got %v, want %v", got, tc.expected)
			}
		})
	}
}

// TestCheck_AnonymousRejectedForPut is the primary done-condition test from
// the item spec (dontguess-j9p): an anonymous agent sending a put is rejected
// with ErrInsufficientProvenance.
func TestCheck_AnonymousRejectedForPut(t *testing.T) {
	store := makeStore(t)
	checker, err := exchange.NewProvenanceChecker(store)
	if err != nil {
		t.Fatalf("NewProvenanceChecker: %v", err)
	}

	err = checker.Check("key-anon", exchange.OperationPut, "")
	if err == nil {
		t.Fatal("expected ErrInsufficientProvenance for anonymous put, got nil")
	}
	if !errors.Is(err, exchange.ErrInsufficientProvenance) {
		t.Errorf("expected ErrInsufficientProvenance, got: %v", err)
	}
}

// TestCheck_ClaimedSucceedsForPut is the secondary done-condition test: a
// claimed agent sending a put succeeds.
func TestCheck_ClaimedSucceedsForPut(t *testing.T) {
	store := makeStore(t)
	checker, err := exchange.NewProvenanceChecker(store)
	if err != nil {
		t.Fatalf("NewProvenanceChecker: %v", err)
	}

	err = checker.Check("key-claimed", exchange.OperationPut, "")
	if err != nil {
		t.Errorf("expected claimed sender to succeed for put, got: %v", err)
	}
}

// TestCheck_AnonymousAcceptedForBuy confirms anonymous agents can buy.
func TestCheck_AnonymousAcceptedForBuy(t *testing.T) {
	store := makeStore(t)
	checker, err := exchange.NewProvenanceChecker(store)
	if err != nil {
		t.Fatalf("NewProvenanceChecker: %v", err)
	}

	err = checker.Check("key-anon", exchange.OperationBuy, "")
	if err != nil {
		t.Errorf("expected anonymous sender to succeed for buy, got: %v", err)
	}
}

// TestCheck_ProvenanceLevelMatrix runs a matrix of sender levels × operations
// to confirm the gating is correct.
func TestCheck_ProvenanceLevelMatrix(t *testing.T) {
	store := makeStore(t)
	checker, err := exchange.NewProvenanceChecker(store)
	if err != nil {
		t.Fatalf("NewProvenanceChecker: %v", err)
	}

	cases := []struct {
		name      string
		senderKey string
		op        exchange.Operation
		phase     exchange.SettlePhase
		wantErr   bool
	}{
		// anonymous: allowed for buy and reads, blocked for everything else
		{"anon/buy ok", "key-anon", exchange.OperationBuy, "", false},
		{"anon/inventory-read ok", "key-anon", exchange.OperationInventoryRead, "", false},
		{"anon/put rejected", "key-anon", exchange.OperationPut, "", true},
		{"anon/assign rejected", "key-anon", exchange.OperationAssign, "", true},
		{"anon/mint rejected", "key-anon", exchange.OperationMint, "", true},
		{"anon/settle buyer-accept rejected", "key-anon", exchange.OperationSettle, exchange.SettlePhaseBuyerAccept, true},

		// claimed: allowed for buy, put, and buyer settle phases; blocked for contactable+
		{"claimed/buy ok", "key-claimed", exchange.OperationBuy, "", false},
		{"claimed/put ok", "key-claimed", exchange.OperationPut, "", false},
		{"claimed/settle buyer-accept ok", "key-claimed", exchange.OperationSettle, exchange.SettlePhaseBuyerAccept, false},
		{"claimed/settle complete ok", "key-claimed", exchange.OperationSettle, exchange.SettlePhaseComplete, false},
		{"claimed/settle dispute ok", "key-claimed", exchange.OperationSettle, exchange.SettlePhaseDispute, false},
		{"claimed/assign rejected", "key-claimed", exchange.OperationAssign, "", true},
		{"claimed/mint rejected", "key-claimed", exchange.OperationMint, "", true},
		{"claimed/settle put-accept rejected", "key-claimed", exchange.OperationSettle, exchange.SettlePhasePutAccept, true},

		// contactable: allowed for assign; blocked for present-only ops
		{"contactable/assign ok", "key-contactable", exchange.OperationAssign, "", false},
		{"contactable/put ok", "key-contactable", exchange.OperationPut, "", false},
		{"contactable/mint rejected", "key-contactable", exchange.OperationMint, "", true},
		{"contactable/settle put-accept rejected", "key-contactable", exchange.OperationSettle, exchange.SettlePhasePutAccept, true},

		// present: allowed for all operations
		{"present/put ok", "key-present", exchange.OperationPut, "", false},
		{"present/assign ok", "key-present", exchange.OperationAssign, "", false},
		{"present/mint ok", "key-present", exchange.OperationMint, "", false},
		{"present/settle put-accept ok", "key-present", exchange.OperationSettle, exchange.SettlePhasePutAccept, false},
		{"present/settle deliver ok", "key-present", exchange.OperationSettle, exchange.SettlePhaseDeliver, false},
		{"present/match ok", "key-present", exchange.OperationMatch, "", false},
		{"present/convention-promote ok", "key-present", exchange.OperationConventionPromote, "", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checker.Check(tc.senderKey, tc.op, tc.phase)
			if tc.wantErr && err == nil {
				t.Errorf("expected rejection, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("expected success, got: %v", err)
			}
			if tc.wantErr && err != nil && !errors.Is(err, exchange.ErrInsufficientProvenance) {
				t.Errorf("expected ErrInsufficientProvenance, got different error: %v", err)
			}
		})
	}
}

// TestNewProvenanceChecker_NilStoreReturnsError verifies that NewProvenanceChecker
// returns ErrNilProvenanceStore when passed a nil store (no longer panics).
func TestNewProvenanceChecker_NilStoreReturnsError(t *testing.T) {
	checker, err := exchange.NewProvenanceChecker(nil)
	if err == nil {
		t.Fatal("expected error for nil store, got nil")
	}
	if !errors.Is(err, exchange.ErrNilProvenanceStore) {
		t.Errorf("expected ErrNilProvenanceStore, got: %v", err)
	}
	if checker != nil {
		t.Errorf("expected nil checker on error, got non-nil")
	}
}
