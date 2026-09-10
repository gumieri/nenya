#!/usr/bin/env bash
# Start the demo harness: builds the mock upstream and the Nenya gateway,
# generates dummy secrets under /tmp (nothing real, nothing committed),
# starts both, and waits until Nenya has discovered the mock's model catalog.
set -euo pipefail
cd "$(dirname "$0")"

REPO_ROOT="$(git rev-parse --show-toplevel)"
WORK=/tmp/nenya-demo

rm -rf "$WORK"
mkdir -p "$WORK/secrets"

# Pre-clean: kill strays from previous runs that may hold the demo ports.
fuser -k 9999/tcp 8080/tcp >/dev/null 2>&1 || true
sleep 0.3

go build -o "$WORK/mock" ./mock
go build -o "$WORK/nenya" "$REPO_ROOT/cmd/nenya"

printf '{"client_token": "nk-demo-demo-demo-demo"}\n' > "$WORK/secrets/client.json"
printf '{"provider_keys": {"mock": "sk-demo-mock"}}\n' > "$WORK/secrets/provider_keys.json"
printf 'Authorization: Bearer nk-demo-demo-demo-demo\n' > "$WORK/auth"

(cd "$WORK" && DEMO_DUMP_DIR="$WORK" setsid ./mock </dev/null >mock.log 2>&1 &)

NENYA_CONFIG_FILE="$PWD/demo.config.json" \
NENYA_SECRETS_DIR="$WORK/secrets" \
  setsid "$WORK/nenya" </dev/null >"$WORK/nenya.log" 2>&1 &
echo $! >"$WORK/nenya.pid"
for _ in $(seq 1 50); do
  if curl -sf http://127.0.0.1:8080/healthz >/dev/null 2>&1; then
    sleep 1 # allow model discovery to settle
    echo "demo harness up: nenya on :8080, mock upstream on :9999 (workdir $WORK)"
    exit 0
  fi
  sleep 0.2
done

echo "demo harness failed to become healthy; logs in $WORK" >&2
exit 1
