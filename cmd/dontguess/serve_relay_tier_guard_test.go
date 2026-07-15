package main

// serve_relay_tier_guard_test.go — item dontguess-daa (Gate A, design §1/§6 +
// operator ruling 2026-07-15). Relay-tier intent must be EXPLICIT and relay
// setup REQUIRED AT CONFIG TIME via a PERSISTED Config.Tier — no silent tier
// downgrade, no hardcoded relay default. This is the config-time COMPLEMENT of
// serve_relay_startup_guard_test.go (§3.9, the opposite direction): §3.9 fires
// when relays ARE present but the operator config is not; this fires when a
// team/fleet tier IS declared but no relay is configured.
//
// These are package-main in-process tests: they drive the REAL runServeLocal /
// runServeLocalCtx + resolveServeTierAndRelays so serve.go's edits are exercised
// end-to-end (any serve.go edit invalidates the test cache — H7 discipline).

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/3dl-dev/dontguess/pkg/exchange"
)

// discardLogger is a logger that drops output — the resolver's migration notice
// is behavior we assert via the persisted config, not the log stream.
func discardLogger() *log.Logger {
	return log.New(io.Discard, "", 0)
}

// runServeLocalBounded drives the REAL runServeLocal on a goroutine and returns
// its error, failing the test if it does not return within timeout. It PROVES a
// fail-closed guard returns an error rather than hanging (the operator ruling's
// "not a hang" requirement).
func runServeLocalBounded(t *testing.T, dgHome string, timeout time.Duration) error {
	t.Helper()
	errCh := make(chan error, 1)
	go func() { errCh <- runServeLocal(dgHome) }()
	select {
	case err := <-errCh:
		return err
	case <-time.After(timeout):
		t.Fatalf("runServeLocal did not return within %s — a declared team/fleet tier with no relay must fail LOUD, never hang", timeout)
		return nil
	}
}

// TestServeTierGuard_TeamDeclaredNoRelay_HardErrorsNoHang drives runServeLocal
// with a PERSISTED tier=team config and NO relay (env unset + config relay_urls
// empty). It must return a non-nil error naming the tier and the relay flag
// within a bounded deadline — never silently start solo, never hang. Repeats for
// tier=fleet.
func TestServeTierGuard_TeamDeclaredNoRelay_HardErrorsNoHang(t *testing.T) {
	for _, tier := range []exchange.Tier{exchange.TierTeam, exchange.TierFleet} {
		tier := tier
		t.Run(string(tier), func(t *testing.T) {
			dgHome := t.TempDir()

			// Env has NO relay — the tier is declared purely by the persisted config.
			t.Setenv("DONTGUESS_RELAY_URLS", "")
			t.Setenv("DONTGUESS_RELAY_URL", "")

			// Persist a config declaring the tier with an EMPTY relay set (and an
			// operator key present, so this is NOT the §3.9 empty-key case — it is
			// specifically the missing-relay case).
			cfg := &exchange.Config{
				OperatorKeyHex: "aabbccddeeff00112233445566778899aabbccddeeff001122334455667788",
				Tier:           tier,
				StorePath:      filepath.Join(dgHome, "events.jsonl"),
				CreatedAt:      time.Now().UnixNano(),
			}
			if err := exchange.WriteConfig(exchange.ConfigPath(dgHome), cfg); err != nil {
				t.Fatalf("writing %s-tier config: %v", tier, err)
			}

			err := runServeLocalBounded(t, dgHome, 5*time.Second)
			if err == nil {
				t.Fatalf("tier=%s + no relay: runServeLocal returned nil — silent solo downgrade, the guard did not fire", tier)
			}
			msg := err.Error()
			if !strings.Contains(msg, string(tier)) {
				t.Errorf("tier=%s: error does not name the declared tier: %q", tier, msg)
			}
			// The remedy text must name the REAL remedy — the DONTGUESS_RELAY_URLS
			// env var — not a nonexistent --relay flag on init/serve (dontguess-4f0
			// b6e3 fix: init/serve have no --relay flag; --relay lives on put/buy).
			if !strings.Contains(msg, "DONTGUESS_RELAY_URLS") {
				t.Errorf("tier=%s: error does not name the DONTGUESS_RELAY_URLS remedy: %q", tier, msg)
			}
			if strings.Contains(msg, "--relay") {
				t.Errorf("tier=%s: error still names a nonexistent --relay flag on serve: %q", tier, msg)
			}

			// It must have failed at the guard, never reaching the engine/socket.
			if _, serr := os.Stat(resolveOperatorSocketPath(dgHome)); serr == nil {
				t.Errorf("tier=%s: operator socket exists — serve advanced past the fail-closed tier guard", tier)
			}
		})
	}
}

