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
LOCAL_SERVER=""
LOCAL_CONFIGS=""
LOCAL_COMMANDS=""
LOCAL_RUNTIME_CONFIG=""
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
  --binary FILE --server FILE --configs DIR --commands DIR
  --runtime-config FILE --resources DIR [--overwrite-configs]
  --archive FILE [--checksums FILE]

Options:
  --version VERSION    Install a specific release tag; default is latest.
  --install-dir DIR    Symlink directory; default is ~/.local/bin.
  --home DIR           Runtime home; default is ~/.sn.
  --overwrite-configs  Replace profiles, subcommands, and runtime.json.
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
    --server) [ "$#" -ge 2 ] || die "--server requires a value"; LOCAL_SERVER="$2"; shift 2 ;;
    --configs) [ "$#" -ge 2 ] || die "--configs requires a value"; LOCAL_CONFIGS="$2"; shift 2 ;;
    --commands) [ "$#" -ge 2 ] || die "--commands requires a value"; LOCAL_COMMANDS="$2"; shift 2 ;;
    --runtime-config) [ "$#" -ge 2 ] || die "--runtime-config requires a value"; LOCAL_RUNTIME_CONFIG="$2"; shift 2 ;;
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
  log "server: $SN_CLI_HOME/bin/sn-server"
  log "profiles: $SN_CLI_HOME/configs"
  log "commands: $SN_CLI_HOME/commands"
  log "runtime config: $SN_CLI_HOME/runtime.json"
  log "resources: $SN_CLI_HOME/resources"
  log "overwrite configs: $OVERWRITE_CONFIGS"
  log "symlink: $INSTALL_DIR/sn-cli"
  if [ -n "$LOCAL_BINARY" ]; then
    log "source binary: $LOCAL_BINARY"
    log "source server: $LOCAL_SERVER"
    log "source profiles: $LOCAL_CONFIGS/*.json"
    log "source commands: $LOCAL_COMMANDS"
    log "source runtime config: $LOCAL_RUNTIME_CONFIG"
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
if [ -n "$LOCAL_BINARY" ] && { [ -z "$LOCAL_SERVER" ] || [ -z "$LOCAL_CONFIGS" ] || [ -z "$LOCAL_COMMANDS" ] || [ -z "$LOCAL_RUNTIME_CONFIG" ] || [ -z "$LOCAL_RESOURCES" ]; }; then
  die "--binary requires --server, --configs, --commands, --runtime-config, and --resources"
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

replace_directory() {
  local source="$1" target="$2" parent name previous
  [ -d "$source" ] && [ ! -L "$source" ] || die "replacement source is not a directory: $source"
  parent="$(dirname "$target")"
  name="$(basename "$target")"
  previous="$parent/.$name.previous.$$"
  mkdir -p "$parent"
  [ ! -e "$previous" ] && [ ! -L "$previous" ] || die "replacement staging path already exists: $previous"
  if [ -e "$target" ] || [ -L "$target" ]; then
    [ -d "$target" ] && [ ! -L "$target" ] || die "replacement target is not a directory: $target"
    mv "$target" "$previous"
  else
    previous=""
  fi
  if mv "$source" "$target"; then
    chmod 700 "$target"
    if [ -n "$previous" ]; then
      rm -rf "$previous"
    fi
    return 0
  fi
  if [ -n "$previous" ] && [ -d "$previous" ]; then
    mv "$previous" "$target"
  fi
  die "failed to replace directory: $target"
}

