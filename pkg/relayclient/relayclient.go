// Package relayclient is the nostr-first CLIENT side of dontguess (team tier):
// sign(agentKey) -> submit(RelayTransport) -> await(per-phase predicate, bounded
// ctx). It is the counterpart to the operator-side pkg/relay transport built in
// dontguess-3b8/4bd; it deliberately does NOT reuse demuxPublisher (operator-
// coupled: its waiter is fed only by the operator's own runReader) and instead
// drives a single-goroutine send-EVENT -> Recv-loop+ParseFrame -> match
// OK-by-event-id, bounded end to end by the caller's context.
//
// Design authority: docs/design/nostr-first-client-ed2.md §3.1 (DQ1 — publish
// leg). This file implements item ed2-A: the publish primitive + put-reject
// surfacing. Buy/settle (ed2-B/ed2-C) are separate follow-on items.
//
// LOUD-EVERYWHERE discipline (relay-transport.md §0, carried into ed2 §5): a
// relay OK is a TRANSPORT RECEIPT ONLY, never reported as put success — see
// PutResult.Success. A malformed frame from a hostile/buggy relay is skipped,
// never panics. A stalled connection (accepts the socket, then sends nothing)
// fails loud inside the caller's bounded context, never hangs — see the
// watchdog dialer below.
package relayclient

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"github.com/campfire-net/dontguess/pkg/exchange"
	"github.com/campfire-net/dontguess/pkg/identity"
	"github.com/campfire-net/dontguess/pkg/nostr"
	"github.com/campfire-net/dontguess/pkg/proto"
	"github.com/campfire-net/dontguess/pkg/relay"
	"github.com/gorilla/websocket"
)

// DefaultBackoff is the client's bounded reconnect schedule. It deliberately
// does NOT reuse relay.DefaultBackoff (MaxAttempts=0, retry forever) — a
// one-shot CLI invocation against a dead or misconfigured relay must fail fast
// and loud, not retry indefinitely (design §3.1).
var DefaultBackoff = relay.Backoff{
	Initial:     300 * time.Millisecond,
	Max:         3 * time.Second,
	MaxAttempts: 4,
}

// DefaultTimeout is the default end-to-end bound for a Put call (dial +
// handshake + publish + await-OK + await-put-reject) when the caller does not
// supply its own deadline via ctx.
const DefaultTimeout = 15 * time.Second

// PutRequest is the caller-supplied content of a put. Content is the RAW
// (already-decoded) bytes — Put base64-encodes it onto the wire payload
// itself, mirroring the engine's applyPut expectation
// (pkg/exchange/state_put.go).
type PutRequest struct {
	Description string
	Content     []byte
	TokenCost   int64
	// ContentType is the FULL exchange content-type tag, e.g.
	// "exchange:content-type:code" (the engine strips the prefix internally).
	ContentType string
	Domains     []string
}

// PutResult is the outcome of a Put call.
//
//   - Accepted reflects the relay's ["OK", id, true/false] transport receipt
//     ONLY — the relay stored the event. It is NOT proof the operator admitted
//     the put into inventory (design §3.1: "a relay OK is a transport receipt
//     ONLY ... the client MUST NOT report success on OK").
//   - Rejected/RejectReason are populated only if a durable, signed
//     settle(put-reject) event referencing this put arrived within the bounded
//     await window (e.g. the seller's npub is not on the operator's
//     allowlist — engine_pricing.go rejectPutLocked, "dropped_unlisted").
//   - Success is the ONLY field callers should treat as "the put worked":
//     transport-accepted AND no reject observed within the bounded window. An
//     absent reject is not a guarantee the put was admitted (the operator may
//     still be reviewing it) — it means no rejection was OBSERVED in the
//     window, which is the actionable signal ed2-A promises.
type PutResult struct {
	PutID        string
	Accepted     bool
	OKMessage    string
	Rejected     bool
	RejectReason string
}

// Success reports whether the put transport-succeeded and no reject was
// observed within the bounded await window. Callers MUST use this — not
// Accepted alone — to decide whether to report success to the user (OK is not
// success; design §3.1).
func (r *PutResult) Success() bool {
	return r != nil && r.Accepted && !r.Rejected
}

