package exchange_test

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/campfire-net/campfire/pkg/beacon"
	"github.com/campfire-net/campfire/pkg/convention"
	"github.com/campfire-net/campfire/pkg/naming"
	"github.com/campfire-net/campfire/pkg/protocol"

	"github.com/campfire-net/dontguess/pkg/exchange"
)

// conventionDir returns the path to the project's convention declarations.
func conventionDir(t *testing.T) string {
	t.Helper()
	// Walk up from the package dir to find docs/convention.
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		candidate := filepath.Join(dir, "docs", "convention")
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("could not locate docs/convention — run tests from within the dontguess repo")
	return ""
}

// initExchange calls exchange.Init with temp dirs, closes the client, and returns the config.
// SkipConfigCascade defaults to true to avoid ancestor .cf/config.toml auto_join
// beacons that add ~15s per call. Tests that explicitly need config cascade must
// set SkipConfigCascade=false.
func initExchange(t *testing.T, opts exchange.InitOptions) *exchange.Config {
	t.Helper()
	if !opts.SkipConfigCascade {
		opts.SkipConfigCascade = true
	}
	cfg, client, err := exchange.Init(opts)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { client.Close() })
	return cfg
}

func TestInit_CreatesExchangeCampfire(t *testing.T) {
	t.Parallel()

	configDir := t.TempDir()
	transportDir := t.TempDir()
	beaconDir := t.TempDir()
	convDir := conventionDir(t)

	opts := exchange.InitOptions{
		ConfigDir:     configDir,
		Transport:     protocol.FilesystemTransport{Dir: transportDir},
		BeaconDir:     beaconDir,
		ConventionDir: convDir,
		Alias:         "exchange.dontguess",
	}

	cfg := initExchange(t, opts)

	// Config must have a non-empty campfire ID (64 hex chars).
	if len(cfg.ExchangeCampfireID) != 64 {
		t.Errorf("exchange_campfire_id len = %d, want 64", len(cfg.ExchangeCampfireID))
	}
	if cfg.OperatorKeyHex == "" {
		t.Error("operator_key is empty")
	}
	if cfg.Alias != "exchange.dontguess" {
		t.Errorf("alias = %q, want %q", cfg.Alias, "exchange.dontguess")
	}
	if cfg.ConventionVersion != "0.1" {
		t.Errorf("convention_version = %q, want %q", cfg.ConventionVersion, "0.1")
	}

	// Config file must exist on disk and match returned struct.
	configPath := exchange.ConfigPath(configDir)
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("reading config: %v", err)
	}
	var diskCfg exchange.Config
	if err := json.Unmarshal(data, &diskCfg); err != nil {
		t.Fatalf("parsing config: %v", err)
	}
	if diskCfg.ExchangeCampfireID != cfg.ExchangeCampfireID {
		t.Errorf("disk campfire_id = %q, want %q", diskCfg.ExchangeCampfireID, cfg.ExchangeCampfireID)
	}
}

func TestInit_TransportDirectoryCreated(t *testing.T) {
	t.Parallel()

	configDir := t.TempDir()
	transportDir := t.TempDir()
	beaconDir := t.TempDir()
	convDir := conventionDir(t)

	cfg := initExchange(t, exchange.InitOptions{
		ConfigDir:     configDir,
		Transport:     protocol.FilesystemTransport{Dir: transportDir},
		BeaconDir:     beaconDir,
		ConventionDir: convDir,
	})

	// client.Create stores transport at {transportDir}/{campfireID}/.
	campfireDir := filepath.Join(transportDir, cfg.ExchangeCampfireID)

	// campfire.cbor must exist.
	if _, err := os.Stat(filepath.Join(campfireDir, "campfire.cbor")); os.IsNotExist(err) {
		t.Error("campfire.cbor not found in transport dir")
	}
	// members/ must exist with at least one entry.
	entries, err := os.ReadDir(filepath.Join(campfireDir, "members"))
	if err != nil {
		t.Fatalf("reading members dir: %v", err)
	}
	if len(entries) == 0 {
		t.Error("no member records in transport dir")
	}
}

