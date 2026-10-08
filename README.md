<p align="center">
  <img src="docs/assets/zero-logo.png" alt="Zero" width="385">
</p>

<p align="center"><strong>A terminal coding agent you own.</strong></p>

<p align="center">
  <a href="LICENSE"><img alt="license" src="https://img.shields.io/badge/license-MIT-blue"></a>
  <img alt="Go 1.26.6+" src="https://img.shields.io/badge/Go-1.26.6+-00ADD8?logo=go&logoColor=white">
  <img alt="25+ providers" src="https://img.shields.io/badge/providers-25+-34E2EA">
  <a href="https://discord.gg/CaQDS6wdFn"><img alt="Discord" src="https://img.shields.io/badge/Discord-join-5865F2?logo=discord&logoColor=white"></a>
  <br>
  <strong>English</strong> | <a href="README_ZH.md">中文</a>
</p>

Zero is an AI coding agent for your local terminal. It can inspect a repository,
edit files, run commands, use browser/terminal helpers, and keep durable local
sessions while you choose the model and the permission level.

```bash
zero
zero exec "fix the failing test in ./pkg"
zero exec --output-format stream-json < turns.jsonl
```

## Why Zero

- **Use the model you want.** Bring OpenAI, Anthropic, Gemini, Groq, OpenRouter,
  DeepSeek, Mistral, xAI, Qwen, Kimi, GitHub Models, Ollama, LM Studio, Atomic Chat, or any
  OpenAI-/Anthropic-compatible endpoint.
- **Stay in control.** File writes, shell commands, network access, and
  out-of-workspace writes go through Zero's permission and sandbox policy.
- **Works in the terminal.** The TUI has model/provider pickers, image input,
  slash commands, live plan/tool rendering, scrollback, themes, and resume/fork
  support.
- **Works without the TUI.** `zero exec` is scriptable, supports text/JSON/
  stream-JSON I/O, isolated worktrees, spec-first runs, and meaningful exit
  codes for CI.
- **Keeps context local.** Sessions are stored on disk, searchable, resumable,
  and never uploaded as telemetry by Zero.
- **Extensible when you need it.** Use MCP servers, skills, plugins, hooks, and
  specialist subagents from the same CLI.

## Install

### npm

```bash
npm install -g @gitlawb/zero
zero
```

The npm package is a small wrapper whose platform build (Linux and macOS on
x64/arm64, Windows on x64 — including the browser/terminal control helpers)
installs as an optional dependency straight from the npm registry — no install
scripts, no downloads outside npm. Bun, pnpm, and yarn work the same way with
no trust or approval steps. Installs that skip optional dependencies
(`--omit=optional`) still work: the wrapper fetches the binary from the
matching GitHub Release whenever it is missing. Windows on ARM runs the x64
build under emulation. See [docs/NPM_PACKAGING.md](docs/NPM_PACKAGING.md) for
how the package is put together.

### Install scripts

Linux/macOS:

```bash
curl -fsSL https://raw.githubusercontent.com/Gitlawb/zero/main/scripts/install.sh | bash
```

Windows PowerShell:

```powershell
irm https://raw.githubusercontent.com/Gitlawb/zero/main/scripts/install.ps1 | iex
```

### From source

Source builds require Go 1.26.6+.

```bash
git clone https://github.com/Gitlawb/zero.git
cd zero
go run ./cmd/zero
```

Release installers and the npm wrapper require published GitHub Release assets.
If you are testing before the first public release, build from source:

```bash
go build -o zero ./cmd/zero
```

On Linux, build the sandbox helper too if you want native sandboxing:

```bash
go build -o zero-linux-sandbox ./cmd/zero-linux-sandbox
go build -o zero-seccomp ./cmd/zero-seccomp   # optional compatibility wrapper
```

Put `zero` and `zero-linux-sandbox` in the same directory on `PATH`
(`~/.local/bin` is a good default). macOS does not need an extra helper binary.
Windows source builds can use the main `zero.exe` as their sandbox helper; release
archives still ship standalone Windows helper executables.

More install details: [docs/INSTALL.md](docs/INSTALL.md).

## First Run

Start the TUI:

```bash
zero
```

The setup wizard helps you pick a provider and model. You can also configure
providers from the command line:

```bash
zero setup
zero providers list
zero models list
zero doctor
```

For API providers, set the matching environment variable before setup or enter
the key in the wizard:

```bash
export OPENAI_API_KEY=sk-...
export ANTHROPIC_API_KEY=...
export GEMINI_API_KEY=...
export AIMLAPI_API_KEY=...
export LONGCAT_API_KEY=...
export FIREWORKS_API_KEY=...
export MINIMAX_API_KEY=...
export MINIMAXI_API_KEY=...
```

To configure AI/ML API directly, run:

```bash
zero providers setup aimlapi --set-active
```

To configure Meituan LongCat (LongCat-2.0) directly, run:

```bash
zero providers setup longcat --set-active
```

To configure Fireworks AI directly, run:

```bash
zero providers setup fireworks --set-active
```

MiniMax presets use the Anthropic-compatible endpoints for the global and China
regions:

```bash
zero providers add minimax --set-active
zero providers add minimaxi-cn --set-active
```

To use the OpenAI-compatible endpoints instead, add a custom compatible profile
for the required region:

```bash
zero providers add custom-openai-compatible \
  --name minimax-openai \
  --model MiniMax-M3 \
  --base-url https://api.minimax.io/v1 \
  --api-key-env MINIMAX_API_KEY \
  --set-active

zero providers add custom-openai-compatible \
  --name minimax-cn-openai \
  --model MiniMax-M3 \
  --base-url https://api.minimaxi.com/v1 \
  --api-key-env MINIMAXI_API_KEY \
  --set-active
```

