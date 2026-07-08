package main

// serve_relay_test.go — the M2 WIRING + ACCEPTANCE GATE (dontguess-4bd).
//
// These tests compose the SAME pieces attachRelayTransport wires into the serve
// path — a real local exchange.Engine sharing its LocalStore with a real Intake,
// Outbox, and Sequencer — against an IN-PROCESS FAKE RELAY (a live relay is
// dontguess-13f, infra-gated, out of scope). They prove the four acceptance-gate
// properties from docs/design/relay-transport.md §2.3/§2.4/§2.4a/§2.6:
//
//  1. ROUND-TRIP: a foreign put+buy arriving over the relay fold into the engine,
//     which matches and publishes its operator match back OUT to the relay; a
//     buyer settle referencing that match folds. Both transport legs compose with
//     the unchanged engine.
//  2. RESTART-SEED: after a restart, a re-broadcast OLD operator event (or the
//     operator's own echo) is deduped and NOT re-folded — no double-credit. A
//     mutation twin (no seed) proves the seed is load-bearing.
//  3. HOT-PATH ISOLATION: with the relay publish leg fully BLOCKED, buy/match keeps
//     flowing off the local fold at p99 < 50ms — relay reads are never on the hot
//     path.
//  4. (The full 32,209-LOC pkg/exchange + pkg/scrip suites pass UNCHANGED — run
//     separately; this file adds only new tests, mutates none.)

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/campfire-net/dontguess/pkg/exchange"
	"github.com/campfire-net/dontguess/pkg/identity"
	"github.com/campfire-net/dontguess/pkg/nostr"
	"github.com/campfire-net/dontguess/pkg/proto"
	"github.com/campfire-net/dontguess/pkg/relay"
	dgstore "github.com/campfire-net/dontguess/pkg/store"
)

// --- in-process fake relay ---------------------------------------------------

// fakeRelayConn is an in-process stand-in for a NIP-42 relay.Conn. It satisfies
// both frameReceiver (Recv) and frameSender (Send). On Send of an ["EVENT", ev]:
// it records the event, ACKs it with an ["OK", id, true] frame delivered back to
// the reader (unless blockOK), and — if echo is set — echoes the EVENT back to
// the subscriber (modeling a relay that re-delivers a published event to the
// operator's own subscription). REQ/CLOSE frames are accepted with no response.
// inject() pushes a foreign EVENT to the subscriber. All delivery rides one
// buffered channel the single reader loop drains, exactly as the production
// relay.Conn.Recv feeds runReader.
type fakeRelayConn struct {
	recvCh chan []byte

	mu      sync.Mutex
	events  []*identity.Event // every EVENT the "relay" received via Send
	echo    bool
	blockOK bool
}

func newFakeRelayConn(echo bool) *fakeRelayConn {
	return &fakeRelayConn{recvCh: make(chan []byte, 4096), echo: echo}
}

func (r *fakeRelayConn) setBlockOK(b bool) {
	r.mu.Lock()
	r.blockOK = b
	r.mu.Unlock()
}

func (r *fakeRelayConn) Send(_ context.Context, frame []byte) error {
	f, err := relay.ParseFrame(frame)
	if err != nil {
		return nil // tolerate anything non-parseable (defensive)
	}
	if f.Type != relay.LabelEVENT || f.Event == nil {
		return nil // REQ / CLOSE: accepted, no response
	}
	r.mu.Lock()
	r.events = append(r.events, f.Event)
	blockOK := r.blockOK
	echo := r.echo
	r.mu.Unlock()

	if !blockOK {
		ok, _ := relay.EncodeOK(f.Event.ID, true, "")
		r.recvCh <- ok
	}
	if echo {
		ev, _ := relay.EncodeSubEvent("dg-exchange", f.Event)
		r.recvCh <- ev
	}
	return nil
}

func (r *fakeRelayConn) Recv(ctx context.Context) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case f := <-r.recvCh:
		return f, nil
	}
}

// inject delivers a foreign signed event to the operator's subscription.
func (r *fakeRelayConn) inject(ev *identity.Event) {
	frame, _ := relay.EncodeSubEvent("dg-exchange", ev)
	r.recvCh <- frame
}