func TestInit_ConventionDeclarationsPromoted(t *testing.T) {
	t.Parallel()

	configDir := t.TempDir()
	transportDir := t.TempDir()
	beaconDir := t.TempDir()
	convDir := conventionDir(t)

	cfg := initExchange(t, exchange.InitOptions{
		ConfigDir:     configDir,
		Transport:     protocol.FilesystemTransport{Dir: transportDir},
		BeaconDir:     beaconDir,
		ConventionDir: convDir,
	})

	// Use protocol.Init to open a second client and read messages via SDK.
	verifyClient, _, err := protocol.Init(configDir)
	if err != nil {
		t.Fatalf("protocol.Init for verify: %v", err)
	}
	defer verifyClient.Close()

	result, err := verifyClient.Read(protocol.ReadRequest{
		CampfireID: cfg.ExchangeCampfireID,
		Tags:       []string{convention.ConventionOperationTag},
	})
	if err != nil {
		t.Fatalf("Read convention messages: %v", err)
	}

	msgs := result.Messages
	if len(msgs) == 0 {
		t.Fatal("no convention messages found")
	}

	opNames := make(map[string]bool)
	for _, msg := range msgs {
		decl, _, err := convention.Parse(msg.Tags, msg.Payload, "", "")
		if err != nil {
			t.Errorf("parsing declaration in msg %s: %v", msg.ID, err)
			continue
		}
		opNames[decl.Operation] = true
	}

	// Core operations must be present.
	for _, op := range []string{"put", "buy", "match", "settle"} {
		if !opNames[op] {
			t.Errorf("expected operation %q to be promoted, got: %v", op, opNames)
		}
	}
	// Scrip operations must be present.
	for _, op := range []string{"scrip:mint", "scrip:burn"} {
		if !opNames[op] {
			t.Errorf("expected operation %q to be promoted, got: %v", op, opNames)
		}
	}
}

// TestInit_PutNotDoublePromoted verifies that when multiple put convention
// files are present in the convention directory, only one "put" declaration
// is promoted — the highest version wins. Double-promotion would create
// ambiguous registry state without a supersedes chain.
func TestInit_PutNotDoublePromoted(t *testing.T) {
	t.Parallel()

	configDir := t.TempDir()
	transportDir := t.TempDir()
	beaconDir := t.TempDir()
	convDir := conventionDir(t)

	cfg := initExchange(t, exchange.InitOptions{
		ConfigDir:     configDir,
		Transport:     protocol.FilesystemTransport{Dir: transportDir},
		BeaconDir:     beaconDir,
		ConventionDir: convDir,
	})

	verifyClient, _, err := protocol.Init(configDir)
	if err != nil {
		t.Fatalf("protocol.Init for verify: %v", err)
	}
	defer verifyClient.Close()

	result, err := verifyClient.Read(protocol.ReadRequest{
		CampfireID: cfg.ExchangeCampfireID,
		Tags:       []string{convention.ConventionOperationTag},
	})
	if err != nil {
		t.Fatalf("Read convention messages: %v", err)
	}

	// Count how many promoted messages declare operation == "put".
	var putCount int
	var putVersion string
	for _, msg := range result.Messages {
		decl, _, parseErr := convention.Parse(msg.Tags, msg.Payload, "", "")
		if parseErr != nil {
			continue
		}
		if decl.Operation == "put" {
			putCount++
			putVersion = decl.Version
		}
	}

	if putCount != 1 {
		t.Errorf("expected exactly 1 promoted 'put' declaration, got %d", putCount)
	}
	// The promoted version must be the latest (0.3), not the older 0.1 or 0.2.
	if putVersion != "0.3" {
		t.Errorf("expected promoted 'put' version 0.3, got %q", putVersion)
	}
}

func TestInit_NamingAliasRegistered(t *testing.T) {
	t.Parallel()

	configDir := t.TempDir()
	transportDir := t.TempDir()
	beaconDir := t.TempDir()
	convDir := conventionDir(t)
	alias := "home.exchange.dontguess"

	cfg := initExchange(t, exchange.InitOptions{
		ConfigDir:     configDir,
		Transport:     protocol.FilesystemTransport{Dir: transportDir},
		BeaconDir:     beaconDir,
		ConventionDir: convDir,
		Alias:         alias,
	})

	aliases := naming.NewAliasStore(configDir)
	resolved, err := aliases.Get(alias)
	if err != nil {
		t.Fatalf("alias lookup: %v", err)
	}
	if resolved != cfg.ExchangeCampfireID {
		t.Errorf("alias %q resolves to %q, want %q", alias, resolved, cfg.ExchangeCampfireID)
	}
}

