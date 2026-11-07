package specialist

import (
	"context"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/tools"
)

// THE CLAIMS NOBODY RE-DERIVES. A multi-task plan under the posture that names
// no verifier gets one appended: read-only, depending on every other task, told
// to refute. Everything below is a way that must NOT cost the author their plan.

func verifyStageTasks(ids ...string) []any {
	out := make([]any, 0, len(ids))
	for _, id := range ids {
		out = append(out, map[string]any{"id": id, "prompt": "trace the " + id + " path"})
	}
	return out
}

func taskIDsOf(t *testing.T, tasks []any) []string {
	t.Helper()
	out := make([]string, 0, len(tasks))
	for _, raw := range tasks {
		fields, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("task is not an object: %#v", raw)
		}
		out = append(out, planString(fields, "id"))
	}
	return out
}

func TestAVerifierIsAppendedToAnUnverifiedPlan(t *testing.T) {
	tasks, note := appendVerifyStageToTaskArgs(verifyStageTasks("scan", "trace"), 20, 0, 0)
	if note == "" {
		t.Fatal("a multi-task plan with no verifier must get one")
	}
	ids := taskIDsOf(t, tasks)
	if len(ids) != 3 || ids[2] != verifyStageTaskID {
		t.Fatalf("ids = %v, want the two originals plus %q last", ids, verifyStageTaskID)
	}
	verifier, _ := tasks[2].(map[string]any)
	// FAN-IN: it must depend on every task, or it can start before there is
	// anything to verify.
	deps := planStrings(verifier, "depends_on")
	if len(deps) != 2 || deps[0] != "scan" || deps[1] != "trace" {
		t.Fatalf("depends_on = %v, want every task in the plan", deps)
	}
	// NO TOOLS KEY: planToolGrant then resolves the read-only intersection of the
	// parent's grant, which is what keeps this from flipping the whole plan into
	// worktree isolation.
	if _, present := verifier["tools"]; present {
		t.Fatal("the verifier must name no tools, or a write grant could isolate the plan")
	}
	// It must be told to refute, not to agree.
	prompt := planString(verifier, "prompt")
	if !strings.Contains(prompt, "REFUTE") || !strings.Contains(prompt, "Default to refuted") {
		t.Fatalf("the verifier must be told to refute and default to refuted: %q", prompt)
	}
	// And it classifies as verify, so the user's verify pin routes it.
	if role := classifyTaskRole(Task{ID: verifyStageTaskID, Prompt: prompt}); role != TaskRoleVerify {
		t.Fatalf("the appended task classifies as %q, want verify — the verify pin would not route it", role)
	}
}

// A plan that already verifies is left alone: this adds a missing habit, it does
// not second-guess an author who has one.
func TestAPlanThatAlreadyVerifiesIsUntouched(t *testing.T) {
	tasks := append(verifyStageTasks("scan", "trace"),
		map[string]any{"id": "check", "prompt": "review the two findings above and confirm them"})
	got, note := appendVerifyStageToTaskArgs(tasks, 20, 0, 0)
	if note != "" || len(got) != 3 {
		t.Fatalf("a plan with a verify task must be untouched: note=%q tasks=%d", note, len(got))
	}
}

// A single-task plan is one agent's work, read by the person who asked for it.
func TestASingleTaskPlanGetsNoVerifier(t *testing.T) {
	if _, note := appendVerifyStageToTaskArgs(verifyStageTasks("only"), 20, 0, 0); note != "" {
		t.Fatal("a one-task plan must not grow a verifier")
	}
}

