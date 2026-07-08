// Package relay is dontguess's single-relay transport: it PUBLISHES and
// SUBSCRIBES exchange events over one NIP-42-authed nostr relay at team tier.
//
// Design authority: docs/design/relay-transport.md §2.1. This file is the
// NIP-01 wire codec (frames.go); conn.go is the websocket lifecycle (dial +
// NIP-42 handshake + reconnect). The Intake (§2.4), Outbox (§2.3), watchdog
// (§2.5), and metrics fan-out are separate follow-on workstreams and are NOT
// built here — this package currently owns conn + codec only.
//
// LOCKED invariant honoured here (relay-transport.md §0.5): a malformed frame is
// a LOUD reject — every parse path returns a descriptive error and NEVER panics.
// A dumb or hostile relay can put arbitrary bytes on the wire; the reader must
// treat every frame as untrusted input and degrade loudly, never silently.
package relay

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/campfire-net/dontguess/pkg/identity"
)

// NIP-01 / NIP-42 frame labels — the first element of every wire array.
const (
	LabelREQ    = "REQ"
	LabelEVENT  = "EVENT"
	LabelEOSE   = "EOSE"
	LabelCLOSE  = "CLOSE"
	LabelOK     = "OK"
	LabelNOTICE = "NOTICE"
)

// Filter is a NIP-01 subscription filter (the subset dontguess needs, plus
// generic single-letter tag filters via Tags). All fields are optional; an
// empty Filter matches everything. Since/Until/Limit are pointers so "absent"
// is distinguishable from a zero value on the wire.
//
// Correctness of subscribe/backfill does NOT depend on Since/Until being exact
// (relay-transport.md §B, ADV-9): they are coarse fetch hints and the Sequencer's
// id-dedup absorbs any overlap. IDs is the targeted re-fetch used by the orphan
// watchdog (§2.5).
type Filter struct {
	IDs     []string
	Authors []string
	Kinds   []int
	Since   *int64
	Until   *int64
	Limit   *int
	// Tags holds generic "#<letter>" tag filters, keyed by the single-letter tag
	// name WITHOUT the '#' (e.g. Tags["e"] = []string{"<id>"} → wire "#e").
	Tags map[string][]string
}

// MarshalJSON renders the filter as a NIP-01 filter object, emitting only the
// fields that are set and rendering tag filters as "#e"/"#p" keys.
func (f Filter) MarshalJSON() ([]byte, error) {
	m := map[string]interface{}{}
	if len(f.IDs) > 0 {
		m["ids"] = f.IDs
	}
	if len(f.Authors) > 0 {
		m["authors"] = f.Authors
	}
	if len(f.Kinds) > 0 {
		m["kinds"] = f.Kinds
	}
	if f.Since != nil {
		m["since"] = *f.Since
	}
	if f.Until != nil {
		m["until"] = *f.Until
	}
	if f.Limit != nil {
		m["limit"] = *f.Limit
	}
	for name, vals := range f.Tags {
		if name == "" {
			return nil, fmt.Errorf("encode filter: empty tag-filter name")
		}
		m["#"+name] = vals
	}
	return json.Marshal(m)
}

// UnmarshalJSON parses a NIP-01 filter object, routing "#x" keys into Tags.
func (f *Filter) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("parse filter: %w", err)
	}
	*f = Filter{}
	for key, val := range raw {
		var err error
		switch {
		case key == "ids":
			err = json.Unmarshal(val, &f.IDs)
		case key == "authors":
			err = json.Unmarshal(val, &f.Authors)
		case key == "kinds":
			err = json.Unmarshal(val, &f.Kinds)
		case key == "since":
			var v int64
			if err = json.Unmarshal(val, &v); err == nil {
				f.Since = &v
			}
		case key == "until":
			var v int64
			if err = json.Unmarshal(val, &v); err == nil {
				f.Until = &v
			}
		case key == "limit":
			var v int
			if err = json.Unmarshal(val, &v); err == nil {
				f.Limit = &v
			}
		case strings.HasPrefix(key, "#") && len(key) >= 2:
			var vals []string
			if err = json.Unmarshal(val, &vals); err == nil {
				if f.Tags == nil {
					f.Tags = map[string][]string{}
				}
				f.Tags[key[1:]] = vals
			}
		default:
			// Unknown filter keys are ignored (forward-compat), not an error.
		}
		if err != nil {
			return fmt.Errorf("parse filter field %q: %w", key, err)
		}
	}
	return nil
}

