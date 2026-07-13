package exchange

// Tests for dontguess-1856: griefer-aware weighting of the false-positive-expiry
// signal (dontguess-046's ExpiryCandidates / entryDeliverCount vs. entryConsumeCount
// ratio).
//
// Problem: ExpiryCandidates previously treated an entry's deliver-without-consume
// (dwc) ratio identically whether the abandoning buyers were a small number of
// chronic never-completers (a funded griefer) or a broad set of otherwise-healthy
// buyers who each abandoned once. The item spec states the design intent twice,
// unambiguously:
//
//	"add a weighting function that scales an entry's effective dwc ratio DOWN
//	 when the buyers who abandoned it have a low personal completion rate (i.e.
//	 don't let one griefer poison an otherwise-good entry's false-positive-expiry
//	 signal)"
//
// i.e. an entry hammered by a chronic near-zero-completion griefer should have
// its effective ratio scaled DOWN (less likely to be flagged — that pattern is
// noise from a bad actor, not signal about entry quality). An entry abandoned by
// a broad set of buyers who otherwise complete normally elsewhere keeps its
// ratio at full strength (that pattern IS real signal the entry is stale/bad).
//
// NOTE: the item's "DONE when" sentence describes the two entries as "ONLY the
// griefer-driven entry demotes/flags" — that phrasing is inverted relative to
// the design rationale stated twice in the same item's Task paragraph. This test
// (and the implementation in state_behavioral.go's entryDeliverAbandonWeightLocked)
// follows the twice-stated design rationale: the entry abandoned by low-completion-
// rate (griefer) buyers must NOT flag, and the entry abandoned by otherwise-healthy
// buyers DOES flag. Flagged in the return summary for review.
//
// The weighting is EXTERNAL (excludes the entry being judged from a buyer's
// personal-rate computation) rather than a simple global/inclusive rate. This
// was required to keep pkg/exchange's pre-existing dontguess-659 griefing
// regression green (TestGriefing_DeliverWithoutConsume_CannotRemoveOrDerankCompetitor),
// which asserts that a SINGLE funded attacker abandoning ONE entry (no track
// record elsewhere) must still surface as an operator-facing expiry candidate
// — flagging itself is harmless/advisory-only; the real mitigation there is
// the pre-existing floor-gate bound, not signal suppression. Only a buyer with
// an ESTABLISHED chronic-never-completes pattern on OTHER entries should
// discount the entry under evaluation.
//
// White-box tests (package exchange) — same seeding pattern as
// false_positive_signals_test.go: seedDeliverChain + Apply for delivers, direct
// settle(complete) message construction for consumes.

import (
	"encoding/json"
	"testing"
	"time"
)

// seedCompleteMsg builds a settle(complete) message whose antecedent is
// deliverMsgID, sent by buyerKey. Mirrors the payload shape used across the
// existing settle-complete tests (see state_settle_complete_idempotent_test.go).
func seedCompleteMsg(completeMsgID, deliverMsgID, buyerKey string, price int64) *Message {
	payload, _ := json.Marshal(map[string]any{
		"phase": SettlePhaseStrComplete,
		"price": price,
	})
	return &Message{
		ID:          completeMsgID,
		Sender:      buyerKey,
		Tags:        []string{TagSettle, TagPhasePrefix + SettlePhaseStrComplete},
		Payload:     payload,
		Antecedents: []string{deliverMsgID},
		Timestamp:   time.Now().UnixNano(),
	}
}

// deliverAndComplete drives a full deliver->complete chain for one buyer against
// one entry, via real Apply() calls (ground-source: exercises applySettleDeliver
// and applySettleComplete exactly as production traffic would).
func deliverAndComplete(t *testing.T, st *State, idPrefix, entryID, buyerKey string) {
	t.Helper()
	matchID := idPrefix + "-match"
	baID := idPrefix + "-ba"
	deliverID := idPrefix + "-deliver"
	completeID := idPrefix + "-complete"

	deliverMsg := seedDeliverChain(st, matchID, entryID, baID, deliverID, buyerKey)
	deliverMsg.Sender = st.OperatorKey
	st.Apply(deliverMsg)

	completeMsg := seedCompleteMsg(completeID, deliverID, buyerKey, 1000)
	st.Apply(completeMsg)
}

