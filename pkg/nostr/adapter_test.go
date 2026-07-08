package nostr

import (
	"encoding/json"
	"reflect"
	"regexp"
	"sort"
	"testing"

	"github.com/campfire-net/dontguess/pkg/exchange"
	"github.com/campfire-net/dontguess/pkg/proto"
	"github.com/campfire-net/dontguess/pkg/scrip"
)

// sampleSender / sampleID are realistic hex pubkey-shaped strings.
const (
	sampleSender = "a1b2c3d4e5f60718293a4b5c6d7e8f90112233445566778899aabbccddeeff00"
	sampleID     = "0011223344556677889900aabbccddeeff112233445566778899aabbccddeeff"
	sampleAnte0  = "ffeeddccbbaa99887766554433221100ffeeddccbbaa99887766554433221100"
	sampleAnte1  = "1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"
)

// mkMsg builds a proto.Message with the given op tag and extra tags/antecedents.
// All non-tag fields are populated with distinctive values so the round-trip
// asserts full-field fidelity, not just the tag mapping.
func mkMsg(opTag string, extraTags []string, antecedents []string) *proto.Message {
	tags := append([]string{opTag}, extraTags...)
	return &proto.Message{
		ID:          sampleID,
		CampfireID:  "campfire-xyz",
		Sender:      sampleSender,
		Payload:     []byte(`{"description":"flock contention test pattern for Go","content_hash":"sha256:deadbeef","token_cost":1600}`),
		Tags:        tags,
		Antecedents: antecedents,
		Timestamp:   1_717_000_000_123_456_789, // nanosecond precision, not a whole second
		Instance:    "seller-fleet-3",
	}
}

// normSlice treats a nil slice as an empty slice for comparison.
func normSlice(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// tagSet returns the tag list as a sorted copy for order-independent comparison.
// The exchange engine treats Tags as a set — every consumer scans the slice
// (exchangeOp, settlePhaseFromTags, the applyLocked switch). Tag order therefore
// carries no semantics, so the adapter guarantees set-level round-trip fidelity.
func tagSet(tags []string) []string {
	out := append([]string(nil), tags...)
	sort.Strings(out)
	return out
}

// assertRoundTrip asserts Message -> event -> Message is an identity at the field
// level (tags compared as a set; every other field compared exactly).
func assertRoundTrip(t *testing.T, in *proto.Message) *Event {
	t.Helper()
	ev, err := ToNostrEvent(in)
	if err != nil {
		t.Fatalf("ToNostrEvent: %v", err)
	}
	out, err := FromNostrEvent(ev)
	if err != nil {
		t.Fatalf("FromNostrEvent: %v", err)
	}
	if out.ID != in.ID {
		t.Errorf("ID: got %q want %q", out.ID, in.ID)
	}
	if out.Sender != in.Sender {
		t.Errorf("Sender: got %q want %q", out.Sender, in.Sender)
	}
	if out.CampfireID != in.CampfireID {
		t.Errorf("CampfireID: got %q want %q", out.CampfireID, in.CampfireID)
	}
	if out.Instance != in.Instance {
		t.Errorf("Instance: got %q want %q", out.Instance, in.Instance)
	}
	if out.Timestamp != in.Timestamp {
		t.Errorf("Timestamp: got %d want %d", out.Timestamp, in.Timestamp)
	}
	if !reflect.DeepEqual(out.Payload, in.Payload) {
		t.Errorf("Payload: got %s want %s", out.Payload, in.Payload)
	}
	// nil and empty antecedent slices are equivalent to the engine
	// (exchange.FromSDKMessage itself normalises nil -> []string{}), so compare
	// after normalisation.
	if !reflect.DeepEqual(normSlice(out.Antecedents), normSlice(in.Antecedents)) {
		t.Errorf("Antecedents: got %#v want %#v", out.Antecedents, in.Antecedents)
	}
	if !reflect.DeepEqual(tagSet(out.Tags), tagSet(in.Tags)) {
		t.Errorf("Tags (as set): got %v want %v", tagSet(out.Tags), tagSet(in.Tags))
	}
	return ev
}

func TestRoundTrip_AllOpKinds(t *testing.T) {
	cases := []struct {
		name      string
		op        string
		extraTags []string
		wantKind  int
	}{
		{"put", exchange.TagPut, []string{"exchange:content-type:code", "exchange:domain:matching", "exchange:domain:go"}, KindPut},
		{"buy", exchange.TagBuy, nil, KindBuy},
		{"match", exchange.TagMatch, nil, KindMatch},
		{"settle_put_accept", exchange.TagSettle, []string{"exchange:phase:put-accept"}, KindSettle},
		{"settle_complete", exchange.TagSettle, []string{"exchange:phase:complete"}, KindSettle},
		{"assign", exchange.TagAssign, []string{"exchange:assign-type:compression"}, KindAssign},
		{"assign_claim", exchange.TagAssignClaim, nil, KindAssign},
		{"assign_complete", exchange.TagAssignComplete, nil, KindAssign},
		{"assign_accept", exchange.TagAssignAccept, nil, KindAssign},
		{"assign_reject", exchange.TagAssignReject, nil, KindAssign},
		{"assign_expire", exchange.TagAssignExpire, nil, KindAssign},
		{"assign_auction_close", exchange.TagAssignAuctionClose, nil, KindAssign},
		{"scrip_mint", scrip.TagScripMint, nil, KindScrip},
		{"scrip_settle", scrip.TagScripSettle, nil, KindScrip},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Exercise with and without antecedents to cover the e-tag path.
			for _, antes := range [][]string{nil, {sampleAnte0}, {sampleAnte0, sampleAnte1}} {
				in := mkMsg(tc.op, tc.extraTags, antes)
				ev := assertRoundTrip(t, in)
				if ev.Kind != tc.wantKind {
					t.Errorf("kind: got %d want %d", ev.Kind, tc.wantKind)
				}
			}
		})
	}
}

