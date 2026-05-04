# Nenya Configuration Reference

Nenya is configured via JSON files. The config root directory can contain either a single
`config.json` file or a `config.d/` directory with multiple `*.json` files (merged in sorted
order). Config files support `//` line comments and `/* */` block comments.

## Top-Level Sections

| Section | Required | Description |
|---------|----------|-------------|
| `server` | no | HTTP server settings (listen addr, body limits) |
| `context` | no | Context management (truncation, TF-IDF, routing behavior) |
| `governance` | no | Rate limits, retries, routing policy |
| `bouncer` | no | Content interception, PII redaction, summarization engine |
| `window` | no | Context window compaction settings |
| `prefix_cache` | no | Prefix caching optimizations |
| `compaction` | no | Payload compaction (minify, prune, trim) |
| `response_cache` | no | Response caching (LRU) |
| `discovery` | no | Dynamic model discovery from upstream providers |
| `agents` | yes | Named agent configurations with model lists |
| `providers` | yes | Upstream provider API endpoints |
| `mcp_servers` | no | MCP server connections for tool integration |

## `server`

```json
{
  "server": {
    "listen_addr": ":8080",
    "max_body_bytes": 10485760,
    "user_agent": "nenya/1.0",
    "log_level": "info",
    "secure_memory_required": true
  }
}
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `listen_addr` | string | `":8080"` | HTTP listen address |
| `max_body_bytes` | int | `10485760` | Maximum request body size (10 MB) |
| `user_agent` | string | `"nenya/1.0"` | User-Agent header for upstream requests |
| `log_level` | string | `"info"` | Log level: `debug`, `info`, `warn`, `error` |
| `secure_memory_required` | bool | `true` | Require secure memory for secrets (mlock) |

## `context`

Context management settings control how Nenya prepares the request payload before forwarding
upstream. This includes truncation strategies and TF-IDF relevance scoring.

```json
{
  "context": {
    "truncation_strategy": "middle-out",
    "truncation_keep_first_pct": 15,
    "truncation_keep_last_pct": 25,
    "tfidf_query_source": ""
  }
}
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `truncation_strategy` | string | `"middle-out"` | Context truncation: `"middle-out"`, `"keep_first_last"` |
| `truncation_keep_first_pct` | float | `15` | First portion % to preserve during truncation |
| `truncation_keep_last_pct` | float | `25` | Last portion % to preserve during truncation |
| `tfidf_query_source` | string | `""` | TF-IDF query source: `""`, `"prior_messages"`, `"self"` |

## `governance`

Governance settings control operational policies: rate limits, retries, routing decisions,
and safety constraints.

