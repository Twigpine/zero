package specialist

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/tools"
)

func discovered() []DiscoveredModel {
	return []DiscoveredModel{
		{ID: "claude-opus-4.1", ToolCall: true, Reasoning: true, InputCost: 15, OutputCost: 75},
		{ID: "gpt-4.1-nano", ToolCall: true, InputCost: 0.1, OutputCost: 0.4},
		{ID: "claude-sonnet-4.5", ToolCall: true, InputCost: 3, OutputCost: 15},
		{ID: "gpt-4.1-mini", ToolCall: false, InputCost: 0.01},
		{ID: "some-uncurated-proxy-model", ToolCall: true, InputCost: 0.001},
	}
}

// Tiers rank by PRICE, and a model that cannot call a tool is not eligible at
// all — a plan task that cannot call a tool cannot do a plan task's job.
func TestTiersRankByCostAndRequireToolCalling(t *testing.T) {
	tiers := buildModelTiers(discovered(), ModelPreferences{})
	if tiers.strong != "claude-opus-4.1" {
		t.Errorf("strong = %q", tiers.strong)
	}
	if tiers.strong != "claude-opus-4.1" {
		t.Errorf("strong = %q", tiers.strong)
	}
	if tiers.cheap == "gpt-4.1-mini" || tiers.balanced == "gpt-4.1-mini" || tiers.strong == "gpt-4.1-mini" {
		t.Errorf("a model that cannot call tools was made eligible: %+v", tiers)
	}
	// An UNCURATED model IS eligible — discovery asked the provider what it
	// serves, and that is the authority. Requiring curation as well made
	// auto-assign do nothing on an xAI or Ollama account.
	if tiers.cheap != "some-uncurated-proxy-model" {
		t.Errorf("cheap = %q; the cheapest model the provider serves must be eligible even when uncurated", tiers.cheap)
	}

	// One model means every task gets it — today's behaviour, not a failure.
	single := buildModelTiers([]DiscoveredModel{{ID: "gpt-4.1", ToolCall: true}}, ModelPreferences{})
	if single.cheap != "gpt-4.1" || single.balanced != "gpt-4.1" || single.strong != "gpt-4.1" {
		t.Errorf("a single-model provider must fill every tier: %+v", single)
	}

	// A provider that publishes a bare id list with no capabilities at all is
	// USABLE: "cannot call tools" and "said nothing" are the same bool, and
	// requiring the flag outright made auto_assign silently do nothing on every
	// such provider. The parent is calling tools on it already.
	if got := buildModelTiers([]DiscoveredModel{{ID: "gpt-4.1", ToolCall: false}}, ModelPreferences{}); got == (modelTiers{}) {
		t.Error("a provider that reports no capabilities at all must still be assignable")
	}
	// But when SOME model claims tool calling, the ones that do not are excluded
	// — there the flag is real information rather than an absent field.
	mixed := buildModelTiers([]DiscoveredModel{
		{ID: "gpt-4.1-nano", ToolCall: true, InputCost: 1},
		{ID: "gpt-4o", ToolCall: false, InputCost: 0.01},
	}, ModelPreferences{})
	if mixed.cheap == "gpt-4o" {
		t.Errorf("a model that explicitly cannot call tools was chosen over one that can: %+v", mixed)
	}
	// Nothing at all still yields nothing.
	if got := buildModelTiers(nil, ModelPreferences{}); got != (modelTiers{}) {
		t.Errorf("no models must yield no tiers, got %+v", got)
	}
}

// The strong tier prefers a REASONING model when the priciest is not one —
// verify is what that tier exists for.
func TestTheStrongTierPrefersAReasoningModel(t *testing.T) {
	tiers := buildModelTiers([]DiscoveredModel{
		{ID: "gpt-4.1-nano", ToolCall: true, InputCost: 1},
		{ID: "claude-opus-4.1", ToolCall: true, Reasoning: true, InputCost: 5},
		{ID: "gpt-4o", ToolCall: true, InputCost: 20},
	}, ModelPreferences{})
	if tiers.strong != "claude-opus-4.1" {
		t.Errorf("strong = %q, want the reasoning model over the merely expensive one", tiers.strong)
	}
}

