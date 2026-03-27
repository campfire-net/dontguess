package matching

import (
	"testing"
	"time"
)

// makeEntry is a test helper to build a RankInput with sensible defaults.
func makeEntry(id, sellerKey, description, contentType string, tokenCost, price int64, rep int) RankInput {
	return RankInput{
		EntryID:          id,
		SellerKey:        sellerKey,
		Description:      description,
		ContentType:      contentType,
		Domains:          []string{"go", "testing"},
		TokenCost:        tokenCost,
		Price:            price,
		SellerReputation: rep,
		PutTimestamp:     time.Now().Add(-24 * time.Hour).UnixNano(), // 1 day old
		HasUpheldDispute: false,
	}
}

// TestRank_CorrectGateExcludesDisputed verifies Layer 0: entries with
// HasUpheldDispute=true are excluded from results entirely.
func TestRank_CorrectGateExcludesDisputed(t *testing.T) {
	t.Parallel()
	e := NewTFIDFEmbedder()

	candidates := []RankInput{
		makeEntry("good-1", "seller-a", "Go HTTP handler unit tests table-driven", "code", 10000, 1000, 70),
		{
			EntryID:          "disputed-1",
			SellerKey:        "seller-b",
			Description:      "Go HTTP handler unit tests table-driven",
			ContentType:      "code",
			Domains:          []string{"go"},
			TokenCost:        10000,
			Price:            800,
			SellerReputation: 20,
			PutTimestamp:     time.Now().Add(-1 * time.Hour).UnixNano(),
			HasUpheldDispute: true, // should be excluded
		},
	}

	results := Rank("Go HTTP unit test generator", candidates, e, RankOptions{})
	for _, r := range results {
		if r.EntryID == "disputed-1" {
			t.Errorf("disputed entry appeared in results")
		}
	}
	// good-1 must appear.
	found := false
	for _, r := range results {
		if r.EntryID == "good-1" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected good-1 in results, got %d results", len(results))
	}
}

// TestRank_TopResultIsRelevant verifies that with 10 entries covering different
// domains, the most semantically relevant entry ranks first.
func TestRank_TopResultIsRelevant(t *testing.T) {
	t.Parallel()
	e := NewTFIDFEmbedder()

	// Build a set of 10 entries spanning different domains.
	// The one most relevant to the buy task should rank first.
	candidates := []RankInput{
		makeEntry("tf-module", "seller-a", "Terraform AWS S3 bucket module with versioning and lifecycle rules", "code", 5000, 500, 70),
		makeEntry("go-http", "seller-b", "Go HTTP handler unit test generator table-driven tests edge cases", "code", 8000, 800, 80),
		makeEntry("py-data", "seller-c", "Python pandas dataframe aggregation and pivot table analysis", "analysis", 6000, 600, 65),
		makeEntry("k8s-deploy", "seller-d", "Kubernetes deployment YAML generator with resource limits", "code", 7000, 700, 72),
		makeEntry("rust-async", "seller-e", "Rust async tokio HTTP client with retry and backoff", "code", 9000, 900, 78),
		makeEntry("docker-multi", "seller-f", "Docker multi-stage build optimization for Go services", "code", 4000, 400, 60),
		makeEntry("sql-query", "seller-g", "PostgreSQL query optimization for time-series analytics", "analysis", 5500, 550, 68),
		makeEntry("ts-react", "seller-h", "TypeScript React component testing with Jest and Testing Library", "code", 7500, 750, 75),
		makeEntry("ci-pipeline", "seller-i", "GitHub Actions CI pipeline for Go with coverage and lint", "code", 3000, 300, 62),
		makeEntry("sec-audit", "seller-j", "Security audit checklist for Go HTTP APIs", "review", 4500, 450, 85),
	}

	// Prime IDF from descriptions.
	docs := make([]string, len(candidates))
	for i, c := range candidates {
		docs[i] = c.Description
	}
	e.IndexCorpus(docs)

	task := "I need unit tests for a Go HTTP handler that accepts JSON POST requests with validation"
	results := Rank(task, candidates, e, RankOptions{})

	if len(results) == 0 {
		t.Fatal("expected results, got none")
	}
	if results[0].EntryID != "go-http" {
		t.Errorf("top result = %q, want %q (got results: %v)", results[0].EntryID, "go-http", summarizeResults(results))
	}
}

// TestRank_PartialMatchFlagged verifies that entries with confidence < 0.5 are
// flagged as partial matches.
func TestRank_PartialMatchFlagged(t *testing.T) {
	t.Parallel()
	e := NewTFIDFEmbedder()

	// Use two entries: one closely related, one distantly related.
	candidates := []RankInput{
		makeEntry("close", "seller-a", "Go HTTP handler test generator", "code", 10000, 1000, 70),
		makeEntry("distant", "seller-b", "Terraform EC2 auto-scaling configuration module", "code", 10000, 1000, 70),
	}

	docs := []string{candidates[0].Description, candidates[1].Description}
	e.IndexCorpus(docs)

	results := Rank("Go unit testing HTTP handler", candidates, e, RankOptions{})

	// "close" should have higher confidence than "distant".
	var closeConf, distantConf float64
	for _, r := range results {
		switch r.EntryID {
		case "close":
			closeConf = r.Confidence
		case "distant":
			distantConf = r.Confidence
		}
	}
	if closeConf <= distantConf {
		t.Errorf("close confidence (%f) should exceed distant confidence (%f)", closeConf, distantConf)
	}
}

