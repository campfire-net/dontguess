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
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// ErrModelNotCached is returned by LoadCached when the model files are not yet
// present in the cache. Callers that must never block on a network download
// (e.g. the serve startup path) use this to fall back cleanly without fetching.
var ErrModelNotCached = errors.New("nativebert: model not cached")

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
	if err := Fetch(cacheDir); err != nil {
		return nil, err
	}
	stPath, tjPath := modelPaths(cacheDir)
	return LoadFromFiles(stPath, tjPath)
}

// modelPaths returns the cached model.safetensors and tokenizer.json paths for
// cacheDir (empty → DefaultCacheDir).
func modelPaths(cacheDir string) (stPath, tjPath string) {
	if cacheDir == "" {
		cacheDir = DefaultCacheDir()
	}
	return filepath.Join(cacheDir, "model.safetensors"), filepath.Join(cacheDir, "tokenizer.json")
}

// Cached reports whether both model files are already present and non-truncated
// in cacheDir, i.e. whether LoadCached will succeed without any network access.
func Cached(cacheDir string) bool {
	stPath, tjPath := modelPaths(cacheDir)
	st, err := os.Stat(stPath)
	if err != nil || st.Size() < minSafetensorsBytes {
		return false
	}
	tj, err := os.Stat(tjPath)
	return err == nil && tj.Size() >= minTokenizerBytes
}

// LoadCached loads the embedder only if the model is already cached, never
// downloading. It returns ErrModelNotCached when the files are absent so a
// caller on a latency-sensitive path (serve startup) can fall back to TF-IDF
// without blocking on a ~87 MB fetch.
func LoadCached(cacheDir string) (*Embedder, error) {
	if !Cached(cacheDir) {
		return nil, ErrModelNotCached
	}
	stPath, tjPath := modelPaths(cacheDir)
	return LoadFromFiles(stPath, tjPath)
}

// Fetch downloads the model files into cacheDir (empty → DefaultCacheDir) if
// they are not already present, without loading them. It is the explicit,
// blocking way to provision the model out of the serve hot path (see the
// `dontguess embed pull` command).
func Fetch(cacheDir string) error {
	if cacheDir == "" {
		cacheDir = DefaultCacheDir()
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return fmt.Errorf("nativebert: mkdir cache %s: %w", cacheDir, err)
	}
	stPath, tjPath := modelPaths(cacheDir)
	if err := ensureFile(stPath, safetensorsURL, minSafetensorsBytes); err != nil {
		return err
	}
	return ensureFile(tjPath, tokenizerURL, minTokenizerBytes)
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
