package matching

import (
	"math"
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
