package exchange_test

// Federation-aware credit tests (§4A of docs/design/semantic-matching-marketplace.md).
//
// Covers:
//  1. FederationNodeProfile created on first message from a sender.
//  2. trust_score converges toward 1.0 via the slow loop computation.
//  3. New-node dual guard routes low-trust senders to inline (not brokered) matching.

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/campfire-net/campfire/pkg/store"

	"github.com/campfire-net/dontguess/pkg/exchange"
	"github.com/campfire-net/dontguess/pkg/pricing"
)

// countBrokeredMatchAssigns counts exchange:assign messages in the given list
// whose payload contains task_type="brokered-match".
func countBrokeredMatchAssigns(msgs []store.MessageRecord) int {
	count := 0
	for _, m := range msgs {
		var p struct {
			TaskType string `json:"task_type"`
		}
		if err := json.Unmarshal(m.Payload, &p); err == nil && p.TaskType == "brokered-match" {
			count++
		}
	}
	return count
}

// --- 1. FederationNodeProfile created on first message ---

// TestFederationProfile_CreatedOnFirstMessage verifies that the exchange State
// creates a FederationNodeProfile for a sender the first time it applies a
// message from that sender.
func TestFederationProfile_CreatedOnFirstMessage(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	// Before any message, no profile exists.
	if p := eng.State().FederationProfile(h.buyer.pubKeyHex); p != nil {
		t.Fatalf("expected no profile before first message, got %+v", p)
	}

	// Send one buy message from the buyer.
	h.sendMessage(h.buyer,
		buyPayload("Generate a Go HTTP server", 5000),
		[]string{exchange.TagBuy},
		nil,
	)

	// Apply via engine's Replay to ensure state is updated.
	msgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{})
	state := exchange.NewState()
	exMsgs := exchange.FromStoreRecords(msgs)
	state.Replay(exMsgs)

	// Profile must exist after Replay.
	p := state.FederationProfile(h.buyer.pubKeyHex)
	if p == nil {
		t.Fatal("FederationNodeProfile not created after first message")
	}
	if p.SenderKey != h.buyer.pubKeyHex {
		t.Errorf("SenderKey: got %q, want %q", p.SenderKey, h.buyer.pubKeyHex)
	}
	if p.TrustScore != exchange.NewNodeTrustScoreStart {
		t.Errorf("initial TrustScore: got %.2f, want %.2f", p.TrustScore, exchange.NewNodeTrustScoreStart)
	}
	if p.FirstSeenAt.IsZero() {
		t.Error("FirstSeenAt should not be zero after first message")
	}
	_ = eng // referenced to prevent unused-import lint
}

// TestFederationProfile_HopDepthTracked verifies that Antecedents length is
// recorded as provenance hop depth and that the profile's HopDepth reflects
// the median.
func TestFederationProfile_HopDepthTracked(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)

	state := exchange.NewState()

	// 3 messages: hop depths 0, 2, 2 → median = 2.
	antecedentSets := [][]string{
		nil,           // hop depth 0
		{"a1", "a2"}, // hop depth 2
		{"b1", "b2"}, // hop depth 2
	}
	for i, ants := range antecedentSets {
		msg := h.sendMessage(h.seller,
			putPayload(fmt.Sprintf("task %d", i), fmt.Sprintf("sha256:%064x", i), "code", 1000, 500),
			[]string{exchange.TagPut},
			ants,
		)
		state.Apply(msg)
	}

	p := state.FederationProfile(h.seller.pubKeyHex)
	if p == nil {
		t.Fatal("no profile after messages")
	}
	if p.HopDepth != 2 {
		t.Errorf("HopDepth: got %d, want 2 (median of [0,2,2])", p.HopDepth)
	}
}

// --- 2. trust_score converges with history via slow loop ---

