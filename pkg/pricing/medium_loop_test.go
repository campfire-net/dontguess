package pricing_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/3dl-dev/dontguess/pkg/exchange"
	"github.com/3dl-dev/dontguess/pkg/pricing"
	"github.com/3dl-dev/dontguess/pkg/scrip"
)

// =============================================================================
// Stub implementations
// =============================================================================

// mediumStubState implements MediumStateReadWriter for medium loop tests.
type mediumStubState struct {
	mu                 sync.Mutex
	inventory          []*exchange.InventoryEntry
	history            []exchange.PriceRecord
	adjustments        map[string]exchange.PriceAdjustment
	reputation         map[string]int
	demandCount        map[string]int
	purchaseCount      map[string]int
	compressedVersions map[string]bool // entryID → has a compressed derivative
	activeAssigns      map[string][]*exchange.AssignRecord
}

func newMediumStubState() *mediumStubState {
	return &mediumStubState{
		adjustments:        make(map[string]exchange.PriceAdjustment),
		reputation:         make(map[string]int),
		demandCount:        make(map[string]int),
		purchaseCount:      make(map[string]int),
		compressedVersions: make(map[string]bool),
		activeAssigns:      make(map[string][]*exchange.AssignRecord),
	}
}

func (s *mediumStubState) Inventory() []*exchange.InventoryEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inventory
}

func (s *mediumStubState) PriceHistory() []exchange.PriceRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.history
}

func (s *mediumStubState) AllPriceAdjustments() map[string]exchange.PriceAdjustment {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]exchange.PriceAdjustment, len(s.adjustments))
	for k, v := range s.adjustments {
		if !v.IsExpired() && v.Multiplier > 0 {
			out[k] = v
		}
	}
	return out
}

func (s *mediumStubState) SellerReputation(sellerKey string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.reputation[sellerKey]; ok {
		return r
	}
	return exchange.DefaultReputation
}

func (s *mediumStubState) EntryDemandCount(entryID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.demandCount[entryID]
}

func (s *mediumStubState) AllSellerKeys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := make(map[string]struct{})
	for _, e := range s.inventory {
		seen[e.SellerKey] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	return out
}

func (s *mediumStubState) HasCompressedVersion(entryID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.compressedVersions[entryID]
}

func (s *mediumStubState) PurchaseCount(entryID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.purchaseCount[entryID]
}

func (s *mediumStubState) ActiveAssigns(entryID string) []*exchange.AssignRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	recs := s.activeAssigns[entryID]
	out := make([]*exchange.AssignRecord, len(recs))
	copy(out, recs)
	return out
}

func (s *mediumStubState) SetPriceAdjustment(entryID string, adj exchange.PriceAdjustment) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.adjustments[entryID] = adj
}

// setAdj is a test helper to pre-populate adjustments without expiry.
func (s *mediumStubState) setAdj(entryID string, multiplier float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.adjustments[entryID] = exchange.PriceAdjustment{
		Multiplier: multiplier,
		// Zero ExpiresAt = never expires.
	}
}

// =============================================================================
// Stub ScripStore
// =============================================================================

// stubScripStore records AddBudget calls made by the medium loop.
type stubScripStore struct {
	mu       sync.Mutex
	payments []residualPayment
	failKeys map[string]bool // keys for which AddBudget should fail
}

type residualPayment struct {
	pk     string
	rk     string
	amount int64
}

func newStubScripStore() *stubScripStore {
	return &stubScripStore{failKeys: make(map[string]bool)}
}

func (s *stubScripStore) AddBudget(_ context.Context, pk, rk string, amount int64, _ string) (int64, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failKeys[pk] {
		return 0, "", scrip.ErrConflict
	}
	s.payments = append(s.payments, residualPayment{pk: pk, rk: rk, amount: amount})
	return amount, "etag1", nil
}

