package main

// put_test.go is ed2-A's cobra-wiring test: it drives the ACTUAL runPut
// RunE function (package-main, so a source edit here or in put.go/
// pkg/relayclient invalidates the go test cache — the H7 discipline
// serve_relay_test.go / serve_poison_injection_e2e_test.go established)
// against a real in-process websocket relay server (net/http/httptest +
// gorilla/websocket), proving the outcome the item's OUTCOME line promises:
//
//   - an allowlisted npub's put becomes matchable inventory on a running
//     operator: modeled here as "no put-reject arrives" (the fake relay
//     server's default behavior — a real operator behaves identically for an
//     allowlisted seller, per engine_pricing.go autoAcceptPutLocked).
//   - a non-allowlisted put surfaces the operator's put-reject reason.
//   - missing config (no relay, no AGENT_CF_HOME) fails loud before any
//     network I/O.

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/campfire-net/dontguess/pkg/identity"
	"github.com/campfire-net/dontguess/pkg/nostr"
	"github.com/campfire-net/dontguess/pkg/proto"
	"github.com/campfire-net/dontguess/pkg/relay"
	"github.com/gorilla/websocket"
	"github.com/spf13/cobra"
)

func setPutFlags(t *testing.T, cmd *cobra.Command, vals map[string]string) {
	t.Helper()
	for k, v := range vals {
		if err := cmd.Flags().Set(k, v); err != nil {
			t.Fatalf("set flag %s=%q: %v", k, v, err)
		}
	}
}

func TestRunPut_NoRelayConfigured(t *testing.T) {
	t.Setenv("DONTGUESS_RELAY_URLS", "")
	t.Setenv("DONTGUESS_RELAY_URL", "")
	cmd := newPutCmd()
	setPutFlags(t, cmd, map[string]string{
		"description": "x",
		"content":     base64.StdEncoding.EncodeToString([]byte("y")),
		"token_cost":  "1000",
	})
	err := runPut(cmd, nil)
	if err == nil || !strings.Contains(err.Error(), "no relay configured") {
		t.Fatalf("expected 'no relay configured' error, got %v", err)
	}
}

func TestRunPut_MissingAgentIdentity(t *testing.T) {
	t.Setenv("AGENT_CF_HOME", "")
	cmd := newPutCmd()
	setPutFlags(t, cmd, map[string]string{
		"description": "x",
		"content":     base64.StdEncoding.EncodeToString([]byte("y")),
		"token_cost":  "1000",
		"relay":       "ws://127.0.0.1:1",
	})
	err := runPut(cmd, nil)
	if err == nil || !strings.Contains(err.Error(), "AGENT_CF_HOME") {
		t.Fatalf("expected AGENT_CF_HOME error, got %v", err)
	}
}

func TestRunPut_AllowlistedRelay_NoRejectObserved_Succeeds(t *testing.T) {
	srv := newFakeRelayServer(t, fakeRelayBehavior{})
	defer srv.Close()

	agentHome := t.TempDir()
	if _, _, err := identity.LoadOrCreate(agentHome); err != nil {
		t.Fatalf("LoadOrCreate agent: %v", err)
	}
	t.Setenv("AGENT_CF_HOME", agentHome)

	cmd := newPutCmd()
	setPutFlags(t, cmd, map[string]string{
		"description":  "reusable CI path filter",
		"content":      base64.StdEncoding.EncodeToString([]byte("computed content")),
		"token_cost":   "1000",
		"content_type": "exchange:content-type:code",
		"relay":        wsURL(srv.URL),
		"timeout":      "500ms",
	})
	if err := runPut(cmd, nil); err != nil {
		t.Fatalf("runPut: %v", err)
	}
}

func TestRunPut_NonAllowlistedRelay_SurfacesRejectAndFails(t *testing.T) {
	operator, _, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreate operator: %v", err)
	}
	srv := newFakeRelayServer(t, fakeRelayBehavior{
		reject:   true,
		operator: operator,
		reason:   "trust-gate: dropped_unlisted",
	})
	defer srv.Close()

	agentHome := t.TempDir()
	if _, _, err := identity.LoadOrCreate(agentHome); err != nil {
		t.Fatalf("LoadOrCreate agent: %v", err)
	}
	t.Setenv("AGENT_CF_HOME", agentHome)

	cmd := newPutCmd()
	setPutFlags(t, cmd, map[string]string{
		"description": "test content",
		"content":     base64.StdEncoding.EncodeToString([]byte("computed content")),
		"token_cost":  "1000",
		"relay":       wsURL(srv.URL),
		"timeout":     "1s",
	})
	err = runPut(cmd, nil)
	if err == nil {
		t.Fatalf("expected an error surfacing the operator's put-reject")
	}
	if !strings.Contains(err.Error(), "trust-gate: dropped_unlisted") {
		t.Fatalf("error %q does not surface the put-reject reason", err)
	}
}

// --- fake relay server (real websocket, in-process) --------------------------

type fakeRelayBehavior struct {
	reject   bool
	operator identity.Signer
	reason   string
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

// newFakeRelayServer runs a real in-process websocket server behaving like a
// minimal NIP-01 relay: it ACKs every published EVENT with OK, and — if
// behavior.reject is set — answers a REQ subscription with a genuinely signed
// settle(put-reject) event referencing the put id, mirroring
// engine_pricing.go's rejectPutLocked wire shape.
func newFakeRelayServer(t *testing.T, behavior fakeRelayBehavior) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		var putID string
		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				return
			}
			f, perr := relay.ParseFrame(raw)
			if perr != nil {
				continue
			}
			switch f.Type {
			case relay.LabelEVENT:
				if f.Event == nil {
					continue
				}
				putID = f.Event.ID
				ok, _ := relay.EncodeOK(f.Event.ID, true, "")
				_ = conn.WriteMessage(websocket.TextMessage, ok)
			case relay.LabelREQ:
				if !behavior.reject || putID == "" {
					continue
				}
				rejectEv := buildFakeRejectEvent(behavior.operator, putID, behavior.reason)
				frame, _ := relay.EncodeSubEvent(f.SubID, rejectEv)
				_ = conn.WriteMessage(websocket.TextMessage, frame)
			}
		}
	})
	return httptest.NewServer(mux)
}

func buildFakeRejectEvent(operator identity.Signer, putID, reason string) *identity.Event {
	payload, err := json.Marshal(map[string]any{
		"phase":    "put-reject",
		"entry_id": putID,
		"reason":   reason,
	})
	if err != nil {
		panic(err)
	}
	msg := &proto.Message{
		Sender:      operator.PubKeyHex(),
		Payload:     payload,
		Tags:        []string{"exchange:settle", "exchange:phase:put-reject", "exchange:verdict:rejected"},
		Antecedents: []string{putID},
		Timestamp:   time.Now().UnixNano(),
	}
	nev, err := nostr.ToNostrEvent(msg)
	if err != nil {
		panic(err)
	}
	ev := &identity.Event{
		PubKey:    nev.PubKey,
		CreatedAt: nev.CreatedAt,
		Kind:      nev.Kind,
		Tags:      nev.Tags,
		Content:   nev.Content,
	}
	if err := identity.SignEvent(operator, ev); err != nil {
		panic(err)
	}
	return ev
}

// wsURL converts an httptest server's http:// URL to a ws:// URL.
func wsURL(httpURL string) string {
	return "ws" + strings.TrimPrefix(httpURL, "http")
}
