package tui

import (
	"context"
	"testing"
	"time"
)

// A background plan outlives the run that launched it — that is the whole point
// of the flag, and every message it posts carries it so the stale-run guard lets
// it through. beginRun wiping the panel anyway leaves those messages passing the
// guard and then no-oping against an empty byID: the PLAN surface disappears for
// the rest of the plan's life while it keeps running.
func TestBeginRunKeepsTheOrchestratePanelForALiveBackgroundPlan(t *testing.T) {
	plan := samplePlan(t)

	for name, testCase := range map[string]struct {
		background bool
		wantKept   bool
	}{
		"background plan survives the next run": {background: true, wantKept: true},
		"foreground leftovers are cleared":      {background: false, wantKept: false},
	} {
		t.Run(name, func(t *testing.T) {
			m, _ := savedPlanModel(t)
			m.now = time.Now
			m.planProgress.SetBackground(testCase.background)
			m.planProgress.PlanAdmitted(plan)

			// Whatever the panel held when the next run began.
			m.orchestrate.admit(planAdmittedMsg{
				name:  plan.Name(),
				tasks: []planGraphTask{{id: "a"}, {id: "b"}},
			}, time.Now())
			if m.orchestrate.isEmpty() {
				t.Fatal("precondition: the panel should hold the admitted tasks")
			}

			m = m.beginRun(context.CancelFunc(func() {}))

			if kept := !m.orchestrate.isEmpty(); kept != testCase.wantKept {
				t.Errorf("panel kept = %v, want %v", kept, testCase.wantKept)
			}
		})
	}
}