// deliverOnly drives just a settle(deliver) (no complete) for one buyer against
// one entry — an abandoned delivery.
func deliverOnly(t *testing.T, st *State, idPrefix, entryID, buyerKey string) {
	t.Helper()
	matchID := idPrefix + "-match"
	baID := idPrefix + "-ba"
	deliverID := idPrefix + "-deliver"

	deliverMsg := seedDeliverChain(st, matchID, entryID, baID, deliverID, buyerKey)
	deliverMsg.Sender = st.OperatorKey
	st.Apply(deliverMsg)
}

// TestEntryDeliverAbandonWeight_GrieferVsHealthyBuyers is the core proof: two
// entries each receive 6 raw abandoned deliveries against entry-griefer /
// entry-healthy respectively (deliver, never consumed) — identical raw dwc
// signal (deliver=6, consume=0, raw ratio=6.0, comfortably past
// FalsePositiveWindowMin=3 and FalsePositiveRatioThreshold=5.0). The ONLY
// difference is who the abandoning buyers are:
//
//   - entry-healthy: 6 distinct buyers, each with a 9-delivery/9-complete
//     EXTERNAL history (on private per-buyer warm-up entries, rate 1.0)
//     BEFORE abandoning entry-healthy once each.
//   - entry-griefer: 1 buyer with an ESTABLISHED EXTERNAL chronic
//     near-zero-completion track record (10 abandoned deliveries against two
//     OTHER "prior-victim" entries, zero completes ever) BEFORE also
//     abandoning entry-griefer 6 times.
//
// The external (excluding-this-entry) framing is deliberate: it is what
// distinguishes this scenario from dontguess-659's single-episode griefing
// regression (TestGriefing_DeliverWithoutConsume_CannotRemoveOrDerankCompetitor),
// where a funded attacker abandons ONE entry with no track record elsewhere —
// that must still flag (see TestEntryDeliverAbandonWeight_SingleEpisodeNoExternalHistoryStillFlags
// below). Only an established pattern OUTSIDE the entry being judged
// discounts that entry's signal.
//
// Per the design rationale (stated twice in the item spec): the
// established-griefer-driven entry's effective dwc ratio must be scaled down
// enough to drop it below the false-positive-expiry criterion (it is noise
// from a known bad actor, not signal about entry quality); the
// healthy-buyers-driven entry's ratio must stay at (near) full strength and
// still flag (broad abandonment by normally-reliable buyers IS real signal).
func TestEntryDeliverAbandonWeight_GrieferVsHealthyBuyers(t *testing.T) {
	t.Parallel()

	st := NewState()
	st.OperatorKey = "operator-key-hex"

	const entryHealthy = "entry-healthy-buyers"
	const entryGriefer = "entry-griefer"

	// 6 healthy buyers: each completes 9 warm-up transactions on a PRIVATE
	// per-buyer entry (external rate 1.0), THEN abandons entry-healthy once.
	for i := 0; i < 6; i++ {
		buyer := "buyer-healthy-" + string(rune('A'+i))
		for w := 0; w < 9; w++ {
			idPrefix := buyer + "-warmup-" + string(rune('0'+w))
			warmupEntry := "entry-warmup-" + buyer
			deliverAndComplete(t, st, idPrefix, warmupEntry, buyer)
		}
		deliverOnly(t, st, buyer+"-abandon", entryHealthy, buyer)
	}

	// 1 griefer buyer: an ESTABLISHED external track record of 10 abandoned
	// deliveries (zero completes) spread across two OTHER "prior-victim"
	// entries, THEN 6 more abandoned deliveries against entry-griefer.
	const griefer = "buyer-griefer"
	for i := 0; i < 5; i++ {
		deliverOnly(t, st, griefer+"-priorvictim1-"+string(rune('0'+i)), "entry-prior-victim-1", griefer)
	}
	for i := 0; i < 5; i++ {
		deliverOnly(t, st, griefer+"-priorvictim2-"+string(rune('0'+i)), "entry-prior-victim-2", griefer)
	}
	for i := 0; i < 6; i++ {
		idPrefix := griefer + "-abandon-" + string(rune('0'+i))
		deliverOnly(t, st, idPrefix, entryGriefer, griefer)
	}

	// Sanity: raw signal on the two entries under test is identical (and
	// would, unweighted, flag both).
	st.mu.RLock()
	rawHealthy := st.entryDeliverCount[entryHealthy]
	rawGriefer := st.entryDeliverCount[entryGriefer]
	st.mu.RUnlock()
	if rawHealthy != 6 {
		t.Fatalf("entry-healthy raw deliver count = %d, want 6", rawHealthy)
	}
	if rawGriefer != 6 {
		t.Fatalf("entry-griefer raw deliver count = %d, want 6", rawGriefer)
	}

	// Direct unit check of the weighting function itself.
	st.mu.RLock()
	weightHealthy := st.entryDeliverAbandonWeightLocked(entryHealthy)
	weightGriefer := st.entryDeliverAbandonWeightLocked(entryGriefer)
	st.mu.RUnlock()
	if weightHealthy != 1.0 {
		t.Errorf("entry-healthy abandon weight = %f, want 1.0 (buyers' external completion rate is perfect)", weightHealthy)
	}
	if weightGriefer != 0.0 {
		t.Errorf("entry-griefer abandon weight = %f, want 0.0 (buyer's EXTERNAL track record is 10 abandons / 0 completes)", weightGriefer)
	}

	// The actual behavior under test: ExpiryCandidates() must flag ONLY the
	// entry abandoned by otherwise-healthy buyers, not the established-
	// griefer-driven one.
	candidates := st.ExpiryCandidates()

	foundHealthy := false
	foundGriefer := false
	for _, c := range candidates {
		if c.EntryID == entryHealthy {
			foundHealthy = true
		}
		if c.EntryID == entryGriefer {
			foundGriefer = true
		}
	}

	if !foundHealthy {
		t.Errorf("entry-healthy-buyers (abandoned by 6 otherwise-reliable buyers) must appear in ExpiryCandidates — real signal, not griefing")
	}
	if foundGriefer {
		t.Errorf("entry-griefer (abandoned by a buyer with an established chronic-never-completes track record elsewhere) must NOT appear in ExpiryCandidates — griefer weighting must suppress the false positive")
	}
}

