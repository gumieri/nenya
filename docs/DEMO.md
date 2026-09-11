# Nenya Gateway Demo

Two ways to see Nenya in action: an **offline redaction demo** that runs entirely
on your machine with no API keys, and a **local end-to-end demo** against a real
provider.

## 1. Offline redaction demo (no API keys)

The `examples/demo/` harness starts Nenya plus a local mock upstream that dumps
every request it receives. Send a payload full of fake secrets and compare what
the upstream actually got:

```bash
./examples/demo/up.sh     # builds both binaries, generates dummy secrets
curl -sN --max-time 10 http://127.0.0.1:8080/v1/chat/completions \
  -H 'Authorization: Bearer nk-demo-demo-demo-demo' \
  -d @examples/demo/request.json
jq -r '.messages[].content' /tmp/nenya-demo/received.json   # [REDACTED] everywhere
./examples/demo/down.sh
```

The same harness renders the README GIF (`docs/demo.gif`):

```bash
mise run demo    # requires vhs v0.11.0, ttyd, ffmpeg
```

Only documented fake secrets are used (AKIAIOSFODNN7EXAMPLE, a fake `ghp_…`
token, a dummy client token). Nothing leaves your machine.

## 2. Local end-to-end demo (real provider)

### 1. Build the gateway

```bash
mise run build
```

### 2. Prepare configuration

Copy an example config and adjust as needed:

```bash
cp examples/example.config.json config.json
# or the minimal one:
cp examples/minimal_example.config.json config.json
```

### 3. Prepare secrets

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
```

### 4. Run locally (without systemd)

```bash
CREDENTIALS_DIRECTORY=$(pwd)/creds ./nenya -config config.json
```

Or use the mise task (creates dummy secrets automatically):

```bash
mise run run
```

### 5. Send a request

```bash
curl -N -H "Authorization: Bearer test-client-token" \
  -d '{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"hello"}]}' \
  http://localhost:8080/v1/chat/completions
```