// Stub out the remaining SpendingStore methods (unused by the medium loop).
func (s *stubScripStore) DecrementBudget(_ context.Context, _, _ string, _ int64, _ string) (int64, string, error) {
	return 0, "", nil
}
func (s *stubScripStore) GetBudget(_ context.Context, _, _ string) (int64, string, error) {
	return 0, "", nil
}
func (s *stubScripStore) SaveReservation(_ context.Context, _ scrip.Reservation) error {
	return nil
}
func (s *stubScripStore) GetReservation(_ context.Context, _ string) (scrip.Reservation, error) {
	return scrip.Reservation{}, scrip.ErrReservationNotFound
}
func (s *stubScripStore) DeleteReservation(_ context.Context, _ string) error {
	return nil
}
func (s *stubScripStore) ConsumeReservation(_ context.Context, _ string) (scrip.Reservation, error) {
	return scrip.Reservation{}, scrip.ErrReservationNotFound
}

// =============================================================================
// Cluster correction tests
// =============================================================================

// TestMediumLoop_ClusterCorrection_DampensOutlier verifies that an outlier
// adjustment in a content-type cluster with ≥ MinClusterSize entries is
// pulled toward the cluster mean.
func TestMediumLoop_ClusterCorrection_DampensOutlier(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	st := newMediumStubState()

	// 5 entries of content type "code" — forms a cluster.
	// Most at ~1.0x, one outlier at 2.0x (MaxMultiplier).
	for i, mult := range []float64{1.0, 1.05, 0.95, 1.02, 2.0} {
		id := "entry-code-" + string(rune('A'+i))
		st.inventory = append(st.inventory, &exchange.InventoryEntry{
			EntryID:     id,
			SellerKey:   "seller-1",
			ContentType: "code",
		})
		st.setAdj(id, mult)
	}

	loop := pricing.NewMediumLoop(pricing.MediumLoopOptions{
		State: st,
		Now:   func() time.Time { return now },
	})

	result := loop.Tick(context.Background())

	if result.ClusterCorrections == 0 {
		t.Fatal("expected at least one cluster correction for the 2.0x outlier")
	}

	// The outlier (entry-code-E) should be dampened toward cluster mean.
	// Cluster mean ≈ (1.0 + 1.05 + 0.95 + 1.02 + 2.0) / 5 ≈ 1.204.
	// After 50% dampening: 2.0 + 0.5*(1.204 - 2.0) ≈ 1.602.
	adj, ok := st.adjustments["entry-code-E"]
	if !ok {
		t.Fatal("expected adjustment for entry-code-E after correction")
	}
	if adj.Multiplier >= 2.0 {
		t.Errorf("outlier should be dampened below 2.0, got %.4f", adj.Multiplier)
	}
	if adj.Multiplier < pricing.MinMultiplier {
		t.Errorf("dampened value %.4f below MinMultiplier %.4f", adj.Multiplier, pricing.MinMultiplier)
	}
}

// TestMediumLoop_ClusterCorrection_SmallClusterUntouched verifies that clusters
// smaller than MinClusterSize are not corrected.
func TestMediumLoop_ClusterCorrection_SmallClusterUntouched(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	st := newMediumStubState()

	// Only 2 entries (below MinClusterSize=3). Even with a big outlier, no correction.
	for i, mult := range []float64{1.0, 2.0} {
		id := "entry-summary-" + string(rune('A'+i))
		st.inventory = append(st.inventory, &exchange.InventoryEntry{
			EntryID:     id,
			SellerKey:   "seller-1",
			ContentType: "summary",
		})
		st.setAdj(id, mult)
	}

	loop := pricing.NewMediumLoop(pricing.MediumLoopOptions{
		State: st,
		Now:   func() time.Time { return now },
	})

	result := loop.Tick(context.Background())

	if result.ClusterCorrections != 0 {
		t.Errorf("expected 0 cluster corrections for small cluster, got %d", result.ClusterCorrections)
	}

	// The 2.0x entry should be unchanged.
	adj := st.adjustments["entry-summary-B"]
	if adj.Multiplier != 2.0 {
		t.Errorf("small-cluster outlier should be unchanged at 2.0, got %.4f", adj.Multiplier)
	}
}

