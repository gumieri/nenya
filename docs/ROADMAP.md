# Nenya Roadmap

This document outlines planned features and improvements for Nenya. Items are grouped by domain but not prioritized — implementation order will depend on user demand and technical feasibility.

---

## API Surface Expansion

### 1. Non-Streaming Chat Completions
**Current**: Streaming only (`/v1/chat/completions` with SSE)  
**Planned**: Support synchronous non-streaming responses

Buffer the full upstream response and return as a single JSON object. Some clients and workflows require synchronous responses rather than streaming.

**Scope**:
- Add `stream: false` parameter support to `/v1/chat/completions`
- Buffer upstream SSE stream into complete response
- Return standard OpenAI-compatible JSON format
- Maintain existing streaming behavior as default

**Implementation** (Completed 2026-04-30):
- Extract `stream` boolean from JSON payload in `validateChatRequest`
- Thread `stream` through `forwardOptions` → `retryLoop` → `prepareAndSend`
- `handleUpstreamResponse` returns `actionResponse` for non-streaming (vs `actionStream` for streaming)
- `handleNonStreamingResponse` buffers full body, parses JSON, returns single object
- Retry loop handles `actionResponse` similarly to `actionStream` (with fallback to next target on empty response)
- Comprehensive test coverage: non-streaming happy path, empty response handling

---

### 2. Responses API: Full Lifecycle
**Current**: Full lifecycle (`/v1/responses`) — **Completed 2026-04-30**
**Planned**: Full lifecycle management (GET/cancel/delete/compact)

Implement the OpenAI Responses API with full CRUD operations beyond blind passthrough.

**Scope**:
- `GET /v1/responses/{response_id}` — retrieve response details
- `POST /v1/responses/{response_id}/cancel` — cancel in-progress response
- `DELETE /v1/responses/{response_id}` — delete response
- `POST /v1/responses/{response_id}/compact` — compact response content
- Maintain passthrough for unsupported providers

**Implementation**:
- `handleResponses` supports all HTTP methods (GET, POST, DELETE) with path-based routing
- Model resolution via `ResolveProviders` with fallback to `getDefaultResponseProvider`
- URL derivation uses `provider.BaseURL + "/responses"` with sub-path forwarding
- `isPathSafeResponses`, `readResponsesBody`, `resolveResponsesURL`, `buildResponsesContext` helpers keep complexity ≤15
- Retry logic via `util.DoWithRetry`
- Existing retry/error tests pass unchanged

---

### 3. Embeddings (Enhanced)
**Current**: Enhanced (`/v1/embeddings`) — **Completed 2026-04-30**
**Planned**: Proper routing with token counting

Add intelligent routing and token counting for embeddings requests, not just blind proxying.

**Scope**:
- Provider routing via model name (existing ModelRegistry)
- Token counting for input text (tiktoken integration)
- Usage tracking via `/statsz` and metrics
- Per-provider request/response transformation if needed

**Implementation**:
- Added `countEmbeddingInputTokens` helper (estimates tokens from `input` string or array)
- `handleEmbeddings` now calls `gw.RateLimiter.Check` and records `Stats.RecordRequest` / `Metrics.RecordUpstreamRequest`
- Changed URL derivation from `provider.URL` trimming to `provider.BaseURL + "/embeddings"`
- `forwardEmbeddingsRequest` parses response `usage` field and records `Stats.RecordOutput` / `Metrics.RecordTokens` for output tokens
- Extracted `recordEmbeddingUsage` helper to reduce nesting complexity

---

### 4. File Operations
**Current**: Implemented (`/v1/files`) — **Completed 2026-04-30**
**Planned**: CRUD: create/list/get/delete/content

Implement file upload, retrieval, and management per provider (OpenAI, Anthropic, etc.).

**Scope**:
- `POST /v1/files` — upload file
- `GET /v1/files` — list files
- `GET /v1/files/{file_id}` — get file metadata
- `GET /v1/files/{file_id}/content` — get file content
- `DELETE /v1/files/{file_id}` — delete file
- Provider-specific handling (OpenAI vs Anthropic file APIs)

**Implementation**:
- Shared `handleFilesOrBatches` function handles all endpoints
- `isPathSafe` uses `url.PathUnescape` + `path.Clean` to block URL-encoded traversal (`%2e%2e`)
- Rate limiting and metrics recording consistent with passthrough handler
- `buildFilesContext` returns idiomatic cancel func (no nil)
- Comprehensive test coverage: JSON upload, multipart, empty body, too-large body, list, get, delete, content download
- Uses `provider.BaseURL + "/files"` for URL derivation

