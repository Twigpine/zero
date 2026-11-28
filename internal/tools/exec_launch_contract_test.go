package tools

import (
	"testing"

	"github.com/Twigpine/zero/internal/execution"
)

// THE LAUNCH FACT HAS TO SURVIVE EVERY RESULT SHAPE, NOT JUST THE CAPTURED ONE.
//
// exec_command builds its outcome through its own conversion rather than through
// Runner.ExecuteCaptured, and that conversion used to assert `launched = true` on
// the grounds that a start failure returns earlier. That reasoning holds for the
// process the tool starts, and on Windows a wrapped plan starts the sandbox
// helper: the requested child is created inside it, after marker, ACL, network,
// SID and token setup, any of which can fail with the helper already running.
// Asserting the launch there tells the operator that reads were denied in
// exchange for the write jail when nothing ran under that enforcement.
//
// The retained shape is the one worth pinning hardest: exec_command can return a
// running session before the helper has even attempted the inner launch, so an
// absent report there means "not yet", not "yes".
func TestExecOutcomeTakesTheLaunchFactFromTheAdapter(t *testing.T) {
	yes, no := true, false

	cases := []struct {
		name    string
		owned   bool
		report  execution.AdapterReport
		exited  bool
		want    bool
		because string
	}{
		{
			name:  "wrapped helper failed before creating the child",
			owned: true, report: execution.AdapterReport{ChildLaunched: &no}, exited: true,
			want: false, because: "only the unsandboxed helper ran",
		},
		{
			name:  "wrapped plan, adapter said nothing",
			owned: true, report: execution.AdapterReport{}, exited: true,
			want: false, because: "an absent report is not proof that enforcement applied",
		},
		{
			name:  "wrapped plan still running, nothing reported yet",
			owned: true, report: execution.AdapterReport{}, exited: false,
			want: false, because: "the helper can be returned before it attempts the inner launch",
		},
		{
			name:  "wrapped plan, restricted child confirmed",
			owned: true, report: execution.AdapterReport{ChildLaunched: &yes}, exited: true,
			want: true, because: "the adapter saw the transition",
		},
		{
			name:  "direct command keeps its own observation",
			owned: false, report: execution.AdapterReport{}, exited: true,
			want: true, because: "the process the tool started is the requested one",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			input := execToolResultInput{
				commandText:               "echo hi",
				sessionID:                 7,
				exited:                    testCase.exited,
				report:                    testCase.report,
				childLaunchOwnedByAdapter: testCase.owned,
				enforcement:               execution.Enforcement{Notices: []string{"denyRead is configured, so the write jail is not confining writes"}},
			}
			// Through the PRODUCTION conversion, which is what decides the launch
			// state. Computing it here instead would pin the shared helper and prove
			// nothing about whether exec_command consults it.
			result := execToolResult(input)
			outcome := result.ExecutionOutcome
			if outcome == nil {
				t.Fatalf("SETUP INVALID: the conversion produced no execution outcome")
			}
			if outcome.Launched != testCase.want {
				t.Fatalf("Launched = %v, want %v: %s", outcome.Launched, testCase.want, testCase.because)
			}
			notices := outcome.AppliedEnforcementNotices()
			if testCase.want && len(notices) != 1 {
				t.Fatalf("a confirmed launch disclosed %q, want the planned notice exactly once", notices)
			}
			if !testCase.want && len(notices) != 0 {
				t.Fatalf("no requested child ran, but the outcome disclosed %q", notices)
			}
		})
	}
}

// And the shared resolution itself, since three launchers now depend on it
// answering the same way.
func TestResolveChildLaunchedIsOneAnswerForEveryLauncher(t *testing.T) {
	yes, no := true, false
	cases := []struct {
		name     string
		observed bool
		owned    bool
		report   execution.AdapterReport
		want     bool
	}{
		{"adapter confirms over a false observation", false, true, execution.AdapterReport{ChildLaunched: &yes}, true},
		{"adapter denies over a true observation", true, true, execution.AdapterReport{ChildLaunched: &no}, false},
		{"owned and silent fails closed", true, true, execution.AdapterReport{}, false},
		{"unowned keeps the observation, true", true, false, execution.AdapterReport{}, true},
		{"unowned keeps the observation, false", false, false, execution.AdapterReport{}, false},
		{"an adapter may speak even when unowned", false, false, execution.AdapterReport{ChildLaunched: &yes}, true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := execution.ResolveChildLaunched(testCase.observed, testCase.owned, testCase.report); got != testCase.want {
				t.Fatalf("ResolveChildLaunched(%v, %v, %+v) = %v, want %v",
					testCase.observed, testCase.owned, testCase.report, got, testCase.want)
			}
		})
	}
}
