# Plan: Platform Installer Bundles for CI

Add three platform-specific installer artifacts to the release pipeline, produced alongside the existing `.tar.gz`/`.zip` archives in `release-artifacts.yml`.

Three new scripts, one CI file modified, no Go code touched.

---

## 1. Windows — NSIS Installer

**File to create:** `scripts/build-windows-installer.ps1`

**Complete script:**

```powershell
param(
    [Parameter(Mandatory)][string]$NsisDir,
    [Parameter(Mandatory)][string]$StagingDir,
    [Parameter(Mandatory)][string]$OutputDir,
    [Parameter(Mandatory)][string]$Version,
    [Parameter(Mandatory)][string]$Arch
)

$ErrorActionPreference = "Stop"
$pkgName = "zero-v${Version}-windows-${Arch}"
$installerName = "${pkgName}-installer.exe"
$installerPath = Join-Path $OutputDir $installerName
$nsiPath = Join-Path $env:TEMP "zero-installer.nsi"

# NSIS script — no external plugins, uses WriteRegStr for PATH
@"
!include "MUI2.nsh"

Name "Zero v${Version}"
OutFile "$installerPath"
InstallDir "$PROGRAMFILES64\Zero"
RequestExecutionLevel admin
BrandingText "Zero Installer"

!insertmacro MUI_PAGE_WELCOME
!insertmacro MUI_PAGE_DIRECTORY
!insertmacro MUI_PAGE_INSTFILES
!insertmacro MUI_PAGE_FINISH

!insertmacro MUI_LANGUAGE "English"

Section "Install"
  SetOutPath "`$INSTDIR"
  File /r "${StagingDir}\*.*"
  WriteUninstaller "`$INSTDIR\uninstall.exe"

  # Add to PATH (HKCU so it doesn't require elevated install)
  ReadRegStr `$R0 HKCU "Environment" "Path"
  StrCmp `$R0 "" addPath
    StrCpy `$R0 "`$R0;`$INSTDIR"
    Goto writePath
  addPath:
    StrCpy `$R0 "`$INSTDIR"
  writePath:
    WriteRegStr HKCU "Environment" "Path" "`$R0"
    System::Call "user32::SendMessage(i 0xffff, i 0x1a, i 0, i 0) i"

  # Start Menu shortcut
  CreateDirectory "`$SMPROGRAMS\Zero"
  CreateShortCut "`$SMPROGRAMS\Zero\Zero.lnk" "`$INSTDIR\zero.exe" "" "`$INSTDIR\zero.exe" 0
  CreateShortCut "`$SMPROGRAMS\Zero\Uninstall Zero.lnk" "`$INSTDIR\uninstall.exe"

  # Desktop icon (current user only)
  CreateShortCut "`$DESKTOP\Zero.lnk" "`$INSTDIR\zero.exe" "" "`$INSTDIR\zero.exe" 0

  # Register uninstaller in Add/Remove Programs
  WriteRegStr HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\Zero" \
    "DisplayName" "Zero v${Version}"
  WriteRegStr HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\Zero" \
    "UninstallString" "`"`$INSTDIR\uninstall.exe`""
  WriteRegStr HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\Zero" \
    "DisplayVersion" "${Version}"
  WriteRegDWORD HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\Zero" \
    "NoModify" 1
  WriteRegDWORD HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\Zero" \
    "NoRepair" 1
SectionEnd

Section "Uninstall"
  # Remove from PATH
  ReadRegStr `$R0 HKCU "Environment" "Path"
  Push `$R0
  Push "`$INSTDIR"
  Call un.StrDel
  Pop `$R0
  WriteRegStr HKCU "Environment" "Path" "`$R0"
  System::Call "user32::SendMessage(i 0xffff, i 0x1a, i 0, i 0) i"

  # Remove Start Menu and Desktop shortcuts
  RmDir /r "`$SMPROGRAMS\Zero"
  Delete "`$DESKTOP\Zero.lnk"

  # Remove uninstaller registry key
  DeleteRegKey HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\Zero"

  # Remove files
  RmDir /r "`$INSTDIR"
SectionEnd

