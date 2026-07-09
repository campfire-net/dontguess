package main

import (
	"os"
	"path/filepath"
)

// resolveDGHome returns the DG_HOME directory: the DG_HOME environment variable
// if set, otherwise $HOME/.dontguess. This is the single canonical implementation —
// operator.go (socketPath) and status.go previously each had their own copy.
//
// dontguess is its own portfolio member: its operator identity
// ($DG_HOME/nostr-operator.key), per-agent identities ($DG_HOME/agents/), and
// operator IPC socket live under dontguess's OWN home — NOT under ~/.cf, which
// is cf/rd's identity home (a nostr-operator.key there would collide with rd's
// portfolio key). The legacy campfire SDK config (CF_HOME / ~/.cf) is a separate,
// campfire-era concern and is unaffected by this default.
//
// Socket path: resolveDGHome() + "/ipc/dontguess.sock"
func resolveDGHome() string {
	if dg := os.Getenv("DG_HOME"); dg != "" {
		return dg
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".dontguess"
	}
	return filepath.Join(home, ".dontguess")
}
