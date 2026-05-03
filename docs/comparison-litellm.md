# Nenya vs LiteLLM — Comprehensive Comparison

> **Date**: 2026-05-02  
> **Nenya**: `/home/rafael/Projects/git.0ur.uk/nenya` — Go AI API gateway  
> **LiteLLM**: `/home/rafael/Projects/github.com/BerriAI/litellm` — Python AI gateway + SDK  

---

## Table of Contents

1. [Project Identity & Scope](#1-project-identity--scope)
2. [Architecture Overview](#2-architecture-overview)
3. [Provider Support](#3-provider-support)
4. [Routing & Load Balancing](#4-routing--load-balancing)
5. [Content Pipeline & Guardrails](#5-content-pipeline--guardrails)
6. [MCP & Agent Integration](#6-mcp--agent-integration)
7. [Resilience & Error Handling](#7-resilience--error-handling)
8. [Security Posture](#8-security-posture)
9. [Observability & Operations](#9-observability--operations)
10. [Multi-Tenancy & Governance](#10-multi-tenancy--governance)
11. [Deployment Footprint](#11-deployment-footprint)
12. [Code Quality & Engineering](#12-code-quality--engineering)
13. [Feature Matrix](#13-feature-matrix)
14. [When to Use Which](#14-when-to-use-which)

---

## 1. Project Identity & Scope

### Nenya

- **Tagline**: Lightweight, secure AI API gateway/proxy written in Go.
- **Role**: Transparent middleware between AI coding clients (OpenCode, Aider) and upstream LLM providers.
- **Primary superpower**: The **"Bouncer"** — intercepts massive HTTP payloads, routes them to a local Ollama instance for summarization and PII/credential redaction, then forwards the sanitized payload upstream.
- **Target audience**: Individual developers, small teams, homelab enthusiasts who want a zero-dependency, security-hardened proxy.
- **License**: MIT.
- **Repository**: `git.0ur.uk/nenya` (private).

### LiteLLM

- **Tagline**: Python SDK, Proxy Server (AI Gateway) to call 100+ LLM APIs in OpenAI format, with cost tracking, guardrails, loadbalancing and logging.
- **Role**: Full-stack enterprise AI gateway + unified SDK.
- **Primary superpower**: 100+ provider support out of the box with a comprehensive enterprise feature set (virtual keys, spend tracking, admin dashboard, SSO, audit logs).
- **Target audience**: Engineering teams, enterprises, SaaS providers needing multi-tenant LLM access management.
- **License**: MIT (core), commercial (enterprise).
- **Repository**: `github.com/BerriAI/litellm` (public, 45.5k stars).
- **Funding**: Y Combinator W23.

---

## 2. Architecture Overview

### Nenya

| Aspect | Detail |
|--------|--------|
| **Language** | Go (1.26) |
| **External deps** | **Zero** — Go standard library only |
| **Source files** | ~105 non-test `.go` files in `internal/` |
| **Server** | `net/http` with explicit timeouts on ListenAndServe |
| **Config** | JSON (single file or directory with deep-merge) |
| **Storage** | None (stateless); optional in-memory LRU response cache |
| **Loading** | Lazy: `NenyaGateway` struct holds all components |
| **Layer DAG** | `config → infra → discovery → stream → pipeline → resilience → providers → adapter → routing → gateway → proxy → mcp` |

**Request flow**:

```
Client POST /v1/chat/completions
  → Auth (Bearer token)
  → Parse JSON, extract model
  → Classify client (IDE detection via User-Agent)
  → Resolve agent or provider
  → Response cache lookup (SHA-256 fingerprint)
  → MCP auto-search & tool injection
  → Content pipeline (best-effort):
      1. Prefix cache optimizations
      2. Tier-0 regex secret redaction + Shannon entropy
      3. Text compaction
      4. Stale tool call pruning
      5. Thought pruning
      6. Window compaction
      7. 3-tier engine interception (TF-IDF + Ollama)
      8. JSON minification
  → Routing: agent fallback chain → circuit breaker → rate limiter → adapter
  → SSE streaming: stallReader → SSETransformingReader → StreamFilter → FlushWriter
  → Response cache store (on success)
```

### LiteLLM

| Aspect | Detail |
|--------|--------|
| **Language** | Python 3.10–3.13 |
| **External deps** | ~100+ packages (FastAPI, httpx, pydantic, Redis, SQLAlchemy, etc.) |
| **Source files** | ~14,663-line `proxy_server.py` + 10,529-line `router.py` + hundreds of modules |
| **Server** | FastAPI + Uvicorn/Gunicorn |
| **Config** | YAML/JSON with env-var interpolation |
| **Storage** | **PostgreSQL** (required) + **Redis** (required) |
| **Loading** | Eager: Prisma migration + Redis cache warmup at startup |
| **Key modules** | `litellm/main.py` (SDK), `litellm/router.py` (load balancer), `litellm/llms/` (118 provider dirs), `litellm/proxy/` (90+ endpoint modules), `litellm/integrations/` (50+ observability hooks) |

**Request flow**:

```
Client → proxy/proxy_server.py → auth/user_api_key_auth.py
  → Redis cache (key lookup)
  → hooks/ (max_budget_limiter, parallel_request_limiter)
  → route_llm_request.py → router.py (load balancing)
  → main.py (SDK entry)
  → llms/{provider}/chat/transformation.py (format conversion)
  → LLM Provider API
  → cost_calculator.py → DB spend writer → PostgreSQL
```

### Key Architectural Differences

| Dimension | Nenya | LiteLLM |
|-----------|-------|---------|
| **Statelessness** | Fully stateless (no DB required) | Stateful (PostgreSQL + Redis required) |
| **Startup time** | Instant (binary) | Heavy (Prisma migration, dependency loading) |
| **Memory profile** | Predictable (manual memory management) | Variable (Python GC, large object graphs) |
| **Concurrency model** | Goroutines (lightweight, 1 per request) | AsyncIO + thread pool |
| **Deployment** | Single binary + config file | Python venv + DB + Redis + workers |

---

## 3. Provider Support

### Nenya

**20 built-in providers**, config-drivable for OpenAI-compatible endpoints:

- **Full adapters** (custom wire format): Anthropic, Gemini, z.ai (Zhipu), Ollama
- **Adapter with tweaks**: OpenRouter, Azure OpenAI, Perplexity, Cohere, DeepInfra
- **Drop-in OpenAI**: DeepSeek, Mistral, xAI (Grok), Groq, Together, SambaNova, Cerebras, NVIDIA, GitHub, Qwen, MiniMax, OpenCode Zen

**Adding a new provider**:
- OpenAI-compatible: **Zero Go code** — add JSON to `providers` section
- Alien format: Write a `ProviderAdapter` implementation in Go (~50-100 lines)

### LiteLLM

**118 provider directories** under `litellm/llms/`, supporting 100+ LLM providers including:

- All major clouds: AWS Bedrock, SageMaker, Azure OpenAI, Azure AI, GCP Vertex AI
- All major AI vendors: OpenAI, Anthropic, Gemini, Cohere, Mistral, DeepSeek
- Self-hosted: Ollama, vLLM, LM Studio, LlamaFile, Triton
- Enterprise: IBM Watsonx, Snowflake, Databricks, Oracle OCI
- Search/retrieval: Tavily, Exa AI, Brave, Serper, SearXNG
- Multimodal: ElevenLabs (audio), Deepgram (speech), Stability AI (image), RunwayML (video)
- Batch/image/audio/rerank endpoints supported for many providers

**Adding a new provider**: Create a Python module under `llms/{provider}/chat/transformation.py`.

### Endpoint Coverage

| Endpoint | Nenya | LiteLLM |
|----------|-------|---------|
| `/chat/completions` | ✅ Full | ✅ Full |
| `/v1/messages` (Anthropic) | ✅ Via format conversion | ✅ Native |
| `/responses` (OpenAI) | ✅ Passthrough | ✅ Full |
| `/embeddings` | ✅ Passthrough | ✅ Full |
| `/images/generations` | ❌ | ✅ Full |
| `/audio/transcriptions` | ❌ | ✅ Full |
| `/audio/speech` | ❌ | ✅ Full |
| `/moderations` | ❌ | ✅ Full |
| `/batches` | ❌ | ✅ Full |
| `/rerank` | ❌ | ✅ Full |
| `/a2a` (Agent-to-Agent) | ❌ | ✅ Full |
| `/proxy/{provider}/*` | ✅ Passthrough | ✅ Via `pass_through_endpoints` |

---

## 4. Routing & Load Balancing

### Nenya

| Feature | Implementation |
|---------|---------------|
| **Agent model lists** | Named agents with fallback or round-robin across model lists |
| **Fallback chains** | Exhaustive: try each target in order, circuit breaker per combination |
| **Round-robin** | Across models in an agent |
| **Regex model selectors** | `provider_rgx`, `model_rgx` against discovery catalog |
| **Deferred provider expansion** | String shorthand resolves to all providers offering the model |
| **Latency-aware routing** | Sorted buffer median latency + ±5% jitter (thundering herd prevention) |
| **Balanced routing** | Multi-dimensional scoring: latency, cost, capability matching, per-model score bonus |
| **Cost tracking** | Price data from OpenRouter `/v1/models`, per-request cost via `PricingEntry.CalculateCost()` |
| **Auto-context-skip** | Skip models with insufficient context window for the input |

**Circuit breaker**: Per agent+provider+model combination (Closed/Open/HalfOpen/ForceOpen states). Checked twice per target (during build and before send).

### LiteLLM

| Feature | Implementation |
|---------|---------------|
| **Routing strategies** | 10 strategies: lowest latency, lowest cost, least busy, lowest TPM/RPM, tag-based, shuffle, adaptive, auto, complexity, quality |
| **Fallbacks** | `context_window_fallbacks`, model group aliasing |
| **Wildcard routing** | `*` → all models, `anthropic/*` → all Anthropic models |
| **Cooldowns** | Provider-level cooldown after failures |
| **Budget limiting** | Per-key, per-team budget enforcement |
| **Parallel request limiting** | Per-key concurrent request cap |
| **Model groups** | Arbitrary model groupings with aliases |
| **Adaptive routing** | Bayesian model state, online learning |

---

## 5. Content Pipeline & Guardrails

### Nenya — The "Bouncer" Pipeline

The content pipeline runs **best-effort** — failures never block the request:

| Stage | Description |
|-------|-------------|
| **Tier-0 regex redaction** | Built-in patterns + custom regex for secrets (AWS keys, GitHub tokens, PEM keys, etc.) |
| **Shannon entropy redaction** | Optional — detects high-entropy tokens (JWTs, opaque API keys) |
| **Text compaction** | Normalize line endings, trim whitespace, collapse blank lines |
| **Stale tool call pruning** | Compact old assistant+tool pairs into summaries |
| **Thought pruning** | Strip `<think.../think>` reasoning blocks |
| **Window compaction** | Sliding window summarization when tokens exceed threshold |
| **3-tier engine interception** | Soft limit → Ollama summarization / Hard limit → TF-IDF truncation + engine |
| **TF-IDF relevance scoring** | Pure Go TF-IDF with no network calls — scores content blocks by relevance to query |
| **Code-aware truncation** | Snaps cuts at blank-line boundaries (IDE mode) |
| **JSON minification** | Final body compaction |

**Engine resolution**:
- `security_filter.engine` supports agent references (with fallback chain) or inline provider/model
- Default engine: Ollama `qwen2.5-coder:7b`
- Engine failure → skip (configurable), forward original payload unchanged

### LiteLLM

| Feature | Description |
|---------|-------------|
| **Secret detection** | 80+ plugins via `detect-secrets` + custom plugins (enterprise) |
| **PII redaction** | Via `enterprise_callbacks/secrets_plugins/` |
| **Policy engine** | Versioned policies with inheritance, conditions, guardrail pipelines (enterprise) |
| **Content moderation** | `/moderations` endpoint passthrough |
| **Prompt injection detection** | Via guardrail hooks |
| **Budget enforcement** | Hard spend limits per key/team/org |
| **Guardrail pipeline** | Sequential guardrail execution with approval workflows |
| **Custom guardrails** | User-defined guardrail callbacks (enterprise) |

**Key difference**: Nenya's guardrails focus on **payload size reduction and secret redaction** for AI coding assistants. LiteLLM's guardrails focus on **policy enforcement, compliance, and content moderation** for enterprise LLM usage.

---

## 6. MCP & Agent Integration

### Nenya

| Feature | Detail |
|---------|--------|
| **Transport** | HTTP + SSE (MCP protocol) |
| **Tool discovery** | At startup, fetch tools from configured MCP servers |
| **Tool injection** | Injected as OpenAI function tools with `server__tool` namespacing |
| **Multi-turn loop** | Buffer SSE response, extract tool_calls, execute MCP tools in parallel, re-send |
| **Auto-search** | Query MCP server for relevant context on each request |
| **Auto-save** | Async POST to MCP server with assistant response content |
| **MCP ToolRegistry** | Thread-safe index of all tools across all servers |
| **Max iterations** | Configurable (default 10), with 5-minute overall deadline |
| **Graceful degradation** | Unreachable server → log warning + skip; mid-session failure → return error as tool result |

### LiteLLM

| Feature | Detail |
|---------|--------|
| **Transport** | MCP gateway + `experimental_mcp_client` bridge |
| **Tool loading** | Load MCP tools in OpenAI format, use with any LLM |
| **MCP gateway** | `/chat/completions` with `type: "mcp"` tool references |
| **Cursor IDE integration** | MCP server endpoint `litellm_proxy/mcp/{server}` |
| **A2A protocol** | Full Agent-to-Agent protocol support |
| **Tool registry** | `LiteLLM_ToolTable` + `LiteLLM_MCPServerTable` in PostgreSQL |
| **BYOK (Bring Your Own Keys)** | Per-user MCP credentials |
| **Tool approval** | `require_approval` per tool call |

---

## 7. Resilience & Error Handling

### Nenya

| Mechanism | Detail |
|-----------|--------|
| **Circuit breaker** | Per agent+provider+model; Closed/Open/HalfOpen/ForceOpen |
| **Retry logic** | Exponential backoff via `util.DoWithRetry`; network errors + 5xx are retried; 4xx are not |
| **Cooldown** | Per-model cooldown after non-retryable errors |
| **Double-check pattern** | Circuit breaker checked twice: target list build + before send |
| **Quota exhaustion** | Long cooldown (up to 30 min) via ForceOpen state |
| **Empty-stream detection** | 200 OK + zero-byte body → emit SSE error payload, increment metric, record failure |
| **Stall detection** | `stallReader` aborts after 120s of no data |
| **Context timeouts** | Per-provider timeout from config, per-MCP-call timeout (30s), MCP loop deadline (5min) |
| **Client disconnect** | Upstream connection aborted, 5s timeout for goroutine cleanup |
| **Best-effort pipeline** | All pipeline steps are best-effort — failures logged, never block request |

### LiteLLM

| Mechanism | Detail |
|-----------|--------|
| **Retry** | `num_retries` config, provider-level |
| **Cooldowns** | Per-provider cooldown tracking |
| **Timeouts** | Per-model `timeout` and `stream_timeout` |
| **Fallbacks** | `context_window_fallbacks` for context overflow |
| **Health checks** | Continuous background health checks per model |
| **Spend limits** | Hard budget enforcement per key/team/org |
| **Rate limiting** | Per-key TPM/RPM with Redis counters |

---

## 8. Security Posture

### Nenya

Nenya is designed with **security as a primary concern**:

| Measure | Detail |
|---------|--------|
| **Timeouts** | Explicit `ReadTimeout`, `WriteTimeout`, `IdleTimeout` on server, timeout on client — never default `http.Client` |
| **Body limits** | `http.MaxBytesReader` on all incoming requests (DoS protection) |
| **Header sanitization** | Strip hop-by-hop headers (Connection, Content-Length) when proxying |
| **Error safety** | Never expose internal stack traces; log internally, return standard HTTP codes |
| **Secret redaction** | Tier-0 regex + optional Shannon entropy detection |
| **Zero external deps** | No supply-chain risk from third-party modules |
| **Stream output filtering** | Sliding window (4KB) for cross-chunk pattern matching in responses |
| **Secret storage** | systemd credentials or docker secrets (filesystem with restricted permissions) |
| **RBAC** | Per-key agent allowlists via `api_keys` in secrets |
| **Path traversal** | `filepath.Abs` prefix validation for prompt files |
| **Integer overflow** | Explicit bounds checking (`math.MaxInt` guards) on all allocations |
| **CodeQL/linting** | `golangci-lint` with `gocyclo`, `funlen`, `nestif`, `staticcheck`, `errcheck`, `misspell` |
| **Binary** | Single static binary — no interpreter, no runtime dependencies |

### LiteLLM

| Measure | Detail |
|---------|--------|
| **Bug bounty** | $500–$3,000 for P0/P1 vulnerabilities |
| **CodeQL** | GitHub CodeQL on all commits |
| **Authentication** | API keys, JWT, OAuth2, SSO, IP allowlisting |
| **Encryption** | TLS in transit, data encrypted with `LITELLM_MASTER_KEY` (cloud) |
| **Audit logs** | Full audit trail with before/after values (enterprise) |
| **Secret scanning** | 80+ detection plugins via `detect-secrets` |
| **Auth styles** | Bearer, api-key, x-api-key, AWS SigV4, GCP service accounts |
| **Vulnerability program** | Private GitHub vulnerability reports |
| **Supply chain** | Multiple dependencies (~100+ packages) = larger attack surface |

**Key difference**: Nenya's security is **architectural** (zero deps, explicit timeouts, body limits, no internal exposure). LiteLLM's security is **operational** (auth, audit, SSO, bug bounty).

---

## 9. Observability & Operations

### Nenya

| Feature | Detail |
|---------|--------|
| **Logging** | `slog` with structured key-values; TTY vs systemd auto-detect |
| **Metrics** | Prometheus `/metrics` endpoint |
| **Stats** | `/statsz` endpoint: per-model token usage + circuit breaker state |
| **Health** | `/healthz` endpoint (no auth) |
| **Usage tracking** | Atomic per-model counters: requests, input/output tokens, errors |
| **Latency tracking** | Per-model sorted buffer with binary-search insertion; median latency |
| **Cost tracking** | Per-request cost from pricing data (microUSD precision) |
| **Hot reload** | `systemctl reload nenya` — reloads config, re-discovers models, preserves counters |
| **Validation** | `./nenya -validate` checks config structure, provider reachability, model integrity |
| **SSE streaming pipeline** | Composable readers/writers: stall detection, SSE transformation, stream filtering, tee writer for cache |

### LiteLLM

| Feature | Detail |
|---------|--------|
| **Logging** | Structured logging via Python `logging` |
| **Metrics** | Prometheus via `success_callback: ["prometheus"]` |
| **Integrations** | 50+ observability hooks: Langfuse, Datadog, OTel, Sentry, New Relic, etc. |
| **Health** | Background health checks per model (APScheduler) |
| **Usage tracking** | PostgreSQL spend logs with daily aggregation tables |
| **Admin dashboard** | Full Next.js UI: usage analytics, logs, key management, guardrails, playground |
| **Background jobs** | Spend batch write (60s), budget reset (10-12min), deployment sync (10s), key rotation (1hr), Slack alerts |
| **Cost attribution** | `cost_calculator.py` → `response._hidden_params["response_cost"]` → header → async DB write |

---

## 10. Multi-Tenancy & Governance

### Nenya

| Feature | Detail |
|---------|--------|
| **Tenancy model** | Agent-based routing (named agents with model lists) |
| **Auth** | Single `client_token` or per-key tokens with agent allowlists |
| **RBAC** | Basic: `api_keys` with `allowed_agents` and `roles` |
| **Spend controls** | Optional per-agent `budget_limit_usd` (tracked but not enforced yet) |
| **Model access** | Agent model lists control what models each agent can use |
| **Scope** | Single-user or small-team focused |

### LiteLLM

| Feature | Detail |
|---------|--------|
| **Tenancy model** | Organization → Team → Project → API Key hierarchy |
| **Auth** | Virtual API keys with full RBAC, SSO, OAuth2 |
| **RBAC** | Complete role system: admins, members, project-scoped permissions |
| **Spend controls** | Per-key, per-team, per-org budgets with hard enforcement |
| **Model access** | Model-level access control per key/team/org via access groups |
| **Guardrails** | Per-key guardrail policies, policy attachments to any scope |
| **Audit** | Full audit log with before/after values |
| **Scope** | Enterprise-grade multi-tenant |

---

## 11. Deployment Footprint

### Nenya

| Resource | Requirement |
|----------|-------------|
| **Runtime** | Go binary (no runtime needed) |
| **RAM** | ~10–50 MB idle |
| **Storage** | Binary ~10 MB + config files |
| **Database** | None (stateless) |
| **Cache** | Optional (in-memory LRU) |
| **Network** | Access to Ollama + upstream provider APIs |
| **OS** | Any (Linux, macOS, Windows) |
| **Startup** | Sub-second |
| **Install** | `go build` or download binary |
| **Update** | Replace binary + SIGHUP reload config |

### LiteLLM

| Resource | Requirement |
|----------|-------------|
| **Runtime** | Python 3.10+ (~100 MB) |
| **RAM** | ~200–500 MB+ idle (Python + dependencies) |
| **Storage** | ~50–100 MB (venv + dependencies) |
| **Database** | PostgreSQL (required) |
| **Cache** | Redis (required) |
| **Network** | Access to upstream providers, DB, Redis |
| **OS** | Linux preferred |
| **Startup** | 5–30 seconds (dependency loading + DB migration) |
| **Install** | `pip install 'litellm[proxy]'` or Docker |
| **Update** | `pip install --upgrade litellm` + restart |

---

## 12. Code Quality & Engineering

### Nenya (Go)

| Metric | Value |
|--------|-------|
| **Non-test Go files** | ~105 in `internal/` |
| **External deps** | 0 |
| **Linting** | `golangci-lint` with strict rules (gocyclo ≤15, funlen ≤150 lines, nestif ≤4) |
| **Testing** | `go test ./...` |
| **Code style** | Effective Go, receiver methods, dependency injection |
| **Error handling** | Wrapped errors, never expose internals, context propagation |
| **Concurrency** | Goroutines with context propagation, `sync.RWMutex`, `atomic` operations |
| **Integer overflow** | Explicit `math.MaxInt` guards on all allocations |

### LiteLLM (Python)

| Metric | Value |
|--------|-------|
| **Python files** | Hundreds across `litellm/` |
| **External deps** | ~100+ packages |
| **Linting** | ruff, mypy, flake8 |
| **Testing** | pytest (async, mock, cov, xdist) |
| **Code style** | Python typing, pydantic models, class-based providers |
| **Error handling** | Exception-based with retry decorators |
| **Concurrency** | AsyncIO + thread pool for blocking operations |

---

## 13. Feature Matrix

| Feature | Nenya | LiteLLM |
|---------|-------|---------|
| **Chat completions (streaming)** | ✅ Full SSE pipeline | ✅ Full |
| **Chat completions (non-streaming)** | ✅ | ✅ |
| **Embeddings** | ✅ Passthrough | ✅ Full |
| **Responses API** | ✅ Passthrough | ✅ Full |
| **Image generation** | ❌ | ✅ |
| **Audio transcription/speech** | ❌ | ✅ |
| **Batches** | ❌ | ✅ |
| **Rerank** | ❌ | ✅ |
| **Moderations** | ❌ | ✅ |
| **Provider count** | 20 built-in + unlimited OpenAI-compatible via config | 100+ via 118 provider modules |
| **Custom providers** | JSON config (OpenAI) + Go adapter (alien format) | Python module |
| **Model discovery** | ✅ Dynamic catalog fetch | ✅ Strategic-based |
| **Agent routing** | ✅ Named agents with fallback chains | ✅ Model groups with aliases |
| **Load balancing** | Latency-weighted + balanced scoring | 10 strategies |
| **Circuit breaker** | ✅ Per combination | ❌ (cooldown-based) |
| **Rate limiting** | ✅ TPM/RPM per host | ✅ Per-key TPM/RPM |
| **Response caching** | ✅ In-memory LRU (SHA-256) | ✅ Redis + disk + S3 + semantic |
| **Secret redaction** | ✅ Regex + entropy | ✅ 80+ plugins (enterprise) |
| **PII detection** | ✅ Via Ollama Bouncer | ✅ Via plugins |
| **Content summarization** | ✅ Ollama engine + TF-IDF | ❌ |
| **Spend tracking** | ✅ In-memory counters | ✅ PostgreSQL + daily aggregation |
| **Cost tracking** | ✅ Per-request pricing | ✅ Full with budget enforcement |
| **Admin UI** | ❌ | ✅ Full Next.js dashboard |
| **MCP support** | ✅ HTTP+SSE, tool injection, multi-turn | ✅ MCP gateway + experimental client |
| **A2A protocol** | ❌ | ✅ Full |
| **SSO** | ❌ | ✅ OAuth2 (Google, Okta, MS, KeyCloak) |
| **Multi-tenancy** | Basic (agent-based + per-key RBAC) | Full (org → team → key) |
| **Audit logs** | ❌ | ✅ PostgreSQL |
| **IDE detection** | ✅ User-Agent inspection | ❌ |
| **Live reload** | ✅ SIGHUP | ✅ Endpoint-based |
| **Prometheus metrics** | ✅ | ✅ |
| **Hot-swap config** | ✅ atomic.Pointer | ✅ (via DB + cache refresh) |
| **Zero external deps** | ✅ | ❌ |
| **Docker image** | ✅ | ✅ |
| **Systemd integration** | ✅ credentials + notify | ❌ |

---

## 14. When to Use Which

### Choose Nenya when:

- **You need maximum security with minimal attack surface**: Zero external dependencies, explicit timeouts, body limits, hardened HTTP client.
- **You use AI coding assistants** (OpenCode, Aider, Cursor): Nenya's Bouncer pipeline is specifically designed for massive code payloads with IDE-aware truncation and code-boundary preservation.
- **You want a lightweight, stateless proxy**: Single binary, no database, sub-second startup, ~10 MB RAM.
- **You have a local Ollama instance** and want local summarization/PII redaction before sending to cloud providers.
- **You need fine-grained circuit breaking**: Per agent+provider+model circuit breakers with double-check pattern.
- **You want dynamic model discovery** without manual configuration updates.

### Choose LiteLLM when:

- **You need a full enterprise AI gateway** with multi-tenant access management, SSO, and audit logs.
- **You need comprehensive provider coverage** across 100+ LLM APIs including cloud platforms (Bedrock, Vertex AI, Azure).
- **You need endpoint coverage** beyond chat: images, audio, batches, rerank, moderations.
- **You need an admin UI** for non-technical team members to manage keys, view usage, and configure models.
- **You need spend tracking and budget enforcement** for cost control across teams.
- **You need a Python SDK** for direct library integration (not just a proxy).
- **You're building a SaaS platform** that resells access to multiple LLM providers.

### Can Use Both When:

- LiteLLM handles multi-tenant access management, billing, and the admin dashboard.
- Nenya sits in front of LiteLLM as a security-hardened edge proxy with payload summarization and PII redaction.

---

## Appendix: Source Code Sizes

### Nenya (Go)

| Directory | `.go` files (excl. tests) | Purpose |
|-----------|--------------------------:|---------|
| `internal/adapter/` | ~8 | Provider wire format adapters |
| `internal/discovery/` | ~6 | Dynamic model discovery |
| `internal/gateway/` | ~4 | Main gateway struct |
| `internal/infra/` | ~10 | Logging, metrics, rate limiter, trackers |
| `internal/mcp/` | ~8 | MCP client + transport |
| `internal/pipeline/` | ~14 | Content pipeline (redaction, TF-IDF, compaction) |
| `internal/providers/` | ~5 | Provider specs + capabilities |
| `internal/proxy/` | ~12 | HTTP handlers, forwarding, SSE streaming |
| `internal/resilience/` | ~3 | Circuit breaker |
| `internal/routing/` | ~12 | Provider resolution, transform, sanitize |
| `internal/security/` | ~2 | Secure memory, token handling |
| `internal/stream/` | ~5 | SSE reader/writer components |
| `internal/tiktoken/` | ~2 | Token counting (BPE) |
| `internal/util/` | ~6 | Shared helpers |
| `internal/testutil/` | ~3 | Test helpers |
| **Total** | **~105** | |

### LiteLLM (Python)

| Directory / File | Size | Purpose |
|------------------|------|---------|
| `litellm/proxy/proxy_server.py` | ~14,663 lines | FastAPI application (all routes) |
| `litellm/router.py` | ~10,529 lines | Load balancer with all strategies |
| `litellm/main.py` | Large | SDK entry points |
| `litellm/llms/` | 118 dirs | Provider implementations |
| `litellm/proxy/` | 90+ entries | Endpoint implementations |
| `litellm/integrations/` | 75+ entries | Observability integrations |
| `litellm/router_strategy/` | 11 strategies | Routing algorithms |
| `litellm/caching/` | 8+ backends | Cache implementations |
| `schema.prisma` | ~1,370 lines, 45 models | Database schema |
| `ui/litellm-dashboard/` | Full Next.js app | Admin dashboard |
| **Total** | **~70k+ lines** | |

---

## License

- **Nenya**: MIT
- **LiteLLM**: MIT (core), commercial (enterprise)
