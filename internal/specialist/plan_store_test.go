package specialist

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/tools"
)

func savedPlanFixture(t *testing.T) Plan {
	t.Helper()
	return mustPlan(t, []any{
		task("root", "look at the tree"),
		map[string]any{"id": "left", "prompt": "read a\nsecond line", "depends_on": []any{"root"},
			"tools": []any{"grep"}, "phase": "analysis"},
		task("right", "read b", "root"),
	}, map[string]any{
		"max_workers": float64(1), "max_tokens": float64(500_000),
		"max_wall_seconds": float64(600), "max_stall_seconds": float64(45), "max_retries": float64(2),
	}, readOnlyLimits())
}

// THE ROUND TRIP IS THE FEATURE. A saved plan is stored as ARGS and re-admitted
// through ParsePlan, so a plan that comes back has to be the plan that went in —
// including through JSON, which is the form it is actually stored in.
func TestASavedPlanRoundTripsThroughArgsAndJSON(t *testing.T) {
	original := savedPlanFixture(t)

	encoded, err := json.Marshal(original.Args())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	restored, err := ParsePlan(decoded, readOnlyLimits())
	if err != nil {
		t.Fatalf("a saved plan did not re-admit: %v", err)
	}

	if !reflect.DeepEqual(original.Tasks(), restored.Tasks()) {
		t.Fatalf("tasks changed:\n%+v\nvs\n%+v", original.Tasks(), restored.Tasks())
	}
	if !reflect.DeepEqual(original.Order(), restored.Order()) {
		t.Fatalf("execution order changed: %v vs %v", original.Order(), restored.Order())
	}
	if original.Budget() != restored.Budget() {
		t.Fatalf("budget changed:\n%+v\nvs\n%+v", original.Budget(), restored.Budget())
	}
	if original.Name() != restored.Name() || original.Description() != restored.Description() {
		t.Fatalf("identity changed: %q/%q vs %q/%q",
			original.Name(), original.Description(), restored.Name(), restored.Description())
	}
}

// RESOLVED DEFAULTS ARE WRITTEN OUT. A plan saved today must run the same way
// after a default moves — otherwise "run it again" quietly means something else.
func TestASavedPlanPinsTheDefaultsThatWereInForce(t *testing.T) {
	plan := mustPlan(t, []any{task("a", "x")}, map[string]any{"max_workers": float64(1)}, readOnlyLimits())
	args := plan.Args()
	budget, _ := args["budget"].(map[string]any)
	if budget["max_retries"] != defaultPlanRetries {
		t.Fatalf("max_retries = %v; the resolved default must be written out", budget["max_retries"])
	}
	// An unbounded budget stays unbounded rather than acquiring a zero that a
	// later reader might treat as a bound.
	if _, present := budget["max_tokens"]; present {
		t.Fatalf("an unbounded plan gained a max_tokens: %v", budget["max_tokens"])
	}
}

func TestSavedPlansAreWrittenAndListedByScope(t *testing.T) {
	root := t.TempDir()
	userRoot := t.TempDir()
	paths := PlanPaths{
		ProjectRoot: root,
		ProjectDir:  filepath.Join(root, ".zero", "plans"),
		UserRoot:    userRoot,
		UserDir:     filepath.Join(userRoot, "zero", "plans"),
	}

	if _, err := SavePlan(paths.ProjectRoot, paths.ProjectDir, "sweep", savedPlanFixture(t)); err != nil {
		t.Fatalf("SavePlan project: %v", err)
	}
	if _, err := SavePlan(paths.UserRoot, paths.UserDir, "personal", savedPlanFixture(t)); err != nil {
		t.Fatalf("SavePlan user: %v", err)
	}

	plans, problems := LoadPlans(paths)
	if len(problems) != 0 {
		t.Fatalf("unexpected problems: %v", problems)
	}
	plans = onlyOnDisk(plans)
	if len(plans) != 2 {
		t.Fatalf("loaded %d on-disk plans, want 2", len(plans))
	}
	byName := map[string]SavedPlan{}
	for _, plan := range plans {
		byName[plan.Name] = plan
	}
	if !byName["sweep"].Project() {
		t.Fatal("the project plan is not marked as one")
	}
	if byName["personal"].Project() {
		t.Fatal("the user plan is marked as a project plan")
	}
	if byName["sweep"].TaskCount != 3 {
		t.Fatalf("task count = %d, want 3", byName["sweep"].TaskCount)
	}
}

