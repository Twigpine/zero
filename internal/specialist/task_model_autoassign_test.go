package specialist

import (
	"context"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/sessions"
	"github.com/Twigpine/zero/internal/streamjson"
)

// PER-SUB-AGENT MODELS ON THE DELEGATION PATH. The plan tool routes a task to a
// role-appropriate model; a bare Task delegation used to inherit the parent's
// model unconditionally, so every sub-agent ran on the session model no matter
// what it was for. autoTaskModel closes that gap by reusing the plan path's own
// classifier and role pins — but ONLY under the zeromaxing posture with
// auto-assign configured, so nothing changes for anyone who did not ask for it.

func autoAssignPrefs() ModelPreferences {
	return ModelPreferences{
		AutoAssign: true,
		Scan:       "deepseek-v4-flash",
		Implement:  "gpt-oss:20b",
		Verify:     "kimi-k2.6",
	}
}

// delegatedManifest is what a genuine Task delegation resolves: a registry
// specialist, NOT a plan-authored manifest. The distinction is load-bearing —
// autoTaskModel keys on plan provenance ("(plan)") to avoid second-guessing the
// plan tool's own assignment, so a fixture built with planTaskManifest would be
// skipped for the wrong reason and these tests would pass against gutted code.
func delegatedManifest(tools ...string) Manifest {
	manifest := planTaskManifest("explorer", "", "", tools)
	manifest.FilePath = "explorer.yaml"
	return manifest
}

// servesEverything is a discovery stub listing the session model and every pin
// used in these tests, so the served-check passes and the routing logic itself
// is what is under test. The mismatch cases build their own narrower stubs.
func servesEverything(context.Context) ([]DiscoveredModel, error) {
	return []DiscoveredModel{
		{ID: "glm-5.2"}, {ID: "deepseek-v4-flash"}, {ID: "gpt-oss:20b"}, {ID: "kimi-k2.6"},
	}, nil
}

func TestAutoTaskModelRoutesByRole(t *testing.T) {
	write := delegatedManifest("write_file")
	readOnly := delegatedManifest("grep")

	on := func(prefs ModelPreferences) Executor {
		return Executor{PostureActive: func() bool { return true }, ModelPrefs: prefs, DiscoverModels: servesEverything}
	}

	cases := []struct {
		name     string
		executor Executor
		manifest Manifest
		prompt   string
		want     string
	}{
		// The grant outranks the prose: a write-capable task is "implement"
		// regardless of what the prompt says.
		{"write grant -> implement pin", on(autoAssignPrefs()), write, "look into the parser", "gpt-oss:20b"},
		// Read-only, so the prose decides. Verify is tested before implement, so a
		// review of a change is not sent to the coding model.
		{"read-only review -> verify pin", on(autoAssignPrefs()), readOnly, "review the auth change", "kimi-k2.6"},
		{"read-only find -> scan pin", on(autoAssignPrefs()), readOnly, "find every caller", "deepseek-v4-flash"},
		// Unclassifiable: the honest answer is to inherit, not to force a bucket.
		{"read-only neutral -> inherit", on(autoAssignPrefs()), readOnly, "hello there", ""},
		// The role classifies, but no pin exists for it: still inherit.
		{"role has no pin -> inherit", on(ModelPreferences{AutoAssign: true}), write, "do it", ""},
		// The two gates. Either one off means the whole feature is off.
		{"posture off -> inherit", Executor{PostureActive: func() bool { return false }, ModelPrefs: autoAssignPrefs(), DiscoverModels: servesEverything}, write, "do it", ""},
		{"nil posture -> inherit", Executor{ModelPrefs: autoAssignPrefs(), DiscoverModels: servesEverything}, write, "do it", ""},
		{"auto-assign off -> inherit", on(ModelPreferences{Implement: "gpt-oss:20b"}), write, "do it", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.executor.autoTaskModel(context.Background(), tc.manifest, tc.prompt, "glm-5.2"); got != tc.want {
				t.Fatalf("autoTaskModel = %q, want %q", got, tc.want)
			}
		})
	}
}