// TestServeTierGuard_SoloCleanStart_IndividualTier proves the solo path (no tier
// declared, no relay) resolves to the individual tier and boots clean. The
// resolver returning (solo, empty) IS the SOLO GATE: every relay-tier wiring in
// runServeLocalCtx (operatorSigner, ScripStore, TrustChecker) is gated on
// len(relayURLs) > 0, so an empty relay set keeps all three at their true-nil
// individual-tier values. The bounded runServeLocalCtx drive then proves the
// real entrypoint boots individual-tier (operator socket answers) and never
// hangs or errors.
func TestServeTierGuard_SoloCleanStart_IndividualTier(t *testing.T) {
	dgHome := t.TempDir()
	t.Setenv("DONTGUESS_RELAY_URLS", "")
	t.Setenv("DONTGUESS_RELAY_URL", "")

	// f5e always mints the nostr operator key now, so we assert the SOLO GATES
	// (tier=solo, empty relay set → nil signer/scrip/trust downstream), not the
	// key's absence.
	operatorIdentity, err := loadOrCreateNostrOperatorIdentity(dgHome)
	if err != nil {
		t.Fatalf("loadOrCreateNostrOperatorIdentity: %v", err)
	}
	tier, relayURLs, rerr := resolveServeTierAndRelays(dgHome, operatorIdentity, discardLogger())
	if rerr != nil {
		t.Fatalf("solo resolve: unexpected error: %v", rerr)
	}
	if tier != exchange.TierSolo {
		t.Errorf("solo resolve tier = %q, want %q", tier, exchange.TierSolo)
	}
	if len(relayURLs) != 0 {
		t.Errorf("solo resolve relayURLs = %v, want empty (no relay, no default substituted)", relayURLs)
	}

	// Drive the REAL runServeLocalCtx bounded by a cancelable context; prove the
	// operator socket comes up (clean individual-tier boot) and the entrypoint
	// returns cleanly on cancel.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runServeLocalCtx(ctx, dgHome) }()

	sockPath := resolveOperatorSocketPath(dgHome)
	deadline := time.Now().Add(3 * time.Second)
	var conn net.Conn
	for {
		conn, err = net.Dial("unix", sockPath)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatalf("operator socket never came up on the solo tier: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if derr := conn.SetDeadline(time.Now().Add(2 * time.Second)); derr != nil {
		t.Fatalf("SetDeadline: %v", derr)
	}
	if err := json.NewEncoder(conn).Encode(map[string]any{"op": OpListHeld}); err != nil {
		t.Fatalf("encode list-held: %v", err)
	}
	var resp map[string]any
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		t.Fatalf("decode list-held on solo tier: %v", err)
	}
	conn.Close() //nolint:errcheck

	cancel()
	select {
	case serr := <-done:
		if serr != nil {
			t.Fatalf("solo runServeLocalCtx returned error: %v", serr)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("solo runServeLocalCtx did not shut down within 5s after cancel")
	}
}

// TestServeTierGuard_LiveOperatorMigration_NoSoloDowngrade proves the live
// env-configured team operator (project memory "Live exchange") is MIGRATED, not
// downgraded. The live operator ran `init` (so a persisted config with the
// operator key exists) but predates the Tier field (no persisted tier), and its
// relay is supplied via env DONTGUESS_RELAY_URLS. On first upgraded run the
// resolver persists tier=team + the env relays into that existing config and does
// NOT downgrade to solo. (A truly config-LESS relay serve is the §3.9 rogue case
// and stays covered by serve_relay_startup_guard_test.go — migration deliberately
// does not auto-create a config there.) Drives both the resolver directly
// (deterministic assertions) and the real runServeLocalCtx entrypoint.
func TestServeTierGuard_LiveOperatorMigration_NoSoloDowngrade(t *testing.T) {
	dgHome := t.TempDir()
	// Deliberately UNREACHABLE relays (RFC-5737 TEST-NET-1 + an invalid host): the
	// migration + tier resolution is what we assert; the async relay legs (347)
	// must never touch a real/live relay from a unit test.
	envRelays := "ws://192.0.2.1:7777,wss://relay.invalid.example"
	t.Setenv("DONTGUESS_RELAY_URLS", envRelays)
	t.Setenv("DONTGUESS_RELAY_URL", "")

	operatorIdentity, err := loadOrCreateNostrOperatorIdentity(dgHome)
	if err != nil {
		t.Fatalf("loadOrCreateNostrOperatorIdentity: %v", err)
	}

	// Pre-write the LIVE operator's pre-upgrade config: `init` wrote it with the
	// operator key, but it predates the Tier field (tier absent, relay_urls empty
	// because the operator supplied relays via env). This is the exact "no
	// persisted Config.Tier" state the migration targets.
	preCfg := &exchange.Config{
		OperatorKeyHex: operatorIdentity.PubKeyHex(),
		OperatorNpub:   operatorIdentity.Npub(),
		StorePath:      filepath.Join(dgHome, "events.jsonl"),
		CreatedAt:      time.Now().UnixNano(),
		MinReputation:  exchange.DefaultMinReputation,
	}
	if err := exchange.WriteConfig(exchange.ConfigPath(dgHome), preCfg); err != nil {
		t.Fatalf("writing pre-upgrade config: %v", err)
	}

	tier, relayURLs, rerr := resolveServeTierAndRelays(dgHome, operatorIdentity, discardLogger())
	if rerr != nil {
		t.Fatalf("migration resolve: unexpected error: %v", rerr)
	}
	if tier != exchange.TierTeam {
		t.Fatalf("migration resolve tier = %q, want %q (must NOT downgrade to solo)", tier, exchange.TierTeam)
	}
	if len(relayURLs) != 2 {
		t.Fatalf("migration resolve relayURLs = %v, want the 2 env relays", relayURLs)
	}

	// The migration must have PERSISTED tier=team + relays + the operator key.
	persisted, lerr := exchange.LoadConfig(dgHome)
	if lerr != nil {
		t.Fatalf("LoadConfig after migration: %v", lerr)
	}
	if persisted.Tier != exchange.TierTeam {
		t.Errorf("persisted tier = %q, want %q — migration did not persist team tier", persisted.Tier, exchange.TierTeam)
	}
	if len(persisted.RelayURLs) != 2 {
		t.Errorf("persisted relay_urls = %v, want the 2 env relays", persisted.RelayURLs)
	}
	if persisted.OperatorKeyHex != operatorIdentity.PubKeyHex() {
		t.Errorf("persisted operator_key = %q, want the minted identity key %q", persisted.OperatorKeyHex, operatorIdentity.PubKeyHex())
	}

	// End-to-end through the real entrypoint: with the config now migrated the
	// §3.9 guard passes; the invalid relays attach ASYNCHRONOUSLY (347) so serve
	// does not block on them. Drive bounded and confirm the persisted config still
	// carries team tier (never re-downgraded), then cancel.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runServeLocalCtx(ctx, dgHome) }()

	// Give the entrypoint a moment to reach steady state, confirm no downgrade.
	time.Sleep(200 * time.Millisecond)
	again, aerr := exchange.LoadConfig(dgHome)
	if aerr != nil {
		cancel()
		<-done
		t.Fatalf("LoadConfig during serve: %v", aerr)
	}
	if again.Tier != exchange.TierTeam {
		cancel()
		<-done
		t.Fatalf("tier during serve = %q, want %q — the live operator was downgraded", again.Tier, exchange.TierTeam)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("migration runServeLocalCtx did not shut down within 5s after cancel")
	}
}

// TestServeTierGuard_CorruptConfig_HardErrorsNeverDowngrades is the ground-source
// veracity test for dontguess-4f0 (CONFIRMED HIGH confidentiality-downgrade). A
// would-be team operator whose PERSISTED config becomes PRESENT-but-unreadable
// (truncated JSON, or the min_reputation>max validation error LoadConfig returns)
// with NO env relays must produce a HARD startup error and NEVER silently boot an
// individual-tier / plaintext-solo store. Pre-fix, resolveServeTierAndRelays read
// tier ONLY `if cfgErr == nil` and swallowed cfgErr → effectiveTier defaulted to
// solo → the operator booted plaintext. This drives the REAL runServeLocal
// entrypoint bounded and asserts (a) it returns a non-nil error and (b) the
// operator socket NEVER comes up (it never reached a serving state).
func TestServeTierGuard_CorruptConfig_HardErrorsNeverDowngrades(t *testing.T) {
	// Two PRESENT-but-unreadable shapes, both for a config that WOULD have carried
	// a team tier / relay set (the exact downgrade the reject clause forbids).
	cases := []struct {
		name  string
		write func(path string) // writes a corrupt config to path
	}{
		{
			name: "truncated-json",
			write: func(path string) {
				// A team config, truncated mid-object so json.Unmarshal fails. If the
				// resolver swallowed this it would default to solo/plaintext.
				if err := os.WriteFile(path, []byte(`{"tier":"team","relay_urls":["wss://relay.exampl`), 0600); err != nil {
					t.Fatalf("writing truncated config: %v", err)
				}
			},
		},
		{
			name: "min_reputation_over_max",
			write: func(path string) {
				// Parses cleanly but LoadConfig rejects it (min_reputation>max). The
				// config DECLARES team tier + a relay, so a solo/plaintext resolution
				// here would be the confidentiality downgrade under test.
				cfg := map[string]any{
					"tier":           string(exchange.TierTeam),
					"relay_urls":     []string{"wss://relay.example:7777"},
					"operator_key":   "aabbccddeeff00112233445566778899aabbccddeeff001122334455667788",
					"store_path":     "events.jsonl",
					"min_reputation": int(exchange.MaxMinReputation) + 1,
				}
				data, merr := json.Marshal(cfg)
				if merr != nil {
					t.Fatalf("marshal corrupt config: %v", merr)
				}
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatalf("writing min_reputation config: %v", err)
				}
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			dgHome := t.TempDir()
			// NO env relays — the tier would come purely from the (now corrupt)
			// persisted config, so a swallowed error downgrades to plaintext solo.
			t.Setenv("DONTGUESS_RELAY_URLS", "")
			t.Setenv("DONTGUESS_RELAY_URL", "")

			tc.write(exchange.ConfigPath(dgHome))

			err := runServeLocalBounded(t, dgHome, 5*time.Second)
			if err == nil {
				t.Fatalf("corrupt config (%s): runServeLocal returned nil — the operator was silently DOWNGRADED to plaintext solo (confidentiality-downgrade, guard did not fire)", tc.name)
			}
			// The error must be the config-unreadable refusal, not some unrelated
			// downstream failure — it names the config path and the refusal.
			msg := err.Error()
			if !strings.Contains(msg, "unreadable") && !strings.Contains(msg, "corrupt") {
				t.Errorf("corrupt config (%s): error is not the fail-closed refusal: %q", tc.name, msg)
			}

			// It must have failed BEFORE serving: the operator socket never came up.
			if _, serr := os.Stat(resolveOperatorSocketPath(dgHome)); serr == nil {
				t.Errorf("corrupt config (%s): operator socket exists — serve reached a serving (plaintext-solo) state past the guard", tc.name)
			}
		})
	}
}

