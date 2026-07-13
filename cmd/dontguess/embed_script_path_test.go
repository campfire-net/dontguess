package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDefaultEmbedScriptPath_FindsRelativeToWorkingDir verifies the fallback
// no longer hardcodes a dev-machine absolute path (dontguess-740). It builds
// a fake tree with cmd/embed/main.py under a temp root, cds into a
// subdirectory of that tree, and confirms the real (unstubbed)
// defaultEmbedScriptPath resolves the script via directory-walk discovery.
func TestDefaultEmbedScriptPath_FindsRelativeToWorkingDir(t *testing.T) {
	root := t.TempDir()
	embedDir := filepath.Join(root, "cmd", "embed")
	if err := os.MkdirAll(embedDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	scriptPath := filepath.Join(embedDir, "main.py")
	if err := os.WriteFile(scriptPath, []byte("# stub embed script\n"), 0o644); err != nil {
		t.Fatalf("write script: %v", err)
	}

	// Simulate running from a nested subdirectory of the repo (e.g. as if
	// the binary/build were invoked from cmd/dontguess), which is the
	// realistic case defaultEmbedScriptPath must handle since os.Executable()
	// on a `go test` binary points into a temp build dir, not the repo.
	nested := filepath.Join(root, "cmd", "dontguess")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("mkdir nested: %v", err)
	}

	origWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(origWd) })
	if err := os.Chdir(nested); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	got := defaultEmbedScriptPath()
	if got == "" {
		t.Fatalf("defaultEmbedScriptPath() returned empty; expected to discover %s via working-directory walk", scriptPath)
	}
	resolvedGot, err := filepath.EvalSymlinks(got)
	if err != nil {
		t.Fatalf("EvalSymlinks(got): %v", err)
	}
	resolvedWant, err := filepath.EvalSymlinks(scriptPath)
	if err != nil {
		t.Fatalf("EvalSymlinks(want): %v", err)
	}
	if resolvedGot != resolvedWant {
		t.Fatalf("defaultEmbedScriptPath() = %q, want %q", got, resolvedWant)
	}
}

// TestDefaultEmbedScriptPath_NoHardcodedDevPath asserts the fix removed the
// original hardcoded absolute path so it can never resurface as a silent
// fallback on machines that lack that exact directory tree.
func TestDefaultEmbedScriptPath_NoHardcodedDevPath(t *testing.T) {
	origWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	tmp := t.TempDir()
	t.Cleanup(func() { _ = os.Chdir(origWd) })
	if err := os.Chdir(tmp); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	got := defaultEmbedScriptPath()
	const hardcoded = "/home/baron/projects/dontguess/cmd/embed/main.py"
	if got == hardcoded {
		t.Fatalf("defaultEmbedScriptPath() returned the old hardcoded dev-machine path %q; fix regressed", hardcoded)
	}
}
