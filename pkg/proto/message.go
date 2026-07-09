// Package proto defines shared message types used across dontguess packages.
//
// Message is the dontguess-owned representation of an exchange message.
// Placing it here (instead of pkg/exchange) avoids circular imports: both
// pkg/exchange and pkg/scrip can import pkg/proto without depending on each other.
package proto

// Message is the dontguess-owned representation of an exchange message.
//
// Internally all state processing and engine handlers operate on *Message so
// that exchange logic is decoupled from any transport boundary type. The
// store->Message conversion lives at the campfire-free dgstore boundary
// (pkg/store.Record.ToMessage / pkg/exchange.FromStoreRecord, dontguess-657).
type Message struct {
	// ID is the message ID (hex-encoded public key hash).
	ID string
	// CampfireID is the exchange this message belongs to.
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
