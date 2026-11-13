#!/usr/bin/env node
// postinstall: fetch the prebuilt `zero` binary for this platform from the
// matching GitHub Release and place it next to bin/zero.js, which execs it.
//
// Mirrors scripts/install.sh and scripts/install.ps1. The asset-name scheme is
// the source of truth in internal/release/release.go:
//   zero-v{version}-{linux|macos|windows}-{x64|arm64}.{tar.gz|zip}  (+ .sha256)
//
// Safety: HTTPS-only download from the pinned repo, SHA-256 verification against
// the release's own .sha256, and extraction that never trusts archive paths —
// we extract to a temp dir and copy only known binary basenames into place, so a
// crafted archive cannot write outside the package (no zip-slip).
//
// Env overrides (testing / mirrors / locked-down installs):
//   ZERO_SKIP_DOWNLOAD=1      skip entirely, exit 0 (wrapper will guide if run)
//   ZERO_INSTALL_DRY_RUN=1    print the resolved plan as JSON, no network, exit 0
//   ZERO_INSTALL_PLATFORM=…   override process.platform (linux|darwin|win32|android)
//   ZERO_INSTALL_ARCH=…       override process.arch (x64|arm64)
//   ZERO_REPO=owner/name      override the GitHub repo (default Twigpine/zero)
//   ZERO_GITHUB_BASE_URL=…    override the download host (default https://github.com)

import {
  chmodSync,
  copyFileSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readdirSync,
  readFileSync,
  rmSync,
  statSync,
  writeFileSync,
} from 'node:fs';
import { createHash } from 'node:crypto';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { spawnSync } from 'node:child_process';

const packageRoot = dirname(dirname(fileURLToPath(import.meta.url)));
const pkg = JSON.parse(readFileSync(join(packageRoot, 'package.json'), 'utf8'));
const VERSION = pkg.version;

const REPO = process.env.ZERO_REPO || 'Twigpine/zero';
const BASE = (process.env.ZERO_GITHUB_BASE_URL || 'https://github.com').replace(/\/+$/, '');
// The .sha256 is fetched from the same origin as the archive, so TLS authenticity
// is the only real integrity control — require https unless explicitly overridden
// for local testing (e.g. a localhost mirror in tests).
const ALLOW_INSECURE = process.env.ZERO_ALLOW_INSECURE_DOWNLOAD === '1';
const MAX_DOWNLOAD_BYTES = 512 * 1024 * 1024;

function fail(message) {
  console.error(`[zero] ${message}`);
  process.exit(1);
}

function warnSkip(message) {
  // Exit 0 so an unsupported platform or an opt-out does not break `npm install`;
  // bin/zero.js reports a clear "no native binary" message if the user runs zero.
  console.error(`[zero] ${message}`);
  process.exit(0);
}

// Maps mirror internal/release/release.go (ReleasePlatform / ReleaseArch).
function resolvePlatform(p) {
  switch (p) {
    case 'linux':
    case 'android':
      return 'linux';
    case 'darwin':
      return 'macos';
    case 'win32':
      return 'windows';
    default:
      return null;
  }
}

function resolveArch(a) {
  switch (a) {
    case 'x64':
      return 'x64';
    case 'arm64':
      return 'arm64';
    default:
      return null;
  }
}

if (process.env.ZERO_SKIP_DOWNLOAD === '1') {
  warnSkip('ZERO_SKIP_DOWNLOAD=1 set — skipping native binary download.');
}

const rawPlatform = process.env.ZERO_INSTALL_PLATFORM || process.platform;
const rawArch = process.env.ZERO_INSTALL_ARCH || process.arch;
const platform = resolvePlatform(rawPlatform);
const arch = resolveArch(rawArch);

if (!platform || !arch) {
  warnSkip(
    `no prebuilt binary for ${rawPlatform}/${rawArch}. Build from source: ` +
      `https://github.com/${REPO} (go run ./cmd/zero).`,
  );
}

