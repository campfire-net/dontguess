package relay

// outbox.go is the PUBLISH leg of the single-relay transport
// (docs/design/relay-transport.md §2.3). It implements FOLD-THEN-PUBLISH: the
// local append is the SOLE source of truth; relay publish is asynchronous and
// strictly off the engine's hot path. The Outbox tails the LocalStore, and for
// every operator-authored (Origin=local) record it has not yet published, it
// signs a nostr EVENT, ships it to the relay, awaits the NIP-01 OK, and only
// then advances a crash-durable cursor.
//
// Two invariants this file enforces by construction:
//
//  1. Ping-pong prevention (§2.3): records with Origin="relay" are NEVER
//     republished. Skipping them by their store provenance marker is the whole
//     dedup mechanism — there is no separate seen-set. An event that arrived
//     over the relay must not be echoed back to the relay.
//
//  2. Crash-safe idempotent republish (ADV-2, §2.3): the durable cursor counts
//     Origin=local records that have been published AND ACKed. It is advanced
//     (fsynced) only AFTER the relay OK. A crash anywhere between the local fold
//     and the cursor advance leaves cursor < local-log-length, so on restart the
//     Outbox resumes from the cursor and republishes. Because the nostr event id
//     is a content hash, the relay re-ACKs the duplicate — replication factor is
//     restored with no divergence.
//
// The Outbox NEVER blocks the engine: it runs on its own goroutine, and a
// publish failure is loud-logged, counted (publish_retry), and retried with
// backoff. publish_lag (= local-log-length − cursor) is exported and alarmed
// above a threshold — that is exactly the RF-has-dropped-to-1 condition relay
// durability exists to surface.

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/campfire-net/dontguess/pkg/identity"
	"github.com/campfire-net/dontguess/pkg/nostr"
	"github.com/campfire-net/dontguess/pkg/store"
)

// localLog is the subset of *store.Store the Outbox tails. It is an interface so
// a test can drive the tail logic with a scripted log without a real file, and
// so the Outbox depends on the store's read surface only (never its writer).
type localLog interface {
	ReadAll() ([]store.Record, error)
}

// EventPublisher ships one signed nostr EVENT to the relay and blocks for its
// NIP-01 OK, returning whether the relay accepted it. It is the seam between the
// Outbox's tail/cursor logic and the wire: the production implementation
// (ConnPublisher) rides a relay.Conn; tests inject a fake to drive ACK, reject,
// and transient-failure paths deterministically.
type EventPublisher interface {
	PublishEvent(ctx context.Context, ev *identity.Event) (accepted bool, err error)
}

// isRelayOrigin reports whether a record was ingested from a relay (Origin
// "relay") rather than operator-authored (Origin "" or "local"). Relay-origin
// records are never republished (ping-pong prevention, §2.3).
func isRelayOrigin(origin string) bool {
	return origin == "relay"
}

// isLocalOrigin reports whether a record is operator-authored and therefore a
// publish candidate. "" is the legacy/default operator origin; "local" is the
// explicit form. Anything else (currently only "relay") is not local.
func isLocalOrigin(origin string) bool {
	return origin == "" || origin == "local"
}

// OutboxOption customises an Outbox.
type OutboxOption func(*Outbox)

// WithPublishBackoff overrides the per-event publish retry schedule.
func WithPublishBackoff(b Backoff) OutboxOption { return func(o *Outbox) { o.backoff = b } }

// WithOutboxLogf overrides the loud-degradation logger (default log.Printf).
func WithOutboxLogf(logf func(format string, args ...interface{})) OutboxOption {
	return func(o *Outbox) {
		if logf != nil {
			o.logf = logf
		}
	}
}

// WithLagAlarmThreshold sets the publish_lag value at (and above) which each
// tick loud-logs an alarm. 0 disables the alarm (lag is still exported).
func WithLagAlarmThreshold(n int64) OutboxOption {
	return func(o *Outbox) { o.lagAlarm = n }
}

