package main

// operator_test.go — feature tests for dontguess-71c.
//
// Tests the unix socket IPC for operator CLI commands (list-held, accept-put,
// reject-put). Uses Option A: in-process test with real net.Listener and real
// exchange.Engine via a local test harness.
//
// All tests use real net.Listen + net.Dial + real JSON send/recv and exercise
// real engine state transitions (no mocks).

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/campfire-net/campfire/pkg/protocol"
	"github.com/campfire-net/campfire/pkg/store"
	"github.com/campfire-net/dontguess/pkg/exchange"
)

// ---- minimal in-package test harness ----

type opTestHarness struct {
	t              *testing.T
	cfID           string
	cfHome         string
	operatorClient *protocol.Client
	st             store.Store
	seller         string // hex pubkey
}

func newOpTestHarness(t *testing.T) *opTestHarness {
	t.Helper()
	cfHome := t.TempDir()
	transportDir := t.TempDir()
	convDir := conventionDirForOpTest(t)

	cfg, initClient, err := exchange.Init(exchange.InitOptions{
		ConfigDir:     cfHome,
		Transport:     protocol.FilesystemTransport{Dir: transportDir},
		BeaconDir:     t.TempDir(),
		ConventionDir: convDir,
	})
	if err != nil {
		t.Fatalf("exchange.Init: %v", err)
	}
	t.Cleanup(func() { initClient.Close() })

	st, err := store.Open(store.StorePath(cfHome))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	// Generate a seller identity.
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating seller key: %v", err)
	}
	sellerKey := hex.EncodeToString(pub)

	return &opTestHarness{
		t:              t,
		cfID:           cfg.ExchangeCampfireID,
		cfHome:         cfHome,
		operatorClient: initClient,
		st:             st,
		seller:         sellerKey,
	}
}

// conventionDirForOpTest locates the convention directory by walking up from the
// current working directory. Mirrors the pattern used in pkg/exchange/init_test.go.
func conventionDirForOpTest(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		candidate := filepath.Join(dir, "docs", "convention")
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("could not locate docs/convention — run tests from within the dontguess repo")
	return ""
}

// newEngine returns an exchange.Engine wired to the harness.
func (h *opTestHarness) newEngine() *exchange.Engine {
	opts := exchange.EngineOptions{
		CampfireID:   h.cfID,
		Store:        h.st,
		ReadClient:   protocol.New(h.st, nil),
		WriteClient:  h.operatorClient,
		ReadSkipSync: true,
		Logger: func(format string, args ...any) {
			h.t.Logf("[engine] "+format, args...)
		},
	}
	return exchange.NewEngine(opts)
}

// sendHeldPut inserts a put message with tokenCost over 1M (guaranteed held)
// and replays it into eng. Returns the put message ID.
func (h *opTestHarness) sendHeldPut(eng *exchange.Engine, desc string, tokenCost int64) string {
	h.t.Helper()

	idBytes := make([]byte, 32)
	if _, err := rand.Read(idBytes); err != nil {
		h.t.Fatalf("rand.Read: %v", err)
	}
	msgID := hex.EncodeToString(idBytes)

	// Build put payload with content large enough to pass MaxContentBytes.
	prefix := []byte("cached inference result: " + desc + " ")
	size := 1024
	if size < len(prefix) {
		size = len(prefix)
	}
	contentBytes := make([]byte, size)
	copy(contentBytes, prefix)
	for i := len(prefix); i < size; i++ {
		contentBytes[i] = byte('a' + i%26)
	}
	contentB64 := base64.StdEncoding.EncodeToString(contentBytes)

	payload, _ := json.Marshal(map[string]any{
		"description":  desc,
		"content":      contentB64,
		"token_cost":   tokenCost,
		"content_type": "exchange:content-type:code",
		"domains":      []string{"go"},
	})

	rec := store.MessageRecord{
		ID:          msgID,
		CampfireID:  h.cfID,
		Sender:      h.seller,
		Payload:     payload,
		Tags:        []string{exchange.TagPut, "exchange:content-type:code", "exchange:domain:go"},
		Antecedents: []string{},
		Timestamp:   store.NowNano(),
		Signature:   []byte{},
	}
	if _, err := h.st.AddMessage(rec); err != nil {
		h.t.Fatalf("AddMessage: %v", err)
	}

	// Replay all messages into eng state.
	msgs, err := h.st.ListMessages(h.cfID, 0)
	if err != nil {
		h.t.Fatalf("ListMessages: %v", err)
	}
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	// Classify it as held via RunAutoAccept with a low cap.
	skipped := make(map[string]struct{})
	eng.RunAutoAccept(1_000_000, time.Now(), skipped)

	return msgID
}

