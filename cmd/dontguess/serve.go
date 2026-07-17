package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/3dl-dev/dontguess/pkg/exchange"
	"github.com/3dl-dev/dontguess/pkg/identity"
	"github.com/3dl-dev/dontguess/pkg/matching"
	"github.com/3dl-dev/dontguess/pkg/nativebert"
	"github.com/3dl-dev/dontguess/pkg/pricing"
	"github.com/3dl-dev/dontguess/pkg/scrip"
	dgstore "github.com/3dl-dev/dontguess/pkg/store"
	"github.com/spf13/cobra"
	"gopkg.in/natefinch/lumberjack.v2"
)

// DefaultAutoAcceptMax is the default value for --auto-accept-max-price.
// It is exported so tests can assert against it without hardcoding the magic number.
// If this constant is changed, TestServeAutoAcceptMaxDefault will fail — update both together.
const DefaultAutoAcceptMax = int64(1_000_000)

var (
	servePollInterval  time.Duration
	serveAutoAccept    bool
	serveAutoAcceptMax int64
	// serveLocal is a retained no-op alias flag (dontguess-b14): the default
	// serve path is already campfire-free/local, so --local changes nothing.
	serveLocal bool

	// serveMediumLoopInterval is how often the pricing medium loop (dontguess-ffb,
	// restoring pkg/pricing.MediumLoop into the running serve) ticks. Each tick
	// scans inventory for high-demand uncompressed entries and posts open
	// (non-exclusive) compression assigns via Engine.PostOpenCompressionAssign —
	// the SAME cold-compression call path individual_ops_test.go's
	// TestOpListAssigns_Individual_SurfacesOpenCompressAssign already proves is
	// discoverable via `dontguess assigns` / OpListAssigns. Overridable so tests
	// don't wait a full hour for a tick; production default is
	// pricing.DefaultMediumLoopInterval (1h).
	serveMediumLoopInterval time.Duration

	// operatorConnDeadline is the per-connection deadline applied both initially
	// (stall protection) and again after AutoAcceptPut (dontguess-777 reset).
	// Exposed as a package-level variable so tests can shorten it without changing
	// production behaviour. Default is 5 seconds.
	operatorConnDeadline = 5 * time.Second

	// rosterPublishTimeout bounds each per-leg roster republish on a live allowlist
	// hot-reload (dontguess-113) so a dead/slow relay leg cannot hold the OpAllowlist
	// response past operatorConnDeadline — the live KeySet + config are already
	// updated by the time the roster is published, so it is best-effort.
	rosterPublishTimeout = 3 * time.Second
)

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Run the exchange engine",
	Long: `Start the DontGuess exchange engine. The engine is campfire-free: ingest and
egress both go through a local append-only event log under DG_HOME (pkg/store).
No 'dontguess init', no campfire join, no campfire identity — the operator key
and store file are created on first run.

  dontguess serve                      # default: 500ms poll, auto-accept puts
  dontguess serve --poll-interval 1s   # slower poll
  dontguess serve --no-auto-accept     # manual put approval only

Relay federation (optional, nostr): set DONTGUESS_RELAY_URL for a single relay,
or DONTGUESS_RELAY_URLS (comma-separated) for several, e.g.

  DONTGUESS_RELAY_URLS=ws://192.168.2.40:7777,ws://192.168.2.41:7777 dontguess serve

Each relay gets its own Intake (subscribe) + Outbox (publish) leg tailing the
same local log; both legs are off the buy/match hot path. When any relay is
configured the operator signs with a persisted secp256k1 (nostr) identity.

--local is accepted as a no-op alias (the default path is already local).`,
	RunE: runServe,
}

func init() {
	serveCmd.Flags().DurationVar(&servePollInterval, "poll-interval", 500*time.Millisecond, "how often to poll for new messages")
	serveCmd.Flags().BoolVar(&serveAutoAccept, "auto-accept", true, "automatically accept all puts at token cost")
	serveCmd.Flags().Int64Var(&serveAutoAcceptMax, "auto-accept-max-price", DefaultAutoAcceptMax, "maximum token cost to auto-accept (puts above this cap are classified as held-for-review)")
	serveCmd.Flags().BoolVar(&serveLocal, "local", false, "no-op alias: serve is always campfire-free/local (retained for backward compatibility)")
	serveCmd.Flags().DurationVar(&serveMediumLoopInterval, "medium-loop-interval", pricing.DefaultMediumLoopInterval, "how often the pricing medium loop scans inventory and posts open compression assigns for high-demand uncompressed entries")
	rootCmd.AddCommand(serveCmd)
}

func runServe(_ *cobra.Command, _ []string) error {
	// serve is always campfire-free; --local is a retained no-op alias.
	return runServeLocal(resolveDGHome())
}

// resolveRelayURLs returns the ordered, de-duplicated set of relay websocket
// URLs the operator federates over. It reads DONTGUESS_RELAY_URLS
// (comma-separated) followed by the legacy single DONTGUESS_RELAY_URL; empty
// entries are skipped and duplicates collapsed, preserving first-seen order.
func resolveRelayURLs() []string {
	var raw []string
	if v := strings.TrimSpace(os.Getenv("DONTGUESS_RELAY_URLS")); v != "" {
		raw = append(raw, strings.Split(v, ",")...)
	}
	if v := strings.TrimSpace(os.Getenv("DONTGUESS_RELAY_URL")); v != "" {
		raw = append(raw, v)
	}
	// No env override → fall back to the project-local .dg/config.json discovered by
	// walk-up (dontguess-884), so a client needs no DONTGUESS_RELAY_URLS env var.
	if len(raw) == 0 {
		if cfg, _ := loadClientConfig(); len(cfg.RelayURLs) > 0 {
			raw = append(raw, cfg.RelayURLs...)
		}
	}
	seen := make(map[string]struct{})
	out := make([]string, 0, len(raw))
	for _, u := range raw {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		if _, ok := seen[u]; ok {
			continue
		}
		seen[u] = struct{}{}
		out = append(out, u)
	}
	return out
}

// resolveServeTierAndRelays resolves the operator's EFFECTIVE tier and relay set
// at serve time from the PERSISTED config as the source of truth (dontguess-daa,
// design §1/§6), with env DONTGUESS_RELAY_URLS honored as a backward-compat
// override. It supersedes the old silent auto-detect (relay-present ⇒ team) by
// keying tier off the explicit persisted Config.Tier:
//
//   - LIVE-operator migration: an env-configured team operator (env relays set,
//     no persisted tier) is migrated to a persisted tier=team config on first
//     upgraded run — it is NEVER downgraded to solo. The migration merges into
//     any existing config so the operator key / socket path / allowlist survive,
//     minting the operator key from the identity only if the config lacks one.
//   - Effective relay set: env overrides persisted config; empty means no relay.
//   - Effective tier: the persisted declaration wins; an absent declaration
//     resolves to solo (backward compat — existing solo homes keep working).
//   - Fail-closed guard (INVERSE of §3.9): a declared team/fleet tier with NO
//     effective relay is a hard startup error naming the tier and the relay flag.
//
// It never substitutes a default relay endpoint: a clean env+config yields an
// empty relay set and the solo tier.
func resolveServeTierAndRelays(dgHome string, operatorIdentity *identity.Secp256k1Identity, logger *log.Logger) (exchange.Tier, []string, error) {
	envRelays := resolveRelayURLs()

	cfg, cfgErr := exchange.LoadConfig(dgHome)
	// Distinguish config-ABSENT from config-PRESENT-but-unreadable (dontguess-4f0,
	// CONFIRMED HIGH confidentiality-downgrade). An ABSENT config (os.ErrNotExist)
	// is legitimately solo — a fresh/clean home keeps working. A PRESENT config
	// that fails to load (truncated/corrupt JSON, bad perms, or the
	// min_reputation>max validation error) is a HARD startup error: swallowing it
	// and defaulting to solo would silently DOWNGRADE a declared team/fleet
	// operator to a PLAINTEXT solo store — the exact inverse of the fail-closed
	// requirement. Refuse to boot rather than downgrade. (os.ErrNotExist is
	// matched via errors.Is because LoadConfig wraps os.ReadFile's error with %w.)
	if cfgErr != nil && !errors.Is(cfgErr, os.ErrNotExist) {
		return "", nil, fmt.Errorf(
			"startup: operator config exists but is unreadable/corrupt — refusing to boot "+
				"(a declared team/fleet operator must NEVER silently downgrade to a plaintext solo store; "+
				"fix or remove %s): %w", exchange.ConfigPath(dgHome), cfgErr)
	}
	var cfgTier exchange.Tier
	var cfgRelays []string
	if cfgErr == nil {
		cfgTier = cfg.Tier
		cfgRelays = cfg.RelayURLs
	}

	// LIVE-operator migration (dontguess-daa BACKWARD-COMPAT): the durable
	// systemd --user operator (project memory "Live exchange") is env-configured
	// team-tier — DONTGUESS_RELAY_URLS set, and a persisted config that `init`
	// already wrote (operator key present) but with NO Tier field (the field is
	// new). On the first upgraded run persist tier=team + the env relays into that
	// existing config and emit a one-line deprecation notice. The live operator
	// must NOT be downgraded to solo.
	//
	// The migration fires ONLY for a config that LOADS with a NON-EMPTY operator
	// key (a genuine operator that already ran `init`). It deliberately does NOT
	// fire when the config is absent OR carries an empty operator_key — those are
	// exactly the two §3.9 rogue-competing-sequencer cases that
	// assertRelayServeHasOperatorConfig below MUST still refuse (minting/forking a
	// fresh operator key). Auto-creating or auto-filling a config here would
	// silently defeat that guard, so we do not. A tier already declared, or no env
	// relays, => no migration (a genuine solo operator with empty env+config stays
	// solo, byte-for-byte).
	migrateRelays := effectiveRelayURLs(envRelays, cfgRelays)
	if cfgErr == nil && cfg != nil && cfg.OperatorKeyHex != "" && cfgTier == "" && len(migrateRelays) > 0 {
		if cfg.StorePath == "" {
			cfg.StorePath = filepath.Join(dgHome, storeFileName)
		}
		cfg.Tier = exchange.TierTeam
		cfg.RelayURLs = migrateRelays
		if werr := exchange.WriteConfig(exchange.ConfigPath(dgHome), cfg); werr != nil {
			return "", nil, fmt.Errorf("migrating env relay config to persisted tier: %w", werr)
		}
		cfgTier = exchange.TierTeam
		cfgRelays = migrateRelays
		logger.Printf("  migration: relay configured with no persisted tier — migrated to persisted config (tier=team); env still honored")
	}

	// Effective relay set: env overrides persisted config (backward compat).
	relayURLs := effectiveRelayURLs(envRelays, cfgRelays)

	// Effective tier: the persisted declaration is authoritative; an absent
	// declaration resolves to solo (backward compat).
	effectiveTier := cfgTier
	if effectiveTier == "" {
		effectiveTier = exchange.TierSolo
	}

	// Fail-closed guard (INVERSE of §3.9): a declared team/fleet tier with NO
	// effective relay is a hard startup error — never a silent solo downgrade,
	// never a default relay, never a hang.
	if gerr := assertTierHasRelay(effectiveTier, relayURLs); gerr != nil {
		return "", nil, fmt.Errorf("startup: %w", gerr)
	}

	return effectiveTier, relayURLs, nil
}

