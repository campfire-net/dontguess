package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/campfire-net/dontguess/pkg/exchange"
	"github.com/campfire-net/dontguess/pkg/identity"
	"github.com/spf13/cobra"
)

// allowlist.go — dontguess-b45 (design §6/§9-item2): `dontguess allowlist
// add|remove|list <npub>` mutates the persisted operator config's
// Config.FleetAllowlist — the flat, operator-maintained seller npub set
// consulted once a TrustChecker is constructed (team/federated tier, relays
// attached). No vouching or transitive edges (design §2). This item ONLY
// mutates the config file on disk; wiring the live TrustChecker/KeySet at
// serve-time (Seam A/B/C/D) is a separate item (dontguess-3b8 §9-item3).
//
// Validation reuses identity.NewAllowlist (pkg/identity/allowlist.go) so a
// malformed npub is a hard, loud error — nothing is read or written to the
// config on a validation failure.

var allowlistCmd = &cobra.Command{
	Use:   "allowlist",
	Short: "Manage the operator's fleet seller allowlist (team/federated tier)",
	Long: `The fleet allowlist is the flat, operator-maintained set of seller
npubs admitted once relays are attached and a TrustChecker is constructed. It
has no vouching or transitive edges — see
docs/design/nostr-admission-scrip-rehome-3b8.md §6.

The individual tier (no relays configured) never consults this list.`,
}

var allowlistAddCmd = &cobra.Command{
	Use:   "add <npub>",
	Short: "Admit a seller npub to the fleet allowlist",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runAllowlistAdd(resolveDGHome(), args[0], cmd.OutOrStdout())
	},
}

var allowlistRemoveCmd = &cobra.Command{
	Use:     "remove <npub>",
	Aliases: []string{"rm"},
	Short:   "Remove a seller npub from the fleet allowlist",
	Args:    cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runAllowlistRemove(resolveDGHome(), args[0], cmd.OutOrStdout())
	},
}

var allowlistListCmd = &cobra.Command{
	Use:   "list",
	Short: "List the fleet allowlist",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runAllowlistList(resolveDGHome(), cmd.OutOrStdout())
	},
}

func init() {
	allowlistCmd.AddCommand(allowlistAddCmd, allowlistRemoveCmd, allowlistListCmd)
	rootCmd.AddCommand(allowlistCmd)
}

// runAllowlistAdd validates npub (reusing identity.NewAllowlist — a malformed
// entry is a hard error, never silently dropped) BEFORE touching the config,
// so a rejected entry persists nothing. A duplicate add (already admitted,
// compared by normalized hex so npub and hex forms of the same key collide)
// is a no-op, not an error.
func runAllowlistAdd(dgHome, npub string, out io.Writer) error {
	if _, err := identity.NewAllowlist(npub); err != nil {
		return fmt.Errorf("allowlist add: %w", err)
	}
	hexKey, err := normalizeToHex(npub)
	if err != nil {
		return fmt.Errorf("allowlist add: %w", err)
	}

	cfg, err := exchange.LoadConfig(dgHome)
	if err != nil {
		return fmt.Errorf("allowlist add: %w", err)
	}

	for _, existing := range cfg.FleetAllowlist {
		if existingHex, nerr := normalizeToHex(existing); nerr == nil && existingHex == hexKey {
			fmt.Fprintf(out, "already allowlisted: %s\n", npub)
			return nil
		}
	}

	cfg.FleetAllowlist = append(cfg.FleetAllowlist, npub)
	if err := exchange.WriteConfig(exchange.ConfigPath(dgHome), cfg); err != nil {
		return fmt.Errorf("allowlist add: %w", err)
	}
	fmt.Fprintf(out, "allowlisted: %s\n", npub)
	return nil
}

// runAllowlistRemove validates npub the same way as add (loud error, nothing
// persisted on a malformed entry), then drops any config entry whose
// normalized hex matches — regardless of whether it was originally stored as
// npub or hex. Removing an absent entry is a no-op, not an error.
func runAllowlistRemove(dgHome, npub string, out io.Writer) error {
	if _, err := identity.NewAllowlist(npub); err != nil {
		return fmt.Errorf("allowlist remove: %w", err)
	}
	hexKey, err := normalizeToHex(npub)
	if err != nil {
		return fmt.Errorf("allowlist remove: %w", err)
	}

	cfg, err := exchange.LoadConfig(dgHome)
	if err != nil {
		return fmt.Errorf("allowlist remove: %w", err)
	}

	var kept []string
	removed := false
	for _, existing := range cfg.FleetAllowlist {
		if existingHex, nerr := normalizeToHex(existing); nerr == nil && existingHex == hexKey {
			removed = true
			continue
		}
		kept = append(kept, existing)
	}
	if !removed {
		fmt.Fprintf(out, "not allowlisted: %s\n", npub)
		return nil
	}

	cfg.FleetAllowlist = kept
	if err := exchange.WriteConfig(exchange.ConfigPath(dgHome), cfg); err != nil {
		return fmt.Errorf("allowlist remove: %w", err)
	}
	fmt.Fprintf(out, "removed: %s\n", npub)
	return nil
}

// runAllowlistList prints the persisted FleetAllowlist, one entry per line
// (or as a JSON array with --json).
func runAllowlistList(dgHome string, out io.Writer) error {
	cfg, err := exchange.LoadConfig(dgHome)
	if err != nil {
		return fmt.Errorf("allowlist list: %w", err)
	}

	if jsonOutput {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(cfg.FleetAllowlist)
	}

	if len(cfg.FleetAllowlist) == 0 {
		fmt.Fprintln(out, "(empty — only the operator key is admitted once a TrustChecker is constructed)")
		return nil
	}
	for _, e := range cfg.FleetAllowlist {
		fmt.Fprintln(out, e)
	}
	return nil
}

// normalizeToHex converts an npub or hex pubkey entry to lowercase hex, so
// add/remove/dedup compare correctly regardless of which form the config
// stores an entry in. Callers must have already validated the entry via
// identity.NewAllowlist; this only decodes.
func normalizeToHex(entry string) (string, error) {
	entry = strings.TrimSpace(entry)
	if strings.HasPrefix(entry, "npub1") {
		return identity.DecodeNpubToHex(entry)
	}
	return strings.ToLower(entry), nil
}
