#!/usr/bin/env bash
set -euo pipefail

umask 077

REPOSITORY="${SN_CLI_REPOSITORY:-yy003x/runtime}"
SN_CLI_HOME="${SN_CLI_HOME:-$HOME/.sn}"
INSTALL_DIR="${SN_CLI_INSTALL_DIR:-${PREFIX:-$HOME/.local}/bin}"
VERSION="${SN_CLI_VERSION:-}"
LOCAL_ARCHIVE=""
LOCAL_CHECKSUMS=""
LOCAL_BINARY=""
LOCAL_CONFIGS=""
LOCAL_RESOURCES=""
OVERWRITE_CONFIGS=0
DRY_RUN=0

usage() {
  cat <<'EOF'
Install sn-cli from a GitHub Release without downloading source code.

Usage:
  curl -fsSL https://raw.githubusercontent.com/yy003x/runtime/main/install.sh | bash
  bash install.sh [--version VERSION] [--dry-run]

Local package options used by `make install`:
  --binary FILE --configs DIR --resources DIR [--overwrite-configs]
  --archive FILE [--checksums FILE]

Options:
  --version VERSION    Install a specific release tag; default is latest.
  --install-dir DIR    Symlink directory; default is ~/.local/bin.
  --home DIR           Runtime home; default is ~/.sn.
  --overwrite-configs  Replace same-name config files; keep extra local files.
  --dry-run            Print the resolved install plan without writing files.
  -h, --help           Show this help.

Environment:
  SN_CLI_HOME                 Runtime home.
  SN_CLI_INSTALL_DIR          Symlink directory.
  SN_CLI_REPOSITORY           GitHub owner/repository.
  SN_CLI_DOWNLOAD_BASE_URL    Exact release asset directory, for mirrors/tests.
EOF
}

log() { printf '%s\n' "$*" >&2; }
die() { printf 'sn-cli install: %s\n' "$*" >&2; exit 1; }

while [ "$#" -gt 0 ]; do
  case "$1" in
    --version) [ "$#" -ge 2 ] || die "--version requires a value"; VERSION="$2"; shift 2 ;;
    --install-dir) [ "$#" -ge 2 ] || die "--install-dir requires a value"; INSTALL_DIR="$2"; shift 2 ;;
    --home) [ "$#" -ge 2 ] || die "--home requires a value"; SN_CLI_HOME="$2"; shift 2 ;;
    --archive) [ "$#" -ge 2 ] || die "--archive requires a value"; LOCAL_ARCHIVE="$2"; shift 2 ;;
    --checksums) [ "$#" -ge 2 ] || die "--checksums requires a value"; LOCAL_CHECKSUMS="$2"; shift 2 ;;
    --binary) [ "$#" -ge 2 ] || die "--binary requires a value"; LOCAL_BINARY="$2"; shift 2 ;;
    --configs) [ "$#" -ge 2 ] || die "--configs requires a value"; LOCAL_CONFIGS="$2"; shift 2 ;;
    --resources) [ "$#" -ge 2 ] || die "--resources requires a value"; LOCAL_RESOURCES="$2"; shift 2 ;;
    --overwrite-configs) OVERWRITE_CONFIGS=1; shift ;;
    --dry-run) DRY_RUN=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) die "unknown option: $1" ;;
  esac
done

require_command() {
  command -v "$1" >/dev/null 2>&1 || die "$1 is required"
}

platform() {
  local os arch
  case "$(uname -s)" in
    Darwin) os="darwin" ;;
    Linux) os="linux" ;;
    *) die "unsupported operating system: $(uname -s)" ;;
  esac
  case "$(uname -m)" in
    arm64|aarch64) arch="arm64" ;;
    x86_64|amd64) arch="amd64" ;;
    *) die "unsupported architecture: $(uname -m)" ;;
  esac
  printf '%s %s\n' "$os" "$arch"
}

read -r OS_NAME ARCH_NAME < <(platform)
ARCHIVE_NAME="sn-cli-${OS_NAME}-${ARCH_NAME}.tar.gz"
if [ -n "${SN_CLI_DOWNLOAD_BASE_URL:-}" ]; then
  DOWNLOAD_BASE="${SN_CLI_DOWNLOAD_BASE_URL%/}"
elif [ -n "$VERSION" ]; then
  DOWNLOAD_BASE="https://github.com/$REPOSITORY/releases/download/$VERSION"
