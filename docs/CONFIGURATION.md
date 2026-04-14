# Configuration Reference

Nenya reads its configuration from a JSON file (default: `config.json`). See [`../configs/example.config.json`](../configs/example.config.json) for a fully-documented example or [`../configs/minimal_example.config.json`](../configs/minimal_example.config.json) for the smallest possible configuration.

**Important**: Configuration format changed from TOML to JSON with semantic grouping. Old `interceptor`, `ollama`, `ratelimit`, and `filter` sections are now unified under `governance` and `security_filter` with engine abstraction.

## Top-Level Sections

| Section | JSON key | Description |
|---------|----------|-------------|
| Server | `server` | Listen address, body limits, token estimation |
| Governance | `governance` | Unified context limits, truncation, and rate limiting |
| Security Filter | `security_filter` | Tier-0 regex secret redaction with configurable engine |
| Prefix Cache | `prefix_cache` | Prompt cache alignment optimizations |
| Compaction | `compaction` | Text compaction (whitespace, blank lines, JSON) |
| Window | `window` | Sliding window conversation compaction with configurable engine |
| Response Cache | `response_cache` | In-memory LRU cache for deterministic response caching |
 | Agents | `agents` | Named agent definitions with fallback chains, circuit breaker, optional system prompts, optional memory integration, and optional MCP server integration |
| Providers | `providers` | Upstream provider registry |

## `server`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `listen_addr` | string | `":8080"` | Bind address and port |
| `max_body_bytes` | int | `10485760` (10 MB) | Maximum incoming request body size |
| `token_ratio` | float | `4.0` | Characters (runes) per token for estimation. Used to approximate token counts without a tokenizer. Adjust based on your model's tokenization. |

## `governance`

Unified configuration for context management, truncation, and rate limiting.

The interceptor implements a 3-tier pipeline for the last user message content:

- **Tier 1** (pass-through): content below `context_soft_limit` runes
- **Tier 2** (engine summarization): content between `context_soft_limit` and `context_hard_limit` runes
- **Tier 3** (truncation + engine): content above `context_hard_limit` runes. Truncation uses the strategy selected by `truncation_strategy`:
  - `"middle-out"` (default): positional — keeps first/last percentages, discards middle
  - When `tfidf_query_source` is set: **TF-IDF scoring** — splits content into blocks (paragraphs + code fences), scores each block's relevance to the user's prior messages or the start of the current message, and greedily keeps the most relevant blocks within budget. First/last blocks are pinned as a safety net. If TF-IDF reduces the payload below `context_soft_limit`, the engine call is skipped entirely (zero network overhead).

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `ratelimit_max_tpm` | int | `250000` | Max tokens per minute per upstream host (0 = disabled) |
| `ratelimit_max_rpm` | int | `15` | Max requests per minute per upstream host (0 = disabled) |
| `context_soft_limit` | int | `4000` | Runes below this pass through unmodified. Also the threshold below which TF-IDF-pruned payloads skip the engine call. |
| `context_hard_limit` | int | `24000` | Runes above this are truncated before engine summarization |
| `truncation_strategy` | string | `"middle-out"` | Truncation method. `"middle-out"` (positional) or any value — TF-IDF is activated by setting `tfidf_query_source` instead. |
| `keep_first_percent` | float | `15.0` | Percentage of blocks to pin from the start when truncating (safety net for both middle-out and TF-IDF) |
| `keep_last_percent` | float | `25.0` | Percentage of blocks to pin from the end when truncating (safety net for both middle-out and TF-IDF) |
| `tfidf_query_source` | string | `""` (disabled) | Enable TF-IDF relevance-scored truncation for Tier 3. `""` = disabled (use middle-out). `"prior_messages"` = use previous user messages as query terms. `"self"` = use first 500 runes of the massive message as query terms. When enabled, if TF-IDF reduces the payload below `context_soft_limit`, the engine call is skipped entirely. |
| `retryable_status_codes` | []int | `[429, 500, 502, 503, 504]` | HTTP status codes that trigger fallback to the next model in an agent chain. **Warning: setting this field REPLACES the built-in defaults entirely.** You must include all codes you want retryable (including the standard ones). Per-provider override available via `providers.<name>.retryable_status_codes` (provider-level replaces global for that provider). |

