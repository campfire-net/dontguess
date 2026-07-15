package exchange_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/3dl-dev/dontguess/pkg/exchange"
	"github.com/3dl-dev/dontguess/pkg/identity"
)

// The campfire-bootstrap tests (campfire creation, transport dir, beacon,
// naming alias/registry, membership, convention promotion, config cascade,
// display-name config.toml) were deleted with dontguess-69a: exchange.Init no
// longer creates a campfire — it bootstraps the operator's OWN home
// (secp256k1 operator identity + local event store + relay config) campfire-free
// per docs/design/nostr-first-rebuild-decision.md. Those tests exercised removed
// functionality and have no campfire-free equivalent. What remains tests the
// new contract: identity mint-when-absent, reuse-when-present (never
// overwritten), store creation, relay config, and 0600 key perms.

const (
	operatorKeyFile = "nostr-operator.key"
	storeFile       = "events.jsonl"
)

// TestInit_MintsIdentityWhenAbsent verifies Init mints a secp256k1 operator
// identity on first run and records its pubkey/npub in the config.
func TestInit_MintsIdentityWhenAbsent(t *testing.T) {
	t.Parallel()

	dgHome := t.TempDir()

	// Precondition: no operator key on disk yet.
	if _, err := os.Stat(filepath.Join(dgHome, operatorKeyFile)); !os.IsNotExist(err) {
		t.Fatalf("precondition: %s already exists", operatorKeyFile)
	}

	cfg, err := exchange.Init(exchange.InitOptions{DGHome: dgHome})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	if cfg.OperatorKeyHex == "" {
		t.Error("operator_key is empty — no identity minted")
	}
	if cfg.OperatorNpub == "" {
		t.Error("operator_npub is empty")
	}

	// The key file must exist and its private key must derive the config pubkey.
	keyPath := filepath.Join(dgHome, operatorKeyFile)
	data, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("reading minted key: %v", err)
	}
	id, err := identity.FromPrivHex(trimSpace(string(data)))
	if err != nil {
		t.Fatalf("parsing minted key: %v", err)
	}
	if id.PubKeyHex() != cfg.OperatorKeyHex {
		t.Errorf("config operator_key %q != key-derived pubkey %q", cfg.OperatorKeyHex, id.PubKeyHex())
	}
	if id.Npub() != cfg.OperatorNpub {
		t.Errorf("config operator_npub %q != key-derived npub %q", cfg.OperatorNpub, id.Npub())
	}
}

// TestInit_ReusesIdentityWhenPresent verifies Init NEVER overwrites an existing
// operator key: a second Init returns the same pubkey and the on-disk key bytes
// are byte-identical across runs (idempotent).
func TestInit_ReusesIdentityWhenPresent(t *testing.T) {
	t.Parallel()

	dgHome := t.TempDir()
	keyPath := filepath.Join(dgHome, operatorKeyFile)

	cfg1, err := exchange.Init(exchange.InitOptions{DGHome: dgHome})
	if err != nil {
		t.Fatalf("first Init: %v", err)
	}
	firstKey, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("reading key after first Init: %v", err)
	}

	cfg2, err := exchange.Init(exchange.InitOptions{DGHome: dgHome})
	if err != nil {
		t.Fatalf("second Init: %v", err)
	}
	secondKey, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("reading key after second Init: %v", err)
	}

	if cfg1.OperatorKeyHex != cfg2.OperatorKeyHex {
		t.Errorf("operator key changed on re-init: %q -> %q (identity clobbered)", cfg1.OperatorKeyHex, cfg2.OperatorKeyHex)
	}
	if string(firstKey) != string(secondKey) {
		t.Error("on-disk operator key bytes changed across Init calls — key was overwritten")
	}
}

// TestInit_KeyPerms0600 verifies the minted operator private key is written
// with 0600 permissions (owner read/write only). SECURITY: a world- or
// group-readable operator private key would leak signing authority.
func TestInit_KeyPerms0600(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix file permissions not meaningful on windows")
	}
	t.Parallel()

	dgHome := t.TempDir()
	if _, err := exchange.Init(exchange.InitOptions{DGHome: dgHome}); err != nil {
		t.Fatalf("Init: %v", err)
	}

	info, err := os.Stat(filepath.Join(dgHome, operatorKeyFile))
	if err != nil {
		t.Fatalf("stat key file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("operator key perms = %o, want 0600", perm)
	}
}

