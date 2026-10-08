# Security Policy

Zero is a terminal coding agent: it reads and edits files in your repository and can run shell
commands and call external model/tool endpoints. We take security reports seriously.

## Reporting a vulnerability

**Please do not open a public issue for security vulnerabilities.**

Report privately through GitHub's private vulnerability reporting:

1. Go to the repository's **Security** tab → **Report a vulnerability** (GitHub Security Advisories).
2. Describe the issue, the affected version/commit, reproduction steps, and the impact.

If you cannot use GitHub Security Advisories, open a regular issue that contains **only** a request to
be contacted for a security report (no details), and a maintainer will arrange a private channel.

Please include, where possible:

- The version, branch, or commit affected.
- A minimal reproduction and the impact (e.g. sandbox escape, secret exposure, RCE).
- Any logs or output, with secrets redacted.

## What to expect

- We aim to acknowledge a report within a few business days.
- We will work with you on a fix and a coordinated disclosure timeline, and credit you in the release
  notes unless you prefer to remain anonymous.

## Scope notes

- Findings that require a malicious local user who already controls the machine running Zero, or that
  depend on a user explicitly disabling the sandbox (`--skip-permissions-unsafe`), are generally
  out of scope — but please report anything you are unsure about.
- Zero sends no telemetry. Network calls go to the model/tool providers you configure (and, for
  keyless web search, to Exa's hosted MCP endpoint at `https://mcp.exa.ai/mcp`).
