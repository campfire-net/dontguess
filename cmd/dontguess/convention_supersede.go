package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	cfconvention "github.com/campfire-net/campfire/pkg/convention"
	"github.com/campfire-net/campfire/pkg/protocol"
	"github.com/campfire-net/campfire/pkg/store"
	dgconv "github.com/3dl-dev/dontguess/pkg/convention"
	"github.com/spf13/cobra"
)

// supersedeResult is the JSON output for a supersede operation.
type supersedeResult struct {
	File         string                  `json:"file"`
	Operation    string                  `json:"operation,omitempty"`
	OldVersion   string                  `json:"old_version,omitempty"`
	NewVersion   string                  `json:"new_version,omitempty"`
	ChangeKind   dgconv.VersionKind      `json:"change_kind,omitempty"`
	Breaking     []dgconv.BreakingChange `json:"breaking,omitempty"`
	Additions    []string                `json:"additions,omitempty"`
	Deprecations []string                `json:"deprecations,omitempty"`
	MessageID    string                  `json:"message_id,omitempty"`
	SupersedesID string                  `json:"supersedes_id,omitempty"`
	Error        string                  `json:"error,omitempty"`
}

var conventionSupersedeCmd = &cobra.Command{
	Use:   "supersede <new-file> --registry <campfire-id> --supersedes <message-id>",
	Short: "Publish a new convention version, superseding the old one",
	Long: `Publish a new convention declaration version via cf registry supersede.

This command:
  1. Lints the new declaration.
  2. Loads the old declaration (by message-id) from the registry to diff against.
  3. Detects breaking changes (arg removal, required-arg addition, etc).
  4. Validates that the version bump is consistent with the change kind.
  5. Publishes the new declaration with supersedes=<message-id>.

Breaking change policy (from the DontGuess convention spec):
  major version bump  removed/renamed args, required→optional, optional→required
  minor version bump  new optional arg added
  patch version bump  description/rate_limit only

Agents subscribed to the registry campfire see updated operations automatically
via convention registry resolution — no re-joining required.`,
	Args: cobra.ExactArgs(1),
	RunE: runConventionSupersede,
}

var (
	supersedeRegistry   string
	supersedesMessageID string
	supersedeForce      bool
)

func init() {
	conventionSupersedeCmd.Flags().StringVar(&supersedeRegistry, "registry", "", "convention registry campfire ID (required)")
	conventionSupersedeCmd.Flags().StringVar(&supersedesMessageID, "supersedes", "", "message ID of the declaration being replaced (required)")
	conventionSupersedeCmd.Flags().BoolVar(&supersedeForce, "force", false, "skip breaking-change validation and publish anyway")
	_ = conventionSupersedeCmd.MarkFlagRequired("registry")
	_ = conventionSupersedeCmd.MarkFlagRequired("supersedes")
	conventionCmd.AddCommand(conventionSupersedeCmd)
}

func runConventionSupersede(_ *cobra.Command, args []string) error {
	newFilePath := args[0]

	// Read new declaration.
	newPayload, err := os.ReadFile(newFilePath)
	if err != nil {
		return fmt.Errorf("reading %q: %w", newFilePath, err)
	}

	// Initialize protocol client from campfire home.
	client, err := requireClient()
	if err != nil {
		return err
	}
	defer client.Close()

	// Verify membership.
	m, err := client.GetMembership(supersedeRegistry)
	if err != nil {
		return fmt.Errorf("querying membership for registry %s: %w", shortID(supersedeRegistry), err)
	}
	if m == nil {
		return fmt.Errorf("not a member of registry campfire %s — join first", shortID(supersedeRegistry))
	}

	result, err := performSupersede(
		newFilePath, newPayload,
		supersedeRegistry, supersedesMessageID,
		client, m,
		supersedeForce,
	)
	if err != nil {
		return err
	}

	if jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(result)
	}

	if result.Error != "" {
		fmt.Fprintf(os.Stderr, "  FAIL  %s: %s\n", result.File, result.Error)
		return fmt.Errorf("supersede failed")
	}

	fmt.Fprintf(os.Stdout, "  ok    %s %s → %s (%s) → msgID=%s\n",
		result.Operation, result.OldVersion, result.NewVersion,
		result.ChangeKind, shortID(result.MessageID))

	if len(result.Breaking) > 0 {
		fmt.Fprintln(os.Stdout, "  note  breaking changes (--force was used):")
		for _, b := range result.Breaking {
			fmt.Fprintf(os.Stdout, "        %s: %s\n", b.Kind, b.Detail)
		}
	}
	if len(result.Additions) > 0 {
		fmt.Fprintf(os.Stdout, "  note  new optional args: %v\n", result.Additions)
	}
	return nil
}

