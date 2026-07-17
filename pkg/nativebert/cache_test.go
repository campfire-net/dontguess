package nativebert

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestLoadCached_NotCached verifies the non-blocking serve path: with an empty
// cache dir, LoadCached must return ErrModelNotCached and MUST NOT attempt a
// network download. Cached() must agree.
func TestLoadCached_NotCached(t *testing.T) {
	dir := t.TempDir()
	if Cached(dir) {
		t.Fatalf("Cached reported true for empty dir %s", dir)
	}
	_, err := LoadCached(dir)
	if !errors.Is(err, ErrModelNotCached) {
		t.Fatalf("LoadCached(empty) err = %v, want ErrModelNotCached", err)
	}
}

// TestCached_TruncatedFilesRejected verifies a truncated (partial) download is
// not mistaken for a valid cache — Cached must require the size floors.
func TestCached_TruncatedFilesRejected(t *testing.T) {
	dir := t.TempDir()
	// Write undersized stand-ins for both files.
	if err := os.WriteFile(filepath.Join(dir, "model.safetensors"), []byte("truncated"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tokenizer.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if Cached(dir) {
		t.Fatalf("Cached reported true for truncated files")
	}
}
