package exchange_test

import (
	"encoding/json"
	"os"
	"path/filepath"
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
func initExchange(t *testing.T, opts exchange.InitOptions) *exchange.Config {
	t.Helper()
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
	verifyClient, err := protocol.Init(configDir)
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

	verifyClient, err := protocol.Init(configDir)
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
	verifyClient, err := protocol.Init(configDir)
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
		ConfigDir:     configDir,
		Transport:     protocol.FilesystemTransport{Dir: transportDir},
		BeaconDir:     beaconDir,
		ConventionDir: convDir,
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
		ConfigDir:     configDir,
		Transport:     protocol.FilesystemTransport{Dir: transportDir},
		BeaconDir:     beaconDir,
		ConventionDir: convDir,
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
