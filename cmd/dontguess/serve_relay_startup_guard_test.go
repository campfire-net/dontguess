package main

// serve_relay_startup_guard_test.go — item ed2-F (dontguess-03e), design §3.9
// (H6 / RT-C#2). A relay-attached serve (len(relayURLs) > 0) MUST refuse to start
// (hard error) when the persisted exchange config is absent or carries an empty
// OperatorKeyHex. This is defense-in-depth behind the wrapper's individual-tier
// auto-start gate (§3.10): a stray/auto-started team-tier serve fails LOUD here
// instead of silently minting a fresh nostr operator key and forking the
// sequencer. The individual tier (no relay URLs) never reaches the guard and is
// byte-for-byte unaffected.
//
// This is a package-main in-process test (drives runServeLocal + the guard), so
// any edit to serve.go invalidates the test cache (the H7 cache-gap discipline).

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAssertRelayServeHasOperatorConfig unit-tests the pure guard: absent config
// (loadErr != nil) and empty operator key each hard-error; a present operator key
// passes.
func TestAssertRelayServeHasOperatorConfig(t *testing.T) {
	loadErr := os.ErrNotExist

	// Absent config → hard error mentioning the guard.
	if err := assertRelayServeHasOperatorConfig("", loadErr); err == nil {
		t.Fatalf("absent config: want hard error, got nil")
	} else {
		msg := err.Error()
		if !strings.Contains(msg, "persisted exchange config") || !strings.Contains(msg, "§3.9") {
			t.Errorf("absent-config error is not LOUD/attributed: %q", msg)
		}
	}

	// Present config but empty operator key → hard error mentioning operator_key.
	if err := assertRelayServeHasOperatorConfig("", nil); err == nil {
		t.Fatalf("empty operator key: want hard error, got nil")
	} else {
		msg := err.Error()
		if !strings.Contains(msg, "operator_key") || !strings.Contains(msg, "§3.9") {
			t.Errorf("empty-key error is not LOUD/attributed: %q", msg)
		}
	}

	// Present operator key → no error.
	if err := assertRelayServeHasOperatorConfig("aabbccddeeff00112233445566778899aabbccddeeff001122334455667788", nil); err != nil {
		t.Errorf("present operator key: want nil, got %v", err)
	}
}

// TestServe_RelayAttached_AbsentConfig_HardErrorsLoud drives the real
// runServeLocal on the TEAM tier (DONTGUESS_RELAY_URLS set) with a DG_HOME that
// has NO exchange config. It must return a LOUD startup error BEFORE ever dialing
// a relay — proving a config-less relay serve refuses to become an operator.
func TestServe_RelayAttached_AbsentConfig_HardErrorsLoud(t *testing.T) {
	dgHome := t.TempDir()
	// Team tier: any relay URL selects the relay-attached branch. runServeLocal
	// returns at the §3.9 guard well before it dials this (unreachable) URL.
	t.Setenv("DONTGUESS_RELAY_URLS", "wss://relay.invalid.example")
	t.Setenv("DONTGUESS_RELAY_URL", "")

	err := runServeLocal(dgHome)
	if err == nil {
		t.Fatalf("relay-attached serve with NO config started without error — the rogue-operator guard did not fire")
	}
	msg := err.Error()
	if !strings.Contains(msg, "startup") || !strings.Contains(msg, "persisted exchange config") {
		t.Fatalf("startup error is not the §3.9 absent-config guard: %q", msg)
	}
	if !strings.Contains(msg, "§3.9") {
		t.Errorf("startup error is not attributed to the design guard: %q", msg)
	}

	// It must have failed at the guard, NOT reached the engine — the socket is
	// never created because runServeLocal returned before eng.Start.
	if _, serr := os.Stat(filepath.Join(dgHome, "ipc", "dontguess.sock")); serr == nil {
		t.Errorf("operator IPC socket exists — serve advanced past the startup guard into serving")
	}
}

// TestServe_RelayAttached_EmptyOperatorKey_HardErrorsLoud drives runServeLocal on
// the TEAM tier with a config present but carrying an empty operator_key. It must
// hard-error at the §3.9 guard.
func TestServe_RelayAttached_EmptyOperatorKey_HardErrorsLoud(t *testing.T) {
	dgHome := t.TempDir()
	cfgPath := filepath.Join(dgHome, "dontguess-exchange.json")
	// operator_key deliberately empty — the guard's empty-key branch.
	if err := os.WriteFile(cfgPath, []byte(`{"operator_key": "", "store_path": "events.jsonl"}`), 0600); err != nil {
		t.Fatalf("writing empty-key config: %v", err)
	}
	t.Setenv("DONTGUESS_RELAY_URLS", "wss://relay.invalid.example")
	t.Setenv("DONTGUESS_RELAY_URL", "")

	err := runServeLocal(dgHome)
	if err == nil {
		t.Fatalf("relay-attached serve with EMPTY operator_key started without error — the guard did not fire")
	}
	msg := err.Error()
	if !strings.Contains(msg, "startup") || !strings.Contains(msg, "operator_key") {
		t.Fatalf("startup error is not the §3.9 empty-key guard: %q", msg)
	}
}

// TestServe_IndividualTier_SkipsRelayGuard proves the individual tier is
// unaffected: with DONTGUESS_RELAY_URLS unset, resolveRelayURLs() is empty, so the
// relay-attached branch — and therefore the §3.9 guard — is never entered. An
// individual-tier serve boots with no config and no operator key requirement
// (proven exhaustively by serve_local_test.go); the guard is structurally
// unreachable there.
func TestServe_IndividualTier_SkipsRelayGuard(t *testing.T) {
	t.Setenv("DONTGUESS_RELAY_URLS", "")
	t.Setenv("DONTGUESS_RELAY_URL", "")

	if urls := resolveRelayURLs(); len(urls) != 0 {
		t.Fatalf("individual tier: resolveRelayURLs() = %v, want empty — the relay branch (and §3.9 guard) must be skipped", urls)
	}
}
