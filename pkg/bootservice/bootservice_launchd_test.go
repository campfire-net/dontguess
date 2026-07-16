package bootservice

import (
	"encoding/xml"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestRenderPlistResolvesPathsNotHardcoded mirrors
// TestRenderUnitResolvesPathsNotHardcoded for the launchd backend: the
// plist content carries the CALLER-supplied serve binary + DG_HOME paths
// verbatim — never a hardcoded path.
func TestRenderPlistResolvesPathsNotHardcoded(t *testing.T) {
	opts := Options{
		ServeBinary: "/opt/custom/bin/dontguess",
		DGHome:      "/Users/weird-operator/Library/Application Support/dontguess",
		RelayURLs:   []string{"ws://192.168.2.40:7777", "ws://192.168.2.41:7777"},
	}
	content, err := RenderPlist(opts)
	if err != nil {
		t.Fatalf("RenderPlist: %v", err)
	}
	if !strings.Contains(content, "<string>/opt/custom/bin/dontguess</string>") {
		t.Errorf("plist does not reference caller-resolved ServeBinary:\n%s", content)
	}
	if !strings.Contains(content, "<string>/Users/weird-operator/Library/Application Support/dontguess</string>") {
		t.Errorf("plist does not reference caller-resolved DGHome:\n%s", content)
	}
	if !strings.Contains(content, "<string>ws://192.168.2.40:7777,ws://192.168.2.41:7777</string>") {
		t.Errorf("plist does not reference caller-resolved RelayURLs:\n%s", content)
	}
	if !strings.Contains(content, LaunchAgentLabel) {
		t.Errorf("plist missing Label %s:\n%s", LaunchAgentLabel, content)
	}
}

func TestRenderPlistRejectsRelativePaths(t *testing.T) {
	cases := []Options{
		{ServeBinary: "dontguess", DGHome: "/abs/home"},
		{ServeBinary: "/abs/bin/dontguess", DGHome: "relative/home"},
		{ServeBinary: "", DGHome: "/abs/home"},
		{ServeBinary: "/abs/bin/dontguess", DGHome: ""},
	}
	for i, opts := range cases {
		if _, err := RenderPlist(opts); err == nil {
			t.Errorf("case %d: expected error for %+v, got nil", i, opts)
		}
	}
}

// TestRenderPlistEscapesXMLMetacharacters is the GROUND-SOURCE test for
// dontguess-983: a ServeBinary/DGHome/RelayURLs value containing an XML
// metacharacter (&, <, >) — plausible in a relay URL query string or a
// path — must not corrupt the plist. Before the fix, RenderPlist used
// text/template (which does not escape) so this produced malformed,
// non-well-formed XML and returned a NIL error (silent corruption). We
// assert well-formedness via a real encoding/xml round-trip (Decoder token
// walk to EOF, not a mock/regex) AND that the decoded <string> element text
// equals the original unescaped value — proving the escape/unescape pair
// is correct, not merely that some coincidental output happens to parse.
func TestRenderPlistEscapesXMLMetacharacters(t *testing.T) {
	opts := Options{
		ServeBinary: `/opt/A & B/<dontguess>`,
		DGHome:      `/srv/op "home" & <weird>/.dontguess`,
		RelayURLs:   []string{"wss://relay.example/path?a=1&b=2<x>"},
	}
	content, err := RenderPlist(opts)
	if err != nil {
		t.Fatalf("RenderPlist: %v", err)
	}

	// GROUND-SOURCE: real encoding/xml token walk over the full document —
	// any malformed markup (unescaped & or < inside element text) makes
	// the decoder return a non-EOF error.
	dec := xml.NewDecoder(strings.NewReader(content))
	var stringTexts []string
	var sawStringElem bool
	for {
		tok, err := dec.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("plist is not well-formed XML: %v\ncontent:\n%s", err, content)
		}
		switch se := tok.(type) {
		case xml.StartElement:
			sawStringElem = se.Name.Local == "string"
		case xml.CharData:
			if sawStringElem {
				stringTexts = append(stringTexts, string(se))
			}
		case xml.EndElement:
			sawStringElem = false
		}
	}

	wantValues := []string{opts.ServeBinary, opts.DGHome, "wss://relay.example/path?a=1&b=2<x>"}
	for _, want := range wantValues {
		found := false
		for _, got := range stringTexts {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("decoded plist <string> elements do not contain the round-tripped value %q; decoded values: %q\ncontent:\n%s", want, stringTexts, content)
		}
	}
}

