package main

import (
	"github.com/spf13/cobra"
)

var conventionCmd = &cobra.Command{
	Use:   "convention",
	Short: "Convention lifecycle tools: lint, supersede",
	Long: `Convention lifecycle tools for the DontGuess exchange convention.

  dontguess convention supersede  publish a new version via cf registry supersede`,
}

func init() {
	rootCmd.AddCommand(conventionCmd)
}
