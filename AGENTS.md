# Nenya - AI Agent Instructions

## Project Overview

Nenya is a lightweight, highly secure AI API Gateway/Proxy written in Go. It acts as a transparent middleware between local AI coding clients (like OpenCode/Aider) and upstream LLM providers (Anthropic, Google Gemini, DeepSeek, Mistral, xAI, OpenRouter, and many more).

Its primary superpower is the **"Bouncer" mechanism**: intercepting massive HTTP payloads, routing them to a local Ollama instance (`qwen2.5-coder`) for summarization and PII/credential redaction, and forwarding the sanitized, much smaller payload to the upstream cloud AI using Server-Sent Events (SSE) streaming.

## Agent Role & Persona

You are acting as a **Senior Go Security Engineer and Network Architect**. Your code must be production-ready, highly performant, and paranoid about security and memory leaks.

## Strict Engineering Guidelines

### 1. Language & Communication

- **English Only:** All code, variables, functions, comments, commit messages, and documentation MUST be written in English.
- **No Yapping:** When generating code, output only the requested changes or files. Keep explanations brief and technical.

### 2. Go Architecture & OOP Patterns

- Follow Object-Oriented patterns via Go structs and receiver methods.
- **No Global Variables:** Encapsulate state inside structs (e.g., `NenyaGateway` holding the `Config` and `http.Client`).
- Use Dependency Injection where appropriate.
- Keep the `main.go` clean; delegate business logic to receiver methods.

### 3. Dependency Policy & Tech Stack

- The project relies exclusively on the Go Standard Library (`net/http`, `encoding/json`, `io`, `bytes`, `regexp`, `sort`).
- **Zero external dependencies.** DO NOT import any third-party packages without explicit human authorization.

### 4. Hardcore Security Rules (CRITICAL)

- **Timeouts:** NEVER use the default `http.Client` or `http.ListenAndServe`. Always explicitly define `ReadTimeout`, `WriteTimeout`, `IdleTimeout`, and Client `Timeout` to prevent resource exhaustion and hanging connections. The listener's `WriteTimeout` stays 0 for streaming; the per-write `server.stream_stall_timeout_seconds` deadline (NENYA-155, re-armed via `http.ResponseController` before every write/flush) bounds stuck readers instead — a breach is classified `client_stalled` and never charged to provider health.
- **Body Limits:** Always wrap incoming requests with `http.MaxBytesReader` to prevent memory exhaustion attacks (DoS) from massive payloads.
- **Header Sanitization:** When proxying requests, strip hop-by-hop headers (like `Connection`, `Content-Length`) to prevent HTTP desync attacks. Pass only necessary headers (e.g., `Authorization`).
- **Error Handling:** Never expose internal stack traces to the HTTP response. Log errors internally and return standard HTTP status codes.

### 5. Context Package Standards (CRITICAL)

- **Request Context:** Always use `r.Context()` from `http.Request` as the root context for request-scoped operations. Thread it through the entire call stack.
- **Timeout Enforcement:** Apply appropriate timeouts to all outbound calls:
  - Upstream requests: Use `provider.TimeoutSeconds` from config
  - MCP operations: Use configured timeouts (30s for tool calls, 10s for auto-search)
  - Health checks: Use short timeouts (5s)
  - Long-running loops: Use overall deadlines (5min for MCP multi-turn loop)
- **Context Propagation:** Functions doing I/O or long-running work MUST accept `context.Context` as their first parameter. Never create a new `context.Background()` mid-request unless the operation is fire-and-forget and must outlive the request.
- **Goroutine Lifecycle:** All goroutines must respect cancellation:
  - Request-scoped goroutines: Use the request context or a derived context
  - Background workers: Use dedicated cancellation channels (`closeCh`, `stopCh`)
  - Fire-and-forget operations: Use `context.Background()` with explicit timeout
- **Loop Cancellation:** Long-running loops MUST check for cancellation:
  - Use `select { case <-ctx.Done(): }` for blocking operations
  - Check `ctx.Err()` in loop bodies for non-blocking operations
  - Never have infinite loops without cancellation checks
- **HTTP Requests:** Always use `http.NewRequestWithContext(ctx, ...)` — never `http.NewRequest` (deprecated)
- **I/O Operations:** Use context-aware I/O functions (e.g., `copyStream(ctx, dst, src, buf)`) instead of bare `io.Copy`
- **Anti-Patterns to Avoid:**
  - `context.TODO()` — never use, always have a clear parent
  - `context.WithValue()` — avoid for request-scoped data; use struct fields or parameters
  - `context.Background()` mid-request — only use for top-level contexts or fire-and-forget goroutines
  - Ignoring `ctx.Done()` — always respect cancellation signals

### 6. Core Workflows to Maintain

