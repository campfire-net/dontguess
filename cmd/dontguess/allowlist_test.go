package main

// allowlist_test.go — dontguess-b45 ground-source tests: `dontguess
// allowlist add|remove|list` mutates the persisted Config.FleetAllowlist on
// disk (real config path, no mocks). Proves both the accept path (valid npub
// round-trips add->list->remove) and the reject path (malformed npub is a
// loud error and persists nothing).

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/campfire-net/dontguess/pkg/exchange"
	"github.com/campfire-net/dontguess/pkg/identity"
)

// bootstrapAllowlistHome inits a scratch DG_HOME (so LoadConfig has a config
// file to mutate) and returns the path.
func bootstrapAllowlistHome(t *testing.T) string {
	t.Helper()
	dgHome := t.TempDir()
	if _, err := exchange.Init(exchange.InitOptions{DGHome: dgHome}); err != nil {
		t.Fatalf("exchange.Init: %v", err)
	}
	return dgHome
}

// TestAllowlist_AddListRemoveRoundTrip is the primary accept-path proof: add
// persists the npub to Config.FleetAllowlist on disk, list surfaces it,
// remove drops it — verified by re-reading the config file directly (not
// just the in-process return value) at every step.
func TestAllowlist_AddListRemoveRoundTrip(t *testing.T) {
	t.Parallel()

	dgHome := bootstrapAllowlistHome(t)
	id, err := identity.Generate()
	if err != nil {
		t.Fatalf("identity.Generate: %v", err)
	}
	npub := id.Npub()

	// --- add ---
	var addOut bytes.Buffer
	if err := runAllowlistAdd(dgHome, npub, &addOut); err != nil {
		t.Fatalf("runAllowlistAdd: %v", err)
	}
	cfgAfterAdd := readAllowlistConfig(t, dgHome)
	if len(cfgAfterAdd.FleetAllowlist) != 1 || cfgAfterAdd.FleetAllowlist[0] != npub {
		t.Fatalf("after add, on-disk fleet_allowlist = %v, want [%s]", cfgAfterAdd.FleetAllowlist, npub)
	}
	rawAfterAdd := readAllowlistConfigRaw(t, dgHome)
	if !strings.Contains(rawAfterAdd, "fleet_allowlist") {
		t.Errorf("config JSON does not contain fleet_allowlist key:\n%s", rawAfterAdd)
	}

	// --- list ---
	var listOut bytes.Buffer
	if err := runAllowlistList(dgHome, &listOut); err != nil {
		t.Fatalf("runAllowlistList: %v", err)
	}
	if !strings.Contains(listOut.String(), npub) {
		t.Errorf("list output %q does not contain added npub %q", listOut.String(), npub)
	}

	// --- remove ---
	var rmOut bytes.Buffer
	if err := runAllowlistRemove(dgHome, npub, &rmOut); err != nil {
		t.Fatalf("runAllowlistRemove: %v", err)
	}
	cfgAfterRemove := readAllowlistConfig(t, dgHome)
	if len(cfgAfterRemove.FleetAllowlist) != 0 {
		t.Fatalf("after remove, on-disk fleet_allowlist = %v, want empty", cfgAfterRemove.FleetAllowlist)
	}

	// --- list again: empty ---
	var listOut2 bytes.Buffer
	if err := runAllowlistList(dgHome, &listOut2); err != nil {
		t.Fatalf("runAllowlistList (post-remove): %v", err)
	}
	if strings.Contains(listOut2.String(), npub) {
		t.Errorf("list output after remove still contains %q: %q", npub, listOut2.String())
	}
}

// TestAllowlist_AddRejectsMalformedNpub proves the reject path: a malformed
// npub is a loud, non-nil error and the config's FleetAllowlist is left
// completely untouched (nothing persisted).
func TestAllowlist_AddRejectsMalformedNpub(t *testing.T) {
	t.Parallel()

	dgHome := bootstrapAllowlistHome(t)
	before := readAllowlistConfig(t, dgHome)

	var out bytes.Buffer
	err := runAllowlistAdd(dgHome, "npub1not-a-valid-key", &out)
	if err == nil {
		t.Fatal("runAllowlistAdd with malformed npub returned nil error, want a loud rejection")
	}

	after := readAllowlistConfig(t, dgHome)
	if len(after.FleetAllowlist) != len(before.FleetAllowlist) {
		t.Errorf("malformed add mutated on-disk fleet_allowlist: before=%v after=%v", before.FleetAllowlist, after.FleetAllowlist)
	}
	if len(after.FleetAllowlist) != 0 {
		t.Errorf("malformed add persisted an entry: %v", after.FleetAllowlist)
	}
}

