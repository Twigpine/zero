#!/usr/bin/env node
// build-platform-packages.mjs: assemble the npm publish payloads for a release
// from the per-platform archives built by `zero-release package`.
//
// Layout written under --out-dir (default dist/npm), where <name> is the
// package.json name, or --package-name when given:
//   wrapper/                        <name>@X.Y.Z — bin/zero.js, the
//                                   first-run fallback downloader, and a
//                                   package.json with NO scripts and NO
//                                   dependencies, only optionalDependencies
//                                   aliasing the platform versions below.
//   platforms/zero-<platform>-<arch>/
//                                   <name>@X.Y.Z-<platform>-<arch> —
//                                   the zero binary, sandbox helpers, and the
//                                   vendored local-control helpers/ tree,
//                                   copied straight out of the release archive.
//
// The platform "packages" are versions of the SAME package at suffixed
// (semver-prerelease) versions, referenced from the wrapper through npm:
// aliases. One package name keeps a single npm trusted-publisher config.
// See docs/NPM_PACKAGING.md for the publishing rules this feeds.
//
// Archives are verified against their .sha256 files before extraction. All
// five platforms are required unless --only limits the set (testing only —
// CI must always assemble the full matrix, and the wrapper's
// optionalDependencies always reference the full matrix regardless).
//
// --package-name assembles the same artifacts under another name. It was
// added for the one release (0.10.0) also published under the pre-rename name
// @gitlawb/zero (see docs/NPM_PACKAGING.md); everything in the payloads that
// names the package follows it, so the binary and wrapper know which name they
// were installed as.
//
// Usage:
//   node scripts/npm/build-platform-packages.mjs \
//     --artifacts-dir dist/artifacts [--out-dir dist/npm] [--only linux-x64,...] \
//     [--package-name @scope/name]

import {
  chmodSync,
  cpSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readdirSync,
  readFileSync,
  realpathSync,
  rmSync,
  statSync,
  writeFileSync,
} from 'node:fs';
import { createHash } from 'node:crypto';
import { tmpdir } from 'node:os';
import { basename, dirname, join, relative, sep } from 'node:path';
import { fileURLToPath } from 'node:url';
import { spawnSync } from 'node:child_process';

const repoRoot = dirname(dirname(dirname(fileURLToPath(import.meta.url))));

// Node platform/arch names (they name the alias suffixes so bin/zero.js can
// derive its platform package from process.platform/process.arch) mapped to
// the release asset naming from internal/release/release.go. windows-arm64 is
// deliberately absent — the release matrix does not build it.
const MATRIX = [
  { platform: 'darwin', arch: 'arm64', release: 'macos-arm64', ext: 'tar.gz' },
  { platform: 'darwin', arch: 'x64', release: 'macos-x64', ext: 'tar.gz' },
  { platform: 'linux', arch: 'arm64', release: 'linux-arm64', ext: 'tar.gz' },
  { platform: 'linux', arch: 'x64', release: 'linux-x64', ext: 'tar.gz' },
  { platform: 'win32', arch: 'x64', release: 'windows-x64', ext: 'zip' },
];

// Files staged into release archives for the shell installers / humans that
// must NOT ship in a platform npm package (the wrapper package owns them).
const PLATFORM_PACKAGE_EXCLUDES = ['package.json', 'README.md', 'VERSION', 'bin'];

function fail(message) {
  console.error(`[build-platform-packages] ${message}`);
  process.exit(1);
}

function parseArgs(argv) {
  const args = { artifactsDir: '', outDir: join(repoRoot, 'dist', 'npm'), only: null, packageName: '' };
  for (let i = 0; i < argv.length; i += 1) {
    const arg = argv[i];
    const next = () => {
      i += 1;
      if (i >= argv.length) fail(`missing value for ${arg}`);
      return argv[i];
    };
    switch (arg) {
      case '--artifacts-dir':
        args.artifactsDir = next();
        break;
      case '--out-dir':
        args.outDir = next();
        break;
      case '--only':
        args.only = new Set(next().split(',').map((entry) => entry.trim()).filter(Boolean));
        break;
      case '--package-name':
        args.packageName = next();
        // A scoped or unscoped npm name; anything else would also become a
        // path segment in the alias npm installs.
        if (!/^(@[a-z0-9][a-z0-9._~-]*\/)?[a-z0-9][a-z0-9._~-]*$/.test(args.packageName)) {
          fail(`--package-name ${JSON.stringify(args.packageName)} is not an npm package name`);
        }
        break;
      default:
        fail(`unknown argument ${arg}`);
    }
  }
  if (!args.artifactsDir) fail('--artifacts-dir is required');
  return args;
}

