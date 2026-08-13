#!/usr/bin/env bash

# validate_installer_path_safety exercises only installer path and ownership
# barriers. It deliberately uses the coordinator's temporary root and release
# archive; no active Runtime home is touched.
# shellcheck disable=SC2154 # coordinator-owned ROOT_DIR/DIST_DIR/temp_root/archive
validate_installer_path_safety() {
  log "[release-check] validating installer path safety"
  if bash "$ROOT_DIR/install.sh" --dry-run \
    --binary "$temp_root/a" --archive "$temp_root/b" \
    >"$temp_root/mixed-local.out" 2>"$temp_root/mixed-local.err"; then
    die "installer accepted mutually exclusive local source modes"
  fi
  if bash "$ROOT_DIR/install.sh" --dry-run \
    --archive "$temp_root/a" --server "$temp_root/b" \
    >"$temp_root/archive-extra.out" 2>"$temp_root/archive-extra.err"; then
    die "installer accepted a binary-only option with --archive"
  fi
  if bash "$ROOT_DIR/install.sh" --dry-run \
    --checksums "$temp_root/a" \
    >"$temp_root/network-checksum.out" 2>"$temp_root/network-checksum.err"; then
    die "installer accepted --checksums without --archive"
  fi
  if bash "$ROOT_DIR/install.sh" --dry-run --home / \
    >"$temp_root/root-home.out" 2>"$temp_root/root-home.err"; then
    die "installer accepted / as Runtime home"
  fi
  if bash "$ROOT_DIR/install.sh" --dry-run --home "" \
    >"$temp_root/empty-home.out" 2>"$temp_root/empty-home.err"; then
    die "installer accepted an empty Runtime home"
  fi
  (
    cd "$temp_root" || exit
    bash "$ROOT_DIR/install.sh" --dry-run \
      --home relative-home --install-dir relative-bin
  ) >"$temp_root/relative-home.out" 2>&1
  grep -q "home: $temp_root/relative-home" "$temp_root/relative-home.out" ||
    die "installer did not canonicalize a relative Runtime home"

  local inside_home case_home unicode_home
  inside_home="$temp_root/inside-home"
  if bash "$ROOT_DIR/install.sh" \
    --archive "$archive" \
    --checksums "$DIST_DIR/checksums.txt" \
    --home "$inside_home" \
    --install-dir "$inside_home/configs" \
    --overwrite-configs; then
    die "installer accepted an install directory inside Runtime home"
  fi
  [ ! -e "$inside_home" ] ||
    die "invalid nested install directory created Runtime home"

  case_home="$temp_root/RuntimeHome"
  if bash "$ROOT_DIR/install.sh" --dry-run \
    --home "$case_home" \
    --install-dir "$temp_root/runtimehome/configs" \
    >"$temp_root/case-home.out" 2>"$temp_root/case-home.err"; then
    die "installer accepted a case-folded install directory inside Runtime home"
  fi
  [ ! -e "$case_home" ] && [ ! -e "$temp_root/runtimehome" ] ||
    die "case-folded invalid paths created Runtime home"

  unicode_home="$temp_root/ÄHome"
  if bash "$ROOT_DIR/install.sh" --dry-run \
    --home "$unicode_home" \
    --install-dir "$temp_root/ähome/configs" \
    >"$temp_root/unicode-home.out" 2>"$temp_root/unicode-home.err"; then
    die "installer accepted unresolved non-ASCII paths with ambiguous containment"
  fi
  [ ! -e "$unicode_home" ] && [ ! -e "$temp_root/ähome" ] ||
    die "invalid unresolved Unicode paths created Runtime home"

  local alias_parent real_parent alias_install alias_home
  alias_parent="$temp_root/alias-parent"
  real_parent="$temp_root/real-parent"
  alias_install="$temp_root/alias-bin"
  mkdir -p "$real_parent"
  ln -s "$real_parent" "$alias_parent"
  alias_home="$alias_parent/runtime"
  bash "$ROOT_DIR/install.sh" \
    --archive "$archive" \
    --checksums "$DIST_DIR/checksums.txt" \
    --home "$alias_home" \
    --install-dir "$alias_install" \
    --overwrite-configs
  [ -x "$real_parent/runtime/bin/sn-cli" ] ||
    die "installer did not canonicalize a missing home below a symlink ancestor"
  [ "$(readlink "$alias_install/sn-cli")" = "$real_parent/runtime/bin/sn-cli" ] ||
    die "installer command link did not use canonical Runtime home"

  local external_home symlink_home conflict_home conflict_install
  external_home="$temp_root/external-home"
  symlink_home="$temp_root/symlink-home"
  mkdir -p "$external_home"
  chmod 700 "$external_home"
  printf '%s\n' safe >"$external_home/sentinel"
  ln -s "$external_home" "$symlink_home"
  if bash "$ROOT_DIR/install.sh" \
    --archive "$archive" \
    --checksums "$DIST_DIR/checksums.txt" \
    --home "$symlink_home" \
    --install-dir "$temp_root/symlink-bin" \
    --overwrite-configs; then
    die "installer accepted a symlink Runtime home"
  fi
  [ "$(cat "$external_home/sentinel")" = "safe" ] ||
    die "symlink Runtime home changed an external sentinel"
  [ ! -e "$external_home/bin/sn-cli" ] ||
    die "symlink Runtime home installed outside its declared root"

  conflict_home="$temp_root/conflict-home"
  conflict_install="$temp_root/conflict-bin"
  mkdir -p "$conflict_install"
  printf '%s\n' occupied >"$conflict_install/sn-cli"
  if bash "$ROOT_DIR/install.sh" \
    --archive "$archive" \
    --checksums "$DIST_DIR/checksums.txt" \
    --home "$conflict_home" \
    --install-dir "$conflict_install" \
    --overwrite-configs; then
    die "installer overwrote a non-symlink command target"
  fi
  [ ! -e "$conflict_home/bin/sn-cli" ] ||
    die "install-link conflict was detected after Runtime activation"
}
