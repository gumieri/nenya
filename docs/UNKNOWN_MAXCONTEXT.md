# Unknown MaxContext Behavior

## Overview

When a model's `max_context` (context window size) is not known to Nenya (value `<= 0`), the gateway takes a different approach to avoid silent data loss.

## Problem Solved

Previously, when `MaxContext` was unknown (common for Ollama models and other local providers that don't report this field), Nenya would default to aggressive truncation thresholds:
- `softLimit = 4000` tokens (triggers Ollama summarization)
- `hardLimit = 24000` tokens (absolute truncation limit)

This caused silent context loss: the conversation history was truncated before sending to the model, resulting in disconnected responses without any indication that data was lost.

## New Behavior

### 1. Proactive Truncation Disabled

When `MaxContext` is unknown, the gateway sets `softLimit = 0` and `hardLimit = 0`, which means:
- **No proactive truncation**: The full payload is sent to the upstream provider as-is
- **Interceptors skip**: The BouncerInterceptor and other token-budget interceptors are disabled (their `CanHandle` guards check `TokenCount >= SoftLimit`)

### 2. Upstream Error Fallback

If the upstream provider cannot handle the payload size (e.g., Ollama returns `context_length_exceeded`), Nenya's existing retry logic kicks in:

1. **Detect context limit error**: `handleContextLimitError` in `internal/proxy/retry.go` detects the error
2. **Attempt summarization retry**: If `AutoRetryOnContextLimitEnabled` is `true`, the gateway calls `attemptContextLimitSummarization` to compact the payload
3. **Retry with compacted payload**: The summarized payload is retried with the same provider
4. **Surface error if failed**: If summarization fails or is disabled, the error is returned to the client

### 3. Logging

The gateway logs warnings at two levels:

**Startup warning** (once per provider after discovery completes):
```
WARN provider has models without max_context configured — proactive truncation disabled; upstream may return context_length_exceeded (retries with summarization will be attempted)
  provider=ollama models=`qwen3:14b`, `llama2` count=2
```

**Per-request warning** (when a request hits a model without MaxContext):
```
WARN MaxContext unknown for model, proactive truncation disabled — configure max_context to enable
  model=qwen3:14b provider=ollama
```

### 4. Configuration

To avoid `context_length_exceeded` errors, configure `max_context` for your models:

**In agent config** (`agents.json`):
```json
{
  "agent-name": {
    "models": [
      {
        "provider": "ollama",
        "model": "qwen3:14b",
        "max_context": 32000
      }
    ]
  }
}
```

**In provider config** (if the provider supports it):
```json
{
  "providers": {
    "ollama": {
      "url": "http://localhost:11434/v1",
      "max_context": {
        "qwen3:14b": 32000,
        "llama2:7b": 4096
      }
    }
  }
}
```

## Behavior Comparison

| Scenario | Before | After |
|----------|--------|-------|
| Ollama model, MaxContext=0, payload=50K tokens | Silent truncation to 24K → hallucination | Full payload sent → `context_length_exceeded` → summarization retry |
| Ollama model, MaxContext=0, payload=2K tokens | No truncation (under limits) | No truncation (unchanged) |
| Cloud provider, MaxContext=128K, payload=50K tokens | No truncation (under limits) | No truncation (unchanged) |
| Cloud provider, MaxContext=128K, payload=200K tokens | Proactive truncation to 96K | Proactive truncation to 96K (unchanged) |

## Trade-offs

### Advantages

- **No silent data loss**: Users see explicit errors instead of hallucinations
- **Automatic recovery**: Existing retry-with-summarization logic handles oversize payloads
- **Better debugging**: Clear warnings point to missing configuration
- **Backward compatible**: Properly configured models behave identically

### Disadvantages

- **More error logs**: Models without `max_context` may generate `context_length_exceeded` errors
- **Retry overhead**: Summarization retry adds latency (same as before, just more visible)

## Monitoring

Monitor these metrics to identify models needing `max_context`:

- `nenya_context_limit_errors_total` — count of `context_length_exceeded` errors by model
- `nenya_summarization_retries_total` — count of summarization retry attempts

If these metrics are high for a particular model, add `max_context` to enable proactive truncation.

## References

- Implementation: `internal/proxy/chat.go` (lines 335-368), `internal/gateway/gateway.go` (lines 28-49), `internal/pipeline/trim.go` (line 26)
- Retry logic: `internal/proxy/retry.go` (lines 312-344)
- Model catalog: `internal/discovery/discovery.go` (lines 8-17)