package exchange

import (
	"fmt"
	"strings"
	"testing"
)

// generateContent creates a content string of approximately the given number of tokens.
// Each "token" is ~4 bytes. We use paragraph-structured prose by default.
func generateProse(tokens int) []byte {
	// Each paragraph ~50 tokens (~200 bytes). Build enough paragraphs.
	paragraphTokens := 50
	paragraphText := strings.Repeat("word ", paragraphTokens) // 50 words
	paragraphs := (tokens + paragraphTokens - 1) / paragraphTokens
	var sb strings.Builder
	for i := 0; i < paragraphs; i++ {
		sb.WriteString(paragraphText)
		sb.WriteString("\n\n")
	}
	return []byte(sb.String())
}

// generateCode creates Go-like code content of approximately the given number of tokens.
func generateCode(tokens int) []byte {
	funcTokens := 30 // each func ~30 tokens
	funcs := (tokens + funcTokens - 1) / funcTokens
	var sb strings.Builder
	for i := 0; i < funcs; i++ {
		sb.WriteString(fmt.Sprintf("\nfunc foo%d() {\n\t// body line one\n\t// body line two\n}\n", i))
	}
	return []byte(sb.String())
}

// generateData creates CSV-like data of approximately the given number of tokens.
func generateData(tokens int) []byte {
	lineTokens := 10 // each record ~10 tokens
	lines := (tokens + lineTokens - 1) / lineTokens
	var sb strings.Builder
	sb.WriteString("id,name,value\n")
	for i := 0; i < lines; i++ {
		sb.WriteString(fmt.Sprintf("%d,item%d,%d\n", i, i, i*10))
	}
	return []byte(sb.String())
}

var pa = &PreviewAssembler{}

// ---- Determinism tests ----

// TestPreviewDeterministic verifies identical inputs produce identical outputs.
func TestPreviewDeterministic(t *testing.T) {
	content := generateProse(2000)
	req := PreviewRequest{
		Content:     content,
		ContentType: "analysis",
		EntryID:     "entry-abc",
	}

	r1, err := pa.Assemble(req)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	r2, err := pa.Assemble(req)
	if err != nil {
		t.Fatalf("Assemble second: %v", err)
	}

	if len(r1.Chunks) != len(r2.Chunks) {
		t.Fatalf("chunk count mismatch: %d vs %d", len(r1.Chunks), len(r2.Chunks))
	}
	for i, c := range r1.Chunks {
		if c.Content != r2.Chunks[i].Content {
			t.Errorf("chunk %d content differs between identical calls", i)
		}
		if c.StartByte != r2.Chunks[i].StartByte {
			t.Errorf("chunk %d StartByte differs: %d vs %d", i, c.StartByte, r2.Chunks[i].StartByte)
		}
	}
}

