package main

// serve_relay_consume_test.go — the BLOCKER-CLOSED PROOF for dontguess-d52
// (team-tier Outbox FATAL on the settle(complete)->consume operator record).
//
// Before d52 the operator's exchange:consume behavioral signal had no nostr kind:
// nostr.ToNostrEvent returned "carries no recognised exchange operation tag", the
// Outbox treated that conversion failure as FATAL and KILLED the publish loop
// (pkg/relay/outbox.go), stranding every subsequent operator record at RF=1. A
// team-tier settle(complete) is the first real trigger of emitConsumeSignal over a
// live Outbox, so it detonated the whole publish leg.
//
// This test drives the full team-tier money path — put->buy->match->buyer-accept->
// (auto)deliver->complete — over the exact serve-path wiring (engine + Intake +
// Outbox + Sequencer + LocalScripStore + wire->store alias) against the in-process
// fake relay, then proves:
//
//  1. the settle(complete)->consume record is PUBLISHED (a KindConsume=3406 event
//     appears on the fake relay's received set) and round-trips back to the exact
//     [exchange:consume] proto.Message a relay reader folds;
//  2. the Outbox did NOT go FATAL (no "outbox: FATAL" log line captured); and
//  3. the publish loop is still alive with publish_lag drained to 0 — EVERY
//     operator record in the chain (including the consume) is on the wire, i.e.
//     nothing is stranded at RF=1, and every one is nostr-serializable.

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"sync"
	"testing"
	"time"

	"github.com/campfire-net/dontguess/pkg/exchange"
	"github.com/campfire-net/dontguess/pkg/identity"
	"github.com/campfire-net/dontguess/pkg/nostr"
	dgstore "github.com/campfire-net/dontguess/pkg/store"
)

// syncLogBuf is a goroutine-safe log sink: the Outbox publish loop runs on its own
// goroutine and its FATAL (if any) is emitted via the standard log package, so the
// capture must be locked.
type syncLogBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncLogBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncLogBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// receivedIDs returns the set of every EVENT id the fake relay has received via
// Send (the operator's published wire ids), regardless of kind.
func (r *fakeRelayConn) receivedIDs() map[string]struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]struct{}, len(r.events))
	for _, e := range r.events {
		out[e.ID] = struct{}{}
	}
	return out
}

