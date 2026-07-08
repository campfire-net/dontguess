package identity

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// wsRelay stands up a real websocket server that runs the relay half of the
// NIP-42 handshake (RelayAuthenticate) against a supplied allowlist. It is the
// test fixture that makes "relay round-trip authenticated via NIP-42" a genuine
// over-the-wire exchange rather than an in-process function call — the client
// speaks to it through a real *websocket.Conn.
func wsRelay(t *testing.T, allowlist *Allowlist) (relayURL string, results <-chan relayResult) {
	t.Helper()
	ch := make(chan relayResult, 4)
	upgrader := websocket.Upgrader{}

	var relayURLHolder string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		pk, authErr := RelayAuthenticate(conn, relayURLHolder, allowlist)
		ch <- relayResult{pubkey: pk, err: authErr}
		// Give the client a moment to read the OK before the conn closes.
		time.Sleep(20 * time.Millisecond)
	}))
	t.Cleanup(srv.Close)

	relayURLHolder = "ws" + strings.TrimPrefix(srv.URL, "http")
	return relayURLHolder, ch
}

type relayResult struct {
	pubkey string
	err    error
}

func dial(t *testing.T, relayURL string) *websocket.Conn {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial(relayURL, nil)
	if err != nil {
		t.Fatalf("dial relay %s: %v", relayURL, err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// TestNIP42RoundTrip_AllowlistedAccepted drives the full handshake over a real
// websocket: an allowlisted fleet member authenticates and the relay accepts,
// returning the correct authenticated pubkey.
func TestNIP42RoundTrip_AllowlistedAccepted(t *testing.T) {
	t.Parallel()

	member, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	al, err := NewAllowlist(member.Npub())
	if err != nil {
		t.Fatalf("NewAllowlist: %v", err)
	}

	relayURL, results := wsRelay(t, al)
	conn := dial(t, relayURL)

	if err := ClientAuthenticate(conn, member, relayURL); err != nil {
		t.Fatalf("ClientAuthenticate (allowlisted member should be accepted): %v", err)
	}

	select {
	case res := <-results:
		if res.err != nil {
			t.Fatalf("relay reported auth error for allowlisted member: %v", res.err)
		}
		if res.pubkey != member.PubKeyHex() {
			t.Fatalf("relay authed pubkey %s, want %s", res.pubkey, member.PubKeyHex())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for relay result")
	}
}

// TestNIP42RoundTrip_StrangerRejected proves the allowlist is enforced at the
// handshake: an identity NOT on the allowlist is refused even though its
// signature is cryptographically valid (NIP-42 auth proves who, allowlist
// decides whether).
func TestNIP42RoundTrip_StrangerRejected(t *testing.T) {
	t.Parallel()

	member, _ := Generate()
	stranger, _ := Generate()
	al, err := NewAllowlist(member.Npub()) // stranger deliberately excluded
	if err != nil {
		t.Fatalf("NewAllowlist: %v", err)
	}

	relayURL, results := wsRelay(t, al)
	conn := dial(t, relayURL)

	err = ClientAuthenticate(conn, stranger, relayURL)
	if err == nil {
		t.Fatal("stranger not on the allowlist was accepted (allowlist not enforced at handshake)")
	}
	if !strings.Contains(err.Error(), "reject") {
		t.Fatalf("expected a rejection error, got: %v", err)
	}

	select {
	case res := <-results:
		if res.err == nil {
			t.Fatal("relay accepted a non-allowlisted stranger")
		}
		if res.pubkey != "" {
			t.Fatalf("relay returned a pubkey %q for a rejected stranger", res.pubkey)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for relay result")
	}
}

// TestRelayAuthenticate_NilAllowlistFailsClosed proves RelayAuthenticate no
// longer treats a nil allowlist as "no enforcement" (individual tier). A nil
// allowlist is now a configuration error that is rejected outright, before
// any handshake I/O — so a relay that forgot to configure an allowlist
// cannot silently admit every cryptographically-valid pubkey. The rejection
// must happen without the relay ever writing a challenge frame: this is
// checked directly against a conn stub, not over a real websocket, so the
// test can assert zero bytes were written.
func TestRelayAuthenticate_NilAllowlistFailsClosed(t *testing.T) {
	t.Parallel()

	conn := &recordingConn{}
	pubkey, err := RelayAuthenticate(conn, "wss://relay.example", nil)
	if err == nil {
		t.Fatal("RelayAuthenticate accepted a nil allowlist (should fail closed)")
	}
	if pubkey != "" {
		t.Fatalf("RelayAuthenticate returned pubkey %q on the nil-allowlist error path, want empty", pubkey)
	}
	if !strings.Contains(err.Error(), "nil allowlist") {
		t.Fatalf("error %q does not explain the nil-allowlist rejection", err.Error())
	}
	if conn.writes != 0 {
		t.Fatalf("RelayAuthenticate wrote %d frame(s) before rejecting a nil allowlist; want 0 (fail fast, no wasted handshake I/O)", conn.writes)
	}
}

// TestRelayAuthenticate_OpenAllowlistAdmitsAnyPubkey proves the explicit
// opt-in path: passing identity.OpenAllowlist() (rather than nil) is the only
// supported way to disable allowlist enforcement, and it still admits a
// pubkey with no prior allowlist membership.
func TestRelayAuthenticate_OpenAllowlistAdmitsAnyPubkey(t *testing.T) {
	t.Parallel()

	stranger, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	relayURL, results := wsRelay(t, OpenAllowlist())
	conn := dial(t, relayURL)

	if err := ClientAuthenticate(conn, stranger, relayURL); err != nil {
		t.Fatalf("ClientAuthenticate (OpenAllowlist should accept anyone): %v", err)
	}

	select {
	case res := <-results:
		if res.err != nil {
			t.Fatalf("relay reported auth error under OpenAllowlist: %v", res.err)
		}
		if res.pubkey != stranger.PubKeyHex() {
			t.Fatalf("relay authed pubkey %s, want %s", res.pubkey, stranger.PubKeyHex())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for relay result")
	}
}

// recordingConn is a minimal FrameConn stub that records how many frames were
// written, so TestRelayAuthenticate_NilAllowlistFailsClosed can assert the
// nil-allowlist guard rejects before any handshake I/O occurs.
type recordingConn struct {
	writes int
}

func (c *recordingConn) WriteMessage(messageType int, data []byte) error {
	c.writes++
	return nil
}

func (c *recordingConn) ReadMessage() (int, []byte, error) {
	return 0, nil, fmt.Errorf("recordingConn: ReadMessage should not be called")
}
