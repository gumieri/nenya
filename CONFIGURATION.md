# Configuration Reference

Nenya reads its configuration from a JSON file (default: `config.json`). See [`example.config.json`](example.config.json) for a working example.

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
| Agents | `agents` | Named agent definitions with fallback chains and optional system prompts |
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
- **Tier 3** (truncation + engine): content above `context_hard_limit` runes

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `ratelimit_max_tpm` | int | `250000` | Max tokens per minute per upstream host (0 = disabled) |
| `ratelimit_max_rpm` | int | `15` | Max requests per minute per upstream host (0 = disabled) |
| `context_soft_limit` | int | `4000` | Runes below this pass through unmodified |
| `context_hard_limit` | int | `24000` | Runes above this are middle-out truncated before engine summarization |
| `truncation_strategy` | string | `"middle-out"` | Truncation method (`"middle-out"` only) |
| `keep_first_percent` | float | `15.0` | Percentage of content to keep from the start when truncating |
| `keep_last_percent` | float | `25.0` | Percentage of content to keep from the end when truncating |

## `security_filter`

Tier-0 regex-based secret redaction runs on every request, before any other pipeline step. Includes configurable engine for privacy filtering.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enabled` | bool | `true` | Enable/disable the filter. Defaults to `true` if `patterns` are provided but field omitted. |
| `patterns` | []string | (9 built-in) | Custom regex patterns. Replaces built-in patterns if set. Built-in patterns match: AWS keys, GitHub tokens, Google OAuth, sk- API keys, PEM private keys, AWS credential file lines, password/key assignments, Docker tokens, SendGrid keys. |
| `redaction_label` | string | `"[REDACTED]"` | Replacement string for matched secrets |
| `engine` | EngineConfig | (see below) | Engine configuration for privacy filtering |

### Engine Configuration (`engine`)

Both `security_filter.engine` and `window.engine` use the same `EngineConfig` structure:

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `provider` | string | `"ollama"` | Provider name from the `[providers]` registry. URL, auth, and API format are inherited from the provider definition. |
| `model` | string | `"qwen2.5-coder:7b"` | Model identifier for the engine |
| `system_prompt` | string | `""` | Inline system prompt. Highest priority. |
| `system_prompt_file` | string | `""` | Path to system prompt file (relative to config). Falls back to built-in prompt if empty. |
| `timeout_seconds` | int | `600` | Timeout for individual engine API calls |

**Prompt priority**: `system_prompt` (inline) > `system_prompt_file` > built-in default.

**Note**: `system_prompt_file` supports `%d` placeholder for window summarization (replaced with active message count).

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
| `mode` | string | `"summarize"` | `"summarize"` (engine) or `"truncate"` (hard cut) |
| `active_messages` | int | `6` | Number of recent messages to preserve unchanged |
| `trigger_ratio` | float | `0.8` | Trigger when tokens exceed `max_context * ratio` (0.0-1.0) |
| `summary_max_runes` | int | `4000` | Maximum length of the generated summary |
| `max_context` | int | `128000` | Context window size. Overridden by agent model `max_context` when routing through agents. |
| `engine` | EngineConfig | (see above) | Engine configuration for window summarization |

## `agents`

Named agent definitions with model fallback chains and optional system prompts. When a request specifies `model: "<agent_name>"`, the gateway routes through the agent's model list.

```json
{
  "agents": {
    "build": {
      "strategy": "fallback",
      "cooldown_seconds": 60,
      "system_prompt": "Reply with maximum brevity. Code only.",
      "models": [
        { "provider": "gemini", "model": "gemini-3.1-flash-lite-preview", "max_context": 128000 },
        { "provider": "deepseek", "model": "deepseek-reasoner", "max_context": 128000 }
      ]
    }
  }
}
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `strategy` | string | `"round-robin"` | `"round-robin"` or `"fallback"` |
| `cooldown_seconds` | int | `60` | Seconds to skip a model after a retryable error |
| `system_prompt` | string | `""` | Inline system prompt injected as the first message (only if no existing system message). |
| `system_prompt_file` | string | `""` | Path to system prompt file. Lower priority than `system_prompt`. |
| `models` | array | (required) | List of model entries to try in order |

Each model entry:

| Field | Type | Description |
|-------|------|-------------|
| `provider` | string | Provider name (must match a key in `providers` or a built-in) |
| `model` | string | Model identifier sent to the upstream API |
| `url` | string | (optional) Override provider URL for this specific model |
| `max_context` | int | Context window size for token budgeting and window compaction |

**System prompt injection**: If `system_prompt` or `system_prompt_file` is set, the prompt is injected as the first message in the array only when no existing system message is present.

## `providers`

Upstream LLM provider registry. Built-in providers are automatically loaded:

| Name | URL | Prefixes | Auth Style |
|------|-----|----------|------------|
| `gemini` | `https://generativelanguage.googleapis.com/v1beta/openai/chat/completions` | `gemini-` | `bearer+x-goog` |
| `deepseek` | `https://api.deepseek.com/chat/completions` | `deepseek-` | `bearer` |
| `zai` | `https://api.z.ai/v1/chat/completions` | `zai-`, `glm-` | `bearer` |
| `groq` | `https://api.groq.com/openai/v1/chat/completions` | `llama-`, `llama3-`, `mixtral-`, `whisper-` | `bearer` |
| `together` | `https://api.together.xyz/v1/chat/completions` | `meta-llama/`, `mistralai/`, `qwen/`, `together/` | `bearer` |
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

API keys are loaded from the secrets file via `provider_keys` (keyed by provider name). See [`SECRETS_FORMAT.md`](SECRETS_FORMAT.md).

### Gemini `auth_style: "bearer+x-goog"`

Gemini requires both `Authorization: Bearer <key>` and `x-goog-api-key: <key>` headers. The `bearer+x-goog` auth style sets both automatically.

### Gemini `extra_content` (Thought Signatures)

Gemini 3 models include `extra_content.google.thought_signature` in tool_calls responses. Nenya preserves this field (it is required for multi-turn function calling with Gemini 3) and adds the missing `index` field to comply with the OpenAI spec.

## Processing Pipeline Order

1. **Prefix cache optimizations** (pin system messages, sort tools)
2. **Agent system prompt injection** (if agent has prompt and no system message exists)
3. **Tier-0 regex redaction** (secret patterns via `security_filter`)
4. **Text compaction** (normalize, trim, collapse blanks)
5. **Window compaction** (if enabled and threshold exceeded)
6. **Engine interception** (3-tier last-message summarization using `security_filter.engine`)
7. **JSON minification** (final body compaction)

## Configuration Notes

- **JSON with Comments**: Configuration files support `//` and `/* */` comments
- **External Prompts**: System prompts can be inline (`system_prompt`) or external files (`system_prompt_file`) in `./prompts/` directory
- **Separate Engines**: `security_filter.engine` and `window.engine` can use different models/APIs
- **API Format Abstraction**: Supports `"ollama"` (native `/api/generate`) and `"openai"` (compatible `/v1/chat/completions`) formats
- **Auto-enable**: `security_filter.enabled` defaults to `true` when patterns are provided; `prefix_cache` and `compaction` auto-enable when sub-fields are set

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