// storeFileName is the DG_HOME-relative name of the local append-only event log
// (mirrors pkg/exchange's storeFile and the localStorePath below). Named here so
// the migration write in resolveServeTierAndRelays can stamp a store_path into a
// freshly-created config without importing the unexported pkg/exchange constant.
const storeFileName = "events.jsonl"

// effectiveRelayURLs returns the effective relay set: env relays override the
// persisted config relays (backward compat), else the persisted config relays.
func effectiveRelayURLs(envRelays, cfgRelays []string) []string {
	if len(envRelays) > 0 {
		return envRelays
	}
	return cfgRelays
}

// assertTierHasRelay is the config-time / serve-time fail-closed tier guard
// (dontguess-daa, design §1/§6) — the INVERSE of assertRelayServeHasOperatorConfig
// (§3.9). A declared team/fleet tier with NO effective relay is a hard error
// naming the tier and the relay flag: no silent solo downgrade, no default relay.
// Solo (and the empty/undeclared tier) never trips it.
func assertTierHasRelay(tier exchange.Tier, relayURLs []string) error {
	if tier.RequiresRelay() && len(relayURLs) == 0 {
		return fmt.Errorf(
			"declared %q tier requires a relay but none is configured — set DONTGUESS_RELAY_URLS "+
				"(or DONTGUESS_RELAY_URL) or persist relay_urls in the operator config, "+
				"or clear the persisted tier to run solo; refusing to start SOLO under a declared %s tier "+
				"(no silent downgrade, no default relay)", tier, tier)
	}
	return nil
}

// relayCursorPath returns the durable Outbox publish-cursor sidecar path for a
// relay, keyed by a hash of its URL so each configured relay tracks its own
// publish watermark independently (multiple relays tailing one local log must
// never share a cursor file).
func relayCursorPath(storePath, url string) string {
	h := sha256.Sum256([]byte(url))
	return fmt.Sprintf("%s.pubcursor.%s", storePath, hex.EncodeToString(h[:4]))
}

// intakeCursorPath returns the durable per-relay Intake-cursor sidecar path
// (dontguess-61a), mirroring relayCursorPath's per-URL-hash naming so a
// multi-relay operator tracks each relay's ingest watermark independently. It
// is a DIFFERENT sidecar file from relayCursorPath (".intakecursor." vs
// ".pubcursor." suffix) because the two cursors track opposite legs of the
// transport: relayCursorPath counts operator-published-and-ACKed records
// (Outbox), intakeCursorPath tracks the highest relay event created_at this
// operator has ingested (Intake) — conflating them would let a publish-heavy,
// ingest-light relay leg silently starve the backfill floor.
func intakeCursorPath(storePath, url string) string {
	h := sha256.Sum256([]byte(url))
	return fmt.Sprintf("%s.intakecursor.%s", storePath, hex.EncodeToString(h[:4]))
}

// newDenseEmbedderCached loads the pure-Go native MiniLM embedder
// (pkg/nativebert) ONLY if the model is already cached — it never downloads, so
// serve startup never blocks on a ~87 MB fetch and the operator socket comes up
// immediately (dontguess-31a). There is no python, no onnxruntime, and no shared
// library — the model runs entirely in Go. Returns nativebert.ErrModelNotCached
// when the model is absent, so the caller falls back to TF-IDF and points the
// operator at `dontguess embed pull`. DONTGUESS_EMBED_CACHE overrides the cache
// directory; empty uses nativebert.DefaultCacheDir.
func newDenseEmbedderCached() (matching.Embedder, error) {
	return matching.NewNativeEmbedderCached(os.Getenv("DONTGUESS_EMBED_CACHE"))
}

// shouldPrefetchModel decides whether serve should kick off a background model
// download. It is a pure function so every branch is testable. Prefetch is ON
// by default (operators get native dense automatically) but suppressed when:
// explicitly opted out (DONTGUESS_EMBED_NO_PREFETCH=1), running under `go test`
// (never touch the network from tests), or the model is already cached.
func shouldPrefetchModel(underTest bool, noPrefetchEnv string, cached bool) bool {
	if noPrefetchEnv == "1" {
		return false
	}
	if underTest {
		return false
	}
	return !cached
}

// maybePrefetchModel starts a non-blocking background download of the MiniLM
// model when appropriate, so a serve that fell back to TF-IDF (model not cached)
// activates native dense embeddings on its next restart. It never blocks serve
// startup and never runs during tests (flag "test.v" is registered by the go
// test harness). The download is atomic (Fetch → tmp + rename), so a serve that
// is killed mid-download leaves no partial file.
func maybePrefetchModel(logf func(string, ...any)) {
	cacheDir := os.Getenv("DONTGUESS_EMBED_CACHE")
	underTest := flag.Lookup("test.v") != nil
	if !shouldPrefetchModel(underTest, os.Getenv("DONTGUESS_EMBED_NO_PREFETCH"), nativebert.Cached(cacheDir)) {
		return
	}
	go func() {
		logf("  embedder:  prefetching all-MiniLM-L6-v2 (~87MB) in background — native dense matching activates on next restart (set DONTGUESS_EMBED_NO_PREFETCH=1 to disable)")
		if err := nativebert.Fetch(cacheDir); err != nil {
			logf("  embedder:  background model prefetch failed: %v — will retry next start, or run `dontguess embed pull`", err)
			return
		}
		logf("  embedder:  background model prefetch complete — restart serve to enable native dense embeddings")
	}()
}

// runServeLocal runs the exchange engine in standalone local-only mode
// (dontguess-275): no campfire relay, no campfire identity, no scrip network
// dependency. Ingest and egress both go through a local pkg/store event log
// under dgHome instead of a campfire ReadClient/WriteClient — see
// exchange.EngineOptions.LocalStore.
//
// No 'dontguess init' step is required: the secp256k1 nostr operator key
// (loadOrCreateNostrOperatorIdentity, the single stable P3 identity) and the
// event log file are created on first run inside dgHome, which this function
// creates if missing.
func runServeLocal(dgHome string) error {
	// Own the signal-driven shutdown context here so runServeLocalCtx can be
	// driven by tests with a cancelable context (bounded-deadline serve drives)
	// without racing a process-global SIGINT handler.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return runServeLocalCtx(ctx, dgHome)
}

