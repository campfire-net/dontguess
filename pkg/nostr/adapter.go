package nostr

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/campfire-net/dontguess/pkg/exchange"
	"github.com/campfire-net/dontguess/pkg/proto"
)

// domainPrefix is the exchange domain tag prefix. The exchange package strips it
// internally (stripDomainPrefixes) but does not export it as a constant, so the
// adapter mirrors the literal here. Domain tags map to NIP-01 topic (#t) tags.
const domainPrefix = "exchange:domain:"

// nanosPerSecond converts Message.Timestamp (nanoseconds) to nostr created_at
// (seconds).
const nanosPerSecond = int64(1_000_000_000)

// ToNostrEvent converts an exchange message into a nostr Event.
//
// Mapping (docs/design/nostr-first-rebuild-decision.md §Nostr Architecture):
//   - the single exchange operation tag -> Kind (put=3401, buy=3402, match=3403,
//     settle=3404, assign*=3405, scrip*=3411). For the four base ops the kind
//     fully determines the op, so the op tag is consumed by the kind. Assign and
//     scrip share a kind, so their exact sub-op is preserved in an ["op", <tag>]
//     discriminator.
//   - Antecedents -> ["e", <id>, "", "reply"] tags, in order. Index [0] carries
//     the NIP-01 simple reply marker (the engine only ever reads Antecedents[0]);
//     any further antecedents are preserved as plain ["e", <id>] tags so the
//     round-trip is lossless.
//   - Sender (author) -> PubKey and a ["p", <sender>] tag.
//   - exchange:domain:X -> ["t", X]   (NIP-01 topic filter)
//   - exchange:phase:X  -> ["phase", X]
//   - any other exchange tag -> ["x", <full-tag>] (lossless passthrough)
//   - Payload -> Content (opaque; the adapter never parses the payload)
//   - Timestamp/CampfireID/Instance -> dg_* preservation tags
//
// Loud degradation (hard constraint #4, the dontguess-553 lesson): a message with
// no recognised exchange operation tag is a conversion failure and returns an
// error rather than silently emitting a kind-0 event.
//
// The adapter never emits an embedding vector: embeddings live only in the
// operator's local vector index keyed by content_hash and are not carried on any
// proto.Message field, so there is nothing here to leak onto the wire.
func ToNostrEvent(msg *proto.Message) (*Event, error) {
	if msg == nil {
		return nil, fmt.Errorf("nostr: ToNostrEvent: nil message")
	}

	op := primaryOp(msg.Tags)
	if op == "" {
		return nil, fmt.Errorf("nostr: ToNostrEvent: message %q carries no recognised exchange operation tag", msg.ID)
	}
	kind, ok := kindForOp(op)
	if !ok {
		// Unreachable: primaryOp only returns ops kindForOp recognises. Guard
		// anyway so a future edit that desyncs the two maps fails loudly.
		return nil, fmt.Errorf("nostr: ToNostrEvent: no kind for operation %q", op)
	}

	tags := make([][]string, 0, len(msg.Tags)+len(msg.Antecedents)+4)

	// Antecedents -> e-tags (index 0 gets the NIP-01 reply marker).
	for i, ante := range msg.Antecedents {
		if i == 0 {
			tags = append(tags, []string{tagE, ante, "", replyMarker})
		} else {
			tags = append(tags, []string{tagE, ante})
		}
	}

	// Author -> p-tag.
	tags = append(tags, []string{tagP, msg.Sender})

	// Assign/scrip share a kind: preserve the exact sub-op.
	if kind == KindAssign || kind == KindScrip {
		tags = append(tags, []string{tagOp, op})
	}

	// Remaining exchange tags.
	for _, t := range msg.Tags {
		switch {
		case t == op:
			// Consumed by kind (base ops) or already preserved via the op
			// discriminator (assign/scrip). Skip either way.
			continue
		case strings.HasPrefix(t, domainPrefix):
			tags = append(tags, []string{tagT, strings.TrimPrefix(t, domainPrefix)})
		case strings.HasPrefix(t, exchange.TagPhasePrefix):
			tags = append(tags, []string{tagPhase, strings.TrimPrefix(t, exchange.TagPhasePrefix)})
		default:
			tags = append(tags, []string{tagX, t})
		}
	}

	// Preservation tags for campfire-era fields with no nostr-native home.
	tags = append(tags, []string{tagDGTimestamp, strconv.FormatInt(msg.Timestamp, 10)})
	if msg.CampfireID != "" {
		tags = append(tags, []string{tagDGCampfire, msg.CampfireID})
	}
	if msg.Instance != "" {
		tags = append(tags, []string{tagDGInstance, msg.Instance})
	}

	ev := &Event{
		ID:        msg.ID,
		PubKey:    msg.Sender,
		CreatedAt: msg.Timestamp / nanosPerSecond,
		Kind:      kind,
		Tags:      tags,
		Content:   string(msg.Payload),
	}
	return ev, nil
}

