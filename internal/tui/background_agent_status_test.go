package tui

import "testing"

// A BACKGROUND SUB-AGENT THAT WAS ONLY JUST LAUNCHED IS NOT A FINISHED ONE.
//
// THE MEASURED RUN. Four workers were spawned with run_in_background, and the
// TUI rendered each as "✓ completed · 0 tool calls · 1s" with the header
// reading "4 finished" — while all four were still running. The session log has
// four specialist_start events with mode=background and NOT ONE
// specialist_stop.
//
// THE MECHANISM. model.go completed specialist tracking whenever the Task tool
// returned, and a background Task returns the instant the child is launched. So
// "the tool returned" was read as "the sub-agent is done".
//
// Same invariant as "never report failure as success": work that has not
// happened must not be shown as work that has.
func TestABackgroundStatusOnlyCompletesWhenItReallyHas(t *testing.T) {
	for _, tc := range []struct {
		raw    string
		want   specialistStatus
		isDone bool
	}{
		{"completed", specialistCompleted, true},
		{"error", specialistError, true},
		{"killed", specialistCancelled, true},
		// The case this exists for: polling an agent that is still working must
		// leave it running.
		{"running", specialistRunning, false},
		// Fail-safe: anything unrecognised keeps showing the work as in flight
		// rather than declaring it done.
		{"", specialistRunning, false},
		{"queued", specialistRunning, false},
		{"COMPLETED", specialistRunning, false},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			status, done := backgroundAgentStatus(tc.raw)
			if done != tc.isDone {
				t.Fatalf("backgroundAgentStatus(%q) terminal=%v, want %v", tc.raw, done, tc.isDone)
			}
			if status != tc.want {
				t.Fatalf("backgroundAgentStatus(%q) = %v, want %v", tc.raw, status, tc.want)
			}
		})
	}
}

// A killed background agent is NOT red. Cancelled and skipped are deliberately
// not failures elsewhere in this UI, and a member the user stopped is the same
// kind of thing.
func TestAKilledBackgroundAgentIsNotShownAsAnError(t *testing.T) {
	status, done := backgroundAgentStatus("killed")
	if !done {
		t.Fatal("a killed agent is terminal")
	}
	if status == specialistError {
		t.Fatal("a killed agent is drawn as a defect; cancelled is not a failure in this UI")
	}
}

// THE RULE ITSELF. A background spawn must not finish its agent; a foreground
// one must.
//
// SCOPE, stated honestly: this covers the predicate, not the call site inside
// runAgentWithOptions — that function runs a full agent loop and no unit test
// drives it. Naming the rule is what makes the call site a single visible call
// rather than an inline condition that can be widened back unnoticed.
func TestOnlyAForegroundTaskFinishesItsSpecialist(t *testing.T) {
	for _, tc := range []struct {
		name string
		tool string
		meta map[string]string
		want bool
	}{
		{"a foreground Task finishes it", "Task", map[string]string{"session_id": "s"}, true},
		{"a background Task does not", "Task", map[string]string{"session_id": "s", "background": "true"}, false},
		{"nil meta is foreground", "Task", nil, true},
		{"another tool never finishes a specialist", "TaskOutput", map[string]string{"background": "true"}, false},
		{"read_file never does", "read_file", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := taskResultFinishesSpecialist(tc.tool, tc.meta); got != tc.want {
				t.Fatalf("taskResultFinishesSpecialist(%q, %v) = %v, want %v", tc.tool, tc.meta, got, tc.want)
			}
		})
	}
}
