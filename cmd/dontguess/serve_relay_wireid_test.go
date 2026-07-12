package main

// serve_relay_wireid_test.go — the MONEY PROOF for dontguess-55c (team-tier
// settle wire-id reconciliation + operator auto-deliver). These tests drive REAL
// scrip end-to-end through the exact serve-path wiring (engine + Intake + Outbox +
// Sequencer + LocalScripStore + the wire→store alias) against the in-process fake
// relay, with a MINTED buyer — the money motion the ~32K in-process exchange suite
// structurally cannot exercise, because it drives settle in-process with
// consistent store ids and never crosses the relay's wire-id boundary.
//
// They pin the four properties the ruling requires beyond the frozen suite:
//
//  1. TestRelayTeamTierWireIDSettle — a buyer-accept e-tagging the published match
//     WIRE id moves REAL scrip: buyer debited price+fee (a real hold), a content
//     deliver is auto-emitted+published, and a complete e-tagging the deliver WIRE
//     id credits the seller residual and settles the match.
//  2. TestRelayRestartWireIDSettle — the same, but the buyer-accept lands against a
//     FRESH engine whose wire→store alias was rebuilt purely by
//     buildRelayWiring→seedEmittedFromStore over the persisted log (restart).
//  3. TestRelayAutoDeliverExactlyOnce — a re-accept and a post-settlement re-accept
//     emit NO second deliver: the operator emits exactly one content deliver/match.

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"testing"
	"time"

	"github.com/campfire-net/dontguess/pkg/exchange"
	"github.com/campfire-net/dontguess/pkg/identity"
	"github.com/campfire-net/dontguess/pkg/nostr"
	"github.com/campfire-net/dontguess/pkg/scrip"
	dgstore "github.com/campfire-net/dontguess/pkg/store"
)

const wireIDBuyerMint = int64(1_000_000)

// hushRelayLogs silences the default logger for the duration of a test. The team-tier
// Outbox tail cannot yet nostr-encode the operator's own exchange:consume behavioral
// signal (no KindConsume in the adapter — a PRE-EXISTING gap, see the item followups),
// so after a settle(complete) it hot-loops "outbox: FATAL cannot convert/sign …" to
// stderr on every tick. That spam is orthogonal to the wire-id money flow under test but
// its synchronous stderr I/O + CPU burn measurably amplifies the suite's pre-existing
// timing-sensitive tests. Silencing it here keeps the wire-id tests from being that
// amplifier. Sequential tests only (no t.Parallel), so the global swap is safe; restored
// on cleanup.
func hushRelayLogs(t *testing.T) {
	t.Helper()
	prev := log.Writer()
	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(prev) })
}