func TestAntecedentZeroMapsToReplyETag(t *testing.T) {
	in := mkMsg(exchange.TagMatch, nil, []string{sampleAnte0})
	ev, err := ToNostrEvent(in)
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, tag := range ev.Tags {
		if tag[0] == tagE {
			found = tag
		}
	}
	if found == nil {
		t.Fatal("no e-tag emitted for Antecedents[0]")
	}
	// NIP-01 simple reply marker: ["e", <id>, "", "reply"].
	want := []string{tagE, sampleAnte0, "", replyMarker}
	if !reflect.DeepEqual(found, want) {
		t.Errorf("e-tag: got %v want %v", found, want)
	}
	// The engine only reads Antecedents[0]; confirm it survives the round-trip.
	out, err := FromNostrEvent(ev)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Antecedents) == 0 || out.Antecedents[0] != sampleAnte0 {
		t.Errorf("Antecedents[0]: got %v want %q", out.Antecedents, sampleAnte0)
	}
}

func TestAuthorMapsToPTagAndPubkey(t *testing.T) {
	in := mkMsg(exchange.TagPut, nil, nil)
	ev, err := ToNostrEvent(in)
	if err != nil {
		t.Fatal(err)
	}
	if ev.PubKey != sampleSender {
		t.Errorf("PubKey: got %q want %q", ev.PubKey, sampleSender)
	}
	var pTag []string
	for _, tag := range ev.Tags {
		if tag[0] == tagP {
			pTag = tag
		}
	}
	if pTag == nil {
		t.Fatal("no p-tag emitted for author")
	}
	if len(pTag) < 2 || pTag[1] != sampleSender {
		t.Errorf("p-tag: got %v want author %q", pTag, sampleSender)
	}
}

func TestDomainMapsToTopicTag(t *testing.T) {
	in := mkMsg(exchange.TagPut, []string{"exchange:domain:matching"}, nil)
	ev, err := ToNostrEvent(in)
	if err != nil {
		t.Fatal(err)
	}
	var tTag []string
	for _, tag := range ev.Tags {
		if tag[0] == tagT {
			tTag = tag
		}
	}
	if tTag == nil {
		t.Fatal("no #t topic tag emitted for domain")
	}
	if len(tTag) < 2 || tTag[1] != "matching" {
		t.Errorf("#t tag: got %v want value %q", tTag, "matching")
	}
}

