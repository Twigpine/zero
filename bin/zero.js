#!/usr/bin/env node

import { spawnSync } from 'node:child_process';
import { existsSync, readFileSync, writeSync } from 'node:fs';
import { createRequire } from 'node:module';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const packageRoot = dirname(dirname(fileURLToPath(import.meta.url)));

const CURRENT_PACKAGE_NAME = '@twigpine/zero';
// The npm package moved from @gitlawb/zero to @twigpine/zero in 0.10.0. That
// release was also published once under the old name, so 0.9.0's `zero
// upgrade` and anyone still installing the old name land on a working build
// that says where the package went (docs/NPM_PACKAGING.md). Nothing is
// published under the old name after that.
const LEGACY_PACKAGE_NAME = '@gitlawb/zero';

// The npm name this wrapper was published under, read from its own
// package.json so the name lives in one place. The platform payload alias and
// every reinstall hint below derive from it.
function readPackageName(root) {
  try {
    const { name } = JSON.parse(readFileSync(join(root, 'package.json'), 'utf8'));
    if (typeof name === 'string' && name !== '') return name;
  } catch {
    // An unreadable manifest must not stop the CLI from launching.
  }
  return CURRENT_PACKAGE_NAME;
}

const packageName = readPackageName(packageRoot);

// Same note as legacyPackageSwitchMessage in internal/update/installmethod.go.
// Uninstalling first is the only safe order: both packages own the `zero` bin,
// so npm refuses to install the new one over the old (EEXIST), and forcing it
// lets a later uninstall of the old package delete the shared bin.
function legacyPackageNotice() {
  return (
    `[zero] Zero has moved to ${CURRENT_PACKAGE_NAME} on npm, and ${LEGACY_PACKAGE_NAME} gets no further releases.\n` +
    'To keep getting updates, quit zero and run:\n' +
    `  npm uninstall -g ${LEGACY_PACKAGE_NAME}\n` +
    `  npm install -g ${CURRENT_PACKAGE_NAME}\n` +
    'Uninstall first: both packages provide the `zero` command. With bun, pnpm or yarn, use its global remove and add commands instead.\n'
  );
}

// Printed when the wrapper exits rather than before zero starts, because the
// TUI takes over the screen and would hide anything printed first. Only on a
// terminal, so scripts that capture stderr see nothing new. `zero update` and
// `zero upgrade` print the same note themselves.
if (packageName === LEGACY_PACKAGE_NAME && !['update', 'upgrade'].includes(process.argv[2])) {
  process.on('exit', () => {
    if (process.stderr.isTTY || process.env.ZERO_WRAPPER_SIMULATE_TTY === '1') {
      // Synchronous: an exit handler cannot wait for an asynchronous write.
      writeSync(2, legacyPackageNotice());
    }
  });
}

function zeroBinaryName(platform = process.platform) {
  return platform === 'win32' ? 'zero.exe' : 'zero';
}

function helperShimNames(name, platform = process.platform) {
  if (platform === 'win32') {
    return [`${name}.cmd`, `${name}.exe`, name];
  }
  return [name];
}

function commandForShim(path, platform = process.platform) {
  if (platform === 'win32' && path.toLowerCase().endsWith('.cmd')) {
    return {
      command: process.env.ComSpec || 'cmd.exe',
      prefixArgs: ['/d', '/s', '/c', `"${path.replace(/"/g, '""')}"`],
    };
  }
  return { command: path, prefixArgs: [] };
}

function resolveHelper(packageRoot, name) {
  const binDir = join(packageRoot, 'node_modules', '.bin');
  for (const shimName of helperShimNames(name)) {
    const candidate = join(binDir, shimName);
    if (!existsSync(candidate)) continue;
    return {
      ...commandForShim(candidate),
      pathPrepend: [binDir],
    };
  }
  return null;
}

// Legacy path: helpers installed as npm dependencies of the wrapper. Current
// installs vendor the helpers inside the platform package instead (a helpers/
// directory next to the binary, found by the Go side on its own), so this
// manifest is usually empty — it only matters for a downloaded-binary install
// whose wrapper still carries helper dependencies.
function localControlHelperManifest(packageRoot) {
  const helpers = {};
  for (const name of ['agent-browser', 'tuistory']) {
    const helper = resolveHelper(packageRoot, name);
    if (helper) helpers[name] = helper;
  }
  if (Object.keys(helpers).length === 0) return '';
  return JSON.stringify({ version: 1, helpers });
}

