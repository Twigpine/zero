package specialist

import (
	"context"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/streamjson"
)

// childRunner returns an Executor whose child emits the given tool output and
// then answers with the given text — the shape of a task that runs a command
// and then reports a number about it.
func childRunner(t *testing.T, toolOutput, answer string) PlanRunner {
	t.Helper()
	return childRunnerEvents(t, []streamjson.Event{
		{Type: streamjson.EventToolCall, ID: "measure-1", Name: "exec_command", Args: map[string]any{"cmd": "go test -json ./..."}},
		{Type: streamjson.EventToolResult, ID: "measure-1", Name: "exec_command", Status: "ok", Output: toolOutput},
		{Type: streamjson.EventFinal, Text: answer},
	})
}

func childRunnerEvents(t *testing.T, events []streamjson.Event) PlanRunner {
	t.Helper()
	executor := Executor{
		BinaryPath:   "/bin/true",
		NewSessionID: func() (string, error) { return "specialist_00000000000000000000000a", nil },
		Load:         func(LoadOptions) (LoadResult, error) { return LoadResult{}, nil },
		RunChild: func(_ context.Context, _ string, _ []string, progress func(streamjson.Event)) (ChildRunResult, error) {
			// A real child does BOTH: streams each event as it happens (which is
			// what the ledger reads) and returns them for SummarizeStream to fold
			// into the task's output. A fixture that only did one would test a
			// path production never takes.
			for _, event := range events {
				if progress != nil {
					progress(event)
				}
			}
			return ChildRunResult{Started: true, Events: events}, nil
		},
	}
	return NewPlanRunner(PlanTaskContext{Executor: executor, Cwd: t.TempDir(), SpecialistName: "explorer"})
}

const taskSuiteOutput = `{"Action":"pass","Package":"github.com/x/y","Test":"TestChattyChild","Elapsed":0.86}
{"Action":"pass","Package":"github.com/x/y","Elapsed":0.86}
`

// A PLAN TASK'S OWN COMMANDS ARE THE CHECK ON ITS OWN NUMBERS.
//
// The parent's tripwire compares the parent's answer against the parent's tool
// output. A plan task's commands run in the CHILD's session, so a figure a task
// invents was caught by neither — which is the gap this closes. The child's tool
// results do stream to the parent, which is what makes the check possible.
func TestATaskReportingAFigureItsCommandsContradictIsFlagged(t *testing.T) {
	run := childRunner(t, taskSuiteOutput, "github.com/x/y took 4.20s, well within budget.")
	result, err := run(context.Background(), PlanTaskRequest{
		Task:  Task{ID: "bench", Prompt: "measure it"},
		Tools: []string{"read_file"},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(result.MeasurementConflicts) != 1 {
		t.Fatalf("got %d conflicts, want 1: %+v", len(result.MeasurementConflicts), result.MeasurementConflicts)
	}
	got := result.MeasurementConflicts[0]
	for _, required := range []string{"github.com/x/y", "4.2s", "0.86s"} {
		if !strings.Contains(got, required) {
			t.Errorf("the conflict does not mention %q: %s", required, got)
		}
	}
}

// AN HONEST TASK CARRIES NOTHING. A check that fires on a correct report is
// worse than no check: it gets ignored, and then it catches nothing.
func TestATaskReportingWhatItMeasuredIsNotFlagged(t *testing.T) {
	for name, answer := range map[string]string{
		"the figure as measured": "github.com/x/y took 0.86s.",
		"ordinary variation":     "github.com/x/y took 0.91s.",
		"no figure at all":       "github.com/x/y passes.",
		"a name never measured":  "TestSomethingElse took 99.0s.",
	} {
		t.Run(name, func(t *testing.T) {
			run := childRunner(t, taskSuiteOutput, answer)
			result, err := run(context.Background(), PlanTaskRequest{
				Task: Task{ID: "bench", Prompt: "measure it"}, Tools: []string{"read_file"},
			})
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if len(result.MeasurementConflicts) != 0 {
				t.Errorf("an honest report was flagged: %+v", result.MeasurementConflicts)
			}
		})
	}
}

// THE READER MUST SEE IT. A conflict recorded on a struct nobody renders is a
// conflict nobody acts on — and this is the one finding a reader cannot check
// for themselves, because the command ran inside a child's session.
func TestTheReportNamesAnUnverifiedFigure(t *testing.T) {
	report := PlanReport{
		Status: PlanCompleted, Succeeded: 1, TokensUsed: 10,
		Tasks: []TaskResult{{
			ID: "bench", Outcome: TaskSucceeded,
			MeasurementConflicts: []string{"github.com/x/y: reported 4.2s, but this task's own commands printed 0.86s"},
		}},
	}
	summary := report.Summary()
	for _, required := range []string{"unverified figure", "bench", "github.com/x/y", "4.2s", "0.86s"} {
		if !strings.Contains(summary, required) {
			t.Errorf("the summary does not carry %q:\n%s", required, summary)
		}
	}
	// ...and stays silent for an ordinary plan.
	clean := PlanReport{Status: PlanCompleted, Succeeded: 1, TokensUsed: 10,
		Tasks: []TaskResult{{ID: "bench", Outcome: TaskSucceeded}}}
	if strings.Contains(clean.Summary(), "unverified figure") {
		t.Errorf("a plan with no conflicts announced one:\n%s", clean.Summary())
	}
}
