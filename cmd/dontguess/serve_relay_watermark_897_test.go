package main

// serve_relay_watermark_897_test.go — dontguess-897: harden the climb-watermark
// against sidecar loss on an ALREADY-fleet operator, and directly exercise
// establishClimbWatermark's write/reload idempotency path.
//
// Two gaps from dontguess-e18d (the climb egress fence), both in the
// sidecar-ABSENT branch of establishClimbWatermark:
//
//  (1) ROBUSTNESS BUG (TestClimbWatermark_SidecarLossRecompute_*): if the
//      climb-egress.watermark sidecar is lost/corrupted after the climb, the old
//      recompute set the watermark to the CURRENT operator-record total. That total
//      has drifted UPWARD as post-climb v2-ciphertext inventory accumulated, so a
//      SECOND relay added afterward seeded its fresh cursor to the inflated count
//      and never backfilled the encrypted inventory — a replication gap on the new
//      relay (no plaintext leak; every over-fenced record is ciphertext). The fix
//      RECONSTRUCTS the true climb point (position past the last inline-plaintext
//      content record), which is stable under post-climb growth.
//
//  (2) TEST-COVERAGE (TestEstablishClimbWatermark_WriteReloadIdempotent): the
//      sidecar write→reload idempotency (once written, NEVER recomputed even as the
//      store grows) and the temp-file crash-partial path were only exercised inline
//      by a Wave-3 test. These drive establishClimbWatermark's own file path.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/3dl-dev/dontguess/pkg/exchange"
	"github.com/3dl-dev/dontguess/pkg/identity"
	"github.com/3dl-dev/dontguess/pkg/nostr"
	dgstore "github.com/3dl-dev/dontguess/pkg/store"
)

// v2PutPayload builds a §3.3 v2 confidential put payload: v>=2 with an "enc"
// envelope and NO plaintext "content" field. This is exactly the shape a post-climb
// (team-tier) put takes — the content travels only as AEAD ciphertext — so
// recordCarriesInlinePlaintextContent must classify it as safe-to-republish.
func v2PutPayload(desc string, tokenCost int64) []byte {
	// A realistic-but-minimal enc envelope (state_put.go encEnvelope). The fence
	// reconstruction only needs v>=2 + a non-null enc object + absent "content".
	fakeCipher := base64.StdEncoding.EncodeToString([]byte("nonce||AEAD-ciphertext for " + desc))
	p, _ := json.Marshal(map[string]any{
		"v":            2,
		"description":  desc,
		"token_cost":   tokenCost,
		"content_type": "exchange:content-type:code",
		"domains":      []string{"go"},
		"enc": map[string]any{
			"content_alg":     "nip44-v2",
			"ciphertext_hash": "sha256:" + strings.Repeat("00", 32),
			"ciphertext":      fakeCipher,
			"key_wrap": map[string]any{
				"alg":     "nip44-v2-secp256k1",
				"wrapped": base64.StdEncoding.EncodeToString([]byte("wrapped-CEK")),
			},
		},
	})
	return p
}

func appendPut(t *testing.T, ls *dgstore.Store, operator identity.Signer, payload []byte, ts int64) {
	t.Helper()
	rec := dgstore.Record{
		ID:         randomLocalMsgID(t),
		CampfireID: "local",
		Sender:     operator.PubKeyHex(),
		Payload:    payload,
		Tags:       []string{exchange.TagPut, "exchange:content-type:code"},
		Timestamp:  ts,
		Origin:     "local",
	}
	if err := ls.Append(rec); err != nil {
		t.Fatalf("append put: %v", err)
	}
}

