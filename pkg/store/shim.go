package store

// This file provides a campfire-store-COMPATIBLE surface over the
// campfire-free pkg/store log (dontguess-657). It exists so that the
// pkg/exchange test harness — which historically observed operator egress via
// the campfire SDK store package — can observe the same egress through this
// local, zero-campfire store with a mechanical import swap and no call-site
// changes.
//
// The names, signatures, and semantics below intentionally mirror the subset
// of the campfire store package the exchange tests use:
//
//	store.MessageRecord               -> MessageRecord (alias for Record)
//	store.MessageFilter{Tags: ...}    -> MessageFilter
//	(*store.Store).AddMessage(rec)    -> (*Store).AddMessage
//	(*store.Store).GetMessage(id)     -> (*Store).GetMessage
//	(*store.Store).ListMessages(...)  -> (*Store).ListMessages
//	store.NowNano()                   -> NowNano()
//	store.StorePath(dir)              -> StorePath(dir)
//
// Zero campfire dependency: this file imports only the standard library, in
// keeping with the pkg/store package invariant (see store.go package doc).

import (
	"path/filepath"
	"sort"
	"time"
)

// MessageRecord is a campfire-store-compatible alias for Record.
//
// The campfire store's MessageRecord and pkg/store's Record share the same
// Message-relevant field subset (ID, CampfireID, Sender, Payload, Tags,
// Antecedents, Timestamp, Instance), so an alias — rather than a separate type
// — lets exchange test files that build store.MessageRecord{...} literals and
// pass []store.MessageRecord to exchange.FromStoreRecords keep compiling
// unchanged after the import swap.
//
// Note: the campfire MessageRecord additionally carries Signature, Provenance,
// ReceivedAt, and SenderCampfireID fields that this store does not model —
// those are cf-SDK boundary concerns (SQLite NOT NULL constraints, relay
// provenance) with no meaning in the single-writer local log. Test literals
// that set those fields must drop them (dontguess-657).
type MessageRecord = Record

// MessageFilter mirrors the tag-filter subset of the campfire store's
// MessageFilter used by the exchange tests. Only Tags is modeled because that
// is the only field the tests use.
//
// Tags uses OR semantics, exactly matching campfire ListMessages: a record
// matches the filter if it carries ANY of the listed tags (case-insensitive).
// An empty Tags means no tag filtering.
type MessageFilter struct {
	Tags []string
}

// NowNano returns the current wall-clock time in nanoseconds since the Unix
// epoch, matching campfire store.NowNano. Used by the exchange tests to stamp
// synthetic record timestamps.
func NowNano() int64 { return time.Now().UnixNano() }

// StorePath returns the conventional path of the local event log within dir,
// matching the shape of campfire store.StorePath (which returns the backing
// store file path for a config dir). For the campfire-free log this is the
// append-only events.jsonl file.
func StorePath(dir string) string { return filepath.Join(dir, "events.jsonl") }

// AddMessage appends rec to the log, mirroring the campfire store's
// AddMessage. It returns 1 (the number of records appended) on success so the
// signature stays shaped like campfire's (count/bool, error) return; every
// exchange test call site discards the first value. Unlike campfire's
// INSERT OR IGNORE, this is an unconditional append — the single-writer local
// log has no dedup requirement (see package doc).
func (s *Store) AddMessage(rec MessageRecord) (int64, error) {
	if err := s.Append(rec); err != nil {
		return 0, err
	}
	return 1, nil
}

// GetMessage returns a copy of the record with the given ID, or (nil, nil) if
// no such record exists — matching campfire store.GetMessage's not-found
// contract (nil record, nil error). It reads the current on-disk log.
func (s *Store) GetMessage(id string) (*MessageRecord, error) {
	recs, err := s.ReadAll()
	if err != nil {
		return nil, err
	}
	for i := range recs {
		if recs[i].ID == id {
			r := recs[i]
			return &r, nil
		}
	}
	return nil, nil
}

// ListMessages returns the records for campfireID whose Timestamp is strictly
// greater than since, optionally tag-filtered, ordered by Timestamp ascending.
// It reproduces the exact semantics of the campfire store's ListMessages that
// the exchange tests depend on:
//
//   - campfireID == "" matches every campfire; otherwise records must match
//     campfireID exactly.
//   - since is an exclusive lower bound: only records with Timestamp > since
//     are returned (campfire uses `timestamp > afterTimestamp`).
//   - filters is variadic but only the first filter is applied, exactly like
//     campfire (which reads filter[0]). Within that filter, Tags uses OR
//     semantics (a record matches if it carries any listed tag,
//     case-insensitive); an empty Tags disables tag filtering.
//   - results are ordered by Timestamp ascending; ties preserve append order
//     (stable sort), matching campfire's `ORDER BY timestamp` over an
//     insertion-ordered table.
func (s *Store) ListMessages(campfireID string, since int64, filters ...MessageFilter) ([]MessageRecord, error) {
	recs, err := s.ReadAll()
	if err != nil {
		return nil, err
	}
	var f MessageFilter
	if len(filters) > 0 {
		f = filters[0]
	}
	out := make([]MessageRecord, 0, len(recs))
	for _, r := range recs {
		if campfireID != "" && r.CampfireID != campfireID {
			continue
		}
		if r.Timestamp <= since {
			continue
		}
		if len(f.Tags) > 0 && !hasAnyTagFold(r.Tags, f.Tags) {
			continue
		}
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Timestamp < out[j].Timestamp
	})
	return out, nil
}

// hasAnyTagFold reports whether recTags contains any tag in wantTags, using a
// case-insensitive comparison — matching campfire ListMessages, which lowers
// both the stored tag values and the filter tags before comparing.
func hasAnyTagFold(recTags, wantTags []string) bool {
	for _, want := range wantTags {
		for _, have := range recTags {
			if equalFold(have, want) {
				return true
			}
		}
	}
	return false
}

// equalFold is a tiny ASCII-only case-insensitive comparison. Exchange tags
// are ASCII (e.g. "exchange:match"), so a full Unicode fold is unnecessary;
// this avoids pulling in strings solely for EqualFold and keeps the comparison
// allocation-free.
func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}
