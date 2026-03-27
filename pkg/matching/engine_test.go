package matching_test

import (
	"testing"
	"time"

	"github.com/3dl-dev/dontguess/pkg/matching"
)

// buildIndex creates a test index with the given entries (no IDF priming).
func buildIndex(entries []matching.RankInput) *matching.Index {
	idx := matching.NewIndex(nil, matching.RankOptions{})
	idx.Rebuild(entries)
	return idx
}

// TestIndex_SearchReturnsTopRelevant verifies that with 10 entries, a buy task
// for "Go HTTP handler unit tests" returns the most relevant entry first.
// This is the core done-condition from the bead spec.
func TestIndex_SearchReturnsTopRelevant(t *testing.T) {
	t.Parallel()

	now := time.Now()
	entries := []matching.RankInput{
		{EntryID: "tf-s3", SellerKey: "s1", Description: "Terraform AWS S3 bucket module versioning lifecycle", ContentType: "code", Domains: []string{"terraform", "aws"}, TokenCost: 5000, Price: 500, SellerReputation: 70, PutTimestamp: now.Add(-24 * time.Hour).UnixNano()},
		{EntryID: "go-http-tests", SellerKey: "s2", Description: "Go HTTP handler unit tests table-driven JSON POST validation", ContentType: "code", Domains: []string{"go", "testing"}, TokenCost: 8000, Price: 800, SellerReputation: 80, PutTimestamp: now.Add(-2 * time.Hour).UnixNano()},
		{EntryID: "py-pandas", SellerKey: "s3", Description: "Python pandas dataframe aggregation pivot analysis", ContentType: "analysis", Domains: []string{"python", "data"}, TokenCost: 6000, Price: 600, SellerReputation: 65, PutTimestamp: now.Add(-48 * time.Hour).UnixNano()},
		{EntryID: "k8s-yaml", SellerKey: "s4", Description: "Kubernetes deployment YAML generator resource limits", ContentType: "code", Domains: []string{"kubernetes"}, TokenCost: 7000, Price: 700, SellerReputation: 72, PutTimestamp: now.Add(-12 * time.Hour).UnixNano()},
		{EntryID: "rust-http", SellerKey: "s5", Description: "Rust async tokio HTTP client retry backoff", ContentType: "code", Domains: []string{"rust"}, TokenCost: 9000, Price: 900, SellerReputation: 78, PutTimestamp: now.Add(-6 * time.Hour).UnixNano()},
		{EntryID: "docker-go", SellerKey: "s6", Description: "Docker multi-stage build optimization Go services", ContentType: "code", Domains: []string{"docker", "go"}, TokenCost: 4000, Price: 400, SellerReputation: 60, PutTimestamp: now.Add(-72 * time.Hour).UnixNano()},
		{EntryID: "sql-timeseries", SellerKey: "s7", Description: "PostgreSQL query optimization time-series analytics", ContentType: "analysis", Domains: []string{"sql", "postgres"}, TokenCost: 5500, Price: 550, SellerReputation: 68, PutTimestamp: now.Add(-36 * time.Hour).UnixNano()},
		{EntryID: "ts-jest", SellerKey: "s8", Description: "TypeScript React component testing Jest Testing Library", ContentType: "code", Domains: []string{"typescript", "react"}, TokenCost: 7500, Price: 750, SellerReputation: 75, PutTimestamp: now.Add(-18 * time.Hour).UnixNano()},
		{EntryID: "ci-go", SellerKey: "s9", Description: "GitHub Actions CI pipeline Go coverage lint", ContentType: "code", Domains: []string{"ci", "go"}, TokenCost: 3000, Price: 300, SellerReputation: 62, PutTimestamp: now.Add(-96 * time.Hour).UnixNano()},
		{EntryID: "sec-api", SellerKey: "s10", Description: "Security audit checklist Go HTTP APIs", ContentType: "review", Domains: []string{"security", "go"}, TokenCost: 4500, Price: 450, SellerReputation: 85, PutTimestamp: now.Add(-8 * time.Hour).UnixNano()},
	}

	idx := buildIndex(entries)
	if idx.Len() != 10 {
		t.Fatalf("expected 10 indexed entries, got %d", idx.Len())
	}

	task := "unit tests for a Go HTTP handler accepting JSON POST requests with validation"
	results := idx.Search(task, 5)

	if len(results) == 0 {
		t.Fatal("expected results, got none")
	}
	if results[0].EntryID != "go-http-tests" {
		t.Errorf("top result = %q, want go-http-tests\nall results: %v", results[0].EntryID, entryIDs(results))
	}
}

// TestIndex_DisputedEntryExcluded verifies that Layer 0 works through the Index.
func TestIndex_DisputedEntryExcluded(t *testing.T) {
	t.Parallel()

	now := time.Now()
	entries := []matching.RankInput{
		{EntryID: "good", SellerKey: "s1", Description: "Go HTTP handler tests", ContentType: "code", Domains: []string{"go"}, TokenCost: 5000, Price: 500, SellerReputation: 70, PutTimestamp: now.Add(-1 * time.Hour).UnixNano(), HasUpheldDispute: false},
		{EntryID: "disputed", SellerKey: "s2", Description: "Go HTTP handler tests", ContentType: "code", Domains: []string{"go"}, TokenCost: 5000, Price: 500, SellerReputation: 70, PutTimestamp: now.Add(-1 * time.Hour).UnixNano(), HasUpheldDispute: true},
	}
	idx := buildIndex(entries)

	results := idx.Search("Go HTTP handler unit tests", 10)
	for _, r := range results {
		if r.EntryID == "disputed" {
			t.Errorf("disputed entry appeared in search results")
		}
	}
}

