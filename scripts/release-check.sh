#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DIST_DIR="$ROOT_DIR/dist"
RELEASE_VERSION="${SN_CLI_VERSION:-release-check}"
GO_BIN="${GO:-go}"

log() { printf '%s\n' "$*" >&2; }
die() { printf 'release-check: %s\n' "$*" >&2; exit 1; }
run_make() {
  make --no-print-directory -C "$ROOT_DIR" V="${V:-0}" "$@"
}
replay_logs() {
  local file
  for file in "$@"; do
    if [ -s "$file" ]; then
      log "[release-check] output: $file"
      sed -n '1,400p' "$file" >&2
    fi
  done
}

if [[ ! "$RELEASE_VERSION" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$ ]]; then
  die "SN_CLI_VERSION must be a SemVer Git tag such as v0.1.0: $RELEASE_VERSION"
fi

log "[release-check] validating source"
[ -f "$ROOT_DIR/configs/runtime/runtime.json" ] || die "missing configs/runtime/runtime.json"
required_profiles=(api-cc.json api-cx.json cc-bai.json cc.json commit.json cx-adv.json cx-deep.json cx-image.json cx-spark.json cx.json)
for profile in "${required_profiles[@]}"; do
  [ -f "$ROOT_DIR/configs/$profile" ] || die "missing profile: $profile"
done
required_commands=(cc-bai.json cc.json commit.json cx-adv.json cx-deep.json cx-image.json cx-spark.json cx.json)
for command in "${required_commands[@]}"; do
  [ -f "$ROOT_DIR/configs/commands/$command" ] || die "missing subcommand: $command"
done
unexpected_config_entries="$(find "$ROOT_DIR/configs" -mindepth 1 -maxdepth 1 \
  ! -name commands ! -name runtime ! -name '*.json' -print -quit)"
[ -z "$unexpected_config_entries" ] || die "unexpected configs entry: $unexpected_config_entries"
for directory in commands; do
  unexpected="$(find "$ROOT_DIR/configs/$directory" -mindepth 1 -maxdepth 1 \
    \( ! -type f -o ! -name '*.json' \) -print -quit)"
  [ -z "$unexpected" ] || die "configs/$directory only accepts JSON files: $unexpected"
done
for schema in profile.schema.json subcommand.schema.json runtime.schema.json; do
  [ -f "$ROOT_DIR/resources/schema/$schema" ] || die "missing resource schema: $schema"
done

run_make fmt-check
run_make test-serial
run_make test-race
env GOCACHE="${GOCACHE:-$("$GO_BIN" env GOCACHE)}" \
  GOMODCACHE="${GOMODCACHE:-$("$GO_BIN" env GOMODCACHE)}" \
  "$GO_BIN" -C "$ROOT_DIR" vet ./...
bash "$ROOT_DIR/scripts/make-step-test.sh"

log "[release-check] building assets version=$RELEASE_VERSION"
run_make release-assets SN_CLI_VERSION="$RELEASE_VERSION"

expected_assets=(checksums.txt)
for platform in darwin-arm64 darwin-amd64 linux-arm64 linux-amd64; do
  expected_assets+=("sn-cli-$platform.tar.gz")
done
for asset in "${expected_assets[@]}"; do
  [ -f "$DIST_DIR/$asset" ] || die "missing release asset: $asset"
done
for asset in "${expected_assets[@]:1}"; do
  awk -v name="$asset" '$2 == name || $2 == "*" name {found=1} END {exit !found}' \
    "$DIST_DIR/checksums.txt" || die "checksum missing for $asset"
done
checksum_log="$(mktemp)"
if command -v sha256sum >/dev/null 2>&1; then
  if ! (cd "$DIST_DIR" && sha256sum --check checksums.txt) >"$checksum_log" 2>&1; then
    replay_logs "$checksum_log"
    rm -f "$checksum_log"
    die "release asset checksum verification failed"
  fi
else
  if ! (cd "$DIST_DIR" && shasum -a 256 --check checksums.txt) >"$checksum_log" 2>&1; then
    replay_logs "$checksum_log"
    rm -f "$checksum_log"
    die "release asset checksum verification failed"
  fi
fi
rm -f "$checksum_log"

case "$(uname -s)" in
  Darwin) os_name="darwin" ;;
  Linux) os_name="linux" ;;
  *) die "unsupported smoke-test operating system: $(uname -s)" ;;
esac
case "$(uname -m)" in
  arm64|aarch64) arch_name="arm64" ;;
  x86_64|amd64) arch_name="amd64" ;;
  *) die "unsupported smoke-test architecture: $(uname -m)" ;;
esac

temp_root="$(mktemp -d)"
runtime_home="$temp_root/home"
install_dir="$temp_root/bin"
archive="$DIST_DIR/sn-cli-$os_name-$arch_name.tar.gz"
cleanup() {
  if [ -x "$install_dir/sn-cli" ]; then
    SN_CLI_HOME="$runtime_home" "$install_dir/sn-cli" server stop >/dev/null 2>&1 || true
  fi
  rm -rf "$temp_root"
}
trap cleanup EXIT

