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
- **Dynamic Routing:** The proxy must inspect the JSON body, read the `"model"` string, and dynamically route to the correct provider via `resolveProvider()`. Agents with fallback chains are resolved via `buildTargetList()`. Agent model lists support string shorthand (looked up from `ModelRegistry`), full object notation (explicit provider/model), or regex-based patterns (`provider_rgx`/`model_rgx` inline on any model entry) that dynamically match against the discovery catalog at runtime.
- **The Ollama Interceptor:** If the `messages[-1].content` length exceeds `config.Governance.ContextSoftLimit`, the proxy must synchronously call the local Ollama API to summarize the text BEFORE forwarding the request upstream. The engine is configured via `security_filter.engine` which supports a string (agent name reference with fallback chain) or an inline object (direct provider/model). See `internal/config/engine_resolve.go` for resolution logic and `internal/pipeline/engine.go` `CallEngineChain` for the fallback chain implementation. When `config.Governance.TFIDFQuerySource` is set, Tier 3 uses TF-IDF relevance scoring (`internal/pipeline/tfidf.go`) to prune content blocks by relevance to the user's query terms, potentially eliminating the engine call entirely if the payload drops below `ContextSoftLimit`.
- **Transparent Streaming:** The proxy must flawlessly pipe the upstream SSE (Server-Sent Events) stream to the client, applying provider-specific response transformations (e.g. Gemini tool_calls normalization, Anthropic OpenAI↔Anthropic format conversion) as needed via the adapter system.
- **Endpoints:** The gateway exposes `/v1/chat/completions` (streaming with Ollama interception), `/v1/models` (model catalog), `/v1/embeddings` (passthrough proxy), `/v1/responses` (passthrough proxy), `/healthz` (engine health probe, no auth), `/statsz` (per-model token usage counters + circuit breaker state, no auth), and `/metrics` (Prometheus-compatible metrics, no auth). All `/v1/*` endpoints require `Authorization: Bearer <client_token>`.
- **Token Usage Tracking:** `infra/usage_tracker.go` implements `UsageTracker` with atomic per-model counters (requests, input_tokens, output_tokens, errors). Input tokens are counted at dispatch; output tokens are extracted from SSE `usage` fields via `stream.SSETransformingReader.OnUsage` callback.
- **Latency Tracking:** `infra/latency_tracker.go` implements `LatencyTracker` which maintains per-model sorted sample buffers (incremental binary-search insertion, O(n) per record) and computes median latency. When `governance.auto_reorder_by_latency` is enabled, `routing.SortTargetsByLatency` reorders targets by median latency with ±5% jitter to prevent thundering herd. The jitter function is injectable for deterministic testing.
- **Structured Logging:** All logging uses `slog` with structured key-value pairs. The logger auto-detects TTY (text) vs systemd (JSON) format. Debug-level logs are gated behind `g.logger.Enabled(ctx, slog.LevelDebug)`.
- **Provider Adapter Pattern:** Provider-specific wire format differences (auth injection, request/response mutation, error classification) are handled by `internal/adapter/` via the `ProviderAdapter` interface. See [`docs/ADAPTERS.md`](docs/ADAPTERS.md) for full details.
- **Circuit Breaker:** Each agent+provider+model combination has an independent circuit breaker (Closed/Open/HalfOpen states) implemented in `internal/resilience/`. Failure thresholds, success thresholds, and max_retries are configured per-agent. See [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md#circuit-breaker) for the state machine.
- **Provider Spec System:** `internal/providers/` defines `ProviderSpec` with capabilities (`SupportsStreamOptions`, `SupportsAutoToolChoice`, `SupportsContentArrays`, `SupportsToolCalls`, `SupportsReasoning`, `SupportsVision`) and per-provider sanitization/response transformer functions. The adapter system builds on top of this.
- **Provider-Specific Thinking Activation:** `ProviderSpec.SanitizeRequest` hooks receive `SanitizeDeps` with `SupportsReasoning` and `ProviderThinking` closure functions (avoiding import cycles with `config`). Zai's `zaiSanitize` injects `thinking: {type: "enabled", clear_thinking: false}` for reasoning-capable models, configurable per-provider via `thinking.enabled` in the config file. Temperature defaults are applied model-specifically (GLM-4.6/4.7 → 1.0). See `internal/providers/zai.go`.
- **DeepSeek v4 Reasoning Injection:** `internal/routing/sanitize.go:ensureDeepSeekReasoningContent` injects `reasoning_content: ""` on all assistant messages for DeepSeek. Required because DeepSeek v4 returns 400 if any assistant message lacks `reasoning_content` in multi-turn conversations where tool calls occurred.
- **MCP Tool Integration:** The gateway supports Model Context Protocol servers via HTTP+SSE transport. When an agent has MCP servers configured, tools are discovered at startup, injected into requests as OpenAI function tools with `tool_choice: "auto"`, and a multi-turn loop executes tool calls between the client and upstream model. See `internal/mcp/` and `internal/proxy/mcp_tools.go` for the implementation.

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

### 8. Code Readability Patterns (Strong Recommendations)
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

### 9. Error Handling & Reliability (CRITICAL)
- **No Panics in Library Code:** NEVER use `panic` in non-`main` packages. Library functions MUST return `error` values. Panics bypass graceful error handling and can crash the entire gateway. The only acceptable use of `panic` is in `cmd/nenya/main.go` for fatal startup failures where recovery is impossible.
- **Error Wrapping:** All errors MUST be wrapped with `fmt.Errorf("context: %w", err)` to preserve the error chain. Never discard error context with bare error strings.
- **No Swallowed Errors:** Never assign errors to `_` unless explicitly justified with a comment explaining why the error is safe to ignore (e.g., `defer func() { _ = resp.Body.Close() }()`).
- **No `os.Exit` in Library Code:** `os.Exit` is only allowed in `cmd/nenya/main.go`. All other packages must return errors to callers.
- **Structured Logging Only:** Use `slog` with structured key-value pairs exclusively. Never use `log.Println`, `fmt.Println`, or `log.Printf` for operational logging.

### 10. Documentation Standards
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
