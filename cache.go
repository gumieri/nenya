package main

import (
	"sync"
	"time"
)

type cacheEntry struct {
	value    interface{}
	expireAt time.Time
}

type ThoughtSignatureCache struct {
	mu         sync.RWMutex
	entries    map[string]cacheEntry
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
		entries:    make(map[string]cacheEntry, maxSize),
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

	if len(c.entries) >= c.maxSize {
		c.evictLocked()
	}

	c.entries[key] = cacheEntry{
		value:    value,
		expireAt: time.Now().Add(c.defaultTTL),
	}
}

func (c *ThoughtSignatureCache) Load(key string) (interface{}, bool) {
	if key == "" {
		return nil, false
	}
	c.mu.RLock()
	entry, ok := c.entries[key]
	c.mu.RUnlock()

	if !ok {
		return nil, false
	}
	if time.Now().After(entry.expireAt) {
		c.mu.Lock()
		delete(c.entries, key)
		c.mu.Unlock()
		return nil, false
	}
	return entry.value, true
}

func (c *ThoughtSignatureCache) evictLocked() {
	now := time.Now()
	for k, e := range c.entries {
		if now.After(e.expireAt) {
			delete(c.entries, k)
		}
	}
	if len(c.entries) >= c.maxSize {
		count := len(c.entries) / 2
		i := 0
		for k := range c.entries {
			delete(c.entries, k)
			i++
			if i >= count {
				break
			}
		}
	}
}

func (c *ThoughtSignatureCache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries)
}
