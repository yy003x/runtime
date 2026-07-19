#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DIST_DIR="$ROOT_DIR/dist"
RELEASE_VERSION="${SN_CLI_VERSION:-release-check}"

log() { printf '%s\n' "$*" >&2; }
die() { printf 'release-check: %s\n' "$*" >&2; exit 1; }

log "[release-check] validating source"
make -C "$ROOT_DIR" fmt-check
make -C "$ROOT_DIR" test-serial
env GOCACHE="${GOCACHE:-/tmp/go-build}" GOMODCACHE="${GOMODCACHE:-/tmp/go-mod}" go -C "$ROOT_DIR" vet ./...

log "[release-check] building assets version=$RELEASE_VERSION"
make -C "$ROOT_DIR" release SN_CLI_VERSION="$RELEASE_VERSION"

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
trap 'rm -rf "$temp_root"' EXIT
runtime_home="$temp_root/home"
install_dir="$temp_root/bin"
archive="$DIST_DIR/sn-cli-$os_name-$arch_name.tar.gz"

log "[release-check] installing and exercising $archive"
bash "$ROOT_DIR/install.sh" \
  --archive "$archive" \
  --checksums "$DIST_DIR/checksums.txt" \
  --home "$runtime_home" \
  --install-dir "$install_dir"

SN_CLI_HOME="$runtime_home" "$install_dir/sn-cli" version >/dev/null
doctor_output="$(SN_CLI_HOME="$runtime_home" "$install_dir/sn-cli" doctor --json)"
printf '%s\n' "$doctor_output" | grep -Eq '"contract_version"[[:space:]]*:[[:space:]]*1' || \
  die "doctor output has no supported contract_version"
SN_CLI_HOME="$runtime_home" "$install_dir/sn-cli" profiles >/dev/null
SN_CLI_HOME="$runtime_home" "$install_dir/sn-cli" config validate -c native-mock >/dev/null
run_output="$(SN_CLI_HOME="$runtime_home" "$install_dir/sn-cli" task run \
  -c native-mock --run-id release-check --project release-check --cwd "$ROOT_DIR" \
  --deadline-seconds 30 'release smoke')"
printf '%s\n' "$run_output" | grep -Eq '"state"[[:space:]]*:[[:space:]]*"done"' || \
  die "native-mock task did not finish"
result_output="$(SN_CLI_HOME="$runtime_home" "$install_dir/sn-cli" task result --run-id release-check)"
printf '%s\n' "$result_output" | grep -Eq '"outcome"[[:space:]]*:[[:space:]]*"succeeded"' || \
  die "native-mock result did not succeed"

log "[release-check] passed"
