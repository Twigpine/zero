package specialist

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func routerCandidates() []DiscoveredModel {
	return []DiscoveredModel{
		{ID: "deepseek-v4-flash", InputCost: 0.1},
		{ID: "glm-5.2", InputCost: 1},
		{ID: "qwen3.5:397b", InputCost: 9, Reasoning: true},
	}
}

func routerTasks() []routableTask {
	return []routableTask{
		{ID: "l-files", Prompt: "Listing every .go file under internal/specialist"},
		{ID: "j-race", Prompt: "Deciding whether a task can begin before its dependencies resolve"},
		{ID: "j-merge", Prompt: "Deciding whether a project config can raise a user limit"},
	}
}

// A router that ANSWERS is believed, and its choices reach the tasks.
func TestTheRouterDecidesWhichModelRunsEachTask(t *testing.T) {
	run := func(_ context.Context, req PlanTaskRequest) (TaskResult, error) {
		// It must be asked on the router model, and shown the real task text.
		if req.Task.Model != "qwen3.5:397b" {
			t.Errorf("router ran on %q, want the strongest model", req.Task.Model)
		}
		for _, want := range []string{"l-files", "j-race", "Listing every .go file"} {
			if !strings.Contains(req.Task.Prompt, want) {
				t.Errorf("the router was not shown %q", want)
			}
		}
		return TaskResult{Outcome: TaskSucceeded, Output: `Here you go:
{"assignments":[
 {"id":"l-files","model":"deepseek-v4-flash","reason":"mechanical listing"},
 {"id":"j-race","model":"qwen3.5:397b","reason":"needs judgement"},
 {"id":"j-merge","model":"qwen3.5:397b","reason":"needs judgement"}
]}`}, nil
	}
	got, _, err := routeTaskModels(context.Background(), run, PlanTaskRequest{}, "qwen3.5:397b",
		routerTasks(), routerCandidates(), "")
	if err != nil {
		t.Fatalf("router: %v", err)
	}
	if got["l-files"] != "deepseek-v4-flash" {
		t.Errorf("a mechanical listing went to %q", got["l-files"])
	}
	if got["j-race"] != "qwen3.5:397b" || got["j-merge"] != "qwen3.5:397b" {
		t.Errorf("judgement tasks went to %v", got)
	}
}

// EVERY FAILURE FALLS BACK. A router is an optimisation; it must never be able
// to stop a plan, and the worst outcome is the classifier routing we had.
func TestEveryRouterFailureFallsBackInsteadOfBreaking(t *testing.T) {
	tasks, candidates := routerTasks(), routerCandidates()

	for name, run := range map[string]PlanRunner{
		"errors": func(context.Context, PlanTaskRequest) (TaskResult, error) {
			return TaskResult{}, fmt.Errorf("provider exploded")
		},
		"fails": func(context.Context, PlanTaskRequest) (TaskResult, error) {
			return TaskResult{Outcome: TaskFailed, Err: "exit 4"}, nil
		},
		"prose": func(context.Context, PlanTaskRequest) (TaskResult, error) {
			return TaskResult{Outcome: TaskSucceeded, Output: "I think flash is fine for all of them."}, nil
		},
		"broken json": func(context.Context, PlanTaskRequest) (TaskResult, error) {
			return TaskResult{Outcome: TaskSucceeded, Output: `{"assignments":[{"id":`}, nil
		},
	} {
		got, _, err := routeTaskModels(context.Background(), run, PlanTaskRequest{}, "qwen3.5:397b", tasks, candidates, "")
		if err == nil {
			t.Errorf("%s: expected a reported error so the caller can say so", name)
		}
		if len(got) != 0 {
			t.Errorf("%s: a failed router must route nothing, got %v", name, got)
		}
	}

	// A HALLUCINATED model is dropped, and the rest of the answer still counts —
	// discarding good decisions to punish a bad one helps nobody.
	partial := func(context.Context, PlanTaskRequest) (TaskResult, error) {
		return TaskResult{Outcome: TaskSucceeded, Output: `{"assignments":[
		 {"id":"l-files","model":"deepseek-v4-flash"},
		 {"id":"j-race","model":"gpt-9-imaginary"},
		 {"id":"not-a-task","model":"glm-5.2"}]}`}, nil
	}
	got, _, err := routeTaskModels(context.Background(), partial, PlanTaskRequest{}, "qwen3.5:397b", tasks, candidates, "")
	if err != nil {
		t.Fatalf("a partially valid answer is usable: %v", err)
	}
	if got["l-files"] != "deepseek-v4-flash" {
		t.Errorf("the valid assignment was discarded: %v", got)
	}
	if _, ok := got["j-race"]; ok {
		t.Error("a model the provider does not serve was accepted")
	}
	if _, ok := got["not-a-task"]; ok {
		t.Error("an id that is not in this plan was accepted")
	}
}