---

### 5. Batch Processing
**Current**: Implemented (`/v1/batches`) — **Completed 2026-04-30**
**Planned**: Native batch lifecycle

Implement batch API for processing multiple requests efficiently.

**Scope**:
- `POST /v1/batches` — submit batch request
- `GET /v1/batches/{batch_id}` — check batch status
- `POST /v1/batches/{batch_id}/cancel` — cancel batch
- `GET /v1/batches/{batch_id}/results` — retrieve batch results
- Per-provider batch API translation

**Implementation**:
- Shared `handleFilesOrBatches` for both files and batches
- Same rate limiting, metrics, auth, retry, and timeout patterns as files

---

### 6. Passthrough Proxy
**Current**: Implemented (`/proxy/{provider}/*`) — **Completed 2026-04-22**
**Planned**: `/proxy/{provider}/*` — arbitrary method passthrough

Enable raw proxying to any provider endpoint for unsupported APIs or custom integrations.

**Scope**:
- Route pattern: `/proxy/{provider}/*` (e.g., `/proxy/anthropic/v1/messages`)
- Support all HTTP methods: GET, POST, PUT, PATCH, DELETE, HEAD, OPTIONS
- Auth injection per provider (API keys, bearer tokens)
- Header stripping (hop-by-hop, auth, host, content-length)
- Streaming auto-detect (`text/event-stream` → SSE pipe)
- Usage tracking for passthrough requests

**Example**:
```
POST /proxy/anthropic/v1/messages
Authorization: Bearer nenya-client-token
→ Forwards to Anthropic with provider-specific auth
```

**Implementation**:
- Added `BaseURL` field to `Provider` struct for URL derivation
- Implemented `handlePassthrough()` with SSE streaming support
- Comprehensive test coverage (10 table-driven tests)
- Applies auth, rate limiting, metrics, logging, and panic recovery
- Bypasses content pipeline, circuit breaker, and retry logic (raw proxy)

---

### 7. OpenAPI Specification
**Current**: Not available  
**Planned**: Auto-generated spec served at `/openapi.json`

Generate and serve OpenAPI 3.0 specification for all endpoints.

**Scope**:
- Auto-generate from code annotations or manual spec
- Serve at `/openapi.json` and `/docs` (Swagger UI optional)
- Include all `/v1/*` endpoints, `/proxy/*`, admin APIs
- Model schemas for request/response bodies
- Authentication documentation

---

## Intelligence & Routing

### 8. Model Discovery
**Current**: Static config only (ModelRegistry in JSON)  
**Planned**: Dynamic fetch from providers at startup

Fetch `/v1/models` from each provider at startup and merge with static configuration.

**Scope**:
- Fetch `/v1/models` from each configured provider at startup
- Merge with static ModelRegistry (static takes precedence)
- Cache results in memory (refresh on SIGHUP or periodic)
- Update `/v1/models` catalog with discovered models
- Handle provider failures gracefully (use static fallback)

---

### 9. Semantic Caching
**Current**: Exact-match LRU with SHA-256  
**Planned**: Vector-based semantic caching

Cache semantically similar queries using vector embeddings and similarity scoring.

**Scope**:
- Local embedding generation (via configured provider or local model)
- Cosine similarity threshold for cache hits
- In-memory vector store (stdlib-only) or optional backend (Qdrant, etc.)
- Cache key: embedding + model + parameters
- Cache invalidation: TTL, size limits, manual flush
- Metrics: cache hit rate, similarity scores

**Design decision**: With zero-dep philosophy, default to local-only (in-memory embeddings, cosine similarity). Optional backend support via config.

---

### 10. Auto-Fallback Intelligence
**Current**: Sequential + round-robin fallback  
**Planned**: Elo rank-based with capability overlap scoring

Intelligent fallback based on model quality rankings and capability overlap.

**Scope**:
- Elo ranking system for models (configurable or learned)
- Capability overlap scoring (e.g., vision, tools, context window)
- Fallback chain optimization based on success rates
- Per-agent fallback strategies (cost vs quality vs speed)
- Metrics: fallback rates, success rates per model

---

### 11. Model Metadata
**Current**: Not available  
**Planned**: External model list with pricing, categories, rankings

