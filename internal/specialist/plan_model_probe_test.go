package specialist

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Twigpine/zero/internal/streamjson"
	"github.com/Twigpine/zero/internal/tools"
)

// THE PROVIDER'S OWN REFUSALS, CLASSIFIED. These are the exact messages that
// killed real plans this week.
func TestAProviderRefusalIsDistinguishedFromAnUnreachableProvider(t *testing.T) {
	refusals := []string{
		`{"code":"not-found","error":"The model deepseek-v4-flash does not exist or your team does not have access to it."}`,
		`provider request error: "Multi Agent requests are not allowed on chat completions"`,
		`model_not_found`,
		`unknown model: banana`,
	}
	for _, message := range refusals {
		got := ClassifyProbeError(errors.New(message))
		if got.Verdict != ProbeRefuses {
			t.Errorf("a refusal was not recognised: %q -> %v", message, got.Verdict)
		}
		if got.Reason == "" {
			t.Errorf("a refusal kept no reason to report: %q", message)
		}
	}

	// FAIL TOWARD KEEPING THE MODEL. Anything not recognised changes nothing:
	// being wrong here costs one failed task, while being wrong the other way
	// silently removes a model the user pays for.
	for _, message := range []string{
		"context deadline exceeded",
		"connection refused",
		"429 too many requests",
		"internal server error",
	} {
		if got := ClassifyProbeError(errors.New(message)); got.Verdict != ProbeUnknown {
			t.Errorf("a transient failure was read as a refusal: %q -> %v", message, got.Verdict)
		}
	}

	if got := ClassifyProbeError(nil); got.Verdict != ProbeServes {
		t.Errorf("a successful probe was not read as serving: %v", got.Verdict)
	}
}

// A MODEL THE PROVIDER WILL NOT RUN IS NEVER OFFERED — before tiers, the served
// set or the router's list are built from it.
func TestAModelThatFailsItsProbeIsNotOfferedToAnyTask(t *testing.T) {
	discovered := []DiscoveredModel{
		{ID: "grok-code-fast", ToolCall: true, InputCost: 1},
		{ID: "grok-4.20-multi-agent-0309", ToolCall: true, InputCost: 4},
		{ID: "grok-4.5", ToolCall: true, InputCost: 9},
	}
	tool := &OrchestrateTool{
		DiscoverModels: func(context.Context) ([]DiscoveredModel, error) { return discovered, nil },
		ProbeModel: func(_ context.Context, id string) ModelProbeResult {
			if id == "grok-4.20-multi-agent-0309" {
				return ClassifyProbeError(errors.New(`"Multi Agent requests are not allowed on chat completions"`))
			}
			return ModelProbeResult{Verdict: ProbeServes}
		},
		ModelPrefs: ModelPreferences{AutoAssign: true},
	}
	args := map[string]any{"tasks": []any{
		map[string]any{"id": "a", "prompt": "list the files"},
		map[string]any{"id": "b", "prompt": "audit it and judge whether it holds"},
	}}

	notes, err := tool.autoAssignModels(context.Background(), args, tools.RunOptions{Model: "grok-4.5"})
	if err != nil {
		t.Fatalf("auto-assign: %v", err)
	}
	for _, entry := range args["tasks"].([]any) {
		if model := planString(entry.(map[string]any), "model"); model == "grok-4.20-multi-agent-0309" {
			t.Errorf("a task was assigned a model the provider refuses: %v", entry)
		}
	}
	// SAID, NOT SILENT — this is the id the user would otherwise add to their
	// exclude list by hand, after a plan died on it.
	joined := strings.Join(notes, " ")
	if !strings.Contains(joined, "grok-4.20-multi-agent-0309") || !strings.Contains(joined, "will not run it") {
		t.Errorf("the dropped model was not reported: %v", notes)
	}
	if !strings.Contains(joined, "Multi Agent requests are not allowed") {
		t.Errorf("the provider's own reason was not carried into the note: %v", notes)
	}
}

