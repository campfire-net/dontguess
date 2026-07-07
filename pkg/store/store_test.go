package store_test

import (
	"path/filepath"
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
