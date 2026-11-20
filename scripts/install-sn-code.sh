#!/usr/bin/env bash
set -euo pipefail

[ "$#" -eq 1 ] || { echo 'usage: install-sn-code.sh <built-binary>' >&2; exit 2; }
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
runtime_home="${SN_CLI_HOME:-$HOME/.sn}"
command_dir="${SN_CODE_INSTALL_DIR:-${PREFIX:-$HOME/.local}/bin}"

# go -C 切换到源码目录前，保留调用者传入的相对路径。
absolute_path() {
  case "$1" in
    /*) printf '%s\n' "$1" ;;
    *) printf '%s/%s\n' "$PWD" "$1" ;;
  esac
}

exec "${GO:-go}" -C "$root" run ./cmd/install-sn-code \
  "$(absolute_path "$1")" "$(absolute_path "$runtime_home")" "$(absolute_path "$command_dir")"
