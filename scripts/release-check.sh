#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DIST_DIR="$ROOT_DIR/dist"
RELEASE_VERSION="${SN_CLI_VERSION:-release-check}"
GO_BIN="${GO:-go}"

# shellcheck source=scripts/release-profile-files.sh
source "$ROOT_DIR/scripts/release-profile-files.sh"
# shellcheck source=scripts/release-tool-files.sh
source "$ROOT_DIR/scripts/release-tool-files.sh"

log() { printf '%s\n' "$*" >&2; }
die() { printf 'release-check: %s\n' "$*" >&2; exit 1; }
run_make() {
  make --no-print-directory -C "$ROOT_DIR" V="${V:-0}" "$@"
}
# Test stages are timing-sensitive on CI; retry them at the shell level so a
# flaky run does not block a release. (The release workflow itself must not use
# a retry action: that changes the process-control environment and hangs the
# smoke section's background sn-server.) fmt-check is deterministic and is not
# retried.
retry_make() {
  local attempts=3 n=0
  until run_make "$@"; do
    n=$((n + 1))
    if [ "$n" -ge "$attempts" ]; then
      log "[release-check] $* failed after $attempts attempts"
      return 1
    fi
    log "[release-check] $* attempt $n failed, retrying"
  done
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
require_command() {
  command -v "$1" >/dev/null 2>&1 || die "$1 is required"
}
sha256_file() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    require_command shasum
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

if [[ ! "$RELEASE_VERSION" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$ ]]; then
  die "SN_CLI_VERSION must be a SemVer Git tag such as v0.1.0: $RELEASE_VERSION"
fi

log "[release-check] validating source"
for legacy_source in \
  "$ROOT_DIR/tools" \
  "$ROOT_DIR/configs/runtime" \
  "$ROOT_DIR/resources/tmux.conf" \
  "$ROOT_DIR/resources/release.json"; do
  [ ! -e "$legacy_source" ] && [ ! -L "$legacy_source" ] ||
    die "legacy source layout entry remains: $legacy_source"
done
[ -d "$ROOT_DIR/resources/tools" ] && [ ! -L "$ROOT_DIR/resources/tools" ] ||
  die "resources/tools must be a directory, not a symlink"
[ -d "$ROOT_DIR/release" ] && [ ! -L "$ROOT_DIR/release" ] ||
  die "release must be a directory, not a symlink"
[ -f "$ROOT_DIR/release/runtime.json" ] && [ ! -L "$ROOT_DIR/release/runtime.json" ] ||
  die "missing or unsafe release/runtime.json"
for profile in "${SN_CLI_RELEASE_PROFILE_FILES[@]}"; do
  [ -f "$ROOT_DIR/configs/$profile" ] || die "missing profile: $profile"
done
for tool in "${SN_CLI_RELEASE_TOOL_FILES[@]}"; do
  [ -f "$ROOT_DIR/resources/tools/$tool" ] && [ ! -L "$ROOT_DIR/resources/tools/$tool" ] ||
    die "missing or unsafe tool: $tool"
done
unexpected_config_entries="$(find "$ROOT_DIR/configs" -mindepth 1 -maxdepth 1 \
  ! -name '*.json' -print -quit)"
[ -z "$unexpected_config_entries" ] || die "unexpected configs entry: $unexpected_config_entries"
unexpected_resource_entries="$(find "$ROOT_DIR/resources" -mindepth 1 -maxdepth 1 \
  ! -name schema ! -name tools -print -quit)"
[ -z "$unexpected_resource_entries" ] || die "unexpected resources entry: $unexpected_resource_entries"
unexpected_tool_entries="$(find "$ROOT_DIR/resources/tools" -mindepth 1 -maxdepth 1 \
  ! -name '*.json' -print -quit)"
[ -z "$unexpected_tool_entries" ] || die "unexpected tools entry: $unexpected_tool_entries"
unexpected_release_entries="$(find "$ROOT_DIR/release" -mindepth 1 -maxdepth 1 \
  ! -name runtime.json ! -name tmux.conf ! -name release.json -print -quit)"
[ -z "$unexpected_release_entries" ] || die "unexpected release entry: $unexpected_release_entries"
for schema in profile.schema.json runtime.schema.json tool.schema.json; do
  [ -f "$ROOT_DIR/resources/schema/$schema" ] || die "missing resource schema: $schema"
done
[ -f "$ROOT_DIR/release/release.json" ] && [ ! -L "$ROOT_DIR/release/release.json" ] ||
  die "missing or unsafe activation manifest: release/release.json"
grep -Eq '"run_schema_version"[[:space:]]*:[[:space:]]*4([,}[:space:]]|$)' \
  "$ROOT_DIR/release/release.json" ||
  die "release manifest does not declare Run SQLite schema 4"
grep -Eq '"activation_epoch"[[:space:]]*:[[:space:]]*4([,}[:space:]]|$)' \
  "$ROOT_DIR/release/release.json" ||
  die "release manifest does not declare activation epoch 4"
grep -Eq '"contract_version"[[:space:]]*:[[:space:]]*5([,}[:space:]]|$)' \
  "$ROOT_DIR/release/release.json" ||
  die "release manifest does not declare contract version 5"
[ -f "$ROOT_DIR/release/tmux.conf" ] && [ ! -L "$ROOT_DIR/release/tmux.conf" ] ||
  die "missing or unsafe dedicated Tmux bootstrap config: release/tmux.conf"

run_make fmt-check
retry_make test-serial
retry_make test-race
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
  expected_profile_entries="$(
    printf 'configs/%s\n' "${SN_CLI_RELEASE_PROFILE_FILES[@]}" |
      LC_ALL=C sort
  )"
  actual_profile_entries="$(
    tar -tzf "$DIST_DIR/$asset" |
      awk 'index($0, "configs/") == 1 && $0 != "configs/" {print}' |
      LC_ALL=C sort
  )"
  [ "$actual_profile_entries" = "$expected_profile_entries" ] ||
    die "release asset Profile set does not match the formal release list: $asset"
  expected_tool_entries="$(
    printf 'resources/tools/%s\n' "${SN_CLI_RELEASE_TOOL_FILES[@]}" |
      LC_ALL=C sort
  )"
  actual_tool_entries="$(
    tar -tzf "$DIST_DIR/$asset" |
      awk 'index($0, "resources/tools/") == 1 && $0 != "resources/tools/" {print}' |
      LC_ALL=C sort
  )"
  [ "$actual_tool_entries" = "$expected_tool_entries" ] ||
    die "release asset Tool set does not match the formal release list: $asset"
  expected_release_entries="$(printf '%s\n' \
    release/release.json release/runtime.json release/tmux.conf | LC_ALL=C sort)"
  actual_release_entries="$(
    tar -tzf "$DIST_DIR/$asset" |
      awk 'index($0, "release/") == 1 && $0 != "release/" {print}' |
      LC_ALL=C sort
  )"
  [ "$actual_release_entries" = "$expected_release_entries" ] ||
    die "release asset fixed release set is invalid: $asset"
  legacy_payload_entries="$(
    tar -tzf "$DIST_DIR/$asset" |
      awk '$0 == "tools/" || index($0, "tools/") == 1 ||
        $0 == "runtime.json" || $0 == "resources/release.json" ||
        $0 == "resources/tmux.conf" {print}'
  )"
  [ -z "$legacy_payload_entries" ] ||
    die "release asset retained a legacy config path: $asset"
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
temp_root="$(cd "$temp_root" && pwd -P)"
runtime_home="$temp_root/home"
install_dir="$temp_root/bin"
archive="$DIST_DIR/sn-cli-$os_name-$arch_name.tar.gz"
release_server_pid=""
local_source_home=""
local_source_bin=""
release_tmux_tmp=""
tmux_id=""
cleanup() {
  if [ -n "$release_server_pid" ]; then
    kill "$release_server_pid" >/dev/null 2>&1 || true
    wait "$release_server_pid" >/dev/null 2>&1 || true
  fi
  if [ -x "$install_dir/sn-cli" ]; then
    SN_CLI_HOME="$runtime_home" "$install_dir/sn-cli" server stop >/dev/null 2>&1 || true
  fi
  if [ -n "$release_tmux_tmp" ] && [ -x "$install_dir/sn-cli" ]; then
    SN_CLI_HOME="$runtime_home" TMUX_TMPDIR="$release_tmux_tmp" \
      "$install_dir/sn-cli" session close-all \
      >/dev/null 2>&1 || true
    SN_CLI_HOME="$runtime_home" TMUX_TMPDIR="$release_tmux_tmp" \
      "$install_dir/sn-cli" tmux stop-all \
      >/dev/null 2>&1 || true
  fi
  if [ -n "$local_source_home" ] && [ -x "$local_source_bin/sn-cli" ]; then
    SN_CLI_HOME="$local_source_home" \
      "$local_source_bin/sn-cli" server stop >/dev/null 2>&1 || true
  fi
  rm -rf "$temp_root"
}
trap cleanup EXIT

