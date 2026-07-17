package exchange_test

// assign_autoaccept_test.go is the MANDATORY enforcement proof for
// dontguess-462: the operator-side AUTO-VALIDATE-AND-PAY surface for completed
// COMPRESSION assigns (Engine.RunAutoAcceptAssigns). It drives the REAL
// production ticker (RunAutoAcceptAssigns — the exact method serve's
// auto-accept-assign goroutine calls each tick, NOT a direct test-only
// AcceptAssign call) against a REAL exchange.Engine backed by a REAL
// scrip.LocalScripStore and a REAL TrustChecker, with the assign lifecycle
// folded through the SAME production State.Apply path a deployed operator runs.
//
// The four enforcement cases (item text):
//
//	(i)   a scrip-poor admitted agent claims + completes a VALID compression
//	      assign — the ticker PAYS it; the agent's scrip balance INCREASES.
//	(ii)  an INVALID completion is REJECTED and NOT paid — one case each for
//	      insufficient_reduction, low_similarity, and integrity (bad evidence hash).
//	(iii) a claimant NOT on the allowlist is REJECTED and NOT paid (not_admitted)
//	      — proves 491f (the pay path re-checks the allowlist) is closed.
//	(iv)  idempotency: running the ticker twice does not double-pay a single
//	      completion (ClaimAssignPayment's AssignAccepted → AssignPaid atomicity).
//
// The Embedder is a deterministic marker stub so GATE2's cosine threshold is
// tested at an exact, reproducible boundary (1.0 for a same-marker "compression",
// 0.0 for a different-marker one) — the gate LOGIC is under test here, not
// MiniLM's numeric output. Everything else (engine, ScripStore, TrustChecker,
// fold, ticker) is real.

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/3dl-dev/dontguess/pkg/exchange"
	"github.com/3dl-dev/dontguess/pkg/scrip"
)

// markerEmbedder is a deterministic 2-D embedder keyed on a marker substring:
// "VEC_A" -> [1,0], "VEC_B" -> [0,1]. Cosine of two "VEC_A" texts is exactly 1.0
// (>= 0.85 -> GATE2 pass); cosine of a "VEC_A" original vs a "VEC_B" compression
// is exactly 0.0 (< 0.85 -> GATE2 fail). This makes the similarity gate boundary
// reproducible without depending on TF-IDF/MiniLM numerics.
type markerEmbedder struct{}

func (markerEmbedder) Embed(text string) []float64 {
	switch {
	case strings.Contains(text, "VEC_A"):
		return []float64{1, 0}
	case strings.Contains(text, "VEC_B"):
		return []float64{0, 1}
	default:
		return []float64{1, 1}
	}
}