## `security_filter`

Tier-0 regex-based secret redaction runs on every request, before any other pipeline step. Includes configurable engine for privacy filtering.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enabled` | bool | `true` | Enable/disable the filter. Defaults to `true` if `patterns` are provided but field omitted. |
| `patterns` | []string | (9 built-in) | Custom regex patterns. Replaces built-in patterns if set. Built-in patterns match: AWS keys, GitHub tokens, Google OAuth, sk- API keys, PEM private keys, AWS credential file lines, password/key assignments, Docker tokens, SendGrid keys. |
| `redaction_label` | string | `"[REDACTED]"` | Replacement string for matched secrets |
| `output_enabled` | bool | `false` | Enable stream output filtering (secret redaction and execution policy blocking on responses) |
| `output_window_chars` | int | `4096` | Sliding window size (in chars) for cross-chunk pattern matching in output streams |
| `skip_on_engine_failure` | bool | `true` | When the engine (Ollama/cloud) is unreachable, skip summarization and forward the original payload. If `false`, hard-limit payloads are truncated even when the engine fails. |
| `engine` | string or object | (see below) | Agent name reference or inline engine configuration |

### Engine Configuration (`engine`)

Both `security_filter.engine` and `window.engine` support two forms:

#### Form 1: Agent Reference (string)

References a named agent by name. The agent's model list becomes the engine's fallback chain. The agent's `system_prompt` / `system_prompt_file` are used as defaults (overridable by inline fields on the `EngineRef`).

```json
{
  "security_filter": {
    "engine": "summarizer"
  }
}
```

The agent `"summarizer"` must exist in the `agents` section with at least one model.

#### Form 2: Inline Configuration (object)

Directly specifies the engine model, identical to the previous `EngineConfig` format:

```json
{
  "security_filter": {
    "engine": {
      "provider": "ollama",
      "model": "qwen2.5-coder:7b",
      "timeout_seconds": 600
    }
  }
}
```

#### Engine Fields

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `provider` | string | `"ollama"` | Provider name from the `providers` registry (inline mode only) |
| `model` | string | `"qwen2.5-coder:7b"` | Model identifier (inline mode only) |
| `system_prompt` | string | `""` | Inline system prompt. Highest priority. |
| `system_prompt_file` | string | `""` | Path to system prompt file. Falls back to built-in prompt if empty. |
| `timeout_seconds` | int | `60` | Timeout for individual engine API calls |

**Prompt priority**: `system_prompt` (inline) > `system_prompt_file` > agent's `system_prompt` > built-in default.

**Note**: `system_prompt_file` supports `%d` placeholder for window summarization (replaced with active message count).

#### Agent-as-Engine: Fallback Chain

When an agent reference is used, the engine inherits the agent's full model list as a fallback chain. If the primary model fails, the next model in the list is tried automatically. This provides resilience for engine calls (e.g., Ollama down → fall back to a cloud provider).

```json
{
  "agents": {
    "summarizer": {
      "strategy": "fallback",
      "system_prompt": "You are a privacy filter...",
      "models": [
        { "provider": "ollama", "model": "qwen2.5-coder:7b" },
        { "provider": "deepseek", "model": "deepseek-chat" }
      ]
    }
  },
  "security_filter": {
    "engine": "summarizer"
  },
  "window": {
    "engine": "summarizer"
  }
}
```

**Structured logging**: Engine calls log the `caller` (`security_filter` or `window`), `agent` name (or `inline`), `provider`, `model`, and `attempt`/`total` for observability.

## `prefix_cache`

Optimizations to improve upstream provider prefix cache hit rates by stabilizing the prompt structure.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enabled` | bool | `true` (auto) | Master toggle. Auto-enabled when any sub-field is explicitly set to `true`. |
| `pin_system_first` | bool | `true` | Reorder all `system` role messages to the top of the messages array |
| `stable_tools` | bool | `true` | Sort `tools[]` array by `function.name` for deterministic ordering |
| `skip_redaction_on_system` | bool | `true` | Skip Tier-0 regex redaction on system messages to preserve prefix byte-identity |

