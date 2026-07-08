package main

import (
	"fmt"
	"reflect"
	"sort"
	"testing"

	"github.com/campfire-net/campfire/cf-protocol/protocol"
	"github.com/campfire-net/campfire/cf-protocol/store"
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
// readFilter — since/until/limit behavior against a real campfire store.
// --------------------------------------------------------------------------

func TestReadFilter_SinceUntilLimit(t *testing.T) {
	t.Parallel()

	cfHome := t.TempDir()
	transportDir := t.TempDir()
	convDir := conventionDirForOpTest(t)

	cfg, initClient, err := exchange.Init(exchange.InitOptions{
		ConfigDir:         cfHome,
		Transport:         protocol.FilesystemTransport{Dir: transportDir},
		BeaconDir:         t.TempDir(),
		ConventionDir:     convDir,
		SkipConfigCascade: true,
	})
	if err != nil {
		t.Fatalf("exchange.Init: %v", err)
	}
	t.Cleanup(func() { initClient.Close() })

	st, err := store.Open(store.StorePath(cfHome))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	cfID := cfg.ExchangeCampfireID

	// Insert 5 puts at distinct, known timestamps 100ns apart.
	base := store.NowNano()
	var timestamps []int64
	for i := 0; i < 5; i++ {
		ts := base + int64(i)*100
		timestamps = append(timestamps, ts)
		rec := store.MessageRecord{
			ID:          fmt.Sprintf("test-put-%d", i),
			CampfireID:  cfID,
			Sender:      "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
			Payload:     []byte(`{}`),
			Tags:        []string{exchange.TagPut},
			Antecedents: []string{},
			Timestamp:   ts,
			Signature:   []byte{},
		}
		if _, err := st.AddMessage(rec); err != nil {
			t.Fatalf("AddMessage %d: %v", i, err)
		}
	}

	readClient := protocol.New(st, nil)

	t.Run("no window returns all", func(t *testing.T) {
		got, err := readFilter(readClient, cfID, putsFilter(0))
		if err != nil {
			t.Fatalf("readFilter: %v", err)
		}
		if len(got) != 5 {
			t.Errorf("len = %d, want 5", len(got))
		}
	})

	t.Run("since excludes earlier messages", func(t *testing.T) {
		f := putsFilter(timestamps[2])
		got, err := readFilter(readClient, cfID, f)
		if err != nil {
			t.Fatalf("readFilter: %v", err)
		}
		if len(got) != 3 { // indices 2,3,4
			t.Errorf("len = %d, want 3", len(got))
		}
	})

	t.Run("until excludes later messages", func(t *testing.T) {
		f := putsFilter(0)
		f.Until = timestamps[2] // exclusive upper bound
		got, err := readFilter(readClient, cfID, f)
		if err != nil {
			t.Fatalf("readFilter: %v", err)
		}
		if len(got) != 2 { // indices 0,1
			t.Errorf("len = %d, want 2", len(got))
		}
	})

	t.Run("since and until combine to a window", func(t *testing.T) {
		f := putsFilter(timestamps[1])
		f.Until = timestamps[4] // [1,4) -> indices 1,2,3
		got, err := readFilter(readClient, cfID, f)
		if err != nil {
			t.Fatalf("readFilter: %v", err)
		}
		if len(got) != 3 {
			t.Errorf("len = %d, want 3", len(got))
		}
	})

	t.Run("limit caps the result", func(t *testing.T) {
		f := putsFilter(0)
		f.Limit = 2
		got, err := readFilter(readClient, cfID, f)
		if err != nil {
			t.Fatalf("readFilter: %v", err)
		}
		if len(got) != 2 {
			t.Errorf("len = %d, want 2", len(got))
		}
	})
}

// TestReadFilter_KindOnlyMatchesThatOp confirms readFilter's Kinds->tag
// translation is exclusive: a buysFilter never returns a put message even
// when both exist in the same campfire.
func TestReadFilter_KindOnlyMatchesThatOp(t *testing.T) {
	t.Parallel()

	cfHome := t.TempDir()
	transportDir := t.TempDir()
	convDir := conventionDirForOpTest(t)

	cfg, initClient, err := exchange.Init(exchange.InitOptions{
		ConfigDir:         cfHome,
		Transport:         protocol.FilesystemTransport{Dir: transportDir},
		BeaconDir:         t.TempDir(),
		ConventionDir:     convDir,
		SkipConfigCascade: true,
	})
	if err != nil {
		t.Fatalf("exchange.Init: %v", err)
	}
	t.Cleanup(func() { initClient.Close() })

	st, err := store.Open(store.StorePath(cfHome))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	cfID := cfg.ExchangeCampfireID

	insert := func(id string, tags []string) {
		t.Helper()
		rec := store.MessageRecord{
			ID:          id,
			CampfireID:  cfID,
			Sender:      "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
			Payload:     []byte(`{}`),
			Tags:        tags,
			Antecedents: []string{},
			Timestamp:   store.NowNano(),
			Signature:   []byte{},
		}
		if _, err := st.AddMessage(rec); err != nil {
			t.Fatalf("AddMessage %s: %v", id, err)
		}
	}
	insert("put-1", []string{exchange.TagPut})
	insert("buy-1", []string{exchange.TagBuy})

	readClient := protocol.New(st, nil)

	got, err := readFilter(readClient, cfID, buysFilter(0))
	if err != nil {
		t.Fatalf("readFilter: %v", err)
	}
	if len(got) != 1 || got[0].ID != "buy-1" {
		t.Errorf("buysFilter returned %+v, want exactly [buy-1]", got)
	}
}
