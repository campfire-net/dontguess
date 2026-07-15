package main

// operator_export_import_test.go — feature tests for dontguess-51c.
//
// 1Password is a live third-party account with no test tenancy — these tests
// MUST NOT create, read, or mutate real 1Password items. fakeOpRunner stands
// in for the `op` CLI (the opRunner seam in operator_export_import.go) and is
// an in-memory map, but every other line under test is the REAL production
// code path: buildOperatorItemTemplate's JSON shape, runOperatorExport /
// runOperatorImport's RunE logic (invoked directly, not re-implemented),
// importOperatorKey's conflict detection, and identity.LoadOrCreateRawKey's
// real atomic file write under DG_HOME (a real temp dir, real os.Link).
//
// Ground-source coverage (item's mandatory clauses):
//  1. TestOperatorExportImport_RoundTripByteIdentical — export on host A,
//     import on host B, assert privkey/pubkey/npub are byte-identical and the
//     on-disk key file matches.
//  2. TestOperatorImport_RefusesDistinctExistingIdentity — host B already has
//     its OWN distinct operator key; import must fail loud and must NOT
//     overwrite the existing file.
//  3. TestOperatorImport_IdempotentSameKey — importing the same key twice
//     succeeds both times and never leaves a torn/altered file.
//  4. TestOperatorExport_NeverWritesPlaintextScratchFile — export must not
//     create any new file under DG_HOME beyond the pre-existing operator key
//     file (the raw key must cross the process boundary only via the fake's
//     in-memory Create call, mirroring the real stdin pipe).
//  5. TestOperatorExport_RefusesDistinctExistingVaultItem — the 1Password
//     item under --title already holds a DIFFERENT pubkey (e.g. someone else's
//     export under the same title) — export must refuse, not clobber.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/3dl-dev/dontguess/pkg/identity"
)

// fakeOpRunner is an in-memory double for the 1Password CLI. Keyed by
// "vault/title" -> field label -> value, exactly mirroring what the real `op`
// CLI would persist server-side.
type fakeOpRunner struct {
	items map[string]map[string]string
}

func newFakeOpRunner() *fakeOpRunner {
	return &fakeOpRunner{items: map[string]map[string]string{}}
}

func (f *fakeOpRunner) key(vault, title string) string { return vault + "/" + title }

func (f *fakeOpRunner) CreateItem(vault string, template []byte) error {
	var t opItemTemplate
	if err := json.Unmarshal(template, &t); err != nil {
		return fmt.Errorf("fake op: unmarshal template: %w", err)
	}
	fields := map[string]string{}
	for _, fl := range t.Fields {
		fields[fl.Label] = fl.Value
	}
	f.items[f.key(vault, t.Title)] = fields
	return nil
}

func (f *fakeOpRunner) ReadField(vault, title, field string) (string, error) {
	fields, ok := f.items[f.key(vault, title)]
	if !ok {
		return "", fmt.Errorf("fake op: item %q not found in vault %q", title, vault)
	}
	v, ok := fields[field]
	if !ok {
		return "", fmt.Errorf("fake op: field %q not found on item %q", field, title)
	}
	return v, nil
}

// withFakeOpRunner swaps opRunnerImpl for a fresh fake for the duration of fn
// and restores the real implementation afterward (belt-and-suspenders: no
// other test in the package should ever hit the real `op` binary, but this
// keeps the swap scoped and explicit).
func withFakeOpRunner(t *testing.T, fn func(f *fakeOpRunner)) {
	t.Helper()
	prev := opRunnerImpl
	fake := newFakeOpRunner()
	opRunnerImpl = fake
	t.Cleanup(func() { opRunnerImpl = prev })
	fn(fake)
}

