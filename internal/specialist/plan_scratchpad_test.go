package specialist

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// THE MEASURED LOSS THIS CLOSES. A code-review child produced 18,349 characters
// and a dependent task received 4,000 of them. The rest existed, was paid for,
// and was unreachable.
const measuredChildReport = 18_349

func TestATruncatedExcerptNowPointsAtTheWholeAnswer(t *testing.T) {
	pad, err := NewScratchpad("audit")
	if err != nil {
		t.Fatal(err)
	}
	defer pad.Release()

	full := strings.Repeat("E", measuredChildReport)
	path, err := pad.Record("by_name", full)
	if err != nil {
		t.Fatal(err)
	}
	if path == "" {
		t.Fatal("nothing was recorded")
	}

	results := map[string]TaskResult{
		"by_name": {ID: "by_name", Outcome: TaskSucceeded, Output: full, ScratchpadPath: path},
	}
	brief := withDependencyBriefingBudget(Task{ID: "synth", DependsOn: []string{"by_name"}}, results, 4000, 12000)

	if !strings.Contains(brief, path) {
		t.Fatalf("the dependent was truncated and never told where the rest is:\n%s", brief)
	}
	if !strings.Contains(brief, "18349") {
		t.Fatalf("the briefing does not say how much it is holding back:\n%s", brief)
	}
	// And the file really holds the whole thing.
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(onDisk) != measuredChildReport {
		t.Fatalf("recorded %d characters, the task produced %d", len(onDisk), measuredChildReport)
	}
}

// A briefing that was NOT truncated has nothing to point at, and a path in every
// briefing trains the reader to ignore the one that matters.
func TestAnUntruncatedBriefingCarriesNoPointer(t *testing.T) {
	pad, err := NewScratchpad("small")
	if err != nil {
		t.Fatal(err)
	}
	defer pad.Release()
	path, err := pad.Record("find", "short answer")
	if err != nil {
		t.Fatal(err)
	}
	results := map[string]TaskResult{
		"find": {ID: "find", Outcome: TaskSucceeded, Output: "short answer", ScratchpadPath: path},
	}
	brief := withDependencyBriefingBudget(Task{ID: "s", DependsOn: []string{"find"}}, results, 4000, 12000)
	if strings.Contains(brief, path) {
		t.Fatalf("an untruncated briefing carried a pointer:\n%s", brief)
	}
}

// WITHOUT A SCRATCHPAD, THE OLD SENTENCE. Every existing plan must be unchanged.
func TestWithoutAScratchpadTheBriefingIsExactlyWhatItWas(t *testing.T) {
	results := map[string]TaskResult{
		"find": {ID: "find", Outcome: TaskSucceeded, Output: strings.Repeat("E", 9000)},
	}
	brief := withDependencyBriefingBudget(Task{ID: "s", DependsOn: []string{"find"}}, results, 4000, 12000)
	if !strings.Contains(brief, "[truncated — re-read the files named above if you need more]") {
		t.Fatalf("the pre-existing truncation notice is gone:\n%s", brief)
	}
}

// A nil scratchpad is a working value on every method — the path every caller
// that never asked for one takes.
func TestANilScratchpadIsAWorkingValue(t *testing.T) {
	var pad *Scratchpad
	if root := pad.Root(); root != "" {
		t.Fatalf("nil scratchpad reported root %q", root)
	}
	path, err := pad.Record("t", "output")
	if err != nil || path != "" {
		t.Fatalf("nil scratchpad recorded %q (err %v)", path, err)
	}
	pad.Release()
	pad.Release()
	if roots := scratchpadReadRoots(pad); roots != nil {
		t.Fatalf("nil scratchpad granted %v", roots)
	}
}

// A TASK ID BECOMES A FILENAME, so it is allow-listed. Model-supplied ids reach
// this, and a deny-list of traversal sequences is the pattern that has leaked
// here repeatedly.
func TestATaskIDThatCannotBeAFilenameIsRefusedNotEscaped(t *testing.T) {
	pad, err := NewScratchpad("p")
	if err != nil {
		t.Fatal(err)
	}
	defer pad.Release()
	for _, id := range []string{
		"../escape", "a/b", "..", ".", "with space", "semi;colon",
		strings.Repeat("x", 65), "", "nul\x00byte",
	} {
		path, err := pad.Record(id, "payload")
		if err != nil {
			t.Fatalf("id %q returned an error rather than declining: %v", id, err)
		}
		if path != "" {
			t.Fatalf("id %q was accepted as a filename: %q", id, path)
		}
	}
	// Nothing escaped the directory.
	entries, err := os.ReadDir(pad.Root())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("refused ids still wrote %d file(s)", len(entries))
	}
}

// Release actually deletes it — a plan's working notes are disposable, and a
// directory per plan run that never went away would accumulate silently.
func TestReleaseDeletesTheWholeDirectory(t *testing.T) {
	pad, err := NewScratchpad("p")
	if err != nil {
		t.Fatal(err)
	}
	root := pad.Root()
	if _, err := pad.Record("a", "x"); err != nil {
		t.Fatal(err)
	}
	pad.Release()
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("the scratchpad survived release: %v", err)
	}
	if pad.Root() != "" {
		t.Fatal("a released scratchpad still reports a root")
	}
}

