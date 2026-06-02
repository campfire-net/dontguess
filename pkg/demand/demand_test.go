package demand

import (
	"encoding/json"
	"fmt"
	"testing"
)

// makeMiss builds a MissMessage with the given miss ID, buy ID, and task text.
// The payload mirrors the shape emitted by Engine.handleBuyMiss.
func makeMiss(missID, buyID, task string) MissMessage {
	p := BuyMissPayload{
		Task:             task,
		TaskHash:         "sha256:" + buyID,
		OfferedPriceRate: 70,
		ExpiresAt:        "2026-06-03T00:00:00Z",
		BuyMsgID:         buyID,
	}
	payload, _ := json.Marshal(p)
	return MissMessage{ID: missID, Payload: payload}
}

// --- IsSynthetic ---

func TestIsSynthetic_ExcludesRegression(t *testing.T) {
	cases := []struct {
		task    string
		want    bool
		comment string
	}{
		{"regression-parallel-178949-buy-0001", true, "regression- prefix"},
		{"regression-timeout-178949-buy-0001", true, "regression-timeout prefix"},
		{"timeout-178 some task", true, "contains timeout-178"},
		{"test", true, "exact 'test'"},
		{"test suite for something", false, "starts with 'test ' but not bare 'test' — real task"},
		{"Test Suite scan", false, "case-insensitive prefix 'test ' — real task, not synthetic"},
		{"upgrade smoke test cf v0.31.2 operator", true, "junk smoke test entry"},
		{"parallel-3-105489", true, "load-test parallel buy series"},
		{"parallel-warmup-105489", true, "load-test warmup parallel"},
		{"validation-preflight-1776348953", true, "infra validation probe"},
		{"e2e-startup-probe", true, "e2e startup probe"},
		{"test-investigation-probe", true, "infra probe task"},
		{"final-flake-attestation-N10-stress-sweep", true, "CI probe attestation"},
		{"zzqq nonsense xyzzy no such cached inference", true, "explicit nonsense marker"},
		{"orchestrator precondition check: dontguess reachable", true, "infra precondition check"},
		{"post-identity-fix health check", true, "health check probe"},
		{"audit test suite for untested endpoints", false, "real audit miss — not synthetic"},
		{"campfire SDK surface review", false, "real campfire miss"},
		{"convention declaration revoke/supersede authorization", false, "real convention miss"},
		{"RPT review of campfire SDK surface", false, "real review miss"},
		{"FROST threshold key ceremony for cold wallet signing", false, "real security miss"},
		{"fix convention.Server subscribe cursor", false, "real campfire miss"},
		{"", false, "empty task is not synthetic"},
	}
	for _, tc := range cases {
		if got := IsSynthetic(tc.task); got != tc.want {
			t.Errorf("IsSynthetic(%q) = %v, want %v (%s)", tc.task, got, tc.want, tc.comment)
		}
	}
}

// --- assignCluster ---

func TestAssignCluster(t *testing.T) {
	cases := []struct {
		task    string
		cluster string
	}{
		{"audit test suite for untested endpoints, missing error paths, edge case gaps in ops/welcome-center-server", "audit"},
		{"RPT review of campfire SDK surface", "review"},
		{"campfire SDK convention declaration lifecycle", "campfire"},
		{"convention declaration revoke/supersede authorization", "convention"},
		{"FROST threshold key ceremony for cold wallet signing", "security"},
		{"fix convention.Server subscribe cursor and implement warm-worker EventSink", "campfire"},
		{"GateEvaluator pattern — which endpoints need auth gates?", "security"},
		{"something completely unrelated to any cluster", "other"},
		{"test-gap scan for missing test coverage in pkg/exchange", "test-gap"},
	}
	for _, tc := range cases {
		if got := assignCluster(tc.task); got != tc.cluster {
			t.Errorf("assignCluster(%q) = %q, want %q", tc.task, got, tc.cluster)
		}
	}
}

// --- BuildBacklog ---