func (markerEmbedder) Similarity(a, b []float64) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// sha256Ref mirrors the exchange's canonical content-address form used by the
// integrity gate ("sha256:"+hex(sha256(b))).
func sha256Ref(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

const (
	autoAcceptTokenCost = int64(20000)
	// wantAutoAcceptReward is 20% of token_cost (ColdCompressionBountyPct).
	wantAutoAcceptReward = autoAcceptTokenCost * exchange.ColdCompressionBountyPct / 100
)

// compressFixture bundles a real team-tier engine with one plaintext inventory
// entry and one posted cold compression assign, ready for a claim/complete/tick.
type compressFixture struct {
	h           *testHarness
	eng         *exchange.Engine
	cs          *scrip.LocalScripStore
	agent       *testAgent
	entryID     string
	assignID    string
	origContent string
}

// padTo returns marker followed by filler so the whole string is exactly n bytes.
func padTo(marker string, n int) string {
	if len(marker) >= n {
		return marker[:n]
	}
	return marker + strings.Repeat("z", n-len(marker))
}

// newCompressFixture builds a real engine (LocalStore + ScripStore +
// TrustChecker + marker Embedder), folds a plaintext put + put-accept to create
// one inventory entry with a known VEC_A original content of exactly origBytes,
// then posts a cold compression assign for it. allowAgent controls whether the
// claiming agent is on the fleet allowlist (false exercises the 491f not_admitted
// gate). The put/put-accept are folded directly (not via AutoAcceptPut) so the
// automatic hot-compression assign AutoAcceptPut fires does not pre-empt the cold
// assign under test — the same technique the ffb/d26 fixtures use.
func newCompressFixture(t *testing.T, allowAgent bool, origBytes int) *compressFixture {
	t.Helper()
	h := newTestHarness(t)
	agent := newTestAgent(t)

	cs, err := scrip.NewLocalScripStore(h.st, h.operator.PublicKeyHex())
	if err != nil {
		t.Fatalf("NewLocalScripStore: %v", err)
	}
	if err := cs.Replay(); err != nil {
		t.Fatalf("cs.Replay: %v", err)
	}

	keys := []string{h.seller.pubKeyHex}
	if allowAgent {
		keys = append(keys, agent.pubKeyHex)
	}
	ks := exchange.NewKeySet(keys...)
	tc, err := exchange.NewTrustChecker(h.operator.pubKeyHex, ks)
	if err != nil {
		t.Fatalf("NewTrustChecker: %v", err)
	}

	eng := exchange.NewEngine(exchange.EngineOptions{
		CampfireID:        h.cfID,
		LocalStore:        h.st,
		OperatorPublicKey: h.operator.pubKeyHex,
		// OperatorSigner deliberately omitted: it arms encryptedRequired, which
		// would fail-closed DROP the plaintext fixture put (§541 §6). This engine
		// never handles a v2 put, so plaintext-local is the correct minimal config
		// (mirrors assign_e2e_d26_test.go).
		ScripStore:   cs,
		TrustChecker: tc,
		Embedder:     markerEmbedder{},
		Logger:       func(string, ...any) {},
	})

	origContent := padTo("VEC_A original plaintext ", origBytes)
	putMsg := h.sendMessage(h.seller,
		compressPutPayload("auto-accept-assign fixture: compressible doc", origContent, autoAcceptTokenCost),
		[]string{exchange.TagPut, "exchange:content-type:code", "exchange:domain:go"},
		nil,
	)
	acceptPayload, _ := json.Marshal(map[string]any{
		"phase":      "put-accept",
		"entry_id":   putMsg.ID,
		"price":      autoAcceptTokenCost * 70 / 100,
		"expires_at": time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339),
	})
	h.sendMessage(h.operator, acceptPayload,
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrPutAccept,
			exchange.TagVerdictPrefix + "accepted",
		},
		[]string{putMsg.ID},
	)
	replayAll(t, h, eng)

	inv := eng.State().Inventory()
	if len(inv) != 1 {
		t.Fatalf("inventory after put-accept = %d, want 1", len(inv))
	}
	entryID := inv[0].EntryID
	if int(inv[0].ContentSize) != origBytes {
		t.Fatalf("entry ContentSize = %d, want %d", inv[0].ContentSize, origBytes)
	}
	if len(inv[0].Content) == 0 {
		t.Fatalf("entry Content not stored inline — GATE2 cannot embed the original")
	}

	if err := eng.PostOpenCompressionAssign(entryID); err != nil {
		t.Fatalf("PostOpenCompressionAssign: %v", err)
	}
	replayAll(t, h, eng)

	assignID := ""
	for _, a := range eng.State().AllActiveAssigns() {
		if a.TaskType == "compress" && a.EntryID == entryID {
			assignID = a.AssignID
			if a.Reward != wantAutoAcceptReward {
				t.Fatalf("posted assign reward = %d, want %d", a.Reward, wantAutoAcceptReward)
			}
			break
		}
	}
	if assignID == "" {
		t.Fatalf("no cold compression assign posted for entry %s", entryID)
	}

	return &compressFixture{h: h, eng: eng, cs: cs, agent: agent, entryID: entryID, assignID: assignID, origContent: origContent}
}

