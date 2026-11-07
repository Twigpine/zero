package specialist

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/Twigpine/zero/internal/tools"
)

// EVERY TEMPLATE MUST ADMIT. The whole point is that a model asking for a shape
// by name cannot produce plan JSON that admission refuses — which is the turn a
// real run spent discovering budget.max_tokens_per_task: 5000 was too low.
func TestEveryTemplateProducesAPlanThatAdmits(t *testing.T) {
	samples := map[string]map[string]string{
		"audit":   {"subject": "the retry watchdog"},
		"compare": {"before": "the old scheduler", "after": "the new scheduler"},
		"sweep":   {"question": "does this package validate its input?", "targets": "internal/tui, internal/cli, internal/sandbox"},
	}
	limits := Limits{MaxTasks: 50, ParentTools: PlanReadOnlyToolNames()}

	for _, template := range PlanTemplates() {
		t.Run(template.Name, func(t *testing.T) {
			params, ok := samples[template.Name]
			if !ok {
				t.Fatalf("template %q has no sample here: a template nothing exercises is untested", template.Name)
			}
			args, err := BuildTemplatePlan(template.Name, params)
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			plan, err := ParsePlan(args, limits)
			if err != nil {
				t.Fatalf("the template emitted a plan admission refuses: %v", err)
			}
			if plan.TaskCount() < 2 {
				t.Fatalf("a %d-task plan is not worth a plan", plan.TaskCount())
			}
			// No placeholder survived into a prompt.
			for _, task := range plan.Tasks() {
				if strings.Contains(task.Prompt, "${") || strings.TrimSpace(task.Prompt) == "" {
					t.Fatalf("task %q has an unfilled or empty prompt: %q", task.ID, task.Prompt)
				}
			}
			// And the parameters actually reached the prompts, rather than the
			// template emitting a generic graph that ignores its input.
			// Every supplied value must appear, and a comma-separated list must
			// appear ITEM BY ITEM — a sweep that mentioned the list verbatim but
			// gave no target its own task would pass a laxer check.
			joined := strings.ToLower(renderPlanPrompts(plan))
			for _, value := range params {
				for _, piece := range strings.Split(value, ",") {
					piece = strings.ToLower(strings.TrimSpace(piece))
					if piece == "" {
						continue
					}
					if !strings.Contains(joined, piece) {
						t.Errorf("parameter value %q never reached any prompt", piece)
					}
				}
			}
		})
	}
}

// FAILS CLOSED IN BOTH DIRECTIONS, the same contract expandPlanParams holds for
// saved plans.
func TestATemplateRefusesMissingAndUnknownParameters(t *testing.T) {
	if _, err := BuildTemplatePlan("audit", nil); err == nil {
		t.Fatal("audit built a plan with no subject: five tasks would examine an empty string")
	}
	if _, err := BuildTemplatePlan("audit", map[string]string{"subject": "   "}); err == nil {
		t.Fatal("whitespace was accepted as a subject")
	}
	if _, err := BuildTemplatePlan("audit", map[string]string{"subject": "x", "scope": "y"}); err == nil {
		t.Fatal("an unknown parameter was silently dropped: the caller meant it")
	}
	if _, err := BuildTemplatePlan("nosuchtemplate", nil); err == nil {
		t.Fatal("an unknown template name produced a plan")
	}
	// And the refusal names the alternatives, or the model cannot recover.
	_, err := BuildTemplatePlan("nosuchtemplate", nil)
	for _, name := range []string{"audit", "compare", "sweep"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("the refusal does not name %q: %v", name, err)
		}
	}
}

func TestSweepRefusesTargetsThatNormalizeToNothing(t *testing.T) {
	for _, targets := range []string{",", ", ,", " \t,\n,  "} {
		if _, err := BuildTemplatePlan("sweep", map[string]string{"question": "q", "targets": targets}); err == nil {
			t.Fatalf("targets %q produced a combine-only plan", targets)
		}
	}
}

