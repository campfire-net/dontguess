package main

// status_test.go — feature tests for dontguess-6f2.
//
// Tests for `dontguess status`: wrapper attempt log reader, since filter,
// JSON output schema, operator-dead path, socket-unreachable path, and
// the exchange view reader (using a real in-process campfire harness).
//
// All tests use real file I/O, real JSON parsing, and real socket operations
// where possible. No mocks.

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/campfire-net/campfire/pkg/protocol"
	"github.com/campfire-net/campfire/pkg/store"
	"github.com/campfire-net/dontguess/pkg/exchange"
)

// --------------------------------------------------------------------------
// Helpers
// --------------------------------------------------------------------------

// writeAttemptLog writes JSONL lines to $dir/dontguess-attempts.log.
func writeAttemptLog(t *testing.T, dir string, lines []map[string]any) {
	t.Helper()
	path := filepath.Join(dir, "dontguess-attempts.log")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create attempts log: %v", err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, l := range lines {
		if err := enc.Encode(l); err != nil {
			t.Fatalf("write log line: %v", err)
		}
	}
}

// tsRFC3339 converts a time.Time to the RFC3339 string used in attempt log.
// The wrapper writes: date -u +%Y-%m-%dT%H:%M:%SZ (second precision, UTC, Z suffix).
func tsRFC3339(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

// --------------------------------------------------------------------------
// TestStatus_WrapperLogReader
// --------------------------------------------------------------------------

// TestStatus_WrapperLogReader creates a tmpdir with JSONL lines in the REAL
// wrapper format (RFC3339 ts, exit+tag, no "result" field) and asserts correct
// aggregate counts.
//
// This test was updated as part of dontguess-5ce: previously used float ts
// and a "result" field that the wrapper never emits. The new format matches
// the wrapper heredoc in site/install.sh.
func TestStatus_WrapperLogReader(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	now := time.Now()

	// Use the real wrapper format: RFC3339 ts, exit, tag — no "result" field.
	writeAttemptLog(t, dir, []map[string]any{
		{"ts": tsRFC3339(now), "pid": 100, "cmd": "buy", "exit": 0, "tag": "success", "cf_home": "/tmp/cf", "cwd": "/home/user", "caller": nil},
		{"ts": tsRFC3339(now), "pid": 101, "cmd": "buy", "exit": 1, "tag": "operator_down", "cf_home": "/tmp/cf", "cwd": "/home/user", "caller": nil},
		{"ts": tsRFC3339(now), "pid": 102, "cmd": "put", "exit": 1, "tag": "identity_wrapped", "cf_home": "/tmp/cf", "cwd": "/home/user", "caller": nil},
		{"ts": tsRFC3339(now), "pid": 103, "cmd": "buy", "exit": 1, "tag": "other", "cf_home": "/tmp/cf", "cwd": "/home/user", "caller": nil},
	})

	cutoff := now.Add(-time.Hour)
	wa := readWrapperLog(dir, cutoff)

	if wa.Total != 4 {
		t.Errorf("Total = %d, want 4", wa.Total)
	}
	if wa.Success != 1 {
		t.Errorf("Success = %d, want 1", wa.Success)
	}
	if wa.Failed != 3 {
		t.Errorf("Failed = %d, want 3 (operator_down + identity_wrapped + other)", wa.Failed)
	}
	if wa.ByTag["success"] != 1 {
		t.Errorf("ByTag[success] = %d, want 1", wa.ByTag["success"])
	}
	if wa.ByTag["operator_down"] != 1 {
		t.Errorf("ByTag[operator_down] = %d, want 1", wa.ByTag["operator_down"])
	}
	if wa.ByTag["identity_wrapped"] != 1 {
		t.Errorf("ByTag[identity_wrapped] = %d, want 1", wa.ByTag["identity_wrapped"])
	}
	if wa.ByTag["other"] != 1 {
		t.Errorf("ByTag[other] = %d, want 1", wa.ByTag["other"])
	}
}

// --------------------------------------------------------------------------
// TestStatus_SinceFilter
// --------------------------------------------------------------------------

// TestStatus_SinceFilter writes lines with timestamps inside and outside the
// since window and asserts only the in-window lines are counted.
func TestStatus_SinceFilter(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	now := time.Now()
	cutoff := now.Add(-time.Hour)

	inside1 := now.Add(-30 * time.Minute)
	inside2 := now.Add(-10 * time.Minute)
	outside1 := now.Add(-2 * time.Hour)
	outside2 := now.Add(-90 * time.Minute)

	writeAttemptLog(t, dir, []map[string]any{
		{"ts": tsRFC3339(inside1), "pid": 1, "cmd": "buy", "exit": 0, "tag": "success", "cf_home": "", "cwd": "", "caller": nil},
		{"ts": tsRFC3339(inside2), "pid": 2, "cmd": "buy", "exit": 0, "tag": "success", "cf_home": "", "cwd": "", "caller": nil},
		{"ts": tsRFC3339(outside1), "pid": 3, "cmd": "buy", "exit": 0, "tag": "success", "cf_home": "", "cwd": "", "caller": nil},
		{"ts": tsRFC3339(outside2), "pid": 4, "cmd": "buy", "exit": 0, "tag": "success", "cf_home": "", "cwd": "", "caller": nil},
	})

	wa := readWrapperLog(dir, cutoff)

	if wa.Total != 2 {
		t.Errorf("Total = %d, want 2 (only inside-window lines)", wa.Total)
	}
}

// --------------------------------------------------------------------------
// TestStatus_WrapperLogMalformedLines
// --------------------------------------------------------------------------

// TestStatus_WrapperLogMalformedLines verifies that malformed JSON lines are
// skipped without a panic, and valid lines are still counted.
func TestStatus_WrapperLogMalformedLines(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	now := time.Now()
	path := filepath.Join(dir, "dontguess-attempts.log")

	// Write a mix of valid and invalid lines using the real wrapper format.
	content := fmt.Sprintf(
		`{"ts":"%s","pid":1,"cmd":"buy","exit":0,"tag":"success","cf_home":"","cwd":"","caller":null}
not-json
{"ts":"%s","pid":2,"cmd":"buy","exit":1,"tag":"other","cf_home":"","cwd":"","caller":null}
{"broken":
`, tsRFC3339(now), tsRFC3339(now))
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("write log: %v", err)
	}

	cutoff := now.Add(-time.Hour)
	wa := readWrapperLog(dir, cutoff)

	if wa.Total != 2 {
		t.Errorf("Total = %d, want 2 (malformed lines skipped)", wa.Total)
	}
}

