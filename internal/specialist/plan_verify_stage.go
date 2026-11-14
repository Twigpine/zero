package specialist

// THE CLAIMS NOBODY RE-DERIVES.
//
// A plan fans out, every task reports, and the report carries what they said.
// Under zeromaxing that is five or more sub-agents' worth of claims, each
// written by a model that cannot see the others' work and will never be asked
// about it again. The bundled `research` plan ends in a refute step for exactly
// this reason, and the evidence rules in planTaskSystemPrompt exist to make one
// possible — but both are habits. A plan whose author forgot to add a verifier
// finished green with nothing checked, and a wrong "looks fine" is
// indistinguishable from a right one once it reaches the report.
//
// So under the posture, a multi-task plan that names no verification gets one
// appended: a read-only task depending on every other, told to REFUTE rather
// than to agree.
//
// FIVE THINGS IT MUST NOT DO, each of which is why a condition below exists:
//
//   - Refuse a plan the author wrote. The size tier caps TASK COUNT, so
//     appending to a plan already at its ceiling would turn a valid plan into a
//     rejected one — for a task the author never wrote. No headroom, no append.
//   - Change what the plan may touch. The appended task names NO tools, which
//     planToolGrant resolves to the read-only intersection of the parent's
//     grant. A named write tool would flip RequiresIsolation for the WHOLE plan
//     (isolation is plan-level), moving every other task into a worktree — the
//     measured failure plan_workspace_reach.go exists to catch.
//   - Second-guess a plan that already verifies. classifyTaskRole is the same
//     classifier the model router uses; if any task already reads as verify,
//     this does nothing.
//   - Collide with an author's id. The id is probed against the plan's own ids
//     and suffixed until free.
//   - Appear from nowhere. Every append returns a note, which the tool prints
//     with its assignment notes, so a task in the report that nobody wrote is
//     explained where it happens.

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/Twigpine/zero/internal/tools"
)

// verifyStageTaskID is the appended task's preferred id.
const verifyStageTaskID = "verify_claims"

// verifyStagePrompt is what the appended verifier is told.
//
// IT IS TOLD TO REFUTE, and to default to refuted when uncertain, because a
// verifier that sets out to agree always does — the same reasoning the bundled
// research plan's refute step carries, and the reason that plan ships at all.
// The wording is deliberately about CLAIMS rather than about code: the tasks
// above it may have read, measured or changed things, and the check that
// matters is whether what they reported is what actually happened.
const verifyStagePrompt = "Verify the claims the tasks above made, and try to REFUTE them. " +
	"Default to refuted when uncertain. For each claim: open the cited file:line and check the claim " +
	"is what the code actually does, look for a second path that behaves differently, and look for a " +
	"case the claim does not cover. Any number reported as measured must be traceable to a command " +
	"that was run; if you cannot trace it, say so. Report each claim as VERIFIED with the line that " +
	"proves it, or REFUTED with the line that disproves it, and list separately anything you could not " +
	"check and why. Refuting the other tasks' answers is the job; agreeing is not."

// verifyStageNote explains the appended task wherever notes are shown. Built
// from the id ACTUALLY used, which is suffixed when the author already took the
// preferred one — a note naming a task the plan does not contain is worse than
// no note.
func verifyStageNote(id string) string {
	return id + ": added — no task in this plan verifies the others' claims"
}

// appendVerifyStageToTaskArgs adds a verification task to a plan that has none.
//
// WORKS ON THE ARGS, BEFORE ParsePlan, for the same reason assignModelsToTaskArgs
// does: every downstream property then falls out for free. The appended task is
// validated by the same constructor as a hand-written one, round-trips through
// Plan.Args() into a saved plan, and a resumed plan re-admits exactly what ran.
// Applying it to a parsed Plan instead would mean a second write path into the
// one object ParsePlan exists to be the sole author of.
//
// Returns the tasks unchanged and an empty note whenever any condition fails —
// this is an addition that must never be the reason a plan does not run.
func appendVerifyStageToTaskArgs(tasks []any, maxTasks, budgetMaxTokens, budgetPerTask int) ([]any, string) {
	if len(tasks) < verifyStageMinimumTasks {
		return tasks, ""
	}
	// HEADROOM FIRST, before anything is built: a plan at the tier's ceiling is
	// a plan the author is entitled to run.
	if maxTasks > 0 && len(tasks)+1 > maxTasks {
		return tasks, ""
	}
	// AND THE BUDGET IS A SECOND CEILING, through a door the task-count check
	// does not cover: refuseImplausibleBudget requires max_tokens to fund
	// minimumPlausibleTaskTokens PER TASK, so one more task raises the bar by
	// 50,000. A plan whose budget exactly funded its own tasks was refused
	// outright — "raise max_tokens to at least …" — for a task its author never
	// wrote. The existing suite caught this; without the check the feature turns
	// working plans into rejected ones.
	if budgetMaxTokens > 0 && budgetMaxTokens < (len(tasks)+1)*minimumPlausibleTaskTokens {
		return tasks, ""
	}
	// AND THE PER-TASK CAP IS A THIRD CEILING, missed by the first version of
	// this guard. refuseUnreachablePerTaskCap requires max_tokens to cover
	// max_tokens_per_task × taskCount, so the extra task raises that bar by one
	// whole per-task cap. A 4-task plan with max_tokens 400,000 and
	// max_tokens_per_task 100,000 parses fine and was then REFUSED outright once
	// the verifier made it five — naming a task count and a budget its author
	// never chose. Any plan whose per-task cap exceeds the 50,000 floor above
	// falls in this window, so the floor check alone does not cover it.
	if budgetMaxTokens > 0 && budgetPerTask > 0 && budgetMaxTokens < (len(tasks)+1)*budgetPerTask {
		return tasks, ""
	}
	ids := make([]string, 0, len(tasks))
	taken := map[string]bool{}
	for _, raw := range tasks {
		fields, ok := raw.(map[string]any)
		if !ok {
			// Not an object: ParsePlan owns every message about task shape, and a
			// plan that is about to be refused must not first grow a task.
			return tasks, ""
		}
		id := strings.TrimSpace(planString(fields, "id"))
		if id == "" {
			return tasks, ""
		}
		// ALREADY VERIFIES? Then this has nothing to add. The task's own tools
		// are read for the classification the same way the router reads them, so
		// a write-capable task is "implement" here exactly as it is there.
		if classifyTaskRole(Task{
			ID:     id,
			Prompt: planString(fields, "prompt"),
			Tools:  planStrings(fields, "tools"),
		}) == TaskRoleVerify {
			return tasks, ""
		}
		ids = append(ids, id)
		taken[id] = true
	}

	id := freeVerifyStageID(taken)
	verifier := map[string]any{
		"id":     id,
		"prompt": verifyStagePrompt,
		// depends_on EVERY task, which is what makes this a fan-in: the verifier
		// starts only once there is something to verify, and the dependency
		// briefing carries the others' output into its prompt. No cycle is
		// possible — nothing depends on a task that did not exist a moment ago.
		"depends_on": idsAsAny(ids),
		// NO "tools" KEY. planToolGrant resolves an absent grant to the
		// read-only intersection of the parent's, so this can neither modify
		// anything nor flip the plan into worktree isolation.
	}
	return append(append([]any{}, tasks...), verifier), id
}

