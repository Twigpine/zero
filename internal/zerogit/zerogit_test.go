package zerogit

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Gitlawb/zero/internal/redaction"
)

func TestInspectSummarizesChangesAndRedactsDiff(t *testing.T) {
	root := t.TempDir()
	runner := &fakeRunner{results: []CommandResult{
		{Stdout: root + "\n"},
		{Stdout: "feature/m5\n"},
		{Stdout: "abc1234\n"},
		{Stdout: " M internal/verify/verify.go\x00?? internal/zerogit/zerogit.go\x00"},
		{Stdout: "abc1234\n"},
		{},
		{},
		{Stdout: " internal/verify/verify.go | 2 +-\n 1 file changed, 1 insertion(+), 1 deletion(-)\n"},
		{Stdout: "diff --git a/internal/verify/verify.go b/internal/verify/verify.go\n+token sk-proj-abcdefghijklmnopqrstuvwxyz0\n"},
	}}

	summary, err := Inspect(context.Background(), InspectOptions{
		Cwd:          root,
		MaxDiffBytes: 80,
		RunGit:       runner.Run,
	})
	if err != nil {
		t.Fatalf("Inspect returned error: %v", err)
	}

	if summary.Root != root || summary.Branch != "feature/m5" || summary.Commit != "abc1234" {
		t.Fatalf("unexpected git metadata: %#v", summary)
	}
	if summary.Clean {
		t.Fatalf("Clean = true, want false")
	}
	if len(summary.Files) != 2 {
		t.Fatalf("expected two changed files, got %#v", summary.Files)
	}
	if summary.Files[0].Path != "internal/verify/verify.go" || summary.Files[0].Status != "modified" || !summary.Files[0].Unstaged {
		t.Fatalf("unexpected modified file summary: %#v", summary.Files[0])
	}
	if summary.Files[1].Path != "internal/zerogit/zerogit.go" || summary.Files[1].Status != "untracked" || !summary.Files[1].Untracked {
		t.Fatalf("unexpected untracked file summary: %#v", summary.Files[1])
	}
	if strings.Contains(summary.Diff, "sk-proj-abcdefghijklmnopqrstuvwxyz0") || !strings.Contains(summary.Diff, "[REDACTED]") {
		t.Fatalf("expected redacted diff, got %q", summary.Diff)
	}
	if !summary.Truncated {
		t.Fatalf("expected diff to be marked truncated")
	}
	if got := runner.commandLine(3); got != "git status --porcelain -z --untracked-files=all" {
		t.Fatalf("status command = %q", got)
	}
	if got := runner.commandLine(6); got != "git add -A" {
		t.Fatalf("preview stage command = %q", got)
	}
	if got := runner.commandLine(7); got != "git diff --cached --stat --" {
		t.Fatalf("preview diff stat command = %q", got)
	}
}

func TestCommitStagesAllChangesAndUsesGeneratedMessage(t *testing.T) {
	root := t.TempDir()
	runner := &fakeRunner{results: []CommandResult{
		{Stdout: root + "\n"},
		{Stdout: "main\n"},
		{Stdout: "abc1234\n"},
		{Stdout: " M internal/verify/verify.go\x00?? internal/zerogit/zerogit.go\x00"},
		{Stdout: "abc1234\n"},
		{},
		{},
		{Stdout: " 2 files changed, 10 insertions(+)\n"},
		{Stdout: "diff --git a/internal/verify/verify.go b/internal/verify/verify.go\n"},
		{},
		{Stdout: "[main def5678] Update 2 files\n"},
		{Stdout: "def5678\n"},
	}}

	result, err := Commit(context.Background(), CommitOptions{
		Cwd:    root,
		RunGit: runner.Run,
	})
	if err != nil {
		t.Fatalf("Commit returned error: %v", err)
	}

	if !result.Committed || result.CommitHash != "def5678" {
		t.Fatalf("unexpected commit result: %#v", result)
	}
	if result.Message == "" || len(result.Message) > 72 || !strings.Contains(result.Message, "2 files") {
		t.Fatalf("unexpected generated commit message: %q", result.Message)
	}
	if got := runner.commandLine(9); got != "git add -A" {
		t.Fatalf("stage command = %q", got)
	}
	if got := runner.commandLine(10); !strings.HasPrefix(got, "git commit -m ") {
		t.Fatalf("commit command = %q", got)
	}
}

func TestCommitDryRunDoesNotMutateRepository(t *testing.T) {
	root := t.TempDir()
	runner := &fakeRunner{results: []CommandResult{
		{Stdout: root + "\n"},
		{Stdout: "main\n"},
		{Stdout: "abc1234\n"},
		{Stdout: " M README.md\x00"},
		{Stdout: "abc1234\n"},
		{},
		{},
		{Stdout: " README.md | 1 +\n"},
		{Stdout: "diff --git a/README.md b/README.md\n"},
	}}

	result, err := Commit(context.Background(), CommitOptions{
		Cwd:     root,
		Message: "Update README",
		DryRun:  true,
		RunGit:  runner.Run,
	})
	if err != nil {
		t.Fatalf("Commit dry-run returned error: %v", err)
	}

	if result.Committed || !result.DryRun || result.Message != "Update README" {
		t.Fatalf("unexpected dry-run result: %#v", result)
	}
	if len(runner.calls) != 9 {
		t.Fatalf("dry-run should only inspect changes, got calls %#v", runner.calls)
	}
}

func TestCommitRejectsCleanTreeAndInvalidMessage(t *testing.T) {
	root := t.TempDir()
	cleanRunner := &fakeRunner{results: []CommandResult{
		{Stdout: root + "\n"},
		{Stdout: "main\n"},
		{Stdout: "abc1234\n"},
		{Stdout: ""},
		{Stdout: "abc1234\n"},
		{},
		{},
		{Stdout: ""},
		{Stdout: ""},
	}}
	if _, err := Commit(context.Background(), CommitOptions{Cwd: root, Message: "Update", RunGit: cleanRunner.Run}); err == nil || !strings.Contains(err.Error(), "no changes") {
		t.Fatalf("expected clean tree error, got %v", err)
	}
	if err := ValidateMessage("   "); err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("expected message validation error, got %v", err)
	}
}

func TestInspectPreviewIncludesUntrackedOnlyChanges(t *testing.T) {
	root := initGitRepo(t, true)
	writeTestFile(t, filepath.Join(root, "notes.md"), "hello zero\n")

	summary, err := Inspect(context.Background(), InspectOptions{Cwd: root})
	if err != nil {
		t.Fatalf("Inspect returned error: %v", err)
	}

	if summary.Clean {
		t.Fatalf("Clean = true, want false")
	}
	if len(summary.Files) != 1 || summary.Files[0].Path != "notes.md" || !summary.Files[0].Untracked {
		t.Fatalf("unexpected untracked summary: %#v", summary.Files)
	}
	if !strings.Contains(summary.DiffStat, "notes.md") {
		t.Fatalf("diff stat does not include untracked file: %q", summary.DiffStat)
	}
	if !strings.Contains(summary.Diff, "diff --git a/notes.md b/notes.md") || !strings.Contains(summary.Diff, "+hello zero") {
		t.Fatalf("diff does not include untracked file content: %q", summary.Diff)
	}
	if staged := runGitCommand(t, root, "diff", "--cached", "--name-only"); strings.TrimSpace(staged) != "" {
		t.Fatalf("Inspect mutated the real index, staged files: %q", staged)
	}
}

func TestInspectPreviewWorksWithUnbornHead(t *testing.T) {
	root := initGitRepo(t, false)
	writeTestFile(t, filepath.Join(root, "README.md"), "new repository\n")

	summary, err := Inspect(context.Background(), InspectOptions{Cwd: root})
	if err != nil {
		t.Fatalf("Inspect returned error for unborn HEAD: %v", err)
	}

	if summary.Clean {
		t.Fatalf("Clean = true, want false")
	}
	if len(summary.Files) != 1 || summary.Files[0].Path != "README.md" || !summary.Files[0].Untracked {
		t.Fatalf("unexpected unborn HEAD summary: %#v", summary.Files)
	}
	if !strings.Contains(summary.DiffStat, "README.md") || !strings.Contains(summary.Diff, "+new repository") {
		t.Fatalf("unborn HEAD preview did not include README: stat=%q diff=%q", summary.DiffStat, summary.Diff)
	}
	if staged := runGitCommand(t, root, "diff", "--cached", "--name-only"); strings.TrimSpace(staged) != "" {
		t.Fatalf("Inspect mutated the real unborn index, staged files: %q", staged)
	}
}

func TestInspectBaseRefEmptyUsesSnapshotPath(t *testing.T) {
	root := t.TempDir()
	runner := &fakeRunner{results: []CommandResult{
		{Stdout: root + "\n"},
		{Stdout: "main\n"},
		{Stdout: "abc1234\n"},
		{Stdout: " M README.md\x00"},
		{Stdout: "abc1234\n"},
		{},
		{},
		{Stdout: " README.md | 1 +\n"},
		{Stdout: "diff --git a/README.md b/README.md\n"},
	}}

	summary, err := Inspect(context.Background(), InspectOptions{Cwd: root, RunGit: runner.Run})
	if err != nil {
		t.Fatalf("Inspect returned error: %v", err)
	}
	if summary.Base != "" {
		t.Fatalf("Base = %q, want empty for default path", summary.Base)
	}
	if got := runner.commandLine(3); got != "git status --porcelain -z --untracked-files=all" {
		t.Fatalf("default path must use git status, got %q", got)
	}
	if got := runner.commandLine(6); got != "git add -A" {
		t.Fatalf("default path must use snapshot index, got %q", got)
	}
	for _, call := range runner.calls {
		joined := strings.Join(call.args, " ")
		if strings.Contains(joined, "...HEAD") {
			t.Fatalf("default path must not issue a three-dot diff, saw %q", joined)
		}
	}
}