// TestIndex_AddAndRemove verifies incremental index mutations.
func TestIndex_AddAndRemove(t *testing.T) {
	t.Parallel()

	idx := matching.NewIndex(nil, matching.RankOptions{})
	if idx.Len() != 0 {
		t.Errorf("empty index len = %d, want 0", idx.Len())
	}

	entry := matching.RankInput{
		EntryID:          "e1",
		SellerKey:        "s1",
		Description:      "Go unit test generator",
		ContentType:      "code",
		Domains:          []string{"go"},
		TokenCost:        5000,
		Price:            500,
		SellerReputation: 70,
		PutTimestamp:     time.Now().Add(-1 * time.Hour).UnixNano(),
	}

	idx.Add(entry)
	if idx.Len() != 1 {
		t.Errorf("after Add, len = %d, want 1", idx.Len())
	}

	// Adding with same EntryID replaces.
	entry.SellerReputation = 85
	idx.Add(entry)
	if idx.Len() != 1 {
		t.Errorf("after duplicate Add, len = %d, want 1 (should replace)", idx.Len())
	}

	idx.Remove("e1")
	if idx.Len() != 0 {
		t.Errorf("after Remove, len = %d, want 0", idx.Len())
	}

	// Remove non-existent is a no-op.
	idx.Remove("nonexistent")
	if idx.Len() != 0 {
		t.Errorf("after no-op Remove, len = %d, want 0", idx.Len())
	}
}

// TestIndex_MaxResultsCap verifies that Search respects the maxResults cap.
func TestIndex_MaxResultsCap(t *testing.T) {
	t.Parallel()

	entries := make([]matching.RankInput, 5)
	for i := range entries {
		entries[i] = matching.RankInput{
			EntryID:          string(rune('a' + i)),
			SellerKey:        string(rune('a' + i)),
			Description:      "Go HTTP handler unit test generator",
			ContentType:      "code",
			Domains:          []string{"go"},
			TokenCost:        5000,
			Price:            500,
			SellerReputation: 70,
			PutTimestamp:     time.Now().Add(-1 * time.Hour).UnixNano(),
		}
	}
	idx := buildIndex(entries)

	results := idx.Search("Go unit tests", 3)
	if len(results) > 3 {
		t.Errorf("Search(maxResults=3) returned %d results, want <=3", len(results))
	}
}

// TestIndex_EmptyIndexReturnsNil verifies searching an empty index is safe.
func TestIndex_EmptyIndexReturnsNil(t *testing.T) {
	t.Parallel()

	idx := matching.NewIndex(nil, matching.RankOptions{})
	results := idx.Search("anything", 5)
	if results != nil {
		t.Errorf("empty index search returned non-nil: %v", results)
	}
}

// stubEmbedder is a minimal Embedder implementation that does NOT implement
// CorpusIndexer. It uses a fixed 1-dimensional embedding so it never panics
// on Rebuild regardless of the entry corpus.
type stubEmbedder struct {
	embedCalls int
}

func (s *stubEmbedder) Embed(_ string) []float64 {
	s.embedCalls++
	return []float64{1.0}
}

func (s *stubEmbedder) Similarity(a, b []float64) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	return 1.0
}

// TestIndex_RebuildWithNonTFIDFEmbedder verifies that Rebuild does not panic
// and correctly indexes entries when given an Embedder that does not implement
// CorpusIndexer (i.e., no concrete type assertion to *TFIDFEmbedder).
func TestIndex_RebuildWithNonTFIDFEmbedder(t *testing.T) {
	t.Parallel()

	emb := &stubEmbedder{}
	idx := matching.NewIndex(emb, matching.RankOptions{})

	entries := []matching.RankInput{
		{EntryID: "a", SellerKey: "s1", Description: "foo bar baz", ContentType: "code", Domains: []string{"go"}, TokenCost: 1000, Price: 100, SellerReputation: 70, PutTimestamp: time.Now().Add(-1 * time.Hour).UnixNano()},
		{EntryID: "b", SellerKey: "s2", Description: "qux quux corge", ContentType: "code", Domains: []string{"go"}, TokenCost: 1000, Price: 100, SellerReputation: 70, PutTimestamp: time.Now().Add(-2 * time.Hour).UnixNano()},
	}

	// Must not panic.
	idx.Rebuild(entries)

	if idx.Len() != 2 {
		t.Fatalf("after Rebuild, len = %d, want 2", idx.Len())
	}
	if emb.embedCalls != 2 {
		t.Errorf("Embed called %d times during Rebuild, want 2", emb.embedCalls)
	}
}

func entryIDs(results []matching.RankedResult) []string {
	ids := make([]string, len(results))
	for i, r := range results {
		ids[i] = r.EntryID
	}
	return ids
}
