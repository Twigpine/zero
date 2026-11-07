package specialist

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/sandbox"
)

// argValues returns every value passed for a repeated flag.
func argValues(args []string, flag string) []string {
	var out []string
	for i, arg := range args {
		if arg == flag && i+1 < len(args) {
			out = append(out, args[i+1])
		}
	}
	return out
}

// AN ISOLATED PLAN TASK MUST NOT BE HANDED THE PARENT'S TREE BACK.
//
// The worktree exists so a write-capable plan has "somewhere to write that is
// not the user's tree" (plan_worktree.go) — one branch, one diff, a discard
// path. --cwd narrows the child to it. If the parent's own workspace root then
// arrives as --add-dir, the very next flag re-opens everything the worktree was
// protecting, and the isolation is decoration.
//
// This is not hypothetical. In a measured run the task's cwd was the worktree
// and ten writes landed in /Users/kratos/mini, each allowed with the reason
// "workspace write is allowed" — because by then it genuinely was inside the
// child's write roots.
func TestAnIsolatedPlanTaskIsNotHandedTheParentTree(t *testing.T) {
	parentWorkspace, worktree, granted := isolationDirs(t)

	executor := Executor{
		BinaryPath:   "/bin/true",
		NewSessionID: func() (string, error) { return "specialist_00000000000000000000000a", nil },
		// Exactly what app.go wires: the run's scope, asked for what it holds
		// BEYOND its workspace. Wiring scope.Roots here instead — which also
		// returns the workspace root — is the defect this test exists for.
		ExtraWriteRoots: mustScope(t, parentWorkspace, granted).ExtraRoots,
	}

	built, err := executor.BuildArgs(BuildArgsInput{
		Manifest:     Manifest{Metadata: Metadata{Name: "explorer"}},
		Prompt:       "do the work",
		Cwd:          worktree,
		CurrentDepth: 0,
	})
	if err != nil {
		t.Fatalf("BuildArgs: %v", err)
	}

	added := argValues(built.Args, "--add-dir")
	for _, root := range added {
		if root == parentWorkspace {
			t.Errorf("the child was handed the parent's workspace %q as a write root, "+
				"so the worktree at %q isolates nothing: --add-dir %v",
				parentWorkspace, worktree, added)
		}
	}
	// A genuine mid-session grant must still reach the child — a child confined
	// MORE tightly than its parent is its own bug, and the reason this list
	// exists at all.
	if !containsRoot(added, granted) {
		t.Errorf("a granted root was withheld from the child: --add-dir %v", added)
	}
	// And the worktree itself is covered by --cwd, never repeated.
	if containsRoot(added, worktree) {
		t.Errorf("the child's own workspace was repeated as an extra root: %v", added)
	}
	if cwds := argValues(built.Args, "--cwd"); len(cwds) != 1 || cwds[0] != worktree {
		t.Errorf("--cwd = %v, want exactly the worktree", cwds)
	}
}

// An ORDINARY sub-agent, whose cwd is the parent's workspace, is unaffected: the
// workspace root was already skipped for matching its cwd. This pins that the
// fix changes only the isolated case.
func TestAnOrdinarySubAgentKeepsEveryRootItHadBefore(t *testing.T) {
	parentWorkspace, _, granted := isolationDirs(t)

	executor := Executor{
		BinaryPath:      "/bin/true",
		NewSessionID:    func() (string, error) { return "specialist_00000000000000000000000a", nil },
		ExtraWriteRoots: mustScope(t, parentWorkspace, granted).ExtraRoots,
	}
	built, err := executor.BuildArgs(BuildArgsInput{
		Manifest:     Manifest{Metadata: Metadata{Name: "explorer"}},
		Prompt:       "do the work",
		Cwd:          parentWorkspace,
		CurrentDepth: 0,
	})
	if err != nil {
		t.Fatalf("BuildArgs: %v", err)
	}
	added := argValues(built.Args, "--add-dir")
	if !containsRoot(added, granted) {
		t.Errorf("a granted root was withheld: %v", added)
	}
	if containsRoot(added, parentWorkspace) {
		t.Errorf("the child's own workspace was repeated as an extra root: %v", added)
	}
}

// isolationDirs makes the three real directories a plan run involves: the
// parent's workspace, the worktree a write-capable plan is isolated into, and a
// root granted mid-session beyond the workspace.
func isolationDirs(t *testing.T) (parentWorkspace, worktree, granted string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	parentWorkspace = filepath.Join(base, "project")
	worktree = filepath.Join(base, "worktrees", "plan-x")
	granted = filepath.Join(base, "granted-elsewhere")
	for _, dir := range []string{parentWorkspace, worktree, granted} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return parentWorkspace, worktree, granted
}

// mustScope builds the same sandbox scope a session holds: a workspace root plus
// anything granted beyond it.
func mustScope(t *testing.T, workspaceRoot string, extras ...string) *sandbox.Scope {
	t.Helper()
	scope, err := sandbox.NewScope(workspaceRoot, extras)
	if err != nil {
		t.Fatalf("NewScope: %v", err)
	}
	return scope
}

func containsRoot(roots []string, want string) bool {
	for _, root := range roots {
		if strings.TrimSpace(root) == want {
			return true
		}
	}
	return false
}