func TestInspectBaseRefRealGitDiffsBranchAgainstBase(t *testing.T) {
	root := initGitRepo(t, true)
	baseRef := runGitCommand(t, root, "rev-parse", "HEAD")
	runGitCommand(t, root, "checkout", "-q", "-b", "feature")
	writeTestFile(t, filepath.Join(root, "feature.md"), "branch only\n")
	runGitCommand(t, root, "add", "feature.md")
	runGitCommand(t, root, "-c", "user.name=Zero", "-c", "user.email=zero@example.invalid", "commit", "-m", "Add feature")

	summary, err := Inspect(context.Background(), InspectOptions{Cwd: root, BaseRef: strings.TrimSpace(baseRef)})
	if err != nil {
		t.Fatalf("Inspect returned error: %v", err)
	}
	if summary.Clean {
		t.Fatalf("Clean = true, want false")
	}
	if len(summary.Files) != 1 || summary.Files[0].Path != "feature.md" || summary.Files[0].Status != "added" {
		t.Fatalf("unexpected base diff files: %#v", summary.Files)
	}
	if summary.Branch != "feature" {
		t.Fatalf("Branch = %q, want feature (HEAD branch preserved)", summary.Branch)
	}
	if !strings.Contains(summary.Diff, "+branch only") {
		t.Fatalf("diff missing branch content: %q", summary.Diff)
	}
	if staged := runGitCommand(t, root, "diff", "--cached", "--name-only"); strings.TrimSpace(staged) != "" {
		t.Fatalf("Inspect mutated the real index, staged files: %q", staged)
	}
}

func TestInspectBaseRefUsesThreeDotDiff(t *testing.T) {
	root := t.TempDir()
	runner := &fakeRunner{results: []CommandResult{
		{Stdout: root + "\n"},            // rev-parse --show-toplevel
		{Stdout: "feature/m5\n"},         // rev-parse --abbrev-ref HEAD
		{Stdout: "abc1234\n"},            // rev-parse --short HEAD
		{Stdout: "M\ta.txt\nA\tb.txt\n"}, // diff --name-status main...HEAD
		{Stdout: " a.txt | 1 +\n b.txt | 1 +\n 2 files changed, 2 insertions(+)\n"},                                                      // diff --stat main...HEAD
		{Stdout: "diff --git a/internal/changes/changes.go b/internal/changes/changes.go\n+token sk-proj-abcdefghijklmnopqrstuvwxyz0\n"}, // diff main...HEAD
	}}

	summary, err := Inspect(context.Background(), InspectOptions{
		Cwd:          root,
		BaseRef:      "main",
		MaxDiffBytes: 80,
		RunGit:       runner.Run,
	})
	if err != nil {
		t.Fatalf("Inspect returned error: %v", err)
	}

	if summary.Base != "main" {
		t.Fatalf("Base = %q, want main", summary.Base)
	}
	if summary.Branch != "feature/m5" {
		t.Fatalf("Branch = %q, want feature/m5 (HEAD branch must be preserved)", summary.Branch)
	}
	if summary.Clean {
		t.Fatalf("Clean = true, want false")
	}
	if len(summary.Files) != 2 {
		t.Fatalf("expected two files from name-status, got %#v", summary.Files)
	}
	if summary.Files[0].Path != "a.txt" || summary.Files[0].Status != "modified" {
		t.Fatalf("unexpected first file: %#v", summary.Files[0])
	}
	if summary.Files[1].Path != "b.txt" || summary.Files[1].Status != "added" {
		t.Fatalf("unexpected second file: %#v", summary.Files[1])
	}
	if strings.Contains(summary.Diff, "sk-proj-abcdefghijklmnopqrstuvwxyz0") || !strings.Contains(summary.Diff, "[REDACTED]") {
		t.Fatalf("expected redacted diff, got %q", summary.Diff)
	}
	if !summary.Truncated {
		t.Fatalf("expected diff to be marked truncated")
	}
	if got := runner.commandLine(3); got != "git diff --name-status main...HEAD --" {
		t.Fatalf("name-status command = %q", got)
	}
	if got := runner.commandLine(4); got != "git diff --stat main...HEAD --" {
		t.Fatalf("stat command = %q", got)
	}
	if got := runner.commandLine(5); got != "git diff main...HEAD --" {
		t.Fatalf("diff command = %q", got)
	}
}

func TestInspectBaseRefEmptyDiffIsClean(t *testing.T) {
	root := t.TempDir()
	runner := &fakeRunner{results: []CommandResult{
		{Stdout: root + "\n"},
		{Stdout: "main\n"},
		{Stdout: "abc1234\n"},
		{Stdout: ""}, // diff --name-status (no changes vs base)
		{Stdout: ""}, // diff --stat
		{Stdout: ""}, // diff
	}}

	summary, err := Inspect(context.Background(), InspectOptions{Cwd: root, BaseRef: "main", RunGit: runner.Run})
	if err != nil {
		t.Fatalf("Inspect returned error: %v", err)
	}
	if !summary.Clean || len(summary.Files) != 0 {
		t.Fatalf("expected clean base diff, got %#v", summary)
	}
	if summary.Base != "main" {
		t.Fatalf("Base = %q, want main", summary.Base)
	}
}

func TestParseNameStatusRenameAndCopy(t *testing.T) {
	cases := []struct {
		name       string
		line       string
		wantPath   string
		wantStatus string
	}{
		{
			name:       "rename uses new path",
			line:       "R100\told.txt\tnew.txt",
			wantPath:   "new.txt",
			wantStatus: "renamed",
		},
		{
			name:       "copy uses destination path",
			line:       "C75\tsrc.txt\tdst.txt",
			wantPath:   "dst.txt",
			wantStatus: "copied",
		},
		{
			name:       "modify two-field no regression",
			line:       "M\ta.txt",
			wantPath:   "a.txt",
			wantStatus: "modified",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files := parseNameStatus(tc.line)
			if len(files) != 1 {
				t.Fatalf("expected 1 file entry, got %d: %#v", len(files), files)
			}
			if files[0].Path != tc.wantPath {
				t.Fatalf("Path = %q, want %q", files[0].Path, tc.wantPath)
			}
			if files[0].Status != tc.wantStatus {
				t.Fatalf("Status = %q, want %q", files[0].Status, tc.wantStatus)
			}
		})
	}
}

func TestTruncateStringHonorsMaxBytesWithRedactionMarker(t *testing.T) {
	value := strings.Repeat("a", 32) + redaction.RedactedSecret + strings.Repeat("b", 32)
	for maxBytes := 1; maxBytes < len(redaction.RedactedSecret)+len("\n[truncated]"); maxBytes++ {
		truncated, ok := truncateString(value, maxBytes)
		if !ok {
			t.Fatalf("truncateString truncated = false for maxBytes=%d", maxBytes)
		}
		if len(truncated) > maxBytes {
			t.Fatalf("truncateString returned %d bytes for maxBytes=%d: %q", len(truncated), maxBytes, truncated)
		}
	}
}

type fakeRunner struct {
	calls   []gitCall
	results []CommandResult
}

func (runner *fakeRunner) Run(ctx context.Context, dir string, args ...string) (CommandResult, error) {
	runner.calls = append(runner.calls, gitCall{dir: dir, args: append([]string{}, args...)})
	if len(runner.results) == 0 {
		return CommandResult{}, nil
	}
	result := runner.results[0]
	runner.results = runner.results[1:]
	return result, nil
}

func (runner *fakeRunner) commandLine(index int) string {
	if index >= len(runner.calls) {
		return ""
	}
	return "git " + strings.Join(runner.calls[index].args, " ")
}

type gitCall struct {
	dir  string
	args []string
}

func initGitRepo(t *testing.T, withCommit bool) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git unavailable: %v", err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	root := t.TempDir()
	runGitCommand(t, root, "init")
	if withCommit {
		writeTestFile(t, filepath.Join(root, "README.md"), "initial\n")
		runGitCommand(t, root, "add", "README.md")
		runGitCommand(t, root, "-c", "user.name=Zero", "-c", "user.email=zero@example.invalid", "commit", "-m", "Initial commit")
	}
	return root
}

func runGitCommand(t *testing.T, dir string, args ...string) string {
	t.Helper()
	ctx := context.Background()
	if deadline, ok := t.Deadline(); ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, deadline)
		defer cancel()
	}
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = dir
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, string(output))
	}
	return string(output)
}

// requireGitAtLeast skips the test when the installed Git predates the given
// version, for tests that rely on newer git features (for example
// branch.autoSetupMerge=inherit, added in 2.35).
func requireGitAtLeast(t *testing.T, wantMajor, wantMinor int) {
	t.Helper()
	out, err := exec.Command("git", "--version").Output()
	if err != nil {
		t.Skipf("cannot determine git version: %v", err)
	}
	version := strings.TrimSpace(string(out))
	rest, ok := strings.CutPrefix(version, "git version ")
	if !ok {
		t.Skipf("cannot parse git version %q", version)
	}
	var major, minor int
	if n, err := fmt.Sscanf(rest, "%d.%d", &major, &minor); err != nil || n != 2 {
		t.Skipf("cannot parse git version %q", version)
	}
	if major < wantMajor || (major == wantMajor && minor < wantMinor) {
		t.Skipf("git %d.%d+ required, have %s", wantMajor, wantMinor, version)
	}
}

func writeTestFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestValidateMessageCountsRunesNotBytes(t *testing.T) {
	// 72 multi-byte runes (é = 2 bytes = 144 bytes) is a valid subject; the old
	// byte-length check wrongly rejected it.
	subject := strings.Repeat("é", 72)
	if err := ValidateMessage(subject); err != nil {
		t.Fatalf("72-rune non-ASCII subject should be valid, got %v", err)
	}
	// 73 runes must still be rejected.
	if err := ValidateMessage(strings.Repeat("é", 73)); err == nil {
		t.Fatal("73-rune subject should be rejected")
	}
}

func TestParseStatusZHandlesRenamesAndSpecialPaths(t *testing.T) {
	// NUL-delimited `git status --porcelain -z` output: paths are verbatim (never
	// C-quoted) and a rename is `XY <dest>\0<src>`.
	status := strings.Join([]string{
		" M internal/a.go",  // modified in worktree only
		"R  new name.go",    // staged rename; next field is the source
		"old name.go",       // rename SOURCE — must be consumed, not its own entry
		"A  café.go",        // staged add, non-ASCII path (no octal escaping)
		"?? un tracked.txt", // untracked, embedded space
		"",                  // trailing empty field after the final NUL
	}, "\x00")

	files := parseStatus(status)
	if len(files) != 4 {
		t.Fatalf("expected 4 entries (rename source consumed), got %d: %#v", len(files), files)
	}

	if files[0].Path != "internal/a.go" || files[0].Staged || !files[0].Unstaged {
		t.Fatalf("unexpected modified entry: %#v", files[0])
	}
	// Destination of the rename, not the unsplit "new name.go -> old name.go".
	if files[1].Path != "new name.go" || !files[1].Staged {
		t.Fatalf("rename should report the destination path staged: %#v", files[1])
	}
	// Non-ASCII path arrives verbatim — no `"caf\303\251.go"` quoting/escaping.
	if files[2].Path != "café.go" || !files[2].Staged {
		t.Fatalf("non-ASCII path should be verbatim: %#v", files[2])
	}
	if files[3].Path != "un tracked.txt" || !files[3].Untracked {
		t.Fatalf("untracked path with space should be preserved: %#v", files[3])
	}
	for _, f := range files {
		if f.Path == "old name.go" {
			t.Fatalf("rename source must not surface as its own entry: %#v", files)
		}
	}
}