// ONCE PER MODEL, NOT ONCE PER PLAN. The guarantee is cheap only if it is paid
// for once; per-plan probing turns it into a tax on every plan in a session.
func TestAModelIsProbedOnceAndRememberedForTheSession(t *testing.T) {
	var probes atomic.Int64
	tool := &OrchestrateTool{
		DiscoverModels: func(context.Context) ([]DiscoveredModel, error) {
			return []DiscoveredModel{{ID: "a", ToolCall: true, InputCost: 1}, {ID: "b", ToolCall: true, InputCost: 2}}, nil
		},
		ProbeModel: func(context.Context, string) ModelProbeResult {
			probes.Add(1)
			return ModelProbeResult{Verdict: ProbeServes}
		},
		ModelPrefs: ModelPreferences{AutoAssign: true},
	}
	for round := 0; round < 3; round++ {
		args := map[string]any{"tasks": []any{map[string]any{"id": "t", "prompt": "look"}}}
		if _, err := tool.autoAssignModels(context.Background(), args, tools.RunOptions{Model: "a"}); err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
	}
	if got := probes.Load(); got != 2 {
		t.Errorf("expected one probe per model for the session, got %d across three plans", got)
	}
}

// AN UNREACHABLE PROBE MUST NOT BE REMEMBERED. It records a failure to ask, not
// an answer; caching it would let one flaky moment exclude a working model for
// the rest of the session.
func TestAnUnknownVerdictIsNotCached(t *testing.T) {
	cache := &probeCache{}
	cache.put("provider-a", "m", ModelProbeResult{Verdict: ProbeUnknown, Reason: "timeout"})
	if _, ok := cache.get("provider-a", "m"); ok {
		t.Error("a failure to reach the provider was remembered as an answer")
	}
	cache.put("provider-a", "m", ModelProbeResult{Verdict: ProbeRefuses, Reason: "no such model"})
	if _, ok := cache.get("provider-a", "m"); !ok {
		t.Error("a real verdict was not remembered")
	}
}

func TestProbeCacheSeparatesProvidersAdvertisingTheSameModel(t *testing.T) {
	cache := &probeCache{}
	cache.put("provider-a|https://a.example/v1", "shared-model", ModelProbeResult{Verdict: ProbeRefuses})
	cache.put("provider-b|https://b.example/v1", "shared-model", ModelProbeResult{Verdict: ProbeServes})

	if got, ok := cache.get("provider-a|https://a.example/v1", "shared-model"); !ok || got.Verdict != ProbeRefuses {
		t.Fatalf("provider A verdict = %#v, %v", got, ok)
	}
	if got, ok := cache.get("provider-b|https://b.example/v1", "shared-model"); !ok || got.Verdict != ProbeServes {
		t.Fatalf("provider B verdict = %#v, %v", got, ok)
	}
}

func TestSwitchingProviderReprobesAnOverlappingModelID(t *testing.T) {
	cache := &probeCache{}
	models := []DiscoveredModel{{ID: "shared-model"}}
	probes := 0
	probe := func(context.Context, string) ModelProbeResult {
		probes++
		if probes == 1 {
			return ModelProbeResult{Verdict: ProbeRefuses, Reason: "provider A refuses"}
		}
		return ModelProbeResult{Verdict: ProbeServes}
	}

	proveModels(context.Background(), models, probe, cache, "profile-a|https://a.example/v1")
	proveModels(context.Background(), models, probe, cache, "profile-b|https://b.example/v1")
	if probes != 2 {
		t.Fatalf("provider B reused provider A's verdict; prober called %d time(s)", probes)
	}
}

// EVERY CANDIDATE REFUSED POINTS AT THE PROVIDER, NOT THE MODELS. A credential
// or endpoint problem rejects the whole list, and dropping all of them would
// disable routing silently at the moment the user most needs telling.
func TestWhenEveryModelFailsItsProbeTheListIsKeptAndTheUserIsTold(t *testing.T) {
	models := []DiscoveredModel{{ID: "a"}, {ID: "b"}}
	kept, notes := proveModels(context.Background(), models,
		func(context.Context, string) ModelProbeResult {
			return ClassifyProbeError(errors.New("the model does not exist"))
		}, &probeCache{})
	if len(kept) != len(models) {
		t.Fatalf("a whole-provider failure emptied the candidate list: %d kept", len(kept))
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "points at this provider") {
		t.Errorf("the user was not told the provider looks wrong: %v", notes)
	}
}

