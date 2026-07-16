package main

// serve_redeem_store.go — the DURABLE one-time redeemed-grant-id set (design §1 +
// §9 Gate B/P8, ADV-15). Persisting the set is what makes an invite single-use
// across an operator restart: a replay of a grant id already redeemed is rejected
// even after the process (and its in-memory state) has been torn down and rebuilt.
//
// Format: an append-only, newline-delimited log of redeemed grant ids under
// $DG_HOME. Append-only + fsync-per-add is crash-safe: a torn final line (partial
// write on crash) is simply skipped on load, and the grant it represents was never
// acknowledged to the member (the promote/mint had not run), so re-redeeming it is
// correct. It is per-DG_HOME (one operator, one set), mirroring roster.lastcreated.

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// redeemedInvitesPath is the per-DG_HOME durable redeemed-grant-id log (design §1 +
// §9 Gate B/P8). Per-DG_HOME (one operator, one one-time set), mirroring
// rosterCursorPath — the redeemed set is the operator's single source of truth for
// which grants have been consumed, shared across every relay leg's redeemHandler.
func redeemedInvitesPath(dgHome string) string {
	return filepath.Join(dgHome, "redeemed-invites.log")
}

// redeemedStore is the in-memory redeemed-grant-id set backed by an append-only
// on-disk log. Its own mutex makes has/add safe even though the redeemHandler
// already serializes callers — the store is self-contained and reusable.
type redeemedStore struct {
	path string
	mu   sync.Mutex
	set  map[string]struct{}
	f    *os.File
}

// openRedeemedStore loads any existing redeemed-id log at path into memory and
// opens it for append. A missing file is fine (a fresh operator has redeemed
// nothing). A present-but-unreadable file is a hard error — fail closed rather than
// serve redemptions with an unknown one-time set (which would re-admit replays).
func openRedeemedStore(path string) (*redeemedStore, error) {
	rs := &redeemedStore{path: path, set: make(map[string]struct{})}
	if data, err := os.ReadFile(path); err == nil {
		sc := bufio.NewScanner(strings.NewReader(string(data)))
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			id := strings.TrimSpace(sc.Text())
			if id != "" {
				rs.set[id] = struct{}{}
			}
		}
		if err := sc.Err(); err != nil {
			return nil, fmt.Errorf("scan redeemed log %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read redeemed log %s: %w", path, err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return nil, fmt.Errorf("open redeemed log %s: %w", path, err)
	}
	rs.f = f
	return rs, nil
}

// has reports whether grant has already been redeemed.
func (rs *redeemedStore) has(grant string) bool {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	_, ok := rs.set[grant]
	return ok
}

// add durably records grant as redeemed: it appends the id + newline and fsyncs
// BEFORE updating the in-memory set, so a returned-nil add is durable — a replay
// after a restart re-reads it. A duplicate add is a no-op (already durable). An I/O
// failure is surfaced so the caller fails closed (no promote/mint on a
// non-durable redemption).
func (rs *redeemedStore) add(grant string) error {
	grant = strings.TrimSpace(grant)
	if grant == "" {
		return fmt.Errorf("empty grant id")
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if _, ok := rs.set[grant]; ok {
		return nil
	}
	if _, err := rs.f.WriteString(grant + "\n"); err != nil {
		return fmt.Errorf("append redeemed grant: %w", err)
	}
	if err := rs.f.Sync(); err != nil {
		return fmt.Errorf("fsync redeemed log: %w", err)
	}
	rs.set[grant] = struct{}{}
	return nil
}

// close releases the append handle. Best-effort; safe to call on a nil store.
func (rs *redeemedStore) close() error {
	if rs == nil || rs.f == nil {
		return nil
	}
	return rs.f.Close()
}
