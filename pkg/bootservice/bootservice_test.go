package bootservice

import (
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

// TestRenderUnitResolvesPathsNotHardcoded asserts the unit content carries
// the CALLER-supplied serve binary + DG_HOME paths verbatim — never a
// hardcoded path — and rejects non-absolute inputs (dontguess-748
// CONSTRAINTS).
func TestRenderUnitResolvesPathsNotHardcoded(t *testing.T) {
	opts := Options{
		ServeBinary: "/opt/custom/bin/dontguess",
		DGHome:      "/srv/weird-operator-home/.dontguess",
		RelayURLs:   []string{"ws://192.168.2.40:7777", "ws://192.168.2.41:7777"},
	}
	content, err := RenderUnit(opts)
	if err != nil {
		t.Fatalf("RenderUnit: %v", err)
	}
	if !strings.Contains(content, `ExecStart="/opt/custom/bin/dontguess" serve`) {
		t.Errorf("unit does not reference caller-resolved ServeBinary:\n%s", content)
	}
	if !strings.Contains(content, `Environment="DG_HOME=/srv/weird-operator-home/.dontguess"`) {
		t.Errorf("unit does not reference caller-resolved DGHome:\n%s", content)
	}
	if !strings.Contains(content, `Environment="DONTGUESS_RELAY_URLS=ws://192.168.2.40:7777,ws://192.168.2.41:7777"`) {
		t.Errorf("unit does not reference caller-resolved RelayURLs:\n%s", content)
	}
	// No path literal from a DIFFERENT operator/home should ever appear —
	// guards against a future regression that hardcodes a dev-machine path.
	if strings.Contains(content, "/home/") {
		t.Errorf("unit content unexpectedly contains a /home/ literal not supplied via Options:\n%s", content)
	}
}

func TestRenderUnitRejectsRelativePaths(t *testing.T) {
	cases := []Options{
		{ServeBinary: "dontguess", DGHome: "/abs/home"},
		{ServeBinary: "/abs/bin/dontguess", DGHome: "relative/home"},
		{ServeBinary: "", DGHome: "/abs/home"},
		{ServeBinary: "/abs/bin/dontguess", DGHome: ""},
	}
	for i, opts := range cases {
		if _, err := RenderUnit(opts); err == nil {
			t.Errorf("case %d: expected error for %+v, got nil", i, opts)
		}
	}
}

// TestRenderUnitQuotesSpaceInPath is the GROUND-SOURCE test for
// dontguess-983: a ServeBinary/DGHome path containing a space (valid on
// Linux) must render as a unit systemd parses as ONE token/ONE
// assignment — not split on the embedded whitespace. We assert this by
// parsing the rendered ExecStart= and Environment= lines with a
// systemd.syntax(7)-shaped quoted-token splitter (whitespace splits
// outside double quotes, backslash/double-quote are the only escapes
// inside quotes) rather than trusting substring containment, which would
// pass even on unquoted, systemd-broken output.
func TestRenderUnitQuotesSpaceInPath(t *testing.T) {
	opts := Options{
		ServeBinary: "/opt/my custom bin/dontguess",
		DGHome:      "/srv/weird operator home/.dontguess",
		RelayURLs:   []string{"ws://192.168.2.40:7777"},
	}
	content, err := RenderUnit(opts)
	if err != nil {
		t.Fatalf("RenderUnit: %v", err)
	}

	execLine := findLine(t, content, "ExecStart=")
	tokens := splitSystemdQuoted(execLine[len("ExecStart="):])
	if len(tokens) != 2 {
		t.Fatalf("ExecStart tokenized to %d tokens (want 2 — binary path is being split on the embedded space): %q from line %q", len(tokens), tokens, execLine)
	}
	if tokens[0] != opts.ServeBinary {
		t.Errorf("ExecStart first token = %q, want %q (space-containing ServeBinary)", tokens[0], opts.ServeBinary)
	}
	if tokens[1] != "serve" {
		t.Errorf("ExecStart second token = %q, want %q", tokens[1], "serve")
	}

	dgHomeLine := findLine(t, content, "Environment=\"DG_HOME=")
	dgTokens := splitSystemdQuoted(dgHomeLine[len("Environment="):])
	if len(dgTokens) != 1 {
		t.Fatalf("Environment=DG_HOME tokenized to %d tokens (want 1 — assignment is being split on the embedded space): %q from line %q", len(dgTokens), dgTokens, dgHomeLine)
	}
	wantAssignment := "DG_HOME=" + opts.DGHome
	if dgTokens[0] != wantAssignment {
		t.Errorf("Environment=DG_HOME assignment = %q, want %q", dgTokens[0], wantAssignment)
	}
}

// findLine returns the first line of content containing prefix, or fails
// the test.
func findLine(t *testing.T, content, prefix string) string {
	t.Helper()
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	t.Fatalf("no line with prefix %q in content:\n%s", prefix, content)
	return ""
}

// splitSystemdQuoted splits s into tokens on whitespace OUTSIDE double
// quotes, per systemd.syntax(7) C-style quoting: inside a double-quoted
// token, backslash escapes the next character (only \\ and \" are
// produced by systemdQuoteArg, but this splitter honors any \X per the
// real systemd grammar) and the token ends at the next unescaped ".
func splitSystemdQuoted(s string) []string {
	var tokens []string
	var cur strings.Builder
	inQuotes := false
	hasCur := false
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case inQuotes && c == '\\' && i+1 < len(s):
			cur.WriteByte(s[i+1])
			i += 2
			continue
		case c == '"':
			inQuotes = !inQuotes
			hasCur = true
			i++
			continue
		case !inQuotes && (c == ' ' || c == '\t'):
			if hasCur {
				tokens = append(tokens, cur.String())
				cur.Reset()
				hasCur = false
			}
			i++
			continue
		default:
			cur.WriteByte(c)
			hasCur = true
			i++
		}
	}
	if hasCur {
		tokens = append(tokens, cur.String())
	}
	return tokens
}