log "[release-check] validating installer path safety"
if bash "$ROOT_DIR/install.sh" --dry-run \
  --binary "$temp_root/a" --archive "$temp_root/b" \
  >"$temp_root/mixed-local.out" 2>"$temp_root/mixed-local.err"; then
  die "installer accepted mutually exclusive local source modes"
fi
if bash "$ROOT_DIR/install.sh" --dry-run \
  --archive "$temp_root/a" --server "$temp_root/b" \
  >"$temp_root/archive-extra.out" 2>"$temp_root/archive-extra.err"; then
  die "installer accepted a binary-only option with --archive"
fi
if bash "$ROOT_DIR/install.sh" --dry-run \
  --checksums "$temp_root/a" \
  >"$temp_root/network-checksum.out" 2>"$temp_root/network-checksum.err"; then
  die "installer accepted --checksums without --archive"
fi
if bash "$ROOT_DIR/install.sh" --dry-run --home / \
  >"$temp_root/root-home.out" 2>"$temp_root/root-home.err"; then
  die "installer accepted / as Runtime home"
fi
if bash "$ROOT_DIR/install.sh" --dry-run --home "" \
  >"$temp_root/empty-home.out" 2>"$temp_root/empty-home.err"; then
  die "installer accepted an empty Runtime home"
