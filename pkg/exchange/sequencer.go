package exchange

import (
	"container/heap"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// DefaultMaxOrphans is the default bound on the orphan/pending-antecedent
// buffer. When more than this many events are held waiting for antecedents
// that have not yet arrived, the sequencer fails loud (ErrOrphanBufferOverflow)
// rather than growing without bound or silently dropping events.
const DefaultMaxOrphans = 1000

// ErrOrphanBufferOverflow is returned when the number of buffered
// pending-antecedent events exceeds the configured bound. It is a loud failure
// on purpose: an unbounded orphan buffer under relay reorder is a
// denial-of-service and a correctness hazard (the fold would stall silently).
var ErrOrphanBufferOverflow = errors.New("sequencer: orphan buffer overflow")

// ErrUnrecoverableAntecedent is returned by Seal when events remain buffered
// with antecedents that were never sequenced. Under multi-relay nostr ingest an
// e-tagged event can reference an antecedent that a relay has pruned and that
// no relay in the mesh still serves. When the domain is sealed (no further
// events will arrive) such an event's antecedent is provably unrecoverable.
// This MUST fail loud — the dontguess-553 lesson is that a broken settle chain
// degrading silently corrupts the scrip ledger; a pruned antecedent may never
// be silently dropped.
var ErrUnrecoverableAntecedent = errors.New("sequencer: unrecoverable (pruned) antecedent")

// Sequenced is one event paired with the local monotonic sequence number the
// operator assigned it at release. The fold consumes events in ascending Seq.
type Sequenced struct {
	// Seq is the operator-assigned local monotonic sequence number. It is the
	// authoritative fold order for this domain — NOT relay ingest order.
	Seq int64
	// Msg is the event released at this sequence position.
	Msg Message
}

// Sequencer is the operator-side authoritative sequencer for a single domain.
//
// Nostr gives no total order and no causal delivery: under multi-relay publish
// an e-tagged event can arrive before its antecedent. The Sequencer restores
// the single-deterministic-writer invariant the in-memory scrip ETag /
// atomic-reservation double-spend guard depends on (design
// docs/design/nostr-first-rebuild-decision.md §Sequencer per domain):
//
//  1. The operator is the sole authoritative sequencer. Relay ingest order is
//     NOT fold order.
//  2. On release the operator assigns a local monotonic sequence number; the
//     fold replays in that order.
//  3. Duplicate delivery (the same event id arriving from multiple relays) is
//     deduped by event id — a second copy is a no-op.
//  4. An event whose antecedent has not yet been released is held in a bounded
//     orphan buffer and released once the antecedent lands. If the antecedent
//     is provably unrecoverable (Seal with orphans remaining) the sequencer
//     fails loud, never silently dropping the causal chain.
//
// Determinism (the property the scrip ETag and the recovery replay both need):
// the release order is a pure function of the event set and its antecedent DAG,
// NOT of arrival order. Among the events that are causally ready at each step
// (every antecedent already released), the sequencer always releases the one
// with the smallest (Timestamp, ID). This is the unique canonical linear
// extension of the causal partial order, so any permutation or duplication of a
// causally-closed event set yields the byte-identical released sequence — and
// therefore a byte-identical fold.
//
// The Sequencer is safe for concurrent use.
type Sequencer struct {
	mu         sync.Mutex
	nextSeq    int64
	maxOrphans int
	// emitted is the set of event ids already released (or seeded via
	// MarkEmitted as folded in a prior checkpoint). An antecedent is satisfied
	// iff it is in this set.
	emitted map[string]struct{}
	// buffered holds ingested-but-not-yet-released events, keyed by id. It is
	// both the dedup index and the orphan buffer: an event stays here until all
	// its antecedents are emitted.
	buffered map[string]*Message

	// trueOrphans is the O(1)-maintained count of buffered events that have at
	// least one antecedent that has never been INGESTED at all (neither
	// emitted nor currently buffered) -- a genuine gap, as opposed to an event
	// that is merely waiting for Drain to cascade-release an antecedent that
	// has already arrived. This is the quantity the ingest-time bound guards:
	// it is exactly the quantity Seal() would ultimately report as
	// unrecoverable if no further events ever arrive, so bounding it (rather
	// than bounding raw buffer size) never trips on a large but eventually-
	// resolvable causal chain fed in via Ingest before any Drain call (e.g.
	// SequenceForFold's batch replay path).
	trueOrphans int
	// missingCount[id] is the number of currently-unknown (never-ingested,
	// deduplicated) antecedents that buffered event `id` still has. Only
	// entries with a positive count exist; entries are removed once the count
	// reaches zero. len(missingCount) == trueOrphans at all times.
	missingCount map[string]int
	// missingWaiters[a] lists the buffered event ids that count `a` among
	// their currently-unknown antecedents. When `a` is ingested (arrives, by
	// any means -- buffered or immediately emitted), every waiter's
	// missingCount is decremented in O(1) amortized total work, and any
	// waiter that reaches zero is no longer a true orphan.
	missingWaiters map[string][]string
}

// NewSequencer returns a Sequencer with the given orphan-buffer bound. A
// non-positive maxOrphans uses DefaultMaxOrphans.
func NewSequencer(maxOrphans int) *Sequencer {
	if maxOrphans <= 0 {
		maxOrphans = DefaultMaxOrphans
	}
	return &Sequencer{
		maxOrphans:     maxOrphans,
		emitted:        make(map[string]struct{}),
		buffered:       make(map[string]*Message),
		missingCount:   make(map[string]int),
		missingWaiters: make(map[string][]string),
	}
}

// MarkEmitted seeds event ids as already-sequenced. It is used on recovery from
// a checkpoint: events folded before the sequencer started are known-present,
// so events that reference them as antecedents are not treated as orphans. It
// does not assign sequence numbers (seeded events are not re-released).
func (s *Sequencer) MarkEmitted(ids ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range ids {
		if id == "" {
			continue
		}
		s.emitted[id] = struct{}{}
		// A seeded id is now "known" the same as an ingested one: resolve any
		// buffered event that was counting it as a true (never-arrived) gap.
		s.resolveArrivalLocked(id)
	}
}

// Ingest records an event for sequencing. It dedups by event id: a second copy
// of an already-released or already-buffered event is a no-op (this is how
// duplicate multi-relay delivery is absorbed). Ingest does not release anything
// — call Drain to release the events that are now causally ready.
//
// An event with an empty ID is rejected: nostr event ids are content hashes and
// an empty id cannot be deduped or referenced as an antecedent.
//
// Ingest maintains the orphan bound itself, in O(1) amortized work, rather than
// deferring the check to Drain: an event is a TRUE orphan only if at least one
// of its antecedents has never been ingested at all (neither emitted nor
// currently buffered). An antecedent that is merely buffered-but-not-yet-
// released is NOT a gap — Drain's causal cascade resolves it in the same pass
// once its own antecedents land, so a large causally-in-order batch (e.g. the
// full replay set SequenceForFold ingests before its single Drain call) never
// trips the bound. A genuine flood of events referencing antecedents that never
// arrive — the actual denial-of-service shape — is rejected LOUD right here,
// before the buffer can grow past the configured bound, instead of only being
// detected after Drain has already absorbed unbounded memory.
func (s *Sequencer) Ingest(m Message) error {
	if m.ID == "" {
		return fmt.Errorf("sequencer: refusing to ingest event with empty ID")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.emitted[m.ID]; ok {
		return nil // duplicate of an already-released event
	}
	if _, ok := s.buffered[m.ID]; ok {
		return nil // duplicate still waiting in the buffer
	}

	// Deduplicated set of antecedents that have never been ingested at all —
	// the true gaps. O(k) in the number of antecedents on this one event
	// (small, bounded per-event), never proportional to buffer size.
	var missing []string
	if len(m.Antecedents) > 0 {
		seen := make(map[string]struct{}, len(m.Antecedents))
		for _, a := range m.Antecedents {
			if a == "" {
				continue
			}
			if _, dup := seen[a]; dup {
				continue
			}
			seen[a] = struct{}{}
			if !s.isKnownLocked(a) {
				missing = append(missing, a)
			}
		}
	}

	if len(missing) > 0 && s.trueOrphans+1 > s.maxOrphans {
		return fmt.Errorf("%w: ingesting %s would bring true orphans to %d, exceeding bound %d",
			ErrOrphanBufferOverflow, shortKey(m.ID), s.trueOrphans+1, s.maxOrphans)
	}

	cp := m
	s.buffered[m.ID] = &cp
	if len(missing) > 0 {
		s.missingCount[m.ID] = len(missing)
		s.trueOrphans++
		for _, a := range missing {
			s.missingWaiters[a] = append(s.missingWaiters[a], m.ID)
		}
	}
	// This event's own id is now known; resolve any earlier-buffered event
	// that was counting m.ID as one of its missing antecedents.
	s.resolveArrivalLocked(m.ID)
	return nil
}

// isKnownLocked reports whether id has been ingested in any form — released
// (emitted) or still buffered. Caller must hold s.mu.
func (s *Sequencer) isKnownLocked(id string) bool {
	if _, ok := s.emitted[id]; ok {
		return true
	}
	_, ok := s.buffered[id]
	return ok
}

// resolveArrivalLocked notifies every buffered event that was counting id
// among its missing (never-ingested) antecedents that id has now arrived,
// decrementing each waiter's missingCount and, when it reaches zero, removing
// it from the true-orphan count. Amortized O(1) per waiter over the object's
// lifetime: each (event, antecedent) edge is resolved at most once. Caller
// must hold s.mu.
func (s *Sequencer) resolveArrivalLocked(id string) {
	waiters := s.missingWaiters[id]
	if len(waiters) == 0 {
		return
	}
	delete(s.missingWaiters, id)
	for _, w := range waiters {
		c, ok := s.missingCount[w]
		if !ok {
			continue // already resolved via another path (defensive)
		}
		c--
		if c <= 0 {
			delete(s.missingCount, w)
			s.trueOrphans--
		} else {
			s.missingCount[w] = c
		}
	}
}

// messageHeap is a min-heap of buffered *Message ordered by the canonical
// (Timestamp, ID) release key. It backs Drain's cascade so the globally
// smallest ready event is always released next in O(log n), instead of
// re-scanning the entire remaining buffer to find it.
type messageHeap []*Message

func (h messageHeap) Len() int { return len(h) }
func (h messageHeap) Less(i, j int) bool {
	if h[i].Timestamp != h[j].Timestamp {
		return h[i].Timestamp < h[j].Timestamp
	}
	return h[i].ID < h[j].ID
}
func (h messageHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *messageHeap) Push(x any)   { *h = append(*h, x.(*Message)) }
func (h *messageHeap) Pop() any {
	old := *h
	n := len(old)
	item := old[n-1]
	old[n-1] = nil
	*h = old[:n-1]
	return item
}

// Drain releases every event that is now causally ready — every antecedent
// already released — assigning each the next monotonic sequence number. Ready
// events are released in canonical (Timestamp, ID) order, re-evaluated after
// each release so that an event made ready by the release of its antecedent
// takes its correct canonical position. The returned slice is in ascending Seq.
//
// Drain runs in O(N log N) over the currently buffered set (N events, each
// with a small bounded number of antecedents): it builds the causal
// dependency graph once — for each buffered event, how many of its distinct
// antecedents are still un-emitted, and a reverse index from antecedent id to
// waiting dependents — then releases ready events off a canonical-order
// min-heap, pushing each dependent onto the heap the instant its last pending
// antecedent is emitted. Every event is visited, and every antecedent edge is
// walked, exactly once; this replaces the previous algorithm's repeated
// full-buffer rescan per single release (O(N) work × N releases = O(N^2)).
//
// Events whose antecedents are still absent remain buffered (orphans). If, after
// releasing everything releasable, the number of remaining orphans exceeds the
// configured bound, Drain returns ErrOrphanBufferOverflow — a loud failure, not
// a silent drop. The events released before the overflow are still returned so
// the caller can fold them; the overflow signals the caller to stop and alert.
// In normal operation this residual check should never fire: Ingest already
// bounds true orphans at ingest time. It stays as a defense-in-depth invariant
// check on the actual backlog size, cheap (O(1)) since Drain already has
// len(s.buffered) in hand.
func (s *Sequencer) Drain() ([]Sequenced, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.buffered) == 0 {
		return nil, nil
	}

	pending := make(map[string]int, len(s.buffered))
	dependents := make(map[string][]string)
	h := &messageHeap{}
	for id, m := range s.buffered {
		seen := make(map[string]struct{}, len(m.Antecedents))
		n := 0
		for _, a := range m.Antecedents {
			if a == "" {
				continue
			}
			if _, dup := seen[a]; dup {
				continue
			}
			seen[a] = struct{}{}
			if _, ok := s.emitted[a]; ok {
				continue // already satisfied
			}
			n++
			dependents[a] = append(dependents[a], id)
		}
		pending[id] = n
		if n == 0 {
			heap.Push(h, s.buffered[id])
		}
	}

	var out []Sequenced
	for h.Len() > 0 {
		m := heap.Pop(h).(*Message)
		s.emitted[m.ID] = struct{}{}
		delete(s.buffered, m.ID)
		delete(pending, m.ID)
		out = append(out, Sequenced{Seq: s.nextSeq, Msg: *m})
		s.nextSeq++
		for _, depID := range dependents[m.ID] {
			left, ok := pending[depID]
			if !ok {
				continue
			}
			left--
			if left <= 0 {
				delete(pending, depID)
				heap.Push(h, s.buffered[depID])
			} else {
				pending[depID] = left
			}
		}
	}

	if len(s.buffered) > s.maxOrphans {
		return out, fmt.Errorf("%w: %d orphans exceed bound %d",
			ErrOrphanBufferOverflow, len(s.buffered), s.maxOrphans)
	}
	return out, nil
}

// Seal asserts that no further events will arrive for this domain. Any event
// still buffered has an antecedent that was never sequenced — provably
// unrecoverable (relay-pruned) — so Seal fails loud with
// ErrUnrecoverableAntecedent, naming the orphaned events and their missing
// antecedents. It never silently discards a buffered event.
//
// Seal is idempotent when the buffer is empty (returns nil). It does not clear
// the buffer on failure, so the caller can inspect PendingCount / retry after
// re-fetching from another relay.
func (s *Sequencer) Seal() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.buffered) == 0 {
		return nil
	}
	// Deterministic, bounded diagnostic: sort orphans by id and list the first
	// missing antecedent of each (capped so the error stays readable).
	ids := make([]string, 0, len(s.buffered))
	for id := range s.buffered {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	const maxListed = 8
	var b strings.Builder
	listed := 0
	for _, id := range ids {
		if listed >= maxListed {
			fmt.Fprintf(&b, " …(+%d more)", len(ids)-listed)
			break
		}
		m := s.buffered[id]
		missing := s.firstMissingAntecedentLocked(m)
		fmt.Fprintf(&b, " event %s→missing %s;", shortKey(id), shortKey(missing))
		listed++
	}
	return fmt.Errorf("%w: %d orphaned event(s) with never-sequenced antecedents:%s",
		ErrUnrecoverableAntecedent, len(s.buffered), b.String())
}

