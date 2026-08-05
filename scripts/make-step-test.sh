#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RUNNER="$ROOT_DIR/scripts/make-step.sh"
scratch="$(mktemp -d "${TMPDIR:-/tmp}/sn-runtime-make-step-test.XXXXXX")"
runner_tmp="$scratch/runner-tmp"
mkdir -p "$runner_tmp"
test_child_pid=""

cleanup() {
  if [[ -n "$test_child_pid" ]] && kill -0 "$test_child_pid" 2>/dev/null; then
    kill "$test_child_pid" 2>/dev/null || true
    wait "$test_child_pid" 2>/dev/null || true
  fi
  rm -rf -- "$scratch"
}
trap cleanup EXIT

fail() {
  printf 'make-step-test: %s\n' "$*" >&2
  exit 1
}

run_isolated_make() {
  env \
    -u MAKEFLAGS \
    -u MFLAGS \
    -u MAKEOVERRIDES \
    -u MAKELEVEL \
    make --no-print-directory -C "$ROOT_DIR" "$@"
}

TMPDIR="$runner_tmp" V=0 bash "$RUNNER" \
  --stage "quiet build" \
  --meta "output=bin/sn cli" \
  -- bash -c 'printf "hidden-success-stdout\n"; printf "hidden-success-stderr\n" >&2' \
  >"$scratch/success.stdout" 2>"$scratch/success.stderr"

[[ ! -s "$scratch/success.stdout" ]] || fail "quiet success leaked stdout"
grep -Fq 'stage=quiet\ build state=started output=bin/sn\ cli' "$scratch/success.stderr" ||
  fail "quiet success did not report start metadata"
grep -Fq 'state=completed result=success' "$scratch/success.stderr" ||
  fail "quiet success did not report completion"
if grep -Fq 'hidden-success' "$scratch/success.stderr"; then
  fail "quiet success leaked child output"
fi

set +e
TMPDIR="$runner_tmp" V=0 bash "$RUNNER" \
  --stage failure \
  -- bash -c 'printf "failure-stdout\n"; printf "failure-stderr\n" >&2; exit 7' \
  >"$scratch/failure.stdout" 2>"$scratch/failure.stderr"
failure_status=$?
set -e

[[ "$failure_status" -eq 7 ]] || fail "failure exit code was $failure_status, want 7"
[[ ! -s "$scratch/failure.stdout" ]] || fail "quiet failure leaked stdout"
[[ "$(grep -Fc 'failure-stdout' "$scratch/failure.stderr")" -eq 1 ]] ||
  fail "failure stdout was not replayed exactly once"
[[ "$(grep -Fc 'failure-stderr' "$scratch/failure.stderr")" -eq 1 ]] ||
  fail "failure stderr was not replayed exactly once"
grep -Fq 'state=failed result=failure' "$scratch/failure.stderr" ||
  fail "failure result was not reported"
grep -Fq 'exit=7' "$scratch/failure.stderr" ||
  fail "failure exit code was not reported"

TMPDIR="$runner_tmp" V=1 bash "$RUNNER" \
  --stage verbose \
  -- bash -c 'printf "live-stdout\n"; printf "live-stderr\n" >&2' \
  >"$scratch/verbose.stdout" 2>"$scratch/verbose.stderr"

grep -Fq 'live-stdout' "$scratch/verbose.stdout" || fail "V=1 did not stream stdout"
grep -Fq 'live-stderr' "$scratch/verbose.stderr" || fail "V=1 did not stream stderr"
grep -Fq 'command=bash -c' "$scratch/verbose.stderr" ||
  fail "V=1 did not print the quoted command"

dangerous_arg="space 'quote' \$(touch '$scratch/injected-command') ; touch '$scratch/injected-semicolon'"
printf -v quoted_dangerous_arg '%q' "$dangerous_arg"
TMPDIR="$runner_tmp" V=1 bash "$RUNNER" \
  --stage argv-safety \
  -- /usr/bin/printf '%s\n' "$dangerous_arg" \
  >"$scratch/argv-safety.stdout" 2>"$scratch/argv-safety.stderr"

printf '%s\n' "$dangerous_arg" >"$scratch/argv-safety.expected"
cmp -s "$scratch/argv-safety.expected" "$scratch/argv-safety.stdout" ||
  fail "V=1 changed a literal argv token"
