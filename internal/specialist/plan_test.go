package specialist

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Twigpine/zero/internal/tools"
)

func planArgs(tasks []any, budget map[string]any) map[string]any {
	return map[string]any{"name": "p", "tasks": tasks, "budget": budget}
}

// okBudget is a budget that PASSES admission — refuseImplausibleBudget rejects a
// plan whose budget cannot cover its own tasks, and 1000 tokens for anything
// could not. Generous on purpose: these fixtures are about task behaviour, and a
// budget that runs out mid-fixture would make them about the budget instead.
func okBudget() map[string]any {
	return map[string]any{"max_workers": float64(1), "max_tokens": float64(2_000_000)}
}

func task(id, prompt string, deps ...string) map[string]any {
	out := map[string]any{"id": id, "prompt": prompt}
	if len(deps) > 0 {
		raw := make([]any, len(deps))
		for i, d := range deps {
			raw[i] = d
		}
		out["depends_on"] = raw
	}
	return out
}

func readOnlyLimits() Limits {
	// MaxTokens 0 mirrors production: defaultPlanMaxTokens is 0, meaning this run
	// puts NO ceiling on what a plan may request. A fixture with a ceiling
	// production does not have makes every budget test answer a question nobody
	// asks.
	return Limits{MaxTasks: 20, ParentTools: []string{"read_file", "grep", "glob"}}
}

// (e) ParsePlan is the ONLY constructor. A zero Plan is inert, so no exported
// path can produce an executable plan that skipped validation.
func TestParsePlanIsTheOnlyConstructor(t *testing.T) {
	var zero Plan
	if zero.TaskCount() != 0 || len(zero.Order()) != 0 || len(zero.Tasks()) != 0 {
		t.Fatal("a zero Plan must carry no tasks and no order")
	}
	// Executing one runs nothing and reports failed — never success.
	report := ExecutePlan(context.Background(), zero, nil, func(context.Context, PlanTaskRequest) (TaskResult, error) {
		t.Fatal("a zero Plan must not dispatch anything")
		return TaskResult{}, nil
	}, nil)
	if report.Status != PlanFailed {
		t.Fatalf("a zero Plan must report failed, got %q", report.Status)
	}
	// Tasks() returns a copy: mutating it cannot corrupt a validated plan.
	plan, err := ParsePlan(planArgs([]any{task("a", "do a")}, okBudget()), readOnlyLimits())
	if err != nil {
		t.Fatalf("ParsePlan: %v", err)
	}
	copied := plan.Tasks()
	copied[0].ID = "mutated"
	if plan.Tasks()[0].ID != "a" {
		t.Fatal("Tasks() must return a copy; a validated plan is immutable")
	}
}

// (f) A cycle is rejected AND the involved ids are named. Audit U24: a cyclic
// graph hangs forever precisely because nothing checks.
func TestParsePlanRejectsCyclesAndNamesThem(t *testing.T) {
	cases := []struct {
		name  string
		tasks []any
		want  []string
	}{
		{"two-node cycle", []any{task("a", "x", "b"), task("b", "y", "a")}, []string{"a", "b"}},
		{"three-node cycle", []any{task("a", "x", "c"), task("b", "y", "a"), task("c", "z", "b")}, []string{"a", "b", "c"}},
		{"cycle with an innocent bystander", []any{
			task("free", "x"), task("a", "y", "b"), task("b", "z", "a"),
		}, []string{"a", "b"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParsePlan(planArgs(tc.tasks, okBudget()), readOnlyLimits())
			if err == nil {
				t.Fatal("a cyclic plan must be rejected")
			}
			if !strings.Contains(err.Error(), "cycle") {
				t.Fatalf("the error must say it is a cycle: %v", err)
			}
			for _, id := range tc.want {
				if !strings.Contains(err.Error(), id) {
					t.Fatalf("the error must name %q so a 20-task plan is actionable: %v", id, err)
				}
			}
			if tc.name == "cycle with an innocent bystander" && strings.Contains(err.Error(), "free") {
				t.Fatalf("a task outside the cycle must not be named: %v", err)
			}
		})
	}
}

