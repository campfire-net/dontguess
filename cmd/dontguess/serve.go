package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/campfire-net/dontguess/pkg/exchange"
	"github.com/campfire-net/dontguess/pkg/matching"
	"github.com/campfire-net/dontguess/pkg/scrip"
	"github.com/campfire-net/campfire/pkg/protocol"
	"github.com/campfire-net/campfire/pkg/provenance"
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

	logger := log.New(os.Stderr, "[exchange] ", log.LstdFlags|log.Lmsgprefix)

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

	if err := eng.Start(ctx); err != nil && ctx.Err() == nil {
		return fmt.Errorf("engine error: %w", err)
	}

	logger.Printf("exchange shut down")
	return nil
}