function readWrapperPackage() {
  const pkg = JSON.parse(readFileSync(join(repoRoot, 'package.json'), 'utf8'));
  if (!pkg.name) fail('package.json name is empty');
  if (!pkg.version) fail('package.json version is empty');
  return pkg;
}

function platformVersion(version, entry) {
  return `${version}-${entry.platform}-${entry.arch}`;
}

// bin/zero.js derives the same alias from its own package name at runtime, so
// the wrapper name, the alias names, and the alias targets must all come from
// one value.
function aliasName(packageName, entry) {
  return `${packageName}-${entry.platform}-${entry.arch}`;
}

function optionalDependencies(packageName, version) {
  const aliases = {};
  for (const entry of MATRIX) {
    aliases[aliasName(packageName, entry)] = `npm:${packageName}@${platformVersion(version, entry)}`;
  }
  return aliases;
}

// Same anchored sha256sum parsing as scripts/postinstall.mjs: when a line
// carries a filename it must match, so a checksum file cannot misattribute a
// digest.
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
  fail(`could not find a SHA-256 digest for ${wantName} in its .sha256 file`);
}

function verifyChecksum(archivePath, assetName) {
  const checksumPath = `${archivePath}.sha256`;
  if (!existsSync(checksumPath)) fail(`missing checksum file ${checksumPath}`);
  const expected = parseSha256(readFileSync(checksumPath, 'utf8'), assetName);
  const actual = createHash('sha256').update(readFileSync(archivePath)).digest('hex');
  if (actual !== expected) {
    fail(`checksum mismatch for ${assetName}: expected ${expected}, got ${actual}`);
  }
}

function extractArchive(archivePath, ext, destDir) {
  mkdirSync(destDir, { recursive: true });
  const commands =
    ext === 'zip'
      ? [
          ['unzip', ['-q', archivePath, '-d', destDir]],
          // bsdtar (macOS / Windows tar.exe) extracts zips; GNU tar does not,
          // hence unzip first.
          ['tar', ['-xf', archivePath, '-C', destDir]],
        ]
      : [['tar', ['-xzf', archivePath, '-C', destDir]]];
  let lastError = '';
  for (const [command, commandArgs] of commands) {
    const result = spawnSync(command, commandArgs, { stdio: ['ignore', 'ignore', 'pipe'] });
    if (result.status === 0) return;
    lastError = result.error ? result.error.message : (result.stderr || '').toString().trim();
  }
  fail(`failed to extract ${archivePath}: ${lastError}`);
}

// zero-release archives carry their payload at the archive root; also accept
// a single zero-vX.Y.Z-<platform>-<arch>/ prefix directory (the shape
// install.sh tolerates) so fixtures and future layout changes both work.
function extractedPackageDir(extractDir, packageName, binaryName) {
  if (existsSync(join(extractDir, binaryName))) return extractDir;
  const exact = join(extractDir, packageName);
  if (existsSync(join(exact, binaryName))) return exact;
  const entries = readdirSync(extractDir, { withFileTypes: true }).filter((entry) => entry.isDirectory());
  if (entries.length === 1 && existsSync(join(extractDir, entries[0].name, binaryName))) {
    return join(extractDir, entries[0].name);
  }
  fail(`archive ${packageName} did not contain the ${binaryName} binary at a recognizable root`);
}

