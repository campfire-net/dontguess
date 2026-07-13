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

	"github.com/3dl-dev/dontguess/pkg/exchange"
	"github.com/3dl-dev/dontguess/pkg/identity"
	"github.com/3dl-dev/dontguess/pkg/nip44"
	"github.com/3dl-dev/dontguess/pkg/nostr"
	"github.com/3dl-dev/dontguess/pkg/proto"
	"github.com/3dl-dev/dontguess/pkg/relay"
	"golang.org/x/crypto/chacha20poly1305"
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
		content, contentHash, err := verifyDeliver(ctx, conn, signer, ev, opts.MaxInlineBytes)
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

// deliverConn is the minimal relay-transport surface verifyDeliver needs to
// REQ-fetch the already-public ciphertext referenced by a §3.4 v2 deliver. The
// production *relay.Conn satisfies it; a unit test supplies a fake so the decrypt
// path is exercised without a live relay (the REAL relay fetch is exercised by
// the cmd/dontguess TeamRoundTrip E2E).
type deliverConn interface {
	Send(ctx context.Context, frame []byte) error
	Recv(ctx context.Context) ([]byte, error)
}

// deliverPayload is the superset of BOTH settle(deliver) wire shapes:
//   - the LEGACY individual-tier plaintext deliver ({content, content_hash,
//     blob_pointer}); and
//   - the §3.4 v2 confidential envelope ({v:2, ciphertext_ref, ciphertext_hash,
//     key_wrap}) the operator now emits for a team-tier encrypted entry
//     (dontguess-9e8 emitDeliverEnvelope).
//
// verifyDeliver dispatches on shape (§3.1(6)): a v2 payload carries a key_wrap;
// the legacy payload carries a plaintext content field.
type deliverPayload struct {
	Phase string `json:"phase"`
	V     int    `json:"v"`

	// legacy individual-tier plaintext deliver
	Content     string `json:"content"`
	ContentHash string `json:"content_hash"`
	BlobPointer string `json:"blob_pointer"`

	// §3.4 v2 confidential envelope
	EntryID       string `json:"entry_id"`
	ContentAlg    string `json:"content_alg"`
	CiphertextRef struct {
		PutEvent    string `json:"put_event"`
		BlobPointer string `json:"blob_pointer"`
	} `json:"ciphertext_ref"`
	CiphertextHash string `json:"ciphertext_hash"`
	KeyWrap        struct {
		Alg       string `json:"alg"`
		Recipient string `json:"recipient"`
		Wrapped   string `json:"wrapped"`
	} `json:"key_wrap"`
}

// verifyDeliver validates an operator settle(deliver) event and returns the
// decoded, integrity-verified PLAINTEXT the buyer pipes to stdout. It dispatches
// on the deliver wire shape (§3.1(6)):
//
//   - §3.4 v2 confidential envelope (key_wrap present, team tier): unwrap the CEK
//     (NIP-44 from the operator = the deliver author), REQ-fetch the already-public
//     ciphertext referenced by ciphertext_ref.put_event, verify
//     sha256(ciphertext)==ciphertext_hash BEFORE decrypting, then AEAD-decrypt
//     (dontguess-5db). This is the paying buyer's end-to-end decrypt.
//   - LEGACY individual-tier plaintext deliver (content field present): the
//     unchanged decode+hash-verify path (backward compat — individual tier is
//     already confidential and stays byte-for-byte unchanged, design §Scope).
//
// It FAILS LOUD (returns err, never settle(complete)) on: a Blossom-pointer/-ref
// deliver (Blossom buyer fetch is DEFERRED — dontguess-640); oversize content; a
// hash mismatch (possible tampering); a misrouted/undecryptable wrap; or a
// missing/undecodable body.
func verifyDeliver(ctx context.Context, conn deliverConn, signer identity.Signer, ev *identity.Event, maxInlineBytes int) (content []byte, contentHash string, err error) {
	var payload deliverPayload
	if uerr := json.Unmarshal([]byte(ev.Content), &payload); uerr != nil {
		return nil, "", fmt.Errorf("relayclient: settle: deliver %s: parse payload: %w", shortID(ev.ID), uerr)
	}

	// Dispatch on shape. A v2 confidential deliver is identified by v>=2 OR a
	// present key_wrap.wrapped (the operator always emits both together); the
	// legacy plaintext deliver has neither.
	if payload.V >= 2 || payload.KeyWrap.Wrapped != "" {
		return decryptDeliverV2(ctx, conn, signer, ev, &payload, maxInlineBytes)
	}
	return verifyLegacyDeliver(ev, &payload, maxInlineBytes)
}

