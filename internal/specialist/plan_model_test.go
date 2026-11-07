package specialist

import (
	"context"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/execprofile"
	"github.com/Twigpine/zero/internal/modelregistry"
)

// THE RESOLVER MUST MATCH THE CHILD'S. The child resolves its model through
// ResolveWithFallback; validating with ResolveID would refuse inputs the child
// accepts, and would let a deprecated id be saved and displayed while the child
// silently ran its replacement.
func TestTaskModelResolvesTheSameWayTheChildWill(t *testing.T) {
	registry, err := modelregistry.DefaultRegistry()
	if err != nil {
		t.Fatalf("registry: %v", err)
	}

	// An empty request inherits the parent, and is not an error.
	if got, err := resolveTaskModel("  "); err != nil || got != "" {
		t.Fatalf("empty model = %q, %v; want \"\", nil", got, err)
	}

	// A canonical id round-trips to itself.
	if got, err := resolveTaskModel("claude-haiku-4.5"); err != nil {
		t.Fatalf("canonical id rejected: %v", err)
	} else if want, _ := registry.ResolveID("claude-haiku-4.5"); got != want {
		t.Errorf("canonical id = %q, want %q", got, want)
	}

	// An alias resolves to the canonical id, not back to the alias — the id is
	// what reaches argv.
	got, err := resolveTaskModel("haiku-4.5")
	if err != nil {
		t.Fatalf("alias rejected: %v", err)
	}
	if got == "haiku-4.5" {
		t.Errorf("the alias was echoed back instead of resolved: %q", got)
	}
	if entry, _, ok := registry.ResolveWithFallback("haiku-4.5"); !ok || got != entry.ID {
		t.Errorf("alias resolved to %q, want the registry's id", got)
	}

	// A DEPRECATED id must come back as its replacement, so the plan stores what
	// the child will actually run.
	deprecated, _, ok := registry.ResolveWithFallback("claude-haiku-3.5")
	if !ok {
		t.Skip("no deprecated fixture in the registry")
	}
	resolved, err := resolveTaskModel("claude-haiku-3.5")
	if err != nil {
		t.Fatalf("deprecated id rejected outright: %v", err)
	}
	if resolved != deprecated.ID {
		t.Errorf("deprecated id resolved to %q, want the child's %q", resolved, deprecated.ID)
	}
}

// AN UNCURATED MODEL PASSES THROUGH, deliberately. The registry is a curated
// subset for aliases, pricing and display — not an inventory of what a provider
// serves. An xAI account offers Grok models the registry has never heard of and
// the picker lists in full; refusing them made per-task models unusable there.
//
// The check moves rather than disappears: the child resolves the name through
// its own provider config and fails with "zero model X belongs to ..." when the
// provider cannot serve it.
func TestAnUncuratedTaskModelPassesThroughForTheProviderToJudge(t *testing.T) {
	got, err := resolveTaskModel("grok-4.20-reasoning")
	if err != nil {
		t.Fatalf("an uncurated model must not be refused at admission: %v", err)
	}
	if got != "grok-4.20-reasoning" {
		t.Errorf("model = %q, want it carried through unchanged", got)
	}
	// A KNOWN model is still canonicalised, so aliases and deprecations keep
	// resolving to what the child will actually run.
	if got, _ := resolveTaskModel("haiku-4.5"); got == "haiku-4.5" {
		t.Errorf("a known alias must still canonicalise, got %q", got)
	}
}

// THE ROUND TRIP, which is where a per-task model gets silently lost. Args() is
// what /plans save writes, /plans show renders, and resume re-admits. A model on
// the struct but absent from that map means the plan reruns on the parent's
// model with nothing anywhere reporting the change.
func TestATasksModelSurvivesTheArgsRoundTrip(t *testing.T) {
	plan := mustPlan(t, []any{
		map[string]any{"id": "scan", "prompt": "look", "model": "haiku-4.5"},
		map[string]any{"id": "judge", "prompt": "assess", "depends_on": []any{"scan"}},
	}, map[string]any{"max_workers": float64(1)}, readOnlyLimits())

	first := planTaskByID(t, plan, "scan")
	if first.Model == "" {
		t.Fatal("the task's model was dropped at parse")
	}
	if first.Model == "haiku-4.5" {
		t.Errorf("the alias was stored instead of the canonical id: %q", first.Model)
	}
	if other := planTaskByID(t, plan, "judge"); other.Model != "" {
		t.Errorf("a task that named no model must inherit, got %q", other.Model)
	}

	// Re-admit exactly what would have been saved.
	again, err := ParsePlan(plan.Args(), readOnlyLimits())
	if err != nil {
		t.Fatalf("the saved plan does not re-admit: %v", err)
	}
	if got := planTaskByID(t, again, "scan").Model; got != first.Model {
		t.Errorf("model after a save/reload round trip = %q, want %q", got, first.Model)
	}
	if got := planTaskByID(t, again, "judge").Model; got != "" {
		t.Errorf("an inheriting task gained a model across the round trip: %q", got)
	}
}