// Mirrors internal/cli/observability.go parseDoctorArgs + writeDoctorHelp so the
// wrapper's missing-binary doctor fallback matches the Go doctor CLI surface.
const DOCTOR_HELP = `Usage:
  zero doctor [flags]

Runs Go backend health checks for config and provider setup.

Flags:
      --json            Print JSON report
      --connectivity    Include provider endpoint connectivity probe when available
  -h, --help            Show this help
`;

const EXIT_USAGE = 2;

function installedByBun() {
  if (process.env.ZERO_WRAPPER_SIMULATE_BUN === '1') {
    return true;
  }
  return process.execPath.includes('bun') || !!process.versions?.bun;
}

function bunRecoveryParagraph() {
  return (
    'You installed with Bun, which does not run dependency lifecycle scripts\n' +
    'by default. Trust the package to run the blocked postinstall:\n' +
    `  bun pm trust ${packageName}       (project install)\n` +
    `  bun pm -g trust ${packageName}    (global install)\n` +
    'On Bun versions without `bun pm trust`, add\n' +
    `  "trustedDependencies": ["${packageName}"]\n` +
    'to your project package.json and reinstall.\n' +
    '\n'
  );
}

function buildFromSourceParagraph() {
  return (
    'If this platform has no prebuilt binary, build from source:\n' +
    'https://github.com/Twigpine/zero (go run ./cmd/zero, requires Go 1.26+).'
  );
}

function missingBinaryContextParagraph() {
  return (
    `Normally npm installs it as an optional dependency of ${packageName}\n` +
    `(${packageName}-${process.platform}-${process.arch}), and the wrapper can\n` +
    'also download it from the GitHub Release when it is missing.'
  );
}

function missingNativeRecoveryParagraphs(postinstallScript) {
  const ranByBun = installedByBun();
  return (
    missingBinaryContextParagraph() +
    '\n' +
    '\n' +
    'Things to try:\n' +
    '  - reinstall without omitting optional dependencies:\n' +
    `      npm install -g ${packageName}\n` +
    '  - run the downloader manually (needs write access to the package dir):\n' +
    `      node "${postinstallScript}"\n` +
    '\n' +
    (ranByBun ? bunRecoveryParagraph() : '') +
    buildFromSourceParagraph()
  );
}

function formatGenericMissingNativeBinaryMessage(postinstallScript) {
  return (
    '[zero] No native binary is available for this install.\n' +
    missingNativeRecoveryParagraphs(postinstallScript)
  );
}

function parseDoctorArgs(args) {
  let json = false;
  for (const arg of args) {
    switch (arg) {
      case '-h':
      case '--help':
      case 'help':
        return { kind: 'help' };
      case '--json':
        json = true;
        break;
      case '--connectivity':
        break;
      default:
        return { kind: 'error', message: `unknown doctor flag ${JSON.stringify(arg)}` };
    }
  }
  return { kind: 'run', json };
}

function missingNativeDoctorJSONReport(postinstallScript) {
  return {
    generatedAt: new Date().toISOString().replace(/\.\d{3}Z$/, 'Z'),
    ok: false,
    checks: [
      {
        id: 'runtime.go',
        label: 'Go runtime',
        status: 'fail',
        message: 'Native zero binary is missing next to the npm wrapper.',
        details: {
          remedy: `node "${postinstallScript}"`,
        },
      },
    ],
  };
}

function missingNativeDoctorTextReport(postinstallScript) {
  return (
    'Zero doctor report (' +
    new Date().toISOString() +
    ')\n' +
    'Overall: fail\n' +
    '[fail] runtime.go - Native zero binary is missing next to the npm wrapper.\n' +
    '  remedy: node "' +
    postinstallScript +
    '"\n' +
    '\n' +
    missingNativeRecoveryParagraphs(postinstallScript)
  );
}

// The platform payload is a version of this same package, installed under an
// npm: alias (see docs/NPM_PACKAGING.md). The alias name is derived from
// process.platform/process.arch, so an unsupported platform simply fails to
// resolve and we fall through to the downloader.
function platformPackageBinary() {
  const aliasedName = `${packageName}-${process.platform}-${process.arch}`;
  let manifestPath;
  try {
    manifestPath = createRequire(import.meta.url).resolve(`${aliasedName}/package.json`);
  } catch {
    return null;
  }
  const candidate = join(dirname(manifestPath), zeroBinaryName());
  return existsSync(candidate) ? candidate : null;
}

