// Package bootservice installs a platform boot service that runs
// `dontguess serve` and persists across logout (ADV-6, design §1 "up
// --relay" bullet + §9 Gate B/P7, operator ruling 2026-07-15 §10 Q5:
// cross-platform from day one).
//
// The composition — Options/Result types, absolute-path validation,
// render-then-probe-then-install flow, DryRun fallback when the platform
// tool is unavailable — is shared across backends. Only the platform
// backend differs:
//
//   - Linux: systemd --user unit + `loginctl enable-linger` (this file,
//     dontguess-748, Gate B/P7-linux).
//   - macOS: launchd LaunchAgent (bootservice_launchd.go, dontguess-aa4,
//     Gate B/P7-mac). RunAtLoad+KeepAlive is the launchd analog of
//     enable+linger — no separate "linger" step exists on launchd.
//
// Install() dispatches by runtime.GOOS. Callers that want a specific
// backend directly (e.g. tests) may call installSystemd/InstallLaunchAgent.
package bootservice

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"text/template"
)

// UnitName is the systemd --user unit filename installed for the operator.
const UnitName = "dontguess.service"

// Options configures the boot service install. ServeBinary and DGHome are
// caller-resolved absolute paths — this package never hardcodes either; it
// only templates what it is given.
type Options struct {
	// ServeBinary is the absolute path to the dontguess executable to run
	// as `<ServeBinary> serve`. Callers resolve this (e.g. via
	// os.Executable()) — never hardcoded here.
	ServeBinary string
	// DGHome is the absolute DG_HOME directory to export into the unit's
	// environment. Callers resolve this (e.g. via resolveDGHome()) — never
	// hardcoded here.
	DGHome string
	// RelayURLs, if non-empty, is exported as DONTGUESS_RELAY_URLS in the
	// unit environment (comma-joined) so a --relay `up` survives restarts
	// with the same federation config.
	RelayURLs []string
	// UnitDir overrides the systemd --user unit directory
	// ($XDG_CONFIG_HOME/systemd/user, normally ~/.config/systemd/user).
	// Tests set this to a scratch dir; production leaves it empty and
	// DefaultUnitDir() is used.
	UnitDir string
}

// Result reports what Install did, for callers (up, tests) to inspect or
// print.
type Result struct {
	UnitPath   string // the unit file written
	Enabled    bool   // systemctl --user enable succeeded
	Lingering  bool   // loginctl enable-linger succeeded (or was already set)
	DryRun     bool   // true if systemd --user was unavailable and only the unit file was templated
	DryRunNote string // why DryRun is true, when it is
}

const unitTemplate = `[Unit]
Description=DontGuess exchange operator
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart="{{.ServeBinary}}" serve
Environment="DG_HOME={{.DGHome}}"
{{- if .RelayURLs}}
Environment="DONTGUESS_RELAY_URLS={{.RelayURLs}}"
{{- end}}
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
`

// validateOptions checks the absolute-path invariants shared by every
// backend's render step (RenderUnit for systemd, RenderPlist for launchd) —
// callers must resolve ServeBinary/DGHome themselves; this package never
// hardcodes either.
func validateOptions(opts Options) error {
	if strings.TrimSpace(opts.ServeBinary) == "" {
		return errors.New("bootservice: ServeBinary is required")
	}
	if !filepath.IsAbs(opts.ServeBinary) {
		return fmt.Errorf("bootservice: ServeBinary must be an absolute path, got %q", opts.ServeBinary)
	}
	if strings.TrimSpace(opts.DGHome) == "" {
		return errors.New("bootservice: DGHome is required")
	}
	if !filepath.IsAbs(opts.DGHome) {
		return fmt.Errorf("bootservice: DGHome must be an absolute path, got %q", opts.DGHome)
	}
	return nil
}

// DefaultUnitDir returns the systemd --user unit directory for the current
// user: $XDG_CONFIG_HOME/systemd/user, falling back to
// ~/.config/systemd/user.
func DefaultUnitDir() (string, error) {
	if xdg := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); xdg != "" {
		return filepath.Join(xdg, "systemd", "user"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, ".config", "systemd", "user"), nil
}

// RenderUnit renders the systemd --user unit file content for opts. Pure
// function — no filesystem or systemctl calls — so templating can be tested
// without a systemd --user runtime.
func RenderUnit(opts Options) (string, error) {
	if err := validateOptions(opts); err != nil {
		return "", err
	}

	tmpl, err := template.New("dontguess.service").Parse(unitTemplate)
	if err != nil {
		return "", fmt.Errorf("parse unit template: %w", err)
	}

	data := struct {
		ServeBinary string
		DGHome      string
		RelayURLs   string
	}{
		ServeBinary: systemdQuoteArg(opts.ServeBinary),
		DGHome:      systemdQuoteArg(opts.DGHome),
		RelayURLs:   systemdQuoteArg(strings.Join(opts.RelayURLs, ",")),
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("render unit template: %w", err)
	}
	return buf.String(), nil
}

