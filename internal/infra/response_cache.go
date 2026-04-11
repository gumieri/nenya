package infra

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"
)

var cacheableFields = []string{
	"model", "messages", "temperature", "top_p",
	"max_tokens", "tools", "tool_choice", "response_format", "stop",
	"stream",
}

func FingerprintPayload(payload map[string]interface{}) string {
	canonical := make(map[string]interface{}, len(cacheableFields))
	for _, field := range cacheableFields {
		if v, ok := payload[field]; ok {
			canonical[field] = v
		}
	}
	data, err := json.Marshal(canonical)
	if err != nil {
		return ""
	}
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

type responseCacheEntry struct {
	data     []byte
	expireAt time.Time
	element  *list.Element
}

type ResponseCache struct {
	mu       sync.RWMutex
	items    map[string]*responseCacheEntry
	order    *list.List
	maxSize  int
	maxBytes int64
	ttl      time.Duration
	stopCh   chan struct{}
}

func NewResponseCache(maxSize int, maxBytes int64, ttl, evictInterval time.Duration) *ResponseCache {
	if maxSize <= 0 {
		maxSize = 512
	}
	if maxBytes <= 0 {
		maxBytes = 1 << 20
	}
	if ttl <= 0 {
		ttl = 1 * time.Hour
	}
	if evictInterval <= 0 {
		evictInterval = 5 * time.Minute
	}
	c := &ResponseCache{
		items:    make(map[string]*responseCacheEntry),
		order:    list.New(),
		maxSize:  maxSize,
		maxBytes: maxBytes,
		ttl:      ttl,
		stopCh:   make(chan struct{}),
	}
	c.startEvicter(evictInterval)
	return c
}

func (c *ResponseCache) Lookup(key string) ([]byte, bool) {
	if key == "" {
		return nil, false
	}
	c.mu.RLock()
	entry, ok := c.items[key]
	if !ok {
		c.mu.RUnlock()
		return nil, false
	}
	if time.Now().After(entry.expireAt) {
		c.mu.RUnlock()
		c.mu.Lock()
		if current, exists := c.items[key]; exists && time.Now().After(current.expireAt) {
			c.order.Remove(current.element)
			delete(c.items, key)
		}
		c.mu.Unlock()
		return nil, false
	}
	data := entry.data
	c.mu.RUnlock()
	return data, true
}

func (c *ResponseCache) Store(key string, data []byte) {
	if key == "" || len(data) == 0 {
		return
	}
	if int64(len(data)) > c.maxBytes {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if existing, ok := c.items[key]; ok {
		existing.data = data
		existing.expireAt = time.Now().Add(c.ttl)
		c.order.MoveToFront(existing.element)
		return
	}

	if len(c.items) >= c.maxSize {
		c.evictLocked()
	}

	elem := c.order.PushFront(key)
	c.items[key] = &responseCacheEntry{
		data:     data,
		expireAt: time.Now().Add(c.ttl),
		element:  elem,
	}
}

func (c *ResponseCache) evictLocked() {
	now := time.Now()

	for e := c.order.Back(); e != nil; {
		next := e.Prev()
		key := e.Value.(string)
		if entry, ok := c.items[key]; ok && now.After(entry.expireAt) {
			c.order.Remove(e)
			delete(c.items, key)
		}
		e = next
	}

	for len(c.items) >= c.maxSize {
		e := c.order.Back()
		if e == nil {
			break
		}
		key := e.Value.(string)
		c.order.Remove(e)
		delete(c.items, key)
	}
}

func (c *ResponseCache) startEvicter(interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				c.mu.Lock()
				c.evictLocked()
				c.mu.Unlock()
			case <-c.stopCh:
				return
			}
		}
	}()
}

func (c *ResponseCache) Stop() {
	close(c.stopCh)
}

func (c *ResponseCache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.items)
}
