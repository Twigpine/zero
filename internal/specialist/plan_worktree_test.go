package specialist

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Twigpine/zero/internal/streamjson"
)

// writePlan is a plan whose task names a write tool. It cannot be built through
// ParsePlan today — validateTaskTools refuses it, which is step 3's job to
// relax — so it is constructed directly to exercise the isolation rule that
// must already be correct when that happens.
func writePlan(t *testing.T, name string) Plan {
	t.Helper()
	base := mustPlan(t, []any{task("a", "x")}, okBudget(), readOnlyLimits())
	base.name = name
	base.tasks[0].Tools = []string{"write_file"}
	return base
}

// ISOLATION IS DERIVED FROM THE PLAN, not declared by a caller. That is what
// makes step 3 safe by construction: the moment write tools are permitted, the
// requirement appears without a second place needing to be updated.
func TestIsolationIsRequiredExactlyWhenAPlanCanWrite(t *testing.T) {
	readOnly := mustPlan(t, []any{
		map[string]any{"id": "a", "prompt": "x", "tools": []any{"grep", "read_file"}},
	}, okBudget(), readOnlyLimits())
	if readOnly.RequiresIsolation() {
		t.Fatal("a read-only plan asked for isolation it does not need")
	}
	if !writePlan(t, "w").RequiresIsolation() {
		t.Fatal("a plan holding write_file did not require isolation")
	}
	// A task with NO explicit tools inherits the parent's read-only grant, so it
	// cannot write and must not force a worktree.
	inherited := mustPlan(t, []any{task("a", "x")}, okBudget(), readOnlyLimits())
	if inherited.RequiresIsolation() {
		t.Fatal("a task inheriting the read-only grant asked for isolation")
	}
}

// A read-only plan runs where the parent runs. Preparing a worktree for it would
// cost a checkout to protect a tree nothing can touch.
func TestAReadOnlyPlanIsNotIsolated(t *testing.T) {
	called := false
	isolate := func(context.Context, string) (PlanWorkspace, error) {
		called = true
		return PlanWorkspace{Path: "/tmp/nope", Isolated: true}, nil
	}
	workspace, err := resolvePlanWorkspace(context.Background(),
		mustPlan(t, []any{task("a", "x")}, okBudget(), readOnlyLimits()), isolate)
	if err != nil {
		t.Fatalf("resolvePlanWorkspace: %v", err)
	}
	if called {
		t.Fatal("a read-only plan prepared a worktree")
	}
	if workspace.Path != "" || workspace.Isolated {
		t.Fatalf("a read-only plan got an isolated workspace: %+v", workspace)
	}
	if workspace.Release == nil {
		t.Fatal("Release must always be callable")
	}
}

// FAIL CLOSED, and this is the whole point of the precondition. A plan that can
// write and cannot be isolated is REFUSED — never run in the user's tree with a
// warning.
func TestAWritePlanWithNoIsolatorIsRefused(t *testing.T) {
	_, err := resolvePlanWorkspace(context.Background(), writePlan(t, "sweep"), nil)
	if err == nil {
		t.Fatal("a write-capable plan ran without isolation")
	}
	for _, want := range []string{"sweep", "cannot isolate", "git repository"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must say %q: %v", want, err)
		}
	}
}

// ...and one whose preparation FAILS is refused too, carrying the reason rather
// than falling back.
func TestAWritePlanWhoseWorktreeFailsIsRefused(t *testing.T) {
	isolate := func(context.Context, string) (PlanWorkspace, error) {
		return PlanWorkspace{}, context.DeadlineExceeded
	}
	_, err := resolvePlanWorkspace(context.Background(), writePlan(t, "sweep"), isolate)
	if err == nil {
		t.Fatal("a write-capable plan ran after its worktree failed to prepare")
	}
	if !strings.Contains(err.Error(), "could not be prepared") {
		t.Fatalf("the refusal must carry the reason: %v", err)
	}
}

