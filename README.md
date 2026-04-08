<img width="2752" height="1536" alt="nenya" src="https://github.com/user-attachments/assets/bd518ded-2b65-42f9-866e-5a670cf9dbb1" />

# Nenya AI Gateway

![go-version] [![License]][license] ![zero-deps] [![CI]][ci] [![CodeQL]][codeql] [![Release]][release] [![Sponsor]][sponsor]

A lightweight, highly secure AI API Gateway/Proxy written in Go. Acts as transparent middleware between local AI coding clients (OpenCode/Aider) and upstream LLM providers (Gemini, DeepSeek, z.ai).

Nenya acts as a **silent guardian** for your AI interactions. Its core strength is the **"Bouncer" mechanism**: like the Ring of Adamant shielding Lothlórien, it intercepts massive payloads to discern essential context and redact sensitive data locally, before forwarding the refined essence to upstream cloud providers.

## 🛡️ Features

### 🗺️ Routing & Providers

- **Config-driven provider registry** — add providers (OpenAI, Anthropic, etc.) via JSON config + secrets, zero code changes
- **Built-in model registry** — reference models by string shorthand (e.g., `"deepseek-reasoner"`) with automatic provider/context resolution
- **Dynamic routing** based on model name prefixes configured per provider, with direct ModelRegistry lookups taking priority
- **Gemini compatibility** — automatic model name mapping, SSE transformation (index field injection, thought signature preservation)

### 🔐 Security & Privacy

- **Tier-0 regex secret filter** — always-on regex-based redaction of AWS keys, GitHub tokens, passwords, etc.
- **3-Tier UTF-8 safe pipeline**:
  - **Tier 1** (pass-through): payloads under `soft_limit` characters
  - **Tier 2** (engine only): payloads between `soft_limit` and `hard_limit` — summarized locally
  - **Tier 3** (truncation + engine): payloads over `hard_limit` — middle-out truncation then summarization
- **Context window compaction** — sliding window summarization of old messages
- **Hardened security** — strict timeouts, request size limits, hop-by-hop header stripping, panic recovery
- **Systemd credential management** — API keys loaded from `CREDENTIALS_DIRECTORY`

### ⚡ Performance & Reliability

- **Zero external dependencies** — Go standard library only
- **Rate limiting** per upstream host (RPM/TPM)
- **Transparent SSE streaming** — flawless pipe of upstream Server-Sent Events to the client

### 🤖 Agent System

- **Agent system prompts** — inject custom system prompts per agent (inline or file-based)
- **Per-model max_tokens injection** — `max_tokens` injected from `ModelRegistry.MaxOutput` when client doesn't set it
- **Fallback chains** — agent-level strategy with per-provider cooldown and automatic failover

## Configuration

### `config.json`

See [`example.config.json`](example.config.json) for a fully-documented example or [`minimal_example.config.json`](minimal_example.config.json) for the smallest possible config. Full reference in [`CONFIGURATION.md`](CONFIGURATION.md).

### Minimal Configuration

The smallest useful configuration — only agents with string shorthand models, everything else uses built-in defaults:

```json
{
  "agents": {
    "plan": {
      "strategy": "fallback",
      "models": ["deepseek-reasoner"]
    },
    "build": {
      "strategy": "fallback",
      "models": ["glm-5-turbo"]
    }
  }
}
```

### Full Configuration

```json
{
  "server": {
    "listen_addr": ":8080",
    "max_body_bytes": 10485760
  },
  "governance": {
    "ratelimit_max_tpm": 250000,
    "ratelimit_max_rpm": 15,
    "context_soft_limit": 4000,
    "context_hard_limit": 24000,
    "truncation_strategy": "middle-out",
    "keep_first_percent": 15.0,
    "keep_last_percent": 25.0
  },
  "security_filter": {
    "enabled": true,
    "redaction_label": "[REDACTED]",
    "engine": {
      "provider": "ollama",
      "model": "qwen2.5-coder:7b",
      "system_prompt_file": "./prompts/privacy_filter.md",
      "timeout_seconds": 600
    }
  },
  "agents": {
    "build": {
      "strategy": "fallback",
      "cooldown_seconds": 60,
      "system_prompt": "Reply with maximum brevity. Code only.",
      "models": [
        {
          "provider": "gemini",
          "model": "gemini-3.1-flash-lite-preview",
          "max_context": 128000
        },
        {
          "provider": "deepseek",
          "model": "deepseek-reasoner",
          "max_context": 128000
        }
      ]
    }
  },
  "providers": {
    "gemini": {
      "url": "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions",
      "auth_style": "bearer+x-goog",
      "route_prefixes": ["gemini-"]
    }
  }
}
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

**config.json:**

```json
{
  "providers": {
    "openai": {
      "url": "https://api.openai.com/v1/chat/completions",
      "route_prefixes": ["gpt-", "o3-", "o4-"],
      "auth_style": "bearer"
    }
  }
}
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

### Configuration Validation

Before starting the gateway, validate your configuration and API keys:

```bash
CREDENTIALS_DIRECTORY=/path/to/creds ./nenya -config config.json -validate
```