// AN EXPLICIT MODEL IS NEVER OVERRIDDEN, and assignment works on the ARGS so an
// assigned model is indistinguishable from a hand-written one downstream.
func TestAssignmentFillsOnlyTasksThatNamedNoModel(t *testing.T) {
	tasks := []any{
		map[string]any{"id": "scan", "prompt": "find every caller"},
		map[string]any{"id": "judge", "prompt": "review the result"},
		map[string]any{"id": "mine", "prompt": "find things", "model": "claude-haiku-4.5"},
		map[string]any{"id": "vague", "prompt": "consider the situation"},
	}
	out, notes := assignModelsToTaskArgs(tasks, buildModelTiers(discovered(), ModelPreferences{}), ModelPreferences{}, servedModels(discovered()), nil)

	got := map[string]string{}
	for _, raw := range out {
		fields := raw.(map[string]any)
		got[planString(fields, "id")] = planString(fields, "model")
	}
	if got["scan"] != "some-uncurated-proxy-model" {
		t.Errorf("scan got %q, want the cheapest model the provider serves", got["scan"])
	}
	if got["judge"] != "claude-opus-4.1" {
		t.Errorf("judge got %q, want the strong tier", got["judge"])
	}
	if got["mine"] != "claude-haiku-4.5" {
		t.Errorf("an explicit model was overridden: %q", got["mine"])
	}
	if got["vague"] != "" {
		t.Errorf("an unclassifiable task must inherit, got %q", got["vague"])
	}
	if len(notes) != 2 {
		t.Errorf("expected a note per assignment, got %v", notes)
	}

	// The caller's maps must not be mutated — on the saved-plan path they belong
	// to a stored plan the caller may still be holding.
	if _, mutated := tasks[0].(map[string]any)["model"]; mutated {
		t.Error("assignment wrote through to the caller's task map")
	}
}

// OFF UNLESS ASKED. Default-on would change which model every existing plan runs
// on, and what it costs, without anyone choosing that.
func TestAutoAssignIsOffByDefaultAndRefusesWhenUnavailable(t *testing.T) {
	gate := &PostureGate{}
	gate.Set(true)
	base := func() *OrchestrateTool {
		return &OrchestrateTool{
			PostureActive: gate.Active,
			ParentTools:   []string{"read_file"},
			RunTask: NewPlanRunner(PlanTaskContext{
				Executor: progressExecutor(t), Cwd: t.TempDir(), SpecialistName: "explorer",
			}),
		}
	}
	args := func(auto bool) map[string]any {
		a := map[string]any{
			"name":   "p",
			"tasks":  []any{map[string]any{"id": "a", "prompt": "find every caller"}},
			"budget": map[string]any{"max_workers": float64(1), "max_tokens": float64(100000)},
		}
		if auto {
			a["auto_assign"] = true
		}
		return a
	}

	// Default off: no discoverer wired, and the plan runs anyway.
	if res := base().RunWithOptions(context.Background(), args(false), tools.RunOptions{}); res.Status == tools.StatusError {
		t.Fatalf("a plan that did not ask for auto_assign must not need a discoverer: %s", res.Output)
	}

	// Asked for, unavailable: refused with a reason, not run silently without it.
	res := base().RunWithOptions(context.Background(), args(true), tools.RunOptions{})
	if res.Status != tools.StatusError {
		t.Fatalf("auto_assign with no discoverer must be refused, got %s", res.Status)
	}
	if !strings.Contains(res.Output, "auto_assign is not available") {
		t.Errorf("the refusal must say why: %q", res.Output)
	}

	// Asked for and available: assigned, and the result SAYS what it chose.
	tool := base()
	tool.DiscoverModels = func(context.Context) ([]DiscoveredModel, error) { return discovered(), nil }
	res = tool.RunWithOptions(context.Background(), args(true), tools.RunOptions{})
	if res.Status == tools.StatusError {
		t.Fatalf("auto_assign failed: %s", res.Output)
	}
	if !strings.Contains(res.Output, "Models assigned automatically") {
		t.Errorf("the result must report what it assigned:\n%s", res.Output)
	}
	if !strings.Contains(res.Output, "some-uncurated-proxy-model") {
		t.Errorf("the scan task's model is missing from the report:\n%s", res.Output)
	}
}

