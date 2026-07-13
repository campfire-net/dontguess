package relay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/campfire-net/dontguess/pkg/identity"
	"github.com/gorilla/websocket"
)

// --- Integration: real websocket, in-process NIP-42 relay -------------------

// forcedDropRelay stands up a real websocket relay that runs the NIP-42 relay
// handshake, then behaves differently per connection:
//   - connection 1: sends a NOTICE("first") and FORCE-CLOSES (simulating a drop);
//   - connection 2+: sends a NOTICE("second") and holds the connection open
//     until the test tears down (via done).
//
// This is the in-process fake relay the item permits: it exercises dial + real
// NIP-42 handshake over a genuine *websocket.Conn + forced-disconnect recovery,
// end to end, with no external relay.
func forcedDropRelay(t *testing.T, al *identity.Allowlist) (url string, done chan struct{}, connections *int32) {
	t.Helper()
	var count int32
	done = make(chan struct{})
	connections = &count
	upgrader := websocket.Upgrader{}

	var urlHolder string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if _, err := identity.RelayAuthenticate(conn, urlHolder, al); err != nil {
			return
		}
		n := atomic.AddInt32(&count, 1)
		if n == 1 {
			notice, _ := EncodeNotice("first")
			_ = conn.WriteMessage(websocket.TextMessage, notice)
			// Give the client a beat to read the NOTICE, then force the drop by
			// returning (defer closes the underlying TCP connection).
			time.Sleep(30 * time.Millisecond)
			return
		}
		notice, _ := EncodeNotice("second")
		_ = conn.WriteMessage(websocket.TextMessage, notice)
		<-done // hold the reconnected session open until the test finishes
	}))
	t.Cleanup(srv.Close)

	urlHolder = "ws" + strings.TrimPrefix(srv.URL, "http")
	return urlHolder, done, connections
}

