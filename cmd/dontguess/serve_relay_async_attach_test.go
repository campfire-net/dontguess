package main

// serve_relay_async_attach_test.go — item dontguess-347 (design §4/§9 Gate
// A/P1). GROUND-SOURCE: a real TCP black-hole (a listener that ACCEPTS every
// connection but never writes a byte back) genuinely blocks
// Conn.dialAndAuth's websocket handshake read. Before the fix, the relay-leg
// attach loop in runServeLocal ran synchronously BEFORE the operator socket
// bound (serve.go's old socket-bind-inside-runEngineLoop, called AFTER the
// attach loop) — so a dead/slow relay hung serve startup and the operator
// socket never came up. This test drives the two extracted pieces exactly as
// runServeLocal composes them (bindOperatorSocket called BEFORE
// attachRelayLegsAsync) against the real black-hole and asserts a real IPC
// round-trip over the real operator socket completes in well under 1s, even
// though the relay leg is still stuck retrying its dial in the background.

import (
	"context"
	"encoding/json"
	"log"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/3dl-dev/dontguess/pkg/exchange"
	dgstore "github.com/3dl-dev/dontguess/pkg/store"
)

// newTCPBlackHole starts a real TCP listener that accepts every connection
// and then holds it open forever without ever writing a response — the
// classic "firewalled port that still completes the TCP handshake" hang
// scenario named in the item's GROUND-SOURCE clause. It genuinely blocks
// anything doing an HTTP/websocket upgrade read against it (real network IO,
// not a mock of dialAndAuth or Conn). Returns the listener (caller closes it
// on cleanup, which unblocks any goroutine still reading) and its address.
func newTCPBlackHole(t *testing.T) (net.Listener, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("black hole listen: %v", err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // listener closed — test cleanup
			}
			// Accept and go silent forever: never write the HTTP 101 upgrade
			// response the websocket client is blocked reading. Held open
			// (not closed) so the client's read blocks on the live socket
			// rather than getting an immediate EOF/reset.
			go func(c net.Conn) {
				buf := make([]byte, 4096)
				for {
					if _, err := c.Read(buf); err != nil {
						return
					}
				}
			}(conn)
		}
	}()
	return ln, ln.Addr().String()
}

// TestServeStartup_OperatorSocketRespondsUnder1s_WithDeadRelayLeg is the
// dontguess-347 ground-source test. It composes bindOperatorSocket and
// attachRelayLegsAsync in the SAME order runServeLocal now uses — socket
// bound first, relay leg attached asynchronously second — against a relay
// URL pointing at a real TCP black-hole, and asserts a real "list-held" IPC
// round-trip over the real unix socket completes in under 1s. Before the fix
// (synchronous attach BEFORE the socket bind, blocking in Conn.dialAndAuth
// against the black hole) this same composition would hang past any
// reasonable deadline; asserting <1s wall-clock is the reject/accept gate
// named in the item's DONE clause.
func TestServeStartup_OperatorSocketRespondsUnder1s_WithDeadRelayLeg(t *testing.T) {
	dgHome := t.TempDir()

	localStorePath := filepath.Join(dgHome, "events.jsonl")
	localStore, err := dgstore.Open(localStorePath)
	if err != nil {
		t.Fatalf("opening local store: %v", err)
	}
	defer localStore.Close() //nolint:errcheck

	operatorIdentity, err := loadOrCreateNostrOperatorIdentity(dgHome)
	if err != nil {
		t.Fatalf("nostr operator identity: %v", err)
	}

	eng := exchange.NewEngine(exchange.EngineOptions{
		CampfireID:        "local",
		LocalStore:        localStore,
		OperatorPublicKey: operatorIdentity.PubKeyHex(),
		Logger:            t.Logf,
	})

	logger := log.New(os.Stderr, "[test-serve] ", 0)

	blackHole, blackHoleAddr := newTCPBlackHole(t)
	defer blackHole.Close()
	relayURL := "ws://" + blackHoleAddr

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	startupBegin := time.Now()

	// 1) Bind the operator socket FIRST — exactly the order runServeLocal now
	// uses (dontguess-347 fix). bindOperatorSocket now returns a hard error on
	// bind failure (dontguess-7b2) instead of a nil-cleanup WARN.
	socketCleanup, bindErr := bindOperatorSocket(ctx, dgHome, eng, logger)
	if bindErr != nil {
		t.Fatalf("bindOperatorSocket: %v", bindErr)
	}
	defer socketCleanup()

	// 2) Attach the relay leg asynchronously — must NOT block, even though
	// relayURL is a genuine TCP black hole that will hang dialAndAuth's
	// handshake read.
	var wg sync.WaitGroup
	var legsMu sync.Mutex
	var legs []relayLeg
	attachRelayLegsAsync(ctx, &wg, &legsMu, &legs, []string{relayURL},
		localStore, operatorIdentity, localStorePath, nil, eng, logger, 0, nil, nil)
	// attachRelayLegsAsync itself must return immediately (it only spawns
	// goroutines); if it were the old synchronous per-URL loop this call
	// would already be blocked inside Conn.dialAndAuth against the black
	// hole.
	if attachReturnElapsed := time.Since(startupBegin); attachReturnElapsed > time.Second {
		t.Fatalf("attachRelayLegsAsync blocked for %s attaching to a dead relay — must return immediately", attachReturnElapsed)
	}

	// 3) Real IPC round-trip over the real operator unix socket, timed from
	// serve "startup" (socket bind) to response. Must complete well under 1s
	// even though the relay leg above is still stuck retrying its dial.
	//
	// dontguess-7b2: dial the RESOLVED path, not the hardcoded DG_HOME-
	// relative default — t.TempDir() embeds the full test name and can push
	// the default path past the platform's unix socket length limit, in
	// which case bindOperatorSocket above already relocated it under
	// $XDG_RUNTIME_DIR.
	sockPath := resolveOperatorSocketPath(dgHome)
	deadline := time.Now().Add(2 * time.Second)
	var conn net.Conn
	for {
		conn, err = net.Dial("unix", sockPath)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("dialing operator socket: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	defer conn.Close()

	if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}
	if err := json.NewEncoder(conn).Encode(map[string]any{"op": OpListHeld}); err != nil {
		t.Fatalf("encode list-held request: %v", err)
	}
	var resp map[string]any
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		t.Fatalf("decode list-held response: %v", err)
	}

	elapsed := time.Since(startupBegin)
	if elapsed >= time.Second {
		t.Fatalf("operator IPC round-trip took %s (want <1s) with a dead relay leg attached — relay attach is blocking serve startup", elapsed)
	}
	t.Logf("operator IPC responded in %s with a dead-relay leg attached (async attach confirmed)", elapsed)

	// The relay leg must still be stuck retrying — proves the block really
	// was real (not e.g. a relay that happened to fail fast) and that
	// attachRelayLegsAsync is actually off the response path, not merely
	// fast in this run.
	legsMu.Lock()
	attachedCount := len(legs)
	legsMu.Unlock()
	if attachedCount != 0 {
		t.Fatalf("relay leg unexpectedly attached to a TCP black hole (legs=%d) — the retry goroutine should still be blocked/retrying", attachedCount)
	}
}