// TestMediumLoop_ClusterCorrection_InlierUntouched verifies that entries within
// one standard deviation of the cluster mean are not corrected.
func TestMediumLoop_ClusterCorrection_InlierUntouched(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	st := newMediumStubState()

	// 4 entries with very similar multipliers — no meaningful outliers.
	// Mean = 1.1, stddev ≈ 0.007 → all entries within 1 stddev.
	for i, mult := range []float64{1.1, 1.1, 1.09, 1.11} {
		id := "entry-plan-" + string(rune('A'+i))
		st.inventory = append(st.inventory, &exchange.InventoryEntry{
			EntryID:     id,
			SellerKey:   "seller-1",
			ContentType: "plan",
		})
		st.setAdj(id, mult)
	}

	loop := pricing.NewMediumLoop(pricing.MediumLoopOptions{
		State: st,
		Now:   func() time.Time { return now },
	})

	result := loop.Tick(context.Background())

	if result.ClusterCorrections != 0 {
		t.Errorf("expected 0 corrections for tight cluster, got %d", result.ClusterCorrections)
	}
}

// =============================================================================
// Residual settlement tests
// =============================================================================

// TestMediumLoop_ResidualSettlement_PaysResidual verifies that completed sales
// within the window result in residual payments to the seller.
func TestMediumLoop_ResidualSettlement_PaysResidual(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	st := newMediumStubState()
	ss := newStubScripStore()

	st.inventory = []*exchange.InventoryEntry{
		{EntryID: "entry-1", SellerKey: "seller-alpha", ContentType: "code"},
	}

	// 3 sales in the last hour, each at price 100. Total revenue = 300.
	// Residual = 300 / ResidualRate (10) = 30.
	for i := range 3 {
		ts := now.Add(-time.Duration(i+1) * 15 * time.Minute)
		st.history = append(st.history, exchange.PriceRecord{
			EntryID:   "entry-1",
			SalePrice: 100,
			Timestamp: ts.UnixNano(),
		})
	}

	loop := pricing.NewMediumLoop(pricing.MediumLoopOptions{
		State:      st,
		ScripStore: ss,
		Now:        func() time.Time { return now },
	})

	result := loop.Tick(context.Background())

	if result.ResidualsPaid != 1 {
		t.Errorf("expected 1 residual payment, got %d", result.ResidualsPaid)
	}

	expectedScrip := int64(300 / exchange.ResidualRate) // 30
	if result.TotalResidualScrip != expectedScrip {
		t.Errorf("expected total scrip %d, got %d", expectedScrip, result.TotalResidualScrip)
	}

	if len(ss.payments) != 1 {
		t.Fatalf("expected 1 payment to scrip store, got %d", len(ss.payments))
	}
	if ss.payments[0].pk != "seller-alpha" {
		t.Errorf("expected payment to seller-alpha, got %s", ss.payments[0].pk)
	}
	if ss.payments[0].amount != expectedScrip {
		t.Errorf("expected payment amount %d, got %d", expectedScrip, ss.payments[0].amount)
	}
}

// TestMediumLoop_ResidualSettlement_NoDuplicatePayment verifies that a second
// Tick call does not re-pay residuals already processed in the first tick.
func TestMediumLoop_ResidualSettlement_NoDuplicatePayment(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	st := newMediumStubState()
	ss := newStubScripStore()

	st.inventory = []*exchange.InventoryEntry{
		{EntryID: "entry-2", SellerKey: "seller-beta", ContentType: "analysis"},
	}

	ts := now.Add(-30 * time.Minute)
	st.history = append(st.history, exchange.PriceRecord{
		EntryID:   "entry-2",
		SalePrice: 200,
		Timestamp: ts.UnixNano(),
	})

	loop := pricing.NewMediumLoop(pricing.MediumLoopOptions{
		State:      st,
		ScripStore: ss,
		Now:        func() time.Time { return now },
	})

	// First tick — should pay residual once.
	result1 := loop.Tick(context.Background())
	if result1.ResidualsPaid != 1 {
		t.Errorf("first tick: expected 1 payment, got %d", result1.ResidualsPaid)
	}

	// Second tick — same history, same loop instance. Should NOT re-pay.
	result2 := loop.Tick(context.Background())
	if result2.ResidualsPaid != 0 {
		t.Errorf("second tick: expected 0 payments (dedup), got %d", result2.ResidualsPaid)
	}

	if len(ss.payments) != 1 {
		t.Errorf("expected exactly 1 total payment across both ticks, got %d", len(ss.payments))
	}
}

