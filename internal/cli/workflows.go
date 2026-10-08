package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/redaction"
	"github.com/Gitlawb/zero/internal/selfverify"
	"github.com/Gitlawb/zero/internal/testrunner"
	"github.com/Gitlawb/zero/internal/verify"
	"github.com/Gitlawb/zero/internal/worktrees"
	"github.com/Gitlawb/zero/internal/zerogit"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

type worktreeCommandOptions struct {
	json    bool
	name    string
	baseDir string
	cwd     string
}

type verifyCommandOptions struct {
	json      bool
	cwd       string
	only      []string
	timeoutMS int
	attempts  int
}

type changesCommandOptions struct {
	json         bool
	cwd          string
	baseRef      string
	message      string
	hasMessage   bool
	dryRun       bool
	maxDiffBytes int
	remote       string
	force        bool
	title        string
	body         string
	fill         bool
	draft        bool
	yes          bool
	auto         bool
}

func runWorktrees(args []string, stdout io.Writer, stderr io.Writer, deps appDeps) int {
	command := "prepare"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		command = strings.ToLower(strings.TrimSpace(args[0]))
		args = args[1:]
	}
	if command == "help" {
		if err := writeWorktreesHelp(stdout); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	if command == "release" {
		return runWorktreesRelease(args, stdout, stderr, deps)
	}
	if command != "prepare" {
		return writeExecUsageError(stderr, fmt.Sprintf("unknown worktrees command %q", command))
	}
	options, help, err := parseWorktreeCommandArgs(args)
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	if help {
		if err := writeWorktreesHelp(stdout); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	workspaceRoot, err := resolveWorkspaceRoot(options.cwd, deps)
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	result, err := deps.prepareWorktree(context.Background(), worktrees.Options{
		Cwd:     workspaceRoot,
		Name:    options.name,
		BaseDir: options.baseDir,
		Now:     deps.now,
	})
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	safeResult := redactWorktreeResult(result)
	if options.json {
		if err := writePrettyJSON(stdout, safeResult); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	if _, err := fmt.Fprintln(stdout, formatWorktreeResult(safeResult)); err != nil {
		return exitCrash
	}
	return exitSuccess
}

// runWorktreesRelease unlocks a worktree `zero worktrees prepare` created, so
// Zero's automatic Clean pass can reclaim it once it goes stale. Prepare locks
// every worktree it creates and nothing else in that command's own lifetime
// unlocks it (its process exits right after printing the path, long before
// whatever external caller actually finishes using the worktree), so that
// caller is responsible for running this once it is done.
func runWorktreesRelease(args []string, stdout io.Writer, stderr io.Writer, deps appDeps) int {
	var path string
	var cwdFlag string
	for index := 0; index < len(args); index++ {
		arg := args[index]
		switch {
		case arg == "-h" || arg == "--help" || arg == "help":
			if err := writeWorktreesHelp(stdout); err != nil {
				return exitCrash
			}
			return exitSuccess
		case arg == "-C" || arg == "--cwd":
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return writeExecUsageError(stderr, err.Error())
			}
			cwdFlag = value
			index = next
		case strings.HasPrefix(arg, "--cwd="):
			cwdFlag = strings.TrimSpace(strings.TrimPrefix(arg, "--cwd="))
		case strings.HasPrefix(arg, "-"):
			return writeExecUsageError(stderr, fmt.Sprintf("unknown worktrees release flag %q", arg))
		default:
			if path != "" {
				return writeExecUsageError(stderr, "worktree path was provided more than once")
			}
			path = arg
		}
	}
	path = strings.TrimSpace(path)
	if path == "" {
		return writeExecUsageError(stderr, "worktrees release requires a worktree path")
	}
	// git worktree unlock matches against the path git recorded when the
	// worktree was created, so a relative argument here (resolved against
	// whatever directory the caller happens to be running in, not
	// necessarily the worktree's own) can fail to match. Resolve to absolute
	// up front so the unlock target is unambiguous regardless of cwd.
	absPath, err := filepath.Abs(path)
	if err != nil {
		return writeExecUsageError(stderr, redactCLIString(err.Error()))
	}
	// Release falls back to Options.Cwd as git's working directory when the
	// worktree directory itself was deleted by hand instead of released; with
	// no Cwd wired in, that recovery path runs `git worktree unlock` from
	// wherever the caller happens to be, which outside the source repository
	// leaves the orphaned lock behind forever (Clean skips locked entries).
	// -C/--cwd names the source repository explicitly for exactly that case,
	// since the deleted worktree path itself carries no way back to the repo;
	// without the flag, resolve the launch directory best-effort (the
	// ordinary existing-path case does not need it, so a resolution failure
	// is not an error here).
	releaseOptions := worktrees.Options{}
	if cwdFlag != "" {
		workspaceRoot, err := resolveWorkspaceRoot(cwdFlag, deps)
		if err != nil {
			return writeExecUsageError(stderr, redactCLIString(err.Error()))
		}
		releaseOptions.Cwd = workspaceRoot
	} else if workspaceRoot, rootErr := resolveWorkspaceRoot("", deps); rootErr == nil {
		releaseOptions.Cwd = workspaceRoot
	}
	if err := deps.releaseWorktree(context.Background(), releaseOptions, absPath); err != nil {
		return writeExecUsageError(stderr, redactCLIString(err.Error()))
	}
	// Print the absolute path that was released so relative arguments still
	// show an unambiguous target in the confirmation line.
	if _, err := fmt.Fprintf(stdout, "released %s\n", redactCLIString(absPath)); err != nil {
		return exitCrash
	}
	return exitSuccess
}

func runVerifyCommand(args []string, stdout io.Writer, stderr io.Writer, deps appDeps) int {
	options, help, err := parseVerifyCommandArgs(args)
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	if help {
		if err := writeVerifyHelp(stdout); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	workspaceRoot, err := resolveWorkspaceRoot(options.cwd, deps)
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	plan, err := deps.detectVerifyPlan(workspaceRoot)
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	if options.attempts > 1 {
		loopReport := deps.runSelfVerify(context.Background(), plan, selfverify.Options{
			RunOptions: verify.RunOptions{
				Only:      options.only,
				TimeoutMS: options.timeoutMS,
				Now:       deps.now,
			},
			MaxAttempts: options.attempts,
		})
		safeLoopReport := redactVerifyLoopReport(loopReport)
		if options.json {
			if err := writePrettyJSON(stdout, selfverify.SnapshotFromReport(safeLoopReport)); err != nil {
				return exitCrash
			}
		} else if _, err := fmt.Fprintln(stdout, formatVerifyLoopReport(safeLoopReport)); err != nil {
			return exitCrash
		}
		if !loopReport.OK {
			return exitProvider
		}
		return exitSuccess
	}
	report := deps.runVerify(context.Background(), plan, verify.RunOptions{
		Only:      options.only,
		TimeoutMS: options.timeoutMS,
		Now:       deps.now,
	})
	safeReport := redactVerifyReport(report)
	if options.json {
		if err := writePrettyJSON(stdout, verify.SnapshotFromReport(safeReport)); err != nil {
			return exitCrash
		}
	} else if _, err := fmt.Fprintln(stdout, formatVerifyReport(safeReport)); err != nil {
		return exitCrash
	}
	if !report.OK {
		return exitProvider
	}
	return exitSuccess
}

func runChanges(args []string, stdout io.Writer, stderr io.Writer, deps appDeps) int {
	command := "inspect"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		command = strings.ToLower(strings.TrimSpace(args[0]))
		args = args[1:]
	}
	if command == "help" {
		if err := writeChangesHelp(stdout); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	options, help, err := parseChangesArgs(args, command)
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	if help {
		if err := writeChangesHelp(stdout); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	workspaceRoot, err := resolveWorkspaceRoot(options.cwd, deps)
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	switch command {
	case "inspect", "status":
		summary, err := deps.inspectChanges(context.Background(), zerogit.InspectOptions{
			Cwd:          workspaceRoot,
			BaseRef:      options.baseRef,
			MaxDiffBytes: options.maxDiffBytes,
		})
		if err != nil {
			return writeExecUsageError(stderr, err.Error())
		}
		safeSummary := redactChangeSummary(summary)
		if options.json {
			if err := writePrettyJSON(stdout, zerogit.SnapshotFromSummary(safeSummary)); err != nil {
				return exitCrash
			}
			return exitSuccess
		}
		if _, err := fmt.Fprintln(stdout, formatChangeSummary(safeSummary)); err != nil {
			return exitCrash
		}
		return exitSuccess
	case "commit":
		var message string
		if options.auto {
			summary, err := deps.inspectChanges(context.Background(), zerogit.InspectOptions{
				Cwd:          workspaceRoot,
				MaxDiffBytes: options.maxDiffBytes,
			})
			if err != nil {
				return writeExecUsageError(stderr, fmt.Sprintf("failed to inspect changes: %v", err))
			}
			if summary.Clean {
				return writeExecUsageError(stderr, "no changes to commit")
			}

			resolved, err := deps.resolveConfig(workspaceRoot, config.Overrides{})
			if err != nil {
				return writeExecUsageError(stderr, fmt.Sprintf("failed to resolve config: %v", err))
			}
			if !config.HasProviderProfile(resolved.Provider) {
				return writeExecUsageError(stderr, "no provider configured for auto-commit message")
			}
			provider, err := deps.newProvider(resolved.Provider)
			if err != nil {
				return writeExecUsageError(stderr, fmt.Sprintf("failed to create provider: %v", err))
			}

			if !options.json {
				if _, err := fmt.Fprintln(stdout, "Generating commit message using LLM..."); err != nil {
					return exitCrash
				}
			}

			safeSummary := redactChangeSummary(summary)
			genCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			msg, err := generateAutoCommitMessage(genCtx, provider, resolved.Provider.Model, safeSummary)
			if err != nil {
				return writeExecUsageError(stderr, fmt.Sprintf("failed to generate commit message: %v", err))
			}
			message = msg
		} else {
			message = options.message
		}

		result, err := deps.commitChanges(context.Background(), zerogit.CommitOptions{
			Cwd:          workspaceRoot,
			Message:      message,
			DryRun:       options.dryRun,
			MaxDiffBytes: options.maxDiffBytes,
		})
		if err != nil {
			return writeExecUsageError(stderr, err.Error())
		}
		safeResult := redactCommitResult(result)
		if options.json {
			if err := writePrettyJSON(stdout, safeResult); err != nil {
				return exitCrash
			}
			return exitSuccess
		}
		if _, err := fmt.Fprintln(stdout, formatCommitResult(safeResult)); err != nil {
			return exitCrash
		}
		return exitSuccess
	case "push":
		return runChangesPush(args, stdout, stderr, deps)
	case "pr", "pull-request":
		return runChangesPR(args, stdout, stderr, deps)
	default:
		return writeExecUsageError(stderr, fmt.Sprintf("unknown changes command %q", command))
	}
}

func parseWorktreeCommandArgs(args []string) (worktreeCommandOptions, bool, error) {
	options := worktreeCommandOptions{}
	for index := 0; index < len(args); index++ {
		arg := args[index]
		switch {
		case arg == "-h" || arg == "--help" || arg == "help":
			return options, true, nil
		case arg == "--json":
			options.json = true
		case arg == "--name":
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return options, false, err
			}
			if err := setWorktreeName(&options, value); err != nil {
				return options, false, err
			}
			index = next
		case strings.HasPrefix(arg, "--name="):
			if err := setWorktreeName(&options, strings.TrimPrefix(arg, "--name=")); err != nil {
				return options, false, err
			}
		case arg == "--dir":
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return options, false, err
			}
			options.baseDir = value
			index = next
		case strings.HasPrefix(arg, "--dir="):
			options.baseDir = strings.TrimSpace(strings.TrimPrefix(arg, "--dir="))
		case arg == "-C" || arg == "--cwd":
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return options, false, err
			}
			options.cwd = value
			index = next
		case strings.HasPrefix(arg, "--cwd="):
			options.cwd = strings.TrimSpace(strings.TrimPrefix(arg, "--cwd="))
		case strings.HasPrefix(arg, "-"):
			return options, false, execUsageError{fmt.Sprintf("unknown worktrees flag %q", arg)}
		default:
			if err := setWorktreeName(&options, arg); err != nil {
				return options, false, err
			}
		}
	}
	return options, false, nil
}

