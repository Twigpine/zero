#!/usr/bin/env bash
# Build a complete macOS .app bundle for Zero with Terminal launcher,
# .icns icon, ad-hoc code signature, and npm helpers.
set -euo pipefail

usage() {
  echo "Usage: $0 --staging-dir DIR --output-dir DIR --version X.Y.Z --arch ARCH"
  exit 1
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --staging-dir) STAGING_DIR="$2"; shift 2 ;;
    --output-dir)  OUTPUT_DIR="$2";  shift 2 ;;
    --version)     VERSION="$2";     shift 2 ;;
    --arch)        ARCH="$2";        shift 2 ;;
    *) usage ;;
  esac
done

: "${STAGING_DIR:?missing}"
: "${OUTPUT_DIR:?missing}"
: "${VERSION:?missing}"
: "${ARCH:?missing}"

case "$ARCH" in
  x64|amd64)        PKG_ARCH="x64"   ;;
  arm64|aarch64)    PKG_ARCH="arm64" ;;
  *) echo "unknown arch: $ARCH"; exit 1 ;;
esac

PKG_NAME="zero-v${VERSION}-macos-${PKG_ARCH}"
APP_BUNDLE="${OUTPUT_DIR}/Zero.app"
APPZIP="${OUTPUT_DIR}/${PKG_NAME}.zip"

rm -rf "${APP_BUNDLE}"

mkdir -p "${APP_BUNDLE}/Contents/MacOS"
mkdir -p "${APP_BUNDLE}/Contents/Resources"

cat > "${APP_BUNDLE}/Contents/MacOS/ZeroLauncher" <<'LAUNCHER'
#!/bin/bash
set -euo pipefail

ZERO_DIR="$(dirname "$0")"
ZERO_HELPERS_DIR="$(dirname "$ZERO_DIR")/Helpers"

TMPFILE="$(mktemp /tmp/zero-launch.XXXXXX)"
cat > "$TMPFILE" << EOF
export ZERO_HELPERS_DIR='$ZERO_HELPERS_DIR'
echo 'Zero v__VERSION__'
echo ''
'$ZERO_DIR/zero'
rm -f "\$0"
EOF

chmod +x "$TMPFILE"
open -a Terminal "$TMPFILE"
LAUNCHER

sed -i "s/__VERSION__/${VERSION}/g" "${APP_BUNDLE}/Contents/MacOS/ZeroLauncher"
chmod 755 "${APP_BUNDLE}/Contents/MacOS/ZeroLauncher"

cp "${STAGING_DIR}/zero" "${APP_BUNDLE}/Contents/MacOS/zero"
chmod 755 "${APP_BUNDLE}/Contents/MacOS/zero"

cat > "${APP_BUNDLE}/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleDevelopmentRegion</key>
  <string>en-US</string>
  <key>CFBundleDisplayName</key>
  <string>Zero</string>
  <key>CFBundleExecutable</key>
  <string>ZeroLauncher</string>
  <key>CFBundleIconFile</key>
  <string>zero</string>
  <key>CFBundleIdentifier</key>
  <string>io.gitlawb.zero</string>
  <key>CFBundleInfoDictionaryVersion</key>
  <string>6.0</string>
  <key>CFBundleName</key>
  <string>Zero</string>
  <key>CFBundlePackageType</key>
  <string>APPL</string>
  <key>CFBundleShortVersionString</key>
  <string>${VERSION}</string>
  <key>CFBundleSignature</key>
  <string>????</string>
  <key>CFBundleVersion</key>
  <string>${VERSION}</string>
  <key>LSApplicationCategoryType</key>
  <string>public.app-category.developer-tools</string>
  <key>LSMinimumSystemVersion</key>
  <string>13.0</string>
  <key>NSHighResolutionCapable</key>
  <true/>
  <key>NSSupportsAutomaticGraphicsSwitching</key>
  <true/>
  <key>NSAppleEventsUsageDescription</key>
  <string>Zero needs to run shell commands in Terminal to function as a coding agent.</string>
  <key>NSHumanReadableCopyright</key>
  <string>Copyright $(date +%Y) Gitlawb. MIT License.</string>
</dict>
</plist>
PLIST

echo "APPL????" > "${APP_BUNDLE}/Contents/PkgInfo"

ICONSET="${OUTPUT_DIR}/zero.iconset"
ICON_SRC="docs/assets/logo-great.png"
mkdir -p "${ICONSET}"

if [ -f "${ICON_SRC}" ]; then
  for SIZE in 16 32 64 128 256 512; do
    sips -z "${SIZE}" "${SIZE}" "${ICON_SRC}" \
      --out "${ICONSET}/icon_${SIZE}x${SIZE}.png" >/dev/null 2>&1 || true
    RETINA=$((SIZE * 2))
    sips -z "${RETINA}" "${RETINA}" "${ICON_SRC}" \
      --out "${ICONSET}/icon_${SIZE}x${SIZE}@2x.png" >/dev/null 2>&1 || true
  done
  sips -z 1024 1024 "${ICON_SRC}" \
    --out "${ICONSET}/icon_512x512@2x.png" >/dev/null 2>&1 || true

  iconutil -c icns "${ICONSET}" -o "${APP_BUNDLE}/Contents/Resources/zero.icns"
  rm -rf "${ICONSET}"
else
  echo "Warning: ${ICON_SRC} not found, skipping icon." >&2
fi

if [ -f "LICENSE" ]; then
  cp "LICENSE" "${APP_BUNDLE}/Contents/Resources/LICENSE"
fi

if [ -d "${STAGING_DIR}/helpers" ]; then
  cp -r "${STAGING_DIR}/helpers" "${APP_BUNDLE}/Contents/Helpers"
fi

codesign --force --deep --sign - "${APP_BUNDLE}" 2>/dev/null || \
  echo "Warning: ad-hoc code signing failed (non-fatal)." >&2

cd "${OUTPUT_DIR}"
zip -r -y "${APPZIP}" "Zero.app" -x "*.DS_Store"
cd - >/dev/null
rm -rf "${APP_BUNDLE}"

sha256sum "${APPZIP}" | sed 's/  */  /' > "${APPZIP}.sha256"
echo "Created ${APPZIP}"