// WithEmittedSeeder wires the operator-echo dedup seed (§D, relay-transport.md).
// seed is invoked with the SIGNED nostr event id (the NIP-01 content hash) of
// every operator-authored record STRICTLY BEFORE that event is published to the
// relay. Production wires it to Sequencer.MarkEmitted so the emitted-set already
// contains the content-hash id before any relay echo of that event can possibly
// arrive at the concurrent Intake subscriber — closing the seed-after-publish
// TOCTOU (a wave-9-review HIGH) whereby the echo re-folds the operator's own
// event and double-credits scrip.
//
// Why the SIGNED id and not the local record id: the Outbox re-signs each local
// record, and identity.SignEvent stamps a content-hash id that DIFFERS from the
// pre-signature store record id. The relay echo carries the signed id, so the
// seed MUST use the signed id (ev.ID after toSignedEvent) or the echo would not
// dedup (the earlier pre-sign-id bug this reworks).
//
// A nil seed (the default) is exactly today's behavior — no seeding, no echo
// dedup wiring — so the Outbox is drop-in when relay echo dedup is not wired.
func WithEmittedSeeder(seed func(id string)) OutboxOption {
	return func(o *Outbox) { o.seedEmitted = seed }
}

// WithAliasRegistrar wires the wire→store id alias (dontguess-55c GAP 1,
// docs/design/settle-wire-id-reconciliation-55c.md). register is invoked with the
// SIGNED nostr event id (the content-hash WIRE id a relay buyer sees + e-tags) and
// the pre-signature STORE id of every operator-authored record, STRICTLY BEFORE
// that event is published — the same seam and ordering as WithEmittedSeeder.
// Production wires it to exchange State.RegisterWireAlias so a team-tier buyer's
// settle antecedent, which can only carry the wire id, resolves back to the store
// id every state map is keyed by. Ordering holds by construction: the buyer cannot
// learn the wire id until after publish, and the alias is registered before
// publish, so it is always present before any settle referencing it can arrive.
//
// A nil register (the default) is exactly today's behavior — no alias wiring — so
// the Outbox is drop-in when the alias is not needed (no-relay / individual tier).
func WithAliasRegistrar(register func(wire, store string)) OutboxOption {
	return func(o *Outbox) { o.aliasRegistrar = register }
}

// Outbox publishes operator-authored local records to the relay, off the hot
// path, advancing a crash-durable cursor on each ACK. See the file doc for the
// two load-bearing invariants (ping-pong prevention, crash-safe republish).
//
// Concurrency: Tick is intended to be driven from a single goroutine (Run owns
// one). Notify may be called from any goroutine (it is how the engine's
// post-append hook wakes the Outbox). The exported metric accessors are safe to
// read concurrently.
type Outbox struct {
	log    localLog
	signer identity.Signer
	pub    EventPublisher

	cursor  *cursorFile
	backoff Backoff
	logf    func(format string, args ...interface{})

	// seedEmitted, if non-nil, is called with the SIGNED content-hash event id of
	// each operator-authored record STRICTLY BEFORE it is published (echo dedup,
	// §D). nil = today's behavior (no seeding). See WithEmittedSeeder.
	seedEmitted func(id string)

	// aliasRegistrar, if non-nil, is called with the SIGNED content-hash WIRE id
	// and the pre-signature STORE id of each operator-authored record STRICTLY
	// BEFORE it is published (wire→store alias, dontguess-55c GAP 1). nil = today's
	// behavior (no alias wiring). See WithAliasRegistrar.
	aliasRegistrar func(wire, store string)

	lagAlarm int64 // publish_lag threshold for the loud alarm (0 = disabled)

	publishRetry int64 // atomic: total publish attempts that failed and were retried
	publishLag   int64 // atomic: local-log-length − cursor, refreshed each tick

	signal chan struct{} // buffered(1) new-local-append wakeup for Run
}

