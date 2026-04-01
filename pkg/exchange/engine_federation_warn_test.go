package exchange_test

// engine_federation_warn_test.go — tests for the federation guard startup warning.
//
// dontguess-fbd1: FederationGuardEnabled=false default allows new-node brokered
// routing in production. The engine emits a WARN-level log when BrokeredMatchMode
// is enabled without FederationGuardEnabled, alerting operators to set the flag.

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/campfire-net/campfire/pkg/protocol"

	"github.com/campfire-net/dontguess/pkg/exchange"
)

// TestEngine_FederationGuardWarning verifies that starting an engine with
// BrokeredMatchMode=true and FederationGuardEnabled=false emits a startup
// warning log containing "FederationGuardEnabled=false".
func TestEngine_FederationGuardWarning(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)

	var mu sync.Mutex
	var logs []string
	warned := make(chan struct{}, 1)

	eng := exchange.NewEngine(exchange.EngineOptions{
		CampfireID:             h.cfID,
		Store:                  h.st,
		ReadClient:             protocol.New(h.st, nil),
		WriteClient:            h.newOperatorClient(),
		BrokeredMatchMode:      true,
		FederationGuardEnabled: false,
		Logger: func(format string, args ...any) {
			mu.Lock()
			logs = append(logs, format)
			mu.Unlock()
			if strings.Contains(format, "FederationGuardEnabled=false") {
				select {
				case warned <- struct{}{}:
				default:
				}
			}
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = eng.Start(ctx) }()

	select {
	case <-warned:
		// Warning emitted — pass.
	case <-ctx.Done():
		t.Error("context cancelled before federation guard warning was emitted")
	}
}

// TestEngine_FederationGuardNoWarning verifies no spurious warning when the
// guard is properly configured or brokered mode is disabled.
func TestEngine_FederationGuardNoWarning(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name                   string
		brokeredMatchMode      bool
		federationGuardEnabled bool
	}{
		{"guard enabled", true, true},
		{"brokered off guard off", false, false},
		{"brokered off guard on", false, true},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newTestHarness(t)

			var mu sync.Mutex
			var logs []string

			eng := exchange.NewEngine(exchange.EngineOptions{
				CampfireID:             h.cfID,
				Store:                  h.st,
				ReadClient:             protocol.New(h.st, nil),
				WriteClient:            h.newOperatorClient(),
				BrokeredMatchMode:      tc.brokeredMatchMode,
				FederationGuardEnabled: tc.federationGuardEnabled,
				Logger: func(format string, args ...any) {
					mu.Lock()
					logs = append(logs, format)
					mu.Unlock()
				},
			})

			// Cancel immediately so Start returns without entering the poll loop.
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			_ = eng.Start(ctx)

			mu.Lock()
			defer mu.Unlock()
			for _, line := range logs {
				if strings.Contains(line, "FederationGuardEnabled=false") {
					t.Errorf("unexpected federation guard warning for case %q: %s", tc.name, line)
				}
			}
		})
	}
}
