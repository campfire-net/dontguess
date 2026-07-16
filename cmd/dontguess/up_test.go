package main

// up_test.go — dontguess-75a GROUND-SOURCE (design §1 + §6 + §9 Gate B/P7).
//
// These are package-main tests driving the REAL runUpCore against the REAL
// exchange.Init, the REAL runServeLocalCtx (in-process, since os.Executable()
// resolves to the `go test` binary under `go test` — not `dontguess` — so the
// production spawnDetachedServe path cannot be driven here; a real detached
// subprocess launch is exercised manually/by the CLI, not by this suite), the
// REAL operator IPC socket + OpAllowlist self-admit path, and a REAL in-process
// NIP-01 websocket relay (miniRelay below — nothing stubbed but the relay's
// persistence, which is exactly what "in-process test relay" means).
//
// Covers the item's four mandatory GROUND-SOURCE assertions:
//
//	(a) solo `up` (no relay) boots individual-tier clean + idempotent (run
//	    twice, no duplicated state).
//	(b) `up --relay <in-process-test-relay-url>` persists tier=team + relay,
//	    serves team, self-admits the operator's own key into the roster/KeySet
//	    (assert the operator key is Allowed) — against the IN-PROCESS test
//	    relay, not a deployed writePolicy.
//	(c) `up --team` with no relay => the daa fail-closed error, bounded, no hang.
//	(d) SECOND-MACHINE refuse-mint: an existing operator's events already on
//	    the relay + NO local operator private key => `up --relay` detects and
//	    REFUSES to mint a competing sequencer (ADV-4); no operator key or
//	    config is ever written.

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/3dl-dev/dontguess/pkg/bootservice"
	"github.com/3dl-dev/dontguess/pkg/exchange"
	"github.com/3dl-dev/dontguess/pkg/identity"
	"github.com/3dl-dev/dontguess/pkg/nostr"
	"github.com/3dl-dev/dontguess/pkg/relay"
	"github.com/gorilla/websocket"
)

// --- miniRelay: a real, minimal in-process NIP-01 websocket relay -----------
//
// Unlike put_test.go's newFakeRelayServer (which never answers a bare REQ —
// it only injects a canned reject event), miniRelay is a faithful enough
// relay for `up`'s needs: it ACKs every published EVENT with OK, retains it,
// and answers every REQ by replaying stored events matching the filter
// (reusing the production relay.ParseFrame/EncodeReq codec and the existing
// package-test ed2cMatchFilter) followed by EOSE — so both a genuinely empty
// relay (fresh bootstrap) and a pre-seeded relay (ADV-4 fixture) behave like
// a real relay would.
type miniRelay struct {
	srv *httptest.Server
	mu  sync.Mutex
	evs []*identity.Event
	// failFirstN rejects the first N websocket handshakes with 503 (dontguess-e39
	// gap 2 fixture): a genuinely transiently-unreachable relay — each rejected
	// upgrade surfaces to probeExistingOperatorEvents as a real connect error, so
	// the retry loop is exercised end-to-end against a REAL relay, not a stub.
	failFirstN int
	upgrades   int
}

func newMiniRelay(t *testing.T) *miniRelay {
	t.Helper()
	r := &miniRelay{}
	mux := http.NewServeMux()
	mux.HandleFunc("/", r.serveWS)
	r.srv = httptest.NewServer(mux)
	t.Cleanup(r.srv.Close)
	return r
}

func (r *miniRelay) wsURL() string { return wsURL(r.srv.URL) }

// seed pre-populates the relay with an event as if some other client had
// already published it — the ADV-4 fixture ("an existing operator's events
// already on the relay").
func (r *miniRelay) seed(ev *identity.Event) {
	r.mu.Lock()
	r.evs = append(r.evs, ev)
	r.mu.Unlock()
}

