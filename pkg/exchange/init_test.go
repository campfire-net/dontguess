package exchange_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/campfire-net/campfire/pkg/beacon"
	"github.com/campfire-net/campfire/pkg/convention"
	"github.com/campfire-net/campfire/pkg/naming"
	"github.com/campfire-net/campfire/pkg/store"
	"github.com/campfire-net/campfire/pkg/transport/fs"

	"github.com/3dl-dev/dontguess/pkg/exchange"
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

func TestInit_CreatesExchangeCampfire(t *testing.T) {
	t.Parallel()

	cfHome := t.TempDir()
	transportDir := t.TempDir()
	beaconDir := t.TempDir()
	convDir := conventionDir(t)

	opts := exchange.InitOptions{
		CFHome:           cfHome,
		TransportBaseDir: transportDir,
		BeaconDir:        beaconDir,
		ConventionDir:    convDir,
		Alias:            "exchange.dontguess",
	}

	cfg, err := exchange.Init(opts)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}

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
	configPath := exchange.ConfigPath(cfHome)
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

	cfHome := t.TempDir()
	transportDir := t.TempDir()
	beaconDir := t.TempDir()
	convDir := conventionDir(t)

	cfg, err := exchange.Init(exchange.InitOptions{
		CFHome:           cfHome,
		TransportBaseDir: transportDir,
		BeaconDir:        beaconDir,
		ConventionDir:    convDir,
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	transport := fs.New(transportDir)
	campfireDir := transport.CampfireDir(cfg.ExchangeCampfireID)

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

	cfHome := t.TempDir()
	transportDir := t.TempDir()
	beaconDir := t.TempDir()
	convDir := conventionDir(t)

	cfg, err := exchange.Init(exchange.InitOptions{
		CFHome:           cfHome,
		TransportBaseDir: transportDir,
		BeaconDir:        beaconDir,
		ConventionDir:    convDir,
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	transport := fs.New(transportDir)
	msgs, err := transport.ListMessages(cfg.ExchangeCampfireID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}

	// Every message must carry convention:operation tag and parse cleanly.
	if len(msgs) == 0 {
		t.Fatal("no convention messages found in transport")
	}

	opNames := make(map[string]bool)
	for _, msg := range msgs {
		hasTag := false
		for _, tag := range msg.Tags {
			if tag == convention.ConventionOperationTag {
				hasTag = true
				break
			}
			// Skip non-convention messages (e.g. campfire:view).
			if tag == "campfire:view" {
				break
			}
		}
		if !hasTag {
			continue // non-convention message (view, etc.)
		}
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

// TestInit_PutNotDoublePromoted verifies that when both put.json (v0.1) and
// put-v0.2.json (v0.2) are present in the convention directory, only one
// "put" declaration is promoted — the highest version wins. Double-promotion
// would create ambiguous registry state without a supersedes chain.
func TestInit_PutNotDoublePromoted(t *testing.T) {
	t.Parallel()

	cfHome := t.TempDir()
	transportDir := t.TempDir()
	beaconDir := t.TempDir()
	convDir := conventionDir(t)

	cfg, err := exchange.Init(exchange.InitOptions{
		CFHome:           cfHome,
		TransportBaseDir: transportDir,
		BeaconDir:        beaconDir,
		ConventionDir:    convDir,
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	tr := fs.New(transportDir)
	msgs, err := tr.ListMessages(cfg.ExchangeCampfireID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}

	// Count how many promoted messages declare operation == "put".
	var putCount int
	var putVersion string
	for _, msg := range msgs {
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
	// The promoted version must be the latest (0.2), not the older 0.1.
	if putVersion != "0.2" {
		t.Errorf("expected promoted 'put' version 0.2, got %q", putVersion)
	}
}

func TestInit_NamingAliasRegistered(t *testing.T) {
	t.Parallel()

	cfHome := t.TempDir()
	transportDir := t.TempDir()
	beaconDir := t.TempDir()
	convDir := conventionDir(t)
	alias := "home.exchange.dontguess"

	cfg, err := exchange.Init(exchange.InitOptions{
		CFHome:           cfHome,
		TransportBaseDir: transportDir,
		BeaconDir:        beaconDir,
		ConventionDir:    convDir,
		Alias:            alias,
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	aliases := naming.NewAliasStore(cfHome)
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

	cfHome := t.TempDir()
	transportDir := t.TempDir()
	beaconDir := t.TempDir()
	convDir := conventionDir(t)

	cfg, err := exchange.Init(exchange.InitOptions{
		CFHome:           cfHome,
		TransportBaseDir: transportDir,
		BeaconDir:        beaconDir,
		ConventionDir:    convDir,
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}

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

	cfHome := t.TempDir()
	transportDir := t.TempDir()
	beaconDir := t.TempDir()
	convDir := conventionDir(t)

	cfg, err := exchange.Init(exchange.InitOptions{
		CFHome:           cfHome,
		TransportBaseDir: transportDir,
		BeaconDir:        beaconDir,
		ConventionDir:    convDir,
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	s, err := store.Open(store.StorePath(cfHome))
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	defer s.Close()

	m, err := s.GetMembership(cfg.ExchangeCampfireID)
	if err != nil {
		t.Fatalf("GetMembership: %v", err)
	}
	if m == nil {
		t.Fatal("no membership record for exchange campfire")
	}
	if m.Role != store.PeerRoleCreator {
		t.Errorf("role = %q, want %q", m.Role, store.PeerRoleCreator)
	}
}

func TestInit_Idempotent(t *testing.T) {
	t.Parallel()

	cfHome := t.TempDir()
	transportDir := t.TempDir()
	beaconDir := t.TempDir()
	convDir := conventionDir(t)

	opts := exchange.InitOptions{
		CFHome:           cfHome,
		TransportBaseDir: transportDir,
		BeaconDir:        beaconDir,
		ConventionDir:    convDir,
	}

	cfg1, err := exchange.Init(opts)
	if err != nil {
		t.Fatalf("first Init: %v", err)
	}

	cfg2, err := exchange.Init(opts)
	if err != nil {
		t.Fatalf("second Init: %v", err)
	}

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

	cfHome := t.TempDir()
	transportDir := t.TempDir()
	beaconDir := t.TempDir()
	convDir := conventionDir(t)

	opts := exchange.InitOptions{
		CFHome:           cfHome,
		TransportBaseDir: transportDir,
		BeaconDir:        beaconDir,
		ConventionDir:    convDir,
	}

	cfg1, err := exchange.Init(opts)
	if err != nil {
		t.Fatalf("first Init: %v", err)
	}

	opts.Force = true
	cfg2, err := exchange.Init(opts)
	if err != nil {
		t.Fatalf("force Init: %v", err)
	}

	// Force must create a new campfire (different ID).
	if cfg1.ExchangeCampfireID == cfg2.ExchangeCampfireID {
		t.Error("force init should create a new campfire, got same ID")
	}
}
