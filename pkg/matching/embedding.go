// Package matching implements semantic similarity search and 4-layer ranking
// for the DontGuess exchange engine.
//
// Embedding approach for v0.1: TF-IDF bag-of-words with cosine similarity.
// The interface is pluggable — swap in all-MiniLM-L6-v2 (384-dim) via ONNX
// when operational at scale without changing callers.
package matching

import (
	"math"
	"regexp"
	"strings"
)

// Embedder computes a vector embedding for a text string.
// The embedding dimension is implementation-defined.
//
// v0.1 implementation: sparse TF-IDF bag-of-words over a vocabulary built
// from the corpus of descriptions at index time. Cosine similarity over
// this sparse representation approximates semantic search for short task
// descriptions well enough for a functional exchange.
//
// Future: swap in all-MiniLM-L6-v2 (384-dim float32 dense vector) via ONNX
// runtime or HTTP sidecar. The interface is stable across both approaches.
type Embedder interface {
	// Embed returns a vector representation of text.
	// The returned slice must not be modified by callers.
	Embed(text string) []float64
	// Similarity returns the cosine similarity between two embeddings.
	// Returns a value in [-1, 1], higher is more similar.
	// Callers must pass embeddings produced by the same Embedder.
	Similarity(a, b []float64) float64
}

// CorpusIndexer is an optional interface implemented by Embedder instances
// that benefit from a corpus-wide IDF pass before per-entry embedding.
// Index.Rebuild calls IndexCorpus when the embedder implements this interface.
// Embedders that compute fixed-dimension dense vectors (e.g., all-MiniLM-L6-v2)
// do not need to implement this interface.
type CorpusIndexer interface {
	IndexCorpus(docs []string)
}

// NewTFIDFEmbedder returns a corpus-free TF-IDF embedder.
//
// The vocabulary is built on-the-fly from the text passed to Embed.
// Two embeddings are comparable if produced by the same TFIDFEmbedder
// instance with the same internal vocabulary state at the time of embedding.
//
// For the exchange matching engine, the recommended pattern is:
//  1. Build the embedder once at engine startup.
//  2. Call IndexCorpus(descriptions) to prime the IDF weights.
//  3. Embed all inventory descriptions to build the index.
//  4. Embed buy task descriptions at query time.
//
// Without IndexCorpus, the embedder uses term frequency only (TF, no IDF).
// This is sufficient for functional matching but rewards common words more.
func NewTFIDFEmbedder() *TFIDFEmbedder {
	return &TFIDFEmbedder{
		idf:     make(map[string]float64),
		vocabID: make(map[string]int),
	}
}

// TFIDFEmbedder is a sparse TF-IDF bag-of-words embedder.
// Safe for concurrent reads after IndexCorpus; mutations are not concurrent-safe.
type TFIDFEmbedder struct {
	// idf maps term -> inverse document frequency weight.
	// Populated by IndexCorpus.
	idf map[string]float64

	// vocabID maps term -> dense vector index.
	// Built from IndexCorpus + subsequent Embed calls.
	vocabID map[string]int
}

// IndexCorpus computes IDF weights from a slice of documents.
// Must be called before Embed for meaningful TF-IDF results.
// Calling IndexCorpus multiple times replaces the previous IDF weights.
func (e *TFIDFEmbedder) IndexCorpus(docs []string) {
	N := len(docs)
	if N == 0 {
		return
	}

	// Count document frequency for each term.
	df := make(map[string]int)
	for _, doc := range docs {
		seen := make(map[string]bool)
		for _, tok := range tokenize(doc) {
			if !seen[tok] {
				df[tok]++
				seen[tok] = true
			}
		}
	}

	// Compute IDF: log((N+1) / (df+1)) + 1 (smoothed, avoids zero).
	e.idf = make(map[string]float64, len(df))
	for term, count := range df {
		e.idf[term] = math.Log(float64(N+1)/float64(count+1)) + 1.0
		// Ensure vocabulary index exists.
		if _, ok := e.vocabID[term]; !ok {
			e.vocabID[term] = len(e.vocabID)
		}
	}
}

// Embed returns a TF-IDF vector for the given text.
// Returns a dense vector indexed by the internal vocabulary.
// New terms encountered outside the corpus are assigned new vocab IDs with
// IDF weight 1.0 (neutral — no corpus evidence).
func (e *TFIDFEmbedder) Embed(text string) []float64 {
	tokens := tokenize(text)
	if len(tokens) == 0 {
		// Return a zero-length vector; Similarity handles the zero-norm case.
		return []float64{}
	}

	// Compute term frequencies.
	tf := make(map[string]float64)
	for _, tok := range tokens {
		tf[tok]++
	}
	total := float64(len(tokens))
	for k := range tf {
		tf[k] /= total
	}

	// Assign vocab IDs to any new terms.
	for term := range tf {
		if _, ok := e.vocabID[term]; !ok {
			e.vocabID[term] = len(e.vocabID)
		}
	}

	// Build dense vector. Size = current vocabulary.
	dim := len(e.vocabID)
	vec := make([]float64, dim)
	for term, freq := range tf {
		id := e.vocabID[term]
		idf := e.idf[term]
		if idf == 0 {
			idf = 1.0 // neutral weight for unseen terms
		}
		vec[id] = freq * idf
	}
	return vec
}

// Similarity returns the cosine similarity between two embedding vectors.
// Returns 0 for zero-length or zero-norm vectors.
func (e *TFIDFEmbedder) Similarity(a, b []float64) float64 {
	return cosineSimilarity(a, b)
}

// cosineSimilarity computes cosine similarity between two vectors.
// Handles mismatched lengths by treating missing dimensions as 0.
// Returns 0 for zero-norm inputs.
func cosineSimilarity(a, b []float64) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}

	// Extend shorter vector with zeros.
	maxLen := len(a)
	if len(b) > maxLen {
		maxLen = len(b)
	}

	dot, normA, normB := 0.0, 0.0, 0.0
	for i := 0; i < maxLen; i++ {
		ai, bi := 0.0, 0.0
		if i < len(a) {
			ai = a[i]
		}
		if i < len(b) {
			bi = b[i]
		}
		dot += ai * bi
		normA += ai * ai
		normB += bi * bi
	}

	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}

// wordBoundary matches sequences of non-alphanumeric characters.
var wordBoundary = regexp.MustCompile(`[^a-z0-9]+`)

// tokenize splits text into lowercase alphanumeric tokens, filtering
// common English stop words to improve signal-to-noise ratio.
func tokenize(text string) []string {
	lowered := strings.ToLower(text)
	parts := wordBoundary.Split(lowered, -1)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if len(p) < 2 {
			continue
		}
		if stopWords[p] {
			continue
		}
		out = append(out, p)
	}
	return out
}

// stopWords is a minimal set of English stop words that add noise to
// task-description matching.
var stopWords = map[string]bool{
	"a": true, "an": true, "and": true, "are": true, "as": true,
	"at": true, "be": true, "by": true, "do": true, "for": true,
	"from": true, "has": true, "have": true, "he": true, "in": true,
	"is": true, "it": true, "its": true, "of": true, "on": true,
	"or": true, "that": true, "the": true, "this": true, "to": true,
	"was": true, "with": true, "you": true, "your": true, "we": true,
	"our": true, "they": true, "will": true, "not": true, "but": true,
	"if": true, "can": true, "all": true, "so": true, "up": true,
	"which": true, "when": true, "how": true, "what": true, "given": true,
	"using": true, "into": true, "would": true, "about": true, "than": true,
	"also": true, "any": true, "more": true, "been": true, "me": true,
}
