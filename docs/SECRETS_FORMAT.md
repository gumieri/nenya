# Secrets Format for Nenya Gateway

Nenya loads secrets from JSON files. Multiple secret sources are supported with priority ordering, and directories containing multiple JSON files are merged alphabetically (same behavior as config directory).

## Secret Paths (Priority Order)

| Priority | Path | Description |
|----------|------|-------------|
| 1 | `$CREDENTIALS_DIRECTORY/secrets` | systemd single file (highest priority) |
| 2 | `$CREDENTIALS_DIRECTORY/secrets.d/*.json` | systemd directory with merged JSON files |
| 3 | `$NENYA_SECRETS_DIR/*.json` | configurable path (env var) |
| 4 | `/run/secrets/nenya/*.json` | K8s/Docker standard path (fallback) |
| 5 | `<config-root>/secrets.json` | single file, directory mode only — the file the shipped unit wires via `LoadCredential` and `nenya secret set` writes by default (CONTRACT.md §6.1 source 5) |

The first path that exists is used. If multiple JSON files exist in a directory, they are merged alphabetically (last-wins for duplicate keys), except `api_keys`: a later `enabled:false` entry does not override an earlier enabled one (CONTRACT.md §6.2). Sources 3 and 4 are the same probe: `NENYA_SECRETS_DIR` replaces the `/run/secrets/nenya` default rather than adding a second directory. `NENYA_SECRETS_DIR` may name a single JSON file instead of a directory. A `config.json` found inside a secrets directory is skipped (it is configuration, not secrets).

## JSON Format

All JSON files support the same structure. Fields can be split across multiple files:

```json
{
  "client_token": "nk-$(openssl rand -hex 32)",
  "provider_keys": {
    "gemini": "AIza<your-real-key>",
    "deepseek": "sk-<your-real-key>",
    "openai": "sk-proj-<your-real-key>"
  },
  "api_keys": {
    "dev-user": {
      "name": "dev-user",
      "token": "nk-$(openssl rand -hex 32)",
      "roles": ["user"],
      "allowed_agents": ["build", "plan"],
      "enabled": true
    }
  }
}
```

### Fields

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `client_token` | string | Yes | Admin bearer token for `/v1/*` endpoints |
| `provider_keys` | object | No | Map of provider name → API key |
| `api_keys` | object | No | Map of key ID → `ApiKey` config (RBAC) |

## Deployment with systemd

### Single file

```bash
# Create secrets file — the token is generated outside the heredoc because
# quoted heredocs do not expand $(...); pasting the command literally would
# ship a guessable token (and is refused at boot).
CLIENT_TOKEN="nk-$(openssl rand -hex 32)"
cat > /etc/nenya/secrets.json << EOF
{
  "client_token": "$CLIENT_TOKEN",
  "provider_keys": {
    "deepseek": "sk-<your-real-key>",
    "gemini": "AIza<your-real-key>"
  }
}
EOF

# Configure systemd
cat > /etc/systemd/system/nenya.service << 'EOF'
[Service]
LoadCredential=secrets:/etc/nenya/secrets.json
EOF
```

The gateway reads from `$CREDENTIALS_DIRECTORY/secrets`.

### Directory with merged files

```bash
# Create secrets directory
mkdir -p /etc/nenya/secrets.d

# Split into multiple files — the token is generated outside the quoted
# heredoc (quoted heredocs do not expand $(...); pasting the command literally
# would be refused at boot).
CLIENT_TOKEN="nk-$(openssl rand -hex 32)"
cat > /etc/nenya/secrets.d/01-client.json << EOF
{"client_token": "$CLIENT_TOKEN"}
EOF

cat > /etc/nenya/secrets.d/02-providers.json << 'EOF'
{"provider_keys": {"deepseek": "sk-<your-real-key>", "gemini": "AIza<your-real-key>"}}
EOF

cat > /etc/nenya/secrets.d/03-api-keys.json << 'EOF'
{"api_keys": {...}}
EOF

# No systemd change needed — gateway auto-detects secrets.d/*.json
```

## Container / Kubernetes Deployment

### Docker Compose

Mount secrets as files. Use `NENYA_SECRETS_DIR` to point to the mount point:

```yaml
services:
  nenya:
    image: ghcr.io/gumieri/nenya:latest
    ports:
      - "8080:8080"
    volumes:
      - ./config:/etc/nenya:ro
      - ./secrets:/run/secrets/nenya:ro
    environment:
      NENYA_SECRETS_DIR: /run/secrets/nenya
    restart: unless-stopped
```

