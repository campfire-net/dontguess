package main

// agent_init.go — dontguess agent-init <name>
//
// Provisions a per-agent secp256k1/schnorr nostr identity under
// $DG_HOME/agents/<name>/. Nostr-first: this command mints (or borrows) the
// identity that signs nostr events and authenticates to the team relay via
// NIP-42. There is no campfire admit/join ceremony here — that Ed25519
// campfire identity flow was removed as part of the nostr-first cutover
// (docs/design/nostr-first-rebuild-decision.md); see the be4 finding.
//
// Prints "export AGENT_CF_HOME=<path>" for the user to eval.
//
// Idempotent: re-running with the same name loads the existing identity
// and skips re-generation. The export line is printed again either way.
//
// One of --fleet-member or --parent is REQUIRED (fail-closed — there is no
// default that mints an identity):
//
//  1. FLEET MEMBER (--fleet-member, no --parent): gets a PERSISTENT npub via
//     identity.LoadOrCreate. Re-running loads the SAME key rather than
//     minting a throwaway.
//  2. EPHEMERAL SUBAGENT (--parent P): signs under fleet member P's npub via
//     identity.BorrowParent. No new independent npub is minted — a fresh
//     throwaway per subagent would destroy reputation continuity AND inflate
//     convergence independence (a Sybil vector). Convergence is scored at
//     the parent (fleet-member) npub granularity.
//
// See docs/design/nostr-first-rebuild-decision.md (key-management ruling)
// and docs/design/convergence-sybil-defense.md.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/campfire-net/dontguess/pkg/identity"
	"github.com/spf13/cobra"
)

var agentInitCmd = &cobra.Command{
	Use:   "agent-init <name>",
	Short: "Provision a per-agent secp256k1 nostr identity",
	Long: `Create a per-agent secp256k1/schnorr nostr identity under $DG_HOME/agents/<name>/.
Prints the export AGENT_CF_HOME line for the user to eval.

Idempotent: re-running with the same name does not regenerate the identity.
The export line is printed regardless.

One of --fleet-member or --parent is REQUIRED (fail-closed — there is no
default that mints an identity):

  dontguess agent-init alice --fleet-member     # persistent fleet member, own npub
  dontguess agent-init sub1 --parent alice      # ephemeral subagent, signs as alice
  eval $(dontguess agent-init alice --fleet-member)   # sets AGENT_CF_HOME in current shell`,
	Args: cobra.ExactArgs(1),
	RunE: runAgentInit,
}

func init() {
	agentInitCmd.Flags().String("parent", "",
		"provision an ephemeral subagent that signs under this parent fleet member's npub (no new key is minted)")
	agentInitCmd.Flags().Bool("fleet-member", false,
		"provision a persistent fleet member with its own npub (required when --parent is not given; fail-closed default)")
	rootCmd.AddCommand(agentInitCmd)
}

func runAgentInit(cmd *cobra.Command, args []string) error {
	parent := ""
	fleetMember := false
	if cmd != nil {
		parent, _ = cmd.Flags().GetString("parent")
		fleetMember, _ = cmd.Flags().GetBool("fleet-member")
	}
	return runAgentInitCore(resolveDGHome(), args[0], parent, fleetMember)
}

