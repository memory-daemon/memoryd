#!/bin/bash
# Build a macOS .app bundle for memoryd-tray
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
APP="$ROOT/bin/Memoryd.app"

echo "Building memoryd-tray..."
cd "$ROOT"
go build -o bin/memoryd-tray ./cmd/memoryd-tray
go build -ldflags "-X main.version=$(git describe --tags --always --dirty 2>/dev/null || echo dev)" -o bin/memoryd ./cmd/memoryd

echo "Creating Memoryd.app bundle..."
rm -rf "$APP"

mkdir -p "$APP/Contents/MacOS"
mkdir -p "$APP/Contents/Resources"

cp bin/memoryd-tray "$APP/Contents/MacOS/memoryd-tray"
cp bin/memoryd "$APP/Contents/MacOS/memoryd"

cat > "$APP/Contents/Info.plist" << 'PLIST'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>CFBundleName</key>
    <string>Memoryd</string>
    <key>CFBundleDisplayName</key>
    <string>Memoryd</string>
    <key>CFBundleIdentifier</key>
    <string>io.memorydaemon.memoryd</string>
    <key>CFBundleVersion</key>
    <string>1.0</string>
    <key>CFBundleShortVersionString</key>
    <string>1.0</string>
    <key>CFBundleExecutable</key>
    <string>memoryd-tray</string>
    <key>LSUIElement</key>
    <true/>
    <key>CFBundlePackageType</key>
    <string>APPL</string>
    <key>NSHighResolutionCapable</key>
    <true/>
</dict>
</plist>
PLIST

echo "Built $APP"
echo "Run: open $APP"
