// Package exchange implements the DontGuess exchange operator lifecycle.
// An exchange is a campfire with the dontguess-exchange convention declarations
// promoted to its registry. Operators run Init to bootstrap a new exchange.
package exchange

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/campfire-net/campfire/pkg/convention"
	"github.com/campfire-net/campfire/pkg/naming"
	"github.com/campfire-net/campfire/pkg/protocol"
)

// Config is the local operator config written after init.
type Config struct {
	ExchangeCampfireID string           `json:"exchange_campfire_id"`
	OperatorKeyHex     string           `json:"operator_key"`
	ConventionVersion  string           `json:"convention_version"`
	Alias              string           `json:"alias"`
	CreatedAt          int64            `json:"created_at"`
	ProvenanceLevels   ProvenanceLevels `json:"provenance_levels,omitempty"`
}

// InitOptions controls the Init operation.
type InitOptions struct {
	// ConfigDir is the campfire config directory (default: ~/.campfire).
	// protocol.Init is called with this directory to load/generate identity and open store.
	ConfigDir string
	// Transport selects and configures the filesystem transport for the exchange campfire.
	// If nil, FilesystemTransport with fs.DefaultBaseDir() is used.
	Transport protocol.Transport
	// BeaconDir overrides the beacon publish directory. If empty, the SDK default is used.
	BeaconDir string
	// ConventionDir is the path to the directory containing exchange-core/ and
	// exchange-scrip/ sub-directories with the .json declaration files.
	// If empty, EmbeddedConventions is used.
	ConventionDir string
	// EmbeddedConventions is an embedded filesystem containing convention
	// declarations. Used as fallback when ConventionDir is empty. The FS
	// should contain docs/convention/exchange-core/*.json and
	// docs/convention/exchange-scrip/*.json.
	EmbeddedConventions fs.FS
	// Alias registers this exchange under the given naming alias
	// (e.g. "home.exchange.dontguess"). Default: "exchange.dontguess".
	Alias string
	// Description is posted as the exchange beacon description.
	Description string
	// Force overwrites an existing config if present.
	Force bool
}

