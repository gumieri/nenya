# Architecture

## Package Dependency DAG

```
config → infra → discovery → stream → pipeline → resilience → providers → adapter → routing → gateway → proxy → mcp
```

Each layer may only import from layers to its left. This prevents circular dependencies and keeps the codebase testable in isolation.

## Package Overview

| Package | Responsibility |
|---------|---------------|
| `cmd/nenya/` | Entry point, server bootstrap with graceful shutdown |
| `internal/config/` | Configuration types, JSON loading, model/provider registries, defaults, validation, engine reference resolution |
| `internal/infra/` | Structured logging, thought signature cache, Prometheus metrics, rate limiter, usage tracker, latency tracker (sorted-buffer median with incremental insertion), response cache |
| `internal/discovery/` | Dynamic model catalog discovery from upstream providers, three-tier merge (config > discovered > static), per-provider response parsers |
| `internal/stream/` | SSE transforming reader, sliding window stream filter |
| `internal/pipeline/` | Client classification, code fence detection, tier-0 regex secret redaction, Shannon entropy redaction, TF-IDF relevance-scored truncation, middle-out truncation (code-boundary-aware for IDEs), text compaction, stale tool call pruning, thought pruning, context window compaction, engine calls with fallback chains |
| `internal/resilience/` | Circuit breaker with Closed/Open/HalfOpen states, exponential backoff |
| `internal/providers/` | Provider capability specs (stream_options, auto_tool_choice, content_arrays), per-provider sanitization, response transformers |
| `internal/adapter/` | Provider Adapter pattern: request mutation, auth injection, response mutation, error classification |
| `internal/routing/` | Dynamic provider resolution, agent fallback chains, latency-aware reordering with jitter (thundering herd prevention), upstream request transformation, API key injection |
| `internal/mcp/` | MCP (Model Context Protocol) client: HTTP+SSE transport, tool discovery, tool call execution, OpenAI schema transformation |
| `internal/gateway/` | NenyaGateway struct, HTTP client configuration, token counting, MCP client initialization, MCP tool index |
| `internal/proxy/` | HTTP handlers, content pipeline orchestration, upstream forwarding with retry, transparent SSE streaming, MCP multi-turn tool call loop, buffered SSE response, empty-stream detection with SSE error payload |

## Request Lifecycle

