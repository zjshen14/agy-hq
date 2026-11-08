#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

export PATH="/opt/homebrew/bin:/usr/local/go/bin:$HOME/.local/go/bin:$HOME/.local/bin:$PATH"

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
go build -o ./bin/hq ./cmd/hq

echo "==> [3/5] Initializing private Vault (./local/) & PII pre-commit guard..."
./bin/hq init

if [ -d ".git/hooks" ]; then
  cat > .git/hooks/pre-commit <<'EOF'
#!/bin/sh
# Block any staged files inside local/ or containing the local user's home path / git email
if git diff --cached --name-only | grep -qE '^(local|vault)/'; then
  echo "ERROR: Refusing to commit files inside private local/ Vault."
  exit 1
fi
USER_EMAIL="$(git config user.email 2>/dev/null || true)"
HOME_DIR="$HOME"
if [ -n "$HOME_DIR" ] && git diff --cached -U0 | grep -E '^\+' | grep -qF "$HOME_DIR"; then
  echo "ERROR: Staged diff contains your local HOME path ($HOME_DIR). Keep personal paths in local/."
  exit 1
fi
if [ -n "$USER_EMAIL" ] && git diff --cached -U0 | grep -E '^\+' | grep -qFi "$USER_EMAIL"; then
  echo "ERROR: Staged diff contains your personal email ($USER_EMAIL). Keep personal state in local/."
  exit 1
fi
EOF
  chmod +x .git/hooks/pre-commit
fi

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
