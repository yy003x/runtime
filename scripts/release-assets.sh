#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

GO="${GO:-go}"
GOCACHE="${GOCACHE:-$("$GO" env GOCACHE)}"
GOMODCACHE="${GOMODCACHE:-$("$GO" env GOMODCACHE)}"
SN_CLI_LDFLAGS="${SN_CLI_LDFLAGS:-}"

rm -rf dist
mkdir -p dist

for platform in darwin/arm64 darwin/amd64 linux/arm64 linux/amd64; do
  os="${platform%/*}"
  arch="${platform#*/}"
  stage="dist/.stage-$os-$arch"

  mkdir -p "$stage/configs" "$stage/resources"
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
  cp configs/*.json "$stage/configs/"
  cp configs/runtime/runtime.json "$stage/runtime.json"
  cp -R resources/. "$stage/resources/"
  COPYFILE_DISABLE=1 tar -czf "dist/sn-cli-$os-$arch.tar.gz" \
    -C "$stage" sn-cli sn-server configs runtime.json resources
  rm -rf "$stage"
done

cd dist
if command -v sha256sum >/dev/null 2>&1; then
  sha256sum sn-cli-*.tar.gz >checksums.txt
else
  shasum -a 256 sn-cli-*.tar.gz >checksums.txt
fi
