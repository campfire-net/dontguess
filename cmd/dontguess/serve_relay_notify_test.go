package main

// serve_relay_notify_test.go — acceptance coverage for the H1 fix (design §3.8,
// dontguess-97f): an operator match must publish the INSTANT it is folded, not up
// to a full outbox tick later. Before this wiring Outbox.Notify had zero callers,
// so a match sat in events.jsonl until the next periodic tick (5s in production);
// a client would time out on a REAL cache hit.
//
// The fix: EngineOptions.OnLocalAppend fires after every operator append and is
// fanned out (appendNotifier) to each attached leg's Outbox.Notify. This test
// composes the SAME serve-path wiring the other relay tests do (real Engine +
// Intake + Outbox + Sequencer over the fake relay) but runs the Outbox with a
// DELIBERATELY LONG 30s tick. The periodic tick therefore cannot be the publisher
// inside the test window — if the match reaches the wire, the ONLY thing that
// could have driven it is the OnLocalAppend -> Notify path. A nil/unwired hook
// would leave the match unpublished until the 30s tick and the assertion would
// time out. That contrast is what makes this a faithful regression for H1.

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/campfire-net/dontguess/pkg/exchange"
	"github.com/campfire-net/dontguess/pkg/identity"
	"github.com/campfire-net/dontguess/pkg/nostr"
	"github.com/campfire-net/dontguess/pkg/relay"
	dgstore "github.com/campfire-net/dontguess/pkg/store"
)

// longPublishInterval is far larger than any test deadline below, so the Outbox's
// periodic Tick provably cannot be the publisher — only a Notify wakeup can.
const longPublishInterval = 30 * time.Second

func TestRelayNotify_OperatorMatchPublishesOnFoldNotTick(t *testing.T) {
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

	// Build the wiring directly so the test holds the Outbox (to read PublishLag)
	// and can register its Notify into the fan-out BEFORE constructing the engine —
	// mirroring serve.go, where appendNotify is created, passed to the engine as
	// OnLocalAppend, and each leg's Notify registered by attachRelayTransport.
	relayConn := newFakeRelayConn(true /* echo */)
	pub := newDemuxPublisher(relayConn)
	w, watermark, err := buildRelayWiring(ls, operator, operator.PubKeyHex(),
		dir+"/events.jsonl.pubcursor", pub, 0, func(string, error, *nostr.Event) {}, nil)
	if err != nil {
		t.Fatalf("buildRelayWiring: %v", err)
	}

	// The production fan-out: OnLocalAppend -> appendNotifier.fire -> leg's Notify.
	// A counter wraps fire so the test can additionally assert the hook fired on the
	// operator append (not merely that a publish happened by some other route).
	notifier := &appendNotifier{}
	notifier.add(w.outbox.Notify)
	var hookFires int64
	onLocalAppend := func() {
		atomic.AddInt64(&hookFires, 1)
		notifier.fire()
	}

	eng := exchange.NewEngine(exchange.EngineOptions{
		CampfireID:        "local",
		LocalStore:        ls,
		OperatorPublicKey: operator.PubKeyHex(),
		PollInterval:      5 * time.Millisecond,
		Logger:            func(string, ...any) {},
		OnLocalAppend:     onLocalAppend,
	})

	// Initial subscribe (mirrors attachRelayTransport).
	since := watermark/1_000_000_000 - reconnectSlackSeconds
	if since < 0 {
		since = 0
	}
	reqFrame, _ := relay.EncodeReq(relaySubID, relay.Filter{Since: &since})
	if err := relayConn.Send(ctx, reqFrame); err != nil {
		t.Fatalf("initial REQ: %v", err)
	}

	go w.runReader(ctx, relayConn, pub)
	// LONG tick: the periodic Tick fires only at t=0 (nothing to publish yet) and
	// then not again until 30s — well past every deadline below. Any publish inside
	// the window is therefore Notify-driven.
	go w.outbox.Run(ctx, longPublishInterval)

	engDone := startEngineWithAutoAccept(t, ctx, eng)
	t.Cleanup(func() { cancel(); <-engDone })

	// A foreign put arrives, folds, and auto-accepts into inventory.
	putEv := signExchangeEvent(t, seller,
		[]string{exchange.TagPut, "exchange:content-type:code", "exchange:domain:go"}, nil,
		localPutPayload("Go HTTP handler unit test generator", 8000))
	relayConn.inject(putEv)
	waitFor(t, 8*time.Second, "put folds + auto-accepts into inventory", func() bool {
		return len(eng.State().Inventory()) == 1
	})

	// A foreign buy arrives and the engine matches; the operator match record folds
	// into the local log.
	buyEv := signExchangeEvent(t, buyer, []string{exchange.TagBuy}, nil,
		localBuyPayload("Generate unit tests for a Go HTTP handler", 50000))
	relayConn.inject(buyEv)
	waitFor(t, 8*time.Second, "match record folds into the local log", func() bool {
		recs, _ := ls.ReadAll()
		return countTag(recs, exchange.TagMatch) >= 1
	})

	// THE H1 ASSERTION: the operator match reaches the wire WELL WITHIN the 30s
	// tick. With Notify unwired this would not publish until t=30s and this wait
	// would time out. 4s is generous headroom over the sub-ms real path while
	// staying far below the tick — the discriminating window.
	waitFor(t, 4*time.Second, "operator match publishes on fold (Notify-driven, before the 30s tick)", func() bool {
		return len(relayConn.receivedByKind(nostr.KindMatch)) >= 1
	})

	// publish_lag drains to ~0 on the match path: every folded operator record has
	// been published + ACKed (cursor advanced), so nothing sits at RF=1.
	waitFor(t, 4*time.Second, "publish_lag drains to 0 (match published + ACKed, not waiting on the tick)", func() bool {
		return w.outbox.PublishLag() == 0
	})

	// The hook actually fired on the operator append(s) — the put-accept and the
	// match both egress through appendLocalRecord, so at least those two fired.
	if got := atomic.LoadInt64(&hookFires); got < 2 {
		t.Fatalf("OnLocalAppend fired %d times, want >= 2 (put-accept + match are operator appends) — the fold->publish hook was not exercised", got)
	}

	// The published match must be operator-authored (the publish leg re-signed the
	// operator's own record), same invariant the round-trip test pins.
	matches := relayConn.receivedByKind(nostr.KindMatch)
	if matches[0].PubKey != operator.PubKeyHex() {
		t.Fatalf("published match author %s != operator %s", matches[0].PubKey, operator.PubKeyHex())
	}
}
