package relayclient

// assign.go is item dontguess-d26 (#2 AGENT DOOR): the team-tier CLIENT side of
// the assign lifecycle — discovery + claim/complete publish. The assign
// lifecycle itself (post/claim/complete/accept/reject/expire) already folds
// correctly engine-side (pkg/exchange/state_assign.go, dispatched generically
// by engine_core.go's op switch regardless of transport); this file is the
// missing CLI door onto it. AssignClaim/AssignComplete mirror buy.go/put.go's
// sign(agentKey) -> publish(relay) chain exactly — never the operator key.
//
// FetchOpenAssigns is the discovery half: exchange:assign messages are
// OPERATOR-authored broadcasts the Outbox already publishes to the relay like
// any other operator record (pkg/relay/outbox.go), so a fleet member can
// discover open/claimable work by REQ-subscribing kind=3405 and folding the
// result into a local scratch exchange.State — no operator IPC needed on this
// tier (contrast the individual/zero-relay tier, ipc.go's OpListAssigns, which
// reads the live engine's State directly over the local operator socket).

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/3dl-dev/dontguess/pkg/exchange"
	"github.com/3dl-dev/dontguess/pkg/identity"
	"github.com/3dl-dev/dontguess/pkg/nostr"
	"github.com/3dl-dev/dontguess/pkg/proto"
	"github.com/3dl-dev/dontguess/pkg/relay"
)

// DefaultAssignActionTimeout bounds an assign-claim/assign-complete publish
// (dial + publish + await relay OK). Mirrors DefaultTimeout's role for Put —
// these are one-shot publishes, not awaited settlement chains, so the same
// bound applies.
const DefaultAssignActionTimeout = 15 * time.Second

// DefaultAssignListTimeout bounds the `dontguess assigns` discovery fetch
// (dial + REQ + collect-until-EOSE).
const DefaultAssignListTimeout = 10 * time.Second

// AssignActionResult is the outcome of a claim/complete publish. Mirrors
// PutResult's transport-receipt-only semantics (§3.1): Accepted means the
// relay stored the event, NOT that the engine's fold admitted the claim — an
// exclusive-sender mismatch, an already-claimed task, or a stale antecedent
// silently no-ops in the fold (state_assign.go). The caller has no synchronous
// admission signal beyond the relay receipt; running `dontguess assigns` again
// is how a caller confirms a claim actually took.
type AssignActionResult struct {
	EventID   string
	Accepted  bool
	OKMessage string
}

// AssignClaim signs an exchange:assign-claim(3405) event e-tagging assignID
// with signer (the AGENT key) and publishes it on conn, returning once the
// relay's OK for that event arrives (bounded by ctx).
func AssignClaim(ctx context.Context, conn *relay.Conn, signer identity.Signer, assignID string) (*AssignActionResult, error) {
	return publishAssignSubOp(ctx, conn, signer, exchange.TagAssignClaim, assignID, []byte(`{}`))
}

// AssignComplete signs an exchange:assign-complete(3405) event e-tagging
// claimEventID (the assign-CLAIM event id — NOT the assign id itself; see
// state_assign.go applyAssignComplete, which resolves the assign via the claim
// antecedent) with signer and publishes it. result is the raw JSON payload the
// engine stores verbatim as AssignRecord.Result; for a "compress" task_type it
// must carry content_hash+content_size for createCompressionDerivative
// (engine_core.go) to parse — BuildAssignResult builds that shape.
func AssignComplete(ctx context.Context, conn *relay.Conn, signer identity.Signer, claimEventID string, result []byte) (*AssignActionResult, error) {
	return publishAssignSubOp(ctx, conn, signer, exchange.TagAssignComplete, claimEventID, result)
}

// BuildAssignResult renders the assign-complete result payload
// createCompressionDerivative (pkg/exchange/engine_core.go) parses:
// content_hash (sha256:-prefixed, over the completed content) + content_size.
// The content itself rides alongside (base64) so an operator/reviewer can
// inspect the submission without a second fetch — the engine only reads
// content_hash/content_size.
func BuildAssignResult(content []byte) ([]byte, error) {
	return json.Marshal(map[string]any{
		"content_hash": sha256Ref(content),
		"content_size": int64(len(content)),
		"content":      base64.StdEncoding.EncodeToString(content),
	})
}