log "[release-check] installing and exercising $archive"
mkdir -p "$runtime_home/configs" "$runtime_home/commands" "$runtime_home/resources"
printf '%s\n' '{"type":"cli","binary":"/bin/true","transport":"tty","prompt_delivery":"manual"}' \
  >"$runtime_home/configs/local-only.json"
printf '%s\n' '{"profile":"local-only"}' >"$runtime_home/commands/local-only.json"
printf '%s\n' '{"type":"cli","binary":"/bin/true","transport":"tty","prompt_delivery":"manual"}' \
  >"$runtime_home/configs/cx.json"
printf '%s\n' '{"profile":"local-only"}' >"$runtime_home/commands/cx.json"
printf '%s\n' '{"terminal":{"driver":"iterm2"}}' >"$runtime_home/runtime.json"
mkdir -p "$runtime_home/resources/schema"
printf '%s\n' 'outdated schema' >"$runtime_home/resources/schema/runtime.schema.json"
bash "$ROOT_DIR/install.sh" \
  --archive "$archive" \
  --checksums "$DIST_DIR/checksums.txt" \
  --home "$runtime_home" \
  --install-dir "$install_dir"

grep -q '"/bin/true"' "$runtime_home/configs/local-only.json" ||
  die "default install overwrote an existing profile"
grep -q '"local-only"' "$runtime_home/commands/local-only.json" ||
  die "default install overwrote an existing subcommand"
grep -q '"/bin/true"' "$runtime_home/configs/cx.json" ||
  die "default install overwrote an existing same-name profile"
grep -q '"local-only"' "$runtime_home/commands/cx.json" ||
  die "default install overwrote an existing same-name subcommand"
grep -q '"iterm2"' "$runtime_home/runtime.json" ||
  die "default install overwrote runtime.json"
cmp "$ROOT_DIR/resources/schema/runtime.schema.json" \
  "$runtime_home/resources/schema/runtime.schema.json" >/dev/null ||
  die "default install did not refresh managed resources"
[ -x "$runtime_home/bin/sn-cli" ] || die "sn-cli was not installed"
[ -x "$runtime_home/bin/sn-server" ] || die "sn-server was not installed"

bash "$ROOT_DIR/install.sh" \
  --archive "$archive" \
  --checksums "$DIST_DIR/checksums.txt" \
  --home "$runtime_home" \
  --install-dir "$install_dir" \
  --overwrite-configs

[ ! -e "$runtime_home/commands/local-only.json" ] ||
  die "--overwrite-configs kept a local-only subcommand"
[ ! -e "$runtime_home/configs/local-only.json" ] ||
  die "--overwrite-configs kept a local-only profile"
cmp "$ROOT_DIR/configs/cx.json" "$runtime_home/configs/cx.json" >/dev/null ||
  die "--overwrite-configs did not replace a same-name profile"
cmp "$ROOT_DIR/configs/commands/cx.json" "$runtime_home/commands/cx.json" >/dev/null ||
  die "--overwrite-configs did not replace a same-name subcommand"
cmp "$ROOT_DIR/configs/runtime/runtime.json" "$runtime_home/runtime.json" >/dev/null ||
  die "--overwrite-configs did not replace runtime.json"
cmp "$ROOT_DIR/resources/schema/runtime.schema.json" \
  "$runtime_home/resources/schema/runtime.schema.json" >/dev/null ||
  die "install did not refresh managed resources"

version_output="$(SN_CLI_HOME="$runtime_home" "$install_dir/sn-cli" --version)"
if [[ "$version_output" != "sn-cli $RELEASE_VERSION" && "$version_output" != "sn-cli $RELEASE_VERSION ("* ]]; then
  die "release binary version mismatch: $version_output"
fi
profile_human="$(SN_CLI_HOME="$runtime_home" "$install_dir/sn-cli" profile list)"
profile_human_trimmed="$(printf '%s' "$profile_human" | sed -E 's/^[[:space:]]+//')"
case "$profile_human_trimmed" in
  \{*) die "profile list default output was JSON instead of human text" ;;
esac
SN_CLI_HOME="$runtime_home" "$install_dir/sn-cli" --json profile check >/dev/null
SN_CLI_HOME="$runtime_home" "$install_dir/sn-cli" --json profile list >/dev/null
server_info="$(
  SN_CLI_HOME="$runtime_home" "$install_dir/sn-cli" --json server info
)"
printf '%s\n' "$server_info" | grep -Eq '"contract_version"[[:space:]]*:[[:space:]]*2' ||
  die "server info did not report contract_version=2"
printf '%s\n' "$server_info" | grep -Eq '"server"' ||
  die "server info did not report the server namespace"
[[ "$server_info" != *$'\n'* ]] ||
  die "server info JSON was not one compact document"
case "$server_info" in
  \{*\}) ;;
  *) die "server info JSON was not an object document" ;;
