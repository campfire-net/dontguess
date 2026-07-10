// Package exchange implements the DontGuess exchange operator lifecycle.
//
// Nostr-first (docs/design/nostr-first-rebuild-decision.md): an exchange is no
// longer a campfire. `dontguess init` bootstraps the operator's OWN home under
// DG_HOME — a persistent secp256k1 (nostr) operator identity, the local
// append-only event store (pkg/store), and a config file recording the relay
// URLs the operator federates over. There is no campfire creation, beacon,
// naming registration, or convention promotion here.
package exchange

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/campfire-net/dontguess/pkg/identity"
	dgstore "github.com/campfire-net/dontguess/pkg/store"
)

// operatorKeyFile is the on-disk name of the persisted secp256k1 (nostr)
// operator private key within DG_HOME. It mirrors the name used by the serve
// path (cmd/dontguess/serve.go loadOrCreateNostrOperatorIdentity) so `init` and
// `serve` bootstrap the SAME operator identity.
const operatorKeyFile = "nostr-operator.key"

// storeFile is the DG_HOME-relative name of the local append-only event log.
const storeFile = "events.jsonl"

// Config is the local operator config written after init. It is campfire-free:
// it records the operator's nostr identity, the relay URLs the operator serves,
// and the local store path.
type Config struct {
	// OperatorKeyHex is the operator's nostr public key (x-only BIP-340 hex).
	OperatorKeyHex string `json:"operator_key"`
	// OperatorNpub is the NIP-19 bech32 encoding of OperatorKeyHex.
	OperatorNpub string `json:"operator_npub,omitempty"`
	// RelayURLs are the relay websocket URLs the operator federates over
	// (the DONTGUESS_RELAY_URLS the operator will serve).
	RelayURLs []string `json:"relay_urls,omitempty"`
	// StorePath is the absolute path to the local event log.
	StorePath string `json:"store_path"`
	// CreatedAt is the wall-clock nanosecond timestamp of first init.
	CreatedAt int64 `json:"created_at"`
	// TrustLevels configures per-operation trust floors (serve-path concern).
	// Left untouched by init; preserved here for the serve wiring that reads it.
	TrustLevels TrustLevels `json:"trust_levels,omitempty"`
	// MinReputation is the sell-side reputation floor. A fresh config defaults
	// to DefaultMinReputation (demotion-only rate-limiting); LoadConfig rejects
	// any value above MaxMinReputation.
	MinReputation int `json:"min_reputation,omitempty"`
	// FleetAllowlist is the flat, operator-maintained set of admitted seller
	// npubs (or hex pubkeys) for the team/federated tier — no vouching or
	// transitive edges. Mutated via `dontguess allowlist add|remove`. Only
	// consulted once a TrustChecker is constructed (relays attached); the
	// individual tier (no relays) never reads it.
	// See docs/design/nostr-admission-scrip-rehome-3b8.md §6.
	FleetAllowlist []string `json:"fleet_allowlist,omitempty"`
}

// DefaultMinReputation is the sell-side reputation floor written into a fresh
// config. Demotion-only rate-limiting per
// docs/design/nostr-admission-scrip-rehome-3b8.md §8 D3 — the flat
// FleetAllowlist remains the sole anti-poisoning primitive.
const DefaultMinReputation = 40

// MaxMinReputation is the highest MinReputation LoadConfig accepts. A floor
// above DefaultReputation (a fresh seller's starting reputation, 50) is a
// cold-start deadlock — no new seller could ever sell. Rejected loudly at
// load, never silently clamped.
const MaxMinReputation = 50

// DefaultMinBuyBalance is the anonymous-buy demand-signal bound applied on the
// team/federated tier (EngineOptions.MinBuyBalance) per
// docs/design/nostr-admission-scrip-rehome-3b8.md §8 D1. A buyer must hold at
// least this many scrip before a buy is allowed to contribute to matching /
// demand / pricing — closing the free-Sybil ranking-gaming lever. A value of 1
// is the least-restrictive correct bound: it blocks only the zero-scrip Sybil
// (the exact attack) and never a funded buyer (who necessarily holds more than
// price+fee to complete any purchase). Scrip enters only via x402 purchase or
// labor, so requiring a positive balance is an economic cost the Sybil cannot
// dodge. Operators may raise it. Individual tier (ScripStore nil) never applies it.
const DefaultMinBuyBalance int64 = 1

// InitOptions controls the campfire-free Init operation.
type InitOptions struct {
	// DGHome is the operator home directory. If empty, it resolves to the
	// DG_HOME environment variable, then $HOME/.dontguess.
	DGHome string
	// RelayURLs are recorded in the config as the relays the operator serves.
	RelayURLs []string
	// Force rewrites the config even if one already exists. The operator key is
	// NEVER overwritten regardless of Force (identity is load-or-create).
	Force bool
}

// resolveDGHome mirrors cmd/dontguess/dgpath.go: DG_HOME env, then
// $HOME/.dontguess. Kept package-local so pkg/exchange has no dependency on the
// cmd package.
func resolveDGHome(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if dg := os.Getenv("DG_HOME"); dg != "" {
		return dg
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".dontguess"
	}
	return filepath.Join(home, ".dontguess")
}

