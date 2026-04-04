# Configuration Reference

Nenya reads its configuration from a JSON file (default: `config.json`). See [`example.config.json`](example.config.json) for a working example.

## Top-Level Sections

| Section | JSON key | Description |
|---------|----------|-------------|
| Server | `server` | Listen address, body limits, token estimation |
| Interceptor | `interceptor` | 3-tier payload pipeline thresholds |
| Ollama | `ollama` | Local Ollama connection for summarization |
| Rate Limit | `ratelimit` | Per-host RPM/TPM limits |
| Filter | `filter` | Tier-0 regex secret redaction |
| Prefix Cache | `prefix_cache` | Prompt cache alignment optimizations |
| Compaction | `compaction` | Text compaction (whitespace, blank lines, JSON) |
| Window | `window` | Sliding window conversation compaction |
| Agents | `agents` | Named agent definitions with fallback chains |
| Providers | `providers` | Upstream provider registry |

## `server`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `listen_addr` | string | `":8080"` | Bind address and port |
| `max_body_bytes` | int | `10485760` (10 MB) | Maximum incoming request body size |
| `token_ratio` | float | `4.0` | Characters (runes) per token for estimation. Used to approximate token counts without a tokenizer. Adjust based on your model's tokenization. |

## `interceptor`

The interceptor implements a 3-tier pipeline for the last user message content:

- **Tier 1** (pass-through): content below `soft_limit` runes
- **Tier 2** (Ollama summarization): content between `soft_limit` and `hard_limit` runes
- **Tier 3** (truncation + Ollama): content above `hard_limit` runes

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `soft_limit` | int | `4000` | Runes below this pass through unmodified |
| `hard_limit` | int | `24000` | Runes above this are middle-out truncated before Ollama |
| `truncation_strategy` | string | `"middle-out"` | Truncation method (`"middle-out"` only) |
| `keep_first_percent` | float | `15.0` | Percentage of content to keep from the start when truncating |
| `keep_last_percent` | float | `25.0` | Percentage of content to keep from the end when truncating |

## `ollama`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `url` | string | `"http://127.0.0.1:11434/api/generate"` | Ollama generate API endpoint |
| `model` | string | `"qwen2.5-coder:7b"` | Local model used for summarization/redaction |
| `system_prompt` | string | (built-in) | System prompt for the interceptor Ollama call |
| `timeout_seconds` | int | `600` | Timeout for individual Ollama API calls |

## `ratelimit`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `max_tpm` | int | `250000` | Max tokens per minute per upstream host (0 = disabled) |
| `max_rpm` | int | `15` | Max requests per minute per upstream host (0 = disabled) |

## `filter`

Tier-0 regex-based secret redaction runs on every request, before any other pipeline step.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enabled` | bool | `true` | Enable/disable the filter |
| `patterns` | []string | (9 built-in) | Custom regex patterns. Replaces built-in patterns if set. Built-in patterns match: AWS keys, GitHub tokens, Google OAuth, sk- API keys, PEM private keys, AWS credential file lines, password/key assignments, Docker tokens, SendGrid keys. |
| `redaction_label` | string | `"[REDACTED]"` | Replacement string for matched secrets |

## `prefix_cache`

Optimizations to improve upstream provider prefix cache hit rates by stabilizing the prompt structure.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enabled` | bool | `true` | Master toggle |
| `pin_system_first` | bool | `true` | Reorder all `system` role messages to the top of the messages array |
| `stable_tools` | bool | `true` | Sort `tools[]` array by `function.name` for deterministic ordering |
| `skip_redaction_on_system` | bool | `true` | Skip Tier-0 regex redaction on system messages to preserve prefix byte-identity |

## `compaction`

Text compaction applied to all message content (both string and multi-part content arrays).

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enabled` | bool | `true` | Master toggle |
| `normalize_line_endings` | bool | `true` | Convert CRLF to LF |
| `trim_trailing_whitespace` | bool | `true` | Remove trailing spaces/tabs from each line |
| `collapse_blank_lines` | bool | `true` | Collapse runs of 3+ blank lines to max 2 |
| `json_minify` | bool | `true` | Minify the final JSON body with `json.Compact` |

Compaction runs after redaction, before Ollama interception. JSON minify runs at the very end of the pipeline.

## `window`

Sliding window conversation compaction for long conversations. When the estimated token count exceeds `max_context * trigger_ratio`, older messages are summarized (or truncated) and replaced with a single system summary message.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enabled` | bool | `false` | Master toggle (off by default) |
| `mode` | string | `"summarize"` | `"summarize"` (Ollama) or `"truncate"` (hard cut) |
| `active_messages` | int | `6` | Number of recent messages to preserve unchanged |
| `trigger_ratio` | float | `0.8` | Trigger when tokens exceed `max_context * ratio` (0.0-1.0) |
| `summary_max_runes` | int | `4000` | Maximum length of the generated summary |
| `max_context` | int | `128000` | Context window size. Overridden by agent model `max_context` when routing through agents. |

## `agents`

Named agent definitions with model fallback chains. When a request specifies `model: "<agent_name>"`, the gateway routes through the agent's model list.

```json
{
  "agents": {
    "my-agent": {
      "strategy": "round-robin",
      "cooldown_seconds": 300,
      "models": [
        {
          "provider": "gemini",
          "model": "gemini-3-flash",
          "max_context": 1000000
        },
        {
          "provider": "deepseek",
          "model": "deepseek-chat",
          "max_context": 64000
        }
      ]
    }
  }
}
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `strategy` | string | `"round-robin"` | `"round-robin"` or `"fallback"` |
| `cooldown_seconds` | int | `60` | Seconds to skip a model after a retryable error |
| `models` | array | (required) | List of model entries to try in order |

Each model entry:

| Field | Type | Description |
|-------|------|-------------|
| `provider` | string | Provider name (must match a key in `providers` or a built-in) |
| `model` | string | Model identifier sent to the upstream API |
| `url` | string | (optional) Override provider URL for this specific model |
| `max_context` | int | Context window size for token budgeting and window compaction |

## `providers`

Upstream LLM provider registry. Built-in providers are automatically loaded:

| Name | Prefixes | Auth Style |
|------|----------|------------|
| `gemini` | `gemini-` | `bearer+x-goog` |
| `deepseek` | `deepseek-` | `bearer` |
| `zai` | `zai-`, `glm-` | `bearer` |
| `groq` | `llama-`, `llama3-`, `mixtral-`, `whisper-` | `bearer` |
| `together` | `meta-llama/`, `mistralai/`, `qwen/`, `together/` | `bearer` |
| `ollama` | (none) | `none` |

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
| `auth_style` | string | `"bearer"`, `"bearer+x-goog"` (Gemini), or `"none"` |

API keys are loaded from the secrets file via `provider_keys` (keyed by provider name). See [`SECRETS_FORMAT.md`](SECRETS_FORMAT.md).

## Processing Pipeline Order

1. **Prefix cache optimizations** (pin system messages, sort tools)
2. **Tier-0 regex redaction** (secret patterns)
3. **Text compaction** (normalize, trim, collapse blanks)
4. **Window compaction** (if enabled and threshold exceeded)
5. **Ollama interception** (3-tier last-message summarization)
6. **JSON minification** (final body compaction)

## Secrets

API keys and client tokens are loaded from a JSON file via systemd credentials (`CREDENTIALS_DIRECTORY`). See [`SECRETS_FORMAT.md`](SECRETS_FORMAT.md) for the full format.
