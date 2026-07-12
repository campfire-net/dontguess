package exchange

// wire_alias_test.go — white-box coverage for the dontguess-55c GAP-1 wire→store
// alias. It proves the two properties State.Replay + the accessors must hold:
//
//  1. resolveAlias-survives-Replay: a full State.Replay resets every log-derived
//     index but NOT wireToStore, so a resolver that can only reach a match via the
//     WIRE id still resolves after a rebuild (the restart-determinism invariant).
//  2. The alias is LOAD-BEARING: without it the wire id resolves to nothing, so
//     the assertion in (1) is not passing by some incidental path.

import (
	"encoding/json"
	"testing"
)

func TestResolveAlias_SurvivesReplay(t *testing.T) {
	const (
		buyID      = "storebuy00000000000000000000000000000000000000000000000000000000"
		matchStore = "storematch000000000000000000000000000000000000000000000000000000"
		matchWire  = "wirematch0000000000000000000000000000000000000000000000000000000"
		buyerKey   = "buyerkeyhex000000000000000000000000000000000000000000000000000000"
	)

	buyPayload, _ := json.Marshal(map[string]any{"task": "generate go http handler tests", "budget": int64(1000)})
	matchPayload, _ := json.Marshal(map[string]any{"results": []map[string]any{{"entry_id": "e1"}}})

	// The persisted operator log: a buy, then the operator match keyed by its STORE
	// id (as sendLocalOperatorMessage keys it). OperatorKey is "" on a bare NewState,
	// so applyMatch does not gate on the match sender.
	log := []Message{
		{ID: buyID, Sender: buyerKey, Tags: []string{TagBuy}, Payload: buyPayload},
		{ID: matchStore, Tags: []string{TagMatch}, Antecedents: []string{buyID}, Payload: matchPayload},
	}

	st := NewState()
	// Register the wire→store alias exactly as the Outbox would at publish time.
	st.RegisterWireAlias(matchWire, matchStore)
	// Full replay: resets matchToBuyer (and every other log-derived index) and
	// re-folds the log. wireToStore MUST survive.
	st.Replay(log)

	// The match resolves via the WIRE id after Replay — the alias was not wiped and
	// the accessor prepends resolveAlias.
	gotMatch, gotBuyer, found := st.ResolveMatchFromAntecedent(matchWire)
	if !found {
		t.Fatalf("wire-id antecedent did not resolve after Replay — the alias was wiped by Replay")
	}
	if gotMatch != matchStore {
		t.Fatalf("resolved match id = %q, want store id %q", gotMatch, matchStore)
	}
	if gotBuyer != buyerKey {
		t.Fatalf("resolved buyer = %q, want %q", gotBuyer, buyerKey)
	}

	// LOAD-BEARING control: a fresh state that folds the SAME log but never
	// registered the alias cannot resolve the wire id — proving (1) succeeds ONLY
	// because of the surviving alias, not via matchToBuyer directly.
	noAlias := NewState()
	noAlias.Replay(log)
	if _, _, found := noAlias.ResolveMatchFromAntecedent(matchWire); found {
		t.Fatalf("wire id resolved WITHOUT a registered alias — the test is not load-bearing")
	}
	// Sanity: the store id resolves in the no-alias state via the identity path, so
	// the log fold itself is sound (only the wire id needs the alias).
	if _, _, found := noAlias.ResolveMatchFromAntecedent(matchStore); !found {
		t.Fatalf("store-id antecedent did not resolve via the identity path — log fold is broken")
	}
}

// TestRegisterWireAlias_IdempotentAndCollisionSafe pins the RegisterWireAlias
// contract the money path relies on: a re-register of the same pair is a no-op,
// and a DIFFERENT store id for an already-mapped wire id NEVER overwrites the
// first mapping (a hostile/buggy re-derivation cannot repoint a live match alias).
func TestRegisterWireAlias_IdempotentAndCollisionSafe(t *testing.T) {
	st := NewState()
	st.RegisterWireAlias("wireA", "storeA")
	st.RegisterWireAlias("wireA", "storeA") // idempotent re-register
	st.RegisterWireAlias("wireA", "storeB") // collision: must be refused, first kept

	st.mu.RLock()
	got := st.resolveAlias("wireA")
	st.mu.RUnlock()
	if got != "storeA" {
		t.Fatalf("wireA resolved to %q after a collision attempt, want the first mapping storeA", got)
	}

	// Degenerate inputs are ignored (never a panic, never a self-alias entry).
	st.RegisterWireAlias("", "storeX")
	st.RegisterWireAlias("wireY", "")
	st.RegisterWireAlias("same", "same")
	st.mu.RLock()
	defer st.mu.RUnlock()
	if len(st.wireToStore) != 1 {
		t.Fatalf("wireToStore has %d entries, want exactly 1 (degenerate inputs must not register)", len(st.wireToStore))
	}
}