// A VARIABLE TASK COUNT is the shape a model most often gets wrong by hand: one
// unique id per target, every one of them listed in the synthesis dependency.
func TestSweepWiresEveryTargetIntoTheSynthesis(t *testing.T) {
	args, err := BuildTemplatePlan("sweep", map[string]string{
		"question": "does this validate input?",
		"targets":  "internal/tui, internal/cli, internal/sandbox, internal/agent",
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := ParsePlan(args, Limits{MaxTasks: 50, ParentTools: PlanReadOnlyToolNames()})
	if err != nil {
		t.Fatalf("sweep does not admit: %v", err)
	}
	if plan.TaskCount() != 5 {
		t.Fatalf("4 targets produced %d tasks, want 4 + 1 synthesis", plan.TaskCount())
	}
	var combine Task
	ids := map[string]bool{}
	for _, task := range plan.Tasks() {
		ids[task.ID] = true
		if task.ID == "combine" {
			combine = task
		}
	}
	if len(ids) != plan.TaskCount() {
		t.Fatal("two tasks share an id, so one target's work is lost")
	}
	if len(combine.DependsOn) != 4 {
		t.Fatalf("the synthesis waits on %d of 4 targets: the rest are read before they finish", len(combine.DependsOn))
	}
	for _, dep := range combine.DependsOn {
		if !ids[dep] {
			t.Fatalf("the synthesis depends on %q, which is not a task", dep)
		}
	}
}

// TARGETS THAT SANITISE TO THE SAME STRING MUST NOT COLLIDE. A collision drops
// one target's task and its dependency edge, silently.
func TestSweepTargetsThatLookAlikeStillGetDistinctTasks(t *testing.T) {
	args, err := BuildTemplatePlan("sweep", map[string]string{
		"question": "q",
		"targets":  "internal/tui, internal/TUI, internal-tui, internal tui",
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := ParsePlan(args, Limits{MaxTasks: 50, ParentTools: PlanReadOnlyToolNames()})
	if err != nil {
		t.Fatalf("look-alike targets do not admit: %v", err)
	}
	if plan.TaskCount() != 5 {
		t.Fatalf("4 look-alike targets produced %d tasks: ids collided", plan.TaskCount())
	}
}

// A sweep over more targets than the worker ceiling must still admit — the plan
// runs them in waves rather than being refused.
func TestALargeSweepStaysInsideTheWorkerCeiling(t *testing.T) {
	var targets []string
	for i := 0; i < 30; i++ {
		targets = append(targets, fmt.Sprintf("pkg%d", i))
	}
	args, err := BuildTemplatePlan("sweep", map[string]string{
		"question": "q", "targets": strings.Join(targets, ","),
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := ParsePlan(args, Limits{MaxTasks: 50, ParentTools: PlanReadOnlyToolNames()})
	if err != nil {
		t.Fatalf("a 30-target sweep does not admit: %v", err)
	}
	if workers := plan.Budget().MaxWorkers; workers < 1 || workers > maxPlanWorkers {
		t.Fatalf("max_workers = %d, outside 1..%d", workers, maxPlanWorkers)
	}
	// A trailing comma must not become a task with no target.
	trailing, err := BuildTemplatePlan("sweep", map[string]string{"question": "q", "targets": "a,b,"})
	if err != nil {
		t.Fatal(err)
	}
	if plan, err := ParsePlan(trailing, Limits{MaxTasks: 50, ParentTools: PlanReadOnlyToolNames()}); err != nil {
		t.Fatalf("a trailing comma broke the plan: %v", err)
	} else if plan.TaskCount() != 3 {
		t.Fatalf("'a,b,' produced %d tasks, want 2 + 1", plan.TaskCount())
	}
}

// A generated plan is READ-ONLY. It names no tools, so it inherits the
// read-only grant and never trips RequiresIsolation — a template that quietly
// demanded a git worktree would refuse to run wherever one is unavailable.
func TestGeneratedPlansAreReadOnlyAndNeedNoWorktree(t *testing.T) {
	for _, sample := range []struct {
		name   string
		params map[string]string
	}{
		{"audit", map[string]string{"subject": "s"}},
		{"compare", map[string]string{"before": "a", "after": "b"}},
		{"sweep", map[string]string{"question": "q", "targets": "a,b"}},
	} {
		args, err := BuildTemplatePlan(sample.name, sample.params)
		if err != nil {
			t.Fatal(err)
		}
		plan, err := ParsePlan(args, Limits{MaxTasks: 50, ParentTools: PlanReadOnlyToolNames()})
		if err != nil {
			t.Fatal(err)
		}
		if plan.RequiresIsolation() {
			t.Errorf("template %q requires a worktree: it cannot run where git is unavailable", sample.name)
		}
	}
}

// Each template must be distinguishable, or the model picks at random.
func TestEveryTemplateSaysWhatItIsFor(t *testing.T) {
	seen := map[string]bool{}
	for _, template := range PlanTemplates() {
		if strings.TrimSpace(template.WhenToUse) == "" {
			t.Errorf("template %q says nothing about when to use it", template.Name)
		}
		if seen[template.Name] {
			t.Errorf("two templates named %q", template.Name)
		}
		seen[template.Name] = true
		if len(template.Params) == 0 {
			t.Errorf("template %q takes no parameters, so it cannot be about anything", template.Name)
		}
	}
}

func renderPlanPrompts(plan Plan) string {
	var b strings.Builder
	for _, task := range plan.Tasks() {
		b.WriteString(task.Prompt)
		b.WriteString("\n")
	}
	b.WriteString(plan.Description())
	return b.String()
}

// THE TOOL PATH, not just the builder. A template the orchestrate tool cannot
// resolve is a feature nothing can reach — the "layer B does not carry it"
// defect this branch has produced repeatedly.
func TestTheOrchestrateToolResolvesATemplate(t *testing.T) {
	args, err := resolveTemplatePlan(map[string]any{
		"template": "audit",
		"params":   map[string]any{"subject": "the retry watchdog"},
	})
	if err != nil {
		t.Fatalf("the tool could not resolve a template: %v", err)
	}
	plan, err := ParsePlan(args, Limits{MaxTasks: 50, ParentTools: PlanReadOnlyToolNames()})
	if err != nil {
		t.Fatalf("the resolved template does not admit: %v", err)
	}
	if plan.TaskCount() != 5 {
		t.Fatalf("audit produced %d tasks", plan.TaskCount())
	}
	if !strings.Contains(renderPlanPrompts(plan), "the retry watchdog") {
		t.Fatal("the subject never reached the resolved plan")
	}
}

// A template alongside hand-written tasks is neither one, and picking silently
// would run something the caller did not describe.
func TestATemplateAlongsideTasksIsRefused(t *testing.T) {
	for _, field := range []string{"tasks", "saved"} {
		_, err := resolveTemplatePlan(map[string]any{
			"template": "audit",
			"params":   map[string]any{"subject": "x"},
			field:      "anything",
		})
		if err == nil {
			t.Fatalf("template + %s was silently resolved to one of them", field)
		}
	}
}

// Execution directives say HOW to run, not WHAT to run, so they survive the
// swap — dropping them silently ran a background plan in the foreground once.
func TestExecutionDirectivesSurviveTemplateResolution(t *testing.T) {
	args, err := resolveTemplatePlan(map[string]any{
		"template":    "audit",
		"params":      map[string]any{"subject": "x"},
		"background":  true,
		"auto_assign": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if args["background"] != true || args["auto_assign"] != true {
		t.Fatalf("directives were dropped: %+v", args)
	}
}

// No template named means the arguments pass through untouched — every existing
// call site takes this path.
func TestNoTemplateLeavesTheArgumentsAlone(t *testing.T) {
	original := map[string]any{"name": "p", "tasks": []any{}}
	got, err := resolveTemplatePlan(original)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(original) || got["name"] != "p" {
		t.Fatalf("arguments were rewritten without a template: %+v", got)
	}
}

// AND THE TOOL MUST ACTUALLY RESOLVE IT. The test above calls
// resolveTemplatePlan directly and proves nothing about whether the tool
// consults it — a mutation that dropped the call from Run passed it cleanly.
// Same seam, same lesson as TestTheToolPrintsWhereAWritePlanWrote.
func TestTheToolRunsATemplateEndToEnd(t *testing.T) {
	// LOCKED, because a sweep sets max_workers to its target count and the
	// runner really is called from several goroutines at once. The race
	// detector caught this the first time it ran, which is the point of it.
	var mu sync.Mutex
	var ran []string
	tool := &OrchestrateTool{
		PostureActive: func() bool { return true },
		ParentTools:   PlanReadOnlyToolNames(),
		RunTask: func(_ context.Context, req PlanTaskRequest) (TaskResult, error) {
			mu.Lock()
			ran = append(ran, req.Task.ID)
			mu.Unlock()
			return TaskResult{ID: req.Task.ID, Outcome: TaskSucceeded, Output: "looked at " + req.Task.ID}, nil
		},
	}
	result := tool.RunWithOptions(context.Background(), map[string]any{
		"template": "sweep",
		"params":   map[string]any{"question": "does it validate input?", "targets": "internal/tui, internal/cli"},
	}, tools.RunOptions{Model: "m"})

	if result.Status == tools.StatusError {
		t.Fatalf("the tool refused a template it advertises: %s", result.Output)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(ran) != 3 {
		t.Fatalf("the template ran %d tasks (%v), want 2 targets + 1 synthesis", len(ran), ran)
	}
}

// A template the tool cannot build must be refused with a usable message, not
// run as an empty plan.
func TestTheToolRefusesAnUnknownTemplateByName(t *testing.T) {
	tool := &OrchestrateTool{
		PostureActive: func() bool { return true },
		ParentTools:   PlanReadOnlyToolNames(),
		RunTask: func(context.Context, PlanTaskRequest) (TaskResult, error) {
			t.Fatal("a task ran for a template that does not exist")
			return TaskResult{}, nil
		},
	}
	result := tool.RunWithOptions(context.Background(), map[string]any{
		"template": "nosuchshape",
	}, tools.RunOptions{Model: "m"})
	if result.Status != tools.StatusError {
		t.Fatalf("an unknown template was accepted: %s", result.Output)
	}
	if !strings.Contains(result.Output, "audit") {
		t.Fatalf("the refusal does not name the real templates: %s", result.Output)
	}
}
