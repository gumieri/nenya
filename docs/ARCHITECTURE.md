# Architecture

## Package Dependency DAG

```
config → infra → stream → pipeline → resilience → providers → adapter → routing → gateway → proxy → cmd
```

Each layer may only import from layers to its left. This prevents circular dependencies and keeps the codebase testable in isolation.

## Package Overview

| Package | Responsibility |
|---------|---------------|
| `cmd/nenya/` | Entry point, server bootstrap with graceful shutdown |
| `internal/config/` | Configuration types, JSON loading, model/provider registries, defaults, validation, engine reference resolution |
| `internal/infra/` | Structured logging, thought signature cache, Prometheus metrics, rate limiter, usage tracker, response cache |
| `internal/stream/` | SSE transforming reader, sliding window stream filter |
| `internal/pipeline/` | Tier-0 regex secret redaction, middle-out truncation, text compaction, context window compaction, engine calls with fallback chains |
| `internal/resilience/` | Circuit breaker with Closed/Open/HalfOpen states, exponential backoff |
| `internal/providers/` | Provider capability specs (stream_options, auto_tool_choice, content_arrays), per-provider sanitization, response transformers |
| `internal/adapter/` | Provider Adapter pattern: request mutation, auth injection, response mutation, error classification |
| `internal/routing/` | Dynamic provider resolution, agent fallback chains, upstream request transformation, API key injection |
| `internal/gateway/` | NenyaGateway struct, HTTP client configuration, token counting |
| `internal/proxy/` | HTTP handlers, content pipeline orchestration, upstream forwarding with retry, transparent SSE streaming |

## Request Lifecycle

```
Client Request
  │
  ├─ POST /v1/chat/completions
  │   ├─ Auth check (Bearer token)
  │   ├─ Parse JSON body, extract model
  │   ├─ Resolve agent or provider
  │   ├─ Response cache lookup (if enabled)
  │   │   └─ HIT → replay cached SSE, done
   │   ├─ Content pipeline (best-effort — failures logged, never block request):
   │   │   ├─ Prefix cache optimizations
  │   │   ├─ Tier-0 regex secret redaction
  │   │   ├─ Text compaction
  │   │   ├─ Window compaction (if enabled)
  │   │   ├─ 3-tier engine interception (soft/hard limits)
  │   │   └─ JSON minification
  │   ├─ Agent fallback loop:
   │   │   ├─ Circuit breaker check (skip if Open/ForceOpen)
   │   │   ├─ Circuit breaker re-check before send (skip if tripped during queue wait)
   │   │   ├─ Rate limiter check
  │   │   ├─ adapter.MutateRequest() (payload transform)
  │   │   ├─ adapter.InjectAuth() (header signing)
  │   │   ├─ HTTP POST to upstream
  │   │   ├─ Error classification (adapter.NormalizeError)
   │   │   │   ├─ ErrorRetryable → exponential backoff, retry
   │   │   │   ├─ ErrorRateLimited → cooldown, retry with delay
   │   │   │   ├─ ErrorQuotaExhausted → long cooldown
   │   │   │   ├─ ErrorPermanent → try next target (or return error if no more targets)
  │   │   └─ On success → circuit breaker.RecordSuccess()
  │   └─ SSE stream pipeline:
  │       ├─ stallReader (120s idle timeout)
  │       ├─ SSETransformingReader (adapter.MutateResponse per chunk)
  │       ├─ StreamFilter (blocked execution patterns)
  │       ├─ immediateFlushWriter (Flush after every Write)
  │       └─ sseTeeWriter (capture for response cache)
  │
  ├─ GET /v1/models
  ├─ POST /v1/embeddings
  ├─ GET /healthz
  └─ GET /statsz
```

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
2. Applies per-target timeout from `EngineConfig.TimeoutSeconds`
3. On failure, logs a structured warning and tries the next target
4. Returns on first success; on all failures, returns the last error

All engine calls log `caller` (`security_filter` or `window`), `agent` name (or `inline`), `provider`, `model`, and `attempt`/`total` for observability.

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
| `SSETransformingReader` | Read from upstream | Parses SSE frames, calls `adapter.MutateResponse()` per chunk, extracts usage |
| `StreamFilter` | Read | Kills stream if blocked execution patterns detected |
| `immediateFlushWriter` | Write to client | Wraps `http.ResponseWriter`, calls `Flush()` after every `Write()` |
| `sseTeeWriter` | Write to client + buffer | Captures response bytes for response cache storage |

Buffer pooling via `sync.Pool` (32KB buffers) reduces GC pressure under high concurrency.

## Response Cache

In-memory LRU cache with deterministic SHA-256 fingerprinting. See [`CONFIGURATION.md`](CONFIGURATION.md#response_cache) for configuration.

- **Cache key**: SHA-256 of canonical JSON subset (`model`, `messages`, `temperature`, `top_p`, `max_tokens`, `tools`, `tool_choice`, `response_format`, `stop`, `stream`)
- **Storage**: Completed SSE streams captured via `sseTeeWriter`
- **Replay**: Cache hits replay the stored SSE stream with `X-Nenya-Cache-Status: HIT`
- **Bypass**: `x-nenya-cache-force-refresh` header forces cache miss
- **Eviction**: Background goroutine sweeps expired entries every `evict_every_seconds`

## Graceful Degradation

Nenya is designed to never break the flow between AI coding clients (OpenCode, Aider) and upstream providers. The following mechanisms ensure resilience:

### Best-Effort Content Pipeline

The entire content pipeline (prefix cache, redaction, compaction, window, engine interception) runs as best-effort. Any failure is logged as a warning and the request proceeds with the original payload. No pipeline error results in an HTTP 500 to the client.

### Skip on Engine Failure

When `security_filter.skip_on_engine_failure` is `true` (default):
- **Soft limit** (Tier 2): Engine summarization fails → original payload forwarded unchanged
- **Hard limit** (Tier 3): Engine summarization fails → original payload forwarded unchanged (not truncated)

This means users without a local Ollama instance can use Nenya purely as a routing proxy with regex-based secret redaction.

### Exhaustive Fallback

For agents with multiple models in a fallback chain, non-retryable errors (e.g., HTTP 400, 401) from one provider still try the next provider before returning an error to the client. Only when all targets are exhausted does the gateway return an error.

### Client Disconnect Cleanup

When a client disconnects during SSE streaming, the upstream connection is aborted and a 5-second timeout ensures the stream copy goroutine doesn't leak indefinitely.
