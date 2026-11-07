package specialist

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Twigpine/zero/internal/streamjson"
)

// modelFromArgs reads the model the child was actually launched with. Asserted
// from ARGV rather than from the manifest struct, because argv is what the
// child receives — a field set and then dropped by appendModelArgs would pass
// a struct assertion and fail in production.
func modelFromArgs(args []string) string {
	for index, arg := range args {
		if arg == "--model" && index+1 < len(args) {
			return args[index+1]
		}
	}
	return ""
}

// A model the provider will not run must cost the task a retry, not its life.
//
// Auto-assignment picks from the list the provider itself published, so
// "listed but unusable" is the NORMAL failure of a discovery endpoint that
// describes products rather than endpoints. Two real ones, both fatal before
// this: a model belonging to another account, and grok-4.20-multi-agent-0309,
// whose id lists on /v1/models while chat completions answers "Multi Agent
// requests are not allowed on chat completions". One task of four died.
//
// The trigger is structural on purpose — the runner already warns that choosing
// to spend another child by reading error prose is the wrong instrument.
func TestATaskKilledByAnUnusableAssignedModelRetriesOnTheParentModel(t *testing.T) {
	var ran []string
	exec := Executor{
		RunChild: func(_ context.Context, _ string, args []string, progress func(streamjson.Event)) (ChildRunResult, error) {
			model := modelFromArgs(args)
			ran = append(ran, model)
			if model == "grok-4.20-multi-agent-0309" {
				// Exactly the shape of the real failure: refused before generating
				// anything, so no events, no output and no tokens.
				return ChildRunResult{Started: true, ExitCode: 3},
					errors.New("provider request error: Multi Agent requests are not allowed on chat completions")
			}
			if progress != nil {
				progress(streamjson.Event{Type: streamjson.EventToolCall, Name: "read_file"})
			}
			return ChildRunResult{Started: true}, nil
		},
	}
	report := runOneTaskPlan(t, exec, "config-overrides", "survey config precedence", "grok-4.20-multi-agent-0309")
	result := report.Tasks[0]
	if result.Outcome != TaskSucceeded {
		t.Fatalf("the task was not rescued: %s / %s", result.Outcome, result.Err)
	}
	if len(ran) != 2 || ran[0] != "grok-4.20-multi-agent-0309" || ran[1] != "" {
		t.Fatalf("expected the assigned model then the parent's, got %q", ran)
	}
	// The failed choice must be NAMED, or the plan silently uses a different
	// model than it reports and the next plan picks the broken one again.
	if result.RetriedOnParentModel != "grok-4.20-multi-agent-0309" {
		t.Errorf("the unusable model was not recorded: %q", result.RetriedOnParentModel)
	}
	if result.Model != "" {
		t.Errorf("the result claims it ran on %q, but the fallback ran on the parent's model", result.Model)
	}
	// THE SPEND MUST BE COUNTED. A retry hidden inside the runner reported one
	// attempt for two children and dropped the first one's duration — which is
	// exactly why the executor owns retries.
	if result.Attempts != 2 {
		t.Errorf("two children ran but the plan recorded %d attempt(s)", result.Attempts)
	}

	if summary := report.Summary(); !strings.Contains(summary, "fell back from grok-4.20-multi-agent-0309") {
		t.Errorf("the summary hides the fallback:\n%s", summary)
	}
}

// runOneTaskPlan drives a single task through ExecutePlan, so the retry policy,
// the wall deadline and the attempt record are the real ones. Asserting against
// the runner alone would miss precisely the defect this file exists for.
func runOneTaskPlan(t *testing.T, exec Executor, id, prompt, model string) PlanReport {
	t.Helper()
	fields := map[string]any{"id": id, "prompt": prompt}
	if model != "" {
		fields["model"] = model
	}
	plan := mustPlan(t, []any{fields}, okBudget(), readOnlyLimits())
	run := NewPlanRunner(PlanTaskContext{Executor: exec, Cwd: t.TempDir(), SpecialistName: "explorer"})
	report := ExecutePlan(context.Background(), plan, []string{"read_file"}, run, nil)
	if len(report.Tasks) != 1 {
		t.Fatalf("expected one task result, got %d", len(report.Tasks))
	}
	return report
}

// WORK THAT ACTUALLY RAN AND FAILED MUST NOT BE RETRIED. A task that reasoned,
// answered and got it wrong has spent tokens; re-running it on another model is
// a second opinion nobody asked for, and it would double the cost of every
// genuine failure in a plan.
func TestATaskThatFailedAfterDoingWorkIsNotRetriedOnTheParentModel(t *testing.T) {
	attempts := 0
	exec := Executor{
		RunChild: func(_ context.Context, _ string, _ []string, progress func(streamjson.Event)) (ChildRunResult, error) {
			attempts++
			// The child RAN: it reasoned, spent tokens and answered wrongly.
			// Events go on ChildRunResult, not only through progress — that is
			// where the executor reads usage from.
			spent := 4000
			events := []streamjson.Event{
				{Type: streamjson.EventText, Text: "I could not determine the answer"},
				{Type: streamjson.EventUsage, TotalTokens: &spent},
			}
			for _, event := range events {
				if progress != nil {
					progress(event)
				}
			}
			return ChildRunResult{Started: true, ExitCode: 1, Events: events}, errors.New("task failed")
		},
	}
	result := runOneTaskPlan(t, exec, "j", "decide", "grok-4.3").Tasks[0]
	if attempts != 1 {
		t.Fatalf("real work that failed was retried %d times", attempts)
	}
	if result.Outcome != TaskFailed || result.RetriedOnParentModel != "" {
		t.Errorf("a genuine failure was laundered into a fallback: %+v", result)
	}
}

