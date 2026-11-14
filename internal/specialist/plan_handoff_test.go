package specialist

import (
	"context"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/streamjson"
)

// overspendingChild emits the shape of a task killed mid-edit-loop: tool results
// that changed files, and NOT ONE line of prose — which is exactly why the
// measured run handed on nothing.
func overspendingChild(t *testing.T, perEvent int, files ...string) PlanRunner {
	t.Helper()
	executor := Executor{
		BinaryPath:   "/bin/true",
		NewSessionID: func() (string, error) { return "specialist_00000000000000000000000a", nil },
		Load:         func(LoadOptions) (LoadResult, error) { return LoadResult{}, nil },
		RunChild: func(ctx context.Context, _ string, _ []string, progress func(streamjson.Event)) (ChildRunResult, error) {
			for _, file := range files {
				if progress != nil {
					progress(streamjson.Event{Type: streamjson.EventToolCall, Name: "edit_file"})
					progress(streamjson.Event{Type: streamjson.EventToolResult, ChangedFiles: []string{file}})
					progress(streamjson.Event{Type: streamjson.EventUsage, TotalTokens: &perEvent})
				}
			}
			<-ctx.Done()
			return ChildRunResult{Started: true, ExitCode: -1}, ctx.Err()
		},
	}
	return NewPlanRunner(PlanTaskContext{Executor: executor, Cwd: t.TempDir(), SpecialistName: "explorer"})
}

// A TASK STOPPED BY ITS BUDGET MUST HAND SOMETHING ON.
//
// A measured plan lost 858,231 tokens of real work this way: m6-bytecode-vm was
// stopped at its per-task cap having edited vm.go nine times, and returned ZERO
// characters — because a task killed mid-edit-loop has written no prose. The
// briefing that carries a cut-short task's output to its dependents worked
// correctly on an empty input.
func TestABudgetCancelledTaskHandsOnWhatItChanged(t *testing.T) {
	run := overspendingChild(t, 600, "vm/vm.go", "bytecode/bytecode.go")
	result, _ := run(context.Background(), PlanTaskRequest{
		Task:          Task{ID: "m6", Prompt: "build the VM"},
		Tools:         []string{"edit_file"},
		MaxTaskTokens: 1000,
	})

	if result.Outcome != TaskCancelled {
		t.Fatalf("outcome = %q, want %q", result.Outcome, TaskCancelled)
	}
	if strings.TrimSpace(result.Output) == "" {
		t.Fatal("a cut-short task handed on nothing; a follow-up starts blind and must re-derive the state from a tree that may not compile")
	}
	for _, file := range []string{"vm/vm.go", "bytecode/bytecode.go"} {
		if !strings.Contains(result.Output, file) {
			t.Errorf("the handoff does not name %q:\n%s", file, result.Output)
		}
	}
	// It must warn that the files are mid-change — a follow-up that assumes they
	// compile will misread a broken tree as the task's considered output.
	if !strings.Contains(result.Output, "need not compile") {
		t.Errorf("the handoff does not warn the files may be mid-change:\n%s", result.Output)
	}
	// The reason the task stopped is unchanged and still structural.
	if !strings.Contains(result.Err, "max_tokens_per_task") {
		t.Errorf("the cancellation reason was lost: %q", result.Err)
	}
}

// THE HANDOFF REACHES A FOLLOW-UP TASK, through the briefing that already exists
// for cut-short work. Asserting on the runner alone would prove a string was
// built, not that anyone receives it.
func TestTheHandoffReachesADependentTask(t *testing.T) {
	briefing := withDependencyBriefing(
		Task{ID: "m6-continue", Prompt: "finish the VM", DependsOn: []string{"m6"}},
		map[string]TaskResult{"m6": {
			ID:      "m6",
			Outcome: TaskCancelled,
			Output:  planTaskHandoff(map[string]bool{"vm/vm.go": true}, ""),
		}},
	)
	if !strings.Contains(briefing, "vm/vm.go") {
		t.Fatalf("a follow-up task is not told which files the cut-short task changed:\n%s", briefing)
	}
	if !strings.Contains(briefing, "INCOMPLETE") {
		t.Errorf("the briefing does not mark the work as unfinished:\n%s", briefing)
	}
}

// PROSE IS KEPT, never replaced: a task that did say something before it was
// stopped has already said something worth having.
func TestTheHandoffIsAppendedToWhatTheTaskSaid(t *testing.T) {
	got := planTaskHandoff(map[string]bool{"a.go": true}, "I finished the parser.")
	if !strings.Contains(got, "I finished the parser.") {
		t.Errorf("the task's own words were dropped:\n%s", got)
	}
	if strings.Index(got, "I finished the parser.") > strings.Index(got, "a.go") {
		t.Errorf("the file list displaced the task's own words:\n%s", got)
	}
	// A task that changed nothing gains no invented handoff.
	if got := planTaskHandoff(nil, "nothing to do here"); got != "nothing to do here" {
		t.Errorf("a task that changed no files gained a handoff: %q", got)
	}
	if got := planTaskHandoff(nil, ""); got != "" {
		t.Errorf("an empty task produced %q", got)
	}
}
