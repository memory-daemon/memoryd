#!/usr/bin/env bash
set -euo pipefail

# memoryd installer — one command to install and start everything.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/memory-daemon/memoryd/main/install.sh | bash
#   curl -fsSL ... | bash -s -- --atlas "mongodb+srv://..."

REPO="memory-daemon/memoryd"
ATLAS_URI=""
INSTALL_DIR="/usr/local/bin"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --atlas)   ATLAS_URI="$2"; shift 2 ;;
    --dir)     INSTALL_DIR="$2"; shift 2 ;;
    *) echo "Unknown option: $1"; echo "Usage: $0 [--atlas <uri>] [--dir <install_path>]"; exit 1 ;;
  esac
done

MEMORYD_DIR="$HOME/.memoryd"
MODEL_DIR="$MEMORYD_DIR/models"
MODEL_PATH="$MODEL_DIR/voyage-4-nano.gguf"
MODEL_URL="https://huggingface.co/jsonMartin/voyage-4-nano-gguf/resolve/main/voyage-4-nano-q8_0.gguf?download=true"
CONFIG_PATH="$MEMORYD_DIR/config.yaml"
INDEX_URL="https://raw.githubusercontent.com/$REPO/main/scripts/create_index.js"

ok()   { printf '  \033[32m✓\033[0m %s\n' "$1"; }
fail() { printf '  \033[31m✗\033[0m %s\n' "$1"; }
info() { printf '  \033[34m→\033[0m %s\n' "$1"; }
step() { printf '\n\033[1m%s\033[0m\n' "$1"; }

# ── Detect platform ────────────────────────────────────────────────

OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
ARCH="$(uname -m)"
case "$ARCH" in
  x86_64)  ARCH="amd64" ;;
  aarch64) ARCH="arm64" ;;
  arm64)   ARCH="arm64" ;;
  *) fail "Unsupported architecture: $ARCH"; exit 1 ;;
esac

# ── Pre-flight checks ─────────────────────────────────────────────

step "Pre-flight checks"

# curl or wget
if command -v curl &>/dev/null; then
  DOWNLOAD="curl -fSL"
  DOWNLOAD_QUIET="curl -fsSL"
  DOWNLOAD_PROGRESS="curl -fL --progress-bar"
elif command -v wget &>/dev/null; then
  DOWNLOAD="wget -qO-"
  DOWNLOAD_QUIET="wget -qO-"
  DOWNLOAD_PROGRESS="wget -q --show-progress -O"
else
  fail "curl or wget required"; exit 1
fi
ok "Download tool available"

# llama.cpp
if command -v llama-server &>/dev/null; then
  ok "llama-server installed"
else
  info "Installing llama.cpp from GitHub release..."
  LLAMA_REPO="ggerganov/llama.cpp"
  LLAMA_TAG=$($DOWNLOAD_QUIET "https://api.github.com/repos/$LLAMA_REPO/releases/latest" 2>/dev/null | grep '"tag_name"' | head -1 | sed -E 's/.*"([^"]+)".*/\1/' || echo "")
  if [[ -z "$LLAMA_TAG" ]]; then
    fail "Could not determine latest llama.cpp release"
    exit 1
  fi
  info "Latest llama.cpp release: $LLAMA_TAG"

  if [[ "$OS" == "darwin" ]]; then
    LLAMA_ASSET="llama-${LLAMA_TAG}-bin-macos-arm64.tar.gz"
    if [[ "$ARCH" == "amd64" ]]; then
      LLAMA_ASSET="llama-${LLAMA_TAG}-bin-macos-x64.tar.gz"
    fi
  elif [[ "$OS" == "linux" ]]; then
    LLAMA_ASSET="llama-${LLAMA_TAG}-bin-ubuntu-x64.tar.gz"
  else
    fail "No prebuilt llama.cpp for $OS/$ARCH"
    exit 1
  fi

  LLAMA_URL="https://github.com/$LLAMA_REPO/releases/download/${LLAMA_TAG}/${LLAMA_ASSET}"
  LLAMA_TMPDIR=$(mktemp -d)
  if ! $DOWNLOAD_PROGRESS -o "$LLAMA_TMPDIR/$LLAMA_ASSET" "$LLAMA_URL" 2>&1; then
    fail "Could not download llama.cpp from $LLAMA_URL"
    rm -rf "$LLAMA_TMPDIR"
    exit 1
  fi
  mkdir -p "$LLAMA_TMPDIR/llama"
  tar -xzf "$LLAMA_TMPDIR/$LLAMA_ASSET" -C "$LLAMA_TMPDIR/llama"

  # Find llama-server in the extracted archive.
  LLAMA_SERVER=$(find "$LLAMA_TMPDIR/llama" -name "llama-server" -type f | head -1)
  if [[ -z "$LLAMA_SERVER" ]]; then
    fail "llama-server not found in release archive"
    rm -rf "$LLAMA_TMPDIR"
    exit 1
  fi

  chmod +x "$LLAMA_SERVER"
  if [[ -w "$INSTALL_DIR" ]]; then
    cp "$LLAMA_SERVER" "$INSTALL_DIR/llama-server"
  else
    sudo cp "$LLAMA_SERVER" "$INSTALL_DIR/llama-server"
  fi
  rm -rf "$LLAMA_TMPDIR"
  ok "llama-server → $INSTALL_DIR/llama-server"
