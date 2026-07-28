#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

GO="${GO:-go}"
GOCACHE="${GOCACHE:-$("$GO" env GOCACHE)}"
GOMODCACHE="${GOMODCACHE:-$("$GO" env GOMODCACHE)}"
SERVER_ADDR="${SERVER_ADDR:-127.0.0.1:8080}"

case "$(uname -s)" in
  Darwin) stat_format=(-f '%m %N') ;;
  Linux) stat_format=(-c '%Y %n') ;;
  *)
    printf 'dev: unsupported operating system: %s\n' "$(uname -s)" >&2
    exit 1
    ;;
esac

checksum_stream() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum
    return
  fi
  if command -v shasum >/dev/null 2>&1; then
    shasum -a 256
    return
  fi
  printf 'dev: sha256sum or shasum is required\n' >&2
  return 1
}

source_signature() {
  find agent cmd command contract internal model profile provider run runtimetest session store transport configs resources \
    -type f \( -name '*.go' -o -name '*.json' \) -print0 |
    xargs -0 stat "${stat_format[@]}" |
    LC_ALL=C sort |
    checksum_stream |
    awk '{print $1}'
}

pid=""
signal_tree() {
  local signal="$1"
  local parent="$2"
  local child

  if command -v pgrep >/dev/null 2>&1; then
    for child in $(pgrep -P "$parent" 2>/dev/null || true); do
      signal_tree "$signal" "$child"
    done
  fi
  kill "-$signal" "$parent" 2>/dev/null || true
}

child_is_running() {
  local child="$1"
  local process_state

  kill -0 "$child" 2>/dev/null || return 1
  process_state="$(ps -o stat= -p "$child" 2>/dev/null || true)"
  [[ -n "$process_state" && "$process_state" != Z* ]]
}

stop_child() {
  local child="$pid"
  local attempt

  [[ -n "$child" ]] || return 0
  pid=""
  if child_is_running "$child"; then
    signal_tree TERM "$child"
    for ((attempt = 0; attempt < 10; attempt++)); do
      child_is_running "$child" || break
      sleep 0.1
    done
    if child_is_running "$child"; then
      signal_tree KILL "$child"
    fi
  fi
  wait "$child" 2>/dev/null || true
}

cleanup() {
  stop_child
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

check_child() {
  local exit_code

  [[ -n "$pid" ]] || return 0
  child_is_running "$pid" && return 0

  set +e
  wait "$pid"
  exit_code=$?
  set -e
  pid=""
  if ((exit_code == 0)); then
    exit_code=1
  fi
  printf 'dev: server process exited unexpectedly exit=%s\n' "$exit_code" >&2
  exit "$exit_code"
}

start_child() {
  HTTP_ADDR="$SERVER_ADDR" \
    GOCACHE="$GOCACHE" \
    GOMODCACHE="$GOMODCACHE" \
    "$GO" run ./cmd/sn-server &
  pid="$!"
  printf 'dev: server process started pid=%s address=%s\n' "$pid" "$SERVER_ADDR" >&2
}

last_sig=""
while true; do
  check_child
  if ! sig="$(source_signature)"; then
    printf 'dev: source signature failed; retrying\n' >&2
    sleep 1
    continue
  fi
  if [[ "$sig" != "$last_sig" ]]; then
    if [[ -n "$pid" ]]; then
      printf 'dev: change detected, restarting\n' >&2
      stop_child
    fi
    last_sig="$sig"
    start_child
  fi
  sleep 1
done