// npm pack silently drops symlinks from the tarball, and the staged helpers
// tree relies on them: npm's own .bin shims are symlinks on POSIX. Replace
// every symlink with something pack-safe — .bin links become relocatable
// `sh` exec shims, anything else becomes a dereferenced copy — so the
// vendored helpers survive publishing intact.
function materializeSymlinks(dir) {
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const full = join(dir, entry.name);
    if (entry.isSymbolicLink()) {
      let target;
      try {
        target = realpathSync(full);
      } catch {
        fail(`broken symlink in staged helpers tree: ${full}`);
      }
      const targetInfo = statSync(target);
      rmSync(full);
      if (basename(dir) === '.bin' && !targetInfo.isDirectory()) {
        // relative() must compare like with like: target came out of
        // realpathSync, so resolve dir the same way (macOS tmp dirs reach the
        // same tree via /var and /private/var, which would otherwise produce a
        // ../..-through-the-root monstrosity).
        const relTarget = relative(realpathSync(dir), target).split(sep).join('/');
        writeFileSync(
          full,
          '#!/bin/sh\n' +
            'here="$(cd "$(dirname "$0")" && pwd)"\n' +
            `exec "$here/${relTarget}" "$@"\n`,
        );
        chmodSync(full, 0o755);
        // Direct exec relies on the target's own shebang and executable bit;
        // npm pack preserves modes, so set it here.
        chmodSync(target, 0o755);
      } else {
        cpSync(target, full, { recursive: true });
      }
    } else if (entry.isDirectory()) {
      materializeSymlinks(full);
    }
  }
}

// agent-browser's npm package ships one native binary per supported platform
// (~11 MB each); its bin/agent-browser.js launcher picks the right one from
// process.platform/process.arch at runtime. A platform payload only ever runs
// on its own platform, so keep just the matching binary (plus the musl
// variant on linux — the launcher detects musl at runtime) and drop the rest.
function pruneAgentBrowserBinaries(packageDir, entry) {
  const binDir = join(packageDir, 'helpers', 'node_modules', 'agent-browser', 'bin');
  if (!existsSync(binDir)) return;
  const keep = new Set(
    entry.platform === 'linux'
      ? [`linux-${entry.arch}`, `linux-musl-${entry.arch}`]
      : [`${entry.platform}-${entry.arch}`],
  );
  for (const name of readdirSync(binDir)) {
    const match = name.match(/^agent-browser-(.+?)(\.exe)?$/);
    if (!match || match[1] === '') continue;
    if (!keep.has(match[1])) rmSync(join(binDir, name));
  }
  const kept = readdirSync(binDir).filter((name) => /^agent-browser-.+/.test(name));
  if (kept.length === 0) {
    fail(`pruning agent-browser binaries for ${entry.platform}-${entry.arch} left none behind`);
  }
}

// Mirrors verifyStagedLocalControlHelpers in internal/release/release.go: the
// payload must carry runnable shims for both vendored helpers, or browser /
// terminal control silently degrades to a PATH lookup for every npm user.
function verifyHelperShims(packageDir, entry, assetName) {
  const binDir = join(packageDir, 'helpers', 'node_modules', '.bin');
  for (const helper of ['agent-browser', 'tuistory']) {
    const shimNames =
      entry.platform === 'win32' ? [`${helper}.cmd`, `${helper}.exe`, helper] : [helper];
    if (!shimNames.some((name) => existsSync(join(binDir, name)))) {
      fail(`${assetName} helpers tree is missing an executable ${helper} shim`);
    }
  }
}

