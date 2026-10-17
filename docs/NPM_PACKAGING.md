# npm packaging

How `@twigpine/zero` is put together on npm, why it is shaped this way, and the
rules the release pipeline must follow. Read this before touching
`package.json`, `bin/zero.js`, `scripts/postinstall.mjs`,
`scripts/npm/build-platform-packages.mjs`, or the npm-publish steps of
`release-artifacts.yml`.

## Goals

A `npm install -g @twigpine/zero` must be **silent and self-contained**:

- No `EBADENGINE` warnings — ours or from any package in the dependency tree.
- No install scripts anywhere in the tree, so npm's `allow-scripts` gating,
  Bun's blocked-by-default lifecycle scripts, and pnpm's strict mode all
  install zero without prompts, trust ceremonies, or broken binaries.
- No network fetches outside the npm registry at install time. GitHub being
  down or rate-limited must not break `npm install`.
- Browser control (`agent-browser`) and terminal control (`tuistory`) work out
  of the box.

## Architecture

The model is the one used by Codex, esbuild, and Biome: a tiny wrapper package
plus per-platform payloads carrying the native binaries.

```
@twigpine/zero                    <- wrapper: bin/zero.js + optionalDependencies
├─ @twigpine/zero-darwin-arm64 -> npm:@twigpine/zero@{version}-darwin-arm64
├─ @twigpine/zero-darwin-x64   -> npm:@twigpine/zero@{version}-darwin-x64
├─ @twigpine/zero-linux-arm64  -> npm:@twigpine/zero@{version}-linux-arm64
├─ @twigpine/zero-linux-x64    -> npm:@twigpine/zero@{version}-linux-x64
└─ @twigpine/zero-win32-x64    -> npm:@twigpine/zero@{version}-win32-x64
```

- The platform "packages" are **versions of the same `@twigpine/zero` package**,
  published at suffixed versions (`0.4.0-linux-x64`) and referenced through
  `npm:` aliases in `optionalDependencies`. One package name means one npm
  trusted-publisher configuration — no new publish credentials per platform.
  The alias suffixes use Node's `process.platform`/`process.arch` names so
  `bin/zero.js` can derive its platform package directly.
- Each platform version sets `os` and `cpu`, so npm installs exactly one of
  them and skips the rest.
- A platform payload is assembled from the platform's release archive by
  `scripts/npm/build-platform-packages.mjs` and contains the `zero` binary,
  the platform's sandbox helpers, and the vendored `helpers/` tree (see
  below). It has no `bin`, no scripts, and no dependencies — it is inert on
  its own; the wrapper execs the binary out of it.
- The published wrapper has **no scripts and no dependencies** — the
  assembly script strips both from the repo `package.json` and injects the
  exact-version `optionalDependencies`. (The repo `package.json` keeps
  `agent-browser`/`tuistory` in `dependencies` purely as the version pins for
  the vendored helpers tree; they are never installed by consumers.)
- There is **no windows-arm64 build** (matches the release matrix); Windows on
  ARM runs the x64 build under emulation via the first-run fallback.

### Binary resolution in `bin/zero.js`

1. Resolve `@twigpine/zero-<platform>-<arch>` and exec the `zero` binary from
   it. This wins over any previously downloaded copy — the platform version is
   pinned to the wrapper release.
