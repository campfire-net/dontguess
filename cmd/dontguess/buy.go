package main

// buy.go is item ed2-B: the team-tier `dontguess buy` cobra command. It wires
// pkg/relayclient's buy-await protocol to the CLI — sign with the AGENT key
// (never the operator key), subscribe-first, publish the buy direct to the
// relay, and await a discriminated outcome (match / buy-miss / leaked-assign /
// ambiguous-timeout) on a bounded, re-subscribing ctx (design
// docs/design/nostr-first-client-ed2.md §3.2).
//
// SCOPE: on the TEAM tier this command SURFACES a real match (entry id, price,
// seller) and stops at the seam ed2-C extends — it does NOT yet drive
// buyer-accept -> deliver -> complete (that pulls full content and moves scrip).
// On the INDIVIDUAL tier (zero relay, socket IPC to a local serve — item ed2-E,
// dontguess-2b4) DONTGUESS_RELAY_URLS is unset and runBuy routes through
// runBuyIndividual, which returns the matched content INLINE in one round trip
// (no scrip, no settle chain — the engine is the sole trusted local process).

import (
	"context"
	"fmt"

	"github.com/3dl-dev/dontguess/pkg/exchange"
	"github.com/3dl-dev/dontguess/pkg/identity"
	"github.com/3dl-dev/dontguess/pkg/relayclient"
	"github.com/spf13/cobra"
)

// newBuyerBlobStore resolves the buyer-side BlobStore that pkg/relayclient's
// settle chain (dontguess-250) uses to fetch >32 KiB encrypted content offloaded
// to Blossom (dontguess-640). Overridable in tests so an E2E can point the CLI
// at an in-process blob backend. Default: an HTTP Blossom client rooted at
// DONTGUESS_BLOSSOM_URL.
//
// Fail-open on ABSENCE: when DONTGUESS_BLOSSOM_URL is unset this returns a TRUE
// nil interface (never a typed-nil), so the ≤32 KiB inline path is unchanged and
// an oversize blob_pointer deliver LOUD-fails through settle.go's existing
// nil-store guard rather than silently passing.
var newBuyerBlobStore = defaultBuyerBlobStore

func defaultBuyerBlobStore() exchange.BlobStore {
	// Single source of the Blossom transport (blobstore_env.go, dontguess-0fd) —
	// the seller (put.go) and operator (serve.go) resolve the same env var, so an
	// identical ciphertext converges on one content-addressed pointer everywhere.
	return blobStoreFromEnv()
}

// newBuyCmd builds the buy cobra command. Extracted from init() so tests can
// construct an isolated instance per case rather than mutating the package-level
// singleton's flag state.
func newBuyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "buy",
		Short: "Buy cached inference from the team exchange (await a match)",
		Long: `buy publishes an exchange:buy event directly to the team relay, signed with
the AGENT key from AGENT_CF_HOME (never the operator key), after SUBSCRIBING
FIRST for the operator's response so a fast match cannot be missed.

It then awaits a discriminated outcome within a bounded timeout:
  - MATCH      a hit — surfaces entry id, price, seller (settle is ed2-C)
  - BUY-MISS   nobody has this yet — prints the demand-signal guide
  - AMBIGUOUS  timed out — enumerates the actionable causes (NEVER "no cache")

Requires DONTGUESS_RELAY_URLS (team tier). Individual tier (no relay) is not
yet wired to this command.`,
		RunE: runBuy,
	}
	cmd.Flags().String("task", "", "task description — what you need (required)")
	cmd.Flags().Int64("budget", 0, "scrip budget you are willing to spend")
	cmd.Flags().String("content_type", "", "full exchange content-type tag filter (optional)")
	cmd.Flags().StringSlice("domains", nil, "domain tags to filter on (comma-separated)")
	cmd.Flags().Int("min_reputation", 0, "minimum seller reputation")
	cmd.Flags().Int("freshness_hours", 0, "max age of cached content in hours (0 = any)")
	cmd.Flags().Int("max_results", 0, "max ranked results to return (0 = operator default)")
	cmd.Flags().String("relay", "", "relay websocket URL (default: first of DONTGUESS_RELAY_URLS)")
	cmd.Flags().String("operator-npub", "", "operator npub to require as response author (optional, belt-and-suspenders)")
	cmd.Flags().Duration("timeout", relayclient.DefaultBuyTimeout, "bounded end-to-end timeout for the whole buy->settle chain (dial, subscribe, publish, await)")
	cmd.Flags().Bool("relay-auth", false, "opt into the NIP-42 client AUTH handshake (default: WithoutClientAuth)")
	cmd.Flags().Bool("preview", false, "on a hit, request a FREE content preview before buyer-accept (preview-request -> preview -> buyer-accept)")
	return cmd
}