// TestPreviewDeterministicSeedDriven verifies that chunk positions are driven by the seed
// (entryID, buyerKey, matchID), not by random state or content alone.
// - Same seed + different content → positions are seed-driven (not content-hash-driven).
// - Same content + different seeds → different chunk positions.
func TestPreviewDeterministicSeedDriven(t *testing.T) {
	// Build two different contents of the same size so chunk count is equal.
	contentA := generateProse(2000)
	// contentB: same structure but different text.
	contentB := []byte(strings.ReplaceAll(string(contentA), "word", "text"))

	entryID := "entry-seed-test"
	buyerKey := "buyer-seed-test"
	matchID := "match-seed-test"

	// Same seed, different content — positions should differ (content is different).
	rA, err := pa.Assemble(PreviewRequest{Content: contentA, ContentType: "analysis", EntryID: entryID, BuyerKey: buyerKey, MatchID: matchID})
	if err != nil {
		t.Fatalf("Assemble A: %v", err)
	}
	rB, err := pa.Assemble(PreviewRequest{Content: contentB, ContentType: "analysis", EntryID: entryID, BuyerKey: buyerKey, MatchID: matchID})
	if err != nil {
		t.Fatalf("Assemble B: %v", err)
	}
	// Both results must be deterministic when called again with the same inputs.
	rA2, _ := pa.Assemble(PreviewRequest{Content: contentA, ContentType: "analysis", EntryID: entryID, BuyerKey: buyerKey, MatchID: matchID})
	rB2, _ := pa.Assemble(PreviewRequest{Content: contentB, ContentType: "analysis", EntryID: entryID, BuyerKey: buyerKey, MatchID: matchID})
	for i := range rA.Chunks {
		if rA.Chunks[i].StartByte != rA2.Chunks[i].StartByte {
			t.Errorf("content A not deterministic: chunk %d start %d vs %d", i, rA.Chunks[i].StartByte, rA2.Chunks[i].StartByte)
		}
	}
	for i := range rB.Chunks {
		if rB.Chunks[i].StartByte != rB2.Chunks[i].StartByte {
			t.Errorf("content B not deterministic: chunk %d start %d vs %d", i, rB.Chunks[i].StartByte, rB2.Chunks[i].StartByte)
		}
	}

	// Same content + different seeds → different chunk positions.
	rSeed1, _ := pa.Assemble(PreviewRequest{Content: contentA, ContentType: "analysis", EntryID: "e1", BuyerKey: "b1", MatchID: "m1"})
	rSeed2, _ := pa.Assemble(PreviewRequest{Content: contentA, ContentType: "analysis", EntryID: "e2", BuyerKey: "b2", MatchID: "m2"})
	allSame := true
	if len(rSeed1.Chunks) == len(rSeed2.Chunks) {
		for i := range rSeed1.Chunks {
			if rSeed1.Chunks[i].StartByte != rSeed2.Chunks[i].StartByte {
				allSame = false
				break
			}
		}
	} else {
		allSame = false
	}
	if allSame {
		t.Error("same content + different seeds produced identical chunk positions — seeding is broken")
	}
	_ = rA
	_ = rB
}

func TestPreviewDifferentSeeds(t *testing.T) {
	content := generateProse(2000)

	req1 := PreviewRequest{Content: content, ContentType: "analysis", EntryID: "e1", BuyerKey: "b1", MatchID: "m1"}
	req2 := PreviewRequest{Content: content, ContentType: "analysis", EntryID: "e2", BuyerKey: "b2", MatchID: "m2"}

	r1, _ := pa.Assemble(req1)
	r2, _ := pa.Assemble(req2)

	// Different seeds should produce different chunk positions with high probability
	// (birthday collision is negligible for distinct seeds on large content)
	allSame := true
	if len(r1.Chunks) != len(r2.Chunks) {
		allSame = false
	} else {
		for i := range r1.Chunks {
			if r1.Chunks[i].StartByte != r2.Chunks[i].StartByte {
				allSame = false
				break
			}
		}
	}
	if allSame {
		t.Error("different seeds produced identical chunk positions — seeding is broken")
	}
}

// ---- Chunk count tests ----

func TestPreviewFiveChunksLargeContent(t *testing.T) {
	content := generateProse(3000) // well above 500-token threshold
	req := PreviewRequest{Content: content, ContentType: "analysis", EntryID: "e", BuyerKey: "b", MatchID: "m"}

	r, err := pa.Assemble(req)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if len(r.Chunks) != previewChunkCount {
		t.Errorf("expected %d chunks, got %d", previewChunkCount, len(r.Chunks))
	}
}

func TestPreviewReducedChunksSmallContent(t *testing.T) {
	// ~250 tokens: fits 2 min-100-token chunks (250/100 = 2)
	content := generateProse(250)
	req := PreviewRequest{Content: content, ContentType: "analysis", EntryID: "e", BuyerKey: "b", MatchID: "m"}

	r, err := pa.Assemble(req)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if len(r.Chunks) > previewChunkCount {
		t.Errorf("expected <= %d chunks for 250-token content, got %d", previewChunkCount, len(r.Chunks))
	}
	if len(r.Chunks) == 0 {
		t.Error("expected at least one chunk for 250-token content")
	}
}

