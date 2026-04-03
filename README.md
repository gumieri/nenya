# Nenya AI Gateway

A lightweight, highly secure AI API Gateway/Proxy written in Go. Acts as transparent middleware between local AI coding clients (OpenCode/Aider) and upstream LLM providers (Gemini, DeepSeek, z.ai).

Its **superpower** is the **"Bouncer" mechanism**: intercepting massive HTTP payloads, routing them to a local Ollama instance (`qwen2.5-coder`) for summarization and PII/credential redaction, and forwarding the sanitized, much smaller payload to upstream cloud AI using SSE streaming.

## Features

- **Config-driven provider registry** — add providers (OpenAI, Anthropic, etc.) via TOML config + secrets, zero code changes
- **Dynamic routing** based on model name prefixes configured per provider
- **Tier-0 regex secret filter**: always-on regex-based redaction of AWS keys, GitHub tokens, passwords, etc. (configurable patterns)
- **3-Tier UTF-8 safe pipeline**:
  - **Tier 1** (pass-through): payloads under `soft_limit` characters
  - **Tier 2** (Ollama only): payloads between `soft_limit` and `hard_limit` characters — summarized locally
  - **Tier 3** (truncation + Ollama): payloads over `hard_limit` characters — middle-out truncation — summarization
- **Zero-dependency core** (except `github.com/pelletier/go-toml/v2` for config and `github.com/pkoukk/tiktoken-go` for token counting)
- **Hardened security**: strict timeouts, request size limits, hop-by-hop header stripping, panic recovery
- **Systemd credential management**: API keys loaded from `CREDENTIALS_DIRECTORY`
- **Rate limiting** per upstream host (RPM/TPM)
- **Gemini free-tier compatibility**: automatic mapping of AI Studio UI model names to API-accepted names

## Configuration

### `config.toml`

```toml
[server]
listen_addr = ":8080"
max_body_bytes = 10485760

[interceptor]
soft_limit = 4000           # characters (runes) - Tier 1 pass-through
hard_limit = 24000          # characters (runes) - Tier 3 truncation threshold
truncation_strategy = "middle-out"
keep_first_percent = 15.0
keep_last_percent = 25.0

[ratelimit]
max_tpm = 250000            # tokens per minute (approximate)
max_rpm = 15                # requests per minute

[ollama]
url = "http://127.0.0.1:11434/api/generate"
model = "qwen2.5-coder:7b"
system_prompt = "You are a data privacy filter. Summarize the following log/code error in 5 lines. REMOVE any IP addresses, AWS keys (AKIA...), or passwords. Keep only the technical core of the error."

[filter]
# Tier-0 regex-based secret redaction (runs on every request)
enabled = true
redaction_label = "[REDACTED]"

# Provider Registry
# Built-in providers: gemini, deepseek, zai, groq, together, ollama
# Override or add new providers here:
#
# [providers.openai]
# url = "https://api.openai.com/v1/chat/completions"
# route_prefixes = ["gpt-", "o3-", "o4-"]
# auth_style = "bearer"
```

### Secrets (`secrets` JSON file)

Secrets are loaded via systemd credentials (`CREDENTIALS_DIRECTORY`). Create a JSON file with the following structure:

```json
{
  "client_token": "your-client-auth-token",
  "provider_keys": {
    "gemini": "AIza...",
    "deepseek": "sk-...",
    "zai": "..."
  }
}
```

At minimum `client_token` must be present; `provider_keys` entries can be omitted for providers you don't use. See [`SECRETS_FORMAT.md`](SECRETS_FORMAT.md) for full details.

### Adding a New Provider (e.g., OpenAI)

No Go code changes needed. Add two sections:

**config.toml:**
```toml
[providers.openai]
url = "https://api.openai.com/v1/chat/completions"
route_prefixes = ["gpt-", "o3-", "o4-"]
auth_style = "bearer"
```

**secrets.json:**
```json
{
  "provider_keys": {
    "openai": "sk-proj-..."
  }
}
```

Models matching the `route_prefixes` (e.g., `gpt-4o`, `o3-mini`) will now be routed to OpenAI automatically.

## Deployment

### Systemd Service

A hardened systemd service file is included: [`nenya.service`](nenya.service). It uses `DynamicUser` and strict sandboxing.

Installation via mise:

```bash
sudo mise run install
```

This will:
1. Build the binary and install to `/usr/local/bin/nenya`
2. Copy `example.config.toml` to `/etc/nenya/config.toml`
3. Copy `nenya.service` to `/etc/systemd/system/nenya.service`
4. Reload systemd

You must then create the secrets JSON file as described in [`SECRETS_FORMAT.md`](SECRETS_FORMAT.md) at `/etc/nenya/secrets.json` and enable the service:

