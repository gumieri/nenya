package pipeline

import (
	"container/list"
	"sync"
)

// SummaryCache makes window-compaction summaries stable across turns
// (NENYA-24, audit finding F3): regenerating an LLM summary of a growing
// history every request changes the compacted head message every turn, so
// the upstream prompt cache never hits while compaction is active.
//
// Entries are keyed by conversation lineage — a hash of the leading system
// messages plus the first history message, which stays identical while a
// conversation grows by appends. Each entry remembers the summary text and
// the history message count it was generated from. Two reuse tiers:
//
//   - exact: the serialized history is byte-identical to the one that
//     produced the summary (same-input retries) → full reuse
//   - hysteresis: history grew by less than the regeneration ratio → the
//     stored summary is reused and only the delta messages are appended
//     verbatim, so the compacted prefix stays byte-identical and the
//     upstream prefix cache hits again
//
// Bounded: at most `cap` lineages are remembered (LRU). Nothing here is
// persisted; a SIGHUP reload rebuilds the gateway and starts cold, which
// merely costs one regeneration per active conversation.
type SummaryCache struct {
	mu        sync.Mutex
	cap       int
	order     *list.List // front = most recently used lineage
	entries   map[string]*list.Element
	ExactHits uint64
	HystHits  uint64
	Regens    uint64
	MissCold  uint64 // first compaction for a lineage
}

type summaryCacheEntry struct {
	lineageKey string
	lastHash   string
	summary    string
	historyLen int
}

// NewSummaryCache creates a bounded summary cache. Non-positive caps fall
// back to 512.
func NewSummaryCache(cap int) *SummaryCache {
	if cap <= 0 {
		cap = 512
	}
	return &SummaryCache{
		cap:     cap,
		order:   list.New(),
		entries: make(map[string]*list.Element, cap),
	}
}

// Reuse is the outcome of a Lookup.
type Reuse struct {
	Summary string
	// DeltaStart is the history index the cached summary was generated
	// through: history[:DeltaStart] is covered by the summary,
	// history[DeltaStart:] must be appended verbatim. Equal to the full
	// history length on an exact hit.
	DeltaStart int
	Exact      bool
}

// Lookup finds a reusable summary for the given lineage. historyHash is
// the SHA-256 of the serialized history; historyLen its message count.
func (c *SummaryCache) Lookup(lineageKey, historyHash string, historyLen int, regenRatio float64) (Reuse, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	el, ok := c.entries[lineageKey]
	if !ok {
		c.MissCold++
		return Reuse{}, false
	}
	entry := el.Value.(*summaryCacheEntry)
	c.order.MoveToFront(el)

	if entry.lastHash == historyHash {
		c.ExactHits++
		return Reuse{Summary: entry.summary, DeltaStart: historyLen, Exact: true}, true
	}
	if historyLen > entry.historyLen && entry.historyLen > 0 &&
		float64(historyLen-entry.historyLen)/float64(entry.historyLen) < regenRatio {
		c.HystHits++
		return Reuse{Summary: entry.summary, DeltaStart: entry.historyLen}, true
	}
	return Reuse{}, false
}

// Store records a freshly generated summary for the lineage.
func (c *SummaryCache) Store(lineageKey, historyHash string, historyLen int, summary string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Regens++

	if el, ok := c.entries[lineageKey]; ok {
		entry := el.Value.(*summaryCacheEntry)
		entry.lastHash = historyHash
		entry.summary = summary
		entry.historyLen = historyLen
		c.order.MoveToFront(el)
		return
	}
	entry := &summaryCacheEntry{lineageKey: lineageKey, lastHash: historyHash, summary: summary, historyLen: historyLen}
	el := c.order.PushFront(entry)
	c.entries[lineageKey] = el
	for c.order.Len() > c.cap {
		oldest := c.order.Back()
		if oldest == nil {
			break
		}
		delete(c.entries, oldest.Value.(*summaryCacheEntry).lineageKey)
		c.order.Remove(oldest)
	}
}

// Stats returns the cumulative reuse counters.
func (c *SummaryCache) Stats() (exactHits, hystHits, regens, missCold uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ExactHits, c.HystHits, c.Regens, c.MissCold
}