func (r *miniRelay) serveWS(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	r.upgrades++
	reject := r.upgrades <= r.failFirstN
	r.mu.Unlock()
	if reject {
		http.Error(w, "relay warming up", http.StatusServiceUnavailable)
		return
	}
	conn, err := upgrader.Upgrade(w, req, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	var writeMu sync.Mutex
	write := func(frame []byte) {
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = conn.WriteMessage(websocket.TextMessage, frame)
	}
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
			r.mu.Lock()
			r.evs = append(r.evs, f.Event)
			r.mu.Unlock()
			ok, _ := relay.EncodeOK(f.Event.ID, true, "")
			write(ok)
		case relay.LabelREQ:
			r.mu.Lock()
			snapshot := append([]*identity.Event(nil), r.evs...)
			r.mu.Unlock()
			for _, filt := range f.Filters {
				for _, ev := range snapshot {
					if ed2cMatchFilter(filt, ev) {
						frame, ferr := relay.EncodeSubEvent(f.SubID, ev)
						if ferr == nil {
							write(frame)
						}
					}
				}
			}
			eose, _ := relay.EncodeEOSE(f.SubID)
			write(eose)
		case relay.LabelCLOSE:
			// no-op: this relay does not track live per-sub state beyond REQ replay.
		}
	}
}

// --- test seams: in-process serve launch + a no-op boot-service stub --------

// withInProcessServeLauncher overrides upServeLauncher to drive the REAL
// runServeLocalCtx in a goroutine instead of spawning a real OS subprocess —
// os.Executable() under `go test` resolves to the test binary, not
// `dontguess`, so the production spawnDetachedServe path is not drivable
// from this suite. Owns its own cancelable context so ordering is correct by
// construction: t.Cleanup registers cancel-THEN-wait as ONE func (mirroring
// serve.go's own documented dontguess-e35 shutdown order) — Go's testing
// package runs Cleanup funcs LIFO, so a cancel and a wait registered as TWO
// separate Cleanup calls would wait-before-cancel and deadlock.
func withInProcessServeLauncher(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	orig := upServeLauncher
	upServeLauncher = func(dgHome string) (bool, error) {
		if _, ok := dialSocketMaybe(dgHome); ok {
			return true, nil
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			_ = runServeLocalCtx(ctx, dgHome)
		}()
		t.Cleanup(func() { cancel(); <-done })
		if !waitForOperatorSocket(dgHome, 10*time.Second) {
			return false, fmt.Errorf("test serve did not become reachable")
		}
		return false, nil
	}
	t.Cleanup(func() { upServeLauncher = orig })
}

// withStubBootService overrides upInstallBootService with a no-op recorder —
// pkg/bootservice.Install's own OS integration (systemd --user unit write /
// enable / linger) is already ground-source-tested by that package's own
// suite (dontguess-748/aa4); re-driving real systemctl calls (or writing into
// the real invoking user's ~/.config/systemd/user) here would pollute the
// test host and re-prove code this item does not own. What THIS item owns —
// that `up` CALLS Install with the right ServeBinary/DGHome/RelayURLs exactly
// once on the team path — is what the recorder proves.
func withStubBootService(t *testing.T) *[]bootservice.Options {
	t.Helper()
	orig := upInstallBootService
	var calls []bootservice.Options
	upInstallBootService = func(opts bootservice.Options) (*bootservice.Result, error) {
		calls = append(calls, opts)
		return &bootservice.Result{UnitPath: "test-stub-unit", DryRun: true, DryRunNote: "test stub — real systemctl not exercised"}, nil
	}
	t.Cleanup(func() { upInstallBootService = orig })
	return &calls
}

// --- (a) solo `up`: clean individual-tier boot, idempotent re-run -----------

func TestUp_Solo_IdempotentBoot_NoDuplicatedState(t *testing.T) {
	dgHome := t.TempDir()
	t.Setenv("DONTGUESS_RELAY_URLS", "")
	t.Setenv("DONTGUESS_RELAY_URL", "")

	withInProcessServeLauncher(t)

	var out1, out2 bytes.Buffer
	if err := runUpCore(dgHome, nil, "", false, &out1); err != nil {
		t.Fatalf("first up (solo): %v", err)
	}

	cfg1, err := exchange.LoadConfig(dgHome)
	if err != nil {
		t.Fatalf("LoadConfig after first up: %v", err)
	}
	if cfg1.Tier != "" && cfg1.Tier != exchange.TierSolo {
		t.Errorf("solo up persisted tier = %q, want empty/solo", cfg1.Tier)
	}
	if len(cfg1.RelayURLs) != 0 {
		t.Errorf("solo up persisted relay_urls = %v, want none", cfg1.RelayURLs)
	}
	if cfg1.OperatorKeyHex == "" {
		t.Fatalf("solo up did not mint an operator identity")
	}
	if _, ok := dialSocketMaybe(dgHome); !ok {
		t.Fatalf("solo up: operator socket not reachable after boot")
	}

	// Re-run: must be idempotent — same operator key, same CreatedAt (no
	// duplicated state), and the socket is detected as already-running rather
	// than a second serve being spawned.
	if err := runUpCore(dgHome, nil, "", false, &out2); err != nil {
		t.Fatalf("second up (solo, idempotent re-run): %v", err)
	}
	cfg2, err := exchange.LoadConfig(dgHome)
	if err != nil {
		t.Fatalf("LoadConfig after second up: %v", err)
	}
	if cfg2.OperatorKeyHex != cfg1.OperatorKeyHex {
		t.Errorf("re-run minted a DIFFERENT operator key: %s vs %s", cfg2.OperatorKeyHex, cfg1.OperatorKeyHex)
	}
	if cfg2.CreatedAt != cfg1.CreatedAt {
		t.Errorf("re-run changed CreatedAt: %d vs %d — config was rewritten, not idempotently preserved", cfg2.CreatedAt, cfg1.CreatedAt)
	}
	if !strings.Contains(out2.String(), "already running") {
		t.Errorf("second up output did not report the idempotent already-running path:\n%s", out2.String())
	}
}

