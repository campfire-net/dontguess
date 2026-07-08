package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/campfire-net/campfire/cf-protocol/protocol"
	"github.com/campfire-net/dontguess/pkg/exchange"
	"github.com/campfire-net/dontguess/pkg/matching"
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
//
// Quality-weighted (M-rebaseline, dontguess-af8):
// A delivered result counts as a HIT only when the top result's similarity
// meets or exceeds the M1 relevance floor (matching.DefaultMinSimilarity()).
// Historical match-results lacking a "similarity" field (pre-M2/dontguess-b26)
// have their similarity recomputed using TF-IDF re-embedding of the buy task
// and the delivered entry's description. Results that cannot be verified are
// reported as UnverifiableHits and excluded from HitRatePct.
var hitRateCmd = &cobra.Command{
	Use:   "hit-rate",
	Short: "Report quality-weighted buy hit-rate reconstructed from exchange match-results",
	Long: `Reconstruct quality-weighted buy hit-rate from the exchange message log.

Reads exchange:buy orders and exchange:match results, joins them by buy order
ID, and classifies each answered buy as a hit (cached inventory delivered AND
similarity ≥ relevance floor) or a miss (buy-miss standing offer, or delivered
result below the similarity floor). Buys with no match-result yet are pending
and excluded from the hit-rate denominator.

  hit_rate = quality-weighted hits / (quality-weighted hits + misses) * 100

The relevance floor is matching.DefaultMinSimilarity() (M1a, dontguess-7d6).
Historical match-results lacking the "similarity" field (produced before
M2/dontguess-b26) have similarity recomputed from the buy task and delivered
entry description using TF-IDF re-embedding.

Synthetic traffic (exchange:synthetic tagged responses) is excluded from all
counts (M3, dontguess-e93).

Flags:
  --since  only count buys and match-results within this window (default: all)
  --json   emit the report as JSON`,
	RunE: runHitRate,
}

func init() {
	hitRateCmd.Flags().DurationVar(&hitRateSince, "since", 0, "time window (0 = all history)")
	rootCmd.AddCommand(hitRateCmd)
}

// parseBuyTask extracts the "task" field from an exchange:buy message payload.
// Returns empty string if the payload cannot be parsed or lacks a task.
func parseBuyTask(m *exchange.Message) string {
	var p struct {
		Task string `json:"task"`
	}
	if err := json.Unmarshal(m.Payload, &p); err != nil {
		return ""
	}
	return p.Task
}

// buildBuyTaskMap builds a map from buy message ID to task text from a slice
// of exchange:buy messages. Used by the quality-weighted hit-rate reporter to
// look up the originating task when recomputing similarity for pre-M2 matches.
func buildBuyTaskMap(buys []exchange.Message) map[string]string {
	m := make(map[string]string, len(buys))
	for i := range buys {
		task := parseBuyTask(&buys[i])
		if task != "" {
			m[buys[i].ID] = task
		}
	}
	return m
}

// buildRecomputeEmbedder creates a TF-IDF embedder primed with the corpus of
// buy task descriptions and delivered entry descriptions extracted from match
// results. This gives the embedder a realistic IDF vocabulary so that recomputed
// similarities are comparable to those computed at match time by the engine.
//
// The corpus covers all task texts from buys plus all entry descriptions from
// delivered match results (those with a "results" array). We include both so
// that rare technical terms (e.g. "FROST", "campfire", "legion") receive the
// correct IDF weight rather than defaulting to 1.0 (neutral).
func buildRecomputeEmbedder(buys []exchange.Message, matches []exchange.Message) *matching.TFIDFEmbedder {
	emb := matching.NewTFIDFEmbedder()

	// Collect corpus documents: buy tasks + entry descriptions from match results.
	corpus := make([]string, 0, len(buys)+len(matches)*3)
	for i := range buys {
		if task := parseBuyTask(&buys[i]); task != "" {
			corpus = append(corpus, task)
		}
	}
	for i := range matches {
		var p struct {
			Results []struct {
				Description string `json:"description"`
			} `json:"results"`
		}
		if err := json.Unmarshal(matches[i].Payload, &p); err != nil {
			continue
		}
		for _, r := range p.Results {
			if r.Description != "" {
				corpus = append(corpus, r.Description)
			}
		}
	}

	emb.IndexCorpus(corpus)
	return emb
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

	buys, err := readFilter(client, cfID, buysFilter(cutoffNano))
	if err != nil {
		return fmt.Errorf("read buys: %w", err)
	}
	// Match-results carry the exchange:match tag (both hits and misses).
	matches, err := readFilter(client, cfID, matchesFilter(cutoffNano))
	if err != nil {
		return fmt.Errorf("read match-results: %w", err)
	}
	// Consume signals carry the exchange:consume tag (emitted on settle-complete).
	// Used for net-savings economics: a hit is "real" only when the entry was consumed.
	consumes, err := readFilter(client, cfID, consumesFilter(cutoffNano))
	if err != nil {
		// Non-fatal: log and continue with consume unavailable (all hits treated as real).
		fmt.Fprintf(os.Stderr, "warning: could not read consume signals: %v\n", err)
		consumes = nil
	}

	// Build cross-agent convergence map from full exchange history (dontguess-412).
	// Replay all messages (unfiltered by --since) into a fresh State to accumulate
	// EntryBuyerMap across the full history — convergence is a cumulative property,
	// not a windowed one. AfterTimestamp=0 fetches all messages in log order.
	convergenceMap, err := buildConvergenceMapFromClient(client, cfID, cfg.OperatorKeyHex)
	if err != nil {
		// Non-fatal: log and continue with zero convergence rather than failing.
		fmt.Fprintf(os.Stderr, "warning: could not build convergence map: %v\n", err)
		convergenceMap = nil
	}

	// Build quality-weighted options (M-rebaseline, dontguess-af8).
	// MinSimilarity references the M1 floor constant — does NOT hardcode 0.16.
	// Embedder is a TF-IDF instance primed with the full corpus for recompute.
	// ConsumeCountByEntry provides the consume signal for net-savings economics (Track C).
	opts := exchange.HitRateOptions{
		MinSimilarity:       matching.DefaultMinSimilarity(),
		Embedder:            buildRecomputeEmbedder(buys, matches),
		BuyTasks:            buildBuyTaskMap(buys),
		EntryBuyerMap:       convergenceMap,
		ConsumeCountByEntry: exchange.ConsumeCountByEntry(consumes),
	}

	rep := exchange.ComputeHitRate(buys, matches, opts)
	printHitRate(rep, hitRateSince, jsonOutput)
	return nil
}

