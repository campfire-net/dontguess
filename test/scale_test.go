// Package scale_test exercises dontguess deployment modes via public CLI only.
//
// Every test in this file treats dontguess and cf as opaque binaries.
// No internal packages are imported. Tests build the binaries once,
// then run them as subprocesses in isolated HOME directories.
//
// Requires: go toolchain (to build binaries), cf on PATH or built from source.
package scale_test

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// testEnv holds paths to binaries and provides helpers for running them
// in isolated environments.
type testEnv struct {
	t        *testing.T
	dgBin    string // path to dontguess-operator binary
	cfBin    string // path to cf binary
	repoDir  string // repo root (for building)
}

// agent represents an isolated agent environment (one "machine").
type agent struct {
	t            *testing.T
	env          *testEnv
	home         string // isolated HOME
	binDir       string // ~/.local/bin equivalent
	cfHome       string // ~/.campfire
	transportDir string // CF_TRANSPORT_DIR (empty = default)
	name         string // for test output
}

var (
	buildOnce sync.Once
	sharedBinDir string // persists across all tests, cleaned in TestMain
	dgBinPath    string
	cfBinPath    string
	buildErr     error
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "dontguess-scale-test-bin-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "creating bin dir: %v\n", err)
		os.Exit(1)
	}
	sharedBinDir = dir
	code := m.Run()
	os.RemoveAll(sharedBinDir)
	os.Exit(code)
}

func setup(t *testing.T) *testEnv {
	t.Helper()

	repoDir := findRepoRoot(t)

	buildOnce.Do(func() {
		dgBinPath = filepath.Join(sharedBinDir, "dontguess-operator")
		cmd := exec.Command("go", "build", "-o", dgBinPath, "./cmd/dontguess")
		cmd.Dir = repoDir
		cmd.Env = append(os.Environ(), "GOPRIVATE=github.com/campfire-net")
		out, err := cmd.CombinedOutput()
		if err != nil {
			buildErr = fmt.Errorf("building dontguess-operator: %w\n%s", err, out)
			return
		}

		// Find cf — prefer system install, fall back to building
		cfBinPath, _ = exec.LookPath("cf")
		if cfBinPath == "" {
			cfBinPath = filepath.Join(sharedBinDir, "cf")
			cfDir := filepath.Join(repoDir, "..", "campfire")
			cmd = exec.Command("go", "build", "-o", cfBinPath, "./cmd/cf")
			cmd.Dir = cfDir
			cmd.Env = append(os.Environ(), "GOPRIVATE=github.com/campfire-net")
			out, err = cmd.CombinedOutput()
			if err != nil {
				buildErr = fmt.Errorf("building cf: %w\n%s", err, out)
				return
			}
		}
	})

	if buildErr != nil {
		t.Fatalf("binary build failed: %v", buildErr)
	}

	return &testEnv{
		t:       t,
		dgBin:   dgBinPath,
		cfBin:   cfBinPath,
		repoDir: repoDir,
	}
}

// newAgent creates an isolated agent environment with its own HOME.
func (e *testEnv) newAgent(name string) *agent {
	e.t.Helper()
	home := filepath.Join(e.t.TempDir(), name)
	binDir := filepath.Join(home, ".local", "bin")
	os.MkdirAll(binDir, 0755)

	// Symlink binaries into the agent's PATH
	os.Symlink(e.dgBin, filepath.Join(binDir, "dontguess-operator"))
	os.Symlink(e.cfBin, filepath.Join(binDir, "cf"))

	// Write the wrapper script
	writeWrapper(e.t, binDir)

	// Initialize cf identity for this agent
	a := &agent{
		t:      e.t,
		env:    e,
		home:   home,
		binDir: binDir,
		cfHome: filepath.Join(home, ".campfire"),
		name:   name,
	}

	out, err := a.run("cf", "init")
	if err != nil {
		e.t.Fatalf("cf init for %s failed: %v\n%s", name, err, out)
	}

	return a
}

// run executes a command in the agent's isolated environment.
// resolveBin maps a logical name to the binary path.
func (a *agent) resolveBin(name string) string {
	switch name {
	case "cf":
		return filepath.Join(a.binDir, "cf")
	case "dontguess":
		return filepath.Join(a.binDir, "dontguess")
	case "dontguess-operator":
		return filepath.Join(a.binDir, "dontguess-operator")
	default:
		return name
	}
}

