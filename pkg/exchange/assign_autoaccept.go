package exchange

// assign_autoaccept.go is item dontguess-462: the operator-side
// AUTO-VALIDATE-AND-PAY surface for completed COMPRESSION assigns — the
// cold-start earn-from-labor mint. It closes two confirmed gaps:
//
//   - e51: assign-labor earning was unreachable in production. Engine.AcceptAssign
//     (the accept+pay leg, dontguess-d26) had NO production caller — no CLI, no
//     IPC op, no ticker — so a fleet agent could claim+complete a compression
//     assign but never actually get paid without a human operator running
//     AcceptAssign by hand. RunAutoAcceptAssigns is that missing caller: a
//     periodic operator ticker (wired into serve exactly like the auto-accept-put
//     ticker and the ffb medium loop) that scans State for completed compression
//     assigns and accepts+pays every one that passes validation.
//
//   - 491f: the pay path did not re-check the allowlist. applyAssignClaim /
//     applyAssignComplete fold WITHOUT a trust filter (the dispatch trust gate
//     runs on the ENGINE dispatch path, but a relay-folded claim/complete reaches
//     State.Apply directly), so a de-allowlisted or never-admitted key could sit
//     on a completed assign. Validation gate (a) re-runs TrustChecker.Check at PAY
//     time — the last line before scrip moves — so payment is gated on CURRENT
//     fleet membership, not membership at claim time.
//
// ALL validation is operator-COMPUTABLE with no counterparty round trip: the
// operator already holds the original plaintext in inventory (compression
// assigns only fire for plaintext entries — skipCompressionForV2, engine_buy.go),
// and the completion carries the compressed bytes + evidence. A completion that
// fails ANY gate is REJECTED (assign-reject → AssignOpen so another agent may
// retry) and NEVER paid; only an all-pass completion is accepted+paid via the
// reused Engine.acceptAssignLocked → handleAssignAccept → ClaimAssignPayment →
// ScripStore.AddBudget leg (no wire-schema change).

import (
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/3dl-dev/dontguess/pkg/matching"
)

// Compression-assign validation thresholds (all operator-computable).
const (
	// CompressionMaxSizeRatio is GATE1: the compressed size must be at most this
	// fraction of the original size — a >= 30% reduction. size_compressed /
	// size_original <= 0.70.
	CompressionMaxSizeRatio = 0.70

	// CompressionMinSimilarity is GATE2: the cosine similarity between the
	// embedded ORIGINAL content and the embedded compressed content must be at
	// least this — the compression must preserve semantic meaning, not just
	// shrink bytes.
	CompressionMinSimilarity = 0.85
)

// Assign-reject reason strings. These ride in the operator-authored
// assign-reject payload (observability + wire-visible cause) and the operator
// log line. applyAssignReject itself does not read them — it always resets the
// assign to AssignOpen — so they are purely diagnostic, but distinct per gate so
// an operator can tell WHY a completion was refused.
const (
	assignRejectNotAdmitted           = "not_admitted"           // (a) claimant not on the fleet allowlist (491f)
	assignRejectIntegrity             = "integrity"              // (b) evidence hash/size does not match the submitted bytes
	assignRejectInsufficientReduction = "insufficient_reduction" // (c) GATE1 < 30% size reduction
	assignRejectLowSimilarity         = "low_similarity"         // (d) GATE2 cosine < 0.85
	assignRejectOriginalUnavailable   = "original_unavailable"   // original plaintext not inline in inventory — cannot validate GATE2, fail-closed
)

// assignCompressResult is the assign-complete result payload shape a compression
// worker submits (relayclient.BuildAssignResult): the sha256:-prefixed hash over
// the compressed bytes, the compressed byte size, and the base64 compressed
// bytes themselves. It is the SAME shape createCompressionDerivative parses for
// content_hash/content_size (engine_core.go) — this validator additionally reads
// the `content` field so every gate is checkable WITHOUT trusting the worker's
// self-reported hash/size (the bytes are re-hashed and re-measured here). No
// wire-schema change: these fields already exist on the assign-complete message.
type assignCompressResult struct {
	ContentHash string `json:"content_hash"`
	ContentSize int64  `json:"content_size"`
	Content     string `json:"content"` // base64 compressed bytes
}