// AN ISOLATOR THAT REPORTS SUCCESS WITHOUT ISOLATING IS REFUSED. Honouring it
// would run write tasks in the parent tree — the exact outcome the precondition
// exists to prevent — and it is the failure a buggy isolator produces, not a
// hostile one.
func TestAnIsolatorThatDidNotIsolateIsRefused(t *testing.T) {
	for _, bad := range []PlanWorkspace{
		{Path: "/tmp/x", Isolated: false},
		{Path: "", Isolated: true},
		{},
	} {
		_, err := resolvePlanWorkspace(context.Background(), writePlan(t, "sweep"),
			func(context.Context, string) (PlanWorkspace, error) { return bad, nil })
		if err == nil {
			t.Errorf("an isolator returning %+v was honoured", bad)
		}
	}
}

// A prepared workspace is always releasable, so a caller can defer it without a
// nil check — the check nobody writes.
func TestAPreparedWorkspaceIsAlwaysReleasable(t *testing.T) {
	workspace, err := resolvePlanWorkspace(context.Background(), writePlan(t, "sweep"),
		func(context.Context, string) (PlanWorkspace, error) {
			return PlanWorkspace{Path: "/tmp/x", Isolated: true}, nil
		})
	if err != nil {
		t.Fatalf("resolvePlanWorkspace: %v", err)
	}
	workspace.Release()
}

// THE WORKSPACE REACHES THE TASKS. A worktree prepared and not used is the
// isolation equivalent of a guard that cannot fire.
func TestTheWorkspaceReachesEveryTask(t *testing.T) {
	plan := mustPlan(t, []any{task("a", "x"), task("b", "y", "a")}, okBudget(), readOnlyLimits())
	seen := []string{}
	ExecutePlanIn(context.Background(), plan, PlanWorkspace{Path: "/plan/tree", Isolated: true},
		[]string{"read_file"}, func(_ context.Context, req PlanTaskRequest) (TaskResult, error) {
			seen = append(seen, req.Cwd)
			return TaskResult{Outcome: TaskSucceeded}, nil
		}, nil)
	if len(seen) != 2 {
		t.Fatalf("ran %d tasks", len(seen))
	}
	for _, cwd := range seen {
		if cwd != "/plan/tree" {
			t.Fatalf("a task ran in %q, not the plan's workspace", cwd)
		}
	}
}

// ...and a plan with no workspace hands the tasks nothing, so the runner falls
// back to the parent's directory rather than to an empty string.
func TestNoWorkspaceMeansTheParentsDirectory(t *testing.T) {
	plan := mustPlan(t, []any{task("a", "x")}, okBudget(), readOnlyLimits())
	var got string
	ExecutePlan(context.Background(), plan, []string{"read_file"},
		func(_ context.Context, req PlanTaskRequest) (TaskResult, error) {
			got = req.Cwd
			return TaskResult{Outcome: TaskSucceeded}, nil
		}, nil)
	if got != "" {
		t.Fatalf("a read-only task was handed cwd %q", got)
	}
	if resolved := planTaskCwd("/parent", ""); resolved != "/parent" {
		t.Fatalf("an empty override must fall back to the parent: %q", resolved)
	}
	if resolved := planTaskCwd("/parent", "/plan/tree"); resolved != "/plan/tree" {
		t.Fatalf("the plan's workspace must win: %q", resolved)
	}
}

// THE TOOL REFUSES A WRITE PLAN IT CANNOT ISOLATE, on the FOREGROUND path.
func TestTheToolRefusesAnUnisolatableWritePlan(t *testing.T) {
	tool := &OrchestrateTool{
		PostureActive: func() bool { return true },
		RunTask: func(context.Context, PlanTaskRequest) (TaskResult, error) {
			t.Fatal("a write-capable plan ran without isolation")
			return TaskResult{}, nil
		},
		ParentTools: []string{"read_file"},
	}
	// Reaching the workspace check needs a plan that admits, so this exercises
	// resolvePlanWorkspace directly against the tool's own isolator field.
	if _, err := resolvePlanWorkspace(context.Background(), writePlan(t, "w"), tool.Isolate); err == nil {
		t.Fatal("the tool would have run a write plan with no isolator")
	}
}