// buildConvergenceMapFromClient reads all exchange messages (full history, no
// time filter), replays them into a fresh State, and returns the merged
// EntryBuyerMap via BuildConvergenceMap. This is unfiltered by --since because
// cross-agent convergence is a cumulative property of the inventory lifecycle,
// not a windowed metric. The operator key is required to construct the State.
func buildConvergenceMapFromClient(client *protocol.Client, cfID, operatorKeyHex string) (map[string]map[string]struct{}, error) {
	result, err := client.Read(protocol.ReadRequest{
		CampfireID:     cfID,
		AfterTimestamp: 0,
	})
	if err != nil {
		return nil, fmt.Errorf("read all messages: %w", err)
	}
	msgs := exchange.FromSDKMessages(result.Messages)
	st := exchange.NewState()
	st.OperatorKey = operatorKeyHex
	st.Replay(msgs)
	return exchange.BuildConvergenceMap(st), nil
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
	fmt.Printf("  hits (quality):    %d\n", rep.Hits)
	fmt.Printf("  misses:            %d\n", rep.Misses)
	fmt.Printf("  QUALITY HIT RATE:  %.2f%%  (similarity≥floor hits / verified answered)\n", rep.HitRatePct)
	fmt.Println()
	fmt.Printf("  below-floor downgraded: %d  (delivered but similarity < %.2f)\n",
		rep.BelowFloorDowngraded, matching.DefaultMinSimilarity())
	fmt.Printf("  similarity recomputed:  %d  (pre-M2 historical, TF-IDF recomputed)\n", rep.RecomputedSimilarity)
	fmt.Printf("  unverifiable hits:      %d  (no similarity field, no recompute path)\n", rep.UnverifiableHits)
	fmt.Println()
	fmt.Printf("  synthetic excluded:     %d\n", rep.SyntheticExcluded)
	fmt.Printf("  match-results read: %d\n", rep.MatchResultsTotal)
	fmt.Printf("  unjoinable:         %d  (match-result with no buy in window)\n", rep.UnjoinableMatchResults)
	fmt.Println()
	fmt.Printf("  cross-agent convergence: %d  (inventory entries bought by 3+ distinct agent keys)\n", rep.CrossAgentConvergence)
	fmt.Println()
	fmt.Printf("=== net token savings (Track C, dontguess-eff) ===\n\n")
	fmt.Printf("  NET TOKENS SAVED:       %+d  (saved_on_hits − miss_costs − false_positive_waste)\n", rep.NetTokensSaved)
	fmt.Printf("  saved on real hits:     %d\n", rep.SavedOnRealHits)
	// Derive the actual per-miss cost used during computation from the report.
	// TotalMissCost = Misses × missCostPerQuery; recover the rate rather than
	// hardcoding DefaultMissCostPerQuery (which would be wrong when opts.MissCostPerQuery
	// is set to a non-default value by the caller).
	var actualMissCost int64 = exchange.DefaultMissCostPerQuery
	if rep.Misses > 0 {
		actualMissCost = rep.TotalMissCost / int64(rep.Misses)
	}
	fmt.Printf("  miss overhead:          %d  (%d misses × ~%d tokens/miss)\n",
		rep.TotalMissCost, rep.Misses, actualMissCost)
	fmt.Printf("  false-positive waste:   %d  (delivered+unconsumed entries re-derived)\n", rep.TotalFalsePositiveWaste)
	if len(rep.PerQueryEconomics) > 0 {
		fmt.Println()
		fmt.Printf("  per-query economics (%d answered):\n", len(rep.PerQueryEconomics))
		for _, q := range rep.PerQueryEconomics {
			marker := " "
			if q.Saved > 0 {
				marker = "+"
			} else if q.Saved < 0 {
				marker = "-"
			}
			fmt.Printf("    [%s] %-16s %s%d tokens  (entry token_cost=%d)\n",
				q.Outcome, q.BuyID, marker, abs64(q.Saved), q.TokenCostOriginal)
		}
	}
}

// abs64 returns the absolute value of a int64.
func abs64(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}
