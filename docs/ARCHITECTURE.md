# Architecture

## Package Dependency DAG

```
util, tiktoken -> config -> infra -> discovery -> stream -> pipeline -> resilience -> providers -> adapter -> routing -> local -> gateway -> proxy -> mcp
```

Each layer may only import from layers to its left. This prevents circular dependencies and keeps the codebase testable in isolation. Leaf dependencies (`util`, `tiktoken`) contain shared utilities. The `local` package manages Ollama model lifecycles.

## Package Overview

| Package | Responsibility |
|---------|---------------|
| `cmd/nenya/` | Entry point, server bootstrap with graceful shutdown |
| `internal/util/` | Shared utilities: overflow-safe integer arithmetic, ID generation, string formatting, error helpers, retry primitive (`DoWithRetry`) |
| `internal/tiktoken/` | cl100k_base BPE token counter for prompt token estimation (zero external dependencies) |
| `config/` | Configuration types, JSON loading, model/provider registries, defaults, validation, engine reference resolution |
| `internal/infra/` | Structured logging, thought signature cache, Prometheus metrics, rate limiter, usage tracker, latency tracker (sorted-buffer median with incremental insertion), response cache, structured errors (`ErrorKind`, `ErrorResponse`) |
| `internal/discovery/` | Dynamic model catalog discovery from upstream providers, three-tier merge (config > discovered > static), per-provider response parsers |
| `internal/stream/` | SSE transforming reader, sliding window stream filter |
| `internal/pipeline/` | Client classification, code fence detection, interceptor chain (Redact/Entropy/TF-IDF/Bouncer), tier-0 regex secret redaction, Shannon entropy redaction, TF-IDF relevance-scored truncation, middle-out truncation (code-boundary-aware for IDEs), text compaction, stale tool call pruning, thought pruning, context window compaction, engine calls with fallback chains |
| `internal/resilience/` | Circuit breaker with Closed/Open/HalfOpen states, exponential backoff |
| `internal/providers/` | Provider capability specs (stream_options, auto_tool_choice, content_arrays), per-provider sanitization, response transformers |
| `internal/adapter/` | Provider Adapter pattern: request mutation, auth injection, response mutation, error classification, bidirectional OpenAI↔Anthropic format conversion |
| `internal/routing/` | Dynamic provider resolution, agent fallback chains, latency-aware reordering with jitter (thundering herd prevention), upstream request transformation, API key injection, format detection |
| `internal/local/` | Local Ollama model lifecycle management: GPU load/unload, session tracking with LRU eviction, startup preloading |
| `internal/mcp/` | MCP (Model Context Protocol) client: HTTP+SSE transport, tool discovery, tool call execution, OpenAI schema transformation |
| `internal/gateway/` | NenyaGateway struct, HTTP client configuration, token counting, MCP client initialization, MCP tool index |
| `internal/billing/` | Billing-aware routing: quota tracking, spend limits, account selection |
| `internal/auth/` | Authentication (token validation, RBAC enforcement with agent scoping and endpoint allowlists) |
| `internal/security/` | Secure memory: mlock-protected token storage, read-only sealing, core dump prevention |
| `internal/version/` | Build metadata injection: version, commit, build time |
| `internal/proxy/` | HTTP handlers, content pipeline orchestration, upstream forwarding with retry, transparent SSE streaming, MCP multi-turn tool call loop, buffered SSE response, empty-stream detection with SSE error payload, structured error normalization |

## Request Lifecycle