// A plan naming an uncurated model is ADMITTED and carries it to the task, so a
// provider's own models are usable without waiting for them to be curated.
func TestAPlanMayNameAModelTheRegistryDoesNotKnow(t *testing.T) {
	plan, err := ParsePlan(map[string]any{
		"name": "p",
		"tasks": []any{
			map[string]any{"id": "a", "prompt": "one", "model": "grok-4.20-reasoning"},
		},
		"budget": map[string]any{"max_workers": float64(1)},
	}, readOnlyLimits())
	if err != nil {
		t.Fatalf("a plan naming a provider's own model must be admitted: %v", err)
	}
	if got := planTaskByID(t, plan, "a").Model; got != "grok-4.20-reasoning" {
		t.Errorf("task model = %q, want it carried through", got)
	}
	// And it survives save/reload, or the plan would run on something else the
	// second time.
	again, err := ParsePlan(plan.Args(), readOnlyLimits())
	if err != nil {
		t.Fatalf("the saved plan does not re-admit: %v", err)
	}
	if got := planTaskByID(t, again, "a").Model; got != "grok-4.20-reasoning" {
		t.Errorf("model after the round trip = %q", got)
	}
}

func planTaskByID(t *testing.T, plan Plan, id string) Task {
	t.Helper()
	for _, task := range plan.Tasks() {
		if task.ID == id {
			return task
		}
	}
	t.Fatalf("task %q not in plan", id)
	return Task{}
}

// ASSERTED ON ARGV, not on the manifest struct. appendModelArgs is what turns a
// manifest into the child's command line, and it has its own rule about when
// effort is inherited — so a test that checked Metadata.Model would pass while
// the flag never reached the process.
func TestATasksModelAndEffortReachTheChildsCommandLine(t *testing.T) {
	const parentModel, parentEffort = "gpt-4.1", "medium"

	// A task that names a model: both flags present, the effort carried across.
	named := planTaskManifest("explorer", "claude-haiku-4.5",
		planTaskReasoningEffort("claude-haiku-4.5", parentEffort, "high"), []string{"read_file"})
	argv := appendModelArgs(nil, named, parentModel, parentEffort)
	assertFlag(t, argv, "--model", "claude-haiku-4.5")
	assertFlag(t, argv, "--reasoning-effort", parentEffort)

	// A task that names none inherits the parent's model, and the untouched path
	// is exactly what it was before this feature existed.
	inherit := planTaskManifest("explorer", "", planTaskReasoningEffort("", parentEffort, "high"), []string{"read_file"})
	argv = appendModelArgs(nil, inherit, parentModel, parentEffort)
	assertFlag(t, argv, "--model", parentModel)
	assertFlag(t, argv, "--reasoning-effort", parentEffort)
}

// THE CASE THE OLD RULE LOST. appendModelArgs inherits parent effort only when
// no model is named, so a task naming a model would have run at the provider's
// default — thinking LESS than its siblings, under the posture whose entire
// point is thinking more.
func TestNamingAModelDoesNotSilentlyDropTheRaisedEffort(t *testing.T) {
	named := planTaskManifest("explorer", "claude-haiku-4.5",
		planTaskReasoningEffort("claude-haiku-4.5", "high", "high"), []string{"read_file"})
	argv := appendModelArgs(nil, named, "gpt-4.1", "high")
	assertFlag(t, argv, "--reasoning-effort", "high")

	// And when the parent has no effort to give — which is exactly when the
	// posture could not raise it — the posture's own effort is used rather than
	// nothing.
	if got := planTaskReasoningEffort("claude-haiku-4.5", "", string(execprofile.Zeromaxing.ReasoningEffort)); got != string(execprofile.Zeromaxing.ReasoningEffort) {
		t.Errorf("effort with no parent value = %q, want the posture's %q",
			got, execprofile.Zeromaxing.ReasoningEffort)
	}
	// A task naming NO model must still contribute nothing, or the posture-off
	// path stops being byte-identical.
	if got := planTaskReasoningEffort("", "", "high"); got != "" {
		t.Errorf("a task with no model must not gain an effort, got %q", got)
	}
}