// THE GRANT MUST REACH ARGV. A read root on a struct that never becomes
// --add-dir is a directory the child cannot open — the exact "layer B does not
// carry it" defect this branch has produced repeatedly.
func TestTheScratchpadReadRootReachesTheChildsArgv(t *testing.T) {
	pad, err := NewScratchpad("p")
	if err != nil {
		t.Fatal(err)
	}
	defer pad.Release()

	executor := Executor{
		BinaryPath:   "/bin/true",
		NewSessionID: func() (string, error) { return "specialist_00000000000000000000000a", nil },
		Load:         func(LoadOptions) (LoadResult, error) { return LoadResult{}, nil },
	}
	built, err := executor.BuildArgs(BuildArgsInput{
		Prompt:         "look",
		Cwd:            t.TempDir(),
		CurrentDepth:   0,
		PermissionMode: "auto",
		ExtraReadRoots: []string{pad.Root()},
	})
	if err != nil {
		t.Fatalf("BuildArgs: %v", err)
	}
	joined := strings.Join(built.Args, " ")
	if !strings.Contains(joined, pad.Root()) {
		t.Fatalf("the scratchpad never reached argv, so the child cannot read it:\n%s", joined)
	}
	if !strings.Contains(joined, "--add-dir") {
		t.Fatalf("no --add-dir in argv:\n%s", joined)
	}
}

// END TO END: a real plan, a real executor seam, and a dependent that can open
// what its dependency wrote.
func TestAPlanRunLeavesEachTasksFullOutputReadableByItsDependent(t *testing.T) {
	plan := mustParsePlan(t, map[string]any{
		"name": "research",
		"tasks": []any{
			map[string]any{"id": "find", "prompt": "look"},
			map[string]any{"id": "synth", "prompt": "combine", "depends_on": []any{"find"}},
		},
		"budget": map[string]any{"max_workers": float64(1)},
	}, Limits{MaxTasks: 20, ParentTools: PlanReadOnlyToolNames()})

	full := strings.Repeat("E", measuredChildReport)
	var synthPrompt string
	var synthRoots []string
	var readBack int
	var readErr error
	run := func(_ context.Context, req PlanTaskRequest) (TaskResult, error) {
		if req.Task.ID == "synth" {
			synthPrompt = req.Task.Prompt
			synthRoots = req.ReadRoots
			// READ IT HERE, which is what a real dependent does: the scratchpad
			// lives for the PLAN, and reading it after ExecutePlan returns would
			// be asserting that a disposable directory was not disposed of.
			for _, field := range strings.Fields(synthPrompt) {
				candidate := strings.Trim(field, ".,]")
				if !strings.HasSuffix(candidate, "find.md") {
					continue
				}
				var body []byte
				body, readErr = os.ReadFile(candidate)
				readBack = len(body)
				if len(synthRoots) == 1 && !strings.HasPrefix(candidate, synthRoots[0]) {
					t.Errorf("the named path %q is outside the granted root %q", candidate, synthRoots[0])
				}
			}
			return TaskResult{ID: "synth", Outcome: TaskSucceeded, Output: "done"}, nil
		}
		return TaskResult{ID: req.Task.ID, Outcome: TaskSucceeded, Output: full}, nil
	}

	report := ExecutePlan(context.Background(), plan, PlanReadOnlyToolNames(), run, nil, WithScratchpad())
	if report.Failed != 0 {
		t.Fatalf("plan failed: %+v", report)
	}
	if len(synthRoots) != 1 {
		t.Fatalf("the dependent was granted %v, want exactly the scratchpad", synthRoots)
	}
	if readErr != nil {
		t.Fatalf("the dependent could not open what it was pointed at: %v", readErr)
	}
	if readBack != measuredChildReport {
		t.Fatalf("the dependent read %d characters, the task produced %d", readBack, measuredChildReport)
	}
	// And the excerpt it was handed really was smaller than the whole answer,
	// or the pointer solved nothing.
	if strings.Count(synthPrompt, "E") >= measuredChildReport {
		t.Fatal("the excerpt was not truncated, so this proves nothing about the pointer")
	}
}

// The plan's directory does not outlive the plan.
func TestAFinishedPlanLeavesNoScratchpadBehind(t *testing.T) {
	plan := mustParsePlan(t, map[string]any{
		"name":   "research",
		"tasks":  []any{map[string]any{"id": "find", "prompt": "look"}},
		"budget": map[string]any{"max_workers": float64(1)},
	}, Limits{MaxTasks: 20, ParentTools: PlanReadOnlyToolNames()})

	var root string
	run := func(_ context.Context, req PlanTaskRequest) (TaskResult, error) {
		if len(req.ReadRoots) == 1 {
			root = req.ReadRoots[0]
		}
		return TaskResult{ID: req.Task.ID, Outcome: TaskSucceeded, Output: "x"}, nil
	}
	ExecutePlan(context.Background(), plan, PlanReadOnlyToolNames(), run, nil, WithScratchpad())

	if root == "" {
		t.Fatal("no scratchpad was created, so this proves nothing")
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("the scratchpad outlived the plan at %s", root)
	}
}

// A cancelled task's partial findings are evidence — plan_exec already says so
// where it briefs dependents — so they are kept for the same reason.
func TestACancelledTasksPartialOutputIsStillRecorded(t *testing.T) {
	pad, err := NewScratchpad("p")
	if err != nil {
		t.Fatal(err)
	}
	defer pad.Release()
	path, err := pad.Record("cut-short", "found three things before I was stopped")
	if err != nil {
		t.Fatal(err)
	}
	if path == "" {
		t.Fatal("a cut-short task's findings were discarded")
	}
	if filepath.Dir(path) != pad.Root() {
		t.Fatalf("recorded outside the scratchpad: %s", path)
	}
}