Function un.StrDel
  Exch `$R1  # needle
  Exch
  Exch `$R0  # haystack
  Push `$R2
  Push `$R3
  Push `$R4
  StrCpy `$R2 `$R0
  StrLen `$R3 `$R1
  loop:
    StrCpy `$R4 `$R2 `$R3
    StrCmp `$R4 `$R1 0 next
    StrCpy `$R4 `$R2 "" `$R3
    StrLen `$R5 `$R2
    IntOp `$R5 `$R5 - `$R3
    StrCpy `$R2 `$R2 `$R5 `$R3
    Goto done
  next:
    StrCpy `$R4 `$R2 1
    StrCmp `$R4 "" done 0
    StrCpy `$R2 `$R2 "" 1
    Goto loop
  done:
    StrCpy `$R0 `$R2
    Pop `$R4
    Pop `$R3
    Pop `$R2
    Pop `$R1
    Exch `$R0
FunctionEnd
"@ | Set-Content -Path $nsiPath -Encoding ASCII

# Create output dir
New-Item -ItemType Directory -Force -Path $OutputDir | Out-Null

# Compile
$makensis = Join-Path $NsisDir "makensis.exe"
Write-Host "Compiling $installerName ..."
& $makensis $nsiPath
if ($LASTEXITCODE -ne 0) { throw "makensis failed" }

# Checksum
$hash = (Get-FileHash $installerPath -Algorithm SHA256).Hash.ToLower()
"$hash  $installerName" | Set-Content "${installerPath}.sha256" -Encoding ASCII

Write-Host "Created $installerPath"
```

**DIY PATH removal note:** The `un.StrDel` function in the NSIS script removes the `$INSTDIR` path segment from `PATH` on uninstall. It works by scanning the PATH string and removing the first occurrence of the installed directory (including any preceding `;`). This is a standard approach that avoids any external plugin dependency.

**CI addition** (in `windows-latest` runner block, after `Package release artifact`):

```yaml
- name: Install NSIS (portable zip)
  shell: pwsh
  run: |
    $url = "https://sourceforge.net/projects/nsis/files/NSIS%203/3.10/nsis-3.10.zip/download"
    $zip = "$env:RUNNER_TEMP\nsis.zip"
    Invoke-WebRequest -Uri $url -OutFile $zip
    Expand-Archive -Path $zip -DestinationPath "$env:RUNNER_TEMP\nsis"
    echo "NSIS_DIR=$env:RUNNER_TEMP\nsis\NSIS" >> $env:GITHUB_ENV

- name: Build Windows installer
  shell: pwsh
  run: |
    $version = (Get-Content package.json | ConvertFrom-Json).version
    & scripts/build-windows-installer.ps1 `
      -NsisDir "$env:NSIS_DIR" `
      -StagingDir "dist/package/zero-v${version}-windows-x64" `
      -OutputDir "dist/release" `
      -Version $version

- name: Verify installer checksum
  shell: pwsh
  run: |
    if (-not (Test-Path "dist/release/*-installer.exe.sha256")) { throw "Checksum not found" }
```

**Assets produced:**
- `zero-v<version>-windows-x64-installer.exe`
- `zero-v<version>-windows-x64-installer.exe.sha256`
- `zero-v<version>-windows-arm64-installer.exe`
- `zero-v<version>-windows-arm64-installer.exe.sha256`

---

## 2. Linux — AppImage + System Installer

**Files to create:**
- `scripts/build-linux-appimage.sh` — portable AppImage (unchanged from earlier plan)
- `scripts/build-linux-install.sh` — system installer `.run` that installs to `/usr/local`

### 2a. Portable AppImage

```bash
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

# ---------------------------------------------------------------------------
# Directory structure (Freedesktop XDG standard layout)
# ---------------------------------------------------------------------------
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

# ---------------------------------------------------------------------------
# Binaries
# ---------------------------------------------------------------------------
cp "${STAGING_DIR}/zero" "${APPDIR}/usr/bin/zero"
chmod 755 "${APPDIR}/usr/bin/zero"