Maintain external model metadata for cost tracking and routing decisions.

**Scope**:
- External model list (JSON file or API)
- Per-model metadata: pricing (input/output tokens), categories, rankings
- Context window limits per model
- Capability flags (vision, tools, streaming, etc.)
- Integration with auto-fallback and routing
- Usage cost calculation per model

---

## Admin API (for External Dashboard)

### 12. Usage Analytics API
**Current**: `/statsz` with per-model token counters  
**Planned**: Detailed per-agent/model/provider breakdowns

Rich usage analytics for external dashboard consumption.

**Scope**:
- `GET /admin/usage` — query usage with filters (time range, agent, model, provider)
- Breakdown: requests, input tokens, output tokens, errors, cost
- Time-series data (hourly/daily aggregates)
- Per-agent, per-model, per-provider views
- Export formats: JSON, CSV
- Authentication: `client_token` required

**Example response**:
```json
{
  "period": "2026-04-01..2026-04-22",
  "total_requests": 1234,
  "total_tokens": 567890,
  "by_agent": {
    "default": {"requests": 800, "tokens": 400000},
    "coding": {"requests": 434, "tokens": 167890}
  },
  "by_model": {
    "claude-sonnet-4-5": {"requests": 600, "tokens": 300000},
    "gpt-4o": {"requests": 634, "tokens": 267890}
  }
}
```

---

### 13. Configuration Management API
**Current**: JSON file editing + SIGHUP hot-reload  
**Planned**: CRUD via API with internal hot-reload

Manage agents, providers, and keys via API instead of editing JSON files.

**Scope**:
- `GET /admin/config` — read full config
- `POST /admin/config/agents` — create/update agent
- `DELETE /admin/config/agents/{agent_name}` — delete agent
- `POST /admin/config/providers` — create/update provider
- `DELETE /admin/config/providers/{provider_name}` — delete provider
- `POST /admin/config/keys` — add/update API key
- `DELETE /admin/config/keys/{key_id}` — delete API key
- Triggers internal hot-reload on changes
- Validation before apply (reject invalid configs)
- Authentication: `client_token` required

---

### 14. Circuit Breaker Management API
**Current**: `/statsz` shows CB state  
**Planned**: Inspect and control breaker state per target

Admin API for circuit breaker inspection and manual control.

**Scope**:
- `GET /admin/circuit-breakers` — list all breakers with state
- `GET /admin/circuit-breakers/{agent}/{provider}/{model}` — inspect specific breaker
- `POST /admin/circuit-breakers/{agent}/{provider}/{model}/open` — force open
- `POST /admin/circuit-breakers/{agent}/{provider}/{model}/close` — force close
- `POST /admin/circuit-breakers/{agent}/{provider}/{model}/half-open` — force half-open
- `POST /admin/circuit-breakers/{agent}/{provider}/{model}/reset` — reset counters
- Authentication: `client_token` required

**Example response**:
```json
{
  "state": "open",
  "failures": 10,
  "successes": 0,
  "last_failure": "2026-04-22T10:30:00Z",
  "threshold": 5,
  "timeout": "60s"
}
```

---

## Non-Goals

These features are explicitly out of scope for Nenya's single-user, local-first design:

- **Multi-tenancy** — Nenya is designed for single-user, local deployment
- **Per-key budgets and rate limiting** — No multi-user isolation needed
- **Cluster mode** — Single-node by design, no horizontal scaling
- **Admin UI** — Admin APIs will be provided, but UI is a separate project
- **Semantic search** — Not relevant for gateway use case
- **Workflow engine** — Agents serve a similar purpose, no need for complex workflows

---

## Implementation Notes

### Zero-Dependency Philosophy
All features must maintain Nenya's zero external dependency policy. Optional backends (e.g., Qdrant for semantic caching) should be opt-in via config, not required dependencies.

### Backward Compatibility
All new features must maintain backward compatibility with existing `/v1/chat/completions` streaming behavior. Non-streaming, passthrough, and admin APIs are additive.

### Security
All admin APIs require `client_token` authentication. Passthrough proxy must strip sensitive headers and inject provider-specific auth securely.

### Testing
Each feature must include:
- Unit tests for core logic
- Integration tests for API endpoints
- Fuzz tests for parsing/normalization (where applicable)
- Table-driven tests for edge cases

---

*Last updated: 2026-04-30*