// startSocketServer starts the operator socket on a temp path, returns the path and a cancel func.
func startSocketServer(t *testing.T, eng *exchange.Engine) (string, context.CancelFunc) {
	t.Helper()
	sockPath := filepath.Join(t.TempDir(), "test-operator.sock")
	ln, err := listenOperatorSocket(sockPath)
	if err != nil {
		t.Fatalf("listenOperatorSocket: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		serveOperatorSocket(ctx, ln, eng)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
		os.Remove(sockPath)
	})
	return sockPath, cancel
}

// dialAndRequest dials sockPath, sends req, decodes into dst.
func dialAndRequest(t *testing.T, sockPath string, req map[string]any, dst any) {
	t.Helper()
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial %s: %v", sockPath, err)
	}
	defer conn.Close()

	if err := json.NewEncoder(conn).Encode(req); err != nil {
		t.Fatalf("encode request: %v", err)
	}
	if err := json.NewDecoder(conn).Decode(dst); err != nil {
		t.Fatalf("decode response: %v", err)
	}
}

// ---- Tests ----

// TestOperatorSocket_ListHeld verifies that a held put appears in the list-held response.
func TestOperatorSocket_ListHeld(t *testing.T) {
	t.Parallel()

	h := newOpTestHarness(t)
	eng := h.newEngine()

	const tokenCost = int64(2_000_000) // > 1M cap → held
	putID := h.sendHeldPut(eng, "large inference result", tokenCost)

	sockPath, _ := startSocketServer(t, eng)

	var resp struct {
		Puts []struct {
			PutMsgID  string `json:"put_msg_id"`
			TokenCost int64  `json:"token_cost"`
			Seller    string `json:"seller"`
		} `json:"puts"`
	}
	dialAndRequest(t, sockPath, map[string]any{"op": "list-held"}, &resp)

	if len(resp.Puts) != 1 {
		t.Fatalf("expected 1 held put in list-held response, got %d", len(resp.Puts))
	}
	got := resp.Puts[0]
	if got.PutMsgID != putID {
		t.Errorf("list-held: put_msg_id = %s, want %s", got.PutMsgID[:8], putID[:8])
	}
	if got.TokenCost != tokenCost {
		t.Errorf("list-held: token_cost = %d, want %d", got.TokenCost, tokenCost)
	}
	if got.Seller != h.seller {
		t.Errorf("list-held: seller = %s, want %s", got.Seller[:8], h.seller[:8])
	}
}