var buyCmd = newBuyCmd()

func init() {
	rootCmd.AddCommand(buyCmd)
}

func runBuy(cmd *cobra.Command, args []string) error {
	task, _ := cmd.Flags().GetString("task")
	budget, _ := cmd.Flags().GetInt64("budget")
	contentType, _ := cmd.Flags().GetString("content_type")
	domains, _ := cmd.Flags().GetStringSlice("domains")
	minRep, _ := cmd.Flags().GetInt("min_reputation")
	freshness, _ := cmd.Flags().GetInt("freshness_hours")
	maxResults, _ := cmd.Flags().GetInt("max_results")
	relayURL, _ := cmd.Flags().GetString("relay")
	operatorNpub, _ := cmd.Flags().GetString("operator-npub")
	timeout, _ := cmd.Flags().GetDuration("timeout")
	relayAuth, _ := cmd.Flags().GetBool("relay-auth")
	preview, _ := cmd.Flags().GetBool("preview")

	if task == "" {
		return fmt.Errorf("buy: --task is required")
	}
	if budget < 0 {
		return fmt.Errorf("buy: --budget must be non-negative")
	}

	if relayURL == "" {
		urls := resolveRelayURLs()
		if len(urls) == 0 {
			// Individual tier (design §3.3, ed2-E, dontguess-2b4): zero relay,
			// zero identity ceremony — route through the already-running `serve`
			// over the operator unix socket, which returns matched content inline.
			return runBuyIndividual(cmd, task, budget, contentType, domains, maxResults)
		}
		relayURL = urls[0]
	}

	var operatorPubKey string
	if operatorNpub != "" {
		raw, err := identity.DecodeNpub(operatorNpub)
		if err != nil {
			return fmt.Errorf("buy: --operator-npub is not a valid npub: %w", err)
		}
		operatorPubKey = fmt.Sprintf("%x", raw)
	}

	signer, err := loadAgentSigner()
	if err != nil {
		return fmt.Errorf("buy: %w", err)
	}

	conn := relayclient.NewConn(relayURL, signer, relayclient.WithRelayAuth(relayAuth))
	defer conn.Close()

	base := cmd.Context()
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithTimeout(base, timeout)
	defer cancel()

	result, err := relayclient.Buy(ctx, conn, signer, relayclient.BuyRequest{
		Task:           task,
		Budget:         budget,
		ContentType:    contentType,
		Domains:        domains,
		MinReputation:  minRep,
		FreshnessHours: freshness,
		MaxResults:     maxResults,
		OperatorPubKey: operatorPubKey,
	})
	if err != nil {
		return fmt.Errorf("buy failed: %w", err)
	}

	// Non-hit outcomes carry no content: render the LOUD outcome to stderr (so
	// stdout stays a clean, pipeable content channel) and exit non-zero so a
	// wrapping script/agent can branch on the exit code.
	if result.Outcome != relayclient.BuyOutcomeMatch {
		relayclient.WriteOutcome(cmd.ErrOrStderr(), result)
		switch result.Outcome {
		case relayclient.BuyOutcomeMiss:
			return fmt.Errorf("buy %s: no match (buy-miss) — see demand-signal guide above", result.BuyID)
		case relayclient.BuyOutcomeBrokered:
			return fmt.Errorf("buy %s: unexpected brokered assign — not settling", result.BuyID)
		default:
			return fmt.Errorf("buy %s: ambiguous timeout — see enumerated causes above", result.BuyID)
		}
	}

	// HIT (ed2-C): drive the per-phase settle chain over the SAME conn under the
	// SAME identity to move scrip + receive content in this one invocation. The
	// match summary and settle diagnostics go to stderr; only the verified content
	// goes to stdout (pipeable).
	relayclient.WriteOutcome(cmd.ErrOrStderr(), result)
	settleRes, serr := relayclient.Settle(ctx, conn, signer, result, relayclient.SettleOptions{
		Budget:         budget,
		Preview:        preview,
		OperatorPubKey: operatorPubKey,
		// Wire the buyer-side Blossom client so an oversize (>32 KiB) deliver that
		// references its ciphertext as a blob_pointer is fetchable from the CLI
		// (dontguess-250/640). nil (no DONTGUESS_BLOSSOM_URL) leaves the ≤32 KiB
		// inline path untouched; an oversize deliver then LOUD-fails in settle.go.
		BlobStore: newBuyerBlobStore(),
	})
	if serr != nil {
		return fmt.Errorf("buy %s: settle: %w", result.BuyID, serr)
	}
	relayclient.WriteSettleOutcome(cmd.ErrOrStderr(), result.BuyID, settleRes)

	switch settleRes.Outcome {
	case relayclient.SettleOutcomeSettled:
		if _, werr := cmd.OutOrStdout().Write(settleRes.Content); werr != nil {
			return fmt.Errorf("buy %s: writing delivered content: %w", result.BuyID, werr)
		}
		return nil
	case relayclient.SettleOutcomeUnderfunded:
		return fmt.Errorf("buy %s: underfunded — operator rejected buyer-accept (%s); ask the operator to run: dontguess mint <your-npub> <amount>",
			result.BuyID, settleRes.RejectReason)
	case relayclient.SettleOutcomeBudgetExceeded:
		return fmt.Errorf("buy %s: match price %d scrip exceeds budget %d — not purchased", result.BuyID, settleRes.Price, budget)
	default:
		return fmt.Errorf("buy %s: settle ambiguous timeout — matched but content was not delivered in the bound", result.BuyID)
	}
}

