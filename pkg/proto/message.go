// Package proto defines shared message types used across dontguess packages.
//
// Message is the dontguess-owned representation of a campfire message.
// Placing it here (instead of pkg/exchange) avoids circular imports: both
// pkg/exchange and pkg/scrip can import pkg/proto without depending on each other.
package proto

import (
	"encoding/hex"

	cfmessage "github.com/campfire-net/campfire/pkg/message"
	"github.com/campfire-net/campfire/pkg/store"
)

// Message is the dontguess-owned representation of a campfire message.
//
// Internally all state processing and engine handlers operate on *Message
// rather than *store.MessageRecord so that exchange logic is decoupled from
// the cf SDK boundary type. store.MessageRecord stays at the cf boundary
// (poll / replayAll / init); FromStoreRecord converts at that boundary.
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

// FromStoreRecord converts a store.MessageRecord to a dontguess Message.
// This is the only place store.MessageRecord is converted — called at the
// campfire boundary (poll / replayAll / dispatchPendingOrders) so all internal
// processing uses *Message.
func FromStoreRecord(r *store.MessageRecord) *Message {
	return &Message{
		ID:          r.ID,
		CampfireID:  r.CampfireID,
		Sender:      r.Sender,
		Payload:     r.Payload,
		Tags:        r.Tags,
		Antecedents: r.Antecedents,
		Timestamp:   r.Timestamp,
		Instance:    r.Instance,
	}
}

// FromStoreRecords converts a slice of store.MessageRecord to []Message.
// Convenience helper for Replay.
func FromStoreRecords(recs []store.MessageRecord) []Message {
	msgs := make([]Message, len(recs))
	for i := range recs {
		msgs[i] = *FromStoreRecord(&recs[i])
	}
	return msgs
}

// FromProtocolMessage converts a campfire message.Message to a dontguess
// Message. Reserved for Wave 1-2 when the engine transitions to protocol-level
// ingestion. Not called in Wave 0.
func FromProtocolMessage(id, campfireID string, m *cfmessage.Message) *Message {
	if m == nil {
		return nil
	}
	return &Message{
		ID:          id,
		CampfireID:  campfireID,
		Sender:      hex.EncodeToString(m.Sender),
		Payload:     m.Payload,
		Tags:        m.Tags,
		Antecedents: m.Antecedents,
		Timestamp:   m.Timestamp,
		Instance:    m.Instance,
	}
}
