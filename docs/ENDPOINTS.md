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

### Agent-to-Agent (A2A)

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