This checks Ollama engine health, provider endpoint reachability, and API key validity.

## Deployment

### Systemd Service

A hardened systemd service file is included: [`nenya.service`](nenya.service). It uses `DynamicUser` and strict sandboxing.

Installation via mise:

```bash
sudo mise run install
```

This will:

1. Build the binary and install to `/usr/local/bin/nenya`
2. Copy `example.config.json` to `/etc/nenya/config.json` (only if not already present)
3. Copy `nenya.service` to `/etc/systemd/system/nenya.service`
4. Reload systemd

You must then create the secrets JSON file as described in [`SECRETS_FORMAT.md`](SECRETS_FORMAT.md) and enable the service:

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

## API Endpoints

All `/v1/*` endpoints require `Authorization: Bearer <client_token>`.

### `POST /v1/chat/completions`

OpenAI-compatible chat completions with SSE streaming, Ollama interception, and agent fallback chains.

```json
{
  "model": "build",
  "messages": [
    { "role": "user", "content": "Explain quantum computing in one sentence." }
  ]
}
```

### `GET /v1/models`

Returns all available models: agent names, individual models from agent chains, and provider route prefixes.

```bash
curl -H "Authorization: Bearer $TOKEN" http://localhost:8080/v1/models
```

### `POST /v1/embeddings`

Passthrough proxy for embeddings requests. Routes via provider registry, no Ollama interception or SSE.

### `GET /healthz`

Health check (no auth required). Returns JSON with engine status.

### `GET /statsz`

Token usage statistics (no auth required). Returns per-model request counts, input/output tokens, and error counts.

### Model Routing

| Prefix                                                | Provider                  |
| ----------------------------------------------------- | ------------------------- |
| `gemini-*`                                            | Gemini (Google AI Studio) |
| `deepseek-*`                                          | DeepSeek                  |
| `zai-*`, `glm-*`                                      | z.ai                      |
| `llama-*`, `llama3-*`, `mixtral-*`, `whisper-*`       | Groq                      |
| `meta-llama/*`, `mistralai/*`, `qwen/*`, `together/*` | Together                  |

Models not matching any prefix fall back to the `zai` provider.

Gemini model names are automatically mapped (e.g., `gemini-3-flash` to `gemini-3-flash-preview`).

## Development

### Prerequisites

- Go 1.26+
- Ollama with `qwen2.5-coder:7b` (or adjust config)

### Running Tests

```bash
go test ./...
go vet ./...
go fmt ./...
```

### Architecture

- **`main.go`** — Entry point, server bootstrap with graceful shutdown
- **`config.go`** — Configuration types, JSON loading, provider registry
- **`gateway.go`** — NenyaGateway struct, HTTP clients, tokenization
- **`proxy.go`** — HTTP handler, 3-tier pipeline, upstream forwarding
- **`routing.go`** — Dynamic routing, agent fallback chains, API key injection, model mapping
- **`transform.go`** — SSE response transformation (Gemini index injection, thought signature preservation), usage extraction
- **`filter.go`** — Tier-0 regex secret redaction, middle-out truncation
- **`ratelimit.go`** — Token-bucket rate limiter (RPM/TPM)
- **`validate.go`** — Configuration validation, provider health checks
- **`stats.go`** — Token usage tracking, /statsz and /healthz endpoints
- **`logger.go`** — slog setup with TTY/systemd auto-detection

## Sponsor / Support this Project

If Nenya saves you time or money on API costs, consider supporting its development:

- **GitHub Sponsors**: [https://github.com/sponsors/gumieri](https://github.com/sponsors/gumieri)
- **Pix (Brazil)**: [`rgumieri@gmail.com`](https://nubank.com.br/cobrar/2jm8a/69d54dab-4530-4e09-a531-e959e45fb6d8)

## AI Collaboration

This project was built in collaboration with AI coding tools. The architecture, security patterns, and codebase reflect rapid iterative development guided by human engineering decisions. Every line of code has been reviewed, tested, and validated by the maintainer.

---

## License & Disclaimer

This project is licensed under the Apache 2.0 License — see the [LICENSE](LICENSE) file for details.

Before using Nenya with autonomous agents in production environments, please read the [DISCLAIMER](DISCLAIMER.md).

---

[go-version]: https://img.shields.io/badge/Go-1.26-00ADD8?logo=golang&logoColor=white
[license]: https://img.shields.io/badge/License-Apache_2.0-5B44C2?logo=apache&logoColor=white
[zero-deps]: https://img.shields.io/badge/Dependencies-0-2EA043?logo=golang&logoColor=white
[ci]: https://img.shields.io/github/actions/workflow/status/gumieri/nenya/ci.yml?branch=main&logo=github&logoColor=white&label=CI
[codeql]: https://img.shields.io/github/actions/workflow/status/gumieri/nenya/codeql.yml?branch=main&logo=github&logoColor=white&label=CodeQL
[release]: https://img.shields.io/github/v/release/gumieri/nenya?logo=github&logoColor=white&sort=semver
[sponsor]: https://img.shields.io/badge/Sponsor-GitHub-EA4AAA?logo=githubsponsors&logoColor=white