// TestRelayTeamTierConsumePublishesNoOutboxFatal is the d52 blocker-closed proof.
func TestRelayTeamTierConsumePublishesNoOutboxFatal(t *testing.T) {
	// Capture the standard logger for the whole test so an Outbox FATAL is
	// observable. Registered FIRST so it restores LAST — after the stack's
	// cancel+stop cleanup drains the outbox goroutine, so no write races the
	// restore.
	var logbuf syncLogBuf
	prevOut := log.Writer()
	log.SetOutput(&logbuf)
	t.Cleanup(func() { log.SetOutput(prevOut) })

	dir := t.TempDir()
	ls, err := dgstore.Open(dir + "/events.jsonl")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = ls.Close() })

	operator, _ := identity.Generate()
	seller, _ := identity.Generate()
	buyer, _ := identity.Generate()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	st := newWireIDStack(t, ctx, ls, operator, dir+"/events.jsonl.pubcursor")
	t.Cleanup(func() { cancel(); st.stop() })
	st.mintBuyer(t, buyer)

	// --- put -> buy -> match (operator match published) ---
	matchWire, matchStore := st.driveToMatch(t, seller, buyer, operator)

	// --- buyer-accept e-tagging the match WIRE id -> real hold -> auto-deliver ---
	acceptPayload, _ := json.Marshal(map[string]any{"entry_id": ""})
	acceptEv := signExchangeEvent(t, buyer,
		[]string{exchange.TagSettle, exchange.TagPhasePrefix + exchange.SettlePhaseStrBuyerAccept},
		[]string{matchWire}, acceptPayload)
	st.conn.inject(acceptEv)

	waitFor(t, 8*time.Second, "buyer debited a real hold on the wire-id buyer-accept", func() bool {
		return st.scrip.Balance(buyer.PubKeyHex()) < wireIDBuyerMint
	})

	var deliverWire string
	waitFor(t, 8*time.Second, "operator auto-delivered content and published it", func() bool {
		recs, _ := st.ls.ReadAll()
		dr, ok := firstLocalRecordWithTags(recs, exchange.TagSettle, deliverPhaseTag)
		if !ok {
			return false
		}
		w, derr := signedEventID(dr, operator)
		if derr != nil {
			return false
		}
		for _, ev := range st.conn.receivedByKind(nostr.KindSettle) {
			if ev.ID == w {
				deliverWire = w
				return true
			}
		}
		return false
	})

	// --- complete e-tagging the deliver WIRE id -> settle + emitConsumeSignal ---
	completePayload, _ := json.Marshal(map[string]any{"content_hash_verified": true})
	completeEv := signExchangeEvent(t, buyer,
		[]string{exchange.TagSettle, exchange.TagPhasePrefix + exchange.SettlePhaseStrComplete, exchange.TagVerdictPrefix + "accepted"},
		[]string{deliverWire}, completePayload)
	st.conn.inject(completeEv)

	waitFor(t, 8*time.Second, "match settles on the wire-id complete", func() bool {
		return st.eng.State().IsMatchSettled(matchStore)
	})

	// ===================== d52 PROOF (1): consume PUBLISHES =====================
	// Before d52 this never fires: the consume record FATALs the Outbox and never
	// reaches the relay. After d52 it publishes as KindConsume=3406.
	waitFor(t, 8*time.Second, "settle(complete)->consume record is PUBLISHED to the relay (KindConsume)", func() bool {
		return len(st.conn.receivedByKind(nostr.KindConsume)) >= 1
	})

	consumeEvents := st.conn.receivedByKind(nostr.KindConsume)
	if len(consumeEvents) != 1 {
		t.Fatalf("consume events on the wire = %d, want exactly 1", len(consumeEvents))
	}
	cev := consumeEvents[0]
	if cev.Kind != nostr.KindConsume {
		t.Fatalf("consume kind = %d, want KindConsume %d", cev.Kind, nostr.KindConsume)
	}
	if cev.PubKey != operator.PubKeyHex() {
		t.Fatalf("consume author = %s, want operator %s (consume is operator-authored)", cev.PubKey, operator.PubKeyHex())
	}

	// The published consume must ROUND-TRIP back to the exact [exchange:consume]
	// proto.Message the unchanged engine folds — kind resolves, the op tag is
	// reconstructed, the complete-msg antecedent survives, and the payload is
	// preserved. This is what lets a relay reader recompute the Layer-0..4 metrics.
	foldedConsume, err := nostr.FromNostrEvent(identityToNostrEvent(cev))
	if err != nil {
		t.Fatalf("published consume did not round-trip via FromNostrEvent: %v", err)
	}
	if len(foldedConsume.Tags) != 1 || foldedConsume.Tags[0] != exchange.TagConsume {
		t.Fatalf("round-tripped consume tags = %v, want exactly [%q]", foldedConsume.Tags, exchange.TagConsume)
	}
	if len(foldedConsume.Antecedents) != 1 || foldedConsume.Antecedents[0] != completeEv.ID {
		t.Fatalf("round-tripped consume antecedent = %v, want the complete wire id [%s]", foldedConsume.Antecedents, completeEv.ID)
	}
	var cp struct {
		EntryID  string `json:"entry_id"`
		BuyerKey string `json:"buyer_key"`
	}
	if err := json.Unmarshal(foldedConsume.Payload, &cp); err != nil {
		t.Fatalf("round-tripped consume payload is not the engine's consume payload: %v", err)
	}
	if cp.EntryID == "" {
		t.Fatalf("round-tripped consume payload has empty entry_id — the behavioral signal would not count")
	}
	if cp.BuyerKey != buyer.PubKeyHex() {
		t.Fatalf("round-tripped consume buyer_key = %s, want buyer %s", cp.BuyerKey, buyer.PubKeyHex())
	}

	// The folded consume must drive the UNCHANGED engine's fold identically — a
	// relay reader increments the per-entry consume signal that feeds the Layer
	// metrics/pricing booster. Prove the fold seam through the public accessor.
	rst := exchange.NewState()
	rst.OperatorKey = operator.PubKeyHex()
	rst.Apply(foldedConsume)
	if got := rst.AllEntryBehavioralSignals()[cp.EntryID].ConsumeCount; got != 1 {
		t.Fatalf("relay-reader fold of the published consume set ConsumeCount=%d, want 1", got)
	}

	// ============ d52 PROOF (3): loop alive + publish_lag drained to 0 ===========
	// EVERY operator-authored record in the chain is on the wire — the consume did
	// not strand anything at RF=1 and the publish loop kept draining.
	allOperatorRecordsPublished := func() bool {
		recs, _ := st.ls.ReadAll()
		got := st.conn.receivedIDs()
		for _, r := range recs {
			if !isOperatorOrigin(r.Origin) {
				continue
			}
			w, derr := signedEventID(r, operator)
			if derr != nil {
				return false
			}
			if _, ok := got[w]; !ok {
				return false
			}
		}
		return true
	}
	waitFor(t, 8*time.Second, "publish_lag drains to 0: every operator record (incl. consume) is on the relay", allOperatorRecordsPublished)

	// Direct serializability assertion: NO operator record in a real team-tier
	// settle chain may fail ToNostrEvent (which is what FATALed the Outbox).
	recs, _ := st.ls.ReadAll()
	nOperator := 0
	for _, r := range recs {
		if !isOperatorOrigin(r.Origin) {
			continue
		}
		nOperator++
		if _, derr := signedEventID(r, operator); derr != nil {
			t.Fatalf("operator record %s (tags %v) is NOT nostr-serializable: %v — it would FATAL the Outbox", r.ID, r.Tags, derr)
		}
	}
	if nOperator == 0 {
		t.Fatalf("no operator records were emitted — the team-tier chain did not run")
	}

	// ===================== d52 PROOF (2): no Outbox FATAL =======================
	if logs := logbuf.String(); bytes.Contains([]byte(logs), []byte("outbox: FATAL")) {
		t.Fatalf("Outbox went FATAL during the team-tier settle(complete)->consume flow:\n%s", logs)
	}
}