// Project shadows user, mirroring usercommands and the specialist loader: a
// repo's own plan is the one its contributors get.
func TestAProjectPlanShadowsAUserPlanOfTheSameName(t *testing.T) {
	root := t.TempDir()
	userRoot := t.TempDir()
	paths := PlanPaths{
		ProjectRoot: root,
		ProjectDir:  filepath.Join(root, ".zero", "plans"),
		UserRoot:    userRoot,
		UserDir:     filepath.Join(userRoot, "zero", "plans"),
	}
	if _, err := SavePlan(paths.UserRoot, paths.UserDir, "sweep", mustPlan(t,
		[]any{task("u", "user version")}, okBudget(), readOnlyLimits())); err != nil {
		t.Fatal(err)
	}
	if _, err := SavePlan(paths.ProjectRoot, paths.ProjectDir, "sweep", mustPlan(t,
		[]any{task("p1", "project"), task("p2", "project")}, okBudget(), readOnlyLimits())); err != nil {
		t.Fatal(err)
	}

	found, err := FindSavedPlan(paths, "sweep")
	if err != nil {
		t.Fatalf("FindSavedPlan: %v", err)
	}
	if !found.Project() || found.TaskCount != 2 {
		t.Fatalf("the user plan won: project=%v tasks=%d", found.Project(), found.TaskCount)
	}
}

func TestAnUnreadableHigherPriorityPlanBlocksTheLowerPlanOfTheSameName(t *testing.T) {
	root := t.TempDir()
	userRoot := t.TempDir()
	paths := PlanPaths{
		ProjectRoot: root, ProjectDir: filepath.Join(root, ".zero", "plans"),
		UserRoot: userRoot, UserDir: filepath.Join(userRoot, "zero", "plans"),
	}
	if _, err := SavePlan(paths.UserRoot, paths.UserDir, "sweep", savedPlanFixture(t)); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.ProjectDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(paths.ProjectDir, "sweep.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := FindSavedPlan(paths, "sweep"); err == nil || !strings.Contains(err.Error(), "could not be read") {
		t.Fatalf("lookup degraded to the user plan instead of reporting the broken project plan: %v", err)
	}
}

func TestAMissingSavedPlanDirectoryIsNotAProblem(t *testing.T) {
	root := t.TempDir()
	_, problems := LoadPlans(PlanPaths{ProjectRoot: root, ProjectDir: filepath.Join(root, ".zero", "plans")})
	if len(problems) != 0 {
		t.Fatalf("an optional missing directory was reported as broken: %v", problems)
	}
}

// THE NAME IS THE PATH GUARD. It is an allow-list, so no traversal component
// can be spelled at all — the pattern this repo has watched leak three times
// when written as a deny-list.
func TestPlanNamesAreAnAllowListAndCannotTraverse(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "plans")
	for _, name := range []string{
		"../escape", "..", ".", "a/b", `a\b`, "a b", "a.json", "", strings.Repeat("x", 65),
		"~/evil", "a;b", "a\x00b",
	} {
		if _, err := SavePlan(dir, dir, name, savedPlanFixture(t)); err == nil {
			t.Errorf("SavePlan accepted %q", name)
		}
		if _, err := FindSavedPlan(PlanPaths{ProjectDir: dir}, name); err == nil {
			t.Errorf("FindSavedPlan accepted %q", name)
		}
	}
	// ...and the ordinary shapes still work.
	for _, name := range []string{"sweep", "pre-release", "audit_2", "A1"} {
		if _, err := SavePlan(dir, dir, name, savedPlanFixture(t)); err != nil {
			t.Errorf("SavePlan rejected %q: %v", name, err)
		}
	}
}

