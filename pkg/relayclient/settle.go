package relayclient

// settle.go is item ed2-C: the team-tier CLIENT settle chain. On a real match
// (ed2-B BuyResult) it drives the per-phase settle chain over ONE relay.Conn, in
// ONE CLI invocation under ONE identity, to move scrip and receive content:
//
//	buy(3402) -> match(3403) -> [optional preview-request -> preview (free)]
//	          -> buyer-accept (reserves scrip) -> deliver (content) -> complete
//
// Design authority: docs/design/nostr-first-client-ed2.md §3.5 (DQ5 — settle leg,
// per-phase subscription mandatory) + docs/design/settle-wire-id-reconciliation-55c.md
// (§Client remainder — the client ONLY e-tags the WIRE ids it received over the
// relay; the OPERATOR resolves wire->store via its alias, so the client never
// invents/references a store id).
//
// THE HARD PART — PER-PHASE SUBSCRIPTION (H3). The settle chain is a CHAIN, not a
// star rooted at buyID: settle(deliver) e-tags the buyer-accept id and the §3.6
// settle(buyer-accept-reject) also e-tags the buyer-accept id (never buyID). A
// single #e:[buyID] filter is STRUCTURALLY INCAPABLE of receiving either. So after
// building each buyer-side phase message the client subscribes #e:[<that phase's
// WIRE id>] BEFORE publishing it (the id is the deterministic signed-event id, so
// the sub is live before the operator can auto-deliver — the same subscribe-first
// discipline ed2-B uses to beat the operator's fold poll). #p filters are useless
// here: the adapter sets the p-tag to the message AUTHOR (the operator on every
// response), so a buyer p-filter never matches an operator response
// (pkg/nostr/adapter.go). The OPERATOR auto-delivers on a funded buyer-accept
// (dontguess-55c AutoDeliverOnBuyerAccept) — the client never requests deliver; it
// AWAITS it.
//
// SAME-KEY invariant (§3.5). Every buyer-side phase MUST be signed by the exact
// npub that signed the originating buy; the engine re-derives expectedBuyer and
// SILENTLY drops a mismatch (state_settle.go). The client guards locally and FAILS
// LOUD if the key changed across buy->settle, so a misconfigured fleet never
// silently stalls.
//
// LOUD-EVERYWHERE (relay-transport.md §0, ed2 §5): a forged/unsigned response is a
// loud-skip (never trusted), a content_hash mismatch aborts BEFORE settle(complete)
// (never confirm tampered content), oversize/pointer content LOUD-fails pointing at
// the deferred Blossom item (Blossom is NOT implemented here — gate G-blossom), an
// insufficient-scrip reject is a DISTINGUISHED underfunded outcome (not a bare
// timeout), and a timeout is AMBIGUOUS with actionable causes.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/campfire-net/dontguess/pkg/exchange"
	"github.com/campfire-net/dontguess/pkg/identity"
	"github.com/campfire-net/dontguess/pkg/nostr"
	"github.com/campfire-net/dontguess/pkg/proto"
	"github.com/campfire-net/dontguess/pkg/relay"
)

// MaxInlineContentBytes is the hard ceiling on inline delivered content the client
// will accept (design §3.5 "hard max-inline-size guard"). It mirrors the operator's
// exchange.BlossomOffloadThreshold: the operator REFUSES to inline content larger
// than that (engine_settle.go emitDeliverContent), delivering a Blossom pointer
// instead. Blossom fetch is deferred (gate G-blossom, NOT implemented in ed2), so
// an inline body exceeding this — or ANY pointer deliver — is a LOUD failure that
// points at the deferred Blossom item rather than silently trusting or truncating.
const MaxInlineContentBytes = exchange.BlossomOffloadThreshold

// SettleOutcome is the discriminated terminal result of the settle chain.
type SettleOutcome int

