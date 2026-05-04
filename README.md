<img alt="nenya" src="https://github.com/user-attachments/assets/bd518ded-2b65-42f9-866e-5a670cf9dbb1" />

# Nenya AI Gateway

![go-version] ![License][license] ![zero-deps] ![CI][ci] ![CodeQL][codeql] ![Release][release] ![Sponsor][sponsor]

A lightweight, zero-dependency AI API Gateway written in Go. Nenya sits between your AI coding clients and upstream LLM providers, adding secret redaction, context management, agent routing, and MCP tool integration — all with transparent SSE streaming. Security-hardened: non-root execution, mlock for secrets, seccomp + no-new-privileges.

**Compatible with any provider that implements the OpenAI Or Anthropic Chat Completions API.** For 22 providers we ship built-in adapters with specialized handling.

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
- **22 built-in providers** with specialized adapters for wire format differences
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
- **Input validation** — strict body limits, JSON sanitization, header filtering
- **Graceful degradation** — never blocks requests due to engine or pipeline failures

### Hardening (Deployment Security)

- **Secure memory (default)**: All tokens stored in mlock-protected RAM, sealed read-only after init, core dumps disabled
- **Non-root execution** — runs as UID 65532 with dropped capabilities
- **Memory protection** — `LimitMEMLOCK=infinity` and `LimitCORE=0` in systemd
- **Read-only filesystem** — immutable root + private `/tmp`
- **Seccomp + no-new-privileges** — restricted syscalls, prevents privilege escalation
- **Zero-trust secrets** — loaded via systemd credentials or container mounts, never to disk
- **Socket activation** — seamless restarts with zero dropped connections

### Reliability

- **Zero external dependencies** — Go standard library only
- **Hot reload** — `systemctl reload nenya` for zero-downtime config changes
- **Circuit breaker** — per agent+provider+model with automatic failover and backoff
- **Rate limiting** — per upstream host (RPM/TPM)
- **Response cache** — in-memory LRU with SHA-256 fingerprinting

### MCP Tool Integration

- **Tool discovery** — connect to MCP servers for automatic tool injection
- **Multi-turn execution** — intercept tool calls, execute against MCP servers, forward results
- **Auto-search** — pre-fetch relevant context from MCP servers before forwarding
- **Auto-save** — persist assistant responses to MCP memory servers

## Quick Start

### Run with Podman

Create minimal config and secrets:

```bash
mkdir -p config secrets
cat > config/config.json << 'EOF'
{
  "server": { "listen_addr": ":8080" },
  "agents": {
    "default": {
      "strategy": "fallback",
      "models": ["gemini-2.5-flash"]
    }
  }
}
EOF

cat > secrets/provider_keys.json << 'EOF'
{
  "provider_keys": {
    "gemini": "AIza..."
  }
}
EOF

cat > secrets/client.json << 'EOF'
{
  "client_token": "nk-$(openssl rand -hex 32)"
}
EOF
```

Run the container:

```bash
podman run -d \
  --name nenya \
  -p 8080:8080 \
  -v ./config:/etc/nenya:ro \
  -v ./secrets:/run/secrets/nenya:ro \
  -e NENYA_SECRETS_DIR=/run/secrets/nenya \
  --cap-drop=ALL \
  --cap-add=IPC_LOCK \
  --security-opt=no-new-privileges:true \
  --read-only \
  --tmpfs /tmp:rw,noexec,nosuid,size=64M \
  ghcr.io/gumieri/nenya:latest
```

Test it:

```bash
curl -H "Authorization: Bearer $(jq -r '.client_token' secrets/client.json)" \
  http://localhost:8080/healthz
```

### Or Install via Package Manager

Nenya provides native packages for major Linux distributions and community package managers:

| Distribution | Command |
|-------------|---------|
| **Debian/Ubuntu (.deb)** | Download `nenya_<version>_linux_amd64.deb` from the release page and run `sudo dpkg -i` |
| **Fedora/RHEL (.rpm)** | Download `nenya-<version>.x86_64.rpm` from the release page and run `sudo rpm -i` |
| **Arch Linux (.pkg.tar.zst)** | Download `nenya-<version>-x86_64.pkg.tar.zst` from the release page and run `sudo pacman -U` |
| **Arch Linux (AUR)** | `yay -S nenya-bin` (or your preferred AUR helper) |
| **Nix/NixOS** | Add `gumieri/nur-packages` to your NUR registry and use `nenya` |

All packages install the binary to `/usr/bin/nenya` and include systemd service and socket units. After install, enable and start:

```bash
sudo systemctl enable --now nenya.socket
sudo systemctl enable --now nenya.service
```

### Or Choose Your Deployment

- **[Deploy Bare Metal (systemd)](docs/DEPLOY_BAREMETAL.md)** — Direct binary install, socket activation, hot reload
- **[Deploy Container (Podman/Docker Compose)](docs/DEPLOY_CONTAINER.md)** — compose.yml, image verification, security hardening
- **[Deploy Kubernetes (Helm)](docs/DEPLOY_KUBERNETES.md)** — Helm chart, ConfigMap/Secret, ingress setup

## API Endpoints

All `/v1/*` endpoints require `Authorization: Bearer <client_token>`.

| Endpoint | Auth | Description |
|----------|------|-------------|
| `POST /v1/chat/completions` | Bearer | OpenAI-compatible chat with SSE streaming, agent fallback, MCP multi-turn |
| `GET /v1/models` | Bearer | Live model catalog from discovered providers + static registry (context window, max tokens) |
| `POST /v1/embeddings` | Bearer | Passthrough proxy |
| `POST /v1/responses` | Bearer | Passthrough proxy |
| `POST /v1/images/generations` | Bearer | Image generation (OpenAI-compatible) |
| `POST /v1/audio/transcriptions` | Bearer | Audio transcription (Whisper-compatible, multipart support) |
| `POST /v1/audio/speech` | Bearer | Text-to-speech synthesis (OpenAI-compatible) |
| `POST /v1/moderations` | Bearer | Content moderation (OpenAI-compatible) |
| `POST /v1/rerank` | Bearer | Re-ranking API (Cohere/Jina/Voyage-compatible) |
| `POST /v1/a2a` | Bearer | Agent-to-Agent protocol (Google A2A) |
| `GET /v1/files` | Bearer | File listing, upload, retrieval, deletion |
| `POST /v1/batches` | Bearer | Batch API operations |
| `POST /proxy/{provider}/*` | Bearer | Arbitrary provider endpoint passthrough (all HTTP methods, SSE streaming) |
| `GET /healthz` | None | Engine health probe |
| `GET /statsz` | None | Token usage, circuit breaker state, MCP server status |
| `GET /metrics` | None | Prometheus-compatible metrics |

See [`docs/PASSTHROUGH_PROXY.md`](docs/PASSTHROUGH_PROXY.md) for detailed passthrough proxy usage.

## Documentation

| Document | Description |
|----------|-------------|
| [Providers](docs/PROVIDERS.md) | All 22 providers, capabilities matrix, special behaviors, adding custom providers |
| [Configuration](docs/CONFIGURATION.md) | Full config reference, directory mode, all sections and fields |
| [Deploy Bare Metal](docs/DEPLOY_BAREMETAL.md) | Systemd unit, config.d layout, secrets, hot reload |
| [Deploy Container](docs/DEPLOY_CONTAINER.md) | Podman/Docker Compose, image verification, security notes |
| [Deploy Kubernetes](docs/DEPLOY_KUBERNETES.md) | Helm chart usage, ConfigMap/Secret, ingress setup |
| [Passthrough Proxy](docs/PASSTHROUGH_PROXY.md) | Raw provider endpoint proxying, SSE streaming, auth injection |
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