fi
(
  cd "$temp_root"
  bash "$ROOT_DIR/install.sh" --dry-run \
    --home relative-home --install-dir relative-bin
) >"$temp_root/relative-home.out" 2>&1
grep -q "home: $temp_root/relative-home" "$temp_root/relative-home.out" ||
  die "installer did not canonicalize a relative Runtime home"

inside_home="$temp_root/inside-home"
if bash "$ROOT_DIR/install.sh" \
  --archive "$archive" \
  --checksums "$DIST_DIR/checksums.txt" \
  --home "$inside_home" \
  --install-dir "$inside_home/configs" \
  --overwrite-configs; then
  die "installer accepted an install directory inside Runtime home"
fi
[ ! -e "$inside_home" ] ||
  die "invalid nested install directory created Runtime home"

case_home="$temp_root/RuntimeHome"
if bash "$ROOT_DIR/install.sh" --dry-run \
  --home "$case_home" \
  --install-dir "$temp_root/runtimehome/configs" \
  >"$temp_root/case-home.out" 2>"$temp_root/case-home.err"; then
  die "installer accepted a case-folded install directory inside Runtime home"
fi
[ ! -e "$case_home" ] && [ ! -e "$temp_root/runtimehome" ] ||
  die "case-folded invalid paths created Runtime home"

unicode_home="$temp_root/ÄHome"
if bash "$ROOT_DIR/install.sh" --dry-run \
  --home "$unicode_home" \
  --install-dir "$temp_root/ähome/configs" \
  >"$temp_root/unicode-home.out" 2>"$temp_root/unicode-home.err"; then
  die "installer accepted unresolved non-ASCII paths with ambiguous containment"
fi
[ ! -e "$unicode_home" ] && [ ! -e "$temp_root/ähome" ] ||
  die "invalid unresolved Unicode paths created Runtime home"

alias_parent="$temp_root/alias-parent"
real_parent="$temp_root/real-parent"
alias_install="$temp_root/alias-bin"
mkdir -p "$real_parent"
ln -s "$real_parent" "$alias_parent"
alias_home="$alias_parent/runtime"
bash "$ROOT_DIR/install.sh" \
  --archive "$archive" \
  --checksums "$DIST_DIR/checksums.txt" \
  --home "$alias_home" \
  --install-dir "$alias_install" \
  --overwrite-configs
[ -x "$real_parent/runtime/bin/sn-cli" ] ||
  die "installer did not canonicalize a missing home below a symlink ancestor"
[ "$(readlink "$alias_install/sn-cli")" = "$real_parent/runtime/bin/sn-cli" ] ||
  die "installer command link did not use canonical Runtime home"

external_home="$temp_root/external-home"
symlink_home="$temp_root/symlink-home"
mkdir -p "$external_home"
chmod 700 "$external_home"
printf '%s\n' safe >"$external_home/sentinel"
ln -s "$external_home" "$symlink_home"
if bash "$ROOT_DIR/install.sh" \
  --archive "$archive" \
  --checksums "$DIST_DIR/checksums.txt" \
  --home "$symlink_home" \
  --install-dir "$temp_root/symlink-bin" \
  --overwrite-configs; then
  die "installer accepted a symlink Runtime home"
fi
[ "$(cat "$external_home/sentinel")" = "safe" ] ||
  die "symlink Runtime home changed an external sentinel"
[ ! -e "$external_home/bin/sn-cli" ] ||
  die "symlink Runtime home installed outside its declared root"

conflict_home="$temp_root/conflict-home"
conflict_install="$temp_root/conflict-bin"
mkdir -p "$conflict_install"
printf '%s\n' occupied >"$conflict_install/sn-cli"
if bash "$ROOT_DIR/install.sh" \
  --archive "$archive" \
  --checksums "$DIST_DIR/checksums.txt" \
  --home "$conflict_home" \
  --install-dir "$conflict_install" \
  --overwrite-configs; then
  die "installer overwrote a non-symlink command target"
fi
[ ! -e "$conflict_home/bin/sn-cli" ] ||
  die "install-link conflict was detected after Runtime activation"

log "[release-check] installing and exercising $archive"
mkdir -p "$runtime_home/configs" "$runtime_home/tools" "$runtime_home/resources"
chmod 700 "$runtime_home"
printf '%s\n' '{"type":"cli","command":"codex","unexpected":true}' \
  >"$runtime_home/configs/local-only.json"
