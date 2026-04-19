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
- **DeepSeek**, **Mistral**, **xAI**, **Groq**, **Together**, **SambaNova**, **Cerebras**, **NVIDIA**, **GitHub**, **Qwen**, **MiniMax**

## Provider Reference Table

| Provider | Auth Style | Route Prefixes | Stream Options | Auto Tool Choice | Content Arrays | Tool Calls | Reasoning | Vision |
|----------|------------|----------------|----------------|------------------|----------------|------------|-----------|--------|
| **Anthropic** | `anthropic` | `claude-*` | ❌ | ✅ | ❌ | ✅ | ✅ | ✅ |
| **Gemini** | `bearer+x-goog` | `gemini-*` | ❌ | ✅ | ✅ | ✅ | ✅ | ✅ |
| **z.ai** | `bearer` | `glm-*` | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| **Ollama** | `none` | (local) | ❌ | ✅ | ❌ | ✅ | ✅ | ❌ |
| **OpenRouter** | `bearer` | (custom) | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| **Azure OpenAI** | `api-key` | (custom) | ❌ | ✅ | ✅ | ✅ | ✅ | ✅ |
| **Perplexity** | `bearer` | (custom) | ❌ | ❌ | ✅ | ❌ | ❌ | ✅ |
| **Cohere** | `bearer` | (custom) | ❌ | ✅ | ✅ | ✅ | ❌ | ✅ |
| **DeepInfra** | `bearer` | (custom) | ❌ | ✅ | ✅ | ✅ | ❌ | ✅ |
| **DeepSeek** | `bearer` | `deepseek-*` | ✅ | ✅ | ✅ | ✅ | ✅ | ❌ |
| **Mistral** | `bearer` | `mistral-*`, `codestral-*`, `devstral-*` | ❌ | ✅ | ✅ | ✅ | ✅ | ✅ |
| **xAI** | `bearer` | `grok-*` | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| **Groq** | `bearer` | (custom) | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| **Together** | `bearer` | `together/*` | ❌ | ✅ | ✅ | ✅ | ❌ | ✅ |
| **SambaNova** | `bearer` | (custom) | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| **Cerebras** | `bearer` | (custom) | ✅ | ✅ | ✅ | ✅ | ✅ | ❌ |
| **NVIDIA** | `bearer` | (custom) | ❌ | ❌ | ✅ | ✅ | ❌ | ✅ |
| **GitHub** | `bearer` | (custom) | ❌ | ✅ | ✅ | ✅ | ✅ | ✅ |
| **Qwen** | `bearer` | (custom) | ✅ | ❌ | ✅ | ✅ | ✅ | ❌ |
| **MiniMax** | `bearer` | (custom) | ✅ | ❌ | ✅ | ✅ | ✅ | ❌ |

> ✅ = Supported | ❌ = Not Supported

## Special Behaviors

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

### z.ai
- **Request**:
  - Orphaned tool message removal
  - Consecutive user message merging
  - User bridge insertion between consecutive assistant messages
  - System bridge prepending
- **Auth**: `bearer`
- **Error**: Standard classification

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
      "auth_style": "bearer",
      "route_prefixes": ["accounts/fireworks/models/"]
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