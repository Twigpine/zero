package specialist

import (
	"context"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/streamjson"
	"github.com/Twigpine/zero/internal/tools"
)

func argsContainPair(args []string, flag, value string) bool {
	for index, arg := range args {
		if arg == flag && index+1 < len(args) && args[index+1] == value {
			return true
		}
	}
	return false
}

// A CHILD MUST STAND ON THE SAME GROUND AS ITS PARENT, not on less.
//
// A run was granted ~/zm-lab mid-session, created it, copied packages into it,
// then dispatched plan tasks to read them. Every task was refused — "the target
// directory is outside the workspace boundary" — because a child rebuilds its
// sandbox from --cwd alone and the grant lived only in the parent's engine.
// Two tasks died, two dependents were skipped, and a whole retry plan was spent
// rediscovering the boundary.
//
// Asserted from ARGV, which is the only thing the child actually receives.
func TestAChildIsLaunchedWithTheRunsExtraRoots(t *testing.T) {
	executor := Executor{
		NewSessionID:    func() (string, error) { return "specialist_00000000000000000000000a", nil },
		ExtraWriteRoots: func() []string { return []string{"/Users/kratos/dev/zero", "/Users/kratos/zm-lab"} },
	}
	built, err := executor.BuildArgs(BuildArgsInput{
		Manifest: Manifest{Metadata: Metadata{Name: "explorer"}},
		Prompt:   "read the packages",
		Cwd:      "/Users/kratos/dev/zero",
	})
	if err != nil {
		t.Fatalf("BuildArgs: %v", err)
	}
	if !argsContainPair(built.Args, "--add-dir", "/Users/kratos/zm-lab") {
		t.Fatalf("the granted root never reached the child:\n%s", strings.Join(built.Args, " "))
	}
	// THE WORKSPACE IS NOT REPEATED. --cwd already says it, and saying it twice
	// is a line every future reader has to check.
	if argsContainPair(built.Args, "--add-dir", "/Users/kratos/dev/zero") {
		t.Errorf("the workspace was passed again as an extra root:\n%s", strings.Join(built.Args, " "))
	}
}

// A READ GRANT REACHES THE CHILD AS --add-read-dir, NEVER --add-dir. Routing a
// path the parent may only READ through the write flag would make it writable in
// the child — a read grant escalated to write. This is the launch-side half of
// the anti-escalation guarantee (the scope-side half is in internal/sandbox).
func TestAReadOnlyRootIsLaunchedAsReadNotWrite(t *testing.T) {
	executor := Executor{
		NewSessionID: func() (string, error) { return "specialist_00000000000000000000000a", nil },
	}
	built, err := executor.BuildArgs(BuildArgsInput{
		Manifest:      Manifest{Metadata: Metadata{Name: "explorer"}},
		Prompt:        "audit the repo",
		Cwd:           "/Users/kratos/zero-p7-demo",
		ReadOnlyRoots: []string{"/Users/kratos/dev/zero"},
	})
	if err != nil {
		t.Fatalf("BuildArgs: %v", err)
	}
	if !argsContainPair(built.Args, "--add-read-dir", "/Users/kratos/dev/zero") {
		t.Fatalf("the read grant never reached the child as --add-read-dir:\n%s", strings.Join(built.Args, " "))
	}
	if argsContainPair(built.Args, "--add-dir", "/Users/kratos/dev/zero") {
		t.Fatalf("the read grant was emitted as --add-dir (write) — a read grant escalated to write:\n%s", strings.Join(built.Args, " "))
	}
}