esac
system_stdout="$temp_root/system.stdout"
system_stderr="$temp_root/system.stderr"
if SN_CLI_HOME="$runtime_home" "$install_dir/sn-cli" --json system info \
  >"$system_stdout" 2>"$system_stderr"; then
  die "retired system namespace was still accepted"
fi
[ ! -s "$system_stdout" ] || die "failed JSON command wrote to stdout"
grep -Eq '"contract_version"[[:space:]]*:[[:space:]]*2' "$system_stderr" ||
  die "failed JSON command did not return a contract v2 error"
[ "$(awk 'NF {count++} END {print count + 0}' "$system_stderr")" -eq 1 ] ||
  die "failed JSON command did not return exactly one compact error document"

direct_home="$temp_root/direct-home"
mkdir -p "$direct_home/configs" "$direct_home/commands"
cp "$ROOT_DIR/configs/runtime/runtime.json" "$direct_home/runtime.json"
printf '%s\n' '{"type":"cli","binary":"/bin/echo","args":[],"transport":"tty","prompt_delivery":"manual"}' \
  >"$direct_home/configs/cx.json"
printf '%s\n' '{"type":"cli","binary":"/bin/echo","args":["configured"],"transport":"tty","prompt_delivery":"argv"}' \
  >"$direct_home/configs/commit.json"
printf '%s\n' '{"profile":"cx"}' >"$direct_home/commands/cx.json"
printf '%s\n' '{"profile":"commit"}' >"$direct_home/commands/commit.json"
direct_output="$(SN_CLI_HOME="$direct_home" "$install_dir/sn-cli" cx release-smoke)"
[ "$direct_output" = "release-smoke" ] || die "hard-compatible cx direct command failed"
commit_output="$(SN_CLI_HOME="$direct_home" "$install_dir/sn-cli" commit direct-smoke)"
[ "$commit_output" = "configured direct-smoke" ] ||
  die "commit direct command failed"
profile_commit_output="$(
  printf '%s' 'profile-smoke' |
    SN_CLI_HOME="$direct_home" "$install_dir/sn-cli" profile commit
)"
[ "$profile_commit_output" = "configured profile-smoke" ] ||
  die "profile commit one-shot command failed"

HTTP_ADDR="127.0.0.1:0" SN_CLI_HOME="$runtime_home" \
  "$install_dir/sn-cli" server start >/dev/null
for _ in {1..100}; do
  status="$(SN_CLI_HOME="$runtime_home" "$install_dir/sn-cli" --json server status)"
  if printf '%s\n' "$status" | grep -Eq '"running"[[:space:]]*:[[:space:]]*true'; then
    break
  fi
  sleep 0.1
done
printf '%s\n' "$status" | grep -Eq '"running"[[:space:]]*:[[:space:]]*true' ||
  die "sn-server did not start"
SN_CLI_HOME="$runtime_home" "$install_dir/sn-cli" server stop >/dev/null

log "[release-check] exercising concurrent server lifecycle operations"
start_pids=()
for index in 1 2; do
  HTTP_ADDR="127.0.0.1:0" SN_CLI_HOME="$runtime_home" \
    "$install_dir/sn-cli" server start \
    >"$temp_root/concurrent-start-$index.out" \
    2>"$temp_root/concurrent-start-$index.err" &
  start_pids+=("$!")
done
start_successes=0
for pid in "${start_pids[@]}"; do
  if wait "$pid"; then
    start_successes=$((start_successes + 1))
  fi
done
if [ "$start_successes" -lt 1 ]; then
  replay_logs "$temp_root"/concurrent-start-*.out "$temp_root"/concurrent-start-*.err
  die "all concurrent sn-server start operations failed"
fi
status="$(SN_CLI_HOME="$runtime_home" "$install_dir/sn-cli" --json server status)"
printf '%s\n' "$status" | grep -Eq '"running"[[:space:]]*:[[:space:]]*true' ||
  die "sn-server was not running after concurrent start"

stop_pids=()
for index in 1 2; do
  SN_CLI_HOME="$runtime_home" "$install_dir/sn-cli" server stop \
    >"$temp_root/concurrent-stop-$index.out" \
    2>"$temp_root/concurrent-stop-$index.err" &
  stop_pids+=("$!")
done
stop_successes=0
for pid in "${stop_pids[@]}"; do
  if wait "$pid"; then
    stop_successes=$((stop_successes + 1))
  fi
done
if [ "$stop_successes" -lt 1 ]; then
  replay_logs "$temp_root"/concurrent-stop-*.out "$temp_root"/concurrent-stop-*.err
  die "all concurrent sn-server stop operations failed"
fi
status="$(SN_CLI_HOME="$runtime_home" "$install_dir/sn-cli" --json server status)"
printf '%s\n' "$status" | grep -Eq '"running"[[:space:]]*:[[:space:]]*false' ||
  die "sn-server was still running after concurrent stop"

log "[release-check] passed"