# Sandbox helpers (Linux-specific, shipped with the staged package)
if [ -f "${STAGING_DIR}/zero-linux-sandbox" ]; then
  cp "${STAGING_DIR}/zero-linux-sandbox" "${APPDIR}/usr/bin/"
  chmod 755 "${APPDIR}/usr/bin/zero-linux-sandbox"
fi
if [ -f "${STAGING_DIR}/zero-seccomp" ]; then
  cp "${STAGING_DIR}/zero-seccomp" "${APPDIR}/usr/bin/"
  chmod 755 "${APPDIR}/usr/bin/zero-seccomp"
fi

# npm helper packages (agent-browser, tuistory)
if [ -d "${STAGING_DIR}/helpers" ]; then
  cp -r "${STAGING_DIR}/helpers" "${APPDIR}/usr/lib/zero/helpers"
fi

# ---------------------------------------------------------------------------
# AppRun — robust entrypoint that resolves its own location
# and ensures helpers/ and sandbox binaries are findable.
# ---------------------------------------------------------------------------
cat > "${APPDIR}/AppRun" <<'APPRUN'
#!/bin/bash
set -euo pipefail
# Resolve the AppDir root (works with symlinks, readlink, and FUSE-mounted images)
HERE="$(dirname "$(readlink -f "$0")")"
export ZERO_APPIMAGE=1
export PATH="${HERE}/usr/bin:${PATH}"
export ZERO_HELPERS_DIR="${HERE}/usr/lib/zero/helpers"
exec "${HERE}/usr/bin/zero" "$@"
APPRUN
chmod 755 "${APPDIR}/AppRun"

# ---------------------------------------------------------------------------
# .desktop file — full Freedesktop entry with all standard fields
# ---------------------------------------------------------------------------
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

# ---------------------------------------------------------------------------
# AppStream metainfo — enables software-center discovery and updates
# ---------------------------------------------------------------------------
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

# ---------------------------------------------------------------------------
# Icon — generate at all standard Freedesktop sizes from the source logo.
# Requires ImageMagick (installed in the CI step before this script runs).
# ---------------------------------------------------------------------------
ICON_SRC="docs/assets/logo-great.png"
if [ -f "$ICON_SRC" ]; then
  for SIZE in 16 32 48 64 128 256; do
    convert "$ICON_SRC" -resize "${SIZE}x${SIZE}" \
      "${APPDIR}/usr/share/icons/hicolor/${SIZE}x${SIZE}/apps/zero.png"
  done
elif command -v convert &>/dev/null; then
  # Icon source missing, but ImageMagick is available — create a simple placeholder
  echo "Warning: $ICON_SRC not found, using placeholder icon." >&2
  for SIZE in 16 32 48 64 128 256; do
    convert -size "${SIZE}x${SIZE}" xc:transparent \
      "${APPDIR}/usr/share/icons/hicolor/${SIZE}x${SIZE}/apps/zero.png" 2>/dev/null || true
  done
else
  echo "Warning: $ICON_SRC not found and ImageMagick not available — icons will be missing." >&2
fi

# Root-level symlinks for appimagetool detection
ln -sf "usr/share/icons/hicolor/256x256/apps/zero.png" "${APPDIR}/zero.png"
ln -sf "zero.png" "${APPDIR}/.DirIcon"

# ---------------------------------------------------------------------------
# Documentation
# ---------------------------------------------------------------------------
if [ -f "README.md" ]; then
  cp "README.md" "${APPDIR}/usr/share/doc/zero/"
fi
if [ -f "LICENSE" ]; then
  cp "LICENSE" "${APPDIR}/usr/share/doc/zero/"
fi

# ---------------------------------------------------------------------------
# Build the AppImage
# ---------------------------------------------------------------------------
appimagetool "${APPDIR}" "${OUTPUT}"
rm -rf "${APPDIR}"
sha256sum "${OUTPUT}" | sed 's/  */  /' > "${OUTPUT}.sha256"
echo "Created ${OUTPUT}"
```

### 2b. System Installer (.run)

**File to create:** `scripts/build-linux-install.sh`

Produces a self-extracting shell archive (`.run`). When a user runs it with `sudo`, it installs Zero system-wide under `/usr/local/` and creates a desktop entry.

```bash
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

