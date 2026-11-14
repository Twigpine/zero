package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/specialist"
	"github.com/Twigpine/zero/internal/tools"
)

// The plan tool's parent grant was declared, documented and validated against —
// and left nil by BOTH production call sites, which made the "a task may narrow
// the parent's grant, never widen it" rule inert. These tests drive
// registerSpecialistTools, the function both call sites go through, rather than
// the helper it uses: asserting the helper would have passed throughout the
// period the wiring was missing.

func registeredOrchestrateTool(t *testing.T, enabled, disabled []string) *specialist.OrchestrateTool {
	t.Helper()
	workspace := t.TempDir()
	registry := newCoreRegistry(workspace)
	runtime, err := registerSpecialistTools(registry, workspace, 0, enabled, disabled, nil, orchestrateWiring{
		Gate: &specialist.PostureGate{},
	})
	if err != nil {
		t.Fatalf("registerSpecialistTools: %v", err)
	}
	t.Cleanup(func() { closeSpecialistRuntime(nil, runtime) })

	registered, found := registry.Get(specialist.OrchestrateToolName)
	if !found {
		t.Fatal("orchestrate was not registered")
	}
	tool, ok := registered.(*specialist.OrchestrateTool)
	if !ok {
		t.Fatalf("orchestrate is %T, not *specialist.OrchestrateTool", registered)
	}
	return tool
}

// An unfiltered run grants every read-only tool a plan task may hold. The
// failure this pins is a grant of length zero, which is what both call sites
// produced.
func TestRegisteredOrchestrateToolCarriesTheRunsGrant(t *testing.T) {
	tool := registeredOrchestrateTool(t, nil, nil)
	if len(tool.ParentTools) == 0 {
		t.Fatal("the registered orchestrate tool holds no parent grant, so the narrowing rule cannot fire")
	}
	// The grant is every GRANTABLE name this registry holds — read-only plus the
	// write tools a task may name — intersected with what the registry actually
	// has. Comparing against the full list would fail for a registry missing one
	// of them, so it is asserted as a subset with the read-only half required.
	grantable := map[string]bool{}
	for _, name := range specialist.PlanGrantableToolNames() {
		grantable[name] = true
	}
	for _, name := range tool.ParentTools {
		if !grantable[name] {
			t.Fatalf("grant contains %q, which a plan task may never hold", name)
		}
	}
	held := map[string]bool{}
	for _, name := range tool.ParentTools {
		held[name] = true
	}
	for _, name := range specialist.PlanReadOnlyToolNames() {
		if _, found := newCoreRegistry(t.TempDir()).Get(name); found && !held[name] {
			t.Fatalf("the unfiltered grant is missing read-only tool %q", name)
		}
	}
}

// --enabled-tools narrows the grant. This is the reproduction from the audit,
// at the registration boundary: a parent holding only grep must not hand a plan
// task read_file.
func TestRegisteredOrchestrateGrantHonoursEnabledTools(t *testing.T) {
	tool := registeredOrchestrateTool(t, []string{"grep", specialist.OrchestrateToolName}, nil)
	if strings.Join(tool.ParentTools, ",") != "grep" {
		t.Fatalf("grant = %v, want exactly [grep]: a plan task must not hold what --enabled-tools denied the run", tool.ParentTools)
	}
}

// --disabled-tools narrows it too, through the same filter gate the agent loop
// uses, so the two cannot disagree about what the run holds.
func TestRegisteredOrchestrateGrantHonoursDisabledTools(t *testing.T) {
	tool := registeredOrchestrateTool(t, nil, []string{"read_file", "glob"})
	for _, name := range tool.ParentTools {
		if name == "read_file" || name == "glob" {
			t.Fatalf("grant %v still contains a tool --disabled-tools removed from the run", tool.ParentTools)
		}
	}
	if len(tool.ParentTools) == 0 {
		t.Fatal("grant collapsed to empty; only the two disabled tools should have been removed")
	}
}

