#!/usr/bin/env bash
set -euo pipefail

# memoryd installer
# Usage:
#   ./install.sh                              # local Docker MongoDB
#   ./install.sh --atlas "mongodb+srv://..."  # Atlas connection string

ATLAS_URI=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --atlas) ATLAS_URI="$2"; shift 2 ;;
    *) echo "Unknown option: $1"; echo "Usage: $0 [--atlas <connection_string>]"; exit 1 ;;
  esac
done

MEMORYD_DIR="$HOME/.memoryd"
MODEL_DIR="$MEMORYD_DIR/models"
MODEL_PATH="$MODEL_DIR/voyage-4-nano.gguf"
MODEL_URL="https://huggingface.co/jsonMartin/voyage-4-nano-gguf/resolve/main/voyage-4-nano-q8_0.gguf?download=true"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
CONFIG_PATH="$MEMORYD_DIR/config.yaml"

ok()   { printf '  \033[32m✓\033[0m %s\n' "$1"; }
fail() { printf '  \033[31m✗\033[0m %s\n' "$1"; }
info() { printf '  \033[34m→\033[0m %s\n' "$1"; }
step() { printf '\n\033[1m%s\033[0m\n' "$1"; }

# ── Pre-flight checks ──────────────────────────────────────────────

step "Pre-flight checks"

# Go
if command -v go &>/dev/null; then
  GO_VER=$(go version | grep -oE 'go[0-9]+\.[0-9]+' | head -1)
  ok "Go installed ($GO_VER)"
else
  fail "Go is not installed"
  info "Install with: brew install go"
  exit 1
fi

# llama.cpp
if command -v llama-server &>/dev/null; then
  ok "llama-server installed"
else
  info "Installing llama.cpp from GitHub release..."
  OS_NAME="$(uname -s | tr '[:upper:]' '[:lower:]')"
  ARCH_NAME="$(uname -m)"
  LLAMA_REPO="ggerganov/llama.cpp"
  LLAMA_TAG=$(curl -fsSL "https://api.github.com/repos/$LLAMA_REPO/releases/latest" 2>/dev/null | grep '"tag_name"' | head -1 | sed -E 's/.*"([^"]+)".*/\1/' || echo "")
  if [[ -z "$LLAMA_TAG" ]]; then
    fail "Could not determine latest llama.cpp release"
    exit 1
  fi
  if [[ "$OS_NAME" == "darwin" ]]; then
    LLAMA_ASSET="llama-${LLAMA_TAG}-bin-macos-arm64.tar.gz"
    [[ "$ARCH_NAME" == "x86_64" ]] && LLAMA_ASSET="llama-${LLAMA_TAG}-bin-macos-x64.tar.gz"
  else
    LLAMA_ASSET="llama-${LLAMA_TAG}-bin-ubuntu-x64.tar.gz"
  fi
  LLAMA_TMPDIR=$(mktemp -d)
  curl -fL --progress-bar -o "$LLAMA_TMPDIR/$LLAMA_ASSET" "https://github.com/$LLAMA_REPO/releases/download/${LLAMA_TAG}/${LLAMA_ASSET}"
  mkdir -p "$LLAMA_TMPDIR/llama"
  tar -xzf "$LLAMA_TMPDIR/$LLAMA_ASSET" -C "$LLAMA_TMPDIR/llama"
  LLAMA_SERVER=$(find "$LLAMA_TMPDIR/llama" -name "llama-server" -type f | head -1)
  if [[ -z "$LLAMA_SERVER" ]]; then
    fail "llama-server not found in release archive"
    rm -rf "$LLAMA_TMPDIR"
    exit 1
  fi
  chmod +x "$LLAMA_SERVER"
  sudo cp "$LLAMA_SERVER" /usr/local/bin/llama-server
  rm -rf "$LLAMA_TMPDIR"
  ok "llama-server → /usr/local/bin/llama-server"
fi

# Docker (only required when not using Atlas)
if [[ -z "$ATLAS_URI" ]]; then
  if command -v docker &>/dev/null; then
    if docker info &>/dev/null; then
      ok "Docker is running"
    else
      fail "Docker is installed but not running"
      info "Start Docker Desktop and try again"
      exit 1
    fi
  else
    fail "Docker is not installed (required for local MongoDB)"
    info "Install Docker Desktop, or pass --atlas <uri> to use Atlas instead"
    exit 1
  fi
