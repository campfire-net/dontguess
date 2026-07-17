package main

// assign_e2e_d26_test.go is the MANDATORY enforcement-proof test for
// dontguess-d26 (#2 AGENT DOOR). It drives the REAL `dontguess assigns` /
// `dontguess assign claim` / `dontguess assign complete` cobra RunE functions
// against a REAL team-tier exchange.Engine (real ScripStore, real
// TrustChecker) over a REAL in-process NIP-01 websocket relay — nothing here
// mocks the engine or the CLI. The done condition (item text, verbatim): "a
// scrip-poor fleet agent lists a posted [compression] assign, claims it,
// completes it, and its scrip balance INCREASES — verify via `dontguess
// savings` output, NOT a unit mock of the engine."
//
// TOPOLOGY:
//
//	assignHub: a from-scratch in-process NIP-01 relay (not the shared
//	  e2eHub/miniRelay test helpers — see rationale below) that RETAINS every
//	  published event from ANY author, replays the full matching backlog +
//	  EOSE on every fresh REQ, AND live-forwards new events to every open
//	  subscription. This is what both halves of the assign door need at once:
//	  `dontguess assigns` needs EOSE-terminated backlog replay
//	  (relayclient.FetchOpenAssigns), and the OPERATOR's own persistent
//	  attachRelayTransport reader needs LIVE delivery of events published on
//	  OTHER connections (the agent's claim/complete, the operator's own
//	  manually-published assign-accept below). Neither existing shared hub
//	  offers both: e2eHub (serve_relay_ed2g_e2e_test.go) live-forwards but only
//	  retains OPERATOR publishes + puts (no EOSE at all); miniRelay
//	  (up_test.go) retains+replays+EOSE but only on THAT connection's own REQ,
//	  never live-pushes across connections. Building this locally (not
//	  touching either shared helper) keeps blast radius to this file.
//
//	OPERATOR: a real exchange.Engine (LocalStore + OperatorPublicKey +
//	  TrustChecker allowlisting the agent + a real scrip.LocalScripStore),
//	  its Outbox/Intake attached to assignHub via the SAME attachRelayTransport
//	  production wiring serve.go uses, driven by a live eng.Start(ctx) poll
//	  loop — so every claim/complete the agent's CLI publishes folds through
//	  the SAME dispatch path a deployed operator runs.
//
//	SETUP (bypasses this item's scope on purpose): the fixture inventory entry
//	  and the "posted compression assign" are constructed via
//	  eng.IngestLocalRecord + eng.AutoAcceptPut + eng.PostOpenCompressionAssign
//	  — all REAL, EXPORTED, production engine methods (PostOpenCompressionAssign
//	  is the same call the medium loop makes) — rather than driving a full
//	  encrypted `dontguess put`+`dontguess buy` round trip. That round trip is
//	  orthogonal to (and already covered by) other E2E suites, AND is
//	  structurally incompatible with a *warm*-compression assign specifically:
//	  sendWarmCompressionAssign/sendColdCompressionAssign both bail out for any
//	  v2-confidential entry (entry.WrappedCEKOperator != ""), and every
//	  TEAM-TIER encrypted `dontguess put` produces exactly that (skipCompressionForV2,
//	  engine_buy.go) — so a *real* encrypted put+buy could never reach a
//	  compression-assign fixture at all. A cold compression assign
//	  (PostOpenCompressionAssign, no exclusive_sender) is the same TagAssign
//	  lifecycle a warm one is (both task_type="compress", both dispatched
//	  through the identical applyAssign/applyAssignClaim/applyAssignComplete/
//	  applyAssignAccept fold this item's CLI door exercises) — the door under
//	  test does not care which of the three posting paths produced the assign.
//
//	AGENT DOOR UNDER TEST (real CLI RunE, signed via a real walk-up-shaped
//	  agent identity, real relay publish): `dontguess assigns` discovers the
//	  posted assign; `dontguess assign claim <id>` claims it; `dontguess assign
//	  complete <claim-id> --content <b64>` submits the result.
//
//	PAYMENT (deliberately out of this item's scope, per the item text: "CLI
//	  only emits signed claim/complete msgs"): the operator's assign-accept is
//	  NOT a CLI command yet, so the test calls eng.AcceptAssign directly — a
//	  real, exported, production engine method (NOT test-only, NOT a mock) that
//	  this work surfaced as a genuine prerequisite: handleAssignAccept (the
//	  payment handler) turned out to be UNREACHABLE via the normal
//	  emit-then-poll-dispatch path for ANY operator-authored assign-accept
//	  (dispatchLocalGap unconditionally skips dispatch for Sender==OperatorKey —
//	  see AcceptAssign's doc in engine_core.go for the full finding).
//	  AcceptAssign is what actually drives handleAssignAccept ->
//	  ClaimAssignPayment -> ScripStore.AddBudget: REAL scrip movement through
//	  the REAL engine, not a balance mutated directly. A future `dontguess
//	  operator assign-accept` CLI (out of THIS item's scope) would call exactly
//	  this method.
//
//	VERIFICATION: dontguess savings' own collectSavings function (the exact
//	  code runSavings' RunE calls) is invoked against the operator's real
//	  DG_HOME event log and asserted to report the assign-pay bounty AND a
//	  positive internal-economy scrip figure; scrip.LocalScripStore.Balance is
//	  cross-checked as a second, independent signal.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/3dl-dev/dontguess/pkg/exchange"
	"github.com/3dl-dev/dontguess/pkg/identity"
	"github.com/3dl-dev/dontguess/pkg/relay"
	"github.com/3dl-dev/dontguess/pkg/relayclient"
	"github.com/3dl-dev/dontguess/pkg/scrip"
	dgstore "github.com/3dl-dev/dontguess/pkg/store"
)