// FromNostrEvent converts a nostr Event back into an exchange message the
// unchanged engine folds. It is the inverse of ToNostrEvent.
//
// Loud degradation: an event whose kind is not a known regular exchange kind
// returns an error. The 30401 inventory projection is deliberately rejected — it
// is an addressable projection republished from the fold, NOT a folded message,
// and folding it as source of truth would reintroduce already-fixed scrip
// double-spend bugs (hard constraint #2).
func FromNostrEvent(ev *Event) (*proto.Message, error) {
	if ev == nil {
		return nil, fmt.Errorf("nostr: FromNostrEvent: nil event")
	}
	if ev.Kind == KindInventoryProjection {
		return nil, fmt.Errorf("nostr: FromNostrEvent: kind %d is an addressable projection, not a folded message", ev.Kind)
	}

	msg := &proto.Message{
		ID:          ev.ID,
		Sender:      ev.PubKey,
		Payload:     payloadFromContent(ev.Content),
		Antecedents: []string{},
		Tags:        []string{},
		Timestamp:   ev.CreatedAt * nanosPerSecond, // overridden by dg_ts if present
	}

	// Reconstruct the base op tag from the kind. Assign/scrip resolve their exact
	// sub-op from the ["op", <tag>] discriminator below. The op-discriminator (not
	// a bare phase tag) is the ratified mechanism for shared kinds — a phase tag
	// cannot distinguish assign's 7 sub-ops; see
	// docs/design/nostr-first-rebuild-decision.md §Nostr Architecture
	// (dontguess-c08 reconciliation note) and the tagOp handling below, which
	// validates the discriminator against assignOps/scripOps and fails loudly on
	// an unknown value or a stray op tag on a base kind.
	sharedKind := ev.Kind == KindAssign || ev.Kind == KindScrip
	if baseOp, ok := kindToBaseOp[ev.Kind]; ok {
		msg.Tags = append(msg.Tags, baseOp)
	} else if !sharedKind {
		return nil, fmt.Errorf("nostr: FromNostrEvent: unknown exchange kind %d", ev.Kind)
	}

	opFound := false
	for _, t := range ev.Tags {
		if len(t) == 0 {
			continue
		}
		switch t[0] {
		case tagE:
			if len(t) >= 2 {
				msg.Antecedents = append(msg.Antecedents, t[1])
			}
		case tagP:
			// Author reference; Sender is authoritative from PubKey. Ignore.
		case tagOp:
			if !sharedKind {
				// The op discriminator only applies to shared kinds (3405
				// assign*, 3411 scrip*). Base kinds (3401-3404) already fully
				// determine their op from the kind; an op tag on a base kind
				// is not part of the mapping and is ignored rather than
				// trusted (dontguess-c08).
				continue
			}
			if len(t) < 2 {
				continue
			}
			opSet := assignOps
			if ev.Kind == KindScrip {
				opSet = scripOps
			}
			if _, known := opSet[t[1]]; !known {
				return nil, fmt.Errorf("nostr: FromNostrEvent: kind %d op discriminator %q is not a known op", ev.Kind, t[1])
			}
			msg.Tags = append(msg.Tags, t[1])
			opFound = true
		case tagT:
			if len(t) >= 2 {
				msg.Tags = append(msg.Tags, domainPrefix+t[1])
			}
		case tagPhase:
			if len(t) >= 2 {
				msg.Tags = append(msg.Tags, exchange.TagPhasePrefix+t[1])
			}
		case tagX:
			if len(t) >= 2 {
				msg.Tags = append(msg.Tags, t[1])
			}
		case tagDGTimestamp:
			if len(t) >= 2 {
				if ns, err := strconv.ParseInt(t[1], 10, 64); err == nil {
					msg.Timestamp = ns
				} else {
					return nil, fmt.Errorf("nostr: FromNostrEvent: bad %s tag %q: %w", tagDGTimestamp, t[1], err)
				}
			}
		case tagDGCampfire:
			if len(t) >= 2 {
				msg.CampfireID = t[1]
			}
		case tagDGInstance:
			if len(t) >= 2 {
				msg.Instance = t[1]
			}
		}
	}

	if sharedKind && !opFound {
		return nil, fmt.Errorf("nostr: FromNostrEvent: kind %d requires an %q discriminator tag", ev.Kind, tagOp)
	}

	return msg, nil
}

// payloadFromContent maps event content back to a Message payload. An empty
// content yields a nil payload (the zero value the engine expects for a message
// with no body), mirroring how ToNostrEvent renders a nil payload as "".
func payloadFromContent(content string) []byte {
	if content == "" {
		return nil
	}
	return []byte(content)
}
