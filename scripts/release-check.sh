#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DIST_DIR="$ROOT_DIR/dist"
RELEASE_VERSION="${SN_CLI_VERSION:-release-check}"

log() { printf '%s\n' "$*" >&2; }
die() { printf 'release-check: %s\n' "$*" >&2; exit 1; }

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

make -C "$ROOT_DIR" fmt-check
make -C "$ROOT_DIR" test-serial
make -C "$ROOT_DIR" test-race
env GOCACHE="${GOCACHE:-$(go env GOCACHE)}" \
  GOMODCACHE="${GOMODCACHE:-$(go env GOMODCACHE)}" \
  go -C "$ROOT_DIR" vet ./...

log "[release-check] building assets version=$RELEASE_VERSION"
make -C "$ROOT_DIR" release-assets SN_CLI_VERSION="$RELEASE_VERSION"

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
if command -v sha256sum >/dev/null 2>&1; then
  (cd "$DIST_DIR" && sha256sum --check checksums.txt) >/dev/null ||
    die "release asset checksum verification failed"
else
  (cd "$DIST_DIR" && shasum -a 256 --check checksums.txt) >/dev/null ||
    die "release asset checksum verification failed"
fi

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
    SN_CLI_HOME="$runtime_home" "$install_dir/sn-cli" system stop >/dev/null 2>&1 || true
  fi
  rm -rf "$temp_root"
}
trap cleanup EXIT

log "[release-check] installing and exercising $archive"
mkdir -p "$runtime_home/configs" "$runtime_home/commands" "$runtime_home/resources"
printf '%s\n' '{"type":"cli","binary":"/bin/true","transport":"tty","prompt_delivery":"manual"}' \
  >"$runtime_home/configs/local-only.json"
printf '%s\n' '{"profile":"local-only"}' >"$runtime_home/commands/local-only.json"
printf '%s\n' '{"terminal":{"driver":"iterm2"}}' >"$runtime_home/runtime.json"
bash "$ROOT_DIR/install.sh" \
  --archive "$archive" \
  --checksums "$DIST_DIR/checksums.txt" \
  --home "$runtime_home" \
  --install-dir "$install_dir"

grep -q '"/bin/true"' "$runtime_home/configs/local-only.json" ||
  die "default install overwrote an existing profile"
grep -q '"local-only"' "$runtime_home/commands/local-only.json" ||
  die "default install overwrote an existing subcommand"
grep -q '"iterm2"' "$runtime_home/runtime.json" ||
  die "default install overwrote runtime.json"
[ -x "$runtime_home/bin/sn-cli" ] || die "sn-cli was not installed"
[ -x "$runtime_home/bin/sn-server" ] || die "sn-server was not installed"

printf '%s\n' 'outdated schema' >"$runtime_home/resources/schema/runtime.schema.json"
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
cmp "$ROOT_DIR/configs/runtime/runtime.json" "$runtime_home/runtime.json" >/dev/null ||
  die "--overwrite-configs did not replace runtime.json"
cmp "$ROOT_DIR/resources/schema/runtime.schema.json" \
  "$runtime_home/resources/schema/runtime.schema.json" >/dev/null ||
  die "--overwrite-configs did not replace resources"

version_output="$(SN_CLI_HOME="$runtime_home" "$install_dir/sn-cli" --version)"
if [[ "$version_output" != "sn-cli $RELEASE_VERSION" && "$version_output" != "sn-cli $RELEASE_VERSION ("* ]]; then
  die "release binary version mismatch: $version_output"
fi
SN_CLI_HOME="$runtime_home" "$install_dir/sn-cli" profile check >/dev/null
SN_CLI_HOME="$runtime_home" "$install_dir/sn-cli" profile list >/dev/null
SN_CLI_HOME="$runtime_home" "$install_dir/sn-cli" system info >/dev/null

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
  "$install_dir/sn-cli" system start >/dev/null
for _ in {1..100}; do
  status="$(SN_CLI_HOME="$runtime_home" "$install_dir/sn-cli" system status)"
  if printf '%s\n' "$status" | grep -Eq '"running"[[:space:]]*:[[:space:]]*true'; then
    break
  fi
  sleep 0.1
done
printf '%s\n' "$status" | grep -Eq '"running"[[:space:]]*:[[:space:]]*true' ||
  die "sn-server did not start"
SN_CLI_HOME="$runtime_home" "$install_dir/sn-cli" system stop >/dev/null

log "[release-check] passed"
