package relay

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/campfire-net/dontguess/pkg/identity"
)

// sampleEvent is a fully-populated (unsigned-shape) nostr event used to prove
// the codec preserves every field across encode→parse. The signature/id values
// are arbitrary strings — the codec is agnostic to their cryptographic validity
// (verification is the Intake's job, relay-transport.md §2.4, not the codec's).
func sampleEvent() *identity.Event {
	return &identity.Event{
		ID:        "abc123",
		PubKey:    "deadbeef",
		CreatedAt: 1700000000,
		Kind:      3401,
		Tags:      [][]string{{"e", "antecedent-id"}, {"dg_ts", "42"}},
		Content:   "hello \"world\" <&>",
		Sig:       "cafef00d",
	}
}

func i64(v int64) *int64 { return &v }
func intp(v int) *int    { return &v }

// TestFrameRoundTrip encodes every frame type and parses it back, asserting the
// parsed Frame carries exactly the fields that went in.
func TestFrameRoundTrip(t *testing.T) {
	t.Parallel()
	ev := sampleEvent()

	tests := []struct {
		name   string
		encode func() ([]byte, error)
		want   Frame
	}{
		{
			name:   "REQ single filter",
			encode: func() ([]byte, error) { return EncodeReq("sub1", Filter{Kinds: []int{3401, 3402}, Since: i64(100)}) },
			want:   Frame{Type: LabelREQ, SubID: "sub1", Filters: []Filter{{Kinds: []int{3401, 3402}, Since: i64(100)}}},
		},
		{
			name: "REQ multi filter with ids and tags",
			encode: func() ([]byte, error) {
				return EncodeReq("sub2",
					Filter{IDs: []string{"id1", "id2"}, Limit: intp(10)},
					Filter{Authors: []string{"npub-hex"}, Tags: map[string][]string{"e": {"anc"}}, Until: i64(9)},
				)
			},
			want: Frame{Type: LabelREQ, SubID: "sub2", Filters: []Filter{
				{IDs: []string{"id1", "id2"}, Limit: intp(10)},
				{Authors: []string{"npub-hex"}, Tags: map[string][]string{"e": {"anc"}}, Until: i64(9)},
			}},
		},
		{
			name:   "EVENT publish form",
			encode: func() ([]byte, error) { return EncodeEvent(ev) },
			want:   Frame{Type: LabelEVENT, Event: ev},
		},
		{
			name:   "EVENT delivery form",
			encode: func() ([]byte, error) { return EncodeSubEvent("sub3", ev) },
			want:   Frame{Type: LabelEVENT, SubID: "sub3", Event: ev},
		},
		{
			name:   "EOSE",
			encode: func() ([]byte, error) { return EncodeEOSE("sub4") },
			want:   Frame{Type: LabelEOSE, SubID: "sub4"},
		},
		{
			name:   "CLOSE",
			encode: func() ([]byte, error) { return EncodeClose("sub5") },
			want:   Frame{Type: LabelCLOSE, SubID: "sub5"},
		},
		{
			name:   "OK accepted with message",
			encode: func() ([]byte, error) { return EncodeOK("evid", true, "stored") },
			want:   Frame{Type: LabelOK, EventID: "evid", Accepted: true, Message: "stored"},
		},
		{
			name:   "OK rejected",
			encode: func() ([]byte, error) { return EncodeOK("evid2", false, "blocked: pow") },
			want:   Frame{Type: LabelOK, EventID: "evid2", Accepted: false, Message: "blocked: pow"},
		},
		{
			name:   "NOTICE",
			encode: func() ([]byte, error) { return EncodeNotice("relay says hi") },
			want:   Frame{Type: LabelNOTICE, Message: "relay says hi"},
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			raw, err := tc.encode()
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			got, err := ParseFrame(raw)
			if err != nil {
				t.Fatalf("ParseFrame(%s): %v", raw, err)
			}
			assertFrameEqual(t, &tc.want, got)
		})
	}
}