2. Fall back to a binary previously downloaded next to the wrapper.
3. If neither exists (`--omit=optional`, package managers that skip optional
   dependencies), run the **fallback downloader**: `scripts/postinstall.mjs`,
   the exact logic that used to run as a postinstall hook (HTTPS-only, SHA-256
   verified against the release's own checksum file, no zip-slip), invoked by
   the wrapper itself. Failures are deliberately not cached — the fetch
   retries on every run until a binary is in place, so a transient network
   error self-heals. Platforms with no release asset (anything outside the
   matrix above, including windows-arm64) skip the attempt entirely rather
   than probing the network each run.
4. If the download is impossible too, print build-from-source guidance.

There is deliberately **no `scripts.postinstall`** in any published
`package.json`. The downloader exists only as a first-run fallback.

### Vendored helpers (`helpers/`)

`agent-browser` (Apache-2.0, vercel-labs/agent-browser) and `tuistory` are
**vendored binaries/packages inside the platform payload**, not npm
dependencies of the wrapper:

- As a dependency, agent-browser's `engines: { node: ">=24", pnpm: ">=11" }`
  and postinstall script produce `EBADENGINE` and `allow-scripts` warnings for
  every installer. As a vendored tree it produces none, because npm never
  resolves it.
- `zero-release package` already stages the helpers tree into every release
  archive (`stageLocalControlHelpers` runs `npm ci` from the repo's
  `package.json` pins + lockfile on the native builder). The assembly script
  reuses that staged tree, so npm installs and `install.sh` installs get
  identical helpers.
- The Go binary resolves helpers from `<binary dir>/helpers/node_modules/.bin`
  on its own (`internal/localcontrol/browser.go`, `adjacentHelper`) — no
  wrapper involvement, no configuration.
- **Symlink materialization:** `npm pack` silently drops symlinks, and npm's
  own `.bin` shims are symlinks on POSIX. The assembly script rewrites every
  `.bin` symlink into a relocatable `#!/bin/sh` exec shim and dereferences any
  other symlink, so the vendored tree survives publishing. (Verified
  empirically; nested `node_modules` under `helpers/` IS packed — only the
  package-root `node_modules` is always ignored.)
- **Binary pruning:** agent-browser ships one ~11 MB native binary per
  platform (7 total). The assembly script keeps only the payload's own
  platform binary (plus the musl variant on linux, which its launcher detects
  at runtime) — about 65 MB saved per platform package.

### One package name

The npm name lives in `package.json` and nothing else repeats it:
`scripts/npm/build-platform-packages.mjs` names the wrapper, the platform
versions, the alias names, and the alias targets from it, and `bin/zero.js`
reads its own `package.json` to derive the alias it resolves. The Go side
needs the name as a constant (`npmPackageName` in
`internal/update/installmethod.go`), and a test holds it equal to
`package.json`.

## Publishing rules (release pipeline)

These are the invariants `release-artifacts.yml` must hold. Breaking the first
one is user-visible immediately. `scripts/npm/publish-packages.sh` implements
them, and `internal/npmwrapper/publish_test.go` runs it against a fake
registry.

1. **Platform versions must never become `latest`.** `0.4.0-linux-x64` is a
   semver *prerelease* of `0.4.0`; publishing it without an explicit
   non-`latest` dist-tag would clobber `latest` and users would install a
   platform payload as the CLI. The workflow publishes platform versions with
   `--tag platform` and asserts `latest` survived before the wrapper publish,
   then asserts `latest` equals the wrapper version after it. Both lookups
   tolerate a failed `npm view` (registry lag, or no `latest` tag yet on a
   bootstrap publish) — a missing value must not abort the job between the
   platform and wrapper publishes, which would strand a half-published
   release.
2. **Platform versions are on the registry before the wrapper.** The
   wrapper's `optionalDependencies` pin exact suffixed versions; publishing
   the wrapper first would create a window where installs resolve aliases
   that 404. Ordering the uploads is not enough: npm applies a publish
   asynchronously, and in 0.10.0 a platform version appeared ten minutes
   after its upload returned, after the wrapper. The publish script waits
   until npm shows every platform version before it publishes the wrapper,
   and waits the same way for `latest` afterwards (up to 30 minutes in all).
3. **Exact-version pinning.** The wrapper at `X.Y.Z` references platform
   versions `X.Y.Z-<platform>-<arch>` exactly — never ranges — so a wrapper
   and its binaries can never skew. The publish script refuses to start when
   the wrapper's aliases do not match the platform payloads beside it.
4. All publishes go through the existing npm OIDC trusted-publishing flow and
   the `npm-publish` environment gate; no tokens. Because the platform
   payloads are versions of the same package, they reuse the single
   trusted-publisher configuration. npm checks provenance against
   `package.json` `repository`, so it must name the repository the workflow
   runs in.
5. **Re-runs are safe.** A version already on the registry is skipped rather
   than republished, so re-running a job that failed between the platform and
   wrapper publishes finishes the release. Only npm's E404 counts as "not
   published"; a lookup that fails any other way is retried and then stops the
   job before anything is published.

`zero update` keeps working unchanged: it detects an npm install by finding a
`package.json` named `@twigpine/zero` next to the running binary — true inside
a platform payload too — and updates via `npm install -g @twigpine/zero@latest`.

## The move from `@gitlawb/zero`

Zero was published as `@gitlawb/zero` up to 0.9.0 and as `@twigpine/zero`
from 0.10.0. There is no in-place migration: both packages own the `zero`
bin, so npm refuses to install one over the other (`EEXIST`), and forcing it
lets a later uninstall of the old package delete the shared bin. The user
has to uninstall the old package and install the new one, in that order.

Deprecating the old package alone would not have reached existing users.
0.9.0's `zero upgrade` runs `npm install -g @gitlawb/zero@latest`; with
nothing newer under that name, npm reinstalls nothing, prints no deprecation
warning (it only warns when it installs something), and Zero reports the
update as done. So 0.10.0 was published once more under the old name, as a
bridge:

- The release job assembled the same verified artifacts a second time with
  `build-platform-packages.mjs --package-name @gitlawb/zero` and published
  them with the same script, after the `@twigpine/zero` publish. The steps
  that did this ran once and have been removed from `release-artifacts.yml`.
- The bridge build knows its name from the `package.json` beside it. The Go
  binary reports it as `InstallMethodNpmLegacy`: `zero upgrade` refuses
  with the switch steps instead of reinstalling it, and `zero update --check`
  adds them. `bin/zero.js` prints the same steps after `zero` exits, on a
  terminal only. This handling stays in the code, since only a build
  installed under the old name ever triggers it.
- Updating from 0.9.0 installs that bridge build, so npm also shows the
  deprecation message.

`@gitlawb/zero` is deprecated on every version and no longer has a trusted
publisher, so this release workflow cannot publish it again; another version
would take a maintainer publishing by hand. Its versions stay on the
registry: existing installs resolve their platform payload through them, so
they must never be unpublished.

## Runbook: bumping the vendored helpers

The vendored helper versions are pinned by the repo `package.json`
`dependencies` + `package-lock.json`; the release build vendors exactly what
the lockfile resolves. No `^range` surprises ship: a release carries what its
lockfile pinned at build time.

- Routine bump: `npm install agent-browser@<version>` (or `tuistory@…`) in the
  repo to update pin + lockfile, verify the browser/terminal tools end-to-end,
  ship with the next release.
- Security response: if a vendored helper publishes a security fix, the bump
  is release-worthy on its own — cut a patch release of zero carrying only the
  pin change.
- Attribution: agent-browser is Apache-2.0; its LICENSE ships inside the
  vendored package directory (`helpers/node_modules/agent-browser/`), which
  satisfies redistribution attribution.

## History / rationale

Until v0.3.x the npm package was a wrapper with a `postinstall` downloader and
`agent-browser`/`tuistory` as regular dependencies. That produced three
warnings on every install (`EBADENGINE` from agent-browser's `node >=24`
engines pin, and `allow-scripts` warnings for both postinstall scripts), broke
silently under Bun's default script blocking, and coupled `npm install` to
GitHub Releases availability. The platform-package model removes the warnings
structurally — there is nothing left in the tree for a package manager to warn
about — rather than asking users to approve or suppress them.