// TestInit_CreatesLocalStore verifies Init creates the canonical local event
// log at $DG_HOME/events.jsonl.
func TestInit_CreatesLocalStore(t *testing.T) {
	t.Parallel()

	dgHome := t.TempDir()
	cfg, err := exchange.Init(exchange.InitOptions{DGHome: dgHome})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	storePath := filepath.Join(dgHome, storeFile)
	if _, err := os.Stat(storePath); err != nil {
		t.Errorf("local store not created at %s: %v", storePath, err)
	}
	if cfg.StorePath != storePath {
		t.Errorf("config store_path = %q, want %q", cfg.StorePath, storePath)
	}
}

// TestInit_WritesRelayConfig verifies the relay URLs the operator will serve are
// recorded in the config, in order, both in the returned struct and on disk.
func TestInit_WritesRelayConfig(t *testing.T) {
	t.Parallel()

	dgHome := t.TempDir()
	relays := []string{"ws://192.168.2.40:7777", "ws://192.168.2.41:7777"}

	cfg, err := exchange.Init(exchange.InitOptions{DGHome: dgHome, RelayURLs: relays})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	if len(cfg.RelayURLs) != len(relays) {
		t.Fatalf("config relay_urls len = %d, want %d", len(cfg.RelayURLs), len(relays))
	}
	for i, u := range relays {
		if cfg.RelayURLs[i] != u {
			t.Errorf("relay_urls[%d] = %q, want %q", i, cfg.RelayURLs[i], u)
		}
	}

	// The relay config must be persisted to disk, not just returned.
	data, err := os.ReadFile(exchange.ConfigPath(dgHome))
	if err != nil {
		t.Fatalf("reading config: %v", err)
	}
	var diskCfg exchange.Config
	if err := json.Unmarshal(data, &diskCfg); err != nil {
		t.Fatalf("parsing config: %v", err)
	}
	if len(diskCfg.RelayURLs) != len(relays) {
		t.Errorf("on-disk relay_urls len = %d, want %d", len(diskCfg.RelayURLs), len(relays))
	}
	for i := range relays {
		if i < len(diskCfg.RelayURLs) && diskCfg.RelayURLs[i] != relays[i] {
			t.Errorf("on-disk relay_urls[%d] = %q, want %q", i, diskCfg.RelayURLs[i], relays[i])
		}
	}
}

// TestInit_ConfigRoundTrips verifies LoadConfig reads back what Init wrote.
func TestInit_ConfigRoundTrips(t *testing.T) {
	t.Parallel()

	dgHome := t.TempDir()
	cfg, err := exchange.Init(exchange.InitOptions{DGHome: dgHome, RelayURLs: []string{"ws://r:7777"}})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	loaded, err := exchange.LoadConfig(dgHome)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if loaded.OperatorKeyHex != cfg.OperatorKeyHex {
		t.Errorf("loaded operator_key = %q, want %q", loaded.OperatorKeyHex, cfg.OperatorKeyHex)
	}
	if loaded.CreatedAt != cfg.CreatedAt {
		t.Errorf("loaded created_at = %d, want %d", loaded.CreatedAt, cfg.CreatedAt)
	}
}

// TestInit_PreservesCreatedAt verifies re-init without Force preserves the
// original CreatedAt timestamp (idempotent config).
func TestInit_PreservesCreatedAt(t *testing.T) {
	t.Parallel()

	dgHome := t.TempDir()
	cfg1, err := exchange.Init(exchange.InitOptions{DGHome: dgHome})
	if err != nil {
		t.Fatalf("first Init: %v", err)
	}
	cfg2, err := exchange.Init(exchange.InitOptions{DGHome: dgHome})
	if err != nil {
		t.Fatalf("second Init: %v", err)
	}
	if cfg1.CreatedAt != cfg2.CreatedAt {
		t.Errorf("created_at changed on idempotent re-init: %d -> %d", cfg1.CreatedAt, cfg2.CreatedAt)
	}
}

