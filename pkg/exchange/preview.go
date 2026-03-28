package exchange

import (
	"crypto/sha256"
	"encoding/binary"
	"strings"
)

// PreviewAssembler generates content previews for the preview-before-purchase model.
// It produces up to 5 random chunks, each ~4% of content (20% total), with
// content-type-aware boundary snapping and deterministic seeding to prevent
// reconstruction attacks.
type PreviewAssembler struct{}

// PreviewRequest describes the content to preview and the seeding inputs.
type PreviewRequest struct {
	Content     []byte // Full content
	ContentType string // "code", "analysis", "summary", "plan", "data", "review", "other"
	EntryID     string // For deterministic seeding — seed is derived from EntryID only
}

// PreviewResult holds the assembled preview chunks.
type PreviewResult struct {
	Chunks        []PreviewChunk
	TotalTokens   int
	PreviewTokens int
}

// PreviewChunk is a single content excerpt in a preview.
type PreviewChunk struct {
	Content    string
	StartByte  int
	EndByte    int
	ChunkIndex int
}

const (
	previewChunkCount  = 5
	previewChunkPct    = 0.04 // 4% per chunk
	minTokensPerChunk  = 100
	minTokensForChunks = minTokensPerChunk * previewChunkCount // 500
)

// Assemble builds a preview for the given request.
func (pa *PreviewAssembler) Assemble(req PreviewRequest) (PreviewResult, error) {
	if len(req.Content) == 0 {
		return PreviewResult{}, nil
	}

	totalTokens := estimateTokens(req.Content)

	// Too small: return single chunk with all content
	if totalTokens < minTokensPerChunk {
		return PreviewResult{
			Chunks: []PreviewChunk{{
				Content:    string(req.Content),
				StartByte:  0,
				EndByte:    len(req.Content),
				ChunkIndex: 0,
			}},
			TotalTokens:   totalTokens,
			PreviewTokens: totalTokens,
		}, nil
	}

	seed := deriveSeed(req.EntryID)
	boundaries := findBoundaries(req.Content, req.ContentType)

	// Determine how many chunks to produce based on content size
	chunkCount := previewChunkCount
	if totalTokens < minTokensForChunks {
		// Reduce proportionally: how many min-100-token chunks can we fit?
		chunkCount = totalTokens / minTokensPerChunk
		if chunkCount < 1 {
			chunkCount = 1
		}
	}

	chunks := selectChunks(req.Content, boundaries, chunkCount, seed, totalTokens)

	previewTokens := 0
	for _, c := range chunks {
		previewTokens += estimateTokens([]byte(c.Content))
	}

	return PreviewResult{
		Chunks:        chunks,
		TotalTokens:   totalTokens,
		PreviewTokens: previewTokens,
	}, nil
}

// estimateTokens estimates the token count using a simple bytes/4 approximation.
func estimateTokens(content []byte) int {
	return (len(content) + 3) / 4
}

// deriveSeed computes a deterministic uint64 seed from the entry ID only.
// Seeding on entry_id ensures all buyers see the same preview for a given entry,
// preventing reconstruction attacks via multiple buy orders with different match IDs.
func deriveSeed(entryID string) uint64 {
	h := sha256.Sum256([]byte(entryID))
	return binary.LittleEndian.Uint64(h[:8])
}

// findBoundaries locates content-type-aware split points within content.
// Returns a sorted slice of byte offsets suitable for chunk start/end alignment.
// Always includes offset 0 and len(content).
func findBoundaries(content []byte, contentType string) []int {
	s := string(content)
	set := map[int]struct{}{0: {}, len(content): {}}

	switch contentType {
	case "code":
		// Function/method/class boundaries
		keywords := []string{"\nfunc ", "\ndef ", "\nclass ", "\nfunction ", "\nconst ", "\nvar ", "\ntype "}
		for _, kw := range keywords {
			idx := 0
			for {
				pos := strings.Index(s[idx:], kw)
				if pos < 0 {
					break
				}
				abs := idx + pos + 1 // +1 to skip the leading \n, land on the keyword
				set[abs] = struct{}{}
				idx = abs + 1
				if idx >= len(s) {
					break
				}
			}
		}
		// Also split on blank lines between blocks
		addDoubleNewlineBoundaries(s, set)

	case "analysis", "summary", "plan", "review":
		// Paragraph boundaries (double newline)
		addDoubleNewlineBoundaries(s, set)

	case "data":
		// Record boundaries: newlines for CSV/JSONL, object boundaries for JSON arrays
		addNewlineBoundaries(s, set)
		// JSON object boundaries (lines starting with { or })
		for i, ch := range s {
			if i > 0 && (ch == '{' || ch == '[') && (s[i-1] == '\n' || s[i-1] == ',') {
				set[i] = struct{}{}
			}
		}

	default:
		// Line boundaries
		addNewlineBoundaries(s, set)
	}

	return sortedBoundaries(set)
}