// TestClimbWatermark_SidecarLossRecompute_BackfillsEncryptedInventoryToNewRelay
// reproduces gap (1) end-to-end against the REAL serve-path pieces
// (establishClimbWatermark + buildRelayWiring → relay.Outbox), driving publish
// deterministically with a recordingPublisher standing in for the relay wire.
func TestClimbWatermark_SidecarLossRecompute_BackfillsEncryptedInventoryToNewRelay(t *testing.T) {
	dgHome := t.TempDir()
	storePath := filepath.Join(dgHome, "events.jsonl")
	operator, err := identity.Generate()
	if err != nil {
		t.Fatalf("operator identity: %v", err)
	}

	const secret = "SECRET-897-PRECLIMB-PLAINTEXT-CORPUS"
	const nPlain = 3 // pre-climb solo plaintext puts (cleartext "content", §541 §6)
	const mEnc = 4   // post-climb team v2-ciphertext puts (enc, no "content")

	ls, err := dgstore.Open(storePath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = ls.Close() })

	// ── SOLO: N pre-climb PLAINTEXT puts. The secret rides in the cleartext
	//    payload, so any published event carrying it is a provable leak.
	for i := 0; i < nPlain; i++ {
		appendPut(t, ls, operator, localPutPayload(secret+" pre-climb plaintext variant", 8000+int64(i)), time.Now().UnixNano()+int64(i))
	}

	ctx := context.Background()
	wmPath := climbWatermarkPath(dgHome)

	// ── CLIMB: establish the watermark. On a pure-plaintext-put corpus the true
	//    climb point equals the operator-record count (nPlain).
	w1, err := establishClimbWatermark(wmPath, ls)
	if err != nil {
		t.Fatalf("establishClimbWatermark (climb): %v", err)
	}
	if w1 != nPlain {
		t.Fatalf("climb watermark = %d, want %d (the pre-climb plaintext corpus size)", w1, nPlain)
	}

	// Relay A attaches at the climb: fresh cursor seeded to w1, so a full publish
	// pass emits NOTHING (the whole pre-climb corpus is fenced local-only).
	pubA := &recordingPublisher{}
	legA, _, err := buildRelayWiring(ls, operator, operator.PubKeyHex(),
		filepath.Join(dgHome, "relayA.pubcursor"), pubA, 0, nil, nil,
		WithClimbWatermark(w1))
	if err != nil {
		t.Fatalf("buildRelayWiring (relay A): %v", err)
	}
	if got := legA.outbox.Cursor(); got != w1 {
		t.Fatalf("relay A cursor = %d, want the watermark %d", got, w1)
	}
	if err := legA.outbox.Tick(ctx); err != nil {
		t.Fatalf("relay A Tick (climb): %v", err)
	}
	if got := len(pubA.snapshot()); got != 0 {
		t.Fatalf("CLIMB FENCE BREACH: relay A republished %d pre-climb event(s), want 0", got)
	}

	// ── POST-CLIMB (team tier): M v2-encrypted puts append ABOVE the watermark.
	for i := 0; i < mEnc; i++ {
		appendPut(t, ls, operator, v2PutPayload("post-climb encrypted variant", 9000+int64(i)), time.Now().UnixNano()+int64(100+i))
	}
	// Relay A backfills the encrypted inventory (it publishes normally above the fence).
	if err := legA.outbox.Tick(ctx); err != nil {
		t.Fatalf("relay A Tick (post-climb): %v", err)
	}
	if got := len(pubA.snapshot()); got != mEnc {
		t.Fatalf("relay A: republished %d post-climb event(s), want %d (encrypted inventory must reach the fleet)", got, mEnc)
	}

	// ── SIDECAR LOSS on an already-fleet operator.
	if err := os.Remove(wmPath); err != nil {
		t.Fatalf("remove watermark sidecar: %v", err)
	}

	// ── RECOMPUTE. THE FIX: reconstruct the TRUE climb point (nPlain), NOT the
	//    inflated current operator-record total (nPlain+mEnc).
	w2, err := establishClimbWatermark(wmPath, ls)
	if err != nil {
		t.Fatalf("establishClimbWatermark (recompute after loss): %v", err)
	}
	if w2 != nPlain {
		t.Fatalf("recompute-after-loss watermark = %d, want %d (the TRUE climb point) — over-fencing regressed", w2, nPlain)
	}

	// Load-bearing anchor: the OLD code's count-all value is nPlain+mEnc. Prove the
	// store actually holds that many operator records, so the assertion above is a
	// real fix (w2 != countAll), not a vacuous pass.
	recs, err := ls.ReadAll()
	if err != nil {
		t.Fatalf("read store: %v", err)
	}
	var countAll int64
	for i := range recs {
		if isOperatorOrigin(recs[i].Origin) {
			countAll++
		}
	}
	if countAll != nPlain+mEnc {
		t.Fatalf("operator-record count = %d, want %d", countAll, nPlain+mEnc)
	}
	if w2 == countAll {
		t.Fatalf("recompute produced the inflated count-all watermark %d — the over-fence bug is NOT fixed", w2)
	}

	// ── NEW RELAY B added after the loss: fresh cursor seeded to the reconstructed
	//    watermark. It MUST backfill the M encrypted puts and leak ZERO plaintext.
	pubB := &recordingPublisher{}
	legB, _, err := buildRelayWiring(ls, operator, operator.PubKeyHex(),
		filepath.Join(dgHome, "relayB.pubcursor"), pubB, 0, nil, nil,
		WithClimbWatermark(w2))
	if err != nil {
		t.Fatalf("buildRelayWiring (relay B): %v", err)
	}
	if got := legB.outbox.Cursor(); got != nPlain {
		t.Fatalf("relay B cursor = %d, want the reconstructed watermark %d", got, nPlain)
	}
	if err := legB.outbox.Tick(ctx); err != nil {
		t.Fatalf("relay B Tick: %v", err)
	}
	snapB := pubB.snapshot()
	if len(snapB) != mEnc {
		t.Fatalf("REPLICATION GAP: relay B backfilled %d encrypted put(s), want %d — the newly-added relay must receive the post-climb inventory", len(snapB), mEnc)
	}
	for _, ev := range snapB {
		if ev.Kind != nostr.KindPut {
			t.Fatalf("relay B published a non-put event (kind %d) — expected only the %d encrypted puts", ev.Kind, mEnc)
		}
		if strings.Contains(ev.Content, secret) {
			t.Fatalf("CONFIDENTIALITY LEAK: pre-climb plaintext (%q) appeared on the newly-added relay B", secret)
		}
	}

	// ── LOAD-BEARING TWIN: a relay seeded to the OLD (buggy) inflated watermark
	//    (countAll) backfills NOTHING — that is exactly the replication gap the fix
	//    closes, proving relay B's backfill above is real and non-vacuous.
	pubBug := &recordingPublisher{}
	legBug, _, err := buildRelayWiring(ls, operator, operator.PubKeyHex(),
		filepath.Join(dgHome, "relayBug.pubcursor"), pubBug, 0, nil, nil,
		WithClimbWatermark(countAll))
	if err != nil {
		t.Fatalf("buildRelayWiring (buggy twin): %v", err)
	}
	if err := legBug.outbox.Tick(ctx); err != nil {
		t.Fatalf("buggy twin Tick: %v", err)
	}
	if got := len(pubBug.snapshot()); got != 0 {
		t.Fatalf("buggy-twin sanity: seeded to the inflated watermark it published %d event(s), want 0 (the over-fence must strand the whole corpus)", got)
	}
}