For local models, run Ollama, LM Studio, or the [Atomic Chat](https://atomic.chat)
desktop app, then use `zero setup` or `zero providers detect`. For Atomic Chat,
load a model and enable its local OpenAI-compatible API (default
`http://127.0.0.1:1337/v1`). Choose `atomic-chat-local`; detection includes the
loaded model ID in the add command. If no usable ID is discovered, load a model
and retry. Model IDs requiring shell-specific quoting use interactive setup.

## Daily Use

### Interactive TUI

```bash
zero
```

Useful controls:

| Control | Action |
|---|---|
| `Enter` | send the prompt |
| `/` | open slash-command suggestions |
| `Ctrl+X` then letter | common slash commands (e.g. `m` → `/model`; `Ctrl+X` `?` for full list) |
| `Ctrl+P` / `Ctrl+N` | previous / next item in menus (arrows still work) |
| `Shift+Tab` | cycle permission mode |
| `Ctrl+B` | show/hide the sidebar |
| `Ctrl+C` | cancel, exit, or return from a `/btw` conversation |

Common slash commands:

| Command | Purpose |
|---|---|
| `/model`, `/provider` | switch the active model/provider |
| `/spec`, `/plan` | draft and review a plan before building |
| `/image` | attach an image for vision-capable models |
| `/resume`, `/rewind` | continue or roll back local sessions |
| `/new` | start a fresh session in place (previous session stays on disk) |
| `/btw [question]` | ask in an isolated fork without adding the side conversation to the main session |
| `/loop` | repeat a prompt or custom `/command` on an interval (`/loop 5m /babysit-prs`) or self-paced |
| `/compact`, `/context` | manage context usage |
| `/permissions`, `/tools` | inspect available tools and policy |
| `/add-dir` | allow an extra write directory for this session |
| `/theme`, `/doctor`, `/config` | adjust appearance and inspect setup |

### Headless `exec`

```bash
zero exec "explain internal/agent/loop.go"
zero exec --model claude-sonnet-4.5 "refactor the config loader"
zero exec --use-spec "add rate limiting to the API client"
zero exec --worktree "try the migration in an isolated worktree"
zero exec --resume
zero exec --fork <session-id> "try the other approach"
```

Programmatic use:

```bash
zero exec --input-format stream-json --output-format stream-json < turns.jsonl
```

The stream-JSON contract is documented in
[docs/STREAM_JSON_PROTOCOL.md](docs/STREAM_JSON_PROTOCOL.md).

## Safety Model

Zero is designed to make side effects visible.

- Workspace reads are allowed by default.
- File writes are limited to the workspace unless you grant another directory.
- Shell commands, network access, destructive commands, and elevated actions are
  permission-gated.
- `--add-dir <path>` and `/add-dir <path>` grant additional write roots without
  giving the agent the whole filesystem.
- Unsafe/autonomous modes are explicit opt-ins.
- Secrets are redacted from tool output and logs where Zero controls the surface.

Example:

```bash
zero --add-dir ../docs-site
zero exec --add-dir ../shared "update both repos"
```

Sandbox behavior can be inspected with:

```bash
zero sandbox policy
zero sandbox grants list
```

## Web And Local Control

Zero includes local file/search/edit/shell tools, `web_fetch` for public URLs,
and MCP support for additional tools.

`web_fetch` refuses loopback, private and other special-use addresses: it checks
the URL before asking permission, resolves the host, and dials the address it
validated so a name cannot resolve to something else in between.

If `HTTP_PROXY`/`HTTPS_PROXY` is set, `web_fetch` and the provider connectivity
probe use it, and that last step changes: the proxy is dialed and the target
hostname is sent to it, so the proxy decides which address the request actually
reaches. The URL is still checked and resolved locally first, but a proxy that
answers differently can reach a private service. A forward proxy already sees
and can rewrite every request through it, so this is the trust you accept by
configuring one. Leave the variables unset for the checks to be enforced end to
end.

For local dev servers, use shell commands such as `curl` through `exec_command`
so the normal sandbox and permission policy applies. Long-running commands stay
attached to a background terminal session and can be listed or stopped from the
TUI.

The npm package also includes browser and terminal helper packages used by local
browser/terminal tools. Source builds can use the same helpers when they are on
`PATH` or configured in Zero's local-control settings.

## Common Commands

```text
zero                  interactive TUI
zero exec             one-shot or scripted agent run
zero setup            first-run provider setup
zero auth             OAuth/login helpers for supported providers
zero models           model registry and capabilities
zero providers        provider profiles and detection
zero doctor           setup, key, and connectivity checks
zero context          context-budget report
zero repo-map         deterministic repository map
zero repo-info        local repository summary
zero search | find    search local session history
zero sessions         inspect, resume, fork, and rewind sessions
zero spec             manage spec-mode drafts
zero specialist       manage specialist subagents
zero skills           manage markdown instruction skills
zero plugins          manage plugins
zero hooks            manage lifecycle hooks
zero mcp              manage MCP servers and tools
zero serve --mcp      expose Zero tools over MCP stdio
zero sandbox          inspect sandbox policy and grants
zero worktrees        prepare isolated git worktrees
zero verify           detect and run local verification checks
zero changes          inspect and commit local git changes
zero usage            token usage and estimated cost
zero cron             scheduled agent jobs
zero update --check   check for newer releases
zero upgrade          download, verify, and install the latest release
```

## Extending Zero

### Project and personal instructions

Zero appends project-specific guidance to the system prompt from the first
`AGENTS.md`, `ZERO.md`, or `.zero/AGENTS.md` file found in each directory from
the git root down to your current working directory (checked in that order
per directory). Files are injected general-to-specific, capped at 8 KiB per
file and 32 KiB total.

A personal `ZERO.md` under `config.UserConfigDir()/zero/ZERO.md`
(`$XDG_CONFIG_HOME/zero/ZERO.md` or `~/.config/zero/ZERO.md` on Linux/macOS,
`%AppData%\Roaming\zero\ZERO.md` on Windows) applies across every workspace, ahead of any project guidelines.

### Plugins

Plugins are discovered from `~/.config/zero/plugins/<name>/plugin.json` (user
scope — `$XDG_CONFIG_HOME` or `~/.config` on every OS, independent of the
`config.UserConfigDir()` path used above) and `<cwd>/.zero/plugins/<name>/plugin.json`
(project scope — resolved from the current working directory, not the repo
root), and managed with `zero plugins`. A manifest can declare:

- `tools` — custom tools (`command`, `args`, `inputSchema`, and a
  `permission` of `prompt` or `deny`; `allow` is honored only when manifest tool
  auto-approval is enabled)
- `hooks` — commands run on `beforeTool`, `afterTool`, `sessionStart`, or
  `sessionEnd`
- `prompts` and `skills` — additional prompt/skill files

MCP servers (`zero mcp`) and standalone markdown skills (`zero skills`) use
the same extension points and can also be wired up outside of a plugin
manifest.

## Appearance And Accessibility

| Control | Effect |
|---|---|
| `NO_COLOR=<anything>` | disables color output |
| `ZERO_THEME=<name>` | selects the startup theme (`auto`, `dark`, `light`, or a color theme like `dracula`, `nord`, `gruvbox`, `tokyo-night`, `catppuccin`, `one-dark`, `solarized-dark`, `rose-pine`, `everforest`, `neon`, `solarized-light`, `dune`) |
| `--theme <name>` | selects the TUI theme from the CLI (same names) |
| `/theme` | opens the theme picker inside the TUI (live preview; `/theme <name>` switches directly) |
| `ZERO_NO_FADE=1` | disables streaming fade animation |

Meaning does not rely on color alone; diffs, permissions, and statuses also use
text or glyph markers.

## Development

```bash
go test ./...
go run ./cmd/zero-release build
go run ./cmd/zero-release smoke
go run ./cmd/zero-perf-bench
```

Experimental: `ZERO_OPENAI_TURN_SESSION=1` enables the optimized OpenAI turn
session (background connection prewarm + request-prefix telemetry) for headless
`zero exec` runs against official OpenAI profiles. Off by default; `0`, `false`,
or `off` disable it. A/B-benchmark it by running the same `zero-perf-bench` suite
with the variable unset and set.

Native ChatGPT Responses sessions are enabled by default. Set
`ZERO_CHATGPT_TURN_SESSION=0`, `false`, or `off` to restore stateless HTTP/SSE
transport.

### Code Quality and Security Checks

Before committing any changes, ensure all Go code quality and security checks pass. The `make` targets below pin each tool to this module's Go version, so they load correctly even when your default `go` toolchain is older — running the plain `go run ...@version` form yourself can select the tool module's own (older) toolchain and fail to load this module instead.

1. **Formatting**: Run `make fmt` (or `go fmt ./...`).
2. **Vetting**: Run `make vet` (or `go vet ./...`).
3. **Linting**: Run `make lint-static`.
4. **Vulnerability Scan**: Run `make vulncheck`.

Use the repository-managed targets rather than globally installed binaries:
the targets apply the module's required Go toolchain as well as the reviewed,
pinned tool versions.

The installed binaries land in `$GOBIN` when it is set, otherwise in
`$GOPATH/bin` (default `~/go/bin`). That directory must be on your `PATH` to
run them directly. If it isn't, add it:

```bash
gobin="$(go env GOBIN)"; [ -z "$gobin" ] && gobin="$(go env GOPATH)/bin"
case ":$PATH:" in *":$gobin:"*) ;; *) export PATH="$PATH:$gobin" ;; esac
```

### Cross-Compile Examples

```bash
go run ./cmd/zero-release build --goos linux --goarch amd64
go run ./cmd/zero-release build --goos windows --goarch amd64 --output dist/zero.exe
```

## Documentation

- [Install](docs/INSTALL.md)
- [Update flow](docs/UPDATE.md)
- [Themes](docs/THEMES.md)
- [Stream-JSON protocol](docs/STREAM_JSON_PROTOCOL.md)
- [Specialists](docs/SPECIALISTS.md)
- [GitHub Action](docs/GITHUB_ACTION.md)
- [Benchmarks](docs/BENCHMARK.md)
- [Performance](docs/PERFORMANCE.md)
- [Agent evals](docs/AGENT_EVALS.md)

## Community

Real-time chat happens on the [Discord server](https://discord.gg/CaQDS6wdFn).

Questions, setup help, ideas, and sharing all live in
[GitHub Discussions](https://github.com/Gitlawb/zero/discussions):

| Category | Use it for |
|---|---|
| [Q&A](https://github.com/Gitlawb/zero/discussions/categories/q-a) | Setup help, provider/model configuration, "how do I" questions |
| [Ideas](https://github.com/Gitlawb/zero/discussions/categories/ideas) | Feature proposals and design discussion before any PR |
| [Show and tell](https://github.com/Gitlawb/zero/discussions/categories/show-and-tell) | Your skills, plugins, MCP setups, themes, and workflows |
| [Announcements](https://github.com/Gitlawb/zero/discussions/categories/announcements) | Releases and project news from the maintainers |

For a good Q&A answer fast, include `zero --version`, your OS and install
method, the provider/model in use, and `zero doctor` output. See
[SUPPORT.md](SUPPORT.md). Bugs belong in
[issues](https://github.com/Gitlawb/zero/issues/new/choose); security reports
follow [SECURITY.md](SECURITY.md), never a public thread.

## Contributing

Contributions are welcome. Read [CONTRIBUTING.md](CONTRIBUTING.md), run the
relevant tests, and open a focused pull request.

Security reports should follow [SECURITY.md](SECURITY.md).

## License

Zero is released under the [MIT License](LICENSE).