```
Client Request
  │
  ├─ POST /v1/chat/completions
  │   ├─ Auth check (Bearer token)
  │   ├─ Parse JSON body, extract model
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
  │   │   ├─ Tier-0 regex secret redaction + Shannon entropy redaction (applied unconditionally)
  │   │   ├─ Text compaction (skipped for IDE clients)
  │   │   ├─ Stale tool call pruning (if enabled, skipped for IDE clients)
  │   │   ├─ Thought pruning (if enabled — strip <think.../think> reasoning blocks. reasoning_content field is preserved in shared pipeline and stripped per-target for non-reasoning providers)
  │   │   ├─ Window compaction (if enabled)
  │   │   ├─ 3-tier engine interception (soft/hard limits, code-aware prompt for IDEs)
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

Both `security_filter.engine` and `window.engine` use the `EngineRef` type which supports two JSON forms:

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

All engine calls log `caller` (`security_filter` or `window`), `agent` name (or `inline`), `provider`, `model`, and `attempt`/`total` for observability.

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
| **Closed** | Normal operation. Tracks consecutive failures. Trips to Open after `failure_threshold` failures. |
| **Open** | All requests skipped. After `cooldown_seconds`, transitions to HalfOpen. |
| **HalfOpen** | Allows up to `halfOpenMaxRequests` (3) probe requests. All succeed → Closed. Any fail → Open. |
| **ForceOpen** | Immediately opened (used for HTTP 429 rate limits). Extends cooldown for quota exhaustion patterns. |

The circuit breaker is checked twice per target: once during target list construction (`BuildTargetList`) and again immediately before sending the request (`prepareAndSend`). This prevents sending requests to providers that tripped while queued behind other targets.

### Configuration

| Field | JSON Key | Default | Description |
|-------|----------|---------|-------------|
| `failure_threshold` | `failure_threshold` | `5` | Consecutive failures before circuit trips |
| `success_threshold` | `success_threshold` | `1` | Consecutive successes in HalfOpen to recover |
| `max_retries` | `max_retries` | `0` | Cap on retry attempts per request (0 = unlimited) |
| `cooldown_seconds` | `cooldown_seconds` | `60` | Duration to wait before transitioning Open → HalfOpen |

### Observability

- `/statsz` endpoint exposes `circuit_breakers` map with per-key state
- State transitions are logged: WARN on trip, INFO on recovery/probe
- Prometheus gauge `nenya_cb_state` exposed on `/metrics`

## Provider Adapter Pattern

The `internal/adapter` package implements the Adapter Pattern to manage provider-specific wire format differences. See [`ADAPTERS.md`](ADAPTERS.md) for full details.

## SSE Streaming Pipeline

The streaming pipeline is built from composable `io.Reader` and `io.Writer` wrappers:

| Component | Direction | Purpose |
|-----------|-----------|---------|
| `stallReader` | Read from upstream | Aborts after 120s of no data (stall detection) |
| `SSETransformingReader` | Read from upstream | Parses SSE frames, calls `adapter.MutateResponse()` per chunk, extracts usage, fires `OnContent` callback |
| `StreamFilter` | Read | Kills stream if blocked execution patterns detected |
| `immediateFlushWriter` | Write to client | Wraps `http.ResponseWriter`, calls `Flush()` after every `Write()` |
| `sseTeeWriter` | Write to client + buffer | Captures response bytes for response cache storage |
| `emptyStreamSSE` | Write to client | Emits SSE error payload when upstream returns 200 with empty body (when `empty_stream_as_error` is enabled) |

Buffer pooling via `sync.Pool` (32KB buffers) reduces GC pressure under high concurrency.

## Response Cache

In-memory LRU cache with deterministic SHA-256 fingerprinting. See [`CONFIGURATION.md`](CONFIGURATION.md#response_cache) for configuration.

- **Cache key**: SHA-256 of canonical JSON subset (`model`, `messages`, `temperature`, `top_p`, `max_tokens`, `tools`, `tool_choice`, `response_format`, `stop`, `stream`)
- **Storage**: Completed SSE streams captured via `sseTeeWriter`
- **Replay**: Cache hits replay the stored SSE stream with `X-Nenya-Cache-Status: HIT`
- **Bypass**: `x-nenya-cache-force-refresh` header forces cache miss
- **Eviction**: Background goroutine sweeps expired entries every `evict_every_seconds`

## Empty-Stream Detection

When an upstream provider returns `200 OK` with a zero-byte body, the SSE stream completes without any data events. If `governance.empty_stream_as_error` is enabled (default: `false`), Nenya treats this condition as a failure:

1. **Detection** — After `copyStream` completes, the number of bytes written is checked. Zero bytes with no error signals an empty stream.
2. **SSE Error Payload** — An SSE error chunk is emitted to the client:
   ```json
   data: {"type":"error","error":{"code":"empty_response","message":"empty upstream SSE"}}
   data: [DONE]
   ```
   This structured payload is recognized by OpenCode's `parseStreamError` as a retryable `APIError`, triggering the client-side retry mechanism.
3. **Metrics** — The counter `nenya_empty_stream_total{model,provider}` is incremented, allowing operators to identify problematic providers.
4. **Circuit Breaker** — The failure is recorded via `AgentState.RecordFailure`, contributing to cooldown and circuit breaker state.

When the flag is disabled (default), empty streams are treated as a successful response and the client receives a `200 OK` with no SSE events, preserving backward compatibility.

## Latency Tracker

`infra.LatencyTracker` tracks per-model response times for latency-aware routing. When `governance.auto_reorder_by_latency` is enabled, targets are sorted by historical median latency (fastest first) before routing.

### Implementation

- **Sorted buffer**: Samples are maintained in sorted order via incremental binary-search insertion — O(n) per `Record()` instead of O(n log n) full sort. The median is a direct index lookup (`buf[len/2]`).
- **Sliding window**: At most 100 samples per model/provider key. When the buffer overflows, the oldest sample is dropped via explicit copy to a new slice (prevents the underlying array from leaking memory).
- **Eviction**: Stale entries (no updates for 1 hour) are evicted on each `Record()` call.
- **Jitter**: `SortTargetsByLatency` applies ±5% random jitter to median latencies before comparison to prevent thundering herd — all clients hitting the same fastest provider simultaneously. The jitter function is injectable for deterministic testing.

## Graceful Degradation

Nenya is designed to never break the flow between AI coding clients (OpenCode, Aider) and upstream providers. The following mechanisms ensure resilience:

### Best-Effort Content Pipeline

The entire content pipeline (prefix cache, redaction, compaction, tool call pruning, thought pruning, window, TF-IDF truncation, engine interception) runs as best-effort. Any failure is logged as a warning and the request proceeds with the original payload. No pipeline error results in an HTTP 500 to the client.

### Skip on Engine Failure

When `security_filter.skip_on_engine_failure` is `true` (default):
- **Soft limit** (Tier 2): Engine summarization fails → original payload forwarded unchanged
- **Hard limit** (Tier 3): Engine summarization fails → original payload forwarded unchanged (not truncated). When `tfidf_query_source` is set and TF-IDF reduces the payload below `soft_limit`, the engine call is skipped entirely.

This means users without a local Ollama instance can use Nenya purely as a routing proxy with regex-based secret redaction.

### Exhaustive Fallback

For agents with multiple models in a fallback chain, non-retryable errors (e.g., HTTP 400, 401) from one provider still try the next provider before returning an error to the client. Only when all targets are exhausted does the gateway return an error.

### Client Disconnect Cleanup

When a client disconnects during SSE streaming, the upstream connection is aborted and a 5-second timeout ensures the stream copy goroutine doesn't leak indefinitely.

### Empty-Stream Detection

When an upstream provider returns `200 OK` with a zero-byte body, Nenya can optionally treat this as a failure (when `governance.empty_stream_as_error` is enabled). A structured SSE error payload is emitted to the client, which OpenCode recognizes as a retryable error, allowing fallback to the next target in the agent chain. This prevents the client from hanging on an empty response and provides observability via the `nenya_empty_stream_total` metric. When disabled (default), empty streams are treated as successful responses for backward compatibility.

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
- **Scheids/response cache evictor**: Use dedicated shutdown channels
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
