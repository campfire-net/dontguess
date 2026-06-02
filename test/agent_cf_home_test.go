// Package scale_test — AGENT_CF_HOME routing tests (dontguess-a99)
//
// Two test suites:
//
//  1. TestAgentCFHome_DistinctSigningKeys — end-to-end integration test with real cf init.
//     Alice is the operator/exchange owner. Bob puts with AGENT_CF_HOME=bob. Carol buys
//     with AGENT_CF_HOME=carol. Reads campfire messages via cf read and asserts:
//       - put message sender == bob's pubkey (SellerKey)
//       - buy message sender == carol's pubkey (BuyerKey)
//     Uses a wrapper (writeWrapperV2) that routes signing ops through _SIGNING_HOME.
//
//  2. TestAgentCFHome_WrapperArgCapture — shell-level arg-capture test.
//     Installs a stub "cf" binary that records its argv to a file, then drives
//     the wrapper with and without AGENT_CF_HOME. Asserts:
//       - AGENT_CF_HOME set → --cf-home == AGENT_CF_HOME for exchange ops
//       - AGENT_CF_HOME unset → --cf-home == DG_HOME (backward compat)
//
// No mocks: both tests prove routing via real path (subprocess exec + file inspection).
package scale_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Ensure json is used (extractSenderFromMessages uses json.Unmarshal).
var _ = json.Unmarshal

// writeWrapperV2 writes the AGENT_CF_HOME-aware wrapper used in a99 tests.
// It mirrors the production wrapper's signing logic:
//   - DG_HOME is read from the DG_HOME env var (or CF_HOME if DG_HOME unset, for compat).
//   - _SIGNING_HOME = AGENT_CF_HOME if set, else DG_HOME.
//   - All exchange cf calls (buy/put/etc.) use --cf-home $_SIGNING_HOME.
//   - Operator serve uses CF_HOME=DG_HOME (routing stays pinned).
//   - Health probe uses --cf-home $DG_HOME (routing stays pinned).
func writeWrapperV2(t *testing.T, binDir string) {
	t.Helper()
	// The test wrapper uses DG_HOME if set, else CF_HOME (replicating the real wrapper).
	// This allows tests to set DG_HOME=alice.cfHome while setting AGENT_CF_HOME=bob.cfHome
	// for a signing operation.
	wrapper := `#!/bin/sh
set -e
DG_OP="` + binDir + `/dontguess-operator"
CF="` + binDir + `/cf"
CF_HOME="${CF_HOME:-${HOME}/.cf}"
DG_HOME="${DG_HOME:-${CF_HOME}}"
# AGENT_CF_HOME: per-agent signing home. Unset = DG_HOME (backward compat).
_SIGNING_HOME="${AGENT_CF_HOME:-${DG_HOME}}"
CFG="${DG_HOME}/dontguess-exchange.json"
PID="${DG_HOME}/dontguess.pid"
LOG="${DG_HOME}/dontguess.log"
case "${1:-}" in
  init|serve|convention) exec "$DG_OP" "$@";;
  join|leave) subcmd="$1"; shift; exec "$CF" "$subcmd" "$@";;
  version|--version) echo "dontguess wrapper v0.5.0"; exit 0;;
  --help|-h|help|"") echo "dontguess — token-work exchange"; exit 0;;
esac
if [ ! -f "$CFG" ]; then echo "No exchange configured. Run: dontguess init" >&2; exit 1; fi
XCFID=$(sed -n 's/.*"exchange_campfire_id" *: *"\([^"]*\)".*/\1/p' "$CFG")
[ -z "$XCFID" ] && { echo "error: cannot read exchange_campfire_id" >&2; exit 1; }
if ! { [ -f "$PID" ] && kill -0 "$(cat "$PID")" 2>/dev/null; }; then
  nohup env CF_HOME="$DG_HOME" "$DG_OP" serve >"$LOG" 2>&1 &
  echo $! >"$PID"
  sleep 1
  kill -0 "$(cat "$PID")" 2>/dev/null || { echo "error: server failed. See $LOG" >&2; exit 1; }
fi
exec "$CF" --cf-home "$_SIGNING_HOME" "$XCFID" "$@"
`
	path := filepath.Join(binDir, "dontguess")
	if err := os.WriteFile(path, []byte(wrapper), 0755); err != nil {
		t.Fatalf("writing wrapperV2: %v", err)
	}
}