// verifyStageMinimumTasks is the smallest plan worth appending to. A single-task
// plan is one agent's work read by the person who asked for it; the failure this
// addresses is claims from SEVERAL agents that nobody re-derives.
const verifyStageMinimumTasks = 2

// freeVerifyStageID returns an id no task in the plan uses.
func freeVerifyStageID(taken map[string]bool) string {
	if !taken[verifyStageTaskID] {
		return verifyStageTaskID
	}
	for suffix := 2; ; suffix++ {
		candidate := verifyStageTaskID + "_" + strconv.Itoa(suffix)
		if !taken[candidate] {
			return candidate
		}
	}
}

func idsAsAny(ids []string) []any {
	out := make([]any, 0, len(ids))
	for _, id := range ids {
		out = append(out, id)
	}
	return out
}

// appendVerifyStage is the tool's gate around appendVerifyStageToTaskArgs: the
// POSTURE decides whether this happens at all.
//
// Off unless zeromaxing is active, so a plain plan is byte-identical to what it
// was before this file existed — the same contract every other part of the
// posture follows. The size ceiling comes from the tool's own limits, so the
// headroom check upstream reads the tier this run is actually bounded by.
func (tool *OrchestrateTool) appendVerifyStage(args map[string]any, options tools.RunOptions) string {
	if !tool.postureActive() {
		return ""
	}
	raw, ok := args["tasks"].([]any)
	if !ok {
		return ""
	}
	// The budget is read from the ARGS, not from a parsed Budget: this runs
	// before the plan is constructed, and planBudget's own refusal for a missing
	// budget object belongs to ParsePlan. An unreadable or absent max_tokens
	// reads as 0 — unbounded — which is exactly the case the ceiling check skips.
	budgetTokens, budgetPerTask := 0, 0
	if budget, ok := args["budget"].(map[string]any); ok {
		budgetTokens = planInt(budget, "max_tokens")
		budgetPerTask = planInt(budget, "max_tokens_per_task")
	}
	tasks, id := appendVerifyStageToTaskArgs(raw, tool.limits(options).MaxTasks, budgetTokens, budgetPerTask)
	if id == "" {
		return ""
	}
	args["tasks"] = tasks
	return id
}

// verifyStageSummary renders the appended-task note for the plan's output.
func verifyStageSummary(id string) string {
	if strings.TrimSpace(id) == "" {
		return ""
	}
	return fmt.Sprintf("\n\nVerification stage added:\n  %s", verifyStageNote(id))
}

// onlyTheAppendedVerifierFailed reports that every task which did NOT succeed is
// the one this file appended.
//
// A TASK THE AUTHOR DID NOT WRITE MUST NOT DECIDE THE AUTHOR'S VERDICT. The
// verifier runs last, on the strongest tier, with the largest dependency
// briefing — the task most likely to stall or exhaust its provider retry — and
// its failure took the whole call to StatusError with it. The orchestrating
// model then reads "the plan failed" and re-runs three tasks that had already
// succeeded, paying twice for work that was done.
//
// The REPORT stays honest either way: Failed still counts it, the summary still
// names it, and the caller is told plainly that the claims went unverified. Only
// the OK/error verdict on the author's plan is left to the author's tasks.
func onlyTheAppendedVerifierFailed(report PlanReport, verifyTaskID string) bool {
	if strings.TrimSpace(verifyTaskID) == "" {
		return false
	}
	sawIt := false
	for _, task := range report.Tasks {
		if task.Outcome == TaskSucceeded {
			continue
		}
		if task.ID != verifyTaskID {
			return false
		}
		sawIt = true
	}
	return sawIt
}

// verifyStageUnverifiedNote is what the caller is told when the appended
// verifier is the only thing that did not succeed: the work stands, the
// checking did not happen, and re-running the plan is not the answer.
func verifyStageUnverifiedNote(id string) string {
	return "\n\nNote: every task you asked for succeeded, but the appended " + id +
		" did not finish, so their claims were NOT independently verified. " +
		"The task results above are unchecked rather than wrong; re-running the plan would repeat work that already succeeded."
}