// READ AT LAUNCH, NOT AT WIRING. The case this exists for is a permission
// granted MID-SESSION; a value captured when the tool was registered would miss
// exactly that, which is the staleness that made model discovery probe the
// provider a session had already switched away from.
func TestTheRunsRootsAreReadAtEveryLaunchNotCapturedOnce(t *testing.T) {
	roots := []string{"/ws"}
	executor := Executor{
		NewSessionID:    func() (string, error) { return "specialist_00000000000000000000000a", nil },
		ExtraWriteRoots: func() []string { return roots },
	}
	input := BuildArgsInput{
		Manifest: Manifest{Metadata: Metadata{Name: "explorer"}}, Prompt: "p", Cwd: "/ws",
	}
	if built, _ := executor.BuildArgs(input); argsContainPair(built.Args, "--add-dir", "/granted-later") {
		t.Fatal("setup: the root exists before it was granted")
	}

	// The user approves a new directory partway through the session.
	roots = append(roots, "/granted-later")

	built, err := executor.BuildArgs(input)
	if err != nil {
		t.Fatalf("BuildArgs: %v", err)
	}
	if !argsContainPair(built.Args, "--add-dir", "/granted-later") {
		t.Errorf("a mid-session grant never reached a child launched after it:\n%s", strings.Join(built.Args, " "))
	}
}

// UNWIRED MEANS THE WORKSPACE ONLY, which is what every child got before this
// existed. The fail-safe direction: a child confined more tightly than its
// parent is a smaller problem than one confined less.
func TestAnUnwiredSupplierLeavesTheChildConfinedToItsWorkspace(t *testing.T) {
	executor := Executor{NewSessionID: func() (string, error) { return "specialist_00000000000000000000000a", nil }}
	built, err := executor.BuildArgs(BuildArgsInput{
		Manifest: Manifest{Metadata: Metadata{Name: "explorer"}}, Prompt: "p", Cwd: "/ws",
	})
	if err != nil {
		t.Fatalf("BuildArgs: %v", err)
	}
	for _, arg := range built.Args {
		if arg == "--add-dir" {
			t.Errorf("an unwired supplier widened the child anyway:\n%s", strings.Join(built.Args, " "))
		}
	}
}

// A blank root must not become an empty flag value, which the child rejects
// outright — turning a scope detail into a launch failure.
func TestBlankRootsAreDroppedRatherThanEmittedAsEmptyFlags(t *testing.T) {
	executor := Executor{
		NewSessionID:    func() (string, error) { return "specialist_00000000000000000000000a", nil },
		ExtraWriteRoots: func() []string { return []string{"", "   ", "/real"} },
	}
	built, err := executor.BuildArgs(BuildArgsInput{
		Manifest: Manifest{Metadata: Metadata{Name: "explorer"}}, Prompt: "p", Cwd: "/ws",
	})
	if err != nil {
		t.Fatalf("BuildArgs: %v", err)
	}
	if argsContainPair(built.Args, "--add-dir", "") {
		t.Error("a blank root was emitted as an empty --add-dir value")
	}
	if !argsContainPair(built.Args, "--add-dir", "/real") {
		t.Error("the real root was dropped alongside the blank ones")
	}
}

// THE RESUME PATH CARRIES THE SAME ROOTS. This file's history is why it is
// asserted separately: the resume path once forgot the model the fresh path
// carried, and a resumed task silently ran on a different one. A resumed task
// confined more tightly than the task it resumes would fail on files it had
// already read.
func TestAResumedChildCarriesTheSameRootsAsAFreshOne(t *testing.T) {
	executor := Executor{
		NewSessionID:    func() (string, error) { return "specialist_00000000000000000000000a", nil },
		ExtraWriteRoots: func() []string { return []string{"/ws", "/granted"} },
	}
	fresh, err := executor.BuildArgs(BuildArgsInput{
		Manifest: Manifest{Metadata: Metadata{Name: "explorer"}}, Prompt: "p", Cwd: "/ws",
	})
	if err != nil {
		t.Fatalf("BuildArgs: %v", err)
	}
	resumed, err := executor.BuildResumeArgs(BuildResumeArgsInput{
		SessionID: "specialist_00000000000000000000000a",
		Manifest:  Manifest{Metadata: Metadata{Name: "explorer"}}, Prompt: "p", Cwd: "/ws",
	})
	if err != nil {
		t.Fatalf("BuildResumeArgs: %v", err)
	}
	if !argsContainPair(fresh.Args, "--add-dir", "/granted") {
		t.Fatal("setup: the fresh path does not carry the root")
	}
	if !argsContainPair(resumed.Args, "--add-dir", "/granted") {
		t.Errorf("the resume path dropped a root the fresh path carries:\n%s", strings.Join(resumed.Args, " "))
	}
}