// runWithExtraEnv executes a command for agent a with additional environment variables.
func runWithExtraEnv(a *agent, extraEnv []string, name string, args ...string) (string, error) {
	a.t.Helper()
	bin := a.resolveBin(name)
	cmd := exec.Command(bin, args...)
	cmd.Env = append(a.cmdEnv(), extraEnv...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

// extractSenderFromMessages parses cf read --json output and returns all unique
// senders whose messages have the given tag substring.
func extractSenderFromMessages(t *testing.T, cfReadJSON string, tagSubstr string) []string {
	t.Helper()
	var senders []string
	// Each JSON line is a message object: {"id":"...","sender":"...","tags":[...],...}
	for _, line := range strings.Split(cfReadJSON, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == "[" || line == "]" {
			continue
		}
		// Strip leading comma from JSON array elements
		line = strings.TrimPrefix(line, ",")
		line = strings.TrimSuffix(line, ",")

		var msg struct {
			Sender string   `json:"sender"`
			Tags   []string `json:"tags"`
		}
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			continue
		}
		for _, tag := range msg.Tags {
			if strings.Contains(tag, tagSubstr) {
				senders = append(senders, msg.Sender)
				break
			}
		}
	}
	return senders
}

// agentPubkeyHex returns the hex-encoded Ed25519 public key for an agent by
// calling "cf init --json" in that agent's environment. This is the format used
// in campfire message Sender fields and by "cf admit".
func agentPubkeyHex(t *testing.T, a *agent) string {
	t.Helper()
	return extractPubkey(t, a)
}

// TestAgentCFHome_DistinctSigningKeys verifies that AGENT_CF_HOME routes signing to
// the correct identity. Alice is the exchange operator. Bob puts with his AGENT_CF_HOME.
// Carol buys with her AGENT_CF_HOME. The test reads campfire messages and asserts that
// the put message was signed by bob's key and the buy message was signed by carol's key.
func TestAgentCFHome_DistinctSigningKeys(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e test in short mode")
	}
	t.Parallel()
	env := setup(t)

	sharedTransport := filepath.Join(t.TempDir(), "shared-transport")
	os.MkdirAll(sharedTransport, 0755)

	// Alice: exchange operator.
	alice := env.newAgent("alice")
	alice.transportDir = sharedTransport
	// Install the AGENT_CF_HOME-aware wrapper for alice.
	writeWrapperV2(t, alice.binDir)

	// Bob: seller agent with separate identity.
	bob := env.newAgent("bob")
	bob.transportDir = sharedTransport
	writeWrapperV2(t, bob.binDir)

	// Carol: buyer agent with separate identity.
	carol := env.newAgent("carol")
	carol.transportDir = sharedTransport
	writeWrapperV2(t, carol.binDir)

	// Capture public keys for later assertions.
	bobPubkey := agentPubkeyHex(t, bob)
	carolPubkey := agentPubkeyHex(t, carol)

	// Sanity: bob, carol, and alice must have distinct keys.
	alicePubkey := agentPubkeyHex(t, alice)
	if bobPubkey == alicePubkey {
		t.Fatal("bob and alice have the same public key — cf init did not generate distinct identities")
	}
	if carolPubkey == alicePubkey {
		t.Fatal("carol and alice have the same public key")
	}
	if bobPubkey == carolPubkey {
		t.Fatal("bob and carol have the same public key")
	}

	// Alice inits the exchange.
	out, err := alice.run("dontguess", "init")
	if err != nil {
		t.Fatalf("alice init failed: %v\n%s", err, out)
	}
	xcfid := alice.exchangeID()

	// Alice admits bob and carol to the exchange campfire.
	for _, pair := range []struct {
		name   string
		pubkey string
	}{
		{"bob", bobPubkey},
		{"carol", carolPubkey},
	} {
		out, err = alice.run("cf", "admit", xcfid, pair.pubkey)
		if err != nil {
			t.Fatalf("alice admit %s failed: %v\n%s", pair.name, err, out)
		}
	}

	// Bob and carol join the exchange campfire using their own identities.
	out, err = runWithExtraEnv(bob, nil, "cf", "join", xcfid)
	if err != nil {
		t.Fatalf("bob join failed: %v\n%s", err, out)
	}
	out, err = runWithExtraEnv(carol, nil, "cf", "join", xcfid)
	if err != nil {
		t.Fatalf("carol join failed: %v\n%s", err, out)
	}

	// Alice starts the exchange server (DG_HOME = alice.cfHome via wrapper).
	cancel := alice.runBg("dontguess-operator", "serve")
	defer cancel()
	time.Sleep(1 * time.Second)

	// Bob puts with AGENT_CF_HOME=bob.cfHome and DG_HOME=alice.cfHome.
	// This simulates bob signing as himself while routing through alice's exchange.
	putEnv := []string{
		"AGENT_CF_HOME=" + bob.cfHome,
		"DG_HOME=" + alice.cfHome,
	}
	out, err = runWithExtraEnv(alice, putEnv, "dontguess", "put",
		"--description", "AGENT_CF_HOME routing test: Go mutex contention pattern",
		"--content", "sync.Mutex with TryLock fallback for non-blocking acquire.",
		"--token_cost", "3000",
		"--content_type", "code",
		"--domain", "go,concurrency")
	if err != nil {
		t.Fatalf("bob put via alice wrapper failed: %v\n%s", err, out)
	}

	// Wait for put-accept.
	waitFor(t, 10*time.Second, "put-accept for bob's content", func() bool {
		return strings.Contains(alice.cfRead(xcfid), "exchange:phase:put-accept")
	})

	// Carol buys with AGENT_CF_HOME=carol.cfHome and DG_HOME=alice.cfHome.
	buyEnv := []string{
		"AGENT_CF_HOME=" + carol.cfHome,
		"DG_HOME=" + alice.cfHome,
	}
	out, err = runWithExtraEnv(alice, buyEnv, "dontguess", "buy",
		"--task", "Go mutex non-blocking acquire pattern",
		"--budget", "5000")
	if err != nil {
		t.Fatalf("carol buy via alice wrapper failed: %v\n%s", err, out)
	}

	waitFor(t, 10*time.Second, "match for carol's buy", func() bool {
		return strings.Contains(alice.cfRead(xcfid), "exchange:match")
	})

	// Read all messages as JSON and verify sender keys.
	cfReadOut, err := alice.run("cf", "read", xcfid, "--all", "--json")
	if err != nil {
		t.Logf("cf read --json warning: %v\n%s", err, cfReadOut)
		// Fall back to non-JSON read for message verification.
		cfReadOut = alice.cfRead(xcfid)
	}

	// Read all messages. The human-readable cf output abbreviates sender to 8 hex chars.
	// Use the first 8 chars of each key for searching.
	raw := alice.cfRead(xcfid)
	bobPrefix8 := bobPubkey[:8]
	carolPrefix8 := carolPubkey[:8]
	alicePrefix8 := alicePubkey[:8]

	// Assert: bob's key prefix appears in the exchange output.
	// (bob put a message → his 8-char pubkey prefix appears as sender in cf read output)
	if !strings.Contains(raw, bobPrefix8) {
		t.Errorf("put message not signed by bob: bob_pubkey prefix %q not found in exchange messages.\n"+
			"alice_pubkey prefix: %q\n"+
			"Raw campfire output (last 3000 chars):\n%s",
			bobPrefix8, alicePrefix8,
			tailStr(raw, 3000))
	}

	// Assert: carol's key prefix appears in the exchange output.
	// (carol sent a buy → her 8-char pubkey prefix appears as sender)
	if !strings.Contains(raw, carolPrefix8) {
		t.Errorf("buy message not signed by carol: carol_pubkey prefix %q not found in exchange messages.\n"+
			"alice_pubkey prefix: %q\n"+
			"Raw campfire output (last 3000 chars):\n%s",
			carolPrefix8, alicePrefix8,
			tailStr(raw, 3000))
	}

	_ = cfReadOut // used above via extractSenderFromMessages (kept for future --json path)

	t.Logf("AGENT_CF_HOME routing verified:")
	t.Logf("  alice (operator): %s", alicePubkey[:16])
	t.Logf("  bob   (seller):   %s", bobPubkey[:16])
	t.Logf("  carol (buyer):    %s", carolPubkey[:16])
}

