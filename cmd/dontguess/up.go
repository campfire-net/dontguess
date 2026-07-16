package main

// up.go — dontguess-75a (design §1 + §6 + §9 Gate B/P7, re-scoped 2026-07-15 for
// the daa explicit-persisted-tier ruling + the ef1 exchange-side-only self-admit
// reclassification).
//
// `dontguess up` is a single idempotent, re-runnable bootstrap that COMPOSES the
// existing verbs (exchange.Init, serve, the allowlist live-admit IPC path,
// pkg/bootservice) — it does NOT reimplement any of them:
//
//   - `up --relay <urls>`  => exchange.Init persists tier=team + the relay (the
//     daa config-time mechanism), starts serve, self-admits the operator's own
//     key into the fleet allowlist/roster via the SAME P6/dontguess-113
//     signed-IPC OpAllowlist path `dontguess allowlist add` uses, then installs
//     the boot service (pkg/bootservice.Install — Linux systemd --user + linger
//     via 748, macOS launchd via aa4).
//   - `up` with no relay and no prior config => solo: starts serve in the
//     background (spawnDetachedServe) — a backgrounding/pidfile shim, NOT a
//     boot-service install (that stays scoped to the --relay flow per the
//     2026-07-15 re-scope).
//   - `up` with a prior persisted team+relay config (no flags this run) honors
//     it — same team flow, no re-prompt, idempotent.
//   - `up --team`/`--fleet` with no relay anywhere (flag, env, or persisted)
//     surfaces the daa config-time fail-closed error verbatim — never a silent
//     solo downgrade, never a hang (exchange.Init already implements this; up
//     only has to propagate it).
//
// ADV-4 (FLEET = ONE operator): a machine with NO local operator private key
// MUST detect an existing operator's events already on the relay and REFUSE to
// mint a competing sequencer, rather than silently bootstrapping a second
// operator identity. probeExistingOperatorEvents does a bounded one-shot NIP-01
// read (never a write) BEFORE exchange.Init would mint anything.
//
// dontguess-e39 (two ADV-4 probe gaps self-flagged by the 75a impl):
//
//	Gap 1 — PROBE THE EFFECTIVE RESOLVED-RELAY SET, not only this-invocation's
//	declared relays. A machine whose operator key file was lost/deleted (disk
//	issue, accidental rm) but whose exchange config STILL persists an old
//	team+relay, run as plain `dontguess up` (no --relay flag, no env), used to
//	SKIP the probe entirely (declaredRelays empty) and let exchange.Init
//	silently mint a BRAND-NEW operator key — forking the sequencer identity the
//	fleet already trusts. The probe now runs on the RESOLVED relay set:
//	flag/env this run, else the relays persisted in a prior team/fleet config.
//
//	Gap 2 — NEVER FAIL OPEN on an unreachable relay. A single probe error used
//	to warn-and-proceed (mint). The probe now RETRIES with bounded backoff, and
//	if it STILL cannot verify the relay, it REFUSES to mint rather than
//	silently forking — unless the operator explicitly confirms a first-of-its-
//	kind bootstrap with --new-operator (which never overrides a POSITIVELY
//	detected existing operator, only an unverifiable relay).

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/3dl-dev/dontguess/pkg/bootservice"
	"github.com/3dl-dev/dontguess/pkg/exchange"
	"github.com/3dl-dev/dontguess/pkg/identity"
	"github.com/3dl-dev/dontguess/pkg/nostr"
	"github.com/3dl-dev/dontguess/pkg/relay"
	"github.com/spf13/cobra"
)

var upCmd = &cobra.Command{
	Use:   "up",
	Short: "Bootstrap (or re-attach to) this operator — composes init/serve/allowlist/boot-service",
	Long: `dontguess up is a single idempotent, re-runnable bootstrap. It composes the
existing verbs — it does not replace them:

  dontguess up                          # SOLO: local, no relay, no scrip
  dontguess up --relay ws://host:7777   # FLEET: promotes the SAME operator to team tier
  dontguess up --team                   # explicit team tier — REQUIRES a relay (flag,
                                         # DONTGUESS_RELAY_URLS/URL env, or a previously
                                         # persisted relay); fails loud, never a silent
                                         # solo downgrade, if none is configured.

Re-running up is safe: an already-running operator is detected via its IPC
socket and left alone; a persisted team+relay config is honored without
re-prompting.`,
	Args: cobra.NoArgs,
	RunE: runUp,
}

