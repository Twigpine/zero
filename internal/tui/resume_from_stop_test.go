package tui

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/Twigpine/zero/internal/sessions"
	"github.com/Twigpine/zero/internal/specialist"
)

// resumeModelWithHistory is a model whose session log already holds a plan that
// ran, completed its first task WITH a finding, and was then cut short — a real
// bridge wrote every event, so the durable-output write (TaskCompletedEvent) is
// exercised here, not hand-built.
func resumeModelWithHistory(t *testing.T) (model, specialist.PlanPaths) {
	t.Helper()
	m, paths := savedPlanModel(t)
	store := sessions.NewStore(sessions.StoreOptions{RootDir: t.TempDir()})
	session, err := store.Create(sessions.CreateInput{Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	m.sessionStore = store
	m.activeSession = session
	m.planProgress.Attach(func(tea.Msg) {}, 1, store, session.SessionID)

	plan, err := specialist.ParsePlan(map[string]any{
		"name": "audit",
		"tasks": []any{
			map[string]any{"id": "find", "prompt": "find it"},
			map[string]any{"id": "synth", "prompt": "combine the findings", "depends_on": []any{"find"}},
		},
		"budget": map[string]any{"max_workers": float64(1)},
	}, specialist.Limits{MaxTasks: 20, ParentTools: specialist.PlanReadOnlyToolNames()})
	if err != nil {
		t.Fatalf("ParsePlan: %v", err)
	}

	// A real run through the real bridge: admit, dispatch find, complete find with
	// a finding, then stop — synth was never dispatched.
	m.planProgress.PlanAdmitted(plan)
	m.planProgress.TaskDispatched(specialist.Task{ID: "find"})
	m.planProgress.TaskCompleted(specialist.TaskResult{
		ID:      "find",
		Outcome: specialist.TaskSucceeded,
		Output:  "the parser handles a UTF-8 BOM (extract.go:41)",
	})
	if err := m.planProgress.RecordingError(); err != nil {
		t.Fatalf("recording: %v", err)
	}
	return m, paths
}

// BARE "/plans resume" CONTINUES A CANCELLED PLAN FROM WHERE IT STOPPED, briefing
// the remaining task on what its completed dependency found. This is the whole
// point of the #2+#3 pair: the completed task's finding survives the interruption
// and reaches the task that was meant to build on it.
//
// Driven through the real command (handlePlansCommand), not resumeLastPlan
// directly, so the arity/running routing is exercised too. cmd is deliberately
// NOT asserted: launchPrompt returns a nil cmd without a provider (this harness
// has none), exactly as the restart test documents — the staged remainder and
// the notice are what resume is responsible for.
func TestBareResumeContinuesACancelledPlanFromItsStop(t *testing.T) {
	m, paths := resumeModelWithHistory(t)

	updated, _ := m.handlePlansCommand("resume")
	notice := transcriptText(updated.(model).transcript)
	if !strings.Contains(notice, "from where it stopped") || !strings.Contains(notice, "1 task") {
		t.Fatalf("the notice does not describe a resume-from-stop:\n%s", notice)
	}

	// The staged remainder must be exactly synth, briefed on find's finding. If
	// the bare-resume fall-through were reverted to the un-pause control, nothing
	// would be staged and this fails — which is the mutation this test guards.
	staged, err := specialist.FindSavedPlan(paths, "last_run_resume")
	if err != nil {
		t.Fatalf("the remainder was not staged: %v", err)
	}
	plan, err := specialist.ParsePlan(staged.Args, m.savedPlanLimits())
	if err != nil {
		t.Fatalf("the staged remainder does not validate: %v", err)
	}
	if plan.TaskCount() != 1 || plan.Order()[0] != "synth" {
		t.Fatalf("staged %d task(s), want only synth: %v", plan.TaskCount(), plan.Order())
	}
	if !strings.Contains(plan.Tasks()[0].Prompt, "UTF-8 BOM") {
		t.Fatalf("the resumed task was not briefed on find's finding:\n%s", plan.Tasks()[0].Prompt)
	}
}

// With NOTHING ever run, bare resume says so plainly and starts no turn — it does
// not fall through to the un-pause control and appear to work.
func TestBareResumeWithNothingToResumeSaysSo(t *testing.T) {
	m, _ := savedPlanModel(t)
	updated, cmd := m.handlePlansCommand("resume")
	if cmd != nil {
		t.Fatal("bare resume started a turn with no plan")
	}
	text := transcriptText(updated.(model).transcript)
	if !strings.Contains(text, "nothing to resume") {
		t.Fatalf("expected a nothing-to-resume notice:\n%s", text)
	}
}

// C5: the SAVED-plan resume path names a completed task that was EDITED since it
// ran, so the user understands why a task they believe finished is back. Driven
// through the real resumeSavedPlan: a plan runs, its saved file is (in effect)
// edited — modelled by a completion whose recorded fingerprint no longer matches
// the current task — and resume reports it changed.
func TestResumeSavedPlanNamesAnEditedTaskInItsNotice(t *testing.T) {
	m, paths := savedPlanModel(t)
	store := sessions.NewStore(sessions.StoreOptions{RootDir: t.TempDir()})
	session, err := store.Create(sessions.CreateInput{Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	m.sessionStore = store
	m.activeSession = session
	m.planProgress.Attach(func(tea.Msg) {}, 1, store, session.SessionID)

	plan, err := specialist.ParsePlan(map[string]any{
		"name":   "sweep",
		"tasks":  []any{map[string]any{"id": "scan", "prompt": "scan it"}},
		"budget": map[string]any{"max_workers": float64(1)},
	}, m.savedPlanLimits())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := specialist.SavePlan(paths.ProjectRoot, paths.ProjectDir, "sweep", plan); err != nil {
		t.Fatal(err)
	}

	record := func(build func() (sessions.EventType, map[string]any)) {
		typ, payload := build()
		if _, err := store.AppendEvent(session.SessionID, sessions.AppendEventInput{Type: typ, Payload: payload}); err != nil {
			t.Fatalf("append %s: %v", typ, err)
		}
	}
	record(func() (sessions.EventType, map[string]any) { return specialist.PlanAdmittedEvent(plan) })
	// A recorded fingerprint that cannot match the current task's — as if the
	// saved file was edited after the run.
	record(func() (sessions.EventType, map[string]any) {
		return specialist.TaskCompletedEvent(specialist.TaskResult{
			ID: "scan", Outcome: specialist.TaskSucceeded, Output: "found", Identity: "stale-fingerprint"})
	})

	stored, err := specialist.FindSavedPlan(paths, "sweep")
	if err != nil {
		t.Fatal(err)
	}
	_, notice, ok := m.resumeSavedPlan(stored)
	if !ok {
		t.Fatalf("resume refused: %s", notice)
	}
	if !strings.Contains(notice, "scan changed since the last run") {
		t.Fatalf("the notice did not flag the edited task as re-running:\n%s", notice)
	}
}

// A plan admitted in a session with no event log (its store never came up) cannot
// be resumed from stop — there is no record of what ran. Resume must refuse rather
// than silently restart from the top.
func TestBareResumeWithoutAnEventLogRefuses(t *testing.T) {
	m, _ := savedPlanModel(t) // attached to a nil store: LastPlan is set, nothing is recorded
	m.planProgress.PlanAdmitted(samplePlan(t))
	updated, cmd := m.handlePlansCommand("resume")
	if cmd != nil {
		t.Fatal("resume with no event log started a turn")
	}
	if !strings.Contains(transcriptText(updated.(model).transcript), "no event log") {
		t.Fatalf("expected a no-event-log refusal:\n%s", transcriptText(updated.(model).transcript))
	}
}

// Exercise the executor, durable bridge, reducer and saved command together.
// A terminal event closes stopped runs too; it does not prove graph exhaustion.
func TestSavedResumeReportsTheRecordedTerminalStatus(t *testing.T) {
	for _, scenario := range []string{"cancelled", "stopped-partial", "failed-partial"} {
		t.Run(scenario, func(t *testing.T) {
			m, paths := savedPlanModel(t)
			store := sessions.NewStore(sessions.StoreOptions{RootDir: t.TempDir()})
			session, err := store.Create(sessions.CreateInput{Cwd: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			m.sessionStore, m.activeSession = store, session
			m.planProgress.Attach(func(tea.Msg) {}, 1, store, session.SessionID)
			plan := samplePlan(t)
			if _, err := specialist.SavePlan(paths.ProjectRoot, paths.ProjectDir, "audit", plan); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			m.planProgress.PlanRunning(cancel)
			m.planProgress.PlanAdmitted(plan)
			if scenario == "cancelled" {
				m.orchestrateControlText("stop")
			}
			report := specialist.ExecutePlanIn(ctx, plan, specialist.PlanWorkspace{}, []string{"read_file"},
				func(runCtx context.Context, req specialist.PlanTaskRequest) (specialist.TaskResult, error) {
					if req.Task.ID == "a" {
						return specialist.TaskResult{Outcome: specialist.TaskSucceeded, Output: "dependency finding"}, nil
					}
					if scenario == "stopped-partial" {
						m.orchestrateControlText("stop")
						return specialist.TaskResult{Outcome: specialist.TaskFailed, Err: "context canceled"}, runCtx.Err()
					}
					return specialist.TaskResult{Outcome: specialist.TaskFailed, Err: "task failed"}, nil
				}, m.planProgress)
			m.planProgress.PlanCompleted(plan, report)
			wantStatus := specialist.PlanPartial
			wantLeft := 1
			if scenario == "cancelled" {
				wantStatus, wantLeft = specialist.PlanCancelled, 2
			}
			if report.Status != wantStatus {
				t.Fatalf("status = %q, want %q", report.Status, wantStatus)
			}
			if scenario == "stopped-partial" && report.Cancelled != 1 {
				t.Fatalf("cancelled = %d, want 1", report.Cancelled)
			}
			if scenario == "failed-partial" && report.Failed != 1 {
				t.Fatalf("failed = %d, want 1", report.Failed)
			}
			updated, _ := m.handlePlansCommand("resume audit")
			notice := transcriptText(updated.(model).transcript)
			if !strings.Contains(notice, "ended with status \""+string(wantStatus)+"\"") || strings.Contains(notice, "ran to the end") {
				t.Fatalf("resume misreported the prior run:\n%s", notice)
			}
			staged, err := specialist.FindSavedPlan(paths, "audit_resume")
			if err != nil {
				t.Fatal(err)
			}
			remaining, err := specialist.ParsePlan(staged.Args, m.savedPlanLimits())
			if err != nil {
				t.Fatal(err)
			}
			if remaining.TaskCount() != wantLeft {
				t.Fatalf("remaining = %d, want %d", remaining.TaskCount(), wantLeft)
			}
			if wantLeft == 1 && !strings.Contains(remaining.Tasks()[0].Prompt, "dependency finding") {
				t.Fatal("resume lost the succeeded dependency's finding")
			}
		})
	}
}
