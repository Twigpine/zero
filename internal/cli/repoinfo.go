package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/Twigpine/zero/internal/repoinfo"
)

type repoInfoOptions struct {
	json bool
	cwd  string
}

func runRepoInfo(args []string, stdout io.Writer, stderr io.Writer, deps appDeps) int {
	options, help, err := parseRepoInfoArgs(args)
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		return exitUsage
	}
	if help {
		writeRepoInfoHelp(stdout)
		return exitSuccess
	}
	workspaceRoot, err := resolveWorkspaceRoot(options.cwd, deps)
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		return exitUsage
	}
	now := time.Now
	if deps.now != nil {
		now = deps.now
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	info, err := repoinfo.Collect(ctx, repoinfo.Options{Cwd: workspaceRoot, Now: now()})
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		return exitCrash
	}
	if options.json {
		data, err := json.MarshalIndent(info, "", "  ")
		if err != nil {
			fmt.Fprintln(stderr, err.Error())
			return exitCrash
		}
		fmt.Fprintln(stdout, string(data))
		return exitSuccess
	}
	fmt.Fprint(stdout, formatRepoInfo(info))
	return exitSuccess
}

func parseRepoInfoArgs(args []string) (repoInfoOptions, bool, error) {
	var options repoInfoOptions
	for index := 0; index < len(args); index++ {
		arg := args[index]
		switch {
		case arg == "-h" || arg == "--help" || arg == "help":
			return options, true, nil
		case arg == "--json":
			options.json = true
		case arg == "-C" || arg == "--cwd":
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return options, false, err
			}
			options.cwd = value
			index = next
		case strings.HasPrefix(arg, "--cwd="):
			options.cwd = strings.TrimSpace(strings.TrimPrefix(arg, "--cwd="))
		default:
			return options, false, execUsageError{fmt.Sprintf("Unknown repo-info flag: %s", arg)}
		}
	}
	return options, false, nil
}

const repoInfoTopLanguages = 8

func formatRepoInfo(info repoinfo.Info) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Files:        %d\n", info.FileCount)
	fmt.Fprintf(&b, "Directories:  %d (max depth %d)\n", info.DirectoryCount, info.MaxDepth)
	fmt.Fprintf(&b, "Est. LOC:     %d\n", info.LOCEstimate)
	if info.CommitCount != nil {
		fmt.Fprintf(&b, "Commits:      %d\n", *info.CommitCount)
	}
	if info.BranchCount != nil {
		fmt.Fprintf(&b, "Branches:     %d\n", *info.BranchCount)
	}
	if info.TagCount != nil {
		fmt.Fprintf(&b, "Tags:         %d\n", *info.TagCount)
	}
	if info.PrimaryLanguage != "" {
		fmt.Fprintf(&b, "Primary lang: %s\n", info.PrimaryLanguage)
	}
	if len(info.Languages) > 0 {
		b.WriteString("Languages:\n")
		shown := info.Languages
		extra := 0
		if len(shown) > repoInfoTopLanguages {
			extra = len(shown) - repoInfoTopLanguages
			shown = shown[:repoInfoTopLanguages]
		}
		for _, lang := range shown {
			fmt.Fprintf(&b, "  %-14s ~%d LOC  (%d files)\n", lang.Name, lang.LOCEstimate, lang.FileCount)
		}
		if extra > 0 {
			fmt.Fprintf(&b, "  +%d more\n", extra)
		}
	}
	fmt.Fprintf(&b, "Workspace:    %s", info.WorkspaceType)
	if info.WorkspacePackageCount > 0 {
		fmt.Fprintf(&b, " (%d packages)", info.WorkspacePackageCount)
	}
	b.WriteString("\n")
	writeRepoInfoList(&b, "Build tools", info.BuildTools)
	writeRepoInfoList(&b, "Test tools", info.TestTools)
	writeRepoInfoList(&b, "CI/CD", info.CICD)
	if info.Branch != "" {
		fmt.Fprintf(&b, "Branch:       %s\n", info.Branch)
	}
	if info.RemoteURL != "" {
		fmt.Fprintf(&b, "Remote:       %s\n", info.RemoteURL)
	}
	if info.AgeDays != nil {
		fmt.Fprintf(&b, "Age:          %d days\n", *info.AgeDays)
	}
	if info.Contributors90d != nil {
		fmt.Fprintf(&b, "Contributors: %d (last 90d)\n", *info.Contributors90d)
	}
	if info.CommitVelocity30d != nil {
		fmt.Fprintf(&b, "Velocity:     %d commits (last 30d)\n", *info.CommitVelocity30d)
	}
	return b.String()
}

func writeRepoInfoList(b *strings.Builder, label string, items []string) {
	if len(items) == 0 {
		return
	}
	sorted := append([]string{}, items...)
	sort.Strings(sorted)
	fmt.Fprintf(b, "%-13s %s\n", label+":", strings.Join(sorted, ", "))
}

func writeRepoInfoHelp(w io.Writer) {
	fmt.Fprint(w, `zero repo-info — characterize the current repository (local git only)

Usage:
  zero repo-info [--json] [--cwd <dir>]

Flags:
  --json        Emit the full characterization as JSON.
  -C, --cwd     Run against a different directory.
`)
}
