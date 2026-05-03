#!/usr/bin/env bash
set -euo pipefail

systemctl daemon-reload
systemctl enable --now nenya.socket || true
systemctl enable --now nenya.service || true