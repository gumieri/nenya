# Changelog

All notable changes to this project will be documented in this file.

## [Unreleased]

### Added
- **External consumer contract** (`CONTRACT.md`): spec-first interface contract for managers/tooling (nenyactl and similar). Pins the stable surface and defines the target CLI surface (`version`/`paths`/`describe`/`example-config`/`service-unit`/`config set`/`secret set`), on-disk layout and merge precedence, secrets source order, release artifact/member names, integrity verification (SHA-256 + cosign), service-unit and signal semantics, HTTP health/auth rules, and the `contract_version` change process. Marks each surface `stable` vs `target`; eliminates the drift class where a consumer re-encodes Nenya's loader, paths, and archive layout. Linked from the README documentation table
- **Hot-path JSON: top-level field extraction without full unmarshal (NENYA-35)**: the format-detection peek on every request unmarshaled the entire body into a map to read one field. New `util.ExtractTopLevelField` is a hand-rolled RFC 8259-validating byte scanner (zero allocations on miss, string/escape/control-byte aware, nested-container structural validation, last-key-wins duplicate semantics, truncated/trailing-garbage rejection) — **~16× faster than full unmarshal on a production-shaped body (9.7µs vs 158µs, 0 vs 202 allocs/op)**. Correctness is fuzz-enforced against `json.Unmarshal` as oracle (60s+ continuous) with a semantic matrix covering duplicate keys, unicode-escaped keys, whitespace, non-object top levels, and number edge cases (leading zeros, malformed exponents). Perf-guard test pins the regression ceilings in CI: allocs/op must stay strictly below full unmarshal (ceiling 12) and ns/op under 100µs
- **Window compaction summary cache + regeneration hysteresis (NENYA-24)**: summarize-mode compaction regenerated an LLM summary of the growing history head on every request — the compacted head changed every turn, so upstream prompt caches never hit while compaction was active (audit finding F3, the highest-impact cache-buster). Summaries are now cached per conversation lineage (hash of leading system + first history message — stable under appends) with two reuse tiers: byte-identical history reuses the summary outright (fixing same-input retry instability and wasted engine calls), and growth below `window.summary_regen_ratio` (default 0.25) reuses the stored summary with only the delta messages appended verbatim — the compacted prefix stays byte-identical so prefix caches hit again. Bounded LRU (`window.summary_cache_size`, default 512 lineages; cold start on SIGHUP reload costs one regeneration per active conversation); engine failure with a cached summary still serves compaction (fail-open). Metrics in `/statsz` remain per-model; cache counters exposed on the cache itself. Truncate/tfidf modes are already deterministic and unaffected
- **Bounded circuit-breaker registry (NENYA-31)**: the per agent+provider+model circuit registry was an unbounded map keyed on caller-supplied model names — a memory-exhaustion vector under adversarial or churn-heavy IDs. The registry now caps at 1024 circuits with a 10-minute idle TTL (defaults, mirroring the evidence design): when at cap, a lazy sweep (no background goroutine) evicts Closed circuits idle past the TTL oldest-use-first, then the least-recently-used Closed circuits; Open/HalfOpen circuits are never evicted — losing an active breaker would hide an in-progress investigation — so worst-case growth is cap + simultaneously-non-closed circuits. Expired modelLocks are pruned in the same sweep. `registry_size` and `registry_evictions` surface in `/statsz` via `SnapshotDetailed` (`RegistryStats()` accessor added)
- **Deterministic tool-call identity (NENYA-48)**: centralized synthetic tool-call ID generation in `internal/util` (`SyntheticToolCallID`/`IsSyntheticToolCallID`/`ValidToolCallID`/`ParseToolCallIndex`) with one stable `call_` prefix; array-position fallback when the upstream omits the call index (never 0-for-all, which collided across calls in one delta); deterministic deconfliction of duplicate upstream IDs across indexes (first claimant keeps it, later ones re-keyed from the index — stable across requests since clients and ThoughtSigCache replay on these IDs); invalid/empty/oversized (>128 chars) upstream IDs replaced with the deterministic synthetic ID; all generation sites (`stream/reader.go`, `ollama_transformer.go`, `proxy/mcp_tools.go`) route through the shared helper
- **Streaming terminal-state matrix completion (NENYA-46)**: audit of every (upstream termination × format) cell against the CLIProxyAPI evidence series — premature-close/empty/benign-truncation terminals, zero-token usage filtering, frame cloning, idempotent tool-call accumulation, and late-usage extraction were already covered (NENYA-28/27, Phase 0/1); Responses-format cells are N/A (no Responses SSE translation). Two real gaps closed: (1) `[DONE]`-without-`finish_reason` in OpenAI passthrough streams now gets a synthetic `finish_reason:"stop"` chunk emitted ahead of `[DONE]` so clients keying on completion signals finalize correctly (narrow gate: passthrough only, content-seen only, never in discard mode, never when a finish was seen); (2) argument chunks buffered waiting for a tool name that never arrives are no longer lost silently — end-of-stream telemetry warns with the pending count (the args were never forwarded, by design)
- **Param-compat sanitization + reject-retry safety net (NENYA-32)**: new `governance.param_compat` rules (config, `model_prefix`-keyed, shadowing the built-in table) drop parameters new model generations reject, clamp the `reasoning_effort` floor (unknown values never rewritten), and relax forced `tool_choice` to `auto` — the built-in table encodes the Gemini 3 sampling/`candidate_count` rejections. Complementary opt-in safety net `governance.auto_retry_on_param_reject`: a 400 naming a well-known chat parameter that is present in the payload gets exactly one strip-and-retry (consumes the next sweep slot like the context-limit retry), covering rejections the table didn't anticipate. Metrics `nenya_param_reject_strips_total{param,source}` and `nenya_param_reject_retries_total`
- **Gemini token-0 prefix immutability (NENYA-44)**: gemini-provider requests passing through the sanitize chain now demote mid-session `role=system` messages to user turns carrying a `[system]` role marker (block-array content gains a marker block, originals verbatim), so only the leading system run anchors the provider prompt prefix — mirroring NENYA-30 for Gemini's OpenAI-compat endpoint, where scattered system messages are merged unpredictably and bust the cached prefix. Demotion never splits an open function-call sequence: notices arriving between an assistant `tool_calls` message and its results are buffered and flushed before the next non-tool turn (or at conversation end), keeping tool runs contiguous
- **Anthropic mid-conversation system messages keep their cache breakpoints (NENYA-30)**: Anthropic 4.8+/5-family models accept `role=system` messages mid-conversation, and Claude Code appends system reminders there carrying a moving `cache_control` breakpoint. The Anthropic conversion previously hoisted every system message — regardless of position — into the top-level `system` prompt, dropping the breakpoint and mutating the cached prefix every turn (whole conversation billed uncached). Mid-conversation-capable models (family-inferred, overridable via catalog/config `capabilities: ["mid_conversation_system"]`) now keep mid-array system reminders in place with verbatim content blocks; only the leading system run is hoisted. The `/v1/messages` ingress path preserves system-reminder blocks symmetrically, so the moving breakpoint survives the full Anthropic→OpenAI→Anthropic round trip. Legacy models keep the exact prior hoist behavior, and a forced agent system prompt no longer displaces a client system message carrying a breakpoint (it is inserted after it)
- **Leading-turn affinity fork detection (NENYA-39)**: session identity now survives the classic generic-opener collision — two conversations that share the same first user turn (e.g. "hi") but diverge afterwards used to share one session pin and one synthesized upstream session ID, concentrating load on one account and mixing prompt-cache regions. A bounded flat KV (`AffinityStore`: leading-turn SHA-256 prefix-fingerprint chains, 64-turn depth, 16KiB part sampling with head/length/tail for oversized parts, 8192-entry LRU + 1h TTL, mutex-guarded) now validates the opening-turn key: continuations keep the identity, collisions fork a deterministic identity folded from the shared prefix (stable as the fork grows, distinct in the synthesized `X-Opencode-Session` header), and the original session is untouched. Memory ≈2KB/session worst case; chain build is linear in turns and independent of total history size. Deeper LCP routing is deliberately omitted: with nenya's turn-1-keyed identity, shared prefixes imply the same root key, so the collision guard is the only increment the LCP machinery can add — documented in the type
- **Session-sticky API keys under multi-account routing (NENYA-29)**: sessions (keyed by agent + system prompt + first user message) now deterministically reuse one account credential on every agent strategy — previously only the `sticky` strategy pinned accounts, so `fallback`/`round-robin` agents rotated per request via LRU and defeated upstream per-key prompt caches (every turn paid full input price). Non-sticky strategies get credential-only affinity: the front target's provider/model/account is recorded as the session pin (drift follows sibling accounts, failover promotes the pin) without ever reordering the operator-defined chain. Different sessions spread across accounts; sessionless traffic keeps rotating; per-provider opt-out via `providers.<name>.session_sticky_keys: false`. Race-clean pin/promotion paths
- **Gemini thought-signature coverage on all response paths (NENYA-51)**: the ThoughtSigCache was only populated on the streaming OpenAI-format path; three paths silently lost signatures, letting the sanitizer strip just-completed tool turns from history (silent context loss on Gemini 3). Now covered: non-streaming responses extract `extra_content` from `choices[].message/.delta.tool_calls` before any format conversion; the MCP multi-turn accumulator carries signatures through and synthetic assistant `tool_calls` include `extra_content` inline; Anthropic-source clients on Gemini upstreams run the provider transformer (signature caching) composed with the OpenAI→Anthropic converter instead of short-circuiting to the converter alone; and the stream cache now iterates all choices and associates signatures arriving in split delta chunks (signature chunk without id) with the earlier id at the same index
- **Truncated-stream cache guard + terminal validation (NENYA-27)**: streams the SSE reader sanitizes with an injected `gateway_error` terminal (mid-generation cuts, empty streams, oversize lines) are now treated as provider failures — target cooldown recorded, `nenya_stream_interrupts_total{reason="truncated"}` emitted, and the partial output is **never persisted to the response cache** nor fed to MCP auto-save, so an identical retry re-dispatches upstream instead of replaying a dead stream. Refusal/content_filter cache skipping is now decode-based (parses `finish_reason`/`stop_reason`/`refusal` fields from the captured terminal events) instead of substring-matching serialized JSON, eliminating false cache misses on user content that merely contains those words. (The Responses-format incomplete-terminal steps of the upstream evidence are N/A: nenya has no Responses-format SSE translation path.) Closes the NENYA-28 follow-up gap (reader-injected frames ending in `[DONE]` were cached)
- **Stream bootstrap buffering (NENYA-45)**: opt-in defense against upstreams that smuggle capacity rejections inside HTTP-200 SSE streams after handshake metadata (e.g. Codex `server_is_overloaded`) — invisible to the first-event probe, and fatal because the 200 is already committed and failover is impossible. `governance.stream_bootstrap_buffer_bytes` (per-provider override via `providers.<name>.stream_bootstrap_buffer_bytes`) holds handshake events up to a byte budget until the stream is decidable: a real output event (event-type allow-list — OpenAI content/tool_calls deltas, Anthropic `content_block_*`, Gemini `content.parts`; NOT a frame count, so metadata/rate-limit frames never release the buffer early) flushes and commits unchanged; an in-stream rejection fails over to the next target synchronously, pre-commit, with full circuit-breaker/cooldown accounting; a budget overflow degrades to unbuffered streaming. Post-commit errors remain typed stream errors (no retry). Metrics: `nenya_stream_bootstrap_total{outcome}` + `nenya_stream_bootstrap_hold_seconds`
- **Per-key rate limits + token budgets with priority tiers (NENYA-20)**: RBAC extension for metered multi-tenant usage. `api_keys` entries accept `ratelimit_max_rpm` (fixed-minute request window, enforced after auth with `429 rate_limited`), `token_budget_daily` (UTC-day token budget charged as non-refundable pre-dispatch estimate reservations), and `budget_tier` (`always` default | `fill`) — providers accept `token_budget_daily`, and once a provider's budget is exhausted `fill`-tier keys get `429` (reason `provider_budget`) while `always` keys keep being served. Denials feed `nenya_auth_denials_total{reason="rate_limited"/"budget"}`; per-key/per-provider usage is exposed under `key_usage` in `/statsz`. Counters are in-memory and mutex-guarded (single-process by design; externalization point documented)
- **Per-provider model ID aliasing (NENYA-22)**: `providers.<name>.model_aliases` rewrites model IDs at dispatch time — key = canonical ID (what clients send and `/v1/models` lists), value = physical ID (what the upstream receives). Exact match, overrides built-in spec model mappings, applied after routing and before sanitize hooks (including the Anthropic format conversion, which previously would have re-introduced the pre-alias ID) so accounting and catalog stay on the canonical name. Serves the same logical model through gateways/providers that expect different spellings (dotted→dashed slugs) without per-provider code
- **Agent-level `sticky_provider` failover policy (NENYA-18)**: agents can now set `sticky_provider` (`off` | `lenient` | `strict`) to protect upstream prefix caches during failover. `strict` never sweeps to another target (same-target backoff retries and the context-limit summarization retry still run); `lenient` fails over only on explicit 5xx, targets whose circuit was already open, or a summarization retry — 4xx and stream-level failures stay on the pinned target. Repeated provider+model entries in an agent's `models` chain remain the operator-configured same-provider retry mechanism and are deliberately not deduplicated
- **Config load leniency with typo warnings (NENYA-19)**: unknown config fields — a config written for a newer nenya, or a typo in an optional section — never fail startup or SIGHUP reload (lenient JSON decoding, last-known-good retained on any reload error), and a strict secondary decode now emits a warning naming the first unknown field so likely typos surface in the log instead of being silently dropped
- **Graceful shutdown wiring (NENYA-53)**: SIGINT/SIGTERM now runs the full teardown sequence — HTTP drain (rejecting new requests with `503` + `Retry-After`), a bounded wait for in-flight MCP auto-saves, gateway cleanup (quota fetcher, MCP transports, response-cache evicter, secure memory), and unloading of pinned local-engine models (`keep_alive=0`) so they no longer stay resident in Ollama after exit. The process exits non-zero when the drain or gateway cleanup times out instead of reporting success; the server-failure path performs the same best-effort cleanup- **Per-provider rate-limit disable + built-in zai-coding-plan defaults**: provider-level `ratelimit_max_rpm`/`ratelimit_max_tpm` now accept an explicit `0` to disable that dimension for the provider (previously `0` meant "inherit global", making per-provider opt-out impossible). Values resolve per dimension: explicit config value → built-in default → governance global. The built-in `zai-coding-plan` entry ships with both dimensions disabled — the GLM Coding Plan enforces per-model concurrency and a quota, not RPM/TPM, so the global defaults (15 RPM / 250k TPM) only caused spurious `target skipped: rate limit exceeded` local skips on that provider. The `zai` standard variant still inherits the global limits, giving the two variants a matching-default split for their different enforcement models
- **Per-model concurrency admission control**: providers that rate-limit by concurrent in-flight requests per model (e.g. the Z.AI GLM Coding Plan) can now be capped via `providers.<name>.max_concurrent_requests` and `providers.<name>.model_concurrency` (per-model overrides), with a global `governance.max_concurrent_requests` fallback. Excess requests queue context-aware until a slot frees instead of colliding with the upstream cap; the slot is held for the full SSE stream/response lifetime. ZAI error `1302` (concurrency) is now classified `concurrency_limited` — no provider cooldown, no circuit-breaker failure, short ~200ms retry — while `1303` (frequency) keeps rate-limit semantics. Rate-limit RPM/TPM buckets are now keyed by provider name, fixing `zai`/`zai-coding-plan` silently sharing one `api.z.ai` bucket. Metrics: `nenya_concurrency_inflight{provider,model}`, `nenya_concurrency_wait_seconds`, `nenya_concurrency_rejected_total`, `nenya_concurrency_limited_total`
- **Synthesized `x-opencode-session` for session-unaware clients**: when the client does not send the header, Nenya now generates a stable per-conversation session ID (`nenya-<hash16>`, derived from agent + system prompt + first user message — the same identity as sticky routing) on both chat routes, giving session-unaware clients provider-side routing/prompt-cache affinity and satisfying upstreams that require the header