function buildPlatformPackage(entry, packageName, version, artifactsDir, outDir) {
  const assetName = `zero-v${version}-${entry.release}.${entry.ext}`;
  const archivePath = join(artifactsDir, assetName);
  if (!existsSync(archivePath)) fail(`missing release archive ${archivePath}`);
  verifyChecksum(archivePath, assetName);

  const binaryName = entry.platform === 'win32' ? 'zero.exe' : 'zero';
  const tempDir = mkdtempSync(join(tmpdir(), 'zero-npm-'));
  try {
    extractArchive(archivePath, entry.ext, tempDir);
    const sourceDir = extractedPackageDir(tempDir, `zero-v${version}-${entry.release}`, binaryName);
    const packageDir = join(outDir, 'platforms', `zero-${entry.platform}-${entry.arch}`);
    rmSync(packageDir, { recursive: true, force: true });
    mkdirSync(packageDir, { recursive: true });

    for (const item of readdirSync(sourceDir)) {
      if (PLATFORM_PACKAGE_EXCLUDES.includes(item)) continue;
      // verbatimSymlinks keeps relative link targets relative; the default
      // rewrites them to absolute paths into this soon-to-be-deleted temp dir.
      cpSync(join(sourceDir, item), join(packageDir, item), { recursive: true, verbatimSymlinks: true });
    }

    const binaryPath = join(packageDir, binaryName);
    if (!existsSync(binaryPath)) fail(`${assetName} did not contain ${binaryName}`);
    if (entry.platform !== 'win32') chmodSync(binaryPath, 0o755);
    materializeSymlinks(packageDir);
    pruneAgentBrowserBinaries(packageDir, entry);
    verifyHelperShims(packageDir, entry, assetName);

    // npm only ships a license file it finds in the packed directory;
    // package.json's license field alone does not include the text.
    cpSync(join(repoRoot, 'LICENSE'), join(packageDir, 'LICENSE'));

    const wrapperPkg = readWrapperPackage();
    const manifest = {
      name: packageName,
      version: platformVersion(version, entry),
      description: `zero native binary for ${entry.platform}-${entry.arch} (installed via ${packageName}@${version})`,
      os: [entry.platform],
      cpu: [entry.arch],
      license: wrapperPkg.license,
      repository: wrapperPkg.repository,
      // No bin, no scripts, no dependencies: this payload is inert on its own;
      // the wrapper package execs the binary out of it.
    };
    writeFileSync(join(packageDir, 'package.json'), JSON.stringify(manifest, null, 2) + '\n');
    return packageDir;
  } finally {
    rmSync(tempDir, { recursive: true, force: true });
  }
}

function buildWrapperPackage(packageName, version, outDir) {
  const wrapperDir = join(outDir, 'wrapper');
  rmSync(wrapperDir, { recursive: true, force: true });
  mkdirSync(join(wrapperDir, 'bin'), { recursive: true });
  mkdirSync(join(wrapperDir, 'scripts'), { recursive: true });

  cpSync(join(repoRoot, 'bin', 'zero.js'), join(wrapperDir, 'bin', 'zero.js'));
  cpSync(join(repoRoot, 'scripts', 'postinstall.mjs'), join(wrapperDir, 'scripts', 'postinstall.mjs'));
  cpSync(join(repoRoot, 'README.md'), join(wrapperDir, 'README.md'));
  // npm auto-includes LICENSE in the tarball only when the file is present in
  // the directory being packed (the files whitelist cannot exclude it).
  cpSync(join(repoRoot, 'LICENSE'), join(wrapperDir, 'LICENSE'));
  chmodSync(join(wrapperDir, 'bin', 'zero.js'), 0o755);

  const pkg = readWrapperPackage();
  // The published wrapper must stay warning-free on every package manager:
  // no lifecycle scripts and no regular dependencies (the repo's dependencies
  // entries exist only to pin the vendored helpers/ tree that zero-release
  // stages into the platform payloads — see internal/release/release.go).
  delete pkg.scripts;
  delete pkg.dependencies;
  pkg.name = packageName;
  pkg.optionalDependencies = optionalDependencies(packageName, version);
  writeFileSync(join(wrapperDir, 'package.json'), JSON.stringify(pkg, null, 2) + '\n');
  return wrapperDir;
}

function main() {
  const args = parseArgs(process.argv.slice(2));
  const wrapperPkg = readWrapperPackage();
  const packageName = args.packageName || wrapperPkg.name;
  const { version } = wrapperPkg;

  const selected = MATRIX.filter(
    (entry) => !args.only || args.only.has(`${entry.platform}-${entry.arch}`),
  );
  if (selected.length === 0) fail('--only matched no platforms');
  if (args.only) {
    for (const key of args.only) {
      if (!MATRIX.some((entry) => `${entry.platform}-${entry.arch}` === key)) {
        fail(`--only lists unknown platform ${key}`);
      }
    }
  }

  const built = [];
  for (const entry of selected) {
    built.push(buildPlatformPackage(entry, packageName, version, args.artifactsDir, args.outDir));
  }
  const wrapperDir = buildWrapperPackage(packageName, version, args.outDir);

  process.stdout.write(
    JSON.stringify(
      {
        name: packageName,
        version,
        wrapperDir,
        platformDirs: built,
        platformVersions: selected.map((entry) => platformVersion(version, entry)),
      },
      null,
      2,
    ) + '\n',
  );
}

main();
