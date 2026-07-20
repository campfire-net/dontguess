package relayclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/3dl-dev/dontguess/pkg/exchange"
	"github.com/3dl-dev/dontguess/pkg/identity"
	"github.com/3dl-dev/dontguess/pkg/nostr"
	"github.com/3dl-dev/dontguess/pkg/proto"
	"github.com/3dl-dev/dontguess/pkg/relay"
)

// --- buy test fixtures -------------------------------------------------------

// signOperatorEvent builds a genuinely signed operator response event (match /
// buy-miss / assign) of the wire shape the engine emits, by routing a
// proto.Message through the SAME production adapter (nostr.ToNostrEvent) the
// operator uses and signing with the operator key. Mirrors
// relayclient_test.go's buildSignedPutReject. It panics on error (well-formed
// fixtures never fail) because it runs from the fake relay's own goroutine where
// t.Fatalf is unsafe.
func signOperatorEvent(operator identity.Signer, tags []string, antecedents []string, payload map[string]any) *identity.Event {
	pj, err := json.Marshal(payload)
	if err != nil {
		panic(fmt.Sprintf("marshal payload: %v", err))
	}
	msg := &proto.Message{
		Sender:      operator.PubKeyHex(),
		Payload:     pj,
		Tags:        tags,
		Antecedents: antecedents,
		Timestamp:   time.Now().UnixNano(),
	}
	nev, err := nostr.ToNostrEvent(msg)
	if err != nil {
		panic(fmt.Sprintf("ToNostrEvent: %v", err))
	}
	ev := &identity.Event{
		PubKey:    nev.PubKey,
		CreatedAt: nev.CreatedAt,
		Kind:      nev.Kind,
		Tags:      nev.Tags,
		Content:   nev.Content,
	}
	if err := identity.SignEvent(operator, ev); err != nil {
		panic(fmt.Sprintf("SignEvent: %v", err))
	}
	return ev
}

func signedMatch(operator identity.Signer, buyID string) *identity.Event {
	return signOperatorEvent(operator, []string{exchange.TagMatch}, []string{buyID}, map[string]any{
		"results": []map[string]any{{
			"entry_id":          "entry-1",
			"put_msg_id":        "put-1",
			"seller_key":        "seller-abc",
			"description":       "a reusable flock contention test pattern for Go",
			"content_type":      "code",
			"price":             int64(900),
			"seller_reputation": 72,
		}},
		"guide": "ranked by correctness gate then efficiency",
	})
}

func signedBuyMiss(operator identity.Signer, buyID string) *identity.Event {
	return signOperatorEvent(operator, []string{exchange.TagBuyMiss, exchange.TagMatch}, []string{buyID}, map[string]any{
		"task_hash":          "abc123",
		"offered_price_rate": 70,
		"guide":              "No cached inference matched your task. A standing offer has been created.",
	})
}

func signedAssign(operator identity.Signer, buyID string) *identity.Event {
	return signOperatorEvent(operator, []string{exchange.TagAssign}, []string{buyID}, map[string]any{
		"assign_type": "brokered-match",
	})
}

// readItem is one scripted ReadMessage result: either data or a transport error
// (modeling a mid-await conn drop).
type readItem struct {
	data []byte
	err  error
}

