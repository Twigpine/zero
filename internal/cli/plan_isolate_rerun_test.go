package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// worktreeLockReason returns the lock reason git records for path, and whether
// it is locked at all.
func worktreeLockReason(t *testing.T, repo, path string) (string, bool) {
	t.Helper()
	out, err := exec.Command("git", "-C", repo, "worktree", "list", "--porcelain").CombinedOutput()
	if err != nil {
		t.Fatalf("git worktree list: %v\n%s", err, out)
	}
	want, _ := filepath.EvalSymlinks(path)
	var current string
	for _, line := range strings.Split(string(out), "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			current = strings.TrimPrefix(line, "worktree ")
			if resolved, err := filepath.EvalSymlinks(current); err == nil {
				current = resolved
			}
		case current == want && (line == "locked" || strings.HasPrefix(line, "locked ")):
			return strings.TrimSpace(strings.TrimPrefix(line, "locked")), true
		}
	}
	return "", false
}

// A WRITE-CAPABLE PLAN MUST BE ABLE TO RUN TWICE. The isolator used a fixed
// worktree name per plan, a PID-less lease, and a Release that did nothing, so
// the second run of the same plan found its tree "locked by another active run"
// and was refused until someone ran `zero worktrees release` by hand.
//
// Driven against a real repository: each execution gets its own tree, the
// lease names this process while the plan runs, Release drops the lease, and
// the first run's unreviewed output is still on disk afterwards.
func TestAWriteCapablePlanCanRunAgainAfterItFinishes(t *testing.T) {
	repo := initTempGitRepo(t)
	base := t.TempDir()
	clock := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	suffixes := []string{"aaaa0001", "aaaa0002"}
	isolate := newPlanIsolatorWith(repo, planIsolatorDeps{
		baseDir: base,
		// The SAME second for both runs, so only the suffix separates them.
		now: func() time.Time { return clock },
		suffix: func() string {
			next := suffixes[0]
			suffixes = suffixes[1:]
			return next
		},
	})

	first, err := isolate(context.Background(), "sweep")
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	reason, locked := worktreeLockReason(t, repo, first.Path)
	if !locked {
		t.Fatal("the tree a running plan uses must be leased so Clean cannot prune it")
	}
	if !strings.Contains(reason, "(pid "+strconv.Itoa(os.Getpid())+")") {
		t.Fatalf("the lease must name this process so a crash is recoverable, got %q", reason)
	}
	output := filepath.Join(first.Path, "result.txt")
	if err := os.WriteFile(output, []byte("unreviewed plan output\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	first.Release()

	if _, locked := worktreeLockReason(t, repo, first.Path); locked {
		t.Fatal("Release left the finished plan's tree locked; nothing would ever clear it")
	}
	if data, err := os.ReadFile(output); err != nil || string(data) != "unreviewed plan output\n" {
		t.Fatalf("Release must keep the plan's output for review: %q, %v", data, err)
	}

	second, err := isolate(context.Background(), "sweep")
	if err != nil {
		t.Fatalf("a second run of the same plan was refused: %v", err)
	}
	defer second.Release()
	if second.Path == first.Path {
		t.Fatalf("the second run reused the first run's tree %s, on top of output nobody reviewed", first.Path)
	}
	if _, err := os.Stat(filepath.Join(second.Path, "result.txt")); !os.IsNotExist(err) {
		t.Fatalf("the second run's tree carries the first run's output: %v", err)
	}
	if !strings.HasPrefix(filepath.Base(second.Path), "plan-sweep-") {
		t.Fatalf("the tree name no longer says which plan made it: %s", second.Path)
	}
}

func TestAPlanExecutionWorktreeNameIsAcceptedAndBounded(t *testing.T) {
	at := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	got := planExecutionWorktreeName(strings.Repeat("x", 300), at, "deadbeef")
	// worktrees.Prepare's allow-list (worktreeNamePattern there).
	if !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,80}$`).MatchString(got) {
		t.Fatalf("worktrees.Prepare would reject %q", got)
	}
	if got := planExecutionWorktreeName("sweep", at, ""); got != "plan-sweep-20261107-120000" {
		t.Fatalf("name without a suffix = %q", got)
	}
}
