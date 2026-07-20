package relayclient

// buy.go is item ed2-B: the team-tier `dontguess buy` await protocol. It is the
// buyer counterpart to buy.go's put publish primitive (ed2-A) and reuses the
// same sign(agentKey) -> submit(relay) chain, but the hard part is the AWAIT:
//
//   SUBSCRIBE FIRST  ->  PUBLISH buy(3402)  ->  await(re-subscribing, bounded)
//
// Design authority: docs/design/nostr-first-client-ed2.md §3.2 (DQ2 — the buy
// await protocol), §2 (verified facts), §5 (failure-mode matrix + §5.4 the
// AMBIGUOUS-timeout rule).
//
// SCOPE BOUNDARY (ed2-B vs ed2-C): this file publishes the buy, subscribes
// before publishing (to beat the operator's 500ms fold poll + Notify-driven
// publish — H1), re-subscribes on a mid-await conn drop (H5), and DISCRIMINATES
// the outcome (real match / buy-miss / leaked-assign / ambiguous-timeout). It
// does NOT drive the buyer-accept -> deliver -> complete settle chain that moves
// scrip and pulls full content — that is ed2-C. On a real match this SURFACES
// the parsed match (BuyResult.Match, incl. the 3403 message id ed2-C e-tags its
// buyer-accept against) and returns; ed2-C extends BuyResult into settlement.
//
// LOUD-EVERYWHERE (relay-transport.md §0, ed2 §5): a forged/unsigned response is
// a loud-skip (never trusted as a match), a malformed frame is skipped and never
// panics, a dead/stalled relay fails inside the caller's bounded ctx (the same
// watchdogDialer NewConn installs for put), and a timeout is reported as
// AMBIGUOUS with enumerated actionable causes — NEVER "no cache exists" (§5.4).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/3dl-dev/dontguess/pkg/exchange"
	"github.com/3dl-dev/dontguess/pkg/identity"
	"github.com/3dl-dev/dontguess/pkg/nostr"
	"github.com/3dl-dev/dontguess/pkg/proto"
	"github.com/3dl-dev/dontguess/pkg/relay"
)

// DefaultBuyTimeout is the default end-to-end bound for a Buy call (dial +
// subscribe + publish + await). Design §3.2 raises it to 10s (from the draft's
// ~5s) so the floor `pollInterval(500ms) + outboxPublishInterval + relayRTT +
// publishAwaitOK` fits with headroom even before ed2-N's Notify wiring collapses
// the outbox term. The whole bound lives in the caller's ctx; the operator is
// never serialized on a buy.
const DefaultBuyTimeout = 10 * time.Second

// resubscribeSlackSeconds is subtracted from the buy's created_at when a
// re-subscribe (after a mid-await drop) adds a `since` term, so strfry replays
// the match that may have been published during the reconnect gap. Since is a
// coarse fetch hint (relay-transport.md §B, ADV-9) — a few seconds of slack
// absorbs clock skew between the client and the relay without risk.
const resubscribeSlackSeconds = 5

// BuyOutcome is the discriminated result class of a Buy await (§3.2 / §5).
type BuyOutcome int

const (
	// BuyOutcomeMatch: a kind-3403 match WITHOUT the exchange:buy-miss tag — a
	// real hit. BuyResult.Match / .Matches are populated; ed2-C proceeds into
	// settle from here.
	BuyOutcomeMatch BuyOutcome = iota
	// BuyOutcomeMiss: a kind-3403 response WITH the exchange:buy-miss tag — a
	// genuine miss. The demand-signal guide is the actionable result, not an
	// error.
	BuyOutcomeMiss
	// BuyOutcomeBrokered: an assign(3405) e-tagging the buy — BrokeredMatchMode
	// leaked (out of scope, gate G1). Surfaced LOUD so the caller does not hang;
	// ed2 assumes brokered mode disabled.
	BuyOutcomeBrokered
	// BuyOutcomeAmbiguous: the bounded ctx expired with no discriminating
	// response. Per §5.4 this maps to AT LEAST {genuine no-match,
	// underfunded-self, seller-never-admitted}; the client MUST enumerate the
	// actionable causes and MUST NOT claim "no cache exists".
	BuyOutcomeAmbiguous
)

