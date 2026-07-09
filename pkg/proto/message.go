// Package proto defines shared message types used across dontguess packages.
//
// Message is the dontguess-owned representation of a campfire message.
// Placing it here (instead of pkg/exchange) avoids circular imports: both
// pkg/exchange and pkg/scrip can import pkg/proto without depending on each other.
package proto

import (
	"github.com/campfire-net/campfire/cf-protocol/protocol"
)

// Message is the dontguess-owned representation of a campfire message.
//
// Internally all state processing and engine handlers operate on *Message so
// that exchange logic is decoupled from any transport boundary type. The
// store->Message conversion now lives at the campfire-free dgstore boundary
// (pkg/store.Record.ToMessage / pkg/exchange.FromStoreRecord, dontguess-657);
// the SDK boundary conversion (FromSDKMessage) remains here because pkg/scrip
// still ingests via the campfire SDK in the campfire code path.
type Message struct {
	// ID is the campfire message ID (hex-encoded public key hash).
	ID string
	// CampfireID is the campfire this message belongs to.
	CampfireID string
	// Sender is the hex-encoded Ed25519 public key of the sender.
	Sender string
	// Payload is the raw JSON message body.
	Payload []byte
	// Tags is the list of tags on the message.
	Tags []string
	// Antecedents is the list of antecedent message IDs.
	Antecedents []string
	// Timestamp is the sender clock (nanoseconds since epoch).
	Timestamp int64
	// Instance is the sender's self-asserted role / instance name (tainted).
	Instance string
}

// FromSDKMessage converts a protocol.Message (the SDK-facing type returned by
// client.Read) to a dontguess Message.
func FromSDKMessage(m protocol.Message) Message {
	return Message{
		ID:          m.ID,
		CampfireID:  m.CampfireID,
		Sender:      m.Sender,
		Payload:     m.Payload,
		Tags:        m.Tags,
		Antecedents: m.Antecedents,
		Timestamp:   m.Timestamp,
		Instance:    m.Instance,
	}
}

// FromSDKMessages converts a slice of protocol.Message to []Message.
// Convenience helper for Replay via client.Read.
func FromSDKMessages(ms []protocol.Message) []Message {
	msgs := make([]Message, len(ms))
	for i, m := range ms {
		msgs[i] = FromSDKMessage(m)
	}
	return msgs
}
