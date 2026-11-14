package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// "RUN CANCELLED." MUST NOT SIT BESIDE AGENTS THAT LOOK ALIVE. Cancelling the
// run kills the children with the run context, but the trackers were never
// told: a specialist row stayed specialistRunning — spinner on, clock ticking,
// "live" in MODELS — and an orchestrate task stayed orchestrateRunning in the
// PLAN bar, indefinitely, over processes that no longer existed. A real session
// showed exactly that: the cancellation marker in the transcript, and the
// sidebar still "working" a minute later.
func TestCancelSettlesEveryRunningAgentAndTask(t *testing.T) {
	start := time.Unix(1000, 0)
	m := sidebarTestModel()
	m.pending = true
	m.now = func() time.Time { return start }

	m.specialists.start("audit", "Audit specialist", "sess-1", start)
	m.specialists.setModel("sess-1", "grok-4.5")
	m.specialists.setCurrentTool("sess-1", "read_file", "internal/tui/model.go")
	m.specialists.start("done-before", "finished earlier", "sess-2", start)
	m.specialists.complete("sess-2", specialistCompleted, 0, "", start)

	m.orchestrate.tasks = []orchestrateTask{
		{id: "a", status: orchestrateRunning, startedAt: start},
		{id: "b", status: orchestrateDone},
	}

	m.cancelRun()

	for _, info := range m.specialists.specialists {
		if info.childSessionID == "sess-1" {
			if info.status != specialistCancelled {
				t.Fatalf("the running specialist stayed %v after cancel", info.status)
			}
			if info.currentTool != "" {
				t.Fatalf("a cancelled specialist still claims to run %q", info.currentTool)
			}
			if info.completedAt.IsZero() {
				t.Fatal("a cancelled specialist has no end time — its clock would tick forever")
			}
		}
		if info.childSessionID == "sess-2" && info.status != specialistCompleted {
			t.Fatalf("cancel rewrote a specialist that had already finished: %v", info.status)
		}
	}

	done, failed, _, cancelled, running := m.orchestrate.counts()
	if running != 0 {
		t.Fatalf("%d orchestrate task(s) still running after cancel", running)
	}
	if cancelled != 1 || done != 1 || failed != 0 {
		t.Fatalf("counts after cancel: done=%d failed=%d cancelled=%d, want 1/0/1", done, failed, cancelled)
	}

	// And the MODELS section agrees: nothing is "live" any more.
	for _, entry := range modelMixEntries(m.sidebarSpecialists(), "glm-5.2") {
		if entry.working > 0 {
			t.Fatalf("MODELS still counts %d live agent(s) on %s after cancel", entry.working, entry.model)
		}
	}
	if joined := strings.Join(m.renderContextSidebar(44, 40), "\n"); strings.Contains(ansi.Strip(joined), "live") {
		t.Fatalf("the sidebar still says live after cancel:\n%s", ansi.Strip(joined))
	}
}

// A BACKGROUND PLAN OUTLIVES THE RUN THAT LAUNCHED IT, BY DESIGN — beginRun
// makes exactly this exemption a few lines above the settle. Marking its tasks
// cancelled when the user stops the foreground turn was the mirror image of the
// defect the background-status tests exist to prevent: work still spending
// tokens and writing files, reported to the user as stopped.
func TestCancelLeavesALiveBackgroundPlanAlone(t *testing.T) {
	start := time.Unix(1000, 0)
	m := sidebarTestModel()
	m.pending = true
	m.now = func() time.Time { return start }
	m.specialists.start("bg", "background worker", "sess-bg", start)
	// MARKED, as the production path does: a background plan's task-start
	// message carries the flag, and a background Task spawn is marked on its
	// rebind. Without the mark a row is foreground by definition, and the
	// tracker holds both kinds together — see cancelRunning.
	m.specialists.markBackground("sess-bg")
	m.orchestrate.tasks = []orchestrateTask{{id: "a", status: orchestrateRunning, startedAt: start}}

	// A live background plan: the same condition beginRun consults.
	m.planProgress = NewPlanProgressBridge()
	m.planProgress.SetBackground(true)
	// PlanRunning is what a launched plan calls with its cancel func; together
	// with the background flag that is exactly what BackgroundPlanLive reads.
	m.planProgress.PlanRunning(func() {})
	if !m.planProgress.BackgroundPlanLive() {
		t.Fatal("setup: the background plan must look live for this test to mean anything")
	}

	m.cancelRun()

	if _, _, _, _, running := m.orchestrate.counts(); running != 1 {
		t.Fatal("a live background plan's task was marked cancelled; it is still running")
	}
	for _, info := range m.specialists.specialists {
		if info.childSessionID == "sess-bg" && info.status != specialistRunning {
			t.Fatalf("a background sub-agent was marked %v while still working", info.status)
		}
	}
}

// THE MIXED CASE, which neither of the two tests above covers and which both
// reviewers reproduced independently.
//
// The specialist tracker holds foreground and background children TOGETHER, so
// gating the whole settle on BackgroundPlanLive left a FOREGROUND sub-agent
// spinning forever whenever any background plan happened to be live: still
// specialistRunning, still showing its current tool, still counted live in
// MODELS — over a process that died with the run context. Nothing else settles
// it, because a late completion is dropped by the stale-run guard once
// cancelRun has zeroed activeRunID.
func TestCancelSettlesForegroundChildrenEvenWithALiveBackgroundPlan(t *testing.T) {
	start := time.Unix(1000, 0)
	m := sidebarTestModel()
	m.pending = true
	m.now = func() time.Time { return start }

	// A foreground sub-agent: dies with the run context, must be settled.
	m.specialists.start("fg", "foreground worker", "sess-fg", start)
	m.specialists.setCurrentTool("sess-fg", "read_file", "internal/tui/model.go")
	// A background one: outlives the run, must be left alone.
	m.specialists.start("bg", "background worker", "sess-bg", start)
	m.specialists.markBackground("sess-bg")

	m.orchestrate.tasks = []orchestrateTask{{id: "a", status: orchestrateRunning, startedAt: start}}
	m.planProgress = NewPlanProgressBridge()
	m.planProgress.SetBackground(true)
	m.planProgress.PlanRunning(func() {})
	if !m.planProgress.BackgroundPlanLive() {
		t.Fatal("setup: the background plan must look live")
	}

	m.cancelRun()

	for _, info := range m.specialists.specialists {
		switch info.childSessionID {
		case "sess-fg":
			if info.status != specialistCancelled {
				t.Fatalf("the FOREGROUND child stayed %v; its process died with the run context "+
					"and nothing else will settle it", info.status)
			}
			if info.currentTool != "" {
				t.Fatalf("a settled child still claims to run %q", info.currentTool)
			}
		case "sess-bg":
			if info.status != specialistRunning {
				t.Fatalf("the BACKGROUND child was marked %v while still working", info.status)
			}
		}
	}
	// The background plan's own task is still untouched.
	if _, _, _, _, running := m.orchestrate.counts(); running != 1 {
		t.Fatal("a live background plan's task was cancelled; it is still running")
	}
	// And MODELS must not count the settled foreground child as live.
	for _, entry := range modelMixEntries(m.sidebarSpecialists(), "glm-5.2") {
		if entry.working > 1 {
			t.Fatalf("MODELS counts %d live agents; only the background one is still working", entry.working)
		}
	}
}