// A SYMLINK IS REFUSED, on the file and on the directory. Without this, "save my
// plan" is a file-overwrite primitive pointed at whatever the link targets.
func TestSavingRefusesToWriteThroughASymlink(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "precious")
	if err := os.WriteFile(target, []byte("do not clobber"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, "plans")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "sweep.json")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if _, err := SavePlan(dir, dir, "sweep", savedPlanFixture(t)); err == nil {
		t.Fatal("SavePlan wrote through a symlink")
	}
	body, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "do not clobber" {
		t.Fatalf("the symlink target was overwritten: %q", body)
	}

	// A linked DIRECTORY is refused too, or the file check is bypassed by
	// pointing one level up.
	linkedDir := filepath.Join(base, "linked")
	if err := os.Symlink(dir, linkedDir); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := SavePlan(base, linkedDir, "other", savedPlanFixture(t)); err == nil {
		t.Fatal("SavePlan wrote into a symlinked directory")
	}
}

// ...and a symlinked plan file is not LOADED either, or a repo could point one
// at a file outside the workspace and have its contents parsed as a plan.
func TestLoadingRefusesASymlinkedPlanAndSaysSo(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "elsewhere.json")
	if err := os.WriteFile(target, []byte(`{"tasks":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, "plans")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "linked.json")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	plans, problems := LoadPlans(PlanPaths{ProjectDir: dir})
	if got := onlyOnDisk(plans); len(got) != 0 {
		t.Fatalf("a symlinked plan was loaded: %+v", got)
	}
	if len(problems) != 1 || !strings.Contains(problems[0], "symlink") {
		t.Fatalf("the refusal must be reported, not silent: %v", problems)
	}
}

// MALFORMED IS AN ERROR, NEVER A SILENT SKIP. A plan file that does not parse is
// named, or a user believes they ran something they did not.
func TestAMalformedPlanFileIsReportedByName(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "plans")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	plans, problems := LoadPlans(PlanPaths{ProjectDir: dir})
	if got := onlyOnDisk(plans); len(got) != 0 {
		t.Fatalf("a malformed file produced a plan: %+v", got)
	}
	if len(problems) != 1 || !strings.Contains(problems[0], "broken.json") {
		t.Fatalf("the problem must name the file: %v", problems)
	}
	// And looking it up says so, rather than "you have no plan by that name"
	// while it sits on disk.
	_, err := FindSavedPlan(PlanPaths{ProjectDir: dir}, "broken")
	if err == nil || !strings.Contains(err.Error(), "could not be read") {
		t.Fatalf("a lookup past an unreadable file must say so: %v", err)
	}
}

// A saved plan is re-admitted against the CURRENT run's limits, so nothing that
// was legal when it was saved is grandfathered in.
func TestASavedPlanIsRevalidatedAgainstTheRunningLimits(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "plans")
	plan := mustPlan(t, []any{
		task("a", "x"), task("b", "y"), task("c", "z"), task("d", "w"), task("e", "v"), task("f", "u"),
	}, okBudget(), readOnlyLimits())
	if _, err := SavePlan(dir, dir, "big", plan); err != nil {
		t.Fatal(err)
	}
	stored, err := FindSavedPlan(PlanPaths{ProjectDir: dir}, "big")
	if err != nil {
		t.Fatal(err)
	}

	// The tier has since been tightened: the stored plan is refused.
	if _, err := ParsePlan(stored.Args, Limits{MaxTasks: 5, ParentTools: []string{"read_file"}}); err == nil {
		t.Fatal("a stored plan bypassed the current run's task ceiling")
	}
	// And a grant it no longer holds is refused too.
	narrow := mustPlan(t, []any{map[string]any{"id": "a", "prompt": "x", "tools": []any{"grep"}}},
		okBudget(), readOnlyLimits())
	if _, err := SavePlan(dir, dir, "narrow", narrow); err != nil {
		t.Fatal(err)
	}
	storedNarrow, err := FindSavedPlan(PlanPaths{ProjectDir: dir}, "narrow")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParsePlan(storedNarrow.Args, Limits{MaxTasks: 20, ParentTools: []string{"read_file"}}); err == nil {
		t.Fatal("a stored plan kept a tool grant this run does not hold")
	}
}

// The tool's `saved` argument is ONE path into the same constructor, not a
// second way to run a plan.
func TestTheToolRunsASavedPlanThroughTheSameValidation(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "plans")
	if _, err := SavePlan(dir, dir, "sweep", savedPlanFixture(t)); err != nil {
		t.Fatal(err)
	}
	tool := &OrchestrateTool{Plans: PlanPaths{ProjectDir: dir}}

	resolved, err := tool.resolveSavedPlan(map[string]any{"saved": "sweep"})
	if err != nil {
		t.Fatalf("resolveSavedPlan: %v", err)
	}
	plan, err := ParsePlan(resolved, readOnlyLimits())
	if err != nil {
		t.Fatalf("the resolved plan did not admit: %v", err)
	}
	if plan.TaskCount() != 3 {
		t.Fatalf("task count = %d, want 3", plan.TaskCount())
	}
}

// A SAVED PLAN RUNS AS IT WAS SAVED. Merging a caller's field into it would mean
// "run the sweep plan" ran something else while the transcript still said sweep.
func TestASavedReferenceRefusesInlineOverrides(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "plans")
	if _, err := SavePlan(dir, dir, "sweep", savedPlanFixture(t)); err != nil {
		t.Fatal(err)
	}
	tool := &OrchestrateTool{Plans: PlanPaths{ProjectDir: dir}}

	for _, field := range []string{"tasks", "budget", "name", "description"} {
		args := map[string]any{"saved": "sweep", field: "anything"}
		if _, err := tool.resolveSavedPlan(args); err == nil {
			t.Errorf("a saved reference accepted an inline %q", field)
		}
	}
}

// With no plan directories the refusal SAYS saved plans are unavailable, rather
// than "not found", which reads as "you never saved it".
func TestASavedReferenceWithoutStorageSaysSo(t *testing.T) {
	tool := &OrchestrateTool{}
	_, err := tool.resolveSavedPlan(map[string]any{"saved": "sweep"})
	if err == nil || !strings.Contains(err.Error(), "not available") {
		t.Fatalf("err = %v; it must say saved plans are unavailable", err)
	}
}

// A plan with no `saved` reference is untouched — the ordinary path must not
// change shape because a new one exists.
func TestAnInlinePlanIsUnaffectedBySavedPlans(t *testing.T) {
	tool := &OrchestrateTool{Plans: PlanPaths{ProjectDir: t.TempDir()}}
	args := planArgs([]any{task("a", "x")}, okBudget())
	resolved, err := tool.resolveSavedPlan(args)
	if err != nil {
		t.Fatalf("resolveSavedPlan: %v", err)
	}
	if !reflect.DeepEqual(resolved, args) {
		t.Fatalf("an inline plan was rewritten:\n%+v\nvs\n%+v", resolved, args)
	}
}

// The tool still refuses everything when the posture is off, saved plan or not:
// a stored plan must not become a way round the gate.
func TestASavedPlanCannotRunWithThePostureOff(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "plans")
	if _, err := SavePlan(dir, dir, "sweep", savedPlanFixture(t)); err != nil {
		t.Fatal(err)
	}
	tool := &OrchestrateTool{Plans: PlanPaths{ProjectDir: dir}}
	result := tool.Run(t.Context(), map[string]any{"saved": "sweep"})
	if result.Status != tools.StatusError || !strings.Contains(result.Output, "zeromaxing") {
		t.Fatalf("a saved plan ran with the posture off: %+v", result)
	}
}

// onlyOnDisk drops the bundled plans, which every load now includes. Written as
// a filter rather than by adjusting the expected counts so a test that means
// "nothing was written" keeps saying that rather than "one thing was".
func onlyOnDisk(plans []SavedPlan) []SavedPlan {
	out := make([]SavedPlan, 0, len(plans))
	for _, plan := range plans {
		if plan.Scope != PlanScopeBuiltin {
			out = append(out, plan)
		}
	}
	return out
}

// THE BUNDLED PLAN MUST ACTUALLY ADMIT. A shipped example that does not parse is
// worse than none: it is the first thing anyone tries, and it would teach that
// the format does not work.
func TestEveryBundledPlanAdmits(t *testing.T) {
	bundled := builtinPlans()
	if len(bundled) == 0 {
		t.Fatal("no plans are bundled with the binary")
	}
	for _, plan := range bundled {
		admitted, err := ParsePlan(plan.Args, Limits{
			MaxTasks: 20, ParentTools: PlanReadOnlyToolNames()})
		if err != nil {
			t.Errorf("bundled plan %q does not admit: %v", plan.Name, err)
			continue
		}
		if admitted.TaskCount() != plan.TaskCount {
			t.Errorf("%q: listing says %d tasks, the plan has %d",
				plan.Name, plan.TaskCount, admitted.TaskCount())
		}
		if strings.TrimSpace(plan.Description) == "" {
			t.Errorf("bundled plan %q has no description; the listing is where it is discovered", plan.Name)
		}
		if plan.Scope != PlanScopeBuiltin {
			t.Errorf("bundled plan %q is scoped %q", plan.Name, plan.Scope)
		}
	}
}

// It has to fit the SMALLEST tier, or the shipped example is unusable for
// exactly the users who set the tightest ceiling.
func TestTheBundledPlansFitTheSmallestTier(t *testing.T) {
	for _, plan := range builtinPlans() {
		if _, err := ParsePlan(plan.Args, Limits{
			MaxTasks: config.PlanSizeSmall.MaxTasks(), ParentTools: PlanReadOnlyToolNames()}); err != nil {
			t.Errorf("bundled plan %q does not fit the small tier: %v", plan.Name, err)
		}
	}
}

// A BUNDLED PLAN IS AN EXAMPLE, NEVER AN OVERRIDE. Anything on disk with the
// same name wins, or shipping a new example could silently replace something
// someone wrote.
func TestABundledPlanIsShadowedByOneOnDisk(t *testing.T) {
	bundled := builtinPlans()
	if len(bundled) == 0 {
		t.Skip("nothing bundled")
	}
	name := bundled[0].Name
	dir := filepath.Join(t.TempDir(), "plans")
	mine := mustPlan(t, []any{task("mine", "my own version")}, okBudget(), readOnlyLimits())
	if _, err := SavePlan(dir, dir, name, mine); err != nil {
		t.Fatal(err)
	}

	found, err := FindSavedPlan(PlanPaths{UserDir: dir}, name)
	if err != nil {
		t.Fatal(err)
	}
	if found.Scope != PlanScopeUser || found.TaskCount != 1 {
		t.Fatalf("the bundled plan won: scope=%q tasks=%d", found.Scope, found.TaskCount)
	}
}

// The bundled plan is reachable with NO directories configured at all — that is
// the point of shipping it in the binary.
func TestTheBundledPlanIsAvailableWithNoDirectories(t *testing.T) {
	plans, problems := LoadPlans(PlanPaths{})
	if len(problems) != 0 {
		t.Fatalf("problems: %v", problems)
	}
	if len(plans) == 0 {
		t.Fatal("no plans are available without configured directories")
	}
	if _, err := FindSavedPlan(PlanPaths{}, plans[0].Name); err != nil {
		t.Fatalf("the bundled plan is not findable: %v", err)
	}
}

// A BACKGROUND PLAN RETURNS IMMEDIATELY AND SAYS IT IS NOT DONE. Returning a
// summary would be reporting work that has not happened — this repo's oldest
// defect class, at the point where it would be least visible.
func TestABackgroundPlanReturnsWithoutRunningAndSaysSo(t *testing.T) {
	var launched func(context.Context)
	tool := &OrchestrateTool{
		PostureActive: func() bool { return true },
		RunTask: func(context.Context, PlanTaskRequest) (TaskResult, error) {
			t.Fatal("a background plan ran on the tool-call goroutine")
			return TaskResult{}, nil
		},
		ParentTools: []string{"read_file"},
		Launch:      func(run func(context.Context)) bool { launched = run; return true },
	}
	result := tool.Run(t.Context(), map[string]any{
		"name":       "sweep",
		"tasks":      []any{task("a", "x")},
		"budget":     map[string]any{"max_workers": float64(1)},
		"background": true,
	})
	if result.Status != tools.StatusOK {
		t.Fatalf("status = %q: %s", result.Status, result.Output)
	}
	if launched == nil {
		t.Fatal("nothing was handed to the launcher")
	}
	for _, want := range []string{"background", "NOT finished", "later turn in this session", "/resume", "originating session"} {
		if !strings.Contains(result.Output, want) {
			t.Errorf("the result must say %q: %q", want, result.Output)
		}
	}
	if strings.Contains(result.Output, "succeeded") {
		t.Fatalf("a background plan reported a result it does not have: %q", result.Output)
	}
	if result.Meta["plan_status"] != "background" {
		t.Fatalf("plan_status = %q", result.Meta["plan_status"])
	}
}

// NO LAUNCHER MEANS REFUSED, with the reason. A plan started where nothing can
// report it is the background failure mode itself.
func TestABackgroundPlanWithoutALauncherIsRefused(t *testing.T) {
	tool := &OrchestrateTool{
		PostureActive: func() bool { return true },
		RunTask:       func(context.Context, PlanTaskRequest) (TaskResult, error) { return TaskResult{}, nil },
		ParentTools:   []string{"read_file"},
	}
	result := tool.Run(t.Context(), map[string]any{
		"tasks":      []any{task("a", "x")},
		"budget":     map[string]any{"max_workers": float64(1)},
		"background": true,
	})
	if result.Status != tools.StatusError {
		t.Fatalf("status = %q; a background plan with no launcher must be refused", result.Status)
	}
	if !strings.Contains(result.Output, "not available in this run") {
		t.Fatalf("the refusal must say why: %q", result.Output)
	}
}

// A REFUSED LAUNCH IS REPORTED, not turned into a run id for a plan nobody
// started.
func TestARefusedLaunchIsReportedAsAnError(t *testing.T) {
	tool := &OrchestrateTool{
		PostureActive: func() bool { return true },
		RunTask:       func(context.Context, PlanTaskRequest) (TaskResult, error) { return TaskResult{}, nil },
		ParentTools:   []string{"read_file"},
		Launch:        func(func(context.Context)) bool { return false },
	}
	result := tool.Run(t.Context(), map[string]any{
		"tasks":      []any{task("a", "x")},
		"budget":     map[string]any{"max_workers": float64(1)},
		"background": true,
	})
	if result.Status != tools.StatusError || !strings.Contains(result.Output, "not started") {
		t.Fatalf("a refused launch must be an error saying so: %+v", result)
	}
}

// The launched closure runs the SAME plan through the SAME executor. Asserted by
// running it, because a launcher handed a closure that does nothing would
// satisfy every check above.
func TestTheLaunchedClosureActuallyRunsThePlan(t *testing.T) {
	var launched func(context.Context)
	ran := map[string]int{}
	tool := &OrchestrateTool{
		PostureActive: func() bool { return true },
		RunTask: func(_ context.Context, req PlanTaskRequest) (TaskResult, error) {
			ran[req.Task.ID]++
			return TaskResult{Outcome: TaskSucceeded}, nil
		},
		ParentTools: []string{"read_file"},
		Launch:      func(run func(context.Context)) bool { launched = run; return true },
	}
	recorder := &recordingRecorder{}
	tool.Recorder = recorder
	tool.Run(t.Context(), map[string]any{
		"tasks":      []any{task("a", "x"), task("b", "y", "a")},
		"budget":     map[string]any{"max_workers": float64(1)},
		"background": true,
	})
	launched(context.Background())

	if ran["a"] != 1 || ran["b"] != 1 {
		t.Fatalf("the launched closure ran %v; both tasks must run", ran)
	}
	if recorder.admitted != 1 || len(recorder.finished) != 1 {
		t.Fatalf("a background plan must record its own admission and completion: %+v", recorder)
	}
}

// background is OPT-IN. Any value that is not a true boolean leaves the plan in
// the foreground, so a flag that changes where a plan runs can never be
// inferred from something that happened to be there.
func TestBackgroundIsOptInOnly(t *testing.T) {
	for _, value := range []any{nil, "true", float64(1), "yes", false} {
		args := map[string]any{"background": value}
		if value == nil {
			delete(args, "background")
		}
		if planBool(args, "background") {
			t.Errorf("background was inferred from %#v", value)
		}
	}
	if !planBool(map[string]any{"background": true}, "background") {
		t.Fatal("an explicit true must be honoured")
	}
}

// THE TEMP PATH IS WHERE THE BYTES GO, so it is the path that has to be safe.
//
// dir and <name>.json were both refused as symlinks and the write went to
// neither: it went to <name>.json.tmp, a fixed name nothing looked at. A repo
// shipping that name as a symlink turned "save my plan" into the file-overwrite
// primitive the other two checks exist to prevent — the guard was on the door
// and the wall was open.
//
// The requirement is that the target is NOT overwritten. SavePlan is free to
// succeed while satisfying it, and now does: os.CreateTemp picks a name nobody
// can predict and opens it O_EXCL, so the planted link is never opened at all.
// That is stronger than refusing a symlink at a fixed name, which still leaves
// the window between the check and the open.
func TestSavingNeverWritesThroughASymlinkedTempFile(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "precious")
	if err := os.WriteFile(target, []byte("do not clobber"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, "plans")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// The old fixed temp name, planted. <name>.json itself is absent, so the two
	// existing checks both pass and this is the only thing in the way.
	if err := os.Symlink(target, filepath.Join(dir, "sweep"+planFileExt+planTempExt)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if _, err := SavePlan(dir, dir, "sweep", savedPlanFixture(t)); err != nil {
		t.Fatalf("SavePlan: %v", err)
	}
	body, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "do not clobber" {
		t.Fatalf("the symlink target was overwritten through the temp path: %q", body)
	}
}

func TestExclusiveSaveDoesNotReplaceAnExistingPlan(t *testing.T) {
	dir := t.TempDir()
	original := mustPlan(t, []any{task("original", "keep this")}, okBudget(), readOnlyLimits())
	if _, err := SavePlan(dir, dir, "last_run", original); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "last_run"+planFileExt)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	replacement := mustPlan(t, []any{task("replacement", "do not publish")}, okBudget(), readOnlyLimits())
	if _, err := SavePlanExclusive(dir, dir, "last_run", replacement); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("exclusive save did not report the collision: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("exclusive save replaced the existing plan")
	}
}

func TestConcurrentExclusiveSavesPublishExactlyOnePlan(t *testing.T) {
	dir := t.TempDir()
	plan := savedPlanFixture(t)
	errs := make([]error, 8)
	var wait sync.WaitGroup
	for i := range errs {
		wait.Add(1)
		go func(i int) {
			defer wait.Done()
			_, errs[i] = SavePlanExclusive(dir, dir, "last_run", plan)
		}(i)
	}
	wait.Wait()

	winners := 0
	for _, err := range errs {
		if err == nil {
			winners++
			continue
		}
		if !strings.Contains(err.Error(), "already exists") {
			t.Fatalf("losing save returned the wrong error: %v", err)
		}
	}
	if winners != 1 {
		t.Fatalf("exclusive saves produced %d winners, want exactly one", winners)
	}
	if _, err := FindSavedPlan(PlanPaths{ProjectRoot: dir, ProjectDir: dir}, "last_run"); err != nil {
		t.Fatalf("winning plan is not readable: %v", err)
	}
}

// TWO SAVES OF ONE PLAN MUST NOT STOMP EACH OTHER'S BYTES.
//
// The temp file used to be a fixed <name>.json.tmp, so concurrent saves wrote
// into the same file and whichever renamed second published a plan the other had
// half-written. Every save must land a parseable plan, and nothing may be left
// behind in the directory but the plan itself.
func TestConcurrentSavesOfOnePlanDoNotCorruptIt(t *testing.T) {
	dir := t.TempDir()
	plan := savedPlanFixture(t)

	var wait sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wait.Add(1)
		go func(i int) {
			defer wait.Done()
			_, errs[i] = SavePlan(dir, dir, "sweep", plan)
		}(i)
	}
	wait.Wait()

	// WHAT IS ASSERTED IS NON-CORRUPTION, not that every racer wins.
	//
	// Publishing is os.Rename, which on Windows is MoveFileEx with
	// REPLACE_EXISTING; concurrent replaces of one destination can come back as a
	// sharing violation there. That is a platform property this test cannot
	// verify from any other OS, so asserting all eight succeed would be claiming
	// something unchecked — and would turn into an intermittent failure on the
	// one job that cannot be reproduced locally.
	//
	// Elsewhere every save must succeed: nothing should be able to lose.
	landed := 0
	for i, err := range errs {
		switch {
		case err == nil:
			landed++
		case runtime.GOOS == "windows":
			t.Logf("save %d lost the rename race: %v", i, err)
		default:
			t.Fatalf("save %d: %v", i, err)
		}
	}
	if landed == 0 {
		t.Fatal("no concurrent save landed at all")
	}

	if _, err := FindSavedPlan(PlanPaths{UserDir: dir}, "sweep"); err != nil {
		t.Fatalf("the saved plan does not parse after concurrent saves: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) == planTempExt {
			t.Errorf("a temporary file survived a successful save: %s", entry.Name())
		}
	}
}
