package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/campfire-net/campfire/cf-protocol/protocol"
	"github.com/campfire-net/dontguess/pkg/demand"
	"github.com/campfire-net/dontguess/pkg/exchange"
	"github.com/spf13/cobra"
)

// demandSince controls the time window for the demand command.
var demandSince time.Duration

// demandCmd reports the clustered demand backlog from the exchange miss log.
//
// It reads all exchange:buy-miss messages from the exchange campfire (read-only),
// excludes synthetic traffic (regression-*, timeout-178*, test-class), clusters
// the remaining real misses by theme, and prints the assignable work queue.
//
// Each item in the backlog represents unmet demand: a buyer described a task,
// no cached inference matched, and the exchange posted a 70%-rate standing offer.
// Computing the result and putting it to the exchange fills the offer and earns
// 70% of token_cost in scrip.
//
// The miss log is read READ-ONLY. This command never writes to the exchange.
var demandCmd = &cobra.Command{
	Use:   "demand",
	Short: "Show clustered demand backlog from the miss log",
	Long: `Show the clustered demand backlog from exchange buy-miss messages.

Reads exchange:buy-miss messages from the exchange miss log (read-only), strips
synthetic load-test traffic (regression-*, timeout-178*, "test"-class), and
clusters the remaining real misses by theme:

  campfire    — campfire SDK, convention protocol, subscribe cursor
  audit       — test coverage gaps, missing error paths, edge case audits
  convention  — exchange convention declarations, supersede, revoke
  review      — RPT/code/design reviews
  security    — FROST threshold, auth gates, cryptography
  test-gap    — test strategy, test-gap scans
  other       — uncategorized real misses

Each backlog item shows the task description and the 70% standing offer rate:
put a matching result to fill the offer and earn 70% of token_cost in scrip.

  --since   only count misses within this window (e.g. 24h, 168h)
  --json    emit the full backlog as JSON`,
	RunE: runDemand,
}

func init() {
	demandCmd.Flags().DurationVar(&demandSince, "since", 0, "time window (0 = all history)")
	rootCmd.AddCommand(demandCmd)
}

func runDemand(_ *cobra.Command, _ []string) error {
	dgHome := resolveDGHome()

	cfg, err := exchange.LoadConfig(dgHome)
	if err != nil {
		return fmt.Errorf("load exchange config: %w", err)
	}

	client, _, err := protocol.Init(dgHome)
	if err != nil {
		return fmt.Errorf("protocol.Init: %w", err)
	}
	defer client.Close()

	var cutoffNano int64
	if demandSince > 0 {
		cutoffNano = time.Now().Add(-demandSince).UnixNano()
	}

	// Read all exchange:buy-miss messages from the campfire (read-only).
	// buy-miss messages carry both TagBuyMiss and TagMatch tags; query by TagBuyMiss
	// to get only misses (not the hit-match messages).
	rawMisses, err := readTaggedMessages(client, cfg.ExchangeCampfireID, exchange.TagBuyMiss, cutoffNano)
	if err != nil {
		return fmt.Errorf("read miss log: %w", err)
	}

	// Convert exchange.Message to demand.MissMessage.
	msgs := make([]demand.MissMessage, len(rawMisses))
	for i, m := range rawMisses {
		msgs[i] = demand.MissMessage{
			ID:        m.ID,
			Payload:   m.Payload,
			Timestamp: m.Timestamp,
		}
	}

	bl := demand.BuildBacklog(msgs)

	if jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(bl)
	}

	printDemandBacklog(bl, demandSince)
	return nil
}

func printDemandBacklog(bl demand.Backlog, since time.Duration) {
	window := "all history"
	if since > 0 {
		window = "last " + since.String()
	}

	fmt.Printf("=== dontguess demand backlog (%s) ===\n\n", window)
	fmt.Printf("  total misses:       %d\n", bl.TotalMisses)
	fmt.Printf("  synthetic excluded: %d\n", bl.SyntheticExcluded)
	fmt.Printf("  real misses:        %d\n", bl.RealMisses)
	fmt.Println()

	if bl.RealMisses == 0 {
		fmt.Println("  No real misses in the backlog.")
		return
	}

	fmt.Printf("  Clusters (sorted by demand):\n\n")

	for _, c := range bl.Clusters {
		fmt.Printf("  [%s] — %d task(s)\n", c.Name, c.Count)
		fmt.Printf("  %-10s  %-6s  %s\n", "miss_id", "offer%", "task")
		fmt.Printf("  %-10s  %-6s  %s\n", "-------", "------", "----")
		for _, item := range c.Items {
			missShort := item.MissID
			if len(missShort) > 12 {
				missShort = missShort[:12]
			}
			task := item.Task
			if len(task) > 80 {
				task = task[:77] + "..."
			}
			fmt.Printf("  %-12s  %5d%%  %s\n", missShort, item.OfferedPriceRate, task)
		}
		fmt.Println()
	}

	fmt.Printf("  To fill a standing offer: dontguess put --description \"<task>\" \\\n")
	fmt.Printf("    --token_cost <n> --content_type exchange:content-type:code --content <base64>\n")
}
