#!/usr/bin/env bash
set -euo pipefail

umask 077

SN_CLI_HOME="${SN_CLI_HOME:-$HOME/.sn}"
INSTALL_DIR="${SN_CLI_INSTALL_DIR:-${PREFIX:-$HOME/.local}/bin}"
REPOSITORY="${SN_CLI_SOURCE_REPOSITORY:-https://github.com/yy003x/runtime.git}"
REF="${SN_CLI_SOURCE_REF:-main}"
DRY_RUN=0

usage() {
  cat <<'EOF'
Install sn-cli by downloading and compiling its source code.

Usage:
  curl -fsSL https://raw.githubusercontent.com/yy003x/runtime/main/install-source.sh | bash
  bash install-source.sh [--ref REF] [--dry-run]

Options:
  --ref REF             Git branch, tag, or commit; default is main.
  --repository URL      Source Git repository.
  --install-dir DIR     Symlink directory; default is ~/.local/bin.
  --home DIR            Runtime home; default is ~/.sn.
  --dry-run             Print the source installation plan without writing files.
  -h, --help            Show this help.

Environment:
  SN_CLI_HOME                 Runtime home.
  SN_CLI_INSTALL_DIR          Symlink directory.
  SN_CLI_SOURCE_REPOSITORY    Source Git repository.
  SN_CLI_SOURCE_REF           Git branch, tag, or commit.

Source checkout:
  <home>/source/sn-runtime
EOF
}

log() { printf '%s\n' "$*" >&2; }
die() { printf 'sn-cli source install: %s\n' "$*" >&2; exit 1; }

safe_repository() {
  local value="${1%%\?*}" scheme remainder
  case "$value" in
    *://*@*)
      scheme="${value%%://*}"
      remainder="${value#*://}"
      printf '%s://****@%s\n' "$scheme" "${remainder#*@}"
      ;;
    *) printf '%s\n' "$value" ;;
  esac
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --ref) [ "$#" -ge 2 ] || die "--ref requires a value"; REF="$2"; shift 2 ;;
    --repository) [ "$#" -ge 2 ] || die "--repository requires a value"; REPOSITORY="$2"; shift 2 ;;
    --install-dir) [ "$#" -ge 2 ] || die "--install-dir requires a value"; INSTALL_DIR="$2"; shift 2 ;;
    --home) [ "$#" -ge 2 ] || die "--home requires a value"; SN_CLI_HOME="$2"; shift 2 ;;
    --dry-run) DRY_RUN=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) die "unknown option: $1" ;;
  esac
done

case "$REF" in
  ""|-*) die "invalid ref: $REF" ;;
esac
case "$SN_CLI_HOME" in
  ""|/) die "unsafe runtime home: $SN_CLI_HOME" ;;
esac

SOURCE_PARENT="$SN_CLI_HOME/source"
SOURCE_DIR="$SOURCE_PARENT/sn-runtime"

if [ "$DRY_RUN" = "1" ]; then
  log "sn-cli source install dry-run"
  log "repository: $(safe_repository "$REPOSITORY")"
  log "ref: $REF"
  log "source: $SOURCE_DIR"
  log "binary: $SN_CLI_HOME/bin/sn-cli"
  log "configs: $SN_CLI_HOME/configs"
  log "resources: $SN_CLI_HOME/resources"
  log "symlink: $INSTALL_DIR/sn-cli"
  exit 0
fi

require_command() {
  command -v "$1" >/dev/null 2>&1 || die "$1 is required"
}

require_command git
require_command go
require_command make

mkdir -p "$SOURCE_PARENT"
chmod 700 "$SN_CLI_HOME" "$SOURCE_PARENT"

TEMP_SOURCE=""
cleanup() {
  if [ -n "$TEMP_SOURCE" ]; then
    rm -rf "$TEMP_SOURCE"
  fi
}
trap cleanup EXIT

checkout_ref() {
  local directory="$1"
  git -C "$directory" fetch --force --tags origin "$REF"
  git -C "$directory" checkout --detach FETCH_HEAD
}

if [ -L "$SOURCE_DIR" ]; then
  die "source checkout must not be a symlink: $SOURCE_DIR"
elif [ -e "$SOURCE_DIR" ]; then
  [ -d "$SOURCE_DIR/.git" ] || die "source path is not a Git checkout: $SOURCE_DIR"
  CURRENT_REPOSITORY="$(git -C "$SOURCE_DIR" remote get-url origin 2>/dev/null || true)"
  [ "$CURRENT_REPOSITORY" = "$REPOSITORY" ] || die "source remote mismatch at $SOURCE_DIR: $(safe_repository "$CURRENT_REPOSITORY")"
  if [ -n "$(git -C "$SOURCE_DIR" status --porcelain --untracked-files=normal)" ]; then
    die "source checkout has local changes: $SOURCE_DIR"
  fi
  log "updating source: $SOURCE_DIR"
  checkout_ref "$SOURCE_DIR"
else
  TEMP_SOURCE="$SOURCE_PARENT/.sn-runtime.clone.$$"
  log "cloning source: $(safe_repository "$REPOSITORY")"
  git clone --no-checkout -- "$REPOSITORY" "$TEMP_SOURCE"
  checkout_ref "$TEMP_SOURCE"
  mv "$TEMP_SOURCE" "$SOURCE_DIR"
  TEMP_SOURCE=""
fi

for required in go.mod Makefile install.sh configs resources; do
  [ -e "$SOURCE_DIR/$required" ] || die "source checkout is missing $required"
done

COMMIT="$(git -C "$SOURCE_DIR" rev-parse --short HEAD)"
log "building sn-cli from $REF ($COMMIT)"
make -C "$SOURCE_DIR" sn-cli-build

bash "$SOURCE_DIR/install.sh" \
  --binary "$SOURCE_DIR/bin/sn-cli" \
  --configs "$SOURCE_DIR/configs" \
  --resources "$SOURCE_DIR/resources" \
  --home "$SN_CLI_HOME" \
  --install-dir "$INSTALL_DIR"

log "source checkout: $SOURCE_DIR"