printf '%s\n' '{"type":"cli","command":"codex","unexpected":true}' \
  >"$runtime_home/configs/cx.json"
printf '%s\n' '{"unexpected":true}' >"$runtime_home/runtime.json"
printf '%s\n' '{"unexpected":true}' >"$runtime_home/tools/local-only.json"
printf '%s\n' '{"unexpected":true}' >"$runtime_home/tools/web_search.json"
mkdir -p "$runtime_home/resources/schema"
printf '%s\n' 'outdated schema' >"$runtime_home/resources/schema/runtime.schema.json"
if bash "$ROOT_DIR/install.sh" \
  --archive "$archive" \
  --checksums "$DIST_DIR/checksums.txt" \
  --home "$runtime_home" \
  --install-dir "$install_dir"; then
  die "default install accepted invalid Profile/runtime config"
fi
grep -q '"unexpected"' "$runtime_home/configs/local-only.json" ||
  die "failed preflight changed an invalid profile"
grep -q '"unexpected"' "$runtime_home/configs/cx.json" ||
  die "failed preflight changed a same-name invalid profile"
grep -q '"unexpected"' "$runtime_home/runtime.json" ||
  die "failed preflight changed runtime.json"
grep -q '"unexpected"' "$runtime_home/tools/local-only.json" ||
  die "failed preflight changed a local-only tool"
grep -q '"unexpected"' "$runtime_home/tools/web_search.json" ||
  die "failed preflight changed a same-name tool"
grep -q 'outdated schema' "$runtime_home/resources/schema/runtime.schema.json" ||
  die "failed preflight changed managed resources"
[ ! -e "$runtime_home/bin/sn-cli" ] ||
  die "failed preflight installed sn-cli"
[ ! -e "$runtime_home/bin/sn-server" ] ||
  die "failed preflight installed sn-server"

bash "$ROOT_DIR/install.sh" \
  --archive "$archive" \
  --checksums "$DIST_DIR/checksums.txt" \
  --home "$runtime_home" \
  --install-dir "$install_dir" \
  --overwrite-configs

[ ! -e "$runtime_home/configs/local-only.json" ] ||
  die "--overwrite-configs kept a local-only profile"
[ ! -e "$runtime_home/tools/local-only.json" ] ||
  die "--overwrite-configs kept a local-only tool"
cmp "$ROOT_DIR/configs/cx.json" "$runtime_home/configs/cx.json" >/dev/null ||
  die "--overwrite-configs did not replace a same-name profile"
cmp "$ROOT_DIR/release/runtime.json" "$runtime_home/runtime.json" >/dev/null ||
  die "--overwrite-configs did not replace runtime.json"
cmp "$ROOT_DIR/resources/tools/web_search.json" "$runtime_home/tools/web_search.json" >/dev/null ||
  die "--overwrite-configs did not replace a same-name tool"
cmp "$ROOT_DIR/resources/schema/runtime.schema.json" \
  "$runtime_home/resources/schema/runtime.schema.json" >/dev/null ||
  die "install did not refresh managed resources"
cmp "$ROOT_DIR/release/tmux.conf" \
  "$runtime_home/resources/tmux.conf" >/dev/null ||
  die "install did not refresh the Tmux bootstrap config"
grep -Eq '"run_schema_version"[[:space:]]*:[[:space:]]*4([,}[:space:]]|$)' \
  "$runtime_home/resources/release.json" ||
  die "installed release manifest does not declare Run SQLite schema 4"
grep -Eq '"activation_epoch"[[:space:]]*:[[:space:]]*4([,}[:space:]]|$)' \
  "$runtime_home/resources/release.json" ||
  die "installed release manifest does not declare activation epoch 4"
grep -Eq '"contract_version"[[:space:]]*:[[:space:]]*5([,}[:space:]]|$)' \
  "$runtime_home/resources/release.json" ||
  die "installed release manifest does not declare contract version 5"

printf '%s\n' '{"type":"cli","command":"codex"}' \
  >"$runtime_home/configs/local-only.json"
printf '%s\n' '{"type":"cli","command":"codex","prompt":"local-default"}' \
  >"$runtime_home/configs/cx.json"
printf '%s\n' '{"agent":{"max_rounds":7}}' >"$runtime_home/runtime.json"
printf '\n' >>"$runtime_home/tools/web_search.json"
rm "$runtime_home/tools/web_fetch.json"
printf '%s\n' 'stale managed resource' >"$runtime_home/resources/schema/runtime.schema.json"
bash "$ROOT_DIR/install.sh" \
  --archive "$archive" \
  --checksums "$DIST_DIR/checksums.txt" \
  --home "$runtime_home" \
  --install-dir "$install_dir"
