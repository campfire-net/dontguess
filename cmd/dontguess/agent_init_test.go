package main

// agent_init_test.go — ground-source tests for dontguess agent-init subcommand.
//
// Decision reference: docs/design/exchange-per-agent-identity-decision.md §7
// Item: dontguess-04f. Updated for dontguess-88e (de-campfire agent-init):
// agent-init no longer admits/joins the agent to the exchange campfire — it
// provisions ONLY a secp256k1 nostr identity (see agent_init_nostr_test.go,
// agent_init_hardening_test.go, agent_init_sybil_test.go for the nostr-first
// identity coverage). The campfire admit/join assertions previously here
// (Ed25519 pubkey via protocol.Init, GetMembership join verification) are
// removed — there is nothing left to admit or join. What remains
// re-expressed in nostr terms: distinct identities per agent, idempotent
// re-init, and the on-disk agent-home layout.
//
// scratchExchange still provisions a full campfire-backed exchange config —
// it is retained as shared test scaffolding for the sibling agent_init_*_test.go
// files, which use dgHome as an arbitrary scratch DG_HOME regardless of
// whether a campfire exchange backs it. All tests run against scratch temp
// dirs — NOT ~/.cf. No ~/.cf mutations occur; all state lives in t.TempDir()
// paths.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/campfire-net/campfire/cf-protocol/protocol"
	"github.com/campfire-net/dontguess/pkg/exchange"
	"github.com/campfire-net/dontguess/pkg/identity"
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

// runAgentInitWith provisions <name> as a persistent FLEET MEMBER under dgHome
// (the --fleet-member path — these tests predate the fail-closed mode-selection
// requirement added for dontguess-ebf and were never exercising the --parent
// subagent path). Returns any error from the command.
func runAgentInitWith(t *testing.T, dgHome, name string) error {
	t.Helper()
	return runAgentInitCore(dgHome, name, "", true)
}

// agentNpub reads the nostr npub for an agent home directory via
// identity.Load — the canonical accessor for a fleet member's own persistent
// secp256k1 identity (mirrors agent_init_nostr_test.go).
func agentNpub(t *testing.T, agentHome string) string {
	t.Helper()
	id, err := identity.Load(agentHome)
	if err != nil {
		t.Fatalf("identity.Load(%s): %v", agentHome, err)
	}
	return id.Npub()
}

// TestAgentInit_GeneratesDistinctIdentity verifies:
//  1. Two distinct names produce two distinct secp256k1 npubs.
//  2. Re-running the same name yields the SAME npub (idempotency — no clobber).
//  3. Each agent home is under $DG_HOME/agents/<name>/ and holds a nostr identity.
//  4. No ~/.cf mutation occurs — everything lives under the scratch dgHome.
func TestAgentInit_GeneratesDistinctIdentity(t *testing.T) {
	t.Parallel()

	dgHome, _ := scratchExchange(t)

	// --- init alice ---
	if err := runAgentInitWith(t, dgHome, "alice"); err != nil {
		t.Fatalf("agent-init alice: %v", err)
	}
	aliceHome := filepath.Join(dgHome, "agents", "alice")
	aliceKey1 := agentNpub(t, aliceHome)
	if aliceKey1 == "" {
		t.Fatal("alice npub is empty")
	}

	// --- init bob ---
	if err := runAgentInitWith(t, dgHome, "bob"); err != nil {
		t.Fatalf("agent-init bob: %v", err)
	}
	bobHome := filepath.Join(dgHome, "agents", "bob")
	bobKey := agentNpub(t, bobHome)
	if bobKey == "" {
		t.Fatal("bob npub is empty")
	}

	// Assert 1: alice and bob have distinct npubs.
	if aliceKey1 == bobKey {
		t.Errorf("alice and bob have the same npub %s — expected distinct identities", aliceKey1)
	}

	// Assert 2: idempotency — re-init alice yields the same npub (no clobber).
	if err := runAgentInitWith(t, dgHome, "alice"); err != nil {
		t.Fatalf("agent-init alice (2nd run): %v", err)
	}
	aliceKey2 := agentNpub(t, aliceHome)
	if aliceKey1 != aliceKey2 {
		t.Errorf("alice npub changed on re-init: first=%s second=%s — identity was clobbered", aliceKey1, aliceKey2)
	}

	// Assert 3: agent homes are at expected paths and hold a nostr identity.
	for _, home := range []string{aliceHome, bobHome} {
		if _, err := os.Stat(filepath.Join(home, identity.IdentityFile)); err != nil {
			t.Errorf("%s missing at %s: %v", identity.IdentityFile, home, err)
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
}

// TestAgentInit_RejectsPathTraversal verifies that malicious agent names which
// would resolve outside (or to the root of) $DG_HOME/agents are rejected — most
// importantly ".." and ".", which would otherwise resolve to DG_HOME itself and
// mint the nostr identity INTO the OPERATOR's home, handing the caller
// operator signing authority (HIGH-severity privilege escalation). Regression
// for the V4 veracity finding, re-expressed in nostr terms for dontguess-88e:
// the escalation vector used to be loading the operator's Ed25519 campfire
// identity.json; post de-campfire it is minting nostr-identity.json at
// DG_HOME via identity.LoadOrCreate(agentHome) when agentHome resolves to
// dgHome itself.
func TestAgentInit_RejectsPathTraversal(t *testing.T) {
	t.Parallel()

	dgHome, _ := scratchExchange(t)

	// No nostr identity exists at DG_HOME root before any agent-init call —
	// confirms the file we check afterward wasn't already there for other
	// reasons (e.g. an operator nostr identity planted by a different test).
	opNostrIdentity := filepath.Join(dgHome, identity.IdentityFile)
	if _, err := os.Stat(opNostrIdentity); !os.IsNotExist(err) {
		t.Fatalf("precondition failed: %s already exists at dgHome root", identity.IdentityFile)
	}

	for _, name := range []string{"..", ".", "../evil", "a/b", "a\\b", "", "foo/../..", "x..y"} {
		err := runAgentInitWith(t, dgHome, name)
		if err == nil {
			t.Errorf("agent-init %q: expected rejection, got nil error (possible path traversal)", name)
		}
	}

	// No nostr identity may have been minted at DG_HOME root — that would mean
	// a rejected name still resolved agentHome to dgHome and the operator's
	// signing identity was created/borrowed.
	if _, err := os.Stat(opNostrIdentity); !os.IsNotExist(err) {
		t.Errorf("%s was created at dgHome root after rejected agent-init calls — traversal not contained", identity.IdentityFile)
	}

	// No agent home should have been created for any rejected name.
	if entries, err := os.ReadDir(filepath.Join(dgHome, "agents")); err == nil {
		for _, e := range entries {
			t.Errorf("unexpected agent home created by a rejected name: %s", e.Name())
		}
	}

	// Note: "x..y" contains ".." and is rejected by the conservative Contains(name, "..")
	// guard even though it is not a traversal — acceptable: agent names are operator-chosen
	// and need not contain "..".
}
