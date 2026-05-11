package infra

import (
	"math"
	"sync"
)

// EmbedIndex provides in-memory vector similarity search for semantic caching.
// It maintains a linear index of embedding vectors and supports
// insertion, removal, and similarity search using cosine distance.
type EmbedIndex struct {
	mu         sync.RWMutex
	entries    []embedIndexEntry
	maxEntries int
}

// embedIndexEntry stores a cache key with its embedding vector.
type embedIndexEntry struct {
	key       string
	embedding []float32
}

// NewEmbedIndex creates a new vector index with specified maximum entries.
// If maxEntries <= 0, defaults to 10000.
func NewEmbedIndex(maxEntries int) *EmbedIndex {
	if maxEntries <= 0 {
		maxEntries = 10000
	}
	return &EmbedIndex{
		entries:    make([]embedIndexEntry, 0, maxEntries),
		maxEntries: maxEntries,
	}
}

// CosineSimilarity computes the cosine similarity between two vectors.
// Returns 0 for zero vectors or vectors of different lengths.
func CosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) {
		return 0
	}

	var dotProduct float32
	var normA float32
	var normB float32

	for i := range a {
		dotProduct += a[i] * b[i]
		normA += a[i] * a[i]
		normB += b[i] * b[i]
	}

	if normA == 0 || normB == 0 {
		return 0
	}

	return float64(dotProduct) / (math.Sqrt(float64(normA)) * math.Sqrt(float64(normB)))
}

// Search finds the most similar entry to vec with similarity >= threshold.
// Returns the cache key as bytes, similarity score, and true if found.
// Returns nil, 0.0, false if no entry meets the threshold.
func (idx *EmbedIndex) Search(vec []float32, threshold float64) ([]byte, float64, bool) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	var bestKey string
	var bestScore float64

	for _, entry := range idx.entries {
		if len(entry.embedding) != len(vec) {
			continue
		}

		score := CosineSimilarity(vec, entry.embedding)
		if score >= threshold && score > bestScore {
			bestScore = score
			bestKey = entry.key
		}
	}

	if bestKey == "" {
		return nil, 0.0, false
	}

	return []byte(bestKey), bestScore, true
}

// Insert adds a new key-vector pair to the index.
// If the index is at capacity, the oldest entry is removed.
// Silently ignores empty keys, empty vectors, or vectors exceeding 8192 dimensions.
func (idx *EmbedIndex) Insert(key string, vec []float32) {
	if key == "" || len(vec) == 0 {
		return
	}

	// Safety limit: Reject malformed or adversarial vectors to prevent DoS
	// Current embedding models (mxbai-embed-large) produce 1024-dim vectors = 4KB
	// Allow 8x buffer = 32KB per entry as reasonable upper bound
	// This prevents OOM attacks from sending vectors with millions of dimensions
	if len(vec) > 8192 {
		return
	}

	idx.mu.Lock()
	defer idx.mu.Unlock()

	// Enforce max entries limit - remove oldest entry if at capacity
	// Current default is 256 entries, keeping O(n) linear search fast
	// If maxEntries > 1000, consider using circular buffer for O(1) eviction
	if len(idx.entries) >= idx.maxEntries {
		idx.entries = idx.entries[1:]
	}

	idx.entries = append(idx.entries, embedIndexEntry{
		key:       key,
		embedding: vec,
	})
}

// Remove deletes a key from the index by shifting the slice.
// Silently ignores empty keys and missing keys.
func (idx *EmbedIndex) Remove(key string) {
	if key == "" {
		return
	}

	idx.mu.Lock()
	defer idx.mu.Unlock()

	for i, entry := range idx.entries {
		if entry.key == key {
			// Remove by shifting slice
			copy(idx.entries[i:], idx.entries[i+1:])
			idx.entries = idx.entries[:len(idx.entries)-1]
			return
		}
	}
}

// Len returns the current number of entries in the index.
func (idx *EmbedIndex) Len() int {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return len(idx.entries)
}