// PendingCount returns the number of events currently held in the orphan buffer
// (ingested but not yet releasable). Useful for diagnostics and tests.
func (s *Sequencer) PendingCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.buffered)
}

// antecedentsSatisfiedLocked reports whether every antecedent of m has been
// released. An empty antecedent id is ignored (roots and messages that carry no
// causal predecessor). Caller must hold s.mu.
func (s *Sequencer) antecedentsSatisfiedLocked(m *Message) bool {
	for _, a := range m.Antecedents {
		if a == "" {
			continue
		}
		if _, ok := s.emitted[a]; !ok {
			return false
		}
	}
	return true
}

// firstMissingAntecedentLocked returns the first antecedent of m that has not
// been released, or "" if all are satisfied. Caller must hold s.mu.
func (s *Sequencer) firstMissingAntecedentLocked(m *Message) string {
	for _, a := range m.Antecedents {
		if a == "" {
			continue
		}
		if _, ok := s.emitted[a]; !ok {
			return a
		}
	}
	return ""
}

// SequenceForFold is the batch replay/recovery entry point: it sequences a full
// set of events (in any relay-delivery order, with duplicates) into canonical
// fold order. It ingests every event, releases everything causally ready, and
// then seals — so a broken causal closure (a pruned antecedent) fails loud
// instead of yielding a silently-truncated fold.
//
// This is the ingest transform the engine's replay path applies before folding
// under multi-relay nostr ingest (EngineOptions.SequencedIngest). Because the
// released order is a pure function of the event set and its antecedent DAG, any
// permutation or duplication of a causally-closed set produces the byte-identical
// returned slice, and therefore a byte-identical fold.
func SequenceForFold(msgs []Message, maxOrphans int) ([]Message, error) {
	seq := NewSequencer(maxOrphans)
	for i := range msgs {
		if err := seq.Ingest(msgs[i]); err != nil {
			return nil, err
		}
	}
	released, err := seq.Drain()
	if err != nil {
		return nil, err
	}
	if err := seq.Seal(); err != nil {
		return nil, err
	}
	out := make([]Message, len(released))
	for i := range released {
		out[i] = released[i].Msg
	}
	return out, nil
}