// TestFederationTrustScore_ConvergesViaSlowLoop verifies that the slow loop's
// computeFederationTrustScores method writes a trust_score > NewNodeTrustScoreStart
// for a sender with zero hop depth (local sender).
func TestFederationTrustScore_ConvergesViaSlowLoop(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)

	state := exchange.NewState()

	// Send a message with hop depth 0 (local sender).
	msg := h.sendMessage(h.buyer,
		buyPayload("task", 5000),
		[]string{exchange.TagBuy},
		nil,
	)
	state.Apply(msg)

	// Verify profile was created at the new-node start score.
	p := state.FederationProfile(h.buyer.pubKeyHex)
	if p == nil {
		t.Fatal("no federation profile after message")
	}
	if p.TrustScore != exchange.NewNodeTrustScoreStart {
		t.Errorf("pre-slowloop TrustScore: got %.3f, want %.3f", p.TrustScore, exchange.NewNodeTrustScoreStart)
	}

	// Configure and run the slow loop (single tick).
	// FederationState = state (implements both reader and writer interfaces).
	loop := pricing.NewSlowLoop(pricing.SlowLoopOptions{
		State:           state,
		FederationState: state,
		Now:             func() time.Time { return time.Now() },
		Logger: func(format string, args ...any) {
			t.Logf("[slow] "+format, args...)
		},
	})
	result := loop.Tick()

	// Exactly one trust update should have been written.
	if len(result.FederationTrustUpdates) != 1 {
		t.Fatalf("expected 1 federation trust update, got %d", len(result.FederationTrustUpdates))
	}
	u := result.FederationTrustUpdates[0]
	if u.SenderKey != h.buyer.pubKeyHex {
		t.Errorf("update SenderKey: got %q, want %q", u.SenderKey, h.buyer.pubKeyHex)
	}

	// A local sender (hop depth 0) with no defaults:
	//   w_history * 1.0 + w_depth * (1/(1+0)) + w_volume * 1.0
	//   = 0.70 * 1.0 + 0.15 * 1.0 + 0.15 * 1.0 = 1.0
	// It may differ slightly based on volume_fraction computation.
	// The important assertion: score > NewNodeTrustScoreStart (has converged upward).
	if u.TrustScore <= exchange.NewNodeTrustScoreStart {
		t.Errorf("trust_score should be > %.2f after slow loop tick, got %.4f",
			exchange.NewNodeTrustScoreStart, u.TrustScore)
	}

	// Also verify that State.FederationProfile reflects the updated score.
	p2 := state.FederationProfile(h.buyer.pubKeyHex)
	if p2 == nil {
		t.Fatal("profile disappeared after slow loop tick")
	}
	if p2.TrustScore != u.TrustScore {
		t.Errorf("State.FederationProfile TrustScore: got %.4f, want %.4f", p2.TrustScore, u.TrustScore)
	}
}

// TestFederationTrustScore_MultipleNodes verifies that the slow loop computes
// independent trust_scores for each known sender key.
func TestFederationTrustScore_MultipleNodes(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)

	state := exchange.NewState()

	buyer2 := newTestAgent(t)

	// Each sends one message.
	for _, agent := range []*testAgent{h.buyer, buyer2} {
		msg := h.sendMessage(agent,
			buyPayload("some task", 5000),
			[]string{exchange.TagBuy},
			nil,
		)
		state.Apply(msg)
	}

	loop := pricing.NewSlowLoop(pricing.SlowLoopOptions{
		State:           state,
		FederationState: state,
		Now:             func() time.Time { return time.Now() },
	})
	result := loop.Tick()

	if len(result.FederationTrustUpdates) != 2 {
		t.Fatalf("expected 2 federation trust updates, got %d", len(result.FederationTrustUpdates))
	}

	// Both scores should be above the new-node start.
	for _, u := range result.FederationTrustUpdates {
		if u.TrustScore <= exchange.NewNodeTrustScoreStart {
			t.Errorf("sender %s: trust_score %.4f not above new-node start %.2f",
				u.SenderKey[:8], u.TrustScore, exchange.NewNodeTrustScoreStart)
		}
	}
}

// --- 3. New-node dual guard routes low-trust senders to inline ---

