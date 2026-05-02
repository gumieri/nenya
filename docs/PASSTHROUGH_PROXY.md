# Passthrough Proxy

The `/proxy/{provider}/*` endpoint enables raw proxying to any provider endpoint, bypassing Nenya's content pipeline.

## Endpoint

```
POST /proxy/{provider}/{path}
```

Where:
- `{provider}` — provider name (e.g., `anthropic`, `openai`, `gemini`)
- `{path}` — any path on the provider's API (e.g., `v1/models`, `v1/files`)

## Authentication

All `/proxy/*` routes require `Authorization: Bearer <client_token>`.

The proxy automatically injects the provider-specific API key from your secrets configuration.

## Usage Examples

### List Anthropic models

```bash
curl -H "Authorization: Bearer $CLIENT_TOKEN" \
  http://localhost:8080/proxy/anthropic/v1/models
```

### Upload file to OpenAI

```bash
curl -X POST \
  -H "Authorization: Bearer $CLIENT_TOKEN" \
  -F "file=@document.pdf" \
  -F "purpose=fine-tune" \
  http://localhost:8080/proxy/openai/v1/files
```

### Query Gemini pricing

```bash
curl -H "Authorization: Bearer $CLIENT_TOKEN" \
  http://localhost:8080/proxy/gemini/v1/models
```

### Generic proxy

```bash
curl -H "Authorization: Bearer $CLIENT_TOKEN" \
  http://localhost:8080/proxy/{provider}/{path}
```

## Features

| Feature | Description |
|---------|-------------|
| **All HTTP methods** | GET, POST, PUT, PATCH, DELETE, HEAD, OPTIONS |
| **Auth injection** | Provider-specific API keys automatically injected |
| **Header sanitization** | Strips hop-by-hop, auth, and host headers |
| **SSE streaming** | Auto-detects `text/event-stream` and pipes as-is |
| **Rate limiting** | Respects upstream host RPM/TPM limits |
| **Usage tracking** | Records requests in metrics |
| **Metrics** | Increments `nenya_proxy_requests_total` |

## Bypassed Features

The passthrough proxy bypasses:
- Content pipeline (redaction, compaction, summarization)
- Circuit breaker
- Retry logic
- Agent routing

This is useful for operations that don't fit the chat completions model:
- File uploads/downloads
- Batch processing
- Administrative endpoints
- Model listing

## Error Handling

Passthrough errors return the upstream response directly:

```bash
curl -H "Authorization: Bearer $CLIENT_TOKEN" \
  http://localhost:8080/proxy/anthropic/v1/messages
```

Response includes upstream status and headers:

```
HTTP/1.1 400 Bad Request
Content-Type: application/json

{"error": {"type": "invalid_request_error", "message": "..."}}
```

## Rate Limiting

Passthrough requests are subject to the same rate limiting as chat completions:

- `governance.ratelimit_max_rpm` — requests per minute per upstream host
- `governance.ratelimit_max_tpm` — tokens per minute per upstream host

When rate limited by the upstream, Nenya returns `429 Too Many Requests`.
