package exchange

// engine_local_buydrop_test.go — regression test for dontguess-b84.
//
// In local mode (EngineOptions.LocalStore set) two engine paths advanced a
// single length cursor (localSeen): a full state rebuild (replayAllLocal,
// reachable mid-flight via AutoAcceptPut/RejectPut) folded every record into
// State AND advanced the cursor, while the poll loop (pollLocalStore) only
// dispatched records beyond that same cursor. So a buy appended between the
// poll loop's last dispatch and an auto-accept-triggered rebuild was folded
// into State.ActiveOrders and had the cursor advanced past it WITHOUT ever
// being dispatched — handleBuy/matching never ran, and the buy sat unmatched
// forever with no error logged.
//
// This test reproduces that exact interleaving deterministically against a real
// pkg/store (no mocks, no fakes): it seeds an accepted inventory entry, appends
// a buy (as an out-of-process CLI would), then triggers an AutoAcceptPut for a
// second put — whose refresh performs the state rebuild that folds the buy —
// and finally runs one poll cycle. It asserts the buy is matched. Fails against
// the pre-fix code (buy dropped), passes after the fold/dispatch cursor split.

import (
	"encoding/base64"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	dgstore "github.com/campfire-net/dontguess/pkg/store"
)

func TestLocalBuyNotDroppedByAutoAcceptRebuild(t *testing.T) {
	dir := t.TempDir()
	ls, err := dgstore.Open(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatalf("dgstore.Open: %v", err)
	}
	t.Cleanup(func() { ls.Close() }) //nolint:errcheck

	operatorKey := newReservationID()
	eng := NewEngine(EngineOptions{
		CampfireID:        "local",
		LocalStore:        ls,
		OperatorPublicKey: operatorKey,
		PollInterval:      20 * time.Millisecond,
		Logger:            func(format string, args ...any) { t.Logf("[engine] "+format, args...) },
	})

	// --- Simulate the state the engine reaches just after startup ---
	// Startup fold on the (empty) log; both cursors begin at 0. We drive the
	// engine synchronously (no goroutines) so the interleaving is deterministic.
	if err := eng.replayAll(); err != nil {
		t.Fatalf("initial replayAll: %v", err)
	}

	// Seed inventory: append put1 and accept it so there is a matchable entry.
	put1 := newReservationID()
	seller := newReservationID()
	if err := ls.Append(dgstore.Record{
		ID:         put1,
		CampfireID: "local",
		Sender:     seller,
		Payload:    localBuyDropPutPayload(t, "Go HTTP handler unit test generator", 8000),
		Tags:       []string{TagPut, "exchange:content-type:code", "exchange:domain:go"},
		Timestamp:  time.Now().UnixNano(),
	}); err != nil {
		t.Fatalf("append put1: %v", err)
	}
	if err := eng.AutoAcceptPut(put1, 5600, time.Now().Add(72*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut(put1): %v", err)
	}
	if got := len(eng.State().Inventory()); got != 1 {
		t.Fatalf("expected 1 inventory entry after accept, got %d", got)
	}

	// --- The bug window ---
	// A buy is appended to the log (as the out-of-process `dontguess buy` CLI
	// would) but the poll loop has NOT run yet, so it is folded-but-undispatched.
	buyID := newReservationID()
	buyer := newReservationID()
	if err := ls.Append(dgstore.Record{
		ID:         buyID,
		CampfireID: "local",
		Sender:     buyer,
		Payload:    localBuyDropBuyPayload(t, "Generate unit tests for a Go HTTP handler", 50000),
		Tags:       []string{TagBuy},
		Timestamp:  time.Now().UnixNano(),
	}); err != nil {
		t.Fatalf("append buy: %v", err)
	}

	// Before the poll loop dispatches the buy, a second put arrives and the
	// auto-accept path runs. Its refresh rebuilds State from the full log —
	// folding the buy and (pre-fix) advancing the shared cursor past it without
	// dispatching it.
	put2 := newReservationID()
	if err := ls.Append(dgstore.Record{
		ID:         put2,
		CampfireID: "local",
		Sender:     seller,
		Payload:    localBuyDropPutPayload(t, "Go JSON marshaller fuzz corpus", 9000),
		Tags:       []string{TagPut, "exchange:content-type:code", "exchange:domain:go"},
		Timestamp:  time.Now().UnixNano(),
	}); err != nil {
		t.Fatalf("append put2: %v", err)
	}
	if err := eng.AutoAcceptPut(put2, 6300, time.Now().Add(72*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut(put2): %v", err)
	}

	// The poll loop runs. Pre-fix, len(log) <= cursor, so it dispatches nothing
	// and the buy is lost. Post-fix, the buy was already dispatched by the
	// rebuild's gap dispatch (and this poll is a harmless no-op for it).
	if err := eng.pollLocalStore(); err != nil {
		t.Fatalf("pollLocalStore: %v", err)
	}

	if !eng.State().IsOrderMatched(buyID) {
		t.Fatalf("buy %s was folded into State but never dispatched/matched "+
			"(dontguess-b84: a state rebuild advanced the dispatch cursor past an undispatched buy)", buyID[:8])
	}
}

// localBuyDropPutPayload builds a minimal valid exchange:put payload whose
// content satisfies the content-size plausibility check.
func localBuyDropPutPayload(t *testing.T, desc string, tokenCost int64) []byte {
	t.Helper()
	prefix := []byte("cached inference result: " + desc + " ")
	size := int(tokenCost/MaxTokensPerByte) + 1024
	content := make([]byte, size)
	copy(content, prefix)
	for i := len(prefix); i < size; i++ {
		content[i] = byte('a' + i%26)
	}
	p, err := json.Marshal(map[string]any{
		"description":  desc,
		"content":      base64.StdEncoding.EncodeToString(content),
		"token_cost":   tokenCost,
		"content_type": "exchange:content-type:code",
		"domains":      []string{"go"},
	})
	if err != nil {
		t.Fatalf("marshal put payload: %v", err)
	}
	return p
}

func localBuyDropBuyPayload(t *testing.T, task string, budget int64) []byte {
	t.Helper()
	p, err := json.Marshal(map[string]any{
		"task":        task,
		"budget":      budget,
		"max_results": 3,
	})
	if err != nil {
		t.Fatalf("marshal buy payload: %v", err)
	}
	return p
}