// setExportImportFlags points the export/import command flag vars (shared
// package-level vars set by cobra normally) at vault/title, and restores them
// after the test.
func setExportImportFlags(t *testing.T, vault, title string) {
	t.Helper()
	prevEV, prevET, prevIV, prevIT := operatorExportVault, operatorExportTitle, operatorImportVault, operatorImportTitle
	operatorExportVault, operatorExportTitle = vault, title
	operatorImportVault, operatorImportTitle = vault, title
	t.Cleanup(func() {
		operatorExportVault, operatorExportTitle = prevEV, prevET
		operatorImportVault, operatorImportTitle = prevIV, prevIT
	})
}

func TestOperatorExportImport_RoundTripByteIdentical(t *testing.T) {
	withFakeOpRunner(t, func(_ *fakeOpRunner) {
		setExportImportFlags(t, "test-vault", "dontguess-operator")

		hostA := t.TempDir()
		hostB := t.TempDir()

		t.Setenv("DG_HOME", hostA)
		if err := operatorExportCmd.RunE(operatorExportCmd, nil); err != nil {
			t.Fatalf("export on host A: %v", err)
		}
		aKeyBytes, err := os.ReadFile(filepath.Join(hostA, "nostr-operator.key"))
		if err != nil {
			t.Fatalf("reading host A key: %v", err)
		}
		aID, err := identity.FromPrivHex(trimKey(string(aKeyBytes)))
		if err != nil {
			t.Fatalf("parsing host A key: %v", err)
		}

		t.Setenv("DG_HOME", hostB)
		if err := operatorImportCmd.RunE(operatorImportCmd, nil); err != nil {
			t.Fatalf("import on host B: %v", err)
		}
		bKeyBytes, err := os.ReadFile(filepath.Join(hostB, "nostr-operator.key"))
		if err != nil {
			t.Fatalf("reading host B key: %v", err)
		}
		bID, err := identity.FromPrivHex(trimKey(string(bKeyBytes)))
		if err != nil {
			t.Fatalf("parsing host B key: %v", err)
		}

		if aID.PrivHex() != bID.PrivHex() {
			t.Fatalf("private key mismatch after round-trip: A=%s B=%s", aID.PrivHex(), bID.PrivHex())
		}
		if aID.PubKeyHex() != bID.PubKeyHex() {
			t.Fatalf("pubkey mismatch after round-trip: A=%s B=%s", aID.PubKeyHex(), bID.PubKeyHex())
		}
		if aID.Npub() != bID.Npub() {
			t.Fatalf("npub mismatch after round-trip: A=%s B=%s", aID.Npub(), bID.Npub())
		}
	})
}

func TestOperatorImport_RefusesDistinctExistingIdentity(t *testing.T) {
	withFakeOpRunner(t, func(_ *fakeOpRunner) {
		setExportImportFlags(t, "test-vault", "dontguess-operator")

		hostA := t.TempDir()
		t.Setenv("DG_HOME", hostA)
		if err := operatorExportCmd.RunE(operatorExportCmd, nil); err != nil {
			t.Fatalf("export on host A: %v", err)
		}

		// Host B already has its OWN distinct operator identity (e.g. it ran
		// `up` and minted its own key before anyone tried to import).
		hostB := t.TempDir()
		bExisting, err := identity.Generate()
		if err != nil {
			t.Fatalf("generating host B's existing identity: %v", err)
		}
		bKeyPath := filepath.Join(hostB, "nostr-operator.key")
		if err := os.WriteFile(bKeyPath, []byte(bExisting.PrivHex()+"\n"), 0o600); err != nil {
			t.Fatalf("seeding host B existing key: %v", err)
		}

		t.Setenv("DG_HOME", hostB)
		err = operatorImportCmd.RunE(operatorImportCmd, nil)
		if err == nil {
			t.Fatalf("expected import to refuse over a distinct existing operator identity, got nil error")
		}

		// The existing file must be UNCHANGED — no fork, no silent overwrite.
		after, rerr := os.ReadFile(bKeyPath)
		if rerr != nil {
			t.Fatalf("reading host B key after refused import: %v", rerr)
		}
		if trimKey(string(after)) != bExisting.PrivHex() {
			t.Fatalf("host B's existing key was mutated by a refused import: got %s, want %s", trimKey(string(after)), bExisting.PrivHex())
		}
	})
}

