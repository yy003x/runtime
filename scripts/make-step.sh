#!/usr/bin/env bash
set -o pipefail

usage() {
  printf 'usage: make-step.sh [--live] --stage <name> [--meta <key=value>]... -- <command> [args...]\n' >&2
}

die() {
  printf 'make-step: %s\n' "$*" >&2
  exit 2
}

stage=""
live=0
metadata=()

while (($# > 0)); do
  case "$1" in
    --stage)
      (($# >= 2)) || die "--stage requires a value"
      stage="$2"
      shift 2
      ;;
    --meta)
      (($# >= 2)) || die "--meta requires key=value"
      metadata+=("$2")
      shift 2
      ;;
    --live)
      live=1
      shift
      ;;
    --)
      shift
      break
      ;;
    -h | --help)
      usage
      exit 0
      ;;
    *)
      die "unknown option: $1"
      ;;
  esac
done

[[ -n "$stage" ]] || die "--stage is required"
(($# > 0)) || die "command is required"

for item in "${metadata[@]}"; do
  [[ "$item" == *=* ]] || die "metadata must use key=value: $item"
  key="${item%%=*}"
  [[ "$key" =~ ^[A-Za-z_][A-Za-z0-9_.-]*$ ]] || die "invalid metadata key: $key"
done

cmd=("$@")
verbose=0
if [[ "${V:-0}" == "1" ]]; then
  verbose=1
  live=1
fi

emit_metadata() {
  local item key value
  for item in "${metadata[@]}"; do
    key="${item%%=*}"
    value="${item#*=}"
    printf ' %s=' "$key" >&2
    printf '%q' "$value" >&2
  done
}

emit_started() {
  printf '[make] stage=' >&2
  printf '%q' "$stage" >&2
  printf ' state=started' >&2
  emit_metadata
  printf '\n' >&2
}

emit_finished() {
  local state="$1"
  local result="$2"
  local elapsed="$3"
  local exit_code="${4:-}"

  printf '[make] stage=' >&2
  printf '%q' "$stage" >&2
  printf ' state=%s result=%s elapsed=%ss' "$state" "$result" "$elapsed" >&2
  if [[ -n "$exit_code" ]]; then
    printf ' exit=%s' "$exit_code" >&2
  fi
  emit_metadata
  printf '\n' >&2
}

emit_command() {
  local arg
  printf '[make] stage=' >&2
  printf '%q' "$stage" >&2
  printf ' command=' >&2
  for arg in "${cmd[@]}"; do
    printf '%q ' "$arg" >&2
  done
  printf '\n' >&2
}

output_file=""
cleanup() {
  if [[ -n "$output_file" && -f "$output_file" ]]; then
    rm -f -- "$output_file"
  fi
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

emit_started
if ((verbose == 1)); then
  emit_command
fi

started_at=$SECONDS
if ((live == 1)); then
  "${cmd[@]}"
  status=$?
else
  output_file="$(mktemp "${TMPDIR:-/tmp}/sn-runtime-make-step.XXXXXX")" ||
    die "cannot create temporary output file"
  "${cmd[@]}" >"$output_file" 2>&1
  status=$?
fi
elapsed=$((SECONDS - started_at))

if ((status == 0)); then
  emit_finished completed success "$elapsed"
  exit 0
fi

if ((live == 0)) && [[ -s "$output_file" ]]; then
  printf '[make] stage=' >&2
  printf '%q' "$stage" >&2
  printf ' state=failed-output\n' >&2
  cat -- "$output_file" >&2
fi
emit_finished failed failure "$elapsed" "$status"
exit "$status"
