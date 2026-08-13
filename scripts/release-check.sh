#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DIST_DIR="$ROOT_DIR/dist"
RELEASE_VERSION="${SN_CLI_VERSION:-release-check}"
GO_BIN="${GO:-go}"

# shellcheck source=scripts/release-profile-files.sh
source "$ROOT_DIR/scripts/release-profile-files.sh"
# shellcheck source=scripts/release-tool-files.sh
source "$ROOT_DIR/scripts/release-tool-files.sh"
# shellcheck source=scripts/release-check-source.sh
source "$ROOT_DIR/scripts/release-check-source.sh"
# shellcheck source=scripts/release-check-assets.sh
source "$ROOT_DIR/scripts/release-check-assets.sh"
# shellcheck source=scripts/release-check-installer.sh
source "$ROOT_DIR/scripts/release-check-installer.sh"

log() { printf '%s\n' "$*" >&2; }
die() { printf 'release-check: %s\n' "$*" >&2; exit 1; }
run_make() {
  make --no-print-directory -C "$ROOT_DIR" V="${V:-0}" "$@"
}
replay_logs() {
  local file
  for file in "$@"; do
    if [ -s "$file" ]; then
      log "[release-check] output: $file"
      sed -n '1,400p' "$file" >&2
    fi
  done
}
require_command() {
  command -v "$1" >/dev/null 2>&1 || die "$1 is required"
}
sha256_file() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    require_command shasum
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

if [[ ! "$RELEASE_VERSION" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$ ]]; then
  die "SN_CLI_VERSION must be a SemVer Git tag such as v0.1.0: $RELEASE_VERSION"
fi

require_command curl
validate_release_source
build_and_validate_release_assets

case "$(uname -s)" in
  Darwin) os_name="darwin" ;;
  Linux) os_name="linux" ;;
  *) die "unsupported smoke-test operating system: $(uname -s)" ;;
esac
case "$(uname -m)" in
  arm64|aarch64) arch_name="arm64" ;;
  x86_64|amd64) arch_name="amd64" ;;
  *) die "unsupported smoke-test architecture: $(uname -m)" ;;
esac

temp_root="$(mktemp -d)"
temp_root="$(cd "$temp_root" && pwd -P)"
runtime_home="$temp_root/home"
install_dir="$temp_root/bin"
archive="$DIST_DIR/sn-cli-$os_name-$arch_name.tar.gz"
release_server_pid=""
local_source_home=""
local_source_bin=""
release_tmux_tmp=""
tmux_id=""
cleanup() {
  if [ -n "$release_server_pid" ]; then
    kill "$release_server_pid" >/dev/null 2>&1 || true
    wait "$release_server_pid" >/dev/null 2>&1 || true
  fi
  if [ -x "$install_dir/sn-cli" ]; then
    SN_CLI_HOME="$runtime_home" "$install_dir/sn-cli" server stop >/dev/null 2>&1 || true
  fi
  if [ -n "$release_tmux_tmp" ] && [ -x "$install_dir/sn-cli" ]; then
    SN_CLI_HOME="$runtime_home" TMUX_TMPDIR="$release_tmux_tmp" \
      "$install_dir/sn-cli" session close-all \
      >/dev/null 2>&1 || true
    SN_CLI_HOME="$runtime_home" TMUX_TMPDIR="$release_tmux_tmp" \
      "$install_dir/sn-cli" tmux stop-all \
      >/dev/null 2>&1 || true
  fi
  if [ -n "$local_source_home" ] && [ -x "$local_source_bin/sn-cli" ]; then
    SN_CLI_HOME="$local_source_home" \
      "$local_source_bin/sn-cli" server stop >/dev/null 2>&1 || true
  fi
  rm -rf "$temp_root"
}
trap cleanup EXIT

validate_installer_path_safety

log "[release-check] installing and exercising $archive"
mkdir -p "$runtime_home/configs" "$runtime_home/tools" "$runtime_home/resources"
chmod 700 "$runtime_home"
printf '%s\n' '{"type":"cli","command":"codex","unexpected":true}' \
  >"$runtime_home/configs/local-only.json"
printf '%s\n' '{"type":"cli","command":"codex","unexpected":true}' \
  >"$runtime_home/configs/cx.json"
printf '%s\n' '{"unexpected":true}' >"$runtime_home/runtime.json"
printf '%s\n' '{"unexpected":true}' >"$runtime_home/tools/local-only.json"
printf '%s\n' '{"unexpected":true}' >"$runtime_home/tools/web_search.json"
mkdir -p "$runtime_home/resources/schema"
printf '%s\n' 'outdated schema' >"$runtime_home/resources/schema/runtime.schema.json"
if bash "$ROOT_DIR/install.sh" \
  --archive "$archive" \
  --checksums "$DIST_DIR/checksums.txt" \
  --home "$runtime_home" \
  --install-dir "$install_dir"; then
  die "default install accepted invalid Profile/runtime config"
fi
grep -q '"unexpected"' "$runtime_home/configs/local-only.json" ||
  die "failed preflight changed an invalid profile"
