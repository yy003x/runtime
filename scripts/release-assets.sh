#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

# shellcheck source=scripts/release-profile-files.sh
source "$ROOT_DIR/scripts/release-profile-files.sh"
# shellcheck source=scripts/release-tool-files.sh
source "$ROOT_DIR/scripts/release-tool-files.sh"

GO="${GO:-go}"
GOCACHE="${GOCACHE:-$("$GO" env GOCACHE)}"
GOMODCACHE="${GOMODCACHE:-$("$GO" env GOMODCACHE)}"
SN_CLI_LDFLAGS="${SN_CLI_LDFLAGS:-}"

for legacy_source in tools configs/runtime resources/tmux.conf resources/release.json; do
  if [ -e "$legacy_source" ] || [ -L "$legacy_source" ]; then
    printf 'release-assets: legacy source layout entry remains: %s\n' \
      "$legacy_source" >&2
    exit 1
  fi
done
[ -d resources/schema ] && [ ! -L resources/schema ] || {
  printf 'release-assets: resources/schema must be a directory, not a symlink\n' >&2
  exit 1
}
[ -d resources/tools ] && [ ! -L resources/tools ] || {
  printf 'release-assets: resources/tools must be a directory, not a symlink\n' >&2
  exit 1
}
[ -d release ] && [ ! -L release ] || {
  printf 'release-assets: release must be a directory, not a symlink\n' >&2
  exit 1
}
for tool in "${SN_CLI_RELEASE_TOOL_FILES[@]}"; do
  [ -f "resources/tools/$tool" ] && [ ! -L "resources/tools/$tool" ] || {
    printf 'release-assets: tool must be a regular file: %s\n' "$tool" >&2
    exit 1
  }
done
for release_file in runtime.json tmux.conf release.json; do
  [ -f "release/$release_file" ] && [ ! -L "release/$release_file" ] || {
    printf 'release-assets: release file must be regular: %s\n' "$release_file" >&2
    exit 1
  }
done

rm -rf dist
mkdir -p dist

for platform in darwin/arm64 darwin/amd64 linux/arm64 linux/amd64; do
  os="${platform%/*}"
  arch="${platform#*/}"
  stage="dist/.stage-$os-$arch"

  mkdir -p \
    "$stage/configs" \
    "$stage/resources/schema" \
    "$stage/resources/tools" \
    "$stage/release"
  env \
    GOCACHE="$GOCACHE" \
    GOMODCACHE="$GOMODCACHE" \
    CGO_ENABLED=0 \
    GOOS="$os" \
    GOARCH="$arch" \
    "$GO" build -ldflags "$SN_CLI_LDFLAGS" -o "$stage/sn-cli" ./cmd/sn-cli
  env \
    GOCACHE="$GOCACHE" \
    GOMODCACHE="$GOMODCACHE" \
    CGO_ENABLED=0 \
    GOOS="$os" \
    GOARCH="$arch" \
    "$GO" build -o "$stage/sn-server" ./cmd/sn-server
  for profile in "${SN_CLI_RELEASE_PROFILE_FILES[@]}"; do
    cp "configs/$profile" "$stage/configs/"
  done
  cp -R resources/schema/. "$stage/resources/schema/"
  for tool in "${SN_CLI_RELEASE_TOOL_FILES[@]}"; do
    cp "resources/tools/$tool" "$stage/resources/tools/"
  done
  cp release/runtime.json release/tmux.conf release/release.json "$stage/release/"
  COPYFILE_DISABLE=1 tar -czf "dist/sn-cli-$os-$arch.tar.gz" \
    -C "$stage" sn-cli sn-server configs resources release
  rm -rf "$stage"
done

cd dist
if command -v sha256sum >/dev/null 2>&1; then
  sha256sum sn-cli-*.tar.gz >checksums.txt
else
  shasum -a 256 sn-cli-*.tar.gz >checksums.txt
fi