// performSupersede is the testable core of the supersede operation.
// It uses client for all campfire I/O: old message lookup, membership check,
// and publishing the new declaration.
func performSupersede(
	newFilePath string,
	newPayload []byte,
	registryID string,
	oldMessageID string,
	client *protocol.Client,
	m *store.Membership,
	force bool,
) (*supersedeResult, error) {
	result := &supersedeResult{
		File:         newFilePath,
		SupersedesID: oldMessageID,
	}

	// Step 0: Operator identity check — only the campfire creator may supersede
	// conventions. m.CreatorPubkey is set when joining and identifies the
	// operator. Any other campfire member that attempts to supersede is rejected
	// immediately, before any store or network I/O.
	if m.CreatorPubkey != "" && client.PublicKeyHex() != m.CreatorPubkey {
		result.Error = fmt.Sprintf("operator check failed: only the operator (%s) may supersede conventions", shortID(m.CreatorPubkey))
		return result, nil
	}

	// Step 1: Lint the new declaration.
	lintResult := cfconvention.Lint(newPayload)
	if len(lintResult.Errors) > 0 {
		result.Error = fmt.Sprintf("lint failed: %s", lintResult.Errors[0].Message)
		return result, nil
	}

	// Step 2: Parse the new declaration to get metadata.
	newDecl, _, err := cfconvention.Parse(
		[]string{cfconvention.ConventionOperationTag},
		newPayload,
		client.PublicKeyHex(),
		client.PublicKeyHex(),
	)
	if err != nil {
		result.Error = fmt.Sprintf("parse failed: %s", err)
		return result, nil
	}
	result.Operation = newDecl.Operation
	result.NewVersion = newDecl.Version

	// Step 3: Load the old declaration via client.Get / client.GetByPrefix.
	oldMsg, err := client.Get(oldMessageID)
	if err != nil || oldMsg == nil {
		// Try prefix match for short IDs.
		oldMsg, err = client.GetByPrefix(oldMessageID)
		if err != nil || oldMsg == nil {
			result.Error = fmt.Sprintf("old declaration message %s not found in registry — have you synced?", shortID(oldMessageID))
			return result, nil
		}
	}
	oldPayload := oldMsg.Payload

	// Step 4: Parse old declaration for metadata.
	oldDecl, _, err := cfconvention.Parse(
		[]string{cfconvention.ConventionOperationTag},
		oldPayload,
		oldMsg.Sender,
		"",
	)
	if err != nil {
		result.Error = fmt.Sprintf("parsing old declaration: %s", err)
		return result, nil
	}
	result.OldVersion = oldDecl.Version

	// Step 5: Diff.
	diff, err := dgconv.Diff(oldPayload, newPayload)
	if err != nil {
		result.Error = fmt.Sprintf("diff failed: %s", err)
		return result, nil
	}
	result.ChangeKind = diff.Kind
	result.Breaking = diff.Breaking
	result.Additions = diff.Additions
	result.Deprecations = diff.Deprecations

	// Step 6: Validate version bump (unless --force).
	if !force {
		vErrs := dgconv.ValidateVersionBump(diff)
		if len(vErrs) > 0 {
			result.Error = fmt.Sprintf("version bump invalid: %s (use --force to override)", vErrs[0])
			return result, nil
		}
		// Block breaking changes without explicit --force.
		if len(diff.Breaking) > 0 {
			result.Error = fmt.Sprintf("breaking changes detected (--force required): %s", diff.Breaking[0].Detail)
			return result, nil
		}
	}

	// Step 7: Inject the supersedes field into the new payload.
	supersededPayload, err := injectSupersedes(newPayload, oldMessageID)
	if err != nil {
		result.Error = fmt.Sprintf("injecting supersedes field: %s", err)
		return result, nil
	}

	// Step 8: Publish via protocol client.
	msgID, err := sendSupersede(supersededPayload, registryID, client)
	if err != nil {
		result.Error = fmt.Sprintf("send failed: %s", err)
		return result, nil
	}
	result.MessageID = msgID
	return result, nil
}

// injectSupersedes adds or overwrites the "supersedes" field in a JSON
// convention declaration payload.
func injectSupersedes(payload []byte, supersedes string) ([]byte, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(payload, &raw); err != nil {
		return nil, fmt.Errorf("unmarshaling payload: %w", err)
	}
	supJSON, err := json.Marshal(supersedes)
	if err != nil {
		return nil, err
	}
	raw["supersedes"] = supJSON
	return json.Marshal(raw)
}

// sendSupersede publishes the superseding declaration via the protocol client.
// The client handles transport selection, identity signing, and local store persistence.
func sendSupersede(payload []byte, campfireID string, client *protocol.Client) (string, error) {
	msg, err := client.Send(protocol.SendRequest{
		CampfireID: campfireID,
		Payload:    payload,
		Tags:       []string{cfconvention.ConventionOperationTag},
	})
	if err != nil {
		return "", fmt.Errorf("sending supersede message: %w", err)
	}
	return msg.ID, nil
}

// requireClient initializes a protocol.Client from the campfire home directory.
// Uses CF_HOME env var or ~/.campfire as the campfire home directory.
func requireClient() (*protocol.Client, error) {
	home := cfHome()
	client, err := protocol.Init(home)
	if err != nil {
		return nil, fmt.Errorf("initializing campfire client from %s: %w", home, err)
	}
	return client, nil
}

// cfHome returns the campfire home directory.
func cfHome() string {
	if env := os.Getenv("CF_HOME"); env != "" {
		return env
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: cannot determine home directory: %v\n", err)
		os.Exit(1)
	}
	return filepath.Join(home, ".campfire")
}

// shortID returns the first 12 characters of a message or campfire ID.
func shortID(id string) string {
	if len(id) <= 12 {
		return id
	}
	return id[:12]
}

// listOperationsForRegistry is a thin wrapper around convention.ListOperations
// used by tests.
// TODO(tech-debt): convention.ListOperations takes StoreReader; no Client variant
// exists yet. This adapter bridges store.Store -> StoreReader until cf adds a
// Client-native ListOperations. Track in dontguess-831.
func listOperationsForRegistry(s store.Store, campfireID string) ([]*cfconvention.Declaration, error) {
	return cfconvention.ListOperations(context.Background(), cliStoreReader{s}, campfireID, "")
}

// cliStoreReader adapts store.Store to convention.StoreReader.
type cliStoreReader struct {
	store.Store
}

func (r cliStoreReader) ListMessages(campfireID string, afterTimestamp int64, filter ...store.MessageFilter) ([]store.MessageRecord, error) {
	return r.Store.ListMessages(campfireID, afterTimestamp, filter...)
}
