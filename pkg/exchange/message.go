package exchange

import (
	"github.com/campfire-net/dontguess/pkg/proto"
	dgstore "github.com/campfire-net/dontguess/pkg/store"
)

// Message is the dontguess-owned representation of an exchange message used
// throughout the exchange engine and state machine.
//
// It is a type alias for proto.Message so that pkg/scrip can also use the same
// type via pkg/proto without creating a circular import between pkg/exchange
// and pkg/scrip (exchange imports scrip; scrip cannot import exchange).
type Message = proto.Message

// FromStoreRecord converts a campfire-free dgstore.Record to a dontguess
// *Message. The exchange engine and tests read operator egress and ingest from
// the local pkg/store log (dontguess-657), so the conversion boundary is the
// dgstore Record rather than the retired campfire store.MessageRecord.
//
// dgstore.Record.ToMessage carries exactly the Message-relevant subset
// (dropping store-local provenance/ordering metadata), so this is a lossless
// conversion into the *Message the engine's State.Replay/Apply consume.
//
// Note: pkg/store (dgstore) cannot live in pkg/proto because pkg/store already
// imports pkg/proto (for proto.Message) — so the conversion lives here in
// pkg/exchange, which may import both, rather than delegating to proto.
func FromStoreRecord(r *dgstore.Record) *Message {
	m := r.ToMessage()
	return &m
}

// FromStoreRecords converts a slice of dgstore.Record to []Message.
func FromStoreRecords(recs []dgstore.Record) []Message {
	msgs := make([]Message, len(recs))
	for i := range recs {
		msgs[i] = recs[i].ToMessage()
	}
	return msgs
}
