#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DIST_DIR="$ROOT_DIR/dist"
RELEASE_VERSION="${SN_CLI_VERSION:-release-check}"
GO_BIN="${GO:-go}"

# shellcheck source=scripts/release-profile-files.sh
source "$ROOT_DIR/scripts/release-profile-files.sh"

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
[ -f "$ROOT_DIR/configs/runtime/runtime.json" ] || die "missing configs/runtime/runtime.json"
for profile in "${SN_CLI_RELEASE_PROFILE_FILES[@]}"; do
  [ -f "$ROOT_DIR/configs/$profile" ] || die "missing profile: $profile"
done
unexpected_config_entries="$(find "$ROOT_DIR/configs" -mindepth 1 -maxdepth 1 \
  ! -name runtime ! -name '*.json' -print -quit)"
[ -z "$unexpected_config_entries" ] || die "unexpected configs entry: $unexpected_config_entries"
for schema in profile.schema.json runtime.schema.json; do
  [ -f "$ROOT_DIR/resources/schema/$schema" ] || die "missing resource schema: $schema"
done
[ -f "$ROOT_DIR/resources/release.json" ] ||
  die "missing resource activation manifest: resources/release.json"
grep -Eq '"run_schema_version"[[:space:]]*:[[:space:]]*4([,}[:space:]]|$)' \
  "$ROOT_DIR/resources/release.json" ||
  die "release manifest does not declare Run SQLite schema 4"
