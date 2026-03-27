// Package main is the dontguess CLI entry point.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "dontguess",
	Short: "DontGuess — token-work exchange operator CLI",
	Long: `dontguess — operator CLI for the DontGuess token-work exchange.

The exchange runs as a campfire application: all exchange state (inventory,
orders, matches, settlements) is derived from the message log.

  dontguess convention supersede  publish a new convention version via registry supersede`,
}

var jsonOutput bool

func init() {
	rootCmd.PersistentFlags().BoolVar(&jsonOutput, "json", false, "output as JSON")
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
