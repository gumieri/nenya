# Provider Reference

Nenya works with any provider that implements the OpenAI Chat Completions API endpoint (`POST /v1/chat/completions`). For the providers listed below, we ship built-in adapters that handle wire format differences, authentication, and error classification.

## Compatibility Tiers

### Tier 1: Full Adapter
Providers with custom wire formats requiring request/response transformation:
- **Anthropic** - Bidirectional OpenAI↔Anthropic format conversion
- **Gemini** - Thought signature preservation, orphaned tool_call cleanup, model aliasing
- **z.ai** - Orphaned tool message removal, user message merging
- **Ollama** - Local-first, optional auth, conservative error classification

### Tier 2: Adapter with Tweaks
OpenAI-compatible with specific adjustments for auth, headers, or capabilities:
- **OpenRouter** - Adds `HTTP-Referer` and `X-Title` headers
- **Azure OpenAI** - Uses `api-key` header instead of `Authorization: Bearer`
- **Perplexity** - Does not support function calling (`tool_choice` stripped)
- **Cohere** - Content arrays flattened to strings
- **DeepInfra** - Standard capabilities

### Tier 3: Drop-in OpenAI-Compatible
Zero-config integration for providers using the standard OpenAI wire format:
- **DeepSeek**, **Mistral**, **xAI**, **Groq**, **Together**, **SambaNova**, **Cerebras**, **NVIDIA**, **GitHub**, **Qwen**, **MiniMax**, **OpenCode Zen**

## Per-Model Wire Format (`format` attribute)

Nenya supports the `format` attribute on model entries, enabling per-model wire format routing independent of the provider. This is useful for multi-format gateways like OpenCode Zen that serve models from different API families through a single provider endpoint.

| Format | Description | Request Body | Response SSE |
|--------|-------------|--------------|--------------|
| `"openai"` (default) | OpenAI Chat Completions + standard SSE | Passthrough | Passthrough |
| `"anthropic"` | Anthropic Messages API | OpenAI → Anthropic conversion | Anthropic → OpenAI conversion |
| `"gemini"` | Gemini API | URL routing only* | See note* |

> *Gemini format conversion is handled by the existing Gemini provider adapter (`bearer+x-goog` auth style). Setting `format: "gemini"` on a model only affects URL routing (selects the Gemini endpoint from `FormatURLs["gemini"]`). Request body sanitization and SSE response transformation are performed by the Gemini provider's own `ProviderSpec` hooks, not by the format pipeline.

When a model has `format: "anthropic"`:
1. **URL routing**: The request is sent to the provider's `FormatURLs["anthropic"]` endpoint (e.g., `/v1/messages`)
2. **Body conversion**: The OpenAI-format request is automatically converted to Anthropic Messages API format
3. **Response transformation**: Anthropic SSE events (`content_block_delta`, `message_delta`) are converted to OpenAI SSE format

This enables providers like OpenCode Zen to serve Claude models (format: `"anthropic"`) and OpenAI-compatible models (format: `"openai"`) through a single provider entry with zero user configuration.

Providers can declare multiple format endpoints via `FormatURLs` in their registry entry:
```json
{
  "providers": {
    "zen": {
      "url": "https://opencode.ai/zen/v1/chat/completions",
      "format_urls": {
        "anthropic": "https://opencode.ai/zen/v1/messages"
      }
    }
  }
}
```

## Provider Reference Table

| Provider | Auth Style | Stream Options | Auto Tool Choice | Content Arrays | Tool Calls | Reasoning | Vision |
|----------|------------|----------------|------------------|----------------|------------|-----------|--------|
| **Anthropic** | `anthropic` | ❌ | ✅ | ❌ | ✅ | ✅ | ✅ |
| **Gemini** | `bearer+x-goog` | ❌ | ✅ | ✅ | ✅ | ✅ | ✅ |
| **z.ai** | `bearer` | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| **Ollama** | `none` | ❌ | ✅ | ❌ | ✅ | ✅ | ❌ |
| **OpenRouter** | `bearer` | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| **Azure OpenAI** | `api-key` | ❌ | ✅ | ✅ | ✅ | ✅ | ✅ |
| **Perplexity** | `bearer` | ❌ | ❌ | ✅ | ❌ | ❌ | ✅ |
| **Cohere** | `bearer` | ❌ | ✅ | ✅ | ✅ | ❌ | ✅ |
| **DeepInfra** | `bearer` | ❌ | ✅ | ✅ | ✅ | ❌ | ✅ |
| **DeepSeek** | `bearer` | ✅ | ✅ | ✅ | ✅ | ✅ | ❌ |
| **Mistral** | `bearer` | ❌ | ✅ | ✅ | ✅ | ✅ | ✅ |
| **xAI** | `bearer` | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| **Groq** | `bearer` | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| **Together** | `bearer` | ❌ | ✅ | ✅ | ❌ | ✅ | ✅ |
| **SambaNova** | `bearer` | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| **Cerebras** | `bearer` | ✅ | ✅ | ✅ | ✅ | ✅ | ❌ |
| **NVIDIA** | `bearer` | ❌ | ❌ | ✅ | ✅ | ❌ | ✅ |
| **GitHub** | `bearer` | ❌ | ✅ | ✅ | ✅ | ✅ | ✅ |
| **Qwen** | `bearer` | ✅ | ❌ | ✅ | ✅ | ✅ | ❌ |
| **MiniMax** | `bearer` | ✅ | ❌ | ✅ | ✅ | ✅ | ❌ |
| **OpenCode Zen** | `bearer` | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |

> ✅ = Supported | ❌ = Not Supported

## Special Behaviors

### DeepSeek v4 (deepseek-v4-flash, deepseek-v4-pro)

DeepSeek v4 models support a **thinking mode** controlled by the `thinking` parameter and return structured `reasoning_content` in assistant messages.

- **`deepseek-v4-pro`**: Thinking mode is **on by default**.
- **`deepseek-v4-flash`**: Thinking mode is **on by default**. To disable, send `thinking: {"type": "disabled"}`.
- **Reasoning effort**: `reasoning_effort: "high"` (default) or `"max"`. For complex agent requests (Claude Code, OpenCode), DeepSeek auto-escalates to `max`.
- **Multi-turn**: `reasoning_content` from assistant messages is passed back verbatim. When tool calls were performed, this field is **mandatory** — the API returns 400 if missing. The gateway preserves it for reasoning providers and strips it for others.
- **Ignored params**: In thinking mode, `temperature`, `top_p`, `presence_penalty`, `frequency_penalty` are silently ignored. The gateway strips these for DeepSeek when thinking is enabled.
- **Prefix caching**: DeepSeek uses automatic disk-based KV caching with exact prefix matching. Enable `prefix_cache` in the gateway config to optimize cache hits — the gateway pins system messages first and sorts tools deterministically.
- **Limits**: 1M context, 384K max output.

### Anthropic
- **Request**: Converts OpenAI format to Anthropic native format
  - Messages: `system` → `user`/`assistant` pair, `tool` → `user` with `tool_result` content block
  - Tools: OpenAI `function` tools → Anthropic tool format (with `input_schema`)
  - `tool_choice`: `"auto"` → `{"type":"auto"}`, `"required"` → `{"type":"required"}`, named tool → `{"type":"tool","name":"..."}`
  - Parameters mapped: `max_tokens`, `temperature`, `top_p`, `stop` → `stop_sequences`, `user` → `metadata.user_id`
- **Response**: Converts Anthropic format back to OpenAI:
  - Content blocks: `text` → `delta.content`, `tool_use` → `delta.tool_calls`
  - `stop_reason`: `end_turn` → `stop`, `tool_use` → `tool_calls`, `max_tokens` → `length`
  - Usage: `input_tokens`/`output_tokens` → OpenAI usage format
- **Auth**: `x-api-key` header + `anthropic-version: 2023-06-01`
- **Error**: 529 treated as rate-limited (Anthropic overloaded)

### Gemini
- **Request**: 
  - Model name aliasing (e.g., `gemini-flash` → `gemini-2.5-flash`)
  - Orphaned tool_call cleanup (strips tool_calls missing `extra_content`/`thought_signature` plus their paired tool messages)
  - Thought signature cache injection
- **Response**: 
  - Injects missing `index` field on `tool_calls[]`
  - Caches `extra_content` for multi-turn
- **Auth**: `bearer+x-goog` (both `Authorization: Bearer <key>` and `x-goog-api-key: <key>`)
- **Error**: Gemini-specific retryable patterns (`resource_exhausted`, `quota exceeded`, `the response was blocked`, `content has no parts`)

### z.ai (Zhipu)
- **Request**:
  - Orphaned tool message removal
  - Consecutive user message merging
  - User bridge insertion between consecutive assistant messages
  - System bridge prepending
  - Thinking mode auto-activation for reasoning-capable models (e.g., GLM-5)
  - Model-specific temperature defaults (GLM-4.6/4.7 → 1.0)