func (o BuyOutcome) String() string {
	switch o {
	case BuyOutcomeMatch:
		return "match"
	case BuyOutcomeMiss:
		return "buy-miss"
	case BuyOutcomeBrokered:
		return "brokered-assign-leaked"
	case BuyOutcomeAmbiguous:
		return "ambiguous-timeout"
	default:
		return "unknown"
	}
}

// BuyRequest is the caller-supplied buy. Mirrors the fields applyBuy
// (pkg/exchange/state_buy.go) reads from the buy payload; only Task and Budget
// are load-bearing for a minimal buy.
type BuyRequest struct {
	Task           string
	Budget         int64
	ContentType    string // full exchange content-type tag, e.g. "exchange:content-type:code" (optional)
	Domains        []string
	MinReputation  int
	FreshnessHours int
	MaxResults     int

	// OperatorPubKey, when non-empty (32-byte hex), restricts accepted
	// match/miss/assign responses to those AUTHORED by the operator — belt and
	// suspenders on top of the relay's signed-author write allowlist and the
	// per-event signature check. Empty = accept any signature-valid response
	// (the relay allowlist is then the sole author gate). ed2-C hardens the
	// same-key binding across the settle chain; ed2-B only needs to not act on
	// an obviously-foreign response.
	OperatorPubKey string
}

// MatchEntry is one ranked result surfaced from a real match (the seam ed2-C
// extends into settle). Field set mirrors the operator's emitMatchResponse
// MatchResult (pkg/exchange/engine_buy.go); only the fields a buyer acts on are
// carried.
type MatchEntry struct {
	EntryID           string  `json:"entry_id"`
	PutMsgID          string  `json:"put_msg_id"`
	SellerKey         string  `json:"seller_key"`
	Description       string  `json:"description"`
	ContentType       string  `json:"content_type"`
	Price             int64   `json:"price"`
	Confidence        float64 `json:"confidence"`
	SellerReputation  int     `json:"seller_reputation"`
	TokenCostOriginal int64   `json:"token_cost_original"`
}

// BuyResult is the discriminated outcome of a Buy await. Exactly one of the
// outcome-specific field groups is populated, keyed by Outcome.
type BuyResult struct {
	BuyID   string
	Outcome BuyOutcome

	// BuyerPubKey is the hex npub that SIGNED the buy — recorded so ed2-C's Settle
	// can enforce the SAME-KEY invariant (§3.5): every buyer-side settle phase must
	// be signed by the exact key that signed the buy, or the engine silently drops
	// it. Set by Buy from the signer; empty only on a caller-constructed result.
	BuyerPubKey string

	// MatchMsgID is the 3403 match event id — set on BuyOutcomeMatch. This is
	// the CLEAN SEAM for ed2-C: the buyer-accept settle message e-tags this id.
	MatchMsgID string
	// Matches are the ranked results surfaced from a real match (BuyOutcomeMatch).
	// The first is the default selection (mirrors applyMatch matchToEntry[0]).
	Matches []MatchEntry
	// Guide is the operator's inline guide text (match guide or buy-miss guide),
	// surfaced verbatim for the buyer.
	Guide string

	// TaskHash / OfferedPriceRate are populated on BuyOutcomeMiss (the standing
	// buy-miss offer the operator recorded — compute the result and `dontguess
	// put` to claim it).
	TaskHash         string
	OfferedPriceRate int

	// AmbiguousCauses enumerates the actionable causes of a timeout
	// (BuyOutcomeAmbiguous), per §5.4. Never empty on that outcome.
	AmbiguousCauses []string
}

// Match returns the default (top-ranked) match entry on a hit, or nil.
func (r *BuyResult) Match() *MatchEntry {
	if r == nil || r.Outcome != BuyOutcomeMatch || len(r.Matches) == 0 {
		return nil
	}
	return &r.Matches[0]
}