func publishAssignSubOp(ctx context.Context, conn *relay.Conn, signer identity.Signer, tag, antecedent string, payload []byte) (*AssignActionResult, error) {
	if conn == nil {
		return nil, fmt.Errorf("relayclient: %s: nil conn", tag)
	}
	if signer == nil {
		return nil, fmt.Errorf("relayclient: %s: nil signer", tag)
	}
	if antecedent == "" {
		return nil, fmt.Errorf("relayclient: %s: empty antecedent id", tag)
	}
	msg := &proto.Message{
		Sender:      signer.PubKeyHex(),
		Payload:     payload,
		Tags:        []string{tag},
		Antecedents: []string{antecedent},
		Timestamp:   time.Now().UnixNano(),
	}
	ev, err := signAsIdentityEvent(signer, msg)
	if err != nil {
		return nil, fmt.Errorf("relayclient: %s: sign event: %w", tag, err)
	}
	accepted, okMsg, err := PublishEvent(ctx, conn, ev)
	if err != nil {
		return nil, fmt.Errorf("relayclient: %s %s: %w", tag, shortID(ev.ID), err)
	}
	return &AssignActionResult{EventID: ev.ID, Accepted: accepted, OKMessage: okMsg}, nil
}

// OpenAssign is a discovery-friendly view of one claimable assign task,
// surfaced by FetchOpenAssigns. Description is carried separately from
// exchange.AssignRecord (which does not persist it) by reading it directly off
// the original exchange:assign event's payload during the fold.
type OpenAssign struct {
	AssignID        string `json:"assign_id"`
	EntryID         string `json:"entry_id,omitempty"`
	TaskType        string `json:"task_type"`
	Reward          int64  `json:"reward"`
	Status          string `json:"status"`
	ExclusiveSender string `json:"exclusive_sender,omitempty"`
	Description     string `json:"description,omitempty"`
}

// FetchOpenAssigns REQ-subscribes for every exchange:assign* (kind 3405) event
// on conn, folds them (in created_at order) into a scratch exchange.State
// pinned to operatorPubKeyHex — so a forged non-operator assign/accept/reject/
// expire cannot spoof the fold (state_assign.go's per-handler operator-only
// guards) — and returns the open/claimable tasks agentPubKeyHex may claim:
// ExclusiveSender=="" (open to anyone) or ExclusiveSender==agentPubKeyHex.
//
// This is a best-effort, eventually-consistent READ over whatever the relay
// has stored (backfilled history + anything published before EOSE), not a live
// subscription. It terminates on EOSE for its subscription id, bounded by ctx;
// it re-subscribes (with a `since` slack) on a mid-fetch conn drop, mirroring
// fetchPutCiphertext's H5 discipline (settle.go).
func FetchOpenAssigns(ctx context.Context, conn *relay.Conn, operatorPubKeyHex, agentPubKeyHex string) ([]OpenAssign, error) {
	events, err := fetchAllAssignEvents(ctx, conn)
	if err != nil {
		return nil, err
	}

	// Sort by created_at ascending (ties broken by id) so antecedent-referencing
	// sub-ops (claim -> assign, complete -> claim) always fold AFTER the message
	// they reference — State.Apply silently no-ops on an antecedent it has not
	// seen yet (state_assign.go), so fold order matters here exactly as it does
	// for the operator's own poll-ordered local log.
	sort.Slice(events, func(i, j int) bool {
		if events[i].CreatedAt != events[j].CreatedAt {
			return events[i].CreatedAt < events[j].CreatedAt
		}
		return events[i].ID < events[j].ID
	})

	st := exchange.NewState()
	st.OperatorKey = operatorPubKeyHex

	descByAssignID := make(map[string]string, len(events))
	for _, ev := range events {
		// Never fold an unsigned/forged event (mirrors parsePutReject's
		// discipline) — a passive relay reader could otherwise inject a bogus
		// assign/claim/complete into this client-local scratch fold.
		if verr := identity.VerifyEvent(ev); verr != nil {
			continue
		}
		msg, cerr := nostr.FromNostrEvent(identityToNostrEvent(ev))
		if cerr != nil {
			continue // malformed/unknown-shape event: loud-but-skip, never panic
		}
		if hasAssignOp(msg.Tags, exchange.TagAssign) {
			var p struct {
				Description string `json:"description"`
			}
			if json.Unmarshal(msg.Payload, &p) == nil && p.Description != "" {
				descByAssignID[msg.ID] = p.Description
			}
		}
		st.Apply(msg)
	}

	active := st.AllActiveAssigns()
	out := make([]OpenAssign, 0, len(active))
	for _, rec := range active {
		if rec.ExclusiveSender != "" && rec.ExclusiveSender != agentPubKeyHex {
			continue // exclusive to a different agent — not claimable by us
		}
		out = append(out, OpenAssign{
			AssignID:        rec.AssignID,
			EntryID:         rec.EntryID,
			TaskType:        rec.TaskType,
			Reward:          rec.Reward,
			Status:          rec.Status.String(),
			ExclusiveSender: rec.ExclusiveSender,
			Description:     descByAssignID[rec.AssignID],
		})
	}
	return out, nil
}