PAYLOAD="$WORK_DIR/payload"
mkdir -p "$PAYLOAD"
if [ -n "$LOCAL_BINARY" ]; then
  [ -x "$LOCAL_BINARY" ] || die "local binary is not executable: $LOCAL_BINARY"
  [ -x "$LOCAL_SERVER" ] || die "local server is not executable: $LOCAL_SERVER"
  [ -d "$LOCAL_CONFIGS" ] || die "local configs not found: $LOCAL_CONFIGS"
  [ -d "$LOCAL_COMMANDS" ] || die "local commands not found: $LOCAL_COMMANDS"
  [ -f "$LOCAL_RUNTIME_CONFIG" ] && [ ! -L "$LOCAL_RUNTIME_CONFIG" ] || die "local runtime config not found: $LOCAL_RUNTIME_CONFIG"
  [ -d "$LOCAL_RESOURCES" ] || die "local resources not found: $LOCAL_RESOURCES"
  PACKAGE_BINARY="$LOCAL_BINARY"
  PACKAGE_SERVER="$LOCAL_SERVER"
  PACKAGE_CONFIGS="$PAYLOAD/configs"
  mkdir -p "$PACKAGE_CONFIGS"
  profile_count=0
  for profile in "$LOCAL_CONFIGS"/*.json; do
    [ -f "$profile" ] && [ ! -L "$profile" ] ||
      die "local configs must contain only regular top-level JSON profiles"
    cp "$profile" "$PACKAGE_CONFIGS/"
    profile_count=$((profile_count + 1))
  done
  [ "$profile_count" -gt 0 ] || die "local configs contain no JSON profiles"
  PACKAGE_COMMANDS="$LOCAL_COMMANDS"
  PACKAGE_RUNTIME_CONFIG="$LOCAL_RUNTIME_CONFIG"
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
  PACKAGE_SERVER="$PAYLOAD/sn-server"
  PACKAGE_CONFIGS="$PAYLOAD/configs"
  PACKAGE_COMMANDS="$PAYLOAD/commands"
  PACKAGE_RUNTIME_CONFIG="$PAYLOAD/runtime.json"
  PACKAGE_RESOURCES="$PAYLOAD/resources"
fi

[ -x "$PACKAGE_BINARY" ] || die "package has no executable sn-cli"
[ -x "$PACKAGE_SERVER" ] || die "package has no executable sn-server"
[ -d "$PACKAGE_CONFIGS" ] || die "package has no configs directory"
[ -d "$PACKAGE_COMMANDS" ] || die "package has no commands directory"
[ -f "$PACKAGE_RUNTIME_CONFIG" ] && [ ! -L "$PACKAGE_RUNTIME_CONFIG" ] || die "package has no runtime.json"
[ -d "$PACKAGE_RESOURCES" ] || die "package has no resources directory"

TARGET_BINARY="$SN_CLI_HOME/bin/sn-cli"
TARGET_SERVER="$SN_CLI_HOME/bin/sn-server"
for target in "$TARGET_BINARY" "$TARGET_SERVER"; do
  if [ -e "$target" ] || [ -L "$target" ]; then
    [ -f "$target" ] && [ ! -L "$target" ] || die "binary target is not a regular file: $target"
  fi
done
if [ -e "$SN_CLI_HOME/runtime.json" ] || [ -L "$SN_CLI_HOME/runtime.json" ]; then
  [ -f "$SN_CLI_HOME/runtime.json" ] && [ ! -L "$SN_CLI_HOME/runtime.json" ] || die "runtime config target is not a regular file: $SN_CLI_HOME/runtime.json"
fi

MERGED_HOME="$WORK_DIR/merged-home"
mkdir -p "$MERGED_HOME/configs" "$MERGED_HOME/commands" "$MERGED_HOME/resources"
if [ "$OVERWRITE_CONFIGS" = "1" ]; then
  for target in "$SN_CLI_HOME/configs" "$SN_CLI_HOME/commands"; do
    if [ -e "$target" ] || [ -L "$target" ]; then
      [ -d "$target" ] && [ ! -L "$target" ] || die "sync type conflict at $target"
    fi
  done
else
  preflight_sync "$PACKAGE_CONFIGS" "$SN_CLI_HOME/configs"
  preflight_sync "$PACKAGE_COMMANDS" "$SN_CLI_HOME/commands"
fi
preflight_sync "$PACKAGE_RESOURCES" "$SN_CLI_HOME/resources"
if [ "$OVERWRITE_CONFIGS" = "0" ] && [ -d "$SN_CLI_HOME/configs" ]; then
  sync_missing "$SN_CLI_HOME/configs" "$MERGED_HOME/configs" 1
fi
if [ "$OVERWRITE_CONFIGS" = "0" ] && [ -d "$SN_CLI_HOME/commands" ]; then
  sync_missing "$SN_CLI_HOME/commands" "$MERGED_HOME/commands" 1
fi
if [ "$OVERWRITE_CONFIGS" = "1" ]; then
  sync_overwrite "$PACKAGE_CONFIGS" "$MERGED_HOME/configs" 1 profile
else
  sync_missing "$PACKAGE_CONFIGS" "$MERGED_HOME/configs" 1
fi
if [ "$OVERWRITE_CONFIGS" = "1" ]; then
  sync_overwrite "$PACKAGE_COMMANDS" "$MERGED_HOME/commands" 1 command
else
  sync_missing "$PACKAGE_COMMANDS" "$MERGED_HOME/commands" 1
fi
sync_overwrite "$PACKAGE_RESOURCES" "$MERGED_HOME/resources" 1 resource
if [ "$OVERWRITE_CONFIGS" = "0" ] && [ -f "$SN_CLI_HOME/runtime.json" ] && [ ! -L "$SN_CLI_HOME/runtime.json" ]; then
  cp "$SN_CLI_HOME/runtime.json" "$MERGED_HOME/runtime.json"
else
  cp "$PACKAGE_RUNTIME_CONFIG" "$MERGED_HOME/runtime.json"
fi
SN_CLI_HOME="$MERGED_HOME" "$PACKAGE_BINARY" profile check >/dev/null || die "new sn-cli failed profile validation"

if [ "$OVERWRITE_CONFIGS" = "1" ]; then
  replace_directory "$MERGED_HOME/configs" "$SN_CLI_HOME/configs"
  log "replaced profiles: $SN_CLI_HOME/configs"
  replace_directory "$MERGED_HOME/commands" "$SN_CLI_HOME/commands"
  log "replaced commands: $SN_CLI_HOME/commands"
else
  sync_missing "$PACKAGE_CONFIGS" "$SN_CLI_HOME/configs" 0 profile
  sync_missing "$PACKAGE_COMMANDS" "$SN_CLI_HOME/commands" 0 command
fi
if [ "$OVERWRITE_CONFIGS" = "1" ] || [ ! -e "$SN_CLI_HOME/runtime.json" ]; then
  runtime_temp="$(mktemp "$SN_CLI_HOME/.runtime.json.install.XXXXXX")"
  cp "$PACKAGE_RUNTIME_CONFIG" "$runtime_temp"
  chmod 600 "$runtime_temp"
  mv -f "$runtime_temp" "$SN_CLI_HOME/runtime.json"
  log "installed runtime config: $SN_CLI_HOME/runtime.json"
elif [ -L "$SN_CLI_HOME/runtime.json" ] || [ ! -f "$SN_CLI_HOME/runtime.json" ]; then
  die "runtime config target is not a regular file: $SN_CLI_HOME/runtime.json"
fi
replace_directory "$MERGED_HOME/resources" "$SN_CLI_HOME/resources"
log "replaced managed resources: $SN_CLI_HOME/resources"
for directory in \
  "$SN_CLI_HOME/configs" "$SN_CLI_HOME/commands" "$SN_CLI_HOME/resources" \
  "$SN_CLI_HOME/resources/schema" "$SN_CLI_HOME/bin" "$SN_CLI_HOME/sessions" \
  "$SN_CLI_HOME/state" "$SN_CLI_HOME/tmp"; do
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
NEW_SERVER="$SN_CLI_HOME/bin/.sn-server.new.$$"
cp "$PACKAGE_SERVER" "$NEW_SERVER"
chmod 755 "$NEW_SERVER"
mv -f "$NEW_SERVER" "$TARGET_SERVER"

LINK="$INSTALL_DIR/sn-cli"
if [ -d "$LINK" ] && [ ! -L "$LINK" ]; then
  die "symlink target is a directory: $LINK"
fi
NEW_LINK="$INSTALL_DIR/.sn-cli.link.$$"
ln -s "$TARGET_BINARY" "$NEW_LINK"
mv -f "$NEW_LINK" "$LINK"

log "installed binary: $TARGET_BINARY"
log "installed server: $TARGET_SERVER"
log "installed command: $LINK"
case ":$PATH:" in
  *":$INSTALL_DIR:"*) ;;
  *) log "PATH hint: add $INSTALL_DIR to PATH" ;;
esac
