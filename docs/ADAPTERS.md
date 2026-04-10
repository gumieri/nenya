# Provider Adapter System

The `internal/adapter` package manages provider-specific differences in wire format, authentication, response transformation, and error classification through a clean Go interface.

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

## Capabilities

Each adapter declares what the upstream provider supports. Parameters not supported are automatically stripped from the request:

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
| `bearer` | `Authorization: Bearer <key>` | OpenAI, DeepSeek, Groq, Together, OpenRouter, SambaNova, Cerebras, GitHub, z.ai |
| `bearer+x-goog` | `Authorization: Bearer <key>` + `x-goog-api-key: <key>` | Gemini |
| `none` | (no auth) | Ollama |

Auth style is resolved in priority order:
1. Adapter-specific `InjectAuth()` (for registered providers)
2. `ProviderConfig.AuthStyle` from JSON config (for dynamic providers)
3. Default `BearerAuth` fallback

## Built-in Adapters

### OpenAIAdapter (default for ~80% of providers)

Identity transform for request/response. Capability-based parameter stripping:

- `StreamOptions: false` → strips `stream_options` from request
- `AutoToolChoice: false` → strips `tool_choice: "auto"` from request
- `ContentArrays: false` → flattens `content: [{type:"text",text:"..."}]` to `content: "..."`

**Used by**: `openai`, `deepseek`, `groq`, `together`, `github`, `openrouter`, `sambanova`, `cerebras`, `nvidia`, `nvidia_free`, `qwen_free`, `minimax_free`, `zai-coding-plan`

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

- **Request**: Identity (no transformation)
- **Auth**: Optional Bearer (only if API key is non-empty)
- **Error**: Conservative — only 429/5xx are retryable

## Adding a New Provider

### OpenAI-Compatible Provider (zero Go code)

Most providers (Groq, Together, Fireworks, Perplexity, etc.) use the OpenAI wire format. Add them via JSON config only:

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