// buyFakeConn is an in-process fake relay.WSConn tailored to the buy await. It
// captures the subscription id and buy id from the client's writes and scripts
// operator responses. Unlike relayclient_test.go's scriptedWSConn (put-focused),
// it can build a response LAZILY from the captured/observed buy id — required
// because the buy id is the client's own deterministic signed-event id, unknown
// to the test until the client publishes it.
//
// STRFRY LIVE-DELIVERY MODEL (H1 teeth): the fake reproduces the one relay
// semantic the subscribe-first ordering exists to exploit — a real-time (stream)
// delivery reaches ONLY a subscription that was LIVE when the event was
// published. The fake freezes that eligibility at buy-publish time
// (liveAtPublish = "was a REQ seen strictly before the buy EVENT"); the operator
// is modelled as folding+publishing the match at that same instant (worst-case
// H1: an instant Notify-driven response). So a publish-BEFORE-subscribe (or a
// no-subscribe) regression MISSES the real-time match — the buy then times out
// AMBIGUOUS — while a re-REQ carrying a `since` covering the buy REPLAYS the
// stored match exactly as strfry would. Without this gate the buy test would
// pass identically whether the client subscribed first or not (false H1
// assurance); TestBuyFake_LiveDeliveryGatesOnSubscribeFirst locks the gate.
type buyFakeConn struct {
	mu    sync.Mutex
	items chan readItem

	closeOnce sync.Once
	closed    chan struct{}

	operator identity.Signer

	// behavior knobs (set before use).
	okOnBuy          bool                               // ACK the buy EVENT with OK accepted=true
	buildResp        func(buyID string) *identity.Event // the operator response to deliver, built from the buy id
	respondAfter     time.Duration                      // if buildResp set: deliver it this long after the buy OK (0 = immediately)
	respondOnReq     bool                               // deliver buildResp when a REQ arrives (buy id read from the #e filter) — models a reconnect replaying stored events on a fresh socket that never saw the buy
	replayOnSinceREQ bool                               // on THIS conn (that saw the buy), replay buildResp when a REQ carries a `since` covering the buy — models strfry replaying a stored match to a late/re-subscriber
	dropAfterOK      bool                               // after the buy OK, inject a transport drop instead of a response
	failEventWrite   bool                               // fail the publish EVENT write (models a mid-flow send failure that forces Conn.Send to drop+reconnect); the subscribe REQ still succeeds

	subID string
	buyID string
	// sawReq/liveAtPublish/buyCreatedAt implement the live-delivery gate above.
	sawReq        bool  // a REQ has arrived on this conn
	liveAtPublish bool  // frozen at buy-publish: was a REQ live when the buy was published (subscribe-first)?
	buyCreatedAt  int64 // the published buy's created_at (strfry replay floor)
}

func newBuyFakeConn() *buyFakeConn {
	return &buyFakeConn{items: make(chan readItem, 32), closed: make(chan struct{})}
}

func (c *buyFakeConn) push(it readItem) {
	select {
	case c.items <- it:
	case <-c.closed:
	}
}

func (c *buyFakeConn) WriteMessage(_ int, data []byte) error {
	f, err := relay.ParseFrame(data)
	if err != nil {
		return nil
	}
	switch f.Type {
	case relay.LabelREQ:
		c.mu.Lock()
		c.subID = f.SubID
		c.sawReq = true
		buySeen := c.buyID != ""
		buyID := c.buyID
		c.mu.Unlock()

		var filterBuyID string
		var sinceSet bool
		if len(f.Filters) > 0 {
			if e := f.Filters[0].Tags["e"]; len(e) > 0 {
				filterBuyID = e[0]
			}
			sinceSet = f.Filters[0].Since != nil
		}

		// respondOnReq: a fresh relay socket (post-reconnect) that never saw the
		// buy replays the stored operator response on ANY matching REQ, reading
		// the buy id from the #e filter. Models the H5 recovery conn.
		if c.respondOnReq && c.buildResp != nil && filterBuyID != "" {
			frame, _ := relay.EncodeSubEvent(f.SubID, c.buildResp(filterBuyID))
			c.push(readItem{data: frame})
		}

		// replayOnSinceREQ: on the conn that DID see the buy, strfry replays the
		// stored match to a late/re-subscriber only when its REQ carries a `since`
		// covering the buy (a bare late subscribe gets nothing — that is the H1
		// loss window). Requires the buy to have already been published here.
		if c.replayOnSinceREQ && c.buildResp != nil && buySeen && sinceSet {
			frame, _ := relay.EncodeSubEvent(f.SubID, c.buildResp(buyID))
			c.push(readItem{data: frame})
		}
	case relay.LabelEVENT:
		if f.Event == nil {
			return nil
		}
		c.mu.Lock()
		fail := c.failEventWrite
		c.mu.Unlock()
		if fail {
			// Model a publish write that fails on this socket: Conn.Send will drop
			// this conn and reconnect to the next dialed one, replaying ONLY the
			// EVENT (never the earlier subscribe REQ) — the dontguess-989 orphan.
			return fmt.Errorf("fake relay: forced publish EVENT write failure")
		}
		c.mu.Lock()
		c.buyID = f.Event.ID
		c.buyCreatedAt = f.Event.CreatedAt
		// Freeze real-time delivery eligibility at buy-publish time: strfry's live
		// stream reaches only subscriptions already live here. The operator is
		// modelled as publishing the match at this same instant (worst-case H1),
		// so a sub that was not yet live MISSES the real-time match.
		c.liveAtPublish = c.sawReq
		c.mu.Unlock()
		if c.okOnBuy {
			ok, _ := relay.EncodeOK(f.Event.ID, true, "")
			c.push(readItem{data: ok})
		}
		go c.afterBuy()
	}
	return nil
}

