package main

// agent_init.go — dontguess agent-init <name>
//
// Provisions a per-agent Ed25519 identity under $DG_HOME/agents/<name>/.
// Admits the new pubkey to the exchange campfire (operator action) and joins.
// Prints "export AGENT_CF_HOME=<path>" for the user to eval.
//
// Idempotent: re-running with the same name loads the existing identity,
// skips re-generation and re-admit, and prints the export line again.
//
// Steps (§4.3 and §7 of docs/design/exchange-per-agent-identity-decision.md):
//  1. Create $DG_HOME/agents/<name>/ if needed.
//  2. Call protocol.Init(agentHome) — generates identity.json if absent, loads if present.
//     InitResult.IdentityCreated distinguishes new vs. existing.
//  3. As operator (protocol.Init(dgHome)), call client.Admit with the agent's pubkey.
//     Idempotency: Admit writes a member file; re-running is safe (file already exists).
//  4. Read the operator's stored TransportDir for the exchange campfire.
//  5. Call agentClient.Join via FilesystemTransport using that dir.
//     Idempotency: Join skips the write when the agent is already a member on disk.
//  6. Print "export AGENT_CF_HOME=<agentHome>".

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/campfire-net/campfire/cf-protocol/protocol"
	"github.com/campfire-net/dontguess/pkg/exchange"
	"github.com/spf13/cobra"
)

var agentInitCmd = &cobra.Command{
	Use:   "agent-init <name>",
	Short: "Provision a per-agent identity and admit it to the exchange",
	Long: `Create a per-agent Ed25519 identity under $DG_HOME/agents/<name>/.
Admits the new pubkey to the exchange campfire (operator action) and joins.
Prints the export AGENT_CF_HOME line for the user to eval.

Idempotent: re-running with the same name does not regenerate the identity
or double-admit. The export line is printed regardless.

  dontguess agent-init alice
  eval $(dontguess agent-init alice)   # sets AGENT_CF_HOME in current shell

Requires a running or initialized exchange (dontguess init must have been run).`,
	Args: cobra.ExactArgs(1),
	RunE: runAgentInit,
}

func init() {
	rootCmd.AddCommand(agentInitCmd)
}

func runAgentInit(_ *cobra.Command, args []string) error {
	name := args[0]
	if strings.ContainsAny(name, "/\\") {
		return fmt.Errorf("agent name must not contain path separators")
	}

	dgHome := resolveDGHome()

	// Load exchange config — we need ExchangeCampfireID.
	cfg, err := exchange.LoadConfig(dgHome)
	if err != nil {
		return fmt.Errorf("load exchange config (run dontguess init first): %w", err)
	}

	// Step 1: create the agent home directory.
	agentHome := filepath.Join(dgHome, "agents", name)
	if err := os.MkdirAll(agentHome, 0700); err != nil {
		return fmt.Errorf("creating agent home %s: %w", agentHome, err)
	}

	// Step 2: generate (or load) the agent identity via protocol.Init.
	// If identity.json already exists it is loaded; otherwise a new keypair is generated.
	agentClient, agentResult, err := protocol.Init(agentHome)
	if err != nil {
		return fmt.Errorf("protocol.Init for agent %q: %w", name, err)
	}
	defer agentClient.Close()

	agentPubKey := agentClient.PublicKeyHex()
	action := "loaded existing"
	if agentResult.IdentityCreated {
		action = "generated new"
	}

	// Step 3: as operator, admit the agent's pubkey to the exchange campfire.
	// The operator client reads identity from DG_HOME (the operator key).
	// Admit writes a member file to the filesystem transport; re-admit is idempotent.
	operatorClient, _, err := protocol.Init(dgHome)
	if err != nil {
		return fmt.Errorf("protocol.Init for operator: %w", err)
	}
	defer operatorClient.Close()

	// Step 4: resolve the filesystem transport dir from the operator's membership.
	// The operator joined (or created) the exchange campfire during dontguess init;
	// its membership record holds the TransportDir we need to pass to Join.
	membership, err := operatorClient.GetMembership(cfg.ExchangeCampfireID)
	if err != nil {
		return fmt.Errorf("get operator membership for exchange campfire: %w", err)
	}
	if membership == nil {
		return fmt.Errorf("operator is not a member of exchange campfire %s (run dontguess init)", cfg.ExchangeCampfireID[:16])
	}
	transportDir := membership.TransportDir

	admitErr := operatorClient.Admit(protocol.AdmitRequest{
		CampfireID:      cfg.ExchangeCampfireID,
		MemberPubKeyHex: agentPubKey,
	})
	if admitErr != nil {
		// Swallow idempotent re-admit ("already a member" or "already admitted").
		if !strings.Contains(admitErr.Error(), "already") {
			return fmt.Errorf("admit agent %q to exchange: %w", name, admitErr)
		}
	}

	// Step 5: agent joins the exchange campfire via filesystem transport.
	// Idempotency: on a second call the agent's store already has a membership
	// record. The campfire SDK returns a UNIQUE constraint error when it tries
	// to insert a duplicate membership row. We treat that as a successful
	// no-op rather than an error.
	_, joinErr := agentClient.Join(protocol.JoinRequest{
		CampfireID: cfg.ExchangeCampfireID,
		Transport:  protocol.FilesystemTransport{Dir: transportDir},
	})
	if joinErr != nil {
		msg := joinErr.Error()
		if !strings.Contains(msg, "already") && !strings.Contains(msg, "UNIQUE constraint") {
			return fmt.Errorf("join exchange campfire as agent %q: %w", name, joinErr)
		}
	}

	// Step 6: print the export line to stdout (for eval) and info to stderr.
	fmt.Printf("export AGENT_CF_HOME=%s\n", agentHome)
	if !jsonOutput {
		fmt.Fprintf(os.Stderr, "agent-init: %s identity for %q\n", action, name)
		fmt.Fprintf(os.Stderr, "  agent home: %s\n", agentHome)
		fmt.Fprintf(os.Stderr, "  pubkey:     %s\n", agentPubKey)
		fmt.Fprintf(os.Stderr, "  exchange:   %s...\n", cfg.ExchangeCampfireID[:16])
	}

	return nil
}
