package specialist

import (
	"context"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/streamjson"
)

// THE ROUTER MUST NOT BE TOLD TO START WITH A TOOL CALL.
//
// It ran under the plan-task system prompt — "You have read-only tools: USE THEM.
// Start with a tool call, not prose" — while its own prompt demands JSON and
// nothing else. Contradictory instructions to the one prompt that chooses every
// other task's model. A model handed both obeys one, and which one is a coin toss.
//
// Asserted from the manifest the CHILD receives, not from the constant: a
// constant with the right words proves nothing if nothing installs it.
func TestTheRouterRunsUnderItsOwnSystemPromptNotThePlanTaskOne(t *testing.T) {
	var seen string
	exec := Executor{
		BinaryPath:   "/bin/true",
		NewSessionID: func() (string, error) { return "specialist_00000000000000000000000a", nil },
		Load:         func(LoadOptions) (LoadResult, error) { return LoadResult{}, nil },
		RunChild: func(context.Context, string, []string, func(streamjson.Event)) (ChildRunResult, error) {
			return ChildRunResult{Started: true}, nil
		},
	}
	// The runner builds the manifest; capture it by intercepting the load path.
	planCtx := PlanTaskContext{Executor: exec, Cwd: t.TempDir(), SpecialistName: "explorer"}
	planCtx.Executor.Load = func(LoadOptions) (LoadResult, error) { return LoadResult{}, nil }
	run := NewPlanRunner(planCtx)
	planCtx.Executor.RunChild = func(context.Context, string, []string, func(streamjson.Event)) (ChildRunResult, error) {
		return ChildRunResult{Started: true}, nil
	}

	// Drive the real router entry point so the request it builds is the one tested.
	_, _, _ = routeTaskModels(context.Background(),
		func(_ context.Context, req PlanTaskRequest) (TaskResult, error) {
			seen = req.SystemPrompt
			return TaskResult{Outcome: TaskSucceeded, Output: `{"assignments":[]}`}, nil
		},
		PlanTaskRequest{Tools: []string{"read_file"}}, "m",
		routerTasks(), routerCandidates(), "")
	_ = run

	if strings.TrimSpace(seen) == "" {
		t.Fatal("the router was dispatched with no system prompt of its own")
	}
	for _, forbidden := range []string{"USE THEM", "Start with a tool call"} {
		if strings.Contains(seen, forbidden) {
			t.Errorf("the router inherited the plan-task instruction %q:\n%s", forbidden, seen)
		}
	}
	for _, required := range []string{"do not call any tool", "JSON object and nothing else"} {
		if !strings.Contains(seen, required) {
			t.Errorf("the router prompt is missing %q:\n%s", required, seen)
		}
	}
}

// The override must reach the CHILD, or the constant is decoration.
//
// WrapSystemPrompt folds the manifest's system prompt into the prompt the child
// is launched with, so that text is where the override becomes observable. A test
// asserting manifest.SystemPrompt in isolation would pass while the runner
// ignored req.SystemPrompt entirely — which is the wiring gap this feature has
// produced at every seam.
func TestASystemPromptOverrideReachesTheChildAndTheOrdinaryPathIsUnchanged(t *testing.T) {
	launch := func(request PlanTaskRequest) string {
		t.Helper()
		var prompt string
		exec := Executor{
			BinaryPath:   "/bin/true",
			NewSessionID: func() (string, error) { return "specialist_00000000000000000000000a", nil },
			Load:         func(LoadOptions) (LoadResult, error) { return LoadResult{}, nil },
			RunChild: func(_ context.Context, _ string, args []string, _ func(streamjson.Event)) (ChildRunResult, error) {
				prompt = strings.Join(args, "\n")
				return ChildRunResult{Started: true}, nil
			},
			PromptFileMaxSize: 1 << 20, // keep the prompt in argv so the test can read it
		}
		run := NewPlanRunner(PlanTaskContext{Executor: exec, Cwd: t.TempDir(), SpecialistName: "explorer"})
		if _, err := run(context.Background(), request); err != nil {
			t.Fatalf("run: %v", err)
		}
		return prompt
	}

	overridden := launch(PlanTaskRequest{
		Task:         Task{ID: "t", Prompt: "p"},
		Tools:        []string{"read_file"},
		SystemPrompt: "SENTINEL-ROUTER-PROMPT",
	})
	if !strings.Contains(overridden, "SENTINEL-ROUTER-PROMPT") {
		t.Errorf("the override never reached the child:\n%s", overridden)
	}
	if strings.Contains(overridden, "USE THEM") {
		t.Errorf("the plan-task prompt survived alongside the override:\n%s", overridden)
	}

	// THE COMMON PATH IS UNCHANGED. Every task of every plan takes this branch.
	ordinary := launch(PlanTaskRequest{Task: Task{ID: "t", Prompt: "p"}, Tools: []string{"read_file"}})
	if !strings.Contains(ordinary, "USE THEM") {
		t.Errorf("an ordinary plan task lost its system prompt:\n%s", ordinary)
	}
	if strings.Contains(ordinary, "SENTINEL") {
		t.Error("an override leaked into a request that set none")
	}
}
