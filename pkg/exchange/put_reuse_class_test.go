package exchange_test

// put_reuse_class_test.go — tests for dontguess-13a: high-reuse artifact classification
// and incentive advantage.
//
// Design (exchange-matching-measurement-review.md §4):
//
// The §4 high-reuse artifact classes (schema checklists, protocol/setup READMEs,
// CI path filters, language-level test patterns, migration recipes) earn a pricing
// and residual advantage over session ephemera:
//
//	Accept price:  85% of token_cost (vs 70% standard) — 15-point premium at put time.
//	Residual rate: 20% of sale price (vs 10% standard) — double the ongoing revenue stream.
//
// Classification is two-gate resistant to gameability:
//	Gate 1: content_type must be code, analysis, or summary.
//	Gate 2: description must contain a §4 primary keyword AND at least one co-signal.
//	         A bare keyword mention without a co-signal DOES NOT classify.
//
// Tests in this file:
//
//	IsHighReuse classifier — positive cases (§4 examples from the design doc):
//	  TestHighReuseClassifier_PositiveCases
//
//	IsHighReuse classifier — negative cases (MUST return false):
//	  TestHighReuseClassifier_NegativeCases_Ephemera
//	    – "review of the project readme file"           (readme without protocol/setup co-signal)
//	    – "analysis of what the readme says"            (readme without co-signal)
//	    – "my session notes on the guide for X"         (guide without §4 context — not a keyword)
//	    – "summary of patterns found in the codebase"   (pattern without test+language co-signal)
//	    – "checklist of things I need to do today"      (checklist without schema/conformance co-signal)
//
//	Incentive advantage:
//	  TestReuseClassAdvantage_AcceptPrice  — high-reuse put earns more at accept than equivalent ephemera
//	  TestReuseClassAdvantage_RunAutoAccept — RunAutoAccept applies 85% for high-reuse, 70% for standard

import (
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/campfire-net/dontguess/pkg/exchange"
)

// --- IsHighReuseArtifact classifier tests ---

// TestHighReuseClassifier_PositiveCases verifies that §4 exemplar descriptions
// with appropriate content_types return true from IsHighReuseArtifact.
// These are the EXACT examples from exchange-matching-measurement-review.md §4.
func TestHighReuseClassifier_PositiveCases(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		description string
		contentType string
	}{
		{
			name:        "schema_checklist_code",
			description: "legion.tools v1.2 schema correctness checklist",
			contentType: "code",
		},
		{
			name:        "protocol_readme_analysis",
			description: "cf-protocol README CF_NO_PINS setup guide",
			contentType: "analysis",
		},
		{
			name:        "ci_path_filter_code",
			description: "GateEvaluator conformance CI path filter for pipeline",
			contentType: "code",
		},
		{
			name:        "test_pattern_go_code",
			description: "flock contention test pattern for Go with race detector",
			contentType: "code",
		},
		{
			name:        "migration_recipe_analysis",
			description: "cf migrate-store --cf-home symlink bridge migration recipe",
			contentType: "analysis",
		},
		{
			name:        "protocol_readme_summary",
			description: "dontguess protocol README install bootstrap guide",
			contentType: "summary",
		},
		{
			name:        "schema_conformance_checklist",
			description: "convention conformance checklist for schema validation",
			contentType: "analysis",
		},
		{
			name:        "ci_config_fragment",
			description: "reusable CI config filter for conformance checks",
			contentType: "code",
		},
		// --- dontguess-a0e: over-tightening guards ---
		// Genuine §4 artifacts that sit close to the new structural thresholds. These
		// MUST still classify true — proving the length floor (≥5 tokens) and the
		// co-signal adjacency window (≤3 tokens) do not reject real, terse artifacts.
		{
			name:        "edge_five_token_checklist",
			description: "legion.tools v1.2 schema correctness checklist",
			contentType: "code",
			// exactly 5 tokens; primary 'checklist' with 'correctness' adjacent — guards the floor.
		},
		{
			name:        "edge_hyphenated_readme_setup",
			description: "cf-protocol README CF_NO_PINS setup guide",
			contentType: "analysis",
			// 5 tokens incl. hyphenated identifier; 'setup' is 2 tokens from 'readme' — guards tokenizer + window.
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			entry := &exchange.InventoryEntry{
				EntryID:     "test-entry-" + tc.name,
				Description: tc.description,
				ContentType: tc.contentType,
			}
			if !exchange.IsHighReuseArtifactForTest(entry) {
				t.Errorf("IsHighReuseArtifact(%q, type=%q) = false, want true (§4 positive case)",
					tc.description, tc.contentType)
			}
		})
	}
}