// ---- Exact boundary edge cases (Finding 3) ----

// makeExactTokenContent builds content with exactly the given number of tokens.
// estimateTokens = (len+3)/4 = n when len = n*4.
// Content uses 'word '-repeated text with '\n\n' paragraph separators so boundary
// detection produces genuine split points for the assembler to snap to.
func makeExactTokenContent(tokens int) []byte {
	// Each "word " is 5 bytes = ~1.25 tokens. Use repeated words terminated with '\n\n'
	// every ~50 tokens so there are plenty of paragraph boundaries.
	// We build at least `tokens*4` bytes and trim to exactly `tokens*4` bytes.
	target := tokens * 4
	var sb strings.Builder
	wordUnit := "word " // 5 bytes
	for sb.Len() < target+100 {
		sb.WriteString(wordUnit)
		if sb.Len()%200 == 0 { // paragraph break roughly every 200 bytes
			sb.WriteString("\n\n")
		}
	}
	b := []byte(sb.String())
	// Trim or pad to exactly target bytes so estimateTokens returns exactly tokens.
	if len(b) > target {
		b = b[:target]
	}
	for len(b) < target {
		b = append(b, 'x')
	}
	return b
}

// TestPreviewChunkCountAtBoundaries tests the token-count thresholds at critical values.
//
// Logic in Assemble:
//   - totalTokens < minTokensPerChunk (100)  → single full-content chunk (passthrough)
//   - totalTokens < minTokensForChunks (500) → chunkCount = totalTokens / 100
//   - totalTokens >= minTokensForChunks      → chunkCount = 5
//
// The minimum-token enforcement and boundary snapping can reduce the final produced
// chunk count below chunkCount when chunks would overlap (sparse content, small size).
// This test verifies the correct logic path is taken at each threshold boundary, with
// bounds that account for the overlap-resolution behaviour of selectChunks.
//
//   -  99 tokens → 1 chunk containing all content (full passthrough, not chunked)
//   - 100 tokens → 1..1 chunks (chunkCount=1, no further reduction possible)
//   - 499 tokens → 1..4 chunks (chunkCount=4, overlap may reduce to fewer)
//   - 500 tokens → 1..5 chunks (chunkCount=5, first token count above the threshold)
//   - 501 tokens → 1..5 chunks (chunkCount=5, firmly above threshold)
func TestPreviewChunkCountAtBoundaries(t *testing.T) {
	cases := []struct {
		tokens       int
		wantMinChunk int
		wantMaxChunk int
		fullContent  bool // if true, the single chunk must equal all content
		desc         string
	}{
		{99, 1, 1, true, "99 tokens: below per-chunk minimum, full-content passthrough"},
		{100, 1, 1, false, "100 tokens: exactly at per-chunk minimum, chunkCount=100/100=1"},
		{499, 1, 4, false, "499 tokens: below 5-chunk threshold, chunkCount<=4"},
		{500, 1, 5, false, "500 tokens: exactly at 5-chunk threshold (500 is not < 500), chunkCount=5"},
		{501, 1, 5, false, "501 tokens: above threshold, chunkCount=5"},
	}

	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			content := makeExactTokenContent(tc.tokens)
			// Verify the helper produces the exact token count we intend.
			actualTokens := estimateTokens(content)
			if actualTokens != tc.tokens {
				t.Fatalf("makeExactTokenContent(%d) produced %d tokens (len=%d) — helper is wrong",
					tc.tokens, actualTokens, len(content))
			}
			req := PreviewRequest{Content: content, ContentType: "analysis", EntryID: "e", BuyerKey: "b", MatchID: "m"}
			r, err := pa.Assemble(req)
			if err != nil {
				t.Fatalf("Assemble: %v", err)
			}
			n := len(r.Chunks)
			if n < tc.wantMinChunk || n > tc.wantMaxChunk {
				t.Errorf("tokens=%d: got %d chunks, want [%d,%d]", tc.tokens, n, tc.wantMinChunk, tc.wantMaxChunk)
			}
			if tc.fullContent && n == 1 {
				if r.Chunks[0].Content != string(content) {
					t.Errorf("tokens=%d: expected full-content passthrough but chunk content differs", tc.tokens)
				}
			}
		})
	}
}

