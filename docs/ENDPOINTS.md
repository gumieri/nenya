# Extension API Endpoints

Nenya provides first-class proxy support for several OpenAI-compatible API endpoints beyond chat completions. These endpoints are routed to a configured provider (defaulting to `openai` unless otherwise noted), automatically injecting the provider's API key.

## Authentication

All endpoints require `Authorization: Bearer <client_token>`.

## Endpoints

### Image Generation

```
POST /v1/images/generations
```

Generate images from text prompts. Proxied to the upstream provider's `/v1/images/generations` endpoint.

**Default provider:** `openai`

**Example:**
```bash
curl -X POST -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"prompt":"A beautiful sunset","n":1,"size":"1024x1024"}' \
  http://localhost:8080/v1/images/generations
```

### Audio Transcription

```
POST /v1/audio/transcriptions
```

Transcribe audio files to text. Sends multipart/form-data to the upstream provider's `/v1/audio/transcriptions` endpoint. The original `Content-Type` (including boundary) is preserved.

**Default provider:** `openai`

**Example:**
```bash
curl -X POST -H "Authorization: Bearer $TOKEN" \
  -F "file=@audio.mp3" \
  -F "model=whisper-1" \
  http://localhost:8080/v1/audio/transcriptions
```

### Text-to-Speech

```
POST /v1/audio/speech
```

Generate audio from text. Proxied to the upstream provider's `/v1/audio/speech` endpoint. Returns the audio stream (e.g., `audio/mpeg`) directly.

**Default provider:** `openai`

**Example:**
```bash
curl -X POST -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"model":"tts-1","input":"Hello world","voice":"alloy"}' \
  http://localhost:8080/v1/audio/speech -o speech.mp3
```

### Content Moderation

```
POST /v1/moderations
```

Classify text for potentially harmful content. Proxied to the upstream provider's `/v1/moderations` endpoint.

**Default provider:** `openai`

**Example:**
```bash
curl -X POST -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"input":"I want to harm someone"}' \
  http://localhost:8080/v1/moderations
```

### Re-ranking

```
POST /v1/rerank
```

Re-rank documents by relevance to a query. Proxied to the upstream provider's `/v1/rerank` endpoint.

**Default provider:** `cohere` (falls back to any available provider)

**Example:**
```bash
curl -X POST -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "model":"rerank-english-v2.0",
    "query":"What is the capital of France?",
    "documents":["Paris is the capital.","The Eiffel Tower is in Paris."]
  }' \
  http://localhost:8080/v1/rerank
```

### Anthropic Messages API

```
POST /v1/messages
```

Anthropic Messages API endpoint with bidirectional format conversion between OpenAI and Anthropic wire formats. Supports Anthropic-native clients directly. Proxied to the upstream provider's `/v1/messages` endpoint.

**Default provider:** `anthropic` (falls back to any available provider)

**Example:**
```bash
curl -X POST -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -H "anthropic-version: 2023-06-01" \
  -d '{
    "model":"claude-3-5-sonnet-20241022",
    "max_tokens":1024,
    "messages":[{"role":"user","content":"Hello"}]
  }' \
  http://localhost:8080/v1/messages
```

### Files

```
POST   /v1/files
GET    /v1/files
GET    /v1/files/{file_id}
DELETE /v1/files/{file_id}
```

File management operations for uploading, listing, retrieving, and deleting files. Proxied to the upstream provider's files endpoints.

**Default provider:** `openai` (falls back to any available provider)

**Example (upload):**
```bash
curl -X POST -H "Authorization: Bearer $TOKEN" \
  -F "file=@document.pdf" \
  -F "purpose=assistants" \
  http://localhost:8080/v1/files
```

### Batch Operations

```
POST /v1/batches
```

Batch API operations for processing multiple requests asynchronously. Proxied to the upstream provider's `/v1/batches` endpoint.

**Default provider:** `openai` (falls back to any available provider)

**Example:**
```bash
curl -X POST -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "input_file_id":"file-abc123",
    "endpoint":"/v1/chat/completions",
    "completion_window":"24h"
  }' \
  http://localhost:8080/v1/batches
```

### Responses API

```
POST /v1/responses
```

Responses API passthrough endpoint. Proxied to the upstream provider's `/v1/responses` endpoint.

**Default provider:** `openai` (falls back to any available provider)

**Example:**
```bash
curl -X POST -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "input":"What is the capital of France?",
    "model":"gpt-4o"
  }' \
  http://localhost:8080/v1/responses
```

## Agent-to-Agent (A2A)

```
POST /v1/a2a
```

Agent-to-Agent communication protocol (Google A2A). Proxied to the upstream provider's `/v1/a2a` endpoint.

**Default provider:** `gemini` (falls back to any available provider)

**Example:**
```bash
curl -X POST -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"agent_id":"agent-123","message":"Hello from agent A"}' \
  http://localhost:8080/v1/a2a
```

## RBAC Enforcement

All `/v1/*` endpoints enforce role-based access control (RBAC) when using API keys:

- **Roles**: `admin` (unrestricted access), `user` (all endpoints except admin-only), `read-only` (GET requests only)
- **Agent Scoping**: API keys can be restricted to specific agents via `allowed_agents` list
- **Endpoint Allowlists**: API keys can define `allowed_endpoints` for fine-grained access control (HTTP method + path)
- **Key Validation**: Keys must be enabled and not expired
- **Primary Token**: The primary token (`NENYA_PRIMARY_TOKEN`) bypasses RBAC restrictions

See the main documentation for RBAC configuration details.

## Provider Selection

For each endpoint, the gateway selects a provider in this order:

1. Preferred provider (named above) — if configured and has an API key
2. Any configured provider with an API key (first found)

To configure a specific provider for an extension endpoint, add it to your config's `providers` section with the desired name.

## Custom Endpoint URLs via FormatURLs

Providers can override the upstream URL for any extension endpoint using the `format_urls` map in the provider configuration:

```json
{
  "providers": {
    "my-openai": {
      "url": "https://api.my-openai.com/v1/chat/completions",
      "format_urls": {
        "images/generations": "https://images.my-openai.com/v1/images/generations",
        "moderations": "https://moderation.my-openai.com/v1/moderations"
      }
    }
  }
}
```