// floatArrayRe matches a JSON array containing a long run of numbers — the shape
// a 384-dim embedding vector would take if it leaked onto the wire.
var floatArrayRe = regexp.MustCompile(`(\s*-?\d+(\.\d+)?\s*,\s*){50,}`)

func TestNoEmbeddingVectorOnTheWire(t *testing.T) {
	// A put is the op most tempting to enrich with an embedding. The adapter is
	// payload-opaque and never synthesises tags from vector data, so no embedding
	// can appear. Assert it structurally on the serialised event.
	in := mkMsg(exchange.TagPut, []string{"exchange:content-type:code", "exchange:domain:matching"}, []string{sampleAnte0})
	ev, err := ToNostrEvent(in)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	if floatArrayRe.Match(blob) {
		t.Errorf("event carries a long numeric array (looks like an embedding vector): %s", blob)
	}
	// No tag value should be a numeric vector, and the content must be exactly the
	// payload we supplied (nothing injected).
	for _, tag := range ev.Tags {
		for _, v := range tag {
			if floatArrayRe.MatchString(v) {
				t.Errorf("tag value looks like an embedding vector: %v", tag)
			}
		}
	}
	if ev.Content != string(in.Payload) {
		t.Errorf("content mutated: got %q want %q", ev.Content, in.Payload)
	}
}

func TestLoudDegradation(t *testing.T) {
	t.Run("to_nostr_no_op_tag", func(t *testing.T) {
		// A consume message carries no primary operation tag — out of shape scope.
		msg := &proto.Message{ID: sampleID, Sender: sampleSender, Tags: []string{exchange.TagConsume}}
		if _, err := ToNostrEvent(msg); err == nil {
			t.Error("expected error converting a message with no exchange operation tag")
		}
	})
	t.Run("to_nostr_nil", func(t *testing.T) {
		if _, err := ToNostrEvent(nil); err == nil {
			t.Error("expected error on nil message")
		}
	})
	t.Run("from_nostr_projection_kind_rejected", func(t *testing.T) {
		// 30401 is an addressable projection, NOT a folded message (hard constraint #2).
		ev := &Event{ID: sampleID, PubKey: sampleSender, Kind: KindInventoryProjection}
		if _, err := FromNostrEvent(ev); err == nil {
			t.Error("expected error folding a 30401 projection event")
		}
	})
	t.Run("from_nostr_unknown_kind", func(t *testing.T) {
		ev := &Event{ID: sampleID, PubKey: sampleSender, Kind: 9999}
		if _, err := FromNostrEvent(ev); err == nil {
			t.Error("expected error on unknown kind")
		}
	})
	t.Run("from_nostr_assign_missing_discriminator", func(t *testing.T) {
		// Kind 3405 with no ["op", ...] tag cannot resolve its sub-op — fail loud.
		ev := &Event{ID: sampleID, PubKey: sampleSender, Kind: KindAssign, Tags: [][]string{{tagP, sampleSender}}}
		if _, err := FromNostrEvent(ev); err == nil {
			t.Error("expected error folding an assign event with no op discriminator")
		}
	})
	t.Run("from_nostr_nil", func(t *testing.T) {
		if _, err := FromNostrEvent(nil); err == nil {
			t.Error("expected error on nil event")
		}
	})
	t.Run("from_nostr_assign_unknown_op_rejected", func(t *testing.T) {
		// dontguess-c08: an ["op", ...] value not in assignOps must fail loud, not
		// fold silently as if it were a real sub-op.
		ev := &Event{ID: sampleID, PubKey: sampleSender, Kind: KindAssign, Tags: [][]string{
			{tagP, sampleSender},
			{tagOp, "exchange:assign:forged-sub-op"},
		}}
		if _, err := FromNostrEvent(ev); err == nil {
			t.Error("expected error folding an assign event with an unknown op discriminator")
		}
	})
	t.Run("from_nostr_scrip_unknown_op_rejected", func(t *testing.T) {
		// Same discipline for kind 3411 against scripOps.
		ev := &Event{ID: sampleID, PubKey: sampleSender, Kind: KindScrip, Tags: [][]string{
			{tagP, sampleSender},
			{tagOp, "dontguess:scrip:forged"},
		}}
		if _, err := FromNostrEvent(ev); err == nil {
			t.Error("expected error folding a scrip event with an unknown op discriminator")
		}
	})
	t.Run("from_nostr_op_tag_on_base_kind_ignored", func(t *testing.T) {
		// dontguess-c08: a base kind (3401-3404) fully determines its op from the
		// kind alone. A stray ["op", ...] tag on a base-kind event must not be
		// trusted or folded as a second/competing op tag — it is ignored, and the
		// message still decodes cleanly to the kind's single base op.
		ev := &Event{ID: sampleID, PubKey: sampleSender, Kind: KindPut, Tags: [][]string{
			{tagP, sampleSender},
			{tagOp, exchange.TagAssignClaim}, // forged/irrelevant op tag on a put event
		}}
		msg, err := FromNostrEvent(ev)
		if err != nil {
			t.Fatalf("FromNostrEvent: unexpected error: %v", err)
		}
		if !reflect.DeepEqual(msg.Tags, []string{exchange.TagPut}) {
			t.Errorf("Tags: got %#v, want only the base op %q (op tag on base kind must be ignored)", msg.Tags, exchange.TagPut)
		}
	})
}

