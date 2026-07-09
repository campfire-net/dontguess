package main

import (
	"fmt"
	"reflect"
	"sort"
	"testing"

	"github.com/campfire-net/dontguess/pkg/exchange"
	"github.com/campfire-net/dontguess/pkg/nostr"
)

// --------------------------------------------------------------------------
// Filter shape (kinds / #t / since / until / limit) — replaces the deleted
// pkg/exchange.StandardViews() 12-view assertion (views_test.go), one test
// per view the three CLI commands actually query.
// --------------------------------------------------------------------------

func sortedTags(tags []string) []string {
	out := append([]string(nil), tags...)
	sort.Strings(out)
	return out
}

func TestReqFilter_KindShapes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		filter ReqFilter
		want   []string
	}{
		{"puts", putsFilter(0), []string{exchange.TagPut}},
		{"buys", buysFilter(0), []string{exchange.TagBuy}},
		{"match-results", matchesFilter(0), []string{exchange.TagMatch}},
		{"settlements", settlementsFilter(0), []string{exchange.TagSettle}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !reflect.DeepEqual(sortedTags(tc.filter.legacyTags()), sortedTags(tc.want)) {
				t.Errorf("%s.legacyTags() = %v, want %v", tc.name, tc.filter.legacyTags(), tc.want)
			}
			if len(tc.filter.Kinds) != 1 {
				t.Errorf("%s: expected exactly one kind, got %v", tc.name, tc.filter.Kinds)
			}
		})
	}
}

func TestReqFilter_TagShapes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		filter ReqFilter
		want   []string
	}{
		{"put-accepts", putAcceptsFilter(0), []string{exchange.TagPhasePrefix + exchange.SettlePhaseStrPutAccept}},
		{"put-rejects", putRejectsFilter(0), []string{exchange.TagPhasePrefix + exchange.SettlePhaseStrPutReject}},
		{"consumes", consumesFilter(0), []string{exchange.TagConsume}},
		{"buy-miss", buyMissFilter(0), []string{exchange.TagBuyMiss}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !reflect.DeepEqual(sortedTags(tc.filter.legacyTags()), sortedTags(tc.want)) {
				t.Errorf("%s.legacyTags() = %v, want %v", tc.name, tc.filter.legacyTags(), tc.want)
			}
			if len(tc.filter.Kinds) != 0 {
				t.Errorf("%s: expected no Kinds (tag-discriminated, not kind-discriminated), got %v", tc.name, tc.filter.Kinds)
			}
		})
	}
}

// TestReqFilter_KindsMatchNostrPackage locks the filter constructors to the
// authoritative kind numbers in pkg/nostr/kinds.go — if that package's kind
// assignments ever change, this test catches the drift instead of silently
// querying the wrong messages.
func TestReqFilter_KindsMatchNostrPackage(t *testing.T) {
	t.Parallel()

	if got := putsFilter(0).Kinds[0]; got != nostr.KindPut {
		t.Errorf("putsFilter kind = %d, want nostr.KindPut (%d)", got, nostr.KindPut)
	}
	if got := buysFilter(0).Kinds[0]; got != nostr.KindBuy {
		t.Errorf("buysFilter kind = %d, want nostr.KindBuy (%d)", got, nostr.KindBuy)
	}
	if got := matchesFilter(0).Kinds[0]; got != nostr.KindMatch {
		t.Errorf("matchesFilter kind = %d, want nostr.KindMatch (%d)", got, nostr.KindMatch)
	}
	if got := settlementsFilter(0).Kinds[0]; got != nostr.KindSettle {
		t.Errorf("settlementsFilter kind = %d, want nostr.KindSettle (%d)", got, nostr.KindSettle)
	}
}

func TestReqFilter_SinceCarriesThrough(t *testing.T) {
	t.Parallel()

	const since = int64(12345)
	for name, f := range map[string]ReqFilter{
		"puts":        putsFilter(since),
		"buys":        buysFilter(since),
		"matches":     matchesFilter(since),
		"settlements": settlementsFilter(since),
		"put-accepts": putAcceptsFilter(since),
		"put-rejects": putRejectsFilter(since),
		"consumes":    consumesFilter(since),
		"buy-miss":    buyMissFilter(since),
	} {
		if f.Since != since {
			t.Errorf("%s: Since = %d, want %d", name, f.Since, since)
		}
	}
}

// --------------------------------------------------------------------------
// readFilter — since/until/limit/tag behavior against an in-memory message
// slice (dontguess-b13: readFilter no longer talks to a campfire client, it
// filters the []exchange.Message replayed from the local DG_HOME store — see
// loadLocalMessages). Tests build that slice directly instead of standing up
// a real campfire.
// --------------------------------------------------------------------------

// msg is a small exchange.Message builder for these tests.
func msg(id string, tags []string, ts int64) exchange.Message {
	return exchange.Message{
		ID:        id,
		Sender:    "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		Payload:   []byte(`{}`),
		Tags:      tags,
		Timestamp: ts,
	}
}