func setWorktreeName(options *worktreeCommandOptions, value string) error {
	name := strings.TrimSpace(value)
	if name == "" {
		return nil
	}
	if options.name != "" {
		return execUsageError{"worktree name was provided more than once"}
	}
	options.name = name
	return nil
}

func parseVerifyCommandArgs(args []string) (verifyCommandOptions, bool, error) {
	options := verifyCommandOptions{}
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
		case arg == "--only":
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return options, false, err
			}
			options.only = append(options.only, parseToolList(value)...)
			index = next
		case strings.HasPrefix(arg, "--only="):
			options.only = append(options.only, parseToolList(strings.TrimSpace(strings.TrimPrefix(arg, "--only=")))...)
		case arg == "--timeout-ms":
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return options, false, err
			}
			timeoutMS, err := parsePositiveIntFlag("--timeout-ms", value)
			if err != nil {
				return options, false, err
			}
			options.timeoutMS = timeoutMS
			index = next
		case strings.HasPrefix(arg, "--timeout-ms="):
			timeoutMS, err := parsePositiveIntFlag("--timeout-ms", strings.TrimSpace(strings.TrimPrefix(arg, "--timeout-ms=")))
			if err != nil {
				return options, false, err
			}
			options.timeoutMS = timeoutMS
		case arg == "--attempts":
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return options, false, err
			}
			attempts, err := parsePositiveIntFlag("--attempts", value)
			if err != nil {
				return options, false, err
			}
			options.attempts = attempts
			index = next
		case strings.HasPrefix(arg, "--attempts="):
			attempts, err := parsePositiveIntFlag("--attempts", strings.TrimSpace(strings.TrimPrefix(arg, "--attempts=")))
			if err != nil {
				return options, false, err
			}
			options.attempts = attempts
		case strings.HasPrefix(arg, "-"):
			return options, false, execUsageError{fmt.Sprintf("unknown verify flag %q", arg)}
		default:
			return options, false, execUsageError{fmt.Sprintf("unexpected verify argument %q", arg)}
		}
	}
	return options, false, nil
}