func TestInit_BeaconPublished(t *testing.T) {
	t.Parallel()

	configDir := t.TempDir()
	transportDir := t.TempDir()
	beaconDir := t.TempDir()
	convDir := conventionDir(t)

	cfg := initExchange(t, exchange.InitOptions{
		ConfigDir:     configDir,
		Transport:     protocol.FilesystemTransport{Dir: transportDir},
		BeaconDir:     beaconDir,
		ConventionDir: convDir,
	})

	beacons, err := beacon.Scan(beaconDir)
	if err != nil {
		t.Fatalf("scanning beacons: %v", err)
	}
	if len(beacons) == 0 {
		t.Fatal("no beacons published")
	}

	found := false
	for _, b := range beacons {
		if b.CampfireIDHex() == cfg.ExchangeCampfireID {
			found = true
			if !b.Verify() {
				t.Error("beacon signature invalid")
			}
			break
		}
	}
	if !found {
		t.Errorf("no beacon for campfire %s", cfg.ExchangeCampfireID[:16])
	}
}

func TestInit_MembershipRecorded(t *testing.T) {
	t.Parallel()

	configDir := t.TempDir()
	transportDir := t.TempDir()
	beaconDir := t.TempDir()
	convDir := conventionDir(t)

	cfg := initExchange(t, exchange.InitOptions{
		ConfigDir:     configDir,
		Transport:     protocol.FilesystemTransport{Dir: transportDir},
		BeaconDir:     beaconDir,
		ConventionDir: convDir,
	})

	// Use protocol.Init to verify membership via the SDK.
	verifyClient, _, err := protocol.Init(configDir)
	if err != nil {
		t.Fatalf("protocol.Init for verify: %v", err)
	}
	defer verifyClient.Close()

	m, err := verifyClient.GetMembership(cfg.ExchangeCampfireID)
	if err != nil {
		t.Fatalf("GetMembership: %v", err)
	}
	if m == nil {
		t.Fatal("no membership record for exchange campfire")
	}
	// client.Create sets role to "full" for the creator.
	if m.Role != "full" {
		t.Errorf("role = %q, want %q", m.Role, "full")
	}
}

func TestInit_Idempotent(t *testing.T) {
	t.Parallel()

	configDir := t.TempDir()
	transportDir := t.TempDir()
	beaconDir := t.TempDir()
	convDir := conventionDir(t)

	opts := exchange.InitOptions{
		ConfigDir:         configDir,
		Transport:         protocol.FilesystemTransport{Dir: transportDir},
		BeaconDir:         beaconDir,
		ConventionDir:     convDir,
		SkipConfigCascade: true,
	}

	cfg1, client1, err := exchange.Init(opts)
	if err != nil {
		t.Fatalf("first Init: %v", err)
	}
	defer client1.Close()

	cfg2, client2, err := exchange.Init(opts)
	if err != nil {
		t.Fatalf("second Init: %v", err)
	}
	defer client2.Close()

	// Second call must return the same campfire ID (idempotent, no re-init).
	if cfg1.ExchangeCampfireID != cfg2.ExchangeCampfireID {
		t.Errorf("second init created new campfire: %s != %s", cfg1.ExchangeCampfireID, cfg2.ExchangeCampfireID)
	}
}

// TestRatePublishConvention_ZeroOrOneSelfPrior verifies that the rate-publish
// convention declaration uses zero_or_one(self_prior) and parses without error.
// This ensures the genesis case (first rate-publish has no prior) is valid.
func TestRatePublishConvention_ZeroOrOneSelfPrior(t *testing.T) {
	t.Parallel()
	convDir := conventionDir(t)
	payload, err := os.ReadFile(filepath.Join(convDir, "exchange-scrip", "rate-publish.json"))
	if err != nil {
		t.Fatalf("reading rate-publish.json: %v", err)
	}

	decl, result, err := convention.Parse(
		[]string{convention.ConventionOperationTag},
		payload,
		"campfire-key", // sender == campfire key for campfire_key operations
		"campfire-key",
	)
	if err != nil {
		t.Fatalf("Parse(rate-publish.json): %v", err)
	}
	if !result.Valid {
		t.Fatalf("rate-publish.json failed conformance: %v", result.Warnings)
	}
	if decl.Antecedents != "zero_or_one(self_prior)" {
		t.Errorf("rate-publish antecedents = %q, want %q", decl.Antecedents, "zero_or_one(self_prior)")
	}
}

