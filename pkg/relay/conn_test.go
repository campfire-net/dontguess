package relay

import (
	"context"
	"errors"
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
