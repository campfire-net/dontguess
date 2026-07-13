package exchange_test

// Property tests for the operator-side sequencer (dontguess-50d, M2), driven
// through the REAL exchange fold — not a hand-asserted expected order.
//
// The invariant under test (design docs/design/nostr-first-rebuild-decision.md
// §Sequencer per domain): for any permutation and/or duplication of a fixed,
// causally-closed event set, the sequencer produces the byte-identical fold
// order, so the folded State and its Layer 0-4 metrics are BYTE-IDENTICAL. With
// a broken causal closure (a pruned antecedent) the sequenced replay FAILS LOUD
// rather than folding a silently-truncated chain.
//
// Method: build a realistic causal event set with the real harness (real puts,
// real operator-signed put-accepts whose antecedent e-tags point back at the
// puts, a real buy) — no synthetic/hand-built messages, no mocked store. Then
// feed randomized permutations + duplicate deliveries of that set through:
//   (1) exchange.SequenceForFold + a fresh exchange.State.Replay (the fold), and
//   (2) a real Engine with SequencedIngest=true over a real pkg/store.Store
//       (the actual engine ingest path, replayAllLocal),
// and assert the resulting State snapshot is byte-identical to a canonical-order
// reference every time.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/campfire-net/dontguess/pkg/exchange"
	dgstore "github.com/campfire-net/dontguess/pkg/store"
)