// No prober wired means no proving — exactly what every plan did before.
func TestWithoutAProberEveryDiscoveredModelIsStillOffered(t *testing.T) {
	models := []DiscoveredModel{{ID: "a"}, {ID: "b"}}
	kept, notes := proveModels(context.Background(), models, nil, &probeCache{})
	if len(kept) != 2 || len(notes) != 0 {
		t.Errorf("an unwired prober changed the candidate list: %d kept, notes %v", len(kept), notes)
	}
}

// A TASK THAT DECLINED GETS ONE MORE ATTEMPT. A wrong answer does not.
//
// From a real plan: a-fsutil said it could not find a directory that existed and
// that its three sibling tasks read without trouble. That single refusal cost the
// task, its dependent, and the final report — a third of the plan — and the
// parent then did the work itself, serially, losing the parallelism the plan was
// for.
func TestATaskThatDeclinedIsRetriedOnce(t *testing.T) {
	var attempts atomic.Int64
	exec := Executor{
		BinaryPath:   "/bin/true",
		NewSessionID: func() (string, error) { return "specialist_00000000000000000000000a", nil },
		Load:         func(LoadOptions) (LoadResult, error) { return LoadResult{}, nil },
		RunChild: func(_ context.Context, _ string, _ []string, progress func(streamjson.Event)) (ChildRunResult, error) {
			round := attempts.Add(1)
			if round == 1 {
				// The child exits with the "work unfinished" code.
				events := []streamjson.Event{{Type: streamjson.EventError, Message: `the final message admits the objective was not met ("i cannot …")`}}
				for _, e := range events {
					if progress != nil {
						progress(e)
					}
				}
				return ChildRunResult{Started: true, ExitCode: 4, Events: events}, nil
			}
			if progress != nil {
				progress(streamjson.Event{Type: streamjson.EventToolCall, Name: "read_file"})
			}
			return ChildRunResult{Started: true}, nil
		},
	}
	plan := mustPlan(t, []any{task("a-fsutil", "audit the fsutil package")}, okBudget(), readOnlyLimits())
	run := NewPlanRunner(PlanTaskContext{Executor: exec, Cwd: t.TempDir(), SpecialistName: "explorer"})
	report := ExecutePlan(context.Background(), plan, []string{"read_file"}, run, nil)

	if got := attempts.Load(); got != 2 {
		t.Fatalf("a declined task was not retried: %d attempt(s)", got)
	}
	if report.Succeeded != 1 {
		t.Fatalf("the retry did not rescue the task: %+v", report.Tasks)
	}
	if report.Tasks[0].Attempts != 2 {
		t.Errorf("the second attempt was not recorded: %d", report.Tasks[0].Attempts)
	}
}

// BOUNDED AT ONE. A model that declines twice is telling us about the task, not
// having a bad moment — and an unbounded retry is a spend cycle.
func TestARepeatedlyDecliningTaskIsRetriedOnlyOnce(t *testing.T) {
	var attempts atomic.Int64
	exec := Executor{
		BinaryPath:   "/bin/true",
		NewSessionID: func() (string, error) { return "specialist_00000000000000000000000a", nil },
		Load:         func(LoadOptions) (LoadResult, error) { return LoadResult{}, nil },
		RunChild: func(_ context.Context, _ string, _ []string, progress func(streamjson.Event)) (ChildRunResult, error) {
			attempts.Add(1)
			events := []streamjson.Event{{Type: streamjson.EventError, Message: "i cannot do this"}}
			if progress != nil {
				progress(events[0])
			}
			return ChildRunResult{Started: true, ExitCode: 4, Events: events}, nil
		},
	}
	plan := mustPlan(t, []any{task("a", "do it")}, okBudget(), readOnlyLimits())
	run := NewPlanRunner(PlanTaskContext{Executor: exec, Cwd: t.TempDir(), SpecialistName: "explorer"})
	report := ExecutePlan(context.Background(), plan, []string{"read_file"}, run, nil)

	if got := attempts.Load(); got != 2 {
		t.Fatalf("the decline retry is not bounded at one: %d attempts", got)
	}
	if report.Failed != 1 {
		t.Errorf("a task that declined twice should still fail: %+v", report.Tasks)
	}
}