// TestInit_DefaultsMinReputation40 verifies a freshly written config gets
// MinReputation=40 (demotion-only floor, dontguess-b45 / design §8 D3) rather
// than the zero value, without the operator having to set anything.
func TestInit_DefaultsMinReputation40(t *testing.T) {
	t.Parallel()

	dgHome := t.TempDir()
	cfg, err := exchange.Init(exchange.InitOptions{DGHome: dgHome})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if cfg.MinReputation != exchange.DefaultMinReputation {
		t.Errorf("MinReputation = %d, want default %d", cfg.MinReputation, exchange.DefaultMinReputation)
	}

	loaded, err := exchange.LoadConfig(dgHome)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if loaded.MinReputation != exchange.DefaultMinReputation {
		t.Errorf("on-disk MinReputation = %d, want default %d", loaded.MinReputation, exchange.DefaultMinReputation)
	}
}

// TestLoadConfig_RejectsMinReputationAboveMax proves the reject path (design
// §8 D3: "any config value >50 is rejected... at load"): a hand-written
// config with min_reputation above MaxMinReputation fails LoadConfig loudly
// instead of being silently clamped or accepted.
func TestLoadConfig_RejectsMinReputationAboveMax(t *testing.T) {
	t.Parallel()

	dgHome := t.TempDir()
	if _, err := exchange.Init(exchange.InitOptions{DGHome: dgHome}); err != nil {
		t.Fatalf("Init: %v", err)
	}

	raw, err := os.ReadFile(exchange.ConfigPath(dgHome))
	if err != nil {
		t.Fatalf("reading config: %v", err)
	}
	var cfg exchange.Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("parsing config: %v", err)
	}
	cfg.MinReputation = exchange.MaxMinReputation + 1
	data, err := json.MarshalIndent(&cfg, "", "  ")
	if err != nil {
		t.Fatalf("marshaling config: %v", err)
	}
	if err := os.WriteFile(exchange.ConfigPath(dgHome), data, 0600); err != nil {
		t.Fatalf("writing config: %v", err)
	}

	if _, err := exchange.LoadConfig(dgHome); err == nil {
		t.Fatal("LoadConfig accepted min_reputation above MaxMinReputation — want a rejection error")
	}
}

// TestLoadConfig_AcceptsMinReputationAtMax proves the accept path at the
// boundary: MinReputation == MaxMinReputation (50) loads without error.
func TestLoadConfig_AcceptsMinReputationAtMax(t *testing.T) {
	t.Parallel()

	dgHome := t.TempDir()
	if _, err := exchange.Init(exchange.InitOptions{DGHome: dgHome}); err != nil {
		t.Fatalf("Init: %v", err)
	}

	raw, err := os.ReadFile(exchange.ConfigPath(dgHome))
	if err != nil {
		t.Fatalf("reading config: %v", err)
	}
	var cfg exchange.Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("parsing config: %v", err)
	}
	cfg.MinReputation = exchange.MaxMinReputation
	data, err := json.MarshalIndent(&cfg, "", "  ")
	if err != nil {
		t.Fatalf("marshaling config: %v", err)
	}
	if err := os.WriteFile(exchange.ConfigPath(dgHome), data, 0600); err != nil {
		t.Fatalf("writing config: %v", err)
	}

	loaded, err := exchange.LoadConfig(dgHome)
	if err != nil {
		t.Fatalf("LoadConfig at MaxMinReputation boundary: unexpected error: %v", err)
	}
	if loaded.MinReputation != exchange.MaxMinReputation {
		t.Errorf("MinReputation = %d, want %d", loaded.MinReputation, exchange.MaxMinReputation)
	}
}