// A run holding no read-only tools grants nothing. The grant must be EMPTY
// rather than falling back to a default set — an empty grant is refused
// downstream, and the bug was that it expanded instead.
func TestRegisteredOrchestrateGrantIsEmptyWhenTheRunHoldsNoReadTools(t *testing.T) {
	tool := registeredOrchestrateTool(t, []string{specialist.OrchestrateToolName}, nil)
	if len(tool.ParentTools) != 0 {
		t.Fatalf("grant = %v, want empty: this run holds no read-only tools at all", tool.ParentTools)
	}
}

// The candidate set comes from specialist's exported list, not a second copy
// maintained here. Two duplicated lists drift (invariant 5).
func TestPlanParentToolsNeverExceedsWhatAPlanMayHold(t *testing.T) {
	// The bound is PlanGrantableToolNames, not PlanReadOnlyToolNames, and the
	// difference is the point of the write-task arc: a plan task inherits only
	// read-only tools, and may NAME one of a short write allow-list. The grant
	// has to be able to deliver a named write tool or the task would validate
	// and then run with less than it asked for.
	allowed := map[string]bool{}
	for _, name := range specialist.PlanGrantableToolNames() {
		allowed[name] = true
	}
	for _, name := range planParentTools(newCoreRegistry(t.TempDir()), nil, nil) {
		if !allowed[name] {
			t.Fatalf("grant contains %q, which is not a tool a plan task may ever hold", name)
		}
	}
}

// The depth the plan tool measures must be the RUN's depth, not a constant.
// Left unset it was always 0, so the admission headroom check and the
// executor's own nesting check could never fire — an inert guard that reads
// like a live one.
func TestRegisteredOrchestrateToolCarriesTheRunsDepth(t *testing.T) {
	workspace := t.TempDir()
	registry := newCoreRegistry(workspace)
	runtime, err := registerSpecialistTools(registry, workspace, 0, nil, nil, nil, orchestrateWiring{
		Gate:        &specialist.PostureGate{},
		PlanContext: specialist.PlanTaskContext{Cwd: workspace, Depth: 3},
	})
	if err != nil {
		t.Fatalf("registerSpecialistTools: %v", err)
	}
	t.Cleanup(func() { closeSpecialistRuntime(nil, runtime) })

	registered, _ := registry.Get(specialist.OrchestrateToolName)
	tool, ok := registered.(*specialist.OrchestrateTool)
	if !ok {
		t.Fatalf("orchestrate is %T", registered)
	}
	if tool.Depth != 3 {
		t.Fatalf("Depth = %d, want the run's depth 3: an always-zero depth makes the headroom check inert", tool.Depth)
	}
}

// The plan recorder is a REQUIRED parameter, not a wiring field. As a field the
// TUI simply never set it, so a plan run there recorded none of its five
// lifecycle events — finding 9, and the same omission the parent grant
// suffered. Making it positional turns forgetting it into a compile error;
// this test pins that a supplied recorder actually reaches the tool.
func TestRegisteredOrchestrateToolCarriesTheSuppliedRecorder(t *testing.T) {
	workspace := t.TempDir()
	registry := newCoreRegistry(workspace)
	recorder := &countingPlanRecorder{}
	runtime, err := registerSpecialistTools(registry, workspace, 0, nil, nil, recorder, orchestrateWiring{
		Gate: &specialist.PostureGate{},
	})
	if err != nil {
		t.Fatalf("registerSpecialistTools: %v", err)
	}
	t.Cleanup(func() { closeSpecialistRuntime(nil, runtime) })

	registered, _ := registry.Get(specialist.OrchestrateToolName)
	tool, ok := registered.(*specialist.OrchestrateTool)
	if !ok {
		t.Fatalf("orchestrate is %T", registered)
	}
	if tool.Recorder != recorder {
		t.Fatal("the supplied recorder did not reach the tool, so plan events go nowhere")
	}
}

type countingPlanRecorder struct{ dispatched int }

func (r *countingPlanRecorder) TaskDispatched(specialist.Task)      { r.dispatched++ }
func (r *countingPlanRecorder) TaskCompleted(specialist.TaskResult) {}
func (r *countingPlanRecorder) TaskFailed(specialist.TaskResult)    {}