func TestRenderPlistOmitsRelayKeyWhenEmpty(t *testing.T) {
	content, err := RenderPlist(Options{ServeBinary: "/abs/bin/dontguess", DGHome: "/abs/home"})
	if err != nil {
		t.Fatalf("RenderPlist: %v", err)
	}
	if strings.Contains(content, "DONTGUESS_RELAY_URLS") {
		t.Errorf("plist should omit DONTGUESS_RELAY_URLS key when RelayURLs is empty:\n%s", content)
	}
}

func TestRenderPlistSetsRunAtLoadAndKeepAlive(t *testing.T) {
	content, err := RenderPlist(Options{ServeBinary: "/abs/bin/dontguess", DGHome: "/abs/home"})
	if err != nil {
		t.Fatalf("RenderPlist: %v", err)
	}
	if !strings.Contains(content, "<key>RunAtLoad</key>\n\t<true/>") {
		t.Errorf("plist missing RunAtLoad true — required for the agent to start at login:\n%s", content)
	}
	if !strings.Contains(content, "<key>KeepAlive</key>\n\t<true/>") {
		t.Errorf("plist missing KeepAlive true — required to survive respawn after logout:\n%s", content)
	}
}

// TestNoDuplicatedPathValidationBackend asserts the launchd and systemd
// backends share the SAME validateOptions() path-validation logic rather
// than each re-implementing it (dontguess-aa4 CONSTRAINTS: "do NOT
// duplicate the systemd path; share the composition, differ only the
// platform backend"). If RenderUnit and RenderPlist ever diverge on which
// inputs they accept/reject, this test catches the drift immediately.
func TestNoDuplicatedPathValidationBackend(t *testing.T) {
	cases := []Options{
		{ServeBinary: "", DGHome: "/abs/home"},
		{ServeBinary: "/abs/bin/dontguess", DGHome: ""},
		{ServeBinary: "relative", DGHome: "/abs/home"},
		{ServeBinary: "/abs/bin/dontguess", DGHome: "relative"},
		{ServeBinary: "/abs/bin/dontguess", DGHome: "/abs/home"},
	}
	for i, opts := range cases {
		_, unitErr := RenderUnit(opts)
		_, plistErr := RenderPlist(opts)
		if (unitErr == nil) != (plistErr == nil) {
			t.Errorf("case %d (%+v): RenderUnit err=%v, RenderPlist err=%v — backends disagree on validation, meaning validateOptions is not actually shared",
				i, opts, unitErr, plistErr)
		}
	}
}