// A PLAN TASK'S CHILD MUST BE TRACEABLE TO THE CALL THAT SPAWNED IT.
//
// The Task tool carries the parent's tool-call id; this path did not, so a plan
// task's child was the only kind whose accounting named no originating call —
// spend recorded against a session with nothing saying what asked for it.
//
// Asserted from ARGV, which is what the child actually receives. A field set on
// the request and dropped before launch would pass a struct assertion and fail
// in production, which is how this family of bug survives.
func TestAPlanTaskChildIsLaunchedWithItsOriginatingToolCallID(t *testing.T) {
	var argv []string
	exec := Executor{
		BinaryPath:   "/bin/true",
		NewSessionID: func() (string, error) { return "specialist_00000000000000000000000a", nil },
		Load:         func(LoadOptions) (LoadResult, error) { return LoadResult{}, nil },
		RunChild: func(_ context.Context, _ string, args []string, _ func(streamjson.Event)) (ChildRunResult, error) {
			argv = args
			return ChildRunResult{Started: true}, nil
		},
	}
	run := NewPlanRunner(PlanTaskContext{Executor: exec, Cwd: t.TempDir(), SpecialistName: "explorer"})
	if _, err := run(context.Background(), PlanTaskRequest{
		Task:             Task{ID: "t", Prompt: "p"},
		Tools:            []string{"read_file"},
		ParentToolCallID: "call_orchestrate_42",
	}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !argsContainPair(argv, "--calling-tool-use-id", "call_orchestrate_42") {
		t.Fatalf("the originating call never reached the child:\n%s", strings.Join(argv, " "))
	}
}

// AND THE TOOL MUST ACTUALLY ATTACH IT. The test above hands the runner a
// request with the id already on it, which proves the runner forwards it and
// proves nothing about whether anything ever puts it there — a mutation
// deleting the tool's assignment passed that test cleanly.
//
// This is the same shape as the defect being fixed: a value present at one
// layer, consumed at another, with nothing asserting the join. Two seams, two
// tests.
func TestTheOrchestrateToolAttachesItsOwnCallIDToEveryTask(t *testing.T) {
	var seen []string
	tool := &OrchestrateTool{
		PostureActive: func() bool { return true },
		// The run's grant: a plan task inherits from it and can never widen it,
		// so with none the tasks are refused before dispatch and this test would
		// pass for the wrong reason.
		ParentTools: []string{"read_file", "grep"},
		RunTask: func(_ context.Context, req PlanTaskRequest) (TaskResult, error) {
			seen = append(seen, req.ParentToolCallID)
			return TaskResult{Outcome: TaskSucceeded, Output: "ok"}, nil
		},
	}
	result := tool.RunWithOptions(context.Background(), map[string]any{
		"name":   "p",
		"budget": map[string]any{"max_workers": float64(1)},
		"tasks": []any{
			map[string]any{"id": "a", "prompt": "one"},
			map[string]any{"id": "b", "prompt": "two"},
		},
	}, tools.RunOptions{Model: "m", ToolCallID: "call_orchestrate_42"})

	if result.Status == tools.StatusError {
		t.Fatalf("plan refused: %s", result.Output)
	}
	// THREE: the plan's own two, plus the verification task the posture appends
	// to a multi-task plan that names no verifier (plan_verify_stage.go). The
	// property under test is that EVERY dispatched task carries the originating
	// call id — an appended one included, since it is dispatched by the same
	// runner — so the count is asserted as "the plan's tasks plus the verifier"
	// rather than pinned at the number this plan happens to declare.
	if len(seen) != 3 {
		t.Fatalf("expected the two plan tasks plus the appended verifier, saw %d", len(seen))
	}
	for index, id := range seen {
		if id != "call_orchestrate_42" {
			t.Errorf("task %d was dispatched with no originating call id: %q", index, id)
		}
	}
}
