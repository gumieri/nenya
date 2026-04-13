# OpenCode Compatibility

## Overview

Nenya works as a transparent OpenAI-compatible proxy for OpenCode. Point OpenCode's local endpoint at Nenya to get secret redaction, multi-provider routing, and fallback chains.

## Setup

### 1. Configure OpenCode

Set the local endpoint environment variable:

```bash
export LOCAL_ENDPOINT=http://localhost:8080/v1
export OPENAI_API_KEY=your-nenya-client-token
```

Or in your OpenCode config:

```json
{
  "provider": "openai",
  "model": "gpt-4o",
  "base_url": "http://localhost:8080/v1"
}
```

### 2. Nenya Config

```json
{
  "providers": {
    "openai": {
      "url": "https://api.openai.com/v1/chat/completions",
      "auth_style": "bearer",
      "route_prefixes": ["gpt-", "o1-", "o3-"],
      "api_key_secret": "OPENAI_API_KEY"
    },
    "deepseek": {
      "url": "https://api.deepseek.com/v1/chat/completions",
      "auth_style": "bearer",
      "route_prefixes": ["deepseek-"],
      "api_key_secret": "DEEPSEEK_API_KEY"
    },
    "ollama": {
      "url": "http://localhost:11434/v1/chat/completions",
      "auth_style": "none",
      "route_prefixes": ["qwen-", "llama-", "codellama-"]
    }
  },
  "agents": {
    "coder": {
      "models": [
        { "provider": "openai", "model": "gpt-4o" },
        { "provider": "deepseek", "model": "deepseek-coder" },
        { "provider": "ollama", "model": "qwen2.5-coder:7b" }
      ],
      "max_retries": 2,
      "cooldown_seconds": 60
    }
  }
}
```

Use `coder` as the model name in OpenCode to get automatic fallback across providers.

### 3. Verify

```bash
curl -s http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer your-client-token" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}],"stream":true}'
```

## Supported Endpoints

| Endpoint | Status | Notes |
|----------|--------|-------|
| `POST /v1/chat/completions` | Full support | Content pipeline, streaming, tool calls |
| `GET /v1/models` | Supported | OpenCode doesn't call this (hardcoded models), but available |
| `POST /v1/responses` | Passthrough | Available if needed |
| `POST /v1/embeddings` | Passthrough | Available if needed |

## Client Detection

OpenCode uses the official OpenAI Go SDK which sets its own `User-Agent`. In Copilot mode, OpenCode sends identifying headers:

| Header | Value |
|--------|-------|
| `User-Agent` | `OpenCode/1.0` (Copilot mode) or OpenAI SDK UA (standard mode) |
| `Editor-Version` | `OpenCode/1.0` (Copilot mode) |
| `Editor-Plugin-Version` | `OpenCode/1.0` (Copilot mode) |

Nenya detects OpenCode via any of these headers containing "opencode". When detected, IDE-aware pipeline behavior activates automatically.

**Note**: When OpenCode uses the standard OpenAI SDK (non-Copilot mode), the `User-Agent` is set by the SDK and does not contain "opencode". In this case, Nenya treats it as a standard client with full pipeline processing. To force IDE detection, set `Editor-Version: OpenCode/1.0` in your provider config or middleware.

## Pipeline Behavior

When OpenCode is detected as an IDE client:

| Stage | Behavior |
|-------|----------|
| **Secret redaction** | Regex redaction skips code inside markdown fences. Prose outside code blocks is still redacted. |
| **Text compaction** | **Skipped**. Preserves whitespace and line-number references in code payloads. |
| **Truncation** | Code-boundary aware — cuts at blank-line boundaries. When `tfidf_query_source` is set, uses TF-IDF relevance scoring instead. |
| **Engine summarization** | Uses code-preserving prompt — only redacts secrets in prose, never restructures code. |
| **Tool calls** | `tool_calls`, `tool_call_id`, `function_call` pass through unmodified. |

## Tool Use

OpenCode sends tools in standard OpenAI format. Multi-turn tool conversations work through Nenya:

```json
{
  "tools": [{ "type": "function", "function": { "name": "bash", "parameters": { ... } } }],
  "messages": [
    { "role": "user", "content": "list files" },
    { "role": "assistant", "tool_calls": [{ "id": "call_1", "type": "function", "function": { "name": "bash", "arguments": "{\"cmd\":\"ls\"}" } }] },
    { "role": "tool", "tool_call_id": "call_1", "content": "file1.go\nfile2.go" },
    { "role": "assistant", "content": "Here are your files..." }
  ]
}
```

## Content Arrays

OpenCode sends user messages as content arrays (not plain strings):

```json
{
  "content": [{ "type": "text", "text": "explain this code" }]
}
```

This is compatible with Nenya's content array handling. For providers without content array support, text is extracted and flattened.

## Reasoning Models

OpenCode sends `reasoning_effort` and `max_completion_tokens` for models with `CanReason: true` (o1, o3, DeepSeek-R1). These pass through Nenya untouched:

```json
{
  "model": "o3-mini",
  "reasoning_effort": "medium",
  "max_completion_tokens": 16384,
  "messages": [...]
}
```

## Stream Options

OpenCode requests `stream_options: { include_usage: true }` to get token usage in the final SSE chunk. This is stripped by the adapter for providers that don't support it (e.g., OpenAI, Groq, Nvidia) and preserved for providers that do (e.g., DeepSeek, OpenRouter).

## Agent Fallback Example

Define a resilient agent that tries multiple providers:

```json
{
  "agents": {
    "smart-coder": {
      "models": [
        { "provider": "openai", "model": "gpt-4o", "max_context": 128000 },
        { "provider": "deepseek", "model": "deepseek-coder", "max_context": 64000 },
        { "provider": "ollama", "model": "qwen2.5-coder:7b", "max_context": 32000 }
      ],
      "max_retries": 2,
      "cooldown_seconds": 120,
      "strategy": "round-robin"
    }
  }
}
```

Use `"model": "smart-coder"` in OpenCode. If OpenAI rate-limits (429), Nenya automatically falls back to DeepSeek, then to local Ollama. Circuit breakers prevent hammering a tripped provider.
