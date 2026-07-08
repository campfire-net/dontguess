package identity

import (
	"os"
	"path/filepath"
	"testing"
)

// TestBorrowParent_ReusesParentKey proves a subagent home borrows the parent's
// key rather than minting its own: same npub/pubkey, no IdentityFile in the
// subagent home, and Resolve follows the pointer back to the parent.
func TestBorrowParent_ReusesParentKey(t *testing.T) {
	t.Parallel()

	parentDir := t.TempDir()
	parentID, _, err := LoadOrCreate(parentDir)
	if err != nil {
		t.Fatalf("provision parent: %v", err)
	}

	subDir := t.TempDir()
	borrowed, err := BorrowParent(subDir, parentDir)
	if err != nil {
		t.Fatalf("BorrowParent: %v", err)
	}
	if borrowed.Npub() != parentID.Npub() {
		t.Fatalf("borrowed npub %s != parent npub %s", borrowed.Npub(), parentID.Npub())
	}

	// No independent key minted in the subagent home.
	if _, err := os.Stat(filepath.Join(subDir, IdentityFile)); !os.IsNotExist(err) {
		t.Fatalf("subagent home has an independent %s (err=%v) — expected none", IdentityFile, err)
	}
	// A parent pointer must exist.
	if _, err := os.Stat(filepath.Join(subDir, ParentFile)); err != nil {
		t.Fatalf("parent pointer missing: %v", err)
	}

	// Resolve on the subagent home returns the parent identity.
	resolved, err := Resolve(subDir)
	if err != nil {
		t.Fatalf("Resolve subagent: %v", err)
	}
	if resolved.PubKeyHex() != parentID.PubKeyHex() {
		t.Fatalf("resolved pubkey %s != parent pubkey %s", resolved.PubKeyHex(), parentID.PubKeyHex())
	}

	// Resolve on a plain fleet-member home returns its own key.
	resolvedParent, err := Resolve(parentDir)
	if err != nil {
		t.Fatalf("Resolve parent: %v", err)
	}
	if resolvedParent.Npub() != parentID.Npub() {
		t.Fatalf("Resolve(parent) npub %s != %s", resolvedParent.Npub(), parentID.Npub())
	}
}

// TestBorrowParent_RejectsMissingParent proves BorrowParent never silently
// mints a key for an absent parent — the parent must be a real fleet member.
func TestBorrowParent_RejectsMissingParent(t *testing.T) {
	t.Parallel()

	subDir := t.TempDir()
	if _, err := BorrowParent(subDir, filepath.Join(t.TempDir(), "no-such-parent")); err == nil {
		t.Fatal("BorrowParent from a nonexistent parent: expected error, got nil")
	}
	// Nothing may have been written to the subagent home.
	if _, err := os.Stat(filepath.Join(subDir, ParentFile)); err == nil {
		t.Fatal("parent pointer written despite missing parent")
	}
}

// TestBorrowParent_RejectsShadowingOwnKey proves a home that already minted its
// own persistent key cannot also become a subagent — a member is not a subagent.
func TestBorrowParent_RejectsShadowingOwnKey(t *testing.T) {
	t.Parallel()

	parentDir := t.TempDir()
	if _, _, err := LoadOrCreate(parentDir); err != nil {
		t.Fatalf("provision parent: %v", err)
	}

	// The 'subagent' home already has its own minted identity.
	minted := t.TempDir()
	if _, _, err := LoadOrCreate(minted); err != nil {
		t.Fatalf("mint own identity: %v", err)
	}
	if _, err := BorrowParent(minted, parentDir); err == nil {
		t.Fatal("BorrowParent into a home with its own identity: expected error, got nil")
	}
}

// TestResolve_RejectsSwappedParentKey proves Resolve refuses to sign under a
// parent whose key was swapped out from under a stale pointer.
func TestResolve_RejectsSwappedParentKey(t *testing.T) {
	t.Parallel()

	parentDir := t.TempDir()
	if _, _, err := LoadOrCreate(parentDir); err != nil {
		t.Fatalf("provision parent: %v", err)
	}
	subDir := t.TempDir()
	if _, err := BorrowParent(subDir, parentDir); err != nil {
		t.Fatalf("BorrowParent: %v", err)
	}

	// Swap the parent key: remove and regenerate a different identity.
	if err := os.Remove(filepath.Join(parentDir, IdentityFile)); err != nil {
		t.Fatalf("remove parent identity: %v", err)
	}
	if _, _, err := LoadOrCreate(parentDir); err != nil {
		t.Fatalf("regenerate parent identity: %v", err)
	}
	if _, err := Resolve(subDir); err == nil {
		t.Fatal("Resolve with a swapped parent key: expected npub-mismatch error, got nil")
	}
}
