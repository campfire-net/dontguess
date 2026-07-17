package main

import (
	"fmt"
	"os"

	"github.com/3dl-dev/dontguess/pkg/nativebert"
	"github.com/spf13/cobra"
)

// embedCmd groups embedding-model maintenance subcommands.
var embedCmd = &cobra.Command{
	Use:   "embed",
	Short: "Manage the local semantic-embedding model",
	Long: `Manage the pure-Go all-MiniLM-L6-v2 embedding model used for semantic
matching. The model runs entirely in Go — no python, no onnxruntime.

'dontguess serve' uses dense embeddings only when the model is already cached;
run 'dontguess embed pull' once to fetch it and enable native semantic matching.`,
}

// embedPullCmd downloads the model into the local cache. This is the explicit,
// blocking fetch kept out of the serve hot path so serve never blocks its
// operator socket on a ~87 MB download.
var embedPullCmd = &cobra.Command{
	Use:   "pull",
	Short: "Download the all-MiniLM-L6-v2 model into the local cache",
	RunE: func(cmd *cobra.Command, args []string) error {
		cacheDir := os.Getenv("DONTGUESS_EMBED_CACHE")
		target := cacheDir
		if target == "" {
			target = nativebert.DefaultCacheDir()
		}
		if nativebert.Cached(cacheDir) {
			fmt.Printf("model already cached in %s\n", target)
			return nil
		}
		fmt.Printf("downloading all-MiniLM-L6-v2 (~87 MB) to %s ...\n", target)
		if err := nativebert.Fetch(cacheDir); err != nil {
			return fmt.Errorf("embed pull: %w", err)
		}
		fmt.Println("done — `dontguess serve` will now use native dense embeddings")
		return nil
	},
}

func init() {
	embedCmd.AddCommand(embedPullCmd)
	rootCmd.AddCommand(embedCmd)
}
