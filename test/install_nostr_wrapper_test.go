// Package scale_test — nostr-first installer + wrapper regression (dontguess-ed2
// item ed2-F, design docs/design/nostr-first-client-ed2.md §3.10 + §6 item 7).
//
// Campfire was removed portfolio-wide. The installer must NOT download or install
// cf, and the generated wrapper must NOT dispatch through cf — every verb goes to
// the single dontguess-operator binary. The wrapper's flock serve auto-start must
// be gated on the INDIVIDUAL tier ONLY (H6 / RT-C#2): in team tier
// (DONTGUESS_RELAY_URLS set) the client dials the provisioned operator relay
// directly and MUST NEVER auto-start a local operator (a client-spawned
// relay-attached serve would mint its own key and become a rogue competing
// sequencer).
//
// These tests exercise the REAL shipped bytes: install.sh is read verbatim and the
// wrapper is extracted from its heredoc (extractWrapperFromInstaller, shared with
// install_flock_injection_test.go) so a drift in the shipped installer fails here.
package scale_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// readInstallerBytes returns the full site/install.sh bytes.
func readInstallerBytes(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	var installPath string
	for d := dir; ; {
		cand := filepath.Join(d, "site", "install.sh")
		if _, err := os.Stat(cand); err == nil {
			installPath = cand
			break
		}
		parent := filepath.Dir(d)
		if parent == d {
			t.Fatalf("could not locate site/install.sh walking up from %s", dir)
		}
		d = parent
	}
	data, err := os.ReadFile(installPath)
	if err != nil {
		t.Fatalf("reading %s: %v", installPath, err)
	}
	return string(data)
}

// TestInstaller_NoCfDownload asserts the installer never downloads or installs cf.
// The cf-era installer had a CF_REPO and a `command -v cf` / `${INSTALL_DIR}/cf`
// install block; the nostr-first installer must carry none of it.
func TestInstaller_NoCfDownload(t *testing.T) {
	install := readInstallerBytes(t)

	// Concrete cf-download markers — targeted so the bare substring "cf" in prose
	// (e.g. "campfire-free", "cf is gone") does not false-positive.
	forbidden := []string{
		"CF_REPO",           // the cf release repo variable
		`command -v cf`,     // the "cf already installed?" probe
		`${INSTALL_DIR}/cf`, // installing the cf binary
		`"cf" "$LABEL"`,     // fetch_and_verify "$CF_REPO" "cf" ...
		`fetch_and_verify "$CF_REPO"`,
	}
	for _, f := range forbidden {
		if strings.Contains(install, f) {
			t.Errorf("install.sh still contains cf-download marker %q — cf must be fully removed", f)
		}
	}
}

// TestWrapper_NoCfDispatch asserts the generated wrapper never dispatches through
// cf: no CF= binary path, no "$CF" exec, no cf-routed op. Every dispatch execs
// $DG_OP (the dontguess-operator binary).
func TestWrapper_NoCfDispatch(t *testing.T) {
	w := extractWrapperFromInstaller(t)

	forbidden := []string{
		`CF="`,                 // the cf binary path assignment
		`"$CF"`,                // exec cf
		`$CF `,                 // exec cf (unquoted use)
		`exchange_campfire_id`, // the retired campfire-id config field
		`XCFID`,                // the retired campfire-id routing variable
		`--cf-home`,            // the retired cf identity flag
	}
	for _, f := range forbidden {
		if strings.Contains(w, f) {
			t.Errorf("generated wrapper still contains cf-dispatch marker %q — the hot path must exec $DG_OP", f)
		}
	}

	// Positive: the wrapper MUST dispatch buy/put through the dontguess binary.
	if !strings.Contains(w, `"$DG_OP" "$@"`) {
		t.Errorf("generated wrapper does not dispatch to the dontguess binary (`\"$DG_OP\" \"$@\"` missing)")
	}
}

// TestWrapper_ServeAutoStartGatedOnIndividualTier asserts the H6 gate: the flock
// serve auto-start is wrapped in `if [ -z "$DONTGUESS_RELAY_URLS" ]` so it runs on
// the individual tier ONLY. Structurally: the DONTGUESS_RELAY_URLS empty-check must
// appear BEFORE the single flock auto-start, proving the auto-start is nested
// inside the individual-tier guard and never runs in team tier.
func TestWrapper_ServeAutoStartGatedOnIndividualTier(t *testing.T) {
	w := extractWrapperFromInstaller(t)

	// The H6 gate literal.
	const gate = `if [ -z "${DONTGUESS_RELAY_URLS:-}" ]; then`
	gateIdx := strings.Index(w, gate)
	if gateIdx < 0 {
		t.Fatalf("wrapper is missing the H6 individual-tier gate %q around the serve auto-start", gate)
	}

	// The serve auto-start is a flock on the start lock. There must be exactly one,
	// and it must fall AFTER the gate (i.e. inside the individual-tier branch).
	const flock = `flock -n "$LOCK"`
	flockIdx := strings.Index(w, flock)
	if flockIdx < 0 {
		t.Fatalf("wrapper is missing the flock serve auto-start %q", flock)
	}
	if strings.Count(w, flock) != 1 {
		t.Fatalf("expected exactly one flock serve auto-start; found %d — H6 gate reasoning assumes a single auto-start site", strings.Count(w, flock))
	}
	if flockIdx < gateIdx {
		t.Fatalf("flock serve auto-start (idx %d) appears BEFORE the H6 gate (idx %d) — the auto-start is NOT gated on the individual tier (rogue-sequencer bug)", flockIdx, gateIdx)
	}

	// The serve launch line inside the flock body must pin DG_HOME (not CF_HOME).
	if !strings.Contains(w, `env DG_HOME="$_DG_HOME" "$_DG_OP" serve`) {
		t.Errorf("flock body does not launch serve with DG_HOME pinned (`env DG_HOME=... $_DG_OP serve`)")
	}
	if strings.Contains(w, `env CF_HOME=`) {
		t.Errorf("flock body still pins CF_HOME — nostr-first serve must pin DG_HOME")
	}
}