func parseChangesArgs(args []string, command string) (changesCommandOptions, bool, error) {
	options := changesCommandOptions{}
	for index := 0; index < len(args); index++ {
		arg := args[index]
		switch {
		case arg == "-h" || arg == "--help" || arg == "help":
			return options, true, nil
		case arg == "--json":
			options.json = true
		case arg == "--dry-run":
			options.dryRun = true
		case arg == "-C" || arg == "--cwd":
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return options, false, err
			}
			options.cwd = value
			index = next
		case strings.HasPrefix(arg, "--cwd="):
			options.cwd = strings.TrimSpace(strings.TrimPrefix(arg, "--cwd="))
		case arg == "--base":
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return options, false, err
			}
			options.baseRef = strings.TrimSpace(value)
			index = next
		case strings.HasPrefix(arg, "--base="):
			v := strings.TrimSpace(strings.TrimPrefix(arg, "--base="))
			if v == "" || flagValueLooksLikeOption(v) {
				return options, false, execUsageError{"--base requires a value"}
			}
			options.baseRef = v
		case arg == "-m" || arg == "--message":
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return options, false, err
			}
			options.message = value
			options.hasMessage = true
			index = next
		case strings.HasPrefix(arg, "--message="):
			options.message = strings.TrimSpace(strings.TrimPrefix(arg, "--message="))
			options.hasMessage = true
		case arg == "--diff-bytes":
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return options, false, err
			}
			maxDiffBytes, err := parsePositiveIntFlag("--diff-bytes", value)
			if err != nil {
				return options, false, err
			}
			options.maxDiffBytes = maxDiffBytes
			index = next
		case strings.HasPrefix(arg, "--diff-bytes="):
			maxDiffBytes, err := parsePositiveIntFlag("--diff-bytes", strings.TrimSpace(strings.TrimPrefix(arg, "--diff-bytes=")))
			if err != nil {
				return options, false, err
			}
			options.maxDiffBytes = maxDiffBytes
		case arg == "--remote":
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return options, false, err
			}
			options.remote = strings.TrimSpace(value)
			index = next
		case strings.HasPrefix(arg, "--remote="):
			options.remote = strings.TrimSpace(strings.TrimPrefix(arg, "--remote="))
		case arg == "--force":
			options.force = true
		case arg == "--title":
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return options, false, err
			}
			options.title = value
			index = next
		case strings.HasPrefix(arg, "--title="):
			options.title = strings.TrimPrefix(arg, "--title=")
		case arg == "--body":
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return options, false, err
			}
			options.body = value
			index = next
		case strings.HasPrefix(arg, "--body="):
			options.body = strings.TrimPrefix(arg, "--body=")
		case arg == "--fill":
			options.fill = true
		case arg == "--draft":
			options.draft = true
		case arg == "--yes":
			options.yes = true
		case arg == "-a" || arg == "--auto":
			options.auto = true
		case strings.HasPrefix(arg, "-"):
			return options, false, execUsageError{fmt.Sprintf("unknown changes flag %q", arg)}
		default:
			return options, false, execUsageError{fmt.Sprintf("unexpected changes argument %q", arg)}
		}
	}
	if command != "commit" && options.hasMessage {
		return options, false, execUsageError{"--message is only valid with `zero changes commit`"}
	}
	if command == "commit" && options.hasMessage && options.auto {
		return options, false, execUsageError{"cannot specify both --message and --auto"}
	}
	if command != "commit" && command != "push" && options.dryRun {
		return options, false, execUsageError{"--dry-run is only valid with commit or push"}
	}
	// --auto on push/pr is the explicit opt-in for LLM branch naming (see
	// ensureFeatureBranch); on commit it opts into the LLM commit message.
	if command != "commit" && command != "push" && command != "pr" && options.auto {
		return options, false, execUsageError{"--auto is only valid with commit, push, or pr"}
	}
	if command != "inspect" && options.baseRef != "" {
		return options, false, execUsageError{"--base is only valid with `zero changes inspect`"}
	}
	if command != "push" && command != "pr" && (options.remote != "" || options.force) {
		return options, false, execUsageError{"--remote and --force are only valid with push or pr"}
	}
	if command != "pr" && (options.title != "" || options.body != "" || options.fill || options.draft) {
		return options, false, execUsageError{"--title, --body, --fill, and --draft are only valid with `zero changes pr`"}
	}
	if command != "push" && command != "pr" && options.yes {
		return options, false, execUsageError{"--yes is only valid with push or pr"}
	}
	return options, false, nil
}

func parsePositiveIntFlag(flag string, value string) (int, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return 0, execUsageError{flag + " requires a value"}
	}
	number, err := strconv.Atoi(trimmed)
	if err != nil || number <= 0 {
		return 0, execUsageError{fmt.Sprintf("invalid %s %q. Expected a positive integer.", flag, value)}
	}
	return number, nil
}

func redactWorktreeResult(result worktrees.Result) worktrees.Result {
	result.Name = redactCLIString(result.Name)
	result.Path = redactCLIString(result.Path)
	result.RepoRoot = redactCLIString(result.RepoRoot)
	result.SourceBranch = redactCLIString(result.SourceBranch)
	result.SourceCommit = redactCLIString(result.SourceCommit)
	return result
}

func redactVerifyReport(report verify.Report) verify.Report {
	report.Root = redactCLIString(report.Root)
	report.Results = append([]verify.Result{}, report.Results...)
	for index := range report.Results {
		report.Results[index].Stdout = redactCLIString(report.Results[index].Stdout)
		report.Results[index].Stderr = redactCLIString(report.Results[index].Stderr)
		report.Results[index].Error = redactCLIString(report.Results[index].Error)
		if report.Results[index].OutputSummary != nil {
			summary := *report.Results[index].OutputSummary
			summary.Lines = append([]string{}, summary.Lines...)
			for lineIndex := range summary.Lines {
				summary.Lines[lineIndex] = redactCLIString(summary.Lines[lineIndex])
			}
			report.Results[index].OutputSummary = &summary
		}
		if report.Results[index].TestSummary != nil {
			summary := *report.Results[index].TestSummary
			summary.Failures = append([]testrunner.Failure{}, summary.Failures...)
			for failureIndex := range summary.Failures {
				summary.Failures[failureIndex].Name = redactCLIString(summary.Failures[failureIndex].Name)
				summary.Failures[failureIndex].File = redactCLIString(summary.Failures[failureIndex].File)
				summary.Failures[failureIndex].Message = redactCLIString(summary.Failures[failureIndex].Message)
			}
			report.Results[index].TestSummary = &summary
		}
	}
	return report
}

func redactVerifyLoopReport(report selfverify.Report) selfverify.Report {
	report.Root = redactCLIString(report.Root)
	report.Error = redactCLIString(report.Error)
	for index := range report.Attempts {
		report.Attempts[index].Report = redactVerifyReport(report.Attempts[index].Report)
		if report.Attempts[index].Remediation != nil {
			remediation := *report.Attempts[index].Remediation
			remediation.StartedAt = redactCLIString(remediation.StartedAt)
			remediation.EndedAt = redactCLIString(remediation.EndedAt)
			remediation.Message = redactCLIString(remediation.Message)
			remediation.Error = redactCLIString(remediation.Error)
			report.Attempts[index].Remediation = &remediation
		}
	}
	return report
}

func redactChangeSummary(summary zerogit.ChangeSummary) zerogit.ChangeSummary {
	summary.Root = redactCLIString(summary.Root)
	summary.Base = redactCLIString(summary.Base)
	summary.Branch = redactCLIString(summary.Branch)
	summary.Commit = redactCLIString(summary.Commit)
	summary.DiffStat = redactCLIString(summary.DiffStat)
	summary.Diff = redactCLIString(summary.Diff)
	for index := range summary.Files {
		summary.Files[index].Path = redactCLIString(summary.Files[index].Path)
		summary.Files[index].Status = redactCLIString(summary.Files[index].Status)
	}
	return summary
}

