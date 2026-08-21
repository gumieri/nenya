# Caching

Nenya provides two levels of caching for `/v1/chat/completions` responses:

1. **Exact-match caching** - fast lookup by SHA-256 fingerprint of request payload
2. **Semantic caching** (opt-in) - embedding-based similarity search for near-duplicate prompts

Both cache types use an in-memory LRU cache with TTL-based eviction.

## Exact-Match Cache

Exact-match caching is always enabled when `response_cache.enabled` is true. The cache key is derived from a SHA-256 hash of a canonical subset of request fields:

- `model`
- `messages`
- `temperature`
- `top_p`
- `max_tokens`
- `tools`
- `tool_choice`
- `response_format`
- `stop`
- `stream`
- Authorization token (Bearer token, if present)

This means identical requests (same model, messages, parameters, and auth token) will hit the cache.

**Note on token-budget trimming:** When the `bouncer` or context window management trims messages via `TrimPayload` (respecting `hard_limit_tokens`), truncated messages produce different SHA-256 hashes. This means cache entries are keyed to the *trimmed* payload — identical full requests may miss the cache if one was trimmed and another was not. Cache key computation occurs *after* all payload transformations (trimming, summarization).

**Error responses:** Structured error responses (those containing `error_kind` fields) are never cached. Only successful HTTP 200 responses with valid completion data are eligible for caching.

## Semantic Caching

Semantic caching provides a second-level fallback when exact-match misses but the request is semantically similar to a previous request. This is useful for:

- Agentic workflows where prompts vary slightly between turns (different file paths, code snippets)
- RAG (Retrieval-Augmented Generation) queries with similar content but different context
- Iterative coding tasks where the core question remains the same

### How It Works

1. **Embedding Generation**: When `response_cache.enable_semantic` is true, on a cache miss the gateway:
   - Extracts user messages from the request payload
   - Concatenates their content into a single text string
   - Sends this text to the embedding provider (Ollama by default)
   - Stores the embedding vector with the cache entry

2. **Similarity Search**: The in-memory `EmbedIndex` maintains a vector index of all cached entries. On lookup:
   - Computes cosine similarity between the query embedding and all cached embeddings
   - Returns the most similar entry if similarity exceeds `response_cache.similarity_threshold`

3. **Hit Detection**: If a semantic match is found:
   - The response from the cached key is returned to the client
   - Header `X-Nenya-Cache-Status: SEMI-HIT` is set
   - Metrics: `nenya_cache_hit_total{type="semantic", model="...", similarity="0.XXX"}`

### Configuration

```json
{
  "response_cache": {
    "enable_semantic": false,
    "similarity_threshold": 0.9,
    "embedding_model": "mxbai-embed-large",
    "embedding_url": "http://localhost:11434"
  }
}
```

| Field | Default | Description |
|-------|----------|-------------|
| `enable_semantic` | `false` | Enable semantic caching (opt-in) |
| `similarity_threshold` | `0.9` | Minimum cosine similarity (0-1) for semantic match |
| `embedding_model` | `mxbai-embed-large` | Ollama model for embeddings (1024-dim vectors) |
| `embedding_url` | `http://localhost:11434` | Ollama endpoint for embeddings |

### Embedding Provider

The default embedding provider is **Ollama**. This requires:

1. Ollama running on the configured URL (default: `http://localhost:11434`)
2. The embedding model available (default: `mxbai-embed-large`)

The Ollama embedder:

- Uses `POST /api/embed` with the prompt text
- Returns a 1024-dimensional vector (float32)
- Has a 10-second timeout per embedding request
- Failures degrade to exact-match only (no semantic fallback)

### Performance Considerations

**Latency Trade-off**: Semantic caching adds latency on cache misses due to embedding generation. Exact-match hits remain fast (no embedding computation).

**Memory Usage**: Each cached entry stores an additional 1024 × 4 = 4096 bytes for the embedding vector. With default `max_entries=512`, this is ~2 MB additional memory.

