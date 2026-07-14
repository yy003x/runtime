#!/usr/bin/env bash
# Install the sn-cli launcher and binary.
# Local checkout:
#   bash scripts/install-sn-cli.sh
# Remote install:
#   curl -fsSL https://raw.githubusercontent.com/yy003x/runtime/main/scripts/install-sn-cli.sh | bash

set -euo pipefail

DEFAULT_REPO_URL="https://github.com/yy003x/runtime.git"
DEFAULT_REF="main"

SN_CLI_REPO_URL="${SN_CLI_REPO_URL:-$DEFAULT_REPO_URL}"
SN_CLI_REF="${SN_CLI_REF:-$DEFAULT_REF}"
SN_CLI_HOME="${SN_CLI_HOME:-$HOME/.sn-cli}"
SN_CLI_REPO_DIR="${SN_CLI_REPO_DIR:-$SN_CLI_HOME/runtime}"
SN_CLI_INSTALL_DIR="${SN_CLI_INSTALL_DIR:-${PREFIX:-$HOME/.local}/bin}"
SN_CLI_REPO="${SN_CLI_REPO:-}"
SN_CLI_FORCE_CLONE="${SN_CLI_FORCE_CLONE:-0}"

DRY_RUN=0

usage() {
  cat <<'EOF_USAGE'
Install sn-cli.

Usage:
  bash scripts/install-sn-cli.sh [options]
  curl -fsSL https://raw.githubusercontent.com/yy003x/runtime/main/scripts/install-sn-cli.sh | bash

Options:
  --dry-run              Print the install plan without writing files.
  --install-dir DIR      Install launcher into DIR. Default: $HOME/.local/bin.
  --repo-dir DIR         Clone/update runtime repo at DIR when no local repo is found.
  --local-repo DIR       Use an existing runtime checkout as source.
  --repo-url URL         Git URL used by curl/bash installs.
  --ref REF              Branch or tag to checkout for cloned installs. Default: main.
  -h, --help             Show this help.

Environment:
  SN_CLI_INSTALL_DIR     Same as --install-dir.
  PREFIX                 Uses PREFIX/bin when SN_CLI_INSTALL_DIR is not set.
  SN_CLI_HOME            Base directory for managed checkout. Default: $HOME/.sn-cli.
  SN_CLI_REPO_DIR        Managed checkout path. Default: $SN_CLI_HOME/runtime.
  SN_CLI_REPO            Existing local checkout to use.
  SN_CLI_REPO_URL        Git URL for managed checkout.
  SN_CLI_REF             Branch or tag for managed checkout.
  SN_CLI_FORCE_CLONE=1   Ignore the current checkout and use SN_CLI_REPO_DIR.
EOF_USAGE
}

log() {
  printf '%s\n' "$*" >&2
}

die() {
  printf 'install-sn-cli: %s\n' "$*" >&2
  exit 1
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --dry-run)
      DRY_RUN=1
      shift
      ;;
    --install-dir)
      [ "$#" -ge 2 ] || die "--install-dir requires a value"
      SN_CLI_INSTALL_DIR="$2"
      shift 2
      ;;
    --repo-dir)
      [ "$#" -ge 2 ] || die "--repo-dir requires a value"
      SN_CLI_REPO_DIR="$2"
      shift 2
      ;;
    --local-repo)
      [ "$#" -ge 2 ] || die "--local-repo requires a value"
      SN_CLI_REPO="$2"
      SN_CLI_FORCE_CLONE=0
      shift 2
      ;;
    --repo-url)
      [ "$#" -ge 2 ] || die "--repo-url requires a value"
      SN_CLI_REPO_URL="$2"
      shift 2
      ;;
    --ref)
      [ "$#" -ge 2 ] || die "--ref requires a value"
      SN_CLI_REF="$2"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      die "unknown option: $1"
      ;;
  esac
done

require_command() {
  command -v "$1" >/dev/null 2>&1 || die "$1 is required"
}

abs_dir() {
  local dir="$1"
  [ -d "$dir" ] || return 1
  (cd "$dir" && pwd -P)
}

is_sn_cli_repo() {
	local dir="$1"
	[ -f "$dir/go.mod" ] &&
	  [ -f "$dir/cmd/sn-cli/main.go" ]
}

find_repo_upwards() {
  local dir="$PWD"
  while [ "$dir" != "/" ]; do
    if is_sn_cli_repo "$dir"; then
      abs_dir "$dir"
      return 0
    fi
    dir="$(dirname "$dir")"
  done
  return 1
}