func redactCommitResult(result zerogit.CommitResult) zerogit.CommitResult {
	result.Root = redactCLIString(result.Root)
	result.Message = redactCLIString(result.Message)
	result.CommitHash = redactCLIString(result.CommitHash)
	result.Before = redactChangeSummary(result.Before)
	return result
}

func redactCLIString(value string) string {
	// Keep ordinary paths visible; these commands report useful locations.
	// Central redaction still removes secret-looking tokens embedded in paths.
	return redaction.RedactString(value, redaction.Options{})
}

func formatWorktreeResult(result worktrees.Result) string {
	lines := []string{
		"Zero worktree ready",
		"name: " + result.Name,
		"path: " + result.Path,
		"repo: " + result.RepoRoot,
	}
	if result.SourceBranch != "" {
		lines = append(lines, "branch: "+result.SourceBranch)
	}
	if result.SourceCommit != "" {
		lines = append(lines, "commit: "+result.SourceCommit)
	}
	if result.Reused {
		lines = append(lines, "reused: true")
	}
	return strings.Join(lines, "\n")
}

func formatVerifyReport(report verify.Report) string {
	lines := []string{
		"Zero verification",
		"root: " + report.Root,
		fmt.Sprintf("summary: %d total, %d passed, %d failed, %d errors", report.Summary.Total, report.Summary.Passed, report.Summary.Failed, report.Summary.Errors),
	}
	if len(report.Results) == 0 {
		lines = append(lines, "  (no checks detected)")
		return strings.Join(lines, "\n")
	}
	for _, result := range report.Results {
		lines = append(lines, fmt.Sprintf("  [%s] %s - %s", result.Status, result.ID, strings.Join(result.Command, " ")))
		if result.TestSummary != nil {
			lines = append(lines, formatVerifyTestSummary(result.TestSummary))
			for _, failure := range result.TestSummary.Failures {
				if failure.Name == "" {
					continue
				}
				detail := failure.Name
				if failure.File != "" {
					detail += " at " + failure.File
				}
				lines = append(lines, "    failure: "+detail)
			}
		}
		if result.Error != "" {
			lines = append(lines, "    error: "+result.Error)
		}
	}
	return strings.Join(lines, "\n")
}

func formatVerifyTestSummary(summary *testrunner.Summary) string {
	line := fmt.Sprintf("    tests: %d total, %d passed, %d failed", summary.Total, summary.Passed, summary.Failed)
	if summary.Skipped > 0 {
		line += fmt.Sprintf(", %d skipped", summary.Skipped)
	}
	return line
}

func formatVerifyLoopReport(report selfverify.Report) string {
	lines := []string{
		"Zero self-verification",
	}
	if report.Root != "" {
		lines = append(lines, "root: "+report.Root)
	}
	lines = append(lines,
		fmt.Sprintf("attempts: %d", len(report.Attempts)),
		"stop: "+string(report.StopReason),
		fmt.Sprintf("summary: %d total, %d passed, %d failed, %d errors", report.Summary.Total, report.Summary.Passed, report.Summary.Failed, report.Summary.Errors),
	)
	for _, attempt := range report.Attempts {
		status := "failed"
		if attempt.Report.OK {
			status = "passed"
		}
		lines = append(lines, fmt.Sprintf("  attempt %d: %s", attempt.Number, status))
		if attempt.Remediation != nil {
			lines = append(lines, "    remediation: "+formatRemediation(*attempt.Remediation))
		}
	}
	if report.Error != "" {
		lines = append(lines, "error: "+report.Error)
	}
	return strings.Join(lines, "\n")
}

func formatRemediation(remediation selfverify.Remediation) string {
	status := "not applied"
	if remediation.Applied {
		status = "applied"
	}
	details := []string{status}
	if remediation.Message != "" {
		details = append(details, remediation.Message)
	}
	if remediation.Error != "" {
		details = append(details, "error: "+remediation.Error)
	}
	return strings.Join(details, " - ")
}

func formatChangeSummary(summary zerogit.ChangeSummary) string {
	lines := []string{
		"Zero changes",
		"root: " + summary.Root,
		fmt.Sprintf("files: %d changed", len(summary.Files)),
	}
	if summary.Branch != "" {
		lines = append(lines, "branch: "+summary.Branch)
	}
	if summary.Base != "" {
		lines = append(lines, "base: "+summary.Base)
	}
	if summary.Commit != "" {
		lines = append(lines, "commit: "+summary.Commit)
	}
	if summary.Clean {
		lines = append(lines, "clean: true")
		return strings.Join(lines, "\n")
	}
	for _, file := range summary.Files {
		lines = append(lines, fmt.Sprintf("  [%s] %s", file.Status, file.Path))
	}
	return strings.Join(lines, "\n")
}

func formatCommitResult(result zerogit.CommitResult) string {
	lines := []string{
		"Zero changes commit",
		"root: " + result.Root,
		"message: " + result.Message,
		fmt.Sprintf("dry-run: %t", result.DryRun),
		fmt.Sprintf("committed: %t", result.Committed),
		fmt.Sprintf("files: %d changed", len(result.Before.Files)),
	}
	if result.CommitHash != "" {
		lines = append(lines, "commit: "+result.CommitHash)
	}
	return strings.Join(lines, "\n")
}

func writeWorktreesHelp(w io.Writer) error {
	_, err := fmt.Fprint(w, `Usage:
  zero worktrees prepare [flags] [name]
  zero worktrees release [flags] <path>

Prepares an isolated git worktree for a Zero task.

prepare locks the worktree it creates so Zero's automatic cleanup never
removes it out from under you. release unlocks a worktree prepare created,
once you are done with it, so cleanup can reclaim it later if it goes stale.
If the worktree directory was deleted by hand, run release with -C pointing
at the source repository so the orphaned lock can still be cleared.

prepare flags:
      --name <name>       Worktree name; defaults to a timestamped task name
      --dir <path>        Base directory for Zero worktrees
  -C, --cwd <path>        Source repository directory
      --json              Print JSON output
  -h, --help              Show this help

release flags:
  -C, --cwd <path>        Source repository directory; needed when the
                           worktree directory was already deleted and you
                           are not running inside the source repository
  -h, --help              Show this help
`)
	return err
}

func writeVerifyHelp(w io.Writer) error {
	_, err := fmt.Fprint(w, `Usage:
  zero verify [flags]

Detects and runs local verification checks for the workspace.

Flags:
  -C, --cwd <path>        Workspace directory
      --only <ids>        Run only matching check ids
      --timeout-ms <n>    Per-check timeout in milliseconds
      --attempts <n>      Run a bounded self-verification loop
      --json              Print JSON output
  -h, --help              Show this help
`)
	return err
}

func writeChangesHelp(w io.Writer) error {
	_, err := fmt.Fprint(w, `Usage:
  zero changes inspect [flags]
  zero changes commit [flags]
  zero changes push [flags]
  zero changes pr [flags]

Inspects, commits, pushes, and creates pull requests for local git changes.

Flags:
  -C, --cwd <path>        Workspace directory
      --base <ref>        Diff against <ref>...HEAD instead of the working tree
      --diff-bytes <n>    Maximum diff bytes to include
  -m, --message <text>    Commit message for `+"`zero changes commit`"+`
      --dry-run           Preview commit metadata / push without mutating git state
      --remote <name>     Remote to push to (defaults to upstream tracked branch or origin)
      --force             Use force-with-lease when pushing
      --title <text>      PR title
      --body <text>       PR body
      --fill              Automatically populate PR title and body from commits
      --draft             Create PR as a draft
      --yes               Confirm pushing to a default/protected branch
  -a, --auto              Use the LLM: commit generates the message, push/pr name the auto-created branch (sends the diff to the provider)
      --json              Print JSON output
  -h, --help              Show this help
`)
	return err
}