// TestHighReuseClassifier_NegativeCases_Ephemera verifies that session-ephemera
// descriptions return FALSE from IsHighReuseArtifact — even when they contain
// a §4 keyword as a bare mention.
//
// These are the concrete gameability cases the veracity adversary identified.
// They MUST return false to prevent an agent from mislabeling ephemera as
// high-reuse to receive the 85% accept price + 20% residual.
func TestHighReuseClassifier_NegativeCases_Ephemera(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		description string
		contentType string
		reason      string
	}{
		{
			name:        "readme_without_cosignal",
			description: "review of the project readme file",
			contentType: "analysis",
			reason:      "bare 'readme' mention without protocol/setup/config co-signal — session ephemera",
		},
		{
			name:        "readme_analysis_ephemera",
			description: "analysis of what the readme says",
			contentType: "analysis",
			reason:      "bare 'readme' mention — no protocol/setup context, ephemeral session analysis",
		},
		{
			name:        "readme_session_notes",
			description: "my session notes on what the readme says about the API",
			contentType: "summary",
			reason:      "bare 'readme' mention in session notes — no structural protocol co-signal",
		},
		{
			name:        "pattern_without_cosignal",
			description: "summary of patterns found in the codebase",
			contentType: "summary",
			reason:      "'pattern' is not a primary keyword; requires 'test pattern' compound",
		},
		{
			name:        "checklist_without_cosignal",
			description: "checklist of things I need to do today",
			contentType: "analysis",
			reason:      "'checklist' present but no schema/conformance/protocol co-signal",
		},
		{
			name:        "readme_review_wrong_type",
			description: "cf-protocol README setup guide",
			contentType: "review",
			reason:      "correct description but wrong content_type (review is excluded by gate 1)",
		},
		{
			name:        "readme_data_type",
			description: "campfire protocol README config bootstrap",
			contentType: "data",
			reason:      "correct keywords but data content_type excluded by gate 1",
		},
		{
			name:        "test_pattern_no_language",
			description: "test pattern for distributed systems",
			contentType: "code",
			reason:      "'test pattern' present but no language/library/idiom co-signal",
		},
		// --- dontguess-a0e: crafted SHORT keyword-stuff ephemera ---
		// These are minimal concatenations of the classifier's own trigger words with
		// no surrounding description. They satisfied the old (presence-anywhere) gates
		// and earned the +15% accept / +10% residual. They MUST classify false: a
		// distilled artifact is described, not keyword-tagged.
		{
			name:        "stuff_test_pattern_go_idiom",
			description: "test pattern go idiom",
			contentType: "code",
			reason:      "4-token keyword-stuff (primary 'test pattern' + co-signals 'go'/'idiom') — below the 5-token artifact floor",
		},
		{
			name:        "stuff_checklist_schema_validation",
			description: "checklist schema validation",
			contentType: "analysis",
			reason:      "3-token keyword-stuff (primary 'checklist' + co-signals 'schema'/'validation') — below the artifact floor",
		},
		{
			name:        "stuff_readme_setup_guide",
			description: "readme setup guide",
			contentType: "summary",
			reason:      "3-token keyword-stuff (primary 'readme' + co-signal 'setup') — below the artifact floor",
		},
		{
			name:        "stuff_migration_recipe_runbook",
			description: "migration recipe runbook",
			contentType: "analysis",
			reason:      "3-token keyword-stuff (primary 'migration recipe' + co-signal 'runbook') — below the artifact floor",
		},
		{
			name:        "stuff_ci_config_filter",
			description: "ci config filter",
			contentType: "code",
			reason:      "3-token keyword-stuff (primary 'ci config' + co-signal 'filter') — below the artifact floor",
		},
		// --- dontguess-a0e: PADDED keyword-stuff (defeats a naive length-only fix) ---
		// These pad to ≥5 tokens but keep the co-signal far from the primary keyword,
		// so the trigger words are incidental, not a descriptive phrase. The co-signal
		// adjacency gate (window=3) rejects them even though they clear the length floor.
		{
			name:        "padded_test_pattern_far_go",
			description: "test pattern for my session notes today written in go",
			contentType: "code",
			reason:      "9 tokens but 'go' co-signal is 6 tokens from 'test pattern' — incidental, outside the adjacency window",
		},
		{
			name:        "padded_checklist_far_schema",
			description: "checklist of all the random things I should validate against the schema someday",
			contentType: "analysis",
			reason:      "long but 'schema' co-signal is far from 'checklist' — incidental mention, outside adjacency window",
		},
		{
			name:        "padded_readme_far_protocol",
			description: "readme notes I jotted down about the meeting and the team protocol",
			contentType: "summary",
			reason:      "long but 'protocol' co-signal is far from 'readme' — incidental, outside adjacency window",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			entry := &exchange.InventoryEntry{
				EntryID:     "test-entry-" + tc.name,
				Description: tc.description,
				ContentType: tc.contentType,
			}
			if exchange.IsHighReuseArtifactForTest(entry) {
				t.Errorf("IsHighReuseArtifact(%q, type=%q) = true, want false\nReason: %s",
					tc.description, tc.contentType, tc.reason)
			}
		})
	}
}