**Tuning**: Adjust `similarity_threshold` based on your workload:
- Higher threshold (0.95+) → fewer false positives, lower hit rate
- Lower threshold (0.85-) → higher hit rate, risk of irrelevant responses

## Prefix Cache (Prompt Caching)

Nenya also supports **provider-side prompt prefix caching** (`prefix_cache`), which stabilizes the request prefix and injects cache control markers so the upstream provider can reuse previously computed KV blocks. This is independent of the exact/semantic response caches above. See [`CONFIGURATION.md#prefix_cache`](CONFIGURATION.md) for the full configuration reference.

### How it works

- **Prefix stabilization** — system messages are pinned first (`pin_system_first`), tools are sorted deterministically (`stable_tools`), and Tier-0 redaction is skipped on system messages (`skip_redaction_on_system`) so the prefix is byte-identical between requests.
- **Anthropic breakpoints** (`cache_control`) — the gateway injects `cache_control` markers on the system, tools, and messages blocks. Two breakpoint strategies are available:
  - `cache_mode: "explicit"` (default) — injects block-level `cache_control` markers on `CacheSystem`/`CacheTools`/`CacheMessages`.
  - `cache_mode: "automatic"` — emits a single top-level `cache_control` (Anthropic automatic caching). Block-level markers are forced off.
  - Per-breakpoint TTLs default to `cache_control_ttl` (`"ephemeral"` = 5m, `"1h"` supported). TTL order must be non-increasing (longer TTLs before shorter).
- **OpenAI GPT-5.6+ breakpoints** (`prompt_cache_breakpoint` / `prompt_cache_options`) — for models with the `gpt-5.6`/`gpt-5.7` prefix, a breakpoint is injected on the second-to-last message block. `openai_mode: "explicit"` additionally sets `prompt_cache_options`; `openai_mode: "implicit"` (default) only sets the block breakpoint.
- **Prompt cache key** (`prompt_cache_key`) — for xAI/OpenAI, a deterministic `SHA256(agent:model)[:16]` key is injected to keep the cached block stable across requests. A client-supplied key is preserved. OpenAI breakpoint injection skips if the key is missing.
- **Cache salt** (`cache_salt`) — for vLLM/OpenAI-compatible providers, a tenant salt is injected to isolate cached KV blocks between tenants (per-API-key `cache_salt` overrides agent `cache_salt`). Never injected for Anthropic (server-side workspace isolation).

### Token accounting

Usage is parsed and surfaced as deltas via the stream `UsageData`:

```go
type UsageData struct {
    CompletionTokens         int
    PromptTokens             int
    TotalTokens              int
    CacheHitTokens           int
    CacheMissTokens          int
    CacheCreationTokens      int
    ReasoningTokens          int
    CacheReadInputTokens     int
    CacheCreationInputTokens int
}
```

- `prompt_tokens = cache_read + cache_creation + input` for Anthropic.
- Cache-creation breakdown (`ephemeral_5m_input_tokens`, `ephemeral_1h_input_tokens`) and the `iterations` array are parsed into the transformer (capped at 1000 entries).

### Sticky routing and prefix cache warmth