// THE APPEND MUST NEVER BE THE REASON A PLAN IS REFUSED. Two ceilings can do
// that, and each has its own guard: the size tier caps task COUNT, and
// refuseImplausibleBudget requires max_tokens to fund 50,000 per task — so one
// more task raises the bar by 50,000. The budget case was found by the existing
// suite, not by this test: without the guard, TestOrchestrateForwardsProgress…
// failed with "raise max_tokens to at least 150000".
func TestTheAppendNeverCostsAnAuthorTheirPlan(t *testing.T) {
	atTierCeiling := verifyStageTasks("a", "b", "c", "d", "e")
	if _, note := appendVerifyStageToTaskArgs(atTierCeiling, 5, 0, 0); note != "" {
		t.Fatal("a plan at the size tier's ceiling must not be pushed over it")
	}
	if _, note := appendVerifyStageToTaskArgs(atTierCeiling, 6, 0, 0); note == "" {
		t.Fatal("one task of headroom is enough; the append should have happened")
	}
	// Budget exactly funds the two declared tasks (2 × 50,000) and nothing more.
	twoTasks := verifyStageTasks("a", "b")
	if _, note := appendVerifyStageToTaskArgs(twoTasks, 20, 2*minimumPlausibleTaskTokens, 0); note != "" {
		t.Fatal("a plan whose budget exactly funds its own tasks must not be pushed over the floor")
	}
	if _, note := appendVerifyStageToTaskArgs(twoTasks, 20, 3*minimumPlausibleTaskTokens, 0); note == "" {
		t.Fatal("a budget with room for one more task should have taken the verifier")
	}
	// An unset budget is unbounded, not zero-budget: the ceiling check skips it.
	if _, note := appendVerifyStageToTaskArgs(twoTasks, 20, 0, 0); note == "" {
		t.Fatal("an unbounded plan must still get a verifier")
	}
}

// An author who already used the id keeps it; the appended task moves aside.
func TestTheAppendedIDNeverCollides(t *testing.T) {
	tasks := []any{
		map[string]any{"id": verifyStageTaskID, "prompt": "trace the parser"},
		map[string]any{"id": verifyStageTaskID + "_2", "prompt": "trace the lexer"},
	}
	got, note := appendVerifyStageToTaskArgs(tasks, 20, 0, 0)
	if note == "" {
		t.Fatal("setup: the append should have happened")
	}
	ids := taskIDsOf(t, got)
	if ids[2] != verifyStageTaskID+"_3" {
		t.Fatalf("appended id = %q, want the first free suffix", ids[2])
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			t.Fatalf("duplicate id %q — ParsePlan would refuse the whole plan", id)
		}
		seen[id] = true
	}
}

// A malformed task list is ParsePlan's to report, and a plan about to be refused
// must not first grow a task.
func TestAMalformedTaskListIsLeftToParsePlan(t *testing.T) {
	for _, tasks := range [][]any{
		{"not an object", map[string]any{"id": "b", "prompt": "x"}},
		{map[string]any{"prompt": "no id here"}, map[string]any{"id": "b", "prompt": "x"}},
	} {
		if got, note := appendVerifyStageToTaskArgs(tasks, 20, 0, 0); note != "" || len(got) != len(tasks) {
			t.Fatalf("a malformed list must be untouched: note=%q", note)
		}
	}
}

// THE POSTURE IS THE GATE. Off, a plan is byte-identical to what it was before
// this file existed — the contract every other part of the posture follows.
func TestTheVerifyStageIsPostureGated(t *testing.T) {
	args := func() map[string]any {
		return map[string]any{
			"name":   "p",
			"budget": map[string]any{"max_workers": float64(1)},
			"tasks":  verifyStageTasks("scan", "trace"),
		}
	}
	off := &OrchestrateTool{}
	offArgs := args()
	if note := off.appendVerifyStage(offArgs, tools.RunOptions{}); note != "" {
		t.Fatalf("posture off must append nothing, got %q", note)
	}
	if tasks, _ := offArgs["tasks"].([]any); len(tasks) != 2 {
		t.Fatalf("posture off rewrote the task list: %d tasks", len(tasks))
	}
	on := &OrchestrateTool{PostureActive: func() bool { return true }}
	onArgs := args()
	if note := on.appendVerifyStage(onArgs, tools.RunOptions{}); note == "" {
		t.Fatal("posture on must append the verifier")
	}
	if tasks, _ := onArgs["tasks"].([]any); len(tasks) != 3 {
		t.Fatalf("posture on did not rewrite the task list: %d tasks", len(tasks))
	}
}

