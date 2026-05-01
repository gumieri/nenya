<img alt="nenya" src="https://github.com/user-attachments/assets/bd518ded-2b65-42f9-866e-5a670cf9dbb1" />

# Nenya AI Gateway

![go-version] ![License][license] ![zero-deps] ![CI][ci] ![CodeQL][codeql] ![Release][release] ![Sponsor][sponsor]

A lightweight, zero-dependency AI API Gateway written in Go. Nenya sits between your AI coding clients and upstream LLM providers, adding secret redaction, context management, agent routing, and MCP tool integration — all with transparent SSE streaming.

**Compatible with any provider that implements the OpenAI Chat Completions API.** For 22 providers we ship built-in adapters with specialized handling.

## How Nenya handles the requests

```text
+----------------------------------------------+
| Client (Cursor / OpenCode / Aider / etc.)    |
| OpenAI-compatible request                    |
| POST /v1/chat/completions + Bearer token     |
+----------------------------------------------+
                       |
                       v
+----------------------------------------------+
| Nenya Gateway                                |
| - auth check                                 |
| - parse JSON + extract model                 |
| - resolve agent/provider                     |
| - optional cache (HIT => replay SSE)         |
| - optional MCP context/tool injection        |
+----------------------------------------------+
                       |
                       v
+----------------------------------------------+
| Privacy / Context Pipeline (best-effort)     |
| - Tier-0 regex + entropy secret redaction    |
| - compaction / pruning / window mgmt         |
| - engine summarize (usually local Ollama)    |
+----------------------------------------------+
                       |
                       v
+----------------------------------------------+
| Routing                                      |
|  A) Standard forwarding                      |
|     - fallback chain + circuit breaker + RL  |
|  B) MCP multi-turn tool loop (if enabled)    |
|     - buffer SSE, execute MCP tools, re-send |
+----------------------------------------------+
                       |
                       v
+----------------------------------------------+
| Upstream LLM Providers                       |
| Anthropic | Gemini | DeepSeek | Mistral | ...|
+----------------------------------------------+
                       |
                       |  SSE stream
                       v
+----------------------------------------------+
| Nenya SSE Pipeline                           |
| - adapter response transforms                |
| - usage accounting + stream filter           |
| - flush + (optional) cache capture           |
| - (optional) MCP auto-save                   |
+----------------------------------------------+
                       |
                       v
+----------------------------------------------+
| Client receives transparent SSE output       |
+----------------------------------------------+
```

Flow notes:
- `/v1/*` endpoints require client bearer auth; `/healthz`, `/statsz`, `/metrics` do not.
- Pipeline failures degrade gracefully and forward the request instead of returning a 500.
- MCP-enabled agents can run local/remote tools without exposing MCP complexity to the client.

## Features

### Routing & Agents

- **Config-driven provider registry** — add providers via JSON, zero code changes
- **23 built-in providers** with specialized adapters for wire format differences
- **Dynamic model discovery** — fetches live model catalogs from providers at startup and on reload
- **Model registry** — reference models by string shorthand with automatic provider/context resolution
- **Multi-provider model resolution** — when a model exists in multiple providers, all are added to the agent's fallback chain
- **Three-tier model resolution** — config overrides > discovered models > static registry
- **Per-model wire format** — models from multi-format gateways (OpenCode Zen) auto-convert between OpenAI, Anthropic, and Gemini wire formats based on the model's `format` attribute
- **Agent fallback chains** — round-robin or sequential with circuit breaker and automatic failover
- **Latency-aware routing** — auto-reorder targets by historical median response time with ±5% jitter to prevent thundering herd
- **Per-agent system prompts** — inline or file-based

### Security & Privacy

- **Tier-0 regex secret filter** — always-on redaction of AWS keys, GitHub tokens, passwords, etc.
- **3-Tier content pipeline** — pass-through, engine summarization, or TF-IDF relevance-scored truncation
- **Context window compaction** — sliding window summarization with configurable engine
- **Stale tool call pruning** — compact old assistant+tool response pairs to save tokens
- **Thought pruning** — strip reasoning blocks from assistant message history

### Reliability

- **Zero external dependencies** — Go standard library only
- **Hot reload** — `systemctl reload nenya` for zero-downtime config changes
- **Seamless restarts** — `systemctl enable nenya.socket` enables socket activation; when the service restarts, connections queue in the socket and the new process inherits the file descriptor — no dropped requests
- **Circuit breaker** — per agent+provider+model with automatic failover and backoff
- **Rate limiting** — per upstream host (RPM/TPM)
- **Response cache** — in-memory LRU with SHA-256 fingerprinting
- **Graceful degradation** — works without Ollama; never returns 500 for pipeline failures

### MCP Tool Integration

- **Tool discovery** — connect to MCP servers for automatic tool injection
- **Multi-turn execution** — intercept tool calls, execute against MCP servers, forward results
- **Auto-search** — pre-fetch relevant context from MCP servers before forwarding
- **Auto-save** — persist assistant responses to MCP memory servers

## Supported Providers