// The release matrix builds linux/macos {x64,arm64} and windows-x64 — there is no
// windows-arm64 artifact. (win32, arm64) otherwise resolves to a valid
// platform/arch, so it would proceed to download a non-existent asset and hard
// -fail npm install on a 404. Guard it explicitly to skip gracefully; Windows on
// ARM runs the x64 build under emulation, so the x64 package is the fallback.
if (platform === 'windows' && arch === 'arm64') {
  warnSkip(
    `no prebuilt binary for windows-arm64 (the windows-x64 build runs under ` +
      `emulation on Windows on ARM). Build from source or install the x64 package: ` +
      `https://github.com/${REPO} (go run ./cmd/zero).`,
  );
}

const ext = platform === 'windows' ? 'zip' : 'tar.gz';
const binaryName = platform === 'windows' ? 'zero.exe' : 'zero';
const tag = `v${VERSION}`;
const assetName = `zero-v${VERSION}-${platform}-${arch}.${ext}`;
const assetUrl = `${BASE}/${REPO}/releases/download/${tag}/${assetName}`;
const checksumUrl = `${assetUrl}.sha256`;

// Extra binaries the archive may carry; copied when present so the platform
// sandbox finds its adjacent helpers (tier-1 discovery), but never required.
const optionalBinaries =
  platform === 'windows'
    ? ['zero-windows-command-runner.exe', 'zero-windows-sandbox-setup.exe']
    : platform === 'linux'
      ? ['zero-linux-sandbox', 'zero-seccomp']
      : [];

if (process.env.ZERO_INSTALL_DRY_RUN === '1') {
  process.stdout.write(
    JSON.stringify({
      version: VERSION,
      platform,
      arch,
      tag,
      assetName,
      assetUrl,
      checksumUrl,
      binaryName,
      optionalBinaries,
    }) + '\n',
  );
  process.exit(0);
}

// Idempotent: skip if the binary for this exact version is already in place.
const markerPath = join(packageRoot, '.zero-binary-version');
const installedBinary = join(packageRoot, binaryName);
if (
  existsSync(installedBinary) &&
  existsSync(markerPath) &&
  readFileSync(markerPath, 'utf8').trim() === VERSION
) {
  process.exit(0);
}

async function download(url) {
  if (!ALLOW_INSECURE && !url.startsWith('https://')) {
    fail(
      `refusing insecure download origin for ${url}: only https:// is allowed ` +
        `(set ZERO_ALLOW_INSECURE_DOWNLOAD=1 to override for local testing).`,
    );
  }
  let response;
  try {
    response = await fetch(url, { redirect: 'follow' });
  } catch (error) {
    fail(`download failed for ${url}: ${error.message}`);
  }
  // Catch an https->http downgrade across redirects: the verified bytes must
  // still have arrived over TLS for the same-origin checksum to mean anything.
  if (!ALLOW_INSECURE && response.url && !response.url.startsWith('https://')) {
    fail(`refusing insecure redirect to ${response.url}`);
  }
  if (!response.ok) {
    fail(
      `download failed (HTTP ${response.status}) for ${url} (release tag ${tag}). ` +
        `If no release exists yet, or the tag and package.json version disagree, ` +
        `build from source: https://github.com/${REPO} (go run ./cmd/zero).`,
    );
  }
  const declared = Number(response.headers.get('content-length') || 0);
  if (declared && declared > MAX_DOWNLOAD_BYTES) {
    fail(`refusing oversized download (${declared} bytes) from ${url}`);
  }
  const buffer = Buffer.from(await response.arrayBuffer());
  if (buffer.length > MAX_DOWNLOAD_BYTES) {
    fail(`download from ${url} exceeded ${MAX_DOWNLOAD_BYTES} bytes`);
  }
  return buffer;
}

// Parse a sha256sum-style file. Anchored per line, and when the line carries a
// filename field it must equal the requested asset — mirrors the Go
// ParseSHA256Checksum so a checksum file cannot misattribute a digest.
function parseSha256(text, wantName) {
  for (const rawLine of text.split(/\r?\n/)) {
    const line = rawLine.trim();
    if (!line) continue;
    const match = line.match(/^([a-fA-F0-9]{64})(?:\s+\*?(.+))?$/);
    if (!match) continue;
    const [, hex, name] = match;
    if (!name || name.trim() === wantName) {
      return hex.toLowerCase();
    }
  }
  fail(`could not find a SHA-256 digest for ${wantName} in ${wantName}.sha256`);
}

