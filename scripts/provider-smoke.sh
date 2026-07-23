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
trap 'SN_CLI_HOME="$runtime_home" "$ROOT_DIR/bin/sn-cli" system stop >/dev/null 2>&1 || true; rm -rf "$runtime_home"' EXIT
mkdir -p "$runtime_home/configs" "$runtime_home/resources"
cp -R "$ROOT_DIR/configs/." "$runtime_home/configs/"
cp -R "$ROOT_DIR/resources/." "$runtime_home/resources/"

SN_CLI_HOME="$runtime_home" "$ROOT_DIR/bin/sn-cli" profile validate "$profile" --live >/dev/null
run_output="$(SN_CLI_HOME="$runtime_home" "$ROOT_DIR/bin/sn-cli" session run --json \
  --project provider-smoke --cwd "$ROOT_DIR" --deadline-seconds 120 "$profile" "$prompt")"
printf '%s\n' "$run_output" | grep -Eq '"state"[[:space:]]*:[[:space:]]*"done"' || {
  printf '%s\n' "$run_output" >&2
  printf '%s\n' "provider-smoke: real provider run did not finish successfully" >&2
  exit 1
}
printf '%s\n' "provider-smoke: passed profile=$profile" >&2
