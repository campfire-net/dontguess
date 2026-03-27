package matching

import (
	"math"
	"sync"
	"testing"
)

func TestTFIDFEmbedder_Tokenize(t *testing.T) {
	tests := []struct {
		text string
		want []string
	}{
		{"Go HTTP handler", []string{"go", "http", "handler"}},
		{"Unit test generator for Go", []string{"unit", "test", "generator", "go"}},
		{"  multiple   spaces  ", []string{"multiple", "spaces"}},
		{"a", nil}, // single char filtered
		{"", nil},
	}
	for _, tt := range tests {
		got := tokenize(tt.text)
		if len(got) != len(tt.want) {
			t.Errorf("tokenize(%q) = %v, want %v", tt.text, got, tt.want)
			continue
		}
		for i, tok := range got {
			if tok != tt.want[i] {
				t.Errorf("tokenize(%q)[%d] = %q, want %q", tt.text, i, tok, tt.want[i])
			}
		}
	}
}

func TestTFIDFEmbedder_EmbedSameText(t *testing.T) {
	e := NewTFIDFEmbedder()
	a := e.Embed("Go HTTP test generator")
	b := e.Embed("Go HTTP test generator")
	// Identical text must embed to identical vectors.
	sim := e.Similarity(a, b)
	if math.Abs(sim-1.0) > 1e-9 {
		t.Errorf("identical text similarity = %f, want 1.0", sim)
	}
}

func TestTFIDFEmbedder_EmptyText(t *testing.T) {
	e := NewTFIDFEmbedder()
	v := e.Embed("")
	if len(v) != 0 {
		t.Errorf("empty text embed len = %d, want 0", len(v))
	}
	// Similarity with empty vector should be 0.
	v2 := e.Embed("go http")
	sim := e.Similarity(v, v2)
	if sim != 0 {
		t.Errorf("empty vs non-empty similarity = %f, want 0", sim)
	}
}

func TestTFIDFEmbedder_SimilarTexts(t *testing.T) {
	e := NewTFIDFEmbedder()
	docs := []string{
		"Go HTTP handler unit test generator table-driven",
		"Terraform AWS S3 module versioning",
		"Python data science pandas dataframe",
		"Kubernetes deployment yaml generator",
	}
	e.IndexCorpus(docs)

	// Similar texts should score higher than dissimilar.
	queryEmb := e.Embed("Go HTTP unit test")
	goEmb := e.Embed(docs[0])    // very similar
	tfEmb := e.Embed(docs[1])    // dissimilar
	pyEmb := e.Embed(docs[2])    // dissimilar

	goSim := e.Similarity(queryEmb, goEmb)
	tfSim := e.Similarity(queryEmb, tfEmb)
	pySim := e.Similarity(queryEmb, pyEmb)

	if goSim <= tfSim {
		t.Errorf("go similarity (%f) should exceed terraform similarity (%f)", goSim, tfSim)
	}
	if goSim <= pySim {
		t.Errorf("go similarity (%f) should exceed python similarity (%f)", goSim, pySim)
	}
}

func TestCosineSimilarity_ZeroVector(t *testing.T) {
	a := []float64{0, 0, 0}
	b := []float64{1, 2, 3}
	sim := cosineSimilarity(a, b)
	if sim != 0 {
		t.Errorf("zero vector cosine similarity = %f, want 0", sim)
	}
}

func TestCosineSimilarity_OrthogonalVectors(t *testing.T) {
	a := []float64{1, 0}
	b := []float64{0, 1}
	sim := cosineSimilarity(a, b)
	if math.Abs(sim) > 1e-9 {
		t.Errorf("orthogonal vectors similarity = %f, want ~0", sim)
	}
}

func TestCosineSimilarity_MismatchedLength(t *testing.T) {
	a := []float64{1, 0, 0}
	b := []float64{1, 0}
	// Missing dimension treated as 0 — should still be 1.0 for identical prefix.
	sim := cosineSimilarity(a, b)
	if math.Abs(sim-1.0) > 1e-9 {
		t.Errorf("mismatched length same prefix similarity = %f, want 1.0", sim)
	}
}

// TestTFIDFEmbedder_ConcurrentEmbed verifies that concurrent calls to Embed
// do not produce data races on the shared vocabID map.
// Run with: CGO_ENABLED=1 go test -race ./pkg/matching/... (requires gcc).
// The test exercises the same race path regardless — any race will also
// produce unpredictable results or panic under the standard scheduler.
func TestTFIDFEmbedder_ConcurrentEmbed(t *testing.T) {
	e := NewTFIDFEmbedder()
	e.IndexCorpus([]string{
		"Go HTTP handler unit test generator table-driven",
		"Terraform AWS S3 module versioning",
		"Python data science pandas dataframe",
		"Kubernetes deployment yaml generator",
	})

	texts := []string{
		"Go HTTP unit test",
		"AWS Terraform module",
		"pandas dataframe analysis",
		"kubernetes yaml deploy",
		"brand new unseen vocabulary term xyz",
	}

	const goroutines = 20
	const iters = 50

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		g := g
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				text := texts[(g+i)%len(texts)]
				v := e.Embed(text)
				// Embed must return a non-nil slice for non-empty input.
				if v == nil {
					t.Errorf("Embed(%q) returned nil", text)
				}
			}
		}()
	}
	wg.Wait()
}

// TestTFIDFEmbedder_ConcurrentSearch verifies no data race when Search is
// called concurrently on an Index backed by a TFIDFEmbedder.
// Run with: CGO_ENABLED=1 go test -race ./pkg/matching/... (requires gcc).
func TestTFIDFEmbedder_ConcurrentSearch(t *testing.T) {
	emb := NewTFIDFEmbedder()
	entries := []RankInput{
		{EntryID: "e1", Description: "Go HTTP handler unit test"},
		{EntryID: "e2", Description: "Terraform AWS S3 module"},
		{EntryID: "e3", Description: "Python pandas dataframe"},
	}
	idx := NewIndex(emb, RankOptions{})
	idx.Rebuild(entries)

	queries := []string{
		"Go HTTP unit test",
		"AWS Terraform module",
		"pandas dataframe analysis",
		"completely unseen query terms qwerty",
	}

	const goroutines = 20
	const iters = 50

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		g := g
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				q := queries[(g+i)%len(queries)]
				results := idx.Search(q, 3)
				_ = results
			}
		}()
	}
	wg.Wait()
}