// recordHasAllTags reports whether r carries every tag in want.
func recordHasAllTags(r dgstore.Record, want ...string) bool {
	for _, w := range want {
		found := false
		for _, tg := range r.Tags {
			if tg == w {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// firstLocalRecordWithTags returns the first OPERATOR-authored (Origin local)
// record carrying all of wantTags, and whether one exists.
func firstLocalRecordWithTags(recs []dgstore.Record, wantTags ...string) (dgstore.Record, bool) {
	for _, r := range recs {
		if !isOperatorOrigin(r.Origin) {
			continue
		}
		if recordHasAllTags(r, wantTags...) {
			return r, true
		}
	}
	return dgstore.Record{}, false
}

// countLocalRecordsWithTags counts OPERATOR-authored records carrying all wantTags.
func countLocalRecordsWithTags(recs []dgstore.Record, wantTags ...string) int {
	n := 0
	for _, r := range recs {
		if !isOperatorOrigin(r.Origin) {
			continue
		}
		if recordHasAllTags(r, wantTags...) {
			n++
		}
	}
	return n
}

// deliverPhaseTag is the operator content-deliver's phase tag.
var deliverPhaseTag = exchange.TagPhasePrefix + exchange.SettlePhaseStrDeliver

// wireIDStack is one team-tier serve process composed the way runServeLocal does:
// a real engine sharing its LocalStore with a real Intake/Outbox/Sequencer, a real
// LocalScripStore, and the wire→store alias wired into the Outbox + restart-seed.
type wireIDStack struct {
	ls    *dgstore.Store
	eng   *exchange.Engine
	scrip *scrip.LocalScripStore
	conn  *fakeRelayConn
	stop  func()
}

// newWireIDStack builds and starts a team-tier stack over ls (shared across a
// restart) with the given operator signer. It attaches the relay with the engine
// State's RegisterWireAlias, runs the engine + auto-accept loop, and returns a
// teardown closure. AutoDeliverOnBuyerAccept is on (the team-tier serve setting).
func newWireIDStack(t *testing.T, ctx context.Context, ls *dgstore.Store, operator identity.Signer, cursorPath string) *wireIDStack {
	t.Helper()

	ss, err := scrip.NewLocalScripStore(ls, operator.PubKeyHex())
	if err != nil {
		t.Fatalf("NewLocalScripStore: %v", err)
	}

	eng := exchange.NewEngine(exchange.EngineOptions{
		CampfireID:               "local",
		LocalStore:               ls,
		OperatorPublicKey:        operator.PubKeyHex(),
		ScripStore:               ss,
		AutoDeliverOnBuyerAccept: true,
		PollInterval:             10 * time.Millisecond,
		Logger:                   func(string, ...any) {},
	})

	conn := newFakeRelayConn(true /* echo */)
	// A 25ms publish interval (vs 5ms) still publishes match/deliver well within the 8s
	// waitFors, but slows the post-complete consume-FATAL retry loop (see hushRelayLogs)
	// to a light background rate instead of hammering a core.
	stop, err := attachRelayTransport(ctx, ls, operator, operator.PubKeyHex(),
		cursorPath, conn, conn, 25*time.Millisecond, nil, nil,
		eng.State().RegisterWireAlias)
	if err != nil {
		t.Fatalf("attachRelayTransport: %v", err)
	}

	engDone := make(chan struct{})
	go func() { defer close(engDone); _ = eng.Start(ctx) }()
	go func() {
		skipped := map[string]struct{}{}
		tk := time.NewTicker(15 * time.Millisecond)
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

	s := &wireIDStack{ls: ls, eng: eng, scrip: ss, conn: conn}
	s.stop = func() { <-engDone; stop() }
	return s
}

// mintBuyer credits the buyer enough scrip to cover any price+fee hold.
func (s *wireIDStack) mintBuyer(t *testing.T, buyer identity.Signer) {
	t.Helper()
	if _, _, err := s.scrip.AddBudget(context.Background(), buyer.PubKeyHex(), scrip.BalanceKey, wireIDBuyerMint, ""); err != nil {
		t.Fatalf("mint buyer: %v", err)
	}
}

// driveToMatch injects a foreign put (auto-accepted into inventory) and a foreign
// buy, waits for the engine to match and PUBLISH the operator match, and returns
// the published match WIRE id and its persisted STORE id.
func (s *wireIDStack) driveToMatch(t *testing.T, seller, buyer identity.Signer, operator identity.Signer) (matchWire, matchStore string) {
	t.Helper()

	putEv := signExchangeEvent(t, seller,
		[]string{exchange.TagPut, "exchange:content-type:code", "exchange:domain:go"}, nil,
		localPutPayload("Go HTTP handler unit test generator", 8000))
	s.conn.inject(putEv)
	waitFor(t, 8*time.Second, "put folds + auto-accepts into inventory", func() bool {
		return len(s.eng.State().Inventory()) == 1
	})

	buyEv := signExchangeEvent(t, buyer,
		[]string{exchange.TagBuy}, nil,
		localBuyPayload("Generate unit tests for a Go HTTP handler", 50000))
	s.conn.inject(buyEv)

	waitFor(t, 8*time.Second, "operator match published OUT to the relay", func() bool {
		return len(s.conn.receivedByKind(nostr.KindMatch)) >= 1
	})
	matchWire = s.conn.receivedByKind(nostr.KindMatch)[0].ID

	recs, _ := s.ls.ReadAll()
	matchRec, ok := firstLocalRecordWithTags(recs, exchange.TagMatch)
	if !ok {
		t.Fatalf("no operator match record persisted in local log")
	}
	matchStore = matchRec.ID

	// The published WIRE id is the Outbox re-signing of the STORE record — the exact
	// divergence GAP 1 reconciles. They MUST differ, and the alias MUST map one to
	// the other (derived by the same signedEventID path the seed/Outbox use).
	if matchWire == matchStore {
		t.Fatalf("published match wire id equals store id — the wire/store divergence premise is invalid")
	}
	derivedWire, derr := signedEventID(matchRec, operator)
	if derr != nil {
		t.Fatalf("signedEventID(match): %v", derr)
	}
	if derivedWire != matchWire {
		t.Fatalf("published match wire id %s != re-derived signed id %s", matchWire, derivedWire)
	}
	return matchWire, matchStore
}

// TestRelayTeamTierWireIDSettle is the core money regression: a wire-id
// buyer-accept moves real scrip end-to-end. Before dontguess-55c a buyer-accept
// e-tagging the match WIRE id produced NO hold (matchToBuyer miss on the wire id);
// this proves the hold, the auto-deliver, and the residual settlement all fire.
func TestRelayTeamTierWireIDSettle(t *testing.T) {
	hushRelayLogs(t)
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

	matchWire, matchStore := st.driveToMatch(t, seller, buyer, operator)

	// --- Buyer-accept e-tagging the match WIRE id → REAL scrip hold ---
	if got := st.scrip.Balance(buyer.PubKeyHex()); got != wireIDBuyerMint {
		t.Fatalf("buyer balance before accept = %d, want minted %d", got, wireIDBuyerMint)
	}
	acceptPayload, _ := json.Marshal(map[string]any{"entry_id": ""})
	acceptEv := signExchangeEvent(t, buyer,
		[]string{exchange.TagSettle, exchange.TagPhasePrefix + exchange.SettlePhaseStrBuyerAccept},
		[]string{matchWire}, acceptPayload)
	st.conn.inject(acceptEv)

	waitFor(t, 8*time.Second, "buyer debited a REAL price+fee hold on the wire-id buyer-accept", func() bool {
		return st.scrip.Balance(buyer.PubKeyHex()) < wireIDBuyerMint
	})
	debited := wireIDBuyerMint - st.scrip.Balance(buyer.PubKeyHex())
	if debited <= 0 {
		t.Fatalf("buyer was not debited (hold amount %d)", debited)
	}

	// --- Operator auto-delivered content, published OUT to the relay ---
	var deliverWire string
	waitFor(t, 8*time.Second, "operator auto-delivered content and published it to the relay", func() bool {
		recs, _ := st.ls.ReadAll()
		dr, ok := firstLocalRecordWithTags(recs, exchange.TagSettle, deliverPhaseTag)
		if !ok {
			return false
		}
		w, derr := signedEventID(dr, operator)
		if derr != nil {
			return false
		}
		// Published to the relay ⇒ the Outbox tick ran past the alias registration.
		for _, ev := range st.conn.receivedByKind(nostr.KindSettle) {
			if ev.ID == w {
				deliverWire = w
				return true
			}
		}
		return false
	})

	// --- Complete e-tagging the deliver WIRE id → seller residual + match settled ---
	sellerBefore := st.scrip.Balance(seller.PubKeyHex())
	completePayload, _ := json.Marshal(map[string]any{"content_hash_verified": true})
	completeEv := signExchangeEvent(t, buyer,
		[]string{exchange.TagSettle, exchange.TagPhasePrefix + exchange.SettlePhaseStrComplete, exchange.TagVerdictPrefix + "accepted"},
		[]string{deliverWire}, completePayload)
	st.conn.inject(completeEv)

	waitFor(t, 8*time.Second, "match settles (durable scrip-settle) on the wire-id complete", func() bool {
		return st.eng.State().IsMatchSettled(matchStore)
	})
	waitFor(t, 8*time.Second, "seller credited the residual", func() bool {
		return st.scrip.Balance(seller.PubKeyHex()) > sellerBefore
	})
	if got := st.scrip.Balance(seller.PubKeyHex()); got <= sellerBefore {
		t.Fatalf("seller residual not credited: before=%d after=%d", sellerBefore, got)
	}

	// Exactly one operator content deliver was emitted for this single sale.
	recs, _ := st.ls.ReadAll()
	if n := countLocalRecordsWithTags(recs, exchange.TagSettle, deliverPhaseTag); n != 1 {
		t.Fatalf("operator emitted %d content delivers, want exactly 1", n)
	}
}

// TestRelayRestartWireIDSettle proves the restart path of GAP 1: a match created
// BEFORE a restart is still settleable by a wire-id buyer-accept AFTER the restart,
// because buildRelayWiring→seedEmittedFromStore rebuilds the wire→store alias into
// the FRESH engine's State from the persisted log. The scrip balances survive on
// the shared LocalScripStore (the restart scope under test is the alias rebuild).
func TestRelayRestartWireIDSettle(t *testing.T) {
	hushRelayLogs(t)
	dir := t.TempDir()
	ls, err := dgstore.Open(dir + "/events.jsonl")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = ls.Close() })

	operator, _ := identity.Generate()
	seller, _ := identity.Generate()
	buyer, _ := identity.Generate()

	// --- Phase 1: produce a persisted, PUBLISHED operator match, then tear down ---
	var matchWire, matchStore string
	func() {
		ctx1, cancel1 := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel1()
		st1 := newWireIDStack(t, ctx1, ls, operator, dir+"/events.jsonl.pubcursor")
		// Phase 1 only produces put→buy→match — no buyer-accept, so no buyer scrip is
		// needed here. The buyer is minted into the phase-2 store below (each phase's
		// LocalScripStore is its own instance; the restart property under test is the
		// wire→store alias rebuild, not scrip-balance persistence).
		matchWire, matchStore = st1.driveToMatch(t, seller, buyer, operator)
		cancel1()
		st1.stop() // wait engine1 + relay1 goroutines exit — a clean "shutdown"
	}()

	// --- Phase 2: a FRESH engine/relay over the SAME log ("restart") ---
	ctx2, cancel2 := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel2()
	st2 := newWireIDStack(t, ctx2, ls, operator, dir+"/events.jsonl.pubcursor")
	t.Cleanup(func() { cancel2(); st2.stop() })
	st2.mintBuyer(t, buyer)

	// The fresh engine must have rebuilt matchToBuyer (from replay) AND wireToStore
	// (from seedEmittedFromStore) — so the wire id resolves to the pre-restart match.
	waitFor(t, 8*time.Second, "restarted engine replayed the pre-restart match", func() bool {
		return st2.eng.State().MatchBuyerKey(matchStore) == buyer.PubKeyHex()
	})
	if m, _, ok := st2.eng.State().ResolveMatchFromAntecedent(matchWire); !ok || m != matchStore {
		t.Fatalf("post-restart: wire id %s did not resolve to store match %s (rebuilt alias missing)", matchWire, matchStore)
	}

	// A wire-id buyer-accept for the pre-restart match still holds → auto-delivers.
	buyerBefore := st2.scrip.Balance(buyer.PubKeyHex())
	acceptPayload, _ := json.Marshal(map[string]any{"entry_id": ""})
	acceptEv := signExchangeEvent(t, buyer,
		[]string{exchange.TagSettle, exchange.TagPhasePrefix + exchange.SettlePhaseStrBuyerAccept},
		[]string{matchWire}, acceptPayload)
	st2.conn.inject(acceptEv)

	waitFor(t, 8*time.Second, "post-restart buyer-accept holds scrip", func() bool {
		return st2.scrip.Balance(buyer.PubKeyHex()) < buyerBefore
	})

	var deliverWire string
	waitFor(t, 8*time.Second, "post-restart auto-deliver published", func() bool {
		recs, _ := st2.ls.ReadAll()
		dr, ok := firstLocalRecordWithTags(recs, exchange.TagSettle, deliverPhaseTag)
		if !ok {
			return false
		}
		w, derr := signedEventID(dr, operator)
		if derr != nil {
			return false
		}
		for _, ev := range st2.conn.receivedByKind(nostr.KindSettle) {
			if ev.ID == w {
				deliverWire = w
				return true
			}
		}
		return false
	})

	sellerBefore := st2.scrip.Balance(seller.PubKeyHex())
	completePayload, _ := json.Marshal(map[string]any{"content_hash_verified": true})
	completeEv := signExchangeEvent(t, buyer,
		[]string{exchange.TagSettle, exchange.TagPhasePrefix + exchange.SettlePhaseStrComplete, exchange.TagVerdictPrefix + "accepted"},
		[]string{deliverWire}, completePayload)
	st2.conn.inject(completeEv)

	waitFor(t, 8*time.Second, "post-restart match settles", func() bool {
		return st2.eng.State().IsMatchSettled(matchStore)
	})
	// IsMatchSettled flips at MarkMatchSettled, which precedes creditResidualToSeller
	// in performScripSettlement — so wait for the credit itself, not just the flag.
	waitFor(t, 8*time.Second, "post-restart seller credited the residual", func() bool {
		return st2.scrip.Balance(seller.PubKeyHex()) > sellerBefore
	})
}

// TestRelayAutoDeliverExactlyOnce proves the auto-deliver fires EXACTLY ONCE per
// match: a second buyer-accept (pre-settlement re-accept) short-circuits at
// restoreExistingHold, and a third (post-settlement) short-circuits at the
// IsMatchSettled guard — neither emits a second content deliver.
func TestRelayAutoDeliverExactlyOnce(t *testing.T) {
	hushRelayLogs(t)
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

	matchWire, matchStore := st.driveToMatch(t, seller, buyer, operator)

	acceptPayload, _ := json.Marshal(map[string]any{"entry_id": ""})
	mkAccept := func() *identity.Event {
		return signExchangeEvent(t, buyer,
			[]string{exchange.TagSettle, exchange.TagPhasePrefix + exchange.SettlePhaseStrBuyerAccept},
			[]string{matchWire}, acceptPayload)
	}

	// First buyer-accept → exactly one auto-deliver.
	st.conn.inject(mkAccept())
	var deliverWire string
	waitFor(t, 8*time.Second, "first buyer-accept auto-delivers exactly one deliver", func() bool {
		recs, _ := st.ls.ReadAll()
		if countLocalRecordsWithTags(recs, exchange.TagSettle, deliverPhaseTag) != 1 {
			return false
		}
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

	// Pre-settlement RE-ACCEPT (new event id, same match). Then complete. Once the
	// match settles we KNOW the re-accept was processed (append order precedes the
	// complete), so it produced no second deliver.
	st.conn.inject(mkAccept())
	completePayload, _ := json.Marshal(map[string]any{"content_hash_verified": true})
	completeEv := signExchangeEvent(t, buyer,
		[]string{exchange.TagSettle, exchange.TagPhasePrefix + exchange.SettlePhaseStrComplete, exchange.TagVerdictPrefix + "accepted"},
		[]string{deliverWire}, completePayload)
	st.conn.inject(completeEv)
	waitFor(t, 8*time.Second, "match settles after the pre-settlement re-accept", func() bool {
		return st.eng.State().IsMatchSettled(matchStore)
	})
	recs, _ := st.ls.ReadAll()
	if n := countLocalRecordsWithTags(recs, exchange.TagSettle, deliverPhaseTag); n != 1 {
		t.Fatalf("after pre-settlement re-accept: %d content delivers, want exactly 1", n)
	}

	// Post-settlement RE-ACCEPT. Force a synchronous dispatch pass so we KNOW it was
	// handled (settled-match guard) before asserting no second deliver.
	postAccept := mkAccept()
	st.conn.inject(postAccept)
	waitFor(t, 8*time.Second, "post-settlement re-accept folds into the log", func() bool {
		recs, _ := st.ls.ReadAll()
		for _, r := range recs {
			if r.ID == postAccept.ID {
				return true
			}
		}
		return false
	})
	// Drain any pending dispatch deterministically (safe alongside the poll loop —
	// cursors are monotonic + dispatch-exactly-once).
	_ = st.eng.PollLocalStoreForTest()
	_ = st.eng.PollLocalStoreForTest()

	recs, _ = st.ls.ReadAll()
	if n := countLocalRecordsWithTags(recs, exchange.TagSettle, deliverPhaseTag); n != 1 {
		t.Fatalf("after post-settlement re-accept: %d content delivers, want exactly 1", n)
	}
}