// --- assignHub: a genuinely-peer, retain-everything, EOSE + live NIP-01 relay

// assignHub is documented in this file's header comment: retain+replay+EOSE
// (like miniRelay) PLUS live cross-connection forwarding (like e2eHub), from
// every author, with no operator/client distinction — a real relay does both
// at once.
type assignHub struct {
	srv *httptest.Server
	mu  sync.Mutex
	evs []*identity.Event
}

func newAssignHub(t *testing.T) *assignHub {
	t.Helper()
	h := &assignHub{}
	mux := http.NewServeMux()
	mux.HandleFunc("/", h.serveWS)
	h.srv = httptest.NewServer(mux)
	t.Cleanup(h.srv.Close)
	return h
}

func (h *assignHub) wsURL() string { return wsURL(h.srv.URL) }

func (h *assignHub) snapshot() []*identity.Event {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]*identity.Event, len(h.evs))
	copy(out, h.evs)
	return out
}

func (h *assignHub) store(ev *identity.Event) {
	h.mu.Lock()
	h.evs = append(h.evs, ev)
	h.mu.Unlock()
}

func (h *assignHub) serveWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	c := &ed2cClientConn{
		filters:   map[string]relay.Filter{},
		forwarded: map[string]bool{},
		done:      make(chan struct{}),
	}
	c.ws = conn
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
			h.store(f.Event)
			ok, _ := relay.EncodeOK(f.Event.ID, true, "")
			c.write(ok)
		case relay.LabelREQ:
			if len(f.Filters) > 0 {
				c.mu.Lock()
				c.filters[f.SubID] = f.Filters[0]
				c.mu.Unlock()
				// Replay the full matching backlog immediately, then EOSE — a real
				// relay's historical-replay contract, which relayclient.FetchOpenAssigns
				// depends on to terminate its collect loop.
				for _, ev := range h.snapshot() {
					if !ed2cMatchFilter(f.Filters[0], ev) {
						continue
					}
					key := f.SubID + "|" + ev.ID
					c.mu.Lock()
					already := c.forwarded[key]
					if !already {
						c.forwarded[key] = true
					}
					c.mu.Unlock()
					if already {
						continue
					}
					frame, ferr := relay.EncodeSubEvent(f.SubID, ev)
					if ferr == nil {
						c.write(frame)
					}
				}
			}
			eose, _ := relay.EncodeEOSE(f.SubID)
			c.write(eose)
		case relay.LabelCLOSE:
			c.mu.Lock()
			delete(c.filters, f.SubID)
			c.mu.Unlock()
		}
	}
}