// claimAndComplete folds a real assign-claim then assign-complete from the agent
// (through the production applyAssignClaim/applyAssignComplete fold via Replay —
// the same transitions a relay-folded claim/complete drive) and returns the
// resulting AssignRecord's state after the completion.
func (f *compressFixture) claimAndComplete(t *testing.T, result []byte) {
	t.Helper()
	f.h.sendMessage(f.agent, []byte(`{}`), []string{exchange.TagAssignClaim}, []string{f.assignID})
	replayAll(t, f.h, f.eng)

	claimMsgID := ""
	for _, a := range f.eng.State().AllActiveAssigns() {
		if a.AssignID == f.assignID {
			if a.Status != exchange.AssignClaimed || a.ClaimantKey != f.agent.pubKeyHex {
				t.Fatalf("assign after claim: status=%v claimant=%s, want claimed by agent", a.Status, a.ClaimantKey)
			}
			claimMsgID = a.ClaimMsgID
			break
		}
	}
	if claimMsgID == "" {
		t.Fatalf("claim did not fold (assign %s not claimed)", f.assignID)
	}

	f.h.sendMessage(f.agent, result, []string{exchange.TagAssignComplete}, []string{claimMsgID})
	replayAll(t, f.h, f.eng)

	// Confirm the completion folded into the pending-review queue.
	found := false
	for _, a := range f.eng.State().CompletedUnacceptedAssigns() {
		if a.AssignID == f.assignID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("completion did not fold into CompletedUnacceptedAssigns for assign %s", f.assignID)
	}
}

func (f *compressFixture) balance() int64 { return f.cs.Balance(f.agent.PublicKeyHex()) }

func compressPutPayload(desc, content string, tokenCost int64) []byte {
	p, _ := json.Marshal(map[string]any{
		"description":  desc,
		"content":      base64.StdEncoding.EncodeToString([]byte(content)),
		"token_cost":   tokenCost,
		"content_type": "code",
		"domains":      []string{"go"},
	})
	return p
}

// compressResult builds the assign-complete result payload the way a real worker
// (relayclient.BuildAssignResult) does: correct sha256 hash + size + base64 bytes.
func compressResult(content []byte) []byte {
	p, _ := json.Marshal(map[string]any{
		"content_hash": sha256Ref(content),
		"content_size": int64(len(content)),
		"content":      base64.StdEncoding.EncodeToString(content),
	})
	return p
}

// === (i) VALID completion is paid ==========================================

func TestAutoAcceptAssign_ValidCompletion_Pays(t *testing.T) {
	t.Parallel()
	f := newCompressFixture(t, true, 400)
	if f.balance() != 0 {
		t.Fatalf("agent starts with %d scrip, want 0 (scrip-poor)", f.balance())
	}
	// A valid compression: same VEC_A marker (cosine 1.0), 200 bytes (50% of 400,
	// >= 30% reduction), correct evidence.
	good := []byte(padTo("VEC_A compressed ", 200))
	f.claimAndComplete(t, compressResult(good))

	f.eng.RunAutoAcceptAssigns()

	if got := f.balance(); got != wantAutoAcceptReward {
		t.Fatalf("agent balance after auto-accept = %d, want %d (paid once)", got, wantAutoAcceptReward)
	}
	// The assign reached terminal AssignPaid.
	if st := assignStatus(f, f.assignID); st != exchange.AssignPaid {
		t.Fatalf("assign status = %v, want AssignPaid", st)
	}
}

// === (ii) INVALID completions are rejected, not paid =======================

func TestAutoAcceptAssign_InsufficientReduction_NotPaid(t *testing.T) {
	t.Parallel()
	f := newCompressFixture(t, true, 400)
	// 350 bytes > 70% of 400 (=280): fails GATE1. Same VEC_A marker so ONLY the
	// size gate is the cause.
	tooBig := []byte(padTo("VEC_A barely compressed ", 350))
	f.claimAndComplete(t, compressResult(tooBig))

	f.eng.RunAutoAcceptAssigns()

	if got := f.balance(); got != 0 {
		t.Fatalf("agent balance = %d, want 0 (insufficient_reduction must not pay)", got)
	}
	assertNotPaidReopened(t, f)
}