func runServeLocalCtx(parentCtx context.Context, dgHome string) error {
	if err := os.MkdirAll(dgHome, 0700); err != nil {
		return fmt.Errorf("creating DG_HOME %s: %w", dgHome, err)
	}

	// P3 — ONE secp256k1 operator identity from solo onward (design §6, ADV-17).
	// The nostr operator key is minted at the FIRST `up` (solo OR relay) and its
	// PubKeyHex is State.OperatorKey from day one, so every operator record's Sender
	// is stable across the solo→relay climb: zero operator-record re-sign, no Sender
	// mismatch, no migration hang. This replaces the pre-P3 two-identity swap (opaque
	// local key solo / nostr key on relay attach). The individual tier stays
	// byte-identical in behavior — operatorSigner is a TRUE nil interface (no relay
	// signing, encryptedRequired off), ScripStore nil, plaintext-local — only the
	// operator IDENTITY is now permanent from the first solo run.
	operatorIdentity, err := loadOrCreateNostrOperatorIdentity(dgHome)
	if err != nil {
		return fmt.Errorf("nostr operator identity: %w", err)
	}
	engineOperatorKey := operatorIdentity.PubKeyHex()

	// Legacy migration (design §6, ADV-17): a pre-P3 solo home signed its operator
	// records under an opaque 16-byte local-operator.key (non-secp256k1). Read it if
	// it exists so applyLegacyOperatorAlias below re-attributes those historical
	// records to the stable nostr key. A fresh home has no such file (loadLegacy
	// returns "") → no alias, individual tier byte-for-byte unchanged. We NEVER
	// create this file anymore — the opaque local key is a read-only migration input.
	legacyOperatorKey, err := loadLegacyLocalOperatorKey(dgHome)
	if err != nil {
		return fmt.Errorf("legacy local operator key: %w", err)
	}

	localStorePath := filepath.Join(dgHome, "events.jsonl")
	localStore, err := dgstore.Open(localStorePath)
	if err != nil {
		return fmt.Errorf("opening local store %s: %w", localStorePath, err)
	}
	defer localStore.Close() //nolint:errcheck

	logDest, err := buildLogDest(dgHome)
	if err != nil {
		return fmt.Errorf("log setup: %w", err)
	}
	logger := log.New(logDest, "[exchange] ", log.LstdFlags|log.Lmsgprefix)

	// Config-time tier + relay resolution (dontguess-daa, design §1/§6). Tier is
	// EXPLICIT and read from the PERSISTED config as the source of truth — never
	// inferred from relay presence — with env DONTGUESS_RELAY_URLS honored as a
	// backward-compat override. This runs EARLY (after config load, before any
	// relay attach; merged-base note 347) and holds the fail-closed guard that is
	// the INVERSE of the §3.9 assertRelayServeHasOperatorConfig guard below: §3.9
	// fires when relays ARE present but the operator config is not; this fires
	// when a team/fleet tier IS declared but no relay is configured. Both stay.
	effectiveTier, relayURLs, err := resolveServeTierAndRelays(dgHome, operatorIdentity, logger)
	if err != nil {
		return err
	}

	// M2 relay transport (dontguess-4bd) is opt-in: when a relay is configured
	// (env DONTGUESS_RELAY_URLS or the persisted config), the local operator
	// additionally federates over NIP-42 relays. The SAME nostr operator identity
	// minted above signs the Outbox events and drives the NIP-42 handshake — there
	// is no second identity and no engineOperatorKey swap. No relay => unchanged
	// campfire-free single-agent mode.
	//
	// operatorSigner is the SAME operator identity as a TRUE nil interface when no
	// relays are attached (individual tier). Assigning the typed-nil
	// *Secp256k1Identity into the identity.Signer field would make a non-nil interface
	// holding a nil pointer (the dontguess-4bed / TrustChecker typed-nil trap), which
	// would arm encryptedRequired on the individual tier and break the confidential-
	// only guard's tier gating. Keep it untyped-nil unless a real relay is attached.
	var relaySigner *identity.Secp256k1Identity
	var operatorSigner identity.Signer
	if len(relayURLs) > 0 {
		relaySigner = operatorIdentity
		operatorSigner = relaySigner
	}

	// Team/federated-tier admission (dontguess-d53, design §3): when relays are
	// attached, build ONE TrustChecker from the operator-maintained fleet
	// allowlist. It gates the dispatch trust gate (Seam B) and the auto-accept
	// promotion gate (Seam A); the reputation floor source is the engine State,
	// so it is wired via SetReputationFloor AFTER NewEngine, before Start. The
	// individual/no-relay tier keeps trustChecker nil (fail-open is correct — the
	// operator is the sole local writer and local puts use random per-call sender
	// keys that a non-nil allowlist would brick). Config is best-effort: an absent
	// config yields an empty allowlist that admits only the operator key.
	var trustChecker *exchange.TrustChecker
	// rosterFold folds operator-signed kind-30078 fleet roster events into the SAME
	// KeySet the TrustChecker enforces, making the roster event the KeySet's source
	// of truth (design §2/P5, dontguess-c06). nil on the individual tier (no relay,
	// no roster). Built in the team-tier block below over ks, then handed to
	// attachRelayLegsAsync so every relay leg's reader routes rosters into it.
	var rosterFold *rosterFolder
	// liveKeySet is the SAME *KeySet the TrustChecker enforces and the rosterFolder
	// folds into — hoisted out of the team-tier block below so the allowlist
	// hot-reload controller (dontguess-113) can mutate it live. nil on the
	// individual/no-relay tier (no admission gate to reload).
	var liveKeySet *exchange.KeySet
	// scripStore stays a nil scrip.SpendingStore interface on the individual/
	// no-relay tier (content moves free — correct: the operator is the sole local
	// writer and local puts use random per-call sender keys). Declared here (not
	// assigned inside the branch as a concrete type) so the individual tier hands
	// NewEngine a true nil interface, not a typed-nil (design §4/§6).
	var scripStore scrip.SpendingStore
	minReputation := exchange.DefaultMinReputation
	// minBuyBalance stays 0 (disabled) on the individual/no-relay tier — there is
	// no ScripStore, so the anonymous-buy signal bound is a no-op there. It is
	// raised to the D1 default only on the team/federated tier below, where a
	// ScripStore is attached and payment (hence balances) exist.
	var minBuyBalance int64
	if len(relayURLs) > 0 {
		var fleet []string
		var revoked []string
		var cfgOperatorKeyHex string
		cfg, cerr := exchange.LoadConfig(dgHome)
		if cerr == nil {
			fleet = cfg.FleetAllowlist
			revoked = cfg.RevokedSellers
			minReputation = cfg.MinReputation
			cfgOperatorKeyHex = cfg.OperatorKeyHex
		}

		// §3.9 startup guard (design docs/design/nostr-first-client-ed2.md §3.9,
		// H6 / RT-C#2) — defense-in-depth behind the wrapper's individual-tier
		// auto-start gate. A relay-attached serve (len(relayURLs) > 0) MUST refuse
		// to start when the persisted exchange config is absent or carries an empty
		// OperatorKeyHex: `dontguess init` always writes the config with the
		// operator key before the first team-tier serve, so a legitimate operator
		// always passes. A stray/auto-started team-tier serve (the rogue competing
		// sequencer) instead fails LOUD here instead of silently minting a fresh
		// nostr operator key and forking the sequencer. Runs BEFORE
		// assertAdvertiseEqualsSign because an absent config is a stricter failure
		// than a key mismatch. The individual tier (no relay URLs) never reaches
		// this branch — byte-for-byte unaffected.
		if gerr := assertRelayServeHasOperatorConfig(cfgOperatorKeyHex, cerr); gerr != nil {
			return fmt.Errorf("startup: %w", gerr)
		}

		// ed5 HARD PREREQUISITE (design §5): the advertised operator key
		// (persisted config) MUST equal the relay signing key BEFORE the
		// ScripStore is constructed. A mismatch silently rebuilds every scrip
		// balance to zero via the LocalScripStore operator gate (relay_store.go)
		// and DoS's all buys — so it is a STARTUP HARD ERROR, never a warning,
		// and is NOT auto-reconciled to the signer (for an already-admitted relay
		// the config key may be the authoritative one; detect + alarm, let the
		// operator resolve).
		if aerr := assertAdvertiseEqualsSign(cfgOperatorKeyHex, relaySigner.PubKeyHex()); aerr != nil {
			return fmt.Errorf("startup: %w", aerr)
		}

		allow, aerr := identity.NewAllowlist(fleet...)
		if aerr != nil {
			return fmt.Errorf("fleet allowlist: %w", aerr)
		}
		ks := exchange.NewKeySet(allow.HexKeys()...)
		tc, terr := exchange.NewTrustChecker(engineOperatorKey, ks)
		if terr != nil {
			return fmt.Errorf("trust checker: %w", terr)
		}
		trustChecker = tc
		// Load the durable revocation tombstones (dontguess-23c) into the live
		// checker BEFORE the poll loop / first index rebuild, so SEAM D withholds
		// exactly the sellers de-allowlisted for cause and RETAINS everyone else's
		// accepted inventory across this restart. Normalize to hex (matching how the
		// KeySet is built from FleetAllowlist) so IsRevoked's exact-match compares
		// against the hex sender keys carried on events, regardless of stored form.
		revokedHex := make([]string, 0, len(revoked))
		for _, r := range revoked {
			if h, err := normalizeToHex(r); err == nil {
				revokedHex = append(revokedHex, h)
			}
		}
		tc.SetRevoked(revokedHex...)
		if len(revokedHex) > 0 {
			logger.Printf("[exchange]   retention:  %d revoked seller(s) withheld; all other accepted inventory retained", len(revokedHex))
		}
		liveKeySet = ks // shared with the allowlist hot-reload controller (dontguess-113)
		// Fold operator-signed fleet roster events (kind 30078) into THIS ks — the
		// same KeySet the TrustChecker/SEAM A/B gates enforce — so an operator-signed
		// roster on the relay log is the KeySet's source of truth (design §2/P5). The
		// static config allowlist above seeds the initial set; the relay backfill's
		// latest roster then reconciles it via ReplaceAll on fold.
		// Seed the anti-rollback floor from the durable per-DG_HOME sidecar so a
		// removed key stays removed across an operator restart independent of relay
		// honesty (dontguess-61a8). A corrupt/unreadable floor is a fail-closed
		// startup error rather than a silent reset to 0.
		rf, rferr := newRosterFolder(engineOperatorKey, ks, rosterCursorPath(dgHome), logger.Printf)
		if rferr != nil {
			return fmt.Errorf("roster fold: %w", rferr)
		}
		rosterFold = rf
		logger.Printf("  admission:  team tier — %d fleet npub(s) allowlisted, reputation floor %d", ks.Len(), minReputation)

		// Scrip accounting (design §4): payment is enforced on the team/federated
		// tier. NewLocalScripStore folds the local event log; engineOperatorKey is
		// the relay signing pubkey (== the advertised key, asserted above), so the
		// store's operator gate accepts operator-emitted scrip messages.
		ss, serr := scrip.NewLocalScripStore(localStore, engineOperatorKey)
		if serr != nil {
			return fmt.Errorf("scrip store: %w", serr)
		}
		scripStore = ss
		logger.Printf("  scrip:     enabled (LocalScripStore, operator-gated) — payment enforced")

		// D1 anonymous-buy demand-signal bound (design §8-D1): a buyer must hold
		// scrip before a buy contributes to matching/demand/pricing, closing the
		// free-Sybil ranking-gaming lever. Only meaningful with a ScripStore.
		minBuyBalance = exchange.DefaultMinBuyBalance
		logger.Printf("  buy-bound: anonymous-buy signal bound active — min buyer balance %d scrip", minBuyBalance)
	}

	// Pure-Go dense embeddings (all-MiniLM-L6-v2 via pkg/nativebert) when the
	// model is already cached — no python, no shared library, and no blocking
	// download at startup (the socket must come up promptly). When the model is
	// not cached the engine uses zero-dependency TF-IDF and points the operator
	// at `dontguess embed pull`. Force TF-IDF regardless with
	// DONTGUESS_EMBED_TFIDF=1 (measured to win at small inventory scale — see
	// docs/design/exchange-embedding-diagnostic-verdict-b.md).
	var embedder matching.Embedder
	if os.Getenv("DONTGUESS_EMBED_TFIDF") == "1" {
		logger.Printf("  embedder:  tf-idf (forced via DONTGUESS_EMBED_TFIDF=1)")
	} else if e, err := newDenseEmbedderCached(); err == nil {
		embedder = e
		logger.Printf("  embedder:  dense (all-MiniLM-L6-v2, pure-Go native)")
	} else if errors.Is(err, nativebert.ErrModelNotCached) {
		logger.Printf("  embedder:  tf-idf — dense model not cached; prefetching in background (or run `dontguess embed pull`)")
		maybePrefetchModel(logger.Printf)
	} else {
		logger.Printf("  WARNING: embedder falling back to tf-idf — native MiniLM load failed: %v", err)
	}

	// OnLocalAppend fan-out (design §3.8, H1): on the team/federated tier the
	// engine wakes every attached relay leg's Outbox the instant an operator record
	// is folded, so a match publishes sub-second instead of up to a full outbox
	// tick later. Legs register their Notify below (attachRelayTransport). On the
	// individual tier (no relays) OnLocalAppend stays nil — byte-for-byte unchanged.
	var appendNotify *appendNotifier
	var onLocalAppend func()
	if len(relayURLs) > 0 {
		appendNotify = &appendNotifier{}
		onLocalAppend = appendNotify.fire
	}

	// No ReadClient, no WriteClient — neither requires a campfire. ScripStore and
	// TrustChecker are non-nil only on the team/federated tier (relays attached);
	// on the individual tier they are nil, which means "skip these checks" (see
	// their EngineOptions doc). Payment enforcement and admission fall out of the
	// relays-attached branch above.
	// mediumLoopReady is closed by OnStarted (below) the instant Engine.Start has
	// finished its synchronous startup replay + dispatch-cursor seed +
	// dispatchPendingOrders — i.e. the moment State reflects the full persisted
	// log, immediately before the steady-state poll loop. The medium-loop
	// goroutine (started in runEngineLoop, dontguess-ffb) blocks on this before
	// its first Tick so a restart with pre-existing hot inventory is seen on that
	// first tick, instead of racing Start's replay and silently no-op'ing until
	// the next hourly tick.
	mediumLoopReady := make(chan struct{})

	eng := exchange.NewEngine(exchange.EngineOptions{
		CampfireID:        "local",
		LocalStore:        localStore,
		OperatorPublicKey: engineOperatorKey,
		OperatorSigner:    operatorSigner,
		Embedder:          embedder,
		PollInterval:      servePollInterval,
		TrustChecker:      trustChecker,
		ScripStore:        scripStore,
		MinBuyBalance:     minBuyBalance,
		OnLocalAppend:     onLocalAppend,
		// Team tier (scripStore != nil): auto-emit the operator settle(deliver) on a
		// fresh-hold-success buyer-accept (dontguess-55c GAP 2). The relay buyer
		// cannot emit the operator-gated deliver, and there is no manual operator in
		// the loop, so without this a funded buyer-accept holds scrip but content
		// never moves. Individual tier (scripStore == nil) leaves it false — and
		// handleSettleBuyerAcceptScrip never runs there anyway.
		AutoDeliverOnBuyerAccept: scripStore != nil,
		OnStarted:                func() { close(mediumLoopReady) },
		Logger: func(format string, args ...any) {
			logger.Printf(format, args...)
		},
	})

	// Wire the pricing medium loop (dontguess-ffb, restoring pkg/pricing into the
	// running serve — previously imported nowhere in cmd/, so it never ran in
	// production). Compression-assigns ONLY: MediumLoop.Tick's PostAssign
	// callback posts nothing but "compress" tasks (no validate/freshen task type
	// exists yet — those stay DESIGNED-ONLY / manual-post per item scope). Runs
	// on BOTH tiers — PostOpenCompressionAssign is a plain operator broadcast
	// (no ScripStore dependency), same call individual_ops_test.go's
	// TestOpListAssigns_Individual_SurfacesOpenCompressAssign already proves on
	// the individual tier. ScripStore/VigStore stay nil on the individual tier
	// (no relay) — MediumLoop treats nil as "skip residuals"/"zero vig", so the
	// other three Tick corrections (cluster dampening, residual settlement,
	// reputation floor) degrade to no-ops there instead of erroring.
	vigStore, _ := scripStore.(pricing.VigReader)
	mediumLoop := pricing.NewMediumLoop(pricing.MediumLoopOptions{
		State:      eng.State(),
		ScripStore: scripStore,
		VigStore:   vigStore,
		Interval:   serveMediumLoopInterval,
		// Engine.PostOpenCompressionAssign re-derives the bounty from the entry's
		// own TokenCost (ColdCompressionBountyPct) rather than trusting
		// spec.Reward, so only spec.EntryID is threaded through.
		PostAssign: func(spec pricing.AssignSpec) error {
			return eng.PostOpenCompressionAssign(spec.EntryID)
		},
		Logger: func(format string, args ...any) {
			logger.Printf(format, args...)
		},
	})

	// Wire the reputation floor now that the engine State exists (design §3:
	// AFTER NewEngine, BEFORE Start). The source is the engine's behavioral
	// SellerReputation; the floor gates sell-side (put) promotion in Seam A.
	// Individual/no-relay tier (trustChecker nil) skips this — no floor.
	if trustChecker != nil {
		trustChecker.SetReputationFloor(eng.State().SellerReputation, minReputation)
	}

	// Wire the Blossom blob store (dontguess-0fd) so the operator's applyPut can
	// FETCH+verify+gate an offloaded >32 KiB blob_pointer put (state_put.go drops
	// any blob_pointer put when s.blobStore == nil), and the deliver path can
	// reference the same content-addressed pointer. Same DONTGUESS_BLOSSOM_URL seam
	// the seller (put.go) and buyer (buy.go) resolve — sha256 content-addressing
	// keeps every node converged on one pointer. Unset -> nil store -> the ≤32 KiB
	// inline path is byte-for-byte unchanged and an oversize put is dropped by
	// applyPut's existing no-store guard rather than silently accepted.
	if bs := blobStoreFromEnv(); bs != nil {
		eng.State().SetBlobStore(bs)
		logger.Printf("  blobstore: Blossom >32 KiB offload/fetch enabled via DONTGUESS_BLOSSOM_URL")
	} else {
		logger.Printf("  blobstore: none (DONTGUESS_BLOSSOM_URL unset) — >32 KiB blob_pointer puts are dropped by applyPut")
	}

	// Legacy operator-key migration fold (design §6, ADV-17): register the opaque
	// pre-P3 local operator key as a wire-alias of the stable nostr operator key
	// BEFORE the engine's startup Replay (runEngineLoop → eng.Start), so historical
	// solo operator records (Sender == legacyOperatorKey) re-attribute to
	// State.OperatorKey during the fold instead of being dropped by the sender-must-
	// be-operator gate. Local only — no relay IO. No-op on a fresh home (no legacy
	// key), keeping the individual tier byte-for-byte unchanged.
	applyLegacyOperatorAlias(eng.State(), legacyOperatorKey, engineOperatorKey, logger.Printf)

	// Child of the caller-owned parentCtx so the relay-shutdown defer's cancel()
	// still unblocks every reader/outbox/in-flight dial, while a test (or the
	// signal wrapper) retains control of the parent for bounded-deadline drives.
	ctx, cancel := context.WithCancel(parentCtx)
	defer cancel()

	// Bind the operator IPC socket BEFORE the relay-attach loop (dontguess-347,
	// design §4/§9 Gate A/P1). status/accept-put/mint must respond within 1s of
	// serve start even with a dead/slow relay attached; attachRelayTransport's
	// initial REQ Send (and, transitively, Conn.dialAndAuth) can block for
	// seconds against an unreachable relay, so the socket cannot wait behind it.
	//
	// dontguess-7b2: a bind failure here (including a post-relocation failure
	// once the path has already been shortened under $XDG_RUNTIME_DIR) is a
	// HARD startup error — never a WARN-and-continue. A silently unbound socket
	// leaves the operator half-broken: the relay leg and engine loop run fine,
	// but every local CLI command ("operator not reachable") looks dead. Fail
	// loud instead so `up`/serve's caller sees the failure immediately.
	//
	// The relay legs are attached AFTER the socket binds (dontguess-347), but the
	// legs slice + its mutex are declared HERE so the allowlist hot-reload
	// controller's roster-republish closure (dontguess-113) can fan out to whatever
	// legs are attached at call time — reading them under legsMu, which the async
	// attach loop below writes under the same lock.
	var legsMu sync.Mutex
	var legs []relayLeg

	// allowlist hot-reload controller (dontguess-113, design §3 + §9 Gate B/P6):
	// the server-side half of live `dontguess allowlist add|remove`. It mutates the
	// live KeySet, republishes the operator roster, and persists Config.FleetAllowlist
	// — all gated behind an operator-key signature (verifyAllowlistAuth, ADV-16).
	// liveKeySet/operatorSigner are nil on the individual tier, where apply()
	// degrades to a config-only persist. The publishRoster closure fans an
	// operator-signed roster out to every attached relay leg, bounded per leg so a
	// dead relay cannot hold the CLI past the socket deadline (the live KeySet +
	// config are already updated by then — the publish is best-effort and reconciles
	// on the next fold).
	publishRoster := func(ev *identity.Event) {
		legsMu.Lock()
		pubs := make([]*demuxPublisher, 0, len(legs))
		for _, l := range legs {
			if l.publisher != nil {
				pubs = append(pubs, l.publisher)
			}
		}
		legsMu.Unlock()
		for _, p := range pubs {
			pctx, pcancel := context.WithTimeout(ctx, rosterPublishTimeout)
			if _, _, perr := p.PublishEvent(pctx, ev); perr != nil {
				logger.Printf("  allowlist: roster republish to a relay leg failed (live KeySet + config already updated; reconciles on next fold): %v", perr)
			}
			pcancel()
		}
	}
	allowCtrl := &allowlistController{
		keys:           liveKeySet,
		operatorSigner: operatorSigner,
		operatorKeyHex: engineOperatorKey,
		dgHome:         dgHome,
		publishRoster:  publishRoster,
		nowUnix:        func() int64 { return time.Now().Unix() },
		// Retention side of live de-admit / re-admit (dontguess-23c): revoke records
		// the durable tombstone + withholds the seller's inventory now; re-admit
		// clears it + re-indexes the retained inventory.
		onRevoke:  func(sellerHex string) { eng.DeAllowlistSeller(sellerHex) },
		onReadmit: eng.ReAllowlistSeller,
	}

	socketCleanup, err := bindOperatorSocket(ctx, dgHome, eng, logger, allowCtrl)
	if err != nil {
		return fmt.Errorf("operator socket: %w", err)
	}
	if socketCleanup != nil {
		defer socketCleanup()
	}

	// Attach one relay transport leg (Intake + Outbox + restart-seed) per
	// configured relay URL, ASYNCHRONOUSLY (dontguess-347): each leg's dial +
	// NIP-42 handshake + restart-seed + initial REQ Send runs in its own retry
	// goroutine, off the startup path, so a dead/slow relay never blocks the
	// operator socket (bound above) or the engine loop from coming up. Each leg
	// tails the SAME localStore and publishes with its own durable cursor
	// (relayCursorPath, keyed by URL) so the relays' publish watermarks never
	// collide; every leg reads the engine's fold, which is off the buy/match hot
	// path (docs/design/relay-transport.md §2.4).
	var relayWG sync.WaitGroup
	if len(relayURLs) > 0 {
		// CLIMB EGRESS FENCE (ADV-18, design §6 + §9 Gate A/P4). Establish the
		// solo→fleet climb watermark BEFORE the async legs attach: on the first
		// relay-attached serve it is the count of operator-authored records already
		// persisted (the pre-climb PLAINTEXT corpus the individual tier stored in
		// cleartext, §541 §6); on every later start it is the durable value written
		// then. Each leg's Outbox seeds its fresh cursor to this watermark so those
		// pre-climb records stay LOCAL-ONLY and are never republished to a relay in
		// cleartext. Fail LOUD on a corrupt watermark rather than silently
		// defaulting to 0 (which would un-fence and mass-broadcast the corpus).
		climbWatermark, cwerr := establishClimbWatermark(climbWatermarkPath(dgHome), localStore)
		if cwerr != nil {
			return fmt.Errorf("climb egress fence: %w", cwerr)
		}
		if climbWatermark > 0 {
			logger.Printf("  climb-fence: %d pre-climb local record(s) fenced local-only (never republished in cleartext, ADV-18)", climbWatermark)
		}
		// Operator-side invite-redeem handler (design §1/P8, ADV-15): the server-side
		// half of `dontguess invite`/`join`. Shared by every relay leg's reader so the
		// durable one-time redeemed-grant-id set is consistent across relays. It
		// promotes a redeemed member through the SAME allowlist controller `allowlist
		// add` uses (allowCtrl) and mints the genesis grant via eng. Built here (team
		// tier only — inside the len(relayURLs)>0 block) so a stray kind-3410 on the
		// individual tier is ignored.
		redeemH, rherr := newRedeemHandler(engineOperatorKey, allowCtrl, eng, redeemedInvitesPath(dgHome), logger.Printf)
		if rherr != nil {
			return fmt.Errorf("invite redeem handler: %w", rherr)
		}
		defer func() { _ = redeemH.redeemed.close() }()
		// onLegUp asserts config as the authoritative fleet roster on each freshly-
		// attached leg (dontguess-23c, #2): publish a fresh operator-signed roster
		// built from Config.FleetAllowlist (created_at=now) so a STALE roster left on
		// the relay cannot ReplaceAll-demote the live KeySet below config on fold.
		onLegUp := func(pub *demuxPublisher) {
			if pub == nil || operatorSigner == nil {
				return
			}
			ev, n, rerr := allowCtrl.rosterFromConfig()
			if rerr != nil {
				logger.Printf("  roster: config republish skipped (%v)", rerr)
				return
			}
			pctx, pcancel := context.WithTimeout(ctx, rosterPublishTimeout)
			defer pcancel()
			if _, _, perr := pub.PublishEvent(pctx, ev); perr != nil {
				logger.Printf("  roster: config republish to leg failed (reconciles on next fold): %v", perr)
				return
			}
			logger.Printf("  roster: asserted %d-member fleet roster from config on leg attach (supersedes any stale roster)", n)
		}
		attachRelayLegsAsync(ctx, &relayWG, &legsMu, &legs, relayURLs, localStore, relaySigner,
			localStorePath, appendNotify, eng, logger, climbWatermark, rosterFold, redeemH, onLegUp)
		// Combined shutdown in the dontguess-e35 order: cancel the context FIRST
		// (unblocks every reader/outbox and every in-flight dial/attach retry),
		// THEN wait for the attach goroutines to exit, THEN close each attached
		// connection and wait for its goroutines to exit. A bare `defer cancel()`
		// running last under LIFO would let stop() (wg.Wait()) run first and hang
		// forever; cancel-then-wait as one defer avoids that.
		defer func() {
			cancel()
			relayWG.Wait()
			legsMu.Lock()
			defer legsMu.Unlock()
			for _, leg := range legs {
				_ = leg.conn.Close()
				leg.stop()
			}
		}()
	}

	logger.Printf("exchange serving (campfire-free, tier=%s, %d relay URL(s) configured)", effectiveTier, len(relayURLs))
	logger.Printf("  operator:  %s", engineOperatorKey[:16]+"...")
	logger.Printf("  poll:      %s", servePollInterval)
	logger.Printf("  auto-accept: %v (max %d)", serveAutoAccept, serveAutoAcceptMax)
	logger.Printf("  store:     %s", localStorePath)
	logger.Printf("  logging to %s + stderr (rotate at 10MB, 5 backups, 28d retention, gzip)", filepath.Join(dgHome, "dontguess.log"))

	fmt.Printf("\n--- DontGuess exchange (campfire-free) ---\n")
	fmt.Printf("STORE=%s\n", localStorePath)
	fmt.Printf("OPERATOR_KEY=%s\n\n", engineOperatorKey)

	return runEngineLoop(ctx, dgHome, eng, mediumLoop, mediumLoopReady, logger)
}