update_managed_repo() {
  require_command git
  local repo_dir="$1"
  local parent
  parent="$(dirname "$repo_dir")"
  mkdir -p "$parent"

  if [ -d "$repo_dir/.git" ]; then
    log "Updating runtime checkout: $repo_dir"
    git -C "$repo_dir" fetch --tags origin "$SN_CLI_REF" >&2
    if git -C "$repo_dir" show-ref --verify --quiet "refs/heads/$SN_CLI_REF"; then
      git -C "$repo_dir" checkout -q "$SN_CLI_REF"
      git -C "$repo_dir" pull --ff-only origin "$SN_CLI_REF" >&2
    else
      git -C "$repo_dir" checkout -q --detach "origin/$SN_CLI_REF"
    fi
  else
    if [ -e "$repo_dir" ]; then
      die "repo dir exists but is not a git checkout: $repo_dir"
    fi
    log "Cloning runtime checkout: $repo_dir"
    git clone --depth 1 --branch "$SN_CLI_REF" "$SN_CLI_REPO_URL" "$repo_dir" >&2
  fi

  is_sn_cli_repo "$repo_dir" || die "managed checkout is not a valid sn-cli repo: $repo_dir"
  abs_dir "$repo_dir"
}

build_binary() {
  local repo="$1"
  local version
  local commit
  local build_date
  local output
  local tmp_output
  require_command go
  mkdir -p "$repo/runs/global/sn-cli/storage/current/bin"
  version="$(git -C "$repo" describe --tags --always --dirty 2>/dev/null || printf '0.1.0-dev')"
  commit="$(git -C "$repo" rev-parse --short HEAD 2>/dev/null || printf 'unknown')"
  build_date="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  output="$repo/runs/global/sn-cli/storage/current/bin/sn-cli"
  tmp_output="$output.tmp.$$"
  log "Building sn-cli binary"
	(
	  cd "$repo" &&
	    go build \
	      -ldflags "-X agent-runtime/internal/agentrun.Version=$version -X agent-runtime/internal/cli/version.Version=$version -X agent-runtime/internal/cli/version.Commit=$commit -X agent-runtime/internal/cli/version.BuildDate=$build_date" \
	      -o "$tmp_output" \
	      ./cmd/sn-cli
	  )
  mv "$tmp_output" "$output"
  chmod +x "$output"
  [ -x "$output" ] || die "binary was not created"
}

validate_binary() {
  local repo="$1"
  local binary="$repo/runs/global/sn-cli/storage/current/bin/sn-cli"
  log "Validating runtime profiles"
  SN_CLI_ROOT="$repo" "$binary" profiles >/dev/null
  SN_CLI_ROOT="$repo" "$binary" config validate --name fake >/dev/null
}

write_launcher() {
  local repo="$1"
  local launcher="$2"
  local tmp="$launcher.tmp.$$"

  {
    printf '#!/usr/bin/env bash\n'
    printf 'set -euo pipefail\n'
    printf 'DEFAULT_SN_CLI_ROOT=%q\n' "$repo"
    printf 'SN_CLI_ROOT="${SN_CLI_ROOT:-$DEFAULT_SN_CLI_ROOT}"\n'
    printf 'export SN_CLI_ROOT\n'
    printf 'exec "$SN_CLI_ROOT/runs/global/sn-cli/storage/current/bin/sn-cli" "$@"\n'
  } > "$tmp"
  chmod +x "$tmp"
  mv "$tmp" "$launcher"
}

print_path_hint() {
  case ":$PATH:" in
    *":$SN_CLI_INSTALL_DIR:"*) ;;
    *)
      log "PATH hint: add $SN_CLI_INSTALL_DIR to PATH to run sn-cli directly."
      ;;
  esac
}

LOCAL_REPO=""
if [ "$SN_CLI_FORCE_CLONE" != "1" ]; then
  if [ -n "$SN_CLI_REPO" ]; then
    is_sn_cli_repo "$SN_CLI_REPO" || die "SN_CLI_REPO is not a sn-cli checkout: $SN_CLI_REPO"
    LOCAL_REPO="$(abs_dir "$SN_CLI_REPO")"
  elif LOCAL_REPO="$(find_repo_upwards 2>/dev/null)"; then
    :
  else
    LOCAL_REPO=""
  fi
fi

if [ -n "$LOCAL_REPO" ]; then
  REPO_SOURCE="local"
  REPO_ROOT="$LOCAL_REPO"
else
  REPO_SOURCE="managed"
  REPO_ROOT="$SN_CLI_REPO_DIR"
fi

if [ "$DRY_RUN" = "1" ]; then
  log "sn-cli install dry-run"
  log "repo source: $REPO_SOURCE"
  log "repo root: $REPO_ROOT"
  log "repo url: $SN_CLI_REPO_URL"
  log "ref: $SN_CLI_REF"
  log "install dir: $SN_CLI_INSTALL_DIR"
  log "launcher: $SN_CLI_INSTALL_DIR/sn-cli"
  exit 0
fi

if [ "$REPO_SOURCE" = "managed" ]; then
  REPO_ROOT="$(update_managed_repo "$SN_CLI_REPO_DIR")"
else
  log "Using local sn-cli checkout: $REPO_ROOT"
fi

build_binary "$REPO_ROOT"
validate_binary "$REPO_ROOT"
mkdir -p "$SN_CLI_INSTALL_DIR"
write_launcher "$REPO_ROOT" "$SN_CLI_INSTALL_DIR/sn-cli"

log "Installed: $SN_CLI_INSTALL_DIR/sn-cli"
log "sn-cli root: $REPO_ROOT"
print_path_hint
log "Try: sn-cli --help"
