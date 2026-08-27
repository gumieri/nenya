# Changelog

All notable changes to this project will be documented in this file.

## [Unreleased]

## [0.10.0] - 2026-08-27

### Added
- **Sticky session routing** (`strategy: "sticky"`): sessions (identified by agent + system prompt + first user message) pin to one provider/model so provider-side prefix caches stay warm across turns. LRU pin store (512 entries), `sticky_session_ttl_seconds` (default 3600, max 86400), failover re-pinning to the next healthy target, survives SIGHUP reloads. Metrics: `nenya_session_active`, `nenya_session_pin_changes_total{reason}`
- **Stream continuation** (`governance.stream_continuation`): transparently resumes upstream SSE streams cut mid-generation (no `finish_reason`, no `[DONE]`) by re-dispatching the same target with the partial assistant message appended. Fields: `enabled` (default true), `max_attempts` (default 2, cap 5, includes original attempt), `same_model_only` (default true; explicit false opts out entirely), `include_reasoning` (default false). Skipped while a tool call is in flight. Metrics: `nenya_stream_continuations_total{reason}`, `nenya_stream_interrupts_total{reason}`
- **Provider model allowlists** (`providers.<name>.allowed_models`): RE2 regex patterns restricting which models are usable per provider. Non-matching models are dropped from `/v1/models` discovery and blocked in routing (400 `model_not_found`); matching static-registry models are re-added when discovery omits them. Empty/omitted = all models allowed.
- **Early stream-error failover** (`governance.early_stream_error_failover`, default true): upstream SSE error events at the stream head (before HTTP headers are committed) trigger failover to the next target in the agent chain instead of forwarding error bytes. Metric: `nenya_stream_early_errors_total{outcome="failover|forwarded_last_target"}`

### Changed
- Removed the blanket client-side timeout that could interrupt long-running streams mid-flight; stall detection (`stream_idle_timeout_seconds`) now solely governs stalled upstreams

### Fixed
- `bouncer.enabled=false` is now honored; the interceptor chain is rebuilt on SIGHUP reload
- Stream endings without a terminal event are classified correctly, and the circuit breaker is no longer penalized for gateway-side errors
- Session pin last-seen timestamps are updated under lock (data race)
- `trackInFlight` cleanup is invoked via `defer` so the in-flight request counter cannot leak on early returns

## [0.9.1] - 2026-08-13

### Added
- **Prefix-cache token accounting on `/metrics`**: new counters `nenya_cache_read_tokens_total`, `nenya_cache_creation_tokens_total`, and `nenya_cache_miss_tokens_total` with `{model, agent, provider}` labels. These track upstream prompt/prefix-cache token totals (distinct from the `nenya_cache_hit_total`/`nenya_cache_miss_total` response-cache event counters) for billing reconciliation. Wired into both the streaming usage callback and the non-streaming path, including Anthropic's native `cache_creation_input_tokens` field.

### Fixed
- Replaced `context.TODO()` with `context.Background()` in `response_cache.go` (startup evictor goroutine and caching operations)

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