func TestRenderUnitOmitsRelayEnvWhenEmpty(t *testing.T) {
	content, err := RenderUnit(Options{ServeBinary: "/abs/bin/dontguess", DGHome: "/abs/home"})
	if err != nil {
		t.Fatalf("RenderUnit: %v", err)
	}
	if strings.Contains(content, "DONTGUESS_RELAY_URLS") {
		t.Errorf("unit should omit DONTGUESS_RELAY_URLS when RelayURLs is empty:\n%s", content)
	}
}

// TestInstallGroundSource is the MANDATORY ground-source test
// (dontguess-748): on a systemd --user capable runner it actually installs
// the unit and enables linger, then asserts via the real `loginctl
// show-user --property=Linger` and `systemctl --user is-enabled` — not a
// mock of either binary. Where systemd --user is genuinely unavailable
// (checked via the SAME systemdUserAvailable() probe Install uses — not
// assumed), it asserts the DryRun path fired instead and skips the
// enabled/linger assertions with that fact stated, not silently passed.
func TestInstallGroundSource(t *testing.T) {
	available, note := systemdUserAvailable()
	if !available {
		t.Skipf("systemd --user unavailable on this runner (%s) — dry-run path covered by TestInstallDryRunWhenSystemdAbsent instead", note)
	}

	// Deliberately use the REAL default unit dir (no UnitDir override) so
	// systemctl's default search path finds the unit by name — a
	// test-scoped UnitDir is invisible to `systemctl --user is-enabled
	// <name>` (it only resolves unit NAMES against the standard search
	// path, not an arbitrary directory), which would defeat the point of
	// a ground-source assertion. Clean up afterward.
	serveBinary := filepath.Join(t.TempDir(), "dontguess")
	dgHome := filepath.Join(t.TempDir(), ".dontguess")

	opts := Options{
		ServeBinary: serveBinary,
		DGHome:      dgHome,
	}

	result, err := Install(opts)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	t.Cleanup(func() {
		_ = exec.Command("systemctl", "--user", "disable", UnitName).Run()
		_ = os.Remove(result.UnitPath)
		_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
	})

	if result.DryRun {
		t.Fatalf("expected a real install (systemd --user reported available), got DryRun=true note=%q", result.DryRunNote)
	}
	if !result.Enabled {
		t.Fatalf("Install reported Enabled=false")
	}
	if !result.Lingering {
		t.Fatalf("Install reported Lingering=false")
	}

	// Real unit file content carries the RESOLVED (non-hardcoded) paths.
	unitContent, err := RenderUnit(opts)
	if err != nil {
		t.Fatalf("RenderUnit for assertion: %v", err)
	}
	wantUnitDir, err := DefaultUnitDir()
	if err != nil {
		t.Fatalf("DefaultUnitDir: %v", err)
	}
	writtenPath := filepath.Join(wantUnitDir, UnitName)
	if result.UnitPath != writtenPath {
		t.Errorf("UnitPath = %q, want %q", result.UnitPath, writtenPath)
	}
	writtenBytes, err := readFile(writtenPath)
	if err != nil {
		t.Fatalf("read written unit: %v", err)
	}
	if writtenBytes != unitContent {
		t.Errorf("written unit content does not match RenderUnit output:\nwritten:\n%s\nwant:\n%s", writtenBytes, unitContent)
	}
	if !strings.Contains(writtenBytes, serveBinary) {
		t.Errorf("written unit does not reference resolved ServeBinary %q:\n%s", serveBinary, writtenBytes)
	}
	if !strings.Contains(writtenBytes, dgHome) {
		t.Errorf("written unit does not reference resolved DGHome %q:\n%s", dgHome, writtenBytes)
	}

	// GROUND-SOURCE: real systemctl --user is-enabled — not a mock.
	enabledOut, err := exec.Command("systemctl", "--user", "is-enabled", UnitName).CombinedOutput()
	if err != nil {
		t.Fatalf("systemctl --user is-enabled %s: %v: %s", UnitName, err, enabledOut)
	}
	if got := strings.TrimSpace(string(enabledOut)); got != "enabled" {
		t.Errorf("systemctl --user is-enabled = %q, want %q", got, "enabled")
	}

	// GROUND-SOURCE: real loginctl show-user --property=Linger — not a mock.
	u, err := user.Current()
	if err != nil {
		t.Fatalf("user.Current: %v", err)
	}
	lingerOut, err := exec.Command("loginctl", "show-user", "--property=Linger", u.Username).CombinedOutput()
	if err != nil {
		t.Fatalf("loginctl show-user --property=Linger %s: %v: %s", u.Username, err, lingerOut)
	}
	if got := strings.TrimSpace(string(lingerOut)); got != "Linger=yes" {
		t.Errorf("loginctl show-user --property=Linger = %q, want %q", got, "Linger=yes")
	}
}

