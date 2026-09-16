# Changelog

All notable changes to this project will be documented in this file.

## [Unreleased]

### Added
- **Per-model concurrency admission control**: providers that rate-limit by concurrent in-flight requests per model (e.g. the Z.AI GLM Coding Plan) can now be capped via `providers.<name>.max_concurrent_requests` and `providers.<name>.model_concurrency` (per-model overrides), with a global `governance.max_concurrent_requests` fallback. Excess requests queue context-aware until a slot frees instead of colliding with the upstream cap; the slot is held for the full SSE stream/response lifetime. ZAI error `1302` (concurrency) is now classified `concurrency_limited` — no provider cooldown, no circuit-breaker failure, short ~200ms retry — while `1303` (frequency) keeps rate-limit semantics. Rate-limit RPM/TPM buckets are now keyed by provider name, fixing `zai`/`zai-coding-plan` silently sharing one `api.z.ai` bucket. Metrics: `nenya_concurrency_inflight{provider,model}`, `nenya_concurrency_wait_seconds`, `nenya_concurrency_rejected_total`, `nenya_concurrency_limited_total`
- **Synthesized `x-opencode-session` for session-unaware clients**: when the client does not send the header, Nenya now generates a stable per-conversation session ID (`nenya-<hash16>`, derived from agent + system prompt + first user message — the same identity as sticky routing) on both chat routes, giving session-unaware clients provider-side routing/prompt-cache affinity and satisfying upstreams that require the header

### Changed
- **Static model catalog refresh** (sources: LiteLLM catalog, models.dev, router-for-me snapshot, 2026-09-16): repriced the Gemini 3.x flash line and 3.1-pro, fixed shared 2.5-flash/flash-lite pricing, corrected Anthropic limits (sonnet-4-6 → 1M context, 3-7-sonnet → 200k/64k, 3-5-sonnet → 8k output cap) and opus-4-8 pricing, corrected DeepSeek v4 pro/flash pricing, and raised Groq llama-3.3-70b output to 32k; added gemini-3.1-flash-lite, gemini-3.5-flash-lite, gemini-3.6/3.7/3.8-flash, gemini-flash-latest/flash-lite-latest/pro-latest aliases, claude-opus-5, claude-fable-5-1, claude-mythos-5/5-1, deepseek-v4.1-flash, glm-5-code, grok-4.1-fast/4.5/4.6/composer-2.5-fast, and the current Groq catalog (llama-3.1-8b-instant, llama-4 maverick/scout, kimi-k2-instruct-0905, gpt-oss-120b/20b, qwen3-32b/3.6-27b/3.8-27b); removed the retired mixtral-8x7b-32768 from Groq

### Fixed
- **OpenCode Zen/Go `MissingSessionID` errors**: the client-supplied `x-opencode-session` header is now forwarded to upstreams on all dispatch paths (was: silently dropped by the header allowlist); OpenCode Go rejects chat requests without this header. Unusable client values — whitespace-only, non-printable-ASCII or non-ASCII, or over-long — are replaced with the synthesized session ID, or dropped when no session key is derivable, instead of failing every round trip mid-dispatch. (Tab characters inside values are allowed.)

## [0.13.0] - 2026-09-10

### Added
- **Offline demo harness** (`examples/demo/`): local mock upstream that dumps every received request, demo config, lifecycle scripts, and a committed [vhs](https://github.com/charmbracelet/vhs) tape rendering `docs/demo.gif`. Uses only documented fake secrets and runs fully offline. New `mise run demo` task regenerates the GIF; banner wording centralized in `examples/demo/banner.sh`
- **Token-savings telemetry**: `nenya_pipeline_tokens_saved_total{source}` aggregates tokens removed by pipeline stages (trim, window, bouncer summarization) at the sites where deltas are already computed; `nenya_tokens_estimated_total{direction="client"}` records the pre-pipeline estimate at request build, making whole-pipeline savings derivable in Prometheus
- **CONTRIBUTING.md** with the development gate (build/test/vet/lint), commit conventions, and the zero-dependency policy

### Changed
- `nenya_tokens_estimated_total{direction="input"}` is now recorded after the request transform, reflecting the payload actually dispatched (was: pre-pipeline count), so `client − input` measures real savings
- README restructured for launch: quick start moved up, pain-first pitch, new "Why Nenya" section, authenticated smoke test replacing the unauthenticated health check

### Fixed
- Quick-start `client.json` heredoc was quoted, publishing a predictable literal token (`nk-$(openssl rand …)` never expanded); the token is now genuinely random
- `nenya_pipeline_redactions_total` was never incremented — `Metrics.RecordRedaction` existed but had no callers; the Tier-0 redaction interceptor now records every substitution
- Package metadata declared `license: MIT` while the project is Apache-2.0 (deb/rpm/arch/AUR/nix)

## [0.12.0] - 2026-09-09

### Added
- **Weighted fair-queuing multi-account rotation** (`agent.accounts[].weight`): weighted round-robin across provider accounts with graceful degradation to unweighted behavior
- **Rate-limit windows drive cooldown derivation** (`resilience.cooldown_from_upstream_limits`): upstream `x-ratelimit-*` headers and quota bodies set cooldown durations instead of fixed backoff
- **Error-scope axes** (`providers.<name>.request_scoped_errors`): rules marking provider errors as the client's fault even when the status class suggests otherwise; matched errors skip cooldown/rotation state and surface directly, and client cancels are connection-scoped (no retry, no state writes)
- **HTTP-200 embedded error classification**: provider errors delivered inside 200 bodies (JSON and SSE) are classified and counted like real failures
- **Per-provider `retryable_phrases`**: case-insensitive error-body substrings that make a 4xx response retryable/failover-able for aggregator-relayed upstreams, in addition to the built-in pattern sets

### Fixed
- `/v1/messages` now enforces per-key agent scoping (RBAC) like `/v1/chat/completions`; unreadable or oversized bodies fail closed
- The transform-side trim budget is derived from the model's context window (3/4 headroom; `context.hard_limit_tokens` takes precedence), never from the output-token cap

## [0.11.0] - 2026-09-02

### Added
- **Sticky account pinning**: agent-pinned provider accounts (target-build preference via `SelectAccountByID`, account-granularity pin promotion on cooldown/exhaustion, LRU fallback) complement session pinning for prefix-cache stability
- **GLM-5.3 and GLM-5.3-Flash** added to the static model registry
- README request-flow diagram converted to Mermaid

### Fixed
- **MCP transport hardening** (24 review rounds): TLS handshake timeout, JSON-RPC handshake host-validation bypass, bounded SSE lines, 4xx no-retry on connect, endpoint renegotiation, stream-death pending-call drain, init-phase leak, connect TOCTOU, owned-stream lifetime, and close/connect race families closed
- Error-kind class consistency across gateway errors, `/metrics` duplicate families removed, RBAC denial coverage for all pass-through endpoints, sticky-pinning races hardened, bounded multi-exhaustion retry

## [0.10.1] - 2026-08-27

### Fixed
- docs(adapters): corrected the zai adapter init registration note

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