func TestPushBranchesToRemote(t *testing.T) {
	t.Run("HappyPath", func(t *testing.T) {
		root := t.TempDir()
		runner := &fakeRunner{results: []CommandResult{
			{Stdout: root + "\n"},
			{Stdout: "feat/some-feature\n"},
			{Stdout: "origin\n"},                                   // config branch.feat/some-feature.remote
			{Stdout: "ref: refs/heads/main\tHEAD\nabc123\tHEAD\n"}, // ls-remote --symref: default is main
			{Stdout: "Everything up-to-date\n"},
			{Stdout: "origin/feat/some-feature\n"}, // UpstreamRef after push -u
		}}

		result, err := Push(context.Background(), PushOptions{
			Cwd:    root,
			RunGit: runner.Run,
		})
		if err != nil {
			t.Fatalf("Push returned error: %v", err)
		}

		if result.Remote != "origin" || result.Branch != "feat/some-feature" || !strings.Contains(result.Output, "Everything up-to-date") {
			t.Fatalf("unexpected push result: %#v", result)
		}

		if got := runner.commandLine(4); got != "git push -u -- origin feat/some-feature" {
			t.Fatalf("unexpected push command: %q", got)
		}
	})

	t.Run("FlagsForceAndDryRun", func(t *testing.T) {
		root := t.TempDir()
		runner := &fakeRunner{results: []CommandResult{
			{Stdout: root + "\n"},
			{Stdout: "feat/some-feature\n"},
			{Stdout: "origin\n"},                                   // config branch.feat/some-feature.remote
			{Stdout: "ref: refs/heads/main\tHEAD\nabc123\tHEAD\n"}, // ls-remote --symref: default is main
			{Stdout: "Everything up-to-date\n"},
			{Stdout: "origin/feat/some-feature\n"}, // UpstreamRef after push -u
		}}

		_, err := Push(context.Background(), PushOptions{
			Cwd:    root,
			RunGit: runner.Run,
			Force:  true,
			DryRun: true,
		})
		if err != nil {
			t.Fatalf("Push returned error: %v", err)
		}

		if got := runner.commandLine(4); got != "git push --dry-run --force-with-lease -u -- origin feat/some-feature" {
			t.Fatalf("unexpected push command: %q", got)
		}
	})

	t.Run("DetachedHEAD", func(t *testing.T) {
		root := t.TempDir()
		runner := &fakeRunner{results: []CommandResult{
			{Stdout: root + "\n"},
			{Stdout: "HEAD\n"},
		}}

		_, err := Push(context.Background(), PushOptions{
			Cwd:    root,
			RunGit: runner.Run,
		})
		if err == nil {
			t.Fatal("expected error on detached HEAD push, got nil")
		}
		if !strings.Contains(err.Error(), "cannot push: not currently on a branch") {
			t.Fatalf("unexpected error message: %v", err)
		}
	})

	t.Run("RejectsDefaultBranch", func(t *testing.T) {
		// The conventional main/master name is only a fallback for when the
		// remote's actual default cannot be determined live or from the local
		// cache, so the remote is consulted (and found unreachable, with no
		// cached record either) before the name heuristic applies.
		for _, branch := range []string{"main", "master"} {
			root := t.TempDir()
			runner := &fakeRunner{results: []CommandResult{
				{Stdout: root + "\n"},
				{Stdout: branch + "\n"},
				{ExitCode: 1},                     // config branch.<branch>.remote unset → origin
				{ExitCode: 128, Stderr: "fatal:"}, // ls-remote fails
				{ExitCode: 1},                     // no local refs/remotes/origin/HEAD record
			}}

			_, err := Push(context.Background(), PushOptions{
				Cwd:    root,
				RunGit: runner.Run,
			})
			if err == nil {
				t.Fatalf("expected error when pushing %q, got nil", branch)
			}
			if !strings.Contains(err.Error(), "default/protected branch") {
				t.Fatalf("unexpected error for %q: %v", branch, err)
			}
		}
	})

	t.Run("AllowDefaultBranchWithYes", func(t *testing.T) {
		root := t.TempDir()
		runner := &fakeRunner{results: []CommandResult{
			{Stdout: root + "\n"},
			{Stdout: "main\n"},
			{Stdout: "origin\n"},
			{Stdout: "Everything up-to-date\n"},
			{Stdout: "origin/main\n"}, // UpstreamRef after push -u
		}}

		result, err := Push(context.Background(), PushOptions{
			Cwd:                    root,
			RunGit:                 runner.Run,
			AllowPushDefaultBranch: true,
		})
		if err != nil {
			t.Fatalf("Push returned error: %v", err)
		}
		if result.Branch != "main" {
			t.Fatalf("expected branch main, got %q", result.Branch)
		}
	})

	t.Run("FallbackRemoteToOrigin", func(t *testing.T) {
		root := t.TempDir()
		runner := &fakeRunner{results: []CommandResult{
			{Stdout: root + "\n"},
			{Stdout: "feat/some-feature\n"},
			{ExitCode: 1, Stderr: "error: no such section"},        // config lookup fails
			{Stdout: "ref: refs/heads/main\tHEAD\nabc123\tHEAD\n"}, // ls-remote --symref: default is main
			{Stdout: "Everything up-to-date\n"},
			{Stdout: "origin/feat/some-feature\n"}, // UpstreamRef after push -u
		}}

		result, err := Push(context.Background(), PushOptions{
			Cwd:    root,
			RunGit: runner.Run,
		})
		if err != nil {
			t.Fatalf("Push returned error: %v", err)
		}

		if result.Remote != "origin" {
			t.Fatalf("expected fallback remote to be origin, got: %q", result.Remote)
		}

		if got := runner.commandLine(4); got != "git push -u -- origin feat/some-feature" {
			t.Fatalf("unexpected push command: %q", got)
		}
	})

	t.Run("FailsWhenDefaultBranchCannotBeVerified", func(t *testing.T) {
		// Push's own fail-closed path: the remote lookup fails and no local
		// refs/remotes/<remote>/HEAD record exists, so Push must refuse with
		// guidance instead of pushing an unverifiable branch.
		root := t.TempDir()
		runner := &fakeRunner{results: []CommandResult{
			{Stdout: root + "\n"},
			{Stdout: "feat/some-feature\n"},
			{Stdout: "origin\n"},              // config branch.feat/some-feature.remote
			{ExitCode: 128, Stderr: "fatal:"}, // ls-remote fails
			{ExitCode: 1},                     // no local refs/remotes/origin/HEAD record
		}}

		_, err := Push(context.Background(), PushOptions{
			Cwd:    root,
			RunGit: runner.Run,
		})
		if err == nil || !strings.Contains(err.Error(), "use --yes to override") {
			t.Fatalf("expected fail-closed error, got %v", err)
		}
	})

	t.Run("RequireNewRemoteBranchGuardsAgainstConcurrentCreation", func(t *testing.T) {
		// CreateBranch's own remote-collision probe runs before this push, so
		// a concurrent creator of the same name in that window would
		// otherwise be silently fast-forwarded. RequireNewRemoteBranch closes
		// it with a zero-value --force-with-lease asserting the destination
		// still doesn't exist at push time.
		root := t.TempDir()
		runner := &fakeRunner{results: []CommandResult{
			{Stdout: root + "\n"},
			{Stdout: "alice/fix-typo\n"},
			{Stdout: "origin\n"},                                   // config branch.alice/fix-typo.remote
			{Stdout: "ref: refs/heads/main\tHEAD\nabc123\tHEAD\n"}, // ls-remote --symref: default is main
			{Stdout: "Everything up-to-date\n"},
			{Stdout: "origin/alice/fix-typo\n"}, // UpstreamRef after push -u
		}}

		_, err := Push(context.Background(), PushOptions{
			Cwd:                    root,
			RunGit:                 runner.Run,
			RequireNewRemoteBranch: true,
		})
		if err != nil {
			t.Fatalf("Push returned error: %v", err)
		}
		if got := runner.commandLine(4); got != "git push --force-with-lease=alice/fix-typo: -u -- origin alice/fix-typo" {
			t.Fatalf("unexpected push command: %q", got)
		}
	})

	t.Run("RecoversWhenPushUCannotWriteLocalUpstream", func(t *testing.T) {
		// git push -u can exit 0 after publishing the remote branch while
		// failing to write .git/config (config.lock). Push must recover via
		// branch --set-upstream-to rather than reporting a full success with
		// no local upstream.
		root := t.TempDir()
		runner := &fakeRunner{results: []CommandResult{
			{Stdout: root + "\n"},
			{Stdout: "user/slug\n"},
			{Stdout: "origin\n"},
			{Stdout: "ref: refs/heads/main\tHEAD\nabc123\tHEAD\n"},
			{Stdout: "To origin\n * [new branch] user/slug -> user/slug\n"},
			{ExitCode: 128, Stderr: "fatal: no upstream configured for branch 'user/slug'"}, // UpstreamRef missing
			{Stdout: ""},                   // branch --set-upstream-to=origin/user/slug user/slug
			{Stdout: "origin/user/slug\n"}, // UpstreamRef after recovery
		}}

		result, err := Push(context.Background(), PushOptions{
			Cwd:    root,
			RunGit: runner.Run,
		})
		if err != nil {
			t.Fatalf("Push returned error: %v", err)
		}
		if result.Remote != "origin" || result.Branch != "user/slug" {
			t.Fatalf("unexpected push result: %#v", result)
		}
		if got := runner.commandLine(6); got != "git branch --set-upstream-to=origin/user/slug user/slug" {
			t.Fatalf("expected set-upstream recovery, got %q", got)
		}
	})

	t.Run("SurfacesUpstreamWriteFailureWhenRecoveryFails", func(t *testing.T) {
		root := t.TempDir()
		runner := &fakeRunner{results: []CommandResult{
			{Stdout: root + "\n"},
			{Stdout: "user/slug\n"},
			{Stdout: "origin\n"},
			{Stdout: "ref: refs/heads/main\tHEAD\nabc123\tHEAD\n"},
			{Stdout: "To origin\n * [new branch] user/slug -> user/slug\n"},
			{ExitCode: 128, Stderr: "fatal: no upstream configured"},
			{ExitCode: 255, Stderr: "error: could not write config file .git/config: File exists"},
		}}

		result, err := Push(context.Background(), PushOptions{
			Cwd:    root,
			RunGit: runner.Run,
		})
		if err == nil {
			t.Fatal("expected upstream-write failure after successful remote publish")
		}
		if !strings.Contains(err.Error(), "push published origin/user/slug") || !strings.Contains(err.Error(), "failed to configure local upstream") {
			t.Fatalf("unexpected error: %v", err)
		}
		// Remote side did publish: result still carries remote/branch for callers.
		if result.Remote != "origin" || result.Branch != "user/slug" {
			t.Fatalf("expected partial result for published branch, got %#v", result)
		}
	})

	t.Run("SurfacesUpstreamStillWrongAfterRepair", func(t *testing.T) {
		root := t.TempDir()
		runner := &fakeRunner{results: []CommandResult{
			{Stdout: root + "\n"},
			{Stdout: "user/slug\n"},
			{Stdout: "origin\n"},
			{Stdout: "ref: refs/heads/main\tHEAD\nabc123\tHEAD\n"},
			{Stdout: "To origin\n * [new branch] user/slug -> user/slug\n"},
			{ExitCode: 128, Stderr: "fatal: no upstream configured"},
			{Stdout: ""},              // branch --set-upstream-to succeeds
			{Stdout: "origin/main\n"}, // but the upstream is still wrong
		}}

		_, err := Push(context.Background(), PushOptions{Cwd: root, RunGit: runner.Run})
		if err == nil || !strings.Contains(err.Error(), "local upstream is still not") {
			t.Fatalf("expected residual-mismatch error, got %v", err)
		}
	})

	t.Run("DryRunSkipsUpstreamVerification", func(t *testing.T) {
		// git push --dry-run -u does not publish and does not write
		// branch.<name>.remote/merge. Upstream repair must not run: it would
		// mutate config and fail for never-published branches.
		root := t.TempDir()
		runner := &fakeRunner{results: []CommandResult{
			{Stdout: root + "\n"},
			{Stdout: "user/slug\n"},
			{Stdout: "origin\n"},
			{Stdout: "ref: refs/heads/main\tHEAD\nabc123\tHEAD\n"},
			{Stdout: "To origin\n * [new branch] user/slug -> user/slug (dry run)\n"},
		}}

		result, err := Push(context.Background(), PushOptions{
			Cwd:    root,
			RunGit: runner.Run,
			DryRun: true,
		})
		if err != nil {
			t.Fatalf("Push dry-run returned error: %v", err)
		}
		if result.Remote != "origin" || result.Branch != "user/slug" {
			t.Fatalf("unexpected push result: %#v", result)
		}
		if got := runner.commandLine(4); got != "git push --dry-run -u -- origin user/slug" {
			t.Fatalf("unexpected push command: %q", got)
		}
		for i := range runner.calls {
			if strings.Contains(runner.commandLine(i), "branch --set-upstream-to") {
				t.Fatalf("dry run must not attempt upstream repair: %q", runner.commandLine(i))
			}
		}
		if len(runner.calls) != 5 {
			t.Fatalf("expected only pre-push setup + dry-run push (5 calls), got %d", len(runner.calls))
		}
	})

	t.Run("DirectURLRemoteSucceedsWithoutUpstreamVerification", func(t *testing.T) {
		root := t.TempDir()
		directURL := "https://github.com/org/repo.git"
		runner := &fakeRunner{results: []CommandResult{
			{Stdout: root + "\n"},
			{Stdout: "feat/some-feature\n"},
			{Stdout: "ref: refs/heads/main\tHEAD\nabc123\tHEAD\n"}, // ls-remote --symref: default is main
			{Stdout: "Everything up-to-date\n"},
		}}

		result, err := Push(context.Background(), PushOptions{
			Cwd:    root,
			Remote: directURL,
			RunGit: runner.Run,
		})
		if err != nil {
			t.Fatalf("Push returned error: %v", err)
		}
		if result.Remote != directURL || result.Branch != "feat/some-feature" {
			t.Fatalf("unexpected push result: %#v", result)
		}
	})
}

