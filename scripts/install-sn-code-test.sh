#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
binary="$ROOT_DIR/bin/sn-code"
installer="$ROOT_DIR/scripts/install-sn-code.sh"
test_root="$(mktemp -d)"
test_root="$(cd "$test_root" && pwd -P)"
trap 'rm -rf -- "$test_root"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
export SN_CLI_HOME="$test_root/home"
export SN_CODE_INSTALL_DIR="$test_root/commands"

fail() { printf 'sn-code install test: %s\n' "$*" >&2; exit 1; }
expect_failure() {
  if bash "$installer" "$binary" >"$test_root/failure.log" 2>&1; then
    fail "unsafe install unexpectedly succeeded"
  fi
  grep -F "$1" "$test_root/failure.log" >/dev/null || {
    cat "$test_root/failure.log" >&2
    fail "unexpected diagnostic"
  }
}

# 使用真实构建产物，安装与所有反例都限制在临时目录。
mkdir -p "$SN_CLI_HOME/configs" "$SN_CLI_HOME/state" "$SN_CLI_HOME/sncode/sessions"
printf 'preserve\n' >"$SN_CLI_HOME/configs/keep"
printf 'preserve\n' >"$SN_CLI_HOME/state/keep"
printf 'preserve\n' >"$SN_CLI_HOME/sncode/sessions/keep"
bash "$installer" "$binary"
[ -L "$SN_CODE_INSTALL_DIR/sn-code" ] || fail "command is not a symlink"
[ "$(readlink "$SN_CODE_INSTALL_DIR/sn-code")" = "$SN_CLI_HOME/bin/sn-code" ] || fail "wrong link target"
[ "$("$SN_CODE_INSTALL_DIR/sn-code" --version)" = "$("$binary" --version)" ] || fail "wrong version"
bash "$installer" "$binary"
for file in configs/keep state/keep sncode/sessions/keep; do
  [ "$(cat "$SN_CLI_HOME/$file")" = preserve ] || fail "modified unrelated state"
done
[ ! -e "$SN_CLI_HOME/bin/sn-cli" ] && [ ! -e "$SN_CLI_HOME/bin/sn-server" ] || fail "installed Runtime binaries"

rm "$SN_CODE_INSTALL_DIR/sn-code"
printf 'keep command\n' >"$SN_CODE_INSTALL_DIR/sn-code"
expect_failure "already exists and is not a symlink"
[ "$(cat "$SN_CODE_INSTALL_DIR/sn-code")" = "keep command" ] || fail "overwrote command"
cmp "$binary" "$SN_CLI_HOME/bin/sn-code"
rm "$SN_CODE_INSTALL_DIR/sn-code"
ln -s "$test_root/foreign" "$SN_CODE_INSTALL_DIR/sn-code"
expect_failure "command link points outside this Runtime home"
[ "$(readlink "$SN_CODE_INSTALL_DIR/sn-code")" = "$test_root/foreign" ] || fail "replaced foreign link"
rm "$SN_CODE_INSTALL_DIR/sn-code"

rm "$SN_CLI_HOME/bin/sn-code"
printf 'keep target\n' >"$test_root/foreign"
ln -s "$test_root/foreign" "$SN_CLI_HOME/bin/sn-code"
expect_failure "binary target must be a regular file"
[ "$(cat "$test_root/foreign")" = "keep target" ] || fail "overwrote symlink target"
rm "$SN_CLI_HOME/bin/sn-code"

touch "$SN_CLI_HOME/state/activation.journal.json"
expect_failure "requires activation recovery"
rm "$SN_CLI_HOME/state/activation.journal.json"

original_home="$SN_CLI_HOME"
ln -s "$original_home" "$test_root/home-link"
export SN_CLI_HOME="$test_root/home-link/"
expect_failure "runtime home must not be a symlink"
export SN_CLI_HOME="$original_home"
export SN_CODE_INSTALL_DIR="$SN_CLI_HOME/state/auth.json"
expect_failure "command directory must be outside"
[ ! -e "$SN_CODE_INSTALL_DIR" ] || fail "created a directory at the credentials path"
export SN_CODE_INSTALL_DIR="$test_root/commands"

# 安装路径含空格时仍保持参数边界。
export SN_CLI_HOME="$test_root/home with spaces"
export SN_CODE_INSTALL_DIR="$test_root/commands with spaces"
bash "$installer" "$binary"
"$SN_CODE_INSTALL_DIR/sn-code" --version
printf 'sn-code install tests passed\n'