fi

# Docker (only if no Atlas URI)
if [[ -z "$ATLAS_URI" ]]; then
  if command -v docker &>/dev/null; then
    if docker info &>/dev/null 2>&1; then
      ok "Docker is running"
    else
      fail "Docker is installed but not running"
      info "Start Docker Desktop and try again, or pass --atlas <uri>"
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

# ── Download release ───────────────────────────────────────────────

step "Download memoryd"

LATEST_TAG=$($DOWNLOAD_QUIET "https://api.github.com/repos/$REPO/releases/latest" 2>/dev/null | grep '"tag_name"' | head -1 | sed -E 's/.*"([^"]+)".*/\1/' || echo "")

if [[ -z "$LATEST_TAG" ]]; then
  fail "Could not determine latest release from GitHub"
  info "Check https://github.com/$REPO/releases"
  info "Or build from source: git clone https://github.com/$REPO && cd memoryd && make build"
  exit 1
fi

info "Latest release: $LATEST_TAG"

VERSION="${LATEST_TAG#v}"  # strip leading v
ARCHIVE_NAME="memoryd_${VERSION}_${OS}_${ARCH}.tar.gz"
DOWNLOAD_URL="https://github.com/$REPO/releases/download/${LATEST_TAG}/${ARCHIVE_NAME}"

TMPDIR_DL=$(mktemp -d)
trap 'rm -rf "$TMPDIR_DL"' EXIT

info "Downloading $ARCHIVE_NAME..."
if ! $DOWNLOAD_PROGRESS -o "$TMPDIR_DL/$ARCHIVE_NAME" "$DOWNLOAD_URL" 2>&1; then
  fail "Download failed — archive may not exist for $OS/$ARCH"
  info "Check https://github.com/$REPO/releases/tag/$LATEST_TAG"
  info "Or build from source: git clone https://github.com/$REPO && cd memoryd && make build"
  exit 1
fi

tar -xzf "$TMPDIR_DL/$ARCHIVE_NAME" -C "$TMPDIR_DL"
chmod +x "$TMPDIR_DL/memoryd"
ok "Archive extracted"

# Install to target directory.
if [[ -w "$INSTALL_DIR" ]]; then
  mv "$TMPDIR_DL/memoryd" "$INSTALL_DIR/memoryd"
else
  info "Installing to $INSTALL_DIR (requires sudo)..."
  sudo mv "$TMPDIR_DL/memoryd" "$INSTALL_DIR/memoryd"
fi

MEMORYD_BIN="$INSTALL_DIR/memoryd"
ok "memoryd binary → $MEMORYD_BIN"

# Install macOS menu bar app (separate release asset).
APP_DIR="/Applications"
if [[ "$OS" == "darwin" ]]; then
  APP_ZIP_NAME="Memoryd-darwin-${ARCH}.zip"
  APP_ZIP_URL="https://github.com/$REPO/releases/download/${LATEST_TAG}/${APP_ZIP_NAME}"
  APP_TMPDIR=$(mktemp -d)
  info "Downloading Memoryd.app..."
  if $DOWNLOAD_QUIET -o "$APP_TMPDIR/$APP_ZIP_NAME" "$APP_ZIP_URL" 2>/dev/null; then
    pkill -f "Memoryd.app" 2>/dev/null || true
    sleep 0.5
    rm -rf "$APP_DIR/Memoryd.app"
    unzip -q "$APP_TMPDIR/$APP_ZIP_NAME" -d "$APP_DIR"
    ok "Memoryd.app → $APP_DIR/Memoryd.app"
  else
    info "Memoryd.app not available for $ARCH — menu bar app skipped"
  fi
  rm -rf "$APP_TMPDIR"