grep -q '"unexpected"' "$runtime_home/configs/cx.json" ||
  die "failed preflight changed a same-name invalid profile"
grep -q '"unexpected"' "$runtime_home/runtime.json" ||
  die "failed preflight changed runtime.json"
grep -q '"unexpected"' "$runtime_home/tools/local-only.json" ||
  die "failed preflight changed a local-only tool"
grep -q '"unexpected"' "$runtime_home/tools/web_search.json" ||
  die "failed preflight changed a same-name tool"
grep -q 'outdated schema' "$runtime_home/resources/schema/runtime.schema.json" ||
  die "failed preflight changed managed resources"
[ ! -e "$runtime_home/bin/sn-cli" ] ||
  die "failed preflight installed sn-cli"
[ ! -e "$runtime_home/bin/sn-server" ] ||
  die "failed preflight installed sn-server"

bash "$ROOT_DIR/install.sh" \
  --archive "$archive" \
  --checksums "$DIST_DIR/checksums.txt" \
  --home "$runtime_home" \
  --install-dir "$install_dir" \
  --overwrite-configs

[ ! -e "$runtime_home/configs/local-only.json" ] ||
  die "--overwrite-configs kept a local-only profile"
[ ! -e "$runtime_home/tools/local-only.json" ] ||
  die "--overwrite-configs kept a local-only tool"
cmp "$ROOT_DIR/configs/cx.json" "$runtime_home/configs/cx.json" >/dev/null ||
  die "--overwrite-configs did not replace a same-name profile"
cmp "$ROOT_DIR/release/runtime.json" "$runtime_home/runtime.json" >/dev/null ||
  die "--overwrite-configs did not replace runtime.json"
cmp "$ROOT_DIR/resources/tools/web_search.json" "$runtime_home/tools/web_search.json" >/dev/null ||
  die "--overwrite-configs did not replace a same-name tool"
cmp "$ROOT_DIR/resources/schema/runtime.schema.json" \
  "$runtime_home/resources/schema/runtime.schema.json" >/dev/null ||
  die "install did not refresh managed resources"
cmp "$ROOT_DIR/release/tmux.conf" \
  "$runtime_home/resources/tmux.conf" >/dev/null ||
  die "install did not refresh the Tmux bootstrap config"
cmp "$ROOT_DIR/release/release.json" \
  "$runtime_home/resources/release.json" >/dev/null ||
  die "installed release manifest differs from the source compatibility contract"

printf '%s\n' '{"type":"cli","command":"codex"}' \
  >"$runtime_home/configs/local-only.json"
printf '%s\n' '{"type":"cli","command":"codex","prompt":"local-default"}' \
  >"$runtime_home/configs/cx.json"
printf '%s\n' '{"agent":{"max_rounds":7}}' >"$runtime_home/runtime.json"
printf '\n' >>"$runtime_home/tools/web_search.json"
rm "$runtime_home/tools/web_fetch.json"
printf '%s\n' 'stale managed resource' >"$runtime_home/resources/schema/runtime.schema.json"
bash "$ROOT_DIR/install.sh" \
  --archive "$archive" \
  --checksums "$DIST_DIR/checksums.txt" \
  --home "$runtime_home" \
  --install-dir "$install_dir"
grep -q '"command":"codex"' "$runtime_home/configs/local-only.json" ||
  die "default install overwrote a current local profile"
grep -q '"local-default"' "$runtime_home/configs/cx.json" ||
  die "default install overwrote a current same-name profile"
grep -q '"max_rounds":7' "$runtime_home/runtime.json" ||
  die "default install overwrote a current runtime.json"
if cmp "$ROOT_DIR/resources/tools/web_search.json" \
  "$runtime_home/tools/web_search.json" >/dev/null; then
  die "default install overwrote a current same-name tool"
fi
cmp "$ROOT_DIR/resources/tools/web_fetch.json" "$runtime_home/tools/web_fetch.json" >/dev/null ||
  die "default install did not copy a missing packaged tool"
cmp "$ROOT_DIR/resources/schema/runtime.schema.json" \
  "$runtime_home/resources/schema/runtime.schema.json" >/dev/null ||
  die "default install did not refresh managed resources"

release_payload="$temp_root/release-payload"
mkdir -p "$release_payload"
tar -xzf "$archive" -C "$release_payload"
release_root="$temp_root/release-server"
release_dir="$release_root/$RELEASE_VERSION"
mkdir -p "$release_dir"
cp "$archive" "$release_dir/"
cp "$DIST_DIR/checksums.txt" "$release_dir/"
release_server="$temp_root/release-fileserver"
"$GO_BIN" -C "$ROOT_DIR" build \
  -o "$release_server" ./internal/testkit/releasefileserver
release_address_file="$temp_root/release-server.address"
"$release_server" \
  --root "$release_root" \
  --address-file "$release_address_file" \
  >"$temp_root/release-server.out" 2>"$temp_root/release-server.err" &