func TestIsNamedRemote(t *testing.T) {
	runner := &fakeRunner{results: []CommandResult{
		{Stdout: "origin\nupstream\n"},
	}}
	if !IsNamedRemote(context.Background(), "/repo", "origin", runner.Run) {
		t.Fatal("origin should be a named remote")
	}
	if IsNamedRemote(context.Background(), "/repo", "https://github.com/org/repo.git", runner.Run) {
		t.Fatal("https URL must not be a named remote")
	}
	if IsNamedRemote(context.Background(), "/repo", "git@github.com:org/repo.git", runner.Run) {
		t.Fatal("git@ URL must not be a named remote")
	}
	if IsNamedRemote(context.Background(), "/repo", "/path/to/repo", runner.Run) {
		t.Fatal("path must not be a named remote")
	}
	if IsNamedRemote(context.Background(), "/repo", `C:\path\to\repo`, runner.Run) {
		t.Fatal("Windows path must not be a named remote")
	}
}

func TestRefreshTrackingRefSkipsDirectURLOrPath(t *testing.T) {
	runner := &fakeRunner{}
	if err := RefreshTrackingRef(context.Background(), "/repo", "https://github.com/org/repo.git", "main", runner.Run); err != nil {
		t.Fatalf("unexpected error for URL remote: %v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("expected no git calls for URL remote, got %d", len(runner.calls))
	}
}

func TestCreatePRCommandConstruction(t *testing.T) {
	t.Run("CreatePRWithAllOptions", func(t *testing.T) {
		root := t.TempDir()
		runner := &fakeRunner{results: []CommandResult{
			{Stdout: "https://github.com/Gitlawb/zero/pull/123\n"},
		}}

		result, err := CreatePR(context.Background(), PROptions{
			Cwd:   root,
			Fill:  true,
			Draft: true,
			Title: "Feat: some title",
			Body:  "Some body description",
			RunGH: runner.Run,
		})
		if err != nil {
			t.Fatalf("CreatePR returned error: %v", err)
		}

		if result.Output != "https://github.com/Gitlawb/zero/pull/123\n" {
			t.Fatalf("unexpected PR result: %#v", result)
		}

		expectedArgs := []string{"pr", "create", "--fill", "--draft", "--title", "Feat: some title", "--body", "Some body description"}
		if len(runner.calls) != 1 {
			t.Fatalf("expected 1 runner call, got %d", len(runner.calls))
		}
		if got := runner.calls[0].args; !reflect.DeepEqual(got, expectedArgs) {
			t.Fatalf("unexpected gh args: %v, want %v", got, expectedArgs)
		}
		if runner.calls[0].dir != root {
			t.Fatalf("unexpected dir: %q, want %q", runner.calls[0].dir, root)
		}
	})

	t.Run("CreatePRMinimal", func(t *testing.T) {
		root := t.TempDir()
		runner := &fakeRunner{results: []CommandResult{
			{Stdout: "https://github.com/Gitlawb/zero/pull/124\n"},
		}}

		_, err := CreatePR(context.Background(), PROptions{
			Cwd:   root,
			RunGH: runner.Run,
		})
		if err != nil {
			t.Fatalf("CreatePR returned error: %v", err)
		}

		expectedArgs := []string{"pr", "create"}
		if len(runner.calls) != 1 {
			t.Fatalf("expected 1 runner call, got %d", len(runner.calls))
		}
		if got := runner.calls[0].args; !reflect.DeepEqual(got, expectedArgs) {
			t.Fatalf("unexpected gh args: %v, want %v", got, expectedArgs)
		}
		if runner.calls[0].dir != root {
			t.Fatalf("unexpected dir: %q, want %q", runner.calls[0].dir, root)
		}
	})
}

func TestCreateBranch(t *testing.T) {
	t.Run("HappyPath", func(t *testing.T) {
		root := t.TempDir()
		runner := &fakeRunner{results: []CommandResult{
			{Stdout: root + "\n"},
			{ExitCode: 1}, // rev-parse --verify: no local branch by that name yet
			{Stdout: "Switched to a new branch 'alice/fix-typo'\n"},
		}}

		result, err := CreateBranch(context.Background(), BranchOptions{
			Cwd:    root,
			Name:   "alice/fix-typo",
			RunGit: runner.Run,
		})
		if err != nil {
			t.Fatalf("CreateBranch returned error: %v", err)
		}
		if result.Branch != "alice/fix-typo" {
			t.Fatalf("unexpected branch: %#v", result)
		}
		if got := runner.commandLine(1); got != "git rev-parse --verify --quiet refs/heads/alice/fix-typo" {
			t.Fatalf("unexpected existence-check command: %q", got)
		}
		if got := runner.commandLine(2); got != "git checkout -b alice/fix-typo --" {
			t.Fatalf("unexpected checkout command: %q", got)
		}
	})

	t.Run("SuffixesNameInsteadOfCheckingOutExistingBranch", func(t *testing.T) {
		// An existing branch under the generated name may hold entirely
		// unrelated history (an earlier push under the same low-entropy
		// name). Checking it out would publish that stale branch and leave
		// the new commit behind on the default branch, so CreateBranch must
		// pick a fresh suffixed name at the current HEAD instead.
		root := t.TempDir()
		runner := &fakeRunner{results: []CommandResult{
			{Stdout: root + "\n"},
			{Stdout: "abc1234\n"}, // rev-parse --verify: alice/fix-typo already exists
			{ExitCode: 1},         // rev-parse --verify: alice/fix-typo-2 is free
			{Stdout: "Switched to a new branch 'alice/fix-typo-2'\n"},
		}}

		result, err := CreateBranch(context.Background(), BranchOptions{
			Cwd:    root,
			Name:   "alice/fix-typo",
			RunGit: runner.Run,
		})
		if err != nil {
			t.Fatalf("CreateBranch returned error: %v", err)
		}
		if result.Branch != "alice/fix-typo-2" {
			t.Fatalf("unexpected branch: %#v", result)
		}
		if got := runner.commandLine(3); got != "git checkout -b alice/fix-typo-2 --" {
			t.Fatalf("expected a fresh suffixed branch, got %q", got)
		}
	})

	t.Run("FailsVisiblyWhenSuffixNamespaceExhausted", func(t *testing.T) {
		root := t.TempDir()
		results := []CommandResult{{Stdout: root + "\n"}}
		for i := 0; i < 9; i++ {
			results = append(results, CommandResult{Stdout: "abc1234\n"}) // every candidate exists
		}
		runner := &fakeRunner{results: results}

		_, err := CreateBranch(context.Background(), BranchOptions{
			Cwd:    root,
			Name:   "alice/fix-typo",
			RunGit: runner.Run,
		})
		if err == nil || !strings.Contains(err.Error(), "already exists") {
			t.Fatalf("expected a visible exhaustion error, got %v", err)
		}
	})

	t.Run("PropagatesCheckoutFailureForNewBranch", func(t *testing.T) {
		root := t.TempDir()
		runner := &fakeRunner{results: []CommandResult{
			{Stdout: root + "\n"},
			{ExitCode: 1}, // rev-parse --verify: no local branch by that name yet
			{ExitCode: 128, Stderr: "fatal: unable to write new index file"},
		}}

		_, err := CreateBranch(context.Background(), BranchOptions{
			Cwd:    root,
			Name:   "alice/fix-typo",
			RunGit: runner.Run,
		})
		if err == nil || !strings.Contains(err.Error(), "unable to write new index file") {
			t.Fatalf("expected wrapped checkout failure, got %v", err)
		}
	})

	t.Run("DryRunDoesNotCheckout", func(t *testing.T) {
		root := t.TempDir()
		runner := &fakeRunner{results: []CommandResult{
			{Stdout: root + "\n"},
		}}

		result, err := CreateBranch(context.Background(), BranchOptions{
			Cwd:    root,
			Name:   "alice/fix-typo",
			DryRun: true,
			RunGit: runner.Run,
		})
		if err != nil {
			t.Fatalf("CreateBranch returned error: %v", err)
		}
		if result.Branch != "alice/fix-typo" {
			t.Fatalf("unexpected branch: %#v", result)
		}
		if len(runner.calls) != 1 {
			t.Fatalf("expected only the toplevel lookup call, got %d calls", len(runner.calls))
		}
	})

	t.Run("RequiresName", func(t *testing.T) {
		root := t.TempDir()
		runner := &fakeRunner{results: []CommandResult{
			{Stdout: root + "\n"},
		}}

		_, err := CreateBranch(context.Background(), BranchOptions{
			Cwd:    root,
			Name:   "",
			RunGit: runner.Run,
		})
		if err == nil {
			t.Fatal("expected error for empty branch name, got nil")
		}
	})

	for _, tc := range []struct{ name, branch string }{
		{"RejectsLeadingDash", "-feature"},
		{"RejectsLeadingSlash", "/feature"},
		{"RejectsDotDot", "feat..ure"},
		{"RejectsBackslash", `feat\ure`},
		{"RejectsSpace", "feat ure"},
		{"RejectsTab", "feat\ture"},
		{"RejectsNewline", "feat\nure"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			runner := &fakeRunner{results: []CommandResult{
				{Stdout: root + "\n"},
			}}
			_, err := CreateBranch(context.Background(), BranchOptions{
				Cwd:    root,
				Name:   tc.branch,
				RunGit: runner.Run,
			})
			if err == nil {
				t.Fatalf("expected error for branch name %q, got nil", tc.branch)
			}
		})
	}
}

