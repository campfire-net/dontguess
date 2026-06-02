package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/campfire-net/campfire/cf-protocol/protocol"
	"github.com/campfire-net/dontguess/pkg/exchange"
	"github.com/spf13/cobra"
)

var hitRateSince time.Duration

// hitRateCmd reports buy hit-rate reconstructed exchange-side.
//
// Buy matching is asynchronous — `dontguess buy` returns "dispatched" before any
// match exists, so hit-vs-miss is not knowable at the wrapper. This command
// reconstructs it from the exchange message log by reading buy orders and
// match-results and joining them (match antecedent[0] = buy order ID), then
// classifying each match as a HIT (delivered inventory results) or a MISS
// (buy-miss standing offer). See pkg/exchange/hitrate.go.
var hitRateCmd = &cobra.Command{
	Use:   "hit-rate",
	Short: "Report buy hit-rate reconstructed from exchange match-results",
	Long: `Reconstruct buy hit-rate from the exchange message log.

Reads exchange:buy orders and exchange:match results, joins them by buy order
ID, and classifies each answered buy as a hit (cached inventory delivered) or a
miss (a buy-miss standing offer — "No cached inference matched"). Buys with no
match-result yet are pending and excluded from the hit-rate denominator.

  hit_rate = hits / (hits + misses) * 100

Flags:
  --since  only count buys and match-results within this window (default: all)
  --json   emit the report as JSON`,
	RunE: runHitRate,
}

func init() {
	hitRateCmd.Flags().DurationVar(&hitRateSince, "since", 0, "time window (0 = all history)")
	rootCmd.AddCommand(hitRateCmd)
}

// readTaggedMessages reads all messages with the given tag from the exchange
// campfire and returns them as exchange.Message, filtered to those at or after
// cutoffNano (cutoffNano <= 0 means no time filter).
func readTaggedMessages(client *protocol.Client, cfID, tag string, cutoffNano int64) ([]exchange.Message, error) {
	result, err := client.Read(protocol.ReadRequest{
		CampfireID: cfID,
		Tags:       []string{tag},
	})
	if err != nil {
		return nil, err
	}
	out := make([]exchange.Message, 0, len(result.Messages))
	for i := range result.Messages {
		m := result.Messages[i]
		if cutoffNano > 0 && m.Timestamp < cutoffNano {
			continue
		}
		out = append(out, *exchange.FromSDKMessage(&m))
	}
	return out, nil
}

func runHitRate(_ *cobra.Command, _ []string) error {
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
	if hitRateSince > 0 {
		cutoffNano = time.Now().Add(-hitRateSince).UnixNano()
	}
	cfID := cfg.ExchangeCampfireID

	buys, err := readTaggedMessages(client, cfID, exchange.TagBuy, cutoffNano)
	if err != nil {
		return fmt.Errorf("read buys: %w", err)
	}
	// Match-results carry the exchange:match tag (both hits and misses).
	matches, err := readTaggedMessages(client, cfID, exchange.TagMatch, cutoffNano)
	if err != nil {
		return fmt.Errorf("read match-results: %w", err)
	}

	rep := exchange.ComputeHitRate(buys, matches)
	printHitRate(rep, hitRateSince, jsonOutput)
	return nil
}

func printHitRate(rep exchange.HitRateReport, since time.Duration, asJSON bool) {
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(rep) //nolint:errcheck
		return
	}

	window := "all history"
	if since > 0 {
		window = "last " + since.String()
	}
	fmt.Printf("=== dontguess buy hit-rate (%s) ===\n\n", window)
	fmt.Printf("  total buys:        %d\n", rep.TotalBuys)
	fmt.Printf("  answered (matched): %d\n", rep.MatchedBuys)
	fmt.Printf("  pending (no match): %d\n", rep.PendingBuys)
	fmt.Printf("  hits:              %d\n", rep.Hits)
	fmt.Printf("  misses:            %d\n", rep.Misses)
	fmt.Printf("  HIT RATE:          %.2f%%  (hits / answered)\n", rep.HitRatePct)
	fmt.Println()
	fmt.Printf("  match-results read: %d\n", rep.MatchResultsTotal)
	fmt.Printf("  unjoinable:         %d  (match-result with no buy in window)\n", rep.UnjoinableMatchResults)
}
