# Configuration Reference

Nenya reads its configuration from a JSON file or directory (default: `/etc/nenya/`). See [`PROVIDERS.md`](PROVIDERS.md) for the full provider reference.

When a **directory** is specified, all `*.json` files (excluding `secrets.json`) are loaded in alphabetical order and deep-merged. Map fields (`agents`, `providers`, `mcp_servers`) merge per-key; struct fields use last-file-wins. Defaults are applied once after the merge.

When a **file** is specified, only that file is loaded (single-file mode, unchanged behavior).

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

## Multi-File Configuration (Directory Mode)

When `-config` points to a directory (the default), Nenya loads all `*.json` files in sorted order and deep-merges them:

```
/etc/nenya/
├── 00-server.json          # server, governance, security_filter, compaction
├── 10-providers.json       # provider URL or auth overrides
├── 20-agents.json          # agent definitions
└── secrets.json            # EXCLUDED (loaded via systemd credential)
```

**Merge rules:**

| Field Type | Behavior |
|------------|----------|
| `agents` (map) | Per-key merge — later files add or override individual agents |
| `providers` (map) | Per-key merge — later files add or override individual providers |
| `mcp_servers` (map) | Per-key merge |
| `server`, `governance`, `security_filter`, etc. (struct) | Last file wins — if multiple files set the same field, the last one in alphabetical order takes precedence |

This lets you split configuration however makes sense for your deployment — e.g., separate files for server settings, provider credentials, and agent definitions managed by different teams.

## Hot Reload

```bash
systemctl reload nenya
```

- Reloads config from the same path used at startup
- Re-discovers model catalogs from all configured providers
- Validates config structure (patterns, enums) without pinging providers
- Preserves UsageTracker, Metrics, and ThoughtSignatureCache across reloads
- On validation failure: logs error, continues serving with old config

## `server`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `listen_addr` | string | `":8080"` | Bind address and port |
| `max_body_bytes` | int | `10485760` (10 MB) | Maximum incoming request body size |
| `log_level` | string | `"info"` | Log level: `"debug"`, `"info"`, `"warn"`, or `"error"`. The `-verbose` flag overrides this to `"debug"`. |

## `governance`

Unified configuration for context management, truncation, and rate limiting.

The interceptor implements a 3-tier pipeline for the last user message content, with limits derived from the target model's `max_context` (characters, not tokens). If the model has no `max_context`, fallback defaults of 4000/24000 are used.

- **Tier 1** (pass-through): content below `soft_limit` runes
- **Tier 2** (engine summarization): content between `soft_limit` and `hard_limit` runes
- **Tier 3** (truncation + engine): content above `hard_limit` runes. Truncation uses the strategy selected by `truncation_strategy`:
  - `"middle-out"` (default): positional — keeps first/last percentages, discards middle
  - When `tfidf_query_source` is set: **TF-IDF scoring** — splits content into blocks (paragraphs + code fences), scores each block's relevance to the user's prior messages or the start of the current message, and greedily keeps the most relevant blocks within budget. First/last blocks are pinned as a safety net. If TF-IDF reduces the payload below `soft_limit`, the engine call is skipped entirely (zero network overhead).

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `ratelimit_max_tpm` | int | `250000` | Max tokens per minute per upstream host (0 = disabled) |
| `ratelimit_max_rpm` | int | `15` | Max requests per minute per upstream host (0 = disabled) |
| `truncation_strategy` | string | `"middle-out"` | Truncation method. `"middle-out"` (positional) or any value — TF-IDF is activated by setting `tfidf_query_source` instead. |
| `keep_first_percent` | float | `15.0` | Percentage of blocks to pin from the start when truncating (safety net for both middle-out and TF-IDF) |
| `keep_last_percent` | float | `25.0` | Percentage of blocks to pin from the end when truncating (safety net for both middle-out and TF-IDF) |
| `tfidf_query_source` | string | `""` (disabled) | Enable TF-IDF relevance-scored truncation for Tier 3. `""` = disabled (use middle-out). `"prior_messages"` = use previous user messages as query terms. `"self"` = use first 500 runes of the massive message as query terms. When enabled, if TF-IDF reduces the payload below `soft_limit`, the engine call is skipped entirely. |
| `auto_context_skip` | bool | `false` | Automatically skip models that do not meet context requirements for the current request. When enabled, models with `max_context` smaller than the request's input token count are excluded from routing, preventing errors and improving latency. |
| `auto_reorder_by_latency` | bool | `false` | Dynamically sort targets based on historical response times. When enabled, targets are reordered by median latency (fastest first) with ±5% jitter to prevent thundering herd. Requires `infra.LatencyTracker` to be initialized. |
| `routing_strategy` | string | `""` (latency) | Routing strategy when `auto_reorder_by_latency` is enabled. `""` or `"latency"` = latency-only sorting. `"balanced"` = weighted scoring using latency, cost, capability matching, and per-model score bonus. |
| `routing_latency_weight` | float64 | `1.0` | Weight for latency normalization in balanced scoring (0.0-10.0). Higher = prioritize faster models. |
| `routing_cost_weight` | float64 | `0.0` | Weight for cost normalization in balanced scoring (0.0-10.0). Higher = prioritize cheaper models. |
| `max_cost_per_request` | float64 | `0` (disabled) | Maximum allowed cost in USD per request. 0 = no limit. Logged but not yet enforced. |
| `retryable_status_codes` | []int | `[429, 500, 502, 503, 504]` | HTTP status codes that trigger fallback to the next model in an agent chain. **Warning: setting this field REPLACES the built-in defaults entirely.** You must include all codes you want retryable (including the standard ones). Per-provider override available via `providers.<name>.retryable_status_codes` (provider-level replaces global for that provider). |