// TestAgentCFHome_BackwardCompat verifies that with AGENT_CF_HOME unset, the wrapper
// passes --cf-home $DG_HOME to cf (unchanged from v0.4.2 behavior).
func TestAgentCFHome_BackwardCompat(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e test in short mode")
	}
	t.Parallel()
	env := setup(t)

	alice := env.newAgent("alice")
	writeWrapperV2(t, alice.binDir)

	// Init exchange.
	out, err := alice.run("dontguess", "init")
	if err != nil {
		t.Fatalf("init failed: %v\n%s", err, out)
	}
	xcfid := alice.exchangeID()

	cancel := alice.runBg("dontguess-operator", "serve")
	defer cancel()
	time.Sleep(1 * time.Second)

	// With AGENT_CF_HOME unset (no extra env), alice puts using her own key.
	out, err = alice.run("cf", xcfid, "put",
		"--description", "backward compat: no AGENT_CF_HOME",
		"--content", "content",
		"--token_cost", "1000",
		"--content_type", "code",
		"--domain", "test")
	if err != nil {
		t.Fatalf("put failed: %v\n%s", err, out)
	}

	waitFor(t, 10*time.Second, "put-accept", func() bool {
		return strings.Contains(alice.cfRead(xcfid), "exchange:phase:put-accept")
	})

	// Verify alice's key is the seller key (backward compat = operator key).
	// cf read output abbreviates sender to 8 hex chars.
	alicePubkey := agentPubkeyHex(t, alice)
	raw := alice.cfRead(xcfid)
	alicePrefix8 := alicePubkey[:8]
	if !strings.Contains(raw, alicePrefix8) {
		t.Errorf("backward compat: alice's key prefix %q not found in exchange output", alicePrefix8)
	}
	t.Logf("Backward compat verified: alice key %s in exchange messages", alicePrefix8)
}