// ---- Small content edge cases ----

func TestPreviewSingleChunkBelowMinTokens(t *testing.T) {
	// Content with < 100 tokens (< 400 bytes)
	content := []byte(strings.Repeat("a ", 40)) // 40 "words" ≈ 80 bytes ≈ 20 tokens
	req := PreviewRequest{Content: content, ContentType: "other", EntryID: "e", BuyerKey: "b", MatchID: "m"}

	r, err := pa.Assemble(req)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if len(r.Chunks) != 1 {
		t.Errorf("expected 1 chunk for tiny content, got %d", len(r.Chunks))
	}
	if r.Chunks[0].Content != string(content) {
		t.Error("single-chunk result should contain all content")
	}
	if r.Chunks[0].StartByte != 0 || r.Chunks[0].EndByte != len(content) {
		t.Errorf("single chunk bounds wrong: start=%d end=%d len=%d",
			r.Chunks[0].StartByte, r.Chunks[0].EndByte, len(content))
	}
}

func TestPreviewEmptyContent(t *testing.T) {
	req := PreviewRequest{Content: []byte{}, ContentType: "other", EntryID: "e", BuyerKey: "b", MatchID: "m"}
	r, err := pa.Assemble(req)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if len(r.Chunks) != 0 {
		t.Errorf("expected 0 chunks for empty content, got %d", len(r.Chunks))
	}
}

func TestPreviewNilContent(t *testing.T) {
	req := PreviewRequest{Content: nil, ContentType: "other", EntryID: "e", BuyerKey: "b", MatchID: "m"}
	r, err := pa.Assemble(req)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if len(r.Chunks) != 0 {
		t.Errorf("expected 0 chunks for nil content, got %d", len(r.Chunks))
	}
}

func TestPreviewSingleLine(t *testing.T) {
	// A single very long line with no newlines — boundary detection falls back to 0/len
	content := []byte(strings.Repeat("word ", 600)) // ~600 words ≈ 600 tokens
	req := PreviewRequest{Content: content, ContentType: "other", EntryID: "e", BuyerKey: "b", MatchID: "m"}

	r, err := pa.Assemble(req)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if len(r.Chunks) == 0 {
		t.Error("expected chunks for large single-line content")
	}
	// All chunks must be within bounds
	for i, c := range r.Chunks {
		if c.StartByte < 0 || c.EndByte > len(content) || c.StartByte >= c.EndByte {
			t.Errorf("chunk %d has invalid bounds: start=%d end=%d len=%d", i, c.StartByte, c.EndByte, len(content))
		}
	}
}

// ---- Content-type-aware chunking ----

// TestPreviewCodeBoundaries verifies that code chunks start at recognizable code keywords.
// Requirements (Finding 2):
//   - >= 3/5 chunks must start at a code keyword (func/def/class/function)
//   - Blank-line alignment does NOT count as code boundary alignment
//   - At least one chunk must start with a recognizable keyword
func TestPreviewCodeBoundaries(t *testing.T) {
	// Generate code with dense func declarations to ensure many boundaries.
	content := generateCode(1500)
	req := PreviewRequest{Content: content, ContentType: "code", EntryID: "e", BuyerKey: "b", MatchID: "m"}

	r, err := pa.Assemble(req)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if len(r.Chunks) == 0 {
		t.Fatal("expected chunks for code content")
	}

	s := string(content)
	codeKeywords := []string{"func ", "def ", "class ", "function "}

	aligned := 0
	for _, c := range r.Chunks {
		start := c.StartByte
		for _, kw := range codeKeywords {
			if start+len(kw) <= len(s) && s[start:start+len(kw)] == kw {
				aligned++
				break
			}
		}
	}

	// Require at least 3 out of 5 chunks to start at actual code keywords.
	threshold := 3
	if len(r.Chunks) < previewChunkCount {
		// Proportional threshold for smaller content: ceil(60% of chunks)
		threshold = (len(r.Chunks)*3 + 4) / 5
		if threshold < 1 {
			threshold = 1
		}
	}
	if aligned < threshold {
		t.Errorf("only %d/%d chunks start at code keyword boundaries (need >= %d); chunks: %+v",
			aligned, len(r.Chunks), threshold, r.Chunks)
	}

	// At least one chunk must start with a recognizable keyword.
	atLeastOne := false
	for _, c := range r.Chunks {
		start := c.StartByte
		for _, kw := range codeKeywords {
			if start+len(kw) <= len(s) && s[start:start+len(kw)] == kw {
				atLeastOne = true
				break
			}
		}
	}
	if !atLeastOne {
		t.Errorf("no chunk starts with a recognizable code keyword; chunks: %+v", r.Chunks)
	}
}

