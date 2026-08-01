#!/usr/bin/env bash
# Render docs/assets/social-preview.html → docs/assets/social-preview.png (1280×640)
# Uses the locally installed Chrome/Chromium in headless mode. No npm dependencies.
#
# Chrome's --headless=new --screenshot sometimes renders the PNG and then fails to
# exit, so we launch it in the background, wait for the output file to stabilize,
# then terminate the process. Works on macOS and Linux.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SRC="$ROOT/docs/assets/social-preview.html"
OUT="$ROOT/docs/assets/social-preview.png"

# Locate a Chrome/Chromium executable.
CHROME=""
for c in \
  "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" \
  "/Applications/Chromium.app/Contents/MacOS/Chromium" \
  "/Applications/Google Chrome Canary.app/Contents/MacOS/Google Chrome Canary" \
  "/usr/bin/google-chrome" "/usr/bin/google-chrome-stable" \
  "/usr/bin/chromium" "/usr/bin/chromium-browser"; do
  [ -x "$c" ] && CHROME="$c" && break
done
if [ -z "$CHROME" ]; then
  for c in google-chrome google-chrome-stable chromium chromium-browser; do
    p="$(command -v "$c" 2>/dev/null || true)"
    [ -n "$p" ] && CHROME="$p" && break
  done
fi
[ -n "$CHROME" ] || { echo "error: Chrome/Chromium not found" >&2; exit 1; }

# Portable file size in bytes.
filesize() {
  stat -f%z "$1" 2>/dev/null || stat -c%s "$1" 2>/dev/null || echo 0
}

tmp_profile="$(mktemp -d)"
cleanup() {
  [ "${chrome_pid:-}" != "" ] && kill "$chrome_pid" 2>/dev/null || true
  [ "${chrome_pid:-}" != "" ] && wait "$chrome_pid" 2>/dev/null || true
  rm -rf "$tmp_profile"
}
trap cleanup EXIT

rm -f "$OUT"

"$CHROME" --headless=new --disable-gpu --no-first-run --no-default-browser-check \
  --force-device-scale-factor=1 --hide-scrollbars \
  --default-background-color=00000000 \
  --user-data-dir="$tmp_profile" \
  --window-size=1280,640 --screenshot="$OUT" "file://$SRC" >/dev/null 2>&1 &
chrome_pid=$!

# Wait up to 25s for the PNG to be written and its size to stabilize.
deadline=$((SECONDS + 25))
prev=-1
while [ "$SECONDS" -lt "$deadline" ]; do
  if [ -s "$OUT" ]; then
    cur="$(filesize "$OUT")"
    if [ "$cur" -gt 0 ] && [ "$cur" = "$prev" ]; then
      break
    fi
    prev="$cur"
  fi
  sleep 0.5
done

if [ ! -s "$OUT" ]; then
  echo "error: render timed out, no PNG produced" >&2
  exit 1
fi

echo "rendered: $OUT"