## `compaction`

Text compaction applied to all message content (both string and multi-part content arrays).

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enabled` | bool | `true` (auto) | Master toggle. Auto-enabled when any sub-field is explicitly set to `true`. |
| `normalize_line_endings` | bool | `true` | Convert CRLF to LF |
| `trim_trailing_whitespace` | bool | `true` | Remove trailing spaces/tabs from each line |
| `collapse_blank_lines` | bool | `true` | Collapse runs of 3+ blank lines to max 2 |
| `json_minify` | bool | `true` | Minify the final JSON body with `json.Compact` |

Compaction runs after redaction, before engine interception. JSON minify runs at the very end of the pipeline.

## `window`

Sliding window conversation compaction for long conversations. When the estimated token count exceeds `max_context * trigger_ratio`, older messages are summarized (or truncated) and replaced with a single system summary message.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enabled` | bool | `false` | Master toggle (off by default) |
| `mode` | string | `"summarize"` | `"summarize"` (engine), `"truncate"` (hard cut), or `"tfidf"` (relevance-scored, zero network calls) |
| `active_messages` | int | `6` | Number of recent messages to preserve unchanged |
| `trigger_ratio` | float | `0.8` | Trigger when tokens exceed `max_context * ratio` (0.0-1.0) |
| `summary_max_runes` | int | `4000` | Maximum length of the generated summary |
| `max_context` | int | `128000` | Context window size. Overridden by agent model `max_context` when routing through agents. |
| `engine` | string or object | (see below) | Agent name reference or inline engine configuration for window summarization |

## `response_cache`

