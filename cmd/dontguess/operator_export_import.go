package main

// operator_export_import.go — `dontguess operator export` / `operator import`,
// 1Password-backed operator key custody for genuine multi-host operation
// (dontguess-51c, docs/design/onboarding-tiered-scaling-federation.md §6, ADV-4).
//
// §6's ruling: FLEET is ONE operator; member machines run `join`, not a second
// `up --relay` that would mint a competing sequencer. The rare genuine
// multi-host operator (the SAME operator process, or its failover twin,
// running on a second machine) needs the SAME secp256k1 private key on both
// hosts. `export` puts that key into 1Password custody; `import` restores it
// on the second host.
//
// Hard constraints (from the item):
//   - Never write the raw private key to disk unencrypted as part of the
//     transfer. The already-persisted $DG_HOME/nostr-operator.key is the
//     existing on-disk custody format (unchanged by this item — HSM-vs-1Password
//     for THAT file is 10-Q4, open). What this file must never do is spill the
//     key into a NEW unencrypted artifact: no scratch file, no shell history
//     (op CLI's own docs warn that command-line assignment statements are
//     visible in process listings and shell history — see op item create --help),
//     no log line. The key crosses the export/import boundary only via an
//     in-memory JSON template piped over the child process's stdin, and via
//     `op read`'s stdout captured directly into memory.
//   - Round-trip byte-identical: the private key hex (and therefore pubkey and
//     npub) imported on host B must be IDENTICAL to what was exported from
//     host A.
//   - Refuse import over a distinct existing operator identity: if host B
//     already has a DIFFERENT operator key on disk, import must fail loud
//     rather than silently fork or overwrite it (this is the ADV-4 fork this
//     item exists to avoid).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/3dl-dev/dontguess/pkg/identity"
	"github.com/spf13/cobra"
)

// --- 1Password backend abstraction ---
//
// opRunner is the seam between the export/import command logic and the actual
// `op` CLI invocation. Production wires execOpRunner (shells out to the real
// 1Password CLI). Tests inject a fake in-memory runner — 1Password is a live
// third-party account with no test tenancy available in CI/sandboxes, so
// nothing in this repo may create or mutate real 1Password items as part of
// automated testing. The fake still exercises every line of the real
// export/import command logic (template construction, JSON round-trip,
// conflict detection, atomic key persistence) — only the external service
// call itself is doubled.
type opRunner interface {
	// CreateItem creates a new 1Password item in vault from the given JSON
	// item template, delivered over the child process's stdin (never a CLI
	// argument, never a temp file).
	CreateItem(vault string, template []byte) error
	// ReadField reads a single field of an existing item via `op read
	// op://vault/title/field`. Returns an error if the item or field does not
	// exist (this is also how callers probe existence — 1Password has no
	// cheaper "does this exist" primitive than attempting a read).
	ReadField(vault, title, field string) (string, error)
}

// execOpRunner shells out to the real 1Password CLI (`op`).
type execOpRunner struct{}

func (execOpRunner) CreateItem(vault string, template []byte) error {
	if _, err := exec.LookPath("op"); err != nil {
		return fmt.Errorf("1Password CLI (op) not found on PATH: %w", err)
	}
	cmd := exec.Command("op", "item", "create", "--vault", vault, "-")
	cmd.Stdin = bytes.NewReader(template)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("op item create: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func (execOpRunner) ReadField(vault, title, field string) (string, error) {
	if _, err := exec.LookPath("op"); err != nil {
		return "", fmt.Errorf("1Password CLI (op) not found on PATH: %w", err)
	}
	ref := fmt.Sprintf("op://%s/%s/%s", vault, title, field)
	var stdout, stderr bytes.Buffer
	cmd := exec.Command("op", "read", ref)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("op read %s: %w: %s", ref, err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// opRunnerImpl is the active backend. Tests swap this for a fake and restore
// it afterward; production never touches it.
var opRunnerImpl opRunner = execOpRunner{}

// --- 1Password item template ---

// opField is one field of a 1Password item JSON template. See
// `op item create --help` §"CREATE AN ITEM USING A JSON TEMPLATE".
type opField struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Purpose string `json:"purpose,omitempty"`
	Label   string `json:"label"`
	Value   string `json:"value"`
}

// opItemTemplate is the minimal 1Password "Password" category item template
// this command creates: one CONCEALED field for the raw private key, two
// STRING fields for the derived pubkey/npub (safe to leave visible — they are
// public identifiers, not secrets) for reference when browsing the vault.
type opItemTemplate struct {
	Title    string    `json:"title"`
	Category string    `json:"category"`
	Fields   []opField `json:"fields"`
}

// operatorPrivKeyField / operatorPubKeyField / operatorNpubField are the
// 1Password item field labels export writes and import reads back. Both
// commands must agree on these names.
const (
	operatorPrivKeyField = "privkey"
	operatorPubKeyField  = "pubkey"
	operatorNpubField    = "npub"
)

func buildOperatorItemTemplate(title, privHex, pubHex, npub string) []byte {
	tmpl := opItemTemplate{
		Title:    title,
		Category: "PASSWORD",
		Fields: []opField{
			{ID: operatorPrivKeyField, Type: "CONCEALED", Purpose: "PASSWORD", Label: operatorPrivKeyField, Value: privHex},
			{ID: operatorPubKeyField, Type: "STRING", Label: operatorPubKeyField, Value: pubHex},
			{ID: operatorNpubField, Type: "STRING", Label: operatorNpubField, Value: npub},
		},
	}
	// Marshal of a fixed, valid struct never fails.
	b, _ := json.Marshal(tmpl)
	return b
}

// --- import-side key persistence ---

// importOperatorKey persists importedPrivHex as dgHome's operator identity,
// refusing if dgHome already holds a DIFFERENT operator identity (ADV-4: this
// is the guard against forking the operator by importing over an
// independently-minted key). Idempotent: importing the SAME key a second time
// succeeds as a no-op. Uses identity.LoadOrCreateRawKey for the actual write —
// the same atomic create-or-load primitive normal operator key minting uses
// (pkg/identity/keyfile.go), so import never produces a torn or
// present-but-empty key file even under a concurrent racer.
func importOperatorKey(dgHome, importedPrivHex string) (*identity.Secp256k1Identity, error) {
	imported, err := identity.FromPrivHex(importedPrivHex)
	if err != nil {
		return nil, fmt.Errorf("imported key material is not a valid secp256k1 private key: %w", err)
	}

	path := filepath.Join(dgHome, "nostr-operator.key")
	existingRaw, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("checking existing operator key at %s: %w", path, err)
		}
		// No existing key file — fall through to persist the imported key.
	} else if existingHex := strings.TrimSpace(string(existingRaw)); existingHex != "" {
		if existingHex == importedPrivHex {
			// Identical key already present — idempotent success, no write needed.
			return imported, nil
		}
		existingID, verr := identity.FromPrivHex(existingHex)
		if verr != nil {
			return nil, fmt.Errorf("refusing import: %s already holds operator key material that is not a valid identity (%v); resolve manually before importing", path, verr)
		}
		return nil, fmt.Errorf(
			"refusing import: this host already has a DISTINCT operator identity at %s (existing npub %s, importing npub %s) — importing would fork the operator (ADV-4); back up/move the existing key first if you really intend to replace it",
			path, existingID.Npub(), imported.Npub(),
		)
	}

	if _, err := identity.LoadOrCreateRawKey(path, func() (string, error) {
		return importedPrivHex, nil
	}); err != nil {
		return nil, fmt.Errorf("persisting imported operator key to %s: %w", path, err)
	}

	// Re-read through FromPrivHex to report exactly what ended up persisted —
	// under a concurrent-writer race LoadOrCreateRawKey returns whichever
	// candidate actually won the atomic publish, which may not be ours.
	persisted, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading back persisted operator key at %s: %w", path, err)
	}
	return identity.FromPrivHex(strings.TrimSpace(string(persisted)))
}