### Full Adapters (custom wire format handling)

| Provider | Auth | Special Behavior |
|----------|------|-----------------|
| **Anthropic** | `x-api-key` | Full bidirectional OpenAI↔Anthropic format conversion |
| **Gemini** | `bearer+x-goog` | Thought signature preservation, orphaned tool_call cleanup, model aliasing |
| **z.ai** (Zhipu) | `bearer` | Orphaned tool message removal, user message merging, auto-thinking for reasoning models, model-specific temperature defaults, Zhipu error code classification |
| **Ollama** | `none` | Local-first, optional auth, conservative error classification |

### OpenAI-Compatible with Adjustments

| Provider | Auth | Notes |
|----------|------|-------|
| **OpenRouter** | `bearer` | Adds `HTTP-Referer` and `X-Title` headers |
| **Azure OpenAI** | `api-key` | Uses `api-key` header instead of `Authorization: Bearer` |
| **Perplexity** | `bearer` | Does not support function calling |
| **Cohere** | `bearer` | Content arrays flattened |
| **DeepInfra** | `bearer` | Standard capabilities |

### Drop-in OpenAI-Compatible

| Provider | Auth | Notes |
|----------|------|-------|
| **OpenCode Zen** | `bearer` | Multi-format gateway — Claude models auto-convert to Anthropic wire format |
| **DeepSeek** | `bearer` | Thinking mode default, reasoning_content injection, parameter stripping in thinking mode |
| **Mistral** | `bearer` |
| **xAI** | `bearer` |
| **Groq** | `bearer` |
| **Together** | `bearer` |
| **SambaNova** | `bearer` |
| **Cerebras** | `bearer` |
| **NVIDIA** | `bearer` |
| **GitHub** | `bearer` |

> **Any** OpenAI-compatible provider can be added via JSON config — no code changes required. See [`docs/PROVIDERS.md`](docs/PROVIDERS.md) for the full provider reference.

## Quick Start

### 1. Install

```bash
curl -fsSL https://raw.githubusercontent.com/gumieri/nenya/main/install.sh | sudo sh
```

This detects your OS and architecture, downloads the correct binary from GitHub Releases, verifies the checksum, and installs the binary, example config, and systemd unit.

Pinned version:
```bash
curl -fsSL https://raw.githubusercontent.com/gumieri/nenya/main/install.sh | sudo sh -s -- -v 0.1.0
```

Dry run (audit before installing):
```bash
curl -fsSL https://raw.githubusercontent.com/gumieri/nenya/main/install.sh | sh -s -- --dry-run
```

### 2. Create config directory

```bash
sudo mkdir -p /etc/nenya
```

### 3. Split configuration across files

Nenya loads all `*.json` files from `/etc/nenya/` (excluding `secrets.json`), sorted alphabetically, and deep-merges them. Map fields (`agents`, `providers`, `mcp_servers`) merge per-key; struct fields use last-file-wins.

```
/etc/nenya/
├── 00-server.json          # server, governance, security_filter, compaction
├── 10-providers.json       # provider overrides
├── 20-agents.json          # agent definitions with fallback chains
├── 30-agents-mcp.json      # MCP server integration per agent
└── secrets.json            # excluded (loaded via systemd credential)
```

`00-server.json`:
```json
{
  "server": {
    "listen_addr": ":8080"
  },
  "security_filter": {
    "enabled": true,
    "engine": {
      "provider": "ollama",
      "model": "qwen2.5-coder:7b"
    }
  }
}
```

`20-agents.json`:
```json
{
  "agents": {
    "plan": {
      "strategy": "fallback",
      "models": ["deepseek-reasoner"]
    },
    "build": {
      "strategy": "fallback",
      "models": ["gemini-2.5-flash"]
    }
  }
}
```

### 4. Create secrets

Create a JSON file with your secrets:

```bash
sudo mkdir -p /etc/nenya
sudo tee /etc/nenya/secrets.json << 'EOF'
{
  "client_token": "nk-$(openssl rand -hex 32)",
  "provider_keys": {
    "gemini": "AIza...",
    "deepseek": "sk-..."
  }
}
EOF

sudo chmod 600 /etc/nenya/secrets.json
```

**Alternative:** Use a directory with multiple files (auto-merged):

```bash
sudo mkdir -p /etc/nenya/secrets.d

sudo tee /etc/nenya/secrets.d/01-client.json << 'EOF'
{"client_token": "nk-$(openssl rand -hex 32)"}
EOF

sudo tee /etc/nenya/secrets.d/02-providers.json << 'EOF'
{"provider_keys": {"gemini": "AIza...", "deepseek": "sk-..."}}
EOF

sudo chmod 600 /etc/nenya/secrets.d/*.json
```

See [`docs/SECRETS_FORMAT.md`](docs/SECRETS_FORMAT.md) for full documentation on Docker/K8s deployment and advanced options (api_keys, NENYA_SECRETS_DIR).

### 5. Configure systemd

A hardened systemd unit file is provided in `deploy/nenya.service`. Key security features:

```ini
[Service]
LimitMEMLOCK=infinity  # Required for secure memory (mlock)
NoNewPrivileges=yes    # Prevent privilege escalation
ProtectSystem=strict   # Read-only filesystem
ProtectHome=yes        # No home directory access
PrivateTmp=yes         # Isolated /tmp

ExecStart=/usr/local/bin/nenya
ExecReload=/bin/kill -HUP $MAINPID
LoadCredential=secrets:/etc/nenya/secrets.json
```

**Note:** `LimitMEMLOCK=infinity` is required for secure memory storage. Without it, Nenya will fail to start with `ErrMLockFailure`.

```bash
sudo install -m 644 deploy/nenya.service /etc/systemd/system/
sudo install -m 644 deploy/nenya.socket /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now nenya.socket
```

## API Endpoints

All `/v1/*` endpoints require `Authorization: Bearer <client_token>`.

| Endpoint | Auth | Description |
|----------|------|-------------|
| `POST /v1/chat/completions` | Bearer | OpenAI-compatible chat with SSE streaming, agent fallback, MCP multi-turn |
| `GET /v1/models` | Bearer | Live model catalog from discovered providers + static registry (context window, max tokens) |
| `POST /v1/embeddings` | Bearer | Passthrough proxy |
| `POST /v1/responses` | Bearer | Passthrough proxy |
| `POST /proxy/{provider}/*` | Bearer | Arbitrary provider endpoint passthrough (all HTTP methods, SSE streaming) |
| `GET /healthz` | None | Engine health probe |
| `GET /statsz` | None | Token usage, circuit breaker state, MCP server status |
| `GET /metrics` | None | Prometheus-compatible metrics |

### Passthrough Proxy

The `/proxy/{provider}/*` endpoint enables raw proxying to any provider endpoint:

```bash
# Get Anthropic models
curl -H "Authorization: Bearer $CLIENT_TOKEN" \
  http://localhost:8080/proxy/anthropic/v1/models

# Upload file to OpenAI
curl -X POST -H "Authorization: Bearer $CLIENT_TOKEN" \
  -F "file=@document.pdf" -F "purpose=fine-tune" \
  http://localhost:8080/proxy/openai/v1/files

# Any provider endpoint
curl -H "Authorization: Bearer $CLIENT_TOKEN" \
  http://localhost:8080/proxy/{provider}/{path}
```

**Features:**
- All HTTP methods: GET, POST, PUT, PATCH, DELETE, HEAD, OPTIONS
- Automatic auth injection (provider-specific API keys)
- Header sanitization (strips hop-by-hop, auth, host headers)
- SSE streaming auto-detect (`text/event-stream` → pipe as-is)
- Rate limiting, usage tracking, and metrics
- Bypasses content pipeline, circuit breaker, and retry (raw proxy)

## Hot Reload

Send `SIGHUP` to reload configuration without downtime:

```bash
systemctl reload nenya
```

- Reloads config files from `/etc/nenya/` and re-reads secrets
- Re-discovers model catalogs from all configured providers
- Validates config structure (patterns, enums) but does not ping providers
- Preserves UsageTracker, Metrics, and ThoughtSignatureCache across reloads
- On validation failure: logs error, continues serving with old config
- In-flight requests complete with the gateway they started with

## Documentation

| Document | Description |
|----------|-------------|
| [Providers](docs/PROVIDERS.md) | All 22 providers, capabilities matrix, special behaviors, adding custom providers |
| [Configuration](docs/CONFIGURATION.md) | Full config reference, directory mode, all sections and fields |
| [Architecture](docs/ARCHITECTURE.md) | Package DAG, request lifecycle, circuit breaker, SSE pipeline |
| [MCP Integration](docs/MCP_INTEGRATION.md) | MCP server integration, tool discovery, multi-turn execution |
| [Adapters](docs/ADAPTERS.md) | Adapter system internals, auth styles, capability flags |
| [Secrets Format](docs/SECRETS_FORMAT.md) | Systemd credentials, env var fallback, container/K8s deployment |
| [Security](docs/SECURITY.md) | Vulnerability reporting policy |

## License

Apache 2.0. See [`LICENSE`](LICENSE).

---

[go-version]: https://img.shields.io/badge/Go-1.26-00ADD8?logo=golang&logoColor=white
[license]: https://img.shields.io/badge/License-Apache_2.0-5B44C2?logo=apache&logoColor=white
[zero-deps]: https://img.shields.io/badge/Dependencies-0-2EA043?logo=golang&logoColor=white
[ci]: https://img.shields.io/github/actions/workflow/status/gumieri/nenya/ci.yml?branch=main&logo=github&logoColor=white&label=CI
[codeql]: https://img.shields.io/github/actions/workflow/status/gumieri/nenya/codeql.yml?branch=main&logo=github&logoColor=white&label=CodeQL
[release]: https://img.shields.io/github/v/release/gumieri/nenya?logo=github&logoColor=white&sort=semver
[sponsor]: https://img.shields.io/badge/Sponsor-GitHub-EA4AAA?logo=githubsponsors&logoColor=white