// afterBuy runs the scripted follow-up to a buy publish. The buy OK (if any) was
// already enqueued synchronously in WriteMessage before this goroutine starts,
// so the FIFO channel guarantees the client reads the OK before any drop/response
// this pushes — deterministic ordering without sleeps.
func (c *buyFakeConn) afterBuy() {
	if c.dropAfterOK {
		c.push(readItem{err: fmt.Errorf("fake relay: connection dropped mid-await")})
		return
	}
	if c.buildResp == nil {
		return
	}
	if c.respondOnReq {
		return // delivery happens on the (re)subscribe REQ instead
	}
	if c.respondAfter > 0 {
		select {
		case <-time.After(c.respondAfter):
		case <-c.closed:
			return
		}
	}
	c.mu.Lock()
	subID, buyID := c.subID, c.buyID
	live := c.liveAtPublish
	c.mu.Unlock()
	if !live {
		// Subscribe-first was violated (the sub was not live when the buy — and
		// thus the match — was published): strfry's real-time stream never
		// carries the match to this sub. The client will time out AMBIGUOUS
		// unless it re-subscribes with a `since`. This is exactly the H1
		// regression subscribe-before-publish exists to prevent, so the fake
		// withholds delivery and the buy test bites.
		return
	}
	frame, _ := relay.EncodeSubEvent(subID, c.buildResp(buyID))
	c.push(readItem{data: frame})
}

func (c *buyFakeConn) ReadMessage() (int, []byte, error) {
	select {
	case it := <-c.items:
		if it.err != nil {
			return 0, nil, it.err
		}
		return 1, it.data, nil
	case <-c.closed:
		return 0, nil, fmt.Errorf("fake relay: closed")
	}
}

func (c *buyFakeConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}

// seqDialer hands out a fixed sequence of conns, one per Dial — modeling a
// reconnect where the client re-dials the (same) relay and lands on a fresh
// socket that must be re-subscribed.
type seqDialer struct {
	mu    sync.Mutex
	conns []relay.WSConn
	i     int
}

func (d *seqDialer) Dial(_ context.Context, _ string) (relay.WSConn, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.i >= len(d.conns) {
		return nil, fmt.Errorf("seqDialer: no more scripted conns (Dial #%d)", d.i+1)
	}
	c := d.conns[d.i]
	d.i++
	return c, nil
}

// --- tests -------------------------------------------------------------------

// TestBuy_Hit_MatchOneTickLate proves the subscribe-first ordering closes H1:
// the match EVENT is published one tick AFTER the buy OK, yet the client — having
// subscribed BEFORE publishing — receives it well within the timeout and surfaces
// the parsed match (entry id, price, seller) as the ed2-C seam.
//
// This has TEETH because buyFakeConn models strfry live-delivery: it delivers the
// real-time match ONLY if a REQ was live when the buy was published (see the fake's
// liveAtPublish gate). A publish-before-subscribe regression in Buy() therefore
// yields NO match and this test fails with an AMBIGUOUS timeout — verified by the
// ground-truth mutation check in the commit that added the gate (reorder Buy() to
// publish-then-subscribe → this test fails; revert → it passes). The gate itself is
// locked by TestBuyFake_LiveDeliveryGatesOnSubscribeFirst.
func TestBuy_Hit_MatchOneTickLate(t *testing.T) {
	agent := newSigner(t)
	operator := newSigner(t)

	ws := newBuyFakeConn()
	ws.okOnBuy = true
	ws.buildResp = func(buyID string) *identity.Event { return signedMatch(operator, buyID) }
	ws.respondAfter = 40 * time.Millisecond // "one tick late"

	conn := NewConn("ws://fake", agent, WithDialer(fakeDialer{conn: ws}), WithBackoff(testBackoff()))
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	res, err := Buy(ctx, conn, agent, BuyRequest{Task: "flock contention test pattern for Go", Budget: 1000})
	if err != nil {
		t.Fatalf("Buy: %v", err)
	}
	if res.Outcome != BuyOutcomeMatch {
		t.Fatalf("outcome = %v, want match", res.Outcome)
	}
	m := res.Match()
	if m == nil {
		t.Fatalf("expected a surfaced match entry, got none")
	}
	if m.EntryID != "entry-1" || m.Price != 900 || m.SellerKey != "seller-abc" {
		t.Fatalf("surfaced match = %+v, want entry-1/900/seller-abc", m)
	}
	if res.MatchMsgID == "" {
		t.Fatalf("MatchMsgID (the ed2-C settle seam) must be set on a hit")
	}
}