release_server_pid="$!"
for _ in {1..100}; do
  [ -s "$release_address_file" ] && break
  kill -0 "$release_server_pid" >/dev/null 2>&1 ||
    replay_logs "$temp_root/release-server.err"
  sleep 0.05
done
[ -s "$release_address_file" ] ||
  die "local release fixture server did not start"
release_address="$(tr -d '[:space:]' <"$release_address_file")"

current_update_output="$(
  SN_CLI_HOME="$runtime_home" \
    SN_CLI_RELEASE_BASE_URL="http://$release_address" \
    "$install_dir/sn-cli" --json server update \
      --version "$RELEASE_VERSION"
)"
printf '%s\n' "$current_update_output" |
  grep -Eq '"version"[[:space:]]*:[[:space:]]*"'"$RELEASE_VERSION"'"' ||
  die "current updater did not activate the staged candidate"
[ ! -e "$runtime_home/state/activation.guard.json" ] ||
  die "current updater left an activation guard"
[ ! -e "$runtime_home/state/activation.journal.json" ] ||
  die "current updater left an activation journal"

kill "$release_server_pid" >/dev/null 2>&1 || true
wait "$release_server_pid" || true
release_server_pid=""

log "[release-check] validating direct sn-server activation barrier"
printf '%s\n' '{}' >"$runtime_home/state/activation.guard.json"
HTTP_ADDR="127.0.0.1:0" SN_CLI_HOME="$runtime_home" \
  "$runtime_home/bin/sn-server" \
  >"$temp_root/guarded-server.out" 2>"$temp_root/guarded-server.err" &
guarded_server_pid="$!"
# Guard rejection (RequireNoGuard) should make sn-server exit immediately with
# an "activation gate" fatal log. Poll briefly; then SIGKILL unconditionally so
# the process is guaranteed reaped and the wait below cannot block — on some CI
# kernels a served sn-server (or a leftover zombie) does not respond within the
# poll window, and `wait` on it would hang the whole release check.
for _ in {1..100}; do
  if ! kill -0 "$guarded_server_pid" >/dev/null 2>&1; then
    break
  fi
  sleep 0.05
done
kill -KILL "$guarded_server_pid" >/dev/null 2>&1 || true
wait "$guarded_server_pid" >/dev/null 2>&1 || true
log "[release-check] guarded stdout: $(cat "$temp_root/guarded-server.out" 2>/dev/null)"
log "[release-check] guarded stderr: $(cat "$temp_root/guarded-server.err" 2>/dev/null)"
grep -q 'activation gate' "$temp_root/guarded-server.err" ||
  die "direct sn-server guard failure lacked activation diagnostic"
rm -f "$runtime_home/state/activation.guard.json"

version_output="$(SN_CLI_HOME="$runtime_home" "$install_dir/sn-cli" --version)"
if [[ "$version_output" != "sn-cli $RELEASE_VERSION" && "$version_output" != "sn-cli $RELEASE_VERSION ("* ]]; then
  die "release binary version mismatch: $version_output"
fi
profile_human="$(SN_CLI_HOME="$runtime_home" "$install_dir/sn-cli" profile list)"
profile_human_trimmed="$(printf '%s' "$profile_human" | sed -E 's/^[[:space:]]+//')"
case "$profile_human_trimmed" in
  \{*) die "profile list default output was JSON instead of human text" ;;
esac
SN_CLI_HOME="$runtime_home" "$install_dir/sn-cli" --json profile check >/dev/null
SN_CLI_HOME="$runtime_home" "$install_dir/sn-cli" --json profile list >/dev/null
server_info="$(
  SN_CLI_HOME="$runtime_home" "$install_dir/sn-cli" --json server info
)"
printf '%s\n' "$server_info" |
  grep -Eq "\"contract_version\"[[:space:]]*:[[:space:]]*$contract_version" ||
  die "server info did not report contract_version=$contract_version"
printf '%s\n' "$server_info" | grep -Eq '"server"' ||
  die "server info did not report the server namespace"
[[ "$server_info" != *$'\n'* ]] ||
  die "server info JSON was not one compact document"
case "$server_info" in
  \{*\}) ;;
  *) die "server info JSON was not an object document" ;;
esac
unknown_stdout="$temp_root/unknown.stdout"
unknown_stderr="$temp_root/unknown.stderr"
if SN_CLI_HOME="$runtime_home" "$install_dir/sn-cli" --json unknown info \
  >"$unknown_stdout" 2>"$unknown_stderr"; then
  die "unknown namespace was accepted"
fi
[ ! -s "$unknown_stdout" ] || die "failed JSON command wrote to stdout"
grep -Eq "\"contract_version\"[[:space:]]*:[[:space:]]*$contract_version" \
  "$unknown_stderr" ||
  die "failed JSON command did not return a contract v$contract_version error"
[ "$(awk 'NF {count++} END {print count + 0}' "$unknown_stderr")" -eq 1 ] ||
  die "failed JSON command did not return exactly one compact error document"