fi

trap - EXIT
rm -rf "$TMPDIR_DL"

# ── MongoDB ────────────────────────────────────────────────────────

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
  fi

  info "Waiting for MongoDB to be ready..."
  for i in $(seq 1 30); do
    if docker exec memoryd-mongo mongosh --quiet --eval 'db.runCommand({ping:1}).ok' 2>/dev/null | grep -q 1; then
      break
    fi
    sleep 1
  done
  ok "MongoDB is ready"

  # Create vector search index.
  info "Creating vector search index..."
  TMPINDEX=$(mktemp)
  $DOWNLOAD_QUIET "$INDEX_URL" > "$TMPINDEX" 2>/dev/null || {
    # Fallback: write the index script inline.
    cat > "$TMPINDEX" << 'JSEOF'
db.memories.createSearchIndex(
  "vector_index",
  "vectorSearch",
  {
    fields: [
      {
        type: "vector",
        numDimensions: 1024,
        path: "embedding",
        similarity: "cosine"
      }
    ]
  }
);
JSEOF
  }
  docker cp "$TMPINDEX" memoryd-mongo:/tmp/create_index.js 2>/dev/null
  docker exec memoryd-mongo mongosh memoryd --quiet --file /tmp/create_index.js 2>/dev/null || true
  rm -f "$TMPINDEX"
  ok "Vector search index ready"
else
  MONGO_URI="$ATLAS_URI"
  ok "Using Atlas: ${ATLAS_URI:0:30}..."
  info "Ensure you have a vector search index named 'vector_index'"
  info "on the 'memories' collection (numDimensions: 1024, similarity: cosine)"
fi

# ── Embedding model ────────────────────────────────────────────────

step "Embedding model"

mkdir -p "$MODEL_DIR"

if [[ -f "$MODEL_PATH" ]]; then
  ok "Model already downloaded"
else
  info "Downloading voyage-4-nano (Q8_0, ~70MB)..."
  $DOWNLOAD_PROGRESS -o "$MODEL_PATH" "$MODEL_URL"
  ok "Model downloaded"
fi

# ── Config ─────────────────────────────────────────────────────────

step "Config"

mkdir -p "$MEMORYD_DIR"

if [[ -f "$CONFIG_PATH" ]]; then
  ok "Config exists at $CONFIG_PATH"
  if grep -q 'mongodb_atlas_uri:' "$CONFIG_PATH"; then
    info "Updating mongodb_atlas_uri in existing config"
    if [[ "$OS" == "darwin" ]]; then
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

# ── Claude Code MCP ────────────────────────────────────────────────

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

# ── Claude Desktop MCP ─────────────────────────────────────────────

CLAUDE_CONFIG_DIR="$HOME/Library/Application Support/Claude"
CLAUDE_CONFIG="$CLAUDE_CONFIG_DIR/claude_desktop_config.json"

if [[ "$OS" == "darwin" && -d "$CLAUDE_CONFIG_DIR" ]]; then
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

if [[ "$OS" == "darwin" && -d "$APP_DIR/Memoryd.app" ]]; then
  # Launch the menu bar app — it manages the daemon.
  open "$APP_DIR/Memoryd.app"
  ok "Memoryd.app launched (menu bar + daemon)"
else
  # Linux or no .app: start daemon in background.
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

# ── Done ───────────────────────────────────────────────────────────

step "Ready!"

echo ""
echo "  memoryd is running and ready to use."
echo ""
echo "  Dashboard:     http://127.0.0.1:7432"
echo "  Proxy mode:    export ANTHROPIC_BASE_URL=http://127.0.0.1:7432"
echo "  MCP mode:      Already configured in ~/.mcp.json"
echo ""
if [[ "$OS" == "darwin" && -d "$APP_DIR/Memoryd.app" ]]; then
echo "  The Memoryd menu bar app is running — look for M● in your menu bar."
echo "  Use it to start/stop the daemon, switch modes, and manage sources."
echo ""
fi