// --------------------------------------------------------------------------
// TestStatus_JSONOutput
// --------------------------------------------------------------------------

// TestStatus_JSONOutput calls collectStatus with a tmpdir that has no
// exchange config (so exchange counts will be zero), serializes to JSON,
// and verifies schema_version and required keys.
func TestStatus_JSONOutput(t *testing.T) {
	// NOTE: no t.Parallel() — uses t.Setenv which requires serial execution.

	dir := t.TempDir()
	now := time.Now()

	// Write a small attempt log so WrapperAttempts is non-empty (real wrapper format).
	writeAttemptLog(t, dir, []map[string]any{
		{"ts": tsRFC3339(now), "pid": 1, "cmd": "buy", "exit": 0, "tag": "success", "cf_home": "", "cwd": "", "caller": nil},
	})

	// Set DG_HOME so status reads from the tmpdir (no exchange config → zero counts).
	t.Setenv("DG_HOME", dir)

	snap, err := collectStatus(dir, time.Hour)
	if err != nil {
		t.Fatalf("collectStatus: %v", err)
	}

	data, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	// Verify schema_version.
	sv, ok := raw["schema_version"]
	if !ok {
		t.Error("missing key schema_version")
	} else if sv.(float64) != 1 {
		t.Errorf("schema_version = %v, want 1", sv)
	}

	// Verify all required top-level keys.
	required := []string{"schema_version", "since", "wrapper_attempts", "exchange", "operator"}
	for _, key := range required {
		if _, ok := raw[key]; !ok {
			t.Errorf("missing required key %q in JSON output", key)
		}
	}

	// Verify exchange sub-keys.
	exc, ok := raw["exchange"].(map[string]any)
	if !ok {
		t.Fatal("exchange key is not an object")
	}
	excKeys := []string{"buys", "matches", "settlements", "puts_submitted", "puts_accepted", "puts_rejected"}
	for _, k := range excKeys {
		if _, ok := exc[k]; !ok {
			t.Errorf("missing exchange key %q", k)
		}
	}

	// Verify operator sub-keys.
	op, ok := raw["operator"].(map[string]any)
	if !ok {
		t.Fatal("operator key is not an object")
	}
	opKeys := []string{"pid", "alive", "uptime_seconds", "store_size_bytes"}
	for _, k := range opKeys {
		if _, ok := op[k]; !ok {
			t.Errorf("missing operator key %q", k)
		}
	}
}