direct_home="$temp_root/direct-home"
direct_bin="$direct_home/fake-bin"
mkdir -p "$direct_home/configs" "$direct_bin"
$GO_BIN -C "$ROOT_DIR" build \
  -o "$temp_root/ptyrun" ./internal/testkit/ptyx/cmd/ptyrun
# macOS platform binaries can be killed by AMFI after being copied to a new
# path. Keep the signed executable at its system path while exposing the
# adapter fixture under the expected command name.
ln -s /bin/echo "$direct_bin/codex"
ln -s /bin/echo "$direct_bin/claude"
cp "$ROOT_DIR/release/runtime.json" "$direct_home/runtime.json"
printf '%s\n' '{"type":"cli","command":"codex"}' \
  >"$direct_home/configs/cx.json"
printf '%s\n' '{"type":"cli","command":"codex","args":["--search"]}' \
  >"$direct_home/configs/commit.json"
require_command tmux
release_tmux_tmp="$temp_root/tmux-tmp"
mkdir -m 0700 "$release_tmux_tmp"
doctor_output="$(
  PATH="$direct_bin:$PATH" \
    Z_AI_API_KEY=doctor-smoke \
    ALIYUN_API_KEY=doctor-smoke \
    KMM_API_KEY=doctor-smoke \
    BAILIAN_API_KEY=doctor-smoke \
    SN_CLI_HOME="$runtime_home" \
    TMUX_TMPDIR="$release_tmux_tmp" \
    "$install_dir/sn-cli" --json doctor
)"
printf '%s\n' "$doctor_output" |
  grep -Eq '"ok"[[:space:]]*:[[:space:]]*true' ||
  die "doctor did not report an OK installed Runtime: $doctor_output"
printf '%s\n' "$doctor_output" |
  grep -Eq "\"contract_version\"[[:space:]]*:[[:space:]]*$contract_version" ||
  die "doctor did not report contract_version=$contract_version"
help_output="$(SN_CLI_HOME="$runtime_home" "$install_dir/sn-cli" help tmux)"
printf '%s\n' "$help_output" |
  grep -Fq 'sn-cli tmux stop-all' ||
  die "topic help did not document tmux stop-all"
printf '%s\n' "$help_output" |
  grep -Fq 'run session close-all first' ||
  die "topic help did not document Session binding safety"
help_json="$(SN_CLI_HOME="$runtime_home" "$install_dir/sn-cli" --json help doctor)"
printf '%s\n' "$help_json" |
  grep -Eq '"name"[[:space:]]*:[[:space:]]*"doctor"' ||
  die "machine topic help did not identify doctor"
direct_output="$(PATH="$direct_bin:$PATH" SN_CLI_HOME="$direct_home" \
  "$temp_root/ptyrun" "$install_dir/sn-cli" cx release-smoke | tr -d '\r')"
[ "$direct_output" = "-- release-smoke" ] ||
  die "bare cx Profile did not use direct argv: $direct_output"
exec_output="$(PATH="$direct_bin:$PATH" SN_CLI_HOME="$direct_home" \
  "$install_dir/sn-cli" exec cx release-smoke)"
[ "$exec_output" = "exec -- release-smoke" ] ||
  die "exec cx Profile did not use noninteractive argv: $exec_output"
commit_output="$(PATH="$direct_bin:$PATH" SN_CLI_HOME="$direct_home" \
  "$install_dir/sn-cli" exec commit direct-smoke)"
[ "$commit_output" = "--search exec -- direct-smoke" ] ||
  die "exec commit Profile did not use typed exec argv: $commit_output"
profile_commit_output="$(
  printf '%s' 'profile-smoke' |
    PATH="$direct_bin:$PATH" SN_CLI_HOME="$direct_home" \
      "$install_dir/sn-cli" exec commit
)"
[ "$profile_commit_output" = "--search exec -- profile-smoke" ] ||
  die "exec commit stdin task failed"
if PATH="$direct_bin:$PATH" SN_CLI_HOME="$direct_home" \
	"$install_dir/sn-cli" profile commit direct-smoke >/dev/null 2>&1; then
	die "removed profile execution route was accepted"
fi

server_probe_token="release-check-probe-token"
server_probe_base=$((49152 + (($$ + RANDOM) % 12000)))
server_probe_address=""
for server_probe_offset in 0 137 277 419 563 719; do
  server_probe_port=$((49152 + ((server_probe_base - 49152 + server_probe_offset) % 12000)))
  candidate_address="127.0.0.1:$server_probe_port"
  if HTTP_ADDR="$candidate_address" SN_SERVER_TOKEN="$server_probe_token" \
    SN_CLI_HOME="$runtime_home" \
    "$install_dir/sn-cli" server start >/dev/null 2>&1; then
    candidate_url="http://$candidate_address"
    for _ in {1..20}; do
      candidate_code="$(
        curl --silent --show-error --output /dev/null \
          --write-out '%{http_code}' --connect-timeout 0.25 --max-time 0.5 \
          --header "Authorization: Bearer $server_probe_token" \
          "$candidate_url/healthz" 2>/dev/null || true
      )"
      if [ "$candidate_code" = "200" ]; then
        server_probe_address="$candidate_address"
        break
      fi
      sleep 0.05
    done
  fi
  if [ -n "$server_probe_address" ]; then
    break
  fi
  # A candidate may have become unavailable between selection and bind. Stop
  # through the lifecycle owner so its pid/lease state is reconciled before the
  # next bounded candidate is attempted.
  SN_CLI_HOME="$runtime_home" \
    "$install_dir/sn-cli" server stop >/dev/null 2>&1 || true
