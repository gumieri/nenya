# 💍 Nenya AI Gateway

A lightweight, highly secure AI API Gateway/Proxy written in Go. Acts as transparent middleware between local AI coding clients (OpenCode/Aider) and upstream LLM providers (Gemini, DeepSeek, z.ai).

Its **superpower** is the **"Bouncer" mechanism**: intercepting massive HTTP payloads, routing them to a local Ollama instance (`qwen2.5-coder`) for summarization and PII/credential redaction, and forwarding the sanitized, much smaller payload to upstream cloud AI using SSE streaming.

## Features

- **Dynamic routing** based on model name (`gemini-*`, `deepseek-*`, others → z.ai)
- **Tier‑0 regex secret filter**: always‑on regex‑based redaction of AWS keys, GitHub tokens, passwords, etc. (configurable patterns)
- **3‑Tier UTF‑8 safe pipeline**:
  - **Tier 1** (pass‑through): payloads under `soft_limit` characters
  - **Tier 2** (Ollama only): payloads between `soft_limit` and `hard_limit` characters → summarized locally
  - **Tier 3** (truncation + Ollama): payloads over `hard_limit` characters → middle‑out truncation → summarization
- **Zero‑dependency** (except `github.com/pelletier/go-toml/v2` for config)
- **Hardened security**: strict timeouts, request size limits, hop‑by‑hop header stripping, panic recovery
- **Systemd credential management**: API keys loaded from `CREDENTIALS_DIRECTORY`
- **Rate limiting** per upstream host (RPM/TPM)
- **Gemini free‑tier compatibility**: automatic mapping of AI Studio UI model names to API‑accepted names

## Configuration

### `config.toml`

```toml
[server]
listen_addr = ":8080"
max_body_bytes = 10485760

[interceptor]
soft_limit = 4000           # characters (runes) – Tier 1 pass‑through
hard_limit = 24000          # characters (runes) – Tier 3 truncation threshold
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
# Tier‑0 regex‑based secret redaction (runs on every request)
enabled = true
redaction_label = "[REDACTED]"
# Patterns are Go regexps, compiled at startup.
# Default patterns cover AWS keys, GitHub tokens, Google OAuth, private keys, etc.
# patterns = [
#   "(?i)AKIA[0-9A-Z]{16}",
#   "(?i)gh(p|o|s)_[a-zA-Z0-9]{36,255}",
#   "(?i)ya29\\.[0-9A-Za-z\\-_]+",
#   "(?i)sk-[a-zA-Z0-9]{48}",
#   "(?i)-----BEGIN (RSA|DSA|EC|PRIVATE) KEY-----",
#   "(?i)(aws_access_key_id|aws_secret_access_key)\\s*=\\s*['\"][^'\"]{10,}['\"]",
#   "(?i)(password|passwd|pwd|secret|token|key)[\\s:=]+['\"][^'\"]{6,}['\"]",
#   "(?i)\\b[0-9]{1,3}\\.[0-9]{1,3}\\.[0-9]{1,3}\\.[0-9]{1,3}\\b",
#   "(?i)[a-f0-9]{32}:",
#   "(?i)SG\\.[a-zA-Z0-9\\-_]{22}\\.[a-zA-Z0-9\\-_]{43}",
# ]

[upstream]
# gemini_url = "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions"
# deepseek_url = "https://api.deepseek.com/v1/chat/completions"
# zai_url = "https://api.z.ai/v1/chat/completions"
# Uncomment to override default upstream URLs
```

### Secrets (`secrets` JSON file)

Secrets are loaded via systemd credentials (`CREDENTIALS_DIRECTORY`). Create a JSON file with the following structure:

```json
{
  "client_token": "your‑client‑auth‑token",
  "gemini_key": "AIza...",
  "deepseek_key": "sk‑...",
  "zai_key": "..."
}
```

At minimum `client_token` must be present; API keys can be omitted if you don’t use that upstream.

## Deployment

### Systemd Service

A hardened systemd service file is included: [`nenya.service`](nenya.service). It uses `DynamicUser` and strict sandboxing.

Installation via Makefile:

```bash
sudo make install
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
make build
```

Or for a quick local test with dummy secrets:

```bash
make run
```

## API Usage

The gateway exposes a single OpenAI‑compatible endpoint:

```
POST /v1/chat/completions
Authorization: Bearer <client_token>
Content-Type: application/json
```

Example request:

```json
{
  "model": "gemini-3-flash",
  "messages": [
    {"role": "user", "content": "Explain quantum computing in one sentence."}
  ]
}
```

Supported model prefixes:

- `gemini-*` → Gemini (Google AI Studio)
- `deepseek-*` → DeepSeek
- any other (including `glm-5`) → z.ai

The gateway will automatically map Gemini model names (e.g., `gemini-3-flash` → `gemini-flash-latest`) to match the free‑tier API.

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

- **`main.go`** – Core gateway logic, routing, interception, forwarding
- **`main_test.go`** – Unit tests for truncation, token counting, routing, transformation
- **`config.toml`** – Default configuration
- **`example.config.toml`** – Example with comments

### Security Design

- **No global variables** – state encapsulated in `NenyaGateway` struct
- **Strict timeouts** – `http.Client` with 30s dial, 30s TLS, 120s total timeout
- **Request size limits** – `http.MaxBytesReader` prevents memory exhaustion
- **Header sanitization** – hop‑by‑hop headers stripped before forwarding
- **Error safety** – panic recovery in HTTP handler, no stack traces exposed

## Project Structure

- `main.go` – Core gateway logic
- `main_test.go` – Unit tests
- `config.toml` – Default configuration (editable)
- `example.config.toml` – Example with comments
- `nenya.service` – Systemd service unit with sandboxing
- `SECRETS_FORMAT.md` – Secrets file format and security notes
- `Makefile` – Build, install, test, release tasks
- `README.md` – This file

## License

MIT