// A self-edge is a cycle of one and must be rejected too.
func TestParsePlanRejectsSelfDependency(t *testing.T) {
	_, err := ParsePlan(planArgs([]any{task("a", "x", "a")}, okBudget()), readOnlyLimits())
	if err == nil || !strings.Contains(err.Error(), "itself") {
		t.Fatalf("a self-dependency must be rejected, got %v", err)
	}
}

// (g) An unknown edge is REJECTED, never skipped: skipping would run a task
// whose stated precondition never existed.
func TestParsePlanRejectsUnknownDependency(t *testing.T) {
	_, err := ParsePlan(planArgs([]any{task("a", "x", "ghost")}, okBudget()), readOnlyLimits())
	if err == nil {
		t.Fatal("an unknown dependency must be rejected")
	}
	for _, want := range []string{"a", "ghost"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the error must name %q: %v", want, err)
		}
	}
}

// (h) Duplicate ids are rejected — otherwise the graph is ambiguous.
func TestParsePlanRejectsDuplicateIDs(t *testing.T) {
	_, err := ParsePlan(planArgs([]any{task("a", "x"), task("a", "y")}, okBudget()), readOnlyLimits())
	if err == nil || !strings.Contains(err.Error(), "unique") {
		t.Fatalf("duplicate ids must be rejected, got %v", err)
	}
}

// IDs use an ALLOW-LIST charset. A deny-list here would leak, as every
// deny-list in this repo has.
func TestParsePlanRejectsIDsOutsideTheAllowList(t *testing.T) {
	for _, bad := range []string{"a b", "a/b", "a.b", "../x", "a;b", "a\nb", "a$b", ""} {
		_, err := ParsePlan(planArgs([]any{task(bad, "x")}, okBudget()), readOnlyLimits())
		if err == nil {
			t.Fatalf("id %q must be rejected", bad)
		}
	}
	for _, good := range []string{"a", "A1", "read-file", "read_file", "a-b_c9"} {
		if _, err := ParsePlan(planArgs([]any{task(good, "x")}, okBudget()), readOnlyLimits()); err != nil {
			t.Fatalf("id %q must be accepted: %v", good, err)
		}
	}
}

// (i) MaxWorkers > 1 is REJECTED, not coerced. Coercion would let a caller
// believe it got concurrency and would make the field meaningless for Phase 3.
func TestMaxWorkersIsARangeAndIsRejectedOutsideIt(t *testing.T) {
	// THE RULE MOVED, IT DID NOT DISSOLVE. It used to be "must be exactly 1",
	// because the executor was sequential and a caller who asked for 8 and
	// silently got 1 would have been told nothing. The executor changed; that
	// reasoning did not, so the bound became a range and is still REJECTED
	// outside it rather than trimmed into it — a trimmed number is one nobody
	// can reason about afterwards.
	for _, workers := range []float64{0, -1, maxPlanWorkers + 1, 100} {
		budget := okBudget()
		budget["max_workers"] = workers
		_, err := ParsePlan(planArgs([]any{task("a", "x")}, budget), readOnlyLimits())
		if err == nil {
			t.Fatalf("max_workers %v must be rejected", workers)
		}
		if !strings.Contains(err.Error(), "between 1 and") {
			t.Fatalf("the error must state the range: %v", err)
		}
	}
	for _, workers := range []float64{1, 2, 8, maxPlanWorkers} {
		budget := okBudget()
		budget["max_workers"] = workers
		plan, err := ParsePlan(planArgs([]any{task("a", "x")}, budget), readOnlyLimits())
		if err != nil {
			t.Fatalf("max_workers %v must be accepted: %v", workers, err)
		}
		if plan.Budget().MaxWorkers != int(workers) {
			t.Fatalf("max_workers parsed as %d, want %v", plan.Budget().MaxWorkers, workers)
		}
	}
}

