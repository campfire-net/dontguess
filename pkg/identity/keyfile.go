package identity

// keyfile.go — atomic create-or-load for on-disk operator key material
// (dontguess-ed5, docs/design/nostr-admission-scrip-rehome-3b8.md §5).
//
// The bug this kills: `init` and `serve` both did
// ReadFile→IsNotExist→Generate→WriteFile with no atomicity. On a pristine
// DG_HOME, two concurrent first-runs each Generate a DISTINCT key and race
// WriteFile — last-writer-wins on disk while each caller keeps its own
// in-memory key. `init` then advertises pubkey A in config while `serve` signs
// with the on-disk key B => the relay write-allowlist admit keyed to A is
// orphaned and the scrip ledger silently drops the operator's own messages.
//
// A bare O_EXCL create does NOT fix this: O_EXCL makes a ZERO-length file, then
// the hex is written in a SECOND syscall. An EEXIST loser can read that empty
// or half-written file and FromPrivHex a truncated-but-valid-length prefix —
// a DIFFERENT valid key, i.e. the exact advertise!=sign split. The window is
// real.
//
// The fix (per §5): elect a single winner atomically AND publish full content
// atomically, so the final file is created exactly once and is NEVER
// observably present-but-empty:
//   - Winner: write the key to a temp file in the same dir → fsync → publish it
//     onto the final name in one atomic step via os.Link (hard link). Link
//     fails with EEXIST if the final name already exists, so the file is
//     created exactly once, always with complete content.
//   - Loser (EEXIST — lost the race): the winner's Link already published full
//     content atomically, so bounded-retry-read the final file until a
//     validated key appears, then return the WINNER's key (never the loser's
//     discarded candidate) so every caller converges on one identity.
//
// os.Link realizes the design's "atomic rename / RENAME_NOREPLACE, EEXIST on
// lost-the-rename" portably: unlike os.Rename it never overwrites, so two
// winners can't clobber each other, and unlike O_EXCL the published file is
// never zero-length.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// keyfileReadRetries / keyfileReadDelay bound the loser's retry-read of the
// winner's freshly-published key. The winner publishes full content in a single
// atomic os.Link, so in practice the key is already present the instant a loser
// sees EEXIST; the retry loop only tolerates any residual scheduling window and
// then fails closed rather than spin forever.
const (
	keyfileReadRetries = 200
	keyfileReadDelay   = 5 * time.Millisecond
)

// LoadOrCreatePrivHexKey returns the persisted secp256k1 (nostr) operator
// identity at path, atomically minting one on first run. Concurrency-safe: any
// number of racing callers all return the SAME identity (the one that won the
// create) and no caller ever parses a torn/empty file. The key is stored as
// raw 32-byte lowercase hex at 0600 (the DEPLOYED format — NOT identity.Save's
// JSON, which is last-writer-wins and would break deployed keys) and is never
// overwritten once present.
func LoadOrCreatePrivHexKey(path string) (*Secp256k1Identity, error) {
	privHex, err := loadOrCreateKeyFile(path,
		func() (string, error) {
			id, gerr := Generate()
			if gerr != nil {
				return "", gerr
			}
			return id.PrivHex(), nil
		},
		func(s string) error {
			_, perr := FromPrivHex(s)
			return perr
		},
	)
	if err != nil {
		return nil, err
	}
	return FromPrivHex(privHex)
}

// LoadOrCreateRawKey returns the opaque key string persisted at path, atomically
// minting one via genFn on first run. Same concurrency guarantee as
// LoadOrCreatePrivHexKey but for a non-secp256k1 identifier (the local operator
// key): genFn supplies fresh key material and validity is simply non-emptiness.
func LoadOrCreateRawKey(path string, genFn func() (string, error)) (string, error) {
	return loadOrCreateKeyFile(path, genFn, func(s string) error {
		if s == "" {
			return errors.New("empty key")
		}
		return nil
	})
}

