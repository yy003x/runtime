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
LOCAL_SOURCE_INSTALL=0
DRY_RUN=0
VERSION_OPTION_SET=0

usage() {
  cat <<'EOF'
Install sn-cli from a GitHub Release without downloading source code.

Usage:
  curl -fsSL https://raw.githubusercontent.com/yy003x/runtime/main/install.sh | bash
  bash install.sh [--version VERSION] [--dry-run]

Local package options used by `make install`:
  --binary FILE --server FILE --configs DIR --commands DIR
  --runtime-config FILE --resources DIR
  [--overwrite-configs | --local-source-install]
  --archive FILE [--checksums FILE]

Options:
  --version VERSION    Install a specific release tag; default is latest.
  --install-dir DIR    Symlink directory; default is ~/.local/bin.
  --home DIR           Runtime home; default is ~/.sn.
  --overwrite-configs  Replace profiles, subcommands, and runtime.json.
  --local-source-install
                       Destructive source mode used only by `make install`.
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

SEEN_OPTIONS="|"
mark_option_once() {
  local name="$1"
  case "$SEEN_OPTIONS" in
    *"|$name|"*) die "$name may only be specified once" ;;
  esac
  SEEN_OPTIONS="${SEEN_OPTIONS}${name}|"
}

require_option_value() {
  local name="$1" count="$2" value="${3:-}"
  [ "$count" -ge 2 ] || die "$name requires a value"
  [ -n "$value" ] || die "$name requires a non-empty value"
  case "$value" in
    -*) die "$name requires a value before the next option" ;;
  esac
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --version|--install-dir|--home|--archive|--checksums|--binary|--server|--configs|--commands|--runtime-config|--resources)
      option="$1"
      mark_option_once "$option"
      require_option_value "$option" "$#" "${2:-}"
      case "$option" in
        --version) VERSION="$2"; VERSION_OPTION_SET=1 ;;
        --install-dir) INSTALL_DIR="$2" ;;
        --home) SN_CLI_HOME="$2" ;;
        --archive) LOCAL_ARCHIVE="$2" ;;
        --checksums) LOCAL_CHECKSUMS="$2" ;;
        --binary) LOCAL_BINARY="$2" ;;
        --server) LOCAL_SERVER="$2" ;;
        --configs) LOCAL_CONFIGS="$2" ;;
        --commands) LOCAL_COMMANDS="$2" ;;
        --runtime-config) LOCAL_RUNTIME_CONFIG="$2" ;;
        --resources) LOCAL_RESOURCES="$2" ;;
      esac
      shift 2
      ;;
    --overwrite-configs)
      mark_option_once "$1"
      OVERWRITE_CONFIGS=1
      shift
      ;;
    --local-source-install)
      mark_option_once "$1"
      LOCAL_SOURCE_INSTALL=1
      shift
      ;;
    --dry-run)
      mark_option_once "$1"
      DRY_RUN=1
      shift
      ;;
    -h|--help) usage; exit 0 ;;
    *) die "unknown option: $1" ;;
  esac
done

if [ "$LOCAL_SOURCE_INSTALL" = "1" ]; then
  [ -n "$LOCAL_BINARY" ] ||
    die "--local-source-install requires the complete --binary source bundle"
  [ -z "$LOCAL_ARCHIVE" ] && [ -z "$LOCAL_CHECKSUMS" ] &&
    [ "$VERSION_OPTION_SET" = "0" ] ||
    die "--local-source-install cannot be combined with --archive, --checksums, or --version"
  [ "$OVERWRITE_CONFIGS" = "0" ] ||
    die "--local-source-install cannot be combined with --overwrite-configs"
fi

if [ -n "$LOCAL_BINARY" ]; then
  [ -z "$LOCAL_ARCHIVE" ] && [ -z "$LOCAL_CHECKSUMS" ] &&
    [ "$VERSION_OPTION_SET" = "0" ] ||
    die "--binary cannot be combined with --archive, --checksums, or --version"
  [ -n "$LOCAL_SERVER" ] && [ -n "$LOCAL_CONFIGS" ] &&
    [ -n "$LOCAL_COMMANDS" ] && [ -n "$LOCAL_RUNTIME_CONFIG" ] &&
    [ -n "$LOCAL_RESOURCES" ] ||
    die "--binary requires --server, --configs, --commands, --runtime-config, and --resources"