grep -Fq "$quoted_dangerous_arg" "$scratch/argv-safety.stderr" ||
  fail "V=1 did not shell-quote a literal argv token"
[[ ! -e "$scratch/injected-command" ]] || fail "command substitution text was executed"
[[ ! -e "$scratch/injected-semicolon" ]] || fail "semicolon text was executed"

make_shell_marker="$scratch/make-shell-marker"
shell_command_marker="$scratch/shell-command-marker"
semicolon_marker="$scratch/semicolon-marker"
dangerous_make_value="space 'single quote' \"double quote\" ; touch '$semicolon_marker' ; \$(touch '$shell_command_marker') ; \$(shell touch '$make_shell_marker')"
make_variables=(
  APP_NAME
  SERVER_ADDR
  TAG
  GO
  GOCACHE
  GOMODCACHE
  SN_CLI_COMMIT
  SN_CLI_TAG
  SN_CLI_DIRTY
  SN_CLI_BUILDDATE
  SN_CLI_VERSION
  SN_CLI_LDFLAGS
  COVERAGE_PROFILE
  COVERAGE_MIN
)
make_arguments=()
for variable_name in "${make_variables[@]}"; do
  make_arguments+=("$variable_name=$dangerous_make_value")
done

run_isolated_make V=1 _make-variable-probe \
  "${make_arguments[@]}" \
  "MAKE_STEP=$dangerous_make_value" \
  "SHELL=$dangerous_make_value" \
  "RUNTIME_ROOT=$dangerous_make_value" \
  >"$scratch/make-variable.stdout" 2>"$scratch/make-variable.stderr"

for variable_name in "${make_variables[@]}"; do
  grep -Fxq "$variable_name=$dangerous_make_value" "$scratch/make-variable.stdout" ||
    fail "Make changed or interpreted $variable_name"
done
grep -Fxq "RUNTIME_ROOT=$ROOT_DIR" "$scratch/make-variable.stdout" ||
  fail "Make allowed RUNTIME_ROOT to override the repository root"

(
  unset SN_CLI_LDFLAGS
  run_isolated_make _make-variable-probe \
    "SN_CLI_VERSION=$dangerous_make_value"
) >"$scratch/make-derived-ldflags.stdout" 2>"$scratch/make-derived-ldflags.stderr"
grep -Fxq "SN_CLI_VERSION=$dangerous_make_value" "$scratch/make-derived-ldflags.stdout" ||
  fail "Make interpreted SN_CLI_VERSION before deriving ldflags"
grep -Fq "Version=$dangerous_make_value" "$scratch/make-derived-ldflags.stdout" ||
  fail "Make changed SN_CLI_VERSION while deriving ldflags"

(
  unset SN_CLI_VERSION SN_CLI_LDFLAGS
  run_isolated_make _make-variable-probe \
    "SN_CLI_TAG=$dangerous_make_value"
) >"$scratch/make-derived-version.stdout" 2>"$scratch/make-derived-version.stderr"
grep -Fxq "SN_CLI_VERSION=$dangerous_make_value" "$scratch/make-derived-version.stdout" ||
  fail "Make interpreted SN_CLI_TAG while deriving the default version"

run_isolated_make help \
  "SERVER_ADDR=$dangerous_make_value" \
  "COVERAGE_MIN=$dangerous_make_value" \
  >"$scratch/make-help.stdout" 2>"$scratch/make-help.stderr"
grep -Fq "server address: $dangerous_make_value" "$scratch/make-help.stdout" ||
  fail "Make help interpreted SERVER_ADDR"
grep -Fq "coverage minimum: $dangerous_make_value%" "$scratch/make-help.stdout" ||
  fail "Make help interpreted COVERAGE_MIN"

run_isolated_make -n install \
  >"$scratch/make-install-dry-run.stdout" \
  2>"$scratch/make-install-dry-run.stderr"
grep -Fq -- '--local-source-install' "$scratch/make-install-dry-run.stdout" ||
  fail "make install did not select local source install mode"
grep -Fq 'local_source_install=1' "$scratch/make-install-dry-run.stdout" ||
  fail "make install did not report local source install metadata"
grep -Fq -- '--release' "$scratch/make-install-dry-run.stdout" ||
  fail "make install did not pass the release source directory"
