package specialist

import (
	"context"
	"strings"
	"testing"
)

// A DEPENDENCY'S FINDINGS MUST REACH ITS DEPENDENT.
//
// depends_on ordered execution and passed nothing. The dependent started from a
// blank context, so it re-read every file its dependency had already read, and a
// synthesising task received conclusions with no trace behind them — able to
// repeat a claim, never to check one. That is precisely how a real plan reported
// that MCP servers inherit an unscrubbed environment while the tracing task had
// already walked the scrubbing path.
func TestATaskIsToldWhatItsDependenciesFound(t *testing.T) {
	var judgePrompt string
	plan := mustPlan(t, []any{
		task("trace", "trace the env path"),
		task("judge", "decide whether credentials leak", "trace"),
	}, okBudget(), readOnlyLimits())

	report := ExecutePlan(context.Background(), plan, []string{"read_file"},
		func(_ context.Context, req PlanTaskRequest) (TaskResult, error) {
			if req.Task.ID == "judge" {
				judgePrompt = req.Task.Prompt
				return TaskResult{Outcome: TaskSucceeded, Output: "no leak"}, nil
			}
			return TaskResult{
				Outcome: TaskSucceeded,
				Output:  "runner.go:163 sets spec.sensitiveEnvKeys; scrubSensitiveEnv runs at 348 and 446",
			}, nil
		}, nil)

	if report.Failed != 0 {
		t.Fatalf("plan failed: %+v", report.Tasks)
	}
	if !strings.Contains(judgePrompt, "scrubSensitiveEnv runs at 348") {
		t.Fatalf("the dependent never received its dependency's evidence:\n%s", judgePrompt)
	}
	if !strings.Contains(judgePrompt, "decide whether credentials leak") {
		t.Errorf("the task's own prompt was lost:\n%s", judgePrompt)
	}
	// The briefing is EVIDENCE, not gospel — a task that treats a prior
	// conclusion as established fact reproduces the error it was given.
	if !strings.Contains(judgePrompt, "not established fact") {
		t.Errorf("the briefing does not tell the reader to verify it:\n%s", judgePrompt)
	}
}

// A task with no dependencies must see EXACTLY the prompt the plan wrote. This is
// the overwhelming majority of tasks and nothing about them may change.
func TestATaskWithNoDependenciesGetsItsPromptVerbatim(t *testing.T) {
	var seen string
	plan := mustPlan(t, []any{task("solo", "do the thing")}, okBudget(), readOnlyLimits())
	ExecutePlan(context.Background(), plan, []string{"read_file"},
		func(_ context.Context, req PlanTaskRequest) (TaskResult, error) {
			seen = req.Task.Prompt
			return TaskResult{Outcome: TaskSucceeded, Output: "ok"}, nil
		}, nil)
	if seen != "do the thing" {
		t.Errorf("an independent task's prompt was rewritten: %q", seen)
	}
}

// BOUNDED, or a deep chain carries the whole plan into its last task's context.
func TestTheBriefingIsBoundedAndSaysWhenItTruncated(t *testing.T) {
	huge := strings.Repeat("x", dependencyBriefingPerTask*3)
	briefed := withDependencyBriefing(
		Task{ID: "b", Prompt: "judge", DependsOn: []string{"a"}},
		map[string]TaskResult{"a": {Outcome: TaskSucceeded, Output: huge}},
	)
	if len(briefed) > dependencyBriefingTotal+len("judge")+800 {
		t.Errorf("the briefing is unbounded: %d bytes", len(briefed))
	}
	if !strings.Contains(briefed, "truncated") {
		t.Error("a truncated briefing did not say so, so its reader takes a part for the whole")
	}
}

// A dependency that did not succeed contributes NOTHING — no empty heading, no
// half-answer presented as a finding.
func TestOnlySucceededDependenciesAreQuoted(t *testing.T) {
	briefed := withDependencyBriefing(
		Task{ID: "c", Prompt: "judge", DependsOn: []string{"failed", "empty", "good"}},
		map[string]TaskResult{
			"failed": {Outcome: TaskFailed, Output: "half an answer before it died"},
			"empty":  {Outcome: TaskSucceeded, Output: "   "},
			"good":   {Outcome: TaskSucceeded, Output: "the real finding"},
		},
	)
	if strings.Contains(briefed, "half an answer") {
		t.Error("a failed dependency's output was presented as a finding")
	}
	if strings.Contains(briefed, `"empty"`) {
		t.Error("an empty result produced a heading with nothing under it")
	}
	if !strings.Contains(briefed, "the real finding") {
		t.Error("the succeeded dependency was dropped")
	}
}

// DETERMINISTIC ORDER, following DependsOn as the plan declared it. A resumed
// plan must not build a different prompt because a map iterated differently.
func TestTheBriefingOrderFollowsTheDeclaredDependencies(t *testing.T) {
	results := map[string]TaskResult{
		"one":   {Outcome: TaskSucceeded, Output: "FIRST"},
		"two":   {Outcome: TaskSucceeded, Output: "SECOND"},
		"three": {Outcome: TaskSucceeded, Output: "THIRD"},
	}
	want := withDependencyBriefing(Task{ID: "x", Prompt: "p", DependsOn: []string{"one", "two", "three"}}, results)
	for attempt := 0; attempt < 20; attempt++ {
		if got := withDependencyBriefing(Task{ID: "x", Prompt: "p", DependsOn: []string{"one", "two", "three"}}, results); got != want {
			t.Fatal("the briefing is not deterministic across runs")
		}
	}
	if strings.Index(want, "FIRST") > strings.Index(want, "SECOND") {
		t.Error("the briefing does not follow the declared dependency order")
	}
}

// THE TOOL MUST SAY THAT DEPENDENTS RECEIVE THEIR DEPENDENCIES' RESULTS.
//
// The briefing only pays off if the planning model knows it exists: a model that
// believes depends_on merely orders execution writes self-contained tasks that
// rediscover everything, which is the behaviour the briefing was built to end.
// Wiring a capability without telling its only caller is how it stays unused.
func TestTheToolDescriptionTellsThePlannerHowToWriteTasks(t *testing.T) {
	schema := (&OrchestrateTool{}).Parameters()
	tasks, ok := schema.Properties["tasks"]
	if !ok {
		t.Fatal("the tasks property is missing from the schema")
	}
	for _, required := range []string{
		"receives its dependencies' results",
		"Split by SUBJECT",
		"Say what the task must return",
		"leave independent work independent",
	} {
		if !strings.Contains(tasks.Description, required) {
			t.Errorf("the tasks description does not mention %q", required)
		}
	}
}