func TestAutoAcceptAssign_LowSimilarity_NotPaid(t *testing.T) {
	t.Parallel()
	f := newCompressFixture(t, true, 400)
	// VEC_B marker -> cosine 0.0 < 0.85: fails GATE2. Size 200 (passes GATE1) and
	// evidence correct, so ONLY the similarity gate is the cause.
	unrelated := []byte(padTo("VEC_B totally different ", 200))
	f.claimAndComplete(t, compressResult(unrelated))

	f.eng.RunAutoAcceptAssigns()

	if got := f.balance(); got != 0 {
		t.Fatalf("agent balance = %d, want 0 (low_similarity must not pay)", got)
	}
	assertNotPaidReopened(t, f)
}

func TestAutoAcceptAssign_BadEvidenceHash_NotPaid(t *testing.T) {
	t.Parallel()
	f := newCompressFixture(t, true, 400)
	good := []byte(padTo("VEC_A compressed ", 200))
	// Correct size + content, but a WRONG content_hash: fails the integrity gate.
	bad, _ := json.Marshal(map[string]any{
		"content_hash": sha256Ref([]byte("some other bytes entirely")),
		"content_size": int64(len(good)),
		"content":      base64.StdEncoding.EncodeToString(good),
	})
	f.claimAndComplete(t, bad)

	f.eng.RunAutoAcceptAssigns()

	if got := f.balance(); got != 0 {
		t.Fatalf("agent balance = %d, want 0 (integrity failure must not pay)", got)
	}
	assertNotPaidReopened(t, f)
}

// === (iii) not-allowlisted claimant is rejected (491f) =====================

func TestAutoAcceptAssign_NotAllowlisted_NotPaid(t *testing.T) {
	t.Parallel()
	// allowAgent=false: the claiming agent is NOT on the fleet allowlist. The
	// completion is otherwise fully valid (would pass all other gates), so a
	// payment here would prove the pay path skipped the allowlist (the 491f bug).
	f := newCompressFixture(t, false, 400)
	good := []byte(padTo("VEC_A compressed ", 200))
	f.claimAndComplete(t, compressResult(good))

	f.eng.RunAutoAcceptAssigns()

	if got := f.balance(); got != 0 {
		t.Fatalf("agent balance = %d, want 0 (not_admitted claimant must not be paid — 491f)", got)
	}
	assertNotPaidReopened(t, f)
}

// === (iv) idempotency: two ticks do not double-pay =========================

func TestAutoAcceptAssign_Idempotent_NoDoublePay(t *testing.T) {
	t.Parallel()
	f := newCompressFixture(t, true, 400)
	good := []byte(padTo("VEC_A compressed ", 200))
	f.claimAndComplete(t, compressResult(good))

	f.eng.RunAutoAcceptAssigns()
	first := f.balance()
	if first != wantAutoAcceptReward {
		t.Fatalf("balance after first tick = %d, want %d", first, wantAutoAcceptReward)
	}
	// Second tick must be a no-op: the assign is already AssignPaid and out of the
	// pending-review queue, so ClaimAssignPayment can never transition it again.
	f.eng.RunAutoAcceptAssigns()
	if got := f.balance(); got != wantAutoAcceptReward {
		t.Fatalf("balance after second tick = %d, want %d (must not double-pay)", got, wantAutoAcceptReward)
	}
}

// assertNotPaidReopened asserts a rejected completion returned the assign to
// AssignOpen (claimant cleared) — the applyAssignReject retry semantics — and
// that a SECOND tick still does not pay it (the reject is stable).
func assertNotPaidReopened(t *testing.T, f *compressFixture) {
	t.Helper()
	st := assignStatus(f, f.assignID)
	if st != exchange.AssignOpen {
		t.Fatalf("rejected assign status = %v, want AssignOpen (reopened for retry)", st)
	}
	f.eng.RunAutoAcceptAssigns()
	if got := f.balance(); got != 0 {
		t.Fatalf("agent balance after a second tick = %d, want 0 (reject is stable)", got)
	}
}

func assignStatus(f *compressFixture, assignID string) exchange.AssignStatus {
	for id, rec := range f.eng.State().AssignByIDForTest() {
		if id == assignID {
			return rec.Status
		}
	}
	return exchange.AssignStatus(-1)
}
