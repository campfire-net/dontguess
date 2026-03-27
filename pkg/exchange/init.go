// Package exchange implements the DontGuess exchange operator lifecycle.
// An exchange is a campfire with the dontguess-exchange convention declarations
// promoted to its registry. Operators run Init to bootstrap a new exchange.
package exchange

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/campfire-net/campfire/pkg/beacon"
	"github.com/campfire-net/campfire/pkg/campfire"
	"github.com/campfire-net/campfire/pkg/convention"
	"github.com/campfire-net/campfire/pkg/identity"
	"github.com/campfire-net/campfire/pkg/message"
	"github.com/campfire-net/campfire/pkg/naming"
	"github.com/campfire-net/campfire/pkg/store"
	"github.com/campfire-net/campfire/pkg/transport/fs"
)

// Config is the local operator config written after init.
type Config struct {
	ExchangeCampfireID string `json:"exchange_campfire_id"`
	OperatorKeyHex     string `json:"operator_key"`
	ConventionVersion  string `json:"convention_version"`
	Alias              string `json:"alias"`
	CreatedAt          int64  `json:"created_at"`
}

// InitOptions controls the Init operation.
type InitOptions struct {
	// CFHome is the campfire home directory (default: ~/.campfire).
	CFHome string
	// TransportBaseDir is the filesystem transport root (default: /tmp/campfire).
	TransportBaseDir string
	// BeaconDir is the beacon directory (default: ~/.campfire/beacons).
	BeaconDir string
	// ConventionDir is the path to the directory containing exchange-core/ and
	// exchange-scrip/ sub-directories with the .json declaration files.
	ConventionDir string
	// Alias registers this exchange under the given naming alias
	// (e.g. "home.exchange.dontguess"). Default: "exchange.dontguess".
	Alias string
	// Description is posted as the exchange beacon description.
	Description string
	// Force overwrites an existing config if present.
	Force bool
}

func (o *InitOptions) cfHome() string {
	if o.CFHome != "" {
		return o.CFHome
	}
	if env := os.Getenv("CF_HOME"); env != "" {
		return env
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join("/tmp", ".campfire")
	}
	return filepath.Join(home, ".campfire")
}

func (o *InitOptions) transportBaseDir() string {
	if o.TransportBaseDir != "" {
		return o.TransportBaseDir
	}
	return fs.DefaultBaseDir()
}

func (o *InitOptions) beaconDir() string {
	if o.BeaconDir != "" {
		return o.BeaconDir
	}
	return beacon.DefaultBeaconDir()
}

func (o *InitOptions) alias() string {
	if o.Alias != "" {
		return o.Alias
	}
	return "exchange.dontguess"
}

func (o *InitOptions) description() string {
	if o.Description != "" {
		return o.Description
	}
	return "DontGuess exchange — token-work marketplace"
}

// ConfigPath returns the path to the exchange operator config file.
func ConfigPath(cfHome string) string {
	return filepath.Join(cfHome, "dontguess-exchange.json")
}

// LoadConfig reads the exchange config from cfHome.
func LoadConfig(cfHome string) (*Config, error) {
	data, err := os.ReadFile(ConfigPath(cfHome))
	if err != nil {
		return nil, fmt.Errorf("reading exchange config: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing exchange config: %w", err)
	}
	return &cfg, nil
}

