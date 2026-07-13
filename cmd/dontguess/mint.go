package main

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/campfire-net/dontguess/pkg/identity"
	"github.com/spf13/cobra"
)

// mint.go — dontguess-af86 (design §4/§5): `dontguess mint <npub|hex> <amount>`
// is the operator genesis-funding god-button. A fresh team-tier LocalScripStore
// folds zero balances and no other scrip-mint emitter exists, so without a mint
// the first buy deadlocks on ErrBudgetExceeded. Mint funds the team genesis
// balance; thereafter labor income (assign-pay) and residuals recirculate.
//
// It is operator-only: the command reaches the running exchange over the
// operator IPC socket, which lives in a 0700 directory inside the process trust
// boundary (the same channel as accept-put/reject-put). The server-side handler
// (eng.MintScrip) emits a durable operator-signed scrip-mint and audit-logs it;
// x402 is the eventual external real-money rail.
//
// The recipient is validated as a real nostr pubkey (npub or hex) via
// identity.NewAllowlist before anything is sent — a malformed recipient is a
// hard, loud error and nothing is minted.

var mintCmd = &cobra.Command{
	Use:   "mint <npub|hex> <amount>",
	Short: "Operator god-button: mint scrip to an agent (team-tier genesis funding)",
	Long: `Mint scrip into an agent's balance so the first team-tier buy does not
deadlock on ErrBudgetExceeded. Operator-only — the command talks to the running
exchange over the operator socket; the server audit-logs every mint. Requires a
running exchange with relays attached (scrip accounting is disabled on the
individual/no-relay tier). See
docs/design/nostr-admission-scrip-rehome-3b8.md §4.`,
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runMint(args[0], args[1], cmd.OutOrStdout())
	},
}

func init() {
	rootCmd.AddCommand(mintCmd)
}

// runMint validates the recipient (reusing identity.NewAllowlist so a malformed
// npub/hex is rejected before anything is sent) and the amount, then asks the
// running operator to emit the mint over the IPC socket. Nothing is minted on a
// validation failure.
func runMint(recipient, amountStr string, out io.Writer) error {
	if strings.TrimSpace(recipient) == "" {
		return fmt.Errorf("mint: recipient required")
	}
	if _, err := identity.NewAllowlist(recipient); err != nil {
		return fmt.Errorf("mint: invalid recipient: %w", err)
	}
	hexKey, err := normalizeToHex(recipient)
	if err != nil {
		return fmt.Errorf("mint: invalid recipient: %w", err)
	}

	amount, err := strconv.ParseInt(amountStr, 10, 64)
	if err != nil {
		return fmt.Errorf("mint: invalid amount %q: %w", amountStr, err)
	}
	if amount <= 0 {
		return fmt.Errorf("mint: amount must be > 0, got %d", amount)
	}

	// dontguess-f91 (RT-B#3): prove possession of the operator key. The server
	// rejects an OpMint that is not signed by the persisted operator key, so a
	// local process merely reaching the socket can no longer trigger a mint.
	// Load the operator identity and sign an auth event binding recipient+amount.
	signer, err := loadOperatorSigner(resolveDGHome())
	if err != nil {
		return fmt.Errorf("mint: %w", err)
	}
	authEv := buildMintAuthEvent(hexKey, amount, time.Now().Unix())
	if err := identity.SignEvent(signer, authEv); err != nil {
		return fmt.Errorf("mint: signing authorization: %w", err)
	}

	conn := dialSocket()
	defer conn.Close()

	var resp okResponse
	if err := sendRequest(conn, map[string]any{
		"op":        OpMint,
		"recipient": hexKey,
		"amount":    amount,
		"mint_auth": authEv,
	}, &resp); err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("mint: %s", resp.Error)
	}

	fmt.Fprintf(out, "minted %d scrip to %s\n", amount, recipient)
	return nil
}
