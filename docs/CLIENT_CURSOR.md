# Cursor Compatibility

## Overview

Nenya works as a transparent OpenAI-compatible proxy for Cursor. Configure Cursor to point at Nenya's endpoint and all chat, agent, and autocomplete features work through the gateway with secret redaction, routing, and fallback logic.

## Setup

### 1. Configure Cursor

In Cursor Settings > Models > OpenAI API Key:

- **Base URL**: `http://localhost:8080/v1` (or your Nenya address)
- **API Key**: Your Nenya `client_token` from `config.json`

### 2. Nenya Config

```json
{
  "providers": {
    "openai": {
      "url": "https://api.openai.com/v1/chat/completions",
      "auth_style": "bearer",
      "route_prefixes": ["gpt-", "o1-", "o3-", "o4-"],
      "api_key_secret": "OPENAI_API_KEY"
    },
    "anthropic": {
      "url": "https://api.anthropic.com/v1/chat/completions",
      "auth_style": "bearer",
      "route_prefixes": ["claude-"],
      "api_key_secret": "ANTHROPIC_API_KEY"
    },
    "gemini": {
      "url": "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions",
      "auth_style": "bearer+x-goog",
      "route_prefixes": ["gemini-"],
      "api_key_secret": "GEMINI_API_KEY"
    }
  }
}
```

### 3. Verify

```bash
curl -s http://localhost:8080/v1/models \
  -H "Authorization: Bearer your-client-token" | jq .
```

Cursor calls `/v1/models` to discover available models. The response includes all agent names and model prefixes configured in Nenya.

## Supported Endpoints

| Endpoint | Status | Notes |
|----------|--------|-------|
| `POST /v1/chat/completions` | Full support | Content pipeline, streaming, tool calls |
| `GET /v1/models` | Full support | Model catalog from agents + providers |
| `POST /v1/responses` | Passthrough | Transparent proxy, no content pipeline |
| `POST /v1/embeddings` | Passthrough | If needed by extensions |

## Pipeline Behavior

Cursor is detected as an IDE client via `User-Agent`. The following pipeline adaptations apply automatically:

| Stage | Behavior |
|-------|----------|
| **Secret redaction** | Regex redaction skips code inside markdown fences (` ``` `). Prose and documentation outside code blocks are still redacted. |
| **Text compaction** | **Skipped**. Cursor carefully formats payloads with line-number references; collapsing whitespace would break them. |
| **Truncation** | Code-boundary aware — cuts at blank-line boundaries between functions/blocks. When `tfidf_query_source` is set, uses TF-IDF relevance scoring to keep the most relevant blocks instead. |
| **Engine summarization** | Uses code-preserving prompt — only redacts secrets in prose, never restructures or summarizes code. |
| **Tool calls** | `tool_calls`, `tool_call_id`, `function_call` pass through unmodified. |

## Reasoning Models

Cursor sends `reasoning_effort` and `max_completion_tokens` for reasoning models (o1, o3, o4-mini). These pass through Nenya untouched to the upstream provider.

## Tool Use / Agent Mode

Cursor's agent mode sends tool definitions in standard OpenAI format:

```json
{
  "tools": [
    {
      "type": "function",
      "function": {
        "name": "read_file",
        "parameters": { "type": "object", "properties": { "path": { "type": "string" } } }
      }
    }
  ],
  "messages": [
    { "role": "assistant", "tool_calls": [{ "id": "call_123", "type": "function", "function": { "name": "read_file", "arguments": "{...}" } }] },
    { "role": "tool", "tool_call_id": "call_123", "content": "file contents..." }
  ]
}
```

All tool call fields pass through Nenya's sanitization and adapter layers unmodified. Tool call arguments in SSE streaming responses are not filtered by the stream security filter.

## Content Types

Cursor can send mixed content arrays (text + images):

```json
{
  "content": [
    { "type": "text", "text": "What's in this image?" },
    { "type": "image_url", "image_url": { "url": "data:image/png;base64,..." } }
  ]
}
```

Nenya handles `image_url` content types — they are counted as `[image]` for token estimation and preserved in the payload. Providers without content array support have text extracted and non-text types dropped (with a warning logged).

## Large Payloads

Cursor sends full file contents, git diffs, and multi-file context. This can easily exceed Nenya's soft limit (derived from the target model's `max_context`). The 3-tier pipeline handles this:

1. **Below soft limit**: pass through unchanged
2. **Between soft/hard**: send to Ollama for privacy-preserving summarization (code structure preserved for IDE clients)
3. **Above hard limit**: truncate (TF-IDF relevance-scored when `tfidf_query_source` is set, otherwise code-boundary middle-out), then summarize. If TF-IDF reduces payload below `soft_limit`, engine call is skipped entirely.

If Ollama is unavailable and `skip_on_engine_failure` is `true` (default), the original payload is forwarded unchanged.