func TestBuildBacklog_ExcludesSynthetic(t *testing.T) {
	msgs := []MissMessage{
		makeMiss("m-1", "b-1", "audit test suite for untested endpoints"),
		makeMiss("m-2", "b-2", "regression-parallel-178949-buy-0001"),
		makeMiss("m-3", "b-3", "test"),
		makeMiss("m-4", "b-4", "campfire SDK subscribe cursor fix"),
		makeMiss("m-5", "b-5", "upgrade smoke test cf v0.31.2 operator"),
	}

	bl := BuildBacklog(msgs)

	if bl.TotalMisses != 5 {
		t.Errorf("TotalMisses = %d, want 5", bl.TotalMisses)
	}
	if bl.SyntheticExcluded != 3 {
		t.Errorf("SyntheticExcluded = %d, want 3", bl.SyntheticExcluded)
	}
	if bl.RealMisses != 2 {
		t.Errorf("RealMisses = %d, want 2", bl.RealMisses)
	}
}

func TestBuildBacklog_ClusterAssignment(t *testing.T) {
	msgs := []MissMessage{
		makeMiss("m-c1", "b-c1", "campfire SDK convention declaration lifecycle"),
		makeMiss("m-c2", "b-c2", "fix convention.Server subscribe cursor"),
		makeMiss("m-a1", "b-a1", "audit test suite for untested endpoints in ops/welcome-center-server"),
		makeMiss("m-a2", "b-a2", "missing error paths and edge case gaps in justice-server"),
		makeMiss("m-a3", "b-a3", "test coverage audit for pkg/exchange put handler"),
		makeMiss("m-cv1", "b-cv1", "convention declaration revoke authorization"),
		makeMiss("m-r1", "b-r1", "RPT review of campfire SDK surface"),
		makeMiss("m-s1", "b-s1", "FROST threshold key ceremony for cold wallet signing"),
		makeMiss("m-o1", "b-o1", "unrelated domain specific task xyz"),
	}

	bl := BuildBacklog(msgs)

	if bl.RealMisses != len(msgs) {
		t.Errorf("RealMisses = %d, want %d", bl.RealMisses, len(msgs))
	}
	if bl.SyntheticExcluded != 0 {
		t.Errorf("SyntheticExcluded = %d, want 0 (no synthetic in input)", bl.SyntheticExcluded)
	}

	// Build a cluster->count lookup.
	got := make(map[string]int, len(bl.Clusters))
	for _, c := range bl.Clusters {
		got[c.Name] = c.Count
	}

	check := func(name string, wantCount int) {
		t.Helper()
		if got[name] != wantCount {
			t.Errorf("cluster %q count = %d, want %d", name, got[name], wantCount)
		}
	}
	check("campfire", 2)
	check("audit", 3)
	check("convention", 1)
	check("review", 1)
	check("security", 1)
	check("other", 1)
}

func TestBuildBacklog_SortedByCountDescending(t *testing.T) {
	msgs := []MissMessage{
		makeMiss("m-a1", "b-a1", "audit test suite for untested endpoints"),
		makeMiss("m-a2", "b-a2", "missing error paths in welcome-center"),
		makeMiss("m-a3", "b-a3", "edge case gaps in justice server"),
		makeMiss("m-c1", "b-c1", "campfire SDK fix"),
		makeMiss("m-c2", "b-c2", "campfire subscribe cursor"),
		makeMiss("m-r1", "b-r1", "RPT review of design"),
	}

	bl := BuildBacklog(msgs)

	// audit(3) > campfire(2) > review(1)
	if len(bl.Clusters) < 3 {
		t.Fatalf("expected at least 3 clusters, got %d", len(bl.Clusters))
	}
	if bl.Clusters[0].Count < bl.Clusters[1].Count {
		t.Errorf("clusters not sorted: [0].Count=%d < [1].Count=%d", bl.Clusters[0].Count, bl.Clusters[1].Count)
	}
	if bl.Clusters[1].Count < bl.Clusters[2].Count {
		t.Errorf("clusters not sorted: [1].Count=%d < [2].Count=%d", bl.Clusters[1].Count, bl.Clusters[2].Count)
	}
}

