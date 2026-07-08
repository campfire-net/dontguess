package store

// White-box test (same package, unlike store_test.go's external
// package store_test) needed to verify BatchAppend's single-fsync
// contract (dontguess-ba98, docs/design/relay-transport.md §2.1: "appends N
// records under one store-mutex hold and ONE fsync"). There is no way to
// count fsync(2) syscalls from a black-box test without OS-level tracing,
// so this substitutes a counting fake behind the unexported fileSyncer
// interface, constructing a Store directly via its unexported fields.

import (
	"os"
	"path/filepath"
	"testing"
)

// syncCounter wraps a real *os.File and counts Sync calls, so the test can
// assert exactly how many fsyncs a given BatchAppend call performed while
// still durably writing to a real file on disk (not a pure in-memory fake —
// we want the real Write/Sync behavior, just instrumented).
type syncCounter struct {
	*os.File
	syncCount *int
}

func (c syncCounter) Sync() error {
	*c.syncCount++
	return c.File.Sync()
}

func TestBatchAppend_SingleFsync(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	count := 0
	s := &Store{path: path, w: syncCounter{File: f, syncCount: &count}}
	t.Cleanup(func() { s.Close() }) //nolint:errcheck

	recs := []Record{
		{ID: "a", Timestamp: 1, Origin: "relay", Seq: 1},
		{ID: "b", Timestamp: 2, Origin: "relay", Seq: 2},
		{ID: "c", Timestamp: 3, Origin: "relay", Seq: 3},
	}
	if err := s.BatchAppend(recs); err != nil {
		t.Fatalf("BatchAppend: %v", err)
	}
	if count != 1 {
		t.Fatalf("Sync called %d times for a 3-record BatchAppend, want exactly 1 (one fsync per batch)", count)
	}

	got, err := s.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("ReadAll after BatchAppend = %d records, want 3", len(got))
	}
}

// TestBatchAppend_NoWriteOnValidationFailure proves the "before the mutex
// hold" ordering claim directly: an invalid batch must not reach Write/Sync
// at all, not merely fail to leave records readable.
func TestBatchAppend_NoWriteOnValidationFailure(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	count := 0
	s := &Store{path: path, w: syncCounter{File: f, syncCount: &count}}
	t.Cleanup(func() { s.Close() }) //nolint:errcheck

	err = s.BatchAppend([]Record{
		{ID: "a", Timestamp: 1},
		{ID: "", Timestamp: 2}, // invalid
	})
	if err == nil {
		t.Fatal("BatchAppend with invalid record: expected error, got nil")
	}
	if count != 0 {
		t.Fatalf("Sync called %d times on a validation-failed BatchAppend, want 0 (fails before mutex/write)", count)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size() != 0 {
		t.Fatalf("log file size = %d after validation-failed BatchAppend, want 0 (no bytes written)", info.Size())
	}
}
