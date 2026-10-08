package zerogit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Gitlawb/zero/internal/redaction"
)

type Runner func(context.Context, string, ...string) (CommandResult, error)
type EnvRunner func(context.Context, string, []string, ...string) (CommandResult, error)

type CommandResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

type InspectOptions struct {
	Cwd          string
	BaseRef      string
	MaxDiffBytes int
	RunGit       Runner
	RunGitEnv    EnvRunner
}

type CommitOptions struct {
	Cwd          string
	Message      string
	DryRun       bool
	MaxDiffBytes int
	RunGit       Runner
	RunGitEnv    EnvRunner
}

type FileChange struct {
	Path      string `json:"path"`
	Status    string `json:"status"`
	Staged    bool   `json:"staged,omitempty"`
	Unstaged  bool   `json:"unstaged,omitempty"`
	Untracked bool   `json:"untracked,omitempty"`
}

type ChangeSummary struct {
	Root      string       `json:"root"`
	Branch    string       `json:"branch,omitempty"`
	Base      string       `json:"base,omitempty"`
	Commit    string       `json:"commit,omitempty"`
	Clean     bool         `json:"clean"`
	Files     []FileChange `json:"files"`
	DiffStat  string       `json:"diffStat,omitempty"`
	Diff      string       `json:"diff,omitempty"`
	Truncated bool         `json:"truncated,omitempty"`
}

type CommitResult struct {
	Root       string        `json:"root"`
	Message    string        `json:"message"`
	DryRun     bool          `json:"dryRun"`
	Committed  bool          `json:"committed"`
	CommitHash string        `json:"commitHash,omitempty"`
	Before     ChangeSummary `json:"before"`
}

const defaultMaxDiffBytes = 120000

func Inspect(ctx context.Context, options InspectOptions) (ChangeSummary, error) {
	cwd, err := resolveCwd(options.Cwd)
	if err != nil {
		return ChangeSummary{}, err
	}
	runGit, runGitEnv := resolveRunners(options.RunGit, options.RunGitEnv)

	root, err := gitOutput(ctx, runGit, cwd, "rev-parse", "--show-toplevel")
	if err != nil {
		return ChangeSummary{}, fmt.Errorf("not a git repository: %w", err)
	}
	root = filepath.Clean(root)
	branch, _ := gitOutput(ctx, runGit, root, "rev-parse", "--abbrev-ref", "HEAD")
	commit, _ := gitOutput(ctx, runGit, root, "rev-parse", "--short", "HEAD")

	maxDiffBytes := firstPositive(options.MaxDiffBytes, defaultMaxDiffBytes)

	base := strings.TrimSpace(options.BaseRef)
	if base != "" {
		nameStatus, err := gitRawOutput(ctx, runGit, root, "diff", "--name-status", base+"...HEAD", "--")
		if err != nil {
			return ChangeSummary{}, fmt.Errorf("inspect base diff status: %w", err)
		}
		diffStat, err := gitRawOutput(ctx, runGit, root, "diff", "--stat", base+"...HEAD", "--")
		if err != nil {
			return ChangeSummary{}, fmt.Errorf("inspect base diff stat: %w", err)
		}
		diff, err := gitRawOutput(ctx, runGit, root, "diff", base+"...HEAD", "--")
		if err != nil {
			return ChangeSummary{}, fmt.Errorf("inspect base diff: %w", err)
		}
		redactedDiff, truncated := truncateString(redactText(diff), maxDiffBytes)
		files := parseNameStatus(nameStatus)
		return ChangeSummary{
			Root:      root,
			Branch:    redactText(branch),
			Base:      redactText(base),
			Commit:    redactText(commit),
			Clean:     len(files) == 0,
			Files:     files,
			DiffStat:  redactText(diffStat),
			Diff:      redactedDiff,
			Truncated: truncated,
		}, nil
	}

	status, err := gitRawOutput(ctx, runGit, root, "status", "--porcelain", "-z", "--untracked-files=all")
	if err != nil {
		return ChangeSummary{}, fmt.Errorf("inspect git status: %w", err)
	}
	diffStat, diff, err := stagedSnapshotDiff(ctx, runGitEnv, root)
	if err != nil {
		return ChangeSummary{}, err
	}

	redactedDiff, truncated := truncateString(redactText(diff), maxDiffBytes)
	files := parseStatus(status)
	return ChangeSummary{
		Root:      root,
		Branch:    redactText(branch),
		Commit:    redactText(commit),
		Clean:     len(files) == 0,
		Files:     files,
		DiffStat:  redactText(diffStat),
		Diff:      redactedDiff,
		Truncated: truncated,
	}, nil
}

func Commit(ctx context.Context, options CommitOptions) (CommitResult, error) {
	summary, err := Inspect(ctx, InspectOptions{
		Cwd:          options.Cwd,
		MaxDiffBytes: options.MaxDiffBytes,
		RunGit:       options.RunGit,
		RunGitEnv:    options.RunGitEnv,
	})
	if err != nil {
		return CommitResult{}, err
	}
	if summary.Clean {
		return CommitResult{}, fmt.Errorf("no changes to commit")
	}
	message := strings.TrimSpace(options.Message)
	if message == "" {
		message = GenerateMessage(summary)
	}
	if err := ValidateMessage(message); err != nil {
		return CommitResult{}, err
	}
	result := CommitResult{
		Root:      summary.Root,
		Message:   message,
		DryRun:    options.DryRun,
		Committed: false,
		Before:    summary,
	}
	if options.DryRun {
		return result, nil
	}

	runGit, _ := resolveRunners(options.RunGit, options.RunGitEnv)
	if _, err := gitOutput(ctx, runGit, summary.Root, "add", "-A"); err != nil {
		return CommitResult{}, fmt.Errorf("stage changes: %w", err)
	}
	if _, err := gitOutput(ctx, runGit, summary.Root, "commit", "-m", message); err != nil {
		return CommitResult{}, fmt.Errorf("commit changes: %w", err)
	}
	hash, err := gitOutput(ctx, runGit, summary.Root, "rev-parse", "--short", "HEAD")
	if err != nil {
		return CommitResult{}, fmt.Errorf("resolve created commit: %w", err)
	}
	result.Committed = true
	result.CommitHash = redactText(hash)
	return result, nil
}

func GenerateMessage(summary ChangeSummary) string {
	count := len(summary.Files)
	switch count {
	case 0:
		return "Update workspace"
	case 1:
		return truncateSubject("Update " + summary.Files[0].Path)
	default:
		return fmt.Sprintf("Update %d files", count)
	}
}