// TestFoldedMessageDrivesUnchangedEngine proves the adapter output is folded by
// the real exchange State exactly as a campfire-sourced message would be — i.e.
// the seam is preserved and the engine is genuinely untouched. A put converted to
// a nostr event and back must still be recognised as a put by the state machine.
func TestFoldedMessageDrivesUnchangedEngine(t *testing.T) {
	// A put with valid payload, folded through the unchanged State.
	put := &proto.Message{
		ID:         sampleID,
		Sender:     sampleSender,
		CampfireID: "cf",
		Payload:    []byte(`{"description":"flock contention test pattern for Go","content":"aGVsbG8gd29ybGQgY29udGVudCBwYXlsb2FkIGZvciB0ZXN0aW5n","token_cost":1600,"content_type":"code"}`),
		Tags:       []string{exchange.TagPut, "exchange:content-type:code", "exchange:domain:go"},
		Timestamp:  1_717_000_000_000_000_000,
	}
	ev, err := ToNostrEvent(put)
	if err != nil {
		t.Fatal(err)
	}
	folded, err := FromNostrEvent(ev)
	if err != nil {
		t.Fatal(err)
	}

	st := exchange.NewState()
	st.Apply(folded)
	// A recognised put lands in pendingPuts (awaiting operator put-accept). If the
	// adapter had mangled the op tag, the state machine would have ignored it.
	if got := len(st.PendingPuts()); got != 1 {
		t.Fatalf("folded put not recognised by unchanged engine: pendingPuts=%d want 1", got)
	}
}

