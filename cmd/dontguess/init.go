package main

import (
	"encoding/json"
	"fmt"
	"os"

	dontguess "github.com/campfire-net/dontguess"
	"github.com/campfire-net/dontguess/pkg/exchange"
	"github.com/spf13/cobra"
)

var (
	initConventionDir string
	initAlias         string
	initDescription   string
	initForce         bool
)

var initCmd = &cobra.Command{
	Use:   "init",
	Short: "Initialize an exchange campfire",
	Long: `Create an exchange campfire, promote convention declarations to its
registry, register in the naming hierarchy, and publish a discovery beacon.

If an exchange config already exists at ~/.campfire/dontguess-exchange.json,
this command is a no-op unless --force is given.

  dontguess init
  dontguess init --alias home.exchange.dontguess --force`,
	RunE: runInit,
}

func init() {
	initCmd.Flags().StringVar(&initConventionDir, "convention-dir", "", "path to convention declarations directory (must contain exchange-core/ and exchange-scrip/)")
	initCmd.Flags().StringVar(&initAlias, "alias", "", "naming alias for the exchange (default: exchange.dontguess)")
	initCmd.Flags().StringVar(&initDescription, "description", "", "exchange description for beacon")
	initCmd.Flags().BoolVar(&initForce, "force", false, "reinitialize even if config exists")
	rootCmd.AddCommand(initCmd)
}

func runInit(_ *cobra.Command, _ []string) error {
	opts := exchange.InitOptions{
		ConventionDir:       initConventionDir,
		EmbeddedConventions: dontguess.ConventionFS,
		Alias:               initAlias,
		Description:         initDescription,
		Force:               initForce,
	}

	cfg, client, err := exchange.Init(opts)
	if err != nil {
		return fmt.Errorf("init failed: %w", err)
	}
	defer client.Close()

	if jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(cfg)
	}

	fmt.Printf("Exchange initialized\n")
	fmt.Printf("  campfire: %s\n", cfg.ExchangeCampfireID)
	fmt.Printf("  operator: %s\n", cfg.OperatorKeyHex)
	fmt.Printf("  alias:    %s\n", cfg.Alias)
	fmt.Printf("  version:  %s\n", cfg.ConventionVersion)
	fmt.Printf("\nNext: cf join %s...\n", cfg.ExchangeCampfireID[:16])
	fmt.Printf("      cf %s put --help\n", cfg.Alias)
	return nil
}
