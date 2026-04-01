package exchange_test

// safety_hardening_test.go contains targeted tests for the four safety
// improvements from dontguess-g6d:
//   1. shortKey() never panics on short/malformed keys
//   2. Inventory() returns copies — callers cannot mutate internal state
//   3. resMu is RWMutex (structural — tested via concurrent GetReservation)

import (
	"fmt"
	"testing"
	"time"

	"github.com/campfire-net/dontguess/pkg/exchange"
)

// TestShortKey_NeverPanicsOnShortInput verifies that shortKey handles strings
// shorter than 8 characters without panicking (the previous [:8] slice would panic).
func TestShortKey_NeverPanicsOnShortInput(t *testing.T) {
	cases := []struct {
		input    string
		wantLen  int
		wantSame bool // true if output == input
	}{
		{"", 0, true},
		{"a", 1, true},
		{"abcdefg", 7, true},
		{"abcdefgh", 8, true},
		{"abcdefghi", 8, false}, // truncated to 8
		{"abcdefghijklmnop", 8, false},
	}
	for _, c := range cases {
		got := exchange.ShortKeyForTest(c.input)
		if len(got) != c.wantLen {
			t.Errorf("shortKey(%q): got len %d, want %d", c.input, len(got), c.wantLen)
		}
		if c.wantSame && got != c.input {
			t.Errorf("shortKey(%q): got %q, want same as input", c.input, got)
		}
	}
}

// TestInventory_ReturnsCopies verifies that the []*InventoryEntry returned by
// Inventory() are independent copies — mutating a returned entry must not
// affect state.
func TestInventory_ReturnsCopies(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	// Seed one inventory entry: put → put-accept.
	putMsg := h.sendMessage(h.seller,
		putPayload("Domains copy test entry", "sha256:"+fmt.Sprintf("%064x", 42), "code", 10000, 8192),
		[]string{exchange.TagPut, "exchange:content-type:code", "exchange:domain:go"},
		nil,
	)

	msgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	if err := eng.AutoAcceptPut(putMsg.ID, 7000, time.Now().Add(168*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut: %v", err)
	}

	// Get inventory and record the domains from the first call.
	inv1 := eng.State().Inventory()
	if len(inv1) != 1 {
		t.Fatalf("expected 1 inventory entry, got %d", len(inv1))
	}
	originalDomains := make([]string, len(inv1[0].Domains))
	copy(originalDomains, inv1[0].Domains)

	// Mutate the returned entry's Domains slice.
	inv1[0].Domains = append(inv1[0].Domains, "MUTATED")

	// Second Inventory() call must not see the mutation.
	inv2 := eng.State().Inventory()
	if len(inv2) != 1 {
		t.Fatalf("expected 1 inventory entry on second call, got %d", len(inv2))
	}
	if len(inv2[0].Domains) != len(originalDomains) {
		t.Errorf("internal Domains mutated by caller: got %v, want %v", inv2[0].Domains, originalDomains)
	}
	for i, d := range originalDomains {
		if inv2[0].Domains[i] != d {
			t.Errorf("Domains[%d] = %q, want %q", i, inv2[0].Domains[i], d)
		}
	}
}

// TestInventory_DomainsAreCopied is a white-box test in the internal package.
// See state_inventory_copy_test.go (internal package) for the actual assertion.

// TestResMu_ConcurrentGetReservation verifies that concurrent GetReservation
// calls do not deadlock now that resMu is RWMutex (multiple readers allowed).
// This test is in pkg/scrip/campfire_store_test.go where the store is accessible.
// See TestConcurrentGetReservation_NoDeadlock in that file.
func TestResMu_ConcurrentGetReservation(t *testing.T) {
	// resMu is in CampfireScripStore (pkg/scrip), not in the exchange package.
	// The concurrent read regression test lives in pkg/scrip/campfire_store_test.go
	// as TestConcurrentGetReservation_NoDeadlock, which exercises SaveReservation
	// and GetReservation from multiple goroutines simultaneously.
	t.Log("resMu RWMutex test: see TestConcurrentGetReservation_NoDeadlock in pkg/scrip/campfire_store_test.go")
}

// TestShortKey_ExactEight ensures an 8-char key is returned unchanged.
func TestShortKey_ExactEight(t *testing.T) {
	key := "12345678"
	got := exchange.ShortKeyForTest(key)
	if got != key {
		t.Errorf("shortKey(%q): got %q, want same", key, got)
	}
}

// TestShortKey_LongHexKey simulates a typical campfire key (64 hex chars).
func TestShortKey_LongHexKey(t *testing.T) {
	key := "a3f4e5d6b7c8901234567890abcdef1234567890abcdef1234567890abcdef12"
	got := exchange.ShortKeyForTest(key)
	if len(got) != 8 {
		t.Errorf("shortKey(64-char key): got len %d, want 8", len(got))
	}
	if got != key[:8] {
		t.Errorf("shortKey(64-char key): got %q, want %q", got, key[:8])
	}
}

// TestInventoryEntryCopyIndependence seeds a state with a real entry and
// verifies that mutations to the returned slice entries are not reflected
// in a subsequent Inventory() call. This specifically targets the Domains
// slice deep-copy (shallow struct copy would miss slice mutations).
func TestInventoryEntryCopyIndependence(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t)
	eng := h.newEngine()

	// Seed an inventory entry with multiple domains.
	putMsg := h.sendMessage(h.seller,
		putPayload("Copy independence test", "sha256:"+fmt.Sprintf("%064x", 77), "analysis", 5000, 4096),
		[]string{exchange.TagPut, "exchange:content-type:analysis", "exchange:domain:go", "exchange:domain:testing"},
		nil,
	)

	msgs, _ := h.st.ListMessages(h.cfID, 0)
	eng.State().Replay(exchange.FromStoreRecords(msgs))

	if err := eng.AutoAcceptPut(putMsg.ID, 3500, time.Now().Add(48*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut: %v", err)
	}

	// First call: capture domains, then corrupt the returned slice in-place.
	inv1 := eng.State().Inventory()
	if len(inv1) == 0 {
		t.Fatal("no inventory entry after put-accept")
	}
	// Record length before corruption.
	lenBefore := len(inv1[0].Domains)

	// Corrupt: append a sentinel and overwrite the first element.
	inv1[0].Domains = append(inv1[0].Domains, "CORRUPT")
	if len(inv1[0].Domains) > 0 {
		inv1[0].Domains[0] = "CORRUPT"
	}

	// Second call must return the original, unmodified domains.
	inv2 := eng.State().Inventory()
	if len(inv2) == 0 {
		t.Fatal("no inventory entry on second call")
	}
	if len(inv2[0].Domains) != lenBefore {
		t.Errorf("Domains length changed: got %d, want %d — internal state was mutated",
			len(inv2[0].Domains), lenBefore)
	}
	for _, d := range inv2[0].Domains {
		if d == "CORRUPT" {
			t.Errorf("internal Domains contains CORRUPT sentinel — copy not deep enough: %v", inv2[0].Domains)
			break
		}
	}
}

// Ensure ShortKeyForTest is used.
var _ = exchange.ShortKeyForTest
