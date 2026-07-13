package main

// serve_relay_ed2g_e2e_test.go — ed2-G (dontguess-392), the CACHE-SAFE
// ROUND-TRIP RELEASE GATE for the nostr-first client (design
// docs/design/nostr-first-client-ed2.md §6 item 8 + §7).
//
// It is cache-immune by construction, two ways (H7 / RT-C#1):
//
//   (a) These are PACKAGE-MAIN tests that drive the REAL client cobra RunE
//       (`dontguess put` / `dontguess buy [--preview]` via runPut/runBuy) and
//       the relayclient it calls, IN-PROCESS, against a full team-tier operator
//       serve stack (real Engine + Intake + Outbox + Sequencer + LocalScripStore
//       + wire→store alias + AutoDeliverOnBuyerAccept + a live TrustChecker) over
//       an in-process nostr websocket relay. Because the tests live in
//       `cmd/dontguess`, ANY edit to a `cmd/dontguess` source file (put.go,
//       buy.go, serve_relay.go, …) invalidates their test-cache entry — the exact
//       hole a doc comment could never close.
//
//   (b) The acceptance gate invokes an EXPLICIT NAMED, `-count=1` CI step
//       (`.github/workflows/ci.yml` "E2E acceptance gate (cache-immune)"):
//       `go test -race -count=1 -run TestE2E ./cmd/dontguess/... ./test/...`.
//       `-count=1` disables the cache unconditionally; the `TestE2E` prefix on
//       every test here is what that gate selects.
//
// THE CONSOLIDATED HAZARD MATRIX asserted here (§7), all THROUGH the client RunE
// + the full serve stack (nothing stubbed but the websocket wire itself):
//
//   1. allowlisted `dontguess put` → matchable inventory; a MINTED buyer's
//      `dontguess buy` → match → settle moves REAL scrip (buyer debited, seller
//      residual, IsMatchSettled) and delivers content BYTE-EXACT — end to end in
//      ONE client invocation — AND the match/deliver/consume all publish under a
//      30s outbox tick, so the ONLY thing that can have published them within the
//      buy timeout is the OnLocalAppend→Notify fast path (H1). The settle(complete)
//      → consume record publishes as KindConsume with NO Outbox FATAL (d52).
//   2. a NON-allowlisted `dontguess put` → the operator's durable put-reject is
//      RECEIVED and surfaced LOUD by the client (Seam A dropped_unlisted → Outbox
//      → wire → client), not silently swallowed.
//   3. an UNDERFUNDED buyer's `dontguess buy` → the operator's durable
//      settle(buyer-accept-reject) (reason:insufficient_scrip) is RECEIVED via the
//      per-phase #e:[buyer-accept] filter and surfaced by the client with the mint
//      hint — a distinguished UNDERFUNDED outcome, NOT a bare timeout (H2/H3).
//   4. an underfunded buyer that publishes buyer-accept + settle(deliver) receives
//      NO content: the operator emits ZERO content-carrying settle(deliver) and no
//      scrip moves — the Layer-0 free-content exploit is closed through the serve
//      stack (H2, design §3.7).
//   5. a client conn DROP mid-await recovers the match via re-subscribe and still
//      settles the full sale (H5, design §3.2).
//
// The --preview client-RunE path (buy → preview-request → FREE preview →
// buyer-accept e-tagging the preview → deliver → complete) is proven byte-exact
// by TestEd2C_RunBuy_PreviewFlag_SettlesContentAndMovesScrip (serve_relay_ed2c_test.go),
// also a package-main cobra-RunE test — referenced here rather than duplicated.
// The isolated §3.7 reservation-guard proof (operator-authored manual deliver
// trigger with no live reservation) is pkg/exchange
// TestUnfundedBuyerAcceptDeliver_NoFreeContent_ed2D.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/3dl-dev/dontguess/pkg/exchange"
	"github.com/3dl-dev/dontguess/pkg/identity"
	"github.com/3dl-dev/dontguess/pkg/nip44"
	"github.com/3dl-dev/dontguess/pkg/nostr"
	"github.com/3dl-dev/dontguess/pkg/relay"
	"github.com/3dl-dev/dontguess/pkg/scrip"
	dgstore "github.com/3dl-dev/dontguess/pkg/store"
	"golang.org/x/crypto/chacha20poly1305"
)

