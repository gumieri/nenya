package infra

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"time"
)

var cacheableFields = []string{
	"model", "messages", "temperature", "top_p",
	"max_tokens", "tools", "tool_choice", "response_format", "stop",
	"stream",
}

// FingerprintPayload returns the cache fingerprint for payload without auth
// scoping (tests and tooling): "p:" + hex(sha256(canonical payload)).
func FingerprintPayload(payload map[string]interface{}) string {
	return FingerprintPayloadScoped(payload, "", "")
}

// FingerprintPayloadWithAuth returns the fingerprint for an authenticated
// caller with no additional scope: FingerprintPayloadScoped(payload,
// authToken, "").
//
// Deprecated: test/tooling helper. Production callers must scope with
// FingerprintPayloadScoped so the key covers the effective content policy.
func FingerprintPayloadWithAuth(payload map[string]interface{}, authToken string) string {
	return FingerprintPayloadScoped(payload, authToken, "")
}

// FingerprintPayloadScoped returns the cache fingerprint over the payload's
// cacheable fields, scoped by the caller's auth identity and by the request's
// effective content-policy identity (NENYA-154).
//
// The scoped form is
// "auth:" + hex(sha256(sha256(authToken) ‖ sha256(policyScope))) + ":" + hex(sha256(canonical payload)).
// Folding the policy scope into the namespace segment means both the exact key
// AND the semantic-index namespace change when the policy changes, so a
// response cached under a different guardrail set can never be replayed
// (exact hit) or reached as a near-duplicate (semantic hit). The two digests
// are concatenated as fixed-width values so no token/policy pair can collide
// by delimiter placement.
//
// Without a token the fingerprint is "p:" + hex(sha256(canonical payload)) and
// a non-empty policyScope is rejected with "" (no cacheable key): an unscoped
// fingerprint cannot be partitioned by policy, so failing closed is the only
// safe answer. The unscoped form exists for tests and tooling only — every
// request path is authenticated.
func FingerprintPayloadScoped(payload map[string]interface{}, authToken, policyScope string) string {
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
	payloadHash := sha256.Sum256(data)
	if authToken == "" {
		if policyScope != "" {
			// Scoped without a caller identity cannot be partitioned.
			return ""
		}
		return "p:" + hex.EncodeToString(payloadHash[:])
	}
	authHash := sha256.Sum256([]byte(authToken))
	scopeHash := sha256.Sum256([]byte(policyScope))
	var namespaceInput [sha256.Size * 2]byte
	copy(namespaceInput[:sha256.Size], authHash[:])
	copy(namespaceInput[sha256.Size:], scopeHash[:])
	namespace := sha256.Sum256(namespaceInput[:])
	return "auth:" + hex.EncodeToString(namespace[:]) + ":" + hex.EncodeToString(payloadHash[:])
}

type responseCacheEntry struct {
	data      []byte
	expireAt  time.Time
	element   *list.Element
	embedding []float32
	// model is the semantic namespace's model segment, remembered so
	// eviction can find the right namespaced index without re-deriving it
	// from the payload (NENYA-189).
	model string
	// stream records the response shape the entry was stored from. Only SSE
	// streams are ever stored, so replay always frames SSE; the semantic path
	// still verifies the flag because its namespace excludes the payload's
	// stream field (the exact key includes it — see Lookup).
	stream bool
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
	// semIdx partitions the semantic similarity index by caller identity and
	// model (NENYA-189): an auth-scoped fingerprint ("auth:<namespacesegment>:<payloadhash>")
	// namespaces on the namespace segment plus the client-requested model, so a
	// semantic hit can never serve one API key another key's cached response
	// (or a different model's). The namespace segment also folds in the
	// effective content-policy identity (NENYA-154) — see
	// FingerprintPayloadScoped — so a policy change yields a fresh namespace
	// instead of reaching entries cached under the previous guardrails.
	// Unscoped no-auth fingerprints carry the model segment only.
	semIdx map[string]*EmbedIndex
	// semanticEntries tracks the total indexed vectors incrementally so the
	// metrics gauge does not need an O(namespaces) scan (guarded by mu).
	semanticEntries int
	evictionWg      sync.WaitGroup
	evictionMu      sync.Mutex
	evictionStarted bool
}

// semanticIndexMaxEntries is the per-namespace capacity of the semantic
// similarity index.
const semanticIndexMaxEntries = 256

// NewResponseCache returns an LRU+TTL response cache. When semanticEnabled
// is set with an embedder, near-identical prompts replay cached responses
// through per-auth-namespace similarity indexes (NENYA-189): a semantic hit
// never crosses API keys. Zero/negative sizes and durations fall back to
// defaults (512 entries, 1 MiB per entry, 1h TTL, 5m evict interval,
// 0.9 similarity).
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
		semIdx:              make(map[string]*EmbedIndex),
	}
	c.startEvicter(evictInterval)
	return c
}

