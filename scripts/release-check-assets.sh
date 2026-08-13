#!/usr/bin/env bash

# build_and_validate_release_assets owns archive construction, payload-layout
# inspection, and checksum verification. Installed-runtime behavior is tested
# separately by the main release-check coordinator.
build_and_validate_release_assets() {
  log "[release-check] building assets version=$RELEASE_VERSION"
  run_make release-assets SN_CLI_VERSION="$RELEASE_VERSION"

  local -a expected_assets=(checksums.txt)
  local platform asset
  for platform in darwin-arm64 darwin-amd64 linux-arm64 linux-amd64; do
    expected_assets+=("sn-cli-$platform.tar.gz")
  done
  for asset in "${expected_assets[@]}"; do
    [ -f "$DIST_DIR/$asset" ] || die "missing release asset: $asset"
  done

  local expected_profile_entries actual_profile_entries
  local expected_tool_entries actual_tool_entries
  local expected_release_entries actual_release_entries legacy_payload_entries
  for asset in "${expected_assets[@]:1}"; do
    awk -v name="$asset" \
      '$2 == name || $2 == "*" name {found=1} END {exit !found}' \
      "$DIST_DIR/checksums.txt" || die "checksum missing for $asset"
    expected_profile_entries="$(
      printf 'configs/%s\n' "${SN_CLI_RELEASE_PROFILE_FILES[@]}" |
        LC_ALL=C sort
    )"
    actual_profile_entries="$(
      tar -tzf "$DIST_DIR/$asset" |
        awk 'index($0, "configs/") == 1 && $0 != "configs/" {print}' |
        LC_ALL=C sort
    )"
    [ "$actual_profile_entries" = "$expected_profile_entries" ] ||
      die "release asset Profile set does not match the formal release list: $asset"

    expected_tool_entries="$(
      printf 'resources/tools/%s\n' "${SN_CLI_RELEASE_TOOL_FILES[@]}" |
        LC_ALL=C sort
    )"
    actual_tool_entries="$(
      tar -tzf "$DIST_DIR/$asset" |
        awk 'index($0, "resources/tools/") == 1 && $0 != "resources/tools/" {print}' |
        LC_ALL=C sort
    )"
    [ "$actual_tool_entries" = "$expected_tool_entries" ] ||
      die "release asset Tool set does not match the formal release list: $asset"

    expected_release_entries="$(printf '%s\n' \
      release/release.json release/runtime.json release/tmux.conf | LC_ALL=C sort)"
    actual_release_entries="$(
      tar -tzf "$DIST_DIR/$asset" |
        awk 'index($0, "release/") == 1 && $0 != "release/" {print}' |
        LC_ALL=C sort
    )"
    [ "$actual_release_entries" = "$expected_release_entries" ] ||
      die "release asset fixed release set is invalid: $asset"

    legacy_payload_entries="$(
      tar -tzf "$DIST_DIR/$asset" |
        awk '$0 == "tools/" || index($0, "tools/") == 1 ||
          $0 == "runtime.json" || $0 == "resources/release.json" ||
          $0 == "resources/tmux.conf" {print}'
    )"
    [ -z "$legacy_payload_entries" ] ||
      die "release asset retained a legacy config path: $asset"
  done

  local checksum_log
  checksum_log="$(mktemp)"
  if command -v sha256sum >/dev/null 2>&1; then
    if ! (cd "$DIST_DIR" && sha256sum --check checksums.txt) >"$checksum_log" 2>&1; then
      replay_logs "$checksum_log"
      rm -f "$checksum_log"
      die "release asset checksum verification failed"
    fi
  elif ! (cd "$DIST_DIR" && shasum -a 256 --check checksums.txt) >"$checksum_log" 2>&1; then
    replay_logs "$checksum_log"
    rm -f "$checksum_log"
    die "release asset checksum verification failed"
  fi
  rm -f "$checksum_log"
}