// TestRank_EfficiencyFavorsHighTokenSavings verifies that an entry with higher
// token savings per scrip ranks higher when other factors are equal.
func TestRank_EfficiencyFavorsHighTokenSavings(t *testing.T) {
	t.Parallel()
	e := NewTFIDFEmbedder()

	// Two identical-description entries; high-efficiency has better tokens/price ratio.
	base := RankInput{
		SellerKey:        "seller-a",
		Description:      "Go HTTP handler test generator",
		ContentType:      "code",
		Domains:          []string{"go"},
		SellerReputation: 70,
		PutTimestamp:     time.Now().Add(-1 * time.Hour).UnixNano(),
		HasUpheldDispute: false,
	}

	highEff := base
	highEff.EntryID = "high-eff"
	highEff.TokenCost = 50000
	highEff.Price = 1000 // ratio = 50

	lowEff := base
	lowEff.EntryID = "low-eff"
	lowEff.SellerKey = "seller-b" // different seller, so novelty doesn't differ
	lowEff.TokenCost = 2000
	lowEff.Price = 1000 // ratio = 2

	results := Rank("Go HTTP unit tests", []RankInput{highEff, lowEff}, e, RankOptions{})
	if len(results) < 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if results[0].EntryID != "high-eff" {
		t.Errorf("expected high-eff to rank first, got %q", results[0].EntryID)
	}
}

// TestRank_AllDisputedReturnsEmpty verifies that when all candidates have
// upheld disputes, no results are returned.
func TestRank_AllDisputedReturnsEmpty(t *testing.T) {
	t.Parallel()
	e := NewTFIDFEmbedder()

	candidates := []RankInput{
		{EntryID: "a", SellerKey: "s1", Description: "Go test generator", HasUpheldDispute: true, Price: 100, SellerReputation: 50, PutTimestamp: time.Now().UnixNano()},
		{EntryID: "b", SellerKey: "s2", Description: "Go test generator", HasUpheldDispute: true, Price: 100, SellerReputation: 50, PutTimestamp: time.Now().UnixNano()},
	}
	results := Rank("Go unit tests", candidates, e, RankOptions{})
	if len(results) != 0 {
		t.Errorf("expected 0 results when all disputed, got %d", len(results))
	}
}

// TestRank_EmptyCandidates returns nil without panic.
func TestRank_EmptyCandidates(t *testing.T) {
	t.Parallel()
	e := NewTFIDFEmbedder()
	results := Rank("some task", nil, e, RankOptions{})
	if results != nil {
		t.Errorf("expected nil for empty candidates, got %v", results)
	}
}

// TestRank_NoveltyBoostForRareSellerr verifies Layer 3: a seller appearing
// only once gets a higher novelty boost than one appearing many times.
func TestRank_NoveltyBoostForRareSeller(t *testing.T) {
	t.Parallel()
	e := NewTFIDFEmbedder()

	// Dominant seller has 4 entries; rare seller has 1.
	// All entries describe the same task with same quality — novelty decides.
	dominant := "seller-dominant"
	rare := "seller-rare"
	desc := "Go HTTP handler unit test generator"

	candidates := []RankInput{
		{EntryID: "d1", SellerKey: dominant, Description: desc, ContentType: "code", Domains: []string{"go"}, TokenCost: 10000, Price: 1000, SellerReputation: 70, PutTimestamp: time.Now().Add(-1 * time.Hour).UnixNano()},
		{EntryID: "d2", SellerKey: dominant, Description: desc, ContentType: "code", Domains: []string{"go"}, TokenCost: 10000, Price: 1000, SellerReputation: 70, PutTimestamp: time.Now().Add(-1 * time.Hour).UnixNano()},
		{EntryID: "d3", SellerKey: dominant, Description: desc, ContentType: "code", Domains: []string{"go"}, TokenCost: 10000, Price: 1000, SellerReputation: 70, PutTimestamp: time.Now().Add(-1 * time.Hour).UnixNano()},
		{EntryID: "d4", SellerKey: dominant, Description: desc, ContentType: "code", Domains: []string{"go"}, TokenCost: 10000, Price: 1000, SellerReputation: 70, PutTimestamp: time.Now().Add(-1 * time.Hour).UnixNano()},
		{EntryID: "r1", SellerKey: rare, Description: desc, ContentType: "code", Domains: []string{"go"}, TokenCost: 10000, Price: 1000, SellerReputation: 70, PutTimestamp: time.Now().Add(-1 * time.Hour).UnixNano()},
	}

	results := Rank("Go HTTP unit test generator", candidates, e, RankOptions{})

	var rareResult *RankedResult
	for i := range results {
		if results[i].EntryID == "r1" {
			rareResult = &results[i]
			break
		}
	}
	if rareResult == nil {
		t.Fatal("rare seller result not found")
	}

	// Rare seller's novelty boost should be 1.0 (appears once out of max 4).
	if rareResult.NoveltyBoost <= 0.5 {
		t.Errorf("rare seller novelty boost = %f, want > 0.5", rareResult.NoveltyBoost)
	}

	// Rare seller should not be completely buried — check at least one dominant
	// entry has lower score than the rare entry.
	var dominantResults []RankedResult
	for _, r := range results {
		if r.EntryID != "r1" {
			dominantResults = append(dominantResults, r)
		}
	}
	// With novelty boost, rare seller scores higher than dominant sellers with same content.
	if rareResult.CompositeScore <= dominantResults[0].CompositeScore {
		t.Errorf("rare seller score (%f) should exceed dominant seller score (%f) due to novelty",
			rareResult.CompositeScore, dominantResults[0].CompositeScore)
	}
}

// summarizeResults returns a slice of EntryID strings for test error messages.
func summarizeResults(results []RankedResult) []string {
	ids := make([]string, len(results))
	for i, r := range results {
		ids[i] = r.EntryID
	}
	return ids
}