```
Client Request
  │
  ├─ POST /v1/chat/completions
  │   ├─ Auth check (Bearer token)
  │   ├─ RBAC enforcement (agent scoping, endpoint allowlists, role-based permissions)
  │   ├─ Parse JSON body, extract model, detect source format (OpenAI/Anthropic)
  │   ├─ Classify client (IDE detection via User-Agent)
  │   ├─ Resolve agent or provider
  │   ├─ Response cache lookup (if enabled)
  │   │   └─ HIT → replay cached SSE, done
  │   ├─ MCP auto-search (if agent has mcp.auto_search, queries MCP server)
  │   │   └─ Inject relevant context as system message before last user message
  │   ├─ MCP tool injection (if agent has MCP servers configured)
  │   │   ├─ Inject MCP tools as OpenAI function tools into request
  │   │   └─ Inject system prompt instructing LLM to use MCP tools for memory retrieval
  │   ├─ Content pipeline (best-effort — failures logged, never block request):
  │   │   ├─ Prefix cache optimizations
  │   │   ├─ Interceptor chain execution (priority order):
  │   │   │   ├─ RedactInterceptor (Priority 10): Tier-0 regex secret redaction
  │   │   │   ├─ EntropyInterceptor (Priority 20): Shannon entropy redaction
  │   │   │   ├─ TFIDFInterceptor (Priority 30): TF-IDF relevance-scored truncation
  │   │   │   └─ BouncerInterceptor (Priority 50): 3-tier engine interception (soft/hard limits)
  │   │   ├─ Text compaction (skipped for IDE clients)
  │   │   ├─ Stale tool call pruning (if enabled, skipped for IDE clients)
  │   │   ├─ Thought pruning (if enabled — strip <think.../think> reasoning blocks. reasoning_content field is preserved in shared pipeline and stripped per-target for non-reasoning providers)
  │   │   ├─ Window compaction (if enabled)
  │   │   ├─ Token budget trimming (if enabled, trims oldest messages to fit context window)
  │   │   └─ JSON minification
  │   ├─ MCP routing decision:
  │   │   ├─ Agent has MCP tools → MCP multi-turn loop (see below)
  │   │   └─ No MCP tools → standard forwarding (see below)
  │   └─ Standard forwarding (no MCP):
  │       ├─ Agent fallback loop:
  │       │   ├─ Circuit breaker check (skip if Open/ForceOpen)
  │       │   ├─ Circuit breaker re-check before send (skip if tripped during queue wait)
  │       │   ├─ Rate limiter check
  │       │   ├─ adapter.MutateRequest() (payload transform)
  │       │   ├─ adapter.InjectAuth() (header signing)
  │       │   ├─ HTTP POST to upstream
  │       │   ├─ Error classification (adapter.NormalizeError)
  │       │   │   ├─ ErrorRetryable → exponential backoff, retry
  │       │   │   ├─ ErrorRateLimited → cooldown, retry with delay
  │       │   │   ├─ ErrorQuotaExhausted → long cooldown
  │       │   │   ├─ ErrorContextExceeded → context-limit retry with summarization
  │       │   │   └─ ErrorPermanent → try next target (or return error if no more targets)
  │       │   └─ On success → circuit breaker.RecordSuccess()
  │   └─ MCP multi-turn forwarding:
  │       ├─ Buffer upstream SSE response completely
  │       ├─ Extract tool_calls from response
  │       ├─ Partition into MCP tools vs client tools:
  │       │   ├─ Has MCP tools → execute via MCP client (parallel), append results, re-send
  │       │   ├─ Has client tools only → replay to client
  │       │   └─ Mixed → resolve MCP, re-send (LLM may re-request client tools)
  │       └─ Loop until: content response or max iterations reached
  │           ├─ Content response → replay to client
  │           └─ Max iterations → replay last response
  ├─ SSE stream pipeline:
  │       ├─ stallReader (120s idle timeout)
  │       ├─ SSETransformingReader (adapter.MutateResponse per chunk)
  │       ├─ Bidirectional format conversion (OpenAI ↔ Anthropic for Anthropic clients)
  │       ├─ OnContent callback (capture assistant response for memory storage)
  │       ├─ StreamFilter (blocked execution patterns)
  │       ├─ immediateFlushWriter (Flush after every Write)
  │       ├─ sseTeeWriter (capture for response cache)
  │       └─ Empty-stream detection (if enabled, emit SSE error payload on zero-byte response)
  │           └─ Async MCP auto-save (if agent has mcp.auto_save)
  │           └─ POST to MCP server with assistant content (best-effort, tool name configurable)
  ├─ GET /v1/models
  ├─ POST /v1/embeddings
  ├─ POST /v1/responses (transparent passthrough, no content pipeline)
  ├─ /proxy/{provider}/* (arbitrary endpoint passthrough, auth injection, SSE auto-detect)
  ├─ GET /healthz
  └─ GET /statsz
```

## Interceptor Chain

The interceptor chain (`internal/pipeline/interceptor.go:72`) implements the **Chain of Responsibility** pattern. Execution is deterministic by priority (lower numbers run first). All interceptors run best-effort — failures log warnings and fall through to the next interceptor unless `StrictMode` is enabled.

### Interface

```go
type Interceptor interface {
    Name() string
    CanHandle(ctx context.Context, req *InterceptRequest) bool
    Process(ctx context.Context, req *InterceptRequest) (*InterceptResult, error)
    Priority() int
}
```

### Registered Interceptors

| Priority | Interceptor | File | Purpose |
|----------|-------------|------|---------|
| 10 | `RedactInterceptor` | `internal/pipeline/redact_interceptor.go` | Tier-0 regex pattern matching for secrets, tokens, and credentials across the entire payload |
| 20 | `EntropyInterceptor` | `internal/pipeline/entropy_interceptor.go` | Shannon entropy redaction — identifies and redacts high-entropy strings (potential secrets) |
| 30 | `TFIDFInterceptor` | `internal/pipeline/tfidf_interceptor.go` | TF-IDF relevance scoring — prunes content blocks by relevance to user query when `governance.tfidf_query_source` is set |
| 50 | `BouncerInterceptor` | `internal/proxy/bouncer_interceptor.go` | 3-tier engine interception: Tier 1 (soft limit — engine summarization), Tier 2 (hard limit — TF-IDF fallback), Tier 3 (hard limit — engine call with code-aware prompt for IDEs) |

### Execution

`InterceptorChain.Execute()` (`internal/pipeline/interceptor.go:111`) walks interceptors in priority order. Each successful interceptor mutates the request's `Payload` map in-place. On failure, behavior depends on `StrictMode`: fallback to next interceptor (default) or return error (strict). Context cancellation is checked at each interceptor boundary.

### InterceptRequest / InterceptResult

