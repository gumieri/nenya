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

type ThoughtSignatureCache struct {
	mu         sync.Mutex
	entries    map[string]*cacheEntry
	lru        *list.List
	maxSize    int
	defaultTTL time.Duration
}

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

func (c *ThoughtSignatureCache) Store(key string, value interface{}) {
	if key == "" || value == nil {
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

func (c *ThoughtSignatureCache) Load(key string) (interface{}, bool) {
	if key == "" {
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

func (c *ThoughtSignatureCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}