// loadLegacyLocalOperatorKey reads the opaque pre-P3 local-operator.key under
// dgHome WITHOUT creating it, returning "" when the file does not exist (design
// §6, ADV-17). Since P3, `serve` mints a single secp256k1 nostr operator key and
// uses its pubkey as State.OperatorKey from the first solo run, so this opaque key
// is no longer created; it survives only in homes bootstrapped by pre-P3 binaries,
// where it is a read-only migration input (registered as a wire-alias of the nostr
// key so historical solo operator records re-attribute — see applyLegacyOperatorAlias).
func loadLegacyLocalOperatorKey(dgHome string) (string, error) {
	path := filepath.Join(dgHome, "local-operator.key")
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", path, err)
	}
	key := strings.TrimSpace(string(b))
	// dontguess-cbc: a corrupt/truncated legacy key file (anything shorter than
	// the 32-hex-char opaque 16-byte key loadOrCreateLocalOperatorKey always
	// wrote) must be a clear error here, never reach applyLegacyOperatorAlias's
	// legacyOperatorKey[:16] slice and panic on startup.
	if key != "" && len(key) < 16 {
		return "", fmt.Errorf("legacy local operator key at %s is truncated or corrupt (%d chars, expected 32 hex chars): remove or repair the file", path, len(key))
	}
	return key, nil
}