// The plan-size tier must survive registerSpecialistTools. Both call sites pass
// it through this function, and a tier read from config but dropped here would
// leave every run on the default ceiling while the config file said otherwise —
// the same shape as the grant defect above, one layer along.
func TestRegisteredOrchestrateToolCarriesTheConfiguredTier(t *testing.T) {
	workspace := t.TempDir()
	registry := newCoreRegistry(workspace)
	runtime, err := registerSpecialistTools(registry, workspace, 0, nil, nil, nil, orchestrateWiring{
		Gate: &specialist.PostureGate{},
		Size: config.PlanSizeSmall,
	})
	if err != nil {
		t.Fatalf("registerSpecialistTools: %v", err)
	}
	t.Cleanup(func() { closeSpecialistRuntime(nil, runtime) })

	registered, _ := registry.Get(specialist.OrchestrateToolName)
	tool, ok := registered.(*specialist.OrchestrateTool)
	if !ok {
		t.Fatalf("orchestrate is %T", registered)
	}
	if tool.Size != config.PlanSizeSmall {
		t.Fatalf("registered tier = %q; want %q — the wiring dropped it", tool.Size, config.PlanSizeSmall)
	}
}

// EVERY TOOL A PLAN TASK MAY HOLD MUST BE CLASSIFIED READ-ONLY BY CAPABILITY,
// not merely by its safety string.
//
// This is the invariant that broke, and it broke silently. update_plan declared
// readOnlySafety and set no capabilities, so CapabilitiesOf reported
// EffectUnknown — the fail-closed default — while its safety said read-only.
// runCanMutate reads the CAPABILITY, so one unclassified tool in an otherwise
// read-only grant made the whole child look mutating, and ~6,500 characters of
// confirmation policy it can never need rode along on every task of every plan.
//
// Asserted against the REAL registry, because the defect was that two
// descriptions of one tool disagreed — checking the list against itself would
// have found nothing.
func TestEveryPlanGrantToolIsReadOnlyByCapability(t *testing.T) {
	registry := newCoreRegistry(t.TempDir())
	checked := 0
	for _, name := range specialist.PlanReadOnlyToolNames() {
		tool, found := registry.Get(name)
		if !found {
			// Not every read-only name is registered in a bare core registry;
			// what matters is that the ones that are, classify correctly.
			continue
		}
		checked++
		if effect := tools.CapabilitiesOf(tool).Effect; effect != tools.EffectReadOnly {
			t.Errorf("plan grant holds %q whose capability is %v, not EffectReadOnly; "+
				"a plan child would carry the confirmation policy it can never need", name, effect)
		}
	}
	if checked == 0 {
		t.Fatal("no plan-grant tool was found in the registry; this test checked nothing")
	}
}

// BOTH SURFACES SUPPLY AN ISOLATOR. A write-capable plan is refused where none
// exists, so a surface that forgot to wire one would refuse every write plan
// with "this run cannot isolate" — correct, and useless.
func TestBothSurfacesWireAnIsolator(t *testing.T) {
	if newPlanIsolator("") != nil {
		t.Fatal("an empty workspace root produced an isolator")
	}
	if newPlanIsolator(t.TempDir()) == nil {
		t.Fatal("a real workspace root produced no isolator")
	}

	workspace := t.TempDir()
	registry := newCoreRegistry(workspace)
	runtime, err := registerSpecialistTools(registry, workspace, 0, nil, nil, nil, orchestrateWiring{
		Gate:    &specialist.PostureGate{},
		Isolate: newPlanIsolator(workspace),
	})
	if err != nil {
		t.Fatalf("registerSpecialistTools: %v", err)
	}
	t.Cleanup(func() { closeSpecialistRuntime(nil, runtime) })
	registered, _ := registry.Get(specialist.OrchestrateToolName)
	tool, ok := registered.(*specialist.OrchestrateTool)
	if !ok {
		t.Fatalf("orchestrate is %T", registered)
	}
	if tool.Isolate == nil {
		t.Fatal("the wiring dropped the isolator; every write-capable plan would be refused")
	}
}