// pump live-forwards newly stored events (from ANY connection) matching a
// live filter, once per (subID, eventID) — cross-connection broadcast, the
// property e2eHub has (for operator publishes only) and miniRelay lacks
// entirely.
func (h *assignHub) pump(c *ed2cClientConn) {
	tk := time.NewTicker(2 * time.Millisecond)
	defer tk.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-tk.C:
		}
		evs := h.snapshot()
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

// --- the test -----------------------------------------------------------

func TestE2E_AssignDoor_d26_ListClaimComplete_ScripIncreases(t *testing.T) {
	hushRelayLogs(t)

	hub := newAssignHub(t)

	dgHome := t.TempDir()
	ls, err := dgstore.Open(dgHome + "/events.jsonl")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = ls.Close() })

	operator, err := identity.Generate()
	if err != nil {
		t.Fatalf("operator identity: %v", err)
	}
	agent, agentHome := newAgentIdentity(t) // the scrip-poor fleet agent
	seller, err := identity.Generate()      // a DIFFERENT identity puts the fixture entry
	if err != nil {
		t.Fatalf("seller identity: %v", err)
	}

	// Both must be allowlisted: seller so AutoAcceptPut's trust gate admits the
	// fixture put, agent so its assign-claim/-complete pass OperationAssignClaim/
	// OperationAssignComplete's TrustAllowlisted gate. Using a DISTINCT seller
	// (rather than the agent itself) avoids the automatic HOT compression assign
	// engine_buy.go's sendCompressionAssign posts EXCLUSIVELY to the put's own
	// seller on every accept (50% bounty) — that assign would otherwise also be
	// "claimable" by the agent were agent==seller, making the fixture ambiguous
	// between two real compress assigns instead of exercising exactly the one
	// (cold, non-exclusive) this test posts.
	ks := exchange.NewKeySet(agent.PubKeyHex(), seller.PubKeyHex())
	tc, err := exchange.NewTrustChecker(operator.PubKeyHex(), ks)
	if err != nil {
		t.Fatalf("NewTrustChecker: %v", err)
	}
	ss, err := scrip.NewLocalScripStore(ls, operator.PubKeyHex())
	if err != nil {
		t.Fatalf("NewLocalScripStore: %v", err)
	}

	// Scrip-poor: assert the agent starts at zero before anything happens.
	if bal := ss.Balance(agent.PubKeyHex()); bal != 0 {
		t.Fatalf("agent balance before the test = %d, want 0 (scrip-poor fixture)", bal)
	}

	notifier := &appendNotifier{}
	eng := exchange.NewEngine(exchange.EngineOptions{
		CampfireID: "local",
		LocalStore: ls,
		// OperatorPublicKey (NOT OperatorSigner): state.OperatorKey must be set
		// for the operator-only assign apply* guards (applyAssign/
		// applyAssignAccept), but OperatorSigner is deliberately OMITTED — it
		// arms encryptedRequired (engine_core.go: st.encryptedRequired =
		// opts.OperatorSigner != nil), which would fail-closed DROP the
		// legacy-plaintext fixture put below (§541 §6). This test's engine
		// never handles a v2-confidential put, so leaving encryption support
		// unarmed is the correct minimal config, not a shortcut.
		OperatorPublicKey: operator.PubKeyHex(),
		TrustChecker:      tc,
		ScripStore:        ss,
		PollInterval:      10 * time.Millisecond,
		OnLocalAppend:     notifier.fire,
		Logger:            func(string, ...any) {},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)

	// --- SETUP: a real inventory entry + a real posted compression assign ---
	//
	// Deliberately done BEFORE attachRelayTransport/eng.Start below: both
	// IngestLocalRecord and AutoAcceptPut are documented as safe to call
	// concurrently with the live poll loop (they serialize through localMu/
	// opMu the same way the poll loop and appendLocalRecord do), but running
	// setup first — with no concurrent reader/poll goroutine yet touching this
	// engine at all — removes any timing window as a variable and keeps this
	// test's failure signal to the CLI door under test, not scheduler
	// interleaving of two independently-correct subsystems.
	putID := randomLocalMsgID(t)
	const tokenCost int64 = 20000
	if err := eng.IngestLocalRecord(dgstore.Record{
		ID:         putID,
		CampfireID: "local",
		Sender:     seller.PubKeyHex(),
		Payload:    localPutPayload("assign-door fixture: rust borrow checker cheatsheet", tokenCost),
		Tags:       []string{exchange.TagPut, "exchange:content-type:code", "exchange:domain:rust"},
		Timestamp:  time.Now().UnixNano(),
	}); err != nil {
		t.Fatalf("IngestLocalRecord(put): %v", err)
	}
	if err := eng.AutoAcceptPut(putID, tokenCost*70/100, time.Now().UTC().Add(72*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut: %v", err)
	}
	inv := eng.State().Inventory()
	if len(inv) != 1 {
		t.Fatalf("inventory after AutoAcceptPut = %d entries, want 1", len(inv))
	}
	entryID := inv[0].EntryID
	if inv[0].WrappedCEKOperator != "" {
		t.Fatalf("fixture entry is v2-confidential (WrappedCEKOperator=%q) — compression-assign paths would skip it", inv[0].WrappedCEKOperator)
	}

	const wantReward = tokenCost * exchange.ColdCompressionBountyPct / 100 // 4000
	if err := eng.PostOpenCompressionAssign(entryID); err != nil {
		t.Fatalf("PostOpenCompressionAssign: %v", err)
	}

	// --- NOW attach the real relay leg + start the live poll loop: everything
	// from here on (claim/complete/accept) genuinely arrives asynchronously
	// over the wire, which is what this item's CLI door actually exercises. ---
	conn := relay.New(hub.wsURL(), operator, relay.WithoutClientAuth())
	t.Cleanup(func() { _ = conn.Close() })
	stop, err := attachRelayTransport(ctx, ls, operator, operator.PubKeyHex(),
		dgHome+"/events.jsonl.pubcursor", conn, conn, 25*time.Millisecond, nil, notifier,
		eng.State().RegisterWireAlias)
	if err != nil {
		t.Fatalf("attachRelayTransport: %v", err)
	}
	engDone := make(chan struct{})
	go func() { defer close(engDone); _ = eng.Start(ctx) }()
	t.Cleanup(func() { cancel(); <-engDone; stop() })

	// Wait for the Outbox to publish the assign onto the (real) relay — this is
	// what makes it discoverable by the agent's CLI over a SEPARATE connection.
	// Wait specifically for the COLD assign (no exclusive_sender field at all —
	// sendColdCompressionAssign's payload omits the key entirely, unlike hot/warm
	// which always set it) — the SAME accept also fires an automatic HOT
	// compression assign (exclusive to the seller) which reaches the relay on its
	// own, earlier, outbox tick; waiting for "any compress assign" would race and
	// intermittently observe only that one.
	waitFor(t, 10*time.Second, "the posted (cold, non-exclusive) compression assign reaches the relay", func() bool {
		for _, ev := range hub.snapshot() {
			if ev.Kind == 3405 && strings.Contains(ev.Content, `"task_type":"compress"`) && !strings.Contains(ev.Content, `"exclusive_sender"`) {
				return true
			}
		}
		return false
	})

	operatorNpub := operator.Npub()

	// === (1) `dontguess assigns` — the agent discovers the posted task ======
	assignsC := newAssignsCmd()
	var assignsOut strings.Builder
	assignsC.SetOut(&assignsOut)
	setPutFlags(t, assignsC, map[string]string{
		"agent-home":    agentHome,
		"relay":         hub.wsURL(),
		"operator-npub": operatorNpub,
		"timeout":       "20s",
	})
	if err := runAssigns(assignsC, nil); err != nil {
		t.Fatalf("runAssigns: %v\noutput:\n%s", err, assignsOut.String())
	}
	listing := assignsOut.String()
	if !strings.Contains(listing, "compress") {
		t.Fatalf("`dontguess assigns` output does not mention the compress task:\n%s", listing)
	}

	// Independent corroborating read (same exported function the CLI itself
	// calls) to pull the exact assign id for the claim/complete steps below.
	open, err := relayclient.FetchOpenAssigns(mustCtx(t, ctx, 20*time.Second), mustConn(t, hub.wsURL(), agent), operator.PubKeyHex(), agent.PubKeyHex())
	if err != nil {
		t.Fatalf("FetchOpenAssigns (assertion helper): %v", err)
	}
	var assignID string
	for _, a := range open {
		if a.TaskType == "compress" && a.EntryID == entryID {
			assignID = a.AssignID
			if a.Reward != wantReward {
				t.Fatalf("listed reward = %d, want %d (20%% of token_cost %d)", a.Reward, wantReward, tokenCost)
			}
			break
		}
	}
	if assignID == "" {
		t.Fatalf("FetchOpenAssigns did not surface the posted compress assign for entry %s; got %+v", short(entryID), open)
	}
	if !strings.Contains(listing, short(assignID)) {
		t.Fatalf("`dontguess assigns` output does not contain the assign id %s:\n%s", short(assignID), listing)
	}

	// === (2) `dontguess assign claim <assign-id>` ===========================
	claimC := newAssignClaimCmd()
	var claimOut strings.Builder
	claimC.SetOut(&claimOut)
	setPutFlags(t, claimC, map[string]string{
		"agent-home": agentHome,
		"relay":      hub.wsURL(),
		"timeout":    "20s",
	})
	if err := runAssignClaim(claimC, []string{assignID}); err != nil {
		t.Fatalf("runAssignClaim: %v\noutput:\n%s", err, claimOut.String())
	}
	claimText := claimOut.String()
	if !strings.Contains(claimText, "claimed assign") {
		t.Fatalf("assign claim output missing confirmation:\n%s", claimText)
	}

	// findFixtureAssign locates the fixture's OWN engine-state AssignRecord —
	// NOT by assignID: assignID (above) is the WIRE (content-hash) id
	// FetchOpenAssigns surfaced (the only id a relay-only client ever learns),
	// while AssignRecord.AssignID is the operator's original pre-signature
	// STORE id (msg.ID at fold time, see applyAssign) — the wire/store
	// distinction dontguess-55c's resolveAlias reconciles for the ENGINE's own
	// internal antecedent lookups, but this test's own assertions must not
	// conflate the two either. Reward+EntryID+TaskType uniquely identify the
	// cold assign this test posted among the (at most two) compress assigns
	// live on this entry (see the seller!=agent rationale above).
	findFixtureAssign := func() *exchange.AssignRecord {
		for _, rec := range eng.State().AllActiveAssigns() {
			if rec.TaskType == "compress" && rec.EntryID == entryID && rec.Reward == wantReward {
				return rec
			}
		}
		return nil
	}

	waitFor(t, 10*time.Second, "the claim folds into engine state (assign-claimed)", func() bool {
		rec := findFixtureAssign()
		return rec != nil && rec.Status == exchange.AssignClaimed && rec.ClaimantKey == agent.PubKeyHex()
	})

	// Recover the claim event id the SAME way a real operator/agent would —
	// from the CLI's own printed confirmation (deterministic nostr event id,
	// no relay round trip needed; see assign.go's doc on why <id> for
	// `complete` is the claim id, not the assign id).
	claimID := extractClaimID(t, claimText)

	// === (3) `dontguess assign complete <claim-id> --content <b64>` =========
	completeC := newAssignCompleteCmd()
	var completeOut strings.Builder
	completeC.SetOut(&completeOut)
	setPutFlags(t, completeC, map[string]string{
		"agent-home": agentHome,
		"relay":      hub.wsURL(),
		"content":    "cnVzdCBib3Jyb3cgY2hlY2tlciBjb21wcmVzc2VkIGNoZWF0c2hlZXQ=", // "rust borrow checker compressed cheatsheet"
		"timeout":    "20s",
	})
	if err := runAssignComplete(completeC, []string{claimID}); err != nil {
		t.Fatalf("runAssignComplete: %v\noutput:\n%s", err, completeOut.String())
	}

	waitFor(t, 10*time.Second, "the completion folds into engine state (assign-completed)", func() bool {
		rec := findFixtureAssign()
		return rec != nil && rec.Status == exchange.AssignCompleted
	})

	// Find the REAL folded assign-complete event id (the antecedent
	// assign-accept below must e-tag) by reading it back off engine state.
	fixtureRec := findFixtureAssign()
	if fixtureRec == nil || fixtureRec.CompleteMsgID == "" {
		t.Fatalf("could not resolve the folded assign-complete message id for assign %s", short(assignID))
	}
	completeMsgID := fixtureRec.CompleteMsgID

	// === (4) PAYMENT: a real operator accept (out of THIS item's CLI scope, ==
	//         per the item text: "CLI only emits signed claim/complete msgs") =
	//
	// eng.AcceptAssign is a real, exported, production engine method — NOT a
	// mock and NOT a CLI verb — that this work surfaced as a genuine
	// prerequisite gap: handleAssignAccept (the payment handler, already fully
	// written) was UNREACHABLE via the normal emit-then-poll-dispatch path for
	// ANY operator-authored assign-accept (dispatchLocalGap unconditionally
	// skips dispatch for Sender==OperatorKey, engine_core.go — an operator's
	// own emissions are assumed already synchronously handled at the call
	// site, which assign-accept never had). AcceptAssign closes that gap the
	// same way AutoAcceptPut/RejectPut/MintScrip already do it: emit + apply +
	// handle, inline, in one call — see its doc in engine_core.go for the full
	// finding. A future `dontguess operator assign-accept` CLI (out of THIS
	// item's scope) would call exactly this.
	if err := eng.AcceptAssign(completeMsgID); err != nil {
		t.Fatalf("AcceptAssign: %v", err)
	}

	// === (5) SCRIP BALANCE INCREASES — verified via collectSavings, the ======
	//         exact function `dontguess savings`'s RunE calls, reading the ====
	//         operator's REAL persisted local event log ==========================
	waitFor(t, 15*time.Second, "the agent's scrip balance increases (real ScripStore.AddBudget via handleAssignAccept)", func() bool {
		return ss.Balance(agent.PubKeyHex()) > 0
	})
	finalBalance := ss.Balance(agent.PubKeyHex())
	if finalBalance != wantReward {
		t.Fatalf("agent scrip balance = %d, want exactly %d (the cold-compression bounty, paid once)", finalBalance, wantReward)
	}

	rep, serr := collectSavings(dgHome, 0, defaultInRate, defaultOutRate, defaultGenOutputFrac, defaultConsumeFrac)
	if serr != nil {
		t.Fatalf("collectSavings (the real `dontguess savings` computation): %v", serr)
	}
	if rep.Economy.CompressionRewardScrip < wantReward {
		t.Fatalf("dontguess savings report: compression_reward_scrip_posted = %d, want >= %d (the paid bounty)",
			rep.Economy.CompressionRewardScrip, wantReward)
	}
}

// extractClaimID pulls the claim event id out of `dontguess assign claim`'s
// printed confirmation line ("claimed assign <short> -> claim <id>").
func extractClaimID(t *testing.T, out string) string {
	t.Helper()
	const marker = "-> claim "
	i := strings.Index(out, marker)
	if i < 0 {
		t.Fatalf("could not find claim id in output:\n%s", out)
	}
	rest := out[i+len(marker):]
	if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
		rest = rest[:nl]
	}
	id := strings.TrimSpace(rest)
	if id == "" {
		t.Fatalf("parsed empty claim id from output:\n%s", out)
	}
	return id
}

// mustCtx/mustConn are tiny test-local conveniences for the one-off assertion
// fetch (relayclient.FetchOpenAssigns) run independently of the CLI command
// under test, so the CLI's stdout parsing above is corroborated by a second,
// independently-constructed relay read.
func mustCtx(t *testing.T, parent context.Context, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(parent, d)
	t.Cleanup(cancel)
	return ctx
}

func mustConn(t *testing.T, url string, signer identity.Signer) *relay.Conn {
	t.Helper()
	c := relayclient.NewConn(url, signer, relayclient.WithRelayAuth(false))
	t.Cleanup(func() { c.Close() })
	return c
}