// knownV2PutPayload builds a §3.3 v2 content-confidentiality envelope for the
// SAME plaintext knownPutPayload carries, so a team-tier E2E fixture whose put
// is fail-closed-dropped as legacy plaintext (dontguess-4bed §6) can inject a
// well-formed encrypted put instead. It mirrors buildPutMessage byte-for-byte:
// a fresh CEK, ChaCha20-Poly1305(nonce||ct), a ciphertext_hash OVER the
// ciphertext, and the CEK NIP-44-wrapped from seller→operator so the operator
// unwraps it at fold time. The operator stores the decrypted plaintext in
// entry.Content, so the existing (pre-9e8) deliver path still returns the exact
// bytes the buyer asserts on.
func knownV2PutPayload(t *testing.T, seller identity.Signer, operatorPubHex, desc string, content []byte, tokenCost int64) []byte {
	t.Helper()
	cek := make([]byte, chacha20poly1305.KeySize)
	if _, err := rand.Read(cek); err != nil {
		t.Fatalf("gen CEK: %v", err)
	}
	aead, err := chacha20poly1305.New(cek)
	if err != nil {
		t.Fatalf("init AEAD: %v", err)
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		t.Fatalf("gen nonce: %v", err)
	}
	ciphertext := aead.Seal(nonce, nonce, content, nil)
	sum := sha256.Sum256(ciphertext)
	wrapped, err := nip44.Seal(seller, operatorPubHex, cek)
	if err != nil {
		t.Fatalf("wrap CEK to operator: %v", err)
	}
	p, err := json.Marshal(map[string]any{
		"v":            2,
		"description":  desc,
		"token_cost":   tokenCost,
		"content_type": "exchange:content-type:code",
		"domains":      []string{"go"},
		"enc": map[string]any{
			"content_alg":     "chacha20poly1305",
			"ciphertext_hash": "sha256:" + hex.EncodeToString(sum[:]),
			"ciphertext":      base64.StdEncoding.EncodeToString(ciphertext),
			"key_wrap": map[string]any{
				"alg":       "nip44-v2-secp256k1",
				"recipient": operatorPubHex,
				"wrapped":   wrapped,
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal v2 put payload: %v", err)
	}
	return p
}

// e2eLongPublishInterval is far larger than any buy timeout below, so the Outbox
// periodic tick provably cannot be the publisher of an operator match/deliver/
// consume: only an OnLocalAppend→Notify wakeup can. This is the discriminating
// contrast that makes the consolidated round-trip a faithful H1 regression (same
// technique as serve_relay_notify_test.go, but proven through the CLIENT).
const e2eLongPublishInterval = 30 * time.Second

// e2eShortPublishInterval is used by tests that do not assert H1 timing; the
// periodic tick is a belt-and-suspenders backstop behind the always-wired Notify.
const e2eShortPublishInterval = 25 * time.Millisecond

// e2eUnderfundedMint is the tiny balance minted to an "underfunded" buyer: it is
// >= DefaultMinBuyBalance (so the buy passes the D1 anonymous-buy bound and DOES
// match — the reject/deliver-gate paths are only reachable once a match exists),
// yet far below any real price+fee hold for the fixture entry (base price =
// tokenCost*0.70 = 5600), so the buyer-accept hold is GUARANTEED to fail.
const e2eUnderfundedMint = int64(100)

// --- team-tier serve stack (trust-checked, scrip-enabled, Notify-wired) -------

// e2eStack is one team-tier serve process composed exactly the way serve.go's
// `len(relayURLs) > 0` branch wires it: a real Engine sharing its LocalStore with
// a real Intake/Outbox/Sequencer over the in-process fake relay, a live
// TrustChecker (fleet allowlist + reputation floor), a live LocalScripStore
// (payment enforced), the wire→store alias, AutoDeliverOnBuyerAccept, and — the
// H1 fast path — OnLocalAppend fanned out to each leg's Outbox.Notify. Unlike
// newWireIDStack (no TrustChecker, no Notify) this stack can gate put admission
// (allowlisted vs not) AND publish an operator record the instant it is folded.
type e2eStack struct {
	ls    *dgstore.Store
	eng   *exchange.Engine
	scrip *scrip.LocalScripStore
	conn  *fakeRelayConn
	stop  func()
}

// newE2EStack builds and starts the stack over ls with operator `operator`, the
// fleet allowlist `allow` (hex pubkeys; the operator is always implicitly
// trusted), and the given Outbox publish interval. Notify is ALWAYS wired (the
// production team-tier config); `publishInterval` selects whether the periodic
// tick is a backstop (short) or provably-not-the-publisher (long, for H1).
func newE2EStack(t *testing.T, ctx context.Context, ls *dgstore.Store, operator identity.Signer, cursorPath string, publishInterval time.Duration, allow ...string) *e2eStack {
	t.Helper()

	ks := exchange.NewKeySet(allow...)
	tc, err := exchange.NewTrustChecker(operator.PubKeyHex(), ks)
	if err != nil {
		t.Fatalf("NewTrustChecker: %v", err)
	}
	ss, err := scrip.NewLocalScripStore(ls, operator.PubKeyHex())
	if err != nil {
		t.Fatalf("NewLocalScripStore: %v", err)
	}

	// OnLocalAppend fan-out (design §3.8, H1): the engine wakes every attached
	// leg's Outbox the instant an operator record is folded. Registered into the
	// notifier BEFORE the engine so appendLocalRecord fires it; the leg's Notify is
	// added by attachRelayTransport below.
	notifier := &appendNotifier{}

	eng := exchange.NewEngine(exchange.EngineOptions{
		CampfireID:               "local",
		LocalStore:               ls,
		OperatorPublicKey:        operator.PubKeyHex(),
		OperatorSigner:           operator,
		TrustChecker:             tc,
		ScripStore:               ss,
		MinBuyBalance:            exchange.DefaultMinBuyBalance,
		AutoDeliverOnBuyerAccept: true,
		PollInterval:             10 * time.Millisecond,
		OnLocalAppend:            notifier.fire,
		Logger:                   func(string, ...any) {},
	})
	tc.SetReputationFloor(eng.State().SellerReputation, exchange.DefaultMinReputation)

	conn := newFakeRelayConn(true /* echo */)
	stop, err := attachRelayTransport(ctx, ls, operator, operator.PubKeyHex(),
		cursorPath, conn, conn, publishInterval, nil, notifier,
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

	s := &e2eStack{ls: ls, eng: eng, scrip: ss, conn: conn}
	s.stop = func() { <-engDone; stop() }
	return s
}

// mint credits `key` `amount` scrip on the balance rail.
func (s *e2eStack) mint(t *testing.T, key string, amount int64) {
	t.Helper()
	if _, _, err := s.scrip.AddBudget(context.Background(), key, scrip.BalanceKey, amount, ""); err != nil {
		t.Fatalf("mint %s: %v", key, err)
	}
}

// matchStoreID returns the STORE id of the first operator-authored match record
// (the key IsMatchSettled is keyed by). The client only ever sees the WIRE id.
func (s *e2eStack) matchStoreID(t *testing.T) string {
	t.Helper()
	recs, _ := s.ls.ReadAll()
	rec, ok := firstLocalRecordWithTags(recs, exchange.TagMatch)
	if !ok {
		t.Fatalf("no operator match record persisted in the local log")
	}
	return rec.ID
}

// operatorDeliverContentCount counts operator-authored settle(deliver) records —
// i.e. real emitDeliverContent emissions. Zero for an underfunded buyer (H2).
func (s *e2eStack) operatorDeliverContentCount(t *testing.T) int {
	t.Helper()
	recs, _ := s.ls.ReadAll()
	return countLocalRecordsWithTags(recs, exchange.TagSettle, deliverPhaseTag)
}

// --- agent identity ----------------------------------------------------------

// newAgentIdentity provisions an isolated per-agent home and returns the signer
// + its home dir. AGENT_CF_HOME is set by the caller right before the RunE that
// must sign with this identity (runPut/runBuy resolve it via loadAgentSigner).
func newAgentIdentity(t *testing.T) (identity.Signer, string) {
	t.Helper()
	home := t.TempDir()
	id, _, err := identity.LoadOrCreate(home)
	if err != nil {
		t.Fatalf("LoadOrCreate agent identity: %v", err)
	}
	return id, home
}

// --- websocket hub (client <-> operator bridge) with a one-shot mid-await drop -

// e2eHub is a real in-process NIP-01 websocket relay bridging team-tier CLIENTS
// (the production relayclient dialer) to the in-process OPERATOR stack (opConn —
// the fakeRelayConn attachRelayTransport reads/writes). It is functionally the
// serve_relay_ed2c_test.go hub PLUS a one-shot "drop the client conn right after
// a buy EVENT" capability (armDropOnNextBuy) that forces the client's relay.Conn
// to re-dial and re-subscribe mid-await — the H5 fault injection.
type e2eHub struct {
	srv       *httptest.Server
	opConn    *fakeRelayConn
	dropOnBuy int32 // atomic one-shot: close the client ws after the next buy EVENT
	connCount int32 // atomic: websocket connections served (>=2 proves a re-dial)
}

func newE2EHub(t *testing.T, opConn *fakeRelayConn) *e2eHub {
	t.Helper()
	h := &e2eHub{opConn: opConn}
	mux := http.NewServeMux()
	mux.HandleFunc("/", h.serveWS)
	h.srv = httptest.NewServer(mux)
	t.Cleanup(h.srv.Close)
	return h
}

func (h *e2eHub) wsURL() string          { return wsURL(h.srv.URL) }
func (h *e2eHub) armDropOnNextBuy()      { atomic.StoreInt32(&h.dropOnBuy, 1) }
func (h *e2eHub) connectionsServed() int { return int(atomic.LoadInt32(&h.connCount)) }

// serveWS handles one client websocket: it injects client EVENTs into the
// operator's subscription (ACKing each with OK), registers the client's REQ
// filters, and runs a pump forwarding matching operator publishes back. When
// armed, it closes THIS connection immediately after injecting a buy EVENT.
func (h *e2eHub) serveWS(w http.ResponseWriter, r *http.Request) {
	atomic.AddInt32(&h.connCount, 1)
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	c := &ed2cClientConn{
		ws:        conn,
		filters:   map[string]relay.Filter{},
		forwarded: map[string]bool{},
		done:      make(chan struct{}),
	}
	defer close(c.done)
	go h.pump(c)

	for {
		_, raw, rerr := conn.ReadMessage()
		if rerr != nil {
			return
		}
		f, perr := relay.ParseFrame(raw)
		if perr != nil {
			continue
		}
		switch f.Type {
		case relay.LabelEVENT:
			if f.Event == nil {
				continue
			}
			h.opConn.inject(f.Event)
			ok, _ := relay.EncodeOK(f.Event.ID, true, "")
			c.write(ok)
			// H5: one-shot drop right after the buy is injected+ACKed. The operator
			// still folds+matches the buy (opConn is independent of this ws), but
			// this connection dies before the async match is pumped back — so the
			// client's relay.Conn must re-dial and its awaitBuyResponse must
			// re-subscribe to recover the match (design §3.2).
			if f.Event.Kind == nostr.KindBuy && atomic.CompareAndSwapInt32(&h.dropOnBuy, 1, 0) {
				return // defer conn.Close() drops the ws; the client re-dials to a fresh serveWS
			}
		case relay.LabelREQ:
			if len(f.Filters) > 0 {
				c.mu.Lock()
				c.filters[f.SubID] = f.Filters[0]
				c.mu.Unlock()
			}
		case relay.LabelCLOSE:
			c.mu.Lock()
			delete(c.filters, f.SubID)
			c.mu.Unlock()
		}
	}
}

// pump forwards every operator publish (opConn.events) matching a live client
// filter to that client subscription, once per (subID,eventID). It handles both
// live delivery and REQ-time replay (a filter registered after an event was
// published still receives it) — the strfry historical-replay the client's
// re-subscribe-on-drop discipline relies on. Reuses ed2cMatchFilter (a faithful
// #e matcher) and ed2cClientConn (both package-main, serve_relay_ed2c_test.go).
func (h *e2eHub) pump(c *ed2cClientConn) {
	tk := time.NewTicker(2 * time.Millisecond)
	defer tk.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-tk.C:
		}
		h.opConn.mu.Lock()
		evs := make([]*identity.Event, len(h.opConn.events))
		copy(evs, h.opConn.events)
		h.opConn.mu.Unlock()

		c.mu.Lock()
		filters := make(map[string]relay.Filter, len(c.filters))
		for k, v := range c.filters {
			filters[k] = v
		}
		c.mu.Unlock()

		for _, ev := range evs {
			for subID, f := range filters {
				key := subID + "|" + ev.ID
				c.mu.Lock()
				already := c.forwarded[key]
				c.mu.Unlock()
				if already {
					continue
				}
				if ed2cMatchFilter(f, ev) {
					c.mu.Lock()
					c.forwarded[key] = true
					c.mu.Unlock()
					frame, ferr := relay.EncodeSubEvent(subID, ev)
					if ferr == nil {
						c.write(frame)
					}
				}
			}
		}
	}
}

// --- (1) CONSOLIDATED ROUND-TRIP: put→buy→match→settle via client RunE + H1 + d52

// TestE2E_TeamRoundTrip_PutBuyMatchSettle_ClientRunE_NotifyDriven is the ed2-G
// release gate: it drives the ACTUAL `dontguess put` RunE (allowlisted seller)
// and the ACTUAL `dontguess buy` RunE (minted buyer) against the full team-tier
// serve stack over the websocket hub, and proves the whole cached-inference sale
// end to end.
//
// The Outbox publish interval is 30s, so the periodic tick provably cannot be the
// publisher of the operator match, the auto-delivered content, or the consume
// signal within the buy timeout — the ONLY thing that could is the OnLocalAppend→
// Notify fast path. A green run is therefore ALSO the H1 regression (a match
// published one outbox tick late would arrive at t=30s, far past the buy timeout,
// and the buy would time out AMBIGUOUS instead of settling).
func TestE2E_TeamRoundTrip_PutBuyMatchSettle_ClientRunE_NotifyDriven(t *testing.T) {
	// Capture the standard logger (Outbox FATAL is emitted there) FIRST so it is
	// restored LAST — after the stack cleanup drains the outbox goroutine (d52).
	var logbuf syncLogBuf
	prevOut := log.Writer()
	log.SetOutput(&logbuf)
	t.Cleanup(func() { log.SetOutput(prevOut) })

	dir := t.TempDir()
	ls, err := dgstore.Open(dir + "/events.jsonl")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = ls.Close() })

	operator, _ := identity.Generate()
	seller, sellerHome := newAgentIdentity(t)
	buyer, buyerHome := newAgentIdentity(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	// Allowlist BOTH agent keys: the seller's put is a fleet-member put (Seam A),
	// and the buyer's buyer-accept/complete are fleet-member settle phases the
	// team-tier trust gate (Seam B) requires TrustAllowlisted (trust.go
	// defaultSettlePhaseLevels). A buy itself is TrustAnonymous, but the settle
	// chain is not — so an un-allowlisted buyer would match then stall at settle.
	st := newE2EStack(t, ctx, ls, operator, dir+"/events.jsonl.pubcursor",
		e2eLongPublishInterval, seller.PubKeyHex(), buyer.PubKeyHex())
	t.Cleanup(func() { cancel(); st.stop() })
	hub := newE2EHub(t, st.conn)

	// --- allowlisted `dontguess put` RunE → matchable inventory ---
	t.Setenv("AGENT_CF_HOME", sellerHome)
	putCmd := newPutCmd()
	var putOut, putErr bytes.Buffer
	putCmd.SetOut(&putOut)
	putCmd.SetErr(&putErr)
	setPutFlags(t, putCmd, map[string]string{
		"description":   ed2cPutDesc,
		"content":       base64.StdEncoding.EncodeToString(ed2cContent),
		"token_cost":    "8000",
		"content_type":  "exchange:content-type:code",
		"domains":       "go",
		"relay":         hub.wsURL(),
		"timeout":       "3s", // no put-reject expected → returns after the bounded window
		"operator-npub": operator.Npub(),
	})
	if err := runPut(putCmd, nil); err != nil {
		t.Fatalf("runPut (allowlisted seller) returned error: %v\nstdout:\n%s\nstderr:\n%s", err, putOut.String(), putErr.String())
	}
	if strings.Contains(putOut.String(), "REJECTED") {
		t.Fatalf("allowlisted put surfaced a spurious REJECTED:\n%s", putOut.String())
	}
	waitFor(t, 8*time.Second, "client-published allowlisted put auto-accepts into matchable inventory", func() bool {
		return len(st.eng.State().Inventory()) == 1
	})
	if got := st.eng.State().Inventory()[0].SellerKey; got != seller.PubKeyHex() {
		t.Fatalf("matchable entry seller = %s, want the allowlisted seller %s", got, seller.PubKeyHex())
	}

	// --- minted `dontguess buy` RunE → match → settle → content + scrip ---
	st.mint(t, buyer.PubKeyHex(), wireIDBuyerMint)
	t.Setenv("AGENT_CF_HOME", buyerHome)

	buyCmd := newBuyCmd()
	var buyOut, buyErr bytes.Buffer
	buyCmd.SetOut(&buyOut)
	buyCmd.SetErr(&buyErr)
	setBuyFlags(t, buyCmd, map[string]string{
		"task":    ed2cBuyTask,
		"budget":  "1000000",
		"relay":   hub.wsURL(),
		"timeout": "20s", // < the 30s tick: a green settle here is the H1 proof
	})
	start := time.Now()
	if err := runBuy(buyCmd, nil); err != nil {
		t.Fatalf("runBuy (team hit) returned error: %v\nstderr:\n%s", err, buyErr.String())
	}
	elapsed := time.Since(start)

	// (a) content IN HAND, byte-exact, on the pipeable stdout channel.
	if !bytes.Equal(buyOut.Bytes(), ed2cContent) {
		t.Fatalf("delivered content mismatch.\n got (%d bytes): %q\nwant (%d bytes): %q",
			buyOut.Len(), buyOut.String(), len(ed2cContent), string(ed2cContent))
	}
	if !strings.Contains(buyErr.String(), "SETTLED") {
		t.Fatalf("stderr does not surface the SETTLED outcome:\n%s", buyErr.String())
	}
	// H1: the whole match→deliver→complete chain settled well within the 30s tick.
	if elapsed >= e2eLongPublishInterval {
		t.Fatalf("H1 REGRESSION: buy took %s (>= the %s outbox tick) — the match did not publish on fold via Notify", elapsed, e2eLongPublishInterval)
	}

	// (b) REAL scrip moved through the engine.
	waitFor(t, 8*time.Second, "buyer debited a real price+fee hold", func() bool {
		return st.scrip.Balance(buyer.PubKeyHex()) < wireIDBuyerMint
	})
	matchStore := st.matchStoreID(t)
	waitFor(t, 8*time.Second, "match settles (durable scrip-settle) on the client's complete", func() bool {
		return st.eng.State().IsMatchSettled(matchStore)
	})
	waitFor(t, 8*time.Second, "seller credited the residual", func() bool {
		return st.scrip.Balance(seller.PubKeyHex()) > 0
	})
	if got := st.scrip.Balance(buyer.PubKeyHex()); got >= wireIDBuyerMint {
		t.Fatalf("buyer not debited: balance=%d, want < %d", got, wireIDBuyerMint)
	}

	// (c) d52: the settle(complete)→consume record PUBLISHES (KindConsume) via the
	// 30s-tick-immune Notify path, and the Outbox did NOT go FATAL.
	waitFor(t, 8*time.Second, "settle(complete)->consume publishes to the relay (KindConsume)", func() bool {
		return len(st.conn.receivedByKind(nostr.KindConsume)) >= 1
	})
	if logs := logbuf.String(); strings.Contains(logs, "outbox: FATAL") {
		t.Fatalf("Outbox went FATAL during the team-tier round-trip:\n%s", logs)
	}
}

// --- (2) NON-ALLOWLISTED put → LOUD put-reject surfaced by the client ---------

// TestE2E_NonAllowlistedPut_SurfacesLoudPutReject_ClientRunE drives the ACTUAL
// `dontguess put` RunE with a NON-allowlisted agent against the full serve stack
// and proves the operator's durable put-reject (Seam A dropped_unlisted →
// rejectPutLocked → Outbox → hub) is RECEIVED via the client's #e:[put-id]
// subscription and surfaced LOUD — a non-zero exit carrying the reason and the
// actionable allowlist instruction, not a silent "accepted".
func TestE2E_NonAllowlistedPut_SurfacesLoudPutReject_ClientRunE(t *testing.T) {
	hushRelayLogs(t)
	dir := t.TempDir()
	ls, err := dgstore.Open(dir + "/events.jsonl")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = ls.Close() })

	operator, _ := identity.Generate()
	goodSeller, _ := identity.Generate() // some other key IS allowlisted (so the empty-allowlist trivial case is excluded)
	_, attackerHome := newAgentIdentity(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	// The attacker is deliberately NOT on the allowlist.
	st := newE2EStack(t, ctx, ls, operator, dir+"/events.jsonl.pubcursor",
		e2eShortPublishInterval, goodSeller.PubKeyHex())
	t.Cleanup(func() { cancel(); st.stop() })
	hub := newE2EHub(t, st.conn)

	t.Setenv("AGENT_CF_HOME", attackerHome)
	cmd := newPutCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	setPutFlags(t, cmd, map[string]string{
		"description":   "Reverse a linked list in place, iterative",
		"content":       base64.StdEncoding.EncodeToString([]byte("some content the operator must never make matchable")),
		"token_cost":    "8500",
		"content_type":  "exchange:content-type:code",
		"relay":         hub.wsURL(),
		"timeout":       "10s",
		"operator-npub": operator.Npub(),
	})
	err = runPut(cmd, nil)
	if err == nil {
		t.Fatalf("expected a LOUD put-reject error; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	// The reject was RECEIVED (not a bare "accepted; no reject observed"): the error
	// carries the operator's rejection and the actionable allowlist instruction.
	if !strings.Contains(err.Error(), "rejected by operator") {
		t.Fatalf("error %q does not surface the RECEIVED operator put-reject", err)
	}
	if !strings.Contains(err.Error(), "dropped_unlisted") {
		t.Fatalf("error %q does not carry the Seam A dropped_unlisted reason", err)
	}
	if !strings.Contains(stdout.String(), "REJECTED") || !strings.Contains(stdout.String(), "dontguess allowlist add") {
		t.Fatalf("stdout does not surface the LOUD REJECTED + allowlist remediation:\n%s", stdout.String())
	}
	// The rejected put is provably NOT matchable.
	if n := len(st.eng.State().Inventory()); n != 0 {
		t.Fatalf("Seam A FAILED: non-allowlisted put entered matchable inventory (%d entries, want 0)", n)
	}
	if got := st.eng.DegradationSnapshot().DroppedUnlisted; got < 1 {
		t.Fatalf("expected the attacker put counted dropped_unlisted, got %d", got)
	}
}

// --- (3) UNDERFUNDED buyer-accept → LOUD reject RECEIVED + surfaced (H2/H3) ----

// TestE2E_UnderfundedBuyerAccept_ReceivesLoudReject_ClientRunE drives the ACTUAL
// `dontguess buy` RunE with a buyer funded enough to MATCH (passes the D1 bound)
// but NOT enough to cover the buyer-accept hold, and proves the operator's durable
// settle(buyer-accept-reject) (reason:insufficient_scrip) is RECEIVED via the
// per-phase #e:[buyer-accept] filter (H3) and surfaced by the client as a
// DISTINGUISHED UNDERFUNDED outcome with the mint hint — never a bare timeout.
func TestE2E_UnderfundedBuyerAccept_ReceivesLoudReject_ClientRunE(t *testing.T) {
	hushRelayLogs(t)
	dir := t.TempDir()
	ls, err := dgstore.Open(dir + "/events.jsonl")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = ls.Close() })

	operator, _ := identity.Generate()
	seller, _ := identity.Generate()
	buyer, buyerHome := newAgentIdentity(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	// Both fleet keys allowlisted: the underfunded buyer must PASS the Seam B trust
	// gate (buyer-accept is TrustAllowlisted) so its buyer-accept reaches the scrip
	// hold and FAILS there — the reject is a scrip verdict, not a trust drop.
	st := newE2EStack(t, ctx, ls, operator, dir+"/events.jsonl.pubcursor",
		e2eShortPublishInterval, seller.PubKeyHex(), buyer.PubKeyHex())
	t.Cleanup(func() { cancel(); st.stop() })
	hub := newE2EHub(t, st.conn)

	// Seed inventory: an allowlisted seller's put (injected directly — the put path
	// is proven in test 1; this test's subject is the underfunded buyer).
	putEv := signExchangeEvent(t, seller,
		[]string{exchange.TagPut, "exchange:content-type:code", "exchange:domain:go"}, nil,
		knownV2PutPayload(t, seller, operator.PubKeyHex(), ed2cPutDesc, ed2cContent, ed2cTokenCost))
	st.conn.inject(putEv)
	waitFor(t, 8*time.Second, "seller put auto-accepts into inventory", func() bool {
		return len(st.eng.State().Inventory()) == 1
	})

	st.mint(t, buyer.PubKeyHex(), e2eUnderfundedMint) // passes D1, fails the hold
	t.Setenv("AGENT_CF_HOME", buyerHome)

	cmd := newBuyCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	setBuyFlags(t, cmd, map[string]string{
		"task":    ed2cBuyTask,
		"budget":  "1000000", // the buyer LIES about willingness; the BALANCE gates the hold
		"relay":   hub.wsURL(),
		"timeout": "20s",
	})
	err = runBuy(cmd, nil)
	if err == nil {
		t.Fatalf("expected a LOUD underfunded error; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if !strings.Contains(err.Error(), "insufficient_scrip") {
		t.Fatalf("error %q does not surface the RECEIVED insufficient_scrip reject (H3: was the per-phase #e:[buyer-accept] filter used?)\nstderr:\n%s", err, stderr.String())
	}
	if !strings.Contains(err.Error(), "dontguess mint") {
		t.Fatalf("error %q does not surface the actionable mint instruction", err)
	}
	if !strings.Contains(stderr.String(), "UNDERFUNDED") {
		t.Fatalf("stderr does not surface the distinguished UNDERFUNDED outcome (not a bare timeout):\n%s", stderr.String())
	}
	// No content delivered, and no scrip moved.
	if stdout.Len() != 0 {
		t.Fatalf("underfunded buyer received content on stdout (%d bytes): %q", stdout.Len(), stdout.String())
	}
	if got := st.scrip.Balance(buyer.PubKeyHex()); got != e2eUnderfundedMint {
		t.Fatalf("underfunded buyer balance moved: got %d, want untouched %d (no hold should succeed)", got, e2eUnderfundedMint)
	}
	if got := st.scrip.Balance(seller.PubKeyHex()); got != 0 {
		t.Fatalf("seller credited %d on an underfunded buy — no scrip should have moved", got)
	}
}

// --- (4) UNDERFUNDED buyer + settle(deliver) → NO free content (H2) -----------

// TestE2E_UnderfundedDeliver_NoFreeContent_ThroughServeStack drives the free-content
// exploit through the full serve stack: an underfunded buyer's buy matches, its
// buyer-accept fails the hold (no reservation saved), and it then publishes a
// settle(deliver) to pull content anyway. The operator emits ZERO content-carrying
// settle(deliver) and no scrip moves — the Layer-0 exploit is closed (design §3.7).
// (The isolated operator-authored-trigger reservation-guard proof is pkg/exchange
// TestUnfundedBuyerAcceptDeliver_NoFreeContent_ed2D; this is the serve-stack, cache-
// immune end-to-end variant.)
func TestE2E_UnderfundedDeliver_NoFreeContent_ThroughServeStack(t *testing.T) {
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

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	// Buyer allowlisted so its buyer-accept passes Seam B and reaches the scrip
	// hold (which fails). An un-allowlisted buyer would be trust-dropped, and the
	// "no free content" assertion would pass VACUOUSLY (nothing ran) — so the buyer
	// MUST be a fleet member for this exploit test to be meaningful.
	st := newE2EStack(t, ctx, ls, operator, dir+"/events.jsonl.pubcursor",
		e2eShortPublishInterval, seller.PubKeyHex(), buyer.PubKeyHex())
	t.Cleanup(func() { cancel(); st.stop() })

	st.mint(t, buyer.PubKeyHex(), e2eUnderfundedMint) // passes D1 so the buy matches; fails the hold

	supplyBefore := st.scrip.TotalSupply()

	// put → buy → operator match.
	putEv := signExchangeEvent(t, seller,
		[]string{exchange.TagPut, "exchange:content-type:code", "exchange:domain:go"}, nil,
		knownV2PutPayload(t, seller, operator.PubKeyHex(), ed2cPutDesc, ed2cContent, ed2cTokenCost))
	st.conn.inject(putEv)
	waitFor(t, 8*time.Second, "seller put auto-accepts into inventory", func() bool {
		return len(st.eng.State().Inventory()) == 1
	})

	buyEv := signExchangeEvent(t, buyer, []string{exchange.TagBuy}, nil,
		localBuyPayload(ed2cBuyTask, 50000))
	st.conn.inject(buyEv)
	waitFor(t, 8*time.Second, "operator match published OUT to the relay", func() bool {
		return len(st.conn.receivedByKind(nostr.KindMatch)) >= 1
	})
	matchWire := st.conn.receivedByKind(nostr.KindMatch)[0].ID
	matchStore := st.matchStoreID(t)

	// ATTACK STEP 1 — buyer-accept from the underfunded buyer. The hold fails; the
	// operator emits a durable buyer-accept-reject and saves NO reservation.
	acceptPayload, _ := json.Marshal(map[string]any{"entry_id": ""})
	acceptEv := signExchangeEvent(t, buyer,
		[]string{exchange.TagSettle, exchange.TagPhasePrefix + exchange.SettlePhaseStrBuyerAccept},
		[]string{matchWire}, acceptPayload)
	st.conn.inject(acceptEv)
	waitFor(t, 8*time.Second, "operator emits the durable buyer-accept-reject (insufficient scrip)", func() bool {
		recs, _ := st.ls.ReadAll()
		_, ok := firstLocalRecordWithTags(recs, exchange.TagSettle,
			exchange.TagPhasePrefix+exchange.SettlePhaseStrBuyerAcceptReject)
		return ok
	})

	// Precondition: the failed buyer-accept auto-delivered NOTHING (the auto-deliver
	// path is gated on a durable hold, which did not save — design §3.7).
	if n := st.operatorDeliverContentCount(t); n != 0 {
		t.Fatalf("FREE-CONTENT EXPLOIT OPEN: %d content deliver(s) for the failed hold, want 0", n)
	}

	// ATTACK STEP 2 — pull the content anyway: the attacker publishes a
	// settle(deliver) e-tagging its buyer-accept. This is DROPPED before it can move
	// content — settle(deliver) is an operator-only phase (TrustOperator; a buyer
	// holds only TrustAllowlisted) AND the Intake operator-authorship gate refuses a
	// non-operator deliver, so it never even folds. Prove the exploit STAYS closed
	// over a bounded window after the injection (regardless of where the forged
	// deliver dies): zero content delivers, match never settles, no scrip motion.
	deliverPayload, _ := json.Marshal(map[string]any{"entry_id": ""})
	deliverEv := signExchangeEvent(t, buyer,
		[]string{exchange.TagSettle, exchange.TagPhasePrefix + exchange.SettlePhaseStrDeliver},
		[]string{acceptEv.ID}, deliverPayload)
	st.conn.inject(deliverEv)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if n := st.operatorDeliverContentCount(t); n != 0 {
			t.Fatalf("FREE-CONTENT EXPLOIT OPEN: operator emitted %d content deliver(s) for an unfunded buyer (H2 / design §3.7 regression)", n)
		}
		if st.eng.State().IsMatchSettled(matchStore) {
			t.Fatalf("match settled for an unfunded buyer — no scrip should have moved")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// No scrip moved and total supply is conserved (no mint, no burn).
	if got := st.scrip.Balance(buyer.PubKeyHex()); got != e2eUnderfundedMint {
		t.Fatalf("buyer balance moved: got %d, want untouched %d", got, e2eUnderfundedMint)
	}
	if got := st.scrip.Balance(seller.PubKeyHex()); got != 0 {
		t.Fatalf("seller credited %d on an unfunded exploit attempt — no scrip should have moved", got)
	}
	if got := st.scrip.TotalSupply(); got != supplyBefore {
		t.Fatalf("total scrip supply changed: got %d, want %d (supply must be conserved)", got, supplyBefore)
	}
}

// --- (5) CONN DROP mid-await recovers the match via re-subscribe (H5) ----------

// TestE2E_ConnDropMidAwait_RecoversMatch_ClientRunE drives the ACTUAL `dontguess
// buy` RunE against the hub with a one-shot mid-await drop armed: the hub closes
// the client websocket the instant the buy EVENT is injected — before the async
// operator match is pumped back. The client's relay.Conn transparently re-dials
// and awaitBuyResponse re-subscribes (a fresh #e:[buyID] REQ with `since`), which
// recovers the match the hub replays on the new connection (design §3.2, H5). The
// full sale then settles: content byte-exact + REAL scrip moved — proving recovery
// is complete, not partial.
func TestE2E_ConnDropMidAwait_RecoversMatch_ClientRunE(t *testing.T) {
	hushRelayLogs(t)
	dir := t.TempDir()
	ls, err := dgstore.Open(dir + "/events.jsonl")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = ls.Close() })

	operator, _ := identity.Generate()
	seller, _ := identity.Generate()
	buyer, buyerHome := newAgentIdentity(t)

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	st := newE2EStack(t, ctx, ls, operator, dir+"/events.jsonl.pubcursor",
		e2eShortPublishInterval, seller.PubKeyHex(), buyer.PubKeyHex())
	t.Cleanup(func() { cancel(); st.stop() })
	hub := newE2EHub(t, st.conn)

	// Seed inventory.
	putEv := signExchangeEvent(t, seller,
		[]string{exchange.TagPut, "exchange:content-type:code", "exchange:domain:go"}, nil,
		knownV2PutPayload(t, seller, operator.PubKeyHex(), ed2cPutDesc, ed2cContent, ed2cTokenCost))
	st.conn.inject(putEv)
	waitFor(t, 8*time.Second, "seller put auto-accepts into inventory", func() bool {
		return len(st.eng.State().Inventory()) == 1
	})

	st.mint(t, buyer.PubKeyHex(), wireIDBuyerMint)
	t.Setenv("AGENT_CF_HOME", buyerHome)

	// Arm the one-shot drop: the FIRST buy EVENT injected will close the client ws.
	hub.armDropOnNextBuy()

	cmd := newBuyCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	setBuyFlags(t, cmd, map[string]string{
		"task":    ed2cBuyTask,
		"budget":  "1000000",
		"relay":   hub.wsURL(),
		"timeout": "25s",
	})
	if err := runBuy(cmd, nil); err != nil {
		t.Fatalf("runBuy did not recover from the mid-await conn drop: %v\nstderr:\n%s", err, stderr.String())
	}

	// The client re-dialed (a fresh serveWS connection) — the drop actually happened
	// and the recovery is not vacuous.
	if n := hub.connectionsServed(); n < 2 {
		t.Fatalf("hub served %d connections, want >= 2 (the mid-await drop must have forced a client re-dial)", n)
	}
	// The match was recovered and the sale settled: content byte-exact + scrip moved.
	if !bytes.Equal(stdout.Bytes(), ed2cContent) {
		t.Fatalf("post-recovery content mismatch.\n got (%d bytes): %q\nwant (%d bytes): %q",
			stdout.Len(), stdout.String(), len(ed2cContent), string(ed2cContent))
	}
	waitFor(t, 8*time.Second, "buyer debited a real price+fee hold after recovery", func() bool {
		return st.scrip.Balance(buyer.PubKeyHex()) < wireIDBuyerMint
	})
	matchStore := st.matchStoreID(t)
	waitFor(t, 8*time.Second, "match settles after conn-drop recovery", func() bool {
		return st.eng.State().IsMatchSettled(matchStore)
	})
	waitFor(t, 8*time.Second, "seller credited the residual after recovery", func() bool {
		return st.scrip.Balance(seller.PubKeyHex()) > 0
	})
}

// --- (6) --preview client-RunE path delivers byte-exact + moves scrip ----------

// TestE2E_PreviewFlag_SettlesContentByteExact_ClientRunE drives the ACTUAL
// `dontguess buy --preview` RunE on a minted buyer against the full serve stack
// and proves the PREVIEW branch settles END TO END: buy → match → preview-request
// → FREE preview → buyer-accept (e-tagging the PREVIEW wire id) → operator
// auto-deliver → complete, ending with content IN HAND byte-exact and REAL scrip
// moved. This brings the --preview delivery path (matrix item "delivers content
// byte-exact … AND --preview path") under the named cache-immune TestE2E gate.
// (The relayclient-level SettleResult decomposition is proven separately by
// TestEd2C_RunBuy_PreviewFlag_SettlesContentAndMovesScrip.)
func TestE2E_PreviewFlag_SettlesContentByteExact_ClientRunE(t *testing.T) {
	hushRelayLogs(t)
	dir := t.TempDir()
	ls, err := dgstore.Open(dir + "/events.jsonl")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = ls.Close() })

	operator, _ := identity.Generate()
	seller, _ := identity.Generate()
	buyer, buyerHome := newAgentIdentity(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	// Buyer allowlisted: the preview-request/buyer-accept/complete phases are all
	// TrustAllowlisted fleet-member settle phases (trust.go).
	st := newE2EStack(t, ctx, ls, operator, dir+"/events.jsonl.pubcursor",
		e2eShortPublishInterval, seller.PubKeyHex(), buyer.PubKeyHex())
	t.Cleanup(func() { cancel(); st.stop() })
	hub := newE2EHub(t, st.conn)

	putEv := signExchangeEvent(t, seller,
		[]string{exchange.TagPut, "exchange:content-type:code", "exchange:domain:go"}, nil,
		knownV2PutPayload(t, seller, operator.PubKeyHex(), ed2cPutDesc, ed2cContent, ed2cTokenCost))
	st.conn.inject(putEv)
	waitFor(t, 8*time.Second, "seller put auto-accepts into inventory", func() bool {
		return len(st.eng.State().Inventory()) == 1
	})

	st.mint(t, buyer.PubKeyHex(), wireIDBuyerMint)
	t.Setenv("AGENT_CF_HOME", buyerHome)

	cmd := newBuyCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	setBuyFlags(t, cmd, map[string]string{
		"task":    ed2cBuyTask,
		"budget":  "1000000",
		"relay":   hub.wsURL(),
		"timeout": "20s",
		"preview": "true",
	})
	if err := runBuy(cmd, nil); err != nil {
		t.Fatalf("runBuy --preview returned error: %v\nstderr:\n%s", err, stderr.String())
	}

	// (a) content IN HAND, byte-exact, reached THROUGH the preview branch.
	if !bytes.Equal(stdout.Bytes(), ed2cContent) {
		t.Fatalf("delivered content mismatch via --preview.\n got (%d bytes): %q\nwant (%d bytes): %q",
			stdout.Len(), stdout.String(), len(ed2cContent), string(ed2cContent))
	}
	if !strings.Contains(stderr.String(), "SETTLED") {
		t.Fatalf("stderr does not surface the SETTLED outcome via --preview:\n%s", stderr.String())
	}

	// (b) REAL scrip moved through the preview branch.
	waitFor(t, 8*time.Second, "buyer debited a real price+fee hold via --preview", func() bool {
		return st.scrip.Balance(buyer.PubKeyHex()) < wireIDBuyerMint
	})
	matchStore := st.matchStoreID(t)
	waitFor(t, 8*time.Second, "match settles on the --preview complete", func() bool {
		return st.eng.State().IsMatchSettled(matchStore)
	})
	waitFor(t, 8*time.Second, "seller credited the residual via --preview", func() bool {
		return st.scrip.Balance(seller.PubKeyHex()) > 0
	})
}
