package infra

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
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
	logger              *slog.Logger
	semanticEnabled     bool
	similarityThreshold float64
	embedder            EmbeddingProvider
	idx                 *EmbedIndex
	evictionWg          sync.WaitGroup
	evictionMu          sync.Mutex
	evictionStarted     bool
}

func NewResponseCache(maxSize int, maxBytes int64, ttl, evictInterval time.Duration, metrics *Metrics, logger *slog.Logger, semanticEnabled bool, similarityThreshold float64, embedder EmbeddingProvider) *ResponseCache {
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
		logger:              logger,
		semanticEnabled:     semanticEnabled,
		similarityThreshold: similarityThreshold,
		embedder:            embedder,
		idx:                 idx,
	}
	c.startEvicter(evictInterval)
	return c
}

func (c *ResponseCache) handleExactHit(entry *responseCacheEntry, model string) ([]byte, bool, string) {
	data := entry.data
	c.mu.RUnlock()
	if model == "" {
		model = "unknown"
	}
	c.recordExactHit(model)
	if c.logger != nil && c.logger.Enabled(context.TODO(), slog.LevelDebug) {
		c.logger.Debug("cache exact hit", "model", model)
	}
	return data, true, "exact"
}

func (c *ResponseCache) Lookup(key, model string, embed func() ([]float32, error)) ([]byte, bool, string) {
	if key == "" {
		return nil, false, ""
	}

	c.mu.RLock()
	entry, ok := c.items[key]
	if ok && !time.Now().After(entry.expireAt) {
		return c.handleExactHit(entry, model)
	}
	c.mu.RUnlock()

	if c.semanticEnabled && c.embedder != nil && c.idx != nil {
		if data, ok := c.lookupSemantic(key, embed, model); ok {
			return data, true, "semantic"
		}
	}

	if model == "" {
		model = "unknown"
	}
	c.recordMiss("exact", model)
	if c.logger != nil && c.logger.Enabled(context.TODO(), slog.LevelDebug) {
		c.logger.Debug("cache miss", "model", model, "type", "exact")
	}
	return nil, false, ""
}

func (c *ResponseCache) lookupSemantic(key string, embed func() ([]float32, error), model string) ([]byte, bool) {
	vec, err := embed()
	if err != nil {
		c.recordMiss("semantic", model)
		if c.logger != nil && c.logger.Enabled(context.TODO(), slog.LevelDebug) {
			c.logger.Debug("cache semantic embedding failed", "model", model, "err", err)
		}
		return nil, false
	}

	cachedKeyBytes, similarity, ok := c.idx.Search(vec, c.similarityThreshold)
	if !ok {
		c.recordMiss("semantic", model)
		if c.logger != nil && c.logger.Enabled(context.TODO(), slog.LevelDebug) {
			c.logger.Debug("cache semantic miss", "model", model)
		}
		return nil, false
	}

	c.mu.RLock()
	semEntry, semOk := c.items[string(cachedKeyBytes)]
	if semOk && !time.Now().After(semEntry.expireAt) {
		data := semEntry.data
		c.mu.RUnlock()
		c.recordSemanticHit(model, similarity)
		if c.logger != nil && c.logger.Enabled(context.TODO(), slog.LevelDebug) {
			c.logger.Debug("cache semantic hit", "model", model, "similarity", similarity)
		}
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

func (c *ResponseCache) evictExpiredLocked(now time.Time) int {
	evictedCount := 0
	for e := c.order.Back(); e != nil; {
		next := e.Prev()
		key := e.Value.(string)
		if entry, ok := c.items[key]; ok && now.After(entry.expireAt) {
			if c.idx != nil && entry.embedding != nil {
				c.idx.Remove(key)
			}
			c.order.Remove(e)
			delete(c.items, key)
			evictedCount++
		}
		e = next
	}
	return evictedCount
}

func (c *ResponseCache) evictOverflowLocked() int {
	evictedCount := 0
	for len(c.items) >= c.maxSize {
		e := c.order.Back()
		if e == nil {
			break
		}
		key := e.Value.(string)
		if entry, ok := c.items[key]; ok {
			if c.idx != nil && entry.embedding != nil {
				c.idx.Remove(key)
			}
		}
		c.order.Remove(e)
		delete(c.items, key)
		evictedCount++
	}
	return evictedCount
}

func (c *ResponseCache) evictLocked() {
	now := time.Now()
	evictedCount := c.evictExpiredLocked(now)
	evictedCount += c.evictOverflowLocked()

	if c.semanticEnabled && c.idx != nil && c.metrics != nil {
		c.metrics.SetSemanticCacheEntries(int64(c.idx.Len()))
	}
	if evictedCount > 0 && c.logger != nil && c.logger.Enabled(context.TODO(), slog.LevelDebug) {
		c.logger.Debug("cache eviction", "evicted", evictedCount)
	}
}

func (c *ResponseCache) startEvicter(interval time.Duration) {
	c.evictionMu.Lock()
	defer c.evictionMu.Unlock()
	if c.evictionStarted {
		return
	}
	c.evictionStarted = true

	c.evictionWg.Add(1)
	go func() {
		defer c.evictionWg.Done()
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
	done := make(chan struct{})
	go func() {
		c.evictionWg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		if c.logger != nil {
			c.logger.Warn("response cache eviction goroutine did not stop in time")
		}
	}
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
