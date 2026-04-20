# Secrets Format for Nenya Gateway

Nenya uses systemd credentials to securely load API keys and tokens. The secrets file must be a JSON file with the following structure:

```json
{
  "client_token": "your-client-bearer-token-here",
  "provider_keys": {
    "gemini": "AIza...",
    "deepseek": "sk-...",
    "zai": "...",
    "groq": "gsk_...",
    "together": "...",
    "openai": "sk-..."
  }
}
```

## Deployment with systemd

1. Create a JSON file with the above format (e.g., `/etc/nenya/secrets.json`)
2. Configure the systemd service unit to load it as a credential:

```ini
[Service]
...
LoadCredential=secrets:/etc/nenya/secrets.json
```

The gateway will read the file from `$CREDENTIALS_DIRECTORY/secrets`.

## Container / Kubernetes Deployment

When running outside systemd (Docker Compose, Kubernetes, etc.), secrets can be supplied via environment variables:

```bash
NENYA_CLIENT_TOKEN=your-client-bearer-token-here
NENYA_PROVIDER_KEY_GEMINI=AIza...
NENYA_PROVIDER_KEY_DEEPSEEK=sk-...
NENYA_PROVIDER_KEY_OPENAI=sk-proj-...
```

**Resolution order:** `CREDENTIALS_DIRECTORY` (systemd) takes precedence. If the secrets file is not found, the gateway falls back to `NENYA_` environment variables.

**Listen address override:** Use `NENYA_LISTEN_ADDR` (full address like `:9090`) or `PORT` (port number only, e.g. `8080`) to override the configured listen address. Config file value is the default.

### Docker Compose example

```yaml
services:
  nenya:
    image: ghcr.io/gumieri/nenya:latest
    ports:
      - "8080:8080"
    volumes:
      - ./config:/etc/nenya:ro
    environment:
      NENYA_CLIENT_TOKEN: ${NENYA_CLIENT_TOKEN}
      NENYA_PROVIDER_KEY_GEMINI: ${GEMINI_API_KEY}
      NENYA_PROVIDER_KEY_DEEPSEEK: ${DEEPSEEK_API_KEY}
    restart: unless-stopped
```

### Kubernetes example

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: nenya-config
data:
  00-server.json: |
    {"server": {"listen_addr": ":8080"}}
  10-agents.json: |
    {"agents": {"build": {"strategy": "fallback", "models": ["glm-5-turbo"]}}}
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
          ports:
            - containerPort: 8080
          volumeMounts:
            - name: config
              mountPath: /etc/nenya
              readOnly: true
          env:
            - name: NENYA_CLIENT_TOKEN
              valueFrom:
                secretKeyRef:
                  name: nenya-secrets
                  key: client-token
            - name: NENYA_PROVIDER_KEY_GEMINI
              valueFrom:
                secretKeyRef:
                  name: nenya-secrets
                  key: gemini-key
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
```

## Generating a Client Token

The `client_token` is used by AI clients (OpenCode/Aider) to authenticate to Nenya. Generate a secure random token:

```bash
openssl rand -hex 32
```

## Provider Keys

The `provider_keys` object maps provider names (matching provider keys in config) to their API keys. See [`PROVIDERS.md`](PROVIDERS.md) for the full list of built-in providers.

To add a custom provider (e.g., OpenAI), add its key under the matching provider name defined in `config.json`:

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
- The secrets file should be readable only by the service user (e.g., `nenya:nenya`)
- Use systemd's credential mechanism for secure in-memory storage
- Rotate tokens periodically