// TestOperatorSocket_AcceptPut verifies that accept-put removes the put from
// PutsHeldForReview and posts a settle put-accept message.
func TestOperatorSocket_AcceptPut(t *testing.T) {
	t.Parallel()

	h := newOpTestHarness(t)
	eng := h.newEngine()

	const tokenCost = int64(3_000_000)
	putID := h.sendHeldPut(eng, "accept-test result", tokenCost)

	sockPath, _ := startSocketServer(t, eng)

	price := tokenCost * 70 / 100
	expiresStr := time.Now().UTC().Add(72 * time.Hour).Format(time.RFC3339)

	var resp struct {
		OK    bool   `json:"ok"`
		Error string `json:"error,omitempty"`
	}
	dialAndRequest(t, sockPath, map[string]any{
		"op":         "accept-put",
		"put_msg_id": putID,
		"price":      price,
		"expires":    expiresStr,
	}, &resp)

	if !resp.OK {
		t.Fatalf("accept-put: ok=false, error=%s", resp.Error)
	}

	// Assert put is no longer in PutsHeldForReview.
	held := eng.State().PutsHeldForReview()
	for _, e := range held {
		if e.PutMsgID == putID {
			t.Errorf("put %s still in PutsHeldForReview after accept-put", putID[:8])
		}
	}

	// Assert a settle put-accept message was posted to the campfire.
	msgs, err := h.st.ListMessages(h.cfID, 0)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	found := false
	for _, m := range msgs {
		msg := exchange.FromStoreRecord(&m)
		for _, tag := range msg.Tags {
			if tag == exchange.TagPhasePrefix+exchange.SettlePhaseStrPutAccept {
				// Check antecedent references our put.
				for _, ant := range msg.Antecedents {
					if ant == putID {
						found = true
					}
				}
			}
		}
	}
	if !found {
		t.Errorf("no settle put-accept message found with antecedent=%s", putID[:8])
	}
}

// TestOperatorSocket_RejectPut verifies that reject-put posts a settle put-reject message.
func TestOperatorSocket_RejectPut(t *testing.T) {
	t.Parallel()

	h := newOpTestHarness(t)
	eng := h.newEngine()

	const tokenCost = int64(4_000_000)
	putID := h.sendHeldPut(eng, "reject-test result", tokenCost)

	sockPath, _ := startSocketServer(t, eng)

	var resp struct {
		OK    bool   `json:"ok"`
		Error string `json:"error,omitempty"`
	}
	dialAndRequest(t, sockPath, map[string]any{
		"op":         "reject-put",
		"put_msg_id": putID,
		"reason":     "over budget",
	}, &resp)

	if !resp.OK {
		t.Fatalf("reject-put: ok=false, error=%s", resp.Error)
	}

	// Assert a settle put-reject message was posted to the campfire.
	msgs, err := h.st.ListMessages(h.cfID, 0)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	found := false
	for _, m := range msgs {
		msg := exchange.FromStoreRecord(&m)
		for _, tag := range msg.Tags {
			if tag == exchange.TagPhasePrefix+exchange.SettlePhaseStrPutReject {
				for _, ant := range msg.Antecedents {
					if ant == putID {
						found = true
					}
				}
			}
		}
	}
	if !found {
		t.Errorf("no settle put-reject message found with antecedent=%s", putID[:8])
	}
}

// TestOperatorSocket_UnreachableError verifies that dialing a nonexistent socket
// returns a connection error.
func TestOperatorSocket_UnreachableError(t *testing.T) {
	t.Parallel()

	sockPath := filepath.Join(t.TempDir(), "nonexistent.sock")

	conn, err := net.Dial("unix", sockPath)
	if err == nil {
		conn.Close()
		t.Fatal("expected dial to fail for nonexistent socket, but it succeeded")
	}
	// Error confirms the socket is not reachable — the CLI would print the
	// "operator not reachable" message and exit 1. We just verify the error
	// is non-nil (the exact message is OS-dependent).
	t.Logf("got expected error: %v", err)
}

// TestOperatorSocket_ListHeld_Empty verifies that list-held returns an empty
// puts list when no puts are held.
func TestOperatorSocket_ListHeld_Empty(t *testing.T) {
	t.Parallel()

	h := newOpTestHarness(t)
	eng := h.newEngine()

	sockPath, _ := startSocketServer(t, eng)

	var resp struct {
		Puts []any `json:"puts"`
	}
	dialAndRequest(t, sockPath, map[string]any{"op": "list-held"}, &resp)

	if len(resp.Puts) != 0 {
		t.Errorf("expected empty puts list, got %d", len(resp.Puts))
	}
}

