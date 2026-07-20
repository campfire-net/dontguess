package main

// individual_ops.go implements the SERVER side of the individual-tier
// (zero-relay) put/buy IPC, item ed2-E (design
// docs/design/nostr-first-client-ed2.md §3.3, §4 tier table, §6 item 6).
//
// Single-writer invariant (relay-transport.md §0, ed2 §0): the client MUST NOT
// append to events.jsonl directly. OpPut/OpBuy route the request through the
// already-running `dontguess serve` — the ONE engine process that owns
// LocalStore — via the operator unix socket. The external (client-originated)
// record is ingested through exchange.Engine.IngestLocalRecord, which appends
// AND folds the record under the engine's localMu, serializing it with the poll
// loop exactly the way the operator's own appendLocalRecord serializes its
// egress. This is the fix for the dontguess-2b4 fold-cursor race: a raw
// LocalStore.Append from this socket goroutine (the prior approach) does NOT
// hold localMu and corrupts the length-cursor the poll loop and appendLocalRecord
// share (lost/duplicated folds) — see IngestLocalRecord's doc for the full race.
//
// OpPut/OpBuy are INDIVIDUAL-TIER-ONLY: ScripStore==nil, no mint path, no scrip
// movement anywhere in this file. Zero identity ceremony — every call gets a
// fresh random per-call sender key (no persisted agent identity), matching the
// "byte-for-byte individual tier" acceptance gate.

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"time"

	"github.com/3dl-dev/dontguess/pkg/exchange"
	dgstore "github.com/3dl-dev/dontguess/pkg/store"
)

const (
	// opBuyAwaitTimeout bounds how long OpBuy waits SERVER-SIDE for the buy to
	// resolve to a match or a buy-miss. IngestLocalRecord dispatches the buy
	// synchronously (the match/miss is emitted before it returns), so this is
	// generous headroom against an engine stall, not a measured floor — contrast
	// design §3.2's team-tier 10s floor, which has to account for relay RTT and
	// the outbox tick that individual tier does not have.
	opBuyAwaitTimeout = 8 * time.Second

	// opBuyConnDeadline is the OpBuy connection's own read/write deadline. It
	// MUST exceed operatorConnDeadline (serve.go, 5s) — that default is stall
	// protection for an ordinary quick request/response round trip and would
	// truncate the bounded server-side await above (design §3.3: "OpBuy needs an
	// extended socket deadline ... covering the bounded await"). It must also
	// exceed opBuyAwaitTimeout with slack for the request read + response write.
	opBuyConnDeadline = 10 * time.Second

	// opBuyPollInterval is the cadence at which the OpBuy handler re-checks
	// whether the engine has resolved a match/miss for its buy. Cheap (a single
	// mutex-guarded map lookup via State.IsOrderMatched) — no I/O.
	opBuyPollInterval = 25 * time.Millisecond
)

// opPutResponse is the JSON response shape for OpPut.
type opPutResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	PutID string `json:"put_id,omitempty"`
}

// opBuyResponse is the JSON response shape for OpBuy.
//
//   - Matched=true: a real hit. EntryID/ContentType/TokenCost/Content (base64)
//     describe the matched inventory entry, fetched directly from engine state
//     and returned inline in this ONE response — no settle chain, no scrip
//     (design §3.3: ScripStore==nil on this tier, so there is no reservation to
//     gate delivery on and no reason to round-trip a separate deliver phase).
//   - Miss=true: a genuine buy-miss — no cache exists for this task yet.
//   - TimedOut=true: opBuyAwaitTimeout elapsed with no match/miss observed. This
//     should not happen on individual tier absent an engine stall; never
//     conflate it with Miss (design §5.4's AMBIGUOUS-timeout discipline: a
//     timeout is never "no cache exists").
type opBuyResponse struct {
	OK          bool   `json:"ok"`
	Error       string `json:"error,omitempty"`
	BuyID       string `json:"buy_id,omitempty"`
	Matched     bool   `json:"matched"`
	Miss        bool   `json:"miss,omitempty"`
	TimedOut    bool   `json:"timed_out,omitempty"`
	EntryID     string `json:"entry_id,omitempty"`
	ContentType string `json:"content_type,omitempty"`
	TokenCost   int64  `json:"token_cost,omitempty"`
	Content     string `json:"content,omitempty"`
}

