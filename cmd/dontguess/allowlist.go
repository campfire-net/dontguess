package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/3dl-dev/dontguess/pkg/exchange"
	"github.com/3dl-dev/dontguess/pkg/identity"
	"github.com/spf13/cobra"
)

// allowlist.go — dontguess-b45 (design §6/§9-item2): `dontguess allowlist
// add|remove|list <npub>` mutates the persisted operator config's
// Config.FleetAllowlist — the flat, operator-maintained fleet npub set
// consulted once a TrustChecker is constructed (team/federated tier, relays
// attached). No vouching or transitive edges (design §2). This item ONLY
// mutates the config file on disk; wiring the live TrustChecker/KeySet at
// serve-time (Seam A/B/C/D) is a separate item (dontguess-3b8 §9-item3).
//
// NOT seller-only (dontguess-980): pkg/exchange/trust.go's TrustChecker.Level()
// consults this SAME allowlist for every sender regardless of role. A put
// (sell-side) requires TrustAllowlisted, but so do every buyer-side settle
// phase — buyer-accept, complete, dispute, preview-request,
// small-content-dispute (defaultSettlePhaseLevels, trust.go). A team-tier
// buyer who is minted (`dontguess mint`) but never allowlisted here has their
// settle(buyer-accept) silently dropped at the dispatch trust gate — see
// docs/design/nostr-first-client-ed2.md §3.4. `dontguess allowlist add
// <npub>` is required for BOTH sellers and buyers before their first
// put/settle succeeds.
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

	// dontguess-113: prefer the LIVE hot-reload path when the operator is running.
	// The running serve mutates the live TrustChecker KeySet + republishes the
	// operator-signed roster + persists Config.FleetAllowlist, all sub-second with
	// NO restart (so the 61a Since=0 history re-read never fires on admit). When the
	// socket is unreachable the operator is not running — fall back to writing the
	// config directly so the next start picks the entry up.
	if conn, ok := dialSocketMaybe(dgHome); ok {
		defer conn.Close()
		return allowlistLiveRequest(conn, dgHome, allowlistActionAdd, hexKey, npub, out)
	}

	cfg, err := exchange.LoadConfig(dgHome)
	if err != nil {
		return fmt.Errorf("allowlist add: %w", err)
	}

	changed := mutateAllowlistConfig(cfg, allowlistActionAdd, hexKey)
	if err := exchange.WriteConfig(exchange.ConfigPath(dgHome), cfg); err != nil {
		return fmt.Errorf("allowlist add: %w", err)
	}
	if changed {
		fmt.Fprintf(out, "allowlisted: %s\n", npub)
	} else {
		fmt.Fprintf(out, "already allowlisted: %s\n", npub)
	}
	return nil
}

// allowlistLiveRequest signs an operator-key authorization binding action+hexKey
// (mirroring `dontguess mint`) and drives the OpAllowlist IPC op over conn. The
// running operator verifies the signature (verifyAllowlistAuth) before mutating any
// live state, so merely reaching the socket does not admit a member (ADV-16).
// display is the operator-facing form (npub or hex as typed) used only in the
// success message; the wire target and the signature both bind the canonical hex.
func allowlistLiveRequest(conn net.Conn, dgHome, action, hexKey, display string, out io.Writer) error {
	signer, err := loadOperatorSigner(dgHome)
	if err != nil {
		return fmt.Errorf("allowlist %s: %w", action, err)
	}
	authEv := buildAllowlistAuthEvent(action, hexKey, time.Now().Unix())
	if err := identity.SignEvent(signer, authEv); err != nil {
		return fmt.Errorf("allowlist %s: signing authorization: %w", action, err)
	}

	var resp okResponse
	if err := sendRequest(conn, map[string]any{
		"op":               OpAllowlist,
		"allowlist_action": action,
		"allowlist_target": hexKey,
		"allowlist_auth":   authEv,
	}, &resp); err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("allowlist %s: %s", action, resp.Error)
	}
	switch action {
	case allowlistActionRemove:
		fmt.Fprintf(out, "removed (live): %s\n", display)
	default:
		fmt.Fprintf(out, "allowlisted (live): %s\n", display)
	}
	return nil
}

