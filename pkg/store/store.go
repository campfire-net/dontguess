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
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/campfire-net/dontguess/pkg/proto"
)

// ErrStoreClosed is returned by Append/BatchAppend when the Store has already
// been Closed. It is a deterministic, identifiable shutdown signal that
// replaces the racy, OS-level "write ...: file already closed" error a write
// to a closed *os.File would otherwise produce.
//
// It exists because Close and a concurrent Append can race during teardown:
// an engine writer goroutine (the poll loop / a dispatch handler emitting an
// operator record) can reach Append after Close has already run. Returning a
// sentinel — instead of writing to a closed fd — lets the caller distinguish a
// benign "we are shutting down" append from a real write failure and handle it
// gracefully (dontguess-fe9f). Callers test for it with errors.Is.
var ErrStoreClosed = errors.New("store: append after close")

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
	// Origin distinguishes how this record entered the local log:
	// "" or "local" = operator-authored, "relay" = ingested from a relay by
	// the M2 Intake path (docs/design/relay-transport.md §2.1). Additive,
	// persisted, JSON omitempty. ToMessage does NOT carry this into
	// proto.Message — it is a store-local provenance marker, not part of
	// the exchange message shape.
	Origin string `json:"origin,omitempty"`
	// Seq is the operator-assigned monotonic fold order stamped by the M2
	// Sequencer at the ingest boundary (docs/design/relay-transport.md
	// §2.1, §2.4 step 4). Additive, persisted, JSON omitempty. ToMessage
	// does NOT carry this into proto.Message for the same reason as Origin.
	Seq int64 `json:"seq,omitempty"`
}

// ToMessage converts a Record to a proto.Message, the type the exchange
// engine's State.Replay/Apply already consume. Origin and Seq are
// deliberately NOT copied here (docs/design/relay-transport.md §2.1: "Both
// JSON fields with omitempty; ToMessage ignores them (no proto.Message
// change)") — they are store-local provenance/ordering metadata, not part
// of the wire message shape.
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

// fileSyncer is the subset of *os.File that Store needs to write and
// durably flush the log. It exists (rather than using *os.File directly)
// so an internal white-box test can substitute a counting fake to verify
// BatchAppend's single-fsync contract without depending on OS-level
// syscall tracing. *os.File satisfies this interface unchanged.
type fileSyncer interface {
	io.Writer
	Sync() error
	Close() error
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
	w    fileSyncer
	// closed is set by Close under mu. Once set, Append/BatchAppend fail fast
	// with ErrStoreClosed instead of writing to the closed fd, making the
	// Close/Append teardown race deterministic (dontguess-fe9f).
	closed bool
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
	if s.closed {
		return fmt.Errorf("store: append record %s: %w", rec.ID, ErrStoreClosed)
	}
	if _, err := s.w.Write(line); err != nil {
		return fmt.Errorf("store: write record %s: %w", rec.ID, err)
	}
	return s.w.Sync()
}

// BatchAppend writes recs to the end of the log as N JSON lines under ONE
// store-mutex hold and ONE fsync (docs/design/relay-transport.md §2.1, ADV-11
// backfill-storm mitigation) — the same atomic-write discipline Append uses,
// scaled to a batch. It is all-or-nothing: every record is validated and
// marshaled into an in-memory buffer BEFORE the mutex is taken or any byte
// touches the file, so a single invalid record (e.g. empty ID) or marshal
// failure anywhere in the batch aborts the whole call with zero bytes
// written — no record in a failed batch is ever observable via
// ReadAll/Replay. On success, the batch is written as one Write call
// followed by one Sync call, and records land in slice order, preserving
// the single-writer append-order-is-fold-order invariant the package doc
// requires.
func (s *Store) BatchAppend(recs []Record) error {
	if len(recs) == 0 {
		return nil
	}

	var buf bytes.Buffer
	for _, rec := range recs {
		if rec.ID == "" {
			return fmt.Errorf("store: batch append: record ID must not be empty")
		}
		line, err := json.Marshal(rec)
		if err != nil {
			return fmt.Errorf("store: batch append: marshal record %s: %w", rec.ID, err)
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return fmt.Errorf("store: batch append %d records: %w", len(recs), ErrStoreClosed)
	}
	if _, err := s.w.Write(buf.Bytes()); err != nil {
		return fmt.Errorf("store: batch append: write %d records: %w", len(recs), err)
	}
	return s.w.Sync()
}

// ReadAll reads every record currently on disk, in append order. It opens
// the backing file independently of the writer handle so a fresh read
// always observes the latest fsynced state.
//
// Torn-tail recovery: a crash mid-Append can leave a truncated final line (the
// write is not atomic even though a *completed* Append is fsynced — see Append).
// ReadAll tolerates exactly one unparseable TRAILING line, dropping it and
// returning every fully-valid record that precedes it. This upholds the package
// doc's durability contract: every record whose Append returned successfully is
// newline-terminated and recoverable, and a partial record from an interrupted
// Append never poisons the whole log.
//
// Mid-log corruption is NOT recovered: an unparseable line that is followed by
// any further non-empty line is real corruption (not a torn tail), so ReadAll
// surfaces it as an error rather than silently skipping a record in the middle
// of the append-ordered log. Single-writer append order means only the last
// line can ever be torn.
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

	// pendingErr holds an unmarshal failure whose "torn tail vs. mid-log
	// corruption" verdict is not yet decided: it is a torn tail only if no
	// further non-empty line follows it. As soon as a later non-empty line
	// appears, the failure is mid-log corruption and is surfaced.
	var pendingErr error
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		if pendingErr != nil {
			// A prior line failed to parse and this non-empty line follows it,
			// so the failure was NOT the trailing line — real mid-log corruption.
			return nil, fmt.Errorf("store: unmarshal record (mid-log corruption, not a torn tail): %w", pendingErr)
		}
		var rec Record
		if err := json.Unmarshal(line, &rec); err != nil {
			// Defer the verdict: a torn tail if this proves to be the last
			// non-empty line, mid-log corruption if another line follows.
			pendingErr = err
			continue
		}
		recs = append(recs, rec)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("store: scan %s: %w", s.path, err)
	}
	// A still-pending error here means the unparseable line was the trailing
	// line: a torn tail from an interrupted Append. Recover by dropping it and
	// returning the valid prefix.
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
//
// Close is idempotent and flips the closed flag under mu, so any Append that
// races Close during teardown fails fast with ErrStoreClosed (a deterministic
// shutdown signal) instead of writing to a closed fd (dontguess-fe9f). A
// second Close is a no-op that returns nil.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.w.Close()
}