// TestInstallLaunchAgentGroundSource is the MANDATORY ground-source test
// (dontguess-aa4): on a darwin runner it actually loads the LaunchAgent via
// `launchctl load -w` and asserts via the real `launchctl list` — not a mock
// — that it is loaded, and that the written plist carries RunAtLoad +
// KeepAlive (the launchd analog of "survives logout"). On a non-darwin
// runner it asserts the SAME platform-guard probe (launchdAvailable) fired
// and InstallLaunchAgent fell back to a templating-only dry run — i.e. it
// asserts the darwin platform-guard actually prevents launchctl from being
// invoked on linux, per the item's GROUND-SOURCE clause.
func TestInstallLaunchAgentGroundSource(t *testing.T) {
	agentDir := t.TempDir()
	serveBinary := filepath.Join(t.TempDir(), "dontguess")
	dgHome := filepath.Join(t.TempDir(), ".dontguess")
	opts := Options{
		ServeBinary: serveBinary,
		DGHome:      dgHome,
		UnitDir:     agentDir,
	}

	result, err := InstallLaunchAgent(opts)
	if err != nil {
		t.Fatalf("InstallLaunchAgent: %v", err)
	}

	wantPlistPath := filepath.Join(agentDir, PlistName)
	if result.UnitPath != wantPlistPath {
		t.Fatalf("UnitPath = %q, want %q", result.UnitPath, wantPlistPath)
	}
	writtenBytes, err := os.ReadFile(wantPlistPath)
	if err != nil {
		t.Fatalf("dry run did not write plist file at %s: %v", wantPlistPath, err)
	}
	if !strings.Contains(string(writtenBytes), serveBinary) {
		t.Errorf("written plist does not reference resolved ServeBinary %q:\n%s", serveBinary, writtenBytes)
	}
	if !strings.Contains(string(writtenBytes), dgHome) {
		t.Errorf("written plist does not reference resolved DGHome %q:\n%s", dgHome, writtenBytes)
	}
	if !strings.Contains(string(writtenBytes), "RunAtLoad") || !strings.Contains(string(writtenBytes), "KeepAlive") {
		t.Errorf("written plist missing RunAtLoad/KeepAlive:\n%s", writtenBytes)
	}

	if runtime.GOOS != "darwin" {
		// PLATFORM-GUARD ASSERTION (ground-source, non-darwin branch): the
		// guard inside launchdAvailable must have fired and prevented any
		// launchctl invocation — proven by DryRun=true with a note
		// mentioning the actual non-darwin GOOS, not by trusting the code
		// path was skipped.
		if !result.DryRun {
			t.Fatalf("expected DryRun=true on GOOS=%s (platform guard should prevent launchctl calls), got false", runtime.GOOS)
		}
		if !strings.Contains(result.DryRunNote, "darwin-only") {
			t.Errorf("DryRunNote = %q, want it to explain the darwin-only platform guard", result.DryRunNote)
		}
		if result.Enabled || result.Lingering {
			t.Errorf("dry run must not report Enabled/Lingering true: Enabled=%v Lingering=%v", result.Enabled, result.Lingering)
		}
		if _, err := exec.LookPath("launchctl"); err == nil {
			// launchctl happens to exist on this non-darwin box (unlikely,
			// but if it does, prove the guard — not LookPath — is what
			// blocked us: GOOS is still checked first in launchdAvailable).
			t.Logf("launchctl found on PATH despite GOOS=%s — guard still fired on GOOS, not tool absence", runtime.GOOS)
		}
		return
	}

	// GROUND-SOURCE (darwin branch): real launchctl load + list — not a mock.
	if result.DryRun {
		t.Fatalf("expected a real install on darwin, got DryRun=true note=%q", result.DryRunNote)
	}
	if !result.Enabled || !result.Lingering {
		t.Fatalf("InstallLaunchAgent reported Enabled=%v Lingering=%v, want both true", result.Enabled, result.Lingering)
	}
	t.Cleanup(func() {
		_ = exec.Command("launchctl", "unload", wantPlistPath).Run()
	})
	listOut, err := exec.Command("launchctl", "list", LaunchAgentLabel).CombinedOutput()
	if err != nil {
		t.Fatalf("launchctl list %s: %v: %s", LaunchAgentLabel, err, listOut)
	}
	if !strings.Contains(string(listOut), LaunchAgentLabel) {
		t.Errorf("launchctl list %s did not report the agent as loaded:\n%s", LaunchAgentLabel, listOut)
	}
}

// TestInstallDispatchesByPlatform asserts the shared Install() entry point
// (used by `up`) routes to the launchd backend on darwin and the systemd
// backend everywhere else — the platform-guard composition dontguess-aa4
// requires instead of callers hand-rolling a GOOS switch themselves.
func TestInstallDispatchesByPlatform(t *testing.T) {
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
	if result.Enabled {
		// installSystemd's real enable path was exercised (systemd --user
		// available in this session) — clean up the real default-location
		// symlink it created, same as TestInstallGroundSource, so this test
		// doesn't leave state that collides with other runs.
		t.Cleanup(func() {
			_ = exec.Command("systemctl", "--user", "disable", UnitName).Run()
			_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
		})
	}
	if runtime.GOOS == "darwin" {
		if filepath.Base(result.UnitPath) != PlistName {
			t.Errorf("Install on darwin wrote %q, want a %s file", result.UnitPath, PlistName)
		}
	} else {
		if filepath.Base(result.UnitPath) != UnitName {
			t.Errorf("Install on %s wrote %q, want a %s file", runtime.GOOS, result.UnitPath, UnitName)
		}
	}
}