// TestMediumLoop_ResidualSettlement_OutsideWindowIgnored verifies that sales
// older than the lookback window are not counted for residuals.
func TestMediumLoop_ResidualSettlement_OutsideWindowIgnored(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	st := newMediumStubState()
	ss := newStubScripStore()

	st.inventory = []*exchange.InventoryEntry{
		{EntryID: "entry-3", SellerKey: "seller-gamma", ContentType: "data"},
	}

	// Sales older than DefaultMediumLoopWindow (2h).
	for i := range 3 {
		ts := now.Add(-time.Duration(3+i) * time.Hour) // 3h, 4h, 5h ago
		st.history = append(st.history, exchange.PriceRecord{
			EntryID:   "entry-3",
			SalePrice: 100,
			Timestamp: ts.UnixNano(),
		})
	}

	loop := pricing.NewMediumLoop(pricing.MediumLoopOptions{
		State:      st,
		ScripStore: ss,
		Now:        func() time.Time { return now },
	})

	result := loop.Tick(context.Background())

	if result.ResidualsPaid != 0 {
		t.Errorf("expected 0 payments for out-of-window sales, got %d", result.ResidualsPaid)
	}
	if len(ss.payments) != 0 {
		t.Errorf("expected 0 scrip store payments, got %d", len(ss.payments))
	}
}

// TestMediumLoop_ResidualSettlement_NoScripStore verifies that if ScripStore
// is nil, residual settlement is silently skipped (does not panic).
func TestMediumLoop_ResidualSettlement_NoScripStore(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	st := newMediumStubState()

	st.inventory = []*exchange.InventoryEntry{
		{EntryID: "entry-4", SellerKey: "seller-delta", ContentType: "review"},
	}
	st.history = []exchange.PriceRecord{
		{EntryID: "entry-4", SalePrice: 500, Timestamp: now.Add(-10 * time.Minute).UnixNano()},
	}

	loop := pricing.NewMediumLoop(pricing.MediumLoopOptions{
		State:      st,
		ScripStore: nil, // no scrip store
		Now:        func() time.Time { return now },
	})

	// Must not panic.
	result := loop.Tick(context.Background())

	if result.ResidualsPaid != 0 {
		t.Errorf("expected 0 residuals paid when ScripStore is nil, got %d", result.ResidualsPaid)
	}
}

// TestMediumLoop_ResidualSettlement_UnknownEntrySkipped verifies that sales for
// entries not in current inventory (e.g., expired/removed) are skipped gracefully.
func TestMediumLoop_ResidualSettlement_UnknownEntrySkipped(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	st := newMediumStubState()
	ss := newStubScripStore()

	// No matching inventory entry.
	st.history = []exchange.PriceRecord{
		{EntryID: "entry-orphan", SalePrice: 100, Timestamp: now.Add(-20 * time.Minute).UnixNano()},
	}

	loop := pricing.NewMediumLoop(pricing.MediumLoopOptions{
		State:      st,
		ScripStore: ss,
		Now:        func() time.Time { return now },
	})

	result := loop.Tick(context.Background())

	if result.ResidualsPaid != 0 {
		t.Errorf("expected 0 payments for unknown entry, got %d", result.ResidualsPaid)
	}
}

// =============================================================================
// Reputation floor tests
// =============================================================================

// TestMediumLoop_ReputationFloor_LowRepGetsDampen verifies that a seller with
// reputation below RepFloorThreshold and sufficient transactions receives a
// damped price adjustment.
func TestMediumLoop_ReputationFloor_LowRepGetsDampen(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	st := newMediumStubState()

	st.inventory = []*exchange.InventoryEntry{
		{EntryID: "entry-low-rep", SellerKey: "seller-bad"},
	}
	// Low reputation (below RepFloorThreshold=30).
	st.reputation["seller-bad"] = 10
	// Enough transactions to trigger the floor.
	st.demandCount["entry-low-rep"] = pricing.MinTransactionsForReputation

	loop := pricing.NewMediumLoop(pricing.MediumLoopOptions{
		State: st,
		Now:   func() time.Time { return now },
	})

	result := loop.Tick(context.Background())

	if result.ReputationUpdates == 0 {
		t.Fatal("expected at least one reputation floor update")
	}

	adj, ok := st.adjustments["entry-low-rep"]
	if !ok {
		t.Fatal("expected a price adjustment to be written for low-rep seller")
	}
	if adj.Multiplier >= 1.0 {
		t.Errorf("low-rep seller should have dampened multiplier < 1.0, got %.4f", adj.Multiplier)
	}
	if adj.Multiplier < pricing.MinMultiplier {
		t.Errorf("multiplier %.4f below MinMultiplier", adj.Multiplier)
	}
}