// THE SCHEMA IS THE ONLY WAY THE MODEL LEARNS THE OPTION EXISTS.
//
// auto_assign was implemented, wired, unit-tested and unreachable: the property
// was never added to Parameters(), and additionalProperties is false, so a model
// asked point-blank for auto_assign could not send it. The tool ran the plan
// without assignment and reported nothing, which is correct behaviour for an
// absent flag and looked exactly like a broken feature.
//
// The unit tests passed because they put auto_assign straight into the args map,
// which is a thing only a test can do. This asserts the advertisement instead.
func TestEveryArgumentTheToolReadsIsAdvertised(t *testing.T) {
	schema := (&OrchestrateTool{}).Parameters()
	for _, key := range []string{"auto_assign", "background", "saved", "tasks", "budget", "name", "description"} {
		property, ok := schema.Properties[key]
		if !ok {
			t.Errorf("the tool reads %q but never advertises it; a model cannot send what it cannot see", key)
			continue
		}
		if strings.TrimSpace(property.Description) == "" {
			t.Errorf("%q is advertised with no description", key)
		}
	}
	// additionalProperties:false is what makes an unadvertised key not merely
	// undiscoverable but unsendable, so the check above is not cosmetic.
	if schema.AdditionalProperties {
		t.Log("note: additionalProperties is true; an unadvertised key might still arrive")
	}
}

// The description must not promise behaviour the code no longer has. It said an
// unknown model was "refused when the plan is admitted" long after that stopped
// being true, which is a lie told directly to the model composing the call.
func TestTheTaskDescriptionMatchesWhatAdmissionActuallyDoes(t *testing.T) {
	tasks := (&OrchestrateTool{}).Parameters().Properties["tasks"].Description
	if strings.Contains(tasks, "refused when the plan is admitted") {
		t.Errorf("the schema still claims unknown models are refused at admission; they now pass through:\n%s", tasks)
	}
	if !strings.Contains(tasks, "model") {
		t.Errorf("the schema must tell the model a per-task model exists:\n%s", tasks)
	}
}

// A PLAN TASK MUST PRODUCE TEXT. Ranking by price assumes every candidate does
// the same job; an account with image or video models breaks that at the top
// end, exactly where the verify tier looks. A real xAI run picked
// grok-imagine-video-1.5 as the strongest model on the account and every task
// depending on it was doomed before it started.
func TestANonTextModelIsNeverAssigned(t *testing.T) {
	tiers := buildModelTiers([]DiscoveredModel{
		{ID: "grok-4.20-0309-non-reasoning", ToolCall: true, InputCost: 1},
		{ID: "grok-4.3", ToolCall: true, Reasoning: true, InputCost: 5},
		{ID: "grok-imagine-video-1.5", ToolCall: true, InputCost: 90},
	}, ModelPreferences{})
	for tier, id := range map[string]string{"cheap": tiers.cheap, "balanced": tiers.balanced, "strong": tiers.strong} {
		if strings.Contains(id, "video") {
			t.Errorf("%s tier = %q; a video model cannot answer a plan task", tier, id)
		}
	}
	if tiers.strong != "grok-4.3" {
		t.Errorf("strong = %q, want the strongest model that emits text", tiers.strong)
	}

	// A DECLARED modality is believed over the name, in both directions.
	declared := buildModelTiers([]DiscoveredModel{
		{ID: "plain-a", ToolCall: true, InputCost: 1, OutputModalities: []string{"text"}},
		{ID: "plain-b", ToolCall: true, InputCost: 9, OutputModalities: []string{"image"}},
	}, ModelPreferences{})
	if declared.strong == "plain-b" {
		t.Errorf("a model declaring image-only output was assigned: %+v", declared)
	}
	// A name that merely mentions a marker is still excluded — the heuristic only
	// ever removes candidates, so a false positive costs a model, not a broken plan.
	if emitsText(DiscoveredModel{ID: "some-embedding-model"}) {
		t.Error("an embedding model must not be assignable")
	}
	if !emitsText(DiscoveredModel{ID: "grok-4.3"}) {
		t.Error("an ordinary chat model must remain assignable")
	}
}

// EFFORT IS ONLY SENT FOR A MODEL THE REGISTRY CAN VOUCH FOR.
//
// The child clamps a requested effort only for models it can look up; for
// anything else it forwards the value verbatim, and a provider that does not
// accept the parameter rejects the request outright. A real run died three times
// with "Model grok-build-0.1 does not support parameter reasoningEffort".
func TestEffortIsNotSentToAModelNobodyCanVouchFor(t *testing.T) {
	if got := planTaskReasoningEffort("grok-build-0.1", "high", "high"); got != "" {
		t.Errorf("effort %q was sent for an uncurated model; the provider rejects the whole request", got)
	}
	if got := planTaskReasoningEffort("claude-haiku-4.5", "high", "high"); got != "high" {
		t.Errorf("effort for a curated reasoning model = %q, want high", got)
	}
	// And the argv proves it, not just the helper.
	manifest := planTaskManifest("explorer", "grok-build-0.1",
		planTaskReasoningEffort("grok-build-0.1", "high", "high"), []string{"read_file"})
	argv := appendModelArgs(nil, manifest, "grok-4.3", "high")
	for i, arg := range argv {
		if arg == "--reasoning-effort" {
			t.Fatalf("--reasoning-effort %q reached the child for an uncurated model: %v", argv[i+1], argv)
		}
	}
	if !containsArg(argv, "--model", "grok-build-0.1") {
		t.Errorf("the model itself must still be passed: %v", argv)
	}
}