```json
{
  "governance": {
    "ratelimit_max_rpm": 15,
    "ratelimit_max_tpm": 250000,
    "max_retry_attempts": 3,
    "empty_stream_as_error": true,
    "blocked_execution_patterns": ["..."],
    "routing_strategy": "",
    "routing_latency_weight": 0,
    "routing_cost_weight": 0,
    "max_cost_per_request": 0,
    "auto_context_skip": false,
    "auto_reorder_by_latency": false
  }
}
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `ratelimit_max_rpm` | int | `15` | Max requests per minute (0 = unlimited) |
| `ratelimit_max_tpm` | int | `250000` | Max tokens per minute (0 = unlimited) |
| `max_retry_attempts` | int | `3` | Max retries for transient upstream failures |
| `empty_stream_as_error` | bool | `true` | Treat empty SSE as 500 error |
| `blocked_execution_patterns` | []string | built-in | Regexes for commands to block (kill switch) |
| `routing_strategy` | string | `""` | `""` (default), `"latency"`, `"balanced"` |
| `routing_latency_weight` | float | `0` | Weight for latency in balanced routing (0-1) |
| `routing_cost_weight` | float | `0` | Weight for cost in balanced routing (0-1) |
| `max_cost_per_request` | float | `0` | Max USD per request (0 = unlimited) |
| `auto_context_skip` | bool | `false` | Auto-skip models with insufficient context window for payload size |
| `auto_reorder_by_latency` | bool | `false` | Reorder agent targets by median latency with jitter |

## `bouncer`

The bouncer intercepts oversize payloads, redacts secrets, and optionally summarizes
content before forwarding upstream. This is the core security/privacy feature.

```json
{
  "bouncer": {
    "enabled": true,
    "redact_preset": "credentials",
    "redact_patterns": ["..."],
    "redaction_label": "[REDACTED]",
    "redact_output": false,
    "redact_output_window": 4096,
    "fail_open": true,
    "engine": {
      "provider": "ollama",
      "model": "qwen2.5-coder:7b"
    },
    "entropy_enabled": false,
    "entropy_threshold": 4.5,
    "entropy_min_token": 20
  }
}
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enabled` | bool | auto | Auto-enabled if `redact_patterns` or `redact_preset` is set |
| `redact_preset` | string | `""` | Named preset: `"credentials"`, `"pii"`, or `"all"`. Overrides `redact_patterns` |
| `redact_patterns` | []string | built-in | Regex patterns for PII/credential redaction |
| `redaction_label` | string | `"[REDACTED]"` | Replacement text for redacted content |
| `redact_output` | bool | `false` | Also redact upstream response output |
| `redact_output_window` | int | `4096` | Output window size for output redaction |
| `fail_open` | bool | `true` | Allow request through if engine is unreachable |
| `engine` | object/string | required | Engine reference (see below) |
| `entropy_enabled` | bool | `false` | Enable high-entropy string detection |
| `entropy_threshold` | float | `4.5` | Entropy threshold (0-8) |
| `entropy_min_token` | int | `20` | Minimum tokens for entropy check |

### Engine Reference

The `engine` field supports three formats:

**Object** (full control):
```json
"engine": {
  "provider": "ollama",
  "model": "qwen2.5-coder:7b",
  "timeout_seconds": 60
}
```

**Shorthand** (`provider/model`):
```json
"engine": "ollama/qwen2.5-coder:7b"
```

**Agent reference** (uses agent's model list as fallback chain):
```json
"engine": "my-summarizer-agent"
```

### Redact Presets

| Preset | Patterns |
|--------|----------|
| `"credentials"` | AWS keys, GitHub tokens, Google tokens, OpenAI keys, private keys, env secrets, sendgrid keys |
| `"pii"` | Emails, SSNs, credit cards, phone numbers |

## `window`

Controls context window compaction — reducing payload size when approaching context limits.

```json
{
  "window": {
    "enabled": false,
    "mode": "summarize",
    "active_messages": 6,
    "trigger_ratio": 0.8,
    "summary_max_runes": 4000,
    "max_context": 128000,
     "engine": "ollama/qwen2.5-coder:7b",
     "keep_first_pct": 25,
     "keep_last_pct": 30
  }
}
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enabled` | bool | auto | Auto-enabled if any sub-feature is set |
| `mode` | string | `"summarize"` | Window mode: `"summarize"`, `"truncate"` |
| `active_messages` | int | `6` | Messages to keep after window compaction |
| `trigger_ratio` | float | `0.8` | Context fill ratio to trigger compaction |
| `summary_max_runes` | int | `4000` | Max runes in engine-generated summary |
| `max_context` | int | `128000` | Hard context limit in bytes |
| `engine` | object/string | required | Summarization engine reference |
| `keep_first_pct` | float | `25` | First messages % to preserve |
| `keep_last_pct` | float | `30` | Last messages % to preserve |

## `prefix_cache`

```json
{
  "prefix_cache": {
    "enabled": true,
    "pin_system_first": true,
    "stable_tools": true,
    "skip_redaction_on_system": true
  }
}
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enabled` | bool | auto | Auto-enabled if any sub-feature is set |
| `pin_system_first` | bool | `true` | Cache the first system message |
| `stable_tools` | bool | `true` | Treat tool definitions as cacheable |
| `skip_redaction_on_system` | bool | `false` | Skip redaction on cached system prompt |

## `compaction`

Payload compaction optimizes message size before forwarding upstream. Use presets for
common configurations or tune individual settings.

```json
{
  "compaction": {
    "compaction_preset": "balanced",
    "prune_stale_tools": false,
    "tool_protection_window": 4
  }
}
```

### Presets

| Setting | `"aggressive"` | `"balanced"` | `"minimal"` |
|---------|:---:|:---:|:---:|
| `json_minify` | true | true | false |
| `collapse_blank_lines` | true | true | false |
| `trim_trailing_whitespace` | true | true | false |
| `normalize_line_endings` | true | true | false |
| `prune_stale_tools` | true | false | false |
| `prune_thoughts` | true | false | false |

Individual settings can be overridden after choosing a preset.

### Individual Settings

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `compaction_preset` | string | `""` | Preset: `"aggressive"`, `"balanced"`, `"minimal"` |
| `enabled` | bool | auto | Auto-enabled if any feature is on |
| `json_minify` | bool | `true` | Remove unnecessary whitespace from JSON |
| `collapse_blank_lines` | bool | `true` | Collapse multiple blank lines |
| `trim_trailing_whitespace` | bool | `true` | Trim trailing whitespace |
| `normalize_line_endings` | bool | `true` | Normalize to LF |
| `prune_stale_tools` | bool | `false` | Remove stale tool call results |
| `tool_protection_window` | int | `4` | Recent messages to protect from pruning |
| `prune_thoughts` | bool | `false` | Remove AI thinking blocks |

## `response_cache`

```json
{
  "response_cache": {
    "enabled": false,
    "max_entries": 512,
    "max_entry_bytes": 1048576,
    "ttl_seconds": 3600,
    "evict_every_seconds": 300,
    "force_refresh_header": "x-nenya-cache-force-refresh"
  }
}
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enabled` | bool | auto | Auto-enabled if `max_entries > 0` |
| `max_entries` | int | `512` | Max cached responses (LRU eviction) |
| `max_entry_bytes` | int | `1048576` | Max size per cached entry (1 MB) |
| `ttl_seconds` | int | `3600` | Entry TTL (1 hour) |
| `evict_every_seconds` | int | `300` | Background eviction sweep interval (5 min) |
| `force_refresh_header` | string | `"x-nenya-cache-force-refresh"` | Header that bypasses cache |

## `discovery`

```json
{
  "discovery": {
    "enabled": false,
    "auto_agents": false,
    "auto_agents_config": {
      "fast": { "enabled": true },
      "reasoning": { "enabled": true }
    }
  }
}
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enabled` | bool | `false` | Enable dynamic model discovery |
| `auto_agents` | bool | `false` | Auto-generate agents from discovered models |
| `auto_agents_config` | object | `nil` | Per-category auto-agent toggles |

## `agents`

Agents define named model groups with routing strategy, circuit breaker settings,
and optional MCP tool integration.

```json
{
  "agents": {
    "my-agent": {
      "strategy": "fallback",
      "cooldown_seconds": 60,
      "failure_threshold": 5,
      "failure_window_secs": 300,
      "success_threshold": 1,
      "max_retries": 3,
      "system_prompt_file": "./prompts/default.md",
      "force_system_prompt": false,
      "budget_limit_usd": 0.05,
      "models": [
        "gemini-2.5-flash",
        {"provider": "deepseek", "model": "deepseek-chat"},
        {"provider_rgx": "^claude", "model_rgx": ".*sonnet.*"}
      ],
      "mcp": {
        "servers": ["mempalace"],
        "max_iterations": 10,
        "auto_search": true,
        "auto_save": false
      }
    }
  }
}
```

### Agent-level Fields

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `strategy` | string | `"fallback"` | Routing: `"fallback"`, `"round-robin"`, `"latency"` |
| `cooldown_seconds` | int | `0` | Cooldown after circuit breaker trip |
| `failure_threshold` | int | `0` | Consecutive failures to trip breaker |
| `failure_window_secs` | int | `0` | Time window for failure counting |
| `success_threshold` | int | `0` | Consecutive successes to reset breaker |
| `max_retries` | int | `0` | Per-request retry limit (0 = global default) |
| `system_prompt` | string | `""` | Custom system prompt |
| `system_prompt_file` | string | `""` | Path to system prompt file |
| `force_system_prompt` | bool | `false` | Always inject system prompt |
| `budget_limit_usd` | float | `0` | Per-request budget limit |
| `mcp` | object | `nil` | MCP server integration config |
| `models` | array | required | Model list (see below) |

### Model Entry Formats

**String shorthand** (looked up in built-in registry):
```json
"gemini-2.5-flash"
```

**Full object** (explicit provider/model):
```json
{
  "provider": "deepseek",
  "model": "deepseek-chat",
  "max_context": 128000,
  "max_output": 8192,
  "format": "openai",
  "url": "https://custom.example.com/v1/chat"
}
```

**Provider wildcard** (all models from a provider):
```json
{
  "provider": "zai"
}
```

**Regex selector** (dynamic matching against discovery):
```json
{
  "model_rgx": "^claude-3\\.5-.*$"
}
```

**Dual regex** (provider + model matching):
```json
{
  "provider_rgx": ".*free$",
  "model_rgx": ".*flash.*"
}
```

### Full Model Object Fields

| Field | Type | Description |
|-------|------|-------------|
| `provider` | string | Provider name (static) |
| `model` | string | Model name (static) |
| `format` | string | Wire format: `"openai"`, `"anthropic"`, `"gemini"` |
| `url` | string | Per-model URL override |
| `max_context` | int | Context window size |
| `max_output` | int | Max output tokens |
| `required_capabilities` | []string | Required model capabilities |
| `provider_rgx` | string | Regex to match providers (dynamic) |
| `model_rgx` | string | Regex to match models (dynamic) |

## `providers`

Upstream API provider definitions.

```json
{
  "providers": {
    "gemini": {
      "url": "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions",
      "auth_style": "bearer+x-goog"
    },
    "deepseek": {
      "url": "https://api.deepseek.com/chat/completions",
      "auth_style": "bearer",
      "timeout_seconds": 60,
      "max_retry_attempts": 3
    },
    "ollama": {
      "url": "http://127.0.0.1:11434/v1/chat/completions",
      "auth_style": "none"
    }
  }
}
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `url` | string | required | API endpoint URL |
| `auth_style` | string | `"bearer"` | Auth: `"bearer"`, `"bearer+x-goog"`, `"none"` |
| `api_format` | string | `""` | Wire format override |
| `format_urls` | map | `nil` | Per-format URL overrides |
| `timeout_seconds` | int | `60` | Request timeout |
| `retryable_status_codes` | []int | built-in | Status codes to retry |
| `max_retry_attempts` | int | global | Per-provider retry override |
| `thinking` | object | `nil` | Thinking/CoT config: `{ "enabled": true, "clear_thinking": false }` |