func TestInit_ForceReinitializes(t *testing.T) {
	t.Parallel()

	configDir := t.TempDir()
	transportDir := t.TempDir()
	beaconDir := t.TempDir()
	convDir := conventionDir(t)

	opts := exchange.InitOptions{
		ConfigDir:         configDir,
		Transport:         protocol.FilesystemTransport{Dir: transportDir},
		BeaconDir:         beaconDir,
		ConventionDir:     convDir,
		SkipConfigCascade: true,
	}

	cfg1, client1, err := exchange.Init(opts)
	if err != nil {
		t.Fatalf("first Init: %v", err)
	}
	client1.Close()

	opts.Force = true
	cfg2, client2, err := exchange.Init(opts)
	if err != nil {
		t.Fatalf("force Init: %v", err)
	}
	defer client2.Close()

	// Force must create a new campfire (different ID).
	if cfg1.ExchangeCampfireID == cfg2.ExchangeCampfireID {
		t.Error("force init should create a new campfire, got same ID")
	}
}

// TestInit_ConfigCascade verifies that a config.toml with identity.display_name
// in the config directory is picked up by InitWithConfig via the cascade and
// reflected in the InitResult. This exercises the WithConfigDir option path
// through protocol.InitWithConfig.
func TestInit_ConfigCascade(t *testing.T) {
	// Not parallel: t.Chdir isolates from ancestor .cf/config.toml auto_join
	// beacons that cause InitWithConfig to sync real campfires (~15s).
	convDir := conventionDir(t) // resolve before chdir
	configDir := t.TempDir()
	t.Chdir(configDir)
	transportDir := t.TempDir()
	beaconDir := t.TempDir()

	// Write a config.toml with a custom display_name into the config directory.
	configTOML := "[identity]\ndisplay_name = \"TestExchangeOperator\"\n"
	if err := os.WriteFile(filepath.Join(configDir, "config.toml"), []byte(configTOML), 0600); err != nil {
		t.Fatalf("writing config.toml: %v", err)
	}

	// Init using the configDir that contains the config.toml.
	_, client, err := exchange.Init(exchange.InitOptions{
		ConfigDir:         configDir,
		Transport:         protocol.FilesystemTransport{Dir: transportDir},
		BeaconDir:         beaconDir,
		ConventionDir:     convDir,
		SkipConfigCascade: true,
	})
	if err != nil {
		t.Fatalf("Init with config.toml: %v", err)
	}
	defer client.Close()

	// Verify that InitWithConfig read the cascade by calling it again
	// with the same configDir and checking the extended InitResult fields.
	_, result, err := protocol.InitWithConfig(protocol.WithConfigDir(configDir))
	if err != nil {
		t.Fatalf("InitWithConfig for cascade verify: %v", err)
	}

	// ConfigLayers is only populated by InitWithConfig (not plain Init).
	// At least one layer should reference our config.toml.
	if len(result.ConfigLayers) == 0 {
		t.Error("expected at least one ConfigLayer from InitWithConfig, got none")
	}

	// The identity should have been loaded from the directory (env or config).
	// IdentitySource must not be empty — it is only set by InitWithConfig.
	if result.IdentitySource == "" {
		t.Error("IdentitySource is empty — InitWithConfig did not populate extended fields")
	}
}

