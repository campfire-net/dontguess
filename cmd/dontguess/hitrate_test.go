package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/campfire-net/dontguess/pkg/exchange"
)

// TestPrintHitRate_RenderedOutput asserts the text-mode report renders the
// key fields printHitRate is responsible for, for a fixed HitRateReport.
// Guards the CLI-visible output the rebuilt REQ-filter-backed runHitRate
// (see reqfilter.go) still has to produce (dontguess-7fc: views.go deletion
// must not change what operators see from `dontguess hit-rate`).
func TestPrintHitRate_RenderedOutput(t *testing.T) {
	rep := exchange.HitRateReport{
		TotalBuys:               10,
		MatchedBuys:             8,
		PendingBuys:             2,
		Hits:                    5,
		Misses:                  3,
		HitRatePct:              62.5,
		BelowFloorDowngraded:    1,
		RecomputedSimilarity:    2,
		UnverifiableHits:        0,
		SyntheticExcluded:       4,
		MatchResultsTotal:       8,
		UnjoinableMatchResults:  0,
		CrossAgentConvergence:   3,
		NetTokensSaved:          1200,
		SavedOnRealHits:         1500,
		TotalMissCost:           300,
		TotalFalsePositiveWaste: 0,
	}

	stdout := captureStdout(t, func() {
		printHitRate(rep, 24*time.Hour, false)
	})

	for _, want := range []string{
		"dontguess buy hit-rate (last 24h0m0s)",
		"total buys:        10",
		"answered (matched): 8",
		"pending (no match): 2",
		"hits (quality):    5",
		"misses:            3",
		"QUALITY HIT RATE:  62.50%",
		"cross-agent convergence: 3",
		"NET TOKENS SAVED:       +1200",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("printHitRate output missing %q\nfull output:\n%s", want, stdout)
		}
	}
}

func TestPrintHitRate_JSONOutput(t *testing.T) {
	rep := exchange.HitRateReport{TotalBuys: 3, Hits: 1, Misses: 1, HitRatePct: 50}

	stdout := captureStdout(t, func() {
		printHitRate(rep, 0, true)
	})

	if !strings.Contains(stdout, `"total_buys": 3`) && !strings.Contains(stdout, `"TotalBuys": 3`) {
		t.Errorf("JSON output missing total_buys field:\n%s", stdout)
	}
}

// captureStdout redirects os.Stdout for the duration of fn and returns
// everything written to it. Shared helper for command-render tests.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	fn()

	w.Close()
	var buf bytes.Buffer
	buf.ReadFrom(r) //nolint:errcheck
	return buf.String()
}
