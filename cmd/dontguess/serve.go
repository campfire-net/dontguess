package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"time"

	"github.com/3dl-dev/dontguess/pkg/exchange"
	"github.com/3dl-dev/dontguess/pkg/scrip"
	"github.com/campfire-net/campfire/pkg/protocol"
	"github.com/campfire-net/campfire/pkg/store"
	"github.com/spf13/cobra"
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
	serveCmd.Flags().Int64Var(&serveAutoAcceptMax, "auto-accept-max-price", 100000, "maximum token cost to auto-accept")
	rootCmd.AddCommand(serveCmd)
}

func runServe(_ *cobra.Command, _ []string) error {
	cfHome := os.Getenv("CF_HOME")
	if cfHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("resolving home dir: %w", err)
		}
		cfHome = home + "/.campfire"
	}

	cfg, err := exchange.LoadConfig(cfHome)
	if err != nil {
		return fmt.Errorf("load config (did you run 'dontguess init'?): %w", err)
	}

	// Build two clients via protocol.Init — both share the same identity and store file.
	// ReadClient subscribes to the campfire; WriteClient sends operator messages.
	// SDK sync-before-query handles filesystem transport sync automatically.
	readClient, err := protocol.Init(cfHome)
	if err != nil {
		return fmt.Errorf("protocol.Init (read client): %w", err)
	}
	defer readClient.Close() //nolint:errcheck

	writeClient, err := protocol.Init(cfHome)
	if err != nil {
		return fmt.Errorf("protocol.Init (write client): %w", err)
	}
	defer writeClient.Close() //nolint:errcheck

	// Open a shared store for components that require store.Store directly
	// (CampfireScripStore). Uses the same path as protocol.Init.
	dbPath := store.StorePath(cfHome)
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

	cs, err := scrip.NewCampfireScripStore(cfg.ExchangeCampfireID, st, cfg.OperatorKeyHex)
	if err != nil {
		return fmt.Errorf("creating scrip store: %w", err)
	}

	logger := log.New(os.Stderr, "[exchange] ", log.LstdFlags|log.Lmsgprefix)

	eng := exchange.NewEngine(exchange.EngineOptions{
		CampfireID:   cfg.ExchangeCampfireID,
		Store:        st,
		ReadClient:   readClient,
		WriteClient:  writeClient,
		PollInterval: servePollInterval,
		ScripStore:   cs,
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

	fmt.Printf("\n--- Agent connection info ---\n")
	fmt.Printf("EXCHANGE_CAMPFIRE=%s\n", cfg.ExchangeCampfireID)
	fmt.Printf("OPERATOR_KEY=%s\n", cfg.OperatorKeyHex)
	fmt.Printf("\nAgents join with:\n")
	fmt.Printf("  cf join %s\n\n", cfg.ExchangeCampfireID[:16])

	// Auto-accept goroutine.
	if serveAutoAccept {
		go func() {
			ticker := time.NewTicker(1 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					autoAcceptPuts(eng, logger)
				}
			}
		}()
	}

	if err := eng.Start(ctx); err != nil && ctx.Err() == nil {
		return fmt.Errorf("engine error: %w", err)
	}

	logger.Printf("exchange shut down")
	return nil
}

func autoAcceptPuts(eng *exchange.Engine, logger *log.Logger) {
	pending := eng.State().PendingPuts()
	for _, entry := range pending {
		if entry.TokenCost > serveAutoAcceptMax {
			logger.Printf("skipping put %s: token cost %d > max %d",
				entry.PutMsgID[:8], entry.TokenCost, serveAutoAcceptMax)
			continue
		}
		price := entry.TokenCost * 70 / 100
		expires := time.Now().Add(72 * time.Hour)
		if err := eng.AutoAcceptPut(entry.PutMsgID, price, expires); err != nil {
			logger.Printf("auto-accept put %s failed: %v", entry.PutMsgID[:8], err)
		} else {
			logger.Printf("auto-accepted put %s: price=%d (token_cost=%d)",
				entry.PutMsgID[:8], price, entry.TokenCost)
		}
	}
}