func (o *InitOptions) configDir() string {
	if o.ConfigDir != "" {
		return o.ConfigDir
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

func (o *InitOptions) transport() protocol.Transport {
	if o.Transport != nil {
		return o.Transport
	}
	return protocol.FilesystemTransport{Dir: defaultTransportBaseDir()}
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

// defaultTransportBaseDir returns the default filesystem transport base directory.
// Mirrors fs.DefaultBaseDir() without importing the fs package.
func defaultTransportBaseDir() string {
	if env := os.Getenv("CF_TRANSPORT_DIR"); env != "" {
		return env
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "/tmp/campfire"
	}
	return filepath.Join(home, ".campfire", "transport")
}

// ConfigPath returns the path to the exchange operator config file.
func ConfigPath(configDir string) string {
	return filepath.Join(configDir, "dontguess-exchange.json")
}

// LoadConfig reads the exchange config from configDir.
func LoadConfig(configDir string) (*Config, error) {
	data, err := os.ReadFile(ConfigPath(configDir))
	if err != nil {
		return nil, fmt.Errorf("reading exchange config: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing exchange config: %w", err)
	}
	return &cfg, nil
}

// Init creates an exchange campfire via the campfire SDK, promotes convention
// declarations to its registry, and registers the exchange in the operator's
// naming hierarchy.
//
// Uses protocol.Init(configDir) to obtain a Client (loading/generating identity
// and opening the store), then client.Create to generate the campfire, initialize
// the transport, admit the operator as creator, and publish a beacon.
//
// Returns the Config written to disk and the open *protocol.Client. The caller
// is responsible for calling client.Close() when done. If a config already exists
// and opts.Force is false, returns the existing config without re-initializing
// (the returned client is still open and must be closed).
func Init(opts InitOptions) (*Config, *protocol.Client, error) {
	configDir := opts.configDir()
	configPath := ConfigPath(configDir)

	if err := os.MkdirAll(configDir, 0700); err != nil {
		return nil, nil, fmt.Errorf("creating config dir: %w", err)
	}

	// Open or create identity and store via SDK.
	client, _, err := protocol.Init(configDir)
	if err != nil {
		return nil, nil, fmt.Errorf("protocol.Init: %w", err)
	}

	// Check for existing config (after opening client so we can return it).
	if !opts.Force {
		if data, err := os.ReadFile(configPath); err == nil {
			var existing Config
			if jsonErr := json.Unmarshal(data, &existing); jsonErr == nil {
				return &existing, client, nil
			}
		}
	}

	// Create exchange campfire (invite-only, threshold=1).
	// Beacon publishing is handled internally by client.Create.
	createResult, err := client.Create(protocol.CreateRequest{
		Transport:    opts.transport(),
		Description:  opts.description(),
		JoinProtocol: "invite-only",
		Threshold:    1,
		BeaconDir:    opts.BeaconDir,
	})
	if err != nil {
		client.Close() //nolint:errcheck
		return nil, nil, fmt.Errorf("creating exchange campfire: %w", err)
	}

	campfireID := createResult.CampfireID

	// Promote convention declarations.
	if err := promoteDeclarations(opts.ConventionDir, opts.EmbeddedConventions, campfireID, client); err != nil {
		// Non-fatal: log but don't fail init — operator can re-promote later.
		fmt.Fprintf(os.Stderr, "warning: promoting convention declarations: %v\n", err)
	}

	// Create standard named views for convention read operations.
	// On a fresh init the store has no synced messages, so all views are created.
	viewsCreated, viewErr := EnsureViews(campfireID, client)
	if viewErr != nil {
		fmt.Fprintf(os.Stderr, "warning: creating named views: %v\n", viewErr)
	} else if viewsCreated > 0 {
		fmt.Fprintf(os.Stderr, "created %d named views\n", viewsCreated)
	}

	// Register in naming hierarchy.
	aliases := naming.NewAliasStore(configDir)
	alias := opts.alias()
	if err := aliases.Set(alias, campfireID); err != nil {
		// Non-fatal: alias failure doesn't block exchange use.
		fmt.Fprintf(os.Stderr, "warning: setting alias %q: %v\n", alias, err)
	}

	// Write config.
	cfg := &Config{
		ExchangeCampfireID: campfireID,
		OperatorKeyHex:     client.PublicKeyHex(),
		ConventionVersion:  "0.1",
		Alias:              alias,
		CreatedAt:          time.Now().UnixNano(),
	}
	if err := writeConfig(configPath, cfg); err != nil {
		client.Close() //nolint:errcheck
		return nil, nil, fmt.Errorf("writing exchange config: %w", err)
	}

	return cfg, client, nil
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

// promoteDeclarations reads all .json convention files, lints each, and posts
// them as convention:operation messages to the exchange campfire via client.Send.
//
// Source priority: conventionDir (filesystem path) > embeddedFS (go:embed).
// At least one must provide declarations.
//
// When multiple files declare the same convention+operation (e.g. put.json at
// v0.1 and put-v0.2.json at v0.2), only the highest version is promoted.
func promoteDeclarations(conventionDir string, embeddedFS fs.FS, campfireID string, client *protocol.Client) error {
	files, err := collectDeclarationFiles(conventionDir, embeddedFS)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("no convention declaration files found (set --convention-dir or embed conventions)")
	}

	// Collect and parse all candidates, deduplicating by convention+operation.
	winners := make(map[string]*declCandidate)
	var skipped int

	for _, f := range files {
		lintResult := convention.Lint(f.payload)
		if !lintResult.Valid {
			fmt.Fprintf(os.Stderr, "warning: skipping %s (lint failed): %v\n", f.name, lintResult.Errors)
			skipped++
			continue
		}
		cand, parseErr := parseVersionedDecl(f.name, f.name, f.payload)
		if parseErr != nil {
			fmt.Fprintf(os.Stderr, "warning: skipping %s (version parse): %v\n", f.name, parseErr)
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
				f.name, prev.name, cand.key)
		}
	}

	var promoted int
	for _, cand := range winners {
		if err := sendConventionMessage(campfireID, cand.payload, client); err != nil {
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

// declFile is a name + payload pair from either the filesystem or an embed.FS.
type declFile struct {
	name    string
	payload []byte
}

// collectDeclarationFiles gathers .json files from either a filesystem dir or
// an embedded FS. Filesystem takes priority if conventionDir is non-empty.
func collectDeclarationFiles(conventionDir string, embeddedFS fs.FS) ([]declFile, error) {
	if conventionDir != "" {
		return collectFromDisk(conventionDir)
	}
	if embeddedFS != nil {
		return collectFromEmbed(embeddedFS)
	}
	return nil, nil
}

func collectFromDisk(conventionDir string) ([]declFile, error) {
	var files []declFile
	for _, sub := range []string{"exchange-core", "exchange-scrip"} {
		dir := filepath.Join(conventionDir, sub)
		entries, err := os.ReadDir(dir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: reading declaration dir %s: %v\n", dir, err)
			continue
		}
		for _, e := range entries {
			if filepath.Ext(e.Name()) != ".json" {
				continue
			}
			payload, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				fmt.Fprintf(os.Stderr, "warning: reading %s: %v\n", e.Name(), err)
				continue
			}
			files = append(files, declFile{name: e.Name(), payload: payload})
		}
	}
	return files, nil
}

func collectFromEmbed(embedded fs.FS) ([]declFile, error) {
	var files []declFile
	for _, sub := range []string{"docs/convention/exchange-core", "docs/convention/exchange-scrip"} {
		entries, err := fs.ReadDir(embedded, sub)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if filepath.Ext(e.Name()) != ".json" {
				continue
			}
			payload, err := fs.ReadFile(embedded, sub+"/"+e.Name())
			if err != nil {
				fmt.Fprintf(os.Stderr, "warning: reading embedded %s: %v\n", e.Name(), err)
				continue
			}
			files = append(files, declFile{name: e.Name(), payload: payload})
		}
	}
	return files, nil
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


// sendConventionMessage sends a convention:operation message to the exchange
// campfire via client.Send.
func sendConventionMessage(campfireID string, payload []byte, client *protocol.Client) error {
	return sendTaggedMessage(campfireID, payload, []string{convention.ConventionOperationTag}, client)
}

// sendTaggedMessage sends a message with the given tags to the exchange campfire
// via client.Send. Used by both convention declarations and view creation.
func sendTaggedMessage(campfireID string, payload []byte, tags []string, client *protocol.Client) error {
	_, err := client.Send(protocol.SendRequest{
		CampfireID: campfireID,
		Payload:    payload,
		Tags:       tags,
	})
	return err
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
