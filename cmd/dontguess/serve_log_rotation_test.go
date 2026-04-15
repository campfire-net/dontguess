package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/natefinch/lumberjack.v2"
)

// ---- Security regression tests for buildLogDest (dontguess-ba9c) ----

// TestBuildLogDest_RejectsSymlink verifies that buildLogDest returns an error
// when the log path is a pre-existing symlink, preventing a symlink attack
// where an attacker redirects operator logs into an arbitrary writable file.
// Regression test for dontguess-ba9c.
func TestBuildLogDest_RejectsSymlink(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	targetPath := filepath.Join(dir, "target.txt")
	if err := os.WriteFile(targetPath, []byte("original content\n"), 0600); err != nil {
		t.Fatalf("creating target file: %v", err)
	}

	// Create a symlink at the expected log path pointing to the target.
	logPath := filepath.Join(dir, "dontguess.log")
	if err := os.Symlink(targetPath, logPath); err != nil {
		t.Fatalf("os.Symlink: %v", err)
	}

	_, err := buildLogDest(dir)
	if err == nil {
		t.Fatal("buildLogDest returned nil error for a symlink log path — symlink attack not prevented")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Errorf("error message %q does not mention 'symlink'", err.Error())
	}
	t.Logf("correctly rejected symlink: %v", err)

	// Verify the target file was NOT written to (no log data appended).
	data, readErr := os.ReadFile(targetPath)
	if readErr != nil {
		t.Fatalf("reading target file: %v", readErr)
	}
	if string(data) != "original content\n" {
		t.Errorf("target file was modified — symlink write occurred: %q", string(data))
	}
}

// TestBuildLogDest_AcceptsRegularFile verifies that buildLogDest succeeds and
// writes to a pre-existing regular log file (not a symlink).
// Regression test for dontguess-ba9c (normal path must still work).
func TestBuildLogDest_AcceptsRegularFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	logPath := filepath.Join(dir, "dontguess.log")
	// Pre-create a regular file (not a symlink).
	if err := os.WriteFile(logPath, []byte("previous log line\n"), 0600); err != nil {
		t.Fatalf("creating log file: %v", err)
	}

	w, err := buildLogDest(dir)
	if err != nil {
		t.Fatalf("buildLogDest returned unexpected error: %v", err)
	}

	const msg = "test line from TestBuildLogDest_AcceptsRegularFile\n"
	if _, writeErr := io.WriteString(w, msg); writeErr != nil {
		t.Fatalf("writing to log dest: %v", writeErr)
	}

	data, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatalf("reading log file: %v", readErr)
	}
	if !strings.Contains(string(data), "test line from TestBuildLogDest_AcceptsRegularFile") {
		t.Errorf("log message not written to file — content: %q", string(data))
	}
	t.Logf("regular file accepted and written successfully")
}

