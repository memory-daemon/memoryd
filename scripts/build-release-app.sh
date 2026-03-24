#!/bin/bash
# Build a Memoryd.app bundle from pre-built binaries in a target directory.
# Usage: ./scripts/build-release-app.sh dist/darwin-arm64
set -euo pipefail

TARGET_DIR="${1:?Usage: $0 <target-dir>}"
APP="$TARGET_DIR/Memoryd.app"

rm -rf "$APP"
mkdir -p "$APP/Contents/MacOS"
mkdir -p "$APP/Contents/Resources"

cp "$TARGET_DIR/memoryd-tray" "$APP/Contents/MacOS/memoryd-tray"
cp "$TARGET_DIR/memoryd"      "$APP/Contents/MacOS/memoryd"

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