// randomLocalKey returns a fresh 32-byte random hex identifier. Used as the
// per-call sender key and record ID for individual-tier OpPut/OpBuy (design
// §3.3: "zero identity ceremony" — no persisted agent identity, a fresh random
// key every call).
func randomLocalKey() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating random local key: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// hasTag reports whether tags contains want.
func hasTag(tags []string, want string) bool {
	for _, t := range tags {
		if t == want {
			return true
		}
	}
	return false
}

// handleOpPut services the individual-tier OpPut IPC request. It builds an
// exchange:put record for req's content under a fresh random per-call seller
// key, ingests it through eng.IngestLocalRecord (the sole writer process,
// single-writer invariant preserved; localMu-guarded fold, dontguess-2b4), then
// immediately promotes it into matchable inventory via eng.AutoAcceptPut — the
// SAME engine method both the operator CLI's `accept-put` and the background
// auto-accept ticker use — so a following OpBuy in the same session does not
// have to wait on the auto-accept ticker. ScripStore==nil-only; never touches
// scrip.
func handleOpPut(eng *exchange.Engine, req operatorRequest) opPutResponse {
	if req.Description == "" {
		return opPutResponse{OK: false, Error: "put: description is required"}
	}
	if req.Content == "" {
		return opPutResponse{OK: false, Error: "put: content is required (base64-encoded)"}
	}
	if req.TokenCost <= 0 {
		return opPutResponse{OK: false, Error: "put: token_cost must be positive"}
	}
	// Validate the caller's base64 up front so a malformed payload is rejected
	// here with a clear error, rather than being durably appended and silently
	// dropped later at applyPut's own base64-decode guard.
	if _, err := base64.StdEncoding.DecodeString(req.Content); err != nil {
		return opPutResponse{OK: false, Error: fmt.Sprintf("put: content is not valid base64: %v", err)}
	}
	if eng.LocalStore() == nil {
		return opPutResponse{OK: false, Error: "put: no local store configured"}
	}

	sellerKey, err := randomLocalKey()
	if err != nil {
		return opPutResponse{OK: false, Error: err.Error()}
	}
	putID, err := randomLocalKey()
	if err != nil {
		return opPutResponse{OK: false, Error: err.Error()}
	}

	payload, err := json.Marshal(map[string]any{
		"description":  req.Description,
		"content":      req.Content, // already base64, forwarded verbatim
		"token_cost":   req.TokenCost,
		"content_type": req.ContentType,
		"domains":      req.Domains,
	})
	if err != nil {
		return opPutResponse{OK: false, Error: fmt.Sprintf("put: encode payload: %v", err)}
	}

	if err := eng.IngestLocalRecord(dgstore.Record{
		ID:         putID,
		CampfireID: "local",
		Sender:     sellerKey,
		Payload:    payload,
		Tags:       []string{exchange.TagPut},
		Timestamp:  time.Now().UnixNano(),
	}); err != nil {
		return opPutResponse{OK: false, Error: fmt.Sprintf("put: ingest: %v", err)}
	}

	// Auto-accept at 70% of token_cost, mirroring operator.go's acceptPutCmd
	// default price rule. AutoAcceptPut self-refreshes (refreshBeforeOperatorOp)
	// before checking pendingPuts, so it picks up the record just ingested above.
	price := req.TokenCost * 70 / 100
	if price <= 0 {
		price = 1
	}
	expiresAt := time.Now().UTC().Add(72 * time.Hour)
	if err := eng.AutoAcceptPut(putID, price, expiresAt); err != nil {
		return opPutResponse{OK: false, Error: fmt.Sprintf("put %s: accept: %v", short(putID), err)}
	}

	return opPutResponse{OK: true, PutID: putID}
}