elif [ -n "$LOCAL_ARCHIVE" ]; then
  [ -z "$LOCAL_SERVER" ] && [ -z "$LOCAL_CONFIGS" ] &&
    [ -z "$LOCAL_COMMANDS" ] && [ -z "$LOCAL_RUNTIME_CONFIG" ] &&
    [ -z "$LOCAL_RESOURCES" ] && [ "$VERSION_OPTION_SET" = "0" ] ||
    die "--archive cannot be combined with source package options or --version"
else
  [ -z "$LOCAL_CHECKSUMS" ] && [ -z "$LOCAL_SERVER" ] &&
    [ -z "$LOCAL_CONFIGS" ] && [ -z "$LOCAL_COMMANDS" ] &&
    [ -z "$LOCAL_RUNTIME_CONFIG" ] && [ -z "$LOCAL_RESOURCES" ] ||
    die "local package options require --binary or --archive"
fi

if [ "$LOCAL_SOURCE_INSTALL" = "1" ]; then
  OVERWRITE_CONFIGS=1
fi

normalize_absolute_path() {
  local value="$1" component
  local -a input_parts output_parts
  case "$value" in
    /*) ;;
    *) value="$PWD/$value" ;;
  esac
  IFS='/' read -r -a input_parts <<< "$value"
  output_parts=()
  for component in "${input_parts[@]}"; do
    case "$component" in
      ""|.) ;;
      ..)
        if [ "${#output_parts[@]}" -gt 0 ]; then
          unset 'output_parts[${#output_parts[@]}-1]'
        fi
        ;;
      *) output_parts+=("$component") ;;
    esac
  done
  if [ "${#output_parts[@]}" -eq 0 ]; then
    printf '/\n'
  else
    local joined
    joined="$(IFS=/; printf '%s' "${output_parts[*]}")"
    printf '/%s\n' "$joined"
  fi
}

canonicalize_directory_path() {
  local value current parent base component index
  local -a suffix
  value="$(normalize_absolute_path "$1")"
  [ ! -L "$value" ] || die "directory path must not be a symlink: $value"
  if [ -e "$value" ]; then
    [ -d "$value" ] || die "directory path is not a directory: $value"
    (cd "$value" && pwd -P)
    return
  fi
  current="$value"
  suffix=()
  while [ ! -e "$current" ] && [ ! -L "$current" ]; do
    parent="$(dirname "$current")"
    [ "$parent" != "$current" ] ||
      die "directory path has no existing ancestor: $value"
    component="$(basename "$current")"
    if printf '%s' "$component" |
      LC_ALL=C grep -q '[^ -~]'; then
      die "missing directory path components must use printable ASCII: $value"
    fi
    suffix+=("$component")
    current="$parent"
  done
  [ -d "$current" ] ||
    die "directory path parent is not a directory: $current"
  base="$(cd "$current" && pwd -P)"
  for ((index=${#suffix[@]}-1; index>=0; index--)); do
    component="${suffix[$index]}"
    base="$base/$component"
  done
  normalize_absolute_path "$base"
}

require_install_dir_outside_home() {
  local home_guard install_guard
  home_guard="$(
    printf '%s' "$1" |
      LC_ALL=C tr 'ABCDEFGHIJKLMNOPQRSTUVWXYZ' 'abcdefghijklmnopqrstuvwxyz'
  )"
  install_guard="$(
    printf '%s' "$2" |
      LC_ALL=C tr 'ABCDEFGHIJKLMNOPQRSTUVWXYZ' 'abcdefghijklmnopqrstuvwxyz'
  )"
  case "$install_guard" in
    "$home_guard"|"$home_guard"/*)
      die "install directory must be outside the Runtime home"
      ;;
  esac
}

[ -n "$SN_CLI_HOME" ] || die "runtime home must not be empty"
[ -n "$INSTALL_DIR" ] || die "install directory must not be empty"
SN_CLI_HOME="$(canonicalize_directory_path "$SN_CLI_HOME")"
INSTALL_DIR="$(canonicalize_directory_path "$INSTALL_DIR")"
[ "$SN_CLI_HOME" != "/" ] || die "runtime home must not be /"
[ "$INSTALL_DIR" != "/" ] || die "install directory must not be /"
require_install_dir_outside_home "$SN_CLI_HOME" "$INSTALL_DIR"

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
  log "local source install: $LOCAL_SOURCE_INSTALL"
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

WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/sn-cli-install.XXXXXX")"
cleanup() {
  rm -rf "$WORK_DIR"
}
trap cleanup EXIT

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

PAYLOAD="$WORK_DIR/payload"
mkdir -p "$PAYLOAD"
if [ -n "$LOCAL_BINARY" ]; then
  [ -x "$LOCAL_BINARY" ] || die "local binary is not executable: $LOCAL_BINARY"
  [ -x "$LOCAL_SERVER" ] || die "local server is not executable: $LOCAL_SERVER"
  [ -d "$LOCAL_CONFIGS" ] || die "local configs not found: $LOCAL_CONFIGS"
  [ -d "$LOCAL_COMMANDS" ] || die "local commands not found: $LOCAL_COMMANDS"
  [ -f "$LOCAL_RUNTIME_CONFIG" ] && [ ! -L "$LOCAL_RUNTIME_CONFIG" ] || die "local runtime config not found: $LOCAL_RUNTIME_CONFIG"
  [ -d "$LOCAL_RESOURCES" ] || die "local resources not found: $LOCAL_RESOURCES"
  cp "$LOCAL_BINARY" "$PAYLOAD/sn-cli"
  chmod 755 "$PAYLOAD/sn-cli"
  cp "$LOCAL_SERVER" "$PAYLOAD/sn-server"
  chmod 755 "$PAYLOAD/sn-server"
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
  cp -R "$LOCAL_COMMANDS" "$PAYLOAD/commands"
  cp "$LOCAL_RUNTIME_CONFIG" "$PAYLOAD/runtime.json"
  cp -R "$LOCAL_RESOURCES" "$PAYLOAD/resources"
  PACKAGE_BINARY="$PAYLOAD/sn-cli"
  PACKAGE_SERVER="$PAYLOAD/sn-server"
  PACKAGE_COMMANDS="$PAYLOAD/commands"
  PACKAGE_RUNTIME_CONFIG="$PAYLOAD/runtime.json"
  PACKAGE_RESOURCES="$PAYLOAD/resources"
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

if [ -e "$SN_CLI_HOME" ] || [ -L "$SN_CLI_HOME" ]; then
  [ -d "$SN_CLI_HOME" ] && [ ! -L "$SN_CLI_HOME" ] ||
    die "runtime home must be a directory, not a symlink: $SN_CLI_HOME"
  SN_CLI_HOME="$(cd "$SN_CLI_HOME" && pwd -P)"
fi
mkdir -p "$INSTALL_DIR"
[ -d "$INSTALL_DIR" ] && [ ! -L "$INSTALL_DIR" ] ||
  die "install directory must be a directory, not a symlink: $INSTALL_DIR"
INSTALL_DIR="$(cd "$INSTALL_DIR" && pwd -P)"
require_install_dir_outside_home "$SN_CLI_HOME" "$INSTALL_DIR"

LINK="$INSTALL_DIR/sn-cli"
if [ -e "$LINK" ] && [ ! -L "$LINK" ]; then
  die "install command target already exists and is not a symlink: $LINK"
fi
if [ -L "$LINK" ]; then
  [ "$(readlink "$LINK")" = "$SN_CLI_HOME/bin/sn-cli" ] ||
    die "install command symlink points outside this Runtime home: $LINK"
fi

TARGET_BINARY="$SN_CLI_HOME/bin/sn-cli"
TARGET_SERVER="$SN_CLI_HOME/bin/sn-server"
for target in "$TARGET_BINARY" "$TARGET_SERVER"; do
  if [ -e "$target" ] || [ -L "$target" ]; then
    [ -f "$target" ] && [ ! -L "$target" ] || die "binary target is not a regular file: $target"
  fi
done
activation_args=(
  server upgrade-activate
  --payload "$PAYLOAD"
  --target-home "$SN_CLI_HOME"
  --command-link "$LINK"
)
if [ "$OVERWRITE_CONFIGS" = "1" ] &&
  [ "$LOCAL_SOURCE_INSTALL" = "0" ]; then
  activation_args+=(--overwrite-configs)
fi
if [ "$LOCAL_SOURCE_INSTALL" = "1" ]; then
  activation_args+=(--local-source-install)
fi
SN_CLI_HOME="$SN_CLI_HOME" "$PACKAGE_BINARY" "${activation_args[@]}" ||
  die "candidate activation or no-clobber command-link step failed"

log "installed binary: $TARGET_BINARY"
log "installed server: $TARGET_SERVER"
log "installed command: $LINK"
case ":$PATH:" in
  *":$INSTALL_DIR:"*) ;;
  *) log "PATH hint: add $INSTALL_DIR to PATH" ;;
esac