else
  DOWNLOAD_BASE="https://github.com/$REPOSITORY/releases/latest/download"
fi

if [ "$DRY_RUN" = "1" ]; then
  log "sn-cli install dry-run"
  log "home: $SN_CLI_HOME"
  log "binary: $SN_CLI_HOME/bin/sn-cli"
  log "configs: $SN_CLI_HOME/configs"
  log "resources: $SN_CLI_HOME/resources"
  log "overwrite configs: $OVERWRITE_CONFIGS"
  log "symlink: $INSTALL_DIR/sn-cli"
  if [ -n "$LOCAL_BINARY" ]; then
    log "source binary: $LOCAL_BINARY"
    log "source configs: $LOCAL_CONFIGS"
    log "source resources: $LOCAL_RESOURCES"
  elif [ -n "$LOCAL_ARCHIVE" ]; then
    log "source archive: $LOCAL_ARCHIVE"
  else
    log "archive url: $DOWNLOAD_BASE/$ARCHIVE_NAME"
    log "checksums url: $DOWNLOAD_BASE/checksums.txt"
  fi
  exit 0
fi

if [ -n "$LOCAL_BINARY" ] && [ -n "$LOCAL_ARCHIVE" ]; then
  die "--binary and --archive are mutually exclusive"
fi
if [ -n "$LOCAL_BINARY" ] && { [ -z "$LOCAL_CONFIGS" ] || [ -z "$LOCAL_RESOURCES" ]; }; then
  die "--binary requires --configs and --resources"
fi

mkdir -p "$SN_CLI_HOME/tmp"
chmod 700 "$SN_CLI_HOME" "$SN_CLI_HOME/tmp"
WORK_DIR="$(mktemp -d "$SN_CLI_HOME/tmp/install.XXXXXX")"
trap 'rm -rf "$WORK_DIR"' EXIT

sha256_file() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    require_command shasum
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

verify_checksum() {
  local archive="$1" checksums="$2" expected actual
  expected="$(awk -v name="$(basename "$archive")" '$2 == name || $2 == "*" name {print $1; exit}' "$checksums")"
  [ -n "$expected" ] || die "checksum not found for $(basename "$archive")"
  actual="$(sha256_file "$archive")"
  [ "$actual" = "$expected" ] || die "checksum mismatch for $(basename "$archive")"
}