// buildCausalLog produces a realistic, causally-closed exchange event set:
// two put→put-accept chains (the put-accept carries the put's ID as its
// antecedent e-tag, the exact edge nostr can deliver out of order) plus one
// buy. Returned in canonical (append/causal) order. Also returns the operator
// key so the reference/engine folds derive identical operator-sender state.
func buildCausalLog(t *testing.T) (msgs []exchange.Message, operatorKey string) {
	t.Helper()
	h := newTestHarness(t)
	eng := h.newEngine()

	put1 := h.sendMessage(h.seller,
		putPayload("Go HTTP handler unit test generator", "sha256:"+fmt.Sprintf("%064x", 1), "code", 12000, 20000),
		[]string{exchange.TagPut, "exchange:content-type:code", "exchange:domain:go"}, nil)
	if err := eng.AutoAcceptPut(put1.ID, 8400, time.Now().Add(168*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut(put1): %v", err)
	}

	put2 := h.sendMessage(h.seller,
		putPayload("Rust async mutex deadlock explainer", "sha256:"+fmt.Sprintf("%064x", 2), "analysis", 9000, 15000),
		[]string{exchange.TagPut, "exchange:content-type:analysis", "exchange:domain:rust"}, nil)
	if err := eng.AutoAcceptPut(put2.ID, 6300, time.Now().Add(168*time.Hour)); err != nil {
		t.Fatalf("AutoAcceptPut(put2): %v", err)
	}

	h.sendMessage(h.buyer,
		buyPayload("Generate unit tests for a Go HTTP handler accepting JSON POST", 50000),
		[]string{exchange.TagBuy}, nil)

	// The LocalStore now holds every event (puts + operator put-accepts mirrored
	// by sendOperatorMessage + buy) in causal append order. Read it back as the
	// canonical event set.
	canonical, err := h.localStore.Replay()
	if err != nil {
		t.Fatalf("localStore.Replay: %v", err)
	}
	if len(canonical) < 5 {
		t.Fatalf("expected >=5 events (put1, accept1, put2, accept2, buy), got %d", len(canonical))
	}
	return canonical, h.operator.pubKeyHex
}

// stateSnapshot serializes the folded state and its Layer 0-4 metrics into a
// deterministic byte string. Slice-valued accessors are sorted by their stable
// ID so the snapshot depends only on state content, never on map iteration
// order — making "byte-identical" a meaningful assertion.
func stateSnapshot(t *testing.T, s *exchange.State, sellerKeys []string) []byte {
	t.Helper()

	inv := s.Inventory()
	sort.Slice(inv, func(i, j int) bool { return inv[i].EntryID < inv[j].EntryID })
	pending := s.PendingPuts()
	sort.Slice(pending, func(i, j int) bool { return pending[i].EntryID < pending[j].EntryID })
	orders := s.ActiveOrders()
	sort.Slice(orders, func(i, j int) bool { return orders[i].OrderID < orders[j].OrderID })
	held := s.PutsHeldForReview()
	sort.Slice(held, func(i, j int) bool { return held[i].EntryID < held[j].EntryID })

	rep := map[string]int{}
	for _, k := range sellerKeys {
		rep[k] = s.SellerReputation(k)
	}

	snap := struct {
		Inventory   []*exchange.InventoryEntry
		PendingPuts []*exchange.InventoryEntry
		ActiveOrder []*exchange.ActiveOrder
		Held        []*exchange.InventoryEntry
		Reputation  map[string]int
		// Layer 0 correctness gate + related completion metrics — these are
		// pure functions of the folded log, so byte-identity here follows from
		// a byte-identical fold order.
		Layer0TaskCompletionRate float64
		CombinedCompletionRate   float64
		BrokeredCompletionRate   float64
	}{
		Inventory:                inv,
		PendingPuts:              pending,
		ActiveOrder:              orders,
		Held:                     held,
		Reputation:               rep,
		Layer0TaskCompletionRate: s.TaskCompletionRate(),
		CombinedCompletionRate:   s.CombinedCompletionRate(),
		BrokeredCompletionRate:   s.BrokeredMatchCompletionRate(),
	}
	b, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	return b
}

// perturb returns a copy of msgs in a seeded-random order, with a few random
// duplicate deliveries spliced in — the two failure modes nostr multi-relay
// ingest produces (reordering + duplicate delivery from multiple relays).
func perturb(msgs []exchange.Message, seed int64) []exchange.Message {
	rng := rand.New(rand.NewSource(seed))
	out := make([]exchange.Message, len(msgs))
	copy(out, msgs)
	rng.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	// Splice in up to 3 duplicate deliveries at random positions.
	dupCount := rng.Intn(4)
	for d := 0; d < dupCount; d++ {
		src := out[rng.Intn(len(out))]
		pos := rng.Intn(len(out) + 1)
		out = append(out, exchange.Message{})
		copy(out[pos+1:], out[pos:])
		out[pos] = src
	}
	return out
}

func TestSequencer_Property_ByteIdenticalFoldUnderReorderAndDups(t *testing.T) {
	canonical, _ := buildCausalLog(t)
	sellerKeys := sendersOf(canonical)

	// Reference: fold the canonical causal order directly.
	ref := exchange.NewState()
	ref.Replay(canonical)
	refSnap := stateSnapshot(t, ref, sellerKeys)

	// Sequencing an already-canonical log must be order-preserving (a no-op on
	// order) and fold identically.
	seqCanonical, err := exchange.SequenceForFold(canonical, 0)
	if err != nil {
		t.Fatalf("SequenceForFold(canonical): %v", err)
	}
	stCanon := exchange.NewState()
	stCanon.Replay(seqCanonical)
	if got := stateSnapshot(t, stCanon, sellerKeys); !bytes.Equal(got, refSnap) {
		t.Fatalf("sequencing the canonical log changed the fold:\n ref=%s\n got=%s", refSnap, got)
	}

	// Property: any permutation + duplicate delivery folds byte-identically, and
	// the SEQUENCED ORDER itself is identical across permutations.
	var wantOrder []string
	for _, m := range seqCanonical {
		wantOrder = append(wantOrder, m.ID)
	}
	const iterations = 200
	for seed := int64(1); seed <= iterations; seed++ {
		perm := perturb(canonical, seed)
		ordered, err := exchange.SequenceForFold(perm, 0)
		if err != nil {
			t.Fatalf("seed %d: SequenceForFold: %v", seed, err)
		}
		// Sequenced order is byte-identical to the canonical sequencing.
		if len(ordered) != len(wantOrder) {
			t.Fatalf("seed %d: sequenced %d events, want %d", seed, len(ordered), len(wantOrder))
		}
		for i := range wantOrder {
			if ordered[i].ID != wantOrder[i] {
				t.Fatalf("seed %d: sequenced order diverged at %d: got %s want %s",
					seed, i, ordered[i].ID, wantOrder[i])
			}
		}
		// Folded state is byte-identical to the reference.
		st := exchange.NewState()
		st.Replay(ordered)
		if got := stateSnapshot(t, st, sellerKeys); !bytes.Equal(got, refSnap) {
			t.Fatalf("seed %d: folded state diverged from reference:\n ref=%s\n got=%s", seed, refSnap, got)
		}
	}
}

// TestSequencer_Property_ClosedBatchBelowMaxOrphansStillFolds is the
// dontguess-e181 regression driven through the REAL fold. It takes the same
// realistic causally-closed set (two put→put-accept edges + a buy) and delivers
// it in the permutation that maximizes concurrent orphans — every put-accept
// BEFORE its put — then sequences it with maxOrphans = 1, deliberately below the
// orphan peak (2, one per put-accept edge). BEFORE the capacity-widening fix this
// aborted mid-load with ErrOrphanBufferOverflow the instant the second put-accept
// pushed the true-orphan count to 2 over the bound of 1, even though the whole
// set is resident and closes cleanly. AFTER the fix it must sequence to the
// byte-identical fold order the canonical delivery produces — proving the widened
// batch path is not just non-erroring but still deterministic.
func TestSequencer_Property_ClosedBatchBelowMaxOrphansStillFolds(t *testing.T) {
	canonical, _ := buildCausalLog(t)
	sellerKeys := sendersOf(canonical)

	// Reference: canonical-order sequencing (huge bound → never near it).
	ref, err := exchange.SequenceForFold(canonical, 0)
	if err != nil {
		t.Fatalf("SequenceForFold(canonical): %v", err)
	}
	refState := exchange.NewState()
	refState.Replay(ref)
	refSnap := stateSnapshot(t, refState, sellerKeys)

	// Front-load every dependent (an event whose antecedent is another event in
	// the set) ahead of the events it depends on — the worst case for orphan
	// occupancy. Stable within each group so the order is deterministic.
	idset := make(map[string]struct{}, len(canonical))
	for _, m := range canonical {
		idset[m.ID] = struct{}{}
	}
	var dependents, roots []exchange.Message
	for _, m := range canonical {
		isDep := false
		for _, a := range m.Antecedents {
			if _, ok := idset[a]; ok {
				isDep = true
				break
			}
		}
		if isDep {
			dependents = append(dependents, m)
		} else {
			roots = append(roots, m)
		}
	}
	if len(dependents) < 2 {
		t.Fatalf("test needs >=2 dependents to exceed maxOrphans=1; got %d", len(dependents))
	}
	frontLoaded := append(append([]exchange.Message{}, dependents...), roots...)

	// maxOrphans=1 is strictly below the orphan peak (>=2) of this permutation.
	ordered, err := exchange.SequenceForFold(frontLoaded, 1)
	if err != nil {
		t.Fatalf("SequenceForFold(front-loaded, maxOrphans=1): unexpected error: %v (the capacity-widening fix must let a resident closed batch through even below the caller's bound)", err)
	}
	if len(ordered) != len(canonical) {
		t.Fatalf("sequenced %d events, want %d", len(ordered), len(canonical))
	}
	for i := range ref {
		if ordered[i].ID != ref[i].ID {
			t.Fatalf("sequenced order diverged at %d: got %s want %s (must equal canonical fold order)", i, ordered[i].ID, ref[i].ID)
		}
	}
	st := exchange.NewState()
	st.Replay(ordered)
	if got := stateSnapshot(t, st, sellerKeys); !bytes.Equal(got, refSnap) {
		t.Fatalf("front-loaded low-bound fold diverged from canonical reference:\n ref=%s\n got=%s", refSnap, got)
	}
}

// TestSequencer_Property_EngineIngestByteIdentical drives the property through
// the REAL engine ingest path: a fresh Engine with SequencedIngest=true over a
// real pkg/store.Store loaded with a permuted+duped event set must fold to the
// byte-identical State a canonical-order (unsequenced) engine produces.
func TestSequencer_Property_EngineIngestByteIdentical(t *testing.T) {
	canonical, operatorKey := buildCausalLog(t)
	sellerKeys := sendersOf(canonical)

	// Reference engine: canonical order, plain M1 append-order fold.
	refState := replayThroughEngine(t, canonical, operatorKey, false)
	refSnap := stateSnapshot(t, refState, sellerKeys)

	const iterations = 40
	for seed := int64(1); seed <= iterations; seed++ {
		perm := perturb(canonical, seed)
		got := replayThroughEngine(t, perm, operatorKey, true)
		if snap := stateSnapshot(t, got, sellerKeys); !bytes.Equal(snap, refSnap) {
			t.Fatalf("seed %d: engine sequenced-ingest fold diverged from canonical reference:\n ref=%s\n got=%s",
				seed, refSnap, snap)
		}
	}
}

// TestSequencer_Property_EngineIngestPrunedAntecedentFailsLoud proves the engine
// ingest path itself fails loud (never a silent truncated fold) when the
// persisted log has a broken causal closure.
func TestSequencer_Property_EngineIngestPrunedAntecedentFailsLoud(t *testing.T) {
	canonical, operatorKey := buildCausalLog(t)

	// Drop a root put so its put-accept's antecedent e-tag is unrecoverable.
	broken := dropFirstPut(t, canonical)

	dir := t.TempDir()
	ls, err := dgstore.Open(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatalf("dgstore.Open: %v", err)
	}
	defer ls.Close() //nolint:errcheck
	appendAll(t, ls, broken)

	eng := exchange.NewEngine(exchange.EngineOptions{
		CampfireID:        "local",
		LocalStore:        ls,
		OperatorPublicKey: operatorKey,
		SequencedIngest:   true,
	})
	err = eng.ReplayAllForTest()
	if err == nil {
		t.Fatal("engine replay with a pruned antecedent returned nil; must fail loud")
	}
	if !errors.Is(err, exchange.ErrUnrecoverableAntecedent) {
		t.Fatalf("replay error = %v, want ErrUnrecoverableAntecedent", err)
	}
}

// replayThroughEngine loads msgs into a fresh real pkg/store.Store, builds an
// Engine (optionally with SequencedIngest), runs the real replay fold, and
// returns the resulting State.
func replayThroughEngine(t *testing.T, msgs []exchange.Message, operatorKey string, sequenced bool) *exchange.State {
	t.Helper()
	dir := t.TempDir()
	ls, err := dgstore.Open(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatalf("dgstore.Open: %v", err)
	}
	t.Cleanup(func() { ls.Close() }) //nolint:errcheck
	appendAll(t, ls, msgs)

	eng := exchange.NewEngine(exchange.EngineOptions{
		CampfireID:        "local",
		LocalStore:        ls,
		OperatorPublicKey: operatorKey,
		SequencedIngest:   sequenced,
	})
	if err := eng.ReplayAllForTest(); err != nil {
		t.Fatalf("ReplayAllForTest(sequenced=%v): %v", sequenced, err)
	}
	return eng.State()
}

func appendAll(t *testing.T, ls *dgstore.Store, msgs []exchange.Message) {
	t.Helper()
	for i := range msgs {
		m := msgs[i]
		if err := ls.Append(dgstore.Record{
			ID:          m.ID,
			CampfireID:  m.CampfireID,
			Sender:      m.Sender,
			Payload:     m.Payload,
			Tags:        m.Tags,
			Antecedents: m.Antecedents,
			Timestamp:   m.Timestamp,
			Instance:    m.Instance,
		}); err != nil {
			t.Fatalf("append %s: %v", m.ID, err)
		}
	}
}

// dropFirstPut returns msgs with the first exchange:put removed, breaking the
// causal closure for its put-accept.
func dropFirstPut(t *testing.T, msgs []exchange.Message) []exchange.Message {
	t.Helper()
	out := make([]exchange.Message, 0, len(msgs))
	dropped := false
	for _, m := range msgs {
		if !dropped && hasTag(m.Tags, exchange.TagPut) {
			dropped = true
			continue
		}
		out = append(out, m)
	}
	if !dropped {
		t.Fatal("no exchange:put found to drop")
	}
	return out
}

// sendersOf returns the sorted, deduplicated set of sender keys in the log so
// the snapshot's per-seller reputation covers every participant.
func sendersOf(msgs []exchange.Message) []string {
	set := map[string]struct{}{}
	for _, m := range msgs {
		if m.Sender != "" {
			set[m.Sender] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
