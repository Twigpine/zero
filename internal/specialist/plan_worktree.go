package specialist

import (
	"context"
	"fmt"
)

// Where a plan's tasks RUN, when they may write.
//
// STEP 2 OF THREE. Step 1 gave the approval prompt something to show; this
// gives a write-capable plan somewhere to write that is not the user's tree;
// step 3 allows write tools with both as preconditions. Nothing requires
// isolation today, and that is not an oversight — RequiresIsolation is DERIVED
// from the plan's own tools, so it is false while validation rejects write
// tools and becomes true the moment step 3 stops rejecting them. The
// requirement arrives with the capability rather than depending on someone
// remembering to set a flag.
//
// PER PLAN, NOT PER TASK, and the choice matters. Per-task isolation is more
// isolation and less use: a later task
// routinely needs an earlier one's output, so per-task worktrees would either
// hide that or need a merge step nobody wrote. A plan is the unit of work a
// user reviews, so a plan is the unit that gets a tree — one branch, one diff.
//
// FAIL CLOSED. A plan that requires isolation and cannot get it is REFUSED,
// never run in the parent tree with a warning. The whole point of the
// precondition is that it is one.

// PlanWorkspace is the directory a plan's tasks run in.
type PlanWorkspace struct {
	// Path is the working directory for every task in the plan. Empty means the
	// parent's own workspace — the only valid answer for a plan that does not
	// require isolation.
	Path string
	// Isolated reports that Path is a tree of its own rather than the parent's.
	Isolated bool
	// Describe is a short human phrase for the approval card and the run
	// summary: the user is being asked to approve writes SOMEWHERE, and where is
	// half the question.
	Describe string
	// Release is called when the plan ends. Never nil after a successful
	// prepare, so a caller can defer it without a check.
	Release func()
	// Abort rolls back ownership when admission fails after preparation and the
	// caller never receives a runnable plan. It is separate from Release because
	// successful plan work may intentionally remain for human review while an
	// unused lease must never remain locked.
	Abort func()
}

// PlanIsolator prepares an isolated workspace for a plan.
//
// nil means this run CANNOT isolate — a headless run in a non-git directory,
// for instance — and a plan that requires isolation is then refused with that
// reason rather than quietly running in the user's tree.
type PlanIsolator func(ctx context.Context, planName string) (PlanWorkspace, error)

// RequiresIsolation reports whether this plan may write, and therefore must not
// run in the parent's tree.
//
// DERIVED FROM THE PLAN, not declared by the caller. A task that names any tool
// outside the read-only set is a task that can change something, and that is
// exactly the condition isolation exists for. Today validateTaskTools rejects
// such a task, so this is always false; when step 3 permits write tools it
// becomes true without a second place needing to be updated — which is the
// class of defect this feature has produced three times.
func (p Plan) RequiresIsolation() bool {
	for _, task := range p.tasks {
		for _, name := range task.Tools {
			if !planReadOnlyTools[name] {
				return true
			}
		}
	}
	return false
}

// resolvePlanWorkspace decides where a plan runs, refusing rather than
// degrading when a plan that must be isolated cannot be.
func resolvePlanWorkspace(ctx context.Context, plan Plan, isolate PlanIsolator) (PlanWorkspace, error) {
	if !plan.RequiresIsolation() {
		// A read-only plan runs where the parent runs. Preparing a worktree for
		// it would cost a checkout to protect a tree nothing can touch.
		return PlanWorkspace{Release: func() {}, Abort: func() {}}, nil
	}
	if isolate == nil {
		return PlanWorkspace{}, fmt.Errorf(
			"plan %q contains tasks that can write, and this run cannot isolate them: "+
				"a write-capable plan runs in a git worktree of its own, and one is not available here. "+
				"Remove the write tools, or run from a git repository", plan.Name())
	}
	workspace, err := isolate(ctx, plan.Name())
	if err != nil {
		return PlanWorkspace{}, fmt.Errorf(
			"plan %q contains tasks that can write and its isolated workspace could not be prepared: %w", plan.Name(), err)
	}
	if !workspace.Isolated || workspace.Path == "" {
		// An isolator that returns success without an isolated path is a bug in
		// the isolator, and honouring it would run write tasks in the user's
		// tree — the exact outcome the precondition exists to prevent.
		return PlanWorkspace{}, fmt.Errorf(
			"plan %q requires isolation and the isolator returned none; refusing to run write-capable tasks in the parent workspace", plan.Name())
	}
	if workspace.Release == nil {
		workspace.Release = func() {}
	}
	if workspace.Abort == nil {
		workspace.Abort = workspace.Release
	}
	// AND THE WORKTREE MUST BE ABLE TO SERVE THE PLAN. The two refusals above
	// cover isolation being unavailable or dishonest; this covers it being
	// available, honest, and useless — a worktree of the wrong tree, which a
	// six-task plan discovered one failed task at a time. See
	// unreachablePlanPaths for the evidence bar this holds itself to.
	//
	// RELEASED BEFORE RETURNING. A refusal that left the worktree on disk would
	// leak one directory per attempt, and the caller has no handle to clean up
	// something it was never given.
	if unreachable := unreachablePlanPaths(plan, workspace); len(unreachable) > 0 {
		workspace.Abort()
		return PlanWorkspace{}, refuseUnreachableWorkspace(plan, workspace, unreachable)
	}
	// THE SAME FAILURE NAMED RELATIVELY. The check above sees only absolute
	// paths, and a measured run that named its targets as "internal/specialist/"
	// carried none — so six tasks searched an empty worktree and the guard said
	// nothing. See planNamesNothingPresent for the conjunction this holds to.
	if named, bare := planNamesNothingPresent(plan, workspace); bare {
		workspace.Abort()
		return PlanWorkspace{}, refuseBareWorkspace(plan, workspace, named)
	}
	return workspace, nil
}