// TestClimbWatermark_ReconstructTrueClimbPoint_MixedInterleavedLog is a unit-level
// guard on the reconstruction: with post-climb encrypted puts AND non-content
// metadata interleaved after the pre-climb plaintext puts, the recomputed watermark
// still lands exactly past the last plaintext-content record — never inflated by the
// trailing encrypted/metadata records.
func TestClimbWatermark_ReconstructTrueClimbPoint_MixedInterleavedLog(t *testing.T) {
	dgHome := t.TempDir()
	storePath := filepath.Join(dgHome, "events.jsonl")
	operator, err := identity.Generate()
	if err != nil {
		t.Fatalf("operator identity: %v", err)
	}
	ls, err := dgstore.Open(storePath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = ls.Close() })

	// Two pre-climb plaintext puts (local positions 1,2), then post-climb records:
	// a v2 put, a metadata match, another v2 put (positions 3,4,5). The last
	// plaintext-content record is at local position 2 ⇒ watermark 2.
	appendPut(t, ls, operator, localPutPayload("plaintext one", 8000), 1)
	appendPut(t, ls, operator, localPutPayload("plaintext two", 8001), 2)
	appendPut(t, ls, operator, v2PutPayload("encrypted three", 9000), 3)
	if err := ls.Append(dgstore.Record{
		ID: randomLocalMsgID(t), CampfireID: "local", Sender: operator.PubKeyHex(),
		Payload: []byte(`{"buy_id":"b1","entry_id":"e1"}`),
		Tags:    []string{exchange.TagMatch}, Timestamp: 4, Origin: "local",
	}); err != nil {
		t.Fatalf("append match: %v", err)
	}
	appendPut(t, ls, operator, v2PutPayload("encrypted five", 9001), 5)

	w, err := establishClimbWatermark(climbWatermarkPath(dgHome), ls)
	if err != nil {
		t.Fatalf("establishClimbWatermark: %v", err)
	}
	if w != 2 {
		t.Fatalf("reconstructed watermark = %d, want 2 (past the last plaintext put; the 3 trailing encrypted/metadata records must NOT inflate it)", w)
	}
}