// --- CLI commands ---

var (
	operatorExportVault string
	operatorExportTitle string
	operatorImportVault string
	operatorImportTitle string
)

var operatorExportCmd = &cobra.Command{
	Use:   "export",
	Short: "Export the operator secp256k1 key into 1Password custody (for a genuine 2nd-host operator; §6 ADV-4)",
	RunE: func(cmd *cobra.Command, args []string) error {
		dgHome := resolveDGHome()
		id, err := loadOrCreateNostrOperatorIdentity(dgHome)
		if err != nil {
			return fmt.Errorf("loading operator identity: %w", err)
		}

		if existingPub, rerr := opRunnerImpl.ReadField(operatorExportVault, operatorExportTitle, operatorPubKeyField); rerr == nil {
			if existingPub != id.PubKeyHex() {
				return fmt.Errorf(
					"refusing export: 1Password item %q in vault %q already holds a DIFFERENT operator identity (pubkey %s…) than this host's operator (pubkey %s…) — use a different --title or resolve the conflict before exporting",
					operatorExportTitle, operatorExportVault, short(existingPub), short(id.PubKeyHex()),
				)
			}
			fmt.Printf("operator identity already exported to op://%s/%s (npub %s) — no changes made\n", operatorExportVault, operatorExportTitle, id.Npub())
			return nil
		}

		tmpl := buildOperatorItemTemplate(operatorExportTitle, id.PrivHex(), id.PubKeyHex(), id.Npub())
		if err := opRunnerImpl.CreateItem(operatorExportVault, tmpl); err != nil {
			return fmt.Errorf("exporting operator key to 1Password: %w", err)
		}
		fmt.Printf("exported operator identity (npub %s) to op://%s/%s\n", id.Npub(), operatorExportVault, operatorExportTitle)
		return nil
	},
}

var operatorImportCmd = &cobra.Command{
	Use:   "import",
	Short: "Import the operator secp256k1 key from 1Password custody onto this host (§6 ADV-4)",
	RunE: func(cmd *cobra.Command, args []string) error {
		dgHome := resolveDGHome()

		privHex, err := opRunnerImpl.ReadField(operatorImportVault, operatorImportTitle, operatorPrivKeyField)
		if err != nil {
			return fmt.Errorf("reading operator key from 1Password (op://%s/%s/%s): %w", operatorImportVault, operatorImportTitle, operatorPrivKeyField, err)
		}

		id, err := importOperatorKey(dgHome, privHex)
		if err != nil {
			return err
		}
		fmt.Printf("imported operator identity (npub %s) into %s\n", id.Npub(), filepath.Join(dgHome, "nostr-operator.key"))
		return nil
	},
}

func init() {
	operatorExportCmd.Flags().StringVar(&operatorExportVault, "vault", "", "1Password vault name (required)")
	operatorExportCmd.Flags().StringVar(&operatorExportTitle, "title", "dontguess-operator", "1Password item title")
	_ = operatorExportCmd.MarkFlagRequired("vault")

	operatorImportCmd.Flags().StringVar(&operatorImportVault, "vault", "", "1Password vault name (required)")
	operatorImportCmd.Flags().StringVar(&operatorImportTitle, "title", "dontguess-operator", "1Password item title")
	_ = operatorImportCmd.MarkFlagRequired("vault")

	operatorCmd.AddCommand(operatorExportCmd)
	operatorCmd.AddCommand(operatorImportCmd)
}