// --- Accept-price advantage tests ---

// buildReuseClassPutPayload constructs a put payload with explicit content bytes
// sized to safely accommodate the given tokenCost under the MaxTokensPerByte cap.
func buildReuseClassPutPayload(t *testing.T, desc, contentType string, tokenCost int64) []byte {
	t.Helper()
	// Content must be at least tokenCost / MaxTokensPerByte bytes to pass the
	// plausibility check (token_cost ≤ content_size_bytes * MaxTokensPerByte).
	minBytes := tokenCost/exchange.MaxTokensPerByte + 1
	if minBytes < 64 {
		minBytes = 64
	}
	contentBytes := make([]byte, minBytes)
	copy(contentBytes, []byte("cached inference result: "+desc+" "))
	for i := len(desc) + 25; i < int(minBytes); i++ {
		contentBytes[i] = byte('a' + i%26)
	}
	encoded := base64.StdEncoding.EncodeToString(contentBytes)
	p, err := json.Marshal(map[string]any{
		"description":  desc,
		"content":      encoded,
		"token_cost":   tokenCost,
		"content_type": "exchange:content-type:" + contentType,
		"domains":      []string{"go"},
	})
	if err != nil {
		t.Fatalf("buildReuseClassPutPayload: %v", err)
	}
	return p
}