// TestLogRotation_Setup verifies that writing >10MB to the log destination
// triggers lumberjack rotation, leaving the active log file plus at least one
// rotated backup in the temp directory.
func TestLogRotation_Setup(t *testing.T) {
	dir := t.TempDir()

	roller := &lumberjack.Logger{
		Filename:   filepath.Join(dir, "dontguess.log"),
		MaxSize:    10, // 10 MB threshold
		MaxBackups: 5,
		MaxAge:     28,
		Compress:   true,
	}
	defer roller.Close()

	// Write 11 MB of data (1 KB lines × 11264 lines ≈ 11 MB).
	line := strings.Repeat("x", 1023) + "\n" // 1 KB per line
	for i := 0; i < 11264; i++ {
		if _, err := io.WriteString(roller, line); err != nil {
			t.Fatalf("write error at line %d: %v", i, err)
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}

	// Expect: the active dontguess.log plus at least one rotated file.
	var activeFound bool
	var rotatedCount int
	for _, e := range entries {
		name := e.Name()
		if name == "dontguess.log" {
			activeFound = true
		} else if strings.HasPrefix(name, "dontguess-") {
			rotatedCount++
		}
	}

	if !activeFound {
		t.Error("dontguess.log not found after writes")
	}
	if rotatedCount == 0 {
		t.Errorf("no rotated files found after writing 11MB (entries: %v)", func() []string {
			var names []string
			for _, e := range entries {
				names = append(names, e.Name())
			}
			return names
		}())
	}
}

// TestLogRotation_MultiWriter verifies that writes to the combined log
// destination appear both in the captured stderr buffer and in dontguess.log.
func TestLogRotation_MultiWriter(t *testing.T) {
	dir := t.TempDir()

	var stderrBuf bytes.Buffer
	roller := &lumberjack.Logger{
		Filename:   filepath.Join(dir, "dontguess.log"),
		MaxSize:    10,
		MaxBackups: 5,
		MaxAge:     28,
		Compress:   true,
	}
	defer roller.Close()

	dest := io.MultiWriter(&stderrBuf, roller)

	const msg = "hello from multiwriter test\n"
	if _, err := io.WriteString(dest, msg); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Check stderr capture.
	if !strings.Contains(stderrBuf.String(), "hello from multiwriter test") {
		t.Errorf("message not found in stderr buffer: %q", stderrBuf.String())
	}

	// Check file on disk.
	data, err := os.ReadFile(filepath.Join(dir, "dontguess.log"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(data), "hello from multiwriter test") {
		t.Errorf("message not found in dontguess.log: %q", string(data))
	}
}

// TestLogRotation_BackupCap verifies that lumberjack honours MaxBackups=5.
// We write enough data to trigger 7 rotations (7 × 10MB+ chunks) and then
// assert that at most 5 compressed backup files exist in the temp directory.
// The active dontguess.log is not counted as a backup.
// Regression coverage for dontguess-ffa.
func TestLogRotation_BackupCap(t *testing.T) {
	dir := t.TempDir()

	roller := &lumberjack.Logger{
		Filename:   filepath.Join(dir, "dontguess.log"),
		MaxSize:    10, // MB — same as buildLogDest config
		MaxBackups: 5,
		MaxAge:     0,    // no age-based pruning
		Compress:   true, // backups are .gz
	}
	defer roller.Close()

	// Write 7 × ~11 MB = ~77 MB total. Each chunk exceeds the 10 MB threshold,
	// so each flush forces a rotation before the next chunk begins.
	//
	// 11264 lines × 1 KB = ~11 MB per chunk.
	line := strings.Repeat("x", 1023) + "\n"
	for rotation := 0; rotation < 7; rotation++ {
		for i := 0; i < 11264; i++ {
			if _, err := io.WriteString(roller, line); err != nil {
				t.Fatalf("rotation %d line %d: write error: %v", rotation, i, err)
			}
		}
		// Explicitly rotate to flush the current segment before the next chunk.
		if err := roller.Rotate(); err != nil {
			t.Fatalf("rotation %d: Rotate() error: %v", rotation, err)
		}
	}
	roller.Close()

	// Lumberjack compresses old segments asynchronously. Close() does not
	// wait for compression goroutines to finish, so .gz files may still be
	// in flight. Poll until the gz count stabilises or we time out.
	var gzCount int
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("ReadDir: %v", err)
		}
		gzCount = 0
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".gz") {
				gzCount++
			}
		}
		// After 7 rotations with MaxBackups=5, we expect exactly 5 .gz
		// files once compression finishes. Stop polling once we see them.
		if gzCount >= 5 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if gzCount > 5 {
		t.Errorf("MaxBackups=5 not enforced: found %d .gz backup files (want ≤ 5)", gzCount)
	}
	if gzCount == 0 {
		t.Errorf("expected compressed backup files but found none (rotation did not occur)")
	}
	t.Logf("backup cap enforced: %d .gz file(s) present (limit 5)", gzCount)
}

// TestBuildLogDest_UsesPassedPath verifies that buildLogDest writes to the
// explicitly passed dgHome path and does NOT re-derive the path from the
// DG_HOME environment variable (regression for dontguess-34e).
//
// Before the fix, buildLogDest re-read DG_HOME from the environment even
// though the caller had already resolved it and passed it as an argument.
// This caused the log file to appear at the env-derived path instead of the
// caller-controlled path, breaking callers that pass a different directory.
func TestBuildLogDest_UsesPassedPath(t *testing.T) {
	// Cannot use t.Parallel() here — t.Setenv is not allowed with t.Parallel.
	passedDir := t.TempDir()
	envDir := t.TempDir()

	// Point DG_HOME at a *different* directory than what we pass.
	// After the fix, buildLogDest must honour the passed path, not the env var.
	t.Setenv("DG_HOME", envDir)

	dest, err := buildLogDest(passedDir)
	if err != nil {
		t.Fatalf("buildLogDest: %v", err)
	}

	const msg = "log to passed path\n"
	if _, err := io.WriteString(dest, msg); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Log file must appear in passedDir, not envDir.
	logPath := filepath.Join(passedDir, "dontguess.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ReadFile %s: %v — log not written to passed path", logPath, err)
	}
	if !strings.Contains(string(data), "log to passed path") {
		t.Errorf("expected message not in passed-path file: %q", string(data))
	}

	// Confirm nothing was written to the env-override directory.
	envLogPath := filepath.Join(envDir, "dontguess.log")
	if _, statErr := os.Stat(envLogPath); statErr == nil {
		t.Errorf("log file unexpectedly created at env-dir path %s — env override was applied", envLogPath)
	}
	_ = dest
}