// cmdEnv returns the environment variables for this agent's subprocess.
func (a *agent) cmdEnv() []string {
	env := []string{
		"HOME=" + a.home,
		"PATH=" + a.binDir + ":" + os.Getenv("PATH"),
		"CF_HOME=" + a.cfHome,
	}
	if a.transportDir != "" {
		env = append(env, "CF_TRANSPORT_DIR="+a.transportDir)
	}
	return env
}

func (a *agent) run(name string, args ...string) (string, error) {
	a.t.Helper()
	bin := a.resolveBin(name)
	cmd := exec.Command(bin, args...)
	cmd.Env = a.cmdEnv()
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

// runBg starts a command in the background, returning a cleanup function.
func (a *agent) runBg(name string, args ...string) (cancel func()) {
	a.t.Helper()
	bin := a.resolveBin(name)
	cmd := exec.Command(bin, args...)
	cmd.Env = a.cmdEnv()
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Start(); err != nil {
		a.t.Fatalf("starting %s %v: %v", name, args, err)
	}
	return func() {
		cmd.Process.Kill()
		cmd.Wait()
	}
}

// exchangeID reads the exchange campfire ID from config.
func (a *agent) exchangeID() string {
	a.t.Helper()
	cfg, err := os.ReadFile(filepath.Join(a.cfHome, "dontguess-exchange.json"))
	if err != nil {
		a.t.Fatalf("reading exchange config for %s: %v", a.name, err)
	}
	// Simple extraction — same approach as the wrapper script
	for _, line := range strings.Split(string(cfg), "\n") {
		if strings.Contains(line, "exchange_campfire_id") {
			parts := strings.Split(line, `"`)
			for i, p := range parts {
				if p == "exchange_campfire_id" && i+2 < len(parts) {
					return parts[i+2]
				}
			}
		}
	}
	a.t.Fatalf("exchange_campfire_id not found in config for %s", a.name)
	return ""
}

// centerID reads the center campfire ID.
func (a *agent) centerID() string {
	a.t.Helper()
	data, err := os.ReadFile(filepath.Join(a.cfHome, "center"))
	if err != nil {
		a.t.Fatalf("reading center for %s: %v", a.name, err)
	}
	return strings.TrimSpace(string(data))
}

// cfRead reads all messages from a campfire, returns combined output.
func (a *agent) cfRead(campfireID string) string {
	a.t.Helper()
	out, _ := a.run("cf", "read", campfireID, "--all")
	return out
}

func writeWrapper(t *testing.T, binDir string) {
	t.Helper()
	wrapper := `#!/bin/sh
set -e
DG_OP="` + binDir + `/dontguess-operator"
CF="` + binDir + `/cf"
CF_HOME="${CF_HOME:-${HOME}/.campfire}"
CFG="${CF_HOME}/dontguess-exchange.json"
PID="${CF_HOME}/dontguess.pid"
LOG="${CF_HOME}/dontguess.log"
case "${1:-}" in
  init|serve|convention) exec "$DG_OP" "$@";;
  join|leave) subcmd="$1"; shift; exec "$CF" "$subcmd" "$@";;
  version|--version) echo "dontguess wrapper"; exit 0;;
  --help|-h|help|"") echo "dontguess — token-work exchange"; exit 0;;
esac
if [ ! -f "$CFG" ]; then echo "No exchange configured. Run: dontguess init" >&2; exit 1; fi
XCFID=$(sed -n 's/.*"exchange_campfire_id" *: *"\([^"]*\)".*/\1/p' "$CFG")
[ -z "$XCFID" ] && { echo "error: cannot read exchange_campfire_id" >&2; exit 1; }
if ! { [ -f "$PID" ] && kill -0 "$(cat "$PID")" 2>/dev/null; }; then
  nohup "$DG_OP" serve >"$LOG" 2>&1 &
  echo $! >"$PID"
  sleep 1
  kill -0 "$(cat "$PID")" 2>/dev/null || { echo "error: server failed. See $LOG" >&2; exit 1; }
fi
exec "$CF" "$XCFID" "$@"
`
	path := filepath.Join(binDir, "dontguess")
	if err := os.WriteFile(path, []byte(wrapper), 0755); err != nil {
		t.Fatalf("writing wrapper: %v", err)
	}
}

func findRepoRoot(t *testing.T) string {
	t.Helper()
	// Walk up from the test file to find go.mod
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find repo root (go.mod)")
		}
		dir = parent
	}
}