else
  ok "Using Atlas connection string"
fi

# ── MongoDB ─────────────────────────────────────────────────────────

step "MongoDB"

if [[ -z "$ATLAS_URI" ]]; then
  MONGO_URI="mongodb://localhost:27017/?directConnection=true"

  if docker ps --format '{{.Names}}' | grep -q '^memoryd-mongo$'; then
    ok "memoryd-mongo container already running"
  elif docker ps -a --format '{{.Names}}' | grep -q '^memoryd-mongo$'; then
    info "Starting existing memoryd-mongo container..."
    docker start memoryd-mongo >/dev/null
    ok "memoryd-mongo started"
  else
    info "Creating memoryd-mongo container..."
    docker run -d --name memoryd-mongo -p 27017:27017 mongodb/mongodb-atlas-local:8.0 >/dev/null
    ok "memoryd-mongo created"
    info "Waiting for MongoDB to be ready..."
    for i in $(seq 1 30); do
      if docker exec memoryd-mongo mongosh --quiet --eval 'db.runCommand({ping:1}).ok' 2>/dev/null | grep -q 1; then
        break
      fi
      sleep 1
    done
  fi

  # Create vector search index.
  if [[ -f "$SCRIPT_DIR/scripts/create_index.js" ]]; then
    info "Creating vector search index..."
    docker cp "$SCRIPT_DIR/scripts/create_index.js" memoryd-mongo:/tmp/create_index.js 2>/dev/null
    docker exec memoryd-mongo mongosh memoryd --quiet --file /tmp/create_index.js 2>/dev/null || true
    ok "Vector search index ready"
  else
    fail "scripts/create_index.js not found — vector search won't work"
    exit 1
  fi
else
  MONGO_URI="$ATLAS_URI"
  ok "Using Atlas: ${ATLAS_URI:0:30}..."
  info "Make sure you've created a vector search index named 'vector_index'"
  info "on the 'memories' collection with numDimensions: 1024, similarity: cosine"
fi

# ── Embedding model ─────────────────────────────────────────────────

step "Embedding model"

mkdir -p "$MODEL_DIR"

if [[ -f "$MODEL_PATH" ]]; then
  ok "Model already downloaded"
else
  info "Downloading voyage-4-nano (Q8_0, ~70MB)..."
  curl -L --progress-bar -o "$MODEL_PATH" "$MODEL_URL"
  ok "Model downloaded"
fi

# ── Config ──────────────────────────────────────────────────────────

step "Config"

mkdir -p "$MEMORYD_DIR"

if [[ -f "$CONFIG_PATH" ]]; then
  ok "Config exists at $CONFIG_PATH"
  # Update the URI if it changed.
  if grep -q 'mongodb_atlas_uri:' "$CONFIG_PATH"; then
    info "Updating mongodb_atlas_uri in existing config"
    if [[ "$(uname)" == "Darwin" ]]; then
      sed -i '' "s|mongodb_atlas_uri:.*|mongodb_atlas_uri: \"$MONGO_URI\"|" "$CONFIG_PATH"
    else
      sed -i "s|mongodb_atlas_uri:.*|mongodb_atlas_uri: \"$MONGO_URI\"|" "$CONFIG_PATH"
    fi
  fi
else
  cat > "$CONFIG_PATH" << EOF
port: 7432
mongodb_atlas_uri: "$MONGO_URI"
mongodb_database: memoryd
model_path: ~/.memoryd/models/voyage-4-nano.gguf
embedding_dim: 1024
retrieval_top_k: 5
retrieval_max_tokens: 2048
upstream_anthropic_url: https://api.anthropic.com
EOF
  ok "Config written to $CONFIG_PATH"
fi

# ── Build ───────────────────────────────────────────────────────────

step "Build"

cd "$SCRIPT_DIR"
info "Building memoryd + tray app..."
make app 2>&1
MEMORYD_BIN="$SCRIPT_DIR/bin/memoryd"
ok "bin/memoryd and Memoryd.app built"

# ── Claude Code MCP config ─────────────────────────────────────────

step "Claude Code MCP"

MCP_CONFIG="$HOME/.mcp.json"