// TestMediumLoop_ReputationFloor_HighRepUnaffected verifies that a seller with
// reputation at or above RepFloorThreshold is not given a floor adjustment.
func TestMediumLoop_ReputationFloor_HighRepUnaffected(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	st := newMediumStubState()

	st.inventory = []*exchange.InventoryEntry{
		{EntryID: "entry-good-rep", SellerKey: "seller-good"},
	}
	// High reputation (above RepFloorThreshold=30).
	st.reputation["seller-good"] = 75
	st.demandCount["entry-good-rep"] = pricing.MinTransactionsForReputation

	loop := pricing.NewMediumLoop(pricing.MediumLoopOptions{
		State: st,
		Now:   func() time.Time { return now },
	})

	result := loop.Tick(context.Background())

	if result.ReputationUpdates != 0 {
		t.Errorf("expected 0 rep updates for high-rep seller, got %d", result.ReputationUpdates)
	}

	if _, ok := st.adjustments["entry-good-rep"]; ok {
		t.Error("high-rep seller should not receive a floor adjustment")
	}
}

// TestMediumLoop_ReputationFloor_InsufficientTransactionsSkipped verifies that
// sellers with fewer than MinTransactionsForReputation completed sales are
// not floor-adjusted (insufficient data).
func TestMediumLoop_ReputationFloor_InsufficientTransactionsSkipped(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	st := newMediumStubState()

	st.inventory = []*exchange.InventoryEntry{
		{EntryID: "entry-new", SellerKey: "seller-new"},
	}
	st.reputation["seller-new"] = 5   // very low reputation
	st.demandCount["entry-new"] = 1   // only 1 transaction (below threshold of 3)

	loop := pricing.NewMediumLoop(pricing.MediumLoopOptions{
		State: st,
		Now:   func() time.Time { return now },
	})

	result := loop.Tick(context.Background())

	if result.ReputationUpdates != 0 {
		t.Errorf("expected 0 rep updates for new seller (insufficient data), got %d", result.ReputationUpdates)
	}
}

// TestMediumLoop_ReputationFloor_MultiplierScalesWithReputation verifies that
// the floor multiplier scales correctly with reputation score.
// rep=0 → RepFloorMultiplier (0.8x), rep=RepFloorThreshold → just below 1.0x.
func TestMediumLoop_ReputationFloor_MultiplierScalesWithReputation(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	testCases := []struct {
		rep     int
		wantMin float64
		wantMax float64
	}{
		{rep: 0, wantMin: pricing.RepFloorMultiplier, wantMax: pricing.RepFloorMultiplier + 0.01},
		{rep: 15, wantMin: 0.89, wantMax: 0.91}, // midpoint: 0.8 + 0.2*(15/30) = 0.9
		{rep: 29, wantMin: 0.98, wantMax: 1.0},  // near threshold: just below 1.0
	}

	for _, tc := range testCases {
		tc := tc
		t.Run("rep="+string(rune('0'+tc.rep/10))+string(rune('0'+tc.rep%10)), func(t *testing.T) {
			t.Parallel()

			st := newMediumStubState()
			st.inventory = []*exchange.InventoryEntry{
				{EntryID: "entry-rep", SellerKey: "seller-test"},
			}
			st.reputation["seller-test"] = tc.rep
			st.demandCount["entry-rep"] = pricing.MinTransactionsForReputation

			loop := pricing.NewMediumLoop(pricing.MediumLoopOptions{
				State: st,
				Now:   func() time.Time { return now },
			})

			loop.Tick(context.Background())

			adj, ok := st.adjustments["entry-rep"]
			if !ok {
				t.Fatalf("rep=%d: expected adjustment to be written", tc.rep)
			}
			if adj.Multiplier < tc.wantMin || adj.Multiplier > tc.wantMax {
				t.Errorf("rep=%d: multiplier %.4f out of range [%.4f, %.4f]",
					tc.rep, adj.Multiplier, tc.wantMin, tc.wantMax)
			}
		})
	}
}