// NewOutbox builds an Outbox over log, signing with signer and publishing via
// pub. cursorPath is the durable-cursor sidecar file (conventionally the log
// path + ".pubcursor"); it is read on construction so the Outbox resumes from
// where a prior process left off, and created lazily on the first advance.
func NewOutbox(log localLog, signer identity.Signer, pub EventPublisher, cursorPath string, opts ...OutboxOption) (*Outbox, error) {
	if log == nil {
		return nil, fmt.Errorf("outbox: nil local log")
	}
	if signer == nil {
		return nil, fmt.Errorf("outbox: nil signer")
	}
	if pub == nil {
		return nil, fmt.Errorf("outbox: nil publisher")
	}
	cf, err := openCursor(cursorPath)
	if err != nil {
		return nil, err
	}
	o := &Outbox{
		log:     log,
		signer:  signer,
		pub:     pub,
		cursor:  cf,
		backoff: DefaultBackoff,
		logf:    log2Printf,
		signal:  make(chan struct{}, 1),
	}
	for _, opt := range opts {
		opt(o)
	}
	return o, nil
}

// log2Printf is a package-level indirection so the default logger is a plain
// function value (not a method value) — keeps WithOutboxLogf's nil-guard simple.
func log2Printf(format string, args ...interface{}) { log.Printf(format, args...) }

// Cursor returns the number of Origin=local records published and ACKed so far
// (the durable high-water mark).
func (o *Outbox) Cursor() int64 { return o.cursor.Value() }

// PublishLag returns the exported publish_lag: the count of operator-authored
// local records that have been folded locally but not yet published+ACKed to the
// relay. A non-zero lag means those events currently live at RF=1.
func (o *Outbox) PublishLag() int64 { return atomic.LoadInt64(&o.publishLag) }

// PublishRetry returns the exported publish_retry counter: the total number of
// failed publish attempts that were retried.
func (o *Outbox) PublishRetry() int64 { return atomic.LoadInt64(&o.publishRetry) }

// Notify wakes a running Outbox to publish immediately rather than waiting for
// the next tick. It is the target of the engine's post-append hook (§2.1
// OnOperatorEmit). It never blocks: if a wakeup is already pending it is a no-op.
func (o *Outbox) Notify() {
	select {
	case o.signal <- struct{}{}:
	default:
	}
}

