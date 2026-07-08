package identity

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// IdentityFile is the on-disk name of a secp256k1 nostr identity within an
// identity home directory (operator home or a per-agent home under agents/).
const IdentityFile = "nostr-identity.json"

// KeyKind labels the identity scheme on disk so a future re-key is
// self-describing and a mismatched file is rejected loudly rather than
// misinterpreted.
const KeyKind = "secp256k1-schnorr"

// ParentFile is the on-disk name of an ephemeral subagent's parent pointer. A
// subagent home holds THIS file instead of an IdentityFile: it names the parent
// fleet member whose persistent npub the subagent signs under. No private key
// material lives in a subagent home — that is the whole point (a fresh mint per
// subagent would inflate convergence independence and destroy reputation
// continuity; see the key-management ruling in nostr-first-rebuild-decision.md
// and convergence-sybil-defense.md).
const ParentFile = "nostr-parent.json"

// ParentKind labels the parent-pointer file so a mismatched or hand-edited file
// is rejected loudly rather than misinterpreted as an identity.
const ParentKind = "secp256k1-parent-ref"

// parentPointer is the JSON shape of a subagent's parent pointer. ParentDir is
// the identity home of the parent fleet member; ParentNpub is recorded for
// audit and re-verified against the parent key on load so a moved or swapped
// parent key cannot silently change who the subagent signs as.
type parentPointer struct {
	Kind       string `json:"kind"`
	ParentDir  string `json:"parent_dir"`
	ParentNpub string `json:"parent_npub"`
}

// persisted is the JSON shape written to disk. PubKeyHex and Npub are
// derivable from PrivKeyHex; they are stored for human/debug convenience and
// re-verified against the private key on load so a hand-edited file cannot
// desync the recorded npub from the actual signing key.
type persisted struct {
	Kind       string `json:"kind"`
	PrivKeyHex string `json:"priv_key_hex"`
	PubKeyHex  string `json:"pub_key_hex"`
	Npub       string `json:"npub"`
}

// LoadOrCreate loads the secp256k1 identity from <dir>/nostr-identity.json,
// generating and persisting a fresh one if the file does not yet exist. The
// returned bool is true when a new identity was created.
//
// This is the single provisioning path agent-init uses. Idempotency is the
// whole point: a fleet member's npub is persistent, so a re-run loads the same
// key rather than minting a new one (the key-management ruling: never a fresh
// throwaway for a persistent member).
func LoadOrCreate(dir string) (*Secp256k1Identity, bool, error) {
	path := filepath.Join(dir, IdentityFile)
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		id, loadErr := loadFrom(raw, path)
		return id, false, loadErr
	case os.IsNotExist(err):
		id, createErr := create(dir, path)
		return id, true, createErr
	default:
		return nil, false, fmt.Errorf("read identity file %s: %w", path, err)
	}
}

// Load reads an existing identity, erroring if the file is absent.
func Load(dir string) (*Secp256k1Identity, error) {
	path := filepath.Join(dir, IdentityFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read identity file %s: %w", path, err)
	}
	return loadFrom(raw, path)
}

func loadFrom(raw []byte, path string) (*Secp256k1Identity, error) {
	var p persisted
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("parse identity file %s: %w", path, err)
	}
	if p.Kind != KeyKind {
		return nil, fmt.Errorf("identity file %s has kind %q, expected %q", path, p.Kind, KeyKind)
	}
	id, err := FromPrivHex(p.PrivKeyHex)
	if err != nil {
		return nil, fmt.Errorf("identity file %s: %w", path, err)
	}
	// Integrity check: the recorded pubkey/npub must match the derived key. A
	// mismatch means the file was tampered with or corrupted — refuse to sign
	// with a key whose recorded identity is a lie.
	if got := id.PubKeyHex(); got != p.PubKeyHex {
		return nil, fmt.Errorf("identity file %s: recorded pub_key_hex %q does not match private key (%q)", path, p.PubKeyHex, got)
	}
	if got := id.Npub(); got != p.Npub {
		return nil, fmt.Errorf("identity file %s: recorded npub %q does not match private key (%q)", path, p.Npub, got)
	}
	return id, nil
}

func create(dir, path string) (*Secp256k1Identity, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("create identity dir %s: %w", dir, err)
	}
	id, err := Generate()
	if err != nil {
		return nil, err
	}
	if err := Save(dir, id); err != nil {
		return nil, err
	}
	_ = path
	return id, nil
}