`InterceptRequest` (`internal/pipeline/interceptor.go:33`) carries the full payload, parsed messages slice, client profile, soft/hard limits, and current token count. `InterceptResult` (`internal/pipeline/interceptor.go:54`) returns the modified payload, a truncated flag, new token count, reason string, and skip flag.

### Metrics

Each interceptor reports duration, error count, and applied count via Prometheus metrics (`nenya_interceptor_duration_seconds`, `nenya_interceptor_errors_total`, `nenya_interceptor_applied_total`).

## Context-Limit Retry

When an upstream provider returns an HTTP 400 with a `context_length` or `max_tokens` error, Nenya can automatically retry with a summarized payload (`internal/proxy/retry.go:318`).

### Trigger

The error classification in `handleContextLimitError` (`internal/proxy/retry.go:318`) checks:
1. `util.IsContextLengthError(statusCode, body)` — inspects status code and body for context limit patterns
2. `governance.auto_retry_on_context_limit` — must be enabled (default: true in config)

### Flow

1. **Detection**: Upstream returns 400 with context-limit error body
2. **Summarization**: `attemptContextLimitSummarization` parses the original payload, extracts messages, and calls `summarizeMessages` which invokes `CallEngineChain` with the configured engine targets
3. **Single attempt**: Only one summarization is attempted per request to prevent loops (`summarized` flag on `retryLoop`)
4. **Engine fallback**: Uses the full engine fallback chain (local Ollama → remote provider), same as the Bouncer
5. **Retry**: If summarization succeeds, the summarized payload is re-sent through the standard `retryLoop`. If it fails, the original error is returned to the client

### Configuration

Controlled by `governance.auto_retry_on_context_limit` (boolean, default `false`). Requires at least one configured engine target in `bouncer.engine`.

### Metrics

- `nenya_context_limit_errors_total{agent, provider, model}` — incremented on each context-limit error
- `nenya_summarization_retries_total{agent, provider, model}` — incremented on successful summarization and retry

## Local Engine Lifecycle

The `internal/local/` package manages the lifecycle of local Ollama models, handling load/unload from GPU memory, session tracking, and LRU eviction.

### Architecture

```
EngineManager (internal/local/manager.go:13)
  ├─ SessionManager (internal/local/session.go)
  │   ├─ model-loading/unloading via Ollama API
  │   └─ session state per model (loaded, loading, failed)
  └─ LRU eviction when MaxSessions exceeded
```

### Key Design Decisions

**Standard routing path**: Local models are NOT routed through a separate code path. They flow through the existing `retryLoop` → `prepareAndSend` → `streamResponse` pipeline as standard `UpstreamTarget`s. The `SessionManager` only handles load/unload lifecycle; chat completions are proxied by the existing Ollama provider adapter and `OllamaTransformer`.

**Startup preloading**: `EngineManager.Startup()` (`internal/local/manager.go:37`) loads all models in `config.local_engine.startup_models` sequentially at gateway startup. Failures are logged as warnings — the gateway continues without the local model.

**LRU eviction**: When `MaxSessions` is reached and a new model needs loading, the oldest session is evicted via `evictLRU` (`internal/local/manager.go:106`). Eviction creates a 30s context timeout for unloading.

**Thread safety**: Two mutexes with strict ordering: `EngineManager.mu` is acquired BEFORE `SessionManager.mu` to prevent deadlocks. All exports are goroutine-safe.

**Lock ordering**: `em.mu` (EngineManager) → `sm.mu` (SessionManager). Must be maintained in all methods.

### Configuration

```json
{
  "local_engine": {
    "base_url": "http://localhost:11434",
    "max_sessions": 3,
    "timeout_seconds": 120,
    "startup_models": ["qwen2.5-coder:7b"]
  }
}
```

## Structured Error Handling

Nenya uses a typed error system for client-facing diagnostics and internal retry decisions (`internal/infra/errors.go`, `internal/proxy/errors.go`, `internal/proxy/error_normalizer.go`).

### ErrorKind

The `ErrorKind` type (`internal/infra/errors.go:4`) categorizes errors into semantic classes:

| Kind | HTTP Mapping | Description |
|------|-------------|-------------|
| `context_exceeded` | 400 | Upstream context-length exceeded |
| `rate_limited` | 429 | Rate limit hit |
| `auth_failed` | 401/403 | Authentication failure |
| `model_not_found` | 404 | Model unavailable |
| `provider_timeout` | 504 | Upstream timeout |
| `provider_error` | 502 | Generic upstream failure |
| `network_error` | 502 | Transport-level failure |
| `payload_too_large` | 413 | Request exceeds size limits |
| `invalid_request` | 400 | Malformed or invalid request |
| `bouncer_error` | 502 | Engine interception failure |
| `internal_error` | 500 | Gateway internal error |

### GatewayError

The structured `GatewayError` (`internal/proxy/error_normalizer.go:37`) wraps provider errors with:

