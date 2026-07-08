package identity

import (
	"fmt"
	"time"
)

// textMessage mirrors gorilla/websocket.TextMessage (== 1). We define it locally
// so pkg/identity carries no websocket dependency in its production build — any
// conn whose WriteMessage/ReadMessage match FrameConn (a real *websocket.Conn
// does) drives the handshake. NIP-42 frames are always text (JSON arrays).
const textMessage = 1

// FrameConn is the minimal message-oriented connection the NIP-42 handshake
// needs. *github.com/gorilla/websocket.Conn satisfies it directly, so the
// handshake runs over a genuine websocket in tests and production without this
// package importing the websocket library.
type FrameConn interface {
	WriteMessage(messageType int, data []byte) error
	ReadMessage() (messageType int, p []byte, err error)
}

// ClientAuthenticate performs the client half of the NIP-42 handshake:
// read the relay's ["AUTH", challenge] frame, build+sign a kind-22242 event
// bound to that challenge and relay URL, send it back, and read the relay's
// ["OK", …] verdict. It returns an error if the relay rejects the identity
// (e.g. not on the allowlist).
func ClientAuthenticate(conn FrameConn, signer Signer, relayURL string) error {
	_, raw, err := conn.ReadMessage()
	if err != nil {
		return fmt.Errorf("client auth: read challenge: %w", err)
	}
	challenge, err := ParseAuthChallenge(raw)
	if err != nil {
		return fmt.Errorf("client auth: %w", err)
	}

	ev, err := BuildAuthEvent(signer, relayURL, challenge)
	if err != nil {
		return fmt.Errorf("client auth: build event: %w", err)
	}
	frame, err := EncodeAuthResponse(ev)
	if err != nil {
		return fmt.Errorf("client auth: encode response: %w", err)
	}
	if err := conn.WriteMessage(textMessage, frame); err != nil {
		return fmt.Errorf("client auth: send response: %w", err)
	}

	_, okRaw, err := conn.ReadMessage()
	if err != nil {
		return fmt.Errorf("client auth: read OK: %w", err)
	}
	_, accepted, msg, err := ParseOK(okRaw)
	if err != nil {
		return fmt.Errorf("client auth: %w", err)
	}
	if !accepted {
		return fmt.Errorf("client auth: relay rejected: %s", msg)
	}
	return nil
}

// RelayAuthenticate performs the relay half of the handshake: issue a fresh
// random challenge, read the client's signed AUTH event, verify it against the
// challenge and relay URL, then enforce the allowlist. It returns the
// authenticated hex pubkey on success.
//
// allowlist is required and fails closed: a nil allowlist is a configuration
// error, not "no enforcement" — it is rejected outright before any handshake
// I/O happens, so an unconfigured relay cannot silently admit every
// cryptographically-valid pubkey. Callers that genuinely want an open relay
// (e.g. single-operator/individual tier with no fleet to restrict to) must
// opt in explicitly by passing OpenAllowlist(), which is a named, auditable
// choice at the call site rather than an implicit consequence of a nil.
//
// This is the "allowlist enforced at handshake" gate: authenticity
// (VerifyAuthEvent) and authorization (allowlist) are both checked before the
// connection is considered authed, and both failure paths send a rejecting OK
// so the client learns it was refused rather than hanging.
func RelayAuthenticate(conn FrameConn, relayURL string, allowlist *Allowlist) (string, error) {
	if allowlist == nil {
		return "", fmt.Errorf("relay auth: nil allowlist not permitted (fail-closed); pass identity.OpenAllowlist() to explicitly disable enforcement")
	}

	challenge, err := NewChallenge()
	if err != nil {
		return "", fmt.Errorf("relay auth: new challenge: %w", err)
	}
	challFrame, err := EncodeAuthChallenge(challenge)
	if err != nil {
		return "", fmt.Errorf("relay auth: encode challenge: %w", err)
	}
	if err := conn.WriteMessage(textMessage, challFrame); err != nil {
		return "", fmt.Errorf("relay auth: send challenge: %w", err)
	}

	_, raw, err := conn.ReadMessage()
	if err != nil {
		return "", fmt.Errorf("relay auth: read response: %w", err)
	}
	ev, err := ParseAuthResponse(raw)
	if err != nil {
		return "", fmt.Errorf("relay auth: %w", err)
	}

	pubkey, verifyErr := VerifyAuthEvent(ev, relayURL, challenge, time.Now())
	if verifyErr != nil {
		sendOK(conn, ev.ID, false, "auth-required: "+verifyErr.Error())
		return "", fmt.Errorf("relay auth: %w", verifyErr)
	}

	// Allowlist enforcement. allowlist is guaranteed non-nil here (checked at
	// the top of the function); OpenAllowlist() explicitly disables the gate,
	// anything else is fail-closed.
	if !allowlist.Allowed(pubkey) {
		sendOK(conn, ev.ID, false, "restricted: npub not on fleet allowlist")
		return "", fmt.Errorf("relay auth: pubkey %s not on allowlist", pubkey)
	}

	sendOK(conn, ev.ID, true, "")
	return pubkey, nil
}

// sendOK writes an OK verdict frame, ignoring write errors (the connection is
// already being torn down on the reject path, and on the accept path a write
// failure surfaces on the next read).
func sendOK(conn FrameConn, eventID string, accepted bool, message string) {
	if frame, err := EncodeOK(eventID, accepted, message); err == nil {
		_ = conn.WriteMessage(textMessage, frame)
	}
}
