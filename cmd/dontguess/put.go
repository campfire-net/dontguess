package main

// put.go is item ed2-A: the team-tier `dontguess put` cobra command. It wires
// pkg/relayclient's publish primitive to the CLI — sign with the AGENT key
// (never the operator key), publish direct to the relay (team tier never
// routes puts through the operator to be re-signed — design
// docs/design/nostr-first-client-ed2.md §3.1/§3.3), and surface the operator's
// put-reject reason LOUD when the seller is not allowlisted.
//
// Individual tier (zero-relay, socket IPC to a local `serve`) is item ed2-E
// and is out of scope here; this command requires DONTGUESS_RELAY_URLS.

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"strings"

	"github.com/campfire-net/dontguess/pkg/identity"
	"github.com/campfire-net/dontguess/pkg/relayclient"
	"github.com/spf13/cobra"
)

// newPutCmd builds the put cobra command. Extracted from init() so tests can
// construct an isolated instance per test case rather than mutating the
// package-level singleton's flag state across cases.
func newPutCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "put",
		Short: "Publish a put (sell cached inference) to the team exchange",
		Long: `put publishes an exchange:put event directly to the team relay, signed with
the AGENT key from AGENT_CF_HOME (never the operator key).

A relay ["OK", ...] is a TRANSPORT RECEIPT ONLY, not proof the operator
admitted the put. put REQ-subscribes for a settle(put-reject) referencing this
put's id and, if the seller is not on the operator's allowlist (or otherwise
rejected), prints the operator's reason and exits non-zero.

Requires DONTGUESS_RELAY_URLS (team tier). Individual tier (no relay) is not
yet wired to this command.`,
		RunE: runPut,
	}
	cmd.Flags().String("description", "", "task description this content answers (required)")
	cmd.Flags().String("content", "", "base64-encoded content bytes (required)")
	cmd.Flags().Int64("token_cost", 0, "tokens spent computing this content (required)")
	cmd.Flags().String("content_type", "exchange:content-type:text", "full exchange content-type tag")
	cmd.Flags().StringSlice("domains", nil, "domain tags (comma-separated)")
	cmd.Flags().String("relay", "", "relay websocket URL (default: first of DONTGUESS_RELAY_URLS)")
	cmd.Flags().Duration("timeout", relayclient.DefaultTimeout, "bounded end-to-end timeout (dial, publish, await OK + put-reject)")
	cmd.Flags().Bool("relay-auth", false, "opt into the NIP-42 client AUTH handshake (default: WithoutClientAuth)")
	return cmd
}

var putCmd = newPutCmd()

func init() {
	rootCmd.AddCommand(putCmd)
}

func runPut(cmd *cobra.Command, args []string) error {
	description, _ := cmd.Flags().GetString("description")
	contentB64, _ := cmd.Flags().GetString("content")
	tokenCost, _ := cmd.Flags().GetInt64("token_cost")
	contentType, _ := cmd.Flags().GetString("content_type")
	domains, _ := cmd.Flags().GetStringSlice("domains")
	relayURL, _ := cmd.Flags().GetString("relay")
	timeout, _ := cmd.Flags().GetDuration("timeout")
	relayAuth, _ := cmd.Flags().GetBool("relay-auth")

	if description == "" {
		return fmt.Errorf("put: --description is required")
	}
	if contentB64 == "" {
		return fmt.Errorf("put: --content is required (base64-encoded)")
	}
	if tokenCost <= 0 {
		return fmt.Errorf("put: --token_cost must be positive")
	}
	content, err := base64.StdEncoding.DecodeString(contentB64)
	if err != nil {
		return fmt.Errorf("put: --content is not valid base64: %w", err)
	}

	if relayURL == "" {
		urls := resolveRelayURLs()
		if len(urls) == 0 {
			return fmt.Errorf("put: no relay configured — set DONTGUESS_RELAY_URLS (team tier) or pass --relay. Individual-tier (zero-relay) put is not yet wired to this command")
		}
		relayURL = urls[0]
	}

	signer, err := loadAgentSigner()
	if err != nil {
		return fmt.Errorf("put: %w", err)
	}

	conn := relayclient.NewConn(relayURL, signer, relayclient.WithRelayAuth(relayAuth))
	defer conn.Close()

	base := cmd.Context()
	if base == nil {
		// cmd.Context() is nil unless the command was run through
		// (Execute|ExecuteContext); guard so unit tests calling runPut directly
		// don't need to thread a context through cobra's Execute machinery.
		base = context.Background()
	}
	ctx, cancel := context.WithTimeout(base, timeout)
	defer cancel()

	result, err := relayclient.Put(ctx, conn, signer, relayclient.PutRequest{
		Description: description,
		Content:     content,
		TokenCost:   tokenCost,
		ContentType: contentType,
		Domains:     domains,
	})
	if err != nil {
		return fmt.Errorf("put failed: %w", err)
	}

	if result.Rejected {
		fmt.Fprintf(cmd.OutOrStdout(), "put %s REJECTED: %s\n", result.PutID, result.RejectReason)
		fmt.Fprintf(cmd.OutOrStdout(), "ask the operator to run: dontguess allowlist add %s\n", signer.Npub())
		return fmt.Errorf("put %s rejected by operator: %s", result.PutID, result.RejectReason)
	}
	if !result.Accepted {
		return fmt.Errorf("put %s: relay did not accept the event: %s", result.PutID, result.OKMessage)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "put %s accepted by relay; no put-reject observed within %s\n", result.PutID, timeout)
	return nil
}

// loadAgentSigner resolves the AGENT signing identity from AGENT_CF_HOME (set
// by `eval $(dontguess agent-init <name> ...)`). It is deliberately distinct
// from the operator key: team-tier put/buy/settle are always signed by the
// agent identity, never the operator's (design §3.1).
func loadAgentSigner() (identity.Signer, error) {
	dir := strings.TrimSpace(os.Getenv("AGENT_CF_HOME"))
	if dir == "" {
		return nil, fmt.Errorf("AGENT_CF_HOME is not set — run: eval $(dontguess agent-init <name> --fleet-member)")
	}
	id, err := identity.Resolve(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve agent identity at AGENT_CF_HOME=%s: %w", dir, err)
	}
	return id, nil
}
