# Installing Zero

Pick whichever fits how you already install things. None of these is the
blessed one.

| Method | Command | Self-update |
| --- | --- | --- |
| Install script (Linux, macOS) | `curl -fsSL https://raw.githubusercontent.com/Twigpine/zero/main/scripts/install.sh \| bash` | `zero upgrade` |
| Install script (Windows) | `irm https://raw.githubusercontent.com/Twigpine/zero/main/scripts/install.ps1 \| iex` | `zero upgrade` |
| npm | `npm install -g @twigpine/zero` | `zero upgrade` (runs npm) |
| Release archive | download from [Releases](https://github.com/Twigpine/zero/releases) | `zero upgrade` |
| mise | `mise use -g ubi:Twigpine/zero` | `mise upgrade` |
| `go install` | `go install github.com/Gitlawb/zero/cmd/zero@latest` | rerun the command |
| Source | `go build -o zero ./cmd/zero` | rebuild |

`zero upgrade` follows whatever owns the binary, with one exception it cannot
detect. It delegates to npm for an npm install, refuses outright for a Homebrew
keg and names `brew upgrade zero`, and replaces the binary in place for
everything else — including a mise-managed one, which is the exception.

mise is the one case it cannot detect: a mise-managed binary is an ordinary
standalone install on disk, so `zero upgrade` will replace it and leave mise
describing a version that is no longer there. Use `mise upgrade` instead.

Release archives are the substrate for most of the above: the install scripts
and the npm fallback both download a platform archive from a published GitHub
Release and verify its checksum.

## npm

```bash
npm install -g @twigpine/zero
zero
```

The package supports Linux and macOS on x64 and arm64, and Windows on x64
(Windows on ARM runs the x64 build under emulation). It installs a small
`zero` wrapper plus, as an optional dependency, a platform payload with the
native binary and the bundled browser/terminal control helpers. There are no
install scripts and nothing is downloaded from outside the npm registry, so
the install is silent and works identically under npm, Bun, pnpm, and yarn —
no trust or approval steps. See [NPM_PACKAGING.md](NPM_PACKAGING.md) for the
package architecture.

Requirements:

- Node.js 18+
- network access to npm

If the install skipped optional dependencies (`npm install --omit=optional`,
or a package manager configured to do so), the wrapper falls back to
downloading the binary from the matching GitHub Release, with checksum
verification. The fetch is retried on each run until a binary is in place (a
transient network failure heals itself; an install directory the current user
cannot write to keeps failing — rerun the install or the command below with
sufficient permissions). To trigger the fetch manually:

```bash
node "$(npm root -g)/@twigpine/zero/scripts/postinstall.mjs"
```

### Moving from @gitlawb/zero

Zero was published as `@gitlawb/zero` up to 0.9.0. From 0.10.0 it is
`@twigpine/zero`, and the old name gets no further releases.

Both packages provide the `zero` command, so npm refuses to install the new
one while the old one is installed (`EEXIST`). Forcing it is worse: a later
uninstall of the old package removes the shared `zero` command. Quit any
running `zero` (on Windows a running `zero.exe` cannot be removed), then
uninstall the old package before installing the new one:

```bash
npm uninstall -g @gitlawb/zero
npm install -g @twigpine/zero
```

With another package manager, the same two steps:

```bash
bun remove -g @gitlawb/zero && bun add -g @twigpine/zero
pnpm remove -g @gitlawb/zero && pnpm add -g @twigpine/zero
yarn global remove @gitlawb/zero && yarn global add @twigpine/zero
```

Config, sessions, logins, and plugins live in your user config and home
directories, not in the package, so they carry over unchanged.

0.10.0 was also published once as `@gitlawb/zero`, so an old install that
updates lands on a working build. That build prints these steps when `zero`
exits in a terminal, and `zero upgrade` and `zero update --check` print them
too.

## Linux And macOS Script

Install the latest release:

```bash
curl -fsSL https://raw.githubusercontent.com/Twigpine/zero/main/scripts/install.sh | bash
```

From a checkout:

```bash
scripts/install.sh
```

Install a specific version:

```bash
ZERO_VERSION=0.1.0 scripts/install.sh
scripts/install.sh --version 0.1.0
```

Install somewhere else:

```bash
ZERO_INSTALL_DIR="$HOME/bin" scripts/install.sh
scripts/install.sh --install-dir "$HOME/bin"
```

Defaults:

- Repository: `Twigpine/zero`
- Version: latest GitHub release
- Install path: `~/.local/bin/zero`

Requirements: Bash, `curl` or `wget`, `tar`, and `shasum` or `sha256sum`.

## Windows PowerShell Script

Install the latest release:

```powershell
irm https://raw.githubusercontent.com/Twigpine/zero/main/scripts/install.ps1 | iex
```

From a checkout:

```powershell
powershell -ExecutionPolicy Bypass -File scripts/install.ps1
```

Install a specific version:

```powershell
powershell -ExecutionPolicy Bypass -File scripts/install.ps1 -Version 0.1.0
```

Install somewhere else:

```powershell
powershell -ExecutionPolicy Bypass -File scripts/install.ps1 -InstallDir "$env:USERPROFILE\bin"
```

Defaults:

- Repository: `Twigpine/zero`
- Version: latest GitHub release
- Install path: `%LOCALAPPDATA%\zero\bin\zero.exe`

## mise

[mise](https://mise.jdx.dev/) installs Zero straight from the GitHub Release
archives through its `ubi` backend, so there is nothing extra to publish and no
registry in the middle:

```bash
mise use -g ubi:Twigpine/zero
```

Pin a version the same way you would any other tool:

```bash
mise use -g ubi:Twigpine/zero@0.7.1
```

Updates come from `mise upgrade`. `zero upgrade` also works, because a
mise-managed binary is an ordinary standalone install, but then mise's records
describe a version that is no longer on disk. Prefer `mise upgrade`.

## go install

```bash
go install github.com/Gitlawb/zero/cmd/zero@latest
```

Every release so far declares the module path `github.com/Gitlawb/zero`, so
`go install` has to use that path; `github.com/Twigpine/zero/cmd/zero@latest`
fails with "module declares its path as" until a release is tagged with the new
module path.

This builds from source, so it needs Go 1.26.9+ and it does not go through the
release archives. Two consequences worth knowing before you pick it:

- On Linux, Bubblewrap is still required for native sandboxing. Zero resolves
  the helper in this order: an executable `zero-linux-sandbox` next to `zero`,
  then `PATH`, then self-exec of the main binary. Standalone helpers remain
  optional and preferred. See
  [Sandbox Helpers For Source Builds](#sandbox-helpers-for-source-builds).
- `zero upgrade` treats the result as a standalone install and will replace the
  binary with a release build rather than rebuilding from source. If you chose
  `go install` deliberately, rerun it instead.

macOS and Windows need no extra helper.

## From Source

```bash
git clone https://github.com/Twigpine/zero.git
cd zero
go run ./cmd/zero
```

Build a local binary:

```bash
go build -o zero ./cmd/zero
```

Source builds require Go 1.26.9+.

### Sandbox Helpers For Source Builds

Release archives include the platform sandbox helpers. A source build of the
main `zero` binary is enough for Linux native sandboxing. Zero resolves the
helper in this order:

1. an executable `zero-linux-sandbox` next to `zero`
2. then `PATH`
3. then self-exec of the main binary

Bubblewrap must still be installed. Standalone helpers remain optional and
preferred.

A separate helper remains supported if it sits next to `zero` or on `PATH`:

```bash
go build -o zero ./cmd/zero
go build -o zero-linux-sandbox ./cmd/zero-linux-sandbox   # optional
go build -o zero-seccomp ./cmd/zero-seccomp               # optional compatibility wrapper
```

Put `zero` on `PATH`, for example `~/.local/bin`. `zero-seccomp` is kept as a
compatibility wrapper; the sandbox helper applies the Unix-socket filter itself
when that sandbox option is enabled.

macOS uses the system sandbox and does not need an extra helper binary.

### Termux (Android)

Zero can run natively on Android via [Termux](https://termux.dev/). Build with
`GOOS=android` to avoid the `faccessat2` syscall that is blocked by Samsung's
seccomp filter on Android:

```bash
# Install Go in Termux
pkg install golang

# Build Zero for Android
git clone https://github.com/Twigpine/zero.git
cd zero
CGO_ENABLED=0 GOOS=android GOARCH=arm64 go build -ldflags="-s -w" -o zero ./cmd/zero

# Move into PATH
mv zero ~/.local/bin/
```

> **Why `GOOS=android`?** Go 1.26+ detects `runtime.GOOS == "android"` and skips
> the `faccessat2` syscall inside `os/exec.findExecutable`, falling back to
> permission-bit checks. Without this flag, Android's seccomp sends SIGSYS and
> kills the process whenever Zero looks up a binary on `PATH` (git, sh, etc.).

**DNS.** Android does not expose `/etc/resolv.conf`. Go's pure-Go DNS resolver
needs one. Use `proot` to bind-mount Termux's resolver config:

```bash
pkg install proot
proot -b "$PREFIX/etc/resolv.conf:/etc/resolv.conf" zero
```

Create a wrapper at `~/.local/bin/zero` to avoid typing proot every time:

```bash
#!/data/data/com.termux/files/usr/bin/bash
exec proot -b "$PREFIX/etc/resolv.conf:/etc/resolv.conf" ~/.local/bin/zero.bin "$@"
```

**Scroll.** On native Termux (not under PRoot), mouse scrolling works out of the
box. The TUI uses Bubble Tea's `AllMotion` mouse mode by default. If you run Zero
inside PRoot (e.g. through proot-distro), the scroll fix activates `CellMotion`
to avoid PRoot's ptrace interference with the 1003 escape sequence.

**Providers.** Zero works with any OpenAI-compatible provider on Termux. For
example, to use OpenCode Zen's free tier:

```bash
zero providers add opencode \
  --name opencode \
  --model deepseek-v4-flash-free \
  --base-url https://opencode.ai/zen/v1 \
  --set-active
```

Windows source builds can use the main `zero.exe` as the command runner and setup
helper through Zero's built-in self-dispatch path. If you want a release-style
layout anyway, build the standalone helper executables next to `zero.exe`:

```powershell
go build -o zero.exe ./cmd/zero
go build -o zero-windows-command-runner.exe ./cmd/zero-windows-command-runner
go build -o zero-windows-sandbox-setup.exe ./cmd/zero-windows-sandbox-setup
```

## Release Archive Format

Release archives are named:

- `zero-v<version>-linux-<arch>.tar.gz`
- `zero-v<version>-macos-<arch>.tar.gz`
- `zero-v<version>-windows-<arch>.zip`

Supported targets:

- `linux-x64`
- `linux-arm64`
- `macos-x64`
- `macos-arm64`
- `windows-x64` (Windows on ARM runs this build under emulation)

Each archive must have a matching `.sha256` file. The install scripts download
both files, verify the checksum, and then copy the binary into the install
directory.

## Updating

Check for a newer release, then download, verify, and install it in place:

```bash
zero update --check
zero upgrade
```

See the [update guide](UPDATE.md) for update modes, flags, and platform details.

`zero upgrade` behaves differently depending on how Zero was installed, because
overwriting a binary a package manager owns leaves that manager describing a
version that is no longer there:

- **npm**: runs `npm install -g @twigpine/zero@latest` for you. An install of
  the old `@gitlawb/zero` package refuses instead and prints the steps in
  [Moving from @gitlawb/zero](#moving-from-gitlawbzero): that name gets no
  further releases, so npm would only reinstall the same version.
- **Homebrew**: refuses, and tells you to run `brew upgrade zero`. Replacing the
  keg binary directly would be reverted by the next `brew upgrade` or
  `brew reinstall`.
- **everything else**: downloads the verified release archive and replaces the
  binary in place.

Homebrew is detected by the binary living inside a Cellar keg that carries
Homebrew's own `INSTALL_RECEIPT.json`. The keg shape alone is not enough: a
directory tree you built yourself can have exactly that shape, and refusing to
update an install Homebrew never touched would be a real cost for a made-up
reason. The receipt is the thing only Homebrew writes.

An ordinary install under `/usr/local/bin` is left alone even on an Intel Mac
where that is also the Homebrew prefix.