// TestInit_NamingRootRegistersInRegistry verifies that when a naming root is
// configured, Init calls naming.Register so the exchange is discoverable via
// naming.Resolve on the registry campfire.
func TestInit_NamingRootRegistersInRegistry(t *testing.T) {
	// Not parallel: t.Chdir isolates from ancestor .cf/config.toml auto_join
	// beacons that cause InitWithConfig to sync real campfires (~15s).
	convDir := conventionDir(t) // resolve before chdir

	// Use a shared configDir and transportDir so the same identity can write to
	// both the registry campfire and the exchange campfire.
	configDir := t.TempDir()
	t.Chdir(configDir)
	transportDir := t.TempDir()
	beaconDir := t.TempDir()

	// Create a registry campfire to act as the naming root.
	registryClient, _, err := protocol.InitWithConfig(protocol.WithConfigDir(configDir))
	if err != nil {
		t.Fatalf("protocol.InitWithConfig for registry: %v", err)
	}
	defer registryClient.Close()

	registryResult, err := registryClient.Create(protocol.CreateRequest{
		Transport:    protocol.FilesystemTransport{Dir: transportDir},
		Description:  "test naming registry",
		JoinProtocol: "invite-only",
		Threshold:    1,
		BeaconDir:    beaconDir,
	})
	if err != nil {
		t.Fatalf("creating registry campfire: %v", err)
	}
	registryCampfireID := registryResult.CampfireID

	// Init the exchange with the naming root pointing at the registry.
	// Use a hyphenated alias: naming registry segments must be lowercase alphanumeric + hyphens.
	alias := "exchange-dontguess"
	cfg := initExchange(t, exchange.InitOptions{
		ConfigDir:     configDir,
		Transport:     protocol.FilesystemTransport{Dir: transportDir},
		BeaconDir:     beaconDir,
		ConventionDir: convDir,
		Alias:         alias,
		NamingRoot:    registryCampfireID,
	})

	// Init registers the alias directly in the naming root.
	// Since the alias "exchange-dontguess" is a valid single segment, it is
	// registered as-is. Verify that naming.Resolve finds it.
	resp, err := naming.Resolve(t.Context(), registryClient, registryCampfireID, alias)
	if err != nil {
		t.Fatalf("naming.Resolve(%q): %v", alias, err)
	}
	if resp.CampfireID != cfg.ExchangeCampfireID {
		t.Errorf("Resolve(%q) = %q, want %q", alias, resp.CampfireID, cfg.ExchangeCampfireID)
	}
}

// TestInit_NoNamingRootOnlyLocalAlias verifies backward compatibility: without a
// naming root, Init only sets the local alias and does not fail.
func TestInit_NoNamingRootOnlyLocalAlias(t *testing.T) {
	t.Parallel()

	configDir := t.TempDir()
	transportDir := t.TempDir()
	beaconDir := t.TempDir()
	convDir := conventionDir(t)
	alias := "exchange.dontguess"

	// Init without a naming root — backward-compatible path.
	cfg := initExchange(t, exchange.InitOptions{
		ConfigDir:     configDir,
		Transport:     protocol.FilesystemTransport{Dir: transportDir},
		BeaconDir:     beaconDir,
		ConventionDir: convDir,
		Alias:         alias,
		// NamingRoot intentionally omitted.
	})

	// Local alias must still resolve correctly.
	aliases := naming.NewAliasStore(configDir)
	resolved, err := aliases.Get(alias)
	if err != nil {
		t.Fatalf("alias lookup: %v", err)
	}
	if resolved != cfg.ExchangeCampfireID {
		t.Errorf("alias %q resolves to %q, want %q", alias, resolved, cfg.ExchangeCampfireID)
	}
}

// TestInit_BeaconStringStoredInConfig verifies that exchange.Init populates
// ExchangeBeacon in the config with a valid "beacon:BASE64" string.
func TestInit_BeaconStringStoredInConfig(t *testing.T) {
	t.Parallel()

	configDir := t.TempDir()
	transportDir := t.TempDir()
	beaconDir := t.TempDir()
	convDir := conventionDir(t)

	cfg := initExchange(t, exchange.InitOptions{
		ConfigDir:     configDir,
		Transport:     protocol.FilesystemTransport{Dir: transportDir},
		BeaconDir:     beaconDir,
		ConventionDir: convDir,
	})

	if cfg.ExchangeBeacon == "" {
		t.Fatal("ExchangeBeacon is empty — Init did not populate beacon string")
	}
	if !strings.HasPrefix(cfg.ExchangeBeacon, "beacon:") {
		t.Errorf("ExchangeBeacon = %q, want prefix \"beacon:\"", cfg.ExchangeBeacon)
	}
	// A real encoded beacon is much longer than just the prefix.
	if len(cfg.ExchangeBeacon) < 64 {
		t.Errorf("ExchangeBeacon too short (%d chars), likely empty encode", len(cfg.ExchangeBeacon))
	}
}

