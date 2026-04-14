<img width="2752" height="1536" alt="nenya" src="https://github.com/user-attachments/assets/bd518ded-2b65-42f9-866e-5a670cf9dbb1" />

# Nenya AI Gateway

![go-version] [![License]][license] ![zero-deps] [![CI]][ci] [![CodeQL]][codeql] [![Release]][release] [![Sponsor]][sponsor]

A lightweight, highly secure AI API Gateway/Proxy written in Go. Acts as transparent middleware between local AI coding clients (OpenCode/Aider) and upstream LLM providers.

Nenya acts as a **silent guardian** for your AI interactions. Its core strength is the **"Bouncer" mechanism**: like the Ring of Adamant shielding Lothlórien, it intercepts massive payloads to discern essential context and redact sensitive data locally, before forwarding the refined essence to upstream cloud providers.

## Documentation

| Document | Description |
|----------|-------------|
| [Architecture](docs/ARCHITECTURE.md) | Package DAG, request lifecycle, circuit breaker, SSE pipeline, response cache |
| [MCP Integration](docs/MCP_INTEGRATION.md) | MCP server integration, tool discovery, multi-turn execution, auto-search/save |
| [Adapters](docs/ADAPTERS.md) | Provider adapter system, capabilities, auth styles, how to add providers |
| [Configuration](docs/CONFIGURATION.md) | Full config reference with all sections and fields |
| [Demo](docs/DEMO.md) | Quick start, pipeline testing, cache bypass, circuit breaker observability |
| [Secrets Format](docs/SECRETS_FORMAT.md) | Systemd credential management, provider key setup |
| [Security](docs/SECURITY.md) | Vulnerability reporting policy |
| [Disclaimer](docs/DISCLAIMER.md) | Usage terms and liability |

## Features

### Routing & Providers

- **Config-driven provider registry** — add providers via JSON config + secrets, zero code changes
- **Built-in model registry** — reference models by string shorthand with automatic provider/context resolution
- **Dynamic routing** based on model name prefixes, with direct ModelRegistry lookups taking priority
- **Provider adapter system** — clean Go interface for wire format differences, auth injection, response mutation, and error classification across 15+ providers
- **Gemini compatibility** — model name mapping, thought signature preservation, orphaned tool_call cleanup

### Security & Privacy

- **Tier-0 regex secret filter** — always-on redaction of AWS keys, GitHub tokens, passwords, etc.
- **3-Tier UTF-8 safe pipeline** — pass-through, engine summarization, or truncation+summarization based on payload size. TF-IDF relevance-scored truncation optionally skips the engine call entirely.
- **Context window compaction** — sliding window summarization of old messages (supports summarize, truncate, or TF-IDF modes)
- **Hardened security** — strict timeouts, body limits, hop-by-hop header stripping, panic recovery
- **Systemd credential management** — API keys loaded from `CREDENTIALS_DIRECTORY`

### Performance & Reliability

- **Zero external dependencies** — Go standard library only
- **Graceful degradation** — best-effort content pipeline; works without Ollama; never returns 500 for pipeline failures
- **Rate limiting** per upstream host (RPM/TPM)
- **Transparent SSE streaming** — buffer pooling, immediate flush, stall detection (120s idle timeout)
- **Circuit breaker** — per agent+provider+model with Closed/Open/HalfOpen states and exponential backoff
- **In-memory LRU response cache** — deterministic SHA-256 fingerprinting, cache bypass header
- **Exhaustive fallback** — non-retryable errors still try the next provider before giving up

### Agent System

- **Agent fallback chains** — agent-level strategy with circuit breaker and automatic failover
- **System prompts** — inject custom system prompts per agent (inline or file-based)
- **Per-model max_tokens injection** — from ModelRegistry when client doesn't set it
- **MCP tool integration** — connect to MCP servers (MemPalace, mem0, etc.) for tool discovery, auto-search, auto-save, and multi-turn tool execution
- **Long-term memory** — MCP-based memory per agent for persistent memory search and storage

## Quick Start

### Minimal Configuration

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

### Adding a New Provider

No Go code changes needed for OpenAI-compatible providers:

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

See [`docs/ADAPTERS.md`](docs/ADAPTERS.md) for alien-format providers (Bedrock, Vertex, etc.).

### Configuration Validation

```bash
CREDENTIALS_DIRECTORY=/path/to/creds ./nenya -config config.json -validate
```

Full configuration reference: [`docs/CONFIGURATION.md`](docs/CONFIGURATION.md)

## API Endpoints

All `/v1/*` endpoints require `Authorization: Bearer <client_token>`.

| Endpoint | Auth | Description |
|----------|------|-------------|
| `POST /v1/chat/completions` | Bearer | OpenAI-compatible chat with SSE streaming, Ollama interception, agent fallback, MCP multi-turn |
| `GET /v1/models` | Bearer | Available models catalog |
| `POST /v1/embeddings` | Bearer | Passthrough proxy for embeddings |
| `POST /v1/responses` | Bearer | Passthrough proxy for responses API |
| `GET /healthz` | None | Engine health probe |
| `GET /statsz` | None | Token usage, per-model stats, circuit breaker state |
| `GET /metrics` | None | Prometheus-compatible metrics |

## Model Routing

| Prefix | Provider |
|--------|----------|
| `gemini-*` | Gemini (Google AI Studio) |
| `deepseek-*` | DeepSeek |
| `zai-*`, `glm-*` | z.ai |
| `llama-*`, `llama3-*`, `mixtral-*`, `whisper-*` | Groq |
| `meta-llama/*`, `mistralai/*`, `qwen/*`, `together/*` | Together |

Models not matching any prefix fall back to the `zai` provider. Gemini model names are automatically mapped (e.g., `gemini-3-flash` to `gemini-3-flash-preview`).

## Deployment

### Systemd Service

```bash
sudo mise run install
sudo systemctl enable --now nenya
```

### Building from Source

```bash
go build -o nenya ./cmd/nenya
```

Or for a quick local test:

```bash
mise run run
```

## Development

```bash
go test ./...
go vet ./...
go fmt ./...
```

Architecture overview: [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md)

## Sponsor / Support this Project

- **GitHub Sponsors**: [https://github.com/sponsors/gumieri](https://github.com/sponsors/gumieri)
- **Pix (Brazil)**: [`rgumieri@gmail.com`](https://nubank.com.br/cobrar/2jm8a/69d54dab-4530-4e09-a531-e959e45fb6d8)

## License & Disclaimer

Apache 2.0 License. See [`LICENSE`](LICENSE) and [`docs/DISCLAIMER.md`](docs/DISCLAIMER.md).

---

[go-version]: https://img.shields.io/badge/Go-1.26-00ADD8?logo=golang&logoColor=white
[license]: https://img.shields.io/badge/License-Apache_2.0-5B44C2?logo=apache&logoColor=white
[zero-deps]: https://img.shields.io/badge/Dependencies-0-2EA043?logo=golang&logoColor=white
[ci]: https://img.shields.io/github/actions/workflow/status/gumieri/nenya/ci.yml?branch=main&logo=github&logoColor=white&label=CI
[codeql]: https://img.shields.io/github/actions/workflow/status/gumieri/nenya/codeql.yml?branch=main&logo=github&logoColor=white&label=CodeQL
[release]: https://img.shields.io/github/v/release/gumieri/nenya?logo=github&logoColor=white&sort=semver
[sponsor]: https://img.shields.io/badge/Sponsor-GitHub-EA4AAA?logo=githubsponsors&logoColor=white