func TestIsDefaultBranch(t *testing.T) {
	t.Run("ResolvesCurrentBranchByConventionalName", func(t *testing.T) {
		// The live symref confirms main is genuinely the remote's default here;
		// the conventional-name fallback below only applies when the remote
		// can't answer at all (see FallbackToConventionalNameWhenRemoteUnknown).
		root := t.TempDir()
		runner := &fakeRunner{results: []CommandResult{
			{Stdout: root + "\n"},
			{Stdout: "main\n"},
			{ExitCode: 1}, // config branch.main.remote unset → origin
			{Stdout: "ref: refs/heads/main\tHEAD\nabc123\tHEAD\n"}, // ls-remote --symref: default is main
		}}

		isDefault, branch, remote, err := IsDefaultBranch(context.Background(), DefaultBranchOptions{
			Cwd:    root,
			RunGit: runner.Run,
		})
		if err != nil {
			t.Fatalf("IsDefaultBranch returned error: %v", err)
		}
		if !isDefault || branch != "main" || remote != "origin" {
			t.Fatalf("unexpected result: isDefault=%v branch=%q remote=%q", isDefault, branch, remote)
		}
	})

	t.Run("FallbackToConventionalNameWhenRemoteUnknown", func(t *testing.T) {
		// The conventional main/master name only decides the result when the
		// remote's actual default genuinely cannot be determined, live or
		// cached.
		root := t.TempDir()
		runner := &fakeRunner{results: []CommandResult{
			{Stdout: root + "\n"},
			{Stdout: "main\n"},
			{ExitCode: 1},                     // config branch.main.remote unset → origin
			{ExitCode: 128, Stderr: "fatal:"}, // ls-remote fails
			{ExitCode: 1},                     // no local refs/remotes/origin/HEAD record
		}}

		isDefault, branch, remote, err := IsDefaultBranch(context.Background(), DefaultBranchOptions{
			Cwd:    root,
			RunGit: runner.Run,
		})
		if err != nil {
			t.Fatalf("IsDefaultBranch returned error: %v", err)
		}
		if !isDefault || branch != "main" || remote != "origin" {
			t.Fatalf("unexpected result: isDefault=%v branch=%q remote=%q", isDefault, branch, remote)
		}
	})

	// This is the regression test for jatmn's P2 finding: resolve main/master
	// against the selected remote before treating it as protected. A
	// repository whose remote default is genuinely "trunk" can have a
	// legitimate non-default local "main" (e.g. tracking origin/trunk); the
	// live symref result must win over the conventional-name fallback.
	t.Run("NonDefaultLocalMainTrackingADifferentRemoteDefault", func(t *testing.T) {
		root := t.TempDir()
		runner := &fakeRunner{results: []CommandResult{
			{Stdout: root + "\n"},
			{Stdout: "main\n"},
			{Stdout: "origin\n"}, // config branch.main.remote
			{Stdout: "ref: refs/heads/trunk\tHEAD\nabc123\tHEAD\n"}, // ls-remote --symref: default is trunk
		}}

		isDefault, branch, remote, err := IsDefaultBranch(context.Background(), DefaultBranchOptions{
			Cwd:    root,
			RunGit: runner.Run,
		})
		if err != nil {
			t.Fatalf("IsDefaultBranch returned error: %v", err)
		}
		if isDefault {
			t.Fatal("local main tracking a remote whose live default is trunk must not be treated as protected")
		}
		if branch != "main" || remote != "origin" {
			t.Fatalf("unexpected result: branch=%q remote=%q", branch, remote)
		}
	})

	t.Run("ResolvesRemoteFromBranchUpstream", func(t *testing.T) {
		// A fork setup where the current branch tracks "upstream" must
		// resolve and report that remote, not "origin": callers thread it
		// into Push so a freshly created feature branch (which has no
		// tracking configuration yet) still targets the right remote.
		root := t.TempDir()
		runner := &fakeRunner{results: []CommandResult{
			{Stdout: root + "\n"},
			{Stdout: "upstream\n"}, // config branch.feat/some-feature.remote
			{Stdout: "ref: refs/heads/main\tHEAD\nabc123\tHEAD\n"}, // ls-remote --symref against upstream
		}}

		isDefault, branch, remote, err := IsDefaultBranch(context.Background(), DefaultBranchOptions{
			Cwd:    root,
			Branch: "feat/some-feature",
			RunGit: runner.Run,
		})
		if err != nil {
			t.Fatalf("IsDefaultBranch returned error: %v", err)
		}
		if isDefault || branch != "feat/some-feature" || remote != "upstream" {
			t.Fatalf("unexpected result: isDefault=%v branch=%q remote=%q", isDefault, branch, remote)
		}
		if got := runner.commandLine(2); got != "git ls-remote --symref -- upstream HEAD" {
			t.Fatalf("expected lookup against the resolved remote, got %q", got)
		}
	})

	t.Run("FallsBackToLocalRemoteHeadRecord", func(t *testing.T) {
		// When the remote lookup fails (offline, slow), the local
		// refs/remotes/<remote>/HEAD record answers without a network.
		root := t.TempDir()
		runner := &fakeRunner{results: []CommandResult{
			{Stdout: root + "\n"},
			{ExitCode: 1},                           // config lookup fails → origin
			{ExitCode: 128, Stderr: "fatal:"},       // ls-remote fails
			{Stdout: "refs/remotes/origin/trunk\n"}, // local record: default is trunk
		}}

		isDefault, branch, remote, err := IsDefaultBranch(context.Background(), DefaultBranchOptions{
			Cwd:    root,
			Branch: "trunk",
			RunGit: runner.Run,
		})
		if err != nil {
			t.Fatalf("IsDefaultBranch returned error: %v", err)
		}
		if !isDefault || branch != "trunk" || remote != "origin" {
			t.Fatalf("unexpected result: isDefault=%v branch=%q remote=%q", isDefault, branch, remote)
		}
	})

	t.Run("FailsClosedWhenDefaultBranchUnknown", func(t *testing.T) {
		// Before this, a lookup timeout silently downgraded the check to the
		// main/master name heuristic, so a repository whose default is trunk
		// lost the confirmation guard exactly when the remote was slow. An
		// unknown default must now surface as an error, not as "not default".
		root := t.TempDir()
		runner := &fakeRunner{results: []CommandResult{
			{Stdout: root + "\n"},
			{ExitCode: 1},                     // config lookup fails → origin
			{ExitCode: 128, Stderr: "fatal:"}, // ls-remote fails
			{ExitCode: 1},                     // no local refs/remotes/origin/HEAD record
		}}

		_, _, _, err := IsDefaultBranch(context.Background(), DefaultBranchOptions{
			Cwd:    root,
			Branch: "trunk",
			RunGit: runner.Run,
		})
		if err == nil || !strings.Contains(err.Error(), "default branch for remote") {
			t.Fatalf("expected fail-closed error, got %v", err)
		}
	})

	t.Run("StaleCachedRemoteHeadDoesNotClearGuard", func(t *testing.T) {
		// The server renamed its default from main to trunk, but the local
		// refs/remotes/origin/HEAD cache still names main. With the live lookup
		// failing, a cache that does not match the branch is not evidence the
		// branch is unprotected: the check must fail closed, not report "trunk
		// is not the default" and let the push through without --yes.
		root := t.TempDir()
		runner := &fakeRunner{results: []CommandResult{
			{Stdout: root + "\n"},
			{ExitCode: 1},                          // config lookup fails → origin
			{ExitCode: 128, Stderr: "fatal:"},      // ls-remote fails
			{Stdout: "refs/remotes/origin/main\n"}, // stale cache: still says main
		}}

		_, _, _, err := IsDefaultBranch(context.Background(), DefaultBranchOptions{
			Cwd:    root,
			Branch: "trunk",
			RunGit: runner.Run,
		})
		if err == nil || !strings.Contains(err.Error(), "default branch for remote") {
			t.Fatalf("expected fail-closed error on stale cache mismatch, got %v", err)
		}
	})
}