Example secrets directory:

```bash
secrets/
├── 01-client.json     → {"client_token": "nk-$(openssl rand -hex 32)"}
├── 02-providers.json  → {"provider_keys": {"deepseek": "sk-<your-real-key>"}}
└── 03-keys.json       → {"api_keys": {...}}
```

### Kubernetes

Mount a Secret as files. Use `NENYA_SECRETS_DIR` env var:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: nenya-secrets
type: Opaque
stringData:
  01-client.json: |
    {"client_token": "nk-<generate with: openssl rand -hex 32, prefixed nk->"}
  02-providers.json: |
    {"provider_keys": {"deepseek": "sk-<your-real-key>", "gemini": "AIza<your-real-key>"}}
  03-keys.json: |
    {"api_keys": {"dev-user": {...}}}
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: nenya
spec:
  replicas: 1
  template:
    spec:
      containers:
        - name: nenya
          image: ghcr.io/gumieri/nenya:latest
          env:
            - name: NENYA_SECRETS_DIR
              value: "/run/secrets/nenya"
          volumeMounts:
            - name: config
              mountPath: /etc/nenya
              readOnly: true
            - name: secrets
              mountPath: /run/secrets/nenya
              readOnly: true
          livenessProbe:
            httpGet:
              path: /healthz
              port: 8080
          readinessProbe:
            httpGet:
              path: /healthz
              port: 8080
      volumes:
        - name: config
          configMap:
            name: nenya-config
        - name: secrets
          secret:
            secretName: nenya-secrets
