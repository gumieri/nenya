# Changelog

All notable changes to this project will be documented in this file.

## [Unreleased]

### Added
- Path traversal security tests in `config/loading_path_security_test.go` for config loading and prompt file resolution
- Centralized secure memory zeroing primitive in `internal/util/zero.go`

### Changed
- **Phase 014**: Decomposed monolithic `chat.go` into focused modules: `constants.go`, `embeddings.go`, `usage.go`, `responses.go`, `mcp_loop.go`, `response_writer.go` for better maintainability
- **Phase 015**: Unified 6x duplicated passthrough response-handler pattern into a single `response_writer.go` module with shared `writeUpstreamResponse()` and `writeUpstreamBytesResponse()` helpers
- `interceptContent` now checks against configurable hard limit and applies trimming before Bouncer interception when payload exceeds the hard limit
- Updated `TransformDeps` to include `CountTokens` for token-aware truncation during request transformation
- Updated `prepareAndSend` to pass `CountTokens` to `TransformDeps`
- Models without `max_context` configured no longer get incorrect truncation (disabled with warning)
- Provider config merging now correctly merges user values over built-in defaults instead of overriding
- All error responses now include structured `error_kind` field
- Retry loop extracted into dedicated `retryLoop` struct for better readability
- Bidirectional Anthropic Messages API support via `/v1/messages` endpoint with full format conversion
- Pluggable interceptor chain for content preprocessing (redaction, entropy, TF-IDF, bouncer)
- Token-budget trimming pipeline (`TrimPayload`) that drops oldest non-system messages and applies token-aware middle-out truncation when payload exceeds hard limit
- Configurable `hard_limit_tokens` in `context` section to override the default `softLimit * 2` behavior
- Context-limit aware retry with automatic summarization fallback when `auto_retry_on_context_limit` is enabled
- Structured error responses with `error_kind` field for programmatic client diagnostics
- Local Ollama engine lifecycle manager with LRU eviction and startup preloading
- Ollama model discovery enrichment via `/api/show` for accurate context limits and capabilities
- Strip unsupported `tool_choice` field from Ollama requests automatically
- Request-scoped logging with operation and API key correlation
- Response cache instrumentation with debug-level logging
- Metrics for interceptors (duration, applied, errors), context-limit errors, summarization retries

### Fixed
- **Phase 013**: Fixed data race in circuit breaker `SnapshotDetailed()` — now acquires `cb.backoff.mu` lock before accessing `backoff` map
- **Phase 013**: Fixed potential goroutine leak in response cache evictor — now uses `sync.WaitGroup` for clean shutdown on gateway stop
- **Phase 013**: Fixed goroutine leak in Ollama transport loops on context cancellation — added proper `select` on `closeCh` in all long-running loops
- **Phase 013**: Fixed lock deadlock risk in `SessionManager` — enforced strict lock ordering (EngineManager.mu → SessionManager.mu) in all methods
- **Phase 017**: Fixed metrics label sanitization — added package-level `labelEscaper` to sanitize Prometheus labels with double-quotes
- **Phase 017**: Fixed integer overflow risk in `CountRequestTokens()` — added `math.MaxInt` guard before `sb.Grow(n*500)` capacity allocation
- **Phase 017**: Fixed panic in `writeHistogramMap()` when labels contain empty strings — added explicit nil check before building label array
- **Phase 018**: Replaced `context.TODO()` with `context.Background()` in 6 locations in `response_cache.go` (startup evictor goroutine and caching operations)
- **Regression (HIGH)**: Fixed silent response dropping in `writeUpstreamResponse()` and `writeUpstreamBytesResponse()` — removed `ctx.Err() != nil` early return that was aborting entire HTTP responses when client disconnected before headers were sent
- **Regression (MEDIUM)**: Fixed TOCTOU race in `SessionManager.UnloadModel()` — re-checks model existence after acquiring Lock to prevent deleting freshly-reloaded models
- **Regression (MEDIUM)**: Responses passthrough now uses URL-decoded path for `isPathSafe()` checks (hardened against double-encoded traversal)
- **Regression (MEDIUM)**: `LoadModel()` double-check pattern preserves data safety across concurrent loads (occasional duplicate HTTP fetches are acceptable)
- Summarization retry loop now correctly passes agent/provider/model parameters instead of attempting to extract from payload
- Main.go shutdown bug: fixed atomic.Bool usage (was calling as function instead of Store method)
- Staticcheck SA1012 violations in response_cache.go (never pass nil Context)

## [0.3.0] - 2025-05-22

### Added
- Per-key RBAC enforcement with roles (admin, user, read-only), agent scoping, and endpoint allowlists
- Multi-account per-provider API keys with LRU selection and model-aware key rotation
- Semantic caching with embedding-based similarity search and cache-aware prompt rewriting
- Per-provider RPM/TPM rate limit overrides
- Grafana dashboard with comprehensive metrics panels
- Extension API endpoints: image generation, audio transcription, TTS, moderation, reranking, A2A
- Moonshot provider with kimi-k2 base model
- ServiceKinds architecture (LLM, embedding, TTS, STT, image, rerank, webSearch)

### Changed
- Provider-level capability flags replaced with typed ServiceKinds
- Module renamed from `nenya` to `github.com/nenya` for Go 1.26 compatibility
- All `context.TODO()` calls in MCP keepalive replaced with `context.WithTimeout`

### Fixed
- Multi-provider deduplication in MergeCatalog
- Integer overflow in slice allocation using `util.AddCap`
- Cerebras marked as not supporting `reasoning_content`
- Tool-call ID mismatch in Anthropic adapter
- Anthropic adapter whitespace-only content trimming to prevent empty blocks
- Anthropic tool_calls converted to tool_use blocks correctly
- Tool messages coalesced and tool_use_ids validated for Anthropic
- Anthropic consumed SSE events suppressed from leaking to clients

## [0.2.0] - 2025-05-18

### Added
- Semantic caching infrastructure with embedding provider interface and cosine similarity index
- Token-budget trimming pipeline with `TrimPayload` helper and configurable hard-limit fallback
- Comprehensive test coverage improvements across config, proxy, gateway, resilience packages
- Token approximation using tiktoken for embedding operations
- GoDoc comments for retry helpers and pipeline packages

### Changed
- Config rename: `security_filter` → `bouncer`
- Truncation and TF-IDF settings consolidated into new `context` section
- Boolean tracking replaced with `*bool` pointers for better config validation

### Fixed
- Duplicate condition in `TruncateMiddleOutByTokens`
- Context.Background usage in stream.go embedding operations
- Client hangs on upstream provider failures
- SSE/stream reliability improvements
- TestCalculateBackoff robustness with jitter averaging

## [0.1.1] - 2025-05-15

### Fixed
- Fall through to next model when upstream stream stalls (empty=true in retry loop)

## [0.1.0] - 2026-05-09
### Added
- Initial implementation of Nenya AI API Gateway/Proxy.