const (
	// SettleOutcomeSettled: the funded happy path completed — content is IN HAND
	// (hash-verified) and settle(complete) was published. Scrip moved.
	SettleOutcomeSettled SettleOutcome = iota
	// SettleOutcomeUnderfunded: the operator returned a durable settle(buyer-accept-reject)
	// (reason:insufficient_scrip) via the per-phase #e:[buyer-accept] filter (§3.6).
	// This is the H3 proof the subscription topology is right: it is RECEIVED, not a
	// bare timeout. No content, no scrip moved.
	SettleOutcomeUnderfunded
	// SettleOutcomeBudgetExceeded: the match price exceeds the caller's budget, so
	// the client did NOT publish buyer-accept (spend nothing you did not budget).
	SettleOutcomeBudgetExceeded
	// SettleOutcomeAmbiguous: a per-phase await timed out. Distinct from Underfunded:
	// the reject/deliver never arrived within the bound (relay/operator issue), not a
	// received "no".
	SettleOutcomeAmbiguous
)

func (o SettleOutcome) String() string {
	switch o {
	case SettleOutcomeSettled:
		return "settled"
	case SettleOutcomeUnderfunded:
		return "underfunded-reject"
	case SettleOutcomeBudgetExceeded:
		return "budget-exceeded"
	case SettleOutcomeAmbiguous:
		return "ambiguous-timeout"
	default:
		return "unknown"
	}
}

// SettleOptions tunes the settle chain.
type SettleOptions struct {
	// Budget is the maximum scrip the caller will spend. The client proceeds to
	// buyer-accept only when the match price <= Budget (§3.5 common path).
	Budget int64
	// Preview, when true, runs the free preview-request -> preview path before
	// buyer-accept, and the buyer-accept then e-tags the PREVIEW wire id (§3.5).
	Preview bool
	// OperatorPubKey, when non-empty (32-byte hex), restricts accepted operator
	// responses (deliver / reject / preview) to those AUTHORED by the operator —
	// belt and suspenders on top of the relay allowlist + per-event signature check.
	OperatorPubKey string
	// MaxInlineBytes overrides MaxInlineContentBytes (0 => default). Exposed so a
	// test can drive the oversize LOUD-guard without a 32 KiB fixture.
	MaxInlineBytes int
}

// SettleResult is the discriminated outcome of the settle chain.
type SettleResult struct {
	Outcome SettleOutcome

	// PreviewMsgID is the WIRE id of the operator's settle(preview) (Preview path).
	PreviewMsgID string
	// BuyerAcceptID is the WIRE id of the client's settle(buyer-accept). It is the
	// e-tag the operator's deliver AND buyer-accept-reject reply against (H3).
	BuyerAcceptID string
	// DeliverMsgID is the WIRE id of the operator's settle(deliver) (Settled path).
	DeliverMsgID string
	// CompleteMsgID is the WIRE id of the client's settle(complete) (Settled path).
	CompleteMsgID string

	// EntryID is the delivered/selected entry id.
	EntryID string
	// Price is the match price the client acted on.
	Price int64

	// Content is the decoded, HASH-VERIFIED content bytes (Settled path only).
	Content []byte
	// ContentHash is the operator's sha256: digest the client verified (Settled path).
	ContentHash string

	// RejectReason / RejectGuide are populated on Underfunded (verbatim operator text).
	RejectReason string
	RejectGuide  string
}