done
[ -n "$server_probe_address" ] ||
  die "sn-server did not start on any loopback probe candidate"

server_probe_url="http://$server_probe_address"
unauthorized_ready_body="$temp_root/readyz-unauthorized.json"
unauthorized_ready_code="$(
  curl --silent --show-error --output "$unauthorized_ready_body" \
    --write-out '%{http_code}' --connect-timeout 1 --max-time 2 \
    "$server_probe_url/readyz"
)"
[ "$unauthorized_ready_code" = "401" ] ||
  die "unauthenticated /readyz returned HTTP $unauthorized_ready_code, expected 401"
if grep -Eqi '(^|[^[:alnum:]_])(ready|not_ready)([^[:alnum:]_]|$)' \
  "$unauthorized_ready_body"; then
  die "unauthenticated /readyz leaked readiness state"
fi

health_body="$temp_root/healthz.json"
health_code="$(
  curl --silent --show-error --output "$health_body" \
    --write-out '%{http_code}' --connect-timeout 1 --max-time 2 \
    --header "Authorization: Bearer $server_probe_token" \
    "$server_probe_url/healthz"
)"
[ "$health_code" = "200" ] ||
  die "authenticated /healthz returned HTTP $health_code, expected 200"
grep -Eq '"status"[[:space:]]*:[[:space:]]*"ok"' "$health_body" ||
  die "authenticated /healthz did not report status=ok"

ready_body="$temp_root/readyz.json"
ready_code=""
ready_deadline=$((SECONDS + 10))
while [ "$SECONDS" -lt "$ready_deadline" ]; do
  ready_code="$(
    curl --silent --show-error --output "$ready_body" \
      --write-out '%{http_code}' --connect-timeout 0.25 --max-time 0.5 \
      --header "Authorization: Bearer $server_probe_token" \
      "$server_probe_url/readyz" 2>/dev/null || true
  )"
  if [ "$ready_code" = "200" ] &&
    grep -Eq '"status"[[:space:]]*:[[:space:]]*"ready"' "$ready_body"; then
    break
  fi
  sleep 0.1
done
if [ "$ready_code" != "200" ] ||
  ! grep -Eq '"status"[[:space:]]*:[[:space:]]*"ready"' "$ready_body"; then
  die "authenticated /readyz did not become ready within 10s"
fi
status="$(SN_CLI_HOME="$runtime_home" "$install_dir/sn-cli" --json server status)"
printf '%s\n' "$status" | grep -Eq '"running"[[:space:]]*:[[:space:]]*true' ||
  die "sn-server exited after reporting ready"
running_digest_before="$(sha256_file "$runtime_home/bin/sn-cli")"
if bash "$ROOT_DIR/install.sh" \
  --archive "$archive" \
  --checksums "$DIST_DIR/checksums.txt" \
  --home "$runtime_home" \
  --install-dir "$install_dir" \
  --overwrite-configs; then
  die "installer activated while sn-server was running"
fi
[ "$(sha256_file "$runtime_home/bin/sn-cli")" = "$running_digest_before" ] ||
  die "blocked server-live upgrade changed the active binary"
SN_CLI_HOME="$runtime_home" "$install_dir/sn-cli" server stop >/dev/null
[ -f "$runtime_home/logs/sn-server.log" ] ||
  die "sn-server did not write its process log below the Runtime log root"
[ ! -e "$runtime_home/state/sn-server.log" ] ||
  die "sn-server retained the legacy state/sn-server.log path"

if PATH="$direct_bin:$PATH" SN_CLI_HOME="$runtime_home" \
  TMUX_TMPDIR="$release_tmux_tmp" \
  "$install_dir/sn-cli" tmux start cx >/dev/null 2>&1; then
  die "removed tmux start action was accepted"
fi
tmux_open="$(
  PATH="$direct_bin:$PATH" SN_CLI_HOME="$runtime_home" \
    TMUX_TMPDIR="$release_tmux_tmp" \
    "$install_dir/sn-cli" --json tmux open cx "tmux-smoke"
)"
tmux_id="$(
  printf '%s\n' "$tmux_open" |
    sed -n 's/.*"tmux_id":"\([^"]*\)".*/\1/p'
)"
[ -n "$tmux_id" ] || die "tmux open did not return tmux_id: $tmux_open"
TMUX_TMPDIR="$release_tmux_tmp" \
  tmux -L default list-sessions -F '#{session_name}' |
  grep -Fxq 'sn-session' ||
  die "default Tmux mode did not expose sn-session to ordinary tmux"
