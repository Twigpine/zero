# Extending Zero

Zero is an open-source terminal coding agent. Out of the box it does the obvious things — read, edit, run, search — but the design point of the project is that **every surface is configurable**. This document is the user-facing guide for that configuration.

If you only want to *use* Zero, the [README](../README.md) is enough. This page is for the other three jobs:

1. Tell the agent about *your* project (drop an `AGENTS.md` in your repo).
2. Add new specialist sub-agents.
3. Wire Zero into the rest of your toolchain (MCP, skills, hooks, plugins).

## 1. Drop a project `AGENTS.md`

When Zero starts in a directory, it looks for project-level instructions and injects them into the system prompt. The lookup walks from your current working directory **up to the nearest git root** and reads the first matching file at each level — general rules at the repo root, more specific rules in sub-trees. Files are labeled with their directory in the prompt (e.g. `## Project guidelines (services/api/AGENTS.md)`).

Accepted file names, in priority order at each level:

| Path | Notes |
| --- | --- |
| `./AGENTS.md` | The classic spot — committed to your repo, shared with the team. |
| `./ZERO.md` | Brand-specific alias. Same format, lower priority. |
| `./.zero/AGENTS.md` | Project-local, hidden, gitignored. Personal notes that stay out of git. |

Matching is **case-insensitive** on the basename, so `AGENTS.md`, `Agents.md`, and `agents.md` resolve to the same file on Windows and macOS. The git-tracked filename in this repo is `AGENTS.md` — keep that on case-sensitive filesystems (Linux, the WSL filesystem, or a CI runner) to match what the loader looks for.

Both files use the same format. YAML frontmatter is optional; the markdown body is loaded as instructions for the agent. Zero reads the file once at session start, so changes take effect on the next `zero` launch — not mid-session.

```markdown
# Project conventions for <your project>

- Build with `make`, not `go build` directly.
- Tests live next to the source file (`foo_test.go` next to `foo.go`).
- Run `make lint` before opening a PR.
- Never edit files under `third_party/` — those are vendored.
```

Tips:

- Keep each file under ~8 KiB. Zero caps the **total** across all matched files at 32 KiB; everything past the cap is dropped.
- Re-state rules in the imperative voice: "Run `make lint`", not "you should consider running the linter".
- Don't put secrets, model IDs, or environment-specific paths in `AGENTS.md`. Use `config.json` for those.
- In a monorepo, drop a narrower `AGENTS.md` in each sub-tree (e.g. `services/api/AGENTS.md`). Zero picks those up automatically when you launch from inside the sub-tree.
- A YAML frontmatter block (`---\n...\n---`) at the top is preserved verbatim in the injected prompt but is not parsed for `globs:` or `alwaysApply:` scoping today — keep the body self-contained.

### Personal guidelines, across every project

For preferences that follow *you*, not a specific repo (tone, tooling habits, workflow), drop a `ZERO.md` in your user config directory: `~/.config/zero/ZERO.md` on Linux/macOS, `%AppData%\zero\ZERO.md` on Windows — the same directory as `config.json` and your personal specialists. Same format and 8 KiB cap as the project files above, and the same case-insensitive basename match.

This file is injected as its own `## User guidelines` section, before the project's `AGENTS.md`/`ZERO.md`, and is labeled as personal preference in the prompt: project guidelines are the later, more specific instruction and take precedence over it when the two conflict.

## 2. Custom specialists

Specialists are Zero's sub-agents. Three scopes, in priority order:

| Scope | Path | Shared? |
| --- | --- | --- |
| Built-in | compiled into Zero | yes — `worker`, `explorer`, `code-review` |
| User | `~/.config/zero/specialists/*.md` | no — your machine only |
| Project | `./.zero/specialists/*.md` | yes — the repo team |

Project overrides user overrides built-in when names collide.

A specialist is a markdown manifest with frontmatter and a system prompt:

```markdown
---
description: Reviews API changes for breaking-change risk and missing tests.
tools: read-only,plan
---

You review API changes. For every changed hunk in `internal/api/` or any file
that ends in `_api.go`:

1. Confirm the public signature is backward-compatible, or note the breaking
   change explicitly with the migration path.
2. Confirm a corresponding test exists in `internal/api/*_test.go` and that
   the new behaviour is exercised.
3. Flag any new exported symbol without a doc comment.

Reply with one JSON object per finding: `{"file", "line", "severity", "message", "fix"}`.
```

CLI management (the prompt is passed inline via `--prompt`):

```bash
zero specialist list
zero specialist show api-reviewer
zero specialist create api-reviewer \
    --project \
    --description "Reviews API changes" \
    --tools read-only,plan \
    --prompt "$(cat api-reviewer.md)"
zero specialist edit api-reviewer --project
zero specialist delete api-reviewer --project
zero specialist path                       # prints the resolved specialists directory
```

The full format spec (frontmatter fields, tool scopes, prompt conventions) is in [`docs/SPECIALISTS.md`](SPECIALISTS.md).

> **Roadmap.** An in-UI specialist manager (create / edit / delete / preview) is on the backlog. Today you use the `zero specialist` CLI subcommands above.

## 3. Skills

Skills are markdown instruction packs the agent can pull in on demand. Each skill is a directory containing a `SKILL.md`. Standalone project skill directories are not supported in this version (shared project-wide skills must go in `AGENTS.md` or as a hook). However, project plugins (section 6) may bundle skills, which are merged into the active run.

Discovery roots (earlier wins on name collisions):

1. **Primary Zero dir** — `$ZERO_SKILLS_DIR` if set, else `$XDG_DATA_HOME/zero/skills`, else `~/.local/share/zero/skills/`
2. **Shared multi-agent dir** — `~/.agents/skills/` when present (read-only discovery; never an install target)
3. **Plugin skill roots** — skills bundled by active plugins (section 6)

A missing directory is fine — Zero just omits it. Management commands (`zero skill add` / `zero skill remove`) always write to the primary Zero dir only; `list` / `info` search primary + `~/.agents/skills`. `skill info` reports lock source/hash when present and whether the on-disk `SKILL.md` has drifted from the recorded hash.

```text
~/.local/share/zero/skills/          # primary (install/remove target)
  run-benchmarks/
    SKILL.md
  write-changelog/
    SKILL.md

~/.agents/skills/                    # optional shared multi-agent root
  shared-review/
    SKILL.md
```

`SKILL.md` format:

```markdown
---
description: Run the project's benchmark suite and summarize the deltas.
---

# Run benchmarks

1. `make bench` — captures the wall-clock and RSS before and after.
2. `benchstat before.txt after.txt` — diffs the two.
3. Report any regression > 5% with the function name and the previous value.
```

Only `name` and `description` are recognized in the frontmatter today. The `name` defaults to the directory name. Within a single skills root, duplicate names are resolved by lexicographic directory order. Across roots, Zero loads the primary directory first, then `~/.agents/skills`, then plugin skill roots; earlier roots win name collisions. Plugin-declared skills (section 6) are merged into the active agent run at plugin activation time, so bundled skills appear in the available skills list and can be loaded with the `skill` tool.

The `skill` tool lets the agent load any discovered skill by name (primary, agents, or plugin).

## 4. Hooks

Hooks fire shell commands on lifecycle events. Configure them in JSON:

- User: `~/.config/zero/hooks.json`
- Project: `./.zero/hooks.json`

```json
{
  "enabled": true,
  "hooks": [
    {
      "id": "block-rm-rf",
      "event": "beforeTool",
      "matcher": "bash",
      "command": "/usr/local/bin/zero-hook-block-rmrf.sh",
      "enabled": true
    },
    {
      "id": "log-session",
      "event": "sessionStart",
      "command": "/usr/local/bin/zero-hook-log.sh",
      "enabled": true
    }
  ]
}
```

The `args` array (when present) is passed verbatim to `exec.CommandContext`. The actual hook payload — event name, matcher, tool call id, tool name, tool input, tool output, status — is delivered to the command as **JSON on stdin**, not via `${...}` substitution. A typical handler reads stdin and decides what to do:

```bash
#!/usr/bin/env bash
# /usr/local/bin/zero-hook-block-rmrf.sh
set -euo pipefail
payload="$(cat)"
if printf '%s' "$payload" | grep -q '"input":"[^"]*rm[[:space:]]+-rf'; then
  echo "refusing rm -rf" >&2
  exit 1
fi
```

Events the agent emits (in dispatch order):

| Event | Fires when | Matcher allowed? |
| --- | --- | --- |
| `beforeTool` | A tool is about to run | yes (tool name) |
| `afterTool` | A tool just returned | yes (tool name) |
| `sessionStart` | A session begins | no |
| `sessionEnd` | A session ends | no |
| `specialistStart` | A sub-agent is spawned | yes (specialist name) |
| `specialistStop` | A sub-agent ends | yes (specialist name) |

A hook's exit code decides what happens next: `0` continues, non-zero blocks the tool call (`beforeTool`) or surfaces an error (`afterTool`). Hook execution is recorded in the audit log; the audit is reachable from the agent's view of past actions, not from a dedicated `zero doctor` check.

> **Roadmap.** An in-UI hooks manager is on the backlog. Today you edit the JSON directly.

## 5. MCP — Model Context Protocol

Zero is both an **MCP client** (it can call external MCP servers) and an **MCP server** (other agents can call its tools).

### As a client — configure MCP servers in `config.json`

```json
{
  "mcp": {
    "servers": {
      "docs": {
        "type": "stdio",
        "command": "docs-mcp",
        "args": ["--port", "7777"]
      },
      "github": {
        "type": "http",
        "url": "https://api.example.com/mcp",
        "headers": { "Authorization": "Bearer YOUR_TOKEN_HERE" }
      }
    }
  }
}
```

Manage via CLI:

```bash
zero mcp add docs --type stdio -- docs-mcp --port 7777
zero mcp add github --type http --url https://api.example.com/mcp \
    --header "Authorization=Bearer YOUR_TOKEN_HERE"
zero mcp list
zero mcp check docs
zero mcp remove github
zero mcp oauth login github
```

Servers are merged from user and project configs (project wins on conflicts). Token-bearing values in `config.json` are sent verbatim — there is no `${env:...}` expansion — so prefer one of:

- A wrapper script that sources the secret and execs the real command.
- A `--header` value produced by command substitution (`"Authorization=Bearer $(print-token)"`) in a private shell config that you keep out of git.
- A secret manager that injects the env var your MCP server reads on its own (the `command` and `args` then run inside that environment).

Project MCP config remains supported for shared team servers, but a project layer
that changes an existing server's target (`url`, `command`, or `args`) does not
inherit user credentials. Clear or replace `headers`, `env`, or `oauth` explicitly,
or use a new server name. OAuth tokens are bound to the resolved server identity,
so changing a server target can require `zero mcp oauth login <server>` again.
OAuth server names ending in `.<32 hex chars>` are reserved for token storage.

### As a server — expose Zero's tools to another agent

```bash
zero serve --mcp
zero serve --mcp -C /path/to/workspace
zero serve --mcp --add-dir /path/to/extra
```

The server speaks MCP over stdio. Configure it from the receiving side as a `stdio` server whose command is `zero serve --mcp`.

`-C/--cwd` sets the workspace root exposed as MCP resources and used by core tools. `--add-dir` (repeatable) widens that resource/tool scope beyond the workspace without granting the sandbox temp roots used by interactive runs.
## 6. Plugins

A plugin is a self-contained directory that bundles tools, hooks, and skills for one capability. Plugins live at:

- User: `~/.config/zero/plugins/<id>/`
- Project: `./.zero/plugins/<id>/`

Each plugin has a `plugin.json` manifest:

```json
{
  "id": "github-pr-review",
  "name": "GitHub PR Review",
  "description": "Adds review tools for GitHub PRs.",
  "version": "1.0.0",
  "tools": [
    { "name": "list_prs", "command": "./tools/list_prs.sh" }
  ],
  "hooks": [
    { "name": "pre-merge-check", "event": "beforeTool", "command": "./hooks/pre-merge.sh" }
  ],
  "skills": [
    { "path": "./skills/review-checklist/SKILL.md" }
  ]
}
```

Install and manage:

```bash
zero plugins add ./github-pr-review      # copy into ~/.config/zero/plugins/ or ./.zero/plugins/
zero plugins list
zero plugins info github-pr-review       # show manifest path, enabled state, lock metadata
zero plugins disable github-pr-review   # sets "enabled": false in plugin.json
zero plugins enable github-pr-review    # sets "enabled": true in plugin.json
zero plugins remove github-pr-review    # alias: rm
```

A plugin is active when it is present under a plugins directory and its `plugin.json` has `"enabled": true` (the default when the field is omitted). Use `zero plugins disable <id>` / `zero plugins enable <id>` to toggle that field without removing the plugin. Pass `--user` to target only user plugins and ignore `./.zero/plugins`; without it, a project plugin with the same id takes precedence. The toggle edits the `enabled` value in place — the rest of the manifest keeps its formatting, field order, and numeric literals — and serializes with `plugins add` / `plugins remove` on the same plugins root.

Plugin commands run with the plugin directory as their working directory. Use relative paths; the loader resolves them at activation time.

> **Roadmap.** An in-UI plugins manager (browse, install, enable / disable) is on the backlog. Today you use the `zero plugins` CLI subcommands above.

## 7. Configuration locations

Five configuration sources, in precedence order (later sources override earlier ones):

| Source | Path / Key | Notes |
| --- | --- | --- |
| Built-in defaults | compiled in | Lowest priority. |
| User config | `~/.config/zero/config.json` | Your machine. Never committed. |
| Project config | `./.zero/config.json` | The repo. Committed (or not, your call). |
| Environment | `ZERO_*` | Provider commands, secrets, skills dir override. |
| CLI flags | `--model`, `--mode`, ... | Highest priority, per-invocation. |

The user config holds things that should follow the user across projects (default provider, default model, theme). The project config holds things the team agreed on (provider catalog, sandbox policies, model restrictions).

The sandbox `additionalWriteRoots` key is **ignored in project config** by design — a checked-out repo cannot widen its own sandbox. Set it in the user config or pass `--add-dir` per-invocation.

## 8. End-to-end example

A team that wants every contributor's Zero to behave the same way commits:

- `AGENTS.md` — project conventions, build commands, do-not-edit lists.
- `.zero/config.json` — provider catalog, default model, allowed tools.
- `.zero/specialists/api-reviewer.md` — the team's PR-review specialist.
- `.zero/hooks.json` — block `rm -rf` on `beforeTool`.
- `.zero/plugins/internal-tooling/` — a plugin that adds the team's internal CLI tools to the agent's toolset.

Each contributor adds only:

- `~/.config/zero/config.json` — their personal API keys, theme, default mode.
- `~/.config/zero/ZERO.md` — personal preferences that follow them across every project (see section 1).
- `~/.local/share/zero/skills/` — personal skills they keep across projects.

That's it. Run `zero` from the repo root and the agent has the team's full instruction set, every contributor's personal setup, and nothing else.

## 9. Reference

- [README](../README.md) — install, quickstart, command reference.
- [docs/SPECIALISTS.md](SPECIALISTS.md) — full specialist manifest spec.
- [docs/STREAM_JSON_PROTOCOL.md](STREAM_JSON_PROTOCOL.md) — `zero exec` I/O contract.
- [docs/INSTALL.md](INSTALL.md) — install from source or release.
