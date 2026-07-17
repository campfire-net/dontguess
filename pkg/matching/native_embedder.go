package matching

import (
	"math"

	"github.com/3dl-dev/dontguess/pkg/nativebert"
)

// NativeEmbedder adapts the pure-Go nativebert MiniLM model to the Embedder
// interface. It produces 384-dim all-MiniLM-L6-v2 embeddings with no python,
// no onnxruntime, and no shared library — the model runs entirely in Go.
//
// It replaces DenseEmbedder (which shelled out to python3 cmd/embed/main.py).
// Output is numerically equivalent to that ONNX path (cosine >= 0.9999).
type NativeEmbedder struct {
	e *nativebert.Embedder
}

// NewNativeEmbedder loads the MiniLM model from cacheDir (downloading it once
// from HuggingFace if absent; empty cacheDir uses nativebert.DefaultCacheDir).
// It returns an error if the model cannot be loaded — callers that want a
// graceful TF-IDF fallback should handle that error, not panic.
func NewNativeEmbedder(cacheDir string) (*NativeEmbedder, error) {
	e, err := nativebert.Load(cacheDir)
	if err != nil {
		return nil, err
	}
	return &NativeEmbedder{e: e}, nil
}

// NewNativeEmbedderFromFiles loads the MiniLM model directly from on-disk
// safetensors + tokenizer.json without any network access. Useful for tests
// and out-of-band-provisioned model files.
func NewNativeEmbedderFromFiles(safetensorsPath, tokenizerPath string) (*NativeEmbedder, error) {
	e, err := nativebert.LoadFromFiles(safetensorsPath, tokenizerPath)
	if err != nil {
		return nil, err
	}
	return &NativeEmbedder{e: e}, nil
}

// Embed returns a 384-dim normalized vector for the given text.
func (n *NativeEmbedder) Embed(text string) []float64 {
	v := n.e.Embed(text)
	out := make([]float64, len(v))
	for i, f := range v {
		out[i] = float64(f)
	}
	return out
}

// EmbedBatch returns vectors for multiple texts.
func (n *NativeEmbedder) EmbedBatch(texts []string) [][]float64 {
	vecs := n.e.EmbedBatch(texts)
	out := make([][]float64, len(vecs))
	for i, v := range vecs {
		row := make([]float64, len(v))
		for j, f := range v {
			row[j] = float64(f)
		}
		out[i] = row
	}
	return out
}

// Similarity returns cosine similarity between two embeddings.
func (n *NativeEmbedder) Similarity(a, b []float64) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	dot, normA, normB := 0.0, 0.0, 0.0
	for i := 0; i < len(a) && i < len(b); i++ {
		dot += a[i] * b[i]
		normA += a[i] * a[i]
		normB += b[i] * b[i]
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}
