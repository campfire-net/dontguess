package exchange_test

// TestLocalStore_FoldsIdenticallyToCampfireStore is the ground-source proof
// for dontguess-331 (local event store backs the exchange, zero campfire
// import in pkg/store): it drives a REAL exchange flow (put -> put-accept ->
// buy -> engine-emitted match) through the existing campfire-backed test
// harness (real SQLite store, real engine, real matching), then re-derives
// exchange state a SECOND, independent way — by copying the exact same
// messages into a real pkg/store.Store (the new campfire-free append log),
// reading them back via Store.Replay, and folding the result through the
// same exchange.State.Replay every other path uses.
//
// No mocked store and no fake fold: both sides use the real State.Replay and
// a real pkg/store.Store backed by an actual file on disk. If the two
// resulting states diverge, either Record's field mapping or the fold itself
// is not conversion-safe between the campfire transport and the local store.

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/campfire-net/campfire/cf-protocol/store"

	"github.com/campfire-net/dontguess/pkg/exchange"
	dgstore "github.com/campfire-net/dontguess/pkg/store"
)

func TestLocalStore_FoldsIdenticallyToCampfireStore(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	// --- put ---
	putMsg := h.sendMessage(h.seller,
		putPayload("Go HTTP handler unit test generator", "sha256:"+fmt.Sprintf("%064x", 1), "code", 12000, 20000),
		[]string{exchange.TagPut, "exchange:content-type:code", "exchange:domain:go"},
		nil,
	)
	msgs, err := h.st.ListMessages(h.cfID, 0)
	if err != nil {
		t.Fatalf("listing messages: %v", err)
	}
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	// --- put-accept ---
	if err := eng.AutoAcceptPut(putMsg.ID, 8400, time.Now().Add(168*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut: %v", err)
	}

	// --- buy ---
	buyMsg := h.sendMessage(h.buyer,
		buyPayload("Generate unit tests for a Go HTTP handler accepting JSON POST", 50000),
		[]string{exchange.TagBuy},
		nil,
	)
	buyRec, err := h.st.GetMessage(buyMsg.ID)
	if err != nil {
		t.Fatalf("getting buy message: %v", err)
	}
	eng.State().Apply(exchange.FromStoreRecord(buyRec))

	// --- run the engine so it emits a real match message ---
	preMatch, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = eng.Start(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	var matchMsgs []store.MessageRecord
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		matchMsgs, _ = h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})
		if len(matchMsgs) > len(preMatch) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	<-done
	if len(matchMsgs) <= len(preMatch) {
		t.Fatal("no match message emitted by engine — cannot prove fold equivalence over a match event")
	}

	// --- Canonical message log: everything the campfire store recorded. ---
	allMsgs, err := h.st.ListMessages(h.cfID, 0)
	if err != nil {
		t.Fatalf("listing all messages: %v", err)
	}
	if len(allMsgs) < 4 {
		t.Fatalf("expected at least 4 messages (put, put-accept, buy, match), got %d", len(allMsgs))
	}

	// --- Reference fold: replay the canonical campfire-sourced log directly. ---
	reference := exchange.NewState()
	reference.Replay(exchange.FromStoreRecords(allMsgs))

	// --- Local-store fold: copy the SAME messages, in the SAME order, into a
	// real pkg/store.Store, read them back via Replay, and fold that. ---
	localStore, err := dgstore.Open(t.TempDir() + "/events.jsonl")
	if err != nil {
		t.Fatalf("dgstore.Open: %v", err)
	}
	defer localStore.Close() //nolint:errcheck

	for _, rec := range allMsgs {
		err := localStore.Append(dgstore.Record{
			ID:          rec.ID,
			CampfireID:  rec.CampfireID,
			Sender:      rec.Sender,
			Payload:     rec.Payload,
			Tags:        rec.Tags,
			Antecedents: rec.Antecedents,
			Timestamp:   rec.Timestamp,
			Instance:    rec.Instance,
		})
		if err != nil {
			t.Fatalf("localStore.Append(%s): %v", rec.ID[:8], err)
		}
	}

	localMsgs, err := localStore.Replay()
	if err != nil {
		t.Fatalf("localStore.Replay: %v", err)
	}
	if len(localMsgs) != len(allMsgs) {
		t.Fatalf("localStore.Replay returned %d messages, want %d", len(localMsgs), len(allMsgs))
	}

	underTest := exchange.NewState()
	underTest.Replay(localMsgs)

	// --- Compare the two independently-folded states on every exported
	// accessor that reflects state derived from the message log. ---
	if !reflect.DeepEqual(reference.Inventory(), underTest.Inventory()) {
		t.Errorf("Inventory diverged:\n  campfire fold: %+v\n  local-store fold: %+v",
			derefEntries(reference.Inventory()), derefEntries(underTest.Inventory()))
	}
	if !reflect.DeepEqual(reference.ActiveOrders(), underTest.ActiveOrders()) {
		t.Errorf("ActiveOrders diverged:\n  campfire fold: %+v\n  local-store fold: %+v",
			reference.ActiveOrders(), underTest.ActiveOrders())
	}
	if !reflect.DeepEqual(reference.PendingPuts(), underTest.PendingPuts()) {
		t.Errorf("PendingPuts diverged:\n  campfire fold: %+v\n  local-store fold: %+v",
			reference.PendingPuts(), underTest.PendingPuts())
	}
	if got, want := underTest.SellerReputation(h.seller.PublicKeyHex()), reference.SellerReputation(h.seller.PublicKeyHex()); got != want {
		t.Errorf("SellerReputation diverged: local-store fold = %d, campfire fold = %d", got, want)
	}
	if got, want := underTest.TaskCompletionRate(), reference.TaskCompletionRate(); got != want {
		t.Errorf("TaskCompletionRate diverged: local-store fold = %v, campfire fold = %v", got, want)
	}
	if got, want := underTest.IsOrderMatched(buyMsg.ID), reference.IsOrderMatched(buyMsg.ID); got != want {
		t.Errorf("IsOrderMatched(buy) diverged: local-store fold = %v, campfire fold = %v", got, want)
	}
	if len(reference.Inventory()) == 0 {
		t.Fatal("test setup produced empty inventory on the reference fold — assertions above would pass vacuously")
	}
}

func derefEntries(entries []*exchange.InventoryEntry) []exchange.InventoryEntry {
	out := make([]exchange.InventoryEntry, len(entries))
	for i, e := range entries {
		out[i] = *e
	}
	return out
}