Prefix cache relies on stable request prefixes (system prompt, tools, pinned messages) and stable upstream provider placement. Using the `sticky` agent routing strategy (see [`ROUTING.md`](ROUTING.md#agent-routing-strategies)) preserves provider-side KV blocks across turns by pinning a session to a single provider/model combination. Without stickiness, round-robin or fallback routing would rotate requests across providers, breaking per-provider prefix caches and incurring repeated `cache_creation` token costs on every turn.

### Refusal and error cache exclusion

Response caching also skips responses whose payload contains `refusal` or `content_filter` (structured `stop_reason`/`finish_reason` fields) so stale refusal responses are never served from cache.

## Metrics

Cache metrics are exposed via `/metrics` endpoint with Prometheus format:

```
# Exact hits
nenya_cache_hit_total{type="exact", model="gpt-4"} 123

# Semantic hits with similarity score
nenya_cache_hit_total{type="semantic", model="gpt-4", similarity="0.923"} 45

# Misses by type
nenya_cache_miss_total{type="exact", model="gpt-4"} 789
nenya_cache_miss_total{type="semantic", model="gpt-4"} 23
```

Monitor these metrics to:
- Determine optimal `similarity_threshold` (balance hit rate vs relevance)
- Identify which models benefit most from semantic caching
- Track degradation if embedder fails (semantic misses only)

### Prefix-cache token accounting

In addition to the response-cache event counters above, Nenya exposes upstream
prefix/prompt-cache **token totals** for billing and cost reconciliation.
These counters track tokens served from or written to the provider's own
prompt cache (Anthropic, OpenAI, Z.AI, etc.) and are labeled per
`model`/`agent`/`provider`:

```
# Tokens served from the upstream prefix cache
nenya_cache_read_tokens_total{agent="opencode", model="claude-3-5-sonnet", provider="anthropic"} 5120

# Tokens written to the upstream prefix cache (cacheable-system-prompt/ephemeral)
nenya_cache_creation_tokens_total{agent="opencode", model="claude-3-5-sonnet", provider="anthropic"} 1024

# Tokens not served from cache (uncached input)
nenya_cache_miss_tokens_total{agent="opencode", model="claude-3-5-sonnet", provider="anthropic"} 2048
```

These token counters are distinct from the `nenya_cache_hit_total` /
`nenya_cache_miss_total` event counters above: the former count *tokens* for
upstream prompt caching, while the latter count *response events* for Nenya's
own response cache. Both streaming and non-streaming requests are accounted,
including Anthropic's native `cache_creation_input_tokens` field.

## Response Headers

On a cache hit (exact or semantic), Nenya adds:

```
X-Nenya-Cache-Status: HIT         # Exact match
X-Nenya-Cache-Status: SEMI-HIT    # Semantic match
```

These headers help clients understand cache behavior without changing the SSE protocol.

## Cache Storage and Eviction

Both cache levels share the same in-memory LRU cache with:

- **Max entries**: `response_cache.max_entries` (default: 512)
- **Max entry size**: `response_cache.max_entry_bytes` (default: 1 MB)
- **TTL**: `response_cache.ttl_seconds` (default: 3600s)
- **Eviction**: LRU (oldest evicted when full), plus background evictor runs every `response_cache.evict_every_seconds` (default: 300s)

When an entry is evicted, both the exact and semantic indexing are removed together.

## Disabling Semantic Caching

To use only exact-match caching:

```json
{
  "response_cache": {
    "enabled": true,
    "enable_semantic": false
  }
}
```

This provides fast deduplication without the latency and memory overhead of embeddings.

## Security and Privacy

- Embeddings are generated from **user message content only**
- System prompts, tool definitions, and other metadata are **not** embedded
- Embeddings are stored in-memory only (not persisted)
- No PII is sent to the embedding provider beyond what's already in the request payload

## Troubleshooting

### No Semantic Hits

If semantic caching is enabled but you see only exact hits:

1. Check Ollama is accessible: `curl http://localhost:11434/api/tags`
2. Check embedding model is available: `curl http://localhost:11434/api/tags`
3. Review logs for embedding errors
4. Check `similarity_threshold` is not too high
5. Verify user messages contain searchable content (not empty or single tokens)

### High Latency on Misses

If semantic caching slows down requests:

1. Consider disabling it if hit rate is low
2. Increase `similarity_threshold` to reduce search overhead
3. Upgrade Ollama hardware or use a faster embedding model

### Cache Thrashing

If cache evicts too frequently:

1. Increase `response_cache.max_entries`
2. Increase `response_cache.ttl_seconds`
3. Monitor metrics to identify the right cache size for your workload
