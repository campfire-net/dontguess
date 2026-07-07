package store_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/campfire-net/dontguess/pkg/store"
)

func TestStore_AppendReadAll_PreservesOrderAndFields(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() }) //nolint:errcheck

	recs := []store.Record{
		{ID: "a", CampfireID: "cf1", Sender: "sender-a", Payload: []byte(`{"n":1}`), Tags: []string{"exchange:put"}, Timestamp: 100},
		{ID: "b", CampfireID: "cf1", Sender: "sender-b", Payload: []byte(`{"n":2}`), Tags: []string{"exchange:buy"}, Antecedents: []string{"a"}, Timestamp: 200, Instance: "worker-1"},
		{ID: "c", CampfireID: "cf1", Sender: "sender-a", Payload: []byte(`{"n":3}`), Tags: []string{"exchange:match"}, Antecedents: []string{"b"}, Timestamp: 300},
	}
	for _, r := range recs {
		if err := s.Append(r); err != nil {
			t.Fatalf("Append(%s): %v", r.ID, err)
		}
	}

	got, err := s.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(got) != len(recs) {
		t.Fatalf("ReadAll returned %d records, want %d", len(got), len(recs))
	}
	for i, want := range recs {
		if got[i].ID != want.ID {
			t.Errorf("record %d: ID = %q, want %q (order not preserved)", i, got[i].ID, want.ID)
		}
		if string(got[i].Payload) != string(want.Payload) {
			t.Errorf("record %d: Payload = %q, want %q", i, got[i].Payload, want.Payload)
		}
		if len(got[i].Tags) != 1 || got[i].Tags[0] != want.Tags[0] {
			t.Errorf("record %d: Tags = %v, want %v", i, got[i].Tags, want.Tags)
		}
		if got[i].Timestamp != want.Timestamp {
			t.Errorf("record %d: Timestamp = %d, want %d", i, got[i].Timestamp, want.Timestamp)
		}
		if got[i].Instance != want.Instance {
			t.Errorf("record %d: Instance = %q, want %q", i, got[i].Instance, want.Instance)
		}
	}
}

func TestStore_ReadAll_EmptyBeforeAnyAppend(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() }) //nolint:errcheck

	got, err := s.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("ReadAll on empty store = %d records, want 0", len(got))
	}
}

// TestStore_ReadAll_SurvivesReopen proves durability: a fresh Store handle
// opened on the same path after the writer closed sees everything the prior
// handle appended. This is the single-writer M1 contract — one process, one
// open handle at a time, durable across restart.
func TestStore_ReadAll_SurvivesReopen(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")

	s1, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open (1st): %v", err)
	}
	if err := s1.Append(store.Record{ID: "x", Timestamp: 1}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s2, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open (2nd): %v", err)
	}
	t.Cleanup(func() { s2.Close() }) //nolint:errcheck

	got, err := s2.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(got) != 1 || got[0].ID != "x" {
		t.Fatalf("ReadAll after reopen = %v, want [{ID: x}]", got)
	}

	if err := s2.Append(store.Record{ID: "y", Timestamp: 2}); err != nil {
		t.Fatalf("Append (2nd): %v", err)
	}
	got, err = s2.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll after 2nd append: %v", err)
	}
	if len(got) != 2 || got[0].ID != "x" || got[1].ID != "y" {
		t.Fatalf("ReadAll after reopen+append = %v, want [{x} {y}]", got)
	}
}

// TestStore_ReadAll_RecoversFromTornTailLine reproduces dontguess-2e7: a crash
// mid-Append leaves a truncated final line (no closing brace, no trailing
// newline). ReadAll must return every fully-valid record that precedes the torn
// tail — not zero records with an unmarshal error, which would make the entire
// log unreadable and revert all durably-appended state. Fails pre-fix (ReadAll
// returned nil + error), passes post-fix.
func TestStore_ReadAll_RecoversFromTornTailLine(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")

	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Append(store.Record{ID: "a", Sender: "s1", Tags: []string{"exchange:put"}, Timestamp: 10}); err != nil {
		t.Fatalf("Append(a): %v", err)
	}
	if err := s.Append(store.Record{ID: "b", Sender: "s2", Tags: []string{"exchange:buy"}, Timestamp: 20}); err != nil {
		t.Fatalf("Append(b): %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Simulate a crash mid-Append: raw bytes of a partial record, no closing
	// brace and no trailing newline. This is exactly the shape an interrupted
	// json.Marshal + Write leaves on disk.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("OpenFile for torn append: %v", err)
	}
	if _, err := f.WriteString(`{"id":"c","campfire_id":"cf1","sender":"s3","tags":["exchange:mat`); err != nil {
		t.Fatalf("write torn tail: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close torn file: %v", err)
	}

	s2, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open (reader): %v", err)
	}
	t.Cleanup(func() { s2.Close() }) //nolint:errcheck

	got, err := s2.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll on torn-tail log returned error, want recovery: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ReadAll after torn tail = %d records, want 2 (the valid prefix)", len(got))
	}
	if got[0].ID != "a" || got[1].ID != "b" {
		t.Fatalf("ReadAll after torn tail = [%s %s], want [a b]", got[0].ID, got[1].ID)
	}
}

// TestStore_ReadAll_SurfacesMidLogCorruption proves the torn-tail recovery does
// NOT silently swallow corruption in the MIDDLE of the log. A bad line followed
// by further valid lines is real corruption (single-writer append order means
// only the last line can ever be torn), so ReadAll must return an error rather
// than skip a record and return a shorter-than-actual log.
func TestStore_ReadAll_SurfacesMidLogCorruption(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")

	// Hand-build a log with a corrupt line in the middle (valid, garbage, valid),
	// every line newline-terminated so the garbage is unambiguously mid-log.
	content := `{"id":"a","timestamp":10}` + "\n" +
		`{not valid json at all}` + "\n" +
		`{"id":"c","timestamp":30}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() }) //nolint:errcheck

	_, err = s.ReadAll()
	if err == nil {
		t.Fatal("ReadAll on mid-log corruption returned nil error, want an error (must not silently skip a middle record)")
	}
	if !strings.Contains(err.Error(), "mid-log") {
		t.Fatalf("ReadAll mid-log error = %q, want it to identify mid-log corruption", err.Error())
	}
}

func TestStore_Append_RejectsEmptyID(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() }) //nolint:errcheck

	if err := s.Append(store.Record{ID: "", Timestamp: 1}); err == nil {
		t.Fatal("Append with empty ID: expected error, got nil")
	}
}

func TestStore_Replay_ReturnsMessagesInAppendOrder(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() }) //nolint:errcheck

	if err := s.Append(store.Record{ID: "a", Sender: "s1", Tags: []string{"exchange:put"}, Timestamp: 10}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := s.Append(store.Record{ID: "b", Sender: "s2", Tags: []string{"exchange:buy"}, Antecedents: []string{"a"}, Timestamp: 20}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	msgs, err := s.Replay()
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("Replay returned %d messages, want 2", len(msgs))
	}
	if msgs[0].ID != "a" || msgs[1].ID != "b" {
		t.Fatalf("Replay order = [%s %s], want [a b]", msgs[0].ID, msgs[1].ID)
	}
	if msgs[1].Antecedents[0] != "a" {
		t.Fatalf("Replay msgs[1].Antecedents = %v, want [a]", msgs[1].Antecedents)
	}
}