- **Type**: `provider_error`, `rate_limit_error`, `invalid_request_error`, `authentication_error`, `not_found_error`, `gateway_error`, `bouncer_error`
- **OpenAI-compatible envelope**: `{"error": {"type": ..., "message": ..., "param": ..., "code": ...}}`
- **Provider context**: `Provider` field for multi-provider diagnostics
- **Error classification**: `Retryable()` and `ShouldFailover()` methods on `ErrorKind` for automated decisions

### Normalization

`ParseProviderError` (`internal/proxy/error_normalizer.go:189`) converts raw HTTP responses from upstream providers into `GatewayError` instances. It handles:
- Body parsing with 64KB limit (`maxErrorBodySize`)
- OpenRouter wrapper error unwrapping (metadata.raw extraction)
- Provider-specific error details (param, code fields)
- Body redaction for logging (secrets stripped before writing to logs)

### SSE Error Payloads

For streaming requests, `writeGatewayStreamError` (`internal/proxy/error_normalizer.go:369`) emits an OpenAI-compatible error as an SSE event followed by `[DONE]`, ensuring clients receive a properly terminated stream on failure.

## MCP Multi-Turn Tool Call Flow

When an agent has MCP servers configured, the LLM may respond with `tool_calls` targeting MCP tools. Nenya intercepts these locally:

```
Request with MCP tools injected
  │
  ├─ Buffer entire SSE response (no streaming to client yet)
  ├─ Parse response for tool_calls
  ├─ Reconstruct assistant message (with content or tool_calls) from buffered SSE
  ├─ No tool_calls → replay buffered response to client, done
  ├─ Only client tool_calls → replay to client, done
  └─ Has MCP tool_calls:
      ├─ Execute MCP tool calls in parallel
      ├─ Append assistant message + tool results to messages
      └─ Re-send to upstream (loop up to max_iterations)
```

**Buffered mode**: When MCP tools are present, responses are buffered in memory rather than streamed to the client. This allows inspection for tool calls before the client sees anything. The final response (with no tool calls) is replayed as a complete SSE stream. Buffer size is bounded by `MaxBodyBytes`.

**Tool name namespacing**: MCP tools are injected with the pattern `server__tool` (e.g., `mempalace__mempalace_search`). Non-MCP tool calls (from the client like `file_edit`) pass through unmodified.

**Mixed tool calls**: If the LLM calls both MCP and client tools in the same response, MCP tools are resolved first and the entire response is re-sent. The LLM may re-request client-only tools in the next iteration.

## Engine Reference System

Both `bouncer.engine` and `window.engine` use the `EngineRef` type which supports two JSON forms:

| Form | Syntax | Resolution |
|------|--------|------------|
| Agent reference | `"engine": "agent-name"` | Looks up `agents["agent-name"]`, builds one `EngineTarget` per model in the agent's model list |
| Inline object | `"engine": {"provider": "...", "model": "..."}` | Single `EngineTarget` using the specified provider/model |

Resolution happens once at config load time (`resolveEngineRefs` in `internal/config/engine_resolve.go`). The resolved `[]EngineTarget` slices are cached on the `EngineRef` struct — zero per-request overhead.

### `CallEngineChain`

`internal/pipeline/engine.go` implements `CallEngineChain` which iterates the target list:

