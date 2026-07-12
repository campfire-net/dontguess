package store_test

import (
	"errors"
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

// TestStore_BatchAppend_AllOrNothing proves BatchAppend's atomicity
// contract (dontguess-ba98, docs/design/relay-transport.md §2.1): a batch
// containing one invalid record must fail the WHOLE call, and no record
// from that batch — not even the valid ones that sorted earlier in the
// slice — may ever become observable via ReadAll. Validation/marshal
// happens entirely before the mutex is taken or any byte touches the file.
func TestStore_BatchAppend_AllOrNothing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() }) //nolint:errcheck

	recs := []store.Record{
		{ID: "a", Timestamp: 1, Origin: "relay", Seq: 1},
		{ID: "", Timestamp: 2, Origin: "relay", Seq: 2}, // invalid: empty ID
		{ID: "c", Timestamp: 3, Origin: "relay", Seq: 3},
	}
	if err := s.BatchAppend(recs); err == nil {
		t.Fatal("BatchAppend with an invalid record in the batch: expected error, got nil")
	}

	got, err := s.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("BatchAppend partial failure leaked %d records onto disk, want 0 (all-or-nothing)", len(got))
	}

	// A subsequent valid batch must succeed and be the ONLY content —
	// proving the failed batch left no residue for the next call to build on.
	valid := []store.Record{{ID: "x", Timestamp: 10, Origin: "local", Seq: 1}}
	if err := s.BatchAppend(valid); err != nil {
		t.Fatalf("BatchAppend (valid, after failed batch): %v", err)
	}
	got, err = s.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll after valid batch: %v", err)
	}
	if len(got) != 1 || got[0].ID != "x" {
		t.Fatalf("ReadAll after valid batch = %v, want exactly [{ID: x}]", got)
	}
}

// TestStore_BatchAppend_EmptyBatchIsNoop proves BatchAppend(nil/empty) does
// not write a spurious empty line or error.
func TestStore_BatchAppend_EmptyBatchIsNoop(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() }) //nolint:errcheck

	if err := s.BatchAppend(nil); err != nil {
		t.Fatalf("BatchAppend(nil): %v", err)
	}
	got, err := s.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("ReadAll after BatchAppend(nil) = %d records, want 0", len(got))
	}
}

// TestStore_BatchAppend_PreservesOrderAndOriginSeq proves BatchAppend
// preserves slice order (single-writer append-order-is-fold-order) and that
// Origin/Seq round-trip losslessly through the JSONL persist+ReadAll path
// (dontguess-ba98, docs/design/relay-transport.md §2.1).
func TestStore_BatchAppend_PreservesOrderAndOriginSeq(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() }) //nolint:errcheck

	recs := []store.Record{
		{ID: "a", Sender: "s1", Timestamp: 10, Origin: "local", Seq: 1},
		{ID: "b", Sender: "s2", Timestamp: 20, Antecedents: []string{"a"}, Origin: "relay", Seq: 2},
		{ID: "c", Sender: "s3", Timestamp: 30, Antecedents: []string{"b"}, Origin: "relay", Seq: 3},
	}
	if err := s.BatchAppend(recs); err != nil {
		t.Fatalf("BatchAppend: %v", err)
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
			t.Errorf("record %d: ID = %q, want %q (batch order not preserved)", i, got[i].ID, want.ID)
		}
		if got[i].Origin != want.Origin {
			t.Errorf("record %d (%s): Origin = %q, want %q", i, want.ID, got[i].Origin, want.Origin)
		}
		if got[i].Seq != want.Seq {
			t.Errorf("record %d (%s): Seq = %d, want %d", i, want.ID, got[i].Seq, want.Seq)
		}
	}

	// Origin/Seq must also round-trip through Replay()'s underlying ReadAll
	// call — Replay itself converts to proto.Message (which has no
	// Origin/Seq fields by design), but the record-level read they're
	// backed by must still preserve them until that conversion.
	msgs, err := s.Replay()
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(msgs) != len(recs) {
		t.Fatalf("Replay returned %d messages, want %d", len(msgs), len(recs))
	}
	for i, want := range recs {
		if msgs[i].ID != want.ID {
			t.Errorf("Replay message %d: ID = %q, want %q", i, msgs[i].ID, want.ID)
		}
	}
}