// Frame is a parsed NIP-01 wire message. Type is one of the Label* constants;
// only the fields relevant to that type are populated.
type Frame struct {
	Type string

	// SubID is set for REQ, EOSE, CLOSE, and the relay→client form of EVENT.
	SubID string
	// Filters is set for REQ.
	Filters []Filter
	// Event is set for EVENT (both the client publish and relay delivery forms).
	Event *identity.Event
	// EventID / Accepted / Message are set for OK.
	EventID  string
	Accepted bool
	// Message is set for OK (relay's human note) and NOTICE (the notice text).
	Message string
}

// --- Encoders --------------------------------------------------------------

// EncodeReq builds a client→relay ["REQ", subID, filter...] frame. At least one
// filter is required by NIP-01; a subscription with no filter is rejected loudly
// rather than silently matching the entire relay.
func EncodeReq(subID string, filters ...Filter) ([]byte, error) {
	if subID == "" {
		return nil, fmt.Errorf("encode REQ: empty subscription id")
	}
	if len(filters) == 0 {
		return nil, fmt.Errorf("encode REQ: at least one filter required")
	}
	arr := make([]interface{}, 0, len(filters)+2)
	arr = append(arr, LabelREQ, subID)
	for i := range filters {
		arr = append(arr, filters[i])
	}
	return json.Marshal(arr)
}

// EncodeEvent builds the client→relay publish frame ["EVENT", event].
func EncodeEvent(ev *identity.Event) ([]byte, error) {
	if ev == nil {
		return nil, fmt.Errorf("encode EVENT: nil event")
	}
	return json.Marshal([]interface{}{LabelEVENT, ev})
}

// EncodeSubEvent builds the relay→client delivery frame ["EVENT", subID, event].
// It exists so a fake relay (and future Intake tests) can produce genuine
// three-element EVENT frames.
func EncodeSubEvent(subID string, ev *identity.Event) ([]byte, error) {
	if subID == "" {
		return nil, fmt.Errorf("encode EVENT: empty subscription id")
	}
	if ev == nil {
		return nil, fmt.Errorf("encode EVENT: nil event")
	}
	return json.Marshal([]interface{}{LabelEVENT, subID, ev})
}

// EncodeEOSE builds a relay→client ["EOSE", subID] frame.
func EncodeEOSE(subID string) ([]byte, error) {
	if subID == "" {
		return nil, fmt.Errorf("encode EOSE: empty subscription id")
	}
	return json.Marshal([]interface{}{LabelEOSE, subID})
}

// EncodeClose builds a client→relay ["CLOSE", subID] frame.
func EncodeClose(subID string) ([]byte, error) {
	if subID == "" {
		return nil, fmt.Errorf("encode CLOSE: empty subscription id")
	}
	return json.Marshal([]interface{}{LabelCLOSE, subID})
}

// EncodeOK builds a relay→client ["OK", eventID, accepted, message] frame.
func EncodeOK(eventID string, accepted bool, message string) ([]byte, error) {
	return json.Marshal([]interface{}{LabelOK, eventID, accepted, message})
}

// EncodeNotice builds a relay→client ["NOTICE", message] frame.
func EncodeNotice(message string) ([]byte, error) {
	return json.Marshal([]interface{}{LabelNOTICE, message})
}

// --- Parser ----------------------------------------------------------------

// ParseFrame parses any NIP-01 wire frame into a typed Frame. It NEVER panics:
// every malformed input (non-JSON, empty array, wrong label type, missing or
// mistyped elements) returns a descriptive error (LOCKED-5, loud reject).
func ParseFrame(raw []byte) (*Frame, error) {
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil, fmt.Errorf("parse frame: not a JSON array: %w", err)
	}
	if len(arr) == 0 {
		return nil, fmt.Errorf("parse frame: empty array")
	}
	var label string
	if err := json.Unmarshal(arr[0], &label); err != nil {
		return nil, fmt.Errorf("parse frame: label is not a string: %w", err)
	}
	switch label {
	case LabelREQ:
		return parseReq(arr)
	case LabelEVENT:
		return parseEvent(arr)
	case LabelEOSE:
		return parseSubIDOnly(LabelEOSE, arr)
	case LabelCLOSE:
		return parseSubIDOnly(LabelCLOSE, arr)
	case LabelOK:
		return parseOK(arr)
	case LabelNOTICE:
		return parseNotice(arr)
	default:
		return nil, fmt.Errorf("parse frame: unknown label %q", label)
	}
}