// identityToNostrEvent copies a wire identity.Event (the shape relay.ParseFrame
// hands back off an EVENT frame) into the structurally identical nostr.Event
// the adapter (nostr.FromNostrEvent) consumes. Mirrors
// cmd/dontguess/serve_relay.go's identityToNostrEvent — duplicated here rather
// than exported cross-package for a one-struct-literal copy (package main
// cannot be imported).
func identityToNostrEvent(ev *identity.Event) *nostr.Event {
	return &nostr.Event{
		ID:        ev.ID,
		PubKey:    ev.PubKey,
		CreatedAt: ev.CreatedAt,
		Kind:      ev.Kind,
		Tags:      ev.Tags,
		Content:   ev.Content,
		Sig:       ev.Sig,
	}
}

// hasAssignOp reports whether tags carries op.
func hasAssignOp(tags []string, op string) bool {
	for _, t := range tags {
		if t == op {
			return true
		}
	}
	return false
}

// fetchAllAssignEvents REQ-subscribes kind=KindAssign (no id/tag filter — every
// assign sub-op shares this one kind) and collects every EVENT until EOSE for
// its subscription id, bounded by ctx. Re-subscribes (with a `since` slack)
// after a mid-fetch relay.ErrConnDropped, mirroring fetchPutCiphertext's H5
// discipline (settle.go).
func fetchAllAssignEvents(ctx context.Context, conn *relay.Conn) ([]*identity.Event, error) {
	if conn == nil {
		return nil, fmt.Errorf("relayclient: fetch assigns: nil conn")
	}
	subID := "dg-assigns-" + shortID(fmt.Sprintf("%x", time.Now().UnixNano()))
	filter := relay.Filter{Kinds: []int{nostr.KindAssign}}
	if err := sendReq(ctx, conn, subID, filter); err != nil {
		return nil, fmt.Errorf("relayclient: fetch assigns: subscribe: %w", err)
	}

	var events []*identity.Event
	for {
		raw, recvErr := conn.Recv(ctx)
		if recvErr != nil {
			if ctx.Err() != nil {
				return nil, fmt.Errorf("relayclient: fetch assigns: timed out before EOSE: %w", ctx.Err())
			}
			if errors.Is(recvErr, relay.ErrConnDropped) {
				resub := filter
				since := time.Now().Add(-resubscribeSlackSeconds * time.Second).Unix()
				resub.Since = &since
				if err := sendReq(ctx, conn, subID, resub); err != nil {
					return nil, fmt.Errorf("relayclient: fetch assigns: re-subscribe after conn drop: %w", err)
				}
				continue
			}
			return nil, fmt.Errorf("relayclient: fetch assigns: recv: %w", recvErr)
		}
		f, perr := relay.ParseFrame(raw)
		if perr != nil {
			continue // malformed frame: loud-but-skip, never panic
		}
		if f.Type == relay.LabelEOSE && f.SubID == subID {
			return events, nil
		}
		if f.Type == relay.LabelEVENT && f.Event != nil && f.Event.Kind == nostr.KindAssign {
			events = append(events, f.Event)
		}
	}
}
