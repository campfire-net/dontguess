package relay

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/campfire-net/dontguess/pkg/identity"
	"github.com/gorilla/websocket"
)

// textMessage mirrors gorilla/websocket.TextMessage (== 1). NIP-01/NIP-42
// frames are always UTF-8 JSON arrays, i.e. text frames.
const textMessage = websocket.TextMessage

// ErrConnDropped wraps the underlying transport error when a live relay
// connection drops mid-read or mid-write. Callers (the future Intake/Outbox)
// detect it with errors.Is to know the subscription state on the relay is gone
// and their REQ must be re-issued after the connection is re-established. It is
// LOUD by construction: the drop is never swallowed into a silent nil (LOCKED-5,
// relay-transport.md §0.5).
var ErrConnDropped = errors.New("relay connection dropped")

// WSConn is the message-oriented websocket relay.Conn drives. A real
// *github.com/gorilla/websocket.Conn satisfies it directly (and, via its
// WriteMessage/ReadMessage pair, identity.FrameConn — which is why the NIP-42
// handshake runs over it unchanged, relay-transport.md §2.1). The interface is
// the single seam tests inject a scripted connection through.
type WSConn interface {
	identity.FrameConn // WriteMessage(int, []byte) error; ReadMessage() (int, []byte, error)
	Close() error
}

// Dialer establishes a raw (pre-handshake) websocket connection to a relay URL.
// The default gorilla implementation is used in production; tests inject a fake
// to drive dial failures and reconnect/backoff deterministically without a
// network.
type Dialer interface {
	Dial(ctx context.Context, url string) (WSConn, error)
}

// gorillaDialer is the production Dialer backed by gorilla/websocket.
type gorillaDialer struct{}

func (gorillaDialer) Dial(ctx context.Context, url string) (WSConn, error) {
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, url, nil)
	if err != nil {
		return nil, err
	}
	return conn, nil
}

// Backoff configures the reconnect retry schedule. Delay starts at Initial and
// doubles up to Max. MaxAttempts caps the number of dial+auth attempts per
// reconnect call (0 = retry forever until the context is cancelled).
type Backoff struct {
	Initial     time.Duration
	Max         time.Duration
	MaxAttempts int
}

// DefaultBackoff is the production reconnect schedule: sub-second first retry,
// capped at 30s, retrying indefinitely (the operator keeps serving from its
// local fold at RF=1 while the relay is unreachable — relay-transport.md §3).
var DefaultBackoff = Backoff{Initial: 500 * time.Millisecond, Max: 30 * time.Second, MaxAttempts: 0}

// Metrics counts connection-lifecycle events for the loud-degradation model
// (relay-transport.md §3). The Intake/Outbox add their own counters; this is the
// conn-layer subset. Reads/writes are guarded by Conn.mu.
type Metrics struct {
	Connects       int64 // successful dial+auth completions
	Disconnects    int64 // live connection drops detected
	ReconnectFails int64 // reconnect calls that exhausted MaxAttempts
}

// Option customises a Conn.
type Option func(*Conn)

// WithDialer overrides the websocket dialer (tests inject a fake).
func WithDialer(d Dialer) Option { return func(c *Conn) { c.dialer = d } }

// WithBackoff overrides the reconnect schedule.
func WithBackoff(b Backoff) Option { return func(c *Conn) { c.backoff = b } }

// WithLogf overrides the loud-degradation logger (default log.Printf).
func WithLogf(logf func(format string, args ...interface{})) Option {
	return func(c *Conn) {
		if logf != nil {
			c.logf = logf
		}
	}
}

// Conn is a self-healing single-relay connection. It owns the websocket
// lifecycle: dial, the NIP-42 client handshake (via identity.ClientAuthenticate),
// and reconnect-with-backoff on drop. It does NOT own subscription state,
// sequencing, or the fold — those are the Intake/Outbox (out of scope here).
//
// Concurrency: Recv is intended to be called from a single reader goroutine.
// Send may be called concurrently with Recv (writes are serialised by writeMu).
// Reconnection is coordinated by mu; the blocking ReadMessage runs WITHOUT
// holding mu so a reconnect can proceed while a stale read is unwinding.
type Conn struct {
	url    string
	signer identity.Signer

	dialer  Dialer
	backoff Backoff
	logf    func(format string, args ...interface{})

	mu      sync.Mutex // guards ws + reconnection + metrics
	ws      WSConn
	writeMu sync.Mutex // serialises writes to the current ws

	metrics Metrics
}