// addDoubleNewlineBoundaries adds positions after every \n\n sequence.
func addDoubleNewlineBoundaries(s string, set map[int]struct{}) {
	idx := 0
	for {
		pos := strings.Index(s[idx:], "\n\n")
		if pos < 0 {
			break
		}
		abs := idx + pos + 2 // position after the double newline
		if abs < len(s) {
			set[abs] = struct{}{}
		}
		idx = abs
		if idx >= len(s) {
			break
		}
	}
}

// addNewlineBoundaries adds positions after every \n.
func addNewlineBoundaries(s string, set map[int]struct{}) {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' && i+1 < len(s) {
			set[i+1] = struct{}{}
		}
	}
}

// sortedBoundaries converts the set to a sorted slice.
func sortedBoundaries(set map[int]struct{}) []int {
	out := make([]int, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	// Simple insertion sort — boundary count is typically small
	for i := 1; i < len(out); i++ {
		key := out[i]
		j := i - 1
		for j >= 0 && out[j] > key {
			out[j+1] = out[j]
			j--
		}
		out[j+1] = key
	}
	return out
}

// xorShift64 is a fast, simple PRNG for deterministic position selection.
type xorShift64 struct{ state uint64 }

func newXorShift64(seed uint64) *xorShift64 {
	if seed == 0 {
		seed = 0xdeadbeefcafe1234
	}
	return &xorShift64{state: seed}
}

func (x *xorShift64) next() uint64 {
	x.state ^= x.state << 13
	x.state ^= x.state >> 7
	x.state ^= x.state << 17
	return x.state
}

func (x *xorShift64) intn(n int) int {
	if n <= 0 {
		return 0
	}
	return int(x.next() % uint64(n))
}

// selectChunks picks chunkCount non-overlapping chunks from content, one per equal
// region, each ~4% of total content, snapped to the nearest boundary.
func selectChunks(content []byte, boundaries []int, chunkCount int, seed uint64, totalTokens int) []PreviewChunk {
	n := len(content)
	if n == 0 || chunkCount == 0 {
		return nil
	}

	rng := newXorShift64(seed)
	regionSize := n / chunkCount
	chunkSize := int(float64(n) * previewChunkPct)
	if chunkSize < 1 {
		chunkSize = 1
	}

	chunks := make([]PreviewChunk, 0, chunkCount)

	for i := 0; i < chunkCount; i++ {
		regionStart := i * regionSize
		regionEnd := regionStart + regionSize
		if i == chunkCount-1 {
			regionEnd = n
		}

		// Pick a random position within this region
		regionLen := regionEnd - regionStart
		var rawStart int
		if regionLen > 1 {
			rawStart = regionStart + rng.intn(regionLen)
		} else {
			rawStart = regionStart
		}

		// Snap to nearest boundary
		start := snapToBoundary(rawStart, boundaries)

		// Snap end to nearest boundary
		rawEnd := start + chunkSize
		if rawEnd > n {
			rawEnd = n
		}
		end := snapToBoundary(rawEnd, boundaries)

		// Ensure end > start
		if end <= start {
			end = start + chunkSize
			if end > n {
				end = n
			}
			// Try to snap again from the larger set
			if end < n {
				end = snapBoundaryAfter(start+1, boundaries)
			}
		}
		if end > n {
			end = n
		}
		if end <= start {
			continue
		}

		// Enforce minimum token count
		chunkTokens := estimateTokens(content[start:end])
		if chunkTokens < minTokensPerChunk {
			// Extend end until we have enough tokens
			needed := minTokensPerChunk * 4 // bytes needed
			newEnd := start + needed
			if newEnd > n {
				newEnd = n
			}
			// Snap to boundary
			snapped := snapToBoundary(newEnd, boundaries)
			if snapped > start {
				end = snapped
			} else {
				end = newEnd
			}
			if end > n {
				end = n
			}
		}

		if end <= start {
			continue
		}

		chunks = append(chunks, PreviewChunk{
			Content:    string(content[start:end]),
			StartByte:  start,
			EndByte:    end,
			ChunkIndex: i,
		})
	}

	return chunks
}

// snapToBoundary finds the boundary in boundaries closest to pos.
func snapToBoundary(pos int, boundaries []int) int {
	if len(boundaries) == 0 {
		return pos
	}

	// Binary search for insertion point
	lo, hi := 0, len(boundaries)-1
	for lo < hi {
		mid := (lo + hi) / 2
		if boundaries[mid] < pos {
			lo = mid + 1
		} else {
			hi = mid
		}
	}

	// lo is first boundary >= pos
	best := boundaries[lo]
	if lo > 0 {
		prev := boundaries[lo-1]
		if pos-prev < best-pos {
			best = prev
		}
	}
	return best
}

// snapBoundaryAfter finds the first boundary strictly after pos.
func snapBoundaryAfter(pos int, boundaries []int) int {
	for _, b := range boundaries {
		if b > pos {
			return b
		}
	}
	return pos
}
