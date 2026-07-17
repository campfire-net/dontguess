package main

// assign_autoaccept_serve_test.go is the serve-WIRING enforcement proof for
// dontguess-462: it proves runEngineLoop (the real inner serve loop that both
// serve entrypoints run) STARTS the auto-accept-assign ticker goroutine and that
// the ticker validates-and-PAYS a completed compression assign end to end
// through a REAL exchange.Engine + REAL scrip.LocalScripStore — no direct
// eng.AcceptAssign call, no mock. The gate LOGIC (all four validation gates +
// reject + idempotency) is proven exhaustively in
// pkg/exchange/assign_autoaccept_test.go against the same production
// RunAutoAcceptAssigns method; this test proves the production SERVE actually
// drives it on its interval.
//
// The Embedder is a deterministic marker stub so the one valid completion under
// test scores an exact 1.0 cosine (>= 0.85). Everything else is real.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/3dl-dev/dontguess/pkg/exchange"
	"github.com/3dl-dev/dontguess/pkg/identity"
	"github.com/3dl-dev/dontguess/pkg/matching"
	"github.com/3dl-dev/dontguess/pkg/pricing"
	"github.com/3dl-dev/dontguess/pkg/scrip"
	dgstore "github.com/3dl-dev/dontguess/pkg/store"
)

// serveMarkerEmbedder is a deterministic 2-D embedder: "VEC_A" -> [1,0] so a
// same-marker original/compression pair has cosine 1.0 (GATE2 pass). Distinct
// from the exchange_test package's markerEmbedder (different package).
type serveMarkerEmbedder struct{}

func (serveMarkerEmbedder) Embed(text string) []float64 {
	if strings.Contains(text, "VEC_A") {
		return []float64{1, 0}
	}
	return []float64{0, 1}
}