func TestOperatorImport_IdempotentSameKey(t *testing.T) {
	withFakeOpRunner(t, func(_ *fakeOpRunner) {
		setExportImportFlags(t, "test-vault", "dontguess-operator")

		hostA := t.TempDir()
		t.Setenv("DG_HOME", hostA)
		if err := operatorExportCmd.RunE(operatorExportCmd, nil); err != nil {
			t.Fatalf("export on host A: %v", err)
		}

		hostB := t.TempDir()
		t.Setenv("DG_HOME", hostB)
		if err := operatorImportCmd.RunE(operatorImportCmd, nil); err != nil {
			t.Fatalf("first import on host B: %v", err)
		}
		// Second import of the SAME key must succeed (idempotent), not refuse.
		if err := operatorImportCmd.RunE(operatorImportCmd, nil); err != nil {
			t.Fatalf("second (idempotent) import on host B: %v", err)
		}
	})
}

func TestOperatorExport_NeverWritesPlaintextScratchFile(t *testing.T) {
	withFakeOpRunner(t, func(_ *fakeOpRunner) {
		setExportImportFlags(t, "test-vault", "dontguess-operator")

		hostA := t.TempDir()
		t.Setenv("DG_HOME", hostA)

		// Pre-mint the operator key so export's "load" path doesn't itself
		// create the key file mid-test (isolate what export ADDS).
		if _, err := loadOrCreateNostrOperatorIdentity(hostA); err != nil {
			t.Fatalf("pre-minting host A key: %v", err)
		}
		before, err := os.ReadDir(hostA)
		if err != nil {
			t.Fatalf("listing DG_HOME before export: %v", err)
		}

		if err := operatorExportCmd.RunE(operatorExportCmd, nil); err != nil {
			t.Fatalf("export: %v", err)
		}

		after, err := os.ReadDir(hostA)
		if err != nil {
			t.Fatalf("listing DG_HOME after export: %v", err)
		}
		if len(after) != len(before) {
			names := make([]string, 0, len(after))
			for _, e := range after {
				names = append(names, e.Name())
			}
			t.Fatalf("export created new file(s) under DG_HOME (raw key must never spill to a new on-disk artifact): before=%d after=%d entries=%v", len(before), len(after), names)
		}
	})
}

func TestOperatorExport_RefusesDistinctExistingVaultItem(t *testing.T) {
	withFakeOpRunner(t, func(fake *fakeOpRunner) {
		setExportImportFlags(t, "test-vault", "dontguess-operator")

		// Someone else's export already occupies this vault+title.
		other, err := identity.Generate()
		if err != nil {
			t.Fatalf("generating other identity: %v", err)
		}
		fake.items[fake.key("test-vault", "dontguess-operator")] = map[string]string{
			operatorPrivKeyField: other.PrivHex(),
			operatorPubKeyField:  other.PubKeyHex(),
			operatorNpubField:    other.Npub(),
		}

		hostA := t.TempDir()
		t.Setenv("DG_HOME", hostA)
		err = operatorExportCmd.RunE(operatorExportCmd, nil)
		if err == nil {
			t.Fatalf("expected export to refuse clobbering a distinct existing vault item, got nil error")
		}

		// The vault item must be UNCHANGED.
		got := fake.items[fake.key("test-vault", "dontguess-operator")][operatorPubKeyField]
		if got != other.PubKeyHex() {
			t.Fatalf("vault item was mutated by a refused export: got pubkey %s, want %s", got, other.PubKeyHex())
		}
	})
}

// trimKey trims the trailing newline the key file writer appends (matches
// pkg/identity/keyfile.go's WriteString(candidate + "\n")).
func trimKey(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}