// handleOpBuy services the individual-tier OpBuy IPC request. It builds an
// exchange:buy record under a fresh random per-call buyer key, ingests it
// through eng.IngestLocalRecord (localMu-guarded fold + synchronous dispatch,
// dontguess-2b4), then blocks (bounded by opBuyAwaitTimeout) until the engine
// has resolved the buy to a hit or a buy-miss. On a genuine hit it resolves the
// matched inventory entry directly from engine state — no settle chain, no
// scrip, no reservation (ScripStore==nil on this tier: the engine IS the sole
// trusted local process, handing its own content back to its own IPC caller) —
// and returns match + inline content in ONE response (design §3.3). conn's
// deadline is extended to opBuyConnDeadline before the bounded wait so the
// default operatorConnDeadline stall-protection window does not truncate it.
func handleOpBuy(eng *exchange.Engine, conn net.Conn, req operatorRequest) opBuyResponse {
	if req.Task == "" {
		return opBuyResponse{OK: false, Error: "buy: task is required"}
	}
	if eng.LocalStore() == nil {
		return opBuyResponse{OK: false, Error: "buy: no local store configured"}
	}

	buyerKey, err := randomLocalKey()
	if err != nil {
		return opBuyResponse{OK: false, Error: err.Error()}
	}
	buyID, err := randomLocalKey()
	if err != nil {
		return opBuyResponse{OK: false, Error: err.Error()}
	}

	maxResults := req.MaxResults
	if maxResults <= 0 {
		maxResults = 3
	}
	payload, err := json.Marshal(map[string]any{
		"task":         req.Task,
		"budget":       req.Budget,
		"max_results":  maxResults,
		"content_type": req.ContentType,
		"domains":      req.Domains,
	})
	if err != nil {
		return opBuyResponse{OK: false, Error: fmt.Sprintf("buy: encode payload: %v", err)}
	}

	// Extend the connection deadline BEFORE the ingest+bounded wait (design
	// §3.3) — see opBuyConnDeadline doc.
	conn.SetDeadline(time.Now().Add(opBuyConnDeadline)) //nolint:errcheck

	if err := eng.IngestLocalRecord(dgstore.Record{
		ID:         buyID,
		CampfireID: "local",
		Sender:     buyerKey,
		Payload:    payload,
		Tags:       []string{exchange.TagBuy},
		Timestamp:  time.Now().UnixNano(),
	}); err != nil {
		return opBuyResponse{OK: false, Error: fmt.Sprintf("buy: ingest: %v", err)}
	}

	// IngestLocalRecord dispatches the buy synchronously, so the match/miss is
	// normally already resolved here. The bounded poll is a safety net against an
	// engine stall (never a silent hang): a timeout is reported as TimedOut, NOT
	// as a miss (design §5.4).
	deadline := time.Now().Add(opBuyAwaitTimeout)
	for !eng.State().IsOrderMatched(buyID) {
		if time.Now().After(deadline) {
			return opBuyResponse{OK: true, BuyID: buyID, TimedOut: true}
		}
		time.Sleep(opBuyPollInterval)
	}

	// The order is matched; now resolve the match record and its entry mapping.
	// IsOrderMatched (matchedOrders) and the entry mapping (matchToEntry) are both
	// set inside applyMatch, but under a concurrent OpPut/OpBuy storm the match
	// RECORD reaching the store and the State fold that populates matchToEntry can
	// lag the matchedOrders flag by a fold tick. So poll the resolution — bounded
	// by the SAME deadline — rather than reading it once: a genuine lag resolves on
	// a later tick, while true fold-cursor corruption (the mapping never appears)
	// still fails LOUD after the deadline. Reading it once raced with the fold and
	// spuriously reported "resolved to no entry" (dontguess-3cc: faster matching
	// widened the window that exposed this pre-existing race).
	var matchMsgID string
	var miss bool
	var entryID string
	for {
		recs, err := eng.LocalStore().ReadAll()
		if err != nil {
			return opBuyResponse{OK: false, Error: fmt.Sprintf("buy: read local store: %v", err)}
		}
		matchMsgID, miss = "", false
		for i := len(recs) - 1; i >= 0; i-- {
			rec := recs[i]
			if len(rec.Antecedents) == 0 || rec.Antecedents[0] != buyID {
				continue
			}
			if !hasTag(rec.Tags, exchange.TagMatch) {
				continue
			}
			matchMsgID = rec.ID
			miss = hasTag(rec.Tags, exchange.TagBuyMiss)
			break
		}
		if matchMsgID != "" {
			if miss {
				return opBuyResponse{OK: true, BuyID: buyID, Matched: false, Miss: true}
			}
			if entryID = eng.State().MatchEntryID(matchMsgID); entryID != "" {
				break
			}
		}
		if time.Now().After(deadline) {
			if matchMsgID == "" {
				// matchedOrders was set but the record never landed — never silently
				// claim a hit with no evidence backing it (LOUD-EVERYWHERE, design §0/§5).
				return opBuyResponse{OK: false, Error: fmt.Sprintf("buy %s: matched but no match/miss record found in local store", short(buyID))}
			}
			// The record exists but its entry mapping never folded within the bound:
			// this is the genuine fold-cursor-corruption signal, still surfaced LOUD.
			return opBuyResponse{OK: false, Error: fmt.Sprintf("buy %s: match %s resolved to no entry", short(buyID), short(matchMsgID))}
		}
		time.Sleep(opBuyPollInterval)
	}
	entry := eng.State().GetInventoryEntry(entryID)
	if entry == nil {
		return opBuyResponse{OK: false, Error: fmt.Sprintf("buy %s: entry %s not found in inventory", short(buyID), short(entryID))}
	}
	if entry.BlobPointer != "" {
		// Blossom fetch is out of scope for ed2 (design §8 anti-scope) — fail
		// loud rather than silently return the inline preview slice as if it were
		// the full content.
		return opBuyResponse{OK: false, Error: fmt.Sprintf("buy %s: entry %s is Blossom-offloaded — individual-tier IPC delivery does not support blob fetch (out of ed2 scope)", short(buyID), short(entryID))}
	}

	return opBuyResponse{
		OK:          true,
		BuyID:       buyID,
		Matched:     true,
		EntryID:     entryID,
		ContentType: entry.ContentType,
		TokenCost:   entry.TokenCost,
		Content:     base64.StdEncoding.EncodeToString(entry.Content),
	}
}

