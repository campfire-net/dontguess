package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/natefinch/lumberjack.v2"
)

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

// TestLogRotation_DGHomeOverride verifies that buildLogDest respects the
// DG_HOME environment variable when constructing the log file path.
func TestLogRotation_DGHomeOverride(t *testing.T) {
	dir := t.TempDir()

	// Set DG_HOME to the temp dir; buildLogDest should use it.
	t.Setenv("DG_HOME", dir)

	dest := buildLogDest("/should/not/be/used")

	const msg = "dg_home override test line\n"
	if _, err := io.WriteString(dest, msg); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Close lumberjack so the file is flushed (extract roller from MultiWriter
	// via type assertion — we know the structure from buildLogDest).
	type multiWriterCloser interface {
		io.Writer
	}
	// We can't easily reach the lumberjack logger through io.MultiWriter, so
	// read the file directly after a sync via an explicit small write.
	// The file should exist at $DG_HOME/dontguess.log.
	logPath := filepath.Join(dir, "dontguess.log")

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ReadFile %s: %v — DG_HOME override not respected", logPath, err)
	}
	if !strings.Contains(string(data), "dg_home override test line") {
		t.Errorf("expected message not in file: %q", string(data))
	}
	_ = dest
}