In-memory LRU cache for deterministic response caching. Responses are cached by SHA-256 fingerprint of the request payload. On cache hit, the stored SSE stream is replayed to the client with `X-Nenya-Cache-Status: HIT` header.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enabled` | bool | `false` | Master toggle (off by default) |
| `max_entries` | int | `512` | Maximum number of cached responses (LRU eviction) |
| `max_entry_bytes` | int | `1048576` (1 MB) | Maximum size per cached response |
| `ttl_seconds` | int | `3600` (1 hour) | Time-to-live for cached entries |
| `evict_every_seconds` | int | `300` (5 minutes) | Background eviction sweep interval |
| `force_refresh_header` | string | `"x-nenya-cache-force-refresh"` | HTTP header name that bypasses cache when present |

**Cache key**: Deterministic SHA-256 computed from `model`, `messages`, `temperature`, `top_p`, `max_tokens`, `tools`, `tool_choice`, `response_format`, `stop`, `stream`.

**Bypass**: Send any non-empty value for the configured `force_refresh_header` to force a cache miss.

## `agents`

Named agent definitions with model fallback chains and optional system prompts. When a request specifies `model: "<agent_name>"`, the gateway routes through the agent's model list.

### Model Shorthand (Convention)

Models listed in the built-in **Model Registry** can be specified as plain strings. Provider and `max_context` are resolved automatically:

```json
{
  "agents": {
    "build": {
      "strategy": "fallback",
      "models": [
        "gemini-2.5-flash",
        "deepseek-reasoner"
      ]
    }
  }
}
```

### Model Object Notation (Configuration/Override)

For custom or local models (not in the registry), or to override registry defaults, use full objects:

```json
{
  "agents": {
    "build": {
      "strategy": "fallback",
      "models": [
        "gemini-2.5-flash",
        {
          "provider": "ollama",
          "model": "qwen2.5-coder:7b",
          "max_context": 32000,
          "url": "http://localhost:11434/v1/chat/completions"
        }
      ]
    }
  }
}
```

Both styles can be mixed in the same `models` array.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `strategy` | string | `"round-robin"` | `"round-robin"` or `"fallback"` |
| `cooldown_seconds` | int | `60` | Seconds to skip a model after a retryable error |
| `failure_threshold` | int | `5` | Circuit breaker: consecutive failures before tripping to Open state |
| `success_threshold` | int | `1` | Circuit breaker: consecutive successes in HalfOpen to recover to Closed state |
| `max_retries` | int | `0` | Cap on retry attempts per request (0 = unlimited) |
| `system_prompt` | string | `""` | Inline system prompt injected as the first message (only if no existing system message). |
| `system_prompt_file` | string | `""` | Path to system prompt file. Lower priority than `system_prompt`. |
| `mcp` | object | (none) | Optional Model Context Protocol server integration. See [MCP Integration](MCP_INTEGRATION.md) for details. |
| `models` | array | (required) | List of model entries (strings or objects) to try in order |

Each model entry (object form):

| Field | Type | Description |
|-------|------|-------------|
| `provider` | string | Provider name (must match a key in `providers` or a built-in) |
| `model` | string | Model identifier sent to the upstream API |
| `url` | string | (optional) Override provider URL for this specific model |
| `max_context` | int | Context window size for token budgeting and window compaction |

**String shorthand**: If the entry is a string, it must exist in the built-in Model Registry. The registry resolves `provider` and `max_context` automatically. If the model is not found, configuration loading fails with an error.

**System prompt injection**: If `system_prompt` or `system_prompt_file` is set, the prompt is injected as the first message in the array only when no existing system message is present.

## `providers`

Upstream LLM provider registry. Built-in providers are automatically loaded from the internal Provider Registry:

| Name | URL | Prefixes | Auth Style |
|------|-----|----------|------------|
| `gemini` | `https://generativelanguage.googleapis.com/v1beta/openai/chat/completions` | `gemini-` | `bearer+x-goog` |
| `deepseek` | `https://api.deepseek.com/chat/completions` | `deepseek-` | `bearer` |
| `zai` | `https://api.z.ai/api/paas/v4/chat/completions` | `zai-`, `glm-` | `bearer` |
| `groq` | `https://api.groq.com/openai/v1/chat/completions` | `llama-`, `llama3-`, `mixtral-`, `whisper-` | `bearer` |
| `together` | `https://api.together.xyz/v1/chat/completions` | `meta-llama/`, `mistralai/`, `qwen/`, `together/` | `bearer` |
| `nvidia_free` | `https://integrate.api.nvidia.com/v1/chat/completions` | (none) | `bearer` |
| `qwen_free` | `https://dashscope.aliyuncs.com/compatible-mode/v1/chat/completions` | (none) | `bearer` |
| `minimax_free` | `https://api.minimax.chat/v1/chat/completions` | (none) | `bearer` |
| `sambanova` | `https://api.sambanova.ai/v1/chat/completions` | (none) | `bearer` |
| `cerebras` | `https://api.cerebras.ai/v1/chat/completions` | (none) | `bearer` |
| `github` | `https://models.inference.ai.azure.com/chat/completions` | (none) | `bearer` |
| `ollama` | `http://127.0.0.1:11434/v1/chat/completions` | (none) | `none` |

To add or override a provider:

```json
{
  "providers": {
    "openai": {
      "url": "https://api.openai.com/v1/chat/completions",
      "route_prefixes": ["gpt-", "o3-", "o4-"],
      "auth_style": "bearer"
    }
  }
}
```