// Buy signs req as an exchange:buy(3402) event with signer (the AGENT key —
// never the operator key), SUBSCRIBES FIRST for the response, publishes the buy,
// and awaits a discriminating response on a bounded, re-subscribing ctx.
//
// Contract:
//   - A terminal outcome (match / buy-miss / leaked-assign / ambiguous-timeout)
//     returns (result, nil) — the caller renders it and decides its exit code.
//   - A TRANSPORT failure (dead/unreachable relay, the relay refusing the buy
//     write with OK-accepted=false, or a reconnect that cannot re-subscribe
//     within budget) returns (nil, err) — LOUD, never a silent ambiguous.
//
// Subscribe-before-publish is MANDATORY (§3.2): the buy event id is the
// deterministic signed-event id, computable before publish, so the #e filter is
// ready and the subscription is live before the operator can fold+publish the
// match (H1). The bound lives entirely in ctx.
func Buy(ctx context.Context, conn *relay.Conn, signer identity.Signer, req BuyRequest) (*BuyResult, error) {
	if conn == nil {
		return nil, fmt.Errorf("relayclient: buy: nil conn")
	}
	if signer == nil {
		return nil, fmt.Errorf("relayclient: buy: nil signer")
	}
	msg, err := buildBuyMessage(signer, req)
	if err != nil {
		return nil, fmt.Errorf("relayclient: buy: %w", err)
	}
	ev, err := signAsIdentityEvent(signer, msg)
	if err != nil {
		return nil, fmt.Errorf("relayclient: buy: sign event: %w", err)
	}
	buyID := ev.ID
	buyCreatedAt := ev.CreatedAt

	// The subscription the client will (in ed2-C) extend per settle phase. For
	// ed2-B it covers the buy response: match(3403), settle(3404) — reserved for
	// ed2-C's per-phase extension — and assign(3405) so a leaked BrokeredMatchMode
	// assign is DISCRIMINATED loudly rather than silently timing out. All three
	// e-tag the buy id, so one #e:[buyID] filter receives them. (The design's
	// §3.2 base filter lists [3403,3404]; ed2-B unions in 3405 so failure-matrix
	// case (c) — leaked assign — is observable in the SAME subscription instead
	// of manifesting as a bare timeout.)
	subID := "dg-buy-" + shortID(buyID)
	baseFilter := relay.Filter{
		Kinds: []int{nostr.KindMatch, nostr.KindSettle, nostr.KindAssign},
		Tags:  map[string][]string{"e": {buyID}},
	}

	// 1. SUBSCRIBE FIRST — before the buy is on the wire. conn.Send synchronously
	//    writes the REQ (dialing + handshaking under NewConn's watchdog on this
	//    first call), so by the time it returns the subscription is live.
	if err := sendReq(ctx, conn, subID, baseFilter); err != nil {
		return nil, fmt.Errorf("relayclient: buy %s: subscribe-first REQ: %w", shortID(buyID), err)
	}
	// Pin the reader generation to the just-subscribed connection so that if the
	// publish below drops+reconnects the socket (Send replays only its EVENT, not
	// this REQ), the FIRST Recv detects the generation advance and re-subscribes
	// instead of blocking forever on a REQ-less socket (dontguess-989).
	conn.MarkReaderGen()

	// 2. PUBLISH the buy. A relay OK is a transport receipt only (§3.1); the
	//    answer is the operator's match, awaited below.
	frame, err := relay.EncodeEvent(ev)
	if err != nil {
		return nil, fmt.Errorf("relayclient: buy %s: encode EVENT: %w", shortID(buyID), err)
	}
	if err := conn.Send(ctx, frame); err != nil {
		return nil, fmt.Errorf("relayclient: buy %s: publish EVENT: %w", shortID(buyID), err)
	}

	// 3. AWAIT — bounded, re-subscribing on drop.
	res, err := awaitBuyResponse(ctx, conn, subID, baseFilter, buyID, buyCreatedAt, req.OperatorPubKey)
	if res != nil {
		// Record the signing key so ed2-C's Settle can enforce the SAME-KEY
		// invariant across the whole buy->settle chain (§3.5).
		res.BuyerPubKey = signer.PubKeyHex()
	}
	return res, err
}

