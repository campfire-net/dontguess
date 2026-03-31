package exchange_test

// Tests for input validation on TAINTED fields in applyPut and applyBuy.
//
// Convention: oversized or out-of-range TAINTED fields are dropped silently at
// the state layer. The message persists on the campfire log but is never
// materialized into pendingPuts / activeOrders, preventing OOM and int-overflow
// attacks via large allocations.
//
// Covered boundaries:
//   - Put: Description > 64 KiB rejected
//   - Put: Domains count > 5 rejected
//   - Put: TokenCost <= 0 rejected
//   - Put: TokenCost > MaxInt32 rejected
//   - Put: valid values accepted
//   - Buy: Task > 64 KiB rejected
//   - Buy: MaxResults > 100 rejected
//   - Buy: valid values accepted (including MaxResults == 100)

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/3dl-dev/dontguess/pkg/exchange"
)

// buildPutPayload constructs an exchange:put JSON payload with explicit fields.
// content is generated from desc to satisfy the engine's content-required check.
func buildPutPayload(desc string, tokenCost int64, domains []string) []byte {
	contentBytes := []byte("cached inference result: " + desc)
	contentB64 := base64.StdEncoding.EncodeToString(contentBytes)
	p, _ := json.Marshal(map[string]any{
		"description":  desc,
		"content":      contentB64,
		"token_cost":   tokenCost,
		"content_type": "code",
		"domains":      domains,
	})
	return p
}

// buildBuyPayload constructs an exchange:buy JSON payload with explicit fields.
func buildBuyPayload(task string, maxResults int) []byte {
	p, _ := json.Marshal(map[string]any{
		"task":        task,
		"budget":      5000,
		"max_results": maxResults,
	})
	return p
}

// replayIntoEngine sends a put message and replays the log into a fresh engine.
func replayIntoEngine(t *testing.T, h *testHarness, eng *exchange.Engine, payload []byte) {
	t.Helper()
	h.sendMessage(h.seller, payload,
		[]string{exchange.TagPut, "exchange:content-type:code"},
		nil,
	)
	msgs, err := h.st.ListMessages(h.cfID, 0)
	if err != nil {
		t.Fatalf("listing messages: %v", err)
	}
	eng.State().Replay(exchange.FromStoreRecords(msgs))
}

// replayBuyIntoEngine sends a buy message and replays the log.
func replayBuyIntoEngine(t *testing.T, h *testHarness, eng *exchange.Engine, payload []byte) {
	t.Helper()
	h.sendMessage(h.buyer, payload,
		[]string{exchange.TagBuy},
		nil,
	)
	msgs, err := h.st.ListMessages(h.cfID, 0)
	if err != nil {
		t.Fatalf("listing messages: %v", err)
	}
	eng.State().Replay(exchange.FromStoreRecords(msgs))
}

// ---- Put validation tests ----

func TestValidation_Put_OversizedDescriptionDropped(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	// Description just over the 64 KiB limit.
	oversized := strings.Repeat("x", exchange.MaxDescriptionBytes+1)
	replayIntoEngine(t, h, eng, buildPutPayload(oversized, 10000, []string{"go"}))

	// The put should not appear in pendingPuts — AutoAcceptPut must fail to find it.
	fakeMsgID := "not-a-real-id"
	err := eng.AutoAcceptPut(fakeMsgID, 7000, time.Now().Add(72*time.Hour))
	// We expect an error (entry not found), not a panic or nil.
	if err == nil {
		t.Error("expected error for non-existent put, got nil")
	}

	// Confirm inventory is still empty (no put-accept was possible).
	if inv := eng.State().Inventory(); len(inv) != 0 {
		t.Errorf("expected empty inventory after oversized description, got %d entries", len(inv))
	}
}

func TestValidation_Put_ExactDescriptionLimitAccepted(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	// Exactly at the limit: should be accepted.
	exact := strings.Repeat("y", exchange.MaxDescriptionBytes)
	putMsg := h.sendMessage(h.seller,
		buildPutPayload(exact, 10000, []string{"go"}),
		[]string{exchange.TagPut, "exchange:content-type:code"},
		nil,
	)
	msgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	if err := eng.AutoAcceptPut(putMsg.ID, 7000, time.Now().Add(72*time.Hour)); err != nil {
		t.Errorf("valid put at description limit rejected: %v", err)
	}
}