// Option customises the *relay.Conn a caller builds via NewConn.
type Option func(*connConfig)

type connConfig struct {
	dialer    relay.Dialer
	backoff   relay.Backoff
	relayAuth bool
	logf      func(format string, args ...interface{})
}

// WithDialer overrides the underlying websocket dialer. Production callers
// never need this; tests inject a fake to drive dial failures / stalls
// deterministically without a network.
func WithDialer(d relay.Dialer) Option { return func(c *connConfig) { c.dialer = d } }

// WithBackoff overrides the bounded reconnect schedule (default
// DefaultBackoff).
func WithBackoff(b relay.Backoff) Option { return func(c *connConfig) { c.backoff = b } }

// WithLogf overrides the loud-degradation logger relay.Conn uses.
func WithLogf(f func(format string, args ...interface{})) Option {
	return func(c *connConfig) { c.logf = f }
}

// WithRelayAuth opts into the NIP-42 client AUTH handshake (the --relay-auth
// CLI flag). DEFAULT is WithoutClientAuth (design §3.1, H4): the production
// strfry relays gate writes by a signed-author allowlist and never push an
// AUTH challenge, and identity.ClientAuthenticate's ReadMessage has no
// deadline of its own — against a non-challenging relay the handshake would
// block forever without the watchdog this package wraps every dial in. Pass
// WithRelayAuth(true) only for a relay that DOES require NIP-42 AUTH.
func WithRelayAuth(enabled bool) Option { return func(c *connConfig) { c.relayAuth = enabled } }

// NewConn builds a *relay.Conn wired for the client: a bounded backoff (NOT
// relay.DefaultBackoff — a one-shot CLI must fail fast), WithoutClientAuth by
// default (WithRelayAuth opts in), and every dial wrapped in a ctx-driven
// watchdog (watchdogDialer, below) so a connect+handshake — or, since the
// watchdog's Close races the SAME ctx for the connection's whole lifetime, any
// later blocking Recv on a socket that accepts then stalls — fails loud inside
// the caller's bounded context instead of hanging (design §3.1, H4).
func NewConn(url string, signer identity.Signer, opts ...Option) *relay.Conn {
	cfg := &connConfig{
		dialer:  gorillaOrDefaultDialer(),
		backoff: DefaultBackoff,
	}
	for _, o := range opts {
		o(cfg)
	}
	relOpts := []relay.Option{
		relay.WithDialer(watchdogDialer{inner: cfg.dialer}),
		relay.WithBackoff(cfg.backoff),
	}
	if cfg.logf != nil {
		relOpts = append(relOpts, relay.WithLogf(cfg.logf))
	}
	if !cfg.relayAuth {
		relOpts = append(relOpts, relay.WithoutClientAuth())
	}
	return relay.New(url, signer, relOpts...)
}

// gorillaOrDefaultDialer returns the production dialer. Extracted to its own
// function only so tests can see the seam clearly; NewConn always wraps
// whatever dialer is chosen in watchdogDialer, so it must pick one explicitly
// here rather than leaving cfg.dialer nil.
func gorillaOrDefaultDialer() relay.Dialer { return productionDialer{} }

// productionDialer is a small equivalent of pkg/relay's unexported
// gorillaDialer (relay.New installs that one internally when WithDialer is
// never passed, but NewConn always passes WithDialer(watchdogDialer{...}), so
// it needs its own inner dialer to wrap). It dials with the same
// gorilla/websocket client relay.Conn uses in production.
type productionDialer struct{}

func (productionDialer) Dial(ctx context.Context, url string) (relay.WSConn, error) {
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, url, nil)
	if err != nil {
		return nil, err
	}
	return conn, nil
}