// (j) A budget object is required — max_workers must be stated — but
// max_tokens is OPTIONAL and unbounded when omitted.
//
// It used to be required and capped at 200k. Both went: the check ran only
// BETWEEN tasks, so a six-task chain asking for exactly 200k spent 469,555 and
// was cut short anyway. A number that neither bounds spend nor lets heavy work
// finish is worse than none, because it reads like a guarantee. Spend is still
// metered and reported; a caller that wants a bound still sets one.
func TestParsePlanRequiresABudgetButNotATokenCap(t *testing.T) {
	if _, err := ParsePlan(map[string]any{"tasks": []any{task("a", "x")}}, readOnlyLimits()); err == nil {
		t.Fatal("a plan with no budget object must be rejected")
	}

	unbounded := okBudget()
	delete(unbounded, "max_tokens")
	plan, err := ParsePlan(planArgs([]any{task("a", "x")}, unbounded), readOnlyLimits())
	if err != nil {
		t.Fatalf("an omitted max_tokens must be accepted as unbounded, got %v", err)
	}
	if plan.Budget().MaxTokens != 0 {
		t.Fatalf("an omitted max_tokens must read as 0 (unbounded), got %d", plan.Budget().MaxTokens)
	}

	negative := okBudget()
	negative["max_tokens"] = float64(-1)
	if _, err := ParsePlan(planArgs([]any{task("a", "x")}, negative), readOnlyLimits()); err == nil {
		t.Fatal("a negative max_tokens is meaningless and must be rejected")
	}
}

// A caller that DOES want a bound still gets one, enforced exactly as before.
func TestAnExplicitTokenBoundStillStopsThePlan(t *testing.T) {
	budget := okBudget()
	// Two tasks at the admission floor; each reports more than half of it, so
	// the bound is crossed after the first.
	budget["max_tokens"] = float64(100_000)
	plan, err := ParsePlan(planArgs([]any{task("a", "x"), task("b", "y")}, budget), readOnlyLimits())
	if err != nil {
		t.Fatalf("ParsePlan: %v", err)
	}
	report := ExecutePlan(context.Background(), plan, []string{"read_file"},
		func(_ context.Context, req PlanTaskRequest) (TaskResult, error) {
			// More than the whole budget, so the meter is negative before the
			// second dispatch is considered.
			return TaskResult{ID: req.Task.ID, Outcome: TaskSucceeded, Tokens: 110_000}, nil
		}, nil)
	if report.Skipped != 1 {
		t.Fatalf("an explicit bound must still cut the plan short: %+v", report)
	}
}

// ...and an unbounded plan runs every task, however much it spends.
func TestAnUnboundedPlanRunsEveryTask(t *testing.T) {
	budget := okBudget()
	delete(budget, "max_tokens")
	plan, err := ParsePlan(planArgs([]any{task("a", "x"), task("b", "y"), task("c", "z")}, budget), readOnlyLimits())
	if err != nil {
		t.Fatalf("ParsePlan: %v", err)
	}
	report := ExecutePlan(context.Background(), plan, []string{"read_file"},
		func(_ context.Context, req PlanTaskRequest) (TaskResult, error) {
			return TaskResult{ID: req.Task.ID, Outcome: TaskSucceeded, Tokens: 1_000_000}, nil
		}, nil)
	if report.Succeeded != 3 || report.Skipped != 0 {
		t.Fatalf("an unbounded plan must run every task: %+v", report)
	}
	if report.TokensUsed != 3_000_000 {
		t.Fatalf("spend must still be METERED when it is not bounded, got %d", report.TokensUsed)
	}
}

// (k) Depth is checked AT ADMISSION, and the message names the headroom rather
// than failing opaquely partway through a plan.
func TestParsePlanRejectsInsufficientDepthHeadroom(t *testing.T) {
	limits := readOnlyLimits()
	limits.CurrentDepth = maxSpecialistDepth - 1 // tasks would be AT the cap
	_, err := ParsePlan(planArgs([]any{task("a", "x")}, okBudget()), limits)
	if err == nil {
		t.Fatal("a plan with no depth headroom must be rejected at admission")
	}
	for _, want := range []string{"depth", "headroom"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the error must explain the headroom: %v", err)
		}
	}
	// One level shallower still fits.
	limits.CurrentDepth = maxSpecialistDepth - 2
	if _, err := ParsePlan(planArgs([]any{task("a", "x")}, okBudget()), limits); err != nil {
		t.Fatalf("a plan with headroom must be accepted: %v", err)
	}
}