// drainForMatch consumes the fake's outbound frames for up to d, reporting
// whether the buy OK receipt and/or a match EVENT e-tagging buyID were delivered.
// It reads c.items directly (in-package) — a single synchronous consumer with no
// background goroutine, so successive calls never race for the same queue.
func drainForMatch(t *testing.T, ws *buyFakeConn, buyID string, d time.Duration) (sawMatch, sawOK bool) {
	t.Helper()
	deadline := time.After(d)
	for {
		select {
		case <-deadline:
			return
		case <-ws.closed:
			return
		case it := <-ws.items:
			if it.err != nil {
				continue
			}
			f, err := relay.ParseFrame(it.data)
			if err != nil {
				continue
			}
			switch f.Type {
			case relay.LabelOK:
				if f.EventID == buyID {
					sawOK = true
				}
			case relay.LabelEVENT:
				if f.Event != nil && f.Event.Kind == nostr.KindMatch && eventHasETag(f.Event, buyID) {
					sawMatch = true
				}
			}
		}
	}
}

// TestBuyFake_LiveDeliveryGatesOnSubscribeFirst locks the H1 teeth of the buy
// fixture itself, independent of Buy()'s own (correct) ordering. It drives
// buyFakeConn DIRECTLY in the REGRESSION order — publish the buy EVENT, THEN a
// bare late subscribe (no `since`) — and asserts the real-time match is WITHHELD
// (only the buy OK arrives). It then re-subscribes with a `since` covering the
// buy and asserts the stored match IS replayed, exactly as strfry would. If a
// future edit ever defangs the fake (delivering regardless of subscribe order),
// this test fails — which is the guarantee that TestBuy_Hit_MatchOneTickLate is
// really exercising subscribe-first and not passing vacuously.
func TestBuyFake_LiveDeliveryGatesOnSubscribeFirst(t *testing.T) {
	agent := newSigner(t)
	operator := newSigner(t)

	ws := newBuyFakeConn()
	ws.okOnBuy = true
	ws.replayOnSinceREQ = true
	ws.buildResp = func(buyID string) *identity.Event { return signedMatch(operator, buyID) }
	defer ws.Close()

	// A real signed buy EVENT (deterministic id), published directly at the fake.
	buyMsg, err := buildBuyMessage(agent, BuyRequest{Task: "direct-drive H1 probe", Budget: 1000})
	if err != nil {
		t.Fatalf("buildBuyMessage: %v", err)
	}
	buyEv, err := signAsIdentityEvent(agent, buyMsg)
	if err != nil {
		t.Fatalf("sign buy: %v", err)
	}
	evFrame, err := relay.EncodeEvent(buyEv)
	if err != nil {
		t.Fatalf("encode buy EVENT: %v", err)
	}

	// REGRESSION ORDER: publish the buy BEFORE any subscription is live.
	if err := ws.WriteMessage(1, evFrame); err != nil {
		t.Fatalf("publish buy: %v", err)
	}
	// A bare late subscribe (no `since`) — strfry's live stream already passed
	// the match by, and a since-less REQ does not replay it.
	bareReq, err := relay.EncodeReq("dg-buy-late", relay.Filter{
		Kinds: []int{nostr.KindMatch},
		Tags:  map[string][]string{"e": {buyEv.ID}},
	})
	if err != nil {
		t.Fatalf("encode bare REQ: %v", err)
	}
	if err := ws.WriteMessage(1, bareReq); err != nil {
		t.Fatalf("late subscribe: %v", err)
	}

	sawMatch, sawOK := drainForMatch(t, ws, buyEv.ID, 250*time.Millisecond)
	if !sawOK {
		t.Fatalf("expected the buy OK receipt from the fake")
	}
	if sawMatch {
		t.Fatalf("publish-before-subscribe delivered a real-time match — the fake does NOT model strfry live delivery, so TestBuy_Hit_MatchOneTickLate has no H1 teeth")
	}

	// Now re-subscribe WITH a `since` covering the buy: strfry replays the stored
	// match. This is the recovery path the client uses after a conn drop (H5).
	since := buyEv.CreatedAt - resubscribeSlackSeconds
	sinceReq, err := relay.EncodeReq("dg-buy-resub", relay.Filter{
		Kinds: []int{nostr.KindMatch},
		Tags:  map[string][]string{"e": {buyEv.ID}},
		Since: &since,
	})
	if err != nil {
		t.Fatalf("encode since REQ: %v", err)
	}
	if err := ws.WriteMessage(1, sinceReq); err != nil {
		t.Fatalf("re-subscribe with since: %v", err)
	}
	if sawMatch, _ = drainForMatch(t, ws, buyEv.ID, 500*time.Millisecond); !sawMatch {
		t.Fatalf("a re-REQ with a `since` covering the buy must replay the stored match (as strfry would)")
	}
}