// Settle drives the per-phase settle chain on a real match (buy) over conn, signed
// by signer (the SAME AGENT key that signed the buy — never the operator key), and
// returns the discriminated outcome. The whole chain runs in ONE invocation under
// signer.
//
// Contract:
//   - A terminal outcome (settled / underfunded-reject / budget-exceeded /
//     ambiguous-timeout) returns (result, nil) — the caller renders it + decides
//     its exit code.
//   - A hard failure (same-key violation, content_hash mismatch, oversize/pointer
//     deliver, transport drop that cannot re-subscribe within budget, encode/publish
//     failure) returns (nil, err) — LOUD, never a silent ambiguous.
func Settle(ctx context.Context, conn *relay.Conn, signer identity.Signer, buy *BuyResult, opts SettleOptions) (*SettleResult, error) {
	if conn == nil {
		return nil, fmt.Errorf("relayclient: settle: nil conn")
	}
	if signer == nil {
		return nil, fmt.Errorf("relayclient: settle: nil signer")
	}
	if buy == nil {
		return nil, fmt.Errorf("relayclient: settle: nil buy result")
	}
	if buy.Outcome != BuyOutcomeMatch {
		return nil, fmt.Errorf("relayclient: settle: buy outcome is %s, not a match — nothing to settle", buy.Outcome)
	}
	if buy.MatchMsgID == "" {
		return nil, fmt.Errorf("relayclient: settle: buy result carries no match wire id")
	}

	// SAME-KEY invariant (§3.5): the settle MUST be signed by the exact npub that
	// signed the buy. The engine silently drops a mismatch (it re-derives
	// expectedBuyer), so a fleet that ran buy under one key and settle under another
	// would hang forever. Guard locally and FAIL LOUD. (buy.BuyerPubKey is empty only
	// for a caller-constructed BuyResult; then the guard is a no-op — Buy always sets
	// it.)
	if buy.BuyerPubKey != "" && buy.BuyerPubKey != signer.PubKeyHex() {
		return nil, fmt.Errorf("relayclient: settle: SAME-KEY VIOLATION — buy was signed by %s but settle signer is %s; the whole buy->settle chain must run under one identity (the engine silently drops a buyer-key mismatch)",
			shortID(buy.BuyerPubKey), shortID(signer.PubKeyHex()))
	}

	// Budget gate (§3.5 common path): proceed to buyer-accept only when price <= budget.
	var price int64
	selectedEntry := ""
	if m := buy.Match(); m != nil {
		price = m.Price
		selectedEntry = m.EntryID
	}
	if price > opts.Budget {
		return &SettleResult{
			Outcome: SettleOutcomeBudgetExceeded,
			EntryID: selectedEntry,
			Price:   price,
		}, nil
	}

	// The antecedent the buyer-accept e-tags: the match wire id on the common path,
	// or (with --preview) the PREVIEW wire id after the free preview round.
	acceptAntecedent := buy.MatchMsgID
	res := &SettleResult{EntryID: selectedEntry, Price: price}

	if opts.Preview {
		previewWire, timedOut, err := runPreview(ctx, conn, signer, buy.MatchMsgID, selectedEntry, opts.OperatorPubKey)
		if err != nil {
			return nil, err
		}
		if timedOut {
			res.Outcome = SettleOutcomeAmbiguous
			return res, nil
		}
		res.PreviewMsgID = previewWire
		acceptAntecedent = previewWire
	}

	// --- buyer-accept: reserve scrip + trigger the operator auto-deliver ---
	acceptPayload, err := json.Marshal(map[string]any{"entry_id": selectedEntry})
	if err != nil {
		return nil, fmt.Errorf("relayclient: settle: marshal buyer-accept payload: %w", err)
	}
	acceptEv, err := buildSettleEvent(signer, exchange.SettlePhaseStrBuyerAccept, acceptAntecedent, acceptPayload)
	if err != nil {
		return nil, fmt.Errorf("relayclient: settle: build buyer-accept: %w", err)
	}
	buyerAcceptWire := acceptEv.ID
	res.BuyerAcceptID = buyerAcceptWire

	// PER-PHASE SUBSCRIPTION (H3): subscribe #e:[buyer-accept WIRE id] BEFORE
	// publishing the buyer-accept, so BOTH the operator's auto-emitted
	// settle(deliver) AND a settle(buyer-accept-reject) — each e-tags THIS wire id,
	// not buyID — are received. A #e:[buyID] filter would receive NEITHER.
	subID := "dg-settle-ba-" + shortID(buyerAcceptWire)
	baFilter := relay.Filter{
		Kinds: []int{nostr.KindSettle},
		Tags:  map[string][]string{"e": {buyerAcceptWire}},
	}
	if err := sendReq(ctx, conn, subID, baFilter); err != nil {
		return nil, fmt.Errorf("relayclient: settle %s: subscribe #e:[buyer-accept]: %w", shortID(buyerAcceptWire), err)
	}
	if err := publishEvent(ctx, conn, acceptEv); err != nil {
		return nil, fmt.Errorf("relayclient: settle %s: publish buyer-accept: %w", shortID(buyerAcceptWire), err)
	}

	// Await the mutually-exclusive result of the buyer-accept: deliver (funded) XOR
	// buyer-accept-reject (insufficient scrip). Both e-tag the buyer-accept wire id.
	ev, timedOut, err := awaitSettle(ctx, conn, subID, baFilter, buyerAcceptWire,
		map[string]bool{
			exchange.SettlePhaseStrDeliver:           true,
			exchange.SettlePhaseStrBuyerAcceptReject: true,
		}, opts.OperatorPubKey)
	if err != nil {
		return nil, err
	}
	if timedOut {
		res.Outcome = SettleOutcomeAmbiguous
		return res, nil
	}

	switch settlePhaseOf(ev) {
	case exchange.SettlePhaseStrBuyerAcceptReject:
		reason, guide := parseRejectPayload(ev)
		res.Outcome = SettleOutcomeUnderfunded
		res.RejectReason = reason
		res.RejectGuide = guide
		return res, nil

	case exchange.SettlePhaseStrDeliver:
		content, contentHash, err := verifyDeliver(ev, opts.MaxInlineBytes)
		if err != nil {
			return nil, err
		}
		res.DeliverMsgID = ev.ID
		res.Content = content
		res.ContentHash = contentHash

		// settle(complete) is a BEHAVIORAL CONSUME SIGNAL, emitted ONLY after
		// receiving + verifying content (§3.5). It e-tags the DELIVER WIRE id; the
		// operator resolves that wire id -> store id via its alias (55c).
		completePayload, err := json.Marshal(map[string]any{
			"content_hash":          contentHash,
			"content_hash_verified": true,
		})
		if err != nil {
			return nil, fmt.Errorf("relayclient: settle: marshal complete payload: %w", err)
		}
		completeEv, err := buildSettleEvent(signer, exchange.SettlePhaseStrComplete, ev.ID, completePayload,
			exchange.TagVerdictPrefix+"accepted")
		if err != nil {
			return nil, fmt.Errorf("relayclient: settle: build complete: %w", err)
		}
		if err := publishEvent(ctx, conn, completeEv); err != nil {
			return nil, fmt.Errorf("relayclient: settle: publish complete: %w", err)
		}
		res.CompleteMsgID = completeEv.ID
		res.Outcome = SettleOutcomeSettled
		return res, nil

	default:
		// awaitSettle only returns wanted phases, so this is unreachable; guard loudly.
		return nil, fmt.Errorf("relayclient: settle: unexpected settle phase %q on %s", settlePhaseOf(ev), shortID(ev.ID))
	}
}