func containsArg(argv []string, flag, want string) bool {
	for i, arg := range argv {
		if arg == flag && i+1 < len(argv) && argv[i+1] == want {
			return true
		}
	}
	return false
}

// PINS BEAT DISCOVERY, and work where discovery cannot.
//
// The automatic choice ranks by price, which fails both ways on real accounts:
// an xAI account put a build preview on verify because it was the priciest
// thing there, and an Ollama account reports no prices at all so the ranking
// collapses to alphabetical. Neither is a heuristics problem — the person with
// the account knows which model is strongest and the code does not.
func TestPinnedModelsWinAndWorkWithoutAnyDiscoverySignal(t *testing.T) {
	prefs := ModelPreferences{Scan: "deepseek-v4-flash", Verify: "deepseek-v4-pro"}
	// An Ollama-shaped account: ids only, no cost, no capabilities. The pinned
	// models are among them, which is the ordinary case — you pin what you have.
	ollama := []DiscoveredModel{
		{ID: "deepseek-v4-flash"}, {ID: "deepseek-v4-pro"},
		{ID: "gemma4:31b"}, {ID: "glm-5.1"}, {ID: "gpt-oss:120b"},
	}

	tasks := []any{
		map[string]any{"id": "s", "prompt": "find every caller"},
		map[string]any{"id": "i", "prompt": "fix the parser"},
		map[string]any{"id": "v", "prompt": "review the change"},
	}
	out, notes := assignModelsToTaskArgs(tasks, buildModelTiers(ollama, prefs), prefs, servedModels(ollama), nil)
	got := map[string]string{}
	for _, raw := range out {
		fields := raw.(map[string]any)
		got[planString(fields, "id")] = planString(fields, "model")
	}
	if got["s"] != "deepseek-v4-flash" {
		t.Errorf("scan got %q, want the pin", got["s"])
	}
	if got["v"] != "deepseek-v4-pro" {
		t.Errorf("verify got %q, want the pin", got["v"])
	}
	// An UNPINNED role still falls back to discovery.
	if got["i"] == "" {
		t.Error("an unpinned role must still be assigned from discovery")
	}
	// The note says which were the user's choice rather than the code's.
	joined := strings.Join(notes, " | ")
	if !strings.Contains(joined, "(pinned)") {
		t.Errorf("a pinned assignment must be marked as such: %s", joined)
	}

	// PINS ALONE ARE ENOUGH: a provider that offers nothing usable still assigns.
	only, _ := assignModelsToTaskArgs(tasks, buildModelTiers(nil, prefs), prefs, nil, nil)
	fields := only[0].(map[string]any)
	if planString(fields, "model") != "deepseek-v4-flash" {
		t.Errorf("with no discovery at all, a pin must still apply: %v", fields)
	}
}

// EXCLUSIONS remove a model from every tier — for the ones eligible on paper and
// wrong in practice, like a preview build that ranked highest on price and
// failed every task it was given.
func TestAnExcludedModelIsNeverChosen(t *testing.T) {
	models := []DiscoveredModel{
		{ID: "grok-4.20-0309-non-reasoning", ToolCall: true, InputCost: 1},
		{ID: "grok-4.3", ToolCall: true, Reasoning: true, InputCost: 5},
		// Reasoning: true matches what discovery actually reported for it — which
		// is why the reasoning-preference could not save the real run and price
		// alone decided.
		{ID: "grok-build-0.1", ToolCall: true, Reasoning: true, InputCost: 20},
	}
	if got := buildModelTiers(models, ModelPreferences{}); got.strong != "grok-build-0.1" {
		t.Fatalf("sanity check failed: unexcluded, price puts %q on top", got.strong)
	}
	tiers := buildModelTiers(models, ModelPreferences{Exclude: []string{"grok-build-0.1"}})
	for tier, id := range map[string]string{"cheap": tiers.cheap, "balanced": tiers.balanced, "strong": tiers.strong} {
		if id == "grok-build-0.1" {
			t.Errorf("%s tier = %q despite being excluded", tier, id)
		}
	}
	if tiers.strong != "grok-4.3" {
		t.Errorf("strong = %q, want the best remaining model", tiers.strong)
	}
	// Case-insensitive, because a config file is written by a human.
	if !(ModelPreferences{Exclude: []string{"GROK-Build-0.1"}}).excluded("grok-build-0.1") {
		t.Error("exclusion must not depend on case")
	}
}