// TOO SMALL TO BE WORTH A CALL. Two tasks cannot differ enough to justify a
// frontier-model round trip deciding between them.
func TestRoutingIsSkippedWhenItCannotPayForItself(t *testing.T) {
	called := false
	run := func(context.Context, PlanTaskRequest) (TaskResult, error) {
		called = true
		return TaskResult{Outcome: TaskSucceeded, Output: "{}"}, nil
	}
	small := routerTasks()[:2]
	if got, _, err := routeTaskModels(context.Background(), run, PlanTaskRequest{}, "qwen3.5:397b",
		small, routerCandidates(), ""); err != nil || len(got) != 0 {
		t.Errorf("a two-task plan must skip routing, got %v %v", got, err)
	}
	if called {
		t.Error("the router was called for a plan too small to benefit")
	}
	// No router model, no candidates, no runner: all skip silently.
	for name, args := range map[string][3]any{
		"no model":      {"", routerCandidates(), run},
		"no candidates": {"qwen3.5:397b", []DiscoveredModel(nil), run},
	} {
		called = false
		model := args[0].(string)
		cands, _ := args[1].([]DiscoveredModel)
		if _, _, err := routeTaskModels(context.Background(), run, PlanTaskRequest{}, model,
			routerTasks(), cands, ""); err != nil {
			t.Errorf("%s: expected a silent skip, got %v", name, err)
		}
		if called {
			t.Errorf("%s: the router was called anyway", name)
		}
	}
}

// A task that NAMED its own model is not shown to the router at all — an
// explicit choice is not up for reconsideration.
func TestARoutersOpinionIsNotSoughtOnAnExplicitModel(t *testing.T) {
	raw := []any{
		map[string]any{"id": "a", "prompt": "listing files"},
		map[string]any{"id": "b", "prompt": "deciding something", "model": "glm-5.2"},
	}
	got := routableTasks(raw)
	if len(got) != 1 || got[0].ID != "a" {
		t.Errorf("only the unassigned task is routable, got %+v", got)
	}
}

// THE DECISION MUST REACH THE TASK. Every test above exercises the router in
// isolation, which passes perfectly while the assignment path ignores what it
// returned — the producer tested, the wiring not.
func TestARoutedChoiceActuallyLandsOnTheTask(t *testing.T) {
	tasks := []any{
		map[string]any{"id": "l-files", "prompt": "listing every go file"},
		map[string]any{"id": "j-race", "prompt": "deciding whether a race exists"},
	}
	candidates := routerCandidates()
	routed := map[string]string{"l-files": "deepseek-v4-flash", "j-race": "qwen3.5:397b"}

	out, notes := assignModelsToTaskArgs(tasks, buildModelTiers(candidates, ModelPreferences{}),
		ModelPreferences{}, servedModels(candidates), routed)

	got := map[string]string{}
	for _, raw := range out {
		fields := raw.(map[string]any)
		got[planString(fields, "id")] = planString(fields, "model")
	}
	if got["j-race"] != "qwen3.5:397b" {
		t.Errorf("the router chose qwen for the judgement; the task got %q", got["j-race"])
	}
	if got["l-files"] != "deepseek-v4-flash" {
		t.Errorf("the router chose flash for the listing; the task got %q", got["l-files"])
	}
	if joined := strings.Join(notes, " | "); !strings.Contains(joined, "routed") {
		t.Errorf("a routed choice must be reported as routed, not as a role guess: %s", joined)
	}

	// A PIN STILL OUTRANKS THE ROUTER. A pin is the user's own instruction, and
	// another model does not get to overrule it.
	prefs := ModelPreferences{Scan: "glm-5.2"}
	out, _ = assignModelsToTaskArgs(tasks, buildModelTiers(candidates, prefs), prefs,
		servedModels(candidates), routed)
	for _, raw := range out {
		fields := raw.(map[string]any)
		if planString(fields, "id") == "l-files" && planString(fields, "model") != "glm-5.2" {
			t.Errorf("the router overruled a user pin: %q", planString(fields, "model"))
		}
	}
}