grep -q '"command":"codex"' "$runtime_home/configs/local-only.json" ||
  die "default install overwrote a current local profile"
grep -q '"local-default"' "$runtime_home/configs/cx.json" ||
  die "default install overwrote a current same-name profile"
grep -q '"max_rounds":7' "$runtime_home/runtime.json" ||
  die "default install overwrote a current runtime.json"
if cmp "$ROOT_DIR/resources/tools/web_search.json" \
  "$runtime_home/tools/web_search.json" >/dev/null; then
  die "default install overwrote a current same-name tool"
fi
cmp "$ROOT_DIR/resources/tools/web_fetch.json" "$runtime_home/tools/web_fetch.json" >/dev/null ||
  die "default install did not copy a missing packaged tool"
cmp "$ROOT_DIR/resources/schema/runtime.schema.json" \
  "$runtime_home/resources/schema/runtime.schema.json" >/dev/null ||
  die "default install did not refresh managed resources"

release_payload="$temp_root/release-payload"
mkdir -p "$release_payload"
tar -xzf "$archive" -C "$release_payload"
release_root="$temp_root/release-server"
release_dir="$release_root/$RELEASE_VERSION"
mkdir -p "$release_dir"
cp "$archive" "$release_dir/"
cp "$DIST_DIR/checksums.txt" "$release_dir/"
release_server="$temp_root/release-fileserver"
"$GO_BIN" -C "$ROOT_DIR" build \
  -o "$release_server" ./internal/testkit/releasefileserver
release_address_file="$temp_root/release-server.address"
"$release_server" \
  --root "$release_root" \
  --address-file "$release_address_file" \
  >"$temp_root/release-server.out" 2>"$temp_root/release-server.err" &
release_server_pid="$!"
for _ in {1..100}; do
  [ -s "$release_address_file" ] && break
  kill -0 "$release_server_pid" >/dev/null 2>&1 ||
    replay_logs "$temp_root/release-server.err"
  sleep 0.05
done
[ -s "$release_address_file" ] ||
  die "local release fixture server did not start"
release_address="$(tr -d '[:space:]' <"$release_address_file")"

current_update_output="$(
  SN_CLI_HOME="$runtime_home" \
    SN_CLI_RELEASE_BASE_URL="http://$release_address" \
    "$install_dir/sn-cli" --json server update \
      --version "$RELEASE_VERSION"
)"
printf '%s\n' "$current_update_output" |
  grep -Eq '"version"[[:space:]]*:[[:space:]]*"'"$RELEASE_VERSION"'"' ||
  die "current updater did not activate the staged candidate"
[ ! -e "$runtime_home/state/activation.guard.json" ] ||
  die "current updater left an activation guard"
[ ! -e "$runtime_home/state/activation.journal.json" ] ||
  die "current updater left an activation journal"

kill "$release_server_pid" >/dev/null 2>&1 || true
wait "$release_server_pid" || true
release_server_pid=""

log "[release-check] validating direct sn-server activation barrier"
printf '%s\n' '{}' >"$runtime_home/state/activation.guard.json"
HTTP_ADDR="127.0.0.1:0" SN_CLI_HOME="$runtime_home" \
  "$runtime_home/bin/sn-server" \
  >"$temp_root/guarded-server.out" 2>"$temp_root/guarded-server.err" &
guarded_server_pid="$!"
# Guard rejection (RequireNoGuard) should make sn-server exit immediately with
# an "activation gate" fatal log. Poll briefly; then SIGKILL unconditionally so
# the process is guaranteed reaped and the wait below cannot block — on some CI
# kernels a served sn-server (or a leftover zombie) does not respond within the
# poll window, and `wait` on it would hang the whole release check.
for _ in {1..100}; do
  if ! kill -0 "$guarded_server_pid" >/dev/null 2>&1; then
    break
  fi
  sleep 0.05
done
kill -KILL "$guarded_server_pid" >/dev/null 2>&1 || true
wait "$guarded_server_pid" >/dev/null 2>&1 || true
log "[release-check] guarded stdout: $(cat "$temp_root/guarded-server.out" 2>/dev/null)"
log "[release-check] guarded stderr: $(cat "$temp_root/guarded-server.err" 2>/dev/null)"
grep -q 'activation gate' "$temp_root/guarded-server.err" ||
  die "direct sn-server guard failure lacked activation diagnostic"
rm -f "$runtime_home/state/activation.guard.json"

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
printf '%s\n' "$server_info" | grep -Eq '"contract_version"[[:space:]]*:[[:space:]]*5' ||
  die "server info did not report contract_version=5"
printf '%s\n' "$server_info" | grep -Eq '"server"' ||
  die "server info did not report the server namespace"
[[ "$server_info" != *$'\n'* ]] ||
  die "server info JSON was not one compact document"
case "$server_info" in
  \{*\}) ;;
  *) die "server info JSON was not an object document" ;;
