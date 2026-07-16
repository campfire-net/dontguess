package main

// join.go — `dontguess join <token> [--name N] [--relay URL]` (design
// docs/design/onboarding-tiered-scaling-federation.md §1 + §9 Gate B/P8, ADV-15).
//
// The member side of one-paste onboarding. It:
//
//	1. decodes the dgi1_ token and VERIFIES the operator signature + not-expired
//	   (nostr.ParseInviteToken re-runs the real BIP-340 check; CheckInviteFresh);
//	2. self-provisions a FRESH member key via agent-init --fleet-member (fail-closed,
//	   no default mint — runAgentInitCore / agent_init.go:122);
//	3. builds a kind-3410 REDEEM event signed by that fresh member key, EMBEDDING the
//	   whole token, referencing the invite grant id; and
//	4. publishes it to the token's relay(s) — an OPEN relay accepts it freely; the
//	   operator's serve reader does 100% of the verification + promotion + genesis
//	   mint (serve_redeem.go). Absorbs agent-init + allowlist add + mint into one paste.
//
// The relay OK is a TRANSPORT RECEIPT ONLY — admission is the operator's, observed
// on the exchange once the operator's roster republishes. join reports the redeem
// was accepted for delivery; it does not claim admission the operator has not made.

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/3dl-dev/dontguess/pkg/identity"
	"github.com/3dl-dev/dontguess/pkg/nostr"
	"github.com/3dl-dev/dontguess/pkg/relayclient"
	"github.com/spf13/cobra"
)

// joinPublishTimeout bounds each per-relay publish (dial + NIP-01 EVENT + OK). A
// one-shot CLI must fail fast rather than hang on a dead relay.
const joinPublishTimeout = 20 * time.Second

var joinCmd = &cobra.Command{
	Use:   "join <token>",
	Short: "Redeem a dgi1_ invite token — self-provision and join a fleet (member)",
	Long: `Redeem an operator-minted invite token in one paste: verify the operator
signature, self-provision a fresh member identity, and publish a redeem event the
operator verifies and promotes into the fleet (admitting you + minting any genesis
grant).

  dontguess join dgi1_<blob>

--name overrides the member name embedded in the token; --relay overrides the relay
URL(s) the redeem is published to.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name, _ := cmd.Flags().GetString("name")
		relays, _ := cmd.Flags().GetStringSlice("relay")
		return runJoin(cmd.Context(), resolveDGHome(), args[0], name, relays, cmd.OutOrStdout())
	},
}

func init() {
	joinCmd.Flags().String("name", "", "member name to provision under (default: the name embedded in the token)")
	joinCmd.Flags().StringSlice("relay", nil, "relay URL(s) to publish the redeem to (default: the relays embedded in the token)")
	rootCmd.AddCommand(joinCmd)
}

// runJoin executes the member-side redeem. ctx bounds the whole operation; a nil
// ctx falls back to context.Background so cobra call sites without a context work.
func runJoin(ctx context.Context, dgHome, token, nameOverride string, relayOverride []string, out io.Writer) error {
	if ctx == nil {
		ctx = context.Background()
	}

	// (1) Decode + verify the operator signature, then freshness.
	in, err := nostr.ParseInviteToken(token)
	if err != nil {
		return fmt.Errorf("join: invalid token: %w", err)
	}
	if err := nostr.CheckInviteFresh(in, time.Now().Unix()); err != nil {
		return fmt.Errorf("join: %w", err)
	}
	fmt.Fprintf(out, "✓ verified operator signature (npub pin %s), not expired\n", shortHex(in.OperatorPubKey))

	// Pick the member name: explicit override wins, else the token's embedded hint.
	name := nameOverride
	if name == "" {
		name = in.Name
	}
	if name == "" {
		return fmt.Errorf("join: no member name in the token — pass --name <name>")
	}

	// (2) Self-provision a FRESH member key (fail-closed fleet member; no default
	// mint — agent_init.go:122). Idempotent: re-running loads the same key.
	if err := runAgentInitCore(dgHome, name, "", true /* fleetMember */); err != nil {
		return fmt.Errorf("join: provision member identity: %w", err)
	}
	agentHome := filepath.Join(dgHome, "agents", name)
	memberSigner, err := identity.Resolve(agentHome)
	if err != nil {
		return fmt.Errorf("join: load provisioned member identity: %w", err)
	}
	fmt.Fprintf(out, "✓ provisioned member identity %q (npub %s)\n", name, memberSigner.Npub())

	// (3) Build the kind-3410 redeem signed by the fresh member key, embedding the
	// token. The member pubkey is the npub the operator binds to the grant.
	redeem, err := nostr.BuildRedeemEvent(memberSigner, in, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("join: build redeem: %w", err)
	}

	// (4) Publish to the token's relays (or the --relay override). At least one
	// accepted OK is required — the operator reads the redeem off whatever relay it
	// also tails.
	relays := relayOverride
	if len(relays) == 0 {
		relays = in.RelayURLs
	}
	if len(relays) == 0 {
		return fmt.Errorf("join: no relay to publish the redeem to — the token carried none; pass --relay <url>")
	}

	published := false
	var lastErr error
	for _, url := range relays {
		pctx, cancel := context.WithTimeout(ctx, joinPublishTimeout)
		conn := relayclient.NewConn(url, memberSigner)
		accepted, msg, perr := relayclient.PublishEvent(pctx, conn, redeem)
		_ = conn.Close()
		cancel()
		if perr != nil {
			lastErr = perr
			fmt.Fprintf(out, "  relay %s: publish failed: %v\n", url, perr)
			continue
		}
		if !accepted {
			lastErr = fmt.Errorf("relay %s rejected the redeem: %s", url, msg)
			fmt.Fprintf(out, "  relay %s: rejected redeem: %s\n", url, msg)
			continue
		}
		fmt.Fprintf(out, "  relay %s: redeem accepted for delivery\n", url)
		published = true
	}
	if !published {
		return fmt.Errorf("join: could not publish the redeem to any relay: %w", lastErr)
	}

	fmt.Fprintf(out, "\n→ redeem published (grant %s). The operator verifies and admits you;\n", shortRedeemID(in.GrantID))
	fmt.Fprintf(out, "  once its roster republishes you can buy/put/settle as %q.\n", name)
	return nil
}