// runAllowlistRemove validates npub the same way as add (loud error, nothing
// persisted on a malformed entry), then drops any config entry whose
// normalized hex matches — regardless of whether it was originally stored as
// mutateAllowlistConfig applies an add|remove to cfg, keeping FleetAllowlist and
// the RevokedSellers tombstone (dontguess-23c) coherent as a single operator
// intent:
//
//	add    -> ensure present in FleetAllowlist, clear any RevokedSellers tombstone
//	remove -> drop from FleetAllowlist, RECORD a RevokedSellers tombstone
//
// The tombstone is what makes de-allowlisting durable across restart under the
// retention model (SEAM D withholds a revoked seller's accepted inventory; an
// absent-but-not-revoked seller's inventory is RETAINED). Comparison is by
// normalized hex so npub- and hex-stored forms of the same key reconcile.
// Returns whether the FleetAllowlist membership actually changed (for messaging).
func mutateAllowlistConfig(cfg *exchange.Config, action, targetHex string) (changed bool) {
	inList := func(list []string) bool {
		for _, e := range list {
			if h, err := normalizeToHex(e); err == nil && h == targetHex {
				return true
			}
		}
		return false
	}
	switch action {
	case allowlistActionAdd:
		changed = !inList(cfg.FleetAllowlist)
		if changed {
			cfg.FleetAllowlist = append(cfg.FleetAllowlist, targetHex)
		}
		cfg.RevokedSellers = dropHexEntry(cfg.RevokedSellers, targetHex)
	case allowlistActionRemove:
		changed = inList(cfg.FleetAllowlist)
		cfg.FleetAllowlist = dropHexEntry(cfg.FleetAllowlist, targetHex)
		if !inList(cfg.RevokedSellers) {
			cfg.RevokedSellers = append(cfg.RevokedSellers, targetHex)
		}
	}
	return changed
}

// dropHexEntry returns list without any entry whose normalized hex equals targetHex.
func dropHexEntry(list []string, targetHex string) []string {
	var kept []string
	for _, e := range list {
		if h, err := normalizeToHex(e); err == nil && h == targetHex {
			continue
		}
		kept = append(kept, e)
	}
	return kept
}

// npub or hex. Removing an absent entry is a no-op, not an error.
func runAllowlistRemove(dgHome, npub string, out io.Writer) error {
	if _, err := identity.NewAllowlist(npub); err != nil {
		return fmt.Errorf("allowlist remove: %w", err)
	}
	hexKey, err := normalizeToHex(npub)
	if err != nil {
		return fmt.Errorf("allowlist remove: %w", err)
	}

	// dontguess-113: live de-admission when the operator is running (KeySet.Remove +
	// roster republish + config persist, no restart); offline config write otherwise.
	if conn, ok := dialSocketMaybe(dgHome); ok {
		defer conn.Close()
		return allowlistLiveRequest(conn, dgHome, allowlistActionRemove, hexKey, npub, out)
	}

	cfg, err := exchange.LoadConfig(dgHome)
	if err != nil {
		return fmt.Errorf("allowlist remove: %w", err)
	}

	// Always record the revocation tombstone (dontguess-23c), even if the seller
	// was not currently in FleetAllowlist — the operator intent is "withhold this
	// seller's inventory", which must persist regardless of prior membership.
	changed := mutateAllowlistConfig(cfg, allowlistActionRemove, hexKey)
	if err := exchange.WriteConfig(exchange.ConfigPath(dgHome), cfg); err != nil {
		return fmt.Errorf("allowlist remove: %w", err)
	}
	if changed {
		fmt.Fprintf(out, "removed (revoked): %s\n", npub)
	} else {
		fmt.Fprintf(out, "revoked (was not in allowlist): %s\n", npub)
	}
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
	}
	for _, e := range cfg.FleetAllowlist {
		fmt.Fprintln(out, displayNpub(e))
	}
	// Surface the revocation tombstones (dontguess-23c) so the operator can see
	// which sellers are withheld — distinct from "simply not admitted".
	if len(cfg.RevokedSellers) > 0 {
		fmt.Fprintf(out, "\nrevoked (inventory withheld) — %d:\n", len(cfg.RevokedSellers))
		for _, e := range cfg.RevokedSellers {
			fmt.Fprintln(out, displayNpub(e))
		}
	}
	return nil
}

// displayNpub renders a stored entry (canonical lowercase hex, or a legacy
// npub-form entry) as an npub for human-facing output, falling back to the raw
// entry if it cannot be encoded.
func displayNpub(entry string) string {
	hexKey, err := normalizeToHex(entry)
	if err != nil {
		return entry
	}
	if npub, err := identity.EncodeNpubHex(hexKey); err == nil {
		return npub
	}
	return entry
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