// Init creates an exchange campfire, promotes convention declarations to its
// registry, registers the exchange in the operator's naming hierarchy, and
// publishes a discovery beacon.
//
// Returns the Config written to disk. If a config already exists and
// opts.Force is false, returns the existing config without re-initializing.
func Init(opts InitOptions) (*Config, error) {
	cfHome := opts.cfHome()
	configPath := ConfigPath(cfHome)

	// Check for existing config.
	if !opts.Force {
		if data, err := os.ReadFile(configPath); err == nil {
			var existing Config
			if jsonErr := json.Unmarshal(data, &existing); jsonErr == nil {
				return &existing, nil
			}
		}
	}

	// Load or generate operator identity.
	identityPath := filepath.Join(cfHome, "identity.json")
	var operatorID *identity.Identity
	if identity.Exists(identityPath) {
		id, err := identity.Load(identityPath)
		if err != nil {
			return nil, fmt.Errorf("loading operator identity: %w", err)
		}
		operatorID = id
	} else {
		id, err := identity.Generate()
		if err != nil {
			return nil, fmt.Errorf("generating operator identity: %w", err)
		}
		if err := id.Save(identityPath); err != nil {
			return nil, fmt.Errorf("saving operator identity: %w", err)
		}
		operatorID = id
	}

	// Create exchange campfire (invite-only, threshold=1).
	exchangeCF, err := campfire.New("invite-only", nil, 1)
	if err != nil {
		return nil, fmt.Errorf("creating exchange campfire: %w", err)
	}

	// Set up filesystem transport.
	transport := fs.New(opts.transportBaseDir())
	if err := transport.Init(exchangeCF); err != nil {
		return nil, fmt.Errorf("initializing transport: %w", err)
	}

	// Write operator as a member.
	if err := transport.WriteMember(exchangeCF.PublicKeyHex(), campfire.MemberRecord{
		PublicKey: operatorID.PublicKey,
		JoinedAt:  time.Now().UnixNano(),
	}); err != nil {
		return nil, fmt.Errorf("writing operator member record: %w", err)
	}

	// Open store and record membership.
	s, err := store.Open(store.StorePath(cfHome))
	if err != nil {
		return nil, fmt.Errorf("opening store: %w", err)
	}
	defer s.Close()

	transportDir := transport.CampfireDir(exchangeCF.PublicKeyHex())
	if err := s.AddMembership(store.Membership{
		CampfireID:   exchangeCF.PublicKeyHex(),
		TransportDir: transportDir,
		JoinProtocol: exchangeCF.JoinProtocol,
		Role:         store.PeerRoleCreator,
		JoinedAt:     store.NowNano(),
		Threshold:    exchangeCF.Threshold,
		Description:  opts.description(),
	}); err != nil {
		return nil, fmt.Errorf("recording exchange membership: %w", err)
	}

	// Promote convention declarations.
	if err := promoteDeclarations(opts.ConventionDir, exchangeCF.PublicKeyHex(), operatorID, transport); err != nil {
		// Non-fatal: log but don't fail init — operator can re-promote later.
		fmt.Fprintf(os.Stderr, "warning: promoting convention declarations: %v\n", err)
	}

	// Register in naming hierarchy.
	aliases := naming.NewAliasStore(cfHome)
	alias := opts.alias()
	if err := aliases.Set(alias, exchangeCF.PublicKeyHex()); err != nil {
		// Non-fatal: alias failure doesn't block exchange use.
		fmt.Fprintf(os.Stderr, "warning: setting alias %q: %v\n", alias, err)
	}

	// Publish beacon.
	b, err := beacon.New(
		exchangeCF.PublicKey,
		exchangeCF.PrivateKey,
		exchangeCF.JoinProtocol,
		exchangeCF.ReceptionRequirements,
		beacon.TransportConfig{
			Protocol: "filesystem",
			Config:   map[string]string{"dir": transportDir},
		},
		opts.description(),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: creating beacon: %v\n", err)
	} else if err := beacon.Publish(opts.beaconDir(), b); err != nil {
		fmt.Fprintf(os.Stderr, "warning: publishing beacon: %v\n", err)
	}

	// Write config.
	cfg := &Config{
		ExchangeCampfireID: exchangeCF.PublicKeyHex(),
		OperatorKeyHex:     operatorID.PublicKeyHex(),
		ConventionVersion:  "0.1",
		Alias:              alias,
		CreatedAt:          time.Now().UnixNano(),
	}
	if err := writeConfig(configPath, cfg); err != nil {
		return nil, fmt.Errorf("writing exchange config: %w", err)
	}

	return cfg, nil
}

