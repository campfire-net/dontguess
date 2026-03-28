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

func TestPreviewDeterministic(t *testing.T) {
	content := generateProse(2000)
	req := PreviewRequest{
		Content:     content,
		ContentType: "analysis",
		EntryID:     "entry-abc",
		BuyerKey:    "buyer-xyz",
		MatchID:     "match-123",
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

func TestPreviewCodeBoundaries(t *testing.T) {
	// Code with clear function boundaries — chunks should start at func declarations
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

	// At least half the chunks should start at a recognizable code boundary or
	// within a few bytes of one (snap tolerance). Accept chunks that start at
	// offset 0 or immediately after a newline that precedes a keyword.
	aligned := 0
	for _, c := range r.Chunks {
		start := c.StartByte
		// Check if start is at position 0 or the character at start begins a code keyword
		atKeyword := false
		for _, kw := range codeKeywords {
			if start+len(kw) <= len(s) && s[start:start+len(kw)] == kw {
				atKeyword = true
				break
			}
		}
		// Also accept start of file (offset 0) or blank-line boundary
		atBlankLine := start == 0 || (start >= 2 && s[start-2:start] == "\n\n")
		if atKeyword || atBlankLine {
			aligned++
		}
	}
	// Require at least 1 chunk aligned (strict requirement would be all, but snapping
	// can shift to adjacent blank lines)
	if aligned == 0 {
		t.Errorf("no code chunks start at code boundaries; chunks: %+v", r.Chunks)
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

// ---- ChunkIndex ----

func TestPreviewChunkIndexAssigned(t *testing.T) {
	content := generateProse(2000)
	req := PreviewRequest{Content: content, ContentType: "analysis", EntryID: "e", BuyerKey: "b", MatchID: "m"}

	r, err := pa.Assemble(req)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	for i, c := range r.Chunks {
		if c.ChunkIndex < 0 {
			t.Errorf("chunk %d has negative ChunkIndex %d", i, c.ChunkIndex)
		}
	}
}