esac
unknown_stdout="$temp_root/unknown.stdout"
unknown_stderr="$temp_root/unknown.stderr"
if SN_CLI_HOME="$runtime_home" "$install_dir/sn-cli" --json unknown info \
  >"$unknown_stdout" 2>"$unknown_stderr"; then
  die "unknown namespace was accepted"
fi
[ ! -s "$unknown_stdout" ] || die "failed JSON command wrote to stdout"
grep -Eq '"contract_version"[[:space:]]*:[[:space:]]*5' "$unknown_stderr" ||
  die "failed JSON command did not return a contract v5 error"
[ "$(awk 'NF {count++} END {print count + 0}' "$unknown_stderr")" -eq 1 ] ||
  die "failed JSON command did not return exactly one compact error document"

direct_home="$temp_root/direct-home"
direct_bin="$direct_home/fake-bin"
mkdir -p "$direct_home/configs" "$direct_bin"
$GO_BIN -C "$ROOT_DIR" build \
  -o "$temp_root/ptyrun" ./internal/testkit/ptyx/cmd/ptyrun
# macOS platform binaries can be killed by AMFI after being copied to a new
# path. Keep the signed executable at its system path while exposing the
# adapter fixture under the expected command name.
ln -s /bin/echo "$direct_bin/codex"
ln -s /bin/echo "$direct_bin/claude"
cp "$ROOT_DIR/release/runtime.json" "$direct_home/runtime.json"
printf '%s\n' '{"type":"cli","command":"codex"}' \
  >"$direct_home/configs/cx.json"
printf '%s\n' '{"type":"cli","command":"codex","args":["--search"]}' \
  >"$direct_home/configs/commit.json"
require_command tmux
release_tmux_tmp="$temp_root/tmux-tmp"
mkdir -m 0700 "$release_tmux_tmp"
doctor_output="$(
  PATH="$direct_bin:$PATH" \
    Z_AI_API_KEY=doctor-smoke \
    ALIYUN_API_KEY=doctor-smoke \
    KMM_API_KEY=doctor-smoke \
    BAILIAN_API_KEY=doctor-smoke \
    WB_RUNTIME_IMAGE_PATH="$temp_root/doctor-image" \
    SN_CLI_HOME="$runtime_home" \
    TMUX_TMPDIR="$release_tmux_tmp" \
    "$install_dir/sn-cli" --json doctor
)"
printf '%s\n' "$doctor_output" |
  grep -Eq '"ok"[[:space:]]*:[[:space:]]*true' ||
  die "doctor did not report an OK installed Runtime: $doctor_output"
printf '%s\n' "$doctor_output" |
  grep -Eq '"contract_version"[[:space:]]*:[[:space:]]*5' ||
  die "doctor did not report contract_version=5"
direct_output="$(PATH="$direct_bin:$PATH" SN_CLI_HOME="$direct_home" \
  "$temp_root/ptyrun" "$install_dir/sn-cli" cx release-smoke | tr -d '\r')"
[ "$direct_output" = "-- release-smoke" ] ||
  die "bare cx Profile did not use direct argv: $direct_output"
exec_output="$(PATH="$direct_bin:$PATH" SN_CLI_HOME="$direct_home" \
  "$install_dir/sn-cli" exec cx release-smoke)"
[ "$exec_output" = "exec -- release-smoke" ] ||
  die "exec cx Profile did not use noninteractive argv: $exec_output"
commit_output="$(PATH="$direct_bin:$PATH" SN_CLI_HOME="$direct_home" \
  "$install_dir/sn-cli" exec commit direct-smoke)"
[ "$commit_output" = "--search exec -- direct-smoke" ] ||
  die "exec commit Profile did not use typed exec argv: $commit_output"
profile_commit_output="$(
  printf '%s' 'profile-smoke' |
    PATH="$direct_bin:$PATH" SN_CLI_HOME="$direct_home" \
      "$install_dir/sn-cli" exec commit
)"
[ "$profile_commit_output" = "--search exec -- profile-smoke" ] ||
  die "exec commit stdin task failed"
if PATH="$direct_bin:$PATH" SN_CLI_HOME="$direct_home" \
	"$install_dir/sn-cli" profile commit direct-smoke >/dev/null 2>&1; then
	die "removed profile execution route was accepted"
fi

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
running_digest_before="$(sha256_file "$runtime_home/bin/sn-cli")"
if bash "$ROOT_DIR/install.sh" \
  --archive "$archive" \
  --checksums "$DIST_DIR/checksums.txt" \
  --home "$runtime_home" \
  --install-dir "$install_dir" \
  --overwrite-configs; then
  die "installer activated while sn-server was running"
fi
[ "$(sha256_file "$runtime_home/bin/sn-cli")" = "$running_digest_before" ] ||
  die "blocked server-live upgrade changed the active binary"