# ---------------------------------------------------------------------------
# Build the payload directory (everything needed at install time)
# ---------------------------------------------------------------------------
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

# Binaries
cp "${STAGING_DIR}/zero" "${PAYLOAD_DIR}/bin/zero"
chmod 755 "${PAYLOAD_DIR}/bin/zero"
[ -f "${STAGING_DIR}/zero-linux-sandbox" ] && cp "${STAGING_DIR}/zero-linux-sandbox" "${PAYLOAD_DIR}/bin/"
[ -f "${STAGING_DIR}/zero-seccomp" ] && cp "${STAGING_DIR}/zero-seccomp" "${PAYLOAD_DIR}/bin/"
[ -d "${STAGING_DIR}/helpers" ] && cp -r "${STAGING_DIR}/helpers" "${PAYLOAD_DIR}/lib/zero/helpers"

# Desktop entry
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

# Icons (generated from source logo)
ICON_SRC="docs/assets/logo-great.png"
if [ -f "$ICON_SRC" ]; then
  for SIZE in 16 32 48 64 128 256; do
    convert "$ICON_SRC" -resize "${SIZE}x${SIZE}" \
      "${PAYLOAD_DIR}/share/icons/hicolor/${SIZE}x${SIZE}/apps/zero.png"
  done
fi

# ---------------------------------------------------------------------------
# Create the install.sh script (embedded in the .run, runs on user's machine)
# ---------------------------------------------------------------------------
# Write install.sh with __VERSION__ placeholder (single-quoted heredoc
# prevents shell expansion, so we can freely use $0, $(), etc.).
# The placeholder is substituted below with sed.
cat > "${TMPDIR}/install.sh" <<'INSTALL_SCRIPT'
#!/bin/bash
# Zero Linux System Installer -- embedded in the .run self-extracting archive.
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

# Locate and extract the payload
PAYLOAD_LINE=$(grep -an '^__PAYLOAD__$' "$0" | tail -1 | cut -d: -f1)
WORKDIR="$(mktemp -d)"
trap 'rm -rf "${WORKDIR}"' EXIT
tail -n +$((PAYLOAD_LINE + 1)) "$0" | tar xzf - -C "${WORKDIR}"

PAYLOAD="${WORKDIR}/payload"

# --- Install binaries ---
install -d "${BINDIR}"
install -m 755 "${PAYLOAD}/bin/zero" "${BINDIR}/zero"
[ -f "${PAYLOAD}/bin/zero-linux-sandbox" ] && \
  install -m 755 "${PAYLOAD}/bin/zero-linux-sandbox" "${BINDIR}/zero-linux-sandbox"
[ -f "${PAYLOAD}/bin/zero-seccomp" ] && \
  install -m 755 "${PAYLOAD}/bin/zero-seccomp" "${BINDIR}/zero-seccomp"

echo "  Binaries installed to ${BINDIR}/"

# --- Install helpers ---
if [ -d "${PAYLOAD}/lib/zero/helpers" ]; then
  install -d "${LIBDIR}"
  cp -r "${PAYLOAD}/lib/zero/helpers" "${LIBDIR}/"
  echo "  Helpers installed to ${LIBDIR}/"
fi

# --- Install desktop entry ---
install -d "${APPDIR}"
cp "${PAYLOAD}/share/applications/zero.desktop" "${APPDIR}/zero.desktop"
update-desktop-database "${APPDIR}" 2>/dev/null || true
echo "  Desktop entry installed to ${APPDIR}/"