// watchdogDialer wraps an inner Dialer and races the caller's ctx against the
// dialed connection's lifetime: once Dial succeeds, a background goroutine
// waits on ctx.Done() and force-Closes the raw websocket the instant the
// caller's deadline expires. This is the mechanism behind design §3.1's "run
// connect+handshake under a ctx watchdog that Close()s the conn on ctx
// expiry": relay.Conn.Connect/reconnectLocked runs dialAndAuth (dial + the
// NIP-42 handshake, whose ReadMessage has NO deadline of its own —
// nip42_handshake.go:29) WHILE HOLDING Conn.mu, so a watchdog that called
// Conn.Close() would itself deadlock waiting for the same mutex. Closing the
// raw websocket directly (never touching Conn.mu) sidesteps that: a Close on
// a live *websocket.Conn always unblocks a pending ReadMessage with an error,
// regardless of what lock relay.Conn is holding.
//
// Because the watchdog goroutine's lifetime is scoped to the SAME ctx used for
// the whole Put/Buy call (not just the dial), it also covers every Recv AFTER
// a successful connect: a relay that accepts the socket, completes any
// handshake, and then simply sends nothing back is caught by the identical
// mechanism — the "accepts the socket then stalls" acceptance case (design
// §7.8) is a special case of this same watchdog, not a second one.
type watchdogDialer struct {
	inner relay.Dialer
}

func (d watchdogDialer) Dial(ctx context.Context, url string) (relay.WSConn, error) {
	ws, err := d.inner.Dial(ctx, url)
	if err != nil {
		return nil, err
	}
	go func() {
		<-ctx.Done()
		_ = ws.Close()
	}()
	return ws, nil
}

// Put signs req as an exchange:put event with signer (the AGENT key — never
// the operator key), publishes it on conn, and REQ-subscribes for a
// settle(put-reject) referencing the put id (design §3.1/§3.4). It blocks
// until ctx is done, a matching put-reject arrives, or a fatal transport error
// occurs — whichever is first. Every path is bounded by ctx; see PutResult and
// package doc for what "success" means.
func Put(ctx context.Context, conn *relay.Conn, signer identity.Signer, req PutRequest) (*PutResult, error) {
	if conn == nil {
		return nil, fmt.Errorf("relayclient: put: nil conn")
	}
	if signer == nil {
		return nil, fmt.Errorf("relayclient: put: nil signer")
	}
	msg, err := buildPutMessage(signer, req)
	if err != nil {
		return nil, fmt.Errorf("relayclient: put: %w", err)
	}
	ev, err := signAsIdentityEvent(signer, msg)
	if err != nil {
		return nil, fmt.Errorf("relayclient: put: sign event: %w", err)
	}
	putID := ev.ID

	frame, err := relay.EncodeEvent(ev)
	if err != nil {
		return nil, fmt.Errorf("relayclient: put %s: encode EVENT: %w", shortID(putID), err)
	}
	if err := conn.Send(ctx, frame); err != nil {
		return nil, fmt.Errorf("relayclient: put %s: publish EVENT: %w", shortID(putID), err)
	}

	result := &PutResult{PutID: putID}

	// After publishing, REQ-subscribe for settle(put-reject) referencing this
	// put (design §3.4: "The put client REQ-subscribes for settle(put-reject)
	// #e:[<put-id>]"). Subscribing is itself a bounded, loud operation — a
	// failure here means the client cannot observe a reject at all, so it must
	// not silently proceed as if the put succeeded.
	subID := "dg-put-" + shortID(putID)
	reqFrame, err := relay.EncodeReq(subID, relay.Filter{
		Kinds: []int{nostr.KindSettle},
		Tags:  map[string][]string{"e": {putID}},
	})
	if err != nil {
		return nil, fmt.Errorf("relayclient: put %s: encode put-reject REQ: %w", shortID(putID), err)
	}
	if err := conn.Send(ctx, reqFrame); err != nil {
		return nil, fmt.Errorf("relayclient: put %s: subscribe for put-reject: %w", shortID(putID), err)
	}

	for {
		raw, recvErr := conn.Recv(ctx)
		if recvErr != nil {
			if ctx.Err() != nil {
				if result.Accepted {
					// Transport-accepted, and the bounded reject-await window elapsed
					// with no reject observed. Per PutResult doc: this is the caller's
					// actionable "no rejection seen" signal, not a guarantee of
					// operator admission.
					return result, nil
				}
				return nil, fmt.Errorf("relayclient: put %s: timed out waiting for relay OK: %w", shortID(putID), ctx.Err())
			}
			return nil, fmt.Errorf("relayclient: put %s: relay connection dropped awaiting OK/reject: %w", shortID(putID), recvErr)
		}
		f, perr := relay.ParseFrame(raw)
		if perr != nil {
			// A malformed frame from a hostile or buggy relay is a loud-but-skip:
			// never panic, never treat garbage as an implicit success (LOCKED-5).
			continue
		}
		switch f.Type {
		case relay.LabelOK:
			if f.EventID == putID {
				result.Accepted = f.Accepted
				result.OKMessage = f.Message
			}
		case relay.LabelEVENT:
			if rejected, reason, ok := parsePutReject(f.Event, putID); ok && rejected {
				result.Rejected = true
				result.RejectReason = reason
				return result, nil
			}
		}
	}
}