// receivedByKind returns every EVENT the relay received (via Send) of the given
// kind — the operator's published events on the wire.
func (r *fakeRelayConn) receivedByKind(kind int) []*identity.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*identity.Event
	for _, e := range r.events {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

// --- shared helpers ----------------------------------------------------------

// signExchangeEvent builds a proto.Message with the given tags/antecedents/
// payload authored by signer, converts it through the production adapter, and
// signs it — yielding a genuinely Schnorr-signed wire event that passes the
// Intake's universal signature floor. It is how a test synthesizes a foreign
// seller/buyer event the way another agent's client would publish it.
func signExchangeEvent(t *testing.T, signer identity.Signer, tags, antecedents []string, payload []byte) *identity.Event {
	t.Helper()
	msg := &proto.Message{
		CampfireID:  "local",
		Sender:      signer.PubKeyHex(),
		Payload:     payload,
		Tags:        tags,
		Antecedents: antecedents,
		Timestamp:   time.Now().UnixNano(),
	}
	nev, err := nostr.ToNostrEvent(msg)
	if err != nil {
		t.Fatalf("ToNostrEvent(%v): %v", tags, err)
	}
	ev := &identity.Event{
		ID:        nev.ID,
		PubKey:    nev.PubKey,
		CreatedAt: nev.CreatedAt,
		Kind:      nev.Kind,
		Tags:      nev.Tags,
		Content:   nev.Content,
	}
	if err := identity.SignEvent(signer, ev); err != nil {
		t.Fatalf("SignEvent: %v", err)
	}
	return ev
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for: %s", timeout, what)
}

func countTag(recs []dgstore.Record, tag string) int {
	n := 0
	for _, r := range recs {
		for _, tg := range r.Tags {
			if tg == tag {
				n++
			}
		}
	}
	return n
}

// --- 1. ROUND-TRIP -----------------------------------------------------------

// TestRelayRoundTrip_PutBuyMatchSettle_Folds drives the full transport round
// trip through the real serve-path wiring: a foreign put and buy arrive over the
// fake relay (Intake ingest leg -> LocalStore Origin=relay -> engine fold), the
// engine auto-accepts and MATCHES, the operator's match is published back OUT
// over the relay (Outbox publish leg), and a buyer settle referencing that match
// folds. Nothing is stubbed but the relay wire itself.
func TestRelayRoundTrip_PutBuyMatchSettle_Folds(t *testing.T) {
	dir := t.TempDir()
	ls, err := dgstore.Open(dir + "/events.jsonl")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = ls.Close() })

	operator, _ := identity.Generate()
	seller, _ := identity.Generate()
	buyer, _ := identity.Generate()

	eng := exchange.NewEngine(exchange.EngineOptions{
		CampfireID:        "local",
		LocalStore:        ls,
		OperatorPublicKey: operator.PubKeyHex(),
		PollInterval:      5 * time.Millisecond,
		Logger:            func(string, ...any) {},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	relayConn := newFakeRelayConn(true /* echo */)
	stop, err := attachRelayTransport(ctx, ls, operator, operator.PubKeyHex(),
		dir+"/events.jsonl.pubcursor", relayConn, relayConn, 5*time.Millisecond, nil)
	if err != nil {
		t.Fatalf("attachRelayTransport: %v", err)
	}

	// Engine + auto-accept loop (mirrors runEngineLoop).
	engDone := make(chan struct{})
	go func() { defer close(engDone); _ = eng.Start(ctx) }()
	go func() {
		skipped := map[string]struct{}{}
		tk := time.NewTicker(10 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-tk.C:
				eng.RunAutoAccept(1_000_000, now, skipped)
			}
		}
	}()
	t.Cleanup(func() {
		cancel()
		<-engDone
		stop()
	})

	// (a) INGEST LEG: a foreign put arrives over the relay, folds, and is
	// auto-accepted into inventory.
	putEv := signExchangeEvent(t, seller,
		[]string{exchange.TagPut, "exchange:content-type:code", "exchange:domain:go"}, nil,
		localPutPayload("Go HTTP handler unit test generator", 8000))
	relayConn.inject(putEv)
	waitFor(t, 8*time.Second, "put folds + auto-accepts into inventory", func() bool {
		return len(eng.State().Inventory()) == 1
	})

	// (b) HOT PATH + PUBLISH LEG: a foreign buy arrives over the relay, the engine
	// matches, and the operator's match is published back OUT to the relay.
	buyEv := signExchangeEvent(t, buyer,
		[]string{exchange.TagBuy}, nil,
		localBuyPayload("Generate unit tests for a Go HTTP handler", 50000))
	relayConn.inject(buyEv)

	waitFor(t, 8*time.Second, "match record folds into the local log", func() bool {
		recs, _ := ls.ReadAll()
		return countTag(recs, exchange.TagMatch) >= 1
	})
	waitFor(t, 8*time.Second, "operator match published OUT to the relay", func() bool {
		return len(relayConn.receivedByKind(nostr.KindMatch)) >= 1
	})

	// The published match must be operator-authored (author == operator pubkey):
	// the publish leg re-signed the operator's own record.
	matches := relayConn.receivedByKind(nostr.KindMatch)
	if matches[0].PubKey != operator.PubKeyHex() {
		t.Fatalf("published match author %s != operator %s", matches[0].PubKey, operator.PubKeyHex())
	}
	matchSignedID := matches[0].ID

	// (c) SETTLE LEG: a buyer settle referencing the published match folds. The
	// match's signed id is in the emitted-set (seeded before publish), so the
	// settle's antecedent is satisfied and it drains + persists (Origin=relay).
	settlePayload, _ := json.Marshal(map[string]any{"match_id": matchSignedID})
	settleEv := signExchangeEvent(t, buyer,
		[]string{exchange.TagSettle, exchange.TagPhasePrefix + string(exchange.SettlePhaseBuyerAccept)},
		[]string{matchSignedID}, settlePayload)
	relayConn.inject(settleEv)

	waitFor(t, 8*time.Second, "buyer settle folds into the local log (Origin=relay)", func() bool {
		recs, _ := ls.ReadAll()
		for _, r := range recs {
			if r.Origin != "relay" {
				continue
			}
			for _, tg := range r.Tags {
				if tg == exchange.TagSettle {
					return true
				}
			}
		}
		return false
	})

	// The operator's own echoes (put-accept, match) that the relay re-delivered
	// must have deduped — the engine must not have double-folded its own match.
	recs, _ := ls.ReadAll()
	if got := countTag(recs, exchange.TagMatch); got != 1 {
		t.Fatalf("local log has %d match records, want exactly 1 — an echoed operator match double-folded", got)
	}
}