[ -f "$ROOT_DIR/resources/tmux.conf" ] ||
  die "missing dedicated Tmux bootstrap config: resources/tmux.conf"

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
  if tar -tzf "$DIST_DIR/$asset" |
    grep -Eq '(^|/)commands(/|$)'; then
    die "release asset retained the removed commands directory: $asset"
  fi
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
cleanup() {
  if [ -n "$release_server_pid" ]; then
    kill "$release_server_pid" >/dev/null 2>&1 || true
    wait "$release_server_pid" >/dev/null 2>&1 || true
  fi
  if [ -x "$install_dir/sn-cli" ]; then
    SN_CLI_HOME="$runtime_home" "$install_dir/sn-cli" server stop >/dev/null 2>&1 || true
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
mkdir -p "$runtime_home/configs" "$runtime_home/commands" "$runtime_home/resources"
chmod 700 "$runtime_home"
printf '%s\n' '{"type":"cli","binary":"codex","transport":"tty","prompt_delivery":"manual"}' \
  >"$runtime_home/configs/local-only.json"
printf '%s\n' '{"profile":"local-only"}' >"$runtime_home/commands/local-only.json"
printf '%s\n' '{"type":"cli","binary":"codex","transport":"tty","prompt_delivery":"manual"}' \
  >"$runtime_home/configs/cx.json"
printf '%s\n' '{"profile":"local-only"}' >"$runtime_home/commands/cx.json"
printf '%s\n' '{"terminal":{"driver":"iterm2"}}' >"$runtime_home/runtime.json"
mkdir -p "$runtime_home/resources/schema"
printf '%s\n' 'outdated schema' >"$runtime_home/resources/schema/runtime.schema.json"
if bash "$ROOT_DIR/install.sh" \
  --archive "$archive" \
  --checksums "$DIST_DIR/checksums.txt" \
  --home "$runtime_home" \
  --install-dir "$install_dir"; then
  die "default install accepted legacy Profile/runtime config"
fi
grep -q '"binary"' "$runtime_home/configs/local-only.json" ||
  die "failed preflight changed a legacy profile"
grep -q '"local-only"' "$runtime_home/commands/local-only.json" ||
  die "failed preflight changed obsolete commands state"
grep -q '"binary"' "$runtime_home/configs/cx.json" ||
  die "failed preflight changed a same-name legacy profile"
grep -q '"local-only"' "$runtime_home/commands/cx.json" ||
  die "failed preflight changed same-name obsolete commands state"
grep -q '"iterm2"' "$runtime_home/runtime.json" ||
  die "failed preflight changed runtime.json"
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

[ ! -e "$runtime_home/commands" ] ||
  die "--overwrite-configs kept the obsolete commands directory"
[ ! -e "$runtime_home/configs/local-only.json" ] ||
  die "--overwrite-configs kept a local-only profile"
cmp "$ROOT_DIR/configs/cx.json" "$runtime_home/configs/cx.json" >/dev/null ||
  die "--overwrite-configs did not replace a same-name profile"
cmp "$ROOT_DIR/configs/runtime/runtime.json" "$runtime_home/runtime.json" >/dev/null ||
  die "--overwrite-configs did not replace runtime.json"
cmp "$ROOT_DIR/resources/schema/runtime.schema.json" \
  "$runtime_home/resources/schema/runtime.schema.json" >/dev/null ||
  die "install did not refresh managed resources"
cmp "$ROOT_DIR/resources/tmux.conf" \
  "$runtime_home/resources/tmux.conf" >/dev/null ||
  die "install did not refresh the Tmux bootstrap config"
grep -Eq '"run_schema_version"[[:space:]]*:[[:space:]]*4([,}[:space:]]|$)' \
  "$runtime_home/resources/release.json" ||
  die "installed release manifest does not declare Run SQLite schema 4"

printf '%s\n' '{"type":"cli","command":"codex","exec":false}' \
  >"$runtime_home/configs/local-only.json"
mkdir -p "$runtime_home/commands"
printf '%s\n' '{"profile":"local-only"}' >"$runtime_home/commands/local-only.json"
printf '%s\n' '{"type":"cli","command":"codex","exec":false,"prompt":"local-default"}' \
  >"$runtime_home/configs/cx.json"
printf '%s\n' '{"profile":"local-only"}' >"$runtime_home/commands/cx.json"
printf '%s\n' '{"agent":{"max_rounds":7}}' >"$runtime_home/runtime.json"
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
[ ! -e "$runtime_home/commands" ] ||
  die "default install kept the obsolete commands directory"
cmp "$ROOT_DIR/resources/schema/runtime.schema.json" \
  "$runtime_home/resources/schema/runtime.schema.json" >/dev/null ||
  die "default install did not refresh managed resources"

legacy_payload="$temp_root/legacy-payload"
legacy_merged="$temp_root/legacy-merged"
mkdir -p "$legacy_payload" "$legacy_merged"
tar -xzf "$archive" -C "$legacy_payload"
cp -R "$legacy_payload/configs" "$legacy_merged/configs"
cp -R "$legacy_payload/resources" "$legacy_merged/resources"
cp "$legacy_payload/runtime.json" "$legacy_merged/runtime.json"
active_digest_before="$(sha256_file "$runtime_home/bin/sn-cli")"
if SN_CLI_HOME="$legacy_merged" \
  "$legacy_payload/sn-cli" profile list \
  >"$temp_root/legacy.stdout" 2>"$temp_root/legacy.stderr"; then
  die "contract-v3 candidate accepted legacy updater staged profile list"
fi
grep -q 'legacy updater' "$temp_root/legacy.stderr" ||
  die "legacy updater gate did not return the expected diagnostic"
[ "$(sha256_file "$runtime_home/bin/sn-cli")" = "$active_digest_before" ] ||
  die "legacy updater gate changed the active binary"

log "[release-check] exercising the actual v0.1.1 updater"
legacy_source="$temp_root/v0.1.1-source"
legacy_binary="$temp_root/v0.1.1-sn-cli"
mkdir -p "$legacy_source"
git -C "$ROOT_DIR" rev-parse --verify 'refs/tags/v0.1.1^{commit}' >/dev/null ||
  die "required legacy updater fixture tag v0.1.1 is unavailable"
git -C "$ROOT_DIR" archive v0.1.1 | tar -xf - -C "$legacy_source"
(
  cd "$legacy_source"
  "$GO_BIN" build -o "$legacy_binary" ./cmd/sn-cli
)
legacy_release_root="$temp_root/legacy-release-server"
legacy_release_dir="$legacy_release_root/$RELEASE_VERSION"
mkdir -p "$legacy_release_dir"
cp "$archive" "$legacy_release_dir/"
cp "$DIST_DIR/checksums.txt" "$legacy_release_dir/"
release_server="$temp_root/release-fileserver"
"$GO_BIN" -C "$ROOT_DIR" build \
  -o "$release_server" ./runtimetest/releasefileserver
release_address_file="$temp_root/release-server.address"
"$release_server" \
  --root "$legacy_release_root" \
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

legacy_before="$temp_root/legacy-active-before"
mkdir -p "$legacy_before"
cp -R "$runtime_home/bin" "$legacy_before/bin"
cp -R "$runtime_home/configs" "$legacy_before/configs"
cp -R "$runtime_home/resources" "$legacy_before/resources"
cp "$runtime_home/runtime.json" "$legacy_before/runtime.json"
if SN_CLI_HOME="$runtime_home" \
  SN_CLI_RELEASE_BASE_URL="http://$release_address" \
  "$legacy_binary" system update --version "$RELEASE_VERSION" \
  >"$temp_root/v0.1.1-update.out" 2>"$temp_root/v0.1.1-update.err"; then
  die "v0.1.1 updater activated a contract-v3 release"
fi
grep -q 'legacy updater' "$temp_root/v0.1.1-update.err" ||
  die "v0.1.1 updater did not fail at the staged activation gate"
cmp "$legacy_before/bin/sn-cli" "$runtime_home/bin/sn-cli" >/dev/null ||
  die "v0.1.1 updater changed sn-cli"
cmp "$legacy_before/bin/sn-server" "$runtime_home/bin/sn-server" >/dev/null ||
  die "v0.1.1 updater changed sn-server"
diff -qr "$legacy_before/configs" "$runtime_home/configs" >/dev/null ||
  die "v0.1.1 updater changed active profiles"
[ ! -e "$runtime_home/commands" ] ||
  die "v0.1.1 updater recreated the obsolete commands directory"
cmp "$legacy_before/runtime.json" "$runtime_home/runtime.json" >/dev/null ||
  die "v0.1.1 updater changed runtime.json"
for legacy_directory in personas skills tools; do
  legacy_path="$runtime_home/resources/$legacy_directory"
  [ -d "$legacy_path" ] && [ ! -L "$legacy_path" ] ||
    die "v0.1.1 bootstrap created an invalid legacy resource directory"
  unexpected_legacy_entry="$(
    find "$legacy_path" -mindepth 1 -print -quit
  )"
  [ -z "$unexpected_legacy_entry" ] ||
    die "v0.1.1 bootstrap wrote data into legacy resource directory"
  mkdir -p "$legacy_before/resources/$legacy_directory"