// RunAutoAcceptAssigns is the operator-side auto-accept-assign ticker body
// (dontguess-462, e51). On each tick it scans State for completed COMPRESSION
// assigns that have not yet been accepted or rejected, validates each one with
// operator-only computation (allowlist + integrity + GATE1 size reduction +
// GATE2 semantic similarity), and — for every completion that passes ALL gates —
// accepts it and pays the bounty to the claimant via the reused
// acceptAssignLocked / handleAssignAccept / ClaimAssignPayment / AddBudget leg.
// A completion that fails ANY gate is rejected (assign-reject → AssignOpen) and
// NEVER paid.
//
// CONCURRENCY. The whole per-record validate-then-accept/reject is performed
// under a SINGLE e.opMu hold — the SAME operator-broadcast serialization lock
// RunAutoAccept (put promotion), autoAcceptPutLocked, and PostOpenCompressionAssign
// take — so the accept/reject emit (an operator broadcast) cannot race the
// medium-loop cold-assign poster, the auto-accept-put poster, or a warm-assign
// poster. Lock ordering is the documented one: opMu (held) ⊃ compressAssignMu
// (untaken here) ⊃ localMu (taken by the emit) ⊃ State.mu. The snapshot is read
// BEFORE opMu (CompletedUnacceptedAssigns takes only State.mu), then re-validated
// under opMu; ClaimAssignPayment's AssignAccepted → AssignPaid transition is the
// idempotency backstop, so a second tick that observes the same completion (e.g.
// before its accept has folded) cannot double-pay.
//
// Only "compress" assigns are validated here — they are the one assign task_type
// with an operator-computable acceptance test. Other task types (validation,
// freshness, brokered-match) have no auto-validator and are left for their own
// paths / manual operator accept; the ticker skips them entirely.
func (e *Engine) RunAutoAcceptAssigns() {
	if e.opts.LocalStore == nil {
		return // no egress path to emit accept/reject broadcasts (state-only test engine)
	}
	completed := e.state.CompletedUnacceptedAssigns()
	if len(completed) == 0 {
		return
	}

	e.opMu.Lock()
	defer e.opMu.Unlock()
	for _, rec := range completed {
		if rec.TaskType != "compress" {
			continue // no operator-computable validator for other task types
		}
		if reason, ok := e.validateCompletedCompressionAssign(rec); !ok {
			if err := e.rejectCompletedAssignLocked(rec.CompleteMsgID, reason); err != nil {
				e.opts.log("engine: auto-accept-assign: reject emit failed assign=%s reason=%s: %v",
					shortKey(rec.AssignID), reason, err)
			} else {
				e.opts.log("engine: auto-accept-assign: REJECTED assign=%s claimant=%s reason=%s (not paid)",
					shortKey(rec.AssignID), shortKey(rec.ClaimantKey), reason)
			}
			continue
		}
		if err := e.acceptAssignLocked(rec.CompleteMsgID); err != nil {
			e.opts.log("engine: auto-accept-assign: accept+pay failed assign=%s claimant=%s: %v",
				shortKey(rec.AssignID), shortKey(rec.ClaimantKey), err)
			continue
		}
		e.opts.log("engine: auto-accept-assign: ACCEPTED+PAID assign=%s claimant=%s reward=%d",
			shortKey(rec.AssignID), shortKey(rec.ClaimantKey), rec.Reward)
	}
}