// A WRONG ANSWER IS STILL NOT RETRIED. The existing rule holds: the child ran and
// reported, and running it again buys the same report.
func TestATaskThatFailedWithARealErrorIsStillNotRetried(t *testing.T) {
	var attempts atomic.Int64
	exec := Executor{
		BinaryPath:   "/bin/true",
		NewSessionID: func() (string, error) { return "specialist_00000000000000000000000a", nil },
		Load:         func(LoadOptions) (LoadResult, error) { return LoadResult{}, nil },
		RunChild: func(_ context.Context, _ string, _ []string, progress func(streamjson.Event)) (ChildRunResult, error) {
			attempts.Add(1)
			spent := 4000
			events := []streamjson.Event{
				{Type: streamjson.EventToolCall, Name: "read_file"},
				{Type: streamjson.EventUsage, TotalTokens: &spent},
				{Type: streamjson.EventError, Message: "the file it needed does not exist"},
			}
			for _, e := range events {
				if progress != nil {
					progress(e)
				}
			}
			// Exit 1: a real failure, not a decline.
			return ChildRunResult{Started: true, ExitCode: 1, Events: events}, nil
		},
	}
	plan := mustPlan(t, []any{task("a", "do it")}, okBudget(), readOnlyLimits())
	run := NewPlanRunner(PlanTaskContext{Executor: exec, Cwd: t.TempDir(), SpecialistName: "explorer"})
	ExecutePlan(context.Background(), plan, []string{"read_file"}, run, nil)

	if got := attempts.Load(); got != 1 {
		t.Errorf("a genuine failure was retried %d times", got)
	}
}

// THE FAN-OUT IS BOUNDED IN WIDTH, not only in time.
//
// The comment said "CONCURRENT AND BOUNDED" while the loop started one goroutine
// and one provider request per unproven model with no limit. A provider listing
// hundreds of models answered the burst with 429s, which classify as
// ProbeUnknown — so the probe paid for every request, learned nothing, and left
// the plan's real tasks throttled behind it.
func TestProbingManyModelsStaysUnderTheConcurrencyCap(t *testing.T) {
	models := make([]DiscoveredModel, 200)
	for i := range models {
		models[i] = DiscoveredModel{ID: fmt.Sprintf("model-%03d", i)}
	}

	var mu sync.Mutex
	inFlight, peak := 0, 0
	probe := func(context.Context, string) ModelProbeResult {
		mu.Lock()
		inFlight++
		if inFlight > peak {
			peak = inFlight
		}
		mu.Unlock()
		// Long enough that a genuinely unbounded fan-out overlaps here.
		time.Sleep(2 * time.Millisecond)
		mu.Lock()
		inFlight--
		mu.Unlock()
		return ModelProbeResult{Verdict: ProbeServes}
	}

	kept, notes := proveModels(context.Background(), models, probe, &probeCache{})

	mu.Lock()
	got := peak
	mu.Unlock()
	if got > maxProbesInFlight {
		t.Fatalf("peak concurrent probes = %d, cap is %d: %d models produced a burst on the session's own key", got, maxProbesInFlight, len(models))
	}
	if got < 2 {
		t.Fatalf("peak concurrent probes = %d: the probe serialised, so proving a catalogue costs one round trip per model", got)
	}
	if len(kept) != len(models) {
		t.Fatalf("kept %d of %d models that all answered", len(kept), len(models))
	}
	if len(notes) != 0 {
		t.Fatalf("models that answered produced removal notes: %v", notes)
	}
}