func parseReq(arr []json.RawMessage) (*Frame, error) {
	if len(arr) < 3 {
		return nil, fmt.Errorf("parse REQ: expected ≥3 elements (label, subID, filter…), got %d", len(arr))
	}
	var subID string
	if err := json.Unmarshal(arr[1], &subID); err != nil {
		return nil, fmt.Errorf("parse REQ: subscription id not a string: %w", err)
	}
	if subID == "" {
		return nil, fmt.Errorf("parse REQ: empty subscription id")
	}
	filters := make([]Filter, 0, len(arr)-2)
	for i := 2; i < len(arr); i++ {
		var f Filter
		if err := json.Unmarshal(arr[i], &f); err != nil {
			return nil, fmt.Errorf("parse REQ: filter %d: %w", i-2, err)
		}
		filters = append(filters, f)
	}
	return &Frame{Type: LabelREQ, SubID: subID, Filters: filters}, nil
}

// parseEvent handles BOTH NIP-01 EVENT forms: the client→relay publish
// ["EVENT", event] (2 elements) and the relay→client delivery
// ["EVENT", subID, event] (3 elements).
func parseEvent(arr []json.RawMessage) (*Frame, error) {
	switch len(arr) {
	case 2:
		ev, err := decodeEvent(arr[1])
		if err != nil {
			return nil, err
		}
		return &Frame{Type: LabelEVENT, Event: ev}, nil
	default:
		if len(arr) < 3 {
			return nil, fmt.Errorf("parse EVENT: expected 2 or 3 elements, got %d", len(arr))
		}
		var subID string
		if err := json.Unmarshal(arr[1], &subID); err != nil {
			return nil, fmt.Errorf("parse EVENT: subscription id not a string: %w", err)
		}
		if subID == "" {
			return nil, fmt.Errorf("parse EVENT: empty subscription id")
		}
		ev, err := decodeEvent(arr[2])
		if err != nil {
			return nil, err
		}
		return &Frame{Type: LabelEVENT, SubID: subID, Event: ev}, nil
	}
}

func decodeEvent(raw json.RawMessage) (*identity.Event, error) {
	// Reject a JSON that is not an object (e.g. a bare string or array) up front
	// so a malformed EVENT is a loud reject rather than a zero-valued Event.
	trimmed := strings.TrimSpace(string(raw))
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, fmt.Errorf("parse EVENT: event payload is not a JSON object")
	}
	var ev identity.Event
	if err := json.Unmarshal(raw, &ev); err != nil {
		return nil, fmt.Errorf("parse EVENT: event object: %w", err)
	}
	return &ev, nil
}

func parseSubIDOnly(label string, arr []json.RawMessage) (*Frame, error) {
	if len(arr) < 2 {
		return nil, fmt.Errorf("parse %s: expected 2 elements, got %d", label, len(arr))
	}
	var subID string
	if err := json.Unmarshal(arr[1], &subID); err != nil {
		return nil, fmt.Errorf("parse %s: subscription id not a string: %w", label, err)
	}
	if subID == "" {
		return nil, fmt.Errorf("parse %s: empty subscription id", label)
	}
	return &Frame{Type: label, SubID: subID}, nil
}

func parseOK(arr []json.RawMessage) (*Frame, error) {
	if len(arr) < 3 {
		return nil, fmt.Errorf("parse OK: expected ≥3 elements, got %d", len(arr))
	}
	var eventID string
	if err := json.Unmarshal(arr[1], &eventID); err != nil {
		return nil, fmt.Errorf("parse OK: event id not a string: %w", err)
	}
	var accepted bool
	if err := json.Unmarshal(arr[2], &accepted); err != nil {
		return nil, fmt.Errorf("parse OK: accepted flag not a bool: %w", err)
	}
	f := &Frame{Type: LabelOK, EventID: eventID, Accepted: accepted}
	if len(arr) >= 4 {
		if err := json.Unmarshal(arr[3], &f.Message); err != nil {
			return nil, fmt.Errorf("parse OK: message not a string: %w", err)
		}
	}
	return f, nil
}

func parseNotice(arr []json.RawMessage) (*Frame, error) {
	if len(arr) < 2 {
		return nil, fmt.Errorf("parse NOTICE: expected 2 elements, got %d", len(arr))
	}
	var msg string
	if err := json.Unmarshal(arr[1], &msg); err != nil {
		return nil, fmt.Errorf("parse NOTICE: message not a string: %w", err)
	}
	return &Frame{Type: LabelNOTICE, Message: msg}, nil
}

// sortedTagNames is a small determinism helper used by tests to compare tag
// filters without depending on Go map iteration order.
func sortedTagNames(m map[string][]string) []string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}