// =============================================================================
// Integration test: real exchange.State
// =============================================================================

// TestMediumLoop_RealState_AllSellerKeys verifies the AllSellerKeys method on
// the real exchange.State returns the correct seller keys from inventory.
func TestMediumLoop_RealState_AllSellerKeys(t *testing.T) {
	t.Parallel()

	st := exchange.NewState()

	// AllSellerKeys on empty state → empty slice.
	keys := st.AllSellerKeys()
	if len(keys) != 0 {
		t.Errorf("expected 0 seller keys for empty state, got %d", len(keys))
	}

	// Verify the method exists and returns the correct type (compile-time check
	// via interface satisfaction).
	var _ interface{ AllSellerKeys() []string } = st
}

// TestMediumLoop_ClusterCorrection_AdjustmentExpirySet verifies that dampened
// adjustments have a non-zero ExpiresAt set to approximately now + 2*interval.
func TestMediumLoop_ClusterCorrection_AdjustmentExpirySet(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	st := newMediumStubState()
	interval := 30 * time.Minute

	// 4 entries in same cluster, one outlier.
	for i, mult := range []float64{1.0, 1.0, 1.0, 2.0} {
		id := "entry-data-" + string(rune('A'+i))
		st.inventory = append(st.inventory, &exchange.InventoryEntry{
			EntryID:     id,
			SellerKey:   "seller-1",
			ContentType: "data",
		})
		st.setAdj(id, mult)
	}

	loop := pricing.NewMediumLoop(pricing.MediumLoopOptions{
		State:    st,
		Interval: interval,
		Now:      func() time.Time { return now },
	})

	loop.Tick(context.Background())

	// The outlier (entry-data-D, 2.0x) should have been corrected.
	adj, ok := st.adjustments["entry-data-D"]
	if !ok {
		t.Fatal("expected adjustment for entry-data-D")
	}
	if adj.ExpiresAt.IsZero() {
		t.Fatal("ExpiresAt should not be zero after cluster correction")
	}
	expectedExpiry := now.Add(2 * interval)
	diff := adj.ExpiresAt.Sub(expectedExpiry)
	if diff < 0 {
		diff = -diff
	}
	if diff > time.Second {
		t.Errorf("ExpiresAt = %v, want ≈ %v", adj.ExpiresAt, expectedExpiry)
	}
}