// runPreview runs the free preview-before-purchase round: publish
// settle(preview-request) e-tagging the match wire id, subscribe #e:[preview-request
// wire id], and await the operator's settle(preview). Returns the PREVIEW wire id
// (the antecedent the buyer-accept e-tags on this path). No scrip moves.
func runPreview(ctx context.Context, conn *relay.Conn, signer identity.Signer, matchWire, entryID, operatorPubKey string) (previewWire string, timedOut bool, err error) {
	reqPayload, err := json.Marshal(map[string]any{"entry_id": entryID})
	if err != nil {
		return "", false, fmt.Errorf("relayclient: settle: marshal preview-request payload: %w", err)
	}
	prEv, err := buildSettleEvent(signer, exchange.SettlePhaseStrPreviewRequest, matchWire, reqPayload)
	if err != nil {
		return "", false, fmt.Errorf("relayclient: settle: build preview-request: %w", err)
	}
	prWire := prEv.ID

	subID := "dg-settle-pr-" + shortID(prWire)
	prFilter := relay.Filter{
		Kinds: []int{nostr.KindSettle},
		Tags:  map[string][]string{"e": {prWire}},
	}
	if err := sendReq(ctx, conn, subID, prFilter); err != nil {
		return "", false, fmt.Errorf("relayclient: settle %s: subscribe #e:[preview-request]: %w", shortID(prWire), err)
	}
	if err := publishEvent(ctx, conn, prEv); err != nil {
		return "", false, fmt.Errorf("relayclient: settle %s: publish preview-request: %w", shortID(prWire), err)
	}

	ev, timedOut, err := awaitSettle(ctx, conn, subID, prFilter, prWire,
		map[string]bool{exchange.SettlePhaseStrPreview: true}, operatorPubKey)
	if err != nil {
		return "", false, err
	}
	if timedOut {
		return "", true, nil
	}
	return ev.ID, false, nil
}