function findByBasename(dir, name) {
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const full = join(dir, entry.name);
    if (entry.isDirectory()) {
      const nested = findByBasename(full, name);
      if (nested) return nested;
    } else if (entry.name === name) {
      return full;
    }
  }
  return null;
}

// extract runs from `workDir` and passes RELATIVE names so no Windows drive
// letter (e.g. C:\) appears in tar's archive argument — GNU tar otherwise reads
// `C:foo` as a remote `host:path` and fails. archiveName and destName are both
// relative to workDir.
function extract(workDir, archiveName, destName) {
  const opts = { stdio: 'inherit', cwd: workDir };
  if (ext === 'zip') {
    // Windows 10+ ships bsdtar as tar.exe, which extracts zips; fall back to
    // PowerShell Expand-Archive if tar is unavailable or cannot read the zip.
    const viaTar = spawnSync('tar', ['-xf', archiveName, '-C', destName], opts);
    if (viaTar.status === 0) return;
    const absArchive = join(workDir, archiveName);
    const absDest = join(workDir, destName);
    const viaPwsh = spawnSync(
      'powershell',
      [
        '-NoProfile',
        '-NonInteractive',
        '-Command',
        `Expand-Archive -LiteralPath '${absArchive.replace(/'/g, "''")}' ` +
          `-DestinationPath '${absDest.replace(/'/g, "''")}' -Force`,
      ],
      { stdio: 'inherit' },
    );
    if (viaPwsh.status !== 0) fail(`failed to extract ${assetName}`);
    return;
  }
  const viaTar = spawnSync('tar', ['-xzf', archiveName, '-C', destName], opts);
  if (viaTar.status !== 0) fail(`failed to extract ${assetName} (tar exited ${viaTar.status})`);
}

async function main() {
  const archiveBuffer = await download(assetUrl);
  const checksumText = (await download(checksumUrl)).toString('utf8');
  const expected = parseSha256(checksumText, assetName);
  const actual = createHash('sha256').update(archiveBuffer).digest('hex');
  if (actual !== expected) {
    fail(`checksum mismatch for ${assetName}: expected ${expected}, got ${actual}`);
  }

  const tempDir = mkdtempSync(join(tmpdir(), 'zero-install-'));
  try {
    const archivePath = join(tempDir, assetName);
    writeFileSync(archivePath, archiveBuffer);
    const extractDir = join(tempDir, 'extracted');
    mkdirSync(extractDir);
    extract(tempDir, assetName, 'extracted');

    // Copy only known basenames into the package root. We never honor
    // archive-relative paths, so a crafted entry cannot escape the package.
    const primarySource = findByBasename(extractDir, binaryName);
    if (!primarySource) fail(`archive ${assetName} did not contain ${binaryName}`);
    copyFileSync(primarySource, installedBinary);
    if (platform !== 'windows') chmodSync(installedBinary, 0o755);

    for (const name of optionalBinaries) {
      const source = findByBasename(extractDir, name);
      if (!source) {
        console.error(
          `[zero] note: optional helper ${name} was not in ${assetName}; ` +
            `the sandbox may run with reduced isolation.`,
        );
        continue;
      }
      const dest = join(packageRoot, name);
      copyFileSync(source, dest);
      if (platform !== 'windows') chmodSync(dest, 0o755);
    }

    writeFileSync(markerPath, VERSION + '\n');
    const sizeKb = Math.round(statSync(installedBinary).size / 1024);
    console.error(`[zero] installed ${binaryName} ${VERSION} (${platform}-${arch}, ${sizeKb} KB).`);
  } finally {
    rmSync(tempDir, { recursive: true, force: true });
  }
}

main().catch((error) => fail(error.message));