func runChangesPush(args []string, stdout io.Writer, stderr io.Writer, deps appDeps) int {
	options, help, err := parseChangesArgs(args, "push")
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	if help {
		if err := writeChangesHelp(stdout); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	workspaceRoot, err := resolveWorkspaceRoot(options.cwd, deps)
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	branch, remote, created, err := ensureFeatureBranch(ctx, stdout, workspaceRoot, options.remote, featureBranchOptions{
		JSONMode:           options.json,
		AllowDefaultBranch: options.yes,
		DryRun:             options.dryRun,
		AutoNaming:         options.auto,
		MaxDiffBytes:       options.maxDiffBytes,
	}, deps)
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}

	// context.Background() is deliberate: the 2-minute ctx above is the
	// feature-branch preflight (inspection, remote probes, branch creation).
	// The push itself must not be cancelled mid-transfer just because the
	// preflight budget elapsed; a large push can legitimately take longer.
	result, err := deps.pushChanges(context.Background(), zerogit.PushOptions{
		Cwd:                    workspaceRoot,
		Remote:                 firstNonEmptyString(options.remote, remote),
		Branch:                 branch,
		Force:                  options.force,
		DryRun:                 options.dryRun,
		RequireNewRemoteBranch: created,
		AllowPushDefaultBranch: options.yes,
	})
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}

	if options.json {
		if err := writePrettyJSON(stdout, result); err != nil {
			return exitCrash
		}
		return exitSuccess
	}

	dryRunStr := ""
	if options.dryRun {
		dryRunStr = " (dry run)"
	}
	if _, err := fmt.Fprintf(stdout, "Pushed branch %s to remote %s%s\n", result.Branch, result.Remote, dryRunStr); err != nil {
		return exitCrash
	}
	if result.Output != "" {
		if _, err := fmt.Fprintln(stdout, result.Output); err != nil {
			return exitCrash
		}
	}
	return exitSuccess
}

func runChangesPR(args []string, stdout io.Writer, stderr io.Writer, deps appDeps) int {
	options, help, err := parseChangesArgs(args, "pr")
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	if help {
		if err := writeChangesHelp(stdout); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	if !options.fill && options.title == "" {
		return writeExecUsageError(stderr, "must provide either --fill or --title to run non-interactively")
	}
	workspaceRoot, err := resolveWorkspaceRoot(options.cwd, deps)
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	branch, remote, created, err := ensureFeatureBranch(ctx, stdout, workspaceRoot, options.remote, featureBranchOptions{
		JSONMode:           options.json,
		AllowDefaultBranch: options.yes,
		DryRun:             false,
		AutoNaming:         options.auto,
		MaxDiffBytes:       options.maxDiffBytes,
	}, deps)
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}

	// Resolve the target remote for the unborn-remote check. Explicit --remote wins,
	// followed by the remote resolved by ensureFeatureBranch. When --yes is set,
	// fall back to the branch upstream or "origin".
	targetRemote := strings.TrimSpace(options.remote)
	if targetRemote == "" {
		targetRemote = remote
	}
	if targetRemote == "" {
		currentBranch := ""
		if deps.currentGitBranch != nil {
			currentBranch = deps.currentGitBranch(context.Background(), workspaceRoot)
		}
		if deps.branchUpstreamRemote != nil && currentBranch != "" {
			targetRemote = deps.branchUpstreamRemote(context.Background(), workspaceRoot, currentBranch)
		}
		if targetRemote == "" {
			targetRemote = "origin"
		}
	}

	// Probe errors are intentionally ignored so the flow can continue; if the
	// probe fails, the subsequent push or PR operation provides fail-closed behavior.
	if deps.isUnbornRemote != nil {
		if unborn, unbornErr := deps.isUnbornRemote(context.Background(), workspaceRoot, targetRemote); unbornErr == nil && unborn {
			return writeExecUsageError(stderr, fmt.Sprintf("cannot create pull request on unborn remote %s: push the initial default branch first", targetRemote))
		}
	}

	if !options.json {
		if _, err := fmt.Fprintln(stdout, "Pushing current branch to set upstream..."); err != nil {
			return exitCrash
		}
	}
	// context.Background() is deliberate here too: runChangesPR reuses the
	// 2-minute ctx for preflight (branch creation and remote probes), but the
	// push must not be cancelled mid-transfer if that budget elapsed.
	pushResult, err := deps.pushChanges(context.Background(), zerogit.PushOptions{
		Cwd:                    workspaceRoot,
		Remote:                 firstNonEmptyString(options.remote, remote),
		Branch:                 branch,
		Force:                  options.force,
		RequireNewRemoteBranch: created,
		AllowPushDefaultBranch: options.yes,
	})
	if err != nil {
		return writeExecUsageError(stderr, fmt.Sprintf("auto-push failed: %v", err))
	}
	if !options.json {
		if _, err := fmt.Fprintf(stdout, "Pushed branch %s to remote %s\n", pushResult.Branch, pushResult.Remote); err != nil {
			return exitCrash
		}
	}

	prResult, err := deps.createPR(context.Background(), zerogit.PROptions{
		Cwd:   workspaceRoot,
		Fill:  options.fill,
		Draft: options.draft,
		Title: options.title,
		Body:  options.body,
	})
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}

	if options.json {
		type prJSONResult struct {
			Branch string `json:"branch"`
			Remote string `json:"remote"`
			Output string `json:"output"`
		}
		res := prJSONResult{
			Branch: pushResult.Branch,
			Remote: pushResult.Remote,
			Output: strings.TrimSpace(prResult.Output),
		}
		if err := writePrettyJSON(stdout, res); err != nil {
			return exitCrash
		}
		return exitSuccess
	}

	if _, err := fmt.Fprintln(stdout, strings.TrimSpace(prResult.Output)); err != nil {
		return exitCrash
	}
	return exitSuccess
}

func generateAutoCommitMessage(ctx context.Context, provider zeroruntime.Provider, model string, summary zerogit.ChangeSummary) (string, error) {
	var promptBuilder strings.Builder
	promptBuilder.WriteString("Analyze the following git diff and generate a concise, conventional commit message.\n")
	promptBuilder.WriteString("The commit message subject line must be 72 characters or fewer, starting with a conventional commit type (e.g., feat, fix, docs, style, refactor, test, chore) followed by a colon and space, and a lowercase description.\n")
	promptBuilder.WriteString("You may optionally include a blank line and a bulleted list of changes for the body if there are multiple files or complex changes.\n")
	promptBuilder.WriteString("Output ONLY the raw commit message text. Do not wrap the message in markdown code block fence, do not include quotes or any introduction/explanation.\n\n")
	promptBuilder.WriteString("Git Diff:\n")
	promptBuilder.WriteString(summary.Diff)

	request := zeroruntime.CompletionRequest{
		Messages: []zeroruntime.Message{
			{Role: zeroruntime.MessageRoleUser, Content: promptBuilder.String()},
		},
	}
	stream, err := provider.StreamCompletion(ctx, request)
	if err != nil {
		return "", err
	}
	collected := zeroruntime.CollectStream(ctx, stream)
	if collected.Error != "" {
		return "", fmt.Errorf("%s", collected.Error)
	}

	msg := strings.TrimSpace(collected.Text)
	if strings.HasPrefix(msg, "```") {
		if idx := strings.Index(msg, "\n"); idx != -1 {
			msg = msg[idx+1:]
		}
	}
	msg = strings.TrimSuffix(msg, "```")
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return "", fmt.Errorf("provider returned empty commit message")
	}
	return msg, nil
}