// --------------------------------------------------------------------------
// TestStatus_OperatorDead
// --------------------------------------------------------------------------

// TestStatus_OperatorDead points DG_HOME at a tmpdir with no pid file.
// Asserts alive=false and no panic.
func TestStatus_OperatorDead(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// No pid file, no socket.
	h := readOperatorHealth(dir, time.Now().Add(-time.Hour), 0)

	if h.Alive {
		t.Errorf("Alive = true, want false (no pid file)")
	}
	if h.PID != 0 {
		t.Errorf("PID = %d, want 0 (no pid file)", h.PID)
	}
}

// --------------------------------------------------------------------------
// TestStatus_SocketUnreachable
// --------------------------------------------------------------------------

// TestStatus_SocketUnreachable points DG_HOME at a tmpdir with no socket.
// Asserts puts_held is nil and note is set.
func TestStatus_SocketUnreachable(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// No socket file at dir/dontguess.sock.
	held, note := readHeldCount(dir)
	if held != nil {
		t.Errorf("held = %d, want nil (socket not present)", *held)
	}
	if note == "" {
		t.Error("note is empty, want non-empty error note")
	}
}

// --------------------------------------------------------------------------
// TestStatus_ExchangeReader
// --------------------------------------------------------------------------

// TestStatus_ExchangeReader creates a real in-process exchange campfire,
// inserts a few put/buy/settle messages directly into the store, then calls
// readExchangeViewsWithClient and asserts the counts are correct.
func TestStatus_ExchangeReader(t *testing.T) {
	t.Parallel()

	cfHome := t.TempDir()
	transportDir := t.TempDir()
	convDir := conventionDirForOpTest(t)

	cfg, initClient, err := exchange.Init(exchange.InitOptions{
		ConfigDir:     cfHome,
		Transport:     protocol.FilesystemTransport{Dir: transportDir},
		BeaconDir:     t.TempDir(),
		ConventionDir: convDir,
	})
	if err != nil {
		t.Fatalf("exchange.Init: %v", err)
	}
	t.Cleanup(func() { initClient.Close() })

	st, err := store.Open(store.StorePath(cfHome))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	cfID := cfg.ExchangeCampfireID
	cutoff := time.Now().Add(-2 * time.Hour)

	// Insert real store messages for each tag we count.
	insertMsg := func(tag string) {
		t.Helper()
		rec := store.MessageRecord{
			ID:          fmt.Sprintf("test-%d-%s", time.Now().UnixNano(), tag),
			CampfireID:  cfID,
			Sender:      "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
			Payload:     []byte(`{}`),
			Tags:        []string{tag},
			Antecedents: []string{},
			Timestamp:   store.NowNano(),
			Signature:   []byte{},
		}
		if _, err := st.AddMessage(rec); err != nil {
			t.Fatalf("AddMessage tag=%s: %v", tag, err)
		}
	}

	// Insert: 2 puts, 1 buy, 1 match, 1 settle, 1 put-accept, 1 put-reject.
	insertMsg(exchange.TagPut)
	insertMsg(exchange.TagPut)
	insertMsg(exchange.TagBuy)
	insertMsg(exchange.TagMatch)
	insertMsg(exchange.TagSettle)
	insertMsg(exchange.TagPhasePrefix + exchange.SettlePhaseStrPutAccept)
	insertMsg(exchange.TagPhasePrefix + exchange.SettlePhaseStrPutReject)

	// Build a read client against the same store.
	readClient := protocol.New(st, nil)
	counts := readExchangeViewsWithClient(readClient, st, cfID, cutoff)

	if counts.PutsSubmitted != 2 {
		t.Errorf("PutsSubmitted = %d, want 2", counts.PutsSubmitted)
	}
	if counts.Buys != 1 {
		t.Errorf("Buys = %d, want 1", counts.Buys)
	}
	if counts.Matches != 1 {
		t.Errorf("Matches = %d, want 1", counts.Matches)
	}
	if counts.Settlements != 1 {
		t.Errorf("Settlements = %d, want 1", counts.Settlements)
	}
	if counts.PutsAccepted != 1 {
		t.Errorf("PutsAccepted = %d, want 1", counts.PutsAccepted)
	}
	if counts.PutsRejected != 1 {
		t.Errorf("PutsRejected = %d, want 1", counts.PutsRejected)
	}
}

// --------------------------------------------------------------------------
// TestStatus_SocketHeldCount
// --------------------------------------------------------------------------