// TestConnAuthenticatesAndSurvivesForcedDisconnect is the required integration
// test: the Conn completes the NIP-42 handshake, reads a frame, survives a
// relay-forced disconnect (surfaced loudly as ErrConnDropped), reconnects with
// backoff, re-authenticates, and reads a frame on the fresh session.
func TestConnAuthenticatesAndSurvivesForcedDisconnect(t *testing.T) {
	member, err := identity.Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	al, err := identity.NewAllowlist(member.Npub())
	if err != nil {
		t.Fatalf("NewAllowlist: %v", err)
	}
	url, done, connections := forcedDropRelay(t, al)
	defer close(done)

	c := New(url, member, WithBackoff(Backoff{Initial: 5 * time.Millisecond, Max: 50 * time.Millisecond, MaxAttempts: 20}))
	t.Cleanup(func() { _ = c.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// First Recv connects, authenticates, and reads the relay's NOTICE("first").
	raw, err := c.Recv(ctx)
	if err != nil {
		t.Fatalf("first Recv (connect+auth+read): %v", err)
	}
	if f, perr := ParseFrame(raw); perr != nil || f.Message != "first" {
		t.Fatalf("first frame: parsed=%v err=%v, want NOTICE 'first'", f, perr)
	}

	// The relay force-closed connection 1; the next Recv must surface the drop
	// loudly as ErrConnDropped (never a silent nil).
	if _, derr := c.Recv(ctx); !errors.Is(derr, ErrConnDropped) {
		t.Fatalf("expected ErrConnDropped after forced disconnect, got %v", derr)
	}

	// The next Recv transparently reconnects (backoff), re-authenticates, and
	// reads the fresh session's NOTICE("second").
	raw, err = c.Recv(ctx)
	if err != nil {
		t.Fatalf("Recv after reconnect: %v", err)
	}
	if f, perr := ParseFrame(raw); perr != nil || f.Message != "second" {
		t.Fatalf("post-reconnect frame: parsed=%v err=%v, want NOTICE 'second'", f, perr)
	}

	if got := atomic.LoadInt32(connections); got < 2 {
		t.Fatalf("relay saw %d connections, want ≥2 (dial happened twice)", got)
	}
	m := c.Metrics()
	if m.Connects < 2 {
		t.Fatalf("metrics.Connects=%d, want ≥2 (authenticated twice)", m.Connects)
	}
	if m.Disconnects < 1 {
		t.Fatalf("metrics.Disconnects=%d, want ≥1 (forced drop counted)", m.Disconnects)
	}
}

// TestConnRejectedIdentityFailsLoudly proves a non-allowlisted identity cannot
// establish a session: the handshake is rejected and Connect returns an error
// (the relay reject is not swallowed).
func TestConnRejectedIdentityFailsLoudly(t *testing.T) {
	member, _ := identity.Generate()
	stranger, _ := identity.Generate()
	al, err := identity.NewAllowlist(member.Npub()) // stranger excluded
	if err != nil {
		t.Fatalf("NewAllowlist: %v", err)
	}
	url, done, _ := forcedDropRelay(t, al)
	defer close(done)

	c := New(url, stranger, WithBackoff(Backoff{Initial: time.Millisecond, Max: 2 * time.Millisecond, MaxAttempts: 3}))
	t.Cleanup(func() { _ = c.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err == nil {
		t.Fatal("stranger not on allowlist established a session; handshake reject was swallowed")
	}
}

// --- Unit: reconnect/backoff via an injected fake dialer --------------------

// scriptConn is an in-memory WSConn that scripts the relay side of a NIP-42
// handshake so ClientAuthenticate succeeds without a network: ReadMessage
// returns the queued frames in order (an AUTH challenge, then an OK-accepted),
// and WriteMessage records the client's auth response.
type scriptConn struct {
	mu     sync.Mutex
	reads  [][]byte
	idx    int
	writes [][]byte
	closed bool
}

func newScriptConn(t *testing.T) *scriptConn {
	t.Helper()
	chal, err := identity.EncodeAuthChallenge("challenge-nonce")
	if err != nil {
		t.Fatalf("EncodeAuthChallenge: %v", err)
	}
	ok, err := identity.EncodeOK("any-event-id", true, "")
	if err != nil {
		t.Fatalf("EncodeOK: %v", err)
	}
	return &scriptConn{reads: [][]byte{chal, ok}}
}

func (s *scriptConn) ReadMessage() (int, []byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, nil, errors.New("scriptConn: closed")
	}
	if s.idx >= len(s.reads) {
		return 0, nil, io.EOF
	}
	b := s.reads[s.idx]
	s.idx++
	return websocket.TextMessage, b, nil
}

func (s *scriptConn) WriteMessage(_ int, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("scriptConn: write on closed conn")
	}
	cp := append([]byte(nil), data...)
	s.writes = append(s.writes, cp)
	return nil
}

func (s *scriptConn) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

// fakeDialer fails failN times, then returns a fresh scripted connection.
type fakeDialer struct {
	t      *testing.T
	failN  int
	calls  int32
	failed error
}

func (d *fakeDialer) Dial(_ context.Context, _ string) (WSConn, error) {
	n := atomic.AddInt32(&d.calls, 1)
	if int(n) <= d.failN {
		return nil, d.failed
	}
	return newScriptConn(d.t), nil
}

// TestReconnectRetriesWithBackoffThenSucceeds proves the reconnect loop retries
// dial failures on the backoff schedule and, once a dial succeeds, completes the
// NIP-42 handshake and marks the session connected.
func TestReconnectRetriesWithBackoffThenSucceeds(t *testing.T) {
	t.Parallel()
	member, err := identity.Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	d := &fakeDialer{t: t, failN: 2, failed: errors.New("dial refused")}
	c := New("ws://fake", member,
		WithDialer(d),
		WithBackoff(Backoff{Initial: time.Millisecond, Max: 2 * time.Millisecond, MaxAttempts: 0}),
		WithLogf(func(string, ...interface{}) {}), // silence expected retry logs
	)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect should have succeeded after 2 dial failures: %v", err)
	}
	if got := atomic.LoadInt32(&d.calls); got != 3 {
		t.Fatalf("dialer calls=%d, want 3 (2 fail + 1 success)", got)
	}
	if m := c.Metrics(); m.Connects != 1 {
		t.Fatalf("metrics.Connects=%d, want 1", m.Connects)
	}
	// The handshake must have written the client's AUTH response over the conn.
	sc, _ := c.current().(*scriptConn)
	if sc == nil {
		t.Fatal("expected a live scriptConn after Connect")
	}
	sc.mu.Lock()
	nWrites := len(sc.writes)
	sc.mu.Unlock()
	if nWrites != 1 {
		t.Fatalf("scriptConn saw %d writes, want 1 (the signed AUTH response)", nWrites)
	}
}

// TestReconnectGivesUpAfterMaxAttempts proves the backoff loop stops and returns
// a loud error once MaxAttempts dial failures accrue — it does not spin forever.
func TestReconnectGivesUpAfterMaxAttempts(t *testing.T) {
	t.Parallel()
	member, _ := identity.Generate()
	d := &fakeDialer{t: t, failN: 1000, failed: errors.New("relay unreachable")}
	c := New("ws://fake", member,
		WithDialer(d),
		WithBackoff(Backoff{Initial: time.Millisecond, Max: time.Millisecond, MaxAttempts: 3}),
		WithLogf(func(string, ...interface{}) {}),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := c.Connect(ctx)
	if err == nil {
		t.Fatal("Connect should have failed after exhausting MaxAttempts")
	}
	if !strings.Contains(err.Error(), "gave up") {
		t.Fatalf("expected a 'gave up' error, got: %v", err)
	}
	if got := atomic.LoadInt32(&d.calls); got != 3 {
		t.Fatalf("dialer calls=%d, want 3 (MaxAttempts)", got)
	}
	if m := c.Metrics(); m.ReconnectFails != 1 {
		t.Fatalf("metrics.ReconnectFails=%d, want 1", m.ReconnectFails)
	}
}

// --- Unit: Send-triggered silent redial race (dontguess-6df) ----------------

// raceWSConn is a scripted WSConn for TestRecvDetectsSendTriggeredSilentRedial.
// WriteMessage fails exactly once if writeFail is set (simulating an Outbox
// publish write failure), then succeeds. ReadMessage returns queued frames in
// order; once exhausted it blocks on blockRead if set (never delivering — the
// way a real NIP-01 relay never would on a socket with no REQ replayed).
type raceWSConn struct {
	writeFail bool

	mu        sync.Mutex
	wrote     bool
	reads     [][]byte
	readIdx   int
	blockRead chan struct{}
	closed    bool
}

func (c *raceWSConn) WriteMessage(_ int, _ []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.writeFail && !c.wrote {
		c.wrote = true
		return errors.New("raceWSConn: simulated write failure")
	}
	c.wrote = true
	return nil
}

func (c *raceWSConn) ReadMessage() (int, []byte, error) {
	c.mu.Lock()
	if c.readIdx < len(c.reads) {
		b := c.reads[c.readIdx]
		c.readIdx++
		c.mu.Unlock()
		return websocket.TextMessage, b, nil
	}
	block := c.blockRead
	c.mu.Unlock()
	if block != nil {
		<-block
	}
	return 0, nil, errors.New("raceWSConn: no more scripted reads")
}

func (c *raceWSConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}

// raceDialer hands out conns from a fixed script, one per Dial call.
type raceDialer struct {
	conns []*raceWSConn
	idx   int32
}

func (d *raceDialer) Dial(_ context.Context, _ string) (WSConn, error) {
	n := atomic.AddInt32(&d.idx, 1)
	if int(n) > len(d.conns) {
		return nil, fmt.Errorf("raceDialer: exhausted (call %d, have %d)", n, len(d.conns))
	}
	return d.conns[n-1], nil
}

// TestRecvDetectsSendTriggeredSilentRedial reproduces the dontguess-6df race:
// *Conn multiplexes one shared c.ws between Recv (the single reader loop) and
// Send (the Outbox's publish, or the resubscriber's REQ). If a concurrent
// Send() write fails while the reader is BETWEEN Recv calls (not blocked
// inside ReadMessage), Send's own internal drop()+reconnect() (conn.go) redials
// a fresh socket and installs it as c.ws WITHOUT ever sending a REQ frame —
// Send only replays its own publish frame. Pre-fix, the reader's next Recv call
// fetched this fresh, unsubscribed-but-healthy socket and blocked reading on it
// FOREVER: no read error occurs (the socket is fine, just never subscribed), so
// nothing ever surfaces ErrConnDropped and the caller's resubscribe-before-read
// loop never fires — silent ingest death with no alarm.
//
// This test drives that exact sequence with two scripted connections and a
// fake dialer (no network): connection 1 delivers one frame then fails a
// Send() write (as an Outbox publish would); connection 2 is the fresh socket
// Send's internal redial installs, and its ReadMessage blocks forever (proving
// nothing was ever subscribed on it). The fix (Recv's generation guard) must
// make the reader's next Recv call return an error wrapping ErrConnDropped
// immediately, WITHOUT ever calling connection 2's ReadMessage — not hang.
func TestRecvDetectsSendTriggeredSilentRedial(t *testing.T) {
	t.Parallel()
	member, err := identity.Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	frame1, err := EncodeNotice("frame-1")
	if err != nil {
		t.Fatalf("EncodeNotice: %v", err)
	}
	conn1 := &raceWSConn{writeFail: true, reads: [][]byte{frame1}}
	blockRead := make(chan struct{})
	conn2 := &raceWSConn{blockRead: blockRead}
	t.Cleanup(func() { close(blockRead) })

	d := &raceDialer{conns: []*raceWSConn{conn1, conn2}}
	c := New("ws://fake", member,
		WithDialer(d),
		WithoutClientAuth(), // skip NIP-42; the race is at the transport layer, not auth
		WithBackoff(Backoff{Initial: time.Millisecond, Max: 2 * time.Millisecond, MaxAttempts: 0}),
		WithLogf(func(string, ...interface{}) {}),
	)
	t.Cleanup(func() { _ = c.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Reader iteration N: connects (dial #1 -> conn1), reads the queued frame.
	// This is the call that establishes the reader's claim on conn1's generation.
	raw, err := c.Recv(ctx)
	if err != nil {
		t.Fatalf("first Recv (connect+read): %v", err)
	}
	if f, perr := ParseFrame(raw); perr != nil || f.Message != "frame-1" {
		t.Fatalf("first frame: parsed=%v err=%v, want NOTICE frame-1", f, perr)
	}

	// Between Recv calls (the reader is not blocked inside ReadMessage): an
	// Outbox-style Send() publish fails its write on conn1, triggers Conn's OWN
	// internal drop+reconnect (dials conn2), and retries the publish frame on
	// conn2 — never touching the reader's subscription. Send must still report
	// success (its own retry succeeded) — this is the "silent" half of the bug.
	if err := c.Send(ctx, []byte(`["EVENT",{}]`)); err != nil {
		t.Fatalf("Send should have recovered via its own internal redial: %v", err)
	}
	if got := atomic.LoadInt32(&d.idx); got != 2 {
		t.Fatalf("dialer calls=%d, want 2 (conn1 then the Send-triggered redial to conn2)", got)
	}

	// Reader iteration N+1: Recv must detect that the live socket (conn2) is a
	// different generation than the one it last read from (conn1) and report the
	// drop loudly — NOT block forever inside conn2.ReadMessage() (which never
	// delivers: blockRead only closes at test teardown, proving this call never
	// reached it).
	done := make(chan error, 1)
	go func() {
		_, rerr := c.Recv(ctx)
		done <- rerr
	}()
	select {
	case rerr := <-done:
		if !errors.Is(rerr, ErrConnDropped) {
			t.Fatalf("expected ErrConnDropped from the generation-mismatch guard, got %v", rerr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Recv hung reading the unsubscribed socket Send silently redialed to — the dontguess-6df race reproduced (fix not effective)")
	}

	// Recovery: the caller (runReaderReconnect in production) now re-subscribes
	// over the SAME live socket (conn2) via a plain Send. That does not trigger
	// another redial (conn2's write succeeds), so the next Recv proceeds
	// normally on the now-claimed generation instead of mismatching again.
	if err := c.Send(ctx, []byte(`["REQ","dg-exchange",{}]`)); err != nil {
		t.Fatalf("resubscribe Send: %v", err)
	}
	frame2, err := EncodeNotice("frame-2")
	if err != nil {
		t.Fatalf("EncodeNotice: %v", err)
	}
	conn2.mu.Lock()
	conn2.reads = append(conn2.reads, frame2)
	conn2.mu.Unlock()

	raw2, err := c.Recv(ctx)
	if err != nil {
		t.Fatalf("Recv after resubscribe: %v", err)
	}
	if f, perr := ParseFrame(raw2); perr != nil || f.Message != "frame-2" {
		t.Fatalf("post-resubscribe frame: parsed=%v err=%v, want NOTICE frame-2", f, perr)
	}
}

// TestReconnectHonoursContextCancellation proves a cancelled context aborts the
// retry loop promptly rather than blocking on the backoff timer.
func TestReconnectHonoursContextCancellation(t *testing.T) {
	t.Parallel()
	member, _ := identity.Generate()
	d := &fakeDialer{t: t, failN: 1000, failed: errors.New("down")}
	c := New("ws://fake", member,
		WithDialer(d),
		WithBackoff(Backoff{Initial: 50 * time.Millisecond, Max: 50 * time.Millisecond, MaxAttempts: 0}),
		WithLogf(func(string, ...interface{}) {}),
	)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	err := c.Connect(ctx)
	if err == nil {
		t.Fatal("Connect should return an error when the context is cancelled")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got: %v", err)
	}
}