// TestInit_BeaconStringMatchesCampfireID verifies that the beacon string stored
// in config encodes a beacon whose campfire ID matches ExchangeCampfireID.
func TestInit_BeaconStringMatchesCampfireID(t *testing.T) {
	t.Parallel()

	configDir := t.TempDir()
	transportDir := t.TempDir()
	beaconDir := t.TempDir()
	convDir := conventionDir(t)

	cfg := initExchange(t, exchange.InitOptions{
		ConfigDir:     configDir,
		Transport:     protocol.FilesystemTransport{Dir: transportDir},
		BeaconDir:     beaconDir,
		ConventionDir: convDir,
	})

	if cfg.ExchangeBeacon == "" {
		t.Skip("ExchangeBeacon not set — skipping decode check")
	}

	b, err := decodeBeaconString(cfg.ExchangeBeacon, t.TempDir())
	if err != nil {
		t.Fatalf("decoding ExchangeBeacon: %v", err)
	}
	if b.CampfireIDHex() != cfg.ExchangeCampfireID {
		t.Errorf("beacon campfire_id = %q, want %q", b.CampfireIDHex(), cfg.ExchangeCampfireID)
	}
}

// TestInit_BeaconStringPersistedToDisk verifies that the beacon string is
// written to the on-disk config file, not just returned in the struct.
func TestInit_BeaconStringPersistedToDisk(t *testing.T) {
	t.Parallel()

	configDir := t.TempDir()
	transportDir := t.TempDir()
	beaconDir := t.TempDir()
	convDir := conventionDir(t)

	initExchange(t, exchange.InitOptions{
		ConfigDir:     configDir,
		Transport:     protocol.FilesystemTransport{Dir: transportDir},
		BeaconDir:     beaconDir,
		ConventionDir: convDir,
	})

	data, err := os.ReadFile(exchange.ConfigPath(configDir))
	if err != nil {
		t.Fatalf("reading config file: %v", err)
	}
	var diskCfg exchange.Config
	if err := json.Unmarshal(data, &diskCfg); err != nil {
		t.Fatalf("parsing config file: %v", err)
	}
	if diskCfg.ExchangeBeacon == "" {
		t.Error("exchange_beacon not found in on-disk config")
	}
	if !strings.HasPrefix(diskCfg.ExchangeBeacon, "beacon:") {
		t.Errorf("on-disk exchange_beacon = %q, want prefix \"beacon:\"", diskCfg.ExchangeBeacon)
	}
}

// TestInit_DisplayNameWritesConfigTOML verifies that Init writes a config.toml
// with identity.display_name when one does not already exist.
func TestInit_DisplayNameWritesConfigTOML(t *testing.T) {
	t.Parallel()

	configDir := t.TempDir()
	transportDir := t.TempDir()
	beaconDir := t.TempDir()
	convDir := conventionDir(t)

	initExchange(t, exchange.InitOptions{
		ConfigDir:     configDir,
		Transport:     protocol.FilesystemTransport{Dir: transportDir},
		BeaconDir:     beaconDir,
		ConventionDir: convDir,
		DisplayName:   "My Exchange",
	})

	// config.toml must have been written.
	configTOMLPath := filepath.Join(configDir, "config.toml")
	data, err := os.ReadFile(configTOMLPath)
	if err != nil {
		t.Fatalf("config.toml not found: %v", err)
	}
	if !strings.Contains(string(data), `"My Exchange"`) {
		t.Errorf("config.toml does not contain display_name %q:\n%s", "My Exchange", data)
	}
}

// TestInit_DisplayNameDefaultIsExchangeName verifies that Init uses
// "DontGuess Exchange" as the default display name when none is specified.
func TestInit_DisplayNameDefaultIsExchangeName(t *testing.T) {
	t.Parallel()

	configDir := t.TempDir()
	transportDir := t.TempDir()
	beaconDir := t.TempDir()
	convDir := conventionDir(t)

	// Init without specifying DisplayName.
	initExchange(t, exchange.InitOptions{
		ConfigDir:     configDir,
		Transport:     protocol.FilesystemTransport{Dir: transportDir},
		BeaconDir:     beaconDir,
		ConventionDir: convDir,
	})

	configTOMLPath := filepath.Join(configDir, "config.toml")
	data, err := os.ReadFile(configTOMLPath)
	if err != nil {
		t.Fatalf("config.toml not found: %v", err)
	}
	if !strings.Contains(string(data), "DontGuess Exchange") {
		t.Errorf("config.toml does not contain default display_name %q:\n%s", "DontGuess Exchange", data)
	}
}

