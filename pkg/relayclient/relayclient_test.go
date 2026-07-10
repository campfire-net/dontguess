package relayclient

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/campfire-net/dontguess/pkg/identity"
	"github.com/campfire-net/dontguess/pkg/nostr"
	"github.com/campfire-net/dontguess/pkg/proto"
	"github.com/campfire-net/dontguess/pkg/relay"
)

// --- test fixtures -----------------------------------------------------------

// newSigner mints a throwaway secp256k1 identity for a test.
func newSigner(t *testing.T) identity.Signer {
	t.Helper()
	id, _, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	return id
}

// buildSignedPutReject renders a genuinely signed settle(put-reject) event
// referencing putID, exactly the wire shape engine_pricing.go's
// rejectPutLocked emits (mirrors serve_relay_test.go's signExchangeEvent
// pattern: proto.Message -> nostr.ToNostrEvent -> identity.Event ->
// identity.SignEvent). Errors are unexpected for well-formed test fixtures, so
// this panics rather than threading *testing.T through scriptedWSConn (which
// runs from the fake relay's own goroutine, where t.Fatalf is unsafe to call).
func buildSignedPutReject(operator identity.Signer, putID, reason string) *identity.Event {
	payload, err := json.Marshal(map[string]any{
		"phase":    "put-reject",
		"entry_id": putID,
		"reason":   reason,
	})
	if err != nil {
		panic(fmt.Sprintf("marshal put-reject payload: %v", err))
	}
	msg := &proto.Message{
		Sender:      operator.PubKeyHex(),
		Payload:     payload,
		Tags:        []string{"exchange:settle", "exchange:phase:put-reject", "exchange:verdict:rejected"},
		Antecedents: []string{putID},
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

// scriptedWSConn is an in-process fake relay.WSConn (the raw
// WriteMessage/ReadMessage/Close seam relay.Conn drives directly). Unlike
// serve_relay_test.go's fakeRelayConn (which fakes the higher-level
// Send/Recv frameSender/frameReceiver pair the operator's demuxPublisher
// uses), this fakes the RAW websocket so it can also exercise relay.Conn's
// own NIP-42 handshake path (dialAndAuth) — required to prove the H4
// connect-time hang is caught, not just a post-connect Recv stall.
type scriptedWSConn struct {
	mu   sync.Mutex
	recv chan []byte

	closeOnce sync.Once
	closed    chan struct{}

	// behavior knobs, set before use (no concurrent mutation).
	neverRespond bool // accept every write, never push anything to recv (models H4/§7.8 "accept then stall")
	sendReject   bool // on REQ, push a signed put-reject referencing the observed put id
	operator     identity.Signer
	rejectReason string

	putID string // captured from the first EVENT write
}

func newScriptedWSConn() *scriptedWSConn {
	return &scriptedWSConn{recv: make(chan []byte, 16), closed: make(chan struct{})}
}

func (c *scriptedWSConn) WriteMessage(_ int, data []byte) error {
	if c.neverRespond {
		return nil // TCP accept succeeded, write "succeeds", but nothing is ever read back.
	}
	f, err := relay.ParseFrame(data)
	if err != nil {
		return nil
	}
	switch f.Type {
	case relay.LabelEVENT:
		if f.Event == nil {
			return nil
		}
		c.mu.Lock()
		c.putID = f.Event.ID
		c.mu.Unlock()
		ok, _ := relay.EncodeOK(f.Event.ID, true, "")
		select {
		case c.recv <- ok:
		case <-c.closed:
		}
	case relay.LabelREQ:
		if !c.sendReject {
			return nil
		}
		c.mu.Lock()
		putID := c.putID
		c.mu.Unlock()
		rejectEv := buildSignedPutReject(c.operator, putID, c.rejectReason)
		frame, _ := relay.EncodeSubEvent(f.SubID, rejectEv)
		select {
		case c.recv <- frame:
		case <-c.closed:
		}
	}
	return nil
}

func (c *scriptedWSConn) ReadMessage() (int, []byte, error) {
	select {
	case b := <-c.recv:
		return 1, b, nil
	case <-c.closed:
		return 0, nil, fmt.Errorf("scripted ws: closed")
	}
}

func (c *scriptedWSConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}

type fakeDialer struct{ conn relay.WSConn }

func (d fakeDialer) Dial(ctx context.Context, url string) (relay.WSConn, error) {
	return d.conn, nil
}

func testBackoff() relay.Backoff {
	return relay.Backoff{Initial: 5 * time.Millisecond, Max: 20 * time.Millisecond, MaxAttempts: 2}
}

// --- tests --------------------------------------------------------------------

// TestPut_OKIsNotSuccess_RejectSurfaced proves the H2/H3 discipline: a relay
// OK is a transport receipt only. The fake relay ACKs the EVENT (OK, accepted
// = true) and then, on the client's put-reject subscription, delivers a
// genuinely signed settle(put-reject). Put must NOT report success merely
// because OK was accepted=true — PutResult.Success() must be false and the
// reject reason must be surfaced verbatim.
func TestPut_OKIsNotSuccess_RejectSurfaced(t *testing.T) {
	agent := newSigner(t)
	operator := newSigner(t)

	ws := newScriptedWSConn()
	ws.sendReject = true
	ws.operator = operator
	ws.rejectReason = "trust-gate: dropped_unlisted"

	conn := NewConn("ws://fake", agent, WithDialer(fakeDialer{conn: ws}), WithBackoff(testBackoff()))
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	result, err := Put(ctx, conn, agent, PutRequest{
		Description: "a reusable schema checklist",
		Content:     []byte("computed content bytes"),
		TokenCost:   1000,
		ContentType: "exchange:content-type:code",
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if !result.Accepted {
		t.Fatalf("expected transport OK accepted=true, got false")
	}
	if !result.Rejected {
		t.Fatalf("expected Rejected=true (put-reject observed), got false")
	}
	if result.RejectReason != "trust-gate: dropped_unlisted" {
		t.Fatalf("reject reason = %q, want %q", result.RejectReason, "trust-gate: dropped_unlisted")
	}
	if result.Success() {
		t.Fatalf("Success() must be false when a reject was observed, even though the relay OK was accepted — OK != success")
	}
}

// TestPut_AllowlistedNoReject_BoundedSuccess proves the happy path: the relay
// ACKs OK and no reject arrives within the bounded await window. Put must
// return Success()==true, and — because the await window is entirely
// ctx-bounded — must return close to the ctx deadline, not instantly and not
// hung forever.
func TestPut_AllowlistedNoReject_BoundedSuccess(t *testing.T) {
	agent := newSigner(t)
	ws := newScriptedWSConn() // sendReject=false: OK only, no reject ever arrives

	conn := NewConn("ws://fake", agent, WithDialer(fakeDialer{conn: ws}), WithBackoff(testBackoff()))
	defer conn.Close()

	budget := 250 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	start := time.Now()
	result, err := Put(ctx, conn, agent, PutRequest{
		Description: "a reusable CI path filter",
		Content:     []byte("computed content bytes"),
		TokenCost:   1000,
		ContentType: "exchange:content-type:code",
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if !result.Success() {
		t.Fatalf("expected Success()==true (accepted, no reject observed), got Accepted=%v Rejected=%v", result.Accepted, result.Rejected)
	}
	if elapsed < budget/2 {
		t.Fatalf("Put returned in %s, suspiciously fast for a %s bounded reject-wait — did it skip waiting for the window?", elapsed, budget)
	}
	if elapsed > budget+2*time.Second {
		t.Fatalf("Put took %s, way past its %s budget — the bound leaked", elapsed, budget)
	}
}

// TestPut_StalledPostConnectRelay_TimesOutLoud models design §7.8: a relay
// that accepts the socket (dial succeeds, WithoutClientAuth skips the
// handshake) but then sends nothing back at all — never an OK for the
// published EVENT. Put must fail LOUD within the bounded ctx, never hang.
func TestPut_StalledPostConnectRelay_TimesOutLoud(t *testing.T) {
	agent := newSigner(t)
	ws := newScriptedWSConn()
	ws.neverRespond = true

	conn := NewConn("ws://fake", agent, WithDialer(fakeDialer{conn: ws}), WithBackoff(testBackoff()))
	defer conn.Close()

	budget := 150 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	start := time.Now()
	_, err := Put(ctx, conn, agent, PutRequest{
		Description: "stalled relay probe",
		Content:     []byte("x"),
		TokenCost:   1000,
		ContentType: "exchange:content-type:text",
	})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("expected a loud error from a relay that accepts then never responds, got nil (Put hung silently or reported false success)")
	}
	if elapsed > budget+2*time.Second {
		t.Fatalf("Put took %s to fail against a %s budget — the stalled relay was not caught loud inside the timeout", elapsed, budget)
	}
}

// TestPut_StalledHandshakeRelay_TimesOutLoud models H4 directly: with
// --relay-auth opted in, a relay that accepts the socket but never sends the
// NIP-42 ["AUTH", challenge] frame would hang identity.ClientAuthenticate's
// first ReadMessage forever (nip42_handshake.go:29 has no deadline of its
// own). The watchdogDialer must still force this through within the bounded
// ctx.
func TestPut_StalledHandshakeRelay_TimesOutLoud(t *testing.T) {
	agent := newSigner(t)
	ws := newScriptedWSConn()
	ws.neverRespond = true // never sends the AUTH challenge either

	conn := NewConn("ws://fake", agent, WithDialer(fakeDialer{conn: ws}), WithBackoff(testBackoff()), WithRelayAuth(true))
	defer conn.Close()

	budget := 150 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	start := time.Now()
	_, err := Put(ctx, conn, agent, PutRequest{
		Description: "stalled handshake probe",
		Content:     []byte("x"),
		TokenCost:   1000,
		ContentType: "exchange:content-type:text",
	})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("expected a loud error from a relay whose NIP-42 handshake never completes, got nil")
	}
	if elapsed > budget+2*time.Second {
		t.Fatalf("Put took %s to fail against a %s budget — the stalled NIP-42 handshake was not caught loud inside the timeout (H4)", elapsed, budget)
	}
}

// TestPut_DeadRelay_FailsFastNotDefaultBackoff proves the client uses a
// BOUNDED backoff (small MaxAttempts), not relay.DefaultBackoff (retries
// forever): a dead relay (dial always errors) must fail within a small,
// deterministic number of attempts rather than hanging on infinite retries
// even when the caller's ctx has generous headroom.
func TestPut_DeadRelay_FailsFastNotDefaultBackoff(t *testing.T) {
	agent := newSigner(t)
	dialer := errorDialer{}
	conn := NewConn("ws://fake", agent, WithDialer(dialer), WithBackoff(testBackoff()))
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	_, err := Put(ctx, conn, agent, PutRequest{
		Description: "dead relay probe",
		Content:     []byte("x"),
		TokenCost:   1000,
		ContentType: "exchange:content-type:text",
	})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("expected an error against a relay that never dials successfully")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("Put took %s against a dead relay with a 5s ctx budget — expected fast failure via bounded backoff (MaxAttempts), not exhausting the full ctx", elapsed)
	}
}

type errorDialer struct{}

func (errorDialer) Dial(ctx context.Context, url string) (relay.WSConn, error) {
	return nil, fmt.Errorf("dial: connection refused (fake)")
}