// TestAllowlist_RemoveRejectsMalformedNpub proves remove validates the same
// way as add — a malformed npub is rejected before any config I/O.
func TestAllowlist_RemoveRejectsMalformedNpub(t *testing.T) {
	t.Parallel()

	dgHome := bootstrapAllowlistHome(t)

	var out bytes.Buffer
	err := runAllowlistRemove(dgHome, "not-an-npub-or-hex-key", &out)
	if err == nil {
		t.Fatal("runAllowlistRemove with malformed npub returned nil error, want a loud rejection")
	}
}

// TestAllowlist_AddIsIdempotent proves adding the same npub twice does not
// produce a duplicate entry.
func TestAllowlist_AddIsIdempotent(t *testing.T) {
	t.Parallel()

	dgHome := bootstrapAllowlistHome(t)
	id, err := identity.Generate()
	if err != nil {
		t.Fatalf("identity.Generate: %v", err)
	}
	npub := id.Npub()

	var out1, out2 bytes.Buffer
	if err := runAllowlistAdd(dgHome, npub, &out1); err != nil {
		t.Fatalf("first add: %v", err)
	}
	if err := runAllowlistAdd(dgHome, npub, &out2); err != nil {
		t.Fatalf("second add: %v", err)
	}

	cfg := readAllowlistConfig(t, dgHome)
	count := 0
	for _, e := range cfg.FleetAllowlist {
		if e == npub {
			count++
		}
	}
	if count != 1 {
		t.Errorf("fleet_allowlist has %d copies of %s after double-add, want 1: %v", count, npub, cfg.FleetAllowlist)
	}
}

// TestAllowlist_MinReputationAboveMaxRejectedAtLoad proves the sibling reject
// path from dontguess-b45: a persisted MinReputation above 50 fails
// exchange.LoadConfig loudly. This is exercised at the exchange.LoadConfig
// layer directly (the same call runAllowlistList/Add/Remove use), proving
// the allowlist CLI would surface, not swallow, the rejection.
func TestAllowlist_MinReputationAboveMaxRejectedAtLoad(t *testing.T) {
	t.Parallel()

	dgHome := bootstrapAllowlistHome(t)
	cfg := readAllowlistConfig(t, dgHome)
	cfg.MinReputation = 51
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatalf("marshaling config: %v", err)
	}
	if err := os.WriteFile(exchange.ConfigPath(dgHome), data, 0600); err != nil {
		t.Fatalf("writing config: %v", err)
	}

	var out bytes.Buffer
	if err := runAllowlistList(dgHome, &out); err == nil {
		t.Fatal("runAllowlistList succeeded with min_reputation=51 on disk, want a rejection error")
	}
}

// readAllowlistConfig reads and parses the on-disk exchange config for
// dgHome via exchange.LoadConfig — the exact same read path production code
// uses, not a hand-rolled parse.
func readAllowlistConfig(t *testing.T, dgHome string) *exchange.Config {
	t.Helper()
	cfg, err := exchange.LoadConfig(dgHome)
	if err != nil {
		t.Fatalf("exchange.LoadConfig(%s): %v", dgHome, err)
	}
	return cfg
}

// readAllowlistConfigRaw returns the raw JSON bytes of the on-disk config, to
// assert on the literal key name (fleet_allowlist) independent of struct tags
// drifting.
func readAllowlistConfigRaw(t *testing.T, dgHome string) string {
	t.Helper()
	data, err := os.ReadFile(exchange.ConfigPath(dgHome))
	if err != nil {
		t.Fatalf("reading config: %v", err)
	}
	return string(data)
}
