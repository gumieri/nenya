# Nenya - AI Agent Instructions

## Project Overview
Nenya is a lightweight, highly secure AI API Gateway/Proxy written in Go. It acts as a transparent middleware between local AI coding clients (like OpenCode/Aider) and upstream LLM providers (z.ai, Google Gemini, DeepSeek). 

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

### 5. Core Workflows to Maintain
- **Provider Registry:** Upstream providers are config-driven via `"providers"` JSON sections merged with built-in defaults (`builtInProviders()` in `config.go`, sourced from `ProviderRegistry` in `registry.go`). Adding a new provider (e.g., OpenAI) requires zero Go code changes — only JSON config and a secrets key. Routing uses `route_prefixes` for model-name-based provider resolution, with direct model name lookups via `ModelRegistry` taking priority.
- **Dynamic Routing:** The proxy must inspect the JSON body, read the `"model"` string, and dynamically route to the correct provider via `resolveProvider()`. Agents with fallback chains are resolved via `buildTargetList()`. Agent model lists support string shorthand (looked up from `ModelRegistry`) or full object notation (explicit provider/model).
- **The Ollama Interceptor:** If the `messages[-1].content` length exceeds `config.Governance.ContextSoftLimit`, the proxy must synchronously call the local Ollama API to summarize the text BEFORE forwarding the request upstream. The engine is configured via `security_filter.engine` which supports a string (agent name reference with fallback chain) or an inline object (direct provider/model). See `internal/config/engine_resolve.go` for resolution logic and `internal/pipeline/engine.go` `CallEngineChain` for the fallback chain implementation.
- **Transparent Streaming:** The proxy must flawlessly pipe the upstream SSE (Server-Sent Events) stream to the client, applying provider-specific response transformations (e.g. Gemini tool_calls normalization) as needed via the adapter system.
- **Endpoints:** The gateway exposes `/v1/chat/completions` (streaming with Ollama interception), `/v1/models` (model catalog), `/v1/embeddings` (passthrough proxy), `/healthz` (Ollama health probe, no auth), `/statsz` (per-model token usage counters + circuit breaker state, no auth), and `/metrics` (Prometheus-compatible metrics, no auth). All `/v1/*` endpoints require `Authorization: Bearer <client_token>`.
- **Token Usage Tracking:** `infra/usage_tracker.go` implements `UsageTracker` with atomic per-model counters (requests, input_tokens, output_tokens, errors). Input tokens are counted at dispatch; output tokens are extracted from SSE `usage` fields via `stream.SSETransformingReader.OnUsage` callback.
- **Structured Logging:** All logging uses `slog` with structured key-value pairs. The logger auto-detects TTY (text) vs systemd (JSON) format. Debug-level logs are gated behind `g.logger.Enabled(ctx, slog.LevelDebug)`.
- **Provider Adapter Pattern:** Provider-specific wire format differences (auth injection, request/response mutation, error classification) are handled by `internal/adapter/` via the `ProviderAdapter` interface. See [`docs/ADAPTERS.md`](docs/ADAPTERS.md) for full details.
- **Circuit Breaker:** Each agent+provider+model combination has an independent circuit breaker (Closed/Open/HalfOpen states) implemented in `internal/resilience/`. Failure thresholds, success thresholds, and max_retries are configured per-agent. See [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md#circuit-breaker) for the state machine.
- **Provider Spec System:** `internal/providers/` defines `ProviderSpec` with capabilities (`SupportsStreamOptions`, `SupportsAutoToolChoice`, `SupportsContentArrays`) and per-provider sanitization/response transformer functions. The adapter system builds on top of this.
