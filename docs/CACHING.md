# Caching

Nenya provides two levels of caching for `/v1/chat/completions` responses:

1. **Exact-match caching** - fast lookup by fingerprint of the request payload (auth token + effective content-policy scope + canonical fields)
2. **Semantic caching** (opt-in) - embedding-based similarity search for near-duplicate prompts, partitioned per API key, per model, and per policy scope

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

When a Bearer token is present, the fingerprint is `"auth:" + hex(sha256(sha256(token) ‖ sha256(policy scope))) + ":" + hex(sha256(canonical fields))` (the two digests are hashed as separate fixed-width values, so no token/scope pair can collide by delimiter placement); without one, `"p:" + hex(sha256(canonical fields))` (the unscoped form carries no identity and exists for tests/tooling only — every request path is authenticated, and a policy scope supplied without a token yields no cache key at all). The `auth:` segment partitions the semantic index, and identical requests (same model, messages, parameters, auth token, and policy scope) share entries.

**Policy scope (NENYA-154):** the namespace segment hashes the caller's token together with the request's *effective content-policy identity*:

- the resolved **agent** (for agent routes; direct-model routes carry none), including the agent's system prompt identity (`system_prompt`, `system_prompt_file`, `force_system_prompt`), `cache_aware`, and `cache_salt`,
- the client **wire format** the cached bytes are rendered in (`openai` / `anthropic`). The value is the format the gateway *detected* for the request (an Anthropic body is detected by its top-level `type` field) and it is the same value that selects the SSE transformer, so a replayed body always matches the format it was rendered in.
- the security-relevant **guardrail configuration**: output redaction (bouncer `redact_output`/window), the redaction pattern set, preset and label, entropy redaction thresholds, `bouncer.fail_open`, the blocked-execution (response stream filter) pattern set, the prefix-cache redaction skip (`skip_redaction_on_system`), the injection detector (global section plus the agent's override), spotlighting, the egress guard (hosts, query cap, action), the canary tripwire, and the advisory judgment sites (`judgments`, `tfidf_rerank`) including the identity of their engine chains.

A cached body is therefore only replayable for the caller, agent, format, and guardrail set that produced it: tightening a policy (or switching an agent from the global defaults to stricter overrides) starts a fresh namespace instead of serving a body cached before the guardrail existed — the class behind GoModel #971. **Operator-visible hit-rate change:** editing any of the knobs above invalidates entries stored under the previous policy (they simply never match again); agents that rely on per-agent policy overrides no longer share entries with agents using the global defaults.

A config reload (SIGHUP) already constructs a fresh response cache, so reloaded policy never sees stale entries; the scope additionally covers in-process changes (per-agent overrides are part of the key from the first request) and the cross-agent / cross-format dimensions above. Restarts are unaffected (in-memory state).

One known limitation inside the agent identity: `system_prompt_file` is keyed by path, not content — the file is re-read per request, so editing it in place takes effect for new requests while cached bodies keep replaying until they expire or the cache is restarted.

**Note on the content pipeline:** The cache key is computed from the request payload as received, before the content pipeline runs (redaction, bouncer summarization, context trimming). Two requests with identical client payloads therefore share a key even when the pipeline would transform them differently; the stored response is the one produced by whichever execution stored it first. The *guardrail* inputs are keyed (see above); the remaining request-shaping inputs that only change the tokens sent upstream — `context.*` trim limits, `window.*`/`compaction.*`, token calibration, routing, and Nenya-managed MCP tool results injected by the multi-turn loop (absent from the client payload and able to change without a reload) — are accepted pre-pipeline-key staleness: they can change the upstream answer for an unchanged key.

**Error responses:** Structured error responses (those containing `error_kind` fields) are never cached. Only successful HTTP 200 responses with valid completion data are eligible for caching. Streaming responses terminated by a guardrail — the egress guard (`exfil_blocked`), the canary tripwire (`exfil_detected`), the execution-policy filter, or an idle stall — and streams sanitized with an injected `gateway_error` terminal are never cached; tests assert an empty cache after each of those terminals as a regression guard.

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

2. **Similarity Search**: Each (API key, policy scope, model) triple has its own in-memory `EmbedIndex` — the index is partitioned per auth+policy namespace and per model (NENYA-189, NENYA-154), so a semantic lookup can never serve one key another key's cached response, a body cached under a different guardrail policy, nor a different model's (different system prompt/tools) under the same key. On lookup:

   - Computes cosine similarity between the query embedding and the embeddings in the caller's namespace
   - Returns the most similar entry if similarity exceeds `response_cache.similarity_threshold`

   **Multi-key deployments:** semantic hits are per API key, per policy scope, AND per model — two keys (or two models of the same key, e.g. distinct agent aliases, or an agent whose guardrail overrides differ from the global defaults) each populate their own namespace and do not share entries (by design; use a single key and model to share).

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

| Field                  | Default                  | Description                                        |
| ---------------------- | ------------------------ | -------------------------------------------------- |
| `enable_semantic`      | `false`                  | Enable semantic caching (opt-in)                   |
| `similarity_threshold` | `0.9`                    | Minimum cosine similarity (0-1) for semantic match |
| `embedding_model`      | `mxbai-embed-large`      | Ollama model for embeddings (1024-dim vectors)     |
| `embedding_url`        | `http://localhost:11434` | Ollama endpoint for embeddings                     |

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
- **Prompt cache key** (`prompt_cache_key`) — for xAI/OpenAI, a deterministic `SHA256(per-conversation session identity)[:16]` (NENYA-158; the legacy `SHA256(agent:model)[:16]` remains the fallback when no session identity is derivable) key is injected to keep the cached block stable across requests. A client-supplied key is preserved. OpenAI breakpoint injection skips if the key is missing.
- **Cache salt** (`cache_salt`) — for vLLM/OpenAI-compatible providers, a tenant salt is injected to isolate cached KV blocks between tenants (per-API-key `cache_salt` overrides agent `cache_salt`). Never injected for Anthropic (server-side workspace isolation). The agent-level salt is part of the response-cache policy scope because it is injected into the payload sent upstream; the per-key salt is constant within that key's namespace and affects only upstream prompt-cache affinity.

### Per-agent `cache_aware` dispatch

Some providers (DeepSeek, Anthropic, OpenAI) cache prompt prefixes automatically without any client marker. For agents routed to them, `agents.<name>.cache_aware: auto` skips the history-wide window compaction and the tail TF-IDF prune while the payload fits the hard limit, so the gateway sends a stable prefix and lets the provider cache do the work instead of local token surgery. `force` applies the same skip to any provider; `off` (default) keeps the full pipeline. Security interceptors and all other stages still run. This keys on the resolved primary provider name, so renamed/custom cache-rich providers should use `force`. See [`CONFIGURATION.md`](CONFIGURATION.md#agents).

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

Prefix cache relies on stable request prefixes (system prompt, tools, pinned messages) and stable upstream provider placement. Using the `sticky` agent routing strategy (see [`ROUTING.md`](ROUTING.md#agent-routing-strategies)) preserves provider-side KV blocks across turns by pinning a session to a single provider/model combination. Without stickiness, round-robin or fallback routing would rotate requests across providers, breaking per-provider prefix caches and incurring repeated `cache_creation` token costs on every turn. On multi-account providers the pin also carries the serving account and later turns prefer it (account pinning, with LRU fallback when the account is unavailable), keeping a session on the same API key — relevant when a provider shards prefix caches or rate-limit budgets per account.

### Refusal and error cache exclusion

Response caching also skips responses whose payload contains `refusal` or `content_filter` (structured `stop_reason`/`finish_reason` fields) so stale refusal responses are never served from cache, and skips streams that terminate on a guardrail (egress guard block, canary tripwire, execution-policy block), on an idle stall, or that carry a gateway-injected `gateway_error` terminal (truncated/empty upstream).

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
`nenya_cache_miss_total` event counters above: the former count _tokens_ for
upstream prompt caching, while the latter count _response events_ for Nenya's
own response cache. Both streaming and non-streaming requests are accounted,
including Anthropic's native `cache_creation_input_tokens` field. The per-TTL
breakdown (`cache_creation.ephemeral_5m_input_tokens` /
`ephemeral_1h_input_tokens`) survives the buffered format conversions too —
the converted usage publishes the same object the streaming transformer does,
omitted when the upstream did not report it (NENYA-311).

### `/statsz` cache hit ratio

`/statsz` also derives `cache_hit_ratio` per model from the same counters:
`hit / (hit + miss + creation)` over all accounted prompt-cache tokens. It is
`0` for a model with no prompt-cache traffic. Creation tokens count as non-hits
because they were written rather than served, so the first request that writes a
cacheable prefix reports `0` for that request — this is a **served-tokens hit
rate**, not the conventional read-hit rate that ignores writes.

The ratio reflects only the normalized fields the upstream path populates, so
its meaning varies by provider:

- **DeepSeek** reports `prompt_cache_hit_tokens` and `prompt_cache_miss_tokens`
  covering the whole prompt, so `hit / (hit + miss)` is the true hit rate.
- **Anthropic** (normalized) sets `prompt_cache_hit_tokens` to the cache-read
  tokens and `prompt_cache_miss_tokens` to the uncached input remainder, with
  `cache_creation_tokens` for new writes; the three sum to the prompt, so the
  ratio is the true hit rate.
- **OpenAI streaming** supplies only `prompt_tokens_details.cached_tokens` as a
  hit, with no miss or creation count, so the ratio reads `1.0` for any cached
  traffic rather than a full hit rate.
- **OpenAI non-streaming** reads the same `prompt_tokens_details.cached_tokens`
  field (fallback in `internal/proxy/usage.go`), so it behaves like the
  streaming path: `1.0` for any cached traffic, `0` only when the upstream
  reports neither hit nor miss. Native `cache_read_input_tokens` is also folded
  in when present.
- **Gemini** native cached-content tokens (`cachedContentTokenCount`) are mapped
  onto `prompt_cache_hit_tokens`, with the uncached remainder
  (`promptTokenCount - cachedContentTokenCount`) as the miss, so the ratio is
  the true hit rate for Gemini-native usage (NENYA-167).

Only the normalized `prompt_cache_hit_tokens`, `prompt_cache_miss_tokens`, and
`cache_creation_tokens` feed the ratio. Since NENYA-167 the normalization folds
the Anthropic-native `cache_read_input_tokens`/`cache_creation_input_tokens`
into these counters and derives the miss as the uncached remainder, so every
shape whose input field *excludes* the cache dimensions (Anthropic-native, and
Gemini-native via `cachedContentTokenCount`) satisfies `hit + miss + creation =
prompt`. A shape whose input field *includes* them and reports no miss (the
OpenAI decomposition, and the Anthropic-semantics-under-OpenAI-keys case) has no
derived miss, so its ratio keeps the caveat above.
The field reports `0` both for "no prompt-cache traffic" and for a genuine 0% hit
rate — the two are not distinguished. Above roughly 2^53 accumulated tokens the
value becomes approximate (numerator and denominator round independently), so do
not use it for billing.

Excerpt of the `/statsz` body (model name illustrative):

```json
{
  "models": {
    "example-model": {
      "cache_hit_tokens": 3000,
      "cache_miss_tokens": 9000,
      "cache_creation_tokens": 0,
      "cache_hit_ratio": 0.25
    }
  }
}
```

Use this field as the baseline when changing anything that can disturb
prompt-prefix stability (context trimming, summarization, TF-IDF pruning,
message rewrites).

## Response Headers

On a cache hit (exact or semantic), Nenya adds:

```
X-Nenya-Cache-Status: HIT         # Exact match
X-Nenya-Cache-Status: SEMI-HIT    # Semantic match
```

The replayed body is always framed as `text/event-stream`: only streaming responses are ever cached (the upstream content type, not the client's `stream` flag, selects the streaming path).

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

### Automatic breakpoint anchoring (opt-in, NENYA-185)

A client that sends no `cache_control` marks gets no Anthropic prefix cache unless the gateway
anchors one. `agents.<name>.auto_cache_breakpoints: true` (default off) does exactly that, reusing
the explicit-mode breakpoint machinery (including the four-marker cap and the canary exclusion):
the gateway marks the end of the leading system run and the last stable history message. It stands
down — with telemetry, never silently — whenever the client expressed its own placement (a bounded
deep scan of system blocks, message content blocks, tool schemas, and the `extra_body`/
`extra_content` escape hatches), when the request is one-shot (the gateway's own canary trailer is
not history), when the prefix cache is disabled or in `automatic` mode (which emits a single
top-level marker and forces block markers off), and when the history was rewritten by window
compaction or by the per-dispatch input trim (an anchored boundary must be a client-stable prefix,
never a gateway-produced head). Outcomes are counted in
`nenya_auto_cache_breakpoints_total{outcome}` (`injected`, `already_anchored`,
`skipped_client_marks`, `skipped_one_shot`, `skipped_windowed`, `skipped_trimmed`,
`skipped_disabled`, `skipped_automatic_mode`). Outcomes are counted per DISPATCH (one
per target per attempt, and once per MCP loop iteration), not per client request, so a failover can
record several outcomes for one request.