type featureBranchOptions struct {
	JSONMode           bool
	AllowDefaultBranch bool
	DryRun             bool
	AutoNaming         bool
	MaxDiffBytes       int
}

// branchPushPlan holds the resolved parameters for a push or PR workflow
// before any branches are created or refs mutated. Separating plan resolution
// from mutation allows tests to assert the resolution matrix directly without
// touching a git repository.
type branchPushPlan struct {
	ShouldBranch           bool
	CurrentBranch          string
	PushRemote             string
	RequireNewRemoteBranch bool
	TrackingRemote         string
	TrackingBranch         string
	RestoreTip             string
	PublishabilityBase     string
	Summary                zerogit.ChangeSummary
	Slug                   string
}

// resolveBranchPushPlan resolves the branch, remotes, restore tip, and naming
// slug for changes push / pr workflows. It performs all checks (clean working
// tree, unborn remote detection, ahead count, restore tip resolution) but
// executes no mutation (no branch creation, ref resets, or deletions).
func resolveBranchPushPlan(ctx context.Context, stdout io.Writer, workspaceRoot string, requestedRemote string, opts featureBranchOptions, deps appDeps) (branchPushPlan, error) {
	if opts.AllowDefaultBranch || opts.DryRun {
		return branchPushPlan{
			ShouldBranch:           false,
			PushRemote:             strings.TrimSpace(requestedRemote),
			RequireNewRemoteBranch: false,
		}, nil
	}

	if deps.isDefaultBranch == nil {
		return branchPushPlan{}, fmt.Errorf("isDefaultBranch dependency missing")
	}
	isDefault, currentBranch, remote, err := deps.isDefaultBranch(ctx, zerogit.DefaultBranchOptions{Cwd: workspaceRoot, Remote: requestedRemote})
	if err != nil {
		return branchPushPlan{}, err
	}
	if !isDefault {
		// Whether this push needs the nonexistence lease is decided by one
		// live check against the destination remote, not by inference from
		// local git state (upstream config, a generated-branch marker):
		// every local signal that stood in for that check turned out to be
		// wrong in some ordering (inherited config from checkout -b, a
		// config write that failed after a successful push, a push aimed at
		// a different remote than the one local config describes). A branch
		// missing on the remote gets the nonexistence lease, protecting a
		// concurrent creator of the same name; a branch already there gets a
		// plain push, and git's own fast-forward check is the safety net —
		// if the remote history isn't ours, the push fails with a clear
		// non-fast-forward error instead of an attempted silent recovery.
		if deps.remoteHasBranch == nil {
			return branchPushPlan{
				ShouldBranch:           false,
				CurrentBranch:          currentBranch,
				PushRemote:             remote,
				RequireNewRemoteBranch: true,
			}, nil
		}
		targetRemote := firstNonEmptyString(requestedRemote, remote)
		exists, existsErr := deps.remoteHasBranch(ctx, workspaceRoot, targetRemote, currentBranch)
		if existsErr != nil {
			return branchPushPlan{}, fmt.Errorf("cannot check whether %s already exists on remote %s: %w", currentBranch, targetRemote, existsErr)
		}
		return branchPushPlan{
			ShouldBranch:           false,
			CurrentBranch:          currentBranch,
			PushRemote:             remote,
			RequireNewRemoteBranch: !exists,
		}, nil
	}

	// CreateBranch and Push publish commits only. A dirty working tree would
	// leave uncommitted edits behind under a branch/PR that does not include
	// them, so refuse until the tree is clean (commit or stash first).
	workingTree, err := deps.inspectChanges(ctx, zerogit.InspectOptions{Cwd: workspaceRoot})
	if err != nil {
		return branchPushPlan{}, fmt.Errorf("failed to inspect working tree: %w", err)
	}
	if !workingTree.Clean {
		return branchPushPlan{}, fmt.Errorf("working tree has uncommitted changes; commit or stash them before pushing from the default branch")
	}

	// Capture the source default branch's own upstream before refreshing or
	// leaving it. --remote selects the push destination; restore must not
	// rewrite local main to upstream/main when main still tracks origin/main
	// (fork setups), and refreshing must update the exact tracking ref that
	// will supply both the ahead count and the restore target.
	trackingRemote := remote
	trackingBranch := currentBranch
	if deps.branchUpstreamRemoteAndMerge != nil {
		if r, m := deps.branchUpstreamRemoteAndMerge(ctx, workspaceRoot, currentBranch); r != "" && m != "" {
			trackingRemote = r
			trackingBranch = m
		}
	}

	// Refresh the local remote-tracking ref before trusting it: IsDefaultBranch
	// already contacted the remote for its symref check, but the tracking ref
	// commitsAhead reads from is only a local cache (written at clone or the
	// last fetch) and can sit behind the remote's live tip. Left stale, a real
	// publishable range could look like zero commits ahead, and the diff
	// derived below (which reuses this same ref as its base) would be stale
	// too. A failure here (offline, or a genuinely unborn remote with no such
	// ref to fetch yet) is folded into the same fail-closed path as an
	// unresolvable ahead count below.
	var fetchErr error
	if deps.refreshTrackingRef != nil {
		fetchErr = deps.refreshTrackingRef(ctx, workspaceRoot, trackingRemote, trackingBranch)
	}

	// The ahead check gates on the source upstream (trackingRemote), but the
	// feature branch is pushed to `pushRemote`, which can be a different
	// destination (--remote on a fork). Refuse to auto-branch onto an unborn
	// destination: without an initial default branch the new feature branch
	// becomes the destination's first ref and its HEAD is left pointing at it.
	// This is checked once, here, against the push destination, before resolving
	// commit tips or creating any branches.
	pushRemote := firstNonEmptyString(requestedRemote, remote)
	if deps.isUnbornRemote != nil {
		if unbornDest, unbornDestErr := deps.isUnbornRemote(ctx, workspaceRoot, pushRemote); unbornDestErr == nil && unbornDest {
			return branchPushPlan{}, fmt.Errorf("remote %s has no branches yet; push the initial default branch first with --yes (`zero changes push --yes`), then use auto-branching for subsequent work", pushRemote)
		}
	}

	// Branching off the default branch only makes sense when HEAD carries a
	// commit that is not already on the remote default branch. A clean,
	// up-to-date default branch would otherwise publish a feature branch at the
	// exact default tip. If the ahead count cannot be determined (for example
	// the remote-tracking ref was never fetched), fail rather than guess. When
	// the remote is confirmed unborn, refuse auto-branching entirely: the
	// initial default branch must be established first (changes push --yes)
	// so the remote has a real HEAD before any feature branch is published.
	ahead, aheadErr := deps.commitsAhead(ctx, workspaceRoot, trackingRemote, trackingBranch)
	if fetchErr != nil || aheadErr != nil {
		var unbornRemote bool
		var unbornErr error
		if deps.isUnbornRemote != nil {
			unbornRemote, unbornErr = deps.isUnbornRemote(ctx, workspaceRoot, trackingRemote)
		}
		if unbornErr == nil && unbornRemote {
			return branchPushPlan{}, fmt.Errorf("remote %s has no branches yet; push the initial default branch first with --yes (`zero changes push --yes`), then use auto-branching for subsequent work", trackingRemote)
		}
		cause := aheadErr
		if cause == nil {
			cause = fetchErr
		}
		return branchPushPlan{}, fmt.Errorf("cannot determine whether HEAD is ahead of %s/%s: %w; fetch the remote tracking branch first", trackingRemote, trackingBranch, cause)
	}
	if ahead == 0 {
		return branchPushPlan{}, fmt.Errorf("no changes to publish: HEAD is not ahead of %s/%s; commit your work before pushing", trackingRemote, trackingBranch)
	}

	// Resolve the restore tip to a concrete commit before creating any branch.
	// It must never be a bare local branch name: resolving "main" yields the
	// very commit the feature branch is about to own, so restore becomes a
	// silent no-op and the publishable commits stay on the default branch. A
	// named remote resolves through its just-refreshed remote-tracking ref; a
	// direct URL or path asks the remote itself. Fail closed rather than
	// substitute a local branch name.
	restoreTip := ""
	if deps.resolveRemoteBranchTip != nil {
		tip, tipErr := deps.resolveRemoteBranchTip(ctx, workspaceRoot, trackingRemote, trackingBranch)
		if tipErr != nil {
			return branchPushPlan{}, fmt.Errorf("cannot resolve restore tip for %s/%s: %w", trackingRemote, trackingBranch, tipErr)
		}
		restoreTip = tip
	}
	if restoreTip == "" {
		restoreTip = trackingRemote + "/" + trackingBranch
	}

	// Name the branch (and, with --auto, send the provider) from what HEAD is
	// actually ahead of the resolved remote branch by, using the same ref
	// commitsAhead just checked. A working-tree snapshot can describe edits a
	// commit-only push will never include.
	baseRef := restoreTip
	summary, err := deps.inspectChanges(ctx, zerogit.InspectOptions{Cwd: workspaceRoot, BaseRef: baseRef, MaxDiffBytes: opts.MaxDiffBytes})
	if err != nil {
		return branchPushPlan{}, fmt.Errorf("failed to inspect changes: %w", err)
	}

	slug := fallbackBranchSlug(summary)
	if len(summary.Files) == 0 {
		// An empty commit (or one whose only content the diff omits) leaves
		// nothing to derive a slug from; name the branch from the commit
		// subject instead.
		if subject := deps.headCommitSubject(ctx, workspaceRoot); subject != "" {
			slug = zerogit.SlugifyBranchComponent(subject)
		}
	}
	if opts.AutoNaming && strings.TrimSpace(summary.Diff) != "" {
		llmSlug := ""
		if resolved, cfgErr := deps.resolveConfig(workspaceRoot, config.Overrides{}); cfgErr == nil && config.HasProviderProfile(resolved.Provider) {
			if provider, provErr := deps.newProvider(resolved.Provider); provErr == nil {
				if !opts.JSONMode {
					fmt.Fprintln(stdout, "Generating branch name using LLM...")
				}
				genCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
				generated, genErr := generateAutoBranchSlug(genCtx, provider, resolved.Provider.Model, redactChangeSummary(summary))
				cancel()
				if genErr == nil && generated != "" {
					llmSlug = generated
				}
			}
		}
		if llmSlug != "" {
			slug = llmSlug
		} else if !opts.JSONMode {
			// Cover resolveConfig failures, missing provider profiles,
			// newProvider errors, generation errors, and empty results so the
			// user is not left guessing after "Generating..." or silence.
			fmt.Fprintln(stdout, "LLM branch naming unavailable; using deterministic name.")
		}
	}

	return branchPushPlan{
		ShouldBranch:           true,
		CurrentBranch:          currentBranch,
		PushRemote:             pushRemote,
		RequireNewRemoteBranch: true,
		TrackingRemote:         trackingRemote,
		TrackingBranch:         trackingBranch,
		RestoreTip:             restoreTip,
		PublishabilityBase:     baseRef,
		Summary:                summary,
		Slug:                   slug,
	}, nil
}