// validateCompletedCompressionAssign runs every operator-computable acceptance
// gate for a completed compression assign, in cheapest-first order. It returns
// ("", true) when ALL gates pass (accept+pay), or (reason, false) on the FIRST
// gate that fails (reject, no pay). No gate consults the counterparty: (a) reads
// the live allowlist, (b) re-hashes/re-measures the submitted bytes, (c) compares
// against the original entry's stored size, (d) embeds the original plaintext
// (held in inventory) and the submitted compressed bytes with the engine
// Embedder and computes cosine.
//
// CALLER MUST HOLD e.opMu (via RunAutoAcceptAssigns). Reads of State/allowlist/
// embedder are internally synchronized and take no locks that invert the opMu
// ordering.
func (e *Engine) validateCompletedCompressionAssign(rec *AssignRecord) (string, bool) {
	// (a) ALLOWLIST (491f, HARD). Re-check fleet membership at PAY time — the fold
	// admitted this claim/complete without a trust filter, so a never-admitted or
	// since-revoked key must be blocked HERE, the last line before scrip moves.
	// Skipped only when no TrustChecker is configured (individual/no-relay tier,
	// which has no completed assigns to reach this path anyway).
	if e.opts.TrustChecker != nil {
		if err := e.opts.TrustChecker.Check(rec.ClaimantKey, OperationAssignClaim, ""); err != nil {
			return assignRejectNotAdmitted, false
		}
	}

	// Parse the completion evidence. A malformed result is an integrity failure.
	var result assignCompressResult
	if err := json.Unmarshal(rec.Result, &result); err != nil {
		return assignRejectIntegrity, false
	}

	// (b) INTEGRITY. Re-derive the truth from the submitted bytes; never trust the
	// worker's self-reported hash/size.
	decoded, err := base64.StdEncoding.DecodeString(result.Content)
	if err != nil {
		return assignRejectIntegrity, false
	}
	if int64(len(decoded)) != result.ContentSize {
		return assignRejectIntegrity, false
	}
	if result.ContentHash != sha256Ref(decoded) {
		return assignRejectIntegrity, false
	}

	// Look up the ORIGINAL entry — the operator holds its plaintext because
	// compression assigns only fire for plaintext (non-v2) entries.
	orig := e.state.GetInventoryEntry(rec.EntryID)
	if orig == nil {
		return assignRejectOriginalUnavailable, false
	}

	// (c) GATE1 — size reduction >= 30%: size_compressed / size_original <= 0.70.
	// Guard size_original > 0 so a zero/negative original size can never divide-by-
	// zero or vacuously pass.
	if orig.ContentSize <= 0 {
		return assignRejectOriginalUnavailable, false
	}
	if float64(result.ContentSize) > CompressionMaxSizeRatio*float64(orig.ContentSize) {
		return assignRejectInsufficientReduction, false
	}

	// (d) GATE2 — semantic similarity >= 0.85. Embed the original plaintext and the
	// submitted compressed bytes with the engine Embedder and require cosine >=
	// 0.85. The original plaintext must be inline in inventory (orig.Content): an
	// offloaded (>32 KiB Blossom) entry keeps only a preview slice inline, which
	// cannot be embedded as the full original — fail-closed rather than validate
	// against a partial or PAY unvalidated.
	if len(orig.Content) == 0 {
		return assignRejectOriginalUnavailable, false
	}
	emb := e.validationEmbedder()
	origVec := emb.Embed(string(orig.Content))
	compVec := emb.Embed(string(decoded))
	if emb.Similarity(origVec, compVec) < CompressionMinSimilarity {
		return assignRejectLowSimilarity, false
	}

	return "", true
}

// validationEmbedder returns the Embedder used for GATE2. It is the engine's
// configured Embedder (EngineOptions.Embedder — native MiniLM when a dense model
// is cached) when present. When nil, it falls back to a fresh TF-IDF embedder —
// the SAME default the matching Index uses when no dense model is configured
// (matching.NewIndex). This is a documented SAFE DEFAULT: the similarity gate
// still RUNS (a legitimate compression preserves the salient shared vocabulary,
// so a TF-IDF cosine remains a real, if coarser, semantic check) — it NEVER
// silently pays an unvalidated completion. Operators wanting the sharper dense
// gate cache the MiniLM model (`dontguess embed pull`).
func (e *Engine) validationEmbedder() matching.Embedder {
	if e.opts.Embedder != nil {
		return e.opts.Embedder
	}
	return matching.NewTFIDFEmbedder()
}

// rejectCompletedAssignLocked emits an operator-authored exchange:assign-reject
// for the given assign-complete message, folds it, and returns. applyAssignReject
// removes the record from pendingAssignResults and resets it to AssignOpen so a
// DIFFERENT agent may reclaim and retry the task — no scrip is paid (the bounty
// was never held). reason rides in the payload for wire-visible observability;
// the fold does not read it.
//
// CALLER MUST HOLD e.opMu (mirrors acceptAssignLocked). The emit takes localMu
// via appendLocalRecord — opMu ⊃ localMu, the documented order.
func (e *Engine) rejectCompletedAssignLocked(completeMsgID, reason string) error {
	if completeMsgID == "" {
		return fmt.Errorf("engine: rejectCompletedAssign: empty completeMsgID")
	}
	payload, err := e.marshal(map[string]string{"reason": reason})
	if err != nil {
		return fmt.Errorf("engine: rejectCompletedAssign: marshal reason: %w", err)
	}
	msg, err := e.sendOperatorMessage(payload, []string{TagAssignReject}, []string{completeMsgID})
	if err != nil {
		return fmt.Errorf("engine: rejectCompletedAssign: emit: %w", err)
	}
	e.state.Apply(msg)
	return nil
}
