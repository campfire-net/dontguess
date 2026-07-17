package exchange_test

// Enforcement proof for dontguess-4e3 finding B — RESOURCE-EXHAUSTION-DoS in the
// demand-only per-sender bookkeeping.
//
// pruneDemandWindow (dontguess-fd3 finding 2) bounds the LENGTH of ONE sender
// key's timestamp slice, but NOT the NUMBER of sender keys. A Sybil that cycles a
// FRESH sender key on every registration leaves each key at count=1 (below
// DemandOnlyPerSenderCap, so the per-sender cap never fires and never revisits the
// key). Under the pre-fix code that key is written once and never deleted, so
// demandOnlySenderTimes grows one permanent key per distinct unfunded sender —
// unbounded across process life and re-grown whole on every Replay.
//
// The fix sweeps sender keys whose newest timestamp is older than one
// DemandOnlyPerSenderWindow on the SAME event-timestamp-driven eviction pass the
// task-hash TTL uses. Every assertion below drives REAL State.Apply (no mock of the
// thing under test) and uses a CONTROLLABLE CLOCK — the folded event timestamps —
// instead of a real sleep, so eviction is proven deterministically.

import (
	"testing"
	"time"

	"github.com/3dl-dev/dontguess/pkg/exchange"
)

// TestDemandOnly_SybilFreshKeyChurnLeavesSenderKeyMapBounded is the headline
// memory-bound proof. A Sybil folds N demand-only registrations, EACH under a
// distinct fresh sender key, EACH spaced more than one DemandOnlyPerSenderWindow
// past the last (so no window ever overlaps two keys). The sender-key map must NOT
// grow to N: the eviction sweep on each fold must drop every key whose window has
// elapsed, leaving only the small live tail. Under the pre-fix code the map would
// hold all N permanently.
func TestDemandOnly_SybilFreshKeyChurnLeavesSenderKeyMapBounded(t *testing.T) {
	t.Parallel()
	st := exchange.NewState() // OperatorKey "" → applyMatch operator guard disabled

	const n = 500
	spacing := 2 * exchange.DemandOnlyPerSenderWindow // each fold is beyond the window
	base := time.Now().Add(-time.Duration(n) * spacing)

	for i := 0; i < n; i++ {
		ts := base.Add(time.Duration(i) * spacing).UnixNano()
		exp := time.Unix(0, ts).Add(exchange.DemandOnlyTTL)
		// A FRESH sender key AND a FRESH task hash per registration — the exact Sybil
		// shape that dodges both the per-sender cap (count stays 1) and the task-hash
		// dedup (every hash is unique).
		sender := "sybil-" + hexID(i)
		st.Apply(craftDemandOnly(hexID(i), "", sender, "hash-"+hexID(i), exp, ts))
	}

	// Only sender keys within one window of the FINAL fold can survive. With spacing
	// = 2×window, that is at most the single last key. The pre-fix map would hold N.
	got := st.DemandOnlySenderKeyCountForTest()
	if got > 2 {
		t.Fatalf("demandOnlySenderTimes retained %d sender keys after %d fresh-key registrations spaced beyond the window — the per-sender-key map is UNBOUNDED (finding B DoS); want a small live tail (≤2)", got, n)
	}
}

// TestDemandOnly_SenderKeySweptOnLaterEventTimestamp isolates the eviction on a
// deterministic clock: two sender keys are folded far apart in event time, then a
// third fold whose timestamp is past BOTH prior windows must physically evict them.
// Proof is by physical map size (the ForTest accessor reads len directly), not a
// read-side filter — so a surviving-but-filtered key would still fail this.
func TestDemandOnly_SenderKeySweptOnLaterEventTimestamp(t *testing.T) {
	t.Parallel()
	st := exchange.NewState()

	t0 := time.Now()
	// Two distinct senders, each one window apart — after the second fold both are
	// still within-or-adjacent, but crucially each is a distinct key.
	tsA := t0.UnixNano()
	tsB := t0.Add(2 * exchange.DemandOnlyPerSenderWindow).UnixNano()
	st.Apply(craftDemandOnly(hexID(1), "", "sender-A", "hash-A", t0.Add(exchange.DemandOnlyTTL), tsA))
	st.Apply(craftDemandOnly(hexID(2), "", "sender-B", "hash-B",
		time.Unix(0, tsB).Add(exchange.DemandOnlyTTL), tsB))

	// A fresh fold whose timestamp is past EVERY prior sender window.
	tsC := t0.Add(10 * exchange.DemandOnlyPerSenderWindow).UnixNano()
	st.Apply(craftDemandOnly(hexID(3), "", "sender-C", "hash-C",
		time.Unix(0, tsC).Add(exchange.DemandOnlyTTL), tsC))

	// A and B are past their window relative to tsC → swept. Only C survives.
	if got := st.DemandOnlySenderKeyCountForTest(); got != 1 {
		t.Fatalf("sender-key count after sweep = %d, want 1 (only the live sender-C) — stale sender keys were not physically evicted (finding B memory leak)", got)
	}
	// The surviving key is still live and countable within its own window.
	if c := st.DemandOnlyCountForSender("sender-C", time.Unix(0, tsC), exchange.DemandOnlyPerSenderWindow); c != 1 {
		t.Fatalf("DemandOnlyCountForSender(sender-C) = %d, want 1 — the live sender was wrongly swept", c)
	}
}

// TestDemandOnly_LiveSenderCapPreservedAcrossSweep is the control: the sweep must
// NOT weaken the per-sender cap for a sender that keeps registering WITHIN its
// window. A single sender folds DemandOnlyPerSenderCap registrations packed inside
// one window; its count must remain at the cap (its key is never swept, because its
// newest timestamp is always the current event).
func TestDemandOnly_LiveSenderCapPreservedAcrossSweep(t *testing.T) {
	t.Parallel()
	st := exchange.NewState()

	const sender = "busy-live-sender"
	base := time.Now()
	// Pack DemandOnlyPerSenderCap folds inside a fraction of one window so all count.
	step := exchange.DemandOnlyPerSenderWindow / (exchange.DemandOnlyPerSenderCap * 4)
	var lastTs int64
	for i := 0; i < exchange.DemandOnlyPerSenderCap; i++ {
		ts := base.Add(time.Duration(i) * step).UnixNano()
		lastTs = ts
		st.Apply(craftDemandOnly(hexID(i), "", sender, "hash-"+hexID(i),
			time.Unix(0, ts).Add(exchange.DemandOnlyTTL), ts))
	}

	// The live sender key is retained (not swept) and its full in-window count stands.
	if got := st.DemandOnlySenderKeyCountForTest(); got != 1 {
		t.Fatalf("sender-key count = %d, want 1 — the live sender's key must survive the sweep", got)
	}
	if c := st.DemandOnlyCountForSender(sender, time.Unix(0, lastTs), exchange.DemandOnlyPerSenderWindow); c != exchange.DemandOnlyPerSenderCap {
		t.Fatalf("DemandOnlyCountForSender = %d, want %d — the per-sender cap was weakened by the sweep", c, exchange.DemandOnlyPerSenderCap)
	}
}
