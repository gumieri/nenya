# Provider Adapter System

The `internal/adapter` package manages provider-specific differences in wire format, authentication, response transformation, and error classification through a clean Go interface. Nenya includes adapters for 23 built-in providers (OpenAI, Anthropic, Gemini, DeepSeek, Ollama, Groq, Together, SambaNova, Cerebras, NVIDIA, GitHub, Qwen, MiniMax, Moonshot, Mistral, xAI, OpenRouter, Azure, Perplexity, Cohere, DeepInfra, z.ai, and OpenCode Zen). For a user-facing provider guide with capabilities and special behaviors, see [`PROVIDERS.md`](PROVIDERS.md).

## The Interface

```go
type ProviderAdapter interface {
    MutateRequest(body []byte, model string, stream bool) ([]byte, error)
    InjectAuth(req *http.Request, apiKey string) error
    MutateResponse(body []byte) ([]byte, error)
    NormalizeError(statusCode int, body []byte) ErrorClass
}
```

| Method | Purpose |
|--------|---------|
| `MutateRequest` | Transform OpenAI-format request body into provider-specific format. Strip unsupported params based on capabilities. |
| `InjectAuth` | Set authentication headers (Bearer, x-goog-api-key, none) on the upstream request. |
| `MutateResponse` | Normalize SSE response chunks (e.g., inject missing `index` fields in Gemini tool_calls). |
| `NormalizeError` | Classify HTTP errors into retryable categories for the circuit breaker. |

## Error Classification

```go
type ErrorClass int

const (
    ErrorPermanent    // Do not retry
    ErrorRetryable    // Retry with exponential backoff
    ErrorRateLimited  // Retry after delay (parse Retry-After header)
    ErrorQuotaExhausted // Long cooldown (up to 30 minutes)
)
```

## Adapter Capabilities (Request Field Stripping)

Each adapter declares what the upstream provider supports at the wire-format level. Parameters not supported are automatically stripped from the request. These are **provider-level** capabilities, distinct from model-level capabilities (vision, reasoning, etc.) which are inferred dynamically via `discovery.InferCapabilities()`.

```go
type Capabilities struct {
    StreamOptions   bool // Provider supports stream_options.include_usage
    AutoToolChoice  bool // Provider supports tool_choice: "auto"
    ContentArrays   bool // Provider supports content as array of objects (vision)
}
```

## Auth Styles

| Style | Header(s) | Used By |
|-------|-----------|---------|
| `bearer` | `Authorization: Bearer <key>` | OpenAI, DeepSeek, Groq, Together, SambaNova, Cerebras, GitHub, z.ai, Mistral, xAI, Perplexity, Cohere, DeepInfra, Moonshot, Qwen |
| `bearer+x-goog` | `Authorization: Bearer <key>` + `x-goog-api-key: <key>` | Gemini |
| `anthropic` | `x-api-key: <key>` + `anthropic-version: 2023-06-01` | Anthropic |
| `azure` | `api-key: <key>` | Azure OpenAI |
| `none` | (no auth) | Ollama |

Auth style is resolved in priority order:
1. Adapter-specific `InjectAuth()` (for registered providers)
2. `ProviderConfig.AuthStyle` from JSON config (for dynamic providers)
3. Default `BearerAuth` fallback

> **Multi-account key selection**: When a provider is configured with multiple credentials via the `accounts` field, the gateway uses LRU-based account selection per model. The auth style from the resolved adapter is applied to each selected account's credential. See [CONFIGURATION.md](CONFIGURATION.md#multi-account-per-provider-keys) for configuration details.

## Built-in Adapters

### OpenAIAdapter (default for ~80% of providers)

Identity transform for request/response. Capability-based parameter stripping:

- `StreamOptions: false` → strips `stream_options` from request
- `AutoToolChoice: false` → strips `tool_choice: "auto"` from request
- `ContentArrays: false` → flattens `content: [{type:"text",text:"..."}]` to `content: "..."`