// awaitBuyResponse runs the bounded Recv loop that discriminates the buy
// response, re-subscribing on any ErrConnDropped (§3.2, H5) before the next Recv
// within the remaining budget. It mirrors the operator's own
// resubscribe-before-read discipline (cmd/dontguess/serve_relay.go
// runReaderReconnect): relay.Conn transparently re-dials but NEVER replays the
// REQ, so the client must re-issue it — a fresh #e:[buyID] REQ (now with a
// `since` covering the buy) recovers a match strfry stored during the gap.
func awaitBuyResponse(
	ctx context.Context,
	conn *relay.Conn,
	subID string,
	baseFilter relay.Filter,
	buyID string,
	buyCreatedAt int64,
	operatorPubKey string,
) (*BuyResult, error) {
	for {
		raw, recvErr := conn.Recv(ctx)
		if recvErr != nil {
			// ctx expiry is the terminal AMBIGUOUS outcome, NOT a transport error.
			if ctx.Err() != nil {
				return ambiguousResult(buyID), nil
			}
			if errors.Is(recvErr, relay.ErrConnDropped) {
				// Re-subscribe BEFORE the next Recv, within remaining budget. The
				// re-issued REQ adds a `since` covering the buy so strfry replays a
				// match published during the reconnect gap.
				resub := baseFilter
				since := buyCreatedAt - resubscribeSlackSeconds
				resub.Since = &since
				if err := sendReq(ctx, conn, subID, resub); err != nil {
					if ctx.Err() != nil {
						return ambiguousResult(buyID), nil
					}
					return nil, fmt.Errorf("relayclient: buy %s: re-subscribe after conn drop: %w", shortID(buyID), err)
				}
				// Re-pin the reader generation to the connection the re-subscribe
				// REQ landed on, so the next Recv reads it as the claimed generation
				// (and a further reconnect is detected again) — dontguess-989.
				conn.MarkReaderGen()
				continue
			}
			return nil, fmt.Errorf("relayclient: buy %s: await response: %w", shortID(buyID), recvErr)
		}

		f, perr := relay.ParseFrame(raw)
		if perr != nil {
			// Malformed frame from a hostile/buggy relay: loud-skip, never panic,
			// never treat garbage as an answer (LOCKED-5).
			continue
		}
		switch f.Type {
		case relay.LabelOK:
			// The buy's own OK is a transport receipt. accepted=false means the
			// relay REFUSED the write (e.g. the buyer's npub is not a permitted
			// author) — the buy never reached the operator, so fail LOUD rather
			// than wait out the timeout as a false ambiguous.
			if f.EventID == buyID && !f.Accepted {
				return nil, fmt.Errorf("relayclient: buy %s: relay refused the buy event: %s", shortID(buyID), f.Message)
			}
		case relay.LabelEVENT:
			if res, terminal := classifyResponse(f.Event, buyID, operatorPubKey); terminal {
				res.BuyID = buyID
				return res, nil
			}
		}
	}
}

// classifyResponse discriminates a received EVENT against the four ruled buy
// outcomes. terminal=false means "not a response we act on, keep reading"
// (unsigned/forged, wrong author, wrong e-tag, or a 3404 settle reserved for
// ed2-C). It NEVER trusts an event whose signature does not verify — a relay (or
// anyone who can write to it) could otherwise spoof a match/miss.
func classifyResponse(ev *identity.Event, buyID, operatorPubKey string) (*BuyResult, bool) {
	if ev == nil {
		return nil, false
	}
	// Never act on a forged or tampered event.
	if err := identity.VerifyEvent(ev); err != nil {
		return nil, false
	}
	// Optional operator-author gate (belt and suspenders on the relay allowlist).
	if operatorPubKey != "" && ev.PubKey != operatorPubKey {
		return nil, false
	}
	// Defensive: the relay #e filter should guarantee this, but confirm the event
	// actually replies to OUR buy before acting on it.
	if !eventHasETag(ev, buyID) {
		return nil, false
	}

	switch ev.Kind {
	case nostr.KindMatch: // 3403 — match OR buy-miss, discriminated by the tag.
		if eventHasXTag(ev, exchange.TagBuyMiss) {
			return parseBuyMiss(ev), true
		}
		return parseMatch(ev), true
	case nostr.KindAssign: // 3405 — leaked BrokeredMatchMode (out of scope, gate G1).
		return &BuyResult{Outcome: BuyOutcomeBrokered, MatchMsgID: ev.ID}, true
	case nostr.KindSettle: // 3404 — reserved for ed2-C's per-phase settle chain.
		return nil, false
	default:
		return nil, false
	}
}