// TestBuy_ConnDropMidAwait_ReSubscribeRecoversMatch proves H5: the connection
// drops AFTER the buy OK but BEFORE the match; the client must re-issue its REQ
// on the fresh socket (the relay never replays a REQ) and recover the match the
// relay stored. conn1 drops after OK; conn2 replays the match when the
// re-subscribe REQ arrives.
func TestBuy_ConnDropMidAwait_ReSubscribeRecoversMatch(t *testing.T) {
	agent := newSigner(t)
	operator := newSigner(t)

	conn1 := newBuyFakeConn()
	conn1.okOnBuy = true
	conn1.dropAfterOK = true

	conn2 := newBuyFakeConn()
	conn2.respondOnReq = true
	conn2.buildResp = func(buyID string) *identity.Event { return signedMatch(operator, buyID) }

	dialer := &seqDialer{conns: []relay.WSConn{conn1, conn2}}
	conn := NewConn("ws://fake", agent, WithDialer(dialer), WithBackoff(testBackoff()))
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	res, err := Buy(ctx, conn, agent, BuyRequest{Task: "cf-protocol README CF_NO_PINS", Budget: 1000})
	if err != nil {
		t.Fatalf("Buy: %v", err)
	}
	if res.Outcome != BuyOutcomeMatch {
		t.Fatalf("outcome = %v, want match (recovered after re-subscribe)", res.Outcome)
	}
	if res.Match() == nil || res.Match().EntryID != "entry-1" {
		t.Fatalf("expected recovered match entry-1, got %+v", res.Match())
	}
	dialer.mu.Lock()
	dials := dialer.i
	dialer.mu.Unlock()
	if dials < 2 {
		t.Fatalf("expected a reconnect (>=2 dials), got %d — the re-subscribe path was not exercised", dials)
	}
}