// runBuyIndividual is the individual-tier (zero-relay) buy path (design §3.3,
// item ed2-E, dontguess-2b4): no agent signing key, no relay dial — it routes
// the buy through the already-running `dontguess serve` over the operator unix
// socket via relayclient.SocketTransport. The server ingests+dispatches the buy
// (localMu-guarded) and blocks server-side for the match, returning the matched
// content INLINE. On a HIT the raw content is written to stdout (pipeable) and a
// one-line summary to stderr; a MISS and an (unexpected) TIMEOUT each exit
// non-zero with a distinct, LOUD message (a timeout is NEVER "no cache exists",
// design §5.4). No scrip, no settle chain.
func runBuyIndividual(cmd *cobra.Command, task string, budget int64, contentType string, domains []string, maxResults int) error {
	t := relayclient.NewSocketTransport(socketPath())
	result, err := t.Buy(relayclient.SocketBuyRequest{
		Task:        task,
		Budget:      budget,
		ContentType: contentType,
		Domains:     domains,
		MaxResults:  maxResults,
	})
	if err != nil {
		return fmt.Errorf("buy failed: %w", err)
	}

	switch {
	case result.Matched:
		fmt.Fprintf(cmd.ErrOrStderr(), "buy %s HIT (individual tier): entry=%s content_type=%s token_cost=%d (%d bytes)\n",
			result.BuyID, result.EntryID, result.ContentType, result.TokenCost, len(result.Content))
		if _, werr := cmd.OutOrStdout().Write(result.Content); werr != nil {
			return fmt.Errorf("buy %s: writing matched content: %w", result.BuyID, werr)
		}
		return nil
	case result.Miss:
		fmt.Fprintf(cmd.ErrOrStderr(), "buy %s BUY-MISS (individual tier): nobody has cached %q yet — compute it, then `dontguess put` so the next agent hits.\n", result.BuyID, task)
		return fmt.Errorf("buy %s: no match (buy-miss)", result.BuyID)
	case result.TimedOut:
		fmt.Fprintf(cmd.ErrOrStderr(), "buy %s AMBIGUOUS (individual tier): the serve engine did not resolve the buy within the bounded window. This is NOT \"no cache exists\" — the engine may be stalled or overloaded. Check `dontguess serve` is healthy and retry.\n", result.BuyID)
		return fmt.Errorf("buy %s: ambiguous timeout", result.BuyID)
	default:
		return fmt.Errorf("buy %s: unrecognized individual-tier outcome", result.BuyID)
	}
}
