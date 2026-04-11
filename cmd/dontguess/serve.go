package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/campfire-net/dontguess/pkg/exchange"
	"github.com/campfire-net/dontguess/pkg/matching"
	"github.com/campfire-net/dontguess/pkg/scrip"
	"github.com/campfire-net/campfire/pkg/protocol"
	"github.com/campfire-net/campfire/pkg/provenance"
	"github.com/campfire-net/campfire/pkg/store"
	"github.com/spf13/cobra"
	"gopkg.in/natefinch/lumberjack.v2"
)

var (
	servePollInterval  time.Duration
	serveAutoAccept    bool
	serveAutoAcceptMax int64
)

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Run the exchange engine",
	Long: `Start the DontGuess exchange engine. The engine subscribes to the campfire
for new messages (put, buy, settle) and processes them via the SDK Subscribe API.

  dontguess serve                      # default: 500ms poll, auto-accept puts
  dontguess serve --poll-interval 1s   # slower poll
  dontguess serve --no-auto-accept     # manual put approval only

The SDK's sync-before-query handles filesystem transport sync automatically.`,
	RunE: runServe,
}

func init() {
	serveCmd.Flags().DurationVar(&servePollInterval, "poll-interval", 500*time.Millisecond, "how often to poll for new messages")
	serveCmd.Flags().BoolVar(&serveAutoAccept, "auto-accept", true, "automatically accept all puts at token cost")
	serveCmd.Flags().Int64Var(&serveAutoAcceptMax, "auto-accept-max-price", 1000000, "maximum token cost to auto-accept (puts above this cap are classified as held-for-review)")
	rootCmd.AddCommand(serveCmd)
}