1. For each target, selects the appropriate HTTP client (regular vs Ollama) based on provider `ApiFormat`
2. Applies per-target timeout from `EngineConfig.TimeoutSeconds` (falls back to provider's `timeout_seconds`, then hard default `60`)
3. On failure, logs a structured warning and tries the next target
4. Returns on first success; on all failures, returns the last error

All engine calls log `caller` (`bouncer` or `window`), `agent` name (or `inline`), `provider`, `model`, and `attempt`/`total` for observability.

## Model Discovery

Nenya dynamically fetches model catalogs from upstream providers at startup and on SIGHUP reload, replacing the static ModelRegistry with a three-tier priority system.

### Three-Tier Model Resolution

When resolving a model (for routing, `/v1/models` catalog, or `max_tokens` injection), Nenya uses this priority order:

1. **Config overrides** — Agent model entries with explicit `provider`, `max_context`, or `max_output` fields
2. **Discovered models** — Models fetched from provider `/v1/models` endpoints at startup/reload
3. **Static registry** — Built-in ModelRegistry fallback for known models

This allows:
- Custom local models (Ollama) to be discovered automatically
- Provider-specific overrides without code changes
- Graceful fallback when discovery fails (static registry still works)

### Discovery Process

At startup and on `systemctl reload nenya`:

1. **Concurrent fetch** — For each configured provider with an API key, fetch `/v1/models` in parallel (10s timeout per provider)
2. **Provider-specific parsing** — Each provider has a dedicated parser for its response format:
   - OpenAI-compatible: `{"data": [{"id": "..."}]}`
   - Anthropic: `{"data": [{"id": "...", "display_name": "..."}]}`
   - Gemini: `{"models": [{"name": "models/...", "inputTokenLimit": ..., "outputTokenLimit": ...}]}`
   - Ollama: `{"models": [{"name": "..."}]}`
3. **Merge with static registry** — Discovered models are merged with built-in ModelRegistry (config overrides take precedence)
4. **Update catalog** — The merged catalog is stored in `Gateway.ModelCatalog` and used for all subsequent model resolution

### Security Hardening

The discovery package enforces strict security boundaries:

- **Response body limits** — 10 MB max per provider response (DoS protection)
- **JSON decode limits** — 10 MB max with `DisallowUnknownFields` (malformed JSON rejection)
- **Content-type validation** — Only `application/json` responses are parsed
- **Model ID sanitization** — Max 256 chars, printable characters only (XSS prevention)
- **Per-provider timeouts** — 10s context timeout per fetch (no hanging)
- **Panic recovery** — Goroutines have defer/recover to prevent crashes
- **Auth header injection** — Gemini uses `x-goog-api-key` header (not query params)
- **Shared HTTP client** — Reused with proper TLS timeouts (no resource leaks)

### `/v1/models` Endpoint

The `/v1/models` catalog endpoint now returns actual discovered models instead of wildcards:

```json
{
  "object": "list",
  "data": [
    {
      "id": "gemini-2.5-flash",
      "object": "model",
      "owned_by": "google",
      "context_window": 128000,
      "max_tokens": 8192
    },
    {
      "id": "deepseek-chat",
      "object": "model",
      "owned_by": "deepseek",
      "context_window": 128000,
      "max_tokens": 4096
    }
  ]
}
```

Models are filtered to only show those with valid API keys configured. Agent names are also included as models (for agent-based routing).

### Thread Safety

- **ModelCatalog** — Internal `sync.RWMutex` protects all reads/writes
- **Gateway hot-swap** — `Proxy` uses `atomic.Pointer[gateway.NenyaGateway]` for zero-downtime reloads
- **Concurrent discovery** — Provider fetches run in parallel with proper goroutine lifecycle management and concurrency-limited health checks (configurable, default 5 concurrent)

### Graceful Degradation

If discovery fails for any provider:
- The provider is skipped with a warning log
- Static registry models for that provider still work
- Other providers' discovered models are still used
- `/v1/models` endpoint shows only successfully discovered models

This ensures Nenya never breaks due to discovery failures — it's a best-effort enhancement on top of the static registry.

## Circuit Breaker

Each agent+provider+model combination is tracked independently by a circuit breaker.

### States

| State | Behavior |
|-------|----------|
| **Closed** | Normal operation. Tracks consecutive failures with semantic error classification. Trips to Open after `failure_threshold` failures (or immediately for auth errors). |
| **Open** | All requests skipped. After `cooldown_seconds`, transitions to HalfOpen. |
| **HalfOpen** | Allows up to `halfOpenMaxRequests` (3) probe requests. All succeed → Closed. Any fail → Open. |
| **ForceOpen** | Immediately opened (used for HTTP 429 rate limits). Extends cooldown for quota exhaustion patterns. |

The circuit breaker is checked twice per target: once during target list construction (`BuildTargetList`) and again immediately before sending the request (`prepareAndSend`). This prevents sending requests to providers that tripped while queued behind other targets.

### Semantic Error Classification

Failures are classified into 6 error classes for appropriate circuit breaker and backoff decisions:

| Error Class | HTTP Status | Backoff | Lock Behavior |
|-------------|-------------|---------|---------------|
| `auth` | 401, 403 | 5 min cooldown | Immediate trip to Open |
| `rate_limit` | 429 | Exponential (500ms base, ±10% jitter) | Model-level lock with cooldown |
| `quota` | 400 (quota messages) | Exponential (60s base) | Long cooldown, tracked separately |
| `capacity` | 503 (capacity/overload) | Exponential (30s base) | Model-level lock |
| `server` | 500, 502, 504 | 5s cooldown | Transient, short backoff |
| `unknown` | Other 4xx/5xx | No backoff | No lock |

`classifyHTTPError(status, body, backoffLevel)` parses provider-specific error messages for quota/capacity keywords and returns `CooldownDecision` with the appropriate class.

### Exponential Backoff

The `BackoffTracker` manages per-model backoff levels with capped exponential growth:

- **Backoff Levels**: 0–15 (capped at max)
- **Base Delays**: rate_limit=500ms, quota=60s, capacity=30s
- **Jitter**: ±10% per level to prevent thundering herd
- **Increment**: `RecordFailureWithStatus` calls `cb.Increment(model)` which increments the backoff level
- **Reset**: `RecordSuccessWithModel` calls `cb.Reset(model)` which clears the backoff state
- **Callback**: `SetBackoffIncrementCallback(onIncrement func(key string, level int))` for metrics emission (`nenya_backoff_increments_total`)

### Model-Level Locks

Individual model+provider combinations can be locked (skipped) without tripping the circuit breaker:

| Method | Purpose |
|--------|---------|
| `IsModelLocked(model)` | Check if model is in cooldown |
| `GetModelLockUntil(model)` | Get remaining cooldown duration |
| `UnlockModel(model)` | Manually clear a model lock |
| `GetBackoffLevel(model)` | Query current backoff level for a model |

Model locks are checked during `BuildTargetList` — locked models are skipped before dispatch. Locks are automatically cleared when `RecordSuccessWithModel` is called.

### Observability

- `/statsz` endpoint exposes `circuit_breakers` map with per-key state, plus `model_locks` and `backoff_level` from `SnapshotDetailed`
- State transitions are logged: WARN on trip, INFO on recovery/probe
- Prometheus gauges:
  - `nenya_cb_state{key, state}` — 0/1 for Closed/Open per circuit key
  - `nenya_cb_state_transitions_total{key, from, to}` — state change counter
  - `nenya_backoff_increments_total{key, provider, model, level}` — backoff level distribution

### Configuration

| Field | JSON Key | Default | Description |
|-------|----------|---------|-------------|
| `failure_threshold` | `failure_threshold` | `5` | Consecutive failures before circuit trips |
| `success_threshold` | `success_threshold` | `1` | Consecutive successes in HalfOpen to recover |
| `max_retries` | `max_retries` | `0` | Cap on retry attempts per request (0 = unlimited) |
| `cooldown_seconds` | `cooldown_seconds` | `60` | Duration to wait before transitioning Open → HalfOpen |
| `half_open_max_requests` | `half_open_max_requests` | `3` | Max probe requests in HalfOpen state |

### Per-Provider Multi-Account Handling

For providers with multiple credential accounts (configured via `accounts[]` in the config), circuit breaker tracking is scoped to `provider + model` only — not the individual account. This allows automatic account rotation when a specific account hits a rate limit, while tracking failures across the entire provider. The `UpstreamTarget` includes the `AccountKey` field for credential selection, but circuit breaker keys omit it.

### Backoff and Threshold Enhancements

The circuit breaker integrates with the `BackoffTracker` for exponential backoff on rate-limit and quota errors. `calculateBackoff` (`internal/proxy/retry.go:50`) implements base 500ms exponential doubling with ±750ms jitter, capped at 8 seconds. Per-provider `RetryableStatusCodes` allow custom retry policies beyond the standard defaults (429, 500, 502, 503, 504).

## Provider Adapter Pattern

The `internal/adapter` package implements the Adapter Pattern to manage provider-specific wire format differences. See [`ADAPTERS.md`](ADAPTERS.md) for full details.

## SSE Streaming Pipeline

The streaming pipeline is built from composable `io.Reader` and `io.Writer` wrappers:

| Component | Direction | Purpose |
|-----------|-----------|---------|
| `stallReader` | Read from upstream | Aborts after 120s of no data (stall detection) |
| `SSETransformingReader` | Read from upstream | Parses SSE frames, calls `adapter.MutateResponse()` per chunk, extracts usage, fires `OnContent` callback. Handles bidirectional format conversion (OpenAI ↔ Anthropic) when `SourceFormat` is `"anthropic"` via `AnthropicAdapter.ConvertAnthropicToOpenAI()` |
| `StreamFilter` | Read | Kills stream if blocked execution patterns detected |
| `immediateFlushWriter` | Write to client | Wraps `http.ResponseWriter`, calls `Flush()` after every `Write()` |
| `sseTeeWriter` | Write to client + buffer | Captures response bytes for response cache storage |
| `emptyStreamSSE` | Write to client | Emits SSE error payload when upstream returns 200 with empty body (when `empty_stream_as_error` is enabled) |

Buffer pooling via `sync.Pool` (32KB buffers) reduces GC pressure under high concurrency.

## Graceful Shutdown

The gateway supports graceful shutdown with a configurable context deadline:

### Shutdown Process

1. **Signal Handling**: `SIGTERM` / `SIGINT` triggers `Shutdown(ctx)` on the gateway and proxy
2. **In-Flight Requests**: HTTP server accepts new connections for 5s grace period, then stops accepting
3. **Upstream Connections**: In-flight SSE streams complete normally; clients receive full responses
4. **MCP Clients**: `Shutdown` calls `Close()` on all MCP client connections with a 10s timeout
5. **Response Cache**: Background evictor goroutine stops
6. **Metrics**: Final metrics flush before exit

### Implementation

- **Gateway**: `func (g *NenyaGateway) Shutdown(ctx context.Context) error` — closes HTTP client, MCP clients, clears secure memory
- **Proxy**: `func (p *Proxy) Shutdown(ctx context.Context) error` — shuts down HTTP server with grace period
- **Hot Reload**: `Reload()` uses atomic pointer swap (`atomic.StorePointer`) for zero-downtime config changes without shutdown

## Debug Profiling

When `debug.pprof_enabled` is `true` (default: `false`), Nenya exposes Go's `net/http/pprof` endpoints:

| Endpoint | Description |
|----------|-------------|
| `GET /debug/pprof/` | Profiling index |
| `GET /debug/pprof/goroutine` | Goroutine stack traces |
| `GET /debug/pprof/heap` | Heap allocation profile |
| `GET /debug/pprof/threadcreate` | Thread creation profile |
| `GET /debug/pprof/block` | Goroutine blocking profile |
| `GET /debug/pprof/mutex` | Mutex contention profile |
| `GET /debug/pprof/allocs` | Allocation profile |
| `GET /debug/pprof/profile?seconds=30` | CPU profile (30s capture) |

**Security**: All `/debug/pprof/*` endpoints require `Authorization: Bearer <client_token>` or valid API key (same as `/v1/*`).

**Configuration**:

```json
{
  "server": {
    "log_level": "info"
  },
  "debug": {
    "pprof_enabled": false
  }
}
```

Enabling debug profiling is **not recommended for production** — use only during development or performance troubleshooting.

## Response Cache

In-memory LRU cache with deterministic SHA-256 fingerprinting. See [`CONFIGURATION.md`](CONFIGURATION.md#response_cache) for configuration.

- **Cache key**: SHA-256 of canonical JSON subset (`model`, `messages`, `temperature`, `top_p`, `max_tokens`, `tools`, `tool_choice`, `response_format`, `stop`, `stream`)
- **Storage**: Completed SSE streams captured via `sseTeeWriter`
- **Replay**: Cache hits replay the stored SSE stream with `X-Nenya-Cache-Status: HIT`
- **Bypass**: `x-nenya-cache-force-refresh` header forces cache miss
- **Eviction**: Background goroutine sweeps expired entries every `evict_every_seconds`

## Empty-Stream Detection

When an upstream provider returns `200 OK` with a zero-byte body, the SSE stream completes without any data events. If `governance.empty_stream_as_error` is enabled (default: `true`), Nenya treats this condition as a failure:

1. **Detection** — After `copyStream` completes, the number of bytes written is checked. Zero bytes with no error signals an empty stream.
2. **SSE Error Payload** — An SSE error chunk is emitted to the client:
   ```json
   data: {"type":"error","error":{"code":"empty_response","message":"empty upstream SSE"}}
   data: [DONE]
   ```
   This structured payload is recognized by OpenCode's `parseStreamError` as a retryable `APIError`, triggering the client-side retry mechanism.
3. **Metrics** — The counter `nenya_empty_stream_total{model,provider}` is incremented, allowing operators to identify problematic providers.
4. **Circuit Breaker** — The failure is recorded via `AgentState.RecordFailure`, contributing to cooldown and circuit breaker state.

When the flag is disabled, empty streams are treated as a successful response and the client receives a `200 OK` with no SSE events, preserving backward compatibility.

## Latency Tracker

`infra.LatencyTracker` tracks per-model response times for latency-aware routing. When `context.auto_reorder_by_latency` is enabled, targets are sorted by historical median latency (fastest first) before routing.

### Implementation

- **Sorted buffer**: Samples are maintained in sorted order via incremental binary-search insertion — O(n) per `Record()` instead of O(n log n) full sort. The median is a direct index lookup (`buf[len/2]`).
- **Sliding window**: At most 100 samples per model/provider key. When the buffer overflows, the oldest sample is dropped via explicit copy to a new slice (prevents the underlying array from leaking memory).
- **Eviction**: Stale entries (no updates for 1 hour) are evicted on each `Record()` call.
- **Jitter**: `LatencyTracker.SortByLatency` applies ±5% random jitter to median latencies before comparison to prevent thundering herd — all clients hitting the same fastest provider simultaneously. The jitter function is injectable for deterministic testing.

## Graceful Degradation

Nenya is designed to never break the flow between AI coding clients (OpenCode, Aider) and upstream providers. The following mechanisms ensure resilience:

### Best-Effort Content Pipeline

The entire content pipeline (prefix cache, redaction, compaction, tool call pruning, thought pruning, window, TF-IDF truncation, engine interception) runs as best-effort. Any failure is logged as a warning and the request proceeds with the original payload. No pipeline error results in an HTTP 500 to the client.

### Skip on Engine Failure

When `bouncer.fail_open` is `true` (default):
- **Soft limit** (Tier 2): Engine summarization fails → original payload forwarded unchanged
- **Hard limit** (Tier 3): Engine summarization fails → original payload forwarded unchanged (not truncated). When `tfidf_query_source` is set and TF-IDF reduces the payload below `soft_limit`, the engine call is skipped entirely.

This means users without a local Ollama instance can use Nenya purely as a routing proxy with regex-based secret redaction.

### Exhaustive Fallback

For agents with multiple models in a fallback chain, non-retryable errors (e.g., HTTP 400, 401) from one provider still try the next provider before returning an error to the client. Only when all targets are exhausted does the gateway return an error.

### Client Disconnect Cleanup

When a client disconnects during SSE streaming, the upstream connection is aborted and a 5-second timeout ensures the stream copy goroutine doesn't leak indefinitely.

### Empty-Stream Detection

When an upstream provider returns `200 OK` with a zero-byte body, Nenya can optionally treat this as a failure (when `governance.empty_stream_as_error` is enabled, default `true`). A structured SSE error payload is emitted to the client, which OpenCode recognizes as a retryable error, allowing fallback to the next target in the agent chain. This prevents the client from hanging on an empty response and provides observability via the `nenya_empty_stream_total` metric. When disabled, empty streams are treated as successful responses for backward compatibility.

### MCP Graceful Degradation

MCP integration follows the same best-effort philosophy:

- **Server unreachable**: If an MCP server is unreachable at startup, tools from that server are not injected. A warning is logged. Other MCP servers and the request itself proceed normally.
- **Server goes down mid-session**: The MCP client's HTTP+SSE transport detects the disconnection. The tool index is still available from cache, but new tool calls to that server will fail with an error result. The error is returned as a tool result to the LLM, which can decide to retry or inform the user.
- **Timeout**: Each MCP tool call has a 30s timeout (configurable per-server). Timeouts are returned as error results.
- **Max iterations**: The MCP multi-turn loop has a configurable max iteration count (default: 10). When exhausted, the last buffered response is replayed to the client.

## Context Package Usage

Nenya follows Go context best practices for request-scoped values, cancellation, and timeouts:

### Context Propagation
- **Incoming requests**: `r.Context()` from `http.Request` is the root context for each client request
- **Request lifecycle**: Context is threaded through the entire call stack: `handleChatCompletions` → routing → validation → upstream calls
- **Gateway initialization**: Startup/shutdown contexts are created in `main.go` and passed to `gateway.New()` and `Reload()`
- **Validation**: Config/health check validation functions accept `context.Context` to allow cancellation during startup

### Timeout Enforcement
Each outbound call path applies appropriate timeouts:
- **Chat completions**: Uses `provider.TimeoutSeconds` from config (falls back to transport-level timeouts)
- **Embeddings/Responses**: Uses `provider.TimeoutSeconds` from config
- **Passthrough**: Uses `provider.TimeoutSeconds` from config
- **Auto-search**: 10-second timeout to bound MCP search latency
- **Model discovery**: 10-second timeout per provider fetch (concurrent)
- **MCP multi-turn loop**: 5-minute overall deadline to prevent runaway loops
- **MCP tool calls**: 30-second timeout per tool call
- **Health checks**: 5-second timeout for provider availability checks

### Goroutine Lifecycle
All goroutines respect context cancellation or use detached contexts:
- **Stream copying goroutines**: Use request context via `copyStream(ctx, ...)`
- **Discovery goroutines**: Use parent context with per-provider timeout via `context.WithTimeout`
- **Response cache evictor**: Uses `sync.WaitGroup` for clean shutdown on gateway stop
- **MCP auto-save**: Uses `context.Background()` for fire-and-forget semantics (best-effort, outlives request)
- **Background loops**: Use dedicated cancellation channels (`closeCh`, `stopCh`)

### Loop Cancellation
Long-running loops regularly check for cancellation:
- `pipeSSE` → `select { case <-ctx.Done(): }`
- `copyStream` → `if ctx.Err() != nil`
- MCP multi-turn loop → `select { case <-mcpLoopCtx.Done(): }`
- MCP transport loops → `select { case <-closeCh: }`
- Background workers → `select { case <-stopCh: }`

This ensures prompt cleanup during graceful shutdown, client disconnect, or timeout scenarios.

## IDE Compatibility

Nenya detects IDE clients (Cursor, OpenCode) via `User-Agent` header inspection and adapts the content pipeline to preserve code structure. Unknown clients get standard pipeline behavior with zero regression risk.

### Client Classification (`internal/pipeline/client.go`)

`ClassifyClient(headers http.Header)` returns a `ClientProfile{IsIDE, ClientName}`. Inspects multiple headers (`User-Agent`, `Editor-Version`, `Editor-Plugin-Version`). Detection patterns are extensible via the `clientPatterns` registry.

### IDE-Aware Pipeline Behavior

| Stage | Non-IDE Clients | IDE Clients |
|-------|----------------|-------------|
| **Secret redaction** | Regex on entire text | Same — redacts unconditionally including inside code fences |
| **Text compaction** | Collapse blank lines, trim whitespace | **Skipped entirely** — preserves whitespace and line references |
| **Tool call pruning** | Compact old assistant+tool pairs | **Skipped entirely** — preserves full tool context for IDE agents |
| **Thought pruning** | Strip `<think.../think>` reasoning blocks | Same — reasoning tokens stripped from assistant history. The `reasoning_content` field is preserved in the shared pipeline and stripped per-target for non-reasoning providers. |
| **Truncation** | Character-boundary middle-out | `TruncateMiddleOutCodeAware` — snaps cuts to blank-line boundaries |
| **TF-IDF Truncation** | Same as IDE — when `tfidf_query_source` is set | Same — splits into blocks, scores by relevance, keeps highest-scoring. Pure Go, zero network calls. |
| **Engine summarization** | Generic privacy filter prompt | Code-preserving prompt — only redacts secrets, never restructures code |

### Code Fence Detection (`internal/pipeline/code_detect.go`)

`DetectCodeFences(text)` returns `[]CodeSpan{Start, End, Language}` for markdown fenced code blocks (`` ``` ``). Used by code-boundary-aware truncation and summarization to avoid cutting inside code blocks.

### Structured Content Handling (`internal/gateway/gateway.go`)

`ExtractContentText` handles the full OpenAI content array spec:
- `{type: "text"}` — concatenated as before
- `{type: "image_url"}` — replaced with `[image]` placeholder for token counting
- `{type: "input_json"}` — serialized to JSON for token counting

Tool call fields (`tool_calls`, `tool_call_id`, `function_call`) pass through sanitization unmodified.