```bash
sudo systemctl enable --now nenya
```

### Building from Source

```bash
git clone https://git.0ur.uk/nenya.git
cd nenya
go build -o nenya .
```

Or for a quick local test with dummy secrets:

```bash
mise run run
```

## API Usage

All endpoints require `Authorization: Bearer <client_token>`.

### `POST /v1/chat/completions`

OpenAI-compatible chat completions with SSE streaming, Ollama interception, and agent fallback chains.

```json
{
  "model": "zai-coding-plan/glm-5",
  "messages": [
    {"role": "user", "content": "Explain quantum computing in one sentence."}
  ]
}
```

### `GET /v1/models`

Returns all available models: agent names (owned by `nenya`), individual models from agent chains, and provider route prefixes.

```bash
curl -H "Authorization: Bearer $TOKEN" http://localhost:8080/v1/models
```

### `POST /v1/embeddings`

Passthrough proxy for embeddings requests. Routes via provider registry, no Ollama interception or SSE.

```json
{
  "model": "text-embedding-3-small",
  "input": "hello world"
}
```

### `GET /healthz`

Health check (no auth required). Returns JSON with Ollama status. 200 if Ollama is reachable, 503 if degraded.

```json
{"status":"ok","ollama":{"status":true}}
```

### `GET /statsz`

Token usage statistics (no auth required). Returns per-model request counts, input/output tokens, and error counts since startup.

```json
{
  "uptime_seconds": 3600,
  "models": {
    "gemini-2.5-flash": {"requests": 42, "input_tokens": 18000, "output_tokens": 5200, "errors": 1}
  }
}
```

### Model Routing

Supported model prefixes (built-in providers):

| Prefix | Provider |
|--------|----------|
| `gemini-*` | Gemini (Google AI Studio) |
| `deepseek-*` | DeepSeek |
| `zai-*`, `glm-*` | z.ai |
| `llama-*`, `llama3-*`, `mixtral-*`, `whisper-*` | Groq |
| `meta-llama/*`, `mistralai/*`, `qwen/*`, `together/*` | Together |

Models not matching any prefix fall back to the `zai` provider.

The gateway will automatically map Gemini model names (e.g., `gemini-3-flash` to `gemini-3-flash-preview`) to match the API.

## Development

### Prerequisites

- Go 1.26+
- Ollama with `qwen2.5-coder:7b` (or adjust `config.toml`)

### Running Tests

```bash
go test ./...
go vet ./...
go fmt ./...
```

### Architecture

- **`main.go`** — Entry point, server bootstrap with graceful shutdown
- **`config.go`** — Configuration types, TOML/JSON loading, provider registry
- **`gateway.go`** — NenyaGateway struct, HTTP clients, tokenization
- **`proxy.go`** — HTTP handler, 3-tier pipeline, Ollama interceptor, upstream forwarding
- **`routing.go`** — Dynamic routing, agent fallback chains, API key injection, model mapping
- **`transform.go`** — SSE response transformation (Gemini tool_calls normalization), usage extraction
- **`filter.go`** — Tier-0 regex secret redaction, middle-out truncation
- **`ratelimit.go`** — Token-bucket rate limiter (RPM/TPM)
- **`stats.go`** — Token usage tracking, /statsz and /healthz endpoints
- **`logger.go`** — slog setup with TTY/systemd auto-detection

### Security Design

- **No global variables** — state encapsulated in `NenyaGateway` struct
- **Strict timeouts** — `http.Client` with 30s dial, 30s TLS, 120s total timeout
- **Request size limits** — `http.MaxBytesReader` prevents memory exhaustion
- **Header sanitization** — hop-by-hop headers stripped before forwarding
- **Error safety** — panic recovery in HTTP handler, no stack traces exposed

## Project Structure

- `main.go` — Entry point, server bootstrap
- `config.go` — Configuration types, TOML/JSON loading, provider registry
- `gateway.go` — NenyaGateway struct, HTTP clients, tokenization
- `proxy.go` — HTTP handler, 3-tier pipeline, upstream forwarding
- `routing.go` — Dynamic routing, agent fallback chains, model mapping
- `transform.go` — SSE response transformation, usage extraction
- `filter.go` — Tier-0 regex secret redaction, middle-out truncation
- `ratelimit.go` — Token-bucket rate limiter
- `stats.go` — Token usage tracking, /statsz and /healthz endpoints
- `logger.go` — slog setup with TTY/systemd auto-detection
- `config.toml` — Default configuration (editable)
- `example.config.toml` — Example with comments
- `nenya.service` — Systemd service unit with sandboxing
- `SECRETS_FORMAT.md` — Secrets file format and security notes
- `mise.toml` — Build, install, test, release tasks
- `README.md` — This file

## License

MIT