// TestEntryDeliverAbandonWeight_SingleEpisodeNoExternalHistoryStillFlags is
// the companion regression proof: a buyer who abandons ONE entry repeatedly,
// with NO track record on any other entry (the dontguess-659 griefing
// scenario — a funded attacker with a single-episode campaign against one
// competitor entry), must still flag normally. There is no external evidence
// to distinguish this buyer from a legitimate but unlucky one-off; the
// weighting function must default to neutral (1.0), not manufacture
// suppression from a single entry's own abandons.
func TestEntryDeliverAbandonWeight_SingleEpisodeNoExternalHistoryStillFlags(t *testing.T) {
	t.Parallel()

	st := NewState()
	st.OperatorKey = "operator-key-hex"

	const entry = "entry-single-episode-target"
	const buyer = "buyer-single-episode"
	for i := 0; i < 12; i++ {
		deliverOnly(t, st, buyer+"-abandon-"+string(rune('0'+i)), entry, buyer)
	}

	st.mu.RLock()
	weight := st.entryDeliverAbandonWeightLocked(entry)
	st.mu.RUnlock()
	if weight != 1.0 {
		t.Errorf("single-episode abandon weight = %f, want 1.0 (no external track record to judge the buyer by)", weight)
	}

	candidates := st.ExpiryCandidates()
	found := false
	for _, c := range candidates {
		if c.EntryID == entry {
			found = true
		}
	}
	if !found {
		t.Errorf("entry-single-episode-target (12 abandons, single buyer, NO external history) must still flag — matches dontguess-659's griefing regression requirement")
	}
}

// TestEntryDeliverAbandonWeight_NoDataDefaultsToOne verifies the neutral
// fallback: an entry with entryDeliverCount but no recorded per-buyer
// association (e.g. seeded directly into the aggregate map, bypassing
// applySettleDeliver — simulating state replayed from an older log before
// this fold existed) is NOT scaled down, preserving prior unweighted behavior.
func TestEntryDeliverAbandonWeight_NoDataDefaultsToOne(t *testing.T) {
	t.Parallel()

	st := NewState()
	st.mu.Lock()
	st.entryDeliverCount["entry-legacy"] = 10
	st.mu.Unlock()

	st.mu.RLock()
	weight := st.entryDeliverAbandonWeightLocked("entry-legacy")
	st.mu.RUnlock()

	if weight != 1.0 {
		t.Errorf("no-data weight = %f, want 1.0 (must not fabricate a demotion from missing per-buyer data)", weight)
	}

	candidates := st.ExpiryCandidates()
	found := false
	for _, c := range candidates {
		if c.EntryID == "entry-legacy" {
			found = true
		}
	}
	if !found {
		t.Errorf("entry-legacy (deliver=10, consume=0, no per-buyer data) must still flag — unweighted fallback preserved")
	}
}