// CONFIGURED ON, BUT STILL OVERRIDABLE PER PLAN.
//
// auto_assign is off unless asked for, which means a plain zeromaxing prompt
// never routes models — the user has to type the flag every time. A configured
// default fixes that, but only if a single plan can still say no: without
// presence detection an absent argument and an explicit false are the same
// value, and a config default could never be turned off.
func TestConfiguredAutoAssignAppliesUnlessThePlanSaysOtherwise(t *testing.T) {
	gate := &PostureGate{}
	gate.Set(true)
	build := func(prefs ModelPreferences) *OrchestrateTool {
		return &OrchestrateTool{
			PostureActive:  gate.Active,
			ParentTools:    []string{"read_file"},
			ModelPrefs:     prefs,
			DiscoverModels: func(context.Context) ([]DiscoveredModel, error) { return discovered(), nil },
			RunTask: NewPlanRunner(PlanTaskContext{
				Executor: progressExecutor(t), Cwd: t.TempDir(), SpecialistName: "explorer",
			}),
		}
	}
	args := func(mutate func(map[string]any)) map[string]any {
		a := map[string]any{
			"name":   "p",
			"tasks":  []any{map[string]any{"id": "a", "prompt": "find every caller"}},
			"budget": map[string]any{"max_workers": float64(1), "max_tokens": float64(100000)},
		}
		if mutate != nil {
			mutate(a)
		}
		return a
	}

	// Configured on, plan silent: assignment happens without anyone asking.
	on := build(ModelPreferences{AutoAssign: true})
	res := on.RunWithOptions(context.Background(), args(nil), tools.RunOptions{})
	if !strings.Contains(res.Output, "Models assigned automatically") {
		t.Errorf("a configured default must apply to a plan that never mentions it:\n%s", res.Output)
	}

	// Configured on, plan says NO: the plan wins.
	res = on.RunWithOptions(context.Background(), args(func(a map[string]any) { a["auto_assign"] = false }), tools.RunOptions{})
	if strings.Contains(res.Output, "Models assigned automatically") {
		t.Errorf("an explicit auto_assign:false must override the configured default:\n%s", res.Output)
	}

	// Configured off, plan silent: unchanged from before this existed.
	off := build(ModelPreferences{})
	res = off.RunWithOptions(context.Background(), args(nil), tools.RunOptions{})
	if strings.Contains(res.Output, "Models assigned automatically") {
		t.Errorf("with nothing configured and nothing asked, nothing must be assigned:\n%s", res.Output)
	}

	// Configured off, plan says yes: still works.
	res = off.RunWithOptions(context.Background(), args(func(a map[string]any) { a["auto_assign"] = true }), tools.RunOptions{})
	if !strings.Contains(res.Output, "Models assigned automatically") {
		t.Errorf("an explicit request must work with nothing configured:\n%s", res.Output)
	}
}