SN_CLI_HOME="$runtime_home" "$install_dir/sn-cli" server stop >/dev/null
[ -f "$runtime_home/logs/sn-server.log" ] ||
  die "sn-server did not write its process log below the Runtime log root"
[ ! -e "$runtime_home/state/sn-server.log" ] ||
  die "sn-server retained the legacy state/sn-server.log path"

if PATH="$direct_bin:$PATH" SN_CLI_HOME="$runtime_home" \
  TMUX_TMPDIR="$release_tmux_tmp" \
  "$install_dir/sn-cli" tmux start cx >/dev/null 2>&1; then
  die "removed tmux start action was accepted"
fi
tmux_open="$(
  PATH="$direct_bin:$PATH" SN_CLI_HOME="$runtime_home" \
    TMUX_TMPDIR="$release_tmux_tmp" \
    "$install_dir/sn-cli" --json tmux open cx "tmux-smoke"
)"
tmux_id="$(
  printf '%s\n' "$tmux_open" |
    sed -n 's/.*"tmux_id":"\([^"]*\)".*/\1/p'
)"
[ -n "$tmux_id" ] || die "tmux open did not return tmux_id: $tmux_open"
TMUX_TMPDIR="$release_tmux_tmp" \
  tmux -L default list-sessions -F '#{session_name}' |
  grep -Fxq 'sn-session' ||
  die "default Tmux mode did not expose sn-session to ordinary tmux"
tmux_digest_before="$(sha256_file "$runtime_home/bin/sn-cli")"
if TMUX_TMPDIR="$release_tmux_tmp" bash "$ROOT_DIR/install.sh" \
  --archive "$archive" \
  --checksums "$DIST_DIR/checksums.txt" \
  --home "$runtime_home" \
  --install-dir "$install_dir" \
  --overwrite-configs; then
  die "installer activated while the managed Tmux session was live"
fi
[ "$(sha256_file "$runtime_home/bin/sn-cli")" = "$tmux_digest_before" ] ||
  die "blocked Tmux-live upgrade changed the active binary"
tmux_stop_all="$(PATH="$direct_bin:$PATH" SN_CLI_HOME="$runtime_home" \
  TMUX_TMPDIR="$release_tmux_tmp" \
  "$install_dir/sn-cli" --json tmux stop-all)"
printf '%s\n' "$tmux_stop_all" |
  grep -Eq '"stopped_count"[[:space:]]*:[[:space:]]*1' ||
  die "tmux stop-all did not stop the raw window: $tmux_stop_all"
tmux_id=""
if TMUX_TMPDIR="$release_tmux_tmp" \
  tmux -L default list-sessions -F '#{session_name}' 2>/dev/null |
  grep -Fxq 'sn-session'; then
  die "stopping the final managed window left sn-session behind"
fi
TMUX_TMPDIR="$release_tmux_tmp" bash "$ROOT_DIR/install.sh" \
  --archive "$archive" \
  --checksums "$DIST_DIR/checksums.txt" \
  --home "$runtime_home" \
  --install-dir "$install_dir" \
  --overwrite-configs

session_open="$(
  PATH="$direct_bin:$PATH" SN_CLI_HOME="$runtime_home" \
    TMUX_TMPDIR="$release_tmux_tmp" \
    "$install_dir/sn-cli" --json session open cx
)"
session_id="$(
  printf '%s\n' "$session_open" |
    sed -n 's/.*"id":"\(session_[^"]*\)".*/\1/p'
)"
[ -n "$session_id" ] ||
  die "session open did not return session_id: $session_open"
if PATH="$direct_bin:$PATH" SN_CLI_HOME="$runtime_home" \
  TMUX_TMPDIR="$release_tmux_tmp" \
  "$install_dir/sn-cli" tmux stop-all >/dev/null 2>&1; then
  die "tmux stop-all accepted a Session-bound window"
fi
session_close_all="$(
  PATH="$direct_bin:$PATH" SN_CLI_HOME="$runtime_home" \
    TMUX_TMPDIR="$release_tmux_tmp" \
    "$install_dir/sn-cli" --json session close-all
)"
printf '%s\n' "$session_close_all" |
  grep -Eq '"closed_count"[[:space:]]*:[[:space:]]*1' ||
  die "session close-all did not close the Session terminal: $session_close_all"
session_show="$(
  SN_CLI_HOME="$runtime_home" \
    "$install_dir/sn-cli" --json session show --session-id "$session_id"
)"
printf '%s\n' "$session_show" | grep -Fq "$session_id" ||
  die "session close-all removed the canonical Session"
audit_file="$runtime_home/logs/$(date +%y%m%d)/audit.jsonl"
[ -f "$audit_file" ] || die "control audit log was not created"
grep -Fq '"namespace":"doctor"' "$audit_file" ||
  die "doctor audit record is missing"
grep -Fq '"action":"stop-all"' "$audit_file" ||
  die "tmux stop-all audit record is missing"