// TestBuy_PublishReconnectBeforeFirstRecv_ReSubscribeRecoversMatch proves the
// dontguess-989 regression. The buy ordering is subscribe(Send#1) -> publish
// (Send#2) -> Recv. If the publish write FAILS on the subscribed socket, Conn.Send
// drops it and reconnects to a fresh socket, replaying ONLY the buy EVENT — the
// subscription REQ from Send#1 is lost. The first Recv then reads that fresh,
// UNSUBSCRIBED socket: because no Recv had yet claimed a generation (readerGen == 0),
// the send-triggered-reconnect guard (conn.go generation guard) did NOT fire, so the
// reader blocked on a REQ-less socket until the ctx timeout -> AMBIGUOUS, even though
// the operator's match was on the relay. The fix pins the reader generation at
// subscribe time so the guard fires on the FIRST Recv; the client then re-subscribes
// on the fresh socket and recovers the match strfry stored.
//
// TEETH: without the fix conn2 withholds real-time delivery (its subscription was
// not live when the buy was published there — liveAtPublish=false) and the client
// never re-subscribes, so the buy times out AMBIGUOUS and this test fails.
func TestBuy_PublishReconnectBeforeFirstRecv_ReSubscribeRecoversMatch(t *testing.T) {
	agent := newSigner(t)
	operator := newSigner(t)

	// conn1 accepts the subscribe REQ but FAILS the publish EVENT write, forcing
	// Conn.Send to drop conn1 and reconnect to conn2 BEFORE the first Recv.
	conn1 := newBuyFakeConn()
	conn1.failEventWrite = true

	// conn2 is the reconnect target: Conn.Send retries the EVENT here (so conn2
	// "saw the buy"), and conn2 replays the stored match when the client
	// re-subscribes with a `since` covering the buy — exactly strfry's replay to a
	// re-subscriber. It never delivers real-time (its REQ arrives after the EVENT),
	// so ONLY a correct re-subscribe recovers the match.
	conn2 := newBuyFakeConn()
	conn2.okOnBuy = true
	conn2.replayOnSinceREQ = true
	conn2.buildResp = func(buyID string) *identity.Event { return signedMatch(operator, buyID) }

	dialer := &seqDialer{conns: []relay.WSConn{conn1, conn2}}
	conn := NewConn("ws://fake", agent, WithDialer(dialer), WithBackoff(testBackoff()))
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	res, err := Buy(ctx, conn, agent, BuyRequest{Task: "khatru go-nostr azcosmos live-test gotchas", Budget: 1000})
	if err != nil {
		t.Fatalf("Buy: %v", err)
	}
	if res.Outcome != BuyOutcomeMatch {
		t.Fatalf("outcome = %v, want match (recovered after publish-triggered reconnect + re-subscribe)", res.Outcome)
	}
	if res.Match() == nil || res.Match().EntryID != "entry-1" {
		t.Fatalf("expected recovered match entry-1, got %+v", res.Match())
	}
	dialer.mu.Lock()
	dials := dialer.i
	dialer.mu.Unlock()
	if dials < 2 {
		t.Fatalf("expected a publish-triggered reconnect (>=2 dials), got %d", dials)
	}
}

// TestBuy_Miss_PrintsDemandGuide proves failure-matrix (b): a kind-3403 WITH the
// exchange:buy-miss tag is a genuine miss, surfaced as the demand-signal guide,
// LOUD and not an error.
func TestBuy_Miss_PrintsDemandGuide(t *testing.T) {
	agent := newSigner(t)
	operator := newSigner(t)

	ws := newBuyFakeConn()
	ws.okOnBuy = true
	ws.buildResp = func(buyID string) *identity.Event { return signedBuyMiss(operator, buyID) }

	conn := NewConn("ws://fake", agent, WithDialer(fakeDialer{conn: ws}), WithBackoff(testBackoff()))
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	res, err := Buy(ctx, conn, agent, BuyRequest{Task: "something nobody has computed", Budget: 1000})
	if err != nil {
		t.Fatalf("Buy: %v", err)
	}
	if res.Outcome != BuyOutcomeMiss {
		t.Fatalf("outcome = %v, want buy-miss", res.Outcome)
	}
	if res.OfferedPriceRate != 70 {
		t.Fatalf("offered price rate = %d, want 70", res.OfferedPriceRate)
	}
	var buf bytes.Buffer
	WriteOutcome(&buf, res)
	out := buf.String()
	if !strings.Contains(out, "BUY-MISS") || !strings.Contains(strings.ToLower(out), "demand signal") {
		t.Fatalf("miss output missing the demand-signal guide:\n%s", out)
	}
	if !strings.Contains(out, "dontguess put") {
		t.Fatalf("miss output must tell the buyer to `dontguess put`:\n%s", out)
	}
}

// TestBuy_Timeout_PrintsAmbiguousEnumeratedCauses proves §5.4: a buy that gets an
// OK but no discriminating response times out as AMBIGUOUS, enumerating the
// actionable causes and NEVER claiming "no cache exists".
func TestBuy_Timeout_PrintsAmbiguousEnumeratedCauses(t *testing.T) {
	agent := newSigner(t)

	ws := newBuyFakeConn()
	ws.okOnBuy = true // OK only — the operator never answers.

	conn := NewConn("ws://fake", agent, WithDialer(fakeDialer{conn: ws}), WithBackoff(testBackoff()))
	defer conn.Close()

	budget := 200 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	start := time.Now()
	res, err := Buy(ctx, conn, agent, BuyRequest{Task: "ambiguous probe", Budget: 1000})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Buy (ambiguous is a terminal outcome, not an error): %v", err)
	}
	if res.Outcome != BuyOutcomeAmbiguous {
		t.Fatalf("outcome = %v, want ambiguous", res.Outcome)
	}
	if len(res.AmbiguousCauses) < 3 {
		t.Fatalf("expected >=3 enumerated ambiguous causes, got %d", len(res.AmbiguousCauses))
	}
	if elapsed > budget+2*time.Second {
		t.Fatalf("Buy took %s against a %s budget — the await bound leaked", elapsed, budget)
	}
	var buf bytes.Buffer
	WriteOutcome(&buf, res)
	out := buf.String()
	if !strings.Contains(out, "AMBIGUOUS") {
		t.Fatalf("ambiguous output missing the AMBIGUOUS header:\n%s", out)
	}
	if !strings.Contains(out, "does NOT mean no cache exists") {
		t.Fatalf("ambiguous output must not claim no cache exists:\n%s", out)
	}
	// The three wire-invisible/actionable causes must all be surfaced.
	for _, want := range []string{"under-funded", "mint", "allowlist"} {
		if !strings.Contains(out, want) {
			t.Fatalf("ambiguous causes missing %q:\n%s", want, out)
		}
	}
}

