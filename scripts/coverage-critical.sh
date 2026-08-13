#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GO_BIN="${GO:-go}"

check_package() {
  local package="$1" minimum="$2" output coverage
  output="$("$GO_BIN" -C "$ROOT_DIR" test -cover "$package" -count=1)"
  printf '%s\n' "$output"
  coverage="$(printf '%s\n' "$output" |
    sed -nE 's/.*coverage: ([0-9]+([.][0-9]+)?)% of statements.*/\1/p' |
    tail -1)"
  [ -n "$coverage" ] || {
    printf 'critical coverage: unable to read %s coverage\n' "$package" >&2
    return 1
  }
  awk -v package="$package" -v coverage="$coverage" -v minimum="$minimum" '
    BEGIN {
      printf "critical coverage: %s %.1f%% (minimum %.1f%%)\n",
        package, coverage, minimum
      if (coverage + 0 < minimum + 0) exit 1
    }
  '
}

# These floors protect the failure-prone persistence and supervision packages.
# Raise them as focused tests land; do not mask regressions with test retries.
check_package ./pkg/run 70.0
check_package ./pkg/session 60.0
check_package ./pkg/store/sqlite 60.0
check_package ./cmd/sn-server 65.0