// THE MODEL THE USER PICKED DOES THE ROUTING.
//
// Choosing a model with /model is a statement about which one you trust to
// think. Routing on "whatever discovery priced highest" ignores that and can
// hand the decision to a model the user deliberately did not choose — on one
// real account the priciest thing was a build preview, on another a video model.
func TestTheSessionsOwnModelRoutesByDefault(t *testing.T) {
	tiers := buildModelTiers(routerCandidates(), ModelPreferences{})
	served := servedModels(routerCandidates())

	// No configured router: the session's model decides.
	if got := routerModel(ModelPreferences{}, tiers, served, "glm-5.2"); got != "glm-5.2" {
		t.Errorf("router = %q, want the model the user selected", got)
	}
	// An explicit router still wins — it is a more specific instruction.
	if got := routerModel(ModelPreferences{Router: "qwen3.5:397b"}, tiers, served, "glm-5.2"); got != "qwen3.5:397b" {
		t.Errorf("router = %q, want the configured one", got)
	}
	// A session model this provider does not serve falls through rather than
	// failing the call — the same staleness pins already survive.
	if got := routerModel(ModelPreferences{}, tiers, served, "grok-4.3"); got != tiers.strong {
		t.Errorf("router = %q, want the strongest tier as the fallback", got)
	}
	// And with nothing at all to go on, the strongest tier still routes.
	if got := routerModel(ModelPreferences{}, tiers, served, ""); got != tiers.strong {
		t.Errorf("router = %q, want the strongest tier", got)
	}
}

// ONE TASK IN, ONE TASK OUT. Counted, not keyed.
//
// The routed branch once appended its own clone on top of the one already
// emitted, so every routed task came out twice and ParsePlan rejected the plan:
// "task id appears more than once". Every plan of three or more tasks failed,
// because routing does not run below that — and the test that should have caught
// it collected results into a map keyed by id, where a duplicate overwrites its
// twin and vanishes.
func TestAssignmentEmitsExactlyOneEntryPerTask(t *testing.T) {
	candidates := routerCandidates()
	served := servedModels(candidates)
	tiers := buildModelTiers(candidates, ModelPreferences{})

	for name, routed := range map[string]map[string]string{
		"all routed":  {"a": "glm-5.2", "b": "glm-5.2", "c": "qwen3.5:397b"},
		"some routed": {"b": "qwen3.5:397b"},
		"none routed": {},
	} {
		tasks := []any{
			map[string]any{"id": "a", "prompt": "listing every go file"},
			map[string]any{"id": "b", "prompt": "deciding whether a race exists"},
			map[string]any{"id": "c", "prompt": "fixing the comment"},
		}
		out, _ := assignModelsToTaskArgs(tasks, tiers, ModelPreferences{}, served, routed)
		if len(out) != len(tasks) {
			t.Errorf("%s: %d tasks in, %d out — a plan with duplicates is rejected outright", name, len(tasks), len(out))
		}
		seen := map[string]int{}
		for _, raw := range out {
			seen[planString(raw.(map[string]any), "id")]++
		}
		for id, count := range seen {
			if count != 1 {
				t.Errorf("%s: task %q emitted %d times", name, id, count)
			}
		}
	}
}

// THE GUIDANCE MUST DESCRIBE A SPECTRUM, NOT A PAIR.
//
// This once told the router "mechanical lookups belong on the cheapest model,
// work that judges belongs on the strongest" — a binary. A router given nineteen
// models obeyed it precisely and used two of them, always the ends, while every
// mid-priced model on the account went untouched. The middle has to be named, or
// it may as well not be on the list.
func TestTheRouterIsToldToUseTheWholeRangeNotJustTheEnds(t *testing.T) {
	prompt := routerPrompt(routerTasks(), routerCandidates(), "")
	for _, want := range []string{"MIDDLE", "Most tasks belong here", "Do not answer with only the cheapest"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the router prompt no longer steers toward the middle (missing %q)", want)
		}
	}
	// And every candidate is offered, or the middle cannot be chosen even when
	// the guidance asks for it.
	for _, model := range routerCandidates() {
		if !strings.Contains(prompt, model.ID) {
			t.Errorf("candidate %q was not offered to the router", model.ID)
		}
	}
}

// The router is shown each model's SIZE when the id names one, so it can match a
// light model to a scan and a heavy one to a judgement rather than infer
// capability from list position alone. A cloud id that names no size gets no
// invented label.
func TestRouterPromptShowsModelSizeWhenNamed(t *testing.T) {
	candidates := []DiscoveredModel{
		{ID: "gpt-oss:20b"},
		{ID: "qwen3.5:397b", Reasoning: true},
		{ID: "gpt-4o"}, // cloud: no size in the id
	}
	prompt := routerPrompt(routerTasks(), candidates, "")
	if !strings.Contains(prompt, "gpt-oss:20b [20B]") {
		t.Errorf("the router prompt did not label the 20B model:\n%s", prompt)
	}
	if !strings.Contains(prompt, "qwen3.5:397b [397B]") {
		t.Errorf("the router prompt did not label the 397B model:\n%s", prompt)
	}
	if strings.Contains(prompt, "gpt-4o [") {
		t.Errorf("the router prompt invented a size for a cloud model:\n%s", prompt)
	}
	if !strings.Contains(prompt, "least to most capable") {
		t.Errorf("the candidate header no longer states capability order:\n%s", prompt)
	}
}

