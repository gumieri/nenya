# Plan: Cache Improvements for Agentic / Guided RAG

**Status:** Draft  
**Goal:** Add semantic (embedding-based) caching on top of the existing exact-match response cache, configure similarity thresholds, and export cache-hit metrics per model — reducing token consumption and latency for iterative agentic loops and RAG queries.

---

## A. Semantic (embedding-based) caching

### Motivation
Today's `ResponseCache` (`internal/infra/response_cache.go`) fingerprints payloads by hashing a canonical subset of fields (`model`, `messages`, `max_tokens`, etc.). This works for **identical** requests, but in agentic / Guided RAG workflows the user prompt changes slightly every turn (different file paths, different code snippets). A semantic cache can reuse responses for **similar** prompts, dramatically improving hit rate.

### Steps
1. **Add `embedding` field to the cache entry** in `internal/infra/response_cache.go`:
   ```go
   type responseCacheEntry struct {
       data      []byte
       expireAt  time.Time
       element   *list.Element
       embedding []float32  // 👈 new, nil = exact-match only
   }
   ```

2. **Create `EmbeddingProvider` interface** in a new file `internal/infra/embed.go`:
   ```go
   type EmbeddingProvider interface {
       Embed(ctx context.Context, text string) ([]float32, error)
   }
   ```

3. **Implement Ollama-based provider** in `internal/infra/embed_ollama.go`:
   ```go
   type OllamaEmbedder struct {
       client *http.Client
       model  string
   }
   func (o *OllamaEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
       // POST /api/embed to local Ollama with model = configurable (default "mxbai-embed-large")
       // Return the embedding vector.
   }
   ```

4. **Add cosine-similarity index** — implement a simple in-memory flat index in `internal/infra/embed_index.go`:
   ```go
   type EmbedIndex struct {
       mu       sync.RWMutex
       entries  []embedIndexEntry
       maxSize  int
   }
   type embedIndexEntry struct {
       key       string
       embedding []float32
   }
   func CosineSimilarity(a, b []float32) float64  // returns cosine similarity
   func (idx *EmbedIndex) Search(vec []float32, threshold float64) ([]byte, bool)
   func (idx *EmbedIndex) Insert(key string, vec []float32)
   ```

5. **Modify `FingerprintPayload`** — when `semantic_enabled == true`, also compute the embedding of concatenated user messages and store it. The SHA‑256 fingerprint remains for exact-match fast path.

6. **Update `ResponseCache.Lookup`** — when exact-match misses, try `EmbedIndex.Search`:
   ```go
   func (c *ResponseCache) Lookup(key string, embed func() ([]float32, error)) ([]byte, bool) {
       // 1. exact-match fast path
       if entry, ok := c.items[key]; ok { ... }
       // 2. semantic fallback
       if c.semanticEnabled {
           vec, err := embed()
           // ...
           data, ok := c.idx.Search(vec, c.similarityThreshold)
           // ...
       }
       return nil, false
   }
   ```

7. **Wire into gateway** — in `internal/gateway/gateway.go`, when `cfg.ResponseCache.EnableSemantic` is set, create the `OllamaEmbedder` and pass it to `ResponseCache`.

### Files affected
- `internal/infra/response_cache.go` — embedding field, Lookup signature, semantic fallback
- `internal/infra/embed.go` — new file, `EmbeddingProvider`
- `internal/infra/embed_ollama.go` — new file, `OllamaEmbedder`
- `internal/infra/embed_index.go` — new file, `EmbedIndex`
- `internal/gateway/gateway.go` — wire embedder

---

## B. Cache‑aware prompt rewriting on hit

### Motivation
When a semantic cache hit occurs, the upstream provider has already generated the response for a similar query. We can hint to it (or to the client) that the response was reused, and optionally skip regeneration.

### Steps
1. **Add `X-Nenya-Cache-Status` header** — already partially present (`MISS` on cache miss). Extend to:
   - `HIT` → exact match
   - `SEMI-HIT` → semantic match (with similarity score)
   - `MISS` → no match

2. **Inject a hint in the response** — when it's a semantic hit, prepend a short message to the first SSE data chunk:
   ```
   data: {"choices":[{"delta":{"role":"assistant","content":"[cached — similar query] "}}]}
   ```