grep -Fq '"action":"close-all"' "$audit_file" ||
  die "session close-all audit record is missing"

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

log "[release-check] exercising destructive local source install"
local_source_home="$temp_root/local-source-home"
local_source_bin="$temp_root/local-source-bin"
local_source_args=(
  --binary "$release_payload/sn-cli"
  --server "$release_payload/sn-server"
  --configs "$release_payload/configs"
  --resources "$release_payload/resources"
  --release "$release_payload/release"
  --home "$local_source_home"
  --install-dir "$local_source_bin"
  --local-source-install
)
bash "$ROOT_DIR/install.sh" "${local_source_args[@]}"

local_start="$(
  HTTP_ADDR="127.0.0.1:0" SN_CLI_HOME="$local_source_home" \
    "$local_source_bin/sn-cli" --json server start
)"
local_start_pid="$(
  printf '%s\n' "$local_start" |
    sed -n 's/.*"pid":\([0-9][0-9]*\).*/\1/p'
)"
[ -n "$local_start_pid" ] && [ "$local_start_pid" -gt 0 ] ||
  die "local source server start did not return a positive pid: $local_start"
local_status="$(
  SN_CLI_HOME="$local_source_home" \
    "$local_source_bin/sn-cli" --json server status
)"
printf '%s\n' "$local_status" |
  grep -Eq '"pid"[[:space:]]*:[[:space:]]*'"$local_start_pid" ||
  die "server status pid did not match server start"

printf '%s\n' \
  '{"type":"cli","command":"codex","prompt":"local-drift"}' \
  >"$local_source_home/configs/cx.json"
printf '\n' >>"$local_source_home/tools/web_search.json"
mkdir -p \
  "$local_source_home/sessions/_system" \
  "$local_source_home/state/session-locks" \
  "$local_source_home/state/session-invocations" \
  "$local_source_home/state/session-mutations" \
  "$local_source_home/state/session-trash-moves"
printf '%s\n' '{"schema_version":2,"sessions":[]}' \
  >"$local_source_home/sessions/_system/index.json"
printf '%s\n' runtime-state \
  >"$local_source_home/state/session-locks/index.lock"
printf '%s\n' runtime-state \
  >"$local_source_home/state/session-invocations/.invocation-current.json"
printf '%s\n' runtime-state \
  >"$local_source_home/state/session-mutations/session_fixture.json"
printf '%s\n' runtime-state \
  >"$local_source_home/state/session-trash-moves/session_fixture.json"

bash "$ROOT_DIR/install.sh" "${local_source_args[@]}"
local_status="$(
  SN_CLI_HOME="$local_source_home" \
    "$local_source_bin/sn-cli" --json server status
)"
printf '%s\n' "$local_status" |
  grep -Eq '"running"[[:space:]]*:[[:space:]]*false' ||
  die "local source install restarted sn-server"
[ ! -e "$local_source_home/state/sn-server.pid" ] ||
  die "local source install kept the stopped server pid record"
cmp "$ROOT_DIR/configs/cx.json" "$local_source_home/configs/cx.json" >/dev/null ||
  die "local source install did not replace active profiles"
cmp "$ROOT_DIR/resources/tools/web_search.json" \
  "$local_source_home/tools/web_search.json" >/dev/null ||
  die "local source install did not replace active tools"
for reset_path in \
  "$local_source_home/sessions" \
  "$local_source_home/state/session-locks" \
  "$local_source_home/state/session-invocations" \
  "$local_source_home/state/session-mutations" \
  "$local_source_home/state/session-trash-moves" \
  "$local_source_home/state/runtime.db" \
  "$local_source_home/state/runtime.db-wal" \
  "$local_source_home/state/runtime.db-shm" \
  "$local_source_home/state/runtime.db-journal"; do
  [ ! -e "$reset_path" ] ||
    die "local source install kept Runtime state: $reset_path"
done

mkdir -p "$local_source_home/sessions/_system"
printf '%s\n' '{"schema_version":999,"sessions":[]}' \
  >"$local_source_home/sessions/_system/index.json"
printf '%s\n' 'not a current Runtime database' \
  >"$local_source_home/state/runtime.db"
if bash "$ROOT_DIR/install.sh" \
  --archive "$archive" \
  --checksums "$DIST_DIR/checksums.txt" \
  --home "$local_source_home" \
  --install-dir "$local_source_bin" \
  --overwrite-configs; then
  die "archive installer inherited destructive local source semantics"
fi
[ -e "$local_source_home/sessions/_system/index.json" ] &&
  [ -e "$local_source_home/state/runtime.db" ] ||
  die "failed archive preflight changed unsupported Runtime state"
bash "$ROOT_DIR/install.sh" "${local_source_args[@]}"
[ ! -e "$local_source_home/sessions" ] &&
  [ ! -e "$local_source_home/state/runtime.db" ] ||
  die "repeated local source install did not reset unsupported state"

log "[release-check] passed"