// TestStatus_SocketHeldCount starts a real unix socket server that mimics the
// operator's list-held response, and asserts readHeldCount returns the correct count.
func TestStatus_SocketHeldCount(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	sockPath := filepath.Join(dir, "dontguess.sock")

	// Start a minimal unix socket server.
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	// Serve one connection with a canned list-held response (3 held puts).
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		// Read the request (ignore content).
		var req map[string]any
		json.NewDecoder(conn).Decode(&req) //nolint:errcheck
		// Write 3 held puts.
		json.NewEncoder(conn).Encode(map[string]any{ //nolint:errcheck
			"puts": []any{
				map[string]any{"put_msg_id": "aaa", "token_cost": 100, "seller": "key1"},
				map[string]any{"put_msg_id": "bbb", "token_cost": 200, "seller": "key2"},
				map[string]any{"put_msg_id": "ccc", "token_cost": 300, "seller": "key3"},
			},
		})
	}()

	held, note := readHeldCount(dir)
	<-done

	if note != "" {
		t.Errorf("unexpected note %q, want empty (socket reachable)", note)
	}
	if held == nil {
		t.Fatal("held is nil, want non-nil")
	}
	if *held != 3 {
		t.Errorf("held = %d, want 3", *held)
	}
}

// --------------------------------------------------------------------------
// TestStatus_WrapperLogReader_RealWrapperFormat
// --------------------------------------------------------------------------

// TestStatus_WrapperLogReader_RealWrapperFormat is the regression test for
// dontguess-5ce. It uses the EXACT log line format emitted by the wrapper
// heredoc in site/install.sh to verify the reader correctly parses real output.
//
// Real wrapper line: {"ts":"2026-04-11T12:00:00Z","pid":12345,"cmd":"buy",
//
//	"exit":0,"tag":"success","cf_home":"/home/user/.cf","cwd":"/home/user",
//	"caller":null}
//
// This test FAILS against the old code (which used float ts and relied on
// a "result" field that the wrapper never writes).
func TestStatus_WrapperLogReader_RealWrapperFormat(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "dontguess-attempts.log")

	// Exact format from the wrapper _log_attempt function.
	// All tags the wrapper can emit: success, no_exchange_configured,
	// operator_down, identity_wrapped, not_admitted, other.
	lines := []string{
		`{"ts":"2026-04-11T10:00:00Z","pid":1000,"cmd":"buy","exit":0,"tag":"success","cf_home":"/home/user/.cf","cwd":"/home/user","caller":null}`,
		`{"ts":"2026-04-11T10:01:00Z","pid":1001,"cmd":"put","exit":1,"tag":"no_exchange_configured","cf_home":"","cwd":"/home/user","caller":null}`,
		`{"ts":"2026-04-11T10:02:00Z","pid":1002,"cmd":"buy","exit":1,"tag":"operator_down","cf_home":"/home/user/.cf","cwd":"/home/user","caller":null}`,
		`{"ts":"2026-04-11T10:03:00Z","pid":1003,"cmd":"buy","exit":1,"tag":"identity_wrapped","cf_home":"/tmp/cf-abc","cwd":"/home/user","caller":null}`,
		`{"ts":"2026-04-11T10:04:00Z","pid":1004,"cmd":"put","exit":1,"tag":"not_admitted","cf_home":"/home/user/.cf","cwd":"/home/user","caller":null}`,
		`{"ts":"2026-04-11T10:05:00Z","pid":1005,"cmd":"buy","exit":1,"tag":"other","cf_home":"/home/user/.cf","cwd":"/home/user","caller":null}`,
	}
	content := ""
	for _, l := range lines {
		content += l + "\n"
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("write attempt log: %v", err)
	}

	// Cutoff before all lines (2026-04-11T09:00:00Z) so all 6 lines are in-window.
	cutoff, _ := time.Parse(time.RFC3339, "2026-04-11T09:00:00Z")
	wa := readWrapperLog(dir, cutoff)

	if wa.Total != 6 {
		t.Errorf("Total = %d, want 6", wa.Total)
	}
	if wa.Success != 1 {
		t.Errorf("Success = %d, want 1 (only exit=0 tag=success counts)", wa.Success)
	}
	if wa.Failed != 5 {
		t.Errorf("Failed = %d, want 5", wa.Failed)
	}

	// All 6 tags must be present in ByTag.
	expectedTags := []string{"success", "no_exchange_configured", "operator_down", "identity_wrapped", "not_admitted", "other"}
	for _, tag := range expectedTags {
		if wa.ByTag[tag] != 1 {
			t.Errorf("ByTag[%q] = %d, want 1", tag, wa.ByTag[tag])
		}
	}
}