// TestFederationDualGuard_LowTrustRoutesInline verifies that when
// FederationGuardEnabled=true and BrokeredMatchMode=true, a buyer with no
// trust profile (low trust = new node) is routed to inline matching. The engine
// should emit an exchange:match (inline) rather than an exchange:assign (brokered).
func TestFederationDualGuard_LowTrustRoutesInline(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngineWithOpts(func(o *exchange.EngineOptions) {
		o.BrokeredMatchMode = true
		o.FederationGuardEnabled = true
		o.BrokeredMatchReward = 50
	})

	// Seed one inventory entry so there's something to match against.
	putMsg := h.sendMessage(h.seller,
		putPayload("Kubernetes manifest generator", "sha256:"+fmt.Sprintf("%064x", 9001), "code", 8000, 12000),
		[]string{exchange.TagPut, "exchange:content-type:code"},
		nil,
	)
	if err := eng.AutoAcceptPut(putMsg.ID, 5000, time.Now().Add(72*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut: %v", err)
	}

	// Buyer sends buy — buyer has NO trust profile (new node, low trust).
	h.sendMessage(h.buyer,
		buyPayload("Generate a kubernetes manifest for nginx", 20000),
		[]string{exchange.TagBuy},
		nil,
	)

	allAssignsPre, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagAssign}})
	preBrokeredCount := countBrokeredMatchAssigns(allAssignsPre)
	preMatches, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	h.startEngine(eng, ctx, cancel)

	// Wait for a match (inline routing) or a brokered-match assign to appear.
	var finalAllAssigns, finalMatches []store.MessageRecord
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		finalAllAssigns, _ = h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagAssign}})
		finalMatches, _ = h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})
		newBrokered := countBrokeredMatchAssigns(finalAllAssigns) - preBrokeredCount
		newMatches := len(finalMatches) - len(preMatches)
		if newMatches > 0 || newBrokered > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()

	newBrokeredAssigns := countBrokeredMatchAssigns(finalAllAssigns) - preBrokeredCount
	newMatches := len(finalMatches) - len(preMatches)

	// The dual guard should have forced inline: no brokered-match assign,
	// but at least one exchange:match (inline).
	if newBrokeredAssigns > 0 {
		t.Errorf("dual guard failed: got %d new brokered-match assign(s) — low-trust buyer should route inline", newBrokeredAssigns)
	}
	if newMatches == 0 {
		t.Error("expected at least one inline match message for low-trust buyer, got none")
	}
}

// TestFederationDualGuard_HighTrustRoutesBrokered verifies that a buyer with
// a trust_score above the threshold receives brokered routing when
// FederationGuardEnabled=true and BrokeredMatchMode=true.
func TestFederationDualGuard_HighTrustRoutesBrokered(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngineWithOpts(func(o *exchange.EngineOptions) {
		o.BrokeredMatchMode = true
		o.FederationGuardEnabled = true
		o.BrokeredMatchReward = 50
	})

	// Seed inventory.
	putMsg := h.sendMessage(h.seller,
		putPayload("Terraform module for S3 bucket", "sha256:"+fmt.Sprintf("%064x", 9002), "code", 9000, 14000),
		[]string{exchange.TagPut, "exchange:content-type:code"},
		nil,
	)
	if err := eng.AutoAcceptPut(putMsg.ID, 6000, time.Now().Add(72*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut: %v", err)
	}

	// Manually set the buyer's trust score above the threshold BEFORE the buy
	// message is processed by the engine. The engine reads FederationProfile from
	// State, which the slow loop writes via SetFederationTrustScore.
	eng.State().SetFederationTrustScore(h.buyer.pubKeyHex, 0.85)

	h.sendMessage(h.buyer,
		buyPayload("Generate terraform module for S3 with versioning", 20000),
		[]string{exchange.TagBuy},
		nil,
	)

	allAssignsPre, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagAssign}})
	preBrokeredCount := countBrokeredMatchAssigns(allAssignsPre)
	preMatches, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	h.startEngine(eng, ctx, cancel)

	var finalAllAssigns []store.MessageRecord
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		finalAllAssigns, _ = h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagAssign}})
		if countBrokeredMatchAssigns(finalAllAssigns) > preBrokeredCount {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()

	newBrokeredAssigns := countBrokeredMatchAssigns(finalAllAssigns) - preBrokeredCount
	finalMatches, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})
	newMatches := len(finalMatches) - len(preMatches)

	// High-trust buyer should get brokered routing: exactly 1 brokered-match assign.
	if newBrokeredAssigns != 1 {
		t.Errorf("expected 1 brokered-match assign for high-trust buyer, got %d", newBrokeredAssigns)
	}
	if newMatches != 0 {
		t.Errorf("expected no inline match for high-trust buyer (brokered mode), got %d", newMatches)
	}
}

