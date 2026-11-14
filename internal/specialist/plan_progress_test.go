package specialist

import (
	"context"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/streamjson"
	"github.com/Twigpine/zero/internal/tools"
)

// progressExecutor is an Executor whose child immediately emits one stream-json
// event, so a test can observe whether the progress callback reached it.
func progressExecutor(t *testing.T) Executor {
	t.Helper()
	return Executor{
		BinaryPath:   "/bin/true",
		NewSessionID: func() (string, error) { return "specialist_00000000000000000000000a", nil },
		Load:         func(LoadOptions) (LoadResult, error) { return LoadResult{}, nil },
		RunChild: func(_ context.Context, _ string, _ []string, progress func(streamjson.Event)) (ChildRunResult, error) {
			if progress != nil {
				progress(streamjson.Event{Type: streamjson.EventToolCall, Name: "read_file"})
			}
			return ChildRunResult{Started: true}, nil
		},
	}
}

// THE SECOND CONSTRUCTION PATH, both halves, asserted end to end.
//
// The Task tool forwards its caller's progress callback into TaskRunOptions;
// the plan runner built the same struct and omitted the field, and the
// orchestrate tool never read options.Progress at all. Either half alone leaves
// a plan's children streaming to nobody, which is why they are one change and
// one test: a test that only covered the runner would have passed while the
// tool dropped the callback on the floor, and vice versa.
func TestOrchestrateForwardsProgressToEveryTasksChild(t *testing.T) {
	var events int
	gate := &PostureGate{}
	gate.Set(true)

	tool := &OrchestrateTool{
		PostureActive: gate.Active,
		ParentTools:   []string{"read_file"},
		RunTask: NewPlanRunner(PlanTaskContext{
			Executor:       progressExecutor(t),
			Cwd:            t.TempDir(),
			SpecialistName: "explorer",
		}),
	}

	result := tool.RunWithOptions(context.Background(), map[string]any{
		"name": "p",
		"tasks": []any{
			map[string]any{"id": "a", "prompt": "one"},
			map[string]any{"id": "b", "prompt": "two"},
		},
		"budget": map[string]any{"max_workers": float64(1), "max_tokens": float64(100000)},
	}, tools.RunOptions{
		Progress: func(streamjson.Event) { events++ },
	})

	if result.Status == tools.StatusError {
		t.Fatalf("plan failed: %s", result.Output)
	}
	if events != 2 {
		t.Fatalf("saw %d progress events, want one per task: a plan task's child must stream exactly as a Task sub-agent's does", events)
	}
}

// The runner half on its own: a request carrying a callback must put it on
// TaskRunOptions. Pinned separately so a regression names which half broke.
func TestPlanRunnerForwardsProgress(t *testing.T) {
	var got bool
	runner := NewPlanRunner(PlanTaskContext{
		Executor:       progressExecutor(t),
		Cwd:            t.TempDir(),
		SpecialistName: "explorer",
	})
	if _, err := runner(context.Background(), PlanTaskRequest{
		Task:     Task{ID: "a", Prompt: "x"},
		Tools:    []string{"read_file"},
		Progress: func(streamjson.Event) { got = true },
	}); err != nil {
		t.Fatalf("runner: %v", err)
	}
	if !got {
		t.Fatal("the runner dropped the progress callback; its child streamed to nobody")
	}
}

// A caller that wires no progress still gets a callback into the executor —
// the WATCHDOG needs the liveness signal whether or not a UI wants the events.
// What must not happen is the caller's own callback being invented.
//
// This test used to assert the opposite (nil stays nil). That was right until
// the stall watchdog existed: with no feed, a task whose child is talking
// happily would be judged silent and killed. The executor is unaffected either
// way — `progress != nil` gates only the call, and the event is parsed and
// stored regardless — so the cost is one function call per event.
func TestPlanRunnerAlwaysFeedsTheWatchdogEvenWithNoCallerCallback(t *testing.T) {
	var sawCallback bool
	executor := Executor{
		BinaryPath:   "/bin/true",
		NewSessionID: func() (string, error) { return "specialist_00000000000000000000000a", nil },
		Load:         func(LoadOptions) (LoadResult, error) { return LoadResult{}, nil },
		RunChild: func(_ context.Context, _ string, _ []string, progress func(streamjson.Event)) (ChildRunResult, error) {
			sawCallback = progress != nil
			// Emitting is safe: an unwired caller must not be invoked, and the
			// watchdog must be.
			if progress != nil {
				progress(streamjson.Event{Type: streamjson.EventToolCall})
			}
			return ChildRunResult{Started: true}, nil
		},
	}
	runner := NewPlanRunner(PlanTaskContext{Executor: executor, Cwd: t.TempDir(), SpecialistName: "explorer"})
	if _, err := runner(context.Background(), PlanTaskRequest{Task: Task{ID: "a", Prompt: "x"}, Tools: []string{"read_file"}}); err != nil {
		t.Fatalf("runner: %v", err)
	}
	if !sawCallback {
		t.Fatal("the executor got no callback, so the watchdog cannot see the child is alive")
	}
}

