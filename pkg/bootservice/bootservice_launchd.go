// launchd backend for pkg/bootservice (dontguess-aa4, Gate B/P7-mac).
//
// Deliberately NOT named bootservice_darwin.go: a *_darwin.go filename is an
// IMPLICIT Go build constraint (GOOS suffix) and would make this file
// compile only on darwin — but the ground-source test for this item
// requires the platform guard itself to be exercised and asserted from a
// non-darwin CI runner (dontguess-aa4 GROUND-SOURCE clause: "assert the
// darwin platform-guard prevents it running on linux"). So this file
// compiles on every GOOS; the guard is a runtime.GOOS check inside
// launchdAvailable, not a build tag. On a non-darwin host, InstallLaunchAgent
// still renders and writes the plist (so the templating is exercised and
// inspectable, per the DONE clause's "or a templating dry-run on non-macOS
// CI") but never shells out to launchctl.
//
// Shares composition with the systemd backend in bootservice.go: same
// Options/Result types, same validateOptions() path validation, same
// render-then-probe-then-install shape. Only the platform tool differs
// (plist + launchctl vs. unit + systemctl/loginctl) — no logic is
// duplicated between the two backends.
package bootservice

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"text/template"
)

// LaunchAgentLabel is the launchd Label (and plist basename stem) installed
// for the operator.
const LaunchAgentLabel = "ai.dontguess.operator"

// PlistName is the LaunchAgent plist filename installed for the operator.
const PlistName = LaunchAgentLabel + ".plist"

const plistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>{{.Label}}</string>
	<key>ProgramArguments</key>
	<array>
		<string>{{.ServeBinary}}</string>
		<string>serve</string>
	</array>
	<key>EnvironmentVariables</key>
	<dict>
		<key>DG_HOME</key>
		<string>{{.DGHome}}</string>
{{- if .RelayURLs}}
		<key>DONTGUESS_RELAY_URLS</key>
		<string>{{.RelayURLs}}</string>
{{- end}}
	</dict>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
</dict>
</plist>
`

// RenderPlist renders the launchd LaunchAgent plist content for opts. Pure
// function — no filesystem or launchctl calls — so templating can be tested
// without a darwin runtime, mirroring RenderUnit for the systemd backend.
func RenderPlist(opts Options) (string, error) {
	if err := validateOptions(opts); err != nil {
		return "", err
	}

	tmpl, err := template.New(PlistName).Parse(plistTemplate)
	if err != nil {
		return "", fmt.Errorf("parse plist template: %w", err)
	}

	data := struct {
		Label       string
		ServeBinary string
		DGHome      string
		RelayURLs   string
	}{
		Label:       xmlEscapeText(LaunchAgentLabel),
		ServeBinary: xmlEscapeText(opts.ServeBinary),
		DGHome:      xmlEscapeText(opts.DGHome),
		RelayURLs:   xmlEscapeText(strings.Join(opts.RelayURLs, ",")),
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("render plist template: %w", err)
	}
	return buf.String(), nil
}

// xmlEscapeText XML-escapes s for embedding as plist <string> element text
// (RenderPlist uses text/template, which does NOT escape — unlike
// html/template — so every value interpolated into the XML body must be
// pre-escaped here). Without this, a ServeBinary/DGHome/RelayURLs value
// containing an XML metacharacter (&, <, >) produced malformed plist XML
// while RenderPlist returned a nil error — silent corruption (dontguess-983).
func xmlEscapeText(s string) string {
	var buf bytes.Buffer
	// xml.EscapeText only errors if the underlying writer errors;
	// bytes.Buffer never does.
	_ = xml.EscapeText(&buf, []byte(s))
	return buf.String()
}

// DefaultLaunchAgentDir returns the per-user LaunchAgent directory:
// ~/Library/LaunchAgents.
func DefaultLaunchAgentDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, "Library", "LaunchAgents"), nil
}

// launchdAvailable reports whether this host can actually load a launchd
// LaunchAgent: it must be darwin (the platform guard — dontguess-aa4
// CONSTRAINTS: "platform-guard so it only runs on darwin") AND launchctl
// must be on PATH. Returns ok=false with a human note when unavailable —
// never silently assumed, mirroring systemdUserAvailable for the systemd
// backend.
func launchdAvailable() (ok bool, note string) {
	if runtime.GOOS != "darwin" {
		return false, fmt.Sprintf("launchd backend is darwin-only; running on GOOS=%s", runtime.GOOS)
	}
	if _, err := exec.LookPath("launchctl"); err != nil {
		return false, "launchctl not found on PATH"
	}
	return true, ""
}

// InstallLaunchAgent writes the launchd LaunchAgent plist for opts and, when
// running on darwin with launchctl available, loads it (`launchctl load -w`)
// so the operator survives logout — RunAtLoad+KeepAlive in the plist is the
// launchd analog of systemd's enable+linger (ADV-6); launchd has no separate
// "linger" concept, so Result.Lingering mirrors Enabled here.
//
// On any non-darwin host — including CI — InstallLaunchAgent falls back to a
// DRY RUN via the SAME platform-guard probe (launchdAvailable): it still
// renders and writes the plist file (so the templating is exercised and
// inspectable) but never calls launchctl, and reports Result.DryRun=true
// with a DryRunNote explaining why. Callers/tests must assert the guard
// explicitly via the returned note — never assume dry run silently.
func InstallLaunchAgent(opts Options) (*Result, error) {
	content, err := RenderPlist(opts)
	if err != nil {
		return nil, err
	}

	agentDir := opts.UnitDir
	if agentDir == "" {
		agentDir, err = DefaultLaunchAgentDir()
		if err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		return nil, fmt.Errorf("create LaunchAgent dir %s: %w", agentDir, err)
	}
	plistPath := filepath.Join(agentDir, PlistName)
	if err := os.WriteFile(plistPath, []byte(content), 0o644); err != nil {
		return nil, fmt.Errorf("write plist file %s: %w", plistPath, err)
	}

	result := &Result{UnitPath: plistPath}

	available, note := launchdAvailable()
	if !available {
		result.DryRun = true
		result.DryRunNote = note
		return result, nil
	}

	// GROUND-SOURCE: real launchctl load — not a mock. `-w` overrides any
	// prior Disabled key so the agent is not left dormant after re-install.
	if out, err := exec.Command("launchctl", "load", "-w", plistPath).CombinedOutput(); err != nil {
		return result, fmt.Errorf("launchctl load -w %s: %w: %s", plistPath, err, strings.TrimSpace(string(out)))
	}
	result.Enabled = true
	// launchd has no separate linger step: RunAtLoad+KeepAlive in the plist
	// already means the agent starts at login and is respawned by launchd
	// without a foreground session — that's what "survives logout" means on
	// this platform. Mirror Lingering=true so callers/tests can assert
	// "installed and persistent" identically across both backends.
	result.Lingering = true

	return result, nil
}