func TestCommitsAhead(t *testing.T) {
	t.Run("CountsCommitsAheadOfRemoteDefault", func(t *testing.T) {
		root := t.TempDir()
		runner := &fakeRunner{results: []CommandResult{
			{Stdout: "3\n"},
		}}
		count, err := CommitsAhead(context.Background(), root, "origin", "main", runner.Run)
		if err != nil {
			t.Fatalf("CommitsAhead returned error: %v", err)
		}
		if count != 3 {
			t.Fatalf("count = %d, want 3", count)
		}
		if got := runner.commandLine(0); got != "git rev-list --count origin/main..HEAD" {
			t.Fatalf("unexpected rev-list command: %q", got)
		}
	})

	t.Run("CountsCommitsAheadOfDirectRemoteViaLsRemote", func(t *testing.T) {
		root := t.TempDir()
		runner := &fakeRunner{results: []CommandResult{
			{Stdout: "abc1234\trefs/heads/main\n"}, // ls-remote succeeds
			{Stdout: "2\n"},
		}}
		count, err := CommitsAhead(context.Background(), root, "https://github.com/example/repo.git", "main", runner.Run)
		if err != nil {
			t.Fatalf("CommitsAhead returned error: %v", err)
		}
		if count != 2 {
			t.Fatalf("count = %d, want 2", count)
		}
		if got := runner.commandLine(0); got != "git ls-remote --heads -- https://github.com/example/repo.git refs/heads/main" {
			t.Fatalf("unexpected ls-remote command: %q", got)
		}
		if got := runner.commandLine(1); got != "git rev-list --count abc1234..HEAD" {
			t.Fatalf("unexpected rev-list command: %q", got)
		}
	})

	t.Run("CountsCommitsAheadOfUnbornDirectRemote", func(t *testing.T) {
		root := t.TempDir()
		runner := &fakeRunner{results: []CommandResult{
			{Stdout: ""}, // ls-remote returns empty (no heads)
			{Stdout: "5\n"},
		}}
		count, err := CommitsAhead(context.Background(), root, "/path/to/bare.git", "main", runner.Run)
		if err != nil {
			t.Fatalf("CommitsAhead returned error: %v", err)
		}
		if count != 5 {
			t.Fatalf("count = %d, want 5", count)
		}
		if got := runner.commandLine(0); got != "git ls-remote --heads -- /path/to/bare.git refs/heads/main" {
			t.Fatalf("unexpected ls-remote command: %q", got)
		}
		if got := runner.commandLine(1); got != "git rev-list --count HEAD" {
			t.Fatalf("unexpected rev-list command: %q", got)
		}
	})

	t.Run("ReturnsErrorWhenRemoteTrackingRefMissing", func(t *testing.T) {
		root := t.TempDir()
		runner := &fakeRunner{results: []CommandResult{
			{ExitCode: 128, Stderr: "fatal: ambiguous argument 'origin/main..HEAD'"},
		}}
		if _, err := CommitsAhead(context.Background(), root, "origin", "main", runner.Run); err == nil {
			t.Fatal("expected an error when the remote-tracking ref is missing")
		}
	})
}

func TestResolveRemoteBranchTip(t *testing.T) {
	t.Run("ResolvesNamedRemoteFromTrackingRef", func(t *testing.T) {
		root := t.TempDir()
		runner := &fakeRunner{results: []CommandResult{
			{Stdout: "abc123commit\n"},
		}}
		sha, err := ResolveRemoteBranchTip(context.Background(), root, "origin", "main", runner.Run)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if sha != "abc123commit" {
			t.Fatalf("got sha %q, want %q", sha, "abc123commit")
		}
		if got := runner.commandLine(0); got != "git rev-parse --verify refs/remotes/origin/main^{commit}" {
			t.Fatalf("unexpected command: %q", got)
		}
	})

	t.Run("ResolvesDirectURLViaLsRemote", func(t *testing.T) {
		root := t.TempDir()
		runner := &fakeRunner{results: []CommandResult{
			{Stdout: "def456commit\trefs/heads/main\n"},
		}}
		sha, err := ResolveRemoteBranchTip(context.Background(), root, "https://github.com/example/repo.git", "main", runner.Run)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if sha != "def456commit" {
			t.Fatalf("got sha %q, want %q", sha, "def456commit")
		}
		if got := runner.commandLine(0); got != "git ls-remote --heads -- https://github.com/example/repo.git refs/heads/main" {
			t.Fatalf("unexpected command: %q", got)
		}
	})

	t.Run("ResolvesDirectPathViaLsRemote", func(t *testing.T) {
		root := t.TempDir()
		runner := &fakeRunner{results: []CommandResult{
			{Stdout: "7890abccommit\trefs/heads/main\n"},
		}}
		sha, err := ResolveRemoteBranchTip(context.Background(), root, "/path/to/bare.git", "main", runner.Run)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if sha != "7890abccommit" {
			t.Fatalf("got sha %q, want %q", sha, "7890abccommit")
		}
		if got := runner.commandLine(0); got != "git ls-remote --heads -- /path/to/bare.git refs/heads/main" {
			t.Fatalf("unexpected command: %q", got)
		}
	})

	t.Run("FailsWhenDirectRemoteHasNoMatchingBranch", func(t *testing.T) {
		root := t.TempDir()
		runner := &fakeRunner{results: []CommandResult{
			{Stdout: ""},
		}}
		if _, err := ResolveRemoteBranchTip(context.Background(), root, "/path/to/bare.git", "main", runner.Run); err == nil {
			t.Fatal("expected error when direct remote has no branch")
		}
	})

	t.Run("FailsWhenLsRemoteErrors", func(t *testing.T) {
		root := t.TempDir()
		runner := &fakeRunner{results: []CommandResult{
			{ExitCode: 128, Stderr: "fatal: repository not found"},
		}}
		if _, err := ResolveRemoteBranchTip(context.Background(), root, "https://github.com/example/missing.git", "main", runner.Run); err == nil {
			t.Fatal("expected error when ls-remote fails")
		}
	})

	t.Run("FailsWhenRevParseErrorsOnNamedRemote", func(t *testing.T) {
		root := t.TempDir()
		runner := &fakeRunner{results: []CommandResult{
			{ExitCode: 128, Stderr: "fatal: Needed a single revision"},
		}}
		if _, err := ResolveRemoteBranchTip(context.Background(), root, "origin", "main", runner.Run); err == nil {
			t.Fatal("expected error when rev-parse fails")
		}
	})
}

func TestCurrentGitUser(t *testing.T) {
	root := t.TempDir()
	runner := &fakeRunner{results: []CommandResult{
		{Stdout: "Alex Example\n"},
	}}

	if got := CurrentGitUser(context.Background(), root, runner.Run); got != "Alex Example" {
		t.Fatalf("CurrentGitUser = %q, want %q", got, "Alex Example")
	}
	if got := runner.commandLine(0); got != "git config user.name" {
		t.Fatalf("unexpected command: %q", got)
	}
}

// TestCurrentGitUserFallsBackToOSUsername covers the second of CurrentGitUser's
// three fallback tiers: when `git config user.name` fails or returns nothing,
// it falls back to the OS account username. The third tier (the literal
// "user") is covered by TestCurrentGitUserFallsBackToLiteralUser via the
// currentUser seam.
func TestCurrentGitUserFallsBackToOSUsername(t *testing.T) {
	root := t.TempDir()
	want, err := user.Current()
	if err != nil || want.Username == "" {
		t.Skip("no OS user available to compare against in this environment")
	}

	cases := map[string][]CommandResult{
		"ConfigCommandErrors": {{ExitCode: 1, Stderr: "fatal: unable to read config"}},
		"ConfigCommandEmpty":  {{Stdout: ""}},
	}
	for name, results := range cases {
		t.Run(name, func(t *testing.T) {
			runner := &fakeRunner{results: results}
			if got := CurrentGitUser(context.Background(), root, runner.Run); got != want.Username {
				t.Fatalf("CurrentGitUser = %q, want OS username %q", got, want.Username)
			}
			if got := runner.commandLine(0); got != "git config user.name" {
				t.Fatalf("unexpected command: %q", got)
			}
		})
	}
}

// TestCurrentGitUserFallsBackToLiteralUser covers the final fallback tier: when
// both `git config user.name` and the OS account lookup fail, CurrentGitUser
// must return the literal "user" so BuildBranchName always gets a non-empty
// identity to prefix generated branches with.
func TestCurrentGitUserFallsBackToLiteralUser(t *testing.T) {
	original := currentUser
	currentUser = func() (*user.User, error) { return nil, errors.New("no OS user") }
	t.Cleanup(func() { currentUser = original })

	root := t.TempDir()
	runner := &fakeRunner{results: []CommandResult{
		{ExitCode: 1, Stderr: "fatal: unable to read config"},
	}}
	if got := CurrentGitUser(context.Background(), root, runner.Run); got != "user" {
		t.Fatalf("CurrentGitUser = %q, want %q", got, "user")
	}
	if got := runner.commandLine(0); got != "git config user.name" {
		t.Fatalf("unexpected command: %q", got)
	}
}

func TestSlugifyBranchComponent(t *testing.T) {
	cases := map[string]string{
		"Fix Typo In README":      "fix-typo-in-readme",
		"  leading/trailing  --":  "leading-trailing",
		"already-kebab-case":      "already-kebab-case",
		"":                        "",
		"UPPER_CASE_with--dashes": "upper-case-with-dashes",
	}
	for input, want := range cases {
		if got := SlugifyBranchComponent(input); got != want {
			t.Errorf("SlugifyBranchComponent(%q) = %q, want %q", input, got, want)
		}
	}

	long := strings.Repeat("a", 60)
	if got := SlugifyBranchComponent(long); len(got) > maxSlugComponentLen {
		t.Fatalf("SlugifyBranchComponent did not cap length: got %d chars", len(got))
	}
}

func TestBuildBranchName(t *testing.T) {
	if got := BuildBranchName("Alice", "Fix Typo"); got != "alice/fix-typo" {
		t.Fatalf("BuildBranchName = %q, want %q", got, "alice/fix-typo")
	}
	if got := BuildBranchName("", ""); got != "user/changes" {
		t.Fatalf("BuildBranchName with empty inputs = %q, want %q", got, "user/changes")
	}
}

