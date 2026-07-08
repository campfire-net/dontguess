package nostr

// Event is a NIP-01 nostr event. This is the on-the-wire shape.
//
// The adapter (FromNostrEvent/ToNostrEvent) fills the structural fields — Kind,
// Tags, Content, PubKey, CreatedAt — and passes ID through. It does NOT sign
// (Sig stays empty) and does NOT compute the canonical nostr event id hash: both
// belong to the signing/sequencer workstream (dontguess-50d). ID is carried as a
// stable opaque identifier so the Message<->Event round-trip is exact; a signing
// layer downstream may recompute it.
type Event struct {
	ID        string     `json:"id"`
	PubKey    string     `json:"pubkey"`
	CreatedAt int64      `json:"created_at"` // seconds since epoch (NIP-01)
	Kind      int        `json:"kind"`
	Tags      [][]string `json:"tags"`
	Content   string     `json:"content"`
	Sig       string     `json:"sig"`
}