if [[ -f "$MCP_CONFIG" ]]; then
  if grep -q '"memoryd"' "$MCP_CONFIG" 2>/dev/null; then
    ok "memoryd already in $MCP_CONFIG"
  else
    info "Adding memoryd to existing $MCP_CONFIG"
    python3 -c "
import json
with open('$MCP_CONFIG') as f:
    cfg = json.load(f)
cfg.setdefault('mcpServers', {})['memoryd'] = {
    'command': '$MEMORYD_BIN',
    'args': ['mcp']
}
with open('$MCP_CONFIG', 'w') as f:
    json.dump(cfg, f, indent=2)
" 2>/dev/null && ok "Added memoryd to $MCP_CONFIG" || fail "Could not update $MCP_CONFIG — add manually"
  fi
else
  cat > "$MCP_CONFIG" << EOF
{
  "mcpServers": {
    "memoryd": {
      "command": "$MEMORYD_BIN",
      "args": ["mcp"]
    }
  }
}
EOF
  ok "Created $MCP_CONFIG with memoryd MCP server"
fi

# ── Claude Desktop MCP config ──────────────────────────────────────

CLAUDE_CONFIG_DIR="$HOME/Library/Application Support/Claude"
CLAUDE_CONFIG="$CLAUDE_CONFIG_DIR/claude_desktop_config.json"

if [[ "$(uname)" == "Darwin" && -d "$CLAUDE_CONFIG_DIR" ]]; then
  if [[ -f "$CLAUDE_CONFIG" ]]; then
    if grep -q '"memoryd"' "$CLAUDE_CONFIG" 2>/dev/null; then
      ok "memoryd already in Claude Desktop config"
    else
      info "Adding memoryd to Claude Desktop config"
      python3 -c "
import json
with open('$CLAUDE_CONFIG') as f:
    cfg = json.load(f)
cfg.setdefault('mcpServers', {})['memoryd'] = {
    'command': '$MEMORYD_BIN',
    'args': ['mcp']
}
with open('$CLAUDE_CONFIG', 'w') as f:
    json.dump(cfg, f, indent=2)
" 2>/dev/null && ok "Added memoryd to Claude Desktop config" || info "Could not update Claude Desktop config — add manually"
    fi
  fi
fi

# ── Start everything ───────────────────────────────────────────────

step "Starting memoryd"

# Stop any existing daemon.
if curl -sf "http://127.0.0.1:7432/health" >/dev/null 2>&1; then
  info "Stopping existing daemon..."
  curl -sf -X POST "http://127.0.0.1:7432/shutdown" >/dev/null 2>&1 || true
  sleep 1
fi

if [[ "$(uname)" == "Darwin" && -d "$SCRIPT_DIR/bin/Memoryd.app" ]]; then
  pkill -f "Memoryd.app" 2>/dev/null || true
  sleep 0.5
  open "$SCRIPT_DIR/bin/Memoryd.app"
  ok "Memoryd.app launched (menu bar + daemon)"
else
  nohup "$MEMORYD_BIN" start > "$MEMORYD_DIR/daemon.log" 2>&1 &
  ok "Daemon started in background (PID $!)"
fi

# Wait for daemon to be healthy.
info "Waiting for daemon to be ready..."
for i in $(seq 1 20); do
  if curl -sf "http://127.0.0.1:7432/health" >/dev/null 2>&1; then
    break
  fi
  sleep 1
done

if curl -sf "http://127.0.0.1:7432/health" >/dev/null 2>&1; then
  ok "Daemon is healthy"
else
  fail "Daemon did not start — check $MEMORYD_DIR/daemon.log"
fi

# ── Done ────────────────────────────────────────────────────────────

step "Ready!"

echo ""
echo "  memoryd is running and ready to use."
echo ""
echo "  Dashboard:     http://127.0.0.1:7432"
echo "  Proxy mode:    export ANTHROPIC_BASE_URL=http://127.0.0.1:7432"
echo "  MCP mode:      Already configured in ~/.mcp.json"
echo ""
if [[ "$(uname)" == "Darwin" ]]; then
echo "  The Memoryd menu bar app is running — look for M● in your menu bar."
echo "  Use it to start/stop the daemon, switch modes, and manage sources."
echo ""
fi