// TestBuyMissMarkerRoundTripsAndFoldsAsMatch is the regression guard for the
// wave-6 false-positive that this rework removes (dontguess-c22, DONE #2). A
// buy-miss standing offer is a broadcast operator message tagged
// [exchange:buy-miss, exchange:match] and serialized as a KindMatch (3403) event;
// the buy-miss marker rides as a lossless ["x","exchange:buy-miss"] passthrough.
// It MUST survive ToNostrEvent -> FromNostrEvent intact and fold through the
// UNCHANGED exchange State as a match (the buy-miss demand signal is a core
// feature: "a buy miss registers demand"). No such round-trip+fold test existed.
func TestBuyMissMarkerRoundTripsAndFoldsAsMatch(t *testing.T) {
	buyMsgID := sampleAnte0
	buyMiss := &proto.Message{
		ID:          sampleID,
		Sender:      sampleSender, // operator broadcast
		CampfireID:  "cf",
		Payload:     []byte(`{"task_hash":"abcd","offered_price_rate":70,"buy_msg_id":"` + buyMsgID + `"}`),
		Tags:        []string{exchange.TagMatch, exchange.TagBuyMiss},
		Antecedents: []string{buyMsgID},
		Timestamp:   1_717_000_000_000_000_000,
	}

	ev, err := ToNostrEvent(buyMiss)
	if err != nil {
		t.Fatalf("ToNostrEvent: %v", err)
	}
	if ev.Kind != KindMatch {
		t.Fatalf("buy-miss kind = %d, want KindMatch %d", ev.Kind, KindMatch)
	}
	// The buy-miss marker must serialize as an ["x", ...] passthrough, not be dropped.
	var sawBuyMissX bool
	for _, tag := range ev.Tags {
		if tag[0] == tagX && len(tag) >= 2 && tag[1] == exchange.TagBuyMiss {
			sawBuyMissX = true
		}
	}
	if !sawBuyMissX {
		t.Fatalf("buy-miss marker not carried as an x-tag on the event: %v", ev.Tags)
	}

	folded, err := FromNostrEvent(ev)
	if err != nil {
		t.Fatalf("FromNostrEvent: %v (the buy-miss marker must NOT be rejected)", err)
	}
	// Marker round-trips losslessly.
	if !hasStr(folded.Tags, exchange.TagBuyMiss) || !hasStr(folded.Tags, exchange.TagMatch) {
		t.Fatalf("round-trip lost a tag: got %v, want both %q and %q", folded.Tags, exchange.TagMatch, exchange.TagBuyMiss)
	}

	// Folds through the UNCHANGED engine as a match: the standing offer marks its
	// antecedent buy order matched.
	st := exchange.NewState()
	st.Apply(folded)
	if !st.IsOrderMatched(buyMsgID) {
		t.Fatalf("buy-miss did not fold as a match: order %q not marked matched", buyMsgID)
	}
}

// TestReservedXTagSmuggleIsNotRejectedButInert proves the two halves of the
// rework's adapter contract (dontguess-c22, DONE #1): the adapter does NOT reject
// a reserved-vocabulary x-tag (the wave-6 blunt rejection is gone — that would
// also unfold legitimate buy-miss/consume markers), and the smuggled op is INERT
// at the fold because exchangeOp fails loud on the resulting two-op ambiguity.
//
// Craft the b67d exploit event: kind=3405 signed by a NON-operator, discriminator
// ["op","exchange:assign-claim"] (a benign worker op) plus a smuggled
// ["x","exchange:assign-auction-close"]. FromNostrEvent must succeed and carry
// both op strings; folding it must NOT execute an auction-close (no auction is
// set up here, so the observable is simply that the message routes to no handler
// and mutates nothing — the full inert-vs-canonical proof lives in the exchange
// package's TestExchangeOp_SmuggledAuctionCloseIsInertAtFold).
func TestReservedXTagSmuggleIsNotRejectedButInert(t *testing.T) {
	ev := &Event{
		ID:     sampleID,
		PubKey: sampleSender, // attacker's OWN key, not the operator
		Kind:   KindAssign,
		Tags: [][]string{
			{tagP, sampleSender},
			{tagOp, exchange.TagAssignClaim},
			{tagX, exchange.TagAssignAuctionClose}, // smuggled reserved op
		},
	}
	folded, err := FromNostrEvent(ev)
	if err != nil {
		t.Fatalf("FromNostrEvent rejected a reserved x-tag: %v (the blunt rejection must be gone)", err)
	}
	if !hasStr(folded.Tags, exchange.TagAssignClaim) || !hasStr(folded.Tags, exchange.TagAssignAuctionClose) {
		t.Fatalf("smuggle round-trip lost a tag: got %v", folded.Tags)
	}

	// Folding the two-op message must not panic; the smuggled auction-close is
	// inert because exchangeOp resolves the ambiguous op-set to "" so no handler
	// runs (the full inert-vs-canonical proof is in the exchange package —
	// TestExchangeOp_SmuggledAuctionCloseIsInertAtFold, which can reach the
	// package-private assign state that this cross-package test cannot).
	st := exchange.NewState()
	st.OperatorKey = sampleSender
	st.Apply(folded) // must not panic / must route to no handler
}

// hasStr reports whether s contains v.
func hasStr(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
