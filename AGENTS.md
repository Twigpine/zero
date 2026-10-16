# Repository Guidelines for Zero

These instructions apply to all work in this repository. For the user-facing
guide to extending Zero with specialists, hooks, plugins, MCP, and skills, see
[docs/EXTENDING.md](docs/EXTENDING.md).

## 1. Contribution and Pull Request Rules

- Before opening any pull request, **all contributors**—including maintainers,
  community contributors, and coding agents—must read and follow
  [CONTRIBUTING.md](CONTRIBUTING.md).
- Community pull requests require an existing parent issue with the
  `issue-approved` label. Team members may open pull requests through the
  internal development process described in `CONTRIBUTING.md`.
- Keep each change focused on the approved or assigned scope. Do not include
  unrelated fixes, refactors, formatting churn, generated output, or existing
  local changes in the same commit or pull request.
- Discuss new implementation languages, runtimes, major dependency changes,
  and broad architectural rewrites with maintainers before implementation.
- Pull request descriptions must explain what changed and why, link the parent
  issue when required, and list the tests or verification performed. Include
  screenshots or a short recording for user-visible UI changes when practical.

## 2. Repository and Implementation Conventions

- Use the Go version declared in `go.mod`. Do not hardcode a different local
  toolchain version in scripts or documentation.
- Use the repository build and release commands (`make` and
  `go run ./cmd/zero-release ...`) instead of inventing parallel build flows.
- Keep tests beside their source files (`foo_test.go` next to `foo.go`). Add a
  regression test for behavior changes and run affected concurrent code under
  the race detector.
- Never edit files under `third_party/`; they are vendored.
- Prefer one cross-platform function with small conditional checks over
  duplicated platform-specific helpers when the behavior can remain unified.
- Do not commit generated benchmark reports from
  `internal/perfbench/reports/*.json`; reports are configuration-specific
  evidence, not repository state.
- Preserve the user's working tree. Do not overwrite, delete, stage, or commit
  unrelated tracked or untracked files.

## 3. Required Validation

Run validation from the repository root before committing, opening a pull
request, or completing an implementation task:

1. **Formatting check**: `make fmt-check`. If it fails, format with
   `go fmt ./...` (or `make fmt`) and run the check again.
2. **Vet**: `go vet ./...` (or `make vet`).
3. **Tests**: `go test ./...`. Use `make test` for the full race-enabled suite,
   or run focused tests with `-race`, when concurrency is affected.
4. **Build**: `go run ./cmd/zero-release build`.
5. **Smoke test**: `go run ./cmd/zero-release smoke`.
6. **Advisory lint**: `make lint-static`.
7. **Security**: `make vulncheck`.
8. **Diff hygiene**: `git diff HEAD --check` (covers staged and unstaged
   tracked changes).

`make lint` currently runs the formatting check and `go vet`; it does **not**
run golangci-lint. The pinned golangci-lint job is advisory in CI while the
existing repository-wide backlog is cleaned up. Fix findings introduced by or
related to the current change. Unrelated pre-existing advisory findings do not
justify expanding a focused pull request; report them separately.

Formatting, vet, tests, build, smoke, diff hygiene, and govulncheck are hard
requirements. If a related check fails, fix it. If a required check cannot run
because of the environment or fails for an unrelated external reason, report
the exact failure and obtain maintainer direction rather than silently ignoring
it.

## 4. Common Review Blockers

These classes drive multi-round reviews. Fix them before requesting review:

- **Fresh base:** Rebase onto the current PR base (`main` or stacked target)
  before review. A stale head that rolls back mainline commits is a hard
  blocker, not a merge-time detail. Resolve conflicts by keeping upstream
  behavior unless the PR intentionally changes it.
- **Platform truth:** Code and tests must pass on Linux, macOS, and Windows.
  Compare paths after canonicalization (physical/`EvalSymlinks` form); never
  assert raw `t.TempDir()` spellings (`/var` vs `/private/var` on macOS, short
  paths on Windows). Prefer one cross-platform function with small `GOOS`
  checks over duplicated helpers that drift.
- **Supported targets only:** CONTRIBUTING.md names the five native prebuilt
  release artifacts (`linux-x64`, `linux-arm64`, `macos-arm64`, `macos-x64`,
  `windows-x64`) and the additional supported Android/Termux source-build and
  artifact-fallback path. Other `GOOS`/`GOARCH` combinations, including
  `plan9`, are unsupported and are not a review criterion. Do not block a PR
  for failing to compile on an unsupported `GOOS`/`GOARCH`. Tests may use
  `plan9` as a fixture for "unsupported".