func assertFrameEqual(t *testing.T, want, got *Frame) {
	t.Helper()
	if want.Type != got.Type {
		t.Fatalf("Type: got %q want %q", got.Type, want.Type)
	}
	if want.SubID != got.SubID {
		t.Fatalf("SubID: got %q want %q", got.SubID, want.SubID)
	}
	if want.EventID != got.EventID || want.Accepted != got.Accepted || want.Message != got.Message {
		t.Fatalf("OK/NOTICE fields: got (%q,%v,%q) want (%q,%v,%q)",
			got.EventID, got.Accepted, got.Message, want.EventID, want.Accepted, want.Message)
	}
	if (want.Event == nil) != (got.Event == nil) {
		t.Fatalf("Event presence: got %v want %v", got.Event, want.Event)
	}
	if want.Event != nil && !reflect.DeepEqual(*want.Event, *got.Event) {
		t.Fatalf("Event: got %+v want %+v", *got.Event, *want.Event)
	}
	if len(want.Filters) != len(got.Filters) {
		t.Fatalf("Filters len: got %d want %d", len(got.Filters), len(want.Filters))
	}
	for i := range want.Filters {
		if !reflect.DeepEqual(want.Filters[i], got.Filters[i]) {
			t.Fatalf("Filter[%d]: got %+v want %+v", i, got.Filters[i], want.Filters[i])
		}
	}
}

// TestParseFrameMalformed proves every malformed frame is a LOUD reject: a
// descriptive error and NEVER a panic (LOCKED-5, relay-transport.md §0.5). The
// recover() guard fails the test if any input panics.
func TestParseFrameMalformed(t *testing.T) {
	t.Parallel()
	malformed := []struct {
		name string
		raw  string
	}{
		{"not json", `not json at all`},
		{"json object not array", `{"foo":"bar"}`},
		{"empty array", `[]`},
		{"label not a string", `[42, "sub"]`},
		{"unknown label", `["FOObar", "x"]`},
		{"REQ missing filter", `["REQ", "sub"]`},
		{"REQ subid not string", `["REQ", 5, {}]`},
		{"REQ empty subid", `["REQ", "", {}]`},
		{"REQ filter not object", `["REQ", "sub", "notanobject"]`},
		{"REQ filter bad field type", `["REQ", "sub", {"kinds": "shouldbearray"}]`},
		{"EVENT one element", `["EVENT"]`},
		{"EVENT payload not object", `["EVENT", "stringnotevent"]`},
		{"EVENT delivery empty subid", `["EVENT", "", {"id":"x"}]`},
		{"EVENT delivery payload not object", `["EVENT", "sub", 12345]`},
		{"EOSE missing subid", `["EOSE"]`},
		{"EOSE empty subid", `["EOSE", ""]`},
		{"CLOSE subid not string", `["CLOSE", true]`},
		{"OK too few elements", `["OK", "id"]`},
		{"OK accepted not bool", `["OK", "id", "yes"]`},
		{"OK message not string", `["OK", "id", true, 99]`},
		{"NOTICE missing message", `["NOTICE"]`},
		{"NOTICE message not string", `["NOTICE", 123]`},
		{"truncated json", `["REQ", "sub"`},
	}

	for _, tc := range malformed {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("ParseFrame PANICKED on malformed input %q: %v", tc.raw, r)
				}
			}()
			f, err := ParseFrame([]byte(tc.raw))
			if err == nil {
				t.Fatalf("ParseFrame(%q) returned no error (got frame %+v); malformed input must be rejected loudly", tc.raw, f)
			}
		})
	}
}

// TestFilterOmitsEmptyFields proves the filter wire form only carries set
// fields — an empty filter is "{}" (match-all), not a wall of nulls/zeros that
// would over-constrain a subscription.
func TestFilterOmitsEmptyFields(t *testing.T) {
	t.Parallel()
	raw, err := json.Marshal(Filter{})
	if err != nil {
		t.Fatalf("marshal empty filter: %v", err)
	}
	if string(raw) != "{}" {
		t.Fatalf("empty filter marshalled to %q, want %q", raw, "{}")
	}

	raw, err = json.Marshal(Filter{Kinds: []int{3401}, Since: i64(5)})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := m["until"]; ok {
		t.Fatalf("unset field 'until' leaked into wire form: %s", raw)
	}
	if _, ok := m["ids"]; ok {
		t.Fatalf("unset field 'ids' leaked into wire form: %s", raw)
	}
	if _, ok := m["kinds"]; !ok {
		t.Fatalf("set field 'kinds' missing from wire form: %s", raw)
	}
}

func TestSortedTagNames(t *testing.T) {
	t.Parallel()
	got := sortedTagNames(map[string][]string{"p": {"1"}, "e": {"2"}, "a": {"3"}})
	if !reflect.DeepEqual(got, []string{"a", "e", "p"}) {
		t.Fatalf("sortedTagNames: got %v", got)
	}
}