// applyLegacyOperatorAlias registers the opaque pre-P3 local operator key as a
// wire-alias of the stable nostr operator key on the engine State (design §6,
// ADV-17), so the engine's startup Replay re-attributes historical solo operator
// records (whose Sender is the legacy key) to State.OperatorKey instead of dropping
// them at the sender-must-be-operator gate. MUST be called BEFORE eng.Start (the
// alias must be in place for the fold). Local only — no relay IO. A no-op when
// there is no legacy key or it already equals the nostr key, which keeps a fresh
// home's individual tier byte-for-byte unchanged.
func applyLegacyOperatorAlias(st *exchange.State, legacyOperatorKey, operatorKey string, logf func(string, ...any)) {
	if legacyOperatorKey == "" || legacyOperatorKey == operatorKey {
		return
	}
	st.RegisterWireAlias(legacyOperatorKey, operatorKey)
	if logf != nil {
		// dontguess-cbc: guard the [:16] slice — loadLegacyLocalOperatorKey
		// rejects anything shorter than 16 chars, but keep this call site safe
		// against any other caller that doesn't route through that validation.
		preview := legacyOperatorKey
		if len(preview) > 16 {
			preview = preview[:16]
		}
		logf("  migration: legacy operator key %s… re-attributed to nostr operator identity (wire-alias, no re-sign)", preview)
	}
}

