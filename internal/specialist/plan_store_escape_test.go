package specialist

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// planLinkDir uses a junction on Windows: no privilege needed, unlike a symlink,
// so it is both the reachable attack and the only one testable on an ordinary
// Windows account.
func planLinkDir(t *testing.T, target, link string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
			t.Skipf("cannot create a junction: %v %s", err, out)
		}
		return
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create a symlink: %v", err)
	}
}

// SavePlan checked its own directory and file for links and then handed both
// strings to MkdirAll, CreateTemp and Rename, which re-resolve every ancestor.
// A link at .zero therefore turned "save my plan" into a write aimed anywhere
// on disk, with the checks passing because they were looking at components that
// genuinely were not links.
func TestSavingRefusesToWriteThroughALinkedAncestor(t *testing.T) {
	base := t.TempDir()
	outside := filepath.Join(base, "outside")
	workspace := filepath.Join(base, "workspace")
	for _, dir := range []string{outside, workspace} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	planLinkDir(t, outside, filepath.Join(workspace, ".zero"))

	paths := DefaultPlanPaths(workspace, "")
	if _, err := SavePlan(paths.ProjectRoot, paths.ProjectDir, "escaped", savedPlanFixture(t)); err == nil {
		t.Fatal("SavePlan wrote through a linked ancestor, so a saved plan can land anywhere on disk")
	}
	if _, err := os.Stat(filepath.Join(outside, "plans", "escaped.json")); !os.IsNotExist(err) {
		t.Errorf("a plan was written outside the workspace, stat error = %v", err)
	}
}

func TestLoadingRefusesToReadThroughALinkedAncestor(t *testing.T) {
	base := t.TempDir()
	outside := filepath.Join(base, "outside")
	workspace := filepath.Join(base, "workspace")
	if err := os.MkdirAll(filepath.Join(outside, "plans"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "plans", "escaped.json"), []byte(`{"name":"escaped","tasks":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	planLinkDir(t, outside, filepath.Join(workspace, ".zero"))

	paths := DefaultPlanPaths(workspace, "")
	plans, problems := LoadPlans(paths)
	for _, plan := range plans {
		if plan.Name == "escaped" {
			t.Fatal("LoadPlans followed a linked ancestor outside the declared project root")
		}
	}
	if len(problems) == 0 {
		t.Fatal("the linked project scope was silently treated as an absent directory")
	}
}

// The ordinary path still saves, or the test above would pass against a store
// that refused everything.
func TestSavingAnOrdinaryWorkspacePlanStillWorks(t *testing.T) {
	workspace := t.TempDir()
	paths := DefaultPlanPaths(workspace, "")
	path, err := SavePlan(paths.ProjectRoot, paths.ProjectDir, "sweep", savedPlanFixture(t))
	if err != nil {
		t.Fatalf("SavePlan: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the saved plan is not on disk: %v", err)
	}
	plans, problems := LoadPlans(paths)
	if len(problems) != 0 {
		t.Errorf("LoadPlans reported problems: %v", problems)
	}
	// Bundled plans are listed too, so look for ours rather than counting.
	var found bool
	for _, plan := range plans {
		if plan.Name == "sweep" && plan.Scope == PlanScopeProject {
			found = true
		}
	}
	if !found {
		t.Errorf("the saved project plan was not listed back: %+v", plans)
	}
}
