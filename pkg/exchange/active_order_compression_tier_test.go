package exchange_test

// Tests for ActiveOrder.CompressionTier field persistence across replay (dontguess-643).
//
// Bug: applyBuy() did not store compression_tier from the buy payload into
// ActiveOrder.CompressionTier. The engine read compression_tier directly from a
// fresh JSON parse at dispatch time, so initial dispatch was correct — but state
// derived from replay lost the tier filter silently.
//
// Fix: ActiveOrder now carries CompressionTier, populated by applyBuy().

import (
	"encoding/json"
	"testing"

	"github.com/3dl-dev/dontguess/pkg/exchange"
)

// buildBuyPayloadWithTier builds a minimal exchange:buy payload with the given compression_tier.
func buildBuyPayloadWithTier(task string, budget int64, tier string) []byte {
	fields := map[string]any{
		"task":        task,
		"budget":      budget,
		"max_results": 3,
	}
	if tier != "" {
		fields["compression_tier"] = tier
	}
	p, _ := json.Marshal(fields)
	return p
}

// TestActiveOrder_CompressionTier_SurvivesReplay sends a buy with a tier filter,
// replays the message log into a fresh state, and asserts that the reconstructed
// active order carries the correct CompressionTier.
func TestActiveOrder_CompressionTier_SurvivesReplay(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	// Send a buy with compression_tier="hot".
	buyMsg := h.sendMessage(h.buyer,
		buildBuyPayloadWithTier("summarise Go function signatures", 10000, "hot"),
		[]string{exchange.TagBuy},
		nil,
	)

	// Replay the full message log into state (simulating a fresh engine restart).
	msgs, err := h.st.ListMessages(h.cfID, 0)
	if err != nil {
		t.Fatalf("listing messages: %v", err)
	}
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	// Verify that the reconstructed active order has the correct CompressionTier.
	orders := eng.State().ActiveOrders()
	var found *exchange.ActiveOrder
	for _, o := range orders {
		if o.OrderID == buyMsg.ID {
			found = o
			break
		}
	}
	if found == nil {
		t.Fatalf("active order %s not found after replay", buyMsg.ID[:8])
	}
	if found.CompressionTier != "hot" {
		t.Errorf("ActiveOrder.CompressionTier = %q after replay, want %q", found.CompressionTier, "hot")
	}
}

// TestActiveOrder_CompressionTier_AbsentWhenNotSet verifies that a buy without
// compression_tier produces an ActiveOrder with CompressionTier == "".
func TestActiveOrder_CompressionTier_AbsentWhenNotSet(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	buyMsg := h.sendMessage(h.buyer,
		buildBuyPayloadWithTier("summarise Go function signatures", 10000, ""),
		[]string{exchange.TagBuy},
		nil,
	)

	msgs, err := h.st.ListMessages(h.cfID, 0)
	if err != nil {
		t.Fatalf("listing messages: %v", err)
	}
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	orders := eng.State().ActiveOrders()
	var found *exchange.ActiveOrder
	for _, o := range orders {
		if o.OrderID == buyMsg.ID {
			found = o
			break
		}
	}
	if found == nil {
		t.Fatalf("active order %s not found after replay", buyMsg.ID[:8])
	}
	if found.CompressionTier != "" {
		t.Errorf("ActiveOrder.CompressionTier = %q, want empty string when not set", found.CompressionTier)
	}
}

// TestActiveOrder_CompressionTier_AllTierValues verifies CompressionTier is
// preserved correctly for each valid tier value ("hot", "warm", "cold").
func TestActiveOrder_CompressionTier_AllTierValues(t *testing.T) {
	t.Parallel()
	for _, tier := range []string{"hot", "warm", "cold"} {
		tier := tier
		t.Run(tier, func(t *testing.T) {
			t.Parallel()
			h := newTestHarness(t)
			eng := h.newEngine()

			buyMsg := h.sendMessage(h.buyer,
				buildBuyPayloadWithTier("inference task for tier "+tier, 5000, tier),
				[]string{exchange.TagBuy},
				nil,
			)

			msgs, err := h.st.ListMessages(h.cfID, 0)
			if err != nil {
				t.Fatalf("listing messages: %v", err)
			}
			eng.State().Replay(exchange.FromStoreRecords(msgs))

			orders := eng.State().ActiveOrders()
			var found *exchange.ActiveOrder
			for _, o := range orders {
				if o.OrderID == buyMsg.ID {
					found = o
					break
				}
			}
			if found == nil {
				t.Fatalf("active order %s not found after replay", buyMsg.ID[:8])
			}
			if found.CompressionTier != tier {
				t.Errorf("tier=%q: ActiveOrder.CompressionTier = %q, want %q", tier, found.CompressionTier, tier)
			}
		})
	}
}