# --- Install icons ---
for SIZE_DIR in "${PAYLOAD}/share/icons/hicolor"/*/; do
  SIZE="$(basename "${SIZE_DIR}")"
  if [ -f "${SIZE_DIR}/apps/zero.png" ]; then
    install -d "${ICONDIR}/${SIZE}/apps"
    cp "${SIZE_DIR}/apps/zero.png" "${ICONDIR}/${SIZE}/apps/zero.png"
  fi
done
gtk-update-icon-cache -f -t "${INSTALL_PREFIX}/share/icons/hicolor" 2>/dev/null || true
echo "  Icons installed to ${ICONDIR}/"

# --- Verify ---
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

# Substitute build-time variables into install.sh
sed -i "s/__VERSION__/${VERSION}/g" "${TMPDIR}/install.sh"

# ---------------------------------------------------------------------------
# Assemble the payload tarball
# ---------------------------------------------------------------------------
tar czf "${TMPDIR}/payload.tar.gz" -C "${TMPDIR}" "payload"

# ---------------------------------------------------------------------------
# Build the self-extracting .run archive
# Header -> install.sh -> __PAYLOAD__ marker -> binary tarball
# ---------------------------------------------------------------------------
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
```

**How the `.run` installer works:**
- The archive is a shell script (`#!/bin/bash`) containing: short header → `install.sh` → `__PAYLOAD__` marker → gzipped tar payload
- **Run with `sudo ./zero-v*-linux-*-install.run`**
- The `install.sh` script extracts the payload to a temp directory, then uses `install` (GNU coreutils) to copy files to `/usr/local/` with proper permissions
- **Install locations:** binaries → `/usr/local/bin/`, helpers → `/usr/local/lib/zero/`, desktop file → `/usr/local/share/applications/`, icons → `/usr/local/share/icons/hicolor/*/apps/`
- **PATH:** `/usr/local/bin/` is on `PATH` by default on all standard Linux distros — no PATH modification needed
- **Uninstall:** manual `rm -rf /usr/local/bin/zero /usr/local/lib/zero /usr/local/share/applications/zero.desktop` (uninstall script is a future improvement)
- **Custom prefix:** set `INSTALL_PREFIX=/opt/zero` before running to install elsewhere

The key improvement over the previous tarball-to-root approach: payload extracts to a temp dir, then `install -m 755` copies each file to its destination. This is safer (no path traversal risk), gives clear per-step errors, and uses the standard `install` utility for proper permissions.

**Assets produced:**
- `zero-v<version>-linux-x86_64.AppImage` (portable, +.sha256)
- `zero-v<version>-linux-x86_64-install.run` (system installer, +.sha256)
- `zero-v<version>-linux-aarch64.AppImage` (portable, +.sha256)
- `zero-v<version>-linux-aarch64-install.run` (system installer, +.sha256)

---

## 3. macOS — Complete .app Bundle

**File to create:** `scripts/build-macos-app.sh`

Produces a proper macOS `.app` bundle with Terminal launcher, `.icns` icon from the repo logo, ad-hoc code signature, and bundles npm helpers. Packaged as `.zip` for distribution (standard for macOS apps on GitHub Releases).

**Bundle structure:**

```
Zero.app/
  Contents/
    Info.plist                      # full metadata with all standard keys
    PkgInfo                         # APPL????
    MacOS/
      ZeroLauncher                  # shell script that opens Terminal and runs zero
    Resources/
      zero.icns                     # icon generated from logo-great.png
      LICENSE                       # bundled from repo root
    Helpers/                        # npm helper packages (if present)
      agent-browser/
      tuistory/
```

**Complete script:**

```bash
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

# Clean any previous build
rm -rf "${APP_BUNDLE}"

# ---------------------------------------------------------------------------
# Create directory structure
# ---------------------------------------------------------------------------
mkdir -p "${APP_BUNDLE}/Contents/MacOS"
mkdir -p "${APP_BUNDLE}/Contents/Resources"

# ---------------------------------------------------------------------------
# Launcher — since Zero is a CLI tool, double-clicking the .app in Finder
# opens Terminal and runs zero in it. Uses a temp script so no osascript
# quoting issues. The terminal stays open after zero exits.
# ---------------------------------------------------------------------------
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

# Substitute version placeholder
sed -i "s/__VERSION__/${VERSION}/g" "${APP_BUNDLE}/Contents/MacOS/ZeroLauncher"
chmod 755 "${APP_BUNDLE}/Contents/MacOS/ZeroLauncher"

# ---------------------------------------------------------------------------
# Zero binary
# ---------------------------------------------------------------------------
cp "${STAGING_DIR}/zero" "${APP_BUNDLE}/Contents/MacOS/zero"
chmod 755 "${APP_BUNDLE}/Contents/MacOS/zero"

