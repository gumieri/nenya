# Provider Capabilities Matrix

This document provides a comprehensive overview of all supported LLM providers and their capabilities within the Nenya gateway.

| Provider | Stream Options | Auto Tool Choice | Content Arrays | Tool Calls | Reasoning | Vision | Notes |
|----------|---------------|-----------------|----------------|------------|-----------|--------|-------|
| anthropic | ✅ | ✅ | ✅ | ✅ | ✅ | ❌ | Full OpenAI↔Anthropic format conversion |
| azure | ❌ | ✅ | ✅ | ✅ | ✅ | ✅ | Azure OpenAI endpoint |
| cohere | ❌ | ✅ | ❌ | ✅ | ✅ | ❌ | |
| deepinfra | ❌ | ✅ | ✅ | ✅ | ✅ | ❌ | |
| gemini | ❌ | ✅ | ✅ | ✅ | ✅ | ✅ | Google-style dual auth (Authorization + x-goog-api-key) |
| github | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | GitHub Models |
| groq | ✅ | ✅ | ✅ | ✅ | ✅ | ❌ | |
| mistral | ✅ | ✅ | ✅ | ✅ | ✅ | ❌ | |
| nvidia | ✅ | ✅ | ✅ | ✅ | ✅ | ❌ | |
| nvidia_free | ✅ | ✅ | ✅ | ✅ | ✅ | ❌ | |
| ollama | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | Local inference |
| openai | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | OpenAI API |
| openrouter | ❌ | ✅ | ✅ | ✅ | ✅ | ❌ | Aggregator gateway |
| perplexity | ❌ | ✅ | ✅ | ✅ | ✅ | ❌ | |
| qwen_free | ✅ | ✅ | ✅ | ✅ | ✅ | ❌ | |
| sambanova | ✅ | ✅ | ✅ | ✅ | ✅ | ❌ | |
| deepseek | ✅ | ✅ | ✅ | ✅ | ✅ | ❌ | Requires `reasoning_content` on assistant messages |
| together | ✅ | ✅ | ✅ | ✅ | ✅ | ❌ | |
| xai | ❌ | ✅ | ✅ | ✅ | ✅ | ❌ | xAI/Grok |
| zai | ❌ | ✅ | ✅ | ✅ | ✅ | ❌ | Zhipu GLM - supports thinking mode |
| zen | ❌ | ✅ | ✅ | ✅ | ✅ | ❌ | |

## Capability Definitions

- **Stream Options**: Support for `stream_options` parameter (chunk size, include usage, etc.)
- **Auto Tool Choice**: Support for `tool_choice: "auto"` automatic tool selection
- **Content Arrays**: Support for multi-modal content arrays (text + images)
- **Tool Calls**: Support for OpenAI-style function/tool calling
- **Reasoning**: Support for thinking/reasoning tokens (e.g., DeepSeek v4, o1-style models)
- **Vision**: Support for image inputs in messages

## Adding New Providers

To add support for a new provider:

1. Add an entry to the `Registry` map in `internal/providers/spec.go`
2. Define a `ProviderSpec` with the appropriate capability flags
3. If the provider requires custom auth, implement in `internal/providers/` package
4. If wire format differs from OpenAI, create an adapter in `internal/adapter/`

See [docs/ADAPTERS.md](ADAPTERS.md) for full adapter documentation.

## Auto-Generated Documentation

This document is generated from the `ProviderSpec` registry. To regenerate:

```bash
go run ./cmd/gen-provider-matrix
```
