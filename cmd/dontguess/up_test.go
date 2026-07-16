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
	if err := runUpCore(dgHome, nil, "", &out1); err != nil {
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
	if err := runUpCore(dgHome, nil, "", &out2); err != nil {
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
	if err := runUpCore(dgHome, []string{relayServer.wsURL()}, "", &out); err != nil {
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
		errCh <- runUpCore(dgHome, nil, exchange.TierTeam, &out)
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
	err := runUpCore(dgHome, []string{relayServer.wsURL()}, "", &out)
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