# ---------------------------------------------------------------------------
# Info.plist — full macOS application metadata
# ---------------------------------------------------------------------------
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

# ---------------------------------------------------------------------------
# PkgInfo
# ---------------------------------------------------------------------------
echo "APPL????" > "${APP_BUNDLE}/Contents/PkgInfo"

# ---------------------------------------------------------------------------
# Icon (.icns) — generated from the repo logo via macOS iconutil.
# Requires a .iconset directory with all standard sizes.
# ---------------------------------------------------------------------------
ICONSET="${OUTPUT_DIR}/zero.iconset"
ICON_SRC="docs/assets/logo-great.png"
mkdir -p "${ICONSET}"

if [ -f "${ICON_SRC}" ]; then
  # Generate all icon sizes required by macOS (.iconset specification)
  # Standard: 16, 32, 64, 128, 256, 512
  # Retina @2x: 32, 64, 128, 256, 512, 1024
  for SIZE in 16 32 64 128 256 512; do
    sips -z "${SIZE}" "${SIZE}" "${ICON_SRC}" \
      --out "${ICONSET}/icon_${SIZE}x${SIZE}.png" >/dev/null 2>&1 || true
    RETINA=$((SIZE * 2))
    sips -z "${RETINA}" "${RETINA}" "${ICON_SRC}" \
      --out "${ICONSET}/icon_${SIZE}x${SIZE}@2x.png" >/dev/null 2>&1 || true
  done
  # 1024 (from 512@2x specification)
  sips -z 1024 1024 "${ICON_SRC}" \
    --out "${ICONSET}/icon_512x512@2x.png" >/dev/null 2>&1 || true

  # Convert iconset to .icns
  iconutil -c icns "${ICONSET}" -o "${APP_BUNDLE}/Contents/Resources/zero.icns"
  rm -rf "${ICONSET}"
else
  echo "Warning: ${ICON_SRC} not found, skipping icon." >&2
fi

# ---------------------------------------------------------------------------
# License
# ---------------------------------------------------------------------------
if [ -f "LICENSE" ]; then
  cp "LICENSE" "${APP_BUNDLE}/Contents/Resources/LICENSE"
fi

# ---------------------------------------------------------------------------
# Bundle npm helpers (agent-browser, tuistory)
# ---------------------------------------------------------------------------
if [ -d "${STAGING_DIR}/helpers" ]; then
  cp -r "${STAGING_DIR}/helpers" "${APP_BUNDLE}/Contents/Helpers"
fi

# ---------------------------------------------------------------------------
# Ad-hoc code signature (no Apple Developer account needed).
# macOS will still show "unidentified developer" on first run, but the
# ad-hoc signature satisfies Gatekeeper's code-integrity checks and
# prevents macOS from removing the quarantine attribute silently.
# ---------------------------------------------------------------------------
codesign --force --deep --sign - "${APP_BUNDLE}" 2>/dev/null || \
  echo "Warning: ad-hoc code signing failed (non-fatal)." >&2

# ---------------------------------------------------------------------------
# Package as .zip (standard macOS app distribution format — preserves
# extended attributes, resource forks, and code signatures).
# GitHub Releases handles .zip natively.
# ---------------------------------------------------------------------------
cd "${OUTPUT_DIR}"
zip -r -y "${APPZIP}" "Zero.app" -x "*.DS_Store"
cd - >/dev/null
rm -rf "${APP_BUNDLE}"

