// Package agentinit builds the bootstrap prompt for the guided /init flow that
// generates a project AGENTS.md. It seeds the agent with structured repo facts
// from internal/repoinfo so the agent starts from what is already known and
// only investigates the gaps, then writes a concise high-signal AGENTS.md.
package agentinit

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Twigpine/zero/internal/repoinfo"
)

// BuildPrompt returns the bootstrap prompt for `zero init` / `/init`. It runs
// repoinfo.Collect(cwd) and embeds the result as a fact block; collection
// failures are non-fatal (the agent investigates from scratch). now is
// injectable for tests; zero falls back to time.Now.
func BuildPrompt(ctx context.Context, cwd string, now time.Time) string {
	if now.IsZero() {
		now = time.Now()
	}
	facts := collectFacts(ctx, cwd, now)
	return promptHeader + facts + promptInstructions
}

func collectFacts(ctx context.Context, cwd string, now time.Time) string {
	info, err := repoinfo.Collect(ctx, repoinfo.Options{Cwd: cwd, Now: now})
	if err != nil {
		return "Pre-computed repo facts: unavailable (" + err.Error() + "). Investigate the repository yourself.\n"
	}
	return FormatFacts(info)
}

// FormatFacts renders a repoinfo.Info as a compact, model-readable fact block.
// Exported so the headless command can show the same summary.
func FormatFacts(info repoinfo.Info) string {
	var b strings.Builder
	b.WriteString("Pre-computed repository facts (from a local scan — verify and fill gaps):\n")

	if info.PrimaryLanguage != "" {
		fmt.Fprintf(&b, "- Primary language: %s (of %d detected)\n", info.PrimaryLanguage, info.LanguageCount)
	}
	if langs := topLanguages(info.Languages, 5); langs != "" {
		b.WriteString("- Languages: " + langs + "\n")
	}
	fmt.Fprintf(&b, "- Size: ~%d files, ~%d LOC, max dir depth %d\n", info.FileCount, info.LOCEstimate, info.MaxDepth)
	if info.WorkspaceType != "" && info.WorkspaceType != "none" {
		fmt.Fprintf(&b, "- Workspace: %s (%d packages)\n", info.WorkspaceType, info.WorkspacePackageCount)
	}
	if len(info.BuildTools) > 0 {
		b.WriteString("- Build tools: " + strings.Join(info.BuildTools, ", ") + "\n")
	}
	if len(info.TestTools) > 0 {
		b.WriteString("- Test tools: " + strings.Join(info.TestTools, ", ") + "\n")
	}
	if len(info.CICD) > 0 {
		b.WriteString("- CI/CD: " + strings.Join(info.CICD, ", ") + "\n")
	}
	if info.HasGit {
		git := "- Git: yes"
		if info.Branch != "" {
			git += ", branch " + info.Branch
		}
		if info.Contributors90d != nil {
			git += fmt.Sprintf(", %d contributors (90d)", *info.Contributors90d)
		}
		b.WriteString(git + "\n")
	}
	return b.String()
}

func topLanguages(langs []repoinfo.LangStat, n int) string {
	parts := make([]string, 0, n)
	for i, l := range langs {
		if i >= n {
			break
		}
		parts = append(parts, l.Name)
	}
	return strings.Join(parts, ", ")
}

const promptHeader = `Generate an AGENTS.md for this repository so future agent runs start with the right context.

`

const promptInstructions = `
Now investigate the repository to fill in what the facts above don't cover, then write a concise, high-signal AGENTS.md at the repo root. Steps:

1. Use the pre-computed facts as your starting point — don't re-derive what's already stated; investigate the GAPS (conventions, how to build/test/lint, architecture, what to avoid).
2. Read the key entry points, the build/test config, and any existing README or CONTRIBUTING for project-specific rules.
3. Write AGENTS.md with these sections, omitting any that don't apply:
   - A one-line description of what the project is.
   - Build / test / lint commands (the exact commands a contributor runs).
   - Project conventions (naming, file layout, idioms the code follows).
   - Things to avoid (vendored dirs, generated files, gotchas).
4. Keep it tight — aim for under ~60 lines. Prefer imperative, specific rules ("Run make test", not "you could test"). No secrets, no environment-specific paths.
5. If a critical fact is genuinely ambiguous (e.g. two plausible test commands), ask the user ONE batched question; otherwise just write the file.

When done, briefly summarize what you wrote.`
