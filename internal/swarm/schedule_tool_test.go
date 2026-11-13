package swarm

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Twigpine/zero/internal/tools"
)

func TestScheduleToolAddListCancel(t *testing.T) {
	reg, sw := newToolSwarm(t, newLauncher(okFor))
	ctx := context.Background()
	grant := tools.RunOptions{PermissionGranted: true, Model: "m1", Cwd: "/work"}

	// add (24h interval so the real timer never fires during the test)
	res := reg.RunWithOptions(ctx, ScheduleToolName, map[string]any{
		"agent_type": "teammate",
		"task":       "nightly sweep",
		"team":       "alpha",
		"every":      "24h",
		"max_runs":   float64(3),
	}, grant)
	if res.Status != tools.StatusOK {
		t.Fatalf("add status = %v, output=%q", res.Status, res.Output)
	}
	jobID := res.Meta["job_id"]
	if jobID == "" {
		t.Fatal("add must return a job_id in Meta")
	}

	// list shows it
	res = reg.RunWithOptions(ctx, ScheduleToolName, map[string]any{"action": "list"}, grant)
	if res.Status != tools.StatusOK || !strings.Contains(res.Output, jobID) {
		t.Fatalf("list missing job %q: status=%v output=%q", jobID, res.Status, res.Output)
	}

	// cancel removes it
	res = reg.RunWithOptions(ctx, ScheduleToolName, map[string]any{"action": "cancel", "job_id": jobID}, grant)
	if res.Status != tools.StatusOK {
		t.Fatalf("cancel status = %v, output=%q", res.Status, res.Output)
	}
	if got := len(sw.Scheduler().List()); got != 0 {
		t.Fatalf("after cancel, %d jobs remain, want 0", got)
	}

	// cancel again is an error
	res = reg.RunWithOptions(ctx, ScheduleToolName, map[string]any{"action": "cancel", "job_id": jobID}, grant)
	if res.Status != tools.StatusError {
		t.Fatalf("cancel of gone job should error, got %v", res.Status)
	}
}

func TestScheduleToolDailyAt(t *testing.T) {
	reg, sw := newToolSwarm(t, newLauncher(okFor))
	// Make the daily-time math deterministic by injecting a fixed clock into the
	// tool. The registry already holds a scheduleTool; replace it with one whose
	// clock is fixed so first_delay is computed against a known "now".
	st := &scheduleTool{sw: sw, now: func() time.Time { return time.Date(2026, 6, 15, 8, 0, 0, 0, time.UTC) }}
	reg.Register(st)

	res := reg.RunWithOptions(context.Background(), ScheduleToolName, map[string]any{
		"agent_type": "teammate",
		"task":       "report",
		"daily_at":   "23:30",
	}, tools.RunOptions{PermissionGranted: true})
	if res.Status != tools.StatusOK {
		t.Fatalf("daily_at add status = %v, output=%q", res.Status, res.Output)
	}
	jobs := sw.Scheduler().List()
	if len(jobs) != 1 {
		t.Fatalf("want 1 scheduled job, got %d", len(jobs))
	}
	if jobs[0].Every != 24*time.Hour {
		t.Fatalf("daily_at should schedule a 24h interval, got %s", jobs[0].Every)
	}
}

func TestScheduleToolValidation(t *testing.T) {
	reg, _ := newToolSwarm(t, newLauncher(okFor))
	ctx := context.Background()
	grant := tools.RunOptions{PermissionGranted: true}

	cases := []struct {
		name string
		args map[string]any
	}{
		{"missing agent_type", map[string]any{"task": "t", "every": "1h"}},
		{"missing task", map[string]any{"agent_type": "teammate", "every": "1h"}},
		{"neither every nor daily_at", map[string]any{"agent_type": "teammate", "task": "t"}},
		{"both every and daily_at", map[string]any{"agent_type": "teammate", "task": "t", "every": "1h", "daily_at": "10:00"}},
		{"invalid every", map[string]any{"agent_type": "teammate", "task": "t", "every": "nope"}},
		{"sub-second every", map[string]any{"agent_type": "teammate", "task": "t", "every": "500ms"}},
		{"unknown agent_type", map[string]any{"agent_type": "ghost", "task": "t", "every": "1h"}},
		{"invalid daily_at", map[string]any{"agent_type": "teammate", "task": "t", "daily_at": "99:99"}},
		{"unknown action", map[string]any{"action": "frobnicate"}},
		{"cancel without id", map[string]any{"action": "cancel"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := reg.RunWithOptions(ctx, ScheduleToolName, tc.args, grant)
			if res.Status != tools.StatusError {
				t.Fatalf("expected error, got status=%v output=%q", res.Status, res.Output)
			}
		})
	}
}

func TestScheduleToolRequiresPermission(t *testing.T) {
	reg, _ := newToolSwarm(t, newLauncher(okFor))
	// swarm_schedule is a prompt tool: without a grant the registry refuses it.
	res := reg.RunWithOptions(context.Background(), ScheduleToolName, map[string]any{
		"agent_type": "teammate", "task": "t", "every": "1h",
	}, tools.RunOptions{})
	if res.Status != tools.StatusError {
		t.Fatalf("schedule without permission should error, got %v", res.Status)
	}
}