// THE REAL RUNNER MUST PUT THE CHILD IN THE PLAN'S WORKSPACE.
//
// Every test above drives ExecutePlanIn with a FAKE runner that reads req.Cwd,
// so all of them pass against a real runner that ignores it — the same shape as
// the Stalled flag, where a fake that fabricates its own inputs cannot test the
// producer. This drives NewPlanRunner and reads the argument the child actually
// receives.
func TestTheRealRunnerLaunchesTheChildInThePlansWorkspace(t *testing.T) {
	capture := func(t *testing.T, override string) []string {
		t.Helper()
		var childArgs []string
		executor := Executor{
			BinaryPath:   "/bin/true",
			NewSessionID: func() (string, error) { return "specialist_0000000000000000000000ww", nil },
			Load:         func(LoadOptions) (LoadResult, error) { return LoadResult{}, nil },
			RunChild: func(_ context.Context, _ string, args []string, _ func(streamjson.Event)) (ChildRunResult, error) {
				childArgs = args
				return ChildRunResult{Started: true, ExitCode: 0}, nil
			},
		}
		runner := NewPlanRunner(PlanTaskContext{
			Executor: executor, Cwd: "/parent/workspace", SpecialistName: "explorer",
		})
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := runner(ctx, PlanTaskRequest{
			Task:         Task{ID: "a", Prompt: "x"},
			Tools:        []string{"read_file"},
			Cwd:          override,
			StallTimeout: time.Minute,
		}); err != nil {
			t.Fatalf("runner: %v", err)
		}
		return childArgs
	}

	cwdArg := func(args []string) string {
		for index, arg := range args {
			if arg == "--cwd" && index+1 < len(args) {
				return args[index+1]
			}
		}
		return ""
	}

	if got := cwdArg(capture(t, "/plan/tree")); got != "/plan/tree" {
		t.Fatalf("the child ran in %q, not the plan's isolated workspace", got)
	}
	// ...and with no override it is the parent's, not empty — an empty --cwd
	// would put a child somewhere neither the plan nor the parent chose.
	if got := cwdArg(capture(t, "")); got != "/parent/workspace" {
		t.Fatalf("a read-only task ran in %q, not the parent's workspace", got)
	}
}

// THE PLAN TASK'S PROMPT MUST DEMAND TOOL USE, not merely mention tools.
//
// The first version said "You have read-only tools" and asked for nothing. A
// real fifteen-task run then spent 260 seconds on its first task with ZERO tool
// calls — the model answered a "find every definition and quote the file:line"
// task from its own weights. The stall watchdog cannot catch that: it keys on
// silence, and a model writing prose is not silent.
func TestThePlanTaskPromptDemandsToolUse(t *testing.T) {
	manifest := planTaskManifest("explorer", "", "", []string{"read_file", "grep"})
	prompt := manifest.SystemPrompt

	// The obligation, not just the offer.
	for _, want := range []string{"USE THEM", "before you answer", "file:line"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the plan task prompt must contain %q:\n%s", want, prompt)
		}
	}
	// And the honest-failure clause, which is what stops a task that cannot find
	// something from inventing it — a guess is indistinguishable from a finding
	// once it reaches the plan's report.
	if !strings.Contains(prompt, "not found") {
		t.Errorf("the prompt must license an honest failure:\n%s", prompt)
	}
	// The read-only obligation is unchanged: this task may look, never modify.
	if !strings.Contains(prompt, "do not attempt to modify anything") {
		t.Errorf("the prompt lost its read-only instruction:\n%s", prompt)
	}
}