// TestInit_PersistsTier verifies a declared tier is recorded in the config (both
// returned and on disk) and round-trips through LoadConfig (dontguess-daa).
func TestInit_PersistsTier(t *testing.T) {
	t.Parallel()

	dgHome := t.TempDir()
	cfg, err := exchange.Init(exchange.InitOptions{
		DGHome:    dgHome,
		Tier:      exchange.TierTeam,
		RelayURLs: []string{"ws://r:7777"},
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if cfg.Tier != exchange.TierTeam {
		t.Errorf("returned tier = %q, want %q", cfg.Tier, exchange.TierTeam)
	}
	loaded, err := exchange.LoadConfig(dgHome)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if loaded.Tier != exchange.TierTeam {
		t.Errorf("on-disk tier = %q, want %q", loaded.Tier, exchange.TierTeam)
	}
}

// TestInit_RejectsTeamFleetWithoutRelay proves the config-time fail-closed guard:
// a declared team/fleet tier with an empty RelayURLs set is rejected with a clear
// error naming the tier and the relay flag — NOT silently downgraded to solo, NOT
// given a default relay (dontguess-daa DONE clause 1).
func TestInit_RejectsTeamFleetWithoutRelay(t *testing.T) {
	t.Parallel()

	for _, tier := range []exchange.Tier{exchange.TierTeam, exchange.TierFleet} {
		tier := tier
		t.Run(string(tier), func(t *testing.T) {
			t.Parallel()
			dgHome := t.TempDir()
			_, err := exchange.Init(exchange.InitOptions{DGHome: dgHome, Tier: tier})
			if err == nil {
				t.Fatalf("Init(tier=%s, no relay) returned nil — the config-time guard did not fire (silent solo downgrade)", tier)
			}
			msg := err.Error()
			if !strings.Contains(msg, string(tier)) {
				t.Errorf("error does not name the tier %q: %q", tier, msg)
			}
			// The remedy names the REAL remedy — DONTGUESS_RELAY_URLS — not a
			// nonexistent --relay flag on init (dontguess-4f0 b6e3 fix).
			if !strings.Contains(msg, "DONTGUESS_RELAY_URLS") {
				t.Errorf("error does not name the DONTGUESS_RELAY_URLS remedy: %q", msg)
			}
			if strings.Contains(msg, "--relay") {
				t.Errorf("error still names a nonexistent --relay flag on init: %q", msg)
			}
		})
	}
}

// TestInit_SoloWithoutRelaySucceeds proves solo (and the undeclared tier) never
// require a relay — the zero-ceremony individual tier still bootstraps with no
// relay, no tier declaration.
func TestInit_SoloWithoutRelaySucceeds(t *testing.T) {
	t.Parallel()

	dgHome := t.TempDir()
	cfg, err := exchange.Init(exchange.InitOptions{DGHome: dgHome, Tier: exchange.TierSolo})
	if err != nil {
		t.Fatalf("Init(solo, no relay): unexpected error: %v", err)
	}
	if cfg.Tier != exchange.TierSolo {
		t.Errorf("tier = %q, want %q", cfg.Tier, exchange.TierSolo)
	}
	if len(cfg.RelayURLs) != 0 {
		t.Errorf("relay_urls = %v, want empty (no default substituted)", cfg.RelayURLs)
	}
}

// TestInit_PreservesTierOnReInit proves an idempotent re-init (no Force, empty
// caller tier) preserves a previously-declared tier rather than silently
// clearing it to solo.
func TestInit_PreservesTierOnReInit(t *testing.T) {
	t.Parallel()

	dgHome := t.TempDir()
	if _, err := exchange.Init(exchange.InitOptions{
		DGHome:    dgHome,
		Tier:      exchange.TierTeam,
		RelayURLs: []string{"ws://r:7777"},
	}); err != nil {
		t.Fatalf("first Init: %v", err)
	}
	// Re-init with NO tier and NO relay — must preserve team + relays, not reject
	// or downgrade.
	cfg, err := exchange.Init(exchange.InitOptions{DGHome: dgHome})
	if err != nil {
		t.Fatalf("idempotent re-init: %v", err)
	}
	if cfg.Tier != exchange.TierTeam {
		t.Errorf("tier after re-init = %q, want preserved %q", cfg.Tier, exchange.TierTeam)
	}
	if len(cfg.RelayURLs) != 1 {
		t.Errorf("relay_urls after re-init = %v, want preserved [ws://r:7777]", cfg.RelayURLs)
	}
}

// trimSpace strips surrounding ASCII whitespace/newlines from a key file read.
func trimSpace(s string) string {
	for len(s) > 0 {
		switch s[len(s)-1] {
		case '\n', '\r', ' ', '\t':
			s = s[:len(s)-1]
		default:
			return s
		}
	}
	return s
}