// loadOrCreateLocalOperatorKey returns the persisted opaque local operator key
// under dgHome, generating and persisting a fresh random one on first run.
//
// SINCE P3 (design §6, ADV-17) this is NO LONGER on the `serve` path: serve mints a
// single secp256k1 nostr operator key and uses its pubkey as State.OperatorKey from
// the first solo run (see runServeLocal + loadLegacyLocalOperatorKey). This helper
// is retained only for the individual-tier engine tests that build an engine with an
// arbitrary opaque operator key. It is an opaque local identifier only (no
// cryptographic identity, no relay).
func loadOrCreateLocalOperatorKey(dgHome string) (string, error) {
	// Atomic create-or-load (dontguess-ed5): concurrent first-runs converge on
	// ONE local operator key instead of racing WriteFile (last-writer-wins).
	// The key is an opaque 16-byte random hex identifier — no secp256k1 identity.
	return identity.LoadOrCreateRawKey(filepath.Join(dgHome, "local-operator.key"), func() (string, error) {
		b := make([]byte, 16)
		if _, err := rand.Read(b); err != nil {
			return "", fmt.Errorf("generating local operator key: %w", err)
		}
		return hex.EncodeToString(b), nil
	})
}

// loadOrCreateNostrOperatorIdentity returns the persisted secp256k1 (nostr)
// operator identity under dgHome, minting and persisting a fresh one on first
// run. This is the LONG-LIVED relay operator key: it signs the Outbox's
// published events and drives the NIP-42 handshake, and its content-hash event
// ids must be stable across process restarts for the restart-seed echo dedup to
// match (docs/design/relay-transport.md §2.2/§D). The private key is stored
// 32-byte hex at 0600 — handle only inside DG_HOME.
func loadOrCreateNostrOperatorIdentity(dgHome string) (*identity.Secp256k1Identity, error) {
	// Atomic create-or-load (dontguess-ed5): init and serve converge on the SAME
	// operator key even under a concurrent first-run race, so config's advertised
	// pubkey can never diverge from the on-disk signing key. See
	// pkg/identity/keyfile.go §5.
	return identity.LoadOrCreatePrivHexKey(filepath.Join(dgHome, "nostr-operator.key"))
}

// maxUnixSocketPathLen is a conservative bound on the socket path length we
// will attempt to bind directly under DG_HOME (dontguess-7b2). The kernel
// sockaddr_un.sun_path limit is 108 bytes on Linux and 104 on macOS/BSD,
// including the NUL terminator; staying comfortably under the smaller of the
// two (with headroom for the "ipc/dontguess.sock" suffix already included in
// the candidate path) avoids a bind failure that varies by platform.
const maxUnixSocketPathLen = 100

// resolveOperatorSocketPath returns the operator IPC socket path to bind for
// dgHome (dontguess-7b2, design §4/§9 Gate A/P2). The default candidate is
// $DG_HOME/ipc/dontguess.sock (unchanged from prior behavior); when that path
// is too long for the platform's unix socket length limit — the original
// root cause of the long-DG_HOME half-broken-operator bug — the socket is
// relocated to a short, deterministic path under $XDG_RUNTIME_DIR (falling
// back to os.TempDir() when XDG_RUNTIME_DIR is unset), hashed from dgHome so
// repeated serve runs against the same DG_HOME converge on the same socket
// path (mirrors the ssh-agent/docker sun_path-limit workaround).
func resolveOperatorSocketPath(dgHome string) string {
	defaultPath := filepath.Join(dgHome, "ipc", "dontguess.sock")
	if len(defaultPath) <= maxUnixSocketPathLen {
		return defaultPath
	}
	runtimeDir := os.Getenv("XDG_RUNTIME_DIR")
	if runtimeDir == "" {
		runtimeDir = os.TempDir()
	}
	hash := sha256.Sum256([]byte(dgHome))
	// dontguess-f8f: route through a dedicated per-operator subdirectory so the
	// 0700 chmod applied by listenOperatorSocket lands on a directory this
	// operator owns, never on the shared runtimeDir (or /tmp when
	// XDG_RUNTIME_DIR is unset).
	subdir := fmt.Sprintf("dontguess-%x", hash[:8])
	return filepath.Join(runtimeDir, subdir, "dontguess.sock")
}

// recordOperatorSocketPath persists the resolved socket path into the
// exchange config so CLI clients (socketPath() et al.) can find a relocated
// socket instead of assuming the default DG_HOME-relative path (dontguess-
// 7b2). Merges into any existing config rather than overwriting it — the
// team/federated-tier config (operator key, fleet allowlist, etc.) must
// survive this write. A missing config (individual tier, pre-init) is not an
// error: a fresh Config carrying only the socket path is written so a later
// `dontguess init` still LoadConfig-preserves it via its own merge logic.
func recordOperatorSocketPath(dgHome, sockPath string) error {
	cfg, err := exchange.LoadConfig(dgHome)
	if err != nil {
		cfg = &exchange.Config{}
	}
	cfg.OperatorSocketPath = sockPath
	return exchange.WriteConfig(exchange.ConfigPath(dgHome), cfg)
}

// bindOperatorSocket binds the operator IPC unix socket and starts serving
// requests over it in the background (dontguess-347). Extracted out of
// runEngineLoop so runServeLocal can call it BEFORE the relay-attach loop:
// status/accept-put/mint must respond within 1s of serve start even with a
// dead/slow relay attached, and attachRelayTransport's dial + NIP-42 handshake
// (Conn.dialAndAuth) can block for seconds against an unreachable relay — the
// socket cannot be left waiting behind it. The socket lives inside a 0700
// subdirectory so the parent-level permissions bound the TOCTOU window at the
// directory level (dontguess-33a, post-sec-regression fix).
//
// dontguess-7b2: the socket path is resolved via resolveOperatorSocketPath
// (relocating under $XDG_RUNTIME_DIR when the DG_HOME-relative path is too
// long for the platform's unix socket limit), and a bind failure — at either
// the default path OR the relocated path — is returned as a HARD error, never
// swallowed into a WARN. The resolved path is recorded into the exchange
// config on success so CLI clients can find it. Returns a cleanup func that
// closes the listener and removes the socket file, and a non-nil error if the
// socket could not be bound or the resolved path could not be persisted.
func bindOperatorSocket(ctx context.Context, dgHome string, eng *exchange.Engine, logger *log.Logger, allowCtrl ...*allowlistController) (func(), error) {
	sockPath := resolveOperatorSocketPath(dgHome)
	ln, err := listenOperatorSocket(sockPath)
	if err != nil {
		return nil, fmt.Errorf("bind operator socket at %s: %w", sockPath, err)
	}
	if rerr := recordOperatorSocketPath(dgHome, sockPath); rerr != nil {
		ln.Close()
		os.Remove(sockPath)
		return nil, fmt.Errorf("record operator socket path: %w", rerr)
	}
	logger.Printf("  operator socket: %s", sockPath)
	go serveOperatorSocket(ctx, ln, eng, allowCtrl...)
	return func() {
		ln.Close()
		os.Remove(sockPath)
	}, nil
}

// runEngineLoop wires the operator-facing plumbing shared by both serve
// paths — the auto-accept ticker, the pricing medium loop, and the engine
// event loop itself — around an already-configured Engine. Used by both
// runServe (campfire-backed) and runServeLocal (dontguess-275, campfire-free)
// so the two entrypoints differ only in how the Engine's ingest/egress are
// wired (campfire ReadClient/WriteClient vs. LocalStore). The operator IPC
// socket is bound separately by bindOperatorSocket (dontguess-347, called
// BEFORE the relay-attach loop) rather than here.
//
// mediumLoop/mediumLoopReady (dontguess-ffb) restore pkg/pricing.MediumLoop
// into the running serve: mediumLoop.Run blocks until mediumLoopReady closes
// (Engine.Start's OnStarted hook, fired after startup replay so the first tick
// sees the full persisted inventory, not a race against replay), then ticks on
// mediumLoop's own interval for the remainder of ctx's lifetime, exactly
// mirroring the auto-accept ticker's ctx.Done()-gated lifecycle below.
func runEngineLoop(ctx context.Context, dgHome string, eng *exchange.Engine, mediumLoop *pricing.MediumLoop, mediumLoopReady <-chan struct{}, logger *log.Logger) error {
	// Auto-accept goroutine.
	//
	// Dual-map design — two separate maps track over-cap puts, serving different consumers:
	//
	//   skippedPuts (this goroutine, local, not exported):
	//     Log-suppression guard. Keyed by put message ID. Owned exclusively by this
	//     goroutine — no mutex needed. Ensures each over-cap put produces exactly one
	//     "skipping put" log line across the lifetime of the operator process, rather
	//     than one line per tick (~86,400/day at 1s poll). Pruned lazily on each tick
	//     when a put leaves the pending snapshot (accepted, rejected, or removed).
	//
	//   heldForReview (State, synchronized, exported via PutsHeldForReview()):
	//     State-level classification. Keyed by put message ID inside State, protected
	//     by State's mutex. Consumed by the operator socket handler goroutine so the
	//     operator CLI ("dontguess operator status") can surface held puts for human
	//     review. Pruned by State.PruneHeldForReview() on the same tick cadence as
	//     skippedPuts, keeping both maps consistent.
	//
	// Both maps record the same over-cap put IDs, but serve different consumers with
	// different ownership and lifetime semantics.
	if serveAutoAccept {
		go func() {
			skippedPuts := make(map[string]struct{})
			ticker := time.NewTicker(1 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case t := <-ticker.C:
					eng.RunAutoAccept(serveAutoAcceptMax, t, skippedPuts)
				}
			}
		}()
	}

	// Pricing medium-loop goroutine (dontguess-ffb — restore pkg/pricing into the
	// running serve; it was imported nowhere in cmd/ before this, so the medium
	// loop that scans inventory and posts open compression assigns for
	// high-demand uncompressed entries never ran in production). Waits for
	// mediumLoopReady (Engine.Start's OnStarted, fired once startup replay +
	// dispatch-cursor seed complete) so the first tick sees the fully-replayed
	// inventory rather than racing eng.Start below. Signs as the operator — same
	// authority as every other operator-authored broadcast (auto-accept, warm
	// compression on buy) — no new signal lever (D1/anti-Sybil posture unchanged:
	// a compression assign's completion is verifiable labor, see
	// compress_protocol.go acceptance criteria).
	go func() {
		select {
		case <-mediumLoopReady:
		case <-ctx.Done():
			return
		}
		if err := mediumLoop.Run(ctx); err != nil && ctx.Err() == nil {
			logger.Printf("medium loop: unexpected exit: %v", err)
		}
	}()

	if err := eng.Start(ctx); err != nil && ctx.Err() == nil {
		return fmt.Errorf("engine error: %w", err)
	}

	logger.Printf("exchange shut down")
	return nil
}

