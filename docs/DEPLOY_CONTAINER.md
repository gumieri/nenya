# Deploy Container (Podman/Docker Compose)

This guide covers running Nenya as a container using Podman or Docker Compose.

## Image

Container image is available on GitHub Container Registry with multi-arch support (amd64/arm64):

```
ghcr.io/gumieri/nenya:latest
```

## Quick Run with Podman

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

Test:

```bash
curl -H "Authorization: Bearer $(jq -r '.client_token' secrets/client.json)" \
  http://localhost:8080/healthz
```

## Docker Compose

Use the provided `deploy/compose.yml`:

```bash
mkdir -p config secrets
# Create config files in ./config/ (e.g., config.json)
# Create secrets files in ./secrets/*.json
podman compose -f deploy/compose.yml up -d
```

With Docker:

```bash
docker compose -f deploy/compose.yml up -d
```

### Compose File Reference

```yaml
services:
  nenya:
    image: ghcr.io/gumieri/nenya:latest
    container_name: nenya
    ports:
      - "8080:8080"
    volumes:
      - ./config:/etc/nenya:ro      # config directory (read-only)
      - ./secrets:/run/secrets/nenya:ro  # secrets directory (read-only)
    environment:
      NENYA_SECRETS_DIR: /run/secrets/nenya
    cap_drop:
      - ALL
    cap_add:
      - IPC_LOCK                   # required for secure memory (mlock)
    security_opt:
      - no-new-privileges:true     # prevent privilege escalation
    read_only: true               # immutable root filesystem
    tmpfs:
      - /tmp:rw,noexec,nosuid,size=64M  # tmpfs for /tmp
    restart: unless-stopped
```

### Environment Variables

| Variable | Description |
|----------|-------------|
| `NENYA_CONFIG_DIR` | Config root directory (default: `/etc/nenya/`) |
| `NENYA_CONFIG_FILE` | Single config file (overrides `NENYA_CONFIG_DIR`) |
| `NENYA_SECRETS_DIR` | Secrets directory for merged `*.json` files |

## Security Notes

The container runs with strict security controls:

| Option | Purpose |
|--------|---------|
| `--cap-drop=ALL --cap-add=IPC_LOCK` | Required for secure memory (mlock) |
| `--security-opt=no-new-privileges:true` | Prevents privilege escalation |
| `--read-only` | Enforces immutable root filesystem |
| `--tmpfs /tmp` | tmpfs for /tmp to prevent writes |
| Non-root user (UID 65532) | Container runs as non-root |

**Important:** `IPC_LOCK` capability is required for secure memory storage. Without it, Nenya will fail to start with `ErrMLockFailure`.

## Image Verification

Verify the signed image with cosign:

```bash
# Verify image signature
cosign verify ghcr.io/gumieri/nenya:latest

# Verify SBOM attestation
cosign verify-attestation --type spdx ghcr.io/gumieri/nenya:latest
```

## Troubleshooting

### Container fails to start

Check container logs:

```bash
podman logs nenya
```

Common issues:
- **Port already in use**: Another container or service using port 8080
- **`ErrMLockFailure`**: Missing `IPC_LOCK` capability
- **Config not found**: Volume mount paths are incorrect

### Config changes not applied

For podman, restart the container:

```bash
podman restart nenya
```

For compose:

```bash
podman compose -f deploy/compose.yml restart nenya
```

### Secrets not loading

Ensure the secrets directory is correctly mounted and files have the right format:

```bash
# Verify secrets directory contents
ls -la secrets/

# Check secrets can be read
cat secrets/client.json
cat secrets/provider_keys.json
```

### Health check failing

Test the health endpoint directly:

```bash
curl http://localhost:8080/healthz
```

Expected response: `{"status":"ok","engine":"ollama"}` (or similar)

## Image Updates

Pull the latest image:

```bash
podman pull ghcr.io/gumieri/nenya:latest
podman stop nenya
podman rm nenya
# Then re-run the podman run command above
```

Or with compose:

```bash
podman compose -f deploy/compose.yml pull
podman compose -f deploy/compose.yml up -d --force-recreate
```