**Used by**: `openai`, `deepseek`, `groq`, `together`, `github`, `sambanova`, `cerebras`, `nvidia`, `nvidia_free`, `qwen_free`, `minimax_free`, `moonshot`

### GeminiAdapter

- **Request**: Model name aliasing (e.g., `gemini-flash` → `gemini-2.5-flash`), orphaned tool_call cleanup (strips tool_calls missing `extra_content`/thought_signature plus their paired tool messages), thought_signature cache injection
- **Response**: Injects missing `index` field on `tool_calls[]`, caches `extra_content` for multi-turn
- **Auth**: `bearer+x-goog`
- **Error**: Gemini-specific retryable patterns (`resource_exhausted`, `quota exceeded`, `the response was blocked`, `content has no parts`)

### ZAIAdapter

- **Request**: Orphaned tool message removal, consecutive user message merging, user bridge insertion between consecutive assistant messages, system bridge prepending
- **Auth**: `bearer`
- **Error**: Standard classification

### OllamaAdapter

- **Request**: Strips unsupported `tool_choice` field from request body (Ollama does not support tool choice control)
- **Auth**: Optional Bearer (only if API key is non-empty)
- **Error**: Conservative — only 429/5xx are retryable

### AnthropicAdapter

Full bidirectional conversion between OpenAI and Anthropic native API formats.

- **Request**: Converts OpenAI format to Anthropic format:
  - Messages: `system` → `user`/`assistant` pair, `tool` → `user` with `tool_result` content block
  - Tools: OpenAI `function` tools → Anthropic tool format (with `input_schema`)
  - `tool_choice`: `"auto"` → `{"type":"auto"}`, `"required"` → `{"type":"required"}`, named tool → `{"type":"tool","name":"..."}`
  - Parameters mapped: `max_tokens`, `temperature`, `top_p`, `stop` → `stop_sequences`, `user` → `metadata.user_id`
- **Response**: Converts Anthropic format back to OpenAI:
  - Content blocks: `text` → `delta.content`, `tool_use` → `delta.tool_calls`
  - `stop_reason`: `end_turn` → `stop`, `tool_use` → `tool_calls`, `max_tokens` → `length`
  - Usage: `input_tokens`/`output_tokens` → OpenAI usage format
- **Auth**: `x-api-key` header + `anthropic-version` header
- **Error**: 529 treated as rate-limited (Anthropic overloaded)

### MistralAdapter

OpenAI-compatible with capability-based stripping.

- **Request**: Stream options stripped, content arrays preserved, auto tool choice preserved
- **Auth**: Bearer
- **Error**: Standard classification

### XAIAdapter

OpenAI-compatible with extended capabilities.

- **Request**: Stream options and auto tool choice preserved, content arrays preserved
- **Auth**: Bearer
- **Error**: Standard classification

### OpenRouterAdapter

OpenAI-compatible with custom headers for OpenRouter's referral program.

- **Request**: Adds `HTTP-Referer` and `X-Title` headers on every request. Stream options and auto tool choice preserved.
- **Auth**: Bearer + `HTTP-Referer: https://github.com/nenya-project/nenya` + `X-Title: Nenya`
- **Error**: Standard classification

### AzureAdapter

OpenAI-compatible for Azure OpenAI Service endpoints.

- **Request**: Standard capability-based stripping
- **Auth**: `api-key` header (not `Authorization: Bearer`)
- **Error**: Standard classification

### PerplexityAdapter

OpenAI-compatible. Perplexity does not support function calling.

- **Request**: Content arrays preserved, auto tool choice stripped
- **Auth**: Bearer
- **Error**: Standard classification

### CohereAdapter

OpenAI-compatible. Cohere uses a different content format internally.

- **Request**: Content arrays flattened, auto tool choice preserved
- **Auth**: Bearer
- **Error**: Standard classification

### DeepInfraAdapter

OpenAI-compatible with standard capabilities.

- **Request**: Content arrays preserved, auto tool choice preserved
- **Auth**: Bearer
- **Error**: Standard classification

### Provider Adapter Mappings

Many providers are OpenAI-compatible and use the default `OpenAIAdapter` rather than a dedicated adapter:

| Provider | Adapter | Registration | Caps | Notes |
|----------|---------|-------------|------|-------|
| **DeepSeek** | `OpenAIAdapter` | Explicit | `S:✓, A:✓, C:✓` | Stream options enabled |
| **Moonshot** | `OpenAIAdapter` | Explicit | `S:✓, A:✓, C:✓` | Full capabilities |
| **OpenCode Zen** | `OpenAIAdapter` | Explicit | `S:✓, A:✓, C:✓` | Full capabilities |
| **Qwen Free** | `OpenAIAdapter` | Explicit | `S:✓, A:✗, C:✓` | Auto tool choice stripped |
| **MiniMax Free** | `OpenAIAdapter` | Explicit | `S:✓, A:✓, C:✓` | Full capabilities |
| **NVIDIA** | `OpenAIAdapter` | Explicit | `S:✗, A:✗, C:✓` | Restricted capabilities |
| **NVIDIA Free** | `OpenAIAdapter` | Explicit | `S:✗, A:✗, C:✓` | Same as NVIDIA |
| **Qwen** | `OpenAIAdapter` | Default fallback | All disabled | Not in init(); user-config only |
| **MiniMax** | `OpenAIAdapter` | Default fallback | All disabled | Not in init(); user-config only |

S = StreamOptions, A = AutoToolChoice, C = ContentArrays. ✓ = allowed, ✗ = stripped.

These providers use the `OpenAIAdapter` with provider-specific capability settings registered in `internal/adapter/registry.go`. Adding a new OpenAI-compatible provider typically requires zero Go code — only a JSON config entry and a secrets key.

### Explicit vs Default Registration

Providers in the adapter registry fall into three categories:

| Category | Providers | Behavior |
|----------|-----------|----------|
| **Custom adapter** | `gemini`, `zai`, `anthropic`, `openrouter`, `mistral`, `xai`, `azure`, `perplexity`, `cohere`, `deepinfra`, `ollama` | Dedicated adapter implementation registered via `Register()` |
| **OpenAIAdapter (explicit caps)** | `openai`, `deepseek`, `groq`, `together`, `github`, `sambanova`, `cerebras`, `nvidia`, `nvidia_free`, `qwen_free`, `minimax_free`, `zen`, `moonshot` | Registered via `registerOpenAI()` with provider-specific capability settings |
| **OpenAIAdapter (default fallback)** | `qwen`, `minimax`, and any user-configured provider not in the registry | Uses `OpenAIAdapter{Caps: Capabilities{}}` — all capabilities disabled, meaning `stream_options`, `tool_choice: "auto"`, and content arrays are all stripped |

> **Runtime override**: `zai-coding-plan` is initially registered as an OpenAIAdapter in `init()` but is replaced by the `ZAIAdapter` during startup when `InitWithDeps()` is called. This ensures `zai-coding-plan` shares the same message sanitization logic as `zai` (orphaned tool removal, message merging, bridge insertion) and Zhipu-specific error classification.

## Adding a New Provider

### OpenAI-Compatible Provider (zero Go code)

Most providers (Groq, Together, Fireworks, Perplexity, etc.) use the OpenAI wire format. Add them via JSON config only:

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

### Alien-Format Provider (requires Go code)

For providers with fundamentally different wire formats (Bedrock, Vertex, Anthropic, Cohere v1), create a new adapter file in `internal/adapter/`:

1. Define a struct implementing `ProviderAdapter`
2. Register it in `registry.go` via `Register()`
3. Handle auth, request/response mutation, and error classification

See `gemini.go` or `zai.go` for examples.

## Registry

The adapter registry is thread-safe with `sync.RWMutex`:

```go
adapter.Register("my-provider", adapter.AdapterEntry{
    Adapter: NewMyAdapter(),
})

a := adapter.ForProvider("my-provider")
```

Unknown providers get the default `OpenAIAdapter`. Providers defined in JSON config (but not registered) get their adapter resolved by `AuthStyle` via `AdapterForAuthStyle()`.
