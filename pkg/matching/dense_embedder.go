package matching

import (
	"encoding/json"
	"fmt"
	"math"
	"os/exec"
	"strings"
)

// DenseEmbedder calls the embed service (cmd/embed/main.py) to produce
// 384-dim all-MiniLM-L6-v2 embeddings via ONNX runtime.
//
// Implements the Embedder interface. Each call shells out to Python —
// use for index builds and searches, not hot loops.
type DenseEmbedder struct {
	// ScriptPath is the path to cmd/embed/main.py.
	// If empty, "cmd/embed/main.py" is used (relative to working dir).
	ScriptPath string
}

// NewDenseEmbedder returns an embedder backed by the Python ONNX service.
func NewDenseEmbedder(scriptPath string) *DenseEmbedder {
	if scriptPath == "" {
		scriptPath = "cmd/embed/main.py"
	}
	return &DenseEmbedder{ScriptPath: scriptPath}
}

// Embed returns a 384-dim normalized vector for the given text.
func (e *DenseEmbedder) Embed(text string) []float64 {
	result, err := e.embedTexts([]string{text})
	if err != nil || len(result) == 0 {
		return nil
	}
	return result[0]
}

// EmbedBatch returns vectors for multiple texts in a single call.
func (e *DenseEmbedder) EmbedBatch(texts []string) [][]float64 {
	result, err := e.embedTexts(texts)
	if err != nil {
		return nil
	}
	return result
}

// Similarity returns cosine similarity between two embeddings.
func (e *DenseEmbedder) Similarity(a, b []float64) float64 {
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

type embedResponse struct {
	Vector  []float64   `json:"vector,omitempty"`
	Vectors [][]float64 `json:"vectors,omitempty"`
}

func (e *DenseEmbedder) embedTexts(texts []string) ([][]float64, error) {
	args := []string{e.ScriptPath, "embed", "--json"}
	args = append(args, texts...)

	cmd := exec.Command("python3", args...)
	// Suppress stderr (model loading messages, GPU warnings).
	cmd.Stderr = nil
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("embed: %w (output: %s)", err, strings.TrimSpace(string(out)))
	}

	var resp embedResponse
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, fmt.Errorf("embed: parsing response: %w", err)
	}

	if resp.Vectors != nil {
		return resp.Vectors, nil
	}
	if resp.Vector != nil {
		return [][]float64{resp.Vector}, nil
	}
	return nil, fmt.Errorf("embed: empty response")
}