// parseMatch surfaces the operator's match payload as a BuyResult. A match with
// an unparseable or empty results payload is still a real hit (kind 3403, no
// buy-miss tag) — surfaced with whatever parsed, so the caller never mistakes a
// malformed hit for a miss.
func parseMatch(ev *identity.Event) *BuyResult {
	var payload struct {
		Results []MatchEntry `json:"results"`
		Guide   string       `json:"guide"`
	}
	_ = json.Unmarshal([]byte(ev.Content), &payload)
	return &BuyResult{
		Outcome:    BuyOutcomeMatch,
		MatchMsgID: ev.ID,
		Matches:    payload.Results,
		Guide:      payload.Guide,
	}
}

// parseBuyMiss surfaces the operator's buy-miss standing-offer payload.
func parseBuyMiss(ev *identity.Event) *BuyResult {
	var payload struct {
		TaskHash         string `json:"task_hash"`
		OfferedPriceRate int    `json:"offered_price_rate"`
		Guide            string `json:"guide"`
	}
	_ = json.Unmarshal([]byte(ev.Content), &payload)
	return &BuyResult{
		Outcome:          BuyOutcomeMiss,
		TaskHash:         payload.TaskHash,
		OfferedPriceRate: payload.OfferedPriceRate,
		Guide:            payload.Guide,
	}
}

// ambiguousResult builds the §5.4 AMBIGUOUS outcome with the enumerated,
// actionable causes. It MUST NOT claim "no cache exists" — a buy timeout is
// wire-indistinguishable across at least these three causes, two of which are
// wire-invisible to the buyer.
func ambiguousResult(buyID string) *BuyResult {
	return &BuyResult{
		BuyID:   buyID,
		Outcome: BuyOutcomeAmbiguous,
		AmbiguousCauses: []string{
			"genuine no-match: no seller has cached this yet, and the operator has not (yet) turned your buy into a buy-miss offer within the window — try a broader task description or a larger --timeout",
			"under-funded buy: your budget may be below the best price, in which case the operator drops the buy silently — verify your scrip balance and ask the operator to run: dontguess mint <your-npub> <amount>",
			"seller never allowlisted: a matching seller may exist but their put was dropped at admission (the reject went to the seller, not to you) — ask the operator to allowlist the seller's npub",
		},
	}
}

// buildBuyMessage renders req into the proto.Message shape applyBuy
// (pkg/exchange/state_buy.go) expects: task/budget/content_type/domains/
// min_reputation/freshness_hours/max_results in the JSON payload, tagged
// exchange:buy.
func buildBuyMessage(signer identity.Signer, req BuyRequest) (*proto.Message, error) {
	if req.Task == "" {
		return nil, fmt.Errorf("empty task")
	}
	if req.Budget < 0 {
		return nil, fmt.Errorf("budget must be non-negative, got %d", req.Budget)
	}
	payload, err := json.Marshal(map[string]any{
		"task":            req.Task,
		"budget":          req.Budget,
		"content_type":    req.ContentType,
		"domains":         req.Domains,
		"min_reputation":  req.MinReputation,
		"freshness_hours": req.FreshnessHours,
		"max_results":     req.MaxResults,
	})
	if err != nil {
		return nil, fmt.Errorf("encode buy payload: %w", err)
	}
	return &proto.Message{
		Sender:    signer.PubKeyHex(),
		Payload:   payload,
		Tags:      []string{exchange.TagBuy},
		Timestamp: time.Now().UnixNano(),
	}, nil
}

