#!/usr/bin/env bash
# Build a system-installable .run (self-extracting archive) for Linux.
# When executed with sudo, installs Zero to /usr/local/ with desktop integration.
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

PKG_NAME="zero-v${VERSION}-linux-${ARCH}"
OUTPUT="${OUTPUT_DIR}/${PKG_NAME}-install.run"
TMPDIR="$(mktemp -d)"

PAYLOAD_DIR="${TMPDIR}/payload"
mkdir -p "${PAYLOAD_DIR}/bin"
mkdir -p "${PAYLOAD_DIR}/lib/zero"
mkdir -p "${PAYLOAD_DIR}/share/applications"
mkdir -p "${PAYLOAD_DIR}/share/icons/hicolor/16x16/apps"
mkdir -p "${PAYLOAD_DIR}/share/icons/hicolor/32x32/apps"
mkdir -p "${PAYLOAD_DIR}/share/icons/hicolor/48x48/apps"
mkdir -p "${PAYLOAD_DIR}/share/icons/hicolor/64x64/apps"
mkdir -p "${PAYLOAD_DIR}/share/icons/hicolor/128x128/apps"
mkdir -p "${PAYLOAD_DIR}/share/icons/hicolor/256x256/apps"

cp "${STAGING_DIR}/zero" "${PAYLOAD_DIR}/bin/zero"
chmod 755 "${PAYLOAD_DIR}/bin/zero"
[ -f "${STAGING_DIR}/zero-linux-sandbox" ] && cp "${STAGING_DIR}/zero-linux-sandbox" "${PAYLOAD_DIR}/bin/"
[ -f "${STAGING_DIR}/zero-seccomp" ] && cp "${STAGING_DIR}/zero-seccomp" "${PAYLOAD_DIR}/bin/"
[ -d "${STAGING_DIR}/helpers" ] && cp -r "${STAGING_DIR}/helpers" "${PAYLOAD_DIR}/lib/zero/helpers"

cat > "${PAYLOAD_DIR}/share/applications/zero.desktop" <<DESKTOP
[Desktop Entry]
Type=Application
Name=Zero
GenericName=Coding Agent
Comment=AI coding agent for your local terminal
Exec=/usr/local/bin/zero
Icon=zero
Terminal=true
Categories=Development;Utility;
Keywords=ai;coding;agent;terminal;developer;
StartupNotify=false
DESKTOP

ICON_SRC="docs/assets/logo-great.png"
if [ -f "$ICON_SRC" ]; then
  for SIZE in 16 32 48 64 128 256; do
    convert "$ICON_SRC" -resize "${SIZE}x${SIZE}" \
      "${PAYLOAD_DIR}/share/icons/hicolor/${SIZE}x${SIZE}/apps/zero.png"
  done
fi

cat > "${TMPDIR}/install.sh" <<'INSTALL_SCRIPT'
#!/bin/bash
set -euo pipefail

INSTALL_PREFIX="${INSTALL_PREFIX:-/usr/local}"
BINDIR="${INSTALL_PREFIX}/bin"
LIBDIR="${INSTALL_PREFIX}/lib/zero"
APPDIR="${INSTALL_PREFIX}/share/applications"
ICONDIR="${INSTALL_PREFIX}/share/icons/hicolor"

if [ "$(id -u)" -ne 0 ]; then
  echo "Error: Zero system install requires root." >&2
  echo "  Run: sudo $0" >&2
  exit 1
fi

echo "Installing Zero to ${INSTALL_PREFIX} ..."

PAYLOAD_LINE=$(grep -an '^__PAYLOAD__$' "$0" | tail -1 | cut -d: -f1)
WORKDIR="$(mktemp -d)"
trap 'rm -rf "${WORKDIR}"' EXIT
tail -n +$((PAYLOAD_LINE + 1)) "$0" | tar xzf - -C "${WORKDIR}"

PAYLOAD="${WORKDIR}/payload"

install -d "${BINDIR}"
install -m 755 "${PAYLOAD}/bin/zero" "${BINDIR}/zero"
[ -f "${PAYLOAD}/bin/zero-linux-sandbox" ] && \
  install -m 755 "${PAYLOAD}/bin/zero-linux-sandbox" "${BINDIR}/zero-linux-sandbox"
[ -f "${PAYLOAD}/bin/zero-seccomp" ] && \
  install -m 755 "${PAYLOAD}/bin/zero-seccomp" "${BINDIR}/zero-seccomp"

echo "  Binaries installed to ${BINDIR}/"

if [ -d "${PAYLOAD}/lib/zero/helpers" ]; then
  install -d "${LIBDIR}"
  cp -r "${PAYLOAD}/lib/zero/helpers" "${LIBDIR}/"
  echo "  Helpers installed to ${LIBDIR}/"
fi

install -d "${APPDIR}"
cp "${PAYLOAD}/share/applications/zero.desktop" "${APPDIR}/zero.desktop"
update-desktop-database "${APPDIR}" 2>/dev/null || true
echo "  Desktop entry installed to ${APPDIR}/"

for SIZE_DIR in "${PAYLOAD}/share/icons/hicolor"/*/; do
  SIZE="$(basename "${SIZE_DIR}")"
  if [ -f "${SIZE_DIR}/apps/zero.png" ]; then
    install -d "${ICONDIR}/${SIZE}/apps"
    cp "${SIZE_DIR}/apps/zero.png" "${ICONDIR}/${SIZE}/apps/zero.png"
  fi
done
gtk-update-icon-cache -f -t "${INSTALL_PREFIX}/share/icons/hicolor" 2>/dev/null || true
echo "  Icons installed to ${ICONDIR}/"

if ! command -v zero &>/dev/null; then
  echo "Warning: ${BINDIR}/zero is not on PATH. Add it:" >&2
  echo "  export PATH=\"${BINDIR}:\$PATH\"" >&2
fi

echo ""
echo "Zero v__VERSION__ installed successfully."
echo "  Binary:  ${BINDIR}/zero"
echo "  Helpers: ${LIBDIR}/"
echo "  Run:     zero"
exit 0
INSTALL_SCRIPT

sed -i "s/__VERSION__/${VERSION}/g" "${TMPDIR}/install.sh"

tar czf "${TMPDIR}/payload.tar.gz" -C "${TMPDIR}" "payload"

{
  echo '#!/bin/bash'
  echo "# Zero v${VERSION} Linux System Installer"
  echo "# Usage: sudo ./$(basename "${OUTPUT}")"
  echo "# Environment: INSTALL_PREFIX (default: /usr/local)"
  echo ''
  cat "${TMPDIR}/install.sh"
  echo '__PAYLOAD__'
  cat "${TMPDIR}/payload.tar.gz"
} > "${OUTPUT}"

chmod 755 "${OUTPUT}"
rm -rf "${TMPDIR}"

sha256sum "${OUTPUT}" | sed 's/  */  /' > "${OUTPUT}.sha256"
echo "Created ${OUTPUT}"