// (l) ONE counting function, used by admission and the executor, agreeing on
// every size. The prototype counted source text and `agent ("x")` counted zero.
func TestTaskCountAgreesBetweenAdmissionAndExecution(t *testing.T) {
	for _, size := range []int{1, 2, 20} {
		raw := make([]any, size)
		for i := range raw {
			raw[i] = task(string(rune('a'+i%26))+strings.Repeat("x", i/26), "do it")
		}
		limits := readOnlyLimits()
		plan, err := ParsePlan(planArgs(raw, okBudget()), limits)
		if err != nil {
			t.Fatalf("size %d: %v", size, err)
		}
		if plan.TaskCount() != size {
			t.Fatalf("TaskCount = %d, want %d", plan.TaskCount(), size)
		}
		dispatched := 0
		ExecutePlan(context.Background(), plan, []string{"read_file"}, func(context.Context, PlanTaskRequest) (TaskResult, error) {
			dispatched++
			return TaskResult{Outcome: TaskSucceeded}, nil
		}, nil)
		if dispatched != plan.TaskCount() {
			t.Fatalf("executor dispatched %d, admission counted %d — the two disagree", dispatched, plan.TaskCount())
		}
	}
	// At-cap and over-cap.
	limits := readOnlyLimits()
	limits.MaxTasks = 2
	if _, err := ParsePlan(planArgs([]any{task("a", "x"), task("b", "y")}, okBudget()), limits); err != nil {
		t.Fatalf("at the cap must be accepted: %v", err)
	}
	if _, err := ParsePlan(planArgs([]any{task("a", "x"), task("b", "y"), task("c", "z")}, okBudget()), limits); err == nil {
		t.Fatal("over the cap must be rejected")
	}
	// Empty is rejected: a plan with no tasks is not a plan.
	if _, err := ParsePlan(planArgs([]any{}, okBudget()), limits); err == nil {
		t.Fatal("an empty plan must be rejected")
	}
}

// (n) A write tool is rejected — Phase 2 tasks are read-only.
func TestAWriteToolIsPermittedOnlyWhenTheParentHoldsIt(t *testing.T) {
	// THE RULE CHANGED, and this test changed with it rather than around it.
	//
	// It used to assert that a write tool is rejected "even if the PARENT holds
	// it", because plan tasks were read-only by construction. Write-capable
	// tasks are now permitted — gated on an approval that can show the plan and
	// on an isolated worktree — so the remaining bound is the parent's grant,
	// and that is what this now pins.
	for _, tool := range []string{"write_file", "edit_file", "apply_patch", "bash", "exec_command"} {
		raw := task("a", "x")
		raw["tools"] = []any{tool}

		held := readOnlyLimits()
		held.ParentTools = append(held.ParentTools, tool)
		plan, err := ParsePlan(planArgs([]any{raw}, okBudget()), held)
		if err != nil {
			t.Fatalf("tool %q is held by the parent and must be permitted: %v", tool, err)
		}
		// ...and naming it is what makes the plan require isolation, which is
		// the precondition that let this rule relax at all.
		if !plan.RequiresIsolation() {
			t.Fatalf("a plan naming %q does not require isolation", tool)
		}

		withheld := readOnlyLimits()
		if _, err := ParsePlan(planArgs([]any{raw}, okBudget()), withheld); err == nil {
			t.Fatalf("tool %q is NOT held by the parent and must be refused", tool)
		}
	}
}

// A task that names NOTHING stays read-only. Writing is opted into per task, by
// name — otherwise every unqualified task in every plan becomes write-capable
// the day the parent grant widens.
func TestAnUnqualifiedTaskNeverInheritsWriteTools(t *testing.T) {
	limits := readOnlyLimits()
	limits.ParentTools = append(limits.ParentTools, "write_file")
	plan, err := ParsePlan(planArgs([]any{task("a", "x")}, okBudget()), limits)
	if err != nil {
		t.Fatalf("ParsePlan: %v", err)
	}
	if plan.RequiresIsolation() {
		t.Fatal("a task that named no tools was treated as write-capable")
	}
	granted, err := planToolGrant(plan.Tasks()[0], limits.ParentTools)
	if err != nil {
		t.Fatalf("planToolGrant: %v", err)
	}
	for _, name := range granted {
		if name == "write_file" {
			t.Fatal("an unqualified task inherited write_file from the parent grant")
		}
	}
}

