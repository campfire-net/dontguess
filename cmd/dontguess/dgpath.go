package main

import (
	"os"
	"path/filepath"
)

// resolveDGHome returns the DG_HOME directory: the DG_HOME environment variable
// if set, otherwise $HOME/.cf. This is the single canonical implementation —
// operator.go (socketPath) and status.go previously each had their own copy.
//
// Socket path: resolveDGHome() + "/ipc/dontguess.sock"
func resolveDGHome() string {
	if dg := os.Getenv("DG_HOME"); dg != "" {
		return dg
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".cf"
	}
	return filepath.Join(home, ".cf")
}
