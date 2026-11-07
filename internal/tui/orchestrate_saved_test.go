package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/Twigpine/zero/internal/sessions"
	"github.com/Twigpine/zero/internal/specialist"
)

func savedPlanModel(t *testing.T) (model, specialist.PlanPaths) {
	t.Helper()
	root := t.TempDir()
	userRoot := t.TempDir()
	paths := specialist.PlanPaths{
		ProjectRoot: root,
		ProjectDir:  filepath.Join(root, ".zero", "plans"),
		UserRoot:    userRoot,
		UserDir:     filepath.Join(userRoot, "zero", "plans"),
	}
	m := model{planProgress: NewPlanProgressBridge(), planPaths: paths, planStaging: newPlanStaging()}
	m.planProgress.Attach(func(tea.Msg) {}, 1, nil, "")
	return m, paths
}

// SAVE-FROM-RUN. The panel holds a rendering of a plan; the bridge holds the
// plan. Saving from the panel's copy would write something that merely
// resembles what ran.
func TestSavingKeepsThePlanThatActuallyRan(t *testing.T) {
	m, paths := savedPlanModel(t)
	m.planProgress.PlanAdmitted(samplePlan(t))

	text := m.savePlanText("sweep")
	if !strings.Contains(text, "status: info") {
		t.Fatalf("save failed: %s", text)
	}
	stored, err := specialist.FindSavedPlan(paths, "sweep")
	if err != nil {
		t.Fatalf("the saved plan is not findable: %v", err)
	}
	if stored.TaskCount != samplePlan(t).TaskCount() {
		t.Fatalf("saved %d tasks, the plan had %d", stored.TaskCount, samplePlan(t).TaskCount())
	}
	// It must be RUNNABLE, not merely present: re-admitted through the one
	// constructor, exactly as the tool will do.
	if _, err := specialist.ParsePlan(stored.Args, specialist.Limits{
		MaxTasks: 20, ParentTools: specialist.PlanReadOnlyToolNames()}); err != nil {
		t.Fatalf("the saved plan does not re-admit: %v", err)
	}
}

// Saving with nothing to save must refuse. An empty file named after a plan the
// user believes they kept is worse than a refusal.
func TestSavingWithNoPlanRefuses(t *testing.T) {
	m, paths := savedPlanModel(t)
	text := m.savePlanText("sweep")
	if !strings.Contains(text, "status: warning") || !strings.Contains(text, "nothing to save") {
		t.Fatalf("save must refuse with a reason: %s", text)
	}
	// The bundled plans are always present; nothing must have been WRITTEN.
	for _, plan := range mustLoad(t, paths) {
		if plan.Scope != specialist.PlanScopeBuiltin {
			t.Fatalf("a file was written anyway: %+v", plan)
		}
	}
}

func TestSavingRequiresAName(t *testing.T) {
	m, _ := savedPlanModel(t)
	m.planProgress.PlanAdmitted(samplePlan(t))
	if text := m.savePlanText("  "); !strings.Contains(text, "/plans save <name>") {
		t.Fatalf("an unnamed save must say how: %s", text)
	}
}

func TestSavingFallsBackToTheUserPlanRoot(t *testing.T) {
	m, paths := savedPlanModel(t)
	m.planPaths.ProjectRoot = ""
	m.planPaths.ProjectDir = ""
	m.planProgress.PlanAdmitted(samplePlan(t))

	text := m.savePlanText("personal")
	if !strings.Contains(text, "status: info") {
		t.Fatalf("user-scope fallback failed: %s", text)
	}
	paths.ProjectRoot = ""
	paths.ProjectDir = ""
	stored, err := specialist.FindSavedPlan(paths, "personal")
	if err != nil {
		t.Fatalf("user-scope plan was not saved under the user root: %v", err)
	}
	if stored.Scope != specialist.PlanScopeUser {
		t.Fatalf("saved scope = %q, want user", stored.Scope)
	}
}

// An invalid name is refused by the STORE, and the surface reports it rather
// than swallowing it.
func TestSavingAnInvalidNameIsReported(t *testing.T) {
	m, _ := savedPlanModel(t)
	m.planProgress.PlanAdmitted(samplePlan(t))
	text := m.savePlanText("../escape")
	if !strings.Contains(text, "status: warning") {
		t.Fatalf("a traversal name must be refused: %s", text)
	}
}