// New builds a Conn for the given relay URL and signing identity. It does not
// dial; the first Send/Recv (or an explicit Connect) establishes the session.
func New(url string, signer identity.Signer, opts ...Option) *Conn {
	c := &Conn{
		url:     url,
		signer:  signer,
		dialer:  gorillaDialer{},
		backoff: DefaultBackoff,
		logf:    log.Printf,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Connect establishes an authenticated session if one is not already live. It
// is optional — Send/Recv connect lazily — but lets a caller fail fast on a
// misconfigured relay/identity at startup.
func (c *Conn) Connect(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reconnectLocked(ctx)
}

// Metrics returns a snapshot of the connection counters.
func (c *Conn) Metrics() Metrics {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.metrics
}

// current returns the live websocket, or nil if none.
func (c *Conn) current() WSConn {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ws
}

// Send writes a frame to the relay, connecting first if necessary. On a write
// failure it drops the dead connection, reconnects once (with backoff), and
// retries the write a single time. A persistent failure is returned loudly.
func (c *Conn) Send(ctx context.Context, frame []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	ws := c.current()
	if ws == nil {
		if err := c.reconnect(ctx); err != nil {
			return err
		}
		ws = c.current()
	}
	if err := ws.WriteMessage(textMessage, frame); err != nil {
		c.drop(ws, err)
		if rerr := c.reconnect(ctx); rerr != nil {
			return fmt.Errorf("relay send: reconnect after write error: %w", rerr)
		}
		ws = c.current()
		if err2 := ws.WriteMessage(textMessage, frame); err2 != nil {
			c.drop(ws, err2)
			return fmt.Errorf("relay send: write failed after reconnect: %w", err2)
		}
	}
	return nil
}

// Recv reads the next frame from the relay, connecting first if necessary. If
// the live connection drops mid-read it returns an error wrapping ErrConnDropped
// (never a silent nil) so the caller knows to re-issue its subscription; the
// NEXT Recv call transparently reconnects with backoff. Returning the drop —
// rather than swallowing it and auto-replaying the read — is deliberate: the
// relay has no memory of the old REQ, so the caller must re-subscribe
// (relay-transport.md §2.5).
func (c *Conn) Recv(ctx context.Context) ([]byte, error) {
	ws := c.current()
	if ws == nil {
		if err := c.reconnect(ctx); err != nil {
			return nil, err
		}
		ws = c.current()
	}
	_, data, err := ws.ReadMessage()
	if err != nil {
		c.drop(ws, err)
		return nil, fmt.Errorf("relay recv: %w: %v", ErrConnDropped, err)
	}
	return data, nil
}

// Close tears down the live connection. The Conn may be reused afterward (the
// next Send/Recv redials).
func (c *Conn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ws == nil {
		return nil
	}
	err := c.ws.Close()
	c.ws = nil
	return err
}

// drop closes and clears the connection if it is still the one that failed. If
// another goroutine already reconnected (c.ws now points at a fresh conn), the
// stale drop is a no-op. Every real drop is counted and logged loudly.
func (c *Conn) drop(ws WSConn, cause error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ws != ws || ws == nil {
		return
	}
	_ = c.ws.Close()
	c.ws = nil
	c.metrics.Disconnects++
	c.logf("relay: connection to %s dropped (disconnects=%d): %v", c.url, c.metrics.Disconnects, cause)
}

// reconnect acquires mu and (re)establishes the session. If the connection is
// already live (a concurrent caller won the race) it returns immediately.
func (c *Conn) reconnect(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reconnectLocked(ctx)
}

// reconnectLocked dials + runs the NIP-42 client handshake, retrying on failure
// per the backoff schedule. Callers must hold c.mu. On success c.ws is the new
// authenticated connection. Every failed attempt is logged loudly; exhausting
// MaxAttempts (or a cancelled context) returns an error.
func (c *Conn) reconnectLocked(ctx context.Context) error {
	if c.ws != nil {
		return nil
	}
	delay := c.backoff.Initial
	if delay <= 0 {
		delay = 10 * time.Millisecond
	}
	for attempt := 1; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("relay connect to %s: %w", c.url, err)
		}
		ws, err := c.dialAndAuth(ctx)
		if err == nil {
			c.ws = ws
			c.metrics.Connects++
			return nil
		}
		c.logf("relay: connect to %s attempt %d failed: %v", c.url, attempt, err)
		if c.backoff.MaxAttempts > 0 && attempt >= c.backoff.MaxAttempts {
			c.metrics.ReconnectFails++
			return fmt.Errorf("relay connect to %s: gave up after %d attempts: %w", c.url, attempt, err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("relay connect to %s: %w", c.url, ctx.Err())
		case <-time.After(delay):
		}
		if c.backoff.Max > 0 {
			if delay *= 2; delay > c.backoff.Max {
				delay = c.backoff.Max
			}
		}
	}
}

// dialAndAuth performs one dial + NIP-42 client handshake attempt. On any
// failure it closes the half-open connection so a retry starts clean.
func (c *Conn) dialAndAuth(ctx context.Context) (WSConn, error) {
	ws, err := c.dialer.Dial(ctx, c.url)
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}
	// A *websocket.Conn already satisfies identity.FrameConn, so the handshake
	// runs over it directly with no adapter (relay-transport.md §2.1).
	if err := identity.ClientAuthenticate(ws, c.signer, c.url); err != nil {
		_ = ws.Close()
		return nil, fmt.Errorf("nip-42 handshake: %w", err)
	}
	return ws, nil
}