// TestServeTierGuard_AbsentConfig_ResolvesSoloControl is the CONTROL for
// dontguess-4f0: a genuinely ABSENT config (fresh home) with no env relays must
// still resolve to the solo tier with NO error — the fix must reject only
// PRESENT-but-corrupt configs, never a legitimately absent one. os.ErrNotExist is
// the distinguishing signal.
func TestServeTierGuard_AbsentConfig_ResolvesSoloControl(t *testing.T) {
	dgHome := t.TempDir()
	t.Setenv("DONTGUESS_RELAY_URLS", "")
	t.Setenv("DONTGUESS_RELAY_URL", "")

	// No config written — LoadConfig will hit os.ErrNotExist.
	if _, statErr := os.Stat(exchange.ConfigPath(dgHome)); statErr == nil {
		t.Fatalf("precondition: config unexpectedly present at %s", exchange.ConfigPath(dgHome))
	}

	operatorIdentity, err := loadOrCreateNostrOperatorIdentity(dgHome)
	if err != nil {
		t.Fatalf("loadOrCreateNostrOperatorIdentity: %v", err)
	}
	tier, relayURLs, rerr := resolveServeTierAndRelays(dgHome, operatorIdentity, discardLogger())
	if rerr != nil {
		t.Fatalf("absent config: resolver returned error %v — an ABSENT config must resolve solo, not fail closed", rerr)
	}
	if tier != exchange.TierSolo {
		t.Errorf("absent config: tier = %q, want %q", tier, exchange.TierSolo)
	}
	if len(relayURLs) != 0 {
		t.Errorf("absent config: relayURLs = %v, want empty", relayURLs)
	}
}