// TestPreviewPlanParagraphBoundaries verifies 'plan' content snaps to paragraph boundaries.
func TestPreviewPlanParagraphBoundaries(t *testing.T) {
	content := generateProse(2000)
	req := PreviewRequest{Content: content, ContentType: "plan", EntryID: "e", BuyerKey: "b", MatchID: "m"}

	r, err := pa.Assemble(req)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	s := string(content)

	aligned := 0
	for _, c := range r.Chunks {
		start := c.StartByte
		if start == 0 || (start >= 2 && s[start-2:start] == "\n\n") {
			aligned++
		}
	}
	if aligned == 0 {
		t.Errorf("no 'plan' chunks start at paragraph boundaries; chunks: %+v", r.Chunks)
	}
}

// TestPreviewReviewParagraphBoundaries verifies 'review' content snaps to paragraph boundaries.
func TestPreviewReviewParagraphBoundaries(t *testing.T) {
	content := generateProse(2000)
	req := PreviewRequest{Content: content, ContentType: "review", EntryID: "e", BuyerKey: "b", MatchID: "m"}

	r, err := pa.Assemble(req)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	s := string(content)

	aligned := 0
	for _, c := range r.Chunks {
		start := c.StartByte
		if start == 0 || (start >= 2 && s[start-2:start] == "\n\n") {
			aligned++
		}
	}
	if aligned == 0 {
		t.Errorf("no 'review' chunks start at paragraph boundaries; chunks: %+v", r.Chunks)
	}
}

// TestPreviewOtherNewlineBoundaries verifies 'other' content snaps to newline boundaries.
func TestPreviewOtherNewlineBoundaries(t *testing.T) {
	// Generate line-delimited content with many newlines.
	var sb strings.Builder
	for i := 0; i < 400; i++ {
		sb.WriteString(fmt.Sprintf("line %d: some content here for padding purposes\n", i))
	}
	content := []byte(sb.String())
	req := PreviewRequest{Content: content, ContentType: "other", EntryID: "e", BuyerKey: "b", MatchID: "m"}

	r, err := pa.Assemble(req)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	s := string(content)

	aligned := 0
	for _, c := range r.Chunks {
		start := c.StartByte
		if start == 0 || (start > 0 && s[start-1] == '\n') {
			aligned++
		}
	}
	if aligned == 0 {
		t.Errorf("no 'other' chunks start at newline boundaries; chunks: %+v", r.Chunks)
	}
}

func TestPreviewProseParagraphBoundaries(t *testing.T) {
	// Prose with clear paragraph boundaries
	content := generateProse(2000)
	req := PreviewRequest{Content: content, ContentType: "summary", EntryID: "e", BuyerKey: "b", MatchID: "m"}

	r, err := pa.Assemble(req)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	s := string(content)

	aligned := 0
	for _, c := range r.Chunks {
		start := c.StartByte
		// A paragraph boundary means we're at position 0, or the two bytes before are \n\n
		if start == 0 || (start >= 2 && s[start-2:start] == "\n\n") {
			aligned++
		}
	}
	if aligned == 0 {
		t.Errorf("no prose chunks start at paragraph boundaries; first chunk start=%d", r.Chunks[0].StartByte)
	}
}

