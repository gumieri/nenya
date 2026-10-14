package infra

import (
	"container/list"
	"sync"
	"time"
)

type cacheEntry struct {
	key      string
	value    interface{}
	expireAt time.Time
	element  *list.Element
}

// ThoughtSignatureCache caches Gemini thought signatures (extra_content)
// keyed by upstream-issued tool-call ids so follow-up turns can re-attach
// them (NENYA-51). Entries expire after a TTL and are LRU-evicted at
// maxSize. For per-caller isolation of upstream-issued ids (NENYA-189), use
// Scoped views instead of sharing one global namespace across API keys. All
// methods are nil-receiver safe (a nil cache stores nothing and reads miss),
// so a typed-nil *ThoughtSignatureCache inside ThoughtSignatureKV degrades
// instead of panicking.
type ThoughtSignatureCache struct {
	mu         sync.Mutex
	entries    map[string]*cacheEntry
	lru        *list.List
	maxSize    int
	defaultTTL time.Duration
}

// NewThoughtSignatureCache returns a cache holding up to maxSize signatures
// for defaultTTL (defaults: 1000 entries, 10 minutes).
func NewThoughtSignatureCache(maxSize int, defaultTTL time.Duration) *ThoughtSignatureCache {
	if maxSize <= 0 {
		maxSize = 1000
	}
	if defaultTTL <= 0 {
		defaultTTL = 10 * time.Minute
	}
	return &ThoughtSignatureCache{
		entries:    make(map[string]*cacheEntry, maxSize),
		lru:        list.New(),
		maxSize:    maxSize,
		defaultTTL: defaultTTL,
	}
}

// Store caches value under key, refreshing the entry when the key already
// exists. Empty keys and nil values are ignored. Nil-receiver safe.
func (c *ThoughtSignatureCache) Store(key string, value interface{}) {
	if c == nil || key == "" || value == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if e, ok := c.entries[key]; ok {
		e.value = value
		e.expireAt = time.Now().Add(c.defaultTTL)
		c.lru.MoveToFront(e.element)
		return
	}

	if len(c.entries) >= c.maxSize {
		c.evictLocked()
	}

	entry := &cacheEntry{
		key:      key,
		value:    value,
		expireAt: time.Now().Add(c.defaultTTL),
	}
	entry.element = c.lru.PushFront(entry)
	c.entries[key] = entry
}

// Load returns the value cached under key, if present and unexpired.
// Nil-receiver safe.
func (c *ThoughtSignatureCache) Load(key string) (interface{}, bool) {
	if c == nil || key == "" {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	if time.Now().After(entry.expireAt) {
		c.lru.Remove(entry.element)
		delete(c.entries, key)
		return nil, false
	}
	c.lru.MoveToFront(entry.element)
	return entry.value, true
}

// evictLocked removes expired entries and enforces LRU eviction when
// the cache is full. Caller must hold c.mu.
func (c *ThoughtSignatureCache) evictLocked() {
	now := time.Now()
	for k, e := range c.entries {
		if now.After(e.expireAt) {
			c.lru.Remove(e.element)
			delete(c.entries, k)
		}
	}
	for len(c.entries) >= c.maxSize {
		oldest := c.lru.Back()
		if oldest == nil {
			break
		}
		entry := oldest.Value.(*cacheEntry)
		c.lru.Remove(oldest)
		delete(c.entries, entry.key)
	}
}

// Len returns the number of live entries. Nil-receiver safe.
func (c *ThoughtSignatureCache) Len() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// ThoughtSignatureKV is the read/write surface response transformers and
// request sanitizers use to cache and re-attach thought signatures. The
// concrete *ThoughtSignatureCache implements it with a single shared
// namespace; ScopedThoughtSignatureCache implements it with a per-caller
// namespace (NENYA-189) and is the form request paths must use so one API
// key's upstream-issued tool-call ids cannot address another key's cached
// signatures.
type ThoughtSignatureKV interface {
	Store(key string, value any)
	Load(key string) (any, bool)
}

// ScopedThoughtSignatureCache is a namespaced view of a ThoughtSignatureCache:
// every key is stored under scope, so the same upstream-issued tool-call id
// presented under a different scope misses instead of reading another
// caller's cached signature.
type ScopedThoughtSignatureCache struct {
	parent *ThoughtSignatureCache
	scope  string
}

// Scoped returns a view of the cache whose keys are namespaced under scope.
// An empty scope is its own namespace — it never collides with a named scope,
// and two empty-scope views share one namespace (they represent the same
// anonymous caller class). A nil receiver yields a nil view whose Store/Load
// are no-ops, so request paths without a cache degrade instead of panicking.
func (c *ThoughtSignatureCache) Scoped(scope string) *ScopedThoughtSignatureCache {
	if c == nil {
		return nil
	}
	return &ScopedThoughtSignatureCache{parent: c, scope: scope}
}

// Store caches value under the scoped key. No-op for a nil view or an empty
// key. Known limitation: all scopes share the parent LRU — a hot scope can
// evict another scope's entries when maxSize is reached (availability, not
// confidentiality; entries never cross scopes).
func (s *ScopedThoughtSignatureCache) Store(key string, value any) {
	if s == nil || s.parent == nil || key == "" || value == nil {
		return
	}
	s.parent.Store(s.scope+"\x1f"+key, value)
}

// Load returns the value cached under the scoped key, if present and live.
// A nil view is always a miss.
func (s *ScopedThoughtSignatureCache) Load(key string) (any, bool) {
	if s == nil || s.parent == nil || key == "" {
		return nil, false
	}
	return s.parent.Load(s.scope + "\x1f" + key)
}