tmux_digest_before="$(sha256_file "$runtime_home/bin/sn-cli")"
if TMUX_TMPDIR="$release_tmux_tmp" bash "$ROOT_DIR/install.sh" \
  --archive "$archive" \
  --checksums "$DIST_DIR/checksums.txt" \
  --home "$runtime_home" \
  --install-dir "$install_dir" \
  --overwrite-configs; then
  die "installer activated while the managed Tmux session was live"
fi
[ "$(sha256_file "$runtime_home/bin/sn-cli")" = "$tmux_digest_before" ] ||
  die "blocked Tmux-live upgrade changed the active binary"
tmux_stop_all="$(PATH="$direct_bin:$PATH" SN_CLI_HOME="$runtime_home" \
  TMUX_TMPDIR="$release_tmux_tmp" \
  "$install_dir/sn-cli" --json tmux stop-all)"
printf '%s\n' "$tmux_stop_all" |
  grep -Eq '"stopped_count"[[:space:]]*:[[:space:]]*1' ||
  die "tmux stop-all did not stop the raw window: $tmux_stop_all"
tmux_id=""
if TMUX_TMPDIR="$release_tmux_tmp" \
  tmux -L default list-sessions -F '#{session_name}' 2>/dev/null |
  grep -Fxq 'sn-session'; then
  die "stopping the final managed window left sn-session behind"
fi
TMUX_TMPDIR="$release_tmux_tmp" bash "$ROOT_DIR/install.sh" \
  --archive "$archive" \
  --checksums "$DIST_DIR/checksums.txt" \
  --home "$runtime_home" \
  --install-dir "$install_dir" \
  --overwrite-configs

session_fixture_bin="$temp_root/session-fixture-bin"
mkdir -p "$session_fixture_bin"
"$GO_BIN" -C "$ROOT_DIR" build \
  -o "$session_fixture_bin/codex" ./internal/testkit/nativetuitarget
printf '{"type":"cli","command":"%s","model":"fixture"}\n' \
  "$session_fixture_bin/codex" \
  >"$runtime_home/configs/session-control-smoke.json"
session_fixture_fact="$temp_root/session-native-tui.fact"
session_large_input="$(printf 'session-frame-%04d;' {1..128})"
session_open="$(
  SN_CLI_HOME="$runtime_home" SN_NATIVE_TUI_FACT="$session_fixture_fact" \
    TMUX_TMPDIR="$release_tmux_tmp" \
    "$install_dir/sn-cli" --json session open session-control-smoke \
      "$session_large_input"
)"
session_id="$(
  printf '%s\n' "$session_open" |
    sed -n 's/.*"id":"\(session_[^"]*\)".*/\1/p'
)"
[ -n "$session_id" ] ||
  die "session open did not return session_id: $session_open"
printf '%s\n' "$session_open" |
  grep -Eq '"launch_accepted"[[:space:]]*:[[:space:]]*true' ||
  die "session open did not launch the native TUI: $session_open"
printf '%s\n' "$session_open" |
  grep -Eq '"initial_input_supplied"[[:space:]]*:[[:space:]]*true' ||
  die "session open did not report its initial input: $session_open"
printf '%s\n' "$session_open" |
  grep -Eq '"interface"[[:space:]]*:[[:space:]]*"native_tui"' ||
  die "session open did not publish a native_tui Session: $session_open"
session_fixture_output=""
for _ in {1..100}; do
  if [ -f "$session_fixture_fact" ]; then
    session_fixture_output="$(cat "$session_fixture_fact")"
  fi
  if printf '%s\n' "$session_fixture_output" | grep -Fq 'tty:true' &&
    printf '%s\n' "$session_fixture_output" | grep -Fq "$session_large_input"; then
    break
  fi
  sleep 0.1
done
printf '%s\n' "$session_fixture_output" | grep -Fq 'tty:true' ||
  die "session open target did not receive a tmux PTY: $session_fixture_output"
printf '%s\n' "$session_fixture_output" | grep -Fq "$session_large_input" ||
  die "native TUI initial input was not preserved in interactive argv"
session_messages="$(
  SN_CLI_HOME="$runtime_home" \
    "$install_dir/sn-cli" --json session messages --session-id "$session_id"
)"
printf '%s\n' "$session_messages" |
  grep -Eq '"messages"[[:space:]]*:[[:space:]]*(null|\[\])' ||
  die "native TUI Session unexpectedly created canonical messages: $session_messages"
session_events="$(
  SN_CLI_HOME="$runtime_home" \
    "$install_dir/sn-cli" --json session events --session-id "$session_id"
)"
printf '%s\n' "$session_events" |
  grep -Eq '"events"[[:space:]]*:[[:space:]]*(null|\[\])' ||
  die "native TUI Session unexpectedly created canonical events: $session_events"