// declCandidate holds a parsed declaration file ready for promotion.
type declCandidate struct {
	path    string
	name    string
	payload []byte
	// key is "convention:operation" — used for deduplication.
	key string
	// parsed version components for comparison.
	major, minor, patch int
}

// promoteDeclarations reads all .json files from conventionDir (searching
// exchange-core/ and exchange-scrip/ sub-directories), lints each, and posts
// them as convention:operation messages to the exchange campfire transport.
// If conventionDir is empty, the embedded declarations are used via the
// DefaultConventionDir discovery.
//
// When multiple files declare the same convention+operation (e.g. put.json at
// v0.1 and put-v0.2.json at v0.2), only the highest version is promoted.
// Promoting an older version alongside a newer one would create ambiguous
// registry state without a supersedes chain, so the older files are silently
// skipped.
func promoteDeclarations(conventionDir, campfireID string, agentID *identity.Identity, transport *fs.Transport) error {
	dirs := declarationDirs(conventionDir)
	if len(dirs) == 0 {
		return fmt.Errorf("no convention declaration directories found (set ConventionDir)")
	}

	// Collect and parse all candidates, deduplicating by convention+operation.
	// For each key we keep only the entry with the highest version.
	winners := make(map[string]*declCandidate)
	var skipped int

	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: reading declaration dir %s: %v\n", dir, err)
			continue
		}
		for _, e := range entries {
			if filepath.Ext(e.Name()) != ".json" {
				continue
			}
			path := filepath.Join(dir, e.Name())
			payload, err := os.ReadFile(path)
			if err != nil {
				fmt.Fprintf(os.Stderr, "warning: reading %s: %v\n", path, err)
				skipped++
				continue
			}
			lintResult := convention.Lint(payload)
			if !lintResult.Valid {
				fmt.Fprintf(os.Stderr, "warning: skipping %s (lint failed): %v\n", e.Name(), lintResult.Errors)
				skipped++
				continue
			}
			cand, parseErr := parseVersionedDecl(path, e.Name(), payload)
			if parseErr != nil {
				fmt.Fprintf(os.Stderr, "warning: skipping %s (version parse): %v\n", e.Name(), parseErr)
				skipped++
				continue
			}
			prev, exists := winners[cand.key]
			if !exists || declGreater(cand, prev) {
				if exists {
					fmt.Fprintf(os.Stderr, "info: %s supersedes %s for %s (v%d.%d.%d > v%d.%d.%d)\n",
						cand.name, prev.name, cand.key,
						cand.major, cand.minor, cand.patch,
						prev.major, prev.minor, prev.patch,
					)
				}
				winners[cand.key] = cand
			} else {
				fmt.Fprintf(os.Stderr, "info: skipping %s — %s is a higher version for %s\n",
					e.Name(), prev.name, cand.key)
			}
		}
	}

	// Promote the winning declaration for each operation.
	var promoted int
	for _, cand := range winners {
		if err := sendConventionMessage(campfireID, cand.payload, agentID, transport); err != nil {
			fmt.Fprintf(os.Stderr, "warning: promoting %s: %v\n", cand.name, err)
			skipped++
			continue
		}
		promoted++
	}

	if promoted == 0 && skipped > 0 {
		return fmt.Errorf("all %d declarations failed to promote", skipped)
	}
	return nil
}