// loadOrCreateKeyFile is the shared atomic create-or-load primitive. It returns
// the validated key string persisted at path, minting one with gen() on first
// run. validate reports whether a candidate/on-disk value is a usable key.
func loadOrCreateKeyFile(path string, gen func() (string, error), validate func(string) error) (string, error) {
	// Fast path: a valid key already exists — reuse it verbatim, never re-mint.
	if key, ok, err := readValidKey(path, validate); err != nil {
		return "", err
	} else if ok {
		return key, nil
	}

	// Generate a candidate and stage it in a FULLY-written temp file in the same
	// directory (same filesystem, so os.Link is atomic).
	candidate, err := gen()
	if err != nil {
		return "", fmt.Errorf("generating key for %s: %w", path, err)
	}
	if verr := validate(candidate); verr != nil {
		return "", fmt.Errorf("generated key for %s failed validation: %w", path, verr)
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".keyfile-*.tmp")
	if err != nil {
		return "", fmt.Errorf("creating temp key file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	// Remove the temp on every exit: after a successful Link the inode persists
	// under the final name (unlink of the temp link is harmless); on the loser
	// and error paths this cleans up the orphan.
	defer os.Remove(tmpName) //nolint:errcheck

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("chmod temp key file %s: %w", tmpName, err)
	}
	if _, err := tmp.WriteString(candidate + "\n"); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("writing temp key file %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("fsync temp key file %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("closing temp key file %s: %w", tmpName, err)
	}

	// Publish atomically. os.Link makes the fully-written temp appear at `path`
	// in a single step and fails with EEXIST if a concurrent caller already won,
	// so `path` is created exactly once, with complete content, never empty and
	// never overwritten.
	if err := os.Link(tmpName, path); err == nil {
		return candidate, nil // winner: our candidate is the durable key
	} else if !errors.Is(err, os.ErrExist) {
		return "", fmt.Errorf("publishing key file %s: %w", path, err)
	}

	// Loser: a concurrent caller won. Its Link published full content
	// atomically, so bounded-retry-read the final file and return the WINNER's
	// key (never our discarded candidate) so all callers converge.
	for i := 0; i < keyfileReadRetries; i++ {
		if key, ok, rerr := readValidKey(path, validate); rerr != nil {
			return "", rerr
		} else if ok {
			return key, nil
		}
		time.Sleep(keyfileReadDelay)
	}
	return "", fmt.Errorf("key file %s exists but no valid key appeared after %d retries", path, keyfileReadRetries)
}

// readValidKey reads path and returns (key, true, nil) iff it holds a validated
// key. A missing file, or a present-but-not-yet-valid one (empty/torn/partial),
// returns (_, false, nil) so the caller can create or retry. Only a genuine IO
// error other than ENOENT is returned as err — a present-but-invalid file is
// treated as "not ready" so a torn read never propagates as a hard parse error.
//
// dontguess-973 C3: every load of an existing key file (both the fast path and
// the loser's retry-read) verifies the on-disk mode is not group/other
// accessible before trusting its content. This file is written 0600 at
// creation time (Chmod above), but "written 0600 once" is not the same
// guarantee as "still 0600 now" — a hand copy, a permissive umask on
// restore/import, or a loosened backup can widen it later. A widened
// permission bit is treated as a hard load error, not a warning: this is
// private key material.
func readValidKey(path string, validate func(string) error) (string, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("reading key file %s: %w", path, err)
	}
	key := strings.TrimSpace(string(data))
	if key == "" {
		return "", false, nil
	}
	if validate(key) != nil {
		return "", false, nil
	}
	if err := CheckKeyFilePermissions(path); err != nil {
		return "", false, err
	}
	return key, true, nil
}

// CheckKeyFilePermissions fails loud if path is readable or writable by
// group/other. Exported so non-keyfile.go load paths that read operator key
// material directly (e.g. cmd/dontguess/mintauth.go's load-only
// loadOperatorSigner) can apply the same check without going through
// LoadOrCreatePrivHexKey.
func CheckKeyFilePermissions(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat key file %s: %w", path, err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf(
			"key file %s has permissions %#o (group/other accessible) — private key material must be 0600; run `chmod 0600 %s` and retry",
			path, perm, path,
		)
	}
	return nil
}
