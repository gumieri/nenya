# Secrets Format for Nenya Gateway

Nenya loads secrets from JSON files. Multiple secret sources are supported with priority ordering, and directories containing multiple JSON files are merged alphabetically (same behavior as config directory).

## Secret Paths (Priority Order)

| Priority | Path | Description |
|----------|------|-------------|
| 1 | `$CREDENTIALS_DIRECTORY/secrets` | systemd single file (highest priority) |
| 2 | `$CREDENTIALS_DIRECTORY/secrets.d/*.json` | systemd directory with merged JSON files |
| 3 | `$NENYA_SECRETS_DIR/*.json` | configurable path (env var) |
| 4 | `/run/secrets/nenya/*.json` | K8s/Docker standard path (fallback) |

The first path that exists is used. If multiple JSON files exist in a directory, they are merged alphabetically (last-wins for duplicate keys).

## JSON Format

All JSON files support the same structure. Fields can be split across multiple files:

```json
{
  "client_token": "nk-...",
  "provider_keys": {
    "gemini": "AIza...",
    "deepseek": "sk-...",
    "openai": "sk-proj-..."
  },
  "api_keys": {
    "dev-user": {
      "name": "dev-user",
      "token": "nk-...",
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
# Create secrets file
cat > /etc/nenya/secrets.json << 'EOF'
{
  "client_token": "nk-$(openssl rand -hex 32)",
  "provider_keys": {
    "deepseek": "sk-...",
    "gemini": "AIza..."
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

# Split into multiple files
cat > /etc/nenya/secrets.d/01-client.json << 'EOF'
{"client_token": "nk-..."}
EOF

cat > /etc/nenya/secrets.d/02-providers.json << 'EOF'
{"provider_keys": {"deepseek": "sk-...", "gemini": "AIza..."}}
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
├── 01-client.json     → {"client_token": "nk-..."}
├── 02-providers.json  → {"provider_keys": {"deepseek": "sk-..."}}
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
    {"client_token": "nk-..."}
  02-providers.json: |
    {"provider_keys": {"deepseek": "sk-...", "gemini": "AIza..."}}
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
# Output: nk-abc123def456... (prefix + 48 hex chars)
```

### API keys (for client RBAC)

Use the provided utility:

```bash
# Generate key with roles
go run ./cmd/nenya keygen --name "dev-user" --roles user,read-only --agents build,plan
```

Or manually create an `ApiKey` entry in JSON:

```json
{
  "api_keys": {
    "dev-user": {
      "name": "dev-user",
      "token": "nk-...",
      "roles": ["user"],
      "allowed_agents": ["build", "plan"],
      "enabled": true
    }
  }
}
```

## Provider Keys

The `provider_keys` object maps provider names to their API keys. See [`PROVIDERS.md`](PROVIDERS.md) for the full list of built-in providers.

To add a custom provider (e.g., OpenAI):

```json
{
  "provider_keys": {
    "openai": "sk-proj-..."
  }
}
```

At least one provider key must be present for the corresponding provider to work.

## Security Notes

- Never commit secrets to version control
- Secrets files should be readable only by the service user (`chmod 600`)
- Use systemd's credential mechanism for secure in-memory storage
- Rotate tokens periodically
- Path traversal (`..`) is rejected for all secret paths

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
{"client_token": "nk-..."}

// 02-providers.json
{"provider_keys": {"deepseek": "sk-...", "gemini": "AIza..."}}
```

Environment variables are no longer supported. Use JSON files only.
