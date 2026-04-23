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
- **Timeouts:** NEVER use the default `http.Client` or `http.ListenAndServe`. Always explicitly define `ReadTimeout`, `WriteTimeout`, `IdleTimeout`, and Client `Timeout` to prevent resource exhaustion and hanging connections.
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
- **Provider Registry:** Upstream providers are config-driven via `"providers"` JSON sections merged with built-in defaults (`builtInProviders()` in `config.go`, sourced from `ProviderRegistry` in `registry.go`). Adding a new provider (e.g., OpenAI) requires zero Go code changes — only JSON config and a secrets key. Routing uses the `ModelRegistry` for direct model name lookups (priority), then `route_prefixes` for catch-all matching. Only unambiguous prefixes are used (e.g., `claude-` for Anthropic, `grok-` for xAI). Unknown models return a 400 error — there is no default provider fallback.
- **Dynamic Model Discovery:** At startup and on SIGHUP reload, Nenya fetches `/v1/models` from each configured provider (concurrently, concurrency-limited to 5 by default via `HealthCheckConfig.MaxConcurrent`, 10s timeout each). Responses are parsed by provider-specific parsers (`internal/discovery/parse.go`) and merged with the static ModelRegistry using three-tier priority: config overrides > discovered models > static registry. The merged catalog (`ModelCatalog` in `internal/discovery/discovery.go`) is used for model resolution in routing, `/v1/models` endpoint, and `max_tokens` injection. Discovery failures degrade gracefully — providers that fail are skipped and the static registry is used as fallback.
- **Dynamic Routing:** The proxy must inspect the JSON body, read the `"model"` string, and dynamically route to the correct provider via `resolveProvider()`. Agents with fallback chains are resolved via `buildTargetList()`. Agent model lists support string shorthand (looked up from `ModelRegistry`) or full object notation (explicit provider/model).
- **The Ollama Interceptor:** If the `messages[-1].content` length exceeds `config.Governance.ContextSoftLimit`, the proxy must synchronously call the local Ollama API to summarize the text BEFORE forwarding the request upstream. The engine is configured via `security_filter.engine` which supports a string (agent name reference with fallback chain) or an inline object (direct provider/model). See `internal/config/engine_resolve.go` for resolution logic and `internal/pipeline/engine.go` `CallEngineChain` for the fallback chain implementation. When `config.Governance.TFIDFQuerySource` is set, Tier 3 uses TF-IDF relevance scoring (`internal/pipeline/tfidf.go`) to prune content blocks by relevance to the user's query terms, potentially eliminating the engine call entirely if the payload drops below `ContextSoftLimit`.
- **Transparent Streaming:** The proxy must flawlessly pipe the upstream SSE (Server-Sent Events) stream to the client, applying provider-specific response transformations (e.g. Gemini tool_calls normalization, Anthropic OpenAI↔Anthropic format conversion) as needed via the adapter system.
- **Endpoints:** The gateway exposes `/v1/chat/completions` (streaming with Ollama interception), `/v1/models` (model catalog), `/v1/embeddings` (passthrough proxy), `/v1/responses` (passthrough proxy), `/healthz` (engine health probe, no auth), `/statsz` (per-model token usage counters + circuit breaker state, no auth), and `/metrics` (Prometheus-compatible metrics, no auth). All `/v1/*` endpoints require `Authorization: Bearer <client_token>`.
- **Token Usage Tracking:** `infra/usage_tracker.go` implements `UsageTracker` with atomic per-model counters (requests, input_tokens, output_tokens, errors). Input tokens are counted at dispatch; output tokens are extracted from SSE `usage` fields via `stream.SSETransformingReader.OnUsage` callback.
- **Latency Tracking:** `infra/latency_tracker.go` implements `LatencyTracker` which maintains per-model sorted sample buffers (incremental binary-search insertion, O(n) per record) and computes median latency. When `governance.auto_reorder_by_latency` is enabled, `routing.SortTargetsByLatency` reorders targets by median latency with ±5% jitter to prevent thundering herd. The jitter function is injectable for deterministic testing.
- **Structured Logging:** All logging uses `slog` with structured key-value pairs. The logger auto-detects TTY (text) vs systemd (JSON) format. Debug-level logs are gated behind `g.logger.Enabled(ctx, slog.LevelDebug)`.
- **Provider Adapter Pattern:** Provider-specific wire format differences (auth injection, request/response mutation, error classification) are handled by `internal/adapter/` via the `ProviderAdapter` interface. See [`docs/ADAPTERS.md`](docs/ADAPTERS.md) for full details.
- **Circuit Breaker:** Each agent+provider+model combination has an independent circuit breaker (Closed/Open/HalfOpen states) implemented in `internal/resilience/`. Failure thresholds, success thresholds, and max_retries are configured per-agent. See [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md#circuit-breaker) for the state machine.
- **Provider Spec System:** `internal/providers/` defines `ProviderSpec` with capabilities (`SupportsStreamOptions`, `SupportsAutoToolChoice`, `SupportsContentArrays`, `SupportsToolCalls`, `SupportsReasoning`, `SupportsVision`) and per-provider sanitization/response transformer functions. The adapter system builds on top of this.
- **MCP Tool Integration:** The gateway supports Model Context Protocol servers via HTTP+SSE transport. When an agent has MCP servers configured, tools are discovered at startup, injected into requests as OpenAI function tools with `tool_choice: "auto"`, and a multi-turn loop executes tool calls between the client and upstream model. See `internal/mcp/` and `internal/proxy/mcp_tools.go` for the implementation.