// awaitSettle runs the bounded, re-subscribing Recv loop that waits for an operator
// settle response e-tagging antecedent whose phase tag is in wantPhases. It mirrors
// awaitBuyResponse (ed2-B): a mid-await ErrConnDropped re-issues the current REQ
// (with a `since`) before the next Recv within the remaining budget (H5); a forged /
// unsigned / wrong-author / wrong-antecedent event is a loud-skip. Returns
// (ev, false, nil) on a wanted phase, (nil, true, nil) on ctx-expiry timeout, and
// (nil, false, err) on a transport failure that cannot recover in budget.
func awaitSettle(
	ctx context.Context,
	conn *relay.Conn,
	subID string,
	filter relay.Filter,
	antecedent string,
	wantPhases map[string]bool,
	operatorPubKey string,
) (*identity.Event, bool, error) {
	for {
		raw, recvErr := conn.Recv(ctx)
		if recvErr != nil {
			if ctx.Err() != nil {
				return nil, true, nil
			}
			if errors.Is(recvErr, relay.ErrConnDropped) {
				resub := filter
				since := time.Now().Add(-resubscribeSlackSeconds * time.Second).Unix()
				resub.Since = &since
				if err := sendReq(ctx, conn, subID, resub); err != nil {
					if ctx.Err() != nil {
						return nil, true, nil
					}
					return nil, false, fmt.Errorf("relayclient: settle %s: re-subscribe after conn drop: %w", shortID(antecedent), err)
				}
				continue
			}
			return nil, false, fmt.Errorf("relayclient: settle %s: await response: %w", shortID(antecedent), recvErr)
		}

		f, perr := relay.ParseFrame(raw)
		if perr != nil {
			continue // malformed frame: loud-skip, never panic (LOCKED-5)
		}
		if f.Type != relay.LabelEVENT || f.Event == nil {
			continue
		}
		ev := f.Event
		// Never act on a forged/tampered event; belt-and-suspenders operator-author
		// gate; confirm it actually replies to OUR phase message.
		if identity.VerifyEvent(ev) != nil {
			continue
		}
		if operatorPubKey != "" && ev.PubKey != operatorPubKey {
			continue
		}
		if ev.Kind != nostr.KindSettle {
			continue
		}
		if !eventHasETag(ev, antecedent) {
			continue
		}
		if wantPhases[settlePhaseOf(ev)] {
			return ev, false, nil
		}
	}
}