func TestBuildBacklog_BacklogItemFields(t *testing.T) {
	msgs := []MissMessage{
		makeMiss("miss-abc", "buy-xyz", "audit test suite for untested endpoints in ops/justice-server"),
	}

	bl := BuildBacklog(msgs)

	if bl.RealMisses != 1 {
		t.Fatalf("RealMisses = %d, want 1", bl.RealMisses)
	}
	if len(bl.Clusters) == 0 {
		t.Fatal("no clusters returned")
	}
	item := bl.Clusters[0].Items[0]
	if item.MissID != "miss-abc" {
		t.Errorf("MissID = %q, want miss-abc", item.MissID)
	}
	if item.BuyMsgID != "buy-xyz" {
		t.Errorf("BuyMsgID = %q, want buy-xyz", item.BuyMsgID)
	}
	if item.Task == "" {
		t.Error("Task is empty")
	}
	if item.OfferedPriceRate != 70 {
		t.Errorf("OfferedPriceRate = %d, want 70", item.OfferedPriceRate)
	}
	if item.Cluster != "audit" {
		t.Errorf("Cluster = %q, want audit", item.Cluster)
	}
}

func TestBuildBacklog_Empty(t *testing.T) {
	bl := BuildBacklog(nil)
	if bl.TotalMisses != 0 || bl.RealMisses != 0 || bl.SyntheticExcluded != 0 {
		t.Errorf("empty input: got non-zero counts: %+v", bl)
	}
	if len(bl.Clusters) != 0 {
		t.Errorf("empty input: expected no clusters, got %d", len(bl.Clusters))
	}
}

func TestBuildBacklog_AllSynthetic(t *testing.T) {
	msgs := []MissMessage{
		makeMiss("m-1", "b-1", "regression-parallel-178949-buy-0001"),
		makeMiss("m-2", "b-2", "test"),
		makeMiss("m-3", "b-3", "upgrade smoke test cf v0.31.2 operator"),
	}

	bl := BuildBacklog(msgs)

	if bl.TotalMisses != 3 {
		t.Errorf("TotalMisses = %d, want 3", bl.TotalMisses)
	}
	if bl.SyntheticExcluded != 3 {
		t.Errorf("SyntheticExcluded = %d, want 3", bl.SyntheticExcluded)
	}
	if bl.RealMisses != 0 {
		t.Errorf("RealMisses = %d, want 0", bl.RealMisses)
	}
	if len(bl.Clusters) != 0 {
		t.Errorf("expected 0 clusters for all-synthetic input, got %d", len(bl.Clusters))
	}
}