3. **Log the hit** — structured log entry with `cache_type`, `similarity`, `model`, `agent`.

### Files affected
- `internal/proxy/stream.go` — header injection, hint injection, logging

---

## C. Configuration knobs

### Motivation
Semantic caching adds latency per request (embedding computation). Users should be able to enable/disable it and tune the similarity threshold.

### Steps
1. **Extend `config.CacheConfig`** `internal/infra/response_cache.go` (or `config/types.go` if a dedicated cache config section exists):
   ```go
   type ResponseCacheConfig struct {
       MaxSize            int           `json:"max_size"`
       MaxEntryBytes      int64         `json:"max_entry_bytes"`
       TTL                time.Duration `json:"ttl"`
       EvictInterval      time.Duration `json:"evict_interval"`
       EnableSemantic     bool          `json:"enable_semantic,omitempty"`
       SimilarityThreshold float64      `json:"similarity_threshold,omitempty"`
       EmbeddingModel     string        `json:"embedding_model,omitempty"`
   }
   ```

2. **Defaults** in `internal/config/defaults.go`:
   - `EnableSemantic`: false (opt‑in)
   - `SimilarityThreshold`: 0.9
   - `EmbeddingModel`: "mxbai-embed-large"

3. **Validation** in `internal/config/validate.go`:
   - If `EnableSemantic == true`, check the embedding model is reachable (or at least that Ollama is configured).

### Files affected
- `config/types.go` or `internal/infra/config.go` — `ResponseCacheConfig` struct
- `config/defaults.go` — defaults
- `config/validate.go` — validation

---

## D. Metrics

### Motivation
Track cache efficiency per model to help operators tune the threshold and decide which models benefit most.

### Steps
1. **Add counters in `internal/infra/metrics.go`**:
   - `RecordExactCacheHit(model string)`
   - `RecordSemanticCacheHit(model string, similarity float64)`
   - `RecordCacheMiss(model string)`

2. **Wire into `ResponseCache`** — call the appropriate metric on each lookup result.

3. **Export** via `/metrics` with labels `{type="exact|semantic|miss", model="..."}`.

### Files affected
- `internal/infra/metrics.go` — new methods
- `internal/infra/response_cache.go` — call metrics

---

## E. Tests

| Test | File | What to cover |
|------|------|---------------|
| `TestExactCacheHit` | `infra/response_cache_test.go` | Exact match returns data, records exact-hit metric |
| `TestSemanticCacheHit` | `infra/response_cache_test.go` | Near‑identical payload with slight paraphrase hits semantic cache |
| `TestSemanticCacheMiss` | `infra/response_cache_test.go` | Completely different payload → miss |
| `TestEmbedIndexSearch` | `infra/embed_index_test.go` | Insert 3 vectors, search with one similar, verify top result |
| `TestEmbedIndexEmpty` | `infra/embed_index_test.go` | Search on empty index → miss |
| `TestCosineSimilarity` | `infra/embed_index_test.go` | Known vectors → expected similarity |
| `TestOllamaEmbedder` | `infra/embed_ollama_test.go` | Mock `http.Client`, verify payload and URL |
| `TestSemanticDisabled` | `infra/response_cache_test.go` | With `EnableSemantic=false`, only exact match used |
| `TestSemanticHintInjection` | `proxy/stream_test.go` | `X-Nenya-Cache-Status: SEMI-HIT` header present on semantic hit |
| `TestConfigDefaults` | `config/config_test.go` | Semantic cache defaults are false, 0.9, mxbai‑embed‑large |

---

## F. Documentation

1. **`docs/CACHING.md`** — new file documenting:
   - Exact‑match vs semantic caching
   - Configuration reference (`response_cache.*`)
   - Embedding model selection and latency trade‑offs
   - How to monitor via `/metrics`
2. **`docs/CONFIG.md`** — add the new `response_cache` section.
3. **`docs/ENDPOINTS.md`** — note the extended `X-Nenya-Cache-Status` header values.
4. **`CHANGELOG.md`** — entry describing semantic caching, new metrics, and config changes.