// (m) A task may NARROW the parent's grant, never widen it — at validation.
// The dispatch half is asserted separately, in plan_exec_test.go.
func TestParsePlanRejectsToolsOutsideTheParentGrant(t *testing.T) {
	raw := task("a", "x")
	raw["tools"] = []any{"read_file", "grep"}
	limits := Limits{MaxTasks: 20, ParentTools: []string{"read_file"}}
	_, err := ParsePlan(planArgs([]any{raw}, okBudget()), limits)
	if err == nil {
		t.Fatal("a task requesting a tool the parent does not hold must be rejected")
	}
	if !strings.Contains(err.Error(), "never widen") {
		t.Fatalf("the error must name the rule: %v", err)
	}
	// Narrowing is fine.
	raw["tools"] = []any{"read_file"}
	if _, err := ParsePlan(planArgs([]any{raw}, okBudget()), limits); err != nil {
		t.Fatalf("narrowing must be accepted: %v", err)
	}
}

// The topological order is a real order: every dependency precedes its
// dependent, and a diamond resolves.
func TestParsePlanEmitsAValidTopologicalOrder(t *testing.T) {
	// a -> b, a -> c, (b,c) -> d : the diamond.
	plan, err := ParsePlan(planArgs([]any{
		task("d", "join", "b", "c"), task("b", "left", "a"), task("c", "right", "a"), task("a", "root"),
	}, okBudget()), readOnlyLimits())
	if err != nil {
		t.Fatalf("ParsePlan: %v", err)
	}
	position := map[string]int{}
	for index, id := range plan.Order() {
		position[id] = index
	}
	if len(position) != 4 {
		t.Fatalf("order must contain every task: %v", plan.Order())
	}
	for _, edge := range [][2]string{{"a", "b"}, {"a", "c"}, {"b", "d"}, {"c", "d"}} {
		if position[edge[0]] >= position[edge[1]] {
			t.Fatalf("%s must precede %s in %v", edge[0], edge[1], plan.Order())
		}
	}
}

// The tool refuses when the posture is off, even if a model calls it by name —
// the registry dispatches by name, so display gating is not enforcement.
func TestOrchestrateRefusesWhenThePostureIsOff(t *testing.T) {
	tool := &OrchestrateTool{PostureActive: func() bool { return false }}
	res := tool.Run(context.Background(), planArgs([]any{task("a", "x")}, okBudget()))
	if res.Status != tools.StatusError {
		t.Fatalf("status = %v, want error", res.Status)
	}
	if !strings.Contains(res.Output, "zeromaxing") {
		t.Fatalf("the refusal must name the posture: %q", res.Output)
	}
}

// (b)(c)(d) POSTURE ON: the tool is usable, Safety reports Allow rather than
// Deny, and Deferred behaves as specified. These close the two gaps the
// posture-off identity test could not: "Safety always denies" and "Deferred
// never defers" were both invisible to it.
func TestOrchestratePostureOnGates(t *testing.T) {
	on := &OrchestrateTool{PostureActive: func() bool { return true }}
	off := &OrchestrateTool{PostureActive: func() bool { return false }}

	if got := on.Safety().Permission; got != tools.PermissionAllow {
		t.Fatalf("posture ON Safety().Permission = %v, want Allow — see the decision comment in plan_tool.go", got)
	}
	if got := off.Safety().Permission; got != tools.PermissionDeny {
		t.Fatalf("posture OFF Safety().Permission = %v, want Deny", got)
	}
	if on.Deferred() {
		t.Fatal("posture ON must un-defer the tool")
	}
	if !off.Deferred() {
		t.Fatal("posture OFF must defer the tool")
	}
	// DeferralEligible stays true in BOTH states, so un-deferring can never
	// drop the global eligible count below the threshold and force-expose every
	// other deferred tool.
	if !on.DeferralEligible() || !off.DeferralEligible() {
		t.Fatal("DeferralEligible must stay true in both states")
	}
	// A nil PostureActive is off — fail-safe for a tool that spends budget.
	if !(&OrchestrateTool{}).Deferred() {
		t.Fatal("an unwired tool must default to off")
	}
	if got := (&OrchestrateTool{}).Safety().Permission; got != tools.PermissionDeny {
		t.Fatalf("an unwired tool must deny, got %v", got)
	}
}