// TestServeTierGuard_NoBakedInDefault proves NO relay endpoint is ever
// substituted: a clean env+config yields an empty relay set, and the team guard
// ERRORS rather than inventing an endpoint.
func TestServeTierGuard_NoBakedInDefault(t *testing.T) {
	t.Setenv("DONTGUESS_RELAY_URLS", "")
	t.Setenv("DONTGUESS_RELAY_URL", "")

	if urls := resolveRelayURLs(); len(urls) != 0 {
		t.Fatalf("clean env: resolveRelayURLs() = %v, want empty — no baked-in default relay", urls)
	}

	// The team guard errors on an empty relay set — it never substitutes an endpoint.
	if err := assertTierHasRelay(exchange.TierTeam, nil); err == nil {
		t.Fatal("assertTierHasRelay(team, nil) returned nil — a default relay was substituted instead of failing closed")
	}
	if err := assertTierHasRelay(exchange.TierFleet, nil); err == nil {
		t.Fatal("assertTierHasRelay(fleet, nil) returned nil — a default relay was substituted instead of failing closed")
	}
	// Solo (and the undeclared tier) never require a relay.
	if err := assertTierHasRelay(exchange.TierSolo, nil); err != nil {
		t.Errorf("assertTierHasRelay(solo, nil) = %v, want nil (solo is zero-ceremony)", err)
	}
	if err := assertTierHasRelay(exchange.Tier(""), nil); err != nil {
		t.Errorf("assertTierHasRelay(undeclared, nil) = %v, want nil (undeclared → solo)", err)
	}

	// effectiveRelayURLs never invents an endpoint from two empty inputs.
	if got := effectiveRelayURLs(nil, nil); len(got) != 0 {
		t.Errorf("effectiveRelayURLs(nil, nil) = %v, want empty", got)
	}
}