### Changed
- **Static model catalog refresh** (sources: LiteLLM catalog, models.dev, router-for-me snapshot, 2026-09-16): repriced the Gemini 3.x flash line and 3.1-pro, fixed shared 2.5-flash/flash-lite pricing, corrected Anthropic limits (sonnet-4-6 → 1M context, 3-7-sonnet → 200k/64k, 3-5-sonnet → 8k output cap) and opus-4-8 pricing, corrected DeepSeek v4 pro/flash pricing, and raised Groq llama-3.3-70b output to 32k; added gemini-3.1-flash-lite, gemini-3.5-flash-lite, gemini-3.6/3.7/3.8-flash, gemini-flash-latest/flash-lite-latest/pro-latest aliases, claude-opus-5, claude-fable-5-1, claude-mythos-5/5-1, deepseek-v4.1-flash, glm-5-code, grok-4.1-fast/4.5/4.6/composer-2.5-fast, and the current Groq catalog (llama-3.1-8b-instant, llama-4 maverick/scout, kimi-k2-instruct-0905, gpt-oss-120b/20b, qwen3-32b/3.6-27b/3.8-27b); removed the retired mixtral-8x7b-32768 from Groq

### Fixed
- **Retryable failures with no remaining target are retried in place instead of exhausting**: the chat retry loop is a forward-only sweep over the agent's target list, so a retryable error (5xx/429, adapter-classified retryable, retryable 4xx, network error, empty stream, context-limit summarization, or param-strip) on the last remaining target applied backoff and then returned `503` — `max_retries` never re-attempted the same target, and the log mislabeled the failure `non-retryable upstream error` because the retryable-4xx branches were gated on `len(targets) > 1`. The loop now re-attempts the target at the final sweep index when the failure would otherwise end the request (budget: agent `max_retries`, else `governance.max_retry_attempts`, default `3`; `N` = `N` retries beyond the initial attempt), keeping the forward failover sweep when a later target exists; the MCP buffered sweep (`forwardBuffered`) shares the semantics. The `non-retryable` log is now emitted only when every classifier returned permanent, and an exhausted retryable 4xx is relayed with its real upstream status instead of a generic 503
- **Aggregator-relayed opaque 4xx bodies classified retryable**: `governance.retry_opaque_4xx` (default `true`) treats a `400`/`422` response whose body is a JSON object **without** a recognized error-envelope key (`error`/`errors`/`detail`/`details`/`message`/`type`/`code`/`title`/`reason`/`status`) as retryable — the shape gateways produce when relaying an upstream failure (e.g. `{"model":"deepseek-v4.1-flash"}`) instead of wrapping it. Previously such bodies surfaced to clients as hard client errors with no retry. `413` is excluded (the payload is immutable across an identical retry) and enveloped client errors stay non-retryable. Opaque failures count on the circuit-breaker request counters but never toward its failure threshold, so a client provoking an ambiguous body cannot bench a healthy provider; because the signal is only a heuristic, a multi-target retry may re-send the request to another provider, so set the flag `false` to opt out
- **Quota polling silently died 60 seconds after startup (NENYA-53)**: `gateway.New` received the 60s startup context and handed it to the quota fetcher's poll loops, so all provider quota polling stopped one minute in while the process kept running. The fetcher now runs on a service-lifetime context (`context.WithoutCancel`) and is stopped only by the shutdown/reload path
- **Big-context sessions permanently exhausted by the TPM guard (NENYA-70)**: a request whose token estimate met or exceeded the provider's entire `ratelimit_max_tpm` capacity could never be dispatched — the token bucket refills to at most the limit, so the guard rejected it on every target forever (`all upstream targets exhausted` with `attempts: 0`) while normal sessions kept working. Oversized requests are now admitted and drain the bucket (RPM still applies), the token estimate is refreshed after the interceptor chain/trim so guards judge the dispatched payload, rate-limit skips now log the dimension/limit/remaining-bucket/token-count, the TF-IDF interceptor reports its post-prune token count, and the exhaustion error includes the token count
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