// listenOperatorSocket removes any stale socket file and creates a new unix
// domain socket listener at path. The socket is placed inside a 0700
// parent directory so the TOCTOU window between net.Listen and any subsequent
// chmod is closed at the directory level — a local attacker cannot traverse
// into the ipc dir regardless of the socket inode's transient permissions.
//
// Security (dontguess-33a): earlier versions used syscall.Umask to narrow the
// socket's mode at creation time, but syscall.Umask is process-global and
// races with other goroutines (including the parallel Go test runtime, which
// calls mkdir under t.TempDir()). Using a restricted parent directory is both
// race-free and strictly stronger: even a mis-permissioned socket inode would
// be unreachable through a 0700 parent.
// operatorSocketProbeTimeout bounds the pre-bind liveness probe in
// listenOperatorSocket — short so a legitimate first start (stale/absent socket)
// is not delayed, long enough that a live local operator reliably accepts.
const operatorSocketProbeTimeout = 500 * time.Millisecond

func listenOperatorSocket(path string) (net.Listener, error) {
	// Create (or re-use) the restricted parent directory that holds the
	// socket. Using a dedicated subdirectory (not $DG_HOME itself) means we
	// can enforce 0700 without touching the user's broader config dir perms.
	parentDir := filepath.Dir(path)
	if err := os.MkdirAll(parentDir, 0700); err != nil {
		return nil, fmt.Errorf("create operator socket dir: %w", err)
	}
	// Re-chmod in case the directory pre-existed with looser perms.
	if err := os.Chmod(parentDir, 0700); err != nil {
		return nil, fmt.Errorf("chmod operator socket dir: %w", err)
	}

	// Refuse to CLOBBER a live operator (dontguess-884). os.Remove below unlinks
	// the socket file; doing that while another operator is actively listening on
	// it silently steals the path — the real operator's listener is orphaned and
	// its pidfile gets overwritten, breaking it. That is the "an unconfigured
	// client auto-started a serve that clobbered the real operator" failure. Probe
	// first: a socket that ACCEPTS a connection has a live owner, so fail closed.
	// Only a STALE socket (crash leftover, nothing listening) is removed and
	// rebound — os.Remove is required for that case because net.Listen("unix")
	// returns EADDRINUSE on a pre-existing path even when no process owns it.
	if conn, derr := net.DialTimeout("unix", path, operatorSocketProbeTimeout); derr == nil {
		_ = conn.Close()
		return nil, fmt.Errorf("operator already running on this DG_HOME (socket %s is live) — "+
			"refusing to start a competing operator that would clobber it", path)
	}
	// Remove the now-confirmed-stale socket file (crash leftover) if present.
	_ = os.Remove(path)

	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	// Belt-and-suspenders chmod — the parent 0700 is the primary guarantee.
	if err := os.Chmod(path, 0600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

// operatorRequest is the JSON shape received by the socket server.
type operatorRequest struct {
	Op        string `json:"op"`
	PutMsgID  string `json:"put_msg_id,omitempty"`
	Price     int64  `json:"price,omitempty"`
	Expires   string `json:"expires,omitempty"`
	Reason    string `json:"reason,omitempty"`
	Recipient string `json:"recipient,omitempty"`
	Amount    int64  `json:"amount,omitempty"`

	// MintAuth is the operator-key-signed authorization for an OpMint request
	// (dontguess-f91, RT-B#3). It is a BIP-340 Schnorr-signed nostr event,
	// authored by the persisted operator key, that binds the recipient+amount
	// being minted. The OpMint handler rejects the request unless this verifies
	// (verifyMintAuth) — socket reachability alone is NOT authorization for a
	// mint. nil on every other op.
	MintAuth *identity.Event `json:"mint_auth,omitempty"`

	// OpAllowlist fields (dontguess-113, design §3 + §9 Gate B/P6). AllowlistAction
	// is "add"|"remove"; AllowlistTarget is the fleet member's lowercase hex pubkey;
	// AllowlistAuth is the operator-key-signed authorization (allowlistAuthKind)
	// binding this exact action+target. The handler rejects the request unless
	// AllowlistAuth verifies (verifyAllowlistAuth) — socket reachability alone is
	// NOT authorization for a live admit (ADV-16). nil/empty on every other op.
	AllowlistAction string          `json:"allowlist_action,omitempty"`
	AllowlistTarget string          `json:"allowlist_target,omitempty"`
	AllowlistAuth   *identity.Event `json:"allowlist_auth,omitempty"`

	// OpPut/OpBuy fields (individual tier, zero-relay — design §3.3,
	// dontguess-2b4). Description/Content/TokenCost/ContentType/Domains mirror
	// pkg/relayclient.PutRequest's shape; Task/Budget/MaxResults mirror the
	// exchange:buy payload shape (state_buy.go parseBuyPayload).
	Description string   `json:"description,omitempty"`
	Content     string   `json:"content,omitempty"` // base64-encoded
	TokenCost   int64    `json:"token_cost,omitempty"`
	ContentType string   `json:"content_type,omitempty"`
	Domains     []string `json:"domains,omitempty"`
	Task        string   `json:"task,omitempty"`
	Budget      int64    `json:"budget,omitempty"`
	MaxResults  int      `json:"max_results,omitempty"`

	// CallerKey (OpListAssigns, dontguess-d26) is the caller-supplied hex pubkey
	// used to filter exclusive assigns down to ones the caller may claim
	// (ExclusiveSender==""  or  ==CallerKey). Optional — the individual tier has
	// no persisted per-call identity to supply here (design §3.3), so an empty
	// CallerKey simply surfaces only the non-exclusive open tasks. This is a
	// filter hint, never an authorization: OpListAssigns is a plain read with no
	// signed binding to CallerKey (see OpListAssigns doc, ipc.go).
	CallerKey string `json:"caller_key,omitempty"`
}

// serveOperatorSocket accepts connections on ln and handles operator IPC
// requests until ctx is cancelled. Each connection is dispatched to a goroutine
// so a hung client cannot block subsequent operator commands (dontguess-481a).
// A WaitGroup allows clean shutdown — the function returns only after all
// in-flight handlers finish.
func serveOperatorSocket(ctx context.Context, ln net.Listener, eng *exchange.Engine, allowCtrl ...*allowlistController) {
	ctrl := firstAllowlistController(allowCtrl)
	// Close the listener when the context is done so Accept unblocks.
	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	var wg sync.WaitGroup
	for {
		conn, err := ln.Accept()
		if err != nil {
			// Listener closed — wait for in-flight handlers then return.
			wg.Wait()
			return
		}
		if ctx.Err() != nil {
			conn.Close()
			wg.Wait()
			return
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			handleOperatorConn(conn, eng, ctrl)
		}()
	}
}

// handleOperatorConn reads one JSON request from conn, dispatches it, writes
// the JSON response, and closes the connection.
//
// Security (dontguess-481):
//
//	(b) A 5-second read deadline prevents a stalled client from holding the
//	    goroutine indefinitely.
//	(c) The connection reader is wrapped in an io.LimitReader (1 MiB) before
//	    being passed to json.NewDecoder, bounding memory allocation from
//	    oversized payloads. All legitimate requests are small JSON objects
//	    well under this ceiling.
func handleOperatorConn(conn net.Conn, eng *exchange.Engine, allowCtrl ...*allowlistController) {
	defer conn.Close()
	ctrl := firstAllowlistController(allowCtrl)

	// (b) Stall protection: abort if no full request arrives within operatorConnDeadline.
	conn.SetDeadline(time.Now().Add(operatorConnDeadline)) //nolint:errcheck

	// (c) OOM protection: cap input to 1 MiB.
	limited := io.LimitReader(conn, 1<<20)

	var req operatorRequest
	dec := json.NewDecoder(limited)
	if err := dec.Decode(&req); err != nil {
		writeOperatorResp(conn, map[string]any{"ok": false, "error": "bad request: " + err.Error()})
		return
	}

	enc := json.NewEncoder(conn)
	switch req.Op {
	case OpListHeld:
		held := eng.State().PutsHeldForReview()
		type entry struct {
			PutMsgID  string `json:"put_msg_id"`
			TokenCost int64  `json:"token_cost"`
			Seller    string `json:"seller"`
		}
		entries := make([]entry, 0, len(held))
		for _, e := range held {
			entries = append(entries, entry{
				PutMsgID:  e.PutMsgID,
				TokenCost: e.TokenCost,
				Seller:    e.SellerKey,
			})
		}
		enc.Encode(map[string]any{"puts": entries}) //nolint:errcheck

	case OpAcceptPut:
		var expiresAt time.Time
		if req.Expires != "" {
			t, err := time.Parse(time.RFC3339, req.Expires)
			if err != nil {
				writeOperatorResp(conn, map[string]any{"ok": false, "error": "invalid expires: " + err.Error()})
				return
			}
			expiresAt = t
		} else {
			expiresAt = time.Now().UTC().Add(72 * time.Hour)
		}
		price := req.Price
		if price == 0 {
			// Auto-price at 70% of token_cost. Must find the put in
			// heldForReview to compute the default price.
			held := eng.State().PutsHeldForReview()
			for _, e := range held {
				if e.PutMsgID == req.PutMsgID {
					price = e.TokenCost * 70 / 100
					break
				}
			}
		}
		// Defense in depth (dontguess-7d8): a client that bypasses the CLI
		// can call accept-put with price=0 and a stale/unknown put ID; without
		// this guard the server would list the content for free. The CLI
		// already returns an error on unknown IDs (dontguess-a70), but the
		// server must enforce the same invariant — a local process talking
		// directly to the socket is still in the trust boundary but must not
		// be able to trick the operator into a free accept.
		if price <= 0 {
			writeOperatorResp(conn, map[string]any{
				"ok":    false,
				"error": fmt.Sprintf("accept-put: price must be > 0 (put %q not found in held-for-review or no --price supplied)", req.PutMsgID),
			})
			return
		}
		// AutoAcceptPut acquires opMu, which is also held by the auto-accept
		// ticker goroutine during RunAutoAccept. If the ticker holds opMu when
		// this call arrives, we block until the tick finishes. Reset the
		// deadline after the potentially-blocking call so the response write
		// has a fresh window — without this, a slow tick could consume most of
		// the 5s deadline and the client would see an EOF instead of a response
		// (dontguess-777).
		err := eng.AutoAcceptPut(req.PutMsgID, price, expiresAt)
		conn.SetDeadline(time.Now().Add(operatorConnDeadline)) //nolint:errcheck
		if err != nil {
			writeOperatorResp(conn, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeOperatorResp(conn, map[string]any{"ok": true})

	case OpRejectPut:
		if err := eng.RejectPut(req.PutMsgID, req.Reason); err != nil {
			writeOperatorResp(conn, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeOperatorResp(conn, map[string]any{"ok": true})

	case OpMetrics:
		// Degradation counters (docs/design/relay-transport.md §2.4a D4 + §3):
		// dispatch trust-gate rejections, counted and alarmed rather than
		// silently dropped (dontguess-388). Read-only, no engine mutation.
		enc.Encode(map[string]any{"degradation": eng.DegradationSnapshot()}) //nolint:errcheck

	case OpMint:
		// Operator genesis-funding god-button (design §4). Reaching this socket
		// is NECESSARY but NOT sufficient (dontguess-f91, RT-B#3): the request
		// must ALSO carry an operator-key-signed authorization that binds this
		// exact recipient+amount. Without it, any local process able to connect
		// to the 0700-dir socket could trigger an operator-signed scrip-mint.
		// verifyMintAuth performs a REAL BIP-340 Schnorr verify against the
		// persisted operator key (State().OperatorKey) before eng.MintScrip is
		// ever called.
		if err := verifyMintAuth(req.MintAuth, eng.State().OperatorKey, req.Recipient, req.Amount); err != nil {
			writeOperatorResp(conn, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		// MintScrip emits a durable operator-signed scrip-mint and folds it live;
		// it returns an error on the individual tier (ScripStore=nil) and
		// audit-logs every mint.
		if err := eng.MintScrip(req.Recipient, req.Amount); err != nil {
			writeOperatorResp(conn, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeOperatorResp(conn, map[string]any{"ok": true})

	case OpAllowlist:
		// Live fleet-allowlist hot-reload (dontguess-113, design §3 + §9 Gate B/P6).
		// Reaching this socket is NECESSARY but NOT sufficient (ADV-16): apply()
		// runs verifyAllowlistAuth — a REAL BIP-340 Schnorr verify against the
		// persisted operator key, binding this exact action+target — BEFORE it
		// mutates the live KeySet, republishes the operator-signed roster, or
		// persists Config.FleetAllowlist. A nil controller means the serve process
		// did not wire hot-reload (should not happen — it is always built), so fail
		// closed rather than silently no-op.
		if ctrl == nil {
			writeOperatorResp(conn, map[string]any{"ok": false, "error": "allowlist: hot-reload controller unavailable"})
			return
		}
		err := ctrl.apply(req.AllowlistAction, req.AllowlistTarget, req.AllowlistAuth)
		// apply() may block on the roster republish (a bounded per-leg PublishEvent),
		// which can consume much of the 5s deadline; reset it so the response write
		// has a fresh window (mirrors the accept-put dontguess-777 reset).
		conn.SetDeadline(time.Now().Add(operatorConnDeadline)) //nolint:errcheck
		if err != nil {
			writeOperatorResp(conn, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeOperatorResp(conn, map[string]any{"ok": true})

	case OpPut:
		// Individual tier only (design §3.3, dontguess-2b4): zero relay, zero
		// identity ceremony, ScripStore==nil, no mint path. The write is routed
		// through eng.IngestLocalRecord (localMu-guarded fold), never a raw
		// store.Append. See individual_ops.go for the full doc.
		writeOperatorResp(conn, handleOpPut(eng, req))

	case OpBuy:
		// Individual tier only (design §3.3, dontguess-2b4). Extends conn's
		// deadline itself (opBuyConnDeadline) before its bounded server-side
		// await — see individual_ops.go.
		writeOperatorResp(conn, handleOpBuy(eng, conn, req))

	case OpListAssigns:
		// Assign-discovery (dontguess-d26, #2 AGENT DOOR): a plain read of live
		// engine State, no signed authorization required — see OpListAssigns doc,
		// ipc.go, and handleOpListAssigns, individual_ops.go.
		enc.Encode(handleOpListAssigns(eng, req)) //nolint:errcheck

	default:
		writeOperatorResp(conn, map[string]any{"ok": false, "error": "unknown op: " + req.Op})
	}
}

func writeOperatorResp(conn net.Conn, v any) {
	json.NewEncoder(conn).Encode(v) //nolint:errcheck
}

// assertRelayServeHasOperatorConfig implements the design §3.9 startup guard
// (H6 / RT-C#2, docs/design/nostr-first-client-ed2.md §3.9). A relay-attached
// serve (len(relayURLs) > 0) MUST refuse to start when the persisted exchange
// config is absent (loadErr != nil) or carries an empty OperatorKeyHex. This is
// defense-in-depth behind the wrapper's individual-tier auto-start gate (§3.10):
// even if a team-tier serve is auto-started by mistake, it fails LOUD here
// instead of silently minting a fresh nostr operator key and forking the
// sequencer. `dontguess init` writes the config (with the operator key) before
// the first team-tier serve, so a legitimate operator always passes. The
// individual tier (no relay URLs) never calls this — it is byte-for-byte
// unaffected.
func assertRelayServeHasOperatorConfig(cfgOperatorKeyHex string, loadErr error) error {
	if loadErr != nil {
		return fmt.Errorf(
			"relay-attached serve requires a persisted exchange config, but none could be loaded (%v) — "+
				"run `dontguess init` on the operator host before serving the team tier; refusing to start "+
				"(a config-less relay serve would mint a fresh operator key and fork the sequencer — "+
				"docs/design/nostr-first-client-ed2.md §3.9)", loadErr)
	}
	if cfgOperatorKeyHex == "" {
		return fmt.Errorf(
			"relay-attached serve requires a persisted operator key, but the exchange config carries an " +
				"empty operator_key — run `dontguess init` to populate it; refusing to start " +
				"(a config without an operator key would fork the sequencer — " +
				"docs/design/nostr-first-client-ed2.md §3.9)")
	}
	return nil
}

// assertAdvertiseEqualsSign fails closed (design §5, ed5) when the persisted
// config's advertised operator key disagrees with the relay signing key. A
// mismatch silently rebuilds every scrip balance to zero via the
// LocalScripStore operator gate (relay_store.go) and DoS's all buys, so it MUST
// be a startup hard error — not a warning, and NOT auto-reconciled to the
// signer. An empty configOperatorKeyHex (no persisted config yet) is not a
// mismatch: the config is created on first init with the signer's key.
func assertAdvertiseEqualsSign(configOperatorKeyHex, signerPubKeyHex string) error {
	if configOperatorKeyHex != "" && configOperatorKeyHex != signerPubKeyHex {
		return fmt.Errorf(
			"operator key mismatch: config advertises %s but the relay signing key is %s — "+
				"a mismatch silently zeroes every scrip balance (pkg/scrip/relay_store.go operator gate) and DoS's all buys; "+
				"resolve before serving (docs/design/nostr-admission-scrip-rehome-3b8.md §5)",
			configOperatorKeyHex, signerPubKeyHex)
	}
	return nil
}

// buildLogDest constructs the io.Writer used for the exchange logger.
// Logs go to both stderr (for foreground operation) and a rotating file
// at dgHome/dontguess.log (10 MB max, 5 backups, 28-day retention, gzip).
// dgHome must be the already-resolved DG_HOME path — callers should resolve
// it once via resolveDGHome() (dgpath.go) and pass the result here.
//
// Security (dontguess-ba9c): if the target log path is a symlink the function
// returns an error instead of opening the file. Opening a symlink allows an
// attacker who pre-creates a symlink at the log path to redirect operator logs
// into an arbitrary file the process can write (e.g. ~/.ssh/authorized_keys).
// Startup fails fast on this condition — a dangerous config should not be
// silently ignored.
func buildLogDest(dgHome string) (io.Writer, error) {
	logPath := filepath.Join(dgHome, "dontguess.log")

	// Symlink attack prevention: reject a pre-existing symlink at the log path.
	if info, err := os.Lstat(logPath); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("log path %q is a symlink — refusing to open (symlink attack prevention)", logPath)
		}
	}

	roller := &lumberjack.Logger{
		Filename:   logPath,
		MaxSize:    10, // megabytes
		MaxBackups: 5,
		MaxAge:     28, // days
		Compress:   true,
	}
	return io.MultiWriter(os.Stderr, roller), nil
}