func ValidateMessage(message string) error {
	trimmed := strings.TrimSpace(message)
	if trimmed == "" {
		return fmt.Errorf("commit message is required")
	}
	firstLine := strings.Split(trimmed, "\n")[0]
	// Count runes, not bytes, so a valid non-ASCII subject under the limit is not
	// rejected for spilling past 72 bytes.
	if utf8.RuneCountInString(firstLine) > 72 {
		return fmt.Errorf("commit message subject must be 72 characters or fewer")
	}
	return nil
}

// parseStatus parses NUL-delimited `git status --porcelain -z` output. The -z
// form is used instead of the default --short because it never C-quotes paths
// (so non-ASCII or whitespace filenames arrive verbatim rather than as a
// "\303\251"-escaped, double-quoted token) and because it emits a rename/copy as
// two NUL-separated fields — `XY <dest>\0<src>` — letting us record the
// destination path and skip the source instead of mistaking the whole
// `dest -> src` string for a single filename.
func parseStatus(status string) []FileChange {
	files := []FileChange{}
	fields := strings.Split(status, "\x00")
	for i := 0; i < len(fields); i++ {
		entry := fields[i]
		if len(entry) < 3 {
			continue
		}
		code := entry[:2]
		// Format is exactly `XY<space>PATH`; -z never quotes or pads PATH, so it
		// is taken verbatim (no TrimSpace, which would corrupt names with leading
		// or trailing spaces).
		path := entry[3:]
		// A rename/copy is followed by a separate NUL-terminated field holding the
		// original path; consume it so it is not parsed as its own entry. This
		// entry's own path is the destination.
		//
		// Only the INDEX column (code[0]) is checked, never the worktree column
		// (code[1]): porcelain v1 -z reports a rename/copy (and emits the extra
		// source field) only in the index column. A worktree-only rename is shown as
		// a delete + untracked pair (" D old\0?? new\0"), NOT "R" in code[1] — so
		// consuming on code[1]=='R'/'C' would never match real git output and would
		// only risk mis-consuming the next entry on malformed input. (Verified
		// empirically: git mv → "R  new\0old\0"; plain mv → " D old\0?? new\0".)
		if code[0] == 'R' || code[0] == 'C' {
			i++
		}
		if path == "" {
			continue
		}
		files = append(files, FileChange{
			Path:      redactText(path),
			Status:    statusName(code),
			Staged:    code[0] != ' ' && code[0] != '?',
			Unstaged:  code[1] != ' ' && code[1] != '?',
			Untracked: code == "??",
		})
	}
	return files
}

func parseNameStatus(output string) []FileChange {
	files := []FileChange{}
	for _, line := range strings.Split(output, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) < 2 {
			continue
		}
		code := strings.TrimSpace(fields[0])
		if code == "" {
			continue
		}
		path := strings.TrimSpace(fields[len(fields)-1])
		if path == "" {
			continue
		}
		files = append(files, FileChange{
			Path:   redactText(path),
			Status: nameStatusName(code[:1]),
		})
	}
	return files
}

func nameStatusName(letter string) string {
	switch letter {
	case "A":
		return "added"
	case "D":
		return "deleted"
	case "R":
		return "renamed"
	case "C":
		return "copied"
	case "U":
		return "conflicted"
	case "T":
		return "modified"
	default:
		return "modified"
	}
}

func statusName(code string) string {
	if code == "??" {
		return "untracked"
	}
	if strings.Contains(code, "U") {
		return "conflicted"
	}
	if code[0] == 'A' || code[1] == 'A' {
		return "added"
	}
	if code[0] == 'D' || code[1] == 'D' {
		return "deleted"
	}
	if code[0] == 'R' || code[1] == 'R' {
		return "renamed"
	}
	if code[0] == 'C' || code[1] == 'C' {
		return "copied"
	}
	return "modified"
}

func resolveCwd(cwd string) (string, error) {
	if strings.TrimSpace(cwd) == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return "", fmt.Errorf("resolve git cwd: %w", err)
		}
	}
	absolute, err := filepath.Abs(cwd)
	if err != nil {
		return "", fmt.Errorf("resolve git cwd: %w", err)
	}
	info, err := os.Stat(absolute)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("git cwd must be an existing directory: %s", absolute)
	}
	return filepath.Clean(absolute), nil
}

