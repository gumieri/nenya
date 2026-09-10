#!/usr/bin/env bash
# Stop the demo harness started by up.sh and clean up its workdir.
# Uses pkill on the unique workdir paths: the recorded PID of a setsid
# process is the supervisor, not the daemon itself.
set -euo pipefail

WORK=/tmp/nenya-demo
if [ -f "$WORK/nenya.pid" ]; then
  kill "$(cat "$WORK/nenya.pid")" 2>/dev/null || true
fi
# Bracket trick so pkill never matches this script's own command line.
pkill -f "nenya-demo/[m]ock" 2>/dev/null || true
pkill -f "$WORK/[n]enya" 2>/dev/null || true
# Kill anything still holding the demo ports (relative cmdlines like ./mock
# are invisible to pkill -f patterns).
fuser -k 8080/tcp 9999/tcp >/dev/null 2>&1 || true
sleep 0.3
rm -rf "$WORK"
echo "demo harness stopped"
