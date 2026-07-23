#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SOURCE_PUBLISH="$ROOT_DIR/scripts/publish.sh"
TEMP_ROOT="$(mktemp -d)"
trap 'rm -rf "$TEMP_ROOT"' EXIT

fail() {
  printf 'publish-test: %s\n' "$*" >&2
  exit 1
}

new_repo() {
  local name="$1"
  TEST_REPO="$TEMP_ROOT/$name/repo"
  TEST_HOME="$TEMP_ROOT/$name/home"
  TEST_LOG="$TEMP_ROOT/$name/release-version"
  mkdir -p "$TEST_REPO/scripts" "$TEST_HOME"
  cp "$SOURCE_PUBLISH" "$TEST_REPO/scripts/publish.sh"
  chmod +x "$TEST_REPO/scripts/publish.sh"
  cat >"$TEST_REPO/Makefile" <<'EOF'
.PHONY: release-check install

release-check:
	@test "$$FAIL_RELEASE_CHECK" != "1"
	@test -n "$(SN_CLI_VERSION)"
	@printf '%s\n' "$(SN_CLI_VERSION)" >"$$PUBLISH_TEST_LOG"

install:
	@test "$$FAIL_INSTALL" != "1"
	@mkdir -p "$$SN_CLI_HOME/bin"
	@tag="$$(git describe --tags --exact-match)"; \
	printf '%s\n' '#!/usr/bin/env bash' "printf '%s\\n' 'sn-cli $$tag (fixture)'" >"$$SN_CLI_HOME/bin/sn-cli"; \
	chmod +x "$$SN_CLI_HOME/bin/sn-cli"
EOF
  git -C "$TEST_REPO" init -q -b main
  git -C "$TEST_REPO" config user.name "Publish Test"
  git -C "$TEST_REPO" config user.email "publish-test@example.invalid"
  git -C "$TEST_REPO" add Makefile scripts/publish.sh
  git -C "$TEST_REPO" commit -q -m "fixture"
}

run_publish() {
  env \
    VERSION="$1" \
    SN_CLI_HOME="$TEST_HOME" \
    PUBLISH_TEST_LOG="$TEST_LOG" \
    "${@:2}" \
    bash "$TEST_REPO/scripts/publish.sh"
}

new_repo success
success_output="$(run_publish v1.2.3)"
[ "$(cat "$TEST_LOG")" = "v1.2.3" ] || fail "release-check 未收到目标版本"
[ "$(git -C "$TEST_REPO" cat-file -t refs/tags/v1.2.3)" = "tag" ] || fail "未创建 annotated tag"
[ -z "$(git -C "$TEST_REPO" status --porcelain)" ] || fail "成功流程污染工作区"
printf '%s\n' "$success_output" | grep -q '^push:    not performed$' || fail "成功输出未声明不 push"
"$TEST_HOME/bin/sn-cli" --version | grep -q '^sn-cli v1.2.3 ' || fail "安装版本不匹配"

new_repo invalid-version
if run_publish 1.2.3 >"$TEMP_ROOT/invalid-version.out" 2>&1; then
  fail "非法版本意外成功"
fi
grep -q 'VERSION 必须是 SemVer' "$TEMP_ROOT/invalid-version.out" || fail "非法版本错误不明确"

new_repo wrong-branch
git -C "$TEST_REPO" switch -q -c feature
if run_publish v1.2.4 >"$TEMP_ROOT/wrong-branch.out" 2>&1; then
  fail "错误分支意外成功"
fi
grep -q '必须在 main 分支发布' "$TEMP_ROOT/wrong-branch.out" || fail "错误分支提示不明确"

new_repo dirty
printf 'dirty\n' >"$TEST_REPO/untracked.txt"
if run_publish v1.2.4 >"$TEMP_ROOT/dirty.out" 2>&1; then
  fail "脏工作区意外成功"
fi
grep -q '工作区必须干净' "$TEMP_ROOT/dirty.out" || fail "脏工作区提示不明确"

new_repo existing-tag
git -C "$TEST_REPO" tag -a v1.2.4 -m "existing"
if run_publish v1.2.4 >"$TEMP_ROOT/existing-tag.out" 2>&1; then
  fail "重复 tag 意外成功"
fi
grep -q 'tag v1.2.4 已存在' "$TEMP_ROOT/existing-tag.out" || fail "重复 tag 提示不明确"

new_repo tagged-head
git -C "$TEST_REPO" tag -a v1.2.3 -m "existing head"
if run_publish v1.2.4 >"$TEMP_ROOT/tagged-head.out" 2>&1; then
  fail "已发布 HEAD 意外创建新版本"
fi
grep -q '当前 HEAD 已有 release tag v1.2.3' "$TEMP_ROOT/tagged-head.out" || {
  cat "$TEMP_ROOT/tagged-head.out" >&2
  fail "已发布 HEAD 提示不明确"
}

new_repo gate-failure
if run_publish v1.2.4 FAIL_RELEASE_CHECK=1 >"$TEMP_ROOT/gate-failure.out" 2>&1; then
  fail "release-check 失败后意外成功"
fi
if git -C "$TEST_REPO" show-ref --tags --verify --quiet refs/tags/v1.2.4; then
  fail "release-check 失败后创建了 tag"
fi

new_repo install-failure
if run_publish v1.2.4 FAIL_INSTALL=1 >"$TEMP_ROOT/install-failure.out" 2>&1; then
  fail "安装失败后意外成功"
fi
git -C "$TEST_REPO" show-ref --tags --verify --quiet refs/tags/v1.2.4 || fail "安装失败后未保留 tag"
grep -q 'tag v1.2.4 已创建但本地安装失败' "$TEMP_ROOT/install-failure.out" || fail "安装失败恢复提示不明确"

printf 'publish-test: passed\n'
