package exchange

import (
	"github.com/campfire-net/campfire/pkg/message"
	"github.com/campfire-net/campfire/pkg/store"

	"github.com/3dl-dev/dontguess/pkg/proto"
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

// FromProtocolMessage converts a campfire message.Message to a *Message.
// Reserved for Wave 1-2. Not called in Wave 0.
func FromProtocolMessage(id, campfireID string, m *message.Message) *Message {
	return proto.FromProtocolMessage(id, campfireID, m)
}