// Tick performs one publish pass: it replays the log, refreshes publish_lag, and
// publishes every not-yet-published Origin=local record in append order,
// advancing the durable cursor after each relay ACK. Origin=relay records are
// skipped entirely (ping-pong prevention) and do not count toward the cursor.
//
// A publish that ultimately fails (relay unreachable, context cancelled, or a
// relay reject) stops the pass with an error WITHOUT advancing past the failed
// record — publish_lag stays elevated and the next Tick retries from the same
// cursor. Returning the error is loud by construction; the caller (Run) logs and
// reschedules. The engine is never blocked because Tick runs on the Outbox's own
// goroutine.
func (o *Outbox) Tick(ctx context.Context) error {
	recs, err := o.log.ReadAll()
	if err != nil {
		return fmt.Errorf("outbox: read log: %w", err)
	}

	totalLocal := int64(0)
	for i := range recs {
		if isLocalOrigin(recs[i].Origin) {
			totalLocal++
		}
	}
	o.refreshLag(totalLocal)

	localIdx := int64(0)
	for i := range recs {
		rec := recs[i]
		if isRelayOrigin(rec.Origin) {
			// Never republish a relay-ingested record. This is the sole
			// ping-pong guard — no separate dedup set (§2.3).
			continue
		}
		if !isLocalOrigin(rec.Origin) {
			// Unknown origin: refuse to guess. Loud reject rather than silently
			// publishing or silently dropping (LOCKED-5 loud degradation).
			return fmt.Errorf("outbox: record %s has unknown origin %q", rec.ID, rec.Origin)
		}
		if localIdx < o.cursor.Value() {
			// Already published+ACKed in a prior pass (or prior process).
			localIdx++
			continue
		}

		ev, err := o.toSignedEvent(rec)
		if err != nil {
			// Operator-authored records are always valid exchange messages, so a
			// conversion/signing failure is a real defect, not a transient
			// condition. Fail loudly rather than skip (which would strand the
			// record at RF=1 forever with no signal).
			o.logf("outbox: FATAL cannot convert/sign local record %s for publish: %v", rec.ID, err)
			return fmt.Errorf("outbox: convert/sign record %s: %w", rec.ID, err)
		}

		// ECHO DEDUP SEED — seed STRICTLY BEFORE publish (§D, wave-9 TOCTOU fix).
		// The relay echoes a published event back to the concurrent Intake
		// subscriber the instant it accepts the EVENT frame; an echo can therefore
		// only ever arrive AFTER publishWithRetry has sent the frame. Seeding the
		// emitted-set with the signed content-hash id here — before publish — makes
		// the seed happen-before any possible echo, so Sequencer.Ingest dedups the
		// echo and it cannot re-fold (double scrip credit). MarkEmitted is
		// idempotent and pre-seeding a not-yet-published id is safe: if publish
		// fails and a later Tick republishes, toSignedEvent re-derives the IDENTICAL
		// content-hash id, so the seed still matches (never a stale/orphan seed).
		if o.seedEmitted != nil {
			o.seedEmitted(ev.ID)
		}

		// WIRE→STORE ALIAS (dontguess-55c GAP 1) — register STRICTLY BEFORE publish,
		// same ordering rationale as the echo-dedup seed above: the buyer cannot
		// learn ev.ID (the wire id) until this event reaches the relay, so recording
		// the ev.ID→rec.ID alias before publish guarantees it is present before any
		// settle e-tagging ev.ID can arrive. Idempotent; a republish re-derives the
		// IDENTICAL content-hash ev.ID, so the re-register is a no-op.
		if o.aliasRegistrar != nil {
			o.aliasRegistrar(ev.ID, rec.ID)
		}

		if err := o.publishWithRetry(ctx, ev); err != nil {
			return err
		}

		if err := o.cursor.Advance(); err != nil {
			// The relay ACKed but the durable cursor did not advance. This is the
			// SAFE failure direction: on restart we republish and the relay
			// re-ACKs (idempotent content-hash id). Surface it loudly.
			return fmt.Errorf("outbox: advance durable cursor after ACK of %s: %w", ev.ID, err)
		}
		o.refreshLag(totalLocal)
		localIdx++
	}
	return nil
}

// toSignedEvent converts a local store record into a signed nostr EVENT. The
// event id becomes a content hash (identity.SignEvent), which is what makes
// republish idempotent: a re-sent event carries the same id and the relay
// re-ACKs it (§2.3).
func (o *Outbox) toSignedEvent(rec store.Record) (*identity.Event, error) {
	msg := rec.ToMessage()
	nev, err := nostr.ToNostrEvent(&msg)
	if err != nil {
		return nil, fmt.Errorf("to nostr event: %w", err)
	}
	// nostr.Event and identity.Event are structurally identical but distinct
	// types; the adapter fills the structural fields (Kind/Tags/Content/PubKey/
	// CreatedAt) and does NOT sign. Copy into identity.Event and let SignEvent
	// stamp the canonical content-hash id + PubKey + Schnorr sig — the
	// content-hash id is what makes republish idempotent (§2.3).
	ev := &identity.Event{
		ID:        nev.ID,
		PubKey:    nev.PubKey,
		CreatedAt: nev.CreatedAt,
		Kind:      nev.Kind,
		Tags:      nev.Tags,
		Content:   nev.Content,
	}
	if err := identity.SignEvent(o.signer, ev); err != nil {
		return nil, fmt.Errorf("sign event: %w", err)
	}
	return ev, nil
}