// systemdQuoteArg escapes s for embedding inside a double-quoted systemd
// unit-file value (ExecStart= command path, Environment= assignment). The
// unitTemplate wraps every interpolated value in literal double quotes, so a
// path or env value containing a space is parsed as one token/one
// assignment instead of being split on whitespace (systemd.syntax(7) C-style
// quoting: backslash and double-quote are the only characters that must be
// escaped inside a double-quoted token). Without this, a ServeBinary or
// DGHome path containing a space (valid on Linux) produced a unit systemd
// mis-parsed or rejected (dontguess-983).
func systemdQuoteArg(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}

// systemdUserAvailable reports whether this host can actually manage
// systemd --user units and linger in this session: both systemctl and
// loginctl must be on PATH AND `systemctl --user show-environment` must
// not error opening the user bus (e.g. no session, no user@.service, CI
// container without a login session). Returns ok=false with a human note
// when unavailable — never silently assumed.
func systemdUserAvailable() (ok bool, note string) {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return false, "systemctl not found on PATH"
	}
	if _, err := exec.LookPath("loginctl"); err != nil {
		return false, "loginctl not found on PATH"
	}
	cmd := exec.Command("systemctl", "--user", "show-environment")
	if err := cmd.Run(); err != nil {
		return false, fmt.Sprintf("systemctl --user unavailable in this session: %v", err)
	}
	return true, ""
}

// currentUsername resolves the invoking user's username for
// `loginctl enable-linger <user>`. loginctl also accepts no-arg (self) but
// an explicit username makes assertions in callers/tests unambiguous.
func currentUsername() (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("resolve current user: %w", err)
	}
	return u.Username, nil
}

// Install installs the boot service backend for the current platform: the
// launchd LaunchAgent on darwin (InstallLaunchAgent, dontguess-aa4), the
// systemd --user unit everywhere else (installSystemd, dontguess-748). This
// is the single entry point callers (e.g. `up`) should use — it is the
// composition that shares Options/Result across backends and platform-guards
// which one actually runs.
func Install(opts Options) (*Result, error) {
	if runtime.GOOS == "darwin" {
		return InstallLaunchAgent(opts)
	}
	return installSystemd(opts)
}

// installSystemd writes the systemd --user unit for opts, reloads the user
// manager, enables the unit, and enables linger (loginctl enable-linger) so
// the operator survives logout (ADV-6).
//
// If systemd --user is genuinely unavailable in this session (no bus, no
// systemctl/loginctl), installSystemd falls back to a DRY RUN: it still
// renders and writes the unit file (so the templating is exercised and
// inspectable) but skips daemon-reload/enable/linger and reports
// Result.DryRun=true with a DryRunNote explaining why. Callers/tests must
// assert the unavailability explicitly via the returned note — never assume
// dry run silently.
func installSystemd(opts Options) (*Result, error) {
	content, err := RenderUnit(opts)
	if err != nil {
		return nil, err
	}

	unitDir := opts.UnitDir
	if unitDir == "" {
		unitDir, err = DefaultUnitDir()
		if err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		return nil, fmt.Errorf("create unit dir %s: %w", unitDir, err)
	}
	unitPath := filepath.Join(unitDir, UnitName)
	if err := os.WriteFile(unitPath, []byte(content), 0o644); err != nil {
		return nil, fmt.Errorf("write unit file %s: %w", unitPath, err)
	}

	result := &Result{UnitPath: unitPath}

	available, note := systemdUserAvailable()
	if !available {
		result.DryRun = true
		result.DryRunNote = note
		return result, nil
	}

	if out, err := exec.Command("systemctl", "--user", "daemon-reload").CombinedOutput(); err != nil {
		return result, fmt.Errorf("systemctl --user daemon-reload: %w: %s", err, strings.TrimSpace(string(out)))
	}

	// Pass the unit's absolute path (rather than bare UnitName) so `enable`
	// resolves correctly even when UnitDir is not on systemd's default
	// search path (e.g. a test-scoped UnitDir) — systemctl accepts a path
	// to a unit file directly and symlinks it into the enable target.
	if out, err := exec.Command("systemctl", "--user", "enable", unitPath).CombinedOutput(); err != nil {
		return result, fmt.Errorf("systemctl --user enable %s: %w: %s", unitPath, err, strings.TrimSpace(string(out)))
	}
	result.Enabled = true

	username, err := currentUsername()
	if err != nil {
		return result, err
	}
	// loginctl enable-linger is mandatory (ADV-6): without it, the
	// systemd --user manager (and this unit) is torn down on logout even
	// though it is enabled. Do not treat unit-enable alone as success.
	if out, err := exec.Command("loginctl", "enable-linger", username).CombinedOutput(); err != nil {
		return result, fmt.Errorf("loginctl enable-linger %s: %w: %s", username, err, strings.TrimSpace(string(out)))
	}
	result.Lingering = true

	return result, nil
}