// parsePutReject reports whether ev is a genuinely signed settle(put-reject)
// referencing putID. ok=false means "not a put-reject for us" (keep reading);
// ok=true, rejected=true is the terminal case.
func parsePutReject(ev *identity.Event, putID string) (rejected bool, reason string, ok bool) {
	if ev == nil || ev.Kind != nostr.KindSettle {
		return false, "", false
	}
	// Never trust an unsigned or forged event as a reject — a relay (or anyone
	// who can write to it) could otherwise spoof a put-reject to make a client
	// abandon a perfectly good put.
	if err := identity.VerifyEvent(ev); err != nil {
		return false, "", false
	}
	var payload struct {
		Phase   string `json:"phase"`
		EntryID string `json:"entry_id"`
		Reason  string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(ev.Content), &payload); err != nil {
		return false, "", false
	}
	if payload.Phase != "put-reject" || payload.EntryID != putID {
		return false, "", false
	}
	return true, payload.Reason, true
}

// buildPutMessage renders req into the proto.Message shape the engine's
// applyPut (pkg/exchange/state_put.go) expects: description/content/
// token_cost/content_type/domains in the JSON payload, content base64-encoded,
// tagged exchange:put.
func buildPutMessage(signer identity.Signer, req PutRequest) (*proto.Message, error) {
	if req.Description == "" {
		return nil, fmt.Errorf("empty description")
	}
	if len(req.Content) == 0 {
		return nil, fmt.Errorf("empty content")
	}
	if req.TokenCost <= 0 {
		return nil, fmt.Errorf("token_cost must be positive, got %d", req.TokenCost)
	}
	payload, err := json.Marshal(map[string]any{
		"description":  req.Description,
		"content":      base64.StdEncoding.EncodeToString(req.Content),
		"token_cost":   req.TokenCost,
		"content_type": req.ContentType,
		"domains":      req.Domains,
	})
	if err != nil {
		return nil, fmt.Errorf("encode put payload: %w", err)
	}
	return &proto.Message{
		Sender:    signer.PubKeyHex(),
		Payload:   payload,
		Tags:      []string{exchange.TagPut},
		Timestamp: time.Now().UnixNano(),
	}, nil
}

// signAsIdentityEvent converts msg through the production adapter
// (nostr.ToNostrEvent) and signs it with signer via identity.SignEvent — the
// exact sign(agentKey) chain design §3 mandates:
// nostr.ToNostrEvent(msg) -> identity.SignEvent(agentSigner, ev) ->
// relay.EncodeEvent(ev). SignEvent recomputes ev.ID as a content hash
// (pkg/identity/event.go computeID), so the returned event's ID is the
// deterministic, precomputable put id regardless of msg.ID.
func signAsIdentityEvent(signer identity.Signer, msg *proto.Message) (*identity.Event, error) {
	nev, err := nostr.ToNostrEvent(msg)
	if err != nil {
		return nil, fmt.Errorf("to nostr event: %w", err)
	}
	ev := &identity.Event{
		PubKey:    nev.PubKey,
		CreatedAt: nev.CreatedAt,
		Kind:      nev.Kind,
		Tags:      nev.Tags,
		Content:   nev.Content,
	}
	if err := identity.SignEvent(signer, ev); err != nil {
		return nil, fmt.Errorf("sign event: %w", err)
	}
	return ev, nil
}

// shortID truncates an event id for log/error messages.
func shortID(id string) string {
	if len(id) <= 16 {
		return id
	}
	return id[:16]
}