session_second_input="send-native-smoke"
session_send="$(
  SN_CLI_HOME="$runtime_home" \
    TMUX_TMPDIR="$release_tmux_tmp" \
    "$install_dir/sn-cli" --json session send --session-id "$session_id" \
      "$session_second_input"
)"
printf '%s\n' "$session_send" |
  grep -Eq '"accepted"[[:space:]]*:[[:space:]]*true' ||
  die "session send was not accepted by tmux: $session_send"
for _ in {1..100}; do
  session_fixture_output="$(cat "$session_fixture_fact")"
  if printf '%s\n' "$session_fixture_output" |
    grep -Fq "input:$session_second_input"; then
    break
  fi
  sleep 0.1
done
printf '%s\n' "$session_fixture_output" |
  grep -Fq "input:$session_second_input" ||
  die "session send did not reach the Provider-native TUI"
if SN_CLI_HOME="$runtime_home" \
  "$install_dir/sn-cli" session exec session-control-smoke \
    --session-id "$session_id" "must-not-run" \
    >"$temp_root/native-session-exec.out" \
    2>"$temp_root/native-session-exec.err"; then
  die "session exec accepted a native_tui Session ID"
fi
grep -Fq 'native_tui' "$temp_root/native-session-exec.err" ||
  die "session exec native_tui rejection was not explicit"
if PATH="$direct_bin:$PATH" SN_CLI_HOME="$runtime_home" \
  TMUX_TMPDIR="$release_tmux_tmp" \
  "$install_dir/sn-cli" tmux stop-all >/dev/null 2>&1; then
  die "tmux stop-all accepted a Session-bound window"
fi
session_close_all="$(
  PATH="$direct_bin:$PATH" SN_CLI_HOME="$runtime_home" \
    TMUX_TMPDIR="$release_tmux_tmp" \
    "$install_dir/sn-cli" --json session close-all
)"
printf '%s\n' "$session_close_all" |
  grep -Eq '"closed_count"[[:space:]]*:[[:space:]]*1' ||
  die "session close-all did not close the native TUI window: $session_close_all"
session_show="$(
  SN_CLI_HOME="$runtime_home" \
    "$install_dir/sn-cli" --json session show --session-id "$session_id"
)"
printf '%s\n' "$session_show" | grep -Fq "$session_id" ||
  die "session close-all removed the native_tui Session fact"
printf '%s\n' "$session_show" |
  grep -Eq '"interface"[[:space:]]*:[[:space:]]*"native_tui"' ||
  die "closed Session lost its native_tui interface: $session_show"
if SN_CLI_HOME="$runtime_home" SN_NATIVE_TUI_FACT="$session_fixture_fact" \
  TMUX_TMPDIR="$release_tmux_tmp" \
  "$install_dir/sn-cli" session open session-control-smoke \
    --session-id "$session_id" >/dev/null 2>&1; then
  die "session open reused an existing native_tui Session without resume identity"
fi
audit_file="$runtime_home/logs/$(date +%y%m%d)/audit.jsonl"
[ -f "$audit_file" ] || die "control audit log was not created"
grep -Fq '"namespace":"doctor"' "$audit_file" ||
  die "doctor audit record is missing"
grep -Fq '"action":"stop-all"' "$audit_file" ||
  die "tmux stop-all audit record is missing"
grep -Fq '"action":"close-all"' "$audit_file" ||
  die "session close-all audit record is missing"

log "[release-check] exercising concurrent server lifecycle operations"
start_pids=()
for index in 1 2; do
  HTTP_ADDR="127.0.0.1:0" SN_CLI_HOME="$runtime_home" \
    "$install_dir/sn-cli" server start \
    >"$temp_root/concurrent-start-$index.out" \
    2>"$temp_root/concurrent-start-$index.err" &
  start_pids+=("$!")
done
start_successes=0
for pid in "${start_pids[@]}"; do
  if wait "$pid"; then
    start_successes=$((start_successes + 1))
  fi
done
if [ "$start_successes" -lt 1 ]; then
  replay_logs "$temp_root"/concurrent-start-*.out "$temp_root"/concurrent-start-*.err
  die "all concurrent sn-server start operations failed"
fi
status="$(SN_CLI_HOME="$runtime_home" "$install_dir/sn-cli" --json server status)"
printf '%s\n' "$status" | grep -Eq '"running"[[:space:]]*:[[:space:]]*true' ||
  die "sn-server was not running after concurrent start"

stop_pids=()
for index in 1 2; do
  SN_CLI_HOME="$runtime_home" "$install_dir/sn-cli" server stop \
    >"$temp_root/concurrent-stop-$index.out" \
    2>"$temp_root/concurrent-stop-$index.err" &
  stop_pids+=("$!")
done
stop_successes=0
for pid in "${stop_pids[@]}"; do
  if wait "$pid"; then
    stop_successes=$((stop_successes + 1))
  fi
done
if [ "$stop_successes" -lt 1 ]; then
  replay_logs "$temp_root"/concurrent-stop-*.out "$temp_root"/concurrent-stop-*.err
  die "all concurrent sn-server stop operations failed"