// TestReuseClassAdvantage_AcceptPrice verifies that a high-reuse artifact put
// receives an 85% accept price while an equivalent-cost ephemeral put receives 70%.
//
// This tests the CORE OUTCOME of dontguess-13a: puts of distilled artifacts earn
// a measurable advantage over session ephemera. The difference (15 percentage points)
// must be reflected in the price returned by RunAutoAccept.
//
// Test approach:
//  1. Two puts: one high-reuse (§4 class), one ephemeral (same token_cost).
//  2. Both pass quality gates (token_cost ≥ MinTokenCost, unique content, valid desc).
//  3. RunAutoAccept is called for both.
//  4. High-reuse entry's PutPrice = tokenCost * 85 / 100.
//     Ephemeral entry's PutPrice = tokenCost * 70 / 100.
//     The high-reuse entry must earn more than the ephemeral entry.
func TestReuseClassAdvantage_AcceptPrice(t *testing.T) {
	t.Parallel()

	const tokenCost = int64(5000)
	const wantHighReusePrice = tokenCost * exchange.HighReuseAcceptPriceNumerator / 100 // 4250
	const wantStandardPrice = tokenCost * exchange.StandardAcceptPriceNumerator / 100   // 3500

	h := newTestHarness(t)
	eng := h.newEngine()

	// High-reuse put: schema correctness checklist (§4 class, positive example from design doc).
	highReuseDesc := "convention schema correctness checklist for protocol validation"
	highReusePutMsg := h.sendMessage(h.seller,
		buildReuseClassPutPayload(t, highReuseDesc, "code", tokenCost),
		[]string{exchange.TagPut, "exchange:content-type:code"},
		nil,
	)

	// Ephemeral put: session analysis (same token_cost, different content).
	ephemeralDesc := "analysis of the project status from my session on 2026-06-02"
	ephemeralPutMsg := h.sendMessage(h.seller,
		buildReuseClassPutPayload(t, ephemeralDesc, "analysis", tokenCost),
		[]string{exchange.TagPut, "exchange:content-type:analysis"},
		nil,
	)

	// Replay so both puts are in pendingPuts.
	replayAll(t, h, eng)

	pending := eng.State().PendingPuts()
	if len(pending) < 2 {
		t.Fatalf("expected ≥2 pending puts after replay, got %d", len(pending))
	}

	// Verify the classifier agrees with our expectations before testing pricing.
	var highReuseEntry, ephemeralEntry *exchange.InventoryEntry
	for _, e := range pending {
		e := e
		if e.PutMsgID == highReusePutMsg.ID {
			highReuseEntry = e
		}
		if e.PutMsgID == ephemeralPutMsg.ID {
			ephemeralEntry = e
		}
	}
	if highReuseEntry == nil {
		t.Fatalf("high-reuse put %s not in pendingPuts", highReusePutMsg.ID[:8])
	}
	if ephemeralEntry == nil {
		t.Fatalf("ephemeral put %s not in pendingPuts", ephemeralPutMsg.ID[:8])
	}
	if !exchange.IsHighReuseArtifactForTest(highReuseEntry) {
		t.Errorf("IsHighReuseArtifact(%q) = false, want true — test setup error", highReuseDesc)
	}
	if exchange.IsHighReuseArtifactForTest(ephemeralEntry) {
		t.Errorf("IsHighReuseArtifact(%q) = true, want false — test setup error", ephemeralDesc)
	}

	// Run auto-accept for both (RunAutoAccept computes the price internally).
	now := time.Now()
	skipped := make(map[string]struct{})
	eng.RunAutoAccept(tokenCost*10, now, skipped) // max high enough to accept both

	// Check PutPrice in inventory.
	inv := eng.State().Inventory()
	var highReuseAccepted, ephemeralAccepted *exchange.InventoryEntry
	for _, e := range inv {
		e := e
		if e.PutMsgID == highReusePutMsg.ID {
			highReuseAccepted = e
		}
		if e.PutMsgID == ephemeralPutMsg.ID {
			ephemeralAccepted = e
		}
	}

	if highReuseAccepted == nil {
		t.Fatalf("high-reuse put not in inventory after RunAutoAccept")
	}
	if ephemeralAccepted == nil {
		t.Fatalf("ephemeral put not in inventory after RunAutoAccept")
	}

	// High-reuse must earn more than ephemeral.
	if highReuseAccepted.PutPrice <= ephemeralAccepted.PutPrice {
		t.Errorf("high-reuse PutPrice=%d, ephemeral PutPrice=%d: want high-reuse > ephemeral",
			highReuseAccepted.PutPrice, ephemeralAccepted.PutPrice)
	}

	// Exact price assertions.
	if highReuseAccepted.PutPrice != wantHighReusePrice {
		t.Errorf("high-reuse PutPrice=%d, want %d (85%% of %d)",
			highReuseAccepted.PutPrice, wantHighReusePrice, tokenCost)
	}
	if ephemeralAccepted.PutPrice != wantStandardPrice {
		t.Errorf("ephemeral PutPrice=%d, want %d (70%% of %d)",
			ephemeralAccepted.PutPrice, wantStandardPrice, tokenCost)
	}

	t.Logf("PASS: high-reuse accept price=%d (85%%), ephemeral=%d (70%%), advantage=%d scrip",
		highReuseAccepted.PutPrice, ephemeralAccepted.PutPrice,
		highReuseAccepted.PutPrice-ephemeralAccepted.PutPrice)
}

