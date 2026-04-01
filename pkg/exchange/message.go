package exchange

import (
	"github.com/campfire-net/campfire/pkg/protocol"
	"github.com/campfire-net/campfire/pkg/store"

	"github.com/campfire-net/dontguess/pkg/proto"
)

// Message is the dontguess-owned representation of a campfire message used
// throughout the exchange engine and state machine.
//
// It is a type alias for proto.Message so that pkg/scrip can also use the same
// type via pkg/proto without creating a circular import between pkg/exchange
// and pkg/scrip (exchange imports scrip; scrip cannot import exchange).
type Message = proto.Message

// FromStoreRecord converts a store.MessageRecord to a dontguess *Message at
// the campfire SDK boundary. All internal processing uses *Message.
func FromStoreRecord(r *store.MessageRecord) *Message {
	return proto.FromStoreRecord(r)
}

// FromStoreRecords converts a slice of store.MessageRecord to []Message.
func FromStoreRecords(recs []store.MessageRecord) []Message {
	return proto.FromStoreRecords(recs)
}

// FromSDKMessage converts a protocol.Message (from the campfire SDK) to a *Message.
// Used at the Subscribe/Read boundary when ReadClient is configured.
func FromSDKMessage(m *protocol.Message) *Message {
	if m == nil {
		return nil
	}
	tags := m.Tags
	if tags == nil {
		tags = []string{}
	}
	antecedents := m.Antecedents
	if antecedents == nil {
		antecedents = []string{}
	}
	return &Message{
		ID:          m.ID,
		CampfireID:  m.CampfireID,
		Sender:      m.Sender,
		Payload:     m.Payload,
		Tags:        tags,
		Antecedents: antecedents,
		Timestamp:   m.Timestamp,
		Instance:    m.Instance,
	}
}

// FromSDKMessages converts a slice of protocol.Message to []Message.
func FromSDKMessages(msgs []protocol.Message) []Message {
	result := make([]Message, len(msgs))
	for i := range msgs {
		result[i] = *FromSDKMessage(&msgs[i])
	}
	return result
}