// TestBuildBacklog_Representative84 exercises the 84-miss scenario from §4 with a
// representative fixture approximating the live cluster distribution:
//
//	campfire(12), audit(9), convention(8), review(6), security(3), test-gap(2), other(44)
//
// Tasks are chosen to be unambiguous matches for each cluster given the
// clusterRules defined in demand.go. This validates that the clustering logic
// handles scale and that the backlog size matches the expected real-miss count.
func TestBuildBacklog_Representative84(t *testing.T) {
	fixtures := []struct {
		cluster string
		tasks   []string
	}{
		{"campfire", []string{
			// All contain "campfire", "cf-protocol", "subscribe cursor", or
			// "convention declaration" — unambiguous campfire markers.
			"campfire SDK convention declaration lifecycle management",
			"fix convention.Server subscribe cursor warm-worker backend",
			"campfire CF_NO_PINS pin management and storage",
			"cf-protocol README update for cf v0.31",
			"campfire store GC and log compaction",
			"campfire federation trust topology configuration",
			"campfire beacon resolution from alias registry",
			"campfire naming registry lookup protocol",
			"campfire view predicate grammar update",
			"campfire session token TTL extension mechanism",
			"cf-protocol binary framing alignment update",
			"campfire identity cascade config resolution",
		}},
		{"audit", []string{
			// All contain "audit", "missing error", "edge case", or "error path".
			"audit test suite for untested endpoints, missing error paths, edge case gaps in ops/welcome-center-server, ops/justice-server",
			"missing error paths in welcome-center PUT handler",
			"edge case gaps in justice-server routing logic",
			"audit coverage for pkg/exchange settle path",
			"audit endpoint handlers in operator-server for coverage gaps",
			"error path coverage for scrip DecrementBudget failure modes",
			"missing edge cases in matching engine logic",
			"audit put validation boundary conditions",
			"edge case analysis for buy order expiry handling",
		}},
		{"convention", []string{
			// All contain "revoke", "supersede", "exchange convention",
			// "convention:put", "convention:buy", or "convention:assign".
			"convention declaration revoke/supersede authorization lifecycle",
			"convention:put v0.2 schema validation rules update",
			"convention supersede lifecycle for exchange-core declarations",
			"exchange convention operation: dispute initiation phase",
			"convention:assign declaration for brokered-match task type",
			"exchange convention token_cost field validation",
			"convention linting rules for revoke operations",
			"convention:buy budget validation and price cap enforcement",
		}},
		{"review", []string{
			// All contain "rpt review", "code review", or "design review".
			"RPT review of campfire SDK convention dispatch surface",
			"code review for exchange engine dispatch loop and error handling",
			"design review for federation trust model and identity resolution",
			"RPT review of scrip settlement and escrow logic",
			"code review for assign-claim expiry and auction handling",
			"design review for buy-miss fulfillment standing offer lifecycle",
		}},
		{"security", []string{
			// All contain "cold wallet", "frost", "auth gate", or "authentication gate".
			"FROST threshold key ceremony for cold wallet signing protocol",
			"GateEvaluator conformance — which endpoints need auth gates?",
			"authentication gate review for operator API ingress",
		}},
		{"test-gap", []string{
			// All contain "test gap", "test-gap", or "test strategy".
			"test gap scan for missing coverage in pkg/exchange put and settle paths",
			"test strategy for end-to-end buy-miss fulfillment workflow",
		}},
	}

	// Count clustered misses.
	var total int
	for _, f := range fixtures {
		total += len(f.tasks)
	}
	// Fill to 84 with "other" misses that don't match any cluster keyword.
	otherCount := 84 - total
	if otherCount < 0 {
		t.Fatalf("fixture task count %d already exceeds 84", total)
	}

	msgs := make([]MissMessage, 0, 84+10)
	idx := 0
	for _, f := range fixtures {
		for _, task := range f.tasks {
			id := fmt.Sprintf("miss-%03d", idx)
			msgs = append(msgs, makeMiss(id, "buy-"+id, task))
			idx++
		}
	}
	for i := 0; i < otherCount; i++ {
		id := fmt.Sprintf("miss-other-%03d", i)
		msgs = append(msgs, makeMiss(id, "buy-"+id,
			fmt.Sprintf("domain-specific task for project module %d", i)))
	}

	// Add synthetic misses that must be excluded from the backlog.
	syntheticCount := 10
	for i := 0; i < syntheticCount; i++ {
		id := fmt.Sprintf("miss-syn-%03d", i)
		msgs = append(msgs, makeMiss(id, "buy-"+id,
			fmt.Sprintf("regression-parallel-178949-buy-%04d", i)))
	}

	bl := BuildBacklog(msgs)

	// Validate top-level accounting.
	if bl.TotalMisses != 84+syntheticCount {
		t.Errorf("TotalMisses = %d, want %d", bl.TotalMisses, 84+syntheticCount)
	}
	if bl.SyntheticExcluded != syntheticCount {
		t.Errorf("SyntheticExcluded = %d, want %d", bl.SyntheticExcluded, syntheticCount)
	}
	if bl.RealMisses != 84 {
		t.Errorf("RealMisses = %d, want 84", bl.RealMisses)
	}

	// Validate cluster counts and item field integrity.
	got := make(map[string]int)
	for _, c := range bl.Clusters {
		got[c.Name] = c.Count
		for _, item := range c.Items {
			if item.Task == "" {
				t.Errorf("cluster %s: item %s has empty task", c.Name, item.MissID)
			}
			if item.OfferedPriceRate != 70 {
				t.Errorf("cluster %s: item %s OfferedPriceRate = %d, want 70",
					c.Name, item.MissID, item.OfferedPriceRate)
			}
		}
	}

	wantClusters := map[string]int{
		"campfire":   12,
		"audit":      9,
		"convention": 8,
		"review":     6,
		"security":   3,
		"test-gap":   2,
		"other":      otherCount,
	}
	for cluster, want := range wantClusters {
		if got[cluster] != want {
			t.Errorf("cluster %q: count = %d, want %d", cluster, got[cluster], want)
		}
	}

	// Clusters must be sorted by count descending.
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