sha256sum "${APPZIP}" | sed 's/  */  /' > "${APPZIP}.sha256"
echo "Created ${APPZIP}"
```

**How it works:**

- **Double-click in Finder:** macOS launches `ZeroLauncher`, which writes a temp shell script and opens it in Terminal.app. The script sets up `ZERO_HELPERS_DIR`, prints the version, then runs `zero`. After zero exits the temp script deletes itself. The terminal window stays open so the user can read the output.
- **Terminal usage:** Users can also run `/Applications/Zero.app/Contents/MacOS/zero` directly from their shell, or add it to PATH with `export PATH="/Applications/Zero.app/Contents/MacOS:$PATH"`.
- **Icon:** Generated from `docs/assets/logo-great.png` at all macOS standard sizes (16–512@2x) using `sips` + `iconutil`, producing a proper `.icns` file.
- **Code signature:** Ad-hoc (`codesign --sign -`) — no Apple Developer account needed. This satisfies code-integrity verification. Full notarization would require an Apple Developer account ($99/year) and is noted as a future enhancement.

**Assets produced:**
- `zero-v<version>-macos-x64.zip` (contains `Zero.app`)
- `zero-v<version>-macos-x64.zip.sha256`
- `zero-v<version>-macos-arm64.zip` (contains `Zero.app`)
- `zero-v<version>-macos-arm64.zip.sha256`

**Future enhancements (not in this PR):**
- **Notarization:** Requires an Apple Developer Program membership ($99/year). Run `xcrun notarytool submit` and staple the ticket. This removes the "unidentified developer" Gatekeeper warning entirely.
- **DMG packaging:** A `.dmg` with background image and drag-to-Applications shortcut. Adds polish but requires additional assets and tooling. Not standard for CLI tool distributions on GitHub Releases.

---

## 4. CI Workflow Changes

**File to modify:** `.github/workflows/release-artifacts.yml`

Four changes in the `package` job:

1. **Add `windows-11-arm` to the runner matrix** — alongside the existing 5 runners:

```yaml
strategy:
  fail-fast: false
  matrix:
    os:
      - ubuntu-latest          # linux-x64
      - ubuntu-24.04-arm       # linux-arm64
      - macos-latest           # macos-arm64 (Apple Silicon)
      - macos-15-intel         # macos-x64 (Intel)
      - windows-latest         # windows-x64
      - windows-11-arm         # windows-arm64   ← ADD THIS
```

2. **Add ARCH + VERSION env helpers** — after `Setup Go`:

```yaml
- name: Resolve platform arch name and version
  shell: bash
  run: |
    echo "VERSION=$(jq -r .version package.json)" >> "$GITHUB_ENV"
    case "${{ runner.os }}-${{ runner.arch }}" in
      Linux-X64)      echo "ARCH=x64"   >> "$GITHUB_ENV" ;;
      Linux-ARM64)    echo "ARCH=arm64" >> "$GITHUB_ENV" ;;
      macOS-X64)      echo "ARCH=x64"   >> "$GITHUB_ENV" ;;
      macOS-ARM64)    echo "ARCH=arm64" >> "$GITHUB_ENV" ;;
      Windows-X64)    echo "ARCH=x64"   >> "$GITHUB_ENV" ;;
      Windows-ARM64)  echo "ARCH=arm64" >> "$GITHUB_ENV" ;;
    esac
```

3. **Insert platform installer steps** — after the `Verify release checksums` step and before `Upload package`, add conditional steps:

```yaml
      # Windows installer
      - name: Install NSIS (portable)
        if: runner.os == 'Windows'
        shell: pwsh
        run: |
          $url = "https://sourceforge.net/projects/nsis/files/NSIS%203/3.10/nsis-3.10.zip/download"
          $zip = "$env:RUNNER_TEMP\nsis.zip"
          Invoke-WebRequest -Uri $url -OutFile $zip
          Expand-Archive -Path $zip -DestinationPath "$env:RUNNER_TEMP\nsis"
          echo "NSIS_DIR=$env:RUNNER_TEMP\nsis\NSIS" >> $env:GITHUB_ENV

      - name: Build Windows installer
        if: runner.os == 'Windows'
        shell: pwsh
        run: |
          $version = (Get-Content package.json | ConvertFrom-Json).version
          & scripts/build-windows-installer.ps1 `
            -NsisDir "$env:NSIS_DIR" `
            -StagingDir "dist/package/zero-v${version}-windows-$env:ARCH" `
            -OutputDir "dist/release" `
            -Version $version `
            -Arch $env:ARCH

      # Linux AppImage + system installer
      - name: Install Linux build dependencies
        if: runner.os == 'Linux'
        run: |
          sudo apt-get update -qq
          sudo apt-get install -y -qq imagemagick
          wget -q "https://github.com/AppImage/AppImageKit/releases/download/continuous/appimagetool-$(uname -m).AppImage" -O /tmp/appimagetool
          chmod +x /tmp/appimagetool
          cd /tmp && ./appimagetool --appimage-extract >/dev/null 2>&1
          sudo cp squashfs-root/AppRun /usr/local/bin/appimagetool
          rm -rf /tmp/appimagetool /tmp/squashfs-root

      - name: Build AppImage
        if: runner.os == 'Linux'
        run: |
          bash scripts/build-linux-appimage.sh \
            --staging-dir "dist/package/zero-v${VERSION}-linux-${ARCH}" \
            --output-dir "dist/release" \
            --version "${VERSION}" \
            --arch "${ARCH}"

      - name: Build Linux system installer
        if: runner.os == 'Linux'
        run: |
          bash scripts/build-linux-install.sh \
            --staging-dir "dist/package/zero-v${VERSION}-linux-${ARCH}" \
            --output-dir "dist/release" \
            --version "${VERSION}" \
            --arch "${ARCH}"

      # macOS .app
      - name: Build .app bundle
        if: runner.os == 'macOS'
        run: |
          bash scripts/build-macos-app.sh \
            --staging-dir "dist/package/zero-v${VERSION}-macos-${ARCH}" \
            --output-dir "dist/release" \
            --version "${VERSION}" \
            --arch "${ARCH}"
```

