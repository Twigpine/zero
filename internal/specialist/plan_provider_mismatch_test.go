package specialist

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/Twigpine/zero/internal/tools"
)

// A DISCOVERED LIST THAT LACKS THE SESSION'S OWN MODEL IS THE WRONG PROVIDER'S
// LIST, and assigning from it fails every task it touches.
//
// From a real run: the session was xai/grok-4.5, discovery returned nineteen
// Ollama ids, and every guard downstream passed because the wrong list was
// internally consistent with itself. The served-set check compared Ollama
// against Ollama. routerModel's "the session's model comes first" rule rejected
// grok-4.5 for not being served — so the ROUTER ran on qwen3.5:397b and xAI
// refused it. Four of six tasks died at dispatch on models that did not exist
// for them.
//
// The session's own model is the one id that MUST appear in a correct list: the
// parent is running on it right now. Its absence is proof, not suspicion.
func TestADiscoveredListMissingTheSessionModelIsRefusedAsTheWrongProvider(t *testing.T) {
	// Exactly the shape of the real failure: a session on grok-4.5, a list from
	// somewhere else entirely.
	elsewhere := []DiscoveredModel{
		{ID: "deepseek-v4-flash", ToolCall: true, InputCost: 1},
		{ID: "glm-5.2", ToolCall: true, InputCost: 5},
		{ID: "qwen3.5:397b", ToolCall: true, InputCost: 9},
	}
	// WIRED, or the assertion below tests nothing: with no RunTask, runnerForCall
	// returns nil and routeTaskModels leaves on its first guard, so `dispatched`
	// stayed false however badly the provider check behaved. It has to be
	// possible for the router to run before "the router did not run" means
	// anything.
	var mu sync.Mutex
	dispatched := false
	tool := &OrchestrateTool{
		DiscoverModels: func(context.Context) ([]DiscoveredModel, error) { return elsewhere, nil },
		ModelPrefs:     ModelPreferences{AutoAssign: true},
		ParentTools:    []string{"read_file"},
		RunTask: func(context.Context, PlanTaskRequest) (TaskResult, error) {
			mu.Lock()
			dispatched = true
			mu.Unlock()
			return TaskResult{Outcome: TaskSucceeded, Output: "{}"}, nil
		},
	}
	args := map[string]any{"tasks": []any{
		map[string]any{"id": "a", "prompt": "list the files"},
		map[string]any{"id": "b", "prompt": "trace the call path and explain it"},
		map[string]any{"id": "c", "prompt": "decide whether the guarantee holds"},
	}}

	notes, err := tool.autoAssignModels(context.Background(), args, tools.RunOptions{Model: "grok-4.5"})
	if err != nil {
		t.Fatalf("a configured default must degrade, not fail the plan: %v", err)
	}
	mu.Lock()
	ran := dispatched
	mu.Unlock()
	if ran {
		t.Error("the router was called against a provider that cannot serve it")
	}

	joined := strings.Join(notes, " ")
	if !strings.Contains(joined, "grok-4.5") || !strings.Contains(joined, "different provider") {
		t.Errorf("the refusal must name the session model and the reason, got %q", joined)
	}

	// THE TASKS MUST BE UNTOUCHED. Degrading means every task keeps the session's
	// model — precisely what it did before auto-assignment existed. A partially
	// assigned plan would be the same failure with fewer casualties.
	for _, entry := range args["tasks"].([]any) {
		fields := entry.(map[string]any)
		if model := strings.TrimSpace(planString(fields, "model")); model != "" {
			t.Errorf("task %v was assigned %q from the wrong provider's list", fields["id"], model)
		}
	}
}