// parseVersionedDecl extracts the convention, operation, and version from a
// declaration payload, returning a declCandidate for version comparison.
func parseVersionedDecl(path, name string, payload []byte) (*declCandidate, error) {
	var hdr struct {
		Convention string `json:"convention"`
		Operation  string `json:"operation"`
		Version    string `json:"version"`
	}
	if err := json.Unmarshal(payload, &hdr); err != nil {
		return nil, fmt.Errorf("parsing JSON header: %w", err)
	}
	if hdr.Convention == "" || hdr.Operation == "" {
		return nil, fmt.Errorf("missing convention or operation field")
	}
	major, minor, patch, err := parseDeclVersion(hdr.Version)
	if err != nil {
		return nil, fmt.Errorf("parsing version %q: %w", hdr.Version, err)
	}
	return &declCandidate{
		path:    path,
		name:    name,
		payload: payload,
		key:     hdr.Convention + ":" + hdr.Operation,
		major:   major,
		minor:   minor,
		patch:   patch,
	}, nil
}

// parseDeclVersion parses a "major.minor.patch", "major.minor", or "major"
// version string, returning the components as integers.
func parseDeclVersion(v string) (major, minor, patch int, err error) {
	if v == "" {
		return 0, 0, 0, nil // treat missing version as 0.0.0
	}
	parts := strings.SplitN(v, ".", 4)
	if len(parts) > 3 {
		return 0, 0, 0, fmt.Errorf("too many components in %q", v)
	}
	vals := [3]int{}
	for i, p := range parts {
		if p == "" {
			return 0, 0, 0, fmt.Errorf("empty component in %q", v)
		}
		n := 0
		for _, c := range p {
			if c < '0' || c > '9' {
				return 0, 0, 0, fmt.Errorf("non-numeric component %q in %q", p, v)
			}
			n = n*10 + int(c-'0')
		}
		vals[i] = n
	}
	return vals[0], vals[1], vals[2], nil
}

// declGreater reports whether a has a strictly higher version than b.
func declGreater(a, b *declCandidate) bool {
	if a.major != b.major {
		return a.major > b.major
	}
	if a.minor != b.minor {
		return a.minor > b.minor
	}
	return a.patch > b.patch
}

// declarationDirs returns the convention declaration sub-directories to scan.
// If conventionDir is empty, returns nil (caller handles missing dir).
func declarationDirs(conventionDir string) []string {
	if conventionDir == "" {
		return nil
	}
	var dirs []string
	for _, sub := range []string{"exchange-core", "exchange-scrip"} {
		d := filepath.Join(conventionDir, sub)
		if info, err := os.Stat(d); err == nil && info.IsDir() {
			dirs = append(dirs, d)
		}
	}
	return dirs
}

// sendConventionMessage creates, signs, and writes a convention:operation
// message to the exchange campfire transport.
func sendConventionMessage(campfireID string, payload []byte, agentID *identity.Identity, transport *fs.Transport) error {
	tags := []string{convention.ConventionOperationTag}

	msg, err := message.NewMessage(agentID.PrivateKey, agentID.PublicKey, payload, tags, nil)
	if err != nil {
		return fmt.Errorf("creating message: %w", err)
	}

	// Read campfire state and members for provenance hop.
	state, err := transport.ReadState(campfireID)
	if err != nil {
		return fmt.Errorf("reading campfire state: %w", err)
	}
	members, err := transport.ListMembers(campfireID)
	if err != nil {
		return fmt.Errorf("listing members: %w", err)
	}

	cf := state.ToCampfire(members)
	if err := msg.AddHop(
		state.PrivateKey, state.PublicKey,
		cf.MembershipHash(), len(members),
		state.JoinProtocol, state.ReceptionRequirements,
		campfire.RoleFull,
	); err != nil {
		return fmt.Errorf("adding provenance hop: %w", err)
	}

	if err := transport.WriteMessage(campfireID, msg); err != nil {
		return fmt.Errorf("writing message: %w", err)
	}
	return nil
}

// writeConfig serializes cfg to configPath (mode 0600).
func writeConfig(configPath string, cfg *Config) error {
	if err := os.MkdirAll(filepath.Dir(configPath), 0700); err != nil {
		return fmt.Errorf("creating config dir: %w", err)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}
	return os.WriteFile(configPath, data, 0600)
}
