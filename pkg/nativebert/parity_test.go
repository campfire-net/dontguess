package nativebert

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// referenceModelDir is the local cache the reference cmd/embed script populates.
// The parity tests reuse it so they need no network access.
func referenceModelDir(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("home dir: %v", err)
	}
	return filepath.Join(home, ".local", "lib", "embed", "all-MiniLM-L6-v2")
}

func loadTestModel(t *testing.T) (*bertModel, *wordPiece) {
	t.Helper()
	dir := referenceModelDir(t)
	stPath := filepath.Join(dir, "model.safetensors")
	tjPath := filepath.Join(dir, "tokenizer.json")
	if _, err := os.Stat(stPath); err != nil {
		t.Skipf("model.safetensors not present (%s); fetch it to run parity", stPath)
	}
	if _, err := os.Stat(tjPath); err != nil {
		t.Skipf("tokenizer.json not present (%s)", tjPath)
	}

	stBytes, err := os.ReadFile(stPath)
	if err != nil {
		t.Fatalf("read safetensors: %v", err)
	}
	tensors, err := parseSafetensors(stBytes)
	if err != nil {
		t.Fatalf("parseSafetensors: %v", err)
	}
	model, err := newBertModel(tensors, defaultConfig())
	if err != nil {
		t.Fatalf("newBertModel: %v", err)
	}
	tjBytes, err := os.ReadFile(tjPath)
	if err != nil {
		t.Fatalf("read tokenizer.json: %v", err)
	}
	wp, err := newWordPiece(tjBytes)
	if err != nil {
		t.Fatalf("newWordPiece: %v", err)
	}
	return model, wp
}

func cosine(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// TestSafetensorsShapes verifies the loader produced tensors with the shapes
// config.json specifies, guarding against a silently-misparsed checkpoint.
func TestSafetensorsShapes(t *testing.T) {
	dir := referenceModelDir(t)
	stPath := filepath.Join(dir, "model.safetensors")
	if _, err := os.Stat(stPath); err != nil {
		t.Skipf("model.safetensors not present (%s)", stPath)
	}
	stBytes, err := os.ReadFile(stPath)
	if err != nil {
		t.Fatalf("read safetensors: %v", err)
	}
	tensors, err := parseSafetensors(stBytes)
	if err != nil {
		t.Fatalf("parseSafetensors: %v", err)
	}
	want := map[string][]int{
		"embeddings.word_embeddings.weight":       {30522, 384},
		"embeddings.position_embeddings.weight":   {512, 384},
		"embeddings.token_type_embeddings.weight": {2, 384},
		"encoder.layer.0.attention.self.query.weight": {384, 384},
		"encoder.layer.0.intermediate.dense.weight":   {1536, 384},
		"encoder.layer.5.output.dense.weight":         {384, 1536},
	}
	for name, shape := range want {
		ts, ok := tensors[name]
		if !ok {
			t.Errorf("missing tensor %q", name)
			continue
		}
		if len(ts.shape) != len(shape) || ts.shape[0] != shape[0] || ts.shape[len(shape)-1] != shape[len(shape)-1] {
			t.Errorf("tensor %q shape = %v, want %v", name, ts.shape, shape)
		}
	}
}

// TestForwardParityWithReference is the correctness gate: the pure-Go forward
// pass must reproduce the ONNX reference embeddings within cosine >= 0.9999.
func TestForwardParityWithReference(t *testing.T) {
	model, wp := loadTestModel(t)

	goldenBytes, err := os.ReadFile("testdata/embedding_golden.json")
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	var golden struct {
		Cases []struct {
			Text   string    `json:"text"`
			Vector []float32 `json:"vector"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(goldenBytes, &golden); err != nil {
		t.Fatalf("parse golden: %v", err)
	}

	const minCosine = 0.9999
	for _, c := range golden.Cases {
		got := model.embed(wp.encode(c.Text))
		if len(got) != len(c.Vector) {
			t.Errorf("%q: got dim %d, want %d", c.Text, len(got), len(c.Vector))
			continue
		}
		cos := cosine(got, c.Vector)
		if cos < minCosine {
			t.Errorf("%q: cosine %.6f < %.4f (parity FAIL)", c.Text, cos, minCosine)
		} else {
			t.Logf("%q: cosine %.6f OK", c.Text, cos)
		}
	}
}