func gitOutput(ctx context.Context, runGit Runner, dir string, args ...string) (string, error) {
	output, err := gitRawOutput(ctx, runGit, dir, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(output), nil
}

func gitRawOutput(ctx context.Context, runGit Runner, dir string, args ...string) (string, error) {
	result, err := runGit(ctx, dir, args...)
	return gitResultOutput(result, err)
}

func gitRawOutputEnv(ctx context.Context, runGit EnvRunner, dir string, env []string, args ...string) (string, error) {
	result, err := runGit(ctx, dir, env, args...)
	return gitResultOutput(result, err)
}

func gitResultOutput(result CommandResult, err error) (string, error) {
	if err != nil {
		return "", err
	}
	if result.ExitCode != 0 {
		message := strings.TrimSpace(firstNonEmpty(result.Stderr, result.Stdout))
		if message == "" {
			message = fmt.Sprintf("git exited with code %d", result.ExitCode)
		}
		return "", fmt.Errorf("%s", redactText(message))
	}
	return result.Stdout, nil
}

func stagedSnapshotDiff(ctx context.Context, runGit EnvRunner, root string) (string, string, error) {
	tempDir, err := os.MkdirTemp("", "zero-git-index-")
	if err != nil {
		return "", "", fmt.Errorf("prepare preview index: %w", err)
	}
	defer func() { _ = os.RemoveAll(tempDir) }()

	env := []string{"GIT_INDEX_FILE=" + filepath.Join(tempDir, "index")}
	if _, err := gitRawOutputEnv(ctx, runGit, root, env, "rev-parse", "--verify", "HEAD"); err != nil {
		if _, emptyErr := gitRawOutputEnv(ctx, runGit, root, env, "read-tree", "--empty"); emptyErr != nil {
			return "", "", fmt.Errorf("prepare empty preview index: %w", emptyErr)
		}
	} else if _, err := gitRawOutputEnv(ctx, runGit, root, env, "read-tree", "HEAD"); err != nil {
		return "", "", fmt.Errorf("prepare preview index from HEAD: %w", err)
	}
	if _, err := gitRawOutputEnv(ctx, runGit, root, env, "add", "-A"); err != nil {
		return "", "", fmt.Errorf("stage preview index: %w", err)
	}
	diffStat, err := gitRawOutputEnv(ctx, runGit, root, env, "diff", "--cached", "--stat", "--")
	if err != nil {
		return "", "", fmt.Errorf("inspect git diff stat: %w", err)
	}
	diff, err := gitRawOutputEnv(ctx, runGit, root, env, "diff", "--cached", "--")
	if err != nil {
		return "", "", fmt.Errorf("inspect git diff: %w", err)
	}
	return diffStat, diff, nil
}

func defaultRunGit(ctx context.Context, dir string, args ...string) (CommandResult, error) {
	return defaultRunGitEnv(ctx, dir, nil, args...)
}

func defaultRunGitEnv(ctx context.Context, dir string, env []string, args ...string) (CommandResult, error) {
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = dir
	if len(env) > 0 {
		command.Env = append(os.Environ(), env...)
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	exitCode := 0
	if err != nil {
		exitCode = -1
		if exitError, ok := err.(*exec.ExitError); ok {
			exitCode = exitError.ExitCode()
			err = nil
		}
	}
	return CommandResult{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: exitCode}, err
}

func resolveRunners(runGit Runner, runGitEnv EnvRunner) (Runner, EnvRunner) {
	if runGit == nil {
		runGit = defaultRunGit
		if runGitEnv == nil {
			runGitEnv = defaultRunGitEnv
		}
	} else if runGitEnv == nil {
		// A plain Runner has no env parameter, so an env-bearing call (e.g.
		// stagedSnapshotDiff's GIT_INDEX_FILE isolation) cannot thread its env
		// through it; env is intentionally dropped on this adapter. This branch is
		// reached ONLY when a caller supplies a custom Runner without a matching
		// EnvRunner — in practice, tests with a fake Runner that intercepts every
		// git call. Production callers leave both nil and get
		// defaultRunGit/defaultRunGitEnv above, which honor env, so GIT_INDEX_FILE
		// isolation holds on the real path. A custom Runner that also needs env
		// isolation must supply a RunGitEnv alongside it.
		runGitEnv = func(ctx context.Context, dir string, _ []string, args ...string) (CommandResult, error) {
			return runGit(ctx, dir, args...)
		}
	}
	return runGit, runGitEnv
}

func truncateString(value string, maxBytes int) (string, bool) {
	if maxBytes <= 0 || len(value) <= maxBytes {
		return value, false
	}
	suffix := "\n[truncated]"
	if maxBytes <= len(suffix) {
		return suffix[:maxBytes], true
	}
	head := cutGitRuneBoundary(value, maxBytes-len(suffix))
	if strings.Contains(value, redaction.RedactedSecret) && !strings.Contains(head, redaction.RedactedSecret) {
		marker := "\n" + redaction.RedactedSecret
		budget := maxBytes - len(suffix) - len(marker)
		if budget <= 0 {
			allowed := maxBytes - len(suffix)
			if allowed > len(redaction.RedactedSecret) {
				allowed = len(redaction.RedactedSecret)
			}
			return redaction.RedactedSecret[:allowed] + suffix, true
		}
		return cutGitRuneBoundary(value, budget) + marker + suffix, true
	}
	return head + suffix, true
}

// cutGitRuneBoundary truncates to at most n bytes on a rune boundary so
// truncated git output and subjects stay valid UTF-8.
func cutGitRuneBoundary(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func truncateSubject(value string) string {
	// Count runes, not bytes: a 72-byte limit rejected valid non-ASCII
	// subjects and the byte slice could cut a rune in half.
	runes := []rune(value)
	if len(runes) <= 72 {
		return value
	}
	return strings.TrimSpace(string(runes[:69])) + "..."
}

func redactText(value string) string {
	return redaction.RedactString(value, redaction.Options{})
}

func firstPositive(values ...int) int {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

type PushOptions struct {
	Cwd    string
	Remote string
	Branch string
	Force  bool
	DryRun bool
	// RequireNewRemoteBranch guards this push with a zero-value
	// --force-with-lease, so it is rejected instead of fast-forwarding if
	// Branch already exists on Remote. CreateBranch's collision probe reads
	// the remote's branches before this push runs, leaving a window in which
	// a concurrent creator can publish the same generated name; this closes
	// it at the one point that actually talks to the remote atomically.
	RequireNewRemoteBranch bool
	AllowPushDefaultBranch bool
	RunGit                 Runner
	RunGitEnv              EnvRunner
}

type PushResult struct {
	Remote string `json:"remote"`
	Branch string `json:"branch"`
	Output string `json:"output"`
}

func Push(ctx context.Context, options PushOptions) (PushResult, error) {
	cwd, err := resolveCwd(options.Cwd)
	if err != nil {
		return PushResult{}, err
	}
	runGit, _ := resolveRunners(options.RunGit, options.RunGitEnv)

	root, err := gitOutput(ctx, runGit, cwd, "rev-parse", "--show-toplevel")
	if err != nil {
		return PushResult{}, fmt.Errorf("not a git repository: %w", err)
	}
	root = filepath.Clean(root)

	branch := strings.TrimSpace(options.Branch)
	if branch == "" {
		branch, err = gitOutput(ctx, runGit, root, "rev-parse", "--abbrev-ref", "HEAD")
		if err != nil {
			return PushResult{}, fmt.Errorf("resolve current branch: %w", err)
		}
	}
	if branch == "HEAD" {
		return PushResult{}, fmt.Errorf("cannot push: not currently on a branch")
	}

	remote := strings.TrimSpace(options.Remote)
	if remote == "" {
		if upstream, err := gitOutput(ctx, runGit, root, "config", "branch."+branch+".remote"); err == nil && upstream != "" {
			remote = upstream
		} else {
			remote = "origin"
		}
	}

	if !options.AllowPushDefaultBranch {
		isDefault, err := isDefaultBranch(ctx, runGit, root, remote, branch)
		if err != nil {
			return PushResult{}, fmt.Errorf("cannot verify %q is not the default/protected branch: %w; use --yes to override", branch, err)
		}
		if isDefault {
			return PushResult{}, fmt.Errorf("refusing to push to %q (default/protected branch); use --yes to override", branch)
		}
	}

	args := []string{"push"}
	if options.DryRun {
		args = append(args, "--dry-run")
	}
	switch {
	case options.RequireNewRemoteBranch:
		// An empty expected value means the ref must not currently exist on
		// the remote: Git rejects the push if another client created
		// <branch> after CreateBranch's own remote probe ran, instead of
		// silently fast-forwarding it with this work.
		args = append(args, "--force-with-lease="+branch+":")
	case options.Force:
		args = append(args, "--force-with-lease")
	}
	args = append(args, "-u", "--", remote, branch)

	output, err := gitRawOutput(ctx, runGit, root, args...)
	if err != nil {
		return PushResult{}, fmt.Errorf("push: %w", err)
	}

	// git push -u can create the remote branch and still exit 0 when it cannot
	// write branch.<name>.remote/merge (for example .git/config.lock held by
	// another process). Zero must not report that as a full success: without
	// the local upstream, a later ensureFeatureBranch retry would reassert
	// --force-with-lease=<branch>: against a branch that already exists.
	// Dry-run never publishes and never writes branch.<name>.remote/merge, so
	// skip verification/repair: set-upstream-to would mutate the repo and fail
	// for unpublished branches with a misleading "push published" error.
	expectedUpstream := remote + "/" + branch
	if !options.DryRun && IsNamedRemote(ctx, root, remote, runGit) && UpstreamRef(ctx, root, branch, runGit) != expectedUpstream {
		if _, setErr := gitOutput(ctx, runGit, root, "branch", "--set-upstream-to="+expectedUpstream, branch); setErr != nil {
			return PushResult{Remote: remote, Branch: branch, Output: output}, fmt.Errorf(
				"push published %s but failed to configure local upstream %s: %w; run `git branch --set-upstream-to=%s %s` then retry",
				expectedUpstream, expectedUpstream, setErr, expectedUpstream, branch)
		}
		if UpstreamRef(ctx, root, branch, runGit) != expectedUpstream {
			return PushResult{Remote: remote, Branch: branch, Output: output}, fmt.Errorf(
				"push published %s but local upstream is still not %s; run `git branch --set-upstream-to=%s %s` then retry",
				expectedUpstream, expectedUpstream, expectedUpstream, branch)
		}
	}

	return PushResult{
		Remote: remote,
		Branch: branch,
		Output: output,
	}, nil
}

func isDefaultBranch(ctx context.Context, runGit Runner, dir, remote, branch string) (bool, error) {
	// "--" terminates option parsing: remote comes from --remote/branch config,
	// and a value like "--upload-pack=/bin/echo" must reach Git as a positional
	// argument, never as an option.
	if out, err := gitOutput(ctx, runGit, dir, "ls-remote", "--symref", "--", remote, "HEAD"); err == nil {
		for _, line := range strings.Split(out, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "ref: refs/heads/") && strings.HasSuffix(line, "\tHEAD") {
				symref := strings.TrimPrefix(line, "ref: refs/heads/")
				symref = strings.TrimSuffix(symref, "\tHEAD")
				return branch == symref, nil
			}
		}
		if strings.TrimSpace(out) == "" {
			// HEAD didn't resolve to anything. A genuinely unborn remote (no
			// refs at all) answers this way, and it cannot have a protected
			// default branch yet, so the fail-closed error below would make
			// the very first feature-branch push a dead end. But a non-empty
			// remote whose HEAD symref is dangling or missing produces the
			// same empty output while still possibly having a protected
			// default under a name this couldn't identify, so confirm the
			// remote truly has no branches before granting the unborn
			// exception; a non-default first push is safe.
			if heads, headsErr := gitOutput(ctx, runGit, dir, "ls-remote", "--heads", "--", remote); headsErr == nil && strings.TrimSpace(heads) == "" {
				if branch == "main" || branch == "master" {
					return true, nil
				}
				return false, nil
			}
		}
	}
	// The remote lookup failed (unreachable, slow, or gave no symref): the
	// local refs/remotes/<remote>/HEAD record, written by clone or `git remote
	// set-head`, is only a cache. Trust it to *block* a push (a positive match
	// proves branch is the recorded default) but never to *clear* the guard. If
	// the server renamed its default (main -> trunk) the stale record still
	// names main, so a mismatch here is not evidence that pushing trunk is safe;
	// fall through instead of returning false.
	if out, err := gitOutput(ctx, runGit, dir, "symbolic-ref", "--quiet", "refs/remotes/"+remote+"/HEAD"); err == nil {
		if name, ok := strings.CutPrefix(strings.TrimSpace(out), "refs/remotes/"+remote+"/"); ok && name == branch {
			return true, nil
		}
	}
	// The conventional default names are only a fallback for when the remote's
	// actual default genuinely could not be determined above (live or cached).
	// Applying this before consulting the remote at all would misidentify a
	// repository whose real default is e.g. "trunk" but that also happens to
	// have a local/tracked "main": the live symref result must win whenever
	// it's available. This is still safe-direction only (it can block a push,
	// never permit one) since it's the last resort before failing closed.
	if branch == "main" || branch == "master" {
		return true, nil
	}
	// Fail closed: before this, a lookup timeout silently downgraded the
	// check to the main/master name heuristic, so a repository whose default
	// is trunk/develop lost the confirmation guard exactly when the remote
	// was slow.
	return false, fmt.Errorf("default branch for remote %q is unknown (remote lookup failed and no local refs/remotes/%s/HEAD record exists; run `git remote set-head %s --auto` to record it)", remote, remote, remote)
}

// DefaultBranchOptions resolves whether a branch is the repository's
// default/protected branch.
type DefaultBranchOptions struct {
	Cwd    string
	Remote string
	Branch string // empty resolves the current branch
	RunGit Runner
}

// IsDefaultBranch reports whether options.Branch (or, if empty, the current
// branch) is the repository's default/protected branch, using the same check
// Push already applies before refusing to push straight to it. It returns the
// resolved branch name and remote alongside the bool so callers that left
// them empty don't need a second lookup: the remote is resolved exactly the
// way Push resolves it (explicit option, then the branch's configured
// upstream, then "origin"), so a caller can thread the same remote through a
// later Push instead of letting a freshly created branch with no tracking
// configuration silently fall back to "origin".
func IsDefaultBranch(ctx context.Context, options DefaultBranchOptions) (bool, string, string, error) {
	cwd, err := resolveCwd(options.Cwd)
	if err != nil {
		return false, "", "", err
	}
	runGit, _ := resolveRunners(options.RunGit, nil)

	root, err := gitOutput(ctx, runGit, cwd, "rev-parse", "--show-toplevel")
	if err != nil {
		return false, "", "", fmt.Errorf("not a git repository: %w", err)
	}
	root = filepath.Clean(root)

	branch := strings.TrimSpace(options.Branch)
	if branch == "" {
		branch, err = gitOutput(ctx, runGit, root, "rev-parse", "--abbrev-ref", "HEAD")
		if err != nil {
			return false, "", "", fmt.Errorf("resolve current branch: %w", err)
		}
	}
	remote := strings.TrimSpace(options.Remote)
	if remote == "" {
		if upstream, err := gitOutput(ctx, runGit, root, "config", "branch."+branch+".remote"); err == nil && upstream != "" {
			remote = upstream
		} else {
			remote = "origin"
		}
	}
	// Honor the caller's deadline (or lack of one). A fixed short timeout here
	// turned slow but reachable remotes (SSH/VPN handshakes) into fail-closed
	// "use --yes" errors for ordinary feature-branch pushes, because
	// ensureFeatureBranch always consults this before Push. Callers that need
	// a bound should pass a context with a deadline.
	isDefault, err := isDefaultBranch(ctx, runGit, root, remote, branch)
	if err != nil {
		return false, branch, remote, err
	}
	return isDefault, branch, remote, nil
}

// BranchOptions configures creating and checking out a new local branch.
type BranchOptions struct {
	Cwd    string
	Name   string // full branch name, e.g. "alice/fix-typo"
	DryRun bool
	RunGit Runner
	// Remote, when non-empty, is the remote the new branch will be pushed
	// to; its branch names are treated as taken when resolving collisions,
	// so a remote-only stale branch (e.g. an old merged PR) is never
	// fast-forwarded with unrelated new work.
	Remote string
}

// BranchResult reports the branch that was (or, in dry-run, would be) created.
type BranchResult struct {
	Branch string `json:"branch"`
}

// CreateBranch checks out a new local branch named options.Name off the
// current HEAD. DryRun returns the requested trimmed name without collision
// resolution or repository mutation; a real run may append a numeric suffix
// when the name is already taken locally or on the remote.
func CreateBranch(ctx context.Context, options BranchOptions) (BranchResult, error) {
	cwd, err := resolveCwd(options.Cwd)
	if err != nil {
		return BranchResult{}, err
	}
	runGit, _ := resolveRunners(options.RunGit, nil)

	root, err := gitOutput(ctx, runGit, cwd, "rev-parse", "--show-toplevel")
	if err != nil {
		return BranchResult{}, fmt.Errorf("not a git repository: %w", err)
	}
	root = filepath.Clean(root)

	name := strings.TrimSpace(options.Name)
	if name == "" {
		return BranchResult{}, fmt.Errorf("branch name required")
	}
	if strings.HasPrefix(name, "-") || strings.HasPrefix(name, "/") ||
		strings.Contains(name, "..") || strings.ContainsAny(name, "\\ \t\n") {
		return BranchResult{}, fmt.Errorf("invalid branch name %q", name)
	}
	if options.DryRun {
		return BranchResult{Branch: name}, nil
	}
	// A repeated run (the same diff producing the same slug, or a low-entropy
	// fallback name) can collide with a branch that already exists locally.
	// Never check that existing ref out: its history may be entirely
	// unrelated to the current work (an old push under the same name), and
	// switching to it would publish the stale branch while leaving the new
	// commit behind on the default branch. Pick a unique suffixed name off
	// the current HEAD instead, and fail visibly when the namespace is
	// exhausted rather than guess.
	//
	// Local refs are not enough: a branch that exists only on the target
	// remote (an old merged-PR branch, or one pruned locally) would be
	// silently fast-forwarded by the later `push -u`, appending the new work
	// to an unrelated remote branch. Probe the remote's heads once under the
	// caller's context (same connectivity the later push needs) and fail
	// visibly when the remote cannot be consulted.
	remoteTaken := map[string]bool{}
	if remote := strings.TrimSpace(options.Remote); remote != "" {
		out, err := gitOutput(ctx, runGit, root, "ls-remote", "--heads", "--", remote)
		if err != nil {
			return BranchResult{}, fmt.Errorf("cannot check branch names against remote %q: %w", remote, err)
		}
		for _, line := range strings.Split(out, "\n") {
			if _, ref, ok := strings.Cut(strings.TrimSpace(line), "\t"); ok {
				remoteTaken[strings.TrimPrefix(strings.TrimSpace(ref), "refs/heads/")] = true
			}
		}
	}
	base := name
	for suffix := 2; ; suffix++ {
		_, localErr := gitOutput(ctx, runGit, root, "rev-parse", "--verify", "--quiet", "refs/heads/"+name)
		if localErr != nil && !remoteTaken[name] {
			break
		}
		if suffix > 9 {
			return BranchResult{}, fmt.Errorf("branch %q already exists locally or on the remote (as do %s-2 through %s-9); delete the stale branches or create one explicitly with `git checkout -b`", base, base, base)
		}
		name = fmt.Sprintf("%s-%d", base, suffix)
	}
	if _, err := gitOutput(ctx, runGit, root, "checkout", "-b", name, "--"); err != nil {
		return BranchResult{}, fmt.Errorf("create branch %q: %w", name, err)
	}
	return BranchResult{Branch: name}, nil
}

// HeadCommitSubject returns the subject line of the HEAD commit, or "" when
// it cannot be resolved (empty repository, not a git directory). Callers use
// it to name the branch for a push that follows a commit, where the working
// tree is already clean and a diff-based name would be empty.
func HeadCommitSubject(ctx context.Context, cwd string, runGit Runner) string {
	runGit, _ = resolveRunners(runGit, nil)
	if subject, err := gitOutput(ctx, runGit, cwd, "log", "-1", "--format=%s"); err == nil {
		return strings.TrimSpace(subject)
	}
	return ""
}

// CommitsAhead reports how many commits HEAD is ahead of the remote-tracking
// ref <remote>/<branch> or the advertised branch on a direct remote. Auto-branching
// runs this before creating and pushing a feature branch off the default branch: a
// clean, up-to-date default branch has nothing to publish, so the caller can refuse
// rather than push an empty comparison. It returns an error when the count cannot be
// determined (for example the remote-tracking ref was never fetched); callers treat
// that as a hard failure rather than guessing that there is something to publish.
func CommitsAhead(ctx context.Context, cwd, remote, branch string, runGit Runner) (int, error) {
	runGit, _ = resolveRunners(runGit, nil)
	var baseRef string
	if IsNamedRemote(ctx, cwd, remote, runGit) {
		baseRef = remote + "/" + branch
	} else {
		// Non-named remote (direct path or URL). Resolve the advertised remote branch tip via ls-remote.
		out, err := gitOutput(ctx, runGit, cwd, "ls-remote", "--heads", "--", remote, "refs/heads/"+branch)
		if err != nil {
			return 0, fmt.Errorf("cannot check remote branch %s on %s: %w", branch, remote, err)
		}
		var remoteSHA string
		for _, line := range strings.Split(out, "\n") {
			if sha, ref, ok := strings.Cut(strings.TrimSpace(line), "\t"); ok {
				if strings.TrimSpace(ref) == "refs/heads/"+branch {
					remoteSHA = strings.TrimSpace(sha)
					break
				}
			}
		}
		if remoteSHA == "" {
			// Remote has no such branch yet; count all commits on HEAD.
			out, err := gitOutput(ctx, runGit, cwd, "rev-list", "--count", "HEAD")
			if err != nil {
				return 0, err
			}
			count, err := strconv.Atoi(strings.TrimSpace(out))
			if err != nil {
				return 0, fmt.Errorf("parse commit count %q: %w", out, err)
			}
			return count, nil
		}
		baseRef = remoteSHA
	}
	if strings.HasPrefix(baseRef, "-") {
		return 0, fmt.Errorf("invalid base ref %q", baseRef)
	}
	out, err := gitOutput(ctx, runGit, cwd, "rev-list", "--count", baseRef+"..HEAD")
	if err != nil {
		return 0, err
	}
	count, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0, fmt.Errorf("parse commit count %q: %w", out, err)
	}
	return count, nil
}

// RefreshTrackingRef updates the local remote-tracking ref for <remote>/<branch>
// from the remote's current advertised tip. ensureFeatureBranch calls this
// before CommitsAhead: IsDefaultBranch already contacted the remote for its
// symref check, but a merely-cached origin/main (last written at clone or a
// previous fetch) can sit behind the remote's live tip, making a local-only
// rev-list comparison report nothing to publish when the remote has actually
// advanced. When remote is a raw URL or path, no remote-tracking ref is
// maintained and this call is a no-op.
func RefreshTrackingRef(ctx context.Context, cwd, remote, branch string, runGit Runner) error {
	runGit, _ = resolveRunners(runGit, nil)
	if !IsNamedRemote(ctx, cwd, remote, runGit) {
		return nil
	}
	refspec := fmt.Sprintf("+refs/heads/%s:refs/remotes/%s/%s", branch, remote, branch)
	// "--" terminates option parsing so a remote value shaped like an option
	// (--upload-pack=/bin/echo) reaches Git as a positional argument.
	_, err := gitOutput(ctx, runGit, cwd, "fetch", "--", remote, refspec)
	return err
}

// ResolveRemoteBranchTip returns the commit SHA that branch points to on
// remote. ensureFeatureBranch uses this as the restore target after
// auto-branching, and that target must never be a bare local branch name:
// resolving "main" yields the very commit the just-created feature branch
// owns, which makes the restore a silent no-op and leaves the publishable
// commits on the default branch. For a named remote the SHA comes from the
// local remote-tracking ref (which RefreshTrackingRef has just updated); for a
// direct URL or path it is resolved from the remote's advertised heads. It
// fails closed when no such commit exists rather than letting the caller
// substitute a local branch name.
func ResolveRemoteBranchTip(ctx context.Context, cwd, remote, branch string, runGit Runner) (string, error) {
	runGit, _ = resolveRunners(runGit, nil)
	if IsNamedRemote(ctx, cwd, remote, runGit) {
		out, err := gitOutput(ctx, runGit, cwd, "rev-parse", "--verify", "refs/remotes/"+remote+"/"+branch+"^{commit}")
		if err != nil {
			return "", fmt.Errorf("resolve restore tip refs/remotes/%s/%s: %w", remote, branch, err)
		}
		return strings.TrimSpace(out), nil
	}
	// Direct URL or path: resolve the advertised branch tip without creating
	// or updating a remote-tracking ref.
	out, err := gitOutput(ctx, runGit, cwd, "ls-remote", "--heads", "--", remote, "refs/heads/"+branch)
	if err != nil {
		return "", fmt.Errorf("cannot resolve branch %s on %s: %w", branch, remote, err)
	}
	for _, line := range strings.Split(out, "\n") {
		if sha, ref, ok := strings.Cut(strings.TrimSpace(line), "\t"); ok {
			if strings.TrimSpace(ref) == "refs/heads/"+branch && strings.TrimSpace(sha) != "" {
				return strings.TrimSpace(sha), nil
			}
		}
	}
	return "", fmt.Errorf("remote %s has no branch %s to restore to", remote, branch)
}

// isRemoteURLOrPath reports whether remote is formatted as a direct URL
// (https://..., git@..., ssh://...) or filesystem path (/path/to/repo,
// C:\path\to\repo, .\repo), as opposed to a configured named remote (origin, upstream, team/upstream).
func isRemoteURLOrPath(remote string) bool {
	remote = strings.TrimSpace(remote)
	if remote == "" {
		return false
	}
	if strings.Contains(remote, "://") ||
		strings.HasPrefix(remote, "/") ||
		strings.HasPrefix(remote, "\\") ||
		strings.HasPrefix(remote, ".") ||
		strings.HasPrefix(remote, "~") {
		return true
	}
	if len(remote) >= 3 && unicode.IsLetter(rune(remote[0])) && remote[1] == ':' && (remote[2] == '/' || remote[2] == '\\') {
		return true
	}
	if strings.Contains(remote, "@") && strings.Contains(remote, ":") {
		return true
	}
	return false
}

// IsNamedRemote reports whether remote is shaped like a remote NAME (such as
// "origin", "upstream", or "team/upstream") rather than a direct URL
// (https://..., git@...) or a file path (/path/to/repo, C:\path\to\repo).
// It does not consult git config, so a name that is not configured (a typo)
// still reports true; the subsequent git command surfaces that failure.
// ctx, cwd, and runGit are accepted for signature stability with the other
// remote helpers and are unused.
func IsNamedRemote(ctx context.Context, cwd, remote string, runGit Runner) bool {
	remote = strings.TrimSpace(remote)
	if remote == "" || isRemoteURLOrPath(remote) {
		return false
	}
	return true
}

// UpstreamRemoteAndMergeBranch returns the configured upstream remote and merge branch
// for branch from git config branch.<name>.remote and branch.<name>.merge.
func UpstreamRemoteAndMergeBranch(ctx context.Context, cwd, branch string, runGit Runner) (remote string, mergeBranch string) {
	runGit, _ = resolveRunners(runGit, nil)
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return "", ""
	}
	r, _ := gitOutput(ctx, runGit, cwd, "config", "branch."+branch+".remote")
	m, _ := gitOutput(ctx, runGit, cwd, "config", "branch."+branch+".merge")
	remote = strings.TrimSpace(r)
	mergeBranch = strings.TrimPrefix(strings.TrimSpace(m), "refs/heads/")
	return remote, mergeBranch
}

// RemoteHasBranch reports whether remote already has refs/heads/<branch>.
// ensureFeatureBranch uses this on every non-default-branch push (not just
// when local upstream config is missing or wrong): a prior push may have
// published the branch even when push -u failed to write .git/config, and
// the nonexistence lease must not be reasserted against a ref that already
// exists on the remote.
//
// This is existence-only, not tip equality. ensureFeatureBranch already
// covers the concurrent-creator race this could be mistaken for protecting:
// when the branch exists, it drops to a plain push and relies on Git's own
// fast-forward check as the safety net (see
// TestRunChangesPushDropsLeaseWhenBranchExistsOnRemote), so a survivor of a
// branch-name race with unrelated history is rejected there, not here. A
// tip-equality check here instead breaks the ordinary case: after publishing
// once, a second local commit makes HEAD outrun the remote tip, so this
// would report false, Push would reassert the empty-value
// force-with-lease=<branch>: (branch must not exist), and Git would reject
// the plain follow-up push with a stale-info error against a ref that
// plainly exists.
func RemoteHasBranch(ctx context.Context, cwd, remote, branch string, runGit Runner) (bool, error) {
	runGit, _ = resolveRunners(runGit, nil)
	remote = strings.TrimSpace(remote)
	branch = strings.TrimSpace(branch)
	if remote == "" || branch == "" {
		return false, nil
	}
	// Ask for the exact head ref rather than listing every branch. "--"
	// terminates option parsing so a remote shaped like an option stays
	// positional.
	ref := "refs/heads/" + branch
	out, err := gitOutput(ctx, runGit, cwd, "ls-remote", "--heads", "--", remote, ref)
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if _, got, ok := strings.Cut(line, "\t"); ok && strings.TrimSpace(got) == ref {
			return true, nil
		}
	}
	return false, nil
}

