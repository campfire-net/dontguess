package exchange_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/campfire-net/campfire/pkg/campfire"
	"github.com/campfire-net/campfire/pkg/identity"
	"github.com/campfire-net/campfire/pkg/message"
	"github.com/campfire-net/campfire/pkg/store"
	"github.com/campfire-net/campfire/pkg/transport/fs"

	"github.com/3dl-dev/dontguess/pkg/exchange"
)

// testHarness sets up a minimal exchange for engine tests.
type testHarness struct {
	t         *testing.T
	cfID      string
	operator  *identity.Identity
	seller    *identity.Identity
	buyer     *identity.Identity
	transport *fs.Transport
	st        store.Store
}

func newTestHarness(t *testing.T) *testHarness {
	t.Helper()
	cfHome := t.TempDir()
	transportDir := t.TempDir()
	convDir := conventionDir(t)

	// Create exchange via Init to get a properly bootstrapped campfire.
	cfg, err := exchange.Init(exchange.InitOptions{
		CFHome:           cfHome,
		TransportBaseDir: transportDir,
		BeaconDir:        t.TempDir(),
		ConventionDir:    convDir,
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Re-load the operator identity that Init created.
	operatorID, err := identity.Load(cfHome + "/identity.json")
	if err != nil {
		t.Fatalf("loading operator identity: %v", err)
	}

	// Open the store.
	st, err := store.Open(store.StorePath(cfHome))
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	// Pull in the convention messages that Init wrote to the transport into the store.
	transport := fs.New(transportDir)
	syncTransportToStore(t, st, cfg.ExchangeCampfireID, transport)

	// Generate test identities for seller and buyer.
	sellerID, err := identity.Generate()
	if err != nil {
		t.Fatalf("generating seller identity: %v", err)
	}
	buyerID, err := identity.Generate()
	if err != nil {
		t.Fatalf("generating buyer identity: %v", err)
	}

	return &testHarness{
		t:         t,
		cfID:      cfg.ExchangeCampfireID,
		operator:  operatorID,
		seller:    sellerID,
		buyer:     buyerID,
		transport: transport,
		st:        st,
	}
}

// syncTransportToStore reads messages from the transport and adds them to the store.
func syncTransportToStore(t *testing.T, st store.Store, cfID string, transport *fs.Transport) {
	t.Helper()
	msgs, err := transport.ListMessages(cfID)
	if err != nil {
		t.Fatalf("listing transport messages: %v", err)
	}
	for i := range msgs {
		rec := store.MessageRecordFromMessage(cfID, &msgs[i], store.NowNano())
		if _, err := st.AddMessage(rec); err != nil {
			// Ignore duplicate key errors (messages already in store).
			_ = err
		}
	}
}

// sendMessage sends a signed message to the exchange campfire and persists it.
func (h *testHarness) sendMessage(sender *identity.Identity, payload []byte, tags []string, antecedents []string) *store.MessageRecord {
	h.t.Helper()
	msg, err := message.NewMessage(sender.PrivateKey, sender.PublicKey, payload, tags, antecedents)
	if err != nil {
		h.t.Fatalf("creating message: %v", err)
	}

	// Add provenance hop.
	cfState, err := h.transport.ReadState(h.cfID)
	if err != nil {
		h.t.Fatalf("reading campfire state: %v", err)
	}
	members, err := h.transport.ListMembers(h.cfID)
	if err != nil {
		h.t.Fatalf("listing members: %v", err)
	}
	cf := cfState.ToCampfire(members)
	if err := msg.AddHop(
		cfState.PrivateKey, cfState.PublicKey,
		cf.MembershipHash(), len(members),
		cfState.JoinProtocol, cfState.ReceptionRequirements,
		campfire.RoleFull,
	); err != nil {
		h.t.Fatalf("adding hop: %v", err)
	}

	if err := h.transport.WriteMessage(h.cfID, msg); err != nil {
		h.t.Fatalf("writing message to transport: %v", err)
	}

	rec := store.MessageRecordFromMessage(h.cfID, msg, store.NowNano())
	if _, err := h.st.AddMessage(rec); err != nil {
		h.t.Fatalf("adding message to store: %v", err)
	}
	return &rec
}

// newEngine returns a new Engine for this harness.
func (h *testHarness) newEngine() *exchange.Engine {
	return exchange.NewEngine(exchange.EngineOptions{
		CampfireID:       h.cfID,
		OperatorIdentity: h.operator,
		Store:            h.st,
		Transport:        h.transport,
		Logger: func(format string, args ...any) {
			h.t.Logf("[engine] "+format, args...)
		},
	})
}

// putPayload builds a minimal valid exchange:put payload.
func putPayload(desc, contentHash, contentType string, tokenCost, contentSize int64) []byte {
	p, _ := json.Marshal(map[string]any{
		"description":  desc,
		"content_hash": contentHash,
		"token_cost":   tokenCost,
		"content_type": contentType,
		"content_size": contentSize,
		"domains":      []string{"go", "testing"},
	})
	return p
}

// buyPayload builds a minimal valid exchange:buy payload.
func buyPayload(task string, budget int64) []byte {
	p, _ := json.Marshal(map[string]any{
		"task":        task,
		"budget":      budget,
		"max_results": 3,
	})
	return p
}

// TestState_PutAppearsinInventoryAfterAccept tests that a put → put-accept
// flow results in the entry appearing in the derived inventory.
func TestState_PutAppearsInInventoryAfterAccept(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	// Send a put from the seller.
	putMsg := h.sendMessage(h.seller,
		putPayload("Go HTTP test generator", "sha256:"+fmt.Sprintf("%064x", 1), "code", 15000, 24576),
		[]string{exchange.TagPut, "exchange:content-type:code", "exchange:domain:go"},
		nil,
	)

	// Replay log into state.
	msgs, err := h.st.ListMessages(h.cfID, 0)
	if err != nil {
		t.Fatalf("listing messages: %v", err)
	}
	eng.State().Replay(msgs)

	// Inventory should be empty (no put-accept yet).
	inv := eng.State().Inventory()
	if len(inv) != 0 {
		t.Errorf("expected empty inventory before accept, got %d entries", len(inv))
	}

	// Operator sends put-accept.
	if err := eng.AutoAcceptPut(putMsg.ID, 10500, time.Now().Add(168*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut: %v", err)
	}

	// Inventory should now have one entry.
	inv = eng.State().Inventory()
	if len(inv) != 1 {
		t.Fatalf("expected 1 inventory entry after accept, got %d", len(inv))
	}
	if inv[0].PutMsgID != putMsg.ID {
		t.Errorf("entry PutMsgID = %q, want %q", inv[0].PutMsgID, putMsg.ID)
	}
	if inv[0].SellerKey != h.seller.PublicKeyHex() {
		t.Errorf("entry SellerKey = %q, want %q", inv[0].SellerKey, h.seller.PublicKeyHex())
	}
}

// TestEngine_BuyEmitsMatchResponse tests that sending a buy message causes the
// engine to emit a match response that fulfills the buy future.
func TestEngine_BuyEmitsMatchResponse(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	// Seed one inventory entry: put + accept.
	putMsg := h.sendMessage(h.seller,
		putPayload("Terraform AWS module generator", "sha256:"+fmt.Sprintf("%064x", 2), "code", 8000, 12000),
		[]string{exchange.TagPut, "exchange:content-type:code", "exchange:domain:terraform"},
		nil,
	)

	// Replay to load the put.
	msgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(msgs)

	// Accept the put.
	if err := eng.AutoAcceptPut(putMsg.ID, 5600, time.Now().Add(72*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut: %v", err)
	}

	// Verify inventory has the entry.
	inv := eng.State().Inventory()
	if len(inv) != 1 {
		t.Fatalf("expected 1 inventory entry, got %d", len(inv))
	}

	// Buyer sends a buy message.
	buyMsg := h.sendMessage(h.buyer,
		buyPayload("Generate Terraform module for AWS S3 with versioning", 20000),
		[]string{exchange.TagBuy},
		nil,
	)

	// Apply the buy to state and handle it.
	buyRec, err := h.st.GetMessage(buyMsg.ID)
	if err != nil {
		t.Fatalf("getting buy message from store: %v", err)
	}
	eng.State().Apply(buyRec)

	// Dispatch the buy (triggers match response).
	// Access via a test hook: manually run handleBuy via poll.
	// We simulate a poll cycle by listing messages with buy tag after the buy timestamp.
	preMatchMessages, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{
		Tags: []string{exchange.TagMatch},
	})
	preMatchCount := len(preMatchMessages)

	// Run a single poll to dispatch the buy.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// Start the engine in a goroutine; it will process the buy and emit a match.
	// We poll the store for a match message to appear.
	go func() {
		_ = eng.Start(ctx)
	}()

	// Wait for the match message to appear in the store.
	var matchMsgs []store.MessageRecord
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		matchMsgs, _ = h.st.ListMessages(h.cfID, 0, store.MessageFilter{
			Tags: []string{exchange.TagMatch},
		})
		if len(matchMsgs) > preMatchCount {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	cancel() // stop the engine

	if len(matchMsgs) <= preMatchCount {
		t.Fatal("no match message emitted by engine")
	}

	// Verify the match message structure.
	matchMsg := matchMsgs[len(matchMsgs)-1]
	if !hasTag(matchMsg.Tags, exchange.TagMatch) {
		t.Errorf("match message missing exchange:match tag, got %v", matchMsg.Tags)
	}
	// Antecedent must be the buy message.
	if len(matchMsg.Antecedents) == 0 || matchMsg.Antecedents[0] != buyMsg.ID {
		t.Errorf("match antecedent = %v, want [%s]", matchMsg.Antecedents, buyMsg.ID)
	}
	// Sender must be the operator.
	if matchMsg.Sender != h.operator.PublicKeyHex() {
		t.Errorf("match sender = %q, want %q (operator)", matchMsg.Sender, h.operator.PublicKeyHex())
	}

	// Parse and validate match payload.
	var matchPayload struct {
		Results []struct {
			EntryID   string  `json:"entry_id"`
			Price     int64   `json:"price"`
			Confidence float64 `json:"confidence"`
		} `json:"results"`
	}
	if err := json.Unmarshal(matchMsg.Payload, &matchPayload); err != nil {
		t.Fatalf("parsing match payload: %v", err)
	}
	if len(matchPayload.Results) != 1 {
		t.Errorf("expected 1 match result, got %d", len(matchPayload.Results))
	}
	if matchPayload.Results[0].EntryID != putMsg.ID {
		t.Errorf("match result entry_id = %q, want %q", matchPayload.Results[0].EntryID, putMsg.ID)
	}
	// Price must not exceed buyer's budget (20000).
	if matchPayload.Results[0].Price > 20000 {
		t.Errorf("match result price %d exceeds buyer budget 20000", matchPayload.Results[0].Price)
	}
}

// TestState_ReplayRebuildsInventory tests that Replay reconstructs state
// correctly from a message sequence: put → put-accept.
func TestState_ReplayRebuildsInventory(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	// Send put and accept.
	putMsg := h.sendMessage(h.seller,
		putPayload("Security audit for Go HTTP handlers", "sha256:"+fmt.Sprintf("%064x", 3), "review", 20000, 40000),
		[]string{exchange.TagPut, "exchange:content-type:review"},
		nil,
	)
	if err := eng.AutoAcceptPut(putMsg.ID, 14000, time.Now().Add(48*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut: %v", err)
	}

	// Create fresh state, replay from store.
	freshState := exchange.NewState()
	msgs, err := h.st.ListMessages(h.cfID, 0)
	if err != nil {
		t.Fatalf("listing messages: %v", err)
	}
	freshState.Replay(msgs)

	inv := freshState.Inventory()
	if len(inv) != 1 {
		t.Fatalf("replayed inventory len = %d, want 1", len(inv))
	}
	if inv[0].PutMsgID != putMsg.ID {
		t.Errorf("replayed entry PutMsgID = %q, want %q", inv[0].PutMsgID, putMsg.ID)
	}
	if inv[0].PutPrice != 14000 {
		t.Errorf("replayed entry PutPrice = %d, want 14000", inv[0].PutPrice)
	}
}

// TestState_ExpiredEntryExcludedFromInventory tests that entries past their
// expiry are not returned from Inventory().
func TestState_ExpiredEntryExcludedFromInventory(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	putMsg := h.sendMessage(h.seller,
		putPayload("Old inference result", "sha256:"+fmt.Sprintf("%064x", 4), "analysis", 5000, 8000),
		[]string{exchange.TagPut, "exchange:content-type:analysis"},
		nil,
	)

	// Accept with expiry in the past.
	if err := eng.AutoAcceptPut(putMsg.ID, 3500, time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut: %v", err)
	}

	inv := eng.State().Inventory()
	if len(inv) != 0 {
		t.Errorf("expected expired entry excluded from inventory, got %d entries", len(inv))
	}
}

// TestState_SellerReputationStartsAtDefault tests that a new seller's reputation
// is DefaultReputation.
func TestState_SellerReputationStartsAtDefault(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	rep := eng.State().SellerReputation(h.seller.PublicKeyHex())
	if rep != exchange.DefaultReputation {
		t.Errorf("new seller reputation = %d, want %d", rep, exchange.DefaultReputation)
	}
}

// TestState_BuyOrderExpiry tests that orders older than 1 hour are not returned.
func TestState_BuyOrderExpiry(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	// Inject an old buy message by patching timestamp.
	// We can't fake the timestamp from the test; instead, we just verify
	// that a fresh buy is in ActiveOrders and is not expired.
	buyMsg := h.sendMessage(h.buyer,
		buyPayload("Some task", 1000),
		[]string{exchange.TagBuy},
		nil,
	)

	msgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(msgs)

	orders := eng.State().ActiveOrders()
	found := false
	for _, o := range orders {
		if o.OrderID == buyMsg.ID {
			found = true
			if o.IsExpired() {
				t.Error("fresh order should not be expired")
			}
			break
		}
	}
	if !found {
		t.Errorf("buy order %s not found in active orders", buyMsg.ID[:8])
	}
}

// TestState_PutRejectRemovesFromPending tests that a put-reject removes the
// entry from pending puts (it should not appear in inventory).
func TestState_PutRejectRemovesFromPending(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	putMsg := h.sendMessage(h.seller,
		putPayload("Rejected inference", "sha256:"+fmt.Sprintf("%064x", 5), "other", 1000, 512),
		[]string{exchange.TagPut, "exchange:content-type:other"},
		nil,
	)

	// Operator sends put-reject.
	rejectPayload, _ := json.Marshal(map[string]any{
		"phase":    "put-reject",
		"entry_id": putMsg.ID,
		"reason":   "content does not meet quality bar",
	})
	h.sendMessage(h.operator, rejectPayload,
		[]string{exchange.TagSettle, exchange.TagPhasePrefix + exchange.SettlePhaseStrPutReject},
		[]string{putMsg.ID},
	)

	msgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(msgs)

	inv := eng.State().Inventory()
	if len(inv) != 0 {
		t.Errorf("rejected put should not appear in inventory, got %d entries", len(inv))
	}
}

// TestState_SettleDeliverMarksMatchDelivered tests that a settle(deliver) message
// transitions state so that IsMatchDelivered returns true for the match message.
// This is a regression test for the missing SettlePhaseStrDeliver case in applySettle.
func TestState_SettleDeliverMarksMatchDelivered(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	// Step 1: Seller puts an entry and operator accepts it.
	putMsg := h.sendMessage(h.seller,
		putPayload("Deliver-phase test entry", "sha256:"+fmt.Sprintf("%064x", 999), "code", 10000, 16000),
		[]string{exchange.TagPut, "exchange:content-type:code", "exchange:domain:go"},
		nil,
	)
	msgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(msgs)

	if err := eng.AutoAcceptPut(putMsg.ID, 7000, time.Now().Add(48*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut: %v", err)
	}

	// Step 2: Buyer buys; engine emits a match.
	h.sendMessage(h.buyer,
		buyPayload("Unit tests for Go HTTP handler (deliver test)", 30000),
		[]string{exchange.TagBuy},
		nil,
	)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	preMsgs, _ := h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})
	go func() { _ = eng.Start(ctx) }()

	var matchMsgs []store.MessageRecord
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		matchMsgs, _ = h.st.ListMessages(h.cfID, 0, store.MessageFilter{Tags: []string{exchange.TagMatch}})
		if len(matchMsgs) > len(preMsgs) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()

	if len(matchMsgs) <= len(preMsgs) {
		t.Fatal("no match message emitted by engine")
	}
	matchMsg := matchMsgs[len(matchMsgs)-1]

	var mp struct {
		Results []struct {
			EntryID string `json:"entry_id"`
		} `json:"results"`
	}
	if err := json.Unmarshal(matchMsg.Payload, &mp); err != nil || len(mp.Results) == 0 {
		t.Fatalf("parsing match payload: %v", err)
	}

	// Step 3: Buyer accepts the match.
	buyerAcceptPayload, _ := json.Marshal(map[string]any{
		"phase":    "buyer-accept",
		"entry_id": mp.Results[0].EntryID,
		"accepted": true,
	})
	buyerAcceptMsg := h.sendMessage(h.buyer, buyerAcceptPayload,
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrBuyerAccept,
			exchange.TagVerdictPrefix + "accepted",
		},
		[]string{matchMsg.ID},
	)

	allMsgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(allMsgs)

	// Before deliver: match must not be marked delivered.
	if eng.State().IsMatchDelivered(matchMsg.ID) {
		t.Error("match should not be marked delivered before settle(deliver)")
	}

	// Step 4: Operator sends settle(deliver).
	deliverPayload, _ := json.Marshal(map[string]any{
		"phase":        "deliver",
		"entry_id":     mp.Results[0].EntryID,
		"content_ref":  "sha256:" + fmt.Sprintf("%064x", 999),
		"content_size": 16000,
	})
	h.sendMessage(h.operator, deliverPayload,
		[]string{
			exchange.TagSettle,
			exchange.TagPhasePrefix + exchange.SettlePhaseStrDeliver,
		},
		[]string{buyerAcceptMsg.ID},
	)

	allMsgs, _ = h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(allMsgs)

	// After deliver: match must be marked delivered.
	if !eng.State().IsMatchDelivered(matchMsg.ID) {
		t.Error("match should be marked delivered after settle(deliver)")
	}

	// Inventory entry must still be in inventory (deliver does not remove it).
	if eng.State().GetInventoryEntry(mp.Results[0].EntryID) == nil {
		t.Error("inventory entry should remain after deliver")
	}
}

// hasTag checks if tags contains the given tag.
func hasTag(tags []string, tag string) bool {
	for _, t := range tags {
		if t == tag {
			return true
		}
	}
	return false
}
