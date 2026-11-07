package specialist

import (
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/tools"
)

// sizedPlanArgs builds a plan with n trivial tasks, for exercising the ceiling.
func sizedPlanArgs(n int) map[string]any {
	tasks := make([]any, 0, n)
	for i := 0; i < n; i++ {
		tasks = append(tasks, map[string]any{
			"id":     "t" + string(rune('a'+i%26)) + strings.Repeat("x", i/26),
			"prompt": "look at something",
		})
	}
	return map[string]any{
		"tasks":  tasks,
		"budget": map[string]any{"max_workers": float64(1)},
	}
}

// THE WIRE. A tier configured on the tool must reach the ceiling ParsePlan
// enforces — the class of defect this feature keeps producing is a field that is
// declared, documented and never populated at the production site.
func TestTheConfiguredTierIsTheCeilingTheToolEnforces(t *testing.T) {
	for _, tc := range []struct {
		size  config.PlanSize
		admit int
		block int
	}{
		{config.PlanSizeSmall, 5, 6},
		{config.PlanSizeMedium, 20, 21},
		{config.PlanSizeLarge, 50, 51},
	} {
		tool := &OrchestrateTool{Size: tc.size}
		limits := tool.limits(tools.RunOptions{})
		limits.ParentTools = []string{"read_file"}
		if _, err := ParsePlan(sizedPlanArgs(tc.admit), limits); err != nil {
			t.Errorf("%s: a %d-task plan was rejected: %v", tc.size, tc.admit, err)
		}
		if _, err := ParsePlan(sizedPlanArgs(tc.block), limits); err == nil {
			t.Errorf("%s: a %d-task plan was admitted; the ceiling is %d", tc.size, tc.block, tc.admit)
		}
	}
}

// "unrestricted" means no ceiling, and it has to be provable — a tier whose
// number is 0 would silently become "reject everything" if the guard were
// written as >= instead of >.
func TestTheUnrestrictedTierHasNoCeiling(t *testing.T) {
	tool := &OrchestrateTool{Size: config.PlanSizeUnrestricted}
	limits := tool.limits(tools.RunOptions{})
	limits.ParentTools = []string{"read_file"}
	if limits.MaxTasks != 0 {
		t.Fatalf("MaxTasks = %d; unrestricted must carry no ceiling", limits.MaxTasks)
	}
	if _, err := ParsePlan(sizedPlanArgs(120), limits); err != nil {
		t.Fatalf("a 120-task plan was rejected under the unrestricted tier: %v", err)
	}
}

// FAIL CLOSED at the tool boundary too. An unrecognised tier — a typo that
// reached the tool despite the config merge dropping it — must land on the
// default ceiling, never on no ceiling.
func TestAnUnknownTierOnTheToolFallsBackToTheDefaultCeiling(t *testing.T) {
	tool := &OrchestrateTool{Size: config.PlanSize("enormous")}
	limits := tool.limits(tools.RunOptions{})
	if limits.MaxTasks != config.DefaultPlanSize.MaxTasks() {
		t.Fatalf("MaxTasks = %d; want the default %d", limits.MaxTasks, config.DefaultPlanSize.MaxTasks())
	}
	if !strings.Contains(limits.MaxTasksSource, string(config.DefaultPlanSize)) {
		t.Fatalf("MaxTasksSource = %q; it must name the tier actually in force", limits.MaxTasksSource)
	}
}

// A tool that never had a tier wired keeps the ceiling it had before the tier
// existed. This is the additive half: nothing changes for a caller that does
// not opt in.
func TestAnUnwiredTierKeepsTheOldCeiling(t *testing.T) {
	tool := &OrchestrateTool{}
	if got := tool.limits(tools.RunOptions{}).MaxTasks; got != 20 {
		t.Fatalf("MaxTasks = %d; an unwired tier must keep the previous ceiling of 20", got)
	}
}

// The rejection has to be ACTIONABLE. The old message was a number with no
// origin and no remedy, so the only way to act on it was to read the source.
func TestTheTooLargeRejectionNamesTheTierAndTheRemedy(t *testing.T) {
	tool := &OrchestrateTool{Size: config.PlanSizeSmall}
	limits := tool.limits(tools.RunOptions{})
	limits.ParentTools = []string{"read_file"}
	_, err := ParsePlan(sizedPlanArgs(6), limits)
	if err == nil {
		t.Fatal("a 6-task plan must be rejected under the small tier")
	}
	for _, want := range []string{`"small" plan size`, "planSize", ".zero/config.json", "6", "5"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("rejection %q does not mention %q", err.Error(), want)
		}
	}
}

// Without a source label the message still renders — the generic form is what a
// test or an internal caller constructing Limits by hand gets, and it must not
// contain a dangling "set by ".
func TestTheTooLargeRejectionWithoutASourceIsStillWellFormed(t *testing.T) {
	err := planTooLargeError(9, Limits{MaxTasks: 4})
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "set by") {
		t.Fatalf("generic rejection %q leaked an empty source clause", err.Error())
	}
	if !strings.Contains(err.Error(), "9") || !strings.Contains(err.Error(), "4") {
		t.Fatalf("generic rejection %q lost the counts", err.Error())
	}
}