validate_archive_paths() {
  local archive="$1" entry entry_type
  while IFS= read -r entry; do
    case "$entry" in
      /*|..|../*|*/../*) die "unsafe archive path: $entry" ;;
    esac
  done < <(tar -tzf "$archive")
  while IFS= read -r entry; do
    entry_type="${entry:0:1}"
    case "$entry_type" in
      -|d) ;;
      *) die "archive contains unsupported entry: $entry" ;;
    esac
  done < <(tar -tvzf "$archive")
}

preflight_sync() {
  local source="$1" target="$2" path relative destination
  [ -d "$source" ] || die "sync source is not a directory: $source"
  if find "$source" -type l -print -quit | grep -q .; then
    die "sync source contains symlink: $source"
  fi
  if [ -e "$target" ] || [ -L "$target" ]; then
    [ -d "$target" ] && [ ! -L "$target" ] || die "sync type conflict at $target"
  fi
  while IFS= read -r -d '' path; do
    relative="${path#"$source"/}"
    [ "$relative" != "$path" ] || continue
    destination="$target/$relative"
    if [ -L "$destination" ]; then
      die "sync type conflict at $destination"
    fi
    if [ -d "$path" ]; then
      if [ -e "$destination" ] && [ ! -d "$destination" ]; then
        die "sync type conflict at $destination"
      fi
    elif [ -f "$path" ]; then
      if [ -e "$destination" ] && [ ! -f "$destination" ]; then
        die "sync type conflict at $destination"
      fi
    else
      die "sync source contains unsupported file: $path"
    fi
  done < <(find "$source" -mindepth 1 -print0)
}

sync_missing() {
  local source="$1" target="$2" quiet="${3:-0}" kind="${4:-file}" path relative destination
  preflight_sync "$source" "$target"
  mkdir -p "$target"
  chmod 700 "$target"
  while IFS= read -r -d '' path; do
    relative="${path#"$source"/}"
    destination="$target/$relative"
    if [ -d "$path" ]; then
      if [ ! -e "$destination" ]; then
        mkdir -p "$destination"
        chmod 700 "$destination"
      fi
    elif [ ! -e "$destination" ] && [ ! -L "$destination" ]; then
      mkdir -p "$(dirname "$destination")"
      cp "$path" "$destination"
      if [ "$quiet" != "1" ]; then
        log "installed $kind: $relative"
      fi
    fi
  done < <(find "$source" -mindepth 1 -print0)
}

sync_overwrite() {
  local source="$1" target="$2" quiet="${3:-0}" kind="${4:-file}" path relative destination temporary action
  preflight_sync "$source" "$target"
  mkdir -p "$target"
  chmod 700 "$target"
  while IFS= read -r -d '' path; do
    relative="${path#"$source"/}"
    destination="$target/$relative"
    if [ -d "$path" ]; then
      if [ ! -e "$destination" ]; then
        mkdir -p "$destination"
        chmod 700 "$destination"
      fi
    elif [ -f "$path" ]; then
      mkdir -p "$(dirname "$destination")"
      if [ -e "$destination" ]; then
        action="updated"
      else
        action="installed"
      fi
      temporary="$(mktemp "${destination}.install.XXXXXX")"
      cp "$path" "$temporary"
      mv -f "$temporary" "$destination"
      if [ "$quiet" != "1" ]; then
        log "$action $kind: $relative"
      fi
    fi
  done < <(find "$source" -mindepth 1 -print0)
}

PAYLOAD="$WORK_DIR/payload"
mkdir -p "$PAYLOAD"
if [ -n "$LOCAL_BINARY" ]; then
  [ -x "$LOCAL_BINARY" ] || die "local binary is not executable: $LOCAL_BINARY"
  [ -d "$LOCAL_CONFIGS" ] || die "local configs not found: $LOCAL_CONFIGS"
  [ -d "$LOCAL_RESOURCES" ] || die "local resources not found: $LOCAL_RESOURCES"
  PACKAGE_BINARY="$LOCAL_BINARY"
  PACKAGE_CONFIGS="$LOCAL_CONFIGS"
  PACKAGE_RESOURCES="$LOCAL_RESOURCES"
else
  ARCHIVE_PATH="$LOCAL_ARCHIVE"
  CHECKSUMS_PATH="$LOCAL_CHECKSUMS"
  if [ -z "$ARCHIVE_PATH" ]; then
    require_command curl
    ARCHIVE_PATH="$WORK_DIR/$ARCHIVE_NAME"
    CHECKSUMS_PATH="$WORK_DIR/checksums.txt"
    log "downloading $DOWNLOAD_BASE/$ARCHIVE_NAME"
    curl -fL --retry 3 --connect-timeout 15 -o "$ARCHIVE_PATH" "$DOWNLOAD_BASE/$ARCHIVE_NAME"
    curl -fL --retry 3 --connect-timeout 15 -o "$CHECKSUMS_PATH" "$DOWNLOAD_BASE/checksums.txt"
  fi
  [ -f "$ARCHIVE_PATH" ] || die "archive not found: $ARCHIVE_PATH"
  if [ -n "$CHECKSUMS_PATH" ]; then
    [ -f "$CHECKSUMS_PATH" ] || die "checksums not found: $CHECKSUMS_PATH"
    verify_checksum "$ARCHIVE_PATH" "$CHECKSUMS_PATH"
  elif [ -z "$LOCAL_ARCHIVE" ]; then
    die "network installs require checksums"
  fi
  validate_archive_paths "$ARCHIVE_PATH"
  tar -xzf "$ARCHIVE_PATH" -C "$PAYLOAD"
  PACKAGE_BINARY="$PAYLOAD/sn-cli"
  PACKAGE_CONFIGS="$PAYLOAD/configs"
  PACKAGE_RESOURCES="$PAYLOAD/resources"
fi

[ -x "$PACKAGE_BINARY" ] || die "package has no executable sn-cli"
[ -d "$PACKAGE_CONFIGS" ] || die "package has no configs directory"
[ -d "$PACKAGE_RESOURCES" ] || die "package has no resources directory"

TARGET_BINARY="$SN_CLI_HOME/bin/sn-cli"
if [ -e "$TARGET_BINARY" ] || [ -L "$TARGET_BINARY" ]; then
  [ -f "$TARGET_BINARY" ] && [ ! -L "$TARGET_BINARY" ] || die "binary target is not a regular file: $TARGET_BINARY"
fi

MERGED_HOME="$WORK_DIR/merged-home"
mkdir -p "$MERGED_HOME/configs" "$MERGED_HOME/resources"
preflight_sync "$PACKAGE_CONFIGS" "$SN_CLI_HOME/configs"
preflight_sync "$PACKAGE_RESOURCES" "$SN_CLI_HOME/resources"
if [ -d "$SN_CLI_HOME/configs" ]; then
  sync_missing "$SN_CLI_HOME/configs" "$MERGED_HOME/configs" 1
fi
if [ -d "$SN_CLI_HOME/resources" ]; then
  sync_missing "$SN_CLI_HOME/resources" "$MERGED_HOME/resources" 1
fi
SN_CLI_HOME="$MERGED_HOME" "$PACKAGE_BINARY" system migrate-config >/dev/null || \
  die "new sn-cli failed runtime-home migration"
if [ "$OVERWRITE_CONFIGS" = "1" ]; then
  sync_overwrite "$PACKAGE_CONFIGS" "$MERGED_HOME/configs" 1 config
else
  sync_missing "$PACKAGE_CONFIGS" "$MERGED_HOME/configs" 1
fi
sync_missing "$PACKAGE_RESOURCES" "$MERGED_HOME/resources" 1
SN_CLI_HOME="$MERGED_HOME" "$PACKAGE_BINARY" profile list >/dev/null || die "new sn-cli failed config validation"

if [ -d "$SN_CLI_HOME/configs" ]; then
  migration_output="$(SN_CLI_HOME="$SN_CLI_HOME" "$PACKAGE_BINARY" system migrate-config)" || \
    die "new sn-cli failed runtime-home migration"
else
  migration_output='{"changed_configs":[],"copied_resources":[]}'
fi
if ! printf '%s\n' "$migration_output" | grep -Eq '"changed_configs"[[:space:]]*:[[:space:]]*\[[[:space:]]*\]' || \
   ! printf '%s\n' "$migration_output" | grep -Eq '"copied_resources"[[:space:]]*:[[:space:]]*\[[[:space:]]*\]'; then
  log "migrated legacy runtime home"
fi
if [ "$OVERWRITE_CONFIGS" = "1" ]; then
  sync_overwrite "$PACKAGE_CONFIGS" "$SN_CLI_HOME/configs" 0 config
else
  sync_missing "$PACKAGE_CONFIGS" "$SN_CLI_HOME/configs" 0 config
fi
sync_missing "$PACKAGE_RESOURCES" "$SN_CLI_HOME/resources" 0 resource
for directory in \
  "$SN_CLI_HOME/configs" "$SN_CLI_HOME/resources" "$SN_CLI_HOME/resources/personas" \
  "$SN_CLI_HOME/resources/skills" "$SN_CLI_HOME/resources/tools" "$SN_CLI_HOME/resources/schema" \
  "$SN_CLI_HOME/bin" "$SN_CLI_HOME/runs" "$SN_CLI_HOME/daemon" "$SN_CLI_HOME/state" \
  "$SN_CLI_HOME/logs" "$SN_CLI_HOME/cache"; do
  if [ -e "$directory" ] || [ -L "$directory" ]; then
    [ -d "$directory" ] && [ ! -L "$directory" ] || die "runtime directory type conflict at $directory"
  else
    mkdir -p "$directory"
  fi
  chmod 700 "$directory"
done
mkdir -p "$INSTALL_DIR"
NEW_BINARY="$SN_CLI_HOME/bin/.sn-cli.new.$$"
cp "$PACKAGE_BINARY" "$NEW_BINARY"
chmod 755 "$NEW_BINARY"
mv -f "$NEW_BINARY" "$TARGET_BINARY"

LINK="$INSTALL_DIR/sn-cli"
if [ -d "$LINK" ] && [ ! -L "$LINK" ]; then
  die "symlink target is a directory: $LINK"
fi
NEW_LINK="$INSTALL_DIR/.sn-cli.link.$$"
ln -s "$TARGET_BINARY" "$NEW_LINK"
mv -f "$NEW_LINK" "$LINK"

log "installed binary: $TARGET_BINARY"
log "installed command: $LINK"
if [ -d "$HOME/.sn-cli/runtime" ]; then
  log "note: the legacy source checkout was preserved but is no longer used"
fi
case ":$PATH:" in
  *":$INSTALL_DIR:"*) ;;
  *) log "PATH hint: add $INSTALL_DIR to PATH" ;;
esac
