#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "$0")/.." 2>/dev/null && pwd -P || true)"
if [ -n "$ROOT_DIR" ] && [ -f "$ROOT_DIR/install.sh" ]; then
  exec bash "$ROOT_DIR/install.sh" "$@"
fi

command -v curl >/dev/null 2>&1 || { echo "sn-cli install: curl is required" >&2; exit 1; }
curl -fsSL https://raw.githubusercontent.com/yy003x/runtime/main/install.sh | bash -s -- "$@"