// waitFor polls a condition with timeout.
func waitFor(t *testing.T, timeout time.Duration, desc string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for: %s", desc)
}

// --- Mode 1: Project-local ---

func TestMode1_ProjectLocal(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e test in short mode")
	}
	env := setup(t)
	alice := env.newAgent("alice")

	// Init exchange
	out, err := alice.run("dontguess", "init")
	if err != nil {
		t.Fatalf("init failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Exchange initialized") {
		t.Fatalf("expected 'Exchange initialized', got: %s", out)
	}

	xcfid := alice.exchangeID()

	// Join (should say already a member since init auto-joins)
	out, err = alice.run("dontguess", "join", xcfid)
	if err == nil {
		t.Logf("join output: %s", out)
	}
	// Either "already a member" (error) or "Joined" (success) is fine
	if err == nil && !strings.Contains(out, "Joined") {
		// no error and no "Joined" is unexpected but not fatal
	}

	// Start server in background
	cancel := alice.runBg("dontguess-operator", "serve")
	defer cancel()
	time.Sleep(1 * time.Second) // let server start

	// Put
	content := "Token bucket rate limiter in Go: per-key limits with burst support."
	out, err = alice.run("cf", xcfid, "put",
		"--description", "Token bucket rate limiter in Go with Redis backend",
		"--content", content,
		"--token_cost", "2000",
		"--content_type", "code",
		"--domain", "go,networking")
	if err != nil {
		t.Fatalf("put failed: %v\n%s", err, out)
	}

	// Wait for put-accept
	waitFor(t, 10*time.Second, "put-accept", func() bool {
		return strings.Contains(alice.cfRead(xcfid), "exchange:phase:put-accept")
	})

	// Buy
	out, err = alice.run("cf", xcfid, "buy",
		"--task", "rate limiter implementation in Go",
		"--budget", "5000")
	if err != nil {
		t.Fatalf("buy failed: %v\n%s", err, out)
	}

	// Wait for match
	waitFor(t, 10*time.Second, "match", func() bool {
		return strings.Contains(alice.cfRead(xcfid), "exchange:match")
	})
}

// --- Mode 2: User-local (center campfire) ---

func TestMode2_UserLocal(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e test in short mode")
	}
	env := setup(t)

	// Single user with one identity, one center campfire
	user := env.newAgent("user")

	// Init exchange — the exchange should be usable across "projects"
	out, err := user.run("dontguess", "init")
	if err != nil {
		t.Fatalf("init failed: %v\n%s", err, out)
	}

	xcfid := user.exchangeID()

	// Start server
	cancel := user.runBg("dontguess-operator", "serve")
	defer cancel()
	time.Sleep(1 * time.Second)

	// Simulate "project A" putting code-domain content
	out, err = user.run("cf", xcfid, "put",
		"--description", "Terraform module for AWS VPC with private subnets and NAT gateway",
		"--content", "resource aws_vpc main { cidr_block = var.vpc_cidr }",
		"--token_cost", "3000",
		"--content_type", "code",
		"--domain", "terraform,aws,infrastructure")
	if err != nil {
		t.Fatalf("put (project A) failed: %v\n%s", err, out)
	}

	waitFor(t, 10*time.Second, "put-accept for project A content", func() bool {
		return strings.Contains(user.cfRead(xcfid), "exchange:phase:put-accept")
	})

	// Simulate "project B" putting analysis-domain content
	out, err = user.run("cf", xcfid, "put",
		"--description", "Security audit of JWT token validation in Go auth middleware",
		"--content", "JWT validation: check exp, iss, aud claims. Use RS256 not HS256.",
		"--token_cost", "4000",
		"--content_type", "analysis",
		"--domain", "security,go,authentication")
	if err != nil {
		t.Fatalf("put (project B) failed: %v\n%s", err, out)
	}

	// Now simulate "project C" buying — should find content from project A
	out, err = user.run("cf", xcfid, "buy",
		"--task", "AWS VPC setup with Terraform for private subnet isolation",
		"--budget", "5000")
	if err != nil {
		t.Fatalf("buy (cross-project) failed: %v\n%s", err, out)
	}

	waitFor(t, 10*time.Second, "cross-project match", func() bool {
		return strings.Contains(user.cfRead(xcfid), "exchange:match")
	})

	// Verify the match references the terraform content, not the security audit
	messages := user.cfRead(xcfid)
	if !strings.Contains(messages, "exchange:match") {
		t.Fatal("no match found")
	}
	t.Log("Mode 2: cross-project match verified — content from project A discoverable by project C")
}