// Q9: a plan task inherits the parent's model.
//
// The struct comment always claimed it did; the code did not. Both production
// call sites left PlanTaskContext.ParentModel empty, so appendModelArgs got no
// parent model and passed no --model — a plan task ran on whatever the CHILD's
// config resolved, which after a /model switch is a different model entirely.
//
// The fix is not "populate the field at registration": the TUI's registry is
// built once per session while /model changes the model between runs, so a
// value captured there would be stale by design. The three values arrive per
// call, from the same tools.RunOptions the Task tool reads.
func TestPlanTaskInheritsTheParentsModel(t *testing.T) {
	var childArgs []string
	executor := Executor{
		BinaryPath:   "/bin/true",
		NewSessionID: func() (string, error) { return "specialist_00000000000000000000000a", nil },
		Load:         func(LoadOptions) (LoadResult, error) { return LoadResult{}, nil },
		RunChild: func(_ context.Context, _ string, args []string, _ func(streamjson.Event)) (ChildRunResult, error) {
			childArgs = args
			return ChildRunResult{Started: true}, nil
		},
	}
	gate := &PostureGate{}
	gate.Set(true)
	tool := &OrchestrateTool{
		PostureActive: gate.Active,
		ParentTools:   []string{"read_file"},
		RunTask:       NewPlanRunner(PlanTaskContext{Executor: executor, Cwd: t.TempDir(), SpecialistName: "explorer"}),
	}

	tool.RunWithOptions(context.Background(), map[string]any{
		"name":   "p",
		"tasks":  []any{map[string]any{"id": "a", "prompt": "x"}},
		"budget": map[string]any{"max_workers": float64(1), "max_tokens": float64(100000)},
	}, tools.RunOptions{
		Model:           "parent-chose-this",
		SessionID:       "zero_parent_session",
		ReasoningEffort: "high",
	})

	joined := strings.Join(childArgs, " ")
	if !strings.Contains(joined, "--model parent-chose-this") {
		t.Fatalf("the plan task did not inherit the parent's model:\n%s", joined)
	}
	// The parent SESSION travels with the model: it is what links the child
	// back to the run that spawned it, so a plan task stays drillable from its
	// parent rather than looking like an orphan.
	if !strings.Contains(joined, "zero_parent_session") {
		t.Fatalf("the plan task did not inherit the parent's session id:\n%s", joined)
	}
}

// The parent identity is read per CALL. A second call with a different model
// must launch its task on that model — the whole reason these values do not
// live on the registration-time context.
func TestASecondCallUsesTheNewParentModel(t *testing.T) {
	var models []string
	executor := Executor{
		BinaryPath:   "/bin/true",
		NewSessionID: func() (string, error) { return "specialist_00000000000000000000000a", nil },
		Load:         func(LoadOptions) (LoadResult, error) { return LoadResult{}, nil },
		RunChild: func(_ context.Context, _ string, args []string, _ func(streamjson.Event)) (ChildRunResult, error) {
			for index, arg := range args {
				if arg == "--model" && index+1 < len(args) {
					models = append(models, args[index+1])
				}
			}
			return ChildRunResult{Started: true}, nil
		},
	}
	gate := &PostureGate{}
	gate.Set(true)
	tool := &OrchestrateTool{
		PostureActive: gate.Active,
		ParentTools:   []string{"read_file"},
		RunTask:       NewPlanRunner(PlanTaskContext{Executor: executor, Cwd: t.TempDir(), SpecialistName: "explorer"}),
	}
	args := map[string]any{
		"name":   "p",
		"tasks":  []any{map[string]any{"id": "a", "prompt": "x"}},
		"budget": map[string]any{"max_workers": float64(1), "max_tokens": float64(100000)},
	}

	tool.RunWithOptions(context.Background(), args, tools.RunOptions{Model: "first-model"})
	tool.RunWithOptions(context.Background(), args, tools.RunOptions{Model: "second-model"})

	if len(models) != 2 || models[0] != "first-model" || models[1] != "second-model" {
		t.Fatalf("models = %v; the parent model must be read per call, not captured at registration", models)
	}
}