// sendReq encodes and sends a REQ for subID+filter on conn, bounded by ctx.
func sendReq(ctx context.Context, conn *relay.Conn, subID string, filter relay.Filter) error {
	reqFrame, err := relay.EncodeReq(subID, filter)
	if err != nil {
		return fmt.Errorf("encode REQ: %w", err)
	}
	if err := conn.Send(ctx, reqFrame); err != nil {
		return fmt.Errorf("send REQ: %w", err)
	}
	return nil
}

// eventHasETag reports whether ev carries an ["e", id, ...] tag equal to id.
func eventHasETag(ev *identity.Event, id string) bool {
	for _, t := range ev.Tags {
		if len(t) >= 2 && t[0] == "e" && t[1] == id {
			return true
		}
	}
	return false
}

// eventHasXTag reports whether ev carries an ["x", value] passthrough tag equal
// to value (the adapter maps any non-base exchange tag to ["x", <full-tag>], so
// exchange:buy-miss on a 3403 arrives as ["x","exchange:buy-miss"]).
func eventHasXTag(ev *identity.Event, value string) bool {
	for _, t := range ev.Tags {
		if len(t) >= 2 && t[0] == "x" && t[1] == value {
			return true
		}
	}
	return false
}

// WriteOutcome renders a BuyResult as the LOUD, human-facing block the `buy`
// command prints. Kept in the package (not the cobra command) so the exact
// surfaced text — the demand-signal guide on a miss, the enumerated AMBIGUOUS
// causes on a timeout — is directly testable without cobra machinery.
func WriteOutcome(w io.Writer, r *BuyResult) {
	if r == nil {
		fmt.Fprintln(w, "buy: no result")
		return
	}
	switch r.Outcome {
	case BuyOutcomeMatch:
		m := r.Match()
		fmt.Fprintf(w, "buy %s: MATCH (match %s)\n", shortID(r.BuyID), shortID(r.MatchMsgID))
		if m != nil {
			fmt.Fprintf(w, "  entry %s  price %d scrip  seller %s (rep %d)\n",
				shortID(m.EntryID), m.Price, shortID(m.SellerKey), m.SellerReputation)
			if m.Description != "" {
				fmt.Fprintf(w, "  %s\n", m.Description)
			}
		}
		if len(r.Matches) > 1 {
			fmt.Fprintf(w, "  (+%d more ranked result(s))\n", len(r.Matches)-1)
		}
		fmt.Fprintln(w, "  -> settle to receive content (dontguess buy drives buyer-accept -> deliver on funded budget)")
	case BuyOutcomeMiss:
		fmt.Fprintf(w, "buy %s: BUY-MISS — nobody has this yet.\n", shortID(r.BuyID))
		fmt.Fprintln(w, "  This is a demand signal, not an error: compute the result and `dontguess put` it to earn the residual.")
		if r.OfferedPriceRate > 0 {
			fmt.Fprintf(w, "  A standing offer exists: the exchange will buy your put at %d%% of its token_cost.\n", r.OfferedPriceRate)
		}
		if r.Guide != "" {
			fmt.Fprintf(w, "  operator: %s\n", r.Guide)
		}
	case BuyOutcomeBrokered:
		fmt.Fprintf(w, "buy %s: UNEXPECTED brokered-match assign (3405) received — BrokeredMatchMode is out of scope for this client (gate G1).\n", shortID(r.BuyID))
		fmt.Fprintln(w, "  Not settling. Ask the operator whether brokered matching was enabled in error.")
	case BuyOutcomeAmbiguous:
		fmt.Fprintf(w, "buy %s: AMBIGUOUS — no response within the timeout. This does NOT mean no cache exists. Possible causes:\n", shortID(r.BuyID))
		for _, c := range r.AmbiguousCauses {
			fmt.Fprintf(w, "  - %s\n", c)
		}
	default:
		fmt.Fprintf(w, "buy %s: unknown outcome\n", shortID(r.BuyID))
	}
}