// THE WHOLE PATH: the appended task is dispatched like any other, and the run
// says it was added rather than letting a task nobody wrote appear unexplained.
func TestTheAppendedVerifierRunsAndIsReported(t *testing.T) {
	var dispatched []string
	tool := &OrchestrateTool{
		PostureActive: func() bool { return true },
		ParentTools:   []string{"read_file", "grep"},
		RunTask: func(_ context.Context, req PlanTaskRequest) (TaskResult, error) {
			dispatched = append(dispatched, req.Task.ID)
			return TaskResult{Outcome: TaskSucceeded, Output: "ok"}, nil
		},
	}
	result := tool.RunWithOptions(context.Background(), map[string]any{
		"name":   "audit",
		"budget": map[string]any{"max_workers": float64(1)},
		"tasks":  verifyStageTasks("scan", "trace"),
	}, tools.RunOptions{Model: "m"})

	if result.Status == tools.StatusError {
		t.Fatalf("plan refused: %s", result.Output)
	}
	if len(dispatched) != 3 || dispatched[2] != verifyStageTaskID {
		t.Fatalf("dispatched %v, want the verifier last", dispatched)
	}
	if !strings.Contains(result.Output, verifyStageTaskID) {
		t.Fatalf("the run must say a verifier was added:\n%s", result.Output)
	}
}

// THE THIRD CEILING. refuseUnreachablePerTaskCap requires max_tokens to cover
// max_tokens_per_task × taskCount, so the extra task raises that bar by one
// whole per-task cap — a window the 50,000 floor check does not cover whenever
// the per-task cap exceeds it. A 4-task plan with max_tokens 400,000 and
// max_tokens_per_task 100,000 parsed fine and was then refused outright.
func TestTheAppendRespectsThePerTaskCapCeiling(t *testing.T) {
	four := verifyStageTasks("a", "b", "c", "d")
	// Exactly funded for four at 100k each: the fifth task would need 500k.
	if _, note := appendVerifyStageToTaskArgs(four, 20, 400_000, 100_000); note != "" {
		t.Fatal("a plan whose per-task cap exactly consumes max_tokens must not be pushed over it")
	}
	// Room for five: the append is welcome.
	if _, note := appendVerifyStageToTaskArgs(four, 20, 500_000, 100_000); note == "" {
		t.Fatal("a budget with room for the verifier should have taken it")
	}
	// No per-task cap set: only the 50k floor applies, as before.
	if _, note := appendVerifyStageToTaskArgs(four, 20, 400_000, 0); note == "" {
		t.Fatal("without max_tokens_per_task the floor check alone governs")
	}
}

// A TASK THE AUTHOR DID NOT WRITE MUST NOT DECIDE THE AUTHOR'S VERDICT. The
// verifier runs last, on the strongest tier, with the largest briefing — the
// task most likely to stall — and its failure took the whole call to
// StatusError, so the orchestrating model re-ran a plan whose work was done.
func TestTheAppendedVerifiersFailureDoesNotFailThePlan(t *testing.T) {
	report := PlanReport{
		Status:    PlanPartial,
		Succeeded: 3,
		Failed:    1,
		Tasks: []TaskResult{
			{ID: "a", Outcome: TaskSucceeded}, {ID: "b", Outcome: TaskSucceeded},
			{ID: "c", Outcome: TaskSucceeded}, {ID: verifyStageTaskID, Outcome: TaskFailed},
		},
	}
	if !onlyTheAppendedVerifierFailed(report, verifyStageTaskID) {
		t.Fatal("the verifier alone failing must be recognised as such")
	}
	// An author's task failing alongside it is a real failure, verifier or not.
	report.Tasks[0].Outcome = TaskFailed
	if onlyTheAppendedVerifierFailed(report, verifyStageTaskID) {
		t.Fatal("an author task failed too; the plan really did fail")
	}
	// And with nothing appended, the exemption never applies.
	report.Tasks[0].Outcome = TaskSucceeded
	if onlyTheAppendedVerifierFailed(report, "") {
		t.Fatal("no appended task means no exemption")
	}
}