- **Thinking mode**: Auto-enabled when the model supports reasoning. Configurable per-provider via `thinking.enabled` in the provider config:
  ```json
  "zai": {
    "url": "https://api.z.ai/v1/chat/completions",
    "thinking": null
  }
  ```
  - `null` (omitted) → auto mode (enabled for reasoning models only)
  - `{"enabled": true}` → force enable for all models
  - `{"enabled": false}` → force disable
- **Auth**: `bearer`
- **Error**: Zhipu error codes (1302/1303 → rate-limited, 1308/1310 → quota exhausted, 1312 → retryable, 1311/1313 → permanent) + `model_context_window_exceeded` → retryable

### OpenCode Zen
- **Multi-format gateway** — Claude models auto-convert to Anthropic wire format
- Supports both `format: "openai"` and `format: "anthropic"` per model
- See [Per-Model Wire Format](#per-model-wire-format-format-attribute) for details

### Ollama
- **Request**: Identity (no transformation)
- **Auth**: Optional Bearer (only if API key is non-empty)
- **Error**: Conservative — only 429/5xx are retryable

## Adding Custom Providers

### OpenAI-Compatible Providers (Zero Code Changes)
Most providers use the OpenAI wire format. Add them via JSON config only:

```json
{
  "providers": {
    "fireworks": {
      "url": "https://api.fireworks.ai/inference/v1/chat/completions",
      "auth_style": "bearer"
    }
  }
}
```

The adapter registry falls back to `OpenAIAdapter` with conservative capabilities for unknown providers.

### Alien-Format Providers (Requires Go Code)
For providers with fundamentally different wire formats (Bedrock, Vertex, Anthropic v1, Cohere v1), create a new adapter file in `internal/adapter/`:

1. Define a struct implementing `ProviderAdapter`
2. Register it in `registry.go` via `Register()`
3. Handle auth, request/response mutation, and error classification

See `gemini.go` or `zai.go` for examples.

## Auth Styles Reference

| Style | Header(s) | Used By |
|-------|-----------|---------|
| `bearer` | `Authorization: Bearer <key>` | OpenAI, DeepSeek, Groq, Together, SambaNova, Cerebras, GitHub, z.ai, Mistral, xAI, Perplexity, Cohere, DeepInfra |
| `bearer+x-goog` | `Authorization: Bearer <key>` + `x-goog-api-key: <key>` | Gemini |
| `anthropic` | `x-api-key: <key>` + `anthropic-version: 2023-06-01` | Anthropic |
| `azure` | `api-key: <key>` | Azure OpenAI |
| `none` | (no auth) | Ollama |

Auth style resolution priority:
1. Adapter-specific `InjectAuth()` (for registered providers)
2. `ProviderConfig.AuthStyle` from JSON config (for dynamic providers)
3. Default `BearerAuth` fallback

## Provider Spec Capabilities

Each provider declares its capabilities through a `ProviderSpec`:

| Capability | Description |
|------------|-------------|
| `SupportsStreamOptions` | Provider supports `stream_options.include_usage` |
| `SupportsAutoToolChoice` | Provider supports `tool_choice: "auto"` |
| `SupportsContentArrays` | Provider supports content as array of objects (vision) |
| `SupportsToolCalls` | Provider supports function calling |
| `SupportsReasoning` | Provider returns reasoning tokens (`reasoning_content` field) |
| `SupportsVision` | Provider accepts image inputs |

See `internal/providers/spec.go` for the full specification.

## Model Discovery Support

Nenya automatically fetches model catalogs from configured providers at startup and on SIGHUP reload. Discovery support varies by provider:

| Provider | Discovery Support | Endpoint | Response Format |
|----------|-------------------|----------|-----------------|
| **Anthropic** | Full | `api.anthropic.com/v1/models` | `{"data": [{"id": "..."}]}` |
| **Gemini** | Full | `generativelanguage.googleapis.com/v1beta/models` | `{"models": [{"name": "models/...", "inputTokenLimit": ...}]}` |
| **Ollama** | Full | `127.0.0.1:11434/api/tags` | `{"models": [{"name": "..."}]}` |
| **OpenAI-compatible** | Full | `{provider_url}/v1/models` | `{"data": [{"id": "..."}]}` |
| **Others** | Default | Derived from chat endpoint | OpenAI format fallback |

Discovery is automatic — no configuration required. Models are merged with the static registry using three-tier priority (config overrides > discovered > static). Providers without API keys are skipped. See [Configuration > Model Discovery](CONFIGURATION.md#model-discovery) for details.