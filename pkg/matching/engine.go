package matching

import (
	"sync"
)

// Index is the matching engine's in-memory search index.
// It holds pre-computed embeddings for inventory entries and supports
// ranked search against buy task descriptions.
//
// Thread safety: Index is safe for concurrent reads. Mutations (Add, Remove,
// Rebuild) must not be called concurrently with reads or each other.
// The exchange engine calls Rebuild after state replay and Add/Remove
// incrementally as new puts are accepted or entries expire.
type Index struct {
	mu       sync.RWMutex
	embedder Embedder
	opts     RankOptions
	entries  []indexedEntry
}

// indexedEntry stores a single inventory entry and its precomputed embedding.
type indexedEntry struct {
	input     RankInput
	embedding []float64
}

// NewIndex returns an empty matching index using the given embedder.
// Use Rebuild to populate from a full inventory snapshot, or Add to
// insert entries incrementally.
func NewIndex(embedder Embedder, opts RankOptions) *Index {
	if embedder == nil {
		embedder = NewTFIDFEmbedder()
	}
	return &Index{
		embedder: embedder,
		opts:     opts,
	}
}

// Rebuild replaces the index contents with the given entries.
// It re-computes IDF weights from the corpus of descriptions before
// embedding each entry, improving relevance when the inventory is large.
//
// Rebuild acquires a write lock for the duration of indexing.
// This is typically called once at engine startup after state replay.
func (idx *Index) Rebuild(entries []RankInput) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	// Re-prime IDF weights from the new corpus if the embedder supports it.
	if ci, ok := idx.embedder.(CorpusIndexer); ok {
		docs := make([]string, len(entries))
		for i, e := range entries {
			docs[i] = e.Description
		}
		ci.IndexCorpus(docs)
	}

	idx.entries = make([]indexedEntry, 0, len(entries))
	for _, e := range entries {
		emb := idx.embedder.Embed(e.Description)
		idx.entries = append(idx.entries, indexedEntry{input: e, embedding: emb})
	}
}

// Add inserts a new entry into the index. If an entry with the same EntryID
// already exists, it is replaced.
// Add acquires a write lock.
func (idx *Index) Add(entry RankInput) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	emb := idx.embedder.Embed(entry.Description)
	for i, e := range idx.entries {
		if e.input.EntryID == entry.EntryID {
			idx.entries[i] = indexedEntry{input: entry, embedding: emb}
			return
		}
	}
	idx.entries = append(idx.entries, indexedEntry{input: entry, embedding: emb})
}

// Remove removes an entry by EntryID. No-op if the entry does not exist.
// Remove acquires a write lock.
func (idx *Index) Remove(entryID string) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	for i, e := range idx.entries {
		if e.input.EntryID == entryID {
			idx.entries[i] = idx.entries[len(idx.entries)-1]
			idx.entries = idx.entries[:len(idx.entries)-1]
			return
		}
	}
}

// Len returns the number of indexed entries.
func (idx *Index) Len() int {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return len(idx.entries)
}

// Search returns ranked results for a buy task, capped at maxResults.
//
// The embedder embeds the task description at query time. All indexed entries
// are evaluated against the 4-layer value stack. Results with composite score
// below the minimum similarity threshold are excluded.
//
// Partial matches (confidence < 0.5) are included with IsPartialMatch=true.
// The caller (engine.go) decides whether to include them in the match payload.
//
// If maxResults <= 0, all qualifying results are returned.
func (idx *Index) Search(task string, maxResults int) []RankedResult {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	if len(idx.entries) == 0 {
		return nil
	}

	// Build candidate list from indexed entries.
	candidates := make([]RankInput, len(idx.entries))
	for i, e := range idx.entries {
		candidates[i] = e.input
	}

	results := Rank(task, candidates, idx.embedder, idx.opts)

	if maxResults > 0 && len(results) > maxResults {
		results = results[:maxResults]
	}
	return results
}
