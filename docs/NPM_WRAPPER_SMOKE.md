# npm Wrapper Smoke Checklist

Run this checklist when a PR changes npm distribution files such as
`package.json`, `bin/zero.js`, `scripts/npm/build-platform-packages.mjs`,
`scripts/npm/publish-packages.sh`, Go build or release tooling, or the npm
`bin` wrapper.

## Required Checks

```bash
go test ./internal/npmwrapper ./internal/release
go run ./cmd/zero-release build
go run ./cmd/zero-release smoke
```

Also run the Go checks when the PR changes Go entrypoint, CLI, or release
artifact behavior:

```bash
go test ./...
go run ./cmd/zero-release build
go run ./cmd/zero-release smoke
```

## End-To-End Assembly Smoke

Build a real archive for the host platform and assemble the npm payloads from
it (this is exactly what the publish job does):

```bash
go run ./cmd/zero-release package
node scripts/npm/build-platform-packages.mjs \
  --artifacts-dir dist/release --out-dir dist/npm --only <platform>-<arch>
```

Then simulate an installed layout and run the wrapper through each resolution
path:

```bash
# platform-package path
mkdir -p /tmp/zero-sim/node_modules/@twigpine
cp -R dist/npm/wrapper /tmp/zero-sim/node_modules/@twigpine/zero
cp -R dist/npm/platforms/zero-<platform>-<arch> \
  "/tmp/zero-sim/node_modules/@twigpine/zero-<platform>-<arch>"
node /tmp/zero-sim/node_modules/@twigpine/zero/bin/zero.js --version

# first-run download fallback (delete the platform package first)
rm -rf "/tmp/zero-sim/node_modules/@twigpine/zero-<platform>-<arch>"
node /tmp/zero-sim/node_modules/@twigpine/zero/bin/zero.js --version
```

## Checklist

- `package.json` has the expected package name (`@twigpine/zero`), version,
  `bin.zero` entry, and NO `scripts` entries — the published package must be
  free of lifecycle scripts (see NPM_PACKAGING.md).
- `scripts/npm/build-platform-packages.mjs` emits platform payloads whose
  `package.json` is `@twigpine/zero@<version>-<platform>-<arch>` with matching
  `os`/`cpu`, contains the executable binary and the vendored `helpers/` tree
  with regular-file (non-symlink) `.bin` shims, and a wrapper payload with no
  scripts, no dependencies, and the full five-alias `optionalDependencies`
  matrix.
- `scripts/postinstall.mjs` (the first-run fallback) resolves the correct
  release asset name/URL per platform (`ZERO_INSTALL_DRY_RUN=1` prints the
  plan), verifies the downloaded archive's SHA-256, and extracts only the
  known binary basenames into place. `ZERO_SKIP_DOWNLOAD=1` opts out cleanly
  (exit 0) and an unsupported platform/arch is a non-fatal skip.
- The wrapper prefers the platform package binary over a previously downloaded
  one, and `agent-browser`/`tuistory` run from the platform package's
  `helpers/node_modules/.bin` shims.
- The built binary exits 0 for `zero --version` or `zero --help`.
- `zero --version` reports `zero <package.json version>`.
- Release packaging still emits the expected archive and checksum names when
  package release files change.