// --- 2. RESTART-SEED ---------------------------------------------------------

// TestRelayRestartReseed_NoDoubleFold proves the STARTUP RESTART-SEED (§2.2 + the
// 15f/2f0 reviews). A prior run persisted an operator match (Origin=local). A new
// process starts (fresh Sequencer with an EMPTY in-memory emitted-set) and
// rebuilds the wiring, which re-seeds the emitted-set from the persisted log.
// Then the relay re-broadcasts that OLD operator match — carrying its SIGNED
// content-hash id, the id it had on the wire. With the restart-seed the Sequencer
// dedups it: the Intake persists NOTHING (no double-fold, no double scrip
// credit). The subtest twin flips the SINGLE variable — no restart-seed — and
// proves the re-broadcast DOES double-fold, so the assertion is load-bearing.
func TestRelayRestartReseed_NoDoubleFold(t *testing.T) {
	// A helper that persists one operator match record and returns the SIGNED
	// content-hash id the relay would carry for it (the Outbox's derivation).
	persistOperatorMatch := func(t *testing.T, ls *dgstore.Store, operator identity.Signer) (string, dgstore.Record) {
		t.Helper()
		rec := dgstore.Record{
			ID:         randomLocalMsgID(t),
			CampfireID: "local",
			Sender:     operator.PubKeyHex(),
			Payload:    []byte(`{"buy_id":"b1","entry_id":"e1"}`),
			Tags:       []string{exchange.TagMatch},
			Timestamp:  time.Now().UnixNano(),
			Origin:     "local",
		}
		if err := ls.Append(rec); err != nil {
			t.Fatalf("append operator match: %v", err)
		}
		signedID, err := signedEventID(rec, operator)
		if err != nil {
			t.Fatalf("signedEventID: %v", err)
		}
		if signedID == rec.ID {
			t.Fatalf("signed id equals pre-sign store id — invalid test premise")
		}
		return signedID, rec
	}

	t.Run("with restart-seed: re-broadcast old operator match dedups", func(t *testing.T) {
		dir := t.TempDir()
		ls, err := dgstore.Open(dir + "/events.jsonl")
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		t.Cleanup(func() { _ = ls.Close() })
		operator, _ := identity.Generate()

		signedID, _ := persistOperatorMatch(t, ls, operator)

		// RESTART: build the wiring anew. buildRelayWiring runs the restart-seed
		// over the persisted log (seeding the operator record's SIGNED id).
		w, _, err := buildRelayWiring(ls, operator, operator.PubKeyHex(),
			dir+"/events.jsonl.pubcursor", &nopPublisher{}, 0, func(string, error, *nostr.Event) {})
		if err != nil {
			t.Fatalf("buildRelayWiring: %v", err)
		}

		// The relay re-broadcasts the OLD operator match, re-signed exactly as the
		// Outbox published it (same signed content-hash id).
		rebroadcast := reSignPersisted(t, ls, operator, signedID)
		if err := w.intake.HandleEvent(identityToNostrEvent(rebroadcast)); err != nil {
			t.Fatalf("HandleEvent(re-broadcast) returned error, want silent dedup: %v", err)
		}

		if got := w.metrics.Persisted.Load(); got != 0 {
			t.Fatalf("Intake persisted %d records from the re-broadcast, want 0 — restart double-fold", got)
		}
		recs, _ := ls.ReadAll()
		if got := countTag(recs, exchange.TagMatch); got != 1 {
			t.Fatalf("log has %d match records after re-broadcast, want 1 (no double-apply)", got)
		}
		if got := w.metrics.Received.Load(); got != 1 {
			t.Fatalf("Intake received %d events, want 1 — the re-broadcast must reach the pipeline for the 0-persist to be a real dedup", got)
		}
	})

	t.Run("mutation twin: NO restart-seed re-folds the old operator match", func(t *testing.T) {
		dir := t.TempDir()
		ls, err := dgstore.Open(dir + "/events.jsonl")
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		t.Cleanup(func() { _ = ls.Close() })
		operator, _ := identity.Generate()

		signedID, _ := persistOperatorMatch(t, ls, operator)

		// Deliberately DO NOT restart-seed: a bare Sequencer with an empty
		// emitted-set, wired straight into an Intake — the exact restart state the
		// seed exists to fix.
		seq := exchange.NewSequencer(0)
		mx := &relay.IntakeMetrics{}
		in := relay.NewIntake(seq, ls, operator.PubKeyHex(), mx, func(string, error, *nostr.Event) {})

		rebroadcast := reSignPersisted(t, ls, operator, signedID)
		if err := in.HandleEvent(identityToNostrEvent(rebroadcast)); err != nil {
			t.Fatalf("HandleEvent(re-broadcast) unexpected error: %v", err)
		}

		if got := mx.Persisted.Load(); got != 1 {
			t.Fatalf("without the restart-seed the re-broadcast must double-fold: persisted %d, want 1 (proves the seed is load-bearing)", got)
		}
		recs, _ := ls.ReadAll()
		if got := countTag(recs, exchange.TagMatch); got != 2 {
			t.Fatalf("without the seed the log must have 2 match records (the double-apply), got %d", got)
		}
	})
}

