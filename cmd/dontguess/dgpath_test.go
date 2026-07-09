package main

import (
	"os"
	"path/filepath"
	"testing"
)

// resolveDGHome must default to ~/.dontguess (dontguess's OWN home), never ~/.cf
// (cf/rd's identity home). See dgpath.go for the rationale (operator key collision).
func TestResolveDGHome_DefaultsToDontguessHome(t *testing.T) {
	t.Setenv("DG_HOME", "")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	got := resolveDGHome()
	want := filepath.Join(home, ".dontguess")
	if got != want {
		t.Fatalf("resolveDGHome() default = %q, want %q (must NOT be ~/.cf)", got, want)
	}
	if got == filepath.Join(home, ".cf") {
		t.Fatalf("resolveDGHome() defaulted to ~/.cf — collides with cf/rd identity home")
	}
}

func TestResolveDGHome_EnvOverride(t *testing.T) {
	t.Setenv("DG_HOME", "/tmp/explicit-dg-home")
	if got := resolveDGHome(); got != "/tmp/explicit-dg-home" {
		t.Fatalf("resolveDGHome() = %q, want the explicit DG_HOME", got)
	}
}