// verifyDeliver validates an operator settle(deliver) event and returns the decoded,
// hash-verified content. It FAILS LOUD on: a Blossom-pointer deliver (Blossom fetch
// is deferred — gate G-blossom, NOT implemented here); oversize inline content; a
// content_hash mismatch (possible tampering — abort BEFORE settle(complete)); or a
// missing/undecodable body. Mirrors the operator's own sha256("sha256:"+hex) shape
// (engine_settle.go emitDeliverContent).
func verifyDeliver(ev *identity.Event, maxInlineBytes int) (content []byte, contentHash string, err error) {
	var payload struct {
		Phase       string `json:"phase"`
		EntryID     string `json:"entry_id"`
		Content     string `json:"content"`
		ContentHash string `json:"content_hash"`
		BlobPointer string `json:"blob_pointer"`
	}
	if uerr := json.Unmarshal([]byte(ev.Content), &payload); uerr != nil {
		return nil, "", fmt.Errorf("relayclient: settle: deliver %s: parse payload: %w", shortID(ev.ID), uerr)
	}

	if payload.BlobPointer != "" {
		return nil, "", fmt.Errorf("relayclient: settle: deliver %s: operator delivered a BLOSSOM POINTER (%s) for oversized content, but Blossom fetch is DEFERRED in this client (ed2 gate G-blossom, NOT implemented) — cannot fetch/verify; ask the operator for an inline-sized entry or track the deferred Blossom fetch item",
			shortID(ev.ID), payload.BlobPointer)
	}

	max := MaxInlineContentBytes
	if maxInlineBytes > 0 {
		max = maxInlineBytes
	}
	// Guard the base64 length BEFORE decoding so a hostile relay cannot OOM the
	// client with an enormous body: decoded size <= encoded length.
	if len(payload.Content) > max*2+8 {
		return nil, "", fmt.Errorf("relayclient: settle: deliver %s: inline content (%d base64 bytes) exceeds the max-inline guard (%d bytes) — oversized content must travel as a Blossom pointer, which is DEFERRED (gate G-blossom); refusing to inline",
			shortID(ev.ID), len(payload.Content), max)
	}
	content, derr := base64.StdEncoding.DecodeString(payload.Content)
	if derr != nil {
		return nil, "", fmt.Errorf("relayclient: settle: deliver %s: content is not valid base64: %w", shortID(ev.ID), derr)
	}
	if len(content) == 0 {
		return nil, "", fmt.Errorf("relayclient: settle: deliver %s: empty content body", shortID(ev.ID))
	}
	if len(content) > max {
		return nil, "", fmt.Errorf("relayclient: settle: deliver %s: decoded content %d bytes exceeds the max-inline guard (%d bytes) — oversized content must travel as a Blossom pointer, which is DEFERRED (gate G-blossom)",
			shortID(ev.ID), len(content), max)
	}

	rawHash := sha256.Sum256(content)
	got := "sha256:" + hex.EncodeToString(rawHash[:])
	if payload.ContentHash == "" {
		return nil, "", fmt.Errorf("relayclient: settle: deliver %s: operator supplied no content_hash — cannot verify integrity; refusing to settle(complete)", shortID(ev.ID))
	}
	if got != payload.ContentHash {
		return nil, "", fmt.Errorf("relayclient: settle: deliver %s: CONTENT HASH MISMATCH — computed %s but operator claimed %s (possible tampering); NOT sending settle(complete)",
			shortID(ev.ID), got, payload.ContentHash)
	}
	return content, payload.ContentHash, nil
}

// parseRejectPayload extracts the reason + guide from a settle(buyer-accept-reject)
// event (§3.6). Both are operator text surfaced verbatim to the buyer.
func parseRejectPayload(ev *identity.Event) (reason, guide string) {
	var payload struct {
		Reason string `json:"reason"`
		Guide  string `json:"guide"`
	}
	_ = json.Unmarshal([]byte(ev.Content), &payload)
	return payload.Reason, payload.Guide
}

// settlePhaseOf returns the settle phase carried by ev's ["phase", X] tag (the
// adapter maps exchange:phase:X -> ["phase", X]). "" if absent.
func settlePhaseOf(ev *identity.Event) string {
	for _, t := range ev.Tags {
		if len(t) >= 2 && t[0] == "phase" {
			return t[1]
		}
	}
	return ""
}

// buildSettleEvent builds + signs a buyer-authored settle(phase) event e-tagging
// antecedent, with the given JSON payload and any extraTags (e.g. a verdict tag).
// The tags are [exchange:settle, exchange:phase:<phase>, extraTags...]; the adapter
// maps the phase tag to ["phase", <phase>] and the antecedent to a reply e-tag. The
// returned event's ID is the deterministic content-hash WIRE id (the id the client
// e-tags downstream and the operator resolves via its alias).
func buildSettleEvent(signer identity.Signer, phase, antecedent string, payload []byte, extraTags ...string) (*identity.Event, error) {
	tags := append([]string{exchange.TagSettle, exchange.TagPhasePrefix + phase}, extraTags...)
	msg := &proto.Message{
		Sender:      signer.PubKeyHex(),
		Payload:     payload,
		Tags:        tags,
		Antecedents: []string{antecedent},
		Timestamp:   time.Now().UnixNano(),
	}
	return signAsIdentityEvent(signer, msg)
}