```

**Note:** `NENYA_SECRETS_DIR` is optional. If unset, the gateway falls back to `/run/secrets/nenya`.

## Generating Tokens

### Client token (for `/v1/*` auth)

```bash
openssl rand -hex 32
# Output: nk-abc123def456... (prefix + 64 hex chars)
```

### API keys (for client RBAC)

Use the provided utility or manually create `ApiKey` entries in JSON:

```bash
# Generate a strong token value (nk- + 32 bytes hex), then add the api_keys
# entry below with that token
openssl rand -hex 32
```

**Manual configuration:**
```json
{
  "api_keys": {
    "dev-user": {
      "name": "dev-user",
      "token": "nk-$(openssl rand -hex 32)",
      "roles": ["user"],
      "allowed_agents": ["build", "plan"],
      "allowed_endpoints": ["GET /v1/models", "POST /v1/chat/completions"],
      "enabled": true
    }
  }
}
```

**ApiKey Field Reference:**

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | string | Yes | Human-readable key identifier (for logging, metrics, and per-key rate limits/budgets). Must be non-empty, unique among enabled keys, and must not be `primary` (the synthetic primary-token identity). |
| `token` | string | Yes | API key token (minimum 16 characters, prefix `nk-` recommended). Must be unique among enabled keys and must not equal the primary `client_token` (a collision would make the authenticated identity ambiguous). |
| `roles` | []string | Yes | List of roles (one or more): `"admin"`, `"user"`, `"read-only"` |
| `allowed_tools` | []string | No | MCP tools this key may invoke (empty = every tool the agent's MCP configuration exposes). Entries match the namespaced tool name exactly (`server__tool`), a server wildcard (`server__*`), or the bare tool name on any server (`tool`). Enforced at injection and at dispatch, including the auto-search/auto-save dispatches; a namespaced entry must name a configured `mcp_servers` key (NENYA-183). A bare entry is not agent-scoped: it matches that tool name on every server the key's agents may reach, so prefer the namespaced form when servers share tool names. An agent whose MCP server is unusable for a credential reason fails the request closed with `503 error_kind=mcp_unavailable` unless the key's grant cannot reach that server (a key granted only other servers is unaffected by it). |
| `allowed_agents` | []string | No | Agent names this key can access (empty = all agents). Validated against `agents` at boot/`-validate`/reload (admin keys exempt; skipped when discovery runs with `auto_agents`). On `/v1/responses` the list is matched against the request's `model`, so a key scoped to agent names is denied a plain upstream model id. |
| `allowed_endpoints` | []string | No | HTTP method + path allowlist (empty = role-based defaults) |
| `created_at` | string | No | ISO 8601 timestamp for audit trail |
| `expires_at` | string | No | ISO 8601 timestamp for key expiration |
| `enabled` | bool | No | Enable/disable key without deletion |
| `cache_salt` | string | No | Tenant isolation salt injected as `cache_salt` for vLLM/OpenAI-compatible providers. Overrides the agent-level `cache_salt`. Max 64 characters (validation applied). |
**Roles:**

| Role | Permissions | Agent Access | Endpoint Access |
|-------|-------------|--------------|----------------|
| `admin` | Full access (bypasses RBAC) | All agents | All endpoints |
| `user` | Chat, models, embed | Scoped by `allowed_agents` (or all if empty) | All non-admin endpoints |
| `read-only` | Models, metrics only | Scoped by `allowed_agents` (or all if empty) | GET requests only |

**Examples:**

**Read-only key for model catalog:**
```json
{
  "api_keys": {
    "model-reader": {
      "name": "Model Catalog Reader",
      "token": "nk-abc123...",
      "roles": ["read-only"],
      "allowed_agents": [],
      "enabled": true
    }
  }
}
```

**User key scoped to specific agents with endpoint allowlist:**
```json
{
  "api_keys": {
    "build-agent-user": {
      "name": "Build Agent User",
      "token": "nk-def456...",
      "roles": ["user"],
      "allowed_agents": ["build", "test"],
      "allowed_endpoints": ["GET /v1/models", "POST /v1/chat/completions", "POST /v1/embeddings"],
      "enabled": true
    }
  }
}
```

**Admin key with expiration:**
```json
{
  "api_keys": {
    "ops-admin": {
      "name": "Operations Admin",
      "token": "nk-ghi789...",
      "roles": ["admin"],
      "expires_at": "2026-12-31T23:59:59Z",
      "enabled": true
    }
  }
}
```

Or manually create an `ApiKey` entry in JSON:

```json
{
  "api_keys": {
    "dev-user": {
      "name": "dev-user",
      "token": "nk-$(openssl rand -hex 32)",
      "roles": ["user"],
      "allowed_agents": ["build", "plan"],
      "enabled": true
    }
  }
}
```

## Provider Keys

The `provider_keys` object maps provider names to their API keys. See [`PROVIDERS.md`](PROVIDERS.md) for the full list of built-in providers. Abbreviated doc forms (`sk-…`, `AIza…`) illustrate structure only — paste real keys before booting; placeholder credentials are refused at startup (see "Weak-Credential Refusal" below).

**Legacy single-key format:**

```json
{
  "provider_keys": {
    "openai": "sk-proj-..."
  }
}
```

**Multi-account format (recommended for high-volume providers):**

For providers with multiple API keys or credentials, use the `accounts` field in the provider config instead:

```json
{
  "providers": {
    "openai": {
      "url": "https://api.openai.com/v1/chat/completions",
      "accounts": [
        {
          "id": "account-1",
          "type": "apikey",
          "credential": "sk-proj-xxxxx"
        },
        {
          "id": "account-2",
          "type": "apikey",
          "credential": "sk-proj-yyyyy"
        }
      ]
    },
    "google": {
      "url": "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions",
      "accounts": [
        {
          "id": "oauth-dev",
          "type": "oauth",
          "credential": "ya29.a0..."
        }
      ]
    }
  }
}
```

### Multi-Account Behavior

- **LRU Selection**: Credentials are selected using least-recently-used strategy with mutex-guarded access
- **Error Classification**: 6 error classes (auth, rate_limit, quota, capacity, server, unknown) with semantic cooldowns
- **Exponential Backoff**: Failed accounts receive exponentially increasing backoff (±10% jitter) up to level 15
- **Model Locks**: Individual model+provider combinations are locked during cooldown
- **Account State Persistence**: State is persisted in `<provider>.accounts.json` files in the config directory

At least one provider key must be present for the corresponding provider to work. If `accounts` is configured, it takes precedence over `provider_keys` for that provider.

## Security Notes

- Never commit secrets to version control
- Secrets files should be readable only by the service user (`chmod 600`)
- Use systemd's credential mechanism for secure in-memory storage
- Rotate tokens periodically
- Path traversal (`..`) is rejected for all secret paths
- Symlinks are resolved: a relative `file:` reference (directory mode) whose real path escapes the config root is rejected, even when the link itself lives inside it (NENYA-297); a config root behind a symlink still works

### Weak-Credential Refusal (Fail-Closed Boot)

At startup Nenya refuses to serve traffic when a credential from the secrets
document fails its strength class. The primary `client_token` and enabled
`api_keys` entry tokens get the full rule set: empty, publicly known
placeholders (`changeme`, `test-client-token`, `your-api-key-here`; exact or
as an unambiguous phrase), unexpanded templates or shell variables
(`<your-token>`, `$CLIENT_TOKEN`, `$(openssl ...)` pasted literally), trivial
word+digit values (`password123`), repeated single characters, or shorter
than 16 characters. Provider keys are checked for the placeholder, template,
and repeated-character classes only — no length floor, because local and
proxy upstreams legitimately use short shared secrets, and an empty value
means "no credential for this upstream" (as does an empty legacy
`api_key`/`credential` in the config file). The same placeholder/template/
repeated-character classes apply to credentials carried by the config file
itself (`providers.<name>.api_key`, `providers.<name>.accounts[].credential`).

The refusal error names the credential, the secrets source (a single file, or
the merged `secrets.d/` directory), and the remediation:

```
refusing to start: client_token is a publicly known placeholder value (source:
/etc/nenya/secrets.json); generate a strong value with `nenya secret set
--client-token` (or set NENYA_ALLOW_WEAK_SECRETS=1 to override, dev only)
```

(Abridged: provider keys suggest `nenya secret set --provider <name>`,
api_keys tokens are fixed by editing the token in the secrets document.)

`NENYA_ALLOW_WEAK_SECRETS=1` waives the strength pass — structural validation
(a required non-empty `client_token` and well-formed `api_keys` entries)
still applies, and a warning is printed once per process while it is set. It
exists for throwaway local experiments and the demo; never set it in
production. `nenya -validate` surfaces config-file credential refusals
without starting the gateway. Scope note (NENYA-183): credentials in `mcp_servers.<name>.headers` are plain
config strings and are **not** covered by the secrets strength checks or by
secure memory — there is no MCP section in secrets documents. Nenya mitigates
the consequences rather than moving the storage: header values are redacted
from `nenya describe --json` (as is any URL userinfo in a provider `url`, a
`format_urls` entry, or an MCP server `url` — NENYA-295), `nenya config set`
preserves a config file's
existing permissions (defaulting a new file to `0644`; `chmod 600` it if it
carries a token), a server may declare `required_headers` so a missing
token fails the request closed (`503 error_kind=mcp_unavailable`) instead of
degrading to an unauthenticated call, and a server that refuses the configured
credential (401/403) is recorded as unavailable for the same reason. At-rest
encryption of MCP headers is deliberately **not** implemented: the config file
is operator-managed and outside the secret store; treat it as a secret file
(restrict its permissions and never commit it), or front the MCP server with a
local authenticating proxy. `api_keys[].allowed_tools` (see below) caps which
tools a key may invoke regardless of the server's own credentials.

#### Upgrading from earlier versions

Installs whose credentials are placeholders or old quick-start literals will
now fail to load — `secret get` surfaces the refusal; `describe` reports a
`secrets_invalid` diagnostic (exit 0); only `secret set` remains usable for
recovery. Generate replacements in place:

```bash
nenya secret set --client-token                      # generates nk-… + 32 bytes
nenya secret set --provider deepseek sk-<your-real-key>
```

or, for a throwaway environment only, export `NENYA_ALLOW_WEAK_SECRETS=1`
before starting.

### Secure Memory (mlock)

Nenya uses `mlock` to keep authentication tokens in RAM, preventing them from being swapped to disk. This requires systemd to be configured with:

```ini
[Service]
LimitMEMLOCK=infinity
```

If this setting is missing, Nenya will fail to start with error:
```
secure memory allocation failed
```

See [`SECURITY.md`](SECURITY.md#secure-memory-storage) for implementation details.

## Migration from Environment Variables

If you previously used `NENYA_CLIENT_TOKEN` and `NENYA_PROVIDER_KEY_*` environment variables, migrate to JSON files:

**Before (deprecated):**
```bash
export NENYA_CLIENT_TOKEN="nk-..."
export NENYA_PROVIDER_KEY_DEEPSEEK="sk-..."
export NENYA_PROVIDER_KEY_GEMINI="AIza..."
```

**After:**
```json
// 01-client.json
{"client_token": "nk-$(openssl rand -hex 32)"}

// 02-providers.json
{"provider_keys": {"deepseek": "sk-<your-real-key>", "gemini": "AIza<your-real-key>"}}
```

Environment variables are no longer supported. Use JSON files only.