func (serveMarkerEmbedder) Similarity(a, b []float64) float64 {
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

var _ matching.Embedder = serveMarkerEmbedder{}

func TestServeWiring_AutoAcceptAssign_TickerPaysCompletedCompression(t *testing.T) {
	// NOT parallel: mutates the package-level serveAssignAcceptInterval.
	prevInterval := serveAssignAcceptInterval
	serveAssignAcceptInterval = 40 * time.Millisecond
	t.Cleanup(func() { serveAssignAcceptInterval = prevInterval })

	dgHome := t.TempDir()
	ls, err := dgstore.Open(dgHome + "/events.jsonl")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = ls.Close() })

	operator, err := identity.Generate()
	if err != nil {
		t.Fatalf("operator identity: %v", err)
	}
	agent, err := identity.Generate() // the scrip-poor fleet agent
	if err != nil {
		t.Fatalf("agent identity: %v", err)
	}
	seller, err := identity.Generate()
	if err != nil {
		t.Fatalf("seller identity: %v", err)
	}

	ks := exchange.NewKeySet(agent.PubKeyHex(), seller.PubKeyHex())
	tc, err := exchange.NewTrustChecker(operator.PubKeyHex(), ks)
	if err != nil {
		t.Fatalf("NewTrustChecker: %v", err)
	}
	ss, err := scrip.NewLocalScripStore(ls, operator.PubKeyHex())
	if err != nil {
		t.Fatalf("NewLocalScripStore: %v", err)
	}
	if bal := ss.Balance(agent.PubKeyHex()); bal != 0 {
		t.Fatalf("agent balance before test = %d, want 0 (scrip-poor)", bal)
	}

	ready := make(chan struct{})
	eng := exchange.NewEngine(exchange.EngineOptions{
		CampfireID:        "local",
		LocalStore:        ls,
		OperatorPublicKey: operator.PubKeyHex(),
		TrustChecker:      tc,
		ScripStore:        ss,
		Embedder:          serveMarkerEmbedder{},
		PollInterval:      10 * time.Millisecond,
		OnStarted:         func() { close(ready) },
		Logger:            func(string, ...any) {},
	})

	const tokenCost int64 = 20000
	const wantReward = tokenCost * exchange.ColdCompressionBountyPct / 100 // 4000

	// --- SETUP: real inventory entry + posted cold compression assign + a real
	// claim + a real completion, all folded BEFORE runEngineLoop starts (so
	// eng.Start's replay picks them up and the first ticker tick sees a completed
	// assign to pay). ---
	origContent := "VEC_A original plaintext " + strings.Repeat("z", 375) // 400 bytes
	putID := randomLocalMsgID(t)
	if err := eng.IngestLocalRecord(dgstore.Record{
		ID:         putID,
		CampfireID: "local",
		Sender:     seller.PubKeyHex(),
		Payload:    serveCompressPutPayload("serve-wiring fixture: compressible doc", origContent, tokenCost),
		Tags:       []string{exchange.TagPut, "exchange:content-type:code", "exchange:domain:go"},
		Timestamp:  time.Now().UnixNano(),
	}); err != nil {
		t.Fatalf("IngestLocalRecord(put): %v", err)
	}
	foldPutAcceptNoHot(t, eng, operator.PubKeyHex(), putID, tokenCost*70/100)

	inv := eng.State().Inventory()
	if len(inv) != 1 {
		t.Fatalf("inventory after put-accept = %d, want 1", len(inv))
	}
	entryID := inv[0].EntryID
	if err := eng.PostOpenCompressionAssign(entryID); err != nil {
		t.Fatalf("PostOpenCompressionAssign: %v", err)
	}

	assignID := ""
	for _, a := range eng.State().AllActiveAssigns() {
		if a.TaskType == "compress" && a.EntryID == entryID {
			assignID = a.AssignID
			break
		}
	}
	if assignID == "" {
		t.Fatalf("no cold compression assign posted for entry %s", entryID)
	}

	// Agent claims (real fold via IngestLocalRecord).
	if err := eng.IngestLocalRecord(dgstore.Record{
		ID:          randomLocalMsgID(t),
		CampfireID:  "local",
		Sender:      agent.PubKeyHex(),
		Payload:     []byte(`{}`),
		Tags:        []string{exchange.TagAssignClaim},
		Antecedents: []string{assignID},
		Timestamp:   time.Now().UnixNano(),
	}); err != nil {
		t.Fatalf("IngestLocalRecord(claim): %v", err)
	}
	claimMsgID := ""
	for _, a := range eng.State().AllActiveAssigns() {
		if a.AssignID == assignID {
			if a.ClaimantKey != agent.PubKeyHex() {
				t.Fatalf("assign claimant = %s, want agent", a.ClaimantKey)
			}
			claimMsgID = a.ClaimMsgID
			break
		}
	}
	if claimMsgID == "" {
		t.Fatalf("claim did not fold for assign %s", assignID)
	}

	// Agent completes with a VALID compression (VEC_A marker -> cosine 1.0, 200
	// bytes -> 50% reduction, correct evidence).
	good := []byte("VEC_A compressed " + strings.Repeat("z", 183)) // 200 bytes
	if err := eng.IngestLocalRecord(dgstore.Record{
		ID:          randomLocalMsgID(t),
		CampfireID:  "local",
		Sender:      agent.PubKeyHex(),
		Payload:     serveCompressResult(good),
		Tags:        []string{exchange.TagAssignComplete},
		Antecedents: []string{claimMsgID},
		Timestamp:   time.Now().UnixNano(),
	}); err != nil {
		t.Fatalf("IngestLocalRecord(complete): %v", err)
	}

	// --- Start the REAL serve inner loop. Its auto-accept-assign goroutine
	// (started by runEngineLoop) must validate + pay the completed assign. ---
	mediumLoop := pricing.NewMediumLoop(pricing.MediumLoopOptions{
		State:      eng.State(),
		Interval:   time.Hour, // parked — this test is about the assign ticker
		PostAssign: func(pricing.AssignSpec) error { return nil },
	})
	logger := log.New(io.Discard, "", 0)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		_ = runEngineLoop(ctx, dgHome, eng, mediumLoop, ready, logger)
	}()
	t.Cleanup(func() { cancel(); <-loopDone })

	// The ticker (started by runEngineLoop, firing every 40ms) must pay the agent.
	deadline := time.Now().Add(15 * time.Second)
	for ss.Balance(agent.PubKeyHex()) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("timed out: agent scrip balance never increased — runEngineLoop's auto-accept-assign ticker did not pay the completed compression assign")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := ss.Balance(agent.PubKeyHex()); got != wantReward {
		t.Fatalf("agent balance = %d, want %d (cold-compression bounty, paid once by the serve ticker)", got, wantReward)
	}
}

func serveCompressPutPayload(desc, content string, tokenCost int64) []byte {
	p, _ := json.Marshal(map[string]any{
		"description":  desc,
		"content":      base64.StdEncoding.EncodeToString([]byte(content)),
		"token_cost":   tokenCost,
		"content_type": "code",
		"domains":      []string{"go"},
	})
	return p
}

func serveCompressResult(content []byte) []byte {
	p, _ := json.Marshal(map[string]any{
		"content_hash": sha256RefLocal(content),
		"content_size": int64(len(content)),
		"content":      base64.StdEncoding.EncodeToString(content),
	})
	return p
}

func sha256RefLocal(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}