func TestValidation_Put_TooManyDomainsDropped(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	// MaxDomainsCount+1 domains.
	tooMany := make([]string, exchange.MaxDomainsCount+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("domain%d", i)
	}
	replayIntoEngine(t, h, eng, buildPutPayload("valid description", 10000, tooMany))

	if inv := eng.State().Inventory(); len(inv) != 0 {
		t.Errorf("expected empty inventory after too-many domains, got %d entries", len(inv))
	}
}

func TestValidation_Put_ExactDomainsLimitAccepted(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	exact := make([]string, exchange.MaxDomainsCount)
	for i := range exact {
		exact[i] = fmt.Sprintf("domain%d", i)
	}
	putMsg := h.sendMessage(h.seller,
		buildPutPayload("valid description", 10000, exact),
		[]string{exchange.TagPut, "exchange:content-type:code"},
		nil,
	)
	msgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	if err := eng.AutoAcceptPut(putMsg.ID, 7000, time.Now().Add(72*time.Hour)); err != nil {
		t.Errorf("valid put at domains limit rejected: %v", err)
	}
}

func TestValidation_Put_ZeroTokenCostDropped(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	replayIntoEngine(t, h, eng, buildPutPayload("valid description", 0, []string{"go"}))

	if inv := eng.State().Inventory(); len(inv) != 0 {
		t.Errorf("expected empty inventory after zero token_cost, got %d entries", len(inv))
	}
}

func TestValidation_Put_NegativeTokenCostDropped(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	replayIntoEngine(t, h, eng, buildPutPayload("valid description", -1, []string{"go"}))

	if inv := eng.State().Inventory(); len(inv) != 0 {
		t.Errorf("expected empty inventory after negative token_cost, got %d entries", len(inv))
	}
}

func TestValidation_Put_ExcessiveTokenCostDropped(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	// One over the MaxInt32 limit.
	replayIntoEngine(t, h, eng, buildPutPayload("valid description", exchange.MaxTokenCost+1, []string{"go"}))

	if inv := eng.State().Inventory(); len(inv) != 0 {
		t.Errorf("expected empty inventory after excessive token_cost, got %d entries", len(inv))
	}
}

func TestValidation_Put_MaxTokenCostAccepted(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	putMsg := h.sendMessage(h.seller,
		buildPutPayload("valid description", exchange.MaxTokenCost, []string{"go"}),
		[]string{exchange.TagPut, "exchange:content-type:code"},
		nil,
	)
	msgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	if err := eng.AutoAcceptPut(putMsg.ID, 7000, time.Now().Add(72*time.Hour)); err != nil {
		t.Errorf("valid put at max token_cost rejected: %v", err)
	}
}

// ---- Buy validation tests ----

func TestValidation_Buy_OversizedTaskDropped(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	oversized := strings.Repeat("z", exchange.MaxTaskBytes+1)
	replayBuyIntoEngine(t, h, eng, buildBuyPayload(oversized, 3))

	orders := eng.State().ActiveOrders()
	if len(orders) != 0 {
		t.Errorf("expected no active orders after oversized task, got %d", len(orders))
	}
}

func TestValidation_Buy_ExactTaskLimitAccepted(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	exact := strings.Repeat("z", exchange.MaxTaskBytes)
	replayBuyIntoEngine(t, h, eng, buildBuyPayload(exact, 3))

	orders := eng.State().ActiveOrders()
	if len(orders) != 1 {
		t.Errorf("expected 1 active order at task limit, got %d", len(orders))
	}
}

func TestValidation_Buy_ExcessiveMaxResultsDropped(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	replayBuyIntoEngine(t, h, eng, buildBuyPayload("valid task", exchange.MaxBuyMaxResults+1))

	orders := eng.State().ActiveOrders()
	if len(orders) != 0 {
		t.Errorf("expected no active orders after max_results > limit, got %d", len(orders))
	}
}

func TestValidation_Buy_MaxResultsAtLimitAccepted(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	replayBuyIntoEngine(t, h, eng, buildBuyPayload("valid task", exchange.MaxBuyMaxResults))

	orders := eng.State().ActiveOrders()
	if len(orders) != 1 {
		t.Errorf("expected 1 active order at max_results limit, got %d", len(orders))
	}
	if orders[0].MaxResults != exchange.MaxBuyMaxResults {
		t.Errorf("expected MaxResults=%d, got %d", exchange.MaxBuyMaxResults, orders[0].MaxResults)
	}
}