func TestPreviewDataLineBoundaries(t *testing.T) {
	content := generateData(2000)
	req := PreviewRequest{Content: content, ContentType: "data", EntryID: "e", BuyerKey: "b", MatchID: "m"}

	r, err := pa.Assemble(req)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	s := string(content)

	aligned := 0
	for _, c := range r.Chunks {
		start := c.StartByte
		if start == 0 || (start > 0 && s[start-1] == '\n') {
			aligned++
		}
	}
	if aligned == 0 {
		t.Errorf("no data chunks start at line boundaries")
	}
}

// ---- Minimum token enforcement ----

func TestPreviewMinTokensEnforced(t *testing.T) {
	// Large content but we want to verify each chunk meets the minimum
	content := generateProse(3000)
	req := PreviewRequest{Content: content, ContentType: "analysis", EntryID: "e", BuyerKey: "b", MatchID: "m"}

	r, err := pa.Assemble(req)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	for i, c := range r.Chunks {
		tokens := estimateTokens([]byte(c.Content))
		if tokens < minTokensPerChunk {
			t.Errorf("chunk %d has %d tokens, below minimum %d", i, tokens, minTokensPerChunk)
		}
	}
}

// ---- Chunk bounds integrity ----

func TestPreviewChunkBoundsValid(t *testing.T) {
	content := generateProse(2000)
	req := PreviewRequest{Content: content, ContentType: "analysis", EntryID: "e", BuyerKey: "b", MatchID: "m"}

	r, err := pa.Assemble(req)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	for i, c := range r.Chunks {
		if c.StartByte < 0 {
			t.Errorf("chunk %d: StartByte %d < 0", i, c.StartByte)
		}
		if c.EndByte > len(content) {
			t.Errorf("chunk %d: EndByte %d > len(content) %d", i, c.EndByte, len(content))
		}
		if c.StartByte >= c.EndByte {
			t.Errorf("chunk %d: StartByte %d >= EndByte %d", i, c.StartByte, c.EndByte)
		}
		if c.Content != string(content[c.StartByte:c.EndByte]) {
			t.Errorf("chunk %d: Content does not match content[StartByte:EndByte]", i)
		}
	}
}

// ---- Token count summary ----

func TestPreviewTokenSummary(t *testing.T) {
	content := generateProse(2000)
	req := PreviewRequest{Content: content, ContentType: "analysis", EntryID: "e", BuyerKey: "b", MatchID: "m"}

	r, err := pa.Assemble(req)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}

	if r.TotalTokens <= 0 {
		t.Errorf("TotalTokens should be positive, got %d", r.TotalTokens)
	}
	if r.PreviewTokens <= 0 {
		t.Errorf("PreviewTokens should be positive, got %d", r.PreviewTokens)
	}
	if r.PreviewTokens > r.TotalTokens {
		t.Errorf("PreviewTokens %d > TotalTokens %d", r.PreviewTokens, r.TotalTokens)
	}

	// Verify PreviewTokens matches sum of chunk token estimates
	sum := 0
	for _, c := range r.Chunks {
		sum += estimateTokens([]byte(c.Content))
	}
	if sum != r.PreviewTokens {
		t.Errorf("PreviewTokens %d != sum of chunk tokens %d", r.PreviewTokens, sum)
	}
}

// ---- Seed derivation ----

func TestDeriveSeedUniqueness(t *testing.T) {
	// Different inputs must produce different seeds
	seeds := map[uint64]string{}
	cases := [][3]string{
		{"entry-1", "buyer-1", "match-1"},
		{"entry-2", "buyer-1", "match-1"},
		{"entry-1", "buyer-2", "match-1"},
		{"entry-1", "buyer-1", "match-2"},
		{"", "", ""},
		{"a", "b", "c"},
	}
	for _, tc := range cases {
		s := deriveSeed(tc[0], tc[1], tc[2])
		key := fmt.Sprintf("%s|%s|%s", tc[0], tc[1], tc[2])
		if prev, ok := seeds[s]; ok {
			t.Errorf("seed collision: %s and %s both produce %d", key, prev, s)
		}
		seeds[s] = key
	}
}