// UpstreamRef returns the full upstream reference name for branch (e.g.
// "origin/feature-1" or "upstream/main"), or the empty string if no upstream or
// the ref cannot be resolved. Callers that need to know whether Zero's own
// `push -u` published exactly <remote>/<branch> compare this string rather than
// reading branch.<name>.remote alone: with branch.autoSetupMerge=inherit,
// `git checkout -b` copies remote=origin and merge=refs/heads/main from the
// source branch before any push, so a remote config field is not publication
// state.
func UpstreamRef(ctx context.Context, cwd, branch string, runGit Runner) string {
	runGit, _ = resolveRunners(runGit, nil)
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return ""
	}
	out, err := gitOutput(ctx, runGit, cwd, "rev-parse", "--abbrev-ref", branch+"@{upstream}")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// HasUpstream reports whether branch has a published upstream tracking the same
// branch name on a remote (the relationship `git push -u <remote> <branch>`
// records). ensureFeatureBranch consults this when it is called again with a
// non-default current branch: a generated branch that just lost a
// force-with-lease race against a concurrent creator is left checked out
// locally without that relationship, so a retry must not treat it the same as
// an ordinary, already-published feature branch and drop the nonexistence
// lease.
//
// An inherited upstream to a different branch (branch.autoSetupMerge=inherit
// copies origin/main onto a new user/slug) is not publication state and reports
// false. Any failure to resolve the upstream (including "no upstream
// configured") also reports false so the caller keeps requiring the lease.
func HasUpstream(ctx context.Context, cwd, branch string, runGit Runner) (bool, error) {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return false, nil
	}
	ref := UpstreamRef(ctx, cwd, branch, runGit)
	if ref == "" {
		return false, nil
	}
	// rev-parse --abbrev-ref prints "<remote>/<branch>". Both remote names
	// (e.g. team/upstream) and branch names (e.g. user/slug) may contain
	// slashes, so verify that the upstream ref ends with "/" + branch and
	// has a non-empty remote prefix.
	suffix := "/" + branch
	if strings.HasSuffix(ref, suffix) && len(ref) > len(suffix) {
		return true, nil
	}
	return false, nil
}

