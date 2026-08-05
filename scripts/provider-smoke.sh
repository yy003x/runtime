#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if [ "${SN_REAL_PROVIDER_SMOKE:-}" != "1" ]; then
  printf '%s\n' "provider-smoke: skipped; set SN_REAL_PROVIDER_SMOKE=1 to allow a real API call" >&2
  exit 0
fi

profile="${SN_SMOKE_PROFILE:-api-cx}"
prompt="${SN_SMOKE_PROMPT:-只回复 runtime-provider-smoke-ok，不调用工具。}"
runtime_home="$(mktemp -d)"
trap 'rm -rf "$runtime_home"' EXIT
mkdir -p \
  "$runtime_home/configs" \
  "$runtime_home/tools" \
  "$runtime_home/resources/schema"
cp "$ROOT_DIR"/configs/*.json "$runtime_home/configs/"
cp "$ROOT_DIR"/resources/tools/*.json "$runtime_home/tools/"
cp "$ROOT_DIR/release/runtime.json" "$runtime_home/runtime.json"
cp -R "$ROOT_DIR/resources/schema/." "$runtime_home/resources/schema/"
cp "$ROOT_DIR/release/release.json" "$ROOT_DIR/release/tmux.conf" \
  "$runtime_home/resources/"

SN_CLI_HOME="$runtime_home" "$ROOT_DIR/bin/sn-cli" profile check "$profile" >/dev/null
run_output="$(SN_CLI_HOME="$runtime_home" "$ROOT_DIR/bin/sn-cli" --json req "$profile" "$prompt")"
printf '%s\n' "$run_output" | grep -Eq '"state"[[:space:]]*:[[:space:]]*"completed"' || {
  printf '%s\n' "$run_output" >&2
  printf '%s\n' "provider-smoke: real provider run did not finish successfully" >&2
  exit 1
}
printf '%s\n' "provider-smoke: passed profile=$profile" >&2