done
if ! diff -qr "$legacy_before/resources" "$runtime_home/resources" \
  >"$temp_root/v0.1.1-resources.diff"; then
  replay_logs "$temp_root/v0.1.1-resources.diff"
  die "v0.1.1 updater changed managed resources"
fi

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
for _ in {1..20}; do
  if ! kill -0 "$guarded_server_pid" >/dev/null 2>&1; then
    break
  fi
  sleep 0.05
done
if kill -0 "$guarded_server_pid" >/dev/null 2>&1; then
  kill "$guarded_server_pid" >/dev/null 2>&1 || true
  wait "$guarded_server_pid" >/dev/null 2>&1 || true
  die "direct sn-server entry ran while activation guard was present"
fi
if wait "$guarded_server_pid"; then
  die "direct sn-server entry accepted activation guard"
fi
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
printf '%s\n' "$server_info" | grep -Eq '"contract_version"[[:space:]]*:[[:space:]]*3' ||
  die "server info did not report contract_version=3"
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
grep -Eq '"contract_version"[[:space:]]*:[[:space:]]*3' "$system_stderr" ||
  die "failed JSON command did not return a contract v3 error"
[ "$(awk 'NF {count++} END {print count + 0}' "$system_stderr")" -eq 1 ] ||
  die "failed JSON command did not return exactly one compact error document"

direct_home="$temp_root/direct-home"
direct_bin="$direct_home/fake-bin"
mkdir -p "$direct_home/configs" "$direct_bin"
# macOS platform binaries can be killed by AMFI after being copied to a new
# path. Keep the signed executable at its system path while exposing the
# adapter fixture under the expected command name.
ln -s /bin/echo "$direct_bin/codex"
cp "$ROOT_DIR/configs/runtime/runtime.json" "$direct_home/runtime.json"
printf '%s\n' '{"type":"cli","command":"codex","exec":false}' \
  >"$direct_home/configs/cx.json"
printf '%s\n' '{"type":"cli","command":"codex","args":["--search"],"exec":true}' \
  >"$direct_home/configs/commit.json"
direct_output="$(PATH="$direct_bin:$PATH" SN_CLI_HOME="$direct_home" \
  "$install_dir/sn-cli" cx --exec release-smoke)"
explicit_direct_output="$(PATH="$direct_bin:$PATH" SN_CLI_HOME="$direct_home" \
  "$install_dir/sn-cli" profile cx --exec release-smoke)"