- **Idempotency-Key Forwarding (NENYA-165):** A provider declaring `providers.<name>.idempotency_header` (built-in: `anthropic` → `Idempotency-Key`) receives an idempotency key on chat dispatch. The key is bound to the dispatched BODY (`internal/proxy/idempotency.go`: `idempotencyKeyForBody` keeps the binding on the inbound header map, `injectIdempotencyKey` sets the declared header): the client's `Idempotency-Key` (trimmed, visible ASCII, ≤255 chars) is forwarded for the request's first body, else a synthesized `nenya-<hash>` (body digest + per-request nonce). Every attempt that re-sends an identical body — retry-in-place on a retryable failure, failover between accounts of the same provider — presents the SAME key, which is the double-billing case; a re-dispatch of a MODIFIED body (MCP multi-turn iteration, stream continuation, corrective param-strip/summarization retry) is re-keyed, because a provider rejects a repeated key whose payload differs. Admission calls `resetIdempotencyBinding` so a client cannot forge the internal binding. Scope: every dispatch through the shared dispatch builder (chat routes and passthrough endpoints); the engine/judgment dispatches carry no key. The header name is validated at load (token, canonical spelling, not a gateway-owned header).
- **Provider Registry:** Upstream providers are config-driven via `"providers"` JSON sections merged with built-in defaults (`builtInProviders()` in `config.go`, sourced from `ProviderRegistry` in `registry.go`). Adding a new provider (e.g., OpenAI) requires zero Go code changes — only JSON config and a secrets key. Model resolution uses `ResolveProviders()` which checks the dynamic discovery catalog first (returning ALL providers offering that model), then falls back to the static `ModelRegistry`. Unknown models return a 400 error — there is no default provider fallback.
- **Dynamic Model Discovery:** At startup and on SIGHUP reload, Nenya fetches `/v1/models` from each configured provider (concurrently, concurrency-limited to 5 by default via `HealthCheckConfig.MaxConcurrent`, 10s timeout each). Responses are parsed by provider-specific parsers (`internal/discovery/parse.go`) and merged with the static ModelRegistry using three-tier priority: config overrides > discovered models > static registry. The merged catalog (`ModelCatalog` in `internal/discovery/discovery.go`) is used for model resolution in routing, `/v1/models` endpoint, and `max_tokens` injection. Discovery failures degrade gracefully — providers that fail are skipped and the static registry is used as fallback.
- **Policy Refusal Failover:** `internal/stream/refusal.go` classifies policy refusal/safeguard terminals (`IsRefusalTerminal`: OpenAI `finish_reason`/`delta.stop_reason`, Anthropic `message_delta.stop_reason`, Gemini `finishReason`) and policy-worded rejection payloads (`IsPolicyRefusalPayload` over the same phrase list as `error_kind=policy_refusal`). The bootstrap head probe fails a pre-content refusal over to the next target WITHOUT charging cooldown/breaker (`resolveBootstrapRefusal`/`failoverHeadPolicy`); post-content refusals are terminal (the content was delivered, never re-dispatched, never cached — NENYA-27); the /v1/messages route shares the chat failover taxonomy. See `docs/ARCHITECTURE.md` §Policy Refusal Failover.
- **Dynamic Routing:** The proxy must inspect the JSON body, read the `"model"` string, and dynamically route to the correct provider via `resolveProvider()`. Agents with fallback chains are resolved via `buildTargetList()`. Agent model lists support string shorthand (looked up from `ModelRegistry`), full object notation (explicit provider/model/format), or regex-based patterns (`provider_rgx`/`model_rgx` inline on any model entry) that dynamically match against the discovery catalog at runtime.
- **The Ollama Interceptor:** If the `messages[-1].content` exceeds the context soft limit, the proxy must synchronously call the local Ollama API to summarize the text BEFORE forwarding the request upstream. The engine is configured via `bouncer.engine` which supports a string (agent name reference with fallback chain), a shorthand (`provider/model`), or an inline object (direct provider/model). See `config/engine_resolve.go` for resolution logic and `internal/pipeline/engine.go` `CallEngineChain` for the fallback chain implementation. When `governance.tfidf_query_source` is set, Tier 3 uses TF-IDF relevance scoring (`internal/pipeline/tfidf.go`) to prune content blocks by relevance to the user's query terms, potentially eliminating the engine call entirely if the payload drops below the soft limit.
- **Transparent Streaming:** The proxy must flawlessly pipe the upstream SSE (Server-Sent Events) stream to the client, applying provider-specific response transformations (e.g. Gemini tool_calls normalization, Anthropic OpenAI↔Anthropic format conversion) as needed via the adapter system.
- **Endpoints:** The gateway exposes `/v1/chat/completions` (streaming with Ollama interception), `/v1/messages` (Anthropic Messages API with bidirectional format conversion), `/v1/models` (model catalog), `/v1/embeddings` (passthrough proxy), `/v1/responses` (passthrough proxy), `/v1/images/generations` (Image generation), `/v1/audio/transcriptions` (Audio transcription, multipart), `/v1/audio/speech` (Text-to-speech), `/v1/moderations` (Content moderation), `/v1/rerank` (Re-ranking), `/v1/a2a` (Agent-to-Agent protocol), `/v1/files` (File upload/retrieval/delete), `/v1/batches` (Batch operations), `/proxy/` (generic passthrough), `/healthz` (engine health probe, no auth), `/statsz` (per-model token usage counters + circuit breaker state, no auth), and `/metrics` (Prometheus-compatible metrics, no auth). All `/v1/*` endpoints require `Authorization: Bearer <client_token>`. API keys additionally enforce RBAC (agent scoping, endpoint allowlists, roles — see §16).
- **Cache-State-Aware Reorder Tie-Break (NENYA-190):** `infra.CacheStateTracker` records observed cache warmth per (model, provider) pair at the usage-recording sites (`recordStreamUsageDimensions`/`recordNonStreamingUsage`: hit tokens over total input tokens — Prompt IS the total, NENYA-167 — plus last-hit age; keys cloned per NENYA-198; BYOK dispatches stand down per NENYA-195); exported in `/statsz` `cache_state.<model>:<provider>` (both output forms, entries embed model/provider). `governance.auto_reorder_prefer_warm_cache` (off by default, requires `auto_reorder_by_latency`) breaks latency NEAR-TIES in `LatencyTracker.SortByLatencyWithWarmth`/`routing.SortTargetsByLatencyWithWarmth`: when two RAW medians are within the ±5% jitter band the warm target (hit ratio ≥ 0.5, last hit ≤ 10 min) orders first — a cold-cache switch re-ingests the prefix at first-turn rates; outside the band latency stays primary.
- **Empty-200 Counter & Advertised Modalities (NENYA-160):** `nenya_empty_responses_total{model, provider, kind}` (kind `stream`|`buffered`, model label cardinality-guarded) counts upstream HTTP-200 responses with no content — the streaming signal rides the SSE reader's own empty-stream handler via `SetEmptyStreamCallback` (flag-independent; the probe-site counter remains alongside the legacy flag-gated `RecordEmptyStream`) plus both buffered sites (zero-length body in `handleNonStreamingResponse`; zero-choices via `countEmptyChoices`) — the always-on degradation signal, independent of `empty_stream_as_error`. The OpenAI discovery parser merges aggregator-advertised `architecture.input_modalities`/`output_modalities` + `context_length` (`metadataFromAdvertised`) into `ModelMetadata` (vision/audio, case-insensitive, unknown spellings ignored) and `DiscoveredModel.MaxContext`; the resolver consults the structured data before the `InferCapabilities` ID heuristics, and a modality-less entry returns nil metadata so the heuristics are never masked by an empty record.
- **MCP Dead-Server Surfacing (NENYA-152):** `gateway.buildMCPToolIndex` records each server's discovery outcome in a per-gateway registry (`SetMCPServerStatus`, closed set ok/discovering/failed, `MCPServerStatusSnapshot`) — surfaced in `/statsz` via `proxy.mcpSection` (`status`, `last_error`, `status_since` merged into the per-server entry; a status without a client entry still surfaces) and counted per attempt in `nenya_mcp_server_status_total{server, status}`; discovery failures WARN explicitly ("MCP server unreachable, skipping tool discovery") instead of a bare skip. Statuses live on the gateway, so a reload rebuilds them.
- **/statsz Formats & Label Cardinality Guard (NENYA-144):** `/statsz` negotiates `?format=json` (default, historical payload) or `?format=text` (the same snapshot as sorted `dotted.path: value` lines via `handleStatsText`/`renderStatsText`; path segments and values JSON-escaped so a hostile model id cannot line-forge; unknown format = structured 400; auth unchanged; pagination N/A — the snapshot is bounded). `infra.ModelLabelGuard` (labels.go) bounds caller-controlled model labels — passthrough endpoints forward model strings only the upstream validates: non-plausible ids (>64 chars or outside the model-id grammar) collapse to `_invalid_`, new distinct ids past a 512-series budget collapse to `_overflow_`; applied in `Metrics.ModelLabel` (feeding `nenya_tokens_*` and `nenya_cost_micro_usd_total` labels, violations counted `nenya_model_label_violations_total{kind}`) and `UsageTracker` (per-model `/statsz` keys, violations surfaced as `label_violations` in its Snapshot).
- **Capability-Gated Image Blocks (NENYA-149):** image blocks are gated on the routed target before dispatch: `routing/vision.go` (`FormatAcceptsImageMedia` per-format media tables, `SupportsVision`, `DropUnsupportedVisionContent` wired into `SanitizePayload(deps, payload, model, format)` — now format-aware) strips request-message image parts by SHAPE (`image_url`/`input_image`/`image`+`source`): a vision-less model loses every image, Anthropic-format targets lose ALL message-content images (`format_shape` — the adapter copies them verbatim and hard-400s; only tool-result content is converted), remote URLs (`source.type=="url"` or non-`data:` `image_url`) are dropped where the converter only carries `data:`, and media types outside the format table are dropped individually; an emptied array gets a visible `[image omitted: ...]` text part (never a bare empty array). MCP tool-result images (`mcp.ContentBlock.Data`/`MimeType`, `Images()`) ride as OpenAI `image_url` data URLs ONLY through `proxy/mcp_vision.go`'s `mcpVisionGate` (primary target's model + format; zero gate = fail-closed `no_target_context` drop; 4 MiB/image cap; malformed blocks counted as `malformed_image`); a result where no image survives keeps the string shape. Drops count `nenya_vision_content_dropped_total{site, reason}` + Debug log; spotlight envelopes only text parts of a content-array tool result (counter fires only when something was enveloped). Gate covers the chat dispatch builder; `/v1/responses`, `/v1/embeddings`, `/proxy/*` remain ungated (pre-existing).
- **MCP Tool-Surface Policy & Opt-in Tool Selection (NENYA-162):** `mcp_servers.<name>.tool_include`/`tool_exclude` globs (`*`/`?`, exclude wins; `proxy/serverToolPolicy` + the `filterServerToolPolicy` wrapper, applied after the key grant) filter the injected set; `agents.<name>.mcp.tool_search > 0` enables relevance-based selection (`proxy/selectToolsForRequest`: terms 1.5× name + 1.0× description weight × log IDF over the catalog, deterministic score-desc/name-asc, top-N (clamped to the catalog size) + `tool_search_floor` always included; no terms → the full catalog = "selected_full_catalog"; off = pass-through), so a big catalog injects few declarations and identical conversations see identical sets. Loop correctness: `unavailableMCPToolCalls` (the loop's injected set, threaded from the GATEWAY's own injection bookkeeping — `chatRequest.injectedMCPTools` → `forwardOptions.InjectedMCPTools`, never re-derived from the client-writable payload) + `executeMCPCallsWithUnavailable` refuse a non-injected MCP-named call with a structured `[unavailable tool]` IsError result — counted `nenya_mcp_tool_search_total{outcome="noninjected_call_refused"}` — never a silent drop.
- **Secret References & Reload Continuity (NENYA-166):** `provider_keys` values may be `env:NAME`/`file:PATH` references (`config/secrets_refs.go`, `IsSecretReference`), resolved at load and re-resolved on every SIGHUP (`config.LoadSecretsBounded(root, timeout)`, timeout = `governance.secret_resolution_timeout_seconds`, default 2s cap 60; file reads run under a deadline — a blocking FIFO takes the timeout instead of wedging boot/reload/shutdown, documented fire-and-forget goroutine). Fail closed: missing/empty env, missing/empty file, timeout, traversal past the config root, and a RESOLVED value failing strength validation all refuse the whole load; the raw reference value is never strength-checked before resolution. Reload guard: `config.ServeContinuityIssues(oldResolved, newResolved, stillConfigured)` treats a previously-serving provider (had a credential, its auth style needs one) as LOST only when it is still configured but is now absent or credential-less in the new resolved set — a deliberately removed provider reloads normally — and `cmd/nenya/main.go`'s `reloadConfig` rejects the whole reload (metric `nenya_reload_rejected_total{reason="provider_continuity"}`, last-known-good gateway keeps serving) BEFORE `Gateway.Reload` closes the old one.
- **Bring-Your-Own-Key Per-Request Credentials (NENYA-195):** `governance.byok` (default off) lets a caller fund its own traffic: a credential in `governance.byok.header` (default `X-Nenya-Upstream-Key`) replaces the shared provider key for eligible targets. Four fail-closed gates: the global switch; the `byok` key permission (`auth.KeyMayBYOK` — admin implicit, others via `permissions.byok: true`; denial is a 403 + `nenya_auth_denials_total{reason="byok"}`); `providers.<name>.allow_byok` (the target list narrows to opt-in providers; none left is 422 `unsupported_combination`, never a silent fallback); and header-only, canonical-name delivery (`config/validate.go:validateBYOKConfig` rejects non-token/non-canonical names and `Authorization`; a malformed value is 400). `proxy/applyBYOKCredential` sets the credential and `UpstreamTarget.BYOK`, clears the response-cache key, and audits by provider + key name only (`nenya_byok_requests_total{provider}`, value never logged or labelled). A BYOK target writes no shared-account state: `AgentState.ActivateCooldown`/`RecordFailure`/`RecordFailureWithStatus`, the two success sites, `pinSessionTarget`, the billing/spend writes (`recordCostAndBilling`/`recordStreamSpend`), the quota-header marks (`extractQuotaUnlessBYOK`), the provider-budget draw (`providerBudgetGate`), the breaker gate (`breakerGate` — so no half-open probe is consumed), the pre-dispatch agent-budget and max-cost guards (`checkPreDispatchGuards`, NENYA-208: a caller-funded dispatch draws no shared money, while the rate limiter correctly stays — it is gateway capacity, not money) all stand down on `BYOK`; `filterExhaustedTargets` exempts `allow_byok` providers when `req.BYOKRequested`, `applyStickyRouting` returns the built order untouched (intent resolved BEFORE routing, so the pin is never read or written), and the intent is logged when the header is present but the switch is off. The intent-before-routing order is pinned by `TestBYOKIntentPrecedesStickyReorder` (NENYA-208), which drives the real `validateChatRequest` sequence and is mutation-checked (reordering the intent after routing flips the pin reorder into a BYOK request). Documented exceptions: the target build still runs the shared account selector for ordering (nothing is charged), and the session-identity affinity store is updated as for any request.
- **Automatic Prompt-Cache Anchoring (NENYA-185):** `agents.<name>.auto_cache_breakpoints` (default off) makes the gateway anchor an Anthropic prompt cache when the client sent no marks: `routing.applyAutoCacheBreakpoints` injects the gateway's own system + last-stable-history breakpoints (reusing the NENYA-171 machinery and its four-marker cap) only when the agent opted in, the deep client-mark scan finds NO `cache_control` anywhere (including `extra_body`/`extra_content`), the request has prior history, `prefix_cache.enabled` is true, and the content pipeline did not window-compact the history (`TransformDeps.WindowCompacted`, threaded from the content pipeline). Every outcome is counted in `nenya_auto_cache_breakpoints_total{outcome}`; an opted-out agent is silent. NENYA-207 follow-ups: the Anthropic→OpenAI reverse conversion HOISTS a flattened block-level `cache_control` onto the message (string/parts flattening would otherwise destroy the client's placement and the stand-down scan would let the gateway anchor over it while recording `injected`; a client message-level mark wins over a hoisted one), and the adapter's message-marker placement (`injectMessageCache`) anchors the last CLIENT-STABLE message AHEAD of the canary trailer when one is present — on MCP loop iterations the array continues past the request-varying canary with gateway-produced tool traffic, and the old last-but-one rule would anchor a prefix containing the random canary token (missing the cache every later request).
- **Hot-Path Allocation Discipline (NENYA-177):** read-only stages never clone the request: the interceptor chain mutates the caller's message maps in place (pinned by an allocation-ceiling test over a 40-message fixture plus an in-place test in `internal/pipeline`), and the SSE transform path reuses pooled line buffers (`internal/stream/pool_test.go`). Fan-out conversion reuse (one conversion for N accounts of a provider) is NOT shipped: a converted-body cache is only sound when its key covers everything the transform reads (payload content, cache salt, session identity, provider hooks, per-target max-output clamp and trim) and every payload mutation invalidates it — the mutation sites (MCP per-iteration tool-result append, stream continuation, parameter-strip retry) are easy to miss and a miss re-sends a stale body. The sound design keys on a payload digest; deferred deliberately.
- **Trace Parentage & Usage Attributes (NENYA-187):** the request span ADOPTS the client's inbound W3C `traceparent` (continued, propagated upstream, echoed on the response); gateway-initiated work ROOTS its own gateway trace and LINKS the client's context (`tracing.StartGatewaySpan`, span records carrying `linked_trace_id`/`linked_span_id`) — the MCP multi-turn loop (`upstream.mcp_loop`) and engine/judgment calls (`engine.call`) — so gateway-rooted spans always use gateway-generated ids (a client-supplied traceparent is never adopted as a gateway span's parent) and link the current context for correlation. The Info-level `request trace` summary carries GenAI-semconv usage attributes (`gen_ai.system`, `gen_ai.request.model`, `gen_ai.usage.input_tokens`/`output_tokens`, plus `cached_tokens`/`cache_creation_input_tokens`/`reasoning_tokens` when non-zero) built from the normalized usage totals, so provider cache spellings fold into one attribute set on streaming and buffered paths alike (naming only — spans stay log records, no SDK).
- **Target-Aware Request Normalization (NENYA-179):** a request mutation that both the client's wire shape and the target's typing discipline constrain is keyed on the (client format, target executor) pair, never on the client format alone (CLIProxyAPI v8.0.5→v8.0.13 regression class). `param_compat` carries the target axis as an explicit, validated hook: `token_field` (required token-cap spelling; empty = canonical `max_tokens`) and `int_typed_params` (parameters that must be integer-typed JSON numbers). No built-in rename ships — a model-ID prefix is not evidence about the executor behind it (an aggregator relaying a `gpt-5*` model may accept only `max_tokens`), so the axis is operator-declared and `config/validate.go` rejects a typo that would rename the only cap to a junk key. When declared, `routing.normalizeTargetParams` moves a cap onto the required field (logged), keeps the smaller cap when both spellings arrive, coerces a JSON-decoded float cap to an integer (`nenya_param_coercions_total{param,client_format}`) and leaves tolerant targets untouched; `TransformDeps.SourceFormat` is the client axis and is recorded (metric/log), not decisive: the surviving spelling is decided by the target rule alone. Cap plumbing is pair-safe: the Anthropic conversion and `util.EffectiveOutputTokens` read BOTH spellings, so a cap can never be lost or escape the output reservation just because of its name. Stage audit: model resolution, provider sanitize hooks, param-compat, cache injection, prompt hoisting and the max-output clamp are target-driven; the harness normalizer is pair-driven; protocol repairs are target-agnostic. Pair semantics documented next to `param_compat` in docs/CONFIGURATION.md.
- **Harness Payload Normalization (NENYA-186):** coding-agent harness quirks are neutralized loudly, never as silent capability loss. (1) Client scaffolding — a `system`/`user` message whose whole plain-string content is one or more well-formed harness envelopes (`<system-reminder>`, `<env>`, `<command-*>`, `<additional_tools>`, or the gateway's exact canary trailer; `pipeline.ClassifyHarnessEnvelope`) AND whose detections are all imperative-wording categories (never hidden-text/encoded/custom carriers) — is scanned for visibility but excluded from the injection action AND the tier-2 escalation excerpts (`InjectionInterceptor.harnessExemption`): the harness's imperative wording is not the user's, so it is never sanitized, never rejected, and never judged as user content (LiteLLM #40599/#40655 class). The boundary is narrow so it cannot bypass the scanner: tool-role/`function` output, assistant turns, multimodal arrays, unterminated or attribute-bearing tags, spotlight-wrapped content, envelopes mixed with real user text, and tool-call arguments (always scanned and rewritten, even on an exempted message) all stay actionable; every exemption taken is counted (`nenya_harness_envelopes_total{kind}` + injection `cleared`/`harness`). (2) `routing.normalizeDuplicateParams` resolves conflicting duplicate spellings of one knob to the canonical field — `max_tokens`/`max_completion_tokens` keep the SMALLER cap (client budget and model clamp both respected) under the canonical spelling — or the spelling a `param_compat` rule declares for the target — and `stop`/`stop_sequences` keep the canonical; null aliases and agreeing duplicates collapse silently, conflicts are counted (`nenya_duplicate_params_total{param}`) and logged. (3) `routing.auditRequestFields` reports top-level fields no format path consumes (`nenya_unknown_request_fields_total{format}`; field names go to a capped Warn log, never metric labels).
- **Unsendable-Content Reporting & Opaque-Blob Fidelity (NENYA-176):** Nenya's Anthropic↔OpenAI translations never drop content they cannot represent silently. Thinking blocks surface as `reasoning_content` on the OpenAI shape (the opaque encrypted `signature` blob has no OpenAI-shape home and is recorded), `redacted_thinking` and foreign/unknown block types (`document`, web-search tool results, anything unmapped) are recorded by kind on a per-translation `adapter.DropReport` (`internal/adapter/drops.go`), and the OpenAI adapter's `flattenContentArrays` marks image and attachment parts with a visible `[unsendable content: <type>]` marker instead of stripping them for models whose dialect lacks content arrays. Each report surfaces as a Warn structured log, a counter in `nenya_unsendable_content_total{kind}` (counts translations reporting, kinds deduped per translation and collapsed to a closed label set), and the client-visible `X-Nenya-Dropped-Content` response header (comma-joined kinds, merged across translations of the same request; buffered reverse-conversion site + the Anthropic native-request parse). Reports are annotations of fidelity loss surfaced, never hidden — they never fail the request. Streaming caveat: the Anthropic SSE transformer surfaces thinking deltas as plain `content` text (no marker, a reason/answer-text distinction is only available on the buffered path), and the reverse OpenAI→Anthropic streaming transformer does not surface `reasoning_content` deltas at all (the buffered path does, as `[reasoning]` text) — both are recorded limitations, not silent drops of Anthropic `thinking` source content.
- **Token Estimation (NENYA-157):** `NenyaGateway.CountRequestTokens` (the seed for per-target input budgets, trimming, and pipeline savings) composes message text (cl100k BPE, image placeholders excluded), 4 tokens of framing per message, the serialized tool/`response_format` declarations (+8 framing each), and each image by pixel area (`tiktoken.ImageTokens`: Anthropic's `(w × h)/750` read from the PNG/JPEG/GIF/WebP header inside the inlined base64 through a bounded 64 KiB probe — PNG IHDR, JPEG frame header (including one behind up to ~48 KiB of EXIF, beyond which the default applies), GIF screen descriptor, WebP VP8X/VP8L and simple-lossy `VP8 ` — clamped to [85, 1590], default 1105 when unmeasurable — never zero). A failed BPE initialization falls back to `tiktoken.EstimateByClass` (weights per run of one class: letter/digit ~5 chars/token unless high-entropy — a letter/digit mix, a pure number, or a >16-char run with fewer than one vowel per four — which is ~2, or ~3 for a long pure number; punctuation ~3; CJK/kana/hangul 1/char; other non-ASCII 1/2; emoji 3 + 1/modifier; space runs 1/20 with single word-separating spaces absorbed; newline 1 — the exact table is in `internal/tiktoken/doc.go`), which is calibrated against the real encoder (over-estimates indentation/prose/code/JSON; can still under-estimate dense punctuation and some CJK — a safety net, not the budget source of record). The residual cl100k drift is corrected per model by the NENYA-135 calibration loop, which records the same composed measure (an inlined base64 image cannot drive the learned ratio to its clamp).
- **Token Usage Tracking:** `infra/usage_tracker.go` implements `UsageTracker` with atomic per-model counters (requests, input_tokens, output_tokens, errors). Input tokens are counted at dispatch; output tokens are extracted from SSE `usage` fields via `stream.SSETransformingReader.OnUsage` callback. Pipeline savings are exported as `nenya_pipeline_tokens_saved_total{source}` (`Metrics.RecordTokensSaved`, wired at trim/window/bouncer sites where deltas are already computed); `nenya_tokens_estimated_total{direction="client"}` records the pre-pipeline estimate at request build (proxy/chat.go) and `direction="input"` is recorded post-transform (proxy/retry.go) so client − input is derivable in Prometheus.
- **Latency Tracking:** `infra/latency.go` implements `LatencyTracker`, which maintains per-(model, provider) sorted sample buffers (incremental binary-search insertion, O(n) per record) for two dimensions: the upstream response-header latency (`Record`) and the time-to-first-effective-output of streaming requests (`RecordTTFT`, NENYA-178, exposed by `TTFT(model, provider)`). The TTFT clock stops at the first frame `stream.HasEffectiveOutput` accepts (reasoning-only frames only when `governance.ttft_include_reasoning` is set; a continuation re-dispatch and buffered requests are not measured), and is exported as the `nenya_ttft_seconds` histogram and the `/statsz` `latency` section; the slow-call circuit breaker (NENYA-153) consumes the same per-request first-output measurement, while `TTFT(model, provider)` feeds latency-aware routing. When `governance.auto_reorder_by_latency` is enabled, `LatencyTracker.SortByLatency` reorders targets by median latency with ±5% jitter to prevent thundering herd. The jitter function is injectable for deterministic testing.
- **Structured Logging:** All logging uses `slog` with structured key-value pairs. The logger auto-detects TTY (text) vs systemd (JSON) format. Debug-level logs are gated behind `g.logger.Enabled(ctx, slog.LevelDebug)`.
- **Provider Adapter Pattern:** Provider-specific wire format differences (auth injection, request/response mutation, error classification) are handled by `internal/adapter/` via the `ProviderAdapter` interface. See [`docs/ADAPTERS.md`](docs/ADAPTERS.md) for full details.
- **Circuit Breaker:** Each agent+provider+model combination has an independent circuit breaker (Closed/Open/HalfOpen states) implemented in `internal/resilience/`. Failure thresholds, success thresholds, and max_retries are configured per-agent. **Slow calls count as failures (NENYA-153):** with `governance.slow_call_threshold_seconds` (per-agent override `agents.<name>.slow_call_threshold_seconds`), a call slower than the threshold is charged with `RecordFailure` instead of `RecordSuccess` — the client still receives the answer, only the breaker sees it. The clock is the time-to-first-effective-output on streaming paths (NENYA-178) and the whole call on buffered paths, measured once per target — the target's first attempt; a continuation re-dispatch of the same target is not re-measured, while a failover to another target is measured on its own; the served buffered paths also record a success so a half-open circuit can recover. `internal/proxy/slowcall.go` owns the decision; a slow call never closes a half-open circuit. See [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md#circuit-breaker) for the state machine.
- **Provider Spec System:** `internal/providers/` defines `ProviderSpec` with `ServiceKinds` (e.g., `ServiceKindLLM`, `ServiceKindEmbedding`, `ServiceKindTTS`, `ServiceKindRerank`) declaring what endpoint types a provider supports, plus per-provider sanitization/response transformer functions. Model-level wire format capabilities (vision, tool_calls, reasoning, stream_options, content_arrays, auto_tool_choice) are inferred dynamically from model IDs via `discovery.InferCapabilities()` and `discovery.Capability` typed constants, stored in `ModelMetadata`. The adapter system builds on top of this.
- **Per-Model Wire Format (format attribute):** Models can declare a `format` field (`"openai"`, `"anthropic"`, `"gemini"`) that controls request body conversion, URL routing, and SSE response transformation independently of the provider. This enables multi-format gateways (e.g., OpenCode Zen) to serve models from different API families through a single provider entry. The `ProviderEntry.FormatURLs` map supplies format-specific endpoint URLs. Format resolution priority: agent config override > catalog/registry. See `internal/routing/transform.go`, `internal/proxy/stream.go`, `internal/routing/targets.go:resolveTargetFormat`.
- **Provider-Specific Thinking Activation:** `ProviderSpec.SanitizeRequest` hooks receive `SanitizeDeps` with `SupportsReasoning` and `ProviderThinking` closure functions (avoiding import cycles with `config`). Zai's `zaiSanitize` injects `thinking: {type: "enabled", clear_thinking: false}` for reasoning-capable models, configurable per-provider via `thinking.enabled` in the config file. Temperature defaults are applied model-specifically (GLM-4.6/4.7 → 1.0). See `internal/providers/zai.go`.
- **DeepSeek v4 Reasoning Injection:** `internal/routing/sanitize.go:ensureDeepSeekReasoningContent` injects `reasoning_content: ""` on all assistant messages for DeepSeek. Required because DeepSeek v4 returns 400 if any assistant message lacks `reasoning_content` in multi-turn conversations where tool calls occurred.
- **Two-tier Injection Escalation:** With `governance.injection.escalation.enabled`, ambiguous detection-score bands (`[min_score, max_score)`) are routed to an LLM classifier through the shared judgment primitive (`internal/pipeline/injection_escalation.go` building on `Judge.Adjudicate` → `CallEngineChainObserved`, contract `"injection"` with pinned envelope/caller labels) before the tier-1 verdict applies. The classifier verdict is advisory: benign clears surfaces, but malformed output, engine failure, timeout, budget exhaustion, and summarized traffic always fall back to the deterministic tier-1 verdict (the LLM layer can never weaken it). Metrics: `nenya_injection_escalations_total{verdict}`, `nenya_injection_escalation_duration_seconds{verdict}`, plus the judgment-family `nenya_judgments_total{judgment="injection",verdict,engine}`.
- **Typed Advisory Judgment Layer:** `internal/pipeline/judge.go` provides the shared, engine-agnostic, fail-closed adjudication primitive: code-owned `JudgmentContract`s (versioned prompt + closed verdict enum) driven through the engine chain, with configuration selecting only the engine and budgets. Judgment sites (`summary_fidelity`, `egress_screen`, `tfidf_rerank`, `spotlight_tier`, `injection`) run only at ambiguous bands and buffered checkpoints — never per SSE delta — and can only **strengthen** a deterministic tier-1 verdict; any operational failure, contract violation, or truncated excerpt falls back to the deterministic decision (strengthen-only invariant). Contracts are never assumed to produce provider-native structured output. See docs/ARCHITECTURE.md (Advisory Judgment Layer) and docs/CONFIGURATION.md (`judgments`/`tfidf_rerank`/`spotlight.risk_tiers`).
- **Time-of-Day & Cached-Input Cost Model:** Model rate cards (`config.PricingOverride` → `discovery.PricingEntry`) carry a standard (off-peak) input/output baseline, an optional peak pair, and an optional cache-read input rate; providers declare UTC peak windows (`providers.<name>.peak_windows`, half-open, `weekdays_only`; DeepSeek declares 01:00–04:00 and 06:00–10:00 Mon–Fri). `CalculateCost(PricingUsage{Input, CachedInput, Output, Peak})` prices cached hits, remaining input, and output with per-dimension peak fallback and clamps. Billing (`internal/proxy/usage.go`/`stream.go`) resolves the window at the recording instant (streaming prices cumulative usage, records the delta); the `max_cost_per_request` guard estimates at peak (worst case); routing uses the standard baseline so it is time-invariant; static registry pricing surfaces through the discovery catalog on every merge path; operators get `nenya_cost_micro_usd_total{model,window}` and the `/statsz` `cost_model` section. See docs/COST_MODEL.md.
- **Advisory Judgment Transport (System One):** Judgment contracts can be adjudicated by System One decision models (TypeSafe Jev, or a local Laya sidecar) instead of chat LLMs: a chain target whose provider declares `api_format: "systemone"` receives the contract as a typed `choice` question (instructions = contract prompt, criteria = verdict enum, state = excerpt capped at 512 tokens ≈ 2048 bytes) and returns a typed answer read structurally — no prose parsing. Typed answers below the per-judgment `escalate_below_confidence` (0 = off) cascade to the next chain target (confidence routes, never decides policy; counted in `nenya_judgment_cascades_total{judgment}`). The deterministic tier-1 verdict still stands whenever every target fails, is inconclusive, or is truncated; strengthen-only semantics are unchanged. See `internal/pipeline/engine_systemone.go` and docs/SYSTEM_ONE.md §4.
- **ExfilGuard (Output Egress Control):** Deterministic URL policy on the response path (`internal/stream/exfil.go`, wired via `internal/proxy/exfil.go`): model-produced markdown images/links and bare http(s) URLs are checked against scheme/IP-literal/host-allowlist/query-length/query-entropy rules on both streaming (sliding-window over SSE deltas, same mechanism as StreamFilter) and buffered paths. Actions: log (default), strip (placeholder), block (`error_kind=exfil_blocked`). Config: `governance.exfil_guard` with per-agent `enabled`/`action` overrides; metrics `nenya_exfil_detections_total{reason,action}`.
- **Egress Canary Tripwire:** With `governance.canary.enabled`, a per-request random token (`NENYA-CANARY-{32 hex}`, crypto/rand) is injected as a system-context marker after the content pipeline and watched on every egress channel (`internal/pipeline/canary.go`, stream watcher in `internal/stream/exfil.go`, tool-args scan in `internal/proxy/mcp_tools.go`). Detection = the model reproduced injected context into output (injection-driven exfil signature): block terminates with `error_kind=exfil_detected` / refuses the tool call; log records only. Metrics: `nenya_exfil_events_total{channel}`. Exact-substring tripwire — encoded dumps evade (see docs/CONFIGURATION.md).
- **MCP Tool Integration:** The gateway supports Model Context Protocol servers via HTTP+SSE transport. When an agent has MCP servers configured, tools are discovered at startup, injected into requests as OpenAI function tools with `tool_choice: "auto"`, and a multi-turn loop executes tool calls between the client and upstream model. When `governance.spotlight.enabled` is set, MCP tool results, auto-search memory context (delimiters-only), and tool descriptions are enveloped/marked as untrusted content (`internal/pipeline/spotlight.go`, `internal/proxy/spotlight.go`); with `governance.spotlight.risk_tiers.enabled`, history tool messages are risk-tiered instead of blanket-enveloped — markers force datamarking (high tier), plain local output keeps delimiters (low), and the ambiguous band goes to the advisory `spotlight_tier` judgment failing closed to high (`internal/pipeline/spotlight_tier.go`); see §16's sibling section in docs/CONFIGURATION.md for the config surface. See `internal/mcp/` and `internal/proxy/mcp_tools.go` for the implementation.
- **Per-Key RBAC Enforcement:** `internal/auth/rbac.go` implements authorization on top of authentication. API keys (from `secrets.json` → `api_keys`) have roles (admin/user/read-only), agent allowlists, endpoint allowlists, expiration, and enable/disable flags. See §16.
- **Interceptor Chain:** Content preprocessing is handled by a pluggable `InterceptorChain` (`internal/pipeline/interceptor.go`) with priority-ordered steps: RedactInterceptor (regex patterns, priority 10), SpotlightInterceptor (untrusted-content history enveloping, priority 12), InjectionInterceptor (deterministic prompt-injection detection, priority 15), EntropyInterceptor (high-entropy string redaction, priority 20), TFIDFInterceptor (relevance scoring, priority 30), and BouncerInterceptor (engine summarization, priority 50). The chain is built in `main.go:buildInterceptorChain()`, stored on `NenyaGateway.InterceptorChain`, and executes all interceptors in priority order before routing. Failure semantics: security interceptors (redact/spotlight/injection/entropy) implement `Strict()` — operational faults abort with 503 `error_kind=internal_error` (`pipeline.StrictError`); TF-IDF fails open; `RejectError` (policy decisions: strict injection rejection, bouncer `fail_open=false`) always aborts with 403. `InterceptRequest` carries the proxy-resolved `AgentName` + `Agent *config.AgentConfig` (nil-safe) as the per-agent policy context path.
- **Token Budget Trimming:** After interceptor chain execution, if the payload still exceeds the hard limit, `TrimPayload` (`internal/pipeline/trim.go`) drops oldest non-system messages and applies middle-out truncation. The hard limit is `context.hard_limit_tokens` when set, else the per-model derived budget `util.DeriveInputTokenBudget(max_context, effective_output)` = `min(max_context/4*3, max_context - effective_output)`, reserving the request's effective output room (the smaller of client `max_tokens` and the model's `max_output`). The bouncer's summarization trigger is `context.soft_limit_tokens` when set (clamped to the hard limit), else `max_context/8`.
- **Context-Limit Retry:** When an upstream returns a context-length error, the retry loop (`internal/proxy/retry.go`) can automatically summarize messages via the engine chain and retry once. Controlled by `governance.auto_retry_on_context_limit` config boolean. See `handleContextLimitError()` and `attemptContextLimitSummarization()`.
- **Local Engine Lifecycle:** `internal/local/` manages preloading/unloading of local Ollama models. `EngineManager` (`manager.go`) wraps `SessionManager` (`session.go`) with LRU eviction, startup preloading, and dead-lock-free lock ordering. Models are loaded via Ollama's `/api/generate` with `keep_alive=-1` and unloaded with `keep_alive=0`. Configured via `local_engine` config section.
- **Structured Error Handling:** All error responses include an `error_kind` field (`internal/infra/errors.go`) for programmatic diagnostics. Categories: `context_exceeded`, `rate_limited`, `auth_failed`, `model_not_found`, `policy_refusal`, `provider_timeout`, `provider_error`, `network_error`, `payload_too_large`, `invalid_request`, `unsupported_combination` (422: a well-formed request the gateway cannot serve as asked — e.g. an agent with no configured models), `bouncer_error`, `injection_detected`, `internal_error`, `mcp_unavailable`, `quota_exhausted`, `exfil_blocked`, `exfil_detected`. Each `ErrorKind` has `Retryable()`, `ShouldFailover()`, and `HTTPStatus()` (canonical status at new call sites) methods. Untrusted client schemas are budget-checked before routing (`jsonschema.CheckToolListBudget`/`CheckResponseFormatBudget`: nodes/depth/bytes → 400 `invalid_request`, counted in `nenya_schema_budget_rejections_total`). All error writing uses `writeStructuredError()` instead of bare `http.Error()`.

### 7. Integer Overflow Prevention (CWE-190)

- **Vulnerable Patterns:** Arithmetic on length values before slice allocation can cause integer overflow:
  ```go
  // VULNERABLE - n+1 can overflow
  parts := make([]int, n+1)
  ```
- **Safe Pattern:** Always check for potential overflow before arithmetic:
  ```go
  // SAFE - Check for potential overflow before doing n+1
  if n >= math.MaxInt {
      return n
  }
  parts := make([]int, n+1)
  ```
- **Key Principles:**
  1. Always check for potential overflow before arithmetic operations
  2. Validate input sizes against reasonable maximums
  3. Use explicit bounds checking with math.MaxInt constants
  4. Consider using wider integer types (uint64) for intermediate calculations
  5. Clamp values to reasonable limits when overflow is detected
- **Standard Guard Pattern:**
  ```go
  // Standard guard pattern for n+1 allocation
  if n >= math.MaxInt {
      return n  // or handle error appropriately
  }
  parts := make([]int, n+1)
  ```
- **Examples of Fixed Code:**
  - `internal/tiktoken/tiktoken.go:323` - Guard added before `make([]int, n+1)`
  - `internal/pipeline/window.go:268` - Capacity calculation with overflow check
  - `internal/routing/transform.go:112` - Guard added before `len(messages)+1` allocation

### 8. Transient Network Retries (CRITICAL)

All outbound HTTP dispatch points vulnerable to transient network errors (TLS handshake timeout, connection refused, DNS failure, 5xx responses) MUST use the standard `util.DoWithRetry` primitive.

**Standard retry helper:**

- `util.DoWithRetry(ctx, maxAttempts, fn)` — calls `fn` up to `maxAttempts` times with exponential backoff and jitter. Respects `ctx` cancellation. Only succeeds when `fn` returns `nil`.
- `util.CalculateBackoff(attempt int) time.Duration` — reused by the chat-completion proxy retry loop.

**Retry is required for these request types:**

- Model discovery fetches (`internal/discovery/fetch.go:fetchProviderModels`)
- Embeddings passthrough (`internal/proxy/embeddings.go:handleEmbeddings`)
- Responses passthrough (`internal/proxy/responses.go:handleResponses`)
- Health-check probes (`internal/proxy/handler.go:checkOllamaProviderHealth`)
- MCP transport SSE connections and POST requests (`internal/mcp/transport.go:Connect`, `SendRequest`)
- Provider validation at startup (`internal/config/validate.go:validateWithMinimalRequest`)

**Configuration:**

- `governance.max_retry_attempts` — global default (default 3), applied by `GovernanceConfig.EffectiveMaxRetryAttempts()`
- `providers.<name>.max_retry_attempts` — per-provider override, takes precedence over global default
- `providers.<name>.retryable_status_codes` — per-provider retryable HTTP statuses (default: 429, 500, 502, 503, 504, 529, and the Cloudflare origin-failure family 520–526)
- `providers.<name>.retryable_phrases` — case-insensitive error-body substrings that make a 4xx response retryable/failover-able for that provider (e.g. aggregator-relayed upstream failures); empty uses the built-in pattern sets plus the common upstream-blame phrases ("upstream request failed", "upstream error", "upstream unavailable", "reach upstream")
- `providers.<name>.request_scoped_errors` — rules (`status` + case-insensitive `message_pattern`) marking provider errors as the client's fault even when the status class suggests otherwise (e.g. invalid-parameter 401s): matched errors skip cooldown/rotation state and surface directly to the client (both the chat retry loop and the MCP buffered path); context-length handling (summarization retry) takes precedence over rules; client-cancel during error handling or exhaustion reporting is connection-scoped — no retry, no state writes, no client write (NENYA-42)
- The context deadline bounds the total retry time; never exceed the per-request timeout.
- Network errors and 5xx upstream responses are retried. 4xx responses are NOT retried — except when classified retryable (context-length semantics, per-provider statuses/phrases, or provider matchers).

### 9. Code Readability Patterns (Strong Recommendations)

- **Function Length:** Target ≤80 lines. Functions exceeding 150 lines SHOULD be decomposed into smaller, named helpers. Enforced by `funlen` linter.
- **Cyclomatic Complexity:** Keep under 10. Functions with high branch count (many if/else, switch cases) SHOULD extract branches into named methods. Enforced by `gocyclo` linter (threshold: 15 for existing code).
- **Nesting Depth:** Maximum 3 levels. Deeper nesting MUST use guard clauses (early return) or extraction into a named function. Enforced by `nestif` linter.
- **Decomposition Pattern:** Split large handlers into a validate → resolve → execute pipeline where each step is a distinct method returning early on failure:
  ```go
  // PREFER: linear flow with early returns
  req, err := p.validateRequest(r, gw)
  if err != nil { return }
  targets, err := p.resolveTargets(req, gw)
  if err != nil { return }
  p.execute(w, r, req, targets)
  ```
- **Options Struct Pattern:** Functions with >5 parameters MUST group them into a struct (see §11). Name the struct after the operation: `forwardOpts`, `streamConfig`, `pipelineDeps`.
- **Strategy Extraction:** When a function branches on a condition to pick an algorithm (e.g. IDE vs non-IDE truncation), extract each branch into a function and select via a variable or table:
  ```go
  // PREFER: table-driven selection
  var truncate func(string, int, string) string
  if profile.IsIDE {
      truncate = pipeline.TruncateTFIDFCodeAware
  } else {
      truncate = pipeline.TruncateTFIDF
  }
  result := truncate(text, limit, query)
  ```
- **State Machine Extraction:** Loops that track mutable state across iterations (retry counters, backoff timers, circuit state) SHOULD be extracted into a struct with named methods for each transition.
- **Guard Clauses Over Nesting:** Prefer early returns over nested if-else:
  ```go
  // AVOID: nested conditions
  if x != nil {
      if y, ok := x.(string); ok {
          // 3 levels deep
      }
  }
  // PREFER: guard clauses
  if x == nil { return }
  y, ok := x.(string)
  if !ok { return }
  ```
- **Metrics Guard DRY:** Replace repeated `if gw.Metrics != nil` checks with a nil-safe receiver pattern. Metrics methods should handle nil receivers gracefully:
  ```go
  func (m *Metrics) RecordInterception(reason string) {
      if m == nil { return }
      // ... actual logic
  }
  ```
  This eliminates ~20+ repetitive nil checks across the codebase.
- **Scope:** These recommendations apply to production code (`internal/*`, `cmd/*`). Test code follows separate standards (see §12).

### 10. Error Handling & Reliability (CRITICAL)

- **GoDoc for Exported Symbols:** Every exported type, function, method, constant, and variable MUST have a GoDoc comment. The comment must start with the symbol name:
  ```go
  // CountTokens estimates the number of tokens in the given text using
  // the cl100k_base BPE encoding.
  func CountTokens(text string) int { ... }
  ```
- **Package Documentation:** Every `internal/` package MUST have a `doc.go` file with a package-level comment describing the package's purpose and key types.
- **No Stale Comments:** Comments must accurately reflect the code. If code changes, update or remove affected comments.

### 11. Code Organization & DRY (Don't Repeat Yourself)

- **Shared Helpers:** Common utility functions MUST live in `internal/util/` (e.g., `AddCap` for overflow-safe integer addition, `JoinBackticks` for formatting name lists, `ErrNoProvider` for shared error strings).
- **No Copy-Paste:** If you find the same pattern in 3+ places, extract it into a shared helper. Check `internal/util/` before writing new utility code.
- **Parameter Grouping:** Functions with more than 5 parameters MUST use an options struct or config struct to group related arguments. Example:

  ```go
  // BAD: 10+ parameters
  func forward(gw, w, r, targets, payload, cooldown, tokens, agent, retries, cache)

  // GOOD: grouped into a struct
  type forwardOpts struct {
      Targets    []UpstreamTarget
      Payload    map[string]any
      Cooldown   time.Duration
      TokenCount int
      AgentName  string
      MaxRetries int
      CacheKey   string
  }
  ```

- **Single Responsibility:** Each file should have a clear, focused purpose. If a file exceeds ~500 lines, consider splitting related functionality into separate files.

### 12. Testing Standards

- **Test Coverage:** Every non-trivial exported function MUST have at least one test case covering the happy path. Error paths MUST be tested where they represent recoverable conditions.
- **Table-Driven Tests:** Prefer table-driven tests for functions with multiple input/output combinations. Use `t.Run` for sub-test names.
- **Test Helpers:** Shared test utilities live in `internal/testutil/`. Reuse existing helpers (e.g., `testutil.NewTestLogger`, `testutil.NewTestConfig`) instead of duplicating setup code.
- **No Empty Tests:** Every test must contain at least one assertion. Tests that only verify "no panic" must explicitly check postconditions.
- **Test File Naming:** Test files MUST follow the pattern `*_test.go` and reside in the same package as the code under test.
- **Fuzz Tests:** For security-critical parsing functions (e.g., JSON body parsing, SSE parsing), add fuzz tests using `testing.F`.

### 13. File Path Security

- **Prompt File Validation:** `LoadPromptFile` and any function reading files from user-configurable paths MUST validate that the resolved path does not escape the expected directory (e.g., the config directory). Use `filepath.Abs` and check the prefix. Reject paths containing `..` or absolute paths that point outside the config root.
- **No Path Traversal:** Never concatenate user-supplied path components without sanitization. Use `filepath.Join` and validate the result.

### 14. Concurrency Safety

- **Shared State:** All mutable shared state MUST be protected by `sync.Mutex`, `sync.RWMutex`, or `atomic` operations. Document the locking strategy in GoDoc.
- **Goroutine Lifecycle:** Every goroutine MUST have a clear termination condition (context cancellation, channel close, or explicit stop signal). Document the lifecycle in comments.
- **No Goroutine Leaks:** Use `defer cancel()` for all contexts used to spawn goroutines. Ensure background goroutines are stopped on gateway shutdown.
- **Context Propagation:** Never use `context.Background()` in request-scoped code. The request context (`r.Context()`) must flow through the entire call chain. The only exception is fire-and-forget operations that must outlive the request (e.g., metrics recording with explicit timeout).

### 15. Code Quality & Linting (CRITICAL)

- **Mandatory Linting:** After completing any code changes, you MUST run `golangci-lint run` to verify code quality. This is configured in `.golangci.yml` and includes:
  - `gocyclo` (cyclomatic complexity threshold: 15)
  - `funlen` (max 150 lines, 80 statements)
  - `nestif` (max nesting depth: 4)
  - `staticcheck` (static analysis)
  - `govet` (Go vet with all checks enabled)
  - `errcheck` (unchecked errors)
  - `misspell` (spell checking)
- **Compilation Check:** Run `go build ./...` to ensure all code compiles without errors.
- **CI Enforcement:** The GitHub Actions workflow requires lint to pass before tests run. Failing lint will block PRs.
- **Format Enforcement:** `golangci-lint` automatically runs `gofmt` and `goimports` - do not run these separately.

### 16. RBAC Enforcement (Per-Key Access Control)

**Implementation:** `internal/auth/rbac.go` provides role-based authorization on top of authentication.

**Roles:**

- `admin` — Unrestricted access to all agents and endpoints (bypasses RBAC checks)
- `user` — Access to configured agents and all non-admin endpoints (`/v1/chat/completions`, `/v1/embeddings`, `/v1/responses`, `/v1/images/generations`, `/v1/audio/transcriptions`, `/v1/audio/speech`, `/v1/moderations`, `/v1/rerank`, `/v1/a2a`, `/v1/files`, `/v1/batches`, `/proxy/*`)
- `read-only` — Monitoring role: GET requests on `/v1/models`, `/healthz`, `/statsz`, `/metrics` only (path-scoped since NENYA-131; other GET surfaces such as `/proxy/*` and `/debug/pprof` require `user` or `admin`)

**Agent Scoping:**

- API keys define `allowed_agents` list to restrict which agents they can access
- Empty list grants access to all agents (backward compatible)
- Admin keys bypass agent restrictions

**Endpoint Restrictions:**

- API keys define `allowed_endpoints` list for fine-grained allowlisting (HTTP method + path: `GET /v1/models`, `POST /v1/chat/completions`)
- Overrides default role-based permissions when set
- Empty list uses role-based default permissions
- The two chat wire routes are one logical endpoint: a `POST /v1/chat/completions` entry also authorizes `POST /v1/messages` (canonical form); an explicit `POST /v1/messages` entry authorizes only `/v1/messages`
- Agent scoping is enforced on both chat routes (agent name = top-level `model` field); unreadable, oversized, or unparseable bodies fail closed (400/413) rather than bypass scoping
- Admin keys bypass endpoint restrictions

**Key Configuration Fields:**

```go
type ApiKey struct {
    Name             string         `json:"name"`
    Token            string         `json:"token"`
    Roles            []string       `json:"roles"`
    AllowedAgents    []string       `json:"allowed_agents"`
    AllowedEndpoints []string       `json:"allowed_endpoints,omitempty"`
    CreatedAt        string         `json:"created_at,omitempty"`
    ExpiresAt        string         `json:"expires_at,omitempty"`
    Enabled          bool           `json:"enabled"`
    Permissions      map[string]any `json:"permissions,omitempty"`
}
```

**Authorization Functions:**

- `AuthorizeAgent(apiKey *config.ApiKey, agentName string) bool` — Returns true if key can access the agent
- `AuthorizeEndpoint(apiKey *config.ApiKey, method, path string) bool` — Returns true if key can call the endpoint
- `HasPermission(role Role, perm Permission) bool` — Checks if role grants a specific permission

**Metrics:**

- `nenya_auth_denials_total` counter with `reason` label: `agent`, `endpoint`, `disabled`, `expired`, `invalid_expiry`, `missing_model`, `invalid_key`, `rate_limited`, `key_budget`, `provider_budget`, `payload_too_large`, `invalid_body`, `byok`

**Integration:**

- `internal/proxy/handler.go:authenticateAndAuthorize()` — Validates token/primary, checks enabled/expired, enforces RBAC
- All `/v1/*` handlers receive `*config.ApiKey` instead of raw token string
- Primary token returns synthetic `&config.ApiKey{Name: "primary", Roles: []string{"admin"}, Enabled: true}` for metrics/logging

**Security Notes:**

- Nil-safe functions (return false on `nil` key)
- Admin role bypasses all restrictions (intentional security design)
- Empty lists grant full access (backward compatible)
- Key validation enforces at least one role and minimum token length (16 chars)