var (
	upRelayFlag       string
	upTeamFlag        bool
	upFleetFlag       bool
	upNewOperatorFlag bool
)

func init() {
	upCmd.Flags().StringVar(&upRelayFlag, "relay", "", "relay websocket URL(s), comma-separated — promotes the operator to team tier")
	upCmd.Flags().BoolVar(&upTeamFlag, "team", false, "declare team tier explicitly (requires a relay from --relay, env, or a prior persisted config)")
	upCmd.Flags().BoolVar(&upFleetFlag, "fleet", false, "declare fleet tier explicitly (requires a relay from --relay, env, or a prior persisted config)")
	upCmd.Flags().BoolVar(&upNewOperatorFlag, "new-operator", false, "confirm you are the FIRST operator bootstrapping a brand-new relay — required to mint when the ADV-4 probe cannot reach/verify the relay after retries (never overrides a positively-detected existing operator)")
	rootCmd.AddCommand(upCmd)
}

func runUp(cmd *cobra.Command, _ []string) error {
	if upTeamFlag && upFleetFlag {
		return fmt.Errorf("up: --team and --fleet are mutually exclusive")
	}
	var tier exchange.Tier
	switch {
	case upTeamFlag:
		tier = exchange.TierTeam
	case upFleetFlag:
		tier = exchange.TierFleet
	}
	return runUpCore(resolveDGHome(), parseRelayFlag(upRelayFlag), tier, upNewOperatorFlag, cmd.OutOrStdout())
}

