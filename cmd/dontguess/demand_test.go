package main

import (
	"strings"
	"testing"
	"time"

	"github.com/campfire-net/dontguess/pkg/demand"
)

// TestPrintDemandBacklog_RenderedOutput asserts the text-mode backlog renders
// cluster names, counts, and the fill-instructions footer. Guards the
// CLI-visible output the rebuilt REQ-filter-backed runDemand (buyMissFilter,
// see reqfilter.go) still has to produce (dontguess-7fc).
func TestPrintDemandBacklog_RenderedOutput(t *testing.T) {
	bl := demand.Backlog{
		TotalMisses:       7,
		SyntheticExcluded: 2,
		RealMisses:        5,
		Clusters: []demand.Cluster{
			{
				Name:  "security",
				Count: 2,
				Items: []demand.BacklogItem{
					{MissID: "miss-abc123456789", Task: "FROST threshold signing audit", OfferedPriceRate: 70},
					{MissID: "miss-def", Task: "auth gate review", OfferedPriceRate: 70},
				},
			},
		},
	}

	stdout := captureStdout(t, func() {
		printDemandBacklog(bl, 168*time.Hour)
	})

	for _, want := range []string{
		"dontguess demand backlog (last 168h0m0s)",
		"total misses:       7",
		"synthetic excluded: 2",
		"real misses:        5",
		"[security] — 2 task(s)",
		"70%",
		"FROST threshold signing audit",
		"dontguess put --description",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("printDemandBacklog output missing %q\nfull output:\n%s", want, stdout)
		}
	}
}

func TestPrintDemandBacklog_NoRealMisses(t *testing.T) {
	bl := demand.Backlog{TotalMisses: 3, SyntheticExcluded: 3, RealMisses: 0}

	stdout := captureStdout(t, func() {
		printDemandBacklog(bl, 0)
	})

	if !strings.Contains(stdout, "No real misses in the backlog.") {
		t.Errorf("expected empty-backlog message, got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "all history") {
		t.Errorf("expected 'all history' window label for since=0, got:\n%s", stdout)
	}
}
