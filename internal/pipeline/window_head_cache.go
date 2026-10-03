package pipeline

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"sync"
)

// WindowHeadCache keeps a deterministic window-compaction head (the "tfidf"
// mode's truncated history) byte-stable across turns, keyed by conversation
// lineage — the deterministic-head analog of SummaryCache (NENYA-24).
//
// The Phase 014 content-hash memo cannot stabilize this path: the window
// input is the joined history, which grows every turn, so a content key never
// repeats. Lineage is the right identity: it is a hash of the leading system
// messages plus the first history message, which stays identical while a
// conversation grows by appends.
//
// Reuse rules (all three must hold to reuse a stored head):
//
//   - the entry's covered region is still present unchanged. The covered
//     region is the serialized prefix the head was generated from
//     (historyText[:coveredBytes]); comparing its hash detects a content
//     mutation inside the frozen prefix and forces a recompute instead of
//     appending a stale head to changed content. (Non-content fields such as
//     tool_calls never reach the model through the covered region — it is
//     replaced by the head message — so they do not affect the head.) The
//     prefix is re-hashed on each lookup; that O(covered) hash is the price
//     of mutation detection (an equal message count is not enough — a
//     same-length edit changes the bytes).
//   - the history has not shrunk below coveredLen messages; and
//   - the history has not grown past the regeneration ratio
//     (window.summary_regen_ratio), so the appended delta stays small.
//
// On a reuse the uncovered delta (history[coveredLen:]) is appended verbatim,
// keeping the compacted prefix byte-identical while ordinary turn-by-turn
// growth extends the tail.
//
// Bounded LRU (default 512 lineages), concurrency-safe, not persisted. A
// SIGHUP reload may carry it over (deterministic, lineage+region keyed).
type WindowHeadCache struct {
	mu       sync.Mutex
	capacity int
	order    *list.List // front = most recently used lineage
	entries  map[string]*list.Element

	exactHits   uint64
	hystHits    uint64
	regens      uint64
	missCold    uint64 // no entry for the lineage (cold or evicted)
	invalidated uint64 // covered region moved/shrank/mutated; entry dropped
}

type windowHeadEntry struct {
	lineageKey    string
	coveredLen    int    // messages the head covers
	coveredBytes  int    // serialized byte length of the covered region
	coveredHash   string // hash of the covered region's serialized bytes
	head          string
	coveredTokens int // token estimate of the covered region (header)
}

// HeadReuse is the outcome of WindowHeadCache.Lookup.
type HeadReuse struct {
	// Head is the stored compacted head.
	Head string
	// DeltaStart is the history index the head covers: history[:DeltaStart]
	// is represented by Head and history[DeltaStart:] must be appended
	// verbatim.
	DeltaStart int
	// Exact is true when the stored head covers the entire current history
	// (zero delta). Informational (tests/diagnostics).
	Exact bool
	// CoveredTokens is the covered region's token estimate, cached at
	// generation time so reuse does not re-tokenize the region.
	CoveredTokens int
}

// NewWindowHeadCache creates a bounded cache. Non-positive capacities fall
// back to 512.
func NewWindowHeadCache(capacity int) *WindowHeadCache {
	if capacity <= 0 {
		capacity = 512
	}
	return &WindowHeadCache{
		capacity: capacity,
		order:    list.New(),
		entries:  make(map[string]*list.Element, capacity),
	}
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// Lookup finds a reusable head for the lineage given the current serialized
// history and its message count. historyText is the caller's already-computed
// SerializeMessages(history) — the cache slices its covered prefix rather than
// re-serializing.
func (c *WindowHeadCache) Lookup(lineageKey, historyText string, historyLen int, regenRatio float64) (HeadReuse, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	el, ok := c.entries[lineageKey]
	if !ok {
		c.missCold++
		return HeadReuse{}, false
	}
	entry := el.Value.(*windowHeadEntry)

	// The history shrank below the covered region, or its serialized text no
	// longer contains the covered prefix: cannot represent it.
	if historyLen < entry.coveredLen || entry.coveredBytes > len(historyText) {
		c.dropLocked(el, lineageKey)
		return HeadReuse{}, false
	}
	// The covered prefix changed (content mutation inside the frozen
	// region): drop and recompute.
	if entry.coveredLen > 0 && sha256Hex(historyText[:entry.coveredBytes]) != entry.coveredHash {
		c.dropLocked(el, lineageKey)
		return HeadReuse{}, false
	}

	c.order.MoveToFront(el)

	if historyLen == entry.coveredLen {
		c.exactHits++
		return HeadReuse{Head: entry.head, DeltaStart: entry.coveredLen, Exact: true, CoveredTokens: entry.coveredTokens}, true
	}
	if entry.coveredLen > 0 &&
		float64(historyLen-entry.coveredLen)/float64(entry.coveredLen) < regenRatio {
		c.hystHits++
		return HeadReuse{Head: entry.head, DeltaStart: entry.coveredLen, CoveredTokens: entry.coveredTokens}, true
	}

	// Grew past the regeneration ratio: recompute a fresh bounded head.
	return HeadReuse{}, false
}

// Store records a freshly generated head. historyText is the serialization of
// the full history the head was generated from; historyLen is its message
// count (the covered region length).
func (c *WindowHeadCache) Store(lineageKey, historyText string, historyLen, coveredTokens int, head string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.regens++

	coveredHash := sha256Hex(historyText)

	if el, ok := c.entries[lineageKey]; ok {
		entry := el.Value.(*windowHeadEntry)
		entry.coveredLen = historyLen
		entry.coveredBytes = len(historyText)
		entry.coveredHash = coveredHash
		entry.head = head
		entry.coveredTokens = coveredTokens
		c.order.MoveToFront(el)
		return
	}
	entry := &windowHeadEntry{
		lineageKey:    lineageKey,
		coveredLen:    historyLen,
		coveredBytes:  len(historyText),
		coveredHash:   coveredHash,
		head:          head,
		coveredTokens: coveredTokens,
	}
	el := c.order.PushFront(entry)
	c.entries[lineageKey] = el
	for c.order.Len() > c.capacity {
		oldest := c.order.Back()
		if oldest == nil {
			break
		}
		delete(c.entries, oldest.Value.(*windowHeadEntry).lineageKey)
		c.order.Remove(oldest)
	}
}

// dropLocked removes an entry and counts the invalidation.
func (c *WindowHeadCache) dropLocked(el *list.Element, lineageKey string) {
	c.order.Remove(el)
	delete(c.entries, lineageKey)
	c.invalidated++
}

// WindowHeadStats is a snapshot of the cumulative counters.
type WindowHeadStats struct {
	ExactHits   uint64
	HystHits    uint64
	Regens      uint64
	MissCold    uint64 // no entry for the lineage (cold or evicted)
	Invalidated uint64
}

// Stats returns the cumulative counters.
func (c *WindowHeadCache) Stats() WindowHeadStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return WindowHeadStats{
		ExactHits:   c.exactHits,
		HystHits:    c.hystHits,
		Regens:      c.regens,
		MissCold:    c.missCold,
		Invalidated: c.invalidated,
	}
}

// size returns the current entry count (test helper).
func (c *WindowHeadCache) size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}