func TestListingShowsScopeAndUnreadableFiles(t *testing.T) {
	m, paths := savedPlanModel(t)
	m.planProgress.PlanAdmitted(samplePlan(t))
	if text := m.savePlanText("sweep"); !strings.Contains(text, "status: info") {
		t.Fatal(text)
	}
	if err := os.WriteFile(filepath.Join(paths.ProjectDir, "broken.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	text := m.savedPlansText()
	if !strings.Contains(text, "sweep") || !strings.Contains(text, "project") {
		t.Fatalf("the listing must name the plan and its scope: %s", text)
	}
	if !strings.Contains(text, "could not be read") || !strings.Contains(text, "broken.json") {
		t.Fatalf("an unreadable file must be named, not hidden: %s", text)
	}
}

func TestListingWithNothingSavedSaysHowToSave(t *testing.T) {
	m, _ := savedPlanModel(t)
	// The bundled example is listed, but it is not the user's — the listing must
	// still say how to save one, or it reads as though they already have some.
	text := m.savedPlansText()
	if !strings.Contains(text, "Nothing saved yet") || !strings.Contains(text, "/plans save") {
		t.Fatalf("a listing with only bundled plans must say how to fill it: %s", text)
	}
	if !strings.Contains(text, "builtin") {
		t.Fatalf("the bundled plan must be listed and labelled: %s", text)
	}
}

// SHOW renders through ParsePlan, so what is displayed is what would run —
// including the execution order, which a stored task list does not carry.
func TestShowRendersTheExecutionOrder(t *testing.T) {
	m, _ := savedPlanModel(t)
	m.planProgress.PlanAdmitted(samplePlan(t))
	if text := m.savePlanText("sweep"); !strings.Contains(text, "status: info") {
		t.Fatal(text)
	}

	text := m.showSavedPlanText("sweep")
	if !strings.Contains(text, "sweep") {
		t.Fatalf("show did not name the plan: %s", text)
	}
	for _, task := range samplePlan(t).Tasks() {
		if !strings.Contains(text, task.ID) {
			t.Fatalf("show omitted task %q: %s", task.ID, text)
		}
	}
}

func TestShowAndRunRefuseAnUnknownName(t *testing.T) {
	m, _ := savedPlanModel(t)
	if text := m.showSavedPlanText("nope"); !strings.Contains(text, "no saved plan named") {
		t.Fatalf("show must refuse by name: %s", text)
	}
	updated, cmd := m.runSavedPlan("nope", false)
	if cmd != nil {
		t.Fatal("running an unknown plan started a turn")
	}
	rendered := transcriptText(updated.(model).transcript)
	if !strings.Contains(rendered, "no saved plan named") {
		t.Fatalf("run must refuse by name: %s", rendered)
	}
}

// A hand-edited plan file that is VALID JSON but not a valid plan must be
// refused on the way back in, naming the file. This is the re-admit branch that
// actually fires — the one on the save side cannot, and says so.
func TestAHandEditedPlanIsRefusedOnTheWayBackIn(t *testing.T) {
	m, paths := savedPlanModel(t)
	if err := os.MkdirAll(paths.ProjectDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Parseable JSON, impossible plan: a dependency on a task that is not there.
	body := `{"name":"edited","tasks":[{"id":"a","prompt":"x","depends_on":["ghost"]}],"budget":{"max_workers":1}}`
	if err := os.WriteFile(filepath.Join(paths.ProjectDir, "edited.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	text := m.showSavedPlanText("edited")
	if !strings.Contains(text, "status: warning") || !strings.Contains(text, "does not validate") {
		t.Fatalf("an invalid stored plan must be refused: %s", text)
	}
	if !strings.Contains(text, "edited.json") {
		t.Fatalf("the refusal must name the file: %s", text)
	}
	if !strings.Contains(text, "ghost") {
		t.Fatalf("the refusal must say what is wrong with it: %s", text)
	}
}

// resumeModel is a model with a real session store, a real saved plan, and real
// plan events — the whole chain from "a plan ran and died" to "resume it".
func resumeModel(t *testing.T) (model, specialist.PlanPaths, specialist.Plan) {
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

	plan := samplePlan(t)
	if _, err := specialist.SavePlan(paths.ProjectRoot, paths.ProjectDir, "sweep", plan); err != nil {
		t.Fatalf("SavePlan: %v", err)
	}
	return m, paths, plan
}

// THE WHOLE CHAIN. A plan runs, one task finishes, the process dies mid-second
// task, and resuming stages exactly the work that was not done — using only the
// session log and the saved plan, with no third store between them.
func TestResumeStagesOnlyTheWorkThatDidNotFinish(t *testing.T) {
	m, paths, plan := resumeModel(t)
	order := plan.Order()
	if len(order) < 2 {
		t.Skip("the sample plan is too small to interrupt")
	}

	// A real run, recorded through the real bridge: first task done, second
	// dispatched and never finished.
	m.planProgress.PlanAdmitted(plan)
	m.planProgress.TaskDispatched(specialist.Task{ID: order[0]})
	m.planProgress.TaskCompleted(specialist.TaskResult{ID: order[0], Attempts: 1})
	m.planProgress.TaskDispatched(specialist.Task{ID: order[1]})
	if err := m.planProgress.RecordingError(); err != nil {
		t.Fatalf("recording: %v", err)
	}

	stored, err := specialist.FindSavedPlan(paths, "sweep")
	if err != nil {
		t.Fatal(err)
	}
	name, notice, ok := m.resumeSavedPlan(stored)
	if !ok {
		t.Fatalf("resume refused: %s", notice)
	}
	if name != "sweep_resume" {
		t.Fatalf("staged as %q", name)
	}

	remaining, err := specialist.FindSavedPlan(paths, name)
	if err != nil {
		t.Fatalf("the staged remainder is not findable: %v", err)
	}
	restored, err := specialist.ParsePlan(remaining.Args, m.savedPlanLimits())
	if err != nil {
		t.Fatalf("the staged remainder does not validate: %v", err)
	}
	for _, id := range restored.Order() {
		if id == order[0] {
			t.Fatalf("the remainder re-runs %q, which already succeeded", id)
		}
	}
	// The INTERRUPTED task is in the remainder: dispatched with no terminal
	// event means unfinished, never done.
	var sawInterrupted bool
	for _, id := range restored.Order() {
		if id == order[1] {
			sawInterrupted = true
		}
	}
	if !sawInterrupted {
		t.Fatalf("the interrupted task %q was dropped from the remainder", order[1])
	}
	if !strings.Contains(notice, "1 of") {
		t.Fatalf("the notice must say how much was already done: %s", notice)
	}
}

// Resuming a plan that finished is not an error to hide — it is the ordinary
// end of a plan, said plainly, with nothing staged.
func TestResumingAFinishedPlanSaysThereIsNothingLeft(t *testing.T) {
	m, paths, plan := resumeModel(t)
	m.planProgress.PlanAdmitted(plan)
	for _, id := range plan.Order() {
		m.planProgress.TaskDispatched(specialist.Task{ID: id})
		m.planProgress.TaskCompleted(specialist.TaskResult{ID: id, Attempts: 1})
	}

	stored, _ := specialist.FindSavedPlan(paths, "sweep")
	_, notice, ok := m.resumeSavedPlan(stored)
	if ok {
		t.Fatal("a finished plan was staged for resume")
	}
	if !strings.Contains(notice, "nothing left to run") {
		t.Fatalf("notice = %s", notice)
	}
	if _, err := specialist.FindSavedPlan(paths, "sweep_resume"); err == nil {
		t.Fatal("a remainder was written for a plan with no remainder")
	}
}

// With no plan in the log, resume refuses and POINTS AT run — the user wants
// this plan executed, and the difference is only where it starts.
func TestResumingWithNoRecordedPlanPointsAtRun(t *testing.T) {
	m, paths, _ := resumeModel(t)
	stored, _ := specialist.FindSavedPlan(paths, "sweep")
	_, notice, ok := m.resumeSavedPlan(stored)
	if ok {
		t.Fatal("resume staged a plan with nothing recorded")
	}
	if !strings.Contains(notice, "/plans run sweep") {
		t.Fatalf("the refusal must offer the way forward: %s", notice)
	}
}

// BARE `/plans resume` KEEPS ITS OLD MEANING. It un-pauses the running plan; the
// saved-plan resume is the form that takes a name. Overloading by arity must not
// break the control verb that shipped first.
func TestBareResumeStillUnpausesTheRunningPlan(t *testing.T) {
	m, _ := savedPlanModel(t)
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.planProgress.PlanRunning(cancel)
	m.planProgress.SetPlanPaused(true)

	updated, cmd := m.handlePlansCommand("resume")
	if cmd != nil {
		t.Fatal("bare resume started a turn")
	}
	if m.planProgress.PlanPaused() {
		t.Fatal("bare /plans resume did not un-pause the running plan")
	}
	if !strings.Contains(transcriptText(updated.(model).transcript), "Resuming the plan") {
		t.Fatalf("bare resume said something else: %s", transcriptText(updated.(model).transcript))
	}
}

func mustLoad(t *testing.T, paths specialist.PlanPaths) []specialist.SavedPlan {
	t.Helper()
	plans, problems := specialist.LoadPlans(paths)
	if len(problems) != 0 {
		t.Fatalf("problems loading plans: %v", problems)
	}
	return plans
}

// RESTART runs the last plan from the beginning, staged from the BRIDGE's copy
// — the panel holds a rendering, and restarting that would run something that
// merely resembles what ran.
func TestRestartStagesTheLastPlanFromTheBeginning(t *testing.T) {
	m, paths, plan := resumeModel(t)
	m.planProgress.PlanAdmitted(plan)
	// A partly-finished run: restart must ignore the progress entirely, which is
	// exactly what distinguishes it from resume.
	m.planProgress.TaskDispatched(specialist.Task{ID: plan.Order()[0]})
	m.planProgress.TaskCompleted(specialist.TaskResult{ID: plan.Order()[0], Attempts: 1})

	// The turn itself needs a provider this harness has none of, so what is
	// asserted is what restart is responsible for: the staged plan and what the
	// user is told. Whether dispatchCommand can start a run is the composer's
	// business and is covered where a provider exists.
	updated, _ := m.restartLastPlan()
	staged, err := specialist.FindSavedPlan(paths, restartPlanName)
	if err != nil {
		t.Fatalf("nothing was staged: %v", err)
	}
	restored, err := specialist.ParsePlan(staged.Args, m.savedPlanLimits())
	if err != nil {
		t.Fatalf("the staged plan does not validate: %v", err)
	}
	if restored.TaskCount() != plan.TaskCount() {
		t.Fatalf("staged %d tasks, the plan had %d — restart must not narrow",
			restored.TaskCount(), plan.TaskCount())
	}
	if !strings.Contains(transcriptText(updated.(model).transcript), "from the beginning") {
		t.Fatalf("restart must say it starts over: %s", transcriptText(updated.(model).transcript))
	}
}

// seedUserPlan writes a user-authored plan, distinct from anything a restart or
// resume would generate, and returns its path and exact bytes.
func seedUserPlan(t *testing.T, paths specialist.PlanPaths, name string) (string, []byte) {
	t.Helper()
	plan, err := specialist.ParsePlan(map[string]any{
		"name":   "mine",
		"tasks":  []any{map[string]any{"id": "x", "prompt": "user authored, keep me"}},
		"budget": map[string]any{"max_workers": float64(1)},
	}, specialist.Limits{MaxTasks: 20, ParentTools: []string{"read_file"}})
	if err != nil {
		t.Fatal(err)
	}
	path, err := specialist.SavePlan(paths.ProjectRoot, paths.ProjectDir, name, plan)
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return path, body
}

// admitPlan delivers the admission message the plan tool's bridge sends once it
// has loaded and validated a plan.
func admitPlan(t *testing.T, m model, plan specialist.Plan) model {
	t.Helper()
	if m.now == nil {
		m.now = func() time.Time { return time.Unix(1000, 0) }
	}
	updated, _ := m.Update(planAdmittedMsg{runID: m.activeRunID, name: plan.Name(), taskCount: plan.TaskCount()})
	return updated.(model)
}

// STAGING NEVER OWNS A USER'S PLAN. Restart, bare resume and named resume each
// prefer a fixed name a user can also choose. When the user already has a plan
// there, the generated plan is staged under the next free name, the user's
// bytes stay exactly as written, and admission removes only what was staged.
func TestStagingNeverReplacesAUserPlanOfTheSameName(t *testing.T) {
	cases := []struct {
		name      string
		collision string
		stage     func(t *testing.T, m model) (model, string)
	}{
		{
			name:      "restart",
			collision: restartPlanName,
			stage: func(t *testing.T, m model) (model, string) {
				updated, _ := m.restartLastPlan()
				return updated.(model), transcriptText(updated.(model).transcript)
			},
		},
		{
			name:      "bare resume",
			collision: resumePlanName,
			stage: func(t *testing.T, m model) (model, string) {
				updated, _ := m.resumeLastPlan()
				return updated.(model), transcriptText(updated.(model).transcript)
			},
		},
		{
			name:      "named resume",
			collision: "sweep_resume",
			stage: func(t *testing.T, m model) (model, string) {
				stored, err := specialist.FindSavedPlan(m.planPaths, "sweep")
				if err != nil {
					t.Fatal(err)
				}
				name, notice, ok := m.resumeSavedPlan(stored)
				if !ok {
					t.Fatalf("named resume refused: %s", notice)
				}
				return m, notice + "\nstaged=" + name
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, paths, plan := resumeModel(t)
			m.planProgress.PlanAdmitted(plan)
			m.planProgress.TaskDispatched(specialist.Task{ID: plan.Order()[0]})
			userPath, before := seedUserPlan(t, paths, tc.collision)

			m, text := tc.stage(t, m)
			want := tc.collision + "_2"
			if !strings.Contains(text, `"`+want+`"`) && !strings.Contains(text, "staged="+want) {
				t.Fatalf("staging did not move to the free name %q: %s", want, text)
			}
			if after, err := os.ReadFile(userPath); err != nil || string(after) != string(before) {
				t.Fatalf("staging changed the user's %s plan (err=%v):\nbefore %s\nafter  %s", tc.collision, err, before, after)
			}
			staged, err := specialist.FindSavedPlan(paths, want)
			if err != nil {
				t.Fatalf("nothing staged under %q: %v", want, err)
			}
			if _, err := specialist.ParsePlan(staged.Args, m.savedPlanLimits()); err != nil {
				t.Fatalf("the staged plan does not validate: %v", err)
			}

			m = admitPlan(t, m, plan)
			if _, err := os.Stat(staged.Path); !os.IsNotExist(err) {
				t.Fatalf("the staged plan outlived its admission: %v", err)
			}
			if after, err := os.ReadFile(userPath); err != nil || string(after) != string(before) {
				t.Fatalf("admission changed the user's %s plan (err=%v)", tc.collision, err)
			}
		})
	}
}

// A SECOND RESTART MUST WORK. Exclusive publication alone made the first
// restart's own staged file block every later restart in the project, with an
// "already exists" error about a file the user never wrote.
func TestRepeatedRestartReusesTheStagingNameAfterAdmission(t *testing.T) {
	m, paths, plan := resumeModel(t)
	m.planProgress.PlanAdmitted(plan)
	for round := 1; round <= 2; round++ {
		updated, _ := m.restartLastPlan()
		m = updated.(model)
		text := transcriptText(m.transcript)
		if strings.Contains(text, "already exists") {
			t.Fatalf("round %d: restart refused over its own staged plan: %s", round, text)
		}
		staged, err := specialist.FindSavedPlan(paths, restartPlanName)
		if err != nil {
			t.Fatalf("round %d: nothing staged under %q: %v", round, restartPlanName, err)
		}
		m = admitPlan(t, m, plan)
		if _, err := os.Stat(staged.Path); !os.IsNotExist(err) {
			t.Fatalf("round %d: the staged plan outlived its admission: %v", round, err)
		}
	}
}

// Ownership is the staged bytes, not the name: a staged file someone edited
// before admission is theirs now and is left in place.
func TestAdmissionLeavesAnEditedStagedPlanAlone(t *testing.T) {
	m, paths, plan := resumeModel(t)
	m.planProgress.PlanAdmitted(plan)
	updated, _ := m.restartLastPlan()
	m = updated.(model)
	if _, err := specialist.FindSavedPlan(paths, restartPlanName); err != nil {
		t.Fatalf("nothing staged: %v", err)
	}
	editedPath, edited := seedUserPlan(t, paths, restartPlanName)

	m = admitPlan(t, m, plan)
	if after, err := os.ReadFile(editedPath); err != nil || string(after) != string(edited) {
		t.Fatalf("admission removed or changed an edited staged plan (err=%v)", err)
	}
}

// Admission of a DIFFERENT plan does not consume a staged one the model has not
// run yet.
func TestAdmittingAnotherPlanKeepsTheStagedOne(t *testing.T) {
	m, paths, plan := resumeModel(t)
	m.planProgress.PlanAdmitted(plan)
	updated, _ := m.restartLastPlan()
	m = updated.(model)
	other, err := specialist.ParsePlan(map[string]any{
		"name":   "other",
		"tasks":  []any{map[string]any{"id": "y", "prompt": "unrelated"}},
		"budget": map[string]any{"max_workers": float64(1)},
	}, specialist.Limits{MaxTasks: 20, ParentTools: []string{"read_file"}})
	if err != nil {
		t.Fatal(err)
	}
	m = admitPlan(t, m, other)
	if _, err := specialist.FindSavedPlan(paths, restartPlanName); err != nil {
		t.Fatalf("an unrelated admission removed the staged plan: %v", err)
	}
}

// Restart with nothing to restart refuses and points at what does exist.
func TestRestartWithNoPlanRefuses(t *testing.T) {
	m, _ := savedPlanModel(t)
	updated, cmd := m.restartLastPlan()
	if cmd != nil {
		t.Fatal("restart started a turn with no plan")
	}
	text := transcriptText(updated.(model).transcript)
	if !strings.Contains(text, "nothing to restart") || !strings.Contains(text, "/plans run") {
		t.Fatalf("the refusal must point somewhere: %s", text)
	}
}

// RESTARTING UNDER A RUNNING PLAN IS REFUSED. Two plans reporting into one
// panel keyed by task id means the second overwrites the first's rows, and the
// UI would silently pick one.
func TestRestartRefusesWhileAPlanIsRunning(t *testing.T) {
	m, paths, plan := resumeModel(t)
	m.planProgress.PlanAdmitted(plan)
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.planProgress.PlanRunning(cancel)

	updated, cmd := m.restartLastPlan()
	if cmd != nil {
		t.Fatal("restart started a second plan over a running one")
	}
	if !strings.Contains(transcriptText(updated.(model).transcript), "/plans stop") {
		t.Fatalf("the refusal must say how to proceed: %s", transcriptText(updated.(model).transcript))
	}
	if _, err := specialist.FindSavedPlan(paths, restartPlanName); err == nil {
		t.Fatal("a refused restart staged a plan anyway")
	}
}

// Restart and resume are DIFFERENT: one ignores progress, the other honours it.
// Asserted together because the whole risk is that one silently becomes the
// other.
func TestRestartIgnoresProgressWhereResumeHonoursIt(t *testing.T) {
	m, paths, plan := resumeModel(t)
	if _, err := specialist.SavePlan(paths.ProjectRoot, paths.ProjectDir, "sweep", plan); err != nil {
		t.Fatal(err)
	}
	m.planProgress.PlanAdmitted(plan)
	for _, id := range plan.Order()[:len(plan.Order())-1] {
		m.planProgress.TaskDispatched(specialist.Task{ID: id})
		m.planProgress.TaskCompleted(specialist.TaskResult{ID: id, Attempts: 1})
	}

	m.restartLastPlan()
	restarted, err := specialist.FindSavedPlan(paths, restartPlanName)
	if err != nil {
		t.Fatalf("restart staged nothing: %v", err)
	}
	full, _ := specialist.ParsePlan(restarted.Args, m.savedPlanLimits())

	stored, _ := specialist.FindSavedPlan(paths, "sweep")
	resumeName, _, ok := m.resumeSavedPlan(stored)
	if !ok {
		t.Fatal("resume refused")
	}
	resumed, _ := specialist.FindSavedPlan(paths, resumeName)
	narrowed, _ := specialist.ParsePlan(resumed.Args, m.savedPlanLimits())

	if full.TaskCount() != plan.TaskCount() {
		t.Fatalf("restart narrowed the plan: %d of %d", full.TaskCount(), plan.TaskCount())
	}
	if narrowed.TaskCount() >= full.TaskCount() {
		t.Fatalf("resume did not narrow: %d vs restart's %d", narrowed.TaskCount(), full.TaskCount())
	}
}

// A SAVED PLAN'S PROMPTS ARE MODEL-AUTHORED TEXT READ BACK FROM DISK — the
// same trust level as a live task's, sanitized on the same chokepoint. The
// saved path only cut at the first newline, so ESC (the byte that starts an
// ANSI sequence) and other control characters reached the display.
func TestSavedPlanSummariesDropControlCharacters(t *testing.T) {
	got := planTaskSummaryLine("audit \x1b[31mred\x1b[0m thing\x07 done.\nsecond line")
	if strings.ContainsAny(got, "\x1b\x07\r\n") {
		t.Fatalf("control characters reached the summary: %q", got)
	}
	if !strings.Contains(got, "audit") || !strings.Contains(got, "done.") {
		t.Fatalf("the printable text must survive: %q", got)
	}
	if strings.Contains(got, "second line") {
		t.Fatalf("only the first line belongs in a summary: %q", got)
	}
}
