# Deploy Bare Metal (systemd)

This guide covers running Nenya as a systemd service on bare metal or VMs, with socket activation for seamless restarts and hot reload support.

## Install

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

## Configuration Directory

Create the config directory:

```bash
sudo mkdir -p /etc/nenya
```

Nenya supports two configuration modes:

**Directory mode (default):**
```
/etc/nenya/
├── config.json           # single config file
# OR
├── config.d/
│   ├── 01-server.json    # server, governance, bouncer, compaction
│   ├── 02-providers.json # provider overrides
│   ├── 03-agents.json   # agent definitions with fallback chains
│   └── 04-mcp.json      # MCP server integration per agent
```

**Single file mode:**
```bash
./nenya --config /path/to/config.json
```

**Environment variables:**
```bash
NENYA_CONFIG_DIR=/etc/nenya ./nenya           # directory mode
NENYA_CONFIG_FILE=/path/to/config.json ./nenya  # single file mode
```

### Multi-File Configuration (Directory Mode)

When using directory mode, `config.d/*.json` files are loaded in alphabetical order and deep-merged.

**Merge rules:**

| Field Type | Behavior |
|------------|----------|
| `agents` (map) | Per-key merge — later files add or override individual agents |
| `providers` (map) | Per-key merge — later files add or override individual providers |
| `mcp_servers` (map) | Per-key merge |
| `server`, `governance`, `bouncer`, etc. (struct) | Last file wins — if multiple files set the same field, the last one in alphabetical order takes precedence |

**Note:** `config.d/` and `config.json` are mutually exclusive — if `config.d/` exists and is non-empty, `config.json` is ignored.

### Example Config Files

`00-server.json`:
```json
{
  "server": {
    "listen_addr": ":8080"
  },
  "bouncer": {
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

## Secrets

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

See [`docs/SECRETS_FORMAT.md`](SECRETS_FORMAT.md) for full documentation on advanced options (api_keys, environment variables).

## Systemd Service

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

Install and enable the service:

```bash
sudo install -m 644 deploy/nenya.service /etc/systemd/system/
sudo install -m 644 deploy/nenya.socket /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now nenya.socket
```

### Socket Activation

The `nenya.socket` unit enables socket activation. When the service restarts, connections queue in the socket and the new process inherits the file descriptor — no dropped requests.

## Hot Reload

Reload configuration without downtime:

```bash
systemctl reload nenya
```

- Reloads config files from `/etc/nenya/` and re-reads secrets
- Re-discovers model catalogs from all configured providers
- Validates config structure (patterns, enums) but does not ping providers
- Preserves UsageTracker, Metrics, and ThoughtSignatureCache across reloads
- On validation failure: logs error, continues serving with old config
- In-flight requests complete with the gateway they started with

## Verify Installation

Check service status:

```bash
systemctl status nenya
```

Test health endpoint:

```bash
curl http://localhost:8080/healthz
```

Test with a chat request:

```bash
export CLIENT_TOKEN=$(jq -r '.client_token' /etc/nenya/secrets.json)

curl -H "Authorization: Bearer $CLIENT_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gemini-2.5-flash",
    "messages": [{"role": "user", "content": "Hello!"}]
  }' \
  http://localhost:8080/v1/chat/completions
```

## Logging

View logs:

```bash
journalctl -u nenya -f
```

Enable debug logging by setting `server.log_level` in config or using the `-verbose` flag in the systemd unit:

```ini
[Service]
ExecStart=/usr/local/bin/nenya -verbose
```

## Troubleshooting

### Service fails to start

Check the journal for errors:

```bash
journalctl -u nenya -n 50 --no-pager
```

Common issues:
- **`ErrMLockFailure`**: `LimitMEMLOCK=infinity` is missing in the systemd unit
- **Config validation error**: Check JSON syntax and field names in `/etc/nenya/config.d/`
- **Port already in use**: Another service is using port 8080, change `server.listen_addr`

### Hot reload not working

Ensure the service is running (not stopped):

```bash
systemctl is-active nenya
```

Check the journal for reload errors:

```bash
journalctl -u nenya -n 100 | grep -i reload
```

### Model discovery failing

Enable debug logging to see detailed discovery output:

```bash
journalctl -u nenya -f | grep discovery
```

Check that provider API keys are correctly configured in `secrets.json`.