// TestMediumLoop_Run_Cancels verifies that Run returns when the context is cancelled.
func TestMediumLoop_Run_Cancels(t *testing.T) {
	t.Parallel()

	st := newMediumStubState()
	loop := pricing.NewMediumLoop(pricing.MediumLoopOptions{
		State:    st,
		Interval: 10 * time.Millisecond,
		Now:      time.Now,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := loop.Run(ctx)
	if err != context.DeadlineExceeded {
		t.Errorf("expected context.DeadlineExceeded, got %v", err)
	}
}

// TestMediumLoop_EmptyState_IsNoop verifies that a tick with no inventory, no
// history, and no adjustments completes without error and writes nothing.
func TestMediumLoop_EmptyState_IsNoop(t *testing.T) {
	t.Parallel()

	st := newMediumStubState()
	ss := newStubScripStore()

	loop := pricing.NewMediumLoop(pricing.MediumLoopOptions{
		State:      st,
		ScripStore: ss,
		Now:        func() time.Time { return time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC) },
	})

	result := loop.Tick(context.Background())

	if result.ClusterCorrections != 0 || result.ResidualsPaid != 0 || result.ReputationUpdates != 0 {
		t.Errorf("expected all-zero result for empty state, got %+v", result)
	}
	if len(st.adjustments) != 0 {
		t.Errorf("expected no adjustments written, got %d", len(st.adjustments))
	}
	if len(ss.payments) != 0 {
		t.Errorf("expected no scrip payments, got %d", len(ss.payments))
	}
}

// =============================================================================
// Compression assign tests
// =============================================================================

// TestMediumLoop_CompressionAssign_PostedWhenHighDemandNoDerivative verifies
// that the medium loop posts an open compression assign when an entry has
// 3+ purchases, no compressed derivative, and no active assign.
func TestMediumLoop_CompressionAssign_PostedWhenHighDemandNoDerivative(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	st := newMediumStubState()

	const tokenCost int64 = 10000
	st.inventory = []*exchange.InventoryEntry{
		{EntryID: "entry-hot", SellerKey: "seller-1", TokenCost: tokenCost},
	}
	// 3 distinct buyers have purchased this entry.
	st.purchaseCount["entry-hot"] = 3
	// No compressed derivative.
	st.compressedVersions["entry-hot"] = false
	// No active assigns.

	var postedSpecs []pricing.AssignSpec
	loop := pricing.NewMediumLoop(pricing.MediumLoopOptions{
		State: st,
		Now:   func() time.Time { return now },
		PostAssign: func(spec pricing.AssignSpec) error {
			postedSpecs = append(postedSpecs, spec)
			return nil
		},
	})

	result := loop.Tick(context.Background())

	if result.CompressionAssigns != 1 {
		t.Errorf("expected 1 compression assign posted, got %d", result.CompressionAssigns)
	}
	if len(postedSpecs) != 1 {
		t.Fatalf("expected 1 PostAssign call, got %d", len(postedSpecs))
	}
	spec := postedSpecs[0]
	if spec.EntryID != "entry-hot" {
		t.Errorf("expected assign for entry-hot, got %s", spec.EntryID)
	}
	if spec.TaskType != "compress" {
		t.Errorf("expected task_type=compress, got %s", spec.TaskType)
	}
	wantReward := tokenCost / 2
	if spec.Reward != wantReward {
		t.Errorf("expected reward=%d (token_cost/2), got %d", wantReward, spec.Reward)
	}
}

// TestMediumLoop_CompressionAssign_SkippedWhenDerivativeExists verifies that
// no compression assign is posted when a compressed derivative already exists.
func TestMediumLoop_CompressionAssign_SkippedWhenDerivativeExists(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	st := newMediumStubState()

	st.inventory = []*exchange.InventoryEntry{
		{EntryID: "entry-compressed", SellerKey: "seller-1", TokenCost: 8000},
	}
	// 3+ purchases — high demand.
	st.purchaseCount["entry-compressed"] = 5
	// Compressed derivative already exists — no assign should be posted.
	st.compressedVersions["entry-compressed"] = true

	var postCalls int
	loop := pricing.NewMediumLoop(pricing.MediumLoopOptions{
		State: st,
		Now:   func() time.Time { return now },
		PostAssign: func(spec pricing.AssignSpec) error {
			postCalls++
			return nil
		},
	})

	result := loop.Tick(context.Background())

	if result.CompressionAssigns != 0 {
		t.Errorf("expected 0 compression assigns when derivative exists, got %d", result.CompressionAssigns)
	}
	if postCalls != 0 {
		t.Errorf("expected 0 PostAssign calls when derivative exists, got %d", postCalls)
	}
}

// TestMediumLoop_CompressionAssign_SkippedWhenBelowThreshold verifies that
// no compression assign is posted when the entry has fewer than 3 purchases.
func TestMediumLoop_CompressionAssign_SkippedWhenBelowThreshold(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	st := newMediumStubState()

	st.inventory = []*exchange.InventoryEntry{
		{EntryID: "entry-cold", SellerKey: "seller-1", TokenCost: 6000},
	}
	// Only 2 purchases — below the default threshold of 3.
	st.purchaseCount["entry-cold"] = 2
	// No compressed derivative.
	st.compressedVersions["entry-cold"] = false

	var postCalls int
	loop := pricing.NewMediumLoop(pricing.MediumLoopOptions{
		State: st,
		Now:   func() time.Time { return now },
		PostAssign: func(spec pricing.AssignSpec) error {
			postCalls++
			return nil
		},
	})

	result := loop.Tick(context.Background())

	if result.CompressionAssigns != 0 {
		t.Errorf("expected 0 compression assigns when below threshold, got %d", result.CompressionAssigns)
	}
	if postCalls != 0 {
		t.Errorf("expected 0 PostAssign calls when below threshold, got %d", postCalls)
	}
}