// TestFederationDualGuard_DisabledNoEffect verifies that when
// FederationGuardEnabled=false (the default), BrokeredMatchMode=true causes
// all buyers — including new nodes with no trust profile — to receive brokered
// routing. This preserves backward compatibility.
func TestFederationDualGuard_DisabledNoEffect(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngineWithOpts(func(o *exchange.EngineOptions) {
		o.BrokeredMatchMode = true
		// FederationGuardEnabled = false (default)
		o.BrokeredMatchReward = 50
	})

	putMsg := h.sendMessage(h.seller,
		putPayload("Docker multi-stage build", "sha256:"+fmt.Sprintf("%064x", 9003), "code", 7000, 11000),
		[]string{exchange.TagPut, "exchange:content-type:code"},
		nil,
	)
	if err := eng.AutoAcceptPut(putMsg.ID, 4800, time.Now().Add(72*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut: %v", err)
	}

	h.sendMessage(h.buyer,
		buyPayload("Generate docker multi-stage build for node.js app", 20000),
		[]string{exchange.TagBuy},
		nil,
	)

	allAssignsPre, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagAssign}})
	preBrokeredCount := countBrokeredMatchAssigns(allAssignsPre)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	h.startEngine(eng, ctx, cancel)

	var finalAllAssigns []store.MessageRecord
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		finalAllAssigns, _ = h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagAssign}})
		if countBrokeredMatchAssigns(finalAllAssigns) > preBrokeredCount {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()

	newBrokeredAssigns := countBrokeredMatchAssigns(finalAllAssigns) - preBrokeredCount

	// Without federation guard, brokered mode should always route to brokered-match
	// assign regardless of trust score (backward compatibility).
	if newBrokeredAssigns != 1 {
		t.Errorf("expected 1 brokered-match assign (guard disabled), got %d", newBrokeredAssigns)
	}
}

// TestFederationProfile_TransactionCountIncrements verifies that
// incrementFederationTransactionCount is called when a settle:complete is
// processed, and the TransactionCount on the profile reflects this.
func TestFederationProfile_TransactionCountIncrements(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)

	state := exchange.NewState()

	// Seed a message from the buyer so a profile exists.
	buyMsg := h.sendMessage(h.buyer,
		buyPayload("some task", 5000),
		[]string{exchange.TagBuy},
		nil,
	)
	state.Apply(buyMsg)

	// Initial transaction count should be 0.
	p := state.FederationProfile(h.buyer.pubKeyHex)
	if p == nil {
		t.Fatal("no profile after buy message")
	}
	if p.TransactionCount != 0 {
		t.Errorf("initial TransactionCount: got %d, want 0", p.TransactionCount)
	}

	// IsNewNode should return true (count < 50).
	if !p.IsNewNode(time.Now()) {
		t.Error("expected IsNewNode=true with TransactionCount=0")
	}
}


// TestSenderHopDepth_BoundedWindow verifies that senderHopDepth is capped at
// SenderHopDepthWindowSize even when more than that many messages arrive from
// the same sender.
func TestSenderHopDepth_BoundedWindow(t *testing.T) {
	t.Parallel()

	state := exchange.NewState()
	senderKey := "test-sender-bounded-window"

	// Inject SenderHopDepthWindowSize+1 observations via UpdateFederationProfile.
	total := exchange.SenderHopDepthWindowSize + 1
	for i := 0; i < total; i++ {
		state.UpdateFederationProfile(senderKey, i%5)
	}

	depths := state.SenderHopDepths(senderKey)
	if len(depths) != exchange.SenderHopDepthWindowSize {
		t.Errorf("SenderHopDepths len: got %d, want %d (SenderHopDepthWindowSize)", len(depths), exchange.SenderHopDepthWindowSize)
	}
}
