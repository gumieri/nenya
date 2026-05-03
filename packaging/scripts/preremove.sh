#!/usr/bin/env bash
set -euo pipefail

systemctl stop --now nenya.service || true
systemctl stop --now nenya.socket || true
systemctl disable --now nenya.service nenya.socket || true