// Mirrors the release matrix (internal/release/release.go): the platforms a
// GitHub Release asset exists for. android maps to the linux asset; there is
// no windows-arm64 asset (Windows on ARM runs the x64 build under emulation).
function releaseAssetAvailable() {
  const platform = { linux: 'linux', android: 'linux', darwin: 'macos', win32: 'windows' }[
    process.platform
  ];
  const arch = process.arch === 'x64' || process.arch === 'arm64' ? process.arch : null;
  if (!platform || !arch) return false;
  return !(platform === 'windows' && arch === 'arm64');
}

// Fallback for installs without a platform package (--omit=optional, package
// managers that skip optionalDependencies): fetch the binary next to the
// wrapper with the same checksum-verified downloader that used to run as
// postinstall. Deliberately NOT cached on failure — a transient network error
// self-heals on the next run — so this retries on every invocation until a
// binary is in place. Platforms with no release asset skip the attempt.
function downloadMissingBinary(packageRoot) {
  const downloadScript = join(packageRoot, 'scripts', 'postinstall.mjs');
  if (!existsSync(downloadScript) || !releaseAssetAvailable()) return;
  console.error(
    '[zero] no platform package installed — fetching the native binary from the GitHub Release (retried on each run until it succeeds).',
  );
  spawnSync(process.execPath, [downloadScript], {
    stdio: ['ignore', 'inherit', 'inherit'],
  });
}

function resolveNativeBinary() {
  const fromPlatformPackage = platformPackageBinary();
  if (fromPlatformPackage) return fromPlatformPackage;
  const downloaded = join(packageRoot, zeroBinaryName());
  if (existsSync(downloaded)) return downloaded;
  downloadMissingBinary(packageRoot);
  return existsSync(downloaded) ? downloaded : null;
}

const nativePath = resolveNativeBinary();

if (!nativePath) {
  const postinstallScript = join(packageRoot, 'scripts', 'postinstall.mjs');

  // `zero doctor` is the diagnostic command: when the native binary is missing
  // it's the one invocation that must NOT bail with the generic wrapper error,
  // because that's exactly the blind alley issue #405 reports. Instead, surface
  // a doctor-shaped FAIL line for the runtime so the user's diagnostic finds the
  // real cause. We branch on a literal 'doctor' subcommand only (matching `zero
  // doctor` and `zero doctor --connectivity`), preserving the existing bail for
  // every other invocation (exec, providers list, TUI, --version, etc.).
  const argv = process.argv.slice(2);
  const isDoctor = argv.length > 0 && argv[0] === 'doctor';
  if (isDoctor) {
    const parsed = parseDoctorArgs(argv.slice(1));
    if (parsed.kind === 'help') {
      process.stdout.write(DOCTOR_HELP);
      process.exit(0);
    }
    if (parsed.kind === 'error') {
      process.stderr.write(`[zero] ${parsed.message}\n`);
      process.exit(EXIT_USAGE);
    }

    if (parsed.json) {
      process.stdout.write(JSON.stringify(missingNativeDoctorJSONReport(postinstallScript), null, 2) + '\n');
      process.exit(1);
    }

    process.stdout.write(missingNativeDoctorTextReport(postinstallScript) + '\n');
    process.exit(1);
  }

  console.error(formatGenericMissingNativeBinaryMessage(postinstallScript));
  process.exit(1);
}

const env = { ...process.env };
const localControlHelpers = localControlHelperManifest(packageRoot);
if (localControlHelpers) {
  env.ZERO_LOCAL_CONTROL_HELPERS = localControlHelpers;
} else {
  delete env.ZERO_LOCAL_CONTROL_HELPERS;
}

const child = spawnSync(nativePath, process.argv.slice(2), {
  stdio: 'inherit',
  env,
});

if (child.error) {
  console.error(`[zero] Failed to launch wrapper target: ${child.error.message}`);
  process.exit(1);
}

if (child.signal) {
  process.kill(process.pid, child.signal);
}

process.exit(child.status ?? 1);