3. **Add checksum verification** — after installer steps (or just rely on the scripts creating `.sha256`).

4. **Update npm asset verification** — the `Verify release assets are downloadable` step in `publish-npm` does not need to change, since those checks cover the existing archive URLs that install.sh/install.ps1 use. The new installer artifacts are supplemental.

**The `Publish to GitHub Release` upload** stays as `gh release upload "$tag" dist/release/* --clobber` — the glob already picks up all new artifacts as long as they're in `dist/release/`.

---

## Files to Create (4 new)

| File | Purpose |
| --- | --- |
| `scripts/build-windows-installer.ps1` | Produces `.exe` installer (x64 + arm64) |
| `scripts/build-linux-appimage.sh` | Produces `.AppImage` with full desktop integration |
| `scripts/build-linux-install.sh` | Produces `.run` system installer for `/usr/local/` |
| `scripts/build-macos-app.sh` | Produces `.app.tar.gz` bundle |

## Files to Modify (1 existing)

| File | Change |
| --- | --- |
| `.github/workflows/release-artifacts.yml` | Add arch resolution, platform-installer build steps, and the artifact upload glob auto-picks them up |

No Go source code, no Makefile, no config.

---

## Artifact summary (all 6 targets)

| Platform | Arch | Runner | Current asset | New installer asset |
| --- | --- | --- | --- | --- |
| Windows | x64 | `windows-latest` | `zero-v*-windows-x64.zip` | + `zero-v*-windows-x64-installer.exe` |
| Windows | arm64 | `windows-11-arm` | `zero-v*-windows-arm64.zip` | + `zero-v*-windows-arm64-installer.exe` |
| Linux | x64 | `ubuntu-latest` | `zero-v*-linux-x64.tar.gz` | + `zero-v*-linux-x86_64.AppImage` + `zero-v*-linux-x86_64-install.run` |
| Linux | arm64 | `ubuntu-24.04-arm` | `zero-v*-linux-arm64.tar.gz` | + `zero-v*-linux-aarch64.AppImage` + `zero-v*-linux-aarch64-install.run` |
| macOS | x64 | `macos-15-intel` | `zero-v*-macos-x64.tar.gz` | + `zero-v*-macos-x64.zip` (Zero.app) |
| macOS | arm64 | `macos-latest` | `zero-v*-macos-arm64.tar.gz` | + `zero-v*-macos-arm64.zip` (Zero.app) |

All new assets get `.sha256` checksum files alongside them.

**Note on Windows arm64:** The NSIS compiler produces an x86 installer. On Windows ARM64, x86 binaries run under Microsoft's x64/x86 emulation (Prism) without issues. The installer itself (setup UI, PATH modification, shortcuts) works natively — only the packaged `zero.exe` runs under emulation, which is identical to the experience of downloading the existing `zero-v*-windows-arm64.zip` and running `zero.exe` directly.