func assertFlag(t *testing.T, argv []string, flag, want string) {
	t.Helper()
	for i, arg := range argv {
		if arg == flag {
			if i+1 >= len(argv) {
				t.Fatalf("%s has no value in %v", flag, argv)
			}
			if argv[i+1] != want {
				t.Errorf("%s = %q, want %q (argv %v)", flag, argv[i+1], want, argv)
			}
			return
		}
	}
	t.Errorf("%s missing from argv %v", flag, argv)
}

// END TO END. Two tasks naming different models, run through the real executor
// path, each child asked for its own — the wiring, not the pieces.
func TestAPlanRunsItsTasksOnTheModelsTheyNamed(t *testing.T) {
	plan := mustPlan(t, []any{
		map[string]any{"id": "scan", "prompt": "look", "model": "haiku-4.5"},
		map[string]any{"id": "judge", "prompt": "assess", "model": "claude-sonnet-4.5"},
		map[string]any{"id": "plain", "prompt": "inherit"},
	}, map[string]any{"max_workers": float64(1)}, readOnlyLimits())

	seen := map[string]string{}
	report := ExecutePlan(context.Background(), plan, []string{"read_file"},
		func(_ context.Context, req PlanTaskRequest) (TaskResult, error) {
			manifest := planTaskManifest("explorer", req.Task.Model,
				planTaskReasoningEffort(req.Task.Model, req.ParentReasoningEffort, "high"), req.Tools)
			argv := appendModelArgs(nil, manifest, "gpt-4.1", "high")
			for i, arg := range argv {
				if arg == "--model" && i+1 < len(argv) {
					seen[req.Task.ID] = argv[i+1]
				}
			}
			return TaskResult{Outcome: TaskSucceeded}, nil
		}, nil)

	if report.Succeeded != 3 {
		t.Fatalf("report = %+v", report)
	}
	if seen["scan"] == seen["judge"] {
		t.Errorf("two tasks naming different models ran on the same one: %v", seen)
	}
	if seen["scan"] != "claude-haiku-4.5" {
		t.Errorf("scan ran on %q, want the resolved haiku id", seen["scan"])
	}
	if seen["judge"] != "claude-sonnet-4.5" {
		t.Errorf("judge ran on %q", seen["judge"])
	}
	if seen["plain"] != "gpt-4.1" {
		t.Errorf("a task naming no model must inherit the parent's, got %q", seen["plain"])
	}
}

// OBSERVABLE, or it cannot be checked. A per-task model changes which model does
// the work and what it costs; the first real test run showed nothing had changed
// because nothing anywhere reported which model each task used.
func TestTheReportSaysWhichModelEachTaskRanOn(t *testing.T) {
	report := PlanReport{
		Status: PlanCompleted,
		Tasks: []TaskResult{
			{ID: "scan", Outcome: TaskSucceeded, Model: "claude-haiku-4.5"},
			{ID: "plain", Outcome: TaskSucceeded},
		},
	}
	summary := report.Summary()
	if !strings.Contains(summary, "scan") || !strings.Contains(summary, "on claude-haiku-4.5") {
		t.Errorf("the report must say what a task ran on:\n%s", summary)
	}
	// A task that inherited gets no model line — otherwise every task carries
	// "on <the model you already use>" and the one that differs is buried.
	for _, line := range strings.Split(summary, "\n") {
		if strings.Contains(line, "plain") && strings.Contains(line, " on ") {
			t.Errorf("an inheriting task must not claim a model: %q", line)
		}
	}
}

// And the runner carries it, so the report is fed by what actually ran rather
// than by what the plan asked for somewhere else.
func TestTheRunnerReportsTheModelTheTaskRanOn(t *testing.T) {
	run := NewPlanRunner(PlanTaskContext{
		Executor: progressExecutor(t), Cwd: t.TempDir(), SpecialistName: "explorer",
	})
	result, err := run(context.Background(), PlanTaskRequest{
		Task:  Task{ID: "a", Prompt: "look", Model: "claude-haiku-4.5"},
		Tools: []string{"read_file"},
	})
	if err != nil {
		t.Fatalf("runner: %v", err)
	}
	if result.Model != "claude-haiku-4.5" {
		t.Errorf("TaskResult.Model = %q, want what the task named", result.Model)
	}
}
