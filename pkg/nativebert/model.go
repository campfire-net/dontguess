// Package nativebert is a pure-Go (CGO_ENABLED=0), dependency-light
// implementation of the all-MiniLM-L6-v2 sentence-embedding model. It replaces
// the former python3 + onnxruntime sidecar (cmd/embed/main.py): a WordPiece
// tokenizer, a 6-layer BERT encoder forward pass, mean pooling, and L2
// normalization, all in Go. Output is numerically identical to the ONNX
// reference (cosine >= 0.9999; see parity_test.go).
//
// The model weights (model.safetensors, ~87 MB) and tokenizer.json are
// downloaded once from the HuggingFace hub into a local cache, exactly as the
// old python path fetched the ONNX model — no python, no shared library, and
// no model bytes embedded in the binary.
package nativebert

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

const (
	modelRepo      = "sentence-transformers/all-MiniLM-L6-v2"
	safetensorsURL = "https://huggingface.co/" + modelRepo + "/resolve/main/model.safetensors"
	tokenizerURL   = "https://huggingface.co/" + modelRepo + "/resolve/main/tokenizer.json"

	// Sanity floors: a truncated download must not be accepted as valid.
	minSafetensorsBytes = 80 << 20 // real file ~87 MB
	minTokenizerBytes   = 100 << 10 // real file ~455 KB

	// EmbeddingDim is the fixed output dimension of the model.
	EmbeddingDim = 384
)

// Embedder is a loaded, ready-to-run MiniLM embedder. It is safe for
// concurrent use: embed only reads immutable model state.
type Embedder struct {
	model *bertModel
	wp    *wordPiece
}

// Load returns an Embedder, downloading model.safetensors and tokenizer.json
// into cacheDir if they are not already present and valid. If cacheDir is
// empty, DefaultCacheDir() is used. The download happens at most once per
// machine; subsequent loads read straight from the cache.
func Load(cacheDir string) (*Embedder, error) {
	if cacheDir == "" {
		cacheDir = DefaultCacheDir()
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return nil, fmt.Errorf("nativebert: mkdir cache %s: %w", cacheDir, err)
	}

	stPath := filepath.Join(cacheDir, "model.safetensors")
	tjPath := filepath.Join(cacheDir, "tokenizer.json")
	if err := ensureFile(stPath, safetensorsURL, minSafetensorsBytes); err != nil {
		return nil, err
	}
	if err := ensureFile(tjPath, tokenizerURL, minTokenizerBytes); err != nil {
		return nil, err
	}

	return LoadFromFiles(stPath, tjPath)
}

// LoadFromFiles builds an Embedder directly from an on-disk safetensors
// checkpoint and tokenizer.json, without any network access. Useful for tests
// and for operators who provision the model files out of band.
func LoadFromFiles(safetensorsPath, tokenizerPath string) (*Embedder, error) {
	stBytes, err := os.ReadFile(safetensorsPath)
	if err != nil {
		return nil, fmt.Errorf("nativebert: read %s: %w", safetensorsPath, err)
	}
	tensors, err := parseSafetensors(stBytes)
	if err != nil {
		return nil, err
	}
	model, err := newBertModel(tensors, defaultConfig())
	if err != nil {
		return nil, err
	}
	tjBytes, err := os.ReadFile(tokenizerPath)
	if err != nil {
		return nil, fmt.Errorf("nativebert: read %s: %w", tokenizerPath, err)
	}
	wp, err := newWordPiece(tjBytes)
	if err != nil {
		return nil, err
	}
	return &Embedder{model: model, wp: wp}, nil
}

// Embed returns the 384-dim L2-normalized embedding for a single text.
func (e *Embedder) Embed(text string) []float32 {
	return e.model.embed(e.wp.encode(text))
}

// EmbedBatch returns embeddings for multiple texts.
func (e *Embedder) EmbedBatch(texts []string) [][]float32 {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = e.Embed(t)
	}
	return out
}

// DefaultCacheDir returns the model cache directory:
// $XDG_CACHE_HOME/dontguess/models/all-MiniLM-L6-v2 (or ~/.cache/… ), falling
// back to $TMPDIR.
func DefaultCacheDir() string {
	const sub = "dontguess/models/all-MiniLM-L6-v2"
	if x := os.Getenv("XDG_CACHE_HOME"); x != "" {
		return filepath.Join(x, sub)
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".cache", sub)
	}
	return filepath.Join(os.TempDir(), sub)
}

// ensureFile downloads url to path (atomically, via a .tmp + rename) unless a
// file of at least minBytes already exists there.
func ensureFile(path, url string, minBytes int64) error {
	if fi, err := os.Stat(path); err == nil && fi.Size() >= minBytes {
		return nil
	}

	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("nativebert: download %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("nativebert: download %s: HTTP %d", url, resp.StatusCode)
	}

	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("nativebert: create %s: %w", tmp, err)
	}
	n, err := io.Copy(f, resp.Body)
	closeErr := f.Close()
	if err != nil {
		os.Remove(tmp)
		return fmt.Errorf("nativebert: write %s: %w", tmp, err)
	}
	if closeErr != nil {
		os.Remove(tmp)
		return fmt.Errorf("nativebert: close %s: %w", tmp, closeErr)
	}
	if n < minBytes {
		os.Remove(tmp)
		return fmt.Errorf("nativebert: %s truncated (%d bytes < %d)", url, n, minBytes)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("nativebert: rename %s: %w", path, err)
	}
	return nil
}