func TestIsDefaultBranchTerminatesOptionsBeforeRemote(t *testing.T) {
	// A remote value that looks like a Git option (from --remote or branch
	// config) must reach ls-remote as a positional argument after "--",
	// never be parsed as an option such as --upload-pack.
	root := t.TempDir()
	runner := &fakeRunner{results: []CommandResult{
		{Stdout: root + "\n"},
		{Stdout: "ref: refs/heads/main\tHEAD\nabc123\tHEAD\n"},
	}}

	_, _, _, err := IsDefaultBranch(context.Background(), DefaultBranchOptions{
		Cwd:    root,
		Branch: "feature",
		Remote: "--upload-pack=/bin/echo",
		RunGit: runner.Run,
	})
	if err != nil {
		t.Fatalf("IsDefaultBranch returned error: %v", err)
	}
	if got := runner.commandLine(1); got != "git ls-remote --symref -- --upload-pack=/bin/echo HEAD" {
		t.Fatalf("remote was not terminated with --: %q", got)
	}
}

func TestIsDefaultBranchAllowsFirstPushToUnbornRemote(t *testing.T) {
	// A freshly created empty remote has no refs, so ls-remote succeeds with
	// empty output and `git remote set-head --auto` cannot record a default.
	// The guard must not turn the very first feature-branch push into a
	// --yes dead end; main/master stay protected by the name heuristic. The
	// empty symref output must still be confirmed against `ls-remote --heads`
	// before granting the exception (see the dangling-HEAD test below), so a
	// genuinely unborn remote answers empty there too.
	root := t.TempDir()
	runner := &fakeRunner{results: []CommandResult{
		{Stdout: root + "\n"},
		{Stdout: "\n"}, // ls-remote --symref: remote answered, zero refs
		{Stdout: "\n"}, // ls-remote --heads: confirms no branches at all
	}}

	isDefault, _, _, err := IsDefaultBranch(context.Background(), DefaultBranchOptions{
		Cwd:    root,
		Branch: "alice/first-work",
		Remote: "origin",
		RunGit: runner.Run,
	})
	if err != nil {
		t.Fatalf("IsDefaultBranch on an unborn remote: %v", err)
	}
	if isDefault {
		t.Fatal("feature branch on an unborn remote reported as default")
	}
}

func TestIsDefaultBranchClassifiesConventionalDefaultOnUnbornRemote(t *testing.T) {
	root := t.TempDir()
	runner := &fakeRunner{results: []CommandResult{
		{Stdout: root + "\n"},
		{Stdout: "\n"}, // ls-remote --symref: remote answered, zero refs
		{Stdout: "\n"}, // ls-remote --heads: confirms no branches at all
	}}

	isDefault, _, _, err := IsDefaultBranch(context.Background(), DefaultBranchOptions{
		Cwd:    root,
		Branch: "main",
		Remote: "origin",
		RunGit: runner.Run,
	})
	if err != nil {
		t.Fatalf("IsDefaultBranch on an unborn remote: %v", err)
	}
	if !isDefault {
		t.Fatal("conventional main branch on an unborn remote should be reported as default")
	}
}

func TestIsDefaultBranchFailsClosedOnDanglingRemoteHead(t *testing.T) {
	// A non-empty remote whose HEAD symref is dangling or missing produces
	// the exact same empty `ls-remote --symref` output as a genuinely unborn
	// remote, but it may still have a protected default branch under a name
	// this can't identify. `ls-remote --heads` reporting existing branches
	// must block the unborn-repository exception and fail closed rather than
	// silently treat the branch as safe to push.
	root := t.TempDir()
	runner := &fakeRunner{results: []CommandResult{
		{Stdout: root + "\n"},
		{Stdout: "\n"},                         // ls-remote --symref: HEAD didn't resolve
		{Stdout: "abc123\trefs/heads/trunk\n"}, // ls-remote --heads: remote is NOT empty
		{ExitCode: 1},                          // no local refs/remotes/origin/HEAD record
	}}

	_, _, _, err := IsDefaultBranch(context.Background(), DefaultBranchOptions{
		Cwd:    root,
		Branch: "alice/first-work",
		Remote: "origin",
		RunGit: runner.Run,
	})
	if err == nil || !strings.Contains(err.Error(), "default branch for remote") {
		t.Fatalf("expected fail-closed error on dangling remote HEAD, got %v", err)
	}
}

func TestCreateBranchAvoidsRemoteOnlyCollision(t *testing.T) {
	// A branch that exists only on the target remote (an old merged-PR
	// branch pruned locally) must count as taken: `push -u` would otherwise
	// silently fast-forward it with unrelated new work.
	root := t.TempDir()
	runner := &fakeRunner{results: []CommandResult{
		{Stdout: root + "\n"},
		{Stdout: "abc123\trefs/heads/alice/fix-typo\nqrs789\trefs/heads/other\n"}, // ls-remote --heads
		{ExitCode: 1}, // rev-parse: alice/fix-typo not local, but remote-taken
		{ExitCode: 1}, // rev-parse: alice/fix-typo-2 free locally
		{Stdout: "Switched to a new branch 'alice/fix-typo-2'\n"},
	}}

	result, err := CreateBranch(context.Background(), BranchOptions{
		Cwd:    root,
		Name:   "alice/fix-typo",
		Remote: "origin",
		RunGit: runner.Run,
	})
	if err != nil {
		t.Fatalf("CreateBranch returned error: %v", err)
	}
	if result.Branch != "alice/fix-typo-2" {
		t.Fatalf("unexpected branch: %#v", result)
	}
	if got := runner.commandLine(1); got != "git ls-remote --heads -- origin" {
		t.Fatalf("unexpected remote probe: %q", got)
	}
}

func TestCreateBranchFailsWhenRemoteProbeFails(t *testing.T) {
	// When the target remote cannot be consulted, fail visibly instead of
	// risking a push onto an unseen remote-only branch; the push itself
	// would need the same connectivity anyway.
	root := t.TempDir()
	runner := &fakeRunner{results: []CommandResult{
		{Stdout: root + "\n"},
		{ExitCode: 128, Stderr: "fatal: unable to access"}, // ls-remote --heads fails
	}}

	_, err := CreateBranch(context.Background(), BranchOptions{
		Cwd:    root,
		Name:   "alice/fix-typo",
		Remote: "origin",
		RunGit: runner.Run,
	})
	if err == nil {
		t.Fatal("expected an error when the remote probe fails")
	}
}

// TestResetBranchRefMovesDefaultWithoutTouchingFeature covers the post
// auto-branch restore: after checkout -b user/slug at the same HEAD as main,
// main must be moved back to origin/main so the feature branch exclusively
// owns the publishable commits.
func TestResetBranchRefMovesDefaultWithoutTouchingFeature(t *testing.T) {
	root := initGitRepo(t, true)
	// Normalize the default branch name (git may pick master on older installs).
	runGitCommand(t, root, "branch", "-M", "main")
	base := strings.TrimSpace(runGitCommand(t, root, "rev-parse", "HEAD"))

	// Simulate origin/main at the base tip, then a local commit on main.
	runGitCommand(t, root, "update-ref", "refs/remotes/origin/main", base)
	writeTestFile(t, filepath.Join(root, "feature.txt"), "work\n")
	runGitCommand(t, root, "add", "feature.txt")
	runGitCommand(t, root, "-c", "user.name=Zero", "-c", "user.email=zero@example.invalid", "commit", "-m", "add feature")
	featureTip := strings.TrimSpace(runGitCommand(t, root, "rev-parse", "HEAD"))
	if featureTip == base {
		t.Fatal("expected a new commit on main before branching")
	}

	// Create the feature branch at HEAD (same as ensureFeatureBranch / CreateBranch).
	runGitCommand(t, root, "checkout", "-b", "someone/feature-txt")
	if err := ResetBranchRef(context.Background(), root, "main", "origin/main", nil); err != nil {
		t.Fatalf("ResetBranchRef: %v", err)
	}

	mainTip := strings.TrimSpace(runGitCommand(t, root, "rev-parse", "refs/heads/main"))
	if mainTip != base {
		t.Fatalf("main tip = %s, want base %s (must not keep the new commit)", mainTip, base)
	}
	stillFeature := strings.TrimSpace(runGitCommand(t, root, "rev-parse", "refs/heads/someone/feature-txt"))
	if stillFeature != featureTip {
		t.Fatalf("feature branch tip = %s, want %s", stillFeature, featureTip)
	}
	head := strings.TrimSpace(runGitCommand(t, root, "rev-parse", "--abbrev-ref", "HEAD"))
	if head != "someone/feature-txt" {
		t.Fatalf("HEAD = %q, want someone/feature-txt", head)
	}
}

func TestResetBranchRefRefusesCheckedOutBranch(t *testing.T) {
	root := initGitRepo(t, true)
	runGitCommand(t, root, "branch", "-M", "main")
	base := strings.TrimSpace(runGitCommand(t, root, "rev-parse", "HEAD"))
	runGitCommand(t, root, "update-ref", "refs/remotes/origin/main", base)

	err := ResetBranchRef(context.Background(), root, "main", "origin/main", nil)
	if err == nil || !strings.Contains(err.Error(), "currently checked-out") {
		t.Fatalf("expected refuse-checked-out error, got %v", err)
	}
}

func TestCurrentBranchReturnsCheckedOutName(t *testing.T) {
	root := initGitRepo(t, true)
	runGitCommand(t, root, "branch", "-M", "main")
	if got := CurrentBranch(context.Background(), root, nil); got != "main" {
		t.Fatalf("CurrentBranch = %q, want main", got)
	}
	runGitCommand(t, root, "checkout", "-b", "feature/work")
	if got := CurrentBranch(context.Background(), root, nil); got != "feature/work" {
		t.Fatalf("CurrentBranch = %q, want feature/work", got)
	}
}