// ensureFeatureBranch is the branch-naming step `zero changes push`/`pr` run
// before pushing: pushing straight to the default branch is refused deeper in
// zerogit.Push, so rather than surface that as a dead end, create and switch
// to a conventionally named "<user>/<slug>" branch first. It returns the
// branch push/pr should target, or "" to mean "current HEAD branch, unchanged"
// (zerogit.Push already treats an empty Branch that way), plus the remote the
// preflight resolved (requestedRemote, then the original branch's configured
// upstream, then "origin"), plus whether Push should require that branch not
// already exist on that remote. Callers must pass the remote to Push: a
// freshly created branch has no tracking configuration, so Push's own
// fallback would silently retarget "origin" even when the work came from a
// branch tracking a different remote. Callers must also pass that third value
// through as Push's RequireNewRemoteBranch: CreateBranch's own
// remote-collision probe runs before this returns, and closing that race
// requires Push's push itself to assert the destination is still new. That
// requirement also carries across a retry: if the current branch is already
// non-default (this call didn't create it) but has not been successfully
// published to the exact target remote under the same branch name, keep the
// nonexistence lease. branch.<name>.remote alone is not enough: with
// branch.autoSetupMerge=inherit, checkout -b copies remote=origin from the
// source branch before any push. After a successful create, the original
// default branch ref is moved back to its own upstream tip (not necessarily
// the push destination) so the feature branch exclusively owns the
// publishable commits; a failed restore rolls the feature branch back.
// allowDefaultBranch (the --yes flag) and dryRun both opt out via the ""
// branch / false return, leaving Push's own guard/preview behavior on the
// default branch unaffected.
//
// autoNaming gates the LLM naming path (--auto): these commands were
// git-only, and sending the change diff to a configured provider on every
// default-branch push would silently export source code nobody asked to
// share. Without the opt-in the name comes from deterministic local
// information only. maxDiffBytes caps the committed-range diff Inspect
// returns, so a user who passed --diff-bytes to bound the proprietary source
// sent for LLM naming has that cap honored here just as the commit path does.
// The working tree must be clean and HEAD must be ahead of the resolved
// remote branch; both are verified before any branch is created. The
// inspectChanges, commitsAhead, headCommitSubject, currentGitUser, and
// createBranch dependencies are required on the default-branch path and are
// called without nil guards; fillAppDeps populates all of them. The
// refreshTrackingRef, resolveRemoteBranchTip, isUnbornRemote, resetBranchRef,
// and deleteBranch dependencies are optional and are skipped when nil.
func ensureFeatureBranch(ctx context.Context, stdout io.Writer, workspaceRoot string, requestedRemote string, opts featureBranchOptions, deps appDeps) (string, string, bool, error) {
	plan, err := resolveBranchPushPlan(ctx, stdout, workspaceRoot, requestedRemote, opts, deps)
	if err != nil {
		return "", "", false, err
	}
	if !plan.ShouldBranch {
		return plan.CurrentBranch, plan.PushRemote, plan.RequireNewRemoteBranch, nil
	}

	expectedRestoreTip := ""
	if deps.currentBranchTip != nil {
		expectedRestoreTip = deps.currentBranchTip(ctx, workspaceRoot)
	}

	name := zerogit.BuildBranchName(deps.currentGitUser(ctx, workspaceRoot), plan.Slug)
	result, err := deps.createBranch(ctx, zerogit.BranchOptions{Cwd: workspaceRoot, Name: name, Remote: plan.PushRemote})
	if err != nil {
		return "", "", false, fmt.Errorf("failed to create branch: %w", err)
	}
	// The normal flow commits on the default branch first, then creates the
	// feature branch at the same HEAD. Without moving the original default
	// ref back, local main keeps the pre-squash commits and diverges from
	// its upstream after a squash-merge (and a later pull/push can re-publish
	// them). Once the feature branch owns those commits, restore the default
	// branch to the source branch's own upstream tip captured above.
	// Failure-safe: if the restore fails, delete the feature branch and
	// check the default branch back out so the tree is not left half-moved.
	if deps.resetBranchRef != nil {
		if err := deps.resetBranchRef(ctx, workspaceRoot, plan.CurrentBranch, plan.RestoreTip, expectedRestoreTip); err != nil {
			// A compare-and-swap conflict means the default branch moved
			// while we were branching, so restoreTip is no longer the tip we
			// meant to restore. The rollback below deletes the branch that
			// now exclusively owns the user's commits, which would destroy
			// work to recover from a race we can simply report. Keep the
			// branch and let the user reconcile the default branch.
			if errors.Is(err, zerogit.ErrCompareAndSwapConflict) {
				return "", "", false, fmt.Errorf("failed to restore default branch %s to %s after auto-branching: %w; generated branch %s was preserved because the default branch changed concurrently", plan.CurrentBranch, plan.RestoreTip, err, result.Branch)
			}
			var rollbackErr error
			if deps.deleteBranch != nil {
				rollbackErr = deps.deleteBranch(ctx, workspaceRoot, plan.CurrentBranch, result.Branch)
			}
			if rollbackErr != nil {
				return "", "", false, fmt.Errorf("failed to restore default branch %s to %s after auto-branching: %w; rollback of %s also failed: %v", plan.CurrentBranch, plan.RestoreTip, err, result.Branch, rollbackErr)
			}
			return "", "", false, fmt.Errorf("failed to restore default branch %s to %s after auto-branching: %w", plan.CurrentBranch, plan.RestoreTip, err)
		}
	}
	if !opts.JSONMode {
		fmt.Fprintf(stdout, "Created branch %s (was on %s)\n", result.Branch, plan.CurrentBranch)
	}
	return result.Branch, plan.PushRemote, true, nil
}