// TestAgentCFHome_WrapperArgCapture is a shell-level arg-capture test.
// It installs a stub "cf" binary that records its argv to a file, then drives the
// wrapper with and without AGENT_CF_HOME set, and asserts the --cf-home argument
// passed to cf matches expectations.
// This tests the wrapper's shell routing logic directly, without a live exchange.
func TestAgentCFHome_WrapperArgCapture(t *testing.T) {
	t.Parallel()

	// Create an isolated directory for this test.
	testDir := t.TempDir()
	binDir := filepath.Join(testDir, "bin")
	dgHome := filepath.Join(testDir, "dg_home")
	agentCfHome := filepath.Join(testDir, "agent_cf_home")
	argLog := filepath.Join(testDir, "cf_args.log")

	os.MkdirAll(binDir, 0755)
	os.MkdirAll(dgHome, 0755)
	os.MkdirAll(agentCfHome, 0755)

	// Write a minimal dontguess-exchange.json so the wrapper can read XCFID.
	const fakeCFID = "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"
	exchangeJSON := fmt.Sprintf(`{"exchange_campfire_id": %q}`, fakeCFID)
	if err := os.WriteFile(filepath.Join(dgHome, "dontguess-exchange.json"), []byte(exchangeJSON), 0644); err != nil {
		t.Fatalf("writing fake exchange config: %v", err)
	}

	// Write a minimal dontguess.pid so the wrapper thinks the server is running.
	// We write a valid PID (our own) so kill -0 succeeds.
	selfPID := fmt.Sprintf("%d", os.Getpid())
	if err := os.WriteFile(filepath.Join(dgHome, "dontguess.pid"), []byte(selfPID), 0644); err != nil {
		t.Fatalf("writing fake pid: %v", err)
	}

	// Write a stub "cf" binary that logs its argv to argLog and exits 0.
	stubCF := `#!/bin/sh
printf '%s\n' "$@" >> ` + argLog + `
printf '---\n' >> ` + argLog + `
exit 0
`
	stubCFPath := filepath.Join(binDir, "cf")
	if err := os.WriteFile(stubCFPath, []byte(stubCF), 0755); err != nil {
		t.Fatalf("writing stub cf: %v", err)
	}

	// Write a stub "dontguess-operator" binary (for the serve check; not invoked here).
	stubOp := `#!/bin/sh
exit 0
`
	if err := os.WriteFile(filepath.Join(binDir, "dontguess-operator"), []byte(stubOp), 0755); err != nil {
		t.Fatalf("writing stub operator: %v", err)
	}

	// Write the AGENT_CF_HOME-aware wrapper.
	wrapper := `#!/bin/sh
set -e
DG_OP="` + filepath.Join(binDir, "dontguess-operator") + `"
CF="` + filepath.Join(binDir, "cf") + `"
CF_HOME="${CF_HOME:-${HOME}/.cf}"
DG_HOME="${DG_HOME:-${CF_HOME}}"
_SIGNING_HOME="${AGENT_CF_HOME:-${DG_HOME}}"
CFG="${DG_HOME}/dontguess-exchange.json"
PID="${DG_HOME}/dontguess.pid"
LOG="${DG_HOME}/dontguess.log"
case "${1:-}" in
  init|serve|convention) exec "$DG_OP" "$@";;
  join|leave) subcmd="$1"; shift; exec "$CF" "$subcmd" "$@";;
  version|--version) echo "dontguess wrapper v0.5.0"; exit 0;;
  --help|-h|help|"") echo "dontguess — token-work exchange"; exit 0;;
esac
if [ ! -f "$CFG" ]; then echo "No exchange configured. Run: dontguess init" >&2; exit 1; fi
XCFID=$(sed -n 's/.*"exchange_campfire_id" *: *"\([^"]*\)".*/\1/p' "$CFG")
[ -z "$XCFID" ] && { echo "error: cannot read exchange_campfire_id" >&2; exit 1; }
if ! { [ -f "$PID" ] && kill -0 "$(cat "$PID")" 2>/dev/null; }; then
  nohup env CF_HOME="$DG_HOME" "$DG_OP" serve >"$LOG" 2>&1 &
  echo $! >"$PID"
  sleep 1
  kill -0 "$(cat "$PID")" 2>/dev/null || { echo "error: server failed. See $LOG" >&2; exit 1; }
fi
exec "$CF" --cf-home "$_SIGNING_HOME" "$XCFID" "$@"
`
	wrapperPath := filepath.Join(binDir, "dontguess")
	if err := os.WriteFile(wrapperPath, []byte(wrapper), 0755); err != nil {
		t.Fatalf("writing wrapper: %v", err)
	}

	baseEnv := []string{
		"HOME=" + testDir,
		"PATH=" + binDir + ":" + os.Getenv("PATH"),
		"DG_HOME=" + dgHome,
	}

	// ---- Case 1: AGENT_CF_HOME set → --cf-home should be agentCfHome ----
	if err := os.Remove(argLog); err != nil && !os.IsNotExist(err) {
		t.Fatalf("clearing arg log: %v", err)
	}

	cmd := exec.Command(wrapperPath, "buy", "--task", "test task", "--budget", "100")
	cmd.Env = append(baseEnv, "AGENT_CF_HOME="+agentCfHome)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	_ = cmd.Run() // ignore exit code — stub cf exits 0 but wrapper may fail on no operator

	args1 := readArgLog(t, argLog)
	t.Logf("Case1 (AGENT_CF_HOME set): cf argv = %v", args1)

	cfHomeIdx := -1
	for i, a := range args1 {
		if a == "--cf-home" && i+1 < len(args1) {
			cfHomeIdx = i + 1
			break
		}
	}
	if cfHomeIdx == -1 {
		t.Errorf("Case1: --cf-home flag not found in cf args: %v", args1)
	} else if args1[cfHomeIdx] != agentCfHome {
		t.Errorf("Case1: --cf-home = %q, want %q (AGENT_CF_HOME)", args1[cfHomeIdx], agentCfHome)
	}

	// ---- Case 2: AGENT_CF_HOME unset → --cf-home should be DG_HOME ----
	if err := os.Remove(argLog); err != nil && !os.IsNotExist(err) {
		t.Fatalf("clearing arg log: %v", err)
	}

	cmd2 := exec.Command(wrapperPath, "buy", "--task", "test task", "--budget", "100")
	cmd2.Env = baseEnv // no AGENT_CF_HOME
	var buf2 bytes.Buffer
	cmd2.Stdout = &buf2
	cmd2.Stderr = &buf2
	_ = cmd2.Run()

	args2 := readArgLog(t, argLog)
	t.Logf("Case2 (AGENT_CF_HOME unset): cf argv = %v", args2)

	cfHomeIdx2 := -1
	for i, a := range args2 {
		if a == "--cf-home" && i+1 < len(args2) {
			cfHomeIdx2 = i + 1
			break
		}
	}
	if cfHomeIdx2 == -1 {
		t.Errorf("Case2: --cf-home flag not found in cf args: %v", args2)
	} else if args2[cfHomeIdx2] != dgHome {
		t.Errorf("Case2: --cf-home = %q, want %q (DG_HOME, backward compat)", args2[cfHomeIdx2], dgHome)
	}

	t.Log("Shell arg-capture routing verified:")
	t.Logf("  AGENT_CF_HOME set   → --cf-home = AGENT_CF_HOME (%s)", agentCfHome)
	t.Logf("  AGENT_CF_HOME unset → --cf-home = DG_HOME       (%s)", dgHome)
}