func runServe(_ *cobra.Command, _ []string) error {
	// Build two clients via protocol.InitWithConfig — both share the same identity and store file.
	// ReadClient subscribes to the campfire; WriteClient sends operator messages.
	// SDK handles CF_HOME env and ~/.cf default via config cascade.
	// SDK sync-before-query handles filesystem transport sync automatically.
	readClient, initResult, err := protocol.InitWithConfig()
	if err != nil {
		return fmt.Errorf("protocol.InitWithConfig (read client): %w", err)
	}
	defer readClient.Close() //nolint:errcheck

	// Derive config directory and db path from the resolved store path.
	cfHome := filepath.Dir(initResult.StorePath)
	dbPath := initResult.StorePath

	cfg, err := exchange.LoadConfig(cfHome)
	if err != nil {
		return fmt.Errorf("load config (did you run 'dontguess init'?): %w", err)
	}

	writeClient, _, err := protocol.InitWithConfig()
	if err != nil {
		return fmt.Errorf("protocol.InitWithConfig (write client): %w", err)
	}
	defer writeClient.Close() //nolint:errcheck

	// Open a shared store for the exchange engine (Store field in EngineOptions).
	st, err := store.Open(dbPath)
	if err != nil {
		return fmt.Errorf("opening store %s: %w", dbPath, err)
	}
	defer st.Close()

	// Ensure standard named views exist (idempotent — skips existing).
	viewsCreated, viewErr := exchange.EnsureViews(cfg.ExchangeCampfireID, writeClient)
	if viewErr != nil {
		log.Printf("[exchange] warning: ensuring named views: %v", viewErr)
	} else if viewsCreated > 0 {
		log.Printf("[exchange] created %d missing named views", viewsCreated)
	}

	cs, err := scrip.NewCampfireScripStore(cfg.ExchangeCampfireID, readClient, cfg.OperatorKeyHex)
	if err != nil {
		return fmt.Errorf("creating scrip store: %w", err)
	}

	logDest, err := buildLogDest(cfHome)
	if err != nil {
		return fmt.Errorf("log setup: %w", err)
	}
	logger := log.New(logDest, "[exchange] ", log.LstdFlags|log.Lmsgprefix)

	provCfg := provenance.DefaultConfig()
	provCfg.AllowSelfAttestation = true
	provenanceStore := provenance.NewStore(provCfg)
	// Self-claim the operator and all existing campfire members so they can
	// participate immediately. Anonymous provenance blocks all operations.
	operatorKey := writeClient.PublicKeyHex()
	provenanceStore.TrustVerifier(operatorKey, 0)
	provenanceStore.SetSelfClaimed(operatorKey)
	// Self-attest the operator to reach "present" level — required for
	// match, settle(put-accept/reject/deliver), mint, burn, rate-publish.
	_ = provenanceStore.AddAttestation(&provenance.Attestation{
		TargetKey:   operatorKey,
		VerifierKey: operatorKey,
		VerifiedAt:  time.Now(),
		CoSigned:    true,
	})
	members, _ := writeClient.Members(cfg.ExchangeCampfireID)
	for _, m := range members {
		provenanceStore.SetSelfClaimed(m.MemberPubkey)
	}
	provenanceChecker, err := exchange.NewProvenanceChecker(provenanceStore, cfg.ProvenanceLevels)
	if err != nil {
		return fmt.Errorf("creating provenance checker: %w", err)
	}

	// Use dense embeddings if the embed script is available.
	var embedder matching.Embedder
	embedScript := os.Getenv("DONTGUESS_EMBED_SCRIPT")
	if embedScript == "" {
		embedScript = "/home/baron/projects/dontguess/cmd/embed/main.py"
	}
	if _, err := os.Stat(embedScript); err == nil {
		embedder = matching.NewDenseEmbedder(embedScript)
		logger.Printf("  embedder:  dense (all-MiniLM-L6-v2) via %s", embedScript)
	} else {
		logger.Printf("  embedder:  tf-idf (set DONTGUESS_EMBED_SCRIPT for dense)")
	}

	eng := exchange.NewEngine(exchange.EngineOptions{
		CampfireID:        cfg.ExchangeCampfireID,
		Store:             st,
		ReadClient:        readClient,
		WriteClient:       writeClient,
		Embedder:          embedder,
		PollInterval:      servePollInterval,
		ScripStore:        cs,
		ProvenanceChecker: provenanceChecker,
		Logger: func(format string, args ...any) {
			logger.Printf(format, args...)
		},
	})

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	logger.Printf("exchange serving")
	logger.Printf("  campfire:  %s", cfg.ExchangeCampfireID[:16]+"...")
	logger.Printf("  operator:  %s", cfg.OperatorKeyHex[:16]+"...")
	logger.Printf("  poll:      %s", servePollInterval)
	logger.Printf("  auto-accept: %v (max %d)", serveAutoAccept, serveAutoAcceptMax)
	logger.Printf("  store:     %s", dbPath)
	logger.Printf("  logging to %s + stderr (rotate at 10MB, 5 backups, 28d retention, gzip)", filepath.Join(cfHome, "dontguess.log"))

	fmt.Printf("\n--- Agent connection info ---\n")
	fmt.Printf("EXCHANGE_CAMPFIRE=%s\n", cfg.ExchangeCampfireID)
	fmt.Printf("OPERATOR_KEY=%s\n", cfg.OperatorKeyHex)
	fmt.Printf("\nAgents join with:\n")
	fmt.Printf("  cf join %s\n\n", cfg.ExchangeCampfireID[:16])

	// Auto-accept goroutine.
	// skippedPuts is owned exclusively by this goroutine — no mutex needed.
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

	// Unix socket IPC for operator CLI commands.
	sockPath := filepath.Join(cfHome, "dontguess.sock")
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
// domain socket listener at path. Permissions are set to 0600 (owner-only).
//
// Security (dontguess-33a): we close the TOCTOU window between socket creation
// and chmod by narrowing the umask to 0177 before net.Listen so the kernel
// creates the inode with mode 0600 immediately. syscall.Umask is goroutine-
// global — this is safe here because the operator process is single-threaded
// during startup and this function is called only once. The subsequent Chmod is
// belt-and-suspenders in case the platform ignores the umask for UNIX sockets.
func listenOperatorSocket(path string) (net.Listener, error) {
	// Remove stale socket file if present.
	_ = os.Remove(path)

	// Narrow umask so net.Listen creates the socket at 0600 immediately,
	// closing the TOCTOU window between creation and Chmod.
	oldMask := syscall.Umask(0177) // 0177 → socket lands at 0600
	ln, err := net.Listen("unix", path)
	syscall.Umask(oldMask) // restore regardless of Listen outcome

	if err != nil {
		return nil, err
	}
	// Belt-and-suspenders: some platforms don't honour umask for AF_UNIX.
	if err := os.Chmod(path, 0600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

// operatorRequest is the JSON shape received by the socket server.
type operatorRequest struct {
	Op       string `json:"op"`
	PutMsgID string `json:"put_msg_id,omitempty"`
	Price    int64  `json:"price,omitempty"`
	Expires  string `json:"expires,omitempty"`
	Reason   string `json:"reason,omitempty"`
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
//   (b) A 5-second read deadline prevents a stalled client from holding the
//       goroutine indefinitely.
//   (c) The connection reader is wrapped in an io.LimitReader (1 MiB) before
//       being passed to json.NewDecoder, bounding memory allocation from
//       oversized payloads. All legitimate requests are small JSON objects
//       well under this ceiling.
func handleOperatorConn(conn net.Conn, eng *exchange.Engine) {
	defer conn.Close()

	// (b) Stall protection: abort if no full request arrives within 5 seconds.
	conn.SetDeadline(time.Now().Add(5 * time.Second)) //nolint:errcheck

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
	case "list-held":
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

	case "accept-put":
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
			// Auto-price at 70% of token_cost.
			held := eng.State().PutsHeldForReview()
			for _, e := range held {
				if e.PutMsgID == req.PutMsgID {
					price = e.TokenCost * 70 / 100
					break
				}
			}
		}
		if err := eng.AutoAcceptPut(req.PutMsgID, price, expiresAt); err != nil {
			writeOperatorResp(conn, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeOperatorResp(conn, map[string]any{"ok": true})

	case "reject-put":
		if err := eng.RejectPut(req.PutMsgID, req.Reason); err != nil {
			writeOperatorResp(conn, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeOperatorResp(conn, map[string]any{"ok": true})

	default:
		writeOperatorResp(conn, map[string]any{"ok": false, "error": "unknown op: " + req.Op})
	}
}

func writeOperatorResp(conn net.Conn, v any) {
	json.NewEncoder(conn).Encode(v) //nolint:errcheck
}

// buildLogDest constructs the io.Writer used for the exchange logger.
// Logs go to both stderr (for foreground operation) and a rotating file
// at $dgHome/dontguess.log (10 MB max, 5 backups, 28-day retention, gzip).
// dgHome is resolved from DG_HOME env var, falling back to $HOME/.cf.
//
// Security (dontguess-ba9c): if the target log path is a symlink the function
// returns an error instead of opening the file. Opening a symlink allows an
// attacker who pre-creates a symlink at the log path to redirect operator logs
// into an arbitrary file the process can write (e.g. ~/.ssh/authorized_keys).
// Startup fails fast on this condition — a dangerous config should not be
// silently ignored.
func buildLogDest(dgHome string) (io.Writer, error) {
	if override := os.Getenv("DG_HOME"); override != "" {
		dgHome = override
	}
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

