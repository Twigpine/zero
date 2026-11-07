package specialist

import (
	"context"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/streamjson"
)

// A PLAN MUST NOT DIE BECAUSE ITS INPUTS WERE CUT SHORT.
//
// From a real run: four finder tasks stopped at their token budget, three of
// them holding substantial partial findings, and the verify, sweep and synthesis
// tasks that depended on them were all skipped. Two runs, 3.6 million tokens, no
// report — while the evidence to write one sat unread in the results map.
func TestADependentRunsWhenSomeOfItsDependenciesWereCutShort(t *testing.T) {
	plan := mustPlan(t, []any{
		task("f1", "audit surface one"),
		task("f2", "audit surface two"),
		task("verify", "attack every claim", "f1", "f2"),
	}, okBudget(), readOnlyLimits())

	var verifyPrompt string
	report := ExecutePlan(context.Background(), plan, []string{"read_file"},
		func(_ context.Context, req PlanTaskRequest) (TaskResult, error) {
			switch req.Task.ID {
			case "f1":
				return TaskResult{Outcome: TaskSucceeded, Output: "f1 found: engine.go:143 evaluates first"}, nil
			case "f2":
				// Stopped at its budget, having written real findings first.
				return TaskResult{
					Outcome: TaskCancelled,
					Output:  "f2 found so far: runner.go:163 wires the keys",
					Err:     `task "f2" stopped: the plan's token budget ran out while it was running`,
				}, nil
			default:
				verifyPrompt = req.Task.Prompt
				return TaskResult{Outcome: TaskSucceeded, Output: "verified"}, nil
			}
		}, nil)

	if verifyPrompt == "" {
		t.Fatalf("the dependent was skipped even though a dependency had findings: %+v", report.Tasks)
	}
	if !strings.Contains(verifyPrompt, "engine.go:143") {
		t.Errorf("the succeeded dependency's findings are missing:\n%s", verifyPrompt)
	}
	if !strings.Contains(verifyPrompt, "runner.go:163") {
		t.Errorf("the cut-short dependency's partial findings were discarded:\n%s", verifyPrompt)
	}
	// LABELLED, and that is not optional: a reader handed an incomplete answer as
	// if it were finished treats its silences as findings.
	if !strings.Contains(verifyPrompt, "INCOMPLETE") {
		t.Errorf("partial work was presented as a finished result:\n%s", verifyPrompt)
	}
	if !strings.Contains(verifyPrompt, "not absent") {
		t.Errorf("the briefing does not say an unfinished input's silence proves nothing:\n%s", verifyPrompt)
	}
}

// WITH NOTHING TO WORK FROM, IT IS STILL SKIPPED. A task drawing a confident
// answer out of no evidence is worse than the gap it would have left.
func TestADependentIsStillSkippedWhenNoDependencyProducedAnything(t *testing.T) {
	plan := mustPlan(t, []any{
		task("f1", "audit"),
		task("verify", "attack every claim", "f1"),
	}, okBudget(), readOnlyLimits())

	dispatched := 0
	report := ExecutePlan(context.Background(), plan, []string{"read_file"},
		func(_ context.Context, req PlanTaskRequest) (TaskResult, error) {
			dispatched++
			return TaskResult{Outcome: TaskCancelled, Output: "", Err: "stopped before it wrote anything"}, nil
		}, nil)

	if dispatched != 1 {
		t.Fatalf("a dependent ran with no evidence at all: %d dispatches", dispatched)
	}
	byID := map[string]TaskResult{}
	for _, result := range report.Tasks {
		byID[result.ID] = result
	}
	if byID["verify"].Outcome != TaskSkippedDependency {
		t.Errorf("verify = %q, want skipped", byID["verify"].Outcome)
	}
	if !strings.Contains(byID["verify"].Err, "no dependency produced anything") {
		t.Errorf("the skip reason does not say why: %q", byID["verify"].Err)
	}
}

// A FAILED DEPENDENCY IS NOT PARTIAL EVIDENCE. It ran and did not deliver; its
// output is a harness diagnostic or an answer already judged wrong, and passing
// that on as a finding would launder a failure into a source.
func TestAFailedDependencyIsNotTreatedAsPartialEvidence(t *testing.T) {
	briefed := withDependencyBriefing(
		Task{ID: "v", Prompt: "judge", DependsOn: []string{"failed", "cancelled"}},
		map[string]TaskResult{
			"failed":    {Outcome: TaskFailed, Output: "Subagent failed (exit 3)\nerrors: provider request error"},
			"cancelled": {Outcome: TaskCancelled, Output: "real partial finding at foo.go:12"},
		},
	)
	if strings.Contains(briefed, "Subagent failed") {
		t.Error("a failed task's diagnostic was handed to a dependent as a finding")
	}
	if !strings.Contains(briefed, "real partial finding at foo.go:12") {
		t.Error("the cut-short task's genuine partial work was discarded")
	}
}

// WHAT A CUT-SHORT CHILD WROTE MUST SURVIVE THE BOUNDARY.
//
// The kill path returned an empty Result, so a task stopped at its token budget
// handed on nothing however much it had already found — and every dependent was
// then skipped for want of evidence that existed. Driven through the real
// Executor, because the discard happened at exactly that seam.
func TestATaskStoppedAtItsBudgetStillHandsOnWhatItWrote(t *testing.T) {
	exec := Executor{
		BinaryPath:   "/bin/true",
		NewSessionID: func() (string, error) { return "specialist_00000000000000000000000a", nil },
		Load:         func(LoadOptions) (LoadResult, error) { return LoadResult{}, nil },
		RunChild: func(ctx context.Context, _ string, _ []string, progress func(streamjson.Event)) (ChildRunResult, error) {
			// Writes real findings, then keeps spending until the meter stops it.
			events := []streamjson.Event{
				{Type: streamjson.EventText, Delta: "found: engine.go:143 evaluates paths first"},
			}
			for _, event := range events {
				if progress != nil {
					progress(event)
				}
			}
			for round := 0; round < 10; round++ {
				spent := 50_000
				usage := streamjson.Event{Type: streamjson.EventUsage, TotalTokens: &spent}
				events = append(events, usage)
				if progress != nil {
					progress(usage)
				}
				if ctx.Err() != nil {
					return ChildRunResult{Started: true, ExitCode: -1, Events: events}, ctx.Err()
				}
			}
			return ChildRunResult{Started: true, Events: events}, nil
		},
	}
	run := NewPlanRunner(PlanTaskContext{Executor: exec, Cwd: t.TempDir(), SpecialistName: "explorer"})
	result, _ := run(context.Background(), PlanTaskRequest{
		Task:          Task{ID: "finder", Prompt: "audit"},
		Tools:         []string{"read_file"},
		MaxTaskTokens: 120_000,
	})

	if result.Outcome != TaskCancelled {
		t.Fatalf("the budget did not stop the task: %s", result.Outcome)
	}
	if !strings.Contains(result.Output, "engine.go:143") {
		t.Fatalf("the work it had already done was discarded at the boundary: %q", result.Output)
	}
}
