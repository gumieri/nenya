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
	return FingerprintPayloadWithAuth(payload, "")
}

func FingerprintPayloadWithAuth(payload map[string]interface{}, authToken string) string {
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
	h := sha256.New()
	if authToken != "" {
		authHash := sha256.Sum256([]byte(authToken))
		h.Write(authHash[:])
	}
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

type responseCacheEntry struct {
	data      []byte
	expireAt  time.Time
	element   *list.Element
	embedding []float32
}

type ResponseCache struct {
	mu                  sync.RWMutex
	items               map[string]*responseCacheEntry
	order               *list.List
	maxSize             int
	maxBytes            int64
	ttl                 time.Duration
	stopCh              chan struct{}
	metrics             *Metrics
	semanticEnabled     bool
	similarityThreshold float64
	embedder            EmbeddingProvider
	idx                 *EmbedIndex
}

func NewResponseCache(maxSize int, maxBytes int64, ttl, evictInterval time.Duration, metrics *Metrics, semanticEnabled bool, similarityThreshold float64, embedder EmbeddingProvider) *ResponseCache {
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
	if similarityThreshold <= 0 {
		similarityThreshold = 0.9
	}

	var idx *EmbedIndex
	if semanticEnabled && embedder != nil {
		// Use smaller max entries for semantic index (256 vs 512 for exact cache)
		// to keep linear search O(256) vs O(512) for better performance
		idx = NewEmbedIndex(256)
	}

	c := &ResponseCache{
		items:               make(map[string]*responseCacheEntry),
		order:               list.New(),
		maxSize:             maxSize,
		maxBytes:            maxBytes,
		ttl:                 ttl,
		stopCh:              make(chan struct{}),
		metrics:             metrics,
		semanticEnabled:     semanticEnabled,
		similarityThreshold: similarityThreshold,
		embedder:            embedder,
		idx:                 idx,
	}
	c.startEvicter(evictInterval)
	return c
}

func (c *ResponseCache) Lookup(key, model string, embed func() ([]float32, error)) ([]byte, bool, string) {
	if key == "" {
		return nil, false, ""
	}

	// 1. exact-match fast path
	c.mu.RLock()
	entry, ok := c.items[key]
	if ok {
		if !time.Now().After(entry.expireAt) {
			data := entry.data
			c.mu.RUnlock()
			if model == "" {
				model = "unknown"
			}
			c.recordExactHit(model)
			return data, true, "exact"
		}
	}
	c.mu.RUnlock()

	// 2. semantic fallback
	if c.semanticEnabled && c.embedder != nil && c.idx != nil {
		if data, ok := c.lookupSemantic(key, embed, model); ok {
			return data, true, "semantic"
		}
	}

	// 3. miss
	if model == "" {
		model = "unknown"
	}
	c.recordMiss("exact", model)
	return nil, false, ""
}

func (c *ResponseCache) lookupSemantic(key string, embed func() ([]float32, error), model string) ([]byte, bool) {
	vec, err := embed()
	if err != nil {
		c.recordMiss("semantic", model)
		return nil, false
	}

	cachedKeyBytes, similarity, ok := c.idx.Search(vec, c.similarityThreshold)
	if !ok {
		c.recordMiss("semantic", model)
		return nil, false
	}

	c.mu.RLock()
	semEntry, semOk := c.items[string(cachedKeyBytes)]
	if semOk && !time.Now().After(semEntry.expireAt) {
		data := semEntry.data
		c.mu.RUnlock()
		c.recordSemanticHit(model, similarity)
		return data, true
	}
	c.mu.RUnlock()

	c.recordMiss("semantic", model)
	return nil, false
}

func (c *ResponseCache) recordExactHit(model string) {
	if c.metrics != nil {
		c.metrics.RecordExactCacheHit(model)
	}
}

func (c *ResponseCache) recordSemanticHit(model string, similarity float64) {
	if c.metrics != nil {
		c.metrics.RecordSemanticCacheHit(model, similarity)
	}
}

func (c *ResponseCache) recordMiss(cacheType, model string) {
	if c.metrics != nil {
		if cacheType == "" {
			cacheType = "exact"
		}
		if model == "" {
			model = "unknown"
		}
		c.metrics.RecordCacheMiss(cacheType, model)
	}
}

func (c *ResponseCache) Store(key string, data []byte, embedding []float32) {
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
		existing.embedding = embedding
		c.order.MoveToFront(existing.element)
		return
	}

	if len(c.items) >= c.maxSize {
		c.evictLocked()
	}

	elem := c.order.PushFront(key)
	c.items[key] = &responseCacheEntry{
		data:      data,
		expireAt:  time.Now().Add(c.ttl),
		element:   elem,
		embedding: embedding,
	}

	if c.semanticEnabled && c.idx != nil && len(embedding) > 0 {
		c.idx.Insert(key, embedding)
		if c.metrics != nil {
			c.metrics.SetSemanticCacheEntries(int64(c.idx.Len()))
		}
	}
}

func (c *ResponseCache) evictLocked() {
	now := time.Now()

	for e := c.order.Back(); e != nil; {
		next := e.Prev()
		key := e.Value.(string)
		if entry, ok := c.items[key]; ok && now.After(entry.expireAt) {
			// Remove from semantic index if embedding was stored
			if c.idx != nil && entry.embedding != nil {
				c.idx.Remove(key)
			}
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
		if entry, ok := c.items[key]; ok {
			// Remove from semantic index if embedding was stored
			if c.idx != nil && entry.embedding != nil {
				c.idx.Remove(key)
			}
		}
		c.order.Remove(e)
		delete(c.items, key)
	}
	if c.semanticEnabled && c.idx != nil && c.metrics != nil {
		c.metrics.SetSemanticCacheEntries(int64(c.idx.Len()))
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

func (c *ResponseCache) GetEmbedder() EmbeddingProvider {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.embedder
}
