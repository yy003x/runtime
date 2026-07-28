#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TAG="${TAG:-}"

log() { printf '%s\n' "$*" >&2; }
die() { printf 'release: %s\n' "$*" >&2; exit 1; }

# shellcheck source-path=SCRIPTDIR
# shellcheck source=release-lib.sh
source "$ROOT_DIR/scripts/release-lib.sh"
require_release_repository

remote_refs="$(remote_tag_refs)" || die "读取 origin tag 列表失败"
remote_names="$(remote_tag_names "$remote_refs")"
local_names="$(git -C "$ROOT_DIR" tag --list)"

if [ -n "$TAG" ]; then
  is_semver_tag "$TAG" || die "TAG 必须是 SemVer，例如 TAG=v0.2.0"
else
  latest="$(printf '%s\n%s\n' "$local_names" "$remote_names" | latest_stable_tag || true)"
  TAG="$(next_patch_tag "$latest")"
fi

git -C "$ROOT_DIR" show-ref --tags --verify --quiet "refs/tags/$TAG" && die "本地 tag $TAG 已存在"
remote_tag_oid "$remote_refs" "$TAG" >/dev/null 2>&1 && die "远端 tag $TAG 已存在"

head_tags="$(git -C "$ROOT_DIR" tag --points-at HEAD --list 'v*')"
[ -z "$head_tags" ] || die "当前 HEAD 已有 release tag ${head_tags%%$'\n'*}；请先产生新的发布提交"

log "[release] tag=$TAG branch=$RELEASE_BRANCH commit=${RELEASE_HEAD:0:7}"
make --no-print-directory -C "$ROOT_DIR" \
  V="${V:-0}" release-check SN_CLI_VERSION="$TAG"
verify_repository_unchanged "release-check"

remote_refs_after="$(remote_tag_refs)" || die "release-check 后重新读取 origin tag 列表失败"
remote_tag_oid "$remote_refs_after" "$TAG" >/dev/null 2>&1 && die "release-check 期间远端已创建 tag $TAG，本地未打 tag"

git -C "$ROOT_DIR" tag -a "$TAG" -m "sn-cli $TAG"

printf 'release complete\n'
printf 'tag:     %s\n' "$TAG"
printf 'commit:  %s\n' "$RELEASE_HEAD"
printf 'assets:  %s\n' "$ROOT_DIR/dist"
printf 'install: not performed\n'
printf 'push:    not performed\n'