// --- (b) `up --relay`: team boot + self-admit against an IN-PROCESS relay ---

func TestUp_Relay_TeamBoot_SelfAdmitsOperatorKey(t *testing.T) {
	dgHome := t.TempDir()
	t.Setenv("DONTGUESS_RELAY_URLS", "")
	t.Setenv("DONTGUESS_RELAY_URL", "")

	relayServer := newMiniRelay(t) // fresh + empty: the legitimate first-bootstrap case

	withInProcessServeLauncher(t)
	calls := withStubBootService(t)

	var out bytes.Buffer
	if err := runUpCore(dgHome, []string{relayServer.wsURL()}, "", false, &out); err != nil {
		t.Fatalf("up --relay: %v", err)
	}

	cfg, err := exchange.LoadConfig(dgHome)
	if err != nil {
		t.Fatalf("LoadConfig after up --relay: %v", err)
	}
	if cfg.Tier != exchange.TierTeam {
		t.Fatalf("up --relay persisted tier = %q, want %q (daa config-time persist)", cfg.Tier, exchange.TierTeam)
	}
	if len(cfg.RelayURLs) != 1 || cfg.RelayURLs[0] != relayServer.wsURL() {
		t.Fatalf("up --relay persisted relay_urls = %v, want [%s]", cfg.RelayURLs, relayServer.wsURL())
	}

	// GROUND-SOURCE (b): the operator's own key is Allowed — reconstruct the
	// SAME production KeySet type from the persisted fleet allowlist (exactly
	// what serve.go's team-tier wiring does at startup) and assert membership.
	allow, aerr := identity.NewAllowlist(cfg.FleetAllowlist...)
	if aerr != nil {
		t.Fatalf("reconstruct allowlist from persisted config: %v", aerr)
	}
	ks := exchange.NewKeySet(allow.HexKeys()...)
	if !ks.Allowed(cfg.OperatorKeyHex) {
		t.Fatalf("operator key %s is NOT Allowed in the self-admitted roster/KeySet — self-admit did not take effect", cfg.OperatorKeyHex)
	}
	if !strings.Contains(out.String(), "self-admitted") {
		t.Errorf("up --relay output did not report the self-admit step:\n%s", out.String())
	}

	// The boot-service composition seam was invoked exactly once, with the
	// persisted DGHome/RelayURLs threaded through (pkg/bootservice's own
	// suite proves what Install DOES with them).
	if len(*calls) != 1 {
		t.Fatalf("upInstallBootService called %d times, want 1", len(*calls))
	}
	if (*calls)[0].DGHome != dgHome {
		t.Errorf("boot service DGHome = %q, want %q", (*calls)[0].DGHome, dgHome)
	}
	if len((*calls)[0].RelayURLs) != 1 || (*calls)[0].RelayURLs[0] != relayServer.wsURL() {
		t.Errorf("boot service RelayURLs = %v, want [%s]", (*calls)[0].RelayURLs, relayServer.wsURL())
	}
}

// --- (c) `up --team` with no relay anywhere: the daa fail-closed error, bounded --

