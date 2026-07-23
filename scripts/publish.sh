#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TAG="${TAG:-}"

log() { printf '%s\n' "$*" >&2; }
die() { printf 'publish: %s\n' "$*" >&2; exit 1; }

# shellcheck source-path=SCRIPTDIR
# shellcheck source=release-lib.sh
source "$ROOT_DIR/scripts/release-lib.sh"
require_release_repository

if [ -n "$TAG" ]; then
  is_semver_tag "$TAG" || die "TAG 必须是 SemVer，例如 TAG=v0.2.0"
else
  head_tags="$(git -C "$ROOT_DIR" tag --points-at HEAD --list 'v*')"
  TAG="$(printf '%s\n' "$head_tags" | latest_stable_tag || true)"
  [ -n "$TAG" ] || die "当前 HEAD 没有稳定 SemVer release tag；请先执行 make release"
fi

git -C "$ROOT_DIR" show-ref --tags --verify --quiet "refs/tags/$TAG" || die "本地 tag $TAG 不存在"
[ "$(git -C "$ROOT_DIR" cat-file -t "refs/tags/$TAG")" = "tag" ] || die "tag $TAG 不是 annotated tag"
tag_commit="$(git -C "$ROOT_DIR" rev-list -n 1 "$TAG")"
[ "$tag_commit" = "$RELEASE_HEAD" ] || die "tag $TAG 未指向当前 HEAD"

remote_refs="$(remote_tag_refs)" || die "读取 origin tag 列表失败"
local_tag_oid="$(git -C "$ROOT_DIR" rev-parse "refs/tags/$TAG")"
existing_remote_oid="$(remote_tag_oid "$remote_refs" "$TAG" || true)"
if [ -n "$existing_remote_oid" ] && [ "$existing_remote_oid" != "$local_tag_oid" ]; then
  die "远端 tag $TAG 与本地 tag 不一致，拒绝覆盖"
fi

verify_repository_unchanged "publish preflight"
log "[publish] pushing branch=main tag=$TAG commit=${RELEASE_HEAD:0:7}"
git -C "$ROOT_DIR" push --atomic origin \
  "refs/heads/main:refs/heads/main" \
  "refs/tags/$TAG:refs/tags/$TAG"

remote_refs_after="$(remote_tag_refs)" || die "push 后读取 origin tag 列表失败"
[ "$(remote_tag_oid "$remote_refs_after" "$TAG" || true)" = "$local_tag_oid" ] || die "远端 tag $TAG 回读不一致"
remote_main_oid="$(git -C "$ROOT_DIR" ls-remote --heads origin refs/heads/main | awk 'NR == 1 {print $1}')"
[ "$remote_main_oid" = "$RELEASE_HEAD" ] || die "远端 main 回读不一致"

printf 'publish complete\n'
printf 'tag:     %s\n' "$TAG"
printf 'commit:  %s\n' "$RELEASE_HEAD"
printf 'remote:  origin\n'
printf 'push:    main + tag (atomic)\n'