[ "$direct_output" = "$explicit_direct_output" ] ||
  die "implicit cx Profile argv differed from explicit profile cx"
[ "$direct_output" = "exec -- release-smoke" ] ||
  die "implicit cx Profile did not use typed prompt argv: $direct_output"
commit_output="$(PATH="$direct_bin:$PATH" SN_CLI_HOME="$direct_home" \
  "$install_dir/sn-cli" commit direct-smoke)"
explicit_commit_output="$(PATH="$direct_bin:$PATH" SN_CLI_HOME="$direct_home" \
  "$install_dir/sn-cli" profile commit direct-smoke)"
[ "$commit_output" = "$explicit_commit_output" ] ||
  die "implicit commit Profile argv differed from explicit profile commit"
[ "$commit_output" = "--search exec -- direct-smoke" ] ||
  die "implicit commit Profile did not use typed exec argv: $commit_output"
profile_commit_output="$(
  printf '%s' 'profile-smoke' |
    PATH="$direct_bin:$PATH" SN_CLI_HOME="$direct_home" \
      "$install_dir/sn-cli" profile commit
)"
[ "$profile_commit_output" = "--search exec -- profile-smoke" ] ||
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

require_command tmux
tmux_start="$(
  PATH="$direct_bin:$PATH" SN_CLI_HOME="$runtime_home" \
    "$install_dir/sn-cli" --json tmux start cx "tmux-smoke"
)"
tmux_id="$(
  printf '%s\n' "$tmux_start" |
    sed -n 's/.*"tmux_id":"\([^"]*\)".*/\1/p'
)"
[ -n "$tmux_id" ] || die "tmux start did not return tmux_id: $tmux_start"
tmux_digest_before="$(sha256_file "$runtime_home/bin/sn-cli")"
if bash "$ROOT_DIR/install.sh" \
  --archive "$archive" \
  --checksums "$DIST_DIR/checksums.txt" \
  --home "$runtime_home" \
  --install-dir "$install_dir" \
  --overwrite-configs; then
  die "installer activated while the dedicated Tmux server was live"
fi
[ "$(sha256_file "$runtime_home/bin/sn-cli")" = "$tmux_digest_before" ] ||
  die "blocked Tmux-live upgrade changed the active binary"
PATH="$direct_bin:$PATH" SN_CLI_HOME="$runtime_home" \
  "$install_dir/sn-cli" tmux stop --tmux-id "$tmux_id" >/dev/null
bash "$ROOT_DIR/install.sh" \
  --archive "$archive" \
  --checksums "$DIST_DIR/checksums.txt" \
  --home "$runtime_home" \
  --install-dir "$install_dir" \
  --overwrite-configs

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
  --binary "$legacy_payload/sn-cli"
  --server "$legacy_payload/sn-server"
  --configs "$legacy_payload/configs"
  --runtime-config "$legacy_payload/runtime.json"
  --resources "$legacy_payload/resources"
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
  '{"type":"cli","binary":"codex","transport":"tty","prompt_delivery":"manual"}' \
  >"$local_source_home/configs/cx.json"
mkdir -p \
  "$local_source_home/commands" \
  "$local_source_home/sessions/_system" \
  "$local_source_home/state/session-locks" \
  "$local_source_home/state/session-invocations" \
  "$local_source_home/state/session-mutations" \
  "$local_source_home/state/session-trash-moves"
printf '%s\n' '{"profile":"cx"}' \
  >"$local_source_home/commands/cx.json"
printf '%s\n' '{"schema_version":1,"sessions":[]}' \
  >"$local_source_home/sessions/_system/index.json"
printf '%s\n' legacy \
  >"$local_source_home/state/session-locks/index.lock"
printf '%s\n' legacy \
  >"$local_source_home/state/session-invocations/.invocation-old.json"
printf '%s\n' legacy \
  >"$local_source_home/state/session-mutations/session_fixture.json"
printf '%s\n' legacy \
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
[ ! -e "$local_source_home/commands" ] ||
  die "local source install kept the obsolete commands directory"
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
printf '%s\n' '{"schema_version":1,"sessions":[]}' \
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
  die "failed archive preflight changed incompatible Runtime state"
bash "$ROOT_DIR/install.sh" "${local_source_args[@]}"
[ ! -e "$local_source_home/sessions" ] &&
  [ ! -e "$local_source_home/state/runtime.db" ] ||
  die "repeated local source install did not reset incompatible state"

log "[release-check] passed"