// publishWithRetry ships ev and blocks for the relay OK, retrying on failure per
// the backoff schedule. Every failed attempt is loud-logged and counted
// (publish_retry). A relay reject (OK=false) is treated as a retryable loud
// failure — a persistently-rejected event keeps publish_lag elevated and
// alarming, which is the correct signal for operator investigation rather than a
// silent drop. Returns nil once the relay ACKs, or an error if the context is
// cancelled or the backoff's MaxAttempts is exhausted.
func (o *Outbox) publishWithRetry(ctx context.Context, ev *identity.Event) error {
	delay := o.backoff.Initial
	if delay <= 0 {
		delay = 10 * time.Millisecond
	}
	for attempt := 1; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("outbox: publish %s: %w", ev.ID, err)
		}
		accepted, err := o.pub.PublishEvent(ctx, ev)
		if err == nil && accepted {
			return nil
		}
		atomic.AddInt64(&o.publishRetry, 1)
		if err != nil {
			o.logf("outbox: publish %s attempt %d failed: %v", ev.ID, attempt, err)
		} else {
			o.logf("outbox: publish %s attempt %d REJECTED by relay (OK=false)", ev.ID, attempt)
		}
		if o.backoff.MaxAttempts > 0 && attempt >= o.backoff.MaxAttempts {
			return fmt.Errorf("outbox: publish %s: gave up after %d attempts", ev.ID, attempt)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("outbox: publish %s: %w", ev.ID, ctx.Err())
		case <-time.After(delay):
		}
		if o.backoff.Max > 0 {
			if delay *= 2; delay > o.backoff.Max {
				delay = o.backoff.Max
			}
		}
	}
}

// refreshLag recomputes and stores publish_lag = totalLocal − cursor, and
// loud-logs an alarm when the lag is at or above the configured threshold.
func (o *Outbox) refreshLag(totalLocal int64) {
	lag := totalLocal - o.cursor.Value()
	if lag < 0 {
		lag = 0
	}
	atomic.StoreInt64(&o.publishLag, lag)
	if o.lagAlarm > 0 && lag >= o.lagAlarm {
		o.logf("outbox: ALARM publish_lag=%d ≥ threshold=%d (those events are at RF=1 — relay unreachable or rejecting)", lag, o.lagAlarm)
	}
}

// Run drives Tick on an interval and on every Notify wakeup until the context is
// cancelled. A Tick error is loud-logged and the loop continues (the next tick
// retries from the durable cursor) — Run never returns on a transient publish
// failure, only on context cancellation.
func (o *Outbox) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := o.Tick(ctx); err != nil && ctx.Err() == nil {
			o.logf("outbox: tick failed (will retry): %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-o.signal:
		}
	}
}

// --- Connection-backed publisher -------------------------------------------

// frameConn is the subset of *relay.Conn the ConnPublisher rides: a
// send-a-frame / receive-a-frame pair. *Conn satisfies it directly. The
// interface is the seam a test injects a scripted relay through.
type frameConn interface {
	Send(ctx context.Context, frame []byte) error
	Recv(ctx context.Context) ([]byte, error)
}

// ConnPublisher is the production EventPublisher: it encodes ev as a NIP-01
// EVENT frame, sends it over a relay.Conn, and reads frames until the matching
// ["OK", <ev.id>, accepted, msg] arrives.
//
// Scope note: while only the Outbox is wired to a Conn (Intake is a separate
// workstream, §2.4), the ConnPublisher owns the Conn's read loop — it consumes
// every frame until its OK. When Intake lands it will own the read loop and hand
// OK frames to the Outbox; that demux is out of scope here (docs/design/
// relay-transport.md §2.3/§2.4). A malformed frame from a dumb/hostile relay is
// loud-logged and skipped rather than fatal (LOCKED-5).
type ConnPublisher struct {
	conn frameConn
	logf func(format string, args ...interface{})
}

// NewConnPublisher builds a ConnPublisher over a relay.Conn (or any frameConn).
func NewConnPublisher(conn frameConn, logf func(format string, args ...interface{})) *ConnPublisher {
	if logf == nil {
		logf = log2Printf
	}
	return &ConnPublisher{conn: conn, logf: logf}
}