// The operator's own words must reach the router, and must ADD to the built-in
// guidance rather than replace it.
//
// This exists because the code cannot know the account. Discovery orders models
// by price, and price is not capability: one provider's dearest model was a
// build preview that failed every task, another reports no prices at all. The
// person running the machine knows which model reasons well. Without this they
// can only say so by naming models in every prompt — which is the thing
// auto-assignment exists to remove.
func TestOperatorGuidanceIsAddedToTheRouterPromptNotSubstitutedForIt(t *testing.T) {
	const advice = "kimi-k2.6 is the best reasoner here; qwen3.5:397b is slow, save it for judgements."
	prompt := routerPrompt(routerTasks(), routerCandidates(), advice)

	if !strings.Contains(prompt, advice) {
		t.Fatalf("the operator's guidance never reached the router:\n%s", prompt)
	}
	// ADDED, not substituted. The bands, the JSON contract and the candidate list
	// are what make the reply parseable; guidance that replaced them would produce
	// output nothing can decode, and the failure would read as a stupid model
	// rather than a broken prompt.
	for _, required := range []string{
		"Most tasks belong here",
		"Do not answer with only the cheapest",
		`{"assignments":[{"id":"<task id>","model":"<model id exactly as listed>"`,
		"Every task must appear exactly once",
	} {
		if !strings.Contains(prompt, required) {
			t.Errorf("guidance displaced the built-in prompt; missing %q", required)
		}
	}
	for _, model := range routerCandidates() {
		if !strings.Contains(prompt, model.ID) {
			t.Errorf("candidate %q is no longer offered", model.ID)
		}
	}
	// It must be placed AFTER the general rule, so it reads as the more specific
	// instruction. Before it, a model treats it as background and the general rule
	// is what it ends up obeying.
	if strings.Index(prompt, advice) < strings.Index(prompt, "Do not answer with only the cheapest") {
		t.Error("the operator's guidance is placed before the general rule it is meant to outrank")
	}

	// Empty guidance changes nothing at all: nobody who has not configured this
	// should see a difference in what their router is asked.
	if routerPrompt(routerTasks(), routerCandidates(), "   ") != routerPrompt(routerTasks(), routerCandidates(), "") {
		t.Error("blank guidance altered the prompt")
	}
}

// A ROUTER MAY NOT REACH PAST ITS OWN LIST, and the user's exclusions are the
// case that proves why.
//
// The answer was once validated against every id DISCOVERY returned rather than
// against the candidates actually offered. Excluded models, video models and
// non-tool-callers are all still served, so a router naming one was accepted and
// dispatched — silently overriding the planModels.exclude the user wrote to
// avoid exactly that model. Reproduced with three tasks routed onto an excluded
// id before this check existed.
func TestTheRouterCannotAssignAModelThatWasNotOfferedToIt(t *testing.T) {
	all := []DiscoveredModel{
		{ID: "grok-code-fast", ToolCall: true, InputCost: 1},
		{ID: "grok-build-0.1", ToolCall: true, InputCost: 4}, // excluded by the user
		{ID: "grok-4.5", ToolCall: true, InputCost: 9},
	}
	prefs := ModelPreferences{Exclude: []string{"grok-build-0.1"}}
	candidates := eligibleForRouting(all, prefs)
	for _, model := range candidates {
		if model.ID == "grok-build-0.1" {
			t.Fatal("the fixture is wrong: the excluded model is still a candidate")
		}
	}

	run := func(context.Context, PlanTaskRequest) (TaskResult, error) {
		return TaskResult{Outcome: TaskSucceeded, Output: `{"assignments":[
			{"id":"a","model":"grok-build-0.1","reason":"x"},
			{"id":"b","model":"grok-4.5","reason":"legitimate"},
			{"id":"c","model":"grok-not-a-real-model","reason":"invented"}]}`}, nil
	}
	tasks := []routableTask{{ID: "a", Prompt: "p"}, {ID: "b", Prompt: "p"}, {ID: "c", Prompt: "p"}}

	got, _, err := routeTaskModels(context.Background(), run, PlanTaskRequest{}, "grok-4.5", tasks, candidates, "")
	if err != nil {
		t.Fatalf("router: %v", err)
	}
	if model, ok := got["a"]; ok {
		t.Errorf("the user's exclusion was overridden by the router: task a → %q", model)
	}
	if model, ok := got["c"]; ok {
		t.Errorf("an invented model was accepted: task c → %q", model)
	}
	// A PARTIAL ANSWER IS STILL USABLE: the one valid assignment must survive, or
	// rejecting a bad entry would throw away good decisions to punish it.
	if got["b"] != "grok-4.5" {
		t.Errorf("a legitimate assignment was discarded with the bad ones: %v", got)
	}
}