// parseRelayFlag splits a comma-separated --relay value into a trimmed,
// non-empty URL list (nil if the flag was not given).
func parseRelayFlag(v string) []string {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(v, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// Test seams (mirrors the codebase's existing convention of package-level vars
// for production functions that touch the OS/network — e.g. serve.go's
// operatorConnDeadline, relayAttachInitialBackoff): production `up` spawns a
// real detached OS process and calls the real pkg/bootservice.Install /
// probeExistingOperatorEvents; tests substitute an in-process serve launch
// (driving the REAL runServeLocalCtx in a goroutine, since os.Executable()
// under `go test` resolves to the test binary, not `dontguess`) and a fake or
// real in-process relay probe.
var (
	upServeLauncher      = spawnDetachedServe
	upInstallBootService = bootservice.Install
	upProbeRelay         = probeExistingOperatorEvents
)

// runUpCore is the testable core of `up`. out receives the same progress
// lines the CLI prints, so tests can assert on them without capturing os.Stdout.
func runUpCore(dgHome string, relayFlag []string, tier exchange.Tier, newOperator bool, out io.Writer) error {
	// The relay set THIS invocation is declaring: an explicit --relay flag, else
	// the env override (mirrors serve.go's resolveRelayURLs/effectiveRelayURLs
	// backward-compat convention). Empty means "declare nothing new this run" —
	// exchange.Init then preserves whatever was already persisted (the "honor
	// prior persisted team+relay config" case) or resolves to solo.
	declaredRelays := relayFlag
	if len(declaredRelays) == 0 {
		declaredRelays = resolveRelayURLs()
	}

	// ADV-4 refuse-mint guard (design §6, §9 Gate B/P7; dontguess-e39 gap 1).
	// The relay set to PROBE is the EFFECTIVE resolved set: the relays declared
	// THIS run (flag/env), else — when this invocation declares nothing new —
	// the relays persisted in a prior team/fleet config. This closes the
	// silent-fork-on-key-loss hole: a machine whose operator key file was
	// lost/deleted but whose config STILL persists an old team+relay, run as
	// plain `dontguess up`, MUST still probe (its effective tier is team from
	// the persisted config) rather than let exchange.Init mint a fresh operator
	// key and fork the sequencer the fleet already trusts.
	probeRelays := declaredRelays
	if len(probeRelays) == 0 {
		if existing, lerr := exchange.LoadConfig(dgHome); lerr == nil {
			probeRelays = existing.RelayURLs
		}
	}

	// Only relevant when this machine has no local operator private key yet — a
	// genuine existing operator re-running `up` on its OWN machine always has
	// the key file and skips this entirely (loadOperatorSigner succeeds). Runs
	// BEFORE exchange.Init, which is what would otherwise mint a fresh operator
	// identity.
	if len(probeRelays) > 0 {
		if _, keyErr := loadOperatorSigner(dgHome); keyErr != nil {
			for _, url := range probeRelays {
				found, perr := probeRelayWithRetry(context.Background(), url)
				if perr != nil {
					// dontguess-e39 gap 2: the relay could not be verified even
					// after bounded retries. NEVER fail open — a competing
					// operator's events may be sitting on the relay, just
					// unreadable this instant (warming, network blip). Refuse to
					// mint against an unverifiable relay unless the operator
					// EXPLICITLY confirms a brand-new first bootstrap. The flag
					// only overrides an UNVERIFIABLE relay — a positive `found`
					// below still refuses regardless.
					if newOperator {
						fmt.Fprintf(out, "up: warning: could not verify %s carries no existing operator (%v) — proceeding under --new-operator confirmation\n", url, perr)
						continue
					}
					return fmt.Errorf(
						"up --relay: could not verify %s carries no existing operator after %d attempts (%v) — "+
							"refusing to mint against an unverifiable relay (ADV-4). This machine has no local "+
							"operator key (%s absent). Fix relay reachability and retry; a second machine joins an "+
							"existing fleet as a MEMBER (`dontguess join <invite-token>`), never as a second operator. "+
							"If you ARE the first operator bootstrapping a brand-new relay, re-run with --new-operator to confirm",
						url, upProbeRetries+1, perr, filepath.Join(dgHome, "nostr-operator.key"))
				}
				if found {
					return fmt.Errorf(
						"up --relay: refusing to mint a competing operator identity — %s already carries existing "+
							"dontguess events (ADV-4). This machine has no local operator key (%s absent). A second "+
							"machine joins an existing fleet as a MEMBER (`dontguess join <invite-token>`), never as a "+
							"second operator via `up --relay`. If you ARE the operator recovering a lost machine, "+
							"restore the key first (`dontguess operator import`) before running `up --relay` again",
						url, filepath.Join(dgHome, "nostr-operator.key"))
				}
			}
		}
	}

	// exchange.Init persists tier+relay (the daa config-time mechanism). A
	// declared team/fleet tier with no effective relay (flag+env+persisted all
	// empty) surfaces Init's own fail-closed error verbatim here — bounded, no
	// hang, no silent solo downgrade (RE-SCOPE case (c)); up adds nothing.
	cfg, err := exchange.Init(exchange.InitOptions{DGHome: dgHome, RelayURLs: declaredRelays, Tier: tier})
	if err != nil {
		return fmt.Errorf("up: %w", err)
	}

	effectiveTier := cfg.Tier
	if effectiveTier == "" {
		effectiveTier = exchange.TierSolo
	}

	alreadyRunning, err := upServeLauncher(dgHome)
	if err != nil {
		return fmt.Errorf("up: starting serve: %w", err)
	}

	fmt.Fprintf(out, "✓ operator identity (secp256k1) ready: %s\n", cfg.OperatorNpub)
	if alreadyRunning {
		fmt.Fprintf(out, "✓ engine already running (%s tier) — up is idempotent, nothing to (re)start\n", effectiveTier)
	} else {
		fmt.Fprintf(out, "✓ engine running (%s tier)\n", effectiveTier)
	}

	// Team/fleet only: self-admit + boot-service install. Solo never reaches
	// this — the design's boot-service scope is the --relay flow (2026-07-15
	// re-scope); solo's persistence is spawnDetachedServe's backgrounding +
	// pidfile shim above, not a systemd/launchd unit.
	if effectiveTier.RequiresRelay() {
		if serr := selfAdmitOperator(dgHome, cfg.OperatorKeyHex, out); serr != nil {
			return fmt.Errorf("up: self-admit: %w", serr)
		}
		fmt.Fprintf(out, "✓ self-admitted operator key to the fleet allowlist + relay roster\n")

		exe, eerr := os.Executable()
		if eerr != nil {
			return fmt.Errorf("up: resolving executable for boot service: %w", eerr)
		}
		res, berr := upInstallBootService(bootservice.Options{ServeBinary: exe, DGHome: dgHome, RelayURLs: cfg.RelayURLs})
		if berr != nil {
			return fmt.Errorf("up: installing boot service: %w", berr)
		}
		if res.DryRun {
			fmt.Fprintf(out, "✓ boot service: dry-run (%s) — unit written to %s, not enabled\n", res.DryRunNote, res.UnitPath)
		} else {
			fmt.Fprintf(out, "✓ boot service installed (%s), linger=%v\n", res.UnitPath, res.Lingering)
		}
	}

	return nil
}

// selfAdmitOperator publishes the operator's own key into the fleet
// allowlist/roster via the SAME live signed-IPC OpAllowlist path
// `dontguess allowlist add` drives (dontguess-113/allowlist.go) — it is not a
// new admission mechanism. This is EXCHANGE-SIDE ONLY per the ef1
// reclassification (design note 2026-07-15): there is no relay-side writePolicy
// self-admit in the default path, so any stock relay works.
func selfAdmitOperator(dgHome, operatorKeyHex string, out io.Writer) error {
	conn, ok := dialSocketMaybe(dgHome)
	if !ok {
		return fmt.Errorf("operator socket not reachable at %s after serve launch", resolveOperatorSocketPathFor(dgHome))
	}
	defer conn.Close()
	return allowlistLiveRequest(conn, dgHome, allowlistActionAdd, operatorKeyHex, "operator (self)", out)
}

// upServeReadyTimeout bounds how long `up` waits for a freshly-spawned serve
// process's operator socket to become reachable before failing loud — never a
// silent hang. Package var so tests can shrink it.
var upServeReadyTimeout = 15 * time.Second

// spawnDetachedServe is the production up.go serve launcher: the "solo ≈
// runServeLocal wrapped in a backgrounding/pidfile shim" (design §1). If the
// operator socket is already reachable, serve is already running (idempotent
// no-op re-run — no duplicated state). Otherwise it execs THIS SAME dontguess
// binary as `serve`, detached (new session, stdio to /dev/null — serve's own
// buildLogDest already rotates dontguess.log under dgHome), writes a pidfile
// for operator convenience, and waits (bounded) for the socket to come up.
func spawnDetachedServe(dgHome string) (alreadyRunning bool, err error) {
	if _, ok := dialSocketMaybe(dgHome); ok {
		return true, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return false, fmt.Errorf("resolve dontguess executable: %w", err)
	}
	devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return false, fmt.Errorf("open %s: %w", os.DevNull, err)
	}
	defer devnull.Close()

	cmd := exec.Command(exe, "serve")
	cmd.Env = append(os.Environ(), "DG_HOME="+dgHome)
	cmd.Stdout = devnull
	cmd.Stderr = devnull
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if serr := cmd.Start(); serr != nil {
		return false, fmt.Errorf("spawn %s serve: %w", exe, serr)
	}
	if werr := os.WriteFile(pidFilePath(dgHome), []byte(strconv.Itoa(cmd.Process.Pid)), 0o644); werr != nil {
		// Non-fatal: the pidfile is an operator convenience. The operator socket
		// (dialSocketMaybe) is the authoritative liveness source, checked next.
		_ = werr
	}
	if rerr := cmd.Process.Release(); rerr != nil {
		return false, fmt.Errorf("detach serve process: %w", rerr)
	}
	if !waitForOperatorSocket(dgHome, upServeReadyTimeout) {
		return false, fmt.Errorf("serve did not become reachable within %s — check %s/dontguess.log", upServeReadyTimeout, dgHome)
	}
	return false, nil
}

// pidFilePath is the dgHome-relative pidfile spawnDetachedServe writes for
// operator convenience (e.g. `kill $(cat $DG_HOME/dontguess.pid)`).
func pidFilePath(dgHome string) string {
	return filepath.Join(dgHome, "dontguess.pid")
}

// waitForOperatorSocket polls dialSocketMaybe until it succeeds or timeout
// elapses. Bounded — never a silent infinite hang.
func waitForOperatorSocket(dgHome string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if conn, ok := dialSocketMaybe(dgHome); ok {
			conn.Close()
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// upProbeTimeout bounds a single relay probe (the ADV-4 refuse-mint check) so
// a dead/slow relay can never hang `up --relay` — each attempt is individually
// bounded, and the caller RETRIES then REFUSES (never silently mints) if the
// relay stays unverifiable. Package var so tests can shrink it.
var upProbeTimeout = 8 * time.Second

// upProbeRetries / upProbeRetryBackoff bound the ADV-4 probe's retry loop
// (dontguess-e39 gap 2): a transiently-unreachable relay (warming, network
// blip) must not fail the probe OPEN into a silent mint. probeRelayWithRetry
// makes up to upProbeRetries+1 total attempts with a fixed backoff between
// them; only if EVERY attempt errors does the caller refuse (or require
// --new-operator). Package vars so tests can shrink them.
var (
	upProbeRetries      = 3
	upProbeRetryBackoff = 500 * time.Millisecond
)

// probeRelayWithRetry wraps the single-shot upProbeRelay in a bounded retry
// loop. A definitive answer (found/not-found, err==nil) returns immediately; a
// probe error is retried up to upProbeRetries times with upProbeRetryBackoff
// between attempts before the final error is surfaced to the caller, which then
// fails CLOSED (refuse-to-mint) rather than open. Honors ctx cancellation
// during backoff so a cancelled `up` never blocks.
func probeRelayWithRetry(ctx context.Context, relayURL string) (found bool, err error) {
	for attempt := 0; ; attempt++ {
		found, err = upProbeRelay(ctx, relayURL)
		if err == nil {
			return found, nil
		}
		if attempt >= upProbeRetries {
			return false, err
		}
		select {
		case <-ctx.Done():
			return false, err
		case <-time.After(upProbeRetryBackoff):
		}
	}
}

// probeExistingOperatorEvents does a bounded, one-shot NIP-01 read (REQ with
// Limit=1 over the dontguess kind set, waiting for the first EVENT or EOSE) —
// it NEVER signs or publishes anything. It is the ADV-4 detection primitive:
// a non-empty answer means SOME operator already runs an exchange on this
// relay, so a machine with no local operator key must not mint a second one.
//
// Uses an ephemeral throwaway identity purely to satisfy relay.New's
// constructor; relay.WithoutClientAuth skips the NIP-42 handshake entirely
// (mirroring the operator relay legs' own dontguess-726 fix — a relay that
// never pushes an AUTH challenge for reads must not be blocked on one).
func probeExistingOperatorEvents(ctx context.Context, relayURL string) (bool, error) {
	ephemeral, err := identity.Generate()
	if err != nil {
		return false, fmt.Errorf("probe relay: generate ephemeral identity: %w", err)
	}
	pctx, cancel := context.WithTimeout(ctx, upProbeTimeout)
	defer cancel()

	conn := relay.New(relayURL, ephemeral, relay.WithoutClientAuth())
	defer conn.Close()
	if cerr := conn.Connect(pctx); cerr != nil {
		return false, fmt.Errorf("probe relay %s: connect: %w", relayURL, cerr)
	}

	limit := 1
	frame, ferr := relay.EncodeReq("dg-up-probe", relay.Filter{Kinds: nostr.DontguessKinds, Limit: &limit})
	if ferr != nil {
		return false, fmt.Errorf("probe relay: encode REQ: %w", ferr)
	}
	if serr := conn.Send(pctx, frame); serr != nil {
		return false, fmt.Errorf("probe relay %s: send REQ: %w", relayURL, serr)
	}

	type recvResult struct {
		raw []byte
		err error
	}
	for {
		resCh := make(chan recvResult, 1)
		go func() {
			raw, rerr := conn.Recv(pctx)
			resCh <- recvResult{raw, rerr}
		}()
		select {
		case <-pctx.Done():
			return false, fmt.Errorf("probe relay %s: timed out waiting for EOSE/EVENT", relayURL)
		case res := <-resCh:
			if res.err != nil {
				return false, fmt.Errorf("probe relay %s: recv: %w", relayURL, res.err)
			}
			f, perr := relay.ParseFrame(res.raw)
			if perr != nil {
				continue // ignore a malformed frame, keep waiting within the deadline
			}
			switch f.Type {
			case relay.LabelEVENT:
				return true, nil
			case relay.LabelEOSE:
				return false, nil
			}
		}
	}
}
