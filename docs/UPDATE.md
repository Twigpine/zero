# Update Flow

`zero update --check` checks the latest GitHub release and compares it with the
local CLI version. `zero update --apply` (or its shorthand, `zero upgrade`)
downloads, verifies, and installs it.

```bash
zero update --check
zero update --check --json
zero update --check --repo Gitlawb/zero
zero update --check --target windows-x64

zero upgrade
zero update --apply
```

`--check` and `--apply` are mutually exclusive. `zero update` requires one of
them explicitly; `zero upgrade` is `zero update` with `--apply` implied.

`--check` is check-only:

- It does not replace the running binary.
- It exits with code `0` when the check succeeds, even when an update is
  available.
- It exits with code `1` when the release check cannot be completed.
- `--json` prints the same result in a machine-readable format for scripts and
  CI.

`--apply` installs the update in place:

- npm installs delegate to `npm install -g @gitlawb/zero@latest`.
- Standalone installs download the release archive, verify its checksum,
  extract it, and atomically replace the running binary plus any installed
  optional sandbox helpers.
- On Windows, the running executable is renamed aside and cleaned up on the
  next `zero update --apply` or `zero upgrade`, since it can't be overwritten
  while running.
- `--target` cannot be combined with `--apply`; it only applies to `--check`,
  since applying always installs onto the current machine.
- `--repo` and `--endpoint` are ignored when applying to an npm-managed
  install: that path delegates to `npm install -g @gitlawb/zero@latest` and
  takes its release from the npm registry, not from GitHub. They still apply to
  `--check` there.
- `--json` serializes Zero's final result. For npm-managed installs, npm may
  also write progress output to stdout, so neither
  `zero update --apply --json` nor `zero upgrade --json` is guaranteed to
  produce a single parseable JSON document. Use `--check --json` for
  machine-readable automation.

Useful flags:

| Flag | Purpose |
|---|---|
| `--repo <owner/repo>` | Use another GitHub repository for `--check` and `--apply`/`upgrade`. Ignored by an npm-managed apply. |
| `--endpoint <url\|owner/repo>` | Use a specific release API URL or repository slug for `--check` and `--apply`/`upgrade`. Ignored by an npm-managed apply. |
| `--timeout <duration>` | Override the default release check timeout. |
| `--target <platform-arch>` | Validate release metadata for another supported target (`--check` only). |

Supported targets are `linux-x64`, `linux-arm64`, `macos-x64`, `macos-arm64`,
`windows-x64`, and `windows-arm64`. Without `--target`, Zero checks the current
platform.

Endpoint resolution order:

1. `--endpoint`
2. `ZERO_UPDATE_RELEASE_URL`
3. `--repo`
4. `https://api.github.com/repos/Gitlawb/zero/releases/latest`

Installer scripts download the matching release asset for the local platform and
verify its `.sha256` file. If Zero is already installed, run `zero upgrade`
instead of reinstalling.

## Windows recovery state (standalone installs)

This section describes Windows only. On Linux and macOS a standalone update
renames the staged file directly over the executable path through the
installation directory's file descriptor, so the replacement is atomic and no
aside copy, marker, or recovery record is ever created — a failed update leaves
the previous binary in place and nothing to resolve.

On Windows, a running executable cannot be replaced in place, so the previous
binary is moved aside to `<binary>.zero-update-<random>.old` first. The updater
records that exact file — bound to its filesystem identity, in per-user state
outside the installation directory — and deletes only that recorded copy after
the next update is verified in place. Backups it did not create are never
removed, so a file such as `zero.exe.before-manual-patch.old` is left alone. A
recorded copy that something else holds open (an on-access scanner, an editor)
stays recorded and is removed by a later update instead.

An update **refuses to run** while unresolved recovery state exists beside the
binary:

| State on disk | Meaning |
|---|---|
| `<binary>.old` (or `<binary>.<suffix>.old`) plus a `.keep` marker, or a per-user update-recovery record naming that copy | A previous update could not restore the original binary. The `.old` file may be the last binary the updater verified; the installed one may be unverified. The `.keep` marker is in the installation directory and can be deleted; the identity-bound record is not, so deleting the marker alone does not end the refusal. |
| `<binary>.…old.<suffix>.recovery` | A previous update could not even write the marker, so it moved the last verified binary to that name. |
| The binary is missing and one or more `*.old` files exist | The previous attempt was interrupted between moves. |

The refusal names the paths involved and the two moves that end the state:
either move the recovery binary back over the executable path, or — if the
installed binary is the one you want — delete the recovery copy itself (not
only the `.keep` marker) after verifying the installed binary, then update
again. Deleting the marker alone is not enough: a failed restore also records
that copy in per-user state outside the installation directory, and the next
update consults that record even if the marker is gone.

That trusted record lives under `%AppData%\zero\update-recovery` (per-user).
The recovery copy and its `.keep` marker sit in the installation directory
(per-machine). A second Windows account on the same machine does not see the
first account's unresolved record; the sibling marker is the machine-visible
signal for every account. There is no separate machine-wide store: writing one
would need a shared writable location and ACL rules the updater does not own.

This is deliberately fail-closed and differs from older releases, which deleted
`<binary>.old` and proceeded. The trade-off is that anyone who can write in the
installation directory can plant `<binary>.old` and `<binary>.old.keep` there
and make every subsequent update refuse until an operator clears them. That is
the safe direction: the alternative is an update that overwrites the only
verified copy of the previous binary. If updates start refusing, inspect those
files before removing them — an installation directory that a lower-privileged
account can write to is itself worth fixing.