func TestUp_ExplicitTeamNoRelay_FailsClosedBounded(t *testing.T) {
	dgHome := t.TempDir()
	t.Setenv("DONTGUESS_RELAY_URLS", "")
	t.Setenv("DONTGUESS_RELAY_URL", "")

	// Spy launcher: if `up` ever reached serve-launch on this path, that is
	// itself a defect (the fail-closed guard must trip inside exchange.Init,
	// before anything is started).
	orig := upServeLauncher
	upServeLauncher = func(string) (bool, error) {
		t.Fatalf("upServeLauncher was called — up --team with no relay must fail BEFORE starting serve")
		return false, nil
	}
	t.Cleanup(func() { upServeLauncher = orig })

	errCh := make(chan error, 1)
	go func() {
		var out bytes.Buffer
		errCh <- runUpCore(dgHome, nil, exchange.TierTeam, false, &out)
	}()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatalf("up --team with no relay anywhere returned nil error, want the daa fail-closed error")
		}
		if !strings.Contains(err.Error(), "requires at least one relay") {
			t.Errorf("error does not surface the daa fail-closed guard: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("up --team with no relay HUNG for >3s — must fail immediately, no hang")
	}

	if _, cerr := exchange.LoadConfig(dgHome); cerr == nil {
		t.Errorf("up --team fail-closed path must not persist a config")
	}
}

// --- (d) SECOND MACHINE, no operator key: refuse to mint (ADV-4) ------------

func TestUp_Relay_SecondMachineNoOperatorKey_RefusesMint_ADV4(t *testing.T) {
	dgHome := t.TempDir()
	t.Setenv("DONTGUESS_RELAY_URLS", "")
	t.Setenv("DONTGUESS_RELAY_URL", "")

	relayServer := newMiniRelay(t)

	// Seed the relay with a genuinely signed event from a DIFFERENT identity —
	// "an existing operator's events on the relay" — BEFORE this fresh,
	// keyless machine ever calls `up --relay`.
	existingOperator, gerr := identity.Generate()
	if gerr != nil {
		t.Fatalf("generate existing-operator identity: %v", gerr)
	}
	seedEv := &identity.Event{CreatedAt: time.Now().Unix(), Kind: nostr.KindPut, Tags: [][]string{}, Content: ""}
	if serr := identity.SignEvent(existingOperator, seedEv); serr != nil {
		t.Fatalf("sign seed event: %v", serr)
	}
	relayServer.seed(seedEv)

	// Spy launcher: refusing to mint must happen BEFORE serve is ever started.
	orig := upServeLauncher
	upServeLauncher = func(string) (bool, error) {
		t.Fatalf("upServeLauncher was called — ADV-4 refuse-mint must fire before serve starts")
		return false, nil
	}
	t.Cleanup(func() { upServeLauncher = orig })

	var out bytes.Buffer
	err := runUpCore(dgHome, []string{relayServer.wsURL()}, "", false, &out)
	if err == nil {
		t.Fatalf("up --relay on a keyless second machine against a non-empty relay returned nil error, want ADV-4 refusal")
	}
	if !strings.Contains(err.Error(), "refusing to mint") {
		t.Errorf("error does not surface the ADV-4 refuse-mint guard: %v", err)
	}

	// No operator key and no config were ever written — the refusal happens
	// strictly before exchange.Init.
	if _, statErr := os.Stat(filepath.Join(dgHome, "nostr-operator.key")); statErr == nil {
		t.Errorf("ADV-4 refusal minted an operator key anyway at %s", filepath.Join(dgHome, "nostr-operator.key"))
	}
	if _, cerr := exchange.LoadConfig(dgHome); cerr == nil {
		t.Errorf("ADV-4 refusal persisted a config anyway")
	}
}

// --- dontguess-e39 GROUND-SOURCE: ADV-4 probe gap fixes ---------------------

// seedForeignOperatorEvent publishes a genuinely-signed event from a DIFFERENT
// identity to the relay — "an existing operator's events already on the relay"
// (the ADV-4 fixture) — so a keyless machine's probe reads a real EVENT.
func seedForeignOperatorEvent(t *testing.T, r *miniRelay) {
	t.Helper()
	op, err := identity.Generate()
	if err != nil {
		t.Fatalf("generate existing-operator identity: %v", err)
	}
	ev := &identity.Event{CreatedAt: time.Now().Unix(), Kind: nostr.KindPut, Tags: [][]string{}, Content: ""}
	if serr := identity.SignEvent(op, ev); serr != nil {
		t.Fatalf("sign seed event: %v", serr)
	}
	r.seed(ev)
}

