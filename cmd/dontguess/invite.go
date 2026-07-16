package main

// invite.go — `dontguess invite <name> [--scrip N] [--ttl D]` (design
// docs/design/onboarding-tiered-scaling-federation.md §1 + §9 Gate B/P8, ADV-15).
//
// Mints an OPERATOR-SIGNED, scoped, single-use, TTL'd, npub-bound-on-redeem token
// (the "dgi1_" blob) carrying: the operator's relay URLs, the operator npub (the
// member pins it — it is the token's own author pubkey), a one-time admission grant
// id, and an optional genesis scrip grant. The member pastes it into `dontguess
// join`, which self-provisions a member key and publishes a kind-3410 redeem the
// operator verifies + promotes (join.go / serve_redeem.go).
//
// The token is NOT a reusable bearer credential: the grant id is one-time (the
// operator persists it redeemed on first redeem) and the admitted npub is bound at
// redeem to whatever fresh key publishes the redeem. Minting only requires the
// operator key (loadOperatorSigner), exactly like `dontguess mint`.

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/3dl-dev/dontguess/pkg/exchange"
	"github.com/3dl-dev/dontguess/pkg/nostr"
	"github.com/spf13/cobra"
)

// defaultInviteTTL is the token lifetime when --ttl is not given. A bounded default
// (not "forever") keeps a leaked-but-unredeemed token from being valid indefinitely.
const defaultInviteTTL = 72 * time.Hour

var inviteCmd = &cobra.Command{
	Use:   "invite <name>",
	Short: "Mint a one-paste join token for a new fleet member (operator)",
	Long: `Mint an operator-signed, single-use, TTL'd invite token (dgi1_…) a new
member redeems with 'dontguess join <token>'. The token carries the operator's
relay URLs, the operator npub pin, a one-time admission grant id, and an optional
genesis scrip grant.

  dontguess invite alice --scrip 50000 --ttl 72h

The token is not a reusable bearer credential: the grant is single-use (the
operator records it redeemed on first redeem) and the admitted key is bound to the
member key that publishes the redeem.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		scrip, _ := cmd.Flags().GetInt64("scrip")
		ttl, _ := cmd.Flags().GetDuration("ttl")
		return runInvite(resolveDGHome(), args[0], scrip, ttl, cmd.OutOrStdout())
	},
}

func init() {
	inviteCmd.Flags().Int64("scrip", 0, "optional genesis scrip grant minted to the member on redeem (0 = none)")
	inviteCmd.Flags().Duration("ttl", defaultInviteTTL, "token lifetime before it expires (0 = never expires)")
	rootCmd.AddCommand(inviteCmd)
}

// runInvite mints and prints the token. scrip < 0 is rejected. A ttl of 0 means the
// token never expires (expiry field 0); any positive ttl sets an absolute expiry.
func runInvite(dgHome, name string, scrip int64, ttl time.Duration, out interface {
	Write([]byte) (int, error)
}) error {
	if scrip < 0 {
		return fmt.Errorf("invite: --scrip must be >= 0, got %d", scrip)
	}
	if ttl < 0 {
		return fmt.Errorf("invite: --ttl must be >= 0, got %s", ttl)
	}

	signer, err := loadOperatorSigner(dgHome)
	if err != nil {
		return fmt.Errorf("invite: %w", err)
	}

	// Relay URLs travel in the token so join knows where to publish the redeem. An
	// absent config is not fatal here (the member can point join at a relay with
	// --relay), but the team-tier operator always has them.
	var relayURLs []string
	if cfg, cerr := exchange.LoadConfig(dgHome); cerr == nil {
		relayURLs = cfg.RelayURLs
	}

	grantID, err := newInviteGrantID()
	if err != nil {
		return fmt.Errorf("invite: %w", err)
	}

	now := time.Now().Unix()
	var expiry int64 // 0 = never expires
	if ttl > 0 {
		expiry = now + int64(ttl.Seconds())
	}

	token, err := nostr.BuildInviteToken(signer, name, grantID, relayURLs, scrip, now, expiry)
	if err != nil {
		return fmt.Errorf("invite: %w", err)
	}

	fmt.Fprintf(out, "invite token for %q:\n\n%s\n\n", name, token)
	fmt.Fprintf(out, "  operator npub: %s\n", signer.Npub())
	if len(relayURLs) > 0 {
		fmt.Fprintf(out, "  relays:        %v\n", relayURLs)
	}
	if scrip > 0 {
		fmt.Fprintf(out, "  genesis scrip: %d\n", scrip)
	}
	if expiry > 0 {
		fmt.Fprintf(out, "  expires:       %s\n", time.Unix(expiry, 0).Format(time.RFC3339))
	} else {
		fmt.Fprintf(out, "  expires:       never\n")
	}
	fmt.Fprintf(out, "\nOn the member's machine, one paste:\n  dontguess join %s\n", token)
	return nil
}

// newInviteGrantID returns a fresh, unguessable one-time admission grant id (128
// bits of entropy, hex). Unguessability matters: the grant id is the token's
// single-use handle the operator records as redeemed.
func newInviteGrantID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate grant id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}
