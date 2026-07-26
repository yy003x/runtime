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
required_configs=(api-cc.json api-cx.json cc-bai.json cc.json commit.json cx-adv.json cx-deep.json cx-image.json cx-spark.json cx.json runtime.yaml)
for config in "${required_configs[@]}"; do
  [ -f "$ROOT_DIR/configs/$config" ] || die "missing required config template: $config"
done
unexpected_configs="$(find "$ROOT_DIR/configs" -mindepth 1 -maxdepth 1 -type f ! -name '*.json' ! -name 'runtime.yaml' -exec basename {} \; | LC_ALL=C sort | tr '\n' ' ')"
[ -z "$unexpected_configs" ] || die "configs/ only accepts provider JSON and runtime.yaml: $unexpected_configs"
if find "$ROOT_DIR/configs" -mindepth 1 -type d -print -quit | grep -q .; then
  die "configs/ must not contain resource directories"
fi
for schema in provider-profile.schema.json runtime.schema.json; do
  [ -f "$ROOT_DIR/resources/schema/$schema" ] || die "missing resource schema: $schema"
done
make -C "$ROOT_DIR" fmt-check
make -C "$ROOT_DIR" test-serial
env GOCACHE="${GOCACHE:-$(go env GOCACHE)}" GOMODCACHE="${GOMODCACHE:-$(go env GOMODCACHE)}" go -C "$ROOT_DIR" vet ./...

log "[release-check] building assets version=$RELEASE_VERSION"
make -C "$ROOT_DIR" release-assets SN_CLI_VERSION="$RELEASE_VERSION"

expected_assets=(checksums.txt)
for platform in darwin-arm64 darwin-amd64 linux-arm64 linux-amd64; do
  expected_assets+=("sn-cli-$platform.tar.gz" "sn-server-$platform")
done
for asset in "${expected_assets[@]}"; do
  [ -f "$DIST_DIR/$asset" ] || die "missing release asset: $asset"
done
for asset in "${expected_assets[@]:1}"; do
  awk -v name="$asset" '$2 == name || $2 == "*" name {found=1} END {exit !found}' "$DIST_DIR/checksums.txt" || \
    die "checksum missing for $asset"
done
if command -v sha256sum >/dev/null 2>&1; then
  (cd "$DIST_DIR" && sha256sum --check checksums.txt) >/dev/null || die "release asset checksum verification failed"
else
  (cd "$DIST_DIR" && shasum -a 256 --check checksums.txt) >/dev/null || die "release asset checksum verification failed"
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
mkdir -p "$runtime_home/configs" "$runtime_home/resources"
printf '%s\n' '{"command":"/bin/true"}' >"$runtime_home/configs/cx.json"
printf '%s\n' '{"command":"/bin/true"}' >"$runtime_home/configs/local-only.json"
bash "$ROOT_DIR/install.sh" \
  --archive "$archive" \
  --checksums "$DIST_DIR/checksums.txt" \
  --home "$runtime_home" \
  --install-dir "$install_dir"

grep -q '"/bin/true"' "$runtime_home/configs/cx.json" || \
  die "default install overwrote an existing config"
printf '%s\n' 'outdated schema' >"$runtime_home/resources/schema/runtime.schema.json"
bash "$ROOT_DIR/install.sh" \
  --archive "$archive" \
  --checksums "$DIST_DIR/checksums.txt" \
  --home "$runtime_home" \
  --install-dir "$install_dir" \
  --overwrite-configs
cmp "$ROOT_DIR/configs/cx.json" "$runtime_home/configs/cx.json" >/dev/null || \
  die "--overwrite-configs did not replace the packaged config"
[ ! -e "$runtime_home/configs/local-only.json" ] || \
  die "--overwrite-configs kept a local-only profile"
cmp "$ROOT_DIR/resources/schema/runtime.schema.json" "$runtime_home/resources/schema/runtime.schema.json" >/dev/null || \
  die "--overwrite-configs did not replace a packaged resource"

for directory in personas skills tools schema; do
  [ -d "$runtime_home/resources/$directory" ] || die "missing runtime resource directory: $directory"
done
for schema in provider-profile.schema.json runtime.schema.json; do
  [ -f "$runtime_home/resources/schema/$schema" ] || die "release did not install schema resource: $schema"
done

version_output="$(SN_CLI_HOME="$runtime_home" "$install_dir/sn-cli" --version)"
if [[ "$version_output" != "sn-cli $RELEASE_VERSION" && "$version_output" != "sn-cli $RELEASE_VERSION ("* ]]; then
  die "release binary version mismatch: $version_output"
fi
doctor_output="$(SN_CLI_HOME="$runtime_home" "$install_dir/sn-cli" system doctor --json)"
printf '%s\n' "$doctor_output" | grep -Eq '"contract_version"[[:space:]]*:[[:space:]]*1' || \
  die "doctor output has no supported contract_version"
printf '%s\n' "$doctor_output" | grep -Eq '"durable_queue"[[:space:]]*:[[:space:]]*1' || \
  die "doctor output has no durable_queue feature"
SN_CLI_HOME="$runtime_home" "$install_dir/sn-cli" profile list >/dev/null
cp "$ROOT_DIR/test/fixtures/cli-smoke-profile.json" "$runtime_home/configs/cli-smoke.json"
SN_CLI_HOME="$runtime_home" "$install_dir/sn-cli" profile validate cli-smoke >/dev/null
run_output="$(SN_CLI_HOME="$runtime_home" "$install_dir/sn-cli" session run --json \
  --run-id turn-release-check --session-id session-release-check --project release-check --cwd "$ROOT_DIR" \
  --deadline-seconds 30 cli-smoke 'release smoke')"
printf '%s\n' "$run_output" | grep -Eq '"state"[[:space:]]*:[[:space:]]*"done"' || \
  die "cli-smoke task did not finish"
result_output="$(SN_CLI_HOME="$runtime_home" "$install_dir/sn-cli" run result --run-id turn-release-check)"
printf '%s\n' "$result_output" | grep -Eq '"outcome"[[:space:]]*:[[:space:]]*"succeeded"' || \
  die "cli-smoke result did not succeed"

async_output="$(SN_CLI_HOME="$runtime_home" "$install_dir/sn-cli" session submit \
  --run-id turn-release-check-async --session-id session-release-check-async --project release-check --cwd "$ROOT_DIR" \
  --deadline-seconds 30 cli-smoke 'release async smoke')"
printf '%s\n' "$async_output" | grep -Eq '"state"[[:space:]]*:[[:space:]]*"pending"' || \
  die "cli-smoke async submit was not accepted"
for _ in {1..100}; do
  async_status="$(SN_CLI_HOME="$runtime_home" "$install_dir/sn-cli" run show --run-id turn-release-check-async)"
  if printf '%s\n' "$async_status" | grep -Eq '"state"[[:space:]]*:[[:space:]]*"done"'; then
    break
  fi
  sleep 0.1
done
printf '%s\n' "$async_status" | grep -Eq '"state"[[:space:]]*:[[:space:]]*"done"' || \
  die "cli-smoke async task did not finish"
SN_CLI_HOME="$runtime_home" "$install_dir/sn-cli" run list --state "done" --limit 5 >/dev/null
SN_CLI_HOME="$runtime_home" "$install_dir/sn-cli" run reconcile --dry-run >/dev/null
SN_CLI_HOME="$runtime_home" "$install_dir/sn-cli" system stop >/dev/null

log "[release-check] passed"