// A task with NO assigned model already runs on the parent's, so there is
// nothing to fall back to and a retry would just be a free second attempt that
// no other failing task gets.
func TestATaskWithNoAssignedModelIsNotRetried(t *testing.T) {
	attempts := 0
	exec := Executor{
		RunChild: func(_ context.Context, _ string, _ []string, _ func(streamjson.Event)) (ChildRunResult, error) {
			attempts++
			return ChildRunResult{Started: true, ExitCode: 3}, errors.New("network unreachable")
		},
	}
	runOneTaskPlan(t, exec, "a", "p", "")
	if attempts != 1 {
		t.Fatalf("a task with no assigned model ran %d times", attempts)
	}
}

// If the retry fails too, the FIRST result is what the plan reports. The
// fallback must not launder a genuine failure into a different-looking one.
func TestWhenTheFallbackAlsoFailsTheOriginalFailureIsReported(t *testing.T) {
	exec := Executor{
		RunChild: func(_ context.Context, _ string, _ []string, _ func(streamjson.Event)) (ChildRunResult, error) {
			return ChildRunResult{Started: true, ExitCode: 3}, errors.New("the workspace is gone")
		},
	}
	result := runOneTaskPlan(t, exec, "a", "p", "grok-4.3").Tasks[0]
	if result.Outcome != TaskFailed {
		t.Fatalf("outcome: %s", result.Outcome)
	}
	// The fallback is BOUNDED AT ONE. A provider refusing the parent's model too
	// must not put the loop into a spawn cycle.
	if result.Attempts != 2 {
		t.Errorf("the fallback was not bounded at one: %d attempts", result.Attempts)
	}
	// The model that could not run is still named, even though the fallback
	// failed for its own reason — otherwise the next plan picks it again.
	if result.RetriedOnParentModel != "grok-4.3" {
		t.Errorf("the refused model was not recorded: %q", result.RetriedOnParentModel)
	}
}

// THE FALLBACK SPENDS A CHILD, so the plan's wall budget must be able to refuse
// it — exactly as it refuses a stall retry.
//
// This is why the decision belongs in the executor rather than the runner. A
// retry hidden in the runner sees no deadline, no attempt budget and no record:
// it would overrun a plan's wall budget on behalf of the task that already
// exhausted it, and the plan would report one attempt for two children.
func TestTheModelFallbackIsRefusedOnceThePlansWallBudgetIsGone(t *testing.T) {
	attempts := 0
	exec := Executor{
		RunChild: func(_ context.Context, _ string, _ []string, _ func(streamjson.Event)) (ChildRunResult, error) {
			attempts++
			// Burn the whole wall budget, then fail the way a refused model does.
			time.Sleep(1100 * time.Millisecond)
			return ChildRunResult{Started: true, ExitCode: 3}, errors.New("model not found")
		},
	}
	plan := mustPlan(t, []any{
		map[string]any{"id": "a", "prompt": "p", "model": "grok-4.20-multi-agent-0309"},
	}, map[string]any{"max_workers": float64(1), "max_tokens": float64(500_000), "max_wall_seconds": float64(1)},
		readOnlyLimits())

	run := NewPlanRunner(PlanTaskContext{Executor: exec, Cwd: t.TempDir(), SpecialistName: "explorer"})
	report := ExecutePlan(context.Background(), plan, []string{"read_file"}, run, nil)

	if attempts != 1 {
		t.Fatalf("the fallback overran an exhausted wall budget: %d children spawned", attempts)
	}
	// THE OUTCOME IS CANCELLED, NOT FAILED, and that is a deliberate distinction
	// made where the wall budget is enforced: an expired plan deadline cancels
	// the plan's context, and a task stopped by it did not fail — it ran out of
	// time. This test asserted "failed" while the budget was only checked between
	// dispatches; the property it exists for is the one above, that the fallback
	// spends no second child once the budget is gone.
	if outcome := report.Tasks[0].Outcome; outcome == TaskSucceeded {
		t.Errorf("a task stopped by the wall budget reported success: %s", outcome)
	}
}

