package main

import (
	"github.com/campfire-net/campfire/cf-protocol/protocol"
	"github.com/campfire-net/dontguess/pkg/exchange"
	"github.com/campfire-net/dontguess/pkg/nostr"
)

// ReqFilter is a NIP-01-shaped exchange query: kinds, tag filters, and a
// since/until/limit window — the same four dimensions a nostr relay's REQ
// message supports. It replaces pkg/exchange.StandardViews()'s 12 campfire-side
// named views (deleted, dontguess-7fc): each view's tag predicate becomes one
// ReqFilter value below, constructed by the *Filter helper functions in
// hitrate.go, status.go, and demand.go.
//
// The exchange still runs over the campfire transport (no relay yet — see
// docs/design/nostr-first-rebuild-decision.md §Sequencer, dontguess-50d), so
// readFilter translates Kinds+Tags into the campfire client's tag-based
// protocol.ReadRequest. Since/Until/Limit are applied client-side because
// campfire's ReadRequest exposes only a single AfterTimestamp lower bound and
// no upper bound or kind filter — once a real relay lands, readFilter is the
// only function that needs to change.
type ReqFilter struct {
	// Kinds selects exchange operation kinds (pkg/nostr Kind* constants).
	// A message matches if its kind is one of these. Nil/empty means no kind
	// restriction.
	Kinds []int

	// Tags holds NIP-01 "#<tagname>" filters. Two keys are meaningful here:
	//   "phase" — matched against exchange:phase:<value> tags (settle/put
	//             sub-phase discriminators; see pkg/nostr/adapter.go's
	//             "exchange:phase:X -> [\"phase\", X]" mapping).
	//   "x"     — matched literally against the full legacy exchange tag
	//             string (the adapter's lossless passthrough namespace for
	//             tags that don't own a kind, e.g. exchange:buy-miss,
	//             exchange:consume; see adapter.go's
	//             "any other exchange tag -> [\"x\", <full-tag>]").
	// Multiple values under a key, and values across keys, are ORed together
	// with Kinds — matching how the deleted views' single/OR-tag predicates
	// worked. None of the three CLI commands need AND-of-tag-families.
	Tags map[string][]string

	// Since is an inclusive lower bound on message timestamp (nanoseconds).
	// 0 means no lower bound.
	Since int64

	// Until is an exclusive upper bound on message timestamp (nanoseconds).
	// 0 means no upper bound.
	Until int64

	// Limit caps the number of messages returned, applied after Since/Until
	// filtering. 0 means no limit.
	Limit int
}

// kindToOpTag maps the four base exchange kinds (each owns a dedicated kind,
// per pkg/nostr/kinds.go's baseOpToKind) to the legacy campfire tag used to
// query for them. Assign/scrip messages share a kind with several sub-ops and
// are not queried by hitrate/status/demand, so they're intentionally absent —
// callers needing them should filter by Tags["x"] with the specific sub-op tag.
var kindToOpTag = map[int]string{
	nostr.KindPut:    exchange.TagPut,
	nostr.KindBuy:    exchange.TagBuy,
	nostr.KindMatch:  exchange.TagMatch,
	nostr.KindSettle: exchange.TagSettle,
}

// legacyTags renders f's Kinds+Tags into the set of legacy campfire tags to
// OR together in a protocol.ReadRequest.Tags query. A nil/empty result means
// "no tag filter" (protocol.ReadRequest treats that as "match everything").
func (f ReqFilter) legacyTags() []string {
	var tags []string
	for _, k := range f.Kinds {
		if t, ok := kindToOpTag[k]; ok {
			tags = append(tags, t)
		}
	}
	for _, p := range f.Tags["phase"] {
		tags = append(tags, exchange.TagPhasePrefix+p)
	}
	tags = append(tags, f.Tags["x"]...)
	return tags
}

// The functions below are the ReqFilter equivalent of each deleted
// pkg/exchange.StandardViews() entry that hitrate.go, status.go, and demand.go
// actually query (put-accept/put-reject share kind 3401 with plain puts, so
// they're distinguished by the "phase" tag rather than Kinds; buy-miss and
// consume don't own a kind at all, so they're distinguished by the "x"
// passthrough tag — see the ReqFilter.Tags doc above).

// putsFilter is the "puts" view: all exchange:put messages.
func putsFilter(since int64) ReqFilter {
	return ReqFilter{Kinds: []int{nostr.KindPut}, Since: since}
}

// buysFilter is the "buys" view: all exchange:buy messages.
func buysFilter(since int64) ReqFilter {
	return ReqFilter{Kinds: []int{nostr.KindBuy}, Since: since}
}

// matchesFilter is the "match-results" view: all exchange:match messages
// (both hits and buy-miss standing offers carry this kind).
func matchesFilter(since int64) ReqFilter {
	return ReqFilter{Kinds: []int{nostr.KindMatch}, Since: since}
}

// settlementsFilter is the "settlements" view: all exchange:settle messages.
func settlementsFilter(since int64) ReqFilter {
	return ReqFilter{Kinds: []int{nostr.KindSettle}, Since: since}
}

// putAcceptsFilter is the "put-accepts" view: exchange:phase:put-accept
// messages (a put-accept is a phase of the put op, so it carries kind 3401
// plus the phase tag; matching on the phase tag alone reproduces the
// original single-tag-predicate view).
func putAcceptsFilter(since int64) ReqFilter {
	return ReqFilter{Tags: map[string][]string{"phase": {exchange.SettlePhaseStrPutAccept}}, Since: since}
}

// putRejectsFilter mirrors putAcceptsFilter for put-reject. The deleted
// views.go had no dedicated "put-rejects" view (status.go queried the raw
// tag directly); it's included here for symmetry now that both live in one
// filter table.
func putRejectsFilter(since int64) ReqFilter {
	return ReqFilter{Tags: map[string][]string{"phase": {exchange.SettlePhaseStrPutReject}}, Since: since}
}

// consumesFilter reads exchange:consume messages (emitted on settle-complete;
// no dedicated view existed — status/hitrate read the raw tag directly).
func consumesFilter(since int64) ReqFilter {
	return ReqFilter{Tags: map[string][]string{"x": {exchange.TagConsume}}, Since: since}
}

// buyMissFilter is the demand command's read of exchange:buy-miss standing
// offers (no dedicated view existed for this either).
func buyMissFilter(since int64) ReqFilter {
	return ReqFilter{Tags: map[string][]string{"x": {exchange.TagBuyMiss}}, Since: since}
}

// readFilter executes f against the exchange campfire identified by cfID and
// returns matching messages as exchange.Message, applying Since/Until/Limit
// client-side (see ReqFilter doc).
func readFilter(client *protocol.Client, cfID string, f ReqFilter) ([]exchange.Message, error) {
	result, err := client.Read(protocol.ReadRequest{
		CampfireID: cfID,
		Tags:       f.legacyTags(),
	})
	if err != nil {
		return nil, err
	}
	out := make([]exchange.Message, 0, len(result.Messages))
	for i := range result.Messages {
		m := result.Messages[i]
		if f.Since > 0 && m.Timestamp < f.Since {
			continue
		}
		if f.Until > 0 && m.Timestamp >= f.Until {
			continue
		}
		out = append(out, *exchange.FromSDKMessage(&m))
		if f.Limit > 0 && len(out) >= f.Limit {
			break
		}
	}
	return out, nil
}