if grep -Fq 'SN_CLI_OVERWRITE_CONFIGS' "$scratch/make-install-dry-run.stdout" ||
  grep -Fq -- '--overwrite-configs' "$scratch/make-install-dry-run.stdout" ||
  grep -Fq -- '--tools' "$scratch/make-install-dry-run.stdout" ||
  grep -Fq -- '--runtime-config' "$scratch/make-install-dry-run.stdout"; then
  fail "make install retained a removed install control"
fi

for removed_option in --tools --runtime-config; do
  set +e
  bash "$ROOT_DIR/install.sh" "$removed_option" "$scratch/removed" --dry-run \
    >"$scratch/removed-option.stdout" 2>"$scratch/removed-option.stderr"
  removed_option_status=$?
  set -e
  [[ "$removed_option_status" -ne 0 ]] ||
    fail "install accepted removed option $removed_option"
  grep -Fq "unknown option: $removed_option" "$scratch/removed-option.stderr" ||
    fail "install returned the wrong error for removed option $removed_option"
done

local_bundle="$scratch/local-bundle"
local_install_home="$scratch/local-home"
local_install_bin="$scratch/local-bin"
local_candidate_args="$scratch/local-candidate.args"
mkdir -p \
  "$local_bundle/configs" \
  "$local_bundle/resources/schema" \
  "$local_bundle/resources/tools" \
  "$local_bundle/release" \
  "$local_install_bin"
printf '%s\n' \
  '#!/usr/bin/env bash' \
  "printf '%s\\n' \"\$@\" >\"\$LOCAL_CANDIDATE_ARGS\"" \
  >"$local_bundle/sn-cli"
printf '%s\n' '#!/usr/bin/env bash' 'exit 0' >"$local_bundle/sn-server"
chmod 755 "$local_bundle/sn-cli" "$local_bundle/sn-server"
printf '%s\n' '{"type":"cli","command":"codex"}' \
  >"$local_bundle/configs/cx.json"
printf '%s\n' '{"schema_version":1,"name":"web_search"}' \
  >"$local_bundle/resources/tools/web_search.json"
printf '%s\n' '{}' >"$local_bundle/release/runtime.json"
printf '%s\n' '{}' >"$local_bundle/release/release.json"
printf '%s\n' 'set-option -g exit-empty on' >"$local_bundle/release/tmux.conf"
local_source_args=(
  --binary "$local_bundle/sn-cli"
  --server "$local_bundle/sn-server"
  --configs "$local_bundle/configs"
  --resources "$local_bundle/resources"
  --release "$local_bundle/release"
  --home "$local_install_home"
  --install-dir "$local_install_bin"
)

bash "$ROOT_DIR/install.sh" \
  "${local_source_args[@]}" \
  --local-source-install \
  --dry-run \
  >"$scratch/local-source-dry-run.stdout" \
  2>"$scratch/local-source-dry-run.stderr"
grep -Fq 'local source install: 1' "$scratch/local-source-dry-run.stderr" ||
  fail "local source install dry-run did not report its mode"
grep -Fq 'overwrite configs: 1' "$scratch/local-source-dry-run.stderr" ||
  fail "local source install did not fix overwrite mode internally"
grep -Fq 'overwrite tools: 1' "$scratch/local-source-dry-run.stderr" ||
  fail "local source install did not fix Tool overwrite mode internally"
grep -Fq "source release: $local_bundle/release" \
  "$scratch/local-source-dry-run.stderr" ||
  fail "local source install did not report the release source directory"

set +e
bash "$ROOT_DIR/install.sh" \
  "${local_source_args[@]}" \
  --local-source-install \
  --overwrite-configs \
  --dry-run \
  >"$scratch/local-source-conflict.stdout" \
  2>"$scratch/local-source-conflict.stderr"
local_source_conflict_status=$?
set -e
[[ "$local_source_conflict_status" -ne 0 ]] ||
  fail "local source install accepted explicit --overwrite-configs"
grep -Fq 'cannot be combined with --overwrite-configs' \
  "$scratch/local-source-conflict.stderr" ||
  fail "local source install returned the wrong overwrite conflict"