// semanticNamespace partitions the semantic index by caller identity AND
// model (NENYA-189): auth-scoped fingerprints ("auth:<namespacesegment>:<payloadhash>")
// namespace on the namespace segment, plus the model so a semantic hit can
// never serve a response cached for a different model (different system
// prompt / tools under the same key). The namespace segment is produced by
// FingerprintPayloadScoped and already folds in the effective content-policy
// identity (NENYA-154), so a policy change also yields a fresh namespace.
// Legacy unscoped fingerprints ("p:<hash>", tests and tooling) carry the model
// too, so no-auth deployments keep model isolation. Unknown shapes namespace
// on the model alone — the prefix marker keeps a future fingerprint format
// change from silently conflating namespaces.
func semanticNamespace(key, model string) string {
	rest, found := strings.CutPrefix(key, "auth:")
	if !found {
		return "p\x1f" + model
	}
	namespaceSegment, _, ok := strings.Cut(rest, ":")
	if !ok || namespaceSegment == "" {
		return "\x1f" + model
	}
	return namespaceSegment + "\x1f" + model
}

// semanticIndexIfPresent returns the namespace's index without creating it —
// a lookup miss on an unseen namespace must not grow the map (NENYA-189:
// rotated tokens would otherwise leak an index each). Callers must NOT hold
// c.mu.
func (c *ResponseCache) semanticIndexIfPresent(namespace string) *EmbedIndex {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.semIdx[namespace]
}

// semanticIndexForLocked returns the namespace's index, creating it on first
// use. Callers must hold c.mu for writing (the Store insert path).
func (c *ResponseCache) semanticIndexForLocked(namespace string) *EmbedIndex {
	idx, ok := c.semIdx[namespace]
	if !ok {
		idx = NewEmbedIndex(semanticIndexMaxEntries)
		c.semIdx[namespace] = idx
	}
	return idx
}

// dropSemanticKeyLocked removes key from its namespace index (if the index
// exists — never creates) and prunes the namespace when it drains. Callers
// must hold c.mu for writing (eviction and embedding-less refreshes).
func (c *ResponseCache) dropSemanticKeyLocked(key, namespace string) {
	idx, ok := c.semIdx[namespace]
	if !ok {
		return
	}
	if idx.Has(key) {
		idx.Remove(key)
		c.semanticEntries--
	}
	if idx.Len() == 0 {
		delete(c.semIdx, namespace)
	}
}

// semanticEntryCountLocked returns the incrementally tracked indexed-vector
// total. Caller must hold c.mu (used by the eviction sweep's gauge update).
func (c *ResponseCache) semanticEntryCountLocked() int {
	return c.semanticEntries
}

func (c *ResponseCache) handleExactHit(entry *responseCacheEntry, model string) ([]byte, bool, string) {
	data := entry.data
	c.mu.RUnlock()
	if model == "" {
		model = "unknown"
	}
	c.recordExactHit(model)
	if c.logger != nil && c.logger.Enabled(context.Background(), slog.LevelDebug) {
		c.logger.Debug("cache exact hit", "model", model)
	}
	return data, true, "exact"
}

// Lookup returns the cached response for key: an exact match first, then —
// when semantic mode is on — the most similar entry within the key's
// auth+model namespace (never across namespaces, NENYA-189) and with the
// same response shape (stream; enforced on the semantic fallback, whose
// namespace excludes the payload's stream field). embed is only called when
// the semantic fallback runs; a nil embed disables the fallback.
func (c *ResponseCache) Lookup(key, model string, stream bool, embed func() ([]float32, error)) ([]byte, bool, string) {
	if key == "" {
		return nil, false, ""
	}

	c.mu.RLock()
	entry, ok := c.items[key]
	// No response-shape guard on the exact path: the entry's stream flag is the
	// RESPONSE shape (storeStreamCache always records true) and
	// replayCachedResponse frames every hit as SSE accordingly, so there is
	// nothing to compare the request flag against. (The semantic fallback does
	// refuse a cross-shape hit, because its namespace excludes the payload's
	// stream field; the exact key includes it, so the two paths are
	// deliberately asymmetric.)
	if ok && !time.Now().After(entry.expireAt) {
		return c.handleExactHit(entry, model)
	}
	c.mu.RUnlock()

	if c.semanticEnabled && c.embedder != nil {
		if data, ok := c.lookupSemantic(key, model, stream, embed); ok {
			return data, true, "semantic"
		}
	}

	if model == "" {
		model = "unknown"
	}
	c.recordMiss("exact", model)
	if c.logger != nil && c.logger.Enabled(context.Background(), slog.LevelDebug) {
		c.logger.Debug("cache miss", "model", model, "type", "exact")
	}
	return nil, false, ""
}