// TestInstallDryRunWhenSystemdAbsent forces the unavailable branch by
// pointing PATH somewhere systemctl/loginctl cannot be found, and asserts
// Install falls back to templating-only with DryRun=true and a populated
// note — covering the CI-without-systemd branch the item's DONE clause
// requires ("or a templating dry-run on CI where systemd is unavailable").
func TestInstallDryRunWhenSystemdAbsent(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no systemctl/loginctl reachable

	unitDir := t.TempDir()
	opts := Options{
		ServeBinary: filepath.Join(t.TempDir(), "dontguess"),
		DGHome:      filepath.Join(t.TempDir(), ".dontguess"),
		UnitDir:     unitDir,
	}

	result, err := Install(opts)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !result.DryRun {
		t.Fatalf("expected DryRun=true with systemctl/loginctl absent from PATH, got false")
	}
	if result.DryRunNote == "" {
		t.Errorf("expected a non-empty DryRunNote explaining unavailability")
	}
	if result.Enabled || result.Lingering {
		t.Errorf("dry run must not report Enabled/Lingering true: Enabled=%v Lingering=%v", result.Enabled, result.Lingering)
	}
	// The unit file is still written even in dry run, so templating is
	// exercised and the file is inspectable/installable manually.
	if _, err := readFile(result.UnitPath); err != nil {
		t.Errorf("dry run did not write unit file at %s: %v", result.UnitPath, err)
	}
}

func readFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