// ---- PRNG quality ----

func TestXorShift64Distribution(t *testing.T) {
	rng := newXorShift64(0xdeadbeef12345678)
	n := 1000
	buckets := make([]int, 10)
	for i := 0; i < n; i++ {
		buckets[rng.intn(10)]++
	}
	// Each bucket should have roughly n/10 = 100. Allow ±50%.
	for i, count := range buckets {
		if count < 50 || count > 150 {
			t.Errorf("bucket %d has count %d, expected ~100 (PRNG distribution check)", i, count)
		}
	}
}

// TestXorShift64ZeroSeedFallback verifies that a zero seed is replaced with the built-in
// fallback constant, producing a non-zero, non-degenerate sequence (Finding 7).
func TestXorShift64ZeroSeedFallback(t *testing.T) {
	rng := newXorShift64(0)

	// The state should have been set to the fallback constant, not zero.
	if rng.state == 0 {
		t.Fatal("newXorShift64(0) left state as 0 — xorshift will produce only zeros")
	}

	// Generate a sequence and verify it is non-degenerate: not all identical, not all zero.
	const n = 20
	values := make([]uint64, n)
	for i := range values {
		values[i] = rng.next()
	}

	allZero := true
	for _, v := range values {
		if v != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		t.Error("newXorShift64(0) produced all-zero sequence")
	}

	allSame := true
	for i := 1; i < n; i++ {
		if values[i] != values[0] {
			allSame = false
			break
		}
	}
	if allSame {
		t.Errorf("newXorShift64(0) produced constant sequence (value=%d) — degenerate PRNG", values[0])
	}
}

// ---- ChunkIndex integrity (Finding 6) ----

// TestPreviewChunkIndexAssigned verifies ChunkIndex values are unique, cover range [0, chunkCount),
// and are monotonically increasing alongside StartByte.
func TestPreviewChunkIndexAssigned(t *testing.T) {
	content := generateProse(2000)
	req := PreviewRequest{Content: content, ContentType: "analysis", EntryID: "e", BuyerKey: "b", MatchID: "m"}

	r, err := pa.Assemble(req)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if len(r.Chunks) == 0 {
		t.Fatal("expected at least one chunk")
	}

	n := len(r.Chunks)

	// ChunkIndex values must be unique.
	seen := make(map[int]int)
	for i, c := range r.Chunks {
		if prev, dup := seen[c.ChunkIndex]; dup {
			t.Errorf("duplicate ChunkIndex %d at positions %d and %d", c.ChunkIndex, prev, i)
		}
		seen[c.ChunkIndex] = i
	}

	// ChunkIndex values must cover [0, n).
	for idx := 0; idx < n; idx++ {
		if _, ok := seen[idx]; !ok {
			t.Errorf("ChunkIndex %d missing from result (have %d chunks, expected range [0,%d))", idx, n, n)
		}
	}

	// Chunks must be monotonically ordered: chunk[i].ChunkIndex < chunk[i+1].ChunkIndex
	// and chunk[i].StartByte < chunk[i+1].StartByte.
	for i := 1; i < n; i++ {
		if r.Chunks[i].ChunkIndex <= r.Chunks[i-1].ChunkIndex {
			t.Errorf("ChunkIndex not monotonically increasing: chunk[%d].ChunkIndex=%d <= chunk[%d].ChunkIndex=%d",
				i, r.Chunks[i].ChunkIndex, i-1, r.Chunks[i-1].ChunkIndex)
		}
		if r.Chunks[i].StartByte <= r.Chunks[i-1].StartByte {
			t.Errorf("StartByte not monotonically increasing: chunk[%d].StartByte=%d <= chunk[%d].StartByte=%d",
				i, r.Chunks[i].StartByte, i-1, r.Chunks[i-1].StartByte)
		}
	}
}

// ---- Anti-reconstruction exposure bound (Finding 5) ----