// reSignPersisted rebuilds the wire event for the persisted operator record whose
// signed id is wantSignedID — exactly the event the Outbox published and the
// relay would re-broadcast. It asserts the rebuilt id matches (the derivation is
// deterministic), so the test's "re-broadcast" is byte-faithful to the wire.
func reSignPersisted(t *testing.T, ls *dgstore.Store, operator identity.Signer, wantSignedID string) *identity.Event {
	t.Helper()
	recs, err := ls.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	for _, rec := range recs {
		if !isOperatorOrigin(rec.Origin) {
			continue
		}
		msg := rec.ToMessage()
		nev, err := nostr.ToNostrEvent(&msg)
		if err != nil {
			continue
		}
		ev := &identity.Event{
			ID: nev.ID, PubKey: nev.PubKey, CreatedAt: nev.CreatedAt,
			Kind: nev.Kind, Tags: nev.Tags, Content: nev.Content,
		}
		if err := identity.SignEvent(operator, ev); err != nil {
			t.Fatalf("SignEvent: %v", err)
		}
		if ev.ID == wantSignedID {
			return ev
		}
	}
	t.Fatalf("no persisted operator record re-signs to %s", wantSignedID)
	return nil
}

// nopPublisher is an EventPublisher that ACKs without a relay. Used where the
// publish leg is irrelevant to the property under test (the restart-seed).
type nopPublisher struct{}

func (nopPublisher) PublishEvent(context.Context, *identity.Event) (bool, error) { return true, nil }

// --- 3. HOT-PATH ISOLATION ---------------------------------------------------