// A MODEL BILLED SEPARATELY AND UNAFFORDABLE IS A REFUSAL TOO.
//
// The message is verbatim from a real session (2026-08-03, ollama-cloud). The
// model EXISTS and discovery reports it, so nothing structural distinguishes it
// — and because ranking is by price it ranks highest, so auto-assign chose it on
// four separate days across eight plan tasks. Each died in about seven seconds
// with zero tokens and zero tool calls, costing the plan a dispatch and a retry
// cycle every time.
//
// Classified here, before assignment, the model is dropped instead of assigned.
func TestAModelTheAccountCannotAffordIsRefusedBeforeItIsAssigned(t *testing.T) {
	const verbatim = `provider request error: {"error":"this model uses extra usage only (not included plan usage) and your extra usage balance is empty, add extra usage or turn on auto reload at https://ollama.com/settings (ref: d62a7217-60fb-48ad-b9c1-766b717ec5eb)"}`

	got := ClassifyProbeError(errors.New(verbatim))
	if got.Verdict != ProbeRefuses {
		t.Fatalf("the billing refusal was not recognised: %v", got.Verdict)
	}
	// The reason must survive: a model vanishing from routing with no
	// explanation is indistinguishable from a bug, which is why proveModels
	// reports one per removal.
	if !strings.Contains(got.Reason, "extra usage") {
		t.Fatalf("the refusal kept no usable reason: %q", got.Reason)
	}

	// Each marker on its own, so a provider rewording part of the sentence still
	// trips at least one.
	for _, message := range []string{
		"this model uses extra usage only",
		"not included plan usage",
		"your extra usage balance is empty",
	} {
		if verdict := ClassifyProbeError(errors.New(message)).Verdict; verdict != ProbeRefuses {
			t.Errorf("marker not matched: %q -> %v", message, verdict)
		}
	}

	// STILL FAILING TOWARD KEEPING THE MODEL. A balance NOTICE is not a refusal:
	// the request succeeded and the model works, so removing it would drop a
	// model the user is paying for. This is why the marker is "extra usage only"
	// rather than a bare "extra usage".
	for _, message := range []string{
		"you have $5.00 of extra usage remaining",
		"add extra usage to raise your limit",
		"extra usage enabled for this account",
	} {
		if verdict := ClassifyProbeError(errors.New(message)).Verdict; verdict != ProbeUnknown {
			t.Errorf("a balance notice was read as a refusal: %q -> %v", message, verdict)
		}
	}
}

// AND THE REFUSAL MUST REACH THE ASSIGNER, not merely the classifier. A verdict
// nothing consults leaves the model in the ranking and the plan assigns it
// anyway — the "layer B does not carry it" defect this package keeps producing.
func TestAnUnaffordableModelIsRemovedFromTheCandidatesEntirely(t *testing.T) {
	const billing = `{"error":"this model uses extra usage only (not included plan usage) and your extra usage balance is empty"}`
	models := []DiscoveredModel{
		// Ranked highest by price, which is exactly why auto-assign reached for it.
		{ID: "kimi-k3", ToolCall: true, InputCost: 90, OutputCost: 900, OutputModalities: []string{"text"}},
		{ID: "glm-5.2", ToolCall: true, InputCost: 1, OutputCost: 3, OutputModalities: []string{"text"}},
	}
	probe := func(_ context.Context, id string) ModelProbeResult {
		if id == "kimi-k3" {
			return ClassifyProbeError(errors.New(billing))
		}
		return ModelProbeResult{Verdict: ProbeServes}
	}

	survivors, notes := proveModels(context.Background(), models, probe, &probeCache{})
	for _, model := range survivors {
		if model.ID == "kimi-k3" {
			t.Fatal("the unaffordable model survived probing and is still assignable")
		}
	}
	if len(survivors) != 1 || survivors[0].ID != "glm-5.2" {
		t.Fatalf("probing removed the wrong models: %+v", survivors)
	}
	if len(notes) == 0 {
		t.Fatal("a model vanished from routing with no note: indistinguishable from a bug")
	}
	if !strings.Contains(strings.Join(notes, " "), "kimi-k3") {
		t.Fatalf("the note does not name the model that was removed: %v", notes)
	}
}