fi
status="$(SN_CLI_HOME="$runtime_home" "$install_dir/sn-cli" --json server status)"
printf '%s\n' "$status" | grep -Eq '"running"[[:space:]]*:[[:space:]]*false' ||
  die "sn-server was still running after concurrent stop"

log "[release-check] exercising destructive local source install"
local_source_home="$temp_root/local-source-home"
local_source_bin="$temp_root/local-source-bin"
local_source_args=(
  --binary "$release_payload/sn-cli"
  --server "$release_payload/sn-server"
  --configs "$release_payload/configs"
  --resources "$release_payload/resources"
  --release "$release_payload/release"
  --home "$local_source_home"
  --install-dir "$local_source_bin"
  --local-source-install
)
bash "$ROOT_DIR/install.sh" "${local_source_args[@]}"

local_start="$(
  HTTP_ADDR="127.0.0.1:0" SN_CLI_HOME="$local_source_home" \
    "$local_source_bin/sn-cli" --json server start
)"
local_start_pid="$(
  printf '%s\n' "$local_start" |
    sed -n 's/.*"pid":\([0-9][0-9]*\).*/\1/p'
)"
[ -n "$local_start_pid" ] && [ "$local_start_pid" -gt 0 ] ||
  die "local source server start did not return a positive pid: $local_start"
local_status="$(
  SN_CLI_HOME="$local_source_home" \
    "$local_source_bin/sn-cli" --json server status
)"
printf '%s\n' "$local_status" |
  grep -Eq '"pid"[[:space:]]*:[[:space:]]*'"$local_start_pid" ||
  die "server status pid did not match server start"

printf '%s\n' \
  '{"type":"cli","command":"codex","prompt":"local-drift"}' \
  >"$local_source_home/configs/cx.json"
printf '\n' >>"$local_source_home/tools/web_search.json"
mkdir -p \
  "$local_source_home/sessions/_system" \
  "$local_source_home/state/session-locks" \
  "$local_source_home/state/session-invocations" \
  "$local_source_home/state/session-mutations" \
  "$local_source_home/state/session-trash-moves"
printf '{"schema_version":%s,"sessions":[]}\n' "$session_schema_version" \
  >"$local_source_home/sessions/_system/index.json"
printf '%s\n' runtime-state \
  >"$local_source_home/state/session-locks/index.lock"
printf '%s\n' runtime-state \
  >"$local_source_home/state/session-invocations/.invocation-current.json"
printf '%s\n' runtime-state \
  >"$local_source_home/state/session-mutations/session_fixture.json"
printf '%s\n' runtime-state \
  >"$local_source_home/state/session-trash-moves/session_fixture.json"

bash "$ROOT_DIR/install.sh" "${local_source_args[@]}"
local_status="$(
  SN_CLI_HOME="$local_source_home" \
    "$local_source_bin/sn-cli" --json server status
)"
printf '%s\n' "$local_status" |
  grep -Eq '"running"[[:space:]]*:[[:space:]]*false' ||
  die "local source install restarted sn-server"
[ ! -e "$local_source_home/state/sn-server.pid" ] ||
  die "local source install kept the stopped server pid record"
cmp "$ROOT_DIR/configs/cx.json" "$local_source_home/configs/cx.json" >/dev/null ||
  die "local source install did not replace active profiles"
cmp "$ROOT_DIR/resources/tools/web_search.json" \
  "$local_source_home/tools/web_search.json" >/dev/null ||
  die "local source install did not replace active tools"
for reset_path in \
  "$local_source_home/sessions" \
  "$local_source_home/state/session-locks" \
  "$local_source_home/state/session-invocations" \
  "$local_source_home/state/session-mutations" \
  "$local_source_home/state/session-trash-moves" \
  "$local_source_home/state/runtime.db" \
  "$local_source_home/state/runtime.db-wal" \
  "$local_source_home/state/runtime.db-shm" \
  "$local_source_home/state/runtime.db-journal"; do
  [ ! -e "$reset_path" ] ||
    die "local source install kept Runtime state: $reset_path"
done

mkdir -p "$local_source_home/sessions/_system"
printf '%s\n' '{"schema_version":999,"sessions":[]}' \
  >"$local_source_home/sessions/_system/index.json"
printf '%s\n' 'not a current Runtime database' \
  >"$local_source_home/state/runtime.db"
if bash "$ROOT_DIR/install.sh" \
  --archive "$archive" \
  --checksums "$DIST_DIR/checksums.txt" \
  --home "$local_source_home" \
  --install-dir "$local_source_bin" \
  --overwrite-configs; then
  die "archive installer inherited destructive local source semantics"
fi
[ -e "$local_source_home/sessions/_system/index.json" ] &&
  [ -e "$local_source_home/state/runtime.db" ] ||
  die "failed archive preflight changed unsupported Runtime state"
bash "$ROOT_DIR/install.sh" "${local_source_args[@]}"
[ ! -e "$local_source_home/sessions" ] &&
  [ ! -e "$local_source_home/state/runtime.db" ] ||
  die "repeated local source install did not reset unsupported state"

log "[release-check] passed"