// Save writes the identity to <dir>/nostr-identity.json with 0600 permissions
// (it holds private key material). It writes atomically via a temp file rename
// so a crash mid-write cannot leave a half-written key file.
func Save(dir string, id *Secp256k1Identity) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create identity dir %s: %w", dir, err)
	}
	p := persisted{
		Kind:       KeyKind,
		PrivKeyHex: id.PrivHex(),
		PubKeyHex:  id.PubKeyHex(),
		Npub:       id.Npub(),
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal identity: %w", err)
	}
	path := filepath.Join(dir, IdentityFile)
	tmp, err := os.CreateTemp(dir, IdentityFile+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp identity file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op if the rename succeeded
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod temp identity file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp identity file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp identity file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename identity file into place: %w", err)
	}
	return nil
}

// BorrowParent provisions <dir> as an ephemeral subagent that signs under the
// parent fleet member whose identity lives at parentDir. It writes a
// parent-pointer file into <dir> and returns the PARENT's identity. It never
// mints a new key: a subagent has no npub of its own, so convergence
// independence stays scored at fleet-member (parent-npub) granularity.
//
// Preconditions enforced here (the Sybil defense):
//   - parentDir must hold a real persistent identity (IdentityFile). A parent
//     that is itself a subagent (only a ParentFile, no key) fails Load — no
//     borrowing chains, and the operator/fleet-member distinction stays crisp.
//   - <dir> must not already hold its own minted IdentityFile — a subagent that
//     also had an independent key would inflate convergence independence.
func BorrowParent(dir, parentDir string) (*Secp256k1Identity, error) {
	// The parent must be a provisioned fleet member with a persistent npub. Load
	// (not LoadOrCreate) so a missing/absent parent is a hard error rather than
	// silently minting one.
	parentID, err := Load(parentDir)
	if err != nil {
		return nil, fmt.Errorf("borrow parent identity from %s: %w", parentDir, err)
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("create subagent dir %s: %w", dir, err)
	}
	// Refuse to shadow an independent minted key with a parent pointer: a home
	// that already holds its own identity is a fleet member, not a subagent.
	if _, statErr := os.Stat(filepath.Join(dir, IdentityFile)); statErr == nil {
		return nil, fmt.Errorf("home %s already holds an independent identity (%s); a subagent must not also mint its own npub", dir, IdentityFile)
	}
	p := parentPointer{
		Kind:       ParentKind,
		ParentDir:  parentDir,
		ParentNpub: parentID.Npub(),
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal parent pointer: %w", err)
	}
	path := filepath.Join(dir, ParentFile)
	tmp, err := os.CreateTemp(dir, ParentFile+".tmp-*")
	if err != nil {
		return nil, fmt.Errorf("create temp parent pointer: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op if the rename succeeded
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return nil, fmt.Errorf("chmod temp parent pointer: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return nil, fmt.Errorf("write temp parent pointer: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return nil, fmt.Errorf("close temp parent pointer: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return nil, fmt.Errorf("rename parent pointer into place: %w", err)
	}
	return parentID, nil
}

// Resolve returns the signing identity for an identity home, following a parent
// pointer when the home is an ephemeral subagent. A fleet member has its own
// IdentityFile; a subagent has a ParentFile pointing at its parent. The parent
// pointer takes precedence — a subagent NEVER signs under a key minted in its
// own home. Convergence is thus scored at the parent's npub, exactly as the
// key-management ruling requires.
func Resolve(dir string) (*Secp256k1Identity, error) {
	ppath := filepath.Join(dir, ParentFile)
	raw, err := os.ReadFile(ppath)
	switch {
	case err == nil:
		var p parentPointer
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, fmt.Errorf("parse parent pointer %s: %w", ppath, err)
		}
		if p.Kind != ParentKind {
			return nil, fmt.Errorf("parent pointer %s has kind %q, expected %q", ppath, p.Kind, ParentKind)
		}
		id, err := Load(p.ParentDir)
		if err != nil {
			return nil, fmt.Errorf("resolve parent identity for subagent %s: %w", dir, err)
		}
		// Integrity: the recorded parent npub must still match the parent key. A
		// mismatch means the parent key was swapped — refuse to sign as a
		// different identity than the one the pointer was created against.
		if got := id.Npub(); got != p.ParentNpub {
			return nil, fmt.Errorf("parent pointer %s: recorded npub %q no longer matches parent key (%q)", ppath, p.ParentNpub, got)
		}
		return id, nil
	case os.IsNotExist(err):
		// Not a subagent — a fleet member with its own persistent key.
		return Load(dir)
	default:
		return nil, fmt.Errorf("read parent pointer %s: %w", ppath, err)
	}
}