// shrinkProbeTiming makes the ADV-4 retry loop fast + deterministic for tests
// (real defaults: 3 retries, 500ms backoff, 8s per-attempt timeout).
func shrinkProbeTiming(t *testing.T) {
	t.Helper()
	origR, origB, origT := upProbeRetries, upProbeRetryBackoff, upProbeTimeout
	upProbeRetries = 2
	upProbeRetryBackoff = 1 * time.Millisecond
	upProbeTimeout = 2 * time.Second
	t.Cleanup(func() {
		upProbeRetries, upProbeRetryBackoff, upProbeTimeout = origR, origB, origT
	})
}

// (gap 1) PERSISTED-CONFIG PATH — silent fork on key-loss.
//
// A machine whose operator key file was lost/deleted but whose exchange config
// STILL persists an old team+relay carrying ANOTHER operator's events, run as
// plain `dontguess up` (NO --relay flag, NO env), MUST probe the persisted
// relay and REFUSE to mint — the pre-fix code skipped the probe entirely
// (declaredRelays empty) and let exchange.Init silently mint a fresh operator
// key, forking the sequencer identity the fleet already trusts.
func TestUp_PersistedConfigPath_KeyLost_RefusesMint_ADV4(t *testing.T) {
	dgHome := t.TempDir()
	t.Setenv("DONTGUESS_RELAY_URLS", "")
	t.Setenv("DONTGUESS_RELAY_URL", "")
	shrinkProbeTiming(t)

	relayServer := newMiniRelay(t)

	// Persist a real team+relay config (+ operator key) as if this machine had
	// been a healthy team operator previously — exchange.Init does NOT probe,
	// it only persists, so this legitimately stands in for the prior state.
	if _, err := exchange.Init(exchange.InitOptions{
		DGHome:    dgHome,
		RelayURLs: []string{relayServer.wsURL()},
		Tier:      exchange.TierTeam,
	}); err != nil {
		t.Fatalf("seed persisted team config: %v", err)
	}
	keyPath := filepath.Join(dgHome, "nostr-operator.key")
	if _, statErr := os.Stat(keyPath); statErr != nil {
		t.Fatalf("precondition: operator key was not minted by Init: %v", statErr)
	}

	// The disk incident: the operator key file is lost. The config still
	// persists team + relay.
	if rmErr := os.Remove(keyPath); rmErr != nil {
		t.Fatalf("simulate key-loss: %v", rmErr)
	}
	// The relay meanwhile carries ANOTHER operator's events.
	seedForeignOperatorEvent(t, relayServer)

	// Refusing to mint must happen BEFORE serve is ever started.
	orig := upServeLauncher
	upServeLauncher = func(string) (bool, error) {
		t.Fatalf("upServeLauncher was called — ADV-4 refuse-mint must fire before serve on the persisted-config path")
		return false, nil
	}
	t.Cleanup(func() { upServeLauncher = orig })

	// Plain `up`: NO flags, NO env — the pre-fix skip path.
	var out bytes.Buffer
	err := runUpCore(dgHome, nil, "", false, &out)
	if err == nil {
		t.Fatalf("plain `up` on a key-lost machine whose persisted relay carries another operator returned nil error, want ADV-4 refusal")
	}
	if !strings.Contains(err.Error(), "refusing to mint") {
		t.Errorf("error does not surface the ADV-4 refuse-mint guard: %v", err)
	}
	// A brand-new operator key must NOT have been minted to replace the lost one.
	if _, statErr := os.Stat(keyPath); statErr == nil {
		t.Errorf("ADV-4 refusal minted a NEW operator key anyway at %s — the sequencer would have forked", keyPath)
	}
}