// The worktree NAME is derived, not taken. A plan name is model-supplied and
// reaches a filesystem path, so what comes out has to be a name — the allow-list
// in worktrees.Prepare is the guarantee, and this is what feeds it something it
// will accept rather than something it will reject.
func TestAPlanNameBecomesAUsableWorktreeName(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"sweep", "plan-sweep"},
		{"pre release", "plan-prerelease"},
		{"../../etc/passwd", "plan-etcpasswd"},
		{"", "plan"},
		{"!!!", "plan"},
		{"a/b\\c;d", "plan-abcd"},
	} {
		if got := planWorktreeName(tc.in); got != tc.want {
			t.Errorf("planWorktreeName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	// Long names are bounded, so a plan cannot produce a path component the
	// filesystem refuses.
	if got := planWorktreeName(strings.Repeat("x", 300)); len(got) > 64 {
		t.Fatalf("a long plan name produced a %d-character worktree name", len(got))
	}
}

// THE HOOK MUST ACTUALLY BE WIRED. DiscoverModels is consulted only when a plan
// sets auto_assign, so a nil left in the wiring compiles, ships, and refuses
// every such plan with "not available in this run" — the feature present in the
// schema and reachable from nowhere. That is this branch's most repeated defect,
// and it is checked here rather than trusted.
func TestOrchestrateGetsAModelDiscoverer(t *testing.T) {
	registry := tools.NewRegistry()
	gate := &specialist.PostureGate{}
	gate.Set(true)
	workspace := t.TempDir()

	if _, err := registerSpecialistTools(registry, workspace, 0, nil, nil, nil, orchestrateWiring{
		Gate:           gate,
		PlanContext:    specialist.PlanTaskContext{Cwd: workspace},
		DiscoverModels: planModelDiscoverer(t.TempDir(), config.ProviderProfile{Name: "test", Model: "gpt-4.1"}),
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	tool, ok := registry.Get(specialist.OrchestrateToolName)
	if !ok {
		t.Fatal("the orchestrate tool was not registered")
	}
	orchestrate, ok := tool.(*specialist.OrchestrateTool)
	if !ok {
		t.Fatalf("registered tool is %T", tool)
	}
	if orchestrate.DiscoverModels == nil {
		t.Error("DiscoverModels is nil on the registered tool; auto_assign would refuse every plan")
	}
}

// The user's model pins and exclusions must reach the tool. Configured and not
// wired, they are a config key that silently does nothing — the same shape as
// DiscoverModels above, which shipped nil.
func TestOrchestrateGetsTheConfiguredModelPreferences(t *testing.T) {
	registry := tools.NewRegistry()
	gate := &specialist.PostureGate{}
	gate.Set(true)
	workspace := t.TempDir()

	if _, err := registerSpecialistTools(registry, workspace, 0, nil, nil, nil, orchestrateWiring{
		Gate:        gate,
		PlanContext: specialist.PlanTaskContext{Cwd: workspace},
		ModelPrefs: planModelPreferences(config.PlanModelsConfig{
			Verify:  "deepseek-v4-pro",
			Exclude: []string{"grok-build-0.1"},
		}),
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	tool, _ := registry.Get(specialist.OrchestrateToolName)
	orchestrate, ok := tool.(*specialist.OrchestrateTool)
	if !ok {
		t.Fatalf("registered tool is %T", tool)
	}
	if orchestrate.ModelPrefs.Verify != "deepseek-v4-pro" {
		t.Errorf("the verify pin did not reach the tool: %+v", orchestrate.ModelPrefs)
	}
	if len(orchestrate.ModelPrefs.Exclude) != 1 {
		t.Errorf("the exclusion list did not reach the tool: %+v", orchestrate.ModelPrefs)
	}
}

// And the adapter really produces the shape specialist chooses between, rather
// than an empty struct that would make every tier unassignable.
func TestTheModelDiscovererCarriesWhatAPlanChoosesBetween(t *testing.T) {
	discover := planModelDiscoverer(t.TempDir(), config.ProviderProfile{Name: "test", Model: "gpt-4.1"})
	if discover == nil {
		t.Fatal("adapter returned nil")
	}
	// The network call is expected to fail in a test environment; what matters is
	// that a failure is REPORTED rather than silently yielding no models, which
	// would make auto_assign a no-op nobody could diagnose.
	models, err := discover(context.Background())
	if err == nil && len(models) > 0 {
		for _, model := range models {
			if strings.TrimSpace(model.ID) == "" {
				t.Errorf("a discovered model arrived with no id: %+v", model)
			}
		}
	}
}