// opListAssignsResponse is the JSON response shape for OpListAssigns.
type opListAssignsResponse struct {
	OK      bool               `json:"ok"`
	Error   string             `json:"error,omitempty"`
	Assigns []assignsListEntry `json:"assigns"`
}

// assignsListEntry is one open/claimable assign task surfaced by OpListAssigns.
// Mirrors pkg/relayclient.OpenAssign's shape so the CLI's rendering code
// (assign.go) is tier-agnostic.
type assignsListEntry struct {
	AssignID        string `json:"assign_id"`
	EntryID         string `json:"entry_id,omitempty"`
	TaskType        string `json:"task_type"`
	Reward          int64  `json:"reward"`
	Status          string `json:"status"`
	ExclusiveSender string `json:"exclusive_sender,omitempty"`
	Description     string `json:"description,omitempty"`
}

// handleOpListAssigns services the assign-discovery IPC request (item
// dontguess-d26, #2 AGENT DOOR): reads eng.State() directly for every open/
// claimable AssignRecord (State.AllActiveAssigns) and returns those the caller
// may claim — ExclusiveSender=="" (open to anyone) or ExclusiveSender==the
// caller's own key, when supplied. On the individual tier there is no
// persisted per-call identity (design §3.3: OpPut/OpBuy mint a fresh random
// sender key every call), so req.CallerKey is normally empty and only
// non-exclusive tasks are surfaced — an individual-tier caller can never
// present the pubkey a warm-compression assign targets. This is a plain READ:
// the assign lifecycle it exposes is already a public operator broadcast, so
// socket reachability alone is a sufficient trust boundary (matches
// OpMetrics/OpListHeld — no signed authorization required, unlike OpMint/
// OpAllowlist).
//
// Description is read directly off the original exchange:assign local-store
// record's payload (not persisted on AssignRecord) by a best-effort re-read of
// LocalStore, matched by AssignID.
func handleOpListAssigns(eng *exchange.Engine, req operatorRequest) opListAssignsResponse {
	active := eng.State().AllActiveAssigns()

	descByAssignID := map[string]string{}
	if eng.LocalStore() != nil {
		if recs, err := eng.LocalStore().ReadAll(); err == nil {
			for _, rec := range recs {
				if !hasTag(rec.Tags, exchange.TagAssign) {
					continue
				}
				var p struct {
					Description string `json:"description"`
				}
				if json.Unmarshal(rec.Payload, &p) == nil && p.Description != "" {
					descByAssignID[rec.ID] = p.Description
				}
			}
		}
	}

	out := make([]assignsListEntry, 0, len(active))
	for _, rec := range active {
		if rec.ExclusiveSender != "" && rec.ExclusiveSender != req.CallerKey {
			continue
		}
		out = append(out, assignsListEntry{
			AssignID:        rec.AssignID,
			EntryID:         rec.EntryID,
			TaskType:        rec.TaskType,
			Reward:          rec.Reward,
			Status:          rec.Status.String(),
			ExclusiveSender: rec.ExclusiveSender,
			Description:     descByAssignID[rec.AssignID],
		})
	}
	return opListAssignsResponse{OK: true, Assigns: out}
}
