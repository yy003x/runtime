#!/usr/bin/env bash

is_semver_tag() {
  [[ "$1" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$ ]]
}

is_stable_semver_tag() {
  [[ "$1" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]
}

latest_stable_tag() {
  local tag major minor patch
  local found=0 best_major=0 best_minor=0 best_patch=0
  while IFS= read -r tag; do
    if ! is_stable_semver_tag "$tag"; then
      continue
    fi
    major="${BASH_REMATCH[1]}"
    minor="${BASH_REMATCH[2]}"
    patch="${BASH_REMATCH[3]}"
    if [ "$found" -eq 0 ] ||
      [ "$major" -gt "$best_major" ] ||
      { [ "$major" -eq "$best_major" ] && [ "$minor" -gt "$best_minor" ]; } ||
      { [ "$major" -eq "$best_major" ] && [ "$minor" -eq "$best_minor" ] && [ "$patch" -gt "$best_patch" ]; }; then
      found=1
      best_major="$major"
      best_minor="$minor"
      best_patch="$patch"
    fi
  done
  [ "$found" -eq 1 ] || return 1
  printf 'v%s.%s.%s\n' "$best_major" "$best_minor" "$best_patch"
}

next_patch_tag() {
  local latest="$1"
  if [ -z "$latest" ]; then
    printf 'v0.1.0\n'
    return
  fi
  if ! is_stable_semver_tag "$latest"; then
    return 1
  fi
  printf 'v%s.%s.%s\n' "${BASH_REMATCH[1]}" "${BASH_REMATCH[2]}" "$((BASH_REMATCH[3] + 1))"
}

remote_tag_refs() {
  git -C "$ROOT_DIR" ls-remote --tags --refs origin
}

remote_tag_names() {
  local oid ref
  while IFS=$'\t' read -r oid ref; do
    [ -n "$ref" ] || continue
    printf '%s\n' "${ref#refs/tags/}"
  done <<<"$1"
}

remote_tag_oid() {
  local oid ref target="$2"
  while IFS=$'\t' read -r oid ref; do
    if [ "$ref" = "refs/tags/$target" ]; then
      printf '%s\n' "$oid"
      return 0
    fi
  done <<<"$1"
  return 1
}

require_release_repository() {
  command -v git >/dev/null 2>&1 || die "git is required"
  command -v make >/dev/null 2>&1 || die "make is required"
  git -C "$ROOT_DIR" rev-parse --is-inside-work-tree >/dev/null 2>&1 || die "$ROOT_DIR 不是 Git 仓库"
  git -C "$ROOT_DIR" remote get-url origin >/dev/null 2>&1 || die "origin remote 不存在"

  RELEASE_BRANCH="$(git -C "$ROOT_DIR" symbolic-ref --quiet --short HEAD 2>/dev/null || true)"
  [ "$RELEASE_BRANCH" = "main" ] || die "必须在 main 分支操作，当前分支为 ${RELEASE_BRANCH:-detached HEAD}"
  [ -z "$(git -C "$ROOT_DIR" status --porcelain)" ] || die "工作区必须干净；请先提交或处理所有改动"
  RELEASE_HEAD="$(git -C "$ROOT_DIR" rev-parse HEAD)"
}

verify_repository_unchanged() {
  [ "$(git -C "$ROOT_DIR" rev-parse HEAD)" = "$RELEASE_HEAD" ] || die "$1 期间 HEAD 发生变化"
  [ "$(git -C "$ROOT_DIR" symbolic-ref --quiet --short HEAD 2>/dev/null || true)" = "$RELEASE_BRANCH" ] || die "$1 期间分支发生变化"
  [ -z "$(git -C "$ROOT_DIR" status --porcelain)" ] || die "$1 后工作区不再干净"
}
