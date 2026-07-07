// Package store implements the M1 (individual-tier) local event log for
// dontguess (dontguess-331). It is the campfire-free counterpart to
// pkg/proto's FromStoreRecord boundary: an append-only file of Record values
// that fold into exactly the same []proto.Message shape the exchange engine
// already replays from a campfire store.
//
// Zero campfire dependency: this package imports only the standard library
// and pkg/proto (a plain struct with no cf-protocol import of its own in the
// types used here). Nothing in this file imports
// "github.com/campfire-net/campfire/...".
//
// Scope (per docs/design/nostr-first-rebuild-decision.md, Individual tier
// NFR row "Sequencer: none needed — single local writer, trivially
// ordered"): this is a SINGLE-WRITER, strictly-append-order store. Append
// order IS fold order. There is no sequencer, no multi-writer merge, and no
// orphan/out-of-order antecedent buffer — that machinery is explicitly
// deferred to M2 (dontguess-50d), which will need it once nostr multi-relay
// ingest can deliver events out of order. Do not extend this package with
// merge/reorder logic without revisiting that item.
package store

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"sync"

	"github.com/campfire-net/dontguess/pkg/proto"
)

// Record is the on-disk representation of one exchange event. Field names
// and types are a 1:1 mirror of proto.Message (itself a mirror of
// cf-protocol's store.MessageRecord's Message-relevant subset), so ToMessage
// is a lossless, order-preserving conversion — the exchange fold cannot tell
// the difference between a Message that arrived via campfire's
// FromStoreRecord and one that arrived via Record.ToMessage.
type Record struct {
	ID          string   `json:"id"`
	CampfireID  string   `json:"campfire_id"`
	Sender      string   `json:"sender"`
	Payload     []byte   `json:"payload"`
	Tags        []string `json:"tags"`
	Antecedents []string `json:"antecedents"`
	Timestamp   int64    `json:"timestamp"`
	// Instance is tainted (sender-asserted) metadata, carried through
	// unchanged — see proto.Message.Instance for the trust caveat.
	Instance string `json:"instance,omitempty"`
}

// ToMessage converts a Record to a proto.Message, the type the exchange
// engine's State.Replay/Apply already consume.
func (r Record) ToMessage() proto.Message {
	return proto.Message{
		ID:          r.ID,
		CampfireID:  r.CampfireID,
		Sender:      r.Sender,
		Payload:     r.Payload,
		Tags:        r.Tags,
		Antecedents: r.Antecedents,
		Timestamp:   r.Timestamp,
		Instance:    r.Instance,
	}
}

// Store is a single-writer, append-only local event log backed by a flat
// JSONL file (one JSON-encoded Record per line).
//
// Concurrency: Append calls are serialized by an internal mutex, so a single
// process may safely call Append from multiple goroutines. Store does NOT
// perform cross-process file locking — the single-writer invariant is an
// operational contract of M1 (one operator process owns the file), the same
// trust boundary the existing campfire path already assumes for its
// "operator is the sole authoritative sequencer" model. A second OS process
// appending to the same file concurrently is out of scope; that is exactly
// the multi-writer problem M2's sequencer solves.
type Store struct {
	mu   sync.Mutex
	path string
	w    *os.File
}

// Open opens (creating if necessary) the append-only log at path for
// writing. The returned Store must be closed with Close when no longer
// needed.
func Open(path string) (*Store, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	return &Store{path: path, w: f}, nil
}

// Append writes rec to the end of the log as one JSON line and fsyncs it
// before returning, so a successful Append is durable across a crash.
// Append order is fold order — see the package doc for the M1 single-writer
// invariant this depends on.
func (s *Store) Append(rec Record) error {
	if rec.ID == "" {
		return fmt.Errorf("store: append: record ID must not be empty")
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("store: marshal record %s: %w", rec.ID, err)
	}
	line = append(line, '\n')

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.w.Write(line); err != nil {
		return fmt.Errorf("store: write record %s: %w", rec.ID, err)
	}
	return s.w.Sync()
}

// ReadAll reads every record currently on disk, in append order. It opens
// the backing file independently of the writer handle so a fresh read
// always observes the latest fsynced state.
func (s *Store) ReadAll() ([]Record, error) {
	f, err := os.Open(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("store: open for read %s: %w", s.path, err)
	}
	defer f.Close() //nolint:errcheck

	var recs []Record
	scanner := bufio.NewScanner(f)
	// Payloads are base64-encoded JSON bytes and can be large; grow the
	// scanner's buffer well past bufio's 64KiB default line limit.
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var rec Record
		if err := json.Unmarshal(line, &rec); err != nil {
			return nil, fmt.Errorf("store: unmarshal record: %w", err)
		}
		recs = append(recs, rec)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("store: scan %s: %w", s.path, err)
	}
	return recs, nil
}

// Replay reads the full log and converts every Record to a proto.Message in
// append order, ready to pass directly to exchange.State.Replay (whose
// parameter type, []exchange.Message, is a type alias for []proto.Message).
func (s *Store) Replay() ([]proto.Message, error) {
	recs, err := s.ReadAll()
	if err != nil {
		return nil, err
	}
	msgs := make([]proto.Message, len(recs))
	for i, r := range recs {
		msgs[i] = r.ToMessage()
	}
	return msgs, nil
}

// Close closes the writer handle. It does not affect ReadAll, which opens
// its own handle.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Close()
}