// TestEstablishClimbWatermark_WriteReloadIdempotent drives gap (2): the sidecar
// write→reload contract of establishClimbWatermark directly. Once the fence is
// written it is NEVER recomputed — a reload returns the persisted value verbatim
// even after the store grows — and the on-disk file is durable ("<n>\n") with no
// leftover temp files from the temp→fsync→rename crash-safe write.
func TestEstablishClimbWatermark_WriteReloadIdempotent(t *testing.T) {
	dgHome := t.TempDir()
	storePath := filepath.Join(dgHome, "events.jsonl")
	operator, err := identity.Generate()
	if err != nil {
		t.Fatalf("operator identity: %v", err)
	}
	ls, err := dgstore.Open(storePath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = ls.Close() })

	const nPlain = 2
	for i := 0; i < nPlain; i++ {
		appendPut(t, ls, operator, localPutPayload("plaintext put", 8000+int64(i)), int64(i+1))
	}

	wmPath := climbWatermarkPath(dgHome)

	// FIRST establish: computes + writes the sidecar.
	w1, err := establishClimbWatermark(wmPath, ls)
	if err != nil {
		t.Fatalf("first establish: %v", err)
	}
	if w1 != nPlain {
		t.Fatalf("first watermark = %d, want %d", w1, nPlain)
	}

	// The write is durable and normalized: exactly "<n>\n", 0600, and NO leftover
	// ".climb-egress-*.tmp" from the temp→fsync→rename path.
	body, err := os.ReadFile(wmPath)
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	if got := string(body); got != "2\n" {
		t.Fatalf("sidecar body = %q, want %q", got, "2\n")
	}
	entries, err := os.ReadDir(dgHome)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".climb-egress-") && strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("leftover temp file %q — the crash-safe write did not clean up", e.Name())
		}
	}

	// GROW THE STORE, then reload. IDEMPOTENCY: the persisted fence is reused
	// verbatim — it does NOT drift upward with the new inventory. This is the exact
	// property that keeps restarts (and the recompute-after-loss reconstruction)
	// from over-fencing.
	for i := 0; i < 5; i++ {
		appendPut(t, ls, operator, v2PutPayload("post-climb encrypted", 9000+int64(i)), int64(100+i))
	}
	w2, err := establishClimbWatermark(wmPath, ls)
	if err != nil {
		t.Fatalf("reload establish: %v", err)
	}
	if w2 != w1 {
		t.Fatalf("reload watermark = %d, want %d (idempotent — never recomputed once written)", w2, w1)
	}

	// A reload must NOT even consult the store (idempotent read path): passing a nil
	// store still returns the persisted value, proving no recompute happened.
	w3, err := establishClimbWatermark(wmPath, nil)
	if err != nil {
		t.Fatalf("reload with nil store: %v", err)
	}
	if w3 != w1 {
		t.Fatalf("nil-store reload watermark = %d, want %d — the reload must read the sidecar, not recompute", w3, w1)
	}
}

// TestEstablishClimbWatermark_CrashPartialTempIgnored proves the write path is
// resilient to a crash that left a partial temp file behind: a stray
// ".climb-egress-*.tmp" (a half-written, never-renamed attempt from a prior crash)
// does NOT corrupt the reload — establishClimbWatermark reads only the committed
// sidecar produced by the atomic rename.
func TestEstablishClimbWatermark_CrashPartialTempIgnored(t *testing.T) {
	dgHome := t.TempDir()
	storePath := filepath.Join(dgHome, "events.jsonl")
	operator, err := identity.Generate()
	if err != nil {
		t.Fatalf("operator identity: %v", err)
	}
	ls, err := dgstore.Open(storePath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = ls.Close() })
	appendPut(t, ls, operator, localPutPayload("plaintext put", 8000), 1)

	// Simulate a crash mid-write BEFORE the first successful establish: a stray temp
	// file with garbage, never renamed into place.
	stray := filepath.Join(dgHome, ".climb-egress-crash.tmp")
	if err := os.WriteFile(stray, []byte("999999\n"), 0o600); err != nil {
		t.Fatalf("write stray temp: %v", err)
	}

	wmPath := climbWatermarkPath(dgHome)
	w, err := establishClimbWatermark(wmPath, ls)
	if err != nil {
		t.Fatalf("establish with stray temp present: %v", err)
	}
	if w != 1 {
		t.Fatalf("watermark = %d, want 1 — a stray crash temp file must not be read as the fence", w)
	}
	// The committed sidecar reflects the real reconstruction, not the stray garbage.
	body, err := os.ReadFile(wmPath)
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	if got := string(body); got != "1\n" {
		t.Fatalf("committed sidecar = %q, want %q (the stray temp must be ignored)", got, "1\n")
	}
}