func (c *ResponseCache) lookupSemantic(key, model string, stream bool, embed func() ([]float32, error)) ([]byte, bool) {
	if embed == nil {
		// No embedder wired for this request: the semantic fallback cannot
		// run (treated as a miss, never a panic).
		c.recordMiss("semantic", model)
		return nil, false
	}
	vec, err := embed()
	if err != nil {
		c.recordMiss("semantic", model)
		if c.logger != nil && c.logger.Enabled(context.Background(), slog.LevelDebug) {
			c.logger.Debug("cache semantic embedding failed", "model", model, "err", err)
		}
		return nil, false
	}

	idx := c.semanticIndexIfPresent(semanticNamespace(key, model))
	if idx == nil {
		c.recordMiss("semantic", model)
		return nil, false
	}
	cachedKeyBytes, similarity, ok := idx.Search(vec, c.similarityThreshold)
	if !ok {
		c.recordMiss("semantic", model)
		if c.logger != nil && c.logger.Enabled(context.Background(), slog.LevelDebug) {
			c.logger.Debug("cache semantic miss", "model", model)
		}
		return nil, false
	}

	c.mu.RLock()
	semEntry, semOk := c.items[string(cachedKeyBytes)]
	// Defense in depth: the namespace already encodes the model, but verify
	// the entry itself matches so any index/namespace drift degrades to a
	// miss instead of a cross-model response (NENYA-189). The response shape
	// must match too: only SSE streams are stored, and serving one to a
	// non-streaming request would write SSE bytes under a JSON content type.
	if semOk && semEntry.model == model && semEntry.stream == stream && !time.Now().After(semEntry.expireAt) {
		data := semEntry.data
		c.mu.RUnlock()
		c.recordSemanticHit(model, similarity)
		if c.logger != nil && c.logger.Enabled(context.Background(), slog.LevelDebug) {
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

// Store inserts or refreshes the cached response under key and indexes the
// embedding in the key's auth+model semantic namespace when one is present.
// stream records the response shape (semantic hits only serve same-shape
// requests).
func (c *ResponseCache) Store(key, model string, stream bool, data []byte, embedding []float32) {
	if key == "" || len(data) == 0 {
		return
	}
	if int64(len(data)) > c.maxBytes {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if existing, ok := c.items[key]; ok {
		prevModel := existing.model
		existing.data = data
		existing.expireAt = time.Now().Add(c.ttl)
		existing.embedding = embedding
		existing.model = model
		existing.stream = stream
		c.order.MoveToFront(existing.element)
		// A re-store under a different model (failover, model mapping) must
		// not leave an orphan vector in the previous namespace: it could
		// never be pruned and a lookup there could serve this entry's
		// current other-model response (NENYA-189).
		if c.semanticEnabled && prevModel != model {
			c.dropSemanticKeyLocked(key, semanticNamespace(key, prevModel))
		}
	} else {
		if len(c.items) >= c.maxSize {
			c.evictLocked()
		}

		elem := c.order.PushFront(key)
		c.items[key] = &responseCacheEntry{
			data:      data,
			expireAt:  time.Now().Add(c.ttl),
			element:   elem,
			embedding: embedding,
			model:     model,
			stream:    stream,
		}
	}

	// NENYA-189: keep the semantic index in sync with the entry — inserting
	// on first/refreshed embeddings, dropping stale vectors on
	// embedding-less refreshes.
	if c.semanticEnabled {
		c.syncSemanticIndexLocked(key, model, embedding)
	}
}

// syncSemanticIndexLocked maintains the namespace index for one Store:
// upsert the key's vector when an embedding is present, otherwise drop any
// stale vector so a dead key cannot shadow live siblings. Caller must hold
// c.mu for writing.
func (c *ResponseCache) syncSemanticIndexLocked(key, model string, embedding []float32) {
	ns := semanticNamespace(key, model)
	if len(embedding) == 0 {
		c.dropSemanticKeyLocked(key, ns)
		if c.metrics != nil {
			c.metrics.SetSemanticCacheEntries(int64(c.semanticEntries))
		}
		return
	}
	idx := c.semanticIndexForLocked(ns)
	// Insert is append-only: drop any previous vector for this key so a
	// refreshed entry never leaves a stale vector in the index.
	if idx.Has(key) {
		idx.Remove(key)
		c.semanticEntries--
	}
	stored, evicted := idx.Insert(key, embedding)
	if stored {
		c.semanticEntries++
		if evicted {
			// Capacity dropped another key's vector: no longer indexed.
			c.semanticEntries--
		}
	}
	if c.metrics != nil {
		c.metrics.SetSemanticCacheEntries(int64(c.semanticEntries))
	}
}

func (c *ResponseCache) evictExpiredLocked(now time.Time) int {
	evictedCount := 0
	for e := c.order.Back(); e != nil; {
		next := e.Prev()
		key := e.Value.(string)
		if entry, ok := c.items[key]; ok && now.After(entry.expireAt) {
			if entry.embedding != nil {
				c.dropSemanticKeyLocked(key, semanticNamespace(key, entry.model))
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
			if entry.embedding != nil {
				c.dropSemanticKeyLocked(key, semanticNamespace(key, entry.model))
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

	if c.semanticEnabled && c.metrics != nil {
		c.metrics.SetSemanticCacheEntries(int64(c.semanticEntryCountLocked()))
	}
	if evictedCount > 0 && c.logger != nil && c.logger.Enabled(context.Background(), slog.LevelDebug) {
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