// (gap 2) FAIL-OPEN ON UNREACHABLE RELAY — retry, then refuse.
//
// `up --relay <url>` where the relay is briefly UNREACHABLE on the first
// probe(s) then becomes reachable revealing ANOTHER operator's events MUST
// retry and ultimately REFUSE — the pre-fix code treated the first connect
// error as "cannot determine, proceed" and minted a competing operator.
func TestUp_Relay_TransientlyUnreachableThenAnotherOperator_RetriesThenRefuses_ADV4(t *testing.T) {
	dgHome := t.TempDir()
	t.Setenv("DONTGUESS_RELAY_URLS", "")
	t.Setenv("DONTGUESS_RELAY_URL", "")
	shrinkProbeTiming(t)

	relayServer := newMiniRelay(t)
	relayServer.failFirstN = 2 // first two handshakes 503; retries+1=3 total → 3rd connects
	seedForeignOperatorEvent(t, relayServer)

	orig := upServeLauncher
	upServeLauncher = func(string) (bool, error) {
		t.Fatalf("upServeLauncher was called — ADV-4 refuse-mint must fire before serve on the unreachable-relay path")
		return false, nil
	}
	t.Cleanup(func() { upServeLauncher = orig })

	var out bytes.Buffer
	err := runUpCore(dgHome, []string{relayServer.wsURL()}, "", false, &out)
	if err == nil {
		t.Fatalf("up --relay against a transiently-unreachable relay carrying another operator returned nil, want ADV-4 refusal")
	}
	if !strings.Contains(err.Error(), "refusing to mint") {
		t.Errorf("error does not surface the ADV-4 refuse-mint guard after retry: %v", err)
	}
	// It must actually have RETRIED past the transient failures (>=3 handshake
	// attempts: two rejected + one that connected and read the event).
	relayServer.mu.Lock()
	attempts := relayServer.upgrades
	relayServer.mu.Unlock()
	if attempts < 3 {
		t.Errorf("probe made only %d handshake attempts — it did not retry through the transient unreachability", attempts)
	}
	if _, statErr := os.Stat(filepath.Join(dgHome, "nostr-operator.key")); statErr == nil {
		t.Errorf("ADV-4 refusal minted an operator key anyway — silent fork")
	}
}

// (gap 2) PERMANENTLY UNVERIFIABLE RELAY — refuse by default, --new-operator to
// override. A relay that NEVER answers the probe leaves ADV-4 unverifiable:
// by default `up` REFUSES (never silently mints against it); the operator can
// explicitly confirm a brand-new-relay bootstrap with --new-operator.
func TestUp_Relay_Unverifiable_RefusesByDefault_NewOperatorOverrides_ADV4(t *testing.T) {
	t.Setenv("DONTGUESS_RELAY_URLS", "")
	t.Setenv("DONTGUESS_RELAY_URL", "")
	shrinkProbeTiming(t)

	relayServer := newMiniRelay(t)
	relayServer.failFirstN = 1 << 30 // every handshake 503 — permanently unverifiable
	url := relayServer.wsURL()

	// Default (no confirmation): REFUSE, no key minted.
	t.Run("refuses_by_default", func(t *testing.T) {
		dgHome := t.TempDir()
		orig := upServeLauncher
		upServeLauncher = func(string) (bool, error) {
			t.Fatalf("upServeLauncher was called — an unverifiable relay must refuse before serve")
			return false, nil
		}
		t.Cleanup(func() { upServeLauncher = orig })

		var out bytes.Buffer
		err := runUpCore(dgHome, []string{url}, "", false, &out)
		if err == nil {
			t.Fatalf("up --relay against a permanently-unverifiable relay returned nil, want fail-closed refusal")
		}
		if !strings.Contains(err.Error(), "could not verify") {
			t.Errorf("error does not surface the fail-closed unverifiable-relay guard: %v", err)
		}
		if _, statErr := os.Stat(filepath.Join(dgHome, "nostr-operator.key")); statErr == nil {
			t.Errorf("unverifiable relay minted an operator key anyway — silent fork")
		}
	})

	// With --new-operator: the operator explicitly confirms a first bootstrap →
	// proceeds and mints. This is the deliberate escape hatch (never the silent
	// default).
	t.Run("new_operator_flag_overrides", func(t *testing.T) {
		dgHome := t.TempDir()
		withInProcessServeLauncher(t)
		withStubBootService(t)

		var out bytes.Buffer
		if err := runUpCore(dgHome, []string{url}, "", true, &out); err != nil {
			t.Fatalf("up --relay --new-operator against an unverifiable relay should proceed, got: %v", err)
		}
		if !strings.Contains(out.String(), "--new-operator confirmation") {
			t.Errorf("output did not report the --new-operator override:\n%s", out.String())
		}
		if _, statErr := os.Stat(filepath.Join(dgHome, "nostr-operator.key")); statErr != nil {
			t.Errorf("--new-operator confirmed bootstrap did not mint an operator key: %v", statErr)
		}
		cfg, cerr := exchange.LoadConfig(dgHome)
		if cerr != nil {
			t.Fatalf("LoadConfig after --new-operator up: %v", cerr)
		}
		if cfg.Tier != exchange.TierTeam {
			t.Errorf("persisted tier = %q, want %q", cfg.Tier, exchange.TierTeam)
		}
	})
}