// TestPreviewAntiReconstructionExposureBound documents the exposure bound when
// the same (entryID, buyerKey) are used across many different matchIDs.
//
// Design intent: each matchID is a separate transaction — different chunks per match
// is intentional economic friction. This test does NOT assert that reconstruction is
// impossible; it documents how much of the content is exposed across N transactions,
// so the bound is known and visible as a test assertion.
//
// The test asserts that no single matchID alone reveals the full content (each
// transaction is still partial), while acknowledging that N transactions together
// may cover a larger fraction.
func TestPreviewAntiReconstructionExposureBound(t *testing.T) {
	const numMatchIDs = 50
	content := generateProse(2000)
	contentLen := len(content)

	exposed := make([]bool, contentLen)
	exposedPerMatch := make([]int, numMatchIDs)

	for i := 0; i < numMatchIDs; i++ {
		matchID := fmt.Sprintf("match-%04d", i)
		req := PreviewRequest{
			Content:     content,
			ContentType: "analysis",
			EntryID:     "entry-reconstruct-test",
			BuyerKey:    "buyer-reconstruct-test",
			MatchID:     matchID,
		}
		r, err := pa.Assemble(req)
		if err != nil {
			t.Fatalf("Assemble matchID=%s: %v", matchID, err)
		}

		matchExposed := 0
		for _, c := range r.Chunks {
			for b := c.StartByte; b < c.EndByte && b < contentLen; b++ {
				if !exposed[b] {
					exposed[b] = true
				}
				matchExposed++
			}
		}
		exposedPerMatch[i] = matchExposed
	}

	// Count total unique bytes exposed.
	totalExposed := 0
	for _, v := range exposed {
		if v {
			totalExposed++
		}
	}
	exposurePct := float64(totalExposed) / float64(contentLen) * 100.0

	// Each individual match must expose less than 30% of content (the 5×4%=20% target,
	// with some headroom for minimum-token enforcement extension).
	for i, me := range exposedPerMatch {
		pct := float64(me) / float64(contentLen) * 100.0
		if pct > 30.0 {
			t.Errorf("match %d exposed %.1f%% of content (>30%%), single-match exposure too high", i, pct)
		}
	}

	// Document the cumulative bound. We don't assert a hard upper limit on cumulative
	// exposure (it's expected to grow with N), but we log it so regressions are visible.
	t.Logf("exposure bound: %d unique bytes (%.1f%%) exposed across %d match transactions (content=%d bytes)",
		totalExposed, exposurePct, numMatchIDs, contentLen)

	// The cumulative exposure across 50 transactions should not exceed 100% trivially —
	// if it does within 50 matchIDs the chunking is effectively reconstructing the full content.
	// With 5 chunks × 4% = 20% per match, even with no overlap, 50 matches × 20% = 1000%
	// of theoretical coverage, so by ~5 matches we'd expect near-full coverage unless
	// the chunking varies well. This is an intentional design trade-off, not a bug.
	// We assert instead that unique byte coverage grows (chunks are actually different):
	if numMatchIDs > 1 {
		// Verify at least 2 distinct matchIDs produced different chunks (non-constant chunking).
		req0 := PreviewRequest{Content: content, ContentType: "analysis",
			EntryID: "entry-reconstruct-test", BuyerKey: "buyer-reconstruct-test", MatchID: "match-0000"}
		req1 := PreviewRequest{Content: content, ContentType: "analysis",
			EntryID: "entry-reconstruct-test", BuyerKey: "buyer-reconstruct-test", MatchID: "match-0001"}
		r0, _ := pa.Assemble(req0)
		r1, _ := pa.Assemble(req1)
		identical := len(r0.Chunks) == len(r1.Chunks)
		if identical {
			for i := range r0.Chunks {
				if r0.Chunks[i].StartByte != r1.Chunks[i].StartByte {
					identical = false
					break
				}
			}
		}
		if identical {
			t.Error("different matchIDs produced identical chunk positions — matchID is not influencing chunking")
		}
	}
}
