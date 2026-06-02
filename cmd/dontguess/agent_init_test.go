package main

// agent_init_test.go — ground-source tests for dontguess agent-init subcommand.
//
// Decision reference: docs/design/exchange-per-agent-identity-decision.md §7
// Item: dontguess-04f
//
// All tests run against a scratch filesystem-transport exchange — NOT ~/.cf.
// No ~/.cf mutations occur; all state lives in t.TempDir() paths.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/campfire-net/campfire/cf-protocol/protocol"
	"github.com/campfire-net/dontguess/pkg/exchange"
)

// scratchExchange initializes a scratch exchange in a temp dir and returns
// (dgHome, exchangeCampfireID, transportDir). All paths are isolated temp dirs.
// The returned operatorClient must be closed by the caller (use t.Cleanup).
//
// This mirrors newOpTestHarness but is kept minimal and test-local so it
// does not grow as other tests add harness methods.
func scratchExchange(t *testing.T) (dgHome string, cfg *exchange.Config) {
	t.Helper()
	dgHome = t.TempDir()
	transportDir := t.TempDir()
	convDir := conventionDirForOpTest(t)

	var initClient *protocol.Client
	var err error
	cfg, initClient, err = exchange.Init(exchange.InitOptions{
		ConfigDir:         dgHome,
		Transport:         protocol.FilesystemTransport{Dir: transportDir},
		BeaconDir:         t.TempDir(),
		ConventionDir:     convDir,
		SkipConfigCascade: true,
	})
	if err != nil {
		t.Fatalf("exchange.Init: %v", err)
	}
	t.Cleanup(func() { initClient.Close() })
	return dgHome, cfg
}

// runAgentInitWith calls runAgentInit with DG_HOME overridden to dgHome.
// Returns any error from the command. stdout (the export line) goes to os.Stdout
// but that's fine for tests — we check side effects, not stdout.
func runAgentInitWith(t *testing.T, dgHome, name string) error {
	t.Helper()
	// Override DG_HOME for the duration of this call.
	orig := os.Getenv("DG_HOME")
	os.Setenv("DG_HOME", dgHome)
	defer os.Setenv("DG_HOME", orig)

	return runAgentInit(nil, []string{name})
}

// agentPubKey reads the public key hex from an agent home directory by loading
// the identity.json via protocol.Init (idempotent — just loads, never re-generates
// because identity.json already exists).
func agentPubKey(t *testing.T, agentHome string) string {
	t.Helper()
	client, _, err := protocol.Init(agentHome)
	if err != nil {
		t.Fatalf("protocol.Init(%s): %v", agentHome, err)
	}
	defer client.Close()
	return client.PublicKeyHex()
}

// TestAgentInit_GeneratesDistinctIdentity verifies:
//  1. Two distinct names produce two distinct Ed25519 public keys.
//  2. Re-running the same name yields the SAME key (idempotency — no clobber).
//  3. Each agent home is under $DG_HOME/agents/<name>/.
//  4. Admit and join run against a scratch campfire (not ~/.cf).
func TestAgentInit_GeneratesDistinctIdentity(t *testing.T) {
	t.Parallel()

	dgHome, _ := scratchExchange(t)

	// --- init alice ---
	if err := runAgentInitWith(t, dgHome, "alice"); err != nil {
		t.Fatalf("agent-init alice: %v", err)
	}
	aliceHome := filepath.Join(dgHome, "agents", "alice")
	aliceKey1 := agentPubKey(t, aliceHome)
	if aliceKey1 == "" {
		t.Fatal("alice pubkey is empty")
	}

	// --- init bob ---
	if err := runAgentInitWith(t, dgHome, "bob"); err != nil {
		t.Fatalf("agent-init bob: %v", err)
	}
	bobHome := filepath.Join(dgHome, "agents", "bob")
	bobKey := agentPubKey(t, bobHome)
	if bobKey == "" {
		t.Fatal("bob pubkey is empty")
	}

	// Assert 1: alice and bob have distinct public keys.
	if aliceKey1 == bobKey {
		t.Errorf("alice and bob have the same pubkey %s — expected distinct identities", aliceKey1)
	}

	// Assert 2: idempotency — re-init alice yields the same key (no clobber).
	if err := runAgentInitWith(t, dgHome, "alice"); err != nil {
		t.Fatalf("agent-init alice (2nd run): %v", err)
	}
	aliceKey2 := agentPubKey(t, aliceHome)
	if aliceKey1 != aliceKey2 {
		t.Errorf("alice pubkey changed on re-init: first=%s second=%s — identity was clobbered", aliceKey1, aliceKey2)
	}

	// Assert 3: agent homes are at expected paths.
	for _, home := range []string{aliceHome, bobHome} {
		if _, err := os.Stat(filepath.Join(home, "identity.json")); err != nil {
			t.Errorf("identity.json missing at %s: %v", home, err)
		}
	}

	// Assert 4: scratch-only — DG_HOME is a temp dir, not ~/.cf.
	userHome, err := os.UserHomeDir()
	if err == nil {
		realCF := filepath.Join(userHome, ".cf")
		if strings.HasPrefix(dgHome, realCF) {
			t.Errorf("dgHome %s is inside ~/.cf — test must use a scratch dir", dgHome)
		}
	}

	// Assert 5: both agents are admitted to (and joined) the exchange campfire.
	// Verify by loading each agent client and checking its stored membership.
	for _, tc := range []struct {
		name string
		home string
	}{
		{"alice", aliceHome},
		{"bob", bobHome},
	} {
		aClient, _, err := protocol.Init(tc.home)
		if err != nil {
			t.Fatalf("protocol.Init(%s): %v", tc.name, err)
		}
		defer aClient.Close()

		// GetMembership reads from the agent's own store (populated by Join).
		// This verifies the real join path ran, not just the admit path.
		m, err := aClient.GetMembership(agentExchangeID(t, dgHome))
		if err != nil {
			t.Errorf("%s GetMembership: %v", tc.name, err)
			continue
		}
		if m == nil {
			t.Errorf("%s is not a member of the exchange campfire — join did not run", tc.name)
		}
	}
}

// agentExchangeID reads the exchange campfire ID from the dgHome config.
func agentExchangeID(t *testing.T, dgHome string) string {
	t.Helper()
	cfg, err := exchange.LoadConfig(dgHome)
	if err != nil {
		t.Fatalf("LoadConfig(%s): %v", dgHome, err)
	}
	return cfg.ExchangeCampfireID
}
