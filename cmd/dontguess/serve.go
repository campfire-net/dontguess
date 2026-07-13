package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	"github.com/3dl-dev/dontguess/pkg/relay"
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

	// operatorConnDeadline is the per-connection deadline applied both initially
	// (stall protection) and again after AutoAcceptPut (dontguess-777 reset).
	// Exposed as a package-level variable so tests can shorten it without changing
	// production behaviour. Default is 5 seconds.
	operatorConnDeadline = 5 * time.Second
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

// relayCursorPath returns the durable Outbox publish-cursor sidecar path for a
// relay, keyed by a hash of its URL so each configured relay tracks its own
// publish watermark independently (multiple relays tailing one local log must
// never share a cursor file).
func relayCursorPath(storePath, url string) string {
	h := sha256.Sum256([]byte(url))
	return fmt.Sprintf("%s.pubcursor.%s", storePath, hex.EncodeToString(h[:4]))
}

// defaultEmbedScriptPath locates cmd/embed/main.py relative to the running
// binary instead of a hardcoded dev-machine absolute path (dontguess-740).
// It walks up from the executable's directory looking for a
// "cmd/embed/main.py" sibling, which holds for both `go run` (binary lives
// under a temp build dir but the repo checkout is still discoverable via
// os.Getwd as a fallback) and an installed binary sitting at the repo root
// or in a bin/ subdirectory. Returns "" if no candidate exists, in which
// case the caller must warn loudly rather than silently degrade.
func defaultEmbedScriptPath() string {
	const rel = "cmd/embed/main.py"

	candidates := []string{}
	if exe, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
		dir := filepath.Dir(exe)
		for i := 0; i < 6; i++ {
			candidates = append(candidates, filepath.Join(dir, rel))
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	if wd, err := os.Getwd(); err == nil {
		dir := wd
		for i := 0; i < 6; i++ {
			candidates = append(candidates, filepath.Join(dir, rel))
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}

	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

// runServeLocal runs the exchange engine in standalone local-only mode
// (dontguess-275): no campfire relay, no campfire identity, no scrip network
// dependency. Ingest and egress both go through a local pkg/store event log
// under dgHome instead of a campfire ReadClient/WriteClient — see
// exchange.EngineOptions.LocalStore.
//
// No 'dontguess init' step is required: the local operator key and the event
// log file are created on first run (loadOrCreateLocalOperatorKey), inside
// dgHome, which this function creates if missing.
func runServeLocal(dgHome string) error {
	if err := os.MkdirAll(dgHome, 0700); err != nil {
		return fmt.Errorf("creating DG_HOME %s: %w", dgHome, err)
	}

	localOperatorKey, err := loadOrCreateLocalOperatorKey(dgHome)
	if err != nil {
		return fmt.Errorf("local operator key: %w", err)
	}

	localStorePath := filepath.Join(dgHome, "events.jsonl")
	localStore, err := dgstore.Open(localStorePath)
	if err != nil {
		return fmt.Errorf("opening local store %s: %w", localStorePath, err)
	}
	defer localStore.Close() //nolint:errcheck

	// M2 relay transport (dontguess-4bd) is opt-in: when DONTGUESS_RELAY_URL is
	// set, the local operator additionally federates over a single NIP-42 relay.
	// The relay operator identity is a persisted secp256k1 (nostr) key — NOT the
	// opaque local key — because the Outbox signs events and the NIP-42 handshake
	// require it, and its pubkey becomes the engine's operator key so operator
	// records' Sender matches the key the Outbox re-signs with and the Intake
	// gates authorship on. Unset => unchanged campfire-free single-agent mode.
	relayURLs := resolveRelayURLs()
	var relaySigner *identity.Secp256k1Identity
	// operatorSigner is the SAME operator identity as a TRUE nil interface when
	// no relays are attached (individual tier). Passing the typed-nil
	// *Secp256k1Identity straight into the identity.Signer field would make a
	// non-nil interface holding a nil pointer (the dontguess-4bed / TrustChecker
	// typed-nil trap), which would arm encryptedRequired on the individual tier
	// and break the confidential-only guard's tier gating. Keep it untyped-nil
	// unless a real relay signer is loaded.
	var operatorSigner identity.Signer
	engineOperatorKey := localOperatorKey
	if len(relayURLs) > 0 {
		relaySigner, err = loadOrCreateNostrOperatorIdentity(dgHome)
		if err != nil {
			return fmt.Errorf("nostr operator identity: %w", err)
		}
		engineOperatorKey = relaySigner.PubKeyHex()
		operatorSigner = relaySigner
	}

	logDest, err := buildLogDest(dgHome)
	if err != nil {
		return fmt.Errorf("log setup: %w", err)
	}
	logger := log.New(logDest, "[exchange] ", log.LstdFlags|log.Lmsgprefix)

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
		var cfgOperatorKeyHex string
		cfg, cerr := exchange.LoadConfig(dgHome)
		if cerr == nil {
			fleet = cfg.FleetAllowlist
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

	// Use dense embeddings if the embed script is available (same as the
	// campfire-backed path — the matching engine has no campfire dependency
	// either way).
	var embedder matching.Embedder
	embedScript := os.Getenv("DONTGUESS_EMBED_SCRIPT")
	if embedScript == "" {
		embedScript = defaultEmbedScriptPath()
	}
	if embedScript != "" {
		if _, err := os.Stat(embedScript); err == nil {
			embedder = matching.NewDenseEmbedder(embedScript)
			logger.Printf("  embedder:  dense (all-MiniLM-L6-v2) via %s", embedScript)
		} else {
			logger.Printf("  WARNING: embedder falling back to tf-idf — dense embed script not found at %q. Set DONTGUESS_EMBED_SCRIPT to the absolute path of cmd/embed/main.py to restore dense matching quality.", embedScript)
		}
	} else {
		logger.Printf("  WARNING: embedder falling back to tf-idf — could not locate cmd/embed/main.py relative to the running binary. Set DONTGUESS_EMBED_SCRIPT to restore dense matching quality.")
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

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	// Attach one relay transport leg (Intake + Outbox + restart-seed) per
	// configured relay URL. Each leg tails the SAME localStore and publishes
	// with its own durable cursor (relayCursorPath, keyed by URL) so the relays'
	// publish watermarks never collide; every leg reads the engine's fold, which
	// is off the buy/match hot path (docs/design/relay-transport.md §2.4).
	type relayLeg struct {
		conn *relay.Conn
		stop func()
	}
	var legs []relayLeg
	for _, relayURL := range relayURLs {
		conn := relay.New(relayURL, relaySigner)
		stop, aerr := attachRelayTransport(ctx, localStore, relaySigner, relaySigner.PubKeyHex(),
			relayCursorPath(localStorePath, relayURL), conn, conn, 5*time.Second, logger.Printf, appendNotify,
			eng.State().RegisterWireAlias)
		if aerr != nil {
			return fmt.Errorf("attaching relay transport for %s: %w", relayURL, aerr)
		}
		legs = append(legs, relayLeg{conn: conn, stop: stop})
		logger.Printf("  relay:     %s (operator npub %s)", relayURL, relaySigner.Npub())
	}
	if len(legs) > 0 {
		// Combined shutdown in the dontguess-e35 order: cancel the context FIRST
		// (unblocks every reader/outbox), THEN close each connection and wait for
		// its goroutines to exit. A bare `defer cancel()` running last under LIFO
		// would let stop() (wg.Wait()) run first and hang forever; cancel-then-wait
		// as one defer avoids that.
		defer func() {
			cancel()
			for _, leg := range legs {
				_ = leg.conn.Close()
				leg.stop()
			}
		}()
	}

	logger.Printf("exchange serving (campfire-free, %d relay leg(s))", len(legs))
	logger.Printf("  operator:  %s", localOperatorKey[:16]+"...")
	logger.Printf("  poll:      %s", servePollInterval)
	logger.Printf("  auto-accept: %v (max %d)", serveAutoAccept, serveAutoAcceptMax)
	logger.Printf("  store:     %s", localStorePath)
	logger.Printf("  logging to %s + stderr (rotate at 10MB, 5 backups, 28d retention, gzip)", filepath.Join(dgHome, "dontguess.log"))

	fmt.Printf("\n--- DontGuess exchange (campfire-free) ---\n")
	fmt.Printf("STORE=%s\n", localStorePath)
	fmt.Printf("OPERATOR_KEY=%s\n\n", localOperatorKey)

	return runEngineLoop(ctx, dgHome, eng, logger)
}

// loadOrCreateLocalOperatorKey returns the persisted local operator key under
// dgHome, generating and persisting a fresh random one on first run. This is
// an opaque local identifier only (no cryptographic identity, no campfire
// relay involved) — it exists so exchange.State.OperatorKey (and therefore
// Sender on every locally-emitted match/put-accept/settle message) stays
// stable across restarts. Without persistence, a restart would pick a new
// key, and replayAllLocal replaying the prior run's log would see historical
// operator messages attributed to a Sender that no longer matches
// state.OperatorKey.
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

// runEngineLoop wires the operator-facing plumbing shared by both serve
// paths — the auto-accept ticker, the operator IPC socket, and the engine
// event loop itself — around an already-configured Engine. Used by both
// runServe (campfire-backed) and runServeLocal (dontguess-275, campfire-free)
// so the two entrypoints differ only in how the Engine's ingest/egress are
// wired (campfire ReadClient/WriteClient vs. LocalStore).
func runEngineLoop(ctx context.Context, dgHome string, eng *exchange.Engine, logger *log.Logger) error {
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

	// Unix socket IPC for operator CLI commands. The socket lives inside a
	// 0700 subdirectory so the parent-level permissions bound the TOCTOU
	// window at the dir level (dontguess-33a, post-sec-regression fix).
	sockPath := filepath.Join(dgHome, "ipc", "dontguess.sock")
	ln, err := listenOperatorSocket(sockPath)
	if err != nil {
		logger.Printf("warning: operator socket unavailable: %v", err)
	} else {
		logger.Printf("  operator socket: %s", sockPath)
		go serveOperatorSocket(ctx, ln, eng)
		defer func() {
			ln.Close()
			os.Remove(sockPath)
		}()
	}

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

	// Remove stale socket file if present.
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
}

// serveOperatorSocket accepts connections on ln and handles operator IPC
// requests until ctx is cancelled. Each connection is dispatched to a goroutine
// so a hung client cannot block subsequent operator commands (dontguess-481a).
// A WaitGroup allows clean shutdown — the function returns only after all
// in-flight handlers finish.
func serveOperatorSocket(ctx context.Context, ln net.Listener, eng *exchange.Engine) {
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
			handleOperatorConn(conn, eng)
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
func handleOperatorConn(conn net.Conn, eng *exchange.Engine) {
	defer conn.Close()

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
