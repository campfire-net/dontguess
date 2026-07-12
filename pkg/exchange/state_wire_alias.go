package exchange

import "log"

// state_wire_alias.go — the read-time wire→store reconciliation (dontguess-55c
// GAP 1, docs/design/settle-wire-id-reconciliation-55c.md). See the wireToStore
// field doc in state_types.go for the full rationale. In one line: an operator
// match/preview/deliver is keyed in every state map by its random pre-signature
// STORE id, but the Outbox re-signs it on publish to a content-hash WIRE id, and
// a relay buyer can only ever e-tag that wire id — so a settle antecedent misses
// every store-keyed map unless it is first mapped wire→store. This file adds ONLY
// resolution; it never mints scrip, never changes a signed event, and never
// touches the wire.

// RegisterWireAlias records that operator-emitted store record `store` was
// published to the relay under content-hash wire id `wire`, so a later buyer
// message that e-tags `wire` resolves back to `store` at read time (resolveAlias).
//
// Idempotent: re-registering the same (wire, store) pair is a no-op. A DIFFERENT
// store id for an already-mapped wire id is a COLLISION — the FIRST mapping is
// kept (never overwritten) and the attempt is LOUD-logged (LOCKED-5), so a buggy
// or hostile re-derivation can never repoint a live match/deliver alias onto a
// different match. (A genuine collision requires two distinct store records that
// hash to the same wire id — identical kind/tags/content/pubkey/created_at-second
// — which the relay itself would already treat as one event.)
//
// Called from the Outbox publish goroutine (live) and seedEmittedFromStore
// (restart); neither holds s.mu, so this acquires s.mu.Lock() itself. It is the
// SOLE writer of wireToStore; resolveAlias is the only reader.
func (s *State) RegisterWireAlias(wire, store string) {
	if wire == "" || store == "" || wire == store {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.wireToStore[wire]; ok {
		if existing != store {
			log.Printf("SECURITY: exchange wire-alias collision: wire %s already maps to store %s; refusing to remap to %s",
				shortKey(wire), shortKey(existing), shortKey(store))
		}
		return
	}
	s.wireToStore[wire] = store
}

// resolveAlias maps an operator wire id to its store id, returning `id` unchanged
// when no alias is registered. Identity is the correct default for every id that
// is NOT an operator wire alias: buyer/relay-origin ids (store == wire), operator
// ids the local no-Outbox path never re-signs (the ~32K in-process suite and the
// individual tier, where wireToStore is always empty), and any id already in store
// form. Because identity is a no-op, prepending resolveAlias at a resolution site
// is byte-for-byte unchanged wherever no alias exists.
//
// Caller MUST hold s.mu (read or write): every call site is either an accessor
// holding s.mu.RLock or a fold handler running under applyLocked's s.mu.Lock.
func (s *State) resolveAlias(id string) string {
	if store, ok := s.wireToStore[id]; ok {
		return store
	}
	return id
}