for forbidden_option in archive checksums version; do
  case "$forbidden_option" in
    archive) forbidden_args=(--archive "$scratch/source.tar.gz") ;;
    checksums) forbidden_args=(--checksums "$scratch/checksums.txt") ;;
    version) forbidden_args=(--version v0.0.0-test) ;;
  esac
  set +e
  bash "$ROOT_DIR/install.sh" \
    "${local_source_args[@]}" \
    --local-source-install \
    "${forbidden_args[@]}" \
    --dry-run \
    >"$scratch/local-source-$forbidden_option.stdout" \
    2>"$scratch/local-source-$forbidden_option.stderr"
  forbidden_status=$?
  set -e
  [[ "$forbidden_status" -ne 0 ]] ||
    fail "local source install accepted --$forbidden_option"
  grep -Fq 'cannot be combined with --archive, --checksums, or --version' \
    "$scratch/local-source-$forbidden_option.stderr" ||
    fail "local source install returned the wrong --$forbidden_option conflict"
done

set +e
bash "$ROOT_DIR/install.sh" \
  --local-source-install \
  --home "$local_install_home" \
  --install-dir "$local_install_bin" \
  --dry-run \
  >"$scratch/local-source-network.stdout" \
  2>"$scratch/local-source-network.stderr"
local_source_network_status=$?
set -e
[[ "$local_source_network_status" -ne 0 ]] ||
  fail "network install accepted local source mode"
grep -Fq 'requires the complete --binary source bundle' \
  "$scratch/local-source-network.stderr" ||
  fail "network install returned the wrong local source mode error"

LOCAL_CANDIDATE_ARGS="$local_candidate_args" \
  bash "$ROOT_DIR/install.sh" \
  "${local_source_args[@]}" \
  --local-source-install \
  >"$scratch/local-source-install.stdout" \
  2>"$scratch/local-source-install.stderr"
if grep -Fxq -- '--overwrite-configs' "$local_candidate_args"; then
  fail "local source install passed a conflicting overwrite flag to candidate"
fi
grep -Fxq -- '--local-source-install' "$local_candidate_args" ||
  fail "local source install did not pass its private candidate flag"

set +e
run_isolated_make sn-cli-build \
  "GO=$dangerous_make_value" \
  >"$scratch/make-build.stdout" 2>"$scratch/make-build.stderr"
make_build_status=$?
set -e
[[ "$make_build_status" -ne 0 ]] || fail "malicious GO unexpectedly executed as a valid compiler"
grep -Fq 'state=failed result=failure' "$scratch/make-build.stderr" ||
  fail "malicious GO did not fail through the Make runner contract"

for marker in "$make_shell_marker" "$shell_command_marker" "$semicolon_marker"; do
  [[ ! -e "$marker" ]] || fail "Make variable text executed: $marker"
done

grep -Eq 'find .* configs resources release' "$ROOT_DIR/scripts/dev.sh" ||
  fail "dev source signature does not include the release directory"
grep -Fq -- "-name '*.conf'" "$ROOT_DIR/scripts/dev.sh" ||
  fail "dev source signature does not include fixed release config files"

GO=/usr/bin/false \
  GOCACHE="$scratch/go cache" \
  GOMODCACHE="$scratch/go module cache" \
  SERVER_ADDR="$dangerous_make_value" \
  bash "$ROOT_DIR/scripts/dev.sh" \
  >"$scratch/dev-failure.stdout" 2>"$scratch/dev-failure.stderr" &
test_child_pid=$!
for _ in {1..50}; do
  process_state="$(ps -o stat= -p "$test_child_pid" 2>/dev/null || true)"
  if [[ -z "$process_state" || "$process_state" == Z* ]]; then
    break
  fi
  sleep 0.1
done
process_state="$(ps -o stat= -p "$test_child_pid" 2>/dev/null || true)"
[[ -z "$process_state" || "$process_state" == Z* ]] ||
  fail "dev.sh stayed alive after its go process failed"
set +e
wait "$test_child_pid"
dev_failure_status=$?
set -e
test_child_pid=""
[[ "$dev_failure_status" -ne 0 ]] || fail "dev.sh converted a child failure to success"
grep -Fq 'server process exited unexpectedly exit=1' "$scratch/dev-failure.stderr" ||
  fail "dev.sh did not report the failed go process"
for marker in "$make_shell_marker" "$shell_command_marker" "$semicolon_marker"; do
  [[ ! -e "$marker" ]] || fail "dev variable text executed: $marker"
done

if [[ -n "$(find "$runner_tmp" -mindepth 1 -print -quit)" ]]; then
  fail "runner temporary files were not cleaned"
fi

printf 'make-step tests passed\n'
