package specialist

import (
	"os"
	"path/filepath"
	"slices"
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
		WorkspaceRoot:   parentWorkspace,
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
	// CONTAINMENT, not equality: an ancestor of the parent workspace, or a root
	// inside it, re-opens the parent tree exactly as the workspace itself would.
	// The equality-only version of this guard passed while --add-dir ~/dev wrote
	// straight into the user's checkout.
	for _, root := range added {
		if pathsOverlap(root, parentWorkspace, hostPathFlavor) {
			t.Errorf("the child was handed %q, which overlaps the parent's workspace %q, as a write root, "+
				"so the worktree at %q isolates nothing: --add-dir %v",
				root, parentWorkspace, worktree, added)
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
		WorkspaceRoot:   parentWorkspace,
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

// AN ANCESTOR WRITE ROOT MUST NOT FOLLOW A CHILD INTO ITS WORKTREE.
//
// A run started with --add-dir ~/dev (or config additionalWriteRoots) holds a
// write root that CONTAINS its workspace ~/dev/zero. appendExtraWriteRootArgs
// skipped only a root byte-equal to --cwd, so an isolated plan task launched in
// its worktree was handed --add-dir ~/dev and wrote into the parent checkout
// with no branch and no diff. A root INSIDE the parent tree does the same, on a
// smaller scale. Through the real argument builders, fresh and resumed.
func TestAnIsolatedChildIsNotHandedARootOverlappingTheParentTree(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dev := filepath.Join(base, "dev")
	parentWorkspace := filepath.Join(dev, "zero")
	inside := filepath.Join(parentWorkspace, "internal")
	worktree := filepath.Join(base, "state", "worktrees", "plan-a")
	unrelated := filepath.Join(base, "elsewhere")
	// A sibling that merely shares a name prefix with the parent ("zero" /
	// "zero-notes") is NOT inside it, and must still be forwarded.
	prefixSibling := filepath.Join(dev, "zero-notes")
	for _, dir := range []string{inside, worktree, unrelated, prefixSibling} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	for _, tc := range []struct {
		name     string
		extras   []string
		withheld string
	}{
		{name: "ancestor of the parent workspace", extras: []string{dev, unrelated}, withheld: dev},
		{name: "root inside the parent workspace", extras: []string{inside, unrelated, prefixSibling}, withheld: inside},
	} {
		t.Run(tc.name, func(t *testing.T) {
			executor := Executor{
				BinaryPath:      "/bin/true",
				NewSessionID:    func() (string, error) { return "specialist_00000000000000000000000a", nil },
				ExtraWriteRoots: mustScope(t, parentWorkspace, tc.extras...).ExtraRoots,
				WorkspaceRoot:   parentWorkspace,
			}
			fresh, err := executor.BuildArgs(BuildArgsInput{
				Manifest: Manifest{Metadata: Metadata{Name: "explorer"}},
				Prompt:   "do the work",
				Cwd:      worktree,
			})
			if err != nil {
				t.Fatalf("BuildArgs: %v", err)
			}
			resumed, err := executor.BuildResumeArgs(BuildResumeArgsInput{
				SessionID: "specialist_00000000000000000000000a",
				Manifest:  Manifest{Metadata: Metadata{Name: "explorer"}},
				Prompt:    "carry on",
				Cwd:       worktree,
			})
			if err != nil {
				t.Fatalf("BuildResumeArgs: %v", err)
			}
			for label, args := range map[string][]string{"fresh": fresh.Args, "resume": resumed.Args} {
				added := argValues(args, "--add-dir")
				if containsRoot(added, tc.withheld) {
					t.Errorf("%s: the isolated child was handed %q, which overlaps the parent workspace %q: --add-dir %v",
						label, tc.withheld, parentWorkspace, added)
				}
				for _, root := range added {
					if pathsOverlap(root, parentWorkspace, hostPathFlavor) {
						t.Errorf("%s: --add-dir %q overlaps the parent workspace %q", label, root, parentWorkspace)
					}
				}
				// Withholding is targeted: a root unrelated to the parent tree is
				// still the child's, or the child stands on less than its parent.
				if !containsRoot(added, unrelated) {
					t.Errorf("%s: an unrelated root was withheld too: --add-dir %v", label, added)
				}
				if slices.Contains(tc.extras, prefixSibling) && !containsRoot(added, prefixSibling) {
					t.Errorf("%s: %q only shares a name prefix with the parent and was withheld: --add-dir %v",
						label, prefixSibling, added)
				}
			}
		})
	}
}

// A child that runs IN the parent's workspace has no separate tree to protect,
// so an ancestor root is its own as much as the parent's and is kept.
func TestANonIsolatedChildKeepsAnAncestorRoot(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dev := filepath.Join(base, "dev")
	parentWorkspace := filepath.Join(dev, "zero")
	if err := os.MkdirAll(parentWorkspace, 0o755); err != nil {
		t.Fatal(err)
	}
	executor := Executor{
		BinaryPath:      "/bin/true",
		NewSessionID:    func() (string, error) { return "specialist_00000000000000000000000a", nil },
		ExtraWriteRoots: mustScope(t, parentWorkspace, dev).ExtraRoots,
		WorkspaceRoot:   parentWorkspace,
	}
	built, err := executor.BuildArgs(BuildArgsInput{
		Manifest: Manifest{Metadata: Metadata{Name: "explorer"}},
		Prompt:   "do the work",
		Cwd:      parentWorkspace,
	})
	if err != nil {
		t.Fatalf("BuildArgs: %v", err)
	}
	if added := argValues(built.Args, "--add-dir"); !containsRoot(added, dev) {
		t.Errorf("a child in the parent's own workspace lost the ancestor root: --add-dir %v", added)
	}
}

// The containment check on Windows paths, which a Linux run of the tests
// above never exercises: case-insensitive, either separator, and still bounded
// on a separator so a shared name prefix is not containment.
func TestPathOverlapFollowsWindowsRules(t *testing.T) {
	windows := pathFlavor{foldCase: true, backslash: true}
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{`C:\Users\Dev`, `c:\users\dev\zero`, true},
		{`c:\users\dev\zero`, `C:\Users\Dev`, true},
		{`C:\Users\Dev`, `C:/Users/Dev/zero`, true},
		{`C:\Users\Dev\`, `C:\Users\Dev\zero`, true},
		{`C:\`, `C:\Users\Dev\zero`, true},
		{`C:\Users\Dev`, `C:\Users\Developer`, false},
		{`C:\Users\Dev\zero`, `D:\Users\Dev\zero`, false},
	} {
		if got := pathsOverlap(tc.a, tc.b, windows); got != tc.want {
			t.Errorf("pathsOverlap(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
	// And the same pair is NOT folded under POSIX rules.
	if pathsOverlap("/Users/Dev", "/users/dev/zero", pathFlavor{}) {
		t.Error("POSIX rules folded case")
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