// THE JOIN, ASSERTED FROM ARGV. autoTaskModel returning the right id proves
// nothing if the id is dropped before launch — appendModelArgs is a separate
// seam, and this family of bug lives exactly in the gap between them. So this
// drives the real Run and reads the flag the child actually receives.
func TestTaskModelAutoAssignReachesChildArgv(t *testing.T) {
	var argv []string
	write := delegatedManifest("write_file")
	executor := Executor{
		BinaryPath:     "/bin/true",
		NewSessionID:   func() (string, error) { return "specialist_00000000000000000000000a", nil },
		Load:           func(LoadOptions) (LoadResult, error) { return LoadResult{}, nil },
		PostureActive:  func() bool { return true },
		ModelPrefs:     ModelPreferences{AutoAssign: true, Implement: "gpt-oss:20b"},
		DiscoverModels: servesEverything,
		RunChild: func(_ context.Context, _ string, args []string, _ func(streamjson.Event)) (ChildRunResult, error) {
			argv = args
			return ChildRunResult{Started: true}, nil
		},
	}
	if _, err := executor.Run(context.Background(),
		TaskParameters{Name: "explorer", Prompt: "refactor the parser", Manifest: &write},
		TaskRunOptions{Cwd: t.TempDir(), ParentModel: "glm-5.2"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !argsContainPair(argv, "--model", "gpt-oss:20b") {
		t.Fatalf("the auto-assigned model never reached the child argv:\n%s", strings.Join(argv, " "))
	}
}

// A PIN THE PROVIDER DOES NOT SERVE IS NEVER APPLIED — the failure this exists
// for was real: the session moved to xai while the pins named Ollama models,
// and three children died at spawn with `"not-found": The model kimi-k2.6 does
// not exist`. Every uncertain case inherits: an unserved pin, a served list the
// session's own model is missing from (discovery answering for a different
// provider), a failed listing, and no discoverer at all.
func TestAnUnservedPinIsNeverApplied(t *testing.T) {
	write := delegatedManifest("write_file")
	base := Executor{PostureActive: func() bool { return true },
		ModelPrefs: ModelPreferences{AutoAssign: true, Implement: "gpt-oss:20b"}}

	xaiOnly := func(context.Context) ([]DiscoveredModel, error) {
		return []DiscoveredModel{{ID: "grok-4.5"}, {ID: "grok-4.3"}}, nil
	}
	cases := []struct {
		name     string
		discover ModelDiscoverer
		parent   string
	}{
		{"pin not served by this provider", xaiOnly, "grok-4.5"},
		{"session model missing from the list (wrong provider answered)", servesEverything, "grok-4.5"},
		{"discovery failed", func(context.Context) ([]DiscoveredModel, error) {
			return nil, context.DeadlineExceeded
		}, "glm-5.2"},
		{"no discoverer wired", nil, "glm-5.2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			executor := base
			executor.DiscoverModels = tc.discover
			if got := executor.autoTaskModel(context.Background(), write, "refactor it", tc.parent); got != "" {
				t.Fatalf("a pin was applied blind: %q", got)
			}
		})
	}
}

// The serve cache holds one listing for the fan-out: five spawns, one probe.
func TestTheServeCacheProbesOnce(t *testing.T) {
	calls := 0
	executor := Executor{
		PostureActive: func() bool { return true },
		ModelPrefs:    ModelPreferences{AutoAssign: true, Implement: "gpt-oss:20b"},
		ServeCache:    &ModelServeCache{},
		DiscoverModels: func(ctx context.Context) ([]DiscoveredModel, error) {
			calls++
			return servesEverything(ctx)
		},
	}
	write := delegatedManifest("write_file")
	for i := 0; i < 5; i++ {
		if got := executor.autoTaskModel(context.Background(), write, "refactor it", "glm-5.2"); got != "gpt-oss:20b" {
			t.Fatalf("spawn %d: pin = %q, want gpt-oss:20b", i, got)
		}
	}
	if calls != 1 {
		t.Fatalf("discovery ran %d times for five spawns, want 1", calls)
	}
}

// ADDITIVITY. Posture off, the spawn must be exactly what it was before this
// existed: no --model at all, inheriting the parent's model downstream.
func TestTaskModelAutoAssignOffEmitsNoModel(t *testing.T) {
	var argv []string
	write := delegatedManifest("write_file")
	executor := Executor{
		BinaryPath:    "/bin/true",
		NewSessionID:  func() (string, error) { return "specialist_00000000000000000000000a", nil },
		Load:          func(LoadOptions) (LoadResult, error) { return LoadResult{}, nil },
		PostureActive: func() bool { return false },
		ModelPrefs:    ModelPreferences{AutoAssign: true, Implement: "gpt-oss:20b"},
		RunChild: func(_ context.Context, _ string, args []string, _ func(streamjson.Event)) (ChildRunResult, error) {
			argv = args
			return ChildRunResult{Started: true}, nil
		},
	}
	if _, err := executor.Run(context.Background(),
		TaskParameters{Name: "explorer", Prompt: "refactor the parser", Manifest: &write},
		TaskRunOptions{Cwd: t.TempDir()}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if argsHaveFlag(argv, "--model") {
		t.Fatalf("a posture-off spawn emitted --model — additivity is broken:\n%s", strings.Join(argv, " "))
	}
}

// An explicit model on the call always wins over the role pin: auto-assignment
// fills the empty case only.
func TestExplicitTaskModelBeatsAutoAssign(t *testing.T) {
	var argv []string
	write := delegatedManifest("write_file")
	executor := Executor{
		BinaryPath:    "/bin/true",
		NewSessionID:  func() (string, error) { return "specialist_00000000000000000000000a", nil },
		Load:          func(LoadOptions) (LoadResult, error) { return LoadResult{}, nil },
		PostureActive: func() bool { return true },
		ModelPrefs:    ModelPreferences{AutoAssign: true, Implement: "gpt-oss:20b"},
		RunChild: func(_ context.Context, _ string, args []string, _ func(streamjson.Event)) (ChildRunResult, error) {
			argv = args
			return ChildRunResult{Started: true}, nil
		},
	}
	if _, err := executor.Run(context.Background(),
		TaskParameters{Name: "explorer", Prompt: "refactor the parser", Model: "claude-sonnet-4.5", Manifest: &write},
		TaskRunOptions{Cwd: t.TempDir()}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !argsContainPair(argv, "--model", "claude-sonnet-4.5") {
		t.Fatalf("the explicit model was overridden by the role pin:\n%s", strings.Join(argv, " "))
	}
	if argsContainPair(argv, "--model", "gpt-oss:20b") {
		t.Fatalf("the role pin leaked past an explicit model:\n%s", strings.Join(argv, " "))
	}
}

// A specialist that DECLARES its own model keeps it: auto-assignment must not
// override a choice the manifest already made.
func TestManifestOwnModelSurvivesAutoAssign(t *testing.T) {
	var argv []string
	declared := delegatedManifest("write_file")
	declared.Metadata.Model = "claude-sonnet-4.5"
	executor := Executor{
		BinaryPath:    "/bin/true",
		NewSessionID:  func() (string, error) { return "specialist_00000000000000000000000a", nil },
		Load:          func(LoadOptions) (LoadResult, error) { return LoadResult{}, nil },
		PostureActive: func() bool { return true },
		ModelPrefs:    ModelPreferences{AutoAssign: true, Implement: "gpt-oss:20b"},
		RunChild: func(_ context.Context, _ string, args []string, _ func(streamjson.Event)) (ChildRunResult, error) {
			argv = args
			return ChildRunResult{Started: true}, nil
		},
	}
	if _, err := executor.Run(context.Background(),
		TaskParameters{Name: "explorer", Prompt: "refactor the parser", Manifest: &declared},
		TaskRunOptions{Cwd: t.TempDir()}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !argsContainPair(argv, "--model", "claude-sonnet-4.5") {
		t.Fatalf("auto-assignment overrode the manifest's own model:\n%s", strings.Join(argv, " "))
	}
}

// A PLAN TASK'S MODEL IS DECIDED ONCE, BY THE PLAN TOOL. The plan path runs its
// own assignment — router, pins, served-check, probes — honours a per-plan
// auto_assign override, and reports every decision in its notes. A plan task
// arriving at the executor with no model is therefore a DECISION (inherit), not
// an absence, and the Task-path pre-step must not second-guess it: doing so
// would contradict the plan's report, defeat an explicit auto_assign:false, and
// re-apply a pin the plan level passed over as not served by this provider.
func TestAPlanTaskIsNeverSecondGuessedByTaskAutoAssign(t *testing.T) {
	var argv []string
	executor := Executor{
		BinaryPath:    "/bin/true",
		NewSessionID:  func() (string, error) { return "specialist_00000000000000000000000a", nil },
		Load:          func(LoadOptions) (LoadResult, error) { return LoadResult{}, nil },
		PostureActive: func() bool { return true },
		ModelPrefs:    autoAssignPrefs(),
		RunChild: func(_ context.Context, _ string, args []string, _ func(streamjson.Event)) (ChildRunResult, error) {
			argv = args
			return ChildRunResult{Started: true}, nil
		},
	}
	run := NewPlanRunner(PlanTaskContext{Executor: executor, Cwd: t.TempDir(), SpecialistName: "explorer"})
	if _, err := run(context.Background(), PlanTaskRequest{
		// A write grant + an implement-classifying prompt: the strongest bait the
		// pre-step could take. The plan decided "inherit" by assigning no model.
		Task:  Task{ID: "t", Prompt: "refactor the parser"},
		Tools: []string{"write_file", "read_file"},
	}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if argsHaveFlag(argv, "--model") {
		t.Fatalf("the plan's model decision was second-guessed at dispatch:\n%s", strings.Join(argv, " "))
	}
}

// A RESUMED TASK KEEPS THE MODEL IT RAN ON. BuildResumeArgs falls back to the
// parent's model when the manifest names none, so a task launched on an explicit
// or auto-assigned model used to come back from a resume on the parent's — the
// same drift BuildResumeArgsInput's own comment records, one layer up. The child
// recorded its resolved model in the session store at launch; resume must read it.
func TestAResumedTaskKeepsTheModelItRanOn(t *testing.T) {
	store := freshStore(t)
	session, err := store.Create(freshSessionInput(t, "gpt-oss:20b"))
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	var argv []string
	executor := resumeExecutor(store, &argv)
	if _, err := executor.Run(context.Background(),
		TaskParameters{Prompt: "carry on", Resume: session.SessionID},
		TaskRunOptions{Cwd: t.TempDir(), ParentModel: "glm-5.2"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !argsContainPair(argv, "--model", "gpt-oss:20b") {
		t.Fatalf("the resumed child lost the model its session ran on:\n%s", strings.Join(argv, " "))
	}
	if argsContainPair(argv, "--model", "glm-5.2") {
		t.Fatalf("the resumed child drifted to the parent's model:\n%s", strings.Join(argv, " "))
	}
}

// An explicit model on the resume call wins over the session's recorded one,
// exactly as it wins on a fresh launch — the two doors must not disagree.
func TestAnExplicitModelOnResumeWins(t *testing.T) {
	store := freshStore(t)
	session, err := store.Create(freshSessionInput(t, "gpt-oss:20b"))
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	var argv []string
	executor := resumeExecutor(store, &argv)
	if _, err := executor.Run(context.Background(),
		TaskParameters{Prompt: "carry on", Resume: session.SessionID, Model: "claude-sonnet-4.5"},
		TaskRunOptions{Cwd: t.TempDir(), ParentModel: "glm-5.2"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !argsContainPair(argv, "--model", "claude-sonnet-4.5") {
		t.Fatalf("the explicit resume model was ignored:\n%s", strings.Join(argv, " "))
	}
}

// A session that recorded no model resumes exactly as before: the parent's
// model, via appendModelArgs' own fallback.
func TestAResumedTaskWithNoRecordedModelStillInheritsTheParent(t *testing.T) {
	store := freshStore(t)
	session, err := store.Create(freshSessionInput(t, ""))
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	var argv []string
	executor := resumeExecutor(store, &argv)
	if _, err := executor.Run(context.Background(),
		TaskParameters{Prompt: "carry on", Resume: session.SessionID},
		TaskRunOptions{Cwd: t.TempDir(), ParentModel: "glm-5.2"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !argsContainPair(argv, "--model", "glm-5.2") {
		t.Fatalf("the no-record fallback to the parent's model broke:\n%s", strings.Join(argv, " "))
	}
}

func freshStore(t *testing.T) *sessions.Store {
	t.Helper()
	return sessions.NewStore(sessions.StoreOptions{RootDir: t.TempDir()})
}

func freshSessionInput(t *testing.T, modelID string) sessions.CreateInput {
	t.Helper()
	return sessions.CreateInput{
		SessionID:   "specialist_00000000000000000000000a",
		SessionKind: sessions.SessionKindChild,
		Cwd:         t.TempDir(),
		AgentName:   "explorer",
		Tag:         sessionTagSpecialist,
		ModelID:     modelID,
	}
}

func resumeExecutor(store *sessions.Store, argv *[]string) Executor {
	return Executor{
		BinaryPath:   "/bin/true",
		SessionStore: store,
		Load: func(LoadOptions) (LoadResult, error) {
			return LoadResult{Specialists: []Manifest{{
				Metadata:      Metadata{Name: "explorer"},
				SystemPrompt:  "x",
				ResolvedTools: []string{"grep"},
				ToolsResolved: true,
				Location:      LocationBuiltin,
			}}}, nil
		},
		RunChild: func(_ context.Context, _ string, args []string, _ func(streamjson.Event)) (ChildRunResult, error) {
			*argv = args
			return ChildRunResult{Started: true}, nil
		},
	}
}

// EFFORT IS FORWARDED ONLY TO MODELS THE REGISTRY CAN VOUCH FOR — the same
// gate the plan path applies. The child clamps a forwarded effort only for
// models it can look up; anything else passes it to the provider untouched,
// and a provider that does not take the parameter rejects the whole request.
// Under zeromaxing the parent's effort is always raised, so the unconditional
// forward made every Task naming an uncurated model on such a provider DIE AT
// SPAWN — a real orchestrator hit it, gave up, and ran five sub-agents on the
// session's model.
func TestEffortIsNotForwardedToUncuratedTaskModels(t *testing.T) {
	launch := func(model string) []string {
		var argv []string
		manifest := delegatedManifest("grep")
		executor := Executor{
			BinaryPath:   "/bin/true",
			NewSessionID: func() (string, error) { return "specialist_00000000000000000000000a", nil },
			Load:         func(LoadOptions) (LoadResult, error) { return LoadResult{}, nil },
			RunChild: func(_ context.Context, _ string, args []string, _ func(streamjson.Event)) (ChildRunResult, error) {
				argv = args
				return ChildRunResult{Started: true}, nil
			},
		}
		if _, err := executor.Run(context.Background(),
			TaskParameters{Name: "explorer", Prompt: "go", Model: model, Manifest: &manifest},
			TaskRunOptions{Cwd: t.TempDir(), ParentReasoningEffort: "high"}); err != nil {
			t.Fatalf("Run(%s): %v", model, err)
		}
		return argv
	}
	uncurated := launch("gpt-oss:20b")
	if !argsContainPair(uncurated, "--model", "gpt-oss:20b") {
		t.Fatalf("setup: the model must reach argv:\n%s", strings.Join(uncurated, " "))
	}
	if argsHaveFlag(uncurated, "--reasoning-effort") {
		t.Fatalf("an uncurated model was sent an effort the provider may reject:\n%s", strings.Join(uncurated, " "))
	}
	curated := launch("claude-sonnet-4.5")
	if !argsContainPair(curated, "--reasoning-effort", "high") {
		t.Fatalf("a curated model must keep the parent's raised effort:\n%s", strings.Join(curated, " "))
	}
}

func argsHaveFlag(args []string, flag string) bool {
	for _, arg := range args {
		if arg == flag {
			return true
		}
	}
	return false
}