// PublishEvent encodes and sends ev, then blocks reading frames until the relay
// returns the OK for ev.ID. Frames for other events (or NOTICE/EOSE) are
// ignored; a malformed frame is loud-logged and skipped.
func (p *ConnPublisher) PublishEvent(ctx context.Context, ev *identity.Event) (bool, error) {
	frame, err := EncodeEvent(ev)
	if err != nil {
		return false, fmt.Errorf("connpublisher: encode EVENT %s: %w", ev.ID, err)
	}
	if err := p.conn.Send(ctx, frame); err != nil {
		return false, fmt.Errorf("connpublisher: send EVENT %s: %w", ev.ID, err)
	}
	for {
		if err := ctx.Err(); err != nil {
			return false, fmt.Errorf("connpublisher: await OK for %s: %w", ev.ID, err)
		}
		raw, err := p.conn.Recv(ctx)
		if err != nil {
			return false, fmt.Errorf("connpublisher: await OK for %s: %w", ev.ID, err)
		}
		f, perr := ParseFrame(raw)
		if perr != nil {
			p.logf("connpublisher: skipping malformed frame while awaiting OK for %s: %v", ev.ID, perr)
			continue
		}
		if f.Type == LabelOK && f.EventID == ev.ID {
			return f.Accepted, nil
		}
		// Any other frame (a NOTICE, an EOSE, or an OK for a different event) is
		// not ours — keep reading.
	}
}

// --- Durable cursor sidecar -------------------------------------------------

// cursorFile is the crash-durable publish cursor: a one-line file next to the
// log holding the count of Origin=local records published+ACKed. It uses the
// same durability discipline as store.Append — every advance is fsynced before
// it is considered committed — implemented here as write-temp + fsync + atomic
// rename (+ best-effort directory fsync) so a crash mid-write can never leave a
// torn value: the reader sees either the old count or the new count, never a
// partial one.
type cursorFile struct {
	mu   sync.Mutex
	path string
	val  int64
}

// openCursor reads the existing cursor value (0 if the sidecar does not yet
// exist) and returns a cursorFile ready to advance.
func openCursor(path string) (*cursorFile, error) {
	if path == "" {
		return nil, fmt.Errorf("outbox: empty cursor path")
	}
	c := &cursorFile{path: path}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return c, nil
		}
		return nil, fmt.Errorf("outbox: read cursor %s: %w", path, err)
	}
	s := strings.TrimSpace(string(data))
	if s == "" {
		return c, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("outbox: parse cursor %s (value %q): %w", path, s, err)
	}
	if n < 0 {
		return nil, fmt.Errorf("outbox: cursor %s has negative value %d", path, n)
	}
	c.val = n
	return c, nil
}

// Value returns the current durable cursor value.
func (c *cursorFile) Value() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.val
}

// Advance increments the cursor by one and durably persists it, returning only
// after the new value is fsynced. On a persist failure the in-memory value is
// NOT advanced, so the caller (and a subsequent restart) both still see the
// pre-advance count — the record is republished, not silently skipped.
func (c *cursorFile) Advance() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	next := c.val + 1
	if err := c.persistLocked(next); err != nil {
		return err
	}
	c.val = next
	return nil
}

// persistLocked atomically writes n to the sidecar: temp file → fsync → rename →
// best-effort dir fsync. Caller holds c.mu.
func (c *cursorFile) persistLocked(n int64) error {
	dir := filepath.Dir(c.path)
	tmp, err := os.CreateTemp(dir, ".pubcursor-*.tmp")
	if err != nil {
		return fmt.Errorf("outbox: create temp cursor in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup if we bail before the rename commits.
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.WriteString(strconv.FormatInt(n, 10) + "\n"); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("outbox: write temp cursor: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("outbox: fsync temp cursor: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("outbox: close temp cursor: %w", err)
	}
	if err := os.Rename(tmpName, c.path); err != nil {
		return fmt.Errorf("outbox: rename cursor into place: %w", err)
	}
	committed = true

	// Best-effort directory fsync so the rename itself is durable across a crash.
	// A failure here is non-fatal: the rename is atomic, and the next advance
	// re-fsyncs — the worst case is a republish, which is idempotent.
	if d, derr := os.Open(dir); derr == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
