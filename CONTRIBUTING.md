# Contributing

Thanks for your interest in improving this project.

This project is currently in a controlled development phase. During this period,
most implementation work will be completed by the core team while the application
stabilizes. Community participation is still welcome, especially through bug
reports, feature requests, reproduction details, logs, and discussion on existing
issues.

Questions and setup help belong in
[GitHub Discussions](https://github.com/Gitlawb/zero/discussions) (see
[SUPPORT.md](SUPPORT.md)), not in the issue tracker. Feature ideas start in the
[Ideas](https://github.com/Gitlawb/zero/discussions/categories/ideas) category;
an idea that gains traction there is the path to an approved issue and,
eventually, an accepted PR.

Please read this policy before opening a pull request. Pull requests that do not
follow it may be closed without review.

## Current Contribution Policy

The project is not accepting unsolicited community pull requests by default while
the application is still reaching a stable baseline.

Community pull requests must be tied to an existing issue that has already been
reviewed and approved by the core team. Approval is shown by the `issue-approved`
label on the related issue.

If a pull request is opened before the related issue has the `issue-approved`
label, the pull request will be closed without review. Maintainers may ask the
author to continue the discussion on the issue instead.

Team members may open pull requests as part of the internal development cycle.
This does not indicate that the same issue is open for community implementation
unless the issue has been explicitly approved for outside contribution.

## How to Contribute

There are three primary ways to contribute:

1. Open a bug report with clear reproduction steps.
2. Propose a feature in
   [Discussions Ideas](https://github.com/Gitlawb/zero/discussions/categories/ideas),
   describing the problem, use case, and expected behavior. A feature-request
   issue is filed only after the idea gains traction there or a maintainer asks
   for one.
3. Add useful context to an existing issue, such as logs, screenshots, examples,
   affected versions, or reproduction cases.

Please search existing issues before opening a new one. Duplicate or low-signal
issues may be closed so maintainers can focus on actionable reports.

## Issue First Requirement

All community pull requests require an approved parent issue.

Before starting implementation work:

1. For a bug, open an issue using the bug-report template. For a feature, start
   in Discussions Ideas; the feature-request issue comes after the discussion.
2. Describe the bug, feature request, or functional change clearly.
3. Wait for the core team to review the issue.
4. Do not open a pull request unless the issue has the `issue-approved` label.

An approved issue means only that the specific issue has been accepted for work.
It does not approve unrelated future work, future pull requests, or future issues
from the same author. Each issue is reviewed case by case.

## Contribution Volume and Batching

To keep triage and review sustainable while the project stabilizes, please keep
your contribution flow to a size the core team can realistically work through:

- One issue per bug. File each defect as its own issue with its own
  reproduction. Please do not bundle several unrelated findings into a single
  "audit" issue; separate issues are far easier to triage, approve, and close
  individually.
- Keep only a few items open at a time. If you already have several issues or
  pull requests awaiting review, let those move through the queue before opening
  more. A large simultaneous burst from one contributor slows review for
  everyone, even when each item is valid.
- AI-assisted contributions are welcome, held to the same bar. Automated audits
  and generated patches are fine, but each issue and pull request must be
  individually verified, scoped, and reproducible before you open it. Please do
  not post bulk machine-generated findings unfiltered.

These apply to all contributors and are about review bandwidth, not the value of
any single report. Maintainers may ask you to consolidate, split, or hold
submissions to keep the queue manageable.

## Pull Request Requirements

Before opening a community pull request, make sure all of the following are true:

- The pull request links to an existing issue.
- The linked issue already has the `issue-approved` label.
- The pull request is focused on only that approved issue.
- The pull request explains what changed and why.
- The pull request includes tests or verification notes when appropriate.
- UI changes include screenshots or video when possible.
- The pull request does not include fixes, features, refactors, formatting
  changes, or other work outside the approved issue scope unless that expansion
  was discussed with and approved by project team members.

Use `Fixes #123`, `Closes #123`, or another clear link in the pull request
description so maintainers can confirm the approved parent issue.

Pull requests that include unrelated changes, broad rewrites, formatting-only
changes, or unapproved feature work may be closed without review. Scope drift
will be rejected or called out for correction before a pull request can be
approved.

## Project Consistency

Pull requests should stay within the project's existing technical direction.
Changes that introduce a new implementation language, shift existing code away
from the core language used by the project, or add a new language/runtime to the
codebase are unlikely to be accepted unless they were discussed and approved by
maintainers before implementation.

The same applies to dependency changes. Pull requests that replace, rebase, or
significantly restructure the project's dependencies without prior discussion are
likely to be rejected. A preference-based explanation such as "this is better" is
not enough. Dependency changes need a clear project benefit, such as fixing a
specific bug, addressing a security issue, improving compatibility, or supporting
an approved feature.

## Supported platforms

Zero publishes five native prebuilt artifacts from
`.github/workflows/release-artifacts.yml`:

- `linux-x64` (`linux/amd64`)
- `linux-arm64` (`linux/arm64`)
- `macos-arm64` (`darwin/arm64`)
- `macos-x64` (`darwin/amd64`)
- `windows-x64` (`windows/amd64`)

The shell installer and npm wrapper resolve the applicable artifacts from that
list. The PowerShell installer supports the published `windows-x64` artifact;
there is no native Windows ARM64 artifact. Windows on ARM users can run the x64
build under emulation or build from source.

Android/Termux is also supported even though it has no distinct release
artifact. The npm wrapper maps Android to the Linux artifact, and
`docs/INSTALL.md` documents the native `android/arm64` source build needed on
devices whose seccomp policy blocks the Linux build's syscall path.

Other `GOOS`/`GOARCH` values, including Plan 9, are unsupported. They are not a
review criterion. Do not block a pull request for failing to compile on an
unsupported target. Tests may use `plan9` as a fixture for an unsupported OS.

## What We Prioritize

The core team will prioritize:

- Bug reports with clear reproduction steps.
- Functional defects that affect current users.
- Feature requests that align with the project roadmap.
- Requests that explain the user problem and expected behavior.
- Reports with logs, screenshots, test cases, or other concrete evidence.

Most approved implementation work will be completed by team members until the
project reaches a more stable status.

## What We Are Not Prioritizing

The project is not currently prioritizing:

- Unsolicited implementation pull requests.
- Pull requests without an approved parent issue.
- Typo-only changes.
- Formatting-only changes.
- Refactors without a clear functional benefit.
- Large rewrites that were not discussed with maintainers first.
- New features that have not been approved through an issue.

## Submitting a Bug Report

When filing a bug report, please include:

- The version, branch, or commit you are using.
- Your operating system and relevant environment details.
- Steps to reproduce the issue.
- Expected behavior.
- Actual behavior.
- Relevant logs, screenshots, or error messages.
- A minimal reproduction case when possible.

Clear bug reports are the most useful way to help the project during this phase.

## Submitting a Feature Request

Feature requests start as a thread in
[Discussions Ideas](https://github.com/Gitlawb/zero/discussions/categories/ideas),
not as an issue. The feature-request issue form exists for the step AFTER that:
when an idea has gained traction in Discussions or a maintainer has asked for an
issue to track it. Whether in the discussion or the eventual issue, please
include:

- The problem you are trying to solve.
- The behavior or functionality you are requesting.
- Why the request belongs in this project.
- Any alternatives or workarounds you have considered.
- Examples, screenshots, or prior discussion if relevant.

Please do not begin implementation until the issue has been reviewed and marked
with the `issue-approved` label.

## Review Expectations

Maintainers may close issues or pull requests that do not follow these guidelines.
This is not a judgment on the contributor; it is part of keeping the development
cycle focused while the project stabilizes.

If an issue or pull request is closed, the best next step is usually to clarify
the issue, provide more evidence, or wait for the project to reach a more open
contribution phase.

## Getting Help

General questions, setup help, and "is this a bug or my config" uncertainty all
go to
[Discussions Q&A](https://github.com/Gitlawb/zero/discussions/categories/q-a).
If you are unsure whether a proposed code change is appropriate, raise it in
Discussions Ideas or on the relevant issue first and ask. Please keep discussion
focused, specific, and respectful of maintainer time.