// A STANDING PREFERENCE MUST NOT BREAK PLANNING WHEN IT CANNOT BE HONOURED.
//
// A plan that ASKS for auto_assign wants it, so an unavailable run is refused —
// running silently without it is what the request exists to prevent. A CONFIGURED
// default is not a demand: refusing every plan because a models endpoint blinked
// would let one setting break all planning offline or behind a proxy. Found by a
// real test failing the moment the setting was switched on.
func TestAConfiguredDefaultDegradesWhereAnExplicitRequestRefuses(t *testing.T) {
	gate := &PostureGate{}
	gate.Set(true)
	build := func(prefs ModelPreferences, discover ModelDiscoverer) *OrchestrateTool {
		return &OrchestrateTool{
			PostureActive: gate.Active, ParentTools: []string{"read_file"},
			ModelPrefs: prefs, DiscoverModels: discover,
			RunTask: NewPlanRunner(PlanTaskContext{
				Executor: progressExecutor(t), Cwd: t.TempDir(), SpecialistName: "explorer",
			}),
		}
	}
	plan := func(mutate func(map[string]any)) map[string]any {
		a := map[string]any{
			"name":   "p",
			"tasks":  []any{map[string]any{"id": "a", "prompt": "find every caller"}},
			"budget": map[string]any{"max_workers": float64(1), "max_tokens": float64(100000)},
		}
		if mutate != nil {
			mutate(a)
		}
		return a
	}
	broken := func(context.Context) ([]DiscoveredModel, error) {
		return nil, fmt.Errorf("decode models response: unexpected end of input")
	}

	// Configured on, discovery broken, plan silent: the PLAN STILL RUNS.
	res := build(ModelPreferences{AutoAssign: true}, broken).
		RunWithOptions(context.Background(), plan(nil), tools.RunOptions{})
	if res.Status == tools.StatusError {
		t.Fatalf("a configured default must not refuse the plan when discovery fails: %s", res.Output)
	}
	if !strings.Contains(res.Output, "could not list the provider's models") {
		t.Errorf("it must still say what it could not do:\n%s", res.Output)
	}

	// Same failure, but the PLAN asked: refused, because it asked.
	res = build(ModelPreferences{}, broken).
		RunWithOptions(context.Background(), plan(func(a map[string]any) { a["auto_assign"] = true }), tools.RunOptions{})
	if res.Status != tools.StatusError {
		t.Errorf("an explicit auto_assign must be refused when it cannot be honoured, got %s", res.Status)
	}

	// And the same split when the run simply has no discoverer at all.
	res = build(ModelPreferences{AutoAssign: true}, nil).
		RunWithOptions(context.Background(), plan(nil), tools.RunOptions{})
	if res.Status == tools.StatusError {
		t.Errorf("a configured default must degrade when the run cannot discover: %s", res.Output)
	}
	res = build(ModelPreferences{}, nil).
		RunWithOptions(context.Background(), plan(func(a map[string]any) { a["auto_assign"] = true }), tools.RunOptions{})
	if res.Status != tools.StatusError {
		t.Errorf("an explicit request with no discoverer must be refused, got %s", res.Status)
	}
}

// A PIN THE ACTIVE PROVIDER DOES NOT SERVE MUST NOT KILL THE PLAN.
//
// Pins name models and models belong to providers. Switching provider — which a
// user does when an account hits its quota, mid-session — leaves every pin
// naming something the new provider has never heard of. Applied regardless, that
// turned a provider switch into total failure: four tasks dead with
// `model "grok-4.3" not found` before one of them had run.
func TestAPinTheProviderCannotServeIsPassedOverNotForced(t *testing.T) {
	// Pins from a previous provider; discovery reports a different account.
	prefs := ModelPreferences{Scan: "grok-4.20-0309-non-reasoning", Verify: "grok-4.3"}
	nowServing := []DiscoveredModel{{ID: "glm-5.2"}, {ID: "deepseek-v4-pro"}}
	served := servedModels(nowServing)

	tasks := []any{
		map[string]any{"id": "s", "prompt": "searching for every caller"},
		map[string]any{"id": "v", "prompt": "reviewing the change"},
	}
	out, notes := assignModelsToTaskArgs(tasks, buildModelTiers(nowServing, prefs), prefs, served, nil)

	for _, raw := range out {
		fields := raw.(map[string]any)
		got := planString(fields, "model")
		if strings.HasPrefix(got, "grok") {
			t.Errorf("task %q was assigned %q, which this provider does not serve",
				planString(fields, "id"), got)
		}
		if got != "" && !served[got] {
			t.Errorf("task %q was assigned %q, which is not in the served set",
				planString(fields, "id"), got)
		}
	}
	// And it SAYS the pin was passed over, or a user who configured one and sees
	// a different model has no way to find out why.
	joined := strings.Join(notes, " | ")
	if !strings.Contains(joined, "not served by this provider") {
		t.Errorf("a passed-over pin must explain itself: %s", joined)
	}

	// With NO discovery at all, a pin is still honoured — that is the case pins
	// exist for, and an empty list is not evidence the pin is wrong.
	out, _ = assignModelsToTaskArgs(tasks, buildModelTiers(nil, prefs), prefs, nil, nil)
	if got := planString(out[0].(map[string]any), "model"); got != "grok-4.20-0309-non-reasoning" {
		t.Errorf("with no discovery the pin must still apply, got %q", got)
	}
}
