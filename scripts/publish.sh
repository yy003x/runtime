#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
VERSION="${VERSION:-}"
EXPECTED_BRANCH="main"

log() { printf '%s\n' "$*" >&2; }
die() { printf 'publish: %s\n' "$*" >&2; exit 1; }

if [[ ! "$VERSION" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$ ]]; then
  die "VERSION 必须是 SemVer Git tag，例如 make publish VERSION=v0.1.1"
fi

command -v git >/dev/null 2>&1 || die "git is required"
command -v make >/dev/null 2>&1 || die "make is required"
git -C "$ROOT_DIR" rev-parse --is-inside-work-tree >/dev/null 2>&1 || die "$ROOT_DIR 不是 Git 仓库"

branch="$(git -C "$ROOT_DIR" symbolic-ref --quiet --short HEAD 2>/dev/null || true)"
if [ "$branch" != "$EXPECTED_BRANCH" ]; then
  die "必须在 $EXPECTED_BRANCH 分支发布，当前分支为 ${branch:-detached HEAD}"
fi

if [ -n "$(git -C "$ROOT_DIR" status --porcelain)" ]; then
  die "工作区必须干净；请先提交或处理所有改动"
fi

if git -C "$ROOT_DIR" show-ref --tags --verify --quiet "refs/tags/$VERSION"; then
  die "tag $VERSION 已存在"
fi

head_tags="$(git -C "$ROOT_DIR" tag --points-at HEAD --list 'v[0-9]*')"
if [ -n "$head_tags" ]; then
  head_tag="${head_tags%%$'\n'*}"
  die "当前 HEAD 已有 release tag ${head_tag}；请先产生新的发布提交"
fi

head_commit="$(git -C "$ROOT_DIR" rev-parse HEAD)"
log "[publish] preflight version=$VERSION branch=$branch commit=${head_commit:0:7}"
make -C "$ROOT_DIR" release-check SN_CLI_VERSION="$VERSION"

if [ "$(git -C "$ROOT_DIR" rev-parse HEAD)" != "$head_commit" ]; then
  die "release-check 期间 HEAD 发生变化，未创建 tag"
fi
if [ "$(git -C "$ROOT_DIR" symbolic-ref --quiet --short HEAD 2>/dev/null || true)" != "$branch" ]; then
  die "release-check 期间分支发生变化，未创建 tag"
fi
if [ -n "$(git -C "$ROOT_DIR" status --porcelain)" ]; then
  die "release-check 后工作区不再干净，未创建 tag"
fi

git -C "$ROOT_DIR" tag -a "$VERSION" -m "sn-cli $VERSION"
log "[publish] created local annotated tag $VERSION"

if ! make -C "$ROOT_DIR" install; then
  die "tag $VERSION 已创建但本地安装失败；修复后运行 make install，或确认 tag 未 push 后运行 git tag -d $VERSION"
fi

if [ -n "${SN_CLI_HOME:-}" ]; then
  runtime_home="$SN_CLI_HOME"
elif [ -n "${HOME:-}" ]; then
  runtime_home="$HOME/.sn"
else
  die "tag $VERSION 已创建，但 SN_CLI_HOME 与 HOME 均未设置，无法验证安装结果"
fi
installed_binary="$runtime_home/bin/sn-cli"
if [ ! -x "$installed_binary" ]; then
  die "tag $VERSION 已创建，但未找到已安装 binary：$installed_binary"
fi
installed_version="$("$installed_binary" --version)"
if [[ "$installed_version" != "sn-cli $VERSION" && "$installed_version" != "sn-cli $VERSION ("* ]]; then
  die "tag $VERSION 已创建，但安装版本不匹配：$installed_version"
fi

printf 'publish complete\n'
printf 'version: %s\n' "$VERSION"
printf 'commit:  %s\n' "$head_commit"
printf 'tag:     local annotated tag\n'
printf 'binary:  %s\n' "$installed_binary"
printf 'verify:  %s\n' "$installed_version"
printf 'push:    not performed\n'