func TestReadFilter_SinceUntilLimit(t *testing.T) {
	t.Parallel()

	// 5 puts at distinct, known timestamps 100ns apart.
	base := int64(1_000_000)
	var timestamps []int64
	var msgs []exchange.Message
	for i := 0; i < 5; i++ {
		ts := base + int64(i)*100
		timestamps = append(timestamps, ts)
		msgs = append(msgs, msg(fmt.Sprintf("test-put-%d", i), []string{exchange.TagPut}, ts))
	}

	t.Run("no window returns all", func(t *testing.T) {
		got := readFilter(msgs, putsFilter(0))
		if len(got) != 5 {
			t.Errorf("len = %d, want 5", len(got))
		}
	})

	t.Run("since excludes earlier messages", func(t *testing.T) {
		f := putsFilter(timestamps[2])
		got := readFilter(msgs, f)
		if len(got) != 3 { // indices 2,3,4
			t.Errorf("len = %d, want 3", len(got))
		}
	})

	t.Run("until excludes later messages", func(t *testing.T) {
		f := putsFilter(0)
		f.Until = timestamps[2] // exclusive upper bound
		got := readFilter(msgs, f)
		if len(got) != 2 { // indices 0,1
			t.Errorf("len = %d, want 2", len(got))
		}
	})

	t.Run("since and until combine to a window", func(t *testing.T) {
		f := putsFilter(timestamps[1])
		f.Until = timestamps[4] // [1,4) -> indices 1,2,3
		got := readFilter(msgs, f)
		if len(got) != 3 {
			t.Errorf("len = %d, want 3", len(got))
		}
	})

	t.Run("limit caps the result", func(t *testing.T) {
		f := putsFilter(0)
		f.Limit = 2
		got := readFilter(msgs, f)
		if len(got) != 2 {
			t.Errorf("len = %d, want 2", len(got))
		}
	})
}

// TestReadFilter_KindOnlyMatchesThatOp confirms readFilter's Kinds->tag
// translation is exclusive: a buysFilter never returns a put message even
// when both exist in the same message slice.
func TestReadFilter_KindOnlyMatchesThatOp(t *testing.T) {
	t.Parallel()

	msgs := []exchange.Message{
		msg("put-1", []string{exchange.TagPut}, 1),
		msg("buy-1", []string{exchange.TagBuy}, 2),
	}

	got := readFilter(msgs, buysFilter(0))
	if len(got) != 1 || got[0].ID != "buy-1" {
		t.Errorf("buysFilter returned %+v, want exactly [buy-1]", got)
	}
}

// --------------------------------------------------------------------------
// buyMissFilter / consumesFilter parity — assert against the ACTUAL tags
// pkg/exchange stamps (cross-checked against engine_put.go's emitPutAccept
// and engine_buy.go's buy-miss standing offer emission), not just the
// abstract legacyTags() shape. dontguess-40e.
// --------------------------------------------------------------------------

// TestReadFilter_BuyMissExcludesPutAcceptFulfillment reproduces the exact tag
// collision described in buyMissFilter's doc: emitPutAccept
// (pkg/exchange/engine_put.go) stamps a buy-miss fulfillment's
// settle(put-accept) message with TagBuyMiss *alongside* TagSettle +
// phase:put-accept, to link the fulfillment back to the offer it filled. A
// bare exchange:buy-miss tag query (no ExcludeTags) would return both the
// still-open standing offer and every fulfillment message for offers that
// have already been filled — corrupting demand.BuildBacklog, which parses
// each hit as a BuyMissPayload with a "task" field the fulfillment payload
// doesn't have. buyMissFilter's ExcludeTags{TagSettle} must keep the
// fulfillment message out.
func TestReadFilter_BuyMissExcludesPutAcceptFulfillment(t *testing.T) {
	t.Parallel()

	msgs := []exchange.Message{
		// The still-open standing offer — exact tag shape from
		// pkg/exchange/engine_buy.go's buy-miss emission: [TagBuyMiss, TagMatch].
		msg("buy-miss-standing", []string{exchange.TagBuyMiss, exchange.TagMatch}, 1),
		// The fulfillment — exact tag shape from emitPutAccept
		// (pkg/exchange/engine_put.go): [TagSettle, phase:put-accept,
		// verdict:accepted, TagBuyMiss].
		msg("buy-miss-fulfilled", []string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrPutAccept,
			exchange.TagVerdictPrefix + "accepted",
			exchange.TagBuyMiss,
		}, 2),
	}

	got := readFilter(msgs, buyMissFilter(0))
	if len(got) != 1 || got[0].ID != "buy-miss-standing" {
		t.Errorf("buyMissFilter returned %+v, want exactly [buy-miss-standing]", got)
	}
}

// TestReadFilter_ConsumesFilterMatchesActualTag confirms consumesFilter's
// legacyTags() shape (exchange:consume) matches what the exchange actually
// emits and that unrelated kinds (e.g. a plain match) are not swept in by
// the "x" passthrough query.
func TestReadFilter_ConsumesFilterMatchesActualTag(t *testing.T) {
	t.Parallel()

	msgs := []exchange.Message{
		msg("consume-1", []string{exchange.TagConsume}, 1),
		msg("match-1", []string{exchange.TagMatch}, 2),
	}

	got := readFilter(msgs, consumesFilter(0))
	if len(got) != 1 || got[0].ID != "consume-1" {
		t.Errorf("consumesFilter returned %+v, want exactly [consume-1]", got)
	}
}

// --------------------------------------------------------------------------
// loadLocalMessages — reads the local DG_HOME event log (dontguess-b13:
// replaces the campfire protocol.Client the observability commands used to
// read from).
// --------------------------------------------------------------------------

// TestLoadLocalMessages_EmptyDGHome verifies loadLocalMessages creates (but
// does not error on) a missing events.jsonl and returns an empty slice —
// the state a fresh DG_HOME is in before `dontguess serve` has ever run.
func TestLoadLocalMessages_EmptyDGHome(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	got, err := loadLocalMessages(dir)
	if err != nil {
		t.Fatalf("loadLocalMessages: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("len = %d, want 0 (fresh DG_HOME)", len(got))
	}
}
