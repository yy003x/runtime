#!/usr/bin/env bash

# validate_release_source owns repository-layout, compatibility-manifest, and
# deterministic Go validation. It is sourced by release-check.sh so the parsed
# compatibility values remain available to later installed-runtime smoke tests.
validate_release_source() {
  log "[release-check] validating source"
  local legacy_source profile tool schema manifest_value
  for legacy_source in \
    "$ROOT_DIR/tools" \
    "$ROOT_DIR/configs/runtime" \
    "$ROOT_DIR/resources/tmux.conf" \
    "$ROOT_DIR/resources/release.json"; do
    [ ! -e "$legacy_source" ] && [ ! -L "$legacy_source" ] ||
      die "legacy source layout entry remains: $legacy_source"
  done
  [ -d "$ROOT_DIR/resources/tools" ] && [ ! -L "$ROOT_DIR/resources/tools" ] ||
    die "resources/tools must be a directory, not a symlink"
  [ -d "$ROOT_DIR/release" ] && [ ! -L "$ROOT_DIR/release" ] ||
    die "release must be a directory, not a symlink"
  [ -f "$ROOT_DIR/release/runtime.json" ] && [ ! -L "$ROOT_DIR/release/runtime.json" ] ||
    die "missing or unsafe release/runtime.json"
  for profile in "${SN_CLI_RELEASE_PROFILE_FILES[@]}"; do
    [ -f "$ROOT_DIR/configs/$profile" ] || die "missing profile: $profile"
  done
  for tool in "${SN_CLI_RELEASE_TOOL_FILES[@]}"; do
    [ -f "$ROOT_DIR/resources/tools/$tool" ] && [ ! -L "$ROOT_DIR/resources/tools/$tool" ] ||
      die "missing or unsafe tool: $tool"
  done

  local unexpected_config_entries unexpected_resource_entries
  local unexpected_tool_entries unexpected_release_entries
  unexpected_config_entries="$(find "$ROOT_DIR/configs" -mindepth 1 -maxdepth 1 \
    ! -name '*.json' -print -quit)"
  [ -z "$unexpected_config_entries" ] ||
    die "unexpected configs entry: $unexpected_config_entries"
  unexpected_resource_entries="$(find "$ROOT_DIR/resources" -mindepth 1 -maxdepth 1 \
    ! -name schema ! -name tools -print -quit)"
  [ -z "$unexpected_resource_entries" ] ||
    die "unexpected resources entry: $unexpected_resource_entries"
  unexpected_tool_entries="$(find "$ROOT_DIR/resources/tools" -mindepth 1 -maxdepth 1 \
    ! -name '*.json' -print -quit)"
  [ -z "$unexpected_tool_entries" ] ||
    die "unexpected tools entry: $unexpected_tool_entries"
  unexpected_release_entries="$(find "$ROOT_DIR/release" -mindepth 1 -maxdepth 1 \
    ! -name runtime.json ! -name tmux.conf ! -name release.json -print -quit)"
  [ -z "$unexpected_release_entries" ] ||
    die "unexpected release entry: $unexpected_release_entries"

  for schema in profile.schema.json runtime.schema.json tool.schema.json; do
    [ -f "$ROOT_DIR/resources/schema/$schema" ] ||
      die "missing resource schema: $schema"
  done
  [ -f "$ROOT_DIR/release/release.json" ] && [ ! -L "$ROOT_DIR/release/release.json" ] ||
    die "missing or unsafe activation manifest: release/release.json"

  activation_epoch="$(release_manifest_int activation_epoch)"
  contract_version="$(release_manifest_int contract_version)"
  session_schema_version="$(release_manifest_int session_schema_version)"
  run_schema_version="$(release_manifest_int run_schema_version)"
  for manifest_value in \
    "$activation_epoch" "$contract_version" \
    "$session_schema_version" "$run_schema_version"; do
    [[ "$manifest_value" =~ ^[1-9][0-9]*$ ]] ||
      die "release manifest compatibility fields must be positive integers"
  done

  [ -f "$ROOT_DIR/release/tmux.conf" ] && [ ! -L "$ROOT_DIR/release/tmux.conf" ] ||
    die "missing or unsafe dedicated Tmux bootstrap config: release/tmux.conf"

  run_make fmt-check
  # This test reads release/release.json and compares it with the compatibility
  # constants compiled into the Runtime contract and both canonical Stores.
  "$GO_BIN" -C "$ROOT_DIR" test ./internal/application/activation \
    -run '^TestReleaseManifestMatchesCurrentRuntimeCompatibility$' -count=1
  run_make test-serial
  run_make test-race
  run_make coverage-critical
  env GOCACHE="${GOCACHE:-$("$GO_BIN" env GOCACHE)}" \
    GOMODCACHE="${GOMODCACHE:-$("$GO_BIN" env GOMODCACHE)}" \
    "$GO_BIN" -C "$ROOT_DIR" vet ./...
  bash "$ROOT_DIR/scripts/make-step-test.sh"
}

release_manifest_int() {
  sed -nE \
    "s/^[[:space:]]*\"$1\"[[:space:]]*:[[:space:]]*([0-9]+)[,]?[[:space:]]*$/\\1/p" \
    "$ROOT_DIR/release/release.json"
}