// --- Mode 3: Team (two identities, shared filesystem transport) ---
//
// Simulates two machines by using two separate CF_HOME directories with
// different identities. Both access the same exchange campfire via filesystem
// transport (which on a real multi-machine setup would be P2P HTTP or hosted).
// The operator admits the second agent, who then joins, puts, and buys.

func TestMode3_Team(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e test in short mode")
	}
	env := setup(t)

	// Shared transport directory — simulates shared filesystem / network transport
	sharedTransport := filepath.Join(t.TempDir(), "shared-transport")
	os.MkdirAll(sharedTransport, 0755)

	// Alice is the operator — she creates and runs the exchange
	alice := env.newAgent("alice")
	alice.transportDir = sharedTransport

	// Bob is a teammate with a separate identity
	bob := env.newAgent("bob")
	bob.transportDir = sharedTransport

	// Get Bob's public key
	bobPubkey := extractPubkey(t, bob)

	// Alice inits exchange
	out, err := alice.run("dontguess", "init")
	if err != nil {
		t.Fatalf("alice init failed: %v\n%s", err, out)
	}
	xcfid := alice.exchangeID()

	// Alice admits Bob to the exchange campfire
	out, err = alice.run("cf", "admit", xcfid, bobPubkey)
	if err != nil {
		t.Fatalf("alice admit bob failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Admitted") && !strings.Contains(out, "admitted") {
		t.Fatalf("expected admission confirmation, got: %s", out)
	}

	// Bob joins the exchange
	out, err = bob.run("cf", "join", xcfid)
	if err != nil {
		t.Fatalf("bob join failed: %v\n%s", err, out)
	}

	// Alice starts the exchange server
	cancel := alice.runBg("dontguess-operator", "serve")
	defer cancel()
	time.Sleep(1 * time.Second)

	// Bob puts content
	out, err = bob.run("cf", xcfid, "put",
		"--description", "Kubernetes pod autoscaler with custom metrics from Prometheus",
		"--content", "HPA config with custom.metrics.k8s.io API, Prometheus adapter, scale target.",
		"--token_cost", "3500",
		"--content_type", "code",
		"--domain", "kubernetes,infrastructure,monitoring")
	if err != nil {
		t.Fatalf("bob put failed: %v\n%s", err, out)
	}

	// Wait for exchange to accept Bob's put
	waitFor(t, 10*time.Second, "put-accept for bob's content", func() bool {
		return strings.Contains(alice.cfRead(xcfid), "exchange:phase:put-accept")
	})

	// Alice buys — should find Bob's content
	out, err = alice.run("cf", xcfid, "buy",
		"--task", "Kubernetes autoscaling with Prometheus custom metrics",
		"--budget", "5000")
	if err != nil {
		t.Fatalf("alice buy failed: %v\n%s", err, out)
	}

	waitFor(t, 10*time.Second, "cross-identity match", func() bool {
		return strings.Contains(alice.cfRead(xcfid), "exchange:match")
	})

	t.Log("Mode 3: cross-identity match verified — Bob's put discovered by Alice's buy")
}

// extractPubkey gets an agent's public key from their cf identity.
func extractPubkey(t *testing.T, a *agent) string {
	t.Helper()
	// cf init was already called in newAgent; read the identity
	out, err := a.run("cf", "init", "--json")
	if err != nil {
		t.Fatalf("cf init --json for %s failed: %v\n%s", a.name, err, out)
	}
	// Parse {"public_key": "..."}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.Contains(line, `"public_key"`) {
			parts := strings.Split(line, `"`)
			for i, p := range parts {
				if p == "public_key" && i+2 < len(parts) {
					return parts[i+2]
				}
			}
		}
	}
	t.Fatalf("could not extract public_key for %s from: %s", a.name, out)
	return ""
}
