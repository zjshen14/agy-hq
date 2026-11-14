#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

export PATH="/opt/homebrew/bin:/usr/local/go/bin:$HOME/.local/go/bin:$HOME/.local/bin:$PATH"

INIT_FLAGS=()
PLUGIN_FLAGS=()
for arg in "$@"; do
  case "$arg" in
    --demo|--with-sample-data)
      INIT_FLAGS+=("--demo")
      ;;
    --global)
      PLUGIN_FLAGS+=("--global")
      ;;
  esac
done

echo "==> [1/5] Checking Go toolchain..."
if ! command -v go >/dev/null 2>&1; then
  if command -v brew >/dev/null 2>&1; then
    echo "    Go not found; installing via Homebrew..."
    brew install go
  else
    echo "ERROR: Go 1.22+ is required. Install from https://go.dev/dl/ or 'brew install go'." >&2
    exit 1
  fi
fi

echo "==> [2/5] Building ./bin/hq microkernel..."
mkdir -p bin
GIT_SHA="$(git rev-parse --short=12 HEAD 2>/dev/null || echo dev)"
go build -ldflags "-X github.com/zjshen14/agy-hq/internal/version.GitCommit=${GIT_SHA}" -o ./bin/hq ./cmd/hq

echo "==> [3/5] Initializing private Vault (./local/), CI git hooks, & project-scoped hq binary (~/.local/bin/hq)..."
./bin/hq init "${INIT_FLAGS[@]}"
./bin/hq ci install-hooks 2>/dev/null || true
./bin/hq plugin install "${PLUGIN_FLAGS[@]}"

echo "==> [4/5] Auto-arming background daemon & scheduled triggers..."
./bin/hq daemon install 2>/dev/null || true

echo "==> [5/5] Running ./bin/hq doctor..."
./bin/hq doctor

echo ""
echo "agy-hq is ready! Open this folder in Google Antigravity and start your [HQ] Front Desk thread."

if [ -n "${ANTIGRAVITY_CONVERSATION_ID:-}" ]; then
  APP_DATA="${ANTIGRAVITY_APP_DATA_DIR:-$HOME/.gemini/antigravity}"
  ART_DIR="$APP_DATA/brain/$ANTIGRAVITY_CONVERSATION_ID"
  echo ""
  echo "==> Active Antigravity Conversation Detected ($ANTIGRAVITY_CONVERSATION_ID)"
  echo "    Auto-claimed [HQ] Front Desk intake lease!"
  echo "    Action required: Pin $ART_DIR/hq_console.md via write_file"
  echo "    (Overwrite=true, ArtifactMetadata={UserFacing: true, RequestFeedback: false, Summary: \"Live [HQ] Front Desk Console & Task Board\"})"
fi