// TestRelayHotPath_BuyMatchP99_UnderBlockedRelay proves the buy/match hot path is
// isolated from relay latency (§2.4). The relay publish leg is fully BLOCKED (the
// fake never ACKs, so the Outbox's first publish blocks forever), yet buys folded
// through the LOCAL log continue to match — because the engine reads only its
// LocalStore fold and never the relay. It measures per-buy buy->match latency
// serially and asserts p99 < 50ms, AND asserts the Outbox published+ACKed ZERO
// events (the relay is provably stalled) while every buy still matched.
func TestRelayHotPath_BuyMatchP99_UnderBlockedRelay(t *testing.T) {
	dir := t.TempDir()
	ls, err := dgstore.Open(dir + "/events.jsonl")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = ls.Close() })

	operator, _ := identity.Generate()
	seller, _ := identity.Generate()
	buyer, _ := identity.Generate()

	eng := exchange.NewEngine(exchange.EngineOptions{
		CampfireID:        "local",
		LocalStore:        ls,
		OperatorPublicKey: operator.PubKeyHex(),
		PollInterval:      2 * time.Millisecond,
		Logger:            func(string, ...any) {},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Pre-populate inventory: several accepted puts so any buy finds a match.
	const invN = 6
	for i := 0; i < invN; i++ {
		// Unique content per put: identical payloads dedup by content_hash and only
		// the first would register as a pending put.
		putEv := signExchangeEvent(t, seller,
			[]string{exchange.TagPut, "exchange:content-type:code", "exchange:domain:go"}, nil,
			localPutPayload(fmt.Sprintf("Go HTTP handler unit test generator variant %d", i), 8000))
		putMsg, _ := nostr.FromNostrEvent(identityToNostrEvent(putEv))
		if err := ls.Append(dgstore.Record{
			ID: putMsg.ID, CampfireID: "local", Sender: putMsg.Sender,
			Payload: putMsg.Payload, Tags: putMsg.Tags, Timestamp: putMsg.Timestamp,
		}); err != nil {
			t.Fatalf("append put %d: %v", i, err)
		}
		if err := eng.AutoAcceptPut(putMsg.ID, 5600, time.Now().Add(72*time.Hour)); err != nil {
			t.Fatalf("AutoAcceptPut %d: %v", i, err)
		}
	}
	if len(eng.State().Inventory()) != invN {
		t.Fatalf("pre-populated inventory = %d, want %d", len(eng.State().Inventory()), invN)
	}

	// Attach the relay with a BLOCKED publish leg (no ACKs ever).
	relayConn := newFakeRelayConn(false /* no echo */)
	relayConn.setBlockOK(true)
	// Build the wiring directly so we can read the Outbox cursor for the
	// isolation assertion; run the reader + Outbox as attachRelayTransport does.
	pub := newDemuxPublisher(relayConn)
	w, _, err := buildRelayWiring(ls, operator, operator.PubKeyHex(),
		dir+"/events.jsonl.pubcursor", pub, 0, func(string, error, *nostr.Event) {})
	if err != nil {
		t.Fatalf("buildRelayWiring: %v", err)
	}
	go w.runReader(ctx, relayConn, pub)
	go w.outbox.Run(ctx, 2*time.Millisecond)

	engDone := make(chan struct{})
	go func() { defer close(engDone); _ = eng.Start(ctx) }()
	t.Cleanup(func() { cancel(); <-engDone })

	// Stream buys through the LOCAL fold, serially, timing each buy->match.
	const buyN = 50
	latencies := make([]time.Duration, 0, buyN)
	for i := 0; i < buyN; i++ {
		buyEv := signExchangeEvent(t, buyer,
			[]string{exchange.TagBuy}, nil,
			localBuyPayload("Generate unit tests for a Go HTTP handler", 50000))
		buyMsg, _ := nostr.FromNostrEvent(identityToNostrEvent(buyEv))
		start := time.Now()
		if err := ls.Append(dgstore.Record{
			ID: buyMsg.ID, CampfireID: "local", Sender: buyMsg.Sender,
			Payload: buyMsg.Payload, Tags: buyMsg.Tags, Timestamp: buyMsg.Timestamp,
		}); err != nil {
			t.Fatalf("append buy %d: %v", i, err)
		}
		want := i + 1
		waitFor(t, 5*time.Second, "buy matches off the local fold", func() bool {
			recs, _ := ls.ReadAll()
			return countTag(recs, exchange.TagMatch) >= want
		})
		latencies = append(latencies, time.Since(start))
	}

	// ISOLATION: the relay publish leg is provably stalled — zero events
	// published+ACKed — yet every buy matched.
	if got := w.outbox.Cursor(); got != 0 {
		t.Fatalf("Outbox cursor = %d, want 0 — the relay was supposed to be fully blocked (publish must not have progressed)", got)
	}
	if lag := w.outbox.PublishLag(); lag == 0 {
		t.Fatalf("Outbox publish_lag = 0 under a blocked relay, want > 0 — the publish leg was not actually exercised/stalled")
	}

	sort.Slice(latencies, func(a, b int) bool { return latencies[a] < latencies[b] })
	p99 := latencies[(len(latencies)*99)/100]
	if p99 >= 50*time.Millisecond {
		t.Fatalf("buy/match p99 = %s, want < 50ms — the hot path is NOT isolated from the blocked relay", p99)
	}
	t.Logf("hot-path buy/match p99 = %s over %d buys (relay publish fully blocked, lag=%d)", p99, buyN, w.outbox.PublishLag())
}