- **Security edges:** Keep secrets out of argv, env dumps, and logs. Redact
  success and error paths (including stderr). Fail closed on ownership, lease,
  and permission checks. Do not rely on pre-open path resolution
  (`EvalSymlinks` then open) for containment: that is a check-to-use race.
  Bind containment at open/use time with rooted or handle-relative,
  traversal-resistant APIs. If a no-follow API is used, apply it to every
  traversed component and enforce the platform's reparse-point protections;
  final-component-only no-follow is insufficient. On multi-step setup, roll back
  only what this run created; never destroy pre-existing resources you did
  not create; never report success when cleanup or unlock failed.
- **Normalize before matching:** Any transform that REMOVES bytes without
  leaving a gap is also a reassembler, so it has to run before whatever matches
  on the result. Redacting by shape and then stripping control bytes lets a
  credential split by a NUL, an ESC or a C1 byte pass the patterns as two
  fragments and be rejoined on the way out; comparing a path against a form
  produced by the same resolver the kernel just used makes a redirect agree with
  itself. Strip, decode, or canonicalize first, then match. The unsplit value
  passing is not evidence, so the test needs a split case.
- **Atomic shared state:** Write a complete temporary file, then atomically
  replace the destination so concurrent readers never see a partial write.
  Exclusive create or a write lock alone is not enough for readers. Serialize
  the full read-modify-write sequence for lockfiles and shared stores.
- **Tests match the claim:** Every behavior or security-boundary change needs a
  regression test, including the failure path. Path-sensitive logic needs at
  least one non-Linux path case (or a hermetic fake that exercises the same
  normalization).
- **Honest scope:** PR description, help text, and comments must match what
  shipped. Wire advertised entry points or shrink the claim. Do not bundle
  unrelated fixes. Do not widen security allowlists by name alone; check
  classification and side effects too.
- **Host tool version floors:** Zero shells out to `git` and other host tools,
  and the project does not declare a minimum version for any of them. A flag,
  subcommand, or config key that is not long-established therefore raises that
  floor silently, and the failure on an older host is an unhelpful usage error
  rather than a clear message. Before adding one, check when it landed and say
  so where a caller would look. Record the floor for the exact subcommand you
  call, not for the flag in general: `--end-of-options` reached Git's shared
  parser in 2.24, but `git rev-parse` only accepted it from 2.30, and
  `git checkout` and `git reset` later still. Likewise `--show-current` is
  `git branch` 2.22, and `branch.autoSetupMerge=inherit` is 2.35. Then either
  keep a fallback for older versions or gate the call. A test that depends on a
  newer feature must skip below its floor instead of failing, and say which
  version it needs.
- **Prove the test fails without the change:** A regression test earns its
  place only if it fails on the unfixed code, for the reason it claims. Run it
  both ways and quote the failure in the PR. The recurring miss is not a
  missing test but a test that never reaches the behavior it names, because an
  earlier guard rejects the input first, so it passes with and without the fix.
  When a test targets a specific layer, call that layer directly rather than
  relying on a public entry point that may refuse earlier.
- **Hermetic tests:** Tests must not read or write the developer's real config,
  cache, or state directories. Redirect every root the code under test
  resolves, not just config. `os.UserConfigDir` and `os.UserCacheDir` read
  `XDG_CONFIG_HOME` and `XDG_CACHE_HOME` only on Linux and the BSDs. On macOS
  they derive from `HOME`, and on Windows from `%AppData%` and `%LocalAppData%`.
  Setting the XDG variables alone still leaves a macOS run writing to the real
  home, so set `HOME` as well, or take the directory as a parameter. Apply it to
  every test that reaches the same storage, not only the one where it was first
  needed; a test that passes on a clean machine and pollutes a real one is a
  blocker.
- **Error codes vary by platform and by flag combination:** A check written
  against the code one target returns can be silently inert on another while
  still looking correct. `openat(O_DIRECTORY|O_NOFOLLOW)` on a symlink may
  report `ENOTDIR` or `ELOOP` depending on the platform, because the kernel can
  reject the directory mismatch before it ever resolves the link; accept both
  rather than assuming the one POSIX names. On Windows, `\\?\` and `\\.\`
  prefixes are not UNC paths, and an `NtCreateFile` information class must match
  the struct actually passed. Confirm the code each supported target returns
  instead of reasoning from the man page.