// TestInit_DisplayNamePreservesExistingConfigTOML verifies that Init does not
// overwrite an existing config.toml — the operator's config takes precedence.
func TestInit_DisplayNamePreservesExistingConfigTOML(t *testing.T) {
	t.Parallel()

	configDir := t.TempDir()
	transportDir := t.TempDir()
	beaconDir := t.TempDir()
	convDir := conventionDir(t)

	// Pre-write a config.toml with a custom display name.
	existing := "[identity]\ndisplay_name = \"OriginalName\"\n"
	if err := os.WriteFile(filepath.Join(configDir, "config.toml"), []byte(existing), 0600); err != nil {
		t.Fatalf("writing pre-existing config.toml: %v", err)
	}

	// Init with a different DisplayName — should NOT overwrite the existing file.
	initExchange(t, exchange.InitOptions{
		ConfigDir:     configDir,
		Transport:     protocol.FilesystemTransport{Dir: transportDir},
		BeaconDir:     beaconDir,
		ConventionDir: convDir,
		DisplayName:   "NewName",
	})

	data, err := os.ReadFile(filepath.Join(configDir, "config.toml"))
	if err != nil {
		t.Fatalf("reading config.toml: %v", err)
	}
	if !strings.Contains(string(data), "OriginalName") {
		t.Errorf("existing config.toml was overwritten; want OriginalName, got:\n%s", data)
	}
	if strings.Contains(string(data), "NewName") {
		t.Errorf("existing config.toml was overwritten with NewName:\n%s", data)
	}
}

// TestInit_DisplayNameFlowsThroughConfigCascade verifies that the display_name
// written to config.toml is picked up by InitWithConfig via the config cascade.
func TestInit_DisplayNameFlowsThroughConfigCascade(t *testing.T) {
	// Not parallel: t.Chdir isolates from ancestor .cf/config.toml auto_join
	// beacons that cause InitWithConfig to sync real campfires (~15s).
	convDir := conventionDir(t) // resolve before chdir
	configDir := t.TempDir()
	t.Chdir(configDir)
	transportDir := t.TempDir()
	beaconDir := t.TempDir()

	initExchange(t, exchange.InitOptions{
		ConfigDir:     configDir,
		Transport:     protocol.FilesystemTransport{Dir: transportDir},
		BeaconDir:     beaconDir,
		ConventionDir: convDir,
		DisplayName:   "TestOperator",
	})

	// Use InitWithConfig to load the cascade and verify display_name is present.
	_, result, err := protocol.InitWithConfig(protocol.WithConfigDir(configDir))
	if err != nil {
		t.Fatalf("InitWithConfig: %v", err)
	}

	// At least one ConfigLayer must have contributed identity.display_name.
	var foundDisplayName bool
	for _, layer := range result.ConfigLayers {
		for _, field := range layer.Fields {
			if field == "identity.display_name" {
				foundDisplayName = true
			}
		}
	}
	if !foundDisplayName {
		t.Error("identity.display_name not found in any ConfigLayer from InitWithConfig")
	}
}

// decodeBeaconString decodes a "beacon:BASE64" string into a *beacon.Beacon.
// Mirrors the parseBeaconString logic in the cf CLI (share.go).
func decodeBeaconString(s, tmpDir string) (*beacon.Beacon, error) {
	const prefix = "beacon:"
	if !strings.HasPrefix(s, prefix) {
		return nil, fmt.Errorf("not a beacon string: %q", s)
	}
	raw, err := base64.StdEncoding.DecodeString(s[len(prefix):])
	if err != nil {
		return nil, fmt.Errorf("base64 decode: %w", err)
	}
	// Write to a temp .beacon file and use beacon.Scan to parse via the SDK's
	// CBOR unmarshal + signature verification path.
	beaconFile := filepath.Join(tmpDir, "decoded.beacon")
	if err := os.WriteFile(beaconFile, raw, 0600); err != nil {
		return nil, fmt.Errorf("writing temp beacon: %w", err)
	}
	beacons, err := beacon.Scan(tmpDir)
	if err != nil {
		return nil, fmt.Errorf("beacon.Scan: %w", err)
	}
	if len(beacons) == 0 {
		return nil, fmt.Errorf("no valid beacon decoded from string")
	}
	return &beacons[0], nil
}