// TestReuseClassAdvantage_RunAutoAccept verifies that RunAutoAccept applies the
// HighReuseAcceptPriceNumerator (85%) for high-reuse puts and StandardAcceptPriceNumerator
// (70%) for standard puts, with exact price values matching the constants.
//
// This is a focused unit test that does NOT require full engine I/O — it exercises
// RunAutoAccept directly (the same path used in production) to confirm the pricing
// constant is applied at the right call site.
func TestReuseClassAdvantage_RunAutoAccept(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		description string
		contentType string
		tokenCost   int64
		wantPct     int64 // expected accept price as integer percentage of tokenCost
	}{
		{
			name:        "high_reuse_schema_checklist",
			description: "schema conformance checklist for convention validation",
			contentType: "code",
			tokenCost:   10000,
			wantPct:     exchange.HighReuseAcceptPriceNumerator, // 85
		},
		{
			name:        "high_reuse_protocol_readme",
			description: "cf-protocol README setup config integration guide",
			contentType: "analysis",
			tokenCost:   8000,
			wantPct:     exchange.HighReuseAcceptPriceNumerator, // 85
		},
		{
			name:        "standard_ephemeral_analysis",
			description: "analysis of this session's work items and next steps",
			contentType: "analysis",
			tokenCost:   5000,
			wantPct:     exchange.StandardAcceptPriceNumerator, // 70
		},
		{
			name:        "standard_other_type",
			description: "schema correctness checklist conformance validation",
			contentType: "other",
			tokenCost:   5000,
			wantPct:     exchange.StandardAcceptPriceNumerator, // 70 — other type excluded by gate 1
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newTestHarness(t)
			eng := h.newEngine()

			putMsg := h.sendMessage(h.seller,
				buildReuseClassPutPayload(t, tc.description, tc.contentType, tc.tokenCost),
				[]string{exchange.TagPut, "exchange:content-type:" + tc.contentType},
				nil,
			)

			replayAll(t, h, eng)

			pending := eng.State().PendingPuts()
			found := false
			for _, e := range pending {
				if e.PutMsgID == putMsg.ID {
					found = true
				}
			}
			if !found {
				t.Fatalf("put not in pendingPuts after replay — quality gate incorrectly rejected it (desc=%q)", tc.description)
			}

			now := time.Now()
			skipped := make(map[string]struct{})
			eng.RunAutoAccept(tc.tokenCost*10, now, skipped)

			inv := eng.State().Inventory()
			var accepted *exchange.InventoryEntry
			for _, e := range inv {
				e := e
				if e.PutMsgID == putMsg.ID {
					accepted = e
				}
			}
			if accepted == nil {
				t.Fatalf("put not in inventory after RunAutoAccept")
			}

			wantPrice := tc.tokenCost * tc.wantPct / 100
			if accepted.PutPrice != wantPrice {
				t.Errorf("PutPrice=%d, want %d (%d%% of token_cost=%d)",
					accepted.PutPrice, wantPrice, tc.wantPct, tc.tokenCost)
			} else {
				t.Logf("PASS: %s: PutPrice=%d (%d%% of %d)", tc.name, accepted.PutPrice, tc.wantPct, tc.tokenCost)
			}
		})
	}
}
