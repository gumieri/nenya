# Nenya Gateway Demo

## Prerequisites

1. Install Go 1.26+
2. No external dependencies — Go standard library only
3. Have a local Ollama instance running with `qwen2.5-coder:7b` model (or adjust config)
4. Obtain API keys for at least one upstream provider (Gemini, DeepSeek, or z.ai)

## Quick Start

### 1. Build the gateway
```bash
mise run build
```

### 2. Prepare configuration
Copy the example config and adjust as needed:
```bash
cp example.config.json config.json
```

### 3. Prepare secrets (for systemd credentials)
Create a JSON file with your API keys and client token:
```json
{
  "client_token": "test-client-token",
  "provider_keys": {
    "gemini": "your-gemini-key",
    "deepseek": "your-deepseek-key",
    "zai": "your-zai-key"
  }
}
```

### 4. Run locally (without systemd)
Create the secrets file and run:
```bash
mkdir -p creds
cat > creds/secrets << 'EOF'
{
  "client_token": "test-client-token",
  "provider_keys": {
    "gemini": "your-gemini-key",
    "deepseek": "your-deepseek-key",
    "zai": "your-zai-key"
  }
}
EOF

CREDENTIALS_DIRECTORY=$(pwd)/creds ./nenya -config config.json
```

Or use the mise task (creates dummy secrets automatically):
```bash
mise run run
```

The gateway will listen on `:8080`.

## Testing the Pipeline

### Tier 1 (Pass‑through) – Small payload
```bash
curl -X POST http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer test-client-token" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gemini-3-flash",
    "messages": [
      {"role": "user", "content": "Say hello"}
    ]
  }'
```

### Tier 2 (Ollama summarization) – Medium payload (~4000‑24000 runes)
```bash
# Generate a large payload (e.g., 5000 characters)
LARGE_PAYLOAD=$(python3 -c "print('x' * 5000)")
curl -X POST http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer test-client-token" \
  -H "Content-Type: application/json" \
  -d "$(cat <<EOF
{
  "model": "deepseek-chat",
  "messages": [
    {"role": "user", "content": "$LARGE_PAYLOAD"}
  ]
}
EOF
)"
```

### Tier 3 (Truncation + Ollama) – Huge payload (>24000 runes)
```bash
HUGE_PAYLOAD=$(python3 -c "print('y' * 30000)")
curl -X POST http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer test-client-token" \
  -H "Content-Type: application/json" \
  -d "$(cat <<EOF
{
  "model": "glm-5",
  "messages": [
    {"role": "user", "content": "$HUGE_PAYLOAD"}
  ]
}
EOF
)"
```

### Tier 3 (TF-IDF Truncation) – Relevance-scored pruning
When `tfidf_query_source` is set in context, TF-IDF scores content blocks by relevance to prior messages:
```bash
curl -X POST http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer test-client-token" \
  -H "Content-Type: application/json" \
  -d "$(cat <<EOF
{
  "model": "glm-5",
  "messages": [
    {"role": "user", "content": "fix the login handler JWT validation"},
    {"role": "user", "content": "$HUGE_PAYLOAD"}
  ]
}
EOF
)"
```

## Observing Logs

The gateway logs to stdout. Look for indicators:

- `[INFO] Payload within soft limit` – Tier 1 (pass‑through)
- `[WARN] Payload exceeds soft limit, sending to Ollama` – Tier 2 (Ollama only)
- `[WARN] Payload exceeds hard limit, applying middle‑out truncation` – Tier 3 (truncate + Ollama)
- `[INFO] TF-IDF truncation enabled` – Tier 3 with TF-IDF relevance scoring
- `[INFO] TF-IDF reduced payload below soft limit, skipping engine` – TF-IDF eliminated the need for engine call
- `[RATELIMIT] RPM limit exceeded` – Rate limiting active
- `[INFO] response cache hit` – Cached response replayed
- `[WARN] [CIRCUIT BREAKER]` – Circuit tripped or recovering

## Response Cache

When `response_cache.enabled` is set to `true`, responses are cached and replayed on matching requests.

### Observing Cache Hits

Check for the `X-Nenya-Cache-Status` header in the response:

```bash
curl -v -X POST http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer test-client-token" \
  -H "Content-Type: application/json" \
  -d '{"model":"gemini-3-flash","messages":[{"role":"user","content":"Say hello"}]}'
```

First request: `X-Nenya-Cache-Status: MISS` (or no header on some HTTP clients)
Second identical request: `X-Nenya-Cache-Status: HIT`

### Forcing Cache Bypass

Send the configured `force_refresh_header` (default: `x-nenya-cache-force-refresh`) with any non-empty value:

```bash
curl -X POST http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer test-client-token" \
  -H "x-nenya-cache-force-refresh: true" \
  -H "Content-Type: application/json" \
  -d '{"model":"gemini-3-flash","messages":[{"role":"user","content":"Say hello"}]}'
```

This forces a cache miss regardless of whether a cached response exists.

## Circuit Breaker

Circuit breakers protect against cascading failures by tripping (skipping) models that are failing.

### Observing Circuit State

Check the `/statsz` endpoint for the `circuit_breakers` field:

```bash
curl http://localhost:8080/statsz | jq '.circuit_breakers'
```

Example output:

```json
{
  "build:gemini:gemini-3-flash": "closed",
  "build:deepseek:deepseek-reasoner": "open",
  "plan:zai:glm-5-turbo": "half_open"
}
```

- **closed**: Normal operation
- **open**: Circuit tripped, requests skipped until cooldown expires
- **half_open**: Probing with limited requests to test recovery

### Triggering a Circuit Trip

Repeatedly send a request to a non-existent model or invalid endpoint to trigger failures. After `failure_threshold` (default 5) consecutive failures, the circuit will trip and the gateway will log:

```
[WARN] [CIRCUIT BREAKER] agent:provider:model transitioned from closed to open
```

Subsequent requests to that model will be skipped, falling back to the next model in the agent's chain.

## Systemd Deployment

1. Install the gateway system‑wide:
```bash
sudo mise run install
```

2. Place your secrets file at `/etc/nenya/secrets.json` (adjust ownership)
3. Enable and start the service:
```bash
sudo systemctl enable --now nenya
```

4. Check status:
```bash
sudo systemctl status nenya
```

## Troubleshooting

### Ollama connection refused
Ensure Ollama is running and the URL in config matches:
```bash
curl http://127.0.0.1:11434/api/version
```

### Authentication failures
- Verify the `client_token` matches the `Authorization` header
- Ensure upstream API keys are valid and have sufficient quota

### Rate limiting
Adjust `max_tpm` and `max_rpm` in `config.json` or set to `0` to disable.

### UTF‑8 handling
The gateway counts **runes** (Unicode code points), not bytes. Non‑ASCII characters count as one rune each.