| Field | Type | Description |
|-------|------|-------------|
| `url` | string | Upstream chat completions endpoint |
| `route_prefixes` | []string | Model name prefixes that route to this provider |
| `auth_style` | string | `"bearer"`, `"bearer+x-goog"` (Gemini), or `"none"` (Ollama) |
| `retryable_status_codes` | []int | Provider-level override for retryable status codes. **Replaces** both global and built-in defaults for this provider. If not set, falls back to `governance.retryable_status_codes`, then built-in defaults `[429, 500, 502, 503, 504]`. |

API keys are loaded from the secrets file via `provider_keys` (keyed by provider name). See [`SECRETS_FORMAT.md`](SECRETS_FORMAT.md).

### Gemini `auth_style: "bearer+x-goog"`

Gemini requires both `Authorization: Bearer <key>` and `x-goog-api-key: <key>` headers. The `bearer+x-goog` auth style sets both automatically.

### Provider Adapters

Provider-specific wire format differences (auth injection, request/response mutation, error classification) are handled by the adapter system. Most providers use the `OpenAIAdapter` with capability-based parameter stripping. See [`ADAPTERS.md`](ADAPTERS.md) for the full adapter reference and capability matrix.

Gemini requires both `Authorization: Bearer <key>` and `x-goog-api-key: <key>` headers. The `bearer+x-goog` auth style sets both automatically.

### Gemini `extra_content` (Thought Signatures)

Gemini 3 models include `extra_content.google.thought_signature` in tool_calls responses. Nenya preserves this field (it is required for multi-turn function calling with Gemini 3) and adds the missing `index` field to comply with the OpenAI spec.

## Model Registry

Built-in models that can be referenced by string shorthand in agent `models` arrays. Each entry maps to a provider and includes a default `max_context`.

| Model | Provider | Max Context |
|-------|----------|-------------|
| `gemini-3.1-flash-lite-preview` | `gemini` | 128000 |
| `gemini-3-flash-preview` | `gemini` | 128000 |
| `gemini-2.5-flash-lite` | `gemini` | 128000 |
| `gemini-2.5-flash` | `gemini` | 128000 |
| `deepseek-reasoner` | `deepseek` | 128000 |
| `deepseek-chat` | `deepseek` | 128000 |
| `glm-5.1` | `zai` | 128000 |
| `glm-5-turbo` | `zai` | 128000 |
| `glm-5v-turbo` | `zai` | 128000 |
| `glm-5` | `zai` | 128000 |
| `glm-4.7` | `zai` | 128000 |
| `glm-4.7-flash` | `zai` | 128000 |
| `glm-4.7-flashx` | `zai` | 128000 |
| `glm-4.6` | `zai` | 128000 |
| `glm-4.6v` | `zai` | 128000 |
| `glm-4.5` | `zai` | 128000 |
| `glm-4.5-air` | `zai` | 128000 |
| `glm-4.5-flash` | `zai` | 128000 |
| `glm-4.5v` | `zai` | 128000 |
| `nemotron-3-super` | `nvidia_free` | 4000 |
| `qwen-3.6-plus` | `qwen_free` | 8000 |
| `minimax-m2.5` | `minimax_free` | 8000 |
| `llama-3.3-70b-versatile` | `groq` | 131072 |
| `mixtral-8x7b-32768` | `groq` | 32768 |
| `llama-3.1-405b-instruct` | `sambanova` | 128000 |
| `llama-3.3-70b` | `cerebras` | 8192 |
| `gpt-4o` | `github` | 128000 |
| `phi-3.5-mini-instruct` | `github` | 128000 |
| `qwen2.5-72b-turbo` | `together` | 32768 |

Models not in this registry (e.g., local Ollama models, custom endpoints) must be specified as full objects with explicit `provider` and `model` fields.

