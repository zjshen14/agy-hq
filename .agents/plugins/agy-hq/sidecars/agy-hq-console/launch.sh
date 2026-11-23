#!/bin/sh
# Pure-Go agy-hq Console Sidecar launcher (zero Node.js dependency when `hq` is available).
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
HOME_DIR="${HQ_HOME_DIR:-${HOME:-}}"

find_hq() {
  if [ -n "${HQ_BIN_PATH:-}" ] && [ -x "$HQ_BIN_PATH" ]; then
    printf '%s\n' "$HQ_BIN_PATH"
    return 0
  fi
  for cand in \
    "$SCRIPT_DIR/../../../../../bin/hq" \
    "$SCRIPT_DIR/../../bin/hq" \
    "${HOME_DIR:+$HOME_DIR/.local/bin/hq}" \
    "/opt/homebrew/bin/hq" \
    "/usr/local/bin/hq"
  do
    if [ -n "$cand" ] && [ -x "$cand" ]; then
      printf '%s\n' "$cand"
      return 0
    fi
  done
  if command -v hq >/dev/null 2>&1; then
    command -v hq
    return 0
  fi
  return 1
}

if HQ_BIN=$(find_hq); then
  exec "$HQ_BIN" sidecar console
fi

if command -v node >/dev/null 2>&1 && [ -f "$SCRIPT_DIR/main.mjs" ]; then
  exec node "$SCRIPT_DIR/main.mjs"
fi

echo "[sidecar] agy-hq-console: neither hq nor node found on PATH" >&2
exit 1