// fallbackBranchSlug derives a deterministic branch-name slug from a change
// summary without calling an LLM, so ensureFeatureBranch still works when no
// provider is configured.
func fallbackBranchSlug(summary zerogit.ChangeSummary) string {
	switch len(summary.Files) {
	case 0:
		return "changes"
	case 1:
		// Git paths are always slash-separated regardless of platform, so
		// path.Base (not filepath.Base) extracts the filename portably.
		return zerogit.SlugifyBranchComponent(path.Base(summary.Files[0].Path))
	default:
		return fmt.Sprintf("update-%d-files", len(summary.Files))
	}
}

// generateAutoBranchSlug asks the model for a short kebab-case slug
// describing the diff, mirroring generateAutoCommitMessage's prompt shape.
func generateAutoBranchSlug(ctx context.Context, provider zeroruntime.Provider, model string, summary zerogit.ChangeSummary) (string, error) {
	var promptBuilder strings.Builder
	promptBuilder.WriteString("Analyze the following git diff and generate a short git branch name slug for it.\n")
	promptBuilder.WriteString("The slug must be 2 to 5 lowercase words separated by hyphens (kebab-case), using only letters, digits, and hyphens, with no prefix like \"feature/\" or \"fix/\" and no surrounding quotes.\n")
	promptBuilder.WriteString("Output ONLY the raw slug text, nothing else.\n\n")
	promptBuilder.WriteString("Git Diff:\n")
	promptBuilder.WriteString(summary.Diff)

	request := zeroruntime.CompletionRequest{
		Messages: []zeroruntime.Message{
			{Role: zeroruntime.MessageRoleUser, Content: promptBuilder.String()},
		},
	}
	stream, err := provider.StreamCompletion(ctx, request)
	if err != nil {
		return "", err
	}
	collected := zeroruntime.CollectStream(ctx, stream)
	if collected.Error != "" {
		return "", fmt.Errorf("%s", collected.Error)
	}

	slug := zerogit.SlugifyBranchComponent(extractBranchSlug(collected.Text))
	if slug == "" {
		return "", fmt.Errorf("provider returned empty branch slug")
	}
	return slug, nil
}

// slugLineRe matches a line that already reads as a kebab-case slug (letters,
// digits, and internal single hyphens only). extractBranchSlug prefers such a
// line so a preamble sentence is never mistaken for the slug.
var slugLineRe = regexp.MustCompile(`^[a-zA-Z0-9]+(-[a-zA-Z0-9]+)*$`)

func isPreambleText(s string) bool {
	lower := strings.ToLower(strings.TrimSpace(s))
	if strings.HasSuffix(lower, ":") || strings.HasSuffix(lower, ".") || strings.HasSuffix(lower, "!") || strings.HasSuffix(lower, "?") {
		return true
	}
	preambles := []string{
		"here is", "here's", "below is", "below are", "sure", "certainly",
		"i suggest", "suggested branch", "branch name", "recommended branch",
		"how about", "you could use",
	}
	for _, p := range preambles {
		if strings.HasPrefix(lower, p) {
			return true
		}
	}
	return false
}

// extractBranchSlug pulls the intended slug out of a model response that didn't
// follow the "output only the raw slug" instruction exactly. It drops Markdown
// code-fence lines, then prefers a line that already looks like a kebab-case
// slug: that skips a leading preamble such as "Here is a suggested branch
// name:" in favor of the "add-login-page" line that follows it. One-word
// acknowledgements that happen to match the slug regex ("Sure", "Certainly")
// are filtered as preamble before the early return, so "Sure\nadd login page"
// yields the real suggestion. Inline labeled forms ("Branch name: add-login-page")
// still work: the preamble check runs on the extracted value after the label
// is stripped. If no line is strictly kebab-cased, it prefers a plausible
// non-preamble line so plain multi-word replies still slugify correctly.
func extractBranchSlug(text string) string {
	var firstLine string

	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "```") {
			continue
		}
		line = strings.TrimSpace(strings.Trim(line, `"'`))
		if line == "" {
			continue
		}

		candidate := line
		if idx := strings.Index(line, ":"); idx != -1 {
			after := strings.TrimSpace(strings.Trim(line[idx+1:], `"'`))
			if after != "" {
				candidate = after
			}
		}
		// Models often terminate a suggestion with sentence punctuation;
		// strip it so "add login page." is recovered as the slug rather than
		// rejected as preamble (isPreambleText treats a trailing '.' as a
		// full sentence). True preamble lines still match the prefix list.
		candidate = strings.TrimSpace(strings.TrimRight(candidate, ".!?"))
		if candidate == "" {
			continue
		}

		// Preamble classification runs before accepting a slug-shaped line so
		// "Sure" does not win over a later "add-login-page". The check is on
		// candidate (post label-strip) so "Branch name: add-login-page" still
		// returns the extracted slug.
		if slugLineRe.MatchString(candidate) && !isPreambleText(candidate) {
			return candidate
		}

		if firstLine == "" && !isPreambleText(candidate) {
			firstLine = candidate
		}
	}

	return firstLine
}