// TestBuy_DeadRelay_FailsLoudInsideTimeout proves a dead/unreachable relay (dial
// always errors) exits LOUD via the bounded backoff, well inside the ctx budget —
// never a silent ambiguous, never a hang.
func TestBuy_DeadRelay_FailsLoudInsideTimeout(t *testing.T) {
	agent := newSigner(t)

	conn := NewConn("ws://fake", agent, WithDialer(errorDialer{}), WithBackoff(testBackoff()))
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	res, err := Buy(ctx, conn, agent, BuyRequest{Task: "dead relay probe", Budget: 1000})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("expected a loud transport error against a dead relay, got outcome=%v", res.Outcome)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("Buy took %s against a dead relay with a 5s ctx — expected fast bounded-backoff failure", elapsed)
	}
}

// TestBuy_LeakedAssign_SurfacedLoud proves failure-matrix (c): an assign(3405)
// e-tagging the buy (BrokeredMatchMode leaked, out of scope) is discriminated and
// surfaced LOUD rather than silently timing out.
func TestBuy_LeakedAssign_SurfacedLoud(t *testing.T) {
	agent := newSigner(t)
	operator := newSigner(t)

	ws := newBuyFakeConn()
	ws.okOnBuy = true
	ws.buildResp = func(buyID string) *identity.Event { return signedAssign(operator, buyID) }

	conn := NewConn("ws://fake", agent, WithDialer(fakeDialer{conn: ws}), WithBackoff(testBackoff()))
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	res, err := Buy(ctx, conn, agent, BuyRequest{Task: "brokered probe", Budget: 1000})
	if err != nil {
		t.Fatalf("Buy: %v", err)
	}
	if res.Outcome != BuyOutcomeBrokered {
		t.Fatalf("outcome = %v, want brokered-assign-leaked", res.Outcome)
	}
	var buf bytes.Buffer
	WriteOutcome(&buf, res)
	if !strings.Contains(buf.String(), "brokered") {
		t.Fatalf("brokered output missing loud surface:\n%s", buf.String())
	}
}

// TestBuy_ForgedMatch_NotTrusted proves the security floor: a match event NOT
// signed by a valid key (or tampered) is a loud-skip, never surfaced as a hit —
// otherwise a hostile relay could spoof a match. Here a well-formed match is
// tampered post-sign (content changed), so VerifyEvent fails; the client must
// keep waiting and time out AMBIGUOUS rather than report a match.
func TestBuy_ForgedMatch_NotTrusted(t *testing.T) {
	agent := newSigner(t)
	operator := newSigner(t)

	ws := newBuyFakeConn()
	ws.okOnBuy = true
	ws.buildResp = func(buyID string) *identity.Event {
		ev := signedMatch(operator, buyID)
		ev.Content = ev.Content + " tampered" // breaks the id/signature binding
		return ev
	}

	conn := NewConn("ws://fake", agent, WithDialer(fakeDialer{conn: ws}), WithBackoff(testBackoff()))
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	res, err := Buy(ctx, conn, agent, BuyRequest{Task: "forgery probe", Budget: 1000})
	if err != nil {
		t.Fatalf("Buy: %v", err)
	}
	if res.Outcome != BuyOutcomeAmbiguous {
		t.Fatalf("outcome = %v, want ambiguous — a forged match must NOT be trusted as a hit", res.Outcome)
	}
}