// runAgentInitCore provisions agent <name> under dgHome. Exactly one of the
// two identity modes must be selected explicitly — there is no default that
// mints a key:
//
//   - fleetMember=true (--fleet-member, parent empty): <name> is a long-lived
//     FLEET MEMBER and gets a persistent secp256k1 npub via LoadOrCreate.
//   - parent != "" (--parent P, fleetMember false): <name> is an ephemeral
//     SUBAGENT that signs under fleet member P's npub — no new npub is minted
//     (the Sybil / convergence-integrity defense; see
//     docs/design/nostr-first-rebuild-decision.md key-management ruling and
//     docs/design/convergence-sybil-defense.md).
//
// Neither flag given is a HARD ERROR (fail-closed): the parent-key Sybil
// defense must not depend on every caller remembering to pass --parent. An
// ephemeral subagent request that omits --parent is rejected rather than
// silently falling through to minting a persistent npub — minting is only
// reachable via the explicit --fleet-member assertion. Both flags given is
// also rejected: the two modes are mutually exclusive.
func runAgentInitCore(dgHome, name, parent string, fleetMember bool) error {
	// Security: the name becomes a path component under DG_HOME/agents. Reject
	// path separators and any "." / ".." traversal — otherwise `agent-init ..`
	// resolves to DG_HOME itself and would load the operator's identity (CVE-class
	// privilege escalation: the caller would gain operator signing authority).
	if name == "" || name == "." || name == ".." ||
		strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") {
		return fmt.Errorf("invalid agent name %q: must be a single path component without '/', '\\', or '..'", name)
	}
	// The parent name (if any) becomes a path component too — same validation.
	// This also guarantees a subagent can never name the operator ('.'/'..')
	// as its parent: the operator key is never borrowed.
	if parent != "" {
		if parent == "." || parent == ".." ||
			strings.ContainsAny(parent, "/\\") || strings.Contains(parent, "..") {
			return fmt.Errorf("invalid parent name %q: must be a single path component without '/', '\\', or '..'", parent)
		}
		if parent == name {
			return fmt.Errorf("agent %q cannot be its own parent", name)
		}
	}
	// Fail-closed mode selection: exactly one of --parent / --fleet-member must
	// be given. This is the Sybil-defense enforcement point (dontguess-ebf,
	// item ab9 follow-up) — an ephemeral subagent request MUST present a
	// parent; there is no default path that mints a new npub.
	switch {
	case parent == "" && !fleetMember:
		return fmt.Errorf("agent %q: must pass --parent <fleet-member> to provision an ephemeral subagent, or --fleet-member to provision a persistent fleet identity — no default identity is minted", name)
	case parent != "" && fleetMember:
		return fmt.Errorf("agent %q: --parent and --fleet-member are mutually exclusive (a subagent borrows a parent's npub; a fleet member mints its own)", name)
	}

	// Step 1: create the agent home directory.
	agentsRoot := filepath.Join(dgHome, "agents")
	agentHome := filepath.Join(agentsRoot, name)
	// Defense in depth: the resolved path must stay strictly under agents/.
	if agentHome == agentsRoot || !strings.HasPrefix(agentHome+string(filepath.Separator), agentsRoot+string(filepath.Separator)) {
		return fmt.Errorf("invalid agent name %q: resolves outside the agents directory", name)
	}
	if err := os.MkdirAll(agentHome, 0700); err != nil {
		return fmt.Errorf("creating agent home %s: %w", agentHome, err)
	}

	// Step 2: issue (or borrow) the secp256k1/schnorr nostr identity — the
	// identity that signs nostr events and authenticates to the team relay via
	// NIP-42. This is where the key-management ruling is enforced by
	// construction (docs/design/nostr-first-rebuild-decision.md key-mgmt ruling;
	// docs/design/convergence-sybil-defense.md):
	//
	//   - FLEET MEMBER (--fleet-member, no --parent): gets a PERSISTENT npub via
	//     LoadOrCreate. Re-running loads the SAME key rather than minting a
	//     throwaway.
	//   - EPHEMERAL SUBAGENT (--parent P): signs under P's fleet-member npub via
	//     BorrowParent. No new independent npub is minted — a fresh throwaway per
	//     subagent would destroy reputation continuity AND inflate convergence
	//     independence (a Sybil vector). Convergence is scored at the parent
	//     (fleet-member) npub granularity.
	//
	// The operator key is never borrowed: parent is constrained to a single path
	// component under agents/, so it can never resolve to DG_HOME (the operator
	// home). A subagent that named the operator would have to name '.'/'..',
	// which the validation above rejects.
	var nostrAction string
	if parent != "" {
		parentHome := filepath.Join(agentsRoot, parent)
		// Defense in depth (mirrors the agentHome guard): the parent must stay
		// strictly under agents/ — never the operator home, never outside.
		//
		// The lexical check alone is defeatable: BorrowParent -> Load reads
		// <parentHome>/nostr-identity.json via os.ReadFile, which follows
		// symlinks. If a symlink were planted at agents/<parent> pointing
		// outside agents/ (e.g. at DG_HOME itself), the string-prefix check
		// above would still pass (the UNRESOLVED path "agents/<parent>" is
		// lexically under agentsRoot) while the actual file read escapes to
		// the operator's identity — handing a subagent operator signing
		// authority. Resolve symlinks on both sides of the comparison first
		// so the check runs against the real filesystem targets, not the
		// lexical path. A parentHome that doesn't exist yet (e.g. a typo'd
		// parent name) simply fails EvalSymlinks; fall through to the
		// unresolved lexical check in that case — BorrowParent's Load then
		// fails cleanly with "no such fleet member" rather than any
		// security-relevant success.
		resolvedParentHome := parentHome
		resolvedAgentsRoot := agentsRoot
		resolvedDGHome := dgHome
		if rp, evalErr := filepath.EvalSymlinks(parentHome); evalErr == nil {
			resolvedParentHome = rp
			// Only trust the resolved comparison once we can resolve the
			// anchors too — an anchor that fails to resolve here would be a
			// deeper filesystem problem (DG_HOME/agents missing), and Step 1
			// already created agentsRoot via MkdirAll, so this should not fail.
			if ra, err := filepath.EvalSymlinks(agentsRoot); err == nil {
				resolvedAgentsRoot = ra
			}
			if rd, err := filepath.EvalSymlinks(dgHome); err == nil {
				resolvedDGHome = rd
			}
		}
		if parentHome == agentsRoot || parentHome == dgHome ||
			!strings.HasPrefix(parentHome+string(filepath.Separator), agentsRoot+string(filepath.Separator)) ||
			resolvedParentHome == resolvedAgentsRoot || resolvedParentHome == resolvedDGHome ||
			!strings.HasPrefix(resolvedParentHome+string(filepath.Separator), resolvedAgentsRoot+string(filepath.Separator)) {
			return fmt.Errorf("invalid parent %q: resolves outside the agents directory", parent)
		}
		if _, err := identity.BorrowParent(agentHome, parentHome); err != nil {
			return fmt.Errorf("borrow parent %q for subagent %q: %w", parent, name, err)
		}
		nostrAction = fmt.Sprintf("borrowed parent %q", parent)
	} else {
		_, nostrCreated, loadErr := identity.LoadOrCreate(agentHome)
		if loadErr != nil {
			return fmt.Errorf("issue secp256k1 identity for agent %q: %w", name, loadErr)
		}
		nostrAction = "loaded existing"
		if nostrCreated {
			nostrAction = "generated new"
		}
	}

	// identity.Resolve is the single canonical accessor for "what identity does
	// this home sign as" — it follows a subagent's parent pointer or loads a
	// fleet member's own key, exactly the dispatch performed above by hand.
	// Calling it here (rather than trusting the BorrowParent/LoadOrCreate
	// return values, which happen to hold the same identity) wires Resolve
	// into the one production path that provisions an identity home, so any
	// future signing call site (buy/put/settle under AGENT_CF_HOME, the NIP-42
	// relay handshake) reuses this exact function instead of re-deriving the
	// parent-pointer-vs-own-key dispatch a second time.
	nostrID, err := identity.Resolve(agentHome)
	if err != nil {
		return fmt.Errorf("resolve signing identity for agent %q: %w", name, err)
	}

	// Step 3: print the export line to stdout (for eval) and info to stderr.
	fmt.Printf("export AGENT_CF_HOME=%s\n", agentHome)
	if !jsonOutput {
		fmt.Fprintf(os.Stderr, "agent-init: %s identity for %q\n", nostrAction, name)
		fmt.Fprintf(os.Stderr, "  agent home: %s\n", agentHome)
		fmt.Fprintf(os.Stderr, "  npub:       %s\n", nostrID.Npub())
	}

	return nil
}