// ConfigPath returns the path to the exchange operator config file within
// dgHome.
func ConfigPath(dgHome string) string {
	return filepath.Join(dgHome, "dontguess-exchange.json")
}

// LoadConfig reads the exchange config from dgHome.
func LoadConfig(dgHome string) (*Config, error) {
	data, err := os.ReadFile(ConfigPath(dgHome))
	if err != nil {
		return nil, fmt.Errorf("reading exchange config: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing exchange config: %w", err)
	}
	if cfg.MinReputation > MaxMinReputation {
		return nil, fmt.Errorf("exchange config: min_reputation %d exceeds max %d (a floor above the fresh-seller starting reputation bricks onboarding — see docs/design/nostr-admission-scrip-rehome-3b8.md §8 D3)", cfg.MinReputation, MaxMinReputation)
	}
	return &cfg, nil
}

// Init bootstraps the operator's own DontGuess home campfire-free:
//
//	(a) operator identity — a persistent secp256k1 (nostr) key at
//	    $DG_HOME/nostr-operator.key, minted on first run and REUSED (never
//	    overwritten) thereafter. Written 0600.
//	(b) the local event store — the canonical append-only log at
//	    $DG_HOME/events.jsonl, created if absent.
//	(c) the config file — records the operator pubkey and the relay URLs the
//	    operator will serve.
//
// It is idempotent: re-running Init returns the same operator identity and
// leaves the store intact. If a config already exists and opts.Force is false,
// the existing config's CreatedAt is preserved. Returns the Config written to
// disk.
func Init(opts InitOptions) (*Config, error) {
	dgHome := resolveDGHome(opts.DGHome)
	if err := os.MkdirAll(dgHome, 0700); err != nil {
		return nil, fmt.Errorf("creating DG_HOME %s: %w", dgHome, err)
	}

	// (a) Operator identity — load-or-create, never overwrite.
	id, err := loadOrCreateOperatorIdentity(dgHome)
	if err != nil {
		return nil, fmt.Errorf("operator identity: %w", err)
	}

	// (b) Local event store — Open creates the file if absent (O_CREATE) and is
	// a no-op-open on an existing log. Close immediately: init only ensures it
	// exists; serve opens it for the engine.
	storePath := filepath.Join(dgHome, storeFile)
	st, err := dgstore.Open(storePath)
	if err != nil {
		return nil, fmt.Errorf("opening local store %s: %w", storePath, err)
	}
	if cerr := st.Close(); cerr != nil {
		return nil, fmt.Errorf("closing local store %s: %w", storePath, cerr)
	}

	// (c) Config — preserve CreatedAt across re-init for idempotency unless
	// Force is set (a forced re-init stamps a fresh CreatedAt).
	configPath := ConfigPath(dgHome)
	createdAt := time.Now().UnixNano()
	minReputation := DefaultMinReputation
	var fleetAllowlist []string
	if !opts.Force {
		if existing, lerr := LoadConfig(dgHome); lerr == nil && existing.CreatedAt != 0 {
			createdAt = existing.CreatedAt
			// Preserve operator-mutated fields (dontguess allowlist add|remove,
			// dontguess-b45) across an idempotent re-init — init must not clobber
			// state it does not itself manage.
			minReputation = existing.MinReputation
			fleetAllowlist = existing.FleetAllowlist
		}
	}

	cfg := &Config{
		OperatorKeyHex: id.PubKeyHex(),
		OperatorNpub:   id.Npub(),
		RelayURLs:      opts.RelayURLs,
		StorePath:      storePath,
		CreatedAt:      createdAt,
		MinReputation:  minReputation,
		FleetAllowlist: fleetAllowlist,
	}
	if err := WriteConfig(configPath, cfg); err != nil {
		return nil, fmt.Errorf("writing exchange config: %w", err)
	}

	return cfg, nil
}

// loadOrCreateOperatorIdentity returns the persisted secp256k1 (nostr) operator
// identity under dgHome, minting and persisting a fresh one on first run. The
// private key is stored 32-byte hex at 0600 and is NEVER overwritten once
// present (idempotent). This mirrors serve.go's loadOrCreateNostrOperatorIdentity
// so `init` and `serve` converge on the same operator key at
// $DG_HOME/nostr-operator.key.
func loadOrCreateOperatorIdentity(dgHome string) (*identity.Secp256k1Identity, error) {
	// Atomic create-or-load (dontguess-ed5): concurrent first-run init+serve on
	// a pristine DG_HOME converge on ONE operator key, and a loser never parses
	// a torn/empty file. See pkg/identity/keyfile.go §5.
	return identity.LoadOrCreatePrivHexKey(filepath.Join(dgHome, operatorKeyFile))
}

// WriteConfig serializes cfg to configPath (mode 0600). Exported so
// `dontguess allowlist add|remove` (cmd/dontguess/allowlist.go) can persist
// mutations to the same config file `dontguess init`/LoadConfig use, without
// duplicating the write logic.
func WriteConfig(configPath string, cfg *Config) error {
	if err := os.MkdirAll(filepath.Dir(configPath), 0700); err != nil {
		return fmt.Errorf("creating config dir: %w", err)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}
	return os.WriteFile(configPath, data, 0600)
}