// A wired tool with no runner refuses rather than reporting a plan it never ran.
func TestOrchestrateRefusesWithoutARunner(t *testing.T) {
	tool := &OrchestrateTool{PostureActive: func() bool { return true }, ParentTools: []string{"read_file"}}
	res := tool.Run(context.Background(), planArgs([]any{task("a", "x")}, okBudget()))
	if res.Status != tools.StatusError || !strings.Contains(res.Output, "runner") {
		t.Fatalf("a tool with no runner must refuse loudly, got %v %q", res.Status, res.Output)
	}
}

// An invalid plan is refused by the TOOL, proving ParsePlan is on the live path
// and not merely present — the prototype's validator was reachable and never
// called.
func TestOrchestrateRunRejectsAnInvalidPlan(t *testing.T) {
	ran := false
	tool := &OrchestrateTool{
		PostureActive: func() bool { return true },
		ParentTools:   []string{"read_file"},
		RunTask: func(context.Context, PlanTaskRequest) (TaskResult, error) {
			ran = true
			return TaskResult{Outcome: TaskSucceeded}, nil
		},
	}
	res := tool.Run(context.Background(), planArgs([]any{task("a", "x", "b"), task("b", "y", "a")}, okBudget()))
	if res.Status != tools.StatusError || !strings.Contains(res.Output, "cycle") {
		t.Fatalf("the tool must reject a cyclic plan, got %v %q", res.Status, res.Output)
	}
	if ran {
		t.Fatal("no task may run when validation rejects the plan")
	}
}

var _ = errors.New
var _ = time.Second

// A non-completed plan must NOT report OK at the TOOL boundary.
//
// The executor's status was already asserted, but nothing checked what the tool
// hands back to the agent loop — and that is the boundary where "failure
// reported as success" actually reaches the model and the exit path. Audit
// RC-F is about exactly this seam.
func TestOrchestrateToolStatusFollowsThePlanStatus(t *testing.T) {
	cases := []struct {
		name    string
		runner  PlanRunner
		want    tools.Status
		wantSub string
	}{
		{"all succeed", func(context.Context, PlanTaskRequest) (TaskResult, error) {
			return TaskResult{Outcome: TaskSucceeded, Output: "ok"}, nil
		}, tools.StatusOK, "completed"},
		{"one fails -> partial", func(_ context.Context, req PlanTaskRequest) (TaskResult, error) {
			task := req.Task
			if task.ID == "b" {
				return TaskResult{Outcome: TaskFailed, Err: "no"}, errors.New("no")
			}
			return TaskResult{Outcome: TaskSucceeded, Output: "ok"}, nil
		}, tools.StatusError, "partial"},
		{"all fail -> failed", func(context.Context, PlanTaskRequest) (TaskResult, error) {
			return TaskResult{Outcome: TaskFailed, Err: "no"}, errors.New("no")
		}, tools.StatusError, "failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tool := &OrchestrateTool{
				PostureActive: func() bool { return true },
				ParentTools:   []string{"read_file"},
				RunTask:       tc.runner,
			}
			res := tool.Run(context.Background(), planArgs([]any{task("a", "x"), task("b", "y")}, okBudget()))
			if res.Status != tc.want {
				t.Fatalf("status = %v, want %v\n%s", res.Status, tc.want, res.Output)
			}
			if !strings.Contains(res.Output, tc.wantSub) {
				t.Fatalf("the summary must name the terminal status %q:\n%s", tc.wantSub, res.Output)
			}
			if got := res.Meta["plan_status"]; got != tc.wantSub {
				t.Fatalf("Meta[plan_status] = %q, want %q", got, tc.wantSub)
			}
			// max_speedup is surfaced in Meta so the kill criterion is machine
			// readable, not only human readable.
			if _, ok := res.Meta["max_speedup"]; !ok {
				t.Fatal("Meta must carry max_speedup — the number the Phase 3 decision rests on")
			}
		})
	}
}