## Processing Pipeline Order

  1. **Response cache lookup** (if enabled, bypass entire pipeline on hit)
  2. **MCP auto-search** (if agent has mcp.auto_search, query MCP server and inject as system message)
  3. **MCP tool injection** (if agent has MCP servers, inject tools as OpenAI function tools + system prompt)
  4. **Prefix cache optimizations** (pin system messages, sort tools — includes MCP tools)
  5. **Agent system prompt injection** (if agent has prompt and no system message exists)
  6. **Tier-0 regex redaction** (secret patterns via `security_filter`)
  7. **Text compaction** (normalize, trim, collapse blanks)
  8. **Window compaction** (if enabled and threshold exceeded)
  9. **Engine interception** (3-tier last-message summarization using `security_filter.engine`, with TF-IDF relevance-scored truncation when `tfidf_query_source` is set, with fallback chain if agent-referenced)
  10. **JSON minification** (final body compaction)
  11. **Response cache store** (if enabled, store completed SSE response)
  12. **MCP auto-save** (if agent has mcp.auto_save, async store of assistant response to MCP server)

### Best-Effort Pipeline

The content pipeline (steps 2–7) is **best-effort**: if any step fails (e.g., engine unreachable, Ollama down), the gateway logs a warning and proceeds with the original payload. This ensures the proxy never blocks or returns errors due to pipeline failures — the request always reaches an upstream provider. When `skip_on_engine_failure` is `true` (default), hard-limit payloads that fail engine summarization are forwarded unchanged instead of being truncated.

## Configuration Notes

- **JSON with Comments**: Configuration files support `//` and `/* */` comments
- **Model Registry**: Built-in models can be referenced as strings in agent configs; custom/local models use full object notation
- **Provider Registry**: All built-in providers are loaded automatically; user providers are merged on top
- **External Prompts**: System prompts can be inline (`system_prompt`) or external files (`system_prompt_file`) in `./prompts/` directory
- **Separate Engines**: `security_filter.engine` and `window.engine` can use different models/APIs, or share the same agent reference
- **Agent-as-Engine**: Engine fields accept a string (agent name) or inline object. Agent references inherit the agent's model list as a fallback chain
- **API Format Abstraction**: Supports `"ollama"` (native `/api/generate`) and `"openai"` (compatible `/v1/chat/completions`) formats
- **Auto-enable**: `security_filter.enabled` defaults to `true` when patterns are provided; `prefix_cache` and `compaction` auto-enable when sub-fields are set
- **max_tokens injection**: `max_tokens` is injected from per-model `MaxOutput` in the `ModelRegistry` when the client doesn't set it. Unknown models (not in registry) are not injected.
- **Provider Adapters**: Provider-specific wire format differences (auth, request/response mutation, error classification) are handled by the adapter system. See [`ADAPTERS.md`](ADAPTERS.md) for details.
- **Circuit Breaker**: Each agent+provider+model combination has an independent circuit breaker (Closed/Open/HalfOpen states). See [`ARCHITECTURE.md`](ARCHITECTURE.md#circuit-breaker) for the state machine.
- **Graceful Degradation**: When `skip_on_engine_failure` is `true`, the gateway operates normally even without a local Ollama instance. Secret redaction (Tier-0 regex) still runs; only the LLM-based summarization is skipped. When `tfidf_query_source` is set, Tier 3 payloads that reduce below `context_soft_limit` via TF-IDF scoring skip the engine call entirely.
- **TF-IDF Truncation**: When `tfidf_query_source` is set, Tier 3 uses a local TF-IDF algorithm (no external dependencies) to score content blocks by relevance to the user's query terms. This is a pure Go implementation using `strings.Fields` for tokenization and TF-IDF math for scoring. See `internal/pipeline/tfidf.go`.

## Configuration Validation

Before starting the gateway, validate your configuration and API keys:

```bash
CREDENTIALS_DIRECTORY=/path/to/creds ./nenya -config config.json -validate
```

This checks:
1. Ollama engine health (if `security_filter` is enabled and engine is `ollama`)
2. Provider API endpoint reachability
3. API key validity

## Secrets

API keys and client tokens are loaded from a JSON file via systemd credentials (`CREDENTIALS_DIRECTORY`). See [`SECRETS_FORMAT.md`](SECRETS_FORMAT.md) for the full format.
