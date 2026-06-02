package exchange_test

// TestDemand_Integration exercises the end-to-end demand workflow:
//
//  1. Init a real exchange (campfire, SQLite store, real engine).
//  2. Submit representative buy orders covering all cluster themes (campfire,
//     audit, convention, review, security, test-gap, other) plus synthetic
//     load-test buys (regression-*, "test"-class).
//  3. Dispatch each buy through the engine (no inventory → every buy misses).
//  4. Read the resulting exchange:buy-miss messages from the store — the
//     same read-only path that `dontguess demand` takes in production.
//  5. Call demand.BuildBacklog on the real miss messages.
//  6. Assert:
//     - synthetic misses are excluded
//     - real miss count matches expectation
//     - each backlog item has task text and offered_price_rate=70
//     - all expected clusters are present
//     - clusters are sorted by count descending
//
// NO MOCKS. The engine writes real exchange:buy-miss messages; the test reads
// them back from the store. The path from store read → MissMessage → BuildBacklog
// is the identical path runDemand takes in production.

import (
	"encoding/json"
	"testing"

	"github.com/campfire-net/campfire/cf-protocol/store"

	"github.com/campfire-net/dontguess/pkg/demand"
	"github.com/campfire-net/dontguess/pkg/exchange"
)

// TestDemand_Integration runs the full demand backlog workflow on a real exchange.
func TestDemand_Integration(t *testing.T) {
	t.Parallel()

	// Use the shared test harness (testHarness defined in engine_test.go).
	h := newTestHarness(t)
	eng := h.newEngine()

	// Replay existing state (convention declarations etc. from Init).
	existing, err := h.st.ListMessages(h.cfID, 0)
	if err != nil {
		t.Fatalf("ListMessages for replay: %v", err)
	}
	eng.State().Replay(exchange.FromStoreRecords(existing))

	// Representative real-miss tasks — one per major theme from §4.
	realTasks := []string{
		// campfire cluster (3 tasks)
		"campfire SDK convention declaration lifecycle management",
		"fix convention.Server subscribe cursor for warm-worker backends",
		"campfire CF_NO_PINS pin management and beacon resolution protocol",
		// audit cluster (3 tasks)
		"audit test suite for untested endpoints, missing error paths, edge case gaps in ops/welcome-center-server, ops/justice-server",
		"missing error paths in welcome-center-server PUT and DELETE handlers",
		"edge case gaps in the justice-server routing logic and error handling paths",
		// convention cluster (2 tasks)
		"convention declaration revoke/supersede authorization lifecycle",
		"exchange convention put v0.2 schema validation rules update",
		// review cluster (2 tasks)
		"RPT review of campfire SDK surface — convention dispatch and named views",
		"code review for exchange engine dispatch loop and assign-claim expiry handling",
		// security cluster (1 task)
		"FROST threshold key ceremony protocol for cold wallet signing procedures",
		// test-gap cluster (1 task)
		"test gap scan for missing coverage in pkg/exchange put and settle paths",
		// other cluster (1 task)
		"GalTrader game balance analysis for sector trading route optimization",
	}

	syntheticTasks := []string{
		"regression-parallel-178949-buy-0001",
		"regression-parallel-178949-buy-0002",
		"test",
		"upgrade smoke test cf v0.31.2 operator",
	}

	// Send buy orders and dispatch through the engine.
	// No inventory is loaded, so every buy produces an exchange:buy-miss message.
	for _, task := range append(realTasks, syntheticTasks...) {
		payload := demandBuyPayload(t, task, 5000)
		msg := h.sendMessage(h.buyer, payload, []string{exchange.TagBuy}, nil)
		if err := eng.DispatchForTest(msg); err != nil {
			taskPreview := task
			if len(taskPreview) > 40 {
				taskPreview = taskPreview[:40]
			}
			t.Fatalf("dispatch buy(%q): %v", taskPreview, err)
		}
	}

	// Read exchange:buy-miss messages from the store — the production read path.
	allMsgs, err := h.st.ListMessages(h.cfID, 0, store.MessageFilter{
		Tags: []string{exchange.TagBuyMiss},
	})
	if err != nil {
		t.Fatalf("ListMessages(buy-miss): %v", err)
	}

	// Convert store records to demand.MissMessage (same conversion as runDemand).
	rawMisses := make([]demand.MissMessage, 0, len(allMsgs))
	for i := range allMsgs {
		rec := &allMsgs[i]
		rawMisses = append(rawMisses, demand.MissMessage{
			ID:        rec.ID,
			Payload:   rec.Payload,
			Timestamp: rec.Timestamp,
		})
	}

	// Validate raw miss count (all tasks produce a miss since no inventory).
	expectedTotal := len(realTasks) + len(syntheticTasks)
	if len(rawMisses) != expectedTotal {
		t.Errorf("raw miss count = %d, want %d (real=%d + synthetic=%d)",
			len(rawMisses), expectedTotal, len(realTasks), len(syntheticTasks))
	}

	// Call the production demand clustering function.
	bl := demand.BuildBacklog(rawMisses)

	// --- Assert backlog accounting ---
	if bl.TotalMisses != len(rawMisses) {
		t.Errorf("Backlog.TotalMisses = %d, want %d", bl.TotalMisses, len(rawMisses))
	}
	if bl.SyntheticExcluded != len(syntheticTasks) {
		t.Errorf("Backlog.SyntheticExcluded = %d, want %d", bl.SyntheticExcluded, len(syntheticTasks))
	}
	if bl.RealMisses != len(realTasks) {
		t.Errorf("Backlog.RealMisses = %d, want %d", bl.RealMisses, len(realTasks))
	}

	// --- Assert every backlog item has task text and offered_price_rate=70 ---
	for _, c := range bl.Clusters {
		for _, item := range c.Items {
			if item.Task == "" {
				t.Errorf("cluster %s: item %s has empty task text", c.Name, item.MissID)
			}
			if item.OfferedPriceRate != 70 {
				t.Errorf("cluster %s: item %s OfferedPriceRate = %d, want 70",
					c.Name, item.MissID, item.OfferedPriceRate)
			}
			if item.MissID == "" {
				t.Errorf("cluster %s: item has empty MissID (task=%q)", c.Name, item.Task)
			}
		}
	}

	// --- Assert expected clusters are present ---
	clusterCounts := make(map[string]int)
	for _, c := range bl.Clusters {
		clusterCounts[c.Name] = c.Count
	}

	t.Logf("demand backlog: total=%d real=%d synthetic=%d clusters=%v",
		bl.TotalMisses, bl.RealMisses, bl.SyntheticExcluded, clusterCounts)

	if clusterCounts["campfire"] == 0 {
		t.Error("expected campfire cluster in backlog")
	}
	if clusterCounts["audit"] == 0 {
		t.Error("expected audit cluster in backlog")
	}
	if clusterCounts["convention"] == 0 {
		t.Error("expected convention cluster in backlog")
	}
	if clusterCounts["review"] == 0 {
		t.Error("expected review cluster in backlog")
	}
	if clusterCounts["security"] == 0 {
		t.Error("expected security cluster in backlog")
	}

	// --- Assert clusters sorted by count descending ---
	for i := 1; i < len(bl.Clusters); i++ {
		if bl.Clusters[i].Count > bl.Clusters[i-1].Count {
			t.Errorf("clusters not sorted at index %d: %s(%d) > %s(%d)",
				i,
				bl.Clusters[i].Name, bl.Clusters[i].Count,
				bl.Clusters[i-1].Name, bl.Clusters[i-1].Count,
			)
		}
	}
}

// demandBuyPayload builds an exchange:buy JSON payload for demand integration tests.
// Uses a distinct name to avoid conflict with buildBuyPayload in state_validation_test.go.
func demandBuyPayload(t *testing.T, task string, budget int64) []byte {
	t.Helper()
	p := map[string]any{
		"task":           task,
		"budget":         budget,
		"max_results":    3,
		"min_reputation": 0,
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal buy payload: %v", err)
	}
	return b
}