// UpstreamRemote returns the configured upstream remote name for branch (e.g. "origin"),
// or "" if no upstream is configured. This alone is not proof of publication:
// branch.autoSetupMerge=inherit copies branch.<name>.remote from the source
// branch before any push. Prefer UpstreamRef when checking that <remote>/<branch>
// was actually published.
func UpstreamRemote(ctx context.Context, cwd, branch string, runGit Runner) string {
	runGit, _ = resolveRunners(runGit, nil)
	out, err := gitOutput(ctx, runGit, cwd, "config", "branch."+branch+".remote")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// DeleteBranch switches to fallbackBranch and deletes branchToDelete.
func DeleteBranch(ctx context.Context, cwd, fallbackBranch, branchToDelete string, runGit Runner) error {
	runGit, _ = resolveRunners(runGit, nil)
	if _, err := gitOutput(ctx, runGit, cwd, "switch", "--", fallbackBranch); err != nil {
		return err
	}
	_, err := gitOutput(ctx, runGit, cwd, "branch", "-D", "--", branchToDelete)
	return err
}

// CurrentBranchTip returns the SHA of the currently checked-out commit
// (HEAD^{commit}), or "" when HEAD is detached or unresolvable.
// ensureFeatureBranch uses this as the compare-and-swap old value when it
// restores the original default branch after creating a feature branch, so a
// concurrent advance of the default ref is refused instead of rewound.
func CurrentBranchTip(ctx context.Context, cwd string, runGit Runner) string {
	runGit, _ = resolveRunners(runGit, nil)
	out, err := gitOutput(ctx, runGit, cwd, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// CurrentBranch returns the short name of the currently checked-out branch, or
// "" when HEAD is detached / unresolvable. Callers that need the branch's
// configured upstream (for example the --yes unborn-remote preflight) use this
// instead of IsDefaultBranch so a remote-HEAD lookup failure does not block
// remote resolution.
func CurrentBranch(ctx context.Context, cwd string, runGit Runner) string {
	runGit, _ = resolveRunners(runGit, nil)
	out, err := gitOutput(ctx, runGit, cwd, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return ""
	}
	branch := strings.TrimSpace(out)
	if branch == "" || branch == "HEAD" {
		return ""
	}
	return branch
}

// ErrCompareAndSwapConflict reports that a ref changed since the expected tip
// was captured, so the compare-and-swap form of update-ref refused to move it.
// Callers distinguish this from an ordinary update-ref failure because it means
// someone else advanced the ref concurrently rather than that the write itself
// is broken, and destroying work to "recover" from it would be wrong.
var ErrCompareAndSwapConflict = errors.New("compare-and-swap conflict")

// checkedOutInWorktrees reports whether branch is currently checked out in any
// worktree across the repository, returning the path of the worktree if found.
func checkedOutInWorktrees(ctx context.Context, cwd, branch string, runGit Runner) (bool, string, error) {
	out, err := gitOutput(ctx, runGit, cwd, "worktree", "list", "--porcelain")
	if err != nil {
		return false, "", err
	}
	var currentWorktree string
	targetRef := "refs/heads/" + branch
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "worktree ") {
			currentWorktree = strings.TrimPrefix(line, "worktree ")
		} else if strings.HasPrefix(line, "branch ") {
			b := strings.TrimPrefix(line, "branch ")
			if b == targetRef || b == branch {
				return true, currentWorktree, nil
			}
		}
	}
	return false, "", nil
}

// ResetBranchRef points the local branch ref at newTip without checking the
// branch out. ensureFeatureBranch uses this after creating a feature branch
// that owns the publishable commits: the normal flow commits on the default
// branch first, then creates user/slug at the same HEAD, and without this
// restore the local default keeps those commits and diverges from the remote
// after a squash-merge. newTip is typically the remote-tracking ref
// (origin/main) that CommitsAhead already used. Refuses to move the currently
// checked-out branch so callers must have already switched to the feature
// branch.
func ResetBranchRef(ctx context.Context, cwd, branch, newTip string, runGit Runner, expectedOld ...string) error {
	runGit, _ = resolveRunners(runGit, nil)
	branch = strings.TrimSpace(branch)
	newTip = strings.TrimSpace(newTip)
	if branch == "" {
		return fmt.Errorf("branch name required")
	}
	if newTip == "" {
		return fmt.Errorf("new tip required")
	}
	// Branch becomes refs/heads/<branch>; reject traversal / absolute forms so
	// a hostile branch name cannot escape the heads namespace.
	if strings.Contains(branch, "..") || strings.HasPrefix(branch, "/") || strings.ContainsAny(branch, "\\ \t\n") {
		return fmt.Errorf("invalid branch name %q", branch)
	}
	if inUse, wtPath, wtErr := checkedOutInWorktrees(ctx, cwd, branch, runGit); wtErr == nil && inUse {
		return fmt.Errorf("refusing to move currently checked-out branch %q (checked out in worktree %s)", branch, wtPath)
	} else if wtErr != nil {
		headBranch, err := gitOutput(ctx, runGit, cwd, "rev-parse", "--abbrev-ref", "HEAD")
		if err == nil && strings.TrimSpace(headBranch) == branch {
			return fmt.Errorf("refusing to move currently checked-out branch %q", branch)
		}
	}
	tipSHA, err := gitOutput(ctx, runGit, cwd, "rev-parse", "--verify", newTip+"^{commit}")
	if err != nil {
		return fmt.Errorf("resolve tip %q: %w", newTip, err)
	}
	args := []string{"update-ref", "refs/heads/" + branch, strings.TrimSpace(tipSHA)}
	if len(expectedOld) > 0 && strings.TrimSpace(expectedOld[0]) != "" {
		args = append(args, strings.TrimSpace(expectedOld[0]))
	}
	if _, err := gitOutput(ctx, runGit, cwd, args...); err != nil {
		// With an expected-old value this is a compare-and-swap: the only way
		// git refuses is that the ref no longer matches, so report it as a
		// conflict the caller can recognize rather than a generic write error.
		if len(expectedOld) > 0 && strings.TrimSpace(expectedOld[0]) != "" {
			return fmt.Errorf("%w: update-ref refs/heads/%s: %v", ErrCompareAndSwapConflict, branch, err)
		}
		return fmt.Errorf("update-ref refs/heads/%s: %w", branch, err)
	}
	return nil
}

// IsUnbornRemote reports whether remote is a freshly created repository with
// no refs at all (no branches, no HEAD). ensureFeatureBranch consults this
// when CommitsAhead fails to determine why: a genuinely empty remote has no
// <remote>/<branch> tracking ref for CommitsAhead to diff against, and that
// is proof there is nothing published yet, not an unknown state to fail
// closed on. An error here (unreachable remote, timeout) leaves the state
// unconfirmed, so callers must treat that the same as a non-empty remote.
func IsUnbornRemote(ctx context.Context, cwd, remote string, runGit Runner) (bool, error) {
	runGit, _ = resolveRunners(runGit, nil)
	// "--" terminates option parsing so a remote value shaped like an option
	// (--upload-pack=/bin/echo) reaches Git as a positional argument.
	out, err := gitOutput(ctx, runGit, cwd, "ls-remote", "--heads", "--", remote)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "", nil
}

// currentUser is an indirection over os/user.Current so tests can force the
// final fallback tier of CurrentGitUser without depending on the host account.
var currentUser = user.Current

// CurrentGitUser resolves an identity to prefix generated branch names with:
// git config user.name, falling back to the OS account username, falling
// back to the literal "user" so BuildBranchName always gets a non-empty
// input.
func CurrentGitUser(ctx context.Context, cwd string, runGit Runner) string {
	runGit, _ = resolveRunners(runGit, nil)
	if name, err := gitOutput(ctx, runGit, cwd, "config", "user.name"); err == nil && name != "" {
		return name
	}
	if u, err := currentUser(); err == nil && u.Username != "" {
		return u.Username
	}
	return "user"
}

// slugComponentRe matches runs of characters not allowed in a branch-name
// component, so SlugifyBranchComponent can collapse them to a single hyphen.
var slugComponentRe = regexp.MustCompile(`[^a-z0-9]+`)

// maxSlugComponentLen caps a single branch-name component so generated names
// stay short and readable, matching the "username/feature-name" convention
// rather than sprawling into a full sentence.
const maxSlugComponentLen = 40

// SlugifyBranchComponent lowercases s and collapses any run of non
// alphanumeric characters into a single hyphen, trimming leading/trailing
// hyphens and capping length so the result is a safe, short branch-name
// component.
func SlugifyBranchComponent(s string) string {
	slug := slugComponentRe.ReplaceAllString(strings.ToLower(strings.TrimSpace(s)), "-")
	slug = strings.Trim(slug, "-")
	if len(slug) > maxSlugComponentLen {
		slug = strings.Trim(slug[:maxSlugComponentLen], "-")
	}
	return slug
}

// BuildBranchName composes a "<user>/<slug>" branch name from a git identity
// and a short feature slug (the convention used across Gitlawb tooling).
// Empty or unsafe inputs fall back to "user" and "changes" respectively so
// the result is always a valid, non-empty branch name.
func BuildBranchName(gitUser, slug string) string {
	userSlug := SlugifyBranchComponent(gitUser)
	if userSlug == "" {
		userSlug = "user"
	}
	featureSlug := SlugifyBranchComponent(slug)
	if featureSlug == "" {
		featureSlug = "changes"
	}
	return userSlug + "/" + featureSlug
}

type PROptions struct {
	Cwd   string
	Fill  bool
	Draft bool
	Title string
	Body  string
	RunGH Runner
}

type PRResult struct {
	Output string `json:"output"`
}

func CreatePR(ctx context.Context, options PROptions) (PRResult, error) {
	cwd, err := resolveCwd(options.Cwd)
	if err != nil {
		return PRResult{}, err
	}

	runGH := options.RunGH
	if runGH == nil {
		runGH = func(ctx context.Context, dir string, args ...string) (CommandResult, error) {
			cmd := exec.CommandContext(ctx, "gh", args...)
			cmd.Dir = dir
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			err := cmd.Run()

			var exitCode int
			if err != nil {
				if exitError, ok := err.(*exec.ExitError); ok {
					exitCode = exitError.ExitCode()
				} else {
					exitCode = -1
				}
			}
			return CommandResult{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: exitCode}, err
		}
	}

	prArgs := []string{"pr", "create"}
	if options.Fill {
		prArgs = append(prArgs, "--fill")
	}
	if options.Draft {
		prArgs = append(prArgs, "--draft")
	}
	if options.Title != "" {
		prArgs = append(prArgs, "--title", options.Title)
	}
	if options.Body != "" {
		prArgs = append(prArgs, "--body", options.Body)
	}

	res, err := runGH(ctx, cwd, prArgs...)
	if err != nil {
		return PRResult{}, fmt.Errorf("gh pr create failed: %w\n%s", err, res.Stderr)
	}

	return PRResult{
		Output: res.Stdout,
	}, nil
}