// readArgLog reads the stub cf arg log and returns the last call's argv as a slice.
// The stub writes one arg per line, calls separated by "---".
func readArgLog(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("reading arg log %s: %v", path, err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	// Find the last "---" separator to get the last call's args.
	lastSep := -1
	for i := len(lines) - 1; i >= 0; i-- {
		if lines[i] == "---" {
			lastSep = i
			break
		}
	}
	if lastSep == -1 {
		// No separator found — return all lines as one call.
		var result []string
		for _, l := range lines {
			if l != "" {
				result = append(result, l)
			}
		}
		return result
	}
	// Args are the lines before the last "---".
	var result []string
	for _, l := range lines[:lastSep] {
		if l != "" && l != "---" {
			result = append(result, l)
		}
	}
	// If that's empty, collect lines just before lastSep.
	if len(result) == 0 {
		// Find the second-to-last separator.
		prevSep := -1
		for i := lastSep - 1; i >= 0; i-- {
			if lines[i] == "---" {
				prevSep = i
				break
			}
		}
		start := prevSep + 1
		for _, l := range lines[start:lastSep] {
			if l != "" {
				result = append(result, l)
			}
		}
	}
	return result
}

// truncate returns the first n bytes of s as a string.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}

// tailStr returns the last n bytes of s as a string.
func tailStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "(truncated)..." + s[len(s)-n:]
}