// TestRemoteHasBranchReportsTrueForDivergedTip is the ordinary retry case: a
// branch already published once, then a second local commit, then push
// again. RemoteHasBranch must report existence only, not tip equality, or
// ensureFeatureBranch reasserts the nonexistence lease (--force-with-lease=
// <branch>:) against a branch that plainly exists, and the follow-up push
// fails with a stale-info error instead of a normal fast-forward.
func TestRemoteHasBranchReportsTrueForDivergedTip(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git unavailable: %v", err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	tmp := t.TempDir()
	bare := filepath.Join(tmp, "remote.git")
	repo := filepath.Join(tmp, "repo")
	runGitCommand(t, tmp, "init", "--bare", bare)
	runGitCommand(t, tmp, "init", repo)
	runGitCommand(t, repo, "config", "user.name", "Zero")
	runGitCommand(t, repo, "config", "user.email", "zero@example.invalid")
	runGitCommand(t, repo, "checkout", "-b", "main")
	writeTestFile(t, filepath.Join(repo, "README.md"), "initial\n")
	runGitCommand(t, repo, "add", "README.md")
	runGitCommand(t, repo, "commit", "-m", "Initial commit")
	runGitCommand(t, repo, "remote", "add", "origin", bare)
	runGitCommand(t, repo, "push", "-u", "origin", "main")
	runGitCommand(t, repo, "checkout", "-b", "user/slug")
	writeTestFile(t, filepath.Join(repo, "README.md"), "feature work\n")
	runGitCommand(t, repo, "add", "README.md")
	runGitCommand(t, repo, "commit", "-m", "feature")
	runGitCommand(t, repo, "push", "-u", "origin", "user/slug")

	// A second local commit puts HEAD ahead of what was just published; the
	// remote tip and local HEAD now differ.
	writeTestFile(t, filepath.Join(repo, "README.md"), "feature work v2\n")
	runGitCommand(t, repo, "add", "README.md")
	runGitCommand(t, repo, "commit", "-m", "feature v2")

	exists, err := RemoteHasBranch(context.Background(), repo, "origin", "user/slug", nil)
	if err != nil {
		t.Fatalf("RemoteHasBranch: %v", err)
	}
	if !exists {
		t.Fatal("RemoteHasBranch must report true for a published branch whose tip has since diverged locally")
	}
}

// TestRemoteHasBranchSeesPushWithoutLocalUpstream is the publication side of
// the push -u config-write race: a plain `git push` (no -u) leaves local
// upstream empty while the remote branch exists. ensureFeatureBranch must
// probe this rather than reasserting the nonexistence lease.
func TestRemoteHasBranchSeesPushWithoutLocalUpstream(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git unavailable: %v", err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	tmp := t.TempDir()
	bare := filepath.Join(tmp, "remote.git")
	repo := filepath.Join(tmp, "repo")
	runGitCommand(t, tmp, "init", "--bare", bare)
	runGitCommand(t, tmp, "init", repo)
	runGitCommand(t, repo, "config", "user.name", "Zero")
	runGitCommand(t, repo, "config", "user.email", "zero@example.invalid")
	runGitCommand(t, repo, "checkout", "-b", "main")
	writeTestFile(t, filepath.Join(repo, "README.md"), "initial\n")
	runGitCommand(t, repo, "add", "README.md")
	runGitCommand(t, repo, "commit", "-m", "Initial commit")
	runGitCommand(t, repo, "remote", "add", "origin", bare)
	runGitCommand(t, repo, "push", "-u", "origin", "main")
	runGitCommand(t, repo, "checkout", "-b", "user/slug")
	writeTestFile(t, filepath.Join(repo, "README.md"), "feature work\n")
	runGitCommand(t, repo, "add", "README.md")
	runGitCommand(t, repo, "commit", "-m", "feature")
	// Publish without -u so local upstream stays unset (same observable state
	// as push -u succeeding on the remote then failing to write .git/config).
	runGitCommand(t, repo, "push", "origin", "user/slug")

	if ref := UpstreamRef(context.Background(), repo, "user/slug", nil); ref != "" {
		t.Fatalf("UpstreamRef after push without -u = %q, want empty", ref)
	}
	exists, err := RemoteHasBranch(context.Background(), repo, "origin", "user/slug", nil)
	if err != nil {
		t.Fatalf("RemoteHasBranch: %v", err)
	}
	if !exists {
		t.Fatal("RemoteHasBranch must report the published branch even without local upstream")
	}
	missing, err := RemoteHasBranch(context.Background(), repo, "origin", "user/other", nil)
	if err != nil {
		t.Fatalf("RemoteHasBranch missing: %v", err)
	}
	if missing {
		t.Fatal("RemoteHasBranch must be false for an unpublished name")
	}
}

// TestHasUpstreamRejectsInheritedMainUpstream covers branch.autoSetupMerge=inherit:
// checkout -b copies origin/main onto the new branch before any push -u. That
// must not count as a published upstream for the generated branch name.
func TestHasUpstreamRejectsInheritedMainUpstream(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git unavailable: %v", err)
	}
	// branch.autoSetupMerge=inherit (which makes checkout -b copy the source
	// branch's upstream) was added in git 2.35; older git would silently run
	// the test against plain default behavior and fail the assertions.
	requireGitAtLeast(t, 2, 35)
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	tmp := t.TempDir()
	bare := filepath.Join(tmp, "remote.git")
	repo := filepath.Join(tmp, "repo")
	runGitCommand(t, tmp, "init", "--bare", bare)
	runGitCommand(t, tmp, "init", repo)
	runGitCommand(t, repo, "config", "user.name", "Zero")
	runGitCommand(t, repo, "config", "user.email", "zero@example.invalid")
	runGitCommand(t, repo, "checkout", "-b", "main")
	writeTestFile(t, filepath.Join(repo, "README.md"), "initial\n")
	runGitCommand(t, repo, "add", "README.md")
	runGitCommand(t, repo, "commit", "-m", "Initial commit")
	runGitCommand(t, repo, "remote", "add", "origin", bare)
	runGitCommand(t, repo, "push", "-u", "origin", "main")
	runGitCommand(t, repo, "config", "branch.autoSetupMerge", "inherit")
	runGitCommand(t, repo, "checkout", "-b", "user/slug")

	if ref := UpstreamRef(context.Background(), repo, "user/slug", nil); ref != "origin/main" {
		t.Fatalf("UpstreamRef after inherit = %q, want origin/main", ref)
	}
	has, err := HasUpstream(context.Background(), repo, "user/slug", nil)
	if err != nil {
		t.Fatalf("HasUpstream: %v", err)
	}
	if has {
		t.Fatal("HasUpstream must reject inherited origin/main for user/slug")
	}

	runGitCommand(t, repo, "push", "-u", "origin", "user/slug")
	if ref := UpstreamRef(context.Background(), repo, "user/slug", nil); ref != "origin/user/slug" {
		t.Fatalf("UpstreamRef after push -u = %q, want origin/user/slug", ref)
	}
	has, err = HasUpstream(context.Background(), repo, "user/slug", nil)
	if err != nil {
		t.Fatalf("HasUpstream after push: %v", err)
	}
	if !has {
		t.Fatal("HasUpstream must accept exact origin/user/slug after push -u")
	}
}

func TestHasUpstreamMultiSegmentRemote(t *testing.T) {
	root := t.TempDir()
	runner := &fakeRunner{results: []CommandResult{
		{Stdout: "team/upstream/user/slug\n"},
	}}
	has, err := HasUpstream(context.Background(), root, "user/slug", runner.Run)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !has {
		t.Fatal("expected HasUpstream to return true for multi-segment remote tracking branch")
	}
}

func TestResetBranchRefRefusesConcurrentDefaultAdvance(t *testing.T) {
	root := initGitRepo(t, true)
	runGitCommand(t, root, "branch", "-M", "main")
	base := strings.TrimSpace(runGitCommand(t, root, "rev-parse", "HEAD"))
	writeTestFile(t, filepath.Join(root, "feature.txt"), "feature\n")
	runGitCommand(t, root, "add", "feature.txt")
	runGitCommand(t, root, "-c", "user.name=Zero", "-c", "user.email=zero@example.invalid", "commit", "-m", "feature")
	featureTip := strings.TrimSpace(runGitCommand(t, root, "rev-parse", "HEAD"))
	runGitCommand(t, root, "checkout", "-b", "someone/feature")
	writeTestFile(t, filepath.Join(root, "concurrent.txt"), "concurrent\n")
	runGitCommand(t, root, "add", "concurrent.txt")
	runGitCommand(t, root, "-c", "user.name=Zero", "-c", "user.email=zero@example.invalid", "commit", "-m", "concurrent")
	concurrentTip := strings.TrimSpace(runGitCommand(t, root, "rev-parse", "HEAD"))
	runGitCommand(t, root, "update-ref", "refs/heads/main", concurrentTip)
	err := ResetBranchRef(context.Background(), root, "main", base, nil, base)
	if err == nil {
		t.Fatal("expected concurrent default-branch update to reject restoration")
	}
	// The refusal must be identifiable as a compare-and-swap conflict, not a
	// generic write failure: ensureFeatureBranch keys off this to preserve the
	// generated branch instead of deleting it during rollback.
	if !errors.Is(err, ErrCompareAndSwapConflict) {
		t.Fatalf("expected ErrCompareAndSwapConflict, got %v", err)
	}
	got := strings.TrimSpace(runGitCommand(t, root, "rev-parse", "refs/heads/main"))
	if got != concurrentTip {
		t.Fatalf("main tip = %s, want concurrent tip %s", got, concurrentTip)
	}
	if featureTip == concurrentTip {
		t.Fatal("test setup failed to create distinct tips")
	}
}

func TestResetBranchRefRefusesCheckedOutInLinkedWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git unavailable: %v", err)
	}
	root := initGitRepo(t, true)
	runGitCommand(t, root, "branch", "-M", "main")
	base := strings.TrimSpace(runGitCommand(t, root, "rev-parse", "HEAD"))
	runGitCommand(t, root, "checkout", "-b", "feature/new-branch")

	// Create a second linked worktree checking out main.
	wtDir := filepath.Join(t.TempDir(), "wt-main")
	runGitCommand(t, root, "worktree", "add", wtDir, "main")

	err := ResetBranchRef(context.Background(), root, "main", base, nil)
	if err == nil || !strings.Contains(err.Error(), "checked out in worktree") {
		t.Fatalf("expected refusal due to checked out in linked worktree, got %v", err)
	}
}

func TestIsNamedRemoteHandlesSlashes(t *testing.T) {
	root := t.TempDir()
	if !IsNamedRemote(context.Background(), root, "team/upstream", nil) {
		t.Fatal("IsNamedRemote should report true for configured remote containing slash")
	}
}

func TestUpstreamRemoteAndMergeBranch(t *testing.T) {
	root := t.TempDir()
	runner := &fakeRunner{results: []CommandResult{
		{Stdout: "team/upstream\n"},
		{Stdout: "refs/heads/main\n"},
	}}
	remote, merge := UpstreamRemoteAndMergeBranch(context.Background(), root, "feature", runner.Run)
	if remote != "team/upstream" || merge != "main" {
		t.Fatalf("UpstreamRemoteAndMergeBranch = (%q, %q), want (%q, %q)", remote, merge, "team/upstream", "main")
	}
}
