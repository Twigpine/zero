package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/Twigpine/zero/internal/specialist"
	"github.com/Twigpine/zero/internal/worktrees"
)

// The isolator: a git worktree for a plan that may write.
//
// It reuses internal/worktrees, the same package `zero exec --worktree` already
// uses, rather than shelling out to git again. That matters beyond tidiness —
// worktrees.Prepare already handles name validation, the base directory, repo
// discovery from a subdirectory, and the difference between a checkout and a
// worktree. A second implementation would eventually disagree with the first
// about which of those a plan gets.
//
// NOT DELETED ON RELEASE. A plan that wrote something produced work the user has
// not seen yet, and removing the tree would remove the only copy of it. Release
// therefore retains successful output. Abort is the distinct admission-failure
// path: it drops the lease on a tree the plan never received, so a corrected
// retry is not permanently locked out.

// planWorktreeName derives a worktree name from a plan name.
//
// worktrees.Prepare enforces its own allow-list on the result, so this only has
// to produce a candidate; anything it cannot clean is handed over as a prefix
// and Prepare supplies the rest. Deliberately NOT a sanitiser that promises
// safety — the guarantee lives in one place, where it is tested.
func planWorktreeName(planName string) string {
	cleaned := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '-', r == '_':
			return r
		default:
			return -1
		}
	}, planName)
	cleaned = strings.TrimSpace(cleaned)
	if cleaned == "" {
		return "plan"
	}
	if len(cleaned) > 40 {
		cleaned = cleaned[:40]
	}
	return "plan-" + cleaned
}

// newPlanIsolator returns an isolator rooted at the run's workspace, or nil when
// this run cannot isolate at all.
//
// nil is the meaningful answer, not an error: specialist refuses a plan that
// requires isolation when the isolator is nil, with a reason. Returning a
// non-nil isolator that always fails would move the same refusal later and make
// it read like a malfunction.
func newPlanIsolator(workspaceRoot string) specialist.PlanIsolator {
	if strings.TrimSpace(workspaceRoot) == "" {
		return nil
	}
	return func(ctx context.Context, planName string) (specialist.PlanWorkspace, error) {
		prepared, err := worktrees.Prepare(ctx, worktrees.Options{
			Cwd:  workspaceRoot,
			Name: planWorktreeName(planName),
		})
		if err != nil {
			return specialist.PlanWorkspace{}, err
		}
		if strings.TrimSpace(prepared.Path) == "" {
			// Defence in depth against a Prepare that reports success with no
			// path: specialist refuses an un-isolated workspace, and this makes
			// sure it sees one rather than an empty string it might read as the
			// parent's own directory.
			return specialist.PlanWorkspace{}, fmt.Errorf("prepared worktree has no path")
		}
		return specialist.PlanWorkspace{
			Path:     prepared.Path,
			Isolated: true,
			Describe: fmt.Sprintf("worktree %s at %s", prepared.Name, prepared.Path),
			// Release does NOT remove the tree. A plan that wrote produced work
			// nobody has reviewed; deleting it would delete the only copy.
			Release: func() {},
			Abort: func() {
				_ = worktrees.Release(context.Background(), worktrees.Options{Cwd: workspaceRoot}, prepared.Path)
			},
		}, nil
	}
}