## `security_filter`

Tier-0 regex-based secret redaction runs on every request, before any other pipeline step. Includes configurable engine for privacy filtering and optional Shannon entropy detection for unknown high-entropy tokens.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enabled` | bool | `true` | Enable/disable the filter. Defaults to `true` if `patterns` are provided but field omitted. |
| `patterns` | []string | (9 built-in) | Custom regex patterns. Replaces built-in patterns if set. Built-in patterns match: AWS keys, GitHub tokens, Google OAuth, sk- API keys, PEM private keys, AWS credential file lines, password/key assignments, Docker tokens, SendGrid keys. |
| `redaction_label` | string | `"[REDACTED]"` | Replacement string for matched secrets |
| `output_enabled` | bool | `false` | Enable stream output filtering (secret redaction and execution policy blocking on responses) |
| `output_window_chars` | int | `4096` | Sliding window size (in chars) for cross-chunk pattern matching in output streams |
| `skip_on_engine_failure` | bool | `true` | When the engine (Ollama/cloud) is unreachable, skip summarization and forward the original payload. If `false`, hard-limit payloads are truncated even when the engine fails. |
| `entropy_enabled` | bool | `false` | Enable Shannon entropy-based secret detection. Catches high-entropy tokens that don't match regex patterns (JWTs, opaque API keys, base64 credentials). |
| `entropy_threshold` | float64 | `4.5` | Shannon entropy threshold in bits/character. Tokens above this value are redacted. English text: ~3.5, hex secrets: ~4.0, base64 tokens: ~5.5, random API keys: ~4.5-5.5. |
| `entropy_min_token` | int | `20` | Minimum token length (in characters) to evaluate for entropy. Shorter tokens are skipped to reduce false positives. |
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
| `timeout_seconds` | int | `60` | Timeout for individual engine API calls. Falls back to the provider's `timeout_seconds` if not explicitly set, then to hard default `60`. |

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
| `prune_stale_tools` | bool | `false` | Compact old assistant+tool response pairs into summary placeholders |
| `tool_protection_window` | int | `4` | Number of most recent messages to protect from tool call pruning |
| `prune_thoughts` | bool | `false` | Strip reasoning blocks from assistant messages to save context tokens |

Compaction runs after redaction, before engine interception. JSON minify runs at the very end of the pipeline.

### Stale Tool Call Pruning

When `prune_stale_tools` is enabled, the gateway scans the messages array backwards (from oldest to newest) for completed tool execution pairs: an `assistant` message containing `tool_calls`, immediately followed by one or more `tool` messages with the results. When such a pair is found outside the protection window, both the assistant message and its tool responses are replaced with a single summary message:

```
[System] Tool 'tool_name' was executed previously. Result compacted to save context window.
```

The tool name is extracted from the first tool call's `function.name` field. If unavailable, the `tool_call_id` is used as a fallback.

**Protection window**: The last `tool_protection_window` messages (default: 4) are never modified, preserving the LLM's immediate reasoning context including the most recent tool calls.

**Safety**: Orphaned tool calls (assistant with `tool_calls` but missing corresponding `tool` response, e.g., due to stream interruption) are left untouched. The pruning is skipped entirely for IDE clients.

### Thought Pruning

When `prune_thoughts` is enabled, the gateway strips reasoning blocks from all `assistant` messages in the conversation history. This targets `<think.../think>` tags used by reasoning models (DeepSeek, OpenRouter, Groq, Gemini):

**Text tag pruning:** Inside the `content` string, the gateway looks for the `<think` opening tag and `</think` closing tag. When found:
- Both tags and everything between them are removed.
- The removed block is replaced with `[Reasoning pruned by gateway]`.
- If the opening tag exists but the closing tag is missing (stream interruption), everything from `<think` to the end of the string is replaced.
- Multiple reasoning blocks in a single message are all pruned.

The structured `reasoning_content` field is **not** stripped by thought pruning. It is preserved in the shared pipeline and stripped per-target during request sanitization — only for providers that do not support reasoning.

Uses `strings.Index` (not regex) for zero-allocation scanning of large payloads.

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

### Dynamic Model Discovery (Regex Patterns)

Model entries can use regex patterns to dynamically match against the discovery catalog. This enables auto-updating agent model lists when new models appear at providers without manual config changes.

```json
{
  "agents": {
    "all-flash": {
      "strategy": "round-robin",
      "models": [
        { "provider_rgx": "^gemini$", "model_rgx": "^gemini-2\\.5-flash" },
        { "provider": "anthropic", "model_rgx": "^claude-3\\.5-sonnet" }
      ]
    },
    "mixed": {
      "strategy": "fallback",
      "models": [
        "claude-sonnet-4-20250514",
        { "provider_rgx": "^deepseek$", "model": "deepseek-chat" }
      ]
    }
  }
}
```

**Precedence rules:**

- `provider` + `model` — static entry, used as-is
- `provider` + `model_rgx` — static provider, regex model from that provider's catalog
- `provider_rgx` + `model` — regex provider match, static model name
- `provider_rgx` + `model_rgx` — full regex, expands to all matching catalog entries

Static fields always win over regex when both are present on the same key.

> **Note:** If both static and regex fields are set on the same entry (e.g., both `provider` and `provider_rgx`), a warning is logged at startup. The dynamic field takes precedence — but the config is likely a mistake.

**Resolution:**

1. For each model entry with `provider_rgx` or `model_rgx`, expand against the discovery catalog
2. Static entries (no regex) pass through unchanged
3. Results are cached per-agent keyed on catalog timestamp and regex hash
4. If discovery catalog is unavailable, static entries are used as-is

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `strategy` | string | `"round-robin"` | `"round-robin"` or `"fallback"` |
| `cooldown_seconds` | int | `60` | Seconds to skip a model after a retryable error |
| `failure_threshold` | int | `5` | Circuit breaker: consecutive failures before tripping to Open state |
| `success_threshold` | int | `1` | Circuit breaker: consecutive successes in HalfOpen to recover to Closed state |
| `max_retries` | int | `0` | Cap on retry attempts per request (0 = unlimited) |
| `system_prompt` | string | `""` | Inline system prompt injected as the first message (only if no existing system message, unless `force_system_prompt` is true). |
| `system_prompt_file` | string | `""` | Path to system prompt file. Lower priority than `system_prompt`. |
| `force_system_prompt` | bool | `false` | If true, always inject the agent's system prompt as the first message, even if the request already contains a system message. The client's system message is preserved after the forced prompt. |
| `mcp` | object | (none) | Optional Model Context Protocol server integration. See [MCP Integration](MCP_INTEGRATION.md) for details. |
| `models` | array | (required) | List of model entries (strings or objects) to try in order. Strings are looked up from the model registry. Objects can be static (`provider`+`model`) or dynamic (`provider_rgx`/`model_rgx` for catalog expansion). |
| `budget_limit_usd` | float64 | `0` (disabled) | Per-agent cumulative budget limit in USD. 0 = no limit. Logged but not yet enforced. |

Each model entry (object form):

| Field | Type | Description |
|-------|------|-------------|
| `provider` | string | Provider name (must match a key in `providers` or a built-in) |
| `model` | string | Model identifier sent to the upstream API |
| `url` | string | (optional) Override provider URL for this specific model |
| `max_context` | int | Context window size for token budgeting and window compaction |
| `required_capabilities` | []string | (optional) Required model capabilities. Models without matching capabilities are skipped. Values: `"vision"`, `"tool_calls"`, `"reasoning"`, `"content_arrays"`, `"stream_options"`, `"auto_tool_choice"`. Capabilities are determined from provider API responses, static registry entries, or heuristic model name inference. |

**String shorthand**: If the entry is a string, it must exist in the built-in Model Registry. The registry resolves `provider` and `max_context` automatically. If the model is not found, configuration loading fails with an error.

**System prompt injection**: If `system_prompt` or `system_prompt_file` is set, the prompt is injected as the first message in the array only when no existing system message is present. If `force_system_prompt` is true, the agent's system prompt is always injected first, even if the request already contains a system message (the client's system message is preserved after the forced prompt).

## `providers`

Upstream LLM provider registry. Built-in providers are automatically loaded from the internal Provider Registry:

| Name | URL | Prefixes | Auth Style |
|------|-----|----------|------------|
| `gemini` | `https://generativelanguage.googleapis.com/v1beta/openai/chat/completions` | `gemini-` | `bearer+x-goog` |
| `deepseek` | `https://api.deepseek.com/chat/completions` | `deepseek-` | `bearer` |
| `zai` | `https://api.z.ai/api/paas/v4/chat/completions` | `glm-` | `bearer` |
| `groq` | `https://api.groq.com/openai/v1/chat/completions` | (none) | `bearer` |
| `together` | `https://api.together.xyz/v1/chat/completions` | `together/` | `bearer` |
| `anthropic` | `https://api.anthropic.com/v1/messages` | `claude-` | `anthropic` |
| `mistral` | `https://api.mistral.ai/v1/chat/completions` | `mistral-`, `codestral-`, `devstral-` | `bearer` |
| `xai` | `https://api.x.ai/v1/chat/completions` | `grok-` | `bearer` |
| `perplexity` | `https://api.perplexity.ai/chat/completions` | (none) | `bearer` |
| `cohere` | `https://api.cohere.com/v1/chat/completions` | (none) | `bearer` |
| `deepinfra` | `https://api.deepinfra.com/v1/chat/completions` | (none) | `bearer` |
| `openrouter` | `https://openrouter.ai/api/v1/chat/completions` | (none) | `bearer` |
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
    },
    "ollama": {
      "url": "http://127.0.0.1:11434/v1/chat/completions",
      "timeout_seconds": 300
    }
  }
}
```

| Field | Type | Description |
|-------|------|-------------|
| `url` | string | Upstream chat completions endpoint |
| `route_prefixes` | []string | Model name prefixes that route to this provider |
| `auth_style` | string | `"bearer"`, `"bearer+x-goog"` (Gemini), `"anthropic"` (Anthropic), `"azure"` (Azure OpenAI), or `"none"` (Ollama) |
| `timeout_seconds` | int | Per-provider timeout in seconds. For the `ollama` provider, sets the HTTP transport's `ResponseHeaderTimeout` (time-to-first-byte). For other providers, applies as a request context timeout on `/v1/embeddings` and `/v1/responses` endpoints. Also used as a fallback for engine calls (`security_filter.engine`, `window.engine`) when the engine's own `timeout_seconds` is not explicitly set. Default: `30` (transport-level). |
| `retryable_status_codes` | []int | Provider-level override for retryable status codes. **Replaces** both global and built-in defaults for this provider. If not set, falls back to `governance.retryable_status_codes`, then built-in defaults `[429, 500, 502, 503, 504]`. |

**Note**: The `BaseURL` field is automatically derived from `url` by stripping the path component. This is used by the `/proxy/{provider}/*` passthrough endpoint to construct arbitrary provider URLs. For example, if `url` is `https://api.anthropic.com/v1/messages`, the derived `BaseURL` is `https://api.anthropic.com`, allowing passthrough to `/proxy/anthropic/v1/models`.

API keys are loaded from the secrets file via `provider_keys` (keyed by provider name). See [`SECRETS_FORMAT.md`](SECRETS_FORMAT.md).

### Gemini `auth_style: "bearer+x-goog"`

Gemini requires both `Authorization: Bearer <key>` and `x-goog-api-key: <key>` headers. The `bearer+x-goog` auth style sets both automatically.

### Anthropic `auth_style: "anthropic"`

Anthropic uses `x-api-key` header instead of `Authorization: Bearer`. The adapter performs full OpenAI↔Anthropic format conversion automatically — send standard OpenAI-format requests and Nenya handles the rest.

### Azure `auth_style: "azure"`

Azure OpenAI uses `api-key` header instead of `Authorization: Bearer`.

### Gemini `extra_content` (Thought Signatures)

Gemini 3 models include `extra_content.google.thought_signature` in tool_calls responses. Nenya preserves this field (it is required for multi-turn function calling with Gemini 3) and adds the missing `index` field to comply with the OpenAI spec.

## Model Registry

Built-in models that can be referenced by string shorthand in agent `models` arrays. Each entry maps to a provider and includes a default `max_context`.

| Model | Provider | Max Context | Score Bonus |
|-------|----------|-------------|-------------|
| `gemini-3.1-flash-lite-preview` | `gemini` | 128000 | 0.1 |
| `gemini-3.1-flash` | `gemini` | 128000 | 0.1 |
| `gemini-2.5-flash-lite` | `gemini` | 128000 | 0.1 |
| `gemini-2.5-flash` | `gemini` | 128000 | 0.1 |
| `deepseek-reasoner` | `deepseek` | 128000 | 0.2 |
| `deepseek-chat` | `deepseek` | 128000 | 0.2 |
| `glm-5.1` | `zai` | 128000 | 0.0 |
| `glm-5-turbo` | `zai` | 128000 | 0.0 |
| `glm-5v-turbo` | `zai` | 128000 | 0.0 |
| `glm-5` | `zai` | 128000 | 0.0 |
| `glm-4.7` | `zai` | 128000 | 0.0 |
| `glm-4.7-flash` | `zai` | 128000 | 0.0 |
| `glm-4.7-flashx` | `zai` | 128000 | 0.0 |
| `glm-4.6` | `zai` | 128000 | 0.0 |
| `glm-4.6v` | `zai` | 128000 | 0.0 |
| `glm-4.5` | `zai` | 128000 | 0.0 |
| `glm-4.5-air` | `zai` | 128000 | 0.0 |
| `glm-4.5-flash` | `zai` | 128000 | 0.0 |
| `glm-4.5v` | `zai` | 128000 | 0.0 |
| `claude-opus-4-5` | `anthropic` | 200000 | 0.3 |
| `claude-opus-4-0` | `anthropic` | 200000 | 0.3 |
| `claude-sonnet-4-5` | `anthropic` | 200000 | 0.2 |
| `claude-sonnet-4-0` | `anthropic` | 200000 | 0.2 |
| `claude-haiku-4-5` | `anthropic` | 200000 | 0.1 |
| `claude-3-7-sonnet-20250219` | `anthropic` | 200000 | 0.2 |
| `claude-3-5-sonnet-20241022` | `anthropic` | 200000 | 0.2 |
| `claude-3-5-haiku-latest` | `anthropic` | 200000 | 0.1 |
| `mistral-large-latest` | `mistral` | 262144 | 0.0 |
| `mistral-small-latest` | `mistral` | 256000 | 0.0 |
| `mistral-medium-latest` | `mistral` | 128000 | 0.0 |
| `codestral-latest` | `mistral` | 256000 | 0.0 |
| `devstral-medium-latest` | `mistral` | 262144 | 0.0 |
| `magistral-medium-latest` | `mistral` | 128000 | 0.0 |
| `pixtral-large-latest` | `mistral` | 128000 | 0.0 |
| `grok-4` | `xai` | 256000 | 0.0 |
| `grok-4-fast` | `xai` | 2000000 | 0.0 |
| `grok-3` | `xai` | 131072 | 0.0 |
| `grok-3-fast` | `xai` | 131072 | 0.0 |
| `grok-3-mini` | `xai` | 131072 | 0.0 |
| `sonar-pro` | `perplexity` | 200000 | 0.0 |
| `sonar-reasoning-pro` | `perplexity` | 128000 | 0.1 |
| `sonar-deep-research` | `perplexity` | 128000 | 0.1 |
| `sonar` | `perplexity` | 128000 | 0.0 |
| `nemotron-3-super` | `nvidia_free` | 4000 | 0.0 |
| `qwen-3.6-plus` | `qwen_free` | 8000 | 0.0 |
| `minimax-m2.5` | `minimax_free` | 8000 | 0.0 |
| `llama-3.3-70b-versatile` | `groq` | 131072 | 0.0 |
| `mixtral-8x7b-32768` | `groq` | 32768 | 0.0 |
| `llama-3.1-405b-instruct` | `sambanova` | 128000 | 0.1 |
| `llama-3.3-70b` | `cerebras` | 8192 | 0.0 |
| `gpt-4o` | `github` | 128000 | 0.1 |
| `phi-3.5-mini-instruct` | `github` | 128000 | 0.0 |
| `qwen2.5-72b-turbo` | `together` | 32768 | 0.0 |

Models not in this registry (e.g., local Ollama models, custom endpoints) must be specified as full objects with explicit `provider` and `model` fields.

## Model Discovery

Nenya dynamically fetches model catalogs from upstream providers at startup and on SIGHUP reload. This enables automatic discovery of custom models (e.g., Ollama) and reduces the need for manual registry updates.

### Discovery Process

1. **Startup/Reload** — For each configured provider with an API key, fetch `/v1/models` in parallel (10s timeout per provider)
2. **Provider-specific parsing** — Each provider has a dedicated parser for its response format
3. **Three-tier merge** — Discovered models are merged with static registry (config overrides take precedence)
4. **Catalog update** — The merged catalog is used for all subsequent model resolution

### Three-Tier Model Resolution

When resolving a model (for routing, `/v1/models` catalog, or `max_tokens` injection):

| Priority | Source | Description |
|----------|--------|-------------|
| 1 | Config overrides | Agent model entries with explicit `provider`, `max_context`, or `max_output` fields |
| 2 | Discovered models | Models fetched from provider `/v1/models` endpoints at startup/reload |
| 3 | Static registry | Built-in ModelRegistry fallback for known models |

This allows:
- Custom local models (Ollama) to be discovered automatically
- Provider-specific overrides without code changes
- Graceful fallback when discovery fails (static registry still works)

### Security Hardening

The discovery package enforces strict security boundaries:

- **Response body limits** — 10 MB max per provider response (DoS protection)
- **JSON decode limits** — 10 MB max with `DisallowUnknownFields` (malformed JSON rejection)
- **Content-type validation** — Only `application/json` responses are parsed
- **Model ID sanitization** — Max 256 chars, printable characters only (XSS prevention)
- **Per-provider timeouts** — 10s context timeout per fetch (no hanging)
- **Panic recovery** — Goroutines have defer/recover to prevent crashes
- **Auth header injection** — Gemini uses `x-goog-api-key` header (not query params)
- **Shared HTTP client** — Reused with proper TLS timeouts (no resource leaks)

### Graceful Degradation

If discovery fails for any provider:
- The provider is skipped with a warning log
- Static registry models for that provider still work
- Other providers' discovered models are still used
- `/v1/models` endpoint shows only successfully discovered models

### Debug Logging

When `server.log_level` is set to `"debug"`, the discovery process logs detailed information:

- **Per-provider fetch results**: `"discovered models"` log shows model count per provider
- **Full catalog dump**: `"discovery catalog"` debug log shows all discovered models with:
  - Provider name
  - Model ID
  - Context window (`ctx=`)
  - Max output tokens (`out=`)
  - Capabilities (`caps=`): comma-separated list (e.g., `vision,tools,reasoning`)
  - Pricing (`pricing=`): `input_per_1m/output_per_1m` if available, or `false`

Example debug log output:
```
DEBUG discovery catalog providers=[anthropic:3 gemini:5 openrouter:42] models=[
  anthropic/claude-opus-4-5 ctx=200000 out=64000 caps=vision,tools,reasoning pricing=15.00/75.00
  gemini/gemini-2.5-flash ctx=128000 out=8192 caps=vision,tools,reasoning pricing=0.08/0.30
  openrouter/llama-3.3-70b ctx=131072 out=8192 caps=tools pricing=0.59/0.79
]
```

### `/v1/models` Endpoint

The `/v1/models` catalog endpoint returns actual discovered models instead of wildcards:

```json
{
  "object": "list",
  "data": [
    {
      "id": "gemini-2.5-flash",
      "object": "model",
      "owned_by": "google",
      "context_window": 128000,
      "max_tokens": 8192
    },
    {
      "id": "deepseek-chat",
      "object": "model",
      "owned_by": "deepseek",
      "context_window": 128000,
      "max_tokens": 4096
    }
  ]
}
```

## Auto-Agents

When `discovery.auto_agents` is enabled, Nenya automatically generates agent definitions from discovered models. These agents group models by capability and context size, providing convenient routing targets without manual configuration.

### Usage

```json
{ "model": "auto_reasoning" }
```

### Categories

| Agent | Filter | Strategy | Description |
|-------|--------|----------|-------------|
| `auto_fast` | ≤32k context, ≤4k output | round-robin | Low-latency models |
| `auto_reasoning` | reasoning capability + ≥128k context | fallback | Chain-of-thought models |
| `auto_vision` | vision capability | round-robin | Image analysis models |
| `auto_tools` | tool_calls capability | round-robin | Function calling models |
| `auto_large` | ≥200k context | fallback | Long context models |
| `auto_balanced` | 32k–128k context | round-robin | General purpose models |
| `auto_coding` | tool_calls + coding model prefix | fallback | Code-optimized models |

### Per-Category Configuration

```json
{
  "discovery": {
    "enabled": true,
    "auto_agents": true,
    "auto_agents_config": {
      "fast":      { "enabled": true },
      "reasoning": { "enabled": true },
      "coding":    { "enabled": true }
    }
  }
}
```

When `auto_agents_config` is present, only explicitly enabled categories are generated. When omitted, all categories are enabled (default).

### User Override

User-defined agents in the `agents` config section take precedence. If you define an agent named `auto_reasoning`, your definition replaces the auto-generated one.

### Capability Resolution

Auto-agent filters use model-level metadata only. Capabilities are inferred from model name patterns (e.g., `claude-` → vision+tool_calls, `gemini-2` → vision+tool_calls+reasoning). Models without matching capability rules are excluded from capability-based agents.

### Example Log Output

Auto-agent logs are emitted at debug level. Enable debug logging with `server.log_level: "debug"` or the `-verbose` flag:

```
DEBUG generated auto-agent agent=auto_reasoning description="Reasoning models with large context windows (≥128k context, supports reasoning)" strategy=fallback models=[claude-opus-4-5 deepseek-v4-pro gemini-2.5-flash]
DEBUG generated auto-agent agent=auto_coding description="Code-optimized models with tool calling capability" strategy=fallback models=[codestral-latest deepseek-v4-pro qwen2.5-72b-turbo]
DEBUG auto-agents summary total_agents=2 agents=[auto_coding auto_reasoning]
```

## Balanced Routing

When `auto_reorder_by_latency` is enabled and `routing_strategy` is `"balanced"`, targets are scored using a multi-dimensional formula:

```
score = (latency_normalized * latency_weight)
      - (cost_normalized * cost_weight)
      + model.score_bonus
      + capability_boost
```

- **latency_normalized**: `(maxLat - modelLatency) / (maxLat - minLat)` — higher = faster
- **cost_normalized**: `(modelCost - minCost) / (maxCost - minCost)` — lower = cheaper
- **score_bonus**: Per-model override from static registry or discovered metadata
- **capability_boost**: +0.1 per matching capability (tool_calls, reasoning, vision, content_arrays), -0.1 per mismatched capability

### Cost Tracking

When the OpenRouter provider is configured, pricing data is fetched from its `/v1/models` endpoint at startup. Per-request costs are calculated from usage data (input/output tokens) using `PricingEntry.CalculateCost()` and recorded in `CostTracker` (microUSD internal precision).

Cost data is also exposed via the `/statsz` endpoint and in the `/v1/models` response.

## `/v1/models` Response Fields

The model catalog endpoint now includes capability and pricing metadata when available:

```json
{
  "id": "claude-sonnet-4-5",
  "object": "model",
  "owned_by": "anthropic",
  "context_window": 200000,
  "max_tokens": 8192,
  "supports_vision": true,
  "supports_tool_calls": true,
  "supports_reasoning": false,
  "input_cost_per_1m": 3.0,
  "output_cost_per_1m": 15.0
}
```

## Processing Pipeline Order

  1. **Response cache lookup** (if enabled, bypass entire pipeline on hit)
  2. **MCP auto-search** (if agent has mcp.auto_search, query MCP server and inject as system message)
  3. **MCP tool injection** (if agent has MCP servers, inject tools as OpenAI function tools + system prompt)
  4. **Prefix cache optimizations** (pin system messages, sort tools — includes MCP tools)
  5. **Agent system prompt injection** (if agent has prompt and no system message exists)
   6. **Tier-0 regex redaction** (secret patterns via `security_filter`)
   6b. **Shannon entropy redaction** (high-entropy token detection via `security_filter.entropy_enabled`, runs after regex)
   7. **Text compaction** (normalize, trim, collapse blanks)
   8. **Stale tool call pruning** (if `prune_stale_tools` enabled, compact old assistant+tool pairs)
   9. **Thought pruning** (if `prune_thoughts` enabled, strip reasoning blocks from assistant messages)
   10. **Window compaction** (if enabled and threshold exceeded)
   11. **Engine interception** (3-tier last-message summarization using `security_filter.engine`, with TF-IDF relevance-scored truncation when `tfidf_query_source` is set, with fallback chain if agent-referenced)
   12. **JSON minification** (final body compaction)
   13. **Response cache store** (if enabled, store completed SSE response)
   14. **MCP auto-save** (if agent has mcp.auto_save, async store of assistant response to MCP server)

### Best-Effort Pipeline

The content pipeline (steps 2–9) is **best-effort**: if any step fails (e.g., engine unreachable, Ollama down), the gateway logs a warning and proceeds with the original payload. This ensures the proxy never blocks or returns errors due to pipeline failures — the request always reaches an upstream provider. When `skip_on_engine_failure` is `true` (default), hard-limit payloads that fail engine summarization are forwarded unchanged instead of being truncated.

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
- **Capability-Based Routing**: When model entries include `required_capabilities`, models without matching metadata are skipped during target building. Capabilities are inferred from provider responses, static registry entries, or heuristic model name patterns (e.g., `claude-` → vision+tool_calls, `gemini-2` → vision+tool_calls+reasoning).
- **Circuit Breaker**: Each agent+provider+model combination has an independent circuit breaker (Closed/Open/HalfOpen states). See [`ARCHITECTURE.md`](ARCHITECTURE.md#circuit-breaker) for the state machine.
- **Graceful Degradation**: When `skip_on_engine_failure` is `true`, the gateway operates normally even without a local Ollama instance. Secret redaction (Tier-0 regex) still runs; only the LLM-based summarization is skipped. When `tfidf_query_source` is set, Tier 3 payloads that reduce below `soft_limit` via TF-IDF scoring skip the engine call entirely.
- **TF-IDF Truncation**: When `tfidf_query_source` is set, Tier 3 uses a local TF-IDF algorithm (no external dependencies) to score content blocks by relevance to the user's query terms. This is a pure Go implementation using `strings.Fields` for tokenization and TF-IDF math for scoring. See `internal/pipeline/tfidf.go`.

## Configuration Validation

Validate your configuration without starting the gateway:

```bash
./nenya -validate
```

This checks:
1. Ollama engine health (if `security_filter` is enabled and engine is `ollama`)
2. Provider API endpoint reachability
3. API key validity
4. Model registry integrity (validates every `ModelRegistry` entry has a non-empty provider and non-negative limits)

> **Note**: On hot reload (`systemctl reload nenya`), only config structure is validated — provider reachability checks are skipped to prevent transient issues from blocking reloads.

## Secrets

API keys and client tokens are loaded from a JSON file via systemd credentials (`CREDENTIALS_DIRECTORY`). See [`SECRETS_FORMAT.md`](SECRETS_FORMAT.md) for the full format.