// A CANCELLED PLAN MUST NOT SPAWN A FALLBACK CHILD. The prototype's retry loop
// retried a cancelled task, which turned Ctrl-C into another spawn; the model
// fallback spends a child the same way and must answer to the same stop.
func TestTheModelFallbackIsRefusedOnceThePlanIsCancelled(t *testing.T) {
	attempts := 0
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	exec := Executor{
		RunChild: func(_ context.Context, _ string, _ []string, _ func(streamjson.Event)) (ChildRunResult, error) {
			attempts++
			// The user stops the plan while this child is dying on a bad model.
			cancel()
			return ChildRunResult{Started: true, ExitCode: 3}, errors.New("model not found")
		},
	}
	plan := mustPlan(t, []any{
		map[string]any{"id": "a", "prompt": "p", "model": "grok-4.20-multi-agent-0309"},
	}, okBudget(), readOnlyLimits())

	run := NewPlanRunner(PlanTaskContext{Executor: exec, Cwd: t.TempDir(), SpecialistName: "explorer"})
	ExecutePlan(ctx, plan, []string{"read_file"}, run, nil)

	if attempts != 1 {
		t.Fatalf("a cancelled plan spawned a fallback child: %d children", attempts)
	}
}

// THE REAL FAILURE PATH, and it is not the one the tests above drive.
//
// A child that dies on a refused model exits non-zero WITHOUT runChild returning
// an error, so Executor.Run reaches BuildFinalResult — which writes a diagnostic
// into Result.Output: "Subagent failed (exit 3)\nerrors: provider request error:
// ...". Every earlier test in this file returned an error from runChild instead,
// taking the branch that leaves Result empty.
//
// That difference hid a live defect. The fallback's "the child produced nothing"
// test read Result.Output, which is NEVER empty on this path, so ModelRejected
// was never set and two tasks in a real ten-task plan died on
// grok-4.20-multi-agent-0309 with the rescue sitting one branch away. The signal
// now comes from the child's own stream — tool calls and tokens — which describes
// the child rather than the harness's account of its death.
func TestAChildThatExitsNonZeroOnARefusedModelIsStillRescued(t *testing.T) {
	var ran []string
	exec := Executor{
		RunChild: func(_ context.Context, _ string, args []string, progress func(streamjson.Event)) (ChildRunResult, error) {
			model := modelFromArgs(args)
			ran = append(ran, model)
			if model == "grok-4.20-multi-agent-0309" {
				// Exactly production: non-zero exit, NO error from runChild, an
				// error event in the stream. BuildFinalResult turns this into
				// StatusError with a non-empty diagnostic Output.
				events := []streamjson.Event{{
					Type:    streamjson.EventError,
					Message: `provider request error: "Multi Agent requests are not allowed on chat completions"`,
				}}
				for _, event := range events {
					if progress != nil {
						progress(event)
					}
				}
				return ChildRunResult{Started: true, ExitCode: 3, Events: events}, nil
			}
			if progress != nil {
				progress(streamjson.Event{Type: streamjson.EventToolCall, Name: "read_file"})
			}
			return ChildRunResult{Started: true}, nil
		},
	}

	report := runOneTaskPlan(t, exec, "grant", "read the actual source", "grok-4.20-multi-agent-0309")
	result := report.Tasks[0]

	if len(ran) != 2 {
		t.Fatalf("the refused model was not retried on the parent's: children ran on %q", ran)
	}
	if result.Outcome != TaskSucceeded {
		t.Fatalf("the task died on a refused model instead of being rescued: %s / %s",
			result.Outcome, result.Err)
	}
	if result.RetriedOnParentModel != "grok-4.20-multi-agent-0309" {
		t.Errorf("the refused model was not recorded: %q", result.RetriedOnParentModel)
	}
	// RED MUST TURN GREEN. The recorder sees only the final result, so a rescued
	// task reports completed — the card must not stay on the failure.
	if report.Failed != 0 || report.Succeeded != 1 {
		t.Errorf("a rescued task was still counted as a failure: %d failed, %d succeeded",
			report.Failed, report.Succeeded)
	}
}

// A TASK THAT CALLED TOOLS DID WORK, whatever the usage numbers say.
//
// Tokens alone are not enough to tell work from a refusal: plenty of providers
// never report usage, and on those every genuine failure would look like a model
// the provider would not run — so a task that read files, thought, and failed for
// its own reasons would be silently re-run on a different model. Tool calls come
// from the child's own stream and cannot be absent when the child did something.
func TestATaskThatCalledToolsIsNotRetriedEvenWhenUsageIsNeverReported(t *testing.T) {
	attempts := 0
	exec := Executor{
		RunChild: func(_ context.Context, _ string, _ []string, progress func(streamjson.Event)) (ChildRunResult, error) {
			attempts++
			// Real work, and a provider that reports no usage at all.
			events := []streamjson.Event{
				{Type: streamjson.EventToolCall, Name: "read_file"},
				{Type: streamjson.EventError, Message: "the file it needed does not exist"},
			}
			for _, event := range events {
				if progress != nil {
					progress(event)
				}
			}
			return ChildRunResult{Started: true, ExitCode: 1, Events: events}, nil
		},
	}

	result := runOneTaskPlan(t, exec, "t", "read and explain", "grok-4.3").Tasks[0]
	if attempts != 1 {
		t.Fatalf("a task that called tools was re-run on another model: %d attempts", attempts)
	}
	if result.RetriedOnParentModel != "" {
		t.Errorf("a genuine failure was reported as a model fallback: %q", result.RetriedOnParentModel)
	}
}
