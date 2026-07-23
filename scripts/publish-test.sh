#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TEMP_ROOT="$(mktemp -d)"
trap 'rm -rf "$TEMP_ROOT"' EXIT

fail() {
  printf 'publish-test: %s\n' "$*" >&2
  exit 1
}

new_repo() {
  local name="$1"
  TEST_REPO="$TEMP_ROOT/$name/repo"
  TEST_REMOTE="$TEMP_ROOT/$name/origin.git"
  TEST_LOG="$TEMP_ROOT/$name/release-version"
  mkdir -p "$TEST_REPO/scripts"
  git init -q --bare "$TEST_REMOTE"
  cp "$ROOT_DIR/scripts/release-lib.sh" "$ROOT_DIR/scripts/release.sh" "$ROOT_DIR/scripts/publish.sh" "$TEST_REPO/scripts/"
  chmod +x "$TEST_REPO/scripts/release-lib.sh" "$TEST_REPO/scripts/release.sh" "$TEST_REPO/scripts/publish.sh"
  cat >"$TEST_REPO/Makefile" <<'EOF'
.PHONY: release-check

release-check:
	@test "$$FAIL_RELEASE_CHECK" != "1"
	@test -n "$(SN_CLI_VERSION)"
	@printf '%s\n' "$(SN_CLI_VERSION)" >"$$RELEASE_TEST_LOG"
EOF
  git -C "$TEST_REPO" init -q -b main
  git -C "$TEST_REPO" config user.name "Release Test"
  git -C "$TEST_REPO" config user.email "release-test@example.invalid"
  git -C "$TEST_REPO" add Makefile scripts/
  git -C "$TEST_REPO" commit -q -m "fixture"
  git -C "$TEST_REPO" remote add origin "$TEST_REMOTE"
  git -C "$TEST_REPO" push -q -u origin main
}

commit_change() {
  printf '%s\n' "$1" >>"$TEST_REPO/change.txt"
  git -C "$TEST_REPO" add change.txt
  git -C "$TEST_REPO" commit -q -m "$1"
}

run_release() {
  env TAG="$1" RELEASE_TEST_LOG="$TEST_LOG" "${@:2}" bash "$TEST_REPO/scripts/release.sh"
}

run_publish() {
  env TAG="$1" "${@:2}" bash "$TEST_REPO/scripts/publish.sh"
}

new_repo auto-release
git -C "$TEST_REPO" tag -a v0.1.0 -m "v0.1.0"
git -C "$TEST_REPO" push -q origin refs/tags/v0.1.0
commit_change "next release"
release_output="$(run_release "")"
[ "$(cat "$TEST_LOG")" = "v0.1.1" ] || fail "自动 release 未生成 v0.1.1"
[ "$(git -C "$TEST_REPO" cat-file -t refs/tags/v0.1.1)" = "tag" ] || fail "release 未创建 annotated tag"
git --git-dir="$TEST_REMOTE" show-ref --tags --verify --quiet refs/tags/v0.1.1 && fail "release 意外 push tag"
printf '%s\n' "$release_output" | grep -q '^push:    not performed$' || fail "release 输出未声明不 push"

new_repo local-tag-release
git -C "$TEST_REPO" tag -a v0.3.0 -m "local only"
commit_change "next local release"
run_release "" >/dev/null
[ "$(cat "$TEST_LOG")" = "v0.3.1" ] || fail "release 未合并本地 tag 计算版本"

new_repo explicit-release
run_release v0.2.0 >/dev/null
[ "$(cat "$TEST_LOG")" = "v0.2.0" ] || fail "release 未使用显式 TAG"

new_repo invalid-release
if run_release 0.2.0 >"$TEMP_ROOT/invalid-release.out" 2>&1; then
  fail "非法 release TAG 意外成功"
fi
grep -q 'TAG 必须是 SemVer' "$TEMP_ROOT/invalid-release.out" || fail "非法 release TAG 提示不明确"

new_repo dirty-release
printf 'dirty\n' >"$TEST_REPO/untracked.txt"
if run_release "" >"$TEMP_ROOT/dirty-release.out" 2>&1; then
  fail "脏工作区 release 意外成功"
fi
grep -q '工作区必须干净' "$TEMP_ROOT/dirty-release.out" || fail "脏工作区提示不明确"

new_repo release-gate-failure
if run_release "" FAIL_RELEASE_CHECK=1 >"$TEMP_ROOT/release-gate-failure.out" 2>&1; then
  fail "release-check 失败后 release 意外成功"
fi
git -C "$TEST_REPO" tag --list | grep -q . && fail "release-check 失败后创建了 tag"

new_repo auto-publish
commit_change "publish release"
git -C "$TEST_REPO" tag -a v1.2.3 -m "v1.2.3"
publish_output="$(run_publish "")"
[ "$(git --git-dir="$TEST_REMOTE" rev-parse refs/heads/main)" = "$(git -C "$TEST_REPO" rev-parse HEAD)" ] || fail "publish 未更新远端 main"
[ "$(git --git-dir="$TEST_REMOTE" rev-parse refs/tags/v1.2.3)" = "$(git -C "$TEST_REPO" rev-parse refs/tags/v1.2.3)" ] || fail "publish 未推送 tag"
printf '%s\n' "$publish_output" | grep -q '^push:    main + tag (atomic)$' || fail "publish 输出未声明 atomic push"

new_repo no-tag-publish
if run_publish "" >"$TEMP_ROOT/no-tag-publish.out" 2>&1; then
  fail "没有 tag 时 publish 意外成功"
fi
grep -q '请先执行 make release' "$TEMP_ROOT/no-tag-publish.out" || fail "缺少 release tag 提示不明确"

new_repo lightweight-publish
git -C "$TEST_REPO" tag v1.2.3
if run_publish v1.2.3 >"$TEMP_ROOT/lightweight-publish.out" 2>&1; then
  fail "lightweight tag publish 意外成功"
fi
grep -q '不是 annotated tag' "$TEMP_ROOT/lightweight-publish.out" || fail "lightweight tag 提示不明确"

new_repo old-tag-publish
git -C "$TEST_REPO" tag -a v1.2.3 -m "old"
commit_change "after tag"
if run_publish v1.2.3 >"$TEMP_ROOT/old-tag-publish.out" 2>&1; then
  fail "旧 commit tag publish 意外成功"
fi
grep -q '未指向当前 HEAD' "$TEMP_ROOT/old-tag-publish.out" || fail "旧 tag 提示不明确"

new_repo conflicting-remote-tag
git -C "$TEST_REPO" tag -a v1.2.3 -m "remote"
git -C "$TEST_REPO" push -q origin refs/tags/v1.2.3
git -C "$TEST_REPO" tag -d v1.2.3 >/dev/null
commit_change "conflicting local tag"
git -C "$TEST_REPO" tag -a v1.2.3 -m "local"
if run_publish v1.2.3 >"$TEMP_ROOT/conflicting-remote-tag.out" 2>&1; then
  fail "冲突远端 tag publish 意外成功"
fi
grep -q '远端 tag v1.2.3 与本地 tag 不一致' "$TEMP_ROOT/conflicting-remote-tag.out" || fail "远端 tag 冲突提示不明确"

printf 'publish-test: passed\n'