// The tripwire must not fire on the honest case, or it disables the feature.
func TestAListContainingTheSessionModelStillAssigns(t *testing.T) {
	here := []DiscoveredModel{
		{ID: "grok-code-fast", ToolCall: true, InputCost: 1},
		{ID: "grok-4.5", ToolCall: true, InputCost: 5},
		{ID: "grok-4.5-heavy", ToolCall: true, InputCost: 9},
	}
	tool := &OrchestrateTool{
		DiscoverModels: func(context.Context) ([]DiscoveredModel, error) { return here, nil },
		ModelPrefs:     ModelPreferences{AutoAssign: true},
	}
	args := map[string]any{"tasks": []any{
		map[string]any{"id": "a", "prompt": "search for every caller"},
		map[string]any{"id": "b", "prompt": "audit the result and judge whether it is correct"},
	}}
	notes, err := tool.autoAssignModels(context.Background(), args, tools.RunOptions{Model: "grok-4.5"})
	if err != nil {
		t.Fatalf("assignment on a matching provider must work: %v", err)
	}
	if strings.Contains(strings.Join(notes, " "), "different provider") {
		t.Fatalf("the tripwire fired on a list that does contain the session model: %v", notes)
	}
	assigned := 0
	for _, entry := range args["tasks"].([]any) {
		if strings.TrimSpace(planString(entry.(map[string]any), "model")) != "" {
			assigned++
		}
	}
	if assigned == 0 {
		t.Error("nothing was assigned from a provider that serves the session model")
	}
}

// An EMPTY discovered set is not evidence of disagreement — discovery simply told
// us nothing. That is the pre-existing fail-open and must stay open, or a provider
// with no models endpoint starts refusing plans.
func TestAnEmptyDiscoveredListDoesNotTripTheProviderMismatchGuard(t *testing.T) {
	tool := &OrchestrateTool{
		DiscoverModels: func(context.Context) ([]DiscoveredModel, error) { return nil, nil },
		ModelPrefs:     ModelPreferences{AutoAssign: true},
	}
	args := map[string]any{"tasks": []any{map[string]any{"id": "a", "prompt": "list the files"}}}
	notes, err := tool.autoAssignModels(context.Background(), args, tools.RunOptions{Model: "grok-4.5"})
	if err != nil {
		t.Fatalf("an empty list must degrade quietly: %v", err)
	}
	if strings.Contains(strings.Join(notes, " "), "different provider") {
		t.Errorf("an empty list was misread as the wrong provider: %v", notes)
	}
}

// AN INVALID PLAN MUST NOT COST A PROVIDER CALL. Auto-assignment lists the
// provider's models and, with routing on, spends a call on a frontier model. It
// was doing that before the plan was validated, so a plan rejected for a
// duplicate id or a cycle paid for routing and got nothing — and a model that
// proposes the same oversized plan twice paid twice.
func TestAnInvalidPlanIsRejectedBeforeAnyModelDiscoveryHappens(t *testing.T) {
	discovered := false
	tool := &OrchestrateTool{
		DiscoverModels: func(context.Context) ([]DiscoveredModel, error) {
			discovered = true
			return []DiscoveredModel{{ID: "m", ToolCall: true}}, nil
		},
		ModelPrefs:    ModelPreferences{AutoAssign: true},
		RunTask:       func(context.Context, PlanTaskRequest) (TaskResult, error) { return TaskResult{}, nil },
		PostureActive: func() bool { return true },
	}
	// The same id twice: ParsePlan rejects it, and nothing about assigning a
	// model could ever make it valid.
	result := tool.RunWithOptions(context.Background(), map[string]any{
		"name": "dupes",
		"tasks": []any{
			map[string]any{"id": "a", "prompt": "one"},
			map[string]any{"id": "a", "prompt": "two"},
		},
	}, tools.RunOptions{Model: "parent-model"})

	if result.Status != tools.StatusError {
		t.Fatalf("a duplicate task id was admitted: %+v", result)
	}
	// GUARD THE GUARD. The posture gate refuses orchestrate before any of this
	// runs, and a test that trips it passes without exercising the ordering at
	// all — which is exactly what the first version of this test did.
	if strings.Contains(result.Output, "zeromaxing posture") {
		t.Fatalf("the posture gate rejected the plan, so nothing here was tested: %q", result.Output)
	}
	if !strings.Contains(result.Output, "more than once") {
		t.Fatalf("rejected for the wrong reason: %q", result.Output)
	}
	if discovered {
		t.Error("the provider was probed for models on a plan that was never going to run")
	}
}
