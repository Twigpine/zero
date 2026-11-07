package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Twigpine/zero/internal/sandbox"
	"github.com/Twigpine/zero/internal/specialist"
)

// childRootsFixture is the shape a write-capable plan run takes on disk: the
// parent workspace below an ancestor the run was granted (--add-dir ~/dev), and
// an isolated worktree somewhere else entirely.
func childRootsFixture(t *testing.T) (ancestor, workspace, worktree string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ancestor = filepath.Join(base, "dev")
	workspace = filepath.Join(ancestor, "zero")
	worktree = filepath.Join(base, "state", "worktrees", "plan-a")
	for _, dir := range []string{workspace, worktree} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return ancestor, workspace, worktree
}

func flagValues(args []string, flag string) []string {
	var out []string
	for index, arg := range args {
		if arg == flag && index+1 < len(args) {
			out = append(out, args[index+1])
		}
	}
	return out
}

func containsValue(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// THE EXECUTOR THE CLI BUILDS must know the parent's workspace, or the
// isolation filter in the argument builder has nothing to compare against and
// an ancestor write root follows an isolated child into its worktree.
func TestTheCLIExecutorWithholdsAnAncestorRootFromAnIsolatedChild(t *testing.T) {
	ancestor, workspace, worktree := childRootsFixture(t)
	scope, err := sandbox.NewScope(workspace, []string{ancestor})
	if err != nil {
		t.Fatalf("NewScope: %v", err)
	}
	executor := specialist.Executor{
		NewSessionID: func() (string, error) { return "specialist_00000000000000000000000a", nil },
	}
	wireChildRoots(&executor, workspace, orchestrateWiring{ExtraWriteRoots: func(withinTurn bool) []string { return childWriteRoots(scope, withinTurn) }})

	isolated, err := executor.BuildArgs(specialist.BuildArgsInput{
		Manifest: specialist.Manifest{Metadata: specialist.Metadata{Name: "explorer"}},
		Prompt:   "p",
		Cwd:      worktree,
	})
	if err != nil {
		t.Fatalf("BuildArgs: %v", err)
	}
	if added := flagValues(isolated.Args, "--add-dir"); containsValue(added, ancestor) {
		t.Fatalf("the isolated child was handed the ancestor %q of the parent workspace as a write root: --add-dir %v", ancestor, added)
	}

	// The same run's ordinary child, in the parent's own workspace, keeps it.
	ordinary, err := executor.BuildArgs(specialist.BuildArgsInput{
		Manifest: specialist.Manifest{Metadata: specialist.Metadata{Name: "explorer"}},
		Prompt:   "p",
		Cwd:      workspace,
	})
	if err != nil {
		t.Fatalf("BuildArgs: %v", err)
	}
	if added := flagValues(ordinary.Args, "--add-dir"); !containsValue(added, ancestor) {
		t.Fatalf("an ordinary child lost the run's ancestor root: --add-dir %v", added)
	}
}