// verifyLegacyDeliver is the unchanged individual-tier plaintext deliver path:
// decode the inline base64 content and verify its sha256 against content_hash
// (mirrors the operator's sha256("sha256:"+hex) shape). conn/signer are unused on
// this path — an individual-tier deliver carries its content inline.
func verifyLegacyDeliver(ev *identity.Event, payload *deliverPayload, maxInlineBytes int) (content []byte, contentHash string, err error) {
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

// decryptDeliverV2 is the paying buyer's end-to-end decrypt of a §3.4 v2
// confidential deliver (dontguess-5db). Steps (§3.1(6)):
//  1. Unwrap the CEK: cek = NIP-44.Open(buyerSigner, operatorPub, key_wrap.wrapped).
//     The operator key IS the deliver's AUTHOR (ev.PubKey): the one secp256k1
//     keypair both Schnorr-signs the deliver and performs the NIP-44 ECDH wrap
//     (§4.5.5), and awaitSettle already verified ev's signature (and, when set,
//     that the author is opts.OperatorPubKey) upstream — so ev.PubKey is the
//     operator's wrap key, no separate operator-pubkey plumbing needed.
//  2. Fetch the already-public ciphertext from ciphertext_ref.put_event (the 3401
//     put event id), reading its enc.ciphertext.
//  3. Verify sha256(ciphertext)==ciphertext_hash BEFORE decrypting (integrity;
//     mismatch ⇒ abort, do NOT settle(complete)).
//  4. AEAD-decrypt: nonce = first NonceSize bytes of the ciphertext; plaintext =
//     ChaCha20-Poly1305(cek).Open(nonce, rest).
//
// Anti-replay binding (§4.5.4): the wrap is sealed to the antecedent-chain buyer
// key, so a captured deliver replayed toward a DIFFERENT buyer is simply
// undecryptable by them (their key cannot unwrap). We also fail fast if
// key_wrap.recipient is present and is not our key (a misrouted deliver).
func decryptDeliverV2(ctx context.Context, conn deliverConn, signer identity.Signer, ev *identity.Event, p *deliverPayload, maxInlineBytes int) (content []byte, contentHash string, err error) {
	if signer == nil {
		return nil, "", fmt.Errorf("relayclient: settle: deliver %s: v2 confidential deliver requires a buyer signer to unwrap the CEK, got nil", shortID(ev.ID))
	}
	// A blob-pointer ciphertext reference ⇒ Blossom buyer fetch, which is DEFERRED
	// (dontguess-640, NOT implemented here). Loud-fail — never silently skip.
	if p.CiphertextRef.BlobPointer != "" {
		return nil, "", fmt.Errorf("relayclient: settle: deliver %s: v2 deliver references a BLOSSOM blob (%s) but buyer-side Blossom fetch is DEFERRED (dontguess-640, NOT implemented) — cannot fetch/verify; track the deferred Blossom fetch item",
			shortID(ev.ID), p.CiphertextRef.BlobPointer)
	}
	if p.KeyWrap.Alg != "" && p.KeyWrap.Alg != "nip44-v2-secp256k1" {
		return nil, "", fmt.Errorf("relayclient: settle: deliver %s: unsupported key_wrap.alg %q (want nip44-v2-secp256k1)", shortID(ev.ID), p.KeyWrap.Alg)
	}
	if p.KeyWrap.Wrapped == "" {
		return nil, "", fmt.Errorf("relayclient: settle: deliver %s: v2 deliver has no key_wrap.wrapped — nothing to unwrap; refusing to settle(complete)", shortID(ev.ID))
	}
	if p.CiphertextHash == "" {
		return nil, "", fmt.Errorf("relayclient: settle: deliver %s: v2 deliver supplied no ciphertext_hash — cannot verify integrity; refusing to settle(complete)", shortID(ev.ID))
	}
	if p.CiphertextRef.PutEvent == "" {
		return nil, "", fmt.Errorf("relayclient: settle: deliver %s: v2 deliver has no ciphertext_ref.put_event — nowhere to fetch the ciphertext from; refusing to settle(complete)", shortID(ev.ID))
	}
	// Fail fast on a misrouted deliver: the wrap MUST be addressed to OUR key. (The
	// cryptographic binding is the real defense — a forged recipient label still
	// cannot make our key unwrap a CEK sealed to someone else — but this is a
	// cheaper, clearer error for the honest-misroute case.)
	if p.KeyWrap.Recipient != "" && p.KeyWrap.Recipient != signer.PubKeyHex() {
		return nil, "", fmt.Errorf("relayclient: settle: deliver %s: key_wrap.recipient %s is not our key %s — this deliver was wrapped for a different buyer; cannot unwrap",
			shortID(ev.ID), shortID(p.KeyWrap.Recipient), shortID(signer.PubKeyHex()))
	}

	// (1) Unwrap the CEK from the operator (= the signed deliver's author).
	cek, uerr := nip44.Open(signer, ev.PubKey, p.KeyWrap.Wrapped)
	if uerr != nil {
		return nil, "", fmt.Errorf("relayclient: settle: deliver %s: unwrap CEK (NIP-44 open from operator %s) failed: %w — this buyer key cannot decrypt the wrap (a deliver sealed to a different buyer is undecryptable here, §4.5.4)",
			shortID(ev.ID), shortID(ev.PubKey), uerr)
	}
	if len(cek) != chacha20poly1305.KeySize {
		return nil, "", fmt.Errorf("relayclient: settle: deliver %s: unwrapped CEK is %d bytes, want %d", shortID(ev.ID), len(cek), chacha20poly1305.KeySize)
	}

	// (2) Fetch the already-public ciphertext from the referenced 3401 put event.
	ciphertext, ferr := fetchPutCiphertext(ctx, conn, p.CiphertextRef.PutEvent, maxInlineBytes)
	if ferr != nil {
		return nil, "", fmt.Errorf("relayclient: settle: deliver %s: %w", shortID(ev.ID), ferr)
	}

	// (3) Verify sha256(ciphertext)==ciphertext_hash BEFORE decrypting.
	sum := sha256.Sum256(ciphertext)
	got := "sha256:" + hex.EncodeToString(sum[:])
	if got != p.CiphertextHash {
		return nil, "", fmt.Errorf("relayclient: settle: deliver %s: CIPHERTEXT HASH MISMATCH — computed %s but the deliver claimed %s (possible tampering); NOT sending settle(complete)",
			shortID(ev.ID), got, p.CiphertextHash)
	}

	// (4) AEAD-decrypt: nonce = first NonceSize bytes; rest = sealed || tag.
	aead, aerr := chacha20poly1305.New(cek)
	if aerr != nil {
		return nil, "", fmt.Errorf("relayclient: settle: deliver %s: init content AEAD: %w", shortID(ev.ID), aerr)
	}
	ns := aead.NonceSize()
	if len(ciphertext) < ns {
		return nil, "", fmt.Errorf("relayclient: settle: deliver %s: ciphertext %d bytes is shorter than the %d-byte nonce", shortID(ev.ID), len(ciphertext), ns)
	}
	nonce, sealed := ciphertext[:ns], ciphertext[ns:]
	plaintext, oerr := aead.Open(nil, nonce, sealed, nil)
	if oerr != nil {
		return nil, "", fmt.Errorf("relayclient: settle: deliver %s: AEAD open failed: %w — ciphertext or CEK corrupt", shortID(ev.ID), oerr)
	}
	// Return the ciphertext_hash as the "contentHash": it is what the operator
	// tracks for this entry and what the buyer's settle(complete) echoes. (The
	// operator's complete handler derives everything from the antecedent chain and
	// does not re-validate this value — engine_settle.go handleSettleComplete.)
	return plaintext, p.CiphertextHash, nil
}

// fetchPutCiphertext REQ-fetches the 3401 put event by id over conn and returns
// its decoded inline enc.ciphertext bytes. It verifies the put event's signature
// (never trust a forged/unsigned put) and fails loud on a blob-only put (Blossom
// buyer fetch DEFERRED — dontguess-640). It re-subscribes after a mid-fetch conn
// drop (mirrors awaitSettle's H5 discipline) and terminates on EOSE-for-our-subID
// (put not found) or ctx expiry.
func fetchPutCiphertext(ctx context.Context, conn deliverConn, putEventID string, maxInlineBytes int) ([]byte, error) {
	if conn == nil {
		return nil, fmt.Errorf("fetch put %s: nil conn — cannot fetch the referenced ciphertext", shortID(putEventID))
	}
	subID := "dg-fetch-ct-" + shortID(putEventID)
	filter := relay.Filter{
		Kinds: []int{nostr.KindPut},
		IDs:   []string{putEventID},
	}
	sendFetchReq := func(f relay.Filter) error {
		reqFrame, encErr := relay.EncodeReq(subID, f)
		if encErr != nil {
			return fmt.Errorf("encode put-fetch REQ: %w", encErr)
		}
		return conn.Send(ctx, reqFrame)
	}
	if err := sendFetchReq(filter); err != nil {
		return nil, fmt.Errorf("fetch put %s: subscribe #ids:[put_event]: %w", shortID(putEventID), err)
	}

	max := MaxInlineContentBytes
	if maxInlineBytes > 0 {
		max = maxInlineBytes
	}

	for {
		raw, recvErr := conn.Recv(ctx)
		if recvErr != nil {
			if ctx.Err() != nil {
				return nil, fmt.Errorf("fetch put %s: timed out before the ciphertext arrived: %w", shortID(putEventID), ctx.Err())
			}
			if errors.Is(recvErr, relay.ErrConnDropped) {
				resub := filter
				since := time.Now().Add(-resubscribeSlackSeconds * time.Second).Unix()
				resub.Since = &since
				if err := sendFetchReq(resub); err != nil {
					if ctx.Err() != nil {
						return nil, fmt.Errorf("fetch put %s: timed out on re-subscribe: %w", shortID(putEventID), ctx.Err())
					}
					return nil, fmt.Errorf("fetch put %s: re-subscribe after conn drop: %w", shortID(putEventID), err)
				}
				continue
			}
			return nil, fmt.Errorf("fetch put %s: recv: %w", shortID(putEventID), recvErr)
		}

		f, perr := relay.ParseFrame(raw)
		if perr != nil {
			continue // malformed frame: loud-skip, never panic
		}
		if f.Type == relay.LabelEOSE && f.SubID == subID {
			return nil, fmt.Errorf("fetch put %s: relay signalled end-of-stored-events (EOSE) with no matching put — the referenced ciphertext is not retrievable", shortID(putEventID))
		}
		if f.Type != relay.LabelEVENT || f.Event == nil {
			continue
		}
		pev := f.Event
		if pev.ID != putEventID || pev.Kind != nostr.KindPut {
			continue
		}
		// Never trust an unsigned/forged put: its ciphertext hash is the integrity
		// anchor, so the event carrying it must be authentic.
		if identity.VerifyEvent(pev) != nil {
			continue
		}
		return extractPutCiphertext(pev, max)
	}
}

// extractPutCiphertext decodes the inline enc.ciphertext (base64) from a fetched
// 3401 put event. It fails loud on a blob-only put (Blossom deferred) and guards
// the base64 length before decoding so a hostile relay cannot OOM the client.
func extractPutCiphertext(pev *identity.Event, max int) ([]byte, error) {
	var put struct {
		Enc struct {
			Ciphertext  string `json:"ciphertext"`
			BlobPointer string `json:"blob_pointer"`
		} `json:"enc"`
	}
	if err := json.Unmarshal([]byte(pev.Content), &put); err != nil {
		return nil, fmt.Errorf("parse referenced put %s payload: %w", shortID(pev.ID), err)
	}
	if put.Enc.Ciphertext == "" {
		return nil, fmt.Errorf("referenced put %s carries no inline enc.ciphertext (blob-only) — Blossom buyer fetch is DEFERRED (dontguess-640)", shortID(pev.ID))
	}
	// decoded size <= encoded length; guard before allocating.
	if len(put.Enc.Ciphertext) > max*2+64 {
		return nil, fmt.Errorf("referenced put %s ciphertext (%d base64 bytes) exceeds the max-inline guard (%d bytes) — oversized content travels as a Blossom blob (DEFERRED, dontguess-640)", shortID(pev.ID), len(put.Enc.Ciphertext), max)
	}
	ciphertext, err := base64.StdEncoding.DecodeString(put.Enc.Ciphertext)
	if err != nil {
		return nil, fmt.Errorf("referenced put %s enc.ciphertext is not valid base64: %w", shortID(pev.ID), err)
	}
	if len(ciphertext) == 0 {
		return nil, fmt.Errorf("referenced put %s has an empty ciphertext body", shortID(pev.ID))
	}
	return ciphertext, nil
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
