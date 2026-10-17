#!/usr/bin/env bash
# Build a fully self-contained Zero AppImage with desktop integration.
# Requires: appimagetool on PATH, ImageMagick (convert) for icon generation.
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
  x64|amd64)        IMG_ARCH="x86_64"  ;;
  arm64|aarch64)    IMG_ARCH="aarch64" ;;
  *) echo "unknown arch: $ARCH"; exit 1 ;;
esac

PKG_NAME="zero-v${VERSION}-linux-${IMG_ARCH}"
APPDIR="${OUTPUT_DIR}/${PKG_NAME}.AppDir"
OUTPUT="${OUTPUT_DIR}/${PKG_NAME}.AppImage"

mkdir -p "${APPDIR}/usr/bin"
mkdir -p "${APPDIR}/usr/lib/zero"
mkdir -p "${APPDIR}/usr/share/applications"
mkdir -p "${APPDIR}/usr/share/metainfo"
mkdir -p "${APPDIR}/usr/share/doc/zero"
mkdir -p "${APPDIR}/usr/share/icons/hicolor/16x16/apps"
mkdir -p "${APPDIR}/usr/share/icons/hicolor/32x32/apps"
mkdir -p "${APPDIR}/usr/share/icons/hicolor/48x48/apps"
mkdir -p "${APPDIR}/usr/share/icons/hicolor/64x64/apps"
mkdir -p "${APPDIR}/usr/share/icons/hicolor/128x128/apps"
mkdir -p "${APPDIR}/usr/share/icons/hicolor/256x256/apps"
mkdir -p "${APPDIR}/usr/share/icons/hicolor/scalable/apps"

cp "${STAGING_DIR}/zero" "${APPDIR}/usr/bin/zero"
chmod 755 "${APPDIR}/usr/bin/zero"

if [ -f "${STAGING_DIR}/zero-linux-sandbox" ]; then
  cp "${STAGING_DIR}/zero-linux-sandbox" "${APPDIR}/usr/bin/"
  chmod 755 "${APPDIR}/usr/bin/zero-linux-sandbox"
fi
if [ -f "${STAGING_DIR}/zero-seccomp" ]; then
  cp "${STAGING_DIR}/zero-seccomp" "${APPDIR}/usr/bin/"
  chmod 755 "${APPDIR}/usr/bin/zero-seccomp"
fi

if [ -d "${STAGING_DIR}/helpers" ]; then
  cp -r "${STAGING_DIR}/helpers" "${APPDIR}/usr/lib/zero/helpers"
fi

cat > "${APPDIR}/AppRun" <<'APPRUN'
#!/bin/bash
set -euo pipefail
HERE="$(dirname "$(readlink -f "$0")")"
export ZERO_APPIMAGE=1
export PATH="${HERE}/usr/bin:${PATH}"
export ZERO_HELPERS_DIR="${HERE}/usr/lib/zero/helpers"
exec "${HERE}/usr/bin/zero" "$@"
APPRUN
chmod 755 "${APPDIR}/AppRun"

cat > "${APPDIR}/zero.desktop" <<DESKTOP
[Desktop Entry]
Type=Application
Name=Zero
GenericName=Coding Agent
Comment=AI coding agent for your local terminal
Exec=zero %F
Icon=zero
Terminal=true
Categories=Development;Utility;
Keywords=ai;coding;agent;terminal;developer;
StartupNotify=false
MimeType=text/plain;
DESKTOP
cp "${APPDIR}/zero.desktop" "${APPDIR}/usr/share/applications/zero.desktop"

cat > "${APPDIR}/usr/share/metainfo/zero.appdata.xml" <<APPDATA
<?xml version="1.0" encoding="UTF-8"?>
<component type="console-application">
  <id>io.gitlawb.zero</id>
  <name>Zero</name>
  <summary>AI coding agent for your local terminal</summary>
  <metadata_license>MIT</metadata_license>
  <project_license>MIT</project_license>
  <description>
    <p>
      Zero is an AI coding agent for your local terminal. It can inspect a
      repository, edit files, run commands, and keep durable local sessions
      while you choose the model and the permission level.
    </p>
  </description>
  <url type="homepage">https://github.com/Gitlawb/zero</url>
  <url type="bugtracker">https://github.com/Gitlawb/zero/issues</url>
  <provides>
    <binary>zero</binary>
  </provides>
  <releases>
    <release version="${VERSION}" date="$(date +%Y-%m-%d)"/>
  </releases>
</component>
APPDATA

ICON_SRC="docs/assets/logo-great.png"
if [ -f "$ICON_SRC" ]; then
  for SIZE in 16 32 48 64 128 256; do
    convert "$ICON_SRC" -resize "${SIZE}x${SIZE}" \
      "${APPDIR}/usr/share/icons/hicolor/${SIZE}x${SIZE}/apps/zero.png"
  done
elif command -v convert &>/dev/null; then
  echo "Warning: $ICON_SRC not found, using placeholder icon." >&2
  for SIZE in 16 32 48 64 128 256; do
    convert -size "${SIZE}x${SIZE}" xc:transparent \
      "${APPDIR}/usr/share/icons/hicolor/${SIZE}x${SIZE}/apps/zero.png" 2>/dev/null || true
  done
else
  echo "Warning: $ICON_SRC not found and ImageMagick not available — icons will be missing." >&2
fi

ln -sf "usr/share/icons/hicolor/256x256/apps/zero.png" "${APPDIR}/zero.png"
ln -sf "zero.png" "${APPDIR}/.DirIcon"

if [ -f "README.md" ]; then
  cp "README.md" "${APPDIR}/usr/share/doc/zero/"
fi
if [ -f "LICENSE" ]; then
  cp "LICENSE" "${APPDIR}/usr/share/doc/zero/"
fi

appimagetool "${APPDIR}" "${OUTPUT}"
rm -rf "${APPDIR}"

sha256sum "${OUTPUT}" | sed 's/  */  /' > "${OUTPUT}.sha256"
echo "Created ${OUTPUT}"