// publishEvent encodes + sends an EVENT frame on conn, bounded by ctx.
func publishEvent(ctx context.Context, conn *relay.Conn, ev *identity.Event) error {
	frame, err := relay.EncodeEvent(ev)
	if err != nil {
		return fmt.Errorf("encode EVENT: %w", err)
	}
	if err := conn.Send(ctx, frame); err != nil {
		return fmt.Errorf("send EVENT: %w", err)
	}
	return nil
}

// WriteSettleOutcome renders a SettleResult as the LOUD, human-facing block the
// `buy` command prints to stderr on a hit. The raw content itself is written
// separately (to stdout) so it stays pipeable; this is the summary/diagnostic.
func WriteSettleOutcome(w io.Writer, buyID string, r *SettleResult) {
	if r == nil {
		fmt.Fprintln(w, "settle: no result")
		return
	}
	switch r.Outcome {
	case SettleOutcomeSettled:
		fmt.Fprintf(w, "buy %s: SETTLED — content in hand (%d bytes, %s), scrip moved.\n",
			shortID(buyID), len(r.Content), r.ContentHash[:min(24, len(r.ContentHash))])
		fmt.Fprintf(w, "  entry %s  price %d scrip  buyer-accept %s  deliver %s  complete %s\n",
			shortID(r.EntryID), r.Price, shortID(r.BuyerAcceptID), shortID(r.DeliverMsgID), shortID(r.CompleteMsgID))
	case SettleOutcomeUnderfunded:
		fmt.Fprintf(w, "buy %s: UNDERFUNDED — the operator REJECTED your buyer-accept: %s\n", shortID(buyID), r.RejectReason)
		if r.RejectGuide != "" {
			fmt.Fprintf(w, "  operator: %s\n", r.RejectGuide)
		}
		fmt.Fprintln(w, "  No content was delivered and no scrip moved.")
		fmt.Fprintln(w, "  ask the operator to run: dontguess mint <your-npub> <amount>")
	case SettleOutcomeBudgetExceeded:
		fmt.Fprintf(w, "buy %s: PRICE EXCEEDS BUDGET — match price %d scrip is above your budget; NOT accepting (no scrip spent).\n", shortID(buyID), r.Price)
		fmt.Fprintln(w, "  Re-run with a higher --budget to purchase, or preview first with --preview.")
	case SettleOutcomeAmbiguous:
		// Enumerated causes (dontguess-980), mirroring the pattern in
		// pkg/relayclient/buy.go's ambiguousResult(). A per-phase settle await can
		// time out for reasons besides operator/relay slowness: buyer-side settle
		// phases (buyer-accept, complete, ...) require TrustAllowlisted
		// (pkg/exchange/trust.go defaultSettlePhaseLevels), while OperationBuy
		// itself is only TrustAnonymous — so a buyer who was minted but never
		// fleet-allowlisted matches fine and then has their buyer-accept silently
		// dropped pre-fold at the dispatch trust gate (no reject is emitted; it
		// looks identical to a slow operator). Team onboarding requires BOTH
		// `dontguess allowlist add` and `dontguess mint` for a buyer
		// (docs/design/nostr-first-client-ed2.md §3.4).
		fmt.Fprintf(w, "buy %s: AMBIGUOUS — matched, but the settle chain timed out before content or a reject arrived. Possible causes:\n", shortID(buyID))
		fmt.Fprintln(w, "  - operator may be slow, or the relay may not have replayed the response — this is NOT a decline; retry with a larger --timeout")
		fmt.Fprintln(w, "  - buyer may not be fleet-allowlisted — settle(buyer-accept) requires fleet-member trust and is silently dropped for an unallowlisted buyer, even if minted — ask the operator to run: dontguess allowlist add <your-npub>")
	default:
		fmt.Fprintf(w, "buy %s: unknown settle outcome\n", shortID(buyID))
	}
}