## `mcp_servers`

```json
{
  "mcp_servers": {
    "mempalace": {
      "url": "http://localhost:6060",
      "timeout": 30,
      "keep_alive_interval": 30
    }
  }
}
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `url` | string | required | MCP server HTTP+SSE endpoint |
| `timeout` | int | `30` | Request timeout in seconds |
| `keep_alive_interval` | int | `30` | SSE keep-alive interval in seconds |

## Secrets

API keys are stored **separately** from the main config, in a `secrets.json` file
located in `$CREDENTIALS_DIRECTORY/secrets` or `$NENYA_SECRETS_DIR/secrets`.

```json
{
  "client_token": "nk-your-client-token",
  "provider_keys": {
    "gemini": "AIza...",
    "deepseek": "sk-..."
  },
  "api_keys": {
    "user-1": {
      "name": "Alice",
      "token": "nk-...",
      "roles": ["user"],
      "allowed_agents": ["build", "plan"],
      "expires_at": "2026-12-31T23:59:59Z"
    }
  }
}
```

| Field | Type | Description |
|-------|------|-------------|
| `client_token` | string | Gateway auth token for client requests |
| `provider_keys` | map | API key per provider |
| `api_keys` | map | Per-user API keys with roles and permissions |

## Migrating from `security_filter` (pre-v1.0)

| Old Key | New Key |
|---------|---------|
| `security_filter` | `bouncer` |
| `patterns` | `redact_patterns` |
| `output_enabled` | `redact_output` |
| `output_window_chars` | `redact_output_window` |
| `skip_on_engine_failure` | `fail_open` |

The engine no longer defaults to `ollama`/`qwen2.5-coder:7b`. You must explicitly
set `bouncer.engine.provider` and `bouncer.engine.model`, or reference an agent
that has models configured.

## Migrating from `governance` (v1.0 context consolidation)

| Old Key | New Key |
|---------|---------|
| `governance.truncation_strategy` | `context.truncation_strategy` |
| `governance.truncation_keep_first_pct` | `context.truncation_keep_first_pct` |
| `governance.truncation_keep_last_pct` | `context.truncation_keep_last_pct` |
| `governance.tfidf_query_source` | `context.tfidf_query_source` |

## Breaking Changes (v1.1)

| Change | Details |
|--------|---------|
| Routing booleans moved | `context.auto_context_skip` and `context.auto_reorder_by_latency` moved to `governance.*` |
| Compaction presets | Added `compaction.compaction_preset`: `"aggressive"`, `"balanced"`, `"minimal"`. Individual fields still work. |