// TestStore_Append_And_BatchAppend_InterleavedPreserveOrder proves the two
// write paths share the same mutex/append discipline: mixing Append and
// BatchAppend calls on one Store still yields a single strictly-ordered log.
func TestStore_Append_And_BatchAppend_InterleavedPreserveOrder(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() }) //nolint:errcheck

	if err := s.Append(store.Record{ID: "a", Timestamp: 1, Origin: "local"}); err != nil {
		t.Fatalf("Append(a): %v", err)
	}
	if err := s.BatchAppend([]store.Record{
		{ID: "b", Timestamp: 2, Origin: "relay", Seq: 1},
		{ID: "c", Timestamp: 3, Origin: "relay", Seq: 2},
	}); err != nil {
		t.Fatalf("BatchAppend: %v", err)
	}
	if err := s.Append(store.Record{ID: "d", Timestamp: 4, Origin: "local"}); err != nil {
		t.Fatalf("Append(d): %v", err)
	}

	got, err := s.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	wantIDs := []string{"a", "b", "c", "d"}
	if len(got) != len(wantIDs) {
		t.Fatalf("ReadAll returned %d records, want %d", len(got), len(wantIDs))
	}
	for i, want := range wantIDs {
		if got[i].ID != want {
			t.Errorf("record %d: ID = %q, want %q", i, got[i].ID, want)
		}
	}
}

// TestRecord_ToMessage_IgnoresOriginAndSeq proves ToMessage does not leak
// Origin/Seq into the proto.Message it produces (docs/design/relay-transport.md
// §2.1: "ToMessage ignores them (no proto.Message change)"). Two records
// identical except for Origin/Seq must convert to identical messages.
func TestRecord_ToMessage_IgnoresOriginAndSeq(t *testing.T) {
	t.Parallel()
	base := store.Record{
		ID:          "a",
		CampfireID:  "cf1",
		Sender:      "s1",
		Payload:     []byte(`{"n":1}`),
		Tags:        []string{"exchange:put"},
		Antecedents: []string{"z"},
		Timestamp:   100,
		Instance:    "worker-1",
	}
	local := base
	local.Origin = "local"
	local.Seq = 0

	relay := base
	relay.Origin = "relay"
	relay.Seq = 42

	msgLocal := local.ToMessage()
	msgRelay := relay.ToMessage()

	if msgLocal.ID != msgRelay.ID || msgLocal.Sender != msgRelay.Sender ||
		msgLocal.Timestamp != msgRelay.Timestamp || msgLocal.Instance != msgRelay.Instance ||
		string(msgLocal.Payload) != string(msgRelay.Payload) {
		t.Fatalf("ToMessage output differs between Origin/Seq variants: local=%+v relay=%+v", msgLocal, msgRelay)
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

// TestStore_AppendAfterClose_ReturnsErrStoreClosed verifies the teardown-race
// contract (dontguess-fe9f): once Close has run, Append and BatchAppend fail
// fast with store.ErrStoreClosed instead of writing to a closed fd (which
// produces a racy, non-deterministic "file already closed" OS error). This is
// the deterministic shutdown signal the exchange engine relies on so a leaked
// writer goroutine racing teardown degrades gracefully rather than corrupting
// the log or panicking.
func TestStore_AppendAfterClose_ReturnsErrStoreClosed(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Append(store.Record{ID: "a", Sender: "s1", Tags: []string{"exchange:put"}, Timestamp: 10}); err != nil {
		t.Fatalf("Append(a) before close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Single Append after Close: a clean ErrStoreClosed, not an fd error.
	err = s.Append(store.Record{ID: "b", Sender: "s2", Tags: []string{"exchange:buy"}, Timestamp: 20})
	if !errors.Is(err, store.ErrStoreClosed) {
		t.Fatalf("Append after Close = %v, want ErrStoreClosed", err)
	}
	// BatchAppend after Close: same contract.
	err = s.BatchAppend([]store.Record{{ID: "c", Sender: "s3", Tags: []string{"exchange:match"}, Timestamp: 30}})
	if !errors.Is(err, store.ErrStoreClosed) {
		t.Fatalf("BatchAppend after Close = %v, want ErrStoreClosed", err)
	}
	// Close is idempotent: a second Close is a no-op returning nil.
	if err := s.Close(); err != nil {
		t.Fatalf("second Close = %v, want nil (idempotent)", err)
	}

	// The post-close appends must not have landed: ReadAll (own fd) still sees
	// only the pre-close record.
	got, err := s.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll after close: %v", err)
	}
	if len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("ReadAll after close = %v, want exactly [a]", got)
	